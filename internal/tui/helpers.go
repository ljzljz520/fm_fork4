package tui

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/mistakenelf/fm/filetree"
	"github.com/mistakenelf/fm/polish"
)

func contains(s []string, str string) bool {
	return slices.Contains(s, str)
}

func (m *model) disableAllViewports() {
	m.code.SetViewportDisabled(true)
	m.pdf.SetViewportDisabled(true)
	m.markdown.SetViewportDisabled(true)
	m.help.SetViewportDisabled(true)
	m.image.SetViewportDisabled(true)
	m.csv.SetViewportDisabled(true)
}

func (m *model) resetViewports() {
	m.code.GotoTop()
	m.pdf.GotoTop()
	m.markdown.GotoTop()
	m.help.GotoTop()
	m.image.GotoTop()
	m.csv.GotoTop()
}

// watcherWarning returns persistent, visible feedback for degraded watcher
// states, including the manual rescan hint. It returns an empty string while
// watching works normally.
func (m *model) watcherWarning() string {
	warn := lipgloss.NewStyle().Foreground(polish.Colors.Red600).Bold(true)

	switch m.filetree.WatchStatus() {
	case filetree.WatchDirectoryMissing:
		return warn.Render("Directory is gone — press r to rescan, h to go up")
	case filetree.WatchUnavailable:
		return warn.Render("File watching is unavailable — press r to rescan")
	default:
		return ""
	}
}

// watcherIndicator renders a compact health marker next to the item count.
func (m *model) watcherIndicator() string {
	switch m.filetree.WatchStatus() {
	case filetree.WatchActive:
		return lipgloss.NewStyle().Foreground(lipgloss.Color("#16a34a")).Render("●")
	case filetree.WatchDirectoryMissing, filetree.WatchUnavailable:
		return lipgloss.NewStyle().Foreground(polish.Colors.Red600).Bold(true).Render("!")
	default:
		return ""
	}
}

// syncPreviewCmd updates or invalidates the explicitly previewed file after a
// filesystem-driven rescan. A deleted, moved or replaced-by-directory preview
// is closed with visible feedback; a changed file is reloaded from disk.
func (m *model) syncPreviewCmd(report filetree.RefreshReport) tea.Cmd {
	if m.previewPath == "" || filepath.Dir(m.previewPath) != report.Directory {
		return nil
	}

	name := filepath.Base(m.previewPath)

	info, err := os.Stat(m.previewPath)

	if contains(report.Removed, name) || err != nil || (info != nil && info.IsDir()) {
		m.state = idleState
		m.previewPath = ""
		m.disableAllViewports()
		m.resetViewports()

		return m.newStatusMessageCmd(
			lipgloss.NewStyle().
				Foreground(polish.Colors.Red600).
				Bold(true).
				Render("Previewed file changed on disk; preview closed"))
	}

	if contains(report.Changed, name) {
		return m.previewFileCmd(m.previewPath)
	}

	return nil
}

func (m *model) updateStatusBar() {
	count := fmt.Sprintf("%d/%d %s",
		m.filetree.Cursor+1,
		m.filetree.GetTotalItems(),
		m.watcherIndicator())

	if m.filetree.GetSelectedItem().Name != "" {
		statusMessage :=
			m.filetree.CurrentDirectory +
				lipgloss.NewStyle().
					Padding(0, 1).
					Foreground(polish.Colors.Yellow500).
					Render(m.filetree.GetSelectedItem().Details) +
				lipgloss.NewStyle().
					Padding(0, 1).
					Foreground(polish.Colors.Pink).
					Render(m.filetree.GetSelectedItem().
						FileInfo.
						ModTime().Format("2006-01-02"))

		if m.filetree.StatusMessage != "" {
			statusMessage = m.filetree.StatusMessage
		}

		if m.code.StatusMessage != "" {
			statusMessage = m.code.StatusMessage
		}

		if m.markdown.StatusMessage != "" {
			statusMessage = m.markdown.StatusMessage
		}

		if m.pdf.StatusMessage != "" {
			statusMessage = m.pdf.StatusMessage
		}

		if m.image.StatusMessage != "" {
			statusMessage = m.image.StatusMessage
		}

		if m.statusMessage != "" {
			statusMessage = m.statusMessage
		}

		switch {
		case m.showTextInput:
			statusMessage = m.textinput.View()
		case m.filetree.StatusMessage != "" ||
			m.code.StatusMessage != "" ||
			m.markdown.StatusMessage != "" ||
			m.pdf.StatusMessage != "" ||
			m.image.StatusMessage != "" ||
			m.statusMessage != "":
			// Keep the explicit message already selected above.
		case m.watcherWarning() != "":
			// Degraded watcher state is permanently visible when no other
			// message is being shown.
			statusMessage = m.watcherWarning()
		}

		m.statusbar.SetContent(
			m.filetree.GetSelectedItem().Name,
			statusMessage,
			count,
			fmt.Sprintf("%s", m.filetree.GetSelectedItem().FileSize),
		)
	} else {
		statusMessage := "Directory is empty"

		if warning := m.watcherWarning(); warning != "" {
			statusMessage = warning
		}

		if m.showTextInput {
			statusMessage = m.textinput.View()
		}

		m.statusbar.SetContent(
			"N/A",
			statusMessage,
			fmt.Sprintf("0/0 %s", m.watcherIndicator()),
			"FM",
		)
	}
}
