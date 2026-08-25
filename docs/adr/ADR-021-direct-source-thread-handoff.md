# ADR-021: Direct Source Thread Handoff

- Status: accepted
- Date: 2026-08-24
- Amends: ADR-020-linked-thread-ownership-handoff.md
- Installed-profile note: superseded by ADR-022 for the default Windows
  notifier installation; retained only for explicit `minimal` rollback.

## Context

ADR-020 introduced canonical linked tasks for ownership handoff when Telegram
replies to a PC-origin source turn. That remains useful for historical linked
tasks and explicit replies, but it made `/start` selection surprising: choosing
the visible source task could silently bind Telegram to an older linked child.

Operators need selection identity to be exact. If they select source thread `S`,
the next plain Telegram command must try to continue `S`, not canonicalize to
historical linked child `L`.

## Decision

- `/start` existing-thread selection binds the exact selected thread id.
- A plain Telegram message after binding source thread `S` attempts
  `thread/resume(S)` on a bounded worker and starts the turn on `S` when the
  writer is available.
- Historical source-to-linked rows are preserved, but they do not rewrite an
  exact source selection or plain bound-source lookup.
- If a plain bound-source resume gets the exact App Server active-writer
  conflict for `S`, the bridge closes the unused worker and returns
  close-and-retry guidance. It must not fork, create or rewrite a linked row,
  queue the prompt, or persist the rejected plaintext prompt.
- If the selected PC-origin source refreshes as locally active before that
  probe, the bridge still uses the direct resume probe instead of storing the
  prompt in the active-turn queue.
- Explicit linked-thread selection remains exact for linked thread `L`.
- Reply routes with explicit source turn ids and replies routed to `L` keep the
  ADR-020 linked-task behavior.
- Startup, observer, approval, voice-preview, delivery, repair, and linked
  worker release semantics remain unchanged.

## Consequences

- The operator can choose either side of a historical handoff: selecting `S`
  resumes `S`; selecting `L` resumes `L`.
- ADR-020 remains the compatibility contract for already-created linked tasks
  and explicit linked/reply-linked routing.
- Active-writer conflict is now the only ownership probe for a direct selected
  source. The bridge fails closed instead of creating a fallback child.

## Verification

Primary deterministic coverage lives in `internal/daemon` and is indexed in
`docs/testing/regression-map.md`.
