// Package engine is AltNet Relay as a running program, minus any user
// interface: it starts a node, joins the network, loads the user's Relay
// identity and saved conversations, and exposes the handful of things a
// person actually does -- add someone, send, accept or decline a request,
// block someone, claim a name.
//
// It exists so the terminal client and the desktop app share ONE
// implementation of the product's rules. Those rules are easy to get
// subtly wrong in two places at once: who counts as a message request,
// when a conversation becomes accepted, what a declined request means
// after a restart, what blocking someone stops, how a name is verified
// before it is shown. Keeping
// them here means a fix lands everywhere.
package engine

import (
	"errors"
	"fmt"
	mrand "math/rand/v2"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"altnet/core/crypto"
	"altnet/core/dht"
	"altnet/core/peer"

	"altnet-relay/account"
	"altnet-relay/dhtstore"
	"altnet-relay/mailbox"
	"altnet-relay/moderation"
	"altnet-relay/notify"
	"altnet-relay/state"
)

// Config says where the engine keeps its files and how it reaches the
// network.
type Config struct {
	DataDir   string   // identity, keys, saved conversations, DHT store
	Listen    string   // node listen address; "127.0.0.1:0" picks a free port
	Bootstrap []string // AltNet peers to join through
	Relays    []string // relays to register with for NAT traversal
	Public    bool     // node is directly reachable from the internet

	// Operators are the Ed25519 public keys (hex) whose signed ban lists
	// this client enforces. Empty means no operator moderation at all.
	//
	// Bans and handle authority are separate switches (Operators here,
	// HandleAuthorities below) so a development network can use one without
	// the other.
	Operators []string

	// Accounts, when set, makes a Studio account required: the person signs
	// in, this computer's Relay identity is attached to the account, and
	// @names are claimed through the server so they are unique. Nil means
	// no accounts (a local development network), with self-claimed names.
	Accounts *account.Client

	// HandleAuthorities are the public keys (hex) whose signature makes an
	// @name valid. Required whenever Accounts is set; the server signs names
	// with the operator key, so this is normally the same key as Operators.
	// It is process-wide: names are resolved in this mode for every engine
	// in the process.
	HandleAuthorities []string
}

// BanRefreshInterval is how often operator ban lists are re-read. A ban
// is a moderation response, not an alarm: minutes are fine, and each
// refresh of an unchanged list costs one lookup per operator.
const BanRefreshInterval = 10 * time.Minute

// ReconcileInterval is how often the whole inbox is re-checked even if no
// notification arrived.
//
// Instant notification depends on a node near each key understanding the
// "tell me if this changes" request, and today almost none do: the seed
// and ordinary Studio nodes never heard of it. Without this, a message
// could sit unread until the app restarted. A minute is the cost-benefit
// point for a small contact list; it's only a safety net, so it can be
// slow -- direct delivery and notifications are the fast paths.
const ReconcileInterval = 60 * time.Second

// banFile holds the last verified operator ban lists, so a restart
// enforces them immediately and can't be served an older list as if it
// were current (moderation.Sync's rollback floor survives via Export).
const banFile = "bans.json"

// StoreLimit is the most this computer keeps on the network's behalf
// (encrypted pieces of other people's messages).
const StoreLimit = 256 << 20

// Summary is one row of a conversation list.
type Summary struct {
	PeerID       string
	Name         string // what to call them: nickname, else @handle, else short ID
	Handle       string // their verified handle, "" if none
	Accepted     bool
	Request      bool // they wrote first and the user hasn't accepted yet
	Blocked      bool // the user blocked them; history stays, nothing new arrives
	Banned       bool // an operator banned them network-wide; nothing new arrives
	Unread       int  // inbound messages the user hasn't opened yet
	Biz          bool // a coworker's chat: belongs to Business, not the Personal list
	Coworker     bool // they have accepted a workspace we're in: can't be blocked or reported
	Former       bool // a Business chat with someone who is no longer a coworker: read-only
	Preview      string
	LastOutbound bool // the preview is something the user sent
	LastAt       int64
}

