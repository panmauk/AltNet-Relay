package notify

import (
	"encoding/base64"
	"encoding/json"
	"sync"

	"altnet/core/peer"

	"altnet-relay/mailbox"
)

// MsgDeliver hands a message straight to the recipient's device,
// skipping the store-then-discover round trip.
const MsgDeliver = "relay_deliver"

// MaxDeliverChunks bounds how much a peer can push at us in one direct
// delivery, independent of what the note claims.
const MaxDeliverChunks = mailbox.MaxChunksPerMessage

var (
	// maxChunkB64 is the longest base64 text one honest chunk encodes
	// to. Checked before decoding, so a peer can't make us allocate for
	// a chunk no sender could have produced.
	maxChunkB64 = base64.StdEncoding.EncodedLen(mailbox.ChunkSize)

	// maxDeliverPayload is the largest delivery worth parsing at all: a
	// full-size message plus generous room for the note and the JSON
	// around it. core/peer caps a message at 1 MiB today, but that is
	// its limit to change, and this parse shouldn't depend on it.
	maxDeliverPayload = MaxDeliverChunks*maxChunkB64 + 64*1024
)

// maxDeliverAddrs bounds how many addresses one Deliver will dial.
// PeerAddrs is peer-chosen, and Conversation JSON persisted before
// mailbox bounded it on the way in may still hold a long list.
const maxDeliverAddrs = 8

// deliverPayload is a whole message: the signed note plus the chunk
// bytes it references, so the recipient needs no DHT lookup at all.
type deliverPayload struct {
	Note   *mailbox.Note     `json:"note"`
	Chunks map[string]string `json:"chunks"` // hex chunk hash -> base64 bytes
}

// Courier delivers messages directly between devices that are both
// online, and receives such deliveries.
//
// It is a fast path, never the source of truth. Every message is also
// written to the DHT by the normal Send path, because direct delivery
// alone would lose anything sent while the recipient's app was closed,
// break reading history from another device, and vanish if the
// connection dropped mid-send. Direct delivery only removes the waiting.
//
// Reachability comes from mailbox.Conversation.PeerAddrs, which is
// learned from inside the peer's encrypted messages -- never from
// anything public -- so this never widens who can locate a user.
type Courier struct {
	p      *peer.Peer
	client *mailbox.Client

	mu       sync.Mutex
	onDirect func(senderID string, note *mailbox.Note, chunks map[string][]byte)
}

// NewCourier registers a Courier as a peer handler so it receives
// direct deliveries.
func NewCourier(p *peer.Peer, client *mailbox.Client) *Courier {
	c := &Courier{p: p, client: client}
	p.AddHandler(c)
	return c
}

// SetOnDirect installs the callback fired when a peer delivers a message
// to us directly. Safe to call while running.
func (c *Courier) SetOnDirect(fn func(senderID string, note *mailbox.Note, chunks map[string][]byte)) {
	c.mu.Lock()
	c.onDirect = fn
	c.mu.Unlock()
}

// HandleMessage implements peer.Handler.
func (c *Courier) HandleMessage(_ *peer.Peer, _ string, msg peer.Message) {
	if msg.Type != MsgDeliver {
		return
	}
	chunks, note, ok := decodeDelivery(msg.Payload)
	if !ok {
		return
	}

	// The note carries its own signer; the caller resolves that against
	// the conversation it belongs to and re-verifies everything. We do
	// NOT trust msg.From to say who sent this -- the note's signature is
	// the only thing that establishes authorship.
	senderID, err := note.SenderID()
	if err != nil {
		return
	}

	c.mu.Lock()
	cb := c.onDirect
	c.mu.Unlock()
	if cb != nil {
		cb(senderID, note, chunks)
	}
}

// decodeDelivery parses a relay_deliver payload, doing only work whose
// size is bounded by constants: the payload is length-checked before it
// is parsed, and every chunk is checked -- referenced by the note, and
// no longer than an honest chunk encodes to -- before it is decoded.
// Everything it returns is still unverified; OpenDelivered does that.
func decodeDelivery(payload string) (map[string][]byte, *mailbox.Note, bool) {
	if len(payload) > maxDeliverPayload {
		return nil, nil, false
	}
	var dp deliverPayload
	if err := json.Unmarshal([]byte(payload), &dp); err != nil {
		return nil, nil, false
	}
	if dp.Note == nil || len(dp.Chunks) == 0 || len(dp.Chunks) > MaxDeliverChunks ||
		len(dp.Note.Chunks) > MaxDeliverChunks || len(dp.Chunks) > len(dp.Note.Chunks) {
		return nil, nil, false
	}
	wanted := make(map[string]bool, len(dp.Note.Chunks))
	for _, k := range dp.Note.Chunks {
		wanted[k] = true
	}
	chunks := make(map[string][]byte, len(dp.Chunks))
	for hexKey, b64 := range dp.Chunks {
		if !wanted[hexKey] || len(b64) > maxChunkB64 {
			return nil, nil, false
		}
		raw, err := base64.StdEncoding.DecodeString(b64)
		if err != nil {
			return nil, nil, false
		}
		chunks[hexKey] = raw
	}
	return chunks, dp.Note, true
}

// Deliver pushes a note and its chunks to a peer at any of addrs,
// returning true if one accepted the connection.
//
// Best effort by design: a failure here is not an error, it just means
// the message arrives by the normal DHT path instead. It may run before the
// message is stored: the recipient surfaces a pushed message without moving
// its read position, and recognises the stored copy by its ID later.
func (c *Courier) Deliver(addrs []string, note *mailbox.Note, chunks map[string][]byte) bool {
	if len(addrs) == 0 || note == nil || len(chunks) == 0 {
		return false
	}
	encoded := make(map[string]string, len(chunks))
	for hexKey, raw := range chunks {
		encoded[hexKey] = base64.StdEncoding.EncodeToString(raw)
	}
	body, err := json.Marshal(deliverPayload{Note: note, Chunks: encoded})
	if err != nil {
		return false
	}
	if len(body) > maxDeliverPayload {
		return false // the recipient would refuse it unread
	}
	out := peer.Message{Type: MsgDeliver, Payload: string(body)}

	if len(addrs) > maxDeliverAddrs {
		addrs = addrs[:maxDeliverAddrs]
	}
	for _, addr := range addrs {
		if addr == "" {
			continue
		}
		if err := c.p.EnsureConnected(addr); err != nil {
			continue
		}
		if err := c.p.Send(addr, out); err == nil {
			return true
		}
	}
	return false
}
