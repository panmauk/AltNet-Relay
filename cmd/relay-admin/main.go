// Command relay-admin is the operator's tool for Relay moderation: it
// makes an operator key, bans and unbans identities, assigns handles in
// authority mode, and reads the published ban lists.
//
// It is deliberately not a daemon. Each command that touches the network
// starts a throwaway AltNet node, joins through -bootstrap, publishes or
// reads, and exits. The node's transport key is generated fresh and never
// saved: the only long-lived secret is the operator key, and it signs
// records, never traffic.
//
//	relay-admin keygen -out operator.key
//	relay-admin ban -key operator.key -bootstrap host:port [-reason tos-4.1] <target>
//	relay-admin unban -key operator.key -bootstrap host:port <target>
//	relay-admin republish -key operator.key -bootstrap host:port
//	relay-admin sign-handle -key operator.key -bootstrap host:port <handle> <username>
//	relay-admin list -bootstrap host:port (-operator <hex,...> | -key operator.key)
//
// The commands above use the operator key directly. The everyday ones --
// login, reports, decide, ban-account, unban-account -- go through
// relay-server instead; see remote.go.
//
// A <target> is a username (64 hex), "pk:<hex>" for an identity key, or
// an @handle. Flags go before positional arguments.
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"altnet-relay/dhtstore"
	"altnet-relay/mailbox"
	"altnet-relay/moderation"
	"altnet/core/crypto"
	"altnet/core/dht"
	"altnet/core/peer"
)

func main() {
	if len(os.Args) < 2 {
		usage(os.Stderr)
		os.Exit(2)
	}
	cmd, args := os.Args[1], os.Args[2:]
	var err error
	switch cmd {
	case "keygen":
		err = cmdKeygen(args, os.Stdout)
	case "ban":
		err = cmdEdit(args, true)
	case "unban":
		err = cmdEdit(args, false)
	case "republish":
		err = cmdRepublish(args)
	case "sign-handle":
		err = cmdSignHandle(args)
	case "list":
		err = cmdList(args)
	case "login":
		err = cmdLogin(args)
	case "reports":
		err = cmdReports(args)
	case "decide":
		err = cmdDecide(args)
	case "ban-account":
		err = cmdBanAccount(args)
	case "unban-account":
		err = cmdUnbanAccount(args)
	case "unbind-account":
		err = cmdUnbindAccount(args)
	case "help", "-h", "-help", "--help":
		usage(os.Stdout)
		return
	default:
		fmt.Fprintf(os.Stderr, "relay-admin: unknown command %q\n", cmd)
		usage(os.Stderr)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "relay-admin:", err)
		os.Exit(1)
	}
}

func usage(w io.Writer) {
	fmt.Fprint(w, `usage: relay-admin <command> [flags] [args]

  keygen      -out <file>                                 make an operator key; prints its public key
  ban         -key <file> -bootstrap <addr> [-reason ..] <target>
  unban       -key <file> -bootstrap <addr> <target>
  republish   -key <file> -bootstrap <addr>                store the current list again
  sign-handle -key <file> -bootstrap <addr> <handle> <username>
  list        -bootstrap <addr> (-operator <hex,...> | -key <file>)

Day to day, through the server (no key file needed):
  login         -email <you>                              prints an admin session token
  reports       [-status pending|banned|dismissed|all]    (three different reporters ban automatically; pending = fewer than that)
  decide        <report-id> ban|dismiss [note...]
  ban-account   (-email <e> | -username <hex>) <note...>
  unban-account <username>                                lifts a ban and clears the reports made before it
  unbind-account <email>                                  move an account to a new computer
  (these take -server and -token, or RELAY_ADMIN_TOKEN)

<target> is a username (64 hex), pk:<hex> for an identity key, or @handle.
Flags go before arguments. Run "relay-admin <command> -h" for details.
`)
}

