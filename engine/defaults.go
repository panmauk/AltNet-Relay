package engine

import "altnet-relay/account"

// The AltNet seed: the always-on VPS every Relay client starts from.
//
// Two doors on the same machine:
//
//   - 9000 is the peer port. A new client contacts it to join the network
//     and learn about other nodes; this is what "bootstrap" means.
//   - 9100 is the relay port. A computer behind a home router can't accept
//     incoming connections, so it registers here and other people reach it
//     through the seed instead. Without it, two people who are both behind
//     routers could not deliver messages to each other directly.
//
// Both were read from the running server (altnet.service), not assumed
// from the docs. The seed also answers to seed.panmox.org; the app uses the
// IP so that joining never depends on a DNS lookup working. Switching to the
// name would let the seed move machines without shipping a new app.
var (
	DefaultBootstrap = []string{"130.162.215.155:9000"}
	DefaultRelays    = []string{"130.162.215.155:9100"}
)

// DefaultOperators is the operator's PUBLIC key. Every copy of the app
// trusts it for two things: ban lists (who is silenced across the network)
// and @names (only names it signed resolve). The matching private key lives
// on the operator's server and backup; anyone holding it can ban anyone and
// hand out any name, so it is never in this repository or in the app.
//
// Replacing this key is a hard fork of the moderation: apps built with a
// different key trust a different operator.
var DefaultOperators = []string{"312f47337d9a992bce8aac466508358cafa92c4ff7420f4364a2960506caa5f9"}

// DefaultSTUN is the seed's STUN service, used by voice calls to learn a
// computer's address as the internet sees it. It is only a mirror held up to
// the caller: it never carries any call audio. Signed-in people also get
// relay credentials from the server (see Engine.CallServers) for the cases
// where two strict routers can't reach each other directly.
var DefaultSTUN = []account.IceServer{{URLs: []string{"stun:130.162.215.155:3478"}}}
