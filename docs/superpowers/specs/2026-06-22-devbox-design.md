# devbox — v1 Design

> **One-liner:** Continuous, automatic file sync for developers — like Dropbox, but it
> respects a `.devignore`, runs pre/post hooks on sync events, keeps Git-like version
> history, and lets you mount *shares* (or sub-paths) across machines with per-device
> permissions. It is *not* a Git replacement; it keeps your working directory live-synced
> across machines without committing half-done work.

Status: **approved design**, ready for implementation planning.
Date: 2026-06-22.

---

## 1. Problem & Vision

Dropbox/iCloud sync everything blindly — they choke on `node_modules`, leak `.env`
secrets, and thrash on build artifacts. Git is manual, commit-based, and not meant for
continuously mirroring an in-progress working tree across machines. There is no good tool
that says: *"keep these dev directories live-synced across my machines, skip the junk, run
my commands when files land, and let me cherry-pick exactly what each machine gets."*

**devbox** fills that gap: a self-hostable **hub** publishes named **shares**; **devices**
join the hub with a token and **mount** shares (or sub-paths) into any local directory with
read/write permissions. A per-machine **daemon** watches every mount, filters via
`.devignore`, syncs deltas through the hub to the device's other machines in near-real-time,
fires lifecycle **hooks**, and keeps **snapshot history** per share.

**Who it's for (v1):** developers running multiple machines / homelabs who want their active
workspaces mirrored without Git ceremony. Fits a Pi 5 cluster + MacBook + TrueNAS setup
directly. Also the foundation of a future commercial hosted offering (see §13).

---

## 2. Goals / Non-Goals

### Goals
- Continuous, automatic, **bidirectional** sync of shares across linked machines.
- **Shares + selective mounts**: cherry-pick a whole share or any sub-path onto a machine,
  mapped to any local path, with per-mount permissions.
- `.devignore` (gitignore syntax) to exclude paths from sync entirely.
- **Hooks**: run user commands on sync lifecycle events (`pre-push`, `post-push`,
  `pre-pull`, `post-pull`, `on-conflict`).
- **Version history** (Git-like snapshots, per share) so any synced file can be rolled back.
- **Self-hostable hub** — single Go binary, runs on a Pi / TrueNAS, no SaaS dependency.
- Delta/chunked transfer so big files and large trees both stay fast.
- **Never silently lose work.** Conflicts never block and never destroy a byte.
- **Fully cross-platform**: Linux, macOS, Windows.
- Open-source core, with clean seams for a future commercial hosted tier.

### Non-Goals (v1)
- Not a Git replacement — no branching/merging semantics, no PRs.
- No TUI in v1 — `devbox` with no args prints rich text status. (Full TUI parked as a future
  "someday" todo.)
- No mobile/desktop GUI.
- No teams/org ACLs. Single-owner, multi-device. (Permissions are per *device*, not per
  *user*.)
- No re-share / delegation permission (`s`) — deferred to v2; flag syntax stays
  forward-compatible.
- No content-level 3-way text merge — conflicts produce side-by-side copies (v2 question).
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
| Chunking / dedup | FastCDC content-defined chunking + BLAKE3 | Delta sync, dedup big files, content-addressed |
| Transport | HTTP/2 for blobs + WebSocket for live change events | Simple, proxy-friendly (works behind Nginx Proxy Manager) |
| Hub metadata | SQLite (single-writer, WAL) | Zero-ops, fits homelab |
| Blob storage | `BlobStore` interface — disk CAS (v1), S3/R2 impl later | Maps to TrueNAS now, object storage for hosted |
| Auth | Device Ed25519 keypair + bearer token; Docker-style join tokens | No passwords, device-revocable |
| Hooks runner | exec into **bash** (all OSes), env-injected | One hook language everywhere |

---

## 4. Core Concepts

- **Hub** — the server. Publishes shares, stores chunks (CAS) + metadata (SQLite), brokers
  change events. One owner, many devices. Self-hosted single binary.
- **Device** — a machine with an Ed25519 identity, joined to a hub. Runs one daemon.
- **Share** — a named top-level tree on the hub (`projects`, `repos`, `services`). The unit
  of **permission** and of **snapshot history**.
- **Grant** — what a device may do on a share: `r` (pull), `w` (push). Hub-authoritative.
- **Mount** — a device-side binding: `share[/subpath] → localpath` with requested perms,
  clamped to the grant. A device has many mounts; one daemon watches them all.
