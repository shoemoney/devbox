package watch

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func waitSignal(t *testing.T, w *Watcher, within time.Duration) bool {
	t.Helper()
	select {
	case <-w.Events():
		return true
	case <-time.After(within):
		return false
	}
}

func TestWatchDetectsChange(t *testing.T) {
	root := t.TempDir()
	w, err := New(root, 50*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()

	if err := os.WriteFile(filepath.Join(root, "a.txt"), []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !waitSignal(t, w, 2*time.Second) {
		t.Fatal("no signal after file create")
	}
}

func TestWatchPicksUpNewSubdir(t *testing.T) {
	root := t.TempDir()
	w, err := New(root, 50*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()

	sub := filepath.Join(root, "sub")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	waitSignal(t, w, 2*time.Second) // drain the mkdir signal

	if err := os.WriteFile(filepath.Join(sub, "b.txt"), []byte("yo"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !waitSignal(t, w, 2*time.Second) {
		t.Fatal("no signal after writing into newly-created subdir")
	}
}

func TestWatchCoalesces(t *testing.T) {
	root := t.TempDir()
	w, err := New(root, 80*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()

	for i := 0; i < 10; i++ {
		_ = os.WriteFile(filepath.Join(root, "f.txt"), []byte{byte(i)}, 0o644)
		time.Sleep(5 * time.Millisecond)
	}
	if !waitSignal(t, w, 2*time.Second) {
		t.Fatal("expected at least one coalesced signal")
	}
	// After settling, the burst should not have produced a flood of signals.
	extra := 0
	for {
		select {
		case <-w.Events():
			extra++
			if extra > 3 {
				t.Fatal("too many signals; debounce not coalescing")
			}
		case <-time.After(300 * time.Millisecond):
			return
		}
	}
}
