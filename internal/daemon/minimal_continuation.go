package daemon

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/alclssna33/codex_to_telegram/internal/appserver"
	"github.com/alclssna33/codex_to_telegram/internal/control"
	"github.com/alclssna33/codex_to_telegram/internal/model"
)

type continuationSession interface {
	Session
	ThreadFork(context.Context, string, control.ThreadForkOptions) (map[string]any, error)
	ThreadSetName(context.Context, string, string) (map[string]any, error)
}

type continuationStartResult struct {
	thread    *model.Thread
	turnID    string
	forked    bool
	queued    bool
	linkTitle string
}

type continuationForkError struct {
	err                 error
	failureKind         string
	noRemoteTurnStarted bool
}

type minimalWorkerPreTurnError struct {
	err error
}

const (
	minimalContinuationForkNotice        = "PC Codex가 원본 대화를 사용 중이어서, 답변 시점의 문맥을 이어받은 새 대화에서 작업을 시작했습니다."
	minimalContinuationQueueNotice       = "현재 PC Codex가 분석 중입니다. 이 작업이 끝나면 같은 문맥을 이어받아 실행합니다."
	minimalContinuationForkFailureNotice = "PC 대화를 직접 이어갈 수 없었고, 문맥을 이어받는 새 대화 생성도 실패했습니다. 다시 시도하거나 /status를 확인해주세요."
	minimalLinkedTitleSuffix             = " · 텔레그램 연동"
)

var errMinimalLinkedOwnedByCodex = errors.New("minimal linked thread is owned by Codex")

func (r continuationStartResult) threadID() string {
	if r.thread == nil {
		return ""
	}
	return r.thread.ID
}

func (e continuationForkError) Error() string {
	if e.err == nil {
		return "minimal continuation fork failed"
	}
	return e.err.Error()
}

func (e continuationForkError) Unwrap() error {
	return e.err
}

func (e minimalWorkerPreTurnError) Error() string {
	if e.err == nil {
		return "minimal worker pre-turn failure"
	}
	return e.err.Error()
}

func (e minimalWorkerPreTurnError) Unwrap() error {
	return e.err
}

func preTurnWorkerError(err error) error {
	if err == nil {
		return nil
	}
	return minimalWorkerPreTurnError{err: err}
}

func isPreTurnWorkerError(err error) bool {
	var target minimalWorkerPreTurnError
	return errors.As(err, &target)
}

func isActiveWriterConflict(err error, threadID string) bool {
	var rpcErr *appserver.RPCError
	threadID = strings.TrimSpace(threadID)
	return threadID != "" &&
		errors.As(err, &rpcErr) &&
		rpcErr.Code == -32600 &&
		strings.TrimSpace(rpcErr.Message) == "thread "+threadID+" already has an active writer"
}

func (r *Router) startResumedOrContinuationTurn(
	ctx context.Context,
	live Session,
	inbound model.InboundText,
	parent *model.Thread,
	project model.Project,
	anchor sourceTurnAnchor,
	prompt string,
) (continuationStartResult, error) {
	return r.startResumedOrLinkedTurn(ctx, inbound, parent, project, anchor, prompt)
}

func (r *Router) startResumedOrLinkedTurn(
	ctx context.Context,
	inbound model.InboundText,
	parent *model.Thread,
	project model.Project,
	anchor sourceTurnAnchor,
	prompt string,
) (continuationStartResult, error) {
	return r.startResumedOrLinkedTurnWithPolicy(ctx, inbound, parent, project, anchor, prompt, true)
}

func (r *Router) startResumedOrLinkedTurnWithPolicy(
	ctx context.Context,
	inbound model.InboundText,
	parent *model.Thread,
	project model.Project,
	anchor sourceTurnAnchor,
	prompt string,
	allowSourceLink bool,
) (continuationStartResult, error) {
	if allowSourceLink && r.shouldUseContinuationForSource(ctx, parent, anchor) {
		return r.startLinkedTurn(ctx, inbound, parent, project, anchor, prompt)
	}
	result, err := r.startExistingThreadOnWorker(ctx, inbound, parent, project, prompt)
	if err != nil {
		if !isActiveWriterConflict(err, parent.ID) {
			return continuationStartResult{}, err
		}
		if link, linkErr := r.service.store.GetMinimalLinkedThreadByLinkedID(ctx, parent.ID); linkErr != nil {
			return continuationStartResult{}, linkErr
		} else if link != nil {
			_ = r.service.store.RecordMinimalLinkedBlocked(ctx, link.LinkedThreadID, "active_writer", r.service.now())
			return continuationStartResult{thread: parent, linkTitle: link.DesiredTitle}, errMinimalLinkedOwnedByCodex
		}
		if !allowSourceLink {
			return continuationStartResult{thread: parent, linkTitle: safeContinuationSourceTitle(parent)}, errMinimalLinkedOwnedByCodex
		}
		return r.startLinkedTurn(ctx, inbound, parent, project, anchor, prompt)
	}
	return result, nil
}

