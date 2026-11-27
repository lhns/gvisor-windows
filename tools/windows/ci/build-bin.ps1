<#
.SYNOPSIS
  Builds the Windows CI binaries into C:\ci\bin: programs and compiled test
  binaries (<name>.test.exe). Used by the ci workflow's bin job (all of them,
  once per commit) and by a suite that is dispatched alone (just the ones it needs).
#>
param([string]$Targets = 'all', [string]$Out = 'C:\ci\bin')
$ErrorActionPreference = 'Stop'
$env:CGO_ENABLED = '0'
New-Item -ItemType Directory -Force $Out | Out-Null
$progs = [ordered]@{
  runsc = './runsc'; runsctest = './tools/windows/runsctest'; sentryguest = './tools/windows/sentryguest'
  ttydrive = './tools/windows/podman/ttydrive'; sentrysmoke = './tools/windows/sentrysmoke'
}
$tests = [ordered]@{
  winstub = './pkg/sentry/platform/winstub'; winhost = './pkg/sentry/fsimpl/gofer/winhost'
  gofer = './pkg/sentry/fsimpl/gofer'; winnat = './pkg/tcpip/winnat'; rootfs = './pkg/sentry/fsimpl/rootfs'
  safecopy = './pkg/safecopy'; pgalloc = './pkg/sentry/pgalloc'
}
$want = if ($Targets -eq 'all') { @($progs.Keys) + @($tests.Keys) } else { $Targets -split '[, ]+' | Where-Object { $_ } }
foreach ($n in $want) {
  if ($progs.Contains($n)) { go build -p 2 -o "$Out\$n.exe" $progs[$n] }
  elseif ($tests.Contains($n)) { go test -c -p 2 -o "$Out\$n.test.exe" $tests[$n] }
  else { throw "unknown target $n" }
  if ($LASTEXITCODE -ne 0) { throw "build of $n failed" }
}
Get-ChildItem $Out | Select-Object Name, Length | Format-Table -AutoSize | Out-String
