package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/wailsapp/wails/v2/pkg/runtime"

	"altnet-relay/account"
	"altnet-relay/engine"
	"altnet-relay/state"
)

// App is what the frontend sees as window.go.main.App. Every method is a
// thin translation between the engine and the JSON shapes main.js
// expects; product rules live in the engine, not here, so the desktop app
// and the terminal client can't drift apart.
type App struct {
	ctx context.Context
	cfg engine.Config

	mu      sync.Mutex
	e       *engine.Engine
	openErr error

	upd updates // see update.go

	// Notifications are initialized on first use, not at startup. On
	// Windows, initializing registers the app with the notification
	// system (a per-user registry entry, no admin rights), and there's no
	// reason to do that for someone who never gets a message while the
	// window is in the background.
	notifyOnce sync.Once
	notifyErr  error
	notifyOn   bool

	bg background // running without a window; see background.go
}

func NewApp(cfg engine.Config) *App { return &App{cfg: cfg} }

// ─── views sent to the frontend ───

type MeView struct {
	Handle   string `json:"handle"`
	Username string `json:"username"`
	Online   bool   `json:"online"`
	Peers    int    `json:"peers"`
}

type ConvView struct {
	PeerID   string `json:"peerId"`
	Name     string `json:"name"`
	Handle   string `json:"handle"`
	Accepted bool   `json:"accepted"`
	Request  bool   `json:"request"`
	Blocked  bool   `json:"blocked"`
	Banned   bool   `json:"banned"`
	Unread   int    `json:"unread"`
	Preview  string `json:"preview"`
	LastAt   int64  `json:"lastAt"`

	// Groups share this view. PeerID is then "g:" + the group's ID.
	Group   bool   `json:"group"`
	Members int    `json:"members"`
	Admin   bool   `json:"admin"`  // we are the group's admin
	Closed  bool   `json:"closed"` // we left or were removed; history only
	Status  string `json:"status"`

	// Business. Kind is "workspace" (its own chat is #general) or "channel";
	// WS is a channel's workspace as "g:"+id. Biz marks a coworker's direct
	// chat, which lives in Business and stays out of the Personal list.
	Kind   string `json:"kind,omitempty"`
	WS     string `json:"ws,omitempty"`
	Biz    bool   `json:"biz,omitempty"`
	Former bool   `json:"former,omitempty"` // a Business chat with someone who is no longer a coworker
	// Coworker: they have accepted a workspace we're in, so they can't be
	// blocked or reported; whoever runs the workspace decides who stays.
	Coworker bool `json:"coworker,omitempty"`
	// InvitedBy is, for an invitation, the username of who sent it.
	InvitedBy string `json:"invitedBy,omitempty"`
	// Reports is how many reports are waiting, for a workspace we run.
	Reports int `json:"reports,omitempty"`
	Pending int `json:"pending,omitempty"` // of Members, invited and not answered yet
}

// groupPrefix marks a group in the IDs the window uses for conversations.
const groupPrefix = "g:"

func groupID(peerID string) (string, bool) {
	if strings.HasPrefix(peerID, groupPrefix) {
		return strings.TrimPrefix(peerID, groupPrefix), true
	}
	return "", false
}

type MemberView struct {
	PeerID  string `json:"peerId"`
	Name    string `json:"name"`
	Admin   bool   `json:"admin"`
	Me      bool   `json:"me"`
	Pending bool   `json:"pending,omitempty"` // invited to a workspace, hasn't said yes
}

type MsgView struct {
	ID        string `json:"id"`
	Outbound  bool   `json:"outbound"`
	Text      string `json:"text"`
	Sent      int64  `json:"sent"`
	Delivered bool   `json:"delivered"`
	From      string `json:"from,omitempty"`   // in a group: who wrote it
	System    bool   `json:"system,omitempty"` // in a group: a note about the group
	Reached   int    `json:"reached"`          // in a group, for ours: how many members confirmed it
	Of        int    `json:"of"`               // ...out of how many
}

type deliveredEvent struct {
	PeerID string   `json:"peerId"`
	IDs    []string `json:"ids"`
}

type messageEvent struct {
	PeerID   string    `json:"peerId"`
	Messages []MsgView `json:"messages"`
}

