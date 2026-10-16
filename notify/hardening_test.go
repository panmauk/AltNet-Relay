package notify

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"altnet/core/crypto"
	"altnet/core/dht"
	"altnet/core/peer"

	"altnet-relay/mailbox"
)

// bareHub is a Hub with no network, for testing its bookkeeping.
func bareHub() *Hub {
	return &Hub{
		watchers: make(map[dht.NodeID]map[string]registration),
		perPeer:  make(map[string]int),
		watching: make(map[dht.NodeID]bool),
		stop:     make(chan struct{}),
	}
}

func hexKey(i int) string {
	var id dht.NodeID
	copy(id[:], fmt.Sprintf("%032d", i))
	return id.Hex()
}

func peerName(i int) string { return fmt.Sprintf("peer-%d", i) }

// Identities are free, so the per-peer cap alone bounds nothing. The
// total must hold no matter how many peers ask.
func TestHubTotalRegistrationCap(t *testing.T) {
	h := bareHub()
	now := time.Now()
	key := 0
	for p := 0; p < 2*maxTotalRegistrations/MaxKeysPerWatchMessage; p++ {
		keys := make([]string, 0, MaxKeysPerWatchMessage)
		for i := 0; i < MaxKeysPerWatchMessage; i++ {
			keys = append(keys, hexKey(key))
			key++
		}
		h.register(peerName(p), "addr", keys, now)
	}
	if h.total != maxTotalRegistrations {
		t.Fatalf("expected the total capped at %d, got %d", maxTotalRegistrations, h.total)
	}
	if n := h.WatchedKeyCount(); n != maxTotalRegistrations {
		t.Fatalf("expected %d keys held, got %d", maxTotalRegistrations, n)
	}

	// A refresh of an existing registration is still accepted...
	later := now.Add(time.Minute)
	h.register(peerName(0), "addr2", []string{hexKey(0)}, later)
	k0, _ := dht.IDFromHex(hexKey(0))
	if reg := h.watchers[k0][peerName(0)]; reg.addr != "addr2" || !reg.expires.Equal(later.Add(WatchTTL)) {
		t.Fatal("a refresh at the total cap was refused")
	}
	// ...and expiry releases capacity.
	h.notifyTargets(k0, "", later.Add(2*WatchTTL))
	if h.total != maxTotalRegistrations-1 {
		t.Fatalf("expiry did not release capacity: total %d", h.total)
	}
}

// One key can't collect unbounded watchers, so one write to it can't
// make us send unbounded notifications -- and the first watcher keeps
// its place however many come after.
func TestHubPerKeyCapBoundsFanOut(t *testing.T) {
	h := bareHub()
	now := time.Now()
	key := hexKey(1)
	for p := 0; p < 10*maxWatchersPerKey; p++ {
		h.register(peerName(p), "addr-"+peerName(p), []string{key}, now)
	}
	k, _ := dht.IDFromHex(key)
	targets, _, _ := h.notifyTargets(k, "writer", now)
	if len(targets) != maxWatchersPerKey {
		t.Fatalf("expected fan-out capped at %d, got %d", maxWatchersPerKey, len(targets))
	}
	if h.total != maxWatchersPerKey {
		t.Fatalf("expected %d registrations, got %d", maxWatchersPerKey, h.total)
	}
	h.register(peerName(0), "moved", []string{key}, now.Add(time.Minute))
	if h.watchers[k][peerName(0)].addr != "moved" {
		t.Fatal("the first watcher lost its place to later ones")
	}
	// The writer is never told about its own write.
	targets, _, _ = h.notifyTargets(k, peerName(0), now)
	for _, a := range targets {
		if a == "moved" {
			t.Fatal("the writer was notified of its own write")
		}
	}
}

// A refused registration used to leave an empty set behind, so a peer
// at its cap could still grow our memory by one map per key it named.
func TestHubRefusedKeysLeaveNothingBehind(t *testing.T) {
	h := bareHub()
	now := time.Now()
	key := 0
	for len(h.watchers) < MaxWatchedKeysPerPeer {
		h.register("greedy", "addr", []string{hexKey(key)}, now)
		key++
	}
	before := h.WatchedKeyCount()
	for i := 0; i < 20; i++ {
		keys := make([]string, 0, MaxKeysPerWatchMessage)
		for j := 0; j < MaxKeysPerWatchMessage; j++ {
			keys = append(keys, hexKey(key))
			key++
		}
		h.register("greedy", "addr", keys, now)
	}
	if after := h.WatchedKeyCount(); after != before {
		t.Fatalf("refused keys left %d empty entries behind", after-before)
	}
}

