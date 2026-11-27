# Runs as the standard CI user: starts the engine in runsc and the acceptance test against it.
param([Parameter(Mandatory)][string]$Root, [string]$StorageDriver = 'vfs', [string]$Only = 'all', [string]$FsMode = 'default')
$ErrorActionPreference = 'Continue'
# Engine settings of the platform under test (ADR 0020): rewrite mode also keeps AVX-512 visible.
if ($FsMode -eq 'native') { $env:WINSTUB_FSMODE = 'native' }
if ($FsMode -eq 'goavx512off') { $env:ENGINE_GODEBUG = 'cpu.avx512=off' }
if ($FsMode -eq 'rewrite') { $env:WINSTUB_FSMODE = 'rewrite'; $env:WINSTUB_KEEP_AVX512 = '1' }
# State of the platform (goroutines, stub threads, debug-event counts) 20 s after the start, while the engine is still coming up.
New-Item -ItemType Directory -Force "$Root\std\engine\logs" | Out-Null
$env:RUNSC_HANG_DUMP = "$Root\std\engine\logs\state-20s.txt:20"
"fsmode=$FsMode WINSTUB_FSMODE=$env:WINSTUB_FSMODE WINSTUB_KEEP_AVX512=$env:WINSTUB_KEEP_AVX512"
$acceptStart = Get-Date
$repo = (Resolve-Path "$PSScriptRoot\..\..\..").Path
$work = "$Root\std"; New-Item -ItemType Directory -Force $work, "$work\share" | Out-Null
$podman = (Get-ChildItem "$Root\tools" -Recurse -Filter podman.exe | Select-Object -First 1).FullName
$docker = (Get-ChildItem "$Root\tools" -Recurse -Filter docker.exe | Select-Object -First 1).FullName
$env:DOCKER_CONFIG = "$Root\tools\dockercfg"
& $podman --version; & $docker --version
# Probe: does a child of the ConPTY of ttydrive see consoles on its standard handles?
& "$Root\bin\ttydrive.exe" -hello-after 600s -timeout 60s -- 'C:\Program Files\PowerShell\7\pwsh.exe' -NoProfile -Command '"redirected in/out/err: " + [Console]::IsInputRedirected + "," + [Console]::IsOutputRedirected + "," + [Console]::IsErrorRedirected; exit 0' | Tee-Object -Append "$work\tty-probe.log"
$ctl = "$work\engine-ctl.ps1"
@"
param([string]`$Action = 'start')
& '$repo\tools\windows\podman\start-engine.ps1' -Action `$Action -Runsc '$Root\bin\runsc.exe' -Work '$work\engine' -Image '$Root\podman.erofs' -Share '$work\share' -Name ci -StorageDriver $StorageDriver -Podman '$podman'
"@ | Set-Content $ctl
try { & $ctl -Action start } catch { Write-Host "engine start failed: $_" }
if ($LASTEXITCODE -ne 0 -and -not (Test-Path "$work\engine\runsc.pid")) { Write-Host 'engine did not start' }
# Which network backend and firewall driver netavark will use (needed for -p), recorded in the log.
$steps = if ($Only -and $Only -ne 'all') { [int[]]($Only -split "[, ]+" | Where-Object { $_ }) } else { 1..10 }
$sock = (Get-Content "$work\engine\sock.path" -ErrorAction SilentlyContinue)
$purl = 'unix://' + ($sock -replace '^[A-Za-z]:', '' -replace '\\', '/')
& $podman --url $purl info --format '{{.Host.NetworkBackend}} {{.Host.NetworkBackendInfo.Package}} {{.Version.Version}} rootless={{.Host.Security.Rootless}} cgroup={{.Host.CgroupManager}}'
& "$repo\test\e2e\podman-acceptance\run.ps1" -Podman $podman -Docker $docker -PodmanUrl $purl -Pipe podman-ci -ShareDir "$work\share" `
  -Only $steps -EngineCtl $ctl -TtyDrive "$Root\bin\ttydrive.exe" -ResultsDir "$work\results"
$rc = $LASTEXITCODE
"acceptance seconds: $([int]((Get-Date) - $acceptStart).TotalSeconds)"
try { & $ctl -Action stop } catch { }
exit $rc
