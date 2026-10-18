package server

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"altnet/core/crypto"

	"altnet-relay/mailbox"
	"altnet-relay/wire"
)

const (
	// HandleRecordTTL is how long a signed @name stays valid. The app
	// re-requests it each time it starts, so this only lapses for someone
	// who hasn't opened Relay for three months -- or whose account was
	// banned, which is the point of making it expire at all.
	HandleRecordTTL = 90 * 24 * time.Hour

	maxSkew         = 5 * time.Minute
	maxBody         = 64 << 10
	maxReasonLen    = 1000
	maxNoteLen      = 500
	maxEvidenceMsgs = 10 // the app sends at most 10; the Privacy Policy says 10
	maxEvidenceText = 1000
	reportsPerDay   = 10

	// AutoBanReports is how many different people must report the same
	// account before it is banned automatically. Reports pile up quietly
	// until then; the network learns of nothing, and the reported person
	// is not told.
	AutoBanReports = 3
	// AutoBanWindow is how recent those reports must be. A few complaints
	// spread over a year are not the same thing as three in a month.
	AutoBanWindow = 30 * 24 * time.Hour

	// DefaultMinReporterAge is how old an account must be before its reports
	// count towards an automatic ban. Throwaway accounts made for the occasion
	// are the cheap way to pile on someone; making the accounts wait, days
	// before they are needed, is not.
	DefaultMinReporterAge = 72 * time.Hour
	// MaxReportTargets is how many different accounts one person may report
	// (inside AutoBanWindow) and still be counted. Someone who reports half the
	// network isn't telling us about abuse.
	MaxReportTargets = 5
	// MaxStrikes is how many overturned automatic bans make a person's
	// reports stop counting towards them.
	MaxStrikes = 2

	// autoDecider is recorded as who decided a report banned automatically.
	autoDecider = "automatic"

	// autoBanReason is what the banned person is shown (the statement of
	// reasons). It says what happened and how to appeal, and not who reported.
	autoBanReason = "reported by several different people for breaking AltNet's rules"

	// What goes on the PUBLIC ban list: a fixed reason, never an admin's
	// note -- the note is for the person banned (shown to them in the app),
	// not for everyone on the network.
	publicReasonReports  = "Terms §5 — user reports"
	publicReasonOperator = "Terms §5 — operator decision"
)

// reservedHandles can't be claimed: they would let someone pose as the
// service, or as the people who run it. Anyone genuinely entitled to one
// is signed for it by hand with relay-admin.
var reservedHandles = map[string]bool{
	"admin": true, "administrator": true, "altnet": true, "panmox": true,
	"relay": true, "support": true, "help": true, "abuse": true, "security": true,
	"root": true, "system": true, "staff": true, "moderator": true, "official": true,
	"studio": true, "team": true, "info": true, "contact": true, "legal": true,
}

// Banner publishes network bans. In production it is backed by
// moderation.Operator; tests substitute a fake.
type Banner interface {
	Ban(username, reason string) error
	Unban(username string) error
}

// Server is the HTTP API. Create with New and serve Routes().
type Server struct {
	store     *Store
	auth      Verifier
	authority *crypto.Identity
	banner    Banner
	log       *slog.Logger
	now       func() time.Time
	turn      *TURNConfig // nil until SetTURN

	// The release notice for app updates (updates.go): off until SetUpdates.
	updates *UpdateMirror

	// Account deletion (see delete.go): off until SetDeletion.
	mailer   Mailer
	accounts Accounts
	del      deletions
	guesses  passwordGuesses // wrong passwords, per account; see limits.go

	// MinReporterAge is the age an account needs before its reports count
	// towards an automatic ban (DefaultMinReporterAge unless changed).
	MinReporterAge time.Duration

	bannerMu sync.Mutex // one network ban edit at a time
	verMu    sync.Mutex
	lastVer  int64
}

// New builds a Server. authority signs @names; its public key is what
// clients are configured to trust for them.
func New(store *Store, auth Verifier, authority *crypto.Identity, banner Banner, log *slog.Logger) *Server {
	if log == nil {
		log = slog.Default()
	}
	return &Server{store: store, auth: auth, authority: authority, banner: banner, log: log, now: time.Now,
		MinReporterAge: DefaultMinReporterAge}
}

