package daemon

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/alclssna33/codex_to_telegram/internal/appserver"
	"github.com/alclssna33/codex_to_telegram/internal/model"
	"github.com/alclssna33/codex_to_telegram/internal/storage"
)

var (
	ErrCallbackConsumed             = errors.New("callback route is consumed")
	ErrCallbackMismatch             = errors.New("callback route does not match this message")
	ErrApprovalTransportUnavailable = errors.New("approval transport is definitely unavailable")
)

const (
	minimalCommandApprovalKind           = "item/commandExecution/requestApproval"
	minimalFileApprovalKind              = "item/fileChange/requestApproval"
	minimalApprovalQueueKind             = "minimal_approval"
	minimalApprovalInactiveEditQueueKind = "minimal_approval_inactive_edit"
	minimalApprovalPCCleanupQueueKind    = storage.MinimalApprovalPCCleanupQueueKind
	minimalPollOnlyApprovalEventPrefix   = "minimal-pc-approval:"
	minimalApprovalSummaryRunes          = 300
)

type exactMinimalApprovalResponder interface {
	RespondServerRequestExact(ctx context.Context, requestID, threadID, turnID, requestKind string, result map[string]any) error
}

func (s *Service) handleMinimalApprovalServerRequest(ctx context.Context, live Session, sessionIdentity string, event appserver.Event) bool {
	if s.cfg.Profile != "minimal" || event.Channel != "server_request" {
		return false
	}
	method := strings.TrimSpace(event.Method)
	if strings.Contains(strings.ToLower(method), "requestuserinput") {
		return false
	}
	if method != minimalCommandApprovalKind && method != minimalFileApprovalKind {
		return strings.Contains(strings.ToLower(method), "approval")
	}
	wireRequestID := minimalRPCID(event.ID)
	threadID := strings.TrimSpace(payloadMapString(event.Params, "threadId"))
	turnID := strings.TrimSpace(payloadMapString(event.Params, "turnId"))
	sessionIdentity = strings.TrimSpace(sessionIdentity)
	requestID := wireRequestID
	storedWireRequestID := ""
	if minimalWorkerCallbackIdentity(sessionIdentity) {
		requestID, wireRequestID = model.NormalizeRequestIdentity(sessionIdentity, wireRequestID, wireRequestID)
		storedWireRequestID = wireRequestID
	}
	if requestID == "" || wireRequestID == "" || threadID == "" || turnID == "" || sessionIdentity == "" || live == nil {
		return true
	}

	requestCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	payload, err := live.ThreadRead(requestCtx, threadID, true)
	if err != nil || payload == nil {
		return true
	}
	snapshot := appserver.SnapshotFromThreadRead(payload)
	if strings.TrimSpace(snapshot.Thread.ID) != threadID || strings.TrimSpace(snapshot.LatestTurnID) != turnID {
		return true
	}
	snapshot.WaitingOnApproval = true
	project, ok := s.projectRegistry.MatchExactCWD(snapshot.Thread.CWD)
	if !ok {
		return true
	}
	target, err := s.currentBackgroundTarget(ctx)
	if err != nil || target == nil || !target.Enabled {
		return true
	}
	approveToken, err := newMinimalApprovalToken()
	if err != nil {
		return true
	}
	denyToken, err := newMinimalApprovalToken()
	if err != nil {
		return true
	}
	requestLabel, summary := minimalApprovalPresentation(method, event.Params)
	text := strings.Join([]string{
		"프로젝트: " + project.DisplayName,
		"요청: " + requestLabel,
		"요약: " + summary,
	}, "\n")
	deliveryEventID := minimalApprovalEventID(requestID, threadID, turnID, method)
	deliveryPayload := model.DeliveryPayload{
		Text:     text,
		ThreadID: threadID,
		TurnID:   turnID,
		EventID:  requestID,
		Buttons: [][]model.ButtonSpec{{
			{Text: "승인", CallbackData: approveToken},
			{Text: "거부", CallbackData: denyToken},
		}},
	}
	_, createErr := s.store.CreateMinimalApproval(ctx, storage.MinimalApprovalSeed{
		Approval: storage.MinimalApproval{
			RequestID:       requestID,
			WireRequestID:   storedWireRequestID,
			ThreadID:        threadID,
			TurnID:          turnID,
			RequestKind:     method,
			ProjectName:     project.DisplayName,
			SessionIdentity: sessionIdentity,
			Status:          "pending",
		},
		ApproveToken: approveToken,
		DenyToken:    denyToken,
		Delivery: model.DeliveryQueueItem{
			EventID:     deliveryEventID,
			ChatKey:     model.ChatKey(target.ChatID, target.TopicID),
			ChatID:      target.ChatID,
			TopicID:     target.TopicID,
			ThreadID:    threadID,
			Kind:        minimalApprovalQueueKind,
			Status:      model.DeliveryStatusPending,
			AvailableAt: model.NowString(),
			PayloadJSON: storage.MustJSON(deliveryPayload),
			CreatedAt:   model.NowString(),
			UpdatedAt:   model.NowString(),
		},
		SupersedeDeliveryEventID: minimalPollOnlyApprovalEventID(threadID, turnID),
	})
	if createErr == nil {
		s.rememberBridgeOwnedApprovalSnapshot(ctx, snapshot)
	}
	return true
}

