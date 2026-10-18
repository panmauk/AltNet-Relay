package main

// Running in the background.
//
// AltNet Relay is meant to be always on: the node keeps receiving, and a
// message produces a desktop notification straight away, with the window
// open, minimised or closed. Closing the window only hides it (a tray icon
// brings it back; its menu has Quit), and the app can start with Windows.
//
// The notifications are decided here, in Go, not in the page. A hidden page
// can be slowed down by the browser engine, and a notification that waits on
// it isn't "right away". The window only tells Go whether it is being looked
// at (SetFocused); while it is, the window shows its own in-page toast and
// the desktop stays quiet.
//
// As everywhere in this app, a notification names who wrote and nothing else:
// Windows keeps notifications in the Action Center, where anyone at the
// computer can read them.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unicode"

	goruntime "runtime"

	"github.com/wailsapp/wails/v2/pkg/options"
	"github.com/wailsapp/wails/v2/pkg/runtime"

	"altnet-relay/engine"
	"altnet-relay/state"
)

// Settings are the few choices the window offers about running in the
// background. They live beside the app's data, one small file.
type Settings struct {
	Background     bool `json:"background"`     // closing the window keeps the app running
	Notifications  bool `json:"notifications"`  // desktop notifications
	ToldBackground bool `json:"toldBackground"` // the "still running" notice has been shown once
	// StartupAsked: starting with the system has been set up once. On Windows
	// the installer asks; elsewhere the app turns it on at its first start
	// (the same default), and after that only the person changes it.
	StartupAsked bool `json:"startupAsked"`
	// ManualUpdates: the person turned automatic updates off. The app then
	// only says when a new version is out. (Stored as "off" so that files
	// from before updates existed mean on.)
	ManualUpdates bool `json:"manualUpdates"`
}

func defaultSettings() Settings { return Settings{Background: true, Notifications: true} }

const settingsFile = "settings.json"

func loadSettings(dir string) Settings {
	s := defaultSettings()
	raw, err := os.ReadFile(filepath.Join(dir, settingsFile))
	if err == nil && json.Unmarshal(raw, &s) != nil {
		// A damaged file means the defaults. (Decoding over the defaults keeps
		// a choice the file doesn't mention, and unknown fields are ignored.)
		s = defaultSettings()
	}
	return s
}

func saveSettings(dir string, s Settings) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	blob, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	tmp := filepath.Join(dir, settingsFile+".tmp")
	if err := os.WriteFile(tmp, blob, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(dir, settingsFile))
}

// SettingsView is what the Account dialog shows.
type SettingsView struct {
	Background       bool   `json:"background"`
	Notifications    bool   `json:"notifications"`
	Startup          bool   `json:"startup"`          // starts with Windows
	StartupAvailable bool   `json:"startupAvailable"` // this system can do that
	TrayAvailable    bool   `json:"trayAvailable"`    // there is a tray icon to bring the window back
	AutoUpdate       bool   `json:"autoUpdate"`       // install updates by themselves
	Version          string `json:"version"`          // this version of the app
}

// background is the app's state for running without a window.
type background struct {
	setMu    sync.Mutex
	settings Settings

	focused  atomic.Bool // the window is being looked at
	quitting atomic.Bool // the app is really closing, not hiding
	wipe     atomic.Bool // the account was deleted: erase the data folder on the way out
	trayOK   atomic.Bool
	trayStop func()

	alertMu   sync.Mutex
	lastAlert map[string]time.Time
}

// alertEvery limits desktop notifications for one conversation: a burst of
// messages is one notification, not twenty.
const alertEvery = 15 * time.Second

func (a *App) bgSettings() Settings {
	a.bg.setMu.Lock()
	defer a.bg.setMu.Unlock()
	return a.bg.settings
}

// ─── bound methods ───

// Settings returns the background choices for the Account dialog.
func (a *App) Settings() SettingsView {
	s := a.bgSettings()
	avail := startupAvailable()
	return SettingsView{
		Background: s.Background, Notifications: s.Notifications,
		Startup: avail && startupEnabled(), StartupAvailable: avail, TrayAvailable: a.bg.trayOK.Load(),
		AutoUpdate: !s.ManualUpdates, Version: appVersion,
	}
}

