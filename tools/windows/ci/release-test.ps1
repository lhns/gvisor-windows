<#
.SYNOPSIS
  The "clean machine" check of the release zip. Runs as a standard user (run-as-standard-user.ps1): extracts the zip
  into the user's profile, runs the hello bundle and a busybox shell command from the same bundle.
.PARAMETER Zip  the release zip
#>
param([Parameter(Mandatory)][string]$Zip)
$ErrorActionPreference = 'Continue'
$dest = Join-Path $env:USERPROFILE 'gvisor-windows'
function Fail([string]$m) { Write-Host "release-test: FAIL: $m"; $script:failed = $true }
$failed = $false

Write-Host "release-test: user $env:USERNAME, profile $env:USERPROFILE"
Add-Type -AssemblyName System.IO.Compression.FileSystem
[IO.Compression.ZipFile]::ExtractToDirectory($Zip, $dest)
Get-ChildItem $dest | ForEach-Object { "{0,12:N0}  {1}" -f $(if ($_.PSIsContainer) { 0 } else { $_.Length }), $_.Name }
foreach ($f in 'runsc.exe', 'README.md', 'NOTICE', 'LICENSE', 'bundle\config.json') {
  if (-not (Test-Path (Join-Path $dest $f))) { Fail "$f missing from the zip" }
}

Push-Location $dest
# A copy for the shell command before the first run touches the root folder.
Copy-Item -Recurse bundle bundle-sh
& .\runsc.exe --network=none run -bundle bundle hello
if ($LASTEXITCODE -ne 7) { Fail "hello exited with $LASTEXITCODE, want 7" }

$cfg = Get-Content -Raw bundle-sh\config.json | ConvertFrom-Json
$cfg.process.args = @('/bin/sh', '-c', 'echo busybox-ok; ls /bin | cat')
$cfg | ConvertTo-Json -Depth 8 | Set-Content -Encoding ascii bundle-sh\config.json
$out = & .\runsc.exe --network=none run -bundle bundle-sh shell
$code = $LASTEXITCODE
Write-Host "shell output: $out"
if ($code -ne 0 -or -not ($out -contains 'busybox-ok')) { Fail "busybox shell command: exit $code, output above" }
Pop-Location

if ($failed) { exit 1 }
Write-Host 'release-test: OK'
exit 0
