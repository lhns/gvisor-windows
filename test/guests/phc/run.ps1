# Runs probe cases through runsc.exe directly (docs/results/1p-rewrite-heap.md). Each case is
# "name|args|bind|env|flags": bind=1 mounts a fresh host folder at /w, env is K=V,K=V for the guest,
# flags extra runsc flags (space separated). A run is killed after -Timeout seconds (the goroutine
# and stub state are dumped to ID.hang shortly before). Prints one line per run and the PROBE lines.
param([string]$Runsc, [string]$Guests, [string]$Work, [string[]]$Cases, [int]$Reps = 3, [int]$Timeout = 240)
$ErrorActionPreference = 'Stop'
New-Item -ItemType Directory -Force $Work | Out-Null
$rootfs = Join-Path $Work 'rootfs'
foreach ($d in 'bin', 'etc', 'proc', 'dev', 'tmp') { New-Item -ItemType Directory -Force (Join-Path $rootfs $d) | Out-Null }
Copy-Item (Join-Path $Guests '*') (Join-Path $rootfs 'bin')
$fail = 0
foreach ($c in $Cases) {
  $f = $c.Split('|')
  $name, $args0, $bind = $f[0], $f[1], $f[2]
  $genv = if ($f.Count -gt 3 -and $f[3]) { $f[3].Split(',') } else { @() }
  $flags = if ($f.Count -gt 4 -and $f[4]) { $f[4].Split(' ') } else { @() }
  for ($i = 1; $i -le $Reps; $i++) {
    $id = "$name-$i"
    $b = Join-Path $Work "b-$id"
    New-Item -ItemType Directory -Force $b | Out-Null
    $mounts = @(
      @{destination = '/proc'; type = 'proc'; source = 'proc'},
      @{destination = '/tmp'; type = 'tmpfs'; source = 'tmpfs'})
    if ($bind -eq '1') {
      $h = Join-Path $Work "h-$id"
      New-Item -ItemType Directory -Force $h | Out-Null
      $mounts += @{destination = '/w'; type = 'bind'; source = $h; options = @('rbind', 'rw')}
    }
    $cfg = @{ociVersion = '1.0.2'
      process = @{terminal = $false; user = @{uid = 0; gid = 0}; args = @($args0.Split(' ')); env = @('PATH=/bin', 'HOME=/') + $genv; cwd = '/'}
      root = @{path = $rootfs; readonly = $false}
      hostname = 'phc'
      annotations = @{'dev.gvisor.windows.rootfs-options' = 'file-mode=0755'}
      mounts = $mounts
      linux = @{namespaces = @(@{type = 'pid'}, @{type = 'network'}, @{type = 'ipc'}, @{type = 'uts'}, @{type = 'mount'})}}
    $cfg | ConvertTo-Json -Depth 8 | Set-Content -Encoding ascii (Join-Path $b 'config.json')
    $log = Join-Path $Work "$id.log"
    $env:RUNSC_HANG_DUMP = (Join-Path $Work "$id.hang") + ':' + ($Timeout - 15)
    $t0 = Get-Date
    $al = @('--debug', "--debug-log=$log") + $flags + @('--network=none', 'run', '-bundle', $b, "phc-$id")
    $p = Start-Process -FilePath $Runsc -ArgumentList $al -NoNewWindow -PassThru `
      -RedirectStandardOutput (Join-Path $Work "$id.out") -RedirectStandardError (Join-Path $Work "$id.err")
    if (-not $p.WaitForExit($Timeout * 1000)) { Stop-Process -Id $p.Id -Force; $code = 'TIMEOUT' } else { $code = $p.ExitCode }
    $secs = [int]((Get-Date) - $t0).TotalSeconds
    $last = (Get-Content (Join-Path $Work "$id.out") -Tail 1) -join ''
    Write-Host ("RESULT {0} exit {1} {2}s: {3}" -f $id, $code, $secs, $last)
    Get-Content (Join-Path $Work "$id.err") | Select-String -Pattern 'PROBE|free\(\)|double free|corrupt|FSFIX|TRIPWIRE|WATCH' | Select-Object -First 80 | ForEach-Object { Write-Host "  $_" }
    if ($code -ne 0) { $fail++ }
  }
}
Write-Host "phc: $fail failed runs"
