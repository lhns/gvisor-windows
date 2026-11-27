# Diagnostics: start the engine (with RUNSC_EXTRA_FLAGS, e.g. --strace), run containers of the debian image.
param([Parameter(Mandatory)][string]$Root)
$ErrorActionPreference = 'Continue'
if ($env:DIAG_ASPOOL) { $env:GVISOR_ASPOOL = $env:DIAG_ASPOOL }
$env:GVISOR_ASPOOL = '0'
$env:RUNSC_EXTRA_FLAGS = if ($env:DIAG_RUNSC_FLAGS) { $env:DIAG_RUNSC_FLAGS } else { '--strace' }
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
P pull mirror.gcr.io/library/debian:12-slim
"=== ld.so of the debian layer from the host folder and from tmpfs" | Tee-Object -Append $log
P create --name d1 mirror.gcr.io/library/debian:12-slim /bin/echo hi
P run --rm -v /run:/r -v /var/lib/containers:/c mirror.gcr.io/library/alpine:3.20 mkdir -p /r/dtest /c/dtest
$sh = (Get-Content "$PSScriptRoot\ldtest.sh" -Raw) -replace "`r", ""
P run --rm -v /var/lib/containers/storage:/s -v /run/dtest:/t -v /var/lib/containers/dtest:/u mirror.gcr.io/library/alpine:3.20 sh -c $sh
"=== start d1" | Tee-Object -Append $log
P start -a d1
& "$repo\tools\windows\podman\start-engine.ps1" -Action stop -Runsc "$Root\bin\runsc.exe" -Work "$work\engine" -Name ci
