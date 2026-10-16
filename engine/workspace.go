package engine

import (
	"errors"
	"strings"

	"altnet-relay/state"
)

// Workspaces: the Business side of the same account.
//
// A workspace is a company's room, built from the same parts as a group
// chat. Its own chat is the "general" channel; its member list is the
// company's roster; the person who creates it owns it. Everything people
// already trust about groups holds for it unchanged: every message is sealed
// separately for each member, nobody is added without accepting, and the
// owner's numbered snapshots are the only thing that adds or removes people.
//
// On top of that:
//
//   - Every device keeps track of who has accepted (Joined): the owner's
//     snapshots carry the list, and each person who joins tells everyone.
//     Someone who is only invited is not a coworker yet.
//   - Channels are threads inside a workspace for some of its people, each
//     run by whoever made it. They are made of people already in the
//     workspace. Anyone who leaves or is removed from the workspace is taken
//     out of all its channels on every device (purgeChannels), so being
//     invited back later doesn't quietly reopen them. Being added to a
//     channel needs no second yes: you accepted the company.
//   - Coworkers can message each other directly without it being a message
//     request, and call each other without having written first. Those chats
//     belong to Business: they stay out of the Personal list, and stay in
//     Business (closed) after the two stop being coworkers.
//   - Calls in a channel are group calls, like any other.
//
// Workspaces travel under their own wire operations (winvite, wupdate), so
// Relay 0.2 never sees one rather than mistaking it for a group chat.
//
// Nothing here talks to a server. The limits are the group ones: each message
// is sent once per member, so a workspace is capped at MaxWorkspaceMembers.

// CreateWorkspace starts a workspace, optionally inviting people (names or
// IDs). The creator owns it.
func (e *Engine) CreateWorkspace(name string, who []string) (string, error) {
	return e.create(state.KindWorkspace, "", name, who)
}

// CreateChannel starts a channel in a workspace we're in, with the given
// workspace members. The creator runs it.
func (e *Engine) CreateChannel(workspaceID, name string, who []string) (string, error) {
	if !validGroupID(workspaceID) {
		return "", errors.New("you're not in that workspace")
	}
	return e.create(state.KindChannel, workspaceID, name, who)
}

// isCoworker reports whether peerID has accepted a workspace we're in.
// Someone who is only invited doesn't count: the owner can invite anyone, and
// that mustn't make a stranger callable by the whole company.
func (e *Engine) isCoworker(peerID string) bool {
	for _, g := range e.State.GroupsMeta() {
		if g.Kind == state.KindWorkspace && g.Status == state.GroupJoined && contains(g.Joined, peerID) {
			return true
		}
	}
	return false
}

// workPeer reports whether direct messages with peerID belong to Business:
// they're a coworker now, or our chat began while they were one. A personal
// contact is never one -- their chat lives in Personal.
func (e *Engine) workPeer(peerID string) bool {
	if e.State.IsContact(peerID) {
		return false
	}
	return (e.State.IsGroupOnly(peerID) && e.isCoworker(peerID)) || e.State.IsWorkChat(peerID)
}

// ─── channel invitations ───

// Limits on channel invitations held for a workspace we haven't heard of yet:
// anyone can send one, so they must not be able to fill this device up.
const (
	maxEarlyChannels          = 32
	maxEarlyChannelsPerSender = 4
)

// earlyChannel reports whether g is a channel invitation held for a
// workspace this device doesn't know yet.
func (e *Engine) earlyChannel(g state.Group) bool {
	if g.Kind != state.KindChannel || g.Status != state.GroupInvited {
		return false
	}
	_, known := e.State.GroupMeta(g.WS)
	return !known
}

// roomForEarlyChannel says whether one more early invitation from from may
// be kept.
func (e *Engine) roomForEarlyChannel(from string) bool {
	total, theirs := 0, 0
	for _, g := range e.State.GroupsMeta() {
		if !e.earlyChannel(g) {
			continue
		}
		total++
		if g.InvitedBy == from {
			theirs++
		}
	}
	return total < maxEarlyChannels && theirs < maxEarlyChannelsPerSender
}

