package mailbox

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"altnet/core/crypto"
	"altnet/core/dht"
)

// PendingMessageLimit is how many messages an unaccepted sender's
// conversation will yield before the recipient accepts the request.
//
// Enforced here, on the RECEIVING side, on purpose. A sender-side limit
// is unenforceable -- a modified client would simply ignore it -- so the
// rule that actually holds is that a recipient refuses to surface more
// than this many messages from someone they haven't accepted. Same
// instinct as the DHT's existing per-peer storage budget, one layer up.
const PendingMessageLimit = 3

// MaxChainWalk bounds how many Notes a single catch-up will follow
// backward, so a sender who wrote a million notes can't make a recipient
// walk a million lookups.
const MaxChainWalk = 512

// Conversation is the local, per-peer state a client persists between
// runs. It holds no secrets: everything here is either public or a hash,
// so the app layer can serialise it as plain JSON alongside its UI state.
type Conversation struct {
	PeerID     string `json:"peer_id"`
	PeerBoxKey string `json:"peer_box_key,omitempty"` // cached from their Profile
	Accepted   bool   `json:"accepted"`               // false = still a message request

	LastSentHash string `json:"last_sent_hash,omitempty"` // tip of our outbound chain
	LastSentSeq  int64  `json:"last_sent_seq,omitempty"`
	LastReadHash string `json:"last_read_hash,omitempty"` // newest inbound Note already surfaced

	// PeerAddrs is where this peer's device can be reached directly,
	// learned from inside their encrypted messages (never from anything
	// public). Empty until they have written to us at least once, which
	// is exactly the point: a stranger holding our username learns
	// nothing about where we are until we choose to answer.
	PeerAddrs []string `json:"peer_addrs,omitempty"`
}

// Client is one identity's view of the mailbox layer: its keys, its
// storage substrate, and optionally a ban list to filter by.
//
// ID MUST be a Relay-specific identity, NOT the node's peer identity.
//
// This is a privacy requirement, not a style preference. A node's peer
// ID is deliberately public in AltNet: peer.sendOn stamps it (and the
// public key) on every message, hello announces it on every connection,
// and Kademlia spreads it further still by handing Contact{ID,...}
// lists around in response to dht_find_node. Peer IDs are meant to
// circulate -- that's how routing works.
//
// A Relay username, by contrast, is only reachable by someone who was
// handed it: every address this package derives (MailboxKey,
// DoorbellKey, ProfileKey) is a hash OF the username, and the DHT
// answers exact-key lookups only, so none of them can be discovered by
// browsing. Reusing the node identity would destroy that -- every node
// on the network would hold a partial directory of usernames in its own
// routing table, and anyone could message anyone.
//
// So: load this from its own key file (see LoadOrCreateBoxKey's sibling
// path convention), never from the daemon's data/keys/peer.key.
// BanChecker reports whether an identity, given as its hex Ed25519 public
// key, is banned. *BanList is one; the operator ban lists in
// altnet-relay/moderation are another. An interface rather than *BanList
// because those lists change -- operators unban people -- and BanList,
// being a plain set for locally configured bans, deliberately has no
// removal.
type BanChecker interface {
	Contains(pubKeyHex string) bool
}

type Client struct {
	Store Store
	ID    *crypto.Identity
	Box   *BoxKey
	Bans  BanChecker // may be nil

	// SelfAddrs, if set, reports where this device can currently be
	// reached, to be included inside outgoing sealed messages so
	// recipients can deliver directly. A function rather than a slice
	// because a node's advertised addresses change as relays come and
	// go. Leave nil to never advertise reachability.
	SelfAddrs func() []string
}

// PublishProfile publishes this client's profile so others can find the
// box key they need to message it. Call once at startup. handle may be
// empty for a user who hasn't claimed one.
func (c *Client) PublishProfile(handle string) (*Profile, error) {
	return PublishProfile(c.Store, c.ID, c.Box, handle)
}

// Send seals text for the conversation's peer, chunks it, stores the
// chunks and the chained Note, and -- if this is the first message in
// this direction -- knocks on the peer's doorbell so they can discover
// the conversation at all.
//
// conv is updated in place with the new chain tip and sequence number,
// and the caller should persist it.
func (c *Client) Send(conv *Conversation, text string) (*Note, error) {
	note, _, err := c.SendText(conv, text)
	return note, err
}

// SendText is Send, additionally returning the plaintext Message that
// was sealed. Callers keeping a local history need the message's ID and
// timestamp, which are generated inside and would otherwise be lost.
func (c *Client) SendText(conv *Conversation, text string) (*Note, *Message, error) {
	return c.send(conv, KindText, text)
}