// Engine is a running Relay client.
type Engine struct {
	Peer   *peer.Peer
	DHT    *dht.DHT
	ID     *mailbox.Identity
	Client *mailbox.Client
	Hub    *notify.Hub
	Inbox  *notify.Inbox
	State  *state.State

	// OnMessages fires with messages that are new to local history.
	// OnRequest fires when a stranger's first contact arrives.
	// Set both before Start. They run on the inbox worker goroutine, so
	// they may block briefly but should hand off anything slow.
	OnMessages func(peerID string, msgs []state.StoredMessage)
	OnRequest  func(peerID string)

	// OnCall fires for call-setup messages from accepted contacts: an
	// offer to ring on, or the answer, decline, busy or hang-up that
	// follows. See call.go.
	OnCall func(CallSignal)

	// OnTyping fires when an accepted contact is typing to us.
	// OnDelivered fires when messages we sent reached their device.
	// See signals.go.
	OnTyping    func(peerID string)
	OnDelivered func(peerID string, ids []string)

	// OnGroup fires when a group changes (invitation, members, status);
	// OnGroupMessage when a message arrives in a joined group. See group.go.
	OnGroup        func(groupID string)
	OnGroupMessage func(groupID string, m state.GroupMessage)

	// OnGroupTyping fires when a member is typing in a group; OnGroupDelivered
	// when members' devices confirm our group messages arrived.
	// OnInvited fires once when a new invitation to a group or workspace
	// arrives, OnAdded once when we are added to a channel of a workspace
	// we're in. Both exist so the app can say so on the desktop, even with
	// its window closed.
	OnInvited func(groupID string)
	OnAdded   func(groupID string)
	// OnReport fires when a member reports a coworker to the owner of a
	// workspace we run.
	OnReport func(workspaceID string)

	OnGroupTyping    func(groupID, from string)
	OnGroupDelivered func(groupID string, ids []string)

	// Signals carries "typing" and "delivered" between devices.
	Signals *notify.Signals

	// Mod enforces operator ban lists. Always non-nil; with no operators
	// configured it simply never bans anyone.
	Mod *moderation.Sync

	boots   []string
	dataDir string
	stop    chan struct{}
	closed  sync.Once

	banMu      sync.Mutex
	banned     map[string]bool // conversations banned as of the last refresh
	banVersion string          // fingerprint of the lists last saved

	// Which calls have been offered to us / dealt with; see call.go.
	callMu    sync.Mutex
	offered   map[string]bool
	handled   map[string]bool
	waiting   map[string]chan struct{} // callers looking for a reply; see awaitReply
	turn      *account.IceServer       // cached call-relay credentials; see CallServers
	turnUntil time.Time
	stunOK    bool // the operator's STUN server answered; see stun.go
	stunUntil time.Time

	// early holds "delivered" receipts that beat our own record of the
	// message: the receipt can come back from their device before our copy
	// has finished storing. Applied when the message is recorded.
	earlyMu sync.Mutex
	early   map[string]bool

	// groupQueues holds one ordered send queue per group; see fanOut.
	groupMu     sync.Mutex
	groupQueues map[string]chan groupJob

	// sendLocks holds one mutex per conversation. A message is prepared
	// into the next place in the chain and stored afterwards, so a second
	// message to the same person must wait until the first is stored, or
	// both would claim the same place. Different conversations don't wait
	// for each other.
	sendLocks sync.Map // peerID -> *sync.Mutex

	// Sign-in state; see account.go. acct is nil when accounts are off.
	acct        *account.Client
	acctMu      sync.Mutex
	sess        *account.Session
	gate        Gate
	gateReason  string
	acctOffline bool
	lastRefresh time.Time
	publishing  bool // a background retry of the name is running

	// Signing in on other computers; see roaming.go.
	roam *roaming
}

