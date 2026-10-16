//go:build linux

package main

import (
	"os"
	"strings"
	"testing"
)

func TestAutostartEntryRoundTrip(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	if startupEnabled() {
		t.Fatal("nothing set up yet")
	}
	if err := setStartup(true); err != nil {
		t.Fatal(err)
	}
	if !startupEnabled() {
		t.Fatal("after turning it on, it should be on")
	}
	p, _ := autostartPath()
	raw, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	entry := string(raw)
	if !strings.Contains(entry, "[Desktop Entry]") || !strings.Contains(entry, " "+backgroundFlag+"\n") || !strings.Contains(entry, `Exec="`) {
		t.Fatalf("a desktop entry that starts the app minimised: %s", entry)
	}
	if err := setStartup(false); err != nil {
		t.Fatal(err)
	}
	if startupEnabled() {
		t.Fatal("after turning it off, it should be off")
	}
	if err := setStartup(false); err != nil {
		t.Fatalf("turning it off twice is fine: %v", err)
	}
}
