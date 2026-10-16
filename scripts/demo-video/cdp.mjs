// Minimal Chrome DevTools Protocol client plus the recording helpers the demo
// script uses: a drawn cursor that glides to things, real clicks and typing,
// and a screencast saved frame by frame with timestamps.
import { spawn } from 'node:child_process';
import fs from 'node:fs';
import path from 'node:path';

export const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

export class CDP {
    constructor(ws) {
        this.ws = ws; this.id = 0; this.pending = new Map(); this.handlers = new Map();
        ws.onmessage = (ev) => {
            const m = JSON.parse(ev.data);
            if (m.id && this.pending.has(m.id)) {
                const { res, rej } = this.pending.get(m.id); this.pending.delete(m.id);
                m.error ? rej(new Error(m.error.message)) : res(m.result);
            } else if (m.method) {
                (this.handlers.get(m.method) || []).forEach((fn) => fn(m.params));
            }
        };
    }
    static async connect(port) {
        for (let i = 0; i < 100; i++) {
            try {
                const list = await (await fetch(`http://127.0.0.1:${port}/json`)).json();
                const page = list.find((t) => t.type === 'page');
                if (page) {
                    const ws = new WebSocket(page.webSocketDebuggerUrl);
                    await new Promise((res, rej) => { ws.onopen = res; ws.onerror = rej; });
                    return new CDP(ws);
                }
            } catch (_) {}
            await sleep(200);
        }
        throw new Error('no DevTools page');
    }
    send(method, params = {}) {
        const id = ++this.id;
        this.ws.send(JSON.stringify({ id, method, params }));
        return new Promise((res, rej) => this.pending.set(id, { res, rej }));
    }
    on(method, fn) { (this.handlers.get(method) || this.handlers.set(method, []).get(method)).push(fn); }
    async eval(expr) {
        const r = await this.send('Runtime.evaluate', { expression: expr, awaitPromise: true, returnByValue: true });
        if (r.exceptionDetails) throw new Error(r.exceptionDetails.exception?.description || r.exceptionDetails.text);
        return r.result.value;
    }
}

// Injected into the page: a cursor drawn on top (headless capture has none),
// a click ripple, and a finder for elements by selector and visible text.
export const PAGE_HELPERS = `
(() => {
  if (window.__rec) return;
  const mk = () => {
    const c = document.createElement('div');
    c.id = '__cursor';
    c.innerHTML = '<svg width="22" height="30" viewBox="0 0 22 30"><path d="M2 2 L2 24 L8 18.5 L12 27.5 L15.5 26 L11.6 17 L19.5 17 Z" fill="#fff" stroke="#000" stroke-width="1.6" stroke-linejoin="round"/></svg>';
    Object.assign(c.style, { position: 'fixed', left: '0', top: '0', zIndex: 2147483647, pointerEvents: 'none',
      transform: 'translate(720px, 560px)', transition: 'transform 0ms', filter: 'drop-shadow(0 2px 3px rgba(0,0,0,.5))' });
    document.documentElement.appendChild(c);
    const st = document.createElement('style');
    st.textContent = '@keyframes __rip{from{transform:translate(-50%,-50%) scale(.3);opacity:.9}to{transform:translate(-50%,-50%) scale(1.6);opacity:0}}' +
      '.__rip{position:fixed;width:34px;height:34px;border-radius:50%;border:2px solid #00c8ff;pointer-events:none;z-index:2147483646;animation:__rip .45s ease-out forwards}';
    document.documentElement.appendChild(st);
  };
  window.__rec = {
    move(x, y, ms) {
      if (!document.getElementById('__cursor')) mk();
      const c = document.getElementById('__cursor');
      c.style.transition = 'transform ' + ms + 'ms cubic-bezier(.45,.05,.25,1)';
      c.style.transform = 'translate(' + x + 'px,' + y + 'px)';
    },
    ripple(x, y) {
      const r = document.createElement('div'); r.className = '__rip';
      r.style.left = x + 'px'; r.style.top = y + 'px';
      document.documentElement.appendChild(r); setTimeout(() => r.remove(), 600);
    },
    find(sel, text) {
      const want = text == null ? null : (text.startsWith('/') ? new RegExp(text.slice(1, -1)) : null);
      const els = [...document.querySelectorAll(sel)].filter((e) => {
        const b = e.getBoundingClientRect();
        if (!b.width || !b.height || e.closest('[hidden]')) return false;
        if (text == null) return true;
        const t = (e.innerText || e.value || '').trim();
        return want ? want.test(t) : t === text;
      });
      if (!els.length) return null;
      els[0].scrollIntoView({ block: 'nearest' });
      const b = els[0].getBoundingClientRect();
      return { x: b.left + b.width / 2, y: b.top + b.height / 2, w: b.width, h: b.height };
    },
  };
  if (document.readyState === 'loading') document.addEventListener('DOMContentLoaded', mk); else mk();
})();
`;

