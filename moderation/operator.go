package moderation

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"altnet-relay/mailbox"
	"altnet/core/crypto"
)

// Operator is the publishing side: one operator key editing its own list.
//
// It reads through a Sync that trusts only its own key, so the list it
// edits has passed exactly the checks every client applies -- an operator
// can never build on something clients would reject.
//
// Every edit starts from the newest list the operator can see, and refuses
// to publish when that is in doubt. The list is a snapshot, so an edit
// built on a stale base silently reverts every change made since: it
// would drop bans issued from another machine, or undo unbans. Two
// defences, in order: Import the operator's own saved copy first (the CLI
// keeps one beside the key), so a network that returns nothing can't make
// an established list look empty; and if the network holds a newer signed
// head that can't be fully loaded, refuse rather than guess.
type Operator struct {
	mu    sync.Mutex // one edit at a time: two concurrent edits would share a base and the later would drop the earlier
	store mailbox.Store
	id    *crypto.Identity
	pub   string
	view  *Sync
}

// NewOperator returns an Operator publishing to s under id.
func NewOperator(s mailbox.Store, id *crypto.Identity) *Operator {
	pub := crypto.PublicKeyToHex(id.PublicKey)
	return &Operator{store: s, id: id, pub: pub, view: NewSync(s, []string{pub})}
}

// PublicKey is the operator key clients must trust, hex.
func (o *Operator) PublicKey() string { return o.pub }

// Import restores the operator's own saved copy (from Export). Call it
// before the first edit whenever a copy exists.
func (o *Operator) Import(saved []byte) error { return o.view.Import(saved) }

// Export returns the list as last loaded or published, for saving.
func (o *Operator) Export() ([]byte, error) { return o.view.Export() }

// Load fetches the newest published list. It fails if a newer signed list
// exists but can't be loaded, which is also what makes an edit refuse.
func (o *Operator) Load() error { return o.view.Refresh() }

// Bans lists the current bans, as of the last Load or edit.
func (o *Operator) Bans() []Ban { return o.view.List() }

// Info describes the current list; ok is false if there is none yet.
func (o *Operator) Info() (ListInfo, bool) {
	ls := o.view.Lists()
	if len(ls) == 0 {
		return ListInfo{}, false
	}
	return ls[0], true
}

// Ban adds username to the list and publishes the new version. username
// is a Relay username; convert a key with UsernameOf, or a handle with
// ResolveTarget, first.
func (o *Operator) Ban(username, reason string) error {
	u := strings.ToLower(strings.TrimSpace(username))
	if !isLowerHex64(u) {
		return fmt.Errorf("moderation: %q is not a Relay username", username)
	}
	if err := checkReason(reason); err != nil {
		return err
	}
	return o.edit(func(pages [][]entry) ([][]entry, error) {
		for _, pg := range pages {
			for _, e := range pg {
				if e.User == u {
					return nil, ErrAlreadyBanned
				}
			}
		}
		e := entry{User: u, Reason: reason, Since: time.Now().Unix()}
		// Append to the last page while it has room, so every earlier
		// page -- and its hash, and every client's cached copy of it --
		// stays exactly as it was.
		if n := len(pages); n > 0 && pageFits(append(append([]entry(nil), pages[n-1]...), e)) {
			pages[n-1] = append(append([]entry(nil), pages[n-1]...), e)
			return pages, nil
		}
		return append(pages, []entry{e}), nil
	})
}

// Unban removes username from the list and publishes the new version. Only
// the page that held it changes; a page left empty is dropped.
func (o *Operator) Unban(username string) error {
	u := strings.ToLower(strings.TrimSpace(username))
	return o.edit(func(pages [][]entry) ([][]entry, error) {
		for i, pg := range pages {
			for j, e := range pg {
				if e.User == u {
					np := make([]entry, 0, len(pg)-1)
					np = append(np, pg[:j]...)
					np = append(np, pg[j+1:]...)
					pages[i] = np
					return pages, nil
				}
			}
		}
		return nil, ErrNotBanned
	})
}

// Republish stores the current list again, unchanged -- same version, same
// bytes. DHT values live only as long as the nodes holding them, so an
// operator should run this now and then to put the list back on whichever
// nodes are currently closest to its keys.
func (o *Operator) Republish() error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if err := o.Load(); err != nil {
		return err
	}
	cur := o.view.current(o.pub)
	if cur == nil {
		return errors.New("moderation: no list to republish")
	}
	return o.put(cur)
}

