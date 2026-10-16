//go:build !windows

package main

import "errors"

// There is no tray icon outside Windows (Wails v2 has none, and the Windows
// one is our own). Without it, the window is never hidden: closing it
// minimises it, and a background start opens it minimised (see main.go), so
// the Dock or the taskbar always has a way back.

const backgroundFlag = "--background"

type trayHandlers struct {
	open, quit, sessEnd func()
}

func startTray(trayHandlers) (func(), error) { return nil, errors.New("no tray icon on this system") }

// nameToasts is Windows' business only; elsewhere the app's name comes from
// its bundle (macOS) or its desktop entry (Linux).
func nameToasts() {}
