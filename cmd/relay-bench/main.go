// Command relay-bench measures how long a message takes to get from one
// Relay client to another over a real network: the time Send takes on the
// sender, and the time until the message appears on the recipient.
//
// It starts two throwaway clients on this machine (fresh identities in a
// temporary folder, deleted afterwards), joins them through the given seed
// and relay, makes them contacts, and bounces messages between them. Both
// are behind this computer's router, so everything between them goes
// through the seed's relay -- the same path two people at home take.
//
//	relay-bench                      # the live seed
//	relay-bench -n 20
package main

import (
	"flag"
	"fmt"
	"log/slog"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"altnet-relay/engine"
	"altnet-relay/state"
)

func main() {
	n := flag.Int("n", 10, "messages each way")
	boot := flag.String("bootstrap", strings.Join(engine.DefaultBootstrap, ","), "peers to join through")
	relay := flag.String("relay", strings.Join(engine.DefaultRelays, ","), "relays to register with")
	gap := flag.Duration("gap", 400*time.Millisecond, "pause between messages")
	flag.Parse()
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})))

	tmp, err := os.MkdirTemp("", "relay-bench-")
	if err != nil {
		fail(err)
	}
	defer os.RemoveAll(tmp)

	start := func(name string) *engine.Engine {
		e, err := engine.Open(engine.Config{
			DataDir: tmp + "/" + name, Listen: "0.0.0.0:0",
			Bootstrap: split(*boot), Relays: split(*relay),
		})
		if err != nil {
			fail(err)
		}
		return e
	}
	a, b := start("a"), start("b")
	defer a.Close()
	defer b.Close()

	var mu sync.Mutex
	arrived := map[string]time.Time{}
	record := func(_ string, msgs []state.StoredMessage) {
		mu.Lock()
		defer mu.Unlock()
		for _, m := range msgs {
			arrived[m.Text] = time.Now()
		}
	}
	a.OnMessages, b.OnMessages = record, record
	for _, w := range append(a.Start(), b.Start()...) {
		fmt.Println("  !", w)
	}
	time.Sleep(3 * time.Second) // let relay registration and routing settle

	bID, err := a.Add(b.Username(), "b")
	if err != nil {
		fail(fmt.Errorf("a adds b: %w", err))
	}
	aID, err := b.Add(a.Username(), "a")
	if err != nil {
		fail(fmt.Errorf("b adds a: %w", err))
	}

	// Warm up: the first message in each direction also looks up the
	// other's profile and learns their direct address.
	send(a, bID, "warmup-a")
	send(b, aID, "warmup-b")
	waitFor(&mu, arrived, "warmup-a", 90*time.Second)
	waitFor(&mu, arrived, "warmup-b", 90*time.Second)

	var sendDur, deliver []time.Duration
	for i := 0; i < *n; i++ {
		for _, dir := range []struct {
			from *engine.Engine
			to   string
			tag  string
		}{{a, bID, "ab"}, {b, aID, "ba"}} {
			text := fmt.Sprintf("bench-%s-%d-%d", dir.tag, i, time.Now().UnixNano())
			t0 := time.Now()
			send(dir.from, dir.to, text)
			sendDur = append(sendDur, time.Since(t0))
			at := waitFor(&mu, arrived, text, 90*time.Second)
			deliver = append(deliver, at.Sub(t0))
			time.Sleep(*gap)
		}
	}
	fmt.Printf("messages: %d\n", len(deliver))
	fmt.Printf("send call returns:   %s\n", summary(sendDur))
	fmt.Printf("appears on the other: %s\n", summary(deliver))
}

func send(e *engine.Engine, to, text string) {
	if _, err := e.Send(to, text); err != nil {
		fail(fmt.Errorf("send %q: %w", text, err))
	}
}

func waitFor(mu *sync.Mutex, arrived map[string]time.Time, text string, limit time.Duration) time.Time {
	deadline := time.Now().Add(limit)
	for time.Now().Before(deadline) {
		mu.Lock()
		at, ok := arrived[text]
		mu.Unlock()
		if ok {
			return at
		}
		time.Sleep(5 * time.Millisecond)
	}
	fail(fmt.Errorf("%q never arrived", text))
	return time.Time{}
}

func summary(ds []time.Duration) string {
	s := append([]time.Duration(nil), ds...)
	sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
	pct := func(p float64) time.Duration { return s[int(p*float64(len(s)-1))] }
	return fmt.Sprintf("median %v, p90 %v, best %v, worst %v",
		pct(0.5).Round(time.Millisecond), pct(0.9).Round(time.Millisecond),
		s[0].Round(time.Millisecond), s[len(s)-1].Round(time.Millisecond))
}

func split(s string) []string {
	var out []string
	for _, v := range strings.Split(s, ",") {
		if v = strings.TrimSpace(v); v != "" {
			out = append(out, v)
		}
	}
	return out
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "relay-bench:", err)
	os.Exit(1)
}
