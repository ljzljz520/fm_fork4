package filetree

import (
	"sort"
	"time"

	"github.com/charmbracelet/lipgloss"

	"github.com/mistakenelf/fm/internal/watcher"
)

// SetDisabled sets if the bubble is currently active.
func (m *Model) SetDisabled(disabled bool) {
	m.Disabled = disabled
}

// AttachWatcher connects a directory watcher to the tree.
func (m *Model) AttachWatcher(w watcher.Watcher) {
	m.watcher = w
}

// SetCoalesceDelay sets how long the tree waits after the first observed
// change before re-reading the directory. It is primarily used in tests.
func (m *Model) SetCoalesceDelay(delay time.Duration) {
	if delay > 0 {
		m.coalesceDelay = delay
	}
}

// WatchStatus returns the current health of the directory watcher.
func (m Model) WatchStatus() WatchStatus {
	return m.watchStatus
}

// ConsumeRefresh returns and clears the report of the last filesystem-driven
// rescan.
func (m *Model) ConsumeRefresh() RefreshReport {
	report := m.lastRefresh
	m.lastRefresh = RefreshReport{}

	return report
}

// preserveSelectionAndViewport re-anchors the cursor and the visible window
// after a rescan. Entries that still exist keep their selection and their
// position in the viewport; removed entries fall back to the closest valid
// position.
func (m *Model) preserveSelectionAndViewport(oldFiles, newFiles []DirectoryItem) {
	if len(newFiles) == 0 {
		m.Cursor = 0
		m.min = 0
		m.max = max(m.height-1, 0)

		return
	}

	find := func(name string) int {
		for i, item := range newFiles {
			if item.Name == name {
				return i
			}
		}

		return -1
	}

	// Anchor the cursor to the previously selected entry by name.
	newCursor := -1

	if m.Cursor >= 0 && m.Cursor < len(oldFiles) {
		newCursor = find(oldFiles[m.Cursor].Name)
	}

	if newCursor < 0 {
		newCursor = m.Cursor
		if newCursor > len(newFiles)-1 {
			newCursor = len(newFiles) - 1
		}
	}

	if newCursor < 0 {
		newCursor = 0
	}

	// Anchor the top of the visible window to the entry shown there before
	// the rescan, so stable files keep their exact screen position.
	newMin := -1
	if m.min >= 0 && m.min < len(oldFiles) {
		newMin = find(oldFiles[m.min].Name)
	}

	if newMin < 0 {
		newMin = m.min
	}

	visible := m.height - 1
	if visible < 0 {
		visible = 0
	}

	if newMin < 0 {
		newMin = 0
	}

	// Keep the selected entry inside the visible window.
	if newMin > newCursor {
		newMin = newCursor
	}

	if newMin+visible < newCursor {
		newMin = newCursor - visible
	}

	if newMin < 0 {
		newMin = 0
	}

	if newMin > len(newFiles)-1 {
		newMin = max(0, len(newFiles)-visible-1)
	}

	newMax := newMin + visible
	if newMax >= len(newFiles) {
		newMax = len(newFiles) - 1
	}

	m.Cursor = newCursor
	m.min = newMin
	m.max = newMax
}

// sameEntry reports whether two list entries describe the unchanged file,
// comparing the metadata reflected in the status bar.
func sameEntry(a, b DirectoryItem) bool {
	if a.IsDirectory != b.IsDirectory {
		return false
	}

	if a.FileInfo == nil || b.FileInfo == nil {
		return true
	}

	return a.FileInfo.Size() == b.FileInfo.Size() &&
		a.FileInfo.Mode() == b.FileInfo.Mode() &&
		a.FileInfo.ModTime().Equal(b.FileInfo.ModTime())
}

// buildRefreshReport derives what changed between two listings of the same
// directory.
func buildRefreshReport(directory string, oldFiles, newFiles []DirectoryItem) RefreshReport {
	oldByName := make(map[string]DirectoryItem, len(oldFiles))
	newByName := make(map[string]DirectoryItem, len(newFiles))

	for _, item := range oldFiles {
		oldByName[item.Name] = item
	}

	for _, item := range newFiles {
		newByName[item.Name] = item
	}

	report := RefreshReport{Happened: true, Directory: directory}

	for name := range oldByName {
		if _, ok := newByName[name]; !ok {
			report.Removed = append(report.Removed, name)
		}
	}

	for name, item := range newByName {
		old, ok := oldByName[name]
		if !ok || !sameEntry(old, item) {
			report.Changed = append(report.Changed, name)
		}
	}

	sort.Strings(report.Changed)
	sort.Strings(report.Removed)

	return report
}

// GetSelectedItem returns the currently selected file/dir.
func (m Model) GetSelectedItem() DirectoryItem {
	if len(m.files) > 0 {
		return m.files[m.Cursor]
	}

	return DirectoryItem{}
}

// GetTotalItems returns total number of tree items.
func (m Model) GetTotalItems() int {
	return len(m.files)
}

// SetSize Sets the size of the filetree.
func (m *Model) SetSize(width, height int) {
	m.height = height
	m.width = width
	m.max = m.height - 1
}

// SetTheme sets the theme of the tree.
func (m *Model) SetTheme(selectedItemColor, unselectedItemColor lipgloss.AdaptiveColor) {
	m.selectedItemColor = selectedItemColor
	m.unselectedItemColor = unselectedItemColor
}

// GetShowFilesOnly returns the showFilesOnly property.
func (m Model) GetShowFilesOnly() bool {
	return m.showFilesOnly
}

// GetShowDirectoriesOnly returns the showDirectoriesOnly property.
func (m Model) GetShowDirectoriesOnly() bool {
	return m.showDirectoriesOnly
}

// GetShowHidden returns the showHidden property.
func (m Model) GetShowHidden() bool {
	return m.showHidden
}

// SetSelectionPath sets the selection path to be written.
func (m *Model) SetSelectionPath(path string) {
	m.selectionPath = path
}

// SetShowIcons sets whether icons will show or not.
func (m *Model) SetShowIcons(show bool) {
	m.showIcons = show
}
