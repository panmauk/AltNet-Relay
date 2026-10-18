package server

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"time"
)

// ─── wrong passwords ───

// Starting fresh and deleting an account both ask for the password, checked
// with the accounts backend. Without a limit, someone holding a stolen sign-in
// token could use them to guess the password at leisure. Each check is also a
// sign-in at the backend, which leaves a session behind there; the limit bounds
// those too.
const (
	// MaxWrongPasswords is how many wrong passwords one account may give
	// within WrongPasswordWindow before the server stops checking.
	MaxWrongPasswords   = 5
	WrongPasswordWindow = time.Hour
)

var (
	errTooManyPasswords = &httpError{status: http.StatusTooManyRequests, msg: "too many wrong passwords; try again in an hour"}
	errNoPasswordCheck  = errors.New("no password checker configured")
)

type passwordGuesses struct {
	mu    sync.Mutex
	wrong map[string][]time.Time // email -> times of recent wrong passwords
}

// checkPassword checks an account's password with the backend, unless the
// account has given too many wrong ones lately. It returns nil, ErrWrongPassword,
// errTooManyPasswords, errNoPasswordCheck, or the backend's own failure.
func (s *Server) checkPassword(ctx context.Context, email, password string) error {
	check := s.passwords()
	if check == nil {
		return errNoPasswordCheck
	}
	now := s.now()
	g := &s.guesses
	g.mu.Lock()
	recent := g.wrong[email][:0]
	for _, t := range g.wrong[email] {
		if now.Sub(t) < WrongPasswordWindow {
			recent = append(recent, t)
		}
	}
	if g.wrong == nil {
		g.wrong = map[string][]time.Time{}
	}
	g.wrong[email] = recent
	locked := len(recent) >= MaxWrongPasswords
	g.mu.Unlock()
	if locked {
		return errTooManyPasswords
	}

	err := check.CheckPassword(ctx, email, password)
	g.mu.Lock()
	defer g.mu.Unlock()
	switch {
	case errors.Is(err, ErrWrongPassword):
		g.wrong[email] = append(g.wrong[email], now)
	case err == nil:
		delete(g.wrong, email)
	}
	// Forget accounts with nothing recent, so the map can't grow forever.
	if len(g.wrong) > 10000 {
		for e, ts := range g.wrong {
			if len(ts) == 0 || now.Sub(ts[len(ts)-1]) >= WrongPasswordWindow {
				delete(g.wrong, e)
			}
		}
	}
	return err
}

// ─── looking tokens up ───

// Every request with a token the server hasn't seen lately is a lookup at the
// accounts backend. A made-up token costs nothing to send, so lookups are
// rationed: refused tokens are remembered for a while, and new lookups are
// limited to a steady rate with a burst. Tokens already known never wait.
const (
	lookupsPerSecond = 20
	lookupBurst      = 60
	refusedFor       = time.Minute
	maxRemembered    = 4096
)

var errLookupsBusy = errors.New("too many sign-in checks at once")

type lookupBudget struct {
	tokens float64
	last   time.Time
}

// take spends one lookup if the budget allows. Caller holds the verifier lock.
func (b *lookupBudget) take(now time.Time) bool {
	if b.last.IsZero() {
		b.tokens, b.last = lookupBurst, now
	}
	b.tokens += now.Sub(b.last).Seconds() * lookupsPerSecond
	if b.tokens > lookupBurst {
		b.tokens = lookupBurst
	}
	b.last = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}
