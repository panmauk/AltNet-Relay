package engine

import (
	"encoding/json"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"altnet-relay/mailbox"
	"altnet-relay/state"
)

// workspaceOfThree makes a joined workspace "Acme" owned by rob, with kate and
// zed in it, and waits until every device knows everyone has accepted. Kate
// and zed have never met outside it; rob and kate are contacts.
func workspaceOfThree(t *testing.T) (rob, kate, zed *Engine, ws string) {
	t.Helper()
	rob, kate, zed = trio(t)
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
	return
}

// waitAllJoined waits until e sees n people in ws, none of them pending.
func waitAllJoined(t *testing.T, e *Engine, ws string, n int) {
	t.Helper()
	if !waitPolled(e, 10*time.Second, func() bool {
		m, p, ok := e.GroupSize(ws)
		return ok && m == n && p == 0
	}) {
		m, p, _ := e.GroupSize(ws)
		t.Fatalf("never saw everyone joined: %d members, %d pending", m, p)
	}
}

func groupSummary(e *Engine, id string) (GroupSummary, bool) {
	for _, g := range e.Groups() {
		if g.ID == id {
			return g, true
		}
	}
	return GroupSummary{}, false
}

func waitChannel(t *testing.T, e *Engine, id string) {
	t.Helper()
	if !waitPolled(e, 10*time.Second, func() bool { _, ok := groupSummary(e, id); return ok && groupStatus(e, id) == state.GroupJoined }) {
		t.Fatalf("channel never opened (status %q)", groupStatus(e, id))
	}
}

func pendingOf(e *Engine, id, peerID string) (pending, found bool) {
	ms, _ := e.GroupMembers(id)
	for _, m := range ms {
		if m.PeerID == peerID {
			return m.Pending, true
		}
	}
	return false, false
}

// sendWire has from deliver a hand-made group message to to, the way a
// modified client could.
func sendWire(t *testing.T, from, to *Engine, w any) {
	t.Helper()
	blob, err := json.Marshal(w)
	if err != nil {
		t.Fatal(err)
	}
	conv := from.State.Conversation(to.Username())
	if _, _, err := from.Inbox.SendKind(conv, mailbox.KindGroup, string(blob)); err != nil {
		t.Fatal(err)
	}
}

func TestWorkspaceInvitationsAreAnsweredAndEveryoneSeesWho(t *testing.T) {
	rob, kate, zed := trio(t)
	ws, err := rob.CreateWorkspace("Acme", []string{kate.Username(), zed.Username()})
	if err != nil {
		t.Fatal(err)
	}
	waitInvited(t, kate, ws)
	waitInvited(t, zed, ws)

	// An invitation says what it is.
	inv, _ := groupSummary(zed, ws)
	if inv.Kind != state.KindWorkspace || inv.Status != state.GroupInvited || inv.Name != "Acme" || inv.Members != 3 {
		t.Fatalf("zed's invitation: %+v", inv)
	}
	// The owner sees both as waiting.
	if _, p, _ := rob.GroupSize(ws); p != 2 {
		t.Fatalf("both invitees should be pending for the owner, got %d", p)
	}

	if err := kate.JoinGroup(ws); err != nil {
		t.Fatal(err)
	}
	// Kate said yes: the owner hears it, and she is no longer pending.
	if !waitPolled(rob, 10*time.Second, func() bool { p, ok := pendingOf(rob, ws, kate.Username()); return ok && !p }) {
		t.Fatal("the owner never heard that kate joined")
	}
	// Everyone sees who hasn't answered: kate sees zed as invited, rob not.
	if p, _ := pendingOf(kate, ws, zed.Username()); !p {
		t.Fatal("kate should see zed as still invited")
	}
	if p, _ := pendingOf(kate, ws, rob.Username()); p {
		t.Fatal("the owner is never pending")
	}
	// Zed, still deciding, already learns that kate joined.
	if !waitPolled(zed, 10*time.Second, func() bool { p, ok := pendingOf(zed, ws, kate.Username()); return ok && !p }) {
		t.Fatal("an invitee should hear who has joined too")
	}

	// Zed says no: gone from the roster, and everyone is told he declined.
	if err := zed.DeclineGroup(ws); err != nil {
		t.Fatal(err)
	}
	for _, e := range []*Engine{rob, kate} {
		if !waitPolled(e, 10*time.Second, func() bool { g, _ := e.State.Group(ws); return !g.HasMember(zed.Username()) }) {
			t.Fatal("zed should be gone")
		}
		declined := false
		for _, m := range e.GroupThread(ws) {
			if m.System && m.Text == e.Name(zed.Username())+" declined the invitation" {
				declined = true
			}
		}
		if !declined {
			t.Fatalf("should say zed declined: %+v", e.GroupThread(ws))
		}
	}
}

