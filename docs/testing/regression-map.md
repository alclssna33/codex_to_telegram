# Regression Map

This map is the handoff index for agents changing Codex control-plane
contracts, Telegram routing, observer panels, lifecycle recovery, diagnostics,
or Plan Mode.

When behavior changes, update the relevant ADR first, then update or add the tests named here. The tests are part of the architecture: they describe the contract that must survive App Server drift and daemon restarts.

## Control Plane Architecture

ADR: `docs/adr/ADR-019-codex-control-plane.md`; feature brief is
`docs/process/v0.5.0-codex-control-plane-brief.md`; roadmap is
`docs/process/v0.5.0-control-api-roadmap.md`.

Future primary tests:

- App Server capability-map smoke detects supported and missing schema methods.
- Control-core thread lifecycle tests cover list/read/start/resume/fork/rename
  through an App Server-backed implementation.
- Control-core turn lifecycle tests cover start/steer/interrupt plus stale-active
  recovery preservation.
- Event normalization tests cover lifecycle/tool/final/approval/input events
  before Telegram-specific rendering.
- Notification policy tests classify normalized events as `urgent`, `normal`,
  `silent`, or `digest`.
- Local control API tests prove the router-agent HTTP adapter is disabled unless
  configured, loopback-only, read-only for the first slice, and delegates
  thread list/read through the control surface.
- Telegram adapter compatibility tests prove existing commands and callback
  routing still work on top of the control API.

Contract notes:

- App Server remains authoritative for live interactive Codex state.
- SDK/MCP adapters are allowed only as orchestration adapters under ADR-019.
- Session JSONL must not re-enter live UI/control state.
- Generated App Server schema is an input to capability detection, not a file to
  commit as a runtime source of truth.
- Telegram is a channel adapter; Telegram ids must not become Codex identity.

## Distribution And Local Config

ADR: `docs/adr/ADR-017-release-binaries-and-init.md` and
`docs/adr/ADR-018-macos-service-installer.md`; feature briefs are
`docs/process/v0.3.0-distribution-brief.md` and
`docs/process/v0.4.0-macos-service-installer-brief.md`.

Primary tests:

- `internal/config/config_test.go::TestParseEnvFileSupportsCommentsAndQuotes`
- `internal/config/config_test.go::TestParseEnvFileRejectsInvalidLine`
- `internal/config/config_test.go::TestLoadReadsConfigFileAndEnvOverridesIt`
- `internal/config/config_test.go::TestLoadAppliesRuntimeProxyEnvFromConfigFile`
- `internal/config/config_test.go::TestLoadDoesNotOverrideExplicitRuntimeProxyEnv`
- `cmd/ctr-go/main_test.go::TestRunInitWritesPrivateConfigAndRefusesOverwrite`
- `cmd/ctr-go/main_test.go::TestRunInitForceOverwritesConfig`
- `cmd/ctr-go/main_test.go::TestStatusAndDoctorDoNotLeakConfigFileToken`
- `cmd/ctr-go/main_test.go::TestFatalErrorSanitizerRedactsTelegramBotURL`
- `cmd/ctr-go/service_test.go::TestServiceInstallNonInteractiveWritesConfigAndLaunchAgent`
- `cmd/ctr-go/service_test.go::TestServiceInstallCapturesRuntimeProxyEnvInConfig`
- `cmd/ctr-go/service_test.go::TestServiceInstallInteractiveWizardRetriesInvalidValues`
- `cmd/ctr-go/service_test.go::TestServiceInstallNonInteractiveReportsMissingFlags`
- `cmd/ctr-go/service_test.go::TestRenderLaunchAgentPlistContainsOnlyConfigEnvironment`
- `cmd/ctr-go/service_test.go::TestServiceLifecycleUsesLaunchctlRunner`
- `cmd/ctr-go/service_test.go::TestServiceStartAcceptsKickstartFailureWhenServiceLoaded`
- `internal/trayapp/actions_test.go::TestCTRGoArgs`
- `internal/trayapp/actions_test.go::TestServiceSetupArgs`

Contract notes:

- `config.env` is local runtime state and must not be committed.
- Explicit environment variables override config file values.
- macOS LaunchAgent plists must carry only `CTR_GO_CONFIG`, never token/user/cwd env values.
- Proxy env needed by the operator shell may be stored in the private config and
  applied by the process after startup.
- Tray control is not a settings editor in v0.4.0.
- Release archives and packages must not include local config, sessions, SQLite state, logs, or screenshots.

## Plan Mode Routing

ADR: `docs/adr/ADR-006-plan-prompt-mode.md`; reset addendum:
`docs/adr/ADR-016-plan-mode-reset-contract.md`

Primary tests:

