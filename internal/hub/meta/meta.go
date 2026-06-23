// Package meta is the hub's SQLite metadata store: devices, join tokens, shares,
// per-(device,share) write access, snapshots, and chunk refcounts. Blob bytes
// live in the blobstore; this tracks who/what/which-version and drives GC.
//
// Pure-Go driver (modernc.org/sqlite) so the hub cross-compiles without CGO.
package meta

import (
	"database/sql"
	"errors"

	_ "modernc.org/sqlite"
)

const schema = `
PRAGMA journal_mode=WAL;
PRAGMA foreign_keys=ON;
CREATE TABLE IF NOT EXISTS devices (
  id        TEXT PRIMARY KEY,
  name      TEXT NOT NULL,
  pubkey    BLOB NOT NULL,
  last_seen INTEGER,
  revoked   INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS tokens (
  hash       TEXT PRIMARY KEY,
  expires_at INTEGER,
  used       INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS shares (
  name          TEXT PRIMARY KEY,
  head_snapshot TEXT,
  created_by    TEXT NOT NULL,
  created_at    INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS access (
  device_id TEXT NOT NULL,
  share     TEXT NOT NULL,
  writable  INTEGER NOT NULL DEFAULT 1,
  PRIMARY KEY (device_id, share)
);
CREATE TABLE IF NOT EXISTS snapshots (
  id            TEXT PRIMARY KEY,
  share         TEXT NOT NULL,
  parent_id     TEXT,
  device_id     TEXT NOT NULL,
  created_at    INTEGER NOT NULL,
  manifest_hash TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS chunks (
  hash     TEXT PRIMARY KEY,
  size     INTEGER NOT NULL,
  refcount INTEGER NOT NULL DEFAULT 0
);
`

// DB is the hub metadata store.
type DB struct{ sql *sql.DB }

// Open opens (and migrates) the SQLite database at path. Use ":memory:" in tests.
func Open(path string) (*DB, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1) // ponytail: single-writer; SQLite serializes anyway
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, err
	}
	return &DB{sql: db}, nil
}

// Close closes the database.
func (d *DB) Close() error { return d.sql.Close() }

// --- devices -------------------------------------------------------------

// AddDevice registers (or updates) a device by id.
func (d *DB) AddDevice(id, name string, pubkey []byte, lastSeen int64) error {
	_, err := d.sql.Exec(
		`INSERT INTO devices (id, name, pubkey, last_seen) VALUES (?,?,?,?)
		 ON CONFLICT(id) DO UPDATE SET name=excluded.name, last_seen=excluded.last_seen`,
		id, name, pubkey, lastSeen)
	return err
}

// Revoked reports whether a device is unknown or revoked (i.e. not allowed).
func (d *DB) Revoked(id string) (bool, error) {
	var revoked int
	err := d.sql.QueryRow(`SELECT revoked FROM devices WHERE id=?`, id).Scan(&revoked)
	if errors.Is(err, sql.ErrNoRows) {
		return true, nil
	}
	if err != nil {
		return true, err
	}
	return revoked != 0, nil
}

// Revoke marks a device revoked; its access ends immediately.
func (d *DB) Revoke(id string) error {
	_, err := d.sql.Exec(`UPDATE devices SET revoked=1 WHERE id=?`, id)
	return err
}

// --- tokens --------------------------------------------------------------

// CreateToken stores a join token hash with an optional expiry (0 = none).
func (d *DB) CreateToken(hash string, expiresAt int64) error {
	_, err := d.sql.Exec(`INSERT INTO tokens (hash, expires_at) VALUES (?,?)`, hash, nullable(expiresAt))
	return err
}

// RedeemToken atomically consumes a token: valid only if present, unused, and
// unexpired at now. Returns true on success.
func (d *DB) RedeemToken(hash string, now int64) (bool, error) {
	res, err := d.sql.Exec(
		`UPDATE tokens SET used=1 WHERE hash=? AND used=0 AND (expires_at IS NULL OR expires_at > ?)`,
		hash, now)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// --- shares & access -----------------------------------------------------

// CreateShare creates a share (no-op if it already exists).
func (d *DB) CreateShare(name, createdBy string, createdAt int64) error {
	_, err := d.sql.Exec(
		`INSERT INTO shares (name, created_by, created_at) VALUES (?,?,?)
		 ON CONFLICT(name) DO NOTHING`, name, createdBy, createdAt)
	return err
}

// ShareHead returns the current head snapshot id of a share (ok=false if the
// share is missing or has no snapshots yet).
func (d *DB) ShareHead(name string) (head string, ok bool, err error) {
	var h sql.NullString
	err = d.sql.QueryRow(`SELECT head_snapshot FROM shares WHERE name=?`, name).Scan(&h)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return h.String, h.Valid && h.String != "", nil
}

// Writable reports whether a device may push to a share. Default is true; an
// access row with writable=0 makes the device read-only on that share.
func (d *DB) Writable(deviceID, share string) (bool, error) {
	var w int
	err := d.sql.QueryRow(`SELECT writable FROM access WHERE device_id=? AND share=?`, deviceID, share).Scan(&w)
	if errors.Is(err, sql.ErrNoRows) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	return w != 0, nil
}

// SetWritable sets a device's write access on a share (the only access knob).
func (d *DB) SetWritable(deviceID, share string, writable bool) error {
	w := 0
	if writable {
		w = 1
	}
	_, err := d.sql.Exec(
		`INSERT INTO access (device_id, share, writable) VALUES (?,?,?)
		 ON CONFLICT(device_id, share) DO UPDATE SET writable=excluded.writable`,
		deviceID, share, w)
	return err
}

// --- snapshots & chunks --------------------------------------------------

// Snapshot is one accepted change set on a share.
type Snapshot struct {
	ID           string
	Share        string
	ParentID     string // "" for the first snapshot
	DeviceID     string
	ManifestHash string
	CreatedAt    int64
}

// ChunkRef is a chunk a snapshot references.
type ChunkRef struct {
	Hash string
	Size int64
}

// AddSnapshot records a snapshot, bumps the refcount of each distinct chunk it
// references, and advances the share's head — all in one transaction.
func (d *DB) AddSnapshot(s Snapshot, chunks []ChunkRef) error {
	tx, err := d.sql.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if _, err := tx.Exec(
		`INSERT INTO snapshots (id, share, parent_id, device_id, created_at, manifest_hash)
		 VALUES (?,?,?,?,?,?)`,
		s.ID, s.Share, nullableStr(s.ParentID), s.DeviceID, s.CreatedAt, s.ManifestHash); err != nil {
		return err
	}
	seen := make(map[string]bool, len(chunks))
	for _, c := range chunks {
		if seen[c.Hash] { // count each chunk once per snapshot
			continue
		}
		seen[c.Hash] = true
		if _, err := tx.Exec(
			`INSERT INTO chunks (hash, size, refcount) VALUES (?,?,1)
			 ON CONFLICT(hash) DO UPDATE SET refcount=refcount+1`,
			c.Hash, c.Size); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(`UPDATE shares SET head_snapshot=? WHERE name=?`, s.ID, s.Share); err != nil {
		return err
	}
	return tx.Commit()
}

// ChunkRefcount returns a chunk's refcount (0 if unknown). Used by tests and GC.
func (d *DB) ChunkRefcount(hash string) (int, error) {
	var rc int
	err := d.sql.QueryRow(`SELECT refcount FROM chunks WHERE hash=?`, hash).Scan(&rc)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return rc, err
}

func nullable(v int64) any {
	if v == 0 {
		return nil
	}
	return v
}

func nullableStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}
