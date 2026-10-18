// Package account is the app's side of "you need a Studio account to use
// Relay". It talks to two services on the operator's server:
//
//   - the Studio accounts backend (sign up, verify by emailed code, sign in,
//     reset a password) -- the SAME accounts people already use for .alt
//     names, unchanged;
//   - relay-server, next to it (attach your Relay identity to the account,
//     claim a unique @name, report a user).
//
// The app never stores a password. Signing in yields a session token, and
// only that token is kept on disk (see Session). The password is sent once,
// over HTTPS, to the accounts backend and nowhere else.
package account

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"altnet/core/crypto"

	"altnet-relay/mailbox"
	"altnet-relay/wire"
)

// Default addresses of the operator's services. relay-server sits behind
// the same host as the accounts backend, under /relay.
const (
	DefaultStudioURL = "https://api.panmox.org"
	DefaultRelayURL  = "https://api.panmox.org/relay"
)

// maxResponse bounds what we read from either service, so a misbehaving
// server (or something impersonating it) can't make the app swallow memory.
const maxResponse = 1 << 20

// Client speaks to the accounts backend and relay-server.
type Client struct {
	StudioURL string // accounts backend, no trailing slash
	RelayURL  string // relay-server, no trailing slash
	HTTP      *http.Client
}

// New returns a client for the given service addresses; empty ones fall back
// to the defaults.
func New(studioURL, relayURL string) *Client {
	if studioURL == "" {
		studioURL = DefaultStudioURL
	}
	if relayURL == "" {
		relayURL = DefaultRelayURL
	}
	return &Client{
		StudioURL: strings.TrimRight(studioURL, "/"),
		RelayURL:  strings.TrimRight(relayURL, "/"),
		HTTP:      &http.Client{Timeout: 20 * time.Second},
	}
}

// Error is a refusal from one of the services, with a message that is safe to
// show a person as it stands.
type Error struct {
	Status  int
	Message string
	Code    string // set by relay-server for refusals the app acts on
}

func (e *Error) Error() string { return e.Message }

// IsSignedOut reports whether the session token is no longer good: expired,
// revoked, or never valid. The remedy is to sign in again.
func IsSignedOut(err error) bool {
	var e *Error
	return errors.As(err, &e) && e.Status == http.StatusUnauthorized
}

// IsForbidden reports a refusal that signing in again won't fix: an account
// that is suspended or banned. The message says why.
func IsForbidden(err error) bool {
	var e *Error
	return errors.As(err, &e) && e.Status == http.StatusForbidden
}

// IsConflict reports a refusal because something is already taken, such as a
// Relay identity that belongs to another account.
func IsConflict(err error) bool {
	var e *Error
	return errors.As(err, &e) && e.Status == http.StatusConflict
}

// IdentityTaken reports that the Relay identity being attached belongs to
// another account. (A server from before the codes existed only says so in
// words.)
func IdentityTaken(err error) bool {
	var e *Error
	if !errors.As(err, &e) || e.Status != http.StatusConflict {
		return false
	}
	if e.Code != "" {
		return e.Code == "identity_taken"
	}
	return strings.Contains(e.Message, "already belongs to another account")
}

// Unreachable reports that the service couldn't be contacted at all (offline,
// server down), as opposed to answering "no".
func Unreachable(err error) bool {
	if err == nil {
		return false
	}
	var e *Error
	if errors.As(err, &e) {
		return e.Status == http.StatusBadGateway || e.Status == http.StatusServiceUnavailable || e.Status == http.StatusGatewayTimeout
	}
	return true
}

// Session is a signed-in account. Only Token and Email are ever stored.
type Session struct {
	Token     string `json:"token"`
	ExpiresAt int64  `json:"expires_at"`
	Email     string `json:"email"`
	IsAdmin   bool   `json:"is_admin"`
}

// Me is what relay-server knows about the signed-in account.
type Me struct {
	Email     string `json:"email"`
	Username  string `json:"username"`
	Handle    string `json:"handle"`
	Banned    bool   `json:"banned"`
	BanReason string `json:"ban_reason"`
	Admin     bool   `json:"is_admin"`
	Device    string `json:"device"`    // the computer the account is in use on
	BackupAt  int64  `json:"backup_at"` // when its locked copy was last saved; 0 = none
}

// Grant is a name the server has signed for the user: publish Record at
// mailbox.HandleKey(Handle) and everyone can resolve it.
type Grant struct {
	Handle string          `json:"handle"`
	Record json.RawMessage `json:"record"`
}

// ─── Studio accounts ───

// Signup creates an account and emails a verification code.
func (c *Client) Signup(ctx context.Context, email, password string) error {
	return c.call(ctx, c.StudioURL, "POST", "/v1/signup", "",
		map[string]string{"email": email, "password": password}, nil)
}

// Verify confirms the emailed code and returns a signed-in session.
func (c *Client) Verify(ctx context.Context, email, code string) (*Session, error) {
	var s Session
	if err := c.call(ctx, c.StudioURL, "POST", "/v1/verify", "",
		map[string]string{"email": email, "code": code}, &s); err != nil {
		return nil, err
	}
	return &s, nil
}

