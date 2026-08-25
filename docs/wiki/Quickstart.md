# Quickstart

## 1. Create a Telegram Bot

Create a bot with BotFather and keep the token private.

## 2. Download And Initialize

On macOS, download the latest `.pkg` from
[GitHub Releases](https://github.com/alclssna33/codex_to_telegram/releases/latest),
install it, then run:

```powershell
ctr-go service install --start --start-at-login
ctr-go doctor
```

`ctr-go service install` starts a friendly setup wizard when values are not
provided through flags. It writes `~/.codex-tg/config.env`, creates a user
LaunchAgent, and starts the daemon when `--start` is present.

For archive/manual setup on any OS:

```powershell
ctr-go init
ctr-go doctor
ctr-go daemon run
```

Use `CTR_GO_CONFIG` when you want a different config path. Explicit environment
variables override config file values.
When your shell uses proxy env such as `HTTPS_PROXY` or `NO_PROXY`,
`ctr-go service install` preserves those values in the private config so the
macOS LaunchAgent can reach the same network while keeping the plist limited to
`CTR_GO_CONFIG`.

## Environment-Only Setup

```powershell
$env:CTR_GO_TELEGRAM_BOT_TOKEN = "<telegram-bot-token>"
$env:CTR_GO_ALLOWED_USER_IDS = "<telegram-user-id>"
$env:CTR_GO_DEFAULT_CWD = "C:\Users\you\Projects\Codex"
$env:CTR_GO_CODEX_CHATS_ROOT = "C:\Users\you\Documents\Codex"
# Optional: set to "off" to keep New run visible but silent.
$env:CTR_GO_NOTIFY_NEW_RUN = "on"
```

## Build From Source

Source builds require Go 1.26 or newer.

```powershell
go run ./cmd/ctr-go init
go run ./cmd/ctr-go doctor
go run ./cmd/ctr-go daemon run
```

## 3. Enable Observer

In Telegram:

```text
/start
/observe all
/threads
/projects
```

Start or continue a Codex thread from GUI/CLI. The bot should render the run in Telegram.
Only `New run`, `[Plan]`, and `[Final]` use normal Telegram notifications. Live progress cards, menus, and exports are sent silently.

Use `/plan` or `/reply --plan` for Plan Mode. If a thread remains in Plan Mode,
press `Turn off Plan` on the Plan Final Card, or use `/stop <thread>`, then send
the next normal prompt. The bridge applies App Server Default Mode to that next
ordinary turn.

To start a new thread from Telegram, open `/projects`, choose a project, press
`New thread`, then send the first prompt as the next message. The selected
project must already exist in the cached Codex thread list.

Codex UI Chats stored under `Documents/Codex` appear under the `Chats` section
instead of as normal projects. Use `Open Chats` for the full paginated list, or
`/newchat <prompt>` to create a new Codex UI Chat under
`Documents/Codex/<date>/<prompt-slug>`. Use `/newthread <prompt>` for a thread
without choosing a project or creating a Chat folder; App Server may still
attach the daemon default cwd.
