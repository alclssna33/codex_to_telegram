# Windows notifier installation

Use this guide only on a PC you control. The bridge is local: it does not open
an inbound internet listener. A powered-off or sleeping PC cannot discover
Codex completions or send Telegram notifications.

## Prepare Telegram

1. In Telegram, open **BotFather**, run `/newbot`, and save the token in
   Windows Credential Manager or another private local secret store.
2. Message `@userinfobot` or use Telegram API tooling to find your numeric user
   ID. Configure exactly that one owner for notifier mode.
3. Do not put the token, owner ID, screenshots, logs, SQLite files, session
   files, or `.env` files in this repository.

Notifier mode does not require `projects.json`. It watches eligible local Codex
Desktop and CLI conversations across working folders through read-only App
Server list/read calls.

## Install prerequisites and credentials

Install the Codex CLI/App Server and the `ctr-go` binary. Go 1.26+ is required
only when building from source.

Store the Telegram credential privately:

```powershell
ctr-go secrets set telegram
```

OpenAI credentials and `ffmpeg` are not required for notifier mode because voice
messages are not downloaded or transcribed.

## Configure and run

Create a private configuration file or private process environment with:

```powershell
CTR_GO_PROFILE=notifier
CTR_GO_ALLOWED_USER_IDS=<your numeric Telegram user id>
CTR_GO_DEFAULT_CWD=C:\work
```

`CTR_GO_DEFAULT_CWD` is only the daemon's local App Server startup directory; it
does not limit which Codex work folders can be observed.

Validate configuration, then install and start the Windows service:

```powershell
ctr-go doctor
ctr-go service install
ctr-go service start
ctr-go service status
```

Use `ctr-go service restart` after changing configuration. Use
`ctr-go service uninstall` to remove the scheduled startup task; it does not
delete Credential Manager entries unless you explicitly remove them with
`ctr-go secrets delete telegram`.

If Windows power, idle, or login transitions sometimes leave the bridge stopped,
install the optional [current-user watchdog](windows-watchdog.md). It checks for
the exact bridge process every five minutes and does not create duplicates.

## Telegram operation

Send `/start` to the owner DM. The bot confirms notifier mode and stores that DM
as the notification target. Supported commands are:

- `/start`
- `/help`
- `/status`
- `/repair`

When an observed Codex Desktop or CLI conversation reaches a terminal state, the
bot sends one notification with the final folder name, optional conversation
title, and a bounded local summary or fixed fallback text. Historical terminal
conversations present at first activation are baselined and not replayed.

Plain text, replies, voice notes, attachments, and unsupported callbacks do not
reach Codex. `/repair` restarts only the bridge's read-only App Server session
and discovery loop.

## Recovery

If the PC sleeps, loses network, or Codex is closed, Telegram may not receive
notifications until the bridge is running again. Wake the PC, restore
connectivity, run:

```powershell
ctr-go service restart
ctr-go doctor
```

Queued encrypted notifier deliveries retry after restart. A successful delivery
is not replayed after restart.

## Minimal rollback profile

`CTR_GO_PROFILE=minimal` is retained only as an explicit rollback profile. It
requires a private `projects.json`, a registered-project exact-CWD registry,
and the older interactive Telegram behavior: project pickers, existing-thread
selection, Telegram-origin text turns, approval handling for bridge-owned
requests, and voice transcript confirmation.

Use this rollback only when you deliberately accept the ADR-020/021 writer
ownership caveats. The notifier installation contract does not start, resume,
fork, steer, approve, deny, answer, or interrupt Codex conversations from
Telegram.