func TestWorkspaceGeneralChatAndCaps(t *testing.T) {
	rob, kate, zed, ws := workspaceOfThree(t)
	if _, err := rob.SendGroup(ws, "welcome to Acme"); err != nil {
		t.Fatal(err)
	}
	for _, e := range []*Engine{kate, zed} {
		if !waitPolled(e, 10*time.Second, func() bool { return groupHas(e, ws, "welcome to Acme") }) {
			t.Fatal("the general chat never reached a member")
		}
	}
	// A workspace is bigger than a plain group.
	if MaxWorkspaceMembers <= MaxGroupMembers {
		t.Fatal("workspaces should hold more people than groups")
	}
	// It can start empty: invite people afterwards.
	solo, err := rob.CreateWorkspace("Just me", nil)
	if err != nil {
		t.Fatal(err)
	}
	if g, _ := groupSummary(rob, solo); g.Members != 1 || !g.Admin {
		t.Fatalf("a new empty workspace: %+v", g)
	}
	if _, err := rob.CreateGroup("Nobody", nil); err == nil {
		t.Fatal("a plain group still needs someone in it")
	}
}

func TestDeliveredCountLeavesOutPeopleStillDeciding(t *testing.T) {
	rob, kate, zed := trio(t)
	ws, err := rob.CreateWorkspace("Acme", []string{kate.Username(), zed.Username()})
	if err != nil {
		t.Fatal(err)
	}
	waitInvited(t, kate, ws)
	if err := kate.JoinGroup(ws); err != nil {
		t.Fatal(err)
	}
	if !waitPolled(rob, 10*time.Second, func() bool { m, p, _ := rob.GroupSize(ws); return m == 3 && p == 1 }) {
		m, p, _ := rob.GroupSize(ws)
		t.Fatalf("three in it, one still deciding: got %d/%d", m, p)
	}
	_ = zed
}

func TestChannelsAreMadeByMembersAndNeedNoSecondYes(t *testing.T) {
	rob, kate, zed, ws := workspaceOfThree(t)

	// Kate, an ordinary member, makes a channel with zed in it.
	ch, err := kate.CreateChannel(ws, "design", []string{zed.Username()})
	if err != nil {
		t.Fatal(err)
	}
	waitChannel(t, zed, ch)
	z, _ := groupSummary(zed, ch)
	if z.Kind != state.KindChannel || z.WS != ws || z.Name != "design" || z.Members != 2 {
		t.Fatalf("zed's channel: %+v", z)
	}
	// Zed wasn't asked: he was added, because he is in the workspace -- and
	// it shows as unread, so he notices.
	if z.Status != state.GroupJoined || z.Unread == 0 {
		t.Fatalf("a channel needs no second yes, and is marked new: %+v", z)
	}

	if _, err := kate.SendGroup(ch, "mockups are up"); err != nil {
		t.Fatal(err)
	}
	if !waitPolled(zed, 10*time.Second, func() bool { return groupHas(zed, ch, "mockups are up") }) {
		t.Fatal("the channel message never arrived")
	}
	// The owner, who is not in the channel, never sees it.
	if _, ok := groupSummary(rob, ch); ok {
		t.Fatal("the owner is not in kate's channel and mustn't see it")
	}
	if groupHas(rob, ch, "mockups are up") {
		t.Fatal("the owner got a message from a channel they're not in")
	}

	// The creator runs it. The workspace owner does not.
	if err := rob.RenameGroup(ch, "mine"); err == nil {
		t.Fatal("the workspace owner shouldn't rename someone else's channel")
	}
	if err := kate.RenameGroup(ch, "design-team"); err != nil {
		t.Fatal(err)
	}
	if !waitPolled(zed, 10*time.Second, func() bool { g, _ := groupSummary(zed, ch); return g.Name == "design-team" }) {
		t.Fatal("zed never saw the new channel name")
	}

	// Only people in the workspace can be in a channel.
	outsider := open(t, t.TempDir(), rob.Peer.LocalAddr())
	t.Cleanup(outsider.Close)
	time.Sleep(200 * time.Millisecond)
	if _, err := kate.CreateChannel(ws, "leaks", []string{outsider.Username()}); err == nil {
		t.Fatal("a channel can only hold workspace members")
	}
	if err := kate.AddToGroup(ch, []string{outsider.Username()}); err == nil {
		t.Fatal("adding an outsider to a channel must fail")
	}
	// And a channel can't be made in a workspace you're not in.
	if _, err := outsider.CreateChannel(ws, "sneaky", nil); err == nil {
		t.Fatal("an outsider can't make a channel in a workspace")
	}
}

