// Package notify turns the mailbox layer from "walk to the post office
// and check" into "the post office rings you".
//
// The mechanism is the watch-instead-of-poll design from DESIGN-NOTES:
// a client finds the handful of nodes responsible for a key it cares
// about (its doorbell, or a conversation's mailbox slot) and asks them
// once to be told if that key ever changes. No repeated asking, so the
// cost while idle is one held connection rather than N lookups per
// interval.
//
// It needs no changes to AltNet core. peer.handleMessage dispatches
// every verified message to ALL registered handlers, and the DHT is just
// one of them -- so a Hub registered alongside it sees the same
// dht_store traffic the DHT does, and can fire notifications off the
// back of it.
//
// Privacy: a notification carries only the key, never content, and the
// key is an opaque hash. A watching node learns "somebody is interested
// in key X", not who is talking to whom. It does learn the watcher's
// NODE id -- so an adversary who already holds a victim's Relay username
// could compute their doorbell key and see which node watches it,
// correlating username to node. That requires already having the
// username, which is the same bar as messaging them, but it is a real
// correlation and is noted in DESIGN-NOTES rather than papered over.
package notify

import (
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"altnet/core/dht"
	"altnet/core/peer"
)

// Message types on the wire. Namespaced so they can't collide with the
// dht_* family or any future core type.
const (
	MsgWatch   = "relay_watch"
	MsgWatched = "relay_watched" // ack
	MsgNotify  = "relay_notify"
)

const (
	// WatchTTL is how long a registration survives without a refresh.
	// Bounds the memory a vanished peer can leave behind even if its
	// connection died in a way we didn't notice.
	WatchTTL = 10 * time.Minute

	// RefreshInterval is how often a watcher re-asserts its interest.
	// Comfortably inside WatchTTL so a single lost refresh doesn't drop
	// the watch. This is a heartbeat, not polling: one small message
	// covering every watched key, versus a full DHT lookup per key.
	RefreshInterval = 4 * time.Minute

	// MaxWatchedKeysPerPeer caps how many keys one remote peer may
	// register, so a peer can't exhaust our memory with registrations.
	MaxWatchedKeysPerPeer = 256

	// MaxKeysPerWatchMessage bounds one request's key list.
	MaxKeysPerWatchMessage = 64

	// watchRequestTimeout caps how long one watch request waits for its
	// ack. It no longer holds anyone up -- see watchGrace -- it only
	// bounds how long a goroutine lingers on a node that never replies.
	watchRequestTimeout = 5 * time.Second

	// watchGrace is the longest Watch blocks its caller waiting for
	// acknowledgements. Most of the network is ordinary AltNet nodes,
	// the seed included, that have never heard of relay_watch and will
	// never answer it. Waiting for every node meant waiting out the full
	// timeout on every new conversation -- on the live network that
	// alone added ~5 s before a first message could even be read.
	//
	// So Watch returns on the first acknowledgement, or after this
	// grace, whichever comes first; requests still in flight finish in
	// the background and register whatever they can.
	watchGrace = 750 * time.Millisecond

	// silentTTL is how long a node that ignored a watch request is left
	// alone. Nodes don't change what they understand often, and asking
	// again every four-minute refresh would just re-spend a timeout on the
	// same non-participants forever.
	silentTTL = 15 * time.Minute

	// maxTotalRegistrations caps registrations held for ALL peers
	// together. The per-peer cap alone bounds nothing, because peer
	// identities cost nothing to mint: N identities could hold
	// N x MaxWatchedKeysPerPeer entries. Refreshes of registrations
	// already held are always accepted, so reaching the cap stops new
	// interest, never existing watchers.
	maxTotalRegistrations = 16 * 1024

	// maxWatchersPerKey caps the watchers of one key, which is also the
	// most notifications one dht_store can make us send. An honest key
	// -- a doorbell, a conversation slot -- is watched by its owner's
	// devices, a handful at most; without this, anyone could register
	// thousands of identities on one key and turn every write to it
	// into thousands of sends from us. The first watchers keep their
	// places for as long as they refresh, so filling a key later cannot
	// evict the owner who registered first.
	maxWatchersPerKey = 32
)

type watchPayload struct {
	Keys []string `json:"keys"` // hex NodeIDs
}

type notifyPayload struct {
	Key string `json:"key"` // hex NodeID that changed
}

