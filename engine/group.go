package engine

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"altnet-relay/mailbox"
	"altnet-relay/state"
)

// Group chats.
//
// A group message is sent to every member as its own sealed one-to-one
// message (kind "group") -- "fan-out", the way Signal ran groups for years.
// That reuses everything one-to-one messages already have: end-to-end
// encryption per recipient, the instant direct push, storage for whoever is
// offline, bans and blocks. The cost is one send per member, which is why
// groups are capped at MaxGroupMembers.
//
// Membership belongs to the admin (whoever created the group). Their
// numbered snapshots -- name, admin, members -- are the only thing that adds
// or removes people; anyone may remove themselves by leaving. If the admin
// leaves, the next member in the list takes over, which every device works
// out the same way.
//
// Nobody is put in a group without saying yes: an invitation waits, like a
// message request, until it is joined or declined.

const (
	// MaxGroupMembers bounds a group, the admin included.
	MaxGroupMembers = 32
	// MaxWorkspaceMembers bounds a workspace, and each of its channels. A
	// message is sealed and sent once per member, so this is a cost limit,
	// not a policy one; see workspace.go.
	MaxWorkspaceMembers = 50
	maxGroupName        = 64
)

// maxMembers is the size limit for a group of this kind.
func maxMembers(kind string) int {
	if kind == state.KindPlain {
		return MaxGroupMembers
	}
	return MaxWorkspaceMembers
}

// Group operations on the wire.
const (
	gopMsg    = "msg"
	gopInvite = "invite"
	gopUpdate = "update"
	gopLeave  = "leave"

	// Workspaces and channels travel under their own invite and update
	// operations. Relay 0.2 ignores operations it doesn't know, so it simply
	// never sees a workspace, instead of mistaking one for a group chat.
	gopWInvite = "winvite"
	gopWUpdate = "wupdate"
	gopJoined  = "joined"  // a workspace member to the others: "I accepted"
	gopReport  = "wreport" // a member to the workspace's owner: "I'm reporting someone"
)

func inviteOp(kind string) string {
	if kind == state.KindPlain {
		return gopInvite
	}
	return gopWInvite
}

func updateOp(kind string) string {
	if kind == state.KindPlain {
		return gopUpdate
	}
	return gopWUpdate
}

type groupWire struct {
	G    string     `json:"g"`
	Op   string     `json:"op"`
	MID  string     `json:"mid,omitempty"` // group message ID, the same in every member's copy
	Text string     `json:"text,omitempty"`
	Snap *groupSnap `json:"snap,omitempty"`
	Next string     `json:"next,omitempty"` // on an admin's leave: who runs it now
	Rep  *repWire   `json:"rep,omitempty"`  // on a report to the owner
}

type groupSnap struct {
	Name    string   `json:"name"`
	Admin   string   `json:"admin"`
	Members []string `json:"members"`
	V       int64    `json:"v"`
	Kind    string   `json:"kind,omitempty"` // "" | "workspace" | "channel"
	WS      string   `json:"ws,omitempty"`   // a channel's workspace
	J       []string `json:"j,omitempty"`    // a workspace's members known to have accepted
}

// snapOf is the snapshot of a group's membership that goes out to others.
func snapOf(g *state.Group) *groupSnap {
	s := &groupSnap{
		Name: g.Name, Admin: g.Admin, Members: append([]string(nil), g.Members...), V: g.Version,
		Kind: g.Kind, WS: g.WS,
	}
	if g.Kind == state.KindWorkspace {
		s.J = append([]string(nil), g.Joined...)
	}
	return s
}

// GroupSummary is one group's row in the list.
type GroupSummary struct {
	ID           string
	Name         string
	Kind         string // state.KindPlain, KindWorkspace or KindChannel
	WS           string // for a channel, its workspace's ID
	Members      int
	Pending      int // of Members, invited to the workspace and not answered yet
	Reports      int // reports waiting for us to read, if we run the workspace
	Status       string
	InvitedBy    string // name of who invited us, for an invitation
	InvitedByID  string // and their username, so the invitation can be answered with a block
	Admin        bool   // we are the admin
	Unread       int
	Preview      string
	LastOutbound bool
	LastAt       int64
}

// GroupMember is one person in a group, for the members list.
type GroupMember struct {
	PeerID  string
	Name    string
	Admin   bool
	Me      bool
	Pending bool // a workspace owner's view: invited, hasn't said yes yet
}

var (
	errNotInGroup = errors.New("you're not in this group")
	errNotAdmin   = errors.New("only the group's admin can do that")
)

// kindWord is what to call a group of this kind in a sentence.
func kindWord(kind string) string {
	switch kind {
	case state.KindWorkspace:
		return "workspace"
	case state.KindChannel:
		return "channel"
	}
	return "group"
}

func newID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

func validGroupID(id string) bool {
	if len(id) != 32 {
		return false
	}
	_, err := hex.DecodeString(id)
	return err == nil && strings.ToLower(id) == id
}

