package daemon

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alclssna33/codex_to_telegram/internal/appserver"
	"github.com/alclssna33/codex_to_telegram/internal/model"
	"github.com/alclssna33/codex_to_telegram/internal/securestore"
	"github.com/alclssna33/codex_to_telegram/internal/storage"
)

type continuationSubmitResult struct {
	response *DirectResponse
	err      error
}

func TestIsActiveWriterConflictRequiresExactCodeThreadAndMessage(t *testing.T) {
	cases := []struct {
		name string
		err  error
		id   string
		want bool
	}{
		{"exact", &appserver.RPCError{Code: -32600, Message: "thread parent already has an active writer"}, "parent", true},
		{"wrong code", &appserver.RPCError{Code: -32602, Message: "thread parent already has an active writer"}, "parent", false},
		{"wrong thread", &appserver.RPCError{Code: -32600, Message: "thread other already has an active writer"}, "parent", false},
		{"generic text", errors.New("thread parent already has an active writer"), "parent", false},
		{"timeout", context.DeadlineExceeded, "parent", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isActiveWriterConflict(tc.err, tc.id); got != tc.want {
				t.Fatalf("got %t want %t", got, tc.want)
			}
		})
	}
}

func TestPCContinuationForksOnWorkerAndPersistsCanonicalLink(t *testing.T) {
	svc, _ := newMinimalService(t)
	live := newRouterSession()
	useRouterSession(svc, live)
	workers := installWorkerFactory(svc, func(index int, worker *routerSession) {
		worker.forkIDs = []string{"linked-1"}
	})
	seedPCSource(t, svc, live, "source-1", "source-turn-1", "안클코")

	response, err := svc.minimalRouter.Submit(context.Background(), model.InboundText{ChatID: 7, ReplyToMessageID: 501, Text: "후속 작업"})
	if err != nil {
		t.Fatal(err)
	}

	worker := workers.Single(t)
	want := []string{"thread/fork:source-1:source-turn-1", "thread/name/set:linked-1", "turn/start:linked-1"}
	if got := worker.callSequence(); !reflect.DeepEqual(got, want) {
		t.Fatalf("worker calls=%#v, want %#v", got, want)
	}
	assertRouterSessionNoMutations(t, live)
	link, err := svc.store.GetMinimalLinkedThread(context.Background(), 7, 0, "source-1")
	if err != nil {
		t.Fatal(err)
	}
	if link == nil || link.LinkedThreadID != response.ThreadID || link.State != model.MinimalLinkedTelegramRunning || link.ActiveTurnID != response.TurnID {
		t.Fatalf("link=%#v response=%#v", link, response)
	}
}

func TestLinkedTitleUsesTransientSourceTitleAndPersistsProtectedPayload(t *testing.T) {
	svc, _ := newMinimalService(t)
	live := newRouterSession()
	useRouterSession(svc, live)
	const privateTitle = "LINKED_TITLE_PRIVATE_SENTINEL_5b7a"
	var setNameThread, setNameTitle string
	installWorkerFactory(svc, func(index int, worker *routerSession) {
		worker.forkIDs = []string{"linked-title-thread"}
		worker.setNameHook = func(threadID, name string) error {
			setNameThread = threadID
			setNameTitle = name
			return nil
		}
	})
	ctx := context.Background()
	project := svc.cfg.Projects[0]
	if err := svc.store.SetSelectedProject(ctx, 7, 0, project.ID); err != nil {
		t.Fatal(err)
	}
	source := model.Thread{ID: "source-title-thread", CWD: project.CanonicalPath, Status: "completed", Title: "source-title-thread"}
	if err := svc.store.UpsertThread(ctx, source); err != nil {
		t.Fatal(err)
	}
	if err := svc.store.SetBinding(ctx, 7, 0, source.ID, model.BindingModeBound); err != nil {
		t.Fatal(err)
	}
	if err := svc.store.PutMessageRoute(ctx, model.MessageRoute{ChatID: 7, MessageID: 501, ThreadID: source.ID, TurnID: "source-turn-1", CreatedAt: model.NowString()}); err != nil {
		t.Fatal(err)
	}
	live.threadReads = map[string]map[string]any{source.ID: {"thread": map[string]any{
		"id": source.ID, "title": privateTitle, "cwd": project.CanonicalPath, "status": "completed", "source": "vscode", "originator": "Codex Desktop",
		"turns": []any{map[string]any{"id": "source-turn-1", "status": "completed"}},
	}}}

	response := submitMinimal(t, svc, model.InboundText{ChatID: 7, UserID: 7, ReplyToMessageID: 501, Text: "제목 확인", ReceivedAt: svc.now()})

	if response.ThreadID != "linked-title-thread" {
		t.Fatalf("response=%#v, want linked title thread", response)
	}
	if setNameThread != "linked-title-thread" || setNameTitle != privateTitle+" · 텔레그램 연동" {
		t.Fatalf("thread/name/set = %q/%q, want desired title", setNameThread, setNameTitle)
	}
	link, err := svc.store.GetMinimalLinkedThread(ctx, 7, 0, source.ID)
	if err != nil {
		t.Fatal(err)
	}
	if link == nil || link.TitleState != model.MinimalLinkedTitleSet || link.DesiredTitle != privateTitle+" · 텔레그램 연동" {
		t.Fatalf("link=%#v, want decrypted desired title with title_state set", link)
	}
	stored, err := svc.store.GetThread(ctx, "linked-title-thread")
	if err != nil {
		t.Fatal(err)
	}
	if stored == nil || stored.Title != "linked-title-thread" || stored.LastPreview != "" {
		t.Fatalf("stored linked thread=%#v, want scrubbed title metadata", stored)
	}
	if err := svc.Close(); err != nil {
		t.Fatal(err)
	}
	if err := assertSQLiteFilesDoNotContain(t, svc, privateTitle); err != nil {
		t.Fatal(err)
	}
}

func TestExistingLinkedTitleHydratesLegacyPayloadFromSourceRead(t *testing.T) {
	svc, _ := newMinimalService(t)
	live := newRouterSession()
	useRouterSession(svc, live)
	ctx := context.Background()
	project := svc.cfg.Projects[0]
	const privateTitle = "LINKED_LEGACY_TITLE_PRIVATE_SENTINEL_6c91"
	var readThreadID, setNameThread, setNameTitle string
	workers := installWorkerFactory(svc, func(index int, worker *routerSession) {
		worker.threadReads = map[string]map[string]any{"source-legacy-title": {"thread": map[string]any{
			"id": "source-legacy-title", "title": privateTitle, "cwd": project.CanonicalPath, "status": "completed", "source": "vscode", "originator": "Codex Desktop",
			"turns": []any{map[string]any{"id": "source-turn-1", "status": "completed"}},
		}}}
		worker.readHook = func(threadID string) {
			readThreadID = threadID
		}
		worker.setNameHook = func(threadID, name string) error {
			setNameThread = threadID
			setNameTitle = name
			return nil
		}
	})
	source := model.Thread{ID: "source-legacy-title", Title: "source-legacy-title", CWD: project.CanonicalPath, Status: "completed"}
	if err := svc.store.UpsertThread(ctx, source); err != nil {
		t.Fatal(err)
	}
	claimed, created, err := svc.store.ClaimMinimalContinuation(ctx, model.MinimalContinuation{
		Key:       model.MinimalContinuationKey{ChatID: 7, SourceThreadID: source.ID, SourceTurnID: "source-turn-1"},
		ProjectID: project.ID,
	})
	if err != nil || !created {
		t.Fatalf("legacy claim created=%t err=%v", created, err)
	}
	if err := svc.store.ActivateMinimalContinuation(ctx, *claimed, model.Thread{ID: "linked-legacy-title", CWD: project.CanonicalPath, Status: "completed"}); err != nil {
		t.Fatal(err)
	}
	adopted, err := svc.store.AdoptMinimalLinkedThreads(ctx)
	if err != nil || adopted != 1 {
		t.Fatalf("adopted=%d err=%v, want 1 nil", adopted, err)
	}
	anchor := sourceTurnAnchor{ThreadID: source.ID, TurnID: "source-turn-1", Status: "completed", PCOrigin: true}

	result, err := svc.minimalRouter.startResumedOrLinkedTurn(ctx, model.InboundText{ChatID: 7, Text: "legacy title"}, &source, project, anchor, "legacy title")

	if err != nil {
		t.Fatal(err)
	}
	if result.threadID() != "linked-legacy-title" || result.linkTitle != privateTitle+" · 텔레그램 연동" {
		t.Fatalf("result=%#v, want hydrated linked title", result)
	}
	if readThreadID != source.ID {
		t.Fatalf("source title read thread = %q, want %q", readThreadID, source.ID)
	}
	if setNameThread != "linked-legacy-title" || setNameTitle != privateTitle+" · 텔레그램 연동" {
		t.Fatalf("thread/name/set = %q/%q, want hydrated desired title", setNameThread, setNameTitle)
	}
	if got := workers.Single(t).callSequence(); !reflect.DeepEqual(got, []string{"thread/resume:linked-legacy-title", "thread/name/set:linked-legacy-title", "turn/start:linked-legacy-title"}) {
		t.Fatalf("worker calls=%#v", got)
	}
	storedLink, err := svc.store.GetMinimalLinkedThread(ctx, 7, 0, source.ID)
	if err != nil {
		t.Fatal(err)
	}
	if storedLink == nil || storedLink.TitleState != model.MinimalLinkedTitleSet || storedLink.SourceTitle != privateTitle || storedLink.DesiredTitle != privateTitle+" · 텔레그램 연동" {
		t.Fatalf("stored link=%#v, want protected hydrated titles marked set", storedLink)
	}
	storedChild, err := svc.store.GetThread(ctx, "linked-legacy-title")
	if err != nil {
		t.Fatal(err)
	}
	if storedChild == nil || storedChild.Title != "linked-legacy-title" || strings.TrimSpace(storedChild.LastPreview) != "" {
		t.Fatalf("stored child=%#v, want scrubbed title metadata", storedChild)
	}
	if err := svc.Close(); err != nil {
		t.Fatal(err)
	}
	if err := assertSQLiteFilesDoNotContain(t, svc, privateTitle); err != nil {
		t.Fatal(err)
	}
	assertRouterSessionNoMutations(t, live)
}

func TestExistingLinkedPartialTitleFailureSkipsNamingAndLeavesPending(t *testing.T) {
	for _, tc := range []struct {
		name      string
		sourceID  string
		linkedID  string
		configure func(t *testing.T, svc *Service, worker *routerSession, sourceID, linkedID string)
	}{
		{
			name:     "read failure",
			sourceID: "source-partial-read",
			linkedID: "linked-partial-read",
			configure: func(t *testing.T, svc *Service, worker *routerSession, sourceID, linkedID string) {
				worker.threadReadErr = errors.New("source read failed")
			},
		},
		{
			name:     "protection failure",
			sourceID: "source-partial-protect",
			linkedID: "linked-partial-protect",
			configure: func(t *testing.T, svc *Service, worker *routerSession, sourceID, linkedID string) {
				project := svc.cfg.Projects[0]
				worker.threadReads = minimalLinkedTitleThreadReads(sourceID, "PARTIAL_SOURCE_TITLE_PROTECT_9b32", project.CanonicalPath)
				reopenMinimalServiceStore(t, svc, protectFailingTestProtector{delegate: securestore.NewDPAPIProtector(), err: errors.New("protect failed")})
			},
		},
		{
			name:     "reload failure",
			sourceID: "source-partial-reload",
			linkedID: "linked-partial-reload",
			configure: func(t *testing.T, svc *Service, worker *routerSession, sourceID, linkedID string) {
				project := svc.cfg.Projects[0]
				worker.threadReads = minimalLinkedTitleThreadReads(sourceID, "PARTIAL_SOURCE_TITLE_RELOAD_2f40", project.CanonicalPath)
				installMinimalLinkedTitleReloadFailure(t, svc, linkedID)
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc, _ := newMinimalService(t)
			live := newRouterSession()
			useRouterSession(svc, live)
			ctx := context.Background()
			project := svc.cfg.Projects[0]
			desiredTitle := "PARTIAL_DESIRED_TITLE_" + strings.ToUpper(strings.ReplaceAll(tc.name, " ", "_")) + minimalLinkedTitleSuffix
			source := seedPartialMinimalLinkedTitle(t, svc, tc.sourceID, tc.linkedID, desiredTitle)
			workers := installWorkerFactory(svc, func(index int, worker *routerSession) {
				tc.configure(t, svc, worker, tc.sourceID, tc.linkedID)
			})
			anchor := sourceTurnAnchor{ThreadID: source.ID, TurnID: "source-turn-1", Status: "completed", PCOrigin: true}

			result, err := svc.minimalRouter.startResumedOrLinkedTurn(ctx, model.InboundText{ChatID: 7, Text: "partial title"}, &source, project, anchor, "partial title")

			if err != nil {
				t.Fatal(err)
			}
			if result.threadID() != tc.linkedID || strings.TrimSpace(result.turnID) == "" {
				t.Fatalf("result=%#v, want started linked turn", result)
			}
			worker := workers.Single(t)
			if got := worker.ThreadSetNameCalls(); got != 0 {
				t.Fatalf("thread/name/set calls=%d, want 0 after %s", got, tc.name)
			}
			if state := minimalLinkedTitleStateRaw(t, svc, tc.linkedID); state != model.MinimalLinkedTitlePending {
				t.Fatalf("title_state=%q, want pending after %s", state, tc.name)
			}
			assertRouterSessionNoMutations(t, live)
		})
	}
}