// registration is one remote peer's interest in a key.
type registration struct {
	addr    string // address we can Send on; valid while the conn lives
	expires time.Time
}

// Hub implements peer.Handler and plays both roles at once, because
// every node is both somebody's storage node and its own user's client:
//
//   - server role: accept relay_watch from others, observe dht_store,
//     push relay_notify to whoever registered interest.
//   - client role: Watch(keys) to register with the nodes closest to
//     each key, and invoke OnNotify when one of them reports a change.
type Hub struct {
	p *peer.Peer
	d *dht.DHT

	mu sync.Mutex
	// server role: key -> watcher peer ID -> registration
	watchers map[dht.NodeID]map[string]registration
	// server role: watcher peer ID -> how many keys it holds
	perPeer map[string]int
	// server role: registrations held across all keys and peers
	total int
	// client role: keys we've asked others to watch for us
	watching map[dht.NodeID]bool
	// client role: nodes that didn't answer a watch request, and until
	// when to leave them alone. See silentTTL.
	silent map[string]time.Time

	// onNotify fires when a watched key changes. Guarded by mu because
	// it is set from the caller's goroutine while the peer read loop may
	// already be delivering messages. Called on the peer's read
	// goroutine, so implementations must not block.
	onNotify func(key dht.NodeID)

	stop chan struct{}
	once sync.Once
}

// SetOnNotify installs the callback fired when a watched key changes.
// Safe to call while the hub is running.
func (h *Hub) SetOnNotify(fn func(key dht.NodeID)) {
	h.mu.Lock()
	h.onNotify = fn
	h.mu.Unlock()
}

// NewHub creates a Hub and registers it as a peer handler so it sees
// the same traffic the DHT does.
func NewHub(p *peer.Peer, d *dht.DHT) *Hub {
	h := &Hub{
		p:        p,
		d:        d,
		watchers: make(map[dht.NodeID]map[string]registration),
		perPeer:  make(map[string]int),
		watching: make(map[dht.NodeID]bool),
		silent:   make(map[string]time.Time),
		stop:     make(chan struct{}),
	}
	p.AddHandler(h)
	return h
}

// Start begins the background refresh loop that keeps registrations
// alive. Call after setting OnNotify.
func (h *Hub) Start() {
	go h.refreshLoop()
}

// Stop ends the refresh loop. Registrations we hold for others expire on
// their own via WatchTTL; ours lapse when our connections close.
func (h *Hub) Stop() {
	h.once.Do(func() { close(h.stop) })
}

func (h *Hub) refreshLoop() {
	t := time.NewTicker(RefreshInterval)
	defer t.Stop()
	for {
		select {
		case <-h.stop:
			return
		case <-t.C:
			h.mu.Lock()
			keys := make([]dht.NodeID, 0, len(h.watching))
			for k := range h.watching {
				keys = append(keys, k)
			}
			h.mu.Unlock()
			if len(keys) > 0 {
				_ = h.Watch(keys...)
			}
			h.pruneExpired()
		}
	}
}

// HandleMessage implements peer.Handler.
func (h *Hub) HandleMessage(p *peer.Peer, addr string, msg peer.Message) {
	switch msg.Type {
	case MsgWatch:
		h.handleWatch(p, addr, msg)
	case MsgNotify:
		h.handleNotify(msg)
	case "dht_store":
		// The DHT's own handler has already decided whether to keep
		// this value; we only need the key to fan out notifications.
		h.observeStore(msg)
	}
}

// handleWatch registers a remote peer's interest in a set of keys.
func (h *Hub) handleWatch(p *peer.Peer, addr string, msg peer.Message) {
	var wp watchPayload
	if err := json.Unmarshal([]byte(msg.Payload), &wp); err != nil {
		return
	}
	h.register(msg.From, addr, wp.Keys, time.Now())
	_ = p.Reply(addr, msg, peer.Message{Type: MsgWatched})
}