func TestLeavingTheWorkspaceTakesYouOutOfItsChannels(t *testing.T) {
	rob, kate, zed, ws := workspaceOfThree(t)
	ch, err := kate.CreateChannel(ws, "launch", []string{zed.Username()})
	if err != nil {
		t.Fatal(err)
	}
	waitChannel(t, zed, ch)

	// The owner removes zed from the company.
	if err := rob.RemoveFromGroup(ws, zed.Username()); err != nil {
		t.Fatal(err)
	}
	if !waitPolled(zed, 10*time.Second, func() bool { return groupStatus(zed, ws) == state.GroupRemoved }) {
		t.Fatalf("zed should be out of the workspace (status %q)", groupStatus(zed, ws))
	}
	// ...which closes its channels on his side, with the history readable.
	if groupStatus(zed, ch) != state.GroupRemoved {
		t.Fatalf("zed's channel should be closed too: %q", groupStatus(zed, ch))
	}
	// And on kate's he is taken out of the channel itself, whoever runs it.
	if !waitPolled(kate, 10*time.Second, func() bool { g, _ := kate.State.Group(ch); return !g.HasMember(zed.Username()) }) {
		t.Fatal("kate's channel should no longer list zed")
	}
	if _, err := zed.SendGroup(ch, "can I still talk?"); err == nil {
		t.Fatal("a removed member can't post")
	}
	if _, err := kate.SendGroup(ch, "after zed"); err != nil {
		t.Fatal(err)
	}
	settle(zed)
	if groupHas(zed, ch, "after zed") {
		t.Fatal("a removed member must not get the channel's new messages")
	}
}

func TestMemberLeavingTheWorkspaceLeavesItsChannels(t *testing.T) {
	_, kate, zed, ws := workspaceOfThree(t)
	ch, err := kate.CreateChannel(ws, "ops", []string{zed.Username()})
	if err != nil {
		t.Fatal(err)
	}
	waitChannel(t, zed, ch)
	if err := zed.LeaveGroup(ws); err != nil {
		t.Fatal(err)
	}
	if groupStatus(zed, ch) != state.GroupLeft {
		t.Fatalf("leaving the workspace should close its channels: %q", groupStatus(zed, ch))
	}
	// Kate's device takes him out of the channel, not just out of the count.
	if !waitPolled(kate, 10*time.Second, func() bool { g, _ := kate.State.Group(ch); return !g.HasMember(zed.Username()) }) {
		t.Fatal("kate's channel should no longer list zed")
	}
	if g, _ := groupSummary(kate, ch); g.Members != 1 {
		t.Fatalf("kate's channel should be down to her: %+v", g)
	}
}

// Someone removed from a workspace and invited back later starts fresh: the
// channels they were in don't quietly open to them again.
func TestReinvitedMemberDoesNotGetOldChannelsBack(t *testing.T) {
	rob, kate, zed, ws := workspaceOfThree(t)
	ch, err := kate.CreateChannel(ws, "finance", []string{zed.Username()})
	if err != nil {
		t.Fatal(err)
	}
	waitChannel(t, zed, ch)
	if err := rob.RemoveFromGroup(ws, zed.Username()); err != nil {
		t.Fatal(err)
	}
	if !waitPolled(kate, 10*time.Second, func() bool { g, _ := kate.State.Group(ch); return !g.HasMember(zed.Username()) }) {
		t.Fatal("kate's channel should drop zed")
	}
	if !waitPolled(zed, 10*time.Second, func() bool { return groupStatus(zed, ws) == state.GroupRemoved }) {
		t.Fatal("zed should be removed")
	}

	// Invited back, and he accepts.
	if err := rob.AddToGroup(ws, []string{zed.Username()}); err != nil {
		t.Fatal(err)
	}
	waitInvited(t, zed, ws)
	if err := zed.JoinGroup(ws); err != nil {
		t.Fatal(err)
	}
	waitAllJoined(t, kate, ws, 3)

	if g, _ := groupSummary(kate, ch); g.Members != 1 {
		t.Fatalf("rejoining the workspace must not put zed back in the channel: %+v", g)
	}
	if _, err := kate.SendGroup(ch, "numbers for the board"); err != nil {
		t.Fatal(err)
	}
	settle(zed)
	if groupHas(zed, ch, "numbers for the board") {
		t.Fatal("a channel message reached someone no longer in it")
	}
	if groupStatus(zed, ch) == state.GroupJoined {
		t.Fatal("zed's old channel must stay closed")
	}

	// Kate can still add him back on purpose, and that works.
	if err := kate.AddToGroup(ch, []string{zed.Username()}); err != nil {
		t.Fatal(err)
	}
	waitChannel(t, zed, ch)
}