// SetSetting changes one choice: "background", "notifications" or "startup".
func (a *App) SetSetting(name string, on bool) error {
	if name == "startup" {
		return setStartup(on)
	}
	a.bg.setMu.Lock()
	defer a.bg.setMu.Unlock()
	switch name {
	case "background":
		a.bg.settings.Background = on
	case "notifications":
		a.bg.settings.Notifications = on
	case "autoUpdate":
		a.bg.settings.ManualUpdates = !on
	default:
		return fmt.Errorf("unknown setting %q", name)
	}
	return saveSettings(a.cfg.DataDir, a.bg.settings)
}

// SetFocused is how the window says whether it is being looked at: in front
// and visible. While it is, nothing is shown on the desktop.
func (a *App) SetFocused(focused bool) { a.bg.focused.Store(focused) }

// Quit closes the app for real: the tray icon goes, and so does the node.
func (a *App) Quit() {
	a.bg.quitting.Store(true)
	runtime.Quit(a.ctx)
}

// TestNotification sends one notification now, even with the window in
// front, and says what happened -- a person with no notifications needs to
// know whether the app or Windows is the one not showing them.
func (a *App) TestNotification() (string, error) {
	if !a.bgSettings().Notifications {
		return "", errors.New("notifications are turned off above; turn them on first")
	}
	if err := a.toast("Notifications are working. This is what a message looks like.", ""); err != nil {
		return "", fmt.Errorf("Windows wouldn't show it: %w", err)
	}
	return "Sent. If nothing appeared, Windows may be hiding it: check Settings → System → Notifications (Focus assist or Do not disturb can silence it).", nil
}

// ─── the window closing, opening ───

// quitOnSignals makes a request to stop -- what Linux and macOS send when the
// person logs out or the computer shuts down, or Ctrl+C in a terminal -- really
// stop the app, rather than being taken for the close button and kept
// running, which would make the system wait on it.
func (a *App) quitOnSignals() {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	<-ch
	a.bg.quitting.Store(true)
	runtime.Quit(a.ctx)
}

// firstStartDefaults turns on starting with the system the first time the app
// runs outside Windows -- the same default the Windows installer offers.
func (a *App) firstStartDefaults() {
	if goruntime.GOOS == "windows" || !startupAvailable() {
		return
	}
	a.bg.setMu.Lock()
	defer a.bg.setMu.Unlock()
	if a.bg.settings.StartupAsked {
		return
	}
	if setStartup(true) == nil {
		a.bg.settings.StartupAsked = true
		_ = saveSettings(a.cfg.DataDir, a.bg.settings)
	}
}

// beforeClose runs when the window's close button is pressed.
func (a *App) beforeClose(_ context.Context) bool {
	if a.bg.quitting.Load() {
		return false
	}
	if goruntime.GOOS == "darwin" {
		// On a Mac the close button hides the app by itself (main.go:
		// HideWindowOnClose) and the Dock icon brings it back, as with any
		// Mac app. What reaches here is quitting -- Cmd+Q, the menu, logging
		// out -- and that must quit, or macOS would cancel the logout.
		return false
	}
	if !a.bgSettings().Background {
		return false // closing really closes
	}
	if !a.bg.trayOK.Load() {
		// Nothing would bring a hidden window back, so keep it in the taskbar.
		runtime.WindowMinimise(a.ctx)
		a.bg.focused.Store(false)
		return true
	}
	runtime.WindowHide(a.ctx)
	a.bg.focused.Store(false)
	a.upd.hidden.Store(true)
	a.toldAboutBackground()
	return true
}

// toldAboutBackground says, once, that closing the window didn't close the
// app. Without it the first thing someone sees is a window that vanished.
func (a *App) toldAboutBackground() {
	a.bg.setMu.Lock()
	told := a.bg.settings.ToldBackground
	if !told {
		a.bg.settings.ToldBackground = true
		_ = saveSettings(a.cfg.DataDir, a.bg.settings)
	}
	a.bg.setMu.Unlock()
	if !told && a.bgSettings().Notifications {
		go func() {
			_ = a.toast("AltNet Relay is still running, so messages reach you straight away. Open it from the tray icon; Quit is in its menu.", "")
		}()
	}
}

