package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"altnet/core/crypto"

	"altnet-relay/mailbox"
)

// ─── fakes ───

type fakeVerifier struct{ accounts map[string]Principal }

func (f *fakeVerifier) Verify(_ context.Context, token string) (Principal, error) {
	switch token {
	case "suspended":
		return Principal{}, ErrSuspended
	case "down":
		return Principal{}, errors.New("backend unreachable")
	}
	if p, ok := f.accounts[token]; ok {
		return p, nil
	}
	return Principal{}, ErrUnauthorized
}

type fakeBanner struct {
	mu     sync.Mutex
	banned map[string]string
	fail   error
}

func (b *fakeBanner) Ban(u, reason string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.fail != nil {
		return b.fail
	}
	b.banned[u] = reason
	return nil
}
func (b *fakeBanner) Unban(u string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.banned, u)
	return nil
}
func (b *fakeBanner) has(u string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	_, ok := b.banned[u]
	return ok
}

type rig struct {
	t         *testing.T
	srv       *Server
	web       *httptest.Server
	banner    *fakeBanner
	authority *crypto.Identity
	lastBody  []byte // raw bytes of the last response
}

func newRig(t *testing.T) *rig {
	t.Helper()
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	authority, err := crypto.NewIdentity()
	if err != nil {
		t.Fatal(err)
	}
	v := &fakeVerifier{accounts: map[string]Principal{
		"alice": {Email: "alice@example.com"},
		"bob":   {Email: "bob@example.com"},
		"carol": {Email: "carol@example.com"},
		"admin": {Email: "admin@example.com", Admin: true},
	}}
	banner := &fakeBanner{banned: map[string]string{}}
	srv := New(store, v, authority, banner, nil)
	// Most tests are about what a report does, not about how old its sender
	// is; the ones about age set it back.
	srv.MinReporterAge = 0
	web := httptest.NewServer(srv.Routes())
	t.Cleanup(web.Close)
	return &rig{t: t, srv: srv, web: web, banner: banner, authority: authority}
}

func (r *rig) do(method, path, token string, body any) (int, map[string]any) {
	r.t.Helper()
	var rd *bytes.Reader
	if body != nil {
		blob, _ := json.Marshal(body)
		rd = bytes.NewReader(blob)
	} else {
		rd = bytes.NewReader(nil)
	}
	req, _ := http.NewRequest(method, r.web.URL+path, rd)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		r.t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	r.lastBody = raw
	out := map[string]any{}
	_ = json.Unmarshal(raw, &out)
	return resp.StatusCode, out
}