func TestTelegramOriginCompletedReplyResumesOnWorkerWithoutFork(t *testing.T) {
	svc, _ := newMinimalService(t)
	live := newRouterSession()
	useRouterSession(svc, live)
	workers := installWorkerFactory(svc)
	parent := model.Thread{ID: "parent", CWD: svc.cfg.Projects[0].CanonicalPath, Status: "completed"}
	if err := svc.store.UpsertThread(context.Background(), parent); err != nil {
		t.Fatal(err)
	}
	if err := svc.markTelegramOriginTurn(context.Background(), parent.ID, "turn-2"); err != nil {
		t.Fatal(err)
	}
	if err := svc.store.PutMessageRoute(context.Background(), model.MessageRoute{ChatID: 100, MessageID: 501, ThreadID: parent.ID, TurnID: "turn-2", CreatedAt: model.NowString()}); err != nil {
		t.Fatal(err)
	}
	live.threadReads = map[string]map[string]any{parent.ID: {"thread": map[string]any{
		"id": parent.ID, "cwd": parent.CWD, "status": "completed",
		"turns": []any{map[string]any{"id": "turn-2", "status": "completed"}},
	}}}
	live.forkIDs = []string{"must-not-fork"}

	response := submitMinimal(t, svc, model.InboundText{ChatID: 100, UserID: 7, ReplyToMessageID: 501, Text: "continue", ReceivedAt: svc.now()})

	if response.ThreadID != parent.ID || strings.Contains(response.Text, "새 대화") {
		t.Fatalf("response=%#v", response)
	}
	worker := workers.Single(t)
	want := []string{"thread/resume:parent", "turn/start:parent"}
	if got := worker.callSequence(); !reflect.DeepEqual(got, want) {
		t.Fatalf("worker calls=%#v, want %#v", got, want)
	}
	assertRouterSessionNoMutations(t, live)
	if got := worker.forkCallCount(); got != 0 {
		t.Fatalf("worker fork calls=%d, want none", got)
	}
}

func TestSelectedPCOriginSourceResumesExactSourceWithoutFork(t *testing.T) {
	svc, _ := newMinimalService(t)
	live := newRouterSession()
	useRouterSession(svc, live)
	workers := installWorkerFactory(svc, func(index int, worker *routerSession) {
		worker.forkIDs = []string{"must-not-create-linked-child"}
	})
	seedPCSource(t, svc, live, "source-direct", "source-turn-1", "안클코")

	response := submitMinimal(t, svc, model.InboundText{ChatID: 7, UserID: 7, Text: "직접 이어서 실행", ReceivedAt: svc.now()})

	if response.ThreadID != "source-direct" || response.Text != "작업을 시작했습니다." {
		t.Fatalf("response=%#v, want direct source start", response)
	}
	worker := workers.Single(t)
	if got := worker.callSequence(); !reflect.DeepEqual(got, []string{"thread/resume:source-direct", "turn/start:source-direct"}) {
		t.Fatalf("worker calls=%#v, want direct source resume/start", got)
	}
	if got := worker.forkCallCount(); got != 0 {
		t.Fatalf("fork calls=%d, want none for selected source", got)
	}
	if link, err := svc.store.GetMinimalLinkedThread(context.Background(), 7, 0, "source-direct"); err != nil || link != nil {
		t.Fatalf("linked row=%#v err=%v, want none", link, err)
	}
	assertRouterSessionNoMutations(t, live)
}

func TestSelectedPCOriginSourceActiveWriterConflictFailsClosedWithoutForkOrPromptPersistence(t *testing.T) {
	svc, _ := newMinimalService(t)
	live := newRouterSession()
	useRouterSession(svc, live)
	workers := installWorkerFactory(svc, func(index int, worker *routerSession) {
		worker.resumeErrByThread = map[string]error{"source-direct": &appserver.RPCError{Code: -32600, Message: "thread source-direct already has an active writer"}}
		worker.forkIDs = []string{"must-not-create-linked-child"}
	})
	seedPCSource(t, svc, live, "source-direct", "source-turn-1", "안클코")
	const prompt = "DIRECT_SOURCE_REJECTED_PROMPT_61f4"

	response, err := svc.minimalRouter.Submit(context.Background(), model.InboundText{ChatID: 7, UserID: 7, Text: prompt, ReceivedAt: svc.now()})
	if err != nil {
		t.Fatal(err)
	}
	if response == nil || response.ThreadID != "source-direct" || !strings.Contains(response.Text, "Codex에서") || !strings.Contains(response.Text, "Codex 앱을 종료") {
		t.Fatalf("response=%#v, want close-and-retry guidance for source", response)
	}
	worker := workers.Single(t)
	if got := worker.callSequence(); !reflect.DeepEqual(got, []string{"thread/resume:source-direct"}) {
		t.Fatalf("worker calls=%#v, want source resume probe only", got)
	}
	if !worker.Closed() {
		t.Fatal("blocked source worker was not closed")
	}
	if got := worker.forkCallCount(); got != 0 {
		t.Fatalf("fork calls=%d, want none for active-writer source", got)
	}
	if link, linkErr := svc.store.GetMinimalLinkedThread(context.Background(), 7, 0, "source-direct"); linkErr != nil || link != nil {
		t.Fatalf("linked row=%#v err=%v, want no canonical link creation", link, linkErr)
	}
	if queued, queueErr := svc.store.ClaimPendingCommand(context.Background(), "source-direct"); queueErr != nil || queued != nil {
		t.Fatalf("source queue after rejected prompt=%#v err=%v, want none", queued, queueErr)
	}
	assertRouterSessionNoMutations(t, live)
	if err := svc.Close(); err != nil {
		t.Fatal(err)
	}
	if err := assertSQLiteFilesDoNotContain(t, svc, prompt); err != nil {
		t.Fatal(err)
	}
}

func TestSelectedPCOriginSourceActiveRefreshConflictFailsClosedWithoutQueueOrPromptPersistence(t *testing.T) {
	svc, _ := newMinimalService(t)
	live := newRouterSession()
	useRouterSession(svc, live)
	workers := installWorkerFactory(svc, func(index int, worker *routerSession) {
		worker.resumeErrByThread = map[string]error{"source-active": &appserver.RPCError{Code: -32600, Message: "thread source-active already has an active writer"}}
		worker.forkIDs = []string{"must-not-create-active-linked-child"}
	})
	seedPCSource(t, svc, live, "source-active", "source-turn-1", "안클코")
	live.threadReads = map[string]map[string]any{"source-active": {"thread": map[string]any{
		"id": "source-active", "title": "안클코", "cwd": svc.cfg.Projects[0].CanonicalPath, "status": "active", "activeTurnId": "active-turn-2",
		"source": "vscode", "originator": "Codex Desktop",
		"turns": []any{
			map[string]any{"id": "source-turn-1", "status": "completed"},
			map[string]any{"id": "active-turn-2", "status": "inProgress"},
		},
	}}}
	const prompt = "DIRECT_SOURCE_ACTIVE_REJECTED_PROMPT_0f2e"

	response, err := svc.minimalRouter.Submit(context.Background(), model.InboundText{ChatID: 7, UserID: 7, Text: prompt, ReceivedAt: svc.now()})
	if err != nil {
		t.Fatal(err)
	}
	if response == nil || response.ThreadID != "source-active" || !strings.Contains(response.Text, "Codex에서") || !strings.Contains(response.Text, "Codex 앱을 종료") {
		t.Fatalf("response=%#v, want active source close-and-retry guidance", response)
	}
	worker := workers.Single(t)
	if got := worker.callSequence(); !reflect.DeepEqual(got, []string{"thread/resume:source-active"}) {
		t.Fatalf("worker calls=%#v, want source resume probe only", got)
	}
	if !worker.Closed() {
		t.Fatal("blocked active source worker was not closed")
	}
	if got := worker.forkCallCount(); got != 0 {
		t.Fatalf("fork calls=%d, want none for active source", got)
	}
	if calls := worker.turnCalls(); len(calls) != 0 {
		t.Fatalf("turn starts=%#v, want none for active-writer source", calls)
	}
	if link, linkErr := svc.store.GetMinimalLinkedThread(context.Background(), 7, 0, "source-active"); linkErr != nil || link != nil {
		t.Fatalf("linked row=%#v err=%v, want no canonical link creation", link, linkErr)
	}
	if queued, queueErr := svc.store.ClaimPendingCommand(context.Background(), "source-active"); queueErr != nil || queued != nil {
		t.Fatalf("source queue after rejected active prompt=%#v err=%v, want none", queued, queueErr)
	}
	assertRouterSessionNoMutations(t, live)
	if err := svc.Close(); err != nil {
		t.Fatal(err)
	}
	if err := assertSQLiteFilesDoNotContain(t, svc, prompt); err != nil {
		t.Fatal(err)
	}
}

func TestRepeatedParentReplyResumesCanonicalLink(t *testing.T) {
	svc, _ := newMinimalService(t)
	live := newRouterSession()
	useRouterSession(svc, live)
	workers := installWorkerFactory(svc, func(index int, worker *routerSession) {
		if index == 0 {
			worker.forkIDs = []string{"linked-1"}
		}
	})
	seedPCSource(t, svc, live, "source-1", "source-turn-1", "안클코")
	submitMinimal(t, svc, model.InboundText{ChatID: 7, UserID: 7, ReplyToMessageID: 501, Text: "first", ReceivedAt: svc.now()})
	releaseLinkedWorkerForTest(t, svc, 7, 0, "source-1")
	if err := svc.store.UpsertThread(context.Background(), model.Thread{ID: "linked-1", CWD: svc.cfg.Projects[0].CanonicalPath, Status: "completed"}); err != nil {
		t.Fatal(err)
	}

	second := submitMinimal(t, svc, model.InboundText{ChatID: 7, UserID: 7, ReplyToMessageID: 501, Text: "second", ReceivedAt: svc.now()})

	if second.ThreadID != "linked-1" || second.Text != "작업을 시작했습니다." {
		t.Fatalf("second response=%#v", second)
	}
	if got := workers.Session(t, 0).callSequence(); !reflect.DeepEqual(got, []string{"thread/fork:source-1:source-turn-1", "thread/name/set:linked-1", "turn/start:linked-1"}) {
		t.Fatalf("first worker calls=%#v", got)
	}
	if got := workers.Session(t, 1).callSequence(); !reflect.DeepEqual(got, []string{"thread/resume:linked-1", "turn/start:linked-1"}) {
		t.Fatalf("second worker calls=%#v", got)
	}
	if got := workers.Session(t, 1).forkCallCount(); got != 0 {
		t.Fatalf("second worker fork calls=%d, want none", got)
	}
	assertRouterSessionNoMutations(t, live)
}

