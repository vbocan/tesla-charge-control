#
# Deploy Tesla Charge Control to a Raspberry Pi (Windows; deploy.sh is the
# Linux/macOS twin). On the Pi the app lives in /opt/deye-monitor.
#
# Usage:
#   .\deploy.ps1 <ssh-host>
#   .\deploy.ps1 pi@192.168.1.100
#   .\deploy.ps1 my-pi            # an alias from ~/.ssh/config
#
# What it does:
#   1. Cross-builds the ARM64 Docker image on this machine
#   2. Transfers the image + config + compose file to the RPi via SSH
#   3. Loads the image on the RPi
#   4. Restarts the container (preserves existing database/state)
#
# Prerequisites:
#   - Docker Desktop with buildx on this machine
#   - SSH access to the RPi (key-based auth recommended)
#   - Docker + Docker Compose installed on the RPi
#
# The script preserves:
#   - Tesla SQLite database (in Docker volume 'deye-monitor_tesla-state')
#   - Tesla charging mode state file
#   - /opt/deye-monitor/.env on the RPi (holds the Cloudflare tunnel token; never
#     transferred or overwritten)
#
# Remote access: with cloudflare_tunnel: true in config.yaml the container set
# includes cloudflared (compose-tunnel.yml), otherwise with a domain set it
# includes Caddy (compose-caddy.yml), otherwise just the app. See docs/deployment.md.

param(
    [Parameter(Mandatory=$true, Position=0)]
    [string]$RpiHost
)

$ErrorActionPreference = "Stop"

$RemoteDir = "/opt/deye-monitor"
$ImageName = "deye-monitor:latest"
$TarFile   = "deye-monitor-arm64.tar"
$ProjectDir = $PSScriptRoot

if (-not (Test-Path "$ProjectDir\config.yaml")) {
    Write-Host "config.yaml not found. Create it from the example first:" -ForegroundColor Red
    Write-Host "    Copy-Item config.example.yaml config.yaml   # then edit it"
    exit 1
}

Write-Host ""
Write-Host "=== Tesla Charge Control: deploy to $RpiHost ===" -ForegroundColor Cyan

# --- Step 1: Build ARM64 image ---
Write-Host ""
Write-Host "[1/5] Building ARM64 Docker image..." -ForegroundColor Yellow
Push-Location $ProjectDir

# Ensure multi-arch builder exists
$builderExists = docker buildx inspect multiarch 2>&1
if ($LASTEXITCODE -ne 0) {
    docker buildx create --name multiarch --driver docker-container
}
docker buildx use multiarch

docker buildx build `
    --platform linux/arm64 `
    -t $ImageName `
    --output "type=docker,dest=$TarFile" `
    .

if ($LASTEXITCODE -ne 0) {
    Write-Host "Build failed!" -ForegroundColor Red
    Pop-Location
    exit 1
}

$imageSize = (Get-Item $TarFile).Length / 1MB
Write-Host "    Image built: $TarFile ($([math]::Round($imageSize, 1)) MB)" -ForegroundColor Green

# --- Step 2: Create remote directory ---
Write-Host ""
Write-Host "[2/5] Preparing remote directory..." -ForegroundColor Yellow
ssh $RpiHost "sudo mkdir -p $RemoteDir && sudo chown `$(whoami) $RemoteDir"

# Is a Cloudflare Tunnel switched on in config.yaml? If so the tunnel token must
# already be on the RPi. Checked here, before anything is transferred: compose
# refuses to start when it is missing, and that would take the app down with it.
$tunnel = $false
foreach ($line in (Get-Content "$ProjectDir\config.yaml")) {
    if ($line -match '^\s*cloudflare_tunnel:\s*true\b') { $tunnel = $true; break }
}
if ($tunnel) {
    $hasToken = ssh $RpiHost "grep -qE '^TUNNEL_TOKEN=.+' $RemoteDir/.env 2>/dev/null && echo yes || echo no"
    if ($hasToken -notmatch 'yes') {
        Write-Host ""
        Write-Host "cloudflare_tunnel is true, but $RemoteDir/.env on the RPi has no TUNNEL_TOKEN." -ForegroundColor Red
        Write-Host "Create it on the RPi (get the token from the Cloudflare Zero Trust dashboard):" -ForegroundColor Red
        Write-Host "    echo 'TUNNEL_TOKEN=<token>' > $RemoteDir/.env && chmod 600 $RemoteDir/.env"
        Write-Host "Nothing was changed on the RPi. See docs/deployment.md." -ForegroundColor Red
        Pop-Location
        exit 1
    }
}

# --- Step 3: Transfer files ---
Write-Host ""
Write-Host "[3/5] Transferring files..." -ForegroundColor Yellow
scp $TarFile "${RpiHost}:${RemoteDir}/"
scp config.yaml "${RpiHost}:${RemoteDir}/"

# Check if a domain is configured for Caddy HTTPS
$domain = ""
$configLines = Get-Content "$ProjectDir\config.yaml"
foreach ($line in $configLines) {
    if ($line -match '^\s*domain:\s*"([^"]+)"' -and $line -notmatch '#') {
        $domain = $Matches[1]
        break
    }
}

if ($tunnel) {
    if ($domain) {
        Write-Host "    Note: cloudflare_tunnel is on, so the domain ($domain) and Caddy are not used" -ForegroundColor Yellow
    }
    Write-Host "    Cloudflare Tunnel enabled - deploying with cloudflared" -ForegroundColor Cyan
    scp "$ProjectDir\deploy\compose-tunnel.yml" "${RpiHost}:${RemoteDir}/docker-compose.yml"
} elseif ($domain) {
    Write-Host "    Domain: $domain - deploying with Caddy reverse proxy" -ForegroundColor Cyan
    # Upload Caddyfile
    ssh $RpiHost "printf '%s {\n    reverse_proxy localhost:8080\n}\n' '$domain' > $RemoteDir/Caddyfile"
    # Use compose with Caddy
    scp "$ProjectDir\deploy\compose-caddy.yml" "${RpiHost}:${RemoteDir}/docker-compose.yml"
} else {
    # Use base compose (no Caddy)
    scp "$ProjectDir\deploy\compose-base.yml" "${RpiHost}:${RemoteDir}/docker-compose.yml"
}

Write-Host "    Transferred: image, config, compose file" -ForegroundColor Green

# --- Step 4: Load image on RPi ---
Write-Host ""
Write-Host "[4/5] Loading Docker image on RPi..." -ForegroundColor Yellow
ssh $RpiHost "docker load < $RemoteDir/$TarFile"

# --- Step 5: Restart container ---
Write-Host ""
Write-Host "[5/5] Restarting container..." -ForegroundColor Yellow
ssh $RpiHost "cd $RemoteDir && docker compose up -d --remove-orphans"

Pop-Location

# Resolve hostname for the URL (supports SSH aliases and user@host)
$sshHostname = (ssh -G $RpiHost | Select-String "^hostname ").ToString() -replace "^hostname ", ""
$hostOnly = if ($sshHostname) { $sshHostname } else { $RpiHost -replace "^.*@", "" }

Write-Host ""
Write-Host "=== Deploy complete ===" -ForegroundColor Green
Write-Host "    Dashboard: http://${hostOnly}:8080"
Write-Host "    Logs:      ssh $RpiHost 'cd $RemoteDir && docker compose logs -f'"
Write-Host ""
Write-Host "    Note: Tesla database and state are preserved in the 'tesla-state' volume."
Write-Host "    Cleanup: ssh $RpiHost 'rm $RemoteDir/$TarFile'"
