// Package dhtstore adapts a live AltNet Kademlia DHT to the storage
// interface the mailbox layer needs.
//
// It is deliberately thin: mailbox.Store was shaped to match the methods
// *dht.DHT already exposes, so this is a translation layer and nothing
// more. All the behaviour lives in mailbox, where it can be tested
// offline against MemStore; this file is what makes those same flows run
// on the real network, on the same nodes that already serve .alt sites.
package dhtstore

import (
	"errors"

	"altnet-relay/mailbox"
	"altnet/core/dht"
)

// Store implements mailbox.Store on top of a live DHT.
type Store struct {
	D *dht.DHT
}

// New wraps a live DHT as a mailbox.Store.
func New(d *dht.DHT) *Store { return &Store{D: d} }

// Put stores a value locally and replicates it to the K closest peers.
//
// Uses the synchronous dht.Store rather than the StoreLocal +
// background-Replicate pattern apps/files uses for site publishing.
// That pattern exists because publishing a whole site means hundreds of
// chunks, and a synchronous Lookup each would serialize into minutes. A
// chat message is one or two small values, and the sender wants to know
// it actually reached the network before the UI says "sent", so paying
// the replication latency here is the right trade.
func (s *Store) Put(key dht.NodeID, value []byte) error {
	_, err := s.D.Store(key, value)
	return err
}

// Get retrieves a content-addressed value, verified: the DHT checks that
// the returned bytes actually hash to key, so a peer serving tampered
// content is rejected rather than returned.
func (s *Store) Get(key dht.NodeID) ([]byte, error) {
	v, err := s.D.Get(key)
	return v, translate(err)
}

// GetAll retrieves every candidate value at a slot key, unverified --
// the key is derived from identities rather than content, so several
// replicas can legitimately hold different versions. The mailbox layer
// verifies signatures and picks a winner.
func (s *Store) GetAll(key dht.NodeID) ([][]byte, error) {
	v, err := s.D.GetAllUnverified(key)
	return v, translate(err)
}

// translate maps the DHT's not-found sentinel onto the mailbox one, so
// callers can use errors.Is(err, mailbox.ErrNotFound) regardless of
// which Store implementation they hold.
func translate(err error) error {
	if err != nil && errors.Is(err, dht.ErrNotFound) {
		return mailbox.ErrNotFound
	}
	return err
}
