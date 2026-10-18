package engine

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"altnet-relay/mailbox"
	"altnet-relay/state"
)

// trio starts rob, kate and zed on one network. Rob and Kate are contacts;
// Zed is a stranger to both.
func trio(t *testing.T) (rob, kate, zed *Engine) {
	t.Helper()
	rob = open(t, t.TempDir())
	t.Cleanup(rob.Close)
	kate = open(t, t.TempDir(), rob.Peer.LocalAddr())
	t.Cleanup(kate.Close)
	zed = open(t, t.TempDir(), rob.Peer.LocalAddr())
	t.Cleanup(zed.Close)
	time.Sleep(300 * time.Millisecond)
	contacts(t, rob, kate)
	return
}

func groupStatus(e *Engine, id string) string {
	g, ok := e.State.Group(id)
	if !ok {
		return ""
	}
	return g.Status
}

func groupHas(e *Engine, id, text string) bool {
	for _, m := range e.GroupThread(id) {
		if m.Text == text {
			return true
		}
	}
	return false
}

func waitInvited(t *testing.T, e *Engine, id string) {
	t.Helper()
	if !waitPolled(e, 10*time.Second, func() bool { return groupStatus(e, id) == state.GroupInvited }) {
		t.Fatalf("invitation never arrived (status %q)", groupStatus(e, id))
	}
}

func TestGroupChatBetweenStrangers(t *testing.T) {
	rob, kate, zed := trio(t)

	id, err := rob.CreateGroup("Friday", []string{kate.Username(), zed.Username()})
	if err != nil {
		t.Fatal(err)
	}
	waitInvited(t, kate, id)
	waitInvited(t, zed, id)

	// An invitation is not a message request, and the inviter doesn't turn
	// up as an empty chat.
	if _, ok := summaryFor(zed, rob.Username()); ok {
		t.Fatal("a group invitation must not appear as a chat or a request")
	}
	inv := zed.Groups()
	if len(inv) != 1 || inv[0].Status != state.GroupInvited || inv[0].Name != "Friday" || inv[0].Members != 3 {
		t.Fatalf("zed's invitation: %+v", inv)
	}

	// Nothing can be sent before joining.
	if _, err := zed.SendGroup(id, "early"); err == nil {
		t.Fatal("sending needs joining first")
	}
	if err := kate.JoinGroup(id); err != nil {
		t.Fatal(err)
	}
	if err := zed.JoinGroup(id); err != nil {
		t.Fatal(err)
	}

	if _, err := rob.SendGroup(id, "hello group"); err != nil {
		t.Fatal(err)
	}
	for _, e := range []*Engine{kate, zed} {
		if !waitPolled(e, 10*time.Second, func() bool { return groupHas(e, id, "hello group") }) {
			t.Fatal("a member never got the message")
		}
	}

	// Kate and Zed have never met. Kate's messages to the group must reach
	// Zed in full -- not capped at three like a stranger's request.
	for i := 1; i <= 5; i++ {
		if _, err := kate.SendGroup(id, "kate "+string(rune('0'+i))); err != nil {
			t.Fatal(err)
		}
	}
	if !waitPolled(zed, 15*time.Second, func() bool { return groupHas(zed, id, "kate 5") }) {
		t.Fatal("zed should get all five of kate's messages")
	}
	for i := 1; i <= 5; i++ {
		if !groupHas(zed, id, "kate "+string(rune('0'+i))) {
			t.Fatalf("zed is missing kate %d", i)
		}
	}
	// ...in order, and attributed to her.
	var seq []string
	for _, m := range zed.GroupThread(id) {
		if strings.HasPrefix(m.Text, "kate ") {
			if m.From != kate.Username() {
				t.Fatalf("message attributed to %s", m.From)
			}
			seq = append(seq, m.Text)
		}
	}
	if strings.Join(seq, ",") != "kate 1,kate 2,kate 3,kate 4,kate 5" {
		t.Fatalf("out of order: %v", seq)
	}
	// And none of this turned anyone into a chat in each other's list.
	if _, ok := summaryFor(zed, kate.Username()); ok {
		t.Fatal("a fellow member must not appear as a chat")
	}
	if g := zed.Groups(); g[0].Unread == 0 {
		t.Fatal("group messages should count as unread")
	}
}

