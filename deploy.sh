#!/usr/bin/env bash
#
# Deploy Tesla Charge Control to a Raspberry Pi (Linux/macOS twin of deploy.ps1).
#
# Usage:
#   ./deploy.sh <ssh-host>
#   ./deploy.sh pi@192.168.1.100
#   ./deploy.sh my-pi            # an alias from ~/.ssh/config
#
# What it does:
#   1. Cross-builds the linux/arm64 Docker image on this machine
#   2. Copies the image, config.yaml and the right compose file to the Pi
#   3. Loads the image and (re)creates the containers
#
# It preserves the Pi's 'tesla-state' volume (BLE key, mode, SQLite log) and the
# /opt/deye-monitor/.env file (Cloudflare tunnel token, TZ), which it never
# creates, copies or overwrites.
#
# Prerequisites: Docker with buildx on this machine, SSH access to the Pi, Docker
# + Docker Compose on the Pi. See docs/deployment.md.

set -euo pipefail

if [[ $# -ne 1 ]]; then
    echo "Usage: $0 <ssh-host>   e.g. $0 pi@192.168.1.100" >&2
    exit 2
fi

RPI_HOST="$1"
REMOTE_DIR="/opt/deye-monitor"
IMAGE="deye-monitor:latest"
TAR="deye-monitor-arm64.tar"
PROJECT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
CONFIG="$PROJECT_DIR/config.yaml"

cyan()   { printf '\033[36m%s\033[0m\n' "$*"; }
yellow() { printf '\033[33m%s\033[0m\n' "$*"; }
green()  { printf '\033[32m%s\033[0m\n' "$*"; }
red()    { printf '\033[31m%s\033[0m\n' "$*" >&2; }

if [[ ! -f "$CONFIG" ]]; then
    red "config.yaml not found. Create it from the example first:"
    red "    cp config.example.yaml config.yaml   # then edit it"
    exit 1
fi

# Settings that decide which compose file is deployed.
tunnel=false
grep -qE '^\s*cloudflare_tunnel:\s*true\b' "$CONFIG" && tunnel=true
domain="$(sed -nE 's/^\s*domain:\s*"([^"]+)".*/\1/p' "$CONFIG" | head -n1)"

echo
cyan "=== Tesla Charge Control: deploy to $RPI_HOST ==="

# --- 1. Build ---------------------------------------------------------------
echo
yellow "[1/5] Building the ARM64 Docker image..."
cd "$PROJECT_DIR"
docker buildx inspect multiarch >/dev/null 2>&1 || docker buildx create --name multiarch --driver docker-container >/dev/null
docker buildx build --builder multiarch --platform linux/arm64 -t "$IMAGE" --output "type=docker,dest=$TAR" .
green "    Image built: $TAR ($(du -h "$TAR" | cut -f1))"

# --- 2. Remote directory and preflight --------------------------------------
echo
yellow "[2/5] Preparing $REMOTE_DIR on the Pi..."
ssh "$RPI_HOST" "sudo mkdir -p $REMOTE_DIR && sudo chown \$(whoami) $REMOTE_DIR"

# A tunnel needs its token on the Pi before anything changes there: compose
# refuses to start without it, and that would take the app down too.
if $tunnel; then
    if ! ssh "$RPI_HOST" "grep -qE '^TUNNEL_TOKEN=.+' $REMOTE_DIR/.env 2>/dev/null"; then
        red ""
        red "cloudflare_tunnel is true, but $REMOTE_DIR/.env on the Pi has no TUNNEL_TOKEN."
        red "Create it on the Pi (token from the Cloudflare Zero Trust dashboard):"
        red "    echo 'TUNNEL_TOKEN=<token>' > $REMOTE_DIR/.env && chmod 600 $REMOTE_DIR/.env"
        red "Nothing was changed on the Pi. See docs/deployment.md."
        exit 1
    fi
fi

# --- 3. Transfer ------------------------------------------------------------
echo
yellow "[3/5] Transferring files..."
scp "$TAR" "$CONFIG" "$RPI_HOST:$REMOTE_DIR/"
if $tunnel; then
    [[ -n "$domain" ]] && yellow "    Note: cloudflare_tunnel is on, so domain ($domain) and Caddy are not used"
    cyan "    Cloudflare Tunnel enabled: deploying with cloudflared"
    scp "$PROJECT_DIR/deploy/compose-tunnel.yml" "$RPI_HOST:$REMOTE_DIR/docker-compose.yml"
elif [[ -n "$domain" ]]; then
    cyan "    Domain: $domain: deploying with the Caddy reverse proxy"
    ssh "$RPI_HOST" "printf '%s {\n    reverse_proxy localhost:8080\n}\n' '$domain' > $REMOTE_DIR/Caddyfile"
    scp "$PROJECT_DIR/deploy/compose-caddy.yml" "$RPI_HOST:$REMOTE_DIR/docker-compose.yml"
else
    scp "$PROJECT_DIR/deploy/compose-base.yml" "$RPI_HOST:$REMOTE_DIR/docker-compose.yml"
fi
green "    Transferred: image, config, compose file"

# --- 4. Load ----------------------------------------------------------------
echo
yellow "[4/5] Loading the image on the Pi..."
ssh "$RPI_HOST" "docker load < $REMOTE_DIR/$TAR && rm $REMOTE_DIR/$TAR"

# --- 5. Restart -------------------------------------------------------------
echo
yellow "[5/5] Restarting the containers..."
ssh "$RPI_HOST" "cd $REMOTE_DIR && docker compose up -d --remove-orphans"

host="$(ssh -G "$RPI_HOST" 2>/dev/null | awk '/^hostname /{print $2; exit}')"
host="${host:-${RPI_HOST#*@}}"
echo
green "=== Deploy complete ==="
echo "    Dashboard: http://$host:8080"
echo "    Logs:      ssh $RPI_HOST 'cd $REMOTE_DIR && docker compose logs -f'"
