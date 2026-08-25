[CmdletBinding()]
param()

$ErrorActionPreference = 'Stop'

function New-Phase0DecisionReport {
    param(
        [Parameter(Mandatory = $true)][string]$Body,
        [string]$Decision = 'PASS'
    )

    if (-not $Body.EndsWith("`n")) { $Body += "`n" }
    $sha256 = [Security.Cryptography.SHA256]::Create()
    try {
        $digest = -join ($sha256.ComputeHash([Text.Encoding]::UTF8.GetBytes($Body)) | ForEach-Object { $_.ToString('x2') })
    } finally {
        $sha256.Dispose()
    }
    return $Body + "<!-- ctr-go-phase0-final-decision:v1`n" +
        "decision=$Decision`n" +
        "content-sha256=$digest`n-->"
}

function Assert-InstallerRejects {
    param([Parameter(Mandatory = $true)][string]$Path)

    $rejected = $false
    try {
        & (Join-Path $PSScriptRoot 'install-windows.ps1') -CtrGoPath $ctrGo -Phase0ReportPath $Path
    } catch {
        $rejected = $_.Exception.Message -match 'Phase 0'
    }
    if (-not $rejected) { throw "installer accepted invalid Phase 0 report: $Path" }
}

function Assert-TextContains {
    param(
        [Parameter(Mandatory = $true)][string]$Text,
        [Parameter(Mandatory = $true)][string]$Needle,
        [Parameter(Mandatory = $true)][string]$Message
    )

    if (-not $Text.Contains($Needle)) { throw $Message }
}

function Assert-TextNotContains {
    param(
        [Parameter(Mandatory = $true)][string]$Text,
        [Parameter(Mandatory = $true)][string]$Needle,
        [Parameter(Mandatory = $true)][string]$Message
    )

    if ($Text.Contains($Needle)) { throw $Message }
}

