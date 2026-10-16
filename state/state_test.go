package state

import (
	"testing"

	"altnet-relay/mailbox"
)

func msg(id, text string, sent int64) *mailbox.Message {
	return &mailbox.Message{ID: id, Kind: mailbox.KindText, Text: text, Sent: sent}
}

func TestStateSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	s, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}

	peer := "aa" + repeat("b", 62)
	if err := s.AddContact("rob", peer); err != nil {
		t.Fatal(err)
	}
	conv := s.Conversation(peer)
	conv.Accepted = true
	conv.LastReadHash = "deadbeef"
	conv.LastSentSeq = 4
	s.Record(peer, false, msg("m1", "hello", 100))
	s.Record(peer, true, msg("m2", "hi back", 200))
	if err := s.Save(); err != nil {
		t.Fatal(err)
	}

	// Reopen, as if the app had been closed and started again.
	again, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := again.Nickname(peer); got != "rob" {
		t.Fatalf("nickname lost: %q", got)
	}
	restored := again.Conversation(peer)
	if !restored.Accepted {
		t.Error("accepted flag lost across restart")
	}
	if restored.LastReadHash != "deadbeef" {
		t.Error("read cursor lost across restart -- catch-up would re-show old messages")
	}
	if restored.LastSentSeq != 4 {
		t.Error("send sequence lost across restart -- the next note would break the chain")
	}
	if n := len(again.Thread(peer)); n != 2 {
		t.Fatalf("history lost: %d messages", n)
	}
}

// Re-recording a message already in history must not duplicate it, even
// in a fresh process where the in-memory dedup set started empty.
func TestRecordDedupsAcrossRestart(t *testing.T) {
	dir := t.TempDir()
	s, _ := Load(dir)
	peer := "cc" + repeat("d", 62)

	s.Record(peer, false, msg("same-id", "only once", 10))
	if err := s.Save(); err != nil {
		t.Fatal(err)
	}

	again, _ := Load(dir)
	added := again.Record(peer, false, msg("same-id", "only once", 10))
	if added != 0 {
		t.Fatal("a message already in history was appended again")
	}
	if n := len(again.Thread(peer)); n != 1 {
		t.Fatalf("expected 1 message, got %d", n)
	}
}

// Both directions merge into one ordered thread.
func TestThreadMergesBothDirectionsInOrder(t *testing.T) {
	s, _ := Load(t.TempDir())
	peer := "ee" + repeat("f", 62)

	s.Record(peer, false, msg("a", "their first", 100))
	s.Record(peer, true, msg("b", "our reply", 150))
	s.Record(peer, false, msg("c", "their second", 200))

	thread := s.Thread(peer)
	want := []string{"their first", "our reply", "their second"}
	if len(thread) != len(want) {
		t.Fatalf("expected %d messages, got %d", len(want), len(thread))
	}
	for i, w := range want {
		if thread[i].Text != w {
			t.Fatalf("position %d: got %q want %q", i, thread[i].Text, w)
		}
	}
	if !thread[1].Outbound {
		t.Error("our own reply should be marked outbound")
	}
	if thread[0].Outbound {
		t.Error("their message should not be marked outbound")
	}
}

// A sender whose clock runs fast (or who lies) must not be able to pin
// their message to the bottom of the thread forever. The sent time is
// clamped to when we logged it, so anything logged later still sorts
// after it.
func TestFutureDatedMessageIsClamped(t *testing.T) {
	const logged = 1700000000

	liar := StoredMessage{ID: "liar", Sent: 32503680000, Logged: logged} // claims year 3000
	if got := orderKey(liar); got != logged {
		t.Fatalf("future-dated message should clamp to %d, got %d", logged, got)
	}

	// An honest message keeps its own timestamp.
	honest := StoredMessage{ID: "honest", Sent: logged - 1000, Logged: logged}
	if got := orderKey(honest); got != logged-1000 {
		t.Fatalf("honest message should keep its sent time, got %d", got)
	}

	// The point of clamping: a message logged later still sorts after
	// the liar, instead of being buried under a year-3000 timestamp.
	later := StoredMessage{ID: "later", Sent: logged + 500, Logged: logged + 500}
	if orderKey(later) <= orderKey(liar) {
		t.Fatal("clamping must let a later message sort after a future-dated one")
	}
}