type callEvent struct {
	PeerID    string `json:"peerId"`
	Name      string `json:"name"`
	Group     string `json:"group,omitempty"` // "g:"+id for a group call
	GroupName string `json:"groupName,omitempty"`
	Op        string `json:"op"`
	CallID    string `json:"callId"`
	SDP       string `json:"sdp"`
	Video     bool   `json:"video"`
}

type statusEvent struct {
	Online bool `json:"online"`
	Peers  int  `json:"peers"`
}

// AccountView is the sign-in state. Gate is one of "off" (no accounts, a
// development network), "signed_out", "ok" or "banned"; the window shows the
// matching screen.
type AccountView struct {
	Gate    string `json:"gate"`
	Email   string `json:"email"`
	Handle  string `json:"handle"`
	Reason  string `json:"reason"`
	Offline bool   `json:"offline"`
}

// ─── lifecycle ───

// config decides where data lives and how the node reaches the network.
//
// RELAY_DATA and RELAY_BOOTSTRAP override the defaults. The data override
// is what lets two copies run side by side on one machine for testing.
//
// Accounts are on for a normal install and off under RELAY_NETWORK=local, so
// the test copies never touch the real account service. RELAY_STUDIO_URL and
// RELAY_SERVER_URL point the app at other services, and setting
// RELAY_SERVER_URL turns accounts on even for a local network -- how the
// sign-in screens are tried against a test server.
func config() engine.Config {
	dir := os.Getenv("RELAY_DATA")
	if dir == "" {
		base, err := os.UserConfigDir()
		if err != nil {
			base = "."
		}
		dir = filepath.Join(base, "AltNetRelay")
	}
	listen := os.Getenv("RELAY_LISTEN")
	if listen == "" {
		listen = "0.0.0.0:0"
	}
	operators := envList("RELAY_OPERATORS")
	if len(operators) == 0 {
		operators = engine.DefaultOperators
	}

	// A normal install joins the real AltNet through the seed with no
	// setup. RELAY_NETWORK=local switches that off, which is how two test
	// copies can talk to each other on one machine without writing
	// anything to the live network; RELAY_BOOTSTRAP / RELAY_RELAYS
	// override the addresses.
	boots, relays := envList("RELAY_BOOTSTRAP"), envList("RELAY_RELAYS")
	if os.Getenv("RELAY_NETWORK") != "local" {
		if len(boots) == 0 {
			boots = engine.DefaultBootstrap
		}
		if len(relays) == 0 {
			relays = engine.DefaultRelays
		}
	}
	cfg := engine.Config{
		DataDir:   dir,
		Listen:    listen,
		Bootstrap: boots,
		Relays:    relays,
		Operators: operators,
	}
	if os.Getenv("RELAY_NETWORK") != "local" || os.Getenv("RELAY_SERVER_URL") != "" {
		cfg.Accounts = account.New(os.Getenv("RELAY_STUDIO_URL"), os.Getenv("RELAY_SERVER_URL"))
		cfg.HandleAuthorities = envList("RELAY_AUTHORITY")
		if len(cfg.HandleAuthorities) == 0 {
			cfg.HandleAuthorities = engine.DefaultOperators
		}
	}
	return cfg
}

func envList(name string) []string {
	var out []string
	for _, v := range strings.Split(os.Getenv(name), ",") {
		if v = strings.TrimSpace(v); v != "" {
			out = append(out, v)
		}
	}
	return out
}

