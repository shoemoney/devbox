// Package proto defines devbox's hub<->device wire types and endpoint paths.
// Transport is JSON over HTTP (blobs are raw bytes). One TLS endpoint, bearer
// auth. WebSocket change events arrive in M3.
package proto

// Endpoint paths (all under the hub base URL).
const (
	PathJoin    = "/v1/join"    // POST, no auth: redeem a join token, enroll a device
	PathPublish = "/v1/publish" // POST, auth: create a share
	PathHave    = "/v1/have"    // POST, auth: which chunk hashes is the hub missing?
	PathBlob    = "/v1/blob/"   // PUT /v1/blob/{hash}, auth: upload one blob's bytes
	PathPush    = "/v1/push"    // POST, auth: commit a snapshot
	PathHead    = "/v1/head"    // GET ?share=, auth: current head snapshot id
	PathLog     = "/v1/log"     // GET ?share=, auth: snapshot history
	PathEvents  = "/v1/events"  // GET ?share=, auth: SSE stream of change events
	PathMetrics = "/metrics"    // GET, no auth: Prometheus text exposition
)

// SnapshotInfo is one entry in a share's history.
type SnapshotInfo struct {
	ID        string `json:"id"`
	Parent    string `json:"parent"`
	Device    string `json:"device"`
	CreatedAt int64  `json:"created_at"`
}

// LogResponse is a share's snapshot history, newest first.
type LogResponse struct {
	Snapshots []SnapshotInfo `json:"snapshots"`
}

// Event is a hub change notification, delivered as one SSE "data:" line (JSON).
type Event struct {
	Share    string `json:"share"`
	Snapshot string `json:"snapshot"`
}

// AuthHeader carries the device bearer token: "Authorization: Bearer <token>".
const AuthHeader = "Authorization"

// JoinRequest enrolls a device by redeeming a one-time join token.
type JoinRequest struct {
	Token  string `json:"token"`
	Name   string `json:"name"`
	Pubkey []byte `json:"pubkey"` // ed25519 public key
}

// JoinResponse returns the device id and the bearer token for future requests.
type JoinResponse struct {
	DeviceID string `json:"device_id"`
	Bearer   string `json:"bearer"`
}

// PublishRequest creates a share owned by the calling device.
type PublishRequest struct {
	Share string `json:"share"`
}

// HaveRequest asks the hub which of these chunk hashes it does NOT already have.
type HaveRequest struct {
	Hashes []string `json:"hashes"`
}

// HaveResponse lists the hashes the hub is missing (and the client must upload).
type HaveResponse struct {
	Missing []string `json:"missing"`
}

// ChunkRef is a chunk a snapshot references.
type ChunkRef struct {
	Hash string `json:"hash"`
	Size int64  `json:"size"`
}

// PushRequest commits a snapshot. The manifest blob (key = ManifestHash) and all
// referenced chunk blobs must already be uploaded via PathBlob.
type PushRequest struct {
	Share        string     `json:"share"`
	Parent       string     `json:"parent"`        // client's last-known head ("" if none)
	ManifestHash string     `json:"manifest_hash"` // hash of the uploaded manifest blob
	Chunks       []ChunkRef `json:"chunks"`        // every distinct chunk + size
}

// PushResponse reports the new snapshot id and head. Conflict handling is M3
// (Conflict is always false in M2's one-way push).
type PushResponse struct {
	Snapshot string `json:"snapshot"`
	Head     string `json:"head"`
	Conflict bool   `json:"conflict"`
}

// HeadResponse is the current head snapshot id of a share ("" if none).
type HeadResponse struct {
	Head string `json:"head"`
}

// Error is the JSON body returned for non-2xx responses.
type Error struct {
	Error string `json:"error"`
}
