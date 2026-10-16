package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	mrand "math/rand/v2"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"altnet-relay/account"
	"altnet-relay/mailbox"
)

// Gate says whether the person may use Relay right now. It is what the app
// window turns into a screen: the sign-in form, the banned notice, or the
// chats themselves.
type Gate string

const (
	// GateOff means accounts aren't configured (a local development
	// network). Nothing is gated.
	GateOff Gate = "off"
	// GateSignedOut means there is no valid session: a first launch, a
	// sign-out, or a session the accounts service no longer accepts.
	GateSignedOut Gate = "signed_out"
	// GateOK means signed in and in good standing.
	GateOK Gate = "ok"
	// GateBanned means the account is suspended or banned. Reason says why.
	GateBanned Gate = "banned"
)

// AccountStatus is a snapshot of the sign-in state, for the UI.
type AccountStatus struct {
	Gate    Gate
	Email   string
	Handle  string
	Reason  string // for GateBanned: the statement of reasons shown to the person
	Offline bool   // the account service couldn't be reached at the last check
}

var (
	errSignedOut = errors.New("sign in to your AltNet account first")
	errNoAccount = errors.New("accounts aren't available in this build")
)

// accountFile is where the session token lives, in the data directory. The
// password is never written anywhere.
const accountFile = "account.json"

// AccountCheckInterval is how often the signed-in account is re-checked
// with the server: still valid, not banned, name still signed.
const AccountCheckInterval = 10 * time.Minute

// handleRefreshEvery is how often the signed name is re-requested while the
// app stays open. The server's signature lasts 90 days; a day is generous.
const handleRefreshEvery = 24 * time.Hour

// accountTimeout bounds each call to the account service so a dead network
// never freezes the window.
const accountTimeout = 20 * time.Second

// Account returns the current sign-in state.
func (e *Engine) Account() AccountStatus {
	e.acctMu.Lock()
	defer e.acctMu.Unlock()
	s := AccountStatus{Gate: e.gate, Reason: e.gateReason, Offline: e.acctOffline, Handle: e.State.MyHandle()}
	if e.sess != nil {
		s.Email = e.sess.Email
	}
	return s
}

// allowed is the gate on everything that reaches out to other people. Receiving
// and safety tools (block, decline, report) are never gated.
func (e *Engine) allowed() error {
	e.acctMu.Lock()
	gate, reason := e.gate, e.gateReason
	e.acctMu.Unlock()
	switch gate {
	case GateSignedOut:
		if reason != "" {
			return errors.New(reason)
		}
		return errSignedOut
	case GateBanned:
		if reason != "" {
			return errors.New(reason)
		}
		return errBanned
	}
	// Before reaching out, make sure (now and then) that another computer
	// hasn't taken the account over.
	return e.stillHere()
}

// loadSession reads the saved sign-in, if any. Called once from Open.
func (e *Engine) loadSession() {
	e.gate = GateOff
	if e.acct == nil {
		return
	}
	e.gate = GateSignedOut
	blob, err := os.ReadFile(filepath.Join(e.dataDir, accountFile))
	if err != nil {
		return
	}
	var s account.Session
	if json.Unmarshal(blob, &s) != nil || s.Token == "" {
		return
	}
	if s.ExpiresAt > 0 && time.Now().Unix() >= s.ExpiresAt {
		return // expired while the app was closed
	}
	e.sess = &s
	e.gate = GateOK // optimistic until the first check says otherwise
}

func (e *Engine) saveSession(s *account.Session) error {
	blob, err := json.Marshal(struct {
		Token     string `json:"token"`
		ExpiresAt int64  `json:"expires_at"`
		Email     string `json:"email"`
	}{s.Token, s.ExpiresAt, s.Email})
	if err != nil {
		return err
	}
	path := filepath.Join(e.dataDir, accountFile)
	if err := os.WriteFile(path+".tmp", blob, 0o600); err != nil {
		return err
	}
	return os.Rename(path+".tmp", path)
}

func (e *Engine) forgetSession() {
	e.sess = nil
	e.gate = GateSignedOut
	e.gateReason = ""
	_ = os.Remove(filepath.Join(e.dataDir, accountFile))
}

func (e *Engine) needAccounts() error {
	if e.acct == nil {
		return errNoAccount
	}
	return nil
}

// SignUp creates a Studio account. A verification code is emailed; enter it
// with VerifyEmail.
func (e *Engine) SignUp(email, password string) error {
	if err := e.needAccounts(); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), accountTimeout)
	defer cancel()
	if err := e.acct.Signup(ctx, strings.TrimSpace(email), password); err != nil {
		return err
	}
	// Kept in memory until the email is verified, to lock the account's copy.
	e.roam.mu.Lock()
	e.roam.pendingPW = password
	e.roam.mu.Unlock()
	return nil
}