// Routes returns the HTTP handler. Paths carry no prefix: whatever fronts
// this service (Caddy) strips /relay before forwarding.
func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("GET /v1/me", s.authed(s.handleMe))
	mux.HandleFunc("GET /v1/update", func(w http.ResponseWriter, r *http.Request) {
		if s.updates == nil {
			writeError(w, http.StatusNotFound, "no release published yet")
			return
		}
		s.updates.ServeHTTP(w, r)
	})
	mux.HandleFunc("POST /v1/identity", s.authed(s.handleBind))
	mux.HandleFunc("POST /v1/handle", s.authed(s.handleClaim))
	mux.HandleFunc("POST /v1/handle/refresh", s.authed(s.handleRefresh))
	mux.HandleFunc("GET /v1/turn", s.authed(s.handleTURN))
	mux.HandleFunc("POST /v1/reports", s.authed(s.handleReport))
	mux.HandleFunc("POST /v1/account/delete/start", s.authed(s.handleDeleteStart))
	mux.HandleFunc("POST /v1/account/delete", s.authed(s.handleDelete))
	mux.HandleFunc("POST /v1/device", s.authed(s.handleDevice))
	mux.HandleFunc("GET /v1/backup", s.authed(s.handleGetBackup))
	mux.HandleFunc("PUT /v1/backup", s.authed(s.handlePutBackup))
	mux.HandleFunc("DELETE /v1/backup", s.authed(s.handleDeleteBackup))
	mux.HandleFunc("GET /v1/admin/reports", s.admin(s.handleAdminReports))
	mux.HandleFunc("POST /v1/admin/reports/decide", s.admin(s.handleAdminDecide))
	mux.HandleFunc("POST /v1/admin/ban", s.admin(s.handleAdminBan))
	mux.HandleFunc("POST /v1/admin/unbind", s.admin(s.handleAdminUnbind))
	mux.HandleFunc("POST /v1/admin/unban", s.admin(s.handleAdminUnban))
	return mux
}

// ─── authentication ───

type principalHandler func(w http.ResponseWriter, r *http.Request, p Principal)

func (s *Server) authed(next principalHandler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token := bearer(r)
		if token == "" {
			writeError(w, http.StatusUnauthorized, "sign in first")
			return
		}
		p, err := s.auth.Verify(r.Context(), token)
		switch {
		case errors.Is(err, ErrUnauthorized):
			writeError(w, http.StatusUnauthorized, err.Error())
			return
		case errors.Is(err, ErrSuspended):
			writeError(w, http.StatusForbidden, err.Error())
			return
		case err != nil:
			s.log.Warn("account check failed", "err", err)
			writeError(w, http.StatusServiceUnavailable, "the account service isn't reachable right now; try again shortly")
			return
		}
		next(w, r, p)
	}
}

func (s *Server) admin(next principalHandler) http.HandlerFunc {
	return s.authed(func(w http.ResponseWriter, r *http.Request, p Principal) {
		if !p.Admin {
			writeError(w, http.StatusForbidden, "admin only")
			return
		}
		next(w, r, p)
	})
}

func bearer(r *http.Request) string {
	const prefix = "Bearer "
	h := r.Header.Get("Authorization")
	if !strings.HasPrefix(h, prefix) {
		return ""
	}
	return strings.TrimSpace(h[len(prefix):])
}

// ─── who am I ───

func (s *Server) handleMe(w http.ResponseWriter, _ *http.Request, p Principal) {
	var out struct {
		Email     string `json:"email"`
		Username  string `json:"username,omitempty"`
		Handle    string `json:"handle,omitempty"`
		Banned    bool   `json:"banned"`
		BanReason string `json:"ban_reason,omitempty"`
		Admin     bool   `json:"is_admin"`
		Device    string `json:"device,omitempty"`
		BackupAt  int64  `json:"backup_at,omitempty"`
	}
	out.Email, out.Admin = p.Email, p.Admin
	s.store.View(func(d *fileData) {
		if a := d.Accounts[p.Email]; a != nil {
			out.Username, out.Handle, out.Banned, out.BanReason = a.Username, a.Handle, a.Banned, a.BanReason
			out.Device, out.BackupAt = a.Device, a.BackupAt
		}
	})
	writeJSON(w, http.StatusOK, out)
}

// ─── binding a Relay identity to the account ───

// BindMessage is what the client signs to prove it holds the Relay key it
// is binding; see wire.BindMessage.
func BindMessage(username string, ts int64) []byte { return wire.BindMessage(username, ts) }

type bindReq struct {
	Username string `json:"username"`
	PubKey   string `json:"pubkey"`
	TS       int64  `json:"ts"`
	Sig      string `json:"sig"`
	// Replace moves the account to this identity: "start fresh here" on a
	// computer that has no way to open the account's locked copy. The old
	// identity is let go; the @name follows the account. It needs the
	// account's password, not just a sign-in token: a stolen token must not
	// be enough to take someone's @name.
	Replace  bool   `json:"replace,omitempty"`
	Password string `json:"password,omitempty"`
}

