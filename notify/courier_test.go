package notify

import (
	"sync"
	"testing"
	"time"

	"altnet-relay/mailbox"
)

// wireCourier gives a node direct-delivery capability and makes it
// advertise its own addresses inside outgoing sealed messages.
func wireCourier(n *node) *Courier {
	n.client.SelfAddrs = n.peer.AdvertisedAddresses
	return NewCourier(n.peer, n.client)
}

// Direct delivery only becomes possible after the peer has written to us
// once, because that is when their address arrives -- inside the
// encrypted envelope, never from anything public.
func TestAddressesTravelInsideEncryptedMessagesOnly(t *testing.T) {
	bob := newNode(t)
	rob := newNode(t)
	wireCourier(bob)
	wireCourier(rob)
	join(t, bob, rob)

	if _, err := rob.client.PublishProfile(""); err != nil {
		t.Fatal(err)
	}

	// Rob's public profile must not reveal where his device is.
	prof, err := mailbox.FetchProfile(rob.client.Store, rob.id.Username())
	if err != nil {
		t.Fatal(err)
	}
	for _, addr := range rob.peer.AdvertisedAddresses() {
		if prof.BoxKey == addr {
			t.Fatal("profile must not carry node addresses")
		}
	}

	// Bob writes first, so Bob's address reaches Rob...
	bobSide := &mailbox.Conversation{PeerID: rob.id.Username()}
	if len(bobSide.PeerAddrs) != 0 {
		t.Fatal("a fresh conversation must not know the peer's address")
	}
	if _, err := bob.client.Send(bobSide, "hello"); err != nil {
		t.Fatal(err)
	}

	knocks, err := rob.client.ReadKnocks()
	if err != nil || len(knocks) == 0 {
		t.Fatalf("expected a knock: %v", err)
	}
	robSide, _ := mailbox.ConversationFromKnock(knocks[0])
	if _, err := rob.client.Receive(robSide); err != nil {
		t.Fatal(err)
	}
	if len(robSide.PeerAddrs) == 0 {
		t.Fatal("rob should have learned bob's address from the message")
	}

	// ...but Bob still knows nothing about Rob, because Rob hasn't
	// answered. That asymmetry is the point.
	if len(bobSide.PeerAddrs) != 0 {
		t.Fatal("bob must not learn rob's address before rob replies")
	}

	// Once Rob replies, Bob learns it too.
	robSide.Accepted = true
	if _, err := rob.client.Send(robSide, "hi back"); err != nil {
		t.Fatal(err)
	}
	if _, err := bob.client.Receive(bobSide); err != nil {
		t.Fatal(err)
	}
	if len(bobSide.PeerAddrs) == 0 {
		t.Fatal("bob should have learned rob's address from the reply")
	}
}

// A directly-delivered message arrives without the recipient consulting
// the DHT at all: the note and its chunk bytes come over the wire
// together.
func TestDirectDeliveryArrivesWithoutDHTFetch(t *testing.T) {
	bob := newNode(t)
	rob := newNode(t)
	bobCourier := wireCourier(bob)
	wireCourier(rob)
	join(t, bob, rob)

	if _, err := bob.client.PublishProfile(""); err != nil {
		t.Fatal(err)
	}
	if _, err := rob.client.PublishProfile(""); err != nil {
		t.Fatal(err)
	}

	var mu sync.Mutex
	var got []string

	robInbox := NewInbox(rob.client, rob.hub).WithCourier(NewCourier(rob.peer, rob.client))
	robInbox.OnMessages = func(c *mailbox.Conversation, msgs []*mailbox.Message) {
		mu.Lock()
		c.Accepted = true
		for _, m := range msgs {
			got = append(got, m.Text)
		}
		mu.Unlock()
	}
	robInbox.Start()
	defer robInbox.Stop()

	// First message goes the normal way and teaches Rob where Bob is.
	bobSide := &mailbox.Conversation{PeerID: rob.id.Username()}
	if _, err := bob.client.Send(bobSide, "opening"); err != nil {
		t.Fatal(err)
	}
	if !waitFor(t, 5*time.Second, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(got) == 1
	}) {
		t.Fatal("first message never arrived")
	}

	// Rob replies, which is how Bob learns Rob's address.
	knocks, _ := rob.client.ReadKnocks()
	robSide, _ := mailbox.ConversationFromKnock(knocks[0])
	robSide.Accepted = true
	if _, err := rob.client.Send(robSide, "ack"); err != nil {
		t.Fatal(err)
	}
	if _, err := bob.client.Receive(bobSide); err != nil {
		t.Fatal(err)
	}
	if len(bobSide.PeerAddrs) == 0 {
		t.Fatal("bob needs rob's address for the direct path")
	}

	// Now deliver directly and confirm it lands.
	note, err := bob.client.Send(bobSide, "straight to you")
	if err != nil {
		t.Fatal(err)
	}
	chunks, err := bob.client.NoteChunks(note)
	if err != nil {
		t.Fatal(err)
	}
	if !bobCourier.Deliver(bobSide.PeerAddrs, note, chunks) {
		t.Fatal("direct delivery was not accepted by any address")
	}

	if !waitFor(t, 5*time.Second, func() bool {
		mu.Lock()
		defer mu.Unlock()
		for _, g := range got {
			if g == "straight to you" {
				return true
			}
		}
		return false
	}) {
		t.Fatal("directly delivered message never surfaced")
	}
}