// VerifyEmail confirms the emailed code, which also signs the person in.
func (e *Engine) VerifyEmail(email, code string) error {
	if err := e.needAccounts(); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), accountTimeout)
	defer cancel()
	sess, err := e.acct.Verify(ctx, strings.TrimSpace(email), strings.TrimSpace(code))
	if err != nil {
		return err
	}
	e.roam.mu.Lock()
	pw := e.roam.pendingPW
	e.roam.pendingPW = ""
	e.roam.mu.Unlock()
	if pw == "" {
		// The app was restarted between the two steps: sign in without the
		// copy key; the Account screen asks for the password once.
		return e.startSession(ctx, sess)
	}
	return e.arrive(ctx, sess, pw, false)
}

// SignIn signs in with an existing account.
func (e *Engine) SignIn(email, password string) error {
	if err := e.needAccounts(); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), accountTimeout)
	defer cancel()
	sess, err := e.acct.Login(ctx, strings.TrimSpace(email), password)
	if err != nil {
		return err
	}
	return e.arrive(ctx, sess, password, false)
}

// SignOut forgets the session on this computer. Conversations and the Relay
// identity stay; only the ability to reach out is withdrawn until the next
// sign-in.
func (e *Engine) SignOut() error {
	if err := e.needAccounts(); err != nil {
		return err
	}
	e.acctMu.Lock()
	e.forgetSession()
	e.acctMu.Unlock()
	return nil
}

// RequestPasswordReset emails a reset code.
func (e *Engine) RequestPasswordReset(email string) error {
	if err := e.needAccounts(); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), accountTimeout)
	defer cancel()
	return e.acct.RequestPasswordReset(ctx, strings.TrimSpace(email))
}

// ConfirmPasswordReset sets a new password using the emailed code.
func (e *Engine) ConfirmPasswordReset(email, code, newPassword string) error {
	if err := e.needAccounts(); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), accountTimeout)
	defer cancel()
	return e.acct.ConfirmPasswordReset(ctx, strings.TrimSpace(email), strings.TrimSpace(code), newPassword)
}

// StartAccountDeletion emails a code for deleting the account. It is the first
// of the steps: code, password, name, and a last confirmation.
func (e *Engine) StartAccountDeletion() error {
	sess, err := e.deletionSession()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), accountTimeout)
	defer cancel()
	return e.acct.StartDeletion(ctx, sess.Token)
}

// CheckAccountDeletion checks the steps done so far without deleting
// anything; empty ones aren't checked yet.
func (e *Engine) CheckAccountDeletion(code, password, name string) error {
	return e.deleteAccount(code, password, name, false)
}

// DeleteAccount deletes the account for good, once every step checks out, and
// forgets the sign-in. Wiping this computer's copy is the app's job.
func (e *Engine) DeleteAccount(code, password, name string) error {
	if err := e.deleteAccount(code, password, name, true); err != nil {
		return err
	}
	e.acctMu.Lock()
	e.forgetSession()
	e.acctMu.Unlock()
	return nil
}

func (e *Engine) deleteAccount(code, password, name string, confirm bool) error {
	sess, err := e.deletionSession()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), accountTimeout)
	defer cancel()
	return e.acct.Delete(ctx, sess.Token, strings.TrimSpace(code), password, strings.TrimSpace(name), confirm)
}

func (e *Engine) deletionSession() (*account.Session, error) {
	if err := e.needAccounts(); err != nil {
		return nil, err
	}
	e.acctMu.Lock()
	sess := e.sess
	e.acctMu.Unlock()
	if sess == nil {
		return nil, errSignedOut
	}
	return sess, nil
}

// startSession records a fresh sign-in and attaches this computer's Relay
// identity to the account. If that attachment is refused, the sign-in is
// undone: someone who can't be tied to an identity must not end up half
// signed in.
func (e *Engine) startSession(ctx context.Context, sess *account.Session) error {
	e.acctMu.Lock()
	e.sess, e.gate, e.gateReason, e.acctOffline = sess, GateOK, "", false
	e.acctMu.Unlock()

	if err := e.syncAccount(ctx); err != nil {
		e.acctMu.Lock()
		// A banned account stays "signed in" so the app can show the
		// statement of reasons on every launch; anything else is a failed
		// sign-in and leaves nothing behind.
		if e.gate == GateBanned {
			_ = e.saveSession(sess)
		} else {
			e.forgetSession()
		}
		e.acctMu.Unlock()
		return err
	}
	e.acctMu.Lock()
	err := e.saveSession(sess)
	e.acctMu.Unlock()
	return err
}

