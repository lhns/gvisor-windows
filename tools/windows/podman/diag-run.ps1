# Diagnostics: start the engine (with RUNSC_EXTRA_FLAGS, e.g. --strace), run a container and a build.
param([Parameter(Mandatory)][string]$Root)
$ErrorActionPreference = 'Continue'
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
"=== run" | Tee-Object -Append $log
P run --rm mirror.gcr.io/library/alpine:3.20 echo ok
"=== build with RUN (pasta)" | Tee-Object -Append $log
$cf = "$work\build"; New-Item -ItemType Directory -Force $cf | Out-Null
"FROM mirror.gcr.io/library/alpine:3.20`nRUN echo built" | Set-Content "$cf\Containerfile" -Encoding ascii
P build -t diag-build $cf
& "$repo\tools\windows\podman\start-engine.ps1" -Action stop -Runsc "$Root\bin\runsc.exe" -Work "$work\engine" -Name ci
