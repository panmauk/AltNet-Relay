package engine

import (
	"errors"
	"sort"
	"strings"
	"time"

	"altnet-relay/state"
)

// Reporting a coworker.
//
// At work, the person who created a workspace decides who belongs in it. A
// member who has a problem with a coworker doesn't block them and doesn't
// report them to AltNet: they report them to the owner, who sees who reported
// whom, why, and (if the reporter chose) what the person wrote -- and then
// removes them or doesn't. Nothing is sent to AltNet, the reported person is
// not told, and nothing happens by itself.
//
// This is a sealed message to the owner and nobody else, like any other
// group message (it travels as one, with an operation of its own). Reports
// are kept on the owner's computer only.

type repWire struct {
	ID     string   `json:"id"`
	Who    string   `json:"who"`
	Reason string   `json:"reason"`
	Ev     []evWire `json:"ev,omitempty"`
}

type evWire struct {
	T string `json:"t"`
	S int64  `json:"s"`
	W string `json:"w,omitempty"`
}

const (
	maxReportReason           = 600 // characters of reason a report to an owner carries
	maxReportEvidenceLines    = 10
	maxReportEvidenceLine     = 300
	maxWorkspaceReportsPerDay = 3 // from one member, so nobody can flood an owner
	maxStoredReports          = 200
)

var (
	errReportOwner = errors.New("the person who runs the workspace decides who stays. If they are the problem, you can leave the workspace")
	errReportSelf  = errors.New("you run this workspace: you can remove them yourself from People")
)

// ReportCoworker reports a member of a workspace to its owner. attach says
// whether to include their recent messages (what they wrote to you directly
// and in the workspace's general chat), up to ten lines.
func (e *Engine) ReportCoworker(workspaceID, peerID, reason string, attach bool) error {
	if err := e.allowed(); err != nil {
		return err
	}
	g, ok := e.State.GroupMeta(workspaceID)
	if !ok || g.Kind != state.KindWorkspace || g.Status != state.GroupJoined {
		return errNotInGroup
	}
	me, owner := e.Username(), e.adminOf(g)
	switch {
	case me == owner:
		return errReportSelf
	case peerID == me:
		return errors.New("that's you")
	case peerID == owner:
		return errReportOwner
	case !contains(g.Members, peerID) || !contains(g.Joined, peerID):
		return errors.New("they're not in this workspace")
	}
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return errors.New("say what happened, in a sentence or two")
	}
	rep := &repWire{ID: newID(), Who: peerID, Reason: clip(reason, maxReportReason)}
	if attach {
		rep.Ev = e.reportEvidence(g, peerID)
	}
	e.fanOut(workspaceID, []string{owner}, groupWire{G: workspaceID, Op: gopReport, Rep: rep})
	return nil
}

// reportEvidence collects what the person wrote that the reporter can see:
// to them directly, and in the workspace's general chat. Newest last, at most
// ten lines.
func (e *Engine) reportEvidence(g state.Group, peerID string) []evWire {
	var lines []evWire
	for _, m := range e.State.Thread(peerID) {
		if !m.Outbound && !strings.HasPrefix(m.ID, missedID) {
			lines = append(lines, evWire{T: clip(m.Text, maxReportEvidenceLine), S: m.Sent, W: "direct"})
		}
	}
	for _, m := range e.State.GroupThread(g.ID) {
		if !m.System && m.From == peerID {
			lines = append(lines, evWire{T: clip(m.Text, maxReportEvidenceLine), S: m.Sent, W: "general"})
		}
	}
	sort.SliceStable(lines, func(i, j int) bool { return lines[i].S < lines[j].S })
	if len(lines) > maxReportEvidenceLines {
		lines = lines[len(lines)-maxReportEvidenceLines:]
	}
	return lines
}