// CheckAccount re-validates the session with the server and keeps the
// account, identity and signed name in step. It is what runs at startup and
// every AccountCheckInterval. Failures to reach the server leave the person
// signed in on the saved session: a dropped connection must not lock anyone
// out of their own conversations.
func (e *Engine) CheckAccount() error {
	if e.acct == nil {
		return nil
	}
	e.acctMu.Lock()
	signedIn := e.sess != nil
	e.acctMu.Unlock()
	if !signedIn {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*accountTimeout)
	defer cancel()
	err := e.syncAccount(ctx)
	if account.IsSignedOut(err) {
		e.acctMu.Lock()
		e.forgetSession()
		e.acctMu.Unlock()
	}
	return err
}

// syncAccount does one round with the server: who am I, is this identity
// attached, is my name signed and published. It updates the gate.
func (e *Engine) syncAccount(ctx context.Context) error {
	e.acctMu.Lock()
	sess := e.sess
	e.acctMu.Unlock()
	if sess == nil {
		return errSignedOut
	}

	me, err := e.acct.Me(ctx, sess.Token)
	switch {
	case account.IsForbidden(err):
		// Suspended by the accounts service: nothing more to do but tell them.
		e.setGate(GateBanned, err.Error(), false)
		return err
	case account.Unreachable(err):
		e.setGate(e.currentGate(), e.currentReason(), true)
		return err
	case err != nil:
		return err
	}
	if me.Banned {
		reason := me.BanReason
		if reason == "" {
			reason = "This account has been banned from AltNet."
		}
		e.setGate(GateBanned, reason, false)
		return errors.New(reason)
	}
	if err := e.checkDevice(ctx, me, sess.Token); err != nil {
		return err
	}

	if me.Username != e.Username() {
		if err := e.acct.Bind(ctx, sess.Token, e.ID); err != nil {
			if account.IsForbidden(err) {
				e.setGate(GateBanned, err.Error(), false)
			}
			return e.bindError(err)
		}
	}
	e.setGate(GateOK, "", false)

	// The name is only ever signed by the server, so a name the account has
	// but this computer doesn't know about (a reinstall that kept the
	// identity) comes back here, and an ordinary run just renews it.
	if me.Handle != "" {
		e.acctMu.Lock()
		due := time.Since(e.lastRefresh) > handleRefreshEvery || e.State.MyHandle() != me.Handle
		e.acctMu.Unlock()
		if due {
			if err := e.refreshHandle(ctx, sess.Token); err != nil && !account.Unreachable(err) {
				return err
			}
		}
	}
	return nil
}

func (e *Engine) bindError(err error) error { return err }

func (e *Engine) setGate(g Gate, reason string, offline bool) {
	e.acctMu.Lock()
	e.gate, e.gateReason, e.acctOffline = g, reason, offline
	e.acctMu.Unlock()
}

func (e *Engine) currentGate() Gate {
	e.acctMu.Lock()
	defer e.acctMu.Unlock()
	return e.gate
}

func (e *Engine) currentReason() string {
	e.acctMu.Lock()
	defer e.acctMu.Unlock()
	return e.gateReason
}

// refreshHandle fetches a freshly signed copy of the account's name and
// publishes it, so the name keeps resolving and survives a reinstall.
func (e *Engine) refreshHandle(ctx context.Context, token string) error {
	grant, err := e.acct.RefreshHandle(ctx, token)
	if err != nil {
		return err
	}
	if err := e.publishGrant(grant); err != nil {
		return err
	}
	e.acctMu.Lock()
	e.lastRefresh = time.Now()
	e.acctMu.Unlock()
	return nil
}

// publishGrant checks a name record the server signed and puts it on the
// network. The check is not distrust of our own server: it catches an app
// pointed at a server whose signing key it wasn't built for, which would
// otherwise "claim" a name nobody could ever resolve.
func (e *Engine) publishGrant(g *account.Grant) error {
	var rec mailbox.HandleRecord
	if err := json.Unmarshal(g.Record, &rec); err != nil {
		return errors.New("the server's name record was unreadable")
	}
	if rec.Username != e.Username() || rec.Handle != mailbox.CanonicalHandle(g.Handle) {
		return errors.New("the server signed a name for someone else; not publishing it")
	}
	if err := rec.Verify(); err != nil {
		return fmt.Errorf("the server's name record didn't verify (%v); is this app up to date?", err)
	}
	// The name is the account's as soon as the server says so; putting it on
	// the network is a separate step that can be retried. A dropped
	// connection at this moment must not fail a sign-in or a claim.
	e.State.SetMyHandle(rec.Handle)
	if err := e.State.Save(); err != nil {
		return err
	}
	if err := e.putGrant(rec.Handle, g.Record); err != nil {
		e.retryPublish(rec.Handle, g.Record)
	}
	return nil
}