func TestGroupMembershipChanges(t *testing.T) {
	rob, kate, zed := trio(t)
	id, err := rob.CreateGroup("Team", []string{kate.Username()})
	if err != nil {
		t.Fatal(err)
	}
	waitInvited(t, kate, id)
	if err := kate.JoinGroup(id); err != nil {
		t.Fatal(err)
	}

	// Only the admin adds people.
	if err := kate.AddToGroup(id, []string{zed.Username()}); err == nil {
		t.Fatal("only the admin can add people")
	}
	if err := rob.AddToGroup(id, []string{zed.Username()}); err != nil {
		t.Fatal(err)
	}
	waitInvited(t, zed, id)
	if !waitPolled(kate, 10*time.Second, func() bool { g, _ := kate.State.Group(id); return g.HasMember(zed.Username()) }) {
		t.Fatal("kate should learn zed was added")
	}
	if err := zed.JoinGroup(id); err != nil {
		t.Fatal(err)
	}

	// Removing someone tells them, and they stop getting messages.
	if err := rob.RemoveFromGroup(id, zed.Username()); err != nil {
		t.Fatal(err)
	}
	if !waitPolled(zed, 10*time.Second, func() bool { return groupStatus(zed, id) == state.GroupRemoved }) {
		t.Fatal("zed should be told he was removed")
	}
	if _, err := zed.SendGroup(id, "still here?"); err == nil {
		t.Fatal("a removed member can't send")
	}
	rob.SendGroup(id, "after zed")
	if !waitPolled(kate, 10*time.Second, func() bool { return groupHas(kate, id, "after zed") }) {
		t.Fatal("kate should still get messages")
	}
	settle(zed)
	if groupHas(zed, id, "after zed") {
		t.Fatal("a removed member must not get new messages")
	}

	// The admin leaving hands the group to the next member.
	if err := rob.LeaveGroup(id); err != nil {
		t.Fatal(err)
	}
	if !waitPolled(kate, 10*time.Second, func() bool {
		g, _ := kate.State.Group(id)
		return !g.HasMember(rob.Username()) && g.Admin == kate.Username()
	}) {
		g, _ := kate.State.Group(id)
		t.Fatalf("kate should be admin after rob leaves: %+v", g)
	}
}

func TestDecliningAnInvitationTakesYouOff(t *testing.T) {
	rob, kate, zed := trio(t)
	id, err := rob.CreateGroup("Maybe", []string{kate.Username(), zed.Username()})
	if err != nil {
		t.Fatal(err)
	}
	waitInvited(t, zed, id)
	if err := zed.DeclineGroup(id); err != nil {
		t.Fatal(err)
	}
	if len(zed.Groups()) != 0 {
		t.Fatal("a declined invitation shouldn't stay in the list")
	}
	if !waitPolled(rob, 10*time.Second, func() bool { g, _ := rob.State.Group(id); return !g.HasMember(zed.Username()) }) {
		t.Fatal("the admin should hear that zed declined")
	}
}

