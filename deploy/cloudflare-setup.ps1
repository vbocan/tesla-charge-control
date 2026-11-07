<#
.SYNOPSIS
  Sets up the Cloudflare side of remote access for the dashboard, in one command.

.DESCRIPTION
  Creates (or reuses) through the Cloudflare API:
    - a remotely managed Cloudflare Tunnel, routed to http://localhost:8080
    - the proxied DNS record for the hostname
    - a self-hosted Access application for the hostname, with one Allow policy
      for your email address
  then reads the Access team domain and Audience (AUD) tag, writes the tunnel
  token to /opt/deye-monitor/.env on the RPi, and fills the Access values into
  config.yaml (cloudflare_tunnel, access_team_domain, access_aud).

  It never overwrites anything it did not create: an existing DNS record, tunnel
  or Access application that does not match is reported and the script stops.
  It is safe to run again. Use -WhatIf first to see what it would do; reads still
  happen, nothing is written.

  The API token is read from the CLOUDFLARE_API_TOKEN environment variable (never
  a parameter) and is never printed. Enter it with the prompt shown in the example
  below rather than typing it into a command: a typed command is saved in your
  PowerShell history, and anything you paste into a chat or a ticket is exposed.
  The tunnel token goes to the RPi over ssh stdin and is never printed either.

.PARAMETER Hostname
  Public hostname for the dashboard, e.g. solar.example.com. Its domain must
  be a zone on your Cloudflare account.

.PARAMETER Email
  The one address the Access policy allows in.

.EXAMPLE
  $secure = Read-Host "Cloudflare API token" -AsSecureString
  $env:CLOUDFLARE_API_TOKEN = [System.Net.NetworkCredential]::new('', $secure).Password
  .\deploy\cloudflare-setup.ps1 -Hostname solar.example.com -Email you@example.com -RpiHost pi@192.168.1.100 -WhatIf
  .\deploy\cloudflare-setup.ps1 -Hostname solar.example.com -Email you@example.com -RpiHost pi@192.168.1.100

.NOTES
  Create the API token at dash.cloudflare.com > My Profile > API Tokens > Create
  Token > Custom token, with these permissions (restrict the zone to your domain,
  give it a short expiry and delete it afterwards):
    Account > Cloudflare Tunnel            > Edit
    Account > Access: Apps and Policies    > Edit
    Account > Access: Organizations, Identity Providers, and Groups > Read
    Zone    > DNS                          > Edit
    Zone    > Zone                         > Read
  Zero Trust must have been enabled once for the account (a team name chosen).
  The account is taken from the zone, so the token needs no permission to list
  accounts; pass -AccountId only if Cloudflare does not report the zone's owner.
  When you are done, delete the token (My Profile > API Tokens).
#>
[CmdletBinding(SupportsShouldProcess = $true, ConfirmImpact = 'Medium')]
param(
    [Parameter(Mandatory = $true)] [string]$Hostname,
    [Parameter(Mandatory = $true)] [string]$Email,
    [string]$RpiHost = "",                                         # the Pi, e.g. pi@192.168.1.100 (required unless -SkipRpi)
    [string]$RemoteDir = "/opt/deye-monitor",
    [string]$TunnelName = "tesla-charge-control",
    [string]$AppName = "Tesla Charge Control",
    [string]$SessionDuration = "24h",
    [string]$AccountId = $env:CLOUDFLARE_ACCOUNT_ID,
    [string]$ConfigPath = (Join-Path $PSScriptRoot "..\config.yaml"),
    [string]$ApiBase = "https://api.cloudflare.com/client/v4",   # a test hook: point at a mock API
    [string]$SshCommand = "ssh",                                   # a test hook: stand-in for ssh
    [switch]$SkipRpi
)

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

