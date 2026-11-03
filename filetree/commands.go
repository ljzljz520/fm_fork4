package filetree

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"time"

	"github.com/atotto/clipboard"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/mistakenelf/fm/filesystem"
	"github.com/mistakenelf/fm/internal/watcher"
)

type errorMsg string
type copyToClipboardMsg string
type statusMessageTimeoutMsg struct{}
type editorFinishedMsg struct{ err error }
type createFileMsg struct{}
type createDirectoryMsg struct{}
type moveDirectoryItemMsg struct{}
type renameDirectoryItemMsg struct{}

type getDirectoryListingMsg struct {
	files            []DirectoryItem
	workingDirectory string
	// gen binds the listing to a navigation generation. Late listings from a
	// directory the user has already left are discarded.
	gen uint64
	// preserve keeps the selection and viewport anchored to the same entries.
	// It is set for filesystem-driven rescans of the current directory.
	preserve bool
}

// refreshFailedMsg reports that a filesystem-driven rescan could not run.
type refreshFailedMsg struct {
	dir           string
	gen           uint64
	err           error
	directoryGone bool
}

// watcherEventMsg wraps a watcher event for the bubbletea runtime.
type watcherEventMsg watcher.Event

// coalesceTickMsg flushes the events that accumulated within one burst.
type coalesceTickMsg struct {
	gen uint64
}

// NewStatusMessageCmd sets a new status message, which will show for a limited
// amount of time. Note that this also returns a command.
func (m *Model) NewStatusMessageCmd(s string) tea.Cmd {
	m.StatusMessage = s

	if m.statusMessageTimer != nil {
		m.statusMessageTimer.Stop()
	}

	m.statusMessageTimer = time.NewTimer(m.StatusMessageLifetime)

	return func() tea.Msg {
		<-m.statusMessageTimer.C
		return statusMessageTimeoutMsg{}
	}
}

// CreateDirectoryCmd creates a directory based on the name provided.
func (m *Model) CreateDirectoryCmd(name string) tea.Cmd {
	return func() tea.Msg {
		if err := filesystem.CreateDirectory(name); err != nil {
			return errorMsg(err.Error())
		}

		return createDirectoryMsg{}
	}
}

// CreateFileCmd creates a file based on the name provided.
func (m *Model) CreateFileCmd(name string) tea.Cmd {
	return func() tea.Msg {
		if err := filesystem.CreateFile(name); err != nil {
			return errorMsg(err.Error())
		}

		return createFileMsg{}
	}
}

// RenameDirectoryItemCmd renames a file or folder given a name.
func (m *Model) RenameDirectoryItemCmd(originalName, newName string) tea.Cmd {
	return func() tea.Msg {
		if err := filesystem.RenameDirectoryItem(originalName, newName); err != nil {
			return errorMsg(err.Error())
		}

		return renameDirectoryItemMsg{}
	}
}

// MoveDirectoryItemCmd moves an item from one place to another.
func (m Model) MoveDirectoryItemCmd(source, destination string) tea.Cmd {
	return func() tea.Msg {
		if err := filesystem.MoveDirectoryItem(source, destination); err != nil {
			return errorMsg(err.Error())
		}

		return moveDirectoryItemMsg{}
	}
}

