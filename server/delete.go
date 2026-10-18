package server

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"strings"
	"sync"
	"time"

	"altnet-relay/mailbox"
)

// Deleting an account is deliberately slow: an emailed code, then the
// password, then the account's name typed out, then a last "are you sure"
// in the app. Each step is checked here as the person reaches it, and all of
// them again at the end, so a modified app can't skip any.
//
// What goes:
//   - the AltNet account itself (email and password), through the accounts
//     backend's own DELETE /v1/me -- that is also the AltNet Studio sign-in;
//   - everything this server keeps about the account: its record, its Relay
//     identity link, its email on any reports it made.
//
// What stays, and why:
//   - the @name stays taken, by nobody: given to someone else it would let a
//     stranger receive messages meant for the person who left;
//   - reports *about* the account, and any ban, stay -- they are the record
//     of having acted on notices, and deleting must not be a way out of a ban.
//     A banned account can't delete itself from the app at all; the person
//     writes to us instead.
//   - the audit log, which is append-only by design.
const (
	deleteCodeTTL     = 15 * time.Minute
	deleteMaxTries    = 5 // wrong codes, passwords or names before the code is spent
	deleteCodesPerDay = 3 // codes one account may ask for in a day
)

// deletedOwner holds the place of a deleted account in the name index: the
// name is still taken, by nobody.
const deletedOwner = "(deleted account)"

// Mailer sends one plain-text email.
type Mailer interface {
	Send(to, subject, body string) error
}

// Accounts is what deletion needs from the accounts backend: checking a
// password, and deleting the account.
type Accounts interface {
	CheckPassword(ctx context.Context, email, password string) error
	DeleteAccount(ctx context.Context, token string) error
}

// ErrWrongPassword is CheckPassword's answer for a password that isn't right.
var ErrWrongPassword = errors.New("wrong password")

type pendingDeletion struct {
	codeHash [32]byte
	expires  time.Time
	tries    int
}

type deletions struct {
	mu      sync.Mutex
	pending map[string]*pendingDeletion // email -> the code in play
	asked   map[string][]time.Time      // email -> when codes were sent
}

// SetDeletion turns on account deletion. Without it the endpoints answer that
// deletion isn't available and point to the abuse address.
func (s *Server) SetDeletion(m Mailer, a Accounts) {
	s.mailer, s.accounts = m, a
}

// passwords is whatever can check an account's password: the accounts
// backend (also the sign-in checker in production).
func (s *Server) passwords() Accounts {
	if s.accounts != nil {
		return s.accounts
	}
	if a, ok := s.auth.(Accounts); ok {
		return a
	}
	return nil
}

func (s *Server) deletionReady(w http.ResponseWriter) bool {
	if s.mailer == nil || s.accounts == nil {
		writeError(w, http.StatusServiceUnavailable, "deleting an account isn't available right now; write to abuse.report@panmox.org and we'll do it for you")
		return false
	}
	return true
}

func (s *Server) refuseIfBanned(w http.ResponseWriter, email string) bool {
	banned := false
	s.store.View(func(d *fileData) {
		if a := d.Accounts[email]; a != nil && a.Banned {
			banned = true
		}
	})
	if banned {
		writeError(w, http.StatusForbidden, "a banned account can't be deleted from the app; write to abuse.report@panmox.org and we'll handle it")
	}
	return banned
}

