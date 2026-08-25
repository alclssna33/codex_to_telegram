# Windows Phase 0 compatibility gate

- Date: 2026-08-20
- Baseline Codex: `codex-cli 0.144.2`
- Passing retry Codex: `codex-cli 0.148.0`
- Windows: `10.0.26200` (build `26200`)
- Pinned upstream: `ec5f8265824b49a023fc3e664c1c4322e7ae611a`
- Probe revision: `2758445`
- Gate status: **PASS for bridge-managed shared App Server sessions**

The live Windows matrix proved that a separate App Server process can list and
read completed Codex Desktop and interactive CLI threads, recover their final
agent messages, distinguish two simultaneous project threads, and reconcile
the same terminal turn after an App Server restart.

The hard approval requirement failed. An interactive CLI thread displayed a
real command approval prompt, but an independently connected App Server probe
watching that exact thread and turn for 30 seconds received no approval server
request. It therefore had no actionable request ID to pass to
`RespondServerRequest`. The safe test command was denied in the CLI and the
test file was not created.

## Live Windows matrix

| Case | Result | Sanitized reason |
| --- | --- | --- |
| Desktop completion | PASS | A Codex Desktop-origin thread was read as `completed`, its final agent message was hashed, and the same turn reconciled after restart. |
| Interactive CLI completion | PASS | A `source=cli` thread was read as `completed`, its final agent message was hashed, and the same turn reconciled after restart. |
| Desktop/CLI approval request | FAIL | The CLI displayed an approval prompt, but the exact-thread probe received no server request ID and could not answer it. |
| Two simultaneous projects | PASS | Two concurrent `source=cli` threads in distinct project directories were independently read as terminal with distinct final hashes and both reconciled after restart. |

## Sanitized evidence

### Desktop completion

- Thread ID: `019edf56-a657-73a3-bf2f-818f3e913dc2`
- Turn ID: `01a01d4e-4f0e-7f00-8bb2-fca7bd474131`
- Status: `completed`
- Observed at: `2026-08-20T03:55:09.0378303Z`
- Final length: `312`
- Final SHA-256: `f96b9393d2148b5f0265cc00e3ca8a8b1bc9ac55be7b5e83999b19544796d948`
- Probe facts: terminal `true`; final `true`; restart reconciled `true`

### Interactive CLI completion

- Thread ID: `01a01d46-57a0-78c3-b193-5f1e8d78ea63`
- Turn ID: `01a01d4d-dc6e-7530-8302-d538a8e46127`
- Status: `completed`
- Observed at: `2026-08-20T03:54:37.7360864Z`
- Final length: `20`
- Final SHA-256: `fd7f15d5f9cf6b34187d645be427d4ddcaa833de75870cb06947b94ca9124434`
- Probe facts: terminal `true`; final `true`; restart reconciled `true`

### PC-origin approval

- Thread ID: `01a01d46-57a0-78c3-b193-5f1e8d78ea63`
- Turn ID during approval: `01a01d47-9021-76e0-b20c-3da8a8b1402f`
- Observation window: `30s`
- CLI approval UI: present
- Approval request ID: unavailable
- Probe facts: approval seen `false`; approval answered `false`
- Side effect: test file absent after CLI denial

### Two simultaneous interactive CLI projects

- Project A thread/turn: `01a01d46-57a0-78c3-b193-5f1e8d78ea63` / `01a01d4d-dc6e-7530-8302-d538a8e46127`
- Project A status/final: `completed`; length `20`; SHA-256 `fd7f15d5f9cf6b34187d645be427d4ddcaa833de75870cb06947b94ca9124434`
- Project B thread/turn: `01a01d4d-93b8-7393-819e-c8f8482a867b` / `01a01d4d-dc6c-7071-8678-a050b59e4337`
- Project B status/final: `completed`; length `20`; SHA-256 `78ca2618e019ccb0b21a1d1e9cc1919ba753cda81fae55fb617a4c8f3f07939c`
- Both probe facts: terminal `true`; final `true`; restart reconciled `true`

## Additional compatibility finding

Two concurrent non-interactive `codex exec` runs completed locally with
`source=exec`, but the current App Server `thread/list` result did not expose
those thread IDs. The passing CLI result above therefore applies to the
interactive CLI (`source=cli`), not to `codex exec`.

## Decision

Phase 0 failed because the PC-origin approval request was not actionable from
the bridge's separate App Server connection. The implementation plan stops
before Task 3. A completion-only observer remains technically feasible for
Codex Desktop and interactive CLI, but it does not satisfy the approved
requirement to approve or deny PC-origin requests from Telegram.

## Phase 0-B shared App Server spike

Phase 0-B tested whether putting the interactive CLI and bridge probe on one
shared App Server instance would make a CLI-origin approval actionable from the
probe. The spike used the installed `codex-cli 0.144.2` without updating Codex
or persisting configuration changes.

The managed-daemon route was unavailable because this Codex version reports
that App Server daemon lifecycle is supported only on Unix platforms. The
Windows fallback therefore used an App Server WebSocket listener bound only to
`127.0.0.1`. The observer initialized successfully and completed a
`thread/list` request before the CLI connected with `--remote`.

The CLI then displayed a real command approval prompt for a harmless marker
file while the observer remained connected to the same App Server process. The
observer received no approval request during its 120-second window, so it had
no request ID to answer. Denying the request in the originating CLI resolved
the prompt, and the marker file remained absent.

