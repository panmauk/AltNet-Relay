package notify

import (
	"sync"

	"altnet/core/dht"

	"altnet-relay/mailbox"
)

// wakeBuffer bounds the pending wake-up queue. Wakes are coalescing by
// nature -- each one just means "go look at this key" -- so dropping one
// when the queue is saturated is safe: whichever wake does get through
// will pick up everything new anyway.
const wakeBuffer = 256

// maxDirectInFlight bounds direct deliveries being processed at once.
// Each one is a goroutine waiting its turn at pollMu, and anyone who can
// reach us can send them, so without a bound a flood of deliveries is a
// flood of parked goroutines. Dropping one is safe for the same reason
// dropping a wake is: every message is also on the DHT, and arrives by
// that path.
const maxDirectInFlight = 16

// Inbox wires a Hub to a mailbox.Client so a user's device is told about
// new messages instead of having to check.
//
// It watches exactly two kinds of address:
//
//   - the user's own doorbell, which is ONE address regardless of how
//     many people might ever try to reach them;
//   - the inbound mailbox slot of each conversation they've added.
//
// That bound is the whole point of the doorbell design: watching cost
// grows with your contact list, not with the size of the network.
// maxSeenMessages bounds the delivered-message dedup set. Comfortably
// larger than any burst, and pruned wholesale when exceeded -- losing
// old entries only risks re-showing a message old enough that its note
// has already been walked past, which cannot happen in practice.
const maxSeenMessages = 4096

type Inbox struct {
	client  *mailbox.Client
	hub     *Hub
	courier *Courier // may be nil: direct delivery is optional

	mu    sync.Mutex
	convs map[dht.NodeID]*mailbox.Conversation // inbound mailbox key -> conversation

	// seen dedups by Message.ID. A message can legitimately arrive
	// twice -- once pushed directly by the sender, once found by
	// walking the chain after a notification -- and the user must see
	// it once.
	seen map[string]bool

	// pollMu serializes all fetching. One outgoing message produces
	// several DHT stores (chunks, the archived note, the slot tip), so
	// notifications arrive in bursts; without this, concurrent Receive
	// calls would race on the conversation's read cursor and could
	// deliver the same message twice or skip one.
	pollMu sync.Mutex

	wake   chan dht.NodeID
	direct chan struct{} // semaphore: direct deliveries in flight
	stopCh chan struct{}
	once   sync.Once

	// OnMessages fires with newly-arrived messages for a conversation.
	// Never called with an empty slice. Called from the Inbox's own
	// worker goroutine, never the peer read loop, so it may block.
	OnMessages func(conv *mailbox.Conversation, msgs []*mailbox.Message)

	// OnKnock fires when a stranger leaves a first-contact card. The
	// conversation is pending (not accepted), so at most
	// mailbox.PendingMessageLimit messages will ever be surfaced until
	// the user accepts it.
	OnKnock func(conv *mailbox.Conversation, knock *mailbox.Knock)

	// Skip, if set, reports peers whose first-contact knocks should be
	// ignored entirely -- requests the user already declined, and later,
	// people they've blocked. A knock stays on the doorbell for good, so
	// without this a declined sender would reappear on every restart.
	// Checked before anything is followed or watched, so a skipped peer
	// costs nothing beyond reading their card.
	Skip func(peerID string) bool
}

// NewInbox builds an Inbox over a client and hub. Call Start to begin.
func NewInbox(client *mailbox.Client, hub *Hub) *Inbox {
	return &Inbox{
		client: client,
		hub:    hub,
		convs:  make(map[dht.NodeID]*mailbox.Conversation),
		seen:   make(map[string]bool),
		wake:   make(chan dht.NodeID, wakeBuffer),
		direct: make(chan struct{}, maxDirectInFlight),
		stopCh: make(chan struct{}),
	}
}

// WithCourier enables the direct-delivery fast path. Optional: without
// it everything still works, just via the DHT.
func (in *Inbox) WithCourier(c *Courier) *Inbox {
	in.courier = c
	c.SetOnDirect(in.handleDirect)
	return in
}

// Start registers the notification callback, starts the worker, and
// begins watching the user's doorbell. Add conversations with Follow.
func (in *Inbox) Start() {
	in.hub.SetOnNotify(in.handleNotify)
	in.hub.Start()
	go in.worker()
	in.hub.Watch(in.doorbellKey())
}

// Send writes a message to the network and, when the recipient's device
// is reachable, also hands it to them directly so it arrives without
// waiting on DHT propagation.
//
// The DHT write always happens and always happens first: direct
// delivery is an accelerator, never the record. If the peer is offline,
// or the direct attempt fails for any reason, the message is already
// stored and will be found the normal way.
func (in *Inbox) Send(conv *mailbox.Conversation, text string) (*mailbox.Note, error) {
	note, _, err := in.SendText(conv, text)
	return note, err
}

