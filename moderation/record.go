// Package moderation carries an operator's decisions -- identity bans and,
// in production, handle assignments -- from the operator to every Relay
// client through the DHT, and gives clients what they need to enforce them.
//
// It is the Relay counterpart of core/dht's signed revokes, with one
// deliberate difference in delivery. A revoke is a one-shot gossip: a node
// that is offline when it goes out never hears it. A ban has to reach
// clients that were offline, clients installed a year later, and it has to
// carry UNbans too, so it is published as state rather than as an event:
// each operator keeps one signed ban list in the DHT (Operator) and clients
// poll it (Sync.Refresh).
//
// Enforcement is client-side only. Storage-side refusal would need nodes to
// understand Relay records, which is a core change; see Sync for why the
// client-side half still silences a banned identity everywhere a user
// would notice it.
//
// Everything read back is attacker-controlled. The only thing trusted is
// the list of operator public keys a client is configured with, exactly as
// core/dht trusts only its configured revokers.
package moderation

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"
	"unicode/utf8"

	"altnet/core/crypto"
	"altnet/core/dht"
)

// A ban list is one signed head plus content-addressed pages:
//
//	HeadKey(operator) -> listHead{version, page hashes, count, sig}
//	sha256(page)      -> listPage{entries}
//
// Only the head is signed. Each page is fetched by its hash and checked
// against it, and those hashes sit inside the signed head, so one signature
// authenticates the whole list however many pages it spans -- the same
// trick a .alt site's tree of chunk hashes uses. Pages are therefore
// immutable and shared between versions: a ban that only touches the last
// page leaves every other page's hash unchanged, and a client that already
// holds those pages never fetches them again.
//
// The list is a SNAPSHOT of who is banned now, not a log of ban and unban
// events. An unban is simply absence from a newer snapshot, so there are no
// tombstones to keep forever, and replay protection reduces to one rule: a
// client never accepts a version at or below the one it already holds.
const (
	// banDomain separates everything here from every other use of an
	// operator key. The same key typically also signs HandleRecords in
	// authority mode, so the signed bytes are prefixed with it: no
	// signature over a ban list can ever be passed off as one over a
	// handle, or the reverse.
	banDomain = "altnet-relay-banlist-v1"
	headKind  = "relay-banlist-head-v1"
	pageKind  = "relay-banlist-page-v1"
)

// Bounds. The head is the only attacker-writable input a client parses:
// pages are only ever fetched when a head that verified against a trusted
// operator key names them. So these split into limits on what an honest
// operator may publish (MaxPages, MaxReasonLen, pageBudget) and limits on
// how much junk at the head key a client will look at.
const (
	// MaxPages caps one operator's list. 512 page hashes keep the head
	// itself around 35 KiB, inside dht.MaxValueSize; at pageBudget that
	// is tens of thousands of bans even with maximum-length reasons, and
	// over two hundred thousand with short reason codes.
	MaxPages = 512

	// MaxReasonLen caps a ban's reason, in bytes. Reasons are PUBLIC --
	// anyone can read the list -- so they should be policy codes
	// ("tos-4.1"), not case notes.
	MaxReasonLen = 200

	// pageBudget is how full the publisher packs a page. Deliberately
	// short of dht.MaxValueSize so an edited page never lands just over
	// the limit.
	pageBudget = 48 * 1024

	// maxPageEntries bounds entries per page as seen by a reader; an
	// honest page at pageBudget holds a few hundred.
	maxPageEntries = 1024

	// maxHeadCandidates caps how many values from one GetAll are
	// examined, and so how many signature checks junk at the key can
	// cost: every candidate is parsed at most once and verified at most
	// once. On the live DHT each replica holds one value per key, so an
	// honest read sees a handful; hundreds means Sybils surround the
	// key, and they could equally withhold the honest head outright, so
	// the cap costs nothing real. Same reasoning as mailbox's
	// maxSlotCandidates.
	maxHeadCandidates = 64

	// maxHeadAttempts bounds how many VALIDLY SIGNED heads one refresh
	// will try to load pages for. Forgeries never get this far, but old
	// genuine heads can be replayed by anyone, and each one names up to
	// MaxPages pages to fetch.
	maxHeadAttempts = 3
)

var (
	// ErrListFull means a ban would push the list past MaxPages.
	ErrListFull = errors.New("moderation: ban list is full")
	// ErrAlreadyBanned and ErrNotBanned report a ban or unban that would
	// change nothing. Nothing is published in either case.
	ErrAlreadyBanned = errors.New("moderation: already banned")
	ErrNotBanned     = errors.New("moderation: not banned")
)

// sigChecks counts signature verifications, so the tests can prove a flood
// of forged heads costs a bounded number of them.
var sigChecks atomic.Int64

