package engine

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"altnet-relay/account"
	"altnet-relay/backup"
	"altnet-relay/mailbox"
	"altnet-relay/state"
)

// Signing in on another computer.
//
// The account's keys and saved state are kept as a locked copy on the
// server, sealed here with the person's password (package backup), so that
// signing in on another computer brings everything along: same identity,
// same @name, same conversations. The copy is refreshed shortly after
// anything changes. The person can turn it off; the account then stays on
// this computer.
//
// One computer at a time: this install has a random device ID, signing in
// claims the account for it, and a computer that finds the account claimed
// by another signs itself out. Two computers writing into the same
// conversations at once would put messages out of order.
//
// Restoring replaces this computer's identity, which the running engine
// can't do to itself: the copy is unpacked into restoreDir, and the next Open
// puts it in place (the app restarts itself to get there). Whatever this
// computer had before is moved aside into previousDir, never deleted.
const (
	copyKeyFile = "backup.key" // the password-derived key that locks the copy
	deviceFile  = "device.id"  // this install's random device ID
	copyOffFile = "copy.off"   // present when the person turned the copy off
	restoreDir  = "restore"    // a copy waiting to be put in place at the next Open
	syncMarker  = "sync"       // in restoreDir: catch up with the network after restoring
	previousDir = "previous"   // what a restore replaced, kept just in case

	// hereCheckEvery is how often, at most, sending first makes sure this
	// computer still holds the account.
	hereCheckEvery = 45 * time.Second
)

// CopyCheckEvery is how often the copy is checked for changes to save.
var CopyCheckEvery = 20 * time.Second

var (
	// ErrRestart: the account's copy is unpacked and the app must restart to
	// use it. The sign-in itself succeeded.
	ErrRestart = errors.New("bringing your account to this computer; AltNet Relay restarts once")
	// ErrNoCopy: the account is set up on another computer and there is no
	// copy to bring here. The person can start fresh (StartFresh).
	ErrNoCopy = errors.New("your account is set up on another computer, and it isn't keeping a copy to bring here")
	// ErrOldCopy: the copy is locked with a different password -- one from
	// before a password change.
	ErrOldCopy = errors.New("your account's copy is still locked with an older password. Sign in once on your other computer to update it, or start fresh here")
	// errElsewhere: another computer has taken the account over.
	errElsewhere  = errors.New("you signed in on another computer, so AltNet Relay signed out here. Sign in again to use it on this computer")
	errCatchingUp = errors.New("bringing your account up to date; try again in a moment")
)

// roaming is the engine's part in all this.
type roaming struct {
	device string

	// upload is held for a whole save, and while the copy is turned off, so
	// a save in flight can't put back a copy that was just deleted.
	upload sync.Mutex

	mu         sync.Mutex
	key        *backup.Key
	savedHash  [32]byte // fingerprint of the state last copied
	savedAt    int64    // when the server last took a copy from us
	lastErr    string
	hereAt     time.Time
	catchingUp bool
	needSync   bool
	pendingPW  string // the password from SignUp, until the email is verified
}

// prepareRoaming runs in Open before the identity loads: it puts a waiting
// copy in place and loads this install's device ID and copy key.
func prepareRoaming(dir string) (*roaming, error) {
	r := &roaming{}
	if err := applyRestore(dir, r); err != nil {
		return nil, err
	}
	if b, err := os.ReadFile(filepath.Join(dir, deviceFile)); err == nil && len(strings.TrimSpace(string(b))) >= 32 {
		r.device = strings.TrimSpace(string(b))
	} else {
		raw := make([]byte, 16)
		if _, err := rand.Read(raw); err != nil {
			return nil, err
		}
		r.device = hex.EncodeToString(raw)
		if err := os.WriteFile(filepath.Join(dir, deviceFile), []byte(r.device), 0o600); err != nil {
			return nil, err
		}
	}
	if b, err := os.ReadFile(filepath.Join(dir, copyKeyFile)); err == nil {
		var k backup.Key
		if json.Unmarshal(b, &k) == nil && len(k.K) == 32 {
			r.key = &k
		}
	}
	return r, nil
}

