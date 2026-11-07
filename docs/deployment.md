# Deployment guide

From a blank SD card to a Raspberry Pi that charges your Tesla from the sun, with optional remote access through Cloudflare. Allow about an hour, most of it waiting for downloads.

- [What you need](#what-you-need)
- [Part 1: Prepare the Raspberry Pi](#part-1-prepare-the-raspberry-pi)
- [Part 2: Configure](#part-2-configure)
- [Part 3: Build and deploy](#part-3-build-and-deploy)
- [Part 4: Pair the car](#part-4-pair-the-car)
- [Part 5: Go live](#part-5-go-live)
- [Part 6: Remote access with Cloudflare (optional)](#part-6-remote-access-with-cloudflare-optional)
- [Operations](#operations)
- [Troubleshooting](#troubleshooting)

```
Workstation (Windows / Linux / macOS)              Raspberry Pi
┌──────────────────────────────┐                 ┌───────────────────────────────────┐
│ git clone + config.yaml      │                 │ /opt/deye-monitor                  │
│ docker buildx  → linux/arm64 │ ── ssh / scp ─► │   docker-compose.yml, config.yaml  │
│ deploy.ps1  |  deploy.sh     │                 │   .env  (TZ, TUNNEL_TOKEN; yours)  │
└──────────────────────────────┘                 │ volume tesla-state → /state        │
                                                 │   tesla-key.pem  tesla.json  .db   │
                                                 └───────────────────────────────────┘
```

The Pi never compiles anything: the image is cross-built on your workstation and shipped over SSH. Everything the app learns or generates lives in the `tesla-state` volume, which deployments never touch.

## What you need

| | Tested with | Notes |
|---|---|---|
| Hybrid inverter | Deye SUN-6K-SG03LP1-EU | Other Deye/Sunsynk single-phase hybrids with a Solarman logger share most registers |
| Inverter logger | Solarman Wi-Fi stick (LSW-3) | Reachable on your LAN, port 8899; note its serial number |
| Wall connector | Tesla Wall Connector Gen 3 | On your Wi-Fi with a fixed IP. Optional but strongly recommended |
| Car | Tesla Model Y | Any Tesla supported by [vehicle-command](https://github.com/teslamotors/vehicle-command) BLE: Model 3/Y and recent S/X |
| Controller | Raspberry Pi Zero 2 W | Any 64-bit Pi with Bluetooth works. Must sit **within good Bluetooth range of where the car parks** |
| Workstation | Docker Desktop / Docker Engine with buildx | Windows (PowerShell) or Linux/macOS (bash) |
| Optional | A domain on Cloudflare | For remote access (free Zero Trust plan) |

## Part 1: Prepare the Raspberry Pi

### 1.1 Flash Raspberry Pi OS

With the [Raspberry Pi Imager](https://www.raspberrypi.com/software/), flash **Raspberry Pi OS Lite (64-bit)**. In the imager's settings set a hostname (e.g. `teslapi`), enable SSH with your public key, your Wi-Fi, and a user (e.g. `pi`).

### 1.2 First boot

Boot, wait a minute, then connect:

```bash
ssh pi@teslapi.local
```

Give the Pi a fixed address (a DHCP reservation on your router is easiest), and add an alias to `~/.ssh/config` on your workstation so the deploy command stays short:

```
Host teslapi
    HostName 192.168.1.100
    User pi
```

### 1.3 Install Docker

```bash
curl -fsSL https://get.docker.com | sh
sudo usermod -aG docker $USER
exit   # log out and back in for the group change
```

### 1.4 Check Bluetooth and Wi-Fi

```bash
sudo systemctl enable --now bluetooth
hciconfig            # hci0 should be "UP RUNNING"
```

Turn off Wi-Fi power saving; on a Pi Zero 2 W it causes dropouts that look like the app going quiet:

```bash
sudo nmcli connection modify preconfigured 802-11-wireless.powersave 2
sudo nmcli connection up preconfigured
```

(`preconfigured` is the connection the imager creates; `nmcli connection show` lists yours.)

## Part 2: Configure

On your workstation:

```bash
git clone https://github.com/vbocan/tesla-charge-control.git
cd tesla-charge-control
cp config.example.yaml config.yaml
```

`config.yaml` is git-ignored. Every setting is documented inline; the ones you must set:

| Setting | What | Where to find it |
|---|---|---|
| `connection.host` | Logger IP | Your router's DHCP list |
| `connection.logger_sn` | Logger serial number | Sticker on the Wi-Fi stick (not the inverter's serial) |
| `max_output_w` | Inverter max AC output | Datasheet (6000 for a SUN-6K) |
| `battery.*`, `solar.*` | Your battery and array | Datasheets |
| `tesla.vin` | The car's VIN | Tesla app → Controls → Software |
| `tesla.max_amps` | Wall connector limit | Your installer / breaker rating |
| `tesla.twc_url` | Wall connector address | `http://<its IP>` |
| `tesla.dry_run` | Leave `true` for now | You flip it in Part 5 |

Optionally create `/opt/deye-monitor/.env` on the Pi to set the container's time zone (logs and the charging log use it):

```bash
ssh teslapi "sudo mkdir -p /opt/deye-monitor && sudo chown \$(whoami) /opt/deye-monitor && echo 'TZ=Europe/Berlin' >> /opt/deye-monitor/.env"
```

## Part 3: Build and deploy

Windows (PowerShell):

```powershell
.\deploy.ps1 teslapi
```

Linux / macOS:

```bash
./deploy.sh teslapi
```

The script:

1. cross-builds the `linux/arm64` image with buildx (it creates a `multiarch` builder the first time; on Linux you may need QEMU: `docker run --privileged --rm tonistiigi/binfmt --install arm64`);
2. checks the Pi, and refuses to continue if a Cloudflare tunnel is enabled but its token is missing;
3. copies the image, `config.yaml` and the right compose file to `/opt/deye-monitor` (base, Caddy or tunnel, depending on your config);
4. loads the image and runs `docker compose up -d --remove-orphans`.

Open `http://teslapi:8080`. You should see live inverter data within a few seconds.

> The production compose files run the container with host networking and `privileged`, and mount `/var/run/dbus`, because BlueZ needs the host's Bluetooth stack. Nothing needs uncommenting.

## Part 4: Pair the car

Pairing generates a BLE key pair in `/state/tesla-key.pem` (the previous one, if any, is backed up) and enrols it as a key on the car. It works in dry-run mode too:

1. Sit in the car with your **key card**, close to the Pi.
2. On the dashboard click **Pair car…** → **Start pairing**.
3. When the car shows the prompt, **tap your key card** on the console and confirm on the touchscreen.

The dialog streams progress and says when pairing is complete. The new key appears in the car under *Controls → Locks*. (If you prefer the command line, Tesla's `tesla-control -ble add-key-request <public key> owner cloud_key` does the same with `/state/tesla-key.pem.pub`.)
## Part 5: Go live

1. Set `tesla.dry_run: false` in `config.yaml` and deploy again.
2. Click **Refresh** on the Vehicle Charge card: the car's battery level should appear.
3. Choose **Solar (Battery First)**. Watch the decision log (`/log`): on a sunny day you'll see the surplus build up, the start delay count down, and charging begin.
4. Test the outage protection once, safely, while the car is charging:

   ```bash
   curl -X POST "http://teslapi:8080/api/debug/outage?on=true"    # car stops; banner appears
   curl -X POST "http://teslapi:8080/api/debug/outage?on=false"   # resumes after grid_restore_delay_s
   ```

### Choosing a mode

| Mode | Use it when |
|---|---|
| **Idle** | You want the car's own schedule. The Pi still keeps the inverter under its limit. |
| **Solar (Battery First)** | Default. Car charges only from what would be exported once the home battery is served. |
| **Solar (Car First)** | You want the car to take PV ahead of the home battery. The grid, not the battery, covers dips. |
| **Max** | You need range now. As fast as the inverter budget allows, from any source. |

## Part 6: Remote access with Cloudflare (optional)

Reach the dashboard from anywhere without opening a port. `cloudflared` on the Pi makes an outbound connection to Cloudflare, and **Cloudflare Access** puts a login (email one-time code) in front of it. The app verifies the Access token itself as a second gate, so a mistake in the Access rules does not expose a page that can start the car.

```
browser ─► Cloudflare edge ─(Access login)─► tunnel ─► cloudflared ─► app :8080
                                                                  └─ re-verifies the Access JWT
```

### Quick path: one command

`deploy/cloudflare-setup.ps1` creates everything through the Cloudflare API: the tunnel and its route, the DNS record, the Access application and an Allow policy for your email. It writes the tunnel token to `.env` on the Pi (over ssh stdin, never printed) and fills `cloudflare_tunnel`, `access_team_domain` and `access_aud` into `config.yaml`. It is idempotent and never overwrites anything it did not create. (It needs PowerShell; on Linux/macOS install `pwsh` or follow the manual steps.)

1. Enable Zero Trust once on your Cloudflare account (free plan; choose a team name).
2. Create an API token (*My Profile → API Tokens → Create Token → Custom*), restricted to your zone, with a short expiry:

   | Scope | Permission | Level |
   |---|---|---|
   | Account | Cloudflare Tunnel | Edit |
   | Account | Access: Apps and Policies | Edit |
   | Account | Access: Organizations, Identity Providers, and Groups | Read |
   | Zone | DNS | Edit |
   | Zone | Zone | Read |

3. Enter it at a prompt (never in a command line, which lands in your history), preview, then apply:

   ```powershell
   $secure = Read-Host "Cloudflare API token" -AsSecureString
   $env:CLOUDFLARE_API_TOKEN = [System.Net.NetworkCredential]::new('', $secure).Password
   .\deploy\cloudflare-setup.ps1 -Hostname solar.example.com -Email you@example.com -RpiHost teslapi -WhatIf
   .\deploy\cloudflare-setup.ps1 -Hostname solar.example.com -Email you@example.com -RpiHost teslapi
   ```

4. Delete the API token, deploy again, and check it (below).

### Manual path

1. **Zero Trust → Networks → Tunnels → Create a tunnel** (Cloudflared). Copy the **tunnel token**. Add a public hostname, e.g. `solar.example.com`, service **HTTP** `localhost:8080`.
2. **Access → Applications → Add → Self-hosted** for the same hostname. Add an **Allow** policy for your email(s). Copy the **Application Audience (AUD) tag**, and note your team domain (`<team>.cloudflareaccess.com`).
3. Put the token on the Pi:

   ```bash
   ssh teslapi "echo 'TUNNEL_TOKEN=<token>' >> /opt/deye-monitor/.env && chmod 600 /opt/deye-monitor/.env"
   ```

4. In `config.yaml`:

   ```yaml
   cloudflare_tunnel: true
   access_team_domain: "<team>.cloudflareaccess.com"
   access_aud: "<Application Audience tag>"
   ```

5. Deploy. The script now ships `deploy/compose-tunnel.yml` (the app plus `cloudflared`).

### Check it

1. Open `https://solar.example.com` in a private window: Cloudflare login, then the dashboard with live updates.
2. The container log says `Cloudflare Access: requests through a proxy must carry a valid Access token for <team>.cloudflareaccess.com`.
3. Pretend to be a proxied request from the LAN; it must get a **403**, and the log shows `[ACCESS] refused`:

   ```bash
   curl -i -H "Cf-Connecting-Ip: 203.0.113.7" http://teslapi:8080/
   ```

### Notes

- Leave `auth_token` empty with the tunnel: tunnel visitors need Access, the LAN needs nothing, and a direct request from a public address is refused.
- Only humans: the check requires an `email` claim, so Access service tokens are rejected.
- Turning it off: `cloudflare_tunnel: false` and deploy; delete the tunnel in the dashboard to revoke the token for good.
- During an internet outage remote access is gone, but LAN access and every car protection keep working.

### Alternative: Caddy with Let's Encrypt

Set `domain: "solar.example.com"`, forward ports 80 and 443 to the Pi, **set a long `auth_token`**, and deploy: the script ships `deploy/compose-caddy.yml`. The Cloudflare route is recommended because it opens no ports.

## Operations

| Task | Command |
|---|---|
| Redeploy after changes | `./deploy.sh teslapi` / `.\deploy.ps1 teslapi` (state is preserved) |
| Follow logs | `ssh teslapi 'cd /opt/deye-monitor && docker compose logs -f'` |
| Restart | `ssh teslapi 'cd /opt/deye-monitor && docker compose restart'` |
| Config-only change | `scp config.yaml teslapi:/opt/deye-monitor/ && ssh teslapi 'cd /opt/deye-monitor && docker compose restart'` |
| Back up the BLE key | `ssh teslapi 'sudo cat /var/lib/docker/volumes/deye-monitor_tesla-state/_data/tesla-key.pem' > tesla-key.pem` |
| Back up the database | `ssh teslapi 'sudo cat /var/lib/docker/volumes/deye-monitor_tesla-state/_data/tesla.db' > tesla.db` |
| Resource use | `ssh teslapi 'docker stats --no-stream'` (expect ~20 MB RAM, <1 % CPU) |
| Update vehicle-command | `go get github.com/teslamotors/vehicle-command@latest && go mod tidy`, then deploy |

Keep the key backup somewhere safe: anyone holding it can command your car within Bluetooth range.

## Troubleshooting

**Dashboard loads but shows no data.** Check the logger is reachable from the Pi (`nc -vz <logger-ip> 8899`) and the serial number is right; look for `Read error` in the logs. Only one client may talk to the logger at a time; close other Solarman integrations (e.g. Home Assistant) or point them elsewhere.

**"Car not reachable", BLE connect timeouts, "failed to enumerate device services".** Check **signal strength first**: it is nearly always the Pi's position. Stop the container (the app holds the adapter), then scan:

```bash
cd /opt/deye-monitor && docker compose stop deye-monitor
sudo btmgmt --index 0 find -l | tee scan.txt     # ~50 s, Ctrl-C to stop
awk '/dev_found/{a=$3;r=$8} /^name /{print a,r,$0}' scan.txt
docker compose start deye-monitor
```

The car advertises as `S<16 hex>C`. Around −50 to −70 dBm is healthy; below −85 dBm commands time out. A single scan sample is noisy (±8 dB), so judge a new position by whether the app's reads succeed. Note that a `name` line follows the `dev_found` line it belongs to.

**The app seems to have gone quiet after a power cut.** An unclean shutdown can leave NUL bytes in the container's JSON log, and `docker logs` silently stops at them. Read the file directly:

```bash
sudo sh -c 'tr -d "\000" < $(docker inspect -f "{{.LogPath}}" deye-monitor-deye-monitor-1)'
```

**Short false grid outages.** The logger sometimes returns garbled register blocks (e.g. 2.00 Hz); these are ignored by design. If you still see false outages, open an issue with the log lines around them.

**Deploy says the tunnel token is missing.** `cloudflare_tunnel: true` but `/opt/deye-monitor/.env` has no `TUNNEL_TOKEN`. Nothing was changed on the Pi; add the token (Part 6) and deploy again.

**Image load fails.** Check disk space on the Pi (`df -h`); keep ~300 MB free.
