[CmdletBinding()]
param(
    [Parameter(Mandatory = $true)]
    [string]$BridgePath
)

$ErrorActionPreference = 'Stop'
$watchdog = Join-Path $PSScriptRoot 'ensure-codex-telegram-bridge.ps1'

function Invoke-WatchdogDryRun {
    param([Parameter(Mandatory = $true)][string]$Path)

    $json = & $watchdog -BridgePath $Path -DryRun
    return $json | ConvertFrom-Json
}

$missing = Invoke-WatchdogDryRun -Path (Join-Path $env:TEMP 'missing-ctr-go.exe')
if ($missing.action -ne 'would_start' -or $missing.process_count -ne 0) {
    throw 'Expected a missing bridge to report would_start.'
}

$running = Invoke-WatchdogDryRun -Path $BridgePath
if ($running.action -ne 'already_running' -or $running.process_count -lt 1) {
    throw 'Expected the running bridge to report already_running.'
}

Write-Output 'watchdog behavior tests passed'
