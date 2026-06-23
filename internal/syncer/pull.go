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
func Pull(c *transport.Client, root, share, base, host string, now int64, ig *ignore.Matcher, guard *secret.Guard) (PullResult, error) {
	head, err := c.Head(share)
	if err != nil {
		return PullResult{}, err
	}
	res := PullResult{Base: base}
	if head == "" || head == base {
		return res, nil // nothing on the hub, or already up to date
	}

	theirs, err := fetchManifest(c, head)
	if err != nil {
		return PullResult{}, err
	}
	var baseM manifest.Manifest
	if base != "" {
		if baseM, err = fetchManifest(c, base); err != nil {
			return PullResult{}, err
		}
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

		oursCh := oOK != bOK || (oOK && bOK && !sameEntry(oe, be))
		theirsCh := tOK != bOK || (tOK && bOK && !sameEntry(te, be))
		if !theirsCh {
			continue // hub didn't touch p; local state (changed or not) stands
		}

		switch {
		case !oursCh:
			// Hub-only change: apply it verbatim.
			if tOK {
				if err := writeFileFromChunks(c, root, te); err != nil {
					return PullResult{}, err
				}
				res.Written = append(res.Written, p)
			} else {
				if err := deleteFile(root, p); err != nil {
					return PullResult{}, err
				}
				res.Deleted = append(res.Deleted, p)
			}

		case tOK && oOK && sameEntry(te, oe):
			// Both made the identical change — already in agreement.

		case tOK && oOK:
			// Both edited differently: preserve ours as a conflict copy, hub canonical.
			cp, err := preserveAsConflict(root, p, host, now)
			if err != nil {
				return PullResult{}, err
			}
			if err := writeFileFromChunks(c, root, te); err != nil {
				return PullResult{}, err
			}
			res.Conflicts = append(res.Conflicts, cp)
			res.Written = append(res.Written, p)

		case tOK && !oOK:
			// Ours deleted, hub edited: hub wins (a delete has no bytes to keep).
			if err := writeFileFromChunks(c, root, te); err != nil {
				return PullResult{}, err
			}
			res.Written = append(res.Written, p)

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
func Sync(c *transport.Client, root, share, base, host string, now int64, ig *ignore.Matcher, guard *secret.Guard) (string, PullResult, error) {
	for attempt := 0; attempt < 5; attempt++ {
		pr, err := Pull(c, root, share, base, host, now, ig, guard)
		if err != nil {
			return base, pr, err
		}
		base = pr.Base
		push, err := Push(c, root, share, ig, guard, base)
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

func sameEntry(a, b manifest.Entry) bool {
	if a.Size != b.Size || a.Mode != b.Mode || len(a.Chunks) != len(b.Chunks) {
		return false
	}
	for i := range a.Chunks {
		if a.Chunks[i] != b.Chunks[i] {
			return false
		}
	}
	return true
}

func writeFileFromChunks(c *transport.Client, root string, e manifest.Entry) error {
	var buf []byte
	for _, h := range e.Chunks {
		b, err := c.GetBlob(h)
		if err != nil {
			return err
		}
		buf = append(buf, b...)
	}
	dst := filepath.Join(root, filepath.FromSlash(e.Path))
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	mode := os.FileMode(e.Mode)
	if mode == 0 {
		mode = 0o644
	}
	return atomicWrite(dst, buf, mode)
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
	cp := fmt.Sprintf("%s.conflict-%s-%d%s", stem, host, ts, ext)
	src := filepath.Join(root, filepath.FromSlash(relPath))
	dst := filepath.Join(root, filepath.FromSlash(cp))
	if err := os.Rename(src, dst); err != nil {
		return "", err
	}
	return cp, nil
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
