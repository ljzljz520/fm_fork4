package filetree

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/mistakenelf/fm/internal/watcher"
)

const testWindowHeight = 4

// setupModel builds a tree watching a real temporary directory with a
// simulated event source and a coalescing delay long enough that flushes are
// driven deterministically in tests.
func setupModel(t *testing.T) (Model, *watcher.ManualWatcher, string) {
	t.Helper()

	return setupModelN(t, 5)
}

// setupModelN seeds the watched directory with n files named a.txt, b.txt, ...
func setupModelN(t *testing.T, n int) (Model, *watcher.ManualWatcher, string) {
	t.Helper()

	if n > 26 {
		t.Fatalf("setupModelN supports at most 26 files, got %d", n)
	}

	dir := t.TempDir()

	for i := range n {
		name := string(rune('a'+i)) + ".txt"

		if err := os.WriteFile(filepath.Join(dir, name), []byte(name), 0o644); err != nil {
			t.Fatal(err)
		}

		time.Sleep(2 * time.Millisecond) // distinct modtimes keep sort order stable
	}

	m := New(dir)
	m.SetSize(40, testWindowHeight)

	w := watcher.NewManual()
	m.AttachWatcher(w)
	m.SetCoalesceDelay(time.Hour)

	applyInitialListing(t, &m)
	startWatching(t, &m)

	if m.WatchStatus() != WatchActive {
		t.Fatalf("watch status = %v, want active", m.WatchStatus())
	}

	if got := w.WatchedDir(); got != dir {
		t.Fatalf("watcher watches %q, want %q", got, dir)
	}

	return m, w, dir
}

// applyInitialListing executes the initial directory listing synchronously
// without running the blocking watcher commands returned alongside it.
func applyInitialListing(t *testing.T, m *Model) {
	t.Helper()

	cmd := m.Init()
	if cmd == nil {
		t.Fatal("expected initial listing command")
	}

	msg, ok := cmd().(getDirectoryListingMsg)
	if !ok {
		t.Fatalf("expected getDirectoryListingMsg, got %T", cmd())
	}

	updated, _ := m.Update(msg)
	*m = updated
	m.SetSize(40, testWindowHeight)
}

// startWatching establishes the watcher the way the runtime would after the
// first listing, surfacing a WatchFailed event synchronously.
func startWatching(t *testing.T, m *Model) {
	t.Helper()

	cmd := m.startWatchingCmd(m.CurrentDirectory)
	if cmd == nil {
		return
	}

	if msg := cmd(); msg != nil {
		updated, _ := m.Update(msg)
		*m = updated
	}
}

// navigate performs a navigation and applies only its listing result.
func navigate(t *testing.T, m *Model, directory string) {
	t.Helper()

	updated, _ := m.BeginNavigation(directory)
	*m = updated

	msg := m.GetDirectoryListingCmd(directory)()

	updated, _ = m.Update(msg)
	*m = updated
}

// sendEvent delivers a simulated event and flushes the coalescing window,
// then executes the resulting rescan against the real directory.
func sendEvent(t *testing.T, m *Model, dir string, ev watcher.Event) {
	t.Helper()

	updated, _ := m.Update(watcherEventMsg(ev))
	*m = updated

	updated, cmd := m.Update(coalesceTickMsg{gen: m.watchGen})
	*m = updated

	if cmd == nil {
		t.Fatalf("expected a rescan command for event %+v", ev)
	}

	updated, _ = m.Update(cmd())
	*m = updated

	if m.CurrentDirectory != dir {
		t.Fatalf("rescan changed directory to %q", m.CurrentDirectory)
	}
}

func TestExternalCreateEventuallyAppears(t *testing.T) {
	t.Parallel()

	m, _, dir := setupModel(t)

	if err := os.WriteFile(filepath.Join(dir, "new.txt"), []byte("new"), 0o644); err != nil {
		t.Fatal(err)
	}

	sendEvent(t, &m, dir, watcher.Event{Dir: dir, Op: watcher.Create, Name: "new.txt"})

	if !slices.Contains(names(m.files), "new.txt") {
		t.Fatalf("created file not present after rescan: %v", names(m.files))
	}

	report := m.ConsumeRefresh()
	if !report.Happened || report.Directory != dir || !slices.Contains(report.Changed, "new.txt") {
		t.Fatalf("unexpected refresh report: %+v", report)
	}

	if m.ConsumeRefresh().Happened {
		t.Fatal("refresh report should be consumed once")
	}
}

