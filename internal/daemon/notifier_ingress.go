package daemon

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/alclssna33/codex_to_telegram/internal/model"
)

const notifierOnlyText = "현재 봇은 Codex 완료 알림 전용입니다."

func (s *Service) handleNotifierInboundText(ctx context.Context, inbound model.InboundText) (*DirectResponse, error) {
	text := strings.TrimSpace(inbound.Text)
	switch minimalCommandName(text) {
	case "start":
		if inbound.ChatID == inbound.UserID && inbound.TopicID == 0 {
			if err := s.store.SetGlobalObserverTarget(ctx, inbound.ChatID, 0, true); err != nil {
				return nil, err
			}
		}
		return &DirectResponse{Text: "Codex 완료 알림을 감시합니다.\n관리 명령: /status /repair /help"}, nil
	case "help":
		return &DirectResponse{Text: "관리 명령:\n/start\n/help\n/status\n/repair"}, nil
	case "status", "repair":
		return s.handleCommand(ctx, inbound.ChatID, inbound.TopicID, text, 0)
	default:
		return &DirectResponse{Text: notifierOnlyText}, nil
	}
}

func (s *Service) handleNotifierCallback(ctx context.Context, userID, chatID int64) (*DirectResponse, error) {
	if !s.IsAllowed(userID, chatID) {
		return nil, ErrUnauthorized
	}
	return &DirectResponse{CallbackText: notifierOnlyText}, nil
}

func (s *Service) notifierStatusSnapshot(ctx context.Context) (string, error) {
	tracked, active, err := s.store.CountNotifierObservations(ctx)
	if err != nil {
		return "", err
	}
	backlog, _ := s.store.DeliveryQueueBacklog(ctx)
	target, configured, _ := s.store.GetGlobalObserverTarget(ctx)
	sweepCursor, _ := s.store.GetState(ctx, notifierSweepCursorKey)
	s.mu.RLock()
	ready := s.ready
	phase := s.phase
	pollConnected := s.pollConnected
	startedAt := s.startedAt
	lastError := sanitizeDiagnosticString(s.lastError)
	s.mu.RUnlock()

	lines := []string{
		"Notifier status",
		fmt.Sprintf("Ready: %t", ready),
		fmt.Sprintf("Phase: %s", phase),
		fmt.Sprintf("Poll app-server: %t", pollConnected),
		fmt.Sprintf("Tracked observations: %d", tracked),
		fmt.Sprintf("Active observations: %d", active),
		fmt.Sprintf("Delivery backlog: %d", backlog),
		fmt.Sprintf("Sweep cursor: %s", notifierSweepLabel(sweepCursor)),
	}
	switch {
	case configured && target != nil && target.Enabled:
		lines = append(lines, fmt.Sprintf("Global target: on -> %s", model.ChatKey(target.ChatID, target.TopicID)))
	case configured && target != nil:
		lines = append(lines, fmt.Sprintf("Global target: off -> %s", model.ChatKey(target.ChatID, target.TopicID)))
	default:
		lines = append(lines, "Global target: unset")
	}
	if !startedAt.IsZero() {
		lines = append(lines, fmt.Sprintf("Started: %s", startedAt.UTC().Format(time.RFC3339)))
	}
	if strings.TrimSpace(lastError) != "" {
		lines = append(lines, fmt.Sprintf("Last error: %s", lastError))
	}
	return strings.Join(lines, "\n"), nil
}

func notifierSweepLabel(cursor string) string {
	cursor = strings.TrimSpace(cursor)
	if cursor == "" {
		return "(head)"
	}
	return cursor
}