// register records from's interest in keys, reachable at addr, subject
// to every cap. Split from handleWatch so the bookkeeping can be tested
// without a network.
func (h *Hub) register(from, addr string, hexKeys []string, now time.Time) {
	if len(hexKeys) > MaxKeysPerWatchMessage {
		hexKeys = hexKeys[:MaxKeysPerWatchMessage]
	}
	expires := now.Add(WatchTTL)

	h.mu.Lock()
	defer h.mu.Unlock()
	for _, hexKey := range hexKeys {
		key, err := dht.IDFromHex(hexKey)
		if err != nil {
			continue
		}
		set := h.watchers[key]
		if _, already := set[from]; !already {
			if h.perPeer[from] >= MaxWatchedKeysPerPeer ||
				h.total >= maxTotalRegistrations ||
				len(set) >= maxWatchersPerKey {
				continue
			}
			// Only now create the key's set: making it before the caps
			// were checked left an empty set behind for every refused
			// key, so a peer at its cap could still grow our memory by
			// one map per key it named.
			if set == nil {
				set = make(map[string]registration)
				h.watchers[key] = set
			}
			h.perPeer[from]++
			h.total++
		}
		set[from] = registration{addr: addr, expires: expires}
	}
}

// observeStore fans a notification out to everyone watching the stored
// key, and -- critically -- also wakes THIS node's own client if it is
// watching that key.
//
// The local case is not an edge case, it's the common one. dht.Store
// writes locally and then replicates to the K closest peers *other than
// itself*, so a node never receives a dht_store message for its own
// write. That means when a message lands on the recipient's own node
// (which happens whenever their node is among the K closest to the key,
// and always in a small network), no remote party exists to notify them.
// Without the local fire below, the recipient would sit waiting for a
// notification that structurally cannot arrive.
//
// We notify even if the DHT ultimately declined the value (blocked hash,
// quota), because we can't cheaply tell from here. A spurious wake-up
// just makes the watcher do one lookup that finds nothing -- harmless,
// and far better than missing a real message.
func (h *Hub) observeStore(msg peer.Message) {
	var sp struct {
		Key string `json:"key"`
	}
	if err := json.Unmarshal([]byte(msg.Payload), &sp); err != nil {
		return
	}
	key, err := dht.IDFromHex(sp.Key)
	if err != nil {
		return
	}

	targets, localInterest, cb := h.notifyTargets(key, msg.From, time.Now())

	if localInterest && cb != nil {
		cb(key)
	}

	if len(targets) == 0 {
		return
	}
	body, err := json.Marshal(notifyPayload{Key: key.Hex()})
	if err != nil {
		return
	}
	out := peer.Message{Type: MsgNotify, Payload: string(body)}
	for _, addr := range targets {
		// Send failure means the watcher's connection is gone; its
		// registration will age out via WatchTTL. Deliberately not
		// retried -- a lapsed watch is the designed behaviour for a
		// client that went away.
		_ = h.p.Send(addr, out)
	}
}

// notifyTargets prunes key's expired watchers and returns the addresses
// to notify of a write by writer (never the writer itself), plus whether
// this node's own client is interested and the callback to wake it.
//
// The fan-out is bounded twice over: register never lets a key collect
// more than maxWatchersPerKey watchers, and the loop stops there too, so
// no future path that fills the set some other way can turn one write
// into unbounded sends.
func (h *Hub) notifyTargets(key dht.NodeID, writer string, now time.Time) ([]string, bool, func(dht.NodeID)) {
	h.mu.Lock()
	defer h.mu.Unlock()
	set := h.watchers[key]
	targets := make([]string, 0, min(len(set), maxWatchersPerKey))
	for peerID, reg := range set {
		if now.After(reg.expires) {
			delete(set, peerID)
			h.decPerPeer(peerID)
			continue
		}
		if peerID == writer || len(targets) >= maxWatchersPerKey {
			continue // don't tell the writer about their own write
		}
		targets = append(targets, reg.addr)
	}
	if set != nil && len(set) == 0 {
		delete(h.watchers, key)
	}
	return targets, h.watching[key], h.onNotify
}

// handleNotify is the client side: somebody we're watching through told
// us a key changed.
func (h *Hub) handleNotify(msg peer.Message) {
	var np notifyPayload
	if err := json.Unmarshal([]byte(msg.Payload), &np); err != nil {
		return
	}
	key, err := dht.IDFromHex(np.Key)
	if err != nil {
		return
	}
	h.mu.Lock()
	interested := h.watching[key]
	cb := h.onNotify
	h.mu.Unlock()

	// Ignore notifications for keys we never asked about, so a peer
	// can't use this to make us do arbitrary work.
	if !interested || cb == nil {
		return
	}
	cb(key)
}