func newIdentity(t *testing.T) *mailbox.Identity {
	t.Helper()
	id, err := mailbox.LoadOrCreateIdentity(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// signedName is how a client must read a server-signed name: as raw bytes
// or a typed struct. Decoding into map[string]any turns the 19-digit
// version number into a float64 that has lost digits, which breaks the
// signature -- an easy mistake, so the tests model doing it right.
type signedName struct {
	Handle string          `json:"handle"`
	Record json.RawMessage `json:"record"`
}

func (r *rig) claimName(token, handle string) (int, signedName) {
	r.t.Helper()
	code, _ := r.do("POST", "/v1/handle", token, map[string]any{"handle": handle})
	var sn signedName
	if code == 200 {
		if err := json.Unmarshal(r.lastBody, &sn); err != nil {
			r.t.Fatal(err)
		}
	}
	return code, sn
}

func (r *rig) bind(token string, id *mailbox.Identity) (int, map[string]any) {
	ts := time.Now().Unix()
	return r.do("POST", "/v1/identity", token, map[string]any{
		"username": id.Username(),
		"pubkey":   crypto.PublicKeyToHex(id.Signing.PublicKey),
		"ts":       ts,
		"sig":      id.Signing.Sign(BindMessage(id.Username(), ts)),
	})
}

// ─── tests ───

func TestUnauthenticatedRequestsRefused(t *testing.T) {
	r := newRig(t)
	if code, _ := r.do("GET", "/v1/me", "", nil); code != 401 {
		t.Fatalf("no token: got %d", code)
	}
	if code, _ := r.do("GET", "/v1/me", "nonsense", nil); code != 401 {
		t.Fatalf("bad token: got %d", code)
	}
	if code, _ := r.do("GET", "/v1/me", "suspended", nil); code != 403 {
		t.Fatalf("suspended account: got %d", code)
	}
	// If the accounts backend is down we say so plainly, instead of
	// telling people their password is wrong.
	if code, _ := r.do("GET", "/v1/me", "down", nil); code != 503 {
		t.Fatalf("backend down: got %d", code)
	}
}

func TestBindRequiresProofOfTheKey(t *testing.T) {
	r := newRig(t)
	id := newIdentity(t)
	other := newIdentity(t)

	if code, _ := r.bind("alice", id); code != 200 {
		t.Fatalf("honest bind failed: %d", code)
	}

	// Someone else's username with a signature from the wrong key.
	ts := time.Now().Unix()
	code, _ := r.do("POST", "/v1/identity", "bob", map[string]any{
		"username": id.Username(),
		"pubkey":   crypto.PublicKeyToHex(other.Signing.PublicKey),
		"ts":       ts,
		"sig":      other.Signing.Sign(BindMessage(id.Username(), ts)),
	})
	if code != 400 {
		t.Fatalf("a key that doesn't match the username must be refused, got %d", code)
	}

	// The right key, but a signature over a different message.
	code, _ = r.do("POST", "/v1/identity", "bob", map[string]any{
		"username": other.Username(),
		"pubkey":   crypto.PublicKeyToHex(other.Signing.PublicKey),
		"ts":       ts,
		"sig":      other.Signing.Sign([]byte("something else")),
	})
	if code != 400 {
		t.Fatalf("a signature over the wrong message must be refused, got %d", code)
	}

	// A stale timestamp can't be replayed.
	old := time.Now().Add(-time.Hour).Unix()
	code, _ = r.do("POST", "/v1/identity", "bob", map[string]any{
		"username": other.Username(),
		"pubkey":   crypto.PublicKeyToHex(other.Signing.PublicKey),
		"ts":       old,
		"sig":      other.Signing.Sign(BindMessage(other.Username(), old)),
	})
	if code != 400 {
		t.Fatalf("a stale proof must be refused, got %d", code)
	}
}

func TestIdentityBelongsToOneAccount(t *testing.T) {
	r := newRig(t)
	id := newIdentity(t)
	if code, _ := r.bind("alice", id); code != 200 {
		t.Fatal("first bind failed")
	}
	// Alice can re-bind the same identity (idempotent)...
	if code, _ := r.bind("alice", id); code != 200 {
		t.Fatalf("re-binding your own identity should work: %d", code)
	}
	// ...but Bob can't take it, even with the key (a copied key would
	// otherwise let a banned person launder themselves onto a new account).
	if code, _ := r.bind("bob", id); code != 409 {
		t.Fatalf("an identity already on another account must be refused, got %d", code)
	}
}

// The important one: a name signed here must resolve through the same
// client code real users run, and only when clients trust this server.
func TestSignedNameResolvesThroughRealClientCode(t *testing.T) {
	r := newRig(t)
	id := newIdentity(t)
	r.bind("alice", id)

	code, sn := r.claimName("alice", "@Alice_W")
	if code != 200 {
		t.Fatalf("claim failed: %d", code)
	}
	if sn.Handle != "alice_w" {
		t.Fatalf("handles are stored canonical (lowercase, no @): %v", sn.Handle)
	}

	store := mailbox.NewMemStore()
	if err := store.Put(mailbox.HandleKey("alice_w"), sn.Record); err != nil {
		t.Fatal(err)
	}

	// Without trusting the server's key, an authority-signed name by
	// someone who isn't the owner must NOT resolve in open mode.
	if _, err := mailbox.ResolveHandle(store, "alice_w"); err == nil {
		t.Fatal("open mode should reject a record not signed by its owner")
	}

	mailbox.SetTrustedHandleAuthorities([]string{crypto.PublicKeyToHex(r.authority.PublicKey)})
	defer mailbox.SetTrustedHandleAuthorities(nil)

	rec, err := mailbox.ResolveHandle(store, "alice_w")
	if err != nil {
		t.Fatalf("a name the server signed should resolve: %v", err)
	}
	if rec.Username != id.Username() {
		t.Fatal("the name must point at the identity bound to the account")
	}

	// And a self-signed claim, which anyone could forge, no longer works
	// once clients trust the authority.
	forged := &mailbox.HandleRecord{Handle: "alice_w", Username: newIdentity(t).Username(),
		Version: 1 << 62, Timestamp: time.Now().Unix(), TTL: 1000}
	_ = forged.Sign(newIdentity(t).Signing)
	fb, _ := forged.Marshal()
	store2 := mailbox.NewMemStore()
	_ = store2.Put(mailbox.HandleKey("alice_w"), fb)
	if _, err := mailbox.ResolveHandle(store2, "alice_w"); err == nil {
		t.Fatal("a self-signed name must not resolve once an authority is required")
	}
}

func TestNamesAreUniqueAndOnePerAccount(t *testing.T) {
	r := newRig(t)
	r.bind("alice", newIdentity(t))
	r.bind("bob", newIdentity(t))
	r.bind("carol", newIdentity(t))

	if code, _ := r.do("POST", "/v1/handle", "alice", map[string]any{"handle": "kate"}); code != 200 {
		t.Fatalf("first claim: %d", code)
	}
	if code, out := r.do("POST", "/v1/handle", "bob", map[string]any{"handle": "KATE"}); code != 409 {
		t.Fatalf("a taken name (any case) must be refused: %d %v", code, out)
	}
	// Alice keeps hers and can't swap.
	if code, _ := r.do("POST", "/v1/handle", "alice", map[string]any{"handle": "other"}); code != 409 {
		t.Fatalf("a second name on one account must be refused: %d", code)
	}
	// Asking for her own again is fine (that's a refresh).
	if code, _ := r.do("POST", "/v1/handle", "alice", map[string]any{"handle": "kate"}); code != 200 {
		t.Fatalf("re-claiming your own name should work: %d", code)
	}
	// The Kelvin-sign trick from the security review: "Kate" must not
	// alias "kate".
	if code, _ := r.do("POST", "/v1/handle", "carol", map[string]any{"handle": "Kate"}); code != 400 {
		t.Fatalf("a look-alike name must be rejected as invalid: %d", code)
	}
}

func TestReservedAndInvalidNamesRefused(t *testing.T) {
	r := newRig(t)
	r.bind("alice", newIdentity(t))
	for _, h := range []string{"admin", "PANMOX", "support", "ab", "1abc", "has space", "dot.dot"} {
		if code, _ := r.do("POST", "/v1/handle", "alice", map[string]any{"handle": h}); code != 400 && code != 409 {
			t.Errorf("%q should be refused, got %d", h, code)
		}
	}
}

func TestClaimNeedsAnIdentityFirst(t *testing.T) {
	r := newRig(t)
	if code, _ := r.do("POST", "/v1/handle", "alice", map[string]any{"handle": "kate"}); code != 409 {
		t.Fatalf("claiming with no identity should be refused: %d", code)
	}
}

func TestRefreshResignsWithHigherVersion(t *testing.T) {
	r := newRig(t)
	r.bind("alice", newIdentity(t))
	_, first := r.claimName("alice", "kate")
	code, _ := r.do("POST", "/v1/handle/refresh", "alice", nil)
	if code != 200 {
		t.Fatalf("refresh: %d", code)
	}
	var second signedName
	_ = json.Unmarshal(r.lastBody, &second)
	version := func(sn signedName) int64 {
		var rec mailbox.HandleRecord
		if err := json.Unmarshal(sn.Record, &rec); err != nil {
			t.Fatal(err)
		}
		return rec.Version
	}
	if version(second) <= version(first) {
		t.Fatal("a refreshed record must outrank the one it replaces")
	}
	if code, _ := r.do("POST", "/v1/handle/refresh", "bob", nil); code != 404 {
		t.Fatalf("refresh with no name should be 404, got %d", code)
	}
}

func TestReportsValidatedRateLimitedAndDeduped(t *testing.T) {
	r := newRig(t)
	self := newIdentity(t)
	r.bind("alice", self)
	target := strings.Repeat("a", 64)

	for name, body := range map[string]map[string]any{
		"short username": {"username": "abc", "reason": "x"},
		"no reason":      {"username": target, "reason": "  "},
		"self":           {"username": self.Username(), "reason": "x"},
		"huge reason":    {"username": target, "reason": strings.Repeat("x", 5000)},
	} {
		if code, _ := r.do("POST", "/v1/reports", "alice", body); code == 200 {
			t.Errorf("%s should be refused", name)
		}
	}

	code, out := r.do("POST", "/v1/reports", "alice", map[string]any{
		"username": target, "reason": "spam",
		"evidence": []map[string]any{{"from": "them", "text": "buy now", "sent": 1}},
	})
	if code != 200 || out["id"] == nil {
		t.Fatalf("a good report should be accepted: %d %v", code, out)
	}
	if !strings.Contains(out["message"].(string), "received your report") {
		t.Fatal("the reporter should get an acknowledgement of receipt")
	}
	if code, _ := r.do("POST", "/v1/reports", "alice", map[string]any{"username": target, "reason": "again"}); code != 409 {
		t.Fatalf("reporting the same person twice should be refused: %d", code)
	}

	// The daily limit stops one account flooding the queue.
	for i := 0; i < reportsPerDay; i++ {
		u := fmt.Sprintf("%064x", i+1)
		if code, _ := r.do("POST", "/v1/reports", "bob", map[string]any{"username": u, "reason": "x"}); code != 200 {
			t.Fatalf("report %d should be accepted, got %d", i+1, code)
		}
	}
	if code, _ := r.do("POST", "/v1/reports", "bob", map[string]any{"username": fmt.Sprintf("%064x", 999), "reason": "x"}); code != 429 {
		t.Fatalf("past the daily limit should be 429, got %d", code)
	}
}

func TestAdminBanEndToEnd(t *testing.T) {
	r := newRig(t)
	victimID := newIdentity(t)
	r.bind("alice", newIdentity(t))
	r.bind("bob", victimID)
	r.do("POST", "/v1/handle", "bob", map[string]any{"handle": "spammer"})

	_, rep := r.do("POST", "/v1/reports", "alice", map[string]any{"username": victimID.Username(), "reason": "harassment"})
	id := rep["id"]

	// Ordinary users can neither see nor decide reports.
	if code, _ := r.do("GET", "/v1/admin/reports", "alice", nil); code != 403 {
		t.Fatalf("reports are admin-only: %d", code)
	}
	if code, _ := r.do("POST", "/v1/admin/reports/decide", "alice", map[string]any{"id": id, "decision": "ban"}); code != 403 {
		t.Fatalf("deciding is admin-only: %d", code)
	}

	code, list := r.do("GET", "/v1/admin/reports", "admin", nil)
	if code != 200 || list["count"].(float64) != 1 {
		t.Fatalf("admin should see the pending report: %d %v", code, list)
	}

	code, out := r.do("POST", "/v1/admin/reports/decide", "admin", map[string]any{"id": id, "decision": "ban", "note": "Repeated harassment"})
	if code != 200 || out["status"] != "banned" {
		t.Fatalf("ban decision: %d %v", code, out)
	}
	if !r.banner.has(victimID.Username()) {
		t.Fatal("the ban must be published to the network")
	}

	// The banned person's account can no longer refresh, claim or bind,
	// and is told why (the DSA statement of reasons).
	code, refresh := r.do("POST", "/v1/handle/refresh", "bob", nil)
	if code != 403 || !strings.Contains(refresh["error"].(string), "Repeated harassment") {
		t.Fatalf("a banned account must be refused with the reason: %d %v", code, refresh)
	}
	if code, _ := r.bind("bob", newIdentity(t)); code != 403 {
		t.Fatalf("a banned account must not bind a fresh identity: %d", code)
	}
	_, me := r.do("GET", "/v1/me", "bob", nil)
	if me["banned"] != true || me["ban_reason"] != "Repeated harassment" {
		t.Fatalf("/v1/me should tell them they're banned and why: %v", me)
	}

	// A decided report can't be decided twice.
	if code, _ := r.do("POST", "/v1/admin/reports/decide", "admin", map[string]any{"id": id, "decision": "dismiss"}); code != 409 {
		t.Fatalf("re-deciding should be 409, got %d", code)
	}

	// Mistakes can be undone.
	if code, _ := r.do("POST", "/v1/admin/unban", "admin", map[string]any{"username": victimID.Username()}); code != 200 {
		t.Fatalf("unban: %d", code)
	}
	if r.banner.has(victimID.Username()) {
		t.Fatal("unban must lift the network ban")
	}
	if code, _ := r.do("POST", "/v1/handle/refresh", "bob", nil); code != 200 {
		t.Fatalf("after an unban the account should work again: %d", code)
	}
}

// If publishing the ban fails, the report must stay pending so an admin
// can retry -- recording a ban that never took effect would be worse.
func TestFailedBanLeavesReportPending(t *testing.T) {
	r := newRig(t)
	victim := newIdentity(t)
	r.bind("bob", victim)
	_, rep := r.do("POST", "/v1/reports", "alice", map[string]any{"username": victim.Username(), "reason": "x"})

	r.banner.fail = errors.New("network unreachable")
	if code, _ := r.do("POST", "/v1/admin/reports/decide", "admin", map[string]any{"id": rep["id"], "decision": "ban"}); code != 502 {
		t.Fatalf("a failed publish should be a 502, got %d", code)
	}
	_, list := r.do("GET", "/v1/admin/reports", "admin", nil)
	if list["count"].(float64) != 1 {
		t.Fatal("the report must still be pending after a failed ban")
	}
	if code, _ := r.do("POST", "/v1/handle/refresh", "bob", nil); code == 403 {
		t.Fatal("the account must not be marked banned when the ban didn't publish")
	}

	r.banner.fail = nil
	if code, _ := r.do("POST", "/v1/admin/reports/decide", "admin", map[string]any{"id": rep["id"], "decision": "ban"}); code != 200 {
		t.Fatalf("the retry should succeed: %d", code)
	}
}

func TestDismissDoesNotBan(t *testing.T) {
	r := newRig(t)
	victim := newIdentity(t)
	r.bind("bob", victim)
	_, rep := r.do("POST", "/v1/reports", "alice", map[string]any{"username": victim.Username(), "reason": "x"})
	if code, _ := r.do("POST", "/v1/admin/reports/decide", "admin", map[string]any{"id": rep["id"], "decision": "dismiss"}); code != 200 {
		t.Fatal("dismiss failed")
	}
	if r.banner.has(victim.Username()) {
		t.Fatal("dismissing must not ban")
	}
}

func TestStateSurvivesRestartAndFailedWritesRollBack(t *testing.T) {
	dir := t.TempDir()
	s, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Update(func(d *fileData) error {
		d.Handles["kate"] = "a@example.com"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	// A failing update changes nothing.
	_ = s.Update(func(d *fileData) error {
		d.Handles["ghost"] = "b@example.com"
		return errors.New("boom")
	})
	s.View(func(d *fileData) {
		if _, ok := d.Handles["ghost"]; ok {
			t.Fatal("a failed update leaked into memory")
		}
	})

	again, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	again.View(func(d *fileData) {
		if d.Handles["kate"] != "a@example.com" {
			t.Fatal("state lost across restart")
		}
		if _, ok := d.Handles["ghost"]; ok {
			t.Fatal("a failed update reached disk")
		}
	})
}

func TestBackendVerifier(t *testing.T) {
	calls := 0
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		calls++
		switch req.Header.Get("Authorization") {
		case "Bearer good":
			_ = json.NewEncoder(w).Encode(map[string]any{"email": "Alice@Example.com", "verified": true, "is_admin": true})
		case "Bearer unverified":
			_ = json.NewEncoder(w).Encode(map[string]any{"email": "x@example.com", "verified": false})
		case "Bearer banned":
			w.WriteHeader(http.StatusForbidden)
		case "Bearer broken":
			w.WriteHeader(http.StatusInternalServerError)
		default:
			w.WriteHeader(http.StatusUnauthorized)
		}
	}))
	defer backend.Close()
	v := &BackendVerifier{BaseURL: backend.URL}
	ctx := context.Background()

	p, err := v.Verify(ctx, "good")
	if err != nil || p.Email != "alice@example.com" || !p.Admin {
		t.Fatalf("good token: %+v %v", p, err)
	}
	if _, err := v.Verify(ctx, "good"); err != nil || calls != 1 {
		t.Fatalf("a repeat within the TTL should be answered from memory (calls=%d)", calls)
	}
	if _, err := v.Verify(ctx, "unverified"); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("an unverified account must not pass: %v", err)
	}
	if _, err := v.Verify(ctx, "banned"); !errors.Is(err, ErrSuspended) {
		t.Fatalf("403 from the backend means suspended: %v", err)
	}
	if _, err := v.Verify(ctx, "nope"); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("401 means unauthorized: %v", err)
	}
	if _, err := v.Verify(ctx, "broken"); err == nil || errors.Is(err, ErrUnauthorized) {
		t.Fatalf("a backend failure must not be reported as bad credentials: %v", err)
	}
}

