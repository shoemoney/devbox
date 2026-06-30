// Package transport is devbox's device-side HTTP client for talking to the hub.
//
// It speaks the wire contract in pkg/proto: JSON over HTTP for control calls,
// raw bytes for blob uploads, one bearer token for auth. Join is the only
// unauthenticated call; redeeming a token yields the bearer used by everything
// else (stashed via SetBearer so the caller doesn't have to thread it through).
package transport

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"golang.org/x/time/rate"

	"github.com/shoemoney/devbox/internal/chunk"
	"github.com/shoemoney/devbox/pkg/proto"
)

// Client talks to one hub. The zero value is not usable; call New.
type Client struct {
	base     string // hub base URL, e.g. "http://host:8080"
	bearer   string // device bearer token; empty until Join
	hc       *http.Client
	limiter  *rate.Limiter // optional blob-transfer rate cap; nil = unlimited
	compress bool          // gzip blob uploads when it shrinks them (settings.transfer.compress)
}

// maxBlobBytes mirrors the hub's per-blob cap; used to bound a gzip-decoded
// download so a malicious/buggy hub can't bomb the client.
const maxBlobBytes = 256 << 20

// blobRetries is how many times a transient blob transfer is attempted before
// giving up (the daemon retries the whole sync on its next trigger regardless).
const blobRetries = 3

// New returns a Client for the hub at base (e.g. "http://host:8080").
func New(base string) *Client {
	return &Client{
		base: strings.TrimRight(base, "/"),
		// No total client Timeout: a 256MiB blob over a slow WAN link can legitimately
		// take minutes, and a wall-clock total would kill it mid-transfer. Bound the
		// phases that SHOULD be fast instead (connect, TLS, time-to-first-byte) and let
		// TCP keepalive reap a dead peer. ponytail: a mid-body stall isn't bounded here;
		// add a stall-timeout reader if a flaky link ever hangs transfers.
		hc: &http.Client{
			Transport: &http.Transport{
				DialContext:           (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
				TLSHandshakeTimeout:   10 * time.Second,
				ResponseHeaderTimeout: 30 * time.Second,
				DisableCompression:    true, // we negotiate gzip explicitly so the rate limiter sees wire bytes
				MaxIdleConnsPerHost:   16,   // reuse connections for parallel blob transfers
			},
		},
	}
}

// transient reports whether a failed attempt is worth retrying: a network error
// (code 0) or a 5xx. A 4xx is a definite client/auth error — retrying just hammers.
func transient(code int) bool { return code < 400 || code >= 500 }

// retry runs attempt up to blobRetries times with exponential backoff, retrying
// only transient failures. attempt returns the HTTP status (0 on a pre-response
// error) and the error. Blob GET/PUT are idempotent (content-addressed), so this
// is safe; it is NOT used for the non-idempotent control calls.
func (c *Client) retry(attempt func() (int, error)) error {
	delay := 200 * time.Millisecond
	var err error
	for i := 0; i < blobRetries; i++ {
		var code int
		if code, err = attempt(); err == nil {
			return nil
		}
		if !transient(code) {
			return err
		}
		if i < blobRetries-1 {
			time.Sleep(delay)
			delay *= 2
		}
	}
	return err
}

// SetBearer sets the device bearer token used for authenticated requests.
func (c *Client) SetBearer(b string) { c.bearer = b }

// SetCompress enables gzip on blob uploads (used when settings.transfer.compress
// is on — e.g. syncing over a WAN link). Off by default.
func (c *Client) SetCompress(on bool) { c.compress = on }

// gzipBytes returns the gzip-compressed form of data. Errors are impossible for
// an in-memory writer; on the off chance, returns data so the caller's "shrank?"
// check falls through to sending it uncompressed.
func gzipBytes(data []byte) []byte {
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write(data); err != nil {
		return data
	}
	if err := zw.Close(); err != nil {
		return data
	}
	return buf.Bytes()
}

// Bearer returns the current device bearer token.
func (c *Client) Bearer() string { return c.bearer }

// Join redeems a one-time token and enrolls this device. It is unauthenticated;
// on success it stashes the returned bearer via SetBearer.
func (c *Client) Join(token, name string, pubkey ed25519.PublicKey, priv ed25519.PrivateKey) (proto.JoinResponse, error) {
	var resp proto.JoinResponse
	sig := ed25519.Sign(priv, proto.JoinChallenge(token, pubkey)) // prove we hold the key
	err := c.do(http.MethodPost, proto.PathJoin, false, proto.JoinRequest{
		Token: token, Name: name, Pubkey: pubkey, Signature: sig,
	}, &resp)
	if err != nil {
		return proto.JoinResponse{}, err
	}
	c.SetBearer(resp.Bearer)
	return resp, nil
}

// Publish creates a share owned by this device.
func (c *Client) Publish(share string) error {
	return c.do(http.MethodPost, proto.PathPublish, true, proto.PublishRequest{Share: share}, nil)
}

// Have asks the hub which of hashes it is missing (and must be uploaded).
func (c *Client) Have(hashes []string) ([]string, error) {
	var resp proto.HaveResponse
	if err := c.do(http.MethodPost, proto.PathHave, true, proto.HaveRequest{Hashes: hashes}, &resp); err != nil {
		return nil, err
	}
	return resp.Missing, nil
}

// PutBlob uploads one blob's raw bytes under its content hash.
func (c *Client) PutBlob(hash string, data []byte) error {
	return c.raw(http.MethodPut, proto.PathBlob+hash, data)
}

// Events opens an SSE stream of hub change events (optionally filtered to one
// share) and calls onEvent for each, until ctx is cancelled or the stream ends.
// Uses a dedicated timeout-less client since the stream is long-lived.
func (c *Client) Events(ctx context.Context, share string, onEvent func(proto.Event)) error {
	u := c.base + proto.PathEvents
	if share != "" {
		u += "?share=" + url.QueryEscape(share)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	req.Header.Set(proto.AuthHeader, "Bearer "+c.bearer)
	resp, err := (&http.Client{}).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		b, _ := io.ReadAll(resp.Body)
		return apiError(resp.StatusCode, b)
	}
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		data, ok := strings.CutPrefix(sc.Text(), "data: ")
		if !ok {
			continue
		}
		var ev proto.Event
		if json.Unmarshal([]byte(data), &ev) == nil {
			onEvent(ev)
		}
	}
	return sc.Err()
}

