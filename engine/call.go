package engine

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"time"

	"altnet-relay/account"
	"altnet-relay/mailbox"
	"altnet-relay/state"
)

// Voice calls. The audio itself never touches this package or the network
// Relay stores things on: it flows directly between the two computers,
// set up by the window's built-in WebRTC. All the engine does is carry the
// SETUP messages -- "I'd like to call you" (offer), "yes, here's how to
// reach me" (answer), "no" (decline), "hang up" (end) -- through the same
// sealed channel as chat.
//
// The setup messages are ordinary sealed Messages of kind "call". They:
//
//   - are never written to the conversation history and never count
//     toward a message request's three-message limit;
//   - only ring for someone the user has accepted. A stranger's offer is
//     dropped, so the message-request wall covers calls too;
//   - are ignored from blocked or banned people;
//   - are ignored if they're old, so opening the app doesn't ring for a
//     call that was over an hour ago.

// Call operations.
const (
	CallOffer   = "offer"
	CallAnswer  = "answer"
	CallDecline = "decline"
	CallBusy    = "busy"
	CallEnd     = "end"
)

// CallSignal is one call-setup message, either direction.
type CallSignal struct {
	PeerID string `json:"peerId"`
	Op     string `json:"op"`
	CallID string `json:"callId"`
	SDP    string `json:"sdp,omitempty"`   // session description, for offer and answer
	Video  bool   `json:"video"`           // on an offer: the caller wants a video call
	Group  string `json:"group,omitempty"` // set for a group call: the group's ID
}

// callWire is what is sealed: the same, minus who it's for (that is the
// conversation it travels in).
type callWire struct {
	Op     string `json:"op"`
	CallID string `json:"call"`
	SDP    string `json:"sdp,omitempty"`
	Video  bool   `json:"video,omitempty"`
	G      string `json:"g,omitempty"`
}

// maxCallSDP bounds a session description. A real one with audio, video and
// the control channel is 5-8 KB; this leaves room for a computer with many
// network interfaces while staying inside one message (mailbox.MaxTextLen,
// less the JSON around it).
const maxCallSDP = 14 * 1024

// callFreshness is how old a setup message may be and still ring. Long
// enough to survive slow delivery and computers whose clocks disagree by a
// couple of minutes (the age is measured with the sender's clock), short
// enough that a message that sat in a mailbox all night never does. A stale
// offer that does slip through is harmless: the caller's hang-up follows it
// in the same conversation and ends it (see routeCalls).
const callFreshness = 3 * time.Minute

// missedID prefixes the ID of the history entry for a call nobody picked up.
const missedID = "missed-"

// SendCall sends one call-setup message to a peer. It is refused unless the
// two of you already have a conversation going: calls are for people you've
// been talking to, not a way to reach a stranger.
func (e *Engine) SendCall(sig CallSignal) error {
	if err := e.allowed(); err != nil {
		return err
	}
	switch sig.Op {
	case CallOffer, CallAnswer, CallDecline, CallBusy, CallEnd:
	default:
		return errors.New("unknown call step")
	}
	if sig.CallID == "" || len(sig.CallID) > 64 {
		return errors.New("a call needs an id")
	}
	if len(sig.SDP) > maxCallSDP {
		return errors.New("call description too large")
	}
	if e.State.IsBlocked(sig.PeerID) {
		return errBlocked
	}
	if e.Mod.BannedPeer(sig.PeerID) {
		return errBanned
	}
	conv := e.State.Conversation(sig.PeerID)
	if !conv.Accepted {
		return errors.New("accept their request before calling")
	}
	if sig.Op == CallOffer && !e.State.HasInbound(sig.PeerID) && !e.isCoworker(sig.PeerID) {
		// You can call someone once they have written to you, which means
		// they have accepted you. Before that, an offer would land in their
		// message requests: it could never ring (strangers can't), yet it
		// would use up one of the three messages a request is allowed.
		return errors.New("you can call them once they've written back")
	}

	blob, err := json.Marshal(callWire{Op: sig.Op, CallID: sig.CallID, SDP: sig.SDP, Video: sig.Video})
	if err != nil {
		return err
	}
	unlock := e.sendLock(sig.PeerID)
	_, _, err = e.Inbox.SendKind(conv, mailbox.KindCall, string(blob))
	unlock()
	if err != nil {
		return err
	}
	switch sig.Op {
	case CallAnswer, CallDecline, CallBusy:
		e.trackCall(sig.CallID, false, true) // dealt with here, so a later hang-up isn't "missed"
	}
	e.State.PutConversation(conv)
	err = e.State.Save()
	if sig.Op == CallOffer {
		go e.awaitReply(conv, sig.CallID)
	}
	return err
}

