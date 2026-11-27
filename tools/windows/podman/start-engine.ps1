<#
.SYNOPSIS
  Starts, stops or restarts Podman inside a runsc.exe sandbox and exposes its API as
  \\.\pipe\podman-<Name> (current user only). No admin rights needed at run time.

.DESCRIPTION
  runsc.exe run (ADR 0017) with:
   - the root: a Windows folder unpacked from the podman image (-Rootfs), or an erofs
     image (-Image, once erofs roots boot);
   - /var/lib/containers on a Windows folder (-Storage, ADR 0007), so images and volumes
     survive a restart;
   - -Share mounted at /mnt/<drive>/<path> (WSL style) so the engine sees the client's
     Windows folder (ADR 0018);
   - `podman system service` on guest TCP port 8888, relayed to the pipe by runsc
     (--pipe-forward, ADR 0018); --network=windows-nat.

.PARAMETER Action   start | stop | restart (default start)
#>
[CmdletBinding()]
param(
  [ValidateSet('start', 'stop', 'restart')][string]$Action = 'start',
  [Parameter(Mandatory)][string]$Runsc,
  [Parameter(Mandatory)][string]$Work,           # state folder: bundle, storage, logs, pid
  [string]$Rootfs,
  [string]$Image,
  [string]$Name = 'ci',
  [string]$Share,                                 # Windows folder to expose to the engine
  [string]$StorageDriver = 'vfs',
  [switch]$StaticOnTmpfs,
  [string]$Podman,                                # podman.exe, to wait for the engine
  [int]$TimeoutSeconds = 300
)
$ErrorActionPreference = 'Stop'
$here = Split-Path -Parent $MyInvocation.MyCommand.Path
$Work = (New-Item -ItemType Directory -Force $Work).FullName
$pidFile = Join-Path $Work 'runsc.pid'
$pipe = "podman-$Name"

function Stop-Engine {
  if (Test-Path $pidFile) {
    $p = Get-Process -Id ([int](Get-Content $pidFile)) -ErrorAction SilentlyContinue
    if ($p) { $p.Kill($true); $p.WaitForExit(30000) | Out-Null }
    Remove-Item $pidFile -Force
  }
}

# The podman client's unix: URL for a socket on the current drive (a drive letter does not survive url parsing).
function UnixUrl([string]$p) { 'unix://' + ($p -replace '^[A-Za-z]:', '' -replace '\\', '/') }

function Guest-Path([string]$win) {
  # C:\a\b -> /mnt/c/a/b, as podman machine and WSL do.
  $full = (Resolve-Path -LiteralPath $win).Path
  '/mnt/' + $full.Substring(0, 1).ToLower() + ($full.Substring(2) -replace '\\', '/')
}