func testNote(t *testing.T, chunks map[string][]byte) *mailbox.Note {
	t.Helper()
	id, err := crypto.NewIdentity()
	if err != nil {
		t.Fatal(err)
	}
	var keys []string
	for k := range chunks {
		keys = append(keys, k)
	}
	n, err := mailbox.NewNote(id, strings.Repeat("0", 64), keys, "", 1)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func encodeDelivery(t *testing.T, n *mailbox.Note, chunks map[string]string) string {
	t.Helper()
	body, err := json.Marshal(deliverPayload{Note: n, Chunks: chunks})
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}

func TestDecodeDeliveryBounds(t *testing.T) {
	data := []byte("sealed bytes")
	key := mailbox.ContentAddress(data).Hex()
	n := testNote(t, map[string][]byte{key: data})
	good := map[string]string{key: base64.StdEncoding.EncodeToString(data)}

	if chunks, note, ok := decodeDelivery(encodeDelivery(t, n, good)); !ok || note == nil || string(chunks[key]) != string(data) {
		t.Fatal("an honest delivery was refused")
	}

	cases := map[string]string{
		"oversized payload": strings.Repeat(" ", maxDeliverPayload+1),
		"unreferenced chunk": encodeDelivery(t, n, map[string]string{
			hexKey(7): base64.StdEncoding.EncodeToString(data),
		}),
		"more chunks than the note": encodeDelivery(t, n, map[string]string{
			key: good[key], hexKey(7): good[key],
		}),
		"chunk longer than any honest one": encodeDelivery(t, n, map[string]string{
			key: strings.Repeat("A", maxChunkB64+4),
		}),
		"bad base64": encodeDelivery(t, n, map[string]string{key: "!!!"}),
		"no chunks":  encodeDelivery(t, n, nil),
		"no note":    encodeDelivery(t, nil, good),
	}
	for name, payload := range cases {
		if _, _, ok := decodeDelivery(payload); ok {
			t.Errorf("%s: accepted", name)
		}
	}
}

// A push from an unknown sender used to buy a full doorbell read inline,
// once per push, and a known sender's pushes each parked a goroutine.
// Now the first becomes a coalescing wake, and the second is bounded.
func TestDirectDeliveryWorkIsBounded(t *testing.T) {
	store := &countingStore{Store: mailbox.NewMemStore()}
	id, _ := crypto.NewIdentity()
	box, _ := mailbox.NewBoxKey()
	client := &mailbox.Client{Store: store, ID: id, Box: box}
	in := NewInbox(client, bareHub())

	for i := 0; i < 10*wakeBuffer; i++ {
		stranger, _ := crypto.NewIdentity()
		in.handleDirect(stranger.ID(), nil, nil)
	}
	if got := store.getAlls.Load(); got != 0 {
		t.Fatalf("unknown senders' pushes read the doorbell %d times inline", got)
	}
	if len(in.wake) == 0 {
		t.Fatal("an unknown sender's push should still wake the doorbell check")
	}
	for len(in.wake) > 0 {
		if k := <-in.wake; k != in.doorbellKey() {
			t.Fatal("an unknown sender's push queued something other than the doorbell")
		}
	}

	friend, _ := crypto.NewIdentity()
	conv := &mailbox.Conversation{PeerID: friend.ID(), Accepted: true}
	in.convs[mailbox.MailboxKey(friend.ID(), id.ID())] = conv

	in.pollMu.Lock() // park every delivery at the lock
	for i := 0; i < 10*maxDirectInFlight; i++ {
		in.handleDirect(friend.ID(), nil, nil)
	}
	if n := len(in.direct); n != maxDirectInFlight {
		in.pollMu.Unlock()
		t.Fatalf("expected %d deliveries in flight, got %d", maxDirectInFlight, n)
	}
	in.pollMu.Unlock()
	if !waitFor(t, 5*time.Second, func() bool { return len(in.direct) == 0 }) {
		t.Fatal("in-flight deliveries never drained")
	}
}

// countingStore counts slot reads.
type countingStore struct {
	mailbox.Store
	getAlls atomic.Int64
}

func (s *countingStore) GetAll(key dht.NodeID) ([][]byte, error) {
	s.getAlls.Add(1)
	return s.Store.GetAll(key)
}

// One sender leaving many cards must cost one Receive per doorbell read,
// not one per card.
func TestDoorbellPollChecksEachConversationOnce(t *testing.T) {
	mem := mailbox.NewMemStore()
	store := &countingStore{Store: mem}
	id, _ := crypto.NewIdentity()
	box, _ := mailbox.NewBoxKey()
	client := &mailbox.Client{Store: store, ID: id, Box: box}
	in := NewInbox(client, bareHub())

	knocker, _ := crypto.NewIdentity()
	kbox, _ := mailbox.NewBoxKey()
	for i := 0; i < 30; i++ {
		k, err := mailbox.NewKnock(knocker, kbox, id.ID(), hexKey(i), "")
		if err != nil {
			t.Fatal(err)
		}
		blob, _ := json.Marshal(k)
		_ = mem.Put(mailbox.DoorbellKey(id.ID()), blob)
	}
	conv := &mailbox.Conversation{PeerID: knocker.ID()}
	in.convs[mailbox.MailboxKey(knocker.ID(), id.ID())] = conv // already followed

	store.getAlls.Store(0)
	in.pollMu.Lock()
	in.pollDoorbellLocked()
	in.pollMu.Unlock()
	// One read of the doorbell, one of the conversation's slot.
	if got := store.getAlls.Load(); got != 2 {
		t.Fatalf("expected 2 slot reads, got %d", got)
	}
}

// HandleMessage must survive anything, and hand on only what decodes.
func TestCourierIgnoresJunk(t *testing.T) {
	c := &Courier{}
	fired := 0
	c.SetOnDirect(func(string, *mailbox.Note, map[string][]byte) { fired++ })
	for _, payload := range []string{"", "null", "{}", `{"note":null}`, `{"note":{},"chunks":{"a":"b"}}`} {
		c.HandleMessage(nil, "", peer.Message{Type: MsgDeliver, Payload: payload})
	}
	if fired != 0 {
		t.Fatalf("junk reached the callback %d times", fired)
	}
}