- `internal/daemon/service_test.go::TestPlanCommandStartsPlanCollaborationMode`
- `internal/daemon/service_test.go::TestPlanCommandUsesBoundThreadWhenNoExplicitThread`
- `internal/daemon/service_test.go::TestPlanCommandUnknownHeadUsesBoundThreadAsPromptText`
- `internal/daemon/service_test.go::TestPlanCommandUnknownHeadWithoutImplicitRouteShowsUsage`
- `internal/daemon/service_test.go::TestPlanCommandUUIDLikeHeadStaysExplicit`
- `internal/daemon/service_test.go::TestPlanCommandKnownThreadHeadStaysExplicit`
- `internal/daemon/service_test.go::TestReplyPlanFlagStartsPlanCollaborationMode`
- `internal/daemon/service_test.go::TestReplyDefaultFlagStartsDefaultCollaborationMode`
- `internal/daemon/service_test.go::TestDefaultModeCommandStartsDefaultCollaborationMode`
- `internal/daemon/service_test.go::TestPlanFinalCardShowsTurnOffPlanButton`
- `internal/daemon/service_test.go::TestNormalFinalCardDoesNotShowTurnOffPlanButton`
- `internal/daemon/service_test.go::TestReplyCommandConsumesDefaultOverrideOnce`
- `internal/daemon/service_test.go::TestDefaultOverrideSurvivesTurnStartFailure`
- `internal/daemon/service_test.go::TestPlanCommandClearsStaleDefaultOverride`
- `internal/daemon/service_test.go::TestStopSetsDefaultOverrideForActiveThread`
- `internal/daemon/service_test.go::TestStopSetsDefaultOverrideForIdleThread`
- `internal/daemon/observer_ui_v2_test.go::TestTurnOffPlanCallbackSetsDefaultOverrideAndEditsFinalCard`
- `internal/daemon/observer_ui_v2_test.go::TestTurnOffPlanCallbackRejectsMismatchedMessageID`
- `internal/daemon/service_test.go::TestPlanModeCommandCanRouteByReply`
- `internal/daemon/service_test.go::TestPlainReplyToSyntheticPlanPromptUsesTurnSteer`
- `internal/daemon/service_test.go::TestPlainReplyToSyntheticPlanPromptFallsBackToTurnStart`
- `internal/daemon/service_test.go::TestPlainReplyToRealPlanPromptUsesServerRequest`
- `internal/daemon/observer_ui_v2_test.go::TestSyncThreadPanelCreatesRouteablePlanPromptAndDedupes`
- `internal/daemon/observer_ui_v2_test.go::TestSyncThreadPanelCreatesServerRequestPlanPromptRoute`

Live E2E:

- `tests/live_e2e/telegram_readback_e2e.py` case `plan_mode_reset`

Contract notes:

- `/plan <text>` uses reply, armed state, or bound thread routing.
- `/plan <thread> <text>` is explicit only for known or UUID-like thread ids.
- `/reply --plan <thread> <text>` remains strict.
- `Turn off Plan` on a Plan Final Card and `/stop <thread>` set a one-shot Default override for the next ordinary turn; they do not start a reset turn.
- The one-shot override is cleared after a successful ordinary `turn/start` and remains after a failed `turn/start`.
- Hidden `/default` and `/reply --default` fallback paths remain tested but are not advertised in public help or Telegram command menu.
- `Turn off Plan` is panel-bound like Details; stale panel/message/thread/turn callbacks fail closed without changing state.
- Plan choice buttons must stay scoped to the same turn as the `[Plan]` card.
- Stale pending `user_input` from an older turn must not add `answer_choice` buttons to a newer `[commentary]` panel.

## Project Thread Creation

ADR: `docs/adr/ADR-014-newchat-chat-folder-contract.md`; feature brief is `docs/process/create-thread-from-project-brief.md`.

Primary tests:

- `internal/daemon/service_test.go::TestProjectsCommandShowsProjectButtonsGroupedByCWD`
- `internal/daemon/service_test.go::TestIsCodexChatsCWDMatchesGenericMacAndWindowsPaths`
- `internal/daemon/service_test.go::TestProjectsCommandShowsChatsSectionAndSortsByRecency`
- `internal/daemon/service_test.go::TestProjectsPaginationUsesPreviewLimitsAndKeepsLatestChats`
- `internal/daemon/service_test.go::TestOpenChatsPaginatesAndChatSelectionBindsThread`
- `internal/daemon/service_test.go::TestProjectsCloseDeletesMenuMessage`
- `internal/daemon/service_test.go::TestProjectOpenShowsNewThreadMenu`
- `internal/daemon/service_test.go::TestProjectNewThreadArmsThenPlainTextCreatesThread`
- `internal/daemon/service_test.go::TestProjectNewThreadRejectsThreadStartWithoutID`
- `internal/daemon/service_test.go::TestProjectNewThreadTurnStartFailureSavesThread`
- `internal/daemon/service_test.go::TestNewChatCommandCreatesCodexUIChatCWDAndBinds`
- `internal/daemon/service_test.go::TestNewChatCWDUsesFallbackSlugAndCollisionSuffix`
- `internal/daemon/service_test.go::TestNewThreadCommandCreatesThreadWithoutCWDAndBinds`
- `internal/daemon/service_test.go::TestNewChatCommandRejectsMissingThreadID`
- `internal/daemon/service_test.go::TestNewChatCommandTurnStartFailureSavesAndBindsThread`
- `internal/daemon/service_test.go::TestNewThreadCommandTurnStartFailureSavesAndBindsThread`
- `internal/config/config_test.go::TestFromEnvReadsCodexChatsRoot`
- `internal/telegram/bot_test.go::TestDefaultCommandsExposeNewChatMenuCommand`
- `internal/daemon/service_test.go::TestSummaryPanelDoesNotShowStalePendingUserInputButtons`
- `tests/config_env_test.go::TestFromEnvProjectChatLimitsClampInvalidValues`