func TestExistingLinkedActiveWriterConflictClosesWorkerAndKeepsPromptOutOfQueue(t *testing.T) {
	svc, _ := newMinimalService(t)
	live := newRouterSession()
	useRouterSession(svc, live)
	workers := installWorkerFactory(svc, func(index int, worker *routerSession) {
		worker.resumeErrByThread = map[string]error{"linked-1": &appserver.RPCError{Code: -32600, Message: "thread linked-1 already has an active writer"}}
	})
	seedReadyLinkedThread(t, svc, 7, 0, "source-1", "source-turn-1", "linked-1")
	if err := svc.store.PutMessageRoute(context.Background(), model.MessageRoute{ChatID: 7, MessageID: 501, ThreadID: "source-1", TurnID: "source-turn-1", CreatedAt: model.NowString()}); err != nil {
		t.Fatal(err)
	}
	live.threadReads = map[string]map[string]any{"source-1": {"thread": map[string]any{
		"id": "source-1", "cwd": svc.cfg.Projects[0].CanonicalPath, "status": "completed", "source": "vscode", "originator": "Codex Desktop",
		"turns": []any{map[string]any{"id": "source-turn-1", "status": "completed"}},
	}}}

	response, err := svc.minimalRouter.Submit(context.Background(), model.InboundText{ChatID: 7, ReplyToMessageID: 501, Text: "do not store me"})
	if err != nil {
		t.Fatal(err)
	}

	if response == nil || !strings.Contains(response.Text, "Codex에서") || !strings.Contains(response.Text, "Codex 앱을 종료") {
		t.Fatalf("response=%#v", response)
	}
	worker := workers.Single(t)
	if got := worker.callSequence(); !reflect.DeepEqual(got, []string{"thread/resume:linked-1"}) {
		t.Fatalf("worker calls=%#v", got)
	}
	if !worker.Closed() {
		t.Fatal("blocked linked worker was not closed")
	}
	if queued, queueErr := svc.store.ClaimPendingCommand(context.Background(), "linked-1"); queueErr != nil || queued != nil {
		t.Fatalf("linked queue after rejected prompt=%#v err=%v", queued, queueErr)
	}
	link, err := svc.store.GetMinimalLinkedThread(context.Background(), 7, 0, "source-1")
	if err != nil {
		t.Fatal(err)
	}
	if link == nil || link.State != model.MinimalLinkedReady || link.LastBlockedCode != "active_writer" {
		t.Fatalf("blocked link=%#v", link)
	}
	assertRouterSessionNoMutations(t, live)
}

func TestLinkedFirstCreationMarkerFailureKeepsStartedWorkerRegistered(t *testing.T) {
	svc, _ := newMinimalService(t)
	live := newRouterSession()
	useRouterSession(svc, live)
	ctx := context.Background()
	workers := installWorkerFactory(svc, func(index int, worker *routerSession) {
		worker.forkIDs = []string{"linked-1"}
		worker.turnStartHook = func(threadID, message, turnID string) {
			if threadID != "linked-1" {
				return
			}
			changed, err := svc.store.MarkMinimalLinkedTurnStarted(ctx, threadID, 1, "raced-turn")
			if err != nil || !changed {
				t.Fatalf("pre-mark linked turn changed=%t err=%v", changed, err)
			}
		}
	})
	project := svc.cfg.Projects[0]
	parent := model.Thread{ID: "source-1", CWD: project.CanonicalPath, Status: "completed", Title: "안클코"}
	if err := svc.store.UpsertThread(ctx, parent); err != nil {
		t.Fatal(err)
	}
	anchor := sourceTurnAnchor{ThreadID: parent.ID, TurnID: "source-turn-1", Status: "completed", PCOrigin: true}

	result, err := svc.minimalRouter.startResumedOrLinkedTurn(ctx, model.InboundText{ChatID: 7, Text: "first"}, &parent, project, anchor, "first")

	if err == nil || strings.TrimSpace(result.turnID) == "" || result.threadID() != "linked-1" {
		t.Fatalf("result=%#v err=%v, want started turn plus marker error", result, err)
	}
	worker := workers.Single(t)
	if worker.Closed() {
		t.Fatal("worker closed after remote turn started but marker failed")
	}
	if registered, ok := svc.minimalWorkers.ByThread("linked-1"); !ok || registered.Generation != 1 {
		t.Fatalf("registered worker=%#v ok=%t, want generation 1", registered, ok)
	}
	assertRouterSessionNoMutations(t, live)
}

func TestLinkedCanonicalReuseMarkerFailureKeepsStartedWorkerRegistered(t *testing.T) {
	svc, _ := newMinimalService(t)
	live := newRouterSession()
	useRouterSession(svc, live)
	ctx := context.Background()
	workers := installWorkerFactory(svc, func(index int, worker *routerSession) {
		if index == 0 {
			worker.forkIDs = []string{"linked-1"}
			return
		}
		worker.turnStartHook = func(threadID, message, turnID string) {
			if threadID != "linked-1" {
				return
			}
			changed, err := svc.store.MarkMinimalLinkedTurnStarted(ctx, threadID, 2, "raced-turn")
			if err != nil || !changed {
				t.Fatalf("pre-mark linked turn changed=%t err=%v", changed, err)
			}
		}
	})
	project := svc.cfg.Projects[0]
	parent := model.Thread{ID: "source-1", CWD: project.CanonicalPath, Status: "completed", Title: "안클코"}
	if err := svc.store.UpsertThread(ctx, parent); err != nil {
		t.Fatal(err)
	}
	anchor := sourceTurnAnchor{ThreadID: parent.ID, TurnID: "source-turn-1", Status: "completed", PCOrigin: true}
	if result, err := svc.minimalRouter.startResumedOrLinkedTurn(ctx, model.InboundText{ChatID: 7, Text: "first"}, &parent, project, anchor, "first"); err != nil || result.threadID() != "linked-1" {
		t.Fatalf("initial result=%#v err=%v", result, err)
	}
	releaseLinkedWorkerForTest(t, svc, 7, 0, parent.ID)
	if err := svc.store.UpsertThread(ctx, model.Thread{ID: "linked-1", CWD: project.CanonicalPath, Status: "completed"}); err != nil {
		t.Fatal(err)
	}

	result, err := svc.minimalRouter.startResumedOrLinkedTurn(ctx, model.InboundText{ChatID: 7, Text: "second"}, &parent, project, anchor, "second")

	if err == nil || strings.TrimSpace(result.turnID) == "" || result.threadID() != "linked-1" {
		t.Fatalf("result=%#v err=%v, want started turn plus marker error", result, err)
	}
	worker := workers.Session(t, 1)
	if worker.Closed() {
		t.Fatal("reuse worker closed after remote turn started but marker failed")
	}
	if registered, ok := svc.minimalWorkers.ByThread("linked-1"); !ok || registered.Generation != 2 {
		t.Fatalf("registered worker=%#v ok=%t, want generation 2", registered, ok)
	}
	assertRouterSessionNoMutations(t, live)
}

func TestLinkedLegacyAdoptionMarkerFailureKeepsStartedWorkerRegistered(t *testing.T) {
	svc, _ := newMinimalService(t)
	live := newRouterSession()
	useRouterSession(svc, live)
	ctx := context.Background()
	workers := installWorkerFactory(svc, func(index int, worker *routerSession) {
		worker.turnStartHook = func(threadID, message, turnID string) {
			if threadID != "legacy-child" {
				return
			}
			changed, err := svc.store.MarkMinimalLinkedTurnStarted(ctx, threadID, 1, "raced-turn")
			if err != nil || !changed {
				t.Fatalf("pre-mark linked turn changed=%t err=%v", changed, err)
			}
		}
	})
	project := svc.cfg.Projects[0]
	parent := model.Thread{ID: "legacy-source", CWD: project.CanonicalPath, Status: "completed", Title: "안클코"}
	if err := svc.store.UpsertThread(ctx, parent); err != nil {
		t.Fatal(err)
	}
	key := model.MinimalContinuationKey{ChatID: 7, TopicID: 0, SourceThreadID: parent.ID, SourceTurnID: "source-turn-1"}
	claim, created, err := svc.store.ClaimMinimalContinuation(ctx, model.MinimalContinuation{Key: key, ProjectID: project.ID})
	if err != nil || !created {
		t.Fatalf("legacy claim created=%t err=%v", created, err)
	}
	if err := svc.store.ActivateMinimalContinuation(ctx, *claim, model.Thread{ID: "legacy-child", CWD: project.CanonicalPath, Status: "completed"}); err != nil {
		t.Fatal(err)
	}
	anchor := sourceTurnAnchor{ThreadID: parent.ID, TurnID: "source-turn-1", Status: "completed", PCOrigin: true}

	result, err := svc.minimalRouter.startResumedOrLinkedTurn(ctx, model.InboundText{ChatID: 7, Text: "legacy"}, &parent, project, anchor, "legacy")

	if err == nil || strings.TrimSpace(result.turnID) == "" || result.threadID() != "legacy-child" {
		t.Fatalf("result=%#v err=%v, want started turn plus marker error", result, err)
	}
	worker := workers.Single(t)
	if worker.Closed() {
		t.Fatal("legacy adoption worker closed after remote turn started but marker failed")
	}
	if registered, ok := svc.minimalWorkers.ByThread("legacy-child"); !ok || registered.Generation != 1 {
		t.Fatalf("registered worker=%#v ok=%t, want generation 1", registered, ok)
	}
	assertRouterSessionNoMutations(t, live)
}

func TestRunningCanonicalLinkAlwaysQueuesEvenIfStoredChildLooksInactive(t *testing.T) {
	svc, _ := newMinimalService(t)
	live := newRouterSession()
	useRouterSession(svc, live)
	ctx := context.Background()
	workers := installWorkerFactory(svc, func(index int, worker *routerSession) {
		worker.forkIDs = []string{"linked-1"}
	})
	project := svc.cfg.Projects[0]
	parent := model.Thread{ID: "source-1", CWD: project.CanonicalPath, Status: "completed", Title: "안클코"}
	if err := svc.store.UpsertThread(ctx, parent); err != nil {
		t.Fatal(err)
	}
	anchor := sourceTurnAnchor{ThreadID: parent.ID, TurnID: "source-turn-1", Status: "completed", PCOrigin: true}
	first, err := svc.minimalRouter.startResumedOrLinkedTurn(ctx, model.InboundText{ChatID: 7, Text: "first"}, &parent, project, anchor, "first")
	if err != nil || first.threadID() != "linked-1" {
		t.Fatalf("initial result=%#v err=%v", first, err)
	}
	if err := svc.store.UpsertThread(ctx, model.Thread{ID: "linked-1", CWD: project.CanonicalPath, Status: "completed"}); err != nil {
		t.Fatal(err)
	}

	second, err := svc.minimalRouter.startResumedOrLinkedTurn(ctx, model.InboundText{ChatID: 7, Text: "second while running"}, &parent, project, anchor, "second while running")

	if err != nil {
		t.Fatal(err)
	}
	if !second.queued || second.threadID() != "linked-1" || second.turnID != first.turnID {
		t.Fatalf("second result=%#v first=%#v, want queued behind running link", second, first)
	}
	if got := workers.Single(t).callSequence(); !reflect.DeepEqual(got, []string{"thread/fork:source-1:source-turn-1", "thread/name/set:linked-1", "turn/start:linked-1"}) {
		t.Fatalf("worker calls=%#v, want no second turn/start", got)
	}
	queued, queueErr := svc.store.ClaimPendingCommand(ctx, "linked-1")
	if queueErr != nil || queued == nil || queued.Prompt != "second while running" || queued.SourceThreadID != parent.ID {
		t.Fatalf("queued=%#v err=%v", queued, queueErr)
	}
	assertRouterSessionNoMutations(t, live)
}

