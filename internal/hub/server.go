// Package hub is the devbox hub HTTP server: device enrollment, content-addressed
// blob upload, snapshot push, and head queries over JSON/HTTP with bearer auth.
package hub

import (
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
	"time"

	"git.shoemoney.ai/shoemoney/devbox/internal/chunk"
	"git.shoemoney.ai/shoemoney/devbox/internal/hub/blobstore"
	"git.shoemoney.ai/shoemoney/devbox/internal/hub/meta"
	"git.shoemoney.ai/shoemoney/devbox/internal/identity"
	"git.shoemoney.ai/shoemoney/devbox/pkg/proto"
)

// Server is the devbox hub: metadata in db, blob bytes in store.
type Server struct {
	db    *meta.DB
	store blobstore.Store
}

// NewServer builds a hub server over the given metadata store and blob store.
func NewServer(db *meta.DB, store blobstore.Store) *Server {
	return &Server{db: db, store: store}
}

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
	mux.HandleFunc("POST "+proto.PathHave, s.auth(s.handleHave))
	mux.HandleFunc("PUT "+proto.PathBlob+"{hash}", s.auth(s.handleBlob))
	mux.HandleFunc("GET "+proto.PathBlob+"{hash}", s.auth(s.handleGetBlob))
	mux.HandleFunc("POST "+proto.PathPush, s.auth(s.handlePush))
	mux.HandleFunc("GET "+proto.PathHead, s.auth(s.handleHead))
	mux.HandleFunc("GET "+proto.PathMetrics, s.handleMetrics)
	return mux
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
	writeJSON(w, http.StatusOK, proto.JoinResponse{DeviceID: deviceID, Bearer: bearer})
}

func (s *Server) handlePublish(w http.ResponseWriter, r *http.Request, deviceID string) {
	var req proto.PublishRequest
	if !decode(w, r, &req) {
		return
	}
	if err := s.db.CreateShare(req.Share, deviceID, time.Now().Unix()); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.WriteHeader(http.StatusOK)
}

func (s *Server) handleHave(w http.ResponseWriter, r *http.Request, _ string) {
	var req proto.HaveRequest
	if !decode(w, r, &req) {
		return
	}
	var missing []string
	for _, h := range req.Hashes {
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
	b, err := s.store.Get(r.PathValue("hash"))
	if errors.Is(err, blobstore.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "no such blob")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Write(b)
}

func (s *Server) handleBlob(w http.ResponseWriter, r *http.Request, _ string) {
	hash := r.PathValue("hash")
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
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
	w.WriteHeader(http.StatusOK)
}

func (s *Server) handlePush(w http.ResponseWriter, r *http.Request, deviceID string) {
	var req proto.PushRequest
	if !decode(w, r, &req) {
		return
	}
	writable, err := s.db.Writable(deviceID, req.Share)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if !writable {
		writeErr(w, http.StatusForbidden, "device is read-only on this share")
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
	writeJSON(w, http.StatusOK, proto.PushResponse{Snapshot: snapshotID, Head: snapshotID, Conflict: false})
}

// missingBlob reports whether the store lacks a blob with this hash.
func (s *Server) missingBlob(hash string) (bool, error) {
	has, err := s.store.Has(hash)
	return !has, err
}

func (s *Server) handleHead(w http.ResponseWriter, r *http.Request, _ string) {
	head, _, err := s.db.ShareHead(r.URL.Query().Get("share"))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, proto.HeadResponse{Head: head})
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
}

// randomBearer returns 32 random bytes hex-encoded.
func randomBearer() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// decode reads a JSON request body into v, writing a 400 on failure.
func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
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
