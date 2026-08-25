package daemon

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alclssna33/codex_to_telegram/internal/config"
	"github.com/alclssna33/codex_to_telegram/internal/control"
	"github.com/alclssna33/codex_to_telegram/internal/model"
	"github.com/alclssna33/codex_to_telegram/internal/securestore"
	"github.com/alclssna33/codex_to_telegram/internal/storage"
)

var notifierTestNow = time.Date(2026, 8, 24, 9, 0, 0, 0, time.UTC)

func TestNotifierDiscoveryUsesInteractiveSourcesWithoutCWD(t *testing.T) {
	svc := newNotifierService(t)
	fake := &notifierObserverSession{
		pages: map[string]map[string]any{
			"": threadListPayload(notifierThreadListItem("thread-cli", "CLI", "/work/one", 100, "running", "turn-cli", "cli")),
		},
	}
	useNotifierPollSession(svc, fake)

	if err := svc.discoverNotifierThreads(context.Background()); err != nil {
		t.Fatal(err)
	}

	if len(fake.listOptionsCalls) != 1 {
		t.Fatalf("ThreadListWithOptions calls = %#v, want one head call", fake.listOptionsCalls)
	}
	assertNotifierListOptions(t, fake.listOptionsCalls[0], "")
	if fake.threadListCalls != 0 {
		t.Fatalf("ThreadList fallback calls = %d, want 0", fake.threadListCalls)
	}
	assertNotifierDueIDs(t, svc, []string{"thread-cli"})
}

func TestNotifierInitialTerminalIsBaselineOnly(t *testing.T) {
	svc := newNotifierService(t)
	ctx := context.Background()
	if err := svc.store.SetGlobalObserverTarget(ctx, 7, 0, true); err != nil {
		t.Fatal(err)
	}
	activation, err := svc.store.EnsureNotifierActivation(ctx, svc.now())
	if err != nil {
		t.Fatal(err)
	}
	fake := &notifierObserverSession{
		pages: map[string]map[string]any{
			"": threadListPayload(notifierThreadListItem("historical", "Old terminal", "/work/old", activation-1, "completed", "", "cli")),
		},
		threadReads: map[string]map[string]any{
			"historical": notifierThreadReadPayload("historical", "Old terminal", "/work/old", "old-turn", "completed", "old result", activation-1),
		},
	}
	useNotifierPollSession(svc, fake)

	if err := svc.discoverNotifierThreads(ctx); err != nil {
		t.Fatal(err)
	}
	if err := svc.pollNotifierThreads(ctx); err != nil {
		t.Fatal(err)
	}
	if got := notifierThreadReadIDs(fake.threadReadCalls); len(got) != 0 {
		t.Fatalf("ThreadRead calls = %v, want none for pre-activation history", got)
	}

	if count := countDeliveryKind(t, svc, "notifier_terminal"); count != 0 {
		t.Fatalf("notifier_terminal deliveries = %d, want 0", count)
	}
	observation, err := svc.store.NotifierObservation(ctx, "historical")
	if err != nil || observation == nil {
		t.Fatalf("observation = %#v, %v", observation, err)
	}
	if !observation.BaselineReady || observation.ReadRequired {
		t.Fatalf("observation = %#v, want baseline ready and not due", observation)
	}
}

func TestNotifierPersistedPreActivationBacklogIsBaselinedWithoutReads(t *testing.T) {
	svc := newNotifierService(t)
	ctx := context.Background()
	activation, err := svc.store.EnsureNotifierActivation(ctx, svc.now())
	if err != nil {
		t.Fatal(err)
	}
	for _, threadID := range []string{"persisted-old-a", "persisted-old-b"} {
		if err := svc.store.ObserveNotifierThread(ctx, threadID, activation-1, svc.now()); err != nil {
			t.Fatal(err)
		}
	}
	fake := &notifierObserverSession{}
	useNotifierPollSession(svc, fake)

	if err := svc.pollNotifierThreads(ctx); err != nil {
		t.Fatal(err)
	}
	if got := notifierThreadReadIDs(fake.threadReadCalls); len(got) != 0 {
		t.Fatalf("ThreadRead calls = %v, want no reads for persisted pre-activation backlog", got)
	}
	for _, threadID := range []string{"persisted-old-a", "persisted-old-b"} {
		observation, err := svc.store.NotifierObservation(ctx, threadID)
		if err != nil || observation == nil || !observation.BaselineReady || observation.ReadRequired {
			t.Fatalf("observation %q = %#v, %v; want baselined and not due", threadID, observation, err)
		}
	}
}

func TestNotifierActiveToCompletedQueuesOneNotification(t *testing.T) {
	svc := newNotifierService(t)
	ctx := context.Background()
	if err := svc.store.SetGlobalObserverTarget(ctx, 7, 0, true); err != nil {
		t.Fatal(err)
	}
	fake := &notifierObserverSession{
		pages: map[string]map[string]any{
			"": threadListPayload(notifierThreadListItem("active", "새 대화", "/workspace/unregistered-work", notifierTestNow.Unix()+1, "running", "turn-1", "cli")),
		},
		threadReads: map[string]map[string]any{
			"active": notifierThreadReadPayload("active", "새 대화", "/workspace/unregistered-work", "turn-1", "running", "", notifierTestNow.Unix()+1),
		},
	}
	useNotifierPollSession(svc, fake)

	if err := svc.discoverNotifierThreads(ctx); err != nil {
		t.Fatal(err)
	}
	if err := svc.pollNotifierThreads(ctx); err != nil {
		t.Fatal(err)
	}
	assertNotifierDueIDs(t, svc, []string{})
	fake.pages[""] = threadListPayload(notifierThreadListItem("active", "새 대화", "/workspace/unregistered-work", notifierTestNow.Unix()+2, "completed", "turn-1", "cli"))
	fake.threadReads["active"] = notifierThreadReadPayload("active", "새 대화", "/workspace/unregistered-work", "turn-1", "completed", "작업 결과입니다.", notifierTestNow.Unix()+2)
	if err := svc.discoverNotifierThreads(ctx); err != nil {
		t.Fatal(err)
	}
	if err := svc.pollNotifierThreads(ctx); err != nil {
		t.Fatal(err)
	}

	sender := &recordingSender{}
	svc.SetSender(sender)
	svc.processDeliveryBatch(ctx)
	if got := sender.joinedText(); !strings.Contains(got, "폴더: unregistered-work") ||
		!strings.Contains(got, "대화: 새 대화") ||
		!strings.Contains(got, "요약: 작업 결과입니다.") {
		t.Fatalf("notification = %q", got)
	}
	if fake.resumeCalls != 0 || fake.startCalls != 0 || fake.forkCalls != 0 || fake.turnStartCalls != 0 || fake.responseCalls != 0 {
		t.Fatalf("mutation calls: %#v", fake)
	}
	if count := countDeliveryKind(t, svc, "notifier_terminal"); count != 1 {
		t.Fatalf("notifier_terminal deliveries = %d, want 1", count)
	}
	db, err := sql.Open("sqlite", svc.store.Path())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var deliveryStatus string
	if err := db.QueryRowContext(ctx, `SELECT delivery_status FROM terminal_events WHERE terminal_key=?`, "active:turn-1:completed").Scan(&deliveryStatus); err != nil {
		t.Fatal(err)
	}
	if deliveryStatus != model.DeliveryStatusDelivered {
		t.Fatalf("terminal event delivery status = %q, want %q", deliveryStatus, model.DeliveryStatusDelivered)
	}
}

