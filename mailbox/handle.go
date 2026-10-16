package mailbox

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"altnet/core/crypto"
	"altnet/core/dht"
)

// A handle is the human-typeable name a person hands out instead of
// their 64-character username: "rob" rather than
// "99e12e96cfbb...". It resolves, through the DHT, to the Relay identity
// it belongs to -- exactly the job a .alt name does for a site.
//
// Handles are OPT-IN, and that is the privacy control. Claiming one
// makes you findable by anyone who types it, which is the point but
// also a real change: a guessable handle means strangers can reach your
// message requests. Not claiming one leaves you reachable only by your
// raw username, i.e. only by people you personally hand it to. Both
// modes are supported on purpose; the user chooses.
//
// The message-request system is what makes public handles tolerable:
// an unknown sender lands in requests, capped at PendingMessageLimit
// messages until accepted, and can be ignored or banned.

const handleDomain = "altnet-relay-handle-v1"

const (
	// MinHandleLen / MaxHandleLen bound a handle. Short enough to say
	// out loud, long enough to be distinctive.
	MinHandleLen = 3
	MaxHandleLen = 32

	// HandleTTL is how long a claim stays valid without a refresh.
	// Much longer than a name record's 7 days: losing your username
	// because you didn't open the app for a week would be indefensible.
	// Clients republish at startup, so this only expires for someone
	// genuinely gone for a month.
	HandleTTL int64 = 30 * 24 * 3600
)

// Authority mode. Mirrors core/name's trusted-registrar mechanism: with
// no authorities configured, handles are open and self-claimed
// (first-writer-wins, adequate for development). Configure the
// operator's key and only handles that authority signed will resolve --
// which is how uniqueness gets genuinely enforced, since the account
// backend can check a unique index before signing. A fork that strips
// the list just builds a separate network.
var (
	handleAuthoritiesMu sync.RWMutex
	handleAuthorities   map[string]bool
)

// SetTrustedHandleAuthorities configures the allowlist of Ed25519
// public keys (hex) permitted to sign handle claims. Empty = open mode.
func SetTrustedHandleAuthorities(keys []string) {
	m := make(map[string]bool, len(keys))
	for _, k := range keys {
		if k = strings.ToLower(strings.TrimSpace(k)); k != "" {
			m[k] = true
		}
	}
	handleAuthoritiesMu.Lock()
	if len(m) == 0 {
		handleAuthorities = nil
	} else {
		handleAuthorities = m
	}
	handleAuthoritiesMu.Unlock()
}

func handleAuthorityAllowed(pk string) (allowed, enforcing bool) {
	handleAuthoritiesMu.RLock()
	defer handleAuthoritiesMu.RUnlock()
	if len(handleAuthorities) == 0 {
		return true, false
	}
	return handleAuthorities[strings.ToLower(strings.TrimSpace(pk))], true
}

// CanonicalHandle lowercases and trims a handle, and strips a leading
// "@" so users can type either form.
//
// Only ASCII is lowercased (asciiLower, not strings.ToLower). The result
// names a DHT key, and ToLower folds the Kelvin sign U+212A into 'k':
// "Kate" would then pass ValidHandle and resolve to whoever owns
// "kate" while displaying as something else. Leaving non-ASCII alone
// makes such input fail ValidHandle instead.
func CanonicalHandle(h string) string {
	h = asciiLower(strings.TrimSpace(h))
	return strings.TrimPrefix(h, "@")
}

