package notify

import (
	"encoding/base64"
	"encoding/json"
	"testing"
	"time"

	"altnet/core/crypto"

	"altnet-relay/mailbox"
)

// sealSignal builds a signal payload the way Send does, with every field
// open to tampering.
func sealSignal(t *testing.T, to *node, signer *crypto.Identity, claimedFrom string, sig Signal) string {
	t.Helper()
	body, _ := json.Marshal(sig)
	inner, _ := json.Marshal(signalInner{
		From: claimedFrom,
		PK:   crypto.PublicKeyToHex(signer.PublicKey),
		Body: body,
		Sig:  signer.Sign(append([]byte(signalDomain), body...)),
	})
	sealed, err := mailbox.Seal(to.client.Box.Public, inner)
	if err != nil {
		t.Fatal(err)
	}
	return base64.StdEncoding.EncodeToString(sealed)
}

func TestSignalsAreAuthenticatedAndAddressed(t *testing.T) {
	bob, rob := newNode(t), newNode(t)
	robSignals := NewSignals(rob.peer, rob.client)
	bobKey := bob.client.ID
	robName := rob.id.Username()
	now := time.Now().Unix()

	good := Signal{Kind: SignalTyping, To: robName, TS: now}
	if from, s, err := robSignals.open(sealSignal(t, rob, bobKey, bobKey.ID(), good)); err != nil || from != bobKey.ID() || s.Kind != SignalTyping {
		t.Fatalf("a genuine signal must open: %q %+v %v", from, s, err)
	}

	stranger, _ := crypto.NewIdentity()
	cases := map[string]string{
		"addressed to someone else": sealSignal(t, rob, bobKey, bobKey.ID(), Signal{Kind: SignalTyping, To: bobKey.ID(), TS: now}),
		"claims to be someone else": sealSignal(t, rob, stranger, bobKey.ID(), good),
		"stale":                     sealSignal(t, rob, bobKey, bobKey.ID(), Signal{Kind: SignalTyping, To: robName, TS: now - 600}),
		"from the future":           sealSignal(t, rob, bobKey, bobKey.ID(), Signal{Kind: SignalTyping, To: robName, TS: now + 600}),
		"not base64":                "%%%",
		"sealed to someone else":    sealSignal(t, bob, bobKey, bobKey.ID(), good),
	}
	for name, payload := range cases {
		if _, _, err := robSignals.open(payload); err == nil {
			t.Errorf("%s: must be rejected", name)
		}
	}

	// A signature over different contents doesn't carry over.
	body, _ := json.Marshal(good)
	other, _ := json.Marshal(Signal{Kind: SignalDelivered, IDs: []string{"x"}, To: robName, TS: now})
	inner, _ := json.Marshal(signalInner{From: bobKey.ID(), PK: crypto.PublicKeyToHex(bobKey.PublicKey),
		Body: other, Sig: bobKey.Sign(append([]byte(signalDomain), body...))})
	sealed, _ := mailbox.Seal(rob.client.Box.Public, inner)
	if _, _, err := robSignals.open(base64.StdEncoding.EncodeToString(sealed)); err == nil {
		t.Error("a signature must cover exactly what it signs")
	}
}
