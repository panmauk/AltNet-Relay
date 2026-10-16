package mailbox

import (
	"bytes"
	"testing"

	"altnet/core/crypto"
)

func TestMailboxKeyIsDirectionalAndDerivable(t *testing.T) {
	alice, _ := crypto.NewIdentity()
	bob, _ := crypto.NewIdentity()

	k1 := MailboxKey(alice.ID(), bob.ID())
	k2 := MailboxKey(alice.ID(), bob.ID())
	if k1 != k2 {
		t.Fatal("same inputs must derive the same key")
	}

	reverse := MailboxKey(bob.ID(), alice.ID())
	if k1 == reverse {
		t.Fatal("Alice->Bob and Bob->Alice must be different keys")
	}

	if DoorbellKey(bob.ID()) == k1 {
		t.Fatal("doorbell and mailbox keys must not collide")
	}
}

func TestSealOpenRoundTrip(t *testing.T) {
	box, err := NewBoxKey()
	if err != nil {
		t.Fatal(err)
	}
	plaintext := []byte("hey rob, want to grab coffee later?")

	sealed, err := Seal(box.Public, plaintext)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(sealed, plaintext) {
		t.Fatal("ciphertext must not contain the plaintext")
	}

	opened, err := Open(box.Private, sealed)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(opened, plaintext) {
		t.Fatalf("got %q, want %q", opened, plaintext)
	}
}

func TestSealOpenWrongKeyFails(t *testing.T) {
	box, _ := NewBoxKey()
	other, _ := NewBoxKey()
	sealed, err := Seal(box.Public, []byte("secret"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Open(other.Private, sealed); err == nil {
		t.Fatal("expected decryption to fail with the wrong key")
	}
}

func TestNoteSignVerifyAndTamperDetection(t *testing.T) {
	bob, _ := crypto.NewIdentity()
	rob, _ := crypto.NewIdentity()

	n, err := NewNote(bob, rob.ID(), []string{"aabb", "ccdd"}, "", 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := n.Verify(); err != nil {
		t.Fatalf("expected valid signature, got %v", err)
	}
	if n.Sender != crypto.PublicKeyToHex(bob.PublicKey) {
		t.Fatal("Note.Sender must be the actual signer, not the recipient")
	}

	// Verify() only checks the embedded signature is self-consistent --
	// callers must separately check Sender against who they expect.
	if n.Sender == rob.ID() {
		t.Fatal("sanity: sender must not equal recipient's ID")
	}

	// Tamper with the payload after signing: verification must fail.
	n.Chunks = append(n.Chunks, "eeff")
	if err := n.Verify(); err == nil {
		t.Fatal("expected verification to fail after tampering")
	}
}

func TestNoteChainLinksByHash(t *testing.T) {
	bob, _ := crypto.NewIdentity()
	rob, _ := crypto.NewIdentity()

	first, err := NewNote(bob, rob.ID(), []string{"aabb"}, "", 1)
	if err != nil {
		t.Fatal(err)
	}
	second, err := NewNote(bob, rob.ID(), []string{"ccdd"}, first.Hash(), 2)
	if err != nil {
		t.Fatal(err)
	}
	if second.Prev != first.Hash() {
		t.Fatal("second note must chain back to the first note's hash")
	}
	if second.Prev == "" {
		t.Fatal("chained note must not have an empty Prev")
	}
}

func TestKnockAnyoneCanSignButChainMustLink(t *testing.T) {
	bob, _ := crypto.NewIdentity()
	hob, _ := crypto.NewIdentity()
	rob, _ := crypto.NewIdentity()
	bobBox, _ := NewBoxKey()
	hobBox, _ := NewBoxKey()

	bobIntro, _ := NewNote(bob, rob.ID(), []string{"aabb"}, "", 1)
	k1, err := NewKnock(bob, bobBox, rob.ID(), bobIntro.Hash(), "")
	if err != nil {
		t.Fatal(err)
	}
	if err := k1.Verify(); err != nil {
		t.Fatalf("bob's knock should verify: %v", err)
	}

	// A completely different, unrelated sender can knock on the same
	// doorbell, chaining after bob's knock.
	hobIntro, _ := NewNote(hob, rob.ID(), []string{"1122"}, "", 1)
	k2, err := NewKnock(hob, hobBox, rob.ID(), hobIntro.Hash(), k1.Hash())
	if err != nil {
		t.Fatal(err)
	}
	if err := k2.Verify(); err != nil {
		t.Fatalf("hob's knock should verify: %v", err)
	}
	if k2.Prev != k1.Hash() {
		t.Fatal("hob's knock must chain to bob's knock as the doorbell tip")
	}
	if k2.Sender == k1.Sender {
		t.Fatal("bob and hob must be different signers")
	}
}