// startup opens the engine (fast, local) and joins the network in the
// background, so the window paints immediately instead of waiting on a
// bootstrap peer that may be slow or unreachable.
func (a *App) startup(ctx context.Context) {
	a.ctx = ctx
	a.bg.settings = loadSettings(a.cfg.DataDir)
	a.firstStartDefaults()
	go a.quitOnSignals()
	e, err := engine.Open(a.cfg)
	if err != nil {
		a.openErr = err
		return
	}
	e.OnMessages = a.onMessages
	e.OnRequest = a.onRequest
	e.OnCall = a.onCall
	e.OnGroup = func(id string) {
		runtime.EventsEmit(a.ctx, "group", map[string]string{"peerId": groupPrefix + id})
	}
	e.OnGroupMessage = func(id string, m state.GroupMessage) {
		runtime.EventsEmit(a.ctx, "message", messageEvent{PeerID: groupPrefix + id, Messages: []MsgView{a.groupMsgView(m)}})
		a.alertGroupMessage(id)
	}
	e.OnInvited = a.alertInvited
	e.OnAdded = a.alertAdded
	e.OnReport = a.alertReport
	e.OnGroupTyping = func(id, from string) {
		runtime.EventsEmit(a.ctx, "typing", map[string]string{"peerId": groupPrefix + id, "who": e.Name(from)})
	}
	e.OnGroupDelivered = func(id string, ids []string) {
		runtime.EventsEmit(a.ctx, "delivered", deliveredEvent{PeerID: groupPrefix + id, IDs: ids})
	}
	e.OnTyping = func(peerID string) {
		runtime.EventsEmit(a.ctx, "typing", map[string]string{"peerId": peerID})
	}
	e.OnDelivered = func(peerID string, ids []string) {
		runtime.EventsEmit(a.ctx, "delivered", deliveredEvent{PeerID: peerID, IDs: ids})
	}
	a.mu.Lock()
	a.e = e
	a.mu.Unlock()

	go func() {
		for _, w := range e.Start() {
			runtime.LogWarning(ctx, w)
		}
	}()
	go a.watchStatus()
	a.startTray()
	go a.updateLoop()
}

// startTray puts the icon in the notification area, so a window that was
// closed (and is only hidden) can be brought back and the app quit properly.
// If it can't be made, closing the window minimises it instead.
func (a *App) startTray() {
	stop, err := startTray(trayHandlers{
		open: a.showWindow,
		quit: a.Quit,
		sessEnd: func() {
			a.bg.quitting.Store(true) // signing out: let the window close
		},
	})
	if err != nil {
		runtime.LogWarning(a.ctx, "no tray icon: "+err.Error())
		return
	}
	a.bg.trayStop = stop
	a.bg.trayOK.Store(true)
}

func (a *App) shutdown(context.Context) {
	a.bg.quitting.Store(true)
	if a.bg.trayStop != nil {
		a.bg.trayStop()
	}
	if a.notifyOn {
		runtime.CleanupNotifications(a.ctx)
	}
	if e := a.engine(); e != nil {
		e.Close()
	}
	if a.bg.wipe.Load() {
		wipeDataDir(a.cfg.DataDir)
	}
}

func (a *App) engine() *engine.Engine {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.e
}

// watchStatus tells the frontend when connectivity changes, so the dot
// beside the wordmark is an honest signal rather than decoration.
func (a *App) watchStatus() {
	last := -1
	var lastAcct AccountView
	for range time.Tick(3 * time.Second) {
		e := a.engine()
		if e == nil {
			continue
		}
		if n := e.Peers(); n != last {
			last = n
			runtime.EventsEmit(a.ctx, "status", statusEvent{Online: n > 0, Peers: n})
		}
		// The account can change without the person doing anything: a session
		// that expires, or a ban that lands. Tell the window so it swaps screens.
		if v := accountView(e.Account()); v != lastAcct {
			lastAcct = v
			runtime.EventsEmit(a.ctx, "account", v)
		}
	}
}

func (a *App) onMessages(peerID string, msgs []state.StoredMessage) {
	views := make([]MsgView, 0, len(msgs))
	for _, m := range msgs {
		views = append(views, msgView(m))
	}
	runtime.EventsEmit(a.ctx, "message", messageEvent{PeerID: peerID, Messages: views})
	go a.alertMessage(peerID)
}

// onCall hands a call-setup message to the window, which owns the call
// itself (the audio runs in the window's WebRTC). The name is added here
// because the window shouldn't have to look it up to say who is ringing.
func (a *App) onCall(sig engine.CallSignal) {
	name := sig.PeerID[:8]
	if e := a.engine(); e != nil {
		name = e.Name(sig.PeerID)
	}
	ev := callEvent{PeerID: sig.PeerID, Name: name, Op: sig.Op, CallID: sig.CallID, SDP: sig.SDP, Video: sig.Video}
	if sig.Group != "" {
		ev.Group = groupPrefix + sig.Group
		if e := a.engine(); e != nil {
			if g, ok := e.State.GroupMeta(sig.Group); ok {
				ev.GroupName = g.Name
			}
		}
	}
	runtime.EventsEmit(a.ctx, "call", ev)
	a.alertCall(sig)
}

