// Package state persists everything a Relay client must remember
// between runs: who you've added, which requests you've accepted, who
// you've blocked, how far you've read in each conversation, and the
// message history itself.
//
// Without this a client forgets its contacts on every restart and
// re-downloads (or worse, re-shows) conversations it has already seen.
// The read cursors in mailbox.Conversation are what make catch-up
// return only what's new, so losing them is not cosmetic.
//
// Storage is a single JSON file written atomically. Chat histories for
// one person are small, and matching the plain-file style the rest of
// AltNet uses keeps a Relay profile directory inspectable and easy to
// back up. A database can replace this later without changing callers.
package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"altnet-relay/mailbox"
)

// FileName is the state file inside a Relay profile directory.
const FileName = "state.json"

// StoredMessage is one message as kept in local history.
//
// Sent is the sender's clock; Logged is ours. Both are kept because
// they answer different questions and neither is trustworthy alone --
// see Thread for how they're combined.
type StoredMessage struct {
	ID       string `json:"id"`
	Outbound bool   `json:"outbound"` // true = we sent it
	Text     string `json:"text"`
	Sent     int64  `json:"sent"`   // unix seconds, sender's clock
	Logged   int64  `json:"logged"` // unix seconds, our clock, when recorded

	// Delivered: for a message we sent, their device said it arrived.
	Delivered bool `json:"delivered,omitempty"`
}

// Contact is a peer we've added.
//
// Handle is the name they claimed on the network (empty if they haven't
// claimed one, or it didn't verify). Nickname is what WE call them
// locally, which defaults to their handle but can be anything; it never
// leaves this device. Both exist because a handle is global and can be
// absent, while a nickname is personal and always present.
type Contact struct {
	Nickname string `json:"nickname"`
	Handle   string `json:"handle,omitempty"`
	PeerID   string `json:"peer_id"`
}

type data struct {
	MyHandle      string                           `json:"my_handle,omitempty"` // handle we've claimed, "" if none
	Contacts      map[string]*Contact              `json:"contacts"`            // peerID -> contact
	Conversations map[string]*mailbox.Conversation `json:"conversations"`       // peerID -> conversation
	History       map[string][]StoredMessage       `json:"history"`             // peerID -> messages

	// Handles caches verified handles for every peer we know of,
	// including message-request senders who aren't contacts. Filled only
	// from forward-verified lookups (Profile.VerifiedHandle), never from
	// what a peer claims about itself. Persisting it keeps the UI from
	// doing a network lookup just to draw a name.
	Handles map[string]string `json:"handles,omitempty"` // peerID -> handle

	// Declined remembers requests the user turned down. Without it, the
	// sender's knock is still sitting on the doorbell and would resurface
	// as a brand-new request on the next restart.
	Declined map[string]bool `json:"declined,omitempty"` // peerID -> true

	// Blocked is the user's block list. Unlike Declined it applies to
	// anyone, contacts included, and keeps the conversation and its
	// history: blocking is "stop hearing from them", not "forget them".
	// Purely local -- nothing on the network records it, and the blocked
	// person has no way to find out.
	Blocked map[string]bool `json:"blocked,omitempty"` // peerID -> true

	// Unread counts inbound messages the user hasn't opened yet. Kept on
	// disk rather than in the UI so a restart doesn't silently mark
	// everything read -- the messages that arrived while the app was
	// closed are exactly the ones worth pointing at.
	Unread map[string]int `json:"unread,omitempty"` // peerID -> count

	// Dropped holds the IDs of messages a blocked peer sent that were
	// deliberately never shown. Their mailbox chain still holds them, and
	// several things legitimately re-walk it from the start -- accepting
	// a request rewinds the read cursor to reveal what the pending cap
	// held back -- so without remembering these, a later rewind would
	// surface exactly the messages the block was meant to keep out.
	Dropped map[string][]string `json:"dropped,omitempty"` // peerID -> message IDs

	// Groups are the group chats this device knows of; see groups.go.
	Groups map[string]*Group `json:"groups,omitempty"` // group ID -> group

	// GroupOnly marks peers we only know through a group; see SetGroupOnly.
	GroupOnly map[string]bool `json:"group_only,omitempty"` // peerID -> true

	// WorkChat marks direct chats that began between coworkers; see
	// SetWorkChat.
	WorkChat map[string]bool `json:"work_chat,omitempty"` // peerID -> true
}

// State is the on-disk client state. Safe for concurrent use: message
// arrival happens on background goroutines while the UI reads.
type State struct {
	path string

	mu   sync.Mutex
	d    data
	seen map[string]bool // message IDs already in history (or dropped), across all peers

	// saveMu serializes whole saves. Without it, two saves at once --
	// a message arriving while the user sends one, say -- both write the
	// same .tmp file and then race to rename it, so one can clobber the
	// other's contents or fail outright.
	saveMu sync.Mutex
}

