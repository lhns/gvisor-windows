# Diagnostics: start the engine (with RUNSC_EXTRA_FLAGS, e.g. --strace) and try one container.
param([Parameter(Mandatory)][string]$Root)
$ErrorActionPreference = 'Continue'
$env:RUNSC_EXTRA_FLAGS = '--strace'
$repo = (Resolve-Path "$PSScriptRoot\..\..\..").Path
$work = "$Root\std"; New-Item -ItemType Directory -Force $work, "$work\results" | Out-Null
$podman = (Get-ChildItem "$Root\tools" -Recurse -Filter podman.exe | Select-Object -First 1).FullName
$log = "$work\results\diag.log"
function P { & $podman @args 2>&1 | Tee-Object -Append $log }
try {
  & "$repo\tools\windows\podman\start-engine.ps1" -Action start -Runsc "$Root\bin\runsc.exe" -Work "$work\engine" -Image "$Root\podman.erofs" -Name ci -Podman $podman
} catch { "engine start failed: $_" | Tee-Object -Append $log }
$sock = Get-Content "$work\engine\sock.path"
$env:CONTAINER_HOST = 'unix://' + ($sock -replace '^[A-Za-z]:', '' -replace '\\', '/')
"url $env:CONTAINER_HOST sock '$sock'" | Tee-Object -Append $log
P pull mirror.gcr.io/library/alpine:3.20
"=== run" | Tee-Object -Append $log
P --log-level=debug run --rm mirror.gcr.io/library/alpine:3.20 echo ok
"=== create (no network, no log driver)" | Tee-Object -Append $log
P --log-level=debug create --name d1 --network=none --log-driver=none mirror.gcr.io/library/alpine:3.20 echo ok
P --log-level=debug start d1
P ps --all
"=== run with the k8s-file log in /tmp and --cgroups=disabled" | Tee-Object -Append $log
P --log-level=debug run --rm --network=none --cgroups=disabled mirror.gcr.io/library/alpine:3.20 echo ok2
& "$repo\tools\windows\podman\start-engine.ps1" -Action stop -Runsc "$Root\bin\runsc.exe" -Work "$work\engine" -Name ci
