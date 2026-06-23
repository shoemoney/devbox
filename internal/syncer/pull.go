package syncer

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"git.shoemoney.ai/shoemoney/devbox/internal/hooks"
	"git.shoemoney.ai/shoemoney/devbox/internal/ignore"
	"git.shoemoney.ai/shoemoney/devbox/internal/manifest"
	"git.shoemoney.ai/shoemoney/devbox/internal/secret"
	"git.shoemoney.ai/shoemoney/devbox/internal/transport"
)

// PullResult summarizes applying a remote head to the local tree.
type PullResult struct {
	Base      string   // the snapshot now applied locally (the head we merged to)
	Written   []string // files written/updated from the hub
	Deleted   []string // files deleted to match the hub
	Conflicts []string // conflict copies created (local edits preserved beside canonical)
	Skipped   []string // paths a filesystem error skipped (e.g. file<->dir clash)
	Vetoed    bool     // a pre-pull hook aborted applying inbound changes
}

// Pull fetches the hub head manifest for share, three-way merges it against the
// local tree (base = last-applied snapshot, "" if none) and applies the result:
//   - a file changed only on the hub is written/deleted locally;
//   - a file changed only locally is left alone (it will be pushed);
//   - a file changed on BOTH sides keeps the hub version canonical, and the local
//     edit is preserved beside it as path.conflict-<host>-<ts>.ext;
//   - delete-vs-edit never loses the edit (the edit always survives).
//
// Never destroys a byte: every losing local edit becomes a conflict copy.
func Pull(c *transport.Client, root, share, subpath, base, host string, now int64, ig *ignore.Matcher, guard *secret.Guard, hk *hooks.Runner) (PullResult, error) {
	head, err := c.Head(share)
	if err != nil {
		return PullResult{}, err
	}
	res := PullResult{Base: base}
	if head == "" || head == base {
		return res, nil // nothing on the hub, or already up to date
	}

	theirsFull, err := fetchManifest(c, head)
	if err != nil {
		return PullResult{}, err
	}
	theirs := filterStrip(theirsFull, subpath)
	var baseM manifest.Manifest
	if base != "" {
		baseFull, err := fetchManifest(c, base)
		if err != nil {
			return PullResult{}, err
		}
		baseM = filterStrip(baseFull, subpath)
	}
	ours, _, err := manifest.Build(root, ig, guard)
	if err != nil {
		return PullResult{}, err
	}

	// pre-pull may veto (e.g. stop a running container before files change).
	incoming := manifest.Diff(baseM, theirs)
	pending := append(append(append([]string{}, incoming.Added...), incoming.Modified...), incoming.Deleted...)
	if err := hk.Run(hooks.PrePull, pending, head); err != nil {
		return PullResult{Base: base, Vetoed: true}, nil
	}

	bi, oi, ti := indexEntries(baseM), indexEntries(ours), indexEntries(theirs)
	for p := range unionKeys(bi, oi, ti) {
		be, bOK := bi[p]
		oe, oOK := oi[p]
		te, tOK := ti[p]

		oursCh := oOK != bOK || (oOK && bOK && !manifest.SameContent(oe, be))
		theirsCh := tOK != bOK || (tOK && bOK && !manifest.SameContent(te, be))
		if !theirsCh {
			continue // hub didn't touch p; local state (changed or not) stands
		}

		switch {
		case !oursCh:
			// Hub-only change: apply it verbatim.
			if tOK {
				if fsErr, err := writeEntry(c, root, te); err != nil {
					if !fsErr {
						return PullResult{}, err // network error: abort, retry whole sync
					}
					res.Skipped = append(res.Skipped, p) // e.g. path is now a directory
				} else {
					res.Written = append(res.Written, p)
				}
			} else if err := deleteFile(root, p); err != nil {
				res.Skipped = append(res.Skipped, p)
			} else {
				res.Deleted = append(res.Deleted, p)
			}

		case tOK && oOK && manifest.SameContent(te, oe):
			// Both made the identical change — already in agreement.

		case tOK && oOK:
			// Both edited differently: preserve ours as a conflict copy, hub canonical.
			cp, cerr := preserveAsConflict(root, p, host, now)
			if cerr != nil {
				res.Skipped = append(res.Skipped, p) // couldn't preserve ours; never clobber
				continue
			}
			if fsErr, err := writeEntry(c, root, te); err != nil {
				if !fsErr {
					return PullResult{}, err
				}
				res.Skipped = append(res.Skipped, p)
			} else {
				res.Conflicts = append(res.Conflicts, cp)
				res.Written = append(res.Written, p)
			}

		case tOK && !oOK:
			// p is absent from `ours` — but `ours` is .devignore/secret-guard
			// FILTERED, so this is NOT necessarily a local delete. The file may
			// still be on disk with valuable content (an ignored config, or the
			// very .env/key the guard protects). Overwriting it blindly is data
			// loss — exactly the bytes we promised never to lose. So if the path
			// really exists on disk, preserve it as a conflict copy first; only a
			// genuinely-absent path is a true delete where the hub wins.
			if dst, jerr := safeJoin(root, p); jerr == nil {
				if _, statErr := os.Stat(dst); statErr == nil {
					cp, cerr := preserveAsConflict(root, p, host, now)
					if cerr != nil {
						res.Skipped = append(res.Skipped, p) // couldn't preserve; never clobber
						continue
					}
					res.Conflicts = append(res.Conflicts, cp)
				}
			}
			if fsErr, err := writeEntry(c, root, te); err != nil {
				if !fsErr {
					return PullResult{}, err
				}
				res.Skipped = append(res.Skipped, p)
			} else {
				res.Written = append(res.Written, p)
			}

		default:
			// Hub deleted, ours edited (!tOK && oOK): the edit wins — keep ours,
			// it will be re-pushed. Or both deleted — nothing to do.
		}
	}

	if len(res.Conflicts) > 0 {
		_ = hk.Run(hooks.OnConflict, res.Conflicts, head)
	}
	_ = hk.Run(hooks.PostPull, append(append([]string{}, res.Written...), res.Deleted...), head)

	res.Base = head
	return res, nil
}

