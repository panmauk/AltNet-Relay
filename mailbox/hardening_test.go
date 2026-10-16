package mailbox

import (
	"encoding/json"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"altnet/core/crypto"
	"altnet/core/dht"
)

// tipStore models a live-DHT slot more closely than MemStore: a replica
// holds one value per key, so the latest write is the only candidate a
// read sees. That is what lets an attacker's newest note BE the tip,
// which MemStore (keeping every version, first one winning ties) hides.
type tipStore struct{ *MemStore }

func (s tipStore) GetAll(key dht.NodeID) ([][]byte, error) {
	all, err := s.MemStore.GetAll(key)
	if err != nil {
		return nil, err
	}
	return all[len(all)-1:], nil
}

// countingStore counts reads, so tests can assert that work is bounded.
type countingStore struct {
	Store
	gets, getAlls atomic.Int64
}

func (s *countingStore) Get(key dht.NodeID) ([]byte, error) {
	s.gets.Add(1)
	return s.Store.Get(key)
}

func (s *countingStore) GetAll(key dht.NodeID) ([][]byte, error) {
	s.getAlls.Add(1)
	return s.Store.GetAll(key)
}

// plantNote writes a note the way a modified client could: any Seq, any
// Prev, any timestamp, any envelope. Chunks, archive copy and slot copy
// are all stored, exactly as putNote does.
func plantNote(t *testing.T, s Store, from *crypto.Identity, to *Client, msg *Message, prev string, seq, ts int64) *Note {
	t.Helper()
	plain, err := json.Marshal(msg)
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := Seal(to.Box.Public, plain)
	if err != nil {
		t.Fatal(err)
	}
	chunks, err := SplitChunks(sealed)
	if err != nil {
		t.Fatal(err)
	}
	var hexes []string
	for _, ch := range chunks {
		if err := s.Put(ch.Key, ch.Data); err != nil {
			t.Fatal(err)
		}
		hexes = append(hexes, ch.Key.Hex())
	}
	n := &Note{Recipient: to.ID.ID(), Chunks: hexes, Prev: prev, Seq: seq, Timestamp: ts}
	if err := n.Sign(from); err != nil {
		t.Fatal(err)
	}
	putPlanted(t, s, n, MailboxKey(from.ID(), n.Recipient))
	return n
}

// putPlanted stores a record at its content address and at slot.
func putPlanted(t *testing.T, s Store, rec interface{ Hash() string }, slot dht.NodeID) {
	t.Helper()
	blob, err := json.Marshal(rec)
	if err != nil {
		t.Fatal(err)
	}
	key, err := dht.IDFromHex(rec.Hash())
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Put(key, blob); err != nil {
		t.Fatal(err)
	}
	if err := s.Put(slot, blob); err != nil {
		t.Fatal(err)
	}
}