func (r *Router) shouldUseContinuationForSource(ctx context.Context, parent *model.Thread, anchor sourceTurnAnchor) bool {
	if r == nil || r.service == nil || parent == nil {
		return false
	}
	sourceThreadID := strings.TrimSpace(anchor.ThreadID)
	if sourceThreadID == "" {
		sourceThreadID = strings.TrimSpace(parent.ID)
	}
	sourceTurnID := strings.TrimSpace(anchor.TurnID)
	return sourceThreadID == strings.TrimSpace(parent.ID) &&
		sourceTurnID != "" &&
		isTerminalStatus(anchor.Status) &&
		(anchor.PCOrigin || r.isPCOriginThread(ctx, sourceThreadID))
}

const minimalPCOriginThreadStatePrefix = "minimal_pc_origin_thread."

func (r *Router) markPCOriginThreadFromPayload(ctx context.Context, threadID string, payload map[string]any) error {
	if r == nil || r.service == nil {
		return nil
	}
	return r.service.markPCOriginThreadFromPayload(ctx, threadID, payload)
}

func (r *Router) isPCOriginThread(ctx context.Context, threadID string) bool {
	if r == nil || r.service == nil {
		return false
	}
	return r.service.isPCOriginThread(ctx, threadID)
}

func (s *Service) markPCOriginThreadFromPayload(ctx context.Context, threadID string, payload map[string]any) error {
	threadID = strings.TrimSpace(threadID)
	if s == nil || s.store == nil || threadID == "" || !payloadLooksPCOriginPayload(payload) {
		return nil
	}
	return s.store.SetState(ctx, minimalPCOriginThreadStatePrefix+threadID, "1")
}

func (s *Service) isPCOriginThread(ctx context.Context, threadID string) bool {
	threadID = strings.TrimSpace(threadID)
	if s == nil || s.store == nil || threadID == "" {
		return false
	}
	value, err := s.store.GetState(ctx, minimalPCOriginThreadStatePrefix+threadID)
	return err == nil && strings.TrimSpace(value) == "1"
}

func (s *Service) canResumeMinimalThreadFromBackground(ctx context.Context, threadID string) bool {
	threadID = strings.TrimSpace(threadID)
	if s == nil || s.store == nil || threadID == "" {
		return false
	}
	continuation, err := s.store.ActiveMinimalContinuationByFork(ctx, threadID)
	return err == nil && continuation != nil
}

func payloadLooksPCOriginPayload(payload map[string]any) bool {
	return payloadLooksPCOrigin(payload) || payloadLooksPCOrigin(payloadMapAny(payload["thread"]))
}

func payloadLooksPCOrigin(payload map[string]any) bool {
	if len(payload) == 0 {
		return false
	}
	source := strings.ToLower(strings.TrimSpace(payloadString(payload["source"])))
	originator := strings.ToLower(strings.TrimSpace(payloadString(payload["originator"])))
	for _, value := range []string{source, originator} {
		if value == "" {
			continue
		}
		if strings.Contains(value, "desktop") ||
			strings.Contains(value, "vscode") ||
			value == "cli" ||
			value == "exec" ||
			strings.Contains(value, "codex cli") {
			return true
		}
	}
	if sourceMap := payloadMapAny(payload["source"]); len(sourceMap) > 0 {
		for _, key := range []string{"type", "name", "originator"} {
			value := strings.ToLower(strings.TrimSpace(payloadString(sourceMap[key])))
			if strings.Contains(value, "desktop") || strings.Contains(value, "vscode") || value == "cli" || value == "exec" {
				return true
			}
		}
	}
	return false
}

func payloadMapAny(value any) map[string]any {
	typed, _ := value.(map[string]any)
	return typed
}

func (r *Router) startNewProjectOnWorker(ctx context.Context, inbound model.InboundText, project model.Project, prompt string) (continuationStartResult, error) {
	if r == nil || r.service == nil || r.service.minimalWorkers == nil {
		return continuationStartResult{}, errors.New("minimal link workers are unavailable")
	}
	worker, err := r.service.minimalWorkers.Acquire(ctx, "new:"+model.ChatKey(inbound.ChatID, inbound.TopicID), "")
	if err != nil {
		return continuationStartResult{}, err
	}
	releasePreTurn := true
	defer func() {
		if releasePreTurn {
			_ = r.releaseMinimalWorker(ctx, worker)
		}
	}()
	requestCtx, cancel := r.requestContext(ctx)
	defer cancel()
	payload, err := worker.Session.ThreadStart(requestCtx, project.CanonicalPath)
	if err != nil {
		return continuationStartResult{}, err
	}
	thread := appserver.ThreadFromPayload(payload)
	if strings.TrimSpace(thread.ID) == "" {
		return continuationStartResult{}, errors.New("app server thread/start returned no thread id")
	}
	project, err = r.service.projectRegistry.Resolve(project.ID)
	if err != nil {
		return continuationStartResult{}, err
	}
	thread.CWD = project.CanonicalPath
	thread.ProjectName = project.DisplayName
	if strings.TrimSpace(thread.Title) == "" {
		thread.Title = "New thread"
	}
	if thread.UpdatedAt == 0 {
		thread.UpdatedAt = r.service.now().UTC().Unix()
	}
	if err := r.service.minimalWorkers.BindThread(worker, thread.ID); err != nil {
		return continuationStartResult{}, err
	}
	if err := r.persistThread(ctx, thread); err != nil {
		return continuationStartResult{}, err
	}
	if err := r.service.store.SetBinding(ctx, inbound.ChatID, inbound.TopicID, thread.ID, model.BindingModeBound); err != nil {
		return continuationStartResult{}, err
	}
	turnID, err := r.startLoadedTurn(ctx, worker.Session, inbound, &thread, project, prompt)
	if strings.TrimSpace(turnID) == "" && err != nil {
		return continuationStartResult{}, err
	}
	releasePreTurn = false
	return continuationStartResult{thread: &thread, turnID: turnID}, err
}