func TestExternalDeletePreservesSelectionAndViewport(t *testing.T) {
	t.Parallel()

	// 8 files force the window to scroll: h,g,f,e,d are shown initially.
	m, _, dir := setupModelN(t, 8)

	// Four downs select d.txt and scroll the window so g.txt is on top.
	for range 4 {
		updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyDown})
		m = updated
	}

	if m.GetSelectedItem().Name != "d.txt" {
		t.Fatalf("selection setup failed: %s", m.GetSelectedItem().Name)
	}

	if m.files[m.min].Name != "g.txt" {
		t.Fatalf("viewport setup failed, top = %s", m.files[m.min].Name)
	}

	if err := os.Remove(filepath.Join(dir, "a.txt")); err != nil {
		t.Fatal(err)
	}

	sendEvent(t, &m, dir, watcher.Event{Dir: dir, Op: watcher.Remove, Name: "a.txt"})

	if m.GetSelectedItem().Name != "d.txt" {
		t.Fatalf("selection changed to %q, want d.txt", m.GetSelectedItem().Name)
	}

	if m.files[m.min].Name != "g.txt" {
		t.Fatalf("viewport top changed to %q, want g.txt", m.files[m.min].Name)
	}

	if slices.Contains(names(m.files), "a.txt") {
		t.Fatal("deleted file still listed")
	}
}

func TestSelectedFileDeletedClampsSelection(t *testing.T) {
	t.Parallel()

	m, _, dir := setupModel(t)

	target := m.GetSelectedItem().Name

	if err := os.Remove(filepath.Join(dir, target)); err != nil {
		t.Fatal(err)
	}

	sendEvent(t, &m, dir, watcher.Event{Dir: dir, Op: watcher.Remove, Name: target})

	if m.Cursor >= len(m.files) || m.Cursor < 0 {
		t.Fatalf("cursor out of range: %d / %d", m.Cursor, len(m.files))
	}

	report := m.ConsumeRefresh()
	if !slices.Contains(report.Removed, target) {
		t.Fatalf("removed %s not reported: %+v", target, report)
	}
}

func TestBurstCoalescedIntoSingleRescan(t *testing.T) {
	t.Parallel()

	m, _, dir := setupModel(t)

	// Rapid burst: three creates before the debounce flushes.
	for _, name := range []string{"x1", "x2", "x3"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(name), 0o644); err != nil {
			t.Fatal(err)
		}

		updated, _ := m.Update(watcherEventMsg(watcher.Event{Dir: dir, Op: watcher.Create, Name: name}))
		m = updated
	}

	if len(m.pendingEvents) != 3 {
		t.Fatalf("expected 3 pending events, got %d", len(m.pendingEvents))
	}

	// Repeated events for the same entry are deduplicated.
	updated, _ := m.Update(watcherEventMsg(watcher.Event{Dir: dir, Op: watcher.Replace, Name: "x1"}))
	m = updated

	if len(m.pendingEvents) != 3 {
		t.Fatalf("duplicate event should be merged, got %d pending", len(m.pendingEvents))
	}

	updated, cmd := m.Update(coalesceTickMsg{gen: m.watchGen})
	m = updated

	if cmd == nil {
		t.Fatal("expected coalesced rescan command")
	}

	updated, _ = m.Update(cmd())
	m = updated

	for _, want := range []string{"x1", "x2", "x3"} {
		if !slices.Contains(names(m.files), want) {
			t.Fatalf("burst file %s missing: %v", want, names(m.files))
		}
	}

	// A late tick without pending events must not trigger another rescan.
	updated, cmd = m.Update(coalesceTickMsg{gen: m.watchGen})
	m = updated

	if cmd != nil {
		t.Fatal("tick without pending events should not rescan")
	}
}