- **Snapshot** — an immutable per-share manifest version (id = BLAKE3 of manifest).
- **Manifest** — `path → [chunk hashes] + mode + size` for a share at a point in time.

---

## 5. Architecture

```
device (laptop)                 hub (Pi / TrueNAS)              device (pi-07)
 devboxd                         devbox-hub                      devboxd
  ├ mount: projects/  ──┐        ├ shares: projects,      ┌──── mount: projects/p22/backend
  ├ mount: repos/  ─────┼─ WS ─► │   repos, services       │      (read-only -> /var/www)
  └ <mount>/.devbox/    │ HTTP   ├ CAS blobs (BLAKE3) ─ WS ┤    └ hooks fire on pull
    hooks, .devignore   └──────► ├ SQLite: snapshots,  ────┘
                                 │   shares, grants,
                                 │   devices, tokens
                                 └ BlobStore: disk (v1) | S3/R2 (later)
```

### Sync loop (per mount, per machine)
1. `fsnotify` reports a change in a mount's local tree (debounced ~300ms).
2. Path checked against the compiled `.devignore`; ignored paths dropped.
3. Changed files chunked (FastCDC) → BLAKE3 hashes → manifest diff vs last-known state.
4. **`pre-push` hook** fires (can veto). New chunks uploaded to the hub (only ones it lacks).
5. Hub appends a snapshot, advances that **share's linear HEAD**, broadcasts a change event
   over WebSocket.
6. **`post-push` hook** fires locally.
7. Other devices with a mount on that share receive the event → **`pre-pull` hook** → fetch
   missing chunks → reassemble → **atomic rename into place** → **`post-pull` hook**.

A `w`-less (read-only) mount skips steps 3–6 (it never pushes) but still does 7.

---

## 6. Shares, Mounts & Permissions

### Permissions
- `r` = pull/receive, `w` = push your edits, `-a` = `rw`. No `s` in v1 (the file is already
  shared by being on the hub; re-share/delegation is v2).
- **Hub-authoritative.** Each device has a grant per share. The client's `-rw`/`-a` flags are
  a *request*, **clamped** to the grant. Asking for `-w` on a read-only grant → read-only +
  a warning. A fat-fingered flag can't pollute canonical upstream.
- **Tokens** (Docker-style, rotatable): `devbox-hub token manager|member`.
  - **Manager** token → device can create shares, mint tokens, manage/revoke devices, rw all.
  - **Member** token → device gets `rw` on shares by default; downgradable per share
    (`devbox-hub grant pi-07 projects r` for a read-only deploy box).

### Creating a share
`devbox publish <localdir> <name>` (manager/write capability). Publishing is the explicit
verb (since `s` is gone, share creation is not a mount flag).

### Mounting
`devbox -rw <share>[/subpath] [localpath]` — mount a whole share or any sub-path to any local
path. Default `localpath` = `./<last-path-segment>`.

```
devbox -a projects ./                         # whole share, rw, here
devbox -r projects/p22/backend /var/www/p22/backend   # one sub-path, read-only, elsewhere
```

A mount is recorded in `~/.config/devbox/daemon.toml` (see §8).

### Read-only mounts & data safety
A read-only mount pulls but never pushes. To still honor "never lose a byte": if an inbound
change would clobber a **locally-modified** file on a read-only mount, devbox first stashes
the local version as `path.conflict-local-<ts>.ext` (preserved on disk, never pushed), then
applies the inbound change. So even pull-only boxes never lose local edits.

---

## 7. Consistency & Conflict Model

The mechanism behind "Dropbox-easy, never loses data." Runs **per share**.

- Each share has **one linear HEAD** on the hub. SQLite single-writer serializes concurrent
  pushes for free.
- A push declares the snapshot it was based on (`parent_id`).
  - `parent == HEAD` → fast-forward: accept, advance HEAD.
  - `parent != HEAD` → **per-file 3-way** vs the common ancestor (the pusher's `parent`
    snapshot):
    - File changed by **only** the pusher → applies cleanly.
    - File changed by **both** sides → **conflict**: the first-to-land (current HEAD) stays
      **canonical**; the loser is written beside it as `path.conflict-<host>-<ts>.ext`;
      `on-conflict` fires. The conflict copy is itself a new path → it syncs everywhere, so
      no edit is ever lost.
- **Comparison is by BLAKE3 content hash**, not timestamps. Touching a file without changing
  bytes is a no-op — no false conflict.
