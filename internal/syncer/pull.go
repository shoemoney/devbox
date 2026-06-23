package syncer

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

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
func Pull(c *transport.Client, root, share, subpath, base, host string, now int64, ig *ignore.Matcher, guard *secret.Guard) (PullResult, error) {
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
			// Ours deleted, hub edited: hub wins (a delete has no bytes to keep).
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

	res.Base = head
	return res, nil
}

// Sync brings a mount into agreement with the hub: pull+merge to head, then push
// the merged local state; retries if the hub advanced underneath. Returns the new
// base snapshot and the pull result.
func Sync(c *transport.Client, root, share, subpath, base, host string, now int64, ig *ignore.Matcher, guard *secret.Guard) (string, PullResult, error) {
	for attempt := 0; attempt < 5; attempt++ {
		pr, err := Pull(c, root, share, subpath, base, host, now, ig, guard)
		if err != nil {
			return base, pr, err
		}
		base = pr.Base
		push, err := Push(c, root, share, subpath, ig, guard, base)
		if err != nil {
			return base, pr, err
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
	dst := filepath.Join(root, filepath.FromSlash(e.Path))
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
	err := os.Remove(filepath.Join(root, filepath.FromSlash(relPath)))
	if os.IsNotExist(err) {
		return nil
	}
	return err
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
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp, perm); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
