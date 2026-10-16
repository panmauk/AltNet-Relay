// Command relay is a terminal client for AltNet Relay.
//
// It is a thin prompt over the engine package, which the desktop app
// uses too -- so both clients follow exactly the same rules for requests,
// names, and what "accepted" means.
//
//	relay -data data/alice -bootstrap seed.example:9000
//
// Commands at the prompt:
//
//	claim <handle>         claim a short name people can reach you by
//	whoami                 what to hand out so people can message you
//	add <handle>           add someone by their name
//	add <nick> <who>       add with your own local nickname
//	contacts               your conversations, with unread counts
//	send <who> <text>      send a message
//	read <who>             print a conversation (marks it read)
//	requests               message requests awaiting your decision
//	accept <who>           let a request in (lifts the 3-message limit)
//	decline <who>          turn a request down; it won't come back
//	block <who>            stop hearing from someone; they aren't told
//	unblock <who>          let them back in
//	status                 node and network status
//	help, quit
package main

import (
	"bufio"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"altnet-relay/engine"
	"altnet-relay/mailbox"
	"altnet-relay/state"
)

func main() {
	dataDir := flag.String("data", "data/relay", "directory for identity, keys, and saved conversations")
	listen := flag.String("listen", "0.0.0.0:0", "address for this node to listen on (0 = pick a free port)")
	bootstrap := flag.String("bootstrap", "", "comma-separated AltNet peers to join through (e.g. seed:9000)")
	relays := flag.String("relay", "", "comma-separated relays to register with for NAT traversal")
	operators := flag.String("operators", "", "comma-separated operator public keys whose ban lists to enforce")
	public := flag.Bool("public", false, "this node is directly reachable from the internet")
	quiet := flag.Bool("quiet", true, "suppress the node's own info logging")
	flag.Parse()

	if *quiet {
		slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr,
			&slog.HandlerOptions{Level: slog.LevelError})))
	}

	e, err := engine.Open(engine.Config{
		DataDir:   *dataDir,
		Listen:    *listen,
		Bootstrap: splitList(*bootstrap),
		Relays:    splitList(*relays),
		Operators: splitList(*operators),
		Public:    *public,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "relay: %v\n", err)
		os.Exit(1)
	}
	c := &cli{e: e}
	e.OnMessages = c.onMessages
	e.OnRequest = c.onRequest
	for _, w := range e.Start() {
		fmt.Printf("  ! %s\n", w)
	}
	defer e.Close()

	c.banner()
	c.repl()
}

type cli struct{ e *engine.Engine }

func (c *cli) banner() {
	fmt.Println()
	fmt.Println("  AltNet Relay")
	fmt.Println("  ─────────────────────────────────────────────")
	if h := c.e.MyHandle(); h != "" {
		fmt.Printf("  you are  @%s\n", h)
		fmt.Println("  (hand that to someone so they can message you)")
	} else {
		fmt.Printf("  username  %s\n", c.e.Username())
		fmt.Println(`  claim a short name with "claim <name>" so people`)
		fmt.Println("  don't have to type all of that")
	}
	fmt.Println()
	fmt.Printf("  node listening on %s\n", c.e.Peer.LocalAddr())
	fmt.Println(`  type "help" for commands`)
	fmt.Println()
}

func (c *cli) onMessages(peerID string, msgs []state.StoredMessage) {
	who := c.e.Name(peerID)
	for _, m := range msgs {
		fmt.Printf("\n  %s: %s\n> ", who, m.Text)
	}
}

func (c *cli) onRequest(peerID string) {
	fmt.Printf("\n  * message request from %s\n", c.e.Name(peerID))
	fmt.Printf("    \"accept %s\" to let them in, \"decline %s\" to turn it down\n> ",
		c.e.Name(peerID), c.e.Name(peerID))
}

