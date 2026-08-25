# ADR-017: Release Binaries And First-Run Init

- Status: accepted
- Decision: publish official release binaries and add a local `ctr-go init` setup flow.

## Context

Source builds are fine for contributors, but they add friction for operators who want to try `codex-tg` after discovering the project. The daemon also currently expects shell environment variables, which makes first-run setup harder than necessary and encourages secrets to drift into shell profiles or ad-hoc notes.

## Decisions

- GitHub Releases publish official `ctr-go` archives for macOS, Linux, and Windows.
- `ctr-go init` creates a local config file at `~/.codex-tg/config.env` by default.
- `CTR_GO_CONFIG` can point at another config file.
- Config file format is simple `.env` style `KEY=VALUE`; comments and quoted values are supported, but shell expansion is not.
- Runtime precedence is explicit environment variables first, then config file values, then built-in defaults.
- The config file is created with `0600` permissions where supported.
- `status`, `doctor`, daemon logs, and init summaries must not print Telegram bot tokens in full.

## Non-Goals

- No cloud account, hosted relay, or external config service.
- No GoReleaser dependency in the first distribution slice.
- Automatic OS service installation was excluded from `v0.3.0`; macOS user-level
  service installation is added later by ADR-018.
- No migration away from environment variables; existing deployments continue to work.

## Consequences

- New users can download a binary, run `ctr-go init`, then run `ctr-go daemon run`.
- Existing LaunchAgent/systemd/manual env setups keep working because env has priority.
- Release packaging becomes part of CI and must stay public-safe: archives may contain the binary, README, LICENSE, and checksums, but never local config, databases, sessions, or logs.
