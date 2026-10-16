// The launch demo: signs in, takes the tour, and clicks through Personal and
// Business, recording the page. Frames land in ./frames; make-video.mjs turns
// them into an mp4. It runs the real interface on its built-in sample data.
//
//   python scripts/serve-ui.py 5173                      (in another window)
//   cd scripts/demo-video/host && go build -ldflags "-H windowsgui" -o host.exe .
//   cd .. && node demo.mjs && node make-video.mjs ../../demo/AltNetRelay-demo.mp4
import fs from 'node:fs';
import path from 'node:path';
import { CDP, Recorder, PAGE_HELPERS, launchHost, sleep } from './cdp.mjs';

const here = path.dirname(new URL(import.meta.url).pathname).replace(/^\/([A-Z]:)/, '$1');
const data = path.join(here, 'wv-data');
fs.rmSync(data, { recursive: true, force: true });   // a first start, so the tour shows
const host = launchHost(path.join(here, 'host', 'host.exe'), 'about:blank', 9333, data);

const ROW = '#list .row, #list .wsbar';
const HEAD = '#main button';
const COMPOSER = '#main textarea, #main input';
let rec, t0;
const mark = (s) => console.log(((Date.now() / 1000 - t0)).toFixed(1).padStart(5), s, 'frames', rec.frames.length);

try {
    const cdp = await CDP.connect(9333);
    await cdp.send('Page.enable');
    await cdp.send('Runtime.enable');
    await cdp.send('Emulation.setDeviceMetricsOverride', { width: 1440, height: 810, deviceScaleFactor: 4 / 3, mobile: false });
    await cdp.send('Page.addScriptToEvaluateOnNewDocument', { source: PAGE_HELPERS });
    rec = new Recorder(cdp, path.join(here, 'frames'));
    await cdp.send('Page.navigate', { url: 'http://127.0.0.1:5173/?gate=signed_out' });
    await sleep(1200);
    await rec.start(); t0 = Date.now() / 1000;
    await sleep(900);

    // ── Sign in ──
    mark('sign in');
    await rec.click('#gate form[data-mode=signin] input[name=email]');
    await rec.type('rob@acme.studio', 24);
    await rec.click('#gate form[data-mode=signin] input[name=password]');
    await rec.type('••••••••••', 24);
    await rec.click('#gate form[data-mode=signin] button[type=submit]');
    await sleep(1300);

    // ── Tour ──
    mark('tour');
    for (let i = 0; i < 8; i++) {
        const open = await cdp.eval(`!document.getElementById('tour').hidden`);
        if (!open) break;
        await sleep(i === 0 ? 1100 : 850);
        await rec.click('#tour-next', null, { pause: 60 });
        await sleep(350);
    }
    await sleep(400);

    // ── Personal: a message request ──
    mark('request');
    await rec.click(ROW, '/@kate/');
    await sleep(1300);
    await rec.click(HEAD + ', #main .btn', '/^Accept/');
    await sleep(900);

    // ── A chat ──
    mark('chat');
    await rec.click(ROW, '/^B\\s*\\n\\s*bob/');
    await sleep(1200);
    await rec.click(COMPOSER);
    await rec.type('Runs great on the laptop. Quick call?');
    await sleep(250);
    await rec.enter();
    await sleep(1100);

    // ── Video call ──
    mark('video call');
    await rec.click(HEAD, '/^Video$/');
    await sleep(3600);
    await rec.hover('#callbar .cbtn', '/^Mute/');
    await sleep(500);
    await rec.click('#callbar .cbtn.end');
    await sleep(1100);

    // ── A group ──
    mark('group');
    await rec.click('#group-btn');
    await sleep(500);
    await rec.click('#group-name');
    await rec.type('Weekend hike');
    await rec.click('#group-people');
    await rec.type('alice, bob');
    await sleep(300);
    await rec.click('#group-go');
    await sleep(1200);
    await rec.click(COMPOSER);
    await rec.type('Saturday 9:00 at the station?');
    await rec.enter();
    await sleep(1100);

    // ── Block (and report) ──
    mark('block');
    await rec.click(ROW, '/alice/');
    await sleep(900);
    await rec.click(HEAD, 'Block');
    await sleep(900);
    await rec.click('#block-modal label.check', 'Also report them to AltNet');
    await sleep(500);
    if (!(await cdp.eval(`document.getElementById('block-report').checked`))) throw new Error('report box not ticked');
    await sleep(1500);
    await rec.click('#block-cancel');
    await sleep(700);

    // ── Business ──
    mark('business');
    await rec.click('#modeswitch button', '/^Business/');
    await sleep(1600);
    await rec.click(ROW, '/design/');
    await sleep(1500);

    mark('channel');
    await rec.click('#channel-btn');
    await sleep(500);
    await rec.click('#channel-name');
    await rec.type('support');
    await rec.click('#channel-all');
    await sleep(400);
    await rec.click('#channel-go');
    await sleep(1200);

    mark('reports');
    await rec.click(ROW, '/general/');
    await sleep(900);
    await rec.click(HEAD, '/report/');
    await sleep(2300);
    await rec.click('#members-modal button', 'Dismiss');
    await sleep(800);
    await rec.click('#members-close');
    await sleep(600);

    mark('meeting');
    await rec.click(HEAD, 'Meet');
    await sleep(3600);
    await rec.click('#callbar .cbtn', '/^Mute/');
    await sleep(1000);
    await rec.click('#callbar .cbtn.end');
    await sleep(1000);

    mark('invitation');
    await rec.click(ROW, '/Prague Dev Guild/');
    await sleep(1300);
    await rec.click(HEAD + ', #main .btn', '/^(Join|Accept)/');
    await sleep(1200);

    // ── Reporting a coworker goes to the workspace owner ──
    mark('work report');
    await rec.click(HEAD, 'People');
    await sleep(900);
    await rec.click('#members-modal button.link', 'Report');
    await sleep(700);
    await rec.click('#report-reason');
    await rec.type('Posting ads in #general all day');
    await sleep(300);
    await rec.click('#report-go');
    await sleep(1300);
    await rec.click('#report-close');
    await sleep(300);
    if (await cdp.eval(`!document.getElementById('members-modal').hidden`)) { await rec.click('#members-close'); }
    await sleep(500);

    // ── Always on: startup, background, notifications ──
    mark('account');
    await rec.click('#account-btn');
    await sleep(1100);
    await rec.click('#bg-settings label.check', '/^Start AltNet Relay/');
    await sleep(500);
    await rec.hover('#bg-settings label.check', '/^Keep running/');
    await sleep(400);
    await rec.click('#set-test');
    await sleep(1200);
    await rec.click('#acct-close');
    await sleep(500);
    await rec.click('#modeswitch button', '/^Personal/');
    await sleep(600);
    mark('done ' + (Date.now() / 1000 - t0).toFixed(1));
    await sleep(1500);
    await rec.stop();
} catch (e) {
    console.error('FAILED:', e.message);
    if (rec) await rec.stop().catch(() => {});
    process.exitCode = 1;
} finally {
    host.kill();
}
