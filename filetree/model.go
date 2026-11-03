package filetree

import (
	"os"
	"time"

	"github.com/charmbracelet/lipgloss"

	"github.com/mistakenelf/fm/filesystem"
	"github.com/mistakenelf/fm/internal/watcher"
	"github.com/mistakenelf/fm/keys"
)

type treeState int

const (
	IdleState treeState = iota
	CreateFileState
	CreateDirectoryState
	MoveState
	RenameState
)

// defaultCoalesceDelay is how long the tree waits after the first observed
// change before re-reading the directory, so a burst of external events is
// merged into one rescan.
const defaultCoalesceDelay = 150 * time.Millisecond

type DirectoryItem struct {
	Name        string
	Details     string
	Path        string
	Extension   string
	FileSize    string
	IsDirectory bool
	FileInfo    os.FileInfo
}

// WatchStatus reports the health of the directory watcher.
type WatchStatus int

const (
	// WatchInactive means no watcher is attached.
	WatchInactive WatchStatus = iota
	// WatchActive means changes are being observed.
	WatchActive
	// WatchUnavailable means watching could not be established.
	WatchUnavailable
	// WatchDirectoryMissing means the watched directory disappeared.
	WatchDirectoryMissing
)

// RefreshReport describes one filesystem-driven refresh. It is consumed by
// the surrounding application, for example to update open previews.
type RefreshReport struct {
	Happened  bool
	Directory string
	// Changed lists names that were created or replaced.
	Changed []string
	// Removed lists names that disappeared (deleted or renamed away).
	Removed []string
}

type Model struct {
	files                 []DirectoryItem
	Cursor                int
	min                   int
	max                   int
	height                int
	width                 int
	Disabled              bool
	showHidden            bool
	showDirectoriesOnly   bool
	showFilesOnly         bool
	showIcons             bool
	keyMap                keys.KeyMap
	startDir              string
	StatusMessage         string
	selectionPath         string
	StatusMessageLifetime time.Duration
	statusMessageTimer    *time.Timer
	selectedItemColor     lipgloss.AdaptiveColor
	unselectedItemColor   lipgloss.AdaptiveColor
	inactiveItemColor     lipgloss.AdaptiveColor
	err                   error
	CurrentDirectory      string
	State                 treeState

	// Directory watching.
	watcher       watcher.Watcher
	watchStatus   WatchStatus
	watchGen      uint64
	waiterArmed   bool
	coalesceDelay time.Duration
	pendingEvents []watcher.Event
	pendingNames  map[string]struct{}
	resyncPending bool
	coalesceTimer *time.Timer
	lastRefresh   RefreshReport
}

func New(startDir string) Model {
	startingDirectory := filesystem.CurrentDirectory

	if startDir != "" {
		startingDirectory = startDir
	}

	return Model{
		Cursor:                0,
		Disabled:              false,
		keyMap:                keys.DefaultKeyMap(),
		min:                   0,
		max:                   0,
		startDir:              startingDirectory,
		showHidden:            true,
		StatusMessage:         "",
		StatusMessageLifetime: time.Second,
		showFilesOnly:         false,
		showDirectoriesOnly:   false,
		selectedItemColor:     lipgloss.AdaptiveColor{Light: "212", Dark: "212"},
		unselectedItemColor:   lipgloss.AdaptiveColor{Light: "ffffff", Dark: "#000000"},
		inactiveItemColor:     lipgloss.AdaptiveColor{Light: "243", Dark: "243"},
		showIcons:             true,
		coalesceDelay:         defaultCoalesceDelay,
		pendingNames:          make(map[string]struct{}),
	}
}