func TestCanonicalLinkedBacklogDrainUsesLinkReuseAndMarksOwnership(t *testing.T) {
	svc, _ := newMinimalService(t)
	live := newRouterSession()
	useRouterSession(svc, live)
	ctx := context.Background()
	workers := installWorkerFactory(svc, func(index int, worker *routerSession) {
		if index == 0 {
			worker.forkIDs = []string{"linked-1"}
		}
	})
	project := svc.cfg.Projects[0]
	parent := model.Thread{ID: "source-1", CWD: project.CanonicalPath, Status: "completed", Title: "안클코"}
	if err := svc.store.UpsertThread(ctx, parent); err != nil {
		t.Fatal(err)
	}
	anchor := sourceTurnAnchor{ThreadID: parent.ID, TurnID: "source-turn-1", Status: "completed", PCOrigin: true}
	first, err := svc.minimalRouter.startResumedOrLinkedTurn(ctx, model.InboundText{ChatID: 7, Text: "first"}, &parent, project, anchor, "first")
	if err != nil || first.threadID() != "linked-1" {
		t.Fatalf("initial result=%#v err=%v", first, err)
	}
	releaseLinkedWorkerForTest(t, svc, 7, 0, parent.ID)
	if err := svc.store.UpsertThread(ctx, model.Thread{ID: "linked-1", CWD: project.CanonicalPath, Status: "completed"}); err != nil {
		t.Fatal(err)
	}
	if err := svc.store.EnqueuePendingCommand(ctx, model.PendingCommand{
		ThreadID: "linked-1", SourceThreadID: parent.ID, SourceTurnID: "source-turn-1",
		ProjectID: project.ID, ChatID: 7, TopicID: 0, Prompt: "queued canonical",
	}); err != nil {
		t.Fatal(err)
	}

	if err := svc.minimalRouter.DrainNext(ctx, "linked-1"); err != nil {
		t.Fatal(err)
	}

	if got := workers.Session(t, 1).callSequence(); !reflect.DeepEqual(got, []string{"thread/resume:linked-1", "turn/start:linked-1"}) {
		t.Fatalf("drain worker calls=%#v", got)
	}
	link, err := svc.store.GetMinimalLinkedThread(ctx, 7, 0, parent.ID)
	if err != nil {
		t.Fatal(err)
	}
	if link == nil || link.State != model.MinimalLinkedTelegramRunning || link.WorkerGeneration != 2 || strings.TrimSpace(link.ActiveTurnID) == "" {
		t.Fatalf("link after drain=%#v, want generation 2 running with active turn", link)
	}
	if queued, queueErr := svc.store.ClaimPendingCommand(ctx, "linked-1"); queueErr != nil || queued != nil {
		t.Fatalf("queued after drain=%#v err=%v", queued, queueErr)
	}
	assertRouterSessionNoMutations(t, live)
}

func TestCanonicalLinkedBacklogDrainActiveWriterReleasesClaimForResend(t *testing.T) {
	svc, _ := newMinimalService(t)
	live := newRouterSession()
	useRouterSession(svc, live)
	ctx := context.Background()
	workers := installWorkerFactory(svc, func(index int, worker *routerSession) {
		if index == 0 {
			worker.forkIDs = []string{"linked-1"}
			return
		}
		worker.resumeErrByThread = map[string]error{"linked-1": &appserver.RPCError{Code: -32600, Message: "thread linked-1 already has an active writer"}}
	})
	project := svc.cfg.Projects[0]
	parent := model.Thread{ID: "source-1", CWD: project.CanonicalPath, Status: "completed", Title: "안클코"}
	if err := svc.store.UpsertThread(ctx, parent); err != nil {
		t.Fatal(err)
	}
	anchor := sourceTurnAnchor{ThreadID: parent.ID, TurnID: "source-turn-1", Status: "completed", PCOrigin: true}
	first, err := svc.minimalRouter.startResumedOrLinkedTurn(ctx, model.InboundText{ChatID: 7, Text: "first"}, &parent, project, anchor, "first")
	if err != nil || first.threadID() != "linked-1" {
		t.Fatalf("initial result=%#v err=%v", first, err)
	}
	releaseLinkedWorkerForTest(t, svc, 7, 0, parent.ID)
	if err := svc.store.UpsertThread(ctx, model.Thread{ID: "linked-1", CWD: project.CanonicalPath, Status: "completed"}); err != nil {
		t.Fatal(err)
	}
	if err := svc.store.EnqueuePendingCommand(ctx, model.PendingCommand{
		ThreadID: "linked-1", SourceThreadID: parent.ID, SourceTurnID: "source-turn-1",
		ProjectID: project.ID, ChatID: 7, TopicID: 0, Prompt: "retry after codex",
	}); err != nil {
		t.Fatal(err)
	}

	if err := svc.minimalRouter.DrainNext(ctx, "linked-1"); err != nil {
		t.Fatal(err)
	}

	worker := workers.Session(t, 1)
	if got := worker.callSequence(); !reflect.DeepEqual(got, []string{"thread/resume:linked-1"}) {
		t.Fatalf("drain worker calls=%#v", got)
	}
	if !worker.Closed() {
		t.Fatal("active-writer worker was not closed")
	}
	retry, err := svc.store.ClaimPendingCommand(ctx, "linked-1")
	if err != nil || retry == nil || retry.Prompt != "retry after codex" {
		t.Fatalf("retry=%#v err=%v, want prompt released for explicit resend", retry, err)
	}
	link, err := svc.store.GetMinimalLinkedThread(ctx, 7, 0, parent.ID)
	if err != nil {
		t.Fatal(err)
	}
	if link == nil || link.State != model.MinimalLinkedReady || link.LastBlockedCode != "active_writer" || strings.TrimSpace(link.ActiveTurnID) != "" {
		t.Fatalf("link after active writer=%#v", link)
	}
	assertRouterSessionNoMutations(t, live)
}

func TestCanonicalLinkedRunningDrainReleasesClaimWithoutDuplicate(t *testing.T) {
	svc, _ := newMinimalService(t)
	live := newRouterSession()
	useRouterSession(svc, live)
	ctx := context.Background()
	workers := installWorkerFactory(svc, func(index int, worker *routerSession) {
		if index == 0 {
			worker.forkIDs = []string{"linked-1"}
		}
	})
	project, parent := seedCanonicalLinkedThreadForDrainTest(t, svc, ctx, false)
	if err := svc.store.EnqueuePendingCommand(ctx, model.PendingCommand{
		ThreadID: "linked-1", SourceThreadID: parent.ID, SourceTurnID: "source-turn-1",
		ProjectID: project.ID, ChatID: 7, TopicID: 0, Prompt: "oldest blocked",
	}); err != nil {
		t.Fatal(err)
	}
	oldestID := claimPendingPromptIDForTest(t, svc, ctx, "linked-1", "oldest blocked")
	if err := svc.store.EnqueuePendingCommand(ctx, model.PendingCommand{
		ThreadID: "linked-1", SourceThreadID: parent.ID, SourceTurnID: "source-turn-1",
		ProjectID: project.ID, ChatID: 7, TopicID: 0, Prompt: "second stays behind",
	}); err != nil {
		t.Fatal(err)
	}

	if err := svc.minimalRouter.DrainNext(ctx, "linked-1"); err != nil {
		t.Fatal(err)
	}

	if got := workers.Single(t).callSequence(); !reflect.DeepEqual(got, []string{"thread/fork:source-1:source-turn-1", "thread/name/set:linked-1", "turn/start:linked-1"}) {
		t.Fatalf("worker calls=%#v, want no queued turn/start or duplicate enqueue", got)
	}
	first, err := svc.store.ClaimPendingCommand(ctx, "linked-1")
	if err != nil || first == nil {
		t.Fatalf("first pending=%#v err=%v", first, err)
	}
	if first.ID != oldestID || first.Prompt != "oldest blocked" {
		t.Fatalf("first pending=%#v, want original oldest id %d prompt retained", first, oldestID)
	}
	if err := svc.store.ReleaseClaimedPendingCommand(ctx, first.ID); err != nil {
		t.Fatal(err)
	}
	again, err := svc.store.ClaimPendingCommand(ctx, "linked-1")
	if err != nil || again == nil {
		t.Fatalf("again pending=%#v err=%v", again, err)
	}
	if again.ID != oldestID || again.Prompt != "oldest blocked" {
		t.Fatalf("again pending=%#v, want same released oldest id %d", again, oldestID)
	}
	if err := svc.store.CompletePendingCommand(ctx, again.ID); err != nil {
		t.Fatal(err)
	}
	second, err := svc.store.ClaimPendingCommand(ctx, "linked-1")
	if err != nil || second == nil || second.Prompt != "second stays behind" || second.ID == oldestID {
		t.Fatalf("second pending=%#v err=%v, want original second prompt only", second, err)
	}
	if err := svc.store.CompletePendingCommand(ctx, second.ID); err != nil {
		t.Fatal(err)
	}
	if next, err := svc.store.ClaimPendingCommand(ctx, "linked-1"); err != nil || next != nil {
		t.Fatalf("extra pending after two originals=%#v err=%v", next, err)
	}
	assertRouterSessionNoMutations(t, live)
}

func TestCanonicalLinkedReadyDrainAcquireBusyReleasesClaimWithoutPromptLoss(t *testing.T) {
	svc, _ := newMinimalService(t)
	live := newRouterSession()
	useRouterSession(svc, live)
	ctx := context.Background()
	workers := installWorkerFactory(svc, func(index int, worker *routerSession) {
		if index == 0 {
			worker.forkIDs = []string{"linked-1"}
		}
	})
	project, parent := seedCanonicalLinkedThreadForDrainTest(t, svc, ctx, true)
	if err := svc.store.EnqueuePendingCommand(ctx, model.PendingCommand{
		ThreadID: "linked-1", SourceThreadID: parent.ID, SourceTurnID: "source-turn-1",
		ProjectID: project.ID, ChatID: 7, TopicID: 0, Prompt: "retry after acquire busy",
	}); err != nil {
		t.Fatal(err)
	}
	pendingID := claimPendingPromptIDForTest(t, svc, ctx, "linked-1", "retry after acquire busy")
	blocker, err := svc.minimalWorkers.Acquire(ctx, "external-blocker", "linked-1")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = svc.minimalRouter.releaseMinimalWorker(ctx, blocker)
	})

	if err := svc.minimalRouter.DrainNext(ctx, "linked-1"); err != nil {
		t.Fatal(err)
	}

	if len(workers.Sessions()) != 2 {
		t.Fatalf("worker sessions=%d, want only initial link worker plus external blocker", len(workers.Sessions()))
	}
	if got := workers.Session(t, 1).callSequence(); len(got) != 0 {
		t.Fatalf("busy blocker calls=%#v, want no remote resume/turn/start", got)
	}
	retry, err := svc.store.ClaimPendingCommand(ctx, "linked-1")
	if err != nil || retry == nil {
		t.Fatalf("retry pending=%#v err=%v", retry, err)
	}
	if retry.ID != pendingID || retry.Prompt != "retry after acquire busy" {
		t.Fatalf("retry pending=%#v, want original pending id %d prompt retained", retry, pendingID)
	}
	if err := svc.store.CompletePendingCommand(ctx, retry.ID); err != nil {
		t.Fatal(err)
	}
	if next, err := svc.store.ClaimPendingCommand(ctx, "linked-1"); err != nil || next != nil {
		t.Fatalf("extra pending after retry=%#v err=%v", next, err)
	}
	assertRouterSessionNoMutations(t, live)
}

func TestCanonicalLinkedReadyDrainResumeFailureReleasesClaimWithoutPromptLoss(t *testing.T) {
	svc, _ := newMinimalService(t)
	live := newRouterSession()
	useRouterSession(svc, live)
	ctx := context.Background()
	workers := installWorkerFactory(svc, func(index int, worker *routerSession) {
		if index == 0 {
			worker.forkIDs = []string{"linked-1"}
			return
		}
		worker.resumeErrByThread = map[string]error{"linked-1": errors.New("transport down")}
	})
	project, parent := seedCanonicalLinkedThreadForDrainTest(t, svc, ctx, true)
	if err := svc.store.EnqueuePendingCommand(ctx, model.PendingCommand{
		ThreadID: "linked-1", SourceThreadID: parent.ID, SourceTurnID: "source-turn-1",
		ProjectID: project.ID, ChatID: 7, TopicID: 0, Prompt: "retry after resume failure",
	}); err != nil {
		t.Fatal(err)
	}
	pendingID := claimPendingPromptIDForTest(t, svc, ctx, "linked-1", "retry after resume failure")

	if err := svc.minimalRouter.DrainNext(ctx, "linked-1"); err != nil {
		t.Fatal(err)
	}

	worker := workers.Session(t, 1)
	if got := worker.callSequence(); !reflect.DeepEqual(got, []string{"thread/resume:linked-1"}) {
		t.Fatalf("resume failure worker calls=%#v, want no turn/start", got)
	}
	if !worker.Closed() {
		t.Fatal("resume failure worker was not closed")
	}
	retry, err := svc.store.ClaimPendingCommand(ctx, "linked-1")
	if err != nil || retry == nil {
		t.Fatalf("retry pending=%#v err=%v", retry, err)
	}
	if retry.ID != pendingID || retry.Prompt != "retry after resume failure" {
		t.Fatalf("retry pending=%#v, want original pending id %d prompt retained", retry, pendingID)
	}
	if err := svc.store.CompletePendingCommand(ctx, retry.ID); err != nil {
		t.Fatal(err)
	}
	if next, err := svc.store.ClaimPendingCommand(ctx, "linked-1"); err != nil || next != nil {
		t.Fatalf("extra pending after retry=%#v err=%v", next, err)
	}
	assertRouterSessionNoMutations(t, live)
}