func TestAdminCanBanAnAccountByEmailWithoutAReport(t *testing.T) {
	r := newRig(t)
	victim := newIdentity(t)
	r.bind("bob", victim)

	if code, _ := r.do("POST", "/v1/admin/ban", "alice", map[string]any{"email": "bob@example.com", "note": "x"}); code != 403 {
		t.Fatalf("only admins can ban: %d", code)
	}
	if code, _ := r.do("POST", "/v1/admin/ban", "admin", map[string]any{"note": "x"}); code != 400 {
		t.Fatalf("naming nobody should be a 400, got %d", code)
	}
	if code, _ := r.do("POST", "/v1/admin/ban", "admin", map[string]any{"email": "nobody@example.com"}); code != 404 {
		t.Fatalf("an account with no Relay identity has nothing to ban: %d", code)
	}

	code, out := r.do("POST", "/v1/admin/ban", "admin", map[string]any{"email": "bob@example.com", "note": "Account suspended by Studio"})
	if code != 200 {
		t.Fatalf("ban by email: %d %v", code, out)
	}
	if !r.banner.has(victim.Username()) {
		t.Fatal("the identity behind that email must be banned on the network")
	}
	_, me := r.do("GET", "/v1/me", "bob", nil)
	if me["banned"] != true || me["ban_reason"] != "Account suspended by Studio" {
		t.Fatalf("the person should be told why: %v", me)
	}
}