$script:Token = $env:CLOUDFLARE_API_TOKEN
if (-not $script:Token) {
    Write-Host "CLOUDFLARE_API_TOKEN is not set." -ForegroundColor Red
    Write-Host "Create a scoped API token (permissions are listed in this script's help: Get-Help $PSCommandPath -Full),"
    Write-Host "then enter it without typing it into a command:"
    Write-Host "  `$secure = Read-Host 'Cloudflare API token' -AsSecureString"
    Write-Host "  `$env:CLOUDFLARE_API_TOKEN = [System.Net.NetworkCredential]::new('', `$secure).Password"
    exit 1
}

if (-not $RpiHost -and -not $SkipRpi) {
    throw "Pass -RpiHost (the Pi's ssh target, e.g. pi@192.168.1.100) so the tunnel token can be written there, or -SkipRpi."
}

$Hostname = $Hostname.Trim().ToLowerInvariant()
if ($Hostname -notmatch '^[a-z0-9]([a-z0-9-]*[a-z0-9])?(\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)+$') {
    throw "'$Hostname' is not a valid host name."
}
if ($Email -notmatch '^[^@\s]+@[^@\s]+\.[^@\s]+$') { throw "'$Email' is not a valid email address." }
if (-not (Test-Path $ConfigPath)) { throw "config.yaml not found at $ConfigPath" }

# ---------------------------------------------------------------------------
# Cloudflare API
# ---------------------------------------------------------------------------

function ConvertTo-ApiErrorText {
    param($Detail)
    if (-not $Detail) { return "no details" }
    try {
        $parsed = $Detail | ConvertFrom-Json
        if ($parsed.errors) {
            return (($parsed.errors | ForEach-Object { "[$($_.code)] $($_.message)" }) -join "; ")
        }
    } catch { }
    return $Detail
}

# Calls the API and returns .result. Any failure throws with Cloudflare's message.
function Invoke-CF {
    param([string]$Method, [string]$Path, [object]$Body = $null)
    $req = @{
        Method      = $Method
        Uri         = "$ApiBase$Path"
        Headers     = @{ Authorization = "Bearer $script:Token" }
        ContentType = 'application/json'
    }
    if ($null -ne $Body) { $req.Body = ($Body | ConvertTo-Json -Depth 10 -Compress) }
    try {
        $resp = Invoke-RestMethod @req
    } catch {
        $detail = $null
        if ($_.ErrorDetails -and $_.ErrorDetails.Message) {
            $detail = $_.ErrorDetails.Message
        } elseif ($_.Exception.Response) {
            try { $detail = (New-Object System.IO.StreamReader($_.Exception.Response.GetResponseStream())).ReadToEnd() } catch { }
        } else {
            $detail = $_.Exception.Message
        }
        throw "Cloudflare API $Method $Path failed: $(ConvertTo-ApiErrorText $detail)"
    }
    if ($resp.PSObject.Properties.Name -contains 'success' -and -not $resp.success) {
        throw "Cloudflare API $Method $Path failed: $(ConvertTo-ApiErrorText ($resp | ConvertTo-Json -Depth 5 -Compress))"
    }
    return $resp.result
}

function Write-Step { param([string]$Text) Write-Host ""; Write-Host "== $Text" -ForegroundColor Cyan }
function Write-Done { param([string]$Text) Write-Host "   $Text" -ForegroundColor Green }
function Write-Plan { param([string]$Text) Write-Host "   (would) $Text" -ForegroundColor Yellow }

# True if the object has the property (API objects omit fields that are unset).
function Test-Prop { param($Obj, [string]$Name) return ($null -ne $Obj) -and ($Obj.PSObject.Properties.Name -contains $Name) }

# ---------------------------------------------------------------------------
# 1. Account and zone
# ---------------------------------------------------------------------------

Write-Step "Zone and account"

# The zone is the longest suffix of the hostname that is a zone this token can see.
$zone = $null
$labels = $Hostname.Split('.')
for ($i = 0; $i -le $labels.Count - 2 -and -not $zone; $i++) {
    $candidate = ($labels[$i..($labels.Count - 1)]) -join '.'
    $found = @(Invoke-CF GET "/zones?name=$candidate")
    if ($found.Count -ge 1) { $zone = $found[0] }
}
if (-not $zone) { throw "No Cloudflare zone for '$Hostname' is visible to this token. Is the domain on your account, and does the token have Zone > Zone > Read for it?" }
Write-Done "Zone: $($zone.name)"

