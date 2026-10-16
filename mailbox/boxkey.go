// Package mailbox implements AltNet Relay's chat data layer on top of the
// existing AltNet network: the same nodes, the same core/dht, the same
// core/crypto peer identities. It adds no new network infrastructure --
// only new record types (Note, Knock) stored through the DHT's existing
// generic Store/Get, the same way core/name stores NameRecords.
//
// See DESIGN-NOTES.md (in the repo root) for the full design reasoning.
// In short:
//
//   - A message is end-to-end encrypted, then chunked and stored
//     content-addressed, same convention as apps/files (see chunk.go,
//     not yet ported into this package).
//   - A Note is a signed, hash-chained pointer at MailboxKey(sender,
//     recipient) saying "here are the chunks for my latest message to
//     you, and here's the previous Note in this chain" -- lets an
//     offline recipient catch up on everything they missed.
//   - A Knock is the same idea at DoorbellKey(recipient) for first
//     contact: unlike a Note, anyone may append one (it's a corkboard,
//     not a single-owner slot), chained the same way.
package mailbox

import (
	"crypto/ecdh"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
)

// BoxKey is a stable X25519 keypair used for asynchronous encryption --
// sealing a message to someone who isn't online to run a live handshake.
//
// This is deliberately separate from the Ed25519 signing identity in
// altnet/core/crypto and from the ephemeral X25519 keys core/secure
// generates fresh per live connection. Those ephemeral keys buy forward
// secrecy for a *session* but can't exist before the recipient is online
// to generate their half; Seal/Open here needs a key that's already
// published and stable, at the cost of no forward secrecy against a
// sender who later loses their own private key (a lesser concern for
// store-and-forward chat than session compromise would be).
type BoxKey struct {
	Private *ecdh.PrivateKey
	Public  *ecdh.PublicKey
}

// NewBoxKey generates a fresh random X25519 keypair.
func NewBoxKey() (*BoxKey, error) {
	priv, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("mailbox: generate box key: %w", err)
	}
	return &BoxKey{Private: priv, Public: priv.PublicKey()}, nil
}

// Save writes the private key to path, hex-encoded, mode 0600 -- same
// shape as core/crypto.Identity.Save.
func (k *BoxKey) Save(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("mailbox: create key dir: %w", err)
	}
	data := []byte(hex.EncodeToString(k.Private.Bytes()))
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return fmt.Errorf("mailbox: write box key: %w", err)
	}
	return nil
}

// LoadBoxKey reads a private key previously written by Save.
func LoadBoxKey(path string) (*BoxKey, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("mailbox: read box key: %w", err)
	}
	raw, err := hex.DecodeString(string(data))
	if err != nil {
		return nil, fmt.Errorf("mailbox: decode box key: %w", err)
	}
	priv, err := ecdh.X25519().NewPrivateKey(raw)
	if err != nil {
		return nil, fmt.Errorf("mailbox: parse box key: %w", err)
	}
	return &BoxKey{Private: priv, Public: priv.PublicKey()}, nil
}

// LoadOrCreateBoxKey loads path if it exists, otherwise generates and
// saves a fresh key -- the typical node startup entry point, same
// pattern as core/crypto.LoadOrCreate.
func LoadOrCreateBoxKey(path string) (*BoxKey, error) {
	if _, err := os.Stat(path); err == nil {
		return LoadBoxKey(path)
	}
	k, err := NewBoxKey()
	if err != nil {
		return nil, err
	}
	if err := k.Save(path); err != nil {
		return nil, err
	}
	return k, nil
}

// BoxPublicKeyFromHex parses a hex-encoded X25519 public key, as carried
// in a Knock's BoxKey field.
func BoxPublicKeyFromHex(s string) (*ecdh.PublicKey, error) {
	raw, err := hex.DecodeString(s)
	if err != nil {
		return nil, fmt.Errorf("mailbox: decode box public key: %w", err)
	}
	return ecdh.X25519().NewPublicKey(raw)
}

// BoxPublicKeyToHex is the inverse of BoxPublicKeyFromHex.
func BoxPublicKeyToHex(pub *ecdh.PublicKey) string {
	return hex.EncodeToString(pub.Bytes())
}