// putGrant stores the signed name and refreshes the profile that points at it.
func (e *Engine) putGrant(handle string, record []byte) error {
	if err := e.Client.Store.Put(mailbox.HandleKey(handle), record); err != nil {
		return fmt.Errorf("couldn't publish your name yet: %w", err)
	}
	if _, err := e.Client.PublishProfile(handle); err != nil {
		return fmt.Errorf("your name is signed, but your profile couldn't be updated yet: %w", err)
	}
	return nil
}

// retryPublish keeps trying, in the background and with growing gaps, to put a
// signed name on the network. One retrier at a time; it gives up after about
// twenty minutes, by which point the daily renewal or the next start takes over.
func (e *Engine) retryPublish(handle string, record []byte) {
	e.acctMu.Lock()
	if e.publishing {
		e.acctMu.Unlock()
		return
	}
	e.publishing = true
	e.acctMu.Unlock()
	go func() {
		defer func() {
			e.acctMu.Lock()
			e.publishing = false
			e.acctMu.Unlock()
		}()
		for _, wait := range []time.Duration{5 * time.Second, 15 * time.Second, 45 * time.Second, 2 * time.Minute, 5 * time.Minute, 10 * time.Minute} {
			select {
			case <-e.stop:
				return
			case <-time.After(wait):
			}
			if e.putGrant(handle, record) == nil {
				return
			}
		}
	}()
}

// accountLoop re-checks the account on a jittered timer. It also renews the
// signed name daily for an app left open for weeks.
func (e *Engine) accountLoop() {
	for {
		wait := AccountCheckInterval + time.Duration(mrand.Int64N(int64(AccountCheckInterval/5)))
		select {
		case <-e.stop:
			return
		case <-time.After(wait):
			e.CheckAccount()
		}
	}
}

// claimWithAccount is Claim when accounts are on: the server checks the name
// is free, reserves it for this account, and signs it.
func (e *Engine) claimWithAccount(handle string) (string, error) {
	if err := e.allowed(); err != nil {
		return "", err
	}
	e.acctMu.Lock()
	sess := e.sess
	e.acctMu.Unlock()
	if sess == nil {
		return "", errSignedOut
	}
	ctx, cancel := context.WithTimeout(context.Background(), accountTimeout)
	defer cancel()

	if err := mailbox.ValidHandle(handle); err != nil {
		return "", err
	}
	if err := mailbox.CheckHandleDecency(handle); err != nil {
		return "", err
	}
	if err := e.syncAccount(ctx); err != nil && !account.Unreachable(err) {
		return "", err
	}
	grant, err := e.acct.ClaimHandle(ctx, sess.Token, handle)
	if err != nil {
		return "", err
	}
	if err := e.publishGrant(grant); err != nil {
		return grant.Handle, err
	}
	return grant.Handle, nil
}

// Report tells the operator about a user. reason is the reporter's own words;
// attach says how many of the most recent messages in the conversation to
// include as evidence (0 for none). Nothing is sent that the reporter didn't
// choose: the server cannot read conversations, and this is the one way any
// text leaves the device.
func (e *Engine) Report(peerID, reason string, attach int) (string, error) {
	if e.acct == nil {
		return "", errNoAccount
	}
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return "", errors.New("say what happened, in a sentence or two")
	}
	if peerID == e.Username() {
		return "", errors.New("that's you")
	}
	e.acctMu.Lock()
	sess := e.sess
	e.acctMu.Unlock()
	if sess == nil {
		return "", errSignedOut
	}

	var evidence []account.Evidence
	if attach > 0 {
		thread := e.Thread(peerID)
		if attach > maxEvidence {
			attach = maxEvidence
		}
		if len(thread) > attach {
			thread = thread[len(thread)-attach:]
		}
		for _, m := range thread {
			from := "them"
			if m.Outbound {
				from = "me"
			}
			evidence = append(evidence, account.Evidence{From: from, Text: clip(m.Text, maxEvidenceText), Sent: m.Sent})
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), accountTimeout)
	defer cancel()
	return e.acct.Report(ctx, sess.Token, peerID, clip(reason, maxReasonText), evidence)
}

// These mirror the server's limits, so a long conversation or a rambling
// reason is trimmed here instead of being rejected there.
const (
	maxEvidence     = 10
	maxEvidenceText = 900
	maxReasonText   = 900
)

// clip shortens s to at most n bytes without cutting a character in half.
func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	s = s[:n]
	for !utf8.ValidString(s) {
		s = s[:len(s)-1]
	}
	return s
}