// Load opens the state file in dir, creating empty state if absent.
func Load(dir string) (*State, error) {
	s := &State{
		path: filepath.Join(dir, FileName),
		d: data{
			Contacts:      make(map[string]*Contact),
			Conversations: make(map[string]*mailbox.Conversation),
			History:       make(map[string][]StoredMessage),
			Handles:       make(map[string]string),
			Declined:      make(map[string]bool),
			Blocked:       make(map[string]bool),
			Unread:        make(map[string]int),
			Dropped:       make(map[string][]string),
			Groups:        make(map[string]*Group),
			GroupOnly:     make(map[string]bool),
			WorkChat:      make(map[string]bool),
		},
		seen: make(map[string]bool),
	}
	raw, err := os.ReadFile(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			return s, nil
		}
		return nil, fmt.Errorf("state: read: %w", err)
	}
	if err := json.Unmarshal(raw, &s.d); err != nil {
		return nil, fmt.Errorf("state: parse %s: %w", s.path, err)
	}
	if s.d.Contacts == nil {
		s.d.Contacts = make(map[string]*Contact)
	}
	if s.d.Conversations == nil {
		s.d.Conversations = make(map[string]*mailbox.Conversation)
	}
	if s.d.History == nil {
		s.d.History = make(map[string][]StoredMessage)
	}
	if s.d.Handles == nil {
		s.d.Handles = make(map[string]string)
	}
	if s.d.Declined == nil {
		s.d.Declined = make(map[string]bool)
	}
	if s.d.Blocked == nil {
		s.d.Blocked = make(map[string]bool)
	}
	if s.d.Unread == nil {
		s.d.Unread = make(map[string]int)
	}
	if s.d.Dropped == nil {
		s.d.Dropped = make(map[string][]string)
	}
	if s.d.Groups == nil {
		s.d.Groups = make(map[string]*Group)
	}
	if s.d.GroupOnly == nil {
		s.d.GroupOnly = make(map[string]bool)
	}
	if s.d.WorkChat == nil {
		s.d.WorkChat = make(map[string]bool)
	}
	// Rebuild the dedup index so a message already in history is never
	// appended twice after a restart. Dropped messages go in too: to
	// RecordNew, "already dealt with" is the same whether it was shown or
	// deliberately discarded.
	for _, msgs := range s.d.History {
		for _, m := range msgs {
			s.seen[m.ID] = true
		}
	}
	for _, ids := range s.d.Dropped {
		for _, id := range ids {
			s.seen[id] = true
		}
	}
	return s, nil
}

// Save writes state to disk atomically, so an interrupted write can't
// leave a truncated file where the contact list used to be.
func (s *State) Save() error {
	s.saveMu.Lock()
	defer s.saveMu.Unlock()

	// Compact, not indented: the file is rewritten on every message, and
	// indenting made it (and the account's locked copy) a third bigger.
	s.mu.Lock()
	blob, err := json.Marshal(&s.d)
	s.mu.Unlock()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, blob, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

// Snapshot is what Save writes, without touching the file: the account's copy
// is made from it (reading the file while Save replaces it fails on Windows).
func (s *State) Snapshot() ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return json.Marshal(&s.d)
}

// Conversation returns the stored conversation for peerID, creating an
// empty pending one if this is the first time we've seen them.
func (s *State) Conversation(peerID string) *mailbox.Conversation {
	s.mu.Lock()
	defer s.mu.Unlock()
	if c, ok := s.d.Conversations[peerID]; ok {
		return c
	}
	c := &mailbox.Conversation{PeerID: peerID}
	s.d.Conversations[peerID] = c
	return c
}

// LookupConversation returns the stored conversation for peerID without
// creating one. Blocking someone who has never written shouldn't leave
// an empty conversation behind in the list.
func (s *State) LookupConversation(peerID string) (*mailbox.Conversation, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.d.Conversations[peerID]
	return c, ok
}

// PutConversation stores (or replaces) a conversation.
func (s *State) PutConversation(c *mailbox.Conversation) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.d.Conversations[c.PeerID] = c
}

// Conversations returns every known conversation.
func (s *State) Conversations() []*mailbox.Conversation {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*mailbox.Conversation, 0, len(s.d.Conversations))
	for _, c := range s.d.Conversations {
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].PeerID < out[j].PeerID })
	return out
}

// MyHandle returns the handle this user has claimed, or "" if none.
func (s *State) MyHandle() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.d.MyHandle
}

// SetMyHandle records a successful handle claim.
func (s *State) SetMyHandle(h string) {
	s.mu.Lock()
	s.d.MyHandle = h
	s.mu.Unlock()
}