// The same message arriving both directly and via the DHT must be shown
// exactly once.
func TestDuplicateAcrossBothPathsShownOnce(t *testing.T) {
	bob := newNode(t)
	rob := newNode(t)
	bobCourier := wireCourier(bob)
	wireCourier(rob)
	join(t, bob, rob)

	if _, err := bob.client.PublishProfile(""); err != nil {
		t.Fatal(err)
	}
	if _, err := rob.client.PublishProfile(""); err != nil {
		t.Fatal(err)
	}

	var mu sync.Mutex
	counts := map[string]int{}

	robInbox := NewInbox(rob.client, rob.hub).WithCourier(NewCourier(rob.peer, rob.client))
	robInbox.OnMessages = func(c *mailbox.Conversation, msgs []*mailbox.Message) {
		mu.Lock()
		c.Accepted = true
		for _, m := range msgs {
			counts[m.Text]++
		}
		mu.Unlock()
	}
	robInbox.Start()
	defer robInbox.Stop()

	bobSide := &mailbox.Conversation{PeerID: rob.id.Username()}
	if _, err := bob.client.Send(bobSide, "opening"); err != nil {
		t.Fatal(err)
	}
	if !waitFor(t, 5*time.Second, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return counts["opening"] > 0
	}) {
		t.Fatal("first message never arrived")
	}

	knocks, _ := rob.client.ReadKnocks()
	robSide, _ := mailbox.ConversationFromKnock(knocks[0])
	robSide.Accepted = true
	if _, err := rob.client.Send(robSide, "ack"); err != nil {
		t.Fatal(err)
	}
	if _, err := bob.client.Receive(bobSide); err != nil {
		t.Fatal(err)
	}

	// Send once, then push it directly as well -- the DHT notification
	// and the direct delivery both describe the same message.
	note, err := bob.client.Send(bobSide, "exactly once please")
	if err != nil {
		t.Fatal(err)
	}
	chunks, _ := bob.client.NoteChunks(note)
	bobCourier.Deliver(bobSide.PeerAddrs, note, chunks)

	// Force the DHT path to run too, so both routes definitely fire.
	time.Sleep(500 * time.Millisecond)
	robInbox.PollAll()
	time.Sleep(500 * time.Millisecond)

	mu.Lock()
	defer mu.Unlock()
	if n := counts["exactly once please"]; n != 1 {
		t.Fatalf("message shown %d times, want exactly 1", n)
	}
}

// A direct delivery carrying chunk bytes that don't match the hashes the
// note claims must be rejected -- a peer handing us data directly is
// strictly less trustworthy than content-addressed storage.
func TestTamperedDirectDeliveryRejected(t *testing.T) {
	bob := newNode(t)
	rob := newNode(t)
	join(t, bob, rob)

	if _, err := rob.client.PublishProfile(""); err != nil {
		t.Fatal(err)
	}

	bobSide := &mailbox.Conversation{PeerID: rob.id.Username()}
	note, err := bob.client.Send(bobSide, "genuine")
	if err != nil {
		t.Fatal(err)
	}
	chunks, err := bob.client.NoteChunks(note)
	if err != nil {
		t.Fatal(err)
	}

	// Corrupt the payload while keeping the note's signature intact.
	for k := range chunks {
		bad := append([]byte(nil), chunks[k]...)
		bad[0] ^= 0xFF
		chunks[k] = bad
	}

	robSide := &mailbox.Conversation{PeerID: bob.id.Username(), Accepted: true}
	if _, err := rob.client.OpenDelivered(robSide, note, chunks); err == nil {
		t.Fatal("tampered chunk data must be rejected")
	}
}

