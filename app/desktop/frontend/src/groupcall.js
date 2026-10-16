/* AltNet Relay — group calls.
 *
 * Every participant connects directly to every other one: with four people,
 * each has three connections and sends their sound (and picture) three
 * times. No server mixes or forwards anything, which is the whole point --
 * and also why a call is capped at eight people.
 *
 * Joining works like this (the engine carries each step, sealed, to the
 * members of the group):
 *   ring  -> everyone   "I've started a call"            (the starter is now in it)
 *   join  -> everyone   "I'm joining"
 *   hello -> one        "I'm already in it", in reply to a join
 *   offer / answer      connecting one pair, exactly as a one-to-one call
 *   leave -> everyone
 * Of every pair, the one with the smaller username makes the offer: when a
 * join arrives, the smaller side offers straight away and the larger says
 * hello so the joiner offers instead. So each pair connects exactly once,
 * even when two people join at the same moment.
 *
 * Each connection carries what a one-to-one call does -- sound, a video slot
 * that starts empty, a small control channel -- so cameras go on and off
 * without any further setup. The address-gathering and the rest of the
 * pattern come from call.js (RelayCalls.gather).
 */
(function () {
'use strict';

const RING_TIMEOUT_MS = 45000; // a call nobody joins ends itself
const MAX_IN_CALL     = 8;     // everyone included
const DISCONNECT_GRACE = 6000;

function newCallId() {
    const b = new Uint8Array(12);
    crypto.getRandomValues(b);
    return Array.from(b, x => x.toString(16).padStart(2, '0')).join('');
}

function create(opts) {
    const RTC = opts.RTC || window.RTCPeerConnection;
    const gather = window.RelayCalls.gather;
    const me = () => (typeof opts.me === 'function' ? opts.me() : opts.me) || '';
    let c = idle();
    // Calls we know are running in our groups, whether or not we're in them:
    // callId -> { group, groupName, people: Set, video }
    const running = new Map();

    function idle() {
        return { phase: 'idle', group: null, groupName: '', callId: null, peers: new Map(),
                 stream: null, cam: null, camOn: false, muted: false, startedAt: 0, timers: {},
                 ringFrom: '', ringName: '', video: false, servers: null, note: '' };
    }

    function connected() { return [...c.peers.values()].filter(p => p.connected); }

    function snapshot() {
        const live = connected();
        return {
            phase: c.phase, group: c.group, groupName: c.groupName, callId: c.callId,
            count: live.length + 1, names: live.map(p => p.name), muted: c.muted,
            camera: c.camOn, anyCamera: c.camOn || live.some(p => p.remoteCam),
            seconds: c.startedAt ? Math.floor((Date.now() - c.startedAt) / 1000) : 0,
            ringName: c.ringName, video: c.video, note: c.note,
        };
    }
    const changed = () => { try { opts.onChange(snapshot()); } catch (_) {} };
    const notice = (t) => { if (opts.onNotice) { try { opts.onNotice(t); } catch (_) {} } };
    const send = (to, op, sdp, video) => opts.send(c.group, to || '', op, c.callId, sdp || '', !!video).catch(() => {});

    function tiles() {
        if (!opts.showTiles) return;
        const list = [{ id: 'me', name: 'You', stream: c.camOn ? c.cam : null, me: true }];
        connected().forEach((p, id) => list.push({ id: p.id, name: p.name, stream: p.remoteCam ? p.remoteVideo : null }));
        try { opts.showTiles(c.phase === 'active' ? list : []); } catch (_) {}
    }

    function tick() {
        clearTimeout(c.timers.tick);
        if (c.phase !== 'active') return;
        changed();
        c.timers.tick = setTimeout(tick, 1000);
    }

    /* ---- one connection per other participant ---- */

    async function makePeer(id, name, offerer) {
        const pc = new RTC({ iceServers: c.servers || [], bundlePolicy: 'max-bundle' });
        const peer = { id, name, pc, ctl: null, videoTx: null, remoteCam: false, remoteVideo: null,
                       offering: offerer, connected: false, timer: null };
        c.peers.set(id, peer);
        c.stream.getAudioTracks().forEach(t => pc.addTrack(t, c.stream));
        if (offerer) {
            peer.videoTx = pc.addTransceiver('video', { direction: 'sendrecv' });
            if (c.camOn && c.cam) await peer.videoTx.sender.replaceTrack(c.cam.getVideoTracks()[0]);
        }
        const ctl = pc.createDataChannel('ctl', { negotiated: true, id: 0 });
        peer.ctl = ctl;
        ctl.onopen = () => { try { ctl.send(JSON.stringify({ cam: c.camOn })); } catch (_) {} };
        ctl.onmessage = (ev) => {
            let m; try { m = JSON.parse(ev.data); } catch (_) { return; }
            if (typeof m.cam === 'boolean' && m.cam !== peer.remoteCam) { peer.remoteCam = m.cam; tiles(); changed(); }
        };
        pc.ontrack = (ev) => {
            if (ev.track.kind === 'video') { peer.remoteVideo = new MediaStream([ev.track]); tiles(); return; }
            const stream = ev.streams && ev.streams[0] ? ev.streams[0] : new MediaStream([ev.track]);
            try { opts.playRemote(id, stream); } catch (_) {}
        };
        pc.onconnectionstatechange = () => {
            if (c.peers.get(id) !== peer) return;
            const s = pc.connectionState;
            if (s === 'connected') {
                clearTimeout(peer.timer);
                if (!peer.connected) {
                    peer.connected = true;
                    if (!c.startedAt) { c.startedAt = Date.now(); clearTimeout(c.timers.ring); c.note = ''; tick(); }
                    tiles();
                    changed();
                }
            } else if (s === 'disconnected') {
                clearTimeout(peer.timer);
                peer.timer = setTimeout(() => { if (pc.connectionState !== 'connected') dropPeer(id); }, DISCONNECT_GRACE);
            } else if (s === 'failed') {
                dropPeer(id);
            }
        };
        return peer;
    }

    function dropPeer(id) {
        const p = c.peers.get(id);
        if (!p) return;
        clearTimeout(p.timer);
        try { p.ctl && p.ctl.close(); } catch (_) {}
        try { p.pc.close(); } catch (_) {}
        try { opts.stopRemote(id); } catch (_) {}
        c.peers.delete(id);
        tiles();
        // Everyone else gone from a call that had started: it's over.
        if (c.phase === 'active' && c.startedAt && c.peers.size === 0) {
            leave('Everyone left the call');
            return;
        }
        changed();
    }

    async function offerTo(id, name) {
        if (c.peers.has(id) || c.peers.size >= MAX_IN_CALL - 1) return;
        const callId = c.callId;
        try {
            const peer = await makePeer(id, name, true);
            await peer.pc.setLocalDescription(await peer.pc.createOffer());
            await gather(peer.pc);
            if (c.callId !== callId || c.peers.get(id) !== peer) return;
            send(id, 'offer', peer.pc.localDescription.sdp);
        } catch (_) { dropPeer(id); }
    }

    async function acceptOffer(id, name, sdp) {
        if (c.peers.size >= MAX_IN_CALL - 1 && !c.peers.has(id)) return;
        const callId = c.callId;
        try {
            const peer = await makePeer(id, name, false);
            await peer.pc.setRemoteDescription({ type: 'offer', sdp });
            peer.videoTx = peer.pc.getTransceivers().find(t => t.receiver && t.receiver.track && t.receiver.track.kind === 'video') || null;
            if (peer.videoTx) {
                peer.videoTx.direction = 'sendrecv';
                if (c.camOn && c.cam) await peer.videoTx.sender.replaceTrack(c.cam.getVideoTracks()[0]);
            }
            await peer.pc.setLocalDescription(await peer.pc.createAnswer());
            await gather(peer.pc);
            if (c.callId !== callId || c.peers.get(id) !== peer) return;
            send(id, 'answer', peer.pc.localDescription.sdp);
        } catch (_) { dropPeer(id); }
    }

    /* ---- the camera, for every connection at once ---- */

    async function toggleCamera() {
        if (c.phase !== 'active') return;
        if (c.camOn) {
            c.camOn = false;
            for (const p of c.peers.values()) {
                try { if (p.videoTx) await p.videoTx.sender.replaceTrack(null); } catch (_) {}
                try { p.ctl && p.ctl.readyState === 'open' && p.ctl.send(JSON.stringify({ cam: false })); } catch (_) {}
            }
            try { c.cam && c.cam.getTracks().forEach(t => t.stop()); } catch (_) {}
            c.cam = null;
        } else {
            let cam;
            try { cam = await opts.getCamera(); } catch (e) { notice(window.RelayCalls.describeCamera(e)); return; }
            if (c.phase !== 'active') { cam.getTracks().forEach(t => t.stop()); return; }
            c.cam = cam;
            c.camOn = true;
            const track = cam.getVideoTracks()[0];
            for (const p of c.peers.values()) {
                try { if (p.videoTx) await p.videoTx.sender.replaceTrack(track); } catch (_) {}
                try { p.ctl && p.ctl.readyState === 'open' && p.ctl.send(JSON.stringify({ cam: true })); } catch (_) {}
            }
        }
        tiles();
        changed();
    }

    function toggleMute() {
        c.muted = !c.muted;
        if (c.stream) c.stream.getAudioTracks().forEach(t => { t.enabled = !c.muted; });
        changed();
    }

    /* ---- starting, joining, leaving ---- */

    async function enter(group, groupName, callId, video) {
        c = idle();
        c.phase = 'active';
        c.group = group; c.groupName = groupName; c.callId = callId; c.video = !!video;
        c.note = 'Connecting…';
        changed();
        try {
            c.servers = await opts.servers().catch(() => []);
            c.stream = await opts.getMedia();
        } catch (e) {
            const why = window.RelayCalls.describe(e);
            c = idle();
            changed();
            throw new Error(why);
        }
        if (video) await toggleCamera();
    }

    async function start(group, groupName, video) {
        if (c.phase !== 'idle' || (opts.isBusy && opts.isBusy())) throw new Error('You\'re already on a call.');
        const callId = newCallId();
        await enter(group, groupName, callId, video);
        c.note = 'Ringing…';
        changed();
        send('', 'ring', '', video);
        c.timers.ring = setTimeout(() => { if (!c.startedAt) leave('No one joined'); }, RING_TIMEOUT_MS);
    }

    async function join(callId, video) {
        const info = running.get(callId) || (c.phase === 'ringing' && c.callId === callId ? { group: c.group, groupName: c.groupName } : null);
        if (!info) throw new Error('That call has ended.');
        if (c.phase === 'active') throw new Error('You\'re already on a call.');
        if (opts.isBusy && opts.isBusy()) throw new Error('You\'re already on a call.');
        clearTimeout(c.timers.ring);
        await enter(info.group, info.groupName, callId, video);
        send('', 'join');
    }

    function decline() {
        if (c.phase !== 'ringing') return;
        Object.values(c.timers).forEach(clearTimeout);
        c = idle();
        changed();
    }

    function leave(reason) {
        if (c.phase === 'ringing') { decline(); return; }
        if (c.phase !== 'active') return;
        send('', 'leave');
        const group = c.group, callId = c.callId;
        Object.values(c.timers).forEach(clearTimeout);
        for (const id of [...c.peers.keys()]) {
            const p = c.peers.get(id);
            try { p.ctl && p.ctl.close(); } catch (_) {}
            try { p.pc.close(); } catch (_) {}
            try { opts.stopRemote(id); } catch (_) {}
        }
        try { c.stream && c.stream.getTracks().forEach(t => t.stop()); } catch (_) {}
        try { c.cam && c.cam.getTracks().forEach(t => t.stop()); } catch (_) {}
        const r = running.get(callId);
        if (r) { r.people.delete(me()); if (!r.people.size) running.delete(callId); }
        c = idle();
        tiles();
        changed();
        if (typeof reason === 'string' && reason && opts.onEnded) { try { opts.onEnded(reason); } catch (_) {} }
        if (opts.onRunning) { try { opts.onRunning(group); } catch (_) {} }
    }

    /* ---- setup messages from the engine ---- */

    function track(sig, add) {
        let r = running.get(sig.callId);
        if (!r && add) { r = { group: sig.group, groupName: sig.groupName, people: new Set(), video: !!sig.video }; running.set(sig.callId, r); }
        if (!r) return;
        if (add) r.people.add(sig.peerId); else r.people.delete(sig.peerId);
        if (!r.people.size) running.delete(sig.callId);
        if (opts.onRunning) { try { opts.onRunning(sig.group); } catch (_) {} }
    }

    async function handle(sig) {
        const inThis = c.phase === 'active' && c.callId === sig.callId;
        switch (sig.op) {
            case 'ring':
                track(sig, true);
                if (c.phase === 'idle' && !(opts.isBusy && opts.isBusy())) {
                    c = idle();
                    c.phase = 'ringing'; c.group = sig.group; c.groupName = sig.groupName || '';
                    c.callId = sig.callId; c.ringFrom = sig.peerId; c.ringName = sig.name || '';
                    c.video = !!sig.video;
                    c.note = (sig.name || 'Someone') + ' started a call';
                    c.timers.ring = setTimeout(() => { if (c.phase === 'ringing') decline(); }, RING_TIMEOUT_MS);
                    changed();
                    if (opts.onRing) { try { opts.onRing(snapshot(), sig.peerId); } catch (_) {} }
                }
                break;
            case 'join':
                track(sig, true);
                if (inThis && !c.peers.has(sig.peerId)) {
                    if (me() < sig.peerId) offerTo(sig.peerId, sig.name);
                    else send(sig.peerId, 'hello');
                }
                break;
            case 'hello':
                track(sig, true);
                if (inThis && !c.peers.has(sig.peerId)) offerTo(sig.peerId, sig.name);
                break;
            case 'offer': {
                if (!inThis) return;
                const existing = c.peers.get(sig.peerId);
                if (existing) {
                    // Both offered at once. The smaller username's offer wins.
                    if (existing.offering && me() < sig.peerId) return;
                    dropPeerQuietly(sig.peerId);
                }
                acceptOffer(sig.peerId, sig.name, sig.sdp);
                break;
            }
            case 'answer': {
                const p = c.peers.get(sig.peerId);
                if (!inThis || !p || !p.offering) return;
                try { await p.pc.setRemoteDescription({ type: 'answer', sdp: sig.sdp }); } catch (_) { dropPeer(sig.peerId); }
                break;
            }
            case 'leave':
                track(sig, false);
                if (inThis) dropPeer(sig.peerId);
                if (c.phase === 'ringing' && c.callId === sig.callId && sig.peerId === c.ringFrom && !running.has(sig.callId)) decline();
                break;
        }
    }

    function dropPeerQuietly(id) {
        const p = c.peers.get(id);
        if (!p) return;
        try { p.pc.close(); } catch (_) {}
        c.peers.delete(id);
    }

    // A call running in this group that we're not in, for a "Join" banner.
    function runningIn(group) {
        for (const [callId, r] of running) {
            if (r.group === group && r.people.size > 0 && !(c.phase === 'active' && c.callId === callId)) {
                return { callId, count: r.people.size, video: r.video };
            }
        }
        return null;
    }

    return {
        start, join, decline, leave: () => leave(''), toggleMute, toggleCamera, handle, runningIn,
        state: snapshot,
        _peers: () => c.peers, // for tests
    };
}

window.RelayGroupCalls = { create };
})();
