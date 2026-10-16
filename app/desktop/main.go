// Command AltNetRelay is the desktop app for AltNet Relay.
//
// The window is Wails (a native WebView2 window on Windows); everything
// it does goes through the engine package, the same code the terminal
// client runs on.
package main

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"errors"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	goruntime "runtime"
	"strings"

	"github.com/wailsapp/wails/v2"
	"github.com/wailsapp/wails/v2/pkg/options"
	"github.com/wailsapp/wails/v2/pkg/options/assetserver"
)

// The frontend is plain files -- no bundler, no npm -- embedded as they
// sit on disk.
//
//go:embed all:frontend
var assets embed.FS

var errNotReady = errors.New("still starting up — try again in a moment")

// instanceID names the single-instance lock after the data folder.
// Windows paths are case-insensitive, so the path is lowercased first:
// "C:\Users\X" and "c:\users\x" are the same folder and must share a lock.
func instanceID(dataDir string) string {
	abs, err := filepath.Abs(dataDir)
	if err != nil {
		abs = dataDir
	}
	sum := sha256.Sum256([]byte(strings.ToLower(abs)))
	return "altnet-relay-" + hex.EncodeToString(sum[:8])
}

func main() {
	ui, err := fs.Sub(assets, "frontend")
	if err != nil {
		log.Fatal(err)
	}

	// A restart (after bringing an account here) waits for the old copy to
	// close before taking the data folder.
	waitForPrevious(os.Args[1:])

	cfg := config()
	app := NewApp(cfg)
	// The system starts the app at sign-in with --background. On Windows that
	// means no window, just the tray icon and the node, until someone opens it.
	// Elsewhere there is no tray icon to bring a hidden window back, so it
	// starts minimised instead: in the Dock or the taskbar, out of the way.
	background := false
	for _, arg := range os.Args[1:] {
		if arg == backgroundFlag {
			background = true
		}
	}
	startHidden := background && goruntime.GOOS == "windows"
	// A checked update waiting from last time is installed instead of
	// starting this (older) version; the installer reopens the app.
	if installPendingAtStart(cfg.DataDir, background) {
		return
	}
	app.upd.hidden.Store(startHidden)
	startState := options.Normal
	if background && !startHidden {
		startState = options.Minimised
	}
	err = wails.Run(&options.App{
		Title:     "AltNet Relay",
		Width:     1040,
		Height:    700,
		MinWidth:  760,
		MinHeight: 520,
		AssetServer: &assetserver.Options{
			Assets: ui,
		},
		// One running copy per data folder. Two copies on the same folder
		// would run two nodes under one identity and race to write the
		// same message store. Keyed by folder rather than globally, so two
		// different profiles (RELAY_DATA) can still run side by side.
		SingleInstanceLock: &options.SingleInstanceLock{
			UniqueId:               instanceID(cfg.DataDir),
			OnSecondInstanceLaunch: app.onSecondInstance,
		},
		// Paint the window the app's own background colour from the first
		// frame, so there's no white flash before the page loads.
		BackgroundColour: &options.RGBA{R: 6, G: 10, B: 24, A: 1},
		StartHidden:      startHidden,
		WindowStartState: startState,
		// Only a Mac hides itself on close (the Dock brings it back); on
		// Windows the tray does that job, and Linux minimises. Changing the
		// setting takes effect here at the next start.
		HideWindowOnClose: goruntime.GOOS == "darwin" && loadSettings(cfg.DataDir).Background,
		OnBeforeClose:     app.beforeClose,
		OnStartup:         app.startup,
		OnShutdown:        app.shutdown,
		Bind:              []interface{}{app},
	})
	if err != nil {
		log.Fatal(err)
	}
}
