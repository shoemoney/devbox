// Command devbox-hub is the hub server + admin CLI.
package main

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"

	"git.shoemoney.ai/shoemoney/devbox/internal/hub"
	"git.shoemoney.ai/shoemoney/devbox/internal/hub/blobstore"
	"git.shoemoney.ai/shoemoney/devbox/internal/hub/meta"
	"git.shoemoney.ai/shoemoney/devbox/internal/manifest"
)

var version = "0.0.0-dev"

func main() {
	root := &cobra.Command{
		Use:           "devbox-hub",
		Short:         "🛰️  devbox hub server",
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.AddCommand(
		&cobra.Command{
			Use:   "version",
			Short: "print version",
			Run:   func(c *cobra.Command, _ []string) { fmt.Fprintln(c.OutOrStdout(), "devbox-hub", version) },
		},
		serveCmd(),
		tokenCmd(),
		readonlyCmd(),
		revokeCmd(),
		gcCmd(),
	)
	if err := root.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

// openDB opens the hub metadata DB under data/.
func openDB(data string) (*meta.DB, error) {
	if err := os.MkdirAll(data, 0o755); err != nil {
		return nil, err
	}
	return meta.Open(filepath.Join(data, "devbox-hub.db"))
}

func serveCmd() *cobra.Command {
	var data, listen string
	cmd := &cobra.Command{
		Use:   "serve",
		Short: "🚀 run the hub server",
		RunE: func(cmd *cobra.Command, _ []string) error {
			db, err := openDB(data)
			if err != nil {
				return err
			}
			store, err := blobstore.NewDisk(filepath.Join(data, "blobs"))
			if err != nil {
				return err
			}
			srv := hub.NewServer(db, store)
			fmt.Fprintf(cmd.OutOrStdout(), "🛰️  devbox-hub listening on %s (data: %s)\n", listen, data)
			// Explicit timeouts bound the slowloris surface (bare ListenAndServe
			// has none). No WriteTimeout on purpose: the /v1/events SSE stream is
			// long-lived and a WriteTimeout would kill it mid-flight.
			httpSrv := &http.Server{
				Addr:              listen,
				Handler:           srv.Handler(),
				ReadHeaderTimeout: 10 * time.Second,
				ReadTimeout:       30 * time.Second,
				IdleTimeout:       120 * time.Second,
				MaxHeaderBytes:    1 << 20,
			}
			return httpSrv.ListenAndServe()
		},
	}
	cmd.Flags().StringVar(&data, "data", "./devbox-hub-data", "hub data directory")
	cmd.Flags().StringVar(&listen, "listen", ":8080", "listen address")
	return cmd
}

func tokenCmd() *cobra.Command {
	var data string
	var ttl time.Duration
	cmd := &cobra.Command{
		Use:   "token",
		Short: "🎟️  mint a one-time join token",
		RunE: func(cmd *cobra.Command, _ []string) error {
			db, err := openDB(data)
			if err != nil {
				return err
			}
			defer db.Close()
			raw, err := randHex(16)
			if err != nil {
				return err
			}
			if err := db.CreateToken(hub.HashToken(raw), time.Now().Add(ttl).Unix()); err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), raw)
			return nil
		},
	}
	cmd.Flags().StringVar(&data, "data", "./devbox-hub-data", "hub data directory")
	cmd.Flags().DurationVar(&ttl, "ttl", time.Hour, "token validity window")
	return cmd
}

func readonlyCmd() *cobra.Command {
	var data string
	var rw bool
	cmd := &cobra.Command{
		Use:   "readonly <device> <share>",
		Short: "🔒 make a device read-only on a share (--rw to restore)",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			db, err := openDB(data)
			if err != nil {
				return err
			}
			defer db.Close()
			if err := db.SetWritable(args[0], args[1], rw); err != nil {
				return err
			}
			mode := "read-only"
			if rw {
				mode = "read-write"
			}
			fmt.Fprintf(cmd.OutOrStdout(), "device %s is now %s on %s\n", args[0], mode, args[1])
			return nil
		},
	}
	cmd.Flags().StringVar(&data, "data", "./devbox-hub-data", "hub data directory")
	cmd.Flags().BoolVar(&rw, "rw", false, "restore read-write instead")
	return cmd
}

func revokeCmd() *cobra.Command {
	var data string
	cmd := &cobra.Command{
		Use:   "revoke <device>",
		Short: "❌ revoke a device (its bearer dies immediately)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			db, err := openDB(data)
			if err != nil {
				return err
			}
			defer db.Close()
			if err := db.Revoke(args[0]); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "revoked %s\n", args[0])
			return nil
		},
	}
	cmd.Flags().StringVar(&data, "data", "./devbox-hub-data", "hub data directory")
	return cmd
}

