package notify

import (
	"sync"
	"testing"
	"time"

	"altnet/core/crypto"
	"altnet/core/dht"
	"altnet/core/peer"

	"altnet-relay/dhtstore"
	"altnet-relay/mailbox"
)

type node struct {
	peer   *peer.Peer
	dht    *dht.DHT
	client *mailbox.Client
	hub    *Hub
	id     *mailbox.Identity
}

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
	hub := NewHub(p, d)
	t.Cleanup(hub.Stop)

	return &node{
		peer:   p,
		dht:    d,
		client: mailbox.NewClient(dhtstore.New(d), relayID, nil),
		hub:    hub,
		id:     relayID,
	}
}

func join(t *testing.T, nodes ...*node) {
	t.Helper()
	seed := nodes[0]
	for _, n := range nodes[1:] {
		if err := n.dht.Bootstrap(seed.peer.LocalAddr()); err != nil {
			t.Fatalf("bootstrap: %v", err)
		}
	}
	for _, n := range nodes {
		n.dht.Lookup(n.dht.Self())
	}
	time.Sleep(200 * time.Millisecond)
}

// waitFor polls a condition until it holds or the deadline passes. Used
// only to bound test time -- the code under test is push-driven.
func waitFor(t *testing.T, d time.Duration, cond func() bool) bool {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return cond()
}

// The headline behaviour: Rob is told about a first-contact message
// without ever asking. No polling loop, no manual check.
func TestFirstMessageArrivesWithoutAsking(t *testing.T) {
	bob := newNode(t)
	rob := newNode(t)
	carrier := newNode(t) // uninvolved node that actually holds the keys
	join(t, bob, rob, carrier)

	if _, err := bob.client.PublishProfile(""); err != nil {
		t.Fatal(err)
	}
	if _, err := rob.client.PublishProfile(""); err != nil {
		t.Fatal(err)
	}

	var mu sync.Mutex
	var got []*mailbox.Message
	var knocked bool

	inbox := NewInbox(rob.client, rob.hub)
	inbox.OnMessages = func(_ *mailbox.Conversation, msgs []*mailbox.Message) {
		mu.Lock()
		got = append(got, msgs...)
		mu.Unlock()
	}
	inbox.OnKnock = func(*mailbox.Conversation, *mailbox.Knock) {
		mu.Lock()
		knocked = true
		mu.Unlock()
	}
	inbox.Start()
	defer inbox.Stop()

	// Bob sends. Rob never calls Receive or ReadKnocks himself.
	const text = "rob - it rang, didn't it?"
	conv := &mailbox.Conversation{PeerID: rob.id.Username()}
	if _, err := bob.client.Send(conv, text); err != nil {
		t.Fatal(err)
	}

	ok := waitFor(t, 5*time.Second, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(got) > 0
	})
	if !ok {
		t.Fatal("message never arrived: notification did not fire")
	}

	mu.Lock()
	defer mu.Unlock()
	if !knocked {
		t.Error("expected a knock callback for a first-time sender")
	}
	if got[0].Text != text {
		t.Fatalf("got %q, want %q", got[0].Text, text)
	}
}

// Once a conversation is established, subsequent messages also arrive
// push-style, and in order.
//
// Deliberately TWO nodes, which is the case that exposed a real bug: the
// value lands on the recipient's own node, and since dht.Store never
// sends itself a dht_store message, no remote party exists to notify
// them. Only Hub.observeStore's local fire makes this work. With three
// or more nodes a third party covers for it and the bug hides, so keep
// this test at two.
func TestOngoingMessagesArrivePushed(t *testing.T) {
	bob := newNode(t)
	rob := newNode(t)
	join(t, bob, rob)

	if _, err := bob.client.PublishProfile(""); err != nil {
		t.Fatal(err)
	}
	if _, err := rob.client.PublishProfile(""); err != nil {
		t.Fatal(err)
	}

	var mu sync.Mutex
	var got []string

	inbox := NewInbox(rob.client, rob.hub)
	inbox.OnMessages = func(conv *mailbox.Conversation, msgs []*mailbox.Message) {
		mu.Lock()
		conv.Accepted = true // accept on first contact, for this test
		for _, m := range msgs {
			got = append(got, m.Text)
		}
		mu.Unlock()
	}
	inbox.Start()
	defer inbox.Stop()

	conv := &mailbox.Conversation{PeerID: rob.id.Username()}
	want := []string{"one", "two", "three"}
	for _, text := range want {
		if _, err := bob.client.Send(conv, text); err != nil {
			t.Fatal(err)
		}
		// Give each notification a moment so ordering is observable;
		// without this the sends race and arrive batched, which is
		// correct but makes the assertion about order meaningless.
		time.Sleep(300 * time.Millisecond)
	}

	ok := waitFor(t, 5*time.Second, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(got) == len(want)
	})
	mu.Lock()
	defer mu.Unlock()
	if !ok {
		t.Fatalf("expected %d pushed messages, got %d (%v)", len(want), len(got), got)
	}
	for i, w := range want {
		if got[i] != w {
			t.Fatalf("message %d: got %q want %q", i, got[i], w)
		}
	}
}