func (a *App) onRequest(peerID string) {
	if e := a.engine(); e != nil {
		for _, s := range e.Conversations() {
			if s.PeerID == peerID {
				runtime.EventsEmit(a.ctx, "request", convView(s))
				go a.alertRequest(peerID)
				return
			}
		}
	}
}

// ─── bound methods (window.go.main.App.*) ───

func (a *App) ready() (*engine.Engine, error) {
	if a.openErr != nil {
		return nil, a.openErr
	}
	e := a.engine()
	if e == nil {
		return nil, errNotReady
	}
	return e, nil
}

func (a *App) Me() (MeView, error) {
	e, err := a.ready()
	if err != nil {
		return MeView{}, err
	}
	n := e.Peers()
	return MeView{Handle: e.MyHandle(), Username: e.Username(), Online: n > 0, Peers: n}, nil
}

func (a *App) Account() (AccountView, error) {
	e, err := a.ready()
	if err != nil {
		return AccountView{}, err
	}
	return accountView(e.Account()), nil
}

func (a *App) SignUp(email, password string) error {
	e, err := a.ready()
	if err != nil {
		return err
	}
	return e.SignUp(email, password)
}

// VerifyEmail confirms the emailed code, which also signs in. Like SignIn it
// answers "ok" or "restart": on a computer someone else used Relay on, the new
// account gets keys of its own, and the app restarts once to load them.
func (a *App) VerifyEmail(email, code string) (string, error) {
	e, err := a.ready()
	if err != nil {
		return "", err
	}
	err = e.VerifyEmail(email, code)
	if errors.Is(err, engine.ErrRestart) {
		go func() {
			time.Sleep(1500 * time.Millisecond) // let the window say what's happening
			a.restart()
		}()
		return "restart", nil
	}
	if err != nil {
		return "", err
	}
	return "ok", nil
}

func (a *App) SignOut() error {
	e, err := a.ready()
	if err != nil {
		return err
	}
	return e.SignOut()
}

func (a *App) RequestReset(email string) error {
	e, err := a.ready()
	if err != nil {
		return err
	}
	return e.RequestPasswordReset(email)
}

func (a *App) ConfirmReset(email, code, password string) error {
	e, err := a.ready()
	if err != nil {
		return err
	}
	return e.ConfirmPasswordReset(email, code, password)
}

// StartAccountDeletion emails the code that begins deleting the account.
func (a *App) StartAccountDeletion() error {
	e, err := a.ready()
	if err != nil {
		return err
	}
	return e.StartAccountDeletion()
}

// CheckAccountDeletion checks the deletion steps done so far.
func (a *App) CheckAccountDeletion(code, password, name string) error {
	e, err := a.ready()
	if err != nil {
		return err
	}
	return e.CheckAccountDeletion(code, password, name)
}

// DeleteAccount deletes the account for good, then this computer's copy of
// everything -- keys, conversations, settings, starting with the computer --
// as the app quits.
func (a *App) DeleteAccount(code, password, name string) error {
	e, err := a.ready()
	if err != nil {
		return err
	}
	if err := e.DeleteAccount(code, password, name); err != nil {
		return err
	}
	_ = setStartup(false)
	a.bg.wipe.Store(true)
	return nil
}

// Report sends a report about a user. attach is how many of the latest
// messages in the conversation the person chose to include.
func (a *App) Report(peerID, reason string, attach int) (string, error) {
	e, err := a.ready()
	if err != nil {
		return "", err
	}
	return e.Report(peerID, reason, attach)
}

func (a *App) Claim(handle string) (string, error) {
	e, err := a.ready()
	if err != nil {
		return "", err
	}
	return e.Claim(handle)
}

