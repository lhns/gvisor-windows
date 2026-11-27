# Dot-source, then Set-Leg <native|rewrite|rewrite-fxsave>: the environment of one CI leg
# (ADR 0020: native %fs, forced rewrite mode, rewrite mode without XSAVE).
function Set-Leg([string]$Mode, [switch]$KeepAvx512) {
  $env:WINSTUB_FSMODE = if ($Mode -eq 'native') { 'native' } else { 'rewrite' }
  $env:WINSTUB_FPMODE = if ($Mode -eq 'rewrite-fxsave') { 'fxsave' } else { '' }
  $env:WINSTUB_KEEP_AVX512 = if ($KeepAvx512 -and $Mode -eq 'rewrite') { '1' } else { '' }
}