// SetContactHandle records the verified handle a peer goes by, whether
// or not they are a contact yet. Callers must only pass handles that were
// verified by a forward lookup.
func (s *State) SetContactHandle(peerID, handle string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if handle == "" {
		delete(s.d.Handles, peerID)
	} else {
		s.d.Handles[peerID] = handle
	}
	if c, ok := s.d.Contacts[peerID]; ok {
		c.Handle = handle
	}
}

// PeerHandle returns the verified handle for any known peer, or "".
func (s *State) PeerHandle(peerID string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if h := s.d.Handles[peerID]; h != "" {
		return h
	}
	if c, ok := s.d.Contacts[peerID]; ok {
		return c.Handle
	}
	return ""
}

// IsContact reports whether the user has added this peer.
func (s *State) IsContact(peerID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.d.Contacts[peerID]
	return ok
}

// Decline forgets a message request and remembers that it was declined.
//
// The conversation and its history are deleted outright -- a declined
// request shouldn't linger on disk -- while the peer ID goes into the
// declined set so the same sender's knock, which stays on the doorbell,
// isn't surfaced again as a fresh request.
func (s *State) Decline(peerID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, m := range s.d.History[peerID] {
		delete(s.seen, m.ID)
	}
	for _, id := range s.d.Dropped[peerID] {
		delete(s.seen, id)
	}
	delete(s.d.History, peerID)
	delete(s.d.Dropped, peerID)
	delete(s.d.Conversations, peerID)
	delete(s.d.Contacts, peerID)
	delete(s.d.Unread, peerID)
	s.d.Declined[peerID] = true
}

// IsDeclined reports whether the user declined this peer's request.
func (s *State) IsDeclined(peerID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.d.Declined[peerID]
}

// Undecline clears a decline. Adding someone on purpose overrides an
// earlier decline -- the user has changed their mind.
func (s *State) Undecline(peerID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.d.Declined, peerID)
}

// Block adds a peer to the block list. Their conversation and history
// are left exactly as they are; only what arrives from now on is
// affected, and that is the engine's job to drop.
//
// Any unread count is cleared too: a conversation the user has just
// chosen to stop hearing from shouldn't keep asking for attention.
func (s *State) Block(peerID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.d.Blocked[peerID] = true
	delete(s.d.Unread, peerID)
}

// Unblock takes a peer off the block list.
func (s *State) Unblock(peerID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.d.Blocked, peerID)
}

// IsBlocked reports whether the user has blocked this peer.
func (s *State) IsBlocked(peerID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.d.Blocked[peerID]
}

// Discard marks messages as dealt with without keeping them: they never
// enter history, never count as unread, and RecordNew will refuse them
// from then on. It is how messages a blocked peer sent stay unseen for
// good, even after they're unblocked. Messages already in history are
// left alone -- discarding is for what was never shown.
func (s *State) Discard(peerID string, msgs ...*mailbox.Message) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, m := range msgs {
		if m == nil || m.ID == "" || s.seen[m.ID] {
			continue
		}
		s.seen[m.ID] = true
		s.d.Dropped[peerID] = append(s.d.Dropped[peerID], m.ID)
	}
}

// Unread is how many inbound messages from this peer the user hasn't
// opened yet.
func (s *State) Unread(peerID string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.d.Unread[peerID]
}

// MarkRead clears a conversation's unread count. It reports whether
// there was anything to clear, so callers can skip a disk write when
// the user merely reopens a conversation they've already read.
func (s *State) MarkRead(peerID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.d.Unread[peerID] == 0 {
		return false
	}
	delete(s.d.Unread, peerID)
	return true
}

// HasInbound reports whether the peer has ever written to us.
func (s *State) HasInbound(peerID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, m := range s.d.History[peerID] {
		if !m.Outbound {
			return true
		}
	}
	return false
}

// AddContact assigns a local nickname to a peer.
func (s *State) AddContact(nickname, peerID string) error {
	nickname = strings.TrimSpace(nickname)
	if nickname == "" {
		return errors.New("state: nickname required")
	}
	if len(peerID) != 64 {
		return fmt.Errorf("state: %q is not a username (expected 64 hex characters, got %d)",
			nickname, len(peerID))
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, c := range s.d.Contacts {
		if id != peerID && strings.EqualFold(c.Nickname, nickname) {
			return fmt.Errorf("state: nickname %q is already used for %.8s", nickname, id)
		}
	}
	s.d.Contacts[peerID] = &Contact{Nickname: nickname, PeerID: peerID}
	return nil
}

// Contacts returns every contact, sorted by nickname.
func (s *State) Contacts() []*Contact {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*Contact, 0, len(s.d.Contacts))
	for _, c := range s.d.Contacts {
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool {
		return strings.ToLower(out[i].Nickname) < strings.ToLower(out[j].Nickname)
	})
	return out
}