| Phase 0-B check | Result | Sanitized reason |
| --- | --- | --- |
| Windows managed daemon | UNAVAILABLE | The installed Codex reports that daemon lifecycle is Unix-only. |
| Loopback shared App Server | PASS | The App Server listened only on localhost; both observer and CLI connected successfully. |
| Observer protocol readiness | PASS | The observer initialized and completed `thread/list`. |
| Cross-client approval delivery | FAIL | The CLI showed the approval UI for 120 seconds, but the second client received no approval server request. |
| Safe denial side effect | PASS | Local denial resolved the CLI prompt and the marker file was not created. |

The observed behavior is consistent with approval server requests being scoped
to the client connection that initiated the turn rather than broadcast to all
clients connected to an App Server. Because the shared CLI path already fails
the actionable-request gate, no Desktop shared-server follow-up was attempted.
Phase 0 remained **FAIL** after this spike because the observer had not joined
the active thread subscription. The corrected Phase 0-C result below supersedes
this interim decision.

## Phase 0-C corrected subscription spike

Phase 0-C repeated the shared-server experiment with the official Windows x64
`codex-cli 0.148.0` release. The executable SHA-256 matched the checksum
published with the OpenAI release. The standard standalone installer extracted
the signed package but could not complete its final directory move while the
Codex desktop runtime was active, so the experiment invoked the verified
release binary by absolute path and did not change `PATH` or stop the desktop
application.

Source inspection identified the flaw in Phase 0-B: `thread/list` discovers a
thread but does not subscribe the connection to its live events. In 0.148.0,
`thread/resume` adds the calling connection to the thread's subscriber set and
replays pending server requests for that thread. Approval requests are sent to
the subscribed connection set, and the first valid response resolves the
pending request.

The corrected observer therefore captured a baseline list, detected a new
interactive thread, called `thread/resume`, and only then waited for approval
requests. The TUI and observer shared one App Server bound to loopback. The TUI
used `--remote`, `approvalPolicy=on-request`, and
`approvals_reviewer="user"`.

| Phase 0-C check | Result | Sanitized reason |
| --- | --- | --- |
| Official 0.148.0 artifact | PASS | The Windows x64 executable hash matched the official release checksum. |
| Loopback shared App Server | PASS | TUI and observer used one process listening only on `127.0.0.1`; the port was closed after the test. |
| Explicit live-thread subscription | PASS | The observer resumed the new thread and received its live server requests. |
| Cross-client deny | PASS | The observer received the command approval request, replied `decline`, the TUI reported denial, and the marker remained absent. |
| Cross-client approve | PASS | A new observer resumed the same thread, replied `accept` to a second request, the TUI completed, and the zero-byte marker appeared. |
| Cleanup | PASS | The marker, observer script, TUI, App Server process, and loopback listener were removed or stopped. |

### Sanitized Phase 0-C evidence

- Thread ID: `01a01d72-e5c8-7141-9216-41c54a549b68`
- Deny turn ID: `01a01d73-473b-7053-8193-22b3c2d1e106`
- Accept turn ID: `01a01d74-87d4-7e13-b0f5-23fc85a2e3fb`
- Request method: `item/commandExecution/requestApproval`
- Deny facts: request seen `true`; response written `true`; side effect absent
- Accept facts: request seen `true`; response written `true`; side effect present
- Final listener state: no listener on test port

The remote TUI thread was reported by `thread/list` with `source="vscode"`, not
`source="cli"`. Production discovery must therefore identify controlled
threads by canonical cwd, thread identity, and subscription state rather than
assuming the source label from the launching surface.

### Open-source comparison

- OpenAI App Server 0.148.0 already contains per-thread multi-connection
  subscription and pending-request replay within one App Server instance. A
  patched Codex fork is not required for this narrower shared-instance approval
  path.
- `incursa/codex-telegram` remains a useful reference for Telegram UX, voice,
  project selection, output relay, and bridge-owned App Server sessions. It
  does not prove passive approval control over a separate native Desktop App
  Server instance.
- `wipcomputer/wip-codex-remote-control` demonstrates broader TUI/Desktop/phone
  co-presence with a patched Codex fork. Its own notes treat that patch as a
  temporary exploration because separate App Server instances still do not
  synchronize.
- Codex `PermissionRequest` hooks can feed external approval UIs, but the open
  upstream reviewer/defer contract means they are not selected as the primary
  approval transport for this bridge.

## Final Phase 0 rationale

The hard actionable-approval gate is now **PASS** for PC-origin interactive
sessions launched against the bridge-managed shared App Server. Both approve
and deny were exercised from a second connection against the exact pending
request, and existing completion/final/restart checks remain passing.

This does not claim that a stock Codex Desktop task, which owns a separate App
Server instance, exposes its native approval request to the bridge. Desktop
completion notifications remain supported through reconciliation; Telegram
approval requires the shared-server launch path until OpenAI provides
cross-instance co-presence or the hook routing contract is validated separately.
Task 3 is unblocked with this launch constraint recorded as part of the product
contract.

<!-- ctr-go-phase0-final-decision:v1
decision=PASS
content-sha256=85b3f6013219975551758b9acedad87d5fa11eb4c2690d911257cd77ca0950bf
-->
