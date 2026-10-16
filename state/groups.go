package state

import (
	"errors"
	"sort"
	"time"
)

// Group status, from this user's point of view.
const (
	GroupInvited  = "invited"  // someone invited us; not answered yet
	GroupJoined   = "joined"   // we're in it
	GroupDeclined = "declined" // we said no to the invitation
	GroupLeft     = "left"     // we left
	GroupRemoved  = "removed"  // the admin took us out
)

// What a group is for. A workspace is a company's room: its own chat is the
// "general" channel, and its member list is the company's roster. A channel
// is a thread inside a workspace for some of its people. An ordinary group
// has neither.
const (
	KindPlain     = ""
	KindWorkspace = "workspace"
	KindChannel   = "channel"
)

// ReportLine is one message attached to a report to a workspace's owner.
type ReportLine struct {
	Text  string `json:"text"`
	Sent  int64  `json:"sent"`
	Where string `json:"where,omitempty"` // "direct" or "general"
}

// WorkspaceReport is one member's report about another, as the workspace's
// owner receives it. It is kept only on the owner's computer, for them to
// read and act on; nothing goes to AltNet.
type WorkspaceReport struct {
	ID       string       `json:"id"`
	From     string       `json:"from"` // who reported
	Who      string       `json:"who"`  // who was reported
	Reason   string       `json:"reason"`
	Evidence []ReportLine `json:"evidence,omitempty"`
	At       int64        `json:"at"`
	Done     bool         `json:"done,omitempty"` // removed or dismissed
}

// Group is one group chat as this device knows it.
//
// Membership is decided by the admin (whoever created the group, until they
// leave): their signed updates are the only thing that adds or removes
// people. Members is everyone, admin included, in a fixed order -- the order
// also decides who becomes admin if the admin leaves.
type Group struct {
	ID        string         `json:"id"`
	Name      string         `json:"name"`
	Admin     string         `json:"admin"`
	Members   []string       `json:"members"`
	Version   int64          `json:"version"`
	Status    string         `json:"status"`
	InvitedBy string         `json:"invited_by,omitempty"`
	History   []GroupMessage `json:"history,omitempty"`
	Unread    int            `json:"unread,omitempty"`

	// Kind is KindWorkspace or KindChannel for business groups. A channel's
	// WS is the ID of the workspace it belongs to.
	Kind string `json:"kind,omitempty"`
	WS   string `json:"ws,omitempty"`

	// Joined, for a workspace, is who is known to have accepted its
	// invitation: the creator, and everyone whose "joined" note or owner's
	// snapshot has reached this device. Members who aren't in it are
	// invited and haven't answered.
	Joined []string `json:"joined,omitempty"`

	// Reports are reports to this workspace's owner (kept by the owner only).
	Reports []WorkspaceReport `json:"reports,omitempty"`
}

// GroupMessage is one line in a group: someone's message, or a note about
// the group itself ("rob added kate").
type GroupMessage struct {
	ID       string `json:"id"`
	From     string `json:"from"`
	Outbound bool   `json:"outbound"`
	Text     string `json:"text"`
	Sent     int64  `json:"sent"`
	Logged   int64  `json:"logged"`
	System   bool   `json:"system,omitempty"`

	// DeliveredTo: for a message we sent, the members whose devices said
	// it arrived.
	DeliveredTo []string `json:"delivered_to,omitempty"`
}

// HasMember reports whether username is in the group.
func (g *Group) HasMember(username string) bool {
	for _, m := range g.Members {
		if m == username {
			return true
		}
	}
	return false
}

// ErrNoGroup is returned for a group this device doesn't know.
var ErrNoGroup = errors.New("no such group")

// copyGroup returns a copy that shares nothing with state. Without history,
// History is left nil: membership questions are asked on every refresh and
// every message, and copying thousands of lines each time to answer them
// would be most of the work.
func copyGroup(g *Group, history bool) Group {
	c := *g
	c.Members = append([]string(nil), g.Members...)
	c.Joined = append([]string(nil), g.Joined...)
	c.Reports = append([]WorkspaceReport(nil), g.Reports...)
	c.History = nil
	if history {
		c.History = append([]GroupMessage(nil), g.History...)
	}
	return c
}

// Group returns a copy of one group, history included.
func (s *State) Group(id string) (Group, bool) {
	return s.group(id, true)
}

// GroupMeta returns a copy of one group without its history.
func (s *State) GroupMeta(id string) (Group, bool) {
	return s.group(id, false)
}

func (s *State) group(id string, history bool) (Group, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	g, ok := s.d.Groups[id]
	if !ok {
		return Group{}, false
	}
	return copyGroup(g, history), true
}

// Groups returns copies of every group, history included.
func (s *State) Groups() []Group { return s.groups(true) }