func seedCanonicalLinkedThreadForDrainTest(t *testing.T, svc *Service, ctx context.Context, release bool) (model.Project, model.Thread) {
	t.Helper()
	project := svc.cfg.Projects[0]
	parent := model.Thread{ID: "source-1", CWD: project.CanonicalPath, Status: "completed", Title: "안클코"}
	if err := svc.store.UpsertThread(ctx, parent); err != nil {
		t.Fatal(err)
	}
	anchor := sourceTurnAnchor{ThreadID: parent.ID, TurnID: "source-turn-1", Status: "completed", PCOrigin: true}
	first, err := svc.minimalRouter.startResumedOrLinkedTurn(ctx, model.InboundText{ChatID: 7, Text: "first"}, &parent, project, anchor, "first")
	if err != nil || first.threadID() != "linked-1" {
		t.Fatalf("initial result=%#v err=%v", first, err)
	}
	if release {
		releaseLinkedWorkerForTest(t, svc, 7, 0, parent.ID)
	}
	if err := svc.store.UpsertThread(ctx, model.Thread{ID: "linked-1", CWD: project.CanonicalPath, Status: "completed"}); err != nil {
		t.Fatal(err)
	}
	return project, parent
}

func claimPendingPromptIDForTest(t *testing.T, svc *Service, ctx context.Context, threadID, prompt string) int64 {
	t.Helper()
	claimed, err := svc.store.ClaimPendingCommand(ctx, threadID)
	if err != nil || claimed == nil || claimed.Prompt != prompt {
		t.Fatalf("claimed pending=%#v err=%v, want prompt %q", claimed, err, prompt)
	}
	if err := svc.store.ReleaseClaimedPendingCommand(ctx, claimed.ID); err != nil {
		t.Fatal(err)
	}
	return claimed.ID
}

func TestLinkedFirstCreationStartFailureMarksFailedAndReleasesWorker(t *testing.T) {
	svc, _ := newMinimalService(t)
	live := newRouterSession()
	useRouterSession(svc, live)
	ctx := context.Background()
	workers := installWorkerFactory(svc, func(index int, worker *routerSession) {
		worker.forkIDs = []string{"linked-1"}
		worker.fail["first"] = errors.New("turn start failed")
	})
	project := svc.cfg.Projects[0]
	parent := model.Thread{ID: "source-1", CWD: project.CanonicalPath, Status: "completed", Title: "안클코"}
	if err := svc.store.UpsertThread(ctx, parent); err != nil {
		t.Fatal(err)
	}
	anchor := sourceTurnAnchor{ThreadID: parent.ID, TurnID: "source-turn-1", Status: "completed", PCOrigin: true}

	result, err := svc.minimalRouter.startResumedOrLinkedTurn(ctx, model.InboundText{ChatID: 7, Text: "first"}, &parent, project, anchor, "first")

	if err == nil || strings.TrimSpace(result.turnID) != "" {
		t.Fatalf("result=%#v err=%v, want pre-turn start failure", result, err)
	}
	worker := workers.Single(t)
	if !worker.Closed() {
		t.Fatal("worker was not released after turn/start failed before a remote turn id")
	}
	if _, ok := svc.minimalWorkers.ByThread("linked-1"); ok {
		t.Fatal("failed start worker is still registered")
	}
	link, linkErr := svc.store.GetMinimalLinkedThread(ctx, 7, 0, parent.ID)
	if linkErr != nil {
		t.Fatal(linkErr)
	}
	if link == nil || link.State != model.MinimalLinkedFailed || link.FailureKind != model.MinimalContinuationFailureAmbiguous {
		t.Fatalf("link=%#v, want failed ambiguous", link)
	}
	assertRouterSessionNoMutations(t, live)
}

func TestDirectReplyForksAndStartsChildWithoutResume(t *testing.T) {
	svc, _ := newMinimalService(t)
	app := newRouterSession()
	useRouterSession(svc, app)
	parent := model.Thread{ID: "parent", CWD: svc.cfg.Projects[0].CanonicalPath, Status: "completed"}
	if err := svc.store.UpsertThread(context.Background(), parent); err != nil {
		t.Fatal(err)
	}
	if err := svc.store.PutMessageRoute(context.Background(), model.MessageRoute{ChatID: 100, MessageID: 501, ThreadID: parent.ID, TurnID: "turn-2", CreatedAt: model.NowString()}); err != nil {
		t.Fatal(err)
	}
	app.threadReads = map[string]map[string]any{parent.ID: {"thread": map[string]any{
		"id": parent.ID, "cwd": parent.CWD, "status": "completed", "source": "vscode", "originator": "Codex Desktop",
		"turns": []any{map[string]any{"id": "turn-2", "status": "completed"}},
	}}}
	app.forkIDs = []string{"child"}

	response := submitMinimal(t, svc, model.InboundText{ChatID: 100, UserID: 7, ReplyToMessageID: 501, Text: "continue", ReceivedAt: svc.now()})

	if response.ThreadID != "child" || !strings.Contains(response.Text, "새 대화") {
		t.Fatalf("response=%#v", response)
	}
	if got := app.callSequence(); !reflect.DeepEqual(got, []string{"thread/fork:parent:turn-2", "turn/start:child"}) {
		t.Fatalf("sequence=%v", got)
	}
	calls := app.turnCalls()
	if len(calls) != 1 {
		t.Fatalf("turn calls=%#v", calls)
	}
	if calls[0].approval != "on-request" || calls[0].sandbox != "workspaceWrite" || calls[0].network {
		t.Fatalf("unpinned child policy: %#v", calls[0])
	}
	if !reflect.DeepEqual(calls[0].roots, []string{svc.cfg.Projects[0].CanonicalPath}) {
		t.Fatalf("child roots=%v", calls[0].roots)
	}
}

func TestTelegramOriginCompletedReplyResumesParentWithoutFork(t *testing.T) {
	svc, _ := newMinimalService(t)
	app := newRouterSession()
	useRouterSession(svc, app)
	parent := model.Thread{ID: "parent", CWD: svc.cfg.Projects[0].CanonicalPath, Status: "completed"}
	if err := svc.store.UpsertThread(context.Background(), parent); err != nil {
		t.Fatal(err)
	}
	if err := svc.markTelegramOriginTurn(context.Background(), parent.ID, "turn-2"); err != nil {
		t.Fatal(err)
	}
	if err := svc.store.PutMessageRoute(context.Background(), model.MessageRoute{ChatID: 100, MessageID: 501, ThreadID: parent.ID, TurnID: "turn-2", CreatedAt: model.NowString()}); err != nil {
		t.Fatal(err)
	}
	app.threadReads = map[string]map[string]any{parent.ID: {"thread": map[string]any{
		"id": parent.ID, "cwd": parent.CWD, "status": "completed",
		"turns": []any{map[string]any{"id": "turn-2", "status": "completed"}},
	}}}
	app.forkIDs = []string{"must-not-fork"}

	response := submitMinimal(t, svc, model.InboundText{ChatID: 100, UserID: 7, ReplyToMessageID: 501, Text: "continue", ReceivedAt: svc.now()})

	if response.ThreadID != parent.ID || strings.Contains(response.Text, "새 대화") {
		t.Fatalf("response=%#v", response)
	}
	if got := app.callSequence(); !reflect.DeepEqual(got, []string{"thread/resume:parent", "turn/start:parent"}) {
		t.Fatalf("sequence=%v", got)
	}
	if got := app.forkCallCount(); got != 0 {
		t.Fatalf("fork calls=%d, want none", got)
	}
}

func TestPCOriginThreadMarkerOverridesPollutedTelegramOriginTurn(t *testing.T) {
	svc, _ := newMinimalService(t)
	app := newRouterSession()
	useRouterSession(svc, app)
	parent := model.Thread{ID: "parent", CWD: svc.cfg.Projects[0].CanonicalPath, Status: "completed"}
	if err := svc.store.UpsertThread(context.Background(), parent); err != nil {
		t.Fatal(err)
	}
	if err := svc.store.SetState(context.Background(), minimalPCOriginThreadStatePrefix+parent.ID, "1"); err != nil {
		t.Fatal(err)
	}
	if err := svc.markTelegramOriginTurn(context.Background(), parent.ID, "turn-2"); err != nil {
		t.Fatal(err)
	}
	if err := svc.store.PutMessageRoute(context.Background(), model.MessageRoute{ChatID: 100, MessageID: 501, ThreadID: parent.ID, TurnID: "turn-2", CreatedAt: model.NowString()}); err != nil {
		t.Fatal(err)
	}
	app.threadReads = map[string]map[string]any{parent.ID: {"thread": map[string]any{
		"id": parent.ID, "cwd": parent.CWD, "status": "completed",
		"turns": []any{map[string]any{"id": "turn-2", "status": "completed"}},
	}}}
	app.forkIDs = []string{"child"}

	response := submitMinimal(t, svc, model.InboundText{ChatID: 100, UserID: 7, ReplyToMessageID: 501, Text: "continue", ReceivedAt: svc.now()})

	if response.ThreadID != "child" || !strings.Contains(response.Text, "새 대화") {
		t.Fatalf("response=%#v", response)
	}
	if got := app.callSequence(); !reflect.DeepEqual(got, []string{"thread/fork:parent:turn-2", "turn/start:child"}) {
		t.Fatalf("sequence=%v", got)
	}
}

func TestDirectReplyForkUsesKoreanNoticeAndShortIDs(t *testing.T) {
	svc, _ := newMinimalService(t)
	app := newRouterSession()
	useRouterSession(svc, app)
	parent := model.Thread{ID: "parent-12345678", CWD: svc.cfg.Projects[0].CanonicalPath, Status: "completed"}
	if err := svc.store.UpsertThread(context.Background(), parent); err != nil {
		t.Fatal(err)
	}
	if err := svc.store.PutMessageRoute(context.Background(), model.MessageRoute{ChatID: 100, MessageID: 501, ThreadID: parent.ID, TurnID: "turn-2", CreatedAt: model.NowString()}); err != nil {
		t.Fatal(err)
	}
	app.threadReads = map[string]map[string]any{parent.ID: {"thread": map[string]any{
		"id": parent.ID, "cwd": parent.CWD, "status": "completed",
		"turns": []any{map[string]any{"id": "turn-2", "status": "completed"}},
	}}}
	app.resumeErrByThread = map[string]error{parent.ID: &appserver.RPCError{Code: -32600, Message: "thread " + parent.ID + " already has an active writer"}}
	app.forkIDs = []string{"child-87654321"}

	response := submitMinimal(t, svc, model.InboundText{ChatID: 100, UserID: 7, ReplyToMessageID: 501, Text: "continue", ReceivedAt: svc.now()})

	want := "PC Codex가 원본 대화를 사용 중이어서, 답변 시점의 문맥을 이어받은 새 대화에서 작업을 시작했습니다.\n\n원본: parent-1 · 이어받은 대화: child-87"
	if response.Text != want {
		t.Fatalf("response text = %q, want %q", response.Text, want)
	}
	if strings.Contains(response.Text, parent.ID) || strings.Contains(response.Text, "child-87654321") || strings.Contains(response.Text, parent.CWD) {
		t.Fatalf("response leaked full identity/path: %q", response.Text)
	}
}

