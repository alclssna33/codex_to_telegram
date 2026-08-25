package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
	"unicode/utf16"

	"github.com/alclssna33/codex_to_telegram/internal/appserver"
	"github.com/alclssna33/codex_to_telegram/internal/model"
	"github.com/alclssna33/codex_to_telegram/internal/tgformat"
)

func (s *Service) projectMinimalTerminal(ctx context.Context, snapshot appserver.ThreadReadSnapshot) error {
	status := canonicalMinimalTerminalStatus(snapshot.LatestTurnStatus)
	if s.cfg.Profile != "minimal" || (status != "completed" && status != "failed" && status != "interrupted") {
		return nil
	}
	project, ok := s.projectRegistry.MatchExactCWD(snapshot.Thread.CWD)
	if !ok {
		return nil
	}
	terminal, err := s.store.ClaimMinimalTerminalTransitionAfterBaseline(
		ctx,
		snapshot.Thread.ID,
		snapshot.LatestTurnID,
		status,
		snapshot.Thread.UpdatedAt,
		s.globalObserverSinceUnix(ctx),
		s.now(),
	)
	if err != nil || terminal == nil {
		return err
	}
	var release *model.MinimalLinkedRelease
	if link, err := s.store.GetMinimalLinkedThreadByLinkedID(ctx, snapshot.Thread.ID); err != nil {
		return err
	} else if link != nil && strings.TrimSpace(link.State) == model.MinimalLinkedTelegramRunning {
		release = &model.MinimalLinkedRelease{
			LinkedThreadID:   link.LinkedThreadID,
			TurnID:           snapshot.LatestTurnID,
			WorkerGeneration: link.WorkerGeneration,
		}
	}
	target, configured, err := s.store.GetGlobalObserverTarget(ctx)
	if err != nil {
		return err
	}
	if !configured || target == nil || !target.Enabled {
		if release != nil {
			_, err = s.store.EnqueueTerminalEventWithLinkedRelease(ctx, *terminal, nil, release)
			if err != nil {
				_ = s.store.ReopenMinimalTerminalTransition(ctx, snapshot.Thread.ID, snapshot.LatestTurnID, s.now())
			}
			return err
		}
		return nil
	}
	marker := "[완료]"
	if status == "failed" || status == "interrupted" {
		marker = "[실패]"
	}
	body := strings.TrimSpace(snapshot.LatestFinalText)
	if body == "" {
		for i := len(snapshot.LatestAgentMessages) - 1; i >= 0; i-- {
			if strings.TrimSpace(snapshot.LatestAgentMessages[i]) != "" {
				body = strings.TrimSpace(snapshot.LatestAgentMessages[i])
				break
			}
		}
	}
	if body == "" {
		body = "Codex가 최종 답변을 남기지 않았습니다."
	}
	title := cleanMinimalDisplayText(snapshot.Thread.Title)
	if title == "" || title == snapshot.Thread.ID {
		title = snapshot.Thread.ShortID()
	}
	headerLines := []string{fmt.Sprintf("%s %s", marker, project.DisplayName)}
	headerLines = append(headerLines, "대화: "+title)
	headerLines = append(headerLines, "Thread: "+snapshot.Thread.ShortID())
	header := strings.Join(headerLines, "\n")
	whole := tgformat.RenderSegments([]tgformat.Segment{tgformat.Plain(header + "\n\n"), tgformat.Markdown(body)}, 1<<30)[0]
	rendered := splitMinimalRendered(whole, tgformat.TelegramMessageLimit-16)
	count := len(rendered)
	items := make([]model.DeliveryQueueItem, 0, count)
	for i, message := range rendered {
		label := fmt.Sprintf("[%d/%d]\n", i+1, count)
		message.Text = label + message.Text
		for j := range message.Entities {
			message.Entities[j].Offset += len([]rune(label))
		}
		payload := model.DeliveryPayload{Text: message.Text, ThreadID: snapshot.Thread.ID, TurnID: snapshot.LatestTurnID, EventID: terminal.TerminalKey, Rendered: &message}
		raw, err := json.Marshal(payload)
		if err != nil {
			return err
		}
		items = append(items, model.DeliveryQueueItem{EventID: fmt.Sprintf("%s:%d", terminal.TerminalKey, i+1), ChatKey: model.ChatKey(target.ChatID, target.TopicID), ChatID: target.ChatID, TopicID: target.TopicID, ThreadID: snapshot.Thread.ID, Kind: "terminal", Status: model.DeliveryStatusPending, GroupID: terminal.TerminalKey, SequenceNo: i + 1, SequenceCount: count, PayloadJSON: string(raw)})
	}
	_, err = s.store.EnqueueTerminalEventWithLinkedRelease(ctx, *terminal, items, release)
	if err != nil {
		_ = s.store.ReopenMinimalTerminalTransition(ctx, snapshot.Thread.ID, snapshot.LatestTurnID, s.now())
	}
	return err
}