func TestChannelCreatorLeavingHandsItOn(t *testing.T) {
	rob, kate, zed, ws := workspaceOfThree(t)
	ch, err := kate.CreateChannel(ws, "project", []string{zed.Username(), rob.Username()})
	if err != nil {
		t.Fatal(err)
	}
	waitChannel(t, zed, ch)
	waitChannel(t, rob, ch)
	// Kate is removed from the company by the owner.
	if err := rob.RemoveFromGroup(ws, kate.Username()); err != nil {
		t.Fatal(err)
	}
	for _, e := range []*Engine{rob, zed} {
		if !waitPolled(e, 10*time.Second, func() bool { g, _ := groupSummary(e, ch); return g.Members == 2 }) {
			t.Fatal("the channel should be down to two")
		}
	}
	// Everyone still in the channel agrees who runs it now.
	rg, _ := rob.State.Group(ch)
	zg, _ := zed.State.Group(ch)
	if rob.adminOf(rg) != zed.adminOf(zg) || rob.adminOf(rg) == kate.Username() {
		t.Fatalf("the two devices disagree about who runs it: %s vs %s", rob.adminOf(rg), zed.adminOf(zg))
	}
	admin := rob
	if zed.adminOf(zg) == zed.Username() {
		admin = zed
	}
	if err := admin.RenameGroup(ch, "project-2"); err != nil {
		t.Fatalf("the new admin should be able to rename: %v", err)
	}
}

// When the owner leaves, the workspace goes to someone who has accepted, not
// to an invitee who never answered.
func TestOwnerLeavingHandsOverToSomeoneWhoJoined(t *testing.T) {
	rob, kate, zed := trio(t)
	// Zed is invited first, so he's next in the list, but he never answers.
	ws, err := rob.CreateWorkspace("Acme", []string{zed.Username(), kate.Username()})
	if err != nil {
		t.Fatal(err)
	}
	waitInvited(t, zed, ws)
	waitInvited(t, kate, ws)
	if err := kate.JoinGroup(ws); err != nil {
		t.Fatal(err)
	}
	if !waitPolled(rob, 10*time.Second, func() bool { p, ok := pendingOf(rob, ws, kate.Username()); return ok && !p }) {
		t.Fatal("rob never heard kate joined")
	}
	if err := rob.LeaveGroup(ws); err != nil {
		t.Fatal(err)
	}
	for _, e := range []*Engine{kate, zed} {
		if !waitPolled(e, 10*time.Second, func() bool { g, _ := e.State.Group(ws); return !g.HasMember(rob.Username()) }) {
			t.Fatal("rob's leaving never arrived")
		}
		if g, _ := e.State.Group(ws); g.Admin != kate.Username() {
			t.Fatalf("the workspace should go to kate, who joined, not %s", g.Admin)
		}
	}
	// And she can run it.
	if err := kate.RenameGroup(ws, "Acme Ltd"); err != nil {
		t.Fatalf("the new owner should be able to rename: %v", err)
	}
}

