// Package watch reports debounced "something changed" signals for a directory
// tree, using fsnotify with recursive (re-)subscription as subdirectories appear.
package watch

import (
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/fsnotify/fsnotify"
)

// Watcher emits a coalesced signal on Events() after the tree has been quiet for
// the debounce interval following one or more filesystem changes.
type Watcher struct {
	fsw      *fsnotify.Watcher
	events   chan struct{}
	done     chan struct{}
	debounce time.Duration
}

// New starts watching root (recursively) with the given debounce interval.
func New(root string, debounce time.Duration) (*Watcher, error) {
	fsw, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, err
	}
	w := &Watcher{
		fsw:      fsw,
		events:   make(chan struct{}, 1),
		done:     make(chan struct{}),
		debounce: debounce,
	}
	if err := w.addTree(root); err != nil {
		fsw.Close()
		return nil, err
	}
	go w.loop()
	return w, nil
}

// Events delivers a signal each time a debounced batch of changes settles.
func (w *Watcher) Events() <-chan struct{} { return w.events }

// Close stops the watcher.
func (w *Watcher) Close() error {
	close(w.done)
	return w.fsw.Close()
}

// addTree adds a watch on root and every directory beneath it. fsnotify watches
// a single directory level, so we subscribe each dir (and re-add new ones live).
func (w *Watcher) addTree(root string) error {
	return filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // ponytail: skip unreadable dirs rather than abort the whole add
		}
		if d.IsDir() {
			_ = w.fsw.Add(p)
		}
		return nil
	})
}

func (w *Watcher) loop() {
	var timer *time.Timer
	var timerC <-chan time.Time
	for {
		select {
		case <-w.done:
			return
		case ev, ok := <-w.fsw.Events:
			if !ok {
				return
			}
			if ev.Op&fsnotify.Create != 0 {
				if info, err := os.Stat(ev.Name); err == nil && info.IsDir() {
					_ = w.addTree(ev.Name) // watch newly-created subdirs
				}
			}
			if timer == nil {
				timer = time.NewTimer(w.debounce)
			} else {
				// Stop and drain any already-buffered tick before Reset, so a
				// tick that fired concurrently with this event can't leak through
				// and trigger a premature (pre-debounce) signal.
				if !timer.Stop() {
					select {
					case <-timer.C:
					default:
					}
				}
				timer.Reset(w.debounce)
			}
			timerC = timer.C
		case <-timerC:
			timerC = nil
			select {
			case w.events <- struct{}{}:
			default: // a pending signal is already queued; coalesce
			}
		case _, ok := <-w.fsw.Errors:
			if !ok {
				return
			}
		}
	}
}
