package mailbox

import (
	"bytes"
	"strings"
	"testing"

	"altnet/core/crypto"
)

// newTestClient spins up an identity + box key on a shared store.
func newTestClient(t *testing.T, s Store) *Client {
	t.Helper()
	id, err := crypto.NewIdentity()
	if err != nil {
		t.Fatal(err)
	}
	box, err := NewBoxKey()
	if err != nil {
		t.Fatal(err)
	}
	c := &Client{Store: s, ID: id, Box: box}
	if _, err := c.PublishProfile(""); err != nil {
		t.Fatal(err)
	}
	return c
}

// The headline scenario from the design conversation: Rob hands Bob his
// username in real life, Bob types it in and sends. Rob knows nothing
// about Bob beforehand, yet discovers the message.
func TestColdContactViaUsernameOnly(t *testing.T) {
	store := NewMemStore()
	bob := newTestClient(t, store)
	rob := newTestClient(t, store)

	// Bob has ONLY Rob's peer ID -- no prior contact, no exchange.
	bobSide := &Conversation{PeerID: rob.ID.ID()}
	if _, err := bob.Send(bobSide, "hey rob, it's bob from the conference"); err != nil {
		t.Fatal(err)
	}

	// Rob watches exactly one address -- his own doorbell -- and finds it.
	knocks, err := rob.ReadKnocks()
	if err != nil {
		t.Fatal(err)
	}
	if len(knocks) != 1 {
		t.Fatalf("expected 1 knock, got %d", len(knocks))
	}

	robSide, err := ConversationFromKnock(knocks[0])
	if err != nil {
		t.Fatal(err)
	}
	if robSide.PeerID != bob.ID.ID() {
		t.Fatal("knock must reveal the sender's peer ID")
	}
	if robSide.Accepted {
		t.Fatal("a fresh knock must land as a pending request, not accepted")
	}

	msgs, err := rob.Receive(robSide)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 1 || msgs[0].Text != "hey rob, it's bob from the conference" {
		t.Fatalf("did not recover the message: %+v", msgs)
	}
}

// A stranger who never got Rob's username can't find anything, even
// though the mailbox address for their pair mathematically exists.
func TestUnrelatedPeerLearnsNothing(t *testing.T) {
	store := NewMemStore()
	bob := newTestClient(t, store)
	rob := newTestClient(t, store)
	eve := newTestClient(t, store)

	conv := &Conversation{PeerID: rob.ID.ID()}
	if _, err := bob.Send(conv, "private plans"); err != nil {
		t.Fatal(err)
	}

	// Eve's own doorbell is empty -- Bob knocked on Rob's, not hers.
	knocks, err := eve.ReadKnocks()
	if err != nil {
		t.Fatal(err)
	}
	if len(knocks) != 0 {
		t.Fatalf("eve should see no knocks, got %d", len(knocks))
	}

	// Even knowing both IDs, Eve can't decrypt: she can locate the
	// mailbox slot but not open what's in it.
	eveSnooping := &Conversation{PeerID: bob.ID.ID()}
	msgs, err := eve.Receive(eveSnooping)
	if err == nil && len(msgs) > 0 {
		t.Fatal("eve must not be able to read a conversation she isn't part of")
	}
}

// Offline catch-up: the recipient misses several messages and recovers
// all of them in order by walking the chain backward.
func TestOfflineCatchUpWalksWholeChain(t *testing.T) {
	store := NewMemStore()
	bob := newTestClient(t, store)
	rob := newTestClient(t, store)

	bobSide := &Conversation{PeerID: rob.ID.ID()}
	sent := []string{"first", "second", "third", "fourth", "fifth"}
	for _, text := range sent {
		if _, err := bob.Send(bobSide, text); err != nil {
			t.Fatal(err)
		}
	}

	knocks, _ := rob.ReadKnocks()
	if len(knocks) != 1 {
		t.Fatalf("expected exactly 1 knock for 5 messages, got %d", len(knocks))
	}
	robSide, _ := ConversationFromKnock(knocks[0])
	robSide.Accepted = true // accepted, so no pending cap

	msgs, err := rob.Receive(robSide)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != len(sent) {
		t.Fatalf("expected %d messages, got %d", len(sent), len(msgs))
	}
	for i, want := range sent {
		if msgs[i].Text != want {
			t.Fatalf("message %d: got %q, want %q (order must be oldest-first)", i, msgs[i].Text, want)
		}
	}

	// A second read returns nothing new.
	again, err := rob.Receive(robSide)
	if err != nil {
		t.Fatal(err)
	}
	if len(again) != 0 {
		t.Fatalf("expected no new messages on re-read, got %d", len(again))
	}

	// ...until another arrives.
	if _, err := bob.Send(bobSide, "sixth"); err != nil {
		t.Fatal(err)
	}
	more, err := rob.Receive(robSide)
	if err != nil {
		t.Fatal(err)
	}
	if len(more) != 1 || more[0].Text != "sixth" {
		t.Fatalf("expected only the new message, got %+v", more)
	}
}