// A channel invitation can overtake the invitation to its workspace (they
// come from different people). It waits on disk, out of sight, and opens when
// the workspace is joined.
func TestChannelInvitationBeforeTheWorkspaceInvitationWaits(t *testing.T) {
	rob, kate, zed := trio(t)
	ws, ch := newID(), newID()
	chWire := groupWire{G: ch, Op: gopWInvite, Snap: &groupSnap{
		Name: "early", Admin: kate.Username(), Members: []string{kate.Username(), zed.Username()}, V: 1,
		Kind: state.KindChannel, WS: ws,
	}}
	// The channel one arrives first: kept, but not shown.
	zed.channelInvite(kate.Username(), chWire)
	if g, ok := zed.State.Group(ch); !ok || g.Status != state.GroupInvited {
		t.Fatalf("an early channel invitation should be kept: %+v %v", g, ok)
	}
	for _, g := range zed.Groups() {
		if g.ID == ch {
			t.Fatal("a channel of an unknown workspace must stay out of the list")
		}
	}
	// Now the workspace invitation (from rob, who owns it).
	wsWire := groupWire{G: ws, Op: gopWInvite, Snap: &groupSnap{
		Name: "Acme", Admin: rob.Username(), Members: []string{rob.Username(), kate.Username(), zed.Username()}, V: 1,
		Kind: state.KindWorkspace, J: []string{rob.Username(), kate.Username()},
	}}
	zed.groupInvite(rob.Username(), wsWire)
	if g, ok := zed.State.Group(ch); !ok || g.Status != state.GroupInvited {
		t.Fatalf("the channel should still be waiting: %+v %v", g, ok)
	}
	for _, g := range zed.Groups() {
		if g.ID == ch {
			t.Fatal("a channel of an unanswered workspace must stay out of the list")
		}
	}
	if err := zed.JoinGroup(ws); err != nil {
		t.Fatal(err)
	}
	if g, ok := groupSummary(zed, ch); !ok || g.Status != state.GroupJoined {
		t.Fatalf("joining the workspace should open the channel: %+v %v", g, ok)
	}
}

func TestEarlyChannelInvitationsAreCheckedAndLimited(t *testing.T) {
	rob, kate, zed := trio(t)
	ws := newID()
	early := func(from *Engine, id string) groupWire {
		return groupWire{G: id, Op: gopWInvite, Snap: &groupSnap{
			Name: "x", Admin: from.Username(), Members: []string{from.Username(), zed.Username()}, V: 1,
			Kind: state.KindChannel, WS: ws,
		}}
	}
	// One sender can't fill this device up with made-up channels.
	var ids []string
	for i := 0; i < maxEarlyChannelsPerSender+3; i++ {
		id := newID()
		ids = append(ids, id)
		zed.channelInvite(kate.Username(), early(kate, id))
	}
	kept := 0
	for _, id := range ids {
		if _, ok := zed.State.Group(id); ok {
			kept++
		}
	}
	if kept != maxEarlyChannelsPerSender {
		t.Fatalf("one sender may leave %d early invitations, kept %d", maxEarlyChannelsPerSender, kept)
	}

	// Someone who turns out not to be in the workspace: dropped when it arrives.
	stranger := open(t, t.TempDir(), rob.Peer.LocalAddr())
	t.Cleanup(stranger.Close)
	bogus := newID()
	zed.channelInvite(stranger.Username(), early(stranger, bogus))
	if _, ok := zed.State.Group(bogus); !ok {
		t.Fatal("the invitation should be held until the workspace is known")
	}
	// A channel claiming the very ID the workspace will have is replaced by it.
	squat := groupWire{G: ws, Op: gopWInvite, Snap: &groupSnap{
		Name: "squat", Admin: stranger.Username(), Members: []string{stranger.Username(), zed.Username()}, V: 1,
		Kind: state.KindChannel, WS: newID(),
	}}
	zed.channelInvite(stranger.Username(), squat)

	zed.groupInvite(rob.Username(), groupWire{G: ws, Op: gopWInvite, Snap: &groupSnap{
		Name: "Acme", Admin: rob.Username(), Members: []string{rob.Username(), kate.Username(), zed.Username()}, V: 1,
		Kind: state.KindWorkspace,
	}})
	if _, ok := zed.State.Group(bogus); ok {
		t.Fatal("an early channel from someone outside the workspace must be dropped")
	}
	if g, ok := zed.State.Group(ws); !ok || g.Kind != state.KindWorkspace || g.Status != state.GroupInvited {
		t.Fatalf("the real workspace invitation should win over a squatter: %+v", g)
	}
	if _, ok := zed.State.Group(ids[0]); !ok {
		t.Fatal("kate's early channel is valid and should stay")
	}
}

