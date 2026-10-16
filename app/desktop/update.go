package main

import (
	"context"
	"errors"
	"math/rand/v2"
	"os"
	"os/exec"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/wailsapp/wails/v2/pkg/runtime"

	"altnet-relay/updater"
)

// Updates.
//
// Every few hours the app asks our server for the latest signed release
// notice (package updater). On Windows, with automatic updates on (the
// default), it downloads the new installer in the background, checks it
// against the signed fingerprint, and installs it at a quiet moment: the
// window closed to the tray, no call going, nobody looking -- or straight
// away when the person presses Restart, or at the next start. The installer
// is per-user, so there is no admin prompt, and it reopens the app.
//
// Elsewhere (Linux packages need the system password; the Mac app is built on
// the Mac) the app only says a new version is out and where to get it.

const (
	updatesDir     = "updates"
	pendingFile    = "pending.json"
	firstCheck     = 90 * time.Second
	checkEvery     = 6 * time.Hour
	quietCheck     = time.Minute
	defaultUpdates = "https://api.panmox.org/relay/v1/update"
)

// UpdateView is what the window shows about an update.
type UpdateView struct {
	Version string `json:"version"`
	Notes   string `json:"notes"`
	Page    string `json:"page"`
	Ready   bool   `json:"ready"`   // downloaded and checked: Restart installs it
	Install string `json:"install"` // how to install by hand, where the app can't
}

type updates struct {
	mu      sync.Mutex
	view    *UpdateView
	path    string       // the checked installer, when Ready
	file    updater.File // what it must match
	busy    atomic.Bool  // a call is going
	hidden  atomic.Bool  // the window is closed to the tray
	running atomic.Bool  // an install has started
}

// updateURL is where the release notice comes from: our server, or
// RELAY_UPDATE_URL for testing. Test networks don't check unless told to.
func updateURL() string {
	if u := os.Getenv("RELAY_UPDATE_URL"); u != "" {
		return u
	}
	if os.Getenv("RELAY_NETWORK") == "local" {
		return ""
	}
	return defaultUpdates
}

func (a *App) autoUpdate() bool { return !a.bgSettings().ManualUpdates }

// updateLoop checks now and then, and installs at a quiet moment.
func (a *App) updateLoop() {
	url := updateURL()
	if url == "" {
		return
	}
	next := time.Now().Add(firstCheck)
	t := time.NewTicker(quietCheck)
	defer t.Stop()
	for {
		select {
		case <-a.ctx.Done():
			return
		case <-t.C:
		}
		if time.Now().After(next) {
			a.checkUpdate(url)
			next = time.Now().Add(checkEvery + time.Duration(rand.Int64N(int64(30*time.Minute))))
		}
		if a.quiet() {
			a.installUpdate(backgroundFlag)
		}
	}
}

// checkUpdate asks for the release notice and, on Windows with automatic
// updates on, fetches the installer.
func (a *App) checkUpdate(url string) {
	ctx, cancel := context.WithTimeout(a.ctx, 15*time.Minute)
	defer cancel()
	m, notice, err := updater.CheckNotice(ctx, updater.DefaultClient, url, appVersion, updater.ReleaseKey())
	if err != nil {
		if !errors.Is(err, updater.ErrNoUpdate) {
			runtime.LogWarning(a.ctx, "update check: "+err.Error())
		}
		return
	}
	v := &UpdateView{Version: m.Version, Notes: m.Notes, Page: m.Page}
	switch goruntime.GOOS {
	case "windows":
		f, ok := m.Files[updater.Windows]
		if ok && a.autoUpdate() {
			path, err := updater.Download(ctx, updater.DefaultClient, f, filepath.Join(a.cfg.DataDir, updatesDir))
			if err != nil {
				runtime.LogWarning(a.ctx, "update download: "+err.Error())
				return // try again at the next check
			}
			savePending(a.cfg.DataDir, notice)
			a.upd.mu.Lock()
			a.upd.path, a.upd.file = path, f
			a.upd.mu.Unlock()
			v.Ready = true
		} else {
			v.Install = "Download the new installer from the release page and run it."
		}
	case "linux":
		if f, ok := m.Files[updater.LinuxDeb]; ok {
			v.Install = "Download " + f.Name + " from the release page, then run: sudo apt install ./" + f.Name
		}
	case "darwin":
		v.Install = "Download the new Mac kit from the release page, unzip it and double-click Build AltNet Relay.command again."
	}
	a.upd.mu.Lock()
	a.upd.view = v
	a.upd.mu.Unlock()
	runtime.EventsEmit(a.ctx, "update", v)
}