// ValidHandle enforces the character rules. Letters, digits, hyphen and
// underscore only, starting with a letter. Dots are excluded so a handle
// can never be mistaken for a .alt domain.
func ValidHandle(h string) error {
	c := CanonicalHandle(h)
	if len(c) < MinHandleLen {
		return fmt.Errorf("handle must be at least %d characters", MinHandleLen)
	}
	if len(c) > MaxHandleLen {
		return fmt.Errorf("handle must be at most %d characters", MaxHandleLen)
	}
	if c[0] < 'a' || c[0] > 'z' {
		return errors.New("handle must start with a letter")
	}
	for _, r := range c {
		ok := (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' || r == '_'
		if !ok {
			return fmt.Errorf("invalid character %q in handle (use a-z, 0-9, - and _)", r)
		}
	}
	return nil
}

// HandleKey is the DHT address a handle's claim lives at. Domain-
// separated from .alt name records, so "rob" as a handle and "rob" as a
// site name never collide.
func HandleKey(handle string) dht.NodeID {
	h := sha256.New()
	h.Write([]byte(handleDomain))
	h.Write([]byte{0})
	h.Write([]byte(CanonicalHandle(handle)))
	var id dht.NodeID
	copy(id[:], h.Sum(nil))
	return id
}

// HandleRecord maps a handle to the Relay username that owns it.
type HandleRecord struct {
	Handle    string `json:"handle"`   // canonical, no "@"
	Username  string `json:"username"` // 64-hex Relay identity this resolves to
	PublicKey string `json:"pk"`       // hex Ed25519 key of the signer
	Version   int64  `json:"version"`  // highest wins
	Timestamp int64  `json:"ts"`
	TTL       int64  `json:"ttl,omitempty"`
	Sig       string `json:"sig,omitempty"`
}

// Marshal serialises the record for storage.
func (h *HandleRecord) Marshal() ([]byte, error) { return json.Marshal(h) }

// Sign fills in PublicKey and Sig.
func (h *HandleRecord) Sign(id *crypto.Identity) error {
	h.PublicKey = crypto.PublicKeyToHex(id.PublicKey)
	h.Sig = ""
	data, err := json.Marshal(h)
	if err != nil {
		return err
	}
	h.Sig = id.Sign(data)
	return nil
}

// Expired reports whether the claim has lapsed, or is future-dated
// beyond tolerable clock skew.
func (h *HandleRecord) Expired(now int64) bool {
	if isFutureDated(h.Timestamp, now) {
		return true
	}
	if h.TTL <= 0 {
		return false
	}
	return now > h.Timestamp+h.TTL
}

// Verify checks the signature and the claim's legitimacy.
//
// In open mode the signer must BE the identity the handle points at
// (their key must hash to Username), so nobody can publish a handle
// that redirects to an identity they don't control. Squatting the
// handle string itself is still possible in open mode -- that is what
// authority mode exists to fix, exactly as with .alt names.
//
// In authority mode the signer must be a configured authority, which is
// what lets the account backend enforce global uniqueness before
// signing.
//
// The signature must also be canonical (see isCanonicalSig), so there is
// exactly one valid encoding of any claim that was signed.
func (h *HandleRecord) Verify() error {
	if !isCanonicalSig(h.Sig) {
		return errors.New("mailbox: handle signature is not in canonical form")
	}
	pub, err := crypto.PublicKeyFromHex(h.PublicKey)
	if err != nil {
		return fmt.Errorf("mailbox: parse handle key: %w", err)
	}
	sig := h.Sig
	h.Sig = ""
	data, err := json.Marshal(h)
	h.Sig = sig
	if err != nil {
		return err
	}
	if err := crypto.Verify(pub, data, sig); err != nil {
		return err
	}

	allowed, enforcing := handleAuthorityAllowed(h.PublicKey)
	if enforcing {
		if !allowed {
			return errors.New("mailbox: handle not signed by an approved authority")
		}
		return nil
	}
	// Open mode: self-claim only.
	if crypto.PublicKeyToID(pub) != h.Username {
		return errors.New("mailbox: handle signer does not own the username it points at")
	}
	return nil
}

// ClaimHandle publishes a self-signed claim binding handle to this
// client's username, and returns it.
//
// Republish periodically (a client does this at startup) so the claim
// doesn't lapse at HandleTTL.
func (c *Client) ClaimHandle(handle string) (*HandleRecord, error) {
	if err := ValidHandle(handle); err != nil {
		return nil, err
	}
	canonical := CanonicalHandle(handle)

	// Refuse to stomp on someone else's live claim. This is advisory,
	// not enforcement -- a modified client could ignore it, which is
	// precisely why production uses authority mode.
	var floor int64
	if existing, err := ResolveHandle(c.Store, canonical); err == nil {
		if existing.Username != c.ID.ID() {
			return nil, fmt.Errorf("mailbox: handle %q is already taken", canonical)
		}
		// Our own earlier claim: this is a refresh, so it must outrank
		// what's already published or the old record keeps winning.
		floor = existing.Version
	}

	rec := &HandleRecord{
		Handle:    canonical,
		Username:  c.ID.ID(),
		Version:   nextVersion(floor),
		Timestamp: time.Now().Unix(),
		TTL:       HandleTTL,
	}
	if err := rec.Sign(c.ID); err != nil {
		return nil, err
	}
	blob, err := rec.Marshal()
	if err != nil {
		return nil, err
	}
	if err := c.Store.Put(HandleKey(canonical), blob); err != nil {
		return nil, fmt.Errorf("mailbox: publish handle: %w", err)
	}
	return rec, nil
}

// ResolveHandle turns a typed handle into the record naming its owner.
// Candidates that fail verification, name a different handle, or have
// expired are skipped; the highest surviving Version wins.
func ResolveHandle(s Store, handle string) (*HandleRecord, error) {
	if err := ValidHandle(handle); err != nil {
		return nil, err
	}
	canonical := CanonicalHandle(handle)
	candidates, err := s.GetAll(HandleKey(canonical))
	if err != nil {
		return nil, err
	}
	now := time.Now().Unix()

	var best *HandleRecord
	var lastErr error
	for _, blob := range boundCandidates(candidates, maxSlotCandidates) {
		var rec HandleRecord
		if err := json.Unmarshal(blob, &rec); err != nil {
			lastErr = err
			continue
		}
		if rec.Handle != canonical {
			lastErr = errors.New("record is for a different handle")
			continue
		}
		// Lowercase hex only: in authority mode nothing else ties
		// Username to a key, and an upper-cased spelling would derive
		// different mailbox and doorbell keys from the real ones.
		if !isLowerHexID(rec.Username) {
			lastErr = errors.New("record has a malformed username")
			continue
		}
		if err := rec.Verify(); err != nil {
			lastErr = err
			continue
		}
		if rec.Expired(now) {
			lastErr = errors.New("claim expired")
			continue
		}
		if best == nil || rec.Version > best.Version {
			r := rec
			best = &r
		}
	}
	if best == nil {
		if lastErr != nil {
			return nil, fmt.Errorf("mailbox: cannot resolve @%s: %w", canonical, lastErr)
		}
		return nil, ErrNotFound
	}
	return best, nil
}

// LookupUsername resolves either a handle or a raw 64-hex username into
// a username, so every command can accept whichever the user typed.
func LookupUsername(s Store, input string) (string, error) {
	in := strings.TrimSpace(input)
	// Accept any ASCII casing of a raw username, but hand back only the
	// lowercase spelling: every key is derived by hashing the string, so
	// any other spelling would address slots nobody reads.
	if id := asciiLower(in); isLowerHexID(id) {
		return id, nil
	}
	rec, err := ResolveHandle(s, in)
	if err != nil {
		return "", err
	}
	return rec.Username, nil
}
