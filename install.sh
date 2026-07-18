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
#
# Uninstall (removes the service + binary, keeps the token/credentials so a
# reinstall resumes cleanly; add --purge / TUNNELO_PURGE=1 to wipe those too):
#   curl -fsSL https://raw.githubusercontent.com/abiteman/tunnelo-agent/main/install.sh | sudo TUNNELO_UNINSTALL=1 sh
#   sudo ./install.sh --uninstall [--purge]
set -eu

REPO="abiteman/tunnelo-agent"
BIN_DIR="/usr/local/bin"
CONFIG_DIR="/etc/tunnelo-agent"
ENV_FILE="${CONFIG_DIR}/agent.env"
UNIT_FILE="/etc/systemd/system/tunnelo-agent.service"
STATE_DIR="/var/lib/tunnelo-agent"

if [ "$(id -u)" -ne 0 ]; then
    echo "error: this installer must run as root (it installs a systemd service)" >&2
    exit 1
fi

# --- uninstall -------------------------------------------------------------
# Parsed before anything install-specific so it works on any host (no arch
# check, no token needed) and is fully idempotent — every step tolerates the
# thing already being gone.
UNINSTALL=0
PURGE=0
for arg in "$@"; do
    case "$arg" in
        --uninstall|-u) UNINSTALL=1 ;;
        --purge)        UNINSTALL=1; PURGE=1 ;;
    esac
done
[ -n "${TUNNELO_UNINSTALL:-}" ] && UNINSTALL=1
[ -n "${TUNNELO_PURGE:-}" ] && { UNINSTALL=1; PURGE=1; }

if [ "$UNINSTALL" -eq 1 ]; then
    if command -v systemctl >/dev/null 2>&1; then
        systemctl disable --now tunnelo-agent 2>/dev/null || true
    fi
    rm -f "$UNIT_FILE"
    if command -v systemctl >/dev/null 2>&1; then
        systemctl daemon-reload 2>/dev/null || true
    fi
    rm -f "$BIN_DIR/tunnelo-agent"
    echo "Removed the tunnelo-agent service and binary."
    if [ "$PURGE" -eq 1 ]; then
        rm -rf "$CONFIG_DIR" "$STATE_DIR"
        echo "Purged $CONFIG_DIR and $STATE_DIR (token + agent credentials)."
    elif [ -d "$CONFIG_DIR" ] || [ -d "$STATE_DIR" ]; then
        echo "Kept $CONFIG_DIR and $STATE_DIR (token + agent credentials) so a"
        echo "reinstall resumes the same registration. Re-run with --purge to remove them."
    fi
    exit 0
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
#
# TUNNELO_GATEWAY_URL and TUNNELO_SERVICES are passed through from the
# installer's environment when set (the dashboard's one-liner sets the gateway
# URL, since the binary's compiled default only matches the public instance —
# self-hosted and non-default deployments must point the agent at their own
# api.<domain>). Both are optional; unset means the agent's built-in defaults.
if [ -n "$TOKEN" ]; then
    mkdir -p "$(dirname "$ENV_FILE")"
    umask 077
    {
        echo "# Tunnelo agent configuration. The token is only used for the first"
        echo "# registration; credentials live in /var/lib/tunnelo-agent afterwards."
        echo "TUNNELO_TOKEN=${TOKEN}"
        [ -n "${TUNNELO_GATEWAY_URL:-}" ] && echo "TUNNELO_GATEWAY_URL=${TUNNELO_GATEWAY_URL}"
        [ -n "${TUNNELO_SERVICES:-}" ] && echo "TUNNELO_SERVICES=${TUNNELO_SERVICES}"
        echo "# Uncomment if Jellyfin is not on this machine at the default port:"
        echo "#TUNNELO_SERVICE_URL=http://127.0.0.1:8096"
    } > "$ENV_FILE"
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
systemctl enable tunnelo-agent 2>/dev/null || true
# restart, not `enable --now`: on a re-run (upgrade, or a changed
# TUNNELO_SERVICES / gateway URL) the unit is already active, and `--now` only
# *starts* it — a no-op that leaves the old binary and env loaded. restart
# starts a stopped unit and reloads a running one, so a re-run always applies.
systemctl restart tunnelo-agent
echo
echo "Tunnelo agent is running. Check it with:"
echo "  systemctl status tunnelo-agent"
echo "  journalctl -u tunnelo-agent -f"