// SendText is Send, also returning the sealed Message so the caller can
// record it in local history under its real ID.
func (in *Inbox) SendText(conv *mailbox.Conversation, text string) (*mailbox.Note, *mailbox.Message, error) {
	return in.SendKind(conv, mailbox.KindText, text)
}

// SendKind is SendText for a message of another kind, such as
// mailbox.KindCall. Delivery is identical, including the direct push.
func (in *Inbox) SendKind(conv *mailbox.Conversation, kind, text string) (*mailbox.Note, *mailbox.Message, error) {
	out, err := in.client.Prepare(conv, kind, text)
	if err != nil {
		return nil, nil, err
	}
	// Hand it straight to their device first, and store it for the record
	// while that is on its way. The push is the fast path (one hop, tens of
	// milliseconds); storing means finding the right nodes for each piece,
	// which is what used to make the recipient wait. The recipient doesn't
	// move its read position for a pushed message, so the stored copy
	// landing a moment later is simply recognised as the same message.
	if in.courier != nil && len(conv.PeerAddrs) > 0 {
		addrs := append([]string(nil), conv.PeerAddrs...)
		go in.courier.Deliver(addrs, out.Note, out.Chunks)
	}
	if err := in.client.Commit(conv, out); err != nil {
		return nil, nil, err
	}
	return out.Note, out.Msg, nil
}

// handleDirect receives a message pushed straight to us by a peer.
// Runs on the peer read goroutine, so it hands off to the worker rather
// than doing the crypto and callbacks inline.
func (in *Inbox) handleDirect(senderID string, note *mailbox.Note, chunks map[string][]byte) {
	key := mailbox.MailboxKey(senderID, in.client.ID.ID())
	in.mu.Lock()
	_, known := in.convs[key]
	in.mu.Unlock()
	if !known {
		// A direct delivery from someone we've never heard of. Don't
		// invent a conversation from an unauthenticated push -- fall
		// back to the doorbell, which is the vetted path for first
		// contact and is where their knock will be.
		//
		// As a wake, not a read: signing a fresh identity per push is
		// free, and reading the doorbell inline would let each one buy
		// a full doorbell walk. Wakes coalesce in the worker's queue.
		in.handleNotify(in.doorbellKey())
		return
	}

	select {
	case in.direct <- struct{}{}:
	default:
		return // saturated; the DHT path still delivers it
	}
	go func() {
		defer func() { <-in.direct }()
		in.pollMu.Lock()
		defer in.pollMu.Unlock()

		// Looked up again under pollMu: the conversation may have been
		// unfollowed, or replaced by Follow, while this waited.
		in.mu.Lock()
		conv, known := in.convs[key]
		in.mu.Unlock()
		if !known {
			return
		}

		msg, err := in.client.OpenDelivered(conv, note, chunks)
		if err != nil {
			return
		}
		in.surfaceLocked(conv, []*mailbox.Message{msg})
	}()
}

// Stop ends the worker and the underlying hub's refresh loop.
func (in *Inbox) Stop() {
	in.once.Do(func() { close(in.stopCh) })
	in.hub.Stop()
}

func (in *Inbox) doorbellKey() dht.NodeID {
	return mailbox.DoorbellKey(in.client.ID.ID())
}

// inboundKey is the slot the peer writes to when messaging us.
func (in *Inbox) inboundKey(conv *mailbox.Conversation) dht.NodeID {
	return mailbox.MailboxKey(conv.PeerID, in.client.ID.ID())
}

// Follow starts watching a conversation for inbound messages. Safe to
// call for both accepted conversations and pending requests, and safe to
// call again for one already followed.
func (in *Inbox) Follow(conv *mailbox.Conversation) {
	key := in.inboundKey(conv)
	in.mu.Lock()
	in.convs[key] = conv
	in.mu.Unlock()
	in.hub.Watch(key)
}

// Unfollow stops watching a conversation.
func (in *Inbox) Unfollow(conv *mailbox.Conversation) {
	key := in.inboundKey(conv)
	in.mu.Lock()
	delete(in.convs, key)
	in.mu.Unlock()
	in.hub.Unwatch(key)
}

// handleNotify runs on the peer read goroutine, so it does nothing but
// hand the key to the worker. Blocking here would stall the connection
// that delivered the notification.
func (in *Inbox) handleNotify(key dht.NodeID) {
	select {
	case in.wake <- key:
	default:
		// Queue saturated: a wake already pending will cover this key's
		// conversation on its next pass.
	}
}

func (in *Inbox) worker() {
	for {
		select {
		case <-in.stopCh:
			return
		case key := <-in.wake:
			in.dispatch(key)
		}
	}
}