func (r *Router) startExistingThreadOnWorker(ctx context.Context, inbound model.InboundText, thread *model.Thread, project model.Project, prompt string) (continuationStartResult, error) {
	if r == nil || r.service == nil || r.service.minimalWorkers == nil {
		return continuationStartResult{}, preTurnWorkerError(errors.New("minimal link workers are unavailable"))
	}
	if thread == nil || strings.TrimSpace(thread.ID) == "" {
		return continuationStartResult{}, preTurnWorkerError(errors.New("thread is required"))
	}
	worker, existing := r.service.minimalWorkers.ByThread(thread.ID)
	acquired := !existing
	if !existing {
		var err error
		worker, err = r.service.minimalWorkers.Acquire(ctx, "thread:"+thread.ID, thread.ID)
		if err != nil {
			return continuationStartResult{}, preTurnWorkerError(err)
		}
	}
	releasePreTurn := acquired
	defer func() {
		if releasePreTurn {
			_ = r.releaseMinimalWorker(ctx, worker)
		}
	}()
	if worker == nil || worker.Session == nil {
		return continuationStartResult{}, preTurnWorkerError(errors.New("minimal link worker is unavailable"))
	}
	if strings.TrimSpace(worker.ThreadID) != strings.TrimSpace(thread.ID) {
		return continuationStartResult{}, preTurnWorkerError(errors.New("minimal link worker thread changed"))
	}
	if acquired {
		if err := r.resumeThread(ctx, worker.Session, thread.ID, project.CanonicalPath); err != nil {
			return continuationStartResult{}, preTurnWorkerError(err)
		}
	}
	turnID, err := r.startLoadedTurn(ctx, worker.Session, inbound, thread, project, prompt)
	if strings.TrimSpace(turnID) == "" && err != nil {
		return continuationStartResult{}, err
	}
	releasePreTurn = false
	return continuationStartResult{thread: thread, turnID: turnID}, err
}

func (r *Router) startLinkedTurn(ctx context.Context, inbound model.InboundText, parent *model.Thread, project model.Project, anchor sourceTurnAnchor, prompt string) (continuationStartResult, error) {
	if parent == nil || strings.TrimSpace(parent.ID) == "" {
		return continuationStartResult{}, errors.New("source thread is required")
	}
	sourceThreadID := strings.TrimSpace(anchor.ThreadID)
	if sourceThreadID == "" {
		sourceThreadID = strings.TrimSpace(parent.ID)
	}
	unlock := r.lockThread("linked:" + model.ChatKey(inbound.ChatID, inbound.TopicID) + ":" + sourceThreadID)
	defer unlock()
	link, err := r.service.store.GetMinimalLinkedThread(ctx, inbound.ChatID, inbound.TopicID, sourceThreadID)
	if err != nil {
		return continuationStartResult{}, err
	}
	if link != nil && strings.TrimSpace(link.LinkedThreadID) != "" {
		return r.startCanonicalLinkedTurn(ctx, inbound, link, parent, project, anchor, prompt)
	}
	return r.createCanonicalLinkedTurn(ctx, inbound, parent, project, anchor, prompt)
}

func (r *Router) startContinuationAfterActiveWriterConflict(
	ctx context.Context,
	live Session,
	inbound model.InboundText,
	parent *model.Thread,
	project model.Project,
	anchor sourceTurnAnchor,
	prompt string,
) (continuationStartResult, error) {
	return r.startLinkedTurn(ctx, inbound, parent, project, anchor, prompt)
}