func (s *Service) rememberBridgeOwnedApprovalSnapshot(ctx context.Context, snapshot appserver.ThreadReadSnapshot) {
	threadID := strings.TrimSpace(snapshot.Thread.ID)
	if threadID == "" || strings.TrimSpace(snapshot.LatestTurnID) == "" || !snapshot.WaitingOnApproval {
		return
	}
	previous, err := s.store.GetSnapshot(ctx, threadID)
	if err != nil {
		return
	}
	next := appserver.CompactSnapshot(previous, s.snapshotForPersistence(snapshot), s.now())
	next.NextPollAfter = model.TimeString(s.now().UTC().Add(s.cfg.ObserverPollInterval).Format(time.RFC3339Nano))
	_ = s.store.UpsertSnapshot(ctx, threadID, next)
}

func (s *Service) enqueueMinimalPollOnlyApprovalNotice(ctx context.Context, snapshot appserver.ThreadReadSnapshot, previous *model.ThreadSnapshotState) {
	threadID := strings.TrimSpace(snapshot.Thread.ID)
	turnID := strings.TrimSpace(snapshot.LatestTurnID)
	if s.cfg.Profile != "minimal" || !snapshot.WaitingOnApproval || threadID == "" || turnID == "" || s.projectRegistry == nil {
		return
	}
	if previous != nil && strings.TrimSpace(previous.LastApprovalFP) == turnID {
		return
	}
	project, ok := s.projectRegistry.MatchExactCWD(snapshot.Thread.CWD)
	if !ok {
		return
	}
	text := strings.Join([]string{
		"프로젝트: " + project.DisplayName,
		"승인 대기 중",
		"이 요청은 PC에서 직접 승인하거나 거부해야 합니다.",
		"Thread: " + snapshot.Thread.ShortID(),
	}, "\n")
	s.enqueueRenderedToBackgroundTargets(ctx, &DirectResponse{Text: text}, threadID, turnID, "", minimalPollOnlyApprovalEventID(threadID, turnID))
}