// GetBlob downloads one blob's raw bytes by content hash (used by pull). When
// compression is on it negotiates gzip explicitly (so the rate limiter counts
// wire bytes) and decompresses bomb-safely. Transient failures are retried.
func (c *Client) GetBlob(hash string) ([]byte, error) {
	var out []byte
	err := c.retry(func() (int, error) {
		req, err := http.NewRequest(http.MethodGet, c.base+proto.PathBlob+hash, nil)
		if err != nil {
			return 0, err
		}
		req.Header.Set(proto.AuthHeader, "Bearer "+c.bearer)
		if c.compress {
			req.Header.Set("Accept-Encoding", "gzip")
		}
		resp, err := c.hc.Do(req)
		if err != nil {
			return 0, err // network: code 0 → retryable
		}
		defer resp.Body.Close()
		reader := c.limit(resp.Body) // count compressed wire bytes against the cap
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			b, _ := io.ReadAll(reader)
			return resp.StatusCode, apiError(resp.StatusCode, b)
		}
		if resp.Header.Get("Content-Encoding") == "gzip" {
			gz, gerr := gzip.NewReader(reader)
			if gerr != nil {
				return resp.StatusCode, gerr
			}
			defer gz.Close()
			reader = io.LimitReader(gz, maxBlobBytes+1) // bomb guard
		}
		body, err := io.ReadAll(reader)
		if err != nil {
			return resp.StatusCode, err // truncated mid-body → retryable (code 200 is < 400)
		}
		if int64(len(body)) > maxBlobBytes {
			return resp.StatusCode, fmt.Errorf("devbox: blob %s exceeds %d bytes decompressed", hash, maxBlobBytes)
		}
		// Blobs (chunks and manifests) are content-addressed: the key is BLAKE3 of the
		// bytes. Verify it so a corrupt/truncated transfer or a buggy/malicious hub
		// can't write wrong content into the user's tree undetected.
		if chunk.Hash(body) != hash {
			return resp.StatusCode, fmt.Errorf("devbox: blob %s failed integrity check (got %s)", hash, chunk.Hash(body))
		}
		out = body
		return resp.StatusCode, nil
	})
	return out, err
}

// Push commits a snapshot. Its manifest and chunk blobs must already be uploaded.
func (c *Client) Push(req proto.PushRequest) (proto.PushResponse, error) {
	var resp proto.PushResponse
	if err := c.do(http.MethodPost, proto.PathPush, true, req, &resp); err != nil {
		return proto.PushResponse{}, err
	}
	return resp, nil
}

