<#
.SYNOPSIS
  Packs runsc.exe, a hello/busybox OCI bundle, the Podman engine image and its scripts into a folder and a zip (the CI "release" artifact).
.PARAMETER Guests  Folder of the runsc-guests artifact (hello, busybox, ...).
.PARAMETER Image   podman.erofs of the Podman engine.
.PARAMETER Zip     The zip to write, gvisor-windows-<sha>.zip.
#>
param([Parameter(Mandatory)][string]$Guests, [Parameter(Mandatory)][string]$Runsc, [Parameter(Mandatory)][string]$Image,
  [Parameter(Mandatory)][string]$Out, [Parameter(Mandatory)][string]$Zip)
$ErrorActionPreference = 'Stop'
$r = "$Out\bundle\rootfs"
foreach ($d in 'bin', 'etc', 'proc', 'dev', 'tmp') { New-Item -ItemType Directory -Force "$r\$d" | Out-Null }
Copy-Item $Runsc "$Out\runsc.exe"
Copy-Item "$Guests\hello" "$r\bin\hello"
foreach ($a in 'busybox', 'sh', 'ls', 'cat', 'echo') { Copy-Item "$Guests\busybox" "$r\bin\$a" }
[IO.File]::WriteAllText("$r\etc\hostname", "runsc-win`n")
$spec = [ordered]@{
  ociVersion = '1.0.2'
  process = [ordered]@{ terminal = $false; user = @{ uid = 0; gid = 0 }; args = @('/bin/hello'); env = @('PATH=/bin', 'HOME=/', 'TERM=xterm'); cwd = '/' }
  root = @{ path = 'rootfs'; readonly = $false }
  hostname = 'runsc-win'
  mounts = @(
    @{ destination = '/proc'; type = 'proc'; source = 'proc' },
    @{ destination = '/dev'; type = 'tmpfs'; source = 'tmpfs'; options = @('nosuid', 'strictatime', 'mode=755', 'size=65536k') },
    @{ destination = '/tmp'; type = 'tmpfs'; source = 'tmpfs' })
  annotations = @{ 'dev.gvisor.windows.rootfs-options' = 'file-mode=0755' }
  linux = [ordered]@{
    namespaces = @(@{ type = 'pid' }, @{ type = 'network' }, @{ type = 'ipc' }, @{ type = 'uts' }, @{ type = 'mount' })
    devices = @(
      @{ path = '/dev/null'; type = 'c'; major = 1; minor = 3; fileMode = 438 },
      @{ path = '/dev/zero'; type = 'c'; major = 1; minor = 5; fileMode = 438 },
      @{ path = '/dev/urandom'; type = 'c'; major = 1; minor = 9; fileMode = 438 })
  }
}
$spec | ConvertTo-Json -Depth 8 | Set-Content -Encoding ascii "$Out\bundle\config.json"
# The Podman engine: erofs image, start-engine.ps1 (lib) and the end-user wrapper, README of the zip.
$pm = Join-Path $PSScriptRoot '..\podman'
New-Item -ItemType Directory -Force "$Out\lib" | Out-Null
Copy-Item $Image "$Out\podman.erofs"
Copy-Item "$pm\start-engine.ps1" "$Out\lib\start-engine.ps1"
Copy-Item "$pm\podman-engine.ps1" "$Out\podman-engine.ps1"
Copy-Item "$pm\README-release.md" "$Out\README.md"
# The zip (Compress-Archive stops at 2 GB). Keep -Zip outside -Out.
Add-Type -AssemblyName System.IO.Compression.FileSystem
$z = $ExecutionContext.SessionState.Path.GetUnresolvedProviderPathFromPSPath($Zip)
if (Test-Path $z) { Remove-Item $z }
[IO.Compression.ZipFile]::CreateFromDirectory((Resolve-Path $Out).Path, $z, [IO.Compression.CompressionLevel]::Optimal, $false)
Get-Item $z | ForEach-Object { "{0}: {1:N1} MB" -f $_.Name, ($_.Length / 1MB) }