// SendKind is SendText for a message of another kind, such as KindCall.
func (c *Client) SendKind(conv *Conversation, kind, text string) (*Note, *Message, error) {
	return c.send(conv, kind, text)
}

func (c *Client) send(conv *Conversation, kind, text string) (*Note, *Message, error) {
	out, err := c.Prepare(conv, kind, text)
	if err != nil {
		return nil, nil, err
	}
	if err := c.Commit(conv, out); err != nil {
		return nil, nil, err
	}
	return out.Note, out.Msg, nil
}

// Outgoing is a message sealed, chunked and signed into its place in the
// conversation's chain, but not yet stored on the network. Splitting the two
// steps is what lets a sender hand the message straight to the recipient's
// device first (Chunks is everything they need to open it) and store it for
// the record while that is already on its way.
type Outgoing struct {
	Note   *Note
	Msg    *Message
	Chunks map[string][]byte // hex key -> sealed bytes
	order  []Chunk
}

// Prepare builds the next message in the conversation without touching the
// network -- except, once per conversation, to look up the peer's encryption
// key. conv is not advanced; Commit does that once the message is stored, so a
// message that never got stored never takes a place in the chain.
//
// Callers must not Prepare a second message for the same conversation until
// the first is committed (or abandoned): both would claim the same place.
func (c *Client) Prepare(conv *Conversation, kind, text string) (*Outgoing, error) {
	if conv == nil || conv.PeerID == "" {
		return nil, errors.New("mailbox: conversation needs a peer ID")
	}

	// Resolve the peer's box key. Cached after the first send; the very
	// first message needs a profile lookup, which is the step that makes
	// "I only have your username" work at all.
	if conv.PeerBoxKey == "" {
		prof, err := FetchProfile(c.Store, conv.PeerID)
		if err != nil {
			return nil, fmt.Errorf("mailbox: cannot reach %.8s: %w", conv.PeerID, err)
		}
		conv.PeerBoxKey = prof.BoxKey
	}
	peerBox, err := BoxPublicKeyFromHex(conv.PeerBoxKey)
	if err != nil {
		return nil, err
	}

	msg, err := NewMessage(kind, text)
	if err != nil {
		return nil, err
	}
	if c.SelfAddrs != nil {
		msg.Addrs = c.SelfAddrs()
	}
	plaintext, err := msg.Marshal()
	if err != nil {
		return nil, err
	}
	sealed, err := Seal(peerBox, plaintext)
	if err != nil {
		return nil, err
	}
	chunks, err := SplitChunks(sealed)
	if err != nil {
		return nil, err
	}
	hexes := make([]string, 0, len(chunks))
	byKey := make(map[string][]byte, len(chunks))
	for _, ch := range chunks {
		hexes = append(hexes, ch.Key.Hex())
		byKey[ch.Key.Hex()] = ch.Data
	}
	note, err := NewNote(c.ID, conv.PeerID, hexes, conv.LastSentHash, conv.LastSentSeq+1)
	if err != nil {
		return nil, err
	}
	return &Outgoing{Note: note, Msg: msg, Chunks: byKey, order: chunks}, nil
}

// Commit stores a prepared message and advances the conversation to it.
//
// Stores that don't depend on each other run at the same time: all the
// chunks together, then both copies of the note together. The order between
// those two groups is kept on purpose -- someone told the note has arrived
// must be able to fetch its chunks.
func (c *Client) Commit(conv *Conversation, out *Outgoing) error {
	if out == nil || out.Note == nil {
		return errors.New("mailbox: nothing to commit")
	}
	if out.Note.Prev != conv.LastSentHash || out.Note.Seq != conv.LastSentSeq+1 {
		return errors.New("mailbox: the conversation moved on since this message was prepared")
	}
	puts := make([]put, 0, len(out.order))
	for _, ch := range out.order {
		puts = append(puts, put{ch.Key, ch.Data})
	}
	if err := c.putAll(puts); err != nil {
		return fmt.Errorf("mailbox: store chunk: %w", err)
	}
	if err := c.putNote(out.Note); err != nil {
		return err
	}

	firstEver := conv.LastSentSeq == 0
	conv.LastSentHash = out.Note.Hash()
	conv.LastSentSeq = out.Note.Seq

	// First contact: leave a card on their doorbell. Without this the
	// recipient has no way to learn our ID, and therefore no way to
	// compute the mailbox key this note lives at.
	if firstEver {
		if err := c.knock(conv.PeerID, out.Note.Hash()); err != nil {
			return fmt.Errorf("mailbox: note stored but knock failed: %w", err)
		}
	}
	return nil
}

