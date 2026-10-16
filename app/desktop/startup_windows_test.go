//go:build windows

package main

import (
	"strings"
	"testing"

	"golang.org/x/sys/windows/registry"
)

// Starting with Windows is one registry value. This touches the real one for a
// moment and puts back whatever was there, so it doesn't undo a person's
// choice made through the installer.
func TestStartupWithWindowsRoundTrip(t *testing.T) {
	k, err := registry.OpenKey(registry.CURRENT_USER, runKey, registry.QUERY_VALUE|registry.SET_VALUE)
	if err != nil {
		t.Skip("no per-user Run key to test against")
	}
	defer k.Close()
	before, _, had := func() (string, uint32, bool) {
		v, typ, err := k.GetStringValue(runValue)
		return v, typ, err == nil
	}()
	t.Cleanup(func() {
		if had {
			_ = k.SetStringValue(runValue, before)
		} else {
			_ = k.DeleteValue(runValue)
		}
	})

	if err := setStartup(true); err != nil {
		t.Fatal(err)
	}
	if !startupEnabled() {
		t.Fatal("after turning it on, startup should be on")
	}
	v, _, err := k.GetStringValue(runValue)
	if err != nil || !strings.HasSuffix(v, " "+backgroundFlag) || !strings.HasPrefix(v, `"`) {
		t.Fatalf("Windows should start the app, quoted, with %s: %q %v", backgroundFlag, v, err)
	}
	if err := setStartup(false); err != nil {
		t.Fatal(err)
	}
	if startupEnabled() {
		t.Fatal("after turning it off, startup should be off")
	}
	if err := setStartup(false); err != nil {
		t.Fatalf("turning it off twice is fine: %v", err)
	}
}
