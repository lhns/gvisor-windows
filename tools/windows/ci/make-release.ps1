<#
.SYNOPSIS
  Packs runsc.exe and a hello/busybox OCI bundle with a README into a folder (the CI "release" artifact).
.PARAMETER Guests  Folder of the runsc-guests artifact (hello, busybox, ...).
#>
param([Parameter(Mandatory)][string]$Guests, [Parameter(Mandatory)][string]$Runsc, [Parameter(Mandatory)][string]$Out)
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
@'
# runsc.exe for Windows, built by CI from this commit

Run the bundled guest (a static glibc program that prints a line and exits with 7):

    runsc.exe --network=none run -bundle bundle hello

To get a shell in the same root folder: edit `bundle\config.json`, set `process.args` to
`["/bin/sh"]` and `process.terminal` to true, and run the same command.
Needs 64-bit Windows 10 or later and no administrator rights. CPUs without FSGSBASE or XSAVE use a slower rewrite mode automatically; the Podman acceptance passes in both modes.
'@ | Set-Content -Encoding ascii "$Out\README.md"
