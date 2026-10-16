package main

import (
	"os"
	"path/filepath"
	"testing"

	"altnet-relay/mailbox"
)

func TestWipeOnlyErasesTheAppsOwnFolder(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "AltNetRelay")
	if _, err := mailbox.LoadOrCreateIdentity(dir); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(dir, "state.json"), []byte("{}"), 0o600)

	other := t.TempDir() // a folder without the app's identity in it
	os.WriteFile(filepath.Join(other, "keep.txt"), []byte("mine"), 0o600)
	if wipeDataDir(other) {
		t.Fatal("wiped a folder that isn't the app's")
	}
	if _, err := os.Stat(filepath.Join(other, "keep.txt")); err != nil {
		t.Fatal("someone else's file is gone")
	}
	for _, p := range []string{"/", `C:\`} {
		if safeToWipe(p) {
			t.Fatalf("%s counted as safe", p)
		}
	}
	if home, err := os.UserHomeDir(); err == nil && safeToWipe(home) {
		t.Fatal("the home folder counted as safe")
	}

	if !wipeDataDir(dir) {
		t.Fatal("didn't wipe the app's folder")
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatal("the app's folder is still there")
	}
}
