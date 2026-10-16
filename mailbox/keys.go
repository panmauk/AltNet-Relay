package mailbox

import (
	"crypto/sha256"

	"altnet/core/dht"
)

// Domain-separation tags. Mixed into every derived key so mailbox and
// doorbell keys can never collide with each other, with content-chunk
// hashes, or with core/name's name-record keys, even if the underlying
// byte inputs happened to coincide.
const (
	mailboxDomain  = "altnet-relay-mailbox-v1"
	doorbellDomain = "altnet-relay-doorbell-v1"
)

// MailboxKey returns the DHT key for the pointer holding sender's
// messages to recipient. It's directional -- MailboxKey(bob, rob) and
// MailboxKey(rob, bob) are different keys -- and it's pure computation:
// anyone who knows both peer IDs can derive it independently, with no
// registration or setup step. That's what makes it a private, unlisted
// spot rather than something that needs provisioning: knowing both IDs
// is the only thing that unlocks it.
func MailboxKey(sender, recipient string) dht.NodeID {
	h := sha256.New()
	h.Write([]byte(mailboxDomain))
	h.Write([]byte{0})
	h.Write([]byte(sender))
	h.Write([]byte{0})
	h.Write([]byte(recipient))
	var id dht.NodeID
	copy(id[:], h.Sum(nil))
	return id
}

// DoorbellKey returns the DHT key for recipient's doorbell: the one
// address a first-time sender can compute from recipient's ID alone (see
// Knock) and recipient's own device has to watch -- exactly one address,
// no matter how many people might eventually try to reach them.
func DoorbellKey(recipient string) dht.NodeID {
	h := sha256.New()
	h.Write([]byte(doorbellDomain))
	h.Write([]byte{0})
	h.Write([]byte(recipient))
	var id dht.NodeID
	copy(id[:], h.Sum(nil))
	return id
}