func (r *Router) startCanonicalLinkedTurn(ctx context.Context, inbound model.InboundText, link *model.MinimalLinkedThread, parent *model.Thread, project model.Project, anchor sourceTurnAnchor, prompt string) (continuationStartResult, error) {
	if link == nil || strings.TrimSpace(link.LinkedThreadID) == "" {
		return continuationStartResult{}, errors.New("minimal linked thread is unavailable")
	}
	if strings.TrimSpace(link.ProjectID) != "" && strings.TrimSpace(link.ProjectID) != project.ID {
		return continuationStartResult{}, errors.New("minimal linked project changed")
	}
	child, err := r.service.store.GetThread(ctx, link.LinkedThreadID)
	if err != nil {
		return continuationStartResult{}, err
	}
	if child == nil {
		child = &model.Thread{ID: link.LinkedThreadID, CWD: project.CanonicalPath, ProjectName: project.DisplayName}
	}
	childProject, canonicalCWD, err := r.projectForThread(child)
	if err != nil {
		return continuationStartResult{}, err
	}
	if childProject.ID != project.ID {
		return continuationStartResult{}, errors.New("minimal linked project changed")
	}
	child.CWD = canonicalCWD
	r.logContinuation("minimal_continuation_fork_reused", model.MinimalContinuation{
		Key: model.MinimalContinuationKey{
			ChatID:         link.ChatID,
			TopicID:        link.TopicID,
			SourceThreadID: link.SourceThreadID,
			SourceTurnID:   link.SourceAnchorTurnID,
		},
		ProjectID:    link.ProjectID,
		ForkThreadID: link.LinkedThreadID,
		Status:       model.MinimalContinuationActive,
	}, link.LinkedThreadID, anchor.Status, nil)
	if strings.TrimSpace(link.State) == model.MinimalLinkedTelegramRunning {
		if err := r.enqueuePending(ctx, model.PendingCommand{
			ThreadID: link.LinkedThreadID, SourceThreadID: sourceIDForLinked(parent, anchor), SourceTurnID: anchor.TurnID,
			ProjectID: project.ID, ChatID: inbound.ChatID, TopicID: inbound.TopicID, Prompt: prompt,
		}); err != nil {
			return continuationStartResult{}, err
		}
		if strings.TrimSpace(child.ActiveTurnID) == "" {
			child.ActiveTurnID = strings.TrimSpace(link.ActiveTurnID)
		}
		return continuationStartResult{thread: child, turnID: child.ActiveTurnID, queued: true, linkTitle: link.DesiredTitle}, nil
	}
	if strings.TrimSpace(link.State) != "" && strings.TrimSpace(link.State) != model.MinimalLinkedReady {
		return continuationStartResult{}, preStartContinuationError(errors.New("minimal linked thread is not ready"), model.MinimalContinuationFailureAmbiguous)
	}
	worker, err := r.service.minimalWorkers.Acquire(ctx, "link:"+link.LinkedThreadID, link.LinkedThreadID)
	if err != nil {
		return continuationStartResult{}, preTurnWorkerError(err)
	}
	releasePreTurn := true
	defer func() {
		if releasePreTurn {
			_ = r.releaseMinimalWorker(ctx, worker)
		}
	}()
	if err := r.resumeThread(ctx, worker.Session, link.LinkedThreadID, project.CanonicalPath); err != nil {
		if isActiveWriterConflict(err, link.LinkedThreadID) {
			_ = r.service.store.RecordMinimalLinkedBlocked(ctx, link.LinkedThreadID, "active_writer", r.service.now())
			return continuationStartResult{thread: child, linkTitle: link.DesiredTitle}, errMinimalLinkedOwnedByCodex
		}
		return continuationStartResult{}, preTurnWorkerError(err)
	}
	claimed, err := r.service.store.ClaimMinimalLinkedWorker(ctx, link.LinkedThreadID, worker.Generation)
	if err != nil {
		return continuationStartResult{}, preTurnWorkerError(err)
	}
	if !claimed {
		return continuationStartResult{}, preTurnWorkerError(errors.New("minimal linked thread worker claim failed"))
	}
	currentLink := *link
	if strings.TrimSpace(link.TitleState) == "" || strings.TrimSpace(link.TitleState) == model.MinimalLinkedTitlePending {
		var titleReady bool
		currentLink, titleReady = r.hydrateMinimalLinkedTitle(ctx, worker, currentLink, parent)
		if titleReady {
			r.nameMinimalLinkedThread(ctx, worker, currentLink)
		}
	}
	turnID, err := r.startLoadedTurn(ctx, worker.Session, inbound, child, project, prompt)
	if strings.TrimSpace(turnID) == "" && err != nil {
		_ = r.service.store.FailMinimalLinkedThread(ctx, link.LinkedThreadID, worker.Generation, model.MinimalContinuationFailureAmbiguous)
		return continuationStartResult{}, err
	}
	if strings.TrimSpace(turnID) != "" {
		releasePreTurn = false
		changed, markErr := r.service.store.MarkMinimalLinkedTurnStarted(ctx, link.LinkedThreadID, worker.Generation, turnID)
		if markErr != nil {
			return continuationStartResult{thread: child, turnID: turnID, linkTitle: currentLink.DesiredTitle}, markErr
		}
		if !changed {
			return continuationStartResult{thread: child, turnID: turnID, linkTitle: currentLink.DesiredTitle}, errors.New("minimal linked turn start marker was not updated")
		}
	}
	return continuationStartResult{thread: child, turnID: turnID, linkTitle: currentLink.DesiredTitle}, err
}