// Commands accept either a nickname or a full username.
func TestResolveAcceptsNicknameOrUsername(t *testing.T) {
	s, _ := Load(t.TempDir())
	peer := "ab" + repeat("c", 62)
	if err := s.AddContact("rob", peer); err != nil {
		t.Fatal(err)
	}

	if got, ok := s.Resolve("rob"); !ok || got != peer {
		t.Fatalf("nickname lookup failed: %q %v", got, ok)
	}
	if got, ok := s.Resolve(peer); !ok || got != peer {
		t.Fatalf("username lookup failed: %q %v", got, ok)
	}
	if _, ok := s.Resolve("nobody"); ok {
		t.Fatal("unknown nickname should not resolve")
	}
}

func TestDuplicateNicknameRejected(t *testing.T) {
	s, _ := Load(t.TempDir())
	if err := s.AddContact("rob", "aa"+repeat("1", 62)); err != nil {
		t.Fatal(err)
	}
	if err := s.AddContact("rob", "bb"+repeat("2", 62)); err == nil {
		t.Fatal("expected a duplicate nickname to be rejected")
	}
}

func TestShortUsernameRejected(t *testing.T) {
	s, _ := Load(t.TempDir())
	if err := s.AddContact("rob", "tooshort"); err == nil {
		t.Fatal("expected a malformed username to be rejected")
	}
}

func repeat(s string, n int) string {
	out := make([]byte, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, s[0])
	}
	return string(out)
}

// A burst of messages sent within the same second all share a timestamp,
// so the thread must keep them in the order they were recorded. IDs are
// random and deliberately chosen here to sort opposite to the true order,
// which is how the old ID tie-break shuffled real bursts.
func TestSameSecondBurstKeepsRecordedOrder(t *testing.T) {
	dir := t.TempDir()
	s, _ := Load(dir)
	peer := "ba" + repeat("4", 62)

	s.Record(peer, false,
		msg("zzz", "hi", 100),
		msg("mmm", "you there?", 100),
		msg("aaa", "?", 100),
	)
	want := []string{"hi", "you there?", "?"}
	check := func(label string, st *State) {
		thread := st.Thread(peer)
		for i, w := range want {
			if thread[i].Text != w {
				t.Fatalf("%s: position %d got %q want %q", label, i, thread[i].Text, w)
			}
		}
	}
	check("fresh", s)

	// And across a restart, since history order is what carries it.
	if err := s.Save(); err != nil {
		t.Fatal(err)
	}
	again, _ := Load(dir)
	check("after restart", again)
}

// A block is a standing decision, so it has to outlive the process --
// otherwise every restart would quietly let the person back in.
func TestBlockSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	s, _ := Load(dir)
	peer := "ab" + repeat("9", 62)

	s.Record(peer, false, msg("h1", "history from before", 10))
	s.Block(peer)
	if err := s.Save(); err != nil {
		t.Fatal(err)
	}

	again, _ := Load(dir)
	if !again.IsBlocked(peer) {
		t.Fatal("block lost across restart")
	}
	if n := len(again.Thread(peer)); n != 1 {
		t.Fatalf("blocking must keep past history, got %d messages", n)
	}

	again.Unblock(peer)
	if again.IsBlocked(peer) {
		t.Fatal("unblock didn't take")
	}
}

// Unread counts only what is genuinely new and inbound, and survives a
// restart -- messages that arrived while the app was closed are the ones
// most worth pointing at.
func TestUnreadCountsOnlyNewInbound(t *testing.T) {
	dir := t.TempDir()
	s, _ := Load(dir)
	peer := "cd" + repeat("7", 62)

	s.Record(peer, false, msg("in1", "one", 10), msg("in2", "two", 11))
	if got := s.Unread(peer); got != 2 {
		t.Fatalf("expected 2 unread, got %d", got)
	}
	// The same message again (pushed directly, then found on the chain)
	// must not count twice.
	s.Record(peer, false, msg("in2", "two", 11))
	if got := s.Unread(peer); got != 2 {
		t.Fatalf("a re-delivered message was counted again: %d", got)
	}
	// Our own messages never count.
	s.Record(peer, true, msg("out1", "reply", 12))
	if got := s.Unread(peer); got != 2 {
		t.Fatalf("our own message changed the unread count: %d", got)
	}

	if !s.MarkRead(peer) {
		t.Fatal("MarkRead should report that it cleared something")
	}
	if got := s.Unread(peer); got != 0 {
		t.Fatalf("MarkRead left %d unread", got)
	}
	if s.MarkRead(peer) {
		t.Fatal("MarkRead on an already-read conversation should be a no-op")
	}

	s.Record(peer, false, msg("in3", "three", 13))
	if err := s.Save(); err != nil {
		t.Fatal(err)
	}
	again, _ := Load(dir)
	if got := again.Unread(peer); got != 1 {
		t.Fatalf("unread count lost across restart: %d", got)
	}
	// Re-fetching after a restart (the notify layer's dedup starts empty)
	// must not inflate it either.
	again.Record(peer, false, msg("in3", "three", 13))
	if got := again.Unread(peer); got != 1 {
		t.Fatalf("a message re-fetched after restart was counted again: %d", got)
	}
}