// Resolve turns a nickname OR a full username into a peer ID, so every
// command accepts either.
func (s *State) Resolve(nameOrID string) (string, bool) {
	nameOrID = strings.TrimSpace(nameOrID)
	if len(nameOrID) == 64 {
		return strings.ToLower(nameOrID), true
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, c := range s.d.Contacts {
		if strings.EqualFold(c.Nickname, nameOrID) {
			return id, true
		}
	}
	return "", false
}

// Nickname returns the local alias for a peer, or a short form of the
// username when there isn't one.
func (s *State) Nickname(peerID string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if c, ok := s.d.Contacts[peerID]; ok {
		return c.Nickname
	}
	if len(peerID) > 8 {
		return peerID[:8]
	}
	return peerID
}

// Record appends messages to a peer's history, skipping any whose ID is
// already stored.
//
// The dedup matters across restarts, not just within a session: the
// in-memory dedup in the notify layer starts empty each run, so without
// this a reconciliation pass after restart could append messages the
// history already holds.
func (s *State) Record(peerID string, outbound bool, msgs ...*mailbox.Message) int {
	return len(s.RecordNew(peerID, outbound, msgs...))
}

// RecordNew is Record, returning the messages that were actually new --
// what a UI should show, as opposed to re-deliveries it already has.
//
// Inbound messages that are new also count toward the conversation's
// unread total. Counting here, under the same lock as the dedup, is what
// keeps the number honest: a message delivered twice (pushed directly,
// then found again on the chain) or re-fetched after a restart is only
// ever counted once. Our own messages never count, and neither does
// anything from a blocked peer -- the engine drops those before they get
// here, and this check makes sure a stray one can't light up the count.
func (s *State) RecordNew(peerID string, outbound bool, msgs ...*mailbox.Message) []StoredMessage {
	now := time.Now().Unix()
	var added []StoredMessage
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, m := range msgs {
		if m == nil || m.ID == "" || s.seen[m.ID] {
			continue
		}
		s.seen[m.ID] = true
		sm := StoredMessage{
			ID:       m.ID,
			Outbound: outbound,
			Text:     m.Text,
			Sent:     m.Sent,
			Logged:   now,
		}
		s.d.History[peerID] = append(s.d.History[peerID], sm)
		added = append(added, sm)
	}
	if !outbound && len(added) > 0 && !s.d.Blocked[peerID] {
		s.d.Unread[peerID] += len(added)
	}
	return added
}

// MarkDelivered records that the peer's device received these outbound
// messages, and returns the IDs that weren't already marked.
func (s *State) MarkDelivered(peerID string, ids []string) []string {
	want := make(map[string]bool, len(ids))
	for _, id := range ids {
		want[id] = true
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var changed []string
	hist := s.d.History[peerID]
	for i := range hist {
		if hist[i].Outbound && !hist[i].Delivered && want[hist[i].ID] {
			hist[i].Delivered = true
			changed = append(changed, hist[i].ID)
		}
	}
	return changed
}

// Thread returns one peer's conversation as a single ordered list,
// merging what we sent with what we received.
//
// Ordering uses the sender's timestamp, clamped to when we logged the
// message: a message cannot, from our point of view, have arrived
// before it was sent, so a sender whose clock runs fast (or who lies)
// cannot pin their messages to the bottom of the thread forever.
//
// Ties keep the order messages were recorded in -- a stable sort with
// no tie-break. Timestamps are whole seconds, so a quick burst ("hi",
// "you there?", "?") all ties, and recording order is the only thing
// that still knows the right sequence: Receive surfaces each chain
// oldest-first, and history is persisted in that order. An earlier
// version broke ties on message ID, which is random, and shuffled every
// burst while looking perfectly deterministic.
//
// This is best-effort ordering, and worth being honest about: the two
// directions are independent chains written by two machines with two
// clocks, so a genuinely concurrent exchange can interleave differently
// on each side. Every chat system has this; none solve it perfectly
// without a shared clock.
func (s *State) Thread(peerID string) []StoredMessage {
	s.mu.Lock()
	msgs := append([]StoredMessage(nil), s.d.History[peerID]...)
	s.mu.Unlock()

	sort.SliceStable(msgs, func(i, j int) bool {
		return orderKey(msgs[i]) < orderKey(msgs[j])
	})
	return msgs
}

func orderKey(m StoredMessage) int64 {
	if m.Logged > 0 && m.Sent > m.Logged {
		return m.Logged
	}
	return m.Sent
}

// Pending returns conversations that are still unaccepted message
// requests.
func (s *State) Pending() []*mailbox.Conversation {
	out := make([]*mailbox.Conversation, 0)
	for _, c := range s.Conversations() {
		if !c.Accepted {
			out = append(out, c)
		}
	}
	return out
}