// Head returns the current head snapshot id of share ("" if none).
func (c *Client) Head(share string) (string, error) {
	var resp proto.HeadResponse
	path := proto.PathHead + "?" + url.Values{"share": {share}}.Encode()
	if err := c.do(http.MethodGet, path, true, nil, &resp); err != nil {
		return "", err
	}
	return resp.Head, nil
}

// Log returns a share's snapshot history, newest first.
func (c *Client) Log(share string) ([]proto.SnapshotInfo, error) {
	var resp proto.LogResponse
	path := proto.PathLog + "?" + url.Values{"share": {share}}.Encode()
	if err := c.do(http.MethodGet, path, true, nil, &resp); err != nil {
		return nil, err
	}
	return resp.Snapshots, nil
}

// Members returns who can access a share (M8a).
func (c *Client) Members(share string) (proto.MembersResponse, error) {
	var resp proto.MembersResponse
	path := proto.PathMembers + "?" + url.Values{"share": {share}}.Encode()
	err := c.do(http.MethodGet, path, true, nil, &resp)
	return resp, err
}

// Invite mints an invite token binding (principal, share, role) — the invitee
// redeems it with `devbox join`. reshare grants the +s delegation bit.
func (c *Client) Invite(share, principal, role string, reshare bool) (string, error) {
	var resp proto.InviteResponse
	req := proto.InviteRequest{Share: share, Principal: principal, Role: role, Reshare: reshare}
	err := c.do(http.MethodPost, proto.PathInvite, true, req, &resp)
	return resp.Token, err
}

// RevokeInvite kills a still-pending invite token before it's redeemed.
func (c *Client) RevokeInvite(token string) error {
	return c.do(http.MethodPost, proto.PathInviteRevoke, true, proto.InviteRevokeRequest{Token: token}, nil)
}

// do sends a JSON request (nil body for GET) and decodes a JSON response into
// out (nil out discards the body). It sets the bearer header when auth is true.
func (c *Client) do(method, path string, auth bool, body, out any) error {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, c.base+path, rdr)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if auth {
		req.Header.Set(proto.AuthHeader, "Bearer "+c.bearer)
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return apiError(resp.StatusCode, data)
	}
	if out != nil {
		return json.Unmarshal(data, out)
	}
	return nil
}

// raw sends raw bytes (used for blob uploads) with the bearer header set.
func (c *Client) raw(method, path string, data []byte) error {
	enc := ""
	// gzip the body when compression is on AND it actually shrinks — incompressible
	// chunks (already-compressed media, encrypted blobs) go raw so we never inflate
	// the wire. The hub hashes the DECOMPRESSED body, so dedup/integrity are intact.
	if c.compress {
		if gz := gzipBytes(data); len(gz) < len(data) {
			data, enc = gz, "gzip"
		}
	}
	// Retry transient failures — a blob PUT is idempotent (content-addressed), and
	// the body reader is rebuilt each attempt since c.limit consumes it.
	return c.retry(func() (int, error) {
		req, err := http.NewRequest(method, c.base+path, c.limit(bytes.NewReader(data)))
		if err != nil {
			return 0, err
		}
		req.ContentLength = int64(len(data)) // body is a custom reader; set length explicitly
		req.Header.Set("Content-Type", "application/octet-stream")
		if enc != "" {
			req.Header.Set("Content-Encoding", enc)
		}
		req.Header.Set(proto.AuthHeader, "Bearer "+c.bearer)
		resp, err := c.hc.Do(req)
		if err != nil {
			return 0, err
		}
		defer resp.Body.Close()
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			return resp.StatusCode, err
		}
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			return resp.StatusCode, apiError(resp.StatusCode, body)
		}
		return resp.StatusCode, nil
	})
}

// HubDate fetches the hub's /healthz endpoint (unauthenticated) and returns the
// Date header so callers can detect clock skew between device and hub.
func (c *Client) HubDate() (time.Time, error) {
	resp, err := c.hc.Get(c.base + "/healthz")
	if err != nil {
		return time.Time{}, err
	}
	resp.Body.Close()
	dh := resp.Header.Get("Date")
	if dh == "" {
		return time.Time{}, fmt.Errorf("devbox: hub /healthz response missing Date header")
	}
	return http.ParseTime(dh)
}

// apiError turns a non-2xx body into an error, preferring proto.Error's message.
func apiError(code int, body []byte) error {
	var e proto.Error
	if json.Unmarshal(body, &e) == nil && e.Error != "" {
		return fmt.Errorf("devbox: %s (status %d)", e.Error, code)
	}
	return fmt.Errorf("devbox: %s (status %d)", strings.TrimSpace(string(body)), code)
}