// readDirectory re-reads the real directory from disk and applies the active
// hidden/files-only/directories-only filters. It is the single source of
// truth shared by navigation commands and filesystem-driven rescans.
func (m Model) readDirectory(directoryName string) ([]DirectoryItem, string, error) {
	var (
		err           error
		files         []fs.DirEntry
		directoryPath string
	)

	if directoryName == filesystem.HomeDirectory {
		directoryName, err = filesystem.GetHomeDirectory()
		if err != nil {
			return nil, "", err
		}
	}

	if !filepath.IsAbs(directoryName) {
		directoryPath, err = filepath.Abs(directoryName)
		if err != nil {
			return nil, "", err
		}
	} else {
		directoryPath = directoryName
	}

	directoryInfo, err := os.Stat(directoryPath)
	if err != nil {
		return nil, directoryPath, err
	}

	if !directoryInfo.IsDir() {
		return nil, directoryPath, errNotADirectory
	}

	if !m.showDirectoriesOnly && !m.showFilesOnly {
		files, err = filesystem.GetDirectoryListing(directoryPath, m.showHidden)
		if err != nil {
			return nil, directoryPath, err
		}
	} else {
		listingType := filesystem.DirectoriesListingType

		if m.showFilesOnly {
			listingType = filesystem.FilesListingType
		}

		files, err = filesystem.GetDirectoryListingByType(directoryPath, listingType, m.showHidden)
		if err != nil {
			return nil, directoryPath, err
		}
	}

	directoryItems := make([]DirectoryItem, 0, len(files))

	for _, file := range files {
		fileInfo, err := file.Info()
		if err != nil {
			continue
		}

		filePath := filepath.Join(directoryPath, file.Name())
		isDirectory := fileInfo.IsDir()

		isSymlink := fileInfo.Mode()&os.ModeSymlink != 0

		if isSymlink {
			symlinkPath, err := filepath.EvalSymlinks(filepath.Join(directoryPath, file.Name()))
			if err != nil {
				// This could be due to symlinks pointing to invalid path
				continue
			}
			filePath = symlinkPath
			symlinkInfo, err := os.Stat(symlinkPath)
			if err != nil {
				return nil, directoryPath, err
			}

			isDirectory = symlinkInfo.IsDir()
		}

		fileSize := filesystem.ConvertBytesToSizeString(fileInfo.Size())

		directoryItems = append(directoryItems, DirectoryItem{
			Name:        file.Name(),
			Details:     fileInfo.Mode().String(),
			Path:        filePath,
			Extension:   filepath.Ext(fileInfo.Name()),
			IsDirectory: isDirectory,
			FileInfo:    fileInfo,
			FileSize:    fileSize,
		})
	}

	sort.Slice(directoryItems, func(i, j int) bool {
		return directoryItems[j].FileInfo.ModTime().Before(directoryItems[i].FileInfo.ModTime())
	})

	return directoryItems, directoryPath, nil
}

var errNotADirectory = errors.New("path is not a directory")

// GetDirectoryListingCmd updates the directory listing based on the name of
// the directory provided. The listing is bound to the current navigation
// generation, so listings arriving after the user navigated elsewhere are
// discarded in Update.
func (m Model) GetDirectoryListingCmd(directoryName string) tea.Cmd {
	gen := m.watchGen

	return func() tea.Msg {
		directoryItems, directoryPath, err := m.readDirectory(directoryName)
		if err != nil {
			if errors.Is(err, errNotADirectory) {
				return nil
			}

			return errorMsg(err.Error())
		}

		return getDirectoryListingMsg{
			files:            directoryItems,
			workingDirectory: directoryPath,
			gen:              gen,
		}
	}
}

// RefreshCurrentDirectoryCmd re-reads the current directory without resetting
// the cursor or the viewport. Both the request and the response are bound to
// the navigation generation and directory, so late results never move the
// user back to a directory they already left.
func (m Model) RefreshCurrentDirectoryCmd() tea.Cmd {
	directory := m.CurrentDirectory
	gen := m.watchGen

	return func() tea.Msg {
		directoryItems, directoryPath, err := m.readDirectory(directory)
		if err != nil {
			return refreshFailedMsg{
				dir:           directory,
				gen:           gen,
				err:           err,
				directoryGone: errors.Is(err, fs.ErrNotExist) || os.IsNotExist(err),
			}
		}

		return getDirectoryListingMsg{
			files:            directoryItems,
			workingDirectory: directoryPath,
			gen:              gen,
			preserve:         true,
		}
	}
}

// StartWatchingCmd (re)starts the watcher for the given directory. Failure to
// establish the watch is delivered as a watcher event so the UI can show
// visible feedback and offer a manual rescan.
func (m Model) StartWatchingCmd(directory string) tea.Cmd {
	return m.startWatchingCmd(directory)
}

// startWatchingCmd (re)starts the watcher for the directory currently shown.
func (m Model) startWatchingCmd(directory string) tea.Cmd {
	w := m.watcher
	if w == nil {
		return nil
	}

	return func() tea.Msg {
		if err := w.Watch(directory); err != nil {
			return watcherEventMsg{Dir: directory, Op: watcher.WatchFailed, Err: err}
		}

		return nil
	}
}

