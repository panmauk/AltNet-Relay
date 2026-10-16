package engine

import (
	"regexp"
	"sync"
	"testing"

	"altnet-relay/server"
)

type codeMailer struct {
	mu   sync.Mutex
	last string
}

func (m *codeMailer) Send(_, _, body string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.last = body
	return nil
}

func (m *codeMailer) code(t *testing.T) string {
	t.Helper()
	m.mu.Lock()
	defer m.mu.Unlock()
	c := regexp.MustCompile(`code: (\d{6})`).FindStringSubmatch(m.last)
	if c == nil {
		t.Fatal("no code was emailed")
	}
	return c[1]
}

func TestDeletingAnAccountEndToEnd(t *testing.T) {
	w := newWorld(t)
	mail := &codeMailer{}
	w.srv.SetDeletion(mail, &server.BackendVerifier{BaseURL: w.studio.web.URL})

	rob := w.engine(t.TempDir())
	w.signUp(rob, "rob@example.com")
	if _, err := rob.Claim("rob"); err != nil {
		t.Fatal(err)
	}

	if err := rob.StartAccountDeletion(); err != nil {
		t.Fatal(err)
	}
	code := mail.code(t)
	// The steps one at a time, as the app asks for them.
	if err := rob.CheckAccountDeletion("111111", "", ""); err == nil {
		t.Fatal("a wrong code was accepted")
	}
	if err := rob.CheckAccountDeletion(code, "", ""); err != nil {
		t.Fatalf("code: %v", err)
	}
	if err := rob.CheckAccountDeletion(code, "wrong", ""); err == nil {
		t.Fatal("a wrong password was accepted")
	}
	if err := rob.CheckAccountDeletion(code, "hunter22", "kate"); err == nil {
		t.Fatal("a wrong name was accepted")
	}
	if err := rob.CheckAccountDeletion(code, "hunter22", "@rob"); err != nil {
		t.Fatalf("all steps: %v", err)
	}
	if rob.Account().Gate != GateOK {
		t.Fatal("checking steps must not delete anything")
	}

	if err := rob.DeleteAccount(code, "hunter22", "rob"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if g := rob.Account().Gate; g != GateSignedOut {
		t.Fatalf("after deleting, the app should be signed out, gate = %q", g)
	}
	// The account is really gone, and its name can't be taken by anyone.
	if err := rob.SignIn("rob@example.com", "hunter22"); err == nil {
		t.Fatal("signed in to a deleted account")
	}
	kate := w.engine(t.TempDir())
	w.signUp(kate, "kate@example.com")
	if _, err := kate.Claim("rob"); err == nil {
		t.Fatal("a deleted account's name was handed to someone else")
	}
}
