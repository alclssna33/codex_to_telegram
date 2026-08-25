package daemon

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/alclssna33/codex_to_telegram/internal/appserver"
	"github.com/alclssna33/codex_to_telegram/internal/control"
	"github.com/alclssna33/codex_to_telegram/internal/model"
)

type Router struct {
	service         *Service
	locks           sync.Map
	persistThread   func(context.Context, model.Thread) error
	enqueuePending  func(context.Context, model.PendingCommand) error
	completePending func(context.Context, int64) error
	failPending     func(context.Context, int64) error

	selectionLockAttemptHook func(chatID, topicID int64)
	threadLockAttemptHook    func(threadID string)
}

type ResolvedTarget struct {
	ProjectID    string
	TargetKind   string
	ThreadID     string
	SourceTurnID string
}

var (
	errSourceTurnUnavailable = errors.New("source turn is unavailable")
	errSourceTurnInProgress  = errors.New("source turn is still in progress")
)

var minimalSelectionLocks sync.Map

func newMinimalRouter(service *Service) *Router {
	return &Router{
		service:         service,
		persistThread:   service.persistThread,
		enqueuePending:  service.store.EnqueuePendingCommand,
		completePending: service.store.CompletePendingCommand,
		failPending:     service.store.FailPendingCommand,
	}
}

func (r *Router) Submit(ctx context.Context, inbound model.InboundText) (*DirectResponse, error) {
	if r == nil || r.service == nil || r.service.cfg.Profile != "minimal" {
		return nil, errors.New("minimal router is unavailable")
	}
	prompt := strings.TrimSpace(inbound.Text)
	if prompt == "" {
		return nil, errors.New("prompt is required")
	}
	unlockSelection := r.lockSelection(inbound.ChatID, inbound.TopicID)
	defer unlockSelection()
	if inbound.ReplyToMessageID != 0 {
		return r.submitReply(ctx, inbound, prompt)
	}
	return r.submitNew(ctx, inbound, prompt)
}

func (r *Router) SubmitResolved(ctx context.Context, inbound model.InboundText, target ResolvedTarget, prompt string) (*DirectResponse, error) {
	if r == nil || r.service == nil || r.service.cfg.Profile != "minimal" {
		return nil, errors.New("minimal router is unavailable")
	}
	prompt = strings.TrimSpace(prompt)
	if prompt == "" {
		return nil, errors.New("prompt is required")
	}
	unlockSelection := r.lockSelection(inbound.ChatID, inbound.TopicID)
	defer unlockSelection()
	switch target.TargetKind {
	case model.VoiceTargetNew:
		return r.submitNewProject(ctx, inbound, prompt, target.ProjectID)
	case model.VoiceTargetThread:
		return r.submitThread(ctx, inbound, prompt, target.ThreadID, target.ProjectID, target.SourceTurnID)
	default:
		return nil, errors.New("resolved target is invalid")
	}
}

func (r *Router) submitNew(ctx context.Context, inbound model.InboundText, prompt string) (*DirectResponse, error) {
	projectID, err := r.service.store.GetSelectedProject(ctx, inbound.ChatID, inbound.TopicID)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(projectID) == "" {
		return r.service.minimalProjectPicker(ctx)
	}
	bound, err := r.service.minimalBoundThreadForProject(ctx, inbound.ChatID, inbound.TopicID, projectID)
	if err != nil {
		return nil, err
	}
	if bound != nil {
		return r.submitThread(ctx, inbound, prompt, bound.ID, projectID, "")
	}
	return r.submitNewProject(ctx, inbound, prompt, projectID)
}

func (r *Router) submitNewProject(ctx context.Context, inbound model.InboundText, prompt, projectID string) (*DirectResponse, error) {
	project, err := r.service.projectRegistry.Resolve(projectID)
	if err != nil {
		return nil, err
	}
	result, err := r.startNewProjectOnWorker(ctx, inbound, project, prompt)
	if err != nil {
		return nil, err
	}
	return &DirectResponse{
		Text:     "새 작업을 시작했습니다.",
		ThreadID: result.thread.ID,
		TurnID:   result.turnID,
	}, nil
}

func (r *Router) submitReply(ctx context.Context, inbound model.InboundText, prompt string) (*DirectResponse, error) {
	route, err := r.service.store.ResolveMessageRoute(ctx, inbound.ChatID, inbound.TopicID, inbound.ReplyToMessageID)
	if err != nil {
		return nil, err
	}
	if route == nil || strings.TrimSpace(route.ThreadID) == "" {
		return nil, errors.New("reply target is not routed to a Codex thread")
	}
	return r.submitThread(ctx, inbound, prompt, route.ThreadID, "", route.TurnID)
}

