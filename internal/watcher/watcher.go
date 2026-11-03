// Package watcher observes a single directory for changes made by external
// programs.
//
// The primary implementation is a polling watcher, which works on every
// platform without external dependencies and guarantees eventual consistency
// by re-reading the real directory on a fixed interval. Every event is bound
// to the absolute path of the directory it was observed in so consumers can
// discard events belonging to directories the user has already left.
package watcher

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// Op describes a set of file operations.
type Op uint32

// Supported operations. The top bits carry watcher lifecycle information.
const (
	Create Op = 1 << iota
	Remove
	Rename
	Replace

	// WatchFailed indicates that watching could not be established.
	WatchFailed
	// WatchLost indicates that the watched directory disappeared or the
	// event stream was interrupted.
	WatchLost
	// Resync requests a full re-read of the directory. It is emitted when
	// the watcher recovered after being lost or when events may have been
	// dropped because the consumer was too slow.
	Resync
)

// Event represents a single filesystem observation. Every event is bound to
// Dir, the absolute path of the directory it belongs to.
type Event struct {
	// Dir is the absolute path of the directory this event belongs to.
	Dir string
	// Op is the operation that triggered the event.
	Op Op
	// Name is the name of the entry involved. Empty for lifecycle events.
	Name string
	// OldName is the previous name when Op is Rename.
	OldName string
	// Err carries the cause for WatchFailed / WatchLost events.
	Err error
}

// String implements fmt.Stringer.
func (e Event) String() string {
	switch {
	case e.Op == WatchFailed:
		return fmt.Sprintf("watch failed for %s: %v", e.Dir, e.Err)
	case e.Op == WatchLost:
		return fmt.Sprintf("watch lost for %s: %v", e.Dir, e.Err)
	case e.Op == Resync:
		return fmt.Sprintf("resync requested for %s", e.Dir)
	case e.Op == Rename:
		return fmt.Sprintf("%s: %q -> %q in %s", e.Op, e.OldName, e.Name, e.Dir)
	default:
		return fmt.Sprintf("%s: %q in %s", e.Op, e.Name, e.Dir)
	}
}

// String implements fmt.Stringer.
func (o Op) String() string {
	switch o {
	case Create:
		return "CREATE"
	case Remove:
		return "REMOVE"
	case Rename:
		return "RENAME"
	case Replace:
		return "REPLACE"
	case WatchFailed:
		return "WATCH_FAILED"
	case WatchLost:
		return "WATCH_LOST"
	case Resync:
		return "RESYNC"
	default:
		return fmt.Sprintf("Op(%d)", uint32(o))
	}
}

// Watcher watches exactly one directory at a time.
type Watcher interface {
	// Watch starts watching the provided directory, replacing any directory
	// previously watched. An error is returned when watching cannot be
	// established.
	Watch(dir string) error
	// Events returns the channel that observations are delivered on. The
	// channel is closed when Close is called.
	Events() <-chan Event
	// Close stops the watcher and releases its resources.
	Close() error
}

// Signature is the identity of a directory entry used to detect replacements.
type Signature struct {
	IsDir   bool
	Mode    os.FileMode
	Size    int64
	ModTime time.Time
}

func readSnapshot(dir string) (map[string]Signature, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}

	snapshot := make(map[string]Signature, len(entries))

	for _, entry := range entries {
		info, err := entry.Info()
		if err != nil {
			continue
		}

		snapshot[entry.Name()] = Signature{
			IsDir:   entry.IsDir(),
			Mode:    info.Mode(),
			Size:    info.Size(),
			ModTime: info.ModTime(),
		}
	}

	return snapshot, nil
}

// Diff compares two directory snapshots and returns the observed operations.
//
// Entries present in after but not in before are Create operations, entries in
// before but not in after are Remove operations and entries whose signature
// changed are Replace operations. When a poll captures exactly one removal and
// one creation of the same kind (file/directory) it is reported as a single
// Rename. Regardless of the reported operation, consumers are expected to
// re-read the real directory, which remains the source of truth.
//
// The returned events are deterministic: sorted by operation then name.
func Diff(before, after map[string]Signature) []Event {
	var (
		created []string
		removed []string
		changed []string
	)

	for name := range after {
		if _, ok := before[name]; !ok {
			created = append(created, name)
		}
	}

	for name, oldSig := range before {
		newSig, ok := after[name]
		switch {
		case !ok:
			removed = append(removed, name)
		case newSig != oldSig:
			changed = append(changed, name)
		}
	}

	sort.Strings(created)
	sort.Strings(removed)
	sort.Strings(changed)

	var events []Event

	// A single removal paired with a single creation carrying the exact same
	// signature is a rename: moving an entry preserves its size, mode and
	// modification time, while unrelated deletes and creates differ.
	if len(removed) == 1 && len(created) == 1 &&
		before[removed[0]] == after[created[0]] {
		events = append(events, Event{
			Op:      Rename,
			OldName: removed[0],
			Name:    created[0],
		})
	} else {
		for _, name := range removed {
			events = append(events, Event{Op: Remove, Name: name})
		}

		for _, name := range created {
			events = append(events, Event{Op: Create, Name: name})
		}
	}

	for _, name := range changed {
		events = append(events, Event{Op: Replace, Name: name})
	}

	return events
}

// eventBuffer is the number of observations that can be queued before a
// resynchronization is requested.
const eventBuffer = 256

// PollingWatcher detects external changes by comparing snapshots of a
// directory at a fixed interval. It is safe for concurrent use.
type PollingWatcher struct {
	interval time.Duration

	// mu guards every state change and every send on events, which makes
	// closing the channel race-free: Close and emit cannot interleave.
	mu     sync.Mutex
	dir    string
	closed bool
	stop   chan struct{}
	events chan Event
}