# The zone says which account owns it, so the token needs no permission to list accounts.
if (-not $AccountId) {
    if ((Test-Prop $zone 'account') -and (Test-Prop $zone.account 'id') -and $zone.account.id) {
        $AccountId = $zone.account.id
        $accountName = if (Test-Prop $zone.account 'name') { $zone.account.name } else { $AccountId }
        Write-Done "Account: $accountName (the zone's owner)"
    } else {
        throw "Cloudflare did not say which account owns the zone. Pass -AccountId (the id in your dashboard URL: dash.cloudflare.com/<account id>/...)."
    }
} else {
    Write-Done "Account: $AccountId"
}

# ---------------------------------------------------------------------------
# 2. Tunnel
# ---------------------------------------------------------------------------

Write-Step "Tunnel '$TunnelName'"
$tunnelId = $null
$existing = @(Invoke-CF GET "/accounts/$AccountId/cfd_tunnel?name=$([uri]::EscapeDataString($TunnelName))&is_deleted=false")
if ($existing.Count -gt 0) {
    $tunnel = $existing[0]
    if ((Test-Prop $tunnel 'config_src') -and $tunnel.config_src -eq 'local') {
        throw "A tunnel named '$TunnelName' exists but is managed by a local config file, not the dashboard. Use another -TunnelName or remove it."
    }
    $tunnelId = $tunnel.id
    Write-Done "Reusing existing tunnel $tunnelId"
} elseif ($PSCmdlet.ShouldProcess("account $AccountId", "create tunnel '$TunnelName'")) {
    $tunnel = Invoke-CF POST "/accounts/$AccountId/cfd_tunnel" @{ name = $TunnelName; config_src = 'cloudflare' }
    $tunnelId = $tunnel.id
    Write-Done "Created tunnel $tunnelId"
} else {
    $tunnelId = '<new tunnel id>'
}

# Ingress: keep any other rules the tunnel already has, set ours, end with the catch-all.
Write-Step "Tunnel route  $Hostname -> http://localhost:8080"
$rules = @()
if ($existing.Count -gt 0) {
    $cfg = Invoke-CF GET "/accounts/$AccountId/cfd_tunnel/$tunnelId/configurations"
    if ((Test-Prop $cfg 'config') -and (Test-Prop $cfg.config 'ingress') -and $cfg.config.ingress) {
        $rules = @($cfg.config.ingress | Where-Object { (Test-Prop $_ 'hostname') -and $_.hostname -and $_.hostname -ne $Hostname })
    }
}
$ingress = @($rules) + @(@{ hostname = $Hostname; service = 'http://localhost:8080' }) + @(@{ service = 'http_status:404' })
if ($PSCmdlet.ShouldProcess("tunnel $tunnelId", "route $Hostname to http://localhost:8080")) {
    [void](Invoke-CF PUT "/accounts/$AccountId/cfd_tunnel/$tunnelId/configurations" @{ config = @{ ingress = $ingress } })
    Write-Done "Route set"
}

# ---------------------------------------------------------------------------
# 3. DNS record
# ---------------------------------------------------------------------------

Write-Step "DNS record"
$cnameTarget = "$tunnelId.cfargotunnel.com"
$records = @(Invoke-CF GET "/zones/$($zone.id)/dns_records?name=$Hostname")
if ($records.Count -gt 0) {
    $rec = $records[0]
    if ($rec.type -eq 'CNAME' -and $rec.content -eq $cnameTarget) {
        Write-Done "Already points at the tunnel"
    } else {
        throw "A DNS record for $Hostname already exists ($($rec.type) -> $($rec.content)) and is not this tunnel's. Remove or change it in the dashboard, then run again. Nothing was overwritten."
    }
} elseif ($PSCmdlet.ShouldProcess("zone $($zone.name)", "create proxied CNAME $Hostname -> $cnameTarget")) {
    [void](Invoke-CF POST "/zones/$($zone.id)/dns_records" @{
        type = 'CNAME'; name = $Hostname; content = $cnameTarget; proxied = $true
        comment = 'Tesla Charge Control tunnel'
    })
    Write-Done "Created $Hostname -> $cnameTarget (proxied)"
}