- **Deletes** propagate (it's sync) but are always recoverable from snapshots.
  **Delete-vs-edit never loses the edit:** a delete has no bytes to preserve, so the edit
  always wins (survives as canonical, or as a `.conflict` copy if it lost ordering).
- **Renames are free**: old-path-gone + new-path-added; chunks are content-addressed, so zero
  bytes re-transfer. No special rename detection in v1.
- **Initial mount/bootstrap**: mounting a share onto a non-empty local dir reconciles per-file
  against the hub HEAD via the same conflict path (ancestor = empty). Matching files (by hash)
  are kept; differing local files become `.conflict` copies; the hub stays canonical.

**Never blocks. Never asks. Never destroys a byte.**

### Conflict UX (per-machine, configurable in `config.toml`)
All four modes ship; each is a toggle. Default = non-blocking (badge + hook + resolver).
- **Status badge** — `devbox status` always shows pending-conflict count. Pull-based, zero
  interruption. (Always on.)
- **`on-conflict` push alert** — a default hook fires when a conflict copy is created; wire it
  to ntfy/desktop/Slack. Non-blocking.
- **`devbox conflicts` resolver** — list, diff, resolve on demand: keep-mine / keep-theirs /
  keep-both / open-in-`$EDITOR`. You choose when to look.
- **Blocking prompt** — *opt-in only*. Daemon prompts at conflict time. Off by default
  (breaks Dropbox-ease); available for those who want it.

---

## 8. State on Disk

- **Machine-global** `~/.config/devbox/`:
  - Ed25519 keypair (one identity per device; **never leaves the box**).
  - `daemon.toml` — the mount list (share, subpath, localpath, perms, hub).
  - `config.toml` — global defaults (conflict-UX prefs, hook timeout, etc.).
- **Per-mount** `<localpath>/.devbox/`:
  - `hooks/` — per-machine lifecycle scripts (never synced).
  - last-synced snapshot id, mount-local config overrides.
  - Always implicitly ignored from sync.
- **`.devignore`** — a *synced* file at a share root (gitignore syntax, shared across
  devices). Nested `.devignore` files allowed, scoped to their subtree.

```toml
# ~/.config/devbox/daemon.toml
[[mount]]
share = "projects"
subpath = ""
local = "/Users/jh/Projects"
perms = "rw"
hub = "hub.shoemoney.ai"

[[mount]]
share = "projects"
subpath = "p22/backend"
local = "/var/www/p22/backend"
perms = "r"
hub = "hub.shoemoney.ai"
```

```toml
# ~/.config/devbox/config.toml
[conflicts]
badge = true            # always on
on_conflict_hook = true
resolver = true
blocking_prompt = false # opt-in

[hooks]
interpreter = "bash"    # all OSes; Windows needs git-bash/WSL (devbox doctor checks)
timeout_secs = 60
```

---

## 9. `.devignore` Spec

Gitignore syntax, evaluated relative to the share/mount root. Lives at the share root; nested
`.devignore` files allowed and scoped to their subtree.

```
# devbox: paths here are NEVER synced
node_modules/
vendor/
dist/
build/
.next/
target/
*.log
*.tmp
.DS_Store

# secrets: never leave the machine
.env
.env.*
*.pem
*.key
secrets/

# negate to force-include
!.env.example
```

- A matched path is invisible to sync in **both** directions (won't upload, won't accept
  inbound writes).
- `!pattern` negation re-includes, gitignore precedence.
- Changing `.devignore` triggers a rescan; newly-ignored files are **left on disk** (never
  deleted) but stop syncing.
- `.devbox/` is always implicitly ignored.

---

## 10. Hooks Spec

Per-machine scripts in `<mount>/.devbox/hooks/`, named after the event, **never synced**.
devbox shells out to `bash <hook>` on every OS (one hook language; `devbox doctor` flags a
missing bash and points Windows users at git-bash/WSL). Non-zero exit on a `pre-*` hook
**aborts** that sync step. Default timeout 60s — a hung hook is killed + logged, never wedges
the loop.

| Hook | Fires | Can abort? | Typical use |
| --- | --- | --- | --- |
| `pre-push` | before local changes upload | ✅ | lint/format, block on secret scan |
| `post-push` | after upload confirmed | ❌ | notify, log, tag a snapshot |
| `pre-pull` | before inbound changes apply | ✅ | stop a container/dev server |
| `post-pull` | after inbound changes applied | ❌ | `pnpm install`, migrate, restart service |
| `on-conflict` | conflict copy created | ❌ | open a diff, ping you, log |

### Injected environment
```bash
DEVBOX_EVENT=post-pull
DEVBOX_MOUNT=/srv/project          # local mount root
DEVBOX_SHARE=projects
DEVBOX_HOST=pi-node-07
DEVBOX_CHANGED_FILES=/tmp/devbox-changes.txt   # newline-delimited list
DEVBOX_SNAPSHOT=ab12cd34
DEVBOX_REMOTE=hub.shoemoney.ai
```

### Example `post-pull`
```bash
#!/usr/bin/env bash
# reinstall deps only if manifest changed, then restart the container
if grep -qE 'package\.json|pnpm-lock\.yaml' "$DEVBOX_CHANGED_FILES"; then
  pnpm install --frozen-lockfile
fi
docker compose restart app
```

---

## 11. Versioning / Snapshots

- Every accepted change set on a share creates an immutable **snapshot** (id = BLAKE3 of the
  manifest). Cheap — chunks are content-addressed and deduped across all shares.
- `devbox log` lists snapshots; `devbox restore <snapshot> [path]` rolls a file or the whole
  share back (a restore is itself a new change → fully reversible).
- Retention configurable on the hub (keep all / keep N / time-window). `devbox-hub gc`
  garbage-collects unreferenced chunks via refcounts.

---

## 12. Cross-Platform Handling (Linux / macOS / Windows)

Go cross-compiles to all three; `fsnotify` covers inotify / FSEvents / ReadDirectoryChangesW.
Explicit handling for what doesn't come free:

- **Paths**: canonical form is **forward-slash, relative**; convert to `\` only at the Windows
  filesystem boundary.
- **Atomic apply**: `os.Rename`-over-existing isn't atomic on Windows → use
  `ReplaceFile`/`MoveFileEx` there; POSIX `rename(2)` elsewhere.
- **Name clashes / reserved names**: a Linux box can create `foo.go` *and* `Foo.go`, or files
  named `aux`/`com1`/with a trailing dot — illegal or colliding on Mac/Windows. Policy:
  **skip + warn + surface** under "unsyncable here" in `devbox status`. The hub keeps the
  bytes; OSes that can hold the name still get the file. Never fatal, never blocks the tree.
- **Line endings**: bytes synced verbatim; devbox does not normalize CRLF/LF (out of scope).
- **Service install**: `devbox start|stop` abstracts launchd (mac) / systemd (linux) /
  Windows Service.
- **Hooks**: bash everywhere (see §10); `devbox doctor` verifies bash on PATH.

---

## 13. Open-Core / Commercial Model

**Open-source core + commercial hosted tier.** The OSS product is the fully self-hostable hub
+ clients (this entire v1 spec). The hosted version is a **separate, closed control plane**
(signup, billing, quotas, provisioning) — **out of scope for v1**, but the core carries clean
seams so it isn't a retrofit:

- **`BlobStore` interface** — disk CAS impl in v1 (Pi/TrueNAS); S3/R2 impl dropped in later
  for the hosted tier. One interface, one implementation now.
- **Config-driven limits** — the hub reads `max_storage`, `retention`, `max_devices` from
  config. Self-host = unlimited; the commercial layer sets these per plan tier.
- **Single-tenant core** — the OSS hub knows nothing about accounts/billing.

### License (decided): AGPLv3 core + commercial license for the hosted tier
Self-host free; anyone offering it as a hosted service must open their mods or buy a
commercial license. Add a contributor CLA once outside PRs arrive (so dual-licensing stays
possible). Revisit only if adoption friction (corp AGPL bans) becomes a real funnel problem.

### Parked recommendations (confirm before launching hosting — neither changes the v1 build):
- **Tenancy**: per-tenant hub instance — the control plane provisions one clean single-tenant
  hub (own SQLite + blob bucket) per account. Zero tenancy code in the OSS core; strong
  isolation. Multi-tenant consolidation is a later scale optimization.
- **Pricing**: small monthly fee, discount for annual. Storage/device/retention tiers enforced
  via the config knobs above.

---

## 14. Security (v1)

- Each device generates an **Ed25519** keypair on first run; the private key never leaves the
  machine. Joining a hub authorizes the device's pubkey.
- **Join tokens** (manager/member) enroll devices; tokens are rotatable and one-time/expiring.
- All hub traffic over **TLS** (behind Nginx Proxy Manager / Let's Encrypt).
- `.devignore` secret patterns keep `.env`, keys, etc. on the machine. Optional `pre-push`
  secret-scan hook as belt-and-suspenders.
- **Revoke** a lost device with `devbox-hub device revoke <id>` — its bearer token dies
  immediately.
- (Deferred to v2) optional client-side E2E chunk encryption so the hub stores only ciphertext.

---

## 15. CLI Surface (v1)

```
# device
devbox join <hub> <token>           # enroll this device (Docker-style)
devbox -rw <share>[/sub] [path]     # mount a share/sub-path (default path ./<name>)
devbox -a  <share> [path]           # mount with all (rw) perms
devbox publish <localdir> <name>    # create a share from a local folder (manager/write)
devbox unmount <share>              # stop syncing a mount (files stay on disk)
devbox start | stop                 # run/stop the daemon (devboxd)
devbox status                       # rich text: shares, mounts, peers, conflicts, pending
devbox pause | resume               # halt sync without unmounting
devbox log                          # snapshot history
devbox restore <snap> [path]        # roll back a file or a whole share
devbox conflicts                    # list + diff + resolve (mine/theirs/both/edit)
devbox ignore <pattern>             # append to .devignore + rescan
devbox hook edit <event>            # open a hook script in $EDITOR
devbox peers                        # linked machines, online/offline, last seen
devbox doctor                       # diagnose watcher limits, perms, bash, connectivity

# hub
devbox-hub serve --config /etc/devbox-hub.toml
devbox-hub token manager|member     # mint a rotatable join token
devbox-hub device list | revoke <id>
devbox-hub grant <device> <share> r|rw
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
  role        TEXT NOT NULL,         -- 'manager' | 'member'
  last_seen   INTEGER,
  revoked     INTEGER DEFAULT 0
);

