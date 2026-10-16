package engine

import (
	"strings"
	"sync"
	"testing"
	"time"

	"altnet-relay/mailbox"
	"altnet-relay/state"
)

// callSink collects the call signals an engine reports.
type callSink struct {
	mu   sync.Mutex
	sigs []CallSignal
}

func (c *callSink) add(s CallSignal) {
	c.mu.Lock()
	c.sigs = append(c.sigs, s)
	c.mu.Unlock()
}

func (c *callSink) has(op string) (CallSignal, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, s := range c.sigs {
		if s.Op == op {
			return s, true
		}
	}
	return CallSignal{}, false
}

func (c *callSink) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.sigs)
}

// twoFriends starts rob and kate as mutual contacts who have each said hello,
// which is what a call needs to exist. Kate's engine reports calls to the sink.
func twoFriends(t *testing.T) (rob, kate *Engine, robID, kateID string, sink *callSink) {
	t.Helper()
	rob = open(t, t.TempDir())
	t.Cleanup(rob.Close)
	kate = open(t, t.TempDir(), rob.Peer.LocalAddr())
	t.Cleanup(kate.Close)
	time.Sleep(200 * time.Millisecond)

	sink = &callSink{}
	kate.OnCall = sink.add

	kateID = contacts(t, rob, kate)
	robID = rob.Username()
	if _, err := rob.Send(kateID, "hi kate"); err != nil {
		t.Fatal(err)
	}
	if !waitPolled(kate, 8*time.Second, func() bool { return threadHas(kate, robID, "hi kate") }) {
		t.Fatal("kate never got the hello")
	}
	if _, err := kate.Send(robID, "hi rob"); err != nil {
		t.Fatal(err)
	}
	// A call can only be placed to someone who has written back.
	if !waitPolled(rob, 8*time.Second, func() bool { return threadHas(rob, kateID, "hi rob") }) {
		t.Fatal("rob never got kate's reply")
	}
	return
}

func TestCallSetupTravelsBothWaysAndStaysOutOfTheChat(t *testing.T) {
	rob, kate, robID, kateID, sink := twoFriends(t)
	robSink := &callSink{}
	rob.OnCall = robSink.add
	before := len(kate.Thread(robID))

	offerSDP := "v=0\r\no=- 1 2 IN IP4 127.0.0.1\r\ns=-\r\n" + strings.Repeat("a=candidate:1 1 udp 2113937151 192.168.1.5 5000 typ host\r\n", 20)
	if err := rob.SendCall(CallSignal{PeerID: kateID, Op: CallOffer, CallID: "call-1", SDP: offerSDP}); err != nil {
		t.Fatal(err)
	}
	if !waitPolled(kate, 8*time.Second, func() bool { _, ok := sink.has(CallOffer); return ok }) {
		t.Fatal("kate's phone never rang")
	}
	got, _ := sink.has(CallOffer)
	if got.PeerID != robID || got.CallID != "call-1" || got.SDP != offerSDP {
		t.Fatalf("offer arrived wrong: %+v", got)
	}

	if err := kate.SendCall(CallSignal{PeerID: robID, Op: CallAnswer, CallID: "call-1", SDP: "answer-sdp"}); err != nil {
		t.Fatal(err)
	}
	if !waitPolled(rob, 8*time.Second, func() bool { _, ok := robSink.has(CallAnswer); return ok }) {
		t.Fatal("rob never got the answer")
	}
	ans, _ := robSink.has(CallAnswer)
	if ans.SDP != "answer-sdp" || ans.PeerID != kateID {
		t.Fatalf("answer wrong: %+v", ans)
	}

	if err := rob.SendCall(CallSignal{PeerID: kateID, Op: CallEnd, CallID: "call-1"}); err != nil {
		t.Fatal(err)
	}
	if !waitPolled(kate, 8*time.Second, func() bool { _, ok := sink.has(CallEnd); return ok }) {
		t.Fatal("hang-up never arrived")
	}

	// None of it is chat: no bubbles, no unread count, and an answered call
	// isn't a missed one.
	if after := len(kate.Thread(robID)); after != before {
		t.Fatalf("call setup leaked into the conversation: %d -> %d messages", before, after)
	}
	for _, m := range kate.Thread(robID) {
		if strings.Contains(m.Text, "call-1") || m.Text == "Missed call" {
			t.Fatalf("unexpected history entry %q", m.Text)
		}
	}
}

