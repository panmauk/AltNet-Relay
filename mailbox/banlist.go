package mailbox

import (
	"bufio"
	"os"
	"strings"
	"sync"

	"altnet/core/crypto"
)

// BanList is a set of Ed25519 identity public keys (hex) whose records
// this node refuses to accept or surface.
//
// It is deliberately the same shape as dht.Blocklist -- which bans
// content hashes -- because it is the identity-flavoured half of the
// same idea, and the eventual network integration extends that existing
// mechanism rather than inventing a parallel one: an operator-signed
// revoke names the banned identity, every node adds it here, and from
// then on nothing that identity signs is stored, relayed, or displayed.
//
// Enforcement is intentionally at BOTH layers, mirroring how the content
// blocklist already works: storage-side (nodes refuse to hold it) and
// client-side (a recipient refuses to display it even if some rogue node
// stored it anyway). This file is the client-side half; the storage-side
// half lands with the network adapter.
//
// What a ban does and doesn't do: it stops the identity being reachable
// or heard anywhere going forward, network-wide, without anyone needing
// to know who that identity talks to. It does not reach backward into
// messages already delivered and decrypted on someone's device -- no
// system can do that.
type BanList struct {
	mu   sync.RWMutex
	keys map[string]struct{}
}

// NewBanList returns an empty list. The zero value is safe: every
// Contains on a nil *BanList returns false, so "no bans configured" is
// a normal operating state that needs no special-casing at call sites.
func NewBanList() *BanList {
	return &BanList{keys: make(map[string]struct{})}
}

// Add inserts a hex identity pubkey. Returns true if newly added.
// Malformed keys are rejected rather than stored, so an operator typo
// can't silently produce a ban that matches nothing.
func (b *BanList) Add(hexKey string) bool {
	if b == nil {
		return false
	}
	hexKey = strings.ToLower(strings.TrimSpace(hexKey))
	if _, err := crypto.PublicKeyFromHex(hexKey); err != nil {
		return false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.keys == nil {
		b.keys = make(map[string]struct{})
	}
	if _, ok := b.keys[hexKey]; ok {
		return false
	}
	b.keys[hexKey] = struct{}{}
	return true
}

// Contains reports whether a hex identity pubkey is banned.
func (b *BanList) Contains(hexKey string) bool {
	if b == nil {
		return false
	}
	hexKey = strings.ToLower(strings.TrimSpace(hexKey))
	b.mu.RLock()
	defer b.mu.RUnlock()
	_, ok := b.keys[hexKey]
	return ok
}

// Size returns how many identities are banned.
func (b *BanList) Size() int {
	if b == nil {
		return 0
	}
	b.mu.RLock()
	defer b.mu.RUnlock()
	return len(b.keys)
}

// LoadBanListFromFile parses one hex identity pubkey per line. Blank
// lines and #-comments are ignored, and a missing file yields an empty
// list with no error -- same conventions as dht.LoadBlocklistFromFile,
// so operators configure both the same way.
func LoadBanListFromFile(path string) (*BanList, error) {
	bl := NewBanList()
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return bl, nil
		}
		return nil, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if i := strings.Index(line, "#"); i >= 0 {
			line = strings.TrimSpace(line[:i])
		}
		bl.Add(line)
	}
	return bl, sc.Err()
}