func (r *Router) submitThread(ctx context.Context, inbound model.InboundText, prompt, threadID, expectedProjectID, sourceTurnID string) (*DirectResponse, error) {
	thread, err := r.service.store.GetThread(ctx, strings.TrimSpace(threadID))
	if err != nil {
		return nil, err
	}
	if thread == nil {
		return nil, errors.New("routed Codex thread is unavailable")
	}
	live, err := r.liveSession()
	if err != nil {
		return nil, err
	}
	sourceTurnID = strings.TrimSpace(sourceTurnID)
	explicitSourceTurnID := sourceTurnID
	var sourceAnchorPayload map[string]any
	var sourceErr error
	linkedRouteTranslated := false
	var linkedDecision minimalLinkedRouteDecision
	routeMayHaveLink, err := r.minimalRouteMayHaveCanonicalLink(ctx, inbound, thread.ID)
	if err != nil {
		return nil, err
	}
	if explicitSourceTurnID == "" {
		routeMayHaveLink = false
		if link, linkErr := r.service.store.GetMinimalLinkedThreadByLinkedID(ctx, thread.ID); linkErr != nil {
			return nil, linkErr
		} else if link != nil && link.ChatID == inbound.ChatID && link.TopicID == inbound.TopicID {
			routeMayHaveLink = true
		}
	}
	if routeMayHaveLink {
		sourceAnchorPayload, sourceErr = r.readSourceAnchorPayload(ctx, live, thread.ID)
		var linkedErr error
		linkedDecision, linkedErr = r.resolveMinimalLinkedRoute(ctx, inbound, thread.ID, sourceTurnID, sourceAnchorPayload)
		if linkedErr != nil {
			if errors.Is(linkedErr, errMinimalLinkedSourceDiverged) {
				return &DirectResponse{
					Text:     minimalLinkedSourceDivergedResponseText(),
					ThreadID: linkedDecision.ThreadID,
					TurnID:   linkedDecision.SourceTurnID,
				}, nil
			}
			if errors.Is(linkedErr, errSourceTurnUnavailable) {
				return &DirectResponse{
					Text:     minimalLinkedSourceUnavailableResponseText(),
					ThreadID: linkedDecision.ThreadID,
					TurnID:   linkedDecision.SourceTurnID,
				}, nil
			}
			return nil, linkedErr
		}
		if linkedDecision.ThreadID != "" && linkedDecision.ThreadID != thread.ID {
			thread, err = r.service.store.GetThread(ctx, linkedDecision.ThreadID)
			if err != nil {
				return nil, err
			}
			if thread == nil {
				return nil, errors.New("canonical linked Codex thread is unavailable")
			}
			sourceTurnID = linkedDecision.SourceTurnID
			linkedRouteTranslated = true
		}
	}
	refreshed, err := r.service.refreshThreadForOperation(ctx, live, thread.ID, "minimal_reply_route")
	if err != nil {
		return nil, err
	}
	if refreshed != nil {
		if hasMeaningfulMinimalThreadTitle(thread) {
			refreshed.Title = thread.Title
		}
		thread = refreshed
	}
	if !routeMayHaveLink {
		sourceAnchorPayload, sourceErr = r.readSourceAnchorPayload(ctx, live, thread.ID)
	}
	unlock := r.lockThread(thread.ID)
	defer unlock()
	if current, loadErr := r.service.store.GetThread(ctx, thread.ID); loadErr != nil {
		return nil, loadErr
	} else if current != nil {
		if hasMeaningfulMinimalThreadTitle(thread) {
			current.Title = thread.Title
		}
		thread = current
	}
	var canonicalLinkedForStart *model.MinimalLinkedThread
	if link, linkErr := r.service.store.GetMinimalLinkedThreadByLinkedID(ctx, thread.ID); linkErr != nil {
		return nil, linkErr
	} else if link != nil && link.ChatID == inbound.ChatID && link.TopicID == inbound.TopicID {
		canonicalLinkedForStart = link
	}
	project, canonicalCWD, err := r.projectForThread(thread)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(expectedProjectID) != "" && project.ID != strings.TrimSpace(expectedProjectID) {
		return nil, errors.New("resolved voice project no longer matches routed thread")
	}
	thread.CWD = canonicalCWD
	sourceThreadID := ""
	var anchor sourceTurnAnchor
	if canonicalLinkedForStart != nil && !linkedRouteTranslated {
		sourceThreadID = strings.TrimSpace(canonicalLinkedForStart.SourceThreadID)
		sourceTurnID = strings.TrimSpace(canonicalLinkedForStart.SourceAnchorTurnID)
		if sourceThreadID == "" || sourceTurnID == "" {
			return nil, errSourceTurnUnavailable
		}
		anchor = sourceTurnAnchor{ThreadID: sourceThreadID, TurnID: sourceTurnID, Status: "completed", PCOrigin: true}
	} else {
		var anchorErr error
		anchor, anchorErr = resolveSourceAnchor(sourceAnchorPayload, sourceTurnID)
		switch {
		case anchorErr == nil:
			sourceThreadID = anchor.ThreadID
			sourceTurnID = anchor.TurnID
		case errors.Is(anchorErr, errSourceTurnInProgress):
			if sourceTurnID == "" {
				return nil, errSourceTurnUnavailable
			}
			sourceThreadID = thread.ID
			anchor = sourceTurnAnchor{ThreadID: thread.ID, TurnID: sourceTurnID, Status: "inProgress", PCOrigin: payloadLooksPCOriginPayload(sourceAnchorPayload)}
			if isTerminalStatus(thread.Status) {
				anchor.Status = canonicalMinimalTerminalStatus(thread.Status)
				thread.ActiveTurnID = ""
				break
			}
			if sourceTurnID != strings.TrimSpace(thread.ActiveTurnID) {
				return nil, errSourceTurnUnavailable
			}
		case errors.Is(anchorErr, errSourceTurnUnavailable):
			return nil, anchorErr
		default:
			return nil, anchorErr
		}
	}
	if sourceThreadID == "" && sourceTurnID != "" {
		sourceThreadID = thread.ID
	}
	if err := r.markPCOriginThreadFromPayload(ctx, sourceThreadID, sourceAnchorPayload); err != nil {
		return nil, err
	}
	if linkedRouteTranslated {
		r.logContinuation("minimal_continuation_fork_reused", model.MinimalContinuation{
			Key: model.MinimalContinuationKey{
				ChatID:         inbound.ChatID,
				TopicID:        inbound.TopicID,
				SourceThreadID: linkedDecision.SourceThreadID,
				SourceTurnID:   linkedDecision.SourceTurnID,
			},
			ProjectID:    project.ID,
			ForkThreadID: thread.ID,
			Status:       model.MinimalContinuationActive,
		}, thread.ID, anchor.Status, nil)
	}
	if sourceErr != nil && sourceTurnID != "" {
		return nil, sourceErr
	}
	if canonicalLinkedForStart != nil {
		source := &model.Thread{
			ID:          strings.TrimSpace(canonicalLinkedForStart.SourceThreadID),
			CWD:         project.CanonicalPath,
			ProjectName: project.DisplayName,
			Title:       canonicalLinkedForStart.SourceTitle,
		}
		result, err := r.startCanonicalLinkedTurn(ctx, inbound, canonicalLinkedForStart, source, project, anchor, prompt)
		return minimalContinuationStartResponse(result, err, thread.ID, sourceTurnID)
	}
	allowSourceLink := explicitSourceTurnID != "" || canonicalLinkedForStart != nil || linkedRouteTranslated
	directPCSourceHandoff := !allowSourceLink && anchor.PCOrigin
	if !directPCSourceHandoff && threadLooksActiveForInput(thread) {
		if explicitSourceTurnID != "" && sourceTurnID != "" && isTerminalStatus(anchor.Status) {
			hasBacklog, err := r.service.store.HasPendingCommandBacklogForSource(ctx, inbound.ChatID, inbound.TopicID, sourceThreadID, sourceTurnID)
			if err != nil {
				return nil, err
			}
			if hasBacklog {
				return r.enqueueBehindSourceBacklogAndStartOldest(ctx, inbound, thread, project, sourceThreadID, sourceTurnID, prompt)
			}
		}
		if explicitSourceTurnID != "" && sourceTurnID != "" && isTerminalStatus(anchor.Status) &&
			strings.TrimSpace(thread.ActiveTurnID) != "" && strings.TrimSpace(thread.ActiveTurnID) != sourceTurnID &&
			r.service.isTelegramOriginTurn(ctx, thread.ID, thread.ActiveTurnID) {
			if err := r.enqueuePending(ctx, model.PendingCommand{
				ThreadID: thread.ID, SourceThreadID: sourceThreadID, SourceTurnID: sourceTurnID,
				ProjectID: project.ID, ChatID: inbound.ChatID, TopicID: inbound.TopicID, Prompt: prompt,
			}); err != nil {
				return nil, err
			}
			return &DirectResponse{
				Text:     "현재 작업 뒤에 실행하도록 대기열에 추가했습니다.",
				ThreadID: thread.ID,
				TurnID:   thread.ActiveTurnID,
			}, nil
		}
		if explicitSourceTurnID != "" && sourceTurnID != "" && isTerminalStatus(anchor.Status) {
			if r.shouldUseContinuationForSource(ctx, thread, anchor) {
				result, err := r.startContinuationAfterActiveWriterConflict(ctx, live, inbound, thread, project, anchor, prompt)
				if err != nil {
					if errors.Is(err, errMinimalLinkedOwnedByCodex) {
						return &DirectResponse{Text: minimalLinkedOwnedByCodexResponseText(result.linkTitle), ThreadID: result.threadID(), TurnID: result.turnID}, nil
					}
					if isContinuationForkFailure(err) {
						return &DirectResponse{Text: continuationForkFailureResponseText(), ThreadID: thread.ID, TurnID: sourceTurnID}, nil
					}
					return nil, err
				}
				if result.queued {
					return &DirectResponse{
						Text:     "현재 작업 뒤에 실행하도록 대기열에 추가했습니다.",
						ThreadID: result.thread.ID,
						TurnID:   result.turnID,
					}, nil
				}
				text := "작업을 시작했습니다."
				if result.forked {
					text = continuationForkResponseText(thread.ID, result.thread.ID)
				}
				return &DirectResponse{
					Text:     text,
					ThreadID: result.thread.ID,
					TurnID:   result.turnID,
				}, nil
			}
			result, err := r.startResumedOrLinkedTurn(ctx, inbound, thread, project, anchor, prompt)
			if err != nil {
				if errors.Is(err, errMinimalLinkedOwnedByCodex) {
					return &DirectResponse{Text: minimalLinkedOwnedByCodexResponseText(result.linkTitle), ThreadID: result.threadID(), TurnID: result.turnID}, nil
				}
				if isContinuationForkFailure(err) {
					return &DirectResponse{Text: continuationForkFailureResponseText(), ThreadID: thread.ID, TurnID: sourceTurnID}, nil
				}
				return nil, err
			}
			if result.queued {
				return &DirectResponse{
					Text:     "현재 작업 뒤에 실행하도록 대기열에 추가했습니다.",
					ThreadID: result.thread.ID,
					TurnID:   result.turnID,
				}, nil
			}
			text := "작업을 시작했습니다."
			if result.forked {
				text = continuationForkResponseText(thread.ID, result.thread.ID)
			}
			return &DirectResponse{
				Text:     text,
				ThreadID: result.thread.ID,
				TurnID:   result.turnID,
			}, nil
		}
		if err := r.enqueuePending(ctx, model.PendingCommand{
			ThreadID: thread.ID, SourceThreadID: sourceThreadID, SourceTurnID: sourceTurnID,
			ProjectID: project.ID, ChatID: inbound.ChatID, TopicID: inbound.TopicID, Prompt: prompt,
		}); err != nil {
			return nil, err
		}
		return &DirectResponse{
			Text:     r.activeQueueResponseText(ctx, sourceThreadID, sourceTurnID, thread.ActiveTurnID),
			ThreadID: thread.ID,
			TurnID:   thread.ActiveTurnID,
		}, nil
	}
	result, err := r.startResumedOrLinkedTurnWithPolicy(ctx, inbound, thread, project, anchor, prompt, allowSourceLink)
	if err != nil {
		if errors.Is(err, errMinimalLinkedOwnedByCodex) {
			return &DirectResponse{Text: minimalLinkedOwnedByCodexResponseText(result.linkTitle), ThreadID: result.threadID(), TurnID: result.turnID}, nil
		}
		if isContinuationForkFailure(err) {
			return &DirectResponse{Text: continuationForkFailureResponseText(), ThreadID: thread.ID, TurnID: sourceTurnID}, nil
		}
		return nil, err
	}
	if result.queued {
		return &DirectResponse{
			Text:     "현재 작업 뒤에 실행하도록 대기열에 추가했습니다.",
			ThreadID: result.thread.ID,
			TurnID:   result.turnID,
		}, nil
	}
	text := "작업을 시작했습니다."
	if result.forked {
		text = continuationForkResponseText(thread.ID, result.thread.ID)
	}
	return &DirectResponse{
		Text:     text,
		ThreadID: result.thread.ID,
		TurnID:   result.turnID,
	}, nil
}

