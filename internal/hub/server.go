// Package hub is the devbox hub HTTP server: device enrollment, content-addressed
// blob upload, snapshot push, and head queries over JSON/HTTP with bearer auth.
package hub

import (
	"compress/gzip"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/shoemoney/devbox/internal/chunk"
	"github.com/shoemoney/devbox/internal/hub/blobstore"
	"github.com/shoemoney/devbox/internal/hub/dashboard"
	"github.com/shoemoney/devbox/internal/hub/meta"
	"github.com/shoemoney/devbox/internal/identity"
	"github.com/shoemoney/devbox/pkg/proto"
)

// Request body caps, enforced via http.MaxBytesReader to bound the DoS surface
// (an unbounded io.ReadAll / JSON decode would let a client OOM the hub).
const (
	maxBlobBytes = 256 << 20 // one chunk/manifest blob — generous
	maxJSONBytes = 8 << 20   // any JSON control request
)

// Server is the devbox hub: metadata in db, blob bytes in store, change events
// fanned out via broker.
type Server struct {
	db        *meta.DB
	store     blobstore.Store
	broker    *broker
	dash      *dashboard.Dashboard // optional live dashboard; nil = disabled (Emit is nil-safe)
	publishMu sync.Mutex           // serializes publish so the case-clash check + create is atomic
	version   string               // build version, surfaced at /healthz (set via SetVersion)

	// Prometheus counters (monotonic). bytesIn/Out are the WAN signal now the hub
	// is internet-reachable; pushes/conflicts show sync churn. Exposed at /metrics.
	bytesIn   atomic.Int64
	bytesOut  atomic.Int64
	pushes    atomic.Int64
	conflicts atomic.Int64
}

// NewServer builds a hub server over the given metadata store and blob store.
func NewServer(db *meta.DB, store blobstore.Store) *Server {
	return &Server{db: db, store: store, broker: newBroker()}
}

// SetVersion records the build version for the /healthz response.
func (s *Server) SetVersion(v string) *Server { s.version = v; return s }

// WithDashboard attaches a live dashboard so the hub emits flow events to it.
func (s *Server) WithDashboard(d *dashboard.Dashboard) *Server { s.dash = d; return s }

// HashToken returns hex(sha256(token)); the token-mint CLI reuses it so the hub
// only ever stores token hashes, never the tokens themselves.
func HashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// hashBearer hashes a bearer the same way, so the hub stores only the hash (no
// plaintext credentials at rest) and lookup compares fixed-width hashes.
func hashBearer(bearer string) string {
	sum := sha256.Sum256([]byte(bearer))
	return hex.EncodeToString(sum[:])
}

// Handler returns the hub's HTTP routes.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST "+proto.PathJoin, s.handleJoin)
	mux.HandleFunc("POST "+proto.PathPublish, s.auth(s.handlePublish))
	mux.HandleFunc("POST "+proto.PathInviteRevoke, s.auth(s.handleInviteRevoke))
	mux.HandleFunc("POST "+proto.PathHave, s.auth(s.handleHave))
	mux.HandleFunc("PUT "+proto.PathBlob+"{hash}", s.auth(s.handleBlob))
	mux.HandleFunc("GET "+proto.PathBlob+"{hash}", s.auth(s.handleGetBlob))
	mux.HandleFunc("POST "+proto.PathPush, s.auth(s.handlePush))
	mux.HandleFunc("GET "+proto.PathHead, s.auth(s.handleHead))
	mux.HandleFunc("GET "+proto.PathLog, s.auth(s.handleLog))
	mux.HandleFunc("GET "+proto.PathMembers, s.auth(s.handleMembers))
	mux.HandleFunc("POST "+proto.PathInvite, s.auth(s.handleInvite))
	mux.HandleFunc("GET "+proto.PathEvents, s.auth(s.handleEvents))
	mux.HandleFunc("GET "+proto.PathMetrics, s.handleMetrics)
	mux.HandleFunc("GET /healthz", s.handleHealthz)
	mux.HandleFunc("GET /readyz", s.handleReadyz)
	return mux
}

// handleHealthz is a liveness probe: the process is up and serving. No auth, no
// I/O — for Docker HEALTHCHECK and load balancers (the compose selfheal profile
// restarts on unhealthy). /readyz is the deeper check.
func (s *Server) handleHealthz(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	if s.version != "" {
		fmt.Fprintf(w, "ok %s\n", s.version) // lets a deploy verify the new build is live
		return
	}
	fmt.Fprintln(w, "ok")
}

