package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"testing"
)

type fakeMailer struct {
	mu   sync.Mutex
	sent []string // bodies
	fail error
}

func (m *fakeMailer) Send(to, subject, body string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.fail != nil {
		return m.fail
	}
	m.sent = append(m.sent, to+"\n"+body)
	return nil
}

func (m *fakeMailer) lastCode(t *testing.T) string {
	t.Helper()
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.sent) == 0 {
		t.Fatal("no email sent")
	}
	c := regexp.MustCompile(`code: (\d{6})`).FindStringSubmatch(m.sent[len(m.sent)-1])
	if c == nil {
		t.Fatal("no code in the email")
	}
	return c[1]
}

type fakeAccounts struct {
	mu        sync.Mutex
	passwords map[string]string
	deleted   []string
	fail      error
}

func (f *fakeAccounts) CheckPassword(_ context.Context, email, pw string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.passwords[email] != pw {
		return ErrWrongPassword
	}
	return nil
}

func (f *fakeAccounts) DeleteAccount(_ context.Context, token string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail != nil {
		return f.fail
	}
	f.deleted = append(f.deleted, token)
	return nil
}

func deletionRig(t *testing.T) (*rig, *fakeMailer, *fakeAccounts) {
	r := newRig(t)
	m := &fakeMailer{}
	a := &fakeAccounts{passwords: map[string]string{"alice@example.com": "correct horse"}}
	r.srv.SetDeletion(m, a)
	return r, m, a
}

func (r *rig) deleteStep(token, code, pw, name string, confirm bool) (int, string) {
	r.t.Helper()
	status, out := r.do("POST", "/v1/account/delete", token, map[string]any{"code": code, "password": pw, "name": name, "confirm": confirm})
	msg, _ := out["error"].(string)
	return status, msg
}

func TestDeletingAnAccountTakesEveryStep(t *testing.T) {
	r, mail, accts := deletionRig(t)
	alice, bob := newIdentity(t), newIdentity(t)
	r.bind("alice", alice)
	if c, _ := r.claimName("alice", "alice"); c != 200 {
		t.Fatalf("claim: %d", c)
	}
	r.bind("bob", bob)
	r.reportAs("alice", bob) // a report she made: it stays, without her

	if c, _ := r.do("POST", "/v1/account/delete/start", "alice", map[string]any{}); c != 200 {
		t.Fatalf("start: %d", c)
	}
	code := mail.lastCode(t)
	if !strings.HasPrefix(mail.sent[0], "alice@example.com\n") {
		t.Fatal("the code must go to the account's own address")
	}

	// Each step on its own, as the app asks for them.
	if c, msg := r.deleteStep("alice", "000000", "", "", false); c != 400 || !strings.Contains(msg, "code") {
		t.Fatalf("wrong code: %d %q", c, msg)
	}
	if c, _ := r.deleteStep("alice", code, "", "", false); c != 200 {
		t.Fatalf("right code: %d", c)
	}
	if c, msg := r.deleteStep("alice", code, "wrong", "", false); c != 400 || !strings.Contains(msg, "password") {
		t.Fatalf("wrong password: %d %q", c, msg)
	}
	if c, msg := r.deleteStep("alice", code, "correct horse", "bob", false); c != 400 || !strings.Contains(msg, "name") {
		t.Fatalf("wrong name: %d %q", c, msg)
	}
	if c, _ := r.deleteStep("alice", code, "correct horse", "@Alice", false); c != 200 {
		t.Fatalf("all right so far: %d", c)
	}
	if len(accts.deleted) != 0 {
		t.Fatal("nothing may be deleted before the final confirmation")
	}
	// A modified app can't skip a step at the end.
	if c, _ := r.deleteStep("alice", code, "", "alice", true); c != 400 {
		t.Fatalf("confirm without the password: %d", c)
	}

	if c, msg := r.deleteStep("alice", code, "correct horse", "alice", true); c != 200 {
		t.Fatalf("delete: %d %q", c, msg)
	}
	if len(accts.deleted) != 1 || accts.deleted[0] != "alice" {
		t.Fatalf("the backend account wasn't deleted: %v", accts.deleted)
	}
	r.srv.store.View(func(d *fileData) {
		if d.Accounts["alice@example.com"] != nil {
			t.Error("her record is still here")
		}
		if d.Handles["alice"] != deletedOwner {
			t.Errorf("the name should stay taken by nobody, is %q", d.Handles["alice"])
		}
		for _, rep := range d.Reports {
			if rep.ReporterEmail == "alice@example.com" {
				t.Error("a report still names her")
			}
		}
		if len(d.Reports) != 1 {
			t.Error("the report she made should stay")
		}
	})
	// Nobody can have her name now.
	r.bind("carol", newIdentity(t))
	if c, _ := r.claimName("carol", "alice"); c == 200 {
		t.Fatal("a deleted account's name was given to someone else")
	}
	// The code is spent.
	if c, _ := r.deleteStep("alice", code, "", "", false); c != 400 {
		t.Fatalf("code used twice: %d", c)
	}
}

func TestAccountWithoutANameTypesItsEmail(t *testing.T) {
	r, mail, _ := deletionRig(t)
	r.bind("alice", newIdentity(t))
	r.do("POST", "/v1/account/delete/start", "alice", map[string]any{})
	code := mail.lastCode(t)
	if c, msg := r.deleteStep("alice", code, "", "alice", false); c != 400 || !strings.Contains(msg, "email") {
		t.Fatalf("a name she doesn't have: %d %q", c, msg)
	}
	if c, _ := r.deleteStep("alice", code, "", "Alice@Example.com", false); c != 200 {
		t.Fatalf("her email: %d", c)
	}
}

