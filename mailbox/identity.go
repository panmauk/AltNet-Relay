package mailbox

import (
	"fmt"
	"path/filepath"

	"altnet/core/crypto"
)

// Key file names inside a Relay identity directory. Kept separate from
// the daemon's data/keys/peer.key on purpose -- see the Client doc for
// why the Relay username must not be the node's peer ID.
const (
	IdentityFileName = "relay.key"
	BoxKeyFileName   = "relay.box"
)

// Identity is a Relay user's full key material: the Ed25519 identity
// their username derives from, and the X25519 box key others seal
// messages to.
type Identity struct {
	Signing *crypto.Identity
	Box     *BoxKey
}

// Username returns the string a user hands out so others can reach
// them. It is the hex peer ID of the Relay signing key -- unguessable
// (256 bits) and undiscoverable, since every address derived from it is
// a hash and the DHT can only be queried by exact key.
func (i *Identity) Username() string { return i.Signing.ID() }

// LoadOrCreateIdentity loads a Relay identity from dir, generating and
// saving one on first run. dir MUST be a Relay-specific directory, not
// the daemon's key directory: reusing the node's peer identity would
// publish the user's username to the whole network through routing
// tables and message headers.
func LoadOrCreateIdentity(dir string) (*Identity, error) {
	signing, err := crypto.LoadOrCreate(filepath.Join(dir, IdentityFileName))
	if err != nil {
		return nil, fmt.Errorf("mailbox: relay identity: %w", err)
	}
	box, err := LoadOrCreateBoxKey(filepath.Join(dir, BoxKeyFileName))
	if err != nil {
		return nil, fmt.Errorf("mailbox: relay box key: %w", err)
	}
	return &Identity{Signing: signing, Box: box}, nil
}

// NewClient builds a Client from a loaded Relay identity.
func NewClient(s Store, id *Identity, bans *BanList) *Client {
	c := &Client{Store: s, ID: id.Signing, Box: id.Box}
	// Only assign a real list: a nil *BanList stored in the interface
	// would make Bans non-nil while holding nothing.
	if bans != nil {
		c.Bans = bans
	}
	return c
}
