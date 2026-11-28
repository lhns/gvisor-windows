<#
.SYNOPSIS
  Podman acceptance test, Windows variant: the stock Windows podman client and docker CLI
  talk to the engine over \\.\pipe\podman-<name>. Same ten steps as run.sh (steps.tsv),
  STRICT mode: -p from Windows at 127.0.0.1, compose service names, a Windows folder bind
  mounted at /w, restart persistence.

  Output per step:  STEP <n> PASS|FAIL <name> <detail>; exit code = number of failed steps.
  A failed step never stops the run.

.PARAMETER Podman     podman.exe (podman remote, from the release zip)
.PARAMETER Docker     docker.exe (static zip; the compose plugin must be in its cli-plugins)
.PARAMETER Pipe       pipe name, e.g. podman-ci
.PARAMETER ShareDir   Windows folder for the bind-mount test (the engine must see it, see ADR 0018)
.PARAMETER EngineCtl  script taking -Action restart; must return when the engine answers again
.PARAMETER TtyDrive   ttydrive.exe (runs a command in a ConPTY and types into it)
#>
param(
  [Parameter(Mandatory)][string]$Podman,
  [Parameter(Mandatory)][string]$Docker,
  [Parameter(Mandatory)][string]$Pipe,
  [Parameter(Mandatory)][string]$ShareDir,
  [string]$PodmanUrl,                            # default npipe://; the stock podman client needs a unix: URL (ADR 0018)
  [string]$EngineCtl,
  [string]$TtyDrive,
  [string]$ResultsDir = '.\results',
  [string]$ImgPrefix = 'mirror.gcr.io/library',
  [string]$ProbeUrl = 'http://127.0.0.1:8080',
  [int]$StepTimeout = 900,
  [int[]]$Only = (1..10)
)
$ErrorActionPreference = 'Continue'
$url = "npipe:////./pipe/$Pipe"
$env:DOCKER_HOST = "npipe:////./pipe/$Pipe"
if ($PodmanUrl) { $url = $PodmanUrl }
$env:CONTAINER_HOST = $url
$ALPINE = "$ImgPrefix/alpine:3.20"; $DEBIAN = "$ImgPrefix/debian:12-slim"; $NGINX = "$ImgPrefix/nginx:alpine"
New-Item -ItemType Directory -Force $ResultsDir, $ShareDir | Out-Null
$ResultsDir = (Resolve-Path $ResultsDir).Path
$script:Failed = 0
$stepsFile = Join-Path $ResultsDir 'steps.txt'
'' | Set-Content $stepsFile

