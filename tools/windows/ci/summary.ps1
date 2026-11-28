<#
.SYNOPSIS
  Appends result tables to the job summary ($GITHUB_STEP_SUMMARY), so a green run needs no artifact download.
.PARAMETER Title  Heading of the section.
.PARAMETER Steps  toJSON(steps): a table of step id and outcome (only steps with an id show up).
.PARAMETER Exits  File globs of logs with "===== name" lines each followed by "exit: N": a pass/fail count per file.
.PARAMETER Code   Files shown as code blocks (all lines matching -Grep, or the last -Tail lines).
#>
param([Parameter(Mandatory)][string]$Title, [string]$Steps, [string[]]$Exits = @(), [string[]]$Code = @(), [string]$Grep, [int]$Tail = 60)
$ErrorActionPreference = 'Continue'
$f = $env:GITHUB_STEP_SUMMARY
if (-not $f) { $f = [IO.Path]::GetTempFileName() }
$o = New-Object System.Collections.Generic.List[string]
$o.Add("### $Title"); $o.Add('')
if ($Steps) {
  $o.Add('| step | outcome |'); $o.Add('|--|--|')
  foreach ($p in (ConvertFrom-Json $Steps).PSObject.Properties) { $o.Add("| $($p.Name) | $($p.Value.outcome) |") }
  $o.Add('')
}
foreach ($g in $Exits) {
  foreach ($file in Get-ChildItem $g -ErrorAction SilentlyContinue) {
    $name = ''; $pass = 0; $bad = @()
    foreach ($l in Get-Content $file.FullName) {
      if ($l -match '^===== (.+)$') { $name = $Matches[1] }
      elseif ($l -match '^exit: (-?\d+)') { if ([int]$Matches[1] -eq 0) { $pass++ } else { $bad += $name } }
    }
    $o.Add("- ``$($file.Name)``: $pass passed, $($bad.Count) failed$(if ($bad) { ': ' + ($bad -join ', ') })")
  }
  $o.Add('')
}
foreach ($g in $Code) {
  foreach ($file in Get-ChildItem $g -ErrorAction SilentlyContinue) {
    $lines = @(Get-Content $file.FullName)
    $lines = if ($Grep) { @($lines | Where-Object { $_ -match $Grep }) } else { @($lines | Select-Object -Last $Tail) }
    if (-not $lines) { continue }
    $o.Add("``$($file.Name)``"); $o.Add('```'); $o.AddRange([string[]]$lines); $o.Add('```'); $o.Add('')
  }
}
Add-Content -Path $f -Value $o -Encoding utf8