// showWindow brings the window to the front, from the tray, a notification
// or a second launch.
func (a *App) showWindow() {
	a.upd.hidden.Store(false)
	runtime.Show(a.ctx) // a Mac app hidden by its close button comes back
	runtime.WindowShow(a.ctx)
	runtime.WindowUnminimise(a.ctx)
	runtime.WindowSetAlwaysOnTop(a.ctx, true) // the usual way to get in front of other windows
	runtime.WindowSetAlwaysOnTop(a.ctx, false)
}

// onSecondInstance runs in the already-open app when someone launches it
// again (double-clicking the icon twice, or clicking a notification after a
// restart). Bring this window forward instead of starting a second copy --
// unless the second launch is Windows starting the app at sign-in, which has
// nothing to show.
func (a *App) onSecondInstance(data options.SecondInstanceData) {
	for _, arg := range data.Args {
		if arg == backgroundFlag {
			return
		}
	}
	a.showWindow()
}

// openFromToast is a notification being clicked: show the window, on that
// conversation.
func (a *App) openFromToast(result runtime.NotificationResult) {
	if result.Error != nil {
		return
	}
	a.showWindow()
	if id, ok := result.Response.UserInfo["peerId"].(string); ok && id != "" {
		runtime.EventsEmit(a.ctx, "open", map[string]string{"peerId": id})
	}
}

// ─── notifications ───

// toast shows one desktop notification. Clicking it opens peerID (if given).
func (a *App) toast(body, peerID string) error {
	a.notifyOnce.Do(func() {
		a.notifyErr = runtime.InitializeNotifications(a.ctx)
		a.notifyOn = a.notifyErr == nil
		if a.notifyOn && goruntime.GOOS == "darwin" {
			// macOS asks the person once; until they allow it, nothing shows.
			if ok, err := runtime.RequestNotificationAuthorization(a.ctx); err != nil || !ok {
				a.notifyErr = errors.New("notifications aren't allowed for AltNet Relay: turn them on in System Settings → Notifications")
				a.notifyOn = false
			}
		}
		if a.notifyOn {
			runtime.OnNotificationResponse(a.ctx, a.openFromToast)
			nameToasts()
		}
	})
	if a.notifyErr != nil {
		return a.notifyErr
	}
	data := map[string]interface{}{}
	if peerID != "" {
		data["peerId"] = peerID
	}
	return runtime.SendNotification(a.ctx, runtime.NotificationOptions{
		ID:    fmt.Sprintf("relay-%d", time.Now().UnixNano()),
		Title: "AltNet Relay",
		Body:  toastText(body),
		Data:  data,
	})
}

// maxToastText bounds a notification's text.
const maxToastText = 200

// toastText keeps only plain characters in a notification's text. Group,
// workspace and channel names are chosen by whoever sends the invitation, and
// on Windows the notification library puts the text into XML without escaping
// it; when Windows rejects that XML, the library falls back to a PowerShell
// script in which "$(...)" runs as a command. So letters, digits, spaces and
// harmless punctuation only: no $ ` " < > & [ ], and no line breaks.
func toastText(s string) string {
	var b strings.Builder
	space := false
	n := 0
	for _, r := range s {
		if n >= maxToastText {
			break
		}
		ok := unicode.IsLetter(r) || unicode.IsDigit(r) || strings.ContainsRune(".,:;!?'-#@()/·", r)
		if !ok {
			// Anything else, whitespace included, becomes one space.
			if !space && b.Len() > 0 {
				b.WriteByte(' ')
				space = true
				n++
			}
			continue
		}
		b.WriteRune(r)
		space = false
		n++
	}
	return strings.TrimSpace(b.String())
}

// shouldAlert: notifications are on and the window isn't being looked at.
func (a *App) shouldAlert() bool {
	return a.bgSettings().Notifications && !a.bg.focused.Load()
}

// alert shows a notification for peerID (a person's username, or "g:" and a
// group's ID) unless one was shown for it a moment ago. It runs in its own
// goroutine: showing a notification can take a while, and the caller is the
// engine, in the middle of receiving.
func (a *App) alert(peerID, body string, throttle bool) {
	if !a.shouldAlert() {
		return
	}
	if throttle {
		a.bg.alertMu.Lock()
		if a.bg.lastAlert == nil {
			a.bg.lastAlert = map[string]time.Time{}
		}
		if time.Since(a.bg.lastAlert[peerID]) < alertEvery {
			a.bg.alertMu.Unlock()
			return
		}
		a.bg.lastAlert[peerID] = time.Now()
		a.bg.alertMu.Unlock()
	}
	go func() { _ = a.toast(body, peerID) }()
}

