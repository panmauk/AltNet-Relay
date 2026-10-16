package server

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"altnet-relay/mailbox"
)

// ─── automatic bans: three different people ───

// addPerson makes one more signed-in person for the rig, besides alice, bob
// and carol.
func (r *rig) addPerson(token string) {
	r.t.Helper()
	email := token + "@example.com"
	if err := r.srv.store.Update(func(d *fileData) error {
		d.Accounts[email] = &Account{Email: email}
		return nil
	}); err != nil {
		r.t.Fatal(err)
	}
	r.srv.auth.(*fakeVerifier).accounts[token] = Principal{Email: email}
}

func (r *rig) reportAs(token string, target *mailbox.Identity) (int, map[string]any) {
	r.t.Helper()
	return r.do("POST", "/v1/reports", token, map[string]any{"username": target.Username(), "reason": "harassment"})
}

func TestThreeDifferentReportersBanAnAccountAutomatically(t *testing.T) {
	r := newRig(t)
	r.addPerson("dave")
	victim := newIdentity(t)
	r.bind("bob", victim)

	for _, who := range []string{"alice", "carol"} {
		if code, _ := r.reportAs(who, victim); code != 200 {
			t.Fatalf("%s's report was refused: %d", who, code)
		}
	}
	// Two people is a complaint, not a ban.
	if r.banner.has(victim.Username()) {
		t.Fatal("two reports must not ban")
	}
	if code, _ := r.do("POST", "/v1/handle/refresh", "bob", nil); code == 403 {
		t.Fatal("the account must work normally until the third report")
	}

	if code, _ := r.reportAs("dave", victim); code != 200 {
		t.Fatalf("the third report was refused: %d", code)
	}
	if !r.banner.has(victim.Username()) {
		t.Fatal("three different people reporting the same account must ban it on the network")
	}
	// The banned person is told why, without being told who.
	code, me := r.do("GET", "/v1/me", "bob", nil)
	reason, _ := me["ban_reason"].(string)
	if code != 200 || me["banned"] != true || !strings.Contains(reason, "several different people") {
		t.Fatalf("the banned account should be told why: %d %v", code, me)
	}
	for _, leak := range []string{"alice", "carol", "dave"} {
		if strings.Contains(reason, leak) {
			t.Fatalf("the reason must not say who reported: %q", reason)
		}
	}
	if code, out := r.do("POST", "/v1/handle/refresh", "bob", nil); code != 403 || !strings.Contains(out["error"].(string), "abuse.report@panmox.org") {
		t.Fatalf("a banned account is refused, with the way to appeal: %d %v", code, out)
	}
	// The reports are settled, as decided automatically, and kept on record.
	_, all := r.do("GET", "/v1/admin/reports?status=banned", "admin", nil)
	if all["count"].(float64) != 3 {
		t.Fatalf("all three reports should be recorded as banned: %v", all)
	}
	for _, rep := range all["reports"].([]any) {
		if rep.(map[string]any)["decided_by"] != "automatic" {
			t.Fatalf("the decision should say it was automatic: %v", rep)
		}
	}
	if _, pending := r.do("GET", "/v1/admin/reports", "admin", nil); pending["count"].(float64) != 0 {
		t.Fatal("nothing should be left pending")
	}
}

func TestOnePersonReportingTwiceCountsOnce(t *testing.T) {
	r := newRig(t)
	victim := newIdentity(t)
	r.bind("bob", victim)
	now := time.Now()
	r.srv.now = func() time.Time { return now }
	r.reportAs("alice", victim)
	// A day later alice may report again (the one-pending-per-day rule has
	// passed), and carol reports once.
	now = now.Add(26 * time.Hour)
	if code, _ := r.reportAs("alice", victim); code != 200 {
		t.Fatalf("a repeat report after a day should be accepted: %d", code)
	}
	r.reportAs("carol", victim)
	if r.banner.has(victim.Username()) {
		t.Fatal("two different people, three reports, is still two people: no ban")
	}
}