// HeadKey is the DHT slot holding an operator's ban-list head. One slot per
// operator key rather than one shared slot: each replica holds a single
// value per key, so two operators sharing a slot would overwrite each
// other's lists. Clients know every trusted key, so they can derive every
// slot.
func HeadKey(operatorPubHex string) dht.NodeID {
	h := sha256.New()
	h.Write([]byte(banDomain))
	h.Write([]byte{0})
	h.Write([]byte("head"))
	h.Write([]byte{0})
	h.Write([]byte(strings.ToLower(strings.TrimSpace(operatorPubHex))))
	var id dht.NodeID
	copy(id[:], h.Sum(nil))
	return id
}

// listHead is the signed root of one operator's list.
type listHead struct {
	Kind      string   `json:"kind"`
	Operator  string   `json:"operator"` // hex Ed25519 key that signed this
	Version   int64    `json:"version"`  // strictly increasing; a client never goes back
	Pages     []string `json:"pages"`    // hex sha256 of each page, in order
	Count     int      `json:"count"`    // total entries, checked against the pages
	Timestamp int64    `json:"ts"`       // publish time, informational only
	Sig       string   `json:"sig,omitempty"`
}

// listPage is one content-addressed page of entries.
type listPage struct {
	Kind    string  `json:"kind"`
	Entries []entry `json:"entries"`
}

// entry is one banned identity, named by its Relay USERNAME -- the hex
// sha256 of its Ed25519 public key -- never by the key itself.
//
// The username is the one name that answers every question a client asks.
// Checking a signer (Note.Sender, Knock.Sender) means hashing the key it
// carries, one sha256; checking a conversation, request or Inbox.Skip is a
// direct lookup. Naming the key instead would add nothing a client needs,
// and would force the operator to learn the key first, which it can only
// do from the identity's own profile -- something a spammer who only ever
// knocks never needs to publish. It also removes an ambiguity that would
// otherwise be silent: both forms are 64 hex characters, and a key filed
// where a username belongs is a ban that matches nobody. So the list holds
// exactly one kind of name, and everything that accepts a key converts it
// (UsernameOf) before it gets anywhere near a list.
type entry struct {
	User   string `json:"user"`
	Reason string `json:"reason,omitempty"`
	Since  int64  `json:"since"` // unix seconds
}

// Ban is one banned identity as a client sees it.
type Ban struct {
	Username string // Relay username (peer ID): hex sha256 of the identity key
	Reason   string
	Since    int64  // unix seconds
	Operator string // hex key of the operator whose list names it
}

// ListInfo describes one operator's current list.
type ListInfo struct {
	Operator  string
	Version   int64
	Published int64 // unix seconds, as the operator stamped it
	Count     int
	Pages     int
}

func (h *listHead) signedBytes() ([]byte, error) {
	c := *h
	c.Sig = ""
	body, err := json.Marshal(&c)
	if err != nil {
		return nil, err
	}
	return append([]byte(banDomain+"\n"), body...), nil
}

func (h *listHead) sign(id *crypto.Identity) error {
	h.Operator = crypto.PublicKeyToHex(id.PublicKey)
	h.Sig = ""
	data, err := h.signedBytes()
	if err != nil {
		return err
	}
	h.Sig = id.Sign(data)
	return nil
}

func (h *listHead) verify() error {
	sigChecks.Add(1)
	pub, err := crypto.PublicKeyFromHex(h.Operator)
	if err != nil {
		return err
	}
	data, err := h.signedBytes()
	if err != nil {
		return err
	}
	return crypto.Verify(pub, data, h.Sig)
}

// list is one operator's list, fully loaded and verified.
type list struct {
	head  listHead
	raw   []byte    // the head exactly as stored, so it can be re-put or saved unchanged
	pages [][]entry // parallel to head.Pages
	blobs [][]byte  // raw page values, parallel to head.Pages
}

func (l *list) info() ListInfo {
	return ListInfo{
		Operator:  l.head.Operator,
		Version:   l.head.Version,
		Published: l.head.Timestamp,
		Count:     l.head.Count,
		Pages:     len(l.head.Pages),
	}
}

