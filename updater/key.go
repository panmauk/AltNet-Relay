package updater

import (
	"crypto/ed25519"
	"encoding/hex"
)

// ReleaseKeyHex is the public half of the release key. Only notices signed by
// its private half (kept offline by the operator) are believed.
const ReleaseKeyHex = "ab68347630d86f1f78c9aa27554e340cfe3998ba38c25560ed3dcc7bed30f2f6"

// ReleaseKey returns the release key built into the app.
func ReleaseKey() ed25519.PublicKey {
	b, err := hex.DecodeString(ReleaseKeyHex)
	if err != nil || len(b) != ed25519.PublicKeySize {
		return nil
	}
	return ed25519.PublicKey(b)
}
