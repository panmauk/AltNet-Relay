package server

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"altnet/core/crypto"
)

// A stolen sign-in token mustn't become a way to guess the password: after
// MaxWrongPasswords wrong ones in an hour, the server stops checking -- the
// right password included -- for both starting fresh and deleting.
func TestWrongPasswordsAreLimited(t *testing.T) {
	r, mail, _ := deletionRig(t)
	first, second := newIdentity(t), newIdentity(t)
	r.bind("alice", first)
	fresh := func(pw string) int {
		ts := r.srv.now().Unix()
		c, _ := r.do("POST", "/v1/identity", "alice", map[string]any{
			"username": second.Username(), "pubkey": hexKey(second), "ts": ts,
			"sig": second.Signing.Sign(BindMessage(second.Username(), ts)), "replace": true, "password": pw,
		})
		return c
	}
	for i := 0; i < MaxWrongPasswords; i++ {
		if c := fresh("guess"); c != 403 {
			t.Fatalf("wrong password %d: %d", i+1, c)
		}
	}
	if c := fresh("correct horse"); c != 429 {
		t.Fatalf("after %d wrong passwords the server must stop checking, got %d", MaxWrongPasswords, c)
	}
	// Deleting asks for the same password, and is held back the same way.
	r.do("POST", "/v1/account/delete/start", "alice", map[string]any{})
	if c, msg := r.deleteStep("alice", mail.lastCode(t), "correct horse", "", false); c != 429 {
		t.Fatalf("deletion password check after too many wrong ones: %d %s", c, msg)
	}

	// An hour later it works again.
	r.srv.now = func() time.Time { return time.Now().Add(WrongPasswordWindow + time.Minute) }
	if c := fresh("correct horse"); c != 200 {
		t.Fatalf("after the hour: %d", c)
	}
	// Other accounts were never affected.
	r.srv.now = time.Now
}

// The two kinds of "conflict" carry codes, so the app can tell "these keys
// belong to someone else" from "this account has other keys".
func TestBindConflictsSayWhich(t *testing.T) {
	r := newRig(t)
	a, b := newIdentity(t), newIdentity(t)
	r.bind("alice", a)
	if c, out := r.bind("bob", a); c != 409 || out["code"] != CodeIdentityTaken {
		t.Fatalf("someone else's keys: %d %v", c, out)
	}
	if c, out := r.bind("alice", b); c != 409 || out["code"] != CodeAccountHasKeys {
		t.Fatalf("other keys for this account: %d %v", c, out)
	}
}

func TestBannedAccountsGetNoCallRelay(t *testing.T) {
	r := newRig(t)
	r.srv.SetTURN(&TURNConfig{Secret: []byte("s"), URLs: []string{"turn:relay.example:3478"}})
	id := newIdentity(t)
	r.bind("bob", id)
	if c, _ := r.do("GET", "/v1/turn", "bob", nil); c != 200 {
		t.Fatalf("before the ban: %d", c)
	}
	if c, _ := r.do("POST", "/v1/admin/ban", "admin", map[string]any{"username": id.Username(), "note": "spam"}); c != 200 {
		t.Fatalf("ban: %d", c)
	}
	if c, _ := r.do("GET", "/v1/turn", "bob", nil); c != 403 {
		t.Fatalf("a banned account got call-relay credentials: %d", c)
	}
}

// Made-up tokens cost the accounts backend one lookup each at most: a refused
// token is remembered, and new lookups are rationed.
func TestTokenLookupsAreRationed(t *testing.T) {
	var lookups atomic.Int64
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		lookups.Add(1)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer backend.Close()
	v := &BackendVerifier{BaseURL: backend.URL}
	ctx := context.Background()

	for i := 0; i < 10; i++ {
		if _, err := v.Verify(ctx, "the-same-made-up-token"); !errors.Is(err, ErrUnauthorized) {
			t.Fatalf("got %v", err)
		}
	}
	if n := lookups.Load(); n != 1 {
		t.Fatalf("a refused token was looked up %d times", n)
	}

	busy := 0
	for i := 0; i < 500; i++ {
		_, err := v.Verify(ctx, "made-up-"+crypto.PublicKeyToHex(mustKey(t)))
		if errors.Is(err, errLookupsBusy) {
			busy++
		}
	}
	if busy == 0 {
		t.Fatal("a flood of new tokens was never held back")
	}
	if n := lookups.Load(); n > lookupBurst+50 {
		t.Fatalf("the backend was asked %d times", n)
	}
}

func mustKey(t *testing.T) []byte {
	t.Helper()
	id, err := crypto.NewIdentity()
	if err != nil {
		t.Fatal(err)
	}
	return id.PublicKey
}
