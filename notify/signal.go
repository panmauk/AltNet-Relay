package notify

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"sync"
	"time"

	"altnet/core/crypto"
	"altnet/core/peer"

	"altnet-relay/mailbox"
)

// MsgSignal carries small, short-lived signals straight between two
// people's devices: "I'm typing" and "your messages reached me". They are
// never stored anywhere -- if the other device isn't online they simply
// don't arrive, which is exactly right for things that are only true for a
// moment.
//
// Everything about a signal is inside the sealed box: who sent it, what it
// says, and the signature that proves it. The relay a signal passes through
// sees an opaque blob between two machines, not who is typing to whom.
const MsgSignal = "relay_signal"

// Signal kinds.
const (
	SignalTyping    = "typing"
	SignalDelivered = "delivered"
)

const (
	// signalFreshness bounds how old a signal may be. A replayed "typing"
	// is harmless, but there's no reason to accept one from a minute ago.
	signalFreshness = 60 * time.Second
	// maxSignalPayload bounds what we parse at all.
	maxSignalPayload = 8 * 1024
	// MaxSignalIDs bounds one "delivered" signal's list.
	MaxSignalIDs = 64

	signalDomain = "altnet-relay-signal-v1\n"
)

// Signal is what one device tells another.
type Signal struct {
	Kind  string   `json:"k"`
	Group string   `json:"g,omitempty"`   // set when it's about a group: typing there, a group message delivered
	IDs   []string `json:"ids,omitempty"` // for "delivered": the message IDs
	To    string   `json:"to"`            // the recipient's username: a signal can't be redirected
	TS    int64    `json:"ts"`
}

// signalInner is sealed to the recipient's box key.
type signalInner struct {
	From string `json:"from"` // sender's username
	PK   string `json:"pk"`   // sender's public key; must hash to From
	Body []byte `json:"body"` // the Signal, as signed
	Sig  string `json:"sig"`
}

// Signals sends and receives signals.
type Signals struct {
	p      *peer.Peer
	client *mailbox.Client

	mu sync.Mutex
	on func(from string, s Signal)
}

// NewSignals registers a Signals as a peer handler.
func NewSignals(p *peer.Peer, client *mailbox.Client) *Signals {
	s := &Signals{p: p, client: client}
	p.AddHandler(s)
	return s
}

// OnSignal installs the callback for verified signals. from is the sender's
// Relay username; whether they're someone we want to hear from is the
// caller's decision. Runs on the peer's read goroutine: keep it quick.
func (s *Signals) OnSignal(fn func(from string, sig Signal)) {
	s.mu.Lock()
	s.on = fn
	s.mu.Unlock()
}

// Send seals and signs a signal for the conversation's peer and pushes it to
// their device. Best effort: false means it didn't go anywhere, which for a
// signal is fine.
func (s *Signals) Send(conv *mailbox.Conversation, kind string, ids []string) bool {
	return s.SendFor(conv, kind, "", ids)
}

// SendFor is Send for a signal about a group.
func (s *Signals) SendFor(conv *mailbox.Conversation, kind, group string, ids []string) bool {
	if conv == nil || conv.PeerBoxKey == "" || len(conv.PeerAddrs) == 0 || len(ids) > MaxSignalIDs {
		return false
	}
	box, err := mailbox.BoxPublicKeyFromHex(conv.PeerBoxKey)
	if err != nil {
		return false
	}
	body, err := json.Marshal(Signal{Kind: kind, Group: group, IDs: ids, To: conv.PeerID, TS: time.Now().Unix()})
	if err != nil {
		return false
	}
	id := s.client.ID
	inner, err := json.Marshal(signalInner{
		From: id.ID(),
		PK:   crypto.PublicKeyToHex(id.PublicKey),
		Body: body,
		Sig:  id.Sign(append([]byte(signalDomain), body...)),
	})
	if err != nil {
		return false
	}
	sealed, err := mailbox.Seal(box, inner)
	if err != nil {
		return false
	}
	out := peer.Message{Type: MsgSignal, Payload: base64.StdEncoding.EncodeToString(sealed)}
	addrs := conv.PeerAddrs
	if len(addrs) > maxDeliverAddrs {
		addrs = addrs[:maxDeliverAddrs]
	}
	for _, addr := range addrs {
		if addr == "" || s.p.EnsureConnected(addr) != nil {
			continue
		}
		if s.p.Send(addr, out) == nil {
			return true
		}
	}
	return false
}

// HandleMessage implements peer.Handler.
func (s *Signals) HandleMessage(_ *peer.Peer, _ string, msg peer.Message) {
	if msg.Type != MsgSignal {
		return
	}
	from, sig, err := s.open(msg.Payload)
	if err != nil {
		return
	}
	s.mu.Lock()
	cb := s.on
	s.mu.Unlock()
	if cb != nil {
		cb(from, sig)
	}
}

// open unseals and verifies a signal: addressed to us, signed by the key it
// names, that key belonging to the username it claims, and recent.
func (s *Signals) open(payload string) (string, Signal, error) {
	var sig Signal
	if len(payload) > maxSignalPayload {
		return "", sig, errors.New("signal too large")
	}
	sealed, err := base64.StdEncoding.DecodeString(payload)
	if err != nil {
		return "", sig, err
	}
	plain, err := mailbox.Open(s.client.Box.Private, sealed)
	if err != nil {
		return "", sig, err
	}
	var in signalInner
	if err := json.Unmarshal(plain, &in); err != nil {
		return "", sig, err
	}
	pub, err := crypto.PublicKeyFromHex(in.PK)
	if err != nil || crypto.PublicKeyToID(pub) != in.From {
		return "", sig, errors.New("signal key doesn't match its sender")
	}
	if err := crypto.Verify(pub, append([]byte(signalDomain), in.Body...), in.Sig); err != nil {
		return "", sig, err
	}
	if err := json.Unmarshal(in.Body, &sig); err != nil {
		return "", sig, err
	}
	if sig.To != s.client.ID.ID() {
		return "", sig, errors.New("signal is for someone else")
	}
	if age := time.Since(time.Unix(sig.TS, 0)); age > signalFreshness || age < -signalFreshness {
		return "", sig, errors.New("stale signal")
	}
	if len(sig.IDs) > MaxSignalIDs {
		return "", sig, errors.New("too many ids")
	}
	return in.From, sig, nil
}