func TestOldReportsDoNotCount(t *testing.T) {
	r := newRig(t)
	r.addPerson("dave")
	victim := newIdentity(t)
	r.bind("bob", victim)
	now := time.Now()
	r.srv.now = func() time.Time { return now }
	r.reportAs("alice", victim)
	r.reportAs("carol", victim)
	// Two months on, a third person reports: the first two have aged out.
	now = now.Add(AutoBanWindow + 24*time.Hour)
	r.reportAs("dave", victim)
	if r.banner.has(victim.Username()) {
		t.Fatal("reports older than the window must not add up to a ban")
	}
}

func TestDismissedReportsDoNotCount(t *testing.T) {
	r := newRig(t)
	r.addPerson("dave")
	victim := newIdentity(t)
	r.bind("bob", victim)
	_, first := r.reportAs("alice", victim)
	if code, _ := r.do("POST", "/v1/admin/reports/decide", "admin", map[string]any{"id": first["id"], "decision": "dismiss"}); code != 200 {
		t.Fatal("dismiss failed")
	}
	r.reportAs("carol", victim)
	r.reportAs("dave", victim)
	if r.banner.has(victim.Username()) {
		t.Fatal("a report an operator dismissed must not count towards a ban")
	}
}

// Someone an operator has cleared isn't banned again by reports that were
// already waiting when they were cleared.
func TestLiftingABanClearsEarlierReports(t *testing.T) {
	r := newRig(t)
	r.addPerson("dave")
	victim := newIdentity(t)
	r.bind("bob", victim)
	now := time.Now()
	r.srv.now = func() time.Time { return now }
	r.reportAs("alice", victim)
	r.reportAs("carol", victim)
	// The operator bans (and then, on appeal, lifts it).
	if code, _ := r.do("POST", "/v1/admin/ban", "admin", map[string]any{"username": victim.Username(), "note": "x"}); code != 200 {
		t.Fatal("manual ban failed")
	}
	now = now.Add(time.Hour)
	if code, _ := r.do("POST", "/v1/admin/unban", "admin", map[string]any{"username": victim.Username()}); code != 200 {
		t.Fatal("unban failed")
	}
	// The two earlier reports were settled when the ban went in; a third
	// person's report on its own mustn't ban.
	now = now.Add(time.Hour)
	r.reportAs("dave", victim)
	if r.banner.has(victim.Username()) {
		t.Fatal("reports from before the ban was lifted must not ban again")
	}
	// But genuinely new reports do, from three people.
	now = now.Add(26 * time.Hour)
	r.reportAs("alice", victim)
	r.reportAs("carol", victim)
	if !r.banner.has(victim.Username()) {
		t.Fatal("three people reporting after the lift should ban again")
	}
}

// Reports still waiting when a ban is lifted are the case that needs the
// clearing time: they were never settled, so only it keeps them from adding up.
func TestPendingReportsBeforeALiftDoNotBanAgain(t *testing.T) {
	r := newRig(t)
	r.addPerson("dave")
	victim := newIdentity(t)
	r.bind("bob", victim)
	now := time.Now()
	r.srv.now = func() time.Time { return now }
	r.reportAs("alice", victim)
	r.reportAs("carol", victim)
	// Banned and cleared through the account record alone (as when an admin
	// lifts a ban that was made some other way), leaving the reports pending.
	r.srv.store.Update(func(d *fileData) error {
		d.byUsername(victim.Username()).Banned = true
		return nil
	})
	now = now.Add(time.Hour)
	if code, _ := r.do("POST", "/v1/admin/unban", "admin", map[string]any{"username": victim.Username()}); code != 200 {
		t.Fatal("unban failed")
	}
	now = now.Add(time.Hour)
	r.reportAs("dave", victim)
	if r.banner.has(victim.Username()) {
		t.Fatal("two reports from before the lift plus one after must not ban")
	}
}

// If the network can't be reached when the third report arrives, nothing is
// recorded as banned, and the sweep finishes the job when it can.
func TestAutomaticBanIsRetriedWhenThePublishFailed(t *testing.T) {
	r := newRig(t)
	r.addPerson("dave")
	victim := newIdentity(t)
	r.bind("bob", victim)
	r.banner.fail = errors.New("network unreachable")
	for _, who := range []string{"alice", "carol", "dave"} {
		if code, _ := r.reportAs(who, victim); code != 200 {
			t.Fatalf("a report is accepted even when the network is down: %d", code)
		}
	}
	if code, _ := r.do("POST", "/v1/handle/refresh", "bob", nil); code == 403 {
		t.Fatal("the account must not be marked banned when the ban didn't publish")
	}
	if _, pending := r.do("GET", "/v1/admin/reports", "admin", nil); pending["count"].(float64) != 3 {
		t.Fatal("the three reports must stay pending")
	}
	r.banner.fail = nil
	r.srv.SweepReports()
	if !r.banner.has(victim.Username()) {
		t.Fatal("the sweep should publish the ban once the network is back")
	}
	if code, _ := r.do("POST", "/v1/handle/refresh", "bob", nil); code != 403 {
		t.Fatal("the account should now be banned")
	}
}

