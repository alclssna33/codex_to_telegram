# ADR-020: Linked Thread Ownership Handoff

- Status: accepted
- Date: 2026-08-23
- Amends: ADR-012

## Context

Telegram continuations need to appear in Codex Desktop as one durable task that
contains the copied source history and all later Telegram turns. Codex App
Server allows only one active writer for a thread. The bridge can read a thread
owned by Codex Desktop, but a second writer must be rejected instead of hidden
behind another fork or a global session takeover.

ADR-012 defined one daemon live session and one daemon poll session as the
session lifecycle backbone. Linked continuations keep that backbone, but move
Telegram execution for linked tasks into bounded per-thread command workers.

## Decision

- The daemon keeps one observer backbone: the long-lived live and poll sessions
  observe, read, reconcile, deliver, and repair control-plane sessions.
- The live and poll sessions must not start, resume, fork, or rename a source or
  linked thread during startup, recovery, repair, or polling fallback merely to
  inspect ownership.
- A canonical link maps one Telegram chat/topic plus one source thread to one
  linked Codex thread. Older continuation rows remain provenance; the bridge
  does not auto-delete or archive non-canonical tasks.
- A linked Telegram command uses a bounded per-thread worker. That worker owns
  exactly one linked thread generation, starts one turn, routes its approvals and
  structured input, and closes after terminal persistence and outbox enqueueing.
- Delivery/outbox persistence happens before worker release. A transient
  Telegram delivery failure must not keep the Codex writer locked.
- `ready` means the bridge has released its writer. It is not proof that Codex
  Desktop has closed the task. The next Telegram command is the ownership probe.
- If the next Telegram command gets the exact App Server active-writer error, the
  bridge leaves the link `ready`, stores only bounded diagnostic metadata, closes
  the unused worker, and asks the operator to close the linked Codex task and
  retry. If that Desktop build keeps the writer after navigation, the operator
  must exit Codex and retry.
- Startup recovery is conservative. `telegram_running` becomes
  `release_pending`; existing `release_pending` is not promoted to `ready`
  unless the current daemon has exact worker-close confirmation for that
  generation. Confirmation does not survive restart.
- Worker event-loop cancellation must not cancel terminal close or exact release
  finalization. Those steps use a separate bounded lifecycle context.
- If terminal close times out, kills the exact App Server worker child, and then
  reaps that child, the current daemon may treat the worker exit as confirmed
  release for that exact generation. If exit/reap is not confirmed, the link
  fails conservatively instead of becoming `ready`.
- `/repair` may rearm ambiguous local creation state and expose pending naming
  retry. It may finish only the bound chat/topic link's `release_pending`
  generation when the current daemon still has a matching confirmed-release
  receipt. It must not acquire a linked writer, resume a source or linked
  thread, start a model turn, or call `thread/name/set`.
- Telegram approval/deny is supported only for turns owned by the bridge worker
  and matched by worker generation. A request observed from Codex Desktop's
  separate App Server may be reported, but it must be answered in Codex Desktop.
- Voice commands remain preview-first. Executing a confirmed transcript follows
  the same canonical link and single-writer rules as text.
- Lifecycle diagnostics for linked ownership use bounded event names and fields:
  chat hash, non-sensitive project id, short source/link/turn ids, generation,
  state, duration, and RPC code. They must not include prompts, titles,
  transcripts, full local paths, full ids, tokens, or session identities.

## Consequences

- Telegram and Codex Desktop can continue the same linked task, but never at the
  same time.
- The original source task becomes historical context after linking. The bridge
  does not merge later divergent source-task turns into the linked task.
- Restart cannot prove that a previous worker cleanly released a writer.
  Conservative recovery prefers `release_pending` or `failed` over falsely
  claiming readiness.
- Operators may need to close the linked task, navigate away, or exit Codex
  Desktop before Telegram can resume the same task.
- Branch merge, task auto-delete, and archive cleanup require separate
  operator-approved work and are not part of this handoff contract.

## Verification

Primary deterministic tests live in the daemon and storage packages and are
indexed in `docs/testing/regression-map.md`. Windows live acceptance must still
prove the installed Codex build's writer-release behavior before operator docs
can claim that navigation alone is sufficient.