func (a *App) Conversations() ([]ConvView, error) {
	e, err := a.ready()
	if err != nil {
		return nil, err
	}
	list := e.Conversations()
	out := make([]ConvView, 0, len(list))
	for _, s := range list {
		out = append(out, convView(s))
	}
	for _, g := range e.Groups() {
		v := ConvView{
			PeerID: groupPrefix + g.ID, Name: g.Name, Group: true, Members: g.Members,
			Admin: g.Admin, Unread: g.Unread, Preview: g.Preview, LastAt: g.LastAt, Status: g.Status,
			Accepted: g.Status == state.GroupJoined,
			Request:  g.Status == state.GroupInvited,
			Closed:   g.Status == state.GroupLeft || g.Status == state.GroupRemoved,
			Kind:     g.Kind,
			Pending:  g.Pending,
			Reports:  g.Reports,
		}
		if g.Status == state.GroupInvited {
			v.InvitedBy = g.InvitedByID
		}
		if g.WS != "" {
			v.WS = groupPrefix + g.WS
		}
		if g.LastOutbound && v.Preview != "" {
			v.Preview = "You: " + v.Preview
		}
		if v.Request {
			v.Preview = g.InvitedBy + " invited you"
		}
		out = append(out, v)
	}
	return out, nil
}

func (a *App) Thread(peerID string) ([]MsgView, error) {
	e, err := a.ready()
	if err != nil {
		return nil, err
	}
	if id, ok := groupID(peerID); ok {
		msgs := e.GroupThread(id)
		out := make([]MsgView, 0, len(msgs))
		for _, m := range msgs {
			out = append(out, a.groupMsgView(m))
		}
		// "Delivered to N of M": M is everyone else who has accepted. Someone
		// still deciding can't confirm anything yet.
		if members, pending, ok := e.GroupSize(id); ok {
			for i := range out {
				if out[i].Outbound {
					out[i].Of = members - pending - 1
				}
			}
		}
		return out, nil
	}
	thread := e.Thread(peerID)
	out := make([]MsgView, 0, len(thread))
	for _, m := range thread {
		out = append(out, msgView(m))
	}
	return out, nil
}

func (a *App) Add(who string) (ConvView, error) {
	e, err := a.ready()
	if err != nil {
		return ConvView{}, err
	}
	id, err := e.Add(who, "")
	if err != nil {
		return ConvView{}, err
	}
	for _, s := range e.Conversations() {
		if s.PeerID == id {
			return convView(s), nil
		}
	}
	return ConvView{PeerID: id, Name: e.Name(id), Accepted: true}, nil
}

func (a *App) Send(peerID, text string) (MsgView, error) {
	e, err := a.ready()
	if err != nil {
		return MsgView{}, err
	}
	if id, ok := groupID(peerID); ok {
		m, err := e.SendGroup(id, text)
		if err != nil {
			return MsgView{}, err
		}
		return a.groupMsgView(m), nil
	}
	m, err := e.Send(peerID, text)
	if err != nil {
		return MsgView{}, err
	}
	return msgView(m), nil
}

func (a *App) Accept(peerID string) error {
	e, err := a.ready()
	if err != nil {
		return err
	}
	if id, ok := groupID(peerID); ok {
		return e.JoinGroup(id)
	}
	return e.Accept(peerID)
}

func (a *App) Decline(peerID string) error {
	e, err := a.ready()
	if err != nil {
		return err
	}
	if id, ok := groupID(peerID); ok {
		return e.DeclineGroup(id)
	}
	return e.Decline(peerID)
}

func (a *App) Block(peerID string) error {
	e, err := a.ready()
	if err != nil {
		return err
	}
	return e.Block(peerID)
}

func (a *App) Unblock(peerID string) error {
	e, err := a.ready()
	if err != nil {
		return err
	}
	return e.Unblock(peerID)
}

func (a *App) MarkRead(peerID string) error {
	e, err := a.ready()
	if err != nil {
		return err
	}
	if id, ok := groupID(peerID); ok {
		return e.MarkGroupRead(id)
	}
	return e.MarkRead(peerID)
}

// Typing tells the open conversation's peer that the user is typing. The
// window throttles it; the engine only passes it on to accepted contacts.
func (a *App) Typing(peerID string) {
	e := a.engine()
	if e == nil {
		return
	}
	if id, ok := groupID(peerID); ok {
		e.TypingGroup(id)
		return
	}
	e.Typing(peerID)
}

