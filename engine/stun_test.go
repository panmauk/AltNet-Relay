package engine

import (
	"encoding/binary"
	"net"
	"testing"
	"time"

	"altnet-relay/account"
)

// fakeSTUN answers Binding Requests the way a real STUN server does (minus
// the address attribute, which nothing here reads).
func fakeSTUN(t *testing.T) string {
	t.Helper()
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pc.Close() })
	go func() {
		buf := make([]byte, 512)
		for {
			n, from, err := pc.ReadFrom(buf)
			if err != nil {
				return
			}
			if n < 20 || binary.BigEndian.Uint16(buf[0:]) != 0x0001 {
				continue
			}
			resp := make([]byte, 20)
			binary.BigEndian.PutUint16(resp[0:], 0x0101)
			binary.BigEndian.PutUint32(resp[4:], 0x2112A442)
			copy(resp[8:], buf[8:20])
			pc.WriteTo(resp, from)
		}
	}()
	return pc.LocalAddr().String()
}

// deadUDP returns a local address nothing answers on.
func deadUDP(t *testing.T) string {
	t.Helper()
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := pc.LocalAddr().String()
	pc.Close()
	return addr
}

func TestSTUNProbe(t *testing.T) {
	if !probeSTUN(fakeSTUN(t), time.Second) {
		t.Fatal("a server that answers must be seen as reachable")
	}
	start := time.Now()
	if probeSTUN(deadUDP(t), 600*time.Millisecond) {
		t.Fatal("a silent address must not count as reachable")
	}
	if time.Since(start) > 1500*time.Millisecond {
		t.Fatal("the probe must give up within its time limit")
	}
}

// While the operator's server can't be reached, calls use the fallback (and
// don't offer the unreachable relay); once it answers, only it is used.
func TestCallServersFallBackOnlyWhileOwnServerIsSilent(t *testing.T) {
	saved := DefaultSTUN
	t.Cleanup(func() { DefaultSTUN = saved })
	e := open(t, t.TempDir())
	defer e.Close()

	DefaultSTUN = []account.IceServer{{URLs: []string{"stun:" + deadUDP(t)}}}
	got := e.CallServers()
	if len(got) != 1 || got[0].URLs[0] != FallbackSTUN[0].URLs[0] {
		t.Fatalf("with our server silent, expected the fallback only, got %+v", got)
	}

	live := "stun:" + fakeSTUN(t)
	DefaultSTUN = []account.IceServer{{URLs: []string{live}}}
	e.callMu.Lock()
	e.stunUntil = time.Time{} // forget the cached "silent"
	e.callMu.Unlock()
	got = e.CallServers()
	for _, s := range got {
		for _, u := range s.URLs {
			if u == FallbackSTUN[0].URLs[0] {
				t.Fatal("once our server answers, the fallback must not be contacted")
			}
		}
	}
	if len(got) == 0 || got[0].URLs[0] != live {
		t.Fatalf("expected our own server, got %+v", got)
	}
}
