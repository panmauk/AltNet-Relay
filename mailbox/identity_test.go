package mailbox

import (
	"path/filepath"
	"testing"

	"altnet/core/crypto"
)

func TestLoadOrCreateIdentityIsStableAndSeparate(t *testing.T) {
	dir := t.TempDir()

	first, err := LoadOrCreateIdentity(dir)
	if err != nil {
		t.Fatal(err)
	}
	second, err := LoadOrCreateIdentity(dir)
	if err != nil {
		t.Fatal(err)
	}
	if first.Username() != second.Username() {
		t.Fatal("identity must be stable across loads")
	}
	if BoxPublicKeyToHex(first.Box.Public) != BoxPublicKeyToHex(second.Box.Public) {
		t.Fatal("box key must be stable across loads")
	}

	// The Relay identity must be its own key material, never the
	// daemon's node identity -- a node peer ID is broadcast in every
	// message and spread through Kademlia routing tables, so reusing it
	// would let anyone on the network message anyone.
	nodeDir := t.TempDir()
	node, err := crypto.LoadOrCreate(filepath.Join(nodeDir, "peer.key"))
	if err != nil {
		t.Fatal(err)
	}
	if first.Username() == node.ID() {
		t.Fatal("relay username must not be the node peer ID")
	}
}

// A username is only useful to someone who was handed it: every address
// derived from it is a hash, so nothing about a user is reachable
// without the username itself.
func TestAddressesAreDerivedFromUsernameOnly(t *testing.T) {
	dir := t.TempDir()
	id, err := LoadOrCreateIdentity(dir)
	if err != nil {
		t.Fatal(err)
	}
	user := id.Username()

	// None of the derived addresses leak the username in the clear.
	if DoorbellKey(user).Hex() == user {
		t.Fatal("doorbell address must not equal the username")
	}
	if ProfileKey(user).Hex() == user {
		t.Fatal("profile address must not equal the username")
	}
	// And they're distinct from each other, so holding one address
	// doesn't reveal the others.
	if DoorbellKey(user) == ProfileKey(user) {
		t.Fatal("doorbell and profile addresses must differ")
	}
}
