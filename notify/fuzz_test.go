package notify

import (
	"encoding/base64"
	"encoding/json"
	"testing"

	"altnet/core/crypto"
	"altnet/core/peer"

	"altnet-relay/mailbox"
)

// A relay_deliver payload is whatever a remote peer chose to send.
// Decoding must never panic, and whatever it passes on must already be
// within the bounds OpenDelivered assumes.
func FuzzCourierPayload(f *testing.F) {
	id, err := crypto.NewIdentity()
	if err != nil {
		f.Fatal(err)
	}
	data := []byte("sealed")
	key := mailbox.ContentAddress(data).Hex()
	n, err := mailbox.NewNote(id, id.ID(), []string{key}, "", 1)
	if err != nil {
		f.Fatal(err)
	}
	seed, _ := json.Marshal(deliverPayload{Note: n, Chunks: map[string]string{
		key: base64.StdEncoding.EncodeToString(data),
	}})
	f.Add(string(seed))
	f.Add(`{"note":{"chunks":["a"]},"chunks":{"a":"AAAA"}}`)
	f.Add(`{"note":null}`)

	c := &Courier{}
	c.SetOnDirect(func(string, *mailbox.Note, map[string][]byte) {})
	f.Fuzz(func(t *testing.T, payload string) {
		c.HandleMessage(nil, "", peer.Message{Type: MsgDeliver, Payload: payload})

		chunks, note, ok := decodeDelivery(payload)
		if !ok {
			return
		}
		if note == nil || len(chunks) == 0 || len(chunks) > MaxDeliverChunks || len(chunks) > len(note.Chunks) {
			t.Fatalf("out-of-bounds delivery passed: %d chunks, note %v", len(chunks), note)
		}
		referenced := make(map[string]bool)
		for _, k := range note.Chunks {
			referenced[k] = true
		}
		for k, raw := range chunks {
			if !referenced[k] {
				t.Fatalf("chunk %q is not referenced by the note", k)
			}
			if len(raw) > mailbox.ChunkSize {
				t.Fatalf("chunk of %d bytes passed", len(raw))
			}
		}
	})
}
