// Package meta is the hub's SQLite metadata store: devices, join tokens, shares,
// per-(device,share) write access, snapshots, and chunk refcounts. Blob bytes
// live in the blobstore; this tracks who/what/which-version and drives GC.
//
// Pure-Go driver (modernc.org/sqlite) so the hub cross-compiles without CGO.
package meta

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strings"

	_ "modernc.org/sqlite"
)

// connPragmas are per-connection settings re-applied on every Open (foreign_keys
// is per-connection; journal_mode=WAL is persistent but harmless to re-assert).
const connPragmas = `
PRAGMA journal_mode=WAL;
PRAGMA busy_timeout=5000;
PRAGMA foreign_keys=ON;
`

// baselineSchema is the v1 (schema version 0) shape. It is CREATE ... IF NOT
// EXISTS so it is a no-op on any existing database and just seeds a fresh one;
// the migrations below then transform it. Never change a column here — that's
// what migrations are for; this only has to create v1 tables for new hubs.
const baselineSchema = `
CREATE TABLE IF NOT EXISTS devices (
  id        TEXT PRIMARY KEY,
  name      TEXT NOT NULL,
  pubkey    BLOB NOT NULL,
  bearer    TEXT,
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

// migrations are ordered, append-only schema transforms. migrations[i] upgrades
// the database from PRAGMA user_version i to i+1, each in its own transaction
// with the user_version bump as the LAST statement — so a crash mid-migration
// rolls the whole step back (the version only advances once the step committed).
// v1 is version 0 (baselineSchema, no migration); the first v2 step takes it to 1.
// NEVER edit or reorder a shipped entry — only append. A database whose version
// exceeds len(migrations) is refused (a newer devbox wrote it).
var migrations = []string{
	// 1 (v2): re-key snapshots from a GLOBAL `id` PK to per-`(share, id)`. v1's
	// global id meant identical content pushed to two shares collided on the same
	// snapshot row, so the second share's chunk refcounts were never counted
	// (undercount). Per-share rows let each share account its own chunks. The wire
	// `id`/Head value stays the manifest hash, so the client contract is unchanged.
	`
CREATE TABLE snapshots_v2 (
  share         TEXT NOT NULL,
  id            TEXT NOT NULL,
  parent_id     TEXT,
  device_id     TEXT NOT NULL,
  created_at    INTEGER NOT NULL,
  manifest_hash TEXT NOT NULL,
  PRIMARY KEY (share, id)
);
INSERT INTO snapshots_v2 (share, id, parent_id, device_id, created_at, manifest_hash)
  SELECT share, id, parent_id, device_id, created_at, manifest_hash FROM snapshots;
DROP TABLE snapshots;
ALTER TABLE snapshots_v2 RENAME TO snapshots;
-- Re-establish the per-share invariant the v1 idempotent push violated: every
-- non-empty share head must have its OWN snapshot row. The old global PK recorded
-- shared content under just the first share, so a share that reverted/re-pushed
-- another share's content has a head with no local row. Copy one from wherever the
-- id was recorded (parent/device/time are informational for log/restore).
INSERT OR IGNORE INTO snapshots (share, id, parent_id, device_id, created_at, manifest_hash)
  SELECT s.name, s.head_snapshot, src.parent_id, src.device_id, src.created_at, src.manifest_hash
  FROM shares s
  JOIN snapshots src ON src.id = s.head_snapshot
  WHERE s.head_snapshot IS NOT NULL AND s.head_snapshot != ''
    AND NOT EXISTS (SELECT 1 FROM snapshots o WHERE o.share = s.name AND o.id = s.head_snapshot);
`,
	// 2 (v2, M8a): identity + per-share roles. A *principal* (person/account) sits
	// above the device: the device stays the auth + revocation unit, the principal
	// is the authorization subject. Every v1 device backfills to ONE synthetic
	// `owner` principal, so behavior is byte-identical to v1 until a share opts into
	// explicit ACLs. shares.acl_mode='legacy' (the default) means every device is an
	// implicit owner (v1); the first member grant flips it to 'explicit' →
	// deny-by-default. The v1 `access.writable` bit stays a clamp that can only
	// REMOVE write, never add it. NO inline FK on the ALTER (SQLite requires a NULL
	// default for that) — integrity is maintained in the app.
	`