// Watch asks the nodes closest to each key to notify us when it changes.
//
// Returns the number of (key, node) registrations that were
// acknowledged. Partial success is normal and fine: a key watched by
// even one live replica still delivers.
func (h *Hub) Watch(keys ...dht.NodeID) int {
	h.mu.Lock()
	for _, k := range keys {
		h.watching[k] = true
	}
	h.mu.Unlock()

	// Group keys by the node that should hold them, so a node
	// responsible for several of our keys gets one message, not several.
	byNode := make(map[string][]string) // addr -> hex keys
	for _, key := range keys {
		for _, c := range h.d.Lookup(key) {
			if c.ID.Equal(h.d.Self()) {
				continue // our own node needs no notification
			}
			for _, addr := range c.AllAddresses() {
				if addr == "" {
					continue
				}
				byNode[addr] = append(byNode[addr], key.Hex())
				break // one address per contact is enough
			}
		}
	}

	// Skip nodes that recently ignored us.
	now := time.Now()
	h.mu.Lock()
	for addr := range byNode {
		if until, ok := h.silent[addr]; ok {
			if now.Before(until) {
				delete(byNode, addr)
			} else {
				delete(h.silent, addr)
			}
		}
	}
	h.mu.Unlock()
	if len(byNode) == 0 {
		return 0
	}

	var mu sync.Mutex
	acked := 0
	first := make(chan struct{}, 1) // signalled on the first acknowledgement
	pending := len(byNode)
	allDone := make(chan struct{})
	for addr, hexKeys := range byNode {
		go func(addr string, hexKeys []string) {
			defer func() {
				mu.Lock()
				pending--
				last := pending == 0
				mu.Unlock()
				if last {
					close(allDone)
				}
			}()
			if len(hexKeys) > MaxKeysPerWatchMessage {
				hexKeys = hexKeys[:MaxKeysPerWatchMessage]
			}
			body, err := json.Marshal(watchPayload{Keys: hexKeys})
			if err != nil {
				return
			}
			reply, err := h.p.Request(addr,
				peer.Message{Type: MsgWatch, Payload: string(body)},
				watchRequestTimeout)
			if err != nil || reply.Type != MsgWatched {
				h.mu.Lock()
				h.silent[addr] = time.Now().Add(silentTTL)
				h.mu.Unlock()
				return
			}
			mu.Lock()
			acked += len(hexKeys)
			mu.Unlock()
			select {
			case first <- struct{}{}:
			default:
			}
		}(addr, hexKeys)
	}

	timer := time.NewTimer(watchGrace)
	defer timer.Stop()
	select {
	case <-first:
	case <-allDone:
	case <-timer.C:
	}
	mu.Lock()
	defer mu.Unlock()
	return acked
}

// Unwatch stops caring about keys locally. Remote registrations are left
// to expire rather than explicitly torn down: they cost one map entry,
// they lapse within WatchTTL, and a peer that vanishes mid-session ages
// out by exactly the same path.
func (h *Hub) Unwatch(keys ...dht.NodeID) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, k := range keys {
		delete(h.watching, k)
	}
}

// WatchedKeyCount reports how many keys this hub is holding
// registrations for on behalf of others. Used by tests and metrics.
func (h *Hub) WatchedKeyCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.watchers)
}

func (h *Hub) pruneExpired() {
	now := time.Now()
	h.mu.Lock()
	defer h.mu.Unlock()
	for key, set := range h.watchers {
		for peerID, reg := range set {
			if now.After(reg.expires) {
				delete(set, peerID)
				h.decPerPeer(peerID)
			}
		}
		if len(set) == 0 {
			delete(h.watchers, key)
		}
	}
}

// decPerPeer decrements a peer's registration count, and the total.
// Caller holds h.mu.
func (h *Hub) decPerPeer(peerID string) {
	if h.total > 0 {
		h.total--
	}
	if n := h.perPeer[peerID]; n <= 1 {
		delete(h.perPeer, peerID)
	} else {
		h.perPeer[peerID] = n - 1
	}
}

// String aids debugging.
func (h *Hub) String() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return fmt.Sprintf("notify.Hub{serving:%d keys, watching:%d keys}",
		len(h.watchers), len(h.watching))
}