func (r *Router) createCanonicalLinkedTurn(ctx context.Context, inbound model.InboundText, parent *model.Thread, project model.Project, anchor sourceTurnAnchor, prompt string) (continuationStartResult, error) {
	preStartErr := func(err error) (continuationStartResult, error) {
		return continuationStartResult{}, preStartContinuationError(err, model.MinimalContinuationFailureAmbiguous)
	}
	sourceThreadID := sourceIDForLinked(parent, anchor)
	if sourceThreadID == "" || strings.TrimSpace(anchor.TurnID) == "" {
		return preStartErr(errors.New("minimal linked source anchor is required"))
	}
	key := model.MinimalContinuationKey{
		ChatID:         inbound.ChatID,
		TopicID:        inbound.TopicID,
		SourceThreadID: sourceThreadID,
		SourceTurnID:   anchor.TurnID,
	}
	seed := model.MinimalContinuation{Key: key, ProjectID: project.ID}
	continuation, created, err := r.service.store.ClaimMinimalContinuation(ctx, seed)
	if err != nil {
		return preStartErr(err)
	}
	if continuation == nil {
		return preStartErr(errors.New("minimal continuation is unavailable"))
	}
	r.logContinuation("minimal_continuation_fork_claimed", *continuation, "", anchor.Status, nil)
	if !created {
		child, _, err := r.loadContinuationChild(ctx, *continuation, project, anchor.Status)
		if err != nil {
			return preStartErr(err)
		}
		return r.activateLegacyContinuationOnWorker(ctx, inbound, continuation, child, parent, project, anchor, prompt)
	}

	worker, err := r.service.minimalWorkers.Acquire(ctx, "link:"+model.ChatKey(inbound.ChatID, inbound.TopicID)+":"+sourceThreadID, "")
	if err != nil {
		continuation = r.failContinuation(ctx, continuation, model.MinimalContinuationFailureAmbiguous)
		return preStartErr(err)
	}
	releasePreTurn := true
	defer func() {
		if releasePreTurn {
			_ = r.releaseMinimalWorker(ctx, worker)
		}
	}()
	requestCtx, cancel := r.requestContext(ctx)
	defer cancel()
	payload, forkErr := worker.Session.ThreadFork(requestCtx, sourceThreadID, control.ThreadForkOptions{
		CWD:        project.CanonicalPath,
		LastTurnID: anchor.TurnID,
	})
	if forkErr != nil {
		kind := minimalContinuationForkFailureKind(forkErr)
		continuation = r.failContinuation(ctx, continuation, kind)
		r.logContinuation("minimal_continuation_fork_failed", *continuation, "", anchor.Status, forkErr)
		return continuationStartResult{}, continuationForkError{err: forkErr, failureKind: kind, noRemoteTurnStarted: true}
	}
	child, err := r.sanitizeForkedContinuationChild(payload, sourceThreadID, project)
	if err != nil {
		continuation = r.failContinuation(ctx, continuation, model.MinimalContinuationFailureAmbiguous)
		r.logContinuation("minimal_continuation_fork_failed", *continuation, "", anchor.Status, err)
		return preStartErr(err)
	}
	if err := r.service.minimalWorkers.BindThread(worker, child.ID); err != nil {
		continuation = r.failContinuation(ctx, continuation, model.MinimalContinuationFailureAmbiguous)
		r.logContinuation("minimal_continuation_fork_failed", *continuation, child.ID, anchor.Status, err)
		return continuationStartResult{}, continuationForkError{err: err, failureKind: model.MinimalContinuationFailureAmbiguous, noRemoteTurnStarted: true}
	}
	link := r.minimalLinkedThreadForStart(inbound, parent, project, anchor, child.ID, worker.Generation)
	if err := r.service.store.ActivateMinimalLinkedThread(ctx, link, *continuation, child); err != nil {
		continuation = r.failContinuation(ctx, continuation, model.MinimalContinuationFailureAmbiguous)
		r.logContinuation("minimal_continuation_fork_failed", *continuation, child.ID, anchor.Status, err)
		return continuationStartResult{}, continuationForkError{err: err, failureKind: model.MinimalContinuationFailureAmbiguous, noRemoteTurnStarted: true}
	}
	continuation.Status = model.MinimalContinuationActive
	continuation.ForkThreadID = child.ID
	r.nameMinimalLinkedThread(ctx, worker, link)
	r.logContinuation("minimal_continuation_queue_rehomed", *continuation, child.ID, anchor.Status, nil)
	r.logContinuation("minimal_continuation_fork_created", *continuation, child.ID, anchor.Status, nil)
	turnID, err := r.startLoadedTurn(ctx, worker.Session, inbound, &child, project, prompt)
	if strings.TrimSpace(turnID) == "" && err != nil {
		_ = r.service.store.FailMinimalLinkedThread(ctx, child.ID, worker.Generation, model.MinimalContinuationFailureAmbiguous)
		return continuationStartResult{}, err
	}
	if strings.TrimSpace(turnID) != "" {
		releasePreTurn = false
		changed, markErr := r.service.store.MarkMinimalLinkedTurnStarted(ctx, child.ID, worker.Generation, turnID)
		if markErr != nil {
			return continuationStartResult{thread: &child, turnID: turnID, forked: true, linkTitle: link.DesiredTitle}, markErr
		}
		if !changed {
			return continuationStartResult{thread: &child, turnID: turnID, forked: true, linkTitle: link.DesiredTitle}, errors.New("minimal linked turn start marker was not updated")
		}
	}
	return continuationStartResult{thread: &child, turnID: turnID, forked: true, linkTitle: link.DesiredTitle}, err
}

