# Quickstart

## 1. Prerequisites

- Go 1.26 or newer.
- OpenAI Codex CLI with `codex app-server` available.
- A Telegram bot created through BotFather.
- Your numeric Telegram user ID.

Keep the bot token and user ID private. Do not commit `.env`, `config.env`,
SQLite databases, sessions, logs, or screenshots containing personal data.

## 2. Build and initialize

```powershell
git clone https://github.com/alclssna33/codex_to_telegram.git
cd codex_to_telegram
go build -o bin/ctr-go.exe ./cmd/ctr-go
.\bin\ctr-go.exe init
.\bin\ctr-go.exe doctor
```

`init` writes private local configuration. In the Windows notifier profile,
configure one allowed Telegram owner and use `CTR_GO_PROFILE=notifier`.

## 3. Run the notifier

For a foreground check:

```powershell
.\bin\ctr-go.exe daemon run
```

For Windows automatic startup:

```powershell
.\bin\ctr-go.exe service install
.\bin\ctr-go.exe service start
.\bin\ctr-go.exe service status
```

Then send `/start` to the bot from the configured owner account. The bot stores
that direct message as its single notification target and sends completion,
failure, or interruption notices for eligible local Codex Desktop and CLI work.

Telegram does not start or control Codex work in the notifier profile. It only
receives completion notifications.