Live E2E:

- Open `/projects`, choose a project, press `New thread`, send a prompt, and verify a new thread/run reaches `[Final]`.
- Open `/projects`, verify normal projects are sorted by recent activity, `Documents/Codex` threads are shown only as latest Chat previews, then open full `Chats` pagination and select a Chat.
- Run `/newchat <prompt>`, verify the new thread reaches `[Final]`, the generated cwd exists under the configured Chats root, `/projects -> Open Chats` shows it, and a plain follow-up routes to the newly bound Chat thread.
- Run `/newthread <prompt>`, verify the new thread reaches `[Final]` without creating a Chat cwd under the configured Chats root.
- Send a plain reply after creation and verify it routes to the newly bound thread.
- Run a Plan Mode prompt with structured choices and verify choice buttons appear only on the current `[Plan]` card.

Contract notes:

- Project/workspace identity comes from cached thread `cwd`; this flow does not create or edit work directories.
- Threads under generic `Documents/Codex` cwd roots or configured `CTR_GO_CODEX_CHATS_ROOT` are Codex UI `Chats`, not normal projects. A Chat selection opens and binds its single thread; Chat lists do not expose project `New thread`.
- Main `/projects` uses project pagination with configurable preview limits and keeps latest Chat previews newest-first. Full Chat pagination lives behind `Open Chats`.
- Project buttons use `N. Project name`; Chat buttons use `Chat N. Thread name`. The menu must not render internal `key:` rows and must show each project row's `last thread:`.
- `/newchat <prompt>` creates a dated Chat cwd from a prompt slug and passes that cwd to App Server `thread/start`.
- `/newthread <prompt>` creates a new App Server thread without a Telegram-selected cwd parameter. It must not create a Chat folder; App Server may still attach the daemon default cwd.
- Telegram must not accept arbitrary local filesystem paths for thread creation.
- The first prompt is required; create-only threads are out of scope for this slice.

## Full Thread ID Access

ADR: `docs/adr/ADR-007-parallel-thread-visual-identity.md`

Primary tests:

- `internal/daemon/service_test.go::TestContextCardBoundThreadIncludesFullThreadID`
- `internal/daemon/service_test.go::TestSummaryPanelGetThreadIDButtonSendsCopyableIDs`
- `internal/daemon/service_test.go::TestFinalSummaryPanelHasGetThreadIDButton`
- `internal/daemon/service_test.go::TestFinalCardGetThreadIDButtonSendsCopyableIDs`

Contract notes:

- Header chips like `T:d663` and `R:d9bc` are visual hints only.
- Operators must be able to retrieve copyable full ids from Telegram without SQLite/log access.

## Final Card Details Binding

ADR: `docs/adr/ADR-004-final-card-details-ux.md`

Primary tests:

- `internal/daemon/observer_ui_v2_test.go::TestFinalCardDetailsCallbacksEditSameMessageAndExportToolsFile`
- `internal/daemon/observer_ui_v2_test.go::TestFinalCardDetailsShowsToolOnlyTurnWithoutCommentary`
- `internal/daemon/observer_ui_v2_test.go::TestDetailsCallbacksUsePanelTurnInsteadOfLatestThreadTurn`
- `internal/daemon/observer_ui_v2_test.go::TestDetailsCallbacksStayBoundToOriginalPanelAfterNewerRunCompletes`
- `internal/daemon/observer_ui_v2_test.go::TestDetailsCallbackWithoutPanelIDDoesNotFallbackToCurrentPanel`
- `internal/daemon/observer_ui_v2_test.go::TestDetailsCallbackRejectsMismatchedMessageID`
- `internal/daemon/observer_ui_v2_test.go::TestDetailsToolsFileRejectsMismatchedPanelRoute`

Contract notes:

- `Details`, pagination, `Tool on`, `Tools file`, and `Back` are bound to the completed panel/card that produced the callback.
- A Details callback without a valid `panel_id`, with a mismatched thread/turn, or from another Telegram message is stale and must not edit/export current run data.
- Pressing `Back` on an older completed run must restore that older Final Card in the same message, not duplicate or replace it with the latest run.
- Finalization sends a new Final Card and moves the panel summary message id to it; Details/Back must edit that new card, not the deleted live commentary card.
- Tool-only turns with no commentary and empty output still expose completed command/status in Details, Tool mode, and Tools file under `Tool activity`.

## Telegram Notification Contract

ADR: `docs/adr/ADR-015-telegram-notification-contract.md`

Primary tests:

- `internal/telegram/api_test.go::TestClientSendMessageSilentSetsDisableNotification`
- `internal/telegram/api_test.go::TestClientSendDocumentSilentSetsDisableNotification`
- `internal/telegram/bot_test.go::TestBotDeliverDirectResponseSendsSilentMessage`
- `internal/config/config_test.go::TestMarshalJSONIncludesNotifyNewRun`
- `tests/config_env_test.go::TestFromEnvDefaultsLoggingOn`
- `tests/config_env_test.go::TestFromEnvPrefersGoScopedEnvVars`
- `internal/daemon/observer_ui_v2_test.go::TestSyncThreadPanelCreatesRouteablePlanPromptAndDedupes`
- `internal/daemon/observer_ui_v2_test.go::TestRunNoticeNotificationFlagCanSilenceNewRun`
- `internal/daemon/observer_ui_v2_test.go::TestFinalTransitionDeletesRunNoticeToolAndOutputButKeepsUser`
- `internal/daemon/observer_ui_v2_test.go::TestFinalCardDetailsCallbacksEditSameMessageAndExportToolsFile`