func (s *Service) handleMinimalApprovalCallback(ctx context.Context, chatID, topicID, messageID int64, existing *storage.MinimalApprovalRoute) (*DirectResponse, error) {
	if existing == nil || existing.Status != "active" {
		return nil, ErrCallbackConsumed
	}
	sessionIdentity := strings.TrimSpace(existing.SessionIdentity)
	route, claimed, err := s.store.ClaimMinimalApproval(ctx, existing.Token, chatID, topicID, messageID, sessionIdentity)
	if err != nil {
		return nil, err
	}
	if !claimed {
		if route == nil || route.Status != "active" {
			return nil, ErrCallbackConsumed
		}
		return nil, ErrCallbackMismatch
	}

	session, ok := s.sessionForCallbackIdentity(route.SessionIdentity, route.ThreadID, route.TurnID)
	if !ok {
		_, _ = s.store.RestoreMinimalApprovalClaim(ctx, route.RequestID, route.Action)
		if route.SessionIdentity != "" && !minimalWorkerCallbackIdentity(route.SessionIdentity) {
			return nil, ErrCallbackMismatch
		}
		return nil, ErrApprovalTransportUnavailable
	}
	result, ok := minimalApprovalResponse(route.RequestKind, route.Action)
	if !ok {
		_, _ = s.store.FinishMinimalApprovalClaim(ctx, route.RequestID, route.Action, "cancelled")
		return nil, ErrCallbackMismatch
	}
	exactResponder, ok := session.(exactMinimalApprovalResponder)
	if !ok {
		_, _ = s.store.RestoreMinimalApprovalClaim(ctx, route.RequestID, route.Action)
		return nil, ErrApprovalTransportUnavailable
	}
	requestCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	err = exactResponder.RespondServerRequestExact(requestCtx, firstNonEmpty(route.WireRequestID, route.RequestID), route.ThreadID, route.TurnID, route.RequestKind, result)
	if err != nil {
		switch {
		case minimalApprovalRequestInactive(err):
			_, _ = s.store.FinishMinimalApprovalClaim(ctx, route.RequestID, route.Action, "expired")
			s.editMinimalApprovalRouteMessage(ctx, route, "요청이 더 이상 활성 상태가 아닙니다.")
			return &DirectResponse{CallbackText: "비활성 요청입니다."}, nil
		case errors.Is(err, ErrApprovalTransportUnavailable) || minimalApprovalDefinitelyUnsent(err):
			_, _ = s.store.RestoreMinimalApprovalClaim(ctx, route.RequestID, route.Action)
			return nil, err
		default:
			_, _ = s.store.FinishMinimalApprovalClaim(ctx, route.RequestID, route.Action, "cancelled")
			s.editMinimalApprovalRouteMessage(ctx, route, "처리 결과를 확인할 수 없어 버튼이 비활성화되었습니다.")
			return nil, err
		}
	}
	status, resolvedText := "approved", "승인됨"
	if route.Action == "deny" {
		status, resolvedText = "denied", "거부됨"
	}
	changed, finishErr := s.store.FinishMinimalApprovalClaim(ctx, route.RequestID, route.Action, status)
	if finishErr != nil {
		queued, queueErr := s.store.CancelMinimalApprovalClaimWithInactiveEdit(ctx, route.RequestID, route.Action, resolvedText)
		if queueErr != nil || !queued {
			_ = s.editMinimalApprovalMessage(ctx, route.RequestID, resolvedText)
			if queueErr != nil {
				return nil, errors.Join(finishErr, queueErr)
			}
			return nil, finishErr
		}
		return &DirectResponse{CallbackText: resolvedText, ThreadID: route.ThreadID, TurnID: route.TurnID}, nil
	}
	if !changed {
		approval, _ := s.store.GetMinimalApproval(ctx, route.RequestID)
		if approval != nil && approval.Status == "expired" {
			s.editMinimalApprovalMessage(ctx, route.RequestID, "요청이 더 이상 활성 상태가 아닙니다.")
			return &DirectResponse{CallbackText: "비활성 요청입니다."}, nil
		}
		return nil, ErrCallbackConsumed
	}
	if editErr := s.editMinimalApprovalMessage(ctx, route.RequestID, resolvedText); editErr != nil {
		return &DirectResponse{Text: resolvedText, CallbackText: resolvedText, ThreadID: route.ThreadID, TurnID: route.TurnID}, nil
	}
	return &DirectResponse{CallbackText: resolvedText, ThreadID: route.ThreadID, TurnID: route.TurnID}, nil
}

