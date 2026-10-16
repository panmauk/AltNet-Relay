package mailbox

import (
	"crypto/ecdh"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"altnet/core/crypto"
	"altnet/core/dht"
)

const profileDomain = "altnet-relay-profile-v1"

// Record versions must strictly increase, because resolution picks the
// highest version and a tie means the update silently loses.
//
// time.Now().UnixNano() alone is not enough: on Windows the clock's
// resolution is coarse (often ~1ms, sometimes worse), so two publishes
// in quick succession -- claiming a handle right after startup already
// published a profile, say -- can read the *same* nanosecond value. This
// keeps a process-local floor so that can't happen.
var (
	versionMu   sync.Mutex
	lastVersion int64
)

// nextVersion returns a version strictly greater than any this process
// has issued, and at least floor+1 so a record already on the network
// (published by an earlier run, or under a skewed clock) is superseded.
func nextVersion(floor int64) int64 {
	versionMu.Lock()
	defer versionMu.Unlock()
	v := time.Now().UnixNano()
	if v <= lastVersion {
		v = lastVersion + 1
	}
	if v <= floor {
		v = floor + 1
	}
	lastVersion = v
	return v
}

// ProfileKey returns the DHT key holding a peer's published Profile.
// Derived from the peer ID alone, so anyone handed a username can look
// the owner's profile up -- the same one-way reachability property the
// doorbell has.
func ProfileKey(peerID string) dht.NodeID {
	h := sha256.New()
	h.Write([]byte(profileDomain))
	h.Write([]byte{0})
	h.Write([]byte(peerID))
	var id dht.NodeID
	copy(id[:], h.Sum(nil))
	return id
}

// Profile publishes the things someone needs in order to contact a peer
// they only know the peer ID of. Today that's one thing: the peer's
// stable X25519 box public key, without which a first message can't be
// sealed (Seal needs the recipient's box key, and a first-time sender
// has never exchanged anything with them).
//
// This is the small piece that makes "I only have your username" work.
// It's the analogue of a Signal prekey bundle, minus the rotation.
type Profile struct {
	PeerID    string `json:"peer_id"` // whose profile this is
	PublicKey string `json:"pk"`      // hex Ed25519 identity pubkey, must hash to PeerID
	BoxKey    string `json:"box_key"` // hex X25519 box public key
	Version   int64  `json:"version"` // bumped on each republish; highest wins
	Timestamp int64  `json:"ts"`

	// Handle is the human-readable name this user claims. It is a HINT
	// ONLY: the profile is self-signed, so anyone could write any handle
	// here. Never display it without resolving it forward
	// (ResolveHandle) and checking the result points back at this same
	// user -- otherwise an impostor's profile could show someone else's
	// name. VerifiedHandle does that check.
	Handle string `json:"handle,omitempty"`

	Sig string `json:"sig,omitempty"`
}

// VerifiedHandle returns the profile's claimed handle only if resolving
// it independently points back at this same user. Returns "" otherwise,
// including when no handle is claimed.
//
// This closes the impersonation gap in the self-signed Handle field: a
// forward lookup is authoritative, the profile's own claim is not.
func (p *Profile) VerifiedHandle(s Store) string {
	if p.Handle == "" {
		return ""
	}
	rec, err := ResolveHandle(s, p.Handle)
	if err != nil || rec.Username != p.PeerID {
		return ""
	}
	return rec.Handle
}

// Marshal serialises the profile for storage.
func (p *Profile) Marshal() ([]byte, error) { return json.Marshal(p) }

// Sign fills in PublicKey and Sig from id.
func (p *Profile) Sign(id *crypto.Identity) error {
	p.PublicKey = crypto.PublicKeyToHex(id.PublicKey)
	p.Sig = ""
	data, err := json.Marshal(p)
	if err != nil {
		return err
	}
	p.Sig = id.Sign(data)
	return nil
}

