package engine

import (
	"sync"
	"testing"
	"time"

	"altnet-relay/mailbox"
)

func msg(id, text string) *mailbox.Message {
	return &mailbox.Message{ID: id, Kind: mailbox.KindText, Text: text, Sent: time.Now().Unix()}
}

func TestDeliveredComesBackAndSticks(t *testing.T) {
	rob, kate, robID, kateID, _ := twoFriends(t)

	var mu sync.Mutex
	var told []string
	rob.OnDelivered = func(peerID string, ids []string) {
		mu.Lock()
		told = append(told, ids...)
		mu.Unlock()
	}
	m, err := rob.Send(kateID, "did you get this?")
	if err != nil {
		t.Fatal(err)
	}
	delivered := func() bool {
		for _, x := range rob.Thread(kateID) {
			if x.ID == m.ID {
				return x.Delivered
			}
		}
		return false
	}
	// Either the receipt landed after our record (OnDelivered fires), or it
	// overtook it and Send already returned the message as delivered.
	if !waitFor(8*time.Second, func() bool { return m.Delivered || delivered() }) {
		t.Fatal("rob should learn his message reached kate's device")
	}
	if !threadHas(kate, robID, "did you get this?") {
		t.Fatal("kate should have the message")
	}
	if !delivered() {
		t.Fatal("delivered must be remembered in rob's history")
	}
}

func TestTypingReachesOnlyAcceptedContacts(t *testing.T) {
	rob, kate, robID, kateID, _ := twoFriends(t)
	var mu sync.Mutex
	typing := map[string]int{}
	rob.OnTyping = func(p string) { mu.Lock(); typing[p]++; mu.Unlock() }
	count := func(p string) int { mu.Lock(); defer mu.Unlock(); return typing[p] }

	kate.Typing(robID)
	if !waitFor(5*time.Second, func() bool { return count(kateID) > 0 }) {
		t.Fatal("rob should see kate typing")
	}

	// Once rob blocks kate, her typing no longer shows -- and she no longer
	// gets his receipts either.
	if err := rob.Block(kateID); err != nil {
		t.Fatal(err)
	}
	before := count(kateID)
	kate.Typing(robID)
	time.Sleep(800 * time.Millisecond)
	if count(kateID) != before {
		t.Fatal("a blocked person's typing must not show")
	}
}

func TestRequestsGetNoReceiptsAndStrangersCantType(t *testing.T) {
	rob := open(t, t.TempDir())
	defer rob.Close()
	kate := open(t, t.TempDir(), rob.Peer.LocalAddr())
	defer kate.Close()
	time.Sleep(200 * time.Millisecond)

	var mu sync.Mutex
	var gotReceipt, gotTyping bool
	rob.OnDelivered = func(string, []string) { mu.Lock(); gotReceipt = true; mu.Unlock() }
	kate.OnTyping = func(string) { mu.Lock(); gotTyping = true; mu.Unlock() }

	// Rob writes to Kate, who hasn't accepted him.
	kateID, err := rob.Add(kate.Username(), "kate")
	if err != nil {
		t.Fatal(err)
	}
	m, err := rob.Send(kateID, "hello stranger")
	if err != nil {
		t.Fatal(err)
	}
	if !waitPolled(kate, 8*time.Second, func() bool { return threadHas(kate, rob.Username(), "hello stranger") }) {
		t.Fatal("kate never got the request")
	}
	// Even if Rob somehow had a way to reach her, his typing doesn't show.
	rob.Typing(kateID)
	time.Sleep(800 * time.Millisecond)

	mu.Lock()
	defer mu.Unlock()
	if gotReceipt || m.Delivered {
		t.Fatal("a message request must not tell the sender it arrived")
	}
	if gotTyping {
		t.Fatal("a stranger's typing must not show")
	}
	for _, x := range rob.Thread(kateID) {
		if x.Delivered {
			t.Fatal("no message to an unaccepted person may be marked delivered")
		}
	}
}

// Delivered marks are only ever set on our own messages, for the IDs named.
func TestMarkDeliveredTouchesOnlyOutbound(t *testing.T) {
	e := open(t, t.TempDir())
	defer e.Close()
	p := "d0d0d0d0d0d0d0d0d0d0d0d0d0d0d0d0d0d0d0d0d0d0d0d0d0d0d0d0d0d0d0d0"
	e.State.RecordNew(p, true, msg("out1", "mine"))
	e.State.RecordNew(p, false, msg("in1", "theirs"))
	changed := e.State.MarkDelivered(p, []string{"out1", "in1", "nope"})
	if len(changed) != 1 || changed[0] != "out1" {
		t.Fatalf("changed = %v", changed)
	}
	if again := e.State.MarkDelivered(p, []string{"out1"}); len(again) != 0 {
		t.Fatal("marking twice should report nothing new")
	}
}
