# ADR-022: Telegram global completion notifier

- Status: Accepted
- Date: 2026-08-24
- Supersedes for installed profile: ADR-020 and ADR-021

## Context

The interactive Telegram bridge proved useful, but it also introduced writer
ownership risk when Telegram tried to continue, fork, steer, approve, or
otherwise take over Codex conversations that were already owned by Codex
Desktop or CLI. The installed Windows profile now needs a safer contract:
Telegram should tell the owner when local Codex work finishes, without becoming
a second Codex client.

## Decision

The installed profile is `notifier`. It monitors all top-level, user-facing
Codex Desktop and CLI conversations exposed by the local App Server catalog,
without filtering by a registered project directory. Discovery uses bounded
`thread/list` pages for the interactive `cli` and `vscode` source kinds, then
uses `thread/read(includeTurns=true)` to confirm terminal transitions.

The notifier sends one Telegram notification for each newly observed terminal
`completed`, `failed`, or `interrupted` turn. It does not send start, progress,
tool, commentary, approval, or user-input messages. On first activation,
already-terminal conversations become baselines and are not replayed; active
conversations remain eligible for a later completion notification.

Notification text contains only safe, bounded fields:

- the final folder name derived from the authoritative CWD, never the full path;
- the user-visible conversation title when present;
- a deterministic local summary from the first meaningful final-answer
  paragraph, capped at 300 Unicode characters; and
- fixed fallback text when final/title/folder data is absent.

The Telegram input surface is restricted to owner-authorized management:
`/start`, `/help`, `/status`, and `/repair`. Plain text, replies, voice notes,
attachments, and unsupported callbacks do not reach Codex. `/repair` restarts
only the bridge's read-only App Server session and discovery loop.

## Read-only guarantees

Notifier mode must not call App Server writer methods, including
`thread/start`, `thread/resume`, `thread/fork`, `turn/start`,
`thread/name/set`, approval responses, user-input responses, steering, or
interrupt operations. The fake App Server E2E harness records only handshake,
`thread/list`, and `thread/read` methods during notifier operation.

SQLite may store observation state, terminal idempotency keys, encrypted
pending delivery payloads, and retry metadata. It must not become a
conversation store, and ordinary observation rows must not persist prompt
bodies, final answers, summaries, raw titles, full local paths, owner IDs, bot
tokens, session paths, or raw App Server payloads. Logs use sanitized event
names, short IDs, counts, and error categories only.

## Runtime profile and rollback

The existing `minimal` profile remains available only as an explicit rollback
profile. It keeps its historical registered-project, continuation, approval,
and voice behavior, but those behaviors are not the installed notifier
contract. Switching back to `minimal` requires an operator decision, a
projects file, and the prior ownership caveats from ADR-020/021.

On migration to `notifier`, stale unsent interactive delivery kinds are retired
so old command, approval, handoff, or voice messages cannot be sent after the
profile switch. Pending notifier terminal deliveries are preserved.

## Consequences

- The owner receives low-noise completion/failure notifications for Codex
  Desktop and CLI work across unregistered folders.
- Telegram can no longer create, continue, fork, approve, or steer Codex work in
  the installed profile.
- The bridge avoids active-writer conflicts by construction.
- Operators who still need interactive Telegram control can roll back to
  `minimal`, accepting the narrower registered-project contour and writer
  ownership caveats.

## Verification

Primary black-box coverage:

- `tests/telegram_notifier_e2e_test.go::TestTelegramNotifierAllFoldersRestartAndInputIsolation`
- `tests/telegram_notifier_e2e_test.go::TestTelegramNotifierRetryIsOnceOnlyAfterAcknowledgement`
- `tests/telegram_notifier_e2e_test.go::TestTelegramNotifierLogsContainNoConversationContent`

Focused command:

```powershell
go test ./tests -run TestTelegramNotifier -count=1
```
