package engine

import (
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"altnet-relay/state"
)

func waitReports(t *testing.T, owner *Engine, ws string, n int) []WorkspaceReportView {
	t.Helper()
	var got []WorkspaceReportView
	if !waitPolled(owner, 10*time.Second, func() bool {
		got, _ = owner.WorkspaceReports(ws)
		return len(got) == n
	}) {
		t.Fatalf("the owner should have %d report(s), has %d", n, len(got))
	}
	return got
}

// A member reports a coworker; the owner sees who, about whom, why and what
// they wrote; nothing goes to AltNet and nothing happens by itself.
func TestMemberReportsCoworkerToTheOwner(t *testing.T) {
	rob, kate, zed, ws := workspaceOfThree(t)
	var told int32
	rob.OnReport = func(string) { atomic.AddInt32(&told, 1) }

	// Zed is unpleasant, in the general chat and to kate directly.
	if _, err := zed.SendGroup(ws, "kate's work is garbage"); err != nil {
		t.Fatal(err)
	}
	if _, err := zed.Send(kate.Username(), "you should quit"); err != nil {
		t.Fatal(err)
	}
	if !waitPolled(kate, 10*time.Second, func() bool {
		return groupHas(kate, ws, "kate's work is garbage") && threadHas(kate, zed.Username(), "you should quit")
	}) {
		t.Fatal("zed's messages never reached kate")
	}

	if err := kate.ReportCoworker(ws, zed.Username(), "he keeps putting me down", true); err != nil {
		t.Fatal(err)
	}
	got := waitReports(t, rob, ws, 1)[0]
	if got.WhoID != zed.Username() || got.Reason != "he keeps putting me down" || got.FromName == "" {
		t.Fatalf("the owner should see who reported whom and why: %+v", got)
	}
	var texts []string
	for _, l := range got.Evidence {
		texts = append(texts, l.Where+":"+l.Text)
	}
	joined := strings.Join(texts, "|")
	if !strings.Contains(joined, "general:kate's work is garbage") || !strings.Contains(joined, "direct:you should quit") {
		t.Fatalf("what zed wrote should come with it: %v", texts)
	}
	if atomic.LoadInt32(&told) != 1 {
		t.Fatal("the owner's app should be told a report arrived")
	}
	if g, _ := groupSummary(rob, ws); g.Reports != 1 {
		t.Fatalf("the workspace should show one report waiting: %+v", g)
	}
	// Nothing happened to zed by itself, and he wasn't told.
	if groupStatus(zed, ws) != state.GroupJoined {
		t.Fatal("a report alone must not remove anyone")
	}
	if _, err := zed.WorkspaceReports(ws); err == nil {
		t.Fatal("only the owner can read reports")
	}
	if _, err := kate.WorkspaceReports(ws); err == nil {
		t.Fatal("only the owner can read reports, not the reporter either")
	}

	// The owner removes him: the report is dealt with.
	if err := rob.RemoveFromGroup(ws, zed.Username()); err != nil {
		t.Fatal(err)
	}
	if left, _ := rob.WorkspaceReports(ws); len(left) != 0 {
		t.Fatalf("removing someone settles the reports about them: %+v", left)
	}
}

func TestOwnerCanDismissAReport(t *testing.T) {
	rob, kate, zed, ws := workspaceOfThree(t)
	if err := kate.ReportCoworker(ws, zed.Username(), "rude in a meeting", false); err != nil {
		t.Fatal(err)
	}
	r := waitReports(t, rob, ws, 1)[0]
	if len(r.Evidence) != 0 {
		t.Fatal("without the box ticked, no messages go with a report")
	}
	if err := kate.DismissReport(ws, r.ID); err == nil {
		t.Fatal("only the owner dismisses")
	}
	if err := rob.DismissReport(ws, r.ID); err != nil {
		t.Fatal(err)
	}
	if left, _ := rob.WorkspaceReports(ws); len(left) != 0 {
		t.Fatal("a dismissed report is no longer waiting")
	}
	if groupStatus(zed, ws) != state.GroupJoined {
		t.Fatal("dismissing removes nobody")
	}
}

func TestWhoCanReportWhomAtWork(t *testing.T) {
	rob, kate, zed, ws := workspaceOfThree(t)
	if err := rob.ReportCoworker(ws, kate.Username(), "x", false); !errors.Is(err, errReportSelf) {
		t.Fatalf("the owner removes people, they don't report them: %v", err)
	}
	if err := kate.ReportCoworker(ws, rob.Username(), "x", false); !errors.Is(err, errReportOwner) {
		t.Fatalf("the owner can't be reported to themselves: %v", err)
	}
	if err := kate.ReportCoworker(ws, kate.Username(), "x", false); err == nil {
		t.Fatal("you can't report yourself")
	}
	if err := kate.ReportCoworker(ws, zed.Username(), "   ", false); err == nil {
		t.Fatal("a report needs a reason")
	}
	outsider := open(t, t.TempDir(), rob.Peer.LocalAddr())
	t.Cleanup(outsider.Close)
	if err := kate.ReportCoworker(ws, outsider.Username(), "x", false); err == nil {
		t.Fatal("someone outside the workspace can't be reported to its owner")
	}
}

// A forged report -- from outside, about an outsider, or to someone who
// doesn't run the workspace -- is ignored, and nobody can flood the owner.
func TestWorkspaceReportsHoldAgainstModifiedClients(t *testing.T) {
	rob, kate, zed, ws := workspaceOfThree(t)
	outsider := open(t, t.TempDir(), rob.Peer.LocalAddr())
	t.Cleanup(outsider.Close)
	time.Sleep(200 * time.Millisecond)

	// From someone who isn't in it.
	sendWire(t, outsider, rob, groupWire{G: ws, Op: gopReport, Rep: &repWire{ID: newID(), Who: kate.Username(), Reason: "fake"}})
	// To a member who doesn't run it.
	sendWire(t, kate, zed, groupWire{G: ws, Op: gopReport, Rep: &repWire{ID: newID(), Who: rob.Username(), Reason: "mutiny"}})
	// About someone who isn't in it.
	sendWire(t, kate, rob, groupWire{G: ws, Op: gopReport, Rep: &repWire{ID: newID(), Who: outsider.Username(), Reason: "who?"}})
	settle(rob)
	settle(zed)
	if got, _ := rob.WorkspaceReports(ws); len(got) != 0 {
		t.Fatalf("forged reports must be ignored: %+v", got)
	}
	g, _ := zed.State.GroupMeta(ws)
	if len(g.Reports) != 0 {
		t.Fatal("a member who doesn't run the workspace keeps no reports")
	}

	// The same person reporting the same coworker again the same day is one
	// report, not two.
	for i := 0; i < 3; i++ {
		if err := kate.ReportCoworker(ws, zed.Username(), "again", false); err != nil {
			t.Fatal(err)
		}
	}
	waitReports(t, rob, ws, 1)
	settle(rob)
	if got, _ := rob.WorkspaceReports(ws); len(got) != 1 {
		t.Fatalf("repeats of the same report must not pile up: %d", len(got))
	}
}
