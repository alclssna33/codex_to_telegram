[CmdletBinding()]
param([Parameter(Mandatory = $true)][string]$CtrGoPath)

$ErrorActionPreference = 'Stop'
$ctrGo = (Resolve-Path -LiteralPath $CtrGoPath).Path
$answer = Read-Host 'Remove the scheduled task and Codex Telegram Bridge application state? Type YES'
if ($answer -ne 'YES') {
    throw 'Uninstall cancelled. Credentials and Codex auth/session/project files were not changed.'
}

# Credentials deliberately require the separate explicit `ctr-go secrets delete`
# command; this command only removes bridge-owned task/application state.
& $ctrGo service uninstall --yes