func TestFreshContinuationForkLocksChildBeforeFirstStart(t *testing.T) {
	svc, _ := newMinimalService(t)
	app := newRouterSession()
	useRouterSession(svc, app)
	project := svc.cfg.Projects[0]
	parent := model.Thread{ID: "parent", CWD: project.CanonicalPath, Status: "completed"}
	if err := svc.store.UpsertThread(context.Background(), parent); err != nil {
		t.Fatal(err)
	}
	if err := svc.store.PutMessageRoute(context.Background(), model.MessageRoute{ChatID: 100, MessageID: 501, ThreadID: parent.ID, TurnID: "turn-2", CreatedAt: model.NowString()}); err != nil {
		t.Fatal(err)
	}
	if err := svc.store.SetSelectedProject(context.Background(), 100, 0, project.ID); err != nil {
		t.Fatal(err)
	}
	app.threadReads = map[string]map[string]any{
		parent.ID: {"thread": map[string]any{
			"id": parent.ID, "cwd": parent.CWD, "status": "completed",
			"turns": []any{map[string]any{"id": "turn-2", "status": "completed"}},
		}},
		"child": {"thread": map[string]any{
			"id": "child", "cwd": project.CanonicalPath, "status": "completed",
			"turns": []any{map[string]any{"id": "turn-2", "status": "completed"}},
		}},
	}
	app.resumeErrByThread = map[string]error{"parent": &appserver.RPCError{Code: -32600, Message: "thread parent already has an active writer"}}
	app.forkIDs = []string{"child"}

	activationSeen := make(chan struct{})
	releaseForkFlow := make(chan struct{})
	var once sync.Once
	app.setNameHook = func(threadID, _ string) error {
		if threadID != "child" {
			return nil
		}
		once.Do(func() { close(activationSeen) })
		select {
		case <-releaseForkFlow:
			return nil
		case <-time.After(2 * time.Second):
			return errors.New("timed out waiting to release fresh fork naming barrier")
		}
	}
	t.Cleanup(func() { once.Do(func() { close(activationSeen) }) })

	parentDone := make(chan continuationSubmitResult, 1)
	go func() {
		response, err := svc.HandleInboundText(context.Background(), model.InboundText{ChatID: 100, UserID: 7, ReplyToMessageID: 501, Text: "intended first", ReceivedAt: svc.now()})
		parentDone <- continuationSubmitResult{response: response, err: err}
	}()

	select {
	case <-activationSeen:
	case result := <-parentDone:
		t.Fatalf("fresh fork finished before activation barrier: response=%#v err=%v", result.response, result.err)
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for fresh fork activation")
	}

	concurrentDone := make(chan continuationSubmitResult, 1)
	go func() {
		response, err := svc.HandleInboundText(context.Background(), model.InboundText{ChatID: 100, UserID: 7, Text: "concurrent bound", ReceivedAt: svc.now()})
		concurrentDone <- continuationSubmitResult{response: response, err: err}
	}()
	assertSubmitNotDone(t, concurrentDone, "concurrent bound submit escaped the fresh child lock before first start")

	close(releaseForkFlow)
	parentResult := waitSubmitResult(t, parentDone, "fresh fork parent submit")
	if parentResult.err != nil {
		t.Fatalf("fresh fork parent submit failed: %v", parentResult.err)
	}
	concurrentResult := waitSubmitResult(t, concurrentDone, "concurrent bound submit")
	if concurrentResult.err != nil {
		t.Fatalf("concurrent bound submit failed: %v", concurrentResult.err)
	}
	if parentResult.response == nil || parentResult.response.ThreadID != "child" {
		t.Fatalf("parent response=%#v", parentResult.response)
	}
	if concurrentResult.response == nil || concurrentResult.response.ThreadID != "child" || !strings.Contains(concurrentResult.response.Text, "대기열") {
		t.Fatalf("concurrent response=%#v", concurrentResult.response)
	}
	calls := app.turnCalls()
	if len(calls) != 1 || calls[0].threadID != "child" || calls[0].message != "intended first" {
		t.Fatalf("turn calls=%#v, want only intended first start", calls)
	}
	queued, err := svc.store.ClaimPendingCommand(context.Background(), "child")
	if err != nil {
		t.Fatal(err)
	}
	if queued == nil || queued.Prompt != "concurrent bound" {
		t.Fatalf("queued command=%#v, want concurrent bound", queued)
	}
}

func TestRepeatedParentReplyReusesContinuation(t *testing.T) {
	svc, _ := newMinimalService(t)
	app := newRouterSession()
	useRouterSession(svc, app)
	parent := model.Thread{ID: "parent", CWD: svc.cfg.Projects[0].CanonicalPath, Status: "completed"}
	if err := svc.store.UpsertThread(context.Background(), parent); err != nil {
		t.Fatal(err)
	}
	if err := svc.store.PutMessageRoute(context.Background(), model.MessageRoute{ChatID: 100, MessageID: 501, ThreadID: parent.ID, TurnID: "turn-2", CreatedAt: model.NowString()}); err != nil {
		t.Fatal(err)
	}
	app.threadReads = map[string]map[string]any{parent.ID: {"thread": map[string]any{
		"id": parent.ID, "cwd": parent.CWD, "status": "completed",
		"turns": []any{map[string]any{"id": "turn-2", "status": "completed"}},
	}}}
	app.resumeErrByThread = map[string]error{"parent": &appserver.RPCError{Code: -32600, Message: "thread parent already has an active writer"}}
	app.forkIDs = []string{"child"}
	submitMinimal(t, svc, model.InboundText{ChatID: 100, UserID: 7, ReplyToMessageID: 501, Text: "first", ReceivedAt: svc.now()})
	child, err := svc.store.GetThread(context.Background(), "child")
	if err != nil || child == nil {
		t.Fatalf("child=%#v err=%v", child, err)
	}
	child.Status, child.ActiveTurnID = "completed", ""
	if err := svc.store.UpsertThread(context.Background(), *child); err != nil {
		t.Fatal(err)
	}
	releaseLinkedWorkerForTest(t, svc, 100, 0, parent.ID)
	app.threadReads["child"] = map[string]any{"thread": map[string]any{
		"id": "child", "cwd": parent.CWD, "status": "completed",
		"turns": []any{map[string]any{"id": "child-turn-1", "status": "completed"}},
	}}

	submitMinimal(t, svc, model.InboundText{ChatID: 100, UserID: 7, ReplyToMessageID: 501, Text: "second", ReceivedAt: svc.now()})

	if got := app.forkCallCount(); got != 1 {
		t.Fatalf("fork calls=%d", got)
	}
	calls := app.turnCalls()
	if len(calls) != 2 || calls[0].threadID != "child" || calls[1].threadID != "child" {
		t.Fatalf("turn calls=%#v", calls)
	}
}

func TestOlderCompletedReplyForksExactTurnWhileNewerParentTurnRuns(t *testing.T) {
	svc, _ := newMinimalService(t)
	app := newRouterSession()
	useRouterSession(svc, app)
	parent := model.Thread{ID: "parent", CWD: svc.cfg.Projects[0].CanonicalPath, Status: "inProgress", ActiveTurnID: "turn-3"}
	if err := svc.store.UpsertThread(context.Background(), parent); err != nil {
		t.Fatal(err)
	}
	if err := svc.store.PutMessageRoute(context.Background(), model.MessageRoute{ChatID: 100, MessageID: 501, ThreadID: parent.ID, TurnID: "turn-2", CreatedAt: model.NowString()}); err != nil {
		t.Fatal(err)
	}
	app.threadReads = map[string]map[string]any{parent.ID: {"thread": map[string]any{
		"id": parent.ID, "cwd": parent.CWD, "status": "active", "source": "vscode", "originator": "Codex Desktop",
		"turns": []any{
			map[string]any{"id": "turn-2", "status": "completed"},
			map[string]any{"id": "turn-3", "status": "inProgress"},
		},
	}}}
	app.forkIDs = []string{"child-from-turn-2"}

	response := submitMinimal(t, svc, model.InboundText{ChatID: 100, UserID: 7, ReplyToMessageID: 501, Text: "use older answer", ReceivedAt: svc.now()})

	if response.ThreadID != "child-from-turn-2" || app.forkCalls[0].options.LastTurnID != "turn-2" {
		t.Fatalf("response=%#v forks=%#v", response, app.forkCalls)
	}
	if got := app.callSequence(); !reflect.DeepEqual(got, []string{"thread/fork:parent:turn-2", "turn/start:child-from-turn-2"}) {
		t.Fatalf("sequence=%v", got)
	}
}

func TestOlderCompletedReplyReusesContinuationWithNormalStartText(t *testing.T) {
	svc, _ := newMinimalService(t)
	logs := captureServiceLogs(svc)
	app := newRouterSession()
	useRouterSession(svc, app)
	parent := model.Thread{ID: "parent-12345678", CWD: svc.cfg.Projects[0].CanonicalPath, Status: "inProgress", ActiveTurnID: "turn-3"}
	if err := svc.store.UpsertThread(context.Background(), parent); err != nil {
		t.Fatal(err)
	}
	if err := svc.store.PutMessageRoute(context.Background(), model.MessageRoute{ChatID: 100, MessageID: 501, ThreadID: parent.ID, TurnID: "turn-2", CreatedAt: model.NowString()}); err != nil {
		t.Fatal(err)
	}
	app.threadReads = map[string]map[string]any{parent.ID: {"thread": map[string]any{
		"id": parent.ID, "cwd": parent.CWD, "status": "active", "activeTurnId": "turn-3",
		"turns": []any{
			map[string]any{"id": "turn-2", "status": "completed"},
			map[string]any{"id": "turn-3", "status": "inProgress"},
		},
	}}}
	app.resumeErrByThread = map[string]error{parent.ID: &appserver.RPCError{Code: -32600, Message: "thread " + parent.ID + " already has an active writer"}}
	app.forkIDs = []string{"child-87654321"}
	first := submitMinimal(t, svc, model.InboundText{ChatID: 100, UserID: 7, ReplyToMessageID: 501, Text: "first", ReceivedAt: svc.now()})
	if first.ThreadID != "child-87654321" || !strings.Contains(first.Text, "새 대화") {
		t.Fatalf("first response=%#v", first)
	}
	child := model.Thread{ID: "child-87654321", CWD: parent.CWD, Status: "completed"}
	if err := svc.store.UpsertThread(context.Background(), child); err != nil {
		t.Fatal(err)
	}
	releaseLinkedWorkerForTest(t, svc, 100, 0, parent.ID)
	app.threadReads[child.ID] = map[string]any{"thread": map[string]any{
		"id": child.ID, "cwd": child.CWD, "status": "completed",
		"turns": []any{map[string]any{"id": "turn-1", "status": "completed"}},
	}}

	second := submitMinimal(t, svc, model.InboundText{ChatID: 100, UserID: 7, ReplyToMessageID: 501, Text: "second", ReceivedAt: svc.now()})

	if second.ThreadID != child.ID || second.Text != "작업을 시작했습니다." {
		t.Fatalf("second response=%#v, want normal reused start text", second)
	}
	if strings.Contains(second.Text, "새 대화") || strings.Contains(second.Text, "이어받은 대화") {
		t.Fatalf("reused continuation showed first-fork notice: %q", second.Text)
	}
	if got := app.forkCallCount(); got != 1 {
		t.Fatalf("fork calls=%d, want reused existing child", got)
	}
	logText := logs.String()
	if !strings.Contains(logText, `"event":"minimal_continuation_fork_reused"`) || !strings.Contains(logText, `"source_status":"completed"`) {
		t.Fatalf("reused continuation log missing sanitized source status:\n%s", logText)
	}
	if strings.Contains(logText, "chat_key") || strings.Contains(logText, model.ChatKey(100, 0)) || !strings.Contains(logText, "chat_hash") {
		t.Fatalf("reused continuation log leaked raw chat identity or omitted hash:\n%s", logText)
	}
	calls := app.turnCalls()
	if len(calls) != 2 || calls[1].threadID != child.ID || calls[1].message != "second" {
		t.Fatalf("turn calls=%#v", calls)
	}
}