// RenameGroup gives a group a new name (admin only).
func (a *App) RenameGroup(peerID, name string) error {
	e, err := a.ready()
	if err != nil {
		return err
	}
	id, _ := groupID(peerID)
	return e.RenameGroup(id, name)
}

// SendGroupCall carries one group-call setup message: to one member (to set),
// or to everyone in the group (to empty).
func (a *App) SendGroupCall(groupPeerID, to, op, callID, sdp string, video bool) error {
	e, err := a.ready()
	if err != nil {
		return err
	}
	id, _ := groupID(groupPeerID)
	return e.SendGroupCall(engine.CallSignal{Group: id, PeerID: to, Op: op, CallID: callID, SDP: sdp, Video: video})
}

// SendCall carries one call-setup message (offer, answer, decline, busy,
// end) to a contact. video marks an offer as a video call. The audio and
// video never go through here.
func (a *App) SendCall(peerID, op, callID, sdp string, video bool) error {
	e, err := a.ready()
	if err != nil {
		return err
	}
	return e.SendCall(engine.CallSignal{PeerID: peerID, Op: op, CallID: callID, SDP: sdp, Video: video})
}

// WarmCalls gets call-relay credentials ready before anyone presses Call.
func (a *App) WarmCalls() {
	if e := a.engine(); e != nil {
		e.WarmCalls()
	}
}

// CallServers returns the STUN/TURN servers a call should use.
func (a *App) CallServers() ([]account.IceServer, error) {
	e, err := a.ready()
	if err != nil {
		return nil, err
	}
	return e.CallServers(), nil
}

// ─── groups ───

// CreateGroup starts a group. who is names or IDs, separated by commas,
// spaces or new lines.
func (a *App) CreateGroup(name, who string) (ConvView, error) {
	e, err := a.ready()
	if err != nil {
		return ConvView{}, err
	}
	id, err := e.CreateGroup(name, splitPeople(who))
	if err != nil {
		return ConvView{}, err
	}
	return ConvView{PeerID: groupPrefix + id, Name: strings.TrimSpace(name), Group: true, Accepted: true, Admin: true, Status: state.GroupJoined}, nil
}

// CreateWorkspace starts a company workspace and invites the given people
// (names or IDs). Business mode.
func (a *App) CreateWorkspace(name, who string) (ConvView, error) {
	e, err := a.ready()
	if err != nil {
		return ConvView{}, err
	}
	id, err := e.CreateWorkspace(name, splitPeople(who))
	if err != nil {
		return ConvView{}, err
	}
	return ConvView{PeerID: groupPrefix + id, Name: strings.TrimSpace(name), Group: true, Accepted: true, Admin: true,
		Status: state.GroupJoined, Kind: state.KindWorkspace}, nil
}

// CreateChannel starts a channel in a workspace (workspacePeerID is its
// "g:" ID) with the given workspace members.
func (a *App) CreateChannel(workspacePeerID, name, who string) (ConvView, error) {
	e, err := a.ready()
	if err != nil {
		return ConvView{}, err
	}
	wsID, _ := groupID(workspacePeerID)
	id, err := e.CreateChannel(wsID, name, splitPeople(who))
	if err != nil {
		return ConvView{}, err
	}
	return ConvView{PeerID: groupPrefix + id, Name: strings.TrimSpace(name), Group: true, Accepted: true, Admin: true,
		Status: state.GroupJoined, Kind: state.KindChannel, WS: workspacePeerID}, nil
}

// ─── reporting a coworker to the workspace's owner ───

// EvidenceView is one message that came with a report.
type EvidenceView struct {
	Text  string `json:"text"`
	Sent  int64  `json:"sent"`
	Where string `json:"where,omitempty"` // "direct" or "general"
}

// ReportView is one report waiting for the owner of a workspace.
type ReportView struct {
	ID       string         `json:"id"`
	From     string         `json:"from"`
	Who      string         `json:"who"`
	WhoID    string         `json:"whoId"`
	Reason   string         `json:"reason"`
	Evidence []EvidenceView `json:"evidence"`
	At       int64          `json:"at"`
}