// quiet: an update is ready and installing it now would interrupt nobody.
func (a *App) quiet() bool {
	a.upd.mu.Lock()
	ready := a.upd.path != ""
	a.upd.mu.Unlock()
	return ready && a.autoUpdate() && a.upd.hidden.Load() && !a.upd.busy.Load() && !a.bg.focused.Load()
}

// installUpdate runs the checked installer and quits; the installer reopens
// the app (in the tray when mode is --background, otherwise with a window).
func (a *App) installUpdate(mode string) error {
	a.upd.mu.Lock()
	path, f := a.upd.path, a.upd.file
	a.upd.mu.Unlock()
	if path == "" {
		return errors.New("no update is ready")
	}
	if !a.upd.running.CompareAndSwap(false, true) {
		return nil
	}
	if err := runInstaller(path, f, mode); err != nil {
		a.upd.running.Store(false)
		return err
	}
	a.Quit()
	return nil
}

// runInstaller checks the file once more and starts it, silently.
func runInstaller(path string, f updater.File, mode string) error {
	if ok, _ := updater.Matches(path, f); !ok {
		os.Remove(path)
		return errors.New("the downloaded update changed on disk; it will be fetched again")
	}
	relaunch := "window"
	if mode == backgroundFlag {
		relaunch = "background"
	}
	cmd := exec.Command(path, "/VERYSILENT", "/SUPPRESSMSGBOXES", "/NORESTART", "/RELAUNCH="+relaunch)
	if err := cmd.Start(); err != nil {
		return err
	}
	_ = cmd.Process.Release()
	return nil
}

// ─── a download kept for the next start ───

// The signed notice itself is kept, not just what it said: at the next start
// it is verified against the release key again, so a file in the data
// folder alone can never make the app run something.
func savePending(dir string, notice []byte) {
	_ = os.WriteFile(filepath.Join(dir, updatesDir, pendingFile), notice, 0o600)
}

// installPendingAtStart runs before the window opens: a checked update that
// is waiting gets installed now, instead of the old version starting. Old
// downloads are cleared away. It reports whether the app should exit.
func installPendingAtStart(dir string, background bool) bool {
	if goruntime.GOOS != "windows" || os.Getenv("RELAY_NETWORK") == "local" && os.Getenv("RELAY_UPDATE_URL") == "" {
		return false
	}
	if loadSettings(dir).ManualUpdates {
		return false
	}
	upd := filepath.Join(dir, updatesDir)
	raw, err := os.ReadFile(filepath.Join(upd, pendingFile))
	if err != nil {
		return false
	}
	m, err := updater.Open(raw, updater.ReleaseKey())
	f, ok := updater.File{}, false
	if err == nil {
		f, ok = m.Files[updater.Windows]
	}
	if err != nil || !ok || !updater.Newer(m.Version, appVersion) {
		_ = os.RemoveAll(upd) // installed already, unsigned, or nonsense: tidy up
		return false
	}
	path := filepath.Join(upd, filepath.Base(f.Name))
	mode := ""
	if background {
		mode = backgroundFlag
	}
	if err := runInstaller(path, f, mode); err != nil {
		_ = os.RemoveAll(upd)
		return false
	}
	return true
}

// ─── bound methods ───

// UpdateStatus is the update the window should mention, if any.
func (a *App) UpdateStatus() *UpdateView {
	a.upd.mu.Lock()
	defer a.upd.mu.Unlock()
	return a.upd.view
}

// InstallUpdate is the Restart button.
func (a *App) InstallUpdate() error { return a.installUpdate("") }

// SetBusy is how the window says a call is going (no installing then).
func (a *App) SetBusy(busy bool) { a.upd.busy.Store(busy) }

// OpenUpdatePage opens the release page in the browser.
func (a *App) OpenUpdatePage() {
	a.upd.mu.Lock()
	v := a.upd.view
	a.upd.mu.Unlock()
	if v != nil && strings.HasPrefix(v.Page, "https://") {
		runtime.BrowserOpenURL(a.ctx, v.Page)
	}
}