// A client that was offline when messages were sent reconciles on
// startup: notifications only fire while connected, so PollAll is the
// designed catch-up path, not a fallback to polling.
func TestOfflineClientReconcilesOnStartup(t *testing.T) {
	bob := newNode(t)
	rob := newNode(t)
	join(t, bob, rob)

	if _, err := bob.client.PublishProfile(""); err != nil {
		t.Fatal(err)
	}
	if _, err := rob.client.PublishProfile(""); err != nil {
		t.Fatal(err)
	}

	// Bob sends while Rob has no inbox running at all.
	conv := &mailbox.Conversation{PeerID: rob.id.Username()}
	for _, text := range []string{"while you were out", "and again"} {
		if _, err := bob.client.Send(conv, text); err != nil {
			t.Fatal(err)
		}
	}

	var mu sync.Mutex
	var got []string

	inbox := NewInbox(rob.client, rob.hub)
	inbox.OnMessages = func(c *mailbox.Conversation, msgs []*mailbox.Message) {
		mu.Lock()
		c.Accepted = true
		for _, m := range msgs {
			got = append(got, m.Text)
		}
		mu.Unlock()
	}
	inbox.Start()
	defer inbox.Stop()

	// Rob comes online and reconciles.
	inbox.PollAll()

	ok := waitFor(t, 5*time.Second, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(got) >= 2
	})
	mu.Lock()
	defer mu.Unlock()
	if !ok {
		t.Fatalf("expected the backlog on startup, got %v", got)
	}
}

// A hub must ignore notifications for keys it never asked about, so a
// peer can't use the mechanism to make it do arbitrary work.
func TestUnrequestedNotificationIgnored(t *testing.T) {
	a := newNode(t)
	b := newNode(t)
	join(t, a, b)

	fired := make(chan dht.NodeID, 1)
	b.hub.SetOnNotify(func(k dht.NodeID) { fired <- k })

	// A notifies B about a key B never watched.
	var bogus dht.NodeID
	bogus[0] = 0xAB
	body := `{"key":"` + bogus.Hex() + `"}`
	if err := a.peer.Send(b.peer.LocalAddr(), peer.Message{
		Type: MsgNotify, Payload: body,
	}); err != nil {
		// Not connected in this direction is fine; connect and retry.
		if err := a.peer.EnsureConnected(b.peer.LocalAddr()); err != nil {
			t.Skipf("could not connect for this check: %v", err)
		}
		if err := a.peer.Send(b.peer.LocalAddr(), peer.Message{
			Type: MsgNotify, Payload: body,
		}); err != nil {
			t.Fatal(err)
		}
	}

	select {
	case k := <-fired:
		t.Fatalf("callback fired for an unrequested key %s", k)
	case <-time.After(500 * time.Millisecond):
		// Correct: ignored.
	}
}

// Watching registers interest on the nodes responsible for a key.
func TestWatchRegistersOnResponsibleNodes(t *testing.T) {
	a := newNode(t)
	b := newNode(t)
	join(t, a, b)

	var key dht.NodeID
	key[0] = 0x42

	if acked := a.hub.Watch(key); acked == 0 {
		t.Fatal("expected at least one node to acknowledge the watch")
	}
	if b.hub.WatchedKeyCount() == 0 {
		t.Fatal("the responsible node should be holding a registration")
	}
}