type put struct {
	key   dht.NodeID
	value []byte
}

// putAll stores several values at once and reports the first failure. Each
// store is a separate lookup of the nodes closest to its key, and those
// lookups don't depend on each other, so running them side by side costs
// about as long as the slowest one instead of the sum.
func (c *Client) putAll(ps []put) error {
	if len(ps) == 1 {
		return c.Store.Put(ps[0].key, ps[0].value)
	}
	errs := make(chan error, len(ps))
	for _, p := range ps {
		go func(p put) { errs <- c.Store.Put(p.key, p.value) }(p)
	}
	var first error
	for range ps {
		if err := <-errs; err != nil && first == nil {
			first = err
		}
	}
	return first
}

// putNote writes both copies of a Note, side by side: the immutable archive
// at its own content address (so the chain stays walkable once the tip moves
// on) and the mutable tip at the conversation's mailbox key.
func (c *Client) putNote(n *Note) error {
	blob, err := json.Marshal(n)
	if err != nil {
		return err
	}
	hash, err := dht.IDFromHex(n.Hash())
	if err != nil {
		return err
	}
	slot := MailboxKey(c.ID.ID(), n.Recipient)
	if err := c.putAll([]put{{hash, blob}, {slot, blob}}); err != nil {
		return fmt.Errorf("mailbox: store note: %w", err)
	}
	return nil
}

// Receive walks the peer's inbound chain from its tip back to whatever
// this client last surfaced, and returns the new messages oldest-first.
//
// conv.LastReadHash is advanced to the new tip, so the next call returns
// only what arrived since. If the conversation hasn't been accepted yet,
// at most PendingMessageLimit messages are ever surfaced.
//
// The pending cap is only as strong as the chain it counts along, so for
// an unaccepted conversation the chain is held to three rules, all of
// which honest senders satisfy by construction:
//
//   - Seq steps down by exactly one per link. Otherwise a sender could
//     stamp every note Seq 1 and have all of them count as "first".
//   - Seq never drops below 1 (checkNoteShape), so the countdown can't
//     continue into zero and negative numbers.
//   - Once something has been surfaced, new notes must chain back to it.
//     Otherwise a sender could abandon their chain and start a fresh one
//     at Seq 1 for a fresh allowance, as often as they liked. A chain
//     that doesn't connect surfaces nothing and leaves the cursor where
//     it was; accepting the conversation reveals it in full.
func (c *Client) Receive(conv *Conversation) ([]*Message, error) {
	if conv == nil || conv.PeerID == "" {
		return nil, errors.New("mailbox: conversation needs a peer ID")
	}
	self := c.ID.ID()

	tip, err := c.fetchTipNote(conv.PeerID, self)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil, nil // nothing sent to us yet
		}
		return nil, err
	}

	// A pending conversation whose cursor already sits at or past the
	// last position the cap allows has nothing left to surface: anything
	// new must chain after the cursor, so its Seq is higher still.
	// Checking that costs one fetch, where the walk below would cost up
	// to MaxChainWalk, and a stranger can trigger a Receive with every
	// note they write. The cursor stays put; accepting rewinds it anyway.
	if !conv.Accepted && conv.LastReadHash != "" && tip.Hash() != conv.LastReadHash {
		last, err := c.fetchNoteByHash(conv.LastReadHash, conv.PeerID, self)
		if err == nil && last.Seq >= PendingMessageLimit {
			return nil, nil
		}
	}

	// Walk backward collecting notes newer than what we've already
	// surfaced, newest-first, then reverse at the end.
	var chain []*Note
	seen := make(map[string]bool)
	connected := conv.LastReadHash == ""
	cur := tip
	for steps := 0; cur != nil && steps < MaxChainWalk; steps++ {
		h := cur.Hash()
		if h == conv.LastReadHash {
			connected = true
			break
		}
		if seen[h] {
			break
		}
		seen[h] = true
		if conv.Accepted || cur.Seq <= PendingMessageLimit {
			chain = append(chain, cur)
		}
		if cur.Prev == "" {
			break
		}
		prev, err := c.fetchNoteByHash(cur.Prev, conv.PeerID, self)
		if err != nil {
			// A missing or unverifiable link truncates the history we
			// can prove; surface what we have rather than failing the
			// whole read.
			break
		}
		if prev.Seq != cur.Seq-1 {
			// A link that doesn't hold the position it claims is not
			// part of any chain an honest client writes. Treat it like
			// a missing link.
			break
		}
		cur = prev
	}

	if !conv.Accepted && !connected {
		return nil, nil
	}

	messages := make([]*Message, 0, len(chain))
	for i := len(chain) - 1; i >= 0; i-- {
		msg, err := c.openNote(chain[i])
		if err != nil {
			continue // one unreadable message shouldn't sink the rest
		}
		// Learn where they can be reached directly. Newest message
		// wins, since addresses change as relays come and go.
		if addrs := sanitizeAddrs(msg.Addrs); len(addrs) > 0 {
			conv.PeerAddrs = addrs
		}
		messages = append(messages, msg)
	}
	conv.LastReadHash = tip.Hash()
	return messages, nil
}

