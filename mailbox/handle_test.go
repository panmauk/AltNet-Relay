package mailbox

import (
	"testing"
	"time"

	"altnet/core/crypto"
)

func newHandleClient(t *testing.T, s Store) *Client {
	t.Helper()
	id, err := crypto.NewIdentity()
	if err != nil {
		t.Fatal(err)
	}
	box, err := NewBoxKey()
	if err != nil {
		t.Fatal(err)
	}
	return &Client{Store: s, ID: id, Box: box}
}

func TestClaimAndResolveHandle(t *testing.T) {
	store := NewMemStore()
	rob := newHandleClient(t, store)

	rec, err := rob.ClaimHandle("rob")
	if err != nil {
		t.Fatal(err)
	}
	if rec.Handle != "rob" || rec.Username != rob.ID.ID() {
		t.Fatalf("unexpected claim: %+v", rec)
	}

	// Anyone can now turn the short name into the full identity.
	got, err := LookupUsername(store, "rob")
	if err != nil {
		t.Fatal(err)
	}
	if got != rob.ID.ID() {
		t.Fatal("handle did not resolve to the right identity")
	}

	// The "@" form and mixed case work too.
	if got, err := LookupUsername(store, "@ROB"); err != nil || got != rob.ID.ID() {
		t.Fatalf("@ROB should resolve the same: %q %v", got, err)
	}
}

// A raw username still works everywhere a handle does, so nobody is
// locked out for not claiming one.
func TestLookupAcceptsRawUsername(t *testing.T) {
	store := NewMemStore()
	rob := newHandleClient(t, store)

	got, err := LookupUsername(store, rob.ID.ID())
	if err != nil {
		t.Fatal(err)
	}
	if got != rob.ID.ID() {
		t.Fatal("a raw username should resolve to itself")
	}
}

// In open mode a claim must be self-signed by the identity it points
// at, so nobody can publish a handle that redirects to someone else.
func TestHandleCannotPointAtSomeoneElse(t *testing.T) {
	store := NewMemStore()
	eve := newHandleClient(t, store)
	victim := newHandleClient(t, store)

	// Eve signs a claim for "rob" that points at the victim's identity.
	forged := &HandleRecord{
		Handle:    "rob",
		Username:  victim.ID.ID(),
		Version:   1 << 62,
		Timestamp: nowSeconds(),
		TTL:       HandleTTL,
	}
	if err := forged.Sign(eve.ID); err != nil {
		t.Fatal(err)
	}
	blob, _ := forged.Marshal()
	if err := store.Put(HandleKey("rob"), blob); err != nil {
		t.Fatal(err)
	}

	if _, err := ResolveHandle(store, "rob"); err == nil {
		t.Fatal("a claim signed by someone other than its owner must not resolve")
	}
}

// Claiming a handle someone else already holds is refused.
func TestHandleAlreadyTaken(t *testing.T) {
	store := NewMemStore()
	first := newHandleClient(t, store)
	second := newHandleClient(t, store)

	if _, err := first.ClaimHandle("rob"); err != nil {
		t.Fatal(err)
	}
	if _, err := second.ClaimHandle("rob"); err == nil {
		t.Fatal("expected the second claim to be refused")
	}
	// The original still resolves to the original owner.
	got, err := LookupUsername(store, "rob")
	if err != nil {
		t.Fatal(err)
	}
	if got != first.ID.ID() {
		t.Fatal("an existing handle must not be stolen")
	}
}

// Re-claiming your own handle is fine -- that's the periodic refresh
// that stops it expiring.
func TestReclaimingOwnHandleRefreshes(t *testing.T) {
	store := NewMemStore()
	rob := newHandleClient(t, store)

	first, err := rob.ClaimHandle("rob")
	if err != nil {
		t.Fatal(err)
	}
	second, err := rob.ClaimHandle("rob")
	if err != nil {
		t.Fatalf("refreshing your own handle should be allowed: %v", err)
	}
	if second.Version <= first.Version {
		t.Fatal("a refresh should bump the version so it wins")
	}
}

// A profile's self-reported handle is a hint only: it must be confirmed
// by resolving forward, or an impostor could display someone else's name.
func TestProfileHandleMustVerifyForward(t *testing.T) {
	store := NewMemStore()
	rob := newHandleClient(t, store)
	eve := newHandleClient(t, store)

	if _, err := rob.ClaimHandle("rob"); err != nil {
		t.Fatal(err)
	}
	if _, err := rob.PublishProfile("rob"); err != nil {
		t.Fatal(err)
	}
	// Eve publishes a profile claiming to be "rob".
	if _, err := eve.PublishProfile("rob"); err != nil {
		t.Fatal(err)
	}

	eveProf, err := FetchProfile(store, eve.ID.ID())
	if err != nil {
		t.Fatal(err)
	}
	if eveProf.Handle != "rob" {
		t.Fatal("sanity: eve's profile should carry the false claim")
	}
	if got := eveProf.VerifiedHandle(store); got != "" {
		t.Fatalf("eve must not be able to display @rob, got %q", got)
	}

	// The real owner verifies fine.
	robProf, err := FetchProfile(store, rob.ID.ID())
	if err != nil {
		t.Fatal(err)
	}
	if got := robProf.VerifiedHandle(store); got != "rob" {
		t.Fatalf("the genuine owner should verify, got %q", got)
	}
}

func TestHandleValidation(t *testing.T) {
	bad := []string{"", "ab", "1rob", "-rob", "rob.alt", "rob bob", "rob!", string(make([]byte, 40))}
	for _, h := range bad {
		if err := ValidHandle(h); err == nil {
			t.Errorf("expected %q to be rejected", h)
		}
	}
	good := []string{"rob", "rob_smith", "rob-smith", "rob99", "@rob", "ROB"}
	for _, h := range good {
		if err := ValidHandle(h); err != nil {
			t.Errorf("expected %q to be accepted, got %v", h, err)
		}
	}
}

// Authority mode: with an operator key configured, only handles that
// authority signed resolve -- which is how uniqueness gets enforced
// centrally without changing anything else.
func TestAuthorityModeRejectsSelfClaims(t *testing.T) {
	store := NewMemStore()
	rob := newHandleClient(t, store)
	authority, err := crypto.NewIdentity()
	if err != nil {
		t.Fatal(err)
	}

	if _, err := rob.ClaimHandle("rob"); err != nil {
		t.Fatal(err)
	}

	SetTrustedHandleAuthorities([]string{crypto.PublicKeyToHex(authority.PublicKey)})
	defer SetTrustedHandleAuthorities(nil) // open mode for other tests

	if _, err := ResolveHandle(store, "rob"); err == nil {
		t.Fatal("a self-claim must not resolve while an authority is enforced")
	}

	// The authority can sign a claim for anyone.
	signed := &HandleRecord{
		Handle:    "rob",
		Username:  rob.ID.ID(),
		Version:   nowSeconds(),
		Timestamp: nowSeconds(),
		TTL:       HandleTTL,
	}
	if err := signed.Sign(authority); err != nil {
		t.Fatal(err)
	}
	blob, _ := signed.Marshal()
	if err := store.Put(HandleKey("rob"), blob); err != nil {
		t.Fatal(err)
	}
	got, err := LookupUsername(store, "rob")
	if err != nil {
		t.Fatal(err)
	}
	if got != rob.ID.ID() {
		t.Fatal("an authority-signed handle should resolve")
	}
}

func nowSeconds() int64 { return time.Now().Unix() }