func textMsg(t *testing.T, text string) *Message {
	t.Helper()
	m, err := NewTextMessage(text)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func now() int64 { return time.Now().Unix() }

// A modified sender stamps every note Seq 1, chained properly by Prev,
// hoping each counts as "first". The cap must still hold.
func TestPendingCapSurvivesSeqOneStamping(t *testing.T) {
	store := tipStore{NewMemStore()}
	spammer := newTestClient(t, store)
	rob := newTestClient(t, store)
	conv := &Conversation{PeerID: spammer.ID.ID()}

	total, prev := 0, ""
	for i := 0; i < 10; i++ {
		n := plantNote(t, store, spammer.ID, rob, textMsg(t, "spam"), prev, 1, now())
		prev = n.Hash()
		msgs, err := rob.Receive(conv)
		if err != nil {
			t.Fatal(err)
		}
		total += len(msgs)
	}
	if total > PendingMessageLimit {
		t.Fatalf("Seq-1 stamping surfaced %d messages past a cap of %d", total, PendingMessageLimit)
	}
}

// Abandoning a chain and starting a fresh one at Seq 1 must not buy a
// fresh allowance once something has been surfaced.
func TestRestartedChainGetsNoFreshAllowance(t *testing.T) {
	store := tipStore{NewMemStore()}
	spammer := newTestClient(t, store)
	rob := newTestClient(t, store)
	conv := &Conversation{PeerID: spammer.ID.ID()}

	prev := ""
	for seq := int64(1); seq <= PendingMessageLimit; seq++ {
		prev = plantNote(t, store, spammer.ID, rob, textMsg(t, "first chain"), prev, seq, now()).Hash()
	}
	msgs, err := rob.Receive(conv)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != PendingMessageLimit {
		t.Fatalf("expected the first %d messages, got %d", PendingMessageLimit, len(msgs))
	}

	for restart := 0; restart < 3; restart++ {
		prev = ""
		for seq := int64(1); seq <= PendingMessageLimit; seq++ {
			prev = plantNote(t, store, spammer.ID, rob, textMsg(t, "restarted"), prev, seq, now()).Hash()
			more, err := rob.Receive(conv)
			if err != nil {
				t.Fatal(err)
			}
			if len(more) != 0 {
				t.Fatalf("restart %d: a fresh chain surfaced %d more messages", restart, len(more))
			}
		}
	}
}

// Seq may not reach zero or below, or a chain counting up from -100
// would put every one of its notes under the cap.
func TestNonPositiveSeqRejected(t *testing.T) {
	store := tipStore{NewMemStore()}
	spammer := newTestClient(t, store)
	rob := newTestClient(t, store)

	// A lone tip at Seq 0, then one at -1: neither is a note.
	for _, seq := range []int64{0, -1} {
		plantNote(t, store, spammer.ID, rob, textMsg(t, "zero"), "", seq, now())
		msgs, err := rob.Receive(&Conversation{PeerID: spammer.ID.ID()})
		if err != nil {
			t.Fatal(err)
		}
		if len(msgs) != 0 {
			t.Fatalf("a note at Seq %d surfaced %d messages", seq, len(msgs))
		}
	}

	// A chain counting up through zero surfaces only Seq 1..3.
	store2 := tipStore{NewMemStore()}
	spammer2 := newTestClient(t, store2)
	rob2 := newTestClient(t, store2)
	prev := ""
	for seq := int64(-20); seq <= PendingMessageLimit; seq++ {
		prev = plantNote(t, store2, spammer2.ID, rob2, textMsg(t, "countdown"), prev, seq, now()).Hash()
	}
	msgs, err := rob2.Receive(&Conversation{PeerID: spammer2.ID.ID()})
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) > PendingMessageLimit {
		t.Fatalf("a chain through zero surfaced %d messages", len(msgs))
	}

	if _, err := NewNote(spammer.ID, rob.ID.ID(), []string{strings.Repeat("0", 64)}, "", 0); err == nil {
		t.Fatal("NewNote must refuse Seq 0")
	}
}

// deliverable returns a note sent by from to to, with its chunks, as a
// Courier would hand it over.
func deliverable(t *testing.T, from *Client, conv *Conversation, text string) (*Note, map[string][]byte) {
	t.Helper()
	n, err := from.Send(conv, text)
	if err != nil {
		t.Fatal(err)
	}
	chunks, err := from.NoteChunks(n)
	if err != nil {
		t.Fatal(err)
	}
	return n, chunks
}

// Even a first note (Seq 1) can't be delivered directly by a stranger:
// a lone note proves nothing about its place in a chain.
func TestOpenDeliveredRefusedWhenUnaccepted(t *testing.T) {
	store := NewMemStore()
	bob := newTestClient(t, store)
	rob := newTestClient(t, store)

	n, chunks := deliverable(t, bob, &Conversation{PeerID: rob.ID.ID()}, "hi")
	if n.Seq != 1 {
		t.Fatalf("expected a first note, got Seq %d", n.Seq)
	}
	pending := &Conversation{PeerID: bob.ID.ID()}
	if _, err := rob.OpenDelivered(pending, n, chunks); err == nil {
		t.Fatal("direct delivery must be refused for an unaccepted conversation")
	}
	if len(pending.PeerAddrs) != 0 || pending.LastReadHash != "" {
		t.Fatal("a refused delivery must not touch the conversation")
	}
}

func TestOpenDeliveredRejectsExtraChunks(t *testing.T) {
	store := NewMemStore()
	bob := newTestClient(t, store)
	rob := newTestClient(t, store)

	n, chunks := deliverable(t, bob, &Conversation{PeerID: rob.ID.ID()}, "hi")
	chunks[strings.Repeat("ab", 32)] = make([]byte, ChunkSize)
	conv := &Conversation{PeerID: bob.ID.ID(), Accepted: true}
	if _, err := rob.OpenDelivered(conv, n, chunks); err == nil {
		t.Fatal("chunks the note doesn't reference must be rejected")
	}
}