// handleReadyz is a readiness probe: 200 only if the metadata DB answers a cheap
// query, else 503 so an orchestrator can hold traffic off a hub that's up but
// whose DB is locked/unreachable (which /metrics, also a Count, can't signal as 503).
func (s *Server) handleReadyz(w http.ResponseWriter, _ *http.Request) {
	if _, err := s.db.Count("devices"); err != nil {
		http.Error(w, "not ready: "+err.Error(), http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	fmt.Fprintln(w, "ready")
}

// auth wraps a handler, requiring a valid "Authorization: Bearer <tok>" that
// resolves to a non-revoked device. The device id is stashed in the request
// context for the wrapped handler.
func (s *Server) auth(next func(http.ResponseWriter, *http.Request, string)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		const prefix = "Bearer "
		h := r.Header.Get(proto.AuthHeader)
		if len(h) <= len(prefix) || h[:len(prefix)] != prefix {
			writeErr(w, http.StatusUnauthorized, "missing bearer token")
			return
		}
		deviceID, ok, err := s.db.DeviceByBearer(hashBearer(h[len(prefix):]))
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		if !ok {
			writeErr(w, http.StatusUnauthorized, "invalid bearer token")
			return
		}
		next(w, r, deviceID)
	}
}

func (s *Server) handleJoin(w http.ResponseWriter, r *http.Request) {
	var req proto.JoinRequest
	if !decode(w, r, &req) {
		return
	}
	now := time.Now().Unix()

	// Proof of possession: the device id is derived from req.Pubkey, so require a
	// signature proving the joiner actually holds that key. Checked BEFORE
	// redeeming the token so a bad request can't burn a legitimate one-time token.
	if len(req.Pubkey) != ed25519.PublicKeySize ||
		!ed25519.Verify(ed25519.PublicKey(req.Pubkey), proto.JoinChallenge(req.Token, req.Pubkey), req.Signature) {
		writeErr(w, http.StatusUnauthorized, "join signature invalid")
		return
	}
	deviceID := identity.FingerprintOf(ed25519.PublicKey(req.Pubkey))

	ok, err := s.db.RedeemToken(HashToken(req.Token), now)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if !ok {
		writeErr(w, http.StatusUnauthorized, "invalid or expired join token")
		return
	}
	if err := s.db.AddDevice(deviceID, req.Name, req.Pubkey, now); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	bearer, err := randomBearer()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if err := s.db.IssueBearer(deviceID, hashBearer(bearer)); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	// If this was an invite token (M8a), apply its binding: the device joins as the
	// bound principal with the bound role on the share. The grant was already
	// attenuation-checked when the invite was minted (handleInvite). A plain join
	// token has no binding → v1 behavior (device stays principal 'owner').
	if inv, ok, err := s.db.InviteBinding(HashToken(req.Token)); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	} else if ok {
		if err := s.db.SetDevicePrincipal(deviceID, inv.Principal); err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		if err := s.db.SetMember(inv.Share, inv.Principal, inv.Role, inv.CanReshare); err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
	}
	s.dash.Emit(dashboard.Event{Type: "join", Device: deviceID, DeviceName: req.Name})
	writeJSON(w, http.StatusOK, proto.JoinResponse{DeviceID: deviceID, Bearer: bearer})
}

// shortID truncates a content hash for the dashboard's compact event payloads.
func shortID(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}

// handleInvite mints an invite token for a share (M8a). The caller's effective
// role + reshare bit must permit the requested grant (attenuation, enforced here
// server-side so a compromised client can't escalate). On a legacy share the
// caller is an implicit owner: minting an invite seeds the caller's own principal
// as owner and flips the share to explicit, so the caller never locks themselves
// (or their other devices, same principal) out.
func (s *Server) handleInvite(w http.ResponseWriter, r *http.Request, deviceID string) {
	var req proto.InviteRequest
	if !decode(w, r, &req) {
		return
	}
	grantRole, ok := meta.ParseRole(req.Role)
	if !ok {
		writeErr(w, http.StatusBadRequest, "unknown role "+req.Role)
		return
	}
	if req.Principal == "" {
		writeErr(w, http.StatusBadRequest, "missing principal")
		return
	}

	// Serialize with publish so the legacy→explicit flip + self-seed is atomic
	// against a concurrent first grant.
	s.publishMu.Lock()
	defer s.publishMu.Unlock()

	callerRole, callerReshare, explicit, err := s.db.EffectiveMember(deviceID, req.Share)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	targetCurrent, err := s.db.RoleOf(req.Share, req.Principal)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if !meta.MayGrant(callerRole, callerReshare, targetCurrent, grantRole, req.Reshare) {
		writeErr(w, http.StatusForbidden, "your role may not grant this")
		return
	}
	// Preserve the caller's access across the legacy→explicit flip: seed the
	// caller's principal as owner before the share stops being open.
	if !explicit {
		callerPrincipal, err := s.db.DevicePrincipal(deviceID)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		if err := s.db.SetMember(req.Share, callerPrincipal, meta.RoleOwner, true); err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
	}
	if err := s.db.EnsurePrincipal(req.Principal, req.Principal, time.Now().Unix()); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	tok, err := randomBearer()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	inv := meta.Invite{Principal: req.Principal, Share: req.Share, Role: grantRole, CanReshare: req.Reshare}
	if err := s.db.CreateInvite(HashToken(tok), time.Now().Add(24*time.Hour).Unix(), inv); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, proto.InviteResponse{Token: tok})
}

