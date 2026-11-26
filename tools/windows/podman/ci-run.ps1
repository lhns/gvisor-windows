# Runs as the standard CI user: starts the engine in runsc and the acceptance test against it.
param([Parameter(Mandatory)][string]$Root, [string]$StorageDriver = 'vfs')
$ErrorActionPreference = 'Continue'
$repo = (Resolve-Path "$PSScriptRoot\..\..\..").Path
$work = "$Root\std"; New-Item -ItemType Directory -Force $work, "$work\share" | Out-Null
$podman = (Get-ChildItem "$Root\tools" -Recurse -Filter podman.exe | Select-Object -First 1).FullName
$docker = (Get-ChildItem "$Root\tools" -Recurse -Filter docker.exe | Select-Object -First 1).FullName
$env:DOCKER_CONFIG = "$Root\tools\dockercfg"
& $podman --version; & $docker --version
$ctl = "$work\engine-ctl.ps1"
@"
param([string]`$Action = 'start')
& '$repo\tools\windows\podman\start-engine.ps1' -Action `$Action -Runsc '$Root\bin\runsc.exe' -Work '$work\engine' -Rootfs '$Root\rootfs' -Share '$work\share' -Name ci -StorageDriver $StorageDriver -StaticOnTmpfs -Podman '$podman'
"@ | Set-Content $ctl
try { & $ctl -Action start } catch { Write-Host "engine start failed: $_" }
if ($LASTEXITCODE -ne 0 -and -not (Test-Path "$work\engine\runsc.pid")) { Write-Host 'engine did not start' }
$sock = (Get-Content "$work\engine\sock.path" -ErrorAction SilentlyContinue)
$purl = 'unix://' + ($sock -replace '^[A-Za-z]:', '' -replace '\\', '/')
& "$repo\test\e2e\podman-acceptance\run.ps1" -Podman $podman -Docker $docker -PodmanUrl $purl -Pipe podman-ci -ShareDir "$work\share" `
  -EngineCtl $ctl -TtyDrive "$Root\bin\ttydrive.exe" -ResultsDir "$work\results"
$rc = $LASTEXITCODE
try { & $ctl -Action stop } catch { }
exit $rc
