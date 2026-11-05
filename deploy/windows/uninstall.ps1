<#
  Remove BackTrackarr (run as Administrator). Your settings and data in
  C:\ProgramData\BackTrackarr are kept unless you pass -Purge.
#>
param([switch]$Purge)
$ErrorActionPreference = "Continue"
Stop-ScheduledTask -TaskName "BackTrackarr" -ErrorAction SilentlyContinue
Unregister-ScheduledTask -TaskName "BackTrackarr" -Confirm:$false -ErrorAction SilentlyContinue
Get-Process backtrackarr -ErrorAction SilentlyContinue | Stop-Process -Force
Remove-NetFirewallRule -DisplayName "BackTrackarr" -ErrorAction SilentlyContinue
Remove-Item (Join-Path $env:ProgramFiles "BackTrackarr") -Recurse -Force -ErrorAction SilentlyContinue
if ($Purge) {
  Remove-Item (Join-Path $env:ProgramData "BackTrackarr") -Recurse -Force -ErrorAction SilentlyContinue
  Write-Host "Removed BackTrackarr and all its data."
} else {
  Write-Host "Removed BackTrackarr. Settings and data kept in $(Join-Path $env:ProgramData 'BackTrackarr')."
}