func gcCmd() *cobra.Command {
	var data string
	var keep int
	cmd := &cobra.Command{
		Use:   "gc",
		Short: "🧹 prune old snapshots and unreferenced chunks",
		RunE: func(cmd *cobra.Command, _ []string) error {
			db, err := openDB(data)
			if err != nil {
				return err
			}
			defer db.Close()
			store, err := blobstore.NewDisk(filepath.Join(data, "blobs"))
			if err != nil {
				return err
			}
			snaps, chunks, err := runGC(db, store, keep)
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "🧹 gc: removed %d snapshots, %d chunks (freed)\n", snaps, chunks)
			return nil
		},
	}
	cmd.Flags().StringVar(&data, "data", "./devbox-hub-data", "hub data directory")
	cmd.Flags().IntVar(&keep, "keep", 10, "snapshots to keep per share (newest)")
	return cmd
}

// runGC prunes deletable snapshots (keeping the head + `keep` newest per share)
// and sweeps unreachable chunks. It is mark-and-sweep from every share's live
// snapshots as roots: nothing reachable from a kept snapshot is ever deleted,
// regardless of refcounts. That makes GC safe even when refcounts are off — e.g.
// content-addressed snapshot ids are shared across shares, so a naive per-share
// refcount can undercount a chunk that two share heads both need.
func runGC(db *meta.DB, store blobstore.Store, keep int) (snaps, chunks int, err error) {
	shares, err := db.ShareNames()
	if err != nil {
		return 0, 0, err
	}
	heads, err := db.Heads()
	if err != nil {
		return 0, 0, err
	}
	headSet := map[string]bool{}
	for _, h := range heads {
		headSet[h] = true
	}

	// Partition snapshots into kept vs prunable. Snapshots are now per-(share, id),
	// so a prunable entry is a (share, id) pair: it's deletable for its share AND
	// not the head of ANY share (a shared content id may be one share's stale
	// snapshot but another's live head — never prune). keptSnaps tracks ids kept by
	// any share, for reachability and to protect a shared manifest blob.
	keptSnaps := map[string]bool{}
	type snapRef struct{ share, id string }
	var prunable []snapRef
	for _, share := range shares {
		del, err := db.DeletableSnapshots(share, keep)
		if err != nil {
			return 0, 0, err
		}
		delSet := map[string]bool{}
		for _, id := range del {
			delSet[id] = true
		}
		ids, err := db.SnapshotIDs(share)
		if err != nil {
			return 0, 0, err
		}
		for _, id := range ids {
			if delSet[id] && !headSet[id] {
				prunable = append(prunable, snapRef{share, id})
			} else {
				keptSnaps[id] = true
			}
		}
	}

	// Ground truth: everything reachable from a kept snapshot survives. Reach a
	// missing manifest? Nothing is reachable through it — skip, don't abort.
	reachable := map[string]bool{}
	for id := range keptSnaps {
		reachable[id] = true // the manifest blob itself is live
		hashes, err := manifestChunks(store, id)
		if err == blobstore.ErrNotFound {
			continue
		}
		if err != nil {
			return 0, 0, err
		}
		for _, h := range hashes {
			reachable[h] = true
		}
	}

	// Prune each (share, id): delete that share's row and decrement its chunks. The
	// same content id in another share keeps its own row and counts. Delete the
	// shared manifest blob at most once, and only when it's unreachable from any
	// kept snapshot (reachable already includes every kept id, so this also won't
	// remove a manifest blob that doubles as a live chunk).
	blobDeleted := map[string]bool{}
	for _, p := range prunable {
		if keptSnaps[p.id] {
			continue // a live/kept snapshot in another share still needs this content
		}
		hashes, err := manifestChunks(store, p.id)
		if err != nil && err != blobstore.ErrNotFound {
			return 0, 0, err
		}
		if err := db.DeleteSnapshot(p.share, p.id, hashes); err != nil {
			return 0, 0, err
		}
		if !reachable[p.id] && !blobDeleted[p.id] {
			blobDeleted[p.id] = true
			if err := store.Delete(p.id); err != nil && err != blobstore.ErrNotFound {
				return 0, 0, err
			}
		}
		snaps++
	}

	// Sweep chunks: refcount==0 candidates, but only delete those NOT reachable
	// from any kept snapshot. The reachability check is the safety net.
	unref, err := db.UnreferencedChunks()
	if err != nil {
		return 0, 0, err
	}
	for _, h := range unref {
		if reachable[h] {
			continue // a live head needs it despite refcount — never delete
		}
		deleted, err := db.DeleteChunkRow(h) // conditional on refcount<=0
		if err != nil {
			return 0, 0, err
		}
		if deleted {
			if err := store.Delete(h); err != nil && err != blobstore.ErrNotFound {
				return 0, 0, err
			}
			chunks++
		}
	}
	return snaps, chunks, nil
}

// manifestChunks returns the distinct chunk hashes referenced by the snapshot
// whose id is its manifest blob key.
func manifestChunks(store blobstore.Store, snapshotID string) ([]string, error) {
	b, err := store.Get(snapshotID)
	if err != nil {
		return nil, err
	}
	m, err := manifest.Unmarshal(b)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var hashes []string
	for _, e := range m.Entries {
		for _, h := range e.Chunks {
			if !seen[h] {
				seen[h] = true
				hashes = append(hashes, h)
			}
		}
	}
	return hashes, nil
}

func randHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
