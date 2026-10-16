package mailbox

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"altnet/core/crypto"
)

// fuzzRecordSeeds returns one genuine record of each kind, marshaled.
func fuzzRecordSeeds(f *testing.F) [][]byte {
	id, err := crypto.NewIdentity()
	if err != nil {
		f.Fatal(err)
	}
	box, err := NewBoxKey()
	if err != nil {
		f.Fatal(err)
	}
	n, err := NewNote(id, id.ID(), []string{strings.Repeat("ab", 32)}, "", 1)
	if err != nil {
		f.Fatal(err)
	}
	k, err := NewKnock(id, box, id.ID(), n.Hash(), "")
	if err != nil {
		f.Fatal(err)
	}
	p := &Profile{PeerID: id.ID(), BoxKey: BoxPublicKeyToHex(box.Public), Version: 1, Timestamp: 1}
	h := &HandleRecord{Handle: "fuzz", Username: id.ID(), Version: 1, Timestamp: 1, TTL: HandleTTL}
	var out [][]byte
	for _, signer := range []interface{ Sign(*crypto.Identity) error }{n, k, p, h} {
		if err := signer.Sign(id); err != nil {
			f.Fatal(err)
		}
		blob, err := json.Marshal(signer)
		if err != nil {
			f.Fatal(err)
		}
		out = append(out, blob)
	}
	return out
}

// Every record kind parses attacker bytes. Parsing and verifying must
// never panic, and anything that verifies must be the one canonical
// encoding: re-encoding it verifies again under the same hash.
func FuzzRecordVerify(f *testing.F) {
	for _, s := range fuzzRecordSeeds(f) {
		f.Add(s)
	}
	f.Add([]byte(`{"sig":"` + strings.Repeat("A", 128) + `"}`))
	f.Fuzz(func(t *testing.T, data []byte) {
		type record interface {
			Verify() error
		}
		kinds := []func() record{
			func() record { return new(Note) },
			func() record { return new(Knock) },
			func() record { return new(Profile) },
			func() record { return new(HandleRecord) },
		}
		for _, mk := range kinds {
			r := mk()
			if json.Unmarshal(data, r) != nil {
				continue
			}
			if n, ok := r.(*Note); ok {
				_ = checkNoteShape(n)
				_ = n.VerifyFrom(strings.Repeat("0", 64), "")
			}
			if r.Verify() != nil {
				continue
			}
			again, err := json.Marshal(r)
			if err != nil {
				t.Fatal(err)
			}
			r2 := mk()
			if err := json.Unmarshal(again, r2); err != nil {
				t.Fatalf("a verified %T does not round-trip: %v", r, err)
			}
			if err := r2.Verify(); err != nil {
				t.Fatalf("a verified %T fails after re-encoding: %v", r, err)
			}
			if h, ok := r.(interface{ Hash() string }); ok && h.Hash() != r2.(interface{ Hash() string }).Hash() {
				t.Fatalf("a verified %T changed hash on re-encoding", r)
			}
		}
	})
}

// Open takes attacker bytes straight off the network. It must never
// panic, and nothing but the genuine ciphertext may open.
func FuzzOpen(f *testing.F) {
	box, err := NewBoxKey()
	if err != nil {
		f.Fatal(err)
	}
	sealed, err := Seal(box.Public, []byte(`{"id":"x","kind":"text","text":"hi","sent":1}`))
	if err != nil {
		f.Fatal(err)
	}
	f.Add(sealed)
	f.Add([]byte{})
	f.Add(make([]byte, 44))
	f.Fuzz(func(t *testing.T, data []byte) {
		pt, err := Open(box.Private, data)
		if err != nil {
			return
		}
		if !bytes.Equal(data, sealed) {
			t.Fatalf("forged ciphertext opened to %q", pt)
		}
		if _, err := openMessage(pt); err != nil {
			t.Fatalf("genuine message rejected: %v", err)
		}
	})
}

// A handle that validates must canonicalize to plain ASCII that names
// the same key as the input, however it was typed: that is what stops
// look-alike input landing on someone else's key.
func FuzzHandleCanonical(f *testing.F) {
	for _, s := range []string{"rob", "@Rob", "  kate ", "Kate", "a-b_c", "@@x", "İstanbul", "ſam"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		c := CanonicalHandle(s)
		if ValidHandle(s) != nil {
			return
		}
		for i := 0; i < len(c); i++ {
			if b := c[i]; !(b >= 'a' && b <= 'z' || b >= '0' && b <= '9' || b == '-' || b == '_') {
				t.Fatalf("valid handle %q canonicalized to %q", s, c)
			}
		}
		if CanonicalHandle(c) != c || ValidHandle(c) != nil {
			t.Fatalf("canonical form %q of %q is not stable", c, s)
		}
		if HandleKey(s) != HandleKey(c) {
			t.Fatalf("%q and its canonical form name different keys", s)
		}
		// Valid input is ASCII, so asciiLower and ToLower agree on it;
		// if they ever differ, some non-ASCII rune slipped through.
		trimmed := strings.TrimPrefix(strings.TrimSpace(s), "@")
		if strings.ToLower(trimmed) != c {
			t.Fatalf("%q validated with non-ASCII content", s)
		}
	})
}