// parseHead is the cheap, signature-free screen every candidate passes
// before anything else is spent on it.
//
// The stored bytes must be exactly the canonical encoding of what they
// decode to. Go's JSON decoder forgives a lot -- field names in any case,
// repeated fields, stray whitespace, unknown fields -- and the signature
// is over the re-encoding, so without this one genuine head could be
// respelled into any number of distinct values that all verify. Each
// would be a separate DHT value, a separate signature check and a
// separate "signed head" for maxHeadAttempts to count; requiring the
// canonical form makes every genuine head exactly one value.
func parseHead(operator string, blob []byte, floor int64) (listHead, bool) {
	var h listHead
	if len(blob) == 0 || len(blob) > dht.MaxValueSize {
		return h, false
	}
	if json.Unmarshal(blob, &h) != nil {
		return h, false
	}
	// Version is bounded away from both ends: <= 0 is never issued, and
	// MaxInt64 would leave nothing above it, so a publisher could never
	// supersede it (nextVersion would overflow).
	if h.Kind != headKind || h.Operator != operator || h.Version <= floor || h.Version <= 0 || h.Version == math.MaxInt64 {
		return h, false
	}
	if len(h.Pages) > MaxPages || h.Count < 0 || h.Count > len(h.Pages)*maxPageEntries {
		return h, false
	}
	if !allLowerHex64(h.Pages) || !isLowerHex(h.Sig, 128) {
		return h, false
	}
	canon, err := json.Marshal(&h)
	if err != nil || !bytes.Equal(canon, blob) {
		return h, false
	}
	return h, true
}

// load picks the newest usable list among the candidate heads found at one
// operator's HeadKey.
//
// floor is the version already held. Anything at or below it is dropped
// before any signature work, and that single comparison is the whole
// replay defence: an old list, however validly signed, can never displace
// a newer one, so re-serving it can neither un-ban someone banned since
// nor re-ban someone unbanned since.
//
// Forged, malformed and foreign candidates are skipped silently, as
// everywhere in mailbox -- junk someone planted is not an error. It
// returns (nil, nil) when nothing newer exists. A non-nil error means a
// genuinely signed newer head could not be loaded (a page missing or
// malformed); if an older-but-still-newer head loaded instead, both come
// back, so the caller can make progress and still know it isn't current.
//
// Cost is bounded whatever sits at the key: at most maxHeadCandidates
// values parsed and verified, at most maxHeadAttempts of those walked, and
// each walk at most MaxPages page fetches.
func load(operator string, candidates [][]byte, floor int64, getPage func(hash string) ([]byte, error)) (*list, error) {
	type cand struct {
		head listHead
		raw  []byte
	}
	var cs []cand
	for i, blob := range candidates {
		if i >= maxHeadCandidates {
			break
		}
		if h, ok := parseHead(operator, blob, floor); ok {
			cs = append(cs, cand{head: h, raw: blob})
		}
	}
	// Newest first, so the common case is one signature check and done.
	// A forgery claiming a huge version sorts first too, but costs only
	// its one failed check: the loop moves on without counting it.
	sort.SliceStable(cs, func(i, j int) bool { return cs[i].head.Version > cs[j].head.Version })

	var failed error
	attempts := 0
	for _, c := range cs {
		if attempts >= maxHeadAttempts {
			break
		}
		if c.head.verify() != nil {
			continue
		}
		attempts++
		l, err := open(c.head, c.raw, getPage)
		if err != nil {
			if failed == nil {
				failed = fmt.Errorf("signed list version %d could not be loaded: %w", c.head.Version, err)
			}
			continue
		}
		return l, failed
	}
	return nil, failed
}

// open fetches and checks every page a verified head names. It stops at
// the first bad page: the head is signed, so a bad page means the list
// is incomplete, and a partial list must never be installed -- it would
// silently lift every ban on the missing pages.
func open(h listHead, raw []byte, getPage func(hash string) ([]byte, error)) (*list, error) {
	l := &list{
		head:  h,
		raw:   raw,
		pages: make([][]entry, 0, len(h.Pages)),
		blobs: make([][]byte, 0, len(h.Pages)),
	}
	seen := make(map[string]bool, h.Count)
	for _, ph := range h.Pages {
		blob, err := getPage(ph)
		if err != nil {
			return nil, fmt.Errorf("page %.8s: %w", ph, err)
		}
		// Checked here even though Store.Get verifies content addresses:
		// the page cache and saved state bypass Get, and the check is one
		// hash.
		if len(blob) > dht.MaxValueSize || hashHex(blob) != ph {
			return nil, fmt.Errorf("page %.8s does not match its hash", ph)
		}
		var p listPage
		if err := json.Unmarshal(blob, &p); err != nil {
			return nil, fmt.Errorf("page %.8s: %w", ph, err)
		}
		if p.Kind != pageKind || len(p.Entries) == 0 || len(p.Entries) > maxPageEntries {
			return nil, fmt.Errorf("page %.8s is malformed", ph)
		}
		for _, e := range p.Entries {
			if !isLowerHex64(e.User) || checkReason(e.Reason) != nil {
				return nil, fmt.Errorf("page %.8s has a malformed entry", ph)
			}
			if seen[e.User] {
				return nil, fmt.Errorf("page %.8s repeats an entry", ph)
			}
			seen[e.User] = true
		}
		if len(seen) > h.Count {
			return nil, errors.New("pages hold more entries than the head declares")
		}
		l.pages = append(l.pages, p.Entries)
		l.blobs = append(l.blobs, blob)
	}
	if len(seen) != h.Count {
		return nil, errors.New("pages hold fewer entries than the head declares")
	}
	return l, nil
}