// NewPolling creates a polling watcher. The interval controls how often the
// real directory is re-read; a non-positive interval defaults to one second.
func NewPolling(interval time.Duration) *PollingWatcher {
	if interval <= 0 {
		interval = time.Second
	}

	return &PollingWatcher{
		interval: interval,
		stop:     make(chan struct{}),
		events:   make(chan Event, eventBuffer),
	}
}

// Events returns the event channel.
func (w *PollingWatcher) Events() <-chan Event {
	return w.events
}

// WatchedDir returns the currently watched directory or an empty string.
func (w *PollingWatcher) WatchedDir() string {
	w.mu.Lock()
	defer w.mu.Unlock()

	return w.dir
}

// ErrWatcherClosed is returned when Watch is called after Close.
var ErrWatcherClosed = errors.New("watcher is closed")

// Watch replaces the watched directory. The first poll establishes a
// baseline silently.
func (w *PollingWatcher) Watch(dir string) error {
	dir = filepath.Clean(dir)

	w.mu.Lock()
	if w.closed {
		w.mu.Unlock()
		return ErrWatcherClosed
	}

	if w.stop != nil {
		select {
		case <-w.stop:
		default:
			close(w.stop)
		}
	}

	w.dir = dir
	w.stop = make(chan struct{})
	stop := w.stop
	w.mu.Unlock()

	go w.poll(dir, stop)

	return nil
}

// Close stops the watcher and closes the event channel.
func (w *PollingWatcher) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()

	if w.closed {
		return nil
	}

	w.closed = true
	w.dir = ""

	if w.stop != nil {
		select {
		case <-w.stop:
		default:
			close(w.stop)
		}
	}

	close(w.events)

	return nil
}

// emit delivers an event without blocking while holding the mutex, so Close
// can never close the channel underneath an in-flight send. A full buffer
// causes the event to be dropped; the caller requests a resynchronization.
func (w *PollingWatcher) emit(ev Event, stop <-chan struct{}) (delivered bool) {
	w.mu.Lock()
	defer w.mu.Unlock()

	if w.closed {
		return false
	}

	// Check for cancellation first and do not let it race the send: a closed
	// stop channel and a ready event channel would otherwise both be selected
	// randomly and leak an event from the directory that is no longer watched.
	select {
	case <-stop:
		return false
	default:
	}

	select {
	case w.events <- ev:
		return true
	default:
		return false
	}
}

func (w *PollingWatcher) poll(dir string, stop <-chan struct{}) {
	ticker := time.NewTicker(w.interval)
	defer ticker.Stop()

	var (
		baseline    map[string]Signature
		lost        bool
		resyncAfter bool
	)

	scan := func() {
		snapshot, err := readSnapshot(dir)
		if err != nil {
			if !lost {
				lost = true
				baseline = nil

				w.emit(Event{Dir: dir, Op: WatchLost, Err: err}, stop)
			}

			return
		}

		if lost {
			// The directory came back; ask for a full re-read and rebuild
			// the baseline on the following poll.
			lost = false
			baseline = nil

			if !w.emit(Event{Dir: dir, Op: Resync}, stop) {
				resyncAfter = true
				return
			}
		}

		if baseline == nil {
			baseline = snapshot
			return
		}

		events := Diff(baseline, snapshot)
		allDelivered := true

		for _, ev := range events {
			ev.Dir = dir
			if !w.emit(ev, stop) {
				allDelivered = false
				break
			}
		}

		if !allDelivered || resyncAfter {
			resyncAfter = false
			w.emit(Event{Dir: dir, Op: Resync}, stop)
		}

		baseline = snapshot
	}

	// Establish the baseline immediately instead of waiting one interval.
	scan()

	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
			scan()
		}
	}
}

// ManualWatcher is a watcher controlled programmatically. It is intended for
// tests and for environments where real filesystem notifications are
// unavailable.
type ManualWatcher struct {
	mu     sync.Mutex
	dir    string
	closed bool
	events chan Event

	// WatchErr, when non-nil, is returned by Watch to simulate a watcher
	// that cannot be established.
	WatchErr error
}

// NewManual creates an empty manual watcher.
func NewManual() *ManualWatcher {
	return &ManualWatcher{events: make(chan Event, eventBuffer)}
}

// Events returns the event channel.
func (w *ManualWatcher) Events() <-chan Event {
	return w.events
}

// Watch records the watched directory.
func (w *ManualWatcher) Watch(dir string) error {
	w.mu.Lock()
	defer w.mu.Unlock()

	if w.closed {
		return ErrWatcherClosed
	}

	if w.WatchErr != nil {
		return w.WatchErr
	}

	w.dir = filepath.Clean(dir)

	return nil
}

// WatchedDir returns the currently watched directory.
func (w *ManualWatcher) WatchedDir() string {
	w.mu.Lock()
	defer w.mu.Unlock()

	return w.dir
}

// Emit queues an event for the currently watched directory.
func (w *ManualWatcher) Emit(ev Event) bool {
	w.mu.Lock()
	defer w.mu.Unlock()

	if w.closed {
		return false
	}

	if ev.Dir == "" {
		ev.Dir = w.dir
	}

	select {
	case w.events <- ev:
		return true
	default:
		return false
	}
}

// Close closes the event channel.
func (w *ManualWatcher) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()

	if w.closed {
		return nil
	}

	w.closed = true
	close(w.events)

	return nil
}