function Report([int]$n, [string]$name, [bool]$ok, [string]$detail) {
  $st = if ($ok) { 'PASS' } else { $script:Failed++; 'FAIL' }
  $secs = [int]((Get-Date) - $script:stepStart).TotalSeconds
  $line = "STEP $n $st $name [${secs}s] $detail"
  Write-Host $line
  Add-Content $stepsFile $line
}
function LastLines([string]$file) {
  if (-not (Test-Path $file)) { return '' }
  $l = Get-Content $file | Where-Object { $_.Trim() } | Select-Object -Last 3
  (($l -join '|') -replace '\x1b\[[0-9;?]*[A-Za-z]', '')[0..299] -join ''
}
# Runs a native command with a timeout; stdout and stderr are appended to $log, stdout is kept in $script:Out.
function Run([string]$exe, [string[]]$argv, [string]$log, [int]$timeout = $StepTimeout) {
  $o = New-TemporaryFile; $e = New-TemporaryFile
  $p = Start-Process -FilePath $exe -ArgumentList ($argv | ForEach-Object { if ($_ -match '[\s"]') { '"' + ($_ -replace '"', '\"') + '"' } else { $_ } }) `
    -NoNewWindow -PassThru -RedirectStandardOutput $o -RedirectStandardError $e
  # Windows PowerShell 5.1 reports no ExitCode unless the handle was opened before the process exits,
  # and .NET Framework has no Kill(bool).
  $null = $p.Handle
  if (-not $p.WaitForExit($timeout * 1000)) { if ($PSVersionTable.PSVersion.Major -ge 7) { $p.Kill($true) } else { $p.Kill() }; Add-Content $log "TIMEOUT after $timeout s: $exe $argv"; $code = 124 } else { $code = $p.ExitCode }
  $script:Out = (Get-Content $o -Raw); if ($null -eq $script:Out) { $script:Out = '' }
  Add-Content $log "> $exe $argv"; Add-Content $log $script:Out; Add-Content $log (Get-Content $e -Raw)
  Remove-Item $o, $e -Force
  $code
}
function Pm([string[]]$argv, [string]$log, [int]$timeout = $StepTimeout) { Run $Podman (@('--url', $url) + $argv) $log $timeout }
$tmpl = { param($n) Join-Path $ResultsDir "step-$n.log" }

function Step1 { $L = & $tmpl 1; '' | Set-Content $L; $ok = $true
  if ((Pm @('version') $L) -ne 0) { $ok = $false }
  if ((Pm @('info') $L) -ne 0) { $ok = $false }
  $t = Get-Content $L -Raw
  if ($t -notmatch '(?i)Server' -or $t -notmatch '(?i)graphDriverName|store:') { $ok = $false }
  Report 1 'engine-reachable' $ok (LastLines $L) }
function Step2 { $L = & $tmpl 2; '' | Set-Content $L
  $ok = ((Pm @('pull', '-q', $ALPINE) $L) -eq 0) -and ((Pm @('pull', '-q', $DEBIAN) $L) -eq 0)
  Report 2 'pull' $ok (LastLines $L) }
function Step3 { $L = & $tmpl 3; '' | Set-Content $L; $ok = $true; $d = ''
  Pm @('run', '--rm', $ALPINE, 'echo', 'ok') $L | Out-Null
  if ($script:Out.Trim() -ne 'ok') { $ok = $false; $d = "echo got '$($script:Out.Trim())'; " }
  $c = Pm @('run', '--rm', $ALPINE, 'sh', '-c', 'exit 42') $L
  if ($c -ne 42) { $ok = $false; $d += "exit code $c != 42" }
  Report 3 'run-exit-code' $ok $(if ($d) { $d } else { 'ok' }) }
function Step4 { $L = & $tmpl 4; '' | Set-Content $L; $ok = $true; $d = ''
  if (-not $TtyDrive) { Report 4 'interactive-tty' $false 'no ttydrive'; return }
  $inner = 'trap "echo GOT_INT; exit 7" INT; echo TTY=$(tty); read l; echo LINE=$l; while :; do sleep 0.2; done'
  Run $TtyDrive @('-timeout', '150s', '--', $Podman, '--url', $url, 'run', '-it', '--rm', $ALPINE, 'sh', '-c', $inner) $L 200 | Out-Null
  $t = Get-Content $L -Raw
  if ($t -notmatch 'TTY=/dev/pts') { $ok = $false; $d = 'no pty; ' }
  if ($t -notmatch 'LINE=hello') { $ok = $false; $d += 'input not delivered; ' }
  if ($t -notmatch 'GOT_INT') { $ok = $false; $d += 'SIGINT not delivered' }
  Report 4 'interactive-tty' $ok $(if ($d) { $d } else { 'ok' }) }
function Step5 { $L = & $tmpl 5; '' | Set-Content $L
  $b = Join-Path $ResultsDir 'build'; New-Item -ItemType Directory -Force $b | Out-Null
  $apt = 'apt-get update && apt-get install -y --no-install-recommends file curl'
  $cf = "FROM $DEBIAN`nRUN $apt`nRUN file /bin/ls && curl --version | head -1`n"
  if ($env:ACC_STEP5_PROBE) {
    # Diagnostics (docs/results/2r-step5-overlay.md): what /dev/null and the mounts look like in the build container, as root and as _apt, and again when apt fails.
    $state = 'id; stat /dev/null; ls -la /dev | head -20; grep -E " /dev| /tmp| / " /proc/self/mountinfo; su -s /bin/sh _apt -c "id; echo x > /dev/null && echo apt-null-ok"'
    $cf = "FROM $DEBIAN`nRUN $state`nRUN ($apt) || { echo PROBE-AFTER-FAILURE; $state; ls -la /tmp /var/cache/apt/archives /var/cache/apt/archives/partial | head -60; for f in /tmp/apt-dpkg-install-*/*; do ls -la `$f; stat -L `$f; done 2>&1 | head -40; exit 1; }`nRUN file /bin/ls && curl --version | head -1`n"
  }
  $cf | Set-Content (Join-Path $b 'Containerfile') -Encoding ascii
  $ok = (Pm @('build', '-t', 'acc-build', $b) $L) -eq 0
  Report 5 'build-apt' $ok (LastLines $L) }