// CreateGroup starts a group with the given people (names or IDs) and
// invites them. The creator is its admin.
func (e *Engine) CreateGroup(name string, who []string) (string, error) {
	return e.create(state.KindPlain, "", name, who)
}

// create starts a group of any kind. A workspace or a channel may begin with
// only its creator in it; a plain group needs someone to talk to.
func (e *Engine) create(kind, ws, name string, who []string) (string, error) {
	if err := e.allowed(); err != nil {
		return "", err
	}
	name = strings.TrimSpace(name)
	if name == "" || len(name) > maxGroupName {
		return "", fmt.Errorf("give it a name of up to %d characters", maxGroupName)
	}
	members, err := e.resolveMembers(who)
	if err != nil {
		return "", err
	}
	if len(members) == 0 && kind == state.KindPlain {
		return "", errors.New("add at least one person")
	}
	if len(members)+1 > maxMembers(kind) {
		return "", fmt.Errorf("it can have at most %d people", maxMembers(kind))
	}
	if kind == state.KindChannel {
		wsg, ok := e.State.GroupMeta(ws)
		if !ok || wsg.Kind != state.KindWorkspace || wsg.Status != state.GroupJoined {
			return "", errors.New("you're not in that workspace")
		}
		for _, m := range members {
			if !contains(wsg.Members, m) {
				return "", fmt.Errorf("%s isn't in the workspace; invite them to it first", e.Name(m))
			}
		}
	}
	me := e.Username()
	id := newID()
	all := append([]string{me}, members...)
	err = e.State.UpdateGroup(id, true, func(g *state.Group) error {
		g.Name, g.Admin, g.Members, g.Version, g.Status = name, me, all, 1, state.GroupJoined
		g.Kind, g.WS = kind, ws
		if kind == state.KindWorkspace {
			g.Joined = []string{me}
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	switch kind {
	case state.KindWorkspace:
		e.systemLine(id, "You created the workspace")
	case state.KindChannel:
		e.systemLine(id, "You created the channel")
	default:
		e.systemLine(id, "You created the group")
	}
	for _, m := range members {
		e.openToMember(m, false)
	}
	_ = e.State.Save()
	g, _ := e.State.GroupMeta(id)
	e.fanOut(id, members, groupWire{G: id, Op: inviteOp(kind), Snap: snapOf(&g)})
	return id, nil
}

// resolveMembers turns typed names and IDs into usernames: no duplicates, not
// us, nobody banned.
func (e *Engine) resolveMembers(who []string) ([]string, error) {
	seen := map[string]bool{e.Username(): true}
	var out []string
	for _, w := range who {
		w = strings.TrimSpace(w)
		if w == "" {
			continue
		}
		id, err := mailbox.LookupUsername(e.Client.Store, w)
		if err != nil {
			if errors.Is(err, mailbox.ErrNotFound) {
				return nil, fmt.Errorf("nobody called %q was found", strings.TrimPrefix(w, "@"))
			}
			return nil, err
		}
		if e.Mod.BannedPeer(id) {
			return nil, fmt.Errorf("%s has been banned from AltNet", strings.TrimPrefix(w, "@"))
		}
		if seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	return out, nil
}

// openToMember lets a group member's messages through at full speed: their
// conversation is opened at the chain level (a stranger is capped at three
// messages) and followed, but marked group-only, so it doesn't become a chat
// of its own. Someone who is already a contact is left as they are.
//
// inPoll says the caller is already on the inbox's receiving side, holding
// its lock (a membership change arriving in a message): then the
// conversation is changed directly, since taking the lock again would wait
// forever.
func (e *Engine) openToMember(peerID string, inPoll bool) {
	if peerID == e.Username() || e.State.IsBlocked(peerID) {
		return
	}
	conv := e.State.Conversation(peerID)
	if !conv.Accepted {
		open := func() {
			conv.Accepted = true
			conv.LastReadHash = "" // reveal what the stranger cap held back
		}
		if inPoll {
			open()
		} else {
			e.Inbox.Update(open)
		}
		e.State.SetGroupOnly(peerID, true)
	}
	e.State.PutConversation(conv)
	if inPoll {
		go e.Inbox.Follow(conv) // registering a watch is a network round trip
	} else {
		e.Inbox.Follow(conv)
	}
	go e.cacheHandle(peerID)
}

// closeToFormerMember undoes openToMember once no group is shared any more:
// a group-only peer goes back to being a stranger, capped like one. inPoll
// as for openToMember.
func (e *Engine) closeToFormerMember(peerID string, inPoll bool) {
	if !e.State.IsGroupOnly(peerID) || e.sharesGroup(peerID) {
		return
	}
	if conv, ok := e.State.LookupConversation(peerID); ok {
		if inPoll {
			conv.Accepted = false
		} else {
			e.Inbox.Update(func() { conv.Accepted = false })
		}
		e.State.PutConversation(conv)
	}
	e.State.SetGroupOnly(peerID, false)
}

func (e *Engine) sharesGroup(peerID string) bool {
	for _, g := range e.State.GroupsMeta() {
		if g.Status == state.GroupJoined && contains(e.roster(g), peerID) {
			return true
		}
	}
	return false
}

// roster is who a group's messages go to and come from. For a channel it is
// its members who are still in the workspace: taking someone out of the
// workspace takes them out of every channel, whoever runs the channel.
func (e *Engine) roster(g state.Group) []string {
	r, _ := e.channelRoster(g)
	return r
}

// channelRoster is roster, plus the workspace's accepted members for a
// channel (nil otherwise).
func (e *Engine) channelRoster(g state.Group) (roster, joined []string) {
	if g.Kind != state.KindChannel {
		return g.Members, nil
	}
	ws, ok := e.State.GroupMeta(g.WS)
	if !ok {
		return nil, nil
	}
	out := make([]string, 0, len(g.Members))
	for _, m := range g.Members {
		if contains(ws.Members, m) {
			out = append(out, m)
		}
	}
	return out, ws.Joined
}

// adminOf is who runs a group. Normally that is simply its admin. A channel
// whose admin is no longer in the workspace falls to a successor (see
// successor) -- a backstop, since the people who drop out of a workspace are
// also taken out of its channels as soon as the news arrives (purgeChannels).
func (e *Engine) adminOf(g state.Group) string {
	if g.Kind != state.KindChannel {
		return g.Admin
	}
	if _, known := e.State.GroupMeta(g.WS); !known {
		return g.Admin // a channel whose workspace we haven't heard of yet
	}
	r, joined := e.channelRoster(g)
	if contains(r, g.Admin) {
		return g.Admin
	}
	return successor(r, joined, "")
}

// successor picks who runs something next from members, leaving out
// leaving: the first one known to have accepted, else simply the first.
// joined is nil for a plain group, which doesn't keep track.
func successor(members, joined []string, leaving string) string {
	first := ""
	for _, m := range members {
		if m == leaving {
			continue
		}
		if first == "" {
			first = m
		}
		if joined == nil || contains(joined, m) {
			return m
		}
	}
	return first
}

// JoinGroup accepts an invitation.
func (e *Engine) JoinGroup(id string) error {
	if err := e.allowed(); err != nil {
		return err
	}
	me := e.Username()
	var members []string
	var kind string
	err := e.State.UpdateGroup(id, false, func(g *state.Group) error {
		if g.Status != state.GroupInvited {
			return errors.New("there's no invitation to answer")
		}
		g.Status = state.GroupJoined
		members = append(members, g.Members...)
		kind = g.Kind
		if kind == state.KindWorkspace && !contains(g.Joined, me) {
			g.Joined = append(g.Joined, me)
		}
		return nil
	})
	if err != nil {
		return err
	}
	for _, m := range members {
		e.openToMember(m, false)
	}
	e.systemLine(id, "You joined")
	if kind == state.KindWorkspace {
		// Everyone is told, so each of them knows we're a coworker now and
		// stops showing us as invited; the channels we were added to while the
		// invitation waited open up.
		e.fanOut(id, e.others(members, nil), groupWire{G: id, Op: gopJoined})
		e.promoteChannels(id)
	}
	err = e.State.Save()
	go e.Inbox.PollAll() // pick up what was sent while the invitation waited
	e.groupChanged(id)
	return err
}

// DeclineGroup turns an invitation down. The members are told, so they stop
// sending to us.
func (e *Engine) DeclineGroup(id string) error {
	return e.quitGroup(id, state.GroupDeclined, "")
}

// LeaveGroup leaves a group. History stays on this device, read-only.
func (e *Engine) LeaveGroup(id string) error {
	return e.quitGroup(id, state.GroupLeft, "You left")
}

func (e *Engine) quitGroup(id, status, line string) error {
	me := e.Username()
	cur, ok := e.State.GroupMeta(id)
	if !ok {
		return errNotInGroup
	}
	// If we run it, we say who runs it next, so that every device agrees
	// even while their lists of who has accepted differ.
	var next string
	if e.adminOf(cur) == me {
		r, joined := e.channelRoster(cur)
		if cur.Kind == state.KindWorkspace {
			joined = cur.Joined
		}
		next = successor(r, joined, me)
	}
	var others []string
	var kind string
	err := e.State.UpdateGroup(id, false, func(g *state.Group) error {
		if g.Status != state.GroupJoined && g.Status != state.GroupInvited {
			return errNotInGroup
		}
		for _, m := range g.Members {
			if m != me {
				others = append(others, m)
			}
		}
		g.Status = status
		g.Unread = 0
		kind = g.Kind
		return nil
	})
	if err != nil {
		return err
	}
	if line != "" {
		e.systemLine(id, line)
	}
	if kind == state.KindWorkspace {
		e.closeChannels(id, status, "You left the workspace")
	}
	for _, m := range others {
		e.closeToFormerMember(m, false)
	}
	_ = e.State.Save()
	e.fanOut(id, others, groupWire{G: id, Op: gopLeave, Next: next})
	e.groupChanged(id)
	return nil
}

// AddToGroup adds people (names or IDs). Admin only.
func (e *Engine) AddToGroup(id string, who []string) error {
	if err := e.allowed(); err != nil {
		return err
	}
	add, err := e.resolveMembers(who)
	if err != nil {
		return err
	}
	cur, ok := e.State.GroupMeta(id)
	if !ok {
		return state.ErrNoGroup
	}
	admin := e.adminOf(cur)
	if cur.Kind == state.KindChannel {
		// A channel is made of people already in its workspace.
		ws, _ := e.State.GroupMeta(cur.WS)
		for _, m := range add {
			if !contains(ws.Members, m) {
				return fmt.Errorf("%s isn't in the workspace; invite them to it first", e.Name(m))
			}
		}
	}
	var snap *groupSnap
	var added []string
	err = e.State.UpdateGroup(id, false, func(g *state.Group) error {
		if g.Status != state.GroupJoined {
			return errNotInGroup
		}
		if admin != e.Username() {
			return errNotAdmin
		}
		for _, m := range add {
			if !g.HasMember(m) {
				added = append(added, m)
			}
		}
		if len(added) == 0 {
			return errors.New("they're already in")
		}
		if len(g.Members)+len(added) > maxMembers(g.Kind) {
			return fmt.Errorf("it can have at most %d people", maxMembers(g.Kind))
		}
		g.Admin = admin
		g.Members = append(g.Members, added...)
		g.Version++
		snap = snapOf(g)
		return nil
	})
	if err != nil {
		return err
	}
	for _, m := range added {
		e.openToMember(m, false)
		e.systemLine(id, "You added "+e.Name(m))
	}
	_ = e.State.Save()
	now, _ := e.State.GroupMeta(id)
	e.fanOut(id, e.others(e.roster(now), added), groupWire{G: id, Op: updateOp(cur.Kind), Snap: snap})
	e.fanOut(id, added, groupWire{G: id, Op: inviteOp(cur.Kind), Snap: snap})
	e.groupChanged(id)
	return nil
}

// RemoveFromGroup takes someone out. Admin only. They are told.
func (e *Engine) RemoveFromGroup(id, peerID string) error {
	if err := e.allowed(); err != nil {
		return err
	}
	cur, ok := e.State.GroupMeta(id)
	if !ok {
		return state.ErrNoGroup
	}
	admin := e.adminOf(cur)
	tell := e.roster(cur) // everyone in it, the person going included: they're told
	var snap *groupSnap
	err := e.State.UpdateGroup(id, false, func(g *state.Group) error {
		if g.Status != state.GroupJoined {
			return errNotInGroup
		}
		if admin != e.Username() {
			return errNotAdmin
		}
		if peerID == e.Username() {
			return errors.New("to leave, use Leave")
		}
		if !g.HasMember(peerID) {
			return errors.New("they're not in it")
		}
		g.Admin = admin
		g.Members = without(g.Members, peerID)
		g.Joined = without(g.Joined, peerID)
		settleReports(g, peerID)
		g.Version++
		snap = snapOf(g)
		return nil
	})
	if err != nil {
		return err
	}
	e.systemLine(id, "You removed "+e.Name(peerID))
	if cur.Kind == state.KindWorkspace {
		e.purgeChannels(id, []string{peerID})
	}
	e.closeToFormerMember(peerID, false)
	_ = e.State.Save()
	e.fanOut(id, e.others(tell, nil), groupWire{G: id, Op: updateOp(cur.Kind), Snap: snap})
	e.groupChanged(id)
	return nil
}

// RenameGroup gives a group a new name. Admin only.
func (e *Engine) RenameGroup(id, name string) error {
	if err := e.allowed(); err != nil {
		return err
	}
	name = strings.TrimSpace(name)
	if name == "" || len(name) > maxGroupName {
		return fmt.Errorf("give it a name of up to %d characters", maxGroupName)
	}
	cur, ok := e.State.GroupMeta(id)
	if !ok {
		return state.ErrNoGroup
	}
	admin := e.adminOf(cur)
	var snap *groupSnap
	err := e.State.UpdateGroup(id, false, func(g *state.Group) error {
		if g.Status != state.GroupJoined {
			return errNotInGroup
		}
		if admin != e.Username() {
			return errNotAdmin
		}
		if g.Name == name {
			return errors.New("that's already its name")
		}
		g.Admin = admin
		g.Name = name
		g.Version++
		snap = snapOf(g)
		return nil
	})
	if err != nil {
		return err
	}
	e.systemLine(id, "You renamed the "+kindWord(cur.Kind)+" to "+name)
	_ = e.State.Save()
	e.fanOut(id, e.others(e.roster(cur), nil), groupWire{G: id, Op: updateOp(cur.Kind), Snap: snap})
	e.groupChanged(id)
	return nil
}

// SendGroup writes to a group. The message is recorded and shown at once;
// copies go out to every member in the background, in order.
func (e *Engine) SendGroup(id, text string) (state.GroupMessage, error) {
	if err := e.allowed(); err != nil {
		return state.GroupMessage{}, err
	}
	text = strings.TrimSpace(text)
	if text == "" {
		return state.GroupMessage{}, errors.New("nothing to send")
	}
	if len(text) > mailbox.MaxTextLen-512 {
		return state.GroupMessage{}, errors.New("that message is too long")
	}
	g, ok := e.State.GroupMeta(id)
	if !ok || g.Status != state.GroupJoined {
		return state.GroupMessage{}, errNotInGroup
	}
	if e.groupBacklogged(id) {
		// Shown as not sent, with Retry, rather than looking sent and never
		// leaving.
		return state.GroupMessage{}, errGroupBacklog
	}
	m := state.GroupMessage{ID: newID(), From: e.Username(), Outbound: true, Text: text, Sent: time.Now().Unix()}
	e.State.AddGroupMessage(id, m)
	_ = e.State.Save()
	e.fanOut(id, e.others(e.roster(g), nil), groupWire{G: id, Op: gopMsg, MID: m.ID, Text: text})
	return m, nil
}

// others is members minus us and minus skip.
func (e *Engine) others(members, skip []string) []string {
	drop := map[string]bool{e.Username(): true}
	for _, s := range skip {
		drop[s] = true
	}
	var out []string
	for _, m := range members {
		if !drop[m] {
			out = append(out, m)
		}
	}
	return out
}

// settleReports marks the reports about someone as dealt with: they have been
// removed, or have left, so there is nothing left for the owner to do.
func settleReports(g *state.Group, who string) {
	for i := range g.Reports {
		if g.Reports[i].Who == who {
			g.Reports[i].Done = true
		}
	}
}

func without(list []string, x string) []string {
	out := make([]string, 0, len(list))
	for _, v := range list {
		if v != x {
			out = append(out, v)
		}
	}
	return out
}

// ─── sending to members ───

type groupJob struct {
	to   []string
	wire groupWire
}

// groupQueueSize is how many group messages may wait to go out per group.
const groupQueueSize = 1024

var errGroupBacklog = errors.New("too many messages are still waiting to go out in this group; try again once you're back online")

// groupBacklogged reports that a group's send queue is nearly full: a new
// message is refused there, leaving room for membership changes.
func (e *Engine) groupBacklogged(id string) bool {
	e.groupMu.Lock()
	defer e.groupMu.Unlock()
	q, ok := e.groupQueues[id]
	return ok && len(q) >= groupQueueSize-64
}

// fanOut queues one group message for every member in to. Each group has
// one worker, so a group's messages leave in the order they were written;
// within one message, every member is sent to at the same time.
func (e *Engine) fanOut(id string, to []string, w groupWire) {
	if len(to) == 0 {
		return
	}
	e.groupMu.Lock()
	q, ok := e.groupQueues[id]
	if !ok {
		q = make(chan groupJob, groupQueueSize)
		e.groupQueues[id] = q
		go e.groupWorker(q)
	}
	e.groupMu.Unlock()
	select {
	case q <- groupJob{to: to, wire: w}:
	default:
		// A thousand messages queued for one group means the network has
		// been gone for a long while; the oldest-first order matters more
		// than any one message, so drop rather than reorder. (New chat
		// messages are refused well before this; see groupBacklogged.)
	}
}

func (e *Engine) groupWorker(q chan groupJob) {
	for {
		select {
		case <-e.stop:
			return
		case job := <-q:
			blob, err := json.Marshal(job.wire)
			if err != nil {
				continue
			}
			var wg sync.WaitGroup
			for _, peerID := range job.to {
				if e.State.IsBlocked(peerID) || e.Mod.BannedPeer(peerID) {
					continue
				}
				wg.Add(1)
				go func(peerID string) {
					defer wg.Done()
					e.sendGroupCopy(peerID, string(blob))
				}(peerID)
			}
			wg.Wait()
		}
	}
}

// sendGroupCopy sends one member their copy, retrying a few times: a group
// message that silently misses one person is worse than a late one.
func (e *Engine) sendGroupCopy(peerID, payload string) {
	for attempt, wait := range []time.Duration{0, 2 * time.Second, 8 * time.Second, 30 * time.Second} {
		if attempt > 0 {
			select {
			case <-e.stop:
				return
			case <-time.After(wait):
			}
		}
		unlock := e.sendLock(peerID)
		conv := e.State.Conversation(peerID)
		_, _, err := e.Inbox.SendKind(conv, mailbox.KindGroup, payload)
		if err == nil {
			e.State.PutConversation(conv)
			_ = e.State.Save()
		}
		unlock()
		if err == nil {
			return
		}
	}
}

// ─── receiving ───

// handleGroup acts on one group message from conv's peer. The sender is
// already known not to be blocked, declined or banned (onMessages checks).
func (e *Engine) handleGroup(conv *mailbox.Conversation, m *mailbox.Message) {
	var w groupWire
	if json.Unmarshal([]byte(m.Text), &w) != nil || !validGroupID(w.G) {
		return
	}
	from := conv.PeerID
	switch w.Op {
	case gopInvite, gopWInvite, gopUpdate, gopWUpdate:
		// The operation and the snapshot must agree on what this is: a
		// workspace can't arrive dressed as a group chat, or the reverse.
		business := w.Op == gopWInvite || w.Op == gopWUpdate
		if w.Snap == nil || business != (w.Snap.Kind != state.KindPlain) {
			return
		}
		if w.Op == gopInvite || w.Op == gopWInvite {
			e.groupInvite(from, w)
		} else {
			e.groupUpdate(from, w)
		}
	case gopLeave:
		e.groupLeave(from, w)
	case gopMsg:
		e.groupMessage(from, w, m.Sent)
	case gopJoined:
		e.groupJoinedNote(from, w)
	case gopReport:
		e.groupReport(from, w)
	}
}

// checkSnap rejects a snapshot no honest admin sends.
func (e *Engine) checkSnap(s *groupSnap) bool {
	if s == nil || s.V < 1 || len(s.Members) < 1 {
		return false
	}
	switch s.Kind {
	case state.KindPlain, state.KindWorkspace:
		if s.WS != "" {
			return false
		}
	case state.KindChannel:
		if !validGroupID(s.WS) {
			return false
		}
	default:
		return false
	}
	if len(s.Members) > maxMembers(s.Kind) {
		return false
	}
	name := strings.TrimSpace(s.Name)
	if name == "" || len(name) > maxGroupName {
		return false
	}
	seen := map[string]bool{}
	for _, m := range s.Members {
		if !isUsername(m) || seen[m] {
			return false
		}
		seen[m] = true
	}
	return seen[s.Admin]
}

func isUsername(s string) bool {
	if len(s) != 64 {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil && strings.ToLower(s) == s
}

func (e *Engine) groupInvite(from string, w groupWire) {
	s := w.Snap
	me := e.Username()
	// Only the admin invites, and the invitation must actually include us.
	// (A channel's invitation comes from whoever made it; see channelInvite.)
	if !e.checkSnap(s) || !contains(s.Members, me) {
		return
	}
	if s.Kind == state.KindChannel {
		e.channelInvite(from, w)
		return
	}
	if s.Admin != from {
		return
	}
	known, ok := e.State.GroupMeta(w.G)
	if ok && known.Kind != s.Kind && e.earlyChannel(known) {
		// Something claimed this ID as a channel before its workspace was
		// known here. The real workspace wins.
		e.State.DeleteGroup(w.G)
		known, ok = state.Group{}, false
	}
	if ok && known.Kind != s.Kind {
		return // a group never changes what it is
	}
	if ok && known.Status == state.GroupJoined {
		e.groupUpdate(from, w) // already in: it's just news about members
		return
	}
	if ok && (s.V <= known.Version || from != e.adminOf(known)) {
		// An old invitation we've already answered -- or someone other than
		// the one who runs it, trying to take over a group we left.
		return
	}
	if !ok && !e.roomForInvite(from) {
		return
	}
	err := e.State.UpdateGroup(w.G, true, func(g *state.Group) error {
		g.Name, g.Admin, g.Members, g.Version = strings.TrimSpace(s.Name), s.Admin, s.Members, s.V
		g.Status, g.InvitedBy = state.GroupInvited, from
		g.Kind, g.WS = s.Kind, s.WS
		if s.Kind == state.KindWorkspace {
			g.Joined = mergeJoined(nil, s.J, s.Members, s.Admin)
		}
		return nil
	})
	if err != nil {
		return
	}
	go e.cacheHandle(from)
	_ = e.State.Save()
	e.groupChanged(w.G)
	if e.OnInvited != nil {
		e.OnInvited(w.G)
	}
	if s.Kind == state.KindWorkspace {
		e.checkEarlyChannels(w.G) // channels that were added before the workspace invitation came
	}
}

// Limits on invitations waiting from people who aren't contacts or coworkers.
// Anyone who knows an @name can invite, and identities cost nothing, so each
// sender gets a couple and all of them together a few dozen; the rest are
// dropped unseen.
const (
	maxInvitesPerStranger = 2
	maxStrangerInvites    = 20
)

// roomForInvite says whether a new invitation from from may be kept.
func (e *Engine) roomForInvite(from string) bool {
	if e.State.IsContact(from) || e.isCoworker(from) {
		return true
	}
	total, theirs := 0, 0
	for _, g := range e.State.GroupsMeta() {
		if g.Status != state.GroupInvited || g.Kind == state.KindChannel {
			continue
		}
		if e.State.IsContact(g.InvitedBy) || e.isCoworker(g.InvitedBy) {
			continue
		}
		total++
		if g.InvitedBy == from {
			theirs++
		}
	}
	return theirs < maxInvitesPerStranger && total < maxStrangerInvites
}

// mergeJoined combines what we knew about who accepted a workspace with an
// owner's snapshot, keeping only people still in it. The owner is always in.
func mergeJoined(have, snap, members []string, admin string) []string {
	var out []string
	add := func(m string) {
		if contains(members, m) && !contains(out, m) {
			out = append(out, m)
		}
	}
	add(admin)
	for _, m := range have {
		add(m)
	}
	for _, m := range snap {
		add(m)
	}
	return out
}

func (e *Engine) groupUpdate(from string, w groupWire) {
	s := w.Snap
	if !e.checkSnap(s) {
		return
	}
	me := e.Username()
	cur, known := e.State.GroupMeta(w.G)
	if !known || cur.Kind != s.Kind || cur.WS != s.WS {
		return
	}
	admin := e.adminOf(cur)
	var before []string
	var oldName string
	var applied, removed bool
	err := e.State.UpdateGroup(w.G, false, func(g *state.Group) error {
		if admin != from || s.V <= g.Version {
			return errors.New("not from the admin, or out of date")
		}
		before = append(before, g.Members...)
		oldName = g.Name
		g.Name, g.Admin, g.Members, g.Version = strings.TrimSpace(s.Name), s.Admin, s.Members, s.V
		if g.Kind == state.KindWorkspace {
			g.Joined = mergeJoined(g.Joined, s.J, s.Members, s.Admin)
		}
		if !contains(s.Members, me) && (g.Status == state.GroupJoined || g.Status == state.GroupInvited) {
			g.Status = state.GroupRemoved
			g.Unread = 0
			removed = true
		}
		applied = true
		return nil
	})
	if err != nil || !applied {
		return
	}
	g, _ := e.State.GroupMeta(w.G)
	adminName := e.Name(from)
	if newName := strings.TrimSpace(s.Name); newName != oldName && oldName != "" && !removed {
		e.systemLine(w.G, adminName+" renamed the "+kindWord(g.Kind)+" to "+newName)
	}
	if removed {
		e.systemLine(w.G, adminName+" removed you")
		if g.Kind == state.KindWorkspace {
			e.closeChannels(w.G, state.GroupRemoved, "You were removed from the workspace")
		}
		for _, m := range before {
			e.closeToFormerMember(m, true)
		}
	} else {
		for _, m := range s.Members {
			if !contains(before, m) && m != me {
				e.systemLine(w.G, adminName+" added "+e.Name(m))
				if g.Status == state.GroupJoined {
					e.openToMember(m, true)
				}
			}
		}
		var gone []string
		for _, m := range before {
			if !contains(s.Members, m) && m != me {
				e.systemLine(w.G, adminName+" removed "+e.Name(m))
				gone = append(gone, m)
			}
		}
		if g.Kind == state.KindWorkspace {
			e.purgeChannels(w.G, gone)
		}
		for _, m := range gone {
			e.closeToFormerMember(m, true)
		}
	}
	_ = e.State.Save()
	e.groupChanged(w.G)
}

func (e *Engine) groupLeave(from string, w groupWire) {
	cur, ok := e.State.GroupMeta(w.G)
	if !ok {
		return
	}
	wasAdmin := e.adminOf(cur) == from
	left, declined := false, false
	var kind string
	err := e.State.UpdateGroup(w.G, false, func(g *state.Group) error {
		if !g.HasMember(from) {
			return errors.New("not a member")
		}
		// A workspace keeps track of who accepted, so it can tell "said no"
		// from "left".
		declined = g.Kind == state.KindWorkspace && !contains(g.Joined, from)
		kind = g.Kind
		g.Joined = without(g.Joined, from)
		g.Members = without(g.Members, from)
		settleReports(g, from)
		if wasAdmin && len(g.Members) > 0 {
			// The one leaving names who runs it next, so every device agrees.
			// From an older version, or a name that isn't in it any more:
			// the next in the list, which every device also agrees on.
			if w.Next != "" && contains(g.Members, w.Next) {
				g.Admin = w.Next
			} else {
				g.Admin = g.Members[0]
			}
		}
		left = true
		return nil
	})
	if err != nil || !left {
		return
	}
	if kind == state.KindWorkspace {
		e.purgeChannels(w.G, []string{from})
	}
	if declined {
		e.systemLine(w.G, e.Name(from)+" declined the invitation")
	} else {
		e.systemLine(w.G, e.Name(from)+" left")
	}
	e.closeToFormerMember(from, true)
	_ = e.State.Save()
	e.groupChanged(w.G)
}

func (e *Engine) groupMessage(from string, w groupWire, sent int64) {
	if w.MID == "" || len(w.MID) > 64 || strings.TrimSpace(w.Text) == "" || len(w.Text) > mailbox.MaxTextLen {
		return
	}
	g, ok := e.State.GroupMeta(w.G)
	if !ok || !contains(e.roster(g), from) || (g.Status != state.GroupJoined && g.Status != state.GroupInvited) {
		return
	}
	if g.Kind == state.KindWorkspace && !contains(g.Joined, from) {
		// Only someone who accepted can write in it: they have joined, even
		// if their note saying so hasn't arrived yet.
		e.noteJoined(w.G, from, false)
	}
	msg := state.GroupMessage{ID: w.MID, From: from, Text: w.Text, Sent: sent}
	if !e.State.AddGroupMessage(w.G, msg) {
		return
	}
	_ = e.State.Save()
	if g.Status == state.GroupJoined {
		e.acknowledgeGroup(w.G, from, w.MID)
		if e.OnGroupMessage != nil {
			e.OnGroupMessage(w.G, msg)
		}
	}
}

func contains(list []string, x string) bool {
	for _, v := range list {
		if v == x {
			return true
		}
	}
	return false
}

// systemLine adds a note about the group itself to its history.
func (e *Engine) systemLine(id, text string) {
	e.State.AddGroupMessage(id, state.GroupMessage{ID: newID(), Text: text, Sent: time.Now().Unix(), System: true})
}

func (e *Engine) groupChanged(id string) {
	if e.OnGroup != nil {
		e.OnGroup(id)
	}
}

// ─── reading ───

// Groups lists the groups to show: joined ones, invitations, and ones we've
// left or been removed from (their history stays readable). Declined
// invitations are hidden.
func (e *Engine) Groups() []GroupSummary {
	me := e.Username()
	var out []GroupSummary
	for _, g := range e.State.GroupsMeta() {
		if g.Status == state.GroupDeclined {
			continue
		}
		// A channel we were added to before saying yes to its workspace stays
		// out of sight until we do.
		if g.Kind == state.KindChannel && g.Status == state.GroupInvited {
			continue
		}
		members, pending := e.groupSize(g)
		s := GroupSummary{
			ID: g.ID, Name: g.Name, Kind: g.Kind, WS: g.WS, Members: members, Pending: pending, Status: g.Status,
			Admin: e.adminOf(g) == me, Unread: g.Unread,
		}
		if s.Admin && g.Kind == state.KindWorkspace {
			for _, r := range g.Reports {
				if !r.Done {
					s.Reports++
				}
			}
		}
		if g.Status == state.GroupInvited {
			s.InvitedBy, s.InvitedByID = e.Name(g.InvitedBy), g.InvitedBy
		}
		if last, ok := e.State.LastGroupMessage(g.ID); ok {
			s.Preview, s.LastOutbound, s.LastAt = last.Text, last.Outbound, last.Sent
			if !last.Outbound && !last.System && last.From != "" {
				s.Preview = e.Name(last.From) + ": " + last.Text
			}
		}
		out = append(out, s)
	}
	return out
}

// GroupThread returns a group's messages, in order.
func (e *Engine) GroupThread(id string) []state.GroupMessage { return e.State.GroupThread(id) }

// GroupMembers lists the people in a group.
func (e *Engine) GroupMembers(id string) ([]GroupMember, error) {
	g, ok := e.State.GroupMeta(id)
	if !ok {
		return nil, state.ErrNoGroup
	}
	me := e.Username()
	roster, joined := e.channelRoster(g)
	if g.Kind == state.KindWorkspace {
		joined = g.Joined
	}
	admin := e.adminOf(g)
	out := make([]GroupMember, 0, len(roster))
	for _, m := range roster {
		name := e.Name(m)
		if m == me {
			name = "You"
		}
		pending := joined != nil && m != me && !contains(joined, m)
		out = append(out, GroupMember{PeerID: m, Name: name, Admin: m == admin, Me: m == me, Pending: pending})
	}
	return out, nil
}

// GroupSize is how many people are in a group, and how many of those are
// invited to its workspace and haven't answered yet (always 0 for a plain
// group, which doesn't keep track). ok is false for an unknown group.
func (e *Engine) GroupSize(id string) (members, pending int, ok bool) {
	g, found := e.State.GroupMeta(id)
	if !found {
		return 0, 0, false
	}
	members, pending = e.groupSize(g)
	return members, pending, true
}

func (e *Engine) groupSize(g state.Group) (members, pending int) {
	roster, joined := e.channelRoster(g)
	if g.Kind == state.KindWorkspace {
		joined = g.Joined
	}
	me := e.Username()
	for _, m := range roster {
		if joined != nil && m != me && !contains(joined, m) {
			pending++
		}
	}
	return len(roster), pending
}

// MarkGroupRead clears a group's unread count.
func (e *Engine) MarkGroupRead(id string) error {
	e.State.MarkGroupRead(id)
	return e.State.Save()
}