// handleInviteRevoke kills a still-pending invite by its raw token. Only a caller
// who could have MINTED it (same MayGrant authority on the bound share) may revoke
// it — so an invite can't be cancelled by someone who couldn't have issued it, and
// the binding isn't probeable by the unauthorized.
func (s *Server) handleInviteRevoke(w http.ResponseWriter, r *http.Request, deviceID string) {
	var req proto.InviteRevokeRequest
	if !decode(w, r, &req) {
		return
	}
	hash := HashToken(req.Token)
	inv, ok, err := s.db.InviteBinding(hash)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if !ok {
		writeErr(w, http.StatusNotFound, "no such invite")
		return
	}
	callerRole, callerReshare, _, err := s.db.EffectiveMember(deviceID, inv.Share)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if !meta.MayGrant(callerRole, callerReshare, 0, inv.Role, inv.CanReshare) {
		writeErr(w, http.StatusForbidden, "your role may not revoke this invite")
		return
	}
	killed, err := s.db.RevokeInvite(hash)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if !killed {
		writeErr(w, http.StatusConflict, "invite already redeemed or revoked")
		return
	}
	writeJSON(w, http.StatusOK, proto.InviteRevokeResponse{Revoked: true})
}

func (s *Server) handlePublish(w http.ResponseWriter, r *http.Request, deviceID string) {
	var req proto.PublishRequest
	if !decode(w, r, &req) {
		return
	}
	// Serialize: the case-clash check below reads all names then creates one, so
	// two concurrent publishes of "Foo"/"foo" must not both pass the check.
	s.publishMu.Lock()
	defer s.publishMu.Unlock()
	existing, err := s.db.ShareNames()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if err := ValidateShareName(req.Share, existing); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := s.db.CreateShare(req.Share, deviceID, time.Now().Unix()); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.WriteHeader(http.StatusOK)
}

// validHash reports whether h is a well-formed content hash (BLAKE3-256 hex).
// Blob keys flow into filesystem paths, so anything else (../, %2f-decoded
// slashes, absolute paths) is rejected at the trust boundary before it can
// escape the blob root. Legit keys are always 64 lowercase hex chars.
func validHash(h string) bool {
	if len(h) != 64 {
		return false
	}
	for i := 0; i < len(h); i++ {
		c := h[i]
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			return false
		}
	}
	return true
}

func (s *Server) handleHave(w http.ResponseWriter, r *http.Request, _ string) {
	var req proto.HaveRequest
	if !decode(w, r, &req) {
		return
	}
	var missing []string
	for _, h := range req.Hashes {
		if !validHash(h) {
			writeErr(w, http.StatusBadRequest, "invalid hash")
			return
		}
		has, err := s.store.Has(h)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		if !has {
			missing = append(missing, h)
		}
	}
	writeJSON(w, http.StatusOK, proto.HaveResponse{Missing: missing})
}

// handleGetBlob serves a blob's bytes for download (pull fetches manifests + chunks).
func (s *Server) handleGetBlob(w http.ResponseWriter, r *http.Request, _ string) {
	hash := r.PathValue("hash")
	if !validHash(hash) {
		// ServeMux URL-decodes %2f/%2e in the {hash} wildcard, so an unvalidated
		// key like "..%2f..%2fetc%2fpasswd" would escape the blob root. Reject as
		// "not found" — an invalid hash is, by definition, not a stored blob.
		writeErr(w, http.StatusNotFound, "no such blob")
		return
	}
	b, err := s.store.Get(hash)
	if errors.Is(err, blobstore.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "no such blob")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.bytesOut.Add(int64(len(b)))
	w.Header().Set("Content-Type", "application/octet-stream")
	// Gzip the response when the client negotiated it (settings.transfer.compress on
	// the device → WAN). The blob is already in memory, so this just streams it
	// through a gzip.Writer. ponytail: no shrink-check — the client only asks when it
	// expects a win; a tiny incompressible chunk may inflate a few bytes, which is
	// noise next to the WAN round-trip it's trying to save.
	if strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
		w.Header().Set("Content-Encoding", "gzip")
		gz := gzip.NewWriter(w)
		defer gz.Close()
		gz.Write(b)
		return
	}
	w.Write(b)
}

