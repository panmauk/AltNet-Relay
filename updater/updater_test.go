package updater

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestNewer(t *testing.T) {
	for _, c := range []struct {
		a, b string
		want bool
	}{
		{"1.0.6", "1.0.5", true}, {"1.0.10", "1.0.9", true}, {"2.0.0", "1.9.9", true},
		{"1.0.5", "1.0.5", false}, {"1.0.4", "1.0.5", false}, {"v1.1.0", "1.0.9", true},
		{"1.0", "0.9.0", false}, {"x.y.z", "1.0.0", false},
	} {
		if got := Newer(c.a, c.b); got != c.want {
			t.Errorf("Newer(%q, %q) = %v", c.a, c.b, got)
		}
	}
}

func rig(t *testing.T, version string, payload []byte, key ed25519.PrivateKey) (*httptest.Server, *Manifest) {
	t.Helper()
	sum := sha256.Sum256(payload)
	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	m := &Manifest{Version: version, Files: map[string]File{
		Windows: {Name: "AltNetRelay-Setup-" + version + ".exe", URL: srv.URL + "/setup.exe", Size: int64(len(payload)), SHA256: hex.EncodeToString(sum[:])},
	}}
	env, err := Sign(m, key)
	if err != nil {
		t.Fatal(err)
	}
	mux.HandleFunc("/update", func(w http.ResponseWriter, _ *http.Request) { json.NewEncoder(w).Encode(env) })
	mux.HandleFunc("/setup.exe", func(w http.ResponseWriter, _ *http.Request) { w.Write(payload) })
	return srv, m
}

func TestCheckAndDownload(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	payload := []byte("pretend installer bytes")
	srv, _ := rig(t, "1.0.6", payload, priv)
	ctx := context.Background()

	if _, err := Check(ctx, srv.Client(), srv.URL+"/update", "1.0.6", pub); !errors.Is(err, ErrNoUpdate) {
		t.Fatalf("same version: %v", err)
	}
	if _, err := Check(ctx, srv.Client(), srv.URL+"/update", "1.0.7", pub); !errors.Is(err, ErrNoUpdate) {
		t.Fatalf("never go back to an older version: %v", err)
	}
	m, err := Check(ctx, srv.Client(), srv.URL+"/update", "1.0.5", pub)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	path, err := Download(ctx, srv.Client(), m.Files[Windows], dir)
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(path); string(got) != string(payload) {
		t.Fatal("wrong bytes")
	}
	if ok, _ := Matches(path, m.Files[Windows]); !ok {
		t.Fatal("a correct download should match")
	}
	// Changed on disk after the download: caught before it would run.
	os.WriteFile(path, []byte("tampered installer bytes"), 0o600)
	if ok, _ := Matches(path, m.Files[Windows]); ok {
		t.Fatal("a changed file must not match")
	}
}

func TestAnotherKeyIsRefused(t *testing.T) {
	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	_, other, _ := ed25519.GenerateKey(rand.Reader)
	srv, _ := rig(t, "9.9.9", []byte("evil"), other)
	if _, err := Check(context.Background(), srv.Client(), srv.URL+"/update", "1.0.5", pub); !errors.Is(err, ErrBadSignature) {
		t.Fatalf("a notice signed by someone else: %v", err)
	}
}

func TestEditedManifestIsRefused(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	m := &Manifest{Version: "1.0.6", Files: map[string]File{}}
	env, _ := Sign(m, priv)
	env.Body = `{"version":"1.0.7","files":{}}`
	raw, _ := json.Marshal(env)
	if _, err := Open(raw, pub); !errors.Is(err, ErrBadSignature) {
		t.Fatalf("an edited manifest: %v", err)
	}
}

func TestDownloadMustMatchTheFingerprint(t *testing.T) {
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	srv, m := rig(t, "1.0.6", []byte("real"), priv)
	f := m.Files[Windows]
	f.SHA256 = hex.EncodeToString(make([]byte, 32))
	dir := t.TempDir()
	if _, err := Download(context.Background(), srv.Client(), f, dir); err == nil {
		t.Fatal("a download with the wrong fingerprint was accepted")
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Fatal("a bad download was left behind")
	}
	// Only https, or http to this computer.
	f2 := m.Files[Windows]
	f2.URL = "http://example.com/setup.exe"
	if _, err := Download(context.Background(), srv.Client(), f2, dir); err == nil {
		t.Fatal("plain http to the internet was accepted")
	}
	f3 := m.Files[Windows]
	f3.Name = "../evil.exe"
	if p, err := Download(context.Background(), srv.Client(), f3, dir); err == nil && filepath.Dir(p) != dir {
		t.Fatal("a file name escaped the folder")
	}
}

func TestTheBuiltInReleaseKeyIsValid(t *testing.T) {
	if len(ReleaseKey()) != ed25519.PublicKeySize {
		t.Fatal("the release key built into the app is missing or malformed")
	}
}