CREATE TABLE principals (
  id         TEXT PRIMARY KEY,
  name       TEXT NOT NULL,
  created_at INTEGER NOT NULL
);
INSERT INTO principals (id, name, created_at) VALUES ('owner', 'owner', 0);
ALTER TABLE devices ADD COLUMN principal_id TEXT NOT NULL DEFAULT 'owner';
CREATE TABLE members (
  share        TEXT NOT NULL,
  principal_id TEXT NOT NULL,
  role         INTEGER NOT NULL,
  can_reshare  INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY (share, principal_id)
);
ALTER TABLE shares ADD COLUMN acl_mode TEXT NOT NULL DEFAULT 'legacy';
`,
	// 3 (v2, M8a): invites. An invite is a normal one-time join token (so it rides
	// the existing /v1/join proof-of-possession flow) PLUS a binding recorded here:
	// when redeemed, the joining device is assigned the bound principal and granted
	// the bound role on the share. A plain join token has no invites row → v1 join.
	`
CREATE TABLE invites (
  token_hash   TEXT PRIMARY KEY,
  principal_id TEXT NOT NULL,
  share        TEXT NOT NULL,
  role         INTEGER NOT NULL,
  can_reshare  INTEGER NOT NULL DEFAULT 0
);
`,
}

// Role levels on a share (higher = more authority). A device's effective write
// right is role>=RoleEditor AND its access.writable clamp.
const (
	RoleViewer = 10
	RoleEditor = 20
	RoleAdmin  = 30
	RoleOwner  = 40
)

// roleNames is the canonical name<->level mapping shared by the CLI and wire.
var roleNames = map[string]int{"viewer": RoleViewer, "editor": RoleEditor, "admin": RoleAdmin, "owner": RoleOwner}

// ParseRole maps a role word to its level (ok=false if unknown).
func ParseRole(name string) (int, bool) { lvl, ok := roleNames[name]; return lvl, ok }

// RoleName maps a level back to its word (or "roleN" for an unknown level).
func RoleName(level int) string {
	for n, l := range roleNames {
		if l == level {
			return n
		}
	}
	return fmt.Sprintf("role%d", level)
}

// DB is the hub metadata store.
type DB struct{ sql *sql.DB }

// Open opens (and migrates) the SQLite database at path. Use ":memory:" in tests.
// An existing on-disk database is backed up to "<path>.pre-v2.bak" before any
// pending migration runs, and a database written by a newer devbox is refused.
func Open(path string) (*DB, error) {
	// Note whether a real database already exists, so we only spend a backup on
	// upgrades that have data to protect (a fresh hub has nothing to lose).
	preexisting := false
	if path != "" && path != ":memory:" {
		if fi, err := os.Stat(path); err == nil && fi.Size() > 0 {
			preexisting = true
		}
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1) // ponytail: single-writer; SQLite serializes anyway
	if _, err := db.Exec(connPragmas); err != nil {
		db.Close()
		return nil, err
	}
	if _, err := db.Exec(baselineSchema); err != nil {
		db.Close()
		return nil, err
	}
	if err := migrate(db, path, preexisting); err != nil {
		db.Close()
		return nil, err
	}
	return &DB{sql: db}, nil
}

// migrate applies every pending migration in order, each in its own transaction
// ending with the user_version bump (so a crash rolls a half-applied step back).
// It refuses a database whose version is beyond what this build knows about.
func migrate(db *sql.DB, path string, preexisting bool) error {
	var cur int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&cur); err != nil {
		return err
	}
	if cur > len(migrations) {
		return fmt.Errorf("hub database is schema version %d but this devbox supports up to %d — upgrade devbox", cur, len(migrations))
	}
	if cur == len(migrations) {
		return nil // already current
	}
	if preexisting {
		bak := path + ".pre-v2.bak"
		_ = os.Remove(bak)
		// VACUUM INTO writes a consistent snapshot of the live DB; it cannot run in
		// a transaction and won't take a bound parameter, so quote the path inline.
		if _, err := db.Exec(`VACUUM INTO '` + strings.ReplaceAll(bak, "'", "''") + `'`); err != nil {
			return fmt.Errorf("pre-migration backup to %s: %w", bak, err)
		}
	}
	for i := cur; i < len(migrations); i++ {
		tx, err := db.Begin()
		if err != nil {
			return err
		}
		if _, err := tx.Exec(migrations[i]); err != nil {
			tx.Rollback()
			return fmt.Errorf("migration to version %d: %w", i+1, err)
		}
		// user_version can't be parameterized; i+1 is an internal int, never input.
		if _, err := tx.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, i+1)); err != nil {
			tx.Rollback()
			return fmt.Errorf("set user_version %d: %w", i+1, err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("commit migration %d: %w", i+1, err)
		}
	}
	return nil
}

