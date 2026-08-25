[CmdletBinding()]
param([Parameter(Mandatory = $true)][string]$CtrGoPath)

$ErrorActionPreference = 'Stop'
$ctrGo = (Resolve-Path -LiteralPath $CtrGoPath).Path
& $ctrGo status
& $ctrGo service status