func (s *Server) handleBind(w http.ResponseWriter, r *http.Request, p Principal) {
	var req bindReq
	if !readJSON(w, r, &req) {
		return
	}
	req.Username = strings.ToLower(strings.TrimSpace(req.Username))
	pub, err := crypto.PublicKeyFromHex(strings.ToLower(strings.TrimSpace(req.PubKey)))
	if err != nil || crypto.PublicKeyToID(pub) != req.Username {
		writeError(w, http.StatusBadRequest, "that key doesn't match that username")
		return
	}
	if !s.fresh(req.TS) {
		writeError(w, http.StatusBadRequest, "your clock looks wrong; check the date and time")
		return
	}
	if err := crypto.Verify(pub, BindMessage(req.Username, req.TS), req.Sig); err != nil {
		writeError(w, http.StatusBadRequest, "couldn't verify that you own that key")
		return
	}
	if req.Replace {
		var he *httpError
		switch err := s.checkPassword(r.Context(), p.Email, req.Password); {
		case err == nil:
		case errors.Is(err, ErrWrongPassword):
			writeError(w, http.StatusForbidden, "that password isn't right")
			return
		case errors.As(err, &he):
			s.fail(w, err)
			return
		case errors.Is(err, errNoPasswordCheck):
			writeError(w, http.StatusServiceUnavailable, "starting fresh isn't available right now; try again later")
			return
		default:
			writeError(w, http.StatusServiceUnavailable, "the account service isn't reachable right now; try again shortly")
			return
		}
	}

	var replaced string
	err = s.store.Update(func(d *fileData) error {
		if other := d.byUsername(req.Username); other != nil && other.Email != p.Email {
			return &httpError{status: http.StatusConflict, code: CodeIdentityTaken,
				msg: "that Relay identity already belongs to another account"}
		}
		a := d.accountFor(p.Email)
		if a.Banned {
			return errBanned(a)
		}
		if a.Username != "" && a.Username != req.Username && !req.Replace {
			// One identity per account. A second computer normally brings the
			// same identity with it (the locked copy); a different one is only
			// taken when the person chose to start fresh there, so two
			// computers never quietly fight over the account.
			return &httpError{status: http.StatusConflict, code: CodeAccountHasKeys,
				msg: "this account is set up with other keys; sign in again to bring them here, or start fresh"}
		}
		if a.Username != "" && a.Username != req.Username {
			replaced = a.Username
		}
		a.Username, a.PubKey, a.BoundAt = req.Username, strings.ToLower(req.PubKey), s.now().Unix()
		return nil
	})
	if s.fail(w, err) {
		return
	}
	if replaced != "" {
		s.store.Audit("rebind", map[string]any{"email": p.Email, "was": replaced, "username": req.Username})
	}
	s.store.Audit("bind", map[string]any{"email": p.Email, "username": req.Username})
	writeJSON(w, http.StatusOK, map[string]string{"username": req.Username})
}

// ─── names ───

type claimReq struct {
	Handle string `json:"handle"`
}

func (s *Server) handleClaim(w http.ResponseWriter, r *http.Request, p Principal) {
	var req claimReq
	if !readJSON(w, r, &req) {
		return
	}
	if err := mailbox.ValidHandle(req.Handle); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	handle := mailbox.CanonicalHandle(req.Handle)
	if reservedHandles[handle] {
		writeError(w, http.StatusConflict, "that name is reserved")
		return
	}
	// The app checks too, to answer at once, but a modified app mustn't be a
	// way round it: this is where names are made.
	if err := mailbox.CheckHandleDecency(handle); err != nil {
		s.store.Audit("claim refused", map[string]any{"email": p.Email, "why": "indecent"})
		writeError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}

	var username string
	err := s.store.Update(func(d *fileData) error {
		a := d.accountFor(p.Email)
		switch {
		case a.Banned:
			return errBanned(a)
		case a.Username == "":
			return errConflict("set up your Relay identity first")
		case a.Handle == handle:
			username = a.Username // claiming your own name again is a refresh
			return nil
		case a.Handle != "":
			return errConflict("you've already chosen a name, and it can't be changed")
		}
		if owner, taken := d.Handles[handle]; taken && owner != p.Email {
			return errConflict("that name is taken")
		}
		d.Handles[handle] = p.Email
		a.Handle = handle
		username = a.Username
		return nil
	})
	if s.fail(w, err) {
		return
	}
	s.store.Audit("claim", map[string]any{"email": p.Email, "handle": handle, "username": username})
	s.respondWithRecord(w, handle, username)
}

