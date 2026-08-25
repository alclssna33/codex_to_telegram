[CmdletBinding()]
param(
    [Parameter(Mandatory = $true)]
    [string]$CtrGoPath,
    [string]$Phase0ReportPath
)

$ErrorActionPreference = 'Stop'

function Test-CtrGoPhase0FinalDecision {
    param([Parameter(Mandatory = $true)][string]$Text)

    # Contract v1: a four-line, checksum-bound comment footer is the complete
    # final decision. Its SHA-256 covers all normalized report bytes before it.
    $normalized = $Text -replace "`r`n", "`n"
    if ($normalized.EndsWith("`n")) { $normalized = $normalized.Substring(0, $normalized.Length - 1) }
    $lines = $normalized -split "`n"
    if ($lines.Count -lt 5) { return $false }
    $footerStart = $lines.Count - 4
    if ($lines[$footerStart] -cne '<!-- ctr-go-phase0-final-decision:v1' -or
        $lines[$footerStart + 1] -cne 'decision=PASS' -or
        $lines[$footerStart + 3] -cne '-->') { return $false }

    $body = [string]::Join("`n", [string[]]$lines[0..($footerStart - 1)]) + "`n"
    $sha256 = [Security.Cryptography.SHA256]::Create()
    try {
        $digest = -join ($sha256.ComputeHash([Text.Encoding]::UTF8.GetBytes($body)) | ForEach-Object { $_.ToString('x2') })
    } finally {
        $sha256.Dispose()
    }
    return $lines[$footerStart + 2] -ceq "content-sha256=$digest"
}

function Get-CtrGoDoctorProfile {
    param([Parameter(Mandatory = $true)]$Doctor)

    $profile = ''
    if ($Doctor.PSObject.Properties.Name -contains 'profile') {
        $profile = [string]$Doctor.profile
    }
    if ([string]::IsNullOrWhiteSpace($profile) -and $null -ne $Doctor.config -and $Doctor.config.PSObject.Properties.Name -contains 'profile') {
        $profile = [string]$Doctor.config.profile
    }
    $profile = $profile.Trim().ToLowerInvariant()
    if ($profile -ne 'minimal' -and $profile -ne 'notifier') {
        throw 'ctr-go doctor must report profile as minimal or notifier before Windows installation.'
    }
    return $profile
}

function Get-CtrGoDoctorAllowedUserIDs {
    param([Parameter(Mandatory = $true)]$Doctor)

    if ($null -ne $Doctor.config -and $Doctor.config.PSObject.Properties.Name -contains 'allowed_user_ids') {
        return @($Doctor.config.allowed_user_ids)
    }
    if ($Doctor.PSObject.Properties.Name -contains 'allowed_user_ids') {
        return @($Doctor.allowed_user_ids)
    }
    return @()
}

function Get-CtrGoDoctorCodexBin {
    param([Parameter(Mandatory = $true)]$Doctor)

    if ($null -ne $Doctor.config -and $Doctor.config.PSObject.Properties.Name -contains 'codex_bin' -and
        -not [string]::IsNullOrWhiteSpace([string]$Doctor.config.codex_bin)) {
        return [string]$Doctor.config.codex_bin
    }
    if ($Doctor.PSObject.Properties.Name -contains 'codex_bin' -and -not [string]::IsNullOrWhiteSpace([string]$Doctor.codex_bin)) {
        return [string]$Doctor.codex_bin
    }
    return 'codex'
}

$ctrGo = (Resolve-Path -LiteralPath $CtrGoPath).Path
if (-not [IO.Path]::IsPathRooted($ctrGo)) { throw 'ctr-go path must be absolute.' }

& $ctrGo version | Out-Null

$doctor = (& $ctrGo doctor | Out-String | ConvertFrom-Json)
$profile = Get-CtrGoDoctorProfile $doctor
$codexBin = Get-CtrGoDoctorCodexBin $doctor
& $codexBin --version | Out-Null

if ($profile -eq 'notifier') {
    $allowedUserIDs = @(Get-CtrGoDoctorAllowedUserIDs $doctor)
    if ($allowedUserIDs.Count -ne 1) {
        throw 'Notifier installation requires exactly one Telegram owner.'
    }
    if (-not $doctor.credential_status.telegram_configured) {
        throw 'Store the Telegram credential with ctr-go secrets set before notifier installation.'
    }
} elseif ($profile -eq 'minimal') {
    & ffmpeg -version | Out-Null

    if (-not $doctor.credential_status.telegram_configured -or -not $doctor.credential_status.openai_configured) {
        throw 'Store both current-user credentials with ctr-go secrets set before installation.'
    }

    if ([string]::IsNullOrWhiteSpace($Phase0ReportPath)) {
        $Phase0ReportPath = Join-Path $PSScriptRoot '..\docs\validation\windows-phase0.md'
    }

    $utf8 = New-Object System.Text.UTF8Encoding($false, $true)
    try {
        $phase0 = $utf8.GetString([IO.File]::ReadAllBytes($Phase0ReportPath))
    } catch {
        throw 'The required final Windows Phase 0 report is not valid UTF-8.'
    }
    if (-not (Test-CtrGoPhase0FinalDecision $phase0)) {
	    throw 'The required final Windows Phase 0 report is not PASS.'
    }
}

# service install repeats runtime validation (profile contract, Credential
# Manager, DPAPI, Codex CLI, and Telegram getMe) immediately before registration.
$installArgs = @('service', 'install', '--ctr-go-bin', $ctrGo)
if ($profile -eq 'minimal') {
    $installArgs += @('--phase0-report', $Phase0ReportPath)
}
& $ctrGo @installArgs