func (r *Router) enqueueBehindSourceBacklogAndStartOldest(ctx context.Context, inbound model.InboundText, thread *model.Thread, project model.Project, sourceThreadID, sourceTurnID, prompt string) (*DirectResponse, error) {
	if err := r.enqueuePending(ctx, model.PendingCommand{
		ThreadID: thread.ID, SourceThreadID: sourceThreadID, SourceTurnID: sourceTurnID,
		ProjectID: project.ID, ChatID: inbound.ChatID, TopicID: inbound.TopicID, Prompt: prompt,
	}); err != nil {
		return nil, err
	}
	command, claimErr := r.service.store.ClaimPendingCommandForSource(ctx, inbound.ChatID, inbound.TopicID, sourceThreadID, sourceTurnID)
	if claimErr != nil {
		return nil, claimErr
	}
	if command == nil {
		return &DirectResponse{
			Text:     "현재 작업 뒤에 실행하도록 대기열에 추가했습니다.",
			ThreadID: thread.ID,
			TurnID:   thread.ActiveTurnID,
		}, nil
	}
	startThread := thread
	startProject := project
	unlockStartThread := func() {}
	if strings.TrimSpace(command.ThreadID) != thread.ID {
		loaded, err := r.service.store.GetThread(ctx, command.ThreadID)
		if err != nil {
			_ = r.releaseClaimedCommand(ctx, command.ID)
			return nil, err
		}
		if loaded == nil {
			_ = r.releaseClaimedCommand(ctx, command.ID)
			return nil, errors.New("queued thread is unavailable")
		}
		unlockStartThread = r.lockThread(loaded.ID)
		defer unlockStartThread()
		startThread = loaded
		loadedProject, canonicalCWD, err := r.projectForThread(startThread)
		if err != nil {
			_ = r.releaseClaimedCommand(ctx, command.ID)
			return nil, err
		}
		if loadedProject.ID != command.ProjectID {
			_ = r.releaseClaimedCommand(ctx, command.ID)
			return nil, errors.New("queued project no longer matches routed thread")
		}
		startThread.CWD = canonicalCWD
		startProject = loadedProject
	}
	turnID, startErr := r.startQueuedCommand(ctx, inboundFromPendingCommand(command), startThread, startProject, command)
	if strings.TrimSpace(turnID) != "" && startErr != nil {
		return nil, fmt.Errorf("persist remotely started turn: %w", startErr)
	}
	if startErr != nil || strings.TrimSpace(turnID) == "" {
		if isQueuedNoRemoteStartError(startErr) {
			if err := r.releaseClaimedCommand(ctx, command.ID); err != nil {
				return nil, err
			}
			return &DirectResponse{
				Text:     "현재 작업 뒤에 실행하도록 대기열에 추가했습니다.",
				ThreadID: thread.ID,
				TurnID:   thread.ActiveTurnID,
			}, nil
		}
		if errors.Is(startErr, errMinimalLinkedOwnedByCodex) {
			if err := r.releaseClaimedCommand(ctx, command.ID); err != nil {
				return nil, err
			}
			return &DirectResponse{Text: minimalLinkedOwnedByCodexResponseText(""), ThreadID: thread.ID, TurnID: sourceTurnID}, nil
		}
		var forkErr continuationForkError
		if errors.As(startErr, &forkErr) && forkErr.noRemoteTurnStarted {
			if err := r.releaseClaimedCommand(ctx, command.ID); err != nil {
				return nil, err
			}
			return &DirectResponse{Text: continuationForkFailureResponseText(), ThreadID: thread.ID, TurnID: sourceTurnID}, nil
		}
		if err := r.failClaimedCommand(ctx, command.ID); err != nil {
			return nil, err
		}
		r.notifyFailedStart(ctx, command)
		return &DirectResponse{
			Text:     "현재 작업 뒤에 실행하도록 대기열에 추가했습니다.",
			ThreadID: thread.ID,
			TurnID:   thread.ActiveTurnID,
		}, nil
	}
	if err := r.completePending(ctx, command.ID); err != nil {
		return nil, err
	}
	return &DirectResponse{
		Text:     "현재 작업 뒤에 실행하도록 대기열에 추가했습니다.",
		ThreadID: startThread.ID,
		TurnID:   turnID,
	}, nil
}