CREATE TABLE tokens (
  hash        TEXT PRIMARY KEY,      -- hash of the join token
  role        TEXT NOT NULL,         -- 'manager' | 'member'
  expires_at  INTEGER,
  used        INTEGER DEFAULT 0
);

CREATE TABLE shares (
  name          TEXT PRIMARY KEY,
  head_snapshot TEXT,                -- current linear HEAD
  created_by    TEXT NOT NULL,
  created_at    INTEGER NOT NULL
);

CREATE TABLE grants (
  device_id   TEXT NOT NULL,
  share       TEXT NOT NULL,
  perms       TEXT NOT NULL,         -- 'r' | 'rw'
  PRIMARY KEY (device_id, share)
);

CREATE TABLE snapshots (
  id          TEXT PRIMARY KEY,      -- blake3(manifest)
  share       TEXT NOT NULL,
  parent_id   TEXT,
  device_id   TEXT NOT NULL,
  created_at  INTEGER NOT NULL,
  manifest    BLOB NOT NULL          -- gzipped: path -> [chunk hashes], mode, size
);

CREATE TABLE chunks (
  hash        TEXT PRIMARY KEY,      -- blake3
  size        INTEGER NOT NULL,
  refcount    INTEGER NOT NULL DEFAULT 0
);
```

Blobs stored via the `BlobStore` interface; disk impl at `blobs/<first2>/<hash>`,
content-addressed and **deduped across all shares**. Refcounts drive GC.

> `ponytail:` manifest is a full gzipped BLOB per snapshot — fine to tens-of-thousands of
> files once `.devignore` cuts the fat. If trees get huge, chunk the manifest itself
> (content-address it like any other blob).

---

## 17. Milestones

| M | Deliverable | Done = |
| --- | --- | --- |
| **M0 — Skeleton** | Go monorepo, `cobra` CLI, `devbox join`, keypair gen, machine config | `devbox join` enrolls a device against a hub |
| **M1 — Watch + ignore** | `fsnotify` watcher, `.devignore` matcher, manifest builder (per mount) | Edits produce a correct filtered manifest diff |
| **M2 — Hub + one-way push** | `devbox-hub serve`, shares, tokens, `BlobStore` (disk CAS), `publish`, chunk upload, snapshots | A device publishes a share and pushes; snapshots land |
| **M3 — Two-way sync** | WebSocket fan-out, mount/pull, atomic apply, per-file 3-way conflict copies | Two devices stay mirrored live; conflicts produce copies |
| **M4 — Permissions** | manager/member tokens, read-only grants, sub-path mounts, flag clamping, read-only data-safety stash | A read-only deploy mount pulls and never pushes |
| **M5 — Hooks** | bash lifecycle runner, templates, env injection, timeout | `post-pull` runs `pnpm install` on a Pi node |
| **M6 — Versioning** | `devbox log` / `restore`, hub GC | Roll back any file from history |
| **M7 — Hardening** | `devbox doctor`, watcher-limit handling, reconnect/backoff, name-clash handling, arm64/mac/win release builds | Survives reboots & flaky network on the cluster |

> Open-core seams (BlobStore interface, config-driven limits) land naturally in M2.
> Commercial control plane (accounts/billing/provisioning) is a **separate sub-project**,
> not part of this v1.

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
│   ├── chunk/         # FastCDC + blake3
│   ├── manifest/      # tree state + diff
│   ├── mount/         # mount config, perms, clamping
│   ├── sync/          # push/pull engine, conflict resolution
│   ├── hooks/         # lifecycle runner (bash)
│   ├── transport/     # ws + http client
│   └── hub/           # server, sqlite, shares, grants, tokens, CAS, gc
│       └── blobstore/ # BlobStore interface: disk (v1), s3/r2 (later)
├── pkg/proto/         # shared wire types
└── deploy/            # systemd/launchd/winsvc units, docker-compose for hub
```

