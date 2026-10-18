"""Makes the Mac build kit: a folder (and a .zip of it) that builds AltNet
Relay on a Mac and makes a .dmg, by double-clicking one file.

    python scripts/make-macos-kit.py

The Mac app can only be compiled on a Mac (its window code is Apple's), so
this gathers everything the Mac needs -- the source, the AltNet core library
and every Go dependency (vendored) -- leaving only Apple's Command Line Tools
and the Go toolchain for the Mac to fetch. Output: dist/AltNetRelay-macOS/
and dist/AltNetRelay-macOS.zip.
"""
import json
import os
import re
import shutil
import stat
import subprocess
import sys
import zipfile

HERE = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
CORE = os.environ.get('ALTNET_CORE', '').strip() or os.path.join(os.path.dirname(os.path.dirname(os.path.abspath(__file__))), '..', 'AltNet')
DIST = os.path.join(HERE, 'dist')
KIT = os.path.join(DIST, 'AltNetRelay-macOS')
SRC = os.path.join(KIT, 'src')

VER = json.load(open(os.path.join(HERE, 'app', 'desktop', 'wails.json')))['info']['productVersion']

SKIP_DIRS = {'.claude', 'dist', 'demo', 'screenshots', 'bin', 'wailsjs', 'vendor', '.git', 'node_modules', '__pycache__'}
SKIP_FILES = re.compile(r'.*\.(exe|syso|log)$', re.I)


def copytree(src, dst):
    for root, dirs, files in os.walk(src):
        dirs[:] = [d for d in dirs if d not in SKIP_DIRS]
        rel = os.path.relpath(root, src)
        out = os.path.join(dst, rel) if rel != '.' else dst
        os.makedirs(out, exist_ok=True)
        for f in files:
            if SKIP_FILES.match(f):
                continue
            shutil.copy2(os.path.join(root, f), os.path.join(out, f))


def tracked(root):
    """The files git tracks in root, as forward-slash paths."""
    out = subprocess.run(['git', 'ls-files', '-z'], cwd=root, capture_output=True)
    if out.returncode != 0:
        sys.exit('make-macos-kit: the project must be a git checkout (only tracked files go into the kit)')
    return [p for p in out.stdout.decode('utf-8').split('\0') if p]


def run(cmd, cwd, env=None):
    e = dict(os.environ)
    e.update(env or {})
    r = subprocess.run(cmd, cwd=cwd, env=e, capture_output=True, text=True)
    if r.returncode != 0:
        print(r.stdout, r.stderr)
        sys.exit('failed: ' + ' '.join(cmd))
    return r.stdout