// Open starts the node and loads the user's identity and state, but does
// not begin receiving yet -- set the callbacks, then call Start.
func Open(cfg Config) (*Engine, error) {
	if cfg.DataDir == "" {
		return nil, errors.New("engine: a data directory is required")
	}
	if cfg.Listen == "" {
		cfg.Listen = "0.0.0.0:0"
	}
	if cfg.Accounts != nil && len(cfg.HandleAuthorities) == 0 {
		// Without the operator's key the app couldn't tell a signed name from a
		// forged one, so every name the server hands out would be rejected.
		return nil, errors.New("engine: accounts need HandleAuthorities (the operator's public key)")
	}
	if len(cfg.HandleAuthorities) > 0 {
		mailbox.SetTrustedHandleAuthorities(cfg.HandleAuthorities)
	}

	// The NODE identity: public by design, spread through routing tables
	// and stamped on every message. Kept in its own file.
	nodeKey, err := crypto.LoadOrCreate(filepath.Join(cfg.DataDir, "node.key"))
	if err != nil {
		return nil, fmt.Errorf("engine: node identity: %w", err)
	}
	p := peer.New(nodeKey, cfg.Listen)
	p.SetPublic(cfg.Public)
	if err := p.Start(); err != nil {
		return nil, fmt.Errorf("engine: start node: %w", err)
	}
	if len(cfg.Relays) > 0 {
		p.UseRelay(cfg.Relays...)
	}

	// Capped: each peer has its own budget, but identities cost nothing, so
	// without a total many made-up peers could fill the user's disk. Over the
	// cap, what was used least recently goes first.
	d, err := dht.NewWithLimit(p, filepath.Join(cfg.DataDir, "store"), StoreLimit)
	if err != nil {
		p.Stop()
		return nil, fmt.Errorf("engine: start dht: %w", err)
	}

	// The RELAY identity: private, never used for transport. The user's
	// username derives from this, which is why it must not be the node
	// key above -- see mailbox.Client.
	// A copy of the account brought from another computer goes in place
	// before the identity is read.
	roam, err := prepareRoaming(cfg.DataDir)
	if err != nil {
		p.Stop()
		return nil, fmt.Errorf("engine: %w", err)
	}
	relayID, err := mailbox.LoadOrCreateIdentity(cfg.DataDir)
	if err != nil {
		p.Stop()
		return nil, err
	}
	st, err := state.Load(cfg.DataDir)
	if err != nil {
		p.Stop()
		return nil, err
	}

	client := mailbox.NewClient(dhtstore.New(d), relayID, nil)
	client.SelfAddrs = p.AdvertisedAddresses

	// Operator bans. The saved copy is loaded first so bans apply from the
	// first instant, before the network is even reachable, and so an old
	// list served by the network can't roll them back.
	mod := moderation.NewSync(dhtstore.New(d), cfg.Operators)
	if saved, err := os.ReadFile(filepath.Join(cfg.DataDir, banFile)); err == nil {
		_ = mod.Import(saved) // a damaged file just means starting from the network
	}
	// Enforce at the lowest layer: notes, knocks and direct deliveries
	// signed by a banned identity are rejected inside mailbox itself.
	client.Bans = mod

	hub := notify.NewHub(p, d)
	e := &Engine{
		Peer: p, DHT: d, ID: relayID, Client: client, Hub: hub, State: st,
		Inbox:   notify.NewInbox(client, hub).WithCourier(notify.NewCourier(p, client)),
		Mod:     mod,
		boots:   cfg.Bootstrap,
		dataDir: cfg.DataDir,
		stop:    make(chan struct{}),
		banned:  make(map[string]bool),
		acct:    cfg.Accounts,
		offered: make(map[string]bool),
		handled: make(map[string]bool),
		waiting: make(map[string]chan struct{}),
		early:   make(map[string]bool),
		roam:    roam,

		groupQueues: make(map[string]chan groupJob),
	}
	e.Signals = notify.NewSignals(p, client)
	e.Signals.OnSignal(e.onSignal)
	e.loadSession()
	e.banVersion = e.banFingerprint()
	return e, nil
}

// Start joins the network and begins receiving. It returns warnings for
// things that didn't work but aren't fatal -- an unreachable bootstrap
// peer, a profile that couldn't be published yet -- so the caller can
// show them. Messages already saved are unaffected either way.
func (e *Engine) Start() []string {
	var warnings []string

	for _, addr := range e.boots {
		if err := e.DHT.Bootstrap(addr); err != nil {
			warnings = append(warnings, fmt.Sprintf("could not reach %s: %v", addr, err))
		}
	}
	e.DHT.StartMaintenance(dht.MaintenanceConfig{BootstrapPeers: e.boots})

	e.Inbox.OnMessages = e.onMessages
	e.Inbox.OnKnock = e.onKnock
	e.Inbox.Skip = e.ignored

	// Current ban lists before anything is followed, so a newly banned
	// person isn't watched even once. Failure keeps the saved lists.
	if err := e.Mod.Refresh(); err != nil {
		warnings = append(warnings, "couldn't check for updated moderation lists; using the saved ones")
	}
	e.saveBans()

	// Re-watch every conversation from earlier runs, except people the
	// user has blocked or an operator has banned: not watching them is
	// what makes silencing cost nothing, and stops us asking their
	// mailbox's storage nodes for news we'd only throw away.
	e.banMu.Lock()
	for _, conv := range e.State.Conversations() {
		banned := e.Mod.BannedPeer(conv.PeerID)
		e.banned[conv.PeerID] = banned
		if !banned && !e.State.IsBlocked(conv.PeerID) {
			e.Inbox.Follow(conv)
		}
	}
	e.banMu.Unlock()
	go e.banLoop()
	go e.reconcileLoop()
	e.Inbox.Start()
	if e.roam.needSync {
		e.catchUp()
	}

	// Republish the profile, and re-assert any handle so it doesn't lapse
	// for someone who simply uses the app.
	if _, err := e.Client.PublishProfile(e.State.MyHandle()); err != nil {
		warnings = append(warnings, "your profile isn't published yet, so new people can't reach you until the network is reachable")
	}
	if e.acct != nil {
		// Names come from the server's signature, so renewing one is a
		// round trip to it. Done in the background: the window shouldn't wait
		// for the account service, and offline is fine.
		go e.accountLoop()
		go e.CheckAccount()
		go e.copyLoop()
	} else if h := e.State.MyHandle(); h != "" {
		if _, err := e.Client.ClaimHandle(h); err != nil {
			warnings = append(warnings, fmt.Sprintf("could not refresh @%s: %v", h, err))
		}
	}

	// Pick up anything that arrived while we were closed.
	go func() {
		time.Sleep(500 * time.Millisecond)
		e.Inbox.PollAll()
	}()
	return warnings
}

