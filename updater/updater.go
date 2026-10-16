// Package updater finds, fetches and checks new versions of the app.
//
// A release is described by a manifest signed with the release key (an
// Ed25519 key kept offline by the operator; its public half is built into the
// app). The app never runs anything the manifest doesn't name, with the exact
// size and SHA-256 the manifest gives, and never goes back to an older
// version. So whoever controls the download host -- GitHub, or anything in
// between -- can't push a fake or old version.
//
// The manifest is fetched from our own server (relay-server mirrors it from
// the latest GitHub release), so checking doesn't tell GitHub about every
// running app; only an actual download goes to GitHub.
package updater

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Platform names in a manifest.
const (
	Windows  = "windows-amd64"   // the installer; the app installs it itself
	LinuxDeb = "linux-deb-amd64" // Ubuntu / Debian package
	LinuxTar = "linux-tar-amd64" // other Linux
	MacKit   = "macos-kit"       // the Mac build kit
)

// MaxManifest bounds a manifest; MaxFile bounds a download.
const (
	MaxManifest = 64 << 10
	MaxFile     = 128 << 20
)

// Envelope is what is published: the manifest's exact bytes and the release
// key's signature over them.
type Envelope struct {
	Body string `json:"body"` // the Manifest, as JSON text
	Sig  string `json:"sig"`  // hex Ed25519 signature over Body
}

// Manifest describes one release.
type Manifest struct {
	Version   string          `json:"version"`   // e.g. "1.0.6"
	Published int64           `json:"published"` // unix seconds
	Notes     string          `json:"notes"`     // a sentence or two for the person
	Page      string          `json:"page"`      // the release's web page
	Files     map[string]File `json:"files"`     // by platform name
}

// File is one download.
type File struct {
	Name   string `json:"name"`
	URL    string `json:"url"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

var (
	ErrBadSignature = errors.New("updater: the release isn't signed by the release key")
	ErrNoUpdate     = errors.New("updater: no newer version")
)

// Sign makes an envelope for a manifest. Used by the release tool.
func Sign(m *Manifest, key ed25519.PrivateKey) (*Envelope, error) {
	body, err := json.Marshal(m)
	if err != nil {
		return nil, err
	}
	return &Envelope{Body: string(body), Sig: hex.EncodeToString(ed25519.Sign(key, body))}, nil
}

// Open checks an envelope against the release key and returns its manifest.
func Open(raw []byte, releaseKey ed25519.PublicKey) (*Manifest, error) {
	var env Envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("updater: unreadable release notice: %w", err)
	}
	sig, err := hex.DecodeString(env.Sig)
	if err != nil || len(releaseKey) != ed25519.PublicKeySize || !ed25519.Verify(releaseKey, []byte(env.Body), sig) {
		return nil, ErrBadSignature
	}
	var m Manifest
	if err := json.Unmarshal([]byte(env.Body), &m); err != nil {
		return nil, fmt.Errorf("updater: unreadable manifest: %w", err)
	}
	if _, ok := parse(m.Version); !ok {
		return nil, fmt.Errorf("updater: bad version %q", m.Version)
	}
	return &m, nil
}

// Check fetches the release notice and returns the manifest if it is newer
// than current. ErrNoUpdate means there's nothing to do.
func Check(ctx context.Context, client *http.Client, noticeURL, current string, releaseKey ed25519.PublicKey) (*Manifest, error) {
	m, _, err := CheckNotice(ctx, client, noticeURL, current, releaseKey)
	return m, err
}

// CheckNotice is Check, also returning the signed notice itself, so it can be
// kept and verified again later (before a downloaded update is installed).
func CheckNotice(ctx context.Context, client *http.Client, noticeURL, current string, releaseKey ed25519.PublicKey) (*Manifest, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, noticeURL, nil)
	if err != nil {
		return nil, nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, nil, ErrNoUpdate // nothing published yet
	}
	if resp.StatusCode != http.StatusOK {
		return nil, nil, fmt.Errorf("updater: the update server answered %d", resp.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, MaxManifest+1))
	if err != nil {
		return nil, nil, err
	}
	if len(raw) > MaxManifest {
		return nil, nil, errors.New("updater: release notice too large")
	}
	m, err := Open(raw, releaseKey)
	if err != nil {
		return nil, nil, err
	}
	if !Newer(m.Version, current) {
		return nil, nil, ErrNoUpdate
	}
	return m, raw, nil
}

// Newer reports whether version a is newer than b ("1.0.10" > "1.0.9").
func Newer(a, b string) bool {
	va, ok1 := parse(a)
	vb, ok2 := parse(b)
	if !ok1 || !ok2 {
		return false
	}
	for i := range va {
		if va[i] != vb[i] {
			return va[i] > vb[i]
		}
	}
	return false
}

func parse(v string) ([3]int, bool) {
	var out [3]int
	parts := strings.Split(strings.TrimPrefix(strings.TrimSpace(v), "v"), ".")
	if len(parts) != 3 {
		return out, false
	}
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 || n > 99999 {
			return out, false
		}
		out[i] = n
	}
	return out, true
}

// allowedURL accepts https downloads, and plain http only to this computer
// (for testing). The signature already pins the bytes; this is belt and
// braces against sending the request somewhere odd.
func allowedURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	switch u.Scheme {
	case "https":
		return u.Host != ""
	case "http":
		h := u.Hostname()
		return h == "127.0.0.1" || h == "localhost" || h == "::1"
	}
	return false
}

// Download fetches a file into dir and checks its size and SHA-256 against
// the manifest. A file already there and correct isn't fetched again. It
// returns the path of the checked file.
func Download(ctx context.Context, client *http.Client, f File, dir string) (string, error) {
	if !allowedURL(f.URL) || f.Size <= 0 || f.Size > MaxFile || len(f.SHA256) != 64 {
		return "", errors.New("updater: the manifest's download entry isn't usable")
	}
	name := filepath.Base(f.Name)
	if name == "." || name == "/" || name == "" || strings.ContainsAny(name, `\/:`) {
		return "", errors.New("updater: bad file name in the manifest")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	final := filepath.Join(dir, name)
	if ok, _ := Matches(final, f); ok {
		return final, nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, f.URL, nil)
	if err != nil {
		return "", err
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("updater: download answered %d", resp.StatusCode)
	}
	tmp := final + ".part"
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return "", err
	}
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(out, h), io.LimitReader(resp.Body, f.Size+1))
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err == nil && n != f.Size {
		err = fmt.Errorf("updater: got %d bytes, expected %d", n, f.Size)
	}
	if err == nil && hex.EncodeToString(h.Sum(nil)) != strings.ToLower(f.SHA256) {
		err = errors.New("updater: the download doesn't match the signed fingerprint")
	}
	if err != nil {
		os.Remove(tmp)
		return "", err
	}
	if err := os.Rename(tmp, final); err != nil {
		os.Remove(tmp)
		return "", err
	}
	return final, nil
}

// Matches reports whether the file at path is exactly the manifest's file.
// It is checked again right before an installer runs.
func Matches(path string, f File) (bool, error) {
	fh, err := os.Open(path)
	if err != nil {
		return false, err
	}
	defer fh.Close()
	st, err := fh.Stat()
	if err != nil || st.Size() != f.Size {
		return false, err
	}
	h := sha256.New()
	if _, err := io.Copy(h, fh); err != nil {
		return false, err
	}
	return hex.EncodeToString(h.Sum(nil)) == strings.ToLower(f.SHA256), nil
}

// DefaultClient is used for checks and downloads.
var DefaultClient = &http.Client{Timeout: 10 * time.Minute}