func TestNotifierInterruptedWithoutFinalAwaitsNewListUpdate(t *testing.T) {
	svc := newNotifierService(t)
	ctx := context.Background()
	if err := svc.store.SetGlobalObserverTarget(ctx, 7, 0, true); err != nil {
		t.Fatal(err)
	}
	fake := &notifierObserverSession{
		pages: map[string]map[string]any{
			"": threadListPayload(notifierThreadListItem("cold", "Cold read", "/work/cold", notifierTestNow.Unix()+1, "running", "turn-cold", "cli")),
		},
		threadReads: map[string]map[string]any{
			"cold": notifierThreadReadPayloadWithoutFinal("cold", "Cold read", "/work/cold", "turn-cold", "running", notifierTestNow.Unix()+1),
		},
	}
	useNotifierPollSession(svc, fake)

	if err := svc.discoverNotifierThreads(ctx); err != nil {
		t.Fatal(err)
	}
	if err := svc.pollNotifierThreads(ctx); err != nil {
		t.Fatal(err)
	}
	fake.pages[""] = threadListPayload(notifierThreadListItem("cold", "Cold read", "/work/cold", notifierTestNow.Unix()+2, "interrupted", "turn-cold", "cli"))
	fake.threadReads["cold"] = notifierThreadReadPayloadWithoutFinal("cold", "Cold read", "/work/cold", "turn-cold", "interrupted", notifierTestNow.Unix()+2)
	if err := svc.discoverNotifierThreads(ctx); err != nil {
		t.Fatal(err)
	}
	if err := svc.pollNotifierThreads(ctx); err != nil {
		t.Fatal(err)
	}
	if count := countDeliveryKind(t, svc, "notifier_terminal"); count != 0 {
		t.Fatalf("notifier_terminal deliveries after provisional interrupted = %d, want 0", count)
	}
	assertNotifierDueIDs(t, svc, []string{})

	fake.pages[""] = threadListPayload(notifierThreadListItem("cold", "Cold read", "/work/cold", notifierTestNow.Unix()+3, "completed", "turn-cold", "cli"))
	fake.threadReads["cold"] = notifierThreadReadPayload("cold", "Cold read", "/work/cold", "turn-cold", "completed", "real result", notifierTestNow.Unix()+3)
	if err := svc.discoverNotifierThreads(ctx); err != nil {
		t.Fatal(err)
	}
	if err := svc.pollNotifierThreads(ctx); err != nil {
		t.Fatal(err)
	}
	if count := countDeliveryKind(t, svc, "notifier_terminal"); count != 1 {
		t.Fatalf("notifier_terminal deliveries after completed = %d, want 1", count)
	}
	assertNotifierDueIDs(t, svc, []string{})
	if err := svc.pollNotifierThreads(ctx); err != nil {
		t.Fatal(err)
	}
	if count := countDeliveryKind(t, svc, "notifier_terminal"); count != 1 {
		t.Fatalf("notifier_terminal deliveries after duplicate poll = %d, want 1", count)
	}
}

func TestNotifierCompletedPriorTurnIsNotSkippedByNewerActiveTurn(t *testing.T) {
	svc := newNotifierService(t)
	ctx := context.Background()
	if err := svc.store.SetGlobalObserverTarget(ctx, 7, 0, true); err != nil {
		t.Fatal(err)
	}
	fake := &notifierObserverSession{
		pages: map[string]map[string]any{
			"": threadListPayload(notifierThreadListItem("quick", "Quick followup", "/work/quick", notifierTestNow.Unix()+1, "running", "turn-1", "cli")),
		},
		threadReads: map[string]map[string]any{
			"quick": notifierThreadReadPayloadWithoutFinal("quick", "Quick followup", "/work/quick", "turn-1", "running", notifierTestNow.Unix()+1),
		},
	}
	useNotifierPollSession(svc, fake)

	if err := svc.discoverNotifierThreads(ctx); err != nil {
		t.Fatal(err)
	}
	if err := svc.pollNotifierThreads(ctx); err != nil {
		t.Fatal(err)
	}
	fake.pages[""] = threadListPayload(notifierThreadListItem("quick", "Quick followup", "/work/quick", notifierTestNow.Unix()+2, "running", "turn-2", "cli"))
	fake.threadReads["quick"] = notifierThreadReadPayloadWithTurns("quick", "Quick followup", "/work/quick", "running", notifierTestNow.Unix()+2,
		notifierTurnPayload("turn-1", "completed", "first result", true),
		notifierTurnPayload("turn-2", "running", "", false),
	)
	if err := svc.discoverNotifierThreads(ctx); err != nil {
		t.Fatal(err)
	}
	if err := svc.pollNotifierThreads(ctx); err != nil {
		t.Fatal(err)
	}

	if count := countDeliveryKind(t, svc, "notifier_terminal"); count != 1 {
		t.Fatalf("notifier_terminal deliveries = %d, want completed prior turn notification", count)
	}
	items, err := svc.store.ClaimDeliveryBatch(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].EventID != "quick:turn-1:completed" {
		t.Fatalf("delivery items = %#v, want turn-1 completed", items)
	}
	assertNotifierDueIDs(t, svc, []string{})
}