---

## 19. Key Risks / Open Decisions

- **Watcher limits at scale** — large trees blow past inotify limits on Linux; mitigated by
  `.devignore` cutting `node_modules` etc., plus a fallback periodic rescan and `devbox
  doctor` to raise `fs.inotify.max_user_watches`.
- **Conflict UX depth** — v1 keeps `.conflict` copies; content-level 3-way text merge is a v2
  question.
- **Hub HA** — v1 is single-hub (SQLite). For a large cluster, run the hub on one stable node
  + TrueNAS-backed blob dir; clustering the hub is v2.
- **License & tenancy** — parked recommendations in §13; confirm before launching hosting.
- **Central hub vs P2P** — central hub for v1 (simpler, matches the "Dropbox" mental model,
  always-on infra exists). A P2P mesh transport can be added later without changing the
  CLI/hook contract.

---

## 20. Backlog & Audit Notes

Captured from a ponytail (lazy-senior) audit of this design. **Not decided** — to weigh
during implementation planning. Nothing here overrides the choices in the body above.

### Candidate v1 simplifications (revisit when planning each milestone)
- **Drop the blocking-prompt conflict mode.** A headless daemon can't show a prompt, and it
  contradicts "never asks." The `on-conflict` hook + `devbox conflicts` already cover "tell
  me." [§7]