func TestActiveWriterContinuationDoesNotForkForNonConflictsOrStartFailure(t *testing.T) {
	cases := []struct {
		name      string
		resumeErr error
		startFail bool
	}{
		{name: "timeout", resumeErr: context.DeadlineExceeded},
		{name: "wrong rpc code", resumeErr: &appserver.RPCError{Code: -32602, Message: "thread parent already has an active writer"}},
		{name: "turn start error", startFail: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			svc, _ := newMinimalService(t)
			app := newRouterSession()
			useRouterSession(svc, app)
			parent := model.Thread{ID: "parent", CWD: svc.cfg.Projects[0].CanonicalPath, Status: "completed"}
			if err := svc.store.UpsertThread(context.Background(), parent); err != nil {
				t.Fatal(err)
			}
			if err := svc.store.PutMessageRoute(context.Background(), model.MessageRoute{ChatID: 100, MessageID: 501, ThreadID: parent.ID, TurnID: "turn-2", CreatedAt: model.NowString()}); err != nil {
				t.Fatal(err)
			}
			app.threadReads = map[string]map[string]any{parent.ID: {"thread": map[string]any{
				"id": parent.ID, "cwd": parent.CWD, "status": "completed",
				"turns": []any{map[string]any{"id": "turn-2", "status": "completed"}},
			}}}
			if tc.resumeErr != nil {
				app.resumeErrByThread = map[string]error{"parent": tc.resumeErr}
			}
			if tc.startFail {
				app.fail["no fork"] = errors.New("turn start failed")
			}

			_, err := svc.HandleInboundText(context.Background(), model.InboundText{ChatID: 100, UserID: 7, ReplyToMessageID: 501, Text: "no fork", ReceivedAt: svc.now()})

			if err == nil {
				t.Fatal("HandleInboundText succeeded, want start/resume error")
			}
			if got := app.forkCallCount(); got != 0 {
				t.Fatalf("fork calls=%d, want 0", got)
			}
		})
	}
}