Live E2E:

- Run a Telegram-origin command with `CTR_GO_NOTIFY_NEW_RUN=on`; verify `New run`, live silent cards, new `[Final]`, and Details/Back.
- Repeat with `CTR_GO_NOTIFY_NEW_RUN=off`; verify `New run` remains visible and `[Final]` still arrives.
- Run a Plan Mode structured-choice prompt and verify `[Plan]` is the only question card with answer buttons.

Contract notes:

- All new bot messages are silent except `New run`, `[Plan]`, and `[Final]`.
- `New run` notification is configurable and enabled by default.
- Explicit exports and direct command/menu responses are silent.
- Old live commentary routes may remain in SQLite after deletion, but active Details routing uses the new Final card message id.

## Turn Lifecycle And Stale Active Recovery

ADR: `docs/adr/ADR-012-turn-lifecycle-normalization.md`

Primary tests:

- `internal/appserver/normalize_test.go::TestSnapshotFromThreadReadTreatsFinalAnswerAsCompletedWhenStatusIsStale`
- `internal/daemon/service_test.go::TestStaleActiveThreadWithFinalAnswerStartsNewTurn`
- `internal/daemon/service_test.go::TestNoActiveTurnSteerFailureFallsBackToTurnStart`
- `internal/daemon/service_test.go::TestReplyToActiveThreadDoesNotFallbackToTurnStartWhenSteerFails`
- `internal/daemon/service_test.go::TestReplyToActiveThreadSteersActiveTurn`
- `internal/daemon/observer_ui_v2_test.go::TestGlobalObserverDoesNotRecreateTelegramOriginPanelOnEditFailure`

Contract notes:

- A final answer is terminal evidence unless the turn is waiting for approval or user input.
- `no active turn to steer` means stale active state and may fall back to a new turn after re-read.
- Active or not-steerable failures still block fallback `turn/start`.
- A Telegram-origin panel must not be duplicated by global observer sync for the same marked turn.

## App Server Session Lifecycle

ADR: `docs/adr/ADR-012-turn-lifecycle-normalization.md`

Primary tests:

- `internal/daemon/service_test.go::TestEnsureLiveSessionSerializedAgainstReconcile`
- `internal/daemon/service_test.go::TestRepairInvalidatesOldLiveLoop`
- `internal/daemon/service_test.go::TestControlLoopProcessesRepairBeforeReconcile`
- `internal/appserver/client_test.go::TestClientStartConcurrentCallsShareInitializedSession`
- `internal/appserver/client_test.go::TestClientStartFailureLeavesClientRetryable`

Contract notes:

- One live App Server session has one live event loop per generation.
- Stale old live-loop closes must not clear newer session state.
- Repair is serialized with reconcile/startup and is processed before replacement reconcile.

## Direct And Linked Thread Ownership Handoff

ADRs: `docs/adr/ADR-021-direct-source-thread-handoff.md` and
`docs/adr/ADR-020-linked-thread-ownership-handoff.md`; ADR-020 amends
`docs/adr/ADR-012-turn-lifecycle-normalization.md`.

Primary tests:

- `internal/daemon/minimal_thread_picker_test.go::TestMinimalPickerSelectsExactSourceDespiteHistoricalLinkedThread`
- `internal/daemon/minimal_thread_picker_test.go::TestMinimalPickerHistoricalLinkedReadFailureStillBindsExactSource`
- `internal/daemon/minimal_thread_picker_test.go::TestMinimalBoundThreadForProjectReturnsExactSourceDespiteHistoricalLinkedThread`
- `internal/daemon/minimal_thread_picker_test.go::TestMinimalBoundThreadForProjectExplicitLinkedBindingReturnsLinkedThread`
- `internal/daemon/minimal_thread_picker_test.go::TestPlainExplicitLinkedBindingReadFailureDoesNotStartNewThread`
- `internal/daemon/minimal_continuation_test.go::TestSelectedPCOriginSourceResumesExactSourceWithoutFork`
- `internal/daemon/minimal_continuation_test.go::TestSelectedPCOriginSourceActiveWriterConflictFailsClosedWithoutForkOrPromptPersistence`
- `internal/daemon/service_test.go::TestMinimalServiceStartupAdoptsLinkedBeforeClaimedCommandRecovery`
- `internal/daemon/service_test.go::TestMinimalRecoveryDoesNotResume`
- `internal/daemon/service_test.go::TestRepairDoesNotAcquireLinked`
- `internal/daemon/service_test.go::TestRepairLinkedReleasePendingWithConfirmedReceiptFinalizesExactGeneration`
- `internal/daemon/service_test.go::TestRepairLinkedReleasePendingWithoutExactReceiptLeavesRowUnchanged`
- `internal/daemon/service_test.go::TestServiceCloseClosesWorkersThenLivePoll`
- `internal/daemon/service_test.go::TestMinimalLinkDiagnostics`
- `internal/daemon/service_test.go::TestMinimalPollTrackedSkipsNotLoadedContinuationChildWithoutResume`
- `internal/appserver/client_test.go::TestClientCloseContextKillsAndReapsExactChildAfterGraceTimeout`
- `internal/storage/store_minimal_test.go::TestAdoptMinimalLinkedThreadsPrefersThreadBindingAndPreservesLegacyRows`
- `internal/storage/store_minimal_test.go::TestAdoptMinimalLinkedThreadsFallsBackToNewestThenChildID`
- `internal/storage/store_minimal_test.go::TestMinimalLinkedThreadProtectsTitlesAndTransitionsByGeneration`
- `internal/storage/store_minimal_test.go::TestMinimalLinkedTitleHydrationFillsLegacyPayloadsWithoutOverwrite`
- `internal/storage/store_minimal_test.go::TestMinimalLinkedBlockedRecordsOnlyReadyOwnershipProbe`
- `internal/daemon/minimal_link_worker_test.go::TestMinimalLinkWorkerRecordsConfirmedReleaseForExactGeneration`
- `internal/daemon/minimal_link_worker_test.go::TestMinimalLinkWorkerCloseUsesIndependentContextAfterLoopCancel`
- `internal/daemon/minimal_link_worker_test.go::TestMinimalLinkWorkerCloseConfirmedExitRecordsRelease`
- `internal/daemon/minimal_link_worker_test.go::TestMinimalLinkWorkerCloseUnconfirmedDeadlineDoesNotRecordRelease`
- `internal/daemon/minimal_continuation_test.go::TestPCContinuationForksOnWorkerAndPersistsCanonicalLink`
- `internal/daemon/minimal_continuation_test.go::TestLinkedTitleUsesTransientSourceTitleAndPersistsProtectedPayload`
- `internal/daemon/minimal_continuation_test.go::TestExistingLinkedTitleHydratesLegacyPayloadFromSourceRead`
- `internal/daemon/minimal_continuation_test.go::TestRepeatedParentReplyResumesCanonicalLink`
- `internal/daemon/minimal_continuation_test.go::TestExistingLinkedActiveWriterConflictClosesWorkerAndKeepsPromptOutOfQueue`
- `internal/daemon/minimal_continuation_test.go::TestCanonicalLinkedBacklogDrainActiveWriterReleasesClaimForResend`
- `internal/daemon/minimal_observer_test.go::TestMinimalLinkedTerminalReleaseUsesIndependentCloseAndFinalizationContext`
- `internal/daemon/minimal_observer_test.go::TestMinimalLinkedTerminalConfirmedDeadlineCloseStillFinalizesReady`
- `internal/daemon/minimal_observer_test.go::TestMinimalLinkedTerminalReleaseDrainsNextCommandOnFreshWorker`
- `internal/daemon/minimal_observer_test.go::TestMinimalLinkedTerminalReleaseDrainsNextCommandWithWorkerContext`
- `internal/daemon/minimal_observer_test.go::TestReleasePendingWithoutConfirmedCloseDoesNotClaimReady`
- `internal/daemon/minimal_observer_test.go::TestStaleRunningLinkedTerminalSkipsProjectionReleaseAndFIFO`
- `internal/daemon/minimal_observer_test.go::TestStaleReleasePendingLinkedTerminalSkipsTerminalOutboxAndFIFO`
- `internal/daemon/minimal_voice_test.go::TestVoiceLinkedActiveWriterConflictRetainsNoRejectedTranscriptOrPrompt`
- `internal/daemon/minimal_approval_test.go::TestStaleWorkerApprovalCallbackExpiresAfterReleaseWithoutGlobalFallback`

Live and environment qualifications:

- Windows live handoff must be verified with `tests/live_e2e/windows_bridge_live_test.go::TestWindowsBridgeLiveAcceptance` on an authorized disposable contour before claiming the installed Codex build releases a writer by task close/navigation. If it does not, operator copy must continue to require exiting Codex and retrying.
- `tests/telegram_minimal_e2e_test.go::TestMinimalBridgeEndToEndAcrossRestart` remains the restart E2E baseline. Do not document it as passing for this handoff unless a fresh run records a pass; a known baseline issue must be reported as a qualification, not hidden by linked-handoff deterministic tests.
- `go test -race` checks require a Go race-capable toolchain with gcc/CGO available on Windows. If gcc is unavailable, record that the race contour was not run instead of calling it clean.

Contract notes:

- `/start` existing-thread selection binds the exact selected thread id. A
  historical source-to-linked row does not rewrite a selected source `S` to
  linked child `L`.
- A plain Telegram message after an exact source binding attempts
  `thread/resume(S)` and `turn/start(S)` on a bounded worker when ownership is
  available.
- If direct source `S` is still owned by Codex Desktop, the exact active-writer
  conflict closes the unused worker and returns close-and-retry guidance. It
  must not fork, create or rewrite a linked row, queue the prompt, or persist
  the rejected plaintext prompt.
- Explicit linked-thread selection and replies routed to a linked thread remain
  usable as exact linked-thread operations.
- Reply routes with explicit source turn ids retain ADR-020 canonical
  linked-task compatibility.