func (r *Router) activeQueueResponseText(ctx context.Context, sourceThreadID, sourceTurnID, activeTurnID string) string {
	sourceThreadID = strings.TrimSpace(sourceThreadID)
	sourceTurnID = strings.TrimSpace(sourceTurnID)
	activeTurnID = strings.TrimSpace(activeTurnID)
	if sourceThreadID != "" && sourceTurnID != "" && sourceTurnID == activeTurnID && !r.service.isTelegramOriginTurn(ctx, sourceThreadID, sourceTurnID) {
		return minimalContinuationQueueNotice
	}
	return "현재 작업 뒤에 실행하도록 대기열에 추가했습니다."
}

func isContinuationForkFailure(err error) bool {
	var forkErr continuationForkError
	return errors.As(err, &forkErr)
}

func minimalContinuationStartResponse(result continuationStartResult, err error, fallbackThreadID, fallbackTurnID string) (*DirectResponse, error) {
	if err != nil {
		if errors.Is(err, errMinimalLinkedOwnedByCodex) {
			return &DirectResponse{Text: minimalLinkedOwnedByCodexResponseText(result.linkTitle), ThreadID: result.threadID(), TurnID: result.turnID}, nil
		}
		if isContinuationForkFailure(err) {
			return &DirectResponse{Text: continuationForkFailureResponseText(), ThreadID: fallbackThreadID, TurnID: fallbackTurnID}, nil
		}
		return nil, err
	}
	if result.queued {
		return &DirectResponse{
			Text:     "현재 작업 뒤에 실행하도록 대기열에 추가했습니다.",
			ThreadID: result.thread.ID,
			TurnID:   result.turnID,
		}, nil
	}
	text := "작업을 시작했습니다."
	if result.forked {
		text = continuationForkResponseText(fallbackThreadID, result.thread.ID)
	}
	return &DirectResponse{
		Text:     text,
		ThreadID: result.thread.ID,
		TurnID:   result.turnID,
	}, nil
}

