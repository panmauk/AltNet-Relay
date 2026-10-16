package engine

import (
	"strings"
	"testing"
	"time"

	"altnet-relay/state"
)

// reportedBy has each of reporters report target (and says how many of the
// reports reached the server's queue, banned or not).
func (w *world) reportsAbout(target string) int {
	w.t.Helper()
	_, out := w.adminDo("GET", "/v1/admin/reports?status=all", nil)
	n := 0
	for _, r := range out["reports"].([]any) {
		if r.(map[string]any)["reported_username"] == target {
			n++
		}
	}
	return n
}

// Three different people reporting one account bans it, everywhere, with no
// administrator involved -- and blocking and reporting work together.
func TestThreeReportsBanAnAccountEverywhere(t *testing.T) {
	w := newWorld(t)
	rob, kate, zed, bad := w.engine(t.TempDir()), w.engine(t.TempDir()), w.engine(t.TempDir()), w.engine(t.TempDir())
	w.signUp(rob, "rob@example.com")
	w.signUp(kate, "kate@example.com")
	w.signUp(zed, "zed@example.com")
	w.signUp(bad, "bad@example.com")

	// The person is awful to everyone.
	for _, e := range []*Engine{rob, kate, zed} {
		id, err := bad.Add(e.Username(), e.Username()[:6])
		if err != nil {
			t.Fatal(err)
		}
		if _, err := bad.Send(id, "you are all idiots"); err != nil {
			t.Fatal(err)
		}
	}
	for _, e := range []*Engine{rob, kate, zed} {
		if !waitPolled(e, 8*time.Second, func() bool { return threadHas(e, bad.Username(), "you are all idiots") }) {
			t.Fatal("a message never arrived")
		}
	}

	// Rob blocks and reports (the two things the window's Block box does).
	if err := rob.Block(bad.Username()); err != nil {
		t.Fatal(err)
	}
	if _, err := rob.Report(bad.Username(), "abuse", 5); err != nil {
		t.Fatal(err)
	}
	if _, err := kate.Report(bad.Username(), "abuse", 0); err != nil {
		t.Fatal(err)
	}
	// Two reports: nothing happens, to him or to anyone.
	bad.CheckAccount()
	if a := bad.Account(); a.Gate != GateOK {
		t.Fatalf("two reports must not ban: %+v", a)
	}
	kate.RefreshBans()
	if kate.IsBanned(bad.Username()) {
		t.Fatal("nobody is banned by two reports")
	}

	// The third report bans him.
	if _, err := zed.Report(bad.Username(), "abuse", 0); err != nil {
		t.Fatal(err)
	}
	bad.CheckAccount()
	a := bad.Account()
	if a.Gate != GateBanned || !strings.Contains(a.Reason, "several different people") {
		t.Fatalf("bad should be shown the ban and why: %+v", a)
	}
	if strings.Contains(a.Reason, "rob") || strings.Contains(a.Reason, "kate") || strings.Contains(a.Reason, "zed") {
		t.Fatalf("the reason must not say who reported: %q", a.Reason)
	}
	for _, e := range []*Engine{rob, kate, zed} {
		e.RefreshBans()
		if !e.IsBanned(bad.Username()) {
			t.Fatalf("everyone's app should now treat bad as banned")
		}
		if s, ok := summaryFor(e, bad.Username()); ok && !s.Banned {
			t.Fatalf("his conversation should show as banned: %+v", s)
		}
		if _, err := e.Send(bad.Username(), "hello?"); err == nil {
			t.Fatal("nobody can write to a banned account")
		}
	}
	if _, err := bad.Send(rob.Username(), "let me back"); err == nil {
		t.Fatal("a banned account can't send")
	}
	// Nothing is waiting for a person to look at.
	_, pending := w.adminDo("GET", "/v1/admin/reports", nil)
	if pending["count"].(float64) != 0 {
		t.Fatalf("the reports should be settled, not queued: %v", pending)
	}
}

