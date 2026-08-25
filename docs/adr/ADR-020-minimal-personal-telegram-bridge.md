# ADR-020: Minimal personal Telegram bridge

- Status: Accepted
- Date: 2026-08-20
- Installed-profile note: superseded by ADR-022 for the default Windows
  notifier installation; retained as the explicit `minimal` rollback contract.

## Decision

Build the Telegram bridge as a single-user minimal profile for one owner, one
Windows PC, and one personal Telegram bot. The bridge uses a fixed project
registry; users select only registered projects and cannot submit arbitrary
paths or manage the registry remotely. A completed Codex task sends the full
final answer, with long answers split into ordered Telegram messages.

Registered-project authorization is exact-CWD only. App Server threads are
eligible for the minimal picker and passive observer only when their canonical
current working directory equals a registered project directory; parent, child,
prefix, cached title, and previously stored project values do not authorize a
thread.

After `/start`, choosing a registered project shows two actions: start new work
or open an existing conversation. Existing conversations are fetched
dynamically from App Server `thread/list`, displayed eight rows per page, and
sorted with active threads first, then newest update time, then stable thread
ID. Selecting a row performs a fresh `thread/read` before binding and calls
`thread/resume` only after the current CWD still exactly matches the selected
registered project. The next plain natural-language message in the chat/topic
continues that bound thread; reply routing remains higher precedence.

The global observer incrementally scans all registered projects in the
background with bounded App Server pages and a durable cursor. Startup schedules
this work but does not block on a full sync. The first terminal snapshot seen
for a discovered thread is an observation baseline and is not replayed to
Telegram. Only later active-to-terminal transitions create alerts, and those
alerts contain the full final/failure answer with a project, conversation, and
short thread header. Progress, tool chatter, and historical finals are not sent
as passive alerts.

Voice commands require transcription preview and an explicit voice
confirmation before execution. Transient secrets and authorization material
are held with Windows DPAPI-backed storage and are not persisted in logs or
long-lived application state.

PC-origin approval is a hard Phase 0 gate: before product implementation is
accepted, a task started in the local Codex Desktop/CLI must be observed and
its completion and full final answer delivered through the bridge. If that
compatibility check fails, implementation does not proceed on an assumed
protocol.

The passing Windows contract uses one bridge-managed App Server shared by the
bridge and PC interactive clients. The bridge discovers the controlled thread,
calls `thread/resume` to become a live subscriber, and answers the exact pending
server request. On Windows 0.148.0 the managed App Server daemon is unavailable,
so this shared transport is a loopback-only WebSocket listener. Native Codex
Desktop sessions that keep a separate App Server remain completion-observable,
but their approvals are PC-only notices without Telegram action buttons; this
profile advertises Telegram approve/deny only for bridge-owned requests it can
answer exactly once.

## Provenance

The implementation repository is the local checkout of
`mideco-tech/codex-tg`, pinned to commit
`ec5f8265824b49a023fc3e664c1c4322e7ae611a` on branch
`feature/telegram-minimal-bridge`.

The approved design and implementation plan are copied from the parent
workspace's approved documents into this repository. The parent workspace is
not a Git repository, so it has no commit to record; its source paths are the
authoritative document inputs. The destination repository's provenance base
is the pinned upstream commit above, and this ADR/documentation commit records
the local preservation step.

`incursa/codex-telegram` is recorded only as a UX reference, not as an
implementation dependency or provenance source. The reviewed reference
checkout was `5cacd4819afc3ad97fbe8e47faedd6cf0dbe7077` (reviewed
2026-08-20).

## Explicit non-goals

- No PWA, web dashboard, or separate relay server.
- No remote desktop, screen sharing, remote PowerShell, or arbitrary terminal.
- No arbitrary Telegram paths, folder registration/deletion, file explorer, or
  mobile code editor.
- No multi-user authorization model or general-purpose remote administration.