type minimalQueuedNoRemoteStartError struct {
	err error
}

func (e minimalQueuedNoRemoteStartError) Error() string {
	if e.err == nil {
		return "minimal queued command deferred before remote start"
	}
	return e.err.Error()
}

func (e minimalQueuedNoRemoteStartError) Unwrap() error {
	return e.err
}

func queuedNoRemoteStartError(err error) error {
	if err == nil {
		err = errors.New("minimal queued command deferred before remote start")
	}
	return minimalQueuedNoRemoteStartError{err: err}
}

func isQueuedNoRemoteStartError(err error) bool {
	var target minimalQueuedNoRemoteStartError
	return errors.As(err, &target)
}

func (r *Router) readSourceAnchorPayload(ctx context.Context, live Session, threadID string) (map[string]any, error) {
	if live == nil || strings.TrimSpace(threadID) == "" {
		return nil, nil
	}
	requestCtx, cancel := r.requestContext(ctx)
	defer cancel()
	return live.ThreadRead(requestCtx, threadID, true)
}

type sourceTurnAnchor struct {
	ThreadID string
	TurnID   string
	Status   string
	PCOrigin bool
}

func minimalSourceTurnStatus(value any) string {
	if status, ok := value.(map[string]any); ok {
		return canonicalMinimalTerminalStatus(payloadMapString(status, "type"))
	}
	return canonicalMinimalTerminalStatus(payloadString(value))
}