func TestTooManyWrongTriesSpendTheCode(t *testing.T) {
	r, mail, _ := deletionRig(t)
	r.do("POST", "/v1/account/delete/start", "alice", map[string]any{})
	code := mail.lastCode(t)
	for i := 0; i < deleteMaxTries; i++ {
		r.deleteStep("alice", code, "guess", "", false)
	}
	if c, _ := r.deleteStep("alice", code, "", "", false); c != 429 {
		t.Fatalf("after %d wrong passwords the code should be spent: %d", deleteMaxTries, c)
	}
}

func TestDeletionCodesAreLimitedAndPrivate(t *testing.T) {
	r, mail, _ := deletionRig(t)
	for i := 0; i < deleteCodesPerDay; i++ {
		if c, _ := r.do("POST", "/v1/account/delete/start", "alice", map[string]any{}); c != 200 {
			t.Fatalf("code %d: %d", i+1, c)
		}
	}
	if c, _ := r.do("POST", "/v1/account/delete/start", "alice", map[string]any{}); c != 429 {
		t.Fatalf("a fourth code in a day: %d", c)
	}
	// Bob can't use Alice's code: codes belong to the signed-in account.
	code := mail.lastCode(t)
	if c, _ := r.deleteStep("bob", code, "", "", false); c != 400 {
		t.Fatalf("someone else's code: %d", c)
	}
	if c, _ := r.do("POST", "/v1/account/delete/start", "", map[string]any{}); c != 401 {
		t.Fatalf("signed out: %d", c)
	}
}

func TestBannedAccountsCannotDeleteThemselves(t *testing.T) {
	r, _, accts := deletionRig(t)
	alice := newIdentity(t)
	r.bind("alice", alice)
	if c, _ := r.do("POST", "/v1/admin/ban", "admin", map[string]any{"username": alice.Username(), "note": "spam"}); c != 200 {
		t.Fatalf("ban: %d", c)
	}
	if c, _ := r.do("POST", "/v1/account/delete/start", "alice", map[string]any{}); c != 403 {
		t.Fatalf("banned start: %d", c)
	}
	if c, _ := r.deleteStep("alice", "123456", "correct horse", "alice", true); c != 403 || len(accts.deleted) != 0 {
		t.Fatalf("banned delete: %d", c)
	}
}

func TestBackendFailureLeavesEverythingForAnotherTry(t *testing.T) {
	r, mail, accts := deletionRig(t)
	r.bind("alice", newIdentity(t))
	r.claimName("alice", "alice")
	r.do("POST", "/v1/account/delete/start", "alice", map[string]any{})
	code := mail.lastCode(t)
	accts.fail = errors.New("down")
	if c, _ := r.deleteStep("alice", code, "correct horse", "alice", true); c != 502 {
		t.Fatalf("backend down: %d", c)
	}
	r.srv.store.View(func(d *fileData) {
		if d.Accounts["alice@example.com"] == nil {
			t.Fatal("nothing should be deleted when the backend failed")
		}
	})
	accts.fail = nil
	if c, msg := r.deleteStep("alice", code, "correct horse", "alice", true); c != 200 {
		t.Fatalf("second try: %d %q", c, msg)
	}
}

func TestDeletionOffWithoutEmail(t *testing.T) {
	r := newRig(t)
	if c, _ := r.do("POST", "/v1/account/delete/start", "alice", map[string]any{}); c != 503 {
		t.Fatalf("not configured: %d", c)
	}
}

func TestFailedEmailLeavesNoCode(t *testing.T) {
	r, mail, _ := deletionRig(t)
	mail.fail = errors.New("smtp down")
	if c, _ := r.do("POST", "/v1/account/delete/start", "alice", map[string]any{}); c != 503 {
		t.Fatalf("mail down: %d", c)
	}
	if c, msg := r.deleteStep("alice", "000000", "", "", false); c != 400 || !strings.Contains(msg, "expired") {
		t.Fatalf("no code should be pending: %d %q", c, msg)
	}
}

func TestBackendPasswordCheckAndDelete(t *testing.T) {
	var deleted string
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == "POST" && r.URL.Path == "/v1/login":
			var in map[string]string
			_ = json.NewDecoder(r.Body).Decode(&in)
			if in["email"] == "a@example.com" && in["password"] == "pw" {
				w.Write([]byte(`{"token":"t"}`))
				return
			}
			w.WriteHeader(401)
		case r.Method == "DELETE" && r.URL.Path == "/v1/me":
			if r.Header.Get("Authorization") != "Bearer tok" {
				w.WriteHeader(401)
				return
			}
			deleted = "tok"
			w.Write([]byte(`{"ok":true}`))
		default:
			w.WriteHeader(404)
		}
	}))
	defer backend.Close()
	v := &BackendVerifier{BaseURL: backend.URL}
	ctx := context.Background()
	if err := v.CheckPassword(ctx, "a@example.com", "pw"); err != nil {
		t.Fatal(err)
	}
	if err := v.CheckPassword(ctx, "a@example.com", "nope"); !errors.Is(err, ErrWrongPassword) {
		t.Fatalf("wrong password: %v", err)
	}
	if err := v.DeleteAccount(ctx, "other"); err == nil {
		t.Fatal("a refused delete must be an error")
	}
	if err := v.DeleteAccount(ctx, "tok"); err != nil || deleted != "tok" {
		t.Fatalf("delete: %v", err)
	}
}