// Notes and chunks are public, so anyone can replay an old one at us.
// That must not roll PeerAddrs back to where the peer used to be.
func TestReplayedNoteCannotRollBackPeerAddrs(t *testing.T) {
	store := NewMemStore()
	bob := newTestClient(t, store)
	rob := newTestClient(t, store)

	addr := "10.0.0.1:1"
	bob.SelfAddrs = func() []string { return []string{addr} }
	bobSide := &Conversation{PeerID: rob.ID.ID()}
	old, oldChunks := deliverable(t, bob, bobSide, "one")
	addr = "10.0.0.2:2"
	deliverable(t, bob, bobSide, "two")

	robSide := &Conversation{PeerID: bob.ID.ID(), Accepted: true}
	if _, err := rob.Receive(robSide); err != nil {
		t.Fatal(err)
	}
	if len(robSide.PeerAddrs) != 1 || robSide.PeerAddrs[0] != "10.0.0.2:2" {
		t.Fatalf("expected the newest address, got %v", robSide.PeerAddrs)
	}

	if _, err := rob.OpenDelivered(robSide, old, oldChunks); err != nil {
		t.Fatalf("the replayed note is genuine and should still open: %v", err)
	}
	if robSide.PeerAddrs[0] != "10.0.0.2:2" {
		t.Fatalf("a replay rolled PeerAddrs back to %v", robSide.PeerAddrs)
	}

	// The note that directly follows what Receive surfaced does update.
	addr = "10.0.0.3:3"
	next, nextChunks := deliverable(t, bob, bobSide, "three")
	if _, err := rob.OpenDelivered(robSide, next, nextChunks); err != nil {
		t.Fatal(err)
	}
	if robSide.PeerAddrs[0] != "10.0.0.3:3" {
		t.Fatalf("the next note should update PeerAddrs, got %v", robSide.PeerAddrs)
	}
}

func TestLearnedAddrsAreSanitized(t *testing.T) {
	store := NewMemStore()
	bob := newTestClient(t, store)
	rob := newTestClient(t, store)

	junk := []string{"", "has space:1", "ctl\x01:1", "del\x7f:1", "é:1", strings.Repeat("a", maxAddrLen+1)}
	var good []string
	for i := 0; i < 3*maxPeerAddrs; i++ {
		good = append(good, "10.0.0.1:"+strings.Repeat("9", 1+i%4))
	}
	bob.SelfAddrs = func() []string { return append(append([]string(nil), junk...), good...) }
	deliverable(t, bob, &Conversation{PeerID: rob.ID.ID()}, "hi")

	robSide := &Conversation{PeerID: bob.ID.ID()}
	if _, err := rob.Receive(robSide); err != nil {
		t.Fatal(err)
	}
	if len(robSide.PeerAddrs) != maxPeerAddrs {
		t.Fatalf("expected %d addresses, got %d", maxPeerAddrs, len(robSide.PeerAddrs))
	}
	for _, a := range robSide.PeerAddrs {
		if !strings.HasPrefix(a, "10.0.0.1:") {
			t.Fatalf("junk address %q survived", a)
		}
	}
}

func TestBoundCandidates(t *testing.T) {
	var in [][]byte
	in = append(in, nil, make([]byte, maxRecordSize+1))
	for i := 0; i < 2*maxSlotCandidates; i++ {
		in = append(in, []byte("{}"))
	}
	out := boundCandidates(in, maxSlotCandidates)
	if len(out) != maxSlotCandidates {
		t.Fatalf("expected %d candidates, got %d", maxSlotCandidates, len(out))
	}
	for _, b := range out {
		if len(b) == 0 || len(b) > maxRecordSize {
			t.Fatal("an empty or oversized candidate survived")
		}
	}
}