func (s *Server) handleRefresh(w http.ResponseWriter, _ *http.Request, p Principal) {
	var handle, username string
	var banned *Account
	s.store.View(func(d *fileData) {
		if a := d.Accounts[p.Email]; a != nil {
			handle, username = a.Handle, a.Username
			if a.Banned {
				banned = a
			}
		}
	})
	if banned != nil {
		writeError(w, http.StatusForbidden, banMessage(banned))
		return
	}
	if handle == "" {
		writeError(w, http.StatusNotFound, "you haven't chosen a name yet")
		return
	}
	s.respondWithRecord(w, handle, username)
}

func (s *Server) respondWithRecord(w http.ResponseWriter, handle, username string) {
	rec := &mailbox.HandleRecord{
		Handle:    handle,
		Username:  username,
		Version:   s.nextVersion(),
		Timestamp: s.now().Unix(),
		TTL:       int64(HandleRecordTTL / time.Second),
	}
	if err := rec.Sign(s.authority); err != nil {
		writeError(w, http.StatusInternalServerError, "couldn't sign that")
		return
	}
	blob, err := rec.Marshal()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "couldn't sign that")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"handle": handle, "record": json.RawMessage(blob)})
}

// nextVersion returns a record version that only ever increases, so a
// re-signed name always outranks the one it replaces.
func (s *Server) nextVersion() int64 {
	s.verMu.Lock()
	defer s.verMu.Unlock()
	v := s.now().UnixNano()
	if v <= s.lastVer {
		v = s.lastVer + 1
	}
	s.lastVer = v
	return v
}

// ─── reports ───

type reportReq struct {
	Username string        `json:"username"`
	Reason   string        `json:"reason"`
	Evidence []EvidenceMsg `json:"evidence"`
}

func (s *Server) handleReport(w http.ResponseWriter, r *http.Request, p Principal) {
	var req reportReq
	if !readJSON(w, r, &req) {
		return
	}
	req.Username = strings.ToLower(strings.TrimSpace(req.Username))
	req.Reason = strings.TrimSpace(req.Reason)
	switch {
	case len(req.Username) != 64 || !isHex(req.Username):
		writeError(w, http.StatusBadRequest, "that isn't a valid username")
		return
	case req.Reason == "":
		writeError(w, http.StatusBadRequest, "tell us what happened")
		return
	case len(req.Reason) > maxReasonLen:
		writeError(w, http.StatusBadRequest, fmt.Sprintf("keep the reason under %d characters", maxReasonLen))
		return
	case len(req.Evidence) > maxEvidenceMsgs:
		writeError(w, http.StatusBadRequest, fmt.Sprintf("attach at most %d messages", maxEvidenceMsgs))
		return
	}
	for i := range req.Evidence {
		e := &req.Evidence[i]
		if e.From != "them" && e.From != "me" {
			writeError(w, http.StatusBadRequest, "bad evidence")
			return
		}
		if len(e.Text) > maxEvidenceText {
			e.Text = e.Text[:maxEvidenceText]
		}
	}

	var id int64
	err := s.store.Update(func(d *fileData) error {
		if a := d.Accounts[p.Email]; a != nil && a.Username == req.Username {
			return errConflict("you can't report yourself")
		}
		cutoff := s.now().Add(-24 * time.Hour).Unix()
		recent := 0
		for _, rep := range d.Reports {
			if rep.ReporterEmail != p.Email || rep.CreatedAt < cutoff {
				continue
			}
			if rep.ReportedUsername == req.Username && rep.Status == "pending" {
				return errConflict("you've already reported them; we'll look at it")
			}
			recent++
		}
		if recent >= reportsPerDay {
			return errTooMany("you've sent a lot of reports today; please try again tomorrow")
		}
		d.NextReportID++
		id = d.NextReportID
		rep := &Report{
			ID: id, ReporterEmail: p.Email, ReportedUsername: req.Username,
			Reason: req.Reason, Evidence: req.Evidence, Status: "pending",
			CreatedAt: s.now().Unix(),
		}
		if target := d.byUsername(req.Username); target != nil {
			rep.ReportedHandle = target.Handle
		}
		d.Reports = append(d.Reports, rep)
		return nil
	})
	if s.fail(w, err) {
		return
	}
	s.store.Audit("report", map[string]any{"id": id, "reporter": p.Email, "reported": req.Username})
	// Three different people reporting the same account bans it. If this
	// report was the third, that happens now, before we answer; if it can't
	// be published (the network is unreachable) the report is kept and the
	// sweep tries again.
	s.autoBan(req.Username)
	// Acknowledgement of receipt: the DSA requires one, and it tells the
	// reporter their report went somewhere.
	writeJSON(w, http.StatusOK, map[string]any{
		"id": id, "message": "Thanks. We've received your report. Once three different people have reported the same account it is restricted automatically, and anyone can appeal at abuse.report@panmox.org.",
	})
}