// Close closes the database.
func (d *DB) Close() error { return d.sql.Close() }

// BackupTo writes a consistent snapshot of the live database to dest using
// VACUUM INTO (SQLite online backup: runs outside a transaction, WAL included).
// Same quoting pattern as the pre-migration backup in migrate().
func (d *DB) BackupTo(dest string) error {
	_, err := d.sql.Exec(`VACUUM INTO '` + strings.ReplaceAll(dest, "'", "''") + `'`)
	return err
}

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

// Revoke marks a device revoked; its access (and bearer) ends immediately.
func (d *DB) Revoke(id string) error {
	_, err := d.sql.Exec(`UPDATE devices SET revoked=1 WHERE id=?`, id)
	return err
}

// IssueBearer sets a device's bearer token (used to authenticate requests).
func (d *DB) IssueBearer(deviceID, bearer string) error {
	_, err := d.sql.Exec(`UPDATE devices SET bearer=? WHERE id=?`, bearer, deviceID)
	return err
}

// DeviceByBearer resolves a bearer token to a non-revoked device id.
func (d *DB) DeviceByBearer(bearer string) (id string, ok bool, err error) {
	if bearer == "" {
		return "", false, nil
	}
	err = d.sql.QueryRow(`SELECT id FROM devices WHERE bearer=? AND revoked=0`, bearer).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return id, true, nil
}

// CountDevices, CountShares, CountSnapshots, CountChunks back the /metrics page.
func (d *DB) Count(table string) (int64, error) {
	var n int64
	// table is a fixed internal constant, never user input.
	err := d.sql.QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&n)
	return n, err
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

// --- principals, members & roles (M8a) -----------------------------------

// Member is one principal's role on a share.
type Member struct {
	Principal  string
	Role       int
	CanReshare bool
}

// EffectiveMember returns a device's role, its reshare (+s) right, and whether
// the share is in explicit-ACL mode. In legacy mode (the v1 default — no member
// grants) every known device is an implicit RoleOwner with reshare, so v1 behavior
// is preserved exactly. In explicit mode the device's principal must hold a member
// row, else role 0 (deny-by-default). An unknown/revoked device gets 0.
func (d *DB) EffectiveMember(deviceID, share string) (role int, canReshare, explicit bool, err error) {
	var pid string
	err = d.sql.QueryRow(`SELECT principal_id FROM devices WHERE id=? AND revoked=0`, deviceID).Scan(&pid)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, false, nil
	}
	if err != nil {
		return 0, false, false, err
	}
	mode, err := d.ACLMode(share)
	if err != nil {
		return 0, false, false, err
	}
	if mode != "explicit" {
		return RoleOwner, true, false, nil // implicit owner can reshare
	}
	var cr int
	err = d.sql.QueryRow(`SELECT role, can_reshare FROM members WHERE share=? AND principal_id=?`, share, pid).Scan(&role, &cr)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, true, nil // deny-by-default
	}
	if err != nil {
		return 0, false, true, err
	}
	return role, cr != 0, true, nil
}

// DevicePrincipal returns the principal a device belongs to.
func (d *DB) DevicePrincipal(deviceID string) (string, error) {
	var pid string
	err := d.sql.QueryRow(`SELECT principal_id FROM devices WHERE id=?`, deviceID).Scan(&pid)
	return pid, err
}

// RoleOf returns a principal's current role on a share (0 if none/legacy).
func (d *DB) RoleOf(share, principalID string) (int, error) {
	var role int
	err := d.sql.QueryRow(`SELECT role FROM members WHERE share=? AND principal_id=?`, share, principalID).Scan(&role)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return role, err
}

// MayGrant reports whether a caller holding callerRole (and the +s bit) may set a
// principal currently at targetCurrentRole to grantRole, optionally conferring the
// +s reshare bit (grantReshare). Pure attenuation: a compromised client can't
// escalate because the hub re-checks this server-side.
//   - grantRole must be a real role;
//   - viewers can't grant; editors need +s; admins/owners always may;
//   - you can never grant ABOVE your own role (delegation only narrows);
//   - you can never touch a principal who currently outranks you (no demoting up);
//   - you can only confer +s if you hold it yourself (owners are unconstrained) —
//     the reshare bit is a delegation power, so it attenuates like the role does.
func MayGrant(callerRole int, callerCanReshare bool, targetCurrentRole, grantRole int, grantReshare bool) bool {
	if grantRole < RoleViewer || grantRole > RoleOwner {
		return false
	}
	if callerRole < RoleEditor {
		return false
	}
	if callerRole < RoleAdmin && !callerCanReshare {
		return false
	}
	if grantReshare && !callerCanReshare && callerRole < RoleOwner {
		return false
	}
	return grantRole <= callerRole && targetCurrentRole <= callerRole
}