// Planted chains at the doorbell cost a bounded number of fetches in
// total, and a genuine knock behind a wall of planted heads is still
// found, because heads are walked in rounds rather than one at a time.
func TestDoorbellFloodIsBoundedAndFair(t *testing.T) {
	mem := NewMemStore()
	store := &countingStore{Store: mem}
	rob := newTestClient(t, store)
	self := rob.ID.ID()

	const heads, depth = maxDoorbellCandidates - 1, 40
	for h := 0; h < heads; h++ {
		id, err := crypto.NewIdentity()
		if err != nil {
			t.Fatal(err)
		}
		prev := ""
		for d := 0; d < depth; d++ {
			k := &Knock{BoxKey: BoxPublicKeyToHex(rob.Box.Public), Recipient: self,
				Intro: strings.Repeat("0", 64), Prev: prev, Timestamp: now()}
			if err := k.Sign(id); err != nil {
				t.Fatal(err)
			}
			blob, _ := json.Marshal(k)
			key, _ := dht.IDFromHex(k.Hash())
			_ = mem.Put(key, blob)
			if d == depth-1 {
				_ = mem.Put(DoorbellKey(self), blob)
			}
			prev = k.Hash()
		}
	}
	// An oversized head is dropped unparsed and takes no place under the
	// cap; the genuine knock lands last among the candidates.
	_ = mem.Put(DoorbellKey(self), make([]byte, maxRecordSize+1))
	friend := newTestClient(t, store)
	if _, err := friend.Send(&Conversation{PeerID: self}, "hello"); err != nil {
		t.Fatal(err)
	}
	// Heads past the candidate cap are never looked at.
	for i := 0; i < 100; i++ {
		_ = mem.Put(DoorbellKey(self), []byte(strings.Repeat(" ", i)+"{}"))
	}

	store.gets.Store(0)
	knocks, err := rob.ReadKnocks()
	if err != nil {
		t.Fatal(err)
	}
	if got := store.gets.Load(); got > maxDoorbellTotalWalk {
		t.Fatalf("ReadKnocks made %d fetches, budget is %d", got, maxDoorbellTotalWalk)
	}
	found := false
	for _, k := range knocks {
		if id, _ := k.SenderID(); id == friend.ID.ID() {
			found = true
		}
	}
	if !found {
		t.Fatal("a genuine knock behind planted heads was lost")
	}
}

// Sig is outside the signed bytes and hex decoding ignores case, so
// without the canonical-form check anyone could re-case a signature and
// mint a second valid record with a different hash.
func TestRecasedSignatureRejected(t *testing.T) {
	store := NewMemStore()
	bob := newTestClient(t, store)
	rob := newTestClient(t, store)

	n, _ := deliverable(t, bob, &Conversation{PeerID: rob.ID.ID()}, "hi")
	knocks, err := rob.ReadKnocks()
	if err != nil || len(knocks) != 1 {
		t.Fatalf("expected one knock: %v", err)
	}
	prof, err := FetchProfile(store, bob.ID.ID())
	if err != nil {
		t.Fatal(err)
	}
	rec, err := newHandleClient(t, store).ClaimHandle("recased")
	if err != nil {
		t.Fatal(err)
	}

	type verifier interface{ Verify() error }
	cases := map[string]struct {
		v   verifier
		sig *string
	}{
		"note":    {n, &n.Sig},
		"knock":   {knocks[0], &knocks[0].Sig},
		"profile": {prof, &prof.Sig},
		"handle":  {rec, &rec.Sig},
	}
	for name, c := range cases {
		if err := c.v.Verify(); err != nil {
			t.Fatalf("%s: genuine record rejected: %v", name, err)
		}
		*c.sig = strings.ToUpper(*c.sig)
		if err := c.v.Verify(); err == nil {
			t.Fatalf("%s: re-cased signature accepted", name)
		}
	}
}

// The attack the canonical-signature check exists for: a re-cased copy
// of an already-read tip, planted in the slot, must not make an accepted
// conversation surface its whole history again.
func TestRecasedTipDoesNotResurfaceHistory(t *testing.T) {
	store := tipStore{NewMemStore()}
	bob := newTestClient(t, store)
	rob := newTestClient(t, store)

	bobSide := &Conversation{PeerID: rob.ID.ID()}
	var tip *Note
	for _, text := range []string{"a", "b", "c"} {
		n, err := bob.Send(bobSide, text)
		if err != nil {
			t.Fatal(err)
		}
		tip = n
	}
	robSide := &Conversation{PeerID: bob.ID.ID(), Accepted: true}
	if msgs, err := rob.Receive(robSide); err != nil || len(msgs) != 3 {
		t.Fatalf("expected 3 messages, got %d (%v)", len(msgs), err)
	}

	forged := *tip
	forged.Sig = strings.ToUpper(forged.Sig)
	putPlanted(t, store, &forged, MailboxKey(bob.ID.ID(), rob.ID.ID()))

	msgs, err := rob.Receive(robSide)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 0 {
		t.Fatalf("a re-cased tip resurfaced %d old messages", len(msgs))
	}
}