function Start-Engine {
  if (-not $Rootfs -and -not $Image) { throw 'give -Rootfs or -Image' }
  $storage = (New-Item -ItemType Directory -Force (Join-Path $Work 'storage')).FullName
  $logs = (New-Item -ItemType Directory -Force (Join-Path $Work 'logs')).FullName
  $root = if ($Image) { (Resolve-Path $Image).Path } else { (Resolve-Path $Rootfs).Path }
  if ($Rootfs) {
    # Engine settings for a sandbox without systemd or cgroup controllers (same files as the Linux harness).
    $etc = New-Item -ItemType Directory -Force (Join-Path $root 'etc\containers')
    Copy-Item (Join-Path $here '..\..\..\test\e2e\podman-acceptance\harness\containers.conf') $etc\containers.conf -Force
    (Get-Content (Join-Path $here '..\..\..\test\e2e\podman-acceptance\harness\storage.conf')) -replace '^driver = .*', "driver = `"$StorageDriver`"" |
      Set-Content $etc\storage.conf -Encoding ascii
    # Gap (docs/results/5-podman-windows.md): sqlite on the host-folder mount fails with "disk I/O error:
    # permission denied"; the database goes to a tmpfs until that is fixed (so it does not survive a restart).
    if ($StaticOnTmpfs) { (Get-Content $etc\containers.conf) -replace '^\[engine\]', "[engine]`nstatic_dir = `"/run/podman-static`"" | Set-Content $etc\containers.conf -Encoding ascii }
    # The Sentry writes no resolv.conf: the virtual DNS of windows-nat is 10.0.2.3 (ADR 0014).
    [IO.File]::WriteAllText((Join-Path $root 'etc\resolv.conf'), "nameserver 10.0.2.3`n")
    foreach ($d in 'run', 'tmp', 'var\lib\containers', 'mnt') { New-Item -ItemType Directory -Force (Join-Path $root $d) | Out-Null }
  }

  $mounts = @(
    @{ destination = '/proc'; type = 'proc'; source = 'proc' },
    @{ destination = '/dev'; type = 'tmpfs'; source = 'tmpfs'; options = @('nosuid', 'mode=755', 'size=65536k') },
    @{ destination = '/dev/pts'; type = 'devpts'; source = 'devpts'; options = @('nosuid', 'noexec', 'newinstance', 'ptmxmode=0666', 'mode=0620') },
    @{ destination = '/dev/shm'; type = 'tmpfs'; source = 'shm'; options = @('nosuid', 'noexec', 'nodev', 'mode=1777', 'size=65536k') },
    @{ destination = '/sys'; type = 'sysfs'; source = 'sysfs'; options = @('nosuid', 'noexec', 'nodev', 'ro') },
    @{ destination = '/sys/fs/cgroup'; type = 'cgroup2'; source = 'cgroup2'; options = @('nosuid', 'noexec', 'nodev') },
    @{ destination = '/run'; type = 'tmpfs'; source = 'tmpfs'; options = @('mode=755') },
    @{ destination = '/tmp'; type = 'tmpfs'; source = 'tmpfs'; options = @('mode=1777') },
    # buildah mounts an overlay with its upperdir in /var/tmp; the Sentry accepts only tmpfs or host-folder uppers for user mounts, not the root overlay.
    @{ destination = '/var/tmp'; type = 'tmpfs'; source = 'tmpfs'; options = @('mode=1777') },
    @{ destination = '/var/lib/containers'; type = 'bind'; source = $storage; options = @('rbind', 'rw', 'case-sensitive') }
  )
  if ($Share) {
    $mounts += @{ destination = (Guest-Path $Share); type = 'bind'; source = (Resolve-Path $Share).Path; options = @('rbind', 'rw', 'case-sensitive') }
  }
  $caps = 'CAP_CHOWN', 'CAP_DAC_OVERRIDE', 'CAP_DAC_READ_SEARCH', 'CAP_FOWNER', 'CAP_FSETID', 'CAP_KILL', 'CAP_SETGID', 'CAP_SETUID', 'CAP_SETPCAP',
  'CAP_LINUX_IMMUTABLE', 'CAP_NET_BIND_SERVICE', 'CAP_NET_BROADCAST', 'CAP_NET_ADMIN', 'CAP_NET_RAW', 'CAP_IPC_LOCK', 'CAP_IPC_OWNER',
  'CAP_SYS_MODULE', 'CAP_SYS_RAWIO', 'CAP_SYS_CHROOT', 'CAP_SYS_PTRACE', 'CAP_SYS_PACCT', 'CAP_SYS_ADMIN', 'CAP_SYS_BOOT', 'CAP_SYS_NICE',
  'CAP_SYS_RESOURCE', 'CAP_SYS_TIME', 'CAP_SYS_TTY_CONFIG', 'CAP_MKNOD', 'CAP_LEASE', 'CAP_AUDIT_WRITE', 'CAP_AUDIT_CONTROL', 'CAP_SETFCAP',
  'CAP_MAC_OVERRIDE', 'CAP_MAC_ADMIN', 'CAP_SYSLOG', 'CAP_WAKE_ALARM', 'CAP_BLOCK_SUSPEND', 'CAP_AUDIT_READ'
  $spec = [ordered]@{
    ociVersion = '1.0.2'
    process    = [ordered]@{
      terminal     = $false
      user         = @{ uid = 0; gid = 0 }
      args         = @('sh', '-c', 'mkdir -p /run/podman /run/containers/storage; exec podman --log-level=info system service --time=0 tcp://0.0.0.0:8888')
      env          = @('PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin', 'HOME=/root', 'TERM=xterm', 'container=podman', $(if ($env:ENGINE_GODEBUG) { "GODEBUG=$env:ENGINE_GODEBUG" } else { 'CONTAINER_NOGODEBUG=1' }),
        'CONTAINERS_STORAGE_CONF=/etc/containers/storage.conf', 'CONTAINERS_CONF=/etc/containers/containers.conf',
        # Without it a build chooses buildah's rootless isolation (pasta networking, which fails as root: it drops
        # its capabilities before setns, on real Linux too, docs/results/0c-linux-strict-bar.md); the OCI isolation uses netavark.
        'BUILDAH_ISOLATION=oci')
      cwd          = '/'
      capabilities = @{ bounding = $caps; effective = $caps; permitted = $caps; inheritable = $caps }
    }
    root       = @{ path = $root; readonly = $false }
    hostname   = "podman-$Name"
    mounts     = $mounts
    linux      = [ordered]@{
      namespaces = @(@{ type = 'pid' }, @{ type = 'network' }, @{ type = 'ipc' }, @{ type = 'uts' }, @{ type = 'mount' })
      # The /dev tmpfs of the spec hides the Sentry's own; runc would create these nodes.
      devices    = @(
        @{ path = '/dev/null'; type = 'c'; major = 1; minor = 3; fileMode = 438; uid = 0; gid = 0 },
        @{ path = '/dev/zero'; type = 'c'; major = 1; minor = 5; fileMode = 438; uid = 0; gid = 0 },
        @{ path = '/dev/full'; type = 'c'; major = 1; minor = 7; fileMode = 438; uid = 0; gid = 0 },
        @{ path = '/dev/random'; type = 'c'; major = 1; minor = 8; fileMode = 438; uid = 0; gid = 0 },
        @{ path = '/dev/urandom'; type = 'c'; major = 1; minor = 9; fileMode = 438; uid = 0; gid = 0 },
        @{ path = '/dev/tty'; type = 'c'; major = 5; minor = 0; fileMode = 438; uid = 0; gid = 0 })
    }
    annotations = @{ 'dev.gvisor.windows.rootfs-options' = 'file-mode=0755,dir-mode=0755' }
  }
  $bundle = (New-Item -ItemType Directory -Force (Join-Path $Work 'bundle')).FullName
  $spec | ConvertTo-Json -Depth 8 | Set-Content (Join-Path $bundle 'config.json') -Encoding ascii

  # The socket for the podman client (it cannot dial pipes) lives in a folder only this user can enter.
  $sockDir = (New-Item -ItemType Directory -Force (Join-Path $Work 'sock')).FullName
  & icacls $sockDir /inheritance:r /grant:r "$($env:USERNAME):(OI)(CI)F" /Q | Out-Null
  $sock = Join-Path $sockDir 'podman.sock'
  $sock | Set-Content (Join-Path $Work 'sock.path')
  $args = @('--debug', "--debug-log=$logs\", '--network=windows-nat', '--overlay2=root:memory', '--in-sandbox-cgroup=v2', "--pipe-forward=8888:$pipe", "--unix-forward=8888:$sock") + @($env:RUNSC_EXTRA_FLAGS -split ' ' | Where-Object { $_ }) + @('run', '-bundle', $bundle, "podman-$Name")
  $stamp = Get-Date -Format "HHmmss"
  # A sandbox still alive after 6 minutes (an acceptance takes about 5) writes the stacks of its goroutines and the
  # winstub state (hang hunt, runsc/winrunsc); the file is uploaded with the logs.
  $env:RUNSC_HANG_DUMP = (Join-Path $logs "hang-$stamp.txt") + ":360"
  $p = Start-Process -FilePath $Runsc -ArgumentList $args -PassThru -WindowStyle Hidden `
    -RedirectStandardOutput (Join-Path $logs "engine-$stamp.out") -RedirectStandardError (Join-Path $logs "engine-$stamp.err")
  $p.Id | Set-Content $pidFile
  Write-Host "start-engine: runsc pid $($p.Id), pipe \\.\pipe\$pipe"

  if ($Podman) {
    $deadline = (Get-Date).AddSeconds($TimeoutSeconds)
    while ((Get-Date) -lt $deadline) {
      if ($p.HasExited) { throw "runsc exited with $($p.ExitCode); see $logs" }
      & $Podman --url (UnixUrl $sock) version *> $null
      if ($LASTEXITCODE -eq 0) { Write-Host 'start-engine: engine answers'; return }
      Start-Sleep -Seconds 3
    }
    throw "engine did not answer within $TimeoutSeconds s; see $logs"
  }
}

switch ($Action) {
  'start' { Start-Engine }
  'stop' { Stop-Engine }
  'restart' { Stop-Engine; Start-Sleep -Seconds 2; Start-Engine }
}
