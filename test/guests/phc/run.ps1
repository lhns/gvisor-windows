# Runs probe cases through runsc.exe directly (docs/results/1p-rewrite-heap.md). Each case is
# "name|args|bind": bind=1 mounts a fresh host folder at /w. Prints one line per run and the PROBE lines.
param([string]$Runsc, [string]$Guests, [string]$Work, [string[]]$Cases, [int]$Reps = 3)
$ErrorActionPreference = 'Stop'
New-Item -ItemType Directory -Force $Work | Out-Null
$rootfs = Join-Path $Work 'rootfs'
foreach ($d in 'bin', 'etc', 'proc', 'dev', 'tmp') { New-Item -ItemType Directory -Force (Join-Path $rootfs $d) | Out-Null }
Copy-Item (Join-Path $Guests '*') (Join-Path $rootfs 'bin')
$fail = 0
foreach ($c in $Cases) {
  $name, $args0, $bind = $c.Split('|')
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
      process = @{terminal = $false; user = @{uid = 0; gid = 0}; args = @($args0.Split(' ')); env = @('PATH=/bin', 'HOME=/'); cwd = '/'}
      root = @{path = $rootfs; readonly = $false}
      hostname = 'phc'
      annotations = @{'dev.gvisor.windows.rootfs-options' = 'file-mode=0755'}
      mounts = $mounts
      linux = @{namespaces = @(@{type = 'pid'}, @{type = 'network'}, @{type = 'ipc'}, @{type = 'uts'}, @{type = 'mount'})}}
    $cfg | ConvertTo-Json -Depth 8 | Set-Content -Encoding ascii (Join-Path $b 'config.json')
    $log = Join-Path $Work "$id.log"
    $t0 = Get-Date
    & $Runsc --debug "--debug-log=$log" --network=none run -bundle $b "phc-$id" > (Join-Path $Work "$id.out") 2> (Join-Path $Work "$id.err")
    $code = $LASTEXITCODE
    $secs = [int]((Get-Date) - $t0).TotalSeconds
    $last = (Get-Content (Join-Path $Work "$id.out") -Tail 1) -join ''
    Write-Host ("RESULT {0} exit {1} {2}s: {3}" -f $id, $code, $secs, $last)
    Get-Content (Join-Path $Work "$id.err") | Select-String -Pattern 'PROBE|free\(\)|double free|corrupt|FSFIX|TRIPWIRE|WATCH' | Select-Object -First 80 | ForEach-Object { Write-Host "  $_" }
    if ($code -ne 0) { $fail++ }
  }
}
Write-Host "phc: $fail failed runs"