- Startup order is `EnsureMinimalSchema`, creating-continuation recovery,
  legacy linked adoption, linked-worker recovery, claimed-command recovery,
  approval recovery, and voice-confirmation recovery.
- Startup, recovery, and `/repair` must not call `thread/resume`,
  `thread/fork`, `turn/start`, or `thread/name/set`, and must not transition a
  released linked task from `ready` to `telegram_running`.
- `ready` means bridge released only. The next Telegram command is the ownership
  probe and may still find a Codex Desktop active writer.
- Terminal worker close and exact release finalization use a bounded lifecycle
  context that is independent of the worker event-loop context being canceled.
- A close timeout followed by killing and reaping the exact App Server worker
  child is current-daemon release confirmation for that exact generation.
  Unconfirmed exit/reap leaves the link failed or unchanged, not `ready`.
- A stale `telegram_running` row becomes `release_pending` after restart.
  `release_pending` becomes `ready` only with exact current-daemon close
  confirmation; that confirmation does not survive restart.
- `/repair` may rearm ambiguous local creation and leave title retry pending for
  the next safe worker acquisition. It may finish only the bound chat/topic
  link's `release_pending` generation when `minimalWorkers` still has a matching
  current-daemon confirmed-release receipt. It must not acquire or resume the
  writer.
- Lifecycle diagnostics for linked ownership are short-id/hash/count/code only:
  no prompt, title, transcript, full local path, full id, token, or session
  identity.

## Transient Interrupted Gating

ADR: `docs/adr/ADR-012-turn-lifecycle-normalization.md`

Primary tests:

- `internal/daemon/terminal_gate_test.go::TestTelegramEmptyInterruptedGateDefersAndKeepsHotPollingMetadata`
- `internal/daemon/terminal_gate_test.go::TestTelegramEmptyInterruptedGateRecoversAndClearsDefer`
- `internal/daemon/terminal_gate_test.go::TestTelegramEmptyInterruptedGateGraceExpiryAccepts`
- `internal/daemon/terminal_gate_test.go::TestTelegramEmptyInterruptedGateExplicitInterruptBypassesDefer`
- `internal/daemon/terminal_gate_test.go::TestTelegramFinalInterruptedGateDefersUntilRecovered`
- `internal/daemon/terminal_gate_test.go::TestTelegramPartialInterruptedGateDefersUntilFinalOrGrace`
- `internal/daemon/service_test.go::TestPollTrackedDefersTelegramOriginEmptyInterruptedAndKeepsActiveState`
- `internal/daemon/service_test.go::TestPollTrackedDefersTelegramOriginPartialInterruptedAndKeepsActiveState`
- `internal/daemon/service_test.go::TestPollTrackedDefersTelegramOriginFinalInterruptedAndKeepsActiveState`
- `internal/daemon/service_test.go::TestTelegramOriginHotPollCapturesRunningTool`
- `internal/daemon/service_test.go::TestLiveToolNotificationIgnoresOlderTurnAfterNewerCompletion`
- `internal/daemon/service_test.go::TestRefreshThreadForOperationDefersEmptyInterrupted`

Live E2E:

- checked-in public-safe harness: `tests/live_e2e/telegram_readback_e2e.py`
- requires `CODEX_TG_LIVE_E2E=1`, `CODEX_TG_E2E_THREAD_ID`, a local Telethon session, and bot identity from local env
- uses MTProto readback of edited messages and optional daemon log correlation
- exercises sequential commands plus a multi-command `/reply` math run to catch accidental self-interruption

Contract notes:

- Implicit Telegram-origin `interrupted` is ambiguous until it recovers, expires, or follows explicit `/stop`.
- Deferred terminal state must not collapse the live panel into a false Final Card.
- The daemon must keep polling deferred turns hot.
- Telegram-origin turns get a short App Server `thread/read` hot-poll window after start so `[Tool]` can become visible even when live events do not expose the running command.
- If App Server still has not exposed a tool for an active turn, `[Tool]` must show neutral active-run elapsed time instead of a static empty state.
- Late live tool notifications from older turns must not overwrite a newer completed turn or reintroduce stale `[Tool]` / `[Output]` content.

## Nil-Safe Telegram Rendering

ADR: `docs/adr/ADR-012-turn-lifecycle-normalization.md`

Primary tests:

- `internal/daemon/log_archive_test.go::TestValueFromMapSkipsNilLikeValues`
- `internal/daemon/log_archive_test.go::TestRenderCommandSkipsNilLikeValues`
- `internal/daemon/log_archive_test.go::TestRenderEventMsgWithoutCommandDoesNotPrintNil`
- `internal/daemon/session_tail_overlay_test.go::TestPollTrackedIgnoresStaleSessionTailTool`
- `internal/daemon/observer_ui_v2_test.go::TestSummaryPanelRemovesNilLiteralBeforeRendering`
- `internal/appserver/client_test.go::TestRPCStringSkipsNilLikeValues`
- `internal/appserver/normalize_test.go::TestStringValueTreatsNilLiteralAsMissing`

Live E2E:

