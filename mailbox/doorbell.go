package mailbox

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"altnet/core/dht"
)

// MaxDoorbellWalk bounds how far back a doorbell chain is followed in
// one read, so a flood of knocks can't make a recipient walk forever.
const MaxDoorbellWalk = 256

// knock leaves a first-contact card on recipient's doorbell, chaining
// after whatever the doorbell's current tip is.
//
// Like a Note, each Knock is stored twice: at its own content address so
// it stays fetchable once superseded, and at the doorbell slot as the
// new tip.
func (c *Client) knock(recipient, introHash string) error {
	var prevTip string
	if tip, err := c.doorbellTip(recipient); err == nil && tip != nil {
		prevTip = tip.Hash()
	}

	k, err := NewKnock(c.ID, c.Box, recipient, introHash, prevTip)
	if err != nil {
		return err
	}
	blob, err := json.Marshal(k)
	if err != nil {
		return err
	}
	hash, err := dht.IDFromHex(k.Hash())
	if err != nil {
		return err
	}
	if err := c.Store.Put(hash, blob); err != nil {
		return fmt.Errorf("mailbox: archive knock: %w", err)
	}
	if err := c.Store.Put(DoorbellKey(recipient), blob); err != nil {
		return fmt.Errorf("mailbox: store knock: %w", err)
	}
	return nil
}

// doorbellTip returns the most recent verifiable Knock at recipient's
// doorbell, or ErrNotFound if there are none. Used only to pick a Prev
// when writing; readers should use ReadKnocks, which tolerates forks.
func (c *Client) doorbellTip(recipient string) (*Knock, error) {
	candidates, err := c.Store.GetAll(DoorbellKey(recipient))
	if err != nil {
		return nil, err
	}
	now := time.Now().Unix()
	var best *Knock
	for _, blob := range boundCandidates(candidates, maxSlotCandidates) {
		var k Knock
		if err := json.Unmarshal(blob, &k); err != nil {
			continue
		}
		if c.banned(k.Sender) || !knockChainable(&k, recipient) {
			continue
		}
		// Newest wins here, so a future-dated card would stay the tip
		// until its date came, and every honest knocker in the meantime
		// would chain after it. With Prev "" that orphans everything
		// older from each new head. Not believed until its time comes.
		if isFutureDated(k.Timestamp, now) {
			continue
		}
		if best == nil || k.Timestamp > best.Timestamp {
			rec := k
			best = &rec
		}
	}
	if best == nil {
		return nil, ErrNotFound
	}
	return best, nil
}

// ReadKnocks returns every first-contact Knock on this client's own
// doorbell, oldest-first.
//
// This is the ONE address a client has to watch, no matter how many
// people might eventually try to reach it -- which is the whole reason
// the doorbell exists.
//
// Because the DHT can't serialise writes, several strangers knocking at
// once will fork the chain. So rather than trusting a single tip, this
// walks back from EVERY candidate the slot returns and dedupes by hash:
// a fork costs a little extra walking and loses nobody. Knocks from
// banned identities and knocks that fail verification are dropped.
func (c *Client) ReadKnocks() ([]*Knock, error) {
	self := c.ID.ID()
	candidates, err := c.Store.GetAll(DoorbellKey(self))
	if err != nil {
		if err == ErrNotFound {
			return nil, nil
		}
		return nil, err
	}

	now := time.Now().Unix()
	seen := make(map[string]bool)
	var found []*Knock

	// Every candidate is a chain head; follow each one back. The walks
	// advance in rounds, one link per head per round, under one budget
	// shared by all of them. Heads are free to plant, so walking each to
	// the end in turn would let a few long planted chains spend the
	// whole budget before an honest head was reached; in rounds, every
	// head gets its share.
	type walk struct {
		cur   *Knock
		steps int
	}
	var walks []*walk
	for _, blob := range boundCandidates(candidates, maxDoorbellCandidates) {
		var k Knock
		if err := json.Unmarshal(blob, &k); err != nil {
			continue
		}
		walks = append(walks, &walk{cur: &k})
	}

	// step examines w's current knock and moves it one link back,
	// reporting whether the walk should continue.
	step := func(w *walk) bool {
		cur := w.cur
		h := cur.Hash()
		if seen[h] {
			return false // already walked this section of the chain
		}
		seen[h] = true
		if !knockChainable(cur, self) {
			// Honest knockers only chain after cards that pass this
			// (doorbellTip), so nothing honest lies behind one that
			// doesn't. Stopping here also means junk costs one check.
			return false
		}
		// Banned and future-dated cards are skipped but walked THROUGH:
		// an honest knocker may well have chained after them.
		if !c.banned(cur.Sender) && !isFutureDated(cur.Timestamp, now) {
			found = append(found, cur)
		}
		w.steps++
		if cur.Prev == "" || w.steps >= MaxDoorbellWalk {
			return false
		}
		prev, err := c.fetchKnockByHash(cur.Prev)
		if err != nil {
			return false
		}
		w.cur = prev
		return true
	}

	for budget := maxDoorbellTotalWalk; len(walks) > 0 && budget > 0; {
		live := walks[:0]
		for _, w := range walks {
			if budget <= 0 {
				break
			}
			budget--
			if step(w) {
				live = append(live, w)
			}
		}
		walks = live
	}

	// Oldest-first, so a UI can render the request list in arrival order.
	for i, j := 0, len(found)-1; i < j; i, j = i+1, j-1 {
		found[i], found[j] = found[j], found[i]
	}
	return found, nil
}

func (c *Client) fetchKnockByHash(hash string) (*Knock, error) {
	key, err := dht.IDFromHex(hash)
	if err != nil {
		return nil, err
	}
	blob, err := c.Store.Get(key)
	if err != nil {
		return nil, err
	}
	if len(blob) > maxRecordSize {
		return nil, errors.New("mailbox: archived knock is oversized")
	}
	var k Knock
	if err := json.Unmarshal(blob, &k); err != nil {
		return nil, err
	}
	return &k, nil
}

// knockChainable reports whether k is a card an honest knocker would
// chain after: addressed to recipient and validly signed. Bans are the
// reader's own policy, and dates change with time, so both are left to
// the caller.
func knockChainable(k *Knock, recipient string) bool {
	return k.Recipient == recipient && k.Verify() == nil
}

// ConversationFromKnock turns an incoming Knock into a pending
// Conversation: not accepted yet, so Receive will surface at most
// PendingMessageLimit messages until the user accepts it.
//
// The knocker's box key is taken straight from the Knock, so replying
// needs no profile lookup.
func ConversationFromKnock(k *Knock) (*Conversation, error) {
	senderID, err := k.SenderID()
	if err != nil {
		return nil, err
	}
	return &Conversation{
		PeerID:     senderID,
		PeerBoxKey: k.BoxKey,
		Accepted:   false,
	}, nil
}