// Restore rewrites the local tree to match an older snapshot, then pushes it as a
// new head — so a restore is itself a reversible snapshot, never a history rewrite.
//
// With onlyPath set, just that one file is brought back to its snapshot content
// (the rest of the tree is untouched). Otherwise the whole mounted subtree is made
// to match the snapshot: every snapshot entry is written and any local path the
// snapshot doesn't contain is deleted. A bad onlyPath (not in the snapshot) errors
// without touching the tree.
func Restore(c *transport.Client, root, share, subpath, snapshot, onlyPath, host string, now int64, ig *ignore.Matcher, guard *secret.Guard, hk *hooks.Runner) (Result, error) {
	if _, err := applySnapshot(c, root, subpath, snapshot, onlyPath, ig, guard); err != nil {
		return Result{}, err
	}
	head, err := c.Head(share)
	if err != nil {
		return Result{}, err
	}
	return Push(c, root, share, subpath, ig, guard, head, hk)
}

// DeployResult summarizes pinning a mount to a snapshot.
type DeployResult struct {
	Snapshot string // the snapshot now applied (== its manifest blob hash)
	Written  int    // files written from the snapshot
}

// Deploy rewrites the mounted subtree to match a snapshot WITHOUT pushing — it
// pins a (typically read-only) mount to a specific version, e.g. blue/green
// deploys of /var/www. Unlike Restore it creates no new head: the snapshot stays
// exactly as-is on the hub and history is untouched. The caller pins the mount
// (config.Mount.Pinned) so the daemon won't live-advance it back to head.
func Deploy(c *transport.Client, root, subpath, snapshot string, ig *ignore.Matcher, guard *secret.Guard) (DeployResult, error) {
	n, err := applySnapshot(c, root, subpath, snapshot, "", ig, guard)
	if err != nil {
		return DeployResult{}, err
	}
	return DeployResult{Snapshot: snapshot, Written: n}, nil
}

