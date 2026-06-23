#Requires -Version 5
<#
.SYNOPSIS
  devbox installer for Windows. Installs devbox.exe to a location you choose and
  (optionally) registers a keep-alive Scheduled Task that runs the sync daemon at
  logon and restarts it on failure.

.EXAMPLE
  irm https://git.shoemoney.ai/shoemoney/devbox-dist/releases/download/latest/install.ps1 | iex
  .\install.ps1 -BinDir "$env:LOCALAPPDATA\Programs\devbox" -Service
  .\install.ps1 -Hub                # also install devbox-hub.exe
#>
param(
  [string]$BinDir = $env:DEVBOX_BIN_DIR,
  [switch]$Hub,
  [switch]$Service,
  [switch]$NoService,
  [string]$ReleaseUrl = $(if ($env:DEVBOX_RELEASE_URL) { $env:DEVBOX_RELEASE_URL } else { "https://git.shoemoney.ai/shoemoney/devbox-dist/releases/download/latest" })
)
$ErrorActionPreference = "Stop"

function Say($m) { Write-Host $m }
function Die($m) { Write-Error $m; exit 1 }

$arch = if ($env:PROCESSOR_ARCHITECTURE -eq "ARM64") { "arm64" } else { "amd64" }
Say "📦 devbox installer — detected windows/$arch"

$scriptDir = if ($PSScriptRoot) { $PSScriptRoot } else { (Get-Location).Path }
$tmp = Join-Path $env:TEMP ("devbox-install-" + [System.IO.Path]::GetRandomFileName())
New-Item -ItemType Directory -Path $tmp -Force | Out-Null
$haveGo = [bool](Get-Command go -ErrorAction SilentlyContinue)

# Resolve a binary: explicit env, local dist\, download, or `go build`.
function Resolve-Bin($name) {
  $envVar = ($name.ToUpper().Replace("-", "_")) + "_BIN"   # devbox -> DEVBOX_BIN
  $explicit = [Environment]::GetEnvironmentVariable($envVar)
  if ($explicit -and (Test-Path $explicit)) { return $explicit }

  foreach ($d in @("$scriptDir\dist", "$scriptDir\..\dist", ".\dist")) {
    $cand = Get-ChildItem -Path $d -Filter "${name}_*_windows_${arch}.exe" -ErrorAction SilentlyContinue | Select-Object -First 1
    if ($cand) { return $cand.FullName }
  }
  $dest = Join-Path $tmp "$name.exe"
  try {
    Invoke-WebRequest -Uri "$ReleaseUrl/${name}_windows_${arch}.exe" -OutFile $dest -UseBasicParsing
    if ((Get-Item $dest).Length -gt 0) { return $dest }
  } catch { }
  if ($haveGo -and (Test-Path "$scriptDir\go.mod")) {
    Say "   building $name from source (go)..."
    $env:CGO_ENABLED = "0"
    Push-Location $scriptDir
    try { & go build -o $dest "./cmd/$name"; if ($LASTEXITCODE -ne 0) { throw } } finally { Pop-Location }
    return $dest
  }
  Die "could not find or fetch '$name' — set $envVar, run scripts/build-release.sh, or pass -ReleaseUrl"
}

# Choose install dir.
if (-not $BinDir) {
  $def = "$env:LOCALAPPDATA\Programs\devbox"
  if ([Environment]::UserInteractive) {
    $ans = Read-Host "Where should the binaries live? [$def]"
    $BinDir = if ($ans) { $ans } else { $def }
  } else { $BinDir = $def }
}
New-Item -ItemType Directory -Path $BinDir -Force | Out-Null
Say "🚚 installing to $BinDir"

function Install-One($name) {
  $src = Resolve-Bin $name
  Copy-Item -Path $src -Destination (Join-Path $BinDir "$name.exe") -Force
  Say "   ✅ $name -> $BinDir\$name.exe"
}
Install-One "devbox"
if ($Hub) { Install-One "devbox-hub" }

# Add to the user PATH (persisted) if missing.
$userPath = [Environment]::GetEnvironmentVariable("Path", "User")
if ($userPath -notlike "*$BinDir*") {
  [Environment]::SetEnvironmentVariable("Path", "$userPath;$BinDir", "User")
  Say "   ➕ added $BinDir to your user PATH (open a new terminal to pick it up)"
}

# Optional keep-alive Scheduled Task: runs `devbox start` at logon, restarts on
# failure — the Windows equivalent of launchd KeepAlive / systemd Restart=always.
function Install-Service {
  $exe = Join-Path $BinDir "devbox.exe"
  $action  = New-ScheduledTaskAction -Execute $exe -Argument "start"
  $trigger = New-ScheduledTaskTrigger -AtLogOn
  $settings = New-ScheduledTaskSettingsSet -StartWhenAvailable -RestartCount 999 -RestartInterval (New-TimeSpan -Minutes 1) -AllowStartIfOnBatteries -DontStopIfGoingOnBatteries
  Register-ScheduledTask -TaskName "devbox" -Action $action -Trigger $trigger -Settings $settings -Force | Out-Null
  Start-ScheduledTask -TaskName "devbox"
  Say "   ✅ scheduled task 'devbox' registered (runs at logon, restarts on failure)"
  Say "      stop: Unregister-ScheduledTask -TaskName devbox -Confirm:`$false"
}

$doService = $false
if ($Service) { $doService = $true }
elseif ($NoService) { $doService = $false }
elseif ([Environment]::UserInteractive) {
  $ans = Read-Host "Set up a keep-alive auto-restart service for the sync daemon? [Y/n]"
  $doService = ($ans -notmatch '^[Nn]')
}
if ($doService) { Say "🔁 setting up keep-alive service"; Install-Service }

# Windows' loose analog to macOS Full Disk Access is Controlled Folder Access
# (Defender ransomware protection) — OFF by default. If a user has it ON, a
# background sync of Documents/Desktop will be blocked until devbox is allowlisted.
try {
  $cfa = (Get-MpPreference -ErrorAction SilentlyContinue).EnableControlledFolderAccess
  if ($cfa -and $cfa -ne 0) {
    Say ""
    Say "🔐 Controlled Folder Access is ON — allow devbox to write protected folders:"
    Say "   Windows Security → Virus & threat protection → Ransomware protection →"
    Say "   Allow an app through Controlled folder access → add $BinDir\devbox.exe"
  }
} catch { }

Remove-Item -Recurse -Force $tmp -ErrorAction SilentlyContinue
Say ""
Say "🎉 done. Next: run 'devbox setup' to join a hub and start syncing."
if ($Hub) { Say "    hub: 'devbox-hub serve --dashboard' — or use the Dockerfile for a NAS (see README)." }
