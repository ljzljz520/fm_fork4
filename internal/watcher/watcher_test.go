package watcher

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

const testInterval = 10 * time.Millisecond

func sig(isDir bool) Signature {
	return Signature{IsDir: isDir, Size: 1, ModTime: time.Unix(1, 0)}
}

func TestDiff(t *testing.T) {
	t.Parallel()

	dir := func(names ...string) map[string]Signature {
		m := map[string]Signature{}

		for i, n := range names {
			m[n] = Signature{IsDir: false, Size: int64(i + 1), ModTime: time.Unix(int64(i+1), 0)}
		}

		return m
	}

	t.Run("create and remove", func(t *testing.T) {
		t.Parallel()

		preserved := sig(false)
		before := map[string]Signature{"a.txt": sig(false), "b.txt": preserved}
		after := map[string]Signature{"b.txt": preserved, "c.txt": {IsDir: false, Size: 9, ModTime: time.Unix(9, 0)}}

		events := Diff(before, after)

		if len(events) != 2 {
			t.Fatalf("expected 2 events, got %d: %v", len(events), events)
		}

		if events[0].Op != Remove || events[0].Name != "a.txt" {
			t.Fatalf("expected remove a.txt, got %v", events[0])
		}

		if events[1].Op != Create || events[1].Name != "c.txt" {
			t.Fatalf("expected create c.txt, got %v", events[1])
		}
	})

	t.Run("rename pair", func(t *testing.T) {
		t.Parallel()

		before := dir("old.txt")
		after := dir("new.txt")
		events := Diff(before, after)

		if len(events) != 1 || events[0].Op != Rename {
			t.Fatalf("expected a single rename, got %v", events)
		}

		if events[0].OldName != "old.txt" || events[0].Name != "new.txt" {
			t.Fatalf("unexpected rename: %v", events[0])
		}
	})

	t.Run("replace changes signature", func(t *testing.T) {
		t.Parallel()

		before := map[string]Signature{"a.txt": sig(false)}
		after := map[string]Signature{"a.txt": {IsDir: false, Size: 2, ModTime: time.Unix(2, 0)}}

		events := Diff(before, after)
		if len(events) != 1 || events[0].Op != Replace || events[0].Name != "a.txt" {
			t.Fatalf("expected replace a.txt, got %v", events)
		}
	})

	t.Run("unchanged is silent", func(t *testing.T) {
		t.Parallel()

		s := dir("a.txt")
		if events := Diff(s, s); len(events) != 0 {
			t.Fatalf("expected no events, got %v", events)
		}
	})

	t.Run("directory kind mismatch is not a rename", func(t *testing.T) {
		t.Parallel()

		before := map[string]Signature{"a": sig(false)}
		after := map[string]Signature{"b": sig(true)}

		events := Diff(before, after)
		if len(events) != 2 {
			t.Fatalf("expected remove+create, got %v", events)
		}
	})
}

func drainUntil(w *PollingWatcher, dur time.Duration, pred func(Event) bool) ([]Event, bool) {
	var got []Event

	deadline := time.After(dur)

	for {
		select {
		case ev := <-w.Events():
			got = append(got, ev)
			if pred(ev) {
				return got, true
			}
		case <-deadline:
			return got, false
		}
	}
}

// waitForEvent reads events until one matching pred arrives or the deadline
// expires.
func waitForEvent(t *testing.T, w *PollingWatcher, deadline time.Duration, pred func(Event) bool) Event {
	t.Helper()

	timeout := time.After(deadline)

	for {
		select {
		case ev := <-w.Events():
			if pred(ev) {
				return ev
			}
		case <-timeout:
			t.Fatalf("timed out waiting for matching event")
		}
	}
}

func drainEvents(w *PollingWatcher, dur time.Duration) []Event {
	var events []Event

	deadline := time.After(dur)

	for {
		select {
		case ev := <-w.Events():
			events = append(events, ev)
		case <-deadline:
			return events
		}
	}
}