// Invite is the binding redeemed when an invite token is used at /v1/join.
type Invite struct {
	Principal  string
	Share      string
	Role       int
	CanReshare bool
}

// CreateInvite mints an invite: a redeemable join token (in tokens) plus its
// binding (in invites), in one transaction.
func (d *DB) CreateInvite(tokenHash string, expiresAt int64, inv Invite) error {
	tx, err := d.sql.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`INSERT INTO tokens (hash, expires_at) VALUES (?,?)`, tokenHash, nullable(expiresAt)); err != nil {
		return err
	}
	cr := 0
	if inv.CanReshare {
		cr = 1
	}
	if _, err := tx.Exec(
		`INSERT INTO invites (token_hash, principal_id, share, role, can_reshare) VALUES (?,?,?,?,?)`,
		tokenHash, inv.Principal, inv.Share, inv.Role, cr); err != nil {
		return err
	}
	return tx.Commit()
}

// InviteBinding returns the binding for a token hash (ok=false for a plain token).
func (d *DB) InviteBinding(tokenHash string) (inv Invite, ok bool, err error) {
	var cr int
	err = d.sql.QueryRow(
		`SELECT principal_id, share, role, can_reshare FROM invites WHERE token_hash=?`, tokenHash).
		Scan(&inv.Principal, &inv.Share, &inv.Role, &cr)
	if errors.Is(err, sql.ErrNoRows) {
		return Invite{}, false, nil
	}
	if err != nil {
		return Invite{}, false, err
	}
	inv.CanReshare = cr != 0
	return inv, true, nil
}

// RevokeInvite kills a still-pending invite: it removes the redeemable token and
// its binding so /v1/join can no longer accept it — the missing primitive for a
// leaked or regretted invite within its TTL. Returns false if nothing was killed
// (already redeemed, already revoked, or never existed). One transaction.
func (d *DB) RevokeInvite(tokenHash string) (bool, error) {
	tx, err := d.sql.Begin()
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	res, err := tx.Exec(`DELETE FROM tokens WHERE hash=? AND used=0`, tokenHash)
	if err != nil {
		return false, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return false, nil // already redeemed/revoked/unknown — nothing to kill
	}
	if _, err := tx.Exec(`DELETE FROM invites WHERE token_hash=?`, tokenHash); err != nil {
		return false, err
	}
	return true, tx.Commit()
}

// EnsurePrincipal creates a principal if absent (idempotent).
func (d *DB) EnsurePrincipal(id, name string, createdAt int64) error {
	_, err := d.sql.Exec(
		`INSERT INTO principals (id, name, created_at) VALUES (?,?,?)
		 ON CONFLICT(id) DO NOTHING`, id, name, createdAt)
	return err
}

// SetDevicePrincipal points a device at a principal (the principal must exist).
func (d *DB) SetDevicePrincipal(deviceID, principalID string) error {
	res, err := d.sql.Exec(`UPDATE devices SET principal_id=? WHERE id=?`, principalID, deviceID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("no such device %q", deviceID)
	}
	return nil
}

