// Package backup locks a copy of a Relay account -- its keys and its saved
// state -- with the account's password, so the account can be signed in to on
// another computer and carry on there.
//
// The copy is locked on the person's own computer before it is sent anywhere:
// the server only ever holds the locked bytes and cannot open them without
// the password. The lock is AES-256-GCM under a key stretched from the
// password with PBKDF2-SHA256 (600,000 rounds, a random salt per key), so
// guessing a password against a stolen copy is slow -- but a weak password is
// still guessable, which the app's wording says plainly.
//
// Layout of a locked copy:
//
//	"ANRB1" | rounds (4 bytes, big-endian) | salt (16) | nonce (12) | sealed payload
//
// The header is bound to the seal as additional data, so it can't be swapped.
package backup

import (
	"bytes"
	"compress/gzip"
	"crypto/aes"
	"crypto/cipher"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

const (
	magic = "ANRB1"
	// Rounds is the PBKDF2 work factor for new keys (OWASP's figure for
	// PBKDF2-HMAC-SHA256).
	Rounds = 600_000
	// MaxSize bounds a locked copy; the server refuses larger ones too.
	MaxSize = 8 << 20

	saltLen   = 16
	nonceLen  = 12
	header    = len(magic) + 4 + saltLen
	minRounds = 100_000
	maxRounds = 10_000_000
)

// ErrWrongPassword means the copy didn't open with that password: a typo, or
// a copy locked before the password was changed.
var ErrWrongPassword = errors.New("backup: the password doesn't open this copy")

// ErrDamaged means the bytes aren't a locked copy at all.
var ErrDamaged = errors.New("backup: not a readable copy")

// Key is a password-derived key, kept on the computer so the copy can be
// refreshed without asking for the password again. It is as sensitive as the
// account's own keys, which sit beside it.
type Key struct {
	Salt   []byte `json:"salt"`
	Rounds int    `json:"rounds"`
	K      []byte `json:"k"`
}

// NewKey derives a key from the password with a fresh random salt.
func NewKey(password string) (*Key, error) {
	salt := make([]byte, saltLen)
	if _, err := rand.Read(salt); err != nil {
		return nil, err
	}
	return derive(password, salt, Rounds)
}

func derive(password string, salt []byte, rounds int) (*Key, error) {
	if password == "" {
		return nil, errors.New("backup: no password")
	}
	k, err := pbkdf2.Key(sha256.New, password, salt, rounds, 32)
	if err != nil {
		return nil, err
	}
	return &Key{Salt: append([]byte(nil), salt...), Rounds: rounds, K: k}, nil
}

// Payload is what a copy carries: the files that make the account.
type Payload struct {
	Version  int    `json:"version"`
	RelayKey []byte `json:"relay_key"` // the Relay identity (Ed25519)
	RelayBox []byte `json:"relay_box"` // its encryption key (X25519)
	State    []byte `json:"state"`     // state.json: contacts, chats, groups
	Saved    int64  `json:"saved"`     // unix seconds, when the copy was made
}

// Lock seals a payload with the key.
func (k *Key) Lock(p *Payload) ([]byte, error) {
	plain, err := json.Marshal(p)
	if err != nil {
		return nil, err
	}
	var zipped bytes.Buffer
	zw := gzip.NewWriter(&zipped)
	if _, err := zw.Write(plain); err != nil {
		return nil, err
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	gcm, err := newGCM(k.K)
	if err != nil {
		return nil, err
	}
	head := make([]byte, header)
	copy(head, magic)
	binary.BigEndian.PutUint32(head[len(magic):], uint32(k.Rounds))
	copy(head[len(magic)+4:], k.Salt)
	nonce := make([]byte, nonceLen)
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	out := append(append(head, nonce...), gcm.Seal(nil, nonce, zipped.Bytes(), head)...)
	if len(out) > MaxSize {
		return nil, fmt.Errorf("backup: this account is too large to copy (%d MB)", len(out)>>20)
	}
	return out, nil
}

// Open unlocks a copy with the password. It also returns the key, so the
// computer that opened it can keep it for later refreshes.
func Open(blob []byte, password string) (*Payload, *Key, error) {
	if len(blob) < header+nonceLen+16 || string(blob[:len(magic)]) != magic {
		return nil, nil, ErrDamaged
	}
	rounds := int(binary.BigEndian.Uint32(blob[len(magic):]))
	if rounds < minRounds || rounds > maxRounds {
		return nil, nil, ErrDamaged
	}
	salt := blob[len(magic)+4 : header]
	k, err := derive(password, salt, rounds)
	if err != nil {
		return nil, nil, err
	}
	p, err := k.open(blob)
	if err != nil {
		return nil, nil, err
	}
	return p, k, nil
}

func (k *Key) open(blob []byte) (*Payload, error) {
	gcm, err := newGCM(k.K)
	if err != nil {
		return nil, err
	}
	head, nonce, sealed := blob[:header], blob[header:header+nonceLen], blob[header+nonceLen:]
	zipped, err := gcm.Open(nil, nonce, sealed, head)
	if err != nil {
		return nil, ErrWrongPassword
	}
	zr, err := gzip.NewReader(bytes.NewReader(zipped))
	if err != nil {
		return nil, ErrDamaged
	}
	plain, err := io.ReadAll(io.LimitReader(zr, 64<<20))
	if err != nil {
		return nil, ErrDamaged
	}
	var p Payload
	if err := json.Unmarshal(plain, &p); err != nil || len(p.RelayKey) == 0 || len(p.RelayBox) == 0 {
		return nil, ErrDamaged
	}
	return &p, nil
}

func newGCM(k []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(k)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}