func TestFutureDatedRecordsNotBelieved(t *testing.T) {
	future := now() + 10*maxClockSkew

	t.Run("knock", func(t *testing.T) {
		store := NewMemStore()
		rob := newTestClient(t, store)
		liar, err := crypto.NewIdentity()
		if err != nil {
			t.Fatal(err)
		}
		k := &Knock{BoxKey: BoxPublicKeyToHex(rob.Box.Public), Recipient: rob.ID.ID(),
			Intro: strings.Repeat("0", 64), Timestamp: future}
		if err := k.Sign(liar); err != nil {
			t.Fatal(err)
		}
		putPlanted(t, store, k, DoorbellKey(rob.ID.ID()))

		// It would win "newest" forever; a writer must not chain after it.
		if tip, err := rob.doorbellTip(rob.ID.ID()); err == nil && tip.Hash() == k.Hash() {
			t.Fatal("a future-dated knock was chosen as the doorbell tip")
		}
		friend := newTestClient(t, store)
		if _, err := friend.Send(&Conversation{PeerID: rob.ID.ID()}, "hi"); err != nil {
			t.Fatal(err)
		}
		knocks, err := rob.ReadKnocks()
		if err != nil {
			t.Fatal(err)
		}
		if len(knocks) != 1 || knocks[0].Timestamp == future {
			t.Fatalf("expected only the genuine knock, got %d", len(knocks))
		}
		if knocks[0].Prev == k.Hash() {
			t.Fatal("a genuine knock chained after a future-dated one")
		}
	})

	t.Run("note", func(t *testing.T) {
		store := NewMemStore()
		bob := newTestClient(t, store)
		rob := newTestClient(t, store)
		plantNote(t, store, bob.ID, rob, textMsg(t, "from the future"), "", 1, future)
		msgs, err := rob.Receive(&Conversation{PeerID: bob.ID.ID(), Accepted: true})
		if err != nil {
			t.Fatal(err)
		}
		if len(msgs) != 0 {
			t.Fatal("a future-dated tip was believed")
		}
	})

	t.Run("profile", func(t *testing.T) {
		store := NewMemStore()
		id, err := crypto.NewIdentity()
		if err != nil {
			t.Fatal(err)
		}
		oldBox, _ := NewBoxKey()
		p := &Profile{PeerID: id.ID(), BoxKey: BoxPublicKeyToHex(oldBox.Public),
			Version: (future + 3600) * 1e9, Timestamp: future}
		if err := p.Sign(id); err != nil {
			t.Fatal(err)
		}
		blob, _ := json.Marshal(p)
		_ = store.Put(ProfileKey(id.ID()), blob)
		if _, err := FetchProfile(store, id.ID()); err == nil {
			t.Fatal("a future-dated profile was believed")
		}

		// A republish must outrank it, or it takes over when it comes due.
		newBox, _ := NewBoxKey()
		fresh, err := PublishProfile(store, id, newBox, "")
		if err != nil {
			t.Fatal(err)
		}
		if fresh.Version <= p.Version {
			t.Fatal("republish did not outrank the future-dated profile")
		}
	})

	t.Run("handle", func(t *testing.T) {
		rec := &HandleRecord{Timestamp: future}
		if !rec.Expired(now()) {
			t.Fatal("a future-dated handle claim was believed")
		}
	})
}

// U+212A KELVIN SIGN lowercases to 'k' under strings.ToLower. It must not
// alias a real handle.
func TestKelvinSignDoesNotAliasHandle(t *testing.T) {
	store := NewMemStore()
	kate := newHandleClient(t, store)
	if _, err := kate.ClaimHandle("kate"); err != nil {
		t.Fatal(err)
	}
	spoof := "Kate"
	if err := ValidHandle(spoof); err == nil {
		t.Fatal("a handle with a Kelvin sign passed validation")
	}
	if CanonicalHandle(spoof) == "kate" || HandleKey(spoof) == HandleKey("kate") {
		t.Fatal("a Kelvin sign canonicalised onto a real handle")
	}
	if _, err := ResolveHandle(store, spoof); err == nil {
		t.Fatal("a Kelvin-sign handle resolved")
	}
	if _, err := LookupUsername(store, spoof); err == nil {
		t.Fatal("a Kelvin-sign handle looked up")
	}
	// Plain ASCII case-folding still works.
	if rec, err := ResolveHandle(store, "@KATE"); err != nil || rec.Username != kate.ID.ID() {
		t.Fatalf("upper-case ASCII should still resolve: %v", err)
	}
}

