/* AltNet Relay — desktop frontend.
 *
 * Talks to Go through the globals Wails injects at runtime
 * (window.go.main.App and window.runtime) instead of the generated import
 * modules. That means no bundler and no npm step: these are plain files
 * embedded into the binary exactly as they sit on disk.
 *
 * Opened in an ordinary browser (no Wails present), it runs against a
 * mock backend with sample data, so the interface can be designed and
 * reviewed without building the app at all.
 *
 * SECURITY: message text, names and handles all come from other people,
 * strangers included. Everything user-supplied is inserted with
 * textContent, never innerHTML. Inside Wails, injected script could call
 * the Go bindings directly -- read every conversation, send as the user
 * -- so this is a hard rule, not a style preference. The only innerHTML
 * in this file sets static, hard-coded markup (icons).
 */
(function () {
'use strict';

/* ───────────────────────────── backend ───────────────────────────── */

const backend = (window.go && window.go.main && window.go.main.App)
    ? wailsBackend(window.go.main.App, window.runtime)
    : mockBackend();

function wailsBackend(App, rt) {
    return {
        live: true,
        me:            ()         => App.Me(),
        claim:         (h)        => App.Claim(h),
        conversations: ()         => App.Conversations(),
        thread:        (id)       => App.Thread(id),
        add:           (who)      => App.Add(who),
        send:          (id, text) => App.Send(id, text),
        accept:        (id)       => App.Accept(id),
        decline:       (id)       => App.Decline(id),
        block:         (id)       => App.Block(id),
        unblock:       (id)       => App.Unblock(id),
        markRead:      (id)       => App.MarkRead(id),
        settings:      ()         => App.Settings(),
        setSetting:    (name, on) => App.SetSetting(name, !!on),
        setFocused:    (on)       => App.SetFocused(!!on),
        testNotification: ()      => App.TestNotification(),
        quit:          ()         => App.Quit(),
        account:       ()         => App.Account(),
        signUp:        (e, p)     => App.SignUp(e, p),
        verifyEmail:   (e, c)     => App.VerifyEmail(e, c),
        signIn:        (e, p)     => App.SignIn(e, p),
        startFresh:    (e, p)     => App.StartFresh(e, p),
        updateStatus:  ()         => App.UpdateStatus(),
        installUpdate: ()         => App.InstallUpdate(),
        openUpdatePage: ()        => App.OpenUpdatePage(),
        setBusy:       (on)       => App.SetBusy(!!on),
        copyStatus:    ()         => App.Copy(),
        setCopy:       (on)       => App.SetCopy(!!on),
        unlockCopy:    (p)        => App.UnlockCopy(p),
        signOut:       ()         => App.SignOut(),
        requestReset:  (e)        => App.RequestReset(e),
        confirmReset:  (e, c, p)  => App.ConfirmReset(e, c, p),
        report:        (id, why, n) => App.Report(id, why, n),
        startAccountDeletion: ()  => App.StartAccountDeletion(),
        checkAccountDeletion: (c, p, n) => App.CheckAccountDeletion(c, p, n),
        deleteAccount: (c, p, n)  => App.DeleteAccount(c, p, n),
        sendCall:      (id, op, callId, sdp, video) => App.SendCall(id, op, callId, sdp, !!video),
        callServers:   ()         => App.CallServers(),
        typing:        (id)       => App.Typing(id),
        warmCalls:     ()         => App.WarmCalls(),
        createGroup:   (name, who) => App.CreateGroup(name, who),
        leaveGroup:    (id)       => App.LeaveGroup(id),
        groupMembers:  (id)       => App.GroupMembers(id),
        addToGroup:    (id, who)  => App.AddToGroup(id, who),
        removeFromGroup: (id, m)  => App.RemoveFromGroup(id, m),
        renameGroup:   (id, name) => App.RenameGroup(id, name),
        createWorkspace: (name, who) => App.CreateWorkspace(name, who),
        reportCoworker: (ws, id, why, attach) => App.ReportCoworker(ws, id, why, !!attach),
        workspaceReports: (ws)    => App.WorkspaceReports(ws),
        dismissReport: (ws, id)   => App.DismissReport(ws, id),
        createChannel: (ws, name, who) => App.CreateChannel(ws, name, who),
        sendGroupCall: (g, to, op, callId, sdp, video) => App.SendGroupCall(g, to, op, callId, sdp, !!video),
        on:            (ev, fn)   => rt.EventsOn(ev, fn),
        copy:          (text)     => rt.ClipboardSetText(text),
    };
}

/* A believable fake world, so every screen state can be looked at in a
   browser: an ongoing chat, a quiet one, and a pending message request.
   Nothing here ships behaviour -- it only has to look right. */
function mockBackend() {
    const mockStart = Date.now();
    const now = Math.floor(Date.now() / 1000);
    const day = 86400;
    const listeners = {};
    const emit = (ev, data) => (listeners[ev] || []).forEach(fn => fn(data));
    const id = (c) => c.repeat(64);

    const me = { handle: 'rob', username: '99e12e96cfbbd886166afb16a4323e5b5363afac7b801591c508e4390b52fd41', online: true, peers: 4 };
    // ?call=group starts the mock with someone ringing in a group.
    try {
        if (new URLSearchParams(location.search).get('call') === 'group') {
            setTimeout(() => emit('call', { peerId: 'b'.repeat(64), name: 'bob', group: 'g:friday', groupName: 'Friday crew', op: 'ring', callId: 'gmock1' }), 1200);
        }
    } catch (_) {}

    const convs = {
        [id('b')]: {
            peerId: id('b'), name: 'bob', handle: 'bob', accepted: true, request: false, coworker: true,
            msgs: [
                { id: 'm1', outbound: false, text: 'hey — did the node come back up after the reboot?', sent: now - day - 3600 },
                { id: 'm2', outbound: true,  text: 'yep, seed is back. took about a minute to rejoin', sent: now - day - 3500 },
                { id: 'm3', outbound: false, text: 'nice', sent: now - day - 3490 },
                { id: 'm4', outbound: false, text: 'sending you the build later tonight', sent: now - 900 },
                { id: 'm5', outbound: true,  text: 'perfect, I\'ll test it on the laptop', sent: now - 600 },
            ],
        },
        [id('a')]: {
            peerId: id('a'), name: 'alice', handle: 'alice', accepted: true, request: false,
            msgs: [
                { id: 'a1', outbound: true,  text: 'thanks for the invite!', sent: now - 3 * day },
                { id: 'a2', outbound: false, text: 'anytime. see you thursday', sent: now - 3 * day + 120 },
            ],
        },
        [id('c')]: {
            peerId: id('c'), name: '@kate', handle: 'kate', accepted: false, request: true,
            msgs: [
                { id: 'k1', outbound: false, text: 'Hi Rob — Bob gave me your name. I run the Prague meetup and wanted to ask about AltNet.', sent: now - 1800 },
            ],
        },
    };

    // A group you're in, and an invitation to another.
    convs['g:friday'] = {
        peerId: 'g:friday', name: 'Friday crew', group: true, accepted: true, admin: true, members: 4,
        people: [{ peerId: 'me', name: 'You', me: true, admin: true }, { peerId: id('b'), name: 'bob' }, { peerId: id('a'), name: 'alice' }, { peerId: id('e'), name: 'eva' }],
        msgs: [
            { id: 'g0', system: true, text: 'You created the group', sent: now - day },
            { id: 'g1', outbound: false, from: 'bob', text: 'who is bringing the speaker?', sent: now - 5000 },
            { id: 'g2', outbound: false, from: 'eva', text: 'I can', sent: now - 4900 },
            { id: 'g3', outbound: true, text: 'great, see you at 8', sent: now - 4000 },
        ],
    };
    convs['g:inv'] = {
        peerId: 'g:inv', name: 'Prague meetup', group: true, request: true, members: 12, invitedBy: 'kate',
        msgs: [],
    };

    // Business: a workspace you own with two channels, a coworker you chat
    // with directly, and an invitation to a second workspace.
    const people = [
        { peerId: 'me', name: 'You', me: true, admin: true },
        { peerId: id('b'), name: 'bob' },
        { peerId: id('f'), name: 'dana' },
        { peerId: id('e'), name: 'eva', pending: true },
    ];
    convs['g:acme'] = {
        peerId: 'g:acme', name: 'Acme Studio', group: true, kind: 'workspace', accepted: true, admin: true, members: 4,
        people: people.map(p => ({ ...p })),
        reports: [{ id: 'r1', from: 'dana', who: 'bob', whoId: id('b'), reason: 'He keeps mocking my designs in front of the client.', at: now - 1800,
                    evidence: [{ text: 'that mockup looks like a child drew it', sent: now - 4000, where: 'general' },
                               { text: 'seriously, just quit', sent: now - 3900, where: 'direct' }] }],
        msgs: [
            { id: 'w0', system: true, text: 'You created the workspace', sent: now - 2 * day },
            { id: 'w1', outbound: true, text: 'welcome to Acme — this is where we talk shop', sent: now - 2 * day + 60 },
            { id: 'w2', outbound: false, from: 'dana', text: 'glad to be here. is the Friday demo still on?', sent: now - 7000 },
        ],
    };
    convs['g:acme-design'] = {
        peerId: 'g:acme-design', name: 'design', group: true, kind: 'channel', ws: 'g:acme', accepted: true, admin: false, members: 3,
        people: [people[0], people[1], people[2]].map(p => ({ ...p, admin: p.peerId === id('b') })),
        msgs: [
            { id: 'd0', system: true, text: 'bob added you', sent: now - day },
            { id: 'd1', outbound: false, from: 'bob', text: 'new landing page mockups are in the shared folder', sent: now - 3600 },
            { id: 'd2', outbound: false, from: 'dana', text: 'love the hero. can we try a darker header?', sent: now - 3000 },
        ],
        unread: 2,
    };
    convs['g:acme-launch'] = {
        peerId: 'g:acme-launch', name: 'launch', group: true, kind: 'channel', ws: 'g:acme', accepted: true, admin: true, members: 4,
        people: people.map(p => ({ ...p, pending: false })),
        msgs: [{ id: 'l0', system: true, text: 'You created the channel', sent: now - day }],
    };
    convs[id('f')] = {
        peerId: id('f'), name: 'dana', handle: 'dana', accepted: true, request: false, biz: true, coworker: true,
        msgs: [
            { id: 'f1', outbound: false, text: 'can you look at the invoice template before 3?', sent: now - 5400 },
            { id: 'f2', outbound: true, text: 'on it', sent: now - 5300 },
        ],
    };
    convs[id('9')] = {
        peerId: id('9'), name: 'greg', handle: 'greg', accepted: false, request: false, biz: true, former: true,
        msgs: [{ id: 'q1', outbound: false, text: 'handing the client over to dana from Monday', sent: now - 9 * day }],
    };
    convs['g:guild'] = {
        peerId: 'g:guild', name: 'Prague Dev Guild', group: true, kind: 'workspace', request: true, members: 3, invitedBy: 'kate', inviterId: id('c'),
        people: [{ peerId: 'me', name: 'You', me: true }, { peerId: id('c'), name: 'kate', admin: true }, { peerId: id('b'), name: 'bob' }],
        msgs: [],
    };

    // Alice starts with two unread, so the count is visible straight away.
    convs[id('a')].unread = 2;

    // An operator-banned account, so that state can be reviewed too.
    convs[id('d')] = {
        peerId: id('d'), name: 'dealz', handle: 'dealz', accepted: true, request: false, banned: true,
        msgs: [{ id: 'd1', outbound: false, text: 'limited offer, click now', sent: now - 5 * day }],
    };

    const view = (c) => {
        const last = c.msgs[c.msgs.length - 1];
        return {
            peerId: c.peerId, name: c.name, handle: c.handle,
            accepted: c.accepted, request: c.request,
            blocked: !!c.blocked, banned: !!c.banned, unread: c.unread || 0,
            preview: c.request && c.group ? c.invitedBy + ' invited you'
                : last ? (last.outbound ? 'You: ' : last.from ? last.from + ': ' : '') + last.text : '',
            lastAt: last ? last.sent : 0,
            group: !!c.group, members: c.members || 0, admin: !!c.admin, closed: !!c.closed,
            kind: c.kind || '', ws: c.ws || '', biz: !!c.biz, former: !!c.former,
            coworker: !!c.coworker, invitedBy: c.request ? (c.inviterId || '') : '',
            reports: (c.reports || []).filter(r => !r.done).length,
            status: c.closed ? (c.status || 'left') : c.request ? 'invited' : 'joined',
            pending: (c.people || []).filter(p => p.pending).length,
        };
    };

    // An incoming message a few seconds in, so arrival behaviour is visible.
    // Like the engine, it counts as unread until the UI marks it read.
    setTimeout(() => emit('typing', { peerId: id('b') }), 3200);
    setTimeout(() => {
        const c = convs[id('b')];
        if (c.blocked) return;
        const m = { id: 'm6', outbound: false, text: 'build is up — let me know how it runs', sent: Math.floor(Date.now() / 1000) };
        c.msgs.push(m);
        c.unread = (c.unread || 0) + 1;
        emit('message', { peerId: id('b'), messages: [m] });
    }, 5000);

    const delay = (v) => new Promise(r => setTimeout(() => r(v), 120));
    const fail = (msg) => new Promise((_, rej) => setTimeout(() => rej(new Error(msg)), 120));

    // ?gate=signed_out or ?gate=banned starts the mock on those screens, so
    // they can be looked at without a server. The sign-in code is 424242.
    const acct = { gate: 'ok', email: 'rob@example.com', handle: me.handle, reason: '', offline: false };
    try {
        const g = new URLSearchParams(location.search).get('gate');
        if (g === 'signed_out' || g === 'banned') acct.gate = g;
        if (g === 'banned') acct.reason = 'This account has been banned from AltNet Relay: Repeated harassment of other users.';
        if (g === 'signed_out') acct.email = '';
    } catch (_) {}
    try {
        const demo = new URLSearchParams(location.search).get('call');
        if (demo === 'incoming' || demo === 'video') {
            setTimeout(() => emit('call', { peerId: id('b'), name: 'bob', op: 'offer', callId: 'mockcall1', sdp: 'mock-offer', video: demo === 'video' }), 1200);
        }
    } catch (_) {}
    const signedIn = (email) => { acct.gate = 'ok'; acct.email = email; return delay(); };
    const copyState = { on: true, needsPassword: false, savedAt: Math.floor(Date.now() / 1000) - 120, problem: '' };

    return {
        live: false,
        // ?slow=8 pretends the app takes 8 seconds to open.
        account: () => {
            const slow = Number(new URLSearchParams(location.search).get('slow') || 0);
            return Date.now() - mockStart < slow * 1000
                ? fail('still starting up — try again in a moment') : delay({ ...acct });
        },
        signUp: (email, pw) => email.includes('taken') ? fail('email already registered') : delay(),
        // Sign up as someone@moving... to see a computer someone else used
        // Relay on: the new account gets its own keys after one restart.
        verifyEmail: (email, code) => code !== '424242' ? fail('invalid email or code')
            : /@moving/.test(email) ? delay('restart')
            : signedIn(email).then(() => 'ok'),
        // Sign in as someone@other... to see the "start fresh" screen, or
        // someone@moving... to see the account being brought here.
        signIn: (email, pw) => pw === 'wrong' ? fail('invalid credentials')
            : /@other/.test(email) ? delay('no_copy')
            : /@moving/.test(email) ? delay('restart')
            : signedIn(email).then(() => 'ok'),
        startFresh: (email) => signedIn(email),
        // ?update=1 shows the "update ready" banner.
        updateStatus: () => delay(new URLSearchParams(location.search).get('update')
            ? { version: '1.0.9', notes: 'Smoother calls.', page: 'https://github.com/panmauk/AltNet-Relay/releases', ready: true, install: '' } : null),
        installUpdate: () => delay(),
        openUpdatePage: () => delay(),
        setBusy: () => Promise.resolve(),
        copyStatus: () => delay({ ...copyState }),
        setCopy: (on) => { copyState.on = on; return delay(); },
        unlockCopy: (p) => p === 'wrong' ? fail("that password isn't right") : (copyState.needsPassword = false, delay()),
        signOut: () => { acct.gate = 'signed_out'; acct.email = ''; return delay(); },
        requestReset: () => delay(),
        confirmReset: (email, code) => code === '424242' ? delay() : fail('invalid code'),
        sendCall: (pid, op, callId, sdp, video) => {
            // A pretend friend: picks up 1.5 s after the phone starts ringing.
            if (op === 'offer') setTimeout(() => emit('call', { peerId: pid, name: (convs[pid] || {}).name, op: 'answer', callId, sdp: 'mock-answer' }), 1500);
            return delay();
        },
        callServers: () => delay([]),
        typing: () => Promise.resolve(),
        warmCalls: () => Promise.resolve(),
        createGroup: (name, who) => {
            const gid = 'g:' + Math.random().toString(16).slice(2);
            const people = who.split(/[\s,;]+/).filter(Boolean);
            convs[gid] = { peerId: gid, name, group: true, accepted: true, admin: true, members: people.length + 1,
                msgs: [{ id: 's' + Math.random(), system: true, text: 'You created the group', sent: Math.floor(Date.now() / 1000) }],
                people: [{ peerId: 'me', name: 'You', me: true, admin: true }].concat(people.map(p => ({ peerId: p, name: p.replace(/^@/, '') }))) };
            return delay(view(convs[gid]));
        },
        leaveGroup: (gid) => {
            convs[gid].closed = true; convs[gid].accepted = false;
            convs[gid].msgs.push({ id: 's' + Math.random(), system: true, text: 'You left', sent: Math.floor(Date.now() / 1000) });
            if (convs[gid].kind === 'workspace') Object.values(convs).forEach(c => { if (c.ws === gid) { c.closed = true; c.accepted = false; } });
            return delay();
        },
        createWorkspace: (name, who) => {
            const gid = 'g:' + Math.random().toString(16).slice(2);
            const invited = who.split(/[\s,;]+/).filter(Boolean);
            convs[gid] = { peerId: gid, name, group: true, kind: 'workspace', accepted: true, admin: true, members: invited.length + 1,
                msgs: [{ id: 's' + Math.random(), system: true, text: 'You created the workspace', sent: Math.floor(Date.now() / 1000) }],
                people: [{ peerId: 'me', name: 'You', me: true, admin: true }].concat(invited.map(p => ({ peerId: p, name: p.replace(/^@/, ''), pending: true }))) };
            return delay(view(convs[gid]));
        },
        createChannel: (ws, name, who) => {
            const gid = 'g:' + Math.random().toString(16).slice(2);
            const ids = who.split(/[\s,;]+/).filter(Boolean);
            const roster = (convs[ws] && convs[ws].people) || [];
            const picked = ids.map(i => roster.find(p => p.peerId === i)).filter(Boolean).map(p => ({ peerId: p.peerId, name: p.name }));
            convs[gid] = { peerId: gid, name, group: true, kind: 'channel', ws, accepted: true, admin: true, members: picked.length + 1,
                msgs: [{ id: 's' + Math.random(), system: true, text: 'You created the channel', sent: Math.floor(Date.now() / 1000) }],
                people: [{ peerId: 'me', name: 'You', me: true, admin: true }].concat(picked) };
            return delay(view(convs[gid]));
        },
        groupMembers: (gid) => delay((convs[gid].people || []).slice()),
        addToGroup: (gid, who) => { who.split(/[\s,;]+/).filter(Boolean).forEach(p => { convs[gid].people.push({ peerId: p, name: p.replace(/^@/, '') }); convs[gid].members++; }); return delay(); },
        renameGroup: (gid, name) => { convs[gid].name = name; convs[gid].msgs.push({ id: 's' + Math.random(), system: true, text: 'You renamed the group to ' + name, sent: Math.floor(Date.now() / 1000) }); return delay(); },
        // Pretend group calls: bob joins whatever we start, and connects.
        sendGroupCall: (g, to, op, callId) => {
            const bob = id('b');
            if (op === 'ring') setTimeout(() => emit('call', { peerId: bob, name: 'bob', group: g, groupName: (convs[g] || {}).name, op: 'join', callId }), 1200);
            if (op === 'hello' && to === bob) setTimeout(() => emit('call', { peerId: bob, name: 'bob', group: g, op: 'offer', callId, sdp: 'mock-offer' }), 300);
            if (op === 'offer' && to === bob) setTimeout(() => emit('call', { peerId: bob, name: 'bob', group: g, op: 'answer', callId, sdp: 'mock-answer' }), 300);
            return delay();
        },
        reportCoworker: (ws, pid, why) => { console.info('[mock] report to the owner of', ws, 'about', pid, ':', why); return delay(); },
        workspaceReports: (ws) => delay((convs[ws] && convs[ws].reports || []).filter(r => !r.done)),
        dismissReport: (ws, rid) => { (convs[ws].reports || []).forEach(r => { if (r.id === rid) r.done = true; }); convs[ws].reportCount = 0; return delay(); },
        settings: () => delay({ background: true, notifications: true, startup: false, startupAvailable: true, trayAvailable: true, autoUpdate: true, version: '1.0.8' }),
        setSetting: () => delay(),
        setFocused: () => Promise.resolve(),
        testNotification: () => delay('Sent. If nothing appeared, Windows may be hiding it.'),
        quit: () => delay(),
        removeFromGroup: (gid, m) => { convs[gid].people = convs[gid].people.filter(x => x.peerId !== m); convs[gid].members--; return delay(); },
        report: (pid, why, n) => delay('Thanks. We have received your report and will review it within 72 hours.'),
        // Deleting: the emailed code is 424242, any password but "wrong", and the name.
        startAccountDeletion: () => delay(),
        checkAccountDeletion: (c, p, n) => {
            if (c !== '424242') return fail("that code isn't right");
            if (p === 'wrong') return fail("that password isn't right");
            if (n && n.replace(/^@/, '').toLowerCase() !== me.handle) return fail("that isn't your name");
            return delay();
        },
        deleteAccount: (c, p, n) => (c === '424242' && p && p !== 'wrong' && n.replace(/^@/, '').toLowerCase() === me.handle)
            ? delay() : fail('every step has to be done first'),
        me: () => delay({ ...me }),
        claim: (h) => {
            h = h.replace(/^@/, '').toLowerCase();
            if (h === 'admin' || h === 'bob') return fail('that name is taken');
            me.handle = h; acct.handle = h;
            return delay(me.handle);
        },
        conversations: () => delay(Object.values(convs).map(view)),
        thread: (pid) => delay(convs[pid] ? convs[pid].msgs.slice() : []),
        add: (who) => {
            const h = who.replace(/^@/, '').toLowerCase();
            const pid = (h[0] || 'x').repeat(64);
            if (!convs[pid]) convs[pid] = { peerId: pid, name: h, handle: h, accepted: true, request: false, msgs: [] };
            return delay(view(convs[pid]));
        },
        send: (pid, text) => {
            if (!convs[pid]) convs[pid] = { peerId: pid, name: ((people.find(p => p.peerId === pid) || {}).name) || 'someone', accepted: true, request: false, biz: true, msgs: [] };
            if (convs[pid].blocked) return Promise.reject(new Error("you've blocked them; unblock them first"));
            const m = { id: 'x' + Math.random(), outbound: true, text, sent: Math.floor(Date.now() / 1000) };
            convs[pid].msgs.push(m);
            setTimeout(() => { m.delivered = true; emit('delivered', { peerId: pid, ids: [m.id] }); }, 300);
            return delay(m);
        },
        accept: (pid) => {
            const c = convs[pid];
            c.accepted = true; c.request = false;
            if (c.group) c.msgs.push({ id: 's' + Math.random(), system: true, text: 'You joined', sent: Math.floor(Date.now() / 1000) });
            else c.name = c.handle;
            return delay();
        },
        decline: (pid) => { delete convs[pid]; return delay(); },
        block: (pid) => { convs[pid].blocked = true; convs[pid].unread = 0; return delay(); },
        unblock: (pid) => { convs[pid].blocked = false; return delay(); },
        markRead: (pid) => { if (convs[pid]) convs[pid].unread = 0; return delay(); },
        on: (ev, fn) => { (listeners[ev] = listeners[ev] || []).push(fn); },
        copy: (text) => navigator.clipboard ? navigator.clipboard.writeText(text) : Promise.resolve(),
    };
}

/* Stand-ins for the microphone and the peer connection, used only when the
   page runs in an ordinary browser against the mock backend, so the call
   screens can be looked at without a second person. Inside the app the real
   ones are used. */
class FakeRTC {
    constructor() { this.iceGatheringState = 'complete'; this.connectionState = 'new'; this.localDescription = null; this._tx = []; }
    addEventListener() {} removeEventListener() {}
    addTrack() {}
    addTransceiver(kind) { const t = { direction: 'sendrecv', sender: { replaceTrack: async () => {} }, receiver: { track: { kind } } }; this._tx.push(t); return t; }
    getTransceivers() { return this._tx.length ? this._tx : [this.addTransceiver('video')]; }
    createDataChannel() { return { readyState: 'closed', send() {}, close() {} }; }
    async createOffer() { return { type: 'offer', sdp: 'mock-offer' }; }
    async createAnswer() { return { type: 'answer', sdp: 'mock-answer' }; }
    async setLocalDescription(d) { this.localDescription = d; if (d.type === 'answer') setTimeout(() => this._connect(), 600); }
    async setRemoteDescription(d) { if (d.type === 'answer') setTimeout(() => this._connect(), 600); }
    _connect() { this.connectionState = 'connected'; if (this.onconnectionstatechange) this.onconnectionstatechange(); }
    close() {}
}
function fakeStream() { return { getAudioTracks: () => [{ enabled: true, stop() {} }], getTracks: () => [{ stop() {} }] }; }

// A stand-in camera for browser review: a slowly shifting PANMOX gradient.
function fakeCamera() {
    const cv = document.createElement('canvas');
    cv.width = 640; cv.height = 360;
    const g = cv.getContext('2d');
    let t = 0;
    const draw = () => {
        t += 0.02;
        const grad = g.createLinearGradient(0, 0, 640, 360);
        grad.addColorStop(0, '#060a18');
        grad.addColorStop(0.5 + 0.3 * Math.sin(t), '#0a3a55');
        grad.addColorStop(1, '#00c8ff');
        g.fillStyle = grad; g.fillRect(0, 0, 640, 360);
        if (stream.getVideoTracks()[0].readyState === 'live') requestAnimationFrame(draw);
    };
    const stream = cv.captureStream(15);
    draw();
    return stream;
}

/* ───────────────────────────── state ─────────────────────────────── */

const S = {
    me: null,
    convs: [],     // conversation views, as returned by backend
    active: null,  // peerId of the open conversation
    thread: [],    // messages of the open conversation
    blocking: null, // peerId awaiting block confirmation
    reporting: null, // peerId being reported
    pending: {},   // peerId -> messages shown but not yet confirmed sent
    drafts: {},    // peerId -> text typed but not sent, kept across redraws
    typing: {},    // peerId -> time until which "typing…" shows
    typingWho: {}, // group peerId -> who is typing there
    gate: 'off',   // 'off' | 'signed_out' | 'ok' | 'banned' -- see engine.Gate
    acct: null,    // last account view from the backend
    mode: 'personal', // 'personal' | 'business': same account, two sides
    ws: null,      // peerId of the workspace shown in Business
    last: { personal: null, business: null }, // what was open on each side
    people: [],    // members of the current workspace
    wsMenu: false, // the workspace switcher is open
    dm: null,      // a chat with a coworker not started yet (no messages, so not in the list)
};

/* Personal and Business share one conversation list from the backend; these
   say which side a row belongs to. Business is workspaces, their channels,
   and chats with coworkers. */
const isWorkspace = (c) => !!c && c.kind === 'workspace';
const isChannel = (c) => !!c && c.kind === 'channel';
const isBiz = (c) => isWorkspace(c) || isChannel(c) || !!(c && c.biz);

const MODE_KEY = 'relay.mode';
const WS_KEY = 'relay.ws';
const TOUR_KEY = 'relay.tour';
function stored(k) { try { return localStorage.getItem(k); } catch (_) { return null; } }
function store(k, v) { try { localStorage.setItem(k, v); } catch (_) {} }

const joinedWorkspaces = () => S.convs.filter(c => isWorkspace(c) && c.accepted && !c.closed);
const workspaceInvites = () => S.convs.filter(c => isWorkspace(c) && c.request);
// Workspaces we left or were removed from: their history stays readable.
const pastWorkspaces = () => S.convs.filter(c => isWorkspace(c) && c.closed);
// The workspace on show: the one picked (even a past one, to read it), else
// the first one we're in.
function currentWs() {
    const picked = S.convs.find(c => c.peerId === S.ws && isWorkspace(c) && !c.request);
    return picked || joinedWorkspaces()[0] || null;
}
// A workspace's channels: the ones we're in -- or, for a past workspace,
// all of them, closed, so what was said can still be read.
const channelsOf = (ws) => S.convs
    .filter(c => isChannel(c) && c.ws === ws.peerId && (ws.closed ? !c.request : c.accepted && !c.closed))
    .sort((a, b) => a.name.localeCompare(b.name));
// What needs you on one side: unread messages, and invitations or requests.
function attention(mode) {
    let n = 0;
    S.convs.forEach(c => {
        if ((mode === 'business') !== isBiz(c)) return;
        if (c.blocked || c.banned) return;
        n += c.request ? 1 : (c.unread || 0);
    });
    return n;
}
// A workspace's own unread: its general chat plus its channels.
function wsUnread(ws) {
    return (ws.unread || 0) + channelsOf(ws).reduce((n, c) => n + (c.unread || 0), 0);
}

/* ──────────────────────────── helpers ────────────────────────────── */

// The ONLY way user-supplied text reaches the page: as textContent.
function el(tag, cls, text) {
    const n = document.createElement(tag);
    if (cls) n.className = cls;
    if (text != null) n.textContent = text;
    return n;
}

const $ = (sel) => document.querySelector(sel);

function toast(msg, ms) {
    const t = $('#toast');
    t.textContent = msg;
    t.classList.add('show');
    clearTimeout(toast.timer);
    toast.timer = setTimeout(() => t.classList.remove('show'), ms || 2200);
}

function errText(e) {
    const s = (e && e.message) || String(e || 'something went wrong');
    // Strip Go package prefixes ("mailbox: ...") -- they mean nothing to a person.
    return s.replace(/^(mailbox|state|engine|dht):\s*/i, '');
}

function initial(name) {
    const s = (name || '?').replace(/^@/, '');
    return s ? s[0] : '?';
}

function dayLabel(ts) {
    const d = new Date(ts * 1000);
    const today = new Date();
    const yest = new Date(); yest.setDate(today.getDate() - 1);
    if (d.toDateString() === today.toDateString()) return 'Today';
    if (d.toDateString() === yest.toDateString()) return 'Yesterday';
    return d.toLocaleDateString(undefined, { day: 'numeric', month: 'short' });
}

function clock(ts) {
    return new Date(ts * 1000).toLocaleTimeString(undefined, { hour: '2-digit', minute: '2-digit' });
}

function validHandle(h) {
    h = h.trim().replace(/^@/, '').toLowerCase();
    if (h.length < 3)  return 'At least 3 characters.';
    if (h.length > 32) return 'At most 32 characters.';
    if (!/^[a-z]/.test(h)) return 'Must start with a letter.';
    if (!/^[a-z0-9_-]+$/.test(h)) return 'Letters, numbers, - and _ only.';
    return '';
}

const PHONE_ICON = '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M22 16.9v3a2 2 0 0 1-2.2 2 19.8 19.8 0 0 1-8.6-3.1 19.5 19.5 0 0 1-6-6A19.8 19.8 0 0 1 2.1 4.2 2 2 0 0 1 4.1 2h3a2 2 0 0 1 2 1.7c.1 1 .4 1.9.7 2.8a2 2 0 0 1-.5 2.1L8.1 9.9a16 16 0 0 0 6 6l1.3-1.3a2 2 0 0 1 2.1-.4c.9.3 1.8.6 2.8.7a2 2 0 0 1 1.7 2z"/></svg>';
const CAMERA_ICON = '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M23 7l-7 5 7 5V7z"/><rect x="1" y="5" width="15" height="14" rx="2"/></svg>';
const CHEVRON_ICON = '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.4" stroke-linecap="round" stroke-linejoin="round"><path d="M6 9l6 6 6-6"/></svg>';
const SEND_ICON = '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.2" stroke-linecap="round" stroke-linejoin="round"><path d="M5 12h14M13 6l6 6-6 6"/></svg>';

/* ─────────────────────────── rendering ───────────────────────────── */

function renderMe() {
    const dot = $('#net-dot');
    dot.classList.toggle('offline', !S.me.online);
    dot.title = S.me.online ? `Connected · ${S.me.peers} peer${S.me.peers === 1 ? '' : 's'}` : 'Not connected';

    const h = $('#my-handle');
    const copy = $('#copy-id');
    const sub = $('#my-sub');
    if (S.me.handle) {
        h.textContent = '@' + S.me.handle;
        copy.hidden = false;
        sub.textContent = 'This is what you give people';
    } else {
        h.textContent = 'No name yet';
        copy.hidden = true;
        sub.textContent = '';
        const choose = el('button', 'copy', 'choose a name');
        choose.onclick = openClaim;
        sub.appendChild(choose);
    }
}

function renderList() {
    renderModeSwitch();
    const list = $('#list');
    list.replaceChildren();
    if (S.mode === 'business') renderBusinessList(list);
    else renderPersonalList(list);
}

// The switch under your name, and the buttons that belong to each side.
function renderModeSwitch() {
    document.querySelectorAll('#modeswitch button').forEach(b => {
        const m = b.dataset.mode;
        b.classList.toggle('on', m === S.mode);
        b.setAttribute('aria-selected', String(m === S.mode));
        const pip = b.querySelector('.pip');
        const n = m === S.mode ? 0 : attention(m);
        pip.hidden = n === 0;
        pip.textContent = n > 99 ? '99+' : String(n);
        b.title = n ? (m === 'business' ? 'Business needs you' : 'Personal needs you') : '';
    });
    const biz = S.mode === 'business';
    const ws = biz ? currentWs() : null;
    $('#add-btn').hidden = biz;
    $('#group-btn').hidden = biz;
    const live = ws && !ws.closed;
    $('#ws-btn').hidden = !biz || !!live;
    $('#channel-btn').hidden = !live;
    $('#invite-btn').hidden = !live;
}

function renderPersonalList(list) {
    const mine = S.convs.filter(c => !isBiz(c));
    const requests = mine.filter(c => c.request);
    const chats = mine.filter(c => !c.request).sort((a, b) => b.lastAt - a.lastAt);

    if (requests.length) {
        const label = el('div', 'section-label', 'Requests');
        label.appendChild(el('span', 'count', String(requests.length)));
        list.appendChild(label);
        requests.forEach(c => list.appendChild(row(c)));
    }

    list.appendChild(el('div', 'section-label', 'Messages'));
    if (!chats.length) {
        list.appendChild(el('div', 'empty-hint', 'Nobody here yet. Add someone by their name to start talking.'));
    }
    chats.forEach(c => list.appendChild(row(c)));
}

// Business: invitations, the workspace you're in, its channels, its people.
function renderBusinessList(list) {
    const invites = workspaceInvites();
    const ws = currentWs();
    // The workspace you're in comes first, like a company's name on its door.
    if (ws) {
        list.appendChild(workspaceBar(ws));
        if (S.wsMenu) list.appendChild(workspaceMenu(ws));
    }
    if (invites.length) {
        const label = el('div', 'section-label', 'Invitations');
        label.appendChild(el('span', 'count', String(invites.length)));
        list.appendChild(label);
        invites.forEach(c => list.appendChild(row(c)));
    }
    const others = ws && !ws.closed ? S.people.filter(p => !p.me) : [];
    if (!ws) {
        if (!invites.length) {
            list.appendChild(el('div', 'empty-hint',
                'Business is for work. Create a workspace for your company and invite your team by their @names, or wait for an invitation.'));
        }
        // Past workspaces stay reachable, to read what was said.
        const past = pastWorkspaces();
        if (past.length) {
            list.appendChild(el('div', 'section-label', 'Past workspaces'));
            past.forEach(w => {
                const r = row(w);
                r.querySelector('.name').textContent = w.name;
                r.onclick = () => selectWs(w.peerId);
                list.appendChild(r);
            });
        }
    } else {
        list.appendChild(el('div', 'section-label', 'Channels'));
        list.appendChild(row(ws));
        channelsOf(ws).forEach(c => list.appendChild(row(c)));

        if (!ws.closed) {
            const label = el('div', 'section-label', 'People');
            if (S.people.length) label.appendChild(el('span', 'count', String(S.people.length)));
            list.appendChild(label);
            if (!others.length) {
                list.appendChild(el('div', 'empty-hint', 'Just you so far. Use Invite people to bring your team in.'));
            }
            others.forEach(p => list.appendChild(personRow(p)));
        }
    }

    // Direct chats with coworkers who aren't in the workspace on show: from
    // another workspace, or people we no longer share one with (read-only).
    const known = new Set(others.map(p => p.peerId));
    const elsewhere = S.convs.filter(c => c.biz && !c.request && !known.has(c.peerId));
    if (elsewhere.length) {
        list.appendChild(el('div', 'section-label', 'Other chats'));
        elsewhere.forEach(c => list.appendChild(row(c)));
    }
}

function workspaceBar(ws) {
    const b = el('button', 'wsbar');
    b.appendChild(el('div', 'avatar', initial(ws.name)));
    b.appendChild(el('div', 'wsname', ws.name));
    if (ws.closed) b.appendChild(el('span', 'tag', ws.status === 'removed' ? 'removed' : 'left'));
    const caret = el('span', 'caret' + (S.wsMenu ? ' open' : ''));
    caret.innerHTML = CHEVRON_ICON; // static markup, not user data
    b.appendChild(caret);
    b.title = 'Switch workspace';
    b.onclick = () => { S.wsMenu = !S.wsMenu; renderList(); };
    return b;
}

function workspaceMenu(current) {
    const m = el('div', 'wsmenu');
    joinedWorkspaces().concat(pastWorkspaces()).forEach(w => {
        const b = el('button', w.peerId === current.peerId ? 'here' : '');
        b.appendChild(el('span', 'grow', w.name + (w.closed ? ' (past)' : '')));
        const n = w.peerId === current.peerId ? 0 : wsUnread(w);
        if (n) b.appendChild(el('span', 'unread', String(n)));
        b.onclick = () => selectWs(w.peerId);
        m.appendChild(b);
    });
    const add = el('button', '', '+ Create a workspace');
    add.onclick = () => { S.wsMenu = false; renderList(); openCreateWs(); };
    m.appendChild(add);
    return m;
}

// A teammate. Their chat (if you've talked) supplies the preview and the
// unread count; a click opens it, or starts one.
function personRow(p) {
    const c = S.convs.find(x => x.peerId === p.peerId);
    const silenced = c && (c.blocked || c.banned);
    const r = el('div', 'row person' + (p.peerId === S.active ? ' active' : '') + (silenced ? ' blocked' : ''));
    r.appendChild(el('div', 'avatar', initial(p.name)));
    const meta = el('div', 'meta');
    meta.appendChild(el('div', 'name', p.name));
    const typing = !silenced && isTyping(p.peerId);
    if (typing || (c && c.preview)) {
        meta.appendChild(el('div', 'preview' + (typing ? ' typing' : ''), typing ? 'typing\u2026' : c.preview));
    }
    r.appendChild(meta);
    if (p.admin) r.appendChild(el('span', 'tag', 'owner'));
    if (p.pending) r.appendChild(el('span', 'tag', 'invited'));
    if (c && c.unread > 0 && !silenced && p.peerId !== S.active) {
        r.appendChild(el('span', 'unread', c.unread > 99 ? '99+' : String(c.unread)));
    }
    r.onclick = () => openDirect(p);
    return r;
}

// What a row calls a conversation: a workspace's own chat is its #general.
const rowName = (c) => (isWorkspace(c) && !c.request) ? 'general' : c.name;
const titleOf = (c) => isChannel(c) ? '# ' + c.name : (isWorkspace(c) && !c.request) ? '# general' : c.name;
function subtitleOf(c) {
    const pending = c.pending || 0;
    const here = c.members - pending;
    const n = here + (here === 1 ? ' person' : ' people') + (pending && !c.request ? ' \u00B7 ' + pending + ' invited' : '');
    if (isChannel(c)) {
        const w = S.convs.find(x => x.peerId === c.ws);
        return (w ? w.name + ' \u00B7 ' : '') + n;
    }
    if (isWorkspace(c)) return c.name + ' \u00B7 ' + n;
    return n;
}
const kindWord = (c) => isWorkspace(c) ? 'workspace' : isChannel(c) ? 'channel' : 'group';

function row(c) {
    const silenced = c.blocked || c.banned;
    const r = el('div', 'row' + (c.peerId === S.active ? ' active' : '') + (silenced ? ' blocked' : ''));
    const hash = isChannel(c) || (isWorkspace(c) && !c.request);
    r.appendChild(el('div', 'avatar' + (hash ? ' hash' : c.group ? ' group' : ''), hash ? '#' : initial(c.name)));
    const meta = el('div', 'meta');
    meta.appendChild(el('div', 'name', rowName(c)));
    const typing = !silenced && isTyping(c.peerId);
    const preview = c.banned ? 'Banned from AltNet' : c.blocked ? 'Blocked'
        : c.request ? (c.group ? c.preview : 'Wants to message you')
        : typing ? (c.group && S.typingWho[c.peerId] ? S.typingWho[c.peerId] + ' is typing…' : 'typing…')
        : c.preview || '';
    meta.appendChild(el('div', 'preview' + (typing ? ' typing' : ''), preview));
    r.appendChild(meta);
    if (c.unread > 0 && !silenced && c.peerId !== S.active) {
        r.appendChild(el('span', 'unread', c.unread > 99 ? '99+' : String(c.unread)));
    }
    r.onclick = () => open(c.peerId);
    return r;
}

function renderEmptyMain(main) {
    const empty = el('div', 'empty');
    const button = (label, fn) => {
        const b = el('button', 'btn', label);
        b.style.width = 'auto';
        b.style.marginTop = '8px';
        b.style.padding = '9px 22px';
        b.onclick = fn;
        empty.appendChild(b);
    };
    if (S.mode === 'business') {
        if (!currentWs()) {
            empty.appendChild(el('h2', null, 'Your company, in one room'));
            empty.appendChild(el('p', null, 'Create a workspace and invite your team by their @names. Everyone gets a #general channel, you can make channels for projects, and anyone can start a video meeting. It\'s the same account as your personal chats \u2014 just the other side of it.'));
            button('Create a workspace', openCreateWs);
            if (workspaceInvites().length) {
                empty.appendChild(el('p', null, 'You also have an invitation waiting on the left.'));
            }
        } else {
            empty.appendChild(el('h2', null, 'Pick a channel'));
            empty.appendChild(el('p', null, 'Channels are for the whole team or just the people on a project. Everything here is end-to-end encrypted and stored across the AltNet network \u2014 not on anyone\'s server.'));
        }
    } else if (S.convs.some(c => !isBiz(c))) {
        empty.appendChild(el('h2', null, 'Pick a conversation'));
        empty.appendChild(el('p', null, 'Everything here is end-to-end encrypted and stored across the AltNet network \u2014 not on anyone\'s server.'));
    } else {
        empty.appendChild(el('h2', null, 'Start a conversation'));
        empty.appendChild(el('p', null, 'Add someone by the name they gave you. Only people you give your name to can reach you.'));
        button('Add someone', openAdd);
    }
    main.appendChild(empty);
}

function renderMain() {
    const main = $('#main');
    main.replaceChildren();

    const c = S.convs.find(x => x.peerId === S.active);
    if (!c) {
        renderEmptyMain(main);
        return;
    }

    if (c.group) { renderGroup(main, c); return; }

    // Header
    const head = el('div', 'thread-head');
    head.appendChild(el('div', 'avatar', initial(c.name)));
    const who = el('div');
    who.appendChild(el('div', 'who', c.name));
    who.appendChild(el('div', 'id', c.handle ? '@' + c.handle : c.peerId.slice(0, 16) + '…'));
    head.appendChild(who);
    // A pending request already has Decline; blocking lives here for
    // everyone else, quietly. A banned account needs no control: the
    // network has already silenced them.
    // Report is offered wherever someone could have done something worth
    // reporting -- including a stranger's request -- but not for an account
    // the network has already banned.
    let first = true;
    // Calls are for people who have written to you (see engine.SendCall):
    // before that the button would only ever say no.
    const canCall = c.accepted && !c.request && !c.blocked && !c.banned && !c.former
        && (c.biz || S.thread.some(m => !m.outbound)) && S.gate !== 'signed_out';
    if (canCall) {
        backend.warmCalls().catch(() => {}); // relay credentials ready before Call is pressed
        const call = el('button', 'head-action call');
        call.innerHTML = PHONE_ICON; // static markup, not user data
        call.appendChild(document.createTextNode('Call'));
        call.title = 'Voice call';
        call.onclick = () => startCall(c, false);
        head.appendChild(call);
        const video = el('button', 'head-action call');
        video.innerHTML = CAMERA_ICON; // static markup, not user data
        video.appendChild(document.createTextNode('Video'));
        video.title = 'Video call';
        video.style.marginLeft = '0';
        video.onclick = () => startCall(c, true);
        head.appendChild(video);
        first = false;
    }
    // Not for coworkers: at work the person who runs the workspace decides
    // who stays. You tell them (a message, like any other). Say so where the
    // buttons would be, or it just looks like they're missing.
    const atWork = c.coworker && S.mode === 'business';
    if (atWork && !c.banned) {
        const ws = currentWs();
        const iRun = !!(ws && ws.admin);
        const theyRun = S.people.some(p => p.peerId === c.peerId && p.admin);
        const inIt = S.people.some(p => p.peerId === c.peerId && !p.pending);
        if (ws && !iRun && !theyRun && inIt) {
            const rep = el('button', 'head-action', 'Report');
            rep.title = 'Report them to the owner of ' + ws.name;
            rep.onclick = () => openWorkReport(c, ws);
            if (!first) rep.style.marginLeft = '0';
            head.appendChild(rep);
            first = false;
        }
        const note = el('button', 'head-action undo', 'Coworker');
        note.title = 'Why is there no Block here?';
        note.onclick = () => toast(iRun
            ? 'You run this workspace: to remove someone, open People in #general.'
            : 'At work, the workspace\u2019s owner decides who stays: report them to the owner here, or leave the workspace. ' +
              'Blocking and reporting to AltNet are in Personal.', 7000);
        if (!first) note.style.marginLeft = '0';
        head.appendChild(note);
        first = false;
    }
    if (!c.banned && S.gate !== 'off' && !atWork) {
        const rep = el('button', 'head-action', 'Report');
        rep.onclick = () => openReport(c);
        if (!first) rep.style.marginLeft = '0';
        head.appendChild(rep);
        first = false;
    }
    if (!c.banned && !atWork && (!c.request || c.blocked)) {
        const act = el('button', 'head-action' + (c.blocked ? ' undo' : ''), c.blocked ? 'Unblock' : 'Block');
        act.onclick = () => c.blocked ? doUnblock(c.peerId) : openBlock(c);
        if (!first) act.style.marginLeft = '0';
        head.appendChild(act);
    }
    main.appendChild(head);

    // Thread
    const t = el('div', 'thread');
    let lastDay = '', prev = null;
    const shown = S.thread.concat(S.pending[c.peerId] || []);
    shown.forEach(m => {
        const d = dayLabel(m.sent);
        if (d !== lastDay) { t.appendChild(el('div', 'day', d)); lastDay = d; prev = null; }
        const same = prev && prev.outbound === m.outbound && (m.sent - prev.sent) < 300;
        const state = m.failed ? ' failed' : m.pending ? ' pending' : '';
        const row = el('div', 'msg ' + (m.outbound ? 'out' : 'in') + (same ? ' same' : '') + state);
        let stamp;
        if (m.failed) {
            // Not sent: say so where the message is, and make retrying one click.
            stamp = el('button', 'stamp retry', 'Not sent · Retry');
            stamp.title = m.error || '';
            stamp.onclick = () => retrySend(c.peerId, m);
        } else {
            stamp = el('span', 'stamp', m.pending ? '' : clock(m.sent));
        }
        const bubble = el('div', 'bubble', m.text);
        if (m.outbound) { row.appendChild(stamp); row.appendChild(bubble); }
        else            { row.appendChild(bubble); row.appendChild(stamp); }
        t.appendChild(row);
        prev = m;
    });
    // "Delivered" under the newest message I sent, once their device has it --
    // like a tick, but in words, and only where it answers a question.
    const lastOut = [...shown].reverse().find(m => m.outbound);
    if (lastOut && lastOut.delivered && !lastOut.pending) {
        t.appendChild(el('div', 'receipt', 'Delivered'));
    }
    if (!c.blocked && !c.banned && isTyping(c.peerId)) {
        const row = el('div', 'msg in typing-row');
        const bubble = el('div', 'bubble dots');
        for (let i = 0; i < 3; i++) bubble.appendChild(el('span'));
        bubble.title = c.name + ' is typing';
        row.appendChild(bubble);
        t.appendChild(row);
    }
    main.appendChild(t);
    t.scrollTop = t.scrollHeight;

    // Banned by an operator: nothing the user can do here, so the bar
    // explains rather than offers a button. History stays readable.
    if (c.banned) {
        const bar = el('div', 'request-bar');
        bar.appendChild(el('p', null,
            `This account has been banned from AltNet for breaking its rules. You won't get messages from it, and you can't reply.`));
        main.appendChild(bar);
        return;
    }

    // Blocked: the history stays readable, but there's nothing to type
    // into -- the only way forward is a deliberate unblock.
    if (c.blocked) {
        const bar = el('div', 'request-bar');
        bar.appendChild(el('p', null,
            `You blocked ${c.name}. You won't get their messages or requests, and they aren't told.`));
        const actions = el('div', 'actions');
        const unblock = el('button', 'btn', 'Unblock');
        unblock.onclick = () => doUnblock(c.peerId);
        actions.appendChild(unblock);
        bar.appendChild(actions);
        main.appendChild(bar);
        return;
    }

    // A work chat with someone who is no longer a coworker: it stays here to
    // read, but Business is no longer a way to reach them.
    if (c.former) {
        const bar = el('div', 'request-bar');
        bar.appendChild(el('p', null,
            `${c.name} is no longer in a workspace with you. Your work chat stays here to read. ` +
            `To keep talking, add them in Personal.`));
        main.appendChild(bar);
        return;
    }

    // A message request gets a decision, not a composer. You can't reply
    // to someone you haven't let in yet -- replying IS letting them in.
    if (c.request) {
        const bar = el('div', 'request-bar');
        bar.appendChild(el('p', null,
            `${c.name} wants to message you. They can send up to 3 messages until you accept. ` +
            `They won't be told either way.`));
        const actions = el('div', 'actions');
        const accept = el('button', 'btn', 'Accept');
        const decline = el('button', 'btn danger', 'Decline');
        accept.onclick = () => doAccept(c.peerId);
        decline.onclick = () => doDecline(c.peerId);
        actions.appendChild(accept);
        actions.appendChild(decline);
        bar.appendChild(actions);
        main.appendChild(bar);
        return;
    }

    buildComposer(main, c);
}

// The message box, shared by chats and groups.
function buildComposer(main, c) {
    const comp = el('div', 'composer');
    const ta = el('textarea');
    ta.rows = 1;
    ta.placeholder = 'Message ' + titleOf(c);
    const send = el('button', 'send');
    send.innerHTML = SEND_ICON; // static markup, not user data
    send.disabled = true;
    send.title = 'Send (Enter)';

    const grow = () => {
        ta.style.height = 'auto';
        ta.style.height = Math.min(ta.scrollHeight, 140) + 'px';
        send.disabled = !ta.value.trim();
    };
    // The whole conversation is redrawn when a message arrives; the draft
    // lives outside it so nothing half-typed is ever lost.
    ta.value = S.drafts[c.peerId] || '';
    ta.oninput = () => {
        S.drafts[c.peerId] = ta.value;
        grow();
        if (ta.value.trim() && !c.closed) sayTyping(c.peerId);
    };
    ta.onkeydown = (e) => {
        if (e.key === 'Enter' && !e.shiftKey) { e.preventDefault(); doSend(ta, send); }
    };
    send.onclick = () => doSend(ta, send);

    comp.appendChild(ta);
    comp.appendChild(send);
    main.appendChild(comp);
    ta.focus();
    if (ta.value) { grow(); ta.setSelectionRange(ta.value.length, ta.value.length); }
}

/* ───────────────────────────── groups ────────────────────────────── */

function renderGroup(main, c) {
    const head = el('div', 'thread-head');
    const hash = isChannel(c) || (isWorkspace(c) && !c.request);
    if (!hash) head.appendChild(el('div', 'avatar group', initial(c.name)));
    const who = el('div');
    who.appendChild(el('div', 'who', titleOf(c)));
    who.appendChild(el('div', 'id', subtitleOf(c)));
    head.appendChild(who);
    const inCall = groupCalls.state().phase === 'active' && groupCalls.state().group === c.peerId;
    if (c.accepted && c.members > 1 && !inCall) {
        backend.warmCalls().catch(() => {});
        const call = el('button', 'head-action call');
        call.innerHTML = PHONE_ICON; // static markup, not user data
        call.appendChild(document.createTextNode('Call'));
        call.title = 'Group call';
        call.onclick = () => startGroupCall(c, false);
        head.appendChild(call);
        const video = el('button', 'head-action call');
        video.innerHTML = CAMERA_ICON; // static markup, not user data
        video.appendChild(document.createTextNode(isBiz(c) ? 'Meet' : 'Video'));
        video.title = isBiz(c) ? 'Start a video meeting' : 'Group video call';
        video.style.marginLeft = '0';
        video.onclick = () => startGroupCall(c, true);
        head.appendChild(video);
    }
    if (c.accepted && isWorkspace(c) && c.admin && c.reports > 0) {
        const rb = el('button', 'head-action report-flag', c.reports === 1 ? '1 report' : c.reports + ' reports');
        rb.title = 'Reports from members, waiting for you';
        rb.onclick = () => openMembers(c);
        if (head.querySelector('.call')) rb.style.marginLeft = '0';
        head.appendChild(rb);
    }
    if (c.accepted) {
        const members = el('button', 'head-action undo', isBiz(c) ? 'People' : 'Members');
        members.onclick = () => openMembers(c);
        head.appendChild(members);
        const leave = el('button', 'head-action', isWorkspace(c) ? 'Leave workspace' : 'Leave');
        leave.style.marginLeft = '0';
        if (!head.querySelector('.call, .report-flag')) members.style.marginLeft = 'auto'; else members.style.marginLeft = '0';
        leave.onclick = () => openLeave(c);
        head.appendChild(leave);
    }
    main.appendChild(head);

    // An invitation: a decision, not the conversation. Nothing from the
    // group is shown until the person says yes.
    if (c.request) {
        const wrap = el('div', 'empty');
        wrap.appendChild(el('h2', null, c.name));
        if (isWorkspace(c)) {
            wrap.appendChild(el('p', null, c.preview + ' to join the workspace ' + c.name + ' (' + c.members + ' people). ' +
                'If you join, you\'ll be in #general with everyone, can be added to channels, and teammates can message and call you directly from Business. ' +
                'Your personal chats stay separate. Nobody is told if you decline, except that you won\'t be in it.'));
        } else {
            wrap.appendChild(el('p', null, c.preview + ' to this group of ' + c.members + ' people. ' +
                'If you join, everyone in it can see your messages, and you theirs. Nobody is told if you decline, except that you won\'t be in it.'));
        }
        const actions = el('div', 'actions');
        const join = el('button', 'btn', 'Join');
        const no = el('button', 'btn danger', 'Decline');
        join.onclick = () => doAccept(c.peerId);
        no.onclick = () => doDecline(c.peerId);
        actions.appendChild(join);
        actions.appendChild(no);
        if (c.invitedBy) {
            // Nobody can be blocked once they're a coworker, so an unwanted
            // invitation is where to say no to the person as well.
            const nb = el('button', 'btn ghost', 'Decline and block');
            nb.title = 'Decline, and block whoever sent it';
            nb.onclick = async () => {
                try {
                    await backend.decline(c.peerId);
                    await backend.block(c.invitedBy);
                    S.active = null;
                    S.thread = [];
                    await refreshList();
                    renderMain();
                    toast('Declined and blocked');
                } catch (e) { toast(errText(e)); }
            };
            actions.appendChild(nb);
        }
        wrap.appendChild(actions);
        main.appendChild(wrap);
        return;
    }

    // A call is going on in this group and we're not in it: say so, with a
    // way in.
    const running = c.accepted ? groupCalls.runningIn(c.peerId) : null;
    if (running) {
        const bar = el('div', 'call-banner');
        bar.appendChild(el('span', null, (isBiz(c) ? 'Meeting in progress \u00B7 ' : 'Call in progress \u00B7 ') + running.count + (running.count === 1 ? ' person' : ' people')));
        const join = el('button', 'cbtn', 'Join');
        join.onclick = () => joinGroupCall(running.callId, false);
        const jv = el('button', 'cbtn quiet', 'Join with video');
        jv.onclick = () => joinGroupCall(running.callId, true);
        bar.appendChild(join);
        bar.appendChild(jv);
        main.appendChild(bar);
    }

    const t = el('div', 'thread');
    let lastDay = '', prev = null;
    const shown = S.thread.concat(S.pending[c.peerId] || []);
    shown.forEach(m => {
        const d = dayLabel(m.sent);
        if (d !== lastDay) { t.appendChild(el('div', 'day', d)); lastDay = d; prev = null; }
        if (m.system) {
            t.appendChild(el('div', 'sysline', m.text));
            prev = null;
            return;
        }
        // Whose message it is, once per run of messages from the same person.
        const newSpeaker = !m.outbound && (!prev || prev.outbound || prev.from !== m.from || (m.sent - prev.sent) >= 300);
        if (newSpeaker && m.from) t.appendChild(el('div', 'speaker', m.from));
        const same = prev && prev.outbound === m.outbound && prev.from === m.from && (m.sent - prev.sent) < 300;
        const state = m.failed ? ' failed' : m.pending ? ' pending' : '';
        const row = el('div', 'msg ' + (m.outbound ? 'out' : 'in') + (same ? ' same' : '') + state);
        let stamp;
        if (m.failed) {
            stamp = el('button', 'stamp retry', 'Not sent · Retry');
            stamp.title = m.error || '';
            stamp.onclick = () => retrySend(c.peerId, m);
        } else {
            stamp = el('span', 'stamp', m.pending ? '' : clock(m.sent));
        }
        const bubble = el('div', 'bubble', m.text);
        if (m.outbound) { row.appendChild(stamp); row.appendChild(bubble); }
        else            { row.appendChild(bubble); row.appendChild(stamp); }
        t.appendChild(row);
        prev = m;
    });
    // How far our newest message got: "Delivered to 2 of 3", then
    // "Delivered to everyone".
    const lastOut = [...shown].reverse().find(m => m.outbound && !m.system);
    if (lastOut && !lastOut.pending && lastOut.of > 0 && lastOut.reached > 0) {
        t.appendChild(el('div', 'receipt', lastOut.reached >= lastOut.of
            ? 'Delivered to everyone' : 'Delivered to ' + lastOut.reached + ' of ' + lastOut.of));
    }
    if (isTyping(c.peerId) && !c.closed) {
        if (S.typingWho[c.peerId]) t.appendChild(el('div', 'speaker', S.typingWho[c.peerId]));
        const row = el('div', 'msg in typing-row');
        const bubble = el('div', 'bubble dots');
        for (let i = 0; i < 3; i++) bubble.appendChild(el('span'));
        row.appendChild(bubble);
        t.appendChild(row);
    }
    main.appendChild(t);
    t.scrollTop = t.scrollHeight;

    if (c.closed) {
        const bar = el('div', 'request-bar');
        bar.appendChild(el('p', null, c.status === 'removed'
            ? 'You were removed from this ' + kindWord(c) + '. You can still read what was said while you were in it.'
            : 'You left this ' + kindWord(c) + '. You can still read what was said while you were in it.'));
        main.appendChild(bar);
        return;
    }
    buildComposer(main, c);
}

function openNewGroup() {
    $('#group-name').value = '';
    $('#group-people').value = '';
    $('#group-error').textContent = '';
    $('#group-modal').hidden = false;
    $('#group-name').focus();
}

async function submitNewGroup() {
    const name = $('#group-name').value.trim();
    const who = $('#group-people').value.trim();
    if (!name) { $('#group-error').textContent = 'Give the group a name.'; return; }
    if (!who) { $('#group-error').textContent = 'Add at least one person.'; return; }
    $('#group-go').disabled = true;
    try {
        const c = await backend.createGroup(name, who);
        $('#group-modal').hidden = true;
        await refreshList();
        await open(c.peerId);
    } catch (e) {
        $('#group-error').textContent = errText(e);
    } finally {
        $('#group-go').disabled = false;
    }
}

let membersOf = null;

async function openMembers(c) {
    membersOf = c;
    $('#members-title').textContent = isWorkspace(c) ? 'People in ' + c.name : isChannel(c) ? '# ' + c.name : c.name;
    $('#members-add').hidden = !c.admin && !isWorkspace(c);
    $('#members-add .row-input').hidden = !c.admin;
    $('#rename-row').hidden = !c.admin;
    $('#rename-input').placeholder = isWorkspace(c) ? 'Workspace name' : isChannel(c) ? 'Channel name' : 'Group name';
    $('#members-input').placeholder = isWorkspace(c) ? 'Invite people: names or IDs'
        : isChannel(c) ? 'Add people from the workspace' : 'Add people: names or IDs';
    $('#members-note').textContent = isWorkspace(c)
        ? (c.admin
            ? 'You decide who belongs here. Remove takes someone out of the workspace and all its channels. Each person you invite chooses whether to join.'
            : 'If someone here is a problem, press Report next to their name: only the owner sees it, and only the owner can remove people. You can also leave the workspace.')
        : isChannel(c) ? 'Only people already in the workspace can be added, and they\'re added straight away.' : '';
    $('#members-note').hidden = !$('#members-note').textContent;
    $('#rename-input').value = c.name;
    $('#members-error').textContent = '';
    $('#members-input').value = '';
    $('#members-modal').hidden = false;
    await drawMembers();
}

async function drawMembers() {
    const c = membersOf;
    if (!c) return;
    const box = $('#members-list');
    box.replaceChildren();
    await drawReports(c);
    let people = [];
    try { people = await backend.groupMembers(c.peerId) || []; } catch (e) { $('#members-error').textContent = errText(e); }
    people.forEach(p => {
        const r = el('div', 'member');
        r.appendChild(el('div', 'avatar', initial(p.name)));
        r.appendChild(el('div', 'mname', p.name));
        if (p.admin) r.appendChild(el('span', 'tag', isWorkspace(c) ? 'owner' : 'admin'));
        if (p.pending) r.appendChild(el('span', 'tag quiet', 'invited'));
        // At work a member reports a coworker to the owner (never the owner).
        if (isWorkspace(c) && !c.admin && !p.me && !p.admin && !p.pending && S.gate !== 'signed_out') {
            const rep = el('button', 'link', 'Report');
            // Close this list first: the report form would open underneath it.
            rep.onclick = () => { $('#members-modal').hidden = true; membersOf = null; openWorkReport({ peerId: p.peerId, name: p.name }, c); };
            r.appendChild(rep);
        }
        if (c.admin && !p.me) {
            const rm = el('button', 'link', 'Remove');
            rm.onclick = async () => {
                try { await backend.removeFromGroup(c.peerId, p.peerId); await refreshList(); await drawMembers(); }
                catch (e) { $('#members-error').textContent = errText(e); }
            };
            r.appendChild(rm);
        }
        box.appendChild(r);
    });
}

// The reports members sent us, as owner of a workspace: who, about whom,
// why, and what the person wrote if it was included. Remove or dismiss.
async function drawReports(c) {
    const box = $('#members-reports');
    box.replaceChildren();
    box.hidden = true;
    if (!isWorkspace(c) || !c.admin) return;
    let list = [];
    try { list = await backend.workspaceReports(c.peerId) || []; } catch (_) { return; }
    if (!list.length) return;
    box.hidden = false;
    box.appendChild(el('div', 'section-label', 'Reports from members'));
    list.forEach(r => {
        const item = el('div', 'wsreport');
        const head = el('div', 'wsr-head');
        head.appendChild(el('strong', null, r.from));
        head.appendChild(document.createTextNode(' reported '));
        head.appendChild(el('strong', null, r.who));
        head.appendChild(el('span', 'wsr-when', ' \u00B7 ' + dayLabel(r.at) + ' ' + clock(r.at)));
        item.appendChild(head);
        item.appendChild(el('div', 'wsr-reason', r.reason));
        if ((r.evidence || []).length) {
            const ul = el('ul', 'wsr-ev');
            r.evidence.forEach(l => {
                const li = el('li');
                li.appendChild(el('span', 'wsr-where', l.where === 'general' ? 'in #general' : l.where === 'direct' ? 'to ' + r.from : ''));
                li.appendChild(el('span', null, l.text));
                ul.appendChild(li);
            });
            item.appendChild(ul);
        }
        const actions = el('div', 'wsr-actions');
        const rm = el('button', 'btn danger', 'Remove ' + r.who);
        rm.onclick = async () => {
            try { await backend.removeFromGroup(c.peerId, r.whoId); await refreshList(); syncMembersOf(); await drawMembers(); if (S.active === c.peerId) renderMain(); toast(r.who + ' was removed'); }
            catch (e) { $('#members-error').textContent = errText(e); }
        };
        const dm = el('button', 'btn ghost', 'Dismiss');
        dm.onclick = async () => {
            try { await backend.dismissReport(c.peerId, r.id); await refreshList(); syncMembersOf(); await drawMembers(); if (S.active === c.peerId) renderMain(); }
            catch (e) { $('#members-error').textContent = errText(e); }
        };
        actions.appendChild(rm);
        actions.appendChild(dm);
        item.appendChild(actions);
        box.appendChild(item);
    });
}

// After a refresh, the dialog's copy of its conversation is stale.
function syncMembersOf() {
    if (!membersOf) return;
    const fresh = S.convs.find(x => x.peerId === membersOf.peerId);
    if (fresh) membersOf = fresh;
}

async function submitRename() {
    const c = membersOf;
    let name = $('#rename-input').value.trim();
    if (isChannel(c)) name = name.replace(/^#+/, '').replace(/\s+/g, '-');
    if (!c || !name || name === c.name) return;
    $('#rename-go').disabled = true;
    try {
        await backend.renameGroup(c.peerId, name);
        c.name = name;
        $('#members-title').textContent = isWorkspace(c) ? 'People in ' + name : isChannel(c) ? '# ' + name : name;
        await refreshList();
        if (S.active === c.peerId) { S.thread = await backend.thread(c.peerId) || []; renderMain(); }
    } catch (e) {
        $('#members-error').textContent = errText(e);
    } finally {
        $('#rename-go').disabled = false;
    }
}

async function submitAddMembers() {
    const c = membersOf;
    const who = $('#members-input').value.trim();
    if (!c || !who) return;
    $('#members-go').disabled = true;
    try {
        await backend.addToGroup(c.peerId, who);
        $('#members-input').value = '';
        await refreshList();
        await drawMembers();
    } catch (e) {
        $('#members-error').textContent = errText(e);
    } finally {
        $('#members-go').disabled = false;
    }
}

let leaving = null;

function openLeave(c) {
    leaving = c.peerId;
    $('#leave-title').textContent = 'Leave ' + (isChannel(c) ? '# ' : '') + c.name + '?';
    $('#leave-text').textContent = isWorkspace(c)
        ? 'You\'ll be taken out of #general and every channel in ' + c.name + ', and the others will see that you left. What was said stays readable on this computer. To come back, the owner has to invite you again.'
        : 'You\'ll stop getting its messages, and the others will see that you left. What was said stays readable on this computer.';
    $('#leave-modal').hidden = false;
}

async function doLeave() {
    const peerId = leaving;
    $('#leave-modal').hidden = true;
    leaving = null;
    if (!peerId) return;
    try {
        const c = S.convs.find(x => x.peerId === peerId);
        await backend.leaveGroup(peerId);
        await refreshList();
        if (isWorkspace(c)) {
            // It stays on show, closed, so what was said can still be read;
            // the switcher moves on to the others.
            await open(peerId);
            toast('You left ' + c.name);
        } else {
            await open(peerId);
            toast('You left the ' + kindWord(c));
        }
    } catch (e) {
        toast(errText(e));
    }
}

/* ───────────────────────────── live ──────────────────────────────── */

const TYPING_SHOW_MS = 6000;   // how long "typing…" lasts after the last signal
const TYPING_EVERY_MS = 3000;  // how often we tell them we're still typing
let lastTypingSent = {};

function isTyping(peerId) { return (S.typing[peerId] || 0) > Date.now(); }

function sayTyping(peerId) {
    const now = Date.now();
    if (now - (lastTypingSent[peerId] || 0) < TYPING_EVERY_MS) return;
    lastTypingSent[peerId] = now;
    backend.typing(peerId).catch(() => {});
}

function showTyping(peerId, on) {
    if (on) S.typing[peerId] = Date.now() + TYPING_SHOW_MS;
    else delete S.typing[peerId];
    renderList();
    if (S.active === peerId) renderMain();
    clearTimeout(showTyping.timers && showTyping.timers[peerId]);
    showTyping.timers = showTyping.timers || {};
    if (on) showTyping.timers[peerId] = setTimeout(() => { if (!isTyping(peerId)) showTyping(peerId, false); }, TYPING_SHOW_MS + 50);
}

/* ──────────────────────────── actions ────────────────────────────── */

async function refreshList() {
    S.convs = await backend.conversations() || [];
    // A chat with a teammate you haven't written to yet isn't in the list
    // (it has no messages); keep it on screen until it is.
    if (S.dm) {
        if (S.convs.some(c => c.peerId === S.dm.peerId)) S.dm = null;
        else S.convs.push(S.dm);
    }
    // Keep the workspace on show while it exists here, even one we have just
    // left or been removed from (it stays readable); otherwise the first one.
    if (!S.convs.some(c => c.peerId === S.ws && isWorkspace(c) && !c.request)) {
        const all = joinedWorkspaces();
        S.ws = all.length ? all[0].peerId : null;
    }
    renderList();
    if (S.mode === 'business') await loadPeople();
}

// Who is in the current workspace, for the People section.
async function loadPeople() {
    const ws = currentWs();
    let people = [];
    if (ws && !ws.closed) { try { people = await backend.groupMembers(ws.peerId) || []; } catch (_) {} }
    if (JSON.stringify(people) !== JSON.stringify(S.people)) {
        S.people = people;
        renderList();
    }
}

async function selectWs(peerId) {
    S.ws = peerId;
    store(WS_KEY, peerId);
    S.wsMenu = false;
    S.people = [];
    S.active = null;
    S.thread = [];
    renderList();
    await loadPeople();
    await open(peerId); // its #general
}

// Open a chat with a teammate; starts one if you haven't talked yet.
async function openDirect(p) {
    if (p.me) return;
    // Someone still deciding isn't a coworker yet: a message now would reach
    // them as a stranger's request, not as work.
    if (p.pending && !S.convs.some(c => c.peerId === p.peerId)) {
        toast(p.name + ' hasn’t joined yet — you can message them once they accept');
        return;
    }
    if (!S.convs.some(c => c.peerId === p.peerId)) {
        S.dm = { peerId: p.peerId, name: p.name, handle: '', accepted: true, request: false, biz: true,
                 blocked: false, banned: false, unread: 0, preview: '', lastAt: 0, group: false };
        S.convs.push(S.dm);
    }
    await open(p.peerId);
}

// Coming back to Business: a workspace we have left is still readable, but
// one we are in comes first.
function preferLiveWs() {
    const cur = currentWs();
    const live = joinedWorkspaces();
    if (cur && cur.closed && live.length) {
        S.ws = live[0].peerId;
        store(WS_KEY, S.ws);
        S.people = [];
        return true;
    }
    return false;
}

// Personal or Business. Each side remembers what was open on it.
async function setMode(mode) {
    if (mode === S.mode) return;
    S.last[S.mode] = S.active;
    S.mode = mode;
    S.wsMenu = false;
    store(MODE_KEY, mode);
    S.active = null;
    S.thread = [];
    let next = S.last[mode];
    const there = next && S.convs.find(c => c.peerId === next);
    if (!there || (mode === 'business') !== isBiz(there)) next = null;
    if (mode === 'business' && preferLiveWs()) next = null;
    if (!next && mode === 'business') {
        const ws = currentWs();
        if (ws) next = ws.peerId; // land in #general
    }
    renderList();
    if (mode === 'business') loadPeople();
    if (next) await open(next); else renderMain();
}

async function open(peerId) {
    S.active = peerId;
    S.thread = await backend.thread(peerId) || [];
    const c = S.convs.find(x => x.peerId === peerId);
    if (c && c.unread > 0) {
        c.unread = 0; // clear it on screen now; the backend write follows
        try { await backend.markRead(peerId); } catch (_) {}
    }
    renderList();
    renderMain();
}

function openBlock(c) {
    S.blocking = c.peerId;
    $('#block-title').textContent = 'Block ' + c.name + '?';
    $('#block-report').checked = false;
    $('#block-report-form').hidden = true;
    $('#block-reason').value = '';
    $('#block-attach').checked = true;
    $('#block-error').textContent = '';
    // No way to report from here without an account to report with.
    $('#block-report').closest('label').hidden = S.gate === 'off';
    $('#block-modal').hidden = false;
}

// Block, and -- if ticked -- report them too. The block is what protects you
// at once, so it goes first and stands even if the report can't be sent.
async function doBlock() {
    const peerId = S.blocking;
    if (!peerId) return;
    const also = $('#block-report').checked && S.gate !== 'off';
    const why = $('#block-reason').value.trim();
    if (also && !why) { $('#block-error').textContent = 'Say what happened, in a sentence or two.'; return; }
    $('#block-go').disabled = true;
    try {
        await backend.block(peerId);
    } catch (e) {
        $('#block-error').textContent = errText(e);
        $('#block-go').disabled = false;
        return;
    }
    $('#block-modal').hidden = true;
    S.blocking = null;
    $('#block-go').disabled = false;
    let msg = 'Blocked';
    if (also) {
        try {
            await backend.report(peerId, why, $('#block-attach').checked ? 10 : 0);
            msg = 'Blocked and reported';
        } catch (e) {
            msg = 'Blocked, but the report didn\u2019t send: ' + errText(e);
        }
    }
    await refreshList();
    renderMain();
    toast(msg);
}

async function doUnblock(peerId) {
    try {
        await backend.unblock(peerId);
        await refreshList();
        renderMain();
        toast('Unblocked');
    } catch (e) {
        toast(errText(e));
    }
}

// Sending shows the message at once. It goes to the other person's device
// straight away; storing it on the network for the record takes a fraction
// of a second more, and until then the bubble is drawn as "on its way".
// Messages to one person go out strictly in the order they were written.
const sendQueue = {};
const earlyDelivered = new Set();

function doSend(ta, btn) {
    const text = ta.value.trim();
    const peerId = S.active;
    if (!text || !peerId) return;
    ta.value = '';
    delete S.drafts[peerId];
    btn.disabled = true;
    const item = { id: 'pending-' + Date.now() + '-' + Math.random(), outbound: true, text,
                   sent: Math.floor(Date.now() / 1000), pending: true };
    (S.pending[peerId] = S.pending[peerId] || []).push(item);
    renderMain();
    queueSend(peerId, item);
}

function queueSend(peerId, item) {
    sendQueue[peerId] = (sendQueue[peerId] || Promise.resolve()).then(() => attemptSend(peerId, item));
}

async function attemptSend(peerId, item) {
    try {
        const m = await backend.send(peerId, item.text);
        const list = S.pending[peerId] || [];
        S.pending[peerId] = list.filter(x => x !== item);
        if (earlyDelivered.has(m.id)) m.delivered = true;
        if (S.active === peerId && !S.thread.some(x => x.id === m.id)) S.thread.push(m);
    } catch (e) {
        item.failed = true;
        item.error = errText(e);
        toast('Not sent — ' + item.error);
    }
    await refreshList();
    if (S.active === peerId) renderMain();
}

function retryFailed() {
    Object.keys(S.pending).forEach(peerId => {
        (S.pending[peerId] || []).filter(m => m.failed).forEach(m => retrySend(peerId, m));
    });
}

function retrySend(peerId, item) {
    item.failed = false;
    item.error = '';
    renderMain();
    queueSend(peerId, item);
}

async function doAccept(peerId) {
    try {
        await backend.accept(peerId);
        const was = S.convs.find(x => x.peerId === peerId);
        if (isWorkspace(was)) { S.ws = peerId; store(WS_KEY, peerId); S.people = []; }
        await refreshList();
        await open(peerId);
    } catch (e) {
        toast(errText(e));
    }
}

async function doDecline(peerId) {
    try {
        await backend.decline(peerId);
        S.active = null;
        S.thread = [];
        await refreshList();
        renderMain();
        toast(peerId.startsWith('g:') ? 'Invitation declined' : 'Request declined');
    } catch (e) {
        toast(errText(e));
    }
}

/* ─────────────────────────────  calls  ───────────────────────────── */

let ringCtx = null, ringTimer = null;

// A plain two-tone ring from the audio API: no sound file to ship. If the
// window isn't allowed to make sound yet, the call bar and notification
// still announce the call.
function startRing() {
    if (ringTimer) return;
    const beep = () => {
        try {
            ringCtx = ringCtx || new (window.AudioContext || window.webkitAudioContext)();
            if (ringCtx.state === 'suspended') ringCtx.resume().catch(() => {});
            [0, 0.28].forEach((at, i) => {
                const o = ringCtx.createOscillator(), g = ringCtx.createGain();
                o.type = 'sine'; o.frequency.value = i ? 660 : 520;
                g.gain.setValueAtTime(0.0001, ringCtx.currentTime + at);
                g.gain.exponentialRampToValueAtTime(0.12, ringCtx.currentTime + at + 0.02);
                g.gain.exponentialRampToValueAtTime(0.0001, ringCtx.currentTime + at + 0.24);
                o.connect(g); g.connect(ringCtx.destination);
                o.start(ringCtx.currentTime + at); o.stop(ringCtx.currentTime + at + 0.26);
            });
        } catch (_) {}
    };
    beep();
    ringTimer = setInterval(beep, 2200);
}
function stopRing() { clearInterval(ringTimer); ringTimer = null; }

const clockText = (sec) => Math.floor(sec / 60) + ':' + String(sec % 60).padStart(2, '0');

function callStatus(s) {
    return s.phase === 'active' ? clockText(s.seconds) : s.note;
}

// One bar for whichever call is going: a one-to-one call, or a group call.
function drawBars() {
    const one = calls.state();
    const grp = groupCalls.state();
    if (one.phase === 'idle' && grp.phase !== 'idle') { renderGroupBar(grp); return; }
    renderCallBar(one);
}

function renderCallBar(s) {
    const bar = $('#callbar');
    if (s.name) $('#stage-name').textContent = s.name;
    $('#stage-grid').hidden = true;
    if (s.phase === 'idle') {
        bar.hidden = true; bar.dataset.phase = ''; bar.classList.remove('staged');
        $('#stage').hidden = true;
        stopRing();
        return;
    }
    if (s.phase !== 'ringing') stopRing();
    bar.hidden = false;
    bar.classList.toggle('live', s.phase === 'active');

    // The clock ticks every second; only its text changes, so leave the
    // buttons alone (rebuilding them would eat a click landing mid-tick).
    // A video call, or any call where a camera is on, gets the stage.
    const staged = s.phase !== 'ringing' && (s.camera || s.remoteCamera);
    $('#stage').hidden = !staged;
    bar.classList.toggle('staged', staged);

    const shape = [s.phase, s.muted, s.camera, s.video, staged].join('|');
    if (bar.dataset.phase === shape) {
        bar.querySelector('.status').textContent = callStatus(s);
        return;
    }
    bar.dataset.phase = shape;
    bar.replaceChildren();
    bar.appendChild(el('div', 'who', s.name));
    bar.appendChild(el('div', 'status', callStatus(s)));
    const btns = el('div', 'btns');
    const button = (label, cls, fn) => { const b = el('button', 'cbtn ' + cls, label); b.onclick = fn; btns.appendChild(b); };
    if (s.phase === 'ringing') {
        // Answering a video call never turns the camera on by itself: the
        // person picks, and can always answer with sound only.
        if (s.video) {
            button('Video', '', () => { calls.answer(true); });
            button('Audio only', 'quiet', () => { calls.answer(false); });
        } else {
            button('Answer', '', () => { calls.answer(false); });
        }
        button('Decline', 'end', () => { calls.decline(); });
    } else {
        if (s.phase === 'active' || s.phase === 'connecting') {
            button(s.muted ? 'Unmute' : 'Mute', 'quiet' + (s.muted ? ' on' : ''), () => { calls.toggleMute(); });
            button(s.camera ? 'Camera off' : 'Camera', 'quiet' + (s.camera ? ' on' : ''), () => { calls.toggleCamera(); });
        }
        button('Hang up', 'end', () => { calls.hangup(); });
    }
    bar.appendChild(btns);
}

// Puts the two camera pictures on the stage. The video elements are
// permanent (in index.html), so a picture never flickers on a re-render.
function showVideo(local, remote) {
    const lv = $('#local-video'), rv = $('#remote-video');
    if (lv.srcObject !== local) lv.srcObject = local;
    if (rv.srcObject !== remote) rv.srcObject = remote;
    lv.hidden = !local;
    rv.hidden = !remote;
    $('#stage-wait').hidden = !!remote;
    [lv, rv].forEach(v => { if (v.srcObject) { const p = v.play(); if (p && p.catch) p.catch(() => {}); } });
}

const calls = window.RelayCalls.create({
    send:    (peerId, op, callId, sdp, video) => backend.sendCall(peerId, op, callId, sdp, video),
    servers: () => backend.callServers().then(x => x || []),
    getMedia: () => backend.live
        ? navigator.mediaDevices.getUserMedia({ audio: { echoCancellation: true, noiseSuppression: true, autoGainControl: true } })
        : Promise.resolve(fakeStream()),
    getCamera: () => backend.live
        ? navigator.mediaDevices.getUserMedia({ video: { width: { ideal: 1280 }, height: { ideal: 720 }, frameRate: { ideal: 30 } } })
        : Promise.resolve(fakeCamera()),
    RTC: backend.live ? undefined : FakeRTC,
    showVideo,
    onChange: () => drawBars(),
    isBusy: () => groupCalls.state().phase !== 'idle',
    onEnded: (text) => toast(text),
    onNotice: (text) => toast(text),
    onRing: () => startRing(),
});

function renderGroupBar(s) {
    const bar = $('#callbar');
    if (s.phase !== 'ringing') stopRing();
    bar.hidden = false;
    bar.classList.toggle('live', s.phase === 'active' && s.count > 1);
    const staged = s.phase === 'active' && s.anyCamera;
    $('#stage').hidden = !staged;
    $('#stage-grid').hidden = !staged;
    ['#remote-video', '#local-video', '#stage-wait'].forEach(sel => { $(sel).hidden = true; });
    bar.classList.toggle('staged', staged);
    const status = s.phase === 'ringing' ? s.note
        : s.count > 1 ? s.count + ' in call · ' + clockText(s.seconds) : s.note;
    const shape = ['g', s.phase, s.muted, s.camera, s.video, staged].join('|');
    if (bar.dataset.phase === shape) {
        bar.querySelector('.status').textContent = status;
        return;
    }
    bar.dataset.phase = shape;
    bar.replaceChildren();
    bar.appendChild(el('div', 'who', s.groupName));
    bar.appendChild(el('div', 'status', status));
    const btns = el('div', 'btns');
    const button = (label, cls, fn) => { const b = el('button', 'cbtn ' + cls, label); b.onclick = fn; btns.appendChild(b); };
    if (s.phase === 'ringing') {
        button('Join', '', () => joinGroupCall(s.callId, false));
        button('Join with video', 'quiet', () => joinGroupCall(s.callId, true));
        button('Not now', 'end', () => { groupCalls.decline(); });
    } else {
        button(s.muted ? 'Unmute' : 'Mute', 'quiet' + (s.muted ? ' on' : ''), () => { groupCalls.toggleMute(); });
        button(s.camera ? 'Camera off' : 'Camera', 'quiet' + (s.camera ? ' on' : ''), () => { groupCalls.toggleCamera(); });
        button('Leave', 'end', () => { groupCalls.leave(); });
    }
    bar.appendChild(btns);
}

// The video grid for a group call. Each person's tile and video element is
// kept between redraws, so pictures never flicker.
const tileEls = new Map();
function showTiles(tiles) {
    const grid = $('#stage-grid');
    const keep = new Set(tiles.map(t => t.id));
    for (const [id, node] of tileEls) {
        if (!keep.has(id)) { node.remove(); tileEls.delete(id); }
    }
    grid.dataset.count = String(tiles.length);
    tiles.forEach(t => {
        let node = tileEls.get(t.id);
        if (!node) {
            node = el('div', 'tile' + (t.me ? ' me' : ''));
            const v = document.createElement('video');
            v.autoplay = true; v.playsInline = true; v.muted = true; // sound comes separately
            node.appendChild(v);
            node.appendChild(el('div', 'tname', t.name));
            tileEls.set(t.id, node);
        }
        grid.appendChild(node); // also keeps the order
        const v = node.querySelector('video');
        if (v.srcObject !== t.stream) v.srcObject = t.stream;
        v.hidden = !t.stream;
        node.classList.toggle('cam', !!t.stream);
        node.querySelector('.tname').textContent = t.name;
        if (t.stream) { const p = v.play(); if (p && p.catch) p.catch(() => {}); }
    });
}

// One hidden <audio> per person in a group call.
const groupAudio = new Map();
function playGroupAudio(id, stream) {
    let a = groupAudio.get(id);
    if (!a) {
        a = document.createElement('audio');
        a.autoplay = true;
        a.style.display = 'none';
        document.body.appendChild(a);
        groupAudio.set(id, a);
    }
    a.srcObject = stream;
    const p = a.play(); if (p && p.catch) p.catch(() => {});
}
function stopGroupAudio(id) {
    const a = groupAudio.get(id);
    if (a) { a.srcObject = null; a.remove(); groupAudio.delete(id); }
}

const groupCalls = window.RelayGroupCalls.create({
    me: () => (S.me && S.me.username) || '',
    send: (group, to, op, callId, sdp, video) => backend.sendGroupCall(group, to, op, callId, sdp, video),
    servers: () => backend.callServers().then(x => x || []),
    getMedia: () => backend.live
        ? navigator.mediaDevices.getUserMedia({ audio: { echoCancellation: true, noiseSuppression: true, autoGainControl: true } })
        : Promise.resolve(fakeStream()),
    getCamera: () => backend.live
        ? navigator.mediaDevices.getUserMedia({ video: { width: { ideal: 640 }, height: { ideal: 360 }, frameRate: { ideal: 24 } } })
        : Promise.resolve(fakeCamera()),
    RTC: backend.live ? undefined : FakeRTC,
    isBusy: () => calls.state().phase !== 'idle',
    playRemote: playGroupAudio,
    stopRemote: stopGroupAudio,
    showTiles,
    onChange: () => drawBars(),
    onEnded: (text) => toast(text),
    onNotice: (text) => toast(text),
    onRing: () => startRing(),
    // A call started or ended somewhere: the group's "Join" banner may change.
    onRunning: (group) => { if (S.active === group) renderMain(); },
});

async function startGroupCall(c, video) {
    try {
        await groupCalls.start(c.peerId, c.name, video);
    } catch (e) {
        toast(errText(e));
    }
}

async function joinGroupCall(callId, video) {
    try {
        await groupCalls.join(callId, video);
    } catch (e) {
        toast(errText(e));
    }
    if (S.active && S.active.startsWith('g:')) renderMain();
}

async function startCall(c, video) {
    try {
        await calls.call(c.peerId, c.name, video);
    } catch (e) {
        toast(errText(e));
    }
}

/* ───────────────────────────── workspaces ────────────────────────── */

function openCreateWs() {
    $('#ws-name').value = '';
    $('#ws-people').value = '';
    $('#ws-error').textContent = '';
    $('#ws-modal').hidden = false;
    $('#ws-name').focus();
}

async function submitCreateWs() {
    const name = $('#ws-name').value.trim();
    const who = $('#ws-people').value.trim();
    if (!name) { $('#ws-error').textContent = 'Give the workspace a name.'; return; }
    $('#ws-go').disabled = true;
    try {
        const c = await backend.createWorkspace(name, who);
        $('#ws-modal').hidden = true;
        S.ws = c.peerId;
        store(WS_KEY, c.peerId);
        S.people = [];
        S.wsMenu = false;
        await refreshList();
        if (S.mode !== 'business') await setMode('business');
        await loadPeople();
        await open(c.peerId);
        toast(who ? 'Workspace created \u2014 invitations sent' : 'Workspace created \u2014 invite your team with Invite people');
    } catch (e) {
        $('#ws-error').textContent = errText(e);
    } finally {
        $('#ws-go').disabled = false;
    }
}

async function openNewChannel() {
    const ws = currentWs();
    if (!ws) return;
    $('#channel-name').value = '';
    $('#channel-error').textContent = '';
    const box = $('#channel-people');
    box.replaceChildren();
    let people = [];
    try { people = await backend.groupMembers(ws.peerId) || []; } catch (_) {}
    const others = people.filter(p => !p.me);
    if (!others.length) {
        box.appendChild(el('div', 'none', 'Nobody else is in this workspace yet. You can make the channel now and add people later.'));
    }
    others.forEach(p => {
        const label = el('label', 'check');
        const cb = document.createElement('input');
        cb.type = 'checkbox';
        cb.value = p.peerId;
        label.appendChild(cb);
        label.appendChild(el('span', null, p.name + (p.pending ? ' (hasn\u2019t joined yet)' : '')));
        box.appendChild(label);
    });
    $('#channel-all').hidden = !others.length;
    $('#channel-all').textContent = 'Select everyone';
    $('#channel-modal').hidden = false;
    $('#channel-name').focus();
}

function toggleEveryone() {
    const boxes = [...document.querySelectorAll('#channel-people input')];
    const all = boxes.every(b => b.checked);
    boxes.forEach(b => { b.checked = !all; });
    $('#channel-all').textContent = all ? 'Select everyone' : 'Clear';
}

async function submitNewChannel() {
    const ws = currentWs();
    if (!ws) return;
    const name = $('#channel-name').value.trim().replace(/^#+/, '').replace(/\s+/g, '-').toLowerCase();
    if (!name) { $('#channel-error').textContent = 'Give the channel a name.'; return; }
    const ids = [...document.querySelectorAll('#channel-people input:checked')].map(i => i.value);
    $('#channel-go').disabled = true;
    try {
        const c = await backend.createChannel(ws.peerId, name, ids.join(' '));
        $('#channel-modal').hidden = true;
        await refreshList();
        await open(c.peerId);
    } catch (e) {
        $('#channel-error').textContent = errText(e);
    } finally {
        $('#channel-go').disabled = false;
    }
}

/* ───────────────────────────── the tour ──────────────────────────── */

// Static artwork for the tour: hard-coded markup, no user data. Cyan is the
// subject; the faint grey is context.
const SVG_OPEN = '<svg viewBox="0 0 300 120" fill="none" stroke="currentColor" stroke-width="1.6" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true">';
function meetingTiles() {
    let out = '';
    [[56, 10], [154, 10], [56, 64], [154, 64]].forEach(([x, y], i) => {
        out += '<g' + (i ? ' class="dim"' : '') + '><rect x="' + x + '" y="' + y + '" width="90" height="46" rx="10"' +
            (i ? '' : ' fill="currentColor" fill-opacity=".14"') + '/>' +
            '<circle cx="' + (x + 45) + '" cy="' + (y + 18) + '" r="7"/>' +
            '<path d="M' + (x + 29) + ' ' + (y + 38) + 'q16 -14 32 0"/></g>';
    });
    return out;
}
const TOUR_ART = {
    network: SVG_OPEN +
        '<g class="dim" opacity=".8"><path d="M56 84L118 38L190 72L246 34M118 38L148 100L190 72M56 84L148 100"/></g>' +
        '<circle cx="56" cy="84" r="9"/><circle cx="118" cy="38" r="9"/>' +
        '<circle cx="190" cy="72" r="12" fill="currentColor" fill-opacity=".16"/>' +
        '<circle cx="246" cy="34" r="8"/><circle cx="148" cy="100" r="7"/></svg>',
    chat: SVG_OPEN +
        '<rect x="46" y="14" width="150" height="40" rx="14"/><path d="M66 54l-6 14 20-14"/>' +
        '<g class="dim"><path d="M64 28h74M64 41h46"/></g>' +
        '<rect x="104" y="66" width="150" height="38" rx="14" fill="currentColor" fill-opacity=".14"/>' +
        '<g class="dim"><path d="M122 79h78M122 91h40"/></g></svg>',
    switch: SVG_OPEN +
        '<circle cx="150" cy="22" r="12" fill="currentColor" fill-opacity=".16"/><path d="M144 22l4 4 8-9"/>' +
        '<path class="dim" d="M150 36V50M150 50L103 64M150 50L199 64"/>' +
        '<rect x="52" y="66" width="196" height="40" rx="13"/>' +
        '<rect x="57" y="71" width="92" height="30" rx="9" fill="currentColor" fill-opacity=".18" stroke="none"/>' +
        '<text x="103" y="91" text-anchor="middle" font-size="13" fill="currentColor" stroke="none">Personal</text>' +
        '<g class="dim"><text x="199" y="91" text-anchor="middle" font-size="13" fill="currentColor" stroke="none">Business</text></g></svg>',
    workspace: SVG_OPEN +
        '<rect class="dim" x="40" y="8" width="220" height="104" rx="12"/><path class="dim" d="M118 8V112"/>' +
        '<rect x="46" y="22" width="66" height="20" rx="6" fill="currentColor" fill-opacity=".16" stroke="none"/>' +
        '<text x="56" y="36" font-size="12" fill="currentColor" stroke="none"># general</text>' +
        '<g class="dim"><text x="56" y="62" font-size="12" fill="currentColor" stroke="none"># design</text>' +
        '<text x="56" y="86" font-size="12" fill="currentColor" stroke="none"># launch</text></g>' +
        '<rect x="134" y="24" width="96" height="22" rx="9"/>' +
        '<rect x="156" y="58" width="94" height="22" rx="9" fill="currentColor" fill-opacity=".14"/>' +
        '<rect class="dim" x="134" y="90" width="62" height="14" rx="7"/></svg>',
    meeting: SVG_OPEN + meetingTiles() + '</svg>',
    shield: SVG_OPEN +
        '<path d="M150 8L98 26v34c0 30 22 46 52 54 30-8 52-24 52-54V26z" fill="currentColor" fill-opacity=".10"/>' +
        '<path d="M128 62l16 16 28-32"/></svg>',
};

const TOUR = [
    { art: 'network', title: 'Welcome to AltNet Relay',
      body: 'Private messages and calls with the people you choose. Nothing you write sits on a company\u2019s server: it\u2019s encrypted on your computer and passed along the AltNet network.',
      points: ['One account for everything, personal and work', 'Your @name is how people find you \u2014 no phone number needed'] },
    { art: 'chat', title: 'Talk to anyone by name',
      body: 'Press Add someone and type their @name. When a stranger writes to you first, it arrives as a request: they can send three messages, and you decide whether to accept. They\u2019re never told what you chose.',
      points: ['See when someone is typing, and when your message has arrived', 'Block or report anyone, quietly'] },
    { art: 'switch', title: 'Personal and Business, one account',
      body: 'The switch at the top of the sidebar flips the whole app between your two lives. Personal is your friends and groups. Business is your workplace. Same name, same account, nothing to sign in to twice.',
      points: ['A dot on the switch means the other side needs you', 'Work chats never show up in your personal list, and the other way round'] },
    { art: 'workspace', title: 'Workspaces: your company\u2019s room',
      body: 'In Business, anyone can create a workspace for their company and invite teammates by @name. Each person accepts or declines \u2014 nobody is added without saying yes.',
      points: ['Every workspace starts with #general for everyone', 'Make channels like #design or #launch for just the people who need them', 'Message any teammate directly \u2014 no request needed', 'The workspace\u2019s creator decides who stays: report a coworker to them, and they can remove them'] },
    { art: 'meeting', title: 'Calls and video meetings',
      body: 'Start a voice or video meeting in any channel, or call a teammate directly. Everyone in the channel gets a ring and can join. Calls go straight between computers; AltNet\u2019s relay only steps in when a router gets in the way, and it can\u2019t see or hear a thing.',
      points: ['Up to 8 people in one meeting', 'Your camera stays off until you turn it on'] },
    { art: 'shield', title: 'You\u2019re ready',
      body: 'That\u2019s everything. Start with a friend, or set up your company\u2019s workspace now. You can replay this tour any time from Account.',
      points: [], last: true },
];

let tourAt = 0;
let tourThen = null;

function startTour(then) {
    tourAt = 0;
    tourThen = then || null;
    $('#tour').hidden = false;
    drawTour();
}

function drawTour() {
    const step = TOUR[tourAt];
    $('#tour-art').innerHTML = TOUR_ART[step.art]; // static markup, not user data
    $('#tour-title').textContent = step.title;
    $('#tour-body').textContent = step.body;
    const pts = $('#tour-points');
    pts.replaceChildren();
    step.points.forEach(t => pts.appendChild(el('li', null, t)));
    const dots = $('#tour-dots');
    dots.replaceChildren();
    TOUR.forEach((_, i) => dots.appendChild(el('i', i === tourAt ? 'on' : '')));
    $('#tour-back').hidden = tourAt === 0;
    $('#tour-skip').hidden = !!step.last;
    $('#tour-alt').hidden = !step.last;
    $('#tour-alt').textContent = 'Set up a workspace';
    $('#tour-next').textContent = step.last ? 'Start with Personal' : 'Next';
    $('#tour-next').focus();
}

// Ending the tour for any reason counts as having seen it.
function endTour(then) {
    if ($('#tour').hidden) return;
    $('#tour').hidden = true;
    store(TOUR_KEY, '1');
    const done = tourThen;
    tourThen = null;
    if (done) done();
    if (then) then();
}

// Put the tour away without counting it as seen or running what follows it.
function abandonTour() {
    $('#tour').hidden = true;
    tourThen = null;
}

function tourNext() {
    if (TOUR[tourAt].last) { endTour(); return; }
    tourAt++;
    drawTour();
}

function tourBack() {
    if (tourAt === 0) return;
    tourAt--;
    drawTour();
}

/* ──────────────────────── sign-in and account ────────────────────── */

// applyGate swaps the whole window between the app, the sign-in screens and
// the banned notice. It runs at startup and whenever the backend says the
// account changed underneath us (a session ended, a ban landed).
async function applyGate(a) {
    const was = S.gate;
    S.acct = a;
    S.gate = a.gate;
    $('#gate').hidden = a.gate !== 'signed_out';
    $('#banned').hidden = a.gate !== 'banned';
    if (a.gate === 'banned') {
        $('#banned-reason').textContent = a.reason || 'This account has been banned from AltNet Relay.';
    }
    if (a.gate === 'signed_out' || a.gate === 'banned') { calls.hangup(); groupCalls.leave(); }
    if (a.gate === 'signed_out' && was !== 'signed_out') {
        closeAllModals();
        showAuth('signin');
    }
    // Why we are signed out, when it wasn't the person's own doing (another
    // computer took the account over).
    const why = $('#gate-reason');
    why.hidden = !(a.gate === 'signed_out' && a.reason);
    why.textContent = a.gate === 'signed_out' ? (a.reason || '') : '';
    // Getting in -- at startup, or after signing in again -- loads the app.
    if ((a.gate === 'ok' || a.gate === 'off') && was !== 'ok' && was !== 'off') {
        await enterApp();
    }
}

const AUTH = { email: '' };

function showAuth(mode, keepError) {
    document.querySelectorAll('#gate .gate-form').forEach(f => {
        f.hidden = f.dataset.mode !== mode;
        if (f.dataset.mode === mode) {
            if (!keepError) f.querySelector('.error').textContent = '';
            const first = f.querySelector('input:not([type=checkbox])');
            if (first) setTimeout(() => first.focus(), 0);
        }
    });
    if (mode === 'verify') {
        $('#gate .verify-hint').textContent = 'We sent a code to ' + AUTH.email + '. Type it here.';
    }
}

// Runs a form's action with its button disabled, showing any refusal under
// the fields in the server's own words.
async function submitAuth(form, fn) {
    const err = form.querySelector('.error');
    const btn = form.querySelector('button[type=submit]');
    err.textContent = '';
    btn.disabled = true;
    try {
        await fn();
    } catch (e) {
        err.textContent = errText(e);
    } finally {
        btn.disabled = false;
    }
}

function wireAuth() {
    const form = (mode) => $('#gate .gate-form[data-mode="' + mode + '"]');
    const val = (f, name) => f.elements[name].value.trim();
    const badEmail = (e) => !/^[^@\s]+@[^@\s]+\.[^@\s]+$/.test(e);

    document.querySelectorAll('#gate [data-go]').forEach(b => {
        b.onclick = () => showAuth(b.dataset.go);
    });

    const signin = form('signin');
    signin.onsubmit = (ev) => {
        ev.preventDefault();
        submitAuth(signin, async () => {
            const email = val(signin, 'email'), pw = signin.elements.password.value;
            if (badEmail(email)) throw new Error('Type the email you signed up with.');
            if (!pw) throw new Error('Type your password.');
            AUTH.email = email;
            let status;
            try {
                status = await backend.signIn(email, pw);
            } catch (e) {
                // Signed up but never typed the code: pick up where they left off.
                if (/not verified/i.test(errText(e))) {
                    showAuth('verify');
                    form('verify').querySelector('.error').textContent = 'This account isn\'t verified yet. Type the code we emailed you.';
                    return;
                }
                throw e;
            }
            if (status === 'restart') {
                signin.elements.password.value = '';
                showAuth('moving');
                return;
            }
            if (status === 'no_copy' || status === 'old_copy') {
                AUTH.pw = pw; // kept only until they choose; cleared below
                $('#fresh-why').textContent = status === 'no_copy'
                    ? 'Your account is set up on another computer, and it isn\u2019t keeping a copy to bring here (the copy is turned off in Account there).'
                    : 'Your account\u2019s copy is still locked with an older password. Sign in once on your other computer to update it, then sign in here again \u2014 or start fresh here.';
                showAuth('fresh');
                return;
            }
            signin.elements.password.value = '';
            await applyGate(await backend.account());
        });
    };

    const fresh = form('fresh');
    fresh.onsubmit = (ev) => {
        ev.preventDefault();
        submitAuth(fresh, async () => {
            await backend.startFresh(AUTH.email, AUTH.pw || '');
            AUTH.pw = '';
            signin.elements.password.value = '';
            await applyGate(await backend.account());
        });
    };
    fresh.querySelector('[data-go]').addEventListener('click', () => { AUTH.pw = ''; });

    const signup = form('signup');
    signup.onsubmit = (ev) => {
        ev.preventDefault();
        submitAuth(signup, async () => {
            const email = val(signup, 'email'), pw = signup.elements.password.value;
            if (badEmail(email)) throw new Error('That doesn\'t look like an email address.');
            if (pw.length < 8) throw new Error('Use at least 8 characters for your password.');
            if (!signup.elements.agree.checked) throw new Error('Tick the box to agree to the Terms and Privacy Policy.');
            AUTH.email = email;
            await backend.signUp(email, pw);
            signup.elements.password.value = '';
            showAuth('verify');
        });
    };

    const verify = form('verify');
    verify.onsubmit = (ev) => {
        ev.preventDefault();
        submitAuth(verify, async () => {
            const code = val(verify, 'code');
            if (!code) throw new Error('Type the code from the email.');
            const status = await backend.verifyEmail(AUTH.email, code);
            verify.elements.code.value = '';
            if (status === 'restart') {
                showAuth('moving');
                return;
            }
            await applyGate(await backend.account());
        });
    };

    const reset = form('reset');
    reset.onsubmit = (ev) => {
        ev.preventDefault();
        submitAuth(reset, async () => {
            const email = val(reset, 'email');
            if (badEmail(email)) throw new Error('Type the email you signed up with.');
            AUTH.email = email;
            await backend.requestReset(email);
            showAuth('reset2');
        });
    };

    const reset2 = form('reset2');
    reset2.onsubmit = (ev) => {
        ev.preventDefault();
        submitAuth(reset2, async () => {
            const code = val(reset2, 'code'), pw = reset2.elements.password.value;
            if (!code) throw new Error('Type the code from the email.');
            if (pw.length < 8) throw new Error('Use at least 8 characters for your password.');
            await backend.confirmReset(AUTH.email, code, pw);
            reset2.elements.code.value = '';
            reset2.elements.password.value = '';
            showAuth('signin');
            toast('Password changed. Sign in with the new one.');
        });
    };

    document.querySelectorAll('[data-terms]').forEach(b => { b.onclick = openTerms; });
    $('#banned-signout').onclick = doSignOut;
}

async function doSignOut() {
    try {
        await backend.signOut();
        closeAllModals();
        await applyGate(await backend.account());
    } catch (e) {
        toast(errText(e));
    }
}

function openAccount() {
    const a = S.acct || {};
    $('#acct-email').textContent = a.email || '';
    $('#acct-handle').textContent = (S.me && S.me.handle) ? '@' + S.me.handle : 'none yet';
    $('#acct-offline').hidden = !a.offline;
    $('#set-msg').textContent = '';
    $('#set-msg').className = 'note';
    loadSettings();
    loadCopy();
    $('#account-modal').hidden = false;
}

/* ─────────────────────────── deleting the account ──────────────────────── */

// Four steps, each checked by the server as it is reached, then one last
// question. Nothing is deleted until "Delete forever".
const DEL_STEPS = ['intro', 'code', 'password', 'name', 'sure', 'done'];
let delStep = 'intro';

function openDeleteAccount() {
    $('#account-modal').hidden = true;
    const email = (S.acct && S.acct.email) || 'your email address';
    $('#del-email').textContent = email;
    $('#del-email2').textContent = email;
    ['#del-code', '#del-password', '#del-name'].forEach(s => { $(s).value = ''; });
    const handle = S.me && S.me.handle;
    $('#del-name-title').textContent = handle ? 'Type your name' : 'Type your email address';
    $('#del-name-hint').textContent = handle
        ? 'To be sure, type your name exactly: @' + handle
        : 'You never chose a name, so type your email address: ' + email;
    $('#del-name').placeholder = handle ? '@' + handle : email;
    showDelStep('intro');
    $('#delete-modal').hidden = false;
}

function showDelStep(step) {
    delStep = step;
    DEL_STEPS.forEach(s => { $('#delete-modal [data-step="' + s + '"]').hidden = s !== step; });
    $('#del-error').textContent = '';
    const next = $('#del-next'), cancel = $('#del-cancel');
    $('#del-actions').hidden = step === 'done';
    next.className = step === 'sure' ? 'btn danger' : 'btn';
    next.textContent = { intro: 'Email me a code', sure: 'Delete forever' }[step] || 'Next';
    cancel.textContent = step === 'sure' ? 'Keep my account' : 'Cancel';
    const field = { code: '#del-code', password: '#del-password', name: '#del-name' }[step];
    if (field) setTimeout(() => $(field).focus(), 0);
}

async function delNext() {
    const code = $('#del-code').value.trim(), pw = $('#del-password').value, name = $('#del-name').value.trim();
    const next = $('#del-next');
    if (next.disabled) return;
    next.disabled = true;
    $('#del-error').textContent = '';
    try {
        switch (delStep) {
        case 'intro':
            await backend.startAccountDeletion();
            showDelStep('code');
            break;
        case 'code':
            if (!/^[0-9]{6}$/.test(code)) throw new Error('the code is the 6 digits from the email');
            await backend.checkAccountDeletion(code, '', '');
            showDelStep('password');
            break;
        case 'password':
            if (!pw) throw new Error('type your password');
            await backend.checkAccountDeletion(code, pw, '');
            showDelStep('name');
            break;
        case 'name':
            if (!name) throw new Error((S.me && S.me.handle) ? 'type your name' : 'type your email address');
            await backend.checkAccountDeletion(code, pw, name);
            showDelStep('sure');
            break;
        case 'sure':
            await backend.deleteAccount(code, pw, name);
            showDelStep('done');
            setTimeout(() => { backend.quit().catch(() => {}); }, 2500);
            break;
        }
    } catch (e) {
        const msg = errText(e);
        // A spent or expired code means asking for a new one.
        if (/expired|new code/i.test(msg)) {
            ['#del-code', '#del-password', '#del-name'].forEach(s => { $(s).value = ''; });
            showDelStep('intro');
        }
        $('#del-error').textContent = msg;
    } finally {
        next.disabled = false;
    }
}

function cancelDelete() {
    $('#delete-modal').hidden = true;
    ['#del-code', '#del-password', '#del-name'].forEach(s => { $(s).value = ''; });
}

/* ─────────────────────── running in the background ───────────────────── */

const lookingNow = () => document.hasFocus() && document.visibilityState === 'visible';
let lastFocus = null;
function reportFocus(force) {
    const now = lookingNow();
    if (!force && now === lastFocus) return;
    lastFocus = now;
    backend.setFocused(now).catch(() => {});
}

function settingNote(text, bad) {
    const n = $('#set-msg');
    n.textContent = text || '';
    n.className = bad ? 'error' : 'note';
}

async function loadSettings() {
    let s;
    try { s = await backend.settings(); } catch (_) { return; }
    $('#set-startup').checked = !!s.startup;
    $('#set-startup').closest('label').hidden = !s.startupAvailable;
    $('#set-background').checked = !!s.background;
    $('#set-notifications').checked = !!s.notifications;
    $('#set-autoupdate').checked = !!s.autoUpdate;
    $('#acct-version').textContent = s.version ? 'Version ' + s.version : '';
    if (s.background && !s.trayAvailable) {
        settingNote('The tray icon couldn\u2019t be added here, so closing the window will minimise it instead.');
    }
}

// ─── a new version ───

function showUpdate(u) {
    S.update = u || null;
    const bar = $('#update-bar');
    if (!u) { bar.hidden = true; return; }
    const t = $('#update-text');
    t.textContent = '';
    const head = el('strong', null, 'AltNet Relay ' + u.version + (u.ready ? ' is ready.' : ' is out.'));
    t.appendChild(head);
    const more = u.ready ? ' Restart to update; your chats stay as they are.' : ' ' + (u.install || 'Get it from the release page.');
    t.appendChild(document.createTextNode(more));
    if (u.notes) { t.appendChild(el('br')); t.appendChild(document.createTextNode(u.notes)); }
    $('#update-go').textContent = u.ready ? 'Restart to update' : 'Open the release page';
    $('#update-go').disabled = false;
    bar.hidden = false;
}

// The app doesn't install an update in the middle of a call.
function reportBusy() {
    backend.setBusy(!$('#callbar').hidden).catch(() => {});
}

// The account's copy: on, off, or waiting for the password once.
async function loadCopy() {
    let c;
    try { c = await backend.copyStatus(); } catch (_) { $('#set-copy').closest('label').hidden = true; return; }
    $('#set-copy').closest('label').hidden = false;
    $('#set-copy').checked = !!c.on;
    $('#copy-unlock').hidden = !(c.on && c.needsPassword);
    $('#copy-pw').value = '';
    const n = $('#copy-note');
    n.className = 'note';
    if (!c.on) {
        n.textContent = 'Off. Your account stays on this computer only; signing in somewhere else starts fresh there.';
    } else if (c.needsPassword) {
        n.textContent = 'Type your password once to lock the copy with it.';
    } else if (c.problem) {
        n.className = 'error';
        n.textContent = 'The copy couldn\u2019t be saved: ' + c.problem;
    } else {
        n.textContent = 'Locked with your password before it leaves this computer, so we can\u2019t open it. ' +
            (c.savedAt ? 'Saved ' + ago(c.savedAt) + '.' : 'Saving shortly.');
    }
}

function ago(t) {
    const s = Math.max(0, Math.floor(Date.now() / 1000) - t);
    if (s < 60) return 'just now';
    if (s < 3600) return Math.floor(s / 60) + ' min ago';
    if (s < 86400) return Math.floor(s / 3600) + ' h ago';
    return new Date(t * 1000).toLocaleDateString();
}

async function changeCopy(input) {
    try {
        await backend.setCopy(input.checked);
    } catch (e) {
        input.checked = !input.checked;
        $('#copy-note').className = 'error';
        $('#copy-note').textContent = errText(e);
        return;
    }
    loadCopy();
}

async function unlockCopy() {
    const pw = $('#copy-pw').value;
    if (!pw) return;
    $('#copy-go').disabled = true;
    try {
        await backend.unlockCopy(pw);
        loadCopy();
    } catch (e) {
        $('#copy-note').className = 'error';
        $('#copy-note').textContent = errText(e);
    } finally {
        $('#copy-go').disabled = false;
        $('#copy-pw').value = '';
    }
}

async function changeSetting(name, input) {
    settingNote('');
    try {
        await backend.setSetting(name, input.checked);
    } catch (e) {
        input.checked = !input.checked;
        settingNote(errText(e), true);
    }
}

async function testNotification() {
    settingNote('');
    try { settingNote(await backend.testNotification()); }
    catch (e) { settingNote(errText(e), true); }
}

/* ──────────────────────────── reporting ──────────────────────────── */

// Reporting a coworker goes to the workspace's owner, not to AltNet.
function openWorkReport(c, ws) {
    openReport(c);
    S.reportingWs = ws.peerId;
    const owner = (S.people.find(p => p.admin) || {}).name || 'the owner';
    $('#report-title').textContent = 'Report ' + c.name + ' to ' + owner;
    $('#report-intro').textContent = 'Tell ' + owner + ', who runs ' + ws.name + ', what happened.';
    $('#report-attach-label').textContent = 'Include what they wrote to you and in #general (the last 10 messages)';
    $('#report-note').textContent = 'Only ' + owner + ' sees this: who you are, who you reported, why, and the messages if you include them. ' +
        owner + ' decides whether to remove them. Nothing goes to AltNet, and ' + c.name + ' isn\u2019t told.';
}

function openReport(c) {
    S.reporting = c.peerId;
    S.reportingWs = null;
    $('#report-intro').textContent = 'Tell us what happened.';
    $('#report-attach-label').textContent = 'Include the last 10 messages from this conversation';
    $('#report-note').textContent = 'Messages are end-to-end encrypted, so AltNet can\u2019t read them. If you include them, they are sent to AltNet for this report only. The person you are reporting isn\u2019t told. When three different people report the same account, it is banned automatically; a ban can always be appealed at abuse.report@panmox.org.';
    $('#report-title').textContent = 'Report ' + c.name;
    $('#report-reason').value = '';
    $('#report-attach').checked = true;
    $('#report-error').textContent = '';
    $('#report-form').hidden = false;
    $('#report-done').hidden = true;
    $('#report-modal').hidden = false;
    $('#report-reason').focus();
}

async function submitReport() {
    const why = $('#report-reason').value.trim();
    if (!why) { $('#report-error').textContent = 'Say what happened, in a sentence or two.'; return; }
    $('#report-go').disabled = true;
    try {
        if (S.reportingWs) {
            await backend.reportCoworker(S.reportingWs, S.reporting, why, $('#report-attach').checked);
            const owner = (S.people.find(p => p.admin) || {}).name || 'The owner';
            $('#report-ack').textContent = 'Sent to ' + owner + '. They decide what happens; the person you reported isn\u2019t told.';
            $('#report-block').hidden = true; // at work, there is no blocking
            $('#report-form').hidden = true;
            $('#report-done').hidden = false;
            return;
        }
        const ack = await backend.report(S.reporting, why, $('#report-attach').checked ? 10 : 0);
        $('#report-ack').textContent = ack || 'Thanks. We have received your report.';
        const c = S.convs.find(x => x.peerId === S.reporting);
        // Offer to block only where blocking is still possible.
        $('#report-block').hidden = !c || c.blocked || c.banned;
        if (c) $('#report-block').textContent = 'Block ' + c.name + ' too';
        $('#report-form').hidden = true;
        $('#report-done').hidden = false;
    } catch (e) {
        $('#report-error').textContent = errText(e);
    } finally {
        $('#report-go').disabled = false;
    }
}

/* ───────────────────────── terms and privacy ─────────────────────── */

// Plain-language terms, shown in the app so they work offline and always
// match the version of the app the person is using. Each entry is a heading
// followed by paragraphs; a paragraph that is an array is a bullet list.
const TERMS = [
    ['The short version', [
        'AltNet Relay is free, open-source software published by PANMOX, Prague, Czech Republic (operator: PANMOX). Contact: abuse.report@panmox.org (postal address on request).',
        'Messages and calls are end-to-end encrypted: we cannot read or hear them. We can see your email address, your @name, your internet address when you connect to our servers, reports (with any messages the reporter chose to include) and bans.',
        'This is a summary. The full Terms of Service, Privacy Policy, Community Guidelines and Workspace Terms are at panmox.org/legal/relay and are part of this agreement.',
    ]],
    ['Your account', [
        'You need a free AltNet account (the same one as AltNet Studio) and must be 16 or older. Keep your password to yourself, and use one you use nowhere else.',
        'Signing in on another computer brings your account with you, through a copy of it that your computer locks with your password before it is sent to our server; we cannot open it. It is on by default and can be turned off under Account. Your account is in use on one computer at a time: the previous one signs itself out.',
        'You can choose one @name. It is permanent and cannot be sold or given away. Vulgar or hateful names are refused, and we may remove a name that impersonates someone or is abusive. Anyone who knows your @name can send you up to three messages as a request until you accept.',
    ]],
    ['Rules', [
        'Do not use Relay to harass, threaten or stalk anyone; to share sexual content involving anyone under 18 or intimate images without consent; to promote terrorism, violence or hatred; for spam, scams or malware; to impersonate anyone; to attack the network; or to break the law. Do not report people falsely, and do not get around a ban with another account.',
    ]],
    ['Reports and bans', [
        'If someone abuses you, report them. You choose whether to include the last 10 messages of the conversation (yours and theirs). If you do, they are decrypted on your computer and sent to us for that report only. The person is never told who reported them.',
        'When three different people report the same account within 30 days, the account is banned automatically: no person decides first. Reports do not count from accounts our server first saw under 72 hours ago, from someone who has reported more than five accounts in 30 days, or from someone whose reports led to bans we overturned twice.',
        'A ban stops the account sending messages or calls anywhere on the network and disables its @name. Its Relay ID is put on a public ban list (ID, reason category, date). Messages already on other people’s computers are not deleted. We may also ban an account ourselves after checking reports, or on an official order.',
        'Every ban can be appealed: write to abuse.report@panmox.org within six months. A person reviews it, considers your side, answers within 14 days, and lifts the ban if it was wrong. You may also go to court.',
        'At work it is different. Inside a workspace you report a coworker to the workspace owner, not to us; the owner sees who reported whom, why and any messages included, and decides whether to remove them. We cannot read it. If you own a workspace you must follow the Workspace Terms. In Personal, blocking and reporting to us work for anyone.',
    ]],
    ['What we hold and why', [
        ['Your email address and a scrambled (hashed) password, to run your account (AltNet account service).',
         'Your Relay ID, @name and the dates they were set up, to make names unique and bans work. Your @name is published on the network so people can find you.',
         'Reports you make or that are made about you, with any messages included, the decision and a log of moderation actions, to keep people safe and handle appeals. Included messages are deleted six months after the decision; the rest after two years.',
         'A locked copy of your account (keys, conversations, contacts, groups, workspaces), sealed with your password on your computer, so you can sign in elsewhere; the random ID of the computer your account is in use on. The copy is deleted when you turn it off, start fresh or delete your account.',
         'Short technical logs with internet addresses and times from our servers and call relay, to run and protect them (7 to 14 days).'],
        'Legal basis: the contract with you for running your account and delivering messages; our and other users’ legitimate interest in safety for reports, bans and logs; and legal obligations when an authority lawfully requires something.',
    ]],
    ['How the network sees you', [
        'AltNet is peer-to-peer. Messages for someone who is offline wait as encrypted pieces on other users’ computers and on ours, and your computer stores pieces for others (at most 256 MB; the oldest are removed first). The pieces cannot be read, but the small routing records next to them show which Relay IDs wrote to each other and when. Computers you connect to, and the person you are calling, can see your internet address.',
        'Calls go directly between computers when possible. Otherwise they pass through our call relay, which cannot hear them. To find a route, the app asks our server, or Cloudflare’s public service if ours does not answer, which internet address it sees.',
        'Your conversations are kept on your computer, unencrypted, in the app’s data folder, and in your locked copy on our server. Protect your computer. If someone else used Relay on this computer before you, their files are moved to a “previous” folder there, not deleted.',
    ]],
    ['Updates', [
        'Relay updates itself. About every six hours the app asks our server whether a new version exists; this sends nothing about you except your internet address, and we keep no log of it. On Windows, new versions are downloaded from GitHub and installed when you are not using the app; you can turn this off under Account, and the app then only tells you. On Linux and Mac it only tells you. Every update is signed with PANMOX’s release key and checked again right before it is installed, including one downloaded earlier.',
    ]],
    ['Who else receives data', [
        'Oracle (our server, Frankfurt), Zoho (sending sign-up codes and our email, EU), Cloudflare (DNS and the backup address check) and GitHub (hosting updates; it sees your internet address when you download one). Where data may leave the EU, EU safeguards (Standard Contractual Clauses or the EU–US Data Privacy Framework) apply. We do not sell data, show adverts, use analytics or set cookies.',
    ]],
    ['Your rights', [
        'You can delete your account yourself under Account → Delete account. You can also ask to see, correct, delete, restrict or take a copy of your data, object to how we use it, and have a person review an automatic ban. Write to abuse.report@panmox.org from your account email; we answer within a month. If you are banned we keep a minimal record while the ban lasts. You can complain to the Czech data protection authority (ÚOOÚ, www.uoou.gov.cz) or the one where you live.',
    ]],
    ['The small print', [
        'Relay is a free service. It is provided as it is, without promises that it will always work or that every message arrives. We are not responsible for what other users do, or for indirect loss or lost data, except where the law does not allow us to limit liability: harm to life or health, harm we cause on purpose or by gross negligence, and your rights as a consumer.',
        'Czech law applies, but you keep the protection of the mandatory law where you live, and can go to court there. If an important change is made to these terms, the app will tell you at least 30 days before; if you do not agree, you can stop and delete your account.',
    ]],
];

function openTerms() {
    const body = $('#terms-body');
    if (!body.childElementCount) {
        TERMS.forEach(([title, blocks]) => {
            body.appendChild(el('h3', null, title));
            blocks.forEach(b => {
                if (Array.isArray(b)) {
                    const ul = el('ul');
                    b.forEach(item => ul.appendChild(el('li', null, item)));
                    body.appendChild(ul);
                } else {
                    body.appendChild(el('p', null, b));
                }
            });
        });
    }
    body.scrollTop = 0;
    $('#terms-modal').hidden = false;
}

function closeAllModals() {
    ['#add-modal', '#claim-modal', '#block-modal', '#account-modal', '#report-modal', '#terms-modal',
     '#group-modal', '#members-modal', '#leave-modal', '#ws-modal', '#channel-modal', '#delete-modal']
        .forEach(sel => { $(sel).hidden = true; });
    S.blocking = null;
    S.reporting = null;
    // Last: finishing the tour may open the name dialog, which must stay
    // open. But not when the account just signed out or was banned -- then the
    // tour is simply put away, to be shown again after signing in.
    if (S.gate === 'ok' || S.gate === 'off') endTour();
    else abandonTour();
}

/* ──────────────────────────── modals ─────────────────────────────── */

const SKIP_KEY = 'relay.skipClaim';

function openClaim() {
    $('#claim-error').textContent = '';
    $('#claim-input').value = '';
    $('#claim-modal').hidden = false;
    $('#claim-input').focus();
}

async function submitClaim() {
    const raw = $('#claim-input').value;
    const bad = validHandle(raw);
    if (bad) { $('#claim-error').textContent = bad; return; }
    $('#claim-go').disabled = true;
    try {
        const h = await backend.claim(raw.trim());
        S.me.handle = h;
        $('#claim-modal').hidden = true;
        renderMe();
        toast('You are now @' + h);
    } catch (e) {
        $('#claim-error').textContent = errText(e);
    } finally {
        $('#claim-go').disabled = false;
    }
}

function openAdd() {
    $('#add-error').textContent = '';
    $('#add-input').value = '';
    $('#add-modal').hidden = false;
    $('#add-input').focus();
}

async function submitAdd() {
    const who = $('#add-input').value.trim();
    if (!who) return;
    $('#add-go').disabled = true;
    try {
        const c = await backend.add(who);
        $('#add-modal').hidden = true;
        await refreshList();
        await open(c.peerId);
    } catch (e) {
        $('#add-error').textContent = errText(e);
    } finally {
        $('#add-go').disabled = false;
    }
}

function wireModals() {
    $('#claim-go').onclick = submitClaim;
    $('#claim-skip').onclick = () => {
        try { localStorage.setItem(SKIP_KEY, '1'); } catch (_) {}
        $('#claim-modal').hidden = true;
    };
    $('#claim-input').onkeydown = (e) => { if (e.key === 'Enter') submitClaim(); };
    $('#claim-input').oninput = () => { $('#claim-error').textContent = ''; };

    $('#add-btn').onclick = openAdd;
    $('#add-go').onclick = submitAdd;
    $('#add-cancel').onclick = () => { $('#add-modal').hidden = true; };
    $('#add-input').onkeydown = (e) => { if (e.key === 'Enter') submitAdd(); };

    $('#copy-id').onclick = async () => {
        if (!S.me) return;
        await backend.copy(S.me.handle ? '@' + S.me.handle : S.me.username);
        toast('Copied — send it to someone so they can reach you');
    };

    $('#block-go').onclick = doBlock;
    $('#block-report').onchange = () => {
        $('#block-report-form').hidden = !$('#block-report').checked;
        if ($('#block-report').checked) $('#block-reason').focus();
    };
    $('#block-cancel').onclick = () => { $('#block-modal').hidden = true; S.blocking = null; };

    $('#group-btn').onclick = openNewGroup;
    $('#group-go').onclick = submitNewGroup;
    $('#group-cancel').onclick = () => { $('#group-modal').hidden = true; };
    $('#group-name').onkeydown = (e) => { if (e.key === 'Enter') $('#group-people').focus(); };
    $('#members-close').onclick = () => { $('#members-modal').hidden = true; membersOf = null; };
    $('#members-go').onclick = submitAddMembers;
    $('#rename-go').onclick = submitRename;
    $('#rename-input').onkeydown = (e) => { if (e.key === 'Enter') submitRename(); };
    $('#members-input').onkeydown = (e) => { if (e.key === 'Enter') submitAddMembers(); };
    $('#leave-go').onclick = doLeave;
    $('#leave-cancel').onclick = () => { $('#leave-modal').hidden = true; leaving = null; };

    document.querySelectorAll('#modeswitch button').forEach(b => { b.onclick = () => setMode(b.dataset.mode); });
    $('#ws-btn').onclick = openCreateWs;
    $('#ws-go').onclick = submitCreateWs;
    $('#ws-cancel').onclick = () => { $('#ws-modal').hidden = true; };
    $('#ws-name').onkeydown = (e) => { if (e.key === 'Enter') $('#ws-people').focus(); };
    $('#channel-btn').onclick = openNewChannel;
    $('#channel-go').onclick = submitNewChannel;
    $('#channel-cancel').onclick = () => { $('#channel-modal').hidden = true; };
    $('#channel-all').onclick = toggleEveryone;
    $('#channel-name').onkeydown = (e) => { if (e.key === 'Enter') submitNewChannel(); };
    $('#invite-btn').onclick = () => { const ws = currentWs(); if (ws) openMembers(ws); };

    $('#tour-next').onclick = tourNext;
    $('#tour-back').onclick = tourBack;
    $('#tour-skip').onclick = () => endTour();
    $('#tour-alt').onclick = () => endTour(async () => { await setMode('business'); if (!currentWs()) openCreateWs(); });
    $('#acct-tour').onclick = () => { $('#account-modal').hidden = true; startTour(null); };

    $('#set-startup').onchange = (e) => changeSetting('startup', e.target);
    $('#set-background').onchange = (e) => changeSetting('background', e.target);
    $('#set-notifications').onchange = (e) => changeSetting('notifications', e.target);
    $('#set-test').onclick = testNotification;
    $('#set-autoupdate').onchange = (e) => changeSetting('autoUpdate', e.target);
    $('#update-go').onclick = () => {
        const u = S.update;
        if (!u) return;
        if (u.ready) {
            $('#update-go').disabled = true;
            $('#update-go').textContent = 'Restarting\u2026';
            backend.installUpdate().catch(e => { toast(errText(e)); $('#update-go').disabled = false; showUpdate(u); });
        } else {
            backend.openUpdatePage().catch(() => {});
        }
    };
    $('#set-copy').onchange = (e) => changeCopy(e.target);
    $('#copy-go').onclick = unlockCopy;
    $('#copy-pw').onkeydown = (e) => { if (e.key === 'Enter') unlockCopy(); };
    $('#set-quit').onclick = () => { backend.quit().catch(() => {}); };

    $('#account-btn').onclick = openAccount;
    $('#acct-close').onclick = () => { $('#account-modal').hidden = true; };
    $('#acct-signout').onclick = doSignOut;
    $('#acct-delete').onclick = openDeleteAccount;
    $('#del-next').onclick = delNext;
    $('#del-cancel').onclick = cancelDelete;
    ['#del-code', '#del-password', '#del-name'].forEach(s => {
        $(s).onkeydown = (e) => { if (e.key === 'Enter') delNext(); };
    });
    $('#acct-terms').onclick = openTerms;
    $('#terms-close').onclick = () => { $('#terms-modal').hidden = true; };

    $('#report-go').onclick = submitReport;
    $('#report-cancel').onclick = () => { $('#report-modal').hidden = true; S.reporting = null; };
    $('#report-close').onclick = () => { $('#report-modal').hidden = true; S.reporting = null; };
    $('#report-block').onclick = async () => {
        const peerId = S.reporting;
        $('#report-modal').hidden = true;
        S.reporting = null;
        if (!peerId) return;
        try {
            await backend.block(peerId);
            await refreshList();
            renderMain();
        } catch (e) {
            toast(errText(e));
        }
    };

    // Escape closes what the person opened, but never the sign-in gate:
    // that isn't a dialog, it's the door.
    document.addEventListener('keydown', (e) => {
        if (e.key === 'Escape') closeAllModals();
        if (!$('#tour').hidden) {
            if (e.key === 'ArrowRight') tourNext();
            if (e.key === 'ArrowLeft') tourBack();
        }
    });
}

/* ───────────────────────────── events ────────────────────────────── */

function wireEvents() {
    // "Looking at it" means the window has focus AND this conversation is
    // open. Anything else -- another conversation, another app, minimised --
    // leaves it unread, and away from the window it also notifies.
    const looking = lookingNow;
    // Go shows the desktop notifications, so it has to know when the window
    // is being looked at. Said on every change, and now and then in case a
    // change was missed (the window can be hidden without the page hearing).
    window.addEventListener('focus', () => reportFocus());
    window.addEventListener('blur', () => reportFocus());
    document.addEventListener('visibilitychange', () => reportFocus());
    setInterval(() => reportFocus(true), 4000);
    reportFocus(true);

    backend.on('message', async (ev) => {
        if (!ev || !ev.peerId) return;
        delete S.typing[ev.peerId];
        const msgs = ev.messages || [];
        if (ev.peerId === S.active) {
            const have = new Set(S.thread.map(m => m.id));
            msgs.forEach(m => { if (!have.has(m.id)) S.thread.push(m); });
            renderMain();
            // The backend counted these as unread on arrival; if the user
            // is actually looking at them, they're read.
            if (looking()) { try { await backend.markRead(ev.peerId); } catch (_) {} }
        }
        await refreshList();
    });

    // A notification was clicked: go to that conversation.
    backend.on('open', async (ev) => {
        if (!ev || !ev.peerId) return;
        await refreshList();
        const c = S.convs.find(x => x.peerId === ev.peerId);
        if (!c) return;
        const want = isBiz(c) ? 'business' : 'personal';
        if (S.mode !== want) await setMode(want);
        const wsId = isChannel(c) ? c.ws : isWorkspace(c) ? c.peerId : '';
        if (wsId && S.ws !== wsId && S.convs.some(x => x.peerId === wsId)) {
            S.ws = wsId;
            store(WS_KEY, wsId);
            S.people = [];
            if (S.mode === 'business') await loadPeople();
        }
        await open(ev.peerId);
    });

    backend.on('request', async (ev) => {
        await refreshList();
        if (!ev || !ev.peerId) return;
        if (looking()) toast(ev.name + ' wants to message you');
    });

    // Coming back to the window: whatever arrived in the open conversation
    // while you were away has now been seen.
    window.addEventListener('focus', async () => {
        if (!S.active) return;
        const c = S.convs.find(x => x.peerId === S.active);
        if (c && c.unread > 0) {
            try { await backend.markRead(S.active); } catch (_) {}
            await refreshList();
        }
    });

    backend.on('call', (ev) => {
        if (!ev || !ev.peerId) return;
        if (ev.group) groupCalls.handle(ev);
        else calls.handle(ev);
    });

    backend.on('typing', (ev) => {
        if (!ev || !ev.peerId) return;
        if (ev.who) S.typingWho[ev.peerId] = ev.who;
        showTyping(ev.peerId, true);
    });

    // A group changed: an invitation, people added or removed, someone left.
    const toldInvite = new Set();
    backend.on('group', async (ev) => {
        if (!ev || !ev.peerId) return;
        // New to us -- or closed until now (added back after leaving).
        const before = S.convs.find(x => x.peerId === ev.peerId);
        const had = !!before && !before.closed;
        await refreshList();
        const c = S.convs.find(x => x.peerId === ev.peerId);
        if (c && c.request && !toldInvite.has(c.peerId)) {
            toldInvite.add(c.peerId);
            if (looking()) toast(c.preview + ' to ' + (isWorkspace(c) ? 'the workspace ' : '') + c.name);
        } else if (c && isChannel(c) && c.accepted && !c.admin && !had && !toldInvite.has(c.peerId)) {
            // Someone added us to a channel: no yes needed, but worth saying.
            toldInvite.add(c.peerId);
            if (looking()) toast('You were added to #' + c.name);
        }
        if (S.active === ev.peerId) {
            S.thread = await backend.thread(ev.peerId) || [];
            renderMain();
        }
        if (membersOf && membersOf.peerId === ev.peerId) drawMembers();
    });

    backend.on('delivered', (ev) => {
        if (!ev || !ev.peerId || !Array.isArray(ev.ids)) return;
        if (ev.peerId.startsWith('g:')) {
            if (ev.peerId === S.active) backend.thread(ev.peerId).then(t => { S.thread = t || []; renderMain(); }).catch(() => {});
            return;
        }
        ev.ids.forEach(id => earlyDelivered.add(id)); // may beat our own copy of the message
        if (ev.peerId !== S.active) return;
        const ids = new Set(ev.ids);
        let hit = false;
        S.thread.forEach(m => { if (ids.has(m.id)) { m.delivered = true; hit = true; } });
        if (hit) renderMain();
    });

    backend.on('status', (ev) => {
        if (!S.me || !ev) return;
        // Back online: anything that failed to send goes again by itself.
        if (ev.online && !S.me.online) retryFailed();
        S.me.online = ev.online;
        S.me.peers = ev.peers;
        renderMe();
    });
}

/* ───────────────────────────── boot ──────────────────────────────── */

// Loads the signed-in app: who I am, the conversation list, and the
// one-time offer to choose a name.
async function enterApp() {
    $('#account-btn').hidden = S.gate === 'off'; // no accounts on a development network
    S.me = await backend.me();
    renderMe();
    S.mode = stored(MODE_KEY) === 'business' ? 'business' : 'personal';
    S.ws = stored(WS_KEY);
    S.active = null;
    S.thread = [];
    await refreshList();
    if (S.mode === 'business') preferLiveWs();
    const ws = S.mode === 'business' ? currentWs() : null;
    if (ws) await open(ws.peerId); // Business opens in #general
    else renderMain();

    let skipped = false;
    try { skipped = localStorage.getItem(SKIP_KEY) === '1'; } catch (_) {}
    const needName = !S.me.handle && !skipped;
    // A new person sees the tour first; the name dialog follows it.
    if (stored(TOUR_KEY) !== '1') startTour(() => { if (needName) openClaim(); });
    else if (needName) openClaim();
}

async function boot() {
    wireModals();
    wireAuth();
    wireEvents();
    backend.on('account', (a) => { if (a) applyGate(a); });
    backend.on('update', (u) => showUpdate(u));
    backend.updateStatus().then(showUpdate).catch(() => {});
    setInterval(reportBusy, 10000);

    // The engine opens a moment after the window does; give it a few tries
    // before calling that a failure.
    // Opening can take a while: loading what this computer stores for the
    // network, or a first start that Windows is still scanning. While the
    // answer is "still starting", keep waiting (and say so); only a real
    // failure, or minutes of nothing, ends in an error -- with a way to retry.
    let acct, lastErr;
    const started = Date.now();
    let note = null;
    while (!acct) {
        try { acct = await backend.account(); break; }
        catch (e) { lastErr = e; }
        const starting = /still starting/i.test(errText(lastErr));
        if (!starting || Date.now() - started > 180000) break;
        if (!note && Date.now() - started > 1500) {
            note = el('div', 'boot-note', 'Starting AltNet Relay…');
            document.body.appendChild(note);
        }
        await new Promise(r => setTimeout(r, 300));
    }
    if (note) note.remove();
    if (!acct) {
        document.body.replaceChildren(el('p', 'boot-note', 'Relay could not start: ' + errText(lastErr)));
        const again = el('button', 'btn', 'Try again');
        again.style.width = 'auto';
        again.style.margin = '0 24px';
        again.onclick = () => location.reload();
        document.body.appendChild(again);
        return;
    }
    S.gate = 'boot';
    try {
        await applyGate(acct);
    } catch (e) {
        document.body.textContent = 'Relay could not start: ' + errText(e);
    }
}

boot();
})();