func (s *Service) handleMinimalApprovalResolved(ctx context.Context, sessionIdentity string, event appserver.Event) bool {
	if s.cfg.Profile != "minimal" || !strings.EqualFold(strings.TrimSpace(event.Method), "serverRequest/resolved") {
		return false
	}
	wireRequestID := strings.TrimSpace(payloadMapString(event.Params, "requestId"))
	if wireRequestID == "" {
		return true
	}
	s.minimalApprovalMu.Lock()
	defer s.minimalApprovalMu.Unlock()
	var approval *storage.MinimalApproval
	var err error
	sessionIdentity = strings.TrimSpace(sessionIdentity)
	if sessionIdentity != "" {
		approval, err = s.store.GetMinimalApprovalForSession(ctx, wireRequestID, sessionIdentity)
		if err != nil {
			return false
		}
	}
	if approval == nil && !minimalWorkerCallbackIdentity(sessionIdentity) {
		approval, err = s.store.GetMinimalApproval(ctx, wireRequestID)
	}
	if err != nil || approval == nil {
		return false
	}
	if sessionIdentity != "" && approval.SessionIdentity != "" && approval.SessionIdentity != sessionIdentity {
		return false
	}
	var changed bool
	if strings.TrimSpace(approval.SessionIdentity) != "" {
		changed, err = s.store.ExpireMinimalApprovalForSession(ctx, wireRequestID, approval.SessionIdentity)
	} else {
		changed, err = s.store.ExpireMinimalApproval(ctx, approval.RequestID)
	}
	if err == nil && changed {
		_ = s.editMinimalApprovalMessage(ctx, approval.RequestID, "요청이 더 이상 활성 상태가 아닙니다.")
	}
	return true
}

func (s *Service) handlePendingServerRequest(ctx context.Context, sessionIdentity string, event appserver.Event) bool {
	approval, ok := appserver.PendingApprovalFromServerRequest(event)
	if !ok {
		return false
	}
	approval.SessionIdentity = strings.TrimSpace(sessionIdentity)
	approval.RequestKind = strings.TrimSpace(event.Method)
	if minimalWorkerCallbackIdentity(approval.SessionIdentity) {
		approval.RequestID, approval.WireRequestID = model.NormalizeRequestIdentity(approval.SessionIdentity, approval.RequestID, approval.RequestID)
	}
	_ = s.store.SavePendingApproval(ctx, *approval)
	return true
}

func (s *Service) processMinimalApprovalDelivery(ctx context.Context, sender Sender, item model.DeliveryQueueItem) {
	var payload model.DeliveryPayload
	if err := json.Unmarshal([]byte(item.PayloadJSON), &payload); err != nil {
		summary := deliveryErrorSummary("decode_error", err)
		_ = s.store.RecordDeliveryAttempt(ctx, item.ID, item.RetryCount+1, "decode_error", summary)
		_ = s.store.FailDelivery(ctx, item.ID, item.RetryCount+1, time.Now().UTC().Add(s.cfg.DeliveryRetryBase), summary, true)
		s.setError(ctx, errors.New(summary))
		return
	}
	s.minimalApprovalMu.Lock()
	defer s.minimalApprovalMu.Unlock()
	active, err := s.store.MinimalApprovalDeliveryActive(ctx, item.ID, payload.EventID)
	if err != nil || !active {
		return
	}
	messageID, sendErr := sender.SendMessage(ctx, item.ChatID, item.TopicID, payload.Text, payload.Buttons, notifySendOptions())
	if sendErr == nil && messageID <= 0 {
		sendErr = errors.New("telegram delivery returned no message id")
	}
	if sendErr != nil {
		attempt := item.RetryCount + 1
		summary := minimalApprovalDeliveryErrorSummary("send_error", sendErr)
		_ = s.store.RecordDeliveryAttempt(ctx, item.ID, attempt, "send_error", summary)
		dead := attempt >= s.cfg.DeliveryMaxAttempts
		backoff := s.cfg.DeliveryRetryBase * time.Duration(1<<min(attempt-1, 4))
		_ = s.store.FailDelivery(ctx, item.ID, attempt, time.Now().UTC().Add(backoff), summary, dead)
		s.setError(ctx, errors.New(summary))
		return
	}
	if err := s.store.CompleteMinimalApprovalDelivery(ctx, item.ID, payload.EventID, item.ChatID, item.TopicID, messageID); err != nil {
		summary := deliveryErrorSummary("persist_error", err)
		_, _ = s.store.ExpireMinimalApproval(ctx, payload.EventID)
		_ = sender.EditMessage(ctx, item.ChatID, item.TopicID, messageID, "요청이 더 이상 활성 상태가 아닙니다.", nil)
		_ = s.store.RecordDeliveryAttempt(ctx, item.ID, item.RetryCount+1, "persist_error", summary)
		_ = s.store.FailDelivery(ctx, item.ID, item.RetryCount+1, time.Now().UTC(), summary, true)
		s.setError(ctx, errors.New(summary))
		return
	}
	_ = s.store.RecordDeliveryAttempt(ctx, item.ID, item.RetryCount+1, "delivered", "")
}