func (s *Service) releaseMinimalTerminalWorker(_ context.Context, snapshot appserver.ThreadReadSnapshot) error {
	if s.cfg.Profile != "minimal" || s.minimalWorkers == nil {
		return nil
	}
	threadID := strings.TrimSpace(snapshot.Thread.ID)
	turnID := strings.TrimSpace(snapshot.LatestTurnID)
	if threadID == "" || turnID == "" {
		return nil
	}
	lookupCtx, lookupCancel := s.minimalLinkedFinalizationContext()
	link, err := s.store.GetMinimalLinkedThreadByLinkedID(lookupCtx, threadID)
	lookupCancel()
	if err != nil || link == nil {
		if err != nil {
			return err
		}
		return s.releaseRegisteredMinimalTerminalWorker(context.Background(), threadID)
	}
	if strings.TrimSpace(link.State) != model.MinimalLinkedReleasePending {
		return nil
	}
	if strings.TrimSpace(link.ActiveTurnID) != turnID {
		return nil
	}
	generation := link.WorkerGeneration
	sessionIdentity := ""
	if worker, ok := s.minimalWorkers.ByThread(link.LinkedThreadID); ok && worker != nil && worker.Generation == generation {
		sessionIdentity = strings.TrimSpace(worker.SessionIdentity)
	}
	closeCtx, closeCancel := s.minimalLinkedCloseContext()
	closed, closeErr := s.minimalWorkers.Release(closeCtx, link.LinkedThreadID, generation)
	closeCancel()
	confirmedClosed := closed
	if !confirmedClosed && closeErr == nil {
		confirmedClosed = s.minimalWorkers.ConfirmedRelease(link.LinkedThreadID, generation)
	}
	if sessionIdentity == "" {
		if confirmedIdentity, ok := s.minimalWorkers.ConfirmedReleaseIdentity(link.LinkedThreadID, generation); ok {
			sessionIdentity = confirmedIdentity
		}
	}
	if closeErr != nil || !confirmedClosed {
		failCtx, failCancel := s.minimalLinkedFinalizationContext()
		_ = s.store.FailMinimalLinkedThread(failCtx, link.LinkedThreadID, generation, "release_close_failed")
		failCancel()
		if closeErr != nil {
			return closeErr
		}
		return fmt.Errorf("minimal linked worker %s generation %d was not closed", shortLogID(link.LinkedThreadID), generation)
	}
	finalizeCtx, finalizeCancel := s.minimalLinkedFinalizationContext()
	defer finalizeCancel()
	ready, err := s.minimalHandoffReadyDelivery(finalizeCtx, *link, turnID)
	if err != nil {
		return err
	}
	changed, err := s.store.FinishMinimalLinkedReleaseWithReadyDelivery(finalizeCtx, link.LinkedThreadID, generation, sessionIdentity, turnID, s.now(), ready)
	if err != nil {
		return err
	}
	if !changed {
		return fmt.Errorf("minimal linked release finish did not match %s generation %d", shortLogID(link.LinkedThreadID), generation)
	}
	s.minimalWorkers.ForgetConfirmedRelease(link.LinkedThreadID, generation)
	return nil
}

func (s *Service) minimalLinkedCloseContext() (context.Context, context.CancelFunc) {
	timeout := 30 * time.Second
	if s != nil && s.minimalWorkers != nil && s.minimalWorkers.closeTimeout > 0 {
		timeout = s.minimalWorkers.closeTimeout
	}
	if s != nil && s.minimalLinkedCloseTimeout > 0 {
		timeout = s.minimalLinkedCloseTimeout
	}
	return context.WithTimeout(context.Background(), timeout)
}

func (s *Service) minimalLinkedFinalizationContext() (context.Context, context.CancelFunc) {
	timeout := 30 * time.Second
	if s != nil && s.minimalLinkedFinalizationTimeout > 0 {
		timeout = s.minimalLinkedFinalizationTimeout
	}
	return context.WithTimeout(context.Background(), timeout)
}

func (s *Service) releaseRegisteredMinimalTerminalWorker(_ context.Context, threadID string) error {
	if s == nil || s.minimalWorkers == nil {
		return nil
	}
	threadID = strings.TrimSpace(threadID)
	if threadID == "" {
		return nil
	}
	worker, ok := s.minimalWorkers.ByThread(threadID)
	if !ok || worker == nil {
		return nil
	}
	generation := worker.Generation
	sessionIdentity := strings.TrimSpace(worker.SessionIdentity)
	closeCtx, closeCancel := s.minimalLinkedCloseContext()
	closed, err := s.minimalWorkers.Release(closeCtx, threadID, generation)
	closeCancel()
	if err != nil {
		return err
	}
	if !closed {
		return nil
	}
	if sessionIdentity != "" {
		cleanupCtx, cleanupCancel := s.minimalLinkedFinalizationContext()
		defer cleanupCancel()
		if _, expireErr := s.store.ExpireMinimalApprovalsForSession(cleanupCtx, sessionIdentity); expireErr != nil {
			return expireErr
		}
	}
	s.minimalWorkers.ForgetConfirmedRelease(threadID, generation)
	return nil
}