// applyRestore moves an unpacked copy into place, keeping what was there.
func applyRestore(dir string, r *roaming) error {
	src := filepath.Join(dir, restoreDir)
	if _, err := os.Stat(filepath.Join(src, mailbox.IdentityFileName)); err != nil {
		_ = os.RemoveAll(src) // nothing complete waiting
		return nil
	}
	keep := filepath.Join(dir, previousDir, time.Now().Format("20060102-150405"))
	files := []string{mailbox.IdentityFileName, mailbox.BoxKeyFileName, state.FileName, copyKeyFile}
	for _, f := range files {
		if _, err := os.Stat(filepath.Join(dir, f)); err == nil {
			if err := os.MkdirAll(keep, 0o700); err != nil {
				return err
			}
			if err := os.Rename(filepath.Join(dir, f), filepath.Join(keep, f)); err != nil {
				return err
			}
		}
	}
	for _, f := range files {
		if _, err := os.Stat(filepath.Join(src, f)); err == nil {
			if err := os.Rename(filepath.Join(src, f), filepath.Join(dir, f)); err != nil {
				return err
			}
		}
	}
	if _, err := os.Stat(filepath.Join(src, syncMarker)); err == nil {
		r.needSync = true
	}
	return os.RemoveAll(src)
}

func (e *Engine) copyOff() bool {
	_, err := os.Stat(filepath.Join(e.dataDir, copyOffFile))
	return err == nil
}

func (e *Engine) saveCopyKey(k *backup.Key) error {
	blob, err := json.Marshal(k)
	if err != nil {
		return err
	}
	path := filepath.Join(e.dataDir, copyKeyFile)
	if err := os.WriteFile(path+".tmp", blob, 0o600); err != nil {
		return err
	}
	if err := os.Rename(path+".tmp", path); err != nil {
		return err
	}
	e.roam.mu.Lock()
	e.roam.key = k
	e.roam.mu.Unlock()
	return nil
}

// arrive finishes a sign-in with the password in hand. It brings the
// account's copy here if the account lives elsewhere (ErrRestart), claims the
// account for this computer, and re-locks the copy with this password.
// fresh is "start fresh here": move the account to this computer's own keys.
func (e *Engine) arrive(ctx context.Context, sess *account.Session, password string, fresh bool) error {
	me, err := e.acct.Me(ctx, sess.Token)
	if err == nil && !me.Banned && me.Username != "" && me.Username != e.Username() && !fresh {
		return e.bringCopy(ctx, sess, password)
	}
	if fresh {
		if err := e.acct.Rebind(ctx, sess.Token, e.ID, password); err != nil {
			return err
		}
	}
	if err == nil && !me.Banned {
		if err := e.claim(ctx, sess.Token); err != nil && !account.Unreachable(err) {
			return err
		}
	}
	if err := e.startSession(ctx, sess); err != nil {
		return err
	}
	// A new key from this password, so the copy always opens with the
	// password last used to sign in -- including after a password reset.
	if k, err := backup.NewKey(password); err == nil {
		_ = e.saveCopyKey(k)
		e.roam.mu.Lock()
		e.roam.savedHash = [32]byte{} // save a copy under the new key
		e.roam.mu.Unlock()
		go e.saveCopy()
	}
	return nil
}

// bringCopy fetches and unlocks the account's copy and leaves it ready for
// the next Open.
func (e *Engine) bringCopy(ctx context.Context, sess *account.Session, password string) error {
	blob, found, err := e.acct.GetBackup(ctx, sess.Token)
	if err != nil {
		return err
	}
	if !found {
		return ErrNoCopy
	}
	p, key, err := backup.Open(blob, password)
	if errors.Is(err, backup.ErrWrongPassword) {
		return ErrOldCopy
	}
	if err != nil {
		return errors.New("your account's copy couldn't be read; you can start fresh here")
	}
	dst := filepath.Join(e.dataDir, restoreDir)
	_ = os.RemoveAll(dst)
	if err := os.MkdirAll(dst, 0o700); err != nil {
		return err
	}
	keyBlob, _ := json.Marshal(key)
	for name, data := range map[string][]byte{
		mailbox.BoxKeyFileName: p.RelayBox,
		state.FileName:         p.State,
		copyKeyFile:            keyBlob,
		syncMarker:             nil,
	} {
		if err := os.WriteFile(filepath.Join(dst, name), data, 0o600); err != nil {
			return err
		}
	}
	// The identity last: its presence is what marks the copy complete.
	if err := os.WriteFile(filepath.Join(dst, mailbox.IdentityFileName), p.RelayKey, 0o600); err != nil {
		return err
	}
	// This computer takes the account over now, so the other one stops
	// writing before this one starts.
	if err := e.claim(ctx, sess.Token); err != nil {
		_ = os.RemoveAll(dst)
		return err
	}
	e.acctMu.Lock()
	err = e.saveSession(sess)
	e.acctMu.Unlock()
	if err != nil {
		return err
	}
	return ErrRestart
}

