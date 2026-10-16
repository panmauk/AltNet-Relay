package server

import (
	"crypto/hmac"
	"crypto/sha1" //nolint:gosec // coturn's REST-credential scheme is defined as HMAC-SHA1
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"net/http"
	"strconv"
	"time"
)

// TURNConfig lets signed-in accounts use the call relay (coturn) on the
// same machine, for calls between two computers whose routers won't let
// them reach each other directly.
//
// The relay is configured with the same shared secret ("use-auth-secret").
// Credentials are derived from it on the fly, expire on their own, and are
// only ever given to a signed-in account -- so an open relay isn't left
// lying around for strangers to push traffic through.
type TURNConfig struct {
	Secret []byte        // coturn's static-auth-secret
	URLs   []string      // turn:host:3478?transport=udp, and so on
	TTL    time.Duration // how long a credential works; 0 means an hour
}

// SetTURN turns on the /v1/turn endpoint.
func (s *Server) SetTURN(c *TURNConfig) { s.turn = c }

// TURNCredentialsFor derives a username and password for coturn's REST
// scheme: the username is an expiry time plus a label, the password is an
// HMAC of it under the shared secret.
func TURNCredentialsFor(secret []byte, label string, expires time.Time) (username, credential string) {
	username = strconv.FormatInt(expires.Unix(), 10) + ":" + label
	mac := hmac.New(sha1.New, secret)
	mac.Write([]byte(username))
	return username, base64.StdEncoding.EncodeToString(mac.Sum(nil))
}

func (s *Server) handleTURN(w http.ResponseWriter, _ *http.Request, p Principal) {
	if s.turn == nil || len(s.turn.Secret) == 0 || len(s.turn.URLs) == 0 {
		writeError(w, http.StatusNotFound, "the call relay isn't set up on this server")
		return
	}
	ttl := s.turn.TTL
	if ttl <= 0 {
		ttl = time.Hour
	}
	// The label lets the operator tie relay traffic back to an account if it
	// is ever abused, without putting an email address in a log line.
	sum := sha256.Sum256([]byte(p.Email))
	user, cred := TURNCredentialsFor(s.turn.Secret, hex.EncodeToString(sum[:4]), s.now().Add(ttl))
	writeJSON(w, http.StatusOK, map[string]any{
		"urls": s.turn.URLs, "username": user, "credential": cred, "ttl": int(ttl / time.Second),
	})
}