// Verify checks the signature AND the identity binding: the embedded
// public key must actually hash to the PeerID this profile claims. That
// binding is what stops an impostor planting their own profile at
// someone else's ProfileKey -- they'd have to produce a key that hashes
// to a peer ID they don't own.
//
// The signature must also be canonical (see isCanonicalSig), so there is
// exactly one valid encoding of any profile the owner signed.
func (p *Profile) Verify() error {
	if !isCanonicalSig(p.Sig) {
		return errors.New("mailbox: profile signature is not in canonical form")
	}
	pub, err := crypto.PublicKeyFromHex(p.PublicKey)
	if err != nil {
		return fmt.Errorf("mailbox: parse profile key: %w", err)
	}
	if crypto.PublicKeyToID(pub) != p.PeerID {
		return errors.New("mailbox: profile public key does not match its peer ID")
	}
	sig := p.Sig
	p.Sig = ""
	data, err := json.Marshal(p)
	p.Sig = sig
	if err != nil {
		return err
	}
	return crypto.Verify(pub, data, sig)
}

// BoxPublic parses the profile's box key into a usable X25519 key.
func (p *Profile) BoxPublic() (*ecdh.PublicKey, error) {
	return BoxPublicKeyFromHex(p.BoxKey)
}

// PublishProfile signs and stores id's profile so others can find the
// box key they need to message them. Call on startup, and again whenever
// the box key changes. handle may be empty.
func PublishProfile(s Store, id *crypto.Identity, box *BoxKey, handle string) (*Profile, error) {
	// Supersede whatever is already published for us, even if that was
	// written under a clock ahead of ours.
	//
	// The floor counts future-dated copies too, which FetchProfile
	// ignores: one left by an earlier run under a fast clock would
	// otherwise come back into force, stale box key and all, the moment
	// its timestamp came due.
	_, floor, _ := fetchProfile(s, id.ID(), time.Now().Unix())
	p := &Profile{
		PeerID:    id.ID(),
		BoxKey:    BoxPublicKeyToHex(box.Public),
		Handle:    CanonicalHandle(handle),
		Version:   nextVersion(floor),
		Timestamp: time.Now().Unix(),
	}
	if err := p.Sign(id); err != nil {
		return nil, err
	}
	blob, err := json.Marshal(p)
	if err != nil {
		return nil, err
	}
	if err := s.Put(ProfileKey(id.ID()), blob); err != nil {
		return nil, fmt.Errorf("mailbox: publish profile: %w", err)
	}
	return p, nil
}

// FetchProfile looks up a peer's profile and returns the verified record
// with the highest Version. Candidates that fail verification or whose
// identity binding doesn't hold are skipped, not fatal -- a single
// planted record among honest replicas must not break resolution.
//
// A profile timestamped further ahead than clock skew explains is not
// believed until its time comes (isFutureDated), the same rule handles
// follow.
func FetchProfile(s Store, peerID string) (*Profile, error) {
	best, _, err := fetchProfile(s, peerID, time.Now().Unix())
	return best, err
}

// fetchProfile is FetchProfile, additionally reporting the highest
// Version among ALL verified candidates, future-dated ones included, so
// a republish can outrank every copy that might later take effect.
func fetchProfile(s Store, peerID string, now int64) (*Profile, int64, error) {
	candidates, err := s.GetAll(ProfileKey(peerID))
	if err != nil {
		return nil, 0, err
	}
	var best *Profile
	var maxVersion int64
	var lastErr error
	for _, blob := range boundCandidates(candidates, maxSlotCandidates) {
		var p Profile
		if err := json.Unmarshal(blob, &p); err != nil {
			lastErr = err
			continue
		}
		if p.PeerID != peerID {
			lastErr = errors.New("mailbox: profile is for a different peer")
			continue
		}
		if err := p.Verify(); err != nil {
			lastErr = err
			continue
		}
		maxVersion = max(maxVersion, p.Version)
		if isFutureDated(p.Timestamp, now) {
			lastErr = errors.New("mailbox: profile is future-dated")
			continue
		}
		if best == nil || p.Version > best.Version {
			rec := p
			best = &rec
		}
	}
	if best == nil {
		if lastErr != nil {
			return nil, maxVersion, fmt.Errorf("mailbox: no valid profile for %.8s: %w", peerID, lastErr)
		}
		return nil, maxVersion, ErrNotFound
	}
	return best, maxVersion, nil
}