// applySnapshot rewrites root to match a snapshot's manifest (filtered+stripped
// to subpath): writes every entry and, when onlyPath is empty, deletes any local
// path the snapshot doesn't contain. With onlyPath set, only that one entry is
// written (no deletes). Returns the number of entries written. A bad onlyPath
// (not in the snapshot) errors without touching the tree.
func applySnapshot(c *transport.Client, root, subpath, snapshot, onlyPath string, ig *ignore.Matcher, guard *secret.Guard) (int, error) {
	full, err := fetchManifest(c, snapshot)
	if err != nil {
		return 0, err
	}
	target := filterStrip(full, subpath)
	idx := indexEntries(target)

	if onlyPath != "" {
		e, ok := idx[onlyPath]
		if !ok {
			return 0, fmt.Errorf("path %q not in snapshot %s", onlyPath, snapshot)
		}
		if _, err := writeEntry(c, root, e); err != nil {
			return 0, err
		}
		return 1, nil
	}

	for _, e := range target.Entries {
		if _, err := writeEntry(c, root, e); err != nil {
			return 0, err
		}
	}
	// Delete tracked files present locally but absent from the snapshot. We diff
	// against manifest.Build (ignore- + secret-guard-filtered) ON PURPOSE: a
	// restore/deploy must NOT delete a user's .devignore'd runtime files (caches,
	// logs) or local secrets — those were never in any snapshot, so "match the
	// snapshot" leaves them untouched. Tracked extras (e.g. a file added in a newer
	// snapshot) are still removed. ponytail: never lose a byte we didn't sync.
	local, _, err := manifest.Build(root, ig, guard)
	if err != nil {
		return 0, err
	}
	for _, e := range local.Entries {
		if _, keep := idx[e.Path]; !keep {
			if err := deleteFile(root, e.Path); err != nil {
				return 0, err
			}
		}
	}
	return len(target.Entries), nil
}

// Sync brings a mount into agreement with the hub: pull+merge to head, then push
// the merged local state; retries if the hub advanced underneath. Returns the new
// base snapshot and the pull result.
func Sync(c *transport.Client, root, share, subpath, base, host string, now int64, ig *ignore.Matcher, guard *secret.Guard, hk *hooks.Runner) (string, PullResult, error) {
	for attempt := 0; attempt < 5; attempt++ {
		pr, err := Pull(c, root, share, subpath, base, host, now, ig, guard, hk)
		if err != nil {
			return base, pr, err
		}
		if pr.Vetoed {
			return base, pr, nil // pre-pull vetoed; wait for the next nudge
		}
		base = pr.Base
		push, err := Push(c, root, share, subpath, ig, guard, base, hk)
		if err != nil {
			return base, pr, err
		}
		if push.Vetoed {
			return base, pr, nil // pre-push vetoed
		}
		if push.Conflict {
			base = push.Head // hub advanced; re-pull against the new head
			continue
		}
		return push.Snapshot, pr, nil
	}
	return base, PullResult{}, fmt.Errorf("sync: too many conflict retries")
}

// --- sub-path helpers: a mount's local root maps to share/subpath/ ---

// under reports whether a share-relative path lives within subpath ("" = whole share).
func under(path, subpath string) bool {
	if subpath == "" {
		return true
	}
	return path == subpath || strings.HasPrefix(path, subpath+"/")
}

// strip removes the subpath prefix, yielding a local-relative path.
func strip(path, subpath string) string {
	if subpath == "" {
		return path
	}
	return strings.TrimPrefix(path, subpath+"/")
}

// prefixPath adds the subpath prefix to a local-relative path.
func prefixPath(path, subpath string) string {
	if subpath == "" {
		return path
	}
	return subpath + "/" + path
}

// filterStrip keeps only the entries of a full share manifest that live under
// subpath, with the prefix stripped (so they're local-relative).
func filterStrip(m manifest.Manifest, subpath string) manifest.Manifest {
	if subpath == "" {
		return m
	}
	var out []manifest.Entry
	for _, e := range m.Entries {
		if under(e.Path, subpath) {
			e.Path = strip(e.Path, subpath)
			out = append(out, e)
		}
	}
	return manifest.Manifest{Entries: out}
}

func fetchManifest(c *transport.Client, id string) (manifest.Manifest, error) {
	b, err := c.GetBlob(id)
	if err != nil {
		return manifest.Manifest{}, err
	}
	return manifest.Unmarshal(b)
}

