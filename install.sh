#!/bin/sh
# Tunnelo agent bare-metal installer: downloads the latest release binary
# and sets it up as a systemd service.
#
# Usage (one-liner from the README):
#   curl -fsSL https://raw.githubusercontent.com/abiteman/tunnelo-agent/main/install.sh | sudo TUNNELO_TOKEN=<token> sh
#
# Or download first and run:
#   sudo ./install.sh <token>
#
# The token is only needed on first install; upgrades can omit it.
set -eu

REPO="abiteman/tunnelo-agent"
BIN_DIR="/usr/local/bin"
ENV_FILE="/etc/tunnelo-agent/agent.env"
UNIT_FILE="/etc/systemd/system/tunnelo-agent.service"

if [ "$(id -u)" -ne 0 ]; then
    echo "error: this installer must run as root (it installs a systemd service)" >&2
    exit 1
fi

if ! command -v systemctl >/dev/null 2>&1; then
    echo "error: systemd not found. For non-systemd hosts, download the binary" >&2
    echo "from https://github.com/${REPO}/releases and run it under your init system." >&2
    exit 1
fi

case "$(uname -m)" in
    x86_64|amd64)   ARCH=amd64 ;;
    aarch64|arm64)  ARCH=arm64 ;;
    *) echo "error: unsupported architecture $(uname -m) (amd64 and arm64 builds are published)" >&2; exit 1 ;;
esac

TOKEN="${TUNNELO_TOKEN:-${1:-}}"
if [ -z "$TOKEN" ] && [ ! -f "$ENV_FILE" ]; then
    echo "error: no token. Pass it via TUNNELO_TOKEN=<token> or as the first argument." >&2
    echo "You can find your token in the Tunnelo dashboard." >&2
    exit 1
fi

TARBALL="tunnelo-agent_linux_${ARCH}.tar.gz"
URL="https://github.com/${REPO}/releases/latest/download/${TARBALL}"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

echo "Downloading ${URL} ..."
curl -fsSL "$URL" -o "$TMP/$TARBALL"
tar -xzf "$TMP/$TARBALL" -C "$TMP" tunnelo-agent
install -m 0755 "$TMP/tunnelo-agent" "$BIN_DIR/tunnelo-agent"
echo "Installed $BIN_DIR/tunnelo-agent"

# Write the environment file only when a token was provided, so re-running
# the installer to upgrade never clobbers an existing registration.
if [ -n "$TOKEN" ]; then
    mkdir -p "$(dirname "$ENV_FILE")"
    umask 077
    cat > "$ENV_FILE" <<EOF
# Tunnelo agent configuration. The token is only used for the first
# registration; credentials live in /var/lib/tunnelo-agent afterwards.
TUNNELO_TOKEN=${TOKEN}
# Uncomment if Jellyfin is not on this machine at the default port:
#TUNNELO_JELLYFIN_URL=http://127.0.0.1:8096
EOF
    echo "Wrote $ENV_FILE"
fi

cat > "$UNIT_FILE" <<'EOF'
[Unit]
Description=Tunnelo agent (WireGuard tunnel for remote Jellyfin access)
Documentation=https://github.com/abiteman/tunnelo-agent
After=network-online.target
Wants=network-online.target

[Service]
ExecStart=/usr/local/bin/tunnelo-agent
EnvironmentFile=/etc/tunnelo-agent/agent.env
Restart=always
RestartSec=5
StateDirectory=tunnelo-agent
Environment=TUNNELO_STATE_DIR=/var/lib/tunnelo-agent

# The agent only needs CAP_NET_ADMIN (WireGuard interface + routes).
CapabilityBoundingSet=CAP_NET_ADMIN
NoNewPrivileges=yes
ProtectSystem=strict
ProtectHome=yes
ReadWritePaths=/var/lib/tunnelo-agent
PrivateTmp=yes

[Install]
WantedBy=multi-user.target
EOF

systemctl daemon-reload
systemctl enable --now tunnelo-agent
echo
echo "Tunnelo agent is running. Check it with:"
echo "  systemctl status tunnelo-agent"
echo "  journalctl -u tunnelo-agent -f"