// sendLock takes the per-conversation send lock and returns its release.
func (e *Engine) sendLock(peerID string) func() {
	m, _ := e.sendLocks.LoadOrStore(peerID, &sync.Mutex{})
	mu := m.(*sync.Mutex)
	mu.Lock()
	return mu.Unlock
}

// Close saves state and leaves the network.
func (e *Engine) Close() {
	e.closed.Do(func() {
		close(e.stop)
		e.Inbox.Stop()
		e.saveBans()
		_ = e.State.Save()
		e.Peer.Stop()
	})
}

// banLoop re-reads the operator ban lists periodically. Jittered so a
// network full of clients started at the same moment doesn't hit the
// same storage nodes in lockstep.
func (e *Engine) banLoop() {
	for {
		wait := BanRefreshInterval + time.Duration(mrand.Int64N(int64(BanRefreshInterval/5)))
		select {
		case <-e.stop:
			return
		case <-time.After(wait):
			e.RefreshBans()
		}
	}
}

// reconcileLoop re-checks the doorbell and every followed conversation on
// a jittered timer. See ReconcileInterval for why this exists.
func (e *Engine) reconcileLoop() {
	for {
		wait := ReconcileInterval + time.Duration(mrand.Int64N(int64(ReconcileInterval/5)))
		select {
		case <-e.stop:
			return
		case <-time.After(wait):
			e.Inbox.PollAll()
		}
	}
}

// RefreshBans fetches the latest operator ban lists and applies them to
// every conversation. Safe to call at any time; a failure keeps the
// lists already in force.
//
// A newly banned peer is unfollowed, so nothing more is fetched from
// them. A newly unbanned peer is followed again, and anything they sent
// during the ban then arrives: a ban withholds messages, it doesn't
// destroy them. That's deliberate -- bans get lifted mostly because they
// were mistakes, and losing someone's messages would compound the
// mistake. (Blocking is different: the user's own choice, and what
// arrives while blocked is discarded.)
func (e *Engine) RefreshBans() error {
	err := e.Mod.Refresh()
	e.saveBans()

	e.banMu.Lock()
	defer e.banMu.Unlock()
	for _, conv := range e.State.Conversations() {
		now := e.Mod.BannedPeer(conv.PeerID)
		was := e.banned[conv.PeerID]
		e.banned[conv.PeerID] = now
		switch {
		case now && !was:
			e.Inbox.Unfollow(conv)
		case !now && was && !e.ignored(conv.PeerID):
			e.Inbox.Follow(conv)
		}
	}
	return err
}

// banFingerprint identifies the ban lists in force, so they're only
// written to disk when they actually change.
func (e *Engine) banFingerprint() string {
	var b strings.Builder
	for _, l := range e.Mod.Lists() {
		fmt.Fprintf(&b, "%s:%d;", l.Operator, l.Version)
	}
	return b.String()
}

// saveBans persists the verified ban lists if they changed. Written via a
// temp file and rename, like the state file, so a crash mid-write can't
// leave a truncated list that would fail to import.
func (e *Engine) saveBans() {
	fp := e.banFingerprint()
	e.banMu.Lock()
	unchanged := fp == e.banVersion
	e.banMu.Unlock()
	if unchanged {
		return
	}
	blob, err := e.Mod.Export()
	if err != nil {
		return
	}
	path := filepath.Join(e.dataDir, banFile)
	if os.WriteFile(path+".tmp", blob, 0o600) != nil || os.Rename(path+".tmp", path) != nil {
		return
	}
	e.banMu.Lock()
	e.banVersion = fp
	e.banMu.Unlock()
}

