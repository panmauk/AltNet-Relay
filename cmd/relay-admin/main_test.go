package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"altnet/core/crypto"
)

func TestKeygen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "operator.key")
	var out bytes.Buffer
	if err := cmdKeygen([]string{"-out", path}, &out); err != nil {
		t.Fatal(err)
	}
	id, err := crypto.Load(path)
	if err != nil {
		t.Fatalf("the key must load with crypto.Load: %v", err)
	}
	// Stdout is the public key and nothing else.
	if got, want := out.String(), crypto.PublicKeyToHex(id.PublicKey)+"\n"; got != want {
		t.Fatalf("stdout = %q, want only the public key", got)
	}
	raw, _ := os.ReadFile(path)
	if strings.Contains(out.String(), string(raw)[:64]) {
		t.Fatal("private key material reached stdout")
	}

	// A second run must not replace it.
	out.Reset()
	if err := cmdKeygen([]string{"-out", path}, &out); err == nil {
		t.Fatal("keygen overwrote an existing key")
	}
	if again, _ := os.ReadFile(path); !bytes.Equal(again, raw) || out.Len() != 0 {
		t.Fatal("a refused keygen changed the file or printed something")
	}
}