func TestWorkspaceRulesHoldAgainstModifiedClients(t *testing.T) {
	rob, kate, zed, ws := workspaceOfThree(t)
	// Zed is in the workspace but forges a channel invitation that claims kate
	// runs it: only the sender can be the channel's admin.
	forged := newID()
	sendWire(t, zed, rob, groupWire{G: forged, Op: gopWInvite, Snap: &groupSnap{
		Name: "forged", Admin: kate.Username(), Members: []string{kate.Username(), rob.Username()}, V: 1,
		Kind: state.KindChannel, WS: ws,
	}})
	// An outsider who isn't in the workspace tries it as its admin.
	outsider := open(t, t.TempDir(), rob.Peer.LocalAddr())
	t.Cleanup(outsider.Close)
	time.Sleep(200 * time.Millisecond)
	intruder := newID()
	sendWire(t, outsider, rob, groupWire{G: intruder, Op: gopWInvite, Snap: &groupSnap{
		Name: "intruder", Admin: outsider.Username(), Members: []string{outsider.Username(), rob.Username()}, V: 1,
		Kind: state.KindChannel, WS: ws,
	}})
	// A workspace dressed up as a plain group invitation (what Relay 0.2 would
	// have mistaken it for) is ignored.
	disguised := newID()
	sendWire(t, outsider, rob, groupWire{G: disguised, Op: gopInvite, Snap: &groupSnap{
		Name: "disguised", Admin: outsider.Username(), Members: []string{outsider.Username(), rob.Username()}, V: 1,
		Kind: state.KindWorkspace,
	}})
	// A plain group can't be turned into a workspace by an update, under
	// either operation.
	g, err := rob.CreateGroup("Friends", []string{kate.Username()})
	if err != nil {
		t.Fatal(err)
	}
	waitInvited(t, kate, g)
	_ = kate.JoinGroup(g)
	for _, op := range []string{gopUpdate, gopWUpdate} {
		sendWire(t, rob, kate, groupWire{G: g, Op: op, Snap: &groupSnap{
			Name: "Friends", Admin: rob.Username(), Members: []string{rob.Username(), kate.Username()}, V: 9,
			Kind: state.KindWorkspace,
		}})
	}
	settle(rob)
	settle(kate)
	for _, id := range []string{forged, intruder, disguised} {
		if _, ok := rob.State.Group(id); ok {
			t.Fatalf("a forged invitation took effect: %s", id)
		}
	}
	if k, _ := kate.State.Group(g); k.Kind != state.KindPlain || k.Version != 1 {
		t.Fatalf("a group changed what it is: %+v", k)
	}
}

// Someone who isn't running a channel can't take it over by "inviting" a
// member who left it.
func TestChannelTakeoverByReinviteIsRejected(t *testing.T) {
	rob, kate, zed, ws := workspaceOfThree(t)
	ch, err := kate.CreateChannel(ws, "design", []string{zed.Username(), rob.Username()})
	if err != nil {
		t.Fatal(err)
	}
	waitChannel(t, zed, ch)
	waitChannel(t, rob, ch)
	if err := zed.LeaveGroup(ch); err != nil {
		t.Fatal(err)
	}
	// Rob, a member but not the one running it, forges a fresh invitation.
	sendWire(t, rob, zed, groupWire{G: ch, Op: gopWInvite, Snap: &groupSnap{
		Name: "design", Admin: rob.Username(), Members: []string{rob.Username(), zed.Username()}, V: 99,
		Kind: state.KindChannel, WS: ws,
	}})
	settle(zed)
	g, _ := zed.State.Group(ch)
	if g.Status != state.GroupLeft || g.Admin != kate.Username() {
		t.Fatalf("the channel was taken over: %+v", g)
	}
	// Kate, who does run it, can add him back.
	if !waitPolled(kate, 10*time.Second, func() bool { g, _ := kate.State.Group(ch); return !g.HasMember(zed.Username()) }) {
		t.Fatal("kate should hear that zed left")
	}
	if err := kate.AddToGroup(ch, []string{zed.Username()}); err != nil {
		t.Fatal(err)
	}
	waitChannel(t, zed, ch)
}