// awaitReply checks the caller's conversation every couple of seconds until
// the other side answers, declines, or the phone has rung out. The answer
// normally arrives straight away by direct delivery; this is what makes a
// call still connect when it doesn't, instead of waiting for the
// once-a-minute check long after the caller gave up.
func (e *Engine) awaitReply(conv *mailbox.Conversation, callID string) {
	done := make(chan struct{})
	e.callMu.Lock()
	e.waiting[callID] = done
	e.callMu.Unlock()
	defer func() {
		e.callMu.Lock()
		delete(e.waiting, callID)
		e.callMu.Unlock()
	}()
	deadline := time.After(callRingFor)
	for {
		select {
		case <-e.stop:
			return
		case <-done:
			return
		case <-deadline:
			return
		case <-time.After(2 * time.Second):
			e.Inbox.Poll(conv)
		}
	}
}

// callRingFor is how long a caller keeps looking for a reply; it matches the
// window's own give-up time for an unanswered call.
const callRingFor = 50 * time.Second

// Group calls. Every participant connects directly to every other one (a
// "mesh"): no server mixes or forwards the sound. Setup:
//
//   - ring  (to every member) -- "I've started a call in this group"
//   - join  (to every member) -- "I'm joining it"
//   - hello (to one)          -- "I'm already in it", in reply to a join
//   - offer / answer (to one) -- connecting one pair, exactly as in a
//     one-to-one call; of each pair, the one with the smaller username offers,
//     so two people never both try at once
//   - leave (to every member)
//
// A mesh is the right shape for a small group and needs nothing from the
// server; it is why group calls are capped (see MaxGroupCall), since every
// participant sends their audio and video once per other participant.
const (
	GroupRing  = "ring"
	GroupJoin  = "join"
	GroupHello = "hello"
	GroupLeave = "leave"
)

// MaxGroupCall is the most people in one group call, everyone included.
const MaxGroupCall = 8

func groupCallOp(op string) bool {
	switch op {
	case GroupRing, GroupJoin, GroupHello, GroupLeave, CallOffer, CallAnswer:
		return true
	}
	return false
}

// SendGroupCall sends one group-call setup message: to one member if
// sig.PeerID is set, otherwise to every member of the group.
func (e *Engine) SendGroupCall(sig CallSignal) error {
	if err := e.allowed(); err != nil {
		return err
	}
	if !groupCallOp(sig.Op) {
		return errors.New("unknown call step")
	}
	if sig.CallID == "" || len(sig.CallID) > 64 || len(sig.SDP) > maxCallSDP {
		return errors.New("bad call setup")
	}
	g, ok := e.State.GroupMeta(sig.Group)
	if !ok || g.Status != state.GroupJoined {
		return errNotInGroup
	}
	var to []string
	if sig.PeerID != "" {
		if !contains(e.roster(g), sig.PeerID) {
			return errors.New("they're not in this group")
		}
		to = []string{sig.PeerID}
	} else {
		to = e.others(e.roster(g), nil)
	}
	blob, err := json.Marshal(callWire{Op: sig.Op, CallID: sig.CallID, SDP: sig.SDP, Video: sig.Video, G: sig.Group})
	if err != nil {
		return err
	}
	var wg sync.WaitGroup
	var mu sync.Mutex
	var firstErr error
	for _, m := range to {
		if e.State.IsBlocked(m) || e.Mod.BannedPeer(m) {
			continue
		}
		wg.Add(1)
		go func(m string) {
			defer wg.Done()
			unlock := e.sendLock(m)
			conv := e.State.Conversation(m)
			_, _, err := e.Inbox.SendKind(conv, mailbox.KindCall, string(blob))
			if err == nil {
				e.State.PutConversation(conv)
			}
			unlock()
			if err != nil {
				mu.Lock()
				if firstErr == nil {
					firstErr = err
				}
				mu.Unlock()
			}
		}(m)
	}
	wg.Wait()
	_ = e.State.Save()
	if sig.Op == GroupRing || sig.Op == GroupJoin {
		go e.awaitGroupReplies(sig.Group, to)
	}
	// One member out of reach shouldn't stop a call with the others.
	if len(to) == 1 {
		return firstErr
	}
	return nil
}