func TestFiltersRemainAppliedAfterRescan(t *testing.T) {
	t.Parallel()

	m, _, dir := setupModel(t)

	// Files only.
	updated, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'F'}})
	m = updated

	updated, _ = m.Update(cmd())
	m = updated

	if err := os.Mkdir(filepath.Join(dir, "new-dir"), 0o755); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(filepath.Join(dir, "new-file.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	sendEvent(t, &m, dir, watcher.Event{Dir: dir, Op: watcher.Create, Name: "new-dir"})

	if slices.Contains(names(m.files), "new-dir") {
		t.Fatal("directory should remain hidden by the files-only filter")
	}

	if !slices.Contains(names(m.files), "new-file.txt") {
		t.Fatalf("new file should appear: %v", names(m.files))
	}

	if !m.GetShowFilesOnly() {
		t.Fatal("files-only filter should remain enabled after rescan")
	}
}

func TestStaleEventFromOldDirectoryIgnored(t *testing.T) {
	t.Parallel()

	m, _, first := setupModel(t)
	second := t.TempDir()

	if err := os.WriteFile(filepath.Join(second, "in-second.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	navigate(t, &m, second)

	if m.CurrentDirectory != second {
		t.Fatalf("current directory = %q, want %q", m.CurrentDirectory, second)
	}

	// A late event from the directory the user left arrives.
	stale := watcher.Event{Dir: first, Op: watcher.Create, Name: "late.txt"}
	updated, _ := m.Update(watcherEventMsg(stale))
	m = updated

	if len(m.pendingEvents) != 0 {
		t.Fatalf("stale event should not be coalesced: %v", m.pendingEvents)
	}

	updated, tickCmd := m.Update(coalesceTickMsg{gen: m.watchGen})
	m = updated

	if tickCmd != nil {
		t.Fatal("stale event must not trigger a rescan that could navigate back")
	}

	if m.CurrentDirectory != second {
		t.Fatalf("user was pulled back to %q", m.CurrentDirectory)
	}
}

func TestStaleListingResultIsDiscarded(t *testing.T) {
	t.Parallel()

	m, _, first := setupModel(t)
	second := t.TempDir()

	// Issue a refresh for the first directory, then navigate away before its
	// result is applied.
	refreshCmd := m.RefreshCurrentDirectoryCmd()

	navigate(t, &m, second)

	// Now the old refresh result lands.
	updated, _ := m.Update(refreshCmd())
	m = updated

	if m.CurrentDirectory != second {
		t.Fatalf("stale refresh navigated back to %q", first)
	}
}

func TestDirectoryMissingShowsFeedbackAndRecovers(t *testing.T) {
	t.Parallel()

	m, _, dir := setupModel(t)

	// Watcher reports the directory disappeared.
	updated, _ := m.Update(watcherEventMsg(watcher.Event{
		Dir: dir,
		Op:  watcher.WatchLost,
		Err: os.ErrNotExist,
	}))
	m = updated

	if m.WatchStatus() != WatchDirectoryMissing {
		t.Fatalf("status = %v, want missing", m.WatchStatus())
	}

	// Change events while missing are not trusted.
	updated, _ = m.Update(watcherEventMsg(watcher.Event{Dir: dir, Op: watcher.Create, Name: "x"}))
	m = updated

	updated, tickCmd := m.Update(coalesceTickMsg{gen: m.watchGen})
	m = updated

	if tickCmd != nil {
		t.Fatal("events must be discarded while the directory is missing")
	}

	// Remove the real directory; a manual rescan keeps the warning visible.
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}

	updated, rescanCmd := m.BeginNavigation(dir)
	m = updated

	updated, _ = m.Update(rescanCmd())
	m = updated

	if m.WatchStatus() != WatchDirectoryMissing {
		t.Fatalf("status = %v, want missing", m.WatchStatus())
	}

	// The watcher recovering and re-syncing rescans automatically.
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(filepath.Join(dir, "returned.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	updated, _ = m.Update(watcherEventMsg(watcher.Event{Dir: dir, Op: watcher.Resync}))
	m = updated

	updated, cmd := m.Update(coalesceTickMsg{gen: m.watchGen})
	m = updated

	if cmd == nil {
		t.Fatal("resync should schedule a rescan")
	}

	updated, _ = m.Update(cmd())
	m = updated

	if m.WatchStatus() != WatchActive {
		t.Fatalf("status = %v, want active after recovery", m.WatchStatus())
	}

	if !slices.Contains(names(m.files), "returned.txt") {
		t.Fatalf("recovered directory listing missing file: %v", names(m.files))
	}
}

func TestWatcherUnavailableIsReportedAndManualRescanWorks(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	if err := os.WriteFile(filepath.Join(dir, "f.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	w := watcher.NewManual()
	w.WatchErr = errors.New("watching not supported")

	m := New(dir)
	m.AttachWatcher(w)
	m.SetSize(40, testWindowHeight)
	applyInitialListing(t, &m)
	startWatching(t, &m)

	if m.WatchStatus() != WatchUnavailable {
		t.Fatalf("status = %v, want unavailable", m.WatchStatus())
	}

	// Manual rescan still works against the real directory even though the
	// watcher cannot be established.
	if err := os.WriteFile(filepath.Join(dir, "manual.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	updated, rescanCmd := m.BeginNavigation(dir)
	m = updated

	updated, _ = m.Update(rescanCmd())
	m = updated

	if !slices.Contains(names(m.files), "manual.txt") {
		t.Fatalf("manual rescan should read the real directory: %v", names(m.files))
	}
}

func names(items []DirectoryItem) []string {
	result := make([]string, 0, len(items))

	for _, item := range items {
		result = append(result, item.Name)
	}

	return result
}