// IsBanned reports whether an operator has banned this peer.
func (e *Engine) IsBanned(peerID string) bool { return e.Mod.BannedPeer(peerID) }

// Username is the user's full 64-character Relay ID.
func (e *Engine) Username() string { return e.ID.Username() }

// MyHandle is the user's claimed name, or "".
func (e *Engine) MyHandle() string { return e.State.MyHandle() }

// Peers is how many AltNet nodes we currently hold connections to.
func (e *Engine) Peers() int { return e.Peer.UniqueConnCount() }

// Claim registers a handle for this user.
func (e *Engine) Claim(handle string) (string, error) {
	if e.acct != nil {
		return e.claimWithAccount(handle)
	}
	if err := mailbox.CheckHandleDecency(handle); err != nil {
		return "", err
	}
	rec, err := e.Client.ClaimHandle(handle)
	if err != nil {
		return "", err
	}
	e.State.SetMyHandle(rec.Handle)
	if _, err := e.Client.PublishProfile(rec.Handle); err != nil {
		return rec.Handle, fmt.Errorf("claimed @%s, but your profile couldn't be updated yet: %w", rec.Handle, err)
	}
	return rec.Handle, e.State.Save()
}

// Add starts a conversation with someone, given their handle or full
// username. nickname may be empty, in which case their handle is used.
//
// Adding someone ACCEPTS the conversation. You reached out to them, so
// their replies are not a stranger's message request -- treating them as
// one would cap the reply at three messages and file it under Requests.
// (An earlier version of the terminal client did exactly that.)
func (e *Engine) Add(who, nickname string) (string, error) {
	if err := e.allowed(); err != nil {
		return "", err
	}
	who = strings.TrimSpace(who)
	if who == "" {
		return "", errors.New("type their name or ID")
	}
	peerID, err := mailbox.LookupUsername(e.Client.Store, who)
	if err != nil {
		if errors.Is(err, mailbox.ErrNotFound) {
			return "", fmt.Errorf("nobody called %q was found", strings.TrimPrefix(who, "@"))
		}
		return "", err
	}
	if peerID == e.Username() {
		return "", errors.New("that's you")
	}
	if e.Mod.BannedPeer(peerID) {
		return "", errBanned
	}

	handle := e.verifiedHandle(peerID, who)
	if nickname == "" {
		nickname = handle
	}
	if nickname == "" {
		nickname = peerID[:8]
	}

	e.State.Undecline(peerID) // adding on purpose overrides an old decline
	e.State.SetGroupOnly(peerID, false)
	if err := e.State.AddContact(nickname, peerID); err != nil {
		return "", err
	}
	if handle != "" {
		e.State.SetContactHandle(peerID, handle)
	}
	conv := e.State.Conversation(peerID)
	e.Inbox.Update(func() {
		// If they'd already written as a request, rewind for the same
		// reason Accept does: the cursor has walked past whatever the
		// pending cap held back, and polling alone would never reach it.
		if !conv.Accepted {
			conv.LastReadHash = ""
		}
		conv.Accepted = true
	})
	e.State.PutConversation(conv)
	// A block, unlike a decline, is only undone by unblocking. Adding
	// someone you've blocked makes them a contact but doesn't let them
	// back in -- that stays a separate, deliberate choice.
	if !e.State.IsBlocked(peerID) {
		e.Inbox.Follow(conv)
	}
	err = e.State.Save()

	go e.Inbox.PollAll() // surface what the pending cap was holding back
	return peerID, err
}

