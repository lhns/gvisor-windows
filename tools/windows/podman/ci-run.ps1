# Runs as the standard CI user: starts the engine in runsc and the acceptance test against it.
param([Parameter(Mandatory)][string]$Root, [string]$StorageDriver = 'vfs')
$ErrorActionPreference = 'Continue'
$repo = (Resolve-Path "$PSScriptRoot\..\..\..").Path
$work = "$Root\std"; New-Item -ItemType Directory -Force $work, "$work\share" | Out-Null
$podman = (Get-ChildItem "$Root\tools" -Recurse -Filter podman.exe | Select-Object -First 1).FullName
$docker = (Get-ChildItem "$Root\tools" -Recurse -Filter docker.exe | Select-Object -First 1).FullName
$env:DOCKER_CONFIG = "$Root	oolsdockercfg"
& $podman --version; & $docker --version $podman --version; & $docker --version
$ctl = "$work\engine-ctl.ps1"
@"
param([string]`$Action = 'start')
& '$repo\tools\windows\podman\start-engine.ps1' -Action `$Action -Runsc '$Root\bin\runsc.exe' -Work '$work\engine' -Rootfs '$Root\rootfs' -Share '$work\share' -Name ci -StorageDriver $StorageDriver -Podman '$podman'
"@ | Set-Content $ctl
& $ctl -Action start
if ($LASTEXITCODE -ne 0 -and -not (Test-Path "$work\engine\runsc.pid")) { Write-Host 'engine did not start' }
& "$repo\test\e2e\podman-acceptance\run.ps1" -Podman $podman -Docker $docker -Pipe podman-ci -ShareDir "$work\share" `
  -EngineCtl $ctl -TtyDrive "$Root\bin\ttydrive.exe" -ResultsDir "$work\results"
$rc = $LASTEXITCODE
& $ctl -Action stop
exit $rc
