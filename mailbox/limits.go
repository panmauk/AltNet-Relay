package mailbox

import (
	"crypto/ed25519"
	"errors"
	"strings"
)

// Bounds on attacker-controlled input.
//
// Everything a Store returns is attacker-controlled: slot values are
// unverified by construction, and even content-addressed values only
// prove they weren't altered, not that anyone honest wrote them. Peer
// identities cost nothing to mint, so "one bad actor" is never one. These
// limits exist so that the work a client does in response to a read is
// bounded by constants rather than by how much junk someone planted.
const (
	// maxRecordSize is the largest slot value worth parsing. Every record
	// this package writes is well under 2 KiB (a Note at the full
	// MaxChunksPerMessage is about 1.5 KiB), so anything near
	// dht.MaxValueSize is junk, and dropping it before json.Unmarshal
	// means a flood of 64 KiB blobs costs a length check each instead of
	// a full parse.
	maxRecordSize = 16 * 1024

	// maxSlotCandidates caps how many candidates from one GetAll are
	// examined. On the live DHT each replica holds one value per key, so
	// honest reads see a handful; a read returning hundreds means the key
	// is surrounded by Sybils, who could equally withhold the honest
	// value outright. The cap therefore gives an attacker nothing new,
	// and it turns "one signature check per planted record" into a
	// constant.
	maxSlotCandidates = 256

	// maxDoorbellCandidates bounds how many chain heads ReadKnocks will
	// start walks from. Lower than maxSlotCandidates because each head
	// costs a walk, not one check.
	maxDoorbellCandidates = 64

	// maxDoorbellTotalWalk bounds the fetches one ReadKnocks makes across
	// ALL heads. MaxDoorbellWalk alone bounds each chain, which let an
	// attacker planting C disjoint chains cost C x MaxDoorbellWalk
	// fetches per read.
	maxDoorbellTotalWalk = 4 * MaxDoorbellWalk

	// maxClockSkew is how far into the future a timestamp may sit before
	// it is treated as a lie rather than a slow clock. Same tolerance
	// HandleRecord.Expired already used.
	maxClockSkew int64 = 300

	// maxPeerAddrs / maxAddrLen bound the reachability hints learned from
	// inside a peer's messages. The peer chooses them, and Courier.Deliver
	// dials them in order, so an unbounded list would let one message
	// make every later send of ours dial thousands of dead addresses.
	maxPeerAddrs = 8
	maxAddrLen   = 256

	// maxMessageIDLen bounds Message.ID, which is kept in dedup sets.
	// Honest IDs are 32 hex characters.
	maxMessageIDLen = 64
)

// boundCandidates drops oversized values and truncates the list to max.
// It is the first thing every GetAll caller does, before any parsing.
func boundCandidates(candidates [][]byte, max int) [][]byte {
	out := make([][]byte, 0, min(len(candidates), max))
	for _, blob := range candidates {
		if len(out) >= max {
			break
		}
		if len(blob) == 0 || len(blob) > maxRecordSize {
			continue
		}
		out = append(out, blob)
	}
	return out
}

// checkNoteShape rejects notes whose unsigned structure can't be honest,
// before any signature work. Seq below 1 matters most: the pending cap
// surfaces notes with Seq <= PendingMessageLimit, and without this a
// chain counting down through zero into negative numbers would satisfy
// it forever.
func checkNoteShape(n *Note) error {
	if n.Seq < 1 {
		return errors.New("mailbox: note has a non-positive seq")
	}
	if len(n.Chunks) == 0 || len(n.Chunks) > MaxChunksPerMessage {
		return errors.New("mailbox: note references a bad number of chunks")
	}
	return nil
}

// sanitizeAddrs keeps at most maxPeerAddrs plausible entries. It is
// deliberately loose about format -- addresses may be host:port or
// relay:// URLs, and future transports will add more -- and strict only
// about size and about characters that have no business in an address.
func sanitizeAddrs(addrs []string) []string {
	out := make([]string, 0, min(len(addrs), maxPeerAddrs))
	for _, a := range addrs {
		if len(out) >= maxPeerAddrs {
			break
		}
		if a == "" || len(a) > maxAddrLen || strings.ContainsFunc(a, func(r rune) bool {
			return r <= ' ' || r >= 0x7f
		}) {
			continue
		}
		out = append(out, a)
	}
	return out
}

// isLowerHexID reports whether s is a 64-character lowercase hex string,
// the only spelling of a username that derives the right keys: MailboxKey
// and friends hash the string itself, so "ABC..." and "abc..." would
// silently address different slots.
func isLowerHexID(s string) bool {
	return len(s) == 64 && isLowerHex(s)
}

// isCanonicalSig reports whether sig is spelled the one way Sign spells
// it: 128 lowercase hex characters.
//
// crypto.Verify decodes hex without caring about case, and Sig is the
// one field outside the signed bytes. Without this, anyone -- not just
// the signer -- could re-case a record's signature and mint a second,
// equally valid copy with a different Hash: a "new" note or knock that
// nobody wrote, which chain walks and dedup-by-hash would treat as
// distinct.
func isCanonicalSig(sig string) bool {
	return len(sig) == 2*ed25519.SignatureSize && isLowerHex(sig)
}

func isLowerHex(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// isFutureDated reports whether a signed timestamp sits further ahead of
// now than clock skew explains. Such a record is either from a broken
// clock or written to outrank honest ones wherever "newest wins", and
// either way it is not believed until its time comes.
func isFutureDated(ts, now int64) bool {
	return ts > now+maxClockSkew
}

// asciiLower lowercases A-Z and leaves every other byte alone.
// strings.ToLower is wrong wherever the result names a key: it folds a
// few non-ASCII runes INTO ASCII (the Kelvin sign U+212A becomes 'k'),
// so visibly different input would pass an a-z check and land on the
// same key as the plain spelling.
func asciiLower(s string) string {
	for i := 0; i < len(s); i++ {
		if c := s[i]; 'A' <= c && c <= 'Z' {
			b := []byte(s)
			for j := i; j < len(b); j++ {
				if 'A' <= b[j] && b[j] <= 'Z' {
					b[j] += 'a' - 'A'
				}
			}
			return string(b)
		}
	}
	return s
}