// groupReport is a report arriving. Only the owner of a workspace takes one,
// only from a member who has joined it, and only about another member who has.
func (e *Engine) groupReport(from string, w groupWire) {
	r := w.Rep
	me := e.Username()
	g, ok := e.State.GroupMeta(w.G)
	if r == nil || !ok || g.Kind != state.KindWorkspace || g.Status != state.GroupJoined || e.adminOf(g) != me {
		return
	}
	if from == me || !contains(g.Members, from) || !contains(g.Joined, from) {
		return
	}
	if r.Who == from || r.Who == me || !contains(g.Members, r.Who) || !contains(g.Joined, r.Who) {
		return
	}
	reason := strings.TrimSpace(r.Reason)
	if reason == "" || r.ID == "" || len(r.ID) > 64 {
		return
	}
	rec := state.WorkspaceReport{ID: r.ID, From: from, Who: r.Who, Reason: clip(reason, maxReportReason), At: time.Now().Unix()}
	for i, l := range r.Ev {
		if i >= maxReportEvidenceLines {
			break
		}
		where := l.W
		if where != "direct" && where != "general" {
			where = ""
		}
		rec.Evidence = append(rec.Evidence, state.ReportLine{Text: clip(l.T, maxReportEvidenceLine), Sent: l.S, Where: where})
	}
	stored := false
	err := e.State.UpdateGroup(w.G, false, func(g *state.Group) error {
		cutoff := time.Now().Add(-24 * time.Hour).Unix()
		recent := 0
		for _, x := range g.Reports {
			if x.ID == rec.ID {
				return errors.New("already have it")
			}
			if x.From == from && x.At >= cutoff {
				if x.Who == rec.Who {
					return errors.New("already reported them today")
				}
				recent++
			}
		}
		if recent >= maxWorkspaceReportsPerDay {
			return errors.New("too many reports today")
		}
		g.Reports = append(g.Reports, rec)
		if len(g.Reports) > maxStoredReports {
			g.Reports = g.Reports[len(g.Reports)-maxStoredReports:]
		}
		stored = true
		return nil
	})
	if err != nil || !stored {
		return
	}
	_ = e.State.Save()
	e.groupChanged(w.G)
	if e.OnReport != nil {
		e.OnReport(w.G)
	}
}

// WorkspaceReportView is one report waiting for the owner.
type WorkspaceReportView struct {
	ID       string
	FromName string
	WhoName  string
	WhoID    string
	Reason   string
	Evidence []state.ReportLine
	At       int64
}

// WorkspaceReports lists the reports waiting for us, as the owner of a
// workspace, newest first.
func (e *Engine) WorkspaceReports(workspaceID string) ([]WorkspaceReportView, error) {
	g, ok := e.State.GroupMeta(workspaceID)
	if !ok || g.Kind != state.KindWorkspace {
		return nil, state.ErrNoGroup
	}
	if e.adminOf(g) != e.Username() {
		return nil, errNotAdmin
	}
	var out []WorkspaceReportView
	for i := len(g.Reports) - 1; i >= 0; i-- {
		r := g.Reports[i]
		if r.Done {
			continue
		}
		out = append(out, WorkspaceReportView{
			ID: r.ID, FromName: e.Name(r.From), WhoName: e.Name(r.Who), WhoID: r.Who,
			Reason: r.Reason, Evidence: r.Evidence, At: r.At,
		})
	}
	return out, nil
}

// DismissReport marks a report as dealt with without removing anyone.
// (Removing the person settles their reports by itself.)
func (e *Engine) DismissReport(workspaceID, reportID string) error {
	g, ok := e.State.GroupMeta(workspaceID)
	if !ok || g.Kind != state.KindWorkspace {
		return state.ErrNoGroup
	}
	if e.adminOf(g) != e.Username() {
		return errNotAdmin
	}
	found := false
	err := e.State.UpdateGroup(workspaceID, false, func(g *state.Group) error {
		for i := range g.Reports {
			if g.Reports[i].ID == reportID {
				g.Reports[i].Done = true
				found = true
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	if !found {
		return errors.New("no such report")
	}
	_ = e.State.Save()
	e.groupChanged(workspaceID)
	return nil
}
