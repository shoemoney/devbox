# devbox — v1 Design

> **One-liner:** Continuous, automatic file sync for developers — like Dropbox, but it
> respects a `.devignore`, refuses to leak secrets, runs pre/post hooks on sync events, keeps
> Git-like version history, and lets you mount *shares* (or sub-paths) across machines. It is
> *not* a Git replacement; it keeps your working directory live-synced across machines without
> committing half-done work.

Status: **approved design**, ready for implementation planning.
Date: 2026-06-22. (Revised post-audit: scope tightened, secret-guard/metrics/bandwidth-cap
promoted into v1.)

---

## 1. Problem & Vision

Dropbox/iCloud sync everything blindly — they choke on `node_modules`, leak `.env` secrets,
and thrash on build artifacts. Git is manual, commit-based, and not meant for continuously
mirroring an in-progress working tree across machines. There is no good tool that says: *"keep
these dev directories live-synced across my machines, skip the junk, never leak my secrets,
run my commands when files land, and let me cherry-pick exactly what each machine gets."*

**devbox** fills that gap: a self-hostable **hub** publishes named **shares**; **devices** join
the hub with a token and **mount** shares (or sub-paths) into any local directory, read-write
or read-only. A per-machine **daemon** watches every mount, filters via `.devignore`, syncs
deltas through the hub to the device's other machines in near-real-time, fires lifecycle
**hooks**, and keeps **snapshot history** per share.

**Who it's for (v1):** developers running multiple machines / homelabs who want their active
workspaces mirrored without Git ceremony. Fits a Pi 5 cluster + MacBook + TrueNAS setup
directly. Also the foundation of a future commercial hosted offering (see §13).

---

## 2. Goals / Non-Goals

### Goals
- Continuous, automatic, **bidirectional** sync of shares across linked machines.
- **Shares + selective mounts**: cherry-pick a whole share or any sub-path onto a machine,
  mapped to any local path, read-write or read-only.
- `.devignore` (gitignore syntax) to exclude paths from sync entirely.
- **Default-on secret protection** — built-in deny-list hard-refuses to upload matched secret
  patterns even if `.devignore` is misconfigured.
- **Hooks**: run user commands on sync lifecycle events.
- **Version history** (Git-like snapshots, per share) so any synced file can be rolled back.
- **Self-hostable hub** — single Go binary, runs on a Pi / TrueNAS, no SaaS dependency.
- Delta/chunked transfer + a **bandwidth cap** so big files and large trees stay fast and don't
  saturate a link.
- **Never silently lose work.** Conflicts never block and never destroy a byte.
- **Hub observability** — `/metrics` + a status page.
- **Fully cross-platform**: Linux, macOS, Windows.
- Open-source (AGPLv3) core, with clean seams for a future commercial hosted tier.