// cmdKeygen writes a new operator key and prints ONLY its public key: the
// private half goes to the file and nowhere else -- not stdout, not a log.
//
// O_EXCL makes "refuse to overwrite" atomic rather than a check followed
// by a write: an operator key that is silently replaced orphans every
// list and handle it signed. Mode 0600 is honoured on Unix; on Windows
// the file inherits its directory's ACL, so keep it somewhere private.
func cmdKeygen(args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("keygen", flag.ContinueOnError)
	out := fs.String("out", "", "file to write the new operator private key to (must not exist)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *out == "" || fs.NArg() != 0 {
		return errors.New("keygen: -out <file> is required and takes no arguments")
	}
	id, err := crypto.NewIdentity()
	if err != nil {
		return err
	}
	f, err := os.OpenFile(*out, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return fmt.Errorf("keygen: %s already exists; refusing to overwrite an operator key", *out)
		}
		return fmt.Errorf("keygen: %w", err)
	}
	// Same encoding crypto.Save writes, so crypto.Load reads it back.
	_, werr := io.WriteString(f, fmt.Sprintf("%x", []byte(id.PrivateKey)))
	cerr := f.Close()
	if werr != nil || cerr != nil {
		os.Remove(*out)
		return fmt.Errorf("keygen: write %s: %v", *out, errors.Join(werr, cerr))
	}
	fmt.Fprintln(stdout, crypto.PublicKeyToHex(id.PublicKey))
	return nil
}

// netFlags are shared by every command that joins the network.
type netFlags struct {
	bootstrap *string
	listen    *string
	verbose   *bool
}

func addNetFlags(fs *flag.FlagSet) netFlags {
	return netFlags{
		bootstrap: fs.String("bootstrap", "", "comma-separated AltNet peers to join through (required)"),
		listen:    fs.String("listen", "0.0.0.0:0", "address for the temporary node to listen on"),
		verbose:   fs.Bool("verbose", false, "show the node's own logging"),
	}
}

// join starts the temporary node. It fails unless at least one bootstrap
// peer answered: publishing into an empty routing table would store the
// value only on this node, which is about to exit.
func (nf netFlags) join() (*dhtstore.Store, func(), error) {
	if !*nf.verbose {
		slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})))
	}
	boots := splitList(*nf.bootstrap)
	if len(boots) == 0 {
		return nil, nil, errors.New("-bootstrap is required")
	}
	key, err := crypto.NewIdentity() // transport identity: throwaway, never saved
	if err != nil {
		return nil, nil, err
	}
	p := peer.New(key, *nf.listen)
	if err := p.Start(); err != nil {
		return nil, nil, fmt.Errorf("start node: %w", err)
	}
	d, err := dht.New(p)
	if err != nil {
		p.Stop()
		return nil, nil, fmt.Errorf("start dht: %w", err)
	}
	joined := 0
	for _, b := range boots {
		if err := d.Bootstrap(b); err != nil {
			fmt.Fprintf(os.Stderr, "warning: could not reach %s: %v\n", b, err)
			continue
		}
		joined++
	}
	if joined == 0 {
		p.Stop()
		return nil, nil, errors.New("could not join the network through any bootstrap peer")
	}
	return dhtstore.New(d), p.Stop, nil
}

// replicated is a Store whose Put fails unless some other node accepted
// the value. dhtstore.Put reports only oversize errors -- a store that
// reached nobody still "succeeds", keeping the value on the local node --
// which is right for a client that stays online and republishes, and
// wrong for a tool that exits a second later taking that copy with it.
type replicated struct {
	*dhtstore.Store
	fewest int // smallest replica count of any Put, for the report
}

func (s *replicated) Put(key dht.NodeID, value []byte) error {
	n, err := s.D.Store(key, value)
	if err != nil {
		return err
	}
	if n == 0 {
		return errors.New("no peer accepted the value, so nothing was published")
	}
	if s.fewest == 0 || n < s.fewest {
		s.fewest = n
	}
	return nil
}

// operatorFlags are shared by the commands that sign with the operator key.
type operatorFlags struct {
	key   *string
	state *string
}

func addOperatorFlags(fs *flag.FlagSet) operatorFlags {
	return operatorFlags{
		key:   fs.String("key", "", "operator private key file (required)"),
		state: fs.String("state", "", "the operator's saved copy of its list (default: <key>.banlist)"),
	}
}