func (c *cli) repl() {
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sig
		fmt.Println("\n  saving and shutting down…")
		c.e.Close()
		os.Exit(0)
	}()

	sc := bufio.NewScanner(os.Stdin)
	fmt.Print("> ")
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line != "" && !c.command(line) {
			return
		}
		fmt.Print("> ")
	}
}

// command runs one line. Returns false to exit.
func (c *cli) command(line string) bool {
	parts := strings.Fields(line)
	arg := func(i int) string { return parts[i] }
	switch parts[0] {
	case "quit", "exit":
		return false
	case "help":
		c.help()
	case "whoami":
		c.whoami()
	case "status":
		c.status()
	case "contacts":
		c.contacts()
	case "requests":
		c.requests()
	case "claim":
		if len(parts) != 2 {
			fmt.Println("  usage: claim <handle>    e.g. claim rob")
			break
		}
		h, err := c.e.Claim(arg(1))
		if err != nil {
			fmt.Printf("  %v\n", err)
			break
		}
		fmt.Printf("  you are now @%s — that's what you give people\n", h)
	case "add":
		var who, nick string
		switch len(parts) {
		case 2:
			who = arg(1)
		case 3:
			nick, who = arg(1), arg(2)
		default:
			fmt.Println("  usage: add <handle>   or   add <nickname> <handle-or-username>")
			return true
		}
		id, err := c.e.Add(who, nick)
		if err != nil {
			fmt.Printf("  %v\n", err)
			break
		}
		fmt.Printf("  added %s\n", c.e.Name(id))
	case "send":
		if len(parts) < 3 {
			fmt.Println("  usage: send <who> <message>")
			break
		}
		id, ok := c.resolve(arg(1))
		if !ok {
			break
		}
		if _, err := c.e.Send(id, strings.Join(parts[2:], " ")); err != nil {
			fmt.Printf("  not sent — %v\n", err)
			break
		}
		fmt.Printf("  sent to %s\n", c.e.Name(id))
	case "read":
		if len(parts) != 2 {
			fmt.Println("  usage: read <who>")
			break
		}
		if id, ok := c.resolve(arg(1)); ok {
			c.read(id)
		}
	case "accept", "decline":
		if len(parts) != 2 {
			fmt.Printf("  usage: %s <who>\n", parts[0])
			break
		}
		id, ok := c.resolve(arg(1))
		if !ok {
			break
		}
		name := c.e.Name(id)
		if parts[0] == "accept" {
			if err := c.e.Accept(id); err != nil {
				fmt.Printf("  %v\n", err)
				break
			}
			fmt.Printf("  accepted %s — the message limit is lifted\n", c.e.Name(id))
		} else {
			if err := c.e.Decline(id); err != nil {
				fmt.Printf("  %v\n", err)
				break
			}
			fmt.Printf("  declined %s — they won't be told, and it won't come back\n", name)
		}
	case "block", "unblock":
		if len(parts) != 2 {
			fmt.Printf("  usage: %s <who>\n", parts[0])
			break
		}
		// Blocking by full username works for anyone, including someone
		// who hasn't written yet, since resolve accepts 64-character IDs.
		id, ok := c.resolve(arg(1))
		if !ok {
			break
		}
		if parts[0] == "block" {
			if err := c.e.Block(id); err != nil {
				fmt.Printf("  %v\n", err)
				break
			}
			fmt.Printf("  blocked %s — nothing new from them will show up, and they won't be told\n", c.e.Name(id))
		} else {
			if err := c.e.Unblock(id); err != nil {
				fmt.Printf("  %v\n", err)
				break
			}
			fmt.Printf("  unblocked %s — what they send from now on will come through\n", c.e.Name(id))
		}
	default:
		fmt.Printf("  unknown command %q — type \"help\"\n", parts[0])
	}
	return true
}