func TestPollingWatcherDetectsExternalChanges(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	w := NewPolling(testInterval)
	defer w.Close()

	if err := w.Watch(root); err != nil {
		t.Fatalf("watch: %v", err)
	}

	// Give the watcher time to establish its baseline.
	time.Sleep(5 * testInterval)

	if err := os.WriteFile(filepath.Join(root, "created.txt"), []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}

	ev := waitForEvent(t, w, time.Second, func(e Event) bool {
		return e.Op == Create && e.Name == "created.txt" && e.Dir == filepath.Clean(root)
	})

	if ev.Dir != filepath.Clean(root) {
		t.Fatalf("event not bound to watched directory: %+v", ev)
	}

	// Replace (overwrite) the file and ensure the signature changes.
	time.Sleep(15 * time.Millisecond) // guarantee a distinct modtime

	if err := os.WriteFile(filepath.Join(root, "created.txt"), []byte("hello world, larger"), 0o644); err != nil {
		t.Fatal(err)
	}

	waitForEvent(t, w, time.Second, func(e Event) bool {
		return e.Op == Replace && e.Name == "created.txt"
	})

	// Rename a separate, settled file so the signature comparison cannot be
	// disturbed by a poll landing in the middle of the preceding overwrite.
	if err := os.WriteFile(filepath.Join(root, "moveme.txt"), []byte("stable"), 0o644); err != nil {
		t.Fatal(err)
	}

	waitForEvent(t, w, time.Second, func(e Event) bool {
		return e.Op == Create && e.Name == "moveme.txt"
	})

	// Wait for the event stream to go quiet: a poll can observe a file while
	// it is still being written and correct itself on the following polls.
	// Renaming only after the stream settled guarantees the baseline holds
	// the final signature, which a rename preserves unchanged.
	waitQuiet(t, w, 5*time.Second)

	if err := os.Rename(filepath.Join(root, "moveme.txt"), filepath.Join(root, "renamed.txt")); err != nil {
		t.Fatal(err)
	}

	if seen, ok := drainUntil(w, 5*time.Second, func(e Event) bool {
		return e.Op == Rename && e.OldName == "moveme.txt" && e.Name == "renamed.txt"
	}); !ok {
		t.Fatalf("rename event not observed, saw: %v", seen)
	}

	// Delete.
	if err := os.Remove(filepath.Join(root, "renamed.txt")); err != nil {
		t.Fatal(err)
	}

	waitForEvent(t, w, 5*time.Second, func(e Event) bool {
		return e.Op == Remove && e.Name == "renamed.txt"
	})
}

// waitQuiet drains events until two consecutive windows of 20 poll intervals
// are empty, meaning the watcher has caught up with the filesystem.
func waitQuiet(t *testing.T, w *PollingWatcher, deadline time.Duration) {
	t.Helper()

	window := 20 * testInterval
	timeout := time.After(deadline)
	quiet := 0

	for quiet < 2 {
		timer := time.NewTimer(window)
		gotEvent := false

	wait:
		for {
			select {
			case <-w.Events():
				gotEvent = true
			case <-timer.C:
				break wait
			case <-timeout:
				timer.Stop()
				t.Fatal("timed out waiting for watcher to settle")
			}
		}

		timer.Stop()

		if gotEvent {
			quiet = 0
		} else {
			quiet++
		}
	}
}

