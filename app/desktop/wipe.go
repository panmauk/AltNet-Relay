package main

import (
	"os"
	"path/filepath"

	"altnet-relay/mailbox"
)

// wipeDataDir erases this computer's copy of everything after the account is
// deleted: the keys, the conversations, the settings. It runs as the app
// quits, after the node has stopped using the files.
//
// The folder can be moved with RELAY_DATA, so it refuses anything that looks
// like more than the app's own folder: a drive root, the home folder, or a
// folder without the app's identity in it.
func wipeDataDir(dir string) bool {
	if !safeToWipe(dir) {
		return false
	}
	return os.RemoveAll(dir) == nil
}

func safeToWipe(dir string) bool {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return false
	}
	abs = filepath.Clean(abs)
	if abs == filepath.Dir(abs) { // a root, like C:\ or /
		return false
	}
	for _, f := range []func() (string, error){os.UserHomeDir, os.UserConfigDir} {
		if p, err := f(); err == nil && filepath.Clean(p) == abs {
			return false
		}
	}
	// Only a folder this app made: it holds the Relay identity.
	_, err = os.Stat(filepath.Join(abs, mailbox.IdentityFileName))
	return err == nil
}
