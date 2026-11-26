<#
.SYNOPSIS
  Runs a program as a freshly created standard (non-admin) local user on a
  Windows CI runner, streams its output and exits with its exit code.

.DESCRIPTION
  Creates a local user in the Users group only (random password, removed at
  the end), grants it read and execute on -GrantPath folders and modify on
  -WritablePath folders, starts the program with CreateProcessWithLogonW (the
  mechanism of Start-Process -Credential) and relays stdout and stderr.
  Needs an elevated runner session (GitHub's windows-latest is one).

  Build or download things as the runner's admin first, then run only the
  finished program here: the standard user cannot see the runner's Go cache
  or toolchain.

.EXAMPLE
  go test -c -o C:\ci\bin\net.test.exe ./pkg/tcpip/winnat
  pwsh tools/windows/ci/run-as-standard-user.ps1 -GrantPath C:\ci\bin `
    -Environment @{ GVISOR_ADMIN_PROBE = 'denied' } `
    -Program C:\ci\bin\net.test.exe -Arguments '-test.v','-test.count=1'

.PARAMETER Program      Executable to run (absolute path).
.PARAMETER Arguments    Its arguments.
.PARAMETER WorkingDirectory  Defaults to the program's folder.
.PARAMETER GrantPath    Folders the user may read and execute.
.PARAMETER WritablePath Folders the user may also write to.
.PARAMETER Environment  Extra environment variables for the program.
.PARAMETER TimeoutSeconds  The program is killed after this long (exit 124).
.PARAMETER UserName     Name of the temporary user.
#>
[CmdletBinding()]
param(
  [Parameter(Mandatory)][string]$Program,
  [string[]]$Arguments = @(),
  [string]$WorkingDirectory,
  [string[]]$GrantPath = @(),
  [string[]]$WritablePath = @(),
  [hashtable]$Environment = @{},
  [int]$TimeoutSeconds = 600,
  [string]$UserName = 'ci-std'
)
$ErrorActionPreference = 'Stop'

$Program = (Resolve-Path -LiteralPath $Program).Path
if (-not $WorkingDirectory) { $WorkingDirectory = Split-Path -Parent $Program }
$WorkingDirectory = (Resolve-Path -LiteralPath $WorkingDirectory).Path

function New-RandomPassword {
  $chars = 'abcdefghijkmnpqrstuvwxyzABCDEFGHJKLMNPQRSTUVWXYZ23456789!#%+='.ToCharArray()
  $bytes = New-Object byte[] 24
  [System.Security.Cryptography.RandomNumberGenerator]::Create().GetBytes($bytes)
  # One of each class first, so the password policy is met.
  'aA3!' + (-join ($bytes | ForEach-Object { $chars[$_ % $chars.Length] }))
}

function Grant([string]$path, [string]$rights) {
  # (OI)(CI): inherited by files and folders below.
  & icacls $path /grant "${UserName}:(OI)(CI)$rights" /T /C /Q | Out-Null
  if ($LASTEXITCODE -ne 0) { throw "icacls failed on $path" }
}

$plain = New-RandomPassword
$secure = ConvertTo-SecureString $plain -AsPlainText -Force
if (Get-LocalUser -Name $UserName -ErrorAction SilentlyContinue) { Remove-LocalUser -Name $UserName }
New-LocalUser -Name $UserName -Password $secure -PasswordNeverExpires -AccountNeverExpires `
  -Description 'temporary standard user for CI' | Out-Null
$exit = 1
try {
  # New-LocalUser adds no group; Users is what an installed standard user has.
  Add-LocalGroupMember -Group 'Users' -Member $UserName
  $admins = Get-LocalGroupMember -Group 'Administrators' | ForEach-Object { $_.Name }
  if ($admins -match "\\$([regex]::Escape($UserName))$") { throw "$UserName is an administrator" }

  Grant $Program 'RX'
  foreach ($p in $GrantPath) { Grant $p 'RX' }
  foreach ($p in $WritablePath) { Grant $p 'M' }
  Grant $WorkingDirectory 'RX'

  $psi = New-Object System.Diagnostics.ProcessStartInfo
  $psi.FileName = $Program
  $psi.Arguments = ($Arguments | ForEach-Object {
      if ($_ -match '[\s"]') { '"' + ($_ -replace '"', '\"') + '"' } else { $_ }
    }) -join ' '
  $psi.WorkingDirectory = $WorkingDirectory
  $psi.UseShellExecute = $false
  $psi.RedirectStandardOutput = $true
  $psi.RedirectStandardError = $true
  $psi.UserName = $UserName
  $psi.Domain = $env:COMPUTERNAME
  $psi.Password = $secure
  $psi.LoadUserProfile = $true
  foreach ($k in $Environment.Keys) { $psi.Environment[$k] = [string]$Environment[$k] }

  Write-Host "run-as-standard-user: $UserName runs $Program $($psi.Arguments)"
  $proc = New-Object System.Diagnostics.Process
  $proc.StartInfo = $psi
  # Relay output as it arrives (queued: event handlers run on other threads).
  $q = [System.Collections.Concurrent.ConcurrentQueue[string]]::new()
  $onData = { if ($null -ne $EventArgs.Data) { $Event.MessageData.Enqueue($EventArgs.Data) } }
  $e1 = Register-ObjectEvent $proc OutputDataReceived -Action $onData -MessageData $q
  $e2 = Register-ObjectEvent $proc ErrorDataReceived -Action $onData -MessageData $q
  $drain = { $l = $null; while ($q.TryDequeue([ref]$l)) { Write-Host $l } }
  [void]$proc.Start()
  $proc.BeginOutputReadLine()
  $proc.BeginErrorReadLine()
  $deadline = [DateTime]::UtcNow.AddSeconds($TimeoutSeconds)
  while (-not $proc.HasExited) {
    & $drain
    if ([DateTime]::UtcNow -gt $deadline) {
      $proc.Kill($true)
      Write-Host "run-as-standard-user: timed out after $TimeoutSeconds s"
      $exit = 124
      break
    }
    Start-Sleep -Milliseconds 200
  }
  if ($exit -ne 124) { $proc.WaitForExit(); $exit = $proc.ExitCode }
  Start-Sleep -Milliseconds 500
  & $drain
  Unregister-Event $e1.Name -ErrorAction SilentlyContinue
  Unregister-Event $e2.Name -ErrorAction SilentlyContinue
  Write-Host "run-as-standard-user: exit code $exit"
}
finally {
  # The user's profile folder is left to the runner; it is thrown away anyway.
  Remove-LocalUser -Name $UserName -ErrorAction SilentlyContinue
}
exit $exit