// awaitGroupReplies checks the members' conversations every few seconds for
// a little while after ringing or joining, for replies that didn't come by
// direct push.
func (e *Engine) awaitGroupReplies(group string, members []string) {
	for i := 0; i < 6; i++ {
		select {
		case <-e.stop:
			return
		case <-time.After(3 * time.Second):
		}
		for _, m := range members {
			if conv, ok := e.State.LookupConversation(m); ok {
				e.Inbox.Poll(conv)
			}
		}
	}
}

// routeCalls splits call-setup messages out of a batch: the chat messages
// come back for the normal path, the calls are handled here. Messages of a
// kind this version doesn't know are dropped, so a future kind can't show up
// as an empty chat bubble.
func (e *Engine) routeCalls(conv *mailbox.Conversation, msgs []*mailbox.Message) []*mailbox.Message {
	// A call that was offered and hung up within the same batch -- typical
	// when the app was closed while someone rang -- is over already. It
	// becomes a missed call without ringing first.
	ended := make(map[string]bool)
	for _, m := range msgs {
		if w, ok := e.parseCall(conv, m); ok && w.Op == CallEnd {
			ended[w.CallID] = true
		}
	}
	chat := msgs[:0:0]
	for _, m := range msgs {
		switch m.Kind {
		case "", mailbox.KindText:
			chat = append(chat, m)
		case mailbox.KindCall:
			if w, ok := e.parseCall(conv, m); ok {
				e.handleCall(conv.PeerID, w, w.Op == CallOffer && ended[w.CallID])
			}
		case mailbox.KindGroup:
			e.handleGroup(conv, m)
		}
	}
	return chat
}

// parseCall decodes a call-setup message and decides whether it may act at
// all: from an accepted, undeclined contact, recent, and well formed.
func (e *Engine) parseCall(conv *mailbox.Conversation, m *mailbox.Message) (callWire, bool) {
	var w callWire
	if m.Kind != mailbox.KindCall {
		return w, false
	}
	if age := time.Since(time.Unix(m.Sent, 0)); age > callFreshness || age < -callFreshness {
		return w, false
	}
	if json.Unmarshal([]byte(m.Text), &w) != nil || w.CallID == "" || len(w.CallID) > 64 || len(w.SDP) > maxCallSDP {
		return w, false
	}
	if w.G != "" {
		// A group call: only from a member of a group we're in.
		g, ok := e.State.GroupMeta(w.G)
		return w, ok && g.Status == state.GroupJoined && contains(e.roster(g), conv.PeerID) && groupCallOp(w.Op)
	}
	// A stranger's offer never rings; neither does a declined request's.
	if !conv.Accepted || e.State.IsDeclined(conv.PeerID) {
		return w, false
	}
	switch w.Op {
	case CallOffer, CallAnswer, CallDecline, CallBusy, CallEnd:
		return w, true
	}
	return w, false
}

