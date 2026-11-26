#!/bin/sh
# DeployMCP agent installer (Ubuntu / Debian).
#
#   curl -fsSL https://get.deploymcp.dev | sh -s -- --domain deploy.example.com --email you@example.com
#
# Options:
#   --domain DOMAIN   Public domain for the agent API (DNS A/AAAA record -> this VPS). Enables HTTPS.
#   --email EMAIL     Email for Let's Encrypt notices (optional).
#   --version TAG     Agent release tag (default: latest).
#   --no-caddy        Do not install/manage Caddy (no automatic HTTPS; apps exposed on host ports).
#   --public-host H   IP/host used in URLs of apps deployed without a domain (default: auto-detect).
#   --tunnel          Behind Cloudflare Tunnel: Caddy serves plain HTTP on :80 (Cloudflare does TLS).
#                     Point tunnel hostnames (agent domain + *.yourdomain) to http://localhost:80.
#   --http-port N     With --tunnel: Caddy listens on 127.0.0.1:N instead of :80
#                     (use when nginx/openresty/a panel already owns port 80).
#
# Env: DEPLOYMCP_REPO (default ZenithHawking/deploymcp), DEPLOYMCP_BINARY (local binary path, skips download).
set -eu

REPO="${DEPLOYMCP_REPO:-ZenithHawking/deploymcp}"
DMCP_VERSION="latest"
DOMAIN=""
EMAIL=""
NO_CADDY=0
TUNNEL=0
HTTP_PORT=""
PUBLIC_HOST=""
BIN=/usr/local/bin/deploymcp-agent
DATA_DIR=/var/lib/deploymcp
CONF_DIR=/etc/deploymcp

say()  { printf '\033[1;34m==>\033[0m %s\n' "$*"; }
warn() { printf '\033[1;33m!!\033[0m %s\n' "$*" >&2; }
die()  { printf '\033[1;31mxx\033[0m %s\n' "$*" >&2; exit 1; }

while [ $# -gt 0 ]; do
  case "$1" in
    --domain) DOMAIN="${2:-}"; shift 2 ;;
    --email) EMAIL="${2:-}"; shift 2 ;;
    --version) DMCP_VERSION="${2:-}"; shift 2 ;;
    --no-caddy) NO_CADDY=1; shift ;;
    --tunnel) TUNNEL=1; shift ;;
    --http-port) HTTP_PORT="${2:-}"; shift 2 ;;
    --public-host) PUBLIC_HOST="${2:-}"; shift 2 ;;
    -h|--help) echo "usage: install.sh [--domain D] [--email E] [--version TAG] [--no-caddy] [--public-host H] [--tunnel] [--http-port N]"; exit 0 ;;
    *) die "unknown option: $1" ;;
  esac
done

if [ "$TUNNEL" -eq 1 ]; then
  [ -n "$DOMAIN" ] || die "--tunnel requires --domain (the agent hostname routed through your tunnel)"
  [ "$NO_CADDY" -eq 0 ] || die "--tunnel needs Caddy; drop --no-caddy"
fi
CADDY_LISTEN=":80"
if [ -n "$HTTP_PORT" ]; then
  [ "$TUNNEL" -eq 1 ] || die "--http-port only works with --tunnel"
  case "$HTTP_PORT" in *[!0-9]*|"") die "--http-port must be a number" ;; esac
  CADDY_LISTEN="127.0.0.1:$HTTP_PORT"
fi

[ "$(id -u)" -eq 0 ] || die "run as root (e.g. curl ... | sudo sh -s -- --domain ...)"
[ -r /etc/os-release ] || die "cannot detect OS"
# shellcheck disable=SC1091
. /etc/os-release
case "${ID:-}:${ID_LIKE:-}" in
  ubuntu:*|debian:*|*:*debian*) ;;
  *) die "only Ubuntu/Debian are supported in v0.1 (detected: ${ID:-unknown})" ;;
esac

case "$(uname -m)" in
  x86_64|amd64) ARCH=amd64 ;;
  aarch64|arm64) ARCH=arm64 ;;
  *) die "unsupported CPU architecture: $(uname -m)" ;;