func TestCoworkersChatDirectlyWithoutRequestsAndStayInBusiness(t *testing.T) {
	_, kate, zed, _ := workspaceOfThree(t)
	var asked int32
	kate.OnRequest = func(string) { atomic.AddInt32(&asked, 1) }

	// Zed and kate have never spoken and aren't contacts. In the same
	// workspace, his first message is a normal chat, not a request.
	if _, err := zed.Send(kate.Username(), "got a minute?"); err != nil {
		t.Fatal(err)
	}
	if !waitPolled(kate, 10*time.Second, func() bool { return threadHas(kate, zed.Username(), "got a minute?") }) {
		t.Fatal("the coworker's message never arrived")
	}
	s, ok := summaryFor(kate, zed.Username())
	if !ok || s.Request || !s.Accepted || !s.Biz || s.Former {
		t.Fatalf("a coworker's message is a Business chat, not a request: %+v", s)
	}
	if atomic.LoadInt32(&asked) != 0 {
		t.Fatal("a coworker must not trigger a message-request notification")
	}
	// Kate answers; it stays a Business chat on both sides, and writing to
	// him doesn't turn him into a Personal contact.
	if _, err := kate.Send(zed.Username(), "sure"); err != nil {
		t.Fatal(err)
	}
	if !waitPolled(zed, 10*time.Second, func() bool { return threadHas(zed, kate.Username(), "sure") }) {
		t.Fatal("the reply never arrived")
	}
	for _, e := range []*Engine{kate, zed} {
		other := zed.Username()
		if e == zed {
			other = kate.Username()
		}
		if s, ok := summaryFor(e, other); !ok || !s.Biz || !s.Accepted {
			t.Fatalf("both sides should see a Business chat: %+v %v", s, ok)
		}
		if e.State.IsContact(other) {
			t.Fatal("chatting in a workspace must not add a personal contact")
		}
	}
	// A stranger outside the workspace is still a request.
	outsider := open(t, t.TempDir(), kate.Peer.LocalAddr())
	t.Cleanup(outsider.Close)
	time.Sleep(200 * time.Millisecond)
	if _, err := outsider.Add(kate.Username(), "kate"); err != nil {
		t.Fatal(err)
	}
	if _, err := outsider.Send(kate.Username(), "hello from outside"); err != nil {
		t.Fatal(err)
	}
	if !waitPolled(kate, 10*time.Second, func() bool { s, ok := summaryFor(kate, outsider.Username()); return ok && s.Request && !s.Biz }) {
		t.Fatal("an outsider should still arrive as a request")
	}
}

// When someone stops being a coworker, the work chat stays in Business, closed
// -- it doesn't turn up in Personal as a message request.
func TestFormerCoworkersChatStaysInBusiness(t *testing.T) {
	rob, kate, zed, ws := workspaceOfThree(t)
	var asked int32
	kate.OnRequest = func(string) { atomic.AddInt32(&asked, 1) }
	if _, err := zed.Send(kate.Username(), "handing over the client"); err != nil {
		t.Fatal(err)
	}
	if !waitPolled(kate, 10*time.Second, func() bool { return threadHas(kate, zed.Username(), "handing over the client") }) {
		t.Fatal("the message never arrived")
	}
	if err := rob.RemoveFromGroup(ws, zed.Username()); err != nil {
		t.Fatal(err)
	}
	if !waitPolled(kate, 10*time.Second, func() bool { s, _ := summaryFor(kate, zed.Username()); return s.Former }) {
		s, _ := summaryFor(kate, zed.Username())
		t.Fatalf("kate's chat with zed should be a closed Business chat: %+v", s)
	}
	s, _ := summaryFor(kate, zed.Username())
	if !s.Biz || s.Request || s.Accepted {
		t.Fatalf("a former coworker's chat: Business, closed, not a request: %+v", s)
	}
	if !waitPolled(zed, 10*time.Second, func() bool { s, _ := summaryFor(zed, kate.Username()); return s.Former }) {
		t.Fatal("zed's side should be closed too")
	}
	if atomic.LoadInt32(&asked) != 0 {
		t.Fatal("no request should be announced")
	}
}

func TestCoworkersCanCallWithoutWritingFirst(t *testing.T) {
	_, kate, zed, _ := workspaceOfThree(t)
	sink := &callSink{}
	kate.OnCall = sink.add
	// Neither has written to the other. Outside a workspace this is refused.
	if err := zed.SendCall(CallSignal{PeerID: kate.Username(), Op: CallOffer, CallID: "ws-call-1", SDP: "x"}); err != nil {
		t.Fatalf("coworkers should be able to call: %v", err)
	}
	if !waitPolled(kate, 10*time.Second, func() bool { _, ok := sink.has(CallOffer); return ok }) {
		t.Fatal("the coworker's call never rang")
	}
}

