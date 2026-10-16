// Command relay-release makes the signed release notice that tells installed
// apps a new version exists (see package updater).
//
//	relay-release keygen -out release.key
//	    Make the release key, once. Keep it offline and backed up, like the
//	    operator key: whoever holds it can ship an update to every user.
//	    Prints the public half, which is built into the app.
//
//	relay-release sign -key release.key -dist dist -repo panmauk/AltNet-Relay [-notes "…"]
//	    Read the version from app/desktop/wails.json, fingerprint the
//	    installers in dist/, and write dist/update.json. Upload update.json to
//	    the GitHub release together with the installers; relay-server mirrors
//	    the latest one to the apps.
//
//	relay-release verify -file dist/update.json
//	    Check a notice against the release key built into the app.
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"altnet/core/crypto"

	"altnet-relay/updater"
)

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	var err error
	switch os.Args[1] {
	case "keygen":
		err = keygen(os.Args[2:])
	case "sign":
		err = sign(os.Args[2:])
	case "verify":
		err = verify(os.Args[2:])
	default:
		usage()
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "relay-release:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: relay-release keygen -out release.key | sign -key release.key -dist dist -repo owner/name [-notes text] | verify -file update.json")
	os.Exit(2)
}

func keygen(args []string) error {
	fs := flag.NewFlagSet("keygen", flag.ExitOnError)
	out := fs.String("out", "", "where to write the release key")
	fs.Parse(args)
	if *out == "" {
		return fmt.Errorf("-out is required")
	}
	if _, err := os.Stat(*out); err == nil {
		return fmt.Errorf("%s already exists; not overwriting a release key", *out)
	}
	id, err := crypto.NewIdentity()
	if err != nil {
		return err
	}
	if err := id.Save(*out); err != nil {
		return err
	}
	fmt.Println("release key written to", *out)
	fmt.Println("public key (build into the app):", hex.EncodeToString(id.PublicKey))
	return nil
}

// what the notice lists, per platform: a file-name pattern in dist/.
func files(version string) map[string]string {
	return map[string]string{
		updater.Windows:  "AltNetRelay-Setup-" + version + ".exe",
		updater.LinuxDeb: "AltNetRelay_" + version + "_amd64.deb",
		updater.LinuxTar: "AltNetRelay-" + version + "-linux-amd64.tar.gz",
		updater.MacKit:   "AltNetRelay-macOS.zip",
	}
}

func sign(args []string) error {
	fs := flag.NewFlagSet("sign", flag.ExitOnError)
	keyPath := fs.String("key", "", "the release key")
	dist := fs.String("dist", "dist", "folder with the installers")
	repo := fs.String("repo", "panmauk/AltNet-Relay", "GitHub repository the release is published in")
	wails := fs.String("wails", "app/desktop/wails.json", "where the version comes from")
	notes := fs.String("notes", "", "a sentence or two about what's new")
	baseOverride := fs.String("base", "", "download address prefix instead of the GitHub release (for testing)")
	fs.Parse(args)
	if *keyPath == "" {
		return fmt.Errorf("-key is required")
	}
	id, err := crypto.Load(*keyPath)
	if err != nil {
		return fmt.Errorf("release key: %w", err)
	}
	if hex.EncodeToString(id.PublicKey) != updater.ReleaseKeyHex {
		return fmt.Errorf("that key isn't the release key built into the app (%.12s…)", updater.ReleaseKeyHex)
	}
	var w struct {
		Info struct {
			ProductVersion string `json:"productVersion"`
		} `json:"info"`
	}
	raw, err := os.ReadFile(*wails)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(raw, &w); err != nil {
		return err
	}
	version := w.Info.ProductVersion
	tag := "v" + version
	base := "https://github.com/" + *repo + "/releases/download/" + tag + "/"
	if *baseOverride != "" {
		base = strings.TrimRight(*baseOverride, "/") + "/"
	}
	m := &updater.Manifest{
		Version: version, Published: time.Now().Unix(), Notes: strings.TrimSpace(*notes),
		Page:  "https://github.com/" + *repo + "/releases/tag/" + tag,
		Files: map[string]updater.File{},
	}
	for platform, name := range files(version) {
		path := filepath.Join(*dist, name)
		size, sum, err := fingerprint(path)
		if err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		// GitHub replaces spaces in asset names; these names have none.
		m.Files[platform] = updater.File{Name: name, URL: base + name, Size: size, SHA256: sum}
		fmt.Printf("  %-16s %s  %d bytes  %s…\n", platform, name, size, sum[:16])
	}
	env, err := updater.Sign(m, id.PrivateKey)
	if err != nil {
		return err
	}
	blob, err := json.MarshalIndent(env, "", "  ")
	if err != nil {
		return err
	}
	out := filepath.Join(*dist, "update.json")
	if err := os.WriteFile(out, blob, 0o644); err != nil {
		return err
	}
	fmt.Printf("wrote %s for %s. Upload it to the GitHub release %s with the files above.\n", out, version, tag)
	return nil
}

func fingerprint(path string) (int64, string, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, "", err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return 0, "", err
	}
	return n, hex.EncodeToString(h.Sum(nil)), nil
}

func verify(args []string) error {
	fs := flag.NewFlagSet("verify", flag.ExitOnError)
	file := fs.String("file", "dist/update.json", "the notice to check")
	fs.Parse(args)
	raw, err := os.ReadFile(*file)
	if err != nil {
		return err
	}
	m, err := updater.Open(raw, updater.ReleaseKey())
	if err != nil {
		return err
	}
	fmt.Printf("good signature: version %s, %d files\n", m.Version, len(m.Files))
	for p, f := range m.Files {
		fmt.Printf("  %-16s %s\n", p, f.URL)
	}
	return nil
}
