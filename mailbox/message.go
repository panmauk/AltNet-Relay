package mailbox

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"
)

// MaxTextLen caps a single text message. Generous for chat, but bounded
// so one message can't be used to push megabytes into other people's
// stores. Larger payloads belong in a future attachment kind, which
// carries its own content root rather than inlining bytes here.
const MaxTextLen = 16 * 1024

// Message kinds. Kept as a string so old clients can recognise and skip
// a kind they don't understand instead of failing to parse the envelope.
const (
	KindText = "text"

	// KindCall carries the setup messages of a voice call (an offer, an
	// answer, a hang-up). They ride the same sealed channel as chat, so only
	// the two people can read them, but they are not chat: they never enter
	// the conversation history and never count toward a message request's
	// limit of three. Older clients see an unknown kind and skip it.
	KindCall = "call"

	// KindGroup carries group chat: a message, an invitation, a change of
	// members, or someone leaving. Each member gets their own sealed copy;
	// see engine/group.go.
	KindGroup = "group"
)

// Message is the plaintext envelope: what's actually inside the sealed
// blob a Note points at. It is never written to the network unencrypted
// -- Seal wraps it first.
//
// ID is a random per-message identifier used for local deduplication:
// the same message can legitimately be discovered twice (once from a
// live delivery, once from walking the chain after a reconnect) and the
// UI should show it once.
// Addrs carries the sender's current node addresses so the recipient can
// deliver future messages straight to their device instead of waiting on
// the DHT.
//
// It lives INSIDE the sealed envelope on purpose. Putting reachability
// in the public Profile would hand every stranger holding a username a
// way to locate that user's machine. Here only the person being written
// to can read it, which produces the right asymmetry: a sender reveals
// where they are by choosing to make contact, while the recipient
// reveals nothing back until they choose to reply.
type Message struct {
	ID    string   `json:"id"`
	Kind  string   `json:"kind"`
	Text  string   `json:"text,omitempty"`
	Sent  int64    `json:"sent"` // unix seconds, per the sender's clock
	Addrs []string `json:"addrs,omitempty"`
}

// NewTextMessage builds a text Message with a fresh random ID and the
// current timestamp.
func NewTextMessage(text string) (*Message, error) { return NewMessage(KindText, text) }

// NewMessage builds a Message of the given kind with a fresh random ID and
// the current timestamp.
func NewMessage(kind, text string) (*Message, error) {
	if len(text) > MaxTextLen {
		return nil, fmt.Errorf("mailbox: message too long (%d > %d)", len(text), MaxTextLen)
	}
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return nil, fmt.Errorf("mailbox: message id: %w", err)
	}
	return &Message{
		ID:   hex.EncodeToString(b[:]),
		Kind: kind,
		Text: text,
		Sent: time.Now().Unix(),
	}, nil
}

// Marshal serialises the message for sealing.
func (m *Message) Marshal() ([]byte, error) { return json.Marshal(m) }

// UnmarshalMessage parses a decrypted message envelope.
func UnmarshalMessage(data []byte) (*Message, error) {
	var m Message
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("mailbox: parse message: %w", err)
	}
	return &m, nil
}
