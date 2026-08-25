package daemon

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/alclssna33/codex_to_telegram/internal/appserver"
	"github.com/alclssna33/codex_to_telegram/internal/model"
	"github.com/alclssna33/codex_to_telegram/internal/storage"
)

type missingRenderedMessageIDSender struct {
	recordingSender
	calls int
	ids   []int64
}

func (s *missingRenderedMessageIDSender) SendRenderedMessages(context.Context, int64, int64, []model.RenderedMessage, [][]model.ButtonSpec, model.SendOptions) ([]int64, error) {
	s.calls++
	return s.ids, nil
}

func TestMinimalRegisteredDiscoveryOneBoundedPageCursorAndDue(t *testing.T) {
	svc, _ := newMinimalService(t)
	ctx := context.Background()
	project := svc.cfg.Projects[0]
	second := svc.cfg.Projects[1]
	child := filepath.Join(project.CanonicalPath, "child")
	if err := ensureTestDir(child); err != nil {
		t.Fatal(err)
	}
	unregistered := t.TempDir()
	items := []map[string]any{
		threadListItem("active-bridge", "Active bridge", project.CanonicalPath, 300, "inProgress", "turn-active-bridge"),
		threadListItem("historical-terminal", "Historical terminal", project.CanonicalPath, 250, "completed", "turn-old"),
		threadListItem("active-second", "Active second", second.CanonicalPath, 200, "running", "turn-active-second"),
		threadListItem("child-thread", "Child", child, 190, "inProgress", "turn-child"),
		threadListItem("unregistered-thread", "Unregistered", unregistered, 180, "inProgress", "turn-unregistered"),
	}
	for i := 0; i < 101; i++ {
		items = append(items, threadListItem(fmt.Sprintf("filler-%03d", i), "Filler", unregistered, int64(i), "completed", ""))
	}
	fake := &minimalCatalogSession{pages: map[string]map[string]any{
		"":         threadListPayloadWithCursor("cursor-2", items...),
		"cursor-2": threadListPayload(threadListItem("active-next-page", "Next page", project.CanonicalPath, 400, "inProgress", "turn-next")),
	}}
	usePollCatalogSession(svc, fake)

	if err := svc.discoverMinimalRegisteredThreads(ctx); err != nil {
		t.Fatal(err)
	}
	if len(fake.listCalls) != 1 || fake.listCalls[0].limit != 100 || fake.listCalls[0].cursor != "" {
		t.Fatalf("first discovery calls = %#v, want one limit-100 first-page call", fake.listCalls)
	}
	due, err := svc.store.ListMinimalObservedThreadsDue(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := stringSet(threadIDs(due)), map[string]bool{"active-bridge": true, "active-second": true}; !sameStringSet(got, want) {
		t.Fatalf("due observed threads = %v, want %v", got, want)
	}
	for _, id := range []string{"child-thread", "unregistered-thread"} {
		if thread, err := svc.store.GetThread(ctx, id); err != nil || thread != nil {
			t.Fatalf("GetThread(%s) = %#v, %v; want nil", id, thread, err)
		}
	}
	cursor, err := svc.store.GetState(ctx, "minimal.observer.thread_list_cursor")
	if err != nil || cursor != "cursor-2" {
		t.Fatalf("discovery cursor = %q, %v; want cursor-2", cursor, err)
	}

	if err := svc.discoverMinimalRegisteredThreads(ctx); err != nil {
		t.Fatal(err)
	}
	if len(fake.listCalls) != 2 || fake.listCalls[1].limit != 100 || fake.listCalls[1].cursor != "cursor-2" {
		t.Fatalf("second discovery calls = %#v, want cursor continuation", fake.listCalls)
	}
	cursor, err = svc.store.GetState(ctx, "minimal.observer.thread_list_cursor")
	if err != nil || cursor != "" {
		t.Fatalf("exhausted discovery cursor = %q, %v; want reset", cursor, err)
	}
}

func TestMinimalRegisteredDiscoveryRunsOnPollTickNotBootstrap(t *testing.T) {
	svc, _ := newMinimalService(t)
	ctx := context.Background()
	fake := &minimalCatalogSession{pages: map[string]map[string]any{
		"": threadListPayload(threadListItem("active-bridge", "Active", svc.cfg.Projects[0].CanonicalPath, 10, "inProgress", "turn-1")),
	}}
	usePollCatalogSession(svc, fake)

	svc.bootstrapTrackedState(ctx)
	if len(fake.listCalls) != 0 {
		t.Fatalf("bootstrap ThreadList calls = %#v, want none", fake.listCalls)
	}
	if err := svc.store.SetGlobalObserverTarget(ctx, 7, 0, true); err != nil {
		t.Fatal(err)
	}
	svc.refreshObserverIndex(ctx)
	if len(fake.listCalls) != 1 || fake.listCalls[0].limit != 100 {
		t.Fatalf("observer tick ThreadList calls = %#v, want one bounded discovery call", fake.listCalls)
	}
}

func TestMinimalNotLoadedRegisteredThreadIsPolledUntilItsCompletionIsDelivered(t *testing.T) {
	svc, _ := newMinimalService(t)
	ctx := context.Background()
	project := svc.cfg.Projects[0]
	if err := svc.store.SetGlobalObserverTarget(ctx, 7, 0, true); err != nil {
		t.Fatal(err)
	}
	fake := &minimalCatalogSession{
		pages: map[string]map[string]any{
			"": threadListPayload(threadListItem("desktop-thread", "Desktop thread", project.CanonicalPath, 100, "notLoaded", "")),
		},
	}
	fake.threadReads = map[string]map[string]any{
		"desktop-thread": threadReadPayload("desktop-thread", "Desktop thread", project.CanonicalPath, "turn-desktop", "inProgress", ""),
	}
	usePollCatalogSession(svc, fake)

	if err := svc.discoverMinimalRegisteredThreads(ctx); err != nil {
		t.Fatal(err)
	}
	if due, err := svc.store.ListMinimalObservedThreadsDue(ctx, 10); err != nil || !sameStringSet(stringSet(threadIDs(due)), map[string]bool{"desktop-thread": true}) {
		t.Fatalf("notLoaded due threads = %#v, %v; want desktop-thread", due, err)
	}

	svc.pollTracked(ctx)
	fake.threadReads["desktop-thread"] = threadReadPayload("desktop-thread", "Desktop thread", project.CanonicalPath, "turn-desktop", "completed", "desktop final")
	svc.pollTracked(ctx)

	if got := strings.Join(drainMinimalDeliveryTexts(t, svc), "\n"); !strings.Contains(got, "desktop final") {
		t.Fatalf("notLoaded terminal delivery = %q, want desktop final", got)
	}
}

func TestMinimalRecentNotLoadedThreadDeliversWhenFirstPollSeesCompletion(t *testing.T) {
	svc, _ := newMinimalService(t)
	ctx := context.Background()
	project := svc.cfg.Projects[0]
	if err := svc.store.SetGlobalObserverTarget(ctx, 7, 0, true); err != nil {
		t.Fatal(err)
	}
	since := svc.globalObserverSinceUnix(ctx)
	fake := &minimalCatalogSession{
		pages: map[string]map[string]any{
			"": threadListPayload(threadListItem("recent-desktop-thread", "Recent desktop thread", project.CanonicalPath, since+1, "notLoaded", "")),
		},
	}
	completed := threadReadPayload("recent-desktop-thread", "Recent desktop thread", project.CanonicalPath, "turn-recent", "completed", "recent desktop final")
	completed["thread"].(map[string]any)["updatedAt"] = since + 1
	fake.threadReads = map[string]map[string]any{"recent-desktop-thread": completed}
	usePollCatalogSession(svc, fake)

	if err := svc.discoverMinimalRegisteredThreads(ctx); err != nil {
		t.Fatal(err)
	}
	svc.pollTracked(ctx)

	if got := strings.Join(drainMinimalDeliveryTexts(t, svc), "\n"); !strings.Contains(got, "recent desktop final") {
		t.Fatalf("first recent notLoaded terminal delivery = %q, want recent desktop final", got)
	}
}

func TestMinimalNotLoadedDiscoveryRearmsAnExistingLegacyObservation(t *testing.T) {
	svc, _ := newMinimalService(t)
	ctx := context.Background()
	project := svc.cfg.Projects[0]
	thread := model.Thread{ID: "legacy-desktop-thread", Title: "Legacy desktop thread", CWD: project.CanonicalPath, ProjectName: project.DisplayName, Status: "notLoaded", UpdatedAt: 100}
	if err := svc.store.UpsertThread(ctx, thread); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", svc.store.Path())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`
		INSERT INTO minimal_thread_observations(thread_id, project_id, last_updated_at, last_turn_id, last_turn_status, baseline_ready, read_required, retired, discovery_seq, updated_at)
		VALUES (?, ?, ?, NULL, 'notLoaded', 1, 0, 0, 1, ?)`, thread.ID, project.ID, thread.UpdatedAt, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	_ = db.Close()

	if err := svc.store.ObserveMinimalThread(ctx, thread, project.ID, svc.now()); err != nil {
		t.Fatal(err)
	}
	if due, err := svc.store.ListMinimalObservedThreadsDue(ctx, 10); err != nil || !sameStringSet(stringSet(threadIDs(due)), map[string]bool{thread.ID: true}) {
		t.Fatalf("legacy notLoaded due threads = %#v, %v; want %s", due, err, thread.ID)
	}
}

func TestMinimalNotLoadedThreadDeliversASecondDesktopRunWithoutListActivity(t *testing.T) {
	svc, _ := newMinimalService(t)
	ctx := context.Background()
	project := svc.cfg.Projects[0]
	if err := svc.store.SetGlobalObserverTarget(ctx, 7, 0, true); err != nil {
		t.Fatal(err)
	}
	fake := &minimalCatalogSession{
		pages: map[string]map[string]any{
			"": threadListPayload(threadListItem("desktop-thread", "Desktop thread", project.CanonicalPath, 100, "notLoaded", "")),
		},
	}
	fake.threadReads = map[string]map[string]any{
		"desktop-thread": threadReadPayload("desktop-thread", "Desktop thread", project.CanonicalPath, "turn-one", "inProgress", ""),
	}
	usePollCatalogSession(svc, fake)
	if err := svc.discoverMinimalRegisteredThreads(ctx); err != nil {
		t.Fatal(err)
	}

	svc.pollTracked(ctx)
	fake.threadReads["desktop-thread"] = threadReadPayload("desktop-thread", "Desktop thread", project.CanonicalPath, "turn-one", "completed", "first final")
	svc.pollTracked(ctx)
	_ = drainMinimalDeliveryTexts(t, svc)

	fake.threadReads["desktop-thread"] = threadReadPayload("desktop-thread", "Desktop thread", project.CanonicalPath, "turn-two", "completed", "second final")
	svc.pollTracked(ctx)
	if got := strings.Join(drainMinimalDeliveryTexts(t, svc), "\n"); !strings.Contains(got, "second final") {
		t.Fatalf("second notLoaded terminal delivery = %q, want second final", got)
	}
}

func TestMinimalObserverBaselinesHistoricalTerminalThenDeliversNewTerminalOnce(t *testing.T) {
	svc, _ := newMinimalService(t)
	ctx := context.Background()
	if err := svc.store.SetGlobalObserverTarget(ctx, 7, 0, true); err != nil {
		t.Fatal(err)
	}
	discoverTerminal(t, svc, "historical-thread", "old-turn", "old final")
	if got := strings.Join(drainMinimalDeliveryTexts(t, svc), "\n"); got != "" {
		t.Fatalf("historical terminal delivery = %q, want none", got)
	}
	discoverActive(t, svc, "active-thread", "turn-1")
	completeMinimalThread(t, svc, "active-thread", "turn-1", "new final")
	if got := strings.Join(drainMinimalDeliveryTexts(t, svc), "\n"); !strings.Contains(got, "new final") {
		t.Fatalf("new terminal delivery = %q, want new final", got)
	}
	completeMinimalThread(t, svc, "active-thread", "turn-1", "new final")
	if got := strings.Join(drainMinimalDeliveryTexts(t, svc), "\n"); got != "" {
		t.Fatalf("duplicate terminal delivery = %q, want none", got)
	}
}

func TestMinimalObserverDeliversTerminalNotificationsInDiscoveryOrder(t *testing.T) {
	svc, _ := newMinimalService(t)
	ctx := context.Background()
	project := svc.cfg.Projects[0]
	if err := svc.store.SetGlobalObserverTarget(ctx, 7, 0, true); err != nil {
		t.Fatal(err)
	}
	fake := &minimalCatalogSession{
		pages: map[string]map[string]any{
			"":         threadListPayloadWithCursor("cursor-b", threadListItem("ordered-a", "Ordered A", project.CanonicalPath, 100, "inProgress", "turn-a")),
			"cursor-b": threadListPayload(threadListItem("ordered-b", "Ordered B", project.CanonicalPath, 200, "inProgress", "turn-b")),
		},
	}
	usePollCatalogSession(svc, fake)
	if err := svc.discoverMinimalRegisteredThreads(ctx); err != nil {
		t.Fatal(err)
	}
	if err := svc.discoverMinimalRegisteredThreads(ctx); err != nil {
		t.Fatal(err)
	}

	currentUpdatedAt := time.Now().UTC().Unix()
	for _, thread := range []model.Thread{
		{ID: "ordered-a", Title: "Ordered A", CWD: project.CanonicalPath, ProjectName: project.DisplayName, Status: "completed", UpdatedAt: currentUpdatedAt},
		{ID: "ordered-b", Title: "Ordered B", CWD: project.CanonicalPath, ProjectName: project.DisplayName, Status: "completed", UpdatedAt: currentUpdatedAt + 1},
	} {
		if err := svc.persistThread(ctx, thread); err != nil {
			t.Fatal(err)
		}
		if err := svc.store.ObserveMinimalThread(ctx, thread, project.ID, svc.now()); err != nil {
			t.Fatal(err)
		}
	}
	fake.threadReads = map[string]map[string]any{
		"ordered-a": threadReadPayload("ordered-a", "Ordered A", project.CanonicalPath, "turn-a", "completed", "final a"),
		"ordered-b": threadReadPayload("ordered-b", "Ordered B", project.CanonicalPath, "turn-b", "completed", "final b"),
	}

	svc.pollTracked(ctx)
	items, err := svc.store.ClaimDeliveryBatch(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	got := make([]string, 0, len(items))
	for _, item := range items {
		got = append(got, item.GroupID)
	}
	want := []string{"ordered-a:turn-a:completed", "ordered-b:turn-b:completed"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("terminal notification groups = %v, want discovery order %v", got, want)
	}
}

func TestMinimalObserverRetiresIneligibleDueThreadSoLaterEligibleThreadIsReached(t *testing.T) {
	svc, _ := newMinimalService(t)
	ctx := context.Background()
	project := svc.cfg.Projects[0]
	child := filepath.Join(project.CanonicalPath, "moved-child")
	if err := ensureTestDir(child); err != nil {
		t.Fatal(err)
	}
	if err := svc.store.SetGlobalObserverTarget(ctx, 7, 0, true); err != nil {
		t.Fatal(err)
	}

	discoveredAt := time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC).Unix()
	items := make([]map[string]any, 0, minimalObservedDueLimit+1)
	items = append(items, threadListItem("retire-a", "Retire A", project.CanonicalPath, discoveredAt, "inProgress", "turn-a"))
	for i := 0; i < minimalObservedDueLimit-1; i++ {
		items = append(items, threadListItem(fmt.Sprintf("filler-%03d", i), "Filler", project.CanonicalPath, discoveredAt-int64(i+1), "inProgress", fmt.Sprintf("turn-filler-%03d", i)))
	}
	items = append(items, threadListItem("eligible-b", "Eligible B", project.CanonicalPath, discoveredAt-1000, "inProgress", "turn-b"))
	fake := &countingMinimalCatalogSession{minimalCatalogSession: &minimalCatalogSession{
		pages: map[string]map[string]any{
			"": threadListPayload(items...),
		},
	}}
	useCountingPollCatalogSession(svc, fake)
	if err := svc.discoverMinimalRegisteredThreads(ctx); err != nil {
		t.Fatal(err)
	}
	if err := svc.store.UpsertThread(ctx, model.Thread{
		ID:          "eligible-b",
		Title:       "eligible-b",
		CWD:         project.CanonicalPath,
		ProjectName: project.DisplayName,
		Status:      "completed",
		UpdatedAt:   1,
	}); err != nil {
		t.Fatal(err)
	}

	fake.threadReads = map[string]map[string]any{
		"retire-a":   threadReadPayload("retire-a", "Retire A", child, "turn-a", "completed", "final a must not notify"),
		"eligible-b": threadReadPayload("eligible-b", "Eligible B", project.CanonicalPath, "turn-b", "completed", "final b"),
	}
	for i := 0; i < minimalObservedDueLimit-1; i++ {
		id := fmt.Sprintf("filler-%03d", i)
		fake.threadReads[id] = threadReadPayload(id, "Filler", project.CanonicalPath, fmt.Sprintf("turn-filler-%03d", i), "inProgress", "")
	}

	svc.pollTracked(ctx)
	svc.pollTracked(ctx)

	if got := fake.readCalls["retire-a"]; got != 1 {
		t.Fatalf("retire-a ThreadRead calls = %d, want exactly one after retirement", got)
	}
	storedA, err := svc.store.GetThread(ctx, "retire-a")
	if err != nil || storedA == nil {
		t.Fatalf("stored retire-a = %#v, %v", storedA, err)
	}
	if storedA.CWD != project.CanonicalPath {
		t.Fatalf("stored retire-a cwd = %q, want original exact project cwd", storedA.CWD)
	}
	due, err := svc.store.ListMinimalObservedThreadsDue(ctx, minimalObservedDueLimit+1)
	if err != nil {
		t.Fatal(err)
	}
	for _, thread := range due {
		if thread.ID == "retire-a" {
			t.Fatalf("retire-a remains due after authoritative child cwd read; due=%v", threadIDs(due))
		}
	}
	itemsToDeliver, err := svc.store.ClaimDeliveryBatch(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	gotGroups := make([]string, 0, len(itemsToDeliver))
	for _, item := range itemsToDeliver {
		gotGroups = append(gotGroups, item.GroupID)
	}
	if want := []string{"eligible-b:turn-b:completed"}; !reflect.DeepEqual(gotGroups, want) {
		t.Fatalf("terminal notification groups = %v, want %v", gotGroups, want)
	}
}

func TestMinimalObserverRetirementSurvivesStaleExactListRevival(t *testing.T) {
	svc, _ := newMinimalService(t)
	ctx := context.Background()
	project := svc.cfg.Projects[0]
	child := filepath.Join(project.CanonicalPath, "stale-child")
	if err := ensureTestDir(child); err != nil {
		t.Fatal(err)
	}
	if err := svc.store.SetGlobalObserverTarget(ctx, 7, 0, true); err != nil {
		t.Fatal(err)
	}

	discoveredAt := time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC).Unix()
	threadID := "stale-revival-a"
	fake := &countingMinimalCatalogSession{minimalCatalogSession: &minimalCatalogSession{
		pages: map[string]map[string]any{
			"": threadListPayload(threadListItem(threadID, "Stale Revival A", project.CanonicalPath, discoveredAt, "inProgress", "turn-a")),
		},
	}}
	useCountingPollCatalogSession(svc, fake)
	if err := svc.discoverMinimalRegisteredThreads(ctx); err != nil {
		t.Fatal(err)
	}
	fake.threadReads = map[string]map[string]any{
		threadID: threadReadPayload(threadID, "Stale Revival A", child, "turn-a", "completed", "final a must stay retired"),
	}

	svc.pollTracked(ctx)
	if got := fake.readCalls[threadID]; got != 1 {
		t.Fatalf("initial ThreadRead calls = %d, want one retirement read", got)
	}
	if items, err := svc.store.ClaimDeliveryBatch(ctx, 10); err != nil || len(items) != 0 {
		t.Fatalf("delivery after ineligible retirement = %#v, %v; want none", items, err)
	}

	fake.pages[""] = threadListPayload(threadListItem(threadID, "Stale Revival A", project.CanonicalPath, discoveredAt+100, "completed", ""))
	if err := svc.discoverMinimalRegisteredThreads(ctx); err != nil {
		t.Fatal(err)
	}
	observed, readRequired, err := svc.store.MinimalObservationReadRequired(ctx, threadID)
	if err != nil {
		t.Fatal(err)
	}
	if !observed || readRequired {
		t.Fatalf("observation after stale exact list = observed:%t readRequired:%t, want retired non-due", observed, readRequired)
	}

	svc.pollTracked(ctx)
	if got := fake.readCalls[threadID]; got != 1 {
		t.Fatalf("ThreadRead calls after stale exact list = %d, want no re-poll after retirement", got)
	}
	if items, err := svc.store.ClaimDeliveryBatch(ctx, 10); err != nil || len(items) != 0 {
		t.Fatalf("delivery after stale exact list = %#v, %v; want none", items, err)
	}
}

type countingMinimalCatalogSession struct {
	*minimalCatalogSession
	readCalls map[string]int
}

func (s *countingMinimalCatalogSession) ThreadRead(ctx context.Context, threadID string, includeTurns bool) (map[string]any, error) {
	if s.readCalls == nil {
		s.readCalls = map[string]int{}
	}
	s.readCalls[threadID]++
	return s.minimalCatalogSession.stubSession.ThreadRead(ctx, threadID, includeTurns)
}

func useCountingPollCatalogSession(svc *Service, session *countingMinimalCatalogSession) {
	svc.mu.Lock()
	defer svc.mu.Unlock()
	svc.poll = session
	svc.pollConnected = true
	svc.liveConnected = false
}

func TestForeignFinalIsFilteredChunkedAndMapped(t *testing.T) {
	svc, _ := newMinimalService(t)
	sender := &recordingSender{}
	svc.SetSender(sender)
	if err := svc.store.SetGlobalObserverTarget(context.Background(), 7, 0, true); err != nil {
		t.Fatal(err)
	}
	snapshot := appserver.ThreadReadSnapshot{Thread: model.Thread{ID: "thr-123456789", CWD: svc.cfg.Projects[0].CanonicalPath}, LatestTurnID: "turn-1", LatestTurnStatus: "completed", LatestFinalText: strings.Repeat("가", 9000)}
	armMinimalTerminalTransition(t, svc, snapshot.Thread.ID, snapshot.Thread.CWD, snapshot.LatestTurnID)
	if err := svc.projectMinimalTerminal(context.Background(), snapshot); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		svc.processDeliveryBatch(context.Background())
	}
	if len(sender.messages) != 3 {
		t.Fatalf("messages = %d, want 3", len(sender.messages))
	}
	for i, message := range sender.messages {
		wantLabel := "[" + string(rune('1'+i)) + "/3]"
		if !strings.HasPrefix(message.text, wantLabel) {
			t.Fatalf("message %d = %q, want label %q", i, message.text[:min(16, len(message.text))], wantLabel)
		}
		route, err := svc.store.ResolveMessageRoute(context.Background(), 7, 0, message.messageID)
		if err != nil || route == nil || route.ThreadID != "thr-123456789" || route.TurnID != "turn-1" {
			t.Fatalf("route = %#v, %v", route, err)
		}
	}
	if err := svc.projectMinimalTerminal(context.Background(), snapshot); err != nil {
		t.Fatal(err)
	}
	svc.processDeliveryBatch(context.Background())
	if len(sender.messages) != 3 {
		t.Fatal("duplicate terminal delivery")
	}
}

func TestForkedCompletionDeliversFullAnswerOnceWithReplyRoute(t *testing.T) {
	svc, _ := newMinimalService(t)
	sender := &recordingSender{}
	svc.SetSender(sender)
	ctx := context.Background()
	if err := svc.store.SetGlobalObserverTarget(ctx, 7, 0, true); err != nil {
		t.Fatal(err)
	}
	final := strings.Repeat("완료답변", 200)
	snapshot := appserver.ThreadReadSnapshot{
		Thread:           model.Thread{ID: "child-87654321", CWD: svc.cfg.Projects[0].CanonicalPath},
		LatestTurnID:     "child-turn",
		LatestTurnStatus: "completed",
		LatestFinalText:  final,
	}
	armMinimalTerminalTransition(t, svc, snapshot.Thread.ID, snapshot.Thread.CWD, snapshot.LatestTurnID)
	if err := svc.projectMinimalTerminal(ctx, snapshot); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		svc.processDeliveryBatch(ctx)
	}
	if len(sender.messages) != 1 {
		t.Fatalf("messages=%#v", sender.messages)
	}
	joined := ""
	for _, message := range sender.messages {
		joined += message.text
		route, err := svc.store.ResolveMessageRoute(ctx, 7, 0, message.messageID)
		if err != nil || route == nil || route.ThreadID != snapshot.Thread.ID || route.TurnID != snapshot.LatestTurnID {
			t.Fatalf("route=%#v err=%v", route, err)
		}
	}
	if !strings.Contains(joined, final) {
		t.Fatalf("joined response omitted final answer: length=%d", len(joined))
	}
	before := len(sender.messages)
	if err := svc.projectMinimalTerminal(ctx, snapshot); err != nil {
		t.Fatal(err)
	}
	svc.processDeliveryBatch(ctx)
	if got := len(sender.messages); got != before || strings.Count(joined, final) != 1 {
		t.Fatalf("message count=%d final copies=%d", got, strings.Count(joined, final))
	}
}

func TestMinimalTerminalHeaderRetainsConversationTitleMatchingProjectName(t *testing.T) {
	svc, _ := newMinimalService(t)
	ctx := context.Background()
	if err := svc.store.SetGlobalObserverTarget(ctx, 7, 0, true); err != nil {
		t.Fatal(err)
	}
	projectName := svc.cfg.Projects[0].DisplayName
	snapshot := appserver.ThreadReadSnapshot{
		Thread: model.Thread{
			ID:    "duplicate-title-thread",
			Title: projectName,
			CWD:   svc.cfg.Projects[0].CanonicalPath,
		},
		LatestTurnID:     "turn-duplicate-title",
		LatestTurnStatus: "completed",
		LatestFinalText:  "done",
	}
	armMinimalTerminalTransition(t, svc, snapshot.Thread.ID, snapshot.Thread.CWD, snapshot.LatestTurnID)
	if err := svc.projectMinimalTerminal(ctx, snapshot); err != nil {
		t.Fatal(err)
	}
	items, err := svc.store.ClaimDeliveryBatch(ctx, 1)
	if err != nil || len(items) != 1 {
		t.Fatalf("items=%#v err=%v", items, err)
	}
	var payload model.DeliveryPayload
	if err := json.Unmarshal([]byte(items[0].PayloadJSON), &payload); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(payload.Text, "[완료] "+projectName+"\n대화: "+projectName+"\nThread: duplicat") {
		t.Fatalf("terminal header = %q, want duplicate conversation title retained", payload.Text)
	}
}

func usePollCatalogSession(svc *Service, session *minimalCatalogSession) {
	svc.mu.Lock()
	defer svc.mu.Unlock()
	svc.poll = session
	svc.pollConnected = true
	svc.liveConnected = false
}

func armMinimalTerminalTransition(t *testing.T, svc *Service, threadID, cwd, turnID string) {
	t.Helper()
	ctx := context.Background()
	project, ok := svc.projectRegistry.MatchExactCWD(cwd)
	if !ok {
		t.Fatalf("test attempted to arm unregistered cwd %q", cwd)
	}
	thread := model.Thread{ID: threadID, Title: threadID, CWD: project.CanonicalPath, ProjectName: project.DisplayName, Status: "inProgress", ActiveTurnID: turnID, UpdatedAt: 1}
	if err := svc.persistThread(ctx, thread); err != nil {
		t.Fatal(err)
	}
	if err := svc.store.ObserveMinimalThread(ctx, thread, project.ID, svc.now()); err != nil {
		t.Fatal(err)
	}
}

func installTerminalWorkerFactory(svc *Service) *workerSessionFactory {
	factory := newWorkerSessionFactory()
	svc.minimalWorkerFactory = factory.New
	svc.minimalWorkers = newMinimalLinkWorkerManager(factory.New, time.Second, svc.logLifecycle)
	svc.minimalWorkers.SetAcquireHook(svc.startMinimalLinkedWorkerEventLoop)
	return factory
}

func seedRunningLinkedTerminal(t *testing.T, svc *Service, linkedID, turnID string, generation uint64) model.MinimalLinkedThread {
	t.Helper()
	ctx := context.Background()
	project := svc.cfg.Projects[0]
	sourceID := "source-" + linkedID
	sourceTurnID := "source-turn-" + linkedID
	link := model.MinimalLinkedThread{
		ChatID: 7, TopicID: 0, ProjectID: project.ID, SourceThreadID: sourceID, LinkedThreadID: linkedID,
		SourceAnchorTurnID: sourceTurnID, SourceTitle: "Source " + linkedID, DesiredTitle: "Source " + linkedID + " · 텔레그램 연동",
		TitleState: model.MinimalLinkedTitleSet, State: model.MinimalLinkedTelegramRunning, WorkerGeneration: generation,
	}
	provenance := model.MinimalContinuation{
		Key:       model.MinimalContinuationKey{ChatID: link.ChatID, TopicID: link.TopicID, SourceThreadID: sourceID, SourceTurnID: sourceTurnID},
		ProjectID: project.ID,
	}
	child := model.Thread{ID: linkedID, Title: linkedID, CWD: project.CanonicalPath, ProjectName: project.DisplayName, Status: "inProgress", ActiveTurnID: turnID}
	if err := svc.store.ActivateMinimalLinkedThread(ctx, link, provenance, child); err != nil {
		t.Fatal(err)
	}
	if changed, err := svc.store.MarkMinimalLinkedTurnStarted(ctx, linkedID, generation, turnID); err != nil || !changed {
		t.Fatalf("turn start changed=%t err=%v, want true nil", changed, err)
	}
	armMinimalTerminalTransition(t, svc, linkedID, project.CanonicalPath, turnID)
	return link
}

func linkedTerminalSnapshot(svc *Service, linkedID, turnID, final string) appserver.ThreadReadSnapshot {
	ctx := context.Background()
	project := svc.cfg.Projects[0]
	thread := model.Thread{ID: linkedID, Title: linkedID, CWD: project.CanonicalPath, ProjectName: project.DisplayName, Status: "completed", UpdatedAt: svc.now().UTC().Unix()}
	_ = svc.persistThread(ctx, thread)
	return appserver.ThreadReadSnapshot{Thread: thread, LatestTurnID: turnID, LatestTurnStatus: "completed", LatestFinalText: final}
}

func countDeliveryKind(t *testing.T, svc *Service, kind string) int {
	t.Helper()
	db, err := sql.Open("sqlite", svc.store.Path())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var count int
	if err := db.QueryRow(`SELECT count(*) FROM delivery_queue WHERE kind=?`, kind).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

func countDeliveryKindStatus(t *testing.T, svc *Service, kind, status string) int {
	t.Helper()
	db, err := sql.Open("sqlite", svc.store.Path())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var count int
	if err := db.QueryRow(`SELECT count(*) FROM delivery_queue WHERE kind=? AND status=?`, kind, status).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

func countTerminalEvents(t *testing.T, svc *Service, terminalKey string) int {
	t.Helper()
	db, err := sql.Open("sqlite", svc.store.Path())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var count int
	if err := db.QueryRow(`SELECT count(*) FROM terminal_events WHERE terminal_key=?`, terminalKey).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

func assertMinimalObservation(t *testing.T, svc *Service, threadID, wantTurnID, wantStatus string) {
	t.Helper()
	db, err := sql.Open("sqlite", svc.store.Path())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var gotTurnID, gotStatus string
	if err := db.QueryRow(`SELECT coalesce(last_turn_id,''), coalesce(last_turn_status,'') FROM minimal_thread_observations WHERE thread_id=?`, threadID).Scan(&gotTurnID, &gotStatus); err != nil {
		t.Fatal(err)
	}
	if gotTurnID != wantTurnID || gotStatus != wantStatus {
		t.Fatalf("minimal observation for %s = turn:%q status:%q, want turn:%q status:%q", threadID, gotTurnID, gotStatus, wantTurnID, wantStatus)
	}
}

func countPendingCommandsByStatus(t *testing.T, svc *Service, status string) int {
	t.Helper()
	db, err := sql.Open("sqlite", svc.store.Path())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var count int
	if err := db.QueryRow(`SELECT count(*) FROM pending_commands WHERE status=?`, status).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

func seedMinimalApprovalForTurn(t *testing.T, svc *Service, requestID, threadID, turnID string) {
	t.Helper()
	ctx := context.Background()
	sessionIdentity := svc.minimalApprovalSessionIdentity()
	if worker, ok := svc.minimalWorkers.ByThread(threadID); ok {
		sessionIdentity = worker.SessionIdentity
	}
	created, err := svc.store.CreateMinimalApproval(ctx, storage.MinimalApprovalSeed{
		Approval: storage.MinimalApproval{
			RequestID:       requestID,
			ThreadID:        threadID,
			TurnID:          turnID,
			RequestKind:     minimalCommandApprovalKind,
			ProjectName:     svc.cfg.Projects[0].DisplayName,
			SessionIdentity: sessionIdentity,
			Status:          "pending",
		},
		ApproveToken: requestID + "-approve-token",
		DenyToken:    requestID + "-deny-token",
		Delivery: model.DeliveryQueueItem{
			EventID:     requestID + ":delivery",
			ChatKey:     model.ChatKey(7, 0),
			ChatID:      7,
			ThreadID:    threadID,
			Kind:        minimalApprovalQueueKind,
			Status:      model.DeliveryStatusPending,
			PayloadJSON: storage.MustJSON(model.DeliveryPayload{Text: "approval", ThreadID: threadID, TurnID: turnID, EventID: requestID}),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !created {
		t.Fatalf("approval %s was not created", requestID)
	}
}

func discoverTerminal(t *testing.T, svc *Service, threadID, turnID, final string) {
	t.Helper()
	ctx := context.Background()
	thread := model.Thread{ID: threadID, Title: threadID, CWD: svc.cfg.Projects[0].CanonicalPath, ProjectName: svc.cfg.Projects[0].DisplayName, Status: "completed", ActiveTurnID: turnID, UpdatedAt: 10}
	if err := svc.persistThread(ctx, thread); err != nil {
		t.Fatal(err)
	}
	if err := svc.store.ObserveMinimalThread(ctx, thread, svc.cfg.Projects[0].ID, svc.now()); err != nil {
		t.Fatal(err)
	}
	snapshot := appserver.ThreadReadSnapshot{Thread: thread, LatestTurnID: turnID, LatestTurnStatus: "completed", LatestFinalText: final}
	if err := svc.projectMinimalTerminal(ctx, snapshot); err != nil {
		t.Fatal(err)
	}
}

func discoverActive(t *testing.T, svc *Service, threadID, turnID string) {
	t.Helper()
	ctx := context.Background()
	thread := model.Thread{ID: threadID, Title: threadID, CWD: svc.cfg.Projects[0].CanonicalPath, ProjectName: svc.cfg.Projects[0].DisplayName, Status: "inProgress", ActiveTurnID: turnID, UpdatedAt: 20}
	if err := svc.persistThread(ctx, thread); err != nil {
		t.Fatal(err)
	}
	if err := svc.store.ObserveMinimalThread(ctx, thread, svc.cfg.Projects[0].ID, svc.now()); err != nil {
		t.Fatal(err)
	}
}

func completeMinimalThread(t *testing.T, svc *Service, threadID, turnID, final string) {
	t.Helper()
	ctx := context.Background()
	snapshot := appserver.ThreadReadSnapshot{Thread: model.Thread{ID: threadID, CWD: svc.cfg.Projects[0].CanonicalPath}, LatestTurnID: turnID, LatestTurnStatus: "completed", LatestFinalText: final}
	if err := svc.projectMinimalTerminal(ctx, snapshot); err != nil {
		t.Fatal(err)
	}
}

func drainMinimalDeliveryTexts(t *testing.T, svc *Service) []string {
	t.Helper()
	items, err := svc.store.ClaimDeliveryBatch(context.Background(), 100)
	if err != nil {
		t.Fatal(err)
	}
	out := make([]string, 0, len(items))
	for _, item := range items {
		var payload model.DeliveryPayload
		if err := json.Unmarshal([]byte(item.PayloadJSON), &payload); err != nil {
			t.Fatal(err)
		}
		out = append(out, payload.Text)
	}
	return out
}

func stringSet(values []string) map[string]bool {
	out := map[string]bool{}
	for _, value := range values {
		out[value] = true
	}
	return out
}

func sameStringSet(left, right map[string]bool) bool {
	if len(left) != len(right) {
		return false
	}
	for key := range left {
		if !right[key] {
			return false
		}
	}
	return true
}

func TestUnregisteredTerminalIsFilteredAndFailureUsesLastAgentMessage(t *testing.T) {
	svc, _ := newMinimalService(t)
	if err := svc.store.SetGlobalObserverTarget(context.Background(), 7, 0, true); err != nil {
		t.Fatal(err)
	}
	foreign := appserver.ThreadReadSnapshot{Thread: model.Thread{ID: "foreign", CWD: t.TempDir()}, LatestTurnID: "turn-x", LatestTurnStatus: "completed", LatestFinalText: "secret"}
	if err := svc.projectMinimalTerminal(context.Background(), foreign); err != nil {
		t.Fatal(err)
	}
	items, err := svc.store.ClaimDeliveryBatch(context.Background(), 10)
	if err != nil || len(items) != 0 {
		t.Fatalf("foreign queue = %#v, %v", items, err)
	}
	failure := appserver.ThreadReadSnapshot{Thread: model.Thread{ID: "failure-thread", CWD: svc.cfg.Projects[0].CanonicalPath}, LatestTurnID: "turn-f", LatestTurnStatus: "failed", LatestAgentMessages: []string{"useful failure detail"}}
	armMinimalTerminalTransition(t, svc, failure.Thread.ID, failure.Thread.CWD, failure.LatestTurnID)
	if err := svc.projectMinimalTerminal(context.Background(), failure); err != nil {
		t.Fatal(err)
	}
	items, err = svc.store.ClaimDeliveryBatch(context.Background(), 1)
	if err != nil || len(items) != 1 {
		t.Fatalf("failure queue = %#v, %v", items, err)
	}
	if !strings.Contains(items[0].PayloadJSON, "[실패]") || !strings.Contains(items[0].PayloadJSON, "useful failure detail") {
		t.Fatalf("failure payload = %s", items[0].PayloadJSON)
	}
}

func TestInterruptedEmptyFinalUsesFailureHeaderAndFallback(t *testing.T) {
	svc, _ := newMinimalService(t)
	if err := svc.store.SetGlobalObserverTarget(context.Background(), 7, 0, true); err != nil {
		t.Fatal(err)
	}
	snapshot := appserver.ThreadReadSnapshot{Thread: model.Thread{ID: "empty-thread", CWD: svc.cfg.Projects[0].CanonicalPath}, LatestTurnID: "turn-i", LatestTurnStatus: "interrupted"}
	armMinimalTerminalTransition(t, svc, snapshot.Thread.ID, snapshot.Thread.CWD, snapshot.LatestTurnID)
	if err := svc.projectMinimalTerminal(context.Background(), snapshot); err != nil {
		t.Fatal(err)
	}
	items, err := svc.store.ClaimDeliveryBatch(context.Background(), 1)
	if err != nil || len(items) != 1 {
		t.Fatalf("items=%#v err=%v", items, err)
	}
	if !strings.Contains(items[0].PayloadJSON, "[실패]") || !strings.Contains(items[0].PayloadJSON, "Codex가 최종 답변을 남기지 않았습니다.") {
		t.Fatalf("payload=%s", items[0].PayloadJSON)
	}
}

func TestTerminalProjectionFailureBlocksFIFOUntilRetry(t *testing.T) {
	svc, _ := newMinimalService(t)
	ctx := context.Background()
	thread := model.Thread{ID: "projection-block", CWD: svc.cfg.Projects[0].CanonicalPath, Status: "inProgress", ActiveTurnID: "turn-terminal"}
	if err := svc.store.UpsertThread(ctx, thread); err != nil {
		t.Fatal(err)
	}
	armMinimalTerminalTransition(t, svc, thread.ID, thread.CWD, thread.ActiveTurnID)
	if err := svc.store.SetGlobalObserverTarget(ctx, 7, 0, true); err != nil {
		t.Fatal(err)
	}
	if err := svc.store.EnqueuePendingCommand(ctx, model.PendingCommand{ThreadID: thread.ID, ProjectID: "bridge", ChatID: 7, Prompt: "must wait"}); err != nil {
		t.Fatal(err)
	}
	app := newRouterSession()
	useRouterSession(svc, app)
	db, err := sql.Open("sqlite", svc.store.Path())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TRIGGER reject_terminal_projection BEFORE INSERT ON terminal_events BEGIN SELECT RAISE(ABORT, 'injected projection failure'); END`); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()
	snapshot := appserver.ThreadReadSnapshot{Thread: model.Thread{ID: thread.ID, CWD: thread.CWD, Status: "completed"}, LatestTurnID: "turn-terminal", LatestTurnStatus: "completed", LatestFinalText: "final"}
	if err := svc.persistThread(ctx, snapshot.Thread); err != nil {
		t.Fatal(err)
	}
	svc.handleTerminalSnapshot(ctx, snapshot, false)
	if calls := app.turnCalls(); len(calls) != 0 {
		t.Fatalf("turn calls after projection failure = %#v, want blocked", calls)
	}
	db, err = sql.Open("sqlite", svc.store.Path())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`DROP TRIGGER reject_terminal_projection`); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()
	svc.handleTerminalSnapshot(ctx, snapshot, false)
	if calls := app.turnCalls(); len(calls) != 1 || calls[0].message != "must wait" {
		t.Fatalf("turn calls after projection retry = %#v", calls)
	}
}