func (of operatorFlags) load() (*crypto.Identity, string, error) {
	if *of.key == "" {
		return nil, "", errors.New("-key is required")
	}
	id, err := crypto.Load(*of.key)
	if err != nil {
		return nil, "", err
	}
	state := *of.state
	if state == "" {
		state = *of.key + ".banlist"
	}
	return id, state, nil
}

// openOperator builds the operator and restores its saved copy. The copy
// is what keeps an edit from being built on an empty list when the
// network fails to return the published one -- see moderation.Operator --
// so a copy that exists but can't be read is fatal, not skipped.
func openOperator(s mailbox.Store, id *crypto.Identity, statePath string) (*moderation.Operator, error) {
	op := moderation.NewOperator(s, id)
	saved, err := os.ReadFile(statePath)
	switch {
	case errors.Is(err, os.ErrNotExist):
	case err != nil:
		return nil, err
	default:
		if err := op.Import(saved); err != nil {
			return nil, fmt.Errorf("saved copy %s: %w", statePath, err)
		}
	}
	return op, nil
}

func saveOperator(op *moderation.Operator, statePath string) error {
	b, err := op.Export()
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(statePath), ".banlist-*")
	if err != nil {
		return err
	}
	_, werr := tmp.Write(b)
	cerr := tmp.Close()
	if werr != nil || cerr != nil {
		os.Remove(tmp.Name())
		return errors.Join(werr, cerr)
	}
	return os.Rename(tmp.Name(), statePath)
}

func cmdEdit(args []string, ban bool) error {
	name := map[bool]string{true: "ban", false: "unban"}[ban]
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	of := addOperatorFlags(fs)
	nf := addNetFlags(fs)
	var reason *string
	if ban {
		reason = fs.String("reason", "", fmt.Sprintf("public reason, ideally a policy code (max %d bytes)", moderation.MaxReasonLen))
	}
	authority := fs.String("authority", "", "comma-separated handle authority keys, as clients are configured; empty = open mode. Only affects @handle targets")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return fmt.Errorf("%s: exactly one target (username, pk:<hex> or @handle) is required", name)
	}
	id, statePath, err := of.load()
	if err != nil {
		return err
	}
	base, stop, err := nf.join()
	if err != nil {
		return err
	}
	defer stop()
	store := &replicated{Store: base}

	// Handles resolve the way clients resolve them, or the operator
	// could ban someone other than the identity users see.
	mailbox.SetTrustedHandleAuthorities(splitList(*authority))
	target := fs.Arg(0)
	user, err := moderation.ResolveTarget(store, target)
	if err != nil {
		return err
	}
	if user != strings.ToLower(strings.TrimSpace(target)) {
		fmt.Printf("%s is username %s\n", target, user)
	}

	op, err := openOperator(store, id, statePath)
	if err != nil {
		return err
	}
	if ban {
		err = op.Ban(user, *reason)
	} else {
		err = op.Unban(user)
	}
	if errors.Is(err, moderation.ErrAlreadyBanned) || errors.Is(err, moderation.ErrNotBanned) {
		fmt.Printf("%s: nothing to do (%v); nothing published\n", name, err)
		return saveOperator(op, statePath)
	}
	if err != nil {
		return err
	}
	if err := saveOperator(op, statePath); err != nil {
		return fmt.Errorf("published, but saving the local copy failed: %w", err)
	}
	info, _ := op.Info()
	past := map[bool]string{true: "banned", false: "unbanned"}[ban]
	fmt.Printf("%s %s\nlist version %d: %d ban(s) on %d page(s), each stored on at least %d peer(s)\n",
		past, user, info.Version, info.Count, info.Pages, store.fewest)
	return nil
}

