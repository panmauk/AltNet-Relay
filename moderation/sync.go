package moderation

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"

	"altnet-relay/mailbox"
	"altnet/core/crypto"
	"altnet/core/dht"
)

// Sync is a client's view of the operators' ban lists: it fetches and
// verifies them (Refresh) and answers "is this identity banned?"
// (Banned for a signer's key, BannedPeer for a username).
//
// It keeps its own authoritative set rather than feeding a
// *mailbox.BanList, because BanList can only grow -- it has no Remove, so
// an unban could never take effect -- and swapping a fresh BanList into
// Client.Bans while the inbox goroutine reads it would be a data race.
// Sync answers from state it swaps under its own lock, so one long-lived
// *Sync can sit behind every check. It has a Contains method with
// BanList's exact signature for the same reason: if Client.Bans becomes a
// one-method interface, a *Sync drops straight in.
//
// Why client-side enforcement is enough to silence someone: every path by
// which another person's words reach a user -- a knock, a stored note, a
// direct delivery -- ends at a client, and every client consults the same
// signed lists. A banned identity can still write to the DHT (storage-side
// refusal is a core change), but nothing it writes is ever shown.
//
// With several trusted operators, an identity is banned if ANY of their
// current lists names it. Each operator's list is kept and advanced
// independently, so one operator's unban can't lift another's ban, and
// one operator's unreadable list doesn't stall the rest.
type Sync struct {
	store   mailbox.Store
	trusted []string // canonical hex keys, config order

	refreshMu sync.Mutex // one Refresh/Import at a time, so their read-then-install can't interleave

	mu    sync.RWMutex
	lists map[string]*list // operator -> last good verified list
	users map[string]Ban   // banned username -> ban, merged across operators
}

// NewSync builds a Sync over s that trusts the given operator public keys
// (hex). Malformed keys are dropped rather than trusted as something that
// matches nothing, and with no valid keys at all a Sync bans nobody --
// the zero state, needing no special-casing by callers.
func NewSync(s mailbox.Store, trustedOperatorKeys []string) *Sync {
	y := &Sync{
		store: s,
		lists: make(map[string]*list),
		users: make(map[string]Ban),
	}
	seen := make(map[string]bool)
	for _, k := range trustedOperatorKeys {
		k = strings.ToLower(strings.TrimSpace(k))
		if validKey(k) && !seen[k] {
			seen[k] = true
			y.trusted = append(y.trusted, k)
		}
	}
	return y
}

// Refresh fetches every trusted operator's list and applies any that is
// newer than what is held. It is idempotent and meant to be called
// periodically: when nothing changed it costs one GetAll per operator and
// no signature checks (the held version is dropped before any), and when
// something did it fetches only the pages whose hashes are new.
//
// A failed refresh changes nothing -- the last good state stays in force,
// so a network outage (or someone withholding the list) can't lift a ban.
// Errors are informational: the state is still safe to use. Junk and
// forgeries at the key are not errors; they are ignored.
func (y *Sync) Refresh() error {
	y.refreshMu.Lock()
	defer y.refreshMu.Unlock()
	var errs []error
	for _, op := range y.trusted {
		if err := y.refreshOne(op); err != nil {
			errs = append(errs, fmt.Errorf("moderation: operator %.8s: %w", op, err))
		}
	}
	return errors.Join(errs...)
}

func (y *Sync) refreshOne(op string) error {
	candidates, err := y.store.GetAll(HeadKey(op))
	if errors.Is(err, mailbox.ErrNotFound) {
		return nil // no list published (or none reachable): keep what we have
	}
	if err != nil {
		return err
	}
	floor, cache := int64(0), map[string][]byte{}
	if cur := y.current(op); cur != nil {
		floor = cur.head.Version
		for i, ph := range cur.head.Pages {
			cache[ph] = cur.blobs[i]
		}
	}
	l, err := load(op, candidates, floor, func(ph string) ([]byte, error) {
		if b, ok := cache[ph]; ok {
			return b, nil
		}
		key, err := dht.IDFromHex(ph)
		if err != nil {
			return nil, err
		}
		return y.store.Get(key)
	})
	if l != nil {
		y.install(op, l)
	}
	return err
}

func (y *Sync) current(op string) *list {
	y.mu.RLock()
	defer y.mu.RUnlock()
	return y.lists[op]
}

// install makes l the operator's current list and rebuilds the merged
// set. Rebuilt whole rather than patched: it is linear in the number of
// bans and only runs when a list actually changed. The version check is
// repeated under the lock so nothing, whatever the call order, can move a
// list backwards.
func (y *Sync) install(op string, l *list) {
	y.mu.Lock()
	defer y.mu.Unlock()
	if cur := y.lists[op]; cur != nil && cur.head.Version >= l.head.Version {
		return
	}
	y.lists[op] = l
	users := make(map[string]Ban)
	for _, o := range y.trusted {
		ol := y.lists[o]
		if ol == nil {
			continue
		}
		for _, pg := range ol.pages {
			for _, e := range pg {
				if _, dup := users[e.User]; dup {
					continue
				}
				users[e.User] = Ban{Username: e.User, Reason: e.Reason, Since: e.Since, Operator: o}
			}
		}
	}
	y.users = users
}