esac

export DEBIAN_FRONTEND=noninteractive
say "Installing base packages"
apt-get update -qq
apt-get install -y -qq curl ca-certificates gnupg >/dev/null

# ---------- Docker ----------
if ! command -v docker >/dev/null 2>&1; then
  say "Installing Docker"
  curl -fsSL https://get.docker.com | sh >/dev/null
fi
systemctl enable --now docker >/dev/null 2>&1 || true
docker compose version >/dev/null 2>&1 || apt-get install -y -qq docker-compose-plugin >/dev/null || warn "docker compose plugin missing: compose projects will not deploy"

# ---------- Caddy ----------
CADDY_ADMIN="http://localhost:2019"
if [ "$NO_CADDY" -eq 1 ]; then
  CADDY_ADMIN="off"
else
  if ! command -v caddy >/dev/null 2>&1; then
    say "Installing Caddy"
    apt-get install -y -qq debian-keyring debian-archive-keyring apt-transport-https >/dev/null
    curl -1sLf 'https://dl.cloudsmith.io/public/caddy/stable/gpg.key' | gpg --dearmor --yes -o /usr/share/keyrings/caddy-stable-archive-keyring.gpg
    curl -1sLf 'https://dl.cloudsmith.io/public/caddy/stable/debian.deb.txt' > /etc/apt/sources.list.d/caddy-stable.list
    apt-get update -qq
    apt-get install -y -qq caddy >/dev/null
  fi
  # The agent owns Caddy's config through the Admin API; --resume keeps it across restarts.
  mkdir -p /etc/systemd/system/caddy.service.d
  cat > /etc/systemd/system/caddy.service.d/deploymcp.conf <<'EOF'
[Service]
ExecStart=
ExecStart=/usr/bin/caddy run --environ --resume
ExecReload=
EOF
  # Start from an empty config: a previous Caddyfile-based autosave may bind
  # ports we must not touch (e.g. :80 owned by nginx). The agent loads the
  # real routes right after it starts. The old autosave is kept as a backup.
  AUTOSAVE=/var/lib/caddy/.config/caddy/autosave.json
  mkdir -p "$(dirname "$AUTOSAVE")"
  if [ -f "$AUTOSAVE" ] && ! grep -q '"deploymcp"' "$AUTOSAVE"; then
    cp "$AUTOSAVE" "$AUTOSAVE.bak.$(date +%s)"
  fi
  if [ ! -f "$AUTOSAVE" ] || ! grep -q '"deploymcp"' "$AUTOSAVE"; then
    echo '{"admin":{"listen":"localhost:2019"}}' > "$AUTOSAVE"
  fi
  chown -R caddy:caddy /var/lib/caddy/.config 2>/dev/null || true
  systemctl daemon-reload
  systemctl enable caddy >/dev/null 2>&1
  systemctl restart caddy || die "Caddy failed to start: journalctl -u caddy -n 30"
fi

# ---------- Agent binary ----------
if [ -n "${DEPLOYMCP_BINARY:-}" ]; then
  say "Installing agent from $DEPLOYMCP_BINARY"
  install -m 0755 "$DEPLOYMCP_BINARY" "$BIN"
else
  if [ "$DMCP_VERSION" = "latest" ]; then
    URL="https://github.com/$REPO/releases/latest/download/deploymcp-agent-linux-$ARCH"
  else
    URL="https://github.com/$REPO/releases/download/$DMCP_VERSION/deploymcp-agent-linux-$ARCH"
  fi
  say "Downloading agent ($ARCH, $DMCP_VERSION)"
  TMP="$(mktemp)"
  curl -fsSL "$URL" -o "$TMP" || die "download failed: $URL"
  install -m 0755 "$TMP" "$BIN"
  rm -f "$TMP"
fi

# ---------- Config ----------
if [ -z "$PUBLIC_HOST" ]; then
  PUBLIC_HOST="$(curl -fsS4 --max-time 5 https://api.ipify.org 2>/dev/null || hostname -I | awk '{print $1}')"
