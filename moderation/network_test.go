package moderation

import (
	"testing"
	"time"

	"altnet-relay/dhtstore"
	"altnet/core/crypto"
	"altnet/core/dht"
	"altnet/core/peer"
)

// startNode runs one real AltNet node on a free localhost port.
func startNode(t *testing.T) (*peer.Peer, *dht.DHT) {
	t.Helper()
	key, err := crypto.NewIdentity()
	if err != nil {
		t.Fatal(err)
	}
	p := peer.New(key, "127.0.0.1:0")
	if err := p.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.Stop)
	d, err := dht.New(p)
	if err != nil {
		t.Fatal(err)
	}
	return p, d
}

// The whole path over real sockets: an operator publishes a ban from one
// node, and a client on another node sees it -- and then sees it lifted.
func TestBanOverRealNetwork(t *testing.T) {
	if testing.Short() {
		t.Skip("starts real nodes")
	}
	seedP, seedD := startNode(t)
	_, opD := startNode(t)
	_, clientD := startNode(t)
	for _, d := range []*dht.DHT{opD, clientD} {
		if err := d.Bootstrap(seedP.LocalAddr()); err != nil {
			t.Fatalf("bootstrap: %v", err)
		}
	}
	for _, d := range []*dht.DHT{seedD, opD, clientD} {
		d.Lookup(d.Self())
	}
	time.Sleep(200 * time.Millisecond)

	opKey := newKey(t)
	op := NewOperator(dhtstore.New(opD), opKey)
	victim := newKey(t)
	if err := op.Ban(victim.ID(), "spam"); err != nil {
		t.Fatalf("publish ban: %v", err)
	}

	client := NewSync(dhtstore.New(clientD), []string{op.PublicKey()})
	if err := client.Refresh(); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if !client.BannedPeer(victim.ID()) || !client.Banned(pubHex(victim)) {
		t.Fatal("the ban did not reach the other node")
	}

	if err := op.Unban(victim.ID()); err != nil {
		t.Fatalf("publish unban: %v", err)
	}
	if err := client.Refresh(); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if client.BannedPeer(victim.ID()) {
		t.Fatal("the unban did not reach the other node")
	}
}