// Only members can post, only the admin changes membership, and a forged
// invitation (not from the group's admin) is ignored.
func TestGroupRulesHoldAgainstModifiedClients(t *testing.T) {
	rob, kate, zed := trio(t)
	id, err := rob.CreateGroup("Closed", []string{kate.Username()})
	if err != nil {
		t.Fatal(err)
	}
	waitInvited(t, kate, id)
	if err := kate.JoinGroup(id); err != nil {
		t.Fatal(err)
	}

	// Zed isn't a member but sends kate a group message for it anyway.
	zedConv := zed.State.Conversation(kate.Username())
	payload := `{"g":"` + id + `","op":"msg","mid":"forged1","text":"let me in"}`
	if _, _, err := zed.Inbox.SendKind(zedConv, mailbox.KindGroup, payload); err != nil {
		t.Fatal(err)
	}
	// Kate (a member, not the admin) tries to rewrite the membership.
	kateConv := kate.State.Conversation(rob.Username())
	update := `{"g":"` + id + `","op":"update","snap":{"name":"Mine now","admin":"` + kate.Username() +
		`","members":["` + kate.Username() + `","` + rob.Username() + `"],"v":99}}`
	if _, _, err := kate.Inbox.SendKind(kateConv, mailbox.KindGroup, update); err != nil {
		t.Fatal(err)
	}
	settle(kate)
	settle(rob)
	if groupHas(kate, id, "let me in") {
		t.Fatal("a non-member's message must be ignored")
	}
	if g, _ := rob.State.Group(id); g.Name != "Closed" || g.Admin != rob.Username() {
		t.Fatalf("a member must not be able to take the group over: %+v", g)
	}
}

func TestMembersDirectMessageIsARequest(t *testing.T) {
	rob, kate, zed := trio(t)
	id, err := rob.CreateGroup("Hi", []string{kate.Username(), zed.Username()})
	if err != nil {
		t.Fatal(err)
	}
	waitInvited(t, kate, id)
	waitInvited(t, zed, id)
	kate.JoinGroup(id)
	zed.JoinGroup(id)

	// Zed writes to Kate directly. They share a group, but that doesn't
	// make him a contact: it's a request.
	if _, err := zed.Add(kate.Username(), "kate"); err != nil {
		t.Fatal(err)
	}
	if _, err := zed.Send(kate.Username(), "hi kate, it's zed from the group"); err != nil {
		t.Fatal(err)
	}
	if !waitPolled(kate, 10*time.Second, func() bool { s, ok := summaryFor(kate, zed.Username()); return ok && s.Request }) {
		s, ok := summaryFor(kate, zed.Username())
		t.Fatalf("zed's direct message should be a request for kate: %+v %v", s, ok)
	}
	if err := kate.Accept(zed.Username()); err != nil {
		t.Fatal(err)
	}
	if s, _ := summaryFor(kate, zed.Username()); s.Request || !s.Accepted {
		t.Fatalf("after accepting, zed is a normal chat: %+v", s)
	}
}

// Anyone who knows your name can invite you, so someone who isn't a contact
// gets at most maxInvitesPerStranger invitations waiting at once. Contacts
// aren't limited.
func TestStrangersCanOnlyInviteSoOften(t *testing.T) {
	rob, kate, zed := trio(t)
	var fromZed, fromKate []string
	for i := 0; i < 3; i++ {
		id, err := zed.CreateGroup(fmt.Sprintf("zed %d", i), []string{rob.Username()})
		if err != nil {
			t.Fatal(err)
		}
		fromZed = append(fromZed, id)
		id, err = kate.CreateGroup(fmt.Sprintf("kate %d", i), []string{rob.Username()})
		if err != nil {
			t.Fatal(err)
		}
		fromKate = append(fromKate, id)
	}
	for _, id := range fromKate {
		waitInvited(t, rob, id)
	}
	invited := func() int {
		n := 0
		for _, id := range fromZed {
			if groupStatus(rob, id) == state.GroupInvited {
				n++
			}
		}
		return n
	}
	if !waitPolled(rob, 10*time.Second, func() bool { return invited() == maxInvitesPerStranger }) {
		t.Fatalf("from a stranger: %d invitations kept, want %d", invited(), maxInvitesPerStranger)
	}
	waitPolled(rob, 2*time.Second, func() bool { return false })
	if n := invited(); n != maxInvitesPerStranger {
		t.Fatalf("a stranger got %d invitations through", n)
	}
}
