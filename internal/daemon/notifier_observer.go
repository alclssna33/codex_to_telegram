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
	"github.com/alclssna33/codex_to_telegram/internal/storage"
)

const (
	notifierCatalogLimit   = 100
	notifierSweepCursorKey = "notifier.sweep_cursor"
	// Four reads keeps a single wedged App Server request from backing up a
	// persistent poll session while still catching a small burst each cycle.
	notifierThreadReadBatchLimit = 4
	// A timed out thread/read remains required but is retried later. This keeps
	// one oversized or stale conversation from repeatedly stalling all others.
	notifierThreadReadRetryDelay = 30 * time.Second
)

func notifierListOptions(cursor string) control.ThreadListOptions {
	archived := false
	return control.ThreadListOptions{
		Limit:         notifierCatalogLimit,
		Cursor:        strings.TrimSpace(cursor),
		SortKey:       "updated_at",
		SortDirection: "desc",
		SourceKinds:   []string{"cli", "vscode"},
		Archived:      &archived,
	}
}

func (s *Service) discoverNotifierThreads(ctx context.Context) error {
	s.mu.RLock()
	poll := s.poll
	pollConnected := s.pollConnected
	s.mu.RUnlock()
	if !pollConnected || poll == nil {
		return nil
	}
	client, ok := poll.(control.FilteredThreads)
	if !ok {
		return nil
	}

	head, err := s.readNotifierListPage(ctx, client, "")
	if err != nil {
		return err
	}
	if err := s.observeNotifierListPage(ctx, head); err != nil {
		return err
	}

	sweepCursor, err := s.store.GetState(ctx, notifierSweepCursorKey)
	if err != nil {
		return err
	}
	sweepCursor = strings.TrimSpace(sweepCursor)
	if sweepCursor == "" {
		sweepCursor = minimalListNextCursor(head)
	}
	if sweepCursor == "" {
		return s.store.SetState(ctx, notifierSweepCursorKey, "")
	}

	sweep, err := s.readNotifierListPage(ctx, client, sweepCursor)
	if err != nil {
		return err
	}
	if err := s.observeNotifierListPage(ctx, sweep); err != nil {
		return err
	}
	return s.store.SetState(ctx, notifierSweepCursorKey, minimalListNextCursor(sweep))
}

func (s *Service) readNotifierListPage(ctx context.Context, client control.FilteredThreads, cursor string) (map[string]any, error) {
	timeout := s.cfg.RequestTimeout
	if timeout <= 0 {
		timeout = 15 * time.Second
	}
	requestCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	return client.ThreadListWithOptions(requestCtx, notifierListOptions(cursor))
}

func (s *Service) observeNotifierListPage(ctx context.Context, result map[string]any) error {
	now := s.now()
	for _, thread := range appserver.ThreadsFromList(result) {
		if err := s.store.ObserveNotifierThread(ctx, thread.ID, thread.UpdatedAt, now); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) pollNotifierThreads(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	repairPending, err := s.store.GetState(ctx, "control.repair_request")
	if err != nil {
		return err
	}
	if strings.TrimSpace(repairPending) != "" {
		return nil
	}
	s.mu.RLock()
	poll := s.poll
	pollConnected := s.pollConnected
	s.mu.RUnlock()
	if !pollConnected || poll == nil {
		return nil
	}
	activationUnix, err := s.store.EnsureNotifierActivation(ctx, s.now())
	if err != nil {
		return err
	}
	if err := s.store.BaselineNotifierObservationsAtOrBefore(ctx, activationUnix, s.now()); err != nil {
		return err
	}
	due, err := s.store.ListNotifierObservationsDueAt(ctx, notifierThreadReadBatchLimit, s.now())
	if err != nil {
		return err
	}
	var firstErr error
	for _, previous := range due {
		if err := s.pollNotifierThread(ctx, poll, previous, activationUnix); err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return ctxErr
			}
			if errors.Is(err, context.DeadlineExceeded) {
				if deferErr := s.store.DeferNotifierRead(ctx, previous.ThreadID, s.now().Add(notifierThreadReadRetryDelay), s.now()); deferErr != nil {
					return errors.Join(err, deferErr)
				}
			}
			if firstErr == nil {
				firstErr = err
			}
		}
	}
	return firstErr
}

func (s *Service) pollNotifierThread(ctx context.Context, poll Session, previous model.NotifierObservation, activationUnix int64) error {
	threadID := strings.TrimSpace(previous.ThreadID)
	if threadID == "" {
		return nil
	}
	requestCtx, cancel := context.WithTimeout(ctx, maxDuration(10*time.Second, s.cfg.ObserverPollInterval*2))
	defer cancel()
	payload, err := poll.ThreadRead(requestCtx, threadID, true)
	if err != nil {
		return err
	}
	snapshot := appserver.SnapshotFromThreadRead(payload)
	if strings.TrimSpace(snapshot.Thread.ID) == "" {
		snapshot.Thread.ID = threadID
	}
	turnSnapshots := notifierTurnSnapshotsFromRead(payload, threadID)
	if len(turnSnapshots) == 0 {
		turnSnapshots = []appserver.ThreadReadSnapshot{snapshot}
	}
	status := canonicalMinimalTerminalStatus(snapshot.LatestTurnStatus)
	terminalSnapshots := notifierTerminalSnapshotsSince(previous, turnSnapshots, activationUnix)
	if len(terminalSnapshots) == 0 {
		return s.store.RecordNotifierRead(ctx, threadID, snapshot.LatestTurnID, status, snapshot.Thread.UpdatedAt, false, s.now())
	}

	target, configured, err := s.store.GetGlobalObserverTarget(ctx)
	if err != nil {
		return err
	}
	if !configured || target == nil || !target.Enabled || target.ChatID == 0 {
		return nil
	}

	for _, terminalSnapshot := range terminalSnapshots {
		terminalStatus, ok := notifierTerminalStatus(terminalSnapshot)
		if !ok {
			continue
		}
		message := renderNotifierTerminal(terminalSnapshot)
		event := model.TerminalEvent{
			TerminalKey:    fmt.Sprintf("%s:%s:%s", threadID, terminalSnapshot.LatestTurnID, terminalStatus),
			ThreadID:       threadID,
			TurnID:         terminalSnapshot.LatestTurnID,
			Status:         terminalStatus,
			DeliveryStatus: model.DeliveryStatusPending,
			UpdatedAt:      model.NowString(),
		}
		if _, err := s.store.EnqueueTerminalEvent(ctx, event, notifierDeliveryItems(*target, event, message)); err != nil {
			return err
		}
	}
	if err := s.store.RecordNotifierRead(ctx, threadID, snapshot.LatestTurnID, status, snapshot.Thread.UpdatedAt, false, s.now()); err != nil {
		return err
	}
	return nil
}

