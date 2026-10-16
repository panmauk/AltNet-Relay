package mailbox

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"altnet/core/crypto"
)

// Note is one entry in a sender's hash-chained mailbox to a specific
// recipient. It doesn't carry the message itself: Chunks lists the
// content-addressed DHT keys (hex) of the encrypted pieces (see Seal
// and SplitChunks), fetched separately.
//
// Each Note is stored twice, and both copies matter:
//
//   - at its own content address (Hash), an immutable archive copy, so
//     the chain can be walked backward by Prev;
//   - at MailboxKey(sender, recipient), the mutable slot holding the
//     current tip, which is the only entry point a recipient can find
//     from the two peer IDs alone.
//
// Prev chains back to the Hash of the previous Note in this direction
// (empty for the first message ever), so a recipient who's been offline
// walks backward from the tip and catches up on everything missed
// instead of seeing only the newest message.
//
// Seq is a 1-based monotonic counter per direction. It picks the tip
// among replicas holding different versions (highest wins, the same job
// NameRecord.Version does), and it's what the unaccepted-sender message
// cap is enforced against.
//
// Only the sender's own key may legitimately occupy this slot. Verify
// checks the signature is self-consistent and that the key binds to the
// claimed sender ID, but callers must still confirm that sender is the
// peer they actually expect -- see VerifyFrom.
type Note struct {
	Sender    string   `json:"sender"`         // hex Ed25519 pubkey of the sender
	Recipient string   `json:"recipient"`      // recipient's peer ID (hex)
	Chunks    []string `json:"chunks"`         // hex chunk hashes, in order
	Prev      string   `json:"prev,omitempty"` // Hash() of the previous Note in this chain
	Seq       int64    `json:"seq"`            // 1-based position in this chain
	Timestamp int64    `json:"ts"`
	Sig       string   `json:"sig,omitempty"`
}

// Marshal serialises the note for storage.
func (n *Note) Marshal() ([]byte, error) { return json.Marshal(n) }

// Hash returns this Note's own content hash -- its content address, and
// the Prev value of whichever Note follows it.
func (n *Note) Hash() string {
	blob, _ := json.Marshal(n)
	sum := sha256.Sum256(blob)
	return hex.EncodeToString(sum[:])
}

// Sign fills in Sender and Sig from id. Call last, after every other
// field is set.
func (n *Note) Sign(id *crypto.Identity) error {
	n.Sender = crypto.PublicKeyToHex(id.PublicKey)
	n.Sig = ""
	data, err := json.Marshal(n)
	if err != nil {
		return err
	}
	n.Sig = id.Sign(data)
	return nil
}

// Verify checks the embedded signature against the embedded sender key.
// It does NOT establish that the sender is who you expect -- use
// VerifyFrom for that.
//
// The signature must also be spelled canonically (see isCanonicalSig):
// Sig is the one field outside the signed bytes, so without that check
// anyone could re-case it and mint a second valid note with a different
// Hash, which a chain walk would treat as new.
func (n *Note) Verify() error {
	if !isCanonicalSig(n.Sig) {
		return errors.New("mailbox: note signature is not in canonical form")
	}
	pub, err := crypto.PublicKeyFromHex(n.Sender)
	if err != nil {
		return fmt.Errorf("mailbox: parse sender key: %w", err)
	}
	sig := n.Sig
	n.Sig = ""
	data, err := json.Marshal(n)
	n.Sig = sig
	if err != nil {
		return err
	}
	return crypto.Verify(pub, data, sig)
}

// VerifyFrom is the check callers should actually use: the signature
// must be valid, the signing key must hash to senderID, and the note
// must be addressed to recipientID.
//
// Both bindings matter. Without the sender check, anyone could plant a
// validly-signed note in someone else's slot (core/name's NameRecord
// verification stops short of this; deliberately not copied here).
// Without the recipient check, a note written for one conversation
// could be replayed into another.
//
// The bindings are checked before the signature because they cost a hash
// rather than an Ed25519 verification, and a planted note usually fails
// them: junk in a slot then costs almost nothing to discard.
func (n *Note) VerifyFrom(senderID, recipientID string) error {
	pub, err := crypto.PublicKeyFromHex(n.Sender)
	if err != nil {
		return fmt.Errorf("mailbox: parse sender key: %w", err)
	}
	if crypto.PublicKeyToID(pub) != senderID {
		return errors.New("mailbox: note not signed by the expected sender")
	}
	if n.Recipient != recipientID {
		return errors.New("mailbox: note addressed to a different recipient")
	}
	return n.Verify()
}

// SenderID returns the peer ID derived from the note's signing key.
func (n *Note) SenderID() (string, error) {
	pub, err := crypto.PublicKeyFromHex(n.Sender)
	if err != nil {
		return "", err
	}
	return crypto.PublicKeyToID(pub), nil
}

// NewNote builds, chains, and signs the next Note in sender's mailbox to
// recipient. prevHash is the Hash() of the previous Note in this chain
// ("" for the first), and seq is its 1-based position.
func NewNote(id *crypto.Identity, recipient string, chunks []string, prevHash string, seq int64) (*Note, error) {
	if recipient == "" {
		return nil, errors.New("mailbox: recipient required")
	}
	if len(chunks) == 0 {
		return nil, errors.New("mailbox: at least one chunk required")
	}
	if seq < 1 {
		return nil, errors.New("mailbox: seq must be 1 or greater")
	}
	n := &Note{
		Recipient: recipient,
		Chunks:    append([]string(nil), chunks...),
		Prev:      prevHash,
		Seq:       seq,
		Timestamp: time.Now().Unix(),
	}
	if err := n.Sign(id); err != nil {
		return nil, err
	}
	return n, nil
}
