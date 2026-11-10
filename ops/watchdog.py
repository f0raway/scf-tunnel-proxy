#!/usr/bin/env python3
"""Check only local endpoints; never create billable SCF tunnels."""
import json
from pathlib import Path
import socket
import subprocess
import urllib.request

CONFIG = Path("/etc/scf-tunnel-proxy/agent.json")
STATE = Path("/var/lib/scf-tunnel-proxy/watchdog-failures")


def address(value):
    host, port = value.rsplit(":", 1)
    host = host.strip("[]")
    if host in ("", "0.0.0.0"):
        host = "127.0.0.1"
    if host == "::":
        host = "::1"
    return host, int(port)


def main():
    try:
        config = json.loads(CONFIG.read_text())
        host, port = address(config["dashboard_addr"])
        url_host = "[" + host + "]" if ":" in host else host
        with urllib.request.urlopen(f"http://{url_host}:{port}/api/stats", timeout=5) as response:
            stats = json.load(response)
        if not stats.get("nodes") or stats.get("mode") != "tcp-tunnel-no-mitm":
            raise ValueError("invalid local status")
        for key in ("http_addr", "socks_addr"):
            with socket.create_connection(address(config[key]), timeout=5):
                pass
        STATE.write_text("0")
    except Exception as error:
        count = int(STATE.read_text() or "0") + 1 if STATE.exists() else 1
        STATE.write_text(str(count))
        print("local_health_failure", count, str(error))
        if count >= 2:
            subprocess.run(["systemctl", "restart", "scf-tunnel-proxy"], check=True)
            STATE.write_text("0")


if __name__ == "__main__":
    main()
