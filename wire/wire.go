// Package wire holds the few definitions the app and relay-server must agree
// on exactly, kept apart so neither has to import the other.
package wire

import "fmt"

// BindMessage is what a client signs to prove it holds the Relay key it is
// attaching to an account. Without this proof anyone could bind someone
// else's username to their own account -- and then a ban of that account
// would silence the innocent owner.
func BindMessage(username string, ts int64) []byte {
	return []byte(fmt.Sprintf("altnet-relay-bind-v1\n%s\n%d", username, ts))
}

// EvidenceMsg is one message a reporter chose to attach to a report.
type EvidenceMsg struct {
	From string `json:"from"` // "them" or "me"
	Text string `json:"text"`
	Sent int64  `json:"sent"`
}
