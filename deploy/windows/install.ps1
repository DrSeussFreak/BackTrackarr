<#
  Install or update BackTrackarr on Windows (run in an elevated PowerShell):

    irm https://raw.githubusercontent.com/DrSeussFreak/BackTrackarr/main/deploy/windows/install.ps1 | iex

  or, from an extracted release zip:  .\install.ps1

  - Program:  C:\Program Files\BackTrackarr\backtrackarr.exe
  - Settings: C:\ProgramData\BackTrackarr\backtrackarr.env   (edit, then restart the task)
  - Data:     C:\ProgramData\BackTrackarr\data
  It runs at startup as a hidden scheduled task (no extra software needed)
  and restarts itself if it stops.
#>
param(
  [string]$Version = "latest",
  [int]$Port = 8787
)
$ErrorActionPreference = "Stop"
$Repo = "DrSeussFreak/BackTrackarr"
$ProgramDir = Join-Path $env:ProgramFiles "BackTrackarr"
$Home_ = Join-Path $env:ProgramData "BackTrackarr"
$TaskName = "BackTrackarr"

$admin = ([Security.Principal.WindowsPrincipal][Security.Principal.WindowsIdentity]::GetCurrent()).IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)
if (-not $admin) { throw "Run this in PowerShell as Administrator." }

$arch = if ($env:PROCESSOR_ARCHITECTURE -eq "ARM64") { "arm64" } else { "amd64" }
$name = "backtrackarr_windows_$arch"

# Use the files next to this script when run from an extracted release zip.
$here = if ($PSScriptRoot) { $PSScriptRoot } else { "" }
if ($here -and (Test-Path (Join-Path $here "backtrackarr.exe"))) {
  $src = $here
} else {
  $url = if ($Version -eq "latest") { "https://github.com/$Repo/releases/latest/download/$name.zip" }
         else { "https://github.com/$Repo/releases/download/$Version/$name.zip" }
  $tmp = Join-Path $env:TEMP "backtrackarr-install"
  Remove-Item $tmp -Recurse -Force -ErrorAction SilentlyContinue
  New-Item -ItemType Directory $tmp | Out-Null
  Write-Host "Downloading $url"
  [Net.ServicePointManager]::SecurityProtocol = [Net.SecurityProtocolType]::Tls12
  Invoke-WebRequest $url -OutFile (Join-Path $tmp "bt.zip") -UseBasicParsing
  Expand-Archive (Join-Path $tmp "bt.zip") -DestinationPath $tmp -Force
  $src = Join-Path $tmp $name
}

# Stop a running copy before replacing the program.
if (Get-ScheduledTask -TaskName $TaskName -ErrorAction SilentlyContinue) {
  Stop-ScheduledTask -TaskName $TaskName -ErrorAction SilentlyContinue
}
Get-Process backtrackarr -ErrorAction SilentlyContinue | Stop-Process -Force -ErrorAction SilentlyContinue
Start-Sleep -Seconds 1

New-Item -ItemType Directory -Force $ProgramDir, $Home_, (Join-Path $Home_ "data") | Out-Null
Copy-Item (Join-Path $src "backtrackarr.exe") $ProgramDir -Force
foreach ($f in "README.md", "LICENSE", "uninstall.ps1") {
  $p = Join-Path $src $f
  if (Test-Path $p) { Copy-Item $p $ProgramDir -Force }
}

$envFile = Join-Path $Home_ "backtrackarr.env"
$first = -not (Test-Path $envFile)
if ($first) {
  $example = Get-Content (Join-Path $src "backtrackarr.env.example")
  $example += ""
  $example += "# Windows paths (set by the installer)"
  $example += "DATA_DIR=$(Join-Path $Home_ 'data')"
  $example += "LISTEN=:$Port"
  Set-Content -Path $envFile -Value $example -Encoding UTF8
}

# Scheduled task: at startup, as SYSTEM, restart on failure, no time limit.
$action = New-ScheduledTaskAction -Execute (Join-Path $ProgramDir "backtrackarr.exe") -WorkingDirectory $Home_
$trigger = New-ScheduledTaskTrigger -AtStartup
$settings = New-ScheduledTaskSettingsSet -AllowStartIfOnBatteries -DontStopIfGoingOnBatteries `
  -RestartCount 999 -RestartInterval (New-TimeSpan -Minutes 1) -ExecutionTimeLimit ([TimeSpan]::Zero) -StartWhenAvailable
$principal = New-ScheduledTaskPrincipal -UserId "SYSTEM" -LogonType ServiceAccount -RunLevel Highest
Register-ScheduledTask -TaskName $TaskName -Action $action -Trigger $trigger -Settings $settings -Principal $principal `
  -Description "BackTrackarr watch history and scrobbler" -Force | Out-Null

# Allow other devices on private networks (e.g. your Plex server) to reach it.
if (-not (Get-NetFirewallRule -DisplayName "BackTrackarr" -ErrorAction SilentlyContinue)) {
  New-NetFirewallRule -DisplayName "BackTrackarr" -Direction Inbound -Protocol TCP -LocalPort $Port -Profile Private,Domain -Action Allow | Out-Null
}

Start-ScheduledTask -TaskName $TaskName
Write-Host ""
Write-Host "BackTrackarr is running: http://localhost:$Port"
Write-Host "Settings: $envFile"
Write-Host "Data:     $(Join-Path $Home_ 'data')"
if ($first) {
  Write-Host "Edit the settings (at least ADMIN_PASSWORD and PLEX_URL/PLEX_TOKEN), then restart:"
  Write-Host "  Stop-ScheduledTask BackTrackarr; Start-ScheduledTask BackTrackarr"
}
