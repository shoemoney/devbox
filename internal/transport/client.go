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
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"golang.org/x/time/rate"

	"git.shoemoney.ai/shoemoney/devbox/pkg/proto"
)

// Client talks to one hub. The zero value is not usable; call New.
type Client struct {
	base    string // hub base URL, e.g. "http://host:8080"
	bearer  string // device bearer token; empty until Join
	hc      *http.Client
	limiter *rate.Limiter // optional blob-transfer rate cap; nil = unlimited
}

// New returns a Client for the hub at base (e.g. "http://host:8080").
func New(base string) *Client {
	return &Client{
		base: strings.TrimRight(base, "/"),
		hc:   &http.Client{Timeout: 30 * time.Second},
	}
}

// SetBearer sets the device bearer token used for authenticated requests.
func (c *Client) SetBearer(b string) { c.bearer = b }

// Bearer returns the current device bearer token.
func (c *Client) Bearer() string { return c.bearer }

// Join redeems a one-time token and enrolls this device. It is unauthenticated;
// on success it stashes the returned bearer via SetBearer.
func (c *Client) Join(token, name string, pubkey []byte) (proto.JoinResponse, error) {
	var resp proto.JoinResponse
	err := c.do(http.MethodPost, proto.PathJoin, false, proto.JoinRequest{
		Token: token, Name: name, Pubkey: pubkey,
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

// GetBlob downloads one blob's raw bytes by content hash (used by pull).
func (c *Client) GetBlob(hash string) ([]byte, error) {
	req, err := http.NewRequest(http.MethodGet, c.base+proto.PathBlob+hash, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set(proto.AuthHeader, "Bearer "+c.bearer)
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(c.limit(resp.Body))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, apiError(resp.StatusCode, body)
	}
	return body, nil
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
	req, err := http.NewRequest(method, c.base+path, c.limit(bytes.NewReader(data)))
	if err != nil {
		return err
	}
	req.ContentLength = int64(len(data)) // body is a custom reader; set length explicitly
	req.Header.Set("Content-Type", "application/octet-stream")
	req.Header.Set(proto.AuthHeader, "Bearer "+c.bearer)
	resp, err := c.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return apiError(resp.StatusCode, body)
	}
	return nil
}

// apiError turns a non-2xx body into an error, preferring proto.Error's message.
func apiError(code int, body []byte) error {
	var e proto.Error
	if json.Unmarshal(body, &e) == nil && e.Error != "" {
		return fmt.Errorf("devbox: %s (status %d)", e.Error, code)
	}
	return fmt.Errorf("devbox: %s (status %d)", strings.TrimSpace(string(body)), code)
}