- checked-in public-safe harness: `tests/live_e2e/telegram_readback_e2e.py`
- run against a dedicated private test thread from local env, not the working operator thread
- scenarios: sequential `pwd`, `date`, `printf`, dedicated sleep-20 timing, slow command, and multi-command math through `/reply`
- acceptance: scan edited Telegram `[Tool]`, `[Output]`, and `[Final]` messages for literal `"<nil>"`, stale commands from earlier runs, false parallel-turn rejection, and visible non-final `interrupted`

Contract notes:

- Missing App Server fields and literal `"<nil>"` are nil-like values, not display text.
- Telegram rendering must clean nil-like values before Markdown/entity conversion.
- Diagnostics for `telegram_render_contains_nil` are bounded and hash-only.

## Diagnostics And Sanitization

ADR: `docs/adr/ADR-012-turn-lifecycle-normalization.md`

Primary tests:

- `internal/daemon/service_test.go::TestTelegramTurnLifecycleLogsSuccessfulStart`
- `internal/daemon/service_test.go::TestTelegramTurnLifecycleLogsThreadResumeFailure`
- `internal/daemon/service_test.go::TestTelegramTurnLifecycleLogsTurnStartFailure`
- `internal/daemon/service_test.go::TestTelegramTurnLifecycleLogsRefreshFailuresAroundStart`
- `internal/daemon/service_test.go::TestDiagnosticLogsAreRateLimited`
- `internal/daemon/service_test.go::TestDiagnosticLoggerCanBeDisabled`
- `internal/daemon/service_test.go::TestObserverSyncResultLogsAreDebounced`
- `internal/daemon/service_test.go::TestGenericThreadReadDiagnosticsAreDebounced`
- `internal/daemon/service_test.go::TestThreadReadSkippedLogsAreDebounced`
- `internal/telegram/bot_test.go::TestSanitizeTelegramLogErrorRedactsBotTokenURL`
- `tests/config_env_test.go::TestFromEnvDefaultsLoggingOn`
- `tests/config_env_test.go::TestFromEnvInvalidLoggingFlagsFallBackToEnabled`
- `cmd/ctr-go/main_test.go::TestDiagnosticLoggerHonorsFlags`

Contract notes:

- Logs may include ids, route source, operation names, durations, item counts, and sanitized stderr tails.
- Logs must not include full prompt bodies, tokens, session files, SQLite paths, `.env` paths, or unbounded output.
- Diagnostic logging is rate-limited to avoid filesystem floods during app-server loops.
- `CTR_GO_LOG_ENABLED=off` discards daemon stdout logs; `CTR_GO_DIAGNOSTIC_LOGS=off` keeps normal bot logs but suppresses structured lifecycle diagnostics.

## Session Tail Overlay Retirement

ADR: `docs/adr/ADR-013-retire-session-tail-tool-overlay.md`

Feature brief: `docs/process/v0.2.0-live-appserver-events-brief.md`

Primary tests:

- `internal/daemon/session_tail_overlay_test.go::TestPollTrackedIgnoresStaleSessionTailTool`
- `internal/appserver/normalize_test.go::TestToolSnapshotFromLiveNotificationMapsRunningCommand`
- `internal/appserver/normalize_test.go::TestCompactSnapshotStoresToolTimingOnFirstSeen`
- `internal/appserver/normalize_test.go::TestCompactSnapshotPreservesToolTimingWhenUnchanged`
- `internal/appserver/normalize_test.go::TestCompactSnapshotUpdatesToolLastUpdateWhenFingerprintChanges`
- `internal/appserver/normalize_test.go::TestCompactSnapshotDoesNotPreserveActiveLiveToolWhenThreadReadOmitsTool`
- `internal/appserver/normalize_test.go::TestCompactSnapshotDoesNotPreserveLiveToolAcrossTurns`
- `internal/appserver/normalize_test.go::TestSnapshotFromThreadReadKeepsToolOnlyTurnDetailsWithoutCommentary`
- `internal/daemon/service_test.go::TestLiveToolNotificationStoresRunningCommandWithoutRenderingItAsCurrent`
- `internal/daemon/service_test.go::TestPollSnapshotWithoutToolDoesNotPreserveSameTurnRunningToolAsCurrent`
- `internal/daemon/service_test.go::TestPollTrackedDeferredInterruptedDoesNotOverwriteFreshLiveToolSnapshot`
- `internal/daemon/service_test.go::TestPollSnapshotWithOlderCompletedToolPreservesTelegramOriginLiveCurrentTool`
- `internal/daemon/service_test.go::TestRefreshThreadForOperationTerminalCompletedToolReplacesLiveCurrent`
- `internal/daemon/observer_ui_v2_test.go::TestRenderToolPanelShowsLastCompletedToolInsteadOfRunningTool`
- `internal/daemon/observer_ui_v2_test.go::TestRenderToolPanelShowsTelegramOriginCurrentTool`
- `internal/daemon/observer_ui_v2_test.go::TestRenderToolPanelKeepsForeignRunningToolHidden`
- `internal/daemon/observer_ui_v2_test.go::TestRenderSummaryPanelShowsActiveRunElapsedTimeAtBottom`
- `internal/daemon/service_test.go::TestFinalCardShowsRunDuration`

Contract notes:

