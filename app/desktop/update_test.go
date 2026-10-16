package main

import (
	"os"
	"path/filepath"
	goruntime "runtime"
	"testing"
)

// A file in the data folder alone must never make the app run something: a
// waiting update is only installed if its notice verifies with the release key.
func TestAWaitingUpdateMustBeSigned(t *testing.T) {
	if goruntime.GOOS != "windows" {
		t.Skip("updates install themselves only on Windows")
	}
	dir := t.TempDir()
	upd := filepath.Join(dir, updatesDir)
	os.MkdirAll(upd, 0o700)
	fake := `{"body":"{\"version\":\"99.0.0\",\"files\":{\"windows-amd64\":{\"name\":\"evil.exe\",\"url\":\"https://x/evil.exe\",\"size\":4,\"sha256\":\"88d4266fd4e6338d13b845fcf289579d209c897823b9217da3e161936f031589\"}}}","sig":"00"}`
	os.WriteFile(filepath.Join(upd, pendingFile), []byte(fake), 0o600)
	os.WriteFile(filepath.Join(upd, "evil.exe"), []byte("abcd"), 0o600)
	t.Setenv("RELAY_NETWORK", "")
	if installPendingAtStart(dir, false) {
		t.Fatal("an unsigned waiting update was run")
	}
	if _, err := os.Stat(upd); !os.IsNotExist(err) {
		t.Fatal("an unsigned waiting update should be cleared away")
	}
}