// checkEarlyChannels runs when a workspace invitation arrives: channel
// invitations that came first (they're from different people and can pass
// each other) are checked against it now, and dropped if they don't fit.
func (e *Engine) checkEarlyChannels(wsID string) {
	ws, ok := e.State.GroupMeta(wsID)
	if !ok {
		return
	}
	me := e.Username()
	for _, g := range e.State.GroupsMeta() {
		if g.Kind != state.KindChannel || g.WS != wsID || g.Status != state.GroupInvited {
			continue
		}
		if g.Admin != g.InvitedBy || !contains(ws.Members, g.InvitedBy) || !contains(ws.Members, me) {
			e.State.DeleteGroup(g.ID)
		}
	}
	_ = e.State.Save()
}

// channelInvite handles being added to a channel. It runs inside the inbox's
// receiving side, so it only uses the lock-free forms of openToMember.
func (e *Engine) channelInvite(from string, w groupWire) {
	s := w.Snap
	me := e.Username()
	// Whoever adds us must run the channel and be in it.
	if s.Admin != from || !contains(s.Members, from) {
		return
	}
	known, exists := e.State.GroupMeta(w.G)
	if exists {
		if known.Kind != state.KindChannel || known.WS != s.WS {
			return
		}
		if known.Status == state.GroupJoined {
			e.groupUpdate(from, w) // already in: just news about its members
			return
		}
		if s.V <= known.Version || from != e.adminOf(known) {
			// An old invitation -- or someone other than whoever runs it,
			// trying to take over a channel we left.
			return
		}
	}
	ws, wsKnown := e.State.GroupMeta(s.WS)
	if wsKnown {
		if ws.Kind != state.KindWorkspace {
			return
		}
		if ws.Status != state.GroupJoined && ws.Status != state.GroupInvited {
			return // we said no to that workspace, or left it
		}
		// A channel can't reach into a company we aren't part of.
		if !contains(ws.Members, from) || !contains(ws.Members, me) {
			return
		}
	} else if !exists && !e.roomForEarlyChannel(from) {
		return
	}
	// Opened straight away only when we're in its workspace. Otherwise it
	// waits, out of sight: on disk, until the workspace invitation arrives
	// (checkEarlyChannels) and is accepted (promoteChannels).
	joinNow := wsKnown && ws.Status == state.GroupJoined
	status := state.GroupInvited
	if joinNow {
		status = state.GroupJoined
	}
	err := e.State.UpdateGroup(w.G, true, func(g *state.Group) error {
		g.Name, g.Admin, g.Members, g.Version = strings.TrimSpace(s.Name), s.Admin, s.Members, s.V
		g.Status, g.InvitedBy = status, from
		g.Kind, g.WS = state.KindChannel, s.WS
		return nil
	})
	if err != nil {
		return
	}
	go e.cacheHandle(from)
	if joinNow {
		e.channelOpened(w.G, from, s.Members, true)
	}
	_ = e.State.Save()
	if wsKnown {
		e.groupChanged(w.G)
	}
	if joinNow && e.OnAdded != nil {
		e.OnAdded(w.G)
	}
}

// channelOpened is what joining a channel does: its members' messages come
// through at full speed, the channel says who added us, and it shows as
// unread so a channel nobody has written in yet is still noticed. inPoll as
// for openToMember.
func (e *Engine) channelOpened(id, by string, members []string, inPoll bool) {
	for _, m := range members {
		e.openToMember(m, inPoll)
	}
	e.systemLine(id, e.Name(by)+" added you")
	_ = e.State.UpdateGroup(id, false, func(g *state.Group) error {
		if g.Unread == 0 {
			g.Unread = 1
		}
		return nil
	})
}