// The 3-message request cap is enforced by the RECEIVER, so a sender
// running modified software that ignores the limit gains nothing.
func TestPendingRequestCapIsReceiverEnforced(t *testing.T) {
	store := NewMemStore()
	spammer := newTestClient(t, store)
	rob := newTestClient(t, store)

	conv := &Conversation{PeerID: rob.ID.ID()}
	for _, text := range []string{"1", "2", "3", "4", "5", "6", "7"} {
		if _, err := spammer.Send(conv, text); err != nil {
			t.Fatal(err)
		}
	}

	knocks, _ := rob.ReadKnocks()
	robSide, _ := ConversationFromKnock(knocks[0])

	msgs, err := rob.Receive(robSide)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != PendingMessageLimit {
		t.Fatalf("unaccepted sender: expected %d messages, got %d", PendingMessageLimit, len(msgs))
	}
	// It must be the FIRST three, not the most recent three.
	for i, want := range []string{"1", "2", "3"} {
		if msgs[i].Text != want {
			t.Fatalf("expected first %d messages; got %q at %d", PendingMessageLimit, msgs[i].Text, i)
		}
	}

	// Accepting lifts the cap and reveals the backlog from the start.
	robSide.Accepted = true
	robSide.LastReadHash = ""
	all, err := rob.Receive(robSide)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 7 {
		t.Fatalf("after accepting, expected all 7 messages, got %d", len(all))
	}
}

// Accepting a request needs no signal back to the sender: the sender was
// already watching the reply direction from the moment they sent.
func TestReplyFlowsWithoutAnAcceptSignal(t *testing.T) {
	store := NewMemStore()
	bob := newTestClient(t, store)
	rob := newTestClient(t, store)

	bobSide := &Conversation{PeerID: rob.ID.ID()}
	if _, err := bob.Send(bobSide, "hi rob"); err != nil {
		t.Fatal(err)
	}

	knocks, _ := rob.ReadKnocks()
	robSide, _ := ConversationFromKnock(knocks[0])
	if _, err := rob.Receive(robSide); err != nil {
		t.Fatal(err)
	}
	robSide.Accepted = true // purely local UI state

	if _, err := rob.Send(robSide, "hey bob, good to hear from you"); err != nil {
		t.Fatal(err)
	}

	// Bob never needed an "accepted" notification -- he just reads the
	// reply direction he could always compute.
	replies, err := bob.Receive(bobSide)
	if err != nil {
		t.Fatal(err)
	}
	if len(replies) != 1 || replies[0].Text != "hey bob, good to hear from you" {
		t.Fatalf("bob should see the reply, got %+v", replies)
	}
}

// Many unrelated strangers knock on one doorbell; none overwrite another.
func TestManyStrangersKnockWithoutCollision(t *testing.T) {
	store := NewMemStore()
	rob := newTestClient(t, store)

	const strangers = 5
	want := make(map[string]bool)
	for i := 0; i < strangers; i++ {
		s := newTestClient(t, store)
		conv := &Conversation{PeerID: rob.ID.ID()}
		if _, err := s.Send(conv, "hello from a stranger"); err != nil {
			t.Fatal(err)
		}
		want[s.ID.ID()] = true
	}

	knocks, err := rob.ReadKnocks()
	if err != nil {
		t.Fatal(err)
	}
	if len(knocks) != strangers {
		t.Fatalf("expected %d knocks, got %d -- a knock was lost", strangers, len(knocks))
	}
	for _, k := range knocks {
		id, err := k.SenderID()
		if err != nil {
			t.Fatal(err)
		}
		if !want[id] {
			t.Fatalf("unexpected knocker %.8s", id)
		}
		delete(want, id)
	}
	if len(want) != 0 {
		t.Fatalf("%d knockers went missing", len(want))
	}
}

// A banned identity goes silent everywhere at once: not just for new
// contacts, but inside an established, already-accepted conversation.
func TestBannedIdentityIsSilencedInExistingConversations(t *testing.T) {
	store := NewMemStore()
	bob := newTestClient(t, store)
	rob := newTestClient(t, store)

	bobSide := &Conversation{PeerID: rob.ID.ID()}
	if _, err := bob.Send(bobSide, "message before the ban"); err != nil {
		t.Fatal(err)
	}
	knocks, _ := rob.ReadKnocks()
	robSide, _ := ConversationFromKnock(knocks[0])
	robSide.Accepted = true
	if msgs, _ := rob.Receive(robSide); len(msgs) != 1 {
		t.Fatalf("expected the pre-ban message to arrive, got %d", len(msgs))
	}

	// Operator bans Bob's identity; Rob's client honours it.
	bans := NewBanList()
	rob.Bans = bans
	if !bans.Add(crypto.PublicKeyToHex(bob.ID.PublicKey)) {
		t.Fatal("ban should have been newly added")
	}

	if _, err := bob.Send(bobSide, "message after the ban"); err != nil {
		t.Fatal(err)
	}
	after, err := rob.Receive(robSide)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != 0 {
		t.Fatalf("banned sender must be silenced in an existing conversation, got %d messages", len(after))
	}

	// Their doorbell knock stops surfacing too, so they can't reappear
	// as a fresh contact request either.
	if ks, _ := rob.ReadKnocks(); len(ks) != 0 {
		t.Fatalf("banned identity's knock must not surface, got %d", len(ks))
	}
}