// ReportCoworker reports a member of a workspace (its "g:" ID) to the
// workspace's owner.
func (a *App) ReportCoworker(workspacePeerID, peerID, reason string, attach bool) error {
	e, err := a.ready()
	if err != nil {
		return err
	}
	ws, _ := groupID(workspacePeerID)
	return e.ReportCoworker(ws, peerID, reason, attach)
}

// WorkspaceReports lists the reports waiting for us, as a workspace's owner.
func (a *App) WorkspaceReports(workspacePeerID string) ([]ReportView, error) {
	e, err := a.ready()
	if err != nil {
		return nil, err
	}
	ws, _ := groupID(workspacePeerID)
	list, err := e.WorkspaceReports(ws)
	if err != nil {
		return nil, err
	}
	out := make([]ReportView, 0, len(list))
	for _, r := range list {
		v := ReportView{ID: r.ID, From: r.FromName, Who: r.WhoName, WhoID: r.WhoID, Reason: r.Reason, At: r.At, Evidence: []EvidenceView{}}
		for _, l := range r.Evidence {
			v.Evidence = append(v.Evidence, EvidenceView{Text: l.Text, Sent: l.Sent, Where: l.Where})
		}
		out = append(out, v)
	}
	return out, nil
}

// DismissReport marks a report as dealt with, removing nobody.
func (a *App) DismissReport(workspacePeerID, id string) error {
	e, err := a.ready()
	if err != nil {
		return err
	}
	ws, _ := groupID(workspacePeerID)
	return e.DismissReport(ws, id)
}

func (a *App) LeaveGroup(peerID string) error {
	e, err := a.ready()
	if err != nil {
		return err
	}
	id, _ := groupID(peerID)
	return e.LeaveGroup(id)
}

func (a *App) GroupMembers(peerID string) ([]MemberView, error) {
	e, err := a.ready()
	if err != nil {
		return nil, err
	}
	id, _ := groupID(peerID)
	ms, err := e.GroupMembers(id)
	if err != nil {
		return nil, err
	}
	out := make([]MemberView, 0, len(ms))
	for _, m := range ms {
		out = append(out, MemberView{PeerID: m.PeerID, Name: m.Name, Admin: m.Admin, Me: m.Me, Pending: m.Pending})
	}
	return out, nil
}

func (a *App) AddToGroup(peerID, who string) error {
	e, err := a.ready()
	if err != nil {
		return err
	}
	id, _ := groupID(peerID)
	return e.AddToGroup(id, splitPeople(who))
}

func (a *App) RemoveFromGroup(peerID, member string) error {
	e, err := a.ready()
	if err != nil {
		return err
	}
	id, _ := groupID(peerID)
	return e.RemoveFromGroup(id, member)
}

func splitPeople(s string) []string {
	return strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == ';' || r == ' ' || r == '\n' || r == '\t' })
}

func (a *App) groupMsgView(m state.GroupMessage) MsgView {
	v := MsgView{ID: m.ID, Outbound: m.Outbound, Text: m.Text, Sent: m.Sent, System: m.System, Reached: len(m.DeliveredTo)}
	if !m.Outbound && !m.System && m.From != "" {
		if e := a.engine(); e != nil {
			v.From = e.Name(m.From)
		}
	}
	return v
}

// ─── conversions ───

func convView(s engine.Summary) ConvView {
	preview := s.Preview
	if preview != "" && s.LastOutbound {
		preview = "You: " + preview
	}
	return ConvView{
		PeerID: s.PeerID, Name: s.Name, Handle: s.Handle,
		Accepted: s.Accepted, Request: s.Request,
		Blocked: s.Blocked, Banned: s.Banned, Unread: s.Unread,
		Preview: preview, LastAt: s.LastAt, Biz: s.Biz, Former: s.Former, Coworker: s.Coworker,
	}
}

func accountView(s engine.AccountStatus) AccountView {
	return AccountView{Gate: string(s.Gate), Email: s.Email, Handle: s.Handle, Reason: s.Reason, Offline: s.Offline}
}

func msgView(m state.StoredMessage) MsgView {
	return MsgView{ID: m.ID, Outbound: m.Outbound, Text: m.Text, Sent: m.Sent, Delivered: m.Delivered}
}