// SetMember grants (or updates) a principal's role on a share and flips the share
// into explicit-ACL mode — once a share has any member grant it is deny-by-default.
func (d *DB) SetMember(share, principalID string, role int, canReshare bool) error {
	tx, err := d.sql.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	cr := 0
	if canReshare {
		cr = 1
	}
	if _, err := tx.Exec(
		`INSERT INTO members (share, principal_id, role, can_reshare) VALUES (?,?,?,?)
		 ON CONFLICT(share, principal_id) DO UPDATE SET role=excluded.role, can_reshare=excluded.can_reshare`,
		share, principalID, role, cr); err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE shares SET acl_mode='explicit' WHERE name=?`, share); err != nil {
		return err
	}
	return tx.Commit()
}

// RemoveMember revokes a principal's grant on a share. The share stays in explicit
// mode (removing the last member = a locked share, the deliberate deny-all state).
func (d *DB) RemoveMember(share, principalID string) error {
	_, err := d.sql.Exec(`DELETE FROM members WHERE share=? AND principal_id=?`, share, principalID)
	return err
}

// ACLMode returns a share's ACL mode ("legacy" or "explicit"); "legacy" if the
// share is unknown, so callers stay v1-permissive.
func (d *DB) ACLMode(share string) (string, error) {
	var mode string
	err := d.sql.QueryRow(`SELECT acl_mode FROM shares WHERE name=?`, share).Scan(&mode)
	if errors.Is(err, sql.ErrNoRows) {
		return "legacy", nil
	}
	if err != nil {
		return "", err
	}
	return mode, nil
}

// Members lists a share's grants (empty for a legacy share).
func (d *DB) Members(share string) ([]Member, error) {
	rows, err := d.sql.Query(`SELECT principal_id, role, can_reshare FROM members WHERE share=? ORDER BY role DESC, principal_id`, share)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Member
	for rows.Next() {
		var m Member
		var cr int
		if err := rows.Scan(&m.Principal, &m.Role, &cr); err != nil {
			return nil, err
		}
		m.CanReshare = cr != 0
		out = append(out, m)
	}
	return out, rows.Err()
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

	// Snapshots are content-addressed, so re-pushing an unchanged tree (or
	// reverting to a prior state) replays an existing id within THIS share. That's
	// idempotent: advance the head to it, but never re-insert or double-count chunk
	// refs. Keyed by (share, id): the same content in another share is a distinct
	// snapshot that must record its own row and count its own chunks.
	var one int
	switch err := tx.QueryRow(`SELECT 1 FROM snapshots WHERE share=? AND id=?`, s.Share, s.ID).Scan(&one); {
	case err == nil:
		if _, err := tx.Exec(`UPDATE shares SET head_snapshot=? WHERE name=?`, s.ID, s.Share); err != nil {
			return err
		}
		return tx.Commit()
	case !errors.Is(err, sql.ErrNoRows):
		return err
	}

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

// --- history & gc -------------------------------------------------------- //

// SnapInfo is one entry in a share's snapshot history.
type SnapInfo struct {
	ID        string
	Parent    string
	Device    string
	CreatedAt int64
}

// SnapshotLog returns a share's snapshots newest-first, capped at limit.
func (d *DB) SnapshotLog(share string, limit int) ([]SnapInfo, error) {
	rows, err := d.sql.Query(
		`SELECT id, COALESCE(parent_id,''), device_id, created_at
		 FROM snapshots WHERE share=? ORDER BY created_at DESC, rowid DESC LIMIT ?`,
		share, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SnapInfo
	for rows.Next() {
		var s SnapInfo
		if err := rows.Scan(&s.ID, &s.Parent, &s.Device, &s.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// ShareNames lists every share name.
func (d *DB) ShareNames() ([]string, error) {
	rows, err := d.sql.Query(`SELECT name FROM shares`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// DeletableSnapshots returns the snapshot ids GC may prune for a share: every
// snapshot except the current head and the `keep` most recent (by created_at).
// The kept set and the head can overlap; either way they are never returned.
func (d *DB) DeletableSnapshots(share string, keep int) ([]string, error) {
	head, _, err := d.ShareHead(share)
	if err != nil {
		return nil, err
	}
	rows, err := d.sql.Query(
		`SELECT id FROM snapshots WHERE share=? ORDER BY created_at DESC, rowid DESC`, share)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var del []string
	for i := 0; rows.Next(); i++ {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		if i < keep || id == head {
			continue // protected: within the keep window or the live head
		}
		del = append(del, id)
	}
	return del, rows.Err()
}

// DeleteSnapshot removes one share's snapshot row and decrements the refcount of
// each distinct chunk hash it referenced, all in one transaction. The caller
// derives chunkHashes from the snapshot's manifest (distinct, as AddSnapshot
// counted them). Keyed by (share, id): pruning one share's copy of shared content
// leaves another share's snapshot (and its refcounts) intact.
func (d *DB) DeleteSnapshot(share, id string, chunkHashes []string) error {
	tx, err := d.sql.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	res, err := tx.Exec(`DELETE FROM snapshots WHERE share=? AND id=?`, share, id)
	if err != nil {
		return err
	}
	// Only decrement if this row actually existed. A double-delete (a retry, or a
	// content id pruned once already) must not over-decrement and drive a chunk a
	// live head still references below zero.
	if n, _ := res.RowsAffected(); n == 0 {
		return tx.Commit()
	}
	for _, h := range chunkHashes {
		// max(0, …) floors the refcount: even if accounting is off, it can never
		// go negative and later let a live chunk read as unreferenced.
		if _, err := tx.Exec(`UPDATE chunks SET refcount=max(0, refcount-1) WHERE hash=?`, h); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// UnreferencedChunks lists chunk hashes whose refcount has dropped to zero: GC
// treats these as deletion CANDIDATES, then re-verifies each against the live
// snapshot set before actually deleting (refcounts are only a hint).
func (d *DB) UnreferencedChunks() ([]string, error) {
	rows, err := d.sql.Query(`SELECT hash FROM chunks WHERE refcount<=0`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var h string
		if err := rows.Scan(&h); err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

// DeleteChunkRow removes a chunk's metadata row, but ONLY if it is still
// unreferenced at delete time (refcount<=0) — so a push that re-referenced the
// chunk between GC's scan and this delete isn't silently corrupted. Returns
// whether a row was removed; the caller deletes the blob only when it was.
func (d *DB) DeleteChunkRow(hash string) (bool, error) {
	res, err := d.sql.Exec(`DELETE FROM chunks WHERE hash=? AND refcount<=0`, hash)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// Heads returns the current head snapshot id of every share that has one. GC
// uses these (plus the keep window) as the roots that must never be swept.
func (d *DB) Heads() ([]string, error) {
	return d.queryStrings(`SELECT head_snapshot FROM shares WHERE head_snapshot IS NOT NULL AND head_snapshot != ''`)
}

// SnapshotIDs returns every snapshot id recorded for a share.
func (d *DB) SnapshotIDs(share string) ([]string, error) {
	return d.queryStrings(`SELECT id FROM snapshots WHERE share=?`, share)
}

// SnapshotTimestamps returns id→created_at for all snapshots of a share.
// Used by GC's --keep-days: snapshots within the window are kept even when
// outside the --keep newest-N window.
func (d *DB) SnapshotTimestamps(share string) (map[string]int64, error) {
	rows, err := d.sql.Query(`SELECT id, created_at FROM snapshots WHERE share=?`, share)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[string]int64)
	for rows.Next() {
		var id string
		var ts int64
		if err := rows.Scan(&id, &ts); err != nil {
			return nil, err
		}
		out[id] = ts
	}
	return out, rows.Err()
}

// --- dashboard reads ------------------------------------------------------

// DeviceRow is a device's detail for the dashboard.
type DeviceRow struct {
	ID        string
	Name      string
	Principal string
	LastSeen  int64
	Revoked   bool
}

// Devices lists every device with detail (newest-name order), for the dashboard.
func (d *DB) Devices() ([]DeviceRow, error) {
	rows, err := d.sql.Query(`SELECT id, name, COALESCE(last_seen,0), revoked, principal_id FROM devices ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []DeviceRow
	for rows.Next() {
		var r DeviceRow
		var revoked int
		if err := rows.Scan(&r.ID, &r.Name, &r.LastSeen, &revoked, &r.Principal); err != nil {
			return nil, err
		}
		r.Revoked = revoked != 0
		out = append(out, r)
	}
	return out, rows.Err()
}

