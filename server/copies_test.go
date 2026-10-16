package server

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"altnet/core/crypto"

	"altnet-relay/mailbox"
)

const devA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
const devB = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"

func (r *rig) putCopy(token, device string, blob []byte) int {
	r.t.Helper()
	req, _ := http.NewRequest("PUT", r.web.URL+"/v1/backup", bytes.NewReader(blob))
	req.Header.Set("Authorization", "Bearer "+token)
	if device != "" {
		req.Header.Set("X-Relay-Device", device)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		r.t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

func (r *rig) getCopy(token string) (int, []byte) {
	r.t.Helper()
	req, _ := http.NewRequest("GET", r.web.URL+"/v1/backup", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		r.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b
}

func lockedCopy(tag string) []byte {
	return []byte("ANRB1" + strings.Repeat(tag, 80))
}

func TestCopyBelongsToTheComputerInUse(t *testing.T) {
	r := newRig(t)
	r.bind("alice", newIdentity(t))
	if c, _ := r.getCopy("alice"); c != 404 {
		t.Fatalf("no copy yet: %d", c)
	}
	// Before claiming the account, a computer can't save a copy.
	if c := r.putCopy("alice", devA, lockedCopy("a")); c != 409 {
		t.Fatalf("unclaimed: %d", c)
	}
	if c, _ := r.do("POST", "/v1/device", "alice", map[string]any{"device": devA}); c != 200 {
		t.Fatalf("claim: %d", c)
	}
	if c := r.putCopy("alice", devA, lockedCopy("a")); c != 200 {
		t.Fatalf("save: %d", c)
	}
	if c, b := r.getCopy("alice"); c != 200 || !bytes.Equal(b, lockedCopy("a")) {
		t.Fatalf("read back: %d", c)
	}
	_, me := r.do("GET", "/v1/me", "alice", nil)
	if me["device"] != devA || me["backup_at"] == nil {
		t.Fatalf("me should say which computer and when it was saved: %v", me)
	}
	// Bob can't read or write Alice's copy.
	if c, _ := r.getCopy("bob"); c != 404 {
		t.Fatalf("someone else's copy: %d", c)
	}

	// She signs in on a second computer: it takes over. The first can no
	// longer overwrite the copy, and /me tells it so.
	r.do("POST", "/v1/device", "alice", map[string]any{"device": devB})
	r.srv.now = func() time.Time { return time.Now().Add(time.Minute) }
	if c := r.putCopy("alice", devA, lockedCopy("x")); c != 409 {
		t.Fatalf("the old computer overwrote the copy: %d", c)
	}
	if c := r.putCopy("alice", devB, lockedCopy("b")); c != 200 {
		t.Fatalf("the new computer saves: %d", c)
	}
	// Not more often than every few seconds.
	if c := r.putCopy("alice", devB, lockedCopy("c")); c != 429 {
		t.Fatalf("saving twice at once: %d", c)
	}
	// Only locked copies are kept, and not too big ones.
	r.srv.now = func() time.Time { return time.Now().Add(2 * time.Minute) }
	if c := r.putCopy("alice", devB, []byte(strings.Repeat("{}", 100))); c != 400 {
		t.Fatalf("plain data: %d", c)
	}
	if c := r.putCopy("alice", devB, append([]byte("ANRB1"), make([]byte, MaxCopySize+1)...)); c != 413 {
		t.Fatalf("too big: %d", c)
	}
	if c := r.putCopy("alice", "", lockedCopy("b")); c != 400 {
		t.Fatalf("no device: %d", c)
	}

	// Turning the copy off deletes it.
	if c, _ := r.do("DELETE", "/v1/backup", "alice", nil); c != 200 {
		t.Fatalf("delete: %d", c)
	}
	if c, _ := r.getCopy("alice"); c != 404 {
		t.Fatalf("still there after delete: %d", c)
	}
}

func TestStartingFreshMovesTheAccountAndItsName(t *testing.T) {
	r, _, _ := deletionRig(t) // gives the server a password checker
	first, second := newIdentity(t), newIdentity(t)
	r.bind("alice", first)
	r.claimName("alice", "alice")

	// Without asking, a different identity is refused...
	if c, _ := r.bind("alice", second); c != 409 {
		t.Fatalf("silent replace: %d", c)
	}
	// A sign-in token alone can't move it: a stolen token mustn't be
	// enough to take her @name.
	fresh := func(pw string) (int, map[string]any) {
		ts := time.Now().Unix()
		return r.do("POST", "/v1/identity", "alice", map[string]any{
			"username": second.Username(), "pubkey": hexKey(second), "ts": ts,
			"sig": second.Signing.Sign(BindMessage(second.Username(), ts)), "replace": true, "password": pw,
		})
	}
	if c, _ := fresh(""); c != 403 {
		t.Fatalf("start fresh without the password: %d", c)
	}
	if c, _ := fresh("guess"); c != 403 {
		t.Fatalf("start fresh with a wrong password: %d", c)
	}
	// ...with her password, "start fresh here" moves the account.
	c, out := fresh("correct horse")
	if c != 200 {
		t.Fatalf("start fresh: %d %v", c, out)
	}
	_, sn := r.claimName("alice", "alice")
	var rec mailbox.HandleRecord
	if err := json.Unmarshal(sn.Record, &rec); err != nil {
		t.Fatal(err)
	}
	if rec.Username != second.Username() {
		t.Fatal("the name should follow the account to its new keys")
	}
	// Someone else's identity still can't be taken over.
	if c, _ := r.bind("bob", second); c != 409 {
		t.Fatalf("another account took her identity: %d", c)
	}
}

func TestDeletingAnAccountRemovesItsCopy(t *testing.T) {
	r, mail, _ := deletionRig(t)
	r.bind("alice", newIdentity(t))
	r.do("POST", "/v1/device", "alice", map[string]any{"device": devA})
	r.putCopy("alice", devA, lockedCopy("a"))
	r.do("POST", "/v1/account/delete/start", "alice", map[string]any{})
	if c, msg := r.deleteStep("alice", mail.lastCode(t), "correct horse", "alice@example.com", true); c != 200 {
		t.Fatalf("delete: %d %s", c, msg)
	}
	r.srv.auth.(*fakeVerifier).accounts["alice"] = Principal{Email: "alice@example.com"}
	if c, _ := r.getCopy("alice"); c != 404 {
		t.Fatalf("the copy outlived the account: %d", c)
	}
}

func hexKey(id *mailbox.Identity) string { return crypto.PublicKeyToHex(id.Signing.PublicKey) }