func (r *Router) activateLegacyContinuationOnWorker(ctx context.Context, inbound model.InboundText, continuation *model.MinimalContinuation, child *model.Thread, parent *model.Thread, project model.Project, anchor sourceTurnAnchor, prompt string) (continuationStartResult, error) {
	if child == nil {
		return continuationStartResult{}, preStartContinuationError(errors.New("minimal continuation child is unavailable"), model.MinimalContinuationFailureAmbiguous)
	}
	if threadLooksActiveForInput(child) {
		if err := r.service.store.RehomePendingCommandsForSource(ctx, inbound.ChatID, inbound.TopicID, sourceIDForLinked(parent, anchor), anchor.TurnID, child.ID); err != nil {
			return continuationStartResult{}, preStartContinuationError(err, model.MinimalContinuationFailureAmbiguous)
		}
		if err := r.enqueuePending(ctx, model.PendingCommand{
			ThreadID: child.ID, SourceThreadID: sourceIDForLinked(parent, anchor), SourceTurnID: anchor.TurnID,
			ProjectID: project.ID, ChatID: inbound.ChatID, TopicID: inbound.TopicID, Prompt: prompt,
		}); err != nil {
			return continuationStartResult{}, preStartContinuationError(err, model.MinimalContinuationFailureAmbiguous)
		}
		return continuationStartResult{thread: child, turnID: child.ActiveTurnID, queued: true}, nil
	}
	worker, err := r.service.minimalWorkers.Acquire(ctx, "link:"+child.ID, child.ID)
	if err != nil {
		return continuationStartResult{}, err
	}
	releasePreTurn := true
	defer func() {
		if releasePreTurn {
			_ = r.releaseMinimalWorker(ctx, worker)
		}
	}()
	if err := r.resumeThread(ctx, worker.Session, child.ID, project.CanonicalPath); err != nil {
		return continuationStartResult{}, err
	}
	link := r.minimalLinkedThreadForStart(inbound, parent, project, anchor, child.ID, worker.Generation)
	if err := r.service.store.ActivateMinimalLinkedThread(ctx, link, *continuation, *child); err != nil {
		return continuationStartResult{}, preStartContinuationError(err, model.MinimalContinuationFailureAmbiguous)
	}
	r.nameMinimalLinkedThread(ctx, worker, link)
	turnID, err := r.startLoadedTurn(ctx, worker.Session, inbound, child, project, prompt)
	if strings.TrimSpace(turnID) == "" && err != nil {
		_ = r.service.store.FailMinimalLinkedThread(ctx, child.ID, worker.Generation, model.MinimalContinuationFailureAmbiguous)
		return continuationStartResult{}, err
	}
	if strings.TrimSpace(turnID) != "" {
		releasePreTurn = false
		changed, markErr := r.service.store.MarkMinimalLinkedTurnStarted(ctx, child.ID, worker.Generation, turnID)
		if markErr != nil {
			return continuationStartResult{thread: child, turnID: turnID, linkTitle: link.DesiredTitle}, markErr
		}
		if !changed {
			return continuationStartResult{thread: child, turnID: turnID, linkTitle: link.DesiredTitle}, errors.New("minimal linked turn start marker was not updated")
		}
	}
	return continuationStartResult{thread: child, turnID: turnID, linkTitle: link.DesiredTitle}, err
}

func (r *Router) minimalLinkedThreadForStart(inbound model.InboundText, parent *model.Thread, project model.Project, anchor sourceTurnAnchor, linkedID string, generation uint64) model.MinimalLinkedThread {
	sourceThreadID := sourceIDForLinked(parent, anchor)
	sourceTitle := safeContinuationSourceTitle(parent)
	if sourceTitle == "" {
		sourceTitle = "Telegram reply"
	}
	return model.MinimalLinkedThread{
		ChatKey:            model.ChatKey(inbound.ChatID, inbound.TopicID),
		ChatID:             inbound.ChatID,
		TopicID:            inbound.TopicID,
		ProjectID:          project.ID,
		SourceThreadID:     sourceThreadID,
		LinkedThreadID:     strings.TrimSpace(linkedID),
		SourceAnchorTurnID: strings.TrimSpace(anchor.TurnID),
		SourceTitle:        sourceTitle,
		DesiredTitle:       sourceTitle + minimalLinkedTitleSuffix,
		TitleState:         model.MinimalLinkedTitlePending,
		State:              model.MinimalLinkedTelegramRunning,
		WorkerGeneration:   generation,
	}
}

func sourceIDForLinked(parent *model.Thread, anchor sourceTurnAnchor) string {
	sourceThreadID := strings.TrimSpace(anchor.ThreadID)
	if sourceThreadID == "" && parent != nil {
		sourceThreadID = strings.TrimSpace(parent.ID)
	}
	return sourceThreadID
}

func (r *Router) nameMinimalLinkedThread(ctx context.Context, worker *minimalLinkWorker, link model.MinimalLinkedThread) {
	if r == nil || r.service == nil || worker == nil || worker.Session == nil {
		return
	}
	title := strings.TrimSpace(link.DesiredTitle)
	if title == "" {
		return
	}
	requestCtx, cancel := r.requestContext(ctx)
	defer cancel()
	if _, err := worker.Session.ThreadSetName(requestCtx, link.LinkedThreadID, title); err == nil {
		_, _ = r.service.store.MarkMinimalLinkedTitleSet(ctx, link.LinkedThreadID, worker.Generation)
	}
}

