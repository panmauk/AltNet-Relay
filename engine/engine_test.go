package engine

import (
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"altnet-relay/state"
)

// open starts a real engine on a free localhost port, joined through
// bootstrap (which may be empty for the first node).
func open(t *testing.T, dir string, bootstrap ...string) *Engine {
	t.Helper()
	e, err := Open(Config{DataDir: dir, Listen: "127.0.0.1:0", Bootstrap: bootstrap})
	if err != nil {
		t.Fatal(err)
	}
	e.Start()
	return e
}

func waitFor(d time.Duration, cond func() bool) bool {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(25 * time.Millisecond)
	}
	return cond()
}

// waitPolled is waitFor with a full inbox check every half second. After
// a restart or an unblock, the watch on a mailbox takes a moment to be
// re-armed; these tests are about what the engine lets through, not how
// quickly a notification finds its way back.
func waitPolled(e *Engine, d time.Duration, cond func() bool) bool {
	deadline := time.Now().Add(d)
	next := time.Now()
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		if time.Now().After(next) {
			e.Inbox.PollAll()
			next = time.Now().Add(500 * time.Millisecond)
		}
		time.Sleep(25 * time.Millisecond)
	}
	return cond()
}

// settle gives anything in flight every chance to arrive, for tests that
// check something did NOT happen.
func settle(e *Engine) {
	time.Sleep(1500 * time.Millisecond)
	e.Inbox.PollAll()
}

func threadHas(e *Engine, peerID, text string) bool {
	for _, m := range e.Thread(peerID) {
		if m.Text == text {
			return true
		}
	}
	return false
}

// contacts makes rob and kate mutual contacts, each having added the
// other, and returns kate's ID as rob knows it.
func contacts(t *testing.T, rob, kate *Engine) string {
	t.Helper()
	kateID, err := rob.Add(kate.Username(), "kate")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := kate.Add(rob.Username(), "rob"); err != nil {
		t.Fatal(err)
	}
	return kateID
}

func summaryFor(e *Engine, peerID string) (Summary, bool) {
	for _, s := range e.Conversations() {
		if s.PeerID == peerID {
			return s, true
		}
	}
	return Summary{}, false
}