### Non-Goals (v1)
- Not a Git replacement — no branching/merging semantics, no PRs.
- No TUI — `devbox` with no args prints rich text status. (Full TUI = future "someday" todo.)
- No mobile/desktop GUI.
- No multi-user roles/teams/ACLs. Single-owner, multi-device. The only access knob is a
  per-device-per-share **read-only** bit (deploy boxes can't push).
- No interactive conflict resolver (mine/theirs/both/edit) — v1 lists conflicts; resolving by
  hand. Interactive resolver is v2.
- No re-share/delegation permission — v2.
- No content-level 3-way text merge — conflicts produce side-by-side copies (v2).
- No LAN peer-to-peer chunk exchange — v1 is hub-centric; P2P block exchange is v2.
- No client-side E2E chunk encryption (v2).

---

## 3. Tech Stack

**Everything in Go.** One toolchain, static single binaries for daemon, CLI, and hub —
cross-compiled to `linux/arm64` (Pi), `darwin/arm64` (Mac), `windows/amd64`.

| Component | Tech | Why |
| --- | --- | --- |
| CLI + daemon (`devbox`, `devboxd`) | Go, `cobra` CLI | Static cross-compiled binaries, trivial Pi deploy |
| File watching | `fsnotify`, debounced rescan | inotify / FSEvents / ReadDirectoryChangesW |
| Ignore matching | gitignore-semantics matcher | `.devignore` = gitignore syntax |
| Chunking / dedup | FastCDC content-defined chunking + BLAKE3 | Delta sync, dedup, content-addressed (manifests too) |
| Transport | **WebSocket for change events + stateless HTTP for content-addressed blobs** | WS handles live events; HTTP gives range/resume/caching for bytes for free. Same TLS endpoint + bearer token. |
| Hub metadata | SQLite (single-writer, WAL) | Zero-ops, fits homelab |
| Blob storage | `BlobStore` interface — disk CAS (v1), S3/R2 impl later | Maps to TrueNAS now, object storage for hosted |
| Auth | Device Ed25519 keypair + bearer token; single rotatable join token | No passwords, device-revocable |
| Hooks runner | exec into **bash** (default, all OSes); `.ps1` hooks auto-run via `pwsh` on Windows | Portable hooks across the fleet; zero-config Windows opt-in |

> **Why not pure WebSocket?** A single WS is one ordered stream — a big blob upload
> head-of-line-blocks event delivery, and WS has no range/resume/caching. Blobs over stateless
> HTTP (`GET /blob/<hash>`, retry = re-request) is *less* code than reinventing resumable
> transfer over a socket. Only the WS needs reconnect logic.

---

## 4. Core Concepts

- **Hub** — the server. Publishes shares, stores chunks + manifests (CAS) + metadata (SQLite),
  brokers change events. One owner, many devices. Self-hosted single binary.
- **Device** — a machine with an Ed25519 identity, joined to a hub. Runs one daemon.
- **Share** — a named top-level tree on the hub (`projects`, `repos`, `services`). The unit of
  snapshot history.
- **Mount** — a device-side binding: `share[/subpath] → localpath`, read-write or read-only. A
  device has many mounts; one daemon watches them all.
- **Read-only bit** — a per-device-per-share `writable` flag on the hub (default writable). A
  non-writable device's pushes are rejected server-side — the only access control in v1.
- **Snapshot** — an immutable per-share manifest version (id = BLAKE3 of manifest).
- **Manifest** — `path → [chunk hashes] + mode + size` for a share at a point in time;
  itself content-addressed and stored in the CAS.

---

## 5. Architecture

```
device (laptop)                 hub (Pi / TrueNAS)              device (pi-07)
 devboxd                         devbox-hub                      devboxd
  ├ mount: projects/  ──┐        ├ shares: projects,      ┌──── mount: projects/p22/backend
  ├ mount: repos/  ─────┤  WS    │   repos, services       │      (read-only -> /var/www)
  │  (events) ──────────┼──────► ├ change log / HEAD  WS ──┤    └ hooks fire on pull
  │  (blobs) ───────────┼─HTTP─► ├ CAS: chunks + manifests │
  └ <mount>/.devbox/    │        ├ SQLite: snapshots,  HTTP┘
    hooks, .devignore   └──────► │   shares, devices,
                                 │   tokens, writable bits
                                 ├ BlobStore: disk (v1) | S3/R2 (later)
                                 └ /metrics + status page
```

### Sync loop (per mount, per machine)
1. `fsnotify` reports a change in a mount's local tree (debounced ~300ms).
2. Path checked against `.devignore` **and the built-in secret deny-list**; matches dropped.
3. Changed files chunked (FastCDC) → BLAKE3 → manifest diff vs last-known state.
4. **`pre-push` hook** fires (can veto). New chunks uploaded over HTTP (only ones the hub
   lacks), subject to the bandwidth cap.
5. Hub appends a snapshot, advances that **share's linear HEAD**, broadcasts a change event
   over WebSocket.
6. **`post-push` hook** fires locally.
7. Other devices with a mount on that share receive the WS event → **`pre-pull` hook** → fetch
   missing chunks over HTTP → reassemble → **atomic rename into place** → **`post-pull` hook**.

A read-only mount skips steps 3–6 (never pushes) but still does 7.

---

## 6. Shares, Mounts & Read-Only

### Mounting
`devbox -rw <share>[/subpath] [localpath]` mounts a whole share or any sub-path to any local
path. Default `localpath` = `./<last-path-segment>`. `-r` = read-only, `-a`/`-rw` = read-write.

```
devbox -a projects ./                                 # whole share, rw, here
devbox -r projects/p22/backend /var/www/p22/backend   # one sub-path, read-only, elsewhere
```

A mount is recorded in `~/.config/devbox/daemon.toml` (see §8).

### Access model (deliberately minimal)
v1 is **single-owner**, so there are no roles, no grants ACL, no flag-clamping subsystem. Every
enrolled device is `rw` by default. The one real need — *deploy boxes must not push* — is a
single **server-enforced** `writable` bit per (device, share):

- Set on the hub: `devbox-hub readonly <device> <share>` → the hub rejects that device's pushes.
- Server-enforced on purpose: a read-only deploy box *cannot* pollute canonical even if its
  local flag is wrong or the client is compromised. (A purely client-side flag would be a
  footgun.)

### Creating a share
`devbox publish <localdir> <name>` — any writable device can create a share from a local folder
and push it. Admin (`devbox-hub …`) runs on the hub host (whoever has shell there); device
management isn't a device privilege.

### Read-only mounts & data safety
A read-only mount pulls but never pushes. To still honor "never lose a byte": if an inbound
change would clobber a **locally-modified** file on a read-only mount, devbox first stashes the
local version as `path.conflict-local-<ts>.ext` (preserved on disk, never pushed), then applies
the inbound change.

---

## 7. Consistency & Conflict Model

The mechanism behind "Dropbox-easy, never loses data." Runs **per share**.

- Each share has **one linear HEAD** on the hub. SQLite single-writer serializes concurrent
  pushes for free.
- A push declares the snapshot it was based on (`parent_id`).
  - `parent == HEAD` → fast-forward: accept, advance HEAD.
  - `parent != HEAD` → **per-file 3-way** vs the common ancestor (the pusher's `parent`):
    - Changed by **only** the pusher → applies cleanly.
    - Changed by **both** → **conflict**: first-to-land (current HEAD) stays **canonical**; the
      loser is written beside it as `path.conflict-<host>-<ts>.ext`; `on-conflict` fires. The
      conflict copy is a new path → it syncs everywhere, so no edit is ever lost.
- **Comparison is by BLAKE3 content hash**, not timestamps — touching a file without changing
  bytes is a no-op.
- **Deletes** propagate (recoverable from snapshots). **Delete-vs-edit never loses the edit** —
  a delete has no bytes to preserve, so the edit always wins (canonical, or a `.conflict` copy).
- **Renames are free** — old-path-gone + new-path-added; content-addressed chunks mean zero
  bytes re-transfer.
- **Initial mount/bootstrap** — mounting onto a non-empty dir reconciles per-file against HEAD
  via the same conflict path (ancestor = empty).

**Never blocks. Never asks. Never destroys a byte.**

### Conflict UX (one mode, not four)
- **Conflict copies** land on disk next to the canonical file (always).
- **`devbox status`** shows a pending-conflict count + lists the copies. (Always on.)
- **`on-conflict` hook** is the notification API — wire it to ntfy/desktop/Slack.
- **`devbox conflicts`** lists the conflict files (read-only listing) so you can find and clean
  them. *(Interactive resolve — mine/theirs/both/edit — is v2. No blocking prompt: a headless
  daemon can't prompt and it would break "never asks.")*

---

## 8. State on Disk

- **Machine-global** `~/.config/devbox/`:
  - Ed25519 keypair (one identity per device; **never leaves the box**).
  - `daemon.toml` — the mount list (share, subpath, localpath, readonly, hub).
  - `config.toml` — global settings.
- **Per-mount** `<localpath>/.devbox/`:
  - `hooks/` — per-machine lifecycle scripts (never synced).
  - last-synced snapshot id, mount-local overrides.
  - Always implicitly ignored from sync.
- **`.devignore`** — a *synced* file at a share root (gitignore syntax). Nested ones allowed.

```toml
# ~/.config/devbox/daemon.toml
[[mount]]
share = "projects"
local = "/Users/jh/Projects"
hub   = "hub.shoemoney.ai"

[[mount]]
share = "projects"
subpath  = "p22/backend"
local    = "/var/www/p22/backend"
readonly = true
hub = "hub.shoemoney.ai"
```

```toml
# ~/.config/devbox/config.toml
[hooks]
# bash everywhere; a .ps1 hook auto-runs via pwsh on Windows
interpreter  = "bash"
timeout_secs = 60

[transfer]
max_kbps = 0            # 0 = unlimited; cap to protect a link

[secrets]
# built-in deny-list always on; add extra patterns here
extra_patterns = []
```

---

## 9. `.devignore` + Secret Guard

### `.devignore`
Gitignore syntax, relative to the share/mount root; nested files allowed.

```
node_modules/
vendor/
dist/
build/
.next/
target/
*.log
*.tmp
.DS_Store
!.env.example
```

- A matched path is invisible to sync in **both** directions.
- `!pattern` negation re-includes (gitignore precedence).
- Changing `.devignore` triggers a rescan; newly-ignored files are **left on disk** but stop
  syncing.
- `.devbox/` is always implicitly ignored.

### Secret guard (default-on, independent of `.devignore`)
A built-in deny-list runs in the push path and **hard-refuses to upload** matched files, even
if `.devignore` is misconfigured. This is the "won't leak your secrets" guarantee, enforced —
not an optional hook.

Default patterns: `.env`, `.env.*` (except `.env.example`), `*.pem`, `*.key`, `id_rsa*`,
`*.p12`, `*.pfx`, `secrets/`, `*.kdbx`, common cloud-credential files. Add more via
`[secrets].extra_patterns`. A blocked file surfaces in `devbox status`. (A `pre-push`
secret-scan hook can still add belt-and-suspenders scanning of file *contents*.)

---

## 10. Hooks Spec

Per-machine scripts in `<mount>/.devbox/hooks/`, named after the event, **never synced**.
devbox shells out to **bash** by default (one portable hook language across your fleet); a hook
file ending in `.ps1` auto-runs via `pwsh` on Windows (zero-config opt-in). `devbox doctor`
flags a missing interpreter. Non-zero exit on a `pre-*` hook **aborts** that step. Default
timeout 60s — a hung hook is killed + logged, never wedges the loop.

| Hook | Fires | Can abort? | Typical use |
| --- | --- | --- | --- |
| `pre-push` | before local changes upload | ✅ | lint/format, secret scan |
| `post-push` | after upload confirmed | ❌ | notify, log, tag a snapshot |
| `pre-pull` | before inbound changes apply | ✅ | stop a container/dev server |
| `post-pull` | after inbound changes applied | ❌ | `pnpm install`, migrate, restart service |
| `on-conflict` | conflict copy created | ❌ | open a diff, ping you, log |

### Injected environment
```bash
DEVBOX_EVENT=post-pull
DEVBOX_MOUNT=/srv/project
DEVBOX_SHARE=projects
DEVBOX_HOST=pi-node-07
DEVBOX_CHANGED_FILES=/tmp/devbox-changes.txt
DEVBOX_SNAPSHOT=ab12cd34
DEVBOX_REMOTE=hub.shoemoney.ai
```

### Example `post-pull`
```bash
#!/usr/bin/env bash
if grep -qE 'package\.json|pnpm-lock\.yaml' "$DEVBOX_CHANGED_FILES"; then
  pnpm install --frozen-lockfile
fi
docker compose restart app
```

---

## 11. Versioning / Snapshots / Deploy

- Every accepted change set on a share creates an immutable **snapshot** (id = BLAKE3 of the
  manifest). The **manifest itself is content-addressed** and stored in the CAS — identical or
  near-identical manifests dedup, so 100 pushes don't store the path→chunks map 100×.
- `devbox log` lists snapshots; `devbox restore <snapshot> [path]` rolls a file or the whole
  share back (a restore is itself a new change → reversible).
- **`devbox deploy <share> <snapshot>`** (v1.5/M6.5) — atomically pin a read-only mount to a
  specific snapshot. Blue/green deploys for `/var/www`-style boxes, nearly free since snapshots
  are already immutable.
- Retention configurable on the hub; `devbox-hub gc` garbage-collects unreferenced chunks via
  refcounts.

---

## 12. Cross-Platform Handling (Linux / macOS / Windows)

Go cross-compiles to all three; `fsnotify` covers inotify / FSEvents / ReadDirectoryChangesW.

- **Paths**: canonical form is **forward-slash, relative**; convert to `\` only at the Windows
  filesystem boundary.
- **Atomic apply**: `ReplaceFile`/`MoveFileEx` on Windows; POSIX `rename(2)` elsewhere.
- **Name clashes / reserved names** (`foo.go` vs `Foo.go`, `aux`/`com1`, trailing dot):
  **skip + warn + surface** under "unsyncable here" in `devbox status`. The hub keeps the bytes;
  OSes that can hold the name still get the file. Never fatal.
- **Line endings**: bytes synced verbatim; no CRLF/LF normalization.
- **Service install**: `devbox start|stop` abstracts launchd / systemd / Windows Service.
- **Hooks**: bash default; `.ps1` → `pwsh` on Windows; `devbox doctor` verifies the interpreter.

---

## 13. Open-Core / Commercial Model

**Open-source core + commercial hosted tier.** The OSS product is the fully self-hostable hub +
clients (this entire v1). The hosted version is a **separate, closed control plane** (signup,
billing, quotas, provisioning) — **out of scope for v1**, but the core carries clean seams:

- **`BlobStore` interface** — disk CAS (v1); S3/R2 impl later for hosted.
- **Config-driven limits** — `max_storage`, `retention`, `max_devices`; self-host unlimited, the
  commercial layer sets them per tier.
- **`/metrics` + status page** — observability now; doubles as the hosted dashboard seam.
- **Single-tenant core** — the OSS hub knows nothing about accounts/billing.

### License (decided): AGPLv3 core + commercial license for the hosted tier
Self-host free; anyone offering it as a hosted service must open their mods or buy a commercial
license. Add a contributor CLA once outside PRs arrive (keeps dual-licensing possible).

### Parked recommendations (confirm before launching hosting — don't change the v1 build):
- **Tenancy**: per-tenant hub instance — the control plane provisions one clean single-tenant
  hub (own SQLite + blob bucket) per account. Zero tenancy code in the OSS core. Multi-tenant
  consolidation is a later scale optimization.
- **Pricing**: small monthly fee, discount for annual. Tiers enforced via the config knobs.

---

## 14. Security (v1)

- Each device generates an **Ed25519** keypair on first run; the private key never leaves the
  machine. Joining a hub authorizes the device's pubkey.
- A single **join token** (rotatable, expiring) enrolls devices: `devbox join <hub> <token>`.
- All hub traffic over **TLS** (behind Nginx Proxy Manager / Let's Encrypt).
- **Secret guard** (§9) keeps `.env`, keys, etc. on the machine — enforced in the push path,
  default-on, independent of `.devignore`.
- **Read-only enforcement** (§6) is server-side — a non-writable device can't push.
- **Revoke** a lost device with `devbox-hub device revoke <id>` — its bearer token dies
  immediately.
- (Deferred to v2) optional client-side E2E chunk encryption so the hub stores only ciphertext.

---

## 15. CLI Surface (v1)

```
# device
devbox join <hub> <token>           # enroll this device
devbox -rw <share>[/sub] [path]     # mount a share/sub-path read-write (default ./<name>)
devbox -r  <share>[/sub] [path]     # mount read-only
devbox publish <localdir> <name>    # create a share from a local folder (writable device)
devbox unmount <share>              # stop syncing a mount (files stay on disk)
devbox start | stop                 # run/stop the daemon (devboxd)
devbox status                       # rich text: shares, mounts, peers, conflicts, blocked secrets
devbox pause | resume               # halt sync without unmounting
devbox log                          # snapshot history
devbox restore <snap> [path]        # roll back a file or a whole share
devbox deploy <share> <snap>        # pin a read-only mount to a snapshot (v1.5)
devbox conflicts                    # list conflict copies (interactive resolve = v2)
devbox ignore <pattern>             # append to .devignore + rescan
devbox hook edit <event>            # open a hook script in $EDITOR
devbox peers                        # linked machines, online/offline, last seen
devbox doctor                       # diagnose watcher limits, perms, hook interpreter, connectivity

# hub (run on the hub host)
devbox-hub serve --config /etc/devbox-hub.toml
devbox-hub token                    # mint/rotate the join token
devbox-hub device list | revoke <id>
devbox-hub readonly <device> <share>   # mark a device read-only on a share (toggle)
devbox-hub gc                       # garbage-collect unreferenced chunks
```

(`devbox` with no args = `devbox status`. A full TUI is a future "someday" todo, not v1.)

---

## 16. Data Model (hub, SQLite WAL)

```sql
CREATE TABLE devices (
  id          TEXT PRIMARY KEY,      -- pubkey fingerprint
  name        TEXT NOT NULL,
  pubkey      BLOB NOT NULL,
  last_seen   INTEGER,
  revoked     INTEGER DEFAULT 0
);

CREATE TABLE tokens (
  hash        TEXT PRIMARY KEY,      -- hash of the join token
  expires_at  INTEGER,
  used        INTEGER DEFAULT 0
);

CREATE TABLE shares (
  name          TEXT PRIMARY KEY,
  head_snapshot TEXT,                -- current linear HEAD
  created_by    TEXT NOT NULL,
  created_at    INTEGER NOT NULL
);

-- only access control in v1: a per-(device,share) writable bit. Row present + writable=0 => read-only.
CREATE TABLE access (
  device_id   TEXT NOT NULL,
  share       TEXT NOT NULL,
  writable    INTEGER NOT NULL DEFAULT 1,
  PRIMARY KEY (device_id, share)
);

CREATE TABLE snapshots (
  id            TEXT PRIMARY KEY,    -- blake3(manifest)
  share         TEXT NOT NULL,
  parent_id     TEXT,
  device_id     TEXT NOT NULL,
  created_at    INTEGER NOT NULL,
  manifest_hash TEXT NOT NULL        -- manifest stored content-addressed in the CAS
);

CREATE TABLE chunks (
  hash        TEXT PRIMARY KEY,      -- blake3 (chunks AND manifests live here)
  size        INTEGER NOT NULL,
  refcount    INTEGER NOT NULL DEFAULT 0
);
```

Blobs (chunks + manifests) stored via the `BlobStore` interface; disk impl at
`blobs/<first2>/<hash>`, content-addressed and **deduped across all shares**. Refcounts drive GC.

---

## 17. Milestones

| M | Deliverable | Done = |
| --- | --- | --- |
| **M0 — Skeleton** | Go monorepo, `cobra` CLI, `devbox join`, keypair gen, machine config | `devbox join` enrolls a device against a hub |
| **M1 — Watch + ignore + secret-guard** | `fsnotify` watcher, `.devignore` matcher, secret deny-list, content-addressed manifest builder | Edits produce a correct filtered manifest diff; secrets are refused |
| **M2 — Hub + one-way push** | `devbox-hub serve`, shares, join token, `BlobStore` (disk CAS for chunks + manifests), `publish`, chunk upload over HTTP, snapshots, `/metrics` + status page | A device publishes a share and pushes; snapshots land; hub is observable |
| **M3 — Two-way sync** | WS events + HTTP blob fetch, mount/pull, atomic apply, per-file 3-way conflict copies, `devbox status` badge + `devbox conflicts` list + `on-conflict` hook | Two devices stay mirrored live; conflicts produce copies |
| **M4 — Read-only + bandwidth** | Server-enforced `writable` bit, `devbox-hub readonly`, read-only data-safety stash, sub-path mounts, bandwidth cap | A read-only deploy mount pulls, never pushes; transfers respect the cap |
| **M5 — Hooks** | bash (+`.ps1`) lifecycle runner, templates, env injection, timeout | `post-pull` runs `pnpm install` on a Pi node |
| **M6 — Versioning** | `devbox log` / `restore`, hub GC | Roll back any file from history |
| **M6.5 — Deploy** | `devbox deploy <share> <snapshot>` (pin read-only mount to a snapshot) | Blue/green flip a services box to a known snapshot |
| **M7 — Hardening** | `devbox doctor`, watcher-limit handling, WS reconnect/backoff, name-clash handling, arm64/mac/win release builds | Survives reboots & flaky network on the cluster |

> Open-core seams (BlobStore interface, config limits, `/metrics`) land in M2. The commercial
> control plane (accounts/billing/provisioning) is a **separate sub-project**, not part of v1.

---

## 18. Repo Layout

```
devbox/
├── cmd/
│   ├── devbox/        # CLI + daemon entrypoint
│   └── devbox-hub/    # hub server
├── internal/
│   ├── watch/         # fsnotify + debounce
│   ├── ignore/        # .devignore matcher
│   ├── secret/        # default-on secret deny-list (push-path guard)
│   ├── chunk/         # FastCDC + blake3
│   ├── manifest/      # tree state + diff (content-addressed)
│   ├── mount/         # mount config, read-only enforcement
│   ├── sync/          # push/pull engine, conflict resolution
│   ├── hooks/         # lifecycle runner (bash/.ps1)
│   ├── transport/     # ws (events) + http (blobs) client
│   └── hub/           # server, sqlite, shares, access, tokens, CAS, gc, metrics
│       └── blobstore/ # BlobStore interface: disk (v1), s3/r2 (later)
├── pkg/proto/         # shared wire types
└── deploy/            # systemd/launchd/winsvc units, docker-compose for hub
```

---

## 19. Key Risks / Open Decisions

- **Watcher limits at scale** — large trees blow past inotify limits on Linux; mitigated by
  `.devignore` cutting `node_modules`, a fallback periodic rescan, and `devbox doctor` to raise
  `fs.inotify.max_user_watches`.
- **Hub uplink saturation** — 40 nodes pulling one big change strains the hub's link. v1
  mitigations: bandwidth cap + `.devignore`. The real fix (LAN peer chunk exchange) is v2.
- **Hub HA** — v1 is single-hub (SQLite). Run it on one stable node + TrueNAS-backed blob dir;
  clustering is v2.
- **Central hub vs P2P** — central hub for v1; a P2P mesh transport can be added later without
  changing the CLI/hook contract.

---

## 20. v2 Backlog

Deferred features, captured so they aren't lost:

- **LAN peer chunk-exchange.** Co-located nodes swap chunks directly (Syncthing-style block
  exchange) instead of all hitting the hub — the real fix for cluster uplink pressure.
- **Interactive conflict resolver.** `devbox conflicts` gains diff + keep-mine/theirs/both/edit.
- **Re-share / delegation permission** (the `s` bit) — device-to-device granting.
- **Content-level 3-way text merge** for conflicting edits.
- **Laptop power sanity** — pause-on-metered / pause-on-battery / scheduled sync windows.
- **Client-side E2E chunk encryption** — hub stores only ciphertext.
- **Hub clustering / HA.**
- **Full TUI** for `devbox` (no-arg dashboard).
```