// Send writes a message to a peer.
func (e *Engine) Send(peerID, text string) (state.StoredMessage, error) {
	if err := e.allowed(); err != nil {
		return state.StoredMessage{}, err
	}
	text = strings.TrimSpace(text)
	if text == "" {
		return state.StoredMessage{}, errors.New("nothing to send")
	}
	// A block is silence both ways. Writing to someone while refusing to
	// hear their answer would be strange, and it would tip them off.
	if e.State.IsBlocked(peerID) {
		return state.StoredMessage{}, errBlocked
	}
	if e.Mod.BannedPeer(peerID) {
		return state.StoredMessage{}, errBanned
	}
	defer e.sendLock(peerID)()
	conv := e.State.Conversation(peerID)
	if !conv.Accepted && e.State.HasInbound(peerID) {
		return state.StoredMessage{}, errors.New("accept their request before replying")
	}
	_, msg, err := e.Inbox.SendText(conv, text)
	if err == nil {
		if e.isCoworker(peerID) && !e.State.IsContact(peerID) {
			e.State.SetWorkChat(peerID) // a work chat: it belongs to Business
		} else {
			e.State.SetGroupOnly(peerID, false) // writing to them directly makes it a chat
		}
	}
	if err != nil {
		return state.StoredMessage{}, err
	}
	added := e.State.RecordNew(peerID, true, msg)
	if e.takeEarly(msg.ID) {
		e.State.MarkDelivered(peerID, []string{msg.ID})
		for i := range added {
			added[i].Delivered = true
		}
	}
	e.State.PutConversation(conv)
	if err := e.State.Save(); err != nil {
		return state.StoredMessage{}, err
	}
	if len(added) == 1 {
		return added[0], nil
	}
	return state.StoredMessage{ID: msg.ID, Outbound: true, Text: msg.Text, Sent: msg.Sent}, nil
}

// Accept lets a message-request sender in: their full backlog becomes
// visible and they join the contact list.
func (e *Engine) Accept(peerID string) error {
	if err := e.allowed(); err != nil {
		return err
	}
	// Accepting rewinds the read cursor, which would walk straight into
	// whatever they sent while blocked. Unblock deals with that properly,
	// so it has to come first.
	if e.State.IsBlocked(peerID) {
		return errBlocked
	}
	e.State.SetGroupOnly(peerID, false)
	conv := e.State.Conversation(peerID)
	e.Inbox.Update(func() {
		conv.Accepted = true
		// Rewind the read cursor. While pending, the cursor advanced past
		// messages the cap held back -- the sender's 4th, 5th, ... -- so
		// without this they'd sit behind it forever and accepting would
		// only reveal what arrives afterwards. Re-walking from the start
		// is safe: messages already shown are deduplicated by ID, both in
		// the inbox and in saved history, so only the held-back ones
		// surface (and count as unread).
		conv.LastReadHash = ""
	})
	e.State.PutConversation(conv)

	if !e.State.IsContact(peerID) {
		base := e.State.PeerHandle(peerID)
		if base == "" {
			// Not verified yet (a slow lookup, or a network blip when the
			// request arrived). Try now rather than naming them by ID.
			base = e.cacheHandle(peerID)
		}
		if base == "" {
			base = peerID[:8]
		}
		// If that nickname is already used for someone else, disambiguate
		// rather than fail -- accepting must always work.
		if err := e.State.AddContact(base, peerID); err != nil {
			if err := e.State.AddContact(base+"-"+peerID[:4], peerID); err != nil {
				return err
			}
		}
	}
	e.Inbox.Follow(conv)
	err := e.State.Save()
	go e.Inbox.PollAll() // surface what the pending cap was holding back
	return err
}

// Decline turns down a message request. The conversation and anything
// they sent are deleted, and their request won't come back. The sender
// is not told either way -- there is no signal to send them.
func (e *Engine) Decline(peerID string) error {
	conv := e.State.Conversation(peerID)
	e.Inbox.Unfollow(conv)
	e.State.Decline(peerID)
	return e.State.Save()
}

// errBlocked is what anything that would talk to a blocked peer returns.
var errBlocked = errors.New("you've blocked them; unblock them first")

// Blocking and reporting to AltNet are Personal tools: blocking is yours alone
// and reporting goes to AltNet. At work they are not offered -- the window
// leaves them out of Business chats -- because there the person who runs the
// workspace decides who belongs: a member reports a coworker to the owner
// (ReportCoworker), and the owner removes them. The engine doesn't enforce the
// split: a block is nobody's business but your own.

// errBanned is returned for anyone an operator has banned from the
// network. Writing to them would be pointless -- they can't be answered --
// and a banned identity is exactly the kind someone might be steered to.
var errBanned = errors.New("this account has been banned from AltNet")

// Block stops hearing from someone: contact, stranger or pending request
// alike. Nothing they send from now on is shown or counted, their knocks
// never become requests, and none of it reaches the callbacks. Their
// past history stays, because blocking means "stop hearing from them",
// not "forget them" (Decline does that, for requests).
//
// It is purely local. There is no signal to send, so the blocked person
// can't find out, and nothing about it is stored on the network.
func (e *Engine) Block(peerID string) error {
	if len(peerID) != 64 {
		return fmt.Errorf("%q isn't a username", peerID)
	}
	if peerID == e.Username() {
		return errors.New("that's you")
	}
	// Record the block BEFORE unfollowing: a poll already walking their
	// chain hands its results to onMessages, which then drops them.
	e.State.Block(peerID)
	e.Inbox.Unfollow(&mailbox.Conversation{PeerID: peerID})
	return e.State.Save()
}