// resolve turns whatever the user typed -- a nickname, "@handle", or a
// full username -- into a peer ID among known conversations.
func (c *cli) resolve(who string) (string, bool) {
	if id, ok := c.e.State.Resolve(who); ok {
		return id, true
	}
	h := mailbox.CanonicalHandle(who)
	for _, s := range c.e.Conversations() {
		if s.Handle != "" && s.Handle == h {
			return s.PeerID, true
		}
	}
	fmt.Printf("  nobody called %q in your conversations — \"add\" them first\n", who)
	return "", false
}

func (c *cli) help() {
	fmt.Println(`
  claim <handle>         claim a short name people can reach you by
  whoami                 what to hand out so people can message you
  add <handle>           add someone by their name
  add <nick> <who>       add with your own local nickname
  contacts               your conversations, with unread counts
  send <who> <text>      send a message
  read <who>             print a conversation (marks it read)
  requests               message requests awaiting your decision
  accept <who>           let a request in (lifts the 3-message limit)
  decline <who>          turn a request down; it won't come back
  block <who>            stop hearing from someone; they aren't told
  unblock <who>          let them back in
  status                 node and network status
  quit`)
}

func (c *cli) whoami() {
	if h := c.e.MyHandle(); h != "" {
		fmt.Printf("  @%s\n  (username: %s)\n", h, c.e.Username())
		return
	}
	fmt.Printf("  %s\n", c.e.Username())
	fmt.Println(`  no name yet — "claim <name>" to get one people can actually type`)
}

func (c *cli) status() {
	fmt.Printf("  username        %s\n", c.e.Username())
	fmt.Printf("  listening on    %s\n", c.e.Peer.LocalAddr())
	fmt.Printf("  peers connected %d\n", c.e.Peers())
	fmt.Printf("  routing table   %d contacts\n", len(c.e.DHT.RoutingTable().All()))
	fmt.Printf("  conversations   %d\n", len(c.e.Conversations()))
}

func (c *cli) contacts() {
	list := c.e.Conversations()
	if len(list) == 0 {
		fmt.Println(`  no conversations yet — "add <name>" to start one`)
		return
	}
	for _, s := range list {
		mark := " "
		if s.Request {
			mark = "*"
		}
		who := s.PeerID[:16] + "…"
		if s.Handle != "" {
			who = "@" + s.Handle
		}
		note := ""
		switch {
		case s.Banned:
			note = "banned from AltNet"
		case s.Blocked:
			note = "blocked"
		case s.Unread > 0:
			note = fmt.Sprintf("%d unread", s.Unread)
		}
		fmt.Printf("  %s %-16s %-20s %s\n", mark, s.Name, who, note)
	}
}

func (c *cli) requests() {
	shown := 0
	for _, s := range c.e.Conversations() {
		if s.Request {
			fmt.Printf("  %s — %s\n", s.Name, s.Preview)
			shown++
		}
	}
	if shown == 0 {
		fmt.Println("  no pending message requests")
	}
}

func (c *cli) read(peerID string) {
	thread := c.e.Thread(peerID)
	if len(thread) == 0 {
		fmt.Println("  nothing here yet")
		return
	}
	them := c.e.Name(peerID)
	for _, m := range thread {
		who := them
		if m.Outbound {
			who = "you"
		}
		fmt.Printf("  %s  %-12s %s\n", time.Unix(m.Sent, 0).Format("02 Jan 15:04"), who, m.Text)
	}
	for _, s := range c.e.Conversations() {
		if s.PeerID == peerID && s.Request {
			fmt.Printf("\n  (message request — at most %d messages shown until you \"accept %s\")\n",
				mailbox.PendingMessageLimit, them)
		}
		if s.PeerID == peerID && s.Blocked {
			fmt.Printf("\n  (blocked — nothing new from them shows up until you \"unblock %s\")\n", them)
		}
	}
	// Printing it is reading it. Messages that scrolled past at the prompt
	// don't count: they're easy to miss, which is what the count is for.
	if err := c.e.MarkRead(peerID); err != nil {
		fmt.Printf("  ! couldn't save that you've read this: %v\n", err)
	}
}

func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
