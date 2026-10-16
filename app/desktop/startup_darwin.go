//go:build darwin

package main

// Starting with macOS: a LaunchAgent in the user's own Library, which opens
// the app (minimised, with --background) when they log in. No administrator
// needed; macOS lists it under Login Items, where it can also be turned off.

import (
	"os"
	"path/filepath"
	"strings"
)

const launchAgentLabel = "org.panmox.altnetrelay"

func launchAgentPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "Library", "LaunchAgents", launchAgentLabel+".plist"), nil
}

func startupAvailable() bool { return true }

func startupEnabled() bool {
	p, err := launchAgentPath()
	if err != nil {
		return false
	}
	_, err = os.Stat(p)
	return err == nil
}

// appBundle is the .app the program runs from, if it does.
func appBundle() string {
	exe, err := os.Executable()
	if err != nil {
		return ""
	}
	if i := strings.Index(exe, ".app/Contents/MacOS/"); i >= 0 {
		return exe[:i+len(".app")]
	}
	return ""
}

func setStartup(on bool) error {
	p, err := launchAgentPath()
	if err != nil {
		return err
	}
	if !on {
		if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}
	// Through `open`, so macOS starts it as an app (with its Dock icon), not
	// as a bare process.
	args := []string{"/usr/bin/open", "-a", appBundle(), "--args", backgroundFlag}
	if appBundle() == "" {
		exe, err := os.Executable()
		if err != nil {
			return err
		}
		args = []string{exe, backgroundFlag}
	}
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>` + launchAgentLabel + `</string>
	<key>ProgramArguments</key>
	<array>
`)
	for _, a := range args {
		b.WriteString("\t\t<string>" + xmlEscape(a) + "</string>\n")
	}
	b.WriteString(`	</array>
	<key>RunAtLoad</key>
	<true/>
	<key>ProcessType</key>
	<string>Interactive</string>
</dict>
</plist>
`)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	return os.WriteFile(p, []byte(b.String()), 0o644)
}

func xmlEscape(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;").Replace(s)
}
