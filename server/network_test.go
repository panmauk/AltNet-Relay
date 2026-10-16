package server

import (
	"log/slog"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"altnet/core/crypto"
	"altnet/core/dht"
	"altnet/core/peer"

	"altnet-relay/dhtstore"
	"altnet-relay/moderation"
)

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

// The whole moderation chain, for real: someone reports a user over HTTP,
// an admin bans them over HTTP, the ban is published onto an actual
// network, and a client on a DIFFERENT node -- which knows nothing about
// the server except the operator's public key -- sees the person banned.
// Then the admin lifts it and the client sees that too.
func TestReportToNetworkBanEndToEnd(t *testing.T) {
	if testing.Short() {
		t.Skip("starts real nodes")
	}
	seedP, seedD := startNode(t)
	_, serverD := startNode(t)
	_, clientD := startNode(t)
	for _, d := range []*dht.DHT{serverD, clientD} {
		if err := d.Bootstrap(seedP.LocalAddr()); err != nil {
			t.Fatal(err)
		}
	}
	for _, d := range []*dht.DHT{seedD, serverD, clientD} {
		d.Lookup(d.Self())
	}
	time.Sleep(200 * time.Millisecond)

	operator, err := crypto.NewIdentity()
	if err != nil {
		t.Fatal(err)
	}
	banner, err := NewOperatorBanner(dhtstore.New(serverD), operator, filepath.Join(t.TempDir(), "banlist.state"))
	if err != nil {
		t.Fatal(err)
	}
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	v := &fakeVerifier{accounts: map[string]Principal{
		"alice": {Email: "alice@example.com"},
		"bob":   {Email: "bob@example.com"},
		"admin": {Email: "admin@example.com", Admin: true},
	}}
	srv := New(store, v, operator, banner, slog.New(slog.DiscardHandler))
	web := httptest.NewServer(srv.Routes())
	defer web.Close()
	r := &rig{t: t, srv: srv, web: web}

	victim := newIdentity(t)
	r.bind("alice", newIdentity(t))
	r.bind("bob", victim)

	_, rep := r.do("POST", "/v1/reports", "alice", map[string]any{"username": victim.Username(), "reason": "harassment"})
	if code, out := r.do("POST", "/v1/admin/reports/decide", "admin", map[string]any{"id": rep["id"], "decision": "ban", "note": "Harassment"}); code != 200 {
		t.Fatalf("ban decision failed: %d %v", code, out)
	}

	client := moderation.NewSync(dhtstore.New(clientD), []string{crypto.PublicKeyToHex(operator.PublicKey)})
	if err := client.Refresh(); err != nil {
		t.Fatalf("client refresh: %v", err)
	}
	if !client.BannedPeer(victim.Username()) {
		t.Fatal("a client on another node should see the ban the admin published")
	}
	if client.BannedPeer(newIdentity(t).Username()) {
		t.Fatal("only the reported person should be banned")
	}

	if code, _ := r.do("POST", "/v1/admin/unban", "admin", map[string]any{"username": victim.Username()}); code != 200 {
		t.Fatalf("unban failed: %d", code)
	}
	if err := client.Refresh(); err != nil {
		t.Fatal(err)
	}
	if client.BannedPeer(victim.Username()) {
		t.Fatal("the client should see the ban lifted")
	}
}
