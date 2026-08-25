package daemon

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/alclssna33/codex_to_telegram/internal/model"
)

func TestMinimalSourceTurnRelationClassifiesNestedTurns(t *testing.T) {
	payload := threadWithTurns("source-1", "source-turn-1", "source-turn-2", "source-turn-3")
	nestedPayload := threadWithTurns("source-1", "source-turn-1", "source-turn-2")
	nestedPayload["thread"].(map[string]any)["metadata"] = map[string]any{
		"turns": []any{map[string]any{"id": "source-turn-3", "status": "completed"}},
	}

	relation, err := minimalSourceTurnRelation(payload, "source-turn-2", "source-turn-1")
	if err != nil {
		t.Fatal(err)
	}
	if relation != minimalTurnAtOrBefore {
		t.Fatalf("relation before anchor = %q, want %q", relation, minimalTurnAtOrBefore)
	}

	relation, err = minimalSourceTurnRelation(payload, "source-turn-2", "source-turn-2")
	if err != nil {
		t.Fatal(err)
	}
	if relation != minimalTurnAtOrBefore {
		t.Fatalf("relation at anchor = %q, want %q", relation, minimalTurnAtOrBefore)
	}

	relation, err = minimalSourceTurnRelation(payload, "source-turn-2", "source-turn-3")
	if err != nil {
		t.Fatal(err)
	}
	if relation != minimalTurnAfter {
		t.Fatalf("relation after anchor = %q, want %q", relation, minimalTurnAfter)
	}

	relation, err = minimalSourceTurnRelation(nestedPayload, "source-turn-2", "source-turn-3")
	if err != nil {
		t.Fatal(err)
	}
	if relation != minimalTurnAfter {
		t.Fatalf("nested relation after anchor = %q, want %q", relation, minimalTurnAfter)
	}
}

func TestMinimalSourceTurnRelationFailsClosedForMissingOrDuplicateIDs(t *testing.T) {
	cases := []struct {
		name          string
		payload       map[string]any
		anchorTurnID  string
		routedTurnID  string
		wantErrTarget error
	}{
		{name: "missing anchor", payload: threadWithTurns("source-1", "source-turn-1"), anchorTurnID: "missing-anchor", routedTurnID: "source-turn-1", wantErrTarget: errSourceTurnUnavailable},
		{name: "missing routed", payload: threadWithTurns("source-1", "source-turn-1"), anchorTurnID: "source-turn-1", routedTurnID: "missing-route", wantErrTarget: errSourceTurnUnavailable},
		{name: "duplicate anchor", payload: threadWithTurns("source-1", "source-turn-1", "source-turn-1"), anchorTurnID: "source-turn-1", routedTurnID: "source-turn-1", wantErrTarget: errSourceTurnUnavailable},
		{name: "blank routed", payload: threadWithTurns("source-1", "source-turn-1"), anchorTurnID: "source-turn-1", routedTurnID: "", wantErrTarget: errSourceTurnUnavailable},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if relation, err := minimalSourceTurnRelation(tc.payload, tc.anchorTurnID, tc.routedTurnID); !errors.Is(err, tc.wantErrTarget) || relation != "" {
				t.Fatalf("relation=%q err=%v, want fail-closed %v", relation, err, tc.wantErrTarget)
			}
		})
	}
}

func TestPreLinkOriginalTurnRoutesToCanonicalLinkedThread(t *testing.T) {
	svc, _ := newMinimalService(t)
	live := newRouterSession()
	useRouterSession(svc, live)
	workers := installWorkerFactory(svc)
	seedReadyLinkedThread(t, svc, 7, 0, "source-1", "source-turn-2", "linked-1")
	setNextMinimalWorkerGeneration(t, svc, 1)
	if err := svc.store.PutMessageRoute(context.Background(), model.MessageRoute{ChatID: 7, MessageID: 99, ThreadID: "source-1", TurnID: "source-turn-1", CreatedAt: model.NowString()}); err != nil {
		t.Fatal(err)
	}
	live.threadReads = map[string]map[string]any{
		"source-1": threadWithTurns("source-1", "source-turn-1", "source-turn-2"),
		"linked-1": threadWithTurns("linked-1", "linked-turn-1"),
	}

	response := submitMinimal(t, svc, model.InboundText{ChatID: 7, UserID: 7, ReplyToMessageID: 99, Text: "앵커 이전에서 계속", ReceivedAt: svc.now()})

	if response.ThreadID != "linked-1" || response.Text != "작업을 시작했습니다." {
		t.Fatalf("response=%#v, want linked thread start", response)
	}
	worker := workers.Single(t)
	if got := worker.callSequence(); !reflect.DeepEqual(got, []string{"thread/resume:linked-1", "turn/start:linked-1"}) {
		t.Fatalf("worker calls=%#v, want canonical linked resume/start", got)
	}
	if got := worker.forkCallCount(); got != 0 {
		t.Fatalf("fork calls=%d, want none", got)
	}
	link, err := svc.store.GetMinimalLinkedThread(context.Background(), 7, 0, "source-1")
	if err != nil {
		t.Fatal(err)
	}
	if link == nil || link.State != model.MinimalLinkedTelegramRunning || link.ActiveTurnID != response.TurnID || link.WorkerGeneration != 2 {
		t.Fatalf("link after canonical start=%#v response=%#v", link, response)
	}
	assertRouterSessionNoMutations(t, live)
}