// Banned reports whether the identity with this public key (hex) is
// banned. This is the check for anything that carries a signer:
// Note.Sender, Knock.Sender. The key is hashed to its username, which is
// what lists name; anything that isn't a key is not banned.
func (y *Sync) Banned(pubKeyHex string) bool {
	if y == nil {
		return false
	}
	pub, err := crypto.PublicKeyFromHex(strings.ToLower(strings.TrimSpace(pubKeyHex)))
	if err != nil {
		return false
	}
	return y.BannedPeer(crypto.PublicKeyToID(pub))
}

// Contains is Banned under mailbox.BanList's method name and signature,
// so a *Sync can stand in wherever a BanList-shaped checker is accepted.
func (y *Sync) Contains(pubKeyHex string) bool { return y.Banned(pubKeyHex) }

// BannedPeer reports whether a Relay username (peer ID) is banned. This is
// the check for everything keyed by peer -- conversations, requests,
// Inbox.Skip.
func (y *Sync) BannedPeer(username string) bool {
	if y == nil {
		return false
	}
	u := strings.ToLower(strings.TrimSpace(username))
	y.mu.RLock()
	defer y.mu.RUnlock()
	_, ok := y.users[u]
	return ok
}

// List returns every current ban, oldest first.
func (y *Sync) List() []Ban {
	y.mu.RLock()
	out := make([]Ban, 0, len(y.users))
	for _, b := range y.users {
		out = append(out, b)
	}
	y.mu.RUnlock()
	sort.Slice(out, func(i, j int) bool {
		if out[i].Since != out[j].Since {
			return out[i].Since < out[j].Since
		}
		return out[i].Username < out[j].Username
	})
	return out
}

// Lists describes each trusted operator's list currently held, in config
// order. Operators whose list has never loaded are absent.
func (y *Sync) Lists() []ListInfo {
	y.mu.RLock()
	defer y.mu.RUnlock()
	var out []ListInfo
	for _, op := range y.trusted {
		if l := y.lists[op]; l != nil {
			out = append(out, l.info())
		}
	}
	return out
}

// savedState is the Export format: each list exactly as it was fetched,
// signature and all, so Import can verify it from scratch.
type savedState struct {
	Lists []savedList `json:"lists"`
}

type savedList struct {
	Head  []byte   `json:"head"`
	Pages [][]byte `json:"pages"`
}

// Export serialises the verified lists for the caller to persist.
//
// Persisting matters for two reasons. A client that restarts with nothing
// enforces nothing until its first refresh succeeds, which on a bad
// network could be a while; and a client that restarts with nothing has
// no floor, so it would accept an old list someone replays at it. Saved
// state closes both.
func (y *Sync) Export() ([]byte, error) {
	y.mu.RLock()
	defer y.mu.RUnlock()
	st := savedState{Lists: []savedList{}}
	for _, op := range y.trusted {
		if l := y.lists[op]; l != nil {
			st.Lists = append(st.Lists, savedList{Head: l.raw, Pages: l.blobs})
		}
	}
	return json.Marshal(st)
}

// Import restores state saved by Export. Every list is verified again
// against THIS Sync's trusted keys -- a key dropped from the config since
// takes its bans with it -- and, like Refresh, is applied only if it is
// newer than what is already held. The saved file is treated as no more
// trustworthy than the DHT: it goes through the same load.
func (y *Sync) Import(saved []byte) error {
	y.refreshMu.Lock()
	defer y.refreshMu.Unlock()
	var st savedState
	if err := json.Unmarshal(saved, &st); err != nil {
		return fmt.Errorf("moderation: saved state: %w", err)
	}
	var errs []error
	for _, sl := range st.Lists {
		var probe struct {
			Operator string `json:"operator"`
		}
		if json.Unmarshal(sl.Head, &probe) != nil || !y.trusts(probe.Operator) {
			continue
		}
		pages := make(map[string][]byte, len(sl.Pages))
		for _, b := range sl.Pages {
			pages[hashHex(b)] = b
		}
		floor := int64(0)
		if cur := y.current(probe.Operator); cur != nil {
			floor = cur.head.Version
		}
		l, err := load(probe.Operator, [][]byte{sl.Head}, floor, func(ph string) ([]byte, error) {
			if b, ok := pages[ph]; ok {
				return b, nil
			}
			return nil, mailbox.ErrNotFound
		})
		if l != nil {
			y.install(probe.Operator, l)
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("moderation: saved list from %.8s: %w", probe.Operator, err))
		}
	}
	return errors.Join(errs...)
}

func (y *Sync) trusts(op string) bool {
	for _, t := range y.trusted {
		if t == op {
			return true
		}
	}
	return false
}