func indexEntries(m manifest.Manifest) map[string]manifest.Entry {
	idx := make(map[string]manifest.Entry, len(m.Entries))
	for _, e := range m.Entries {
		idx[e.Path] = e
	}
	return idx
}

func unionKeys(maps ...map[string]manifest.Entry) map[string]struct{} {
	u := map[string]struct{}{}
	for _, m := range maps {
		for k := range m {
			u[k] = struct{}{}
		}
	}
	return u
}

// writeEntry downloads e's content and writes it atomically. A network/download
// error returns fsErr=false (the caller should abort and retry the whole sync);
// a filesystem error — e.g. the path is currently a directory — returns
// fsErr=true so the caller can skip just that path instead of wedging the mount.
func writeEntry(c *transport.Client, root string, e manifest.Entry) (fsErr bool, err error) {
	var buf []byte
	for _, h := range e.Chunks {
		b, gerr := c.GetBlob(h)
		if gerr != nil {
			return false, gerr // network
		}
		buf = append(buf, b...)
	}
	dst, serr := safeJoin(root, e.Path)
	if serr != nil {
		return true, serr // unsafe manifest path: skip it, don't apply outside root
	}
	if merr := os.MkdirAll(filepath.Dir(dst), 0o755); merr != nil {
		return true, merr
	}
	mode := os.FileMode(e.Mode)
	if mode == 0 {
		mode = 0o644
	}
	if werr := atomicWrite(dst, buf, mode); werr != nil {
		return true, werr
	}
	return false, nil
}

func deleteFile(root, relPath string) error {
	p, serr := safeJoin(root, relPath)
	if serr != nil {
		return serr
	}
	err := os.Remove(p)
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

// safeJoin joins a hub-supplied, slash-separated relative path under root,
// refusing anything that would escape the mount (absolute path, "..", empty, or
// a Windows reserved name). Manifest paths come from the hub/peers and are
// applied to the local filesystem, so an unchecked "../../.ssh/authorized_keys"
// would let a malicious or buggy hub write or delete files outside the mount.
func safeJoin(root, relPath string) (string, error) {
	clean := filepath.FromSlash(relPath)
	if !filepath.IsLocal(clean) {
		return "", fmt.Errorf("refusing unsafe manifest path %q", relPath)
	}
	return filepath.Join(root, clean), nil
}

// preserveAsConflict renames the current local file at relPath to a sibling named
// path.conflict-<host>-<ts>.ext, returning the conflict copy's relative path.
func preserveAsConflict(root, relPath, host string, ts int64) (string, error) {
	ext := filepath.Ext(relPath)
	stem := strings.TrimSuffix(relPath, ext)
	src := filepath.Join(root, filepath.FromSlash(relPath))
	// Probe for a free name (ts is nanoseconds, so collisions are already rare;
	// the counter is belt-and-suspenders against ever overwriting a prior copy).
	for n := 0; ; n++ {
		cp := fmt.Sprintf("%s.conflict-%s-%d%s", stem, host, ts, ext)
		if n > 0 {
			cp = fmt.Sprintf("%s.conflict-%s-%d-%d%s", stem, host, ts, n, ext)
		}
		dst := filepath.Join(root, filepath.FromSlash(cp))
		if _, err := os.Stat(dst); err == nil {
			continue // taken
		} else if !os.IsNotExist(err) {
			return "", err
		}
		if err := os.Rename(src, dst); err != nil {
			return "", err
		}
		return cp, nil
	}
}

// atomicWrite writes data to path via a temp file + rename (atomic on POSIX and
// Windows; never leaves a half-written file in place).
func atomicWrite(path string, data []byte, perm os.FileMode) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".devbox-tmp-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	// Flush the data to the platter before the rename publishes it: a power loss
	// after the rename but before the bytes land would leave a truncated/empty file.
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp, perm); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	// fsync the parent dir so the rename entry itself survives a crash.
	if dir, derr := os.Open(filepath.Dir(path)); derr == nil {
		_ = dir.Sync() // ponytail: dir fsync is best-effort; unsupported on Windows
		dir.Close()
	}
	return nil
}