func cmdRepublish(args []string) error {
	fs := flag.NewFlagSet("republish", flag.ContinueOnError)
	of := addOperatorFlags(fs)
	nf := addNetFlags(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	id, statePath, err := of.load()
	if err != nil {
		return err
	}
	base, stop, err := nf.join()
	if err != nil {
		return err
	}
	defer stop()
	store := &replicated{Store: base}
	op, err := openOperator(store, id, statePath)
	if err != nil {
		return err
	}
	if err := op.Republish(); err != nil {
		return err
	}
	if err := saveOperator(op, statePath); err != nil {
		return err
	}
	info, _ := op.Info()
	fmt.Printf("republished list version %d: %d ban(s) on %d page(s), each stored on at least %d peer(s)\n",
		info.Version, info.Count, info.Pages, store.fewest)
	return nil
}

func cmdSignHandle(args []string) error {
	fs := flag.NewFlagSet("sign-handle", flag.ContinueOnError)
	keyPath := fs.String("key", "", "operator private key file (required)")
	ttl := fs.Duration("ttl", time.Duration(mailbox.HandleTTL)*time.Second, "how long the assignment stays valid; 0 = never lapses")
	nf := addNetFlags(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 2 {
		return errors.New("sign-handle: <handle> <username> are required")
	}
	if *keyPath == "" {
		return errors.New("-key is required")
	}
	id, err := crypto.Load(*keyPath)
	if err != nil {
		return err
	}
	handle, user := fs.Arg(0), strings.ToLower(strings.TrimSpace(fs.Arg(1)))
	if err := mailbox.ValidHandle(handle); err != nil {
		return err
	}
	base, stop, err := nf.join()
	if err != nil {
		return err
	}
	defer stop()
	store := &replicated{Store: base}

	// Trust this key as an authority for the lookups below, so an earlier
	// assignment it made is visible and the new one outranks it.
	pub := crypto.PublicKeyToHex(id.PublicKey)
	mailbox.SetTrustedHandleAuthorities([]string{pub})
	if cur, err := mailbox.ResolveHandle(store, handle); err == nil && cur.Username != user {
		fmt.Printf("reassigning @%s from %s\n", cur.Handle, cur.Username)
	}
	rec, err := moderation.AssignHandle(store, id, handle, user, *ttl)
	if err != nil {
		return err
	}
	expiry := "never"
	if rec.TTL > 0 {
		expiry = time.Unix(rec.Timestamp+rec.TTL, 0).UTC().Format(time.RFC3339)
	}
	fmt.Printf("@%s -> %s\nsigned by %s, version %d, expires %s, stored on %d peer(s)\n",
		rec.Handle, rec.Username, pub, rec.Version, expiry, store.fewest)
	fmt.Println("clients resolve it only if they trust this key as a handle authority")
	return nil
}

func cmdList(args []string) error {
	fs := flag.NewFlagSet("list", flag.ContinueOnError)
	operators := fs.String("operator", "", "comma-separated operator public keys whose lists to read")
	keyPath := fs.String("key", "", "operator private key file; reads that operator's list")
	nf := addNetFlags(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	keys := splitList(*operators)
	if *keyPath != "" {
		id, err := crypto.Load(*keyPath)
		if err != nil {
			return err
		}
		keys = append(keys, crypto.PublicKeyToHex(id.PublicKey))
	}
	if len(keys) == 0 {
		return errors.New("list: -operator or -key is required")
	}
	store, stop, err := nf.join()
	if err != nil {
		return err
	}
	defer stop()

	y := moderation.NewSync(store, keys)
	rerr := y.Refresh()
	infos := y.Lists()
	if len(infos) == 0 {
		fmt.Println("no ban list found")
	}
	for _, li := range infos {
		fmt.Printf("operator %s: version %d, published %s, %d ban(s) on %d page(s)\n",
			li.Operator, li.Version, time.Unix(li.Published, 0).UTC().Format(time.RFC3339), li.Count, li.Pages)
	}
	for _, b := range y.List() {
		// %q, not %s: reasons are checked for control characters, but a
		// display tool shouldn't depend on that alone.
		fmt.Printf("  %s  since %s  by %.8s  %q\n",
			b.Username, time.Unix(b.Since, 0).UTC().Format(time.RFC3339), b.Operator, b.Reason)
	}
	return rerr
}

func splitList(s string) []string {
	var out []string
	for _, part := range strings.Split(s, ",") {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
}