- App Server `thread/read` snapshots remain the durable source.
- App Server live item notifications may update snapshot/detail history.
- Telegram-origin turns may render current command visibility from live `item/started` and `item/updated` only after matching the marked `thread_id + turn_id`.
- Foreign GUI/CLI runs do not promise authoritative current command visibility.
- Long-running active runs render elapsed runtime in `[commentary]`; completed Final Cards render total `Run duration`.
- `[Tool]` renders the current tool only for eligible Telegram-origin active turns; otherwise it renders the last completed tool, or `No completed tool yet.` when no completed tool is available.
- While a Telegram-origin live current tool is active, older completed tool evidence from same-turn `thread/read` may update `[Output]`, but must not make `[Tool]` revert from the current command to the older completed command.
- Empty/interrupted polling snapshots must not overwrite a fresher stored live current tool for the same Telegram-origin turn.
- `[Output]` renders the last completed tool output when available.
- Session JSONL is not a live Telegram UI source.
- Missing App Server tool state renders as neutral absence, not as a guessed command.
- Session JSONL can still be used for explicit full-log export paths.

Slice gate:

- Each v0.2.0 live-event slice must add or update tests first, pass targeted checks, run the relevant live Telegram E2E case, and only then be committed.

## Telegram Notifier Acceptance

ADR: `docs/adr/ADR-022-telegram-global-completion-notifier.md`

Primary deterministic tests:

- `tests/telegram_notifier_e2e_test.go::TestTelegramNotifierAllFoldersRestartAndInputIsolation`
- `tests/telegram_notifier_e2e_test.go::TestTelegramNotifierRetryIsOnceOnlyAfterAcknowledgement`
- `tests/telegram_notifier_e2e_test.go::TestTelegramNotifierLogsContainNoConversationContent`

Contract notes:

- Installed Windows operation uses `CTR_GO_PROFILE=notifier`.
- Discovery is global across user-facing CLI/Desktop threads and does not
  require a projects file or exact-CWD registry match.
- App Server calls are read-only: after session handshake, notifier mode may use
  `thread/list` and `thread/read` only.
- Initial terminal threads are baselined without Telegram replay; observed
  active threads produce one terminal notification after completion, including
  across daemon restart.
- Telegram sends only management commands to the bridge. Plain text, replies,
  voice, attachments, and unsupported callbacks do not create Codex writes,
  downloads, transcriptions, approvals, forks, resumes, starts, or turns.
- Logs and ordinary state must not contain prompt text, final text, summaries,
  titles, full local paths, owner IDs, bot tokens, session files, SQLite paths,
  or raw App Server payloads.
- The `minimal` profile remains documented only as an explicit rollback profile
  with ADR-020/021 ownership caveats.

Focused check:

```powershell
go test ./tests -run TestTelegramNotifier -count=1
```

## Minimal bridge end-to-end acceptance

Primary deterministic test:

- `tests/telegram_minimal_e2e_test.go::TestMinimalBridgeEndToEndAcrossRestart`
- `tests/telegram_minimal_e2e_test.go::TestMinimalBridgeExistingThreadContinuation`
- `tests/telegram_minimal_e2e_test.go::TestMinimalBridgeRegisteredObserverAcrossRestart`

It owns a local fake Telegram HTTP API and fake App Server stdio process. It
covers registered-project selection, same-thread reply, full final chunking,
one failed delivery retry, restart recovery, approval callback routing, voice
preview/cancel/execute, and SQLite/log plaintext-marker scans. The registered
thread picker tests add a two-project fake contour: one selected existing
conversation must receive the next plain message without `thread/start`, and
one active thread in a different registered project must baseline before
restart and then deliver exactly one full passive final notification after
completion.

Live gate:

- `tests/live_e2e/windows_bridge_live_test.go::TestWindowsBridgeLiveAcceptance`
- `tests/live_e2e/windows_bridge_live_test.go::TestWindowsBridgeRegisteredPickerObserverLiveAcceptance`

The live gates skip unless explicitly enabled by local env. They must be run
only with an explicitly authorized private disposable contour and record only
sanitized hashes/counts in test output. Windows Phase 0 qualifies PC-origin
approval only for bridge-managed shared App Server sessions. It does not claim
native approval control for stock Desktop's separate App Server; native-session
approvals are PC-only notices without Telegram action buttons.

Contract notes:

- Minimal registered-project eligibility is exact canonical CWD equality only.
- `/start` shows registered project names, then `새 작업 시작` and `기존 대화 열기`.
- Existing-conversation pages are dynamic App Server reads, eight rows per
  Telegram page, active first, and use opaque scoped callbacks.
- Selecting an existing row fresh-reads and binds without resuming the original
  thread; Desktop/PC-origin evidence from that read is persisted so the next
  non-reply natural-language message uses the continuation-fork path. Reply
  routes remain higher precedence.
- The passive observer scans all registered projects incrementally, suppresses
  initial terminal baselines, and sends full-final-only alerts for new terminal
  transitions.

## Baseline Commands

Run before commit or publish:

```powershell
go test ./...
go build -buildvcs=false ./...
git diff --check
git grep -nE "BOT_TOKEN|TELEGRAM_BOT_TOKEN|api_hash|api_id|phone|password|secret|\\.session|\\.sqlite|\\.env|C:\\\\Users\\\\<private-user>" -- ':!go.sum' ':!.env' ':!.env.example' ':!.git'
```