func resolveSourceAnchor(payload map[string]any, requestedTurnID string) (sourceTurnAnchor, error) {
	thread := appserver.ThreadFromPayload(payload)
	nested := payload
	if value, ok := payload["thread"].(map[string]any); ok {
		nested = value
	}
	turns, _ := nested["turns"].([]any)
	requestedTurnID = strings.TrimSpace(requestedTurnID)
	pcOrigin := payloadLooksPCOriginPayload(payload)
	for i := len(turns) - 1; i >= 0; i-- {
		turn, _ := turns[i].(map[string]any)
		id := strings.TrimSpace(payloadString(turn["id"]))
		status := minimalSourceTurnStatus(turn["status"])
		if requestedTurnID == "" && isTerminalStatus(status) {
			if id == "" {
				return sourceTurnAnchor{}, errSourceTurnUnavailable
			}
			return sourceTurnAnchor{ThreadID: thread.ID, TurnID: id, Status: status, PCOrigin: pcOrigin}, nil
		}
		if id != "" && id == requestedTurnID {
			if !isTerminalStatus(status) {
				return sourceTurnAnchor{}, errSourceTurnInProgress
			}
			return sourceTurnAnchor{ThreadID: thread.ID, TurnID: id, Status: status, PCOrigin: pcOrigin}, nil
		}
	}
	return sourceTurnAnchor{}, errSourceTurnUnavailable
}

func (r *Router) DrainNext(ctx context.Context, threadID string) error {
	if r == nil || r.service == nil || r.service.cfg.Profile != "minimal" {
		return nil
	}
	threadID = strings.TrimSpace(threadID)
	if threadID == "" {
		return errors.New("thread id is required")
	}
	unlock := r.lockThread(threadID)
	defer unlock()
	return r.drainNextLocked(ctx, threadID)
}

func (r *Router) drainNextLocked(ctx context.Context, threadID string) error {
	thread, err := r.service.store.GetThread(ctx, threadID)
	if err != nil {
		return err
	}
	if thread == nil || threadLooksActiveForInput(thread) {
		return nil
	}
	for {
		command, claimErr := r.service.store.ClaimPendingCommand(ctx, threadID)
		if command == nil {
			return claimErr
		}
		if claimErr != nil {
			finalizeErr := r.failClaimedCommand(ctx, command.ID)
			r.notifyFailedStart(ctx, command)
			if finalizeErr != nil {
				return finalizeErr
			}
			continue
		}
		project, canonicalCWD, projectErr := r.projectForThread(thread)
		if projectErr == nil && project.ID != command.ProjectID {
			projectErr = errors.New("queued project no longer matches routed thread")
		}
		if projectErr != nil {
			finalizeErr := r.failClaimedCommand(ctx, command.ID)
			r.notifyFailedStart(ctx, command)
			if finalizeErr != nil {
				return finalizeErr
			}
			continue
		}
		thread.CWD = canonicalCWD
		turnID, startErr := r.startQueuedCommand(ctx, inboundFromPendingCommand(command), thread, project, command)
		if strings.TrimSpace(turnID) != "" && startErr != nil {
			return fmt.Errorf("persist remotely started turn: %w", startErr)
		}
		if startErr != nil || strings.TrimSpace(turnID) == "" {
			if isQueuedNoRemoteStartError(startErr) {
				return r.releaseClaimedCommand(ctx, command.ID)
			}
			if errors.Is(startErr, errMinimalLinkedOwnedByCodex) {
				return r.releaseClaimedCommand(ctx, command.ID)
			}
			var forkErr continuationForkError
			if errors.As(startErr, &forkErr) && forkErr.noRemoteTurnStarted {
				return r.releaseClaimedCommand(ctx, command.ID)
			}
			finalizeErr := r.failClaimedCommand(ctx, command.ID)
			r.notifyFailedStart(ctx, command)
			if finalizeErr != nil {
				return finalizeErr
			}
			continue
		}
		if err := r.completePending(ctx, command.ID); err != nil {
			return err
		}
		return nil
	}
}

