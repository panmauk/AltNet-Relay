package mailbox

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
)

// Seal encrypts plaintext for recipientPub using a fresh ephemeral X25519
// key, so the sender doesn't need the recipient to be online -- the
// asynchronous analogue of core/secure's live handshake. Anyone holding
// the returned bytes without the matching private key learns nothing
// about plaintext, not even its length beyond rounding to the nearest
// byte.
//
// Wire format: [32B ephemeral X25519 pubkey][12B GCM nonce][ciphertext+16B tag]
func Seal(recipientPub *ecdh.PublicKey, plaintext []byte) ([]byte, error) {
	curve := ecdh.X25519()
	ephPriv, err := curve.GenerateKey(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("mailbox: generate ephemeral: %w", err)
	}
	shared, err := ephPriv.ECDH(recipientPub)
	if err != nil {
		return nil, fmt.Errorf("mailbox: ecdh: %w", err)
	}
	ephPub := ephPriv.PublicKey().Bytes()
	key := sealKDF(shared, ephPub, recipientPub.Bytes())

	gcm, err := newGCM(key)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("mailbox: nonce: %w", err)
	}
	ct := gcm.Seal(nil, nonce, plaintext, nil)

	out := make([]byte, 0, len(ephPub)+len(nonce)+len(ct))
	out = append(out, ephPub...)
	out = append(out, nonce...)
	out = append(out, ct...)
	return out, nil
}

// Open reverses Seal using the recipient's own stable private key.
func Open(recipientPriv *ecdh.PrivateKey, sealed []byte) ([]byte, error) {
	curve := ecdh.X25519()
	const ephLen, nonceLen = 32, 12
	if len(sealed) < ephLen+nonceLen {
		return nil, errors.New("mailbox: sealed message too short")
	}
	ephPubBytes := sealed[:ephLen]
	nonce := sealed[ephLen : ephLen+nonceLen]
	ct := sealed[ephLen+nonceLen:]

	ephPub, err := curve.NewPublicKey(ephPubBytes)
	if err != nil {
		return nil, fmt.Errorf("mailbox: bad ephemeral key: %w", err)
	}
	shared, err := recipientPriv.ECDH(ephPub)
	if err != nil {
		return nil, fmt.Errorf("mailbox: ecdh: %w", err)
	}
	key := sealKDF(shared, ephPubBytes, recipientPriv.PublicKey().Bytes())

	gcm, err := newGCM(key)
	if err != nil {
		return nil, err
	}
	pt, err := gcm.Open(nil, nonce, ct, nil)
	if err != nil {
		return nil, fmt.Errorf("mailbox: decrypt failed: %w", err)
	}
	return pt, nil
}

func newGCM(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

// sealKDF derives the AES-256 key from the ECDH shared secret, binding
// in both public keys (transcript) so a passive observer can't reuse
// derived material across different (ephemeral, recipient) pairings.
func sealKDF(shared, ephPub, recipientPub []byte) []byte {
	salt := append(append([]byte{}, ephPub...), recipientPub...)
	return hkdf(shared, salt, []byte("altnet-relay-seal-v1"), 32)
}

// hkdf (RFC 5869, over SHA-256) is a small local copy of the same
// construction core/secure uses -- kept private to this package because
// the domain-separation tag is specific to mailbox sealing, not worth
// promoting to a shared exported helper for one call site.
func hkdf(ikm, salt, info []byte, outLen int) []byte {
	if salt == nil {
		salt = make([]byte, sha256.Size)
	}
	mac := hmac.New(sha256.New, salt)
	mac.Write(ikm)
	prk := mac.Sum(nil)

	var out []byte
	var prev []byte
	for i := byte(1); len(out) < outLen; i++ {
		mac := hmac.New(sha256.New, prk)
		mac.Write(prev)
		mac.Write(info)
		mac.Write([]byte{i})
		prev = mac.Sum(nil)
		out = append(out, prev...)
	}
	return out[:outLen]
}