// NoteChunks gathers the stored chunks a Note references, so a caller
// can hand the whole message to a peer directly instead of making them
// fetch it. Called right after Send, when the chunks are still in the
// local store, so this does not hit the network.
func (c *Client) NoteChunks(n *Note) (map[string][]byte, error) {
	if len(n.Chunks) > MaxChunksPerMessage {
		return nil, errors.New("mailbox: note references too many chunks")
	}
	out := make(map[string][]byte, len(n.Chunks))
	for _, hexKey := range n.Chunks {
		key, err := dht.IDFromHex(hexKey)
		if err != nil {
			return nil, err
		}
		data, err := c.Store.Get(key)
		if err != nil {
			return nil, err
		}
		out[hexKey] = data
	}
	return out, nil
}

// OpenDelivered verifies and decrypts a message handed to us directly by
// a peer, rather than fetched from the DHT.
//
// Everything the DHT path checks is re-checked here, because a direct
// sender is strictly less trustworthy than content-addressed storage:
// the note must be signed by the expected sender and addressed to us,
// the sender must not be banned, and every chunk's bytes must hash to
// the hash the note claims. Without that last check a peer could hand
// over a validly-signed note alongside substituted chunk data.
//
// Unaccepted conversations are refused outright. The pending cap is
// enforced by walking the chain (see Receive), and a lone note handed
// over directly proves nothing about where it sits in one: a stranger
// could sign any number of distinct notes all claiming Seq 1. Their
// messages still arrive, by the DHT path, which the sender always writes
// first.
//
// conv is only modified after everything has checked out, and PeerAddrs
// only when the note directly extends what Receive last surfaced. Notes
// and chunks are public in the DHT, so anyone can replay an old one at
// us; without that check a replay would roll PeerAddrs back to wherever
// the peer used to be, and our next send would dial it.
func (c *Client) OpenDelivered(conv *Conversation, n *Note, chunks map[string][]byte) (*Message, error) {
	if conv == nil || conv.PeerID == "" {
		return nil, errors.New("mailbox: conversation needs a peer ID")
	}
	if n == nil {
		return nil, errors.New("mailbox: no note delivered")
	}
	if !conv.Accepted {
		return nil, errors.New("mailbox: direct delivery refused for an unaccepted sender")
	}
	if err := checkNoteShape(n); err != nil {
		return nil, err
	}
	if isFutureDated(n.Timestamp, time.Now().Unix()) {
		// fetchTipNote wouldn't believe this note yet either.
		return nil, errors.New("mailbox: delivered note is future-dated")
	}
	if len(chunks) > len(n.Chunks) {
		return nil, errors.New("mailbox: delivery carries chunks the note doesn't reference")
	}
	if c.banned(n.Sender) {
		return nil, errors.New("mailbox: note from a banned identity")
	}
	if err := n.VerifyFrom(conv.PeerID, c.ID.ID()); err != nil {
		return nil, err
	}

	pieces := make([][]byte, 0, len(n.Chunks))
	for _, hexKey := range n.Chunks {
		data, ok := chunks[hexKey]
		if !ok {
			return nil, fmt.Errorf("mailbox: delivery missing chunk %.8s", hexKey)
		}
		key, err := dht.IDFromHex(hexKey)
		if err != nil {
			return nil, err
		}
		if ContentAddress(data) != key {
			return nil, fmt.Errorf("mailbox: delivered chunk %.8s does not match its hash", hexKey)
		}
		pieces = append(pieces, data)
	}

	plaintext, err := Open(c.Box.Private, JoinChunks(pieces))
	if err != nil {
		return nil, err
	}
	msg, err := openMessage(plaintext)
	if err != nil {
		return nil, err
	}
	if n.Prev == conv.LastReadHash {
		if addrs := sanitizeAddrs(msg.Addrs); len(addrs) > 0 {
			conv.PeerAddrs = addrs
		}
	}
	return msg, nil
}