func TestNotifierMultipleUnseenTerminalTurnsAreQueuedInOrder(t *testing.T) {
	svc := newNotifierService(t)
	ctx := context.Background()
	if err := svc.store.SetGlobalObserverTarget(ctx, 7, 0, true); err != nil {
		t.Fatal(err)
	}
	fake := &notifierObserverSession{
		pages: map[string]map[string]any{
			"": threadListPayload(notifierThreadListItem("multi", "Multi", "/work/multi", notifierTestNow.Unix()+1, "running", "turn-0", "cli")),
		},
		threadReads: map[string]map[string]any{
			"multi": notifierThreadReadPayloadWithoutFinal("multi", "Multi", "/work/multi", "turn-0", "running", notifierTestNow.Unix()+1),
		},
	}
	useNotifierPollSession(svc, fake)

	if err := svc.discoverNotifierThreads(ctx); err != nil {
		t.Fatal(err)
	}
	if err := svc.pollNotifierThreads(ctx); err != nil {
		t.Fatal(err)
	}
	fake.pages[""] = threadListPayload(notifierThreadListItem("multi", "Multi", "/work/multi", notifierTestNow.Unix()+2, "completed", "turn-2", "cli"))
	fake.threadReads["multi"] = notifierThreadReadPayloadWithTurns("multi", "Multi", "/work/multi", "completed", notifierTestNow.Unix()+2,
		notifierTurnPayload("turn-0", "completed", "zero result", true),
		notifierTurnPayload("turn-1", "completed", "first result", true),
		notifierTurnPayload("turn-2", "completed", "second result", true),
	)
	if err := svc.discoverNotifierThreads(ctx); err != nil {
		t.Fatal(err)
	}
	if err := svc.pollNotifierThreads(ctx); err != nil {
		t.Fatal(err)
	}

	items, err := svc.store.ClaimDeliveryBatch(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	got := make([]string, 0, len(items))
	for _, item := range items {
		got = append(got, item.EventID)
	}
	want := []string{"multi:turn-0:completed", "multi:turn-1:completed", "multi:turn-2:completed"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("delivery order = %v, want %v", got, want)
	}
}

func TestNotifierPollContinuesAfterOneThreadReadError(t *testing.T) {
	svc := newNotifierService(t)
	ctx := context.Background()
	if err := svc.store.SetGlobalObserverTarget(ctx, 7, 0, true); err != nil {
		t.Fatal(err)
	}
	readErr := errors.New("injected stale thread read timeout")
	fake := &notifierObserverSession{
		threadReads: map[string]map[string]any{
			"later-completed": notifierThreadReadPayload("later-completed", "Later completed", "/work/later", "turn-later", "completed", "later result", notifierTestNow.Unix()+1),
		},
		threadReadErrs: map[string]error{
			"stale-timeout": readErr,
		},
	}
	useNotifierPollSession(svc, fake)
	if err := svc.store.ObserveNotifierThread(ctx, "stale-timeout", notifierTestNow.Unix()+2, svc.now()); err != nil {
		t.Fatal(err)
	}
	if err := svc.store.ObserveNotifierThread(ctx, "later-completed", notifierTestNow.Unix()+1, svc.now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}

	err := svc.pollNotifierThreads(ctx)
	if !errors.Is(err, readErr) {
		t.Fatalf("poll error = %v, want %v", err, readErr)
	}
	if got := notifierThreadReadIDs(fake.threadReadCalls); !reflect.DeepEqual(got, []string{"stale-timeout", "later-completed"}) {
		t.Fatalf("ThreadRead calls = %v, want stale-timeout then later-completed", got)
	}
	if count := countDeliveryKind(t, svc, "notifier_terminal"); count != 1 {
		t.Fatalf("notifier_terminal deliveries = %d, want later completed notification", count)
	}
	assertNotifierDueIDs(t, svc, []string{"stale-timeout"})

	if err := svc.pollNotifierThreads(ctx); !errors.Is(err, readErr) {
		t.Fatalf("retry poll error = %v, want %v", err, readErr)
	}
	if count := countDeliveryKind(t, svc, "notifier_terminal"); count != 1 {
		t.Fatalf("notifier_terminal deliveries after retry = %d, want no duplicate", count)
	}
}

func TestNotifierThreadReadDeadlineDefersOnlyThatThreadAndContinuesBatch(t *testing.T) {
	svc := newNotifierService(t)
	ctx := context.Background()
	if err := svc.store.SetGlobalObserverTarget(ctx, 7, 0, true); err != nil {
		t.Fatal(err)
	}
	activation, err := svc.store.EnsureNotifierActivation(ctx, svc.now())
	if err != nil {
		t.Fatal(err)
	}
	fake := &notifierObserverSession{
		threadReads: map[string]map[string]any{
			"later": notifierThreadReadPayload("later", "Later", "/work/later", "turn-later", "completed", "later result", activation+1),
		},
		threadReadErrs: map[string]error{"deadline": context.DeadlineExceeded},
	}
	useNotifierPollSession(svc, fake)
	if err := svc.store.ObserveNotifierThread(ctx, "deadline", activation+2, svc.now()); err != nil {
		t.Fatal(err)
	}
	if err := svc.store.ObserveNotifierThread(ctx, "later", activation+1, svc.now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}

	err = svc.pollNotifierThreads(ctx)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("poll error = %v, want deadline", err)
	}
	if got := notifierThreadReadIDs(fake.threadReadCalls); !reflect.DeepEqual(got, []string{"deadline", "later"}) {
		t.Fatalf("ThreadRead calls = %v, want deadline then later", got)
	}
	repair, err := svc.store.GetState(ctx, "control.repair_request")
	if err != nil || repair != "" {
		t.Fatalf("repair request = %q, %v; want no global repair", repair, err)
	}
	if count := countDeliveryKind(t, svc, "notifier_terminal"); count != 1 {
		t.Fatalf("notifier_terminal deliveries = %d, want later notification", count)
	}
	due, err := svc.store.ListNotifierObservationsDueAt(ctx, 10, svc.now())
	if err != nil || len(due) != 0 {
		t.Fatalf("current due=%#v err=%v, want deadline deferred", due, err)
	}
	due, err = svc.store.ListNotifierObservationsDueAt(ctx, 10, svc.now().Add(notifierThreadReadRetryDelay))
	if err != nil || len(due) != 1 || due[0].ThreadID != "deadline" {
		t.Fatalf("future due=%#v err=%v, want deferred deadline", due, err)
	}
}