// ShareStat is a share's rolled-up detail for the dashboard.
type ShareStat struct {
	Name      string
	Head      string
	ACLMode   string
	Snapshots int
	Members   int
	UpdatedAt int64 // newest snapshot time
}

// ShareStats rolls up per-share counts for the dashboard in one query.
func (d *DB) ShareStats() ([]ShareStat, error) {
	rows, err := d.sql.Query(`
		SELECT s.name, COALESCE(s.head_snapshot,''), s.acl_mode,
		       (SELECT COUNT(*) FROM snapshots sn WHERE sn.share=s.name),
		       (SELECT COUNT(*) FROM members m  WHERE m.share=s.name),
		       COALESCE((SELECT MAX(created_at) FROM snapshots sn WHERE sn.share=s.name),0)
		FROM shares s ORDER BY s.name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ShareStat
	for rows.Next() {
		var s ShareStat
		if err := rows.Scan(&s.Name, &s.Head, &s.ACLMode, &s.Snapshots, &s.Members, &s.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// SumChunkBytes returns the total size of all stored chunks (0 if none).
func (d *DB) SumChunkBytes() (int64, error) {
	var n sql.NullInt64
	if err := d.sql.QueryRow(`SELECT SUM(size) FROM chunks`).Scan(&n); err != nil {
		return 0, err
	}
	return n.Int64, nil
}

func (d *DB) queryStrings(query string, args ...any) ([]string, error) {
	rows, err := d.sql.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
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