export class Recorder {
    constructor(cdp, dir) {
        this.cdp = cdp; this.dir = dir; this.frames = []; this.n = 0;
        this.cx = 720; this.cy = 560;
        fs.rmSync(dir, { recursive: true, force: true }); fs.mkdirSync(dir, { recursive: true });
        cdp.on('Page.screencastFrame', (p) => {
            const name = `f${String(++this.n).padStart(5, '0')}.jpg`;
            fs.writeFileSync(path.join(dir, name), Buffer.from(p.data, 'base64'));
            this.frames.push({ name, t: p.metadata.timestamp });
            cdp.send('Page.screencastFrameAck', { sessionId: p.sessionId }).catch(() => {});
        });
    }
    async start() {
        await this.cdp.send('Page.startScreencast', { format: 'jpeg', quality: 93, maxWidth: 1920, maxHeight: 1080, everyNthFrame: 1 });
        this.t0 = Date.now() / 1000;
    }
    async stop() {
        this.t1 = Date.now() / 1000;
        await this.cdp.send('Page.stopScreencast');
        fs.writeFileSync(path.join(this.dir, 'frames.json'), JSON.stringify({ t0: this.t0, t1: this.t1, frames: this.frames }));
    }

    // Glide the drawn cursor to (x, y), taking longer for longer trips.
    async glide(x, y) {
        const d = Math.hypot(x - this.cx, y - this.cy);
        const ms = Math.round(Math.min(620, 200 + d * 0.42));
        await this.cdp.eval(`window.__rec.move(${x}, ${y}, ${ms})`);
        await this.cdp.send('Input.dispatchMouseEvent', { type: 'mouseMoved', x, y });
        await sleep(ms + 30);
        this.cx = x; this.cy = y;
    }
    async where(sel, text, timeout = 4000) {
        const end = Date.now() + timeout;
        for (;;) {
            const r = await this.cdp.eval(`window.__rec.find(${JSON.stringify(sel)}, ${JSON.stringify(text ?? null)})`);
            if (r) return r;
            if (Date.now() > end) throw new Error(`not found: ${sel} ${text ?? ''}`);
            await sleep(100);
        }
    }
    async click(sel, text, { pause = 80 } = {}) {
        const r = await this.where(sel, text);
        // Aim a little left of centre on wide things, like a person would.
        const x = Math.round(r.w > 200 ? r.x - r.w * 0.2 : r.x), y = Math.round(r.y);
        await this.glide(x, y);
        await sleep(pause);
        await this.cdp.eval(`window.__rec.ripple(${x}, ${y})`);
        await this.cdp.send('Input.dispatchMouseEvent', { type: 'mousePressed', x, y, button: 'left', clickCount: 1 });
        await sleep(70);
        await this.cdp.send('Input.dispatchMouseEvent', { type: 'mouseReleased', x, y, button: 'left', clickCount: 1 });
    }
    async hover(sel, text) {
        const r = await this.where(sel, text);
        await this.glide(Math.round(r.x), Math.round(r.y));
    }
    async type(text, cps = 21) {
        for (const ch of text) {
            await this.cdp.send('Input.insertText', { text: ch });
            await sleep(1000 / cps * (0.6 + Math.random() * 0.8) + (ch === ' ' ? 30 : 0));
        }
    }
    async key(key, code, vk, text) {
        await this.cdp.send('Input.dispatchKeyEvent', { type: 'keyDown', key, code, windowsVirtualKeyCode: vk, text });
        await this.cdp.send('Input.dispatchKeyEvent', { type: 'keyUp', key, code, windowsVirtualKeyCode: vk });
    }
    enter() { return this.key('Enter', 'Enter', 13, '\r'); }
}

export function launchHost(exe, url, port, dataDir) {
    // Off to the side of the screen: the recording comes from the page itself,
    // so the window needn't be seen (occlusion throttling is switched off).
    return spawn(exe, [url, String(port), dataDir, '-2600', '40', '1500', '900'], { stdio: 'ignore' });
}