func TestNotifierThreadListDeadlineRequestsPollRepair(t *testing.T) {
	svc := newNotifierService(t)
	svc.cfg.Profile = "notifier"
	ctx := context.Background()
	fake := &notifierObserverSession{threadListErr: context.DeadlineExceeded}
	useNotifierPollSession(svc, fake)

	svc.refreshObserverIndex(ctx)

	repair, err := svc.store.GetState(ctx, "control.repair_request")
	if err != nil || !strings.Contains(repair, "notifier_thread_list_deadline") {
		t.Fatalf("repair request = %q, %v; want sanitized notifier list deadline repair", repair, err)
	}
}

func TestNotifierObservationCycleSkipsConcurrentBootstrap(t *testing.T) {
	svc := newNotifierService(t)
	svc.cfg.Profile = "notifier"
	ctx := context.Background()
	activation, err := svc.store.EnsureNotifierActivation(ctx, svc.now())
	if err != nil {
		t.Fatal(err)
	}
	readStarted := make(chan struct{})
	releaseRead := make(chan struct{})
	fake := &notifierObserverSession{
		pages: map[string]map[string]any{
			"": threadListPayload(notifierThreadListItem("current", "Current", "/work/current", activation+1, "running", "turn-current", "vscode")),
		},
		threadReads: map[string]map[string]any{
			"current": notifierThreadReadPayload("current", "Current", "/work/current", "turn-current", "running", "", activation+1),
		},
		threadReadStarted: readStarted,
		threadReadRelease: releaseRead,
	}
	useNotifierPollSession(svc, fake)
	firstDone := make(chan struct{})
	go func() {
		svc.bootstrapTrackedState(ctx)
		close(firstDone)
	}()
	<-readStarted
	fake.pages[""] = threadListPayload(
		notifierThreadListItem("current", "Current", "/work/current", activation+1, "running", "turn-current", "vscode"),
		notifierThreadListItem("historic-between-baseline-and-due", "Historic", "/work/old", activation-1, "completed", "", "vscode"),
	)
	secondDone := make(chan struct{})
	go func() {
		svc.bootstrapTrackedState(ctx)
		close(secondDone)
	}()
	select {
	case <-secondDone:
	case <-time.After(250 * time.Millisecond):
		close(releaseRead)
		<-firstDone
		t.Fatal("concurrent notifier bootstrap did not return while the first cycle was active")
	}
	close(releaseRead)
	<-firstDone

	if got := notifierThreadReadIDs(fake.threadReadCalls); !reflect.DeepEqual(got, []string{"current"}) {
		t.Fatalf("ThreadRead calls = %v, want one bounded read from the first cycle", got)
	}
	if got := notifierListCursors(fake.listOptionsCalls); !reflect.DeepEqual(got, []string{""}) {
		t.Fatalf("ThreadList calls = %v, want one discovery call from the first cycle", got)
	}
	observation, err := svc.store.NotifierObservation(ctx, "historic-between-baseline-and-due")
	if err != nil || observation != nil {
		t.Fatalf("historic observation = %#v, %v; want concurrent bootstrap to admit none", observation, err)
	}
}

func TestNotifierCompletedDeliverySurvivesRestartWithoutDuplicate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.sqlite")
	protector := securestore.NewDeterministicTestProtector()
	svc := newNotifierServiceAtPath(t, path, protector)
	ctx := context.Background()
	if err := svc.store.SetGlobalObserverTarget(ctx, 7, 0, true); err != nil {
		t.Fatal(err)
	}
	fake := &notifierObserverSession{
		pages: map[string]map[string]any{
			"": threadListPayload(notifierThreadListItem("crash", "Crash safe", "/work/crash", notifierTestNow.Unix()+1, "completed", "", "cli")),
		},
		threadReads: map[string]map[string]any{
			"crash": notifierThreadReadPayload("crash", "Crash safe", "/work/crash", "turn-crash", "completed", "first result", notifierTestNow.Unix()+1),
		},
	}
	useNotifierPollSession(svc, fake)
	if err := svc.discoverNotifierThreads(ctx); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", svc.store.Path())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TRIGGER reject_notifier_record BEFORE UPDATE OF baseline_ready ON notifier_observations WHEN NEW.baseline_ready = 1 BEGIN SELECT RAISE(ABORT, 'forced notifier record failure'); END`); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	_ = db.Close()

	if err := svc.pollNotifierThreads(ctx); err == nil {
		t.Fatal("first poll error = nil, want forced record failure after enqueue")
	}
	if count := countDeliveryKind(t, svc, "notifier_terminal"); count != 1 {
		t.Fatalf("delivery after failed record = %d, want 1", count)
	}
	if err := svc.store.Close(); err != nil {
		t.Fatal(err)
	}

	restarted := newNotifierServiceAtPath(t, path, protector)
	useNotifierPollSession(restarted, fake)
	db, err = sql.Open("sqlite", restarted.store.Path())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`DROP TRIGGER reject_notifier_record`); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	_ = db.Close()
	if err := restarted.pollNotifierThreads(ctx); err != nil {
		t.Fatal(err)
	}
	if count := countDeliveryKind(t, restarted, "notifier_terminal"); count != 1 {
		t.Fatalf("delivery after restart retry = %d, want still 1", count)
	}
	observation, err := restarted.store.NotifierObservation(ctx, "crash")
	if err != nil || observation == nil || observation.ReadRequired {
		t.Fatalf("observation after restart retry = %#v, %v; want not due", observation, err)
	}
}