func TestUnansweredCallLeavesAMissedCall(t *testing.T) {
	rob, kate, robID, kateID, sink := twoFriends(t)
	var got sync.Mutex
	var texts []string
	kate.OnMessages = func(peerID string, msgs []state.StoredMessage) {
		got.Lock()
		defer got.Unlock()
		for _, m := range msgs {
			texts = append(texts, m.Text)
		}
	}

	if err := rob.SendCall(CallSignal{PeerID: kateID, Op: CallOffer, CallID: "c2", SDP: "sdp"}); err != nil {
		t.Fatal(err)
	}
	if !waitPolled(kate, 8*time.Second, func() bool { _, ok := sink.has(CallOffer); return ok }) {
		t.Fatal("no ring")
	}
	// Rob gives up before Kate answers.
	if err := rob.SendCall(CallSignal{PeerID: kateID, Op: CallEnd, CallID: "c2"}); err != nil {
		t.Fatal(err)
	}
	if !waitPolled(kate, 8*time.Second, func() bool { return threadHas(kate, robID, "Missed call") }) {
		t.Fatal("kate should see a missed call in the conversation")
	}
	told := func() bool {
		got.Lock()
		defer got.Unlock()
		return len(texts) > 0 && texts[len(texts)-1] == "Missed call"
	}
	if !waitFor(2*time.Second, told) {
		t.Fatalf("the window should be told about the missed call, got %v", texts)
	}
}

func TestAnsweredOrDeclinedCallIsNotMissed(t *testing.T) {
	rob, kate, robID, kateID, sink := twoFriends(t)
	if err := rob.SendCall(CallSignal{PeerID: kateID, Op: CallOffer, CallID: "c3", SDP: "sdp"}); err != nil {
		t.Fatal(err)
	}
	if !waitPolled(kate, 8*time.Second, func() bool { _, ok := sink.has(CallOffer); return ok }) {
		t.Fatal("no ring")
	}
	if err := kate.SendCall(CallSignal{PeerID: robID, Op: CallDecline, CallID: "c3"}); err != nil {
		t.Fatal(err)
	}
	rob.SendCall(CallSignal{PeerID: kateID, Op: CallEnd, CallID: "c3"})
	waitPolled(kate, 8*time.Second, func() bool { _, ok := sink.has(CallEnd); return ok })
	if threadHas(kate, robID, "Missed call") {
		t.Fatal("a call Kate declined must not show as missed")
	}
}

func TestStrangersAndBlockedPeopleCannotRing(t *testing.T) {
	rob := open(t, t.TempDir())
	defer rob.Close()
	kate := open(t, t.TempDir(), rob.Peer.LocalAddr())
	defer kate.Close()
	time.Sleep(200 * time.Millisecond)
	sink := &callSink{}
	kate.OnCall = sink.add

	// Rob reaches out and says hello; Kate hasn't accepted him.
	kateID, err := rob.Add(kate.Username(), "kate")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rob.Send(kateID, "hello stranger"); err != nil {
		t.Fatal(err)
	}
	if !waitFor(8*time.Second, func() bool { _, ok := summaryFor(kate, rob.Username()); return ok }) {
		t.Fatal("kate never saw the request")
	}
	// The app won't even try: Kate hasn't written back.
	if err := rob.SendCall(CallSignal{PeerID: kateID, Op: CallOffer, CallID: "c4", SDP: "sdp"}); err == nil {
		t.Fatal("calling someone who never wrote back should be refused")
	}
	// A modified client sends the offer anyway; Kate's side must drop it.
	forced := rob.State.Conversation(kateID)
	if _, _, err := rob.Inbox.SendKind(forced, mailbox.KindCall, `{"op":"offer","call":"c4","sdp":"sdp"}`); err != nil {
		t.Fatal(err)
	}
	settle(kate)
	if sink.count() != 0 {
		t.Fatal("a message-request sender must not be able to ring Kate")
	}

	// Once Kate accepts and writes back, Rob can call -- and once she
	// blocks him, he can't again.
	if err := kate.Accept(rob.Username()); err != nil {
		t.Fatal(err)
	}
	if _, err := kate.Send(rob.Username(), "ok, hi"); err != nil {
		t.Fatal(err)
	}
	if !waitPolled(rob, 8*time.Second, func() bool { return threadHas(rob, kateID, "ok, hi") }) {
		t.Fatal("rob never got kate's reply")
	}
	if err := rob.SendCall(CallSignal{PeerID: kateID, Op: CallOffer, CallID: "c5", SDP: "sdp"}); err != nil {
		t.Fatal(err)
	}
	if !waitPolled(kate, 8*time.Second, func() bool { _, ok := sink.has(CallOffer); return ok }) {
		t.Fatal("an accepted contact should be able to ring")
	}

	n := sink.count()
	if err := kate.Block(rob.Username()); err != nil {
		t.Fatal(err)
	}
	rob.SendCall(CallSignal{PeerID: kateID, Op: CallOffer, CallID: "c6", SDP: "sdp"})
	settle(kate)
	if sink.count() != n {
		t.Fatal("a blocked person must not be able to ring")
	}
	if err := kate.SendCall(CallSignal{PeerID: rob.Username(), Op: CallOffer, CallID: "c7"}); err == nil {
		t.Fatal("you can't call someone you've blocked")
	}
}

