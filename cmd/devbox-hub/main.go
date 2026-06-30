// Command devbox-hub is the hub server + admin CLI.
package main

import (
	"cmp"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"

	"github.com/shoemoney/devbox/internal/hub"
	"github.com/shoemoney/devbox/internal/hub/blobstore"
	"github.com/shoemoney/devbox/internal/hub/dashboard"
	"github.com/shoemoney/devbox/internal/hub/meta"
	"github.com/shoemoney/devbox/internal/manifest"
)

// isLoopback reports whether addr binds only to a loopback interface.
func isLoopback(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr
	}
	if host == "" || host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

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
		memberCmd(),
		principalCmd(),
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
	var data, listen, dashAddr, dashToken string
	var dash bool
	var gcEvery time.Duration
	var gcKeep int
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
			srv := hub.NewServer(db, store).SetVersion(version)
			out := cmd.OutOrStdout()

			// Optional live dashboard on its own address (localhost by default so it
			// never widens the API surface; it's unauthenticated read-only metrics).
			var d *dashboard.Dashboard
			if dash {
				d = dashboard.New(db, version)
				if dashToken != "" {
					d.SetToken(dashToken)
				}
				srv.WithDashboard(d)
				if dashToken != "" {
					fmt.Fprintf(out, "🔐 dashboard requires a token — open http://%s/?token=<token>\n", dashAddr)
				} else if !isLoopback(dashAddr) {
					fmt.Fprintf(out, "⚠️  dashboard on %s is UNAUTHENTICATED and not loopback — anyone who can reach it sees hub activity. Pass --dashboard-token, SSH-tunnel instead, or you accept this.\n", dashAddr)
				}
				go func() {
					ds := &http.Server{Addr: dashAddr, Handler: d.Handler(), ReadHeaderTimeout: 10 * time.Second}
					fmt.Fprintf(out, "📊 dashboard live at http://%s\n", dashAddr)
					if err := ds.ListenAndServe(); err != nil {
						fmt.Fprintf(out, "dashboard server stopped: %v\n", err)
					}
				}()
			}

			// Optional in-process periodic GC: the same mark-and-sweep as the
			// `gc` subcommand, run on a timer so the hub self-maintains — and, when
			// the dashboard is on, each sweep animates as a "gc" flow event. Off by
			// default (0): auto-deleting blobs on a timer is strictly opt-in.
			if gcEvery > 0 {
				go func() {
					t := time.NewTicker(gcEvery)
					defer t.Stop()
					for range t.C {
						snaps, chunks, err := runGC(db, store, gcKeep, false)
						if err != nil {
							fmt.Fprintf(out, "gc sweep error: %v\n", err)
							continue
						}
						if snaps > 0 || chunks > 0 {
							fmt.Fprintf(out, "🧹 gc: pruned %d snapshots, %d chunks\n", snaps, chunks)
						}
						d.Emit(dashboard.Event{Type: "gc", Pruned: snaps, Chunks: chunks})
					}
				}()
			}

			fmt.Fprintf(out, "🛰️  devbox-hub listening on %s (data: %s)\n", listen, data)
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
	cmd.Flags().BoolVar(&dash, "dashboard", false, "serve the live web dashboard")
	cmd.Flags().StringVar(&dashAddr, "dashboard-addr", "127.0.0.1:8099", "dashboard listen address (loopback by default — unauthenticated)")
	cmd.Flags().StringVar(&dashToken, "dashboard-token", "", "require this token to view the dashboard (recommended for non-loopback binds)")
	cmd.Flags().DurationVar(&gcEvery, "gc-every", 0, "run in-process GC on this interval (0 = off; e.g. 24h) — animates on the dashboard")
	cmd.Flags().IntVar(&gcKeep, "gc-keep", 10, "snapshots to keep per share when --gc-every sweeps")
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