// openNote fetches a note's chunks, reassembles them, and decrypts.
func (c *Client) openNote(n *Note) (*Message, error) {
	if err := checkNoteShape(n); err != nil {
		return nil, err
	}
	pieces := make([][]byte, 0, len(n.Chunks))
	for _, hexKey := range n.Chunks {
		key, err := dht.IDFromHex(hexKey)
		if err != nil {
			return nil, err
		}
		data, err := c.Store.Get(key)
		if err != nil {
			return nil, fmt.Errorf("mailbox: missing chunk %.8s: %w", hexKey, err)
		}
		pieces = append(pieces, data)
	}
	plaintext, err := Open(c.Box.Private, JoinChunks(pieces))
	if err != nil {
		return nil, err
	}
	return openMessage(plaintext)
}

// openMessage parses a decrypted envelope and rejects one no honest
// client writes. The ID matters most: it is the key of every dedup set
// above this package, so an empty one would slip past them (or be
// dropped by them, depending on the set) and an oversized one would make
// each entry arbitrarily expensive to hold.
func openMessage(plaintext []byte) (*Message, error) {
	msg, err := UnmarshalMessage(plaintext)
	if err != nil {
		return nil, err
	}
	if msg.ID == "" || len(msg.ID) > maxMessageIDLen {
		return nil, errors.New("mailbox: message has a malformed ID")
	}
	return msg, nil
}

// SyncSent moves the conversation's outgoing chain up to the newest note this
// identity has stored for it, if that is further than conv knows -- written
// from another computer before this one took over the account. It reports
// whether conv changed; the caller holds the conversation's send lock.
func (c *Client) SyncSent(conv *Conversation) (bool, error) {
	tip, err := c.fetchTipNote(c.ID.ID(), conv.PeerID)
	if errors.Is(err, ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if tip.Seq <= conv.LastSentSeq {
		return false, nil
	}
	conv.LastSentHash, conv.LastSentSeq = tip.Hash(), tip.Seq
	return true, nil
}

// fetchTipNote resolves the mailbox slot to the highest-Seq Note that
// verifies as being from senderID and addressed to us.
func (c *Client) fetchTipNote(senderID, self string) (*Note, error) {
	candidates, err := c.Store.GetAll(MailboxKey(senderID, self))
	if err != nil {
		return nil, err
	}
	now := time.Now().Unix()
	var best *Note
	for _, blob := range boundCandidates(candidates, maxSlotCandidates) {
		var n Note
		if err := json.Unmarshal(blob, &n); err != nil {
			continue
		}
		if checkNoteShape(&n) != nil || c.banned(n.Sender) {
			continue
		}
		// A date past clock skew is not believed until its time comes,
		// the rule knocks, profiles and handles follow too, so nothing
		// above this package ever sees a note from the future. Only the
		// tip is checked: links behind it are pinned by hash, and
		// rejecting one there would cut off genuine history written
		// while the sender's clock ran fast.
		if isFutureDated(n.Timestamp, now) {
			continue
		}
		if err := n.VerifyFrom(senderID, self); err != nil {
			continue
		}
		if best == nil || n.Seq > best.Seq {
			rec := n
			best = &rec
		}
	}
	if best == nil {
		return nil, ErrNotFound
	}
	return best, nil
}

// fetchNoteByHash retrieves an archived Note by its content address and
// re-verifies it. Content addressing means a tampered archive copy
// simply won't be returned by Get, but we re-check the signature and
// bindings anyway rather than trusting the store.
func (c *Client) fetchNoteByHash(hash, senderID, self string) (*Note, error) {
	key, err := dht.IDFromHex(hash)
	if err != nil {
		return nil, err
	}
	blob, err := c.Store.Get(key)
	if err != nil {
		return nil, err
	}
	if len(blob) > maxRecordSize {
		return nil, errors.New("mailbox: archived note is oversized")
	}
	var n Note
	if err := json.Unmarshal(blob, &n); err != nil {
		return nil, err
	}
	if err := checkNoteShape(&n); err != nil {
		return nil, err
	}
	if c.banned(n.Sender) {
		return nil, errors.New("mailbox: note from a banned identity")
	}
	if err := n.VerifyFrom(senderID, self); err != nil {
		return nil, err
	}
	return &n, nil
}

func (c *Client) banned(senderHexKey string) bool {
	return c.Bans != nil && c.Bans.Contains(senderHexKey)
}