func TestNotifierPollNeverCallsMutationMethods(t *testing.T) {
	svc := newNotifierService(t)
	ctx := context.Background()
	if err := svc.store.SetGlobalObserverTarget(ctx, 7, 0, true); err != nil {
		t.Fatal(err)
	}
	fake := &notifierObserverSession{
		pages: map[string]map[string]any{
			"": threadListPayload(notifierThreadListItem("readonly", "Read only", "/work/readonly", notifierTestNow.Unix()+1, "completed", "", "cli")),
		},
		threadReads: map[string]map[string]any{
			"readonly": notifierThreadReadPayload("readonly", "Read only", "/work/readonly", "turn-readonly", "completed", "done", notifierTestNow.Unix()+1),
		},
	}
	useNotifierPollSession(svc, fake)

	if err := svc.discoverNotifierThreads(ctx); err != nil {
		t.Fatal(err)
	}
	if err := svc.pollNotifierThreads(ctx); err != nil {
		t.Fatal(err)
	}

	if fake.resumeCalls != 0 || fake.startCalls != 0 || fake.forkCalls != 0 || fake.turnStartCalls != 0 || fake.responseCalls != 0 {
		t.Fatalf("mutation calls: %#v", fake)
	}
}

func TestNotifierHeadAndSweepCoverMoreThanOneHundredThreads(t *testing.T) {
	svc := newNotifierService(t)
	head := make([]map[string]any, 0, notifierCatalogLimit)
	for i := 0; i < notifierCatalogLimit; i++ {
		head = append(head, notifierThreadListItem(fmt.Sprintf("head-%03d", i), "Head", "/work/head", int64(1000+i), "running", "turn", "cli"))
	}
	fake := &notifierObserverSession{pages: map[string]map[string]any{
		"":            threadListPayloadWithCursor("cursor-2", head...),
		"cursor-2":    threadListPayload(notifierThreadListItem("sweep-100", "Sweep", "/work/sweep", 2000, "running", "turn", "vscode")),
		"cursor-next": threadListPayload(notifierThreadListItem("not-yet", "Not yet", "/work/not-yet", 3000, "running", "turn", "cli")),
	}}
	useNotifierPollSession(svc, fake)

	if err := svc.discoverNotifierThreads(context.Background()); err != nil {
		t.Fatal(err)
	}

	due, err := svc.store.ListNotifierObservationsDue(context.Background(), notifierCatalogLimit+2)
	if err != nil {
		t.Fatal(err)
	}
	if len(due) != notifierCatalogLimit+1 {
		t.Fatalf("due observations = %d, want %d", len(due), notifierCatalogLimit+1)
	}
	if cursors := notifierListCursors(fake.listOptionsCalls); !reflect.DeepEqual(cursors, []string{"", "cursor-2"}) {
		t.Fatalf("list cursors = %v, want head and one sweep page", cursors)
	}
}

func TestNotifierTerminalWaitsForObserverTarget(t *testing.T) {
	svc := newNotifierService(t)
	ctx := context.Background()
	fake := &notifierObserverSession{
		pages: map[string]map[string]any{
			"": threadListPayload(notifierThreadListItem("waiting", "Wait target", "/work/waiting", notifierTestNow.Unix()+1, "completed", "", "cli")),
		},
		threadReads: map[string]map[string]any{
			"waiting": notifierThreadReadPayload("waiting", "Wait target", "/work/waiting", "turn-waiting", "completed", "done after activation", notifierTestNow.Unix()+1),
		},
	}
	useNotifierPollSession(svc, fake)

	if err := svc.discoverNotifierThreads(ctx); err != nil {
		t.Fatal(err)
	}
	if err := svc.pollNotifierThreads(ctx); err != nil {
		t.Fatal(err)
	}
	if count := countDeliveryKind(t, svc, "notifier_terminal"); count != 0 {
		t.Fatalf("delivery without target = %d, want 0", count)
	}
	observation, err := svc.store.NotifierObservation(ctx, "waiting")
	if err != nil || observation == nil || !observation.ReadRequired {
		t.Fatalf("observation without target = %#v, %v; want still due", observation, err)
	}

	if err := svc.store.SetGlobalObserverTarget(ctx, 7, 0, true); err != nil {
		t.Fatal(err)
	}
	if err := svc.pollNotifierThreads(ctx); err != nil {
		t.Fatal(err)
	}
	if count := countDeliveryKind(t, svc, "notifier_terminal"); count != 1 {
		t.Fatalf("delivery after target = %d, want 1", count)
	}
}

func TestNotifierTerminalAfterActivationNotifiesOnFirstBoundedSweepRead(t *testing.T) {
	svc := newNotifierService(t)
	ctx := context.Background()
	if err := svc.store.SetGlobalObserverTarget(ctx, 7, 0, true); err != nil {
		t.Fatal(err)
	}
	activation, err := svc.store.EnsureNotifierActivation(ctx, svc.now())
	if err != nil {
		t.Fatal(err)
	}
	fake := &notifierObserverSession{
		pages: map[string]map[string]any{
			"":         threadListPayloadWithCursor("cursor-2", notifierThreadListItem("head-active", "Head", "/work/head", activation, "running", "turn-head", "cli")),
			"cursor-2": threadListPayload(notifierThreadListItem("sweep-terminal", "Sweep terminal", "/work/sweep", activation+1, "completed", "", "vscode")),
		},
		threadReads: map[string]map[string]any{
			"head-active":    notifierThreadReadPayload("head-active", "Head", "/work/head", "turn-head", "running", "", activation),
			"sweep-terminal": notifierThreadReadPayload("sweep-terminal", "Sweep terminal", "/work/sweep", "turn-sweep", "completed", "sweep result", activation+1),
		},
	}
	useNotifierPollSession(svc, fake)

	if err := svc.discoverNotifierThreads(ctx); err != nil {
		t.Fatal(err)
	}
	if err := svc.pollNotifierThreads(ctx); err != nil {
		t.Fatal(err)
	}

	if count := countDeliveryKind(t, svc, "notifier_terminal"); count != 1 {
		t.Fatalf("delivery count = %d, want first sweep terminal notification", count)
	}
}