$notifierRoot = Join-Path ([IO.Path]::GetTempPath()) ("ctr-go-install-notifier-test-" + [Guid]::NewGuid().ToString('N'))
New-Item -ItemType Directory -Path $notifierRoot | Out-Null
try {
    $calls = Join-Path $notifierRoot 'calls.log'
    $ctrGo = Join-Path $notifierRoot 'ctr-go.cmd'
    $codex = Join-Path $notifierRoot 'codex.cmd'
    $ffmpeg = Join-Path $notifierRoot 'ffmpeg.cmd'
    $missingPhase0 = Join-Path $notifierRoot 'missing-phase0.md'

    [IO.File]::WriteAllText($ctrGo, @"
@echo off
echo ctr-go %*>>"$calls"
if "%1"=="version" exit /b 0
if "%1"=="doctor" (
  echo {"config":{"profile":"notifier","allowed_user_ids":[42],"codex_bin":"codex"},"credential_status":{"telegram_configured":true,"openai_configured":false}}
  exit /b 0
)
if "%1"=="service" if "%2"=="install" exit /b 0
exit /b 0
"@)
    [IO.File]::WriteAllText($codex, "@echo off`r`necho codex %*>>`"$calls`"`r`nexit /b 0`r`n")
    [IO.File]::WriteAllText($ffmpeg, "@echo off`r`necho ffmpeg %*>>`"$calls`"`r`nexit /b 44`r`n")

    $oldPath = $env:PATH
    $env:PATH = "$notifierRoot;$oldPath"
    try {
        & (Join-Path $PSScriptRoot 'install-windows.ps1') -CtrGoPath $ctrGo -Phase0ReportPath $missingPhase0
        if ($LASTEXITCODE -ne 0) { throw 'notifier installer rejected reduced prerequisites' }
    } finally {
        $env:PATH = $oldPath
    }

    $joined = ''
    if (Test-Path -LiteralPath $calls) { $joined = [IO.File]::ReadAllText($calls) }
    Assert-TextContains $joined 'ctr-go version' 'notifier installer did not check the ctr-go binary'
    Assert-TextContains $joined 'ctr-go doctor' 'notifier installer did not read doctor profile data'
    Assert-TextContains $joined 'codex --version' 'notifier installer did not check Codex CLI'
    Assert-TextContains $joined 'ctr-go service install' 'notifier installer did not delegate final service validation'
    Assert-TextNotContains $joined 'ffmpeg' 'notifier installer invoked obsolete ffmpeg prerequisite'
} finally {
    Remove-Item -LiteralPath $notifierRoot -Recurse -Force
}

$root = Join-Path ([IO.Path]::GetTempPath()) ("ctr-go-install-test-" + [Guid]::NewGuid().ToString('N'))
New-Item -ItemType Directory -Path $root | Out-Null
try {
    $ctrGo = Join-Path $root 'ctr-go.cmd'
    $codex = Join-Path $root 'codex.cmd'
    $ffmpeg = Join-Path $root 'ffmpeg.cmd'
    [IO.File]::WriteAllText($ctrGo, "@echo off`r`nif `"%1`"==`"doctor`" echo {`"config`":{`"profile`":`"minimal`",`"allowed_user_ids`":[42],`"codex_bin`":`"codex`"},`"credential_status`":{`"telegram_configured`":true,`"openai_configured`":true}}`r`nexit /b 0`r`n")
    [IO.File]::WriteAllText($codex, "@echo off`r`nexit /b 0`r`n")
    [IO.File]::WriteAllText($ffmpeg, "@echo off`r`nexit /b 0`r`n")
    $oldPath = $env:PATH
        $env:PATH = "$root;$oldPath"
    try {
        $valid = Join-Path $root 'valid.md'
        [IO.File]::WriteAllText($valid, (New-Phase0DecisionReport "# evidence`n"))
        & (Join-Path $PSScriptRoot 'install-windows.ps1') -CtrGoPath $ctrGo -Phase0ReportPath $valid
        if ($LASTEXITCODE -ne 0) { throw 'installer rejected valid checksum-bound final PASS decision' }

        & (Join-Path $PSScriptRoot 'install-windows.ps1') -CtrGoPath $ctrGo
        if ($LASTEXITCODE -ne 0) { throw 'installer rejected the documented checksum-bound final PASS decision' }

        $validUtf8 = Join-Path $root 'valid-utf8.md'
        [IO.File]::WriteAllText($validUtf8, (New-Phase0DecisionReport "# evidence 증거`n"))
        & (Join-Path $PSScriptRoot 'install-windows.ps1') -CtrGoPath $ctrGo -Phase0ReportPath $validUtf8
        if ($LASTEXITCODE -ne 0) { throw 'installer rejected valid UTF-8 report content' }

        $validBom = Join-Path $root 'valid-bom.md'
        $bomBody = ([string][char]0xfeff) + "# evidence`n"
        [IO.File]::WriteAllText($validBom, (New-Phase0DecisionReport $bomBody))
        & (Join-Path $PSScriptRoot 'install-windows.ps1') -CtrGoPath $ctrGo -Phase0ReportPath $validBom
        if ($LASTEXITCODE -ne 0) { throw 'installer rejected valid BOM-prefixed UTF-8 report content' }

        $missing = Join-Path $root 'missing.md'
        [IO.File]::WriteAllText($missing, "# evidence`n")
        Assert-InstallerRejects $missing

        $malformed = Join-Path $root 'malformed.md'
        [IO.File]::WriteAllText($malformed, "# evidence`n<!-- ctr-go-phase0-final-decision:v1`ndecision=PASS`ncontent-sha256=broken`n-->")
        Assert-InstallerRejects $malformed

        $uppercaseChecksum = Join-Path $root 'uppercase-checksum.md'
        $uppercaseReport = New-Phase0DecisionReport "# evidence`n"
        $uppercaseReport = [regex]::Replace(
            $uppercaseReport,
            '(?m)(?<=^content-sha256=)[0-9a-f]{64}$',
            { param($match) $match.Value.ToUpperInvariant() }
        )
        [IO.File]::WriteAllText($uppercaseChecksum, $uppercaseReport)
        Assert-InstallerRejects $uppercaseChecksum

        $negative = Join-Path $root 'negative.md'
        [IO.File]::WriteAllText($negative, (New-Phase0DecisionReport "# evidence`n" 'FAIL'))
        Assert-InstallerRejects $negative

        $legacy = Join-Path $root 'legacy-marker-in-failure.md'
        [IO.File]::WriteAllText($legacy, "# evidence`n## Later failure`nQuoted historic authorization follows:`n## Final Phase 0 decision`n`nDecision: PASS")
        Assert-InstallerRejects $legacy

        $footer = New-Phase0DecisionReport "# evidence`n"
        $copied = Join-Path $root 'copied-footer.md'
        [IO.File]::WriteAllText($copied, "$footer`n## Later failure`nQuoted authorization follows:`n$footer")
        Assert-InstallerRejects $copied
    } finally {
        $env:PATH = $oldPath
    }
} finally {
    Remove-Item -LiteralPath $root -Recurse -Force
}