// An unaccepted sender cannot use the direct path to get past the
// message-request cap that the DHT path enforces.
func TestDirectDeliveryRespectsPendingLimit(t *testing.T) {
	bob := newNode(t)
	rob := newNode(t)
	join(t, bob, rob)

	if _, err := rob.client.PublishProfile(""); err != nil {
		t.Fatal(err)
	}

	bobSide := &mailbox.Conversation{PeerID: rob.id.Username()}
	var last *mailbox.Note
	for i := 0; i < mailbox.PendingMessageLimit+1; i++ {
		n, err := bob.client.Send(bobSide, "spam")
		if err != nil {
			t.Fatal(err)
		}
		last = n
	}
	chunks, err := bob.client.NoteChunks(last)
	if err != nil {
		t.Fatal(err)
	}

	pending := &mailbox.Conversation{PeerID: bob.id.Username()} // not accepted
	if _, err := rob.client.OpenDelivered(pending, last, chunks); err == nil {
		t.Fatal("direct delivery must respect the unaccepted-sender limit")
	}

	// Accepting lifts it.
	pending.Accepted = true
	if _, err := rob.client.OpenDelivered(pending, last, chunks); err != nil {
		t.Fatalf("accepted conversation should allow it: %v", err)
	}
}

// The sender now pushes a message before storing it. The recipient must show
// it at once -- with nothing on the network to fetch yet -- and must not show
// it a second time when the stored copy turns up.
func TestPushedBeforeStoredShowsOnceAndCursorStaysSound(t *testing.T) {
	bob := newNode(t)
	rob := newNode(t)
	bobCourier := wireCourier(bob)
	wireCourier(rob)
	join(t, bob, rob)
	for _, n := range []*node{bob, rob} {
		if _, err := n.client.PublishProfile(""); err != nil {
			t.Fatal(err)
		}
	}

	var mu sync.Mutex
	counts := map[string]int{}
	robInbox := NewInbox(rob.client, rob.hub).WithCourier(NewCourier(rob.peer, rob.client))
	robInbox.OnMessages = func(c *mailbox.Conversation, msgs []*mailbox.Message) {
		mu.Lock()
		c.Accepted = true
		for _, m := range msgs {
			counts[m.Text]++
		}
		mu.Unlock()
	}
	robInbox.Start()
	defer robInbox.Stop()
	seen := func(text string) int {
		mu.Lock()
		defer mu.Unlock()
		return counts[text]
	}

	// Set up the direct path: one message each way.
	bobSide := &mailbox.Conversation{PeerID: rob.id.Username()}
	if _, err := bob.client.Send(bobSide, "opening"); err != nil {
		t.Fatal(err)
	}
	if !waitFor(t, 5*time.Second, func() bool { return seen("opening") == 1 }) {
		t.Fatal("first message never arrived")
	}
	knocks, _ := rob.client.ReadKnocks()
	robSide, _ := mailbox.ConversationFromKnock(knocks[0])
	robSide.Accepted = true
	if _, err := rob.client.Send(robSide, "ack"); err != nil {
		t.Fatal(err)
	}
	if _, err := bob.client.Receive(bobSide); err != nil || len(bobSide.PeerAddrs) == 0 {
		t.Fatalf("bob needs rob's address: %v", err)
	}

	// Prepare, push, and only then store.
	out, err := bob.client.Prepare(bobSide, mailbox.KindText, "pushed first")
	if err != nil {
		t.Fatal(err)
	}
	if !bobCourier.Deliver(bobSide.PeerAddrs, out.Note, out.Chunks) {
		t.Fatal("push not accepted")
	}
	if !waitFor(t, 5*time.Second, func() bool { return seen("pushed first") == 1 }) {
		t.Fatal("a pushed message must show before it is stored")
	}
	if err := bob.client.Commit(bobSide, out); err != nil {
		t.Fatal(err)
	}
	// Committing a second time, or a stale preparation, is refused.
	if err := bob.client.Commit(bobSide, out); err == nil {
		t.Fatal("a message must not be committed twice into the chain")
	}

	// The stored copy is found by a full check -- and recognised.
	robInbox.PollAll()
	if n := seen("pushed first"); n != 1 {
		t.Fatalf("shown %d times, want once", n)
	}
	// The chain is intact: the next message (sent the normal way) arrives.
	if _, err := bob.client.Send(bobSide, "after"); err != nil {
		t.Fatal(err)
	}
	robInbox.PollAll()
	if seen("after") != 1 {
		t.Fatal("the message after a pushed one must still arrive through the chain")
	}
}
