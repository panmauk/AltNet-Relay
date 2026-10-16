/* AltNet Relay — voice and video calls.
 *
 * The audio and video never go through Relay or the AltNet network. The two
 * computers connect to each other with WebRTC, which the window already
 * contains; this file is the small state machine around it. The only thing
 * that travels through Relay is the *setup* -- an offer, an answer, a
 * hang-up -- carried as sealed messages by the Go engine (engine/call.go).
 *
 * Setup is a single round trip with no trickling: each side waits until it
 * has gathered its network addresses and puts them all in its one message.
 * That costs a second or two before the phone rings and buys a lot of
 * simplicity, which matters when every message is a store on a
 * peer-to-peer network rather than a socket.
 *
 * Every call, voice or video, is set up the same way: an audio track, a
 * video slot that starts empty, and a tiny control channel. Turning the
 * camera on or off later just fills or empties the video slot and says so
 * over the control channel -- no second round of setup messages, which here
 * would each be a store on the network.
 *
 * The manager is a factory with everything it touches passed in, so the
 * same code runs in the app and in a page with two managers wired back to
 * back for testing.
 *
 *   opts.send(peerId, op, callId, sdp, video) -> Promise  deliver a setup message
 *   opts.servers()                     -> Promise   ICE servers (STUN/TURN)
 *   opts.getMedia()                    -> Promise   a MediaStream with the microphone
 *   opts.getCamera()                   -> Promise   a MediaStream with the camera
 *   opts.onChange(snapshot)                         the call's state changed
 *   opts.onEnded(text)                              the call ended; say why
 *   opts.onRing(snapshot)                           an incoming call started ringing
 *   opts.onNotice(text)                             something to tell the person (camera failed…)
 *   opts.RTC                                        RTCPeerConnection (default: global)
 *   opts.playRemote(stream) / opts.stopRemote()     audio out (default: hidden <audio>)
 *   opts.showVideo(local, remote)                   MediaStreams for the two video views (or null)
 */