func (s *Service) processMinimalApprovalPCCleanup(ctx context.Context, sender Sender, item model.DeliveryQueueItem) {
	var payload model.DeliveryPayload
	if err := json.Unmarshal([]byte(item.PayloadJSON), &payload); err != nil || payload.Mode != "delete_message" || strings.TrimSpace(payload.EventID) == "" {
		if err == nil {
			err = errors.New("minimal approval PC cleanup is incomplete")
		}
		summary := deliveryErrorSummary("decode_error", err)
		_ = s.store.RecordDeliveryAttempt(ctx, item.ID, item.RetryCount+1, "decode_error", summary)
		_ = s.store.FailDelivery(ctx, item.ID, item.RetryCount+1, time.Now().UTC(), summary, true)
		s.setError(ctx, errors.New(summary))
		return
	}
	var route *model.MessageRoute
	var err error
	if payload.MessageID > 0 {
		route, err = s.store.ResolveMessageRoute(ctx, item.ChatID, item.TopicID, payload.MessageID)
	} else {
		route, err = s.store.ResolveMessageRouteByEventIdentity(ctx, payload.EventID, payload.ThreadID, payload.TurnID)
	}
	if err != nil {
		attempt := item.RetryCount + 1
		summary := deliveryErrorSummary("persist_error", err)
		_ = s.store.RecordDeliveryAttempt(ctx, item.ID, attempt, "persist_error", summary)
		dead := attempt >= s.cfg.DeliveryMaxAttempts
		backoff := s.cfg.DeliveryRetryBase * time.Duration(1<<min(attempt-1, 4))
		_ = s.store.FailDelivery(ctx, item.ID, attempt, time.Now().UTC().Add(backoff), summary, dead)
		s.setError(ctx, errors.New(summary))
		return
	}
	if route == nil || route.MessageID <= 0 {
		if err := s.store.CompleteDelivery(ctx, item.ID); err != nil {
			attempt := item.RetryCount + 1
			summary := deliveryErrorSummary("persist_error", err)
			_ = s.store.RecordDeliveryAttempt(ctx, item.ID, attempt, "persist_error", summary)
			dead := attempt >= s.cfg.DeliveryMaxAttempts
			backoff := s.cfg.DeliveryRetryBase * time.Duration(1<<min(attempt-1, 4))
			_ = s.store.FailDelivery(ctx, item.ID, attempt, time.Now().UTC().Add(backoff), summary, dead)
			s.setError(ctx, errors.New(summary))
			return
		}
		_ = s.store.RecordDeliveryAttempt(ctx, item.ID, item.RetryCount+1, "terminal", "")
		return
	}
	if strings.TrimSpace(route.EventID) != payload.EventID {
		if err := s.store.CompleteDelivery(ctx, item.ID); err != nil {
			attempt := item.RetryCount + 1
			summary := deliveryErrorSummary("persist_error", err)
			_ = s.store.RecordDeliveryAttempt(ctx, item.ID, attempt, "persist_error", summary)
			dead := attempt >= s.cfg.DeliveryMaxAttempts
			backoff := s.cfg.DeliveryRetryBase * time.Duration(1<<min(attempt-1, 4))
			_ = s.store.FailDelivery(ctx, item.ID, attempt, time.Now().UTC().Add(backoff), summary, dead)
			s.setError(ctx, errors.New(summary))
			return
		}
		_ = s.store.RecordDeliveryAttempt(ctx, item.ID, item.RetryCount+1, "terminal", "")
		return
	}
	ready, terminal, err := s.store.MinimalApprovalPCCleanupReady(ctx, payload.ItemID, payload.ThreadID, payload.TurnID, payload.EventID)
	if err != nil {
		attempt := item.RetryCount + 1
		summary := deliveryErrorSummary("persist_error", err)
		_ = s.store.RecordDeliveryAttempt(ctx, item.ID, attempt, "persist_error", summary)
		dead := attempt >= s.cfg.DeliveryMaxAttempts
		backoff := s.cfg.DeliveryRetryBase * time.Duration(1<<min(attempt-1, 4))
		_ = s.store.FailDelivery(ctx, item.ID, attempt, time.Now().UTC().Add(backoff), summary, dead)
		s.setError(ctx, errors.New(summary))
		return
	}
	if terminal {
		if err := s.store.CompleteDelivery(ctx, item.ID); err != nil {
			attempt := item.RetryCount + 1
			summary := deliveryErrorSummary("persist_error", err)
			_ = s.store.RecordDeliveryAttempt(ctx, item.ID, attempt, "persist_error", summary)
			dead := attempt >= s.cfg.DeliveryMaxAttempts
			backoff := s.cfg.DeliveryRetryBase * time.Duration(1<<min(attempt-1, 4))
			_ = s.store.FailDelivery(ctx, item.ID, attempt, time.Now().UTC().Add(backoff), summary, dead)
			s.setError(ctx, errors.New(summary))
			return
		}
		_ = s.store.RecordDeliveryAttempt(ctx, item.ID, item.RetryCount+1, "terminal", "")
		return
	}
	if !ready {
		attempt := item.RetryCount + 1
		summary := "minimal approval actionable route is not durable yet"
		_ = s.store.RecordDeliveryAttempt(ctx, item.ID, attempt, "action_route_wait", summary)
		dead := attempt >= s.cfg.DeliveryMaxAttempts
		backoff := s.cfg.DeliveryRetryBase * time.Duration(1<<min(attempt-1, 4))
		_ = s.store.FailDelivery(ctx, item.ID, attempt, time.Now().UTC().Add(backoff), summary, dead)
		return
	}
	if err := sender.DeleteMessage(ctx, route.ChatID, route.TopicID, route.MessageID); err != nil {
		attempt := item.RetryCount + 1
		summary := deliveryErrorSummary("send_error", err)
		_ = s.store.RecordDeliveryAttempt(ctx, item.ID, attempt, "send_error", summary)
		dead := attempt >= s.cfg.DeliveryMaxAttempts
		backoff := s.cfg.DeliveryRetryBase * time.Duration(1<<min(attempt-1, 4))
		_ = s.store.FailDelivery(ctx, item.ID, attempt, time.Now().UTC().Add(backoff), summary, dead)
		s.setError(ctx, errors.New(summary))
		return
	}
	if err := s.store.CompleteDelivery(ctx, item.ID); err != nil {
		attempt := item.RetryCount + 1
		summary := deliveryErrorSummary("persist_error", err)
		_ = s.store.RecordDeliveryAttempt(ctx, item.ID, attempt, "persist_error", summary)
		dead := attempt >= s.cfg.DeliveryMaxAttempts
		backoff := s.cfg.DeliveryRetryBase * time.Duration(1<<min(attempt-1, 4))
		_ = s.store.FailDelivery(ctx, item.ID, attempt, time.Now().UTC().Add(backoff), summary, dead)
		s.setError(ctx, errors.New(summary))
		return
	}
	_ = s.store.RecordDeliveryAttempt(ctx, item.ID, item.RetryCount+1, "delivered", "")
}

