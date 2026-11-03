package filetree

import (
	"path/filepath"

	"github.com/charmbracelet/bubbles/key"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/mistakenelf/fm/filesystem"
	"github.com/mistakenelf/fm/internal/watcher"
	"github.com/mistakenelf/fm/polish"
)

// BeginNavigation invalidates any in-flight listing and coalesced events and
// requests the given directory. It binds the request to a new navigation
// generation so late results from a directory the user has left cannot take
// effect.
func (m Model) BeginNavigation(directory string) (Model, tea.Cmd) {
	m.watchGen++
	m.clearPending()

	return m, m.GetDirectoryListingCmd(directory)
}

func (m Model) Update(msg tea.Msg) (Model, tea.Cmd) {
	var cmds []tea.Cmd

	if m.Disabled {
		return m, nil
	}

	switch msg := msg.(type) {
	case editorFinishedMsg:
		if msg.err != nil {
			m.err = msg.err
			return m, tea.Quit
		}
	case errorMsg:
		cmds = append(cmds, m.NewStatusMessageCmd(
			lipgloss.NewStyle().
				Foreground(polish.Colors.Red600).
				Bold(true).
				Render(string(msg))))
	case statusMessageTimeoutMsg:
		m.StatusMessage = ""
	case moveDirectoryItemMsg:
		m.State = IdleState

		return m, m.beginNavigation(m.CurrentDirectory)
	case copyToClipboardMsg:
		cmds = append(cmds, m.NewStatusMessageCmd(
			lipgloss.NewStyle().
				Bold(true).
				Render(string(msg))))
	case createFileMsg:
		m.State = IdleState

		return m, m.beginNavigation(m.CurrentDirectory)
	case createDirectoryMsg:
		m.State = IdleState

		return m, m.beginNavigation(m.CurrentDirectory)
	case renameDirectoryItemMsg:
		m.State = IdleState

		return m, m.beginNavigation(m.CurrentDirectory)
	case getDirectoryListingMsg:
		// A newer navigation superseded this listing: discard it together
		// with any watcher side effects, so the user is never pulled back to
		// a directory they already left.
		if msg.gen != m.watchGen {
			return m, nil
		}

		oldFiles := m.files

		if msg.files != nil {
			m.files = msg.files
		} else {
			m.files = make([]DirectoryItem, 0)
		}

		m.CurrentDirectory = msg.workingDirectory
		m.clearPending()

		if msg.preserve {
			m.preserveSelectionAndViewport(oldFiles, m.files)
			m.lastRefresh = buildRefreshReport(m.CurrentDirectory, oldFiles, m.files)
		} else {
			m.Cursor = 0
			m.min = 0
			m.max = max(m.max, m.height-1)
			m.lastRefresh = RefreshReport{}
		}

		m.err = nil

		// (Re)establish the watcher only on real navigation: rescans must
		// not churn the watcher, and recovery is delivered as events.
		if m.watcher != nil {
			m.watchStatus = WatchActive

			if !msg.preserve {
				cmds = append(cmds, m.startWatchingCmd(m.CurrentDirectory))

				if !m.waiterArmed {
					m.waiterArmed = true
					cmds = append(cmds, waitForWatcherEventCmd(m.watcher))
				}
			}
		}
	case refreshFailedMsg:
		// Bind the failure to the directory/generation that requested it.
		if msg.gen != m.watchGen || msg.dir != m.CurrentDirectory {
			return m, nil
		}

		m.clearPending()

		if msg.directoryGone {
			m.watchStatus = WatchDirectoryMissing
		} else {
			m.watchStatus = WatchUnavailable
			cmds = append(cmds, m.NewStatusMessageCmd(
				lipgloss.NewStyle().
					Foreground(polish.Colors.Red600).
					Bold(true).
					Render("Could not refresh directory: "+msg.err.Error())))
		}
	case watcherEventMsg:
		// The wait command has returned and must always be re-armed.
		cmds = append(cmds, waitForWatcherEventCmd(m.watcher))

		ev := watcher.Event(msg)

		switch ev.Op {
		case watcher.WatchFailed:
			m.watchStatus = WatchUnavailable
			m.clearPending()
		case watcher.WatchLost:
			if ev.Dir == "" || ev.Dir == m.CurrentDirectory {
				m.watchStatus = WatchDirectoryMissing
				m.clearPending()
			}
		case watcher.Resync:
			if ev.Dir == m.CurrentDirectory {
				m.watchStatus = WatchActive
				m.resyncPending = true

				if m.coalesceTimer == nil {
					cmds = append(cmds, m.armCoalesceCmd(m.watchGen))
				}
			}
		default:
			// Events are bound to their directory. Observations from a
			// directory the user navigated away from are dropped here.
			if ev.Dir != "" && ev.Dir != m.CurrentDirectory {
				break
			}

			// While the watcher is degraded, change events cannot be
			// trusted; recovery arrives through Resync or a manual rescan.
			if m.watchStatus == WatchDirectoryMissing ||
				m.watchStatus == WatchUnavailable {
				break
			}

			cmds = append(cmds, m.coalesce(ev))
		}
	case coalesceTickMsg:
		return m, m.flushCoalescedCmd(msg.gen)
	case tea.KeyMsg:
		switch {
		case key.Matches(msg, m.keyMap.Down):
			if m.State != IdleState {
				return m, nil
			}

			m.Cursor++

			if m.Cursor >= len(m.files) {
				m.Cursor = len(m.files) - 1
			}

			if m.Cursor > m.max {
				m.min++
				m.max++
			}
		case key.Matches(msg, m.keyMap.Up):
			if m.State != IdleState {
				return m, nil
			}

			m.Cursor--

			if m.Cursor < 0 {
				m.Cursor = 0
			}

			if m.Cursor < m.min {
				m.min--
				m.max--
			}
		case key.Matches(msg, m.keyMap.GotoTop):
			if m.State != IdleState {
				return m, nil
			}

			m.Cursor = 0
			m.min = 0
			m.max = m.height
		case key.Matches(msg, m.keyMap.GotoBottom):
			if m.State != IdleState {
				return m, nil
			}

			m.Cursor = len(m.files) - 1
			m.min = len(m.files) - m.height
			m.max = len(m.files) - 1
		case key.Matches(msg, m.keyMap.PageDown):
			if m.State != IdleState {
				return m, nil
			}

			m.Cursor += m.height

			if m.Cursor >= len(m.files) {
				m.Cursor = len(m.files) - 1
			}

			m.min += m.height
			m.max += m.height

			if m.max >= len(m.files) {
				m.max = len(m.files) - 1
				m.min = m.max - m.height
			}
		case key.Matches(msg, m.keyMap.PageUp):
			if m.State != IdleState {
				return m, nil
			}

			m.Cursor -= m.height

			if m.Cursor < 0 {
				m.Cursor = 0
			}

			m.min -= m.height
			m.max -= m.height

			if m.min < 0 {
				m.min = 0
				m.max = m.min + m.height
			}
		case key.Matches(msg, m.keyMap.GoToHomeDirectory):
			if m.State != IdleState {
				return m, nil
			}

			return m, m.beginNavigation(filesystem.HomeDirectory)
		case key.Matches(msg, m.keyMap.GoToRootDirectory):
			if m.State != IdleState {
				return m, nil
			}

			return m, m.beginNavigation(filesystem.RootDirectory)
		case key.Matches(msg, m.keyMap.ToggleHidden):
			if m.State != IdleState {
				return m, nil
			}

			m.showHidden = !m.showHidden

			return m, m.beginNavigation(m.CurrentDirectory)
		case key.Matches(msg, m.keyMap.Rescan):
			if m.State != IdleState {
				return m, nil
			}

			// Manual rescan: retries a missing directory or an
			// unavailable watcher and re-reads the real directory.
			return m, m.beginNavigation(m.CurrentDirectory)
		case key.Matches(msg, m.keyMap.OpenDirectory):
			if m.State != IdleState {
				return m, nil
			}

			if len(m.files) == 0 {
				return m, nil
			}

			if m.files[m.Cursor].IsDirectory {
				return m, m.beginNavigation(m.files[m.Cursor].Path)
			}
		case key.Matches(msg, m.keyMap.PreviousDirectory):
			if m.State != IdleState {
				return m, nil
			}

			return m, m.beginNavigation(
				filepath.Dir(m.CurrentDirectory),
			)
		case key.Matches(msg, m.keyMap.CopyPathToClipboard):
			if m.State != IdleState {
				return m, nil
			}

			return m, copyToClipboardCmd(m.files[m.Cursor].Path)
		case key.Matches(msg, m.keyMap.CopyDirectoryItem):
			if m.State != IdleState {
				return m, nil
			}

			return m, tea.Sequence(
				copyDirectoryItemCmd(m.files[m.Cursor].Path, m.files[m.Cursor].IsDirectory),
				m.GetDirectoryListingCmd(m.CurrentDirectory),
			)
		case key.Matches(msg, m.keyMap.DeleteDirectoryItem):
			if m.State != IdleState {
				return m, nil
			}

			return m, tea.Sequence(
				deleteDirectoryItemCmd(m.files[m.Cursor].Path, m.files[m.Cursor].IsDirectory),
				m.GetDirectoryListingCmd(m.CurrentDirectory),
			)
		case key.Matches(msg, m.keyMap.ZipDirectoryItem):
			if m.State != IdleState {
				return m, nil
			}

			return m, tea.Sequence(
				zipDirectoryCmd(m.files[m.Cursor].Path),
				m.GetDirectoryListingCmd(m.CurrentDirectory),
			)
		case key.Matches(msg, m.keyMap.UnzipDirectoryItem):
			if m.State != IdleState {
				return m, nil
			}

			return m, tea.Sequence(
				unzipDirectoryCmd(m.files[m.Cursor].Name),
				m.GetDirectoryListingCmd(m.CurrentDirectory),
			)
		case key.Matches(msg, m.keyMap.ShowDirectoriesOnly):
			if m.State != IdleState {
				return m, nil
			}

			m.showDirectoriesOnly = !m.showDirectoriesOnly
			m.showFilesOnly = false

			return m, m.beginNavigation(m.CurrentDirectory)
		case key.Matches(msg, m.keyMap.ShowFilesOnly):
			if m.State != IdleState {
				return m, nil
			}

			m.showFilesOnly = !m.showFilesOnly
			m.showDirectoriesOnly = false

			return m, m.beginNavigation(m.CurrentDirectory)
		case key.Matches(msg, m.keyMap.WriteSelectionPath):
			if m.State != IdleState {
				return m, nil
			}

			if m.selectionPath != "" {
				return m, tea.Sequence(
					writeSelectionPathCmd(m.selectionPath, m.files[m.Cursor].Name),
					tea.Quit,
				)
			}
		case key.Matches(msg, m.keyMap.OpenInEditor):
			if m.State != IdleState {
				return m, nil
			}

			return m, openEditorCmd(m.files[m.Cursor].Name)
		case key.Matches(msg, m.keyMap.CreateFile):
			if m.State != IdleState {
				return m, nil
			}

			m.State = CreateFileState

			return m, nil
		case key.Matches(msg, m.keyMap.CreateDirectory):
			if m.State != IdleState {
				return m, nil
			}

			m.State = CreateDirectoryState

			return m, nil
		case key.Matches(msg, m.keyMap.RenameDirectoryItem):
			if m.State != IdleState {
				return m, nil
			}

			m.State = RenameState
		}
	}

	return m, tea.Batch(cmds...)
}

