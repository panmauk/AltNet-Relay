#!/usr/bin/env bash
# Adds the call relay (coturn) to the VPS so voice calls work between people
# whose home routers won't let them reach each other directly.
# Run ON THE SERVER, from the folder with: relay-server (the new build, which
# understands the -turn flags), altnet-relay.service (the version that passes
# them) and turnserver.conf.template.
#
# What it changes (and nothing else):
#   - installs the coturn package (Ubuntu's own, from apt)
#   - writes /etc/turnserver.conf (old one, if any, kept as .bak-<time>)
#   - opens two firewall ports on the machine: UDP 3478 and UDP 49160-49200
#     (inserted before the final REJECT rule, and added to /etc/iptables/rules.v4)
#   - gives relay-server the shared secret so it can hand out call credentials
# It does NOT touch the seed, the accounts backend, or Caddy.
#
# The shared secret is created here and never printed.
#
# Rollback:
#   sudo systemctl disable --now coturn
#   sudo iptables -D INPUT -p udp --dport 3478 -j ACCEPT
#   sudo iptables -D INPUT -p udp --dport 49160:49200 -j ACCEPT
#   sudo cp /etc/iptables/rules.v4.bak-<time> /etc/iptables/rules.v4
#   (and put the previous files back: /etc/systemd/system/altnet-relay.service.bak-<time>
#    and /opt/altnet/relay/relay-server.bak-<time>, then: sudo systemctl daemon-reload && sudo systemctl restart altnet-relay)
set -euo pipefail

here="$(cd "$(dirname "$0")" && pwd)"
stamp="$(date +%Y%m%d-%H%M%S)"
PUBLIC_IP="130.162.215.155"
PRIVATE_IP="$(ip -4 -o addr show ens3 | awk '{print $4}' | cut -d/ -f1)"
[ -n "$PRIVATE_IP" ] || { echo "can't find this machine's private address"; exit 1; }

for f in relay-server altnet-relay.service turnserver.conf.template; do
    [ -f "$here/$f" ] || { echo "missing $f"; exit 1; }
done

echo "== shared secret (created once, never printed) =="
if [ ! -s /opt/altnet/relay/turn-secret ]; then
    openssl rand -hex 32 > /opt/altnet/relay/turn-secret
fi
chmod 600 /opt/altnet/relay/turn-secret

echo "== coturn =="
sudo DEBIAN_FRONTEND=noninteractive apt-get install -y coturn >/dev/null
[ -f /etc/turnserver.conf ] && sudo cp -a /etc/turnserver.conf "/etc/turnserver.conf.bak-$stamp"
secret="$(cat /opt/altnet/relay/turn-secret)"
sed -e "s|__SECRET__|$secret|" -e "s|__PUBLIC_IP__|$PUBLIC_IP|g" -e "s|__PRIVATE_IP__|$PRIVATE_IP|g" \
    "$here/turnserver.conf.template" | sudo tee /etc/turnserver.conf >/dev/null
sudo chown root:turnserver /etc/turnserver.conf
sudo chmod 640 /etc/turnserver.conf
unset secret

echo "== firewall: UDP 3478 and 49160-49200 (before the final REJECT) =="
if ! sudo iptables -C INPUT -p udp --dport 3478 -j ACCEPT 2>/dev/null; then
    # position 10 is right after the existing service ports, ahead of the ufw chains and the REJECT
    sudo iptables -I INPUT 10 -p udp --dport 3478 -j ACCEPT
    sudo iptables -I INPUT 11 -p udp --dport 49160:49200 -j ACCEPT
fi
sudo iptables -S INPUT | grep -E "3478|49160" || { echo "firewall rules missing"; exit 1; }

echo "== firewall: keep the two rules across a reboot =="
# The saved copy is /etc/iptables/rules.v4. Add the same two lines right after
# the existing service ports; the old file is kept, and the edit is checked
# with iptables-restore --test (which parses without applying anything).
if ! sudo grep -q -- "--dport 3478 -j ACCEPT" /etc/iptables/rules.v4; then
    sudo cp -a /etc/iptables/rules.v4 "/etc/iptables/rules.v4.bak-$stamp"
    sudo sed -i '0,/--dport 9000 -j ACCEPT/{//a\
-A INPUT -p udp -m udp --dport 3478 -j ACCEPT\
-A INPUT -p udp -m udp --dport 49160:49200 -j ACCEPT
}' /etc/iptables/rules.v4
    if ! sudo iptables-restore --test < <(sudo cat /etc/iptables/rules.v4); then
        echo "edited rules file doesn't parse -- restoring the old one"
        sudo cp -a "/etc/iptables/rules.v4.bak-$stamp" /etc/iptables/rules.v4
        exit 1
    fi
fi
sudo grep -n -- "3478\|49160" /etc/iptables/rules.v4

echo "== start coturn =="
sudo systemctl enable coturn
sudo systemctl restart coturn # pick up the new config
sleep 2
systemctl is-active coturn
ss -lun | grep 3478 || { echo "coturn isn't listening on 3478"; exit 1; }

echo "== relay-server: new build + hand out call credentials =="
sudo cp -a /opt/altnet/relay/relay-server "/opt/altnet/relay/relay-server.bak-$stamp"
install -m 0755 "$here/relay-server" /opt/altnet/relay/relay-server.new
mv -f /opt/altnet/relay/relay-server.new /opt/altnet/relay/relay-server
sudo cp -a /etc/systemd/system/altnet-relay.service "/etc/systemd/system/altnet-relay.service.bak-$stamp"
sudo install -m 0644 "$here/altnet-relay.service" /etc/systemd/system/altnet-relay.service
sudo systemctl daemon-reload
sudo systemctl restart altnet-relay
sleep 2
systemctl is-active altnet-relay
curl -fsS http://127.0.0.1:8788/healthz && echo

echo "== memory =="
free -m | head -2
ps -o rss,comm -C turnserver,relay-server
echo "done. backups: /etc/turnserver.conf.bak-$stamp (if it existed), /etc/systemd/system/altnet-relay.service.bak-$stamp"