fi
if [ -n "$DOMAIN" ] && [ "$NO_CADDY" -eq 0 ]; then
  LISTEN="127.0.0.1:7070"
  API_URL="https://$DOMAIN"
else
  LISTEN="0.0.0.0:7070"
  API_URL="http://$PUBLIC_HOST:7070"
  warn "No --domain (or --no-caddy): the API is served over plain HTTP on port 7070."
  warn "Your token would travel unencrypted. Use --domain for production."
fi

mkdir -p "$CONF_DIR" "$DATA_DIR"
chmod 700 "$DATA_DIR"
cat > "$CONF_DIR/agent.env" <<EOF
DEPLOYMCP_DATA_DIR=$DATA_DIR
DEPLOYMCP_LISTEN=$LISTEN
DEPLOYMCP_DOMAIN=$DOMAIN
DEPLOYMCP_CADDY_ADMIN=$CADDY_ADMIN
DEPLOYMCP_ACME_EMAIL=$EMAIL
DEPLOYMCP_PUBLIC_HOST=$PUBLIC_HOST
DEPLOYMCP_TUNNEL=$TUNNEL
DEPLOYMCP_CADDY_LISTEN=$CADDY_LISTEN
EOF
chmod 600 "$CONF_DIR/agent.env"

cat > /etc/systemd/system/deploymcp-agent.service <<EOF
[Unit]
Description=DeployMCP agent
After=network-online.target docker.service caddy.service
Wants=network-online.target
Requires=docker.service

[Service]
EnvironmentFile=$CONF_DIR/agent.env
ExecStart=$BIN serve
Restart=always
RestartSec=3
LimitNOFILE=65536

[Install]
WantedBy=multi-user.target
EOF

TOKEN="$(DEPLOYMCP_DATA_DIR=$DATA_DIR "$BIN" init --token-only)"
if [ -z "$TOKEN" ]; then
  TOKEN="$(DEPLOYMCP_DATA_DIR=$DATA_DIR "$BIN" init --force --token-only)"
fi

systemctl daemon-reload
systemctl enable deploymcp-agent >/dev/null 2>&1
systemctl restart deploymcp-agent

say "Waiting for the agent"
i=0
until curl -fsS --max-time 3 "http://127.0.0.1:7070/health" >/dev/null 2>&1; do
  i=$((i+1)); [ $i -gt 20 ] && die "agent did not start: journalctl -u deploymcp-agent -n 50"
  sleep 1
done
if [ -n "$DOMAIN" ] && [ "$NO_CADDY" -eq 0 ]; then
  i=0
  until curl -fsS --max-time 5 "$API_URL/health" >/dev/null 2>&1; do
    i=$((i+1))
    if [ $i -gt 30 ]; then
      if [ "$TUNNEL" -eq 1 ]; then
        warn "$API_URL is not reachable yet. Add a tunnel public hostname $DOMAIN -> http://localhost:${CADDY_LISTEN##*:}"
      else
        warn "$API_URL is not reachable yet. Check that DNS for $DOMAIN points to $PUBLIC_HOST and ports 80/443 are open."
      fi
      break
    fi
    sleep 2
  done
fi

cat <<EOF

✅ DeployMCP Agent is running!  ($("$BIN" version))

👉 Run this command on your local machine to connect Claude Code:

claude mcp add deploymcp --scope user \\
  -e DEPLOYMCP_URL=$API_URL \\
  -e DEPLOYMCP_TOKEN=$TOKEN \\
  -- npx -y deploymcp

👉 Cursor / Windsurf (mcp.json):

{
  "mcpServers": {
    "deploymcp": {
      "command": "npx",
      "args": ["-y", "deploymcp"],
      "env": {
        "DEPLOYMCP_URL": "$API_URL",
        "DEPLOYMCP_TOKEN": "$TOKEN"
      }
    }
  }
}

The token is shown once. Manage tokens with:
  deploymcp-agent token create --apps my-app --perm write
  deploymcp-agent token list | deploymcp-agent token revoke ID
(prefix with DEPLOYMCP_DATA_DIR=$DATA_DIR if needed)
EOF