// summaryFor finds one person's row in the list.
func (a *App) summaryFor(peerID string) (engine.Summary, bool) {
	if e := a.engine(); e != nil {
		for _, s := range e.Conversations() {
			if s.PeerID == peerID {
				return s, true
			}
		}
	}
	return engine.Summary{}, false
}

// alertMessage: someone wrote. A first message from a stranger is a request
// and gets its own notification (alertRequest), not this one.
func (a *App) alertMessage(peerID string) {
	if !a.shouldAlert() {
		return
	}
	s, ok := a.summaryFor(peerID)
	if !ok || s.Request || s.Blocked || s.Banned {
		return
	}
	a.alert(peerID, "New message from "+s.Name, true)
}

func (a *App) alertRequest(peerID string) {
	if s, ok := a.summaryFor(peerID); ok && !s.Blocked && !s.Banned {
		a.alert(peerID, s.Name+" wants to message you", true)
	}
}

// alertGroupMessage: someone wrote in a group, workspace or channel.
func (a *App) alertGroupMessage(groupID string) {
	e := a.engine()
	if e == nil || !a.shouldAlert() {
		return
	}
	g, ok := e.State.GroupMeta(groupID)
	if !ok {
		return
	}
	body := "New message in " + g.Name
	switch g.Kind {
	case state.KindWorkspace:
		body = "New message in " + g.Name + " (general)"
	case state.KindChannel:
		if ws, ok := e.State.GroupMeta(g.WS); ok {
			body = "New message in #" + g.Name + " · " + ws.Name
		}
	}
	a.alert(groupPrefix+groupID, body, true)
}

// alertInvited: someone invited us to a group or workspace.
func (a *App) alertInvited(groupID string) {
	e := a.engine()
	if e == nil || !a.shouldAlert() {
		return
	}
	g, ok := e.State.GroupMeta(groupID)
	if !ok {
		return
	}
	// Named by who sent it, not by the group's name: anyone can invite, and
	// the name is whatever they typed. The window shows it once opened.
	body := e.Name(g.InvitedBy) + " invited you to a group"
	if g.Kind == state.KindWorkspace {
		body = e.Name(g.InvitedBy) + " invited you to a workspace"
	}
	a.alert(groupPrefix+groupID, body, false)
}

// alertReport: someone reported a coworker to us, as owner of a workspace. The
// notification names neither of them -- only where to look.
func (a *App) alertReport(groupID string) {
	e := a.engine()
	if e == nil || !a.shouldAlert() {
		return
	}
	if g, ok := e.State.GroupMeta(groupID); ok {
		a.alert(groupPrefix+groupID, "New report in "+g.Name+" -- open People to read it", true)
	}
}

// alertAdded: we were added to a channel.
func (a *App) alertAdded(groupID string) {
	e := a.engine()
	if e == nil || !a.shouldAlert() {
		return
	}
	g, ok := e.State.GroupMeta(groupID)
	if !ok {
		return
	}
	body := "You were added to #" + g.Name
	if ws, ok := e.State.GroupMeta(g.WS); ok {
		body += " · " + ws.Name
	}
	a.alert(groupPrefix+groupID, body, false)
}

// alertCall: a call is ringing. Only the opening offer or ring: answers,
// hang-ups and the like are the window's business.
func (a *App) alertCall(sig engine.CallSignal) {
	e := a.engine()
	if e == nil || !a.shouldAlert() {
		return
	}
	if sig.Group == "" {
		if sig.Op == engine.CallOffer && !e.IsBlocked(sig.PeerID) {
			a.alert(sig.PeerID, e.Name(sig.PeerID)+" is calling", false)
		}
		return
	}
	if sig.Op != engine.GroupRing {
		return
	}
	if g, ok := e.State.GroupMeta(sig.Group); ok && !e.IsBlocked(sig.PeerID) {
		a.alert(groupPrefix+sig.Group, e.Name(sig.PeerID)+" started a call in "+strings.TrimSpace(g.Name), false)
	}
}
