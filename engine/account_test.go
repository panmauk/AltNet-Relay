package engine

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"altnet/core/crypto"

	"altnet-relay/account"
	"altnet-relay/dhtstore"
	"altnet-relay/mailbox"
	"altnet-relay/server"
)

// fakeStudio stands in for the real accounts backend with the same routes,
// the same JSON, and the same strict decoding. relay-server talks to it
// exactly as it talks to the real one.
type fakeStudio struct {
	mu       sync.Mutex
	users    map[string]*fakeUser // by email
	tokens   map[string]string    // token -> email
	nextTok  int
	web      *httptest.Server
	verifyOK string
}

type fakeUser struct {
	password  string
	verified  bool
	admin     bool
	suspended bool
}

func newFakeStudio(t *testing.T) *fakeStudio {
	t.Helper()
	f := &fakeStudio{users: map[string]*fakeUser{}, tokens: map[string]string{}, verifyOK: "424242"}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/signup", f.signup)
	mux.HandleFunc("POST /v1/verify", f.verify)
	mux.HandleFunc("POST /v1/login", f.login)
	mux.HandleFunc("GET /v1/me", f.me)
	mux.HandleFunc("DELETE /v1/me", f.deleteMe)
	f.web = httptest.NewServer(mux)
	t.Cleanup(f.web.Close)
	return f
}