// autoBan bans username if enough different people have reported it recently,
// and settles the reports that led to it. It reports whether it banned.
//
// Counted: pending reports, each reporter once, made inside AutoBanWindow and
// after any time an operator last cleared the account. A report an operator
// dismissed doesn't count; neither does a second report from the same person,
// or one from an account that is itself banned.
func (s *Server) autoBan(username string) bool {
	var ids []int64
	reporters := map[string]bool{}
	alreadyBanned := false
	s.store.View(func(d *fileData) {
		var cleared int64
		if a := d.byUsername(username); a != nil {
			alreadyBanned, cleared = a.Banned, a.ClearedAt
		}
		cutoff := s.now().Add(-AutoBanWindow).Unix()
		// How many different accounts each person has reported lately.
		targets := map[string]map[string]bool{}
		for _, rep := range d.Reports {
			if rep.CreatedAt < cutoff {
				continue
			}
			if targets[rep.ReporterEmail] == nil {
				targets[rep.ReporterEmail] = map[string]bool{}
			}
			targets[rep.ReporterEmail][rep.ReportedUsername] = true
		}
		for _, rep := range d.Reports {
			if rep.ReportedUsername != username || rep.Status != "pending" {
				continue
			}
			if rep.CreatedAt < cutoff || rep.CreatedAt < cleared {
				continue
			}
			if !s.reportCounts(d, rep.ReporterEmail, len(targets[rep.ReporterEmail])) {
				continue
			}
			reporters[rep.ReporterEmail] = true
			ids = append(ids, rep.ID)
		}
	})
	if len(reporters) < AutoBanReports && !alreadyBanned {
		return false
	}
	if len(ids) == 0 {
		return false
	}
	if !alreadyBanned {
		// Network first, then the record, as for any ban: recording one that
		// never took effect would be worse than trying again.
		if err := s.ban(username, publicReasonReports); err != nil {
			s.log.Warn("automatic ban couldn't be published; will retry", "username", username, "err", err)
			return false
		}
	}
	now := s.now().Unix()
	err := s.store.Update(func(d *fileData) error {
		settle := map[int64]bool{}
		for _, id := range ids {
			settle[id] = true
		}
		for _, rep := range d.Reports {
			if settle[rep.ID] {
				rep.Status, rep.DecidedAt, rep.DecidedBy = "banned", now, autoDecider
				if alreadyBanned {
					rep.Note = "the account was already banned"
				} else {
					rep.Note = "banned automatically after reports from " + fmt.Sprint(len(reporters)) + " different people"
				}
			}
		}
		if a := d.byUsername(username); a != nil && !a.Banned {
			a.Banned, a.BannedAt, a.BanReason = true, now, autoBanReason
		}
		return nil
	})
	if err != nil {
		s.log.Error("recording an automatic ban failed", "username", username, "err", err)
		return false
	}
	if !alreadyBanned {
		s.store.Audit("auto-ban", map[string]any{"username": username, "reporters": len(reporters), "reports": ids})
	}
	return true
}

// reportCounts says whether a person's reports may add up to an automatic ban.
// Their reports are always kept and always reach whoever reviews them; this is
// only about whether they can ban by themselves. Not counted:
//
//   - an account that is itself banned (someone silenced for abuse mustn't get
//     others banned in return);
//   - an account too new (see DefaultMinReporterAge);
//   - someone who has reported many different accounts lately (a spammer);
//   - someone whose reports have already caused automatic bans that a person
//     then overturned.
func (s *Server) reportCounts(d *fileData, email string, targets int) bool {
	if targets > MaxReportTargets {
		return false
	}
	a := d.Accounts[email]
	switch {
	case a == nil:
		return s.MinReporterAge <= 0 // an account the server has never seen has no age to show
	case a.Banned:
		return false
	case a.Strikes >= MaxStrikes:
		return false
	case a.Since != 0 && s.now().Sub(time.Unix(a.Since, 0)) < s.MinReporterAge:
		return false
	}
	return true
}

