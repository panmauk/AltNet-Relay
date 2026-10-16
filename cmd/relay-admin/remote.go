package main

// The commands here talk to relay-server over HTTPS as a signed-in Studio
// administrator, rather than publishing to the network directly with the
// operator key. They are the day-to-day moderation tools: read the reports,
// decide them, ban an account. Deciding a report to "ban" makes the SERVER
// publish the network ban (it holds the operator key), so these commands need
// no key file and can be run from any computer.
//
//	relay-admin login  -email you@panmox.org               prints a session token
//	relay-admin reports [-status pending|banned|dismissed|all]
//	relay-admin decide <report-id> ban|dismiss [note...]
//	relay-admin ban-account (-email <e> | -username <hex>) [note...]
//	relay-admin unban-account <username>
//	relay-admin unbind-account <email>          move an account to a new computer
//
// Each takes -server (default the production relay-server) and -token, or the
// RELAY_ADMIN_TOKEN environment variable.

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"altnet-relay/account"
	"altnet-relay/server"
)

type remoteFlags struct {
	server *string
	token  *string
}

func addRemoteFlags(fs *flag.FlagSet) remoteFlags {
	return remoteFlags{
		server: fs.String("server", account.DefaultRelayURL, "relay-server address"),
		token:  fs.String("token", "", "admin session token (or set RELAY_ADMIN_TOKEN)"),
	}
}

func (rf remoteFlags) call(method, path string, in, out any) error {
	token := *rf.token
	if token == "" {
		token = os.Getenv("RELAY_ADMIN_TOKEN")
	}
	if token == "" {
		return errors.New("no admin token: run `relay-admin login`, then pass -token or set RELAY_ADMIN_TOKEN")
	}
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, strings.TrimRight(*rf.server, "/")+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := (&http.Client{Timeout: 90 * time.Second}).Do(req) // a ban waits on the network
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode/100 != 2 {
		var e struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(raw, &e) == nil && e.Error != "" {
			return fmt.Errorf("%s (%d)", e.Error, resp.StatusCode)
		}
		return fmt.Errorf("server answered %d", resp.StatusCode)
	}
	if out != nil {
		return json.Unmarshal(raw, out)
	}
	return nil
}

// cmdLogin signs in to the accounts backend and prints only the token, so it
// can be captured: export RELAY_ADMIN_TOKEN=$(relay-admin login -email ...).
// The password is read from RELAY_ADMIN_PASSWORD or, failing that, one line
// of standard input; it is never taken as a flag, where it would end up in
// shell history.
func cmdLogin(args []string) error {
	fs := flag.NewFlagSet("login", flag.ContinueOnError)
	studio := fs.String("studio", account.DefaultStudioURL, "accounts backend address")
	email := fs.String("email", "", "your admin account's email")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *email == "" {
		return errors.New("-email is required")
	}
	password := os.Getenv("RELAY_ADMIN_PASSWORD")
	if password == "" {
		fmt.Fprint(os.Stderr, "password: ")
		line, err := bufio.NewReader(os.Stdin).ReadString('\n')
		if err != nil && line == "" {
			return errors.New("no password given")
		}
		password = strings.TrimRight(line, "\r\n")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	sess, err := account.New(*studio, "").Login(ctx, *email, password)
	if err != nil {
		return err
	}
	if !sess.IsAdmin {
		return errors.New("that account is not an administrator")
	}
	fmt.Println(sess.Token)
	return nil
}

func cmdReports(args []string) error {
	fs := flag.NewFlagSet("reports", flag.ContinueOnError)
	rf := addRemoteFlags(fs)
	status := fs.String("status", "pending", "pending, banned, dismissed or all")
	if err := fs.Parse(args); err != nil {
		return err
	}
	var out struct {
		Reports []server.Report `json:"reports"`
	}
	if err := rf.call("GET", "/v1/admin/reports?status="+*status, nil, &out); err != nil {
		return err
	}
	if len(out.Reports) == 0 {
		fmt.Println("no", *status, "reports")
		return nil
	}
	for _, r := range out.Reports {
		printReport(r)
	}
	return nil
}

func printReport(r server.Report) {
	who := r.ReportedUsername
	if r.ReportedHandle != "" {
		who = "@" + r.ReportedHandle + "  (" + who + ")"
	}
	fmt.Printf("#%d  [%s]  %s\n", r.ID, r.Status, time.Unix(r.CreatedAt, 0).Format("2006-01-02 15:04"))
	fmt.Printf("  reported: %s\n  by:       %s\n  reason:   %s\n", who, r.ReporterEmail, r.Reason)
	for _, m := range r.Evidence {
		fmt.Printf("    %-4s %s  %s\n", m.From, time.Unix(m.Sent, 0).Format("01-02 15:04"), m.Text)
	}
	if r.DecidedBy != "" {
		fmt.Printf("  decided by %s: %s\n", r.DecidedBy, r.Note)
	}
	fmt.Println()
}

func cmdDecide(args []string) error {
	fs := flag.NewFlagSet("decide", flag.ContinueOnError)
	rf := addRemoteFlags(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() < 2 {
		return errors.New("usage: relay-admin decide <report-id> ban|dismiss [note...]")
	}
	id, err := strconv.ParseInt(fs.Arg(0), 10, 64)
	if err != nil {
		return errors.New("the report id is the number after # in `relay-admin reports`")
	}
	decision := fs.Arg(1)
	if decision != "ban" && decision != "dismiss" {
		return errors.New(`decision must be "ban" or "dismiss"`)
	}
	note := strings.Join(fs.Args()[2:], " ")
	if decision == "ban" && note == "" {
		return errors.New("a ban needs a note: it is the reason the banned person is shown")
	}
	var out struct{ Status string }
	if err := rf.call("POST", "/v1/admin/reports/decide", map[string]any{"id": id, "decision": decision, "note": note}, &out); err != nil {
		return err
	}
	fmt.Printf("report #%d: %s\n", id, out.Status)
	return nil
}

func cmdBanAccount(args []string) error {
	fs := flag.NewFlagSet("ban-account", flag.ContinueOnError)
	rf := addRemoteFlags(fs)
	email := fs.String("email", "", "the account's email")
	username := fs.String("username", "", "the Relay username (64 hex)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	note := strings.Join(fs.Args(), " ")
	if note == "" {
		return errors.New("a ban needs a note: it is the reason the banned person is shown")
	}
	if err := rf.call("POST", "/v1/admin/ban", map[string]string{"email": *email, "username": *username, "note": note}, nil); err != nil {
		return err
	}
	fmt.Println("banned on the network")
	return nil
}

func cmdUnbanAccount(args []string) error {
	fs := flag.NewFlagSet("unban-account", flag.ContinueOnError)
	rf := addRemoteFlags(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("usage: relay-admin unban-account <username>")
	}
	if err := rf.call("POST", "/v1/admin/unban", map[string]string{"username": fs.Arg(0)}, nil); err != nil {
		return err
	}
	fmt.Println("ban lifted")
	return nil
}

func cmdUnbindAccount(args []string) error {
	fs := flag.NewFlagSet("unbind-account", flag.ContinueOnError)
	rf := addRemoteFlags(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("usage: relay-admin unbind-account <email>")
	}
	if err := rf.call("POST", "/v1/admin/unbind", map[string]string{"email": fs.Arg(0)}, nil); err != nil {
		return err
	}
	fmt.Println("detached: the person can now sign in on their new computer; their @name moves with them")
	return nil
}
