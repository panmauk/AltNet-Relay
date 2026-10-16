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

// Knock is a first-contact announcement left at a recipient's doorbell
// (DoorbellKey). Unlike Note, which only ever has one legitimate signer
// per slot, the doorbell is a corkboard: any stranger who knows the
// recipient's ID can pin up their own card. So a Knock is chained the
// same way a Note is (Prev links to the hash of whatever Knock was the
// doorbell's tip when this one was written), but verification only
// checks that a Knock is validly signed by whoever it claims to be from
// -- there's no single "right" signer to compare against here, unlike
// Note.
//
// Writing a Knock reads the doorbell's current tip and chains after it.
// The DHT has no compare-and-swap, so two strangers knocking at the
// same moment will both chain from the same Prev and fork the chain.
// That's fine and is handled on the read side rather than prevented on
// the write side: ReadKnocks walks every candidate the slot returns,
// follows each chain back, and dedupes by hash, so a fork loses nobody.
// Each Knock is also stored at its own content address so those older
// links stay fetchable after the tip moves on.
type Knock struct {
	Sender    string `json:"sender"`         // hex Ed25519 pubkey of whoever's knocking
	BoxKey    string `json:"box_key"`        // sender's hex X25519 box public key, so the recipient can Seal a reply
	Recipient string `json:"recipient"`      // whose doorbell this is
	Intro     string `json:"intro"`          // Hash() of the first Note already stored at MailboxKey(sender, recipient)
	Prev      string `json:"prev,omitempty"` // hash of the doorbell's previous tip, "" if this is the first-ever knock
	Timestamp int64  `json:"ts"`
	Sig       string `json:"sig,omitempty"`
}

// Hash returns this Knock's own content hash, used as the Prev value of
// whichever Knock supersedes it as the doorbell's tip.
func (k *Knock) Hash() string {
	blob, _ := json.Marshal(k)
	sum := sha256.Sum256(blob)
	return hex.EncodeToString(sum[:])
}

// Sign fills in Sender and Sig from id.
func (k *Knock) Sign(id *crypto.Identity) error {
	k.Sender = crypto.PublicKeyToHex(id.PublicKey)
	k.Sig = ""
	data, err := json.Marshal(k)
	if err != nil {
		return err
	}
	k.Sig = id.Sign(data)
	return nil
}

// Verify checks only that the Knock is validly signed by whoever it
// claims to be from. There is no "expected signer" to additionally check
// against, unlike Note.Verify -- anyone is a legitimate knocker.
//
// The signature must be spelled canonically, as with Note.Verify: a
// re-cased copy would otherwise be a "different" knock under Hash, and
// every reader would surface the same card twice.
func (k *Knock) Verify() error {
	if !isCanonicalSig(k.Sig) {
		return errors.New("mailbox: knock signature is not in canonical form")
	}
	pub, err := crypto.PublicKeyFromHex(k.Sender)
	if err != nil {
		return fmt.Errorf("mailbox: parse knocker key: %w", err)
	}
	sig := k.Sig
	k.Sig = ""
	data, err := json.Marshal(k)
	k.Sig = sig
	if err != nil {
		return err
	}
	return crypto.Verify(pub, data, sig)
}

// SenderID returns the peer ID derived from the knocker's signing key.
// This is how a recipient learns the ID it needs in order to compute
// MailboxKey for the conversation -- the missing ingredient that made
// the doorbell necessary in the first place.
func (k *Knock) SenderID() (string, error) {
	pub, err := crypto.PublicKeyFromHex(k.Sender)
	if err != nil {
		return "", err
	}
	return crypto.PublicKeyToID(pub), nil
}

// NewKnock builds and signs a Knock announcing id's first message to
// recipient. introHash is the Hash() of the first Note already placed at
// MailboxKey(id.ID(), recipient). prevTip is the doorbell's current tip
// hash (Knock.Hash() of whatever DoorbellKey(recipient) currently holds),
// or "" if the doorbell is empty.
func NewKnock(id *crypto.Identity, box *BoxKey, recipient, introHash, prevTip string) (*Knock, error) {
	if recipient == "" {
		return nil, errors.New("mailbox: recipient required")
	}
	if introHash == "" {
		return nil, errors.New("mailbox: introHash required")
	}
	k := &Knock{
		BoxKey:    BoxPublicKeyToHex(box.Public),
		Recipient: recipient,
		Intro:     introHash,
		Prev:      prevTip,
		Timestamp: time.Now().Unix(),
	}
	if err := k.Sign(id); err != nil {
		return nil, err
	}
	return k, nil
}