// SweepReports looks again at every account with pending reports, so a ban
// that couldn't be published when the third report arrived goes out once the
// network is back. The server runs it every few minutes.
func (s *Server) SweepReports() {
	seen := map[string]bool{}
	var targets []string
	s.store.View(func(d *fileData) {
		for _, rep := range d.Reports {
			if rep.Status == "pending" && !seen[rep.ReportedUsername] {
				seen[rep.ReportedUsername] = true
				targets = append(targets, rep.ReportedUsername)
			}
		}
	})
	for _, u := range targets {
		s.autoBan(u)
	}
}

func (s *Server) handleAdminReports(w http.ResponseWriter, r *http.Request, _ Principal) {
	status := r.URL.Query().Get("status")
	if status == "" {
		status = "pending"
	}
	out := []*Report{}
	s.store.View(func(d *fileData) {
		for _, rep := range d.Reports {
			if status == "all" || rep.Status == status {
				cp := *rep
				out = append(out, &cp)
			}
		}
	})
	writeJSON(w, http.StatusOK, map[string]any{"reports": out, "count": len(out)})
}

type decideReq struct {
	ID       int64  `json:"id"`
	Decision string `json:"decision"` // "ban" or "dismiss"
	Note     string `json:"note"`
}

func (s *Server) handleAdminDecide(w http.ResponseWriter, r *http.Request, admin Principal) {
	var req decideReq
	if !readJSON(w, r, &req) {
		return
	}
	req.Note = strings.TrimSpace(req.Note)
	if len(req.Note) > maxNoteLen {
		req.Note = req.Note[:maxNoteLen]
	}
	if req.Decision != "ban" && req.Decision != "dismiss" {
		writeError(w, http.StatusBadRequest, `decision must be "ban" or "dismiss"`)
		return
	}

	var target string
	var found, pending bool
	s.store.View(func(d *fileData) {
		for _, rep := range d.Reports {
			if rep.ID == req.ID {
				found, pending, target = true, rep.Status == "pending", rep.ReportedUsername
			}
		}
	})
	switch {
	case !found:
		writeError(w, http.StatusNotFound, "no such report")
		return
	case !pending:
		writeError(w, http.StatusConflict, "that report has already been decided")
		return
	}

	// Publish the ban to the network BEFORE recording the decision. If it
	// fails the report stays pending and the admin can simply retry; the
	// other order could record a ban that never took effect.
	if req.Decision == "ban" {
		if err := s.ban(target, publicReasonOperator); err != nil {
			s.log.Error("publishing ban failed", "username", target, "err", err)
			writeError(w, http.StatusBadGateway, "couldn't publish the ban to the network: "+err.Error())
			return
		}
	}

	status := "dismissed"
	if req.Decision == "ban" {
		status = "banned"
	}
	err := s.store.Update(func(d *fileData) error {
		for _, rep := range d.Reports {
			if rep.ID != req.ID {
				continue
			}
			rep.Status, rep.DecidedAt, rep.DecidedBy, rep.Note = status, s.now().Unix(), admin.Email, req.Note
		}
		if req.Decision == "ban" {
			if a := d.byUsername(target); a != nil {
				a.Banned, a.BannedAt, a.BanReason = true, s.now().Unix(), banReasonFor(req.Note)
			}
		}
		return nil
	})
	if s.fail(w, err) {
		return
	}
	s.store.Audit("decision", map[string]any{"report": req.ID, "decision": req.Decision, "by": admin.Email, "username": target, "note": req.Note})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "id": req.ID, "status": status})
}

type unbanReq struct {
	Username string `json:"username"`
}

type unbindReq struct {
	Email string `json:"email"`
}