func (r *Router) hydrateMinimalLinkedTitle(ctx context.Context, worker *minimalLinkWorker, link model.MinimalLinkedThread, parent *model.Thread) (model.MinimalLinkedThread, bool) {
	if r == nil || r.service == nil || r.service.store == nil || worker == nil || worker.Session == nil {
		return link, false
	}
	if strings.TrimSpace(link.SourceTitle) != "" && strings.TrimSpace(link.DesiredTitle) != "" {
		return link, true
	}
	sourceTitle := strings.TrimSpace(link.SourceTitle)
	if sourceTitle == "" && parent != nil && strings.TrimSpace(parent.ID) == strings.TrimSpace(link.SourceThreadID) && hasMeaningfulMinimalThreadTitle(parent) {
		sourceTitle = safeContinuationSourceTitle(parent)
	}
	if sourceTitle == "" {
		sourceTitle = r.readMinimalLinkedSourceTitle(ctx, worker.Session, link.SourceThreadID)
	}
	if sourceTitle == "" {
		return link, false
	}
	desiredTitle := strings.TrimSpace(link.DesiredTitle)
	if desiredTitle == "" {
		desiredTitle = sourceTitle + minimalLinkedTitleSuffix
	}
	if _, err := r.service.store.HydrateMinimalLinkedTitles(ctx, link.LinkedThreadID, sourceTitle, desiredTitle); err != nil {
		return link, false
	}
	updated, err := r.service.store.GetMinimalLinkedThreadByLinkedID(ctx, link.LinkedThreadID)
	if err != nil || updated == nil {
		return link, false
	}
	if strings.TrimSpace(updated.SourceTitle) == "" || strings.TrimSpace(updated.DesiredTitle) == "" {
		return *updated, false
	}
	return *updated, true
}

func (r *Router) readMinimalLinkedSourceTitle(ctx context.Context, session continuationSession, sourceThreadID string) string {
	sourceThreadID = strings.TrimSpace(sourceThreadID)
	if r == nil || session == nil || sourceThreadID == "" {
		return ""
	}
	requestCtx, cancel := r.requestContext(ctx)
	defer cancel()
	payload, err := session.ThreadRead(requestCtx, sourceThreadID, true)
	if err != nil {
		return ""
	}
	thread := appserver.ThreadFromPayload(payload)
	if !hasMeaningfulMinimalThreadTitle(&thread) {
		return ""
	}
	return safeContinuationSourceTitle(&thread)
}

func (r *Router) releaseMinimalWorker(ctx context.Context, worker *minimalLinkWorker) error {
	if r == nil || r.service == nil || r.service.minimalWorkers == nil || worker == nil {
		return nil
	}
	manager := r.service.minimalWorkers
	if strings.TrimSpace(worker.ThreadID) != "" {
		_, err := manager.Release(ctx, worker.ThreadID, worker.Generation)
		return err
	}
	manager.mu.Lock()
	manager.removeLocked(worker)
	manager.mu.Unlock()
	manager.logEvent("minimal_link_release_started", worker, nil)
	err := manager.closeWorker(ctx, worker)
	if err != nil {
		manager.logEvent("minimal_link_release_failed", worker, err)
		return err
	}
	manager.logEvent("minimal_link_worker_closed", worker, nil)
	return nil
}

func preStartContinuationError(err error, failureKind string) error {
	if err == nil {
		return nil
	}
	var forkErr continuationForkError
	if errors.As(err, &forkErr) {
		return err
	}
	failureKind = strings.TrimSpace(failureKind)
	if failureKind == "" {
		failureKind = model.MinimalContinuationFailureAmbiguous
	}
	return continuationForkError{err: err, failureKind: failureKind, noRemoteTurnStarted: true}
}

func minimalContinuationForkFailureKind(err error) string {
	var rpcErr *appserver.RPCError
	if errors.As(err, &rpcErr) {
		switch rpcErr.Code {
		case -32700, -32600, -32601, -32602:
			return model.MinimalContinuationFailureDefinite
		default:
			return model.MinimalContinuationFailureAmbiguous
		}
	}
	return model.MinimalContinuationFailureAmbiguous
}

func (r *Router) failContinuation(ctx context.Context, continuation *model.MinimalContinuation, failureKind string) *model.MinimalContinuation {
	if continuation == nil {
		return continuation
	}
	if err := r.service.store.FailMinimalContinuation(ctx, *continuation, failureKind); err != nil {
		return continuation
	}
	continuation.Status = model.MinimalContinuationFailed
	continuation.FailureKind = failureKind
	return continuation
}

