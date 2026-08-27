[CmdletBinding()]
param(
    [Parameter(Mandatory = $true)]
    [ValidateNotNullOrEmpty()]
    [string]$BridgePath,
    [switch]$DryRun
)

$ErrorActionPreference = 'Stop'
$resolvedBridgePath = [IO.Path]::GetFullPath($BridgePath)

function Get-RunningBridge {
    Get-CimInstance Win32_Process |
        Where-Object { $_.ExecutablePath -eq $resolvedBridgePath }
}

$running = @(Get-RunningBridge)
if ($running.Count -gt 0) {
    if ($DryRun) {
        [pscustomobject]@{
            action = 'already_running'
            process_count = $running.Count
        } | ConvertTo-Json -Compress
    }
    exit 0
}

if ($DryRun) {
    [pscustomobject]@{
        action = 'would_start'
        process_count = 0
    } | ConvertTo-Json -Compress
    exit 0
}

if (-not (Test-Path -LiteralPath $resolvedBridgePath -PathType Leaf)) {
    throw "Bridge executable was not found: $resolvedBridgePath"
}

$process = Start-Process -FilePath $resolvedBridgePath -ArgumentList 'daemon' -WindowStyle Hidden -PassThru
Start-Sleep -Seconds 2
if ($process.HasExited -or @(Get-RunningBridge).Count -eq 0) {
    throw 'Bridge process exited before it became ready.'
}