// promoteChannels opens the channels we were added to while the invitation to
// their workspace was still waiting -- unless whoever added us has since left
// the workspace.
func (e *Engine) promoteChannels(wsID string) {
	ws, ok := e.State.GroupMeta(wsID)
	if !ok {
		return
	}
	for _, g := range e.State.GroupsMeta() {
		if g.Kind != state.KindChannel || g.WS != wsID || g.Status != state.GroupInvited {
			continue
		}
		if !contains(ws.Members, g.InvitedBy) {
			e.State.DeleteGroup(g.ID)
			continue
		}
		err := e.State.UpdateGroup(g.ID, false, func(c *state.Group) error {
			if c.Status != state.GroupInvited {
				return errors.New("already answered")
			}
			c.Status = state.GroupJoined
			return nil
		})
		if err != nil {
			continue
		}
		e.channelOpened(g.ID, g.InvitedBy, g.Members, false)
		e.groupChanged(g.ID)
		if e.OnAdded != nil {
			e.OnAdded(g.ID)
		}
	}
}

// closeChannels shuts a workspace's channels on this device when we are no
// longer in it: left, removed, or declined. What was said stays readable.
func (e *Engine) closeChannels(wsID, status, line string) {
	for _, g := range e.State.GroupsMeta() {
		if g.Kind != state.KindChannel || g.WS != wsID {
			continue
		}
		if g.Status != state.GroupJoined && g.Status != state.GroupInvited {
			continue
		}
		wasIn := g.Status == state.GroupJoined
		err := e.State.UpdateGroup(g.ID, false, func(c *state.Group) error {
			c.Status = status
			c.Unread = 0
			return nil
		})
		if err != nil {
			continue
		}
		if wasIn && line != "" {
			e.systemLine(g.ID, line)
		}
		e.groupChanged(g.ID)
	}
}

// purgeChannels takes people who are no longer in a workspace out of every
// one of its channels on this device. Each device does it on the same news
// (the owner's snapshot, or the person's own leave), so they agree; nothing
// is sent. Whoever ran a channel and is gone hands it to its next member,
// chosen as successor does.
func (e *Engine) purgeChannels(wsID string, gone []string) {
	if len(gone) == 0 {
		return
	}
	ws, _ := e.State.GroupMeta(wsID)
	joined := ws.Joined
	for _, g := range e.State.GroupsMeta() {
		if g.Kind != state.KindChannel || g.WS != wsID {
			continue
		}
		hit := false
		for _, m := range gone {
			if g.HasMember(m) {
				hit = true
			}
		}
		if !hit {
			continue
		}
		_ = e.State.UpdateGroup(g.ID, false, func(c *state.Group) error {
			for _, m := range gone {
				c.Members = without(c.Members, m)
			}
			if !contains(c.Members, c.Admin) {
				if next := successor(c.Members, joined, ""); next != "" {
					c.Admin = next
				}
			}
			return nil
		})
		e.groupChanged(g.ID)
	}
}

// groupJoinedNote records that someone accepted a workspace's invitation.
func (e *Engine) groupJoinedNote(from string, w groupWire) {
	e.noteJoined(w.G, from, true)
}

// noteJoined adds who to a workspace's accepted members. line says whether
// to put "X joined" in its history.
func (e *Engine) noteJoined(id, who string, line bool) {
	changed := false
	err := e.State.UpdateGroup(id, false, func(g *state.Group) error {
		if g.Kind != state.KindWorkspace || !g.HasMember(who) || contains(g.Joined, who) {
			return errors.New("nothing to record")
		}
		if g.Status != state.GroupJoined && g.Status != state.GroupInvited {
			return errors.New("not ours any more")
		}
		g.Joined = append(g.Joined, who)
		changed = true
		return nil
	})
	if err != nil || !changed {
		return
	}
	if line {
		e.systemLine(id, e.Name(who)+" joined")
	}
	_ = e.State.Save()
	e.groupChanged(id)
}
