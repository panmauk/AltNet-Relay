package moderation

import (
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"altnet-relay/mailbox"
	"altnet/core/crypto"
	"altnet/core/dht"
)

// latestStore models one real DHT replica: a single value per key, a new
// Put replacing the old. MemStore keeps every value ever written, which is
// the right model for "several replicas disagree" but makes a list edited
// hundreds of times look like hundreds of competing heads.
type latestStore struct {
	mu   sync.Mutex
	vals map[dht.NodeID][]byte
	gets int
}

func newLatestStore() *latestStore { return &latestStore{vals: map[dht.NodeID][]byte{}} }

func (s *latestStore) Put(k dht.NodeID, v []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.vals[k] = append([]byte(nil), v...)
	return nil
}

func (s *latestStore) Get(k dht.NodeID) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.gets++
	v, ok := s.vals[k]
	if !ok || mailbox.ContentAddress(v) != k {
		return nil, mailbox.ErrNotFound
	}
	return append([]byte(nil), v...), nil
}

func (s *latestStore) GetAll(k dht.NodeID) ([][]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.vals[k]
	if !ok {
		return nil, mailbox.ErrNotFound
	}
	return [][]byte{append([]byte(nil), v...)}, nil
}

func (s *latestStore) delete(k dht.NodeID) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.vals, k)
}

// countingStore counts page fetches on top of any store.
type countingStore struct {
	mailbox.Store
	mu   sync.Mutex
	gets int
}

func (s *countingStore) Get(k dht.NodeID) ([]byte, error) {
	s.mu.Lock()
	s.gets++
	s.mu.Unlock()
	return s.Store.Get(k)
}

// failingStore is a network that is down.
type failingStore struct{}

var errDown = errors.New("network down")

func (failingStore) Put(dht.NodeID, []byte) error        { return errDown }
func (failingStore) Get(dht.NodeID) ([]byte, error)      { return nil, errDown }
func (failingStore) GetAll(dht.NodeID) ([][]byte, error) { return nil, errDown }