# ---------------------------------------------------------------------------
# 4. Access application and policy
# ---------------------------------------------------------------------------

Write-Step "Access application"
$org = Invoke-CF GET "/accounts/$AccountId/access/organizations"
$teamDomain = if (Test-Prop $org 'auth_domain') { $org.auth_domain } else { $null }
if (-not $teamDomain) { throw "Zero Trust does not seem to be enabled on this account (no team domain). Enable it once in the dashboard (Zero Trust, free plan) and run again." }
Write-Done "Team domain: $teamDomain"

$apps = @(Invoke-CF GET "/accounts/$AccountId/access/apps?per_page=100")
$app = $apps | Where-Object { $_.domain -eq $Hostname } | Select-Object -First 1
if ($app) {
    Write-Done "Reusing existing application '$($app.name)' ($($app.id))"
} elseif ($PSCmdlet.ShouldProcess("account $AccountId", "create Access application '$AppName' for $Hostname")) {
    $app = Invoke-CF POST "/accounts/$AccountId/access/apps" @{
        name = $AppName; domain = $Hostname; type = 'self_hosted'
        session_duration = $SessionDuration; app_launcher_visible = $false
    }
    Write-Done "Created application $($app.id)"
}

$aud = $null
if ($app) {
    $aud = if (Test-Prop $app 'aud') { $app.aud } else { $null }
    if (-not $aud) { throw "Cloudflare did not return an Audience (AUD) tag for the application." }
    $policies = @(Invoke-CF GET "/accounts/$AccountId/access/apps/$($app.id)/policies")
    $mine = @($policies | Where-Object {
        (Test-Prop $_ 'decision') -and $_.decision -eq 'allow' -and (Test-Prop $_ 'include') -and
        @($_.include | Where-Object { (Test-Prop $_ 'email') -and $_.email.email -eq $Email }).Count -gt 0
    })
    if ($mine.Count -gt 0) {
        Write-Done "Policy for $Email already exists"
    } elseif ($PSCmdlet.ShouldProcess("application $($app.id)", "add an Allow policy for $Email")) {
        [void](Invoke-CF POST "/accounts/$AccountId/access/apps/$($app.id)/policies" @{
            name = "Allow $Email"; decision = 'allow'; precedence = ($policies.Count + 1)
            include = @(@{ email = @{ email = $Email } })
        })
        Write-Done "Added an Allow policy for $Email"
    }
    # Anything else on a pre-existing application is not ours: show it, don't touch it.
    $others = @($policies | Where-Object { $mine -notcontains $_ })
    if ($others.Count -gt 0) {
        Write-Host "   Review: this application also has other policies that I did not create:" -ForegroundColor Yellow
        $others | ForEach-Object { Write-Host "     - $($_.name) ($($_.decision))" -ForegroundColor Yellow }
    }
}

# ---------------------------------------------------------------------------
# 5. Tunnel token -> the RPi's .env (over ssh stdin; never printed)
# ---------------------------------------------------------------------------

Write-Step "Tunnel token"
function Set-RemoteTunnelToken {
    param([string]$TunnelToken)
    $remote = "umask 077; cd $RemoteDir && { grep -v '^TUNNEL_TOKEN=' .env 2>/dev/null; cat; } > .env.new && mv .env.new .env && echo ok"
    $psi = New-Object System.Diagnostics.ProcessStartInfo
    $psi.FileName = $SshCommand
    $psi.Arguments = "$RpiHost `"$remote`""
    $psi.RedirectStandardInput = $true
    $psi.RedirectStandardOutput = $true
    $psi.RedirectStandardError = $true
    $psi.UseShellExecute = $false
    $proc = [System.Diagnostics.Process]::Start($psi)
    $bytes = [System.Text.Encoding]::UTF8.GetBytes("TUNNEL_TOKEN=$TunnelToken`n")   # raw bytes: no CRLF
    $proc.StandardInput.BaseStream.Write($bytes, 0, $bytes.Length)
    $proc.StandardInput.Close()
    $out = $proc.StandardOutput.ReadToEnd()
    $err = $proc.StandardError.ReadToEnd()
    $proc.WaitForExit()
    if ($proc.ExitCode -ne 0 -or $out -notmatch 'ok') {
        throw "Could not write $RemoteDir/.env on ${RpiHost}: $err"
    }
}