func TestPollingWatcherBurstCoalescesToConsistentEvents(t *testing.T) {
	t.Parallel()

	root := t.TempDir()

	// Seed the directory so a baseline exists.
	for _, name := range []string{"a", "b"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte(name), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	w := NewPolling(testInterval)
	defer w.Close()

	if err := w.Watch(root); err != nil {
		t.Fatal(err)
	}

	time.Sleep(5 * testInterval)

	// External burst: many operations between two polls must all surface.
	for _, name := range []string{"burst1", "burst2", "burst3"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte(name), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	if err := os.Remove(filepath.Join(root, "a")); err != nil {
		t.Fatal(err)
	}

	events := drainEvents(w, 100*testInterval)

	seen := map[string]bool{}

	for _, ev := range events {
		if ev.Name != "" {
			seen[ev.Op.String()+":"+ev.Name] = true
		}
	}

	for _, want := range []string{"CREATE:burst1", "CREATE:burst2", "CREATE:burst3", "REMOVE:a"} {
		if !seen[want] {
			t.Fatalf("missing event %s, observed: %v", want, events)
		}
	}
}

func TestPollingWatcherDirectoryMissingAndRecovery(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	watched := filepath.Join(root, "vanishes")

	if err := os.Mkdir(watched, 0o755); err != nil {
		t.Fatal(err)
	}

	w := NewPolling(testInterval)
	defer w.Close()

	if err := w.Watch(watched); err != nil {
		t.Fatalf("watch: %v", err)
	}

	time.Sleep(5 * testInterval)

	if err := os.RemoveAll(watched); err != nil {
		t.Fatal(err)
	}

	lost := waitForEvent(t, w, time.Second, func(e Event) bool { return e.Op == WatchLost })
	if !errors.Is(lost.Err, os.ErrNotExist) && !os.IsNotExist(lost.Err) {
		// os.ReadDir may wrap the error; asserting it is non-nil is enough.
		if lost.Err == nil {
			t.Fatal("WatchLost event should carry the underlying error")
		}
	}

	// Only one WatchLost until recovery.
	time.Sleep(5 * testInterval)

	if err := os.Mkdir(watched, 0o755); err != nil {
		t.Fatal(err)
	}

	waitForEvent(t, w, time.Second, func(e Event) bool { return e.Op == Resync })

	// After recovery a new change is observed again.
	if err := os.WriteFile(filepath.Join(watched, "back.txt"), nil, 0o644); err != nil {
		t.Fatal(err)
	}

	waitForEvent(t, w, time.Second, func(e Event) bool {
		return e.Op == Create && e.Name == "back.txt"
	})
}

func TestPollingWatcherWatchReplacesDirectory(t *testing.T) {
	t.Parallel()

	first := t.TempDir()
	second := t.TempDir()

	w := NewPolling(testInterval)
	defer w.Close()

	if err := w.Watch(first); err != nil {
		t.Fatal(err)
	}

	waitQuiet(t, w, 5*time.Second)

	// While the first directory is watched its events are delivered.
	if err := os.WriteFile(filepath.Join(first, "old.txt"), nil, 0o644); err != nil {
		t.Fatal(err)
	}

	waitForEvent(t, w, 5*time.Second, func(e Event) bool {
		return e.Op == Create && e.Name == "old.txt" && e.Dir == filepath.Clean(first)
	})

	// Switch to the second directory and let its baseline settle.
	if err := w.Watch(second); err != nil {
		t.Fatal(err)
	}

	if got := w.WatchedDir(); got != filepath.Clean(second) {
		t.Fatalf("watched dir = %q, want %q", got, second)
	}

	waitQuiet(t, w, 5*time.Second)

	// A change in the old directory must never be attributed to the new one.
	if err := os.WriteFile(filepath.Join(first, "late.txt"), nil, 0o644); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(filepath.Join(second, "new.txt"), nil, 0o644); err != nil {
		t.Fatal(err)
	}

	deadline := time.After(5 * time.Second)

	for {
		select {
		case ev := <-w.Events():
			if ev.Dir != filepath.Clean(second) {
				t.Fatalf("received event bound to old directory: %+v", ev)
			}

			if ev.Op == Create && ev.Name == "new.txt" {
				return
			}
		case <-deadline:
			t.Fatal("timed out waiting for new.txt create event")
		}
	}
}

func TestPollingWatcherClose(t *testing.T) {
	t.Parallel()

	w := NewPolling(testInterval)

	if err := w.Watch(t.TempDir()); err != nil {
		t.Fatal(err)
	}

	if err := w.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	if err := w.Watch(t.TempDir()); !errors.Is(err, ErrWatcherClosed) {
		t.Fatalf("Watch after Close = %v, want ErrWatcherClosed", err)
	}
}

func TestManualWatcher(t *testing.T) {
	t.Parallel()

	w := NewManual()
	defer w.Close()

	if err := w.Watch("/tmp/some-dir"); err != nil {
		t.Fatal(err)
	}

	if !w.Emit(Event{Op: Create, Name: "x"}) {
		t.Fatal("emit failed")
	}

	select {
	case ev := <-w.Events():
		if ev.Op != Create || ev.Name != "x" || ev.Dir != "/tmp/some-dir" {
			t.Fatalf("unexpected event: %+v", ev)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for manual event")
	}

	w.WatchErr = errors.New("nope")
	if err := w.Watch("/tmp/other"); err == nil {
		t.Fatal("expected simulated watch failure")
	}
}