// The bug this engine fixes: when YOU reach out to someone, their
// replies must not be treated as a stranger's message request. The old
// terminal client left such conversations unaccepted, so a reply was
// capped at three messages and filed under Requests.
func TestReachingOutAcceptsTheirReplies(t *testing.T) {
	rob := open(t, t.TempDir())
	defer rob.Close()
	bob := open(t, t.TempDir(), rob.Peer.LocalAddr())
	defer bob.Close()
	time.Sleep(200 * time.Millisecond)

	if _, err := bob.Claim("bob"); err != nil {
		t.Fatal(err)
	}

	// Rob adds Bob by his handle and writes first.
	bobID, err := rob.Add("bob", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rob.Send(bobID, "hi bob, it's rob"); err != nil {
		t.Fatal(err)
	}

	// Bob sees Rob as a request, accepts, and replies five times --
	// more than the three a request would allow.
	if !waitFor(5*time.Second, func() bool { _, ok := summaryFor(bob, rob.Username()); return ok }) {
		t.Fatal("rob's first message never reached bob")
	}
	if err := bob.Accept(rob.Username()); err != nil {
		t.Fatal(err)
	}
	for _, text := range []string{"one", "two", "three", "four", "five"} {
		if _, err := bob.Send(rob.Username(), text); err != nil {
			t.Fatal(err)
		}
	}

	// Rob must get all five, and Bob must not be sitting in his Requests.
	if !waitFor(8*time.Second, func() bool { return len(rob.Thread(bobID)) >= 6 }) {
		t.Fatalf("rob saw %d messages; replies to someone he reached out to were capped", len(rob.Thread(bobID)))
	}
	s, _ := summaryFor(rob, bobID)
	if s.Request {
		t.Fatal("someone rob added himself must not appear as a message request")
	}
	if s.Name != "bob" {
		t.Fatalf("expected the contact to be called bob, got %q", s.Name)
	}
}

// A stranger's first contact is a request, and it shows who they are by
// their verified handle.
func TestStrangerArrivesAsRequestWithVerifiedName(t *testing.T) {
	rob := open(t, t.TempDir())
	defer rob.Close()
	kate := open(t, t.TempDir(), rob.Peer.LocalAddr())
	defer kate.Close()
	time.Sleep(200 * time.Millisecond)

	if _, err := rob.Claim("rob"); err != nil {
		t.Fatal(err)
	}
	if _, err := kate.Claim("kate"); err != nil {
		t.Fatal(err)
	}

	requested := make(chan string, 1)
	rob.OnRequest = func(peerID string) { requested <- peerID }

	robID, err := kate.Add("rob", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := kate.Send(robID, "hi, bob gave me your name"); err != nil {
		t.Fatal(err)
	}

	select {
	case id := <-requested:
		if id != kate.Username() {
			t.Fatal("request came from the wrong peer")
		}
	case <-time.After(6 * time.Second):
		t.Fatal("no request callback fired")
	}

	if !waitFor(5*time.Second, func() bool { s, ok := summaryFor(rob, kate.Username()); return ok && s.Request }) {
		t.Fatal("kate should appear as a pending request")
	}
	s, _ := summaryFor(rob, kate.Username())
	if s.Name != "@kate" || s.Handle != "kate" {
		t.Fatalf("expected a verified @kate, got name=%q handle=%q", s.Name, s.Handle)
	}
	// And you can't reply to a request without accepting it first.
	if _, err := rob.Send(kate.Username(), "hello"); err == nil {
		t.Fatal("replying to an unaccepted request should be refused")
	}
}

// Declining is final across restarts: the sender's knock stays on the
// doorbell forever, so without remembering the decline the request would
// come straight back the next time the app opened.
func TestDeclineSurvivesRestart(t *testing.T) {
	robDir := t.TempDir()
	rob := open(t, robDir)
	spam := open(t, t.TempDir(), rob.Peer.LocalAddr())
	defer spam.Close()
	time.Sleep(200 * time.Millisecond)

	if _, err := spam.Add(rob.Username(), "rob"); err != nil {
		t.Fatal(err)
	}
	if _, err := spam.Send(rob.Username(), "buy my thing"); err != nil {
		t.Fatal(err)
	}
	if !waitFor(5*time.Second, func() bool { s, ok := summaryFor(rob, spam.Username()); return ok && s.Request }) {
		t.Fatal("request never arrived")
	}

	if err := rob.Decline(spam.Username()); err != nil {
		t.Fatal(err)
	}
	if _, ok := summaryFor(rob, spam.Username()); ok {
		t.Fatal("a declined request should disappear immediately")
	}
	if len(rob.Thread(spam.Username())) != 0 {
		t.Fatal("a declined request's messages should be deleted")
	}

	// Restart Rob on the same data, rejoining the network.
	rob.Close()
	rob = open(t, robDir, spam.Peer.LocalAddr())
	defer rob.Close()

	// Give reconciliation every chance to resurface it.
	time.Sleep(2 * time.Second)
	rob.Inbox.PollAll()
	if _, ok := summaryFor(rob, spam.Username()); ok {
		t.Fatal("a declined request came back after restart")
	}

	// But adding them on purpose overrides the decline.
	if _, err := rob.Add(spam.Username(), "changed-my-mind"); err != nil {
		t.Fatal(err)
	}
	if rob.State.IsDeclined(spam.Username()) {
		t.Fatal("adding someone should clear an earlier decline")
	}
}

// Accepting a request makes the sender a contact, named by their handle.
func TestAcceptMakesThemAContact(t *testing.T) {
	rob := open(t, t.TempDir())
	defer rob.Close()
	kate := open(t, t.TempDir(), rob.Peer.LocalAddr())
	defer kate.Close()
	time.Sleep(200 * time.Millisecond)

	if _, err := kate.Claim("kate"); err != nil {
		t.Fatal(err)
	}
	if _, err := kate.Add(rob.Username(), "rob"); err != nil {
		t.Fatal(err)
	}
	if _, err := kate.Send(rob.Username(), "hello"); err != nil {
		t.Fatal(err)
	}
	if !waitFor(5*time.Second, func() bool { _, ok := summaryFor(rob, kate.Username()); return ok }) {
		t.Fatal("request never arrived")
	}

	if err := rob.Accept(kate.Username()); err != nil {
		t.Fatal(err)
	}
	if !rob.State.IsContact(kate.Username()) {
		t.Fatal("accepting should add them as a contact")
	}
	s, _ := summaryFor(rob, kate.Username())
	if s.Request || !s.Accepted {
		t.Fatal("an accepted request should no longer be a request")
	}
	if s.Name != "kate" {
		t.Fatalf("expected the new contact to be called kate, got %q", s.Name)
	}
	if _, err := rob.Send(kate.Username(), "welcome"); err != nil {
		t.Fatalf("replying after accepting should work: %v", err)
	}
}

// Accepting a request must reveal what the pending cap held back, not
// just what arrives afterwards. The cap is only a gate: accepting opens
// it, and everything the sender already wrote should come through.
func TestAcceptRevealsHeldBackMessages(t *testing.T) {
	rob := open(t, t.TempDir())
	defer rob.Close()
	kate := open(t, t.TempDir(), rob.Peer.LocalAddr())
	defer kate.Close()
	time.Sleep(200 * time.Millisecond)

	if _, err := kate.Add(rob.Username(), "rob"); err != nil {
		t.Fatal(err)
	}
	for _, text := range []string{"1", "2", "3", "4", "5"} {
		if _, err := kate.Send(rob.Username(), text); err != nil {
			t.Fatal(err)
		}
	}

	// While it's a request, Rob sees only the first three.
	if !waitFor(5*time.Second, func() bool { return len(rob.Thread(kate.Username())) == 3 }) {
		t.Fatalf("expected the first 3 while pending, got %d", len(rob.Thread(kate.Username())))
	}
	time.Sleep(500 * time.Millisecond) // let any straggling notifications settle
	if n := len(rob.Thread(kate.Username())); n != 3 {
		t.Fatalf("the pending cap leaked: %d messages visible before accepting", n)
	}

	if err := rob.Accept(kate.Username()); err != nil {
		t.Fatal(err)
	}
	if !waitFor(5*time.Second, func() bool { return len(rob.Thread(kate.Username())) == 5 }) {
		t.Fatalf("accepting should reveal all 5 messages, got %d", len(rob.Thread(kate.Username())))
	}
	thread := rob.Thread(kate.Username())
	for i, want := range []string{"1", "2", "3", "4", "5"} {
		if thread[i].Text != want {
			t.Fatalf("message %d: got %q, want %q", i, thread[i].Text, want)
		}
	}
}

func TestCannotAddYourself(t *testing.T) {
	rob := open(t, t.TempDir())
	defer rob.Close()
	if _, err := rob.Add(rob.Username(), ""); err == nil {
		t.Fatal("adding yourself should be refused")
	}
}

// Blocking works on contacts too, not just strangers: once blocked,
// nothing new from them is shown, counted or handed to the UI, while the
// history from before stays put.
func TestBlockedContactIsSilenced(t *testing.T) {
	rob := open(t, t.TempDir())
	defer rob.Close()
	kate := open(t, t.TempDir(), rob.Peer.LocalAddr())
	defer kate.Close()
	time.Sleep(200 * time.Millisecond)

	kateID := contacts(t, rob, kate)
	if _, err := rob.Send(kateID, "hi kate"); err != nil { // so she can reach him directly too
		t.Fatal(err)
	}
	if _, err := kate.Send(rob.Username(), "hello"); err != nil {
		t.Fatal(err)
	}
	if !waitFor(5*time.Second, func() bool { return threadHas(rob, kateID, "hello") }) {
		t.Fatal("kate's message never arrived")
	}

	var surfaced atomic.Int32
	rob.OnMessages = func(peerID string, msgs []state.StoredMessage) {
		if peerID == kateID {
			surfaced.Add(int32(len(msgs)))
		}
	}
	if err := rob.Block(kateID); err != nil {
		t.Fatal(err)
	}
	if _, err := kate.Send(rob.Username(), "are you ignoring me?"); err != nil {
		t.Fatal(err)
	}
	settle(rob)

	if threadHas(rob, kateID, "are you ignoring me?") || surfaced.Load() != 0 {
		t.Fatal("a blocked contact's new message was surfaced")
	}
	s, ok := summaryFor(rob, kateID)
	if !ok {
		t.Fatal("blocking must keep the conversation")
	}
	if !s.Blocked || s.Request || s.Unread != 0 {
		t.Fatalf("expected a blocked conversation with nothing unread, got %+v", s)
	}
	if n := len(rob.Thread(kateID)); n != 2 {
		t.Fatalf("blocking must keep past history, got %d messages", n)
	}
}

// A stranger blocked before they write never becomes a request: no
// callback, nothing in the list. (Blocking someone pre-emptively, by ID,
// is something the terminal client allows.)
func TestBlockedStrangersKnockIsIgnored(t *testing.T) {
	rob := open(t, t.TempDir())
	defer rob.Close()
	kate := open(t, t.TempDir(), rob.Peer.LocalAddr())
	defer kate.Close()
	time.Sleep(200 * time.Millisecond)

	requested := make(chan string, 4)
	rob.OnRequest = func(peerID string) { requested <- peerID }

	if err := rob.Block(kate.Username()); err != nil {
		t.Fatal(err)
	}
	if len(rob.Conversations()) != 0 {
		t.Fatal("blocking someone who never wrote shouldn't create a conversation")
	}
	if _, err := kate.Add(rob.Username(), "rob"); err != nil {
		t.Fatal(err)
	}
	if _, err := kate.Send(rob.Username(), "hi, bob gave me your name"); err != nil {
		t.Fatal(err)
	}

	select {
	case <-requested:
		t.Fatal("a blocked stranger's knock fired a request callback")
	case <-time.After(2 * time.Second):
	}
	rob.Inbox.PollAll()
	for _, s := range rob.Conversations() {
		if s.PeerID == kate.Username() || s.Request {
			t.Fatalf("a blocked stranger showed up in the list: %+v", s)
		}
	}

	// Unblocking lets their knock through as an ordinary request.
	if err := rob.Unblock(kate.Username()); err != nil {
		t.Fatal(err)
	}
	if !waitPolled(rob, 6*time.Second, func() bool { s, ok := summaryFor(rob, kate.Username()); return ok && s.Request }) {
		t.Fatal("after unblocking, kate's request should come through")
	}
}

// A block is a standing decision: it outlives a restart, and unblocking
// brings back normal delivery -- for what's sent from then on. What they
// sent during the block stays unseen.
func TestBlockSurvivesRestartAndUnblockRestoresDelivery(t *testing.T) {
	robDir := t.TempDir()
	rob := open(t, robDir)
	kate := open(t, t.TempDir(), rob.Peer.LocalAddr())
	defer kate.Close()
	time.Sleep(200 * time.Millisecond)

	kateID := contacts(t, rob, kate)
	if _, err := rob.Send(kateID, "hi kate"); err != nil {
		t.Fatal(err)
	}
	if _, err := kate.Send(rob.Username(), "hi rob"); err != nil {
		t.Fatal(err)
	}
	if !waitFor(5*time.Second, func() bool { return threadHas(rob, kateID, "hi rob") }) {
		t.Fatal("kate's message never arrived")
	}
	if err := rob.Block(kateID); err != nil {
		t.Fatal(err)
	}

	rob.Close()
	rob = open(t, robDir, kate.Peer.LocalAddr())
	defer rob.Close()
	if !rob.IsBlocked(kateID) {
		t.Fatal("the block was lost across a restart")
	}

	if _, err := kate.Send(rob.Username(), "sent while blocked"); err != nil {
		t.Fatal(err)
	}
	settle(rob)
	if threadHas(rob, kateID, "sent while blocked") {
		t.Fatal("a blocked contact's message surfaced after a restart")
	}

	if err := rob.Unblock(kateID); err != nil {
		t.Fatal(err)
	}
	if _, err := kate.Send(rob.Username(), "sent after unblock"); err != nil {
		t.Fatal(err)
	}
	if !waitPolled(rob, 8*time.Second, func() bool { return threadHas(rob, kateID, "sent after unblock") }) {
		t.Fatal("unblocking should restore delivery")
	}
	rob.Inbox.PollAll()
	if threadHas(rob, kateID, "sent while blocked") {
		t.Fatal("unblocking revealed what was sent during the block")
	}
	if s, _ := summaryFor(rob, kateID); s.Blocked || s.Unread != 1 {
		t.Fatalf("expected an unblocked conversation with 1 unread, got %+v", s)
	}
}

// Blocking a pending request, then unblocking and accepting it, must not
// reveal what they sent in between. Accepting rewinds the read cursor to
// uncover what the pending cap held back, which would walk straight into
// the blocked-period messages if Unblock hadn't discarded them.
func TestAcceptAfterUnblockKeepsBlockedMessagesHidden(t *testing.T) {
	rob := open(t, t.TempDir())
	defer rob.Close()
	kate := open(t, t.TempDir(), rob.Peer.LocalAddr())
	defer kate.Close()
	time.Sleep(200 * time.Millisecond)

	if _, err := kate.Add(rob.Username(), "rob"); err != nil {
		t.Fatal(err)
	}
	for _, text := range []string{"1", "2"} {
		if _, err := kate.Send(rob.Username(), text); err != nil {
			t.Fatal(err)
		}
	}
	if !waitFor(5*time.Second, func() bool { return len(rob.Thread(kate.Username())) == 2 }) {
		t.Fatal("kate's request never arrived")
	}

	if err := rob.Block(kate.Username()); err != nil {
		t.Fatal(err)
	}
	if err := rob.Accept(kate.Username()); err == nil || !strings.Contains(err.Error(), "unblock") {
		t.Fatalf("accepting a blocked request should ask to unblock first, got %v", err)
	}
	for _, text := range []string{"3", "4", "5"} {
		if _, err := kate.Send(rob.Username(), text); err != nil {
			t.Fatal(err)
		}
	}
	time.Sleep(500 * time.Millisecond)

	if err := rob.Unblock(kate.Username()); err != nil {
		t.Fatal(err)
	}
	if s, _ := summaryFor(rob, kate.Username()); !s.Request {
		t.Fatal("an unblocked request should be a request again")
	}
	if err := rob.Accept(kate.Username()); err != nil {
		t.Fatal(err)
	}
	if _, err := kate.Send(rob.Username(), "6"); err != nil {
		t.Fatal(err)
	}
	if !waitPolled(rob, 8*time.Second, func() bool { return threadHas(rob, kate.Username(), "6") }) {
		t.Fatal("a message after accepting never arrived")
	}
	var got []string
	for _, m := range rob.Thread(kate.Username()) {
		got = append(got, m.Text)
	}
	if strings.Join(got, ",") != "1,2,6" {
		t.Fatalf("expected 1,2,6 -- nothing from the block -- got %v", got)
	}
}

// You can't write to someone you've blocked; you have to unblock them
// first. Refusing needs no network, so this runs on one node.
func TestBlockedPeerCannotBeWrittenTo(t *testing.T) {
	rob := open(t, t.TempDir())
	defer rob.Close()
	peer := strings.Repeat("ab", 32)

	if err := rob.Block(peer); err != nil {
		t.Fatal(err)
	}
	if _, err := rob.Send(peer, "hello?"); err == nil || !strings.Contains(err.Error(), "unblock them first") {
		t.Fatalf("sending to a blocked peer should be refused with a clear reason, got %v", err)
	}
	if err := rob.Block(rob.Username()); err == nil {
		t.Fatal("blocking yourself should be refused")
	}
	if err := rob.Block("bob"); err == nil {
		t.Fatal("blocking something that isn't a username should be refused")
	}
	if err := rob.Unblock(peer); err != nil || rob.IsBlocked(peer) {
		t.Fatalf("unblock failed: %v", err)
	}
}

// Unread counts what genuinely arrived: it goes up on arrival, never for
// your own messages or a blocked peer's, clears when read, and survives
// a restart.
func TestUnreadCounts(t *testing.T) {
	robDir := t.TempDir()
	rob := open(t, robDir)
	kate := open(t, t.TempDir(), rob.Peer.LocalAddr())
	defer kate.Close()
	time.Sleep(200 * time.Millisecond)

	unread := func() int { s, _ := summaryFor(rob, kate.Username()); return s.Unread }

	kateID := contacts(t, rob, kate)
	for _, text := range []string{"one", "two"} {
		if _, err := kate.Send(rob.Username(), text); err != nil {
			t.Fatal(err)
		}
	}
	if !waitFor(5*time.Second, func() bool { return unread() == 2 }) {
		t.Fatalf("expected 2 unread, got %d", unread())
	}
	if _, err := rob.Send(kateID, "reply"); err != nil {
		t.Fatal(err)
	}
	if n := unread(); n != 2 {
		t.Fatalf("your own message changed the unread count to %d", n)
	}
	if err := rob.MarkRead(kateID); err != nil {
		t.Fatal(err)
	}
	if n := unread(); n != 0 {
		t.Fatalf("MarkRead left %d unread", n)
	}

	if _, err := kate.Send(rob.Username(), "three"); err != nil {
		t.Fatal(err)
	}
	if !waitFor(5*time.Second, func() bool { return unread() == 1 }) {
		t.Fatalf("expected 1 unread, got %d", unread())
	}

	rob.Close()
	rob = open(t, robDir, kate.Peer.LocalAddr())
	defer rob.Close()
	time.Sleep(time.Second) // let Start's catch-up pass re-fetch everything
	if n := unread(); n != 1 {
		t.Fatalf("unread count after restart: got %d, want 1", n)
	}

	if err := rob.Block(kateID); err != nil {
		t.Fatal(err)
	}
	if _, err := kate.Send(rob.Username(), "four"); err != nil {
		t.Fatal(err)
	}
	settle(rob)
	if n := unread(); n != 0 {
		t.Fatalf("a blocked peer's message counted as unread: %d", n)
	}
}

// Adding someone who had already written as a request reveals what the
// pending cap was holding back, the same as accepting them would.
func TestAddingARequesterRevealsHeldBackMessages(t *testing.T) {
	rob := open(t, t.TempDir())
	defer rob.Close()
	kate := open(t, t.TempDir(), rob.Peer.LocalAddr())
	defer kate.Close()
	time.Sleep(200 * time.Millisecond)

	if _, err := kate.Add(rob.Username(), "rob"); err != nil {
		t.Fatal(err)
	}
	for _, text := range []string{"1", "2", "3", "4", "5"} {
		if _, err := kate.Send(rob.Username(), text); err != nil {
			t.Fatal(err)
		}
	}
	if !waitFor(5*time.Second, func() bool { return len(rob.Thread(kate.Username())) == 3 }) {
		t.Fatalf("expected the first 3 while pending, got %d", len(rob.Thread(kate.Username())))
	}
	time.Sleep(500 * time.Millisecond)

	if _, err := rob.Add(kate.Username(), "kate"); err != nil {
		t.Fatal(err)
	}
	if !waitFor(5*time.Second, func() bool { return len(rob.Thread(kate.Username())) == 5 }) {
		t.Fatalf("adding a requester should reveal all 5 messages, got %d", len(rob.Thread(kate.Username())))
	}
}