// A report about someone who is already banned is accepted and settled, not
// left in the queue.
func TestReportsAboutAlreadyBannedAccountsAreSettled(t *testing.T) {
	r := newRig(t)
	victim := newIdentity(t)
	r.bind("bob", victim)
	r.do("POST", "/v1/admin/ban", "admin", map[string]any{"username": victim.Username(), "note": "x"})
	if code, _ := r.reportAs("alice", victim); code != 200 {
		t.Fatalf("a late report should still be accepted: %d", code)
	}
	if _, pending := r.do("GET", "/v1/admin/reports", "admin", nil); pending["count"].(float64) != 0 {
		t.Fatal("a report about an already banned account should be settled")
	}
}

// Someone who has been banned can't get others banned in return.
func TestBannedAccountsReportsDoNotCount(t *testing.T) {
	r := newRig(t)
	r.addPerson("dave")
	victim := newIdentity(t)
	r.bind("bob", victim)
	// Carol has been banned; her report is accepted but doesn't count.
	r.bind("carol", newIdentity(t))
	r.srv.store.Update(func(d *fileData) error {
		d.Accounts["carol@example.com"].Banned = true
		return nil
	})
	for _, who := range []string{"alice", "carol", "dave"} {
		r.reportAs(who, victim)
	}
	if r.banner.has(victim.Username()) {
		t.Fatal("a report from a banned account must not count towards a ban")
	}
}

// ─── against spam: who may ban by reporting ───

// agedBy sets how long ago the server first saw each of these accounts.
func (r *rig) ageOf(token string, age time.Duration) {
	r.t.Helper()
	email := token + "@example.com"
	if err := r.srv.store.Update(func(d *fileData) error {
		a := d.accountFor(email)
		a.Since = time.Now().Add(-age).Unix()
		return nil
	}); err != nil {
		r.t.Fatal(err)
	}
}

func TestReportsFromBrandNewAccountsDoNotBanYet(t *testing.T) {
	r := newRig(t)
	r.addPerson("dave")
	r.srv.MinReporterAge = DefaultMinReporterAge
	victim := newIdentity(t)
	r.bind("bob", victim)
	for _, who := range []string{"alice", "carol", "dave"} {
		r.ageOf(who, time.Hour) // made an hour ago, for the occasion
	}
	for _, who := range []string{"alice", "carol", "dave"} {
		if code, _ := r.reportAs(who, victim); code != 200 {
			t.Fatalf("a new account's report is still accepted and kept: %d", code)
		}
	}
	if r.banner.has(victim.Username()) {
		t.Fatal("three accounts made an hour ago must not be able to ban anyone")
	}
	if _, pending := r.do("GET", "/v1/admin/reports", "admin", nil); pending["count"].(float64) != 3 {
		t.Fatal("their reports must still be waiting for a person to read")
	}
	// Once they have been around long enough, the same reports count.
	for _, who := range []string{"alice", "carol", "dave"} {
		r.ageOf(who, DefaultMinReporterAge+time.Hour)
	}
	r.srv.SweepReports()
	if !r.banner.has(victim.Username()) {
		t.Fatal("the same reports from established accounts should ban")
	}
}

func TestAccountsFromBeforeTheRuleCountAsEstablished(t *testing.T) {
	r := newRig(t)
	r.addPerson("dave")
	r.srv.MinReporterAge = DefaultMinReporterAge
	victim := newIdentity(t)
	r.bind("bob", victim)
	for _, who := range []string{"alice", "carol", "dave"} {
		r.srv.store.Update(func(d *fileData) error { d.accountFor(who + "@example.com").Since = 0; return nil })
		r.reportAs(who, victim)
	}
	if !r.banner.has(victim.Username()) {
		t.Fatal("accounts recorded before ages were kept are old ones")
	}
}