func TestPostLinkOriginalTurnIsRejectedWithoutWorker(t *testing.T) {
	svc, _ := newMinimalService(t)
	live := newRouterSession()
	useRouterSession(svc, live)
	workers := installWorkerFactory(svc)
	seedReadyLinkedThread(t, svc, 7, 0, "source-1", "source-turn-1", "linked-1")
	if err := svc.store.PutMessageRoute(context.Background(), model.MessageRoute{ChatID: 7, MessageID: 99, ThreadID: "source-1", TurnID: "source-turn-2", CreatedAt: model.NowString()}); err != nil {
		t.Fatal(err)
	}
	live.threadReads = map[string]map[string]any{
		"source-1": threadWithTurns("source-1", "source-turn-1", "source-turn-2"),
	}

	response, err := svc.minimalRouter.Submit(context.Background(), model.InboundText{ChatID: 7, UserID: 7, ReplyToMessageID: 99, Text: "최신 답변에서 계속", ReceivedAt: svc.now()})
	if err != nil {
		t.Fatal(err)
	}
	if response == nil || !strings.Contains(response.Text, "자동으로 합칠 수 없습니다") {
		t.Fatalf("response=%#v, want divergence guidance", response)
	}
	if response.ThreadID != "linked-1" {
		t.Fatalf("response thread=%q, want linked diagnostic id", response.ThreadID)
	}
	if got := len(workers.Sessions()); got != 0 {
		t.Fatalf("worker sessions=%d, want none", got)
	}
	for _, threadID := range []string{"source-1", "linked-1"} {
		queued, queueErr := svc.store.ClaimPendingCommand(context.Background(), threadID)
		if queueErr != nil {
			t.Fatal(queueErr)
		}
		if queued != nil {
			t.Fatalf("divergence queued prompt on %s: %#v", threadID, queued)
		}
	}
	assertRouterSessionNoMutations(t, live)
}

func TestMissingLinkedSourceTurnIsRejectedWithoutWorker(t *testing.T) {
	svc, _ := newMinimalService(t)
	live := newRouterSession()
	useRouterSession(svc, live)
	workers := installWorkerFactory(svc)
	seedReadyLinkedThread(t, svc, 7, 0, "source-1", "source-turn-1", "linked-1")
	if err := svc.store.PutMessageRoute(context.Background(), model.MessageRoute{ChatID: 7, MessageID: 99, ThreadID: "source-1", TurnID: "missing-turn", CreatedAt: model.NowString()}); err != nil {
		t.Fatal(err)
	}
	live.threadReads = map[string]map[string]any{
		"source-1": threadWithTurns("source-1", "source-turn-1"),
	}

	response, err := svc.minimalRouter.Submit(context.Background(), model.InboundText{ChatID: 7, UserID: 7, ReplyToMessageID: 99, Text: "사라진 답변에서 계속", ReceivedAt: svc.now()})
	if err != nil {
		t.Fatal(err)
	}
	if response == nil || !strings.Contains(response.Text, "기준 답변을 확인할 수 없어") {
		t.Fatalf("response=%#v, want fail-closed guidance", response)
	}
	if got := len(workers.Sessions()); got != 0 {
		t.Fatalf("worker sessions=%d, want none", got)
	}
	if queued, queueErr := svc.store.ClaimPendingCommand(context.Background(), "linked-1"); queueErr != nil || queued != nil {
		t.Fatalf("queued=%#v err=%v, want no pending prompt", queued, queueErr)
	}
	assertRouterSessionNoMutations(t, live)
}

func TestLinkedRoutePassesUnchanged(t *testing.T) {
	svc, _ := newMinimalService(t)
	live := newRouterSession()
	useRouterSession(svc, live)
	workers := installWorkerFactory(svc)
	seedReadyLinkedThread(t, svc, 7, 0, "source-1", "source-turn-1", "linked-1")
	setNextMinimalWorkerGeneration(t, svc, 1)
	if err := svc.store.PutMessageRoute(context.Background(), model.MessageRoute{ChatID: 7, MessageID: 99, ThreadID: "linked-1", TurnID: "linked-turn-9", CreatedAt: model.NowString()}); err != nil {
		t.Fatal(err)
	}
	live.threadReads = map[string]map[string]any{
		"linked-1": threadWithTurns("linked-1", "linked-turn-9"),
	}

	response := submitMinimal(t, svc, model.InboundText{ChatID: 7, UserID: 7, ReplyToMessageID: 99, Text: "linked에서 계속", ReceivedAt: svc.now()})

	if response.ThreadID != "linked-1" {
		t.Fatalf("response=%#v, want unchanged linked route", response)
	}
	if got := workers.Single(t).callSequence(); !reflect.DeepEqual(got, []string{"thread/resume:linked-1", "turn/start:linked-1"}) {
		t.Fatalf("worker calls=%#v, want linked resume/start", got)
	}
	link, err := svc.store.GetMinimalLinkedThread(context.Background(), 7, 0, "source-1")
	if err != nil {
		t.Fatal(err)
	}
	if link == nil || link.State != model.MinimalLinkedTelegramRunning || link.ActiveTurnID != response.TurnID || link.WorkerGeneration != 2 {
		t.Fatalf("link after linked start=%#v response=%#v", link, response)
	}
}

func threadWithTurns(threadID string, turnIDs ...string) map[string]any {
	turns := make([]any, 0, len(turnIDs))
	for _, id := range turnIDs {
		turns = append(turns, map[string]any{"id": id, "status": "completed"})
	}
	return map[string]any{"thread": map[string]any{
		"id":     threadID,
		"cwd":    "",
		"status": "completed",
		"turns":  turns,
	}}
}

func setNextMinimalWorkerGeneration(t *testing.T, svc *Service, generation uint64) {
	t.Helper()
	if svc == nil || svc.minimalWorkers == nil {
		t.Fatal("minimal worker manager is unavailable")
	}
	svc.minimalWorkers.mu.Lock()
	defer svc.minimalWorkers.mu.Unlock()
	svc.minimalWorkers.nextGeneration = generation
}