func notifierTurnSnapshotsFromRead(payload map[string]any, fallbackThreadID string) []appserver.ThreadReadSnapshot {
	threadPayload := payload
	if nested, ok := payload["thread"].(map[string]any); ok && nested != nil {
		threadPayload = nested
	}
	turns, _ := threadPayload["turns"].([]any)
	out := make([]appserver.ThreadReadSnapshot, 0, len(turns))
	for _, rawTurn := range turns {
		turn, _ := rawTurn.(map[string]any)
		if turn == nil {
			continue
		}
		singleThread := make(map[string]any, len(threadPayload))
		for key, value := range threadPayload {
			singleThread[key] = value
		}
		singleThread["turns"] = []any{turn}
		snapshot := appserver.SnapshotFromThreadRead(map[string]any{"thread": singleThread})
		if strings.TrimSpace(snapshot.Thread.ID) == "" {
			snapshot.Thread.ID = strings.TrimSpace(fallbackThreadID)
		}
		out = append(out, snapshot)
	}
	return out
}

func notifierTerminalSnapshotsSince(previous model.NotifierObservation, snapshots []appserver.ThreadReadSnapshot, activationUnix int64) []appserver.ThreadReadSnapshot {
	if len(snapshots) == 0 {
		return nil
	}
	if !previous.BaselineReady {
		latest := snapshots[len(snapshots)-1]
		if latest.Thread.UpdatedAt <= activationUnix {
			return nil
		}
		if _, ok := notifierTerminalStatus(latest); !ok {
			return nil
		}
		return []appserver.ThreadReadSnapshot{latest}
	}
	start := notifierStartIndex(previous.LastTurnID, snapshots)
	out := []appserver.ThreadReadSnapshot{}
	for i := start; i < len(snapshots); i++ {
		current := snapshots[i]
		if _, ok := notifierTerminalStatus(current); !ok {
			continue
		}
		if notifierTerminalTransition(previous, current, activationUnix) {
			out = append(out, current)
		}
	}
	return out
}

func notifierStartIndex(previousTurnID string, snapshots []appserver.ThreadReadSnapshot) int {
	previousTurnID = strings.TrimSpace(previousTurnID)
	if previousTurnID == "" {
		return len(snapshots) - 1
	}
	for i, snapshot := range snapshots {
		if strings.TrimSpace(snapshot.LatestTurnID) == previousTurnID {
			return i
		}
	}
	return len(snapshots) - 1
}

func notifierTerminalTransition(previous model.NotifierObservation, current appserver.ThreadReadSnapshot, activationUnix int64) bool {
	status, ok := notifierTerminalStatus(current)
	if current.LatestTurnID == "" || !ok {
		return false
	}
	if !previous.BaselineReady {
		return current.Thread.UpdatedAt > activationUnix
	}
	return previous.LastTurnID != current.LatestTurnID ||
		canonicalMinimalTerminalStatus(previous.LastTurnStatus) != status
}

func notifierTerminalStatus(snapshot appserver.ThreadReadSnapshot) (string, bool) {
	status := canonicalMinimalTerminalStatus(snapshot.LatestTurnStatus)
	if !isNotifierTerminalStatus(status) {
		return "", false
	}
	if status == "interrupted" && !snapshotHasFinalSignal(&snapshot) {
		return "", false
	}
	return status, true
}

func isNotifierTerminalStatus(status string) bool {
	switch canonicalMinimalTerminalStatus(status) {
	case "completed", "failed", "interrupted":
		return true
	default:
		return false
	}
}

func notifierDeliveryItems(target model.ObserverTarget, event model.TerminalEvent, message model.RenderedMessage) []model.DeliveryQueueItem {
	payload := model.DeliveryPayload{
		ThreadID: event.ThreadID,
		TurnID:   event.TurnID,
		EventID:  event.TerminalKey,
		Rendered: &message,
	}
	return []model.DeliveryQueueItem{{
		EventID:       event.TerminalKey,
		ChatKey:       model.ChatKey(target.ChatID, target.TopicID),
		ChatID:        target.ChatID,
		TopicID:       target.TopicID,
		ThreadID:      event.ThreadID,
		Kind:          "notifier_terminal",
		Status:        model.DeliveryStatusPending,
		GroupID:       event.TerminalKey,
		SequenceNo:    1,
		SequenceCount: 1,
		AvailableAt:   model.NowString(),
		PayloadJSON:   storage.MustJSON(payload),
		CreatedAt:     model.NowString(),
		UpdatedAt:     model.NowString(),
	}}
}