func newKey(t *testing.T) *crypto.Identity {
	t.Helper()
	id, err := crypto.NewIdentity()
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func pubHex(id *crypto.Identity) string { return crypto.PublicKeyToHex(id.PublicKey) }

// forgeHead builds a head that claims to be operator's but is signed by
// someone else.
func forgeHead(t *testing.T, operator string, signer *crypto.Identity, version int64, pages []string, count int) []byte {
	t.Helper()
	h := listHead{Kind: headKind, Operator: operator, Version: version, Pages: pages, Count: count}
	if h.Pages == nil {
		h.Pages = []string{}
	}
	data, err := h.signedBytes()
	if err != nil {
		t.Fatal(err)
	}
	h.Sig = signer.Sign(data)
	raw, err := json.Marshal(&h)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func mustBan(t *testing.T, o *Operator, user, reason string) {
	t.Helper()
	if err := o.Ban(user, reason); err != nil {
		t.Fatalf("ban: %v", err)
	}
}

func mustRefresh(t *testing.T, y *Sync) {
	t.Helper()
	if err := y.Refresh(); err != nil {
		t.Fatalf("refresh: %v", err)
	}
}

func TestBanUnbanAndChecks(t *testing.T) {
	store := mailbox.NewMemStore()
	opKey := newKey(t)
	op := NewOperator(store, opKey)
	victim := newKey(t)

	mustBan(t, op, victim.ID(), "tos-4.1")
	if err := op.Ban(victim.ID(), "again"); !errors.Is(err, ErrAlreadyBanned) {
		t.Fatalf("second ban: got %v, want ErrAlreadyBanned", err)
	}

	y := NewSync(store, []string{strings.ToUpper(op.PublicKey())}) // config spelling doesn't matter
	mustRefresh(t, y)
	if !y.BannedPeer(victim.ID()) {
		t.Fatal("username should be banned")
	}
	if !y.Banned(pubHex(victim)) || !y.Contains(strings.ToUpper(pubHex(victim))) {
		t.Fatal("the banned identity's signing key should be banned")
	}
	if y.Banned(victim.ID()) {
		t.Fatal("a username is not a key: Banned must not match it")
	}
	bans := y.List()
	if len(bans) != 1 || bans[0].Username != victim.ID() || bans[0].Reason != "tos-4.1" || bans[0].Operator != op.PublicKey() {
		t.Fatalf("unexpected list: %+v", bans)
	}

	if err := op.Unban(victim.ID()); err != nil {
		t.Fatal(err)
	}
	if err := op.Unban(victim.ID()); !errors.Is(err, ErrNotBanned) {
		t.Fatalf("second unban: got %v, want ErrNotBanned", err)
	}
	mustRefresh(t, y)
	if y.BannedPeer(victim.ID()) || y.Banned(pubHex(victim)) || len(y.List()) != 0 {
		t.Fatal("unban did not take effect")
	}

	// Bad input never reaches the list.
	if op.Ban("not-a-username", "") == nil {
		t.Fatal("malformed username accepted")
	}
	if op.Ban(victim.ID(), "evil\x1b[2Jreason") == nil || op.Ban(victim.ID(), "rtl‮txt") == nil {
		t.Fatal("control/format characters accepted in a reason")
	}
	if op.Ban(victim.ID(), strings.Repeat("x", MaxReasonLen+1)) == nil {
		t.Fatal("overlong reason accepted")
	}
}

func TestOnlyTrustedOperatorsCount(t *testing.T) {
	store := mailbox.NewMemStore()
	opKey, mallory := newKey(t), newKey(t)
	opPub := pubHex(opKey)
	victim := newKey(t).ID()

	// Mallory runs a perfectly valid list of her own, and plants copies
	// of it at the operator's slot too. Neither counts.
	evil := NewOperator(store, mallory)
	mustBan(t, evil, victim, "")
	if err := store.Put(HeadKey(opPub), evil.view.current(evil.pub).raw); err != nil {
		t.Fatal(err)
	}

	// A head that names the operator but carries Mallory's signature,
	// pointing at a real page that bans the victim.
	page, _ := json.Marshal(listPage{Kind: pageKind, Entries: []entry{{User: victim, Since: 1}}})
	_ = store.Put(mailbox.ContentAddress(page), page)
	_ = store.Put(HeadKey(opPub), forgeHead(t, opPub, mallory, 1<<60, []string{hashHex(page)}, 1))

	// The operator's genuine head, re-signed after tampering with its
	// page list: the signature no longer covers it.
	good, err := build(opKey, nil, 5)
	if err != nil {
		t.Fatal(err)
	}
	tampered := good.head
	tampered.Pages = []string{hashHex(page)}
	tampered.Count = 1
	tampered.Version = 6
	raw, _ := json.Marshal(&tampered)
	_ = store.Put(HeadKey(opPub), raw)

	y := NewSync(store, []string{opPub, "zz-not-a-key"})
	mustRefresh(t, y)
	if y.BannedPeer(victim) {
		t.Fatal("a ban not signed by a trusted operator took effect")
	}
	if n := len(y.Lists()); n != 0 {
		t.Fatalf("expected no list installed, got %d", n)
	}

	// A Sync that trusts nobody bans nobody, even Mallory's own list.
	if z := NewSync(store, nil); z.Refresh() != nil || z.BannedPeer(victim) {
		t.Fatal("an unconfigured Sync must be inert")
	}
}

func TestNonCanonicalHeadRejected(t *testing.T) {
	opKey := newKey(t)
	l, err := build(opKey, [][]entry{{{User: newKey(t).ID(), Since: 1}}}, 7)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := parseHead(pubHex(opKey), l.raw, 0); !ok {
		t.Fatal("canonical head rejected")
	}
	respelled := []string{
		" " + string(l.raw),
		strings.Replace(string(l.raw), `"kind"`, `"Kind"`, 1),
		strings.Replace(string(l.raw), `{`, `{"extra":1,`, 1),
		strings.Replace(string(l.raw), l.head.Sig, strings.ToUpper(l.head.Sig), 1),
	}
	for i, r := range respelled {
		if _, ok := parseHead(pubHex(opKey), []byte(r), 0); ok {
			t.Fatalf("respelling %d of a genuine head accepted", i)
		}
	}
}

func TestReplayAndRollbackRejected(t *testing.T) {
	store := newLatestStore()
	opKey := newKey(t)
	op := NewOperator(store, opKey)
	a, b := newKey(t).ID(), newKey(t).ID()

	snapshot := func() *list { return op.view.current(op.pub) }

	mustBan(t, op, a, "") // v1: {a}
	v1 := snapshot()
	mustBan(t, op, b, "") // v2: {a, b}
	v2 := snapshot()
	if err := op.Unban(a); err != nil { // v3: {b}
		t.Fatal(err)
	}
	v3 := snapshot()
	if !(v1.head.Version < v2.head.Version && v2.head.Version < v3.head.Version) {
		t.Fatal("versions must strictly increase")
	}

	y := NewSync(store, []string{op.pub})
	mustRefresh(t, y)
	if y.BannedPeer(a) || !y.BannedPeer(b) {
		t.Fatal("client should hold v3")
	}

	// Replay every old head (pages and all) at the slot, alone and
	// together: the client holds v3 and must not move.
	replay := func(ls ...*list) {
		for _, l := range ls {
			for _, blob := range l.blobs {
				_ = store.Put(mailbox.ContentAddress(blob), blob)
			}
		}
		for _, l := range ls {
			_ = store.Put(HeadKey(op.pub), l.raw)
			mustRefresh(t, y)
			if y.BannedPeer(a) || !y.BannedPeer(b) {
				t.Fatalf("replaying version %d rolled the client back", l.head.Version)
			}
		}
	}
	replay(v1, v2)

	mem := mailbox.NewMemStore() // all versions visible at once
	for _, l := range []*list{v1, v2, v3} {
		for _, blob := range l.blobs {
			_ = mem.Put(mailbox.ContentAddress(blob), blob)
		}
		_ = mem.Put(HeadKey(op.pub), l.raw)
	}
	fresh := NewSync(mem, []string{op.pub})
	mustRefresh(t, fresh)
	if fresh.BannedPeer(a) || !fresh.BannedPeer(b) {
		t.Fatal("with every version visible, the newest must win")
	}

	// A restarted client keeps its floor through Export/Import, so an
	// attacker who serves it only v2 can't undo the unban of a...
	saved, err := y.Export()
	if err != nil {
		t.Fatal(err)
	}
	old := mailbox.NewMemStore()
	for _, blob := range v2.blobs {
		_ = old.Put(mailbox.ContentAddress(blob), blob)
	}
	_ = old.Put(HeadKey(op.pub), v2.raw)
	restarted := NewSync(old, []string{op.pub})
	if err := restarted.Import(saved); err != nil {
		t.Fatal(err)
	}
	mustRefresh(t, restarted)
	if restarted.BannedPeer(a) || !restarted.BannedPeer(b) {
		t.Fatal("a replayed v2 rolled a restarted client back")
	}
	// ...and importing an older snapshot over a newer one does nothing.
	older, _ := json.Marshal(savedState{Lists: []savedList{{Head: v1.raw, Pages: v1.blobs}}})
	if err := restarted.Import(older); err != nil {
		t.Fatal(err)
	}
	if restarted.BannedPeer(a) || !restarted.BannedPeer(b) {
		t.Fatal("importing an older snapshot rolled the client back")
	}
	// Saved state from a key the client doesn't trust is ignored.
	other := NewSync(old, []string{pubHex(newKey(t))})
	if err := other.Import(saved); err != nil || other.BannedPeer(b) {
		t.Fatal("saved lists from an untrusted operator were applied")
	}
}

func TestPagingPastOneValue(t *testing.T) {
	store := newLatestStore()
	op := NewOperator(store, newKey(t))
	reason := strings.Repeat("r", MaxReasonLen)
	var users []string
	for i := 0; i < 400; i++ {
		u := newKey(t).ID()
		users = append(users, u)
		mustBan(t, op, u, reason)
	}
	info, _ := op.Info()
	if info.Pages < 2 || info.Count != len(users) {
		t.Fatalf("expected a multi-page list of %d, got %+v", len(users), info)
	}
	cur := op.view.current(op.pub)
	total := len(cur.raw)
	for _, b := range cur.blobs {
		if len(b) > dht.MaxValueSize {
			t.Fatalf("page of %d bytes exceeds a DHT value", len(b))
		}
		total += len(b)
	}
	if total <= dht.MaxValueSize {
		t.Fatalf("list is only %d bytes; the test should exceed one value", total)
	}

	y := NewSync(store, []string{op.pub})
	mustRefresh(t, y)
	for _, u := range users {
		if !y.BannedPeer(u) {
			t.Fatal("a ban on a later page was lost")
		}
	}

	// Unbanning someone on the first page changes only that page, so a
	// client that already holds the list fetches exactly one page.
	before := append([]string(nil), cur.head.Pages...)
	if err := op.Unban(users[0]); err != nil {
		t.Fatal(err)
	}
	after := op.view.current(op.pub).head.Pages
	changed := 0
	for i := range after {
		if after[i] != before[i] {
			changed++
		}
	}
	if len(after) != len(before) || changed != 1 {
		t.Fatalf("unban rewrote %d pages (of %d -> %d)", changed, len(before), len(after))
	}
	store.gets = 0
	mustRefresh(t, y)
	if store.gets != 1 {
		t.Fatalf("refresh fetched %d pages, want 1", store.gets)
	}
	if y.BannedPeer(users[0]) || !y.BannedPeer(users[len(users)-1]) {
		t.Fatal("paged unban applied wrongly")
	}

	// The publisher refuses to grow a list past MaxPages.
	many := make([][]entry, MaxPages+1)
	for i := range many {
		many[i] = []entry{{User: hashHex([]byte{byte(i), byte(i >> 8)}), Since: 1}}
	}
	if _, err := build(newKey(t), many, 1); !errors.Is(err, ErrListFull) {
		t.Fatalf("oversized list: got %v, want ErrListFull", err)
	}
}

func TestJunkAndFloodsAreBounded(t *testing.T) {
	store := mailbox.NewMemStore()
	opKey, mallory := newKey(t), newKey(t)
	op := NewOperator(store, opKey)
	victim := newKey(t).ID()
	mustBan(t, op, victim, "")

	// After the genuine head, a flood: forgeries claiming ever-higher
	// versions (they sort first), unparseable junk, and oversized values.
	key := HeadKey(op.pub)
	for i := 0; i < 1000; i++ {
		_ = store.Put(key, forgeHead(t, op.pub, mallory, int64(1<<61+i), nil, 0))
		_ = store.Put(key, []byte("junk"+strings.Repeat("x", i)))
	}
	_ = store.Put(key, make([]byte, dht.MaxValueSize+1))

	y := NewSync(store, []string{op.pub})
	start := sigChecks.Load()
	mustRefresh(t, y)
	if n := sigChecks.Load() - start; n > maxHeadCandidates {
		t.Fatalf("a flood cost %d signature checks, cap is %d", n, maxHeadCandidates)
	}
	if !y.BannedPeer(victim) {
		t.Fatal("the genuine head, seen before the flood, should still load")
	}
	// Once held, the same flood costs nothing further to re-examine
	// beyond the cap.
	start = sigChecks.Load()
	mustRefresh(t, y)
	if n := sigChecks.Load() - start; n > maxHeadCandidates {
		t.Fatalf("a repeat refresh cost %d signature checks", n)
	}

	// Page walks are bounded too. Replay many genuine old heads whose
	// pages are gone: a fresh client tries at most maxHeadAttempts of
	// them, stops each at its first missing page, and installs nothing.
	src := newLatestStore()
	pub := NewOperator(src, opKey)
	bare := mailbox.NewMemStore()
	for i := 0; i < 10; i++ {
		mustBan(t, pub, newKey(t).ID(), "")
		_ = bare.Put(HeadKey(op.pub), pub.view.current(pub.pub).raw) // heads only, no pages
	}
	cs := &countingStore{Store: bare}
	z := NewSync(cs, []string{op.pub})
	if err := z.Refresh(); err == nil {
		t.Fatal("a list whose pages are missing must report an error")
	}
	if cs.gets > maxHeadAttempts {
		t.Fatalf("fetched %d pages for unloadable heads, cap is %d", cs.gets, maxHeadAttempts)
	}
	if len(z.Lists()) != 0 {
		t.Fatal("a partial list was installed")
	}
}

func TestFailedRefreshKeepsLastGoodState(t *testing.T) {
	store := newLatestStore()
	opKey := newKey(t)
	op := NewOperator(store, opKey)
	a, b := newKey(t).ID(), newKey(t).ID()
	mustBan(t, op, a, "")

	y := NewSync(store, []string{op.pub})
	mustRefresh(t, y)
	saved, _ := y.Export()

	// The network is down.
	down := NewSync(failingStore{}, []string{op.pub})
	if err := down.Import(saved); err != nil {
		t.Fatal(err)
	}
	if err := down.Refresh(); err == nil || !down.BannedPeer(a) {
		t.Fatal("a failed refresh must report an error and keep the ban")
	}

	// The list is withheld entirely.
	empty := NewSync(mailbox.NewMemStore(), []string{op.pub})
	_ = empty.Import(saved)
	if err := empty.Refresh(); err != nil || !empty.BannedPeer(a) {
		t.Fatal("a withheld list must not lift a ban")
	}

	// A newer genuine list is published but one of its pages is missing:
	// the client reports it and keeps enforcing the old list whole.
	mustBan(t, op, b, "")
	newest := op.view.current(op.pub)
	lastPage := newest.head.Pages[len(newest.head.Pages)-1]
	pageKey, _ := dht.IDFromHex(lastPage)
	store.delete(pageKey)
	if err := y.Refresh(); err == nil {
		t.Fatal("expected an error for the unloadable newer list")
	}
	if !y.BannedPeer(a) || y.BannedPeer(b) {
		t.Fatal("the last good list should still be in force, unchanged")
	}
	// The operator, too, refuses to build on a list it can't load.
	other := NewOperator(store, opKey)
	if err := other.Ban(newKey(t).ID(), ""); err == nil {
		t.Fatal("an operator published over a list it could not load")
	}
	// Once the page is back, the client catches up.
	_ = store.Put(pageKey, newest.blobs[len(newest.blobs)-1])
	mustRefresh(t, y)
	if !y.BannedPeer(a) || !y.BannedPeer(b) {
		t.Fatal("client did not catch up once the page reappeared")
	}
}

func TestOperatorBuildsOnSavedCopy(t *testing.T) {
	opKey := newKey(t)
	first := NewOperator(newLatestStore(), opKey)
	a, b := newKey(t).ID(), newKey(t).ID()
	mustBan(t, first, a, "")
	saved, err := first.Export()
	if err != nil {
		t.Fatal(err)
	}

	// Publishing from a node that can't see the existing list: with the
	// saved copy imported, the new version still carries the old ban.
	blind := newLatestStore()
	second := NewOperator(blind, opKey)
	if err := second.Import(saved); err != nil {
		t.Fatal(err)
	}
	mustBan(t, second, b, "")
	y := NewSync(blind, []string{pubHex(opKey)})
	mustRefresh(t, y)
	if !y.BannedPeer(a) || !y.BannedPeer(b) {
		t.Fatal("an edit from a saved copy dropped an earlier ban")
	}
}

func TestAssignHandleUnderAuthority(t *testing.T) {
	store := mailbox.NewMemStore()
	opKey, mallory := newKey(t), newKey(t)
	alice, bob := newKey(t).ID(), newKey(t).ID()

	mailbox.SetTrustedHandleAuthorities([]string{pubHex(opKey)})
	defer mailbox.SetTrustedHandleAuthorities(nil)

	if _, err := AssignHandle(store, opKey, "@Alice", alice, time.Duration(mailbox.HandleTTL)*time.Second); err != nil {
		t.Fatal(err)
	}
	rec, err := mailbox.ResolveHandle(store, "alice")
	if err != nil || rec.Username != alice || rec.PublicKey != pubHex(opKey) {
		t.Fatalf("resolve: %+v, %v", rec, err)
	}
	if got, err := ResolveTarget(store, "@alice"); err != nil || got != alice {
		t.Fatalf("ResolveTarget(@alice) = %q, %v", got, err)
	}

	// A non-authority's claim with a higher version doesn't count.
	fake := &mailbox.HandleRecord{Handle: "alice", Username: mallory.ID(), Version: 1 << 62, Timestamp: time.Now().Unix()}
	if err := fake.Sign(mallory); err != nil {
		t.Fatal(err)
	}
	blob, _ := fake.Marshal()
	_ = store.Put(mailbox.HandleKey("alice"), blob)
	if rec, err := mailbox.ResolveHandle(store, "alice"); err != nil || rec.Username != alice {
		t.Fatalf("a non-authority claim displaced the assignment: %+v, %v", rec, err)
	}

	// Reassignment outranks the previous one.
	if _, err := AssignHandle(store, opKey, "alice", bob, 0); err != nil {
		t.Fatal(err)
	}
	if rec, err := mailbox.ResolveHandle(store, "alice"); err != nil || rec.Username != bob {
		t.Fatalf("reassignment: %+v, %v", rec, err)
	}

	if _, err := AssignHandle(store, opKey, "al", alice, 0); err == nil {
		t.Fatal("invalid handle accepted")
	}
	if _, err := AssignHandle(store, opKey, "alice2", "nope", 0); err == nil {
		t.Fatal("invalid username accepted")
	}
}

func TestResolveTarget(t *testing.T) {
	store := mailbox.NewMemStore()
	id, err := mailbox.LoadOrCreateIdentity(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := mailbox.NewClient(store, id, nil).PublishProfile(""); err != nil {
		t.Fatal(err)
	}
	pk := pubHex(id.Signing)

	if got, err := ResolveTarget(store, "pk:"+strings.ToUpper(pk)); err != nil || got != id.Username() {
		t.Fatalf("pk: form = %q, %v", got, err)
	}
	if got, err := ResolveTarget(store, strings.ToUpper(id.Username())); err != nil || got != id.Username() {
		t.Fatalf("username form = %q, %v", got, err)
	}
	// A bare key typed as a username would ban nobody; with its profile
	// published, that mistake is caught.
	if _, err := ResolveTarget(store, pk); err == nil || !strings.Contains(err.Error(), "pk:") {
		t.Fatalf("bare key taken as a username: %v", err)
	}
	// An unknown username is still accepted: an identity that only ever
	// knocks never has to publish a profile.
	unknown := newKey(t).ID()
	if got, err := ResolveTarget(store, unknown); err != nil || got != unknown {
		t.Fatalf("unknown username = %q, %v", got, err)
	}
	if _, err := ResolveTarget(store, "pk:1234"); err == nil {
		t.Fatal("malformed key accepted")
	}
	if _, err := ResolveTarget(store, "@nobody-here"); err == nil {
		t.Fatal("unresolvable handle accepted")
	}
}