func TestMinimalTerminalEnqueuesBeforeWorkerRelease(t *testing.T) {
	svc, _ := newMinimalService(t)
	ctx := context.Background()
	factory := installTerminalWorkerFactory(svc)
	worker, err := svc.minimalWorkers.Acquire(ctx, "link:linked-terminal-release", "linked-terminal-release")
	if err != nil {
		t.Fatal(err)
	}
	link := seedRunningLinkedTerminal(t, svc, "linked-terminal-release", "turn-terminal", worker.Generation)
	svc.cfg.DeliveryMaxAttempts = 3
	svc.cfg.DeliveryRetryBase = time.Second
	svc.SetSender(&failingDeliverySender{recordingSender: &recordingSender{}, err: errors.New("telegram outage")})
	if err := svc.store.SetGlobalObserverTarget(ctx, link.ChatID, link.TopicID, true); err != nil {
		t.Fatal(err)
	}

	svc.handleTerminalSnapshot(ctx, linkedTerminalSnapshot(svc, link.LinkedThreadID, "turn-terminal", "done"), false)
	svc.processDeliveryBatch(ctx)

	if !factory.Single(t).Closed() {
		t.Fatal("released worker remained open after terminal enqueue")
	}
	got, err := svc.store.GetMinimalLinkedThreadByLinkedID(ctx, link.LinkedThreadID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != model.MinimalLinkedReady || got.ActiveTurnID != "" || got.WorkerGeneration != 0 {
		t.Fatalf("released link=%#v, want ready with cleared turn/generation", got)
	}
	if backlog, err := svc.store.DeliveryQueueBacklog(ctx); err != nil || backlog == 0 {
		t.Fatalf("delivery backlog=%d err=%v, want durable retryable terminal/handoff", backlog, err)
	}
}

func TestMinimalLinkedTerminalReleaseUsesIndependentCloseAndFinalizationContext(t *testing.T) {
	svc, _ := newMinimalService(t)
	ctx := context.Background()
	factory := installTerminalWorkerFactory(svc)
	worker, err := svc.minimalWorkers.Acquire(ctx, "link:linked-terminal-canceled-loop", "linked-terminal-canceled-loop")
	if err != nil {
		t.Fatal(err)
	}
	link := seedRunningLinkedTerminal(t, svc, "linked-terminal-canceled-loop", "turn-terminal", worker.Generation)
	if err := svc.store.SetGlobalObserverTarget(ctx, link.ChatID, link.TopicID, true); err != nil {
		t.Fatal(err)
	}

	svc.handleTerminalSnapshot(worker.Context, linkedTerminalSnapshot(svc, link.LinkedThreadID, "turn-terminal", "done"), false)

	if !factory.Single(t).Closed() {
		t.Fatal("released worker remained open after terminal completion from worker loop")
	}
	got, err := svc.store.GetMinimalLinkedThreadByLinkedID(ctx, link.LinkedThreadID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != model.MinimalLinkedReady || got.ActiveTurnID != "" || got.WorkerGeneration != 0 {
		t.Fatalf("released link=%#v, want ready with cleared turn/generation", got)
	}
	if count := countDeliveryKindStatus(t, svc, "handoff_ready", model.DeliveryStatusPending); count != 1 {
		t.Fatalf("handoff_ready pending deliveries=%d, want 1", count)
	}
}

func TestMinimalLinkedTerminalConfirmedDeadlineCloseStillFinalizesReady(t *testing.T) {
	svc, _ := newMinimalService(t)
	ctx := context.Background()
	factory := newWorkerSessionFactory()
	svc.minimalWorkerFactory = factory.New
	svc.minimalWorkers = newMinimalLinkWorkerManager(factory.New, 5*time.Millisecond, svc.logLifecycle)
	svc.minimalWorkers.SetAcquireHook(svc.startMinimalLinkedWorkerEventLoop)
	svc.minimalLinkedCloseTimeout = 5 * time.Millisecond
	svc.minimalLinkedFinalizationTimeout = time.Second
	worker, err := svc.minimalWorkers.Acquire(ctx, "link:linked-terminal-deadline-close", "linked-terminal-deadline-close")
	if err != nil {
		t.Fatal(err)
	}
	factory.Single(t).SetConfirmedCloseOnContextDone()
	link := seedRunningLinkedTerminal(t, svc, "linked-terminal-deadline-close", "turn-terminal", worker.Generation)
	if err := svc.store.SetGlobalObserverTarget(ctx, link.ChatID, link.TopicID, true); err != nil {
		t.Fatal(err)
	}

	svc.handleTerminalSnapshot(ctx, linkedTerminalSnapshot(svc, link.LinkedThreadID, "turn-terminal", "done"), false)

	got, err := svc.store.GetMinimalLinkedThreadByLinkedID(ctx, link.LinkedThreadID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != model.MinimalLinkedReady || got.ActiveTurnID != "" || got.WorkerGeneration != 0 {
		t.Fatalf("confirmed deadline close link=%#v, want ready with cleared turn/generation", got)
	}
	if count := countDeliveryKindStatus(t, svc, "handoff_ready", model.DeliveryStatusPending); count != 1 {
		t.Fatalf("handoff_ready pending deliveries=%d, want 1", count)
	}
}

func TestMinimalTerminalReleasesNewThreadWorkerWithoutLinkedRow(t *testing.T) {
	svc, _ := newMinimalService(t)
	ctx := context.Background()
	factory := installTerminalWorkerFactory(svc)
	worker, err := svc.minimalWorkers.Acquire(ctx, "new:"+model.ChatKey(7, 0), "")
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.minimalWorkers.BindThread(worker, "plain-new-terminal"); err != nil {
		t.Fatal(err)
	}
	if err := svc.store.SetGlobalObserverTarget(ctx, 7, 0, true); err != nil {
		t.Fatal(err)
	}
	if err := svc.store.PutCallbackRoute(ctx, model.CallbackRoute{
		Token: "new-worker-callback", Action: "answer_choice", ThreadID: "plain-new-terminal", TurnID: "turn-new",
		RequestID: "req-new-input", SessionIdentity: worker.SessionIdentity, Status: model.CallbackStatusActive,
		PayloadJSON: `{"text":"Yes"}`, CreatedAt: model.NowString(),
	}); err != nil {
		t.Fatal(err)
	}
	armMinimalTerminalTransition(t, svc, "plain-new-terminal", svc.cfg.Projects[0].CanonicalPath, "turn-new")

	svc.handleTerminalSnapshot(ctx, linkedTerminalSnapshot(svc, "plain-new-terminal", "turn-new", "done"), false)

	if !factory.Single(t).Closed() {
		t.Fatal("non-linked new-thread worker remained open after terminal projection")
	}
	if _, ok := svc.minimalWorkers.ByThread("plain-new-terminal"); ok {
		t.Fatal("non-linked new-thread worker remained registered after terminal projection")
	}
	route, err := svc.store.GetCallbackRoute(ctx, "new-worker-callback")
	if err != nil || route == nil || route.Status != model.CallbackStatusExpired {
		t.Fatalf("worker callback route=%#v err=%v, want expired", route, err)
	}
	if count := countDeliveryKindStatus(t, svc, "terminal", model.DeliveryStatusPending); count != 1 {
		t.Fatalf("terminal deliveries=%d, want projected terminal before worker release", count)
	}
	if count := countDeliveryKind(t, svc, "handoff_ready"); count != 0 {
		t.Fatalf("handoff_ready deliveries=%d, want none for non-linked worker release", count)
	}
}

func TestMinimalTerminalReleasesExistingThreadWorkerWithoutLinkedRow(t *testing.T) {
	svc, _ := newMinimalService(t)
	ctx := context.Background()
	factory := installTerminalWorkerFactory(svc)
	worker, err := svc.minimalWorkers.Acquire(ctx, "thread:plain-existing-terminal", "plain-existing-terminal")
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.store.SetGlobalObserverTarget(ctx, 7, 0, true); err != nil {
		t.Fatal(err)
	}
	if err := svc.store.SavePendingApproval(ctx, model.PendingApproval{
		RequestID: "req-existing-input", ThreadID: "plain-existing-terminal", TurnID: "turn-existing",
		PromptKind: "item/tool/requestUserInput", RequestKind: "item/tool/requestUserInput",
		SessionIdentity: worker.SessionIdentity, Status: "pending", PayloadJSON: `{"ok":true}`, UpdatedAt: model.NowString(),
	}); err != nil {
		t.Fatal(err)
	}
	armMinimalTerminalTransition(t, svc, "plain-existing-terminal", svc.cfg.Projects[0].CanonicalPath, "turn-existing")

	svc.handleTerminalSnapshot(ctx, linkedTerminalSnapshot(svc, "plain-existing-terminal", "turn-existing", "done"), false)

	if !factory.Single(t).Closed() {
		t.Fatal("non-linked existing-thread worker remained open after terminal projection")
	}
	if _, ok := svc.minimalWorkers.ByThread("plain-existing-terminal"); ok {
		t.Fatal("non-linked existing-thread worker remained registered after terminal projection")
	}
	approval, err := svc.store.GetPendingApproval(ctx, "req-existing-input")
	if err != nil || approval == nil || approval.Status != "expired" {
		t.Fatalf("pending input approval=%#v err=%v, want expired", approval, err)
	}
	if count := countDeliveryKindStatus(t, svc, "terminal", model.DeliveryStatusPending); count != 1 {
		t.Fatalf("terminal deliveries=%d, want projected terminal before worker release", count)
	}
}

func TestMinimalReleaseKeepsOtherWorkerAndGlobalLiveState(t *testing.T) {
	svc, _ := newMinimalService(t)
	ctx := context.Background()
	factory := installTerminalWorkerFactory(svc)
	released, err := svc.minimalWorkers.Acquire(ctx, "link:linked-release-a", "linked-release-a")
	if err != nil {
		t.Fatal(err)
	}
	other, err := svc.minimalWorkers.Acquire(ctx, "link:linked-release-b", "linked-release-b")
	if err != nil {
		t.Fatal(err)
	}
	link := seedRunningLinkedTerminal(t, svc, "linked-release-a", "turn-a", released.Generation)
	seedRunningLinkedTerminal(t, svc, "linked-release-b", "turn-b", other.Generation)
	seedMinimalApprovalForTurn(t, svc, "req-release-a", link.LinkedThreadID, "turn-a")
	liveEvents := make(chan appserver.Event)
	svc.mu.Lock()
	svc.live = &stubSession{}
	svc.liveEvents = liveEvents
	svc.liveConnected = true
	svc.liveGeneration = 42
	svc.mu.Unlock()
	if err := svc.store.SetGlobalObserverTarget(ctx, link.ChatID, link.TopicID, true); err != nil {
		t.Fatal(err)
	}

	svc.handleTerminalSnapshot(ctx, linkedTerminalSnapshot(svc, link.LinkedThreadID, "turn-a", "done"), false)

	sessions := factory.Sessions()
	if len(sessions) != 2 {
		t.Fatalf("worker sessions=%d, want 2", len(sessions))
	}
	if !sessions[0].Closed() {
		t.Fatal("target worker was not closed")
	}
	if sessions[1].Closed() {
		t.Fatal("other worker was closed by target release")
	}
	if got, ok := svc.minimalWorkers.ByThread("linked-release-b"); !ok || got.Generation != other.Generation {
		t.Fatalf("other worker registry entry=%#v ok=%t, want unchanged", got, ok)
	}
	svc.mu.RLock()
	liveConnected, liveGeneration, liveEventsMatch := svc.liveConnected, svc.liveGeneration, svc.liveEvents == liveEvents
	svc.mu.RUnlock()
	if !liveConnected || liveGeneration != 42 || !liveEventsMatch {
		t.Fatalf("global live state changed: connected=%t generation=%d eventsMatch=%t", liveConnected, liveGeneration, liveEventsMatch)
	}
	approval, err := svc.store.GetMinimalApproval(ctx, "req-release-a")
	if err != nil || approval == nil || approval.Status != "expired" {
		t.Fatalf("release approval=%#v err=%v, want expired", approval, err)
	}
	if count := countDeliveryKindStatus(t, svc, minimalApprovalQueueKind, model.DeliveryStatusDead); count != 1 {
		t.Fatalf("dead approval deliveries=%d, want 1", count)
	}
	if count := countDeliveryKindStatus(t, svc, "handoff_ready", model.DeliveryStatusPending); count != 1 {
		t.Fatalf("handoff_ready pending deliveries=%d, want 1", count)
	}
}

func TestMinimalReleaseCloseFailureMarksFailedWithoutReadyNotice(t *testing.T) {
	svc, _ := newMinimalService(t)
	ctx := context.Background()
	factory := installTerminalWorkerFactory(svc)
	worker, err := svc.minimalWorkers.Acquire(ctx, "link:linked-close-failure", "linked-close-failure")
	if err != nil {
		t.Fatal(err)
	}
	session := factory.Single(t)
	session.SetCloseErr(errors.New("close failed"))
	link := seedRunningLinkedTerminal(t, svc, "linked-close-failure", "turn-close", worker.Generation)
	if err := svc.store.SetGlobalObserverTarget(ctx, link.ChatID, link.TopicID, true); err != nil {
		t.Fatal(err)
	}

	svc.handleTerminalSnapshot(ctx, linkedTerminalSnapshot(svc, link.LinkedThreadID, "turn-close", "done"), false)

	got, err := svc.store.GetMinimalLinkedThreadByLinkedID(ctx, link.LinkedThreadID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != model.MinimalLinkedFailed {
		t.Fatalf("link state=%q, want failed after unconfirmed close", got.State)
	}
	if count := countDeliveryKind(t, svc, "handoff_ready"); count != 0 {
		t.Fatalf("handoff_ready deliveries=%d, want none after close failure", count)
	}
	if terminalRows := countDeliveryKind(t, svc, "terminal"); terminalRows == 0 {
		t.Fatal("terminal delivery was not durable before close failure")
	}
}

func TestStaleRunningLinkedTerminalSkipsProjectionReleaseAndFIFO(t *testing.T) {
	svc, _ := newMinimalService(t)
	ctx := context.Background()
	factory := installTerminalWorkerFactory(svc)
	worker, err := svc.minimalWorkers.Acquire(ctx, "link:linked-stale-running-preprojection", "linked-stale-running-preprojection")
	if err != nil {
		t.Fatal(err)
	}
	link := seedRunningLinkedTerminal(t, svc, "linked-stale-running-preprojection", "turn-current", worker.Generation)
	if err := svc.store.SetGlobalObserverTarget(ctx, link.ChatID, link.TopicID, true); err != nil {
		t.Fatal(err)
	}
	if err := svc.store.EnqueuePendingCommand(ctx, model.PendingCommand{
		ThreadID: link.LinkedThreadID, ProjectID: link.ProjectID, ChatID: link.ChatID, TopicID: link.TopicID, Prompt: "must not drain stale running",
	}); err != nil {
		t.Fatal(err)
	}
	armMinimalTerminalTransition(t, svc, link.LinkedThreadID, svc.cfg.Projects[0].CanonicalPath, "turn-old")

	svc.handleTerminalSnapshot(ctx, linkedTerminalSnapshot(svc, link.LinkedThreadID, "turn-old", "old done"), false)

	if factory.Single(t).Closed() {
		t.Fatal("stale running terminal closed the active linked worker")
	}
	if calls := factory.Single(t).turnStartCalls; len(calls) != 0 {
		t.Fatalf("stale running terminal drained FIFO and started turns=%#v", calls)
	}
	got, err := svc.store.GetMinimalLinkedThreadByLinkedID(ctx, link.LinkedThreadID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != model.MinimalLinkedTelegramRunning || got.ActiveTurnID != "turn-current" || got.WorkerGeneration != worker.Generation {
		t.Fatalf("link after stale running terminal=%#v, want running turn-current gen %d", got, worker.Generation)
	}
	assertMinimalObservation(t, svc, link.LinkedThreadID, "turn-old", "inProgress")
	if count := countTerminalEvents(t, svc, "linked-stale-running-preprojection:turn-old:completed"); count != 0 {
		t.Fatalf("terminal events for stale running terminal=%d, want none", count)
	}
	if count := countDeliveryKind(t, svc, "terminal"); count != 0 {
		t.Fatalf("terminal deliveries for stale running terminal=%d, want none", count)
	}
	if pending := countPendingCommandsByStatus(t, svc, model.PendingCommandStatusPending); pending != 1 {
		t.Fatalf("pending commands after stale running terminal=%d, want FIFO untouched", pending)
	}
}

func TestStaleMinimalTerminalDoesNotReleaseNewerWorkerOrProjectOutbox(t *testing.T) {
	svc, _ := newMinimalService(t)
	ctx := context.Background()
	factory := installTerminalWorkerFactory(svc)
	worker, err := svc.minimalWorkers.Acquire(ctx, "link:linked-stale-terminal", "linked-stale-terminal")
	if err != nil {
		t.Fatal(err)
	}
	link := seedRunningLinkedTerminal(t, svc, "linked-stale-terminal", "turn-new", worker.Generation)
	if err := svc.store.SetGlobalObserverTarget(ctx, link.ChatID, link.TopicID, true); err != nil {
		t.Fatal(err)
	}
	armMinimalTerminalTransition(t, svc, link.LinkedThreadID, svc.cfg.Projects[0].CanonicalPath, "turn-old")

	svc.handleTerminalSnapshot(ctx, linkedTerminalSnapshot(svc, link.LinkedThreadID, "turn-old", "old done"), false)

	if factory.Single(t).Closed() {
		t.Fatal("stale terminal closed the newer worker")
	}
	got, err := svc.store.GetMinimalLinkedThreadByLinkedID(ctx, link.LinkedThreadID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != model.MinimalLinkedTelegramRunning || got.ActiveTurnID != "turn-new" || got.WorkerGeneration != worker.Generation {
		t.Fatalf("link after stale terminal=%#v, want newer running turn untouched", got)
	}
	if backlog, err := svc.store.DeliveryQueueBacklog(ctx); err != nil || backlog != 0 {
		t.Fatalf("stale terminal backlog=%d err=%v, want no outbox", backlog, err)
	}
}

func TestStaleReleasePendingTerminalDoesNotReleaseNewerWorker(t *testing.T) {
	svc, _ := newMinimalService(t)
	ctx := context.Background()
	factory := installTerminalWorkerFactory(svc)
	worker, err := svc.minimalWorkers.Acquire(ctx, "link:linked-release-pending-stale", "linked-release-pending-stale")
	if err != nil {
		t.Fatal(err)
	}
	link := seedRunningLinkedTerminal(t, svc, "linked-release-pending-stale", "turn-new", worker.Generation)
	inserted, err := svc.store.EnqueueTerminalEventWithLinkedRelease(ctx,
		model.TerminalEvent{TerminalKey: "linked-release-pending-stale:turn-new:completed", ThreadID: link.LinkedThreadID, TurnID: "turn-new", Status: "completed"},
		nil,
		&model.MinimalLinkedRelease{LinkedThreadID: link.LinkedThreadID, TurnID: "turn-new", WorkerGeneration: worker.Generation},
	)
	if err != nil || !inserted {
		t.Fatalf("release-pending seed inserted=%t err=%v, want true nil", inserted, err)
	}

	if err := svc.releaseMinimalTerminalWorker(ctx, linkedTerminalSnapshot(svc, link.LinkedThreadID, "turn-old", "old done")); err != nil {
		t.Fatalf("stale release returned error: %v", err)
	}

	if factory.Single(t).Closed() {
		t.Fatal("stale release_pending terminal closed the newer worker")
	}
	got, err := svc.store.GetMinimalLinkedThreadByLinkedID(ctx, link.LinkedThreadID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != model.MinimalLinkedReleasePending || got.ActiveTurnID != "turn-new" || got.WorkerGeneration != worker.Generation {
		t.Fatalf("link after stale release_pending terminal=%#v, want release_pending turn-new gen %d", got, worker.Generation)
	}
	if count := countDeliveryKind(t, svc, "handoff_ready"); count != 0 {
		t.Fatalf("handoff_ready deliveries=%d, want none for stale release_pending terminal", count)
	}
}

func TestStaleReleasePendingLinkedTerminalSkipsTerminalOutboxAndFIFO(t *testing.T) {
	svc, _ := newMinimalService(t)
	ctx := context.Background()
	factory := installTerminalWorkerFactory(svc)
	worker, err := svc.minimalWorkers.Acquire(ctx, "link:linked-release-pending-preprojection", "linked-release-pending-preprojection")
	if err != nil {
		t.Fatal(err)
	}
	link := seedRunningLinkedTerminal(t, svc, "linked-release-pending-preprojection", "turn-current", worker.Generation)
	if changed, err := svc.store.BeginMinimalLinkedRelease(ctx, model.MinimalLinkedRelease{LinkedThreadID: link.LinkedThreadID, TurnID: "turn-current", WorkerGeneration: worker.Generation}); err != nil || !changed {
		t.Fatalf("begin release changed=%t err=%v, want true nil", changed, err)
	}
	if err := svc.store.SetGlobalObserverTarget(ctx, link.ChatID, link.TopicID, true); err != nil {
		t.Fatal(err)
	}
	if err := svc.store.EnqueuePendingCommand(ctx, model.PendingCommand{
		ThreadID: link.LinkedThreadID, ProjectID: link.ProjectID, ChatID: link.ChatID, TopicID: link.TopicID, Prompt: "must not drain stale release_pending",
	}); err != nil {
		t.Fatal(err)
	}
	armMinimalTerminalTransition(t, svc, link.LinkedThreadID, svc.cfg.Projects[0].CanonicalPath, "turn-old")

	svc.handleTerminalSnapshot(ctx, linkedTerminalSnapshot(svc, link.LinkedThreadID, "turn-old", "old done"), false)

	if factory.Single(t).Closed() {
		t.Fatal("stale release_pending terminal closed the active linked worker")
	}
	if calls := factory.Single(t).turnStartCalls; len(calls) != 0 {
		t.Fatalf("stale release_pending terminal drained FIFO and started turns=%#v", calls)
	}
	got, err := svc.store.GetMinimalLinkedThreadByLinkedID(ctx, link.LinkedThreadID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != model.MinimalLinkedReleasePending || got.ActiveTurnID != "turn-current" || got.WorkerGeneration != worker.Generation {
		t.Fatalf("link after stale release_pending terminal=%#v, want release_pending turn-current gen %d", got, worker.Generation)
	}
	assertMinimalObservation(t, svc, link.LinkedThreadID, "turn-old", "inProgress")
	if count := countTerminalEvents(t, svc, "linked-release-pending-preprojection:turn-old:completed"); count != 0 {
		t.Fatalf("terminal events for stale release_pending terminal=%d, want none", count)
	}
	if count := countDeliveryKind(t, svc, "terminal"); count != 0 {
		t.Fatalf("terminal deliveries for stale release_pending terminal=%d, want none", count)
	}
	if count := countDeliveryKind(t, svc, "handoff_ready"); count != 0 {
		t.Fatalf("handoff_ready deliveries for stale release_pending terminal=%d, want none", count)
	}
	if pending := countPendingCommandsByStatus(t, svc, model.PendingCommandStatusPending); pending != 1 {
		t.Fatalf("pending commands after stale release_pending terminal=%d, want FIFO untouched", pending)
	}
}

func TestMinimalReleaseReadyDeliveryFailureKeepsReleasePendingAndBlocksFIFO(t *testing.T) {
	svc, _ := newMinimalService(t)
	ctx := context.Background()
	factory := installTerminalWorkerFactory(svc)
	worker, err := svc.minimalWorkers.Acquire(ctx, "link:linked-ready-failure", "linked-ready-failure")
	if err != nil {
		t.Fatal(err)
	}
	link := seedRunningLinkedTerminal(t, svc, "linked-ready-failure", "turn-ready", worker.Generation)
	if err := svc.store.SetGlobalObserverTarget(ctx, link.ChatID, link.TopicID, true); err != nil {
		t.Fatal(err)
	}
	if err := svc.store.EnqueuePendingCommand(ctx, model.PendingCommand{
		ThreadID: link.LinkedThreadID, SourceThreadID: link.SourceThreadID, SourceTurnID: link.SourceAnchorTurnID,
		ProjectID: link.ProjectID, ChatID: link.ChatID, TopicID: link.TopicID, Prompt: "must wait for ready tx",
	}); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", svc.store.Path())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TRIGGER reject_handoff_ready BEFORE INSERT ON delivery_queue WHEN NEW.kind='handoff_ready' BEGIN SELECT RAISE(ABORT, 'forced handoff ready failure'); END`); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()

	svc.handleTerminalSnapshot(ctx, linkedTerminalSnapshot(svc, link.LinkedThreadID, "turn-ready", "done"), false)

	sessions := factory.Sessions()
	if len(sessions) != 1 || !sessions[0].Closed() {
		t.Fatalf("closed sessions=%d firstClosed=%t, want confirmed close before ready failure", len(sessions), len(sessions) == 1 && sessions[0].Closed())
	}
	got, err := svc.store.GetMinimalLinkedThreadByLinkedID(ctx, link.LinkedThreadID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != model.MinimalLinkedReleasePending || got.ActiveTurnID != "turn-ready" || got.WorkerGeneration != worker.Generation {
		t.Fatalf("link after ready delivery failure=%#v, want release_pending turn-ready gen %d", got, worker.Generation)
	}
	if count := countDeliveryKind(t, svc, "handoff_ready"); count != 0 {
		t.Fatalf("handoff_ready deliveries=%d, want none after ready tx failure", count)
	}
	if pending := countPendingCommandsByStatus(t, svc, model.PendingCommandStatusPending); pending != 1 {
		t.Fatalf("pending commands=%d, want FIFO blocked by ready tx failure", pending)
	}
	if sessions := factory.Sessions(); len(sessions) != 1 {
		t.Fatalf("worker sessions after ready failure=%d, want no FIFO worker", len(sessions))
	}
}

func TestConfirmedCloseReadyFailureRetriesSameTerminalAndDrainsFIFO(t *testing.T) {
	svc, _ := newMinimalService(t)
	ctx := context.Background()
	factory := installTerminalWorkerFactory(svc)
	worker, err := svc.minimalWorkers.Acquire(ctx, "link:linked-ready-retry", "linked-ready-retry")
	if err != nil {
		t.Fatal(err)
	}
	link := seedRunningLinkedTerminal(t, svc, "linked-ready-retry", "turn-ready", worker.Generation)
	if err := svc.store.SetGlobalObserverTarget(ctx, link.ChatID, link.TopicID, true); err != nil {
		t.Fatal(err)
	}
	if err := svc.store.EnqueuePendingCommand(ctx, model.PendingCommand{
		ThreadID: link.LinkedThreadID, SourceThreadID: link.SourceThreadID, SourceTurnID: link.SourceAnchorTurnID,
		ProjectID: link.ProjectID, ChatID: link.ChatID, TopicID: link.TopicID, Prompt: "drain after ready retry",
	}); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", svc.store.Path())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TRIGGER reject_handoff_ready_retry BEFORE INSERT ON delivery_queue WHEN NEW.kind='handoff_ready' BEGIN SELECT RAISE(ABORT, 'forced handoff ready failure'); END`); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()
	snapshot := linkedTerminalSnapshot(svc, link.LinkedThreadID, "turn-ready", "done")

	svc.handleTerminalSnapshot(ctx, snapshot, false)

	sessions := factory.Sessions()
	if len(sessions) != 1 || !sessions[0].Closed() {
		t.Fatalf("first release sessions=%d firstClosed=%t, want confirmed close before ready failure", len(sessions), len(sessions) == 1 && sessions[0].Closed())
	}
	got, err := svc.store.GetMinimalLinkedThreadByLinkedID(ctx, link.LinkedThreadID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != model.MinimalLinkedReleasePending || got.ActiveTurnID != "turn-ready" || got.WorkerGeneration != worker.Generation {
		t.Fatalf("link after forced ready failure=%#v, want release_pending turn-ready gen %d", got, worker.Generation)
	}
	if count := countTerminalEvents(t, svc, "linked-ready-retry:turn-ready:completed"); count != 1 {
		t.Fatalf("terminal event after forced ready failure=%d, want durable terminal only once", count)
	}
	if count := countDeliveryKind(t, svc, "handoff_ready"); count != 0 {
		t.Fatalf("handoff_ready after forced ready failure=%d, want none", count)
	}
	if pending := countPendingCommandsByStatus(t, svc, model.PendingCommandStatusPending); pending != 1 {
		t.Fatalf("pending commands after forced ready failure=%d, want FIFO blocked", pending)
	}

	db, err = sql.Open("sqlite", svc.store.Path())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`DROP TRIGGER reject_handoff_ready_retry`); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()

	svc.handleTerminalSnapshot(ctx, snapshot, false)

	sessions = factory.Sessions()
	if len(sessions) != 2 {
		t.Fatalf("worker sessions after retry=%d, want released worker plus fresh FIFO worker", len(sessions))
	}
	if !sessions[0].Closed() || sessions[1].Closed() {
		t.Fatalf("worker closed states after retry first=%t second=%t, want first closed and second running", sessions[0].Closed(), sessions[1].Closed())
	}
	if calls := sessions[1].turnStartCalls; len(calls) != 1 || calls[0].threadID != link.LinkedThreadID || calls[0].message != "drain after ready retry" {
		t.Fatalf("fresh worker turn starts after retry=%#v, want queued command on linked thread", calls)
	}
	got, err = svc.store.GetMinimalLinkedThreadByLinkedID(ctx, link.LinkedThreadID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != model.MinimalLinkedTelegramRunning || got.ActiveTurnID == "" || got.WorkerGeneration == worker.Generation || got.WorkerGeneration == 0 {
		t.Fatalf("link after ready retry FIFO drain=%#v, want fresh running generation", got)
	}
	if count := countTerminalEvents(t, svc, "linked-ready-retry:turn-ready:completed"); count != 1 {
		t.Fatalf("terminal event after retry=%d, want no duplicate terminal", count)
	}
	if count := countDeliveryKindStatus(t, svc, "handoff_ready", model.DeliveryStatusPending); count != 1 {
		t.Fatalf("handoff_ready after retry=%d, want one pending notice", count)
	}
	if pending := countPendingCommandsByStatus(t, svc, model.PendingCommandStatusPending); pending != 0 {
		t.Fatalf("pending commands after ready retry=%d, want FIFO drained", pending)
	}
}

func TestReleasePendingWithoutConfirmedCloseDoesNotClaimReady(t *testing.T) {
	svc, _ := newMinimalService(t)
	ctx := context.Background()
	installTerminalWorkerFactory(svc)
	link := seedRunningLinkedTerminal(t, svc, "linked-no-confirmed-close", "turn-ready", 77)
	if changed, err := svc.store.BeginMinimalLinkedRelease(ctx, model.MinimalLinkedRelease{LinkedThreadID: link.LinkedThreadID, TurnID: "turn-ready", WorkerGeneration: 77}); err != nil || !changed {
		t.Fatalf("begin release changed=%t err=%v, want true nil", changed, err)
	}
	if err := svc.store.SetGlobalObserverTarget(ctx, link.ChatID, link.TopicID, true); err != nil {
		t.Fatal(err)
	}

	svc.handleTerminalSnapshot(ctx, linkedTerminalSnapshot(svc, link.LinkedThreadID, "turn-ready", "done"), false)

	got, err := svc.store.GetMinimalLinkedThreadByLinkedID(ctx, link.LinkedThreadID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State == model.MinimalLinkedReady {
		t.Fatalf("release_pending without confirmed close became ready: %#v", got)
	}
	if count := countDeliveryKind(t, svc, "handoff_ready"); count != 0 {
		t.Fatalf("handoff_ready without confirmed close=%d, want none", count)
	}
}

func TestMinimalLinkedTerminalReleaseDrainsNextCommandOnFreshWorker(t *testing.T) {
	svc, _ := newMinimalService(t)
	ctx := context.Background()
	factory := installTerminalWorkerFactory(svc)
	worker, err := svc.minimalWorkers.Acquire(ctx, "link:linked-fifo", "linked-fifo")
	if err != nil {
		t.Fatal(err)
	}
	link := seedRunningLinkedTerminal(t, svc, "linked-fifo", "turn-first", worker.Generation)
	if err := svc.store.SetGlobalObserverTarget(ctx, link.ChatID, link.TopicID, true); err != nil {
		t.Fatal(err)
	}
	if err := svc.store.EnqueuePendingCommand(ctx, model.PendingCommand{
		ThreadID: link.LinkedThreadID, SourceThreadID: link.SourceThreadID, SourceTurnID: link.SourceAnchorTurnID,
		ProjectID: link.ProjectID, ChatID: link.ChatID, TopicID: link.TopicID, Prompt: "next command",
	}); err != nil {
		t.Fatal(err)
	}

	svc.handleTerminalSnapshot(ctx, linkedTerminalSnapshot(svc, link.LinkedThreadID, "turn-first", "first done"), false)

	sessions := factory.Sessions()
	if len(sessions) != 2 {
		t.Fatalf("worker sessions=%d, want released worker plus fresh FIFO worker", len(sessions))
	}
	if !sessions[0].Closed() || sessions[1].Closed() {
		t.Fatalf("worker closed states first=%t second=%t, want first closed and second running", sessions[0].Closed(), sessions[1].Closed())
	}
	if calls := sessions[1].turnStartCalls; len(calls) != 1 || calls[0].threadID != link.LinkedThreadID || calls[0].message != "next command" {
		t.Fatalf("fresh worker turn starts=%#v, want queued command on linked thread", calls)
	}
	got, err := svc.store.GetMinimalLinkedThreadByLinkedID(ctx, link.LinkedThreadID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != model.MinimalLinkedTelegramRunning || got.ActiveTurnID == "" || got.WorkerGeneration == 0 || got.WorkerGeneration == worker.Generation {
		t.Fatalf("link after FIFO drain=%#v, want fresh running worker generation", got)
	}
}

func TestMinimalLinkedTerminalReleaseDrainsNextCommandWithWorkerContext(t *testing.T) {
	svc, _ := newMinimalService(t)
	ctx := context.Background()
	factory := installTerminalWorkerFactory(svc)
	worker, err := svc.minimalWorkers.Acquire(ctx, "link:linked-fifo-worker-context", "linked-fifo-worker-context")
	if err != nil {
		t.Fatal(err)
	}
	link := seedRunningLinkedTerminal(t, svc, "linked-fifo-worker-context", "turn-first", worker.Generation)
	if err := svc.store.SetGlobalObserverTarget(ctx, link.ChatID, link.TopicID, true); err != nil {
		t.Fatal(err)
	}
	if err := svc.store.EnqueuePendingCommand(ctx, model.PendingCommand{
		ThreadID: link.LinkedThreadID, SourceThreadID: link.SourceThreadID, SourceTurnID: link.SourceAnchorTurnID,
		ProjectID: link.ProjectID, ChatID: link.ChatID, TopicID: link.TopicID, Prompt: "next command from worker context",
	}); err != nil {
		t.Fatal(err)
	}

	svc.handleTerminalSnapshot(worker.Context, linkedTerminalSnapshot(svc, link.LinkedThreadID, "turn-first", "first done"), false)

	sessions := factory.Sessions()
	if len(sessions) != 2 {
		t.Fatalf("worker sessions=%d, want released worker plus fresh FIFO worker", len(sessions))
	}
	if !sessions[0].Closed() || sessions[1].Closed() {
		t.Fatalf("worker closed states first=%t second=%t, want first closed and second running", sessions[0].Closed(), sessions[1].Closed())
	}
	if calls := sessions[1].turnStartCalls; len(calls) != 1 || calls[0].threadID != link.LinkedThreadID || calls[0].message != "next command from worker context" {
		t.Fatalf("fresh worker turn starts=%#v, want queued command on linked thread", calls)
	}
	got, err := svc.store.GetMinimalLinkedThreadByLinkedID(ctx, link.LinkedThreadID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != model.MinimalLinkedTelegramRunning || got.ActiveTurnID == "" || got.WorkerGeneration == 0 || got.WorkerGeneration == worker.Generation {
		t.Fatalf("link after worker-context FIFO drain=%#v, want fresh running worker generation", got)
	}
}

func TestCanceledAliasesCanonicalizeToInterruptedTerminal(t *testing.T) {
	svc, _ := newMinimalService(t)
	ctx := context.Background()
	if err := svc.store.SetGlobalObserverTarget(ctx, 7, 0, true); err != nil {
		t.Fatal(err)
	}
	for i, status := range []string{"canceled", "cancelled", "CANCELED", "CANCELLED"} {
		snapshot := appserver.ThreadReadSnapshot{Thread: model.Thread{ID: "alias-" + string(rune('a'+i)), CWD: svc.cfg.Projects[0].CanonicalPath}, LatestTurnID: "turn", LatestTurnStatus: status, LatestFinalText: "stopped"}
		armMinimalTerminalTransition(t, svc, snapshot.Thread.ID, snapshot.Thread.CWD, snapshot.LatestTurnID)
		if err := svc.projectMinimalTerminal(ctx, snapshot); err != nil {
			t.Fatal(err)
		}
	}
	items, err := svc.store.ClaimDeliveryBatch(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 4 {
		t.Fatalf("alias chunks = %d, want 4", len(items))
	}
	for _, item := range items {
		if !strings.HasSuffix(item.GroupID, ":interrupted") || !strings.Contains(item.PayloadJSON, "[실패]") {
			t.Fatalf("alias item = %#v payload=%s", item, item.PayloadJSON)
		}
	}
}

func TestHandlerCanonicalizesCancellationAliasesBeforeProjectionAndDedupe(t *testing.T) {
	for _, status := range []string{"canceled", "cancelled", "CANCELED", "CANCELLED"} {
		t.Run(status, func(t *testing.T) {
			svc, _ := newMinimalService(t)
			ctx := context.Background()
			logs := captureServiceLogs(svc)
			if err := svc.store.SetGlobalObserverTarget(ctx, 7, 0, true); err != nil {
				t.Fatal(err)
			}
			const threadID = "handler-alias"
			const turnID = "turn"
			if err := svc.markTelegramOriginTurn(ctx, threadID, turnID); err != nil {
				t.Fatal(err)
			}
			if err := svc.markTelegramOriginExplicitInterrupt(ctx, threadID, turnID); err != nil {
				t.Fatal(err)
			}
			snapshot := appserver.ThreadReadSnapshot{Thread: model.Thread{ID: threadID, CWD: svc.cfg.Projects[0].CanonicalPath}, LatestTurnID: turnID, LatestTurnStatus: status, LatestFinalText: "stopped"}
			armMinimalTerminalTransition(t, svc, snapshot.Thread.ID, snapshot.Thread.CWD, snapshot.LatestTurnID)
			svc.handleTerminalSnapshot(ctx, snapshot, false)
			svc.handleTerminalSnapshot(ctx, snapshot, false)
			items, err := svc.store.ClaimDeliveryBatch(ctx, 10)
			if err != nil {
				t.Fatal(err)
			}
			if len(items) != 1 || items[0].GroupID != "handler-alias:turn:interrupted" {
				t.Fatalf("handler alias items = %#v", items)
			}
			var payload model.DeliveryPayload
			if err := json.Unmarshal([]byte(items[0].PayloadJSON), &payload); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(payload.Text, "[실패] Bridge\n대화: handler-\nThread: handler-") {
				t.Fatalf("handler alias delivery header = %q", payload.Text)
			}
			db, err := sql.Open("sqlite", svc.store.Path())
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			var terminalKey, terminalStatus string
			if err := db.QueryRow(`SELECT terminal_key, status FROM terminal_events`).Scan(&terminalKey, &terminalStatus); err != nil {
				t.Fatal(err)
			}
			if terminalKey != "handler-alias:turn:interrupted" || terminalStatus != "interrupted" {
				t.Fatalf("terminal key=%q status=%q", terminalKey, terminalStatus)
			}
			gotLogs := logs.String()
			if !strings.Contains(gotLogs, `"latest_turn_status":"interrupted"`) || strings.Contains(gotLogs, `"latest_turn_status":"`+status+`"`) {
				t.Fatalf("handler did not canonicalize %q before diagnostics: %s", status, gotLogs)
			}
		})
	}
}

func TestHandlerDoesNotTreatNearCancellationStatusAsTerminal(t *testing.T) {
	svc, _ := newMinimalService(t)
	ctx := context.Background()
	if err := svc.store.SetGlobalObserverTarget(ctx, 7, 0, true); err != nil {
		t.Fatal(err)
	}
	snapshot := appserver.ThreadReadSnapshot{Thread: model.Thread{ID: "not-terminal", CWD: svc.cfg.Projects[0].CanonicalPath}, LatestTurnID: "turn", LatestTurnStatus: "canceling", LatestFinalText: "not terminal"}
	svc.handleTerminalSnapshot(ctx, snapshot, false)
	items, err := svc.store.ClaimDeliveryBatch(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 0 {
		t.Fatalf("near-cancellation status projected terminal items: %#v", items)
	}
}

func TestMissingMessageIDRetriesSameTerminalChunk(t *testing.T) {
	for name, ids := range map[string][]int64{"missing": nil, "zero": {0}, "multiple": {41, 42}} {
		t.Run(name, func(t *testing.T) {
			svc, _ := newMinimalService(t)
			ctx := context.Background()
			svc.cfg.DeliveryRetryBase = 0
			svc.cfg.DeliveryMaxAttempts = 3
			if err := svc.store.SetGlobalObserverTarget(ctx, 7, 0, true); err != nil {
				t.Fatal(err)
			}
			snapshot := appserver.ThreadReadSnapshot{Thread: model.Thread{ID: "missing-id-" + name, CWD: svc.cfg.Projects[0].CanonicalPath}, LatestTurnID: "turn", LatestTurnStatus: "completed", LatestFinalText: "final"}
			armMinimalTerminalTransition(t, svc, snapshot.Thread.ID, snapshot.Thread.CWD, snapshot.LatestTurnID)
			if err := svc.projectMinimalTerminal(ctx, snapshot); err != nil {
				t.Fatal(err)
			}
			sender := &missingRenderedMessageIDSender{ids: ids}
			svc.SetSender(sender)
			svc.processDeliveryBatch(ctx)
			items, err := svc.store.ClaimDeliveryBatch(ctx, 1)
			if err != nil {
				t.Fatal(err)
			}
			if sender.calls != 1 || len(items) != 1 || items[0].SequenceNo != 1 || items[0].RetryCount != 1 {
				t.Fatalf("calls=%d retry items=%#v", sender.calls, items)
			}
		})
	}
}

func TestRouteCompletionFailureRetriesSameTerminalChunk(t *testing.T) {
	svc, _ := newMinimalService(t)
	ctx := context.Background()
	svc.cfg.DeliveryRetryBase = 0
	svc.cfg.DeliveryMaxAttempts = 3
	if err := svc.store.SetGlobalObserverTarget(ctx, 7, 0, true); err != nil {
		t.Fatal(err)
	}
	snapshot := appserver.ThreadReadSnapshot{Thread: model.Thread{ID: "route-failure", CWD: svc.cfg.Projects[0].CanonicalPath}, LatestTurnID: "turn", LatestTurnStatus: "completed", LatestFinalText: "final"}
	armMinimalTerminalTransition(t, svc, snapshot.Thread.ID, snapshot.Thread.CWD, snapshot.LatestTurnID)
	if err := svc.projectMinimalTerminal(ctx, snapshot); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", svc.store.Path())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TRIGGER reject_terminal_route BEFORE INSERT ON telegram_message_routes BEGIN SELECT RAISE(ABORT, 'injected route failure'); END`); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()
	sender := &recordingSender{}
	svc.SetSender(sender)
	svc.processDeliveryBatch(ctx)
	db, err = sql.Open("sqlite", svc.store.Path())
	if err != nil {
		t.Fatal(err)
	}
	var routes int
	var terminalStatus string
	if err := db.QueryRow(`SELECT count(*) FROM telegram_message_routes WHERE thread_id='route-failure'`).Scan(&routes); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT delivery_status FROM terminal_events WHERE terminal_key='route-failure:turn:completed'`).Scan(&terminalStatus); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()
	if routes != 0 || terminalStatus != model.DeliveryStatusPending {
		t.Fatalf("routes=%d terminal=%q", routes, terminalStatus)
	}
	items, err := svc.store.ClaimDeliveryBatch(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(sender.messages) != 1 || len(items) != 1 || items[0].RetryCount != 1 {
		t.Fatalf("messages=%d retry items=%#v", len(sender.messages), items)
	}
	state, err := svc.store.GetState(ctx, "daemon.last_error")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(state, "class=telegram_delivery status=persist_error") || strings.Contains(state, "final") {
		t.Fatalf("unsafe persistence error state = %q", state)
	}
}

func TestLiveAndPollTerminalSnapshotsShareDedupe(t *testing.T) {
	svc, app := newMinimalService(t)
	ctx := context.Background()
	if err := svc.store.SetGlobalObserverTarget(ctx, 7, 0, true); err != nil {
		t.Fatal(err)
	}
	threadID := "live-poll-dedupe"
	armMinimalTerminalTransition(t, svc, threadID, svc.cfg.Projects[0].CanonicalPath, "turn")
	app.threadReads = map[string]map[string]any{threadID: {"thread": map[string]any{
		"id": threadID, "cwd": svc.cfg.Projects[0].CanonicalPath, "status": "completed",
		"turns": []any{map[string]any{"id": "turn", "status": "completed", "items": []any{map[string]any{"id": "final", "type": "agentMessage", "phase": "final_answer", "text": "one final"}}}},
	}}}
	if _, err := svc.refreshThreadForOperation(ctx, app, threadID, "live_refresh"); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.refreshThreadForOperation(ctx, app, threadID, "poll_tracked"); err != nil {
		t.Fatal(err)
	}
	items, err := svc.store.ClaimDeliveryBatch(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].GroupID != "live-poll-dedupe:turn:completed" {
		t.Fatalf("deduped items=%#v", items)
	}
}