func inboundFromPendingCommand(command *model.PendingCommand) model.InboundText {
	if command == nil {
		return model.InboundText{}
	}
	return model.InboundText{ChatID: command.ChatID, TopicID: command.TopicID}
}

func (r *Router) startQueuedCommand(ctx context.Context, inbound model.InboundText, thread *model.Thread, project model.Project, command *model.PendingCommand) (string, error) {
	if command == nil {
		return "", errors.New("pending command is required")
	}
	if thread != nil && strings.TrimSpace(command.SourceThreadID) != "" && strings.TrimSpace(command.SourceTurnID) != "" &&
		strings.TrimSpace(command.SourceThreadID) != strings.TrimSpace(thread.ID) {
		link, err := r.service.store.GetMinimalLinkedThreadByLinkedID(ctx, thread.ID)
		if err != nil {
			return "", err
		}
		if link != nil &&
			link.ChatID == command.ChatID &&
			link.TopicID == command.TopicID &&
			strings.TrimSpace(link.ProjectID) == project.ID &&
			strings.TrimSpace(link.SourceThreadID) == strings.TrimSpace(command.SourceThreadID) {
			switch strings.TrimSpace(link.State) {
			case model.MinimalLinkedTelegramRunning, model.MinimalLinkedReleasePending:
				return "", queuedNoRemoteStartError(errors.New("minimal linked thread is not ready"))
			case "", model.MinimalLinkedReady:
			default:
				return "", errors.New("minimal linked thread is not ready")
			}
			source := &model.Thread{
				ID:          strings.TrimSpace(link.SourceThreadID),
				CWD:         project.CanonicalPath,
				ProjectName: project.DisplayName,
				Title:       link.SourceTitle,
			}
			anchor := sourceTurnAnchor{ThreadID: source.ID, TurnID: strings.TrimSpace(command.SourceTurnID), Status: "completed", PCOrigin: true}
			result, err := r.startCanonicalLinkedTurn(ctx, inbound, link, source, project, anchor, command.Prompt)
			if result.thread != nil {
				*thread = *result.thread
			}
			if err != nil {
				if strings.TrimSpace(result.turnID) == "" && isPreTurnWorkerError(err) {
					return result.turnID, queuedNoRemoteStartError(err)
				}
				return result.turnID, err
			}
			return result.turnID, nil
		}
	}
	if strings.TrimSpace(command.SourceThreadID) == thread.ID && strings.TrimSpace(command.SourceTurnID) != "" {
		anchor := sourceTurnAnchor{ThreadID: command.SourceThreadID, TurnID: command.SourceTurnID, Status: "completed"}
		result, err := r.startResumedOrLinkedTurn(ctx, inbound, thread, project, anchor, command.Prompt)
		if result.thread != nil {
			*thread = *result.thread
		}
		if err != nil {
			if strings.TrimSpace(result.turnID) == "" && isPreTurnWorkerError(err) {
				return result.turnID, preStartContinuationError(err, model.MinimalContinuationFailureAmbiguous)
			}
			return result.turnID, err
		}
		return result.turnID, nil
	}
	result, err := r.startExistingThreadOnWorker(ctx, inbound, thread, project, command.Prompt)
	if result.thread != nil {
		*thread = *result.thread
	}
	if err != nil && strings.TrimSpace(result.turnID) == "" && isPreTurnWorkerError(err) {
		return result.turnID, preStartContinuationError(err, model.MinimalContinuationFailureAmbiguous)
	}
	return result.turnID, err
}

func (r *Router) failClaimedCommand(ctx context.Context, commandID int64) error {
	if err := r.failPending(ctx, commandID); err != nil {
		return fmt.Errorf("finalize failed pending command: %w", err)
	}
	return nil
}

func (r *Router) releaseClaimedCommand(ctx context.Context, commandID int64) error {
	if err := r.service.store.ReleaseClaimedPendingCommand(ctx, commandID); err != nil {
		return fmt.Errorf("release pending command for retry: %w", err)
	}
	return nil
}

func (r *Router) lockThread(threadID string) func() {
	threadID = strings.TrimSpace(threadID)
	if r.threadLockAttemptHook != nil {
		r.threadLockAttemptHook(threadID)
	}
	lockValue, _ := r.locks.LoadOrStore(threadID, &sync.Mutex{})
	lock := lockValue.(*sync.Mutex)
	lock.Lock()
	return lock.Unlock
}

