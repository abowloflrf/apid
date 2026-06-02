#!/usr/bin/env bash
# Install a prebuilt apid binary as a systemd service. Run as root (or sudo).
#
# Usage: sudo ./install.sh [path-to-apid-binary]
# The binary defaults to ./apid next to this script (drop it there when
# distributing), or pass an explicit path.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
BIN="${1:-$SCRIPT_DIR/apid}"
PREFIX=/usr/local/bin
CONF_DIR=/etc/apid
UNIT=/etc/systemd/system/apid.service

if [[ $EUID -ne 0 ]]; then
    echo "must run as root (try: sudo $0)" >&2
    exit 1
fi
if [[ ! -f "$BIN" ]]; then
    echo "apid binary not found at '$BIN' (pass its path as the first arg)" >&2
    exit 1
fi

# config.example.toml ships next to the script when distributed, else in the repo root.
EXAMPLE_CONF="$SCRIPT_DIR/config.example.toml"
[[ -f "$EXAMPLE_CONF" ]] || EXAMPLE_CONF="$SCRIPT_DIR/../config.example.toml"

echo "==> installing binary to $PREFIX/apid"
install -m 0755 "$BIN" "$PREFIX/apid"

echo "==> installing config to $CONF_DIR"
install -d "$CONF_DIR"
# Don't clobber an existing live config/env.
if [[ ! -f "$CONF_DIR/config.toml" ]]; then
    install -m 0644 "$EXAMPLE_CONF" "$CONF_DIR/config.toml"
    echo "    wrote $CONF_DIR/config.toml (from example — edit before starting)"
fi
if [[ ! -f "$CONF_DIR/apid.env" ]]; then
    install -m 0644 "$SCRIPT_DIR/apid.env.example" "$CONF_DIR/apid.env"
    echo "    wrote $CONF_DIR/apid.env (from example)"
fi

echo "==> installing systemd unit"
install -m 0644 "$SCRIPT_DIR/apid.service" "$UNIT"
systemctl daemon-reload
systemctl enable apid.service

echo
echo "done. edit $CONF_DIR/config.toml, then:"
echo "    sudo systemctl start apid"
echo "    systemctl status apid"
echo "    journalctl -u apid -f"
