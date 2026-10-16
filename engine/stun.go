package engine

import (
	"bytes"
	"crypto/rand"
	"encoding/binary"
	"net"
	"strings"
	"time"

	"altnet-relay/account"
)

// FallbackSTUN is used for calls only while AltNet's own STUN/TURN server
// can't be reached (for example before its ports are opened). STUN is a
// mirror: a computer asks "what address do you see me at?" and gets one
// line back. The service learns that this computer asked -- not who is being
// called, nor anything said -- and without an answer from someone, two people
// on different home networks generally can't connect a call at all.
//
// As soon as the operator's own server answers again, it is used instead and
// this one is never contacted.
var FallbackSTUN = []account.IceServer{{URLs: []string{"stun:stun.cloudflare.com:3478"}}}

const (
	stunProbeTimeout = 1500 * time.Millisecond
	stunGoodFor      = 10 * time.Minute // a working server is rechecked this often
	stunBadFor       = 2 * time.Minute  // a silent one is retried sooner
)

// ownSTUNReachable reports whether the operator's STUN server answers, from
// a cached probe. The first call probes (one UDP round trip, at most
// stunProbeTimeout); the window warms this when a conversation opens, so a
// call normally finds the answer ready.
func (e *Engine) ownSTUNReachable() bool {
	e.callMu.Lock()
	ok, until := e.stunOK, e.stunUntil
	e.callMu.Unlock()
	if time.Now().Before(until) {
		return ok
	}
	ok = false
	for _, s := range DefaultSTUN {
		for _, u := range s.URLs {
			if strings.HasPrefix(u, "stun:") && probeSTUN(strings.TrimPrefix(u, "stun:"), stunProbeTimeout) {
				ok = true
			}
		}
	}
	good := stunBadFor
	if ok {
		good = stunGoodFor
	}
	e.callMu.Lock()
	e.stunOK, e.stunUntil = ok, time.Now().Add(good)
	e.callMu.Unlock()
	return ok
}

// probeSTUN sends one STUN Binding Request (RFC 5389) and reports whether a
// matching success response comes back in time.
func probeSTUN(addr string, timeout time.Duration) bool {
	conn, err := net.DialTimeout("udp", addr, timeout)
	if err != nil {
		return false
	}
	defer conn.Close()
	var txid [12]byte
	if _, err := rand.Read(txid[:]); err != nil {
		return false
	}
	req := make([]byte, 20)
	binary.BigEndian.PutUint16(req[0:], 0x0001) // Binding Request
	binary.BigEndian.PutUint16(req[2:], 0)      // no attributes
	binary.BigEndian.PutUint32(req[4:], 0x2112A442)
	copy(req[8:], txid[:])
	deadline := time.Now().Add(timeout)
	_ = conn.SetDeadline(deadline)
	// UDP can drop a packet; ask twice within the time allowed.
	for try := 0; try < 2 && time.Now().Before(deadline); try++ {
		if _, err := conn.Write(req); err != nil {
			return false
		}
		_ = conn.SetReadDeadline(time.Now().Add(timeout / 2))
		buf := make([]byte, 512)
		n, err := conn.Read(buf)
		if err != nil {
			continue
		}
		if n >= 20 && binary.BigEndian.Uint16(buf[0:]) == 0x0101 &&
			binary.BigEndian.Uint32(buf[4:]) == 0x2112A442 && bytes.Equal(buf[8:20], txid[:]) {
			return true
		}
	}
	return false
}
