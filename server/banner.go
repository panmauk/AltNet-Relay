package server

import (
	"errors"
	"fmt"
	"os"
	"sync"

	"altnet/core/crypto"

	"altnet-relay/mailbox"
	"altnet-relay/moderation"
)

// OperatorBanner publishes bans through moderation.Operator, keeping the
// operator's own saved copy of the list so a network that momentarily
// returns nothing can never make the list look empty and overwrite it.
type OperatorBanner struct {
	mu   sync.Mutex
	op   *moderation.Operator
	path string
}

// NewOperatorBanner builds a Banner that publishes through s.
func NewOperatorBanner(s mailbox.Store, id *crypto.Identity, statePath string) (*OperatorBanner, error) {
	op := moderation.NewOperator(s, id)
	saved, err := os.ReadFile(statePath)
	switch {
	case errors.Is(err, os.ErrNotExist):
	case err != nil:
		return nil, err
	default:
		if err := op.Import(saved); err != nil {
			return nil, fmt.Errorf("saved ban list %s: %w", statePath, err)
		}
	}
	return &OperatorBanner{op: op, path: statePath}, nil
}

func (b *OperatorBanner) Ban(username, reason string) error {
	return b.edit(func() error {
		err := b.op.Ban(username, reason)
		if errors.Is(err, moderation.ErrAlreadyBanned) {
			return nil // banning someone already banned is not a failure
		}
		return err
	})
}

func (b *OperatorBanner) Unban(username string) error {
	return b.edit(func() error {
		err := b.op.Unban(username)
		if errors.Is(err, moderation.ErrNotBanned) {
			return nil
		}
		return err
	})
}

func (b *OperatorBanner) edit(fn func() error) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	// Build on whatever is currently published, then publish the change.
	if err := b.op.Load(); err != nil {
		return err
	}
	if err := fn(); err != nil {
		return err
	}
	blob, err := b.op.Export()
	if err != nil {
		return err
	}
	tmp := b.path + ".tmp"
	if err := os.WriteFile(tmp, blob, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, b.path)
}

// Republish stores the current ban list on the network again. Lists live on
// ordinary nodes, which come and go, so the operator re-asserts the list now
// and then; it does nothing if no one has ever been banned.
func (b *OperatorBanner) Republish() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if _, ok := b.op.Info(); !ok {
		return nil
	}
	return b.op.Republish()
}