func (s *Service) processMinimalApprovalInactiveEdit(ctx context.Context, sender Sender, item model.DeliveryQueueItem) {
	var payload model.DeliveryPayload
	if err := json.Unmarshal([]byte(item.PayloadJSON), &payload); err != nil || payload.Mode != "edit" || payload.MessageID <= 0 || strings.TrimSpace(payload.Text) == "" {
		if err == nil {
			err = errors.New("minimal approval inactive edit is incomplete")
		}
		summary := deliveryErrorSummary("decode_error", err)
		_ = s.store.RecordDeliveryAttempt(ctx, item.ID, item.RetryCount+1, "decode_error", summary)
		_ = s.store.FailDelivery(ctx, item.ID, item.RetryCount+1, time.Now().UTC(), summary, true)
		s.setError(ctx, errors.New(summary))
		return
	}
	if err := sender.EditMessage(ctx, item.ChatID, item.TopicID, payload.MessageID, payload.Text, nil); err != nil {
		attempt := item.RetryCount + 1
		summary := deliveryErrorSummary("send_error", err)
		_ = s.store.RecordDeliveryAttempt(ctx, item.ID, attempt, "send_error", summary)
		dead := attempt >= s.cfg.DeliveryMaxAttempts
		backoff := s.cfg.DeliveryRetryBase * time.Duration(1<<min(attempt-1, 4))
		_ = s.store.FailDelivery(ctx, item.ID, attempt, time.Now().UTC().Add(backoff), summary, dead)
		s.setError(ctx, errors.New(summary))
		return
	}
	if err := s.store.CompleteDelivery(ctx, item.ID); err != nil {
		attempt := item.RetryCount + 1
		summary := deliveryErrorSummary("persist_error", err)
		_ = s.store.RecordDeliveryAttempt(ctx, item.ID, attempt, "persist_error", summary)
		dead := attempt >= s.cfg.DeliveryMaxAttempts
		backoff := s.cfg.DeliveryRetryBase * time.Duration(1<<min(attempt-1, 4))
		_ = s.store.FailDelivery(ctx, item.ID, attempt, time.Now().UTC().Add(backoff), summary, dead)
		s.setError(ctx, errors.New(summary))
		return
	}
	_ = s.store.RecordDeliveryAttempt(ctx, item.ID, item.RetryCount+1, "delivered", "")
}