// Login signs in with an email and password.
func (c *Client) Login(ctx context.Context, email, password string) (*Session, error) {
	var s Session
	if err := c.call(ctx, c.StudioURL, "POST", "/v1/login", "",
		map[string]string{"email": email, "password": password}, &s); err != nil {
		return nil, err
	}
	return &s, nil
}

// RequestPasswordReset emails a reset code. The backend answers the same
// whether or not the address has an account.
func (c *Client) RequestPasswordReset(ctx context.Context, email string) error {
	return c.call(ctx, c.StudioURL, "POST", "/v1/password/request-reset", "",
		map[string]string{"email": email}, nil)
}

// ConfirmPasswordReset sets a new password using the emailed code.
func (c *Client) ConfirmPasswordReset(ctx context.Context, email, code, newPassword string) error {
	return c.call(ctx, c.StudioURL, "POST", "/v1/password/confirm-reset", "",
		map[string]string{"email": email, "code": code, "new_password": newPassword}, nil)
}

// ─── relay-server ───

// Me asks relay-server about the signed-in account. It doubles as the check
// that the session is still valid and the account isn't suspended or banned.
func (c *Client) Me(ctx context.Context, token string) (*Me, error) {
	var m Me
	if err := c.call(ctx, c.RelayURL, "GET", "/v1/me", token, nil, &m); err != nil {
		return nil, err
	}
	return &m, nil
}

// Bind attaches this Relay identity to the account, proving with a signature
// that the app really holds the identity's key.
func (c *Client) Bind(ctx context.Context, token string, id *mailbox.Identity) error {
	return c.bind(ctx, token, id, false)
}

// Rebind moves the account to this identity: "start fresh here", when the
// account's own keys can't be brought to this computer. The @name follows.
// The server asks for the password again for this.
func (c *Client) Rebind(ctx context.Context, token string, id *mailbox.Identity, password string) error {
	return c.bind(ctx, token, id, true, password)
}

func (c *Client) bind(ctx context.Context, token string, id *mailbox.Identity, replace bool, password ...string) error {
	ts := time.Now().Unix()
	sig := id.Signing.Sign(wire.BindMessage(id.Username(), ts))
	body := map[string]any{
		"username": id.Username(),
		"pubkey":   crypto.PublicKeyToHex(id.Signing.PublicKey),
		"ts":       ts,
		"sig":      sig,
	}
	if replace {
		body["replace"] = true
		if len(password) > 0 {
			body["password"] = password[0]
		}
	}
	return c.call(ctx, c.RelayURL, "POST", "/v1/identity", token, body, nil)
}

// ClaimDevice makes this computer the one the account is in use on.
func (c *Client) ClaimDevice(ctx context.Context, token, device string) error {
	return c.call(ctx, c.RelayURL, "POST", "/v1/device", token, map[string]string{"device": device}, nil)
}