// build packs pages into a signed list at version. Empty pages are dropped
// rather than published, so an unban that empties a page shrinks the list.
func build(id *crypto.Identity, pages [][]entry, version int64) (*list, error) {
	l := &list{}
	count := 0
	var hashes []string
	for _, pg := range pages {
		if len(pg) == 0 {
			continue
		}
		blob, err := json.Marshal(listPage{Kind: pageKind, Entries: pg})
		if err != nil {
			return nil, err
		}
		if len(blob) > dht.MaxValueSize || len(pg) > maxPageEntries {
			return nil, errors.New("moderation: a page overflowed")
		}
		l.pages = append(l.pages, pg)
		l.blobs = append(l.blobs, blob)
		hashes = append(hashes, hashHex(blob))
		count += len(pg)
	}
	if len(hashes) > MaxPages {
		return nil, ErrListFull
	}
	l.head = listHead{
		Kind:      headKind,
		Version:   version,
		Pages:     hashes,
		Count:     count,
		Timestamp: time.Now().Unix(),
	}
	if l.head.Pages == nil {
		l.head.Pages = []string{} // "pages": [] rather than null; an empty list is a real state
	}
	if err := l.head.sign(id); err != nil {
		return nil, err
	}
	raw, err := json.Marshal(&l.head)
	if err != nil {
		return nil, err
	}
	if len(raw) > dht.MaxValueSize {
		return nil, ErrListFull
	}
	l.raw = raw
	return l, nil
}

// pageFits reports whether a page holding entries stays within pageBudget.
func pageFits(entries []entry) bool {
	if len(entries) > maxPageEntries {
		return false
	}
	blob, err := json.Marshal(listPage{Kind: pageKind, Entries: entries})
	return err == nil && len(blob) <= pageBudget
}

// Versions follow mailbox's nextVersion: wall-clock nanoseconds, floored
// above both the last version this process issued and the version already
// published. Time-based rather than a bare counter so that an operator
// publishing from a second machine still outranks what it can't see, and
// floored because Windows' clock is coarse enough for two publishes to
// read the same nanosecond.
var (
	versionMu   sync.Mutex
	lastVersion int64
)

func nextVersion(floor int64) int64 {
	versionMu.Lock()
	defer versionMu.Unlock()
	v := time.Now().UnixNano()
	if v <= lastVersion {
		v = lastVersion + 1
	}
	if v <= floor {
		v = floor + 1
	}
	lastVersion = v
	return v
}

// UsernameOf converts an identity public key (hex) to the Relay username
// a ban names. Strict about the input: a key that is not canonical 64-char
// lowercase hex is an error rather than a guess, since a wrong guess is a
// ban on nobody.
func UsernameOf(pubKeyHex string) (string, error) {
	pub, err := crypto.PublicKeyFromHex(pubKeyHex)
	if err != nil || !isLowerHex64(pubKeyHex) {
		return "", fmt.Errorf("moderation: %q is not a hex Ed25519 public key", pubKeyHex)
	}
	return crypto.PublicKeyToID(pub), nil
}

// validKey reports whether s is a canonical (64-char lowercase hex) Ed25519
// public key. Canonical because it is compared as a string.
func validKey(s string) bool {
	if !isLowerHex64(s) {
		return false
	}
	_, err := crypto.PublicKeyFromHex(s)
	return err == nil
}

// checkReason keeps reasons short and printable. They are shown in admin
// tools and possibly in clients, so control characters (terminal escapes
// included) and invisible format characters (bidi overrides, which can
// make a reason read as something else) have no business in them.
func checkReason(r string) error {
	if len(r) > MaxReasonLen {
		return fmt.Errorf("moderation: reason is longer than %d bytes", MaxReasonLen)
	}
	if !utf8.ValidString(r) {
		return errors.New("moderation: reason is not valid UTF-8")
	}
	if strings.IndexFunc(r, func(c rune) bool { return unicode.IsControl(c) || unicode.Is(unicode.Cf, c) }) >= 0 {
		return errors.New("moderation: reason contains control or format characters")
	}
	return nil
}

func hashHex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func isLowerHex(s string, n int) bool {
	if len(s) != n {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

func isLowerHex64(s string) bool { return isLowerHex(s, 64) }

func allLowerHex64(ss []string) bool {
	for _, s := range ss {
		if !isLowerHex64(s) {
			return false
		}
	}
	return true
}
