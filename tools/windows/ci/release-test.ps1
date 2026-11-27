<#
.SYNOPSIS
  The "clean machine" check of the release zip. Runs as a standard user (run-as-standard-user.ps1): extracts the zip
  into the user's profile, runs the hello bundle, starts the engine with podman-engine.ps1 (as an end user would, in
  Windows PowerShell 5.1) and drives it with the stock podman remote client.
.PARAMETER Zip     gvisor-windows-<sha>.zip
.PARAMETER Podman  podman.exe, the stock remote client
#>
param([Parameter(Mandatory)][string]$Zip, [Parameter(Mandatory)][string]$Podman)
$ErrorActionPreference = 'Continue'
$dest = Join-Path $env:USERPROFILE 'gvisor-windows'
$engineScript = Join-Path $dest 'podman-engine.ps1'
$ps = 'powershell.exe'
function Fail([string]$m) { Write-Host "release-test: FAIL: $m"; $script:failed = $true }
function Engine([string]$a) { & $ps -NoProfile -ExecutionPolicy Bypass -File $engineScript $a -Podman $Podman }
$failed = $false

Write-Host "release-test: user $env:USERNAME, profile $env:USERPROFILE"
Add-Type -AssemblyName System.IO.Compression.FileSystem
[IO.Compression.ZipFile]::ExtractToDirectory($Zip, $dest)
Get-ChildItem $dest | ForEach-Object { "{0,12:N0}  {1}" -f $(if ($_.PSIsContainer) { 0 } else { $_.Length }), $_.Name }

Push-Location $dest
& .\runsc.exe --network=none run -bundle bundle hello
if ($LASTEXITCODE -ne 7) { Fail "hello exited with $LASTEXITCODE, want 7" }
Pop-Location

try {
  if (-not $failed) {
    Engine 'start'
    if ($LASTEXITCODE -ne 0) { Fail "podman-engine start exited with $LASTEXITCODE" }
    Engine 'status'
    if ($LASTEXITCODE -ne 0) { Fail 'status says the engine is not running' }
    $sock = Get-Content (Join-Path $env:LOCALAPPDATA 'gvisor-windows\engine\sock.path')
    $url = 'unix://' + ($sock -replace '^[A-Za-z]:', '' -replace '\', '/')
    & $Podman system connection add --default gvisor $url
    if ($LASTEXITCODE -ne 0) { Fail 'podman system connection add' }
    & $Podman version
    if ($LASTEXITCODE -ne 0) { Fail 'podman version' }
    $out = & $Podman run --rm docker.io/library/alpine:latest echo ok
    Write-Host "podman run output: $out"
    if ($LASTEXITCODE -ne 0 -or ($out | Select-Object -Last 1) -ne 'ok') { Fail 'podman run --rm alpine echo ok' }
  }
}
finally {
  Engine 'stop'
  if (Test-Path (Join-Path $env:LOCALAPPDATA 'gvisor-windows\engine\runsc.pid')) { Fail 'pid file left after stop' }
  Get-ChildItem (Join-Path $env:LOCALAPPDATA 'gvisor-windows\engine\logs') -ErrorAction SilentlyContinue | ForEach-Object { "--- $($_.Name)"; Get-Content $_.FullName -Tail 40 }
}
if ($failed) { exit 1 }
Write-Host 'release-test: OK'
exit 0