func fsErr(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

func fsDecode(w http.ResponseWriter, r *http.Request, dst any) bool {
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields() // the real backend does this too
	if err := dec.Decode(dst); err != nil {
		fsErr(w, 400, "invalid json: "+err.Error())
		return false
	}
	return true
}

func (f *fakeStudio) signup(w http.ResponseWriter, r *http.Request) {
	var req struct{ Email, Password string }
	if !fsDecode(w, r, &req) {
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	email := strings.ToLower(req.Email)
	if _, taken := f.users[email]; taken {
		fsErr(w, 409, "email already registered")
		return
	}
	f.users[email] = &fakeUser{password: req.Password}
	json.NewEncoder(w).Encode(map[string]bool{"ok": true})
}

func (f *fakeStudio) session(w http.ResponseWriter, email string, u *fakeUser) {
	f.nextTok++
	tok := "tok-" + email + "-" + string(rune('a'+f.nextTok))
	f.tokens[tok] = email
	json.NewEncoder(w).Encode(map[string]any{
		"token": tok, "expires_at": time.Now().Add(24 * time.Hour).Unix(), "email": email, "is_admin": u.admin,
	})
}

func (f *fakeStudio) verify(w http.ResponseWriter, r *http.Request) {
	var req struct{ Email, Code string }
	if !fsDecode(w, r, &req) {
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	u := f.users[strings.ToLower(req.Email)]
	if u == nil || req.Code != f.verifyOK {
		fsErr(w, 401, "invalid email or code")
		return
	}
	u.verified = true
	f.session(w, strings.ToLower(req.Email), u)
}

func (f *fakeStudio) login(w http.ResponseWriter, r *http.Request) {
	var req struct{ Email, Password string }
	if !fsDecode(w, r, &req) {
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	u := f.users[strings.ToLower(req.Email)]
	switch {
	case u == nil || u.password != req.Password:
		fsErr(w, 401, "invalid credentials")
	case !u.verified:
		fsErr(w, 403, "email not verified yet")
	case u.suspended:
		fsErr(w, 403, "this account has been suspended; appeal at abuse.report@panmox.org")
	default:
		f.session(w, strings.ToLower(req.Email), u)
	}
}

func (f *fakeStudio) me(w http.ResponseWriter, r *http.Request) {
	tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	f.mu.Lock()
	defer f.mu.Unlock()
	email, ok := f.tokens[tok]
	if !ok {
		fsErr(w, 401, "invalid token")
		return
	}
	u := f.users[email]
	if u.suspended {
		fsErr(w, 403, "this account has been suspended; appeal at abuse.report@panmox.org")
		return
	}
	json.NewEncoder(w).Encode(map[string]any{"email": email, "verified": u.verified, "is_admin": u.admin, "created": 0})
}

// deleteMe is the backend's own account deletion: the user and every
// session go.
func (f *fakeStudio) deleteMe(w http.ResponseWriter, r *http.Request) {
	tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	f.mu.Lock()
	defer f.mu.Unlock()
	email, ok := f.tokens[tok]
	if !ok {
		fsErr(w, 401, "invalid token")
		return
	}
	delete(f.users, email)
	for t, e := range f.tokens {
		if e == email {
			delete(f.tokens, t)
		}
	}
	json.NewEncoder(w).Encode(map[string]bool{"ok": true})
}

// makeAdmin creates a verified administrator and returns a session token.
func (f *fakeStudio) makeAdmin(email string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.users[email] = &fakeUser{password: "pw", verified: true, admin: true}
	f.tokens["admin-token"] = email
	return "admin-token"
}

func (f *fakeStudio) revoke(email string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for tok, e := range f.tokens {
		if e == email {
			delete(f.tokens, tok)
		}
	}
}

func (f *fakeStudio) suspend(email string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.users[email].suspended = true
}

// world is a small real network plus the operator's server in front of it.
type world struct {
	t        *testing.T
	seed     *Engine // plain node the others bootstrap through; also carries the server's bans
	studio   *fakeStudio
	relay    *httptest.Server
	acct     *account.Client
	operator *crypto.Identity
	opKey    string
	adminTok string
	srv      *server.Server
}

func newWorld(t *testing.T) *world {
	t.Helper()
	w := &world{t: t}
	w.operator = mustIdentity(t)
	w.opKey = crypto.PublicKeyToHex(w.operator.PublicKey)

	// Names are resolved in authority mode for the whole test process, so put
	// it back afterwards for the tests that use self-claimed names.
	t.Cleanup(func() { mailbox.SetTrustedHandleAuthorities(nil) })
	mailbox.SetTrustedHandleAuthorities([]string{w.opKey})

	w.seed = open(t, t.TempDir())
	t.Cleanup(w.seed.Close)

	w.studio = newFakeStudio(t)
	w.adminTok = w.studio.makeAdmin("admin@example.com")

	banner, err := server.NewOperatorBanner(dhtstore.New(w.seed.DHT), w.operator, filepath.Join(t.TempDir(), "banlist.state"))
	if err != nil {
		t.Fatal(err)
	}
	store, err := server.OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	srv := server.New(store,
		&server.BackendVerifier{BaseURL: w.studio.web.URL, TTL: time.Nanosecond},
		w.operator, banner, slog.New(slog.DiscardHandler))
	// Test accounts are minutes old; the age rule has its own tests in server.
	srv.MinReporterAge = 0
	w.srv = srv
	w.relay = httptest.NewServer(srv.Routes())
	t.Cleanup(w.relay.Close)
	w.acct = account.New(w.studio.web.URL, w.relay.URL)
	return w
}

func mustIdentity(t *testing.T) *crypto.Identity {
	t.Helper()
	id, err := crypto.NewIdentity()
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// engine opens a client with accounts on, joined to the world's network.
func (w *world) engine(dir string) *Engine {
	w.t.Helper()
	e, err := Open(Config{
		DataDir: dir, Listen: "127.0.0.1:0", Bootstrap: []string{w.seed.Peer.LocalAddr()},
		Operators: []string{w.opKey}, Accounts: w.acct, HandleAuthorities: []string{w.opKey},
	})
	if err != nil {
		w.t.Fatal(err)
	}
	e.Start()
	w.t.Cleanup(e.Close)
	return e
}

// signUp takes a brand-new person from nothing to signed in.
func (w *world) signUp(e *Engine, email string) {
	w.t.Helper()
	if err := e.SignUp(email, "hunter22"); err != nil {
		w.t.Fatal(err)
	}
	if err := e.VerifyEmail(email, w.studio.verifyOK); err != nil {
		w.t.Fatal(err)
	}
}

func (w *world) adminDo(method, path string, body any) (int, map[string]any) {
	w.t.Helper()
	var rd *strings.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = strings.NewReader(string(b))
	} else {
		rd = strings.NewReader("")
	}
	req, _ := http.NewRequest(method, w.relay.URL+path, rd)
	req.Header.Set("Authorization", "Bearer "+w.adminTok)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		w.t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

func TestNothingWorksUntilSignedIn(t *testing.T) {
	w := newWorld(t)
	rob := w.engine(t.TempDir())
	kate := w.engine(t.TempDir())

	if g := rob.Account().Gate; g != GateSignedOut {
		t.Fatalf("a fresh install should ask for sign-in, gate = %q", g)
	}
	if _, err := rob.Add(kate.Username(), "kate"); err == nil {
		t.Fatal("adding someone should need an account")
	}
	if _, err := rob.Send(kate.Username(), "hi"); err == nil {
		t.Fatal("sending should need an account")
	}
	if _, err := rob.Claim("rob"); err == nil {
		t.Fatal("claiming a name should need an account")
	}

	w.signUp(rob, "rob@example.com")
	if a := rob.Account(); a.Gate != GateOK || a.Email != "rob@example.com" {
		t.Fatalf("after signing up: %+v", a)
	}
	// Signing in attached this computer's identity to the account.
	me, err := w.acct.Me(nil2ctx(), rob.sess.Token)
	if err != nil {
		t.Fatal(err)
	}
	if me.Username != rob.Username() {
		t.Fatalf("server has identity %q, want %q", me.Username, rob.Username())
	}
}

func TestWrongPasswordAndUnverifiedAreRefused(t *testing.T) {
	w := newWorld(t)
	rob := w.engine(t.TempDir())
	if err := rob.SignUp("rob@example.com", "hunter22"); err != nil {
		t.Fatal(err)
	}
	if err := rob.VerifyEmail("rob@example.com", "000000"); err == nil {
		t.Fatal("a wrong code must not verify")
	}
	if err := rob.SignIn("rob@example.com", "hunter22"); err == nil {
		t.Fatal("signing in before verifying must fail")
	}
	if err := rob.VerifyEmail("rob@example.com", w.studio.verifyOK); err != nil {
		t.Fatal(err)
	}
	rob.SignOut()
	if err := rob.SignIn("rob@example.com", "wrong"); err == nil {
		t.Fatal("wrong password must not sign in")
	}
	if g := rob.Account().Gate; g != GateSignedOut {
		t.Fatalf("gate = %q after failed sign-in", g)
	}
	if err := rob.SignIn("rob@example.com", "hunter22"); err != nil {
		t.Fatal(err)
	}
}

// The whole point of routing names through the server: one owner each,
// and everyone else can find them.
func TestNamesAreUniqueAndFindable(t *testing.T) {
	w := newWorld(t)
	rob, bob, kate := w.engine(t.TempDir()), w.engine(t.TempDir()), w.engine(t.TempDir())
	w.signUp(rob, "rob@example.com")
	w.signUp(bob, "bob@example.com")
	w.signUp(kate, "kate@example.com")

	if h, err := rob.Claim("Rob"); err != nil || h != "rob" {
		t.Fatalf("claim: %q %v", h, err)
	}
	if _, err := bob.Claim("rob"); err == nil || !strings.Contains(err.Error(), "taken") {
		t.Fatalf("a second person claiming the same name should be told it's taken, got %v", err)
	}
	if _, err := rob.Claim("robert"); err == nil {
		t.Fatal("one name per account")
	}
	if _, err := bob.Claim("admin"); err == nil {
		t.Fatal("reserved names can't be claimed")
	}

	if rob.MyHandle() != "rob" {
		t.Fatalf("MyHandle = %q", rob.MyHandle())
	}
	// Someone else finds Rob by typing his name.
	id, err := kate.Add("@rob", "")
	if err != nil || id != rob.Username() {
		t.Fatalf("kate looking up @rob: %q, %v", id, err)
	}

	// A name nobody's server signed doesn't resolve, whatever gets published.
	forged := mailbox.NewClient(kate.Client.Store, kate.ID, nil)
	if _, err := forged.ClaimHandle("mallory"); err != nil {
		t.Fatal(err)
	}
	if _, err := bob.Add("mallory", ""); err == nil {
		t.Fatal("a self-signed name must not resolve when names come from the server")
	}
}

func TestSessionSurvivesRestartWithoutPassword(t *testing.T) {
	w := newWorld(t)
	dir := t.TempDir()
	rob := w.engine(dir)
	w.signUp(rob, "rob@example.com")
	if _, err := rob.Claim("rob"); err != nil {
		t.Fatal(err)
	}
	rob.Close()

	again := w.engine(dir)
	if a := again.Account(); a.Gate != GateOK || a.Email != "rob@example.com" || a.Handle != "rob" {
		t.Fatalf("after restart: %+v", a)
	}
	// And the password was never stored.
	raw, err := readFile(t, dir, accountFile)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(raw, "hunter22") || strings.Contains(strings.ToLower(raw), "password") {
		t.Fatalf("account.json holds a password: %s", raw)
	}
}

func TestExpiredSessionReturnsToSignIn(t *testing.T) {
	w := newWorld(t)
	rob := w.engine(t.TempDir())
	w.signUp(rob, "rob@example.com")

	w.studio.revoke("rob@example.com")
	if err := rob.CheckAccount(); err == nil {
		t.Fatal("expected the check to report the dead session")
	}
	if g := rob.Account().Gate; g != GateSignedOut {
		t.Fatalf("gate = %q, want signed_out", g)
	}
	if _, err := rob.Send(mustIdentity(t).ID(), "hi"); err == nil {
		t.Fatal("sending should need a fresh sign-in")
	}
	// Signing in again fixes it, with the same identity.
	if err := rob.SignIn("rob@example.com", "hunter22"); err != nil {
		t.Fatal(err)
	}
	if g := rob.Account().Gate; g != GateOK {
		t.Fatalf("gate = %q after signing back in", g)
	}
}

func TestOfflineKeepsSavedSession(t *testing.T) {
	w := newWorld(t)
	dir := t.TempDir()
	rob := w.engine(dir)
	w.signUp(rob, "rob@example.com")
	rob.Close()

	w.relay.Close() // the account service disappears
	again := w.engine(dir)
	if err := again.CheckAccount(); err == nil {
		t.Fatal("expected an error while the server is away")
	}
	a := again.Account()
	if a.Gate != GateOK || !a.Offline {
		t.Fatalf("losing the connection must not lock anyone out: %+v", a)
	}
}

// A person can't put two accounts on one identity: that would let a banned
// person walk back in by making a new email address.
func TestSecondAccountCannotUseSameIdentity(t *testing.T) {
	w := newWorld(t)
	dir := t.TempDir()
	rob := w.engine(dir)
	w.signUp(rob, "rob@example.com")
	rob.SignOut()

	err := rob.SignUp("other@example.com", "hunter22")
	if err != nil {
		t.Fatal(err)
	}
	err = rob.VerifyEmail("other@example.com", w.studio.verifyOK)
	if err == nil || !strings.Contains(err.Error(), "already belongs") {
		t.Fatalf("second account on the same identity should be refused, got %v", err)
	}
	if g := rob.Account().Gate; g != GateSignedOut {
		t.Fatalf("gate = %q after refused sign-in", g)
	}
	// The original owner can still come back.
	if err := rob.SignIn("rob@example.com", "hunter22"); err != nil {
		t.Fatal(err)
	}
}

func TestNameComesBackAfterLosingLocalState(t *testing.T) {
	w := newWorld(t)
	dir := t.TempDir()
	rob := w.engine(dir)
	w.signUp(rob, "rob@example.com")
	if _, err := rob.Claim("rob"); err != nil {
		t.Fatal(err)
	}
	rob.Close()

	// Keep the identity, lose the app's saved state (a reinstall).
	removeFile(t, dir, "state.json")
	again := w.engine(dir)
	if !waitFor(10*time.Second, func() bool { return again.MyHandle() == "rob" }) {
		t.Fatalf("name not restored; MyHandle = %q", again.MyHandle())
	}
}

func TestBanShowsReasonAndStopsSending(t *testing.T) {
	w := newWorld(t)
	rob, bob := w.engine(t.TempDir()), w.engine(t.TempDir())
	w.signUp(rob, "rob@example.com")
	w.signUp(bob, "bob@example.com")
	if _, err := rob.Claim("rob"); err != nil {
		t.Fatal(err)
	}
	bobID, err := bob.Add(rob.Username(), "rob")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := bob.Send(bobID, "you are awful"); err != nil {
		t.Fatal(err)
	}
	if !waitPolled(rob, 8*time.Second, func() bool { return threadHas(rob, bob.Username(), "you are awful") }) {
		t.Fatal("rob never saw bob's message")
	}

	// Rob reports Bob, attaching the message; the admin bans him.
	ack, err := rob.Report(bob.Username(), "abusive messages", 5)
	if err != nil || !strings.Contains(ack, "received your report") {
		t.Fatalf("report: %q %v", ack, err)
	}
	code, out := w.adminDo("GET", "/v1/admin/reports", nil)
	if code != 200 {
		t.Fatalf("list reports: %d %v", code, out)
	}
	reports := out["reports"].([]any)
	if len(reports) != 1 {
		t.Fatalf("expected 1 report, got %v", out)
	}
	first := reports[0].(map[string]any)
	ev, _ := first["evidence"].([]any)
	if len(ev) != 1 || !strings.Contains(ev[0].(map[string]any)["text"].(string), "awful") {
		t.Fatalf("evidence not attached: %v", first)
	}
	if code, out := w.adminDo("POST", "/v1/admin/reports/decide", map[string]any{
		"id": first["id"], "decision": "ban", "note": "Harassment"}); code != 200 {
		t.Fatalf("decide: %d %v", code, out)
	}

	// Bob's app finds out at its next check, with the reason.
	bob.CheckAccount()
	a := bob.Account()
	if a.Gate != GateBanned || a.Reason == "" {
		t.Fatalf("bob should be shown the ban: %+v", a)
	}
	if _, err := bob.Send(bobID, "hello?"); err == nil {
		t.Fatal("a banned account can't send")
	}
	// Everyone else's app silences him too.
	rob.RefreshBans()
	if !rob.IsBanned(bob.Username()) {
		t.Fatal("rob's app should now treat bob as banned")
	}
}

func TestReportNeedsAReasonAndAnAccount(t *testing.T) {
	w := newWorld(t)
	rob := w.engine(t.TempDir())
	other := mustIdentity(t).ID()
	if _, err := rob.Report(other, "spam", 0); err == nil {
		t.Fatal("reporting needs a sign-in")
	}
	w.signUp(rob, "rob@example.com")
	if _, err := rob.Report(other, "   ", 0); err == nil {
		t.Fatal("a report needs a reason")
	}
	if _, err := rob.Report(rob.Username(), "me", 0); err == nil {
		t.Fatal("can't report yourself")
	}
	if _, err := rob.Report(other, "spam", 0); err != nil {
		t.Fatal(err)
	}
}

func TestSuspendedAccountIsLockedOut(t *testing.T) {
	w := newWorld(t)
	rob := w.engine(t.TempDir())
	w.signUp(rob, "rob@example.com")
	w.studio.suspend("rob@example.com")
	rob.CheckAccount()
	a := rob.Account()
	if a.Gate != GateBanned || !strings.Contains(a.Reason, "suspended") {
		t.Fatalf("suspended account: %+v", a)
	}
}

func TestAccountsNeedAuthorityKey(t *testing.T) {
	_, err := Open(Config{DataDir: t.TempDir(), Listen: "127.0.0.1:0", Accounts: account.New("http://x", "http://y")})
	if err == nil {
		t.Fatal("accounts without an operator key can't verify names and should refuse to start")
	}
}

func TestNoAccountsMeansNoGate(t *testing.T) {
	e := open(t, t.TempDir())
	defer e.Close()
	if g := e.Account().Gate; g != GateOff {
		t.Fatalf("gate = %q", g)
	}
	if err := e.SignUp("a@b.co", "pw"); err == nil {
		t.Fatal("sign-up should say accounts are unavailable")
	}
}

func nil2ctx() context.Context { return context.Background() }

func readFile(t *testing.T, dir, name string) (string, error) {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, name))
	return string(b), err
}

func removeFile(t *testing.T, dir, name string) {
	t.Helper()
	if err := os.Remove(filepath.Join(dir, name)); err != nil {
		t.Fatal(err)
	}
}

func TestStartingFreshOnANewComputerKeepsTheName(t *testing.T) {
	w := newWorld(t)
	rob := w.engine(t.TempDir())
	kate := w.engine(t.TempDir())
	w.signUp(rob, "rob@example.com")
	w.signUp(kate, "kate@example.com")
	if _, err := rob.Claim("rob"); err != nil {
		t.Fatal(err)
	}
	oldID := rob.Username()
	// Rob turned the account's copy off, so it can't follow him.
	if err := rob.SetCopy(false); err != nil {
		t.Fatal(err)
	}
	rob.Close()

	// Rob's new computer: same account, brand-new identity, nothing to bring.
	fresh := w.engine(t.TempDir())
	err := fresh.SignIn("rob@example.com", "hunter22")
	if !errors.Is(err, ErrNoCopy) {
		t.Fatalf("a second computer with no copy to bring should say so, got %v", err)
	}
	if g := fresh.Account().Gate; g != GateSignedOut {
		t.Fatalf("gate = %q", g)
	}
	// He chooses to start fresh there.
	if err := fresh.StartFresh("rob@example.com", "hunter22"); err != nil {
		t.Fatal(err)
	}
	if fresh.MyHandle() != "rob" {
		t.Fatalf("the name should come along, got %q", fresh.MyHandle())
	}
	// Other people now reach Rob's new identity by his name.
	if !waitFor(10*time.Second, func() bool {
		id, err := kate.Add("@rob", "")
		return err == nil && id == fresh.Username()
	}) {
		t.Fatalf("@rob should resolve to the new computer (%s), not the old one (%s)", fresh.Username(), oldID)
	}
}

func TestVulgarNameIsRefusedBeforeTheServerIsAsked(t *testing.T) {
	w := newWorld(t)
	rob := w.engine(t.TempDir())
	w.signUp(rob, "rob@example.com")
	if _, err := rob.Claim("big_a55"); !errors.Is(err, mailbox.ErrIndecentName) {
		t.Fatalf("a vulgar name should be refused at once: %v", err)
	}
	if rob.MyHandle() != "" {
		t.Fatal("a refused name must not be kept")
	}
	if _, err := rob.Claim("rob"); err != nil {
		t.Fatalf("a good name still works: %v", err)
	}
}