// memberCmd manages per-share roles (M8a). The FIRST grant on a share flips it
// from legacy (every device an implicit owner = v1) to explicit/deny-by-default,
// so granting roles is how a share becomes multi-owner — handle with care.
func memberCmd() *cobra.Command {
	var data string
	cmd := &cobra.Command{Use: "member", Short: "👥 manage per-share roles (M8a)"}

	var reshare bool
	set := &cobra.Command{
		Use:   "set <share> <principal> <viewer|editor|admin|owner>",
		Short: "grant/update a principal's role on a share (flips the share to explicit ACLs)",
		Args:  cobra.ExactArgs(3),
		RunE: func(cmd *cobra.Command, args []string) error {
			role, ok := meta.ParseRole(args[2])
			if !ok {
				return fmt.Errorf("unknown role %q (want viewer|editor|admin|owner)", args[2])
			}
			db, err := openDB(data)
			if err != nil {
				return err
			}
			defer db.Close()
			if err := db.SetMember(args[0], args[1], role, reshare); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%s is now %s on %s (share now uses explicit ACLs)\n", args[1], args[2], args[0])
			return nil
		},
	}
	set.Flags().BoolVar(&reshare, "reshare", false, "allow this member to delegate (the +s bit)")

	rm := &cobra.Command{
		Use:   "rm <share> <principal>",
		Short: "revoke a principal's role on a share",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			db, err := openDB(data)
			if err != nil {
				return err
			}
			defer db.Close()
			if err := db.RemoveMember(args[0], args[1]); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "removed %s from %s\n", args[1], args[0])
			return nil
		},
	}

	list := &cobra.Command{
		Use:   "list <share>",
		Short: "list a share's role grants",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			db, err := openDB(data)
			if err != nil {
				return err
			}
			defer db.Close()
			ms, err := db.Members(args[0])
			if err != nil {
				return err
			}
			if len(ms) == 0 {
				fmt.Fprintf(cmd.OutOrStdout(), "%s is a legacy share (every device is an implicit owner)\n", args[0])
				return nil
			}
			for _, m := range ms {
				s := ""
				if m.CanReshare {
					s = " +s"
				}
				fmt.Fprintf(cmd.OutOrStdout(), "%-20s %s%s\n", m.Principal, meta.RoleName(m.Role), s)
			}
			return nil
		},
	}

	for _, c := range []*cobra.Command{set, rm, list} {
		c.Flags().StringVar(&data, "data", "./devbox-hub-data", "hub data directory")
	}
	cmd.AddCommand(set, rm, list)
	return cmd
}

// principalCmd assigns a device to a principal (M8a). Every v1 device belongs to
// the synthetic 'owner' principal; point a device at its own principal so per-share
// roles can distinguish people, not just devices.
func principalCmd() *cobra.Command {
	var data, name string
	cmd := &cobra.Command{
		Use:   "principal <device> <principal>",
		Short: "🪪 assign a device to a principal (creates the principal if new)",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			db, err := openDB(data)
			if err != nil {
				return err
			}
			defer db.Close()
			pname := cmp.Or(name, args[1])
			if err := db.EnsurePrincipal(args[1], pname, time.Now().Unix()); err != nil {
				return err
			}
			if err := db.SetDevicePrincipal(args[0], args[1]); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "device %s now belongs to principal %s\n", args[0], args[1])
			return nil
		},
	}
	cmd.Flags().StringVar(&data, "data", "./devbox-hub-data", "hub data directory")
	cmd.Flags().StringVar(&name, "name", "", "human name for the principal (defaults to its id)")
	return cmd
}

func gcCmd() *cobra.Command {
	var data string
	var keep int
	var dryRun bool
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
			snaps, chunks, err := runGC(db, store, keep, dryRun)
			if err != nil {
				return err
			}
			if dryRun {
				fmt.Fprintf(cmd.OutOrStdout(), "🔍 gc --dry-run: would remove %d snapshots, %d chunks (nothing deleted)\n", snaps, chunks)
			} else {
				fmt.Fprintf(cmd.OutOrStdout(), "🧹 gc: removed %d snapshots, %d chunks (freed)\n", snaps, chunks)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&data, "data", "./devbox-hub-data", "hub data directory")
	cmd.Flags().IntVar(&keep, "keep", 10, "snapshots to keep per share (newest)")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "show what would be pruned without deleting anything")
	return cmd
}

// runGC prunes deletable snapshots (keeping the head + `keep` newest per share)
// and sweeps unreachable chunks. It is mark-and-sweep from every share's live
// snapshots as roots: nothing reachable from a kept snapshot is ever deleted,
// regardless of refcounts. That makes GC safe even when refcounts are off — e.g.
// content-addressed snapshot ids are shared across shares, so a naive per-share
// refcount can undercount a chunk that two share heads both need.
func runGC(db *meta.DB, store blobstore.Store, keep int, dryRun bool) (snaps, chunks int, err error) {
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

	// --dry-run: report what WOULD be pruned without mutating. Deletable chunks are
	// those a pruned manifest references but no kept snapshot does, plus any
	// pre-existing orphan (refcount<=0) that's also unreachable — the same set the
	// real sweep removes, computed from reachability (refcount ground truth).
	if dryRun {
		del := map[string]bool{}
		for _, p := range prunable {
			if keptSnaps[p.id] {
				continue
			}
			snaps++
			hashes, err := manifestChunks(store, p.id)
			if err != nil && err != blobstore.ErrNotFound {
				return 0, 0, err
			}
			for _, h := range hashes {
				if !reachable[h] {
					del[h] = true
				}
			}
		}
		unref, err := db.UnreferencedChunks()
		if err != nil {
			return 0, 0, err
		}
		for _, h := range unref {
			if !reachable[h] {
				del[h] = true
			}
		}
		return snaps, len(del), nil
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