// handleAdminUnbind detaches an account from its Relay identity, so it can be
// set up on a new computer. The account's @name stays reserved to it, and the
// next time the app renews the name it is signed for the new identity -- which
// is how a name survives a lost or reinstalled computer. The old identity is
// not banned: it is only no longer tied to anyone.
func (s *Server) handleAdminUnbind(w http.ResponseWriter, r *http.Request, admin Principal) {
	var req unbindReq
	if !readJSON(w, r, &req) {
		return
	}
	email := strings.ToLower(strings.TrimSpace(req.Email))
	var had string
	err := s.store.Update(func(d *fileData) error {
		a := d.Accounts[email]
		if a == nil || a.Username == "" {
			return errNotFound("that account has no Relay identity attached")
		}
		had = a.Username
		a.Username, a.PubKey, a.BoundAt = "", "", 0
		return nil
	})
	if s.fail(w, err) {
		return
	}
	s.store.Audit("unbind", map[string]any{"email": email, "was": had, "by": admin.Email})
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

type banReq struct {
	Email    string `json:"email"`
	Username string `json:"username"`
	Note     string `json:"note"`
}

// handleAdminBan bans an account without a report behind it -- the way to
// act on something the operator learned elsewhere, such as a Studio account
// that was suspended. Name either the email or the Relay username.
func (s *Server) handleAdminBan(w http.ResponseWriter, r *http.Request, admin Principal) {
	var req banReq
	if !readJSON(w, r, &req) {
		return
	}
	req.Email = strings.ToLower(strings.TrimSpace(req.Email))
	req.Username = strings.ToLower(strings.TrimSpace(req.Username))
	req.Note = strings.TrimSpace(req.Note)
	if len(req.Note) > maxNoteLen {
		req.Note = req.Note[:maxNoteLen]
	}
	if req.Email == "" && req.Username == "" {
		writeError(w, http.StatusBadRequest, "give an email or a username")
		return
	}
	if req.Username == "" {
		s.store.View(func(d *fileData) {
			if a := d.Accounts[req.Email]; a != nil {
				req.Username = a.Username
			}
		})
		if req.Username == "" {
			writeError(w, http.StatusNotFound, "that account hasn't set up Relay, so there's no identity to ban")
			return
		}
	}
	if len(req.Username) != 64 || !isHex(req.Username) {
		writeError(w, http.StatusBadRequest, "that isn't a valid username")
		return
	}
	// Network first, then the record, for the same reason as a report decision.
	if err := s.ban(req.Username, publicReasonOperator); err != nil {
		writeError(w, http.StatusBadGateway, "couldn't publish the ban to the network: "+err.Error())
		return
	}
	_ = s.store.Update(func(d *fileData) error {
		if a := d.byUsername(req.Username); a != nil {
			a.Banned, a.BannedAt, a.BanReason = true, s.now().Unix(), banReasonFor(req.Note)
		}
		return nil
	})
	s.store.Audit("ban", map[string]any{"username": req.Username, "email": req.Email, "by": admin.Email, "note": req.Note})
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) handleAdminUnban(w http.ResponseWriter, r *http.Request, admin Principal) {
	var req unbanReq
	if !readJSON(w, r, &req) {
		return
	}
	req.Username = strings.ToLower(strings.TrimSpace(req.Username))
	if len(req.Username) != 64 || !isHex(req.Username) {
		writeError(w, http.StatusBadRequest, "that isn't a valid username")
		return
	}
	s.bannerMu.Lock()
	err := s.banner.Unban(req.Username)
	s.bannerMu.Unlock()
	if err != nil {
		writeError(w, http.StatusBadGateway, "couldn't lift the ban on the network: "+err.Error())
		return
	}
	_ = s.store.Update(func(d *fileData) error {
		if a := d.byUsername(req.Username); a != nil {
			a.Banned, a.BanReason, a.BannedAt = false, "", 0
			a.ClearedAt = s.now().Unix() // earlier reports no longer count
		}
		// If the ban was the automatic kind, a person has now said it was
		// wrong: each of the people whose reports caused it gets a strike. Two
		// and their reports stop banning anyone by themselves.
		struck := map[string]bool{}
		for _, rep := range d.Reports {
			if rep.ReportedUsername == req.Username && rep.Status == "banned" && rep.DecidedBy == autoDecider {
				rep.Status = "overturned"
				if !struck[rep.ReporterEmail] {
					struck[rep.ReporterEmail] = true
					if r := d.Accounts[rep.ReporterEmail]; r != nil {
						r.Strikes++
					}
				}
			}
		}
		return nil
	})
	s.store.Audit("unban", map[string]any{"username": req.Username, "by": admin.Email})
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) ban(username, reason string) error {
	s.bannerMu.Lock()
	defer s.bannerMu.Unlock()
	return s.banner.Ban(username, reason)
}

// ─── helpers ───

func (s *Server) fresh(ts int64) bool {
	d := s.now().Sub(time.Unix(ts, 0))
	return d < maxSkew && d > -maxSkew
}

func banReasonFor(note string) string {
	if note == "" {
		return "Violation of the AltNet Terms of Service"
	}
	return note
}

func banMessage(a *Account) string {
	return "this account has been banned from AltNet Relay: " + banReasonFor(a.BanReason) +
		". You can appeal at abuse.report@panmox.org"
}

func isHex(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// httpError lets a handler abort an Update with the right status code.
type httpError struct {
	status int
	msg    string
	code   string // optional, for the app to tell refusals apart; msg is for people
}

func (e *httpError) Error() string { return e.msg }

func errConflict(m string) error { return &httpError{status: http.StatusConflict, msg: m} }
func errNotFound(m string) error { return &httpError{status: http.StatusNotFound, msg: m} }
func errTooMany(m string) error  { return &httpError{status: http.StatusTooManyRequests, msg: m} }
func errBanned(a *Account) error { return &httpError{status: http.StatusForbidden, msg: banMessage(a)} }

// Codes the app acts on. Both are "conflict", and they call for different
// things: another account owns these keys (this account needs keys of its
// own), or this account already has other keys (bring them, or start fresh).
const (
	CodeIdentityTaken  = "identity_taken"
	CodeAccountHasKeys = "account_has_keys"
)

// fail writes an error response and reports true if err is non-nil.
func (s *Server) fail(w http.ResponseWriter, err error) bool {
	if err == nil {
		return false
	}
	var he *httpError
	if errors.As(err, &he) {
		if he.code != "" {
			writeJSON(w, he.status, map[string]string{"error": he.msg, "code": he.code})
		} else {
			writeError(w, he.status, he.msg)
		}
		return true
	}
	s.log.Error("request failed", "err", err)
	writeError(w, http.StatusInternalServerError, "something went wrong on our side")
	return true
}

func readJSON(w http.ResponseWriter, r *http.Request, dst any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxBody)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		writeError(w, http.StatusBadRequest, "couldn't read that request")
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

// ─── the account check ───

// Principal is who a sign-in token belongs to.
type Principal struct {
	Email string
	Admin bool
}

var (
	// ErrUnauthorized: the token isn't a live sign-in.
	ErrUnauthorized = errors.New("your sign-in has expired; please sign in again")
	// ErrSuspended: the account exists but Studio has suspended it.
	ErrSuspended = errors.New("this account has been suspended; appeal at abuse.report@panmox.org")
)

// Verifier turns a bearer token into an account, or an error.
type Verifier interface {
	Verify(ctx context.Context, token string) (Principal, error)
}

// BackendVerifier asks the Studio accounts backend. It never sees a
// password -- only tokens the backend itself issued -- and remembers a
// positive answer briefly so a burst of requests isn't a burst of lookups.
type BackendVerifier struct {
	BaseURL string
	HTTP    *http.Client
	TTL     time.Duration // 0 means 60s

	mu      sync.Mutex
	cache   map[[32]byte]cachedPrincipal
	refused map[[32]byte]time.Time // tokens the backend said no to, until when
	budget  lookupBudget           // see limits.go
}

type cachedPrincipal struct {
	p       Principal
	expires time.Time
}

func (v *BackendVerifier) Verify(ctx context.Context, token string) (Principal, error) {
	key := sha256.Sum256([]byte(token))
	ttl := v.TTL
	if ttl == 0 {
		ttl = 60 * time.Second
	}
	now := time.Now()
	v.mu.Lock()
	if c, ok := v.cache[key]; ok && now.Before(c.expires) {
		v.mu.Unlock()
		return c.p, nil
	}
	if until, ok := v.refused[key]; ok && now.Before(until) {
		v.mu.Unlock()
		return Principal{}, ErrUnauthorized
	}
	if !v.budget.take(now) {
		v.mu.Unlock()
		return Principal{}, errLookupsBusy
	}
	v.mu.Unlock()

	client := v.HTTP
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(v.BaseURL, "/")+"/v1/me", nil)
	if err != nil {
		return Principal{}, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := client.Do(req)
	if err != nil {
		return Principal{}, err
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusUnauthorized:
		v.remember(key)
		return Principal{}, ErrUnauthorized
	case http.StatusForbidden:
		return Principal{}, ErrSuspended
	default:
		return Principal{}, fmt.Errorf("accounts backend answered %d", resp.StatusCode)
	}
	var me struct {
		Email    string `json:"email"`
		Verified bool   `json:"verified"`
		Admin    bool   `json:"is_admin"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxBody)).Decode(&me); err != nil {
		return Principal{}, err
	}
	if me.Email == "" || !me.Verified {
		v.remember(key)
		return Principal{}, ErrUnauthorized
	}
	p := Principal{Email: strings.ToLower(me.Email), Admin: me.Admin}

	v.mu.Lock()
	if v.cache == nil || len(v.cache) > 4096 {
		v.cache = map[[32]byte]cachedPrincipal{}
	}
	v.cache[key] = cachedPrincipal{p: p, expires: time.Now().Add(ttl)}
	v.mu.Unlock()
	return p, nil
}

// remember notes a token the backend refused, so asking again with it costs
// nothing for a while.
func (v *BackendVerifier) remember(key [32]byte) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.refused == nil || len(v.refused) > maxRemembered {
		v.refused = map[[32]byte]time.Time{}
	}
	v.refused[key] = time.Now().Add(refusedFor)
}
