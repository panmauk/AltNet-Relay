package dhtstore

import (
	"testing"
	"time"

	"altnet/core/crypto"
	"altnet/core/dht"
	"altnet/core/peer"

	"altnet-relay/mailbox"
)

// newNodeWithDir is newNode but with a caller-supplied Relay key
// directory, so a test can shut a node down and bring a *different*
// node up carrying the same Relay identity -- which is what "the user
// closed the app and reopened it later" actually looks like.
func newNodeWithDir(t *testing.T, dir string) *node {
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
	relayID, err := mailbox.LoadOrCreateIdentity(dir)
	if err != nil {
		t.Fatal(err)
	}
	return &node{
		peer:   p,
		dht:    d,
		client: mailbox.NewClient(New(d), relayID, nil),
		id:     relayID,
	}
}

// True store-and-forward: the sender writes a message, then goes
// offline, and the recipient was never running at all during the send.
// The message must still be waiting when the recipient first appears.
//
// This is the question "does Relay work on a tiny network?" in its
// sharpest form. The answer depends entirely on there being at least one
// always-on node -- which AltNet already has, in the seed node. Here
// "seed" plays that role and is the ONLY node online for part of the
// test.
func TestOfflineDeliveryThroughAlwaysOnNode(t *testing.T) {
	seed := newNode(t) // stands in for the always-on VPS node

	// Rob appears just long enough to publish his profile, then leaves.
	robDir := t.TempDir()
	rob := newNodeWithDir(t, robDir)
	join(t, seed, rob)
	if _, err := rob.client.PublishProfile(""); err != nil {
		t.Fatal(err)
	}
	robUsername := rob.id.Username()
	rob.peer.Stop()
	time.Sleep(100 * time.Millisecond)

	// Bob comes online, sends to a username whose owner is offline,
	// then leaves too.
	bob := newNode(t)
	join(t, seed, bob)
	if _, err := bob.client.PublishProfile(""); err != nil {
		t.Fatal(err)
	}

	const text = "sent while you were offline"
	conv := &mailbox.Conversation{PeerID: robUsername}
	if _, err := bob.client.Send(conv, text); err != nil {
		t.Fatalf("send to an offline recipient: %v", err)
	}
	bob.peer.Stop()
	time.Sleep(100 * time.Millisecond)

	// Only the always-on node is left holding anything.

	// Rob reopens the app: a brand-new node, same Relay identity.
	robAgain := newNodeWithDir(t, robDir)
	if robAgain.id.Username() != robUsername {
		t.Fatal("reopening must restore the same Relay identity")
	}
	join(t, seed, robAgain)

	knocks, err := robAgain.client.ReadKnocks()
	if err != nil {
		t.Fatal(err)
	}
	if len(knocks) != 1 {
		t.Fatalf("expected the knock to have survived, got %d", len(knocks))
	}
	robSide, err := mailbox.ConversationFromKnock(knocks[0])
	if err != nil {
		t.Fatal(err)
	}
	msgs, err := robAgain.client.Receive(robSide)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 1 || msgs[0].Text != text {
		t.Fatalf("message did not survive both parties being offline: %+v", msgs)
	}
}

// A two-node network is enough for the whole flow: K=20 is a ceiling
// ("up to the 20 closest"), not a requirement.
func TestWorksOnATwoNodeNetwork(t *testing.T) {
	bob := newNode(t)
	rob := newNode(t)
	join(t, bob, rob)

	if _, err := bob.client.PublishProfile(""); err != nil {
		t.Fatal(err)
	}
	if _, err := rob.client.PublishProfile(""); err != nil {
		t.Fatal(err)
	}

	conv := &mailbox.Conversation{PeerID: rob.id.Username()}
	const text = "two nodes is a network"
	if _, err := bob.client.Send(conv, text); err != nil {
		t.Fatal(err)
	}

	knocks, err := rob.client.ReadKnocks()
	if err != nil {
		t.Fatal(err)
	}
	if len(knocks) != 1 {
		t.Fatalf("expected 1 knock on a two-node network, got %d", len(knocks))
	}
	robSide, _ := mailbox.ConversationFromKnock(knocks[0])
	msgs, err := rob.client.Receive(robSide)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 1 || msgs[0].Text != text {
		t.Fatalf("two-node delivery failed: %+v", msgs)
	}
}