func (s *Server) handleBlob(w http.ResponseWriter, r *http.Request, _ string) {
	hash := r.PathValue("hash")
	// MaxBytesReader caps the COMPRESSED stream (wire DoS bound).
	var src io.Reader = http.MaxBytesReader(w, r.Body, maxBlobBytes)
	if r.Header.Get("Content-Encoding") == "gzip" {
		gz, err := gzip.NewReader(src)
		if err != nil {
			writeErr(w, http.StatusBadRequest, "bad gzip body")
			return
		}
		defer gz.Close()
		// Also cap the DECOMPRESSED size (+1 to detect overflow) so a gzip bomb
		// can't OOM the hub; a blob is ≤ maxBlobBytes uncompressed by definition.
		src = io.LimitReader(gz, maxBlobBytes+1)
	}
	body, err := io.ReadAll(src)
	if err != nil {
		// MaxBytesReader trips here when the body exceeds maxBlobBytes.
		writeErr(w, http.StatusRequestEntityTooLarge, err.Error())
		return
	}
	if len(body) > maxBlobBytes {
		writeErr(w, http.StatusRequestEntityTooLarge, "decompressed blob too large")
		return
	}
	if chunk.Hash(body) != hash {
		writeErr(w, http.StatusBadRequest, "body does not match hash")
		return
	}
	if err := s.store.Put(hash, body); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.bytesIn.Add(int64(len(body)))
	w.WriteHeader(http.StatusOK)
}

func (s *Server) handlePush(w http.ResponseWriter, r *http.Request, deviceID string) {
	var req proto.PushRequest
	if !decode(w, r, &req) {
		return
	}
	// Write gate (M8a): the device's effective role must be >= editor AND its
	// access.writable clamp must allow it. In legacy shares (no member grants)
	// every device is an implicit owner, so this reduces to the v1 writable bit;
	// an explicit-ACL share is deny-by-default.
	writable, err := s.db.Writable(deviceID, req.Share)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	role, _, _, err := s.db.EffectiveMember(deviceID, req.Share)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if !writable || role < meta.RoleEditor {
		writeErr(w, http.StatusForbidden, "device not permitted to write to this share")
		return
	}

	// Conflict detection: a push must extend the current head (or replay it).
	// Re-pushing the existing head is idempotent; a push whose parent IS the
	// current head fast-forwards; anything else means the device is behind and
	// must pull + reconcile (M3) before pushing again — we never clobber head.
	head, _, err := s.db.ShareHead(req.Share)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if req.ManifestHash != head && req.Parent != head {
		s.conflicts.Add(1)
		s.dash.Emit(dashboard.Event{Type: "conflict", Device: deviceID, Share: req.Share})
		writeJSON(w, http.StatusOK, proto.PushResponse{Head: head, Conflict: true})
		return
	}

	// The manifest blob and every referenced chunk must already be uploaded.
	if missing, err := s.missingBlob(req.ManifestHash); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	} else if missing {
		writeErr(w, http.StatusConflict, "missing manifest blob "+req.ManifestHash)
		return
	}
	chunks := make([]meta.ChunkRef, 0, len(req.Chunks))
	for _, c := range req.Chunks {
		if missing, err := s.missingBlob(c.Hash); err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		} else if missing {
			writeErr(w, http.StatusConflict, "missing chunk blob "+c.Hash)
			return
		}
		chunks = append(chunks, meta.ChunkRef{Hash: c.Hash, Size: c.Size})
	}

	snapshotID := req.ManifestHash
	snap := meta.Snapshot{
		ID:           snapshotID,
		Share:        req.Share,
		ParentID:     req.Parent,
		DeviceID:     deviceID,
		ManifestHash: req.ManifestHash,
		CreatedAt:    time.Now().Unix(),
	}
	if err := s.db.AddSnapshot(snap, chunks); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	// Only announce a genuine head advance. An idempotent re-push (manifest ==
	// current head) must NOT broadcast, or daemons would storm: pull -> re-push
	// the same state -> broadcast -> pull -> ... forever.
	advanced := req.ManifestHash != head
	if advanced {
		s.broker.publish(proto.Event{Share: req.Share, Snapshot: snapshotID})
	}
	var bytes int64
	for _, c := range chunks {
		bytes += c.Size
	}
	s.dash.Emit(dashboard.Event{
		Type: "push", Device: deviceID, Share: req.Share,
		Bytes: bytes, Chunks: len(chunks), Snapshot: shortID(snapshotID), NewHead: advanced,
	})
	s.pushes.Add(1)
	writeJSON(w, http.StatusOK, proto.PushResponse{Snapshot: snapshotID, Head: snapshotID, Conflict: false})
}