func TestNotifierDiscoveryExcludesInternalAndNonInteractiveSources(t *testing.T) {
	svc := newNotifierService(t)
	fake := &notifierObserverSession{pages: map[string]map[string]any{
		"": threadListPayload(
			notifierThreadListItem("cli-visible", "CLI", "/work/cli", 100, "running", "turn", "cli"),
			notifierThreadListItem("vscode-visible", "VSCode", "/work/vscode", 101, "running", "turn", "vscode"),
			notifierThreadListItem("web-hidden", "Web", "/work/web", 102, "running", "turn", "web"),
			archivedNotifierThreadListItem("archived-hidden", "Archived", "/work/archived", 103, "running", "turn", "cli"),
			internalThreadListItem("internal-hidden", "/work/internal", 104),
		),
	}}
	useNotifierPollSession(svc, fake)

	if err := svc.discoverNotifierThreads(context.Background()); err != nil {
		t.Fatal(err)
	}

	assertNotifierDueIDs(t, svc, []string{"vscode-visible", "cli-visible"})
}

func TestNotifierInterleavedCompletionsKeepMostRecentOrder(t *testing.T) {
	svc := newNotifierService(t)
	ctx := context.Background()
	if err := svc.store.SetGlobalObserverTarget(ctx, 7, 0, true); err != nil {
		t.Fatal(err)
	}
	fake := &notifierObserverSession{
		pages: map[string]map[string]any{
			"": threadListPayload(
				notifierThreadListItem("ordered-a", "Ordered A", "/work/a", notifierTestNow.Unix()+1, "running", "turn-a", "cli"),
				notifierThreadListItem("ordered-b", "Ordered B", "/work/b", notifierTestNow.Unix()+2, "running", "turn-b", "vscode"),
			),
		},
		threadReads: map[string]map[string]any{
			"ordered-a": notifierThreadReadPayload("ordered-a", "Ordered A", "/work/a", "turn-a", "completed", "final a", notifierTestNow.Unix()+2),
			"ordered-b": notifierThreadReadPayload("ordered-b", "Ordered B", "/work/b", "turn-b", "completed", "final b", notifierTestNow.Unix()+1),
		},
	}
	useNotifierPollSession(svc, fake)

	if err := svc.discoverNotifierThreads(ctx); err != nil {
		t.Fatal(err)
	}
	if err := svc.pollNotifierThreads(ctx); err != nil {
		t.Fatal(err)
	}

	items, err := svc.store.ClaimDeliveryBatch(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	got := make([]string, 0, len(items))
	for _, item := range items {
		got = append(got, item.EventID)
	}
	want := []string{"ordered-b:turn-b:completed", "ordered-a:turn-a:completed"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("delivery order = %v, want %v", got, want)
	}
}

func TestNotifierBootstrapProcessesOnlyBoundedPages(t *testing.T) {
	svc := newNotifierService(t)
	fake := &notifierObserverSession{pages: map[string]map[string]any{
		"":         threadListPayloadWithCursor("cursor-2", notifierThreadListItem("head", "Head", "/work/head", 100, "running", "turn", "cli")),
		"cursor-2": threadListPayloadWithCursor("cursor-3", notifierThreadListItem("sweep", "Sweep", "/work/sweep", 99, "running", "turn", "cli")),
		"cursor-3": threadListPayload(notifierThreadListItem("third-page", "Third", "/work/third", 98, "running", "turn", "cli")),
	}}
	useNotifierPollSession(svc, fake)

	if err := svc.discoverNotifierThreads(context.Background()); err != nil {
		t.Fatal(err)
	}

	if cursors := notifierListCursors(fake.listOptionsCalls); !reflect.DeepEqual(cursors, []string{"", "cursor-2"}) {
		t.Fatalf("list cursors = %v, want only head plus one sweep page", cursors)
	}
	assertNotifierDueIDs(t, svc, []string{"head", "sweep"})
	cursor, err := svc.store.GetState(context.Background(), notifierSweepCursorKey)
	if err != nil || cursor != "cursor-3" {
		t.Fatalf("sweep cursor = %q, %v; want cursor-3", cursor, err)
	}
}

func TestNotifierServiceLoopRefreshUsesNotifierDiscoveryWithoutBackgroundTarget(t *testing.T) {
	svc := newNotifierService(t)
	svc.cfg.Profile = "notifier"
	fake := &notifierObserverSession{
		pages: map[string]map[string]any{
			"": threadListPayload(notifierThreadListItem("loop-discovered", "Loop", "/work/loop", 100, "running", "turn-loop", "cli")),
		},
	}
	useNotifierPollSession(svc, fake)

	svc.refreshObserverIndex(context.Background())

	if len(fake.listOptionsCalls) != 1 {
		t.Fatalf("ThreadListWithOptions calls = %#v, want notifier discovery call", fake.listOptionsCalls)
	}
	assertNotifierListOptions(t, fake.listOptionsCalls[0], "")
	if fake.threadListCalls != 0 {
		t.Fatalf("legacy ThreadList calls = %d, want 0", fake.threadListCalls)
	}
	assertNotifierDueIDs(t, svc, []string{"loop-discovered"})
}

func TestNotifierServiceLoopPollUsesNotifierObservationQueue(t *testing.T) {
	svc := newNotifierService(t)
	svc.cfg.Profile = "notifier"
	ctx := context.Background()
	if err := svc.store.SetGlobalObserverTarget(ctx, 7, 0, true); err != nil {
		t.Fatal(err)
	}
	fake := &notifierObserverSession{
		threadReads: map[string]map[string]any{
			"loop-terminal": notifierThreadReadPayload("loop-terminal", "Loop terminal", "/work/loop", "turn-loop", "completed", "loop result", notifierTestNow.Unix()+1),
		},
	}
	useNotifierPollSession(svc, fake)
	if err := svc.store.ObserveNotifierThread(ctx, "loop-terminal", notifierTestNow.Unix()+1, svc.now()); err != nil {
		t.Fatal(err)
	}

	svc.pollTracked(ctx)

	if len(fake.threadReadCalls) != 1 || fake.threadReadCalls[0].threadID != "loop-terminal" || !fake.threadReadCalls[0].includeTurns {
		t.Fatalf("ThreadRead calls = %#v, want notifier include-turns read", fake.threadReadCalls)
	}
	if fake.threadListCalls != 0 || fake.resumeCalls != 0 || fake.startCalls != 0 || fake.forkCalls != 0 || fake.turnStartCalls != 0 || fake.responseCalls != 0 {
		t.Fatalf("legacy/mutation calls: %#v", fake)
	}
	if count := countDeliveryKind(t, svc, "notifier_terminal"); count != 1 {
		t.Fatalf("notifier_terminal deliveries = %d, want 1", count)
	}
}

func TestNotifierBootstrapTrackedStateUsesNotifierOnly(t *testing.T) {
	svc := newNotifierService(t)
	svc.cfg.Profile = "notifier"
	ctx := context.Background()
	if err := svc.store.SetGlobalObserverTarget(ctx, 7, 0, true); err != nil {
		t.Fatal(err)
	}
	fake := &notifierObserverSession{
		pages: map[string]map[string]any{
			"": threadListPayload(notifierThreadListItem("bootstrap-terminal", "Bootstrap", "/work/bootstrap", notifierTestNow.Unix()+1, "completed", "", "cli")),
		},
		threadReads: map[string]map[string]any{
			"bootstrap-terminal": notifierThreadReadPayload("bootstrap-terminal", "Bootstrap", "/work/bootstrap", "turn-bootstrap", "completed", "bootstrap result", notifierTestNow.Unix()+1),
		},
	}
	useNotifierLiveAndPollSession(svc, fake)

	svc.bootstrapTrackedState(ctx)

	if len(fake.listOptionsCalls) != 1 {
		t.Fatalf("ThreadListWithOptions calls = %#v, want notifier discovery only", fake.listOptionsCalls)
	}
	if fake.threadListCalls != 0 || fake.resumeCalls != 0 || fake.startCalls != 0 || fake.forkCalls != 0 || fake.turnStartCalls != 0 || fake.responseCalls != 0 {
		t.Fatalf("legacy/mutation calls: %#v", fake)
	}
	if count := countDeliveryKind(t, svc, "notifier_terminal"); count != 1 {
		t.Fatalf("notifier_terminal deliveries = %d, want 1", count)
	}
}

func TestNotifierIndexAndAttachLoopsSkipLegacyPathsWhenContextAlreadyCanceled(t *testing.T) {
	svc := newNotifierService(t)
	svc.cfg.Profile = "notifier"
	svc.cfg.IndexRefreshInterval = time.Hour
	svc.cfg.AttachRefreshInterval = time.Hour
	ctx := context.Background()
	if err := svc.store.UpsertThread(ctx, model.Thread{ID: "bound-thread", CWD: "/work/bound", Status: "running"}); err != nil {
		t.Fatal(err)
	}
	if err := svc.store.SetBinding(ctx, 7, 0, "bound-thread", model.BindingModeBound); err != nil {
		t.Fatal(err)
	}
	fake := &notifierObserverSession{}
	useNotifierLiveAndPollSession(svc, fake)
	canceled, cancel := context.WithCancel(ctx)
	cancel()

	svc.indexLoop(canceled)
	svc.attachLoop(canceled)

	if fake.threadListCalls != 0 || fake.resumeCalls != 0 || fake.startCalls != 0 || fake.forkCalls != 0 || fake.turnStartCalls != 0 || fake.responseCalls != 0 {
		t.Fatalf("legacy/mutation calls: %#v", fake)
	}
}

func newNotifierService(t *testing.T) *Service {
	t.Helper()
	return newNotifierServiceAtPath(t, filepath.Join(t.TempDir(), "state.sqlite"), securestore.NewDeterministicTestProtector())
}

func newNotifierServiceAtPath(t *testing.T, path string, protector securestore.Protector) *Service {
	t.Helper()
	store, err := storage.OpenWithProtector(path, protector)
	if err != nil {
		t.Fatal(err)
	}
	svc := &Service{
		cfg:            config.Config{Profile: "minimal", ObserverPollInterval: time.Second, DeliveryRetryBase: time.Millisecond, DeliveryMaxAttempts: 3},
		store:          store,
		logger:         discardDiagnosticLogger(),
		diagnosticBy:   map[string]int{},
		diagnosticLast: map[string]time.Time{},
		now:            func() time.Time { return notifierTestNow },
	}
	t.Cleanup(func() { _ = svc.store.Close() })
	return svc
}

type notifierObserverSession struct {
	stubSession
	mu                sync.Mutex
	pages             map[string]map[string]any
	threadReads       map[string]map[string]any
	listOptionsCalls  []control.ThreadListOptions
	threadReadCalls   []minimalThreadReadCall
	threadReadErr     error
	threadReadErrs    map[string]error
	threadListErr     error
	threadReadStarted chan struct{}
	threadReadRelease <-chan struct{}
	resumeCalls       int
	startCalls        int
	forkCalls         int
	turnStartCalls    int
	responseCalls     int
}

func (s *notifierObserverSession) ThreadListWithOptions(ctx context.Context, options control.ThreadListOptions) (map[string]any, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.listOptionsCalls = append(s.listOptionsCalls, options)
	if s.threadListErr != nil {
		return nil, s.threadListErr
	}
	payload, ok := s.pages[strings.TrimSpace(options.Cursor)]
	if !ok {
		return threadListPayload(), nil
	}
	return filterNotifierListPayload(payload, options), nil
}

func (s *notifierObserverSession) ThreadRead(ctx context.Context, threadID string, includeTurns bool) (map[string]any, error) {
	s.mu.Lock()
	s.threadReadCalls = append(s.threadReadCalls, minimalThreadReadCall{threadID: threadID, includeTurns: includeTurns})
	started := s.threadReadStarted
	release := s.threadReadRelease
	if started != nil {
		s.threadReadStarted = nil
	}
	if err := s.threadReadErrs[threadID]; err != nil {
		s.mu.Unlock()
		return nil, err
	}
	if s.threadReadErr != nil {
		s.mu.Unlock()
		return nil, s.threadReadErr
	}
	payload, ok := s.threadReads[threadID]
	s.mu.Unlock()
	if started != nil {
		close(started)
	}
	if release != nil {
		select {
		case <-release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if ok {
		return payload, nil
	}
	return nil, errors.New("missing test thread read for " + threadID)
}

func (s *notifierObserverSession) ThreadResume(ctx context.Context, threadID, cwd string) (map[string]any, error) {
	s.resumeCalls++
	return nil, nil
}

func (s *notifierObserverSession) ThreadStart(ctx context.Context, cwd string) (map[string]any, error) {
	s.startCalls++
	return nil, nil
}

func (s *notifierObserverSession) TurnStart(ctx context.Context, threadID, message, cwd string, options control.TurnStartOptions) (map[string]any, error) {
	s.turnStartCalls++
	return nil, nil
}

func (s *notifierObserverSession) RespondServerRequest(ctx context.Context, requestID string, result map[string]any) error {
	s.responseCalls++
	return nil
}

func (s *notifierObserverSession) ThreadFork(ctx context.Context, threadID string, options control.ThreadForkOptions) (map[string]any, error) {
	s.forkCalls++
	return nil, nil
}

func useNotifierPollSession(svc *Service, session *notifierObserverSession) {
	svc.mu.Lock()
	defer svc.mu.Unlock()
	svc.poll = session
	svc.pollConnected = true
	svc.liveConnected = false
}

func useNotifierLiveAndPollSession(svc *Service, session *notifierObserverSession) {
	svc.mu.Lock()
	defer svc.mu.Unlock()
	svc.live = session
	svc.poll = session
	svc.liveConnected = true
	svc.pollConnected = true
}

func notifierThreadListItem(id, title, cwd string, updatedAt int64, status, activeTurnID, sourceKind string) map[string]any {
	item := threadListItem(id, title, cwd, updatedAt, status, activeTurnID)
	item["sourceKind"] = sourceKind
	return item
}

func archivedNotifierThreadListItem(id, title, cwd string, updatedAt int64, status, activeTurnID, sourceKind string) map[string]any {
	item := notifierThreadListItem(id, title, cwd, updatedAt, status, activeTurnID, sourceKind)
	item["archived"] = true
	return item
}

func notifierThreadReadPayload(id, title, cwd, turnID, status, summary string, updatedAt int64) map[string]any {
	payload := threadReadPayload(id, title, cwd, turnID, status, summary)
	payload["thread"].(map[string]any)["updatedAt"] = updatedAt
	return payload
}

func notifierThreadReadPayloadWithoutFinal(id, title, cwd, turnID, status string, updatedAt int64) map[string]any {
	return notifierThreadReadPayloadWithTurns(id, title, cwd, status, updatedAt, notifierTurnPayload(turnID, status, "", false))
}

func notifierThreadReadPayloadWithTurns(id, title, cwd, status string, updatedAt int64, turns ...map[string]any) map[string]any {
	rawTurns := make([]any, 0, len(turns))
	for _, turn := range turns {
		rawTurns = append(rawTurns, turn)
	}
	return map[string]any{
		"thread": map[string]any{
			"id":        id,
			"title":     title,
			"cwd":       cwd,
			"status":    status,
			"updatedAt": updatedAt,
			"turns":     rawTurns,
		},
	}
}

func notifierTurnPayload(turnID, status, summary string, includeFinal bool) map[string]any {
	items := []any{}
	if summary != "" {
		items = append(items, map[string]any{"id": "agent-" + turnID, "type": "agentMessage", "text": summary})
	}
	if includeFinal {
		items = append(items, map[string]any{"id": "final-" + turnID, "type": "agentMessage", "phase": "final_answer", "text": summary})
	}
	return map[string]any{
		"id":     turnID,
		"status": status,
		"items":  items,
	}
}

func filterNotifierListPayload(payload map[string]any, options control.ThreadListOptions) map[string]any {
	items, _ := payload["data"].([]any)
	filtered := make([]map[string]any, 0, len(items))
	for _, raw := range items {
		item, _ := raw.(map[string]any)
		if item == nil {
			continue
		}
		if !notifierSourceAllowed(item, options.SourceKinds) {
			continue
		}
		if options.Archived != nil {
			archived, _ := item["archived"].(bool)
			if archived != *options.Archived {
				continue
			}
		}
		filtered = append(filtered, item)
	}
	nextCursor, _ := payload["nextCursor"].(string)
	return threadListPayloadWithCursor(nextCursor, filtered...)
}

func notifierSourceAllowed(item map[string]any, allowed []string) bool {
	sourceKind, _ := item["sourceKind"].(string)
	if sourceKind == "" {
		if source, _ := item["source"].(map[string]any); source != nil {
			sourceKind, _ = source["kind"].(string)
		}
	}
	if sourceKind == "" {
		return true
	}
	for _, kind := range allowed {
		if kind == sourceKind {
			return true
		}
	}
	return false
}

func assertNotifierListOptions(t *testing.T, options control.ThreadListOptions, cursor string) {
	t.Helper()
	if options.Limit != notifierCatalogLimit ||
		options.Cursor != cursor ||
		options.SortKey != "updated_at" ||
		options.SortDirection != "desc" ||
		!reflect.DeepEqual(options.SourceKinds, []string{"cli", "vscode"}) ||
		options.Archived == nil ||
		*options.Archived {
		t.Fatalf("ThreadListWithOptions options = %#v, want notifier cli/vscode archived=false cursor %q", options, cursor)
	}
}

func assertNotifierDueIDs(t *testing.T, svc *Service, want []string) {
	t.Helper()
	due, err := svc.store.ListNotifierObservationsDue(context.Background(), 1000)
	if err != nil {
		t.Fatal(err)
	}
	got := make([]string, 0, len(due))
	for _, observation := range due {
		got = append(got, observation.ThreadID)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("notifier due ids = %v, want %v", got, want)
	}
}

func notifierListCursors(calls []control.ThreadListOptions) []string {
	out := make([]string, 0, len(calls))
	for _, call := range calls {
		out = append(out, call.Cursor)
	}
	return out
}

func notifierThreadReadIDs(calls []minimalThreadReadCall) []string {
	out := make([]string, 0, len(calls))
	for _, call := range calls {
		out = append(out, call.threadID)
	}
	return out
}

func (s *recordingSender) joinedText() string {
	texts := make([]string, 0, len(s.messages))
	for _, message := range s.messages {
		texts = append(texts, message.text)
	}
	return strings.Join(texts, "\n")
}
