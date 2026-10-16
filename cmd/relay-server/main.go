// Command relay-server is the operator's side of AltNet Relay. It runs on
// the always-on VPS beside the seed and the accounts backend, and does
// four things:
//
//   - answers the app's account requests (bind an identity, claim an
//     @name, report a user) after checking each sign-in with the Studio
//     accounts backend;
//   - signs the unique @names with the operator key;
//   - publishes network bans: automatically when three different people have
//     reported the same account, or when an admin decides a report;
//   - optionally (-hub) acts as a notification watcher, so apps hear about
//     new messages instantly rather than on their once-a-minute check.
//
// It joins the network as an ordinary node through the seed on the same
// machine. It never sees a password: only tokens the backend issued.
//
//	relay-server -data /opt/altnet/relay-data -operator-key operator.key
package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"altnet/core/crypto"
	"altnet/core/dht"
	"altnet/core/peer"

	"altnet-relay/dhtstore"
	"altnet-relay/notify"
	"altnet-relay/server"
)

func main() {
	listen := flag.String("listen", "127.0.0.1:8788", "address for the HTTP API (keep it on localhost; Caddy fronts it)")
	dataDir := flag.String("data", "relay-data", "directory for state, the audit log and the DHT store")
	backend := flag.String("backend", "http://127.0.0.1:8787", "the Studio accounts backend, used to check sign-ins")
	keyPath := flag.String("operator-key", "", "operator key file (create with: relay-admin keygen -out <file>)")
	bootstrap := flag.String("bootstrap", "127.0.0.1:9000", "AltNet peer to join through: the seed on this machine")
	peerListen := flag.String("peer-listen", "0.0.0.0:9200", "address this node accepts AltNet peers on")
	public := flag.Bool("public", false, "this node is directly reachable from the internet at -peer-listen")
	relayAddr := flag.String("relay", "", "public address of a relay to register with (the seed's, e.g. 130.162.215.155:9100), so other nodes can reach this one without an open port")
	hub := flag.Bool("hub", false, "also act as a notification watcher for Relay clients")
	storeMB := flag.Int64("store-mb", 16, "most network data this node keeps for others, in MB (it is not meant to be a storage node)")
	turnSecret := flag.String("turn-secret-file", "", "file holding coturn's shared secret; enables call-relay credentials")
	turnURLs := flag.String("turn-urls", "", "comma-separated call-relay addresses handed to the app, e.g. turn:host:3478?transport=udp")
	minReporterAge := flag.Duration("report-min-age", server.DefaultMinReporterAge, "how old an account must be before its reports can add up to an automatic ban (0 counts all)")
	releases := flag.String("releases", "https://github.com/panmauk/AltNet-Relay/releases/latest/download/update.json", "where the latest signed release notice is published (empty: no update notices)")
	flag.Parse()

	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	if err := run(log, *listen, *dataDir, *backend, *keyPath, *bootstrap, *peerListen, *public, *relayAddr, *hub, *turnSecret, *turnURLs, *storeMB, *minReporterAge, *releases); err != nil {
		log.Error("relay-server stopped", "err", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger, listen, dataDir, backend, keyPath, bootstrap, peerListen string, public bool, relayAddr string, hub bool, turnSecretFile, turnURLs string, storeMB int64, minReporterAge time.Duration, releases string) error {
	if keyPath == "" {
		return errors.New("-operator-key is required (create one with: relay-admin keygen -out <file>)")
	}
	operator, err := crypto.Load(keyPath)
	if err != nil {
		return fmt.Errorf("operator key: %w", err)
	}
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return err
	}

	// A node on the network, for publishing bans (and the optional watcher).
	nodeKey, err := crypto.LoadOrCreate(filepath.Join(dataDir, "node.key"))
	if err != nil {
		return fmt.Errorf("node identity: %w", err)
	}
	p := peer.New(nodeKey, peerListen)
	p.SetPublic(public)
	if err := p.Start(); err != nil {
		return fmt.Errorf("start node: %w", err)
	}
	defer p.Stop()
	if relayAddr != "" {
		// Without this, a node that isn't public advertises its listen
		// address, which nobody outside this machine can dial: other nodes
		// would keep trying it and failing, and it could never act as a
		// watcher (-hub). Through the seed's relay it's reachable like any
		// home computer running the app.
		p.UseRelay(relayAddr)
	}
	// A small, capped store. On a network this size every node is among the
	// closest to almost every key, so an uncapped node is sent a copy of
	// everything -- on the first server this was 127 MB and loaded entirely
	// into memory at start, enough to be killed by the memory limit. This
	// process only needs room for ban lists, so it holds little and evicts
	// the oldest.
	d, err := dht.NewWithLimit(p, filepath.Join(dataDir, "store"), storeMB<<20)
	if err != nil {
		return fmt.Errorf("start dht: %w", err)
	}
	if bootstrap != "" {
		if err := d.Bootstrap(bootstrap); err != nil {
			// Not fatal: the account API works without the network, and
			// maintenance keeps retrying. Bans just can't publish until
			// it joins -- and the API says so if an admin tries.
			log.Warn("couldn't join the network yet", "peer", bootstrap, "err", err)
		}
		d.StartMaintenance(dht.MaintenanceConfig{BootstrapPeers: []string{bootstrap}})
	}
	if hub {
		h := notify.NewHub(p, d)
		h.Start()
		defer h.Stop()
		log.Info("acting as a notification watcher")
	}

	store, err := server.OpenStore(dataDir)
	if err != nil {
		return err
	}
	banner, err := server.NewOperatorBanner(dhtstore.New(d), operator, filepath.Join(dataDir, "banlist.state"))
	if err != nil {
		return err
	}
	go func() {
		// Keep the ban list on the network: once shortly after start, then
		// every few hours. A failure is only logged; the next round retries.
		for delay := time.Minute; ; delay = 6 * time.Hour {
			time.Sleep(delay)
			if err := banner.Republish(); err != nil {
				log.Warn("couldn't re-store the ban list", "err", err)
			}
		}
	}()
	accounts := &server.BackendVerifier{BaseURL: backend}
	srv := server.New(store, accounts, operator, banner, log)
	srv.MinReporterAge = minReporterAge
	go func() {
		// Three different people reporting the same account bans it as the
		// third report arrives. This is the safety net for when the network
		// couldn't be reached at that moment: look again every few minutes.
		for delay := 30 * time.Second; ; delay = 5 * time.Minute {
			time.Sleep(delay)
			srv.SweepReports()
		}
	}()
	if turnSecretFile != "" {
		secret, err := os.ReadFile(turnSecretFile)
		if err != nil {
			return fmt.Errorf("call relay secret: %w", err)
		}
		var urls []string
		for _, u := range strings.Split(turnURLs, ",") {
			if u = strings.TrimSpace(u); u != "" {
				urls = append(urls, u)
			}
		}
		srv.SetTURN(&server.TURNConfig{Secret: bytes.TrimSpace(secret), URLs: urls})
		log.Info("call relay credentials enabled", "urls", urls)
	}

	// App updates: mirror the latest signed release notice for the apps.
	if releases != "" {
		mirror := &server.UpdateMirror{Source: releases, Log: log}
		srv.SetUpdates(mirror)
		go mirror.Run(context.Background())
		log.Info("update notices from", "source", releases)
	}

	// Deleting an account emails a code, through the same SMTP relay and
	// settings (ALTNET_SMTP_*) as the accounts backend. Without them the app
	// is told deletion isn't available and to write to us instead.
	if host := os.Getenv("ALTNET_SMTP_HOST"); host != "" {
		port := 587
		if v := os.Getenv("ALTNET_SMTP_PORT"); v != "" {
			if _, err := fmt.Sscan(v, &port); err != nil {
				return fmt.Errorf("ALTNET_SMTP_PORT: %w", err)
			}
		}
		from := os.Getenv("ALTNET_SMTP_FROM")
		if from == "" {
			from = "AltNet <code.verification@panmox.org>"
		}
		srv.SetDeletion(&server.SMTPMailer{Host: host, Port: port,
			User: os.Getenv("ALTNET_SMTP_USER"), Pass: os.Getenv("ALTNET_SMTP_PASS"), From: from}, accounts)
		log.Info("account deletion enabled", "smtp", host)
	} else {
		log.Warn("account deletion is off: ALTNET_SMTP_HOST isn't set")
	}

	httpSrv := &http.Server{
		Addr:              listen,
		Handler:           srv.Routes(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second, // a ban waits on the DHT
		IdleTimeout:       2 * time.Minute,
	}

	log.Info("relay-server up",
		"http", listen, "backend", backend,
		"operator_public_key", crypto.PublicKeyToHex(operator.PublicKey),
		"node", nodeKey.ShortID())

	errc := make(chan error, 1)
	go func() { errc <- httpSrv.ListenAndServe() }()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	select {
	case err := <-errc:
		return err
	case <-sig:
		log.Info("shutting down")
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return httpSrv.Shutdown(ctx)
	}
}
