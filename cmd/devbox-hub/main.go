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
			return http.ListenAndServe(listen, srv.Handler())
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
			raw := randHex(16)
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

func randHex(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return hex.EncodeToString(b)
}
