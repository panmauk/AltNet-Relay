package engine

import (
	"strings"

	"altnet-relay/mailbox"
	"altnet-relay/notify"
	"altnet-relay/state"
)

// Live signals between two people who are both online: "typing…" and
// "delivered". They go straight from one device to the other, sealed and
// signed (notify/signal.go), and are never stored. Only accepted contacts
// exchange them -- a stranger's message request is never told whether it
// arrived, and a stranger can't make "typing…" appear.

// Typing tells the peer the user is typing to them. The window calls it as
// the person types (throttled there); it does nothing for a conversation
// that isn't an accepted, two-way one.
func (e *Engine) Typing(peerID string) {
	conv, ok := e.signalable(peerID)
	if !ok || e.allowed() != nil {
		return
	}
	go e.Signals.Send(conv, notify.SignalTyping, nil)
}

// acknowledge tells the sender which of their messages just reached us.
func (e *Engine) acknowledge(conv *mailbox.Conversation, added []state.StoredMessage) {
	if len(added) == 0 {
		return
	}
	c, ok := e.signalable(conv.PeerID)
	if !ok {
		return
	}
	var ids []string
	for _, m := range added {
		if m.Outbound || strings.HasPrefix(m.ID, missedID) {
			continue
		}
		ids = append(ids, m.ID)
		if len(ids) == notify.MaxSignalIDs {
			break
		}
	}
	if len(ids) > 0 {
		go e.Signals.Send(c, notify.SignalDelivered, ids)
	}
}

// signalable returns the conversation if signals may flow both ways with
// this peer: accepted, not blocked, declined or banned, and we know how to
// reach and encrypt to them.
func (e *Engine) signalable(peerID string) (*mailbox.Conversation, bool) {
	if e.State.IsBlocked(peerID) || e.State.IsDeclined(peerID) || e.Mod.BannedPeer(peerID) {
		return nil, false
	}
	// Looked up, never created: a signal must not be able to put a
	// conversation in the list.
	conv, ok := e.State.LookupConversation(peerID)
	if !ok || !conv.Accepted || conv.PeerBoxKey == "" || len(conv.PeerAddrs) == 0 {
		return nil, false
	}
	return conv, true
}

// TypingGroup tells a group's members (the ones who are online and whose
// devices we know) that the user is typing there.
func (e *Engine) TypingGroup(id string) {
	if e.allowed() != nil {
		return
	}
	g, ok := e.State.GroupMeta(id)
	if !ok || g.Status != state.GroupJoined {
		return
	}
	for _, m := range e.others(e.roster(g), nil) {
		if conv, ok := e.signalable(m); ok {
			go e.Signals.SendFor(conv, notify.SignalTyping, id, nil)
		}
	}
}

// acknowledgeGroup tells the sender of a group message that it reached us.
func (e *Engine) acknowledgeGroup(id, from, mid string) {
	if conv, ok := e.signalable(from); ok {
		go e.Signals.SendFor(conv, notify.SignalDelivered, id, []string{mid})
	}
}

// onGroupSignal handles typing and receipts inside a group: only from a
// member of a group we're in.
func (e *Engine) onGroupSignal(from string, sig notify.Signal) {
	g, ok := e.State.GroupMeta(sig.Group)
	if !ok || g.Status != state.GroupJoined || !contains(e.roster(g), from) {
		return
	}
	switch sig.Kind {
	case notify.SignalTyping:
		if e.OnGroupTyping != nil {
			e.OnGroupTyping(sig.Group, from)
		}
	case notify.SignalDelivered:
		if changed := e.State.MarkGroupDelivered(sig.Group, from, sig.IDs); len(changed) > 0 {
			_ = e.State.Save()
			if e.OnGroupDelivered != nil {
				e.OnGroupDelivered(sig.Group, changed)
			}
		}
	}
}

func (e *Engine) onSignal(from string, sig notify.Signal) {
	// Only from people we have an accepted conversation with. Anyone can
	// seal a signal to us; that isn't a reason to show it.
	if e.State.IsBlocked(from) || e.State.IsDeclined(from) || e.Mod.BannedPeer(from) {
		return
	}
	if conv, ok := e.State.LookupConversation(from); !ok || !conv.Accepted {
		return
	}
	if sig.Group != "" {
		e.onGroupSignal(from, sig)
		return
	}
	switch sig.Kind {
	case notify.SignalTyping:
		if e.OnTyping != nil {
			e.OnTyping(from)
		}
	case notify.SignalDelivered:
		changed := e.State.MarkDelivered(from, sig.IDs)
		if len(changed) < len(sig.IDs) {
			// Some may be messages still being stored on our side: the
			// receipt overtook them. Keep those for when they're recorded.
			have := make(map[string]bool, len(changed))
			for _, id := range changed {
				have[id] = true
			}
			e.earlyMu.Lock()
			if len(e.early) > 256 {
				e.early = make(map[string]bool)
			}
			for _, id := range sig.IDs {
				if !have[id] {
					e.early[id] = true
				}
			}
			e.earlyMu.Unlock()
		}
		if len(changed) > 0 {
			_ = e.State.Save()
			if e.OnDelivered != nil {
				e.OnDelivered(from, changed)
			}
		}
	}
}

// takeEarly reports (and forgets) a receipt that arrived before its message
// was recorded.
func (e *Engine) takeEarly(id string) bool {
	e.earlyMu.Lock()
	defer e.earlyMu.Unlock()
	if e.early[id] {
		delete(e.early, id)
		return true
	}
	return false
}
