package mailbox

import (
	"fmt"

	"altnet/core/dht"
)

// ChunkSize matches apps/files.ChunkSize so Relay content moves through
// the network in the same size units as everything else on AltNet.
// dht.MaxValueSize is also 64 KiB, and the check there is a strict
// greater-than, so a full chunk fits exactly.
const ChunkSize = 64 * 1024

// MaxChunksPerMessage bounds how many chunks one Note may reference, so
// a malicious Note can't make a recipient issue unbounded DHT lookups.
// 16 chunks x 64 KiB = 1 MiB per message, well above any text message
// and a sane ceiling for the first version.
const MaxChunksPerMessage = 16

// Chunk is one content-addressed piece of a sealed message.
type Chunk struct {
	Key  dht.NodeID
	Data []byte
}

// SplitChunks cuts an already-sealed blob into content-addressed pieces.
//
// Order matters: we seal the whole message first and chunk the
// ciphertext, never the other way round. Sealing per chunk would add
// this package's 60-byte header to each piece (pushing a full chunk over
// dht.MaxValueSize) and would leak the message's structure through the
// piece boundaries. One seal, then a dumb byte split, is both simpler
// and tighter.
func SplitChunks(sealed []byte) ([]Chunk, error) {
	if len(sealed) == 0 {
		return nil, fmt.Errorf("mailbox: nothing to chunk")
	}
	n := (len(sealed) + ChunkSize - 1) / ChunkSize
	if n > MaxChunksPerMessage {
		return nil, fmt.Errorf("mailbox: message needs %d chunks (max %d)", n, MaxChunksPerMessage)
	}
	out := make([]Chunk, 0, n)
	for start := 0; start < len(sealed); start += ChunkSize {
		end := start + ChunkSize
		if end > len(sealed) {
			end = len(sealed)
		}
		piece := append([]byte(nil), sealed[start:end]...)
		out = append(out, Chunk{Key: ContentAddress(piece), Data: piece})
	}
	return out, nil
}

// JoinChunks concatenates fetched pieces back into the sealed blob.
// The caller is responsible for passing them in the Note's stated order;
// each piece was verified against its own hash on retrieval, and the
// AEAD tag on the reassembled blob catches any mis-ordering.
func JoinChunks(pieces [][]byte) []byte {
	total := 0
	for _, p := range pieces {
		total += len(p)
	}
	out := make([]byte, 0, total)
	for _, p := range pieces {
		out = append(out, p...)
	}
	return out
}
