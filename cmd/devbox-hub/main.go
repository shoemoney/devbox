// Command devbox-hub is the hub server + admin CLI.
package main

import (
	"cmp"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/shoemoney/devbox/internal/chunk"
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
		backupCmd(),
		deviceCmd(),
		fsckCmd(),
		shareCmd(),
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
	var data, listen, dashAddr, dashToken, metricsToken string
	var dash, accessLog bool
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
			srv := hub.NewServer(db, store).SetVersion(version).SetMetricsToken(metricsToken)
			out := cmd.OutOrStdout()

			// Optional live dashboard on its own address (localhost by default so it
			// never widens the API surface; it's unauthenticated read-only metrics).
			var d *dashboard.Dashboard
			var ds *http.Server // hoisted so graceful shutdown can reach it
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
				ds = &http.Server{Addr: dashAddr, Handler: d.Handler(), ReadHeaderTimeout: 10 * time.Second}
				go func() {
					fmt.Fprintf(out, "📊 dashboard live at http://%s\n", dashAddr)
					if err := ds.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
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
						release, lerr := dataLock(data) // serialize with `backup` so a sweep can't strand a backup mid-copy
						if lerr != nil {
							fmt.Fprintf(out, "gc sweep lock error: %v\n", lerr)
							continue
						}
						snaps, chunks, err := runGC(db, store, gcKeep, 0, false)
						release()
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

			dashState := "off"
			if dash {
				dashState = "on @" + dashAddr
			}
			fmt.Fprintf(out, "🛰️  devbox-hub %s listening on %s (data: %s · dashboard: %s)\n", version, listen, data, dashState)
			if !isLoopback(listen) {
				fmt.Fprintf(out, "⚠️  API on %s serves PLAIN HTTP — bearer tokens travel in cleartext. Put it behind a TLS proxy or accept the risk.\n", listen)
			}
			// Explicit timeouts bound the slowloris surface (bare ListenAndServe
			// has none). No WriteTimeout on purpose: the /v1/events SSE stream is
			// long-lived and a WriteTimeout would kill it mid-flight.
			handler := srv.Handler()
			if accessLog {
				handler = hub.AccessLogMiddleware(handler)
			}
			httpSrv := &http.Server{
				Addr:              listen,
				Handler:           handler,
				ReadHeaderTimeout: 10 * time.Second,
				ReadTimeout:       30 * time.Second,
				IdleTimeout:       120 * time.Second,
				MaxHeaderBytes:    1 << 20,
			}

			// On shutdown, drop SSE streams first — otherwise the drain waits the full
			// timeout, since every connected client daemon holds a long-lived /v1/events stream.
			httpSrv.RegisterOnShutdown(srv.CloseEventStreams)

			ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
			defer stop()

			srvErr := make(chan error, 1)
			go func() {
				if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
					srvErr <- err
				}
			}()

			select {
			case err := <-srvErr:
				return err
			case <-ctx.Done():
				fmt.Fprintf(out, "🛑 shutting down…\n")
				shutCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				if ds != nil {
					_ = ds.Shutdown(shutCtx)
				}
				return httpSrv.Shutdown(shutCtx)
			}
		},
	}
	cmd.Flags().StringVar(&data, "data", "./devbox-hub-data", "hub data directory")
	cmd.Flags().StringVar(&listen, "listen", ":8080", "listen address")
	cmd.Flags().BoolVar(&dash, "dashboard", false, "serve the live web dashboard")
	cmd.Flags().StringVar(&dashAddr, "dashboard-addr", "127.0.0.1:8099", "dashboard listen address (loopback by default — unauthenticated)")
	cmd.Flags().StringVar(&dashToken, "dashboard-token", "", "require this token to view the dashboard (recommended for non-loopback binds)")
	cmd.Flags().DurationVar(&gcEvery, "gc-every", 0, "run in-process GC on this interval (0 = off; e.g. 24h) — animates on the dashboard")
	cmd.Flags().IntVar(&gcKeep, "gc-keep", 10, "snapshots to keep per share when --gc-every sweeps")
	cmd.Flags().StringVar(&metricsToken, "metrics-token", "", "require this token to access /metrics (empty = open)")
	cmd.Flags().BoolVar(&accessLog, "access-log", false, "log one line per request to stderr (method, path, status, bytes, addr, duration)")
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
	var keep, keepDays int
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
			// A real sweep deletes blobs, so serialize with `backup` (dry-run is
			// read-only and must not block on a long backup, so it skips the lock).
			if !dryRun {
				release, lerr := dataLock(data)
				if lerr != nil {
					return lerr
				}
				defer release()
			}
			snaps, chunks, err := runGC(db, store, keep, keepDays, dryRun)
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
	cmd.Flags().IntVar(&keepDays, "keep-days", 0, "also keep snapshots newer than N days regardless of --keep (0 = disabled)")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "show what would be pruned without deleting anything")
	return cmd
}