func TestCallRelayCredentialsOnlyForSignedInAccounts(t *testing.T) {
	r := newRig(t)
	if code, _ := r.do("GET", "/v1/turn", "alice", nil); code != 404 {
		t.Fatalf("without a call relay configured the answer should be 404, got %d", code)
	}

	secret := []byte("shared-with-coturn")
	r.srv.SetTURN(&TURNConfig{Secret: secret, URLs: []string{"turn:relay.example:3478?transport=udp"}, TTL: 30 * time.Minute})
	if code, _ := r.do("GET", "/v1/turn", "", nil); code != 401 {
		t.Fatalf("credentials must need a sign-in, got %d", code)
	}
	code, out := r.do("GET", "/v1/turn", "alice", nil)
	if code != 200 {
		t.Fatalf("turn: %d %v", code, out)
	}
	user, _ := out["username"].(string)
	cred, _ := out["credential"].(string)
	// coturn recomputes exactly this from the username and its own copy of the secret.
	parts := strings.SplitN(user, ":", 2)
	if len(parts) != 2 {
		t.Fatalf("username %q should be <expiry>:<label>", user)
	}
	expiry, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil || time.Until(time.Unix(expiry, 0)) > 31*time.Minute || time.Until(time.Unix(expiry, 0)) < 28*time.Minute {
		t.Fatalf("expiry %q should be about 30 minutes out", parts[0])
	}
	_, recomputed := TURNCredentialsFor(secret, parts[1], time.Unix(expiry, 0))
	if cred != recomputed {
		t.Fatal("credential must be the HMAC coturn expects")
	}
	if strings.Contains(user, "example.com") || strings.Contains(user, "alice") {
		t.Fatalf("the username must not leak the email: %q", user)
	}
	if urls, _ := out["urls"].([]any); len(urls) != 1 {
		t.Fatalf("urls: %v", out["urls"])
	}
}