func (s *Service) editMinimalApprovalMessage(ctx context.Context, requestID, text string) error {
	approval, err := s.store.GetMinimalApproval(ctx, requestID)
	if err != nil || approval == nil || approval.ChatID == 0 || approval.TelegramMessageID <= 0 {
		return err
	}
	return s.editMinimalApprovalTarget(ctx, approval.ChatID, approval.TopicID, approval.TelegramMessageID, text)
}

func (s *Service) editMinimalApprovalRouteMessage(ctx context.Context, route *storage.MinimalApprovalRoute, text string) error {
	if route == nil || route.ChatID == 0 || route.TelegramMessageID <= 0 {
		return nil
	}
	return s.editMinimalApprovalTarget(ctx, route.ChatID, route.TopicID, route.TelegramMessageID, text)
}

func (s *Service) editMinimalApprovalTarget(ctx context.Context, chatID, topicID, messageID int64, text string) error {
	s.mu.RLock()
	sender := s.sender
	s.mu.RUnlock()
	if sender == nil {
		return errors.New("telegram sender is unavailable")
	}
	return sender.EditMessage(ctx, chatID, topicID, messageID, text, nil)
}

func minimalApprovalResponse(kind, action string) (map[string]any, bool) {
	decision := "accept"
	if action == "deny" {
		decision = "decline"
	} else if action != "approve" {
		return nil, false
	}
	// The generated v2 schemas define distinct response types for these two
	// request methods, though their minimal one-shot decisions currently share
	// the same exact string values.
	switch kind {
	case minimalCommandApprovalKind:
		return map[string]any{"decision": decision}, true
	case minimalFileApprovalKind:
		return map[string]any{"decision": decision}, true
	default:
		return nil, false
	}
}

func minimalApprovalPresentation(kind string, params map[string]any) (string, string) {
	switch kind {
	case minimalCommandApprovalKind:
		return "명령 실행", boundedMinimalApprovalSummary(payloadMapString(params, "command"), "명령 세부 정보 없음")
	case minimalFileApprovalKind:
		summary := payloadMapString(params, "reason")
		if summary == "" {
			summary = payloadMapString(params, "grantRoot")
		}
		return "파일 변경", boundedMinimalApprovalSummary(summary, "파일 변경 세부 정보 없음")
	default:
		return "알 수 없는 요청", "세부 정보 없음"
	}
}

func boundedMinimalApprovalSummary(value, fallback string) string {
	value = strings.Join(strings.Fields(strings.TrimSpace(value)), " ")
	if value == "" {
		value = fallback
	}
	runes := []rune(value)
	if len(runes) > minimalApprovalSummaryRunes {
		return string(runes[:minimalApprovalSummaryRunes-1]) + "…"
	}
	return value
}

func minimalRPCID(value any) string {
	switch typed := value.(type) {
	case string:
		return strings.TrimSpace(typed)
	case float64:
		if typed == float64(int64(typed)) {
			return strconv.FormatInt(int64(typed), 10)
		}
	}
	return ""
}