func (o *Operator) edit(change func([][]entry) ([][]entry, error)) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if err := o.Load(); err != nil {
		return fmt.Errorf("moderation: refusing to publish over a list that could not be loaded: %w", err)
	}
	var pages [][]entry
	floor := int64(0)
	if cur := o.view.current(o.pub); cur != nil {
		floor = cur.head.Version
		pages = append(pages, cur.pages...) // the page slices themselves are never mutated
	}
	pages, err := change(pages)
	if err != nil {
		return err
	}
	l, err := build(o.id, pages, nextVersion(floor))
	if err != nil {
		return err
	}
	if err := o.put(l); err != nil {
		return err
	}
	o.view.install(o.pub, l)
	return nil
}

// put stores every page before the head, so no client can ever see a head
// whose pages aren't there yet. Unchanged pages are stored again too: it
// is idempotent, and it refreshes their replicas along with the head's.
func (o *Operator) put(l *list) error {
	for i, blob := range l.blobs {
		if err := o.store.Put(mailbox.ContentAddress(blob), blob); err != nil {
			return fmt.Errorf("moderation: publish page %d: %w", i, err)
		}
	}
	if err := o.store.Put(HeadKey(o.pub), l.raw); err != nil {
		return fmt.Errorf("moderation: publish list head: %w", err)
	}
	return nil
}

// ResolveTarget turns what an operator typed into the username a ban
// names. Accepted forms:
//
//	pk:<hex>          an identity public key, hashed to its username
//	<64 hex>          a username, taken as-is
//	@handle, handle   resolved through mailbox.ResolveHandle
//
// Keys need the explicit "pk:" because a key and a username are both 64
// hex characters, and a key taken as a username bans nobody. The one
// mistake that would still slip through -- a bare key typed where a
// username belongs -- is caught when the key's own profile can be found:
// that is refused with a pointer to "pk:". Handles resolve under whatever
// handle authorities this process has configured, which must match what
// clients use, or the operator could ban a different identity than the
// one users see behind the handle.
func ResolveTarget(s mailbox.Store, target string) (string, error) {
	t := strings.TrimSpace(target)
	lower := strings.ToLower(t)
	switch {
	case strings.HasPrefix(lower, "pk:"):
		return UsernameOf(strings.TrimSpace(lower[len("pk:"):]))
	case isLowerHex64(lower):
		if _, err := mailbox.FetchProfile(s, lower); err != nil {
			if asKey, kerr := UsernameOf(lower); kerr == nil {
				if p, perr := mailbox.FetchProfile(s, asKey); perr == nil && p.PublicKey == lower {
					return "", fmt.Errorf("moderation: %.8s... is a public key, not a username; write pk:%s", lower, lower)
				}
			}
		}
		return lower, nil
	default:
		rec, err := mailbox.ResolveHandle(s, t)
		if err != nil {
			return "", err
		}
		return rec.Username, nil
	}
}

// AssignHandle publishes an operator-signed HandleRecord binding handle to
// username -- what authority mode (mailbox.SetTrustedHandleAuthorities)
// accepts in place of self-claims. ttl <= 0 means the assignment never
// lapses; otherwise it must be re-signed before it does.
//
// The version outranks whatever the handle resolves to now, so a
// reassignment wins over the previous owner; that lookup, like every
// handle lookup, sees only records this process's authority setting
// accepts. The version is also wall-clock based, so it outranks earlier
// assignments even when they can't be seen.
func AssignHandle(s mailbox.Store, id *crypto.Identity, handle, username string, ttl time.Duration) (*mailbox.HandleRecord, error) {
	if err := mailbox.ValidHandle(handle); err != nil {
		return nil, err
	}
	u := strings.ToLower(strings.TrimSpace(username))
	if !isLowerHex64(u) {
		return nil, fmt.Errorf("moderation: %q is not a Relay username", username)
	}
	canonical := mailbox.CanonicalHandle(handle)
	var floor int64
	if cur, err := mailbox.ResolveHandle(s, canonical); err == nil {
		floor = cur.Version
	}
	rec := &mailbox.HandleRecord{
		Handle:    canonical,
		Username:  u,
		Version:   nextVersion(floor),
		Timestamp: time.Now().Unix(),
	}
	if ttl > 0 {
		rec.TTL = int64(ttl / time.Second)
	}
	if err := rec.Sign(id); err != nil {
		return nil, err
	}
	blob, err := rec.Marshal()
	if err != nil {
		return nil, err
	}
	if err := s.Put(mailbox.HandleKey(canonical), blob); err != nil {
		return nil, fmt.Errorf("moderation: publish handle: %w", err)
	}
	return rec, nil
}
