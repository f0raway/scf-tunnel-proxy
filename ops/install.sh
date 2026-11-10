#!/usr/bin/env bash
# Installs/updates a generic Linux service. Does not change firewall rules.
set -euo pipefail
if [[ ${EUID} -ne 0 || $# -ne 2 ]]; then
  echo 'Usage: sudo bash ops/install.sh BINARY AGENT_CONFIG' >&2
  exit 2
fi
BINARY=$(realpath "$1")
AGENT_CONFIG=$(realpath "$2")
OPS_DIR=$(cd "$(dirname "$0")" && pwd)
python3 - "$AGENT_CONFIG" "$BINARY" <<'PY'
import json,sys
from pathlib import Path
from urllib.parse import urlsplit
data=Path(sys.argv[2]).read_bytes()[:20]
if len(data)<20 or data[:4]!=b'\x7fELF' or data[4]!=2 or int.from_bytes(data[18:20],'little')!=62: raise SystemExit('Expected Linux amd64 ELF binary')
c=json.load(open(sys.argv[1]))
for k in ('http_addr','socks_addr','dashboard_addr','user','password','token','ledger_path','nodes','monthly_limit_yuan'):
    if not c.get(k): raise SystemExit('Missing config field: '+k)
if 'CHANGE_ME' in c['password'] or 'CHANGE_ME' in c['token']:
    raise SystemExit('Replace placeholder credentials first')
if c['ledger_path'] != '/var/lib/scf-tunnel-proxy/usage.json':
    raise SystemExit('Generic service expects /var/lib/scf-tunnel-proxy/usage.json; see migration documentation')
for n in c['nodes']:
    p=urlsplit(n['url'])
    if p.scheme!='wss' or not p.hostname or 'YOUR_' in p.hostname:
        raise SystemExit('Invalid WSS node endpoint')
if not 1 <= c.get('max_connections',32) <= 64: raise SystemExit('Invalid max_connections')
if c['monthly_limit_yuan']<=0: raise SystemExit('Budget must be positive')
host=c['dashboard_addr'].rsplit(':',1)[0]
if host not in ('127.0.0.1','localhost','[::1]'): raise SystemExit('Dashboard must be loopback-only')
PY
if ! getent passwd scftunnel >/dev/null; then
  useradd --system --home /var/lib/scf-tunnel-proxy --shell /usr/sbin/nologin scftunnel
fi
install -d -m 755 /opt/scf-tunnel-proxy
install -d -m 750 -o root -g scftunnel /etc/scf-tunnel-proxy /var/log/scf-tunnel-proxy
install -d -m 700 -o scftunnel -g scftunnel /var/lib/scf-tunnel-proxy
# Save only existing service files; backup archives remain root-only.
BACKUP_FILE="/var/backups/scf-tunnel-proxy-$(date -u +%Y%m%dT%H%M%SZ)-$$.tar.gz"
if [[ -f /etc/scf-tunnel-proxy/agent.json ]]; then
  (umask 077; tar -czf "$BACKUP_FILE" /etc/scf-tunnel-proxy /var/lib/scf-tunnel-proxy /opt/scf-tunnel-proxy)
fi
install -m 755 "$BINARY" /opt/scf-tunnel-proxy/.binary-new
install -o root -g scftunnel -m 640 "$AGENT_CONFIG" /etc/scf-tunnel-proxy/.config-new
if systemctl list-unit-files scf-tunnel-proxy-watchdog.timer --no-legend | grep -q scf-tunnel-proxy; then
  systemctl stop scf-tunnel-proxy-watchdog.timer
fi
if systemctl is-active --quiet scf-tunnel-proxy; then
  systemctl stop scf-tunnel-proxy
fi
mv /opt/scf-tunnel-proxy/.binary-new /opt/scf-tunnel-proxy/scf-tunnel
install -m 755 "$OPS_DIR/watchdog.py" /opt/scf-tunnel-proxy/watchdog.py
mv /etc/scf-tunnel-proxy/.config-new /etc/scf-tunnel-proxy/agent.json
install -m 644 "$OPS_DIR/scf-tunnel-proxy.service" /etc/systemd/system/scf-tunnel-proxy.service
cat > /etc/systemd/system/scf-tunnel-proxy-watchdog.service <<'SERVICE'
[Unit]
Description=Check local SCF tunnel endpoints without invoking SCF
After=scf-tunnel-proxy.service
[Service]
Type=oneshot
ExecStart=/usr/bin/python3 /opt/scf-tunnel-proxy/watchdog.py
SERVICE
cat > /etc/systemd/system/scf-tunnel-proxy-watchdog.timer <<'TIMER'
[Unit]
Description=Check local SCF proxy every two minutes
[Timer]
OnBootSec=3min
OnUnitActiveSec=2min
AccuracySec=15s
[Install]
WantedBy=timers.target
TIMER
cat > /etc/logrotate.d/scf-tunnel-proxy <<'LOGROTATE'
/var/log/scf-tunnel-proxy/service.log {
    daily
    maxsize 5M
    rotate 5
    compress
    delaycompress
    missingok
    notifempty
    copytruncate
    su root root
}
LOGROTATE
systemctl daemon-reload
systemctl enable --now scf-tunnel-proxy
systemctl enable --now scf-tunnel-proxy-watchdog.timer
systemctl is-active scf-tunnel-proxy
printf 'Installed. Run an authenticated TCP/TLS test; an active process alone does not confirm cloud forwarding.\n'