// handleCall acts on one accepted setup message. silent records an offer
// without ringing, for a call already known to be over.
func (e *Engine) handleCall(peerID string, w callWire, silent bool) {
	if w.G != "" {
		if e.OnCall != nil {
			e.OnCall(CallSignal{PeerID: peerID, Op: w.Op, CallID: w.CallID, SDP: w.SDP, Video: w.Video, Group: w.G})
		}
		return
	}
	switch w.Op {
	case CallOffer:
		e.trackCall(w.CallID, true, false)
	case CallEnd:
		e.noteMissed(peerID, w.CallID)
	}
	if w.Op != CallOffer {
		// Whatever the reply, the caller can stop looking for one.
		e.callMu.Lock()
		if ch, ok := e.waiting[w.CallID]; ok {
			close(ch)
			delete(e.waiting, w.CallID)
		}
		e.callMu.Unlock()
	}
	if silent || e.OnCall == nil {
		return
	}
	e.OnCall(CallSignal{PeerID: peerID, Op: w.Op, CallID: w.CallID, SDP: w.SDP, Video: w.Video})
}

// noteMissed writes "Missed call" into the conversation when the caller
// hangs up before this side ever answered or declined.
func (e *Engine) noteMissed(peerID, callID string) {
	e.callMu.Lock()
	offered, handled := e.offered[callID], e.handled[callID]
	delete(e.offered, callID)
	delete(e.handled, callID)
	e.callMu.Unlock()
	if !offered || handled {
		return
	}
	msg := &mailbox.Message{ID: missedID + callID, Kind: mailbox.KindText, Text: "Missed call", Sent: time.Now().Unix()}
	added := e.State.RecordNew(peerID, false, msg)
	_ = e.State.Save()
	if len(added) > 0 && e.OnMessages != nil {
		e.OnMessages(peerID, added)
	}
}

// trackCall keeps just enough about each call to tell a missed one from one
// that was answered or turned down here.
func (e *Engine) trackCall(callID string, offered, handled bool) {
	e.callMu.Lock()
	defer e.callMu.Unlock()
	if len(e.offered) > 64 || len(e.handled) > 64 { // never grow without bound
		e.offered, e.handled = map[string]bool{}, map[string]bool{}
	}
	if offered {
		e.offered[callID] = true
	}
	if handled {
		e.handled[callID] = true
	}
}

// CallServers returns the STUN/TURN servers a call should use. Signed-in
// people get short-lived relay credentials from the server, which is what
// lets two people behind strict routers hear each other; without them, or
// offline, calls fall back to the default STUN servers and connect when the
// network allows.
func (e *Engine) CallServers() []account.IceServer {
	if !e.ownSTUNReachable() {
		// The operator's server isn't answering (its ports may not be
		// open). Its relay is behind the same door, so don't offer it
		// either: use the fallback mirror, so calls between two home
		// networks can still connect directly.
		return append([]account.IceServer(nil), FallbackSTUN...)
	}
	servers := append([]account.IceServer(nil), DefaultSTUN...)
	if e.acct == nil {
		return servers
	}
	e.acctMu.Lock()
	sess := e.sess
	e.acctMu.Unlock()
	if sess == nil {
		return servers
	}
	// Relay credentials last an hour; reuse them rather than asking the
	// server before every call. That ask is a round trip the caller would
	// otherwise sit through before the phone even starts ringing.
	e.callMu.Lock()
	cached, until := e.turn, e.turnUntil
	e.callMu.Unlock()
	if cached != nil && time.Now().Before(until) {
		return append(servers, *cached)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	t, err := e.acct.TURN(ctx, sess.Token)
	if err != nil || len(t.URLs) == 0 {
		return servers
	}
	ice := account.IceServer{URLs: t.URLs, Username: t.Username, Credential: t.Credential}
	ttl := time.Duration(t.TTL) * time.Second
	if ttl <= 0 {
		ttl = time.Hour
	}
	e.callMu.Lock()
	// Stop using them well before they expire: a call can last a while,
	// and the relay checks them when the call starts using it.
	e.turn, e.turnUntil = &ice, time.Now().Add(ttl/2)
	e.callMu.Unlock()
	return append(servers, ice)
}

// WarmCalls fetches relay credentials ahead of time, so the first call
// doesn't wait for them. Cheap to call often; the window does it when a
// conversation that can make calls is opened.
func (e *Engine) WarmCalls() { go e.CallServers() }
