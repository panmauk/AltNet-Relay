// Turns the recorded frames (each with the time it was painted) into a
// constant 30 fps H.264 mp4: every frame is held until the next one came.
//   node make-video.mjs <out.mp4>
import fs from 'node:fs';
import path from 'node:path';
import { spawnSync } from 'node:child_process';

const here = path.dirname(new URL(import.meta.url).pathname).replace(/^\/([A-Z]:)/, '$1');
const dir = path.join(here, 'frames');
const out = path.resolve(process.argv[2] || path.join(here, 'demo.mp4'));
const { t0, t1, frames } = JSON.parse(fs.readFileSync(path.join(dir, 'frames.json'), 'utf8'));

const list = [];
const fr = frames.filter((f) => f.t <= t1).sort((a, b) => a.t - b.t);
// The first frame stands in from the start of the recording.
for (let i = 0; i < fr.length; i++) {
    const start = i === 0 ? Math.min(t0, fr[0].t) : fr[i].t;
    const end = i + 1 < fr.length ? fr[i + 1].t : t1;
    const d = Math.max(0, end - start);
    list.push(`file '${fr[i].name}'`, `duration ${d.toFixed(4)}`);
}
list.push(`file '${fr[fr.length - 1].name}'`);   // concat needs the last one repeated
fs.writeFileSync(path.join(dir, 'list.txt'), list.join('\n') + '\n');

const r = spawnSync('ffmpeg', ['-y', '-v', 'error', '-f', 'concat', '-safe', '0', '-i', 'list.txt',
    '-vf', 'fps=30,format=yuv420p', '-c:v', 'libx264', '-preset', 'slow', '-crf', '16',
    '-movflags', '+faststart', out], { cwd: dir, stdio: 'inherit' });
if (r.status !== 0) process.exit(r.status || 1);
console.log('wrote', out, (fs.statSync(out).size / 1e6).toFixed(1), 'MB,', (t1 - t0).toFixed(1), 's');
