package engine

import (
	"strings"
	"sync"
	"testing"
	"time"

	"altnet-relay/mailbox"
)

// groupOfThree makes a joined group of rob (admin), kate and zed, and has each
// of them send one message so everyone has learned where the others are.
func groupOfThree(t *testing.T) (rob, kate, zed *Engine, id string) {
	t.Helper()
	rob, kate, zed = trio(t)
	id, err := rob.CreateGroup("Crew", []string{kate.Username(), zed.Username()})
	if err != nil {
		t.Fatal(err)
	}
	waitInvited(t, kate, id)
	waitInvited(t, zed, id)
	if err := kate.JoinGroup(id); err != nil {
		t.Fatal(err)
	}
	if err := zed.JoinGroup(id); err != nil {
		t.Fatal(err)
	}
	for _, e := range []*Engine{rob, kate, zed} {
		if _, err := e.SendGroup(id, "hi from "+e.Username()[:6]); err != nil {
			t.Fatal(err)
		}
	}
	for _, e := range []*Engine{rob, kate, zed} {
		for _, other := range []*Engine{rob, kate, zed} {
			text := "hi from " + other.Username()[:6]
			if !waitPolled(e, 10*time.Second, func() bool { return groupHas(e, id, text) }) {
				t.Fatalf("hello never went round the group")
			}
		}
	}
	return
}

func TestRenameGroup(t *testing.T) {
	rob, kate, _, id := groupOfThree(t)
	if err := kate.RenameGroup(id, "Kate's now"); err == nil {
		t.Fatal("only the admin can rename")
	}
	if err := rob.RenameGroup(id, "Crew 2"); err != nil {
		t.Fatal(err)
	}
	if !waitPolled(kate, 10*time.Second, func() bool { g, _ := kate.State.Group(id); return g.Name == "Crew 2" }) {
		t.Fatal("kate should see the new name")
	}
	noted := func() bool {
		for _, m := range kate.GroupThread(id) {
			if m.System && strings.Contains(m.Text, "renamed the group to Crew 2") {
				return true
			}
		}
		return false
	}
	if !waitFor(3*time.Second, noted) {
		t.Fatal("the rename should be noted in the group")
	}
}

func TestGroupTypingAndDeliveredCounts(t *testing.T) {
	rob, kate, zed, id := groupOfThree(t)

	var mu sync.Mutex
	typing := map[string]string{}
	rob.OnGroupTyping = func(g, from string) { mu.Lock(); typing[g] = from; mu.Unlock() }
	kate.TypingGroup(id)
	if !waitFor(5*time.Second, func() bool { mu.Lock(); defer mu.Unlock(); return typing[id] == kate.Username() }) {
		t.Fatal("rob should see kate typing in the group")
	}

	m, err := rob.SendGroup(id, "who's in?")
	if err != nil {
		t.Fatal(err)
	}
	reached := func() int {
		for _, x := range rob.GroupThread(id) {
			if x.ID == m.ID {
				return len(x.DeliveredTo)
			}
		}
		return 0
	}
	if !waitFor(10*time.Second, func() bool { return reached() == 2 }) {
		t.Fatalf("rob's message should be confirmed by both kate and zed, got %d", reached())
	}
	_ = zed
}

func TestGroupCallSignalsReachMembersOnly(t *testing.T) {
	rob, kate, zed, id := groupOfThree(t)
	var mu sync.Mutex
	got := map[string][]CallSignal{}
	sink := func(name string) func(CallSignal) {
		return func(s CallSignal) { mu.Lock(); got[name] = append(got[name], s); mu.Unlock() }
	}
	kate.OnCall, zed.OnCall, rob.OnCall = sink("kate"), sink("zed"), sink("rob")
	has := func(name, op string) (CallSignal, bool) {
		mu.Lock()
		defer mu.Unlock()
		for _, s := range got[name] {
			if s.Op == op {
				return s, true
			}
		}
		return CallSignal{}, false
	}

	// Rob rings the group: everyone hears it, as a group call.
	if err := rob.SendGroupCall(CallSignal{Group: id, Op: GroupRing, CallID: "gc1"}); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"kate", "zed"} {
		e := map[string]*Engine{"kate": kate, "zed": zed}[name]
		if !waitPolled(e, 8*time.Second, func() bool { _, ok := has(name, GroupRing); return ok }) {
			t.Fatalf("%s didn't hear the ring", name)
		}
		s, _ := has(name, GroupRing)
		if s.Group != id || s.PeerID != rob.Username() {
			t.Fatalf("%s: ring arrived wrong: %+v", name, s)
		}
	}

	// A connection offer goes to one member only.
	if err := kate.SendGroupCall(CallSignal{Group: id, PeerID: zed.Username(), Op: CallOffer, CallID: "gc1", SDP: "sdp-k2z"}); err != nil {
		t.Fatal(err)
	}
	if !waitPolled(zed, 8*time.Second, func() bool { s, ok := has("zed", CallOffer); return ok && s.SDP == "sdp-k2z" }) {
		t.Fatal("zed should get kate's offer")
	}
	settle(rob)
	if _, ok := has("rob", CallOffer); ok {
		t.Fatal("an offer meant for zed must not reach rob")
	}

	// Someone outside the group can't ring it, even with a hand-made message.
	outsider := open(t, t.TempDir(), rob.Peer.LocalAddr())
	defer outsider.Close()
	time.Sleep(200 * time.Millisecond)
	if err := outsider.SendGroupCall(CallSignal{Group: id, Op: GroupRing, CallID: "x"}); err == nil {
		t.Fatal("you can't ring a group you're not in")
	}
	conv := outsider.State.Conversation(kate.Username())
	if _, _, err := outsider.Inbox.SendKind(conv, mailbox.KindCall, `{"op":"ring","call":"evil","g":"`+id+`"}`); err != nil {
		t.Fatal(err)
	}
	settle(kate)
	mu.Lock()
	defer mu.Unlock()
	for _, s := range got["kate"] {
		if s.CallID == "evil" {
			t.Fatal("a non-member's ring must be ignored")
		}
	}
}