func minimalSelectionLock(chatID, topicID int64) func() {
	lockValue, _ := minimalSelectionLocks.LoadOrStore(fmt.Sprintf("%d:%d", chatID, topicID), &sync.Mutex{})
	lock := lockValue.(*sync.Mutex)
	lock.Lock()
	return lock.Unlock
}

func (r *Router) lockSelection(chatID, topicID int64) func() {
	if r.selectionLockAttemptHook != nil {
		r.selectionLockAttemptHook(chatID, topicID)
	}
	return minimalSelectionLock(chatID, topicID)
}

func (r *Router) resumeThread(ctx context.Context, live Session, threadID, cwd string) error {
	requestCtx, cancel := r.requestContext(ctx)
	defer cancel()
	_, err := live.ThreadResume(requestCtx, threadID, cwd)
	return err
}

func (r *Router) startLoadedTurn(ctx context.Context, live Session, inbound model.InboundText, thread *model.Thread, project model.Project, prompt string) (string, error) {
	currentProject, canonicalCWD, err := r.projectForThread(thread)
	if err != nil {
		return "", err
	}
	if currentProject.ID != project.ID {
		return "", errors.New("registered project changed before turn start")
	}
	project, err = r.service.projectRegistry.Resolve(project.ID)
	if err != nil {
		return "", err
	}
	requestCtx, cancel := r.requestContext(ctx)
	defer cancel()
	options := r.service.turnStartOptions(ctx, "", thread)
	options.ApprovalPolicy = "on-request"
	options.SandboxPolicy = control.SandboxPolicy{
		Type:          "workspaceWrite",
		WritableRoots: []string{project.CanonicalPath},
		NetworkAccess: false,
	}
	payload, err := live.TurnStart(requestCtx, thread.ID, prompt, canonicalCWD, options)
	if err != nil {
		return "", err
	}
	turnID := strings.TrimSpace(appserverThreadTurnID(payload))
	if turnID == "" {
		return "", errors.New("app server turn/start returned no turn id")
	}
	thread.CWD = canonicalCWD
	thread.ProjectName = project.DisplayName
	thread.Status = "inProgress"
	thread.ActiveTurnID = turnID
	thread.UpdatedAt = r.service.now().UTC().Unix()
	if err := r.persistThread(ctx, *thread); err != nil {
		return turnID, err
	}
	_ = r.service.markTelegramOriginTurnFromTelegram(ctx, thread.ID, turnID, inbound.ChatID, inbound.TopicID)
	r.service.ensureStartedTurnSnapshot(ctx, thread, turnID)
	return turnID, nil
}

func (r *Router) projectForThread(thread *model.Thread) (model.Project, string, error) {
	if thread == nil {
		return model.Project{}, "", errors.New("thread is required")
	}
	canonicalCWD, err := canonicalMinimalDirectory(thread.CWD)
	if err != nil {
		return model.Project{}, "", fmt.Errorf("canonicalize routed thread cwd: %w", err)
	}
	project, ok := r.service.projectRegistry.MatchCWD(canonicalCWD)
	if !ok {
		return model.Project{}, "", errors.New("routed thread cwd is outside the fixed project registry")
	}
	project, err = r.service.projectRegistry.Resolve(project.ID)
	if err != nil {
		return model.Project{}, "", err
	}
	return project, canonicalCWD, nil
}

func canonicalMinimalDirectory(value string) (string, error) {
	absolute, err := filepath.Abs(filepath.Clean(strings.TrimSpace(value)))
	if err != nil {
		return "", err
	}
	canonical, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(canonical)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", errors.New("path is not a directory")
	}
	return filepath.Clean(canonical), nil
}

func (r *Router) liveSession() (Session, error) {
	r.service.mu.RLock()
	live := r.service.live
	connected := r.service.liveConnected
	r.service.mu.RUnlock()
	if !connected || live == nil {
		return nil, errors.New("live app-server session is not ready")
	}
	return live, nil
}

func (r *Router) requestContext(ctx context.Context) (context.Context, context.CancelFunc) {
	timeout := r.service.cfg.RequestTimeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	return context.WithTimeout(ctx, timeout)
}

func (r *Router) notifyFailedStart(ctx context.Context, command *model.PendingCommand) {
	r.service.mu.RLock()
	sender := r.service.sender
	r.service.mu.RUnlock()
	if sender == nil {
		return
	}
	target, configured, err := r.service.store.GetGlobalObserverTarget(ctx)
	if err != nil {
		return
	}
	chatID, topicID := int64(0), int64(0)
	if configured && target != nil && target.Enabled {
		chatID, topicID = target.ChatID, target.TopicID
	} else if command != nil {
		chatID, topicID = command.ChatID, command.TopicID
	}
	if chatID == 0 {
		return
	}
	_, _ = sender.SendMessage(ctx, chatID, topicID, "대기 중인 작업을 시작하지 못했습니다.", nil, silentSendOptions())
}
