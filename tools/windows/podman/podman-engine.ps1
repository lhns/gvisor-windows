<#
.SYNOPSIS
  Starts, stops or shows the Podman engine that runs inside runsc.exe (release zip, end users).

.DESCRIPTION
  Wraps lib\start-engine.ps1 (the harness script of the Podman acceptance) with end-user defaults:
  engine state and image storage under %LOCALAPPDATA%\gvisor-windows, pipe \\.\pipe\podman-gvisor,
  no diagnostics. Needs no administrator rights; works in Windows PowerShell 5.1 and PowerShell 7.

.PARAMETER Action   start | stop | status (default status)
.PARAMETER Data     Folder of the engine state and storage (images, containers, volumes, logs).
.PARAMETER Share    Windows folder the engine may bind-mount from (as /mnt/<drive>/<path>); none by default.
.PARAMETER Podman   podman.exe (the remote client); with it, start waits until the engine answers.
.PARAMETER StorageDriver  overlay (default, the Sentry's overlay) or vfs. A storage folder keeps the driver it was made with: delete it to change.
#>
[CmdletBinding()]
param(
  [ValidateSet('start', 'stop', 'status')][string]$Action = 'status',
  [string]$Data = (Join-Path $env:LOCALAPPDATA 'gvisor-windows'),
  [string]$Share,
  [string]$Podman,
  [ValidateSet('overlay', 'vfs')][string]$StorageDriver = 'overlay',
  [int]$TimeoutSeconds = 300
)
$ErrorActionPreference = 'Stop'
$root = Split-Path -Parent $MyInvocation.MyCommand.Path
$engine = Join-Path $Data 'engine'
$pidFile = Join-Path $engine 'runsc.pid'
$pipe = 'podman-gvisor'

function Get-EnginePid {
  if (-not (Test-Path $pidFile)) { return $null }
  $p = Get-Process -Id ([int](Get-Content $pidFile)) -ErrorAction SilentlyContinue
  if ($p -and $p.ProcessName -eq 'runsc') { $p.Id } else { $null }
}

function Show-Connect {
  $sockFile = Join-Path $engine 'sock.path'
  if (Test-Path $sockFile) {
    # The podman client dials a unix socket (a drive letter does not survive its url parsing).
    $url = 'unix://' + ((Get-Content $sockFile) -replace '^[A-Za-z]:', '' -replace '\\', '/')
    Write-Host "  podman:  podman system connection add --default gvisor $url"
  }
  Write-Host "  docker:  `$env:DOCKER_HOST = 'npipe:////./pipe/$pipe'"
  Write-Host "  data:    $Data"
}

switch ($Action) {
  'start' {
    if (Get-EnginePid) { Write-Host "podman-engine: already running (pid $(Get-EnginePid))"; Show-Connect; return }
    $a = @{ Action = 'start'; Quiet = $true; Runsc = (Join-Path $root 'runsc.exe'); Work = $engine; Image = (Join-Path $root 'podman.erofs')
      Name = 'gvisor'; TimeoutSeconds = $TimeoutSeconds; StorageDriver = $StorageDriver }
    if ($Share) { $a.Share = $Share }
    if (-not $Podman) { $c = Get-Command podman.exe -ErrorAction SilentlyContinue; if ($c) { $Podman = $c.Source } }
    if ($Podman) { $a.Podman = $Podman }
    & (Join-Path $root 'lib\start-engine.ps1') @a
    if (-not $Podman) { Write-Host 'podman-engine: started; the engine answers in about a minute (no podman.exe given to wait for it)' }
    Write-Host 'podman-engine: connect with'
    Show-Connect
  }
  'stop' {
    if (-not (Get-EnginePid)) { Write-Host 'podman-engine: not running'; return }
    & (Join-Path $root 'lib\start-engine.ps1') -Action stop -Runsc (Join-Path $root 'runsc.exe') -Work $engine -Name gvisor
    Write-Host 'podman-engine: stopped'
  }
  'status' {
    $id = Get-EnginePid
    if ($id) { Write-Host "podman-engine: running (pid $id), pipe \\.\pipe\$pipe"; Show-Connect } else { Write-Host 'podman-engine: not running'; exit 1 }
  }
}
