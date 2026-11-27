# Runs as the standard CI user: starts the engine in runsc and the acceptance test against it.
param([Parameter(Mandatory)][string]$Root, [string]$StorageDriver = 'overlay', [string]$Only = 'all', [string]$FsMode = 'default')
$ErrorActionPreference = 'Continue'
# The storage driver is overlay (ADR 0021); a "-vfs" suffix on a mode (native-vfs, rewrite-vfs) selects vfs. A "-overlay" suffix (native-overlay)
# is still accepted.
if ($FsMode -like '*-vfs') { $FsMode = $FsMode -replace '-vfs$', ''; $StorageDriver = 'vfs' }
if ($FsMode -like '*-overlay') { $FsMode = $FsMode -replace '-overlay$', ''; $StorageDriver = 'overlay' }
# Engine settings of the platform under test (ADR 0020): rewrite mode also keeps AVX-512 visible.
if ($FsMode -eq 'native') { $env:WINSTUB_FSMODE = 'native' }
if ($FsMode -eq 'native-replace') { $env:WINSTUB_FSMODE = 'native'; $env:WINSTUB_REPLACE_STALLED = '1' }
# Step 5 diagnostics (docs/results/2r-step5-overlay.md): native mode plus a probe in the build (state of /dev/null and the mounts when apt fails);
# native-diag adds the runsc strace of the file system calls of the whole sandbox (large logs).
if ($FsMode -eq 'native-probe' -or $FsMode -eq 'native-diag') { $env:WINSTUB_FSMODE = 'native'; $env:ACC_STEP5_PROBE = '1' }
if ($FsMode -eq 'native-diag') { $env:RUNSC_EXTRA_FLAGS = '--strace --strace-syscalls=mknod,mknodat,open,openat,chmod,fchmod,fchmodat,setuid,setresuid,setgid,setresgid,setgroups,execve,mount,umount2,unshare,setns,pivot_root,chroot,rename,renameat,renameat2,symlink,symlinkat,link,linkat,unlink,unlinkat,rmdir,mkdir,mkdirat,stat,lstat,newfstatat,statx,access,faccessat,faccessat2,readlink,readlinkat,getdents64' }
if ($FsMode -eq 'goavx512off') { $env:ENGINE_GODEBUG = 'cpu.avx512=off' }
if ($FsMode -eq 'rewrite') { $env:WINSTUB_FSMODE = 'rewrite'; $env:WINSTUB_KEEP_AVX512 = '1' }
# Rewrite mode as on a CPU without XSAVE (the development VM): fxsave, and no XSAVE or AVX for the guest.
if ($FsMode -eq 'rewrite-fxsave') { $env:WINSTUB_FSMODE = 'rewrite'; $env:WINSTUB_FPMODE = 'fxsave' }
# State of the platform (goroutines, stub threads, debug-event counts) 20 s after the start, while the engine is still coming up.
New-Item -ItemType Directory -Force "$Root\std\engine\logs" | Out-Null
$env:RUNSC_HANG_DUMP = "$Root\std\engine\logs\state-20s.txt:20"
"fsmode=$FsMode WINSTUB_FSMODE=$env:WINSTUB_FSMODE WINSTUB_KEEP_AVX512=$env:WINSTUB_KEEP_AVX512 WINSTUB_FPMODE=$env:WINSTUB_FPMODE"
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
# Storage driver and c/storage's native-diff probe result (docs/results/2s-overlay-default.md).
$graph = & $podman --url $purl info --format '{{.Store.GraphDriverName}}: {{.Store.GraphStatus}}'
"graph: $graph"
$nativeDiffFail = $StorageDriver -eq 'overlay' -and "$graph" -notmatch 'Native Overlay Diff:\s*true'
if ($nativeDiffFail) { Write-Host 'native overlay diff is not true' }
& "$repo\test\e2e\podman-acceptance\run.ps1" -Podman $podman -Docker $docker -PodmanUrl $purl -Pipe podman-ci -ShareDir "$work\share" `
  -Only $steps -EngineCtl $ctl -TtyDrive "$Root\bin\ttydrive.exe" -ResultsDir "$work\results"
$rc = $LASTEXITCODE
if ($rc -eq 0 -and $nativeDiffFail) { $rc = 1 }
"acceptance seconds: $([int]((Get-Date) - $acceptStart).TotalSeconds)"
try { & $ctl -Action stop } catch { }
exit $rc