// StartFresh signs in and moves the account to this computer's own keys:
// for when the account's copy can't be brought here. The @name comes along;
// earlier conversations stay with the old keys.
func (e *Engine) StartFresh(email, password string) error {
	if err := e.needAccounts(); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), accountTimeout)
	defer cancel()
	sess, err := e.acct.Login(ctx, strings.TrimSpace(email), password)
	if err != nil {
		return err
	}
	// The copy belonged to the old keys: it goes, and this computer's
	// becomes the copy from now on.
	_ = e.acct.DeleteBackup(ctx, sess.Token)
	return e.arrive(ctx, sess, password, true)
}

// checkDevice is part of every account check: it claims the account for this
// computer if nobody has, and signs out here if another computer has.
func (e *Engine) checkDevice(ctx context.Context, me *account.Me, token string) error {
	e.roam.mu.Lock()
	e.roam.hereAt = time.Now()
	e.roam.mu.Unlock()
	switch me.Device {
	case e.roam.device:
		return nil
	case "":
		return e.claim(ctx, token)
	}
	e.acctMu.Lock()
	e.forgetSession()
	e.gateReason = errElsewhere.Error()
	e.acctMu.Unlock()
	return errElsewhere
}

// claim makes this computer the one the account is in use on. A server from
// before this existed answers "not found"; that is no reason to fail.
func (e *Engine) claim(ctx context.Context, token string) error {
	err := e.acct.ClaimDevice(ctx, token, e.roam.device)
	var ae *account.Error
	if errors.As(err, &ae) && ae.Status == 404 {
		return nil
	}
	return err
}

// stillHere makes sure, now and then, that this computer still holds the
// account before something is sent. Offline counts as yes.
func (e *Engine) stillHere() error {
	if e.acct == nil {
		return nil
	}
	e.roam.mu.Lock()
	catching := e.roam.catchingUp
	recent := time.Since(e.roam.hereAt) < hereCheckEvery
	e.roam.mu.Unlock()
	if catching {
		return errCatchingUp
	}
	if recent {
		return nil
	}
	e.acctMu.Lock()
	sess := e.sess
	e.acctMu.Unlock()
	if sess == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel()
	me, err := e.acct.Me(ctx, sess.Token)
	if err != nil {
		return nil
	}
	return e.checkDevice(ctx, me, sess.Token)
}

// catchUp runs once after a restore: the copy can be a little older than the
// conversations on the network, so each conversation's outgoing chain is moved
// up to the newest message this identity wrote, wherever it wrote it.
func (e *Engine) catchUp() {
	e.roam.mu.Lock()
	e.roam.catchingUp = true
	e.roam.mu.Unlock()
	defer func() {
		e.roam.mu.Lock()
		e.roam.catchingUp = false
		e.roam.needSync = false
		e.roam.mu.Unlock()
	}()
	convs := e.State.Conversations()
	sem := make(chan struct{}, 6)
	var wg sync.WaitGroup
	for _, c := range convs {
		wg.Add(1)
		sem <- struct{}{}
		go func(c *mailbox.Conversation) {
			defer wg.Done()
			defer func() { <-sem }()
			unlock := e.sendLock(c.PeerID)
			defer unlock()
			_, _ = e.Client.SyncSent(c)
		}(c)
	}
	wg.Wait()
	_ = e.State.Save()
}

// copyLoop keeps the copy current: shortly after anything changes, a new
// locked copy goes to the server.
func (e *Engine) copyLoop() {
	t := time.NewTicker(CopyCheckEvery)
	defer t.Stop()
	for {
		select {
		case <-e.stop:
			return
		case <-t.C:
			e.saveCopy()
		}
	}
}