if ($SkipRpi) {
    Write-Plan "skipped (-SkipRpi): the tunnel token was not written anywhere"
} elseif ($existing.Count -eq 0 -and $tunnelId -like '<*') {
    Write-Plan "fetch the tunnel token and write it to $RemoteDir/.env on $RpiHost"
} elseif ($PSCmdlet.ShouldProcess("$RpiHost`:$RemoteDir/.env", "write TUNNEL_TOKEN (value not shown)")) {
    $tunnelToken = Invoke-CF GET "/accounts/$AccountId/cfd_tunnel/$tunnelId/token"
    if (-not $tunnelToken -or $tunnelToken -isnot [string]) { throw "Cloudflare did not return a tunnel token." }
    Set-RemoteTunnelToken -TunnelToken $tunnelToken
    Write-Done "Written to $RemoteDir/.env on $RpiHost (chmod 600, value not shown)"
    $tunnelToken = $null
}

# ---------------------------------------------------------------------------
# 6. config.yaml
# ---------------------------------------------------------------------------

Write-Step "config.yaml"
function Set-YamlValue {
    param([string]$Text, [string]$Key, [string]$Value)
    $pattern = "(?m)^(" + [regex]::Escape($Key) + ":[ \t]*)(?:`"[^`"]*`"|true|false)"
    $rx = New-Object System.Text.RegularExpressions.Regex($pattern)
    if (-not $rx.IsMatch($Text)) { throw "'$Key' not found in config.yaml (expected a line like  ${Key}: ...)" }
    $replacer = { param($m) $m.Groups[1].Value + $Value }.GetNewClosure()
    $evaluator = [System.Text.RegularExpressions.MatchEvaluator]$replacer
    return $rx.Replace($Text, $evaluator, 1)
}

if ($aud) {
    if ($PSCmdlet.ShouldProcess($ConfigPath, "set cloudflare_tunnel: true, access_team_domain, access_aud")) {
        $text = [System.IO.File]::ReadAllText($ConfigPath)
        $text = Set-YamlValue $text 'cloudflare_tunnel' 'true'
        $text = Set-YamlValue $text 'access_team_domain' "`"$teamDomain`""
        $text = Set-YamlValue $text 'access_aud' "`"$aud`""
        [System.IO.File]::WriteAllText($ConfigPath, $text, (New-Object System.Text.UTF8Encoding($false)))
        Write-Done "Updated $ConfigPath"
    }
} else {
    Write-Plan "set cloudflare_tunnel: true, access_team_domain: `"$teamDomain`" and access_aud once the application exists"
}

# ---------------------------------------------------------------------------
# Done
# ---------------------------------------------------------------------------

Write-Host ""
if ($WhatIfPreference) {
    Write-Host "Nothing was changed (-WhatIf). Run again without it to apply." -ForegroundColor Yellow
} else {
    Write-Host "Cloudflare is set up." -ForegroundColor Green
    Write-Host "  Hostname      : https://$Hostname"
    Write-Host "  Allowed email : $Email"
    Write-Host "  Team domain   : $teamDomain"
    Write-Host "  Tunnel        : $TunnelName ($tunnelId)"
    Write-Host ""
    Write-Host "Next:"
    Write-Host "  1. Delete the API token you used (it is no longer needed)."
    Write-Host "  2. Deploy:   .\deploy.ps1 $RpiHost"
    Write-Host "  3. Check it: see docs/deployment.md, section 5.5."
}