(function () {
'use strict';

const RING_TIMEOUT_MS    = 45000; // give up on an unanswered call
const CONNECT_TIMEOUT_MS = 20000; // answered, but media never flowed
const GATHER_MAX_MS      = 2000;  // longest we wait for our addresses (a working STUN answers in well under this)
const GATHER_SETTLE_MS   = 250;   // after a relay address, how long to wait for stragglers
const GATHER_SRFLX_MS    = 700;   // after a public address, how long to wait for a relay one
const DISCONNECT_GRACE   = 6000;  // a blip that heals itself isn't a hang-up

function newCallId() {
    const b = new Uint8Array(12);
    crypto.getRandomValues(b);
    return Array.from(b, x => x.toString(16).padStart(2, '0')).join('');
}

// Waits until this side has the network addresses worth sending, and no
// longer. Every address arrives as a "candidate": local ones at once, the
// public one (srflx) once the STUN server answers, a relay one once the
// TURN server does. Waiting for "complete" can take seconds when a
// server is slow or unreachable, and every one of those seconds is
// silence before the phone rings. So: stop as soon as gathering is
// complete, or shortly after a relay address arrives (nothing better is
// coming), or a little after a public one, and never past the cap.
function gathered(pc) {
    return new Promise((resolve) => {
        if (pc.iceGatheringState === 'complete') return resolve();
        let finished = false, settle = null;
        const done = () => {
            if (finished) return;
            finished = true;
            clearTimeout(cap); clearTimeout(settle);
            pc.removeEventListener('icegatheringstatechange', check);
            pc.removeEventListener('icecandidate', seen);
            resolve();
        };
        const check = () => { if (pc.iceGatheringState === 'complete') done(); };
        const soon = (ms) => { clearTimeout(settle); settle = setTimeout(done, ms); };
        const seen = (ev) => {
            const c = ev.candidate && ev.candidate.candidate;
            if (!c) return;
            if (/ typ relay/.test(c)) soon(GATHER_SETTLE_MS);
            else if (/ typ srflx/.test(c) && !settle) soon(GATHER_SRFLX_MS);
        };
        const cap = setTimeout(done, GATHER_MAX_MS);
        pc.addEventListener('icegatheringstatechange', check);
        pc.addEventListener('icecandidate', seen);
    });
}

function describe(e) {
    const n = e && e.name, m = (e && e.message) || String(e || '');
    if (n === 'NotAllowedError' || n === 'SecurityError') return 'Relay can\'t use your microphone. Check Windows Settings > Privacy & security > Microphone.';
    if (n === 'NotFoundError' || n === 'OverconstrainedError') return 'No microphone found.';
    if (n === 'NotReadableError') return 'Your microphone is being used by another app.';
    return m || 'The call didn\'t work.';
}

function describeCamera(e) {
    const n = e && e.name;
    if (n === 'NotAllowedError' || n === 'SecurityError') return 'Relay can\'t use your camera. Check Windows Settings > Privacy & security > Camera.';
    if (n === 'NotFoundError' || n === 'OverconstrainedError') return 'No camera found.';
    if (n === 'NotReadableError') return 'Your camera is being used by another app.';
    return 'The camera couldn\'t be started.';
}

function create(opts) {
    const RTC = opts.RTC || window.RTCPeerConnection;
    let c = idle();

    function idle() {
        return { phase: 'idle', peerId: null, name: '', callId: null, pc: null, stream: null,
                 muted: false, startedAt: 0, offer: null, timers: {}, note: '',
                 wantVideo: false, videoTx: null, ctl: null, cam: null, camOn: false,
                 remoteCam: false, remoteVideo: null, lastCallId: null };
    }

    function snapshot() {
        return {
            phase: c.phase, peerId: c.peerId, name: c.name, muted: c.muted, note: c.note,
            video: c.wantVideo, camera: c.camOn, remoteCamera: c.remoteCam,
            seconds: c.startedAt ? Math.floor((Date.now() - c.startedAt) / 1000) : 0,
        };
    }
    const changed = () => { try { opts.onChange(snapshot()); } catch (_) {} };
    const notice = (t) => { if (opts.onNotice) { try { opts.onNotice(t); } catch (_) {} } };
    const setTimer = (name, fn, ms) => { clearTimeout(c.timers[name]); c.timers[name] = setTimeout(fn, ms); };

    function views() {
        if (!opts.showVideo) return;
        try { opts.showVideo(c.camOn ? c.cam : null, c.remoteCam ? c.remoteVideo : null); } catch (_) {}
    }

    function teardown() {
        const last = c.callId || c.lastCallId;
        Object.values(c.timers).forEach(clearTimeout);
        try { if (c.ctl) c.ctl.close(); } catch (_) {}
        try { if (c.pc) c.pc.close(); } catch (_) {}
        try { if (c.stream) c.stream.getTracks().forEach(t => t.stop()); } catch (_) {}
        try { if (c.cam) c.cam.getTracks().forEach(t => t.stop()); } catch (_) {}
        try { opts.stopRemote ? opts.stopRemote() : defaultStop(); } catch (_) {}
        c = idle();
        c.lastCallId = last; // so a late duplicate of the offer doesn't ring again
        views();
        changed();
    }

    function finish(text) {
        teardown();
        if (text && opts.onEnded) { try { opts.onEnded(text); } catch (_) {} }
    }

    // Best effort: a hang-up that can't be delivered still ends the call here.
    const tell = (op, sdp) => opts.send(c.peerId, op, c.callId, sdp || '', false).catch(() => {});

    /* ---- the peer connection, shared by both sides ---- */

    async function makePeer(isCaller) {
        const servers = await opts.servers().catch(() => []);
        // max-bundle: one set of network addresses for audio, video and the
        // control channel together, which keeps the setup message small.
        const pc = new RTC({ iceServers: servers, bundlePolicy: 'max-bundle' });
        c.pc = pc;
        c.stream = await opts.getMedia();
        c.stream.getAudioTracks().forEach(t => { t.enabled = !c.muted; pc.addTrack(t, c.stream); });
        if (isCaller) {
            c.videoTx = pc.addTransceiver('video', { direction: 'sendrecv' });
        }
        // Negotiated with a fixed id on both sides, so neither has to wait to
        // be told the channel exists.
        const ctl = pc.createDataChannel('ctl', { negotiated: true, id: 0 });
        c.ctl = ctl;
        ctl.onopen = () => sendCtl();
        ctl.onmessage = (ev) => {
            if (c.ctl !== ctl) return;
            let m; try { m = JSON.parse(ev.data); } catch (_) { return; }
            if (typeof m.cam === 'boolean' && m.cam !== c.remoteCam) {
                c.remoteCam = m.cam;
                views();
                changed();
            }
        };

        pc.ontrack = (ev) => {
            const stream = ev.streams && ev.streams[0] ? ev.streams[0] : new MediaStream([ev.track]);
            if (ev.track.kind === 'video') {
                c.remoteVideo = new MediaStream([ev.track]);
                views();
                return;
            }
            try { opts.playRemote ? opts.playRemote(stream) : defaultPlay(stream); } catch (_) {}
        };
        pc.onconnectionstatechange = () => {
            if (c.pc !== pc) return;
            const s = pc.connectionState;
            if (s === 'connected' && c.phase !== 'active') {
                clearTimeout(c.timers.connect);
                c.phase = 'active';
                c.startedAt = Date.now();
                c.note = '';
                tick();
                changed();
            } else if (s === 'disconnected') {
                setTimer('blip', () => { if (c.pc === pc && pc.connectionState !== 'connected') hangup('Connection lost'); }, DISCONNECT_GRACE);
            } else if (s === 'connected') {
                clearTimeout(c.timers.blip);
            } else if (s === 'failed') {
                hangup('Couldn\'t connect the call');
            }
        };
        return pc;
    }

    function sendCtl() {
        try { if (c.ctl && c.ctl.readyState === 'open') c.ctl.send(JSON.stringify({ cam: c.camOn })); } catch (_) {}
    }

    function tick() {
        clearTimeout(c.timers.tick);
        if (c.phase !== 'active') return;
        changed();
        c.timers.tick = setTimeout(tick, 1000);
    }

    /* ---- the camera ---- */

    async function cameraOn() {
        if (c.camOn || !c.videoTx) return;
        const id = c.callId;
        let cam;
        try {
            cam = await opts.getCamera();
        } catch (e) {
            notice(describeCamera(e));
            return;
        }
        if (c.callId !== id) { cam.getTracks().forEach(t => t.stop()); return; } // call ended meanwhile
        const track = cam.getVideoTracks()[0];
        if (!track) { cam.getTracks().forEach(t => t.stop()); notice('No camera found.'); return; }
        try { await c.videoTx.sender.replaceTrack(track); }
        catch (e) { cam.getTracks().forEach(t => t.stop()); notice('The camera couldn\'t be started.'); return; }
        c.cam = cam;
        c.camOn = true;
        sendCtl();
        views();
        changed();
    }

    async function cameraOff() {
        if (!c.camOn) return;
        c.camOn = false;
        sendCtl();
        try { if (c.videoTx) await c.videoTx.sender.replaceTrack(null); } catch (_) {}
        try { if (c.cam) c.cam.getTracks().forEach(t => t.stop()); } catch (_) {}
        c.cam = null;
        views();
        changed();
    }

    function toggleCamera() {
        return c.camOn ? cameraOff() : cameraOn();
    }

    /* ---- caller ---- */

    async function call(peerId, name, video) {
        if (c.phase !== 'idle' || (opts.isBusy && opts.isBusy())) throw new Error('You\'re already on a call.');
        c = idle();
        c.phase = 'calling'; c.peerId = peerId; c.name = name; c.callId = newCallId();
        c.wantVideo = !!video; c.note = 'Starting…';
        changed();
        const id = c.callId;
        try {
            const pc = await makePeer(true);
            if (video) await cameraOn(); // a camera that fails still leaves a voice call
            await pc.setLocalDescription(await pc.createOffer());
            await gathered(pc);
            if (c.callId !== id) return; // hung up while we were getting ready
            await opts.send(peerId, 'offer', id, pc.localDescription.sdp, !!video);
            c.note = 'Calling…';
            changed();
            setTimer('ring', () => hangup('No answer'), RING_TIMEOUT_MS);
        } catch (e) {
            if (c.callId === id) finish(describe(e));
        }
    }

    /* ---- callee ---- */

    function incoming(sig) {
        // The same offer can arrive twice (pushed directly, then found on the
        // network). A repeat of this call, or of one just finished, is ignored.
        if (sig.callId === c.callId || sig.callId === c.lastCallId) return;
        if (c.phase !== 'idle' || (opts.isBusy && opts.isBusy())) {
            // Already talking: tell them, and leave the current call alone.
            opts.send(sig.peerId, 'busy', sig.callId, '', false).catch(() => {});
            return;
        }
        c = idle();
        c.phase = 'ringing'; c.peerId = sig.peerId; c.name = sig.name || sig.peerId.slice(0, 8);
        c.callId = sig.callId; c.offer = sig.sdp; c.wantVideo = !!sig.video;
        c.note = sig.video ? 'Incoming video call' : 'Incoming call';
        changed();
        if (opts.onRing) { try { opts.onRing(snapshot()); } catch (_) {} }
        setTimer('ring', () => finish(''), RING_TIMEOUT_MS); // missed; the engine notes it
    }

    // withVideo: answer with the camera on. Only ever the person's own choice.
    async function answer(withVideo) {
        if (c.phase !== 'ringing') return;
        const id = c.callId, offer = c.offer;
        clearTimeout(c.timers.ring);
        c.phase = 'connecting'; c.note = 'Connecting…';
        changed();
        try {
            const pc = await makePeer(false);
            await pc.setRemoteDescription({ type: 'offer', sdp: offer });
            // Claim the offered video slot for sending too, so the camera can be
            // turned on at any point in the call without another round of setup.
            c.videoTx = pc.getTransceivers().find(t => t.receiver && t.receiver.track && t.receiver.track.kind === 'video') || null;
            if (c.videoTx) c.videoTx.direction = 'sendrecv';
            if (withVideo) await cameraOn();
            await pc.setLocalDescription(await pc.createAnswer());
            await gathered(pc);
            if (c.callId !== id) return;
            await opts.send(c.peerId, 'answer', id, pc.localDescription.sdp, false);
            setTimer('connect', () => hangup('Couldn\'t connect the call'), CONNECT_TIMEOUT_MS);
        } catch (e) {
            if (c.callId === id) { tell('decline'); finish(describe(e)); } // tell them it isn't happening
        }
    }

    function decline() {
        if (c.phase !== 'ringing') return;
        tell('decline');
        finish('');
    }

    /* ---- either side ---- */

    function hangup(reason) {
        if (c.phase === 'idle') return;
        if (c.phase === 'ringing') { decline(); return; }
        tell('end');
        finish(typeof reason === 'string' ? reason : '');
    }

    function toggleMute() {
        c.muted = !c.muted;
        if (c.stream) c.stream.getAudioTracks().forEach(t => { t.enabled = !c.muted; });
        changed();
        return c.muted;
    }

    // A setup message from the engine. sig = {peerId, op, callId, sdp, name, video}.
    async function handle(sig) {
        if (sig.op === 'offer') return incoming(sig);
        if (sig.callId !== c.callId || sig.peerId !== c.peerId) return; // some other call
        switch (sig.op) {
            case 'answer':
                if (c.phase !== 'calling') return;
                clearTimeout(c.timers.ring);
                c.phase = 'connecting'; c.note = 'Connecting…';
                changed();
                try {
                    await c.pc.setRemoteDescription({ type: 'answer', sdp: sig.sdp });
                    setTimer('connect', () => hangup('Couldn\'t connect the call'), CONNECT_TIMEOUT_MS);
                } catch (e) { hangup(describe(e)); }
                break;
            case 'decline': finish(c.name + ' didn\'t answer'); break;
            case 'busy':    finish(c.name + ' is on another call'); break;
            case 'end':     finish(c.phase === 'active' ? 'Call ended' : ''); break;
        }
    }

    /* ---- default audio output: one hidden <audio> element ---- */
    let audioEl = null;
    function defaultPlay(stream) {
        if (!audioEl) {
            audioEl = document.createElement('audio');
            audioEl.autoplay = true;
            audioEl.style.display = 'none';
            document.body.appendChild(audioEl);
        }
        audioEl.srcObject = stream;
        const p = audioEl.play(); if (p && p.catch) p.catch(() => {});
    }
    function defaultStop() {
        if (audioEl) { audioEl.srcObject = null; audioEl.remove(); audioEl = null; }
    }

    return {
        call, answer, decline, hangup: () => hangup(''), toggleMute, toggleCamera, handle,
        state: snapshot,
        // for tests
        _pc: () => c.pc,
    };
}

window.RelayCalls = { create, gather: gathered, describe, describeCamera };
})();