// GetBackup fetches the account's locked copy. found is false when there is
// none.
func (c *Client) GetBackup(ctx context.Context, token string) (blob []byte, found bool, err error) {
	blob, status, err := c.raw(ctx, "GET", "/v1/backup", token, "", nil)
	if status == http.StatusNotFound {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return blob, true, nil
}

// PutBackup replaces the account's locked copy with one from this device.
func (c *Client) PutBackup(ctx context.Context, token, device string, blob []byte) error {
	_, _, err := c.raw(ctx, "PUT", "/v1/backup", token, device, blob)
	return err
}

// DeleteBackup removes the account's locked copy from the server.
func (c *Client) DeleteBackup(ctx context.Context, token string) error {
	return c.call(ctx, c.RelayURL, "DELETE", "/v1/backup", token, nil, nil)
}

// ClaimHandle asks for a unique @name and returns the signed record.
func (c *Client) ClaimHandle(ctx context.Context, token, handle string) (*Grant, error) {
	var g Grant
	if err := c.call(ctx, c.RelayURL, "POST", "/v1/handle", token,
		map[string]string{"handle": handle}, &g); err != nil {
		return nil, err
	}
	return &g, nil
}

// RefreshHandle asks for a freshly signed copy of the user's existing name, so
// it doesn't lapse.
func (c *Client) RefreshHandle(ctx context.Context, token string) (*Grant, error) {
	var g Grant
	if err := c.call(ctx, c.RelayURL, "POST", "/v1/handle/refresh", token, struct{}{}, &g); err != nil {
		return nil, err
	}
	return &g, nil
}

// StartDeletion emails the account's address a code for deleting it.
func (c *Client) StartDeletion(ctx context.Context, token string) error {
	return c.call(ctx, c.RelayURL, "POST", "/v1/account/delete/start", token, struct{}{}, nil)
}

// Delete checks the deletion steps given so far -- the emailed code, the
// password, the account's name -- and, with confirm, deletes the account for
// good. The server checks every step again before it deletes anything.
func (c *Client) Delete(ctx context.Context, token, code, password, name string, confirm bool) error {
	return c.call(ctx, c.RelayURL, "POST", "/v1/account/delete", token, map[string]any{
		"code": code, "password": password, "name": name, "confirm": confirm,
	}, nil)
}

// IceServer is one STUN or TURN server for a call, in the shape the
// browser's RTCPeerConnection expects.
type IceServer struct {
	URLs       []string `json:"urls"`
	Username   string   `json:"username,omitempty"`
	Credential string   `json:"credential,omitempty"`
}

// TURNCredentials are short-lived login details for the call relay. They are
// only handed to signed-in accounts, so the relay can't be used by strangers.
type TURNCredentials struct {
	URLs       []string `json:"urls"`
	Username   string   `json:"username"`
	Credential string   `json:"credential"`
	TTL        int      `json:"ttl"`
}

// TURN asks relay-server for call-relay credentials. It fails with a
// "doesn't offer that" error if the server has no call relay set up.
func (c *Client) TURN(ctx context.Context, token string) (*TURNCredentials, error) {
	var t TURNCredentials
	if err := c.call(ctx, c.RelayURL, "GET", "/v1/turn", token, nil, &t); err != nil {
		return nil, err
	}
	return &t, nil
}

// Evidence is one message the reporter chose to include. Sending it is
// always the reporter's decision, never automatic: the server otherwise has
// no way to see message contents.
type Evidence = wire.EvidenceMsg

// Report tells the operator about a user. The returned text is the
// acknowledgement to show the reporter.
func (c *Client) Report(ctx context.Context, token, username, reason string, evidence []Evidence) (string, error) {
	var out struct {
		Message string `json:"message"`
	}
	err := c.call(ctx, c.RelayURL, "POST", "/v1/reports", token, map[string]any{
		"username": username,
		"reason":   reason,
		"evidence": evidence,
	}, &out)
	return out.Message, err
}

// ─── plumbing ───

// maxCopy bounds a locked copy read back from the server.
const maxCopy = 8<<20 + 1024

// raw sends or fetches bytes rather than JSON: the locked copy.
func (c *Client) raw(ctx context.Context, method, path, token, device string, body []byte) ([]byte, int, error) {
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.RelayURL+path, rd)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	if body != nil {
		req.Header.Set("Content-Type", "application/octet-stream")
	}
	if device != "" {
		req.Header.Set("X-Relay-Device", device)
	}
	hc := *c.HTTP
	hc.Timeout = 2 * time.Minute // a copy can be a few megabytes on a slow line
	resp, err := hc.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, 0, ctx.Err()
		}
		return nil, 0, fmt.Errorf("couldn't reach the AltNet account service; check your internet connection and try again")
	}
	defer resp.Body.Close()
	out, err := io.ReadAll(io.LimitReader(resp.Body, maxCopy))
	if err != nil {
		return nil, resp.StatusCode, fmt.Errorf("the copy was cut off on the way; try again")
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, resp.StatusCode, &Error{Status: resp.StatusCode, Message: errorText(resp.StatusCode, out), Code: errorCode(out)}
	}
	return out, resp.StatusCode, nil
}

func (c *Client) call(ctx context.Context, base, method, path, token string, in, out any) error {
	var body io.Reader
	if in != nil {
		blob, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(blob)
	}
	req, err := http.NewRequestWithContext(ctx, method, base+path, body)
	if err != nil {
		return err
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("couldn't reach the AltNet account service; check your internet connection and try again")
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponse))
	if err != nil {
		return fmt.Errorf("the account service's answer was cut off; try again")
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return &Error{Status: resp.StatusCode, Message: errorText(resp.StatusCode, raw), Code: errorCode(raw)}
	}
	if out == nil || len(bytes.TrimSpace(raw)) == 0 {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("the account service sent something unexpected; try again later")
	}
	return nil
}

// errorCode pulls relay-server's {"code": "..."}, if there is one.
func errorCode(raw []byte) string {
	var e struct {
		Code string `json:"code"`
	}
	if json.Unmarshal(raw, &e) != nil || len(e.Code) > 64 {
		return ""
	}
	return e.Code
}

// errorText pulls the {"error": "..."} message both services use. Anything
// else (a proxy's HTML error page, say) becomes a plain sentence rather than
// markup dumped into the window.
func errorText(status int, raw []byte) string {
	var e struct {
		Error string `json:"error"`
	}
	if json.Unmarshal(raw, &e) == nil && strings.TrimSpace(e.Error) != "" {
		msg := strings.TrimSpace(e.Error)
		if len(msg) > 500 {
			msg = msg[:500]
		}
		return msg
	}
	switch status {
	case http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return "the AltNet account service isn't reachable right now; try again shortly"
	case http.StatusNotFound:
		return "the AltNet account service doesn't offer that yet"
	case http.StatusTooManyRequests:
		return "too many attempts; wait a moment and try again"
	}
	return fmt.Sprintf("the account service refused that (error %d)", status)
}