// Someone who reports one account after another isn't reporting abuse.
func TestPeopleWhoReportEveryoneDoNotCount(t *testing.T) {
	r := newRig(t)
	r.addPerson("dave")
	victim := newIdentity(t)
	r.bind("bob", victim)
	// Alice reports six other accounts first.
	for i := 0; i < MaxReportTargets+1; i++ {
		r.do("POST", "/v1/reports", "alice", map[string]any{"username": fmt.Sprintf("%064x", i+1), "reason": "x"})
	}
	for _, who := range []string{"alice", "carol", "dave"} {
		r.reportAs(who, victim)
	}
	if r.banner.has(victim.Username()) {
		t.Fatal("a serial reporter's report must not be one of the three")
	}
	// A third honest person does it: carol, dave and one more.
	r.addPerson("erin")
	r.reportAs("erin", victim)
	if !r.banner.has(victim.Username()) {
		t.Fatal("three people who aren't spamming reports still ban")
	}
}

// When a person finds an automatic ban was wrong, the people whose reports
// caused it lose the power to ban by reporting.
func TestOverturnedAutomaticBansStrikeTheReporters(t *testing.T) {
	r := newRig(t)
	r.addPerson("dave")
	strikes := func(token string) int {
		n := 0
		r.srv.store.View(func(d *fileData) { n = d.Accounts[token+"@example.com"].Strikes })
		return n
	}
	for _, who := range []string{"alice", "carol", "dave"} {
		r.bind(who, newIdentity(t))
	}
	ban := func() string {
		v := newIdentity(t)
		r.bind("bob"+fmt.Sprint(time.Now().UnixNano()), v) // somebody else each time
		for _, who := range []string{"alice", "carol", "dave"} {
			r.reportAs(who, v)
		}
		if !r.banner.has(v.Username()) {
			t.Fatal("three reports should have banned")
		}
		return v.Username()
	}
	first := ban()
	if code, _ := r.do("POST", "/v1/admin/unban", "admin", map[string]any{"username": first}); code != 200 {
		t.Fatalf("unban: %d", code)
	}
	if strikes("alice") != 1 || strikes("carol") != 1 || strikes("dave") != 1 {
		t.Fatalf("each reporter gets one strike: %d %d %d", strikes("alice"), strikes("carol"), strikes("dave"))
	}
	// Their reports are marked as overturned, not left looking like bans.
	_, over := r.do("GET", "/v1/admin/reports?status=overturned", "admin", nil)
	if over["count"].(float64) != 3 {
		t.Fatalf("the three reports should read as overturned: %v", over)
	}
	// A second time, and then they can't ban by themselves any more.
	second := ban()
	r.do("POST", "/v1/admin/unban", "admin", map[string]any{"username": second})
	v := newIdentity(t)
	r.bind("someone-else", v)
	for _, who := range []string{"alice", "carol", "dave"} {
		r.reportAs(who, v)
	}
	if r.banner.has(v.Username()) {
		t.Fatal("after two overturned bans, the same three people can't ban a third")
	}
	// Their reports are not thrown away: a person can still read them.
	if _, pending := r.do("GET", "/v1/admin/reports", "admin", nil); pending["count"].(float64) != 3 {
		t.Fatal("their reports must still be waiting for a person")
	}
}

// Lifting a ban an operator made by hand is not a verdict on anybody's reports.
func TestLiftingAManualBanStrikesNobody(t *testing.T) {
	r := newRig(t)
	victim := newIdentity(t)
	r.bind("bob", victim)
	r.bind("alice", newIdentity(t))
	_, rep := r.reportAs("alice", victim)
	r.do("POST", "/v1/admin/reports/decide", "admin", map[string]any{"id": rep["id"], "decision": "ban", "note": "yes"})
	r.do("POST", "/v1/admin/unban", "admin", map[string]any{"username": victim.Username()})
	n := -1
	r.srv.store.View(func(d *fileData) { n = d.Accounts["alice@example.com"].Strikes })
	if n != 0 {
		t.Fatalf("a ban a person decided and then lifted must not strike the reporter: %d", n)
	}
}