// Blocking and reporting to AltNet are Personal tools, and stay that way even
// for someone you also work with. What the app does differently is in
// Business: it leaves them out of work chats, where the workspace's owner
// decides (see wsreport_test.go).
func TestPersonalBlockAndReportStillWorkForSomeoneYouAlsoWorkWith(t *testing.T) {
	w := newWorld(t)
	rob, kate, zed := w.engine(t.TempDir()), w.engine(t.TempDir()), w.engine(t.TempDir())
	w.signUp(rob, "rob@example.com")
	w.signUp(kate, "kate@example.com")
	w.signUp(zed, "zed@example.com")

	ws, err := rob.CreateWorkspace("Acme", []string{kate.Username(), zed.Username()})
	if err != nil {
		t.Fatal(err)
	}
	waitInvited(t, kate, ws)
	waitInvited(t, zed, ws)
	if err := kate.JoinGroup(ws); err != nil {
		t.Fatal(err)
	}
	if err := zed.JoinGroup(ws); err != nil {
		t.Fatal(err)
	}
	for _, e := range []*Engine{rob, kate, zed} {
		waitAllJoined(t, e, ws, 3)
	}

	// The window is told who is a coworker, so it can leave the buttons out of
	// Business.
	if _, err := kate.Send(rob.Username(), "quick question"); err != nil {
		t.Fatal(err)
	}
	if !waitPolled(rob, 10*time.Second, func() bool { return threadHas(rob, kate.Username(), "quick question") }) {
		t.Fatal("the coworker's message never arrived")
	}
	if s, ok := summaryFor(rob, kate.Username()); !ok || !s.Coworker {
		t.Fatalf("a coworker should be marked as one: %+v", s)
	}
	// In Personal, the same person can be blocked and reported to AltNet.
	if err := kate.Block(zed.Username()); err != nil {
		t.Fatalf("blocking is yours to do: %v", err)
	}
	if err := kate.Unblock(zed.Username()); err != nil {
		t.Fatal(err)
	}
	if _, err := kate.Report(zed.Username(), "rude to me in private", 0); err != nil {
		t.Fatalf("reporting to AltNet works for someone you also work with: %v", err)
	}
	if w.reportsAbout(zed.Username()) != 1 {
		t.Fatal("that report should have reached AltNet")
	}
	// Only the owner removes people; a member can't, even to help.
	if err := kate.RemoveFromGroup(ws, zed.Username()); err == nil {
		t.Fatal("a member must not be able to remove a coworker")
	}
	if err := rob.RemoveFromGroup(ws, zed.Username()); err != nil {
		t.Fatal(err)
	}
	if !waitPolled(kate, 10*time.Second, func() bool { g, _ := kate.State.Group(ws); return !g.HasMember(zed.Username()) }) {
		t.Fatal("kate should hear zed was removed")
	}
	if !waitPolled(zed, 10*time.Second, func() bool { return groupStatus(zed, ws) == state.GroupRemoved }) {
		t.Fatal("zed should know he was removed")
	}
}

// Block an account you were invited by, rather than join: the invitation is
// where an unwanted person can be said no to, since colleagues can't be.
func TestDecliningAnInvitationAndBlockingTheSender(t *testing.T) {
	rob, kate, zed := trio(t)
	ws, err := rob.CreateWorkspace("Spam Inc", []string{zed.Username()})
	if err != nil {
		t.Fatal(err)
	}
	waitInvited(t, zed, ws)
	g, _ := groupSummary(zed, ws)
	if g.InvitedByID != rob.Username() {
		t.Fatalf("an invitation should say who sent it: %+v", g)
	}
	if err := zed.DeclineGroup(ws); err != nil {
		t.Fatal(err)
	}
	if err := zed.Block(g.InvitedByID); err != nil {
		t.Fatal(err)
	}
	// A second invitation from the blocked sender never shows up.
	ws2, err := rob.CreateWorkspace("Spam Again", []string{zed.Username(), kate.Username()})
	if err != nil {
		t.Fatal(err)
	}
	waitInvited(t, kate, ws2)
	settle(zed)
	if groupStatus(zed, ws2) != "" {
		t.Fatalf("a blocked sender's invitation must not arrive: %q", groupStatus(zed, ws2))
	}
}