// missingBlob reports whether the store lacks a blob with this hash.
func (s *Server) missingBlob(hash string) (bool, error) {
	has, err := s.store.Has(hash)
	return !has, err
}

func (s *Server) handleHead(w http.ResponseWriter, r *http.Request, deviceID string) {
	share := r.URL.Query().Get("share")
	head, _, err := s.db.ShareHead(share)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	// A head fetch that returns content is a device pulling/propagating state —
	// surface it on the dashboard (empty share = nothing to pull, so stay quiet).
	if head != "" {
		s.dash.Emit(dashboard.Event{Type: "pull", Device: deviceID, Share: share, Snapshot: shortID(head)})
	}
	writeJSON(w, http.StatusOK, proto.HeadResponse{Head: head})
}

func (s *Server) handleLog(w http.ResponseWriter, r *http.Request, _ string) {
	snaps, err := s.db.SnapshotLog(r.URL.Query().Get("share"), 100)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	out := make([]proto.SnapshotInfo, len(snaps))
	for i, s := range snaps {
		out[i] = proto.SnapshotInfo{ID: s.ID, Parent: s.Parent, Device: s.Device, CreatedAt: s.CreatedAt}
	}
	writeJSON(w, http.StatusOK, proto.LogResponse{Snapshots: out})
}

// handleMembers lists who can access a share (M8a). Reads stay open to any
// enrolled device in M8a — read-side gating is M9.
func (s *Server) handleMembers(w http.ResponseWriter, r *http.Request, _ string) {
	share := r.URL.Query().Get("share")
	mode, err := s.db.ACLMode(share)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	ms, err := s.db.Members(share)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	out := make([]proto.Member, len(ms))
	for i, m := range ms {
		out[i] = proto.Member{Principal: m.Principal, Role: meta.RoleName(m.Role), CanReshare: m.CanReshare}
	}
	writeJSON(w, http.StatusOK, proto.MembersResponse{Legacy: mode != "explicit", Members: out})
}

func (s *Server) handleMetrics(w http.ResponseWriter, _ *http.Request) {
	gauges := []struct {
		name, table, help string
	}{
		{"devbox_devices", "devices", "Number of enrolled devices."},
		{"devbox_shares", "shares", "Number of shares."},
		{"devbox_snapshots", "snapshots", "Number of snapshots."},
		{"devbox_chunks", "chunks", "Number of distinct chunks."},
	}
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	for _, g := range gauges {
		n, err := s.db.Count(g.table)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		fmt.Fprintf(w, "# HELP %s %s\n", g.name, g.help)
		fmt.Fprintf(w, "# TYPE %s gauge\n", g.name)
		fmt.Fprintf(w, "%s %s\n", g.name, strconv.FormatInt(n, 10))
	}

	counters := []struct {
		name, help string
		val        int64
	}{
		{"devbox_blob_bytes_in_total", "Total bytes received in blob uploads (stored, post-decompress).", s.bytesIn.Load()},
		{"devbox_blob_bytes_out_total", "Total bytes served in blob downloads (pre-compress).", s.bytesOut.Load()},
		{"devbox_pushes_total", "Total push commits accepted.", s.pushes.Load()},
		{"devbox_conflicts_total", "Total pushes rejected as stale-parent conflicts.", s.conflicts.Load()},
	}
	for _, c := range counters {
		fmt.Fprintf(w, "# HELP %s %s\n", c.name, c.help)
		fmt.Fprintf(w, "# TYPE %s counter\n", c.name)
		fmt.Fprintf(w, "%s %s\n", c.name, strconv.FormatInt(c.val, 10))
	}
}

// randomBearer returns 32 random bytes hex-encoded.
func randomBearer() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// decode reads a JSON request body into v, writing a 400 on failure (or a 413 if
// the body exceeds maxJSONBytes — bounding the decode DoS surface).
func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxJSONBytes)
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writeErr(w, http.StatusRequestEntityTooLarge, err.Error())
			return false
		}
		writeErr(w, http.StatusBadRequest, err.Error())
		return false
	}
	return true
}

// writeJSON writes v as JSON with the given status.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

// writeErr writes a proto.Error JSON body with the given status.
func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, proto.Error{Error: msg})
}