func (s *Service) staleMinimalLinkedTerminal(ctx context.Context, snapshot appserver.ThreadReadSnapshot) (bool, error) {
	if s.cfg.Profile != "minimal" || !isTerminalStatus(snapshot.LatestTurnStatus) {
		return false, nil
	}
	threadID := strings.TrimSpace(snapshot.Thread.ID)
	if threadID == "" {
		return false, nil
	}
	link, err := s.store.GetMinimalLinkedThreadByLinkedID(ctx, threadID)
	if err != nil || link == nil {
		return false, err
	}
	state := strings.TrimSpace(link.State)
	if state != model.MinimalLinkedTelegramRunning && state != model.MinimalLinkedReleasePending {
		return false, nil
	}
	activeTurnID := strings.TrimSpace(link.ActiveTurnID)
	if activeTurnID == "" {
		return false, nil
	}
	return activeTurnID != strings.TrimSpace(snapshot.LatestTurnID), nil
}

func (s *Service) minimalHandoffReadyDelivery(ctx context.Context, link model.MinimalLinkedThread, turnID string) (*model.DeliveryQueueItem, error) {
	chatID, topicID := link.ChatID, link.TopicID
	if chatID == 0 {
		target, configured, err := s.store.GetGlobalObserverTarget(ctx)
		if err != nil {
			return nil, err
		}
		if configured && target != nil && target.Enabled {
			chatID, topicID = target.ChatID, target.TopicID
		}
	}
	if chatID == 0 {
		return nil, nil
	}
	title := cleanMinimalDisplayText(link.DesiredTitle)
	if title == "" {
		title = model.Thread{ID: link.LinkedThreadID}.ShortID()
	}
	eventID := fmt.Sprintf("%s:%s:handoff_ready", link.LinkedThreadID, turnID)
	payload := model.DeliveryPayload{
		Text:     fmt.Sprintf("Codex에서 “%s” 작업을 열어 이어서 작업할 수 있습니다.", title),
		ThreadID: link.LinkedThreadID,
		TurnID:   turnID,
		EventID:  eventID,
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	return &model.DeliveryQueueItem{
		EventID:     eventID,
		ChatKey:     model.ChatKey(chatID, topicID),
		ChatID:      chatID,
		TopicID:     topicID,
		ThreadID:    link.LinkedThreadID,
		Kind:        "handoff_ready",
		Status:      model.DeliveryStatusPending,
		AvailableAt: model.NowString(),
		PayloadJSON: string(raw),
		CreatedAt:   model.NowString(),
		UpdatedAt:   model.NowString(),
	}, nil
}

func (s *Service) retireMinimalObservationIfIneligible(ctx context.Context, snapshot appserver.ThreadReadSnapshot) (bool, error) {
	if s.cfg.Profile != "minimal" || s.projectRegistry == nil {
		return false, nil
	}
	threadID := strings.TrimSpace(snapshot.Thread.ID)
	cwd := strings.TrimSpace(snapshot.Thread.CWD)
	if threadID == "" || cwd == "" {
		return false, nil
	}
	if _, ok := s.projectRegistry.MatchExactCWD(cwd); ok {
		return false, nil
	}
	if err := s.store.RetireMinimalObservation(ctx, threadID, s.now()); err != nil {
		return false, err
	}
	return true, nil
}

func canonicalMinimalTerminalStatus(status string) string {
	normalized := strings.ToLower(strings.TrimSpace(status))
	if normalized == "canceled" || normalized == "cancelled" {
		return "interrupted"
	}
	return normalized
}

func splitMinimalRendered(message model.RenderedMessage, limit int) []model.RenderedMessage {
	runes := []rune(message.Text)
	out := []model.RenderedMessage{}
	startRune, startUTF16 := 0, 0
	for startRune < len(runes) {
		endRune, endUTF16 := startRune, startUTF16
		for endRune < len(runes) {
			width := len(utf16.Encode([]rune{runes[endRune]}))
			if endUTF16-startUTF16+width > limit {
				break
			}
			endUTF16 += width
			endRune++
		}
		chunk := model.RenderedMessage{Text: string(runes[startRune:endRune])}
		for _, entity := range message.Entities {
			from := max(entity.Offset, startUTF16)
			to := min(entity.Offset+entity.Length, endUTF16)
			if from < to {
				entity.Offset = from - startUTF16
				entity.Length = to - from
				chunk.Entities = append(chunk.Entities, entity)
			}
		}
		out = append(out, chunk)
		startRune, endRune = endRune, endRune
		startUTF16 = endUTF16
	}
	return out
}