// beginNavigation is the pointer-style helper used within Update, which owns
// a mutable copy of the model.
func (m *Model) beginNavigation(directory string) tea.Cmd {
	m.watchGen++
	m.clearPending()

	return m.GetDirectoryListingCmd(directory)
}

// FlushPendingCmd performs the coalesced rescan immediately if events are
// pending for the current generation. It is primarily used by tests driving
// the model without the bubbletea runtime.
func (m *Model) FlushPendingCmd() tea.Cmd {
	return m.flushCoalescedCmd(m.watchGen)
}

// flushCoalescedCmd issues one rescan on behalf of the whole burst, or nil
// when nothing is pending. Late flushes for a superseded generation are
// discarded.
func (m *Model) flushCoalescedCmd(gen uint64) tea.Cmd {
	if gen != m.watchGen {
		m.clearPending()
		return nil
	}

	if len(m.pendingEvents) == 0 && !m.resyncPending {
		return nil
	}

	m.clearPending()
	m.watchStatus = WatchActive

	// One rescan on behalf of the whole burst.
	return m.RefreshCurrentDirectoryCmd()
}

// clearPending discards coalesced events and cancels an armed debounce timer.
func (m *Model) clearPending() {
	if m.coalesceTimer != nil {
		m.coalesceTimer.Stop()
		m.coalesceTimer = nil
	}

	m.pendingEvents = nil
	m.resyncPending = false
	m.pendingNames = make(map[string]struct{})
}

// coalesce records an event and arms the debounce timer for the first event of
// a burst.
func (m *Model) coalesce(ev watcher.Event) tea.Cmd {
	if m.pendingNames == nil {
		m.pendingNames = make(map[string]struct{})
	}

	names := []string{ev.Name}
	if ev.Op == watcher.Rename && ev.OldName != "" {
		names = append(names, ev.OldName)
	}

	for _, name := range names {
		if name == "" {
			continue
		}

		if _, ok := m.pendingNames[name]; ok {
			continue
		}

		m.pendingNames[name] = struct{}{}
		m.pendingEvents = append(m.pendingEvents, ev)
	}

	if m.coalesceTimer == nil {
		return m.armCoalesceCmd(m.watchGen)
	}

	return nil
}