// WatcherEvents exposes the watcher event channel, or nil when no watcher is
// attached. It allows embedding applications (and tests) to drive the model
// without a full bubbletea runtime.
func (m Model) WatcherEvents() <-chan watcher.Event {
	if m.watcher == nil {
		return nil
	}

	return m.watcher.Events()
}

// WatcherEventMessage converts a watcher event into a model message.
func (m Model) WatcherEventMessage(ev watcher.Event) tea.Msg {
	return watcherEventMsg(ev)
}

// waitForWatcherEventCmd blocks until the watcher delivers one event. It is
// re-armed after every event consumed.
func waitForWatcherEventCmd(w watcher.Watcher) tea.Cmd {
	if w == nil {
		return nil
	}

	return func() tea.Msg {
		ev, ok := <-w.Events()
		if !ok {
			return watcherEventMsg{
				Op:  watcher.WatchFailed,
				Err: errors.New("directory watcher was closed"),
			}
		}

		return watcherEventMsg(ev)
	}
}

// armCoalesceCmd waits for the coalescing window to elapse and then requests a
// single rescan on behalf of every event accumulated in the burst.
func (m *Model) armCoalesceCmd(gen uint64) tea.Cmd {
	if m.coalesceTimer != nil {
		m.coalesceTimer.Stop()
	}

	m.coalesceTimer = time.NewTimer(m.coalesceDelay)
	timer := m.coalesceTimer

	return func() tea.Msg {
		<-timer.C
		return coalesceTickMsg{gen: gen}
	}
}

// deleteDirectoryItemCmd deletes a directory based on the name provided.
func deleteDirectoryItemCmd(name string, isDirectory bool) tea.Cmd {
	return func() tea.Msg {
		if isDirectory {
			if err := filesystem.DeleteDirectory(name); err != nil {
				return errorMsg(err.Error())
			}
		} else {
			if err := filesystem.DeleteFile(name); err != nil {
				return errorMsg(err.Error())
			}
		}

		return nil
	}
}

// zipDirectoryCmd zips a directory based on the name provided.
func zipDirectoryCmd(name string) tea.Cmd {
	return func() tea.Msg {
		if err := filesystem.Zip(name); err != nil {
			return errorMsg(err.Error())
		}

		return nil
	}
}

// unzipDirectoryCmd unzips a directory based on the name provided.
func unzipDirectoryCmd(name string) tea.Cmd {
	return func() tea.Msg {
		if err := filesystem.Unzip(name); err != nil {
			return errorMsg(err.Error())
		}

		return nil
	}
}

// copyDirectoryItemCmd copies a directory based on the name provided.
func copyDirectoryItemCmd(name string, isDirectory bool) tea.Cmd {
	return func() tea.Msg {
		if isDirectory {
			if err := filesystem.CopyDirectory(name); err != nil {
				return errorMsg(err.Error())
			}
		} else {
			if err := filesystem.CopyFile(name); err != nil {
				return errorMsg(err.Error())
			}
		}

		return nil
	}
}

// copyToClipboardCmd copies the provided string to the clipboard.
func copyToClipboardCmd(path string) tea.Cmd {
	return func() tea.Msg {
		err := clipboard.WriteAll(path)
		if err != nil {
			return errorMsg(err.Error())
		}

		return copyToClipboardMsg(
			fmt.Sprintf("%s %s %s", "Successfully copied", path, "to clipboard"),
		)
	}
}

// writeSelectionPathCmd writes content to the file specified.
func writeSelectionPathCmd(selectionPath, filePath string) tea.Cmd {
	return func() tea.Msg {
		if err := filesystem.WriteToFile(selectionPath, filePath); err != nil {
			return errorMsg(err.Error())
		}

		return nil
	}
}

func openEditorCmd(file string) tea.Cmd {
	editor := os.Getenv("EDITOR")
	if editor == "" {
		editor = "vim"
	}

	c := exec.Command(editor, file)

	return tea.ExecProcess(c, func(err error) tea.Msg {
		return editorFinishedMsg{err}
	})
}