// saveCopy locks and sends a copy if anything changed since the last one.
func (e *Engine) saveCopy() {
	if e.acct == nil {
		return
	}
	e.roam.upload.Lock()
	defer e.roam.upload.Unlock()
	if e.copyOff() {
		return
	}
	e.roam.mu.Lock()
	key, last, catching := e.roam.key, e.roam.savedHash, e.roam.catchingUp
	e.roam.mu.Unlock()
	e.acctMu.Lock()
	sess, gate := e.sess, e.gate
	e.acctMu.Unlock()
	if key == nil || sess == nil || gate != GateOK || catching {
		return
	}
	// From memory, not from state.json: reading the file while the state is
	// being saved makes the save fail on Windows.
	snap, err := e.State.Snapshot()
	if err != nil {
		return
	}
	sum := sha256.Sum256(snap)
	if sum == last {
		return // nothing changed since the last copy
	}
	read := func(f string) []byte { b, _ := os.ReadFile(filepath.Join(e.dataDir, f)); return b }
	p := &backup.Payload{Version: 1, RelayKey: read(mailbox.IdentityFileName), RelayBox: read(mailbox.BoxKeyFileName),
		State: snap, Saved: time.Now().Unix()}
	if len(p.RelayKey) == 0 || len(p.RelayBox) == 0 {
		return
	}
	blob, err := key.Lock(p)
	if err == nil {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		err = e.acct.PutBackup(ctx, sess.Token, e.roam.device, blob)
		cancel()
	}
	e.roam.mu.Lock()
	defer e.roam.mu.Unlock()
	if err != nil {
		var ae *account.Error
		if errors.As(err, &ae) && ae.Status == 429 {
			return // saved a moment ago; the next round gets it
		}
		e.roam.lastErr = err.Error()
		if errors.As(err, &ae) && ae.Status == 409 {
			go e.CheckAccount() // another computer has the account now
		}
		return
	}
	e.roam.savedHash, e.roam.savedAt, e.roam.lastErr = sum, time.Now().Unix(), ""
}

// CopyStatus is what the Account screen says about the copy.
type CopyStatus struct {
	On            bool   // the person wants a copy kept
	NeedsPassword bool   // on, but this computer has no key yet: ask for the password once
	SavedAt       int64  // when the copy was last saved from here, 0 if not yet
	Problem       string // why the last save failed, if it did
}

// Copy reports the copy's state.
func (e *Engine) Copy() CopyStatus {
	e.roam.mu.Lock()
	defer e.roam.mu.Unlock()
	return CopyStatus{On: !e.copyOff(), NeedsPassword: !e.copyOff() && e.roam.key == nil,
		SavedAt: e.roam.savedAt, Problem: e.roam.lastErr}
}

// SetCopy turns the copy on or off. Off deletes it from the server: the
// account then can't be brought to another computer.
func (e *Engine) SetCopy(on bool) error {
	if err := e.needAccounts(); err != nil {
		return err
	}
	path := filepath.Join(e.dataDir, copyOffFile)
	if on {
		_ = os.Remove(path)
		e.roam.mu.Lock()
		e.roam.savedHash = [32]byte{}
		e.roam.mu.Unlock()
		go e.saveCopy()
		return nil
	}
	e.acctMu.Lock()
	sess := e.sess
	e.acctMu.Unlock()
	if sess == nil {
		return errSignedOut
	}
	e.roam.upload.Lock()
	defer e.roam.upload.Unlock()
	// Off first, then delete: nothing saves once the switch is off.
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), accountTimeout)
	defer cancel()
	if err := e.acct.DeleteBackup(ctx, sess.Token); err != nil {
		_ = os.Remove(path) // still on, since the copy is still there
		return err
	}
	return nil
}

// UnlockCopy is the one-time password for a computer that has no copy key
// yet (signed in before copies existed). The password is checked with the
// account service first, so a typo can't lock the copy with the wrong one.
func (e *Engine) UnlockCopy(password string) error {
	if err := e.needAccounts(); err != nil {
		return err
	}
	e.acctMu.Lock()
	sess := e.sess
	e.acctMu.Unlock()
	if sess == nil {
		return errSignedOut
	}
	ctx, cancel := context.WithTimeout(context.Background(), accountTimeout)
	defer cancel()
	if _, err := e.acct.Login(ctx, sess.Email, password); err != nil {
		if account.IsSignedOut(err) {
			return errors.New("that password isn't right")
		}
		return err
	}
	k, err := backup.NewKey(password)
	if err != nil {
		return err
	}
	if err := e.saveCopyKey(k); err != nil {
		return err
	}
	e.roam.mu.Lock()
	e.roam.savedHash = [32]byte{}
	e.roam.mu.Unlock()
	go e.saveCopy()
	return nil
}