// Nothing from a blocked peer counts as unread, and blocking clears what
// was already waiting.
func TestBlockedPeerNeverCountsAsUnread(t *testing.T) {
	s, _ := Load(t.TempDir())
	peer := "ef" + repeat("5", 62)

	s.Record(peer, false, msg("a", "before", 10))
	s.Block(peer)
	if got := s.Unread(peer); got != 0 {
		t.Fatalf("blocking should clear the unread count, got %d", got)
	}
	s.Record(peer, false, msg("b", "while blocked", 11))
	if got := s.Unread(peer); got != 0 {
		t.Fatalf("a blocked peer's message was counted as unread: %d", got)
	}

	s.Unblock(peer)
	s.Record(peer, false, msg("c", "after", 12))
	if got := s.Unread(peer); got != 1 {
		t.Fatalf("after unblocking, new messages should count again: %d", got)
	}
}

// Declining forgets the conversation, so it must take the count with it.
func TestDeclineClearsUnread(t *testing.T) {
	s, _ := Load(t.TempDir())
	peer := "0a" + repeat("3", 62)

	s.Record(peer, false, msg("r1", "buy my thing", 10))
	s.Decline(peer)
	if got := s.Unread(peer); got != 0 {
		t.Fatalf("a declined request left %d unread behind", got)
	}
}

// Messages a blocked peer sent are discarded, not just hidden: a later
// re-walk of their chain (accepting a request rewinds the read cursor)
// must not bring them back, including after a restart.
func TestDiscardedMessagesNeverComeBack(t *testing.T) {
	dir := t.TempDir()
	s, _ := Load(dir)
	peer := "1b" + repeat("4", 62)

	s.Record(peer, false, msg("shown", "before the block", 10))
	s.Discard(peer, msg("shown", "before the block", 10), msg("gone", "while blocked", 11))
	if n := s.Record(peer, false, msg("gone", "while blocked", 11)); n != 0 {
		t.Fatal("a discarded message was recorded")
	}
	if n := len(s.Thread(peer)); n != 1 {
		t.Fatalf("discarding must leave shown history alone, got %d messages", n)
	}
	if got := s.Unread(peer); got != 1 {
		t.Fatalf("a discarded message changed the unread count: %d", got)
	}
	if err := s.Save(); err != nil {
		t.Fatal(err)
	}

	again, _ := Load(dir)
	if n := again.Record(peer, false, msg("gone", "while blocked", 11)); n != 0 {
		t.Fatal("a discarded message was recorded after a restart")
	}

	// Declining forgets everything about them, discards included, so
	// adding them on purpose later starts from a clean slate.
	again.Decline(peer)
	if n := again.Record(peer, false, msg("gone", "while blocked", 11)); n != 1 {
		t.Fatal("declining should forget discarded IDs along with the history")
	}
}

// Looking a conversation up must not create one.
func TestLookupConversationDoesNotCreate(t *testing.T) {
	s, _ := Load(t.TempDir())
	peer := "2c" + repeat("6", 62)
	if _, ok := s.LookupConversation(peer); ok {
		t.Fatal("found a conversation that was never created")
	}
	if len(s.Conversations()) != 0 {
		t.Fatal("LookupConversation created a conversation")
	}
	s.Conversation(peer)
	if _, ok := s.LookupConversation(peer); !ok {
		t.Fatal("an existing conversation wasn't found")
	}
}