function Step6 { $L = & $tmpl 6; '' | Set-Content $L; $ok = $true; $d = ''
  $w = Join-Path $ShareDir 'proj'
  if (Test-Path $w) { Remove-Item -Recurse -Force $w }
  New-Item -ItemType Directory -Force $w | Out-Null
  [IO.File]::WriteAllText("$w\h.txt", "from-host`n")
  $script6 = 'set -e; test "$(cat /w/h.txt)" = from-host; echo from-ctr >/w/c.txt; echo r >/w/old; mv /w/old /w/new; echo d >/w/del; rm /w/del; mkdir /w/sub; echo x >/w/sub/f; sync'
  # The Windows path goes to the engine as the client sends it (ADR 0018 records what that gives).
  if ((Pm @('run', '--rm', '-v', "${w}:/w", $ALPINE, 'sh', '-c', $script6) $L) -ne 0) { $ok = $false; $d = 'container script failed; ' }
  if ((Test-Path "$w\c.txt") -and ((Get-Content "$w\c.txt" -Raw).Trim() -eq 'from-ctr')) {} else { $ok = $false; $d += 'write not visible; ' }
  if (-not ((Test-Path "$w\new") -and -not (Test-Path "$w\old"))) { $ok = $false; $d += 'rename not visible; ' }
  if (Test-Path "$w\del") { $ok = $false; $d += 'delete not visible; ' }
  if (-not (Test-Path "$w\sub\f")) { $ok = $false; $d += 'mkdir not visible; ' }
  Pm @('run', '-d', '-v', "${w}:/w", $ALPINE, 'sleep', '60') $L | Out-Null
  $cid = $script:Out.Trim()
  if ($cid) {
    Start-Sleep 2; [IO.File]::WriteAllText("$w\late.txt", "late`n"); Start-Sleep 2
    Pm @('exec', $cid, 'cat', '/w/late.txt') $L | Out-Null
    if ($script:Out.Trim() -ne 'late') { $ok = $false; $d += 'host change not visible in ctr; ' }
    Pm @('rm', '-f', '-t', '0', $cid) $L | Out-Null
  } else { $ok = $false; $d += 'could not start long-running ctr; ' }
  Report 6 'bind-mount' $ok $(if ($d) { $d } else { 'ok' }) }
function Step7 { $L = & $tmpl 7; '' | Set-Content $L; $ok = $false
  Pm @('rm', '-f', '-t', '0', 'acc-web') $L | Out-Null
  Pm @('run', '-d', '--name', 'acc-web', '-p', '8080:80', $NGINX) $L | Out-Null
  if ($script:Out.Trim()) {
    for ($i = 0; $i -lt 30 -and -not $ok; $i++) {
      try { $r = Invoke-WebRequest -UseBasicParsing -TimeoutSec 3 $ProbeUrl; if ($r.StatusCode -eq 200) { $ok = $true } } catch { Add-Content $L "$_"; Start-Sleep 1 }
    }
  }
  Pm @('logs', 'acc-web') $L | Out-Null; Pm @('rm', '-f', '-t', '0', 'acc-web') $L | Out-Null
  Report 7 'port-publish' $ok $(if ($ok) { 'ok' } else { LastLines $L }) }
function Step8 { $L = & $tmpl 8; '' | Set-Content $L; $ok = $true
  $d = Join-Path $ResultsDir 'compose'; New-Item -ItemType Directory -Force $d | Out-Null
  @"
services:
  web:
    image: $NGINX
  cli:
    image: $ALPINE
    command: ["sh","-c","for i in 1 2 3 4 5 6 7 8 9 10; do wget -qO- http://web:80/ && exit 0; sleep 1; done; exit 1"]
    depends_on: [web]
"@ | Set-Content (Join-Path $d 'compose.yaml') -Encoding ascii
  if ((Run $Docker @('compose', '-p', 'acc', '-f', "$d\compose.yaml", 'up', '--abort-on-container-exit', '--exit-code-from', 'cli') $L) -ne 0) { $ok = $false }
  if ((Get-Content $L -Raw) -notmatch 'Welcome to nginx') { $ok = $false }
  Run $Docker @('compose', '-p', 'acc', '-f', "$d\compose.yaml", 'down', '-v') $L | Out-Null
  Report 8 'compose' $ok (LastLines $L) }