// Unblock lets someone back in. What they send from now on arrives as
// usual (as a request again, if that's what they were), but what they
// sent while blocked stays unseen. "Never shown" has to survive a change
// of mind, or unblocking would dump everything they wrote in the meantime.
//
// One gap, accepted knowingly: someone blocked before they ever wrote has
// no conversation and so no cursor to move. If they knocked during the
// block, unblocking lets that knock through as an ordinary request --
// which the user can still decline.
func (e *Engine) Unblock(peerID string) error {
	if !e.State.IsBlocked(peerID) {
		return nil
	}
	conv, known := e.State.LookupConversation(peerID)
	if known {
		// Walk past everything that arrived during the block and discard
		// it. The walk ignores the pending cap so held-back messages are
		// discarded too; otherwise accepting later, which rewinds and
		// re-walks, would reveal them. Best effort: if the network can't
		// be reached right now the walk finds nothing, and a later poll
		// may show what was sent during the block.
		e.Inbox.Update(func() {
			accepted := conv.Accepted
			conv.Accepted = true
			msgs, _ := e.Client.Receive(conv)
			conv.Accepted = accepted
			e.State.Discard(peerID, msgs...)
		})
	}
	e.State.Unblock(peerID)
	if known {
		e.Inbox.Follow(conv)
	}
	return e.State.Save()
}

// IsBlocked reports whether the user has blocked this peer.
func (e *Engine) IsBlocked(peerID string) bool { return e.State.IsBlocked(peerID) }

// MarkRead clears a conversation's unread count, for when the user
// opens it.
func (e *Engine) MarkRead(peerID string) error {
	if !e.State.MarkRead(peerID) {
		return nil // already read; skip the disk write
	}
	return e.State.Save()
}

// Name is what to call a peer on screen.
func (e *Engine) Name(peerID string) string {
	for _, c := range e.State.Contacts() {
		if c.PeerID == peerID {
			return c.Nickname
		}
	}
	if h := e.State.PeerHandle(peerID); h != "" {
		return "@" + h
	}
	return peerID[:8]
}

// Conversations lists everything to show in a sidebar, newest first.
func (e *Engine) Conversations() []Summary {
	convs := e.State.Conversations()
	out := make([]Summary, 0, len(convs))
	for _, c := range convs {
		if e.State.IsDeclined(c.PeerID) {
			continue
		}
		// Someone with no direct messages either way is only a chat if the
		// user added them. Otherwise it's a group member, or a knock that
		// carried a group invitation -- neither belongs in this list.
		if !e.State.IsContact(c.PeerID) && len(e.State.Thread(c.PeerID)) == 0 {
			continue
		}
		blocked := e.State.IsBlocked(c.PeerID)
		banned := e.Mod.BannedPeer(c.PeerID)
		// A coworker is not a stranger: someone in a workspace we've both
		// joined may write to us. Their chat belongs to Business unless we've
		// also made them a contact, and it stays there -- closed -- once they
		// stop being a coworker, rather than turning into a message request.
		groupOnly := e.State.IsGroupOnly(c.PeerID)
		coworker := groupOnly && e.isCoworker(c.PeerID)
		work := e.workPeer(c.PeerID)
		former := work && !coworker
		s := Summary{
			PeerID:   c.PeerID,
			Name:     e.Name(c.PeerID),
			Handle:   e.State.PeerHandle(c.PeerID),
			Accepted: c.Accepted && (!groupOnly || coworker) && !former,
			// A blocked or banned stranger isn't asking anything the user
			// can answer, so they don't sit among the requests. A group
			// member writing directly is asking, like a stranger would.
			Request:  (!c.Accepted || (groupOnly && !coworker)) && !work && !blocked && !banned && e.State.HasInbound(c.PeerID),
			Blocked:  blocked,
			Banned:   banned,
			Biz:      work,
			Coworker: e.isCoworker(c.PeerID),
			Former:   former,
			Unread:   e.State.Unread(c.PeerID),
		}
		if thread := e.State.Thread(c.PeerID); len(thread) > 0 {
			last := thread[len(thread)-1]
			s.Preview, s.LastOutbound, s.LastAt = last.Text, last.Outbound, last.Sent
		}
		out = append(out, s)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].LastAt > out[j].LastAt })
	return out
}