// A profile planted at someone else's ProfileKey must not resolve: the
// embedded key has to hash to the peer ID being looked up.
func TestProfileImpersonationRejected(t *testing.T) {
	store := NewMemStore()
	rob := newTestClient(t, store)
	eve := newTestClient(t, store)

	// Eve signs a profile claiming to be Rob and plants it at Rob's key.
	forged := &Profile{
		PeerID:  rob.ID.ID(),
		BoxKey:  BoxPublicKeyToHex(eve.Box.Public),
		Version: 1 << 62, // try to outrank the real one
	}
	if err := forged.Sign(eve.ID); err != nil {
		t.Fatal(err)
	}
	blob, _ := forged.Marshal()
	if err := store.Put(ProfileKey(rob.ID.ID()), blob); err != nil {
		t.Fatal(err)
	}

	got, err := FetchProfile(store, rob.ID.ID())
	if err != nil {
		t.Fatal(err)
	}
	if got.BoxKey == BoxPublicKeyToHex(eve.Box.Public) {
		t.Fatal("forged profile must not win -- key does not hash to the peer ID")
	}
	if got.BoxKey != BoxPublicKeyToHex(rob.Box.Public) {
		t.Fatal("expected the genuine profile to resolve")
	}
}

// A note planted in someone else's mailbox slot by a third party must
// not be surfaced, even though it carries a valid signature.
func TestPlantedNoteInMailboxSlotRejected(t *testing.T) {
	store := NewMemStore()
	bob := newTestClient(t, store)
	rob := newTestClient(t, store)
	eve := newTestClient(t, store)

	// Eve writes a validly-signed note into the Bob->Rob slot.
	forged, err := NewNote(eve.ID, rob.ID.ID(), []string{"deadbeef"}, "", 99)
	if err != nil {
		t.Fatal(err)
	}
	blob, _ := forged.Marshal()
	if err := store.Put(MailboxKey(bob.ID.ID(), rob.ID.ID()), blob); err != nil {
		t.Fatal(err)
	}

	robSide := &Conversation{PeerID: bob.ID.ID(), Accepted: true}
	msgs, err := rob.Receive(robSide)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 0 {
		t.Fatalf("a note from the wrong signer must be ignored, got %d messages", len(msgs))
	}
}

// Oversized text is refused rather than silently truncated.
func TestOversizedTextRefused(t *testing.T) {
	store := NewMemStore()
	bob := newTestClient(t, store)
	rob := newTestClient(t, store)

	bobSide := &Conversation{PeerID: rob.ID.ID()}
	if _, err := bob.Send(bobSide, strings.Repeat("a", MaxTextLen+1)); err == nil {
		t.Fatal("expected a message beyond MaxTextLen to be refused")
	}
}

// Payloads spanning several chunks survive seal -> split -> join ->
// open intact. Text messages are capped below ChunkSize so they always
// fit in one piece; this exercises the path a future attachment kind
// will take, and proves reassembly order is preserved.
func TestMultiChunkPayloadRoundTrip(t *testing.T) {
	box, err := NewBoxKey()
	if err != nil {
		t.Fatal(err)
	}
	payload := make([]byte, ChunkSize*2+1234)
	for i := range payload {
		payload[i] = byte(i % 251)
	}

	sealed, err := Seal(box.Public, payload)
	if err != nil {
		t.Fatal(err)
	}
	chunks, err := SplitChunks(sealed)
	if err != nil {
		t.Fatal(err)
	}
	if len(chunks) != 3 {
		t.Fatalf("expected 3 chunks, got %d", len(chunks))
	}
	for _, ch := range chunks {
		if ContentAddress(ch.Data) != ch.Key {
			t.Fatal("each chunk must be addressed by its own hash")
		}
		if len(ch.Data) > ChunkSize {
			t.Fatalf("chunk exceeds ChunkSize: %d", len(ch.Data))
		}
	}

	pieces := make([][]byte, 0, len(chunks))
	for _, ch := range chunks {
		pieces = append(pieces, ch.Data)
	}
	opened, err := Open(box.Private, JoinChunks(pieces))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(opened, payload) {
		t.Fatal("multi-chunk payload did not round-trip intact")
	}

	// Reassembling out of order must fail the AEAD check, not silently
	// yield garbage.
	pieces[0], pieces[1] = pieces[1], pieces[0]
	if _, err := Open(box.Private, JoinChunks(pieces)); err == nil {
		t.Fatal("expected mis-ordered reassembly to be detected")
	}
}
