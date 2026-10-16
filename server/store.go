// Package server is the part of AltNet Relay that has to run on a machine
// the operator controls: it turns Studio accounts into Relay identities,
// signs the unique @names, takes user reports, and publishes network bans.
//
// It is deliberately a separate service beside the accounts backend rather
// than a change to it. The backend already holds real people's accounts;
// this service never sees a password. For each request it asks the backend
// one question -- "is this bearer token a valid, non-suspended account?" --
// so accounts stay exactly the Studio accounts, and nothing here can break
// them.
//
// Storage is one JSON file written atomically plus an append-only audit
// log. That is plenty for the size of network this serves, keeps the module
// free of a database dependency, and leaves data trivially easy to back up.
package server

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"altnet-relay/wire"
)

// Account is what the server remembers about one Studio account's use of
// Relay. Keyed by email: that is the identifier the backend exposes.
type Account struct {
	Email     string `json:"email"`
	Username  string `json:"username,omitempty"` // bound Relay identity (64 hex)
	PubKey    string `json:"pubkey,omitempty"`   // its Ed25519 key, hex
	Handle    string `json:"handle,omitempty"`
	BoundAt   int64  `json:"bound_at,omitempty"`
	Banned    bool   `json:"banned,omitempty"`
	BanReason string `json:"ban_reason,omitempty"` // shown to the user: the DSA statement of reasons
	BannedAt  int64  `json:"banned_at,omitempty"`

	// Since is when the server first saw this account (0 for accounts from
	// before it was recorded, which count as established). A report only
	// counts towards an automatic ban once its sender is old enough.
	Since int64 `json:"since,omitempty"`
	// Strikes counts automatic bans that came from this account's reports and
	// were later overturned by a person. At MaxStrikes its reports stop
	// counting towards automatic bans.
	Strikes int `json:"strikes,omitempty"`

	// ClearedAt is when an operator last lifted a ban. Reports made before it
	// don't count towards an automatic ban again: someone cleared by a person
	// mustn't be banned the moment one more report arrives.
	ClearedAt int64 `json:"cleared_at,omitempty"`

	// Device is the computer the account is in use on (a random ID the app
	// makes once per install). Signing in elsewhere moves it; the computer
	// that lost it signs itself out. See copies.go.
	Device   string `json:"device,omitempty"`
	DeviceAt int64  `json:"device_at,omitempty"`
	// BackupAt and BackupSize describe the locked copy of the account, if
	// any. The copy itself is a file of its own (copies.go).
	BackupAt   int64 `json:"backup_at,omitempty"`
	BackupSize int64 `json:"backup_size,omitempty"`
}

// EvidenceMsg is one message a reporter chose to attach.
type EvidenceMsg = wire.EvidenceMsg

// Report is a user report awaiting, or holding, an admin decision.
type Report struct {
	ID               int64         `json:"id"`
	ReporterEmail    string        `json:"reporter_email"`
	ReportedUsername string        `json:"reported_username"`
	ReportedHandle   string        `json:"reported_handle,omitempty"`
	Reason           string        `json:"reason"`
	Evidence         []EvidenceMsg `json:"evidence,omitempty"`
	Status           string        `json:"status"` // pending | banned | dismissed
	CreatedAt        int64         `json:"created_at"`
	DecidedAt        int64         `json:"decided_at,omitempty"`
	DecidedBy        string        `json:"decided_by,omitempty"` // an admin's email, or "automatic"
	Note             string        `json:"note,omitempty"`
}

type fileData struct {
	Accounts     map[string]*Account `json:"accounts"` // email -> account
	Handles      map[string]string   `json:"handles"`  // handle -> email: the uniqueness index
	Reports      []*Report           `json:"reports"`
	NextReportID int64               `json:"next_report_id"`
}

// Store persists the server's state. All access goes through View/Update so
// that every change is atomic with respect to other requests and reaches
// disk before the request is answered.
type Store struct {
	path  string
	audit string

	mu sync.Mutex
	d  fileData
}

// OpenStore loads (or creates) the store under dir.
func OpenStore(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	s := &Store{
		path:  filepath.Join(dir, "relay-server.json"),
		audit: filepath.Join(dir, "audit.log"),
		d: fileData{
			Accounts: map[string]*Account{},
			Handles:  map[string]string{},
		},
	}
	raw, err := os.ReadFile(s.path)
	switch {
	case os.IsNotExist(err):
		return s, nil
	case err != nil:
		return nil, err
	}
	if err := json.Unmarshal(raw, &s.d); err != nil {
		return nil, fmt.Errorf("server: %s is damaged: %w", s.path, err)
	}
	if s.d.Accounts == nil {
		s.d.Accounts = map[string]*Account{}
	}
	if s.d.Handles == nil {
		s.d.Handles = map[string]string{}
	}
	return s, nil
}

// Dir is the folder the store keeps its files in.
func (s *Store) Dir() string { return filepath.Dir(s.path) }

// View runs fn with read access to the data.
func (s *Store) View(fn func(d *fileData)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	fn(&s.d)
}

// Update runs fn and, if it succeeds, writes the result to disk. If fn
// fails, or the write does, the in-memory data is rolled back so memory
// never claims something the disk does not have.
func (s *Store) Update(fn func(d *fileData) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	before, err := json.Marshal(&s.d)
	if err != nil {
		return err
	}
	rollback := func() {
		var restored fileData
		if json.Unmarshal(before, &restored) == nil {
			s.d = restored
		}
	}
	if err := fn(&s.d); err != nil {
		rollback()
		return err
	}
	blob, err := json.MarshalIndent(&s.d, "", "  ")
	if err != nil {
		rollback()
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, blob, 0o600); err != nil {
		rollback()
		return err
	}
	if err := os.Rename(tmp, s.path); err != nil {
		rollback()
		return err
	}
	return nil
}

// Audit appends one line to the audit log: what happened, to whom, by whom.
// Reports and decisions must be kept as a record of having acted on notices;
// this is that record, and it is append-only on purpose.
func (s *Store) Audit(event string, fields map[string]any) {
	entry := map[string]any{"ts": time.Now().Unix(), "event": event}
	for k, v := range fields {
		entry[k] = v
	}
	line, err := json.Marshal(entry)
	if err != nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	f, err := os.OpenFile(s.audit, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = f.Write(append(line, '\n'))
}

// accountFor returns the account for email, creating an empty one. Callers
// hold the store lock (they are inside Update).
func (d *fileData) accountFor(email string) *Account {
	a := d.Accounts[email]
	if a == nil {
		a = &Account{Email: email, Since: time.Now().Unix()}
		d.Accounts[email] = a
	}
	return a
}

// byUsername finds the account bound to a Relay username.
func (d *fileData) byUsername(username string) *Account {
	for _, a := range d.Accounts {
		if a.Username == username {
			return a
		}
	}
	return nil
}
