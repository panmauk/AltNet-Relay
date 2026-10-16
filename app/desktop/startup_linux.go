//go:build linux

package main

// Starting with Linux: an XDG autostart entry in the user's own config, which
// every common desktop (GNOME, KDE, Xfce, Cinnamon...) runs at login. The app
// opens minimised (--background).

import (
	"os"
	"path/filepath"
	"strings"
)

func autostartPath() (string, error) {
	dir := os.Getenv("XDG_CONFIG_HOME")
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		dir = filepath.Join(home, ".config")
	}
	return filepath.Join(dir, "autostart", "altnet-relay.desktop"), nil
}

func startupAvailable() bool { return true }

func startupEnabled() bool {
	p, err := autostartPath()
	if err != nil {
		return false
	}
	_, err = os.Stat(p)
	return err == nil
}

func setStartup(on bool) error {
	p, err := autostartPath()
	if err != nil {
		return err
	}
	if !on {
		if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	// Exec quoting per the desktop entry spec: double quotes, with \ " ` $
	// escaped inside.
	quoted := `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`, "`", "\\`", `$`, `\$`).Replace(exe) + `"`
	entry := "[Desktop Entry]\n" +
		"Type=Application\n" +
		"Name=AltNet Relay\n" +
		"Comment=Private messages and calls on AltNet\n" +
		"Exec=" + quoted + " " + backgroundFlag + "\n" +
		"Icon=altnet-relay\n" +
		"Terminal=false\n" +
		"X-GNOME-Autostart-enabled=true\n"
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	return os.WriteFile(p, []byte(entry), 0o644)
}