func (in *Inbox) dispatch(key dht.NodeID) {
	in.pollMu.Lock()
	defer in.pollMu.Unlock()

	if key == in.doorbellKey() {
		in.pollDoorbellLocked()
		return
	}
	in.mu.Lock()
	conv := in.convs[key]
	in.mu.Unlock()
	if conv != nil {
		in.pollConversationLocked(conv)
	}
}

// Update runs fn while holding the lock that serializes all fetching.
//
// Conversations are shared with the worker goroutine, which reads and
// writes their cursors inside Receive. A caller changing a conversation
// from elsewhere -- accepting a request, rewinding a cursor -- must do it
// through here, or it races a Receive that's mid-walk over the same
// fields. fn must not call back into the Inbox.
func (in *Inbox) Update(fn func()) {
	in.pollMu.Lock()
	defer in.pollMu.Unlock()
	fn()
}

// PollAll does one full check of the doorbell and every followed
// conversation.
//
// Notifications only fire while connected, so a client that was offline
// (or whose OS suspended it) must still reconcile on startup. This is
// that reconciliation -- the cheap catch-up pass the design always
// assumed, not a fallback to polling.
func (in *Inbox) PollAll() {
	in.pollMu.Lock()
	defer in.pollMu.Unlock()

	in.pollDoorbellLocked()
	in.mu.Lock()
	convs := make([]*mailbox.Conversation, 0, len(in.convs))
	for _, c := range in.convs {
		convs = append(convs, c)
	}
	in.mu.Unlock()
	for _, c := range convs {
		in.pollConversationLocked(c)
	}
}

// Poll checks one followed conversation now, rather than waiting for a
// notification or the next full check. For moments when a reply is
// expected within seconds, such as the answer to a call. A conversation
// that isn't being followed (blocked, banned) is left alone.
func (in *Inbox) Poll(conv *mailbox.Conversation) {
	key := in.inboundKey(conv)
	in.mu.Lock()
	c, ok := in.convs[key]
	in.mu.Unlock()
	if !ok {
		return
	}
	in.pollMu.Lock()
	defer in.pollMu.Unlock()
	in.pollConversationLocked(c)
}

// pollDoorbellLocked reads first-contact knocks and surfaces any from
// senders we aren't already following. Caller holds pollMu.
func (in *Inbox) pollDoorbellLocked() {
	knocks, err := in.client.ReadKnocks()
	if err != nil || len(knocks) == 0 {
		return
	}
	// One sender can leave any number of cards (identities are free and
	// so is knocking), and each would otherwise buy a full Receive of
	// the same conversation. Check each conversation once per read.
	polled := make(map[dht.NodeID]bool)
	for _, k := range knocks {
		conv, err := mailbox.ConversationFromKnock(k)
		if err != nil {
			continue
		}
		if in.Skip != nil && in.Skip(conv.PeerID) {
			continue
		}
		key := in.inboundKey(conv)
		if polled[key] {
			continue
		}
		polled[key] = true

		in.mu.Lock()
		existing, known := in.convs[key]
		if !known {
			in.convs[key] = conv
		}
		in.mu.Unlock()
		if known {
			// Already following. Still check it, since the knock may
			// have arrived alongside a message we haven't read.
			in.pollConversationLocked(existing)
			continue
		}

		// Registering for future notifications must not delay reading
		// the message that is already here: this runs while holding the
		// fetch lock, so waiting on the network would stall every other
		// conversation too.
		go in.hub.Watch(key)
		if in.OnKnock != nil {
			in.OnKnock(conv, k)
		}
		// A knock always accompanies a first message, so read it now
		// rather than waiting for a second notification.
		in.pollConversationLocked(conv)
	}
}

// pollConversationLocked fetches whatever is new for one conversation
// and hands it to OnMessages. Caller holds pollMu.
func (in *Inbox) pollConversationLocked(conv *mailbox.Conversation) {
	msgs, err := in.client.Receive(conv)
	if err != nil || len(msgs) == 0 {
		return
	}
	in.surfaceLocked(conv, msgs)
}

// surfaceLocked filters out messages already shown and delivers the
// rest. Caller holds pollMu.
//
// Dedup is by Message.ID rather than by note hash because the same
// message legitimately reaches us by two routes: pushed directly by the
// sender, and found by walking the chain after a notification. Both
// carry the same envelope, so the ID matches and the user sees it once.
func (in *Inbox) surfaceLocked(conv *mailbox.Conversation, msgs []*mailbox.Message) {
	fresh := make([]*mailbox.Message, 0, len(msgs))
	in.mu.Lock()
	if len(in.seen) > maxSeenMessages {
		in.seen = make(map[string]bool, len(msgs))
	}
	for _, m := range msgs {
		if m.ID == "" || in.seen[m.ID] {
			continue
		}
		in.seen[m.ID] = true
		fresh = append(fresh, m)
	}
	in.mu.Unlock()

	if len(fresh) > 0 && in.OnMessages != nil {
		in.OnMessages(conv, fresh)
	}
}