- **Collapse the 4 conflict-notification modes to 1.** Conflict copies on disk + `devbox
  status` badge + the `on-conflict` hook (the real notification API). Pull the mine/theirs/
  both/edit resolver to v2. [§7]
- **Replace manager/member roles + grants table + flag clamping with a per-mount `readonly`
  flag.** v1 is single-owner; the only real need is "deploy boxes don't push." Full role/ACL
  machinery is multi-user (a §2 non-goal) arriving early. [§6, §16]
- **One transport instead of two.** HTTP/2 blobs + WebSocket events = two reconnect/backoff
  paths. Consider HTTP/2 + SSE, or WebSocket for both. [§3, §5]
- **Content-address the manifest from day one** (or store deltas) instead of a full gzipped
  BLOB per snapshot — avoids storing the whole path→chunks map on every push. [§16]
- **Reconsider bash-on-Windows.** Forcing bash makes every Windows user install git-bash/WSL
  before hook #1; PowerShell is zero-install. Trades user setup friction for our convenience.
  [§10, §12]

### Candidate features (v2 unless promoted)
- **LAN peer chunk-exchange + bandwidth cap.** 40 nodes pulling one big change from the hub
  over WAN saturates its uplink; co-located nodes should swap chunks directly (Syncthing-style).
- **Default-on secret protection.** A built-in deny-list that hard-refuses to upload matched
  secret patterns even if `.devignore` is misconfigured — not just an optional `pre-push` hook.
- **`devbox deploy <share> <snapshot>`.** Surface immutable snapshots as pinnable, atomically-
  flippable targets for read-only mounts (blue/green for `/var/www` boxes).
- **Laptop sanity:** pause-on-metered / pause-on-battery / scheduled sync windows.
- **Hub `/metrics` + status page.** Prometheus metrics + health (shares, devices online,
  storage, conflicts, GC). Fleet observability now; commercial-tier dashboard seam later.
```