// GroupsMeta returns copies of every group without their history.
func (s *State) GroupsMeta() []Group { return s.groups(false) }

func (s *State) groups(history bool) []Group {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Group, 0, len(s.d.Groups))
	for _, g := range s.d.Groups {
		out = append(out, copyGroup(g, history))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// DeleteGroup forgets a group entirely.
func (s *State) DeleteGroup(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.d.Groups, id)
}

// LastGroupMessage returns a group's newest line, by the same order
// GroupThread uses, without copying its history.
func (s *State) LastGroupMessage(id string) (GroupMessage, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	g, ok := s.d.Groups[id]
	if !ok || len(g.History) == 0 {
		return GroupMessage{}, false
	}
	best := 0
	for i := range g.History {
		// ">=" keeps the later of two equal keys, matching the stable sort.
		if threadKey(g.History[i]) >= threadKey(g.History[best]) {
			best = i
		}
	}
	return g.History[best], true
}

// threadKey orders a group's lines: the sender's time, clamped to when we
// logged it, so a sender's wrong clock can't push a line to the bottom.
func threadKey(m GroupMessage) int64 {
	if m.Logged > 0 && m.Sent > m.Logged {
		return m.Logged
	}
	return m.Sent
}

// UpdateGroup changes one group under the state lock. With create set, a
// missing group is created (empty) for fn to fill in. fn must not call back
// into State.
func (s *State) UpdateGroup(id string, create bool, fn func(g *Group) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	g, ok := s.d.Groups[id]
	if !ok {
		if !create {
			return ErrNoGroup
		}
		g = &Group{ID: id}
	}
	if err := fn(g); err != nil {
		return err
	}
	s.d.Groups[id] = g
	return nil
}

// AddGroupMessage appends a message unless one with its ID is already there,
// and reports whether it was added. An inbound message in a joined group
// counts as unread.
func (s *State) AddGroupMessage(id string, m GroupMessage) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	g, ok := s.d.Groups[id]
	if !ok {
		return false
	}
	for _, x := range g.History {
		if x.ID == m.ID {
			return false
		}
	}
	if m.Logged == 0 {
		m.Logged = time.Now().Unix()
	}
	g.History = append(g.History, m)
	if !m.Outbound && !m.System && g.Status == GroupJoined {
		g.Unread++
	}
	return true
}

// MarkGroupDelivered records that member's device received these messages
// of ours, and returns the IDs that changed.
func (s *State) MarkGroupDelivered(id, member string, mids []string) []string {
	want := make(map[string]bool, len(mids))
	for _, m := range mids {
		want[m] = true
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	g, ok := s.d.Groups[id]
	if !ok {
		return nil
	}
	var changed []string
	for i := range g.History {
		m := &g.History[i]
		if !m.Outbound || !want[m.ID] {
			continue
		}
		already := false
		for _, who := range m.DeliveredTo {
			if who == member {
				already = true
				break
			}
		}
		if !already {
			m.DeliveredTo = append(m.DeliveredTo, member)
			changed = append(changed, m.ID)
		}
	}
	return changed
}

// GroupThread returns a group's messages in order, ordered the same way as a
// one-to-one thread (sender's time, clamped to when we logged it).
func (s *State) GroupThread(id string) []GroupMessage {
	s.mu.Lock()
	g, ok := s.d.Groups[id]
	var msgs []GroupMessage
	if ok {
		msgs = append(msgs, g.History...)
	}
	s.mu.Unlock()
	sort.SliceStable(msgs, func(i, j int) bool { return threadKey(msgs[i]) < threadKey(msgs[j]) })
	return msgs
}

// MarkGroupRead clears a group's unread count.
func (s *State) MarkGroupRead(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if g, ok := s.d.Groups[id]; ok {
		g.Unread = 0
	}
}

// SetGroupOnly marks (or clears) a peer we only talk to through groups. Their
// conversation is open at the chain level so group messages aren't capped
// like a stranger's, but it isn't a chat of its own: it stays out of the
// list, and a direct message from them still shows as a request.
func (s *State) SetGroupOnly(peerID string, on bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if on {
		s.d.GroupOnly[peerID] = true
	} else {
		delete(s.d.GroupOnly, peerID)
	}
}

// IsGroupOnly reports whether a peer is known only through groups.
func (s *State) IsGroupOnly(peerID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.d.GroupOnly[peerID]
}

// SetWorkChat marks a direct chat as one that began between coworkers. It
// stays a Business chat after the two stop sharing a workspace, closed to
// new messages there, rather than turning into a Personal message request.
func (s *State) SetWorkChat(peerID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.d.WorkChat[peerID] = true
}

// IsWorkChat reports whether a direct chat began between coworkers.
func (s *State) IsWorkChat(peerID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.d.WorkChat[peerID]
}
