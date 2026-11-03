package tui

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/mistakenelf/fm/filetree"
)

const testWatchInterval = 20 * time.Millisecond

// setupWatchModel boots a real model against a temporary directory, using
// the real polling watcher, without running the bubbletea runtime: watcher
// events are read directly from the event channel.
func setupWatchModel(t *testing.T, dir string) model {
	t.Helper()

	m := New(Config{
		StartDir:       dir,
		WatchInterval:  testWatchInterval,
		ShowIcons:      false,
		PrettyMarkdown: true,
	})

	updated, _ := m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	m = updated.(model)

	// Apply the initial directory listing.
	initCmd := m.filetree.Init()

	updated, _ = m.Update(initCmd())
	m = updated.(model)

	// Establish the polling watcher explicitly (normally the returned
	// command runs inside the runtime).
	if cmd := m.filetree.StartWatchingCmd(dir); cmd != nil {
		if msg := cmd(); msg != nil {
			updated, _ = m.Update(msg)
			m = updated.(model)
		}
	}

	if m.filetree.WatchStatus() != filetree.WatchActive {
		t.Fatalf("unexpected initial watch status: %v", m.filetree.WatchStatus())
	}

	return m
}

func applyMsg(t *testing.T, m *model, msg tea.Msg) tea.Cmd {
	t.Helper()

	updated, cmd := m.Update(msg)

	var ok bool
	*m, ok = updated.(model)

	if !ok {
		t.Fatalf("Update returned %T", updated)
	}

	return cmd
}

// watchLoop drives one real watcher event through the full pipeline:
// coalesce -> flush -> directory rescan -> preview sync. All updates run in
// the caller's goroutine; blocking follow-up commands deliver their result on
// followUp. Returned batches containing the perpetual waiter command are
// intentionally not executed.
func watchLoop(t *testing.T, m *model, deadline time.Time, condition func() bool) {
	t.Helper()

	events := m.filetree.WatcherEvents()
	followUp := make(chan tea.Msg, 8)

	for time.Now().Before(deadline) {
		if condition() {
			return
		}

		select {
		case ev, ok := <-events:
			if !ok {
				t.Fatal("watcher event channel closed")
			}

			applyMsg(t, m, m.filetree.WatcherEventMessage(ev))

			// Flush the coalescing window immediately (coalescing timing is
			// covered by the filetree package tests).
			flush := m.filetree.FlushPendingCmd()
			if flush == nil {
				continue
			}

			listing := flush()
			if listing == nil {
				continue
			}

			syncCmd := applyMsg(t, m, listing)
			if syncCmd == nil {
				continue
			}

			// The follow-up command is either a quick preview reload (a file
			// read) or a timer-backed status message; run it off the loop and
			// deliver any immediate result.
			go func(cmd tea.Cmd) {
				msg := cmd()
				if msg != nil {
					select {
					case followUp <- msg:
					default:
					}
				}
			}(syncCmd)
		case msg := <-followUp:
			applyMsg(t, m, msg)
		case <-time.After(100 * time.Millisecond):
		}
	}

	if !condition() {
		t.Fatal("condition not met before deadline")
	}
}

func TestPreviewReloadsWhenFileChanges(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "preview.txt")

	if err := os.WriteFile(path, []byte("UNIQUE_TOKEN_ONE_1111111111"), 0o644); err != nil {
		t.Fatal(err)
	}

	m := setupWatchModel(t, dir)

	// Open the preview explicitly.
	applyMsg(t, &m, m.openFileCmd()())

	if m.state != showCodeState {
		t.Fatalf("state = %v, want showCodeState", m.state)
	}

	if !strings.Contains(m.code.Content, "UNIQUE_TOKEN_ONE") {
		t.Fatalf("initial preview content not rendered: %q", m.code.Content)
	}

	// External program replaces the file.
	time.Sleep(5 * testWatchInterval)

	if err := os.WriteFile(path, []byte("UNIQUE_TOKEN_TWO_22222222222222222222"), 0o644); err != nil {
		t.Fatal(err)
	}

	watchLoop(t, &m, time.Now().Add(5*time.Second), func() bool {
		return strings.Contains(m.code.Content, "UNIQUE_TOKEN_TWO")
	})

	if m.previewPath != path {
		t.Fatalf("preview path changed unexpectedly: %q", m.previewPath)
	}
}

func TestPreviewInvalidatedWhenFileDeleted(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "doomed.txt")

	if err := os.WriteFile(path, []byte("doomed content 1234567890"), 0o644); err != nil {
		t.Fatal(err)
	}

	m := setupWatchModel(t, dir)
	applyMsg(t, &m, m.openFileCmd()())

	if m.state != showCodeState || m.previewPath == "" {
		t.Fatalf("preview not open: state=%v path=%q", m.state, m.previewPath)
	}

	time.Sleep(5 * testWatchInterval)

	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}

	watchLoop(t, &m, time.Now().Add(5*time.Second), func() bool {
		return m.state == idleState && m.previewPath == ""
	})
}

func TestStatusBarShowsActiveIndicatorAndMissingWarning(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	if err := os.WriteFile(filepath.Join(dir, "f.txt"), []byte("hello 12345"), 0o644); err != nil {
		t.Fatal(err)
	}

	m := setupWatchModel(t, dir)

	// The active watcher renders the marker next to the item count.
	if !strings.Contains(m.statusbar.ThirdColumn, "●") {
		t.Fatalf("active watcher marker missing: %q", m.statusbar.ThirdColumn)
	}

	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}

	watchLoop(t, &m, time.Now().Add(5*time.Second), func() bool {
		return strings.Contains(m.statusbar.SecondColumn, "Directory is gone")
	})
}
