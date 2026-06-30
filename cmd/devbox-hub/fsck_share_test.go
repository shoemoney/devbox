package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/shoemoney/devbox/internal/chunk"
	"github.com/shoemoney/devbox/internal/hub/blobstore"
	"github.com/shoemoney/devbox/internal/hub/meta"
)

// TestFsckClean: all blobs hash correctly → exit 0, output says OK.
func TestFsckClean(t *testing.T) {
	data := t.TempDir()
	store, _ := blobstore.NewDisk(filepath.Join(data, "blobs"))
	blob := []byte("hello blob")
	h := chunk.Hash(blob)
	if err := store.Put(h, blob); err != nil {
		t.Fatal(err)
	}

	cmd := fsckCmd()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	if err := cmd.Flags().Set("data", data); err != nil {
		t.Fatal(err)
	}
	if err := cmd.RunE(cmd, nil); err != nil {
		t.Fatalf("expected no error for clean store, got: %v", err)
	}
	if !strings.Contains(buf.String(), "OK") {
		t.Fatalf("expected OK in output, got: %s", buf.String())
	}
}

// TestFsckDetectsCorruption: a file whose name is a hash but content is wrong → error, corruption reported.
func TestFsckDetectsCorruption(t *testing.T) {
	data := t.TempDir()
	store, _ := blobstore.NewDisk(filepath.Join(data, "blobs"))

	// Write a good blob so we have at least one clean entry.
	goodData := []byte("good blob content")
	goodHash := chunk.Hash(goodData)
	if err := store.Put(goodHash, goodData); err != nil {
		t.Fatal(err)
	}

	// Plant a corrupt blob: filename = hash of "correct", content = different bytes.
	correctData := []byte("correct content")
	corruptHash := chunk.Hash(correctData)
	shardDir := filepath.Join(data, "blobs", corruptHash[:2])
	if err := os.MkdirAll(shardDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(shardDir, corruptHash), []byte("wrong content!"), 0o644); err != nil {
		t.Fatal(err)
	}

	cmd := fsckCmd()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	if err := cmd.Flags().Set("data", data); err != nil {
		t.Fatal(err)
	}
	err := cmd.RunE(cmd, nil)
	if err == nil {
		t.Fatal("expected error for corrupt blob, got nil")
	}
	out := buf.String()
	if !strings.Contains(out, "CORRUPTION") {
		t.Fatalf("expected CORRUPTION in output, got: %s", out)
	}
	// Good blob must not be flagged.
	if strings.Contains(out, goodHash) {
		t.Fatalf("good blob wrongly flagged: %s", out)
	}
}

// TestFsckJSONOutput: --json emits structured result with the corrupt entry.
func TestFsckJSONOutput(t *testing.T) {
	data := t.TempDir()

	correctData := []byte("original")
	corruptHash := chunk.Hash(correctData)
	shardDir := filepath.Join(data, "blobs", corruptHash[:2])
	if err := os.MkdirAll(shardDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(shardDir, corruptHash), []byte("tampered"), 0o644); err != nil {
		t.Fatal(err)
	}

	cmd := fsckCmd()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	_ = cmd.Flags().Set("data", data)
	_ = cmd.Flags().Set("json", "true")
	_ = cmd.RunE(cmd, nil)

	var result struct {
		Scanned int `json:"scanned"`
		Corrupt []struct {
			Hash string `json:"hash"`
			Got  string `json:"got"`
		} `json:"corrupt"`
	}
	if err := json.Unmarshal(buf.Bytes(), &result); err != nil {
		t.Fatalf("invalid JSON: %v\noutput: %s", err, buf.String())
	}
	if result.Scanned != 1 {
		t.Fatalf("expected scanned=1, got %d", result.Scanned)
	}
	if len(result.Corrupt) != 1 || result.Corrupt[0].Hash != corruptHash {
		t.Fatalf("unexpected corrupt entries: %+v", result.Corrupt)
	}
}

// TestShareLs: after creating a share via meta directly, share ls shows it.
func TestShareLs(t *testing.T) {
	data := t.TempDir()
	db, err := meta.Open(filepath.Join(data, "devbox-hub.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := db.CreateShare("myproject", "owner", 1000); err != nil {
		t.Fatal(err)
	}
	db.Close()

	cmd := shareCmd()
	ls, _, err := cmd.Find([]string{"ls"})
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	ls.SetOut(&buf)
	_ = ls.Flags().Set("data", data)
	if err := ls.RunE(ls, nil); err != nil {
		t.Fatalf("share ls failed: %v", err)
	}
	if !strings.Contains(buf.String(), "myproject") {
		t.Fatalf("expected myproject in output, got: %s", buf.String())
	}
}

// TestShareLsJSON: --json emits valid JSON with the share name.
func TestShareLsJSON(t *testing.T) {
	data := t.TempDir()
	db, err := meta.Open(filepath.Join(data, "devbox-hub.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := db.CreateShare("jsonshare", "owner", 2000); err != nil {
		t.Fatal(err)
	}
	db.Close()

	cmd := shareCmd()
	ls, _, err := cmd.Find([]string{"ls"})
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	ls.SetOut(&buf)
	_ = ls.Flags().Set("data", data)
	_ = ls.Flags().Set("json", "true")
	if err := ls.RunE(ls, nil); err != nil {
		t.Fatalf("share ls --json failed: %v", err)
	}
	var out []struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(buf.Bytes(), &out); err != nil {
		t.Fatalf("invalid JSON: %v\noutput: %s", err, buf.String())
	}
	if len(out) != 1 || out[0].Name != "jsonshare" {
		t.Fatalf("expected jsonshare, got %+v", out)
	}
}