// Thread is one conversation, both directions, oldest first.
func (e *Engine) Thread(peerID string) []state.StoredMessage {
	return e.State.Thread(peerID)
}

// verifiedHandle finds the handle a peer genuinely goes by. typed is what
// the user entered, which may itself be the handle. A profile's own claim
// is never trusted -- only a forward lookup that points back at them.
func (e *Engine) verifiedHandle(peerID, typed string) string {
	if rec, err := mailbox.ResolveHandle(e.Client.Store, typed); err == nil && rec.Username == peerID {
		return rec.Handle
	}
	if prof, err := mailbox.FetchProfile(e.Client.Store, peerID); err == nil {
		return prof.VerifiedHandle(e.Client.Store)
	}
	return ""
}

// cacheHandle looks up a peer's handle, verified by a forward lookup, and
// remembers it. Returns "" if they have none or it can't be verified.
func (e *Engine) cacheHandle(peerID string) string {
	prof, err := mailbox.FetchProfile(e.Client.Store, peerID)
	if err != nil {
		return ""
	}
	h := prof.VerifiedHandle(e.Client.Store)
	if h != "" {
		e.State.SetContactHandle(peerID, h)
	}
	return h
}

// ignored reports peers whose knocks must never surface: requests the
// user declined, and anyone they've blocked.
func (e *Engine) ignored(peerID string) bool {
	return e.State.IsDeclined(peerID) || e.State.IsBlocked(peerID) || e.Mod.BannedPeer(peerID)
}

func (e *Engine) onMessages(conv *mailbox.Conversation, msgs []*mailbox.Message) {
	if e.State.IsBlocked(conv.PeerID) {
		// Normally unreachable, since blocked conversations aren't
		// followed, but a poll that was mid-walk when the block landed
		// ends up here. Discard rather than just return, so a later
		// rewind of the cursor can't bring these back.
		e.State.Discard(conv.PeerID, msgs...)
		_ = e.State.Save()
		return
	}
	if e.Mod.BannedPeer(conv.PeerID) {
		// Backstop only: mailbox already rejects a banned identity's notes.
		// Withheld, not discarded -- see RefreshBans.
		return
	}
	// Calls and group messages first: declining someone's direct request
	// is about their direct messages, not about a group you share with
	// them. (Calls check for a decline themselves.)
	msgs = e.routeCalls(conv, msgs)
	if e.State.IsDeclined(conv.PeerID) || len(msgs) == 0 {
		e.State.PutConversation(conv)
		_ = e.State.Save()
		return
	}
	// A stranger's first message -- or a group member's first direct one --
	// is a message request. Announced here, once there's a message to show,
	// rather than when their knock arrives: a knock alone can be a group
	// invitation, which isn't a request.
	// A coworker writing isn't asking; neither is someone whose chat with us
	// began at work.
	if conv.Accepted && e.isCoworker(conv.PeerID) && !e.State.IsContact(conv.PeerID) {
		e.State.SetWorkChat(conv.PeerID)
	}
	asks := (!conv.Accepted || e.State.IsGroupOnly(conv.PeerID)) && !e.State.HasInbound(conv.PeerID) &&
		!e.workPeer(conv.PeerID)
	e.State.PutConversation(conv)
	added := e.State.RecordNew(conv.PeerID, false, msgs...)
	_ = e.State.Save()
	if len(added) > 0 && e.OnMessages != nil {
		e.OnMessages(conv.PeerID, added)
	}
	if asks && len(added) > 0 && e.OnRequest != nil {
		e.OnRequest(conv.PeerID)
	}
	e.acknowledge(conv, added)
}

func (e *Engine) onKnock(conv *mailbox.Conversation, k *mailbox.Knock) {
	if e.ignored(conv.PeerID) {
		return // Skip catches these first; this is the backstop
	}
	// Verify their name BEFORE the conversation becomes visible. The
	// other order leaves a window where a request exists with no name, and
	// accepting it inside that window would file them under a hex string
	// for good. (Caught by a test that failed intermittently.)
	e.cacheHandle(conv.PeerID)
	// Someone in a group we've joined writing for the first time: open
	// their conversation like any member's, so it isn't capped.
	if e.sharesGroup(conv.PeerID) {
		conv.Accepted = true
		e.State.SetGroupOnly(conv.PeerID, true)
	}
	e.State.PutConversation(conv)
	_ = e.State.Save()
	// The request itself is announced by onMessages, once its first message
	// is here: a knock can also be a group invitation, which isn't one.
}