func TestCallNeedsAConversationAndSaneInput(t *testing.T) {
	rob := open(t, t.TempDir())
	defer rob.Close()
	kate := open(t, t.TempDir(), rob.Peer.LocalAddr())
	defer kate.Close()
	time.Sleep(200 * time.Millisecond)

	kateID, err := rob.Add(kate.Username(), "kate")
	if err != nil {
		t.Fatal(err)
	}
	if err := rob.SendCall(CallSignal{PeerID: kateID, Op: CallOffer, CallID: "x", SDP: "s"}); err == nil || !strings.Contains(err.Error(), "written back") {
		t.Fatalf("a first contact must be a message, not a call: %v", err)
	}
	rob.Send(kateID, "hi")
	if err := rob.SendCall(CallSignal{PeerID: kateID, Op: CallOffer, CallID: "x", SDP: "s"}); err == nil {
		t.Fatal("writing to someone isn't enough; they have to have written back")
	}
	for name, sig := range map[string]CallSignal{
		"unknown op":   {PeerID: kateID, Op: "explode", CallID: "x"},
		"no call id":   {PeerID: kateID, Op: CallOffer},
		"huge sdp":     {PeerID: kateID, Op: CallOffer, CallID: "x", SDP: strings.Repeat("a", maxCallSDP+1)},
		"long call id": {PeerID: kateID, Op: CallOffer, CallID: strings.Repeat("a", 65)},
	} {
		if err := rob.SendCall(sig); err == nil {
			t.Errorf("%s should be refused", name)
		}
	}
}

// Old setup messages must never ring: opening the app in the morning can't
// show a call from last night.
func TestStaleAndMalformedSignalsAreIgnored(t *testing.T) {
	e := open(t, t.TempDir())
	defer e.Close()
	sink := &callSink{}
	e.OnCall = sink.add
	peer := strings.Repeat("a", 64)
	conv := e.State.Conversation(peer)
	conv.Accepted = true

	send := func(sent int64, text string) {
		e.routeCalls(conv, []*mailbox.Message{{ID: "m", Kind: mailbox.KindCall, Sent: sent, Text: text}})
	}
	now := time.Now().Unix()
	send(now-3600, `{"op":"offer","call":"old","sdp":"s"}`)
	send(now+3600, `{"op":"offer","call":"future","sdp":"s"}`)
	send(now, `not json`)
	send(now, `{"op":"offer"}`)
	send(now, `{"op":"weird","call":"x"}`)
	if sink.count() != 0 {
		t.Fatalf("stale/garbage setup rang: %+v", sink.sigs)
	}
	send(now, `{"op":"offer","call":"fresh","sdp":"s"}`)
	if sink.count() != 1 {
		t.Fatal("a fresh offer should ring")
	}

	// A conversation that isn't accepted never rings, however fresh.
	conv.Accepted = false
	send(now, `{"op":"offer","call":"fresh2","sdp":"s"}`)
	if sink.count() != 1 {
		t.Fatal("an unaccepted conversation must not ring")
	}
}

func TestUnknownMessageKindsDoNotBecomeEmptyBubbles(t *testing.T) {
	e := open(t, t.TempDir())
	defer e.Close()
	peer := strings.Repeat("b", 64)
	conv := e.State.Conversation(peer)
	conv.Accepted = true
	e.onMessages(conv, []*mailbox.Message{
		{ID: "k1", Kind: "hologram", Text: "", Sent: time.Now().Unix()},
		{ID: "k2", Kind: mailbox.KindText, Text: "hello", Sent: time.Now().Unix()},
	})
	th := e.Thread(peer)
	if len(th) != 1 || th[0].Text != "hello" {
		t.Fatalf("thread = %+v; a message kind this version doesn't know must be skipped", th)
	}
}

// Someone rang while the app was closed and gave up: the offer and the
// hang-up arrive together. That is a missed call, and nothing should ring.
func TestCallThatEndedWhileAwayIsMissedWithoutRinging(t *testing.T) {
	e := open(t, t.TempDir())
	defer e.Close()
	sink := &callSink{}
	e.OnCall = sink.add
	peer := strings.Repeat("c", 64)
	conv := e.State.Conversation(peer)
	conv.Accepted = true
	now := time.Now().Unix()
	e.onMessages(conv, []*mailbox.Message{
		{ID: "o1", Kind: mailbox.KindCall, Sent: now - 60, Text: `{"op":"offer","call":"away","sdp":"s"}`},
		{ID: "e1", Kind: mailbox.KindCall, Sent: now - 15, Text: `{"op":"end","call":"away"}`},
	})
	if _, rang := sink.has(CallOffer); rang {
		t.Fatal("a call that is already over must not ring")
	}
	if !threadHas(e, peer, "Missed call") {
		t.Fatal("it should show as a missed call")
	}
}