// runGC prunes deletable snapshots (keeping the head + `keep` newest per share,
// and optionally any snapshot created within the last keepDays days) and sweeps
// unreachable chunks. keepDays=0 preserves the existing behaviour exactly.
// It is mark-and-sweep from every share's live snapshots as roots: nothing
// reachable from a kept snapshot is ever deleted, regardless of refcounts.
func runGC(db *meta.DB, store blobstore.Store, keep, keepDays int, dryRun bool) (snaps, chunks int, err error) {
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

	// Cutoff for --keep-days: snapshots created after this unix timestamp are kept
	// even when outside the --keep newest-N window. Zero means "disabled".
	var keepDaysCutoff int64
	if keepDays > 0 {
		keepDaysCutoff = time.Now().Add(-time.Duration(keepDays) * 24 * time.Hour).Unix()
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
		// Fetch timestamps once per share when --keep-days is active.
		var timestamps map[string]int64
		if keepDaysCutoff != 0 {
			timestamps, err = db.SnapshotTimestamps(share)
			if err != nil {
				return 0, 0, err
			}
		}
		ids, err := db.SnapshotIDs(share)
		if err != nil {
			return 0, 0, err
		}
		for _, id := range ids {
			if delSet[id] && !headSet[id] {
				// Keep if within the --keep-days window (timestamp >= cutoff).
				if keepDaysCutoff != 0 && timestamps[id] >= keepDaysCutoff {
					keptSnaps[id] = true
				} else {
					prunable = append(prunable, snapRef{share, id})
				}
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

	// Pre-enumerate chunk hashes for every distinct prunable id before any blob is
	// deleted. Content is content-addressed: the same id may appear as a prunable
	// entry in two different shares (both advanced past it). Reading the manifest
	// inline — after the first share's iteration has already deleted the blob —
	// returns ErrNotFound, so the second share's DeleteSnapshot gets nil hashes and
	// the shared chunks' refcounts are never decremented, causing a permanent leak.
	// Reading each id exactly once up-front avoids the ordering hazard entirely.
	// ponytail: single pass over distinct IDs; nil slice on ErrNotFound matches the old tolerated-error path.
	chunksByID := make(map[string][]string, len(prunable))
	for _, p := range prunable {
		if keptSnaps[p.id] {
			continue
		}
		if _, seen := chunksByID[p.id]; seen {
			continue
		}
		hashes, err := manifestChunks(store, p.id)
		if err != nil && err != blobstore.ErrNotFound {
			return 0, 0, err
		}
		chunksByID[p.id] = hashes // nil on ErrNotFound is fine; DeleteSnapshot handles it
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
		hashes := chunksByID[p.id]
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

func backupCmd() *cobra.Command {
	var data string
	cmd := &cobra.Command{
		Use:   "backup <dir>",
		Short: "💾 snapshot the DB + blobs to a directory",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			destDir := args[0]
			if err := os.MkdirAll(destDir, 0o700); err != nil {
				return err
			}
			db, err := openDB(data)
			if err != nil {
				return err
			}
			defer db.Close()
			// Hold the data lock across BOTH the DB snapshot and the blob copy so a
			// concurrent gc sweep can't delete a blob the snapshot references mid-copy,
			// leaving an internally-inconsistent backup (a dangling ref you'd only
			// discover on restore). DB-first then blobs is the correct order.
			release, lerr := dataLock(data)
			if lerr != nil {
				return lerr
			}
			defer release()
			if err := db.BackupTo(filepath.Join(destDir, "devbox-hub.db")); err != nil {
				return err
			}
			// ponytail: blobs are a plain recursive copy; rsync/incremental is the upgrade path
			srcBlobs := filepath.Join(data, "blobs")
			dstBlobs := filepath.Join(destDir, "blobs")
			var filesCopied, bytesCopied int64
			walkErr := filepath.WalkDir(srcBlobs, func(path string, d fs.DirEntry, err error) error {
				if err != nil {
					return err
				}
				rel, _ := filepath.Rel(srcBlobs, path)
				dst := filepath.Join(dstBlobs, rel)
				if d.IsDir() {
					return os.MkdirAll(dst, 0o700)
				}
				in, err := os.Open(path)
				if err != nil {
					return err
				}
				defer in.Close()
				out, err := os.Create(dst)
				if err != nil {
					return err
				}
				defer out.Close()
				n, err := io.Copy(out, in)
				if err != nil {
					return err
				}
				filesCopied++
				bytesCopied += n
				return nil
			})
			if walkErr != nil && !errors.Is(walkErr, os.ErrNotExist) {
				return walkErr
			}
			fmt.Fprintf(cmd.OutOrStdout(), "✅ backup: db + %d blobs (%d bytes) → %s\n", filesCopied, bytesCopied, destDir)
			return nil
		},
	}
	cmd.Flags().StringVar(&data, "data", "./devbox-hub-data", "hub data directory")
	return cmd
}

// danglingEntry is a snapshot that references at least one missing blob.
type danglingEntry struct {
	Share   string   `json:"share"`
	ID      string   `json:"id"`
	Missing []string `json:"missing"`
}

// checkDangling enumerates every snapshot in db and returns those that reference
// a blob that does not exist in store (missing manifest or missing chunk).
func checkDangling(db *meta.DB, store blobstore.Store) ([]danglingEntry, error) {
	snaps, err := db.AllSnapshots()
	if err != nil {
		return nil, err
	}
	var out []danglingEntry
	for _, snap := range snaps {
		var missing []string
		if ok, _ := store.Has(snap.ID); !ok {
			missing = append(missing, snap.ID) // manifest blob itself is gone
		} else {
			chunks, err := manifestChunks(store, snap.ID)
			if err != nil {
				if errors.Is(err, blobstore.ErrNotFound) {
					missing = append(missing, snap.ID)
				} else {
					return nil, err
				}
			} else {
				for _, h := range chunks {
					if ok, _ := store.Has(h); !ok {
						missing = append(missing, h)
					}
				}
			}
		}
		if len(missing) > 0 {
			out = append(out, danglingEntry{snap.Share, snap.ID, missing})
		}
	}
	return out, nil
}

// fsckCmd re-hashes every blob on disk to detect at-rest corruption, then
// checks every snapshot for dangling references (manifest or chunk blobs that
// no longer exist in the store).
// ponytail: re-hashes every blob (full read); fine for periodic DR checks, sample/mtime-skip if it ever gets too slow on a huge store.
func fsckCmd() *cobra.Command {
	var data string
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "fsck",
		Short: "🔍 at-rest blob integrity scan (re-hash every blob + dangling snapshot check)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			store, err := blobstore.NewDisk(filepath.Join(data, "blobs"))
			if err != nil {
				return err
			}
			type corruptEntry struct {
				Hash string `json:"hash"`
				Got  string `json:"got"`
			}
			var corrupt []corruptEntry
			var scanned int
			walkErr := store.Walk(func(hash string) error {
				scanned++
				b, err := store.Get(hash)
				if err != nil {
					if !asJSON {
						fmt.Fprintf(cmd.OutOrStdout(), "read error %s: %v\n", hash, err)
					}
					corrupt = append(corrupt, corruptEntry{hash, "read-error"})
					return nil
				}
				got := chunk.Hash(b)
				if got != hash {
					if !asJSON {
						fmt.Fprintf(cmd.OutOrStdout(), "CORRUPTION %s: got %s\n", hash, got)
					}
					corrupt = append(corrupt, corruptEntry{hash, got})
				}
				return nil
			})
			if walkErr != nil {
				return walkErr
			}

			db, err := openDB(data)
			if err != nil {
				return err
			}
			defer db.Close()
			dangling, err := checkDangling(db, store)
			if err != nil {
				return err
			}
			if !asJSON {
				for _, d := range dangling {
					shortID := d.ID
					if len(shortID) > 12 {
						shortID = shortID[:12]
					}
					extra := len(d.Missing) - 1
					if extra > 0 {
						fmt.Fprintf(cmd.OutOrStdout(), "⚠️  DANGLING snapshot %s/%s: missing %s (+%d more)\n", d.Share, shortID, d.Missing[0], extra)
					} else {
						fmt.Fprintf(cmd.OutOrStdout(), "⚠️  DANGLING snapshot %s/%s: missing %s\n", d.Share, shortID, d.Missing[0])
					}
				}
			}

			if asJSON {
				type result struct {
					Scanned  int             `json:"scanned"`
					Corrupt  []corruptEntry  `json:"corrupt"`
					Dangling []danglingEntry `json:"dangling"`
				}
				enc := json.NewEncoder(cmd.OutOrStdout())
				enc.SetIndent("", "  ")
				if err := enc.Encode(result{scanned, corrupt, dangling}); err != nil {
					return err
				}
			} else if len(corrupt) == 0 && len(dangling) == 0 {
				fmt.Fprintf(cmd.OutOrStdout(), "✅ fsck: %d blobs OK\n", scanned)
			} else {
				if len(corrupt) > 0 {
					fmt.Fprintf(cmd.OutOrStdout(), "❌ fsck: %d/%d blobs CORRUPT\n", len(corrupt), scanned)
				}
				if len(dangling) > 0 {
					fmt.Fprintf(cmd.OutOrStdout(), "❌ fsck: %d dangling snapshot(s)\n", len(dangling))
				}
			}
			if len(corrupt) > 0 || len(dangling) > 0 {
				return fmt.Errorf("fsck: %d corrupt blob(s), %d dangling snapshot(s)", len(corrupt), len(dangling))
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&data, "data", "./devbox-hub-data", "hub data directory")
	cmd.Flags().BoolVar(&asJSON, "json", false, "output as JSON")
	return cmd
}

func shareCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "share", Short: "🗂️  inspect shares"}
	var data string
	var asJSON bool
	ls := &cobra.Command{
		Use:   "ls",
		Short: "list shares",
		RunE: func(cmd *cobra.Command, _ []string) error {
			db, err := openDB(data)
			if err != nil {
				return err
			}
			defer db.Close()
			stats, err := db.ShareStats()
			if err != nil {
				return err
			}
			if asJSON {
				type shareView struct {
					Name      string `json:"name"`
					Head      string `json:"head"`
					ACLMode   string `json:"acl_mode"`
					Snapshots int    `json:"snapshots"`
					Members   int    `json:"members"`
					UpdatedAt int64  `json:"updated_at"`
				}
				out := make([]shareView, len(stats))
				for i, s := range stats {
					out[i] = shareView{s.Name, s.Head, s.ACLMode, s.Snapshots, s.Members, s.UpdatedAt}
				}
				enc := json.NewEncoder(cmd.OutOrStdout())
				enc.SetIndent("", "  ")
				return enc.Encode(out)
			}
			w := cmd.OutOrStdout()
			for _, s := range stats {
				head := s.Head
				if len(head) > 12 {
					head = head[:12]
				}
				updatedAt := "never"
				if s.UpdatedAt > 0 {
					updatedAt = time.Unix(s.UpdatedAt, 0).UTC().Format(time.RFC3339)
				}
				fmt.Fprintf(w, "%-30s %-12s %-8s snaps=%-4d members=%-3d %s\n",
					s.Name, head, s.ACLMode, s.Snapshots, s.Members, updatedAt)
			}
			return nil
		},
	}
	ls.Flags().StringVar(&data, "data", "./devbox-hub-data", "hub data directory")
	ls.Flags().BoolVar(&asJSON, "json", false, "output as JSON")
	cmd.AddCommand(ls)
	return cmd
}

func deviceCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "device", Short: "🖥️  manage devices"}
	var data string
	var asJSON bool
	ls := &cobra.Command{
		Use:   "ls",
		Short: "list enrolled devices",
		RunE: func(cmd *cobra.Command, _ []string) error {
			db, err := openDB(data)
			if err != nil {
				return err
			}
			defer db.Close()
			devices, err := db.Devices()
			if err != nil {
				return err
			}
			if asJSON {
				type deviceView struct {
					ID        string `json:"id"`
					Name      string `json:"name"`
					Principal string `json:"principal"`
					LastSeen  int64  `json:"last_seen"`
					Revoked   bool   `json:"revoked"`
				}
				out := make([]deviceView, len(devices))
				for i, d := range devices {
					out[i] = deviceView{d.ID, d.Name, d.Principal, d.LastSeen, d.Revoked}
				}
				enc := json.NewEncoder(cmd.OutOrStdout())
				enc.SetIndent("", "  ")
				return enc.Encode(out)
			}
			w := cmd.OutOrStdout()
			for _, d := range devices {
				id := d.ID
				if len(id) > 12 {
					id = id[:12]
				}
				rev := ""
				if d.Revoked {
					rev = " [revoked]"
				}
				lastSeen := "never"
				if d.LastSeen > 0 {
					lastSeen = time.Unix(d.LastSeen, 0).UTC().Format(time.RFC3339)
				}
				fmt.Fprintf(w, "%-12s %-20s %-20s %s%s\n", id, d.Name, d.Principal, lastSeen, rev)
			}
			return nil
		},
	}
	ls.Flags().StringVar(&data, "data", "./devbox-hub-data", "hub data directory")
	ls.Flags().BoolVar(&asJSON, "json", false, "output as JSON")
	cmd.AddCommand(ls)
	return cmd
}
