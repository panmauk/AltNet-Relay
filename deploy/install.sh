#!/usr/bin/env bash
# Installs the Relay server beside the AltNet seed and accounts backend.
# Run ON THE SERVER, from a folder containing: relay-server, operator.key,
# altnet-relay.service, Caddyfile.new.
#
# What it changes (and nothing else):
#   - creates /opt/altnet/relay/            (new folder, holds only Relay files)
#   - installs the systemd service          (new: altnet-relay.service)
#   - swaps /etc/caddy/Caddyfile            (old one is kept as Caddyfile.bak-<time>)
# It does NOT touch the seed, the accounts backend, their data or their ports.
#
# Rollback:
#   sudo systemctl disable --now altnet-relay
#   sudo cp /etc/caddy/Caddyfile.bak-<time> /etc/caddy/Caddyfile && sudo systemctl reload caddy
set -euo pipefail

here="$(cd "$(dirname "$0")" && pwd)"
stamp="$(date +%Y%m%d-%H%M%S)"

for f in relay-server operator.key altnet-relay.service Caddyfile.new; do
    [ -f "$here/$f" ] || { echo "missing $f"; exit 1; }
done

echo "== backup of the accounts data (a copy; the original is not touched) =="
sudo cp -a /opt/altnet/backend-data "/opt/altnet/backend-data.bak-$stamp"
sudo cp -a /etc/caddy/Caddyfile "/etc/caddy/Caddyfile.bak-$stamp"

echo "== Relay files =="
mkdir -p /opt/altnet/relay/data
install -m 0755 "$here/relay-server" /opt/altnet/relay/relay-server
install -m 0600 "$here/operator.key" /opt/altnet/relay/operator.key

echo "== service =="
sudo install -m 0644 "$here/altnet-relay.service" /etc/systemd/system/altnet-relay.service
sudo systemctl daemon-reload
sudo systemctl enable --now altnet-relay
sleep 2
systemctl is-active altnet-relay

echo "== check the new server answers locally, before Caddy is changed =="
curl -fsS http://127.0.0.1:8788/healthz && echo

echo "== Caddy: validate first, and only reload if the new file is good =="
sudo cp "$here/Caddyfile.new" /etc/caddy/Caddyfile
if sudo caddy validate --config /etc/caddy/Caddyfile --adapter caddyfile >/dev/null 2>&1; then
    sudo systemctl reload caddy
    echo "caddy reloaded"
else
    echo "NEW CADDYFILE INVALID -- restoring the old one"
    sudo cp "/etc/caddy/Caddyfile.bak-$stamp" /etc/caddy/Caddyfile
    exit 1
fi

echo "== end to end, through the public address =="
sleep 1
echo -n "accounts backend still answers: "; curl -fsS https://api.panmox.org/healthz; echo
echo -n "relay server answers:           "; curl -fsS https://api.panmox.org/relay/healthz; echo
echo "done. backups: /opt/altnet/backend-data.bak-$stamp  /etc/caddy/Caddyfile.bak-$stamp"