func TestLookupUsernameCanonicalizes(t *testing.T) {
	store := NewMemStore()
	c := newHandleClient(t, store)
	got, err := LookupUsername(store, "  "+strings.ToUpper(c.ID.ID())+" ")
	if err != nil || got != c.ID.ID() {
		t.Fatalf("upper-case username: got %q, %v", got, err)
	}
	if _, err := LookupUsername(store, strings.Repeat("g", 64)); err == nil {
		t.Fatal("a 64-character non-hex string was accepted as a username")
	}
}

// In authority mode nothing binds Username to a key, so an upper-cased
// one must be refused: it would derive different keys from the real one.
func TestResolveHandleRejectsNonCanonicalUsername(t *testing.T) {
	store := NewMemStore()
	authority, err := crypto.NewIdentity()
	if err != nil {
		t.Fatal(err)
	}
	SetTrustedHandleAuthorities([]string{crypto.PublicKeyToHex(authority.PublicKey)})
	t.Cleanup(func() { SetTrustedHandleAuthorities(nil) })

	user, _ := crypto.NewIdentity()
	rec := &HandleRecord{Handle: "shouty", Username: strings.ToUpper(user.ID()),
		Version: 1, Timestamp: now(), TTL: HandleTTL}
	if err := rec.Sign(authority); err != nil {
		t.Fatal(err)
	}
	blob, _ := rec.Marshal()
	_ = store.Put(HandleKey("shouty"), blob)
	if _, err := ResolveHandle(store, "shouty"); err == nil {
		t.Fatal("an upper-cased username resolved")
	}
}

func TestMalformedMessageIDRejected(t *testing.T) {
	for _, id := range []string{"", strings.Repeat("a", maxMessageIDLen+1)} {
		store := NewMemStore()
		bob := newTestClient(t, store)
		rob := newTestClient(t, store)
		msg := textMsg(t, "odd id")
		msg.ID = id
		n := plantNote(t, store, bob.ID, rob, msg, "", 1, now())

		conv := &Conversation{PeerID: bob.ID.ID(), Accepted: true}
		msgs, err := rob.Receive(conv)
		if err != nil {
			t.Fatal(err)
		}
		if len(msgs) != 0 {
			t.Fatalf("a message with a %d-byte ID was surfaced", len(id))
		}
		chunks, err := rob.NoteChunks(n)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := rob.OpenDelivered(&Conversation{PeerID: bob.ID.ID(), Accepted: true}, n, chunks); err == nil {
			t.Fatalf("a delivered message with a %d-byte ID was opened", len(id))
		}
	}
}

// Once a pending conversation's cursor has used up the allowance,
// further notes from the stranger must not each cost a chain walk.
func TestExhaustedPendingConversationSkipsWalk(t *testing.T) {
	mem := NewMemStore()
	store := &countingStore{Store: mem}
	spammer := newTestClient(t, store)
	rob := newTestClient(t, store)

	spamSide := &Conversation{PeerID: rob.ID.ID()}
	for i := 0; i < PendingMessageLimit; i++ {
		if _, err := spammer.Send(spamSide, "x"); err != nil {
			t.Fatal(err)
		}
	}
	robSide := &Conversation{PeerID: spammer.ID.ID()}
	if msgs, _ := rob.Receive(robSide); len(msgs) != PendingMessageLimit {
		t.Fatalf("expected %d messages", PendingMessageLimit)
	}
	for i := 0; i < 50; i++ {
		if _, err := spammer.Send(spamSide, "more"); err != nil {
			t.Fatal(err)
		}
	}
	store.gets.Store(0)
	msgs, err := rob.Receive(robSide)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 0 {
		t.Fatalf("an exhausted pending conversation surfaced %d more", len(msgs))
	}
	if got := store.gets.Load(); got > 2 {
		t.Fatalf("Receive made %d fetches on an exhausted pending conversation", got)
	}

	// Accepting (and rewinding, as the engine does) still reveals it all.
	robSide.Accepted = true
	robSide.LastReadHash = ""
	all, err := rob.Receive(robSide)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != PendingMessageLimit+50 {
		t.Fatalf("after accepting, expected %d messages, got %d", PendingMessageLimit+50, len(all))
	}
}