func TestAccountStaysOnOneComputerUntilAnAdminMovesIt(t *testing.T) {
	r := newRig(t)
	first, second := newIdentity(t), newIdentity(t)
	if code, _ := r.bind("alice", first); code != 200 {
		t.Fatal("first bind failed")
	}
	// Even with no name yet, a different identity is refused, so two
	// computers on one account can't keep taking the binding from each other.
	code, out := r.bind("alice", second)
	if code != 409 || !strings.Contains(out["error"].(string), "start fresh") {
		t.Fatalf("a second computer must be refused with an explanation: %d %v", code, out)
	}

	// A name is signed for the identity the account has now.
	r.do("POST", "/v1/handle", "alice", map[string]any{"handle": "alice"})

	if code, _ := r.do("POST", "/v1/admin/unbind", "alice", map[string]any{"email": "alice@example.com"}); code != 403 {
		t.Fatalf("only admins can move an account: %d", code)
	}
	if code, _ := r.do("POST", "/v1/admin/unbind", "admin", map[string]any{"email": "nobody@example.com"}); code != 404 {
		t.Fatalf("unbinding an unknown account: %d", code)
	}
	if code, _ := r.do("POST", "/v1/admin/unbind", "admin", map[string]any{"email": "alice@example.com"}); code != 200 {
		t.Fatalf("unbind: %d", code)
	}

	if code, out := r.bind("alice", second); code != 200 {
		t.Fatalf("after the move the new computer should bind: %d %v", code, out)
	}
	// The name she already owns now points at the new computer.
	_, sn := r.claimName("alice", "alice")
	var rec mailbox.HandleRecord
	if err := json.Unmarshal(sn.Record, &rec); err != nil {
		t.Fatal(err)
	}
	if rec.Username != second.Username() {
		t.Fatalf("the name should follow her to the new identity: %s", rec.Username)
	}
	// And the old identity is free for anyone, not banned.
	if r.banner.has(first.Username()) {
		t.Fatal("moving an account is not a ban")
	}
}

