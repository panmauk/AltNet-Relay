package mailbox

import (
	"crypto/sha256"
	"errors"
	"sync"

	"altnet/core/dht"
)

// ErrNotFound means no value was found at a key. Mirrors dht.ErrNotFound;
// the adapter that wires this package to a live DHT translates between
// the two.
var ErrNotFound = errors.New("mailbox: not found")

// Store is the storage substrate this package needs. It is deliberately
// tiny and maps 1:1 onto the methods a live *dht.DHT already exposes
// (Store / Get / GetAllUnverified), so the network adapter is a thin
// shim and every flow in this package stays testable offline against
// MemStore.
//
// Two retrieval styles, matching the two kinds of record we store:
//
//   - Get is for content-addressed values (chunks, and the archived
//     copy of each Note/Knock) where key == sha256(value), so a
//     tampered value is detectable by the caller.
//   - GetAll is for slot values (a mailbox tip, a doorbell tip) whose
//     key is derived from identities rather than content. Different
//     replicas can legitimately hold different versions, so every
//     candidate is returned and the caller picks the best one after
//     verifying signatures -- the same shape as core/name's Resolve.
type Store interface {
	Put(key dht.NodeID, value []byte) error
	Get(key dht.NodeID) ([]byte, error)
	GetAll(key dht.NodeID) ([][]byte, error)
}

// ContentAddress returns the DHT key for a content-addressed value.
// Same derivation apps/files uses, restated here so this package
// doesn't depend on the files layer just for one hash.
func ContentAddress(value []byte) dht.NodeID {
	var id dht.NodeID
	sum := sha256.Sum256(value)
	copy(id[:], sum[:])
	return id
}

// MemStore is an in-memory Store for tests and offline development.
//
// It keeps every distinct value ever written at a key rather than
// overwriting, which deliberately models the awkward part of real DHT
// behaviour: several replicas holding several versions of a mutable
// slot at once. Code that picks a tip has to survive that here, so it
// will survive it on the wire too.
type MemStore struct {
	mu     sync.RWMutex
	values map[dht.NodeID][][]byte
}

// NewMemStore returns an empty in-memory store.
func NewMemStore() *MemStore {
	return &MemStore{values: make(map[dht.NodeID][][]byte)}
}

// Put appends value at key, ignoring exact duplicates.
func (m *MemStore) Put(key dht.NodeID, value []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	cp := append([]byte(nil), value...)
	for _, existing := range m.values[key] {
		if string(existing) == string(cp) {
			return nil
		}
	}
	m.values[key] = append(m.values[key], cp)
	return nil
}

// Get returns the value at key whose hash actually equals key. Anything
// else stored there is ignored, so a peer that planted junk at a
// content address can't poison the read.
func (m *MemStore) Get(key dht.NodeID) ([]byte, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, v := range m.values[key] {
		if ContentAddress(v) == key {
			return append([]byte(nil), v...), nil
		}
	}
	return nil, ErrNotFound
}

// GetAll returns every candidate value stored at key.
func (m *MemStore) GetAll(key dht.NodeID) ([][]byte, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	vals := m.values[key]
	if len(vals) == 0 {
		return nil, ErrNotFound
	}
	out := make([][]byte, 0, len(vals))
	for _, v := range vals {
		out = append(out, append([]byte(nil), v...))
	}
	return out, nil
}
