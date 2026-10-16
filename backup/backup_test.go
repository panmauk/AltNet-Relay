package backup

import (
	"bytes"
	"errors"
	"testing"
)

func TestLockAndOpen(t *testing.T) {
	k, err := NewKey("correct horse")
	if err != nil {
		t.Fatal(err)
	}
	p := &Payload{Version: 1, RelayKey: []byte("key"), RelayBox: []byte("box"), State: bytes.Repeat([]byte(`{"a":1}`), 1000), Saved: 42}
	blob, err := k.Lock(p)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(blob, []byte(`{"a":1}`)) || bytes.Contains(blob, []byte("relay_key")) {
		t.Fatal("the copy isn't locked")
	}

	got, k2, err := Open(blob, "correct horse")
	if err != nil {
		t.Fatal(err)
	}
	if string(got.RelayKey) != "key" || string(got.RelayBox) != "box" || !bytes.Equal(got.State, p.State) || got.Saved != 42 {
		t.Fatal("the copy didn't come back the same")
	}
	if !bytes.Equal(k2.K, k.K) {
		t.Fatal("opening should yield the same key, so the copy can be refreshed")
	}

	if _, _, err := Open(blob, "wrong"); !errors.Is(err, ErrWrongPassword) {
		t.Fatalf("wrong password: %v", err)
	}
	// Tampering with anything, header included, is caught.
	for _, i := range []int{2, 7, len(blob) / 2, len(blob) - 1} {
		bad := append([]byte(nil), blob...)
		bad[i] ^= 1
		if _, _, err := Open(bad, "correct horse"); err == nil {
			t.Fatalf("a change at byte %d went unnoticed", i)
		}
	}
	if _, _, err := Open([]byte("hello"), "x"); !errors.Is(err, ErrDamaged) {
		t.Fatalf("garbage: %v", err)
	}
}

func TestEachKeyHasItsOwnSalt(t *testing.T) {
	a, _ := NewKey("same")
	b, _ := NewKey("same")
	if bytes.Equal(a.Salt, b.Salt) || bytes.Equal(a.K, b.K) {
		t.Fatal("two keys from the same password must differ")
	}
}