// Someone who was only invited to a workspace isn't a coworker yet: the owner
// can invite anyone, and that mustn't make a stranger callable by everyone.
func TestInviteeWhoHasNotAnsweredIsNotACoworker(t *testing.T) {
	rob, kate, zed := trio(t)
	ws, err := rob.CreateWorkspace("Acme", []string{kate.Username(), zed.Username()})
	if err != nil {
		t.Fatal(err)
	}
	waitInvited(t, kate, ws)
	waitInvited(t, zed, ws)
	if err := kate.JoinGroup(ws); err != nil {
		t.Fatal(err)
	}
	if kate.isCoworker(zed.Username()) {
		t.Fatal("zed hasn't answered; he isn't a coworker yet")
	}
	if err := kate.SendCall(CallSignal{PeerID: zed.Username(), Op: CallOffer, CallID: "early", SDP: "x"}); err == nil {
		t.Fatal("calling someone who hasn't accepted should be refused")
	}
	// Once he accepts, he is.
	if err := zed.JoinGroup(ws); err != nil {
		t.Fatal(err)
	}
	if !waitPolled(kate, 10*time.Second, func() bool { return kate.isCoworker(zed.Username()) }) {
		t.Fatal("after joining, zed is a coworker")
	}
}

func TestChannelCallsRingOnlyChannelMembers(t *testing.T) {
	rob, kate, zed, ws := workspaceOfThree(t)
	// Rob and kate in a channel; zed is in the workspace but not the channel.
	ch, err := kate.CreateChannel(ws, "standup", []string{rob.Username()})
	if err != nil {
		t.Fatal(err)
	}
	waitChannel(t, rob, ch)
	robSink, zedSink := &callSink{}, &callSink{}
	rob.OnCall = robSink.add
	zed.OnCall = zedSink.add
	if err := kate.SendGroupCall(CallSignal{Group: ch, Op: GroupRing, CallID: "meet1"}); err != nil {
		t.Fatal(err)
	}
	if !waitPolled(rob, 10*time.Second, func() bool { _, ok := robSink.has(GroupRing); return ok }) {
		t.Fatal("a channel member should be rung")
	}
	settle(zed)
	if zedSink.count() != 0 {
		t.Fatal("someone outside the channel must not be rung")
	}
}

// The app tells the desktop about invitations and channel adds through two
// callbacks. Each fires once per event: once for an invitation, once when
// someone is added to a channel -- and a channel waiting on its workspace
// says nothing until the workspace is joined.
func TestInviteAndChannelAddCallbacksFireOnce(t *testing.T) {
	rob, kate, zed := trio(t)
	var invited, added sync.Map // group ID -> count
	count := func(m *sync.Map, id string) int {
		v, _ := m.Load(id)
		n, _ := v.(int)
		return n
	}
	bump := func(m *sync.Map) func(string) {
		return func(id string) { m.Store(id, count(m, id)+1) }
	}
	zed.OnInvited, zed.OnAdded = bump(&invited), bump(&added)

	ws, err := rob.CreateWorkspace("Acme", []string{kate.Username(), zed.Username()})
	if err != nil {
		t.Fatal(err)
	}
	waitInvited(t, zed, ws)
	waitInvited(t, kate, ws)
	if err := kate.JoinGroup(ws); err != nil {
		t.Fatal(err)
	}
	if n := count(&invited, ws); n != 1 {
		t.Fatalf("one invitation, one callback: got %d", n)
	}

	// Kate adds zed to a channel before he has joined the workspace.
	ch, err := kate.CreateChannel(ws, "early", []string{zed.Username()})
	if err != nil {
		t.Fatal(err)
	}
	settle(zed)
	if n := count(&added, ch); n != 0 {
		t.Fatalf("a channel of an unanswered workspace says nothing yet: %d", n)
	}
	if err := zed.JoinGroup(ws); err != nil {
		t.Fatal(err)
	}
	if n := count(&added, ch); n != 1 {
		t.Fatalf("joining the workspace opens the channel: one add, got %d", n)
	}

	// A channel made afterwards, with zed already in: added straight away.
	ch2, err := kate.CreateChannel(ws, "later", []string{zed.Username()})
	if err != nil {
		t.Fatal(err)
	}
	waitChannel(t, zed, ch2)
	if n := count(&added, ch2); n != 1 {
		t.Fatalf("one channel add, one callback: got %d", n)
	}
	// Renaming it isn't being added again.
	if err := kate.RenameGroup(ch2, "later-2"); err != nil {
		t.Fatal(err)
	}
	settle(zed)
	if n := count(&added, ch2); n != 1 {
		t.Fatalf("a rename must not count as another add: %d", n)
	}
	// A plain group invitation also counts as one invitation.
	g, err := rob.CreateGroup("Friends", []string{zed.Username()})
	if err != nil {
		t.Fatal(err)
	}
	waitInvited(t, zed, g)
	if n := count(&invited, g); n != 1 {
		t.Fatalf("a group invitation fires the invitation callback once: %d", n)
	}
}
