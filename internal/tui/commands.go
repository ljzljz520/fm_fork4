package tui

import (
	"path/filepath"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/mistakenelf/fm/filesystem"
	"github.com/mistakenelf/fm/polish"
)

type statusMessageTimeoutMsg struct{}

func (m *model) openFileCmd() tea.Cmd {
	selectedFile := m.filetree.GetSelectedItem()

	if selectedFile.IsDirectory {
		return nil
	}

	return m.previewFileCmd(selectedFile.Path)
}

// previewFileCmd opens the right-pane preview for an arbitrary path. It is
// used both for explicit previews and for reloading a preview whose file
// changed on disk.
func (m *model) previewFileCmd(path string) tea.Cmd {
	m.resetViewports()
	m.previewPath = path

	isBinary, err := filesystem.IsBinary(path)
	if err != nil {
		m.previewPath = ""
		return m.newStatusMessageCmd(lipgloss.NewStyle().
			Foreground(polish.Colors.Red600).
			Bold(true).
			Render("Selected file type is not supported"))
	}

	switch filepath.Ext(path) {
	case ".csv":
		m.state = showCsvState

		return m.csv.SetFileNameCmd(path)
	case ".png", ".jpg", ".jpeg":
		m.state = showImageState

		return m.image.SetFileNameCmd(path)
	case ".md":
		if !m.config.PrettyMarkdown {
			m.state = showCodeState

			return m.code.SetFileNameCmd(path)
		}

		m.state = showMarkdownState

		return m.markdown.SetFileNameCmd(path)
	case ".pdf":
		m.state = showPdfState

		return m.pdf.SetFileNameCmd(path)
	}

	if isBinary {
		m.previewPath = ""
		return m.newStatusMessageCmd(
			lipgloss.NewStyle().
				Foreground(polish.Colors.Red600).
				Bold(true).
				Render("Selected file cannot be opened"),
		)
	}

	m.state = showCodeState

	return m.code.SetFileNameCmd(path)
}

// newStatusMessage sets a new status message, which will show for a limited
// amount of time.
func (m *model) newStatusMessageCmd(s string) tea.Cmd {
	m.statusMessage = s

	if m.statusMessageTimer != nil {
		m.statusMessageTimer.Stop()
	}

	m.statusMessageTimer = time.NewTimer(m.statusMessageLifetime)

	return func() tea.Msg {
		<-m.statusMessageTimer.C
		return statusMessageTimeoutMsg{}
	}
}