// handleDeleteStart emails a code to the account's address.
func (s *Server) handleDeleteStart(w http.ResponseWriter, r *http.Request, p Principal) {
	var req struct{}
	if !readJSON(w, r, &req) || !s.deletionReady(w) || s.refuseIfBanned(w, p.Email) {
		return
	}
	now := s.now()
	s.del.mu.Lock()
	if s.del.asked == nil {
		s.del.asked = map[string][]time.Time{}
		s.del.pending = map[string]*pendingDeletion{}
	}
	var recent []time.Time
	for _, t := range s.del.asked[p.Email] {
		if now.Sub(t) < 24*time.Hour {
			recent = append(recent, t)
		}
	}
	if len(recent) >= deleteCodesPerDay {
		s.del.mu.Unlock()
		writeError(w, http.StatusTooManyRequests, "you've asked for several codes today; try again tomorrow")
		return
	}
	code, err := sixDigits()
	if err != nil {
		s.del.mu.Unlock()
		s.fail(w, err)
		return
	}
	s.del.asked[p.Email] = append(recent, now)
	s.del.pending[p.Email] = &pendingDeletion{codeHash: sha256.Sum256([]byte(code)), expires: now.Add(deleteCodeTTL)}
	s.del.mu.Unlock()

	body := "Someone -- hopefully you -- asked AltNet Relay to delete the AltNet account for this email address.\n\n" +
		"Your code: " + code + "\n\n" +
		"It works for 15 minutes. After the code, the app also asks for your password and your name.\n\n" +
		"Deleting removes your AltNet account for good. It is also your AltNet Studio sign-in.\n\n" +
		"If this wasn't you, don't share the code, and change your password: nothing happens without it.\n\n" +
		"PANMOX -- abuse.report@panmox.org\n"
	if err := s.mailer.Send(p.Email, "Your code to delete your AltNet account", body); err != nil {
		s.log.Error("deletion code email failed", "err", err)
		s.del.mu.Lock()
		delete(s.del.pending, p.Email)
		s.del.mu.Unlock()
		writeError(w, http.StatusServiceUnavailable, "couldn't send the email right now; try again in a few minutes")
		return
	}
	s.store.Audit("delete code sent", map[string]any{"email": p.Email})
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

type deleteReq struct {
	Code     string `json:"code"`
	Password string `json:"password"`
	Name     string `json:"name"`
	Confirm  bool   `json:"confirm"`
}

// handleDelete checks the steps given so far; with confirm set and every step
// given, it deletes the account.
func (s *Server) handleDelete(w http.ResponseWriter, r *http.Request, p Principal) {
	var req deleteReq
	if !readJSON(w, r, &req) || !s.deletionReady(w) || s.refuseIfBanned(w, p.Email) {
		return
	}
	req.Code = strings.TrimSpace(req.Code)
	req.Name = strings.TrimSpace(req.Name)
	if req.Confirm && (req.Code == "" || req.Password == "" || req.Name == "") {
		writeError(w, http.StatusBadRequest, "every step has to be done first")
		return
	}

	// The code.
	s.del.mu.Lock()
	pd := s.del.pending[p.Email]
	switch {
	case pd == nil || s.now().After(pd.expires):
		delete(s.del.pending, p.Email)
		s.del.mu.Unlock()
		writeError(w, http.StatusBadRequest, "that code has expired; ask for a new one")
		return
	case pd.tries >= deleteMaxTries:
		delete(s.del.pending, p.Email)
		s.del.mu.Unlock()
		writeError(w, http.StatusTooManyRequests, "too many wrong tries; ask for a new code")
		return
	}
	got := sha256.Sum256([]byte(req.Code))
	if subtle.ConstantTimeCompare(got[:], pd.codeHash[:]) != 1 {
		pd.tries++
		s.del.mu.Unlock()
		writeError(w, http.StatusBadRequest, "that code isn't right")
		return
	}
	s.del.mu.Unlock()
	wrong := func(msg string) {
		s.del.mu.Lock()
		if pd := s.del.pending[p.Email]; pd != nil {
			pd.tries++
		}
		s.del.mu.Unlock()
		writeError(w, http.StatusBadRequest, msg)
	}

	// The password, asked of the accounts backend: this server never keeps one.
	if req.Password != "" {
		var he *httpError
		err := s.checkPassword(r.Context(), p.Email, req.Password)
		switch {
		case errors.Is(err, ErrWrongPassword):
			wrong("that password isn't right")
			return
		case errors.As(err, &he):
			s.fail(w, err)
			return
		case err != nil:
			s.log.Warn("password check failed", "err", err)
			writeError(w, http.StatusServiceUnavailable, "the account service isn't reachable right now; try again shortly")
			return
		}
	}

	// The name: the @name, or the email address for an account without one.
	if req.Name != "" {
		var handle string
		s.store.View(func(d *fileData) {
			if a := d.Accounts[p.Email]; a != nil {
				handle = a.Handle
			}
		})
		typed := strings.ToLower(strings.TrimPrefix(req.Name, "@"))
		ok := (handle != "" && mailbox.CanonicalHandle(typed) == handle) || (handle == "" && typed == p.Email)
		if !ok {
			if handle == "" {
				wrong("that isn't your email address")
			} else {
				wrong("that isn't your name")
			}
			return
		}
	}

	if !req.Confirm {
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
		return
	}

	// Everything checks out. The account itself goes first: if the backend
	// fails, nothing has changed and the code is still good for another try.
	// (The other way round, a retry would find half an account here.)
	if err := s.accounts.DeleteAccount(r.Context(), bearer(r)); err != nil {
		s.log.Error("backend account delete failed", "err", err)
		writeError(w, http.StatusBadGateway, "the account service didn't answer, so nothing was deleted; try again")
		return
	}
	s.del.mu.Lock()
	delete(s.del.pending, p.Email)
	delete(s.del.asked, p.Email)
	s.del.mu.Unlock()
	if f, ok := s.auth.(interface{ Forget(token string) }); ok {
		f.Forget(bearer(r))
	}
	var username, handle string
	err := s.store.Update(func(d *fileData) error {
		a := d.Accounts[p.Email]
		if a == nil {
			return nil // already done on an earlier try
		}
		username, handle = a.Username, a.Handle
		for h, owner := range d.Handles {
			if owner == p.Email {
				d.Handles[h] = deletedOwner
			}
		}
		// Reports this person made keep their place (a reported account's
		// history must stay whole) but no longer name them.
		anon := "deleted-" + randomHex(8)
		for _, rep := range d.Reports {
			if rep.ReporterEmail == p.Email {
				rep.ReporterEmail = anon
			}
		}
		delete(d.Accounts, p.Email)
		return nil
	})
	s.removeCopy(p.Email)
	if err != nil {
		// The account is already gone from the backend, so the person can't
		// try again; the operator has to finish this by hand.
		s.log.Error("account deleted but its Relay record couldn't be removed", "err", err)
		s.store.Audit("account deleted, record left behind", map[string]any{"email": p.Email, "err": err.Error()})
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
		return
	}
	s.store.Audit("account deleted", map[string]any{"username": username, "handle": handle})
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func sixDigits() (string, error) {
	n, err := rand.Int(rand.Reader, big.NewInt(1_000_000))
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%06d", n.Int64()), nil
}

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// ─── the accounts backend's side ───

// CheckPassword asks the backend whether the password is right by signing in
// with it. The session that creates is never handed out, and goes with the
// account when it is deleted.
func (v *BackendVerifier) CheckPassword(ctx context.Context, email, password string) error {
	blob, _ := json.Marshal(map[string]string{"email": email, "password": password})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(v.BaseURL, "/")+"/v1/login", bytes.NewReader(blob))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := v.client().Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
		return nil
	case http.StatusUnauthorized:
		return ErrWrongPassword
	}
	return fmt.Errorf("accounts backend answered %d", resp.StatusCode)
}

// DeleteAccount deletes the signed-in account with the backend's own
// DELETE /v1/me.
func (v *BackendVerifier) DeleteAccount(ctx context.Context, token string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, strings.TrimRight(v.BaseURL, "/")+"/v1/me", nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := v.client().Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("accounts backend answered %d", resp.StatusCode)
	}
	return nil
}

// Forget drops a token from the cache, so a deleted account's sign-in stops
// working here at once rather than when the cache would have expired.
func (v *BackendVerifier) Forget(token string) {
	key := sha256.Sum256([]byte(token))
	v.mu.Lock()
	delete(v.cache, key)
	v.mu.Unlock()
}

func (v *BackendVerifier) client() *http.Client {
	if v.HTTP != nil {
		return v.HTTP
	}
	return &http.Client{Timeout: 10 * time.Second}
}