function Step9 { $L = & $tmpl 9; '' | Set-Content $L; $ok = $true; $d = ''
  if (-not $EngineCtl) { Report 9 'restart-persist' $false 'no EngineCtl'; return }
  Pm @('volume', 'create', 'accvol') $L | Out-Null
  Pm @('run', '--rm', '-v', 'accvol:/v', $ALPINE, 'sh', '-c', 'echo keep >/v/f') $L | Out-Null
  Pm @('images', '-q') $L | Out-Null; $before = @($script:Out -split "`n" | Where-Object { $_.Trim() } | Sort-Object -Unique).Count
  try { & $EngineCtl -Action restart *>> $L } catch { $ok = $false; $d = "restart failed: $_; " }
  Pm @('images', '-q') $L | Out-Null; $after = @($script:Out -split "`n" | Where-Object { $_.Trim() } | Sort-Object -Unique).Count
  if (-not ($after -ge $before -and $before -gt 0)) { $ok = $false; $d += "images before=$before after=$after; " }
  if ((Pm @('image', 'exists', $ALPINE) $L) -ne 0) { $ok = $false; $d += 'alpine gone; ' }
  Pm @('run', '--rm', '-v', 'accvol:/v', $ALPINE, 'cat', '/v/f') $L | Out-Null
  if ($script:Out.Trim() -ne 'keep') { $ok = $false; $d += 'volume data gone; ' }
  Report 9 'restart-persist' $ok $(if ($d) { $d } else { "images=$after volume ok" }) }
function Step10 { $L = & $tmpl 10; '' | Set-Content $L; $ok = $true; $d = ''
  Pm @('rm', '-f', '-t', '0', 'acc-a', 'acc-b') $L | Out-Null
  $a = Pm @('run', '-d', '--name', 'acc-a', $ALPINE, 'sleep', '120') $L
  $b = Pm @('run', '-d', '--name', 'acc-b', $ALPINE, 'sleep', '120') $L
  if ($a -ne 0 -or $b -ne 0) { Report 10 'isolation' $false "start failed: $(LastLines $L)"; return }
  Pm @('exec', 'acc-a', 'sh', '-c', 'echo secret >/tmp/only-a') $L | Out-Null
  function Ex($c, $cmd) { Pm (@('exec', $c) + $cmd) $L | Out-Null; $script:Out.Trim() }
  $pa = Ex 'acc-a' @('readlink', '/proc/self/ns/pid'); $pb = Ex 'acc-b' @('readlink', '/proc/self/ns/pid')
  $ma = Ex 'acc-a' @('readlink', '/proc/self/ns/mnt'); $mb = Ex 'acc-b' @('readlink', '/proc/self/ns/mnt')
  if (-not ($pa -and $pa -ne $pb)) { $ok = $false; $d = "pid ns same/missing ($pa|$pb); " }
  if (-not ($ma -and $ma -ne $mb)) { $ok = $false; $d += "mnt ns same/missing ($ma|$mb); " }
  $n = Ex 'acc-b' @('sh', '-c', 'ls /proc | grep -c "^[0-9]"')
  if (-not $n -or [int]$n -gt 5) { $ok = $false; $d += "acc-b sees $n pids; " }
  if ((Pm @('exec', 'acc-b', 'test', '-e', '/tmp/only-a') $L) -eq 0) { $ok = $false; $d += 'file leaked across mount ns; ' }
  Pm @('rm', '-f', '-t', '0', 'acc-a', 'acc-b') $L | Out-Null
  Report 10 'isolation' $ok $(if ($d) { $d } else { 'ok' }) }

foreach ($n in $Only) { $script:stepStart = Get-Date; try { & "Step$n" } catch { Report $n 'exception' $false "$_" } }
"FAILED_STEPS=$script:Failed" | Tee-Object -Append $stepsFile
exit $script:Failed