func TestVulgarNamesCannotBeClaimedEvenByAModifiedApp(t *testing.T) {
	r := newRig(t)
	r.bind("alice", newIdentity(t))
	for _, name := range []string{"fuckface", "sh1thead", "big_ass", "hitler88", "kurva"} {
		code, out := r.do("POST", "/v1/handle", "alice", map[string]any{"handle": name})
		if code == 200 {
			t.Fatalf("%q must not be claimable", name)
		}
		msg, _ := out["error"].(string)
		if !strings.Contains(msg, "isn't allowed") || strings.Contains(strings.ToLower(msg), "fuck") {
			t.Fatalf("a plain refusal that doesn't repeat the word: %q", msg)
		}
	}
	// A refused name leaves the account free to choose another.
	if code, _ := r.do("POST", "/v1/handle", "alice", map[string]any{"handle": "alice-the-builder"}); code != 200 {
		t.Fatalf("a decent name still works afterwards: %d", code)
	}
}

func TestAdminNotesStayOffThePublicBanList(t *testing.T) {
	r := newRig(t)
	bob := newIdentity(t)
	r.bind("bob", bob)
	note := "private: spoke to his employer, see ticket 41"
	if c, _ := r.do("POST", "/v1/admin/ban", "admin", map[string]any{"username": bob.Username(), "note": note}); c != 200 {
		t.Fatalf("ban: %d", c)
	}
	r.banner.mu.Lock()
	public := r.banner.banned[bob.Username()]
	r.banner.mu.Unlock()
	if strings.Contains(public, "employer") || public != publicReasonOperator {
		t.Fatalf("the public ban list got %q", public)
	}
	// The person banned still sees the explanation.
	_, me := r.do("GET", "/v1/me", "bob", nil)
	if !strings.Contains(fmt.Sprint(me["ban_reason"]), "employer") {
		t.Fatalf("the banned person should see the reason: %v", me)
	}
}

func TestAtMostTenMessagesOfEvidence(t *testing.T) {
	r := newRig(t)
	bob := newIdentity(t)
	ev := make([]map[string]any, 11)
	for i := range ev {
		ev[i] = map[string]any{"from": "them", "text": "x", "sent": 1}
	}
	if c, _ := r.do("POST", "/v1/reports", "alice", map[string]any{"username": bob.Username(), "reason": "spam", "evidence": ev}); c != 400 {
		t.Fatalf("11 messages of evidence: %d", c)
	}
}
