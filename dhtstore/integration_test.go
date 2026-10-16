package dhtstore

import (
	"testing"
	"time"

	"altnet/core/crypto"
	"altnet/core/dht"
	"altnet/core/peer"

	"altnet-relay/mailbox"
)

// node is one real AltNet node: a live TCP listener, a real DHT, and a
// Relay client on top of it.
type node struct {
	peer   *peer.Peer
	dht    *dht.DHT
	client *mailbox.Client
	id     *mailbox.Identity
}

// newNode starts a real peer on a free localhost port with a real DHT,
// and gives it a Relay identity whose key material is separate from the
// node's peer key -- the separation the whole privacy model rests on.
func newNode(t *testing.T) *node {
	t.Helper()

	nodeID, err := crypto.NewIdentity()
	if err != nil {
		t.Fatal(err)
	}
	p := peer.New(nodeID, "127.0.0.1:0")
	if err := p.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.Stop)

	d, err := dht.New(p)
	if err != nil {
		t.Fatal(err)
	}

	relayID, err := mailbox.LoadOrCreateIdentity(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if relayID.Username() == nodeID.ID() {
		t.Fatal("relay identity must not be the node identity")
	}

	return &node{
		peer:   p,
		dht:    d,
		client: mailbox.NewClient(New(d), relayID, nil),
		id:     relayID,
	}
}

// join wires every node into one DHT by bootstrapping them all through
// the first, then letting the routing tables settle.
func join(t *testing.T, nodes ...*node) {
	t.Helper()
	seed := nodes[0]
	for _, n := range nodes[1:] {
		if err := n.dht.Bootstrap(seed.peer.LocalAddr()); err != nil {
			t.Fatalf("bootstrap: %v", err)
		}
	}
	// Let lookups propagate contacts between the non-seed nodes.
	for _, n := range nodes {
		n.dht.Lookup(n.dht.Self())
	}
	time.Sleep(200 * time.Millisecond)
}

// The headline scenario, now over real sockets and a real Kademlia DHT
// rather than an in-memory map: Rob hands Bob his username, and that is
// the only thing Bob needs.
func TestColdContactOverRealNetwork(t *testing.T) {
	bob := newNode(t)
	rob := newNode(t)
	relay := newNode(t) // an uninvolved third node, just carrying the network
	join(t, bob, rob, relay)

	// Both publish their profiles so they're reachable by username.
	if _, err := bob.client.PublishProfile(""); err != nil {
		t.Fatal(err)
	}
	if _, err := rob.client.PublishProfile(""); err != nil {
		t.Fatal(err)
	}

	// Bob knows nothing but the username Rob gave him in person.
	conv := &mailbox.Conversation{PeerID: rob.id.Username()}
	const text = "hey rob, it's bob - got your handle at the meetup"
	if _, err := bob.client.Send(conv, text); err != nil {
		t.Fatalf("send over real network: %v", err)
	}

	// Rob watches exactly one address: his own doorbell.
	knocks, err := rob.client.ReadKnocks()
	if err != nil {
		t.Fatal(err)
	}
	if len(knocks) != 1 {
		t.Fatalf("expected 1 knock over the network, got %d", len(knocks))
	}

	robSide, err := mailbox.ConversationFromKnock(knocks[0])
	if err != nil {
		t.Fatal(err)
	}
	if robSide.PeerID != bob.id.Username() {
		t.Fatal("knock must reveal the sender's username")
	}

	msgs, err := rob.client.Receive(robSide)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 1 || msgs[0].Text != text {
		t.Fatalf("message did not survive the round trip: %+v", msgs)
	}
}

// A full back-and-forth conversation, including catch-up after one side
// was effectively offline for several messages.
func TestConversationAndCatchUpOverRealNetwork(t *testing.T) {
	bob := newNode(t)
	rob := newNode(t)
	join(t, bob, rob)

	if _, err := bob.client.PublishProfile(""); err != nil {
		t.Fatal(err)
	}
	if _, err := rob.client.PublishProfile(""); err != nil {
		t.Fatal(err)
	}

	bobSide := &mailbox.Conversation{PeerID: rob.id.Username()}
	if _, err := bob.client.Send(bobSide, "first"); err != nil {
		t.Fatal(err)
	}

	knocks, err := rob.client.ReadKnocks()
	if err != nil {
		t.Fatal(err)
	}
	if len(knocks) == 0 {
		t.Fatal("no knock arrived")
	}
	robSide, _ := mailbox.ConversationFromKnock(knocks[0])
	robSide.Accepted = true

	// Bob keeps talking while Rob isn't reading.
	for _, text := range []string{"second", "third", "fourth"} {
		if _, err := bob.client.Send(bobSide, text); err != nil {
			t.Fatal(err)
		}
	}

	// Rob comes back and catches up on everything, in order.
	msgs, err := rob.client.Receive(robSide)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"first", "second", "third", "fourth"}
	if len(msgs) != len(want) {
		t.Fatalf("expected %d messages after catch-up, got %d", len(want), len(msgs))
	}
	for i, w := range want {
		if msgs[i].Text != w {
			t.Fatalf("message %d: got %q want %q", i, msgs[i].Text, w)
		}
	}

	// Rob replies; Bob receives it with no "accepted" signal involved.
	if _, err := rob.client.Send(robSide, "hey bob"); err != nil {
		t.Fatal(err)
	}
	replies, err := bob.client.Receive(bobSide)
	if err != nil {
		t.Fatal(err)
	}
	if len(replies) != 1 || replies[0].Text != "hey bob" {
		t.Fatalf("bob did not receive the reply: %+v", replies)
	}
}

// The privacy property, tested against the real network: a third node
// that never received the username cannot discover the conversation,
// even though it is a full participant in the DHT and may well be
// storing the very chunks involved.
func TestThirdNodeLearnsNothingOverRealNetwork(t *testing.T) {
	bob := newNode(t)
	rob := newNode(t)
	eve := newNode(t)
	join(t, bob, rob, eve)

	for _, n := range []*node{bob, rob, eve} {
		if _, err := n.client.PublishProfile(""); err != nil {
			t.Fatal(err)
		}
	}

	conv := &mailbox.Conversation{PeerID: rob.id.Username()}
	if _, err := bob.client.Send(conv, "something private"); err != nil {
		t.Fatal(err)
	}

	// Eve's doorbell is empty: nobody knocked on hers.
	knocks, err := eve.client.ReadKnocks()
	if err != nil {
		t.Fatal(err)
	}
	if len(knocks) != 0 {
		t.Fatalf("eve should have no knocks, got %d", len(knocks))
	}

	// Even handed both usernames, Eve can locate the slot but cannot
	// decrypt what is in it -- the message is sealed to Rob's box key.
	snoop := &mailbox.Conversation{PeerID: bob.id.Username(), Accepted: true}
	stolen, err := eve.client.Receive(snoop)
	if err == nil && len(stolen) > 0 {
		t.Fatal("eve must not be able to read someone else's conversation")
	}
}
