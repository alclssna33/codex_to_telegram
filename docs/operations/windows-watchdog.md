# Windows bridge watchdog

Use this optional current-user scheduled task when a Windows login, battery,
or idle transition can leave the bridge stopped. It checks every five minutes
and starts the bridge only if the exact configured `ctr-go.exe` is not already
running. It never launches a duplicate bridge.

The watchdog is separate from `ctr-go` configuration. Keep your Telegram token,
user ID, `.env`, logs, and SQLite state outside this repository.

## Install

Copy `scripts/ensure-codex-telegram-bridge.ps1` (and, if you want to run the
verification command below, its `.test.ps1` companion) to a stable local folder
next to your built or released `ctr-go.exe`. Then run the following in a normal
PowerShell window, replacing the two paths with your own paths:

```powershell
$bridge = 'C:\CodexBridgeTest\bin\ctr-go.exe'
$watchdog = 'C:\CodexBridgeTest\bin\ensure-codex-telegram-bridge.ps1'
$taskName = 'Codex Telegram Bridge Watchdog'

$action = New-ScheduledTaskAction `
  -Execute "$env:WINDIR\System32\WindowsPowerShell\v1.0\powershell.exe" `
  -Argument "-NoProfile -NonInteractive -ExecutionPolicy Bypass -File `"$watchdog`" -BridgePath `"$bridge`""
$logon = New-ScheduledTaskTrigger -AtLogOn
$timer = New-ScheduledTaskTrigger -Once -At (Get-Date).AddMinutes(1)
$timer.Repetition.Interval = 'PT5M'
$timer.Repetition.Duration = 'P365D'
$settings = New-ScheduledTaskSettingsSet `
  -AllowStartIfOnBatteries -DontStopIfGoingOnBatteries `
  -StartWhenAvailable -MultipleInstances IgnoreNew `
  -ExecutionTimeLimit (New-TimeSpan -Minutes 2)

Register-ScheduledTask -TaskName $taskName -Action $action -Trigger $logon,$timer `
  -Settings $settings -Description 'Keeps the local Codex Telegram notifier bridge running.' -Force
Start-ScheduledTask -TaskName $taskName
```

This creates a task under the current Windows user and does not require an
administrator password. If an older task has a different owner or elevated
settings, leave it in place and use this new task name instead.

## Verify

The bridge must already be running for the second assertion below. Run:

```powershell
& 'C:\CodexBridgeTest\bin\ensure-codex-telegram-bridge.test.ps1' `
  -BridgePath 'C:\CodexBridgeTest\bin\ctr-go.exe'
Get-ScheduledTaskInfo -TaskName 'Codex Telegram Bridge Watchdog'
```

Expected test output is `watchdog behavior tests passed`, and the task's
`LastTaskResult` should be `0`. A `Ready` task state is normal: the watchdog
starts the daemon if needed and immediately exits.

## Remove

```powershell
Unregister-ScheduledTask -TaskName 'Codex Telegram Bridge Watchdog' -Confirm:$false
```

Removing the watchdog does not stop the bridge or delete any credentials.