def main():
    if not os.path.isdir(os.path.join(CORE, 'core')):
        sys.exit('AltNet core not found at ' + CORE + ': clone github.com/panmauk/AltNet next to this folder, or set ALTNET_CORE.')
    print(f'==> AltNet Relay {VER}: Mac build kit')
    if os.path.exists(KIT):
        shutil.rmtree(KIT)
    os.makedirs(SRC)

    print('==> copying the source and the AltNet core library')
    # Only what git tracks: the same files as the public repository. Notes,
    # drafts and anything else kept beside the code on this computer stay here.
    for rel in tracked(HERE):
        if rel.split('/')[0] in SKIP_DIRS or SKIP_FILES.match(rel):
            continue
        dst = os.path.join(SRC, *rel.split('/'))
        os.makedirs(os.path.dirname(dst), exist_ok=True)
        shutil.copy2(os.path.join(HERE, *rel.split('/')), dst)
    # The scripts folder is for this PC; the Mac gets its own two files.
    shutil.rmtree(os.path.join(SRC, 'scripts'), ignore_errors=True)
    core_dst = os.path.join(SRC, 'third_party', 'altnet')
    os.makedirs(core_dst)
    shutil.copy2(os.path.join(CORE, 'go.mod'), core_dst)
    copytree(os.path.join(CORE, 'core'), os.path.join(core_dst, 'core'))

    # Point go.mod at the vendored core copy in the kit.
    for gomod, rel in ((os.path.join(SRC, 'go.mod'), './third_party/altnet'),
                       (os.path.join(SRC, 'app', 'desktop', 'go.mod'), '../../third_party/altnet')):
        text = open(gomod, encoding='utf-8').read()
        text, n = re.subn(r'(?m)^replace altnet => .*$', 'replace altnet => ' + rel, text)
        assert n == 1, gomod
        open(gomod, 'w', encoding='utf-8', newline='\n').write(text)

    print('==> storing every Go dependency in the kit (go mod vendor)')
    desktop = os.path.join(SRC, 'app', 'desktop')
    run(['go', 'mod', 'vendor'], cwd=desktop, env={'GOFLAGS': '-mod=mod', 'GOTOOLCHAIN': 'local'})
    darwin = os.path.join(desktop, 'vendor', 'github.com', 'wailsapp', 'wails', 'v2', 'internal', 'frontend', 'desktop', 'darwin')
    objc = [f for f in os.listdir(darwin) if f.endswith('.m')]
    assert objc, 'the Mac window code (.m files) must be in the kit'
    print(f'    {len(objc)} Objective-C files of the Mac window code included')

    print('==> checking it all resolves for a Mac, with nothing to download')
    env = {'GOOS': 'darwin', 'GOARCH': 'amd64', 'GOFLAGS': '-mod=vendor', 'GOPROXY': 'off', 'GOTOOLCHAIN': 'local'}
    deps = run(['go', 'list', '-deps', '-tags', 'desktop,production', '.'], cwd=desktop, env=dict(env, CGO_ENABLED='1'))
    print(f'    {len(deps.splitlines())} packages resolve')
    # Without a Mac's compiler, Wails' window code can't be type-checked; our own
    # code (startup_darwin.go and the rest) can, without the desktop tag.
    run(['go', 'vet', '.'], cwd=desktop, env=dict(env, CGO_ENABLED='0'))
    print('    go vet of our own code for macOS: clean')

    print('==> the double-click file and the instructions')
    for name in ('Build AltNet Relay.command', 'README.txt'):
        text = open(os.path.join(HERE, 'scripts', 'macos', name), encoding='utf-8').read().replace('\r\n', '\n')
        open(os.path.join(KIT, name), 'w', encoding='utf-8', newline='\n').write(text)
    open(os.path.join(KIT, 'VERSION'), 'w', newline='\n').write(VER + '\n')

    print('==> zipping (keeping the double-click file runnable)')
    zpath = os.path.join(DIST, 'AltNetRelay-macOS.zip')
    if os.path.exists(zpath):
        os.remove(zpath)
    with zipfile.ZipFile(zpath, 'w', zipfile.ZIP_DEFLATED, compresslevel=9) as z:
        for root, dirs, files in os.walk(KIT):
            dirs.sort()
            for d in dirs:
                full = os.path.join(root, d)
                info = zipfile.ZipInfo(os.path.relpath(full, DIST).replace(os.sep, '/') + '/')
                info.create_system = 3  # Unix, so the Mac keeps the permissions
                info.external_attr = (stat.S_IFDIR | 0o755) << 16
                z.writestr(info, '')
            for f in sorted(files):
                full = os.path.join(root, f)
                info = zipfile.ZipInfo.from_file(full, os.path.relpath(full, DIST).replace(os.sep, '/'))
                info.create_system = 3
                mode = 0o755 if f.endswith('.command') else 0o644
                info.external_attr = (stat.S_IFREG | mode) << 16
                info.compress_type = zipfile.ZIP_DEFLATED
                with open(full, 'rb') as fh:
                    z.writestr(info, fh.read())
    size = os.path.getsize(zpath) / 1e6
    print(f'==> done: {KIT}  and  {zpath} ({size:.1f} MB)')


if __name__ == '__main__':
    main()