func (r *Router) loadContinuationChild(ctx context.Context, continuation model.MinimalContinuation, project model.Project, sourceStatus string) (*model.Thread, bool, error) {
	if continuation.Status == model.MinimalContinuationFailed {
		if continuation.FailureKind == model.MinimalContinuationFailureAmbiguous {
			return nil, false, continuationForkError{
				err:                 errors.New("minimal continuation is ambiguous; run /repair before retrying"),
				failureKind:         model.MinimalContinuationFailureAmbiguous,
				noRemoteTurnStarted: true,
			}
		}
		return nil, false, continuationForkError{
			err:                 errors.New("minimal continuation creation failed"),
			failureKind:         model.MinimalContinuationFailureDefinite,
			noRemoteTurnStarted: true,
		}
	}
	if continuation.Status == model.MinimalContinuationCreating {
		return nil, false, continuationForkError{
			err:                 errors.New("minimal continuation creation is already in progress"),
			failureKind:         model.MinimalContinuationFailureAmbiguous,
			noRemoteTurnStarted: true,
		}
	}
	childID := strings.TrimSpace(continuation.ForkThreadID)
	if continuation.Status != model.MinimalContinuationActive || childID == "" {
		return nil, false, preStartContinuationError(errors.New("minimal continuation row is incomplete"), model.MinimalContinuationFailureAmbiguous)
	}
	child, err := r.service.store.GetThread(ctx, childID)
	if err != nil {
		return nil, false, preStartContinuationError(err, model.MinimalContinuationFailureAmbiguous)
	}
	if child == nil || strings.TrimSpace(child.ID) == "" {
		return nil, false, preStartContinuationError(errors.New("minimal continuation child is unavailable"), model.MinimalContinuationFailureAmbiguous)
	}
	childProject, _, err := r.projectForThread(child)
	if err != nil {
		return nil, false, preStartContinuationError(err, model.MinimalContinuationFailureAmbiguous)
	}
	if childProject.ID != project.ID || continuation.ProjectID != project.ID {
		return nil, false, preStartContinuationError(errors.New("minimal continuation project changed"), model.MinimalContinuationFailureAmbiguous)
	}
	r.logContinuation("minimal_continuation_fork_reused", continuation, child.ID, sourceStatus, nil)
	return child, false, nil
}

func (r *Router) sanitizeForkedContinuationChild(payload map[string]any, parentID string, project model.Project) (model.Thread, error) {
	child := appserver.ThreadFromPayload(payload)
	child.ID = strings.TrimSpace(child.ID)
	if child.ID == "" {
		return model.Thread{}, errors.New("app server thread/fork returned no child thread id")
	}
	if child.ID == strings.TrimSpace(parentID) {
		return model.Thread{}, errors.New("app server thread/fork returned the parent thread id")
	}
	if strings.TrimSpace(child.CWD) != "" {
		canonical, err := canonicalMinimalDirectory(child.CWD)
		if err != nil {
			return model.Thread{}, fmt.Errorf("canonicalize forked child cwd: %w", err)
		}
		if canonical != project.CanonicalPath {
			return model.Thread{}, errors.New("forked child cwd is outside the exact project root")
		}
	}
	child.CWD = project.CanonicalPath
	child.ProjectName = project.DisplayName
	child.DirectoryName = ""
	child.Title = child.ID
	child.LastPreview = ""
	child.Raw = nil
	if child.UpdatedAt == 0 {
		child.UpdatedAt = r.service.now().UTC().Unix()
	}
	return child, nil
}

func safeContinuationSourceTitle(parent *model.Thread) string {
	if parent == nil {
		return ""
	}
	title := strings.TrimSpace(parent.Title)
	if title == "" || title == parent.ID {
		title = parent.ShortID()
	}
	title = strings.Map(func(r rune) rune {
		switch r {
		case '\r', '\n', '\t':
			return ' '
		default:
			return r
		}
	}, title)
	title = strings.Join(strings.Fields(title), " ")
	runes := []rune(title)
	if len(runes) > 64 {
		title = strings.TrimSpace(string(runes[:64]))
	}
	return title
}

func hasMeaningfulMinimalThreadTitle(thread *model.Thread) bool {
	if thread == nil {
		return false
	}
	title := strings.TrimSpace(thread.Title)
	return title != "" && title != strings.TrimSpace(thread.ID)
}

func continuationForkResponseText(parentID, childID string) string {
	return minimalContinuationForkNotice + "\n\n원본: " + shortLogID(parentID) + " · 이어받은 대화: " + shortLogID(childID)
}

func continuationForkFailureResponseText() string {
	return minimalContinuationForkFailureNotice
}

func minimalLinkedOwnedByCodexResponseText(title string) string {
	title = strings.TrimSpace(title)
	if title == "" {
		title = "텔레그램 연동"
	}
	return fmt.Sprintf("Codex에서 “%s” 작업을 닫은 후 다시 시도해주세요. 계속 잠겨 있으면 Codex 앱을 종료한 뒤 다시 시도해주세요.", title)
}

func (r *Router) logContinuation(event string, continuation model.MinimalContinuation, childID, sourceStatus string, cause error) {
	fields := lifecycleFields{
		"chat_hash":       shortTextHash(model.ChatKey(continuation.Key.ChatID, continuation.Key.TopicID)),
		"project_id":      continuation.ProjectID,
		"source_thread":   shortLogID(continuation.Key.SourceThreadID),
		"source_turn":     shortLogID(continuation.Key.SourceTurnID),
		"fork_thread":     shortLogID(childID),
		"status":          continuation.Status,
		"failure_kind":    continuation.FailureKind,
		"continuation_at": time.Now().UTC().Format(time.RFC3339Nano),
	}
	if strings.TrimSpace(sourceStatus) != "" {
		fields["source_status"] = sourceStatus
	}
	if cause != nil {
		fields["error_len"] = len(cause.Error())
		fields["error_sha256"] = shortTextHash(cause.Error())
		var rpcErr *appserver.RPCError
		if errors.As(cause, &rpcErr) {
			fields["rpc_code"] = rpcErr.Code
		}
	}
	r.service.logLifecycle(event, fields)
}

func shortLogID(value string) string {
	value = strings.TrimSpace(value)
	if len(value) <= 8 {
		return value
	}
	return value[:8]
}