func TestTelegramRepairRearmsOnlyAmbiguousContinuationClaims(t *testing.T) {
	svc, _ := newMinimalService(t)
	ctx := context.Background()
	active, _, err := svc.store.ClaimMinimalContinuation(ctx, model.MinimalContinuation{
		Key: model.MinimalContinuationKey{ChatID: 7, SourceThreadID: "parent-b", SourceTurnID: "turn-b"}, ProjectID: "bridge",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.store.ActivateMinimalContinuation(ctx, *active, model.Thread{ID: "child-b", CWD: svc.cfg.Projects[0].CanonicalPath}); err != nil {
		t.Fatal(err)
	}
	ambiguous, _, err := svc.store.ClaimMinimalContinuation(ctx, model.MinimalContinuation{
		Key: model.MinimalContinuationKey{ChatID: 7, SourceThreadID: "parent-a", SourceTurnID: "turn-a"}, ProjectID: "bridge",
	})
	if err != nil {
		t.Fatal(err)
	}
	otherChat, _, err := svc.store.ClaimMinimalContinuation(ctx, model.MinimalContinuation{
		Key: model.MinimalContinuationKey{ChatID: 8, SourceThreadID: "parent-other", SourceTurnID: "turn-other"}, ProjectID: "bridge",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.store.RecoverCreatingMinimalContinuations(ctx); err != nil {
		t.Fatal(err)
	}

	response, err := svc.HandleInboundText(ctx, model.InboundText{ChatID: 7, UserID: 7, Text: "/repair", ReceivedAt: svc.now()})

	if err != nil {
		t.Fatal(err)
	}
	if response == nil || !strings.Contains(response.Text, "Repair requested") {
		t.Fatalf("repair response=%#v", response)
	}
	repaired, _ := svc.store.GetMinimalContinuation(ctx, ambiguous.Key)
	preserved, _ := svc.store.GetMinimalContinuation(ctx, active.Key)
	other, _ := svc.store.GetMinimalContinuation(ctx, otherChat.Key)
	if repaired.FailureKind != model.MinimalContinuationFailureDefinite ||
		preserved.Status != model.MinimalContinuationActive ||
		preserved.ForkThreadID != "child-b" ||
		other.FailureKind != model.MinimalContinuationFailureAmbiguous {
		t.Fatalf("repaired=%#v preserved=%#v other=%#v", repaired, preserved, other)
	}
}

func TestActiveWriterForkTransportErrorIsAmbiguousAndNotReclaimed(t *testing.T) {
	svc, _ := newMinimalService(t)
	logs := captureServiceLogs(svc)
	app := newRouterSession()
	useRouterSession(svc, app)
	parent := model.Thread{ID: "parent", CWD: svc.cfg.Projects[0].CanonicalPath, Status: "completed"}
	if err := svc.store.UpsertThread(context.Background(), parent); err != nil {
		t.Fatal(err)
	}
	if err := svc.store.PutMessageRoute(context.Background(), model.MessageRoute{ChatID: 100, MessageID: 501, ThreadID: parent.ID, TurnID: "turn-2", CreatedAt: model.NowString()}); err != nil {
		t.Fatal(err)
	}
	app.threadReads = map[string]map[string]any{parent.ID: {"thread": map[string]any{
		"id": parent.ID, "cwd": parent.CWD, "status": "completed",
		"turns": []any{map[string]any{"id": "turn-2", "status": "completed"}},
	}}}
	app.resumeErrByThread = map[string]error{"parent": &appserver.RPCError{Code: -32600, Message: "thread parent already has an active writer"}}
	app.forkErrs = []error{errors.New("write tcp 127.0.0.1: transport closed")}

	response, err := svc.HandleInboundText(context.Background(), model.InboundText{ChatID: 100, UserID: 7, ReplyToMessageID: 501, Text: "first", ReceivedAt: svc.now()})
	if err != nil || response == nil || response.Text != continuationForkFailureResponseText() {
		t.Fatalf("HandleInboundText response=%#v err=%v, want Korean fork failure response", response, err)
	}
	key := model.MinimalContinuationKey{ChatID: 100, SourceThreadID: "parent", SourceTurnID: "turn-2"}
	continuation, loadErr := svc.store.GetMinimalContinuation(context.Background(), key)
	if loadErr != nil {
		t.Fatal(loadErr)
	}
	if continuation == nil || continuation.Status != model.MinimalContinuationFailed || continuation.FailureKind != model.MinimalContinuationFailureAmbiguous {
		t.Fatalf("continuation=%#v, want failed/ambiguous", continuation)
	}
	logText := logs.String()
	if !strings.Contains(logText, `"status":"failed"`) || !strings.Contains(logText, `"failure_kind":"ambiguous"`) {
		t.Fatalf("failure log did not include persisted failed/ambiguous state:\n%s", logText)
	}

	response, err = svc.HandleInboundText(context.Background(), model.InboundText{ChatID: 100, UserID: 7, ReplyToMessageID: 501, Text: "retry blocked", ReceivedAt: svc.now()})
	if err != nil || response == nil || response.Text != continuationForkFailureResponseText() {
		t.Fatalf("ambiguous continuation retry response=%#v err=%v, want sanitized repair guidance", response, err)
	}
	if got := app.forkCallCount(); got != 1 {
		t.Fatalf("fork calls=%d, want no reclaim/refork after ambiguous transport error", got)
	}
}

func TestContinuationForkFailureReturnsKoreanFailureWithoutLeakingError(t *testing.T) {
	svc, _ := newMinimalService(t)
	app := newRouterSession()
	useRouterSession(svc, app)
	parent := model.Thread{ID: "parent", CWD: svc.cfg.Projects[0].CanonicalPath, Status: "completed"}
	if err := svc.store.UpsertThread(context.Background(), parent); err != nil {
		t.Fatal(err)
	}
	if err := svc.store.PutMessageRoute(context.Background(), model.MessageRoute{ChatID: 100, MessageID: 501, ThreadID: parent.ID, TurnID: "turn-2", CreatedAt: model.NowString()}); err != nil {
		t.Fatal(err)
	}
	app.threadReads = map[string]map[string]any{parent.ID: {"thread": map[string]any{
		"id": parent.ID, "cwd": parent.CWD, "status": "completed",
		"turns": []any{map[string]any{"id": "turn-2", "status": "completed"}},
	}}}
	app.resumeErrByThread = map[string]error{"parent": &appserver.RPCError{Code: -32600, Message: "thread parent already has an active writer"}}
	app.forkErrs = []error{errors.New("raw fork marker C:\\Temp\\bridge-private\\credential abc")}

	response, err := svc.HandleInboundText(context.Background(), model.InboundText{ChatID: 100, UserID: 7, ReplyToMessageID: 501, Text: "first", ReceivedAt: svc.now()})

	if err != nil {
		t.Fatalf("HandleInboundText returned err=%v, want Korean DirectResponse", err)
	}
	want := "PC 대화를 직접 이어갈 수 없었고, 문맥을 이어받는 새 대화 생성도 실패했습니다. 다시 시도하거나 /status를 확인해주세요."
	if response == nil || response.Text != want {
		t.Fatalf("response=%#v, want %q", response, want)
	}
	if strings.Contains(response.Text, "C:\\") || strings.Contains(response.Text, "credential") || strings.Contains(response.Text, "raw fork marker") {
		t.Fatalf("response leaked raw fork error: %q", response.Text)
	}
}

func TestActiveWriterForkDefiniteRPCErrorCanBeReclaimed(t *testing.T) {
	svc, _ := newMinimalService(t)
	app := newRouterSession()
	useRouterSession(svc, app)
	parent := model.Thread{ID: "parent", CWD: svc.cfg.Projects[0].CanonicalPath, Status: "completed"}
	if err := svc.store.UpsertThread(context.Background(), parent); err != nil {
		t.Fatal(err)
	}
	if err := svc.store.PutMessageRoute(context.Background(), model.MessageRoute{ChatID: 100, MessageID: 501, ThreadID: parent.ID, TurnID: "turn-2", CreatedAt: model.NowString()}); err != nil {
		t.Fatal(err)
	}
	app.threadReads = map[string]map[string]any{parent.ID: {"thread": map[string]any{
		"id": parent.ID, "cwd": parent.CWD, "status": "completed",
		"turns": []any{map[string]any{"id": "turn-2", "status": "completed"}},
	}}}
	app.resumeErrByThread = map[string]error{"parent": &appserver.RPCError{Code: -32600, Message: "thread parent already has an active writer"}}
	app.forkErrs = []error{&appserver.RPCError{Code: -32602, Message: "lastTurnId is invalid"}}
	app.forkIDs = []string{"", "child-after-rpc-retry"}

	response, err := svc.HandleInboundText(context.Background(), model.InboundText{ChatID: 100, UserID: 7, ReplyToMessageID: 501, Text: "first", ReceivedAt: svc.now()})
	if err != nil || response == nil || response.Text != continuationForkFailureResponseText() {
		t.Fatalf("HandleInboundText response=%#v err=%v, want Korean fork failure response", response, err)
	}
	key := model.MinimalContinuationKey{ChatID: 100, SourceThreadID: "parent", SourceTurnID: "turn-2"}
	continuation, loadErr := svc.store.GetMinimalContinuation(context.Background(), key)
	if loadErr != nil {
		t.Fatal(loadErr)
	}
	if continuation == nil || continuation.Status != model.MinimalContinuationFailed || continuation.FailureKind != model.MinimalContinuationFailureDefinite {
		t.Fatalf("continuation=%#v, want failed/definite", continuation)
	}

	response = submitMinimal(t, svc, model.InboundText{ChatID: 100, UserID: 7, ReplyToMessageID: 501, Text: "retry", ReceivedAt: svc.now()})
	if response.ThreadID != "child-after-rpc-retry" {
		t.Fatalf("response=%#v, want reclaimed retry child", response)
	}
	if got := app.forkCallCount(); got != 2 {
		t.Fatalf("fork calls=%d, want retry after definite RPC rejection", got)
	}
}

func TestActiveWriterForkRPCServerErrorsAreAmbiguousAndNotReclaimed(t *testing.T) {
	for _, tc := range []struct {
		name string
		code int
	}{
		{name: "internal error", code: -32603},
		{name: "unknown application code", code: 42},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc, _ := newMinimalService(t)
			app := newRouterSession()
			useRouterSession(svc, app)
			parent := model.Thread{ID: "parent", CWD: svc.cfg.Projects[0].CanonicalPath, Status: "completed"}
			if err := svc.store.UpsertThread(context.Background(), parent); err != nil {
				t.Fatal(err)
			}
			if err := svc.store.PutMessageRoute(context.Background(), model.MessageRoute{ChatID: 100, MessageID: 501, ThreadID: parent.ID, TurnID: "turn-2", CreatedAt: model.NowString()}); err != nil {
				t.Fatal(err)
			}
			app.threadReads = map[string]map[string]any{parent.ID: {"thread": map[string]any{
				"id": parent.ID, "cwd": parent.CWD, "status": "completed",
				"turns": []any{map[string]any{"id": "turn-2", "status": "completed"}},
			}}}
			app.resumeErrByThread = map[string]error{"parent": &appserver.RPCError{Code: -32600, Message: "thread parent already has an active writer"}}
			app.forkErrs = []error{&appserver.RPCError{Code: tc.code, Message: "fork outcome is not known to be side-effect-free"}}
			app.forkIDs = []string{"", "duplicate-child"}

			response, err := svc.HandleInboundText(context.Background(), model.InboundText{ChatID: 100, UserID: 7, ReplyToMessageID: 501, Text: "first", ReceivedAt: svc.now()})
			if err != nil || response == nil || response.Text != continuationForkFailureResponseText() {
				t.Fatalf("HandleInboundText response=%#v err=%v, want Korean fork failure response", response, err)
			}
			key := model.MinimalContinuationKey{ChatID: 100, SourceThreadID: "parent", SourceTurnID: "turn-2"}
			continuation, loadErr := svc.store.GetMinimalContinuation(context.Background(), key)
			if loadErr != nil {
				t.Fatal(loadErr)
			}
			if continuation == nil || continuation.Status != model.MinimalContinuationFailed || continuation.FailureKind != model.MinimalContinuationFailureAmbiguous {
				t.Fatalf("continuation=%#v, want failed/ambiguous", continuation)
			}

			response, err = svc.HandleInboundText(context.Background(), model.InboundText{ChatID: 100, UserID: 7, ReplyToMessageID: 501, Text: "retry blocked", ReceivedAt: svc.now()})
			if err != nil || response == nil || response.Text != continuationForkFailureResponseText() {
				t.Fatalf("ambiguous RPC retry response=%#v err=%v, want sanitized repair guidance", response, err)
			}
			if got := app.forkCallCount(); got != 1 {
				t.Fatalf("fork calls=%d, want no reclaim/refork after ambiguous RPC error", got)
			}
		})
	}
}

func TestRepeatedParentReplyHoldsReusedChildLockThroughResume(t *testing.T) {
	svc, _ := newMinimalService(t)
	app := newRouterSession()
	useRouterSession(svc, app)
	parent := model.Thread{ID: "parent", CWD: svc.cfg.Projects[0].CanonicalPath, Status: "completed"}
	if err := svc.store.UpsertThread(context.Background(), parent); err != nil {
		t.Fatal(err)
	}
	if err := svc.store.PutMessageRoute(context.Background(), model.MessageRoute{ChatID: 100, MessageID: 501, ThreadID: parent.ID, TurnID: "turn-2", CreatedAt: model.NowString()}); err != nil {
		t.Fatal(err)
	}
	app.threadReads = map[string]map[string]any{parent.ID: {"thread": map[string]any{
		"id": parent.ID, "cwd": parent.CWD, "status": "completed",
		"turns": []any{map[string]any{"id": "turn-2", "status": "completed"}},
	}}}
	app.resumeErrByThread = map[string]error{"parent": &appserver.RPCError{Code: -32600, Message: "thread parent already has an active writer"}}
	app.forkIDs = []string{"child"}
	submitMinimal(t, svc, model.InboundText{ChatID: 100, UserID: 7, ReplyToMessageID: 501, Text: "first", ReceivedAt: svc.now()})
	child, err := svc.store.GetThread(context.Background(), "child")
	if err != nil || child == nil {
		t.Fatalf("child=%#v err=%v", child, err)
	}
	child.Status, child.ActiveTurnID = "completed", ""
	if err := svc.store.UpsertThread(context.Background(), *child); err != nil {
		t.Fatal(err)
	}
	app.threadReads["child"] = map[string]any{"thread": map[string]any{
		"id": "child", "cwd": parent.CWD, "status": "completed",
		"turns": []any{map[string]any{"id": "child-turn-1", "status": "completed"}},
	}}
	app.resumeHook = func(threadID string) error {
		if threadID != "child" {
			return nil
		}
		value, ok := svc.minimalRouter.locks.Load("child")
		if !ok {
			return errors.New("child lock missing during reused child resume")
		}
		lock := value.(*sync.Mutex)
		if lock.TryLock() {
			lock.Unlock()
			return errors.New("child lock was not held during reused child resume")
		}
		return nil
	}

	submitMinimal(t, svc, model.InboundText{ChatID: 100, UserID: 7, ReplyToMessageID: 501, Text: "second", ReceivedAt: svc.now()})
}

func TestActiveWriterContinuationRejectsMalformedOrOutOfRegistryForkResponse(t *testing.T) {
	for _, test := range []struct {
		name        string
		payloadCWD  string
		childID     string
		wantFailure string
	}{
		{name: "empty child", childID: "", wantFailure: model.MinimalContinuationFailureAmbiguous},
		{name: "parent child", childID: "parent", wantFailure: model.MinimalContinuationFailureAmbiguous},
		{name: "out of registry child", childID: "child", wantFailure: model.MinimalContinuationFailureAmbiguous},
	} {
		t.Run(test.name, func(t *testing.T) {
			svc, _ := newMinimalService(t)
			app := newRouterSession()
			useRouterSession(svc, app)
			if test.name == "out of registry child" {
				outside := filepath.Join(t.TempDir(), "outside")
				if err := os.MkdirAll(outside, 0o755); err != nil {
					t.Fatal(err)
				}
				app.forkPayloadCWD = outside
			}
			parent := model.Thread{ID: "parent", CWD: svc.cfg.Projects[0].CanonicalPath, Status: "completed"}
			if err := svc.store.UpsertThread(context.Background(), parent); err != nil {
				t.Fatal(err)
			}
			if err := svc.store.PutMessageRoute(context.Background(), model.MessageRoute{ChatID: 100, MessageID: 501, ThreadID: parent.ID, TurnID: "turn-2", CreatedAt: model.NowString()}); err != nil {
				t.Fatal(err)
			}
			app.threadReads = map[string]map[string]any{parent.ID: {"thread": map[string]any{
				"id": parent.ID, "cwd": parent.CWD, "status": "completed",
				"turns": []any{map[string]any{"id": "turn-2", "status": "completed"}},
			}}}
			app.resumeErrByThread = map[string]error{"parent": &appserver.RPCError{Code: -32600, Message: "thread parent already has an active writer"}}
			app.forkIDs = []string{test.childID}

			response, err := svc.HandleInboundText(context.Background(), model.InboundText{ChatID: 100, UserID: 7, ReplyToMessageID: 501, Text: "bad fork", ReceivedAt: svc.now()})

			if err != nil || response == nil || response.Text != continuationForkFailureResponseText() {
				t.Fatalf("HandleInboundText response=%#v err=%v, want sanitized fork failure", response, err)
			}
			if got := app.forkCallCount(); got != 1 {
				t.Fatalf("fork calls=%d, want 1", got)
			}
			key := model.MinimalContinuationKey{ChatID: 100, SourceThreadID: "parent", SourceTurnID: "turn-2"}
			continuation, loadErr := svc.store.GetMinimalContinuation(context.Background(), key)
			if loadErr != nil {
				t.Fatal(loadErr)
			}
			if continuation == nil || continuation.Status != model.MinimalContinuationFailed || continuation.FailureKind != test.wantFailure {
				t.Fatalf("continuation=%#v", continuation)
			}
			if calls := app.turnCalls(); len(calls) != 0 {
				t.Fatalf("turn calls=%#v, want none", calls)
			}
		})
	}
}

func seedPartialMinimalLinkedTitle(t *testing.T, svc *Service, sourceID, linkedID, desiredTitle string) model.Thread {
	t.Helper()
	ctx := context.Background()
	project := svc.cfg.Projects[0]
	source := model.Thread{ID: sourceID, Title: sourceID, CWD: project.CanonicalPath, Status: "completed"}
	if err := svc.store.UpsertThread(ctx, source); err != nil {
		t.Fatal(err)
	}
	claimed, created, err := svc.store.ClaimMinimalContinuation(ctx, model.MinimalContinuation{
		Key:       model.MinimalContinuationKey{ChatID: 7, SourceThreadID: sourceID, SourceTurnID: "source-turn-1"},
		ProjectID: project.ID,
	})
	if err != nil || !created {
		t.Fatalf("legacy claim created=%t err=%v", created, err)
	}
	if err := svc.store.ActivateMinimalContinuation(ctx, *claimed, model.Thread{ID: linkedID, CWD: project.CanonicalPath, Status: "completed"}); err != nil {
		t.Fatal(err)
	}
	adopted, err := svc.store.AdoptMinimalLinkedThreads(ctx)
	if err != nil || adopted != 1 {
		t.Fatalf("adopted=%d err=%v, want 1 nil", adopted, err)
	}
	changed, err := svc.store.HydrateMinimalLinkedTitles(ctx, linkedID, "seed-source-title", desiredTitle)
	if err != nil || !changed {
		t.Fatalf("seed title hydrate changed=%t err=%v, want true nil", changed, err)
	}
	db := openMinimalServiceDB(t, svc)
	if _, err := db.ExecContext(ctx, `
		UPDATE minimal_linked_threads
		SET source_title_payload = NULL
		WHERE linked_thread_id = ?`, linkedID); err != nil {
		t.Fatal(err)
	}
	return source
}

func minimalLinkedTitleThreadReads(sourceID, title, cwd string) map[string]map[string]any {
	return map[string]map[string]any{sourceID: {"thread": map[string]any{
		"id": sourceID, "title": title, "cwd": cwd, "status": "completed", "source": "vscode", "originator": "Codex Desktop",
		"turns": []any{map[string]any{"id": "source-turn-1", "status": "completed"}},
	}}}
}

func installMinimalLinkedTitleReloadFailure(t *testing.T, svc *Service, linkedID string) {
	t.Helper()
	db := openMinimalServiceDB(t, svc)
	if _, err := db.ExecContext(context.Background(), `
		CREATE TRIGGER corrupt_minimal_linked_title_reload
		AFTER UPDATE OF source_title_payload ON minimal_linked_threads
		WHEN NEW.linked_thread_id = '`+linkedID+`'
		BEGIN
			UPDATE minimal_linked_threads
			SET source_title_payload = 'not-a-protected-envelope'
			WHERE linked_thread_id = NEW.linked_thread_id;
		END`); err != nil {
		t.Fatal(err)
	}
}

func reopenMinimalServiceStore(t *testing.T, svc *Service, protector securestore.Protector) {
	t.Helper()
	if err := svc.store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err := storage.OpenWithProtector(svc.cfg.Paths.DBPath, protector)
	if err != nil {
		t.Fatal(err)
	}
	svc.store = store
}

func minimalLinkedTitleStateRaw(t *testing.T, svc *Service, linkedID string) string {
	t.Helper()
	db := openMinimalServiceDB(t, svc)
	var state string
	if err := db.QueryRowContext(context.Background(), `SELECT title_state FROM minimal_linked_threads WHERE linked_thread_id = ?`, linkedID).Scan(&state); err != nil {
		t.Fatal(err)
	}
	return state
}

func openMinimalServiceDB(t *testing.T, svc *Service) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", svc.cfg.Paths.DBPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

type protectFailingTestProtector struct {
	delegate securestore.Protector
	err      error
}

func (p protectFailingTestProtector) Protect(context.Context, []byte) (string, error) {
	return "", p.err
}

func (p protectFailingTestProtector) Unprotect(ctx context.Context, value string) ([]byte, error) {
	return p.delegate.Unprotect(ctx, value)
}

func waitSubmitResult(t *testing.T, done <-chan continuationSubmitResult, label string) continuationSubmitResult {
	t.Helper()
	select {
	case result := <-done:
		return result
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for %s", label)
		return continuationSubmitResult{}
	}
}

func assertSubmitNotDone(t *testing.T, done <-chan continuationSubmitResult, message string) {
	t.Helper()
	select {
	case result := <-done:
		t.Fatalf("%s: response=%#v err=%v", message, result.response, result.err)
	case <-time.After(100 * time.Millisecond):
	}
}