func (s *Service) minimalApprovalSessionIdentity() string {
	s.mu.RLock()
	generation := s.liveGeneration
	s.mu.RUnlock()
	return s.minimalApprovalSessionID + ":" + strconv.FormatUint(generation, 10)
}

func (s *Service) sessionForCallbackIdentity(identity, threadID, turnID string) (Session, bool) {
	identity = strings.TrimSpace(identity)
	threadID = strings.TrimSpace(threadID)
	if identity != "" && s.minimalWorkers != nil {
		if worker, ok := s.minimalWorkers.BySessionIdentity(identity); ok && worker != nil {
			workerThreadID := strings.TrimSpace(worker.ThreadID)
			if threadID != "" && workerThreadID != "" && workerThreadID != threadID {
				return nil, false
			}
			if worker.Session == nil {
				return nil, false
			}
			return worker.Session, true
		}
		if minimalWorkerCallbackIdentity(identity) {
			return nil, false
		}
	}
	s.mu.RLock()
	live := s.live
	connected := s.liveConnected
	currentLiveIdentity := s.minimalApprovalSessionID + ":" + strconv.FormatUint(s.liveGeneration, 10)
	s.mu.RUnlock()
	if identity != "" && identity != currentLiveIdentity {
		return nil, false
	}
	if !connected || live == nil {
		return nil, false
	}
	return live, true
}

func minimalWorkerCallbackIdentity(identity string) bool {
	return strings.HasPrefix(strings.TrimSpace(identity), "minimal-link-worker:")
}

func minimalApprovalEventID(requestID, threadID, turnID, requestKind string) string {
	digest := sha256.Sum256([]byte(strings.Join([]string{requestID, threadID, turnID, requestKind}, "\x00")))
	return fmt.Sprintf("minimal-approval:%x", digest[:12])
}

func minimalPollOnlyApprovalEventID(threadID, turnID string) string {
	digest := sha256.Sum256([]byte(strings.Join([]string{threadID, turnID}, "\x00")))
	return fmt.Sprintf("%s%x", minimalPollOnlyApprovalEventPrefix, digest[:12])
}

func newMinimalApprovalToken() (string, error) {
	var token [16]byte
	if _, err := rand.Read(token[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(token[:]), nil
}

func minimalApprovalRequestInactive(err error) bool {
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "not pending in the current session") ||
		strings.Contains(message, "already resolved") ||
		strings.Contains(message, "request is not pending")
}

func minimalApprovalDeliveryErrorSummary(status string, err error) string {
	summary := deliveryErrorSummary(status, err)
	if status != "send_error" {
		return summary
	}
	outcome := storage.MinimalApprovalDeliveryOutcomeUnknown
	if minimalApprovalSendDefinitelyUnsent(err) {
		outcome = storage.MinimalApprovalDeliveryOutcomeDefinitelyUnsent
	}
	return summary + " " + outcome
}

func minimalApprovalSendDefinitelyUnsent(err error) bool {
	return errors.Is(err, ErrApprovalTransportUnavailable) ||
		minimalApprovalDefinitelyUnsent(err) ||
		minimalApprovalTelegramSendRejectionDefinitelyUnsent(err)
}

func minimalApprovalDefinitelyUnsent(err error) bool {
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "app-server is not running")
}

func minimalApprovalTelegramSendRejectionDefinitelyUnsent(err error) bool {
	message := strings.ToLower(strings.TrimSpace(err.Error()))
	for _, prefix := range []string{
		"telegram sendmessage http ",
		"telegram sendmessage: ",
	} {
		if !strings.HasPrefix(message, prefix) {
			continue
		}
		code, ok := minimalApprovalLeadingStatusCode(strings.TrimSpace(strings.TrimPrefix(message, prefix)))
		return ok && code >= 400 && code < 500
	}
	return false
}

func minimalApprovalLeadingStatusCode(value string) (int, bool) {
	if value == "" {
		return 0, false
	}
	end := 0
	for end < len(value) && value[end] >= '0' && value[end] <= '9' {
		end++
	}
	if end == 0 {
		return 0, false
	}
	code, err := strconv.Atoi(value[:end])
	if err != nil {
		return 0, false
	}
	return code, true
}
