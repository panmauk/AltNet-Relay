package engine

import (
	"errors"
	"strings"
	"testing"
	"time"

	"altnet-relay/server"
)

// texts lists a conversation's messages as "> text" (sent) or "< text".
func texts(e *Engine, peerID string) []string {
	var out []string
	for _, m := range e.Thread(peerID) {
		dir := "< "
		if m.Outbound {
			dir = "> "
		}
		out = append(out, dir+m.Text)
	}
	return out
}

func TestSigningInOnAnotherComputerBringsTheAccount(t *testing.T) {
	old := server.CopyMinGap
	server.CopyMinGap = 0
	t.Cleanup(func() { server.CopyMinGap = old })
	oldEvery := CopyCheckEvery
	CopyCheckEvery = time.Hour // only the saves the test asks for
	t.Cleanup(func() { CopyCheckEvery = oldEvery })

	w := newWorld(t)
	rob := w.engine(t.TempDir())
	kate := w.engine(t.TempDir())
	w.signUp(rob, "rob@example.com")
	w.signUp(kate, "kate@example.com")
	if _, err := rob.Claim("rob"); err != nil {
		t.Fatal(err)
	}
	robID, kateID := rob.Username(), kate.Username()

	// A conversation to bring along.
	if _, err := rob.Add(kateID, "kate"); err != nil {
		t.Fatal(err)
	}
	if _, err := rob.Send(kateID, "one"); err != nil {
		t.Fatal(err)
	}
	if !waitFor(8*time.Second, func() bool { _, ok := summaryFor(kate, robID); return ok }) {
		t.Fatal("rob's message never reached kate")
	}
	if err := kate.Accept(robID); err != nil {
		t.Fatal(err)
	}
	if _, err := kate.Send(robID, "hi rob"); err != nil {
		t.Fatal(err)
	}
	if !waitFor(8*time.Second, func() bool { return len(rob.Thread(kateID)) >= 2 }) {
		t.Fatal("kate's reply never reached rob")
	}

	// The copy is saved (locked with his password), then he writes once more,
	// so the copy is a little behind the conversation.
	rob.saveCopy()
	if rob.Copy().SavedAt == 0 {
		t.Fatalf("no copy was saved: %+v", rob.Copy())
	}
	if _, err := rob.Send(kateID, "two"); err != nil {
		t.Fatal(err)
	}

	// His second computer. Signing in brings the account; the app restarts.
	dir := t.TempDir()
	second := w.engine(dir)
	if err := second.SignIn("rob@example.com", "hunter22"); !errors.Is(err, ErrRestart) {
		t.Fatalf("signing in should bring the account here, got %v", err)
	}
	second.Close()
	second = w.engine(dir) // what the app's restart does

	if second.Username() != robID {
		t.Fatal("the second computer should be the same Rob, not a new identity")
	}
	if second.MyHandle() != "rob" {
		t.Fatalf("his name should come along, got %q", second.MyHandle())
	}
	if got := texts(second, kateID); len(got) < 2 || got[0] != "> one" || got[1] != "< hi rob" {
		t.Fatalf("the conversation should come along: %v", got)
	}
	if g := second.Account().Gate; g != GateOK {
		t.Fatalf("signed in on the second computer, gate = %q", g)
	}

	// Writing from the second computer carries on the same conversation,
	// after the message sent from the first since the copy was saved.
	if _, err := second.Send(kateID, "three"); err != nil {
		t.Fatal(err)
	}
	want := []string{"< one", "> hi rob", "< two", "< three"}
	if !waitFor(10*time.Second, func() bool { return len(texts(kate, robID)) >= 4 }) {
		t.Fatalf("kate should get everything, in order: %v", texts(kate, robID))
	}
	if got := texts(kate, robID); strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("kate's conversation is out of order or missing something:\n got %v\nwant %v", got, want)
	}

	// The first computer finds out at its next check and steps aside.
	if err := rob.CheckAccount(); err == nil {
		t.Fatal("the first computer should be told another computer has the account")
	}
	if a := rob.Account(); a.Gate != GateSignedOut || !strings.Contains(a.Reason, "another computer") {
		t.Fatalf("the first computer should be signed out, saying why: %+v", a)
	}
	if _, err := rob.Send(kateID, "from the old one"); err == nil {
		t.Fatal("the first computer must not keep writing")
	}

	// And it can't overwrite the second computer's copy.
	if err := second.Copy().Problem; err != "" {
		t.Fatalf("copy problem on the second computer: %s", err)
	}
}

func TestACopyFromBeforeAPasswordChangeSaysSo(t *testing.T) {
	w := newWorld(t)
	rob := w.engine(t.TempDir())
	w.signUp(rob, "rob@example.com")
	rob.saveCopy()
	if !waitFor(5*time.Second, func() bool { return rob.Copy().SavedAt != 0 }) {
		t.Fatal("no copy was saved")
	}
	// His password changes (a reset) before the copy was saved again.
	w.studio.mu.Lock()
	w.studio.users["rob@example.com"].password = "brand new pw"
	w.studio.mu.Unlock()

	second := w.engine(t.TempDir())
	if err := second.SignIn("rob@example.com", "brand new pw"); !errors.Is(err, ErrOldCopy) {
		t.Fatalf("an old copy should say so, got %v", err)
	}
}

func TestTurningTheCopyOffDeletesIt(t *testing.T) {
	w := newWorld(t)
	rob := w.engine(t.TempDir())
	w.signUp(rob, "rob@example.com")
	rob.saveCopy()
	if !rob.Copy().On {
		t.Fatal("the copy is on by default")
	}
	if err := rob.SetCopy(false); err != nil {
		t.Fatal(err)
	}
	if rob.Copy().On {
		t.Fatal("still on")
	}
	second := w.engine(t.TempDir())
	if err := second.SignIn("rob@example.com", "hunter22"); !errors.Is(err, ErrNoCopy) {
		t.Fatalf("with the copy off there is nothing to bring, got %v", err)
	}
}
