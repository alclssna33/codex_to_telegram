package daemon

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alclssna33/codex_to_telegram/internal/appserver"
	"github.com/alclssna33/codex_to_telegram/internal/control"
	"github.com/alclssna33/codex_to_telegram/internal/model"
)

func TestMinimalRouterNewTextCreatesWorkerThreadWithPinnedPolicy(t *testing.T) {
	svc, _ := newMinimalService(t)
	app := newRouterSession("new-1")
	useRouterSession(svc, app)
	ctx := context.Background()
	mustSelect(t, svc, 100, "bridge")
	first := submitMinimal(t, svc, model.InboundText{ChatID: 100, UserID: 7, Text: "run tests", ReceivedAt: svc.now()})
	if first.ThreadID != "new-1" || first.Text == "" || first.TurnID == "" {
		t.Fatalf("response = %#v", first)
	}
	for _, call := range app.turnCalls() {
		if call.approval != "on-request" || call.sandbox != "workspaceWrite" || call.network {
			t.Fatalf("unpinned policy: %#v", call)
		}
		if !reflect.DeepEqual(call.roots, []string{svc.cfg.Projects[0].CanonicalPath}) {
			t.Fatalf("roots = %v", call.roots)
		}
	}
	if err := svc.RegisterDirectDelivery(ctx, 100, 0, 900, first); err != nil {
		t.Fatal(err)
	}
	route, err := svc.store.ResolveMessageRoute(ctx, 100, 0, 900)
	if err != nil || route == nil || route.ThreadID != first.ThreadID || route.TurnID != first.TurnID {
		t.Fatalf("start confirmation route = %#v err=%v", route, err)
	}
	binding, err := svc.store.GetBinding(ctx, 100, 0)
	if err != nil || binding == nil || binding.ThreadID != first.ThreadID {
		t.Fatalf("binding = %#v err=%v", binding, err)
	}
}

func TestTelegramNewThreadUsesWorker(t *testing.T) {
	svc, _ := newMinimalService(t)
	live := newRouterSession("global-must-not-start")
	useRouterSession(svc, live)
	workers := installWorkerFactory(svc, func(index int, worker *routerSession) {
		worker.threadIDs = []string{"worker-new"}
	})
	mustSelect(t, svc, 100, "bridge")

	response := submitMinimal(t, svc, model.InboundText{ChatID: 100, UserID: 7, Text: "run tests", ReceivedAt: svc.now()})

	if response.ThreadID != "worker-new" || response.TurnID == "" {
		t.Fatalf("response=%#v", response)
	}
	worker := workers.Single(t)
	if got := worker.callSequence(); !reflect.DeepEqual(got, []string{"thread/start", "turn/start:worker-new"}) {
		t.Fatalf("worker calls=%#v", got)
	}
	assertRouterSessionNoMutations(t, live)
	if got, want := worker.threadStartCWDs(), []string{svc.cfg.Projects[0].CanonicalPath}; !reflect.DeepEqual(got, want) {
		t.Fatalf("worker ThreadStart CWDs=%v, want %v", got, want)
	}
	calls := worker.turnCalls()
	if len(calls) != 1 || calls[0].threadID != "worker-new" || calls[0].message != "run tests" {
		t.Fatalf("worker TurnStart calls=%#v", calls)
	}
	if _, ok := svc.minimalWorkers.ByThread("worker-new"); !ok {
		t.Fatal("new-thread worker was not bound to the returned thread id")
	}
}

func TestMinimalPlainContinuationUsesBoundThreadInsteadOfStartingNew(t *testing.T) {
	svc, _ := newMinimalService(t)
	app := newRouterSession("new-thread-must-not-start")
	project := svc.cfg.Projects[0]
	thread := model.Thread{ID: "bound-thread", CWD: project.CanonicalPath, Status: "completed"}
	app.threadReads = map[string]map[string]any{
		thread.ID: threadReadPayload(thread.ID, "Bound title", project.CanonicalPath, "old-turn", "completed", "done"),
	}
	useRouterSession(svc, app)
	ctx := context.Background()
	mustSelect(t, svc, 100, project.ID)
	if err := svc.store.SetBinding(ctx, 100, 0, thread.ID, model.BindingModeBound); err != nil {
		t.Fatal(err)
	}
	if err := svc.store.UpsertThread(ctx, thread); err != nil {
		t.Fatal(err)
	}

	response := submitMinimal(t, svc, model.InboundText{ChatID: 100, UserID: 7, Text: "continue bound", ReceivedAt: svc.now()})

	if response.ThreadID != thread.ID {
		t.Fatalf("response thread = %q, want bound thread %q", response.ThreadID, thread.ID)
	}
	if got := app.ThreadStartCalls(); got != 0 {
		t.Fatalf("ThreadStart calls = %d, want 0", got)
	}
	calls := app.turnCalls()
	if len(calls) != 1 || calls[0].threadID != thread.ID || calls[0].message != "continue bound" || calls[0].cwd != project.CanonicalPath {
		t.Fatalf("TurnStart calls = %#v, want one bound continuation", calls)
	}
}

func TestMinimalSubmitNewSelectionLockRoutingFirstDelaysProjectPickerChange(t *testing.T) {
	svc, _ := newMinimalService(t)
	app := newRouterSession("routing-first")
	firstProject := svc.cfg.Projects[0]
	secondProject := svc.cfg.Projects[1]
	startEntered := make(chan struct{})
	releaseStart := make(chan struct{})
	app.startHook = func() error {
		close(startEntered)
		<-releaseStart
		return nil
	}
	useRouterSession(svc, app)
	ctx := context.Background()
	mustSelect(t, svc, 100, firstProject.ID)

	submitDone := make(chan error, 1)
	go func() {
		_, err := svc.HandleInboundText(ctx, model.InboundText{ChatID: 100, UserID: 7, Text: "route first", ReceivedAt: svc.now()})
		submitDone <- err
	}()
	<-startEntered

	token := createMinimalPickerRoute(t, svc, 100, 0, minimalProjectNewAction, secondProject.ID)
	callbackDone := make(chan error, 1)
	go func() {
		_, err := svc.HandleCallback(ctx, 100, 0, 777, 7, token)
		callbackDone <- err
	}()
	assertNotDone(t, callbackDone, "project picker callback completed while routing decision was in flight")
	close(releaseStart)
	if err := <-submitDone; err != nil {
		t.Fatal(err)
	}
	if err := <-callbackDone; err != nil {
		t.Fatal(err)
	}

	if got, want := app.threadStartCWDs(), []string{firstProject.CanonicalPath}; !reflect.DeepEqual(got, want) {
		t.Fatalf("ThreadStart CWDs = %v, want %v", got, want)
	}
	selected, err := svc.store.GetSelectedProject(ctx, 100, 0)
	if err != nil {
		t.Fatal(err)
	}
	if selected != secondProject.ID {
		t.Fatalf("selected project after queued picker callback = %q, want %q", selected, secondProject.ID)
	}
}

func TestMinimalSubmitNewSelectionLockSelectionFirstRoutesUnderNewChoice(t *testing.T) {
	svc, _ := newMinimalService(t)
	app := newRouterSession("selection-first")
	useRouterSession(svc, app)
	sender := newBlockingEditSender()
	svc.SetSender(sender)
	ctx := context.Background()
	firstProject := svc.cfg.Projects[0]
	secondProject := svc.cfg.Projects[1]
	mustSelect(t, svc, 100, firstProject.ID)
	token := createMinimalPickerRoute(t, svc, 100, 0, minimalProjectNewAction, secondProject.ID)

	callbackDone := make(chan error, 1)
	go func() {
		_, err := svc.HandleCallback(ctx, 100, 0, 777, 7, token)
		callbackDone <- err
	}()
	<-sender.entered

	submitDone := make(chan error, 1)
	go func() {
		_, err := svc.HandleInboundText(ctx, model.InboundText{ChatID: 100, UserID: 7, Text: "selection first", ReceivedAt: svc.now()})
		submitDone <- err
	}()
	assertNotDone(t, submitDone, "plain prompt routed before project picker callback finished")
	if got := app.ThreadStartCalls(); got != 0 {
		t.Fatalf("ThreadStart calls while picker callback is in flight = %d, want 0", got)
	}
	close(sender.release)
	if err := <-callbackDone; err != nil {
		t.Fatal(err)
	}
	if err := <-submitDone; err != nil {
		t.Fatal(err)
	}
	if got, want := app.threadStartCWDs(), []string{secondProject.CanonicalPath}; !reflect.DeepEqual(got, want) {
		t.Fatalf("ThreadStart CWDs = %v, want %v", got, want)
	}
}

func TestMinimalRouterDoesNotPersistStartedPromptInSQLiteFiles(t *testing.T) {
	svc, _ := newMinimalService(t)
	app := newRouterSession("private-thread")
	useRouterSession(svc, app)
	mustSelect(t, svc, 100, "bridge")
	const prompt = "ROUTER_PRIVATE_PROMPT_4c92e1"
	submitMinimal(t, svc, model.InboundText{ChatID: 100, UserID: 7, Text: prompt, ReceivedAt: svc.now()})
	if err := svc.Close(); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{svc.cfg.Paths.DBPath, svc.cfg.Paths.DBPath + "-wal", svc.cfg.Paths.DBPath + "-shm"} {
		data, err := os.ReadFile(path)
		if err != nil && os.IsNotExist(err) {
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(data, []byte(prompt)) {
			t.Fatalf("started prompt found in %s", filepath.Base(path))
		}
	}
}

func TestMinimalThreadReadRefreshPersistsMetadataWithoutContent(t *testing.T) {
	svc, _ := newMinimalService(t)
	app := newRouterSession()
	useRouterSession(svc, app)
	thread := model.Thread{ID: "refresh-private", CWD: svc.cfg.Projects[0].CanonicalPath, Status: "inProgress", ActiveTurnID: "turn-private"}
	if err := svc.store.UpsertThread(context.Background(), thread); err != nil {
		t.Fatal(err)
	}
	const prompt = "LATER_REFRESH_PROMPT_91f8c3"
	const final = "LATER_REFRESH_FINAL_7b22d4"
	const output = "LATER_REFRESH_OUTPUT_2d51aa"
	app.threadReads = map[string]map[string]any{thread.ID: {"thread": map[string]any{
		"id": thread.ID, "cwd": thread.CWD, "status": "completed",
		"turns": []any{map[string]any{"id": "turn-private", "status": "completed", "items": []any{
			map[string]any{"id": "user-private", "type": "userMessage", "text": prompt},
			map[string]any{"id": "tool-private", "type": "commandExecution", "command": "private-command", "status": "completed", "aggregatedOutput": output},
			map[string]any{"id": "final-private", "type": "agentMessage", "phase": "final_answer", "text": final},
		}}},
	}}}
	if _, err := svc.refreshThreadForOperation(context.Background(), app, thread.ID, "minimal_private_refresh"); err != nil {
		t.Fatal(err)
	}
	storedThread, err := svc.store.GetThread(context.Background(), thread.ID)
	if err != nil {
		t.Fatal(err)
	}
	storedSnapshot, err := svc.store.GetSnapshot(context.Background(), thread.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{prompt, final, output} {
		if storedThread != nil && (bytes.Contains(storedThread.Raw, []byte(secret)) || strings.Contains(storedThread.LastPreview, secret)) {
			t.Fatalf("thread persistence contains %q", secret)
		}
		if storedSnapshot != nil && bytes.Contains(storedSnapshot.CompactJSON, []byte(secret)) {
			t.Fatalf("snapshot persistence contains %q", secret)
		}
	}
	if err := svc.Close(); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{svc.cfg.Paths.DBPath, svc.cfg.Paths.DBPath + "-wal", svc.cfg.Paths.DBPath + "-shm"} {
		data, err := os.ReadFile(path)
		if err != nil && os.IsNotExist(err) {
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		for _, secret := range []string{prompt, final, output} {
			if bytes.Contains(data, []byte(secret)) {
				t.Fatalf("refresh content %q found in %s", secret, filepath.Base(path))
			}
		}
	}
}

func TestMinimalRouterRecanonicalizesProjectBeforeTurnStart(t *testing.T) {
	svc, _ := newMinimalService(t)
	app := newRouterSession("new-1")
	app.startHook = func() error { return os.Remove(svc.cfg.Projects[0].CanonicalPath) }
	useRouterSession(svc, app)
	mustSelect(t, svc, 100, "bridge")
	_, err := svc.HandleInboundText(context.Background(), model.InboundText{ChatID: 100, UserID: 7, Text: "secret", ReceivedAt: svc.now()})
	if err == nil || len(app.turnCalls()) != 0 {
		t.Fatalf("err=%v calls=%#v", err, app.turnCalls())
	}
}

func TestReplyQueuesBehindActiveTurnAndKeepsThread(t *testing.T) {
	svc, _ := newMinimalService(t)
	app := newRouterSession()
	useRouterSession(svc, app)
	thread := model.Thread{ID: "thr-1", CWD: svc.cfg.Projects[0].CanonicalPath, Status: "inProgress", ActiveTurnID: "turn-1"}
	seedReply(t, svc, 100, 501, thread)
	app.threadReads = map[string]map[string]any{thread.ID: {"thread": map[string]any{
		"id": thread.ID, "cwd": thread.CWD, "status": "active", "activeTurnId": "turn-1",
		"turns": []any{map[string]any{"id": "turn-1", "status": "inProgress"}},
	}}}
	mustSelect(t, svc, 100, "second")
	response := submitMinimal(t, svc, model.InboundText{ChatID: 100, UserID: 7, ReplyToMessageID: 501, Text: "then run lint", ReceivedAt: svc.now()})
	if response.ThreadID != "thr-1" || len(app.turnCalls()) != 0 || len(app.turnSteerCalls) != 0 {
		t.Fatalf("response=%#v starts=%#v", response, app.turnCalls())
	}
	claim, err := svc.store.ClaimPendingCommand(context.Background(), "thr-1")
	if err != nil {
		t.Fatal(err)
	}
	if claim == nil || claim.ThreadID != "thr-1" || claim.ProjectID != "bridge" || claim.Prompt != "then run lint" {
		t.Fatalf("queue claim = %#v", claim)
	}
}

func TestReplyCarriesRoutedCompletedTurnIntoTarget(t *testing.T) {
	svc, _ := newMinimalService(t)
	app := newRouterSession()
	useRouterSession(svc, app)
	parent := model.Thread{ID: "parent", CWD: svc.cfg.Projects[0].CanonicalPath, Status: "inProgress", ActiveTurnID: "turn-2"}
	if err := svc.store.UpsertThread(context.Background(), parent); err != nil {
		t.Fatal(err)
	}
	if err := svc.store.PutMessageRoute(context.Background(), model.MessageRoute{ChatID: 100, MessageID: 501, ThreadID: parent.ID, TurnID: "turn-2", CreatedAt: model.NowString()}); err != nil {
		t.Fatal(err)
	}
	app.threadReads = map[string]map[string]any{parent.ID: {"thread": map[string]any{
		"id": parent.ID, "cwd": parent.CWD, "status": "active", "activeTurnId": "turn-2",
		"turns": []any{map[string]any{"id": "turn-1", "status": "completed"}, map[string]any{"id": "turn-2", "status": "inProgress"}},
	}}}

	submitMinimal(t, svc, model.InboundText{ChatID: 100, UserID: 7, ReplyToMessageID: 501, Text: "after", ReceivedAt: svc.now()})

	queued, err := svc.store.ClaimPendingCommand(context.Background(), parent.ID)
	if err != nil || queued == nil || queued.SourceThreadID != parent.ID || queued.SourceTurnID != "turn-2" {
		t.Fatalf("queued=%#v err=%v", queued, err)
	}
}

func TestReplyRejectsMissingOrInProgressAnchor(t *testing.T) {
	for _, test := range []struct {
		name            string
		routeTurnID     string
		activeTurnID    string
		turns           []any
		wantQueue       bool
		wantSourceTurn  string
		wantSubmitError bool
	}{
		{
			name:            "missing requested turn",
			routeTurnID:     "turn-missing",
			turns:           []any{map[string]any{"id": "turn-1", "status": "completed"}},
			wantSubmitError: true,
		},
		{
			name:            "unrelated in progress requested turn",
			routeTurnID:     "turn-stale-active",
			activeTurnID:    "turn-current",
			turns:           []any{map[string]any{"id": "turn-stale-active", "status": "inProgress"}, map[string]any{"id": "turn-current", "status": "inProgress"}},
			wantSubmitError: true,
		},
		{
			name:           "explicit active turn queues as source",
			routeTurnID:    "turn-current",
			activeTurnID:   "turn-current",
			turns:          []any{map[string]any{"id": "turn-older", "status": "completed"}, map[string]any{"id": "turn-current", "status": "inProgress"}},
			wantQueue:      true,
			wantSourceTurn: "turn-current",
		},
		{
			name:           "plain binding uses latest completed turn",
			activeTurnID:   "turn-current",
			turns:          []any{map[string]any{"id": "turn-older", "status": "completed"}, map[string]any{"id": "turn-current", "status": "inProgress"}},
			wantQueue:      true,
			wantSourceTurn: "turn-older",
		},
		{
			name:            "plain binding rejects missing terminal turn",
			activeTurnID:    "turn-current",
			turns:           []any{map[string]any{"id": "turn-current", "status": "inProgress"}},
			wantSubmitError: true,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			svc, _ := newMinimalService(t)
			app := newRouterSession()
			useRouterSession(svc, app)
			parent := model.Thread{ID: "parent-" + strings.ReplaceAll(test.name, " ", "-"), CWD: svc.cfg.Projects[0].CanonicalPath, Status: "inProgress", ActiveTurnID: test.activeTurnID}
			if parent.ActiveTurnID == "" {
				parent.Status = "completed"
			}
			if err := svc.store.UpsertThread(context.Background(), parent); err != nil {
				t.Fatal(err)
			}
			app.threadReads = map[string]map[string]any{parent.ID: {"thread": map[string]any{
				"id": parent.ID, "cwd": parent.CWD, "status": "active", "activeTurnId": test.activeTurnID, "turns": test.turns,
			}}}
			inbound := model.InboundText{ChatID: 100, UserID: 7, Text: "after", ReceivedAt: svc.now()}
			if test.routeTurnID == "" {
				mustSelect(t, svc, 100, svc.cfg.Projects[0].ID)
				if err := svc.store.SetBinding(context.Background(), 100, 0, parent.ID, model.BindingModeBound); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := svc.store.PutMessageRoute(context.Background(), model.MessageRoute{ChatID: 100, MessageID: 501, ThreadID: parent.ID, TurnID: test.routeTurnID, CreatedAt: model.NowString()}); err != nil {
					t.Fatal(err)
				}
				inbound.ReplyToMessageID = 501
			}

			response, err := svc.HandleInboundText(context.Background(), inbound)
			if test.wantSubmitError {
				if err == nil {
					t.Fatalf("HandleInboundText response=%#v err=nil, want anchor error", response)
				}
				if queued, queueErr := svc.store.ClaimPendingCommand(context.Background(), parent.ID); queueErr != nil || queued != nil {
					t.Fatalf("queued after rejected anchor=%#v err=%v", queued, queueErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			queued, queueErr := svc.store.ClaimPendingCommand(context.Background(), parent.ID)
			if queueErr != nil {
				t.Fatal(queueErr)
			}
			if !test.wantQueue || queued == nil || queued.SourceTurnID != test.wantSourceTurn || queued.SourceThreadID != parent.ID {
				t.Fatalf("queued=%#v want source turn %q", queued, test.wantSourceTurn)
			}
		})
	}
}

func TestPlainBindingRejectsUnavailableSourceAnchorBeforeStart(t *testing.T) {
	svc, _ := newMinimalService(t)
	app := newRouterSession()
	useRouterSession(svc, app)
	parent := model.Thread{ID: "plain-no-anchor", CWD: svc.cfg.Projects[0].CanonicalPath, Status: "completed"}
	if err := svc.store.UpsertThread(context.Background(), parent); err != nil {
		t.Fatal(err)
	}
	mustSelect(t, svc, 100, svc.cfg.Projects[0].ID)
	if err := svc.store.SetBinding(context.Background(), 100, 0, parent.ID, model.BindingModeBound); err != nil {
		t.Fatal(err)
	}
	app.threadReads = map[string]map[string]any{parent.ID: {"thread": map[string]any{
		"id": parent.ID, "cwd": parent.CWD, "status": "completed", "turns": []any{},
	}}}

	response, err := svc.HandleInboundText(context.Background(), model.InboundText{ChatID: 100, UserID: 7, Text: "must not start", ReceivedAt: svc.now()})

	if !errors.Is(err, errSourceTurnUnavailable) {
		t.Fatalf("response=%#v err=%v, want source unavailable", response, err)
	}
	if calls := app.turnCalls(); len(calls) != 0 {
		t.Fatalf("TurnStart calls = %#v, want none", calls)
	}
	if queued, queueErr := svc.store.ClaimPendingCommand(context.Background(), parent.ID); queueErr != nil || queued != nil {
		t.Fatalf("queued after unavailable source=%#v err=%v", queued, queueErr)
	}
}

func TestResolveSourceAnchorRejectsMissingTurn(t *testing.T) {
	payload := map[string]any{"thread": map[string]any{
		"id": "parent", "turns": []any{map[string]any{"id": "turn-1", "status": "completed"}},
	}}
	if _, err := resolveSourceAnchor(payload, "turn-missing"); !errors.Is(err, errSourceTurnUnavailable) {
		t.Fatalf("err=%v", err)
	}
}

func TestResolveSourceAnchorRejectsNewestTerminalTurnWithoutID(t *testing.T) {
	payload := map[string]any{"thread": map[string]any{
		"id": "parent", "turns": []any{
			map[string]any{"id": "turn-older", "status": "completed"},
			map[string]any{"status": "completed"},
		},
	}}
	if _, err := resolveSourceAnchor(payload, ""); !errors.Is(err, errSourceTurnUnavailable) {
		t.Fatalf("err=%v", err)
	}
}

func TestResolveSourceAnchorRejectsTerminalTurnWithoutID(t *testing.T) {
	payload := map[string]any{"thread": map[string]any{
		"id": "parent", "turns": []any{map[string]any{"status": "completed"}},
	}}
	if _, err := resolveSourceAnchor(payload, ""); !errors.Is(err, errSourceTurnUnavailable) {
		t.Fatalf("err=%v", err)
	}
}

func TestMinimalReplyPrecedenceOverridesDifferentCurrentBinding(t *testing.T) {
	svc, _ := newMinimalService(t)
	app := newRouterSession("new-thread-must-not-start")
	useRouterSession(svc, app)
	ctx := context.Background()
	replyThread := model.Thread{ID: "reply-thread", CWD: svc.cfg.Projects[0].CanonicalPath, Status: "completed"}
	boundThread := model.Thread{ID: "bound-thread", CWD: svc.cfg.Projects[1].CanonicalPath, Status: "completed"}
	seedReply(t, svc, 100, 501, replyThread)
	if err := svc.store.UpsertThread(ctx, boundThread); err != nil {
		t.Fatal(err)
	}
	if err := svc.store.SetSelectedProject(ctx, 100, 0, svc.cfg.Projects[1].ID); err != nil {
		t.Fatal(err)
	}
	if err := svc.store.SetBinding(ctx, 100, 0, boundThread.ID, model.BindingModeBound); err != nil {
		t.Fatal(err)
	}
	app.threadReads = map[string]map[string]any{
		replyThread.ID: threadReadPayload(replyThread.ID, "Reply target", replyThread.CWD, "old-turn", "completed", "done"),
		boundThread.ID: threadReadPayload(boundThread.ID, "Bound target", boundThread.CWD, "old-turn", "completed", "done"),
	}

	response := submitMinimal(t, svc, model.InboundText{ChatID: 100, UserID: 7, ReplyToMessageID: 501, Text: "reply wins", ReceivedAt: svc.now()})

	if response.ThreadID != replyThread.ID {
		t.Fatalf("response thread = %q, want reply route %q", response.ThreadID, replyThread.ID)
	}
	calls := app.turnCalls()
	if len(calls) != 1 || calls[0].threadID != replyThread.ID || calls[0].message != "reply wins" {
		t.Fatalf("TurnStart calls = %#v, want reply target only", calls)
	}
}

func TestInactiveReplyIgnoresSelectedProjectAndResumesRoutedThread(t *testing.T) {
	svc, _ := newMinimalService(t)
	app := newRouterSession()
	useRouterSession(svc, app)
	thread := model.Thread{ID: "thr-inactive", CWD: svc.cfg.Projects[0].CanonicalPath, Status: "completed"}
	if err := svc.store.UpsertThread(context.Background(), thread); err != nil {
		t.Fatal(err)
	}
	if err := svc.store.PutMessageRoute(context.Background(), model.MessageRoute{ChatID: 100, MessageID: 502, ThreadID: thread.ID, TurnID: "turn-inactive", CreatedAt: model.NowString()}); err != nil {
		t.Fatal(err)
	}
	app.threadReads = map[string]map[string]any{
		thread.ID: threadReadPayload(thread.ID, "Inactive reply", thread.CWD, "turn-inactive", "completed", "done"),
	}
	mustSelect(t, svc, 100, "second")
	response := submitMinimal(t, svc, model.InboundText{ChatID: 100, UserID: 7, ReplyToMessageID: 502, Text: "continue", ReceivedAt: svc.now()})
	calls := app.turnCalls()
	if response.ThreadID != "thr-inactive" || len(app.threadResumeCalls) != 1 || len(calls) != 1 || calls[0].cwd != svc.cfg.Projects[0].CanonicalPath {
		t.Fatalf("response=%#v resumes=%#v starts=%#v", response, app.threadResumeCalls, calls)
	}
}

func TestReplyUsesAuthoritativeInactiveStateInsteadOfCachedActiveState(t *testing.T) {
	svc, _ := newMinimalService(t)
	app := newRouterSession()
	useRouterSession(svc, app)
	thread := model.Thread{ID: "stale-active", CWD: svc.cfg.Projects[0].CanonicalPath, Status: "inProgress", ActiveTurnID: "old-turn"}
	seedReply(t, svc, 100, 507, thread)
	app.threadReads = map[string]map[string]any{thread.ID: {"thread": map[string]any{
		"id": thread.ID, "cwd": thread.CWD, "status": "completed",
		"turns": []any{map[string]any{"id": "old-turn", "status": "completed"}},
	}}}
	response := submitMinimal(t, svc, model.InboundText{ChatID: 100, UserID: 7, ReplyToMessageID: 507, Text: "start now", ReceivedAt: svc.now()})
	if response.TurnID == "" || len(app.turnCalls()) != 1 {
		t.Fatalf("response=%#v starts=%#v", response, app.turnCalls())
	}
}

func TestPCOriginStaleInProgressAnchorWithTerminalStoreForksBeforeResume(t *testing.T) {
	svc, _ := newMinimalService(t)
	app := newRouterSession()
	useRouterSession(svc, app)
	parent := model.Thread{ID: "parent", CWD: svc.cfg.Projects[0].CanonicalPath, Status: "inProgress", ActiveTurnID: "turn-9"}
	if err := svc.store.UpsertThread(context.Background(), parent); err != nil {
		t.Fatal(err)
	}
	if err := svc.store.PutMessageRoute(context.Background(), model.MessageRoute{ChatID: 100, MessageID: 501, ThreadID: parent.ID, TurnID: "turn-9", CreatedAt: model.NowString()}); err != nil {
		t.Fatal(err)
	}
	terminalRead := map[string]any{"thread": map[string]any{
		"id": parent.ID, "cwd": parent.CWD, "status": "completed", "source": "vscode", "originator": "Codex Desktop",
		"turns": []any{map[string]any{"id": "turn-9", "status": "completed"}},
	}}
	staleActiveRead := map[string]any{"thread": map[string]any{
		"id": parent.ID, "cwd": parent.CWD, "status": "active", "activeTurnId": "turn-9", "source": "vscode", "originator": "Codex Desktop",
		"turns": []any{map[string]any{"id": "turn-9", "status": "inProgress"}},
	}}
	app.threadReadSeq = map[string][]map[string]any{parent.ID: []map[string]any{terminalRead, staleActiveRead}}
	app.readHook = func(threadID string) {
		if threadID != parent.ID {
			return
		}
		if len(app.threadReadSeq[parent.ID]) != 0 {
			return
		}
		if err := svc.store.UpsertThread(context.Background(), model.Thread{
			ID: parent.ID, CWD: parent.CWD, Status: "completed", ActiveTurnID: "turn-9",
		}); err != nil {
			t.Errorf("seed terminal store: %v", err)
		}
	}
	app.forkIDs = []string{"child"}

	response := submitMinimal(t, svc, model.InboundText{ChatID: 100, UserID: 7, ReplyToMessageID: 501, Text: "continue", ReceivedAt: svc.now()})

	if response.ThreadID != "child" || !strings.Contains(response.Text, "새 대화") {
		t.Fatalf("response=%#v", response)
	}
	if got := app.callSequence(); !reflect.DeepEqual(got, []string{"thread/fork:parent:turn-9", "turn/start:child"}) {
		t.Fatalf("sequence=%v", got)
	}
}

func TestPayloadLooksPCOriginCoversCLIAndExecSources(t *testing.T) {
	for _, source := range []string{"cli", "exec"} {
		if !payloadLooksPCOriginPayload(map[string]any{"thread": map[string]any{"source": source}}) {
			t.Fatalf("source %q was not treated as PC-origin", source)
		}
	}
	if payloadLooksPCOriginPayload(map[string]any{"thread": map[string]any{"source": "telegram"}}) {
		t.Fatal("telegram source was treated as PC-origin")
	}
}

func TestReplyRefreshDoesNotStartBesideCommandDrainedFromSameTerminal(t *testing.T) {
	svc, _ := newMinimalService(t)
	app := newRouterSession()
	useRouterSession(svc, app)
	thread := model.Thread{ID: "terminal-race", CWD: svc.cfg.Projects[0].CanonicalPath, Status: "inProgress", ActiveTurnID: "old-turn"}
	seedReply(t, svc, 100, 508, thread)
	if err := svc.store.EnqueuePendingCommand(context.Background(), model.PendingCommand{ThreadID: thread.ID, ProjectID: "bridge", Prompt: "older queued"}); err != nil {
		t.Fatal(err)
	}
	app.threadReads = map[string]map[string]any{thread.ID: {"thread": map[string]any{
		"id": thread.ID, "cwd": thread.CWD, "status": "completed",
		"turns": []any{map[string]any{"id": "old-turn", "status": "completed"}},
	}}}
	response := submitMinimal(t, svc, model.InboundText{ChatID: 100, UserID: 7, ReplyToMessageID: 508, Text: "newer queued", ReceivedAt: svc.now()})
	if calls := app.turnCalls(); len(calls) != 1 || calls[0].message != "older queued" {
		t.Fatalf("parallel starts = %#v", calls)
	}
	if !strings.Contains(response.Text, "대기열") {
		t.Fatalf("response = %#v", response)
	}
}

func TestConcurrentInactiveRepliesStartOneAndQueueOne(t *testing.T) {
	svc, _ := newMinimalService(t)
	app := newRouterSession()
	useRouterSession(svc, app)
	thread := model.Thread{ID: "concurrent-inactive", CWD: svc.cfg.Projects[0].CanonicalPath, Status: "completed"}
	seedReply(t, svc, 100, 509, thread)
	app.threadReads = map[string]map[string]any{thread.ID: {"thread": map[string]any{
		"id": thread.ID, "cwd": thread.CWD, "status": "completed",
		"turns": []any{map[string]any{"id": "old-turn", "status": "completed"}},
	}}}
	start := make(chan struct{})
	errs := make(chan error, 2)
	for _, prompt := range []string{"concurrent-one", "concurrent-two"} {
		prompt := prompt
		go func() {
			<-start
			_, err := svc.HandleInboundText(context.Background(), model.InboundText{ChatID: 100, UserID: 7, ReplyToMessageID: 509, Text: prompt, ReceivedAt: svc.now()})
			errs <- err
		}()
	}
	close(start)
	for range 2 {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
	if calls := app.turnCalls(); len(calls) != 1 {
		t.Fatalf("concurrent inactive starts = %#v", calls)
	}
	claim, err := svc.store.ClaimPendingCommand(context.Background(), thread.ID)
	if err != nil {
		t.Fatal(err)
	}
	if claim == nil || (claim.Prompt != "concurrent-one" && claim.Prompt != "concurrent-two") {
		t.Fatalf("queued concurrent reply = %#v", claim)
	}
}

func seedActivePCReply(t *testing.T, svc *Service, app *routerSession, messageID int64) model.Thread {
	t.Helper()
	parent := model.Thread{ID: "parent", CWD: svc.cfg.Projects[0].CanonicalPath, Status: "inProgress", ActiveTurnID: "turn-9"}
	if err := svc.store.UpsertThread(context.Background(), parent); err != nil {
		t.Fatal(err)
	}
	if err := svc.store.PutMessageRoute(context.Background(), model.MessageRoute{ChatID: 100, MessageID: messageID, ThreadID: parent.ID, TurnID: "turn-9", CreatedAt: model.NowString()}); err != nil {
		t.Fatal(err)
	}
	app.threadReads = map[string]map[string]any{parent.ID: {"thread": map[string]any{
		"id": parent.ID, "cwd": parent.CWD, "status": "active", "source": "vscode", "originator": "Codex Desktop",
		"turns": []any{map[string]any{"id": "turn-9", "status": "inProgress"}},
	}}}
	app.resumeErrByThread = map[string]error{"parent": &appserver.RPCError{Code: -32600, Message: "thread parent already has an active writer"}}
	app.forkIDs = []string{"child"}
	return parent
}

func TestPCActiveReplyWaitsThenForksFromCompletedTurn(t *testing.T) {
	svc, _ := newMinimalService(t)
	app := newRouterSession()
	useRouterSession(svc, app)
	parent := seedActivePCReply(t, svc, app, 509)
	app.resumeErrByThread = nil
	response := submitMinimal(t, svc, model.InboundText{ChatID: 100, UserID: 7, ReplyToMessageID: 509, Text: "first", ReceivedAt: svc.now()})
	if !strings.Contains(response.Text, "PC Codex가 분석 중") || len(app.turnCalls()) != 0 || app.forkCallCount() != 0 {
		t.Fatalf("response=%#v turns=%#v forks=%d", response, app.turnCalls(), app.forkCallCount())
	}
	app.threadReads[parent.ID] = map[string]any{"thread": map[string]any{
		"id": parent.ID, "cwd": parent.CWD, "status": "completed",
		"turns": []any{map[string]any{"id": "turn-9", "status": "completed"}},
	}}
	svc.handleLiveEvent(context.Background(), app, appserver.Event{Method: "turn/completed", Params: map[string]any{"threadId": parent.ID}})
	calls := app.turnCalls()
	if app.forkCallCount() != 1 || len(calls) != 1 || calls[0].threadID != "child" || calls[0].message != "first" {
		t.Fatalf("forks=%d calls=%#v", app.forkCallCount(), calls)
	}
}

func TestTelegramOriginActiveReplyKeepsGenericQueueMessage(t *testing.T) {
	svc, _ := newMinimalService(t)
	app := newRouterSession()
	useRouterSession(svc, app)
	parent := seedActivePCReply(t, svc, app, 509)
	if err := svc.markTelegramOriginTurn(context.Background(), parent.ID, parent.ActiveTurnID); err != nil {
		t.Fatal(err)
	}

	response := submitMinimal(t, svc, model.InboundText{ChatID: 100, UserID: 7, ReplyToMessageID: 509, Text: "first", ReceivedAt: svc.now()})

	if !strings.Contains(response.Text, "대기열") || strings.Contains(response.Text, "PC Codex") {
		t.Fatalf("response=%#v, want generic queue wording for Telegram-origin turn", response)
	}
}

func TestPCActiveRepliesMoveRemainingFIFOToChild(t *testing.T) {
	svc, _ := newMinimalService(t)
	app := newRouterSession()
	useRouterSession(svc, app)
	parent := seedActivePCReply(t, svc, app, 509)
	for _, prompt := range []string{"first", "second"} {
		submitMinimal(t, svc, model.InboundText{ChatID: 100, UserID: 7, ReplyToMessageID: 509, Text: prompt, ReceivedAt: svc.now()})
	}
	app.threadReads[parent.ID] = map[string]any{"thread": map[string]any{
		"id": parent.ID, "cwd": parent.CWD, "status": "completed",
		"turns": []any{map[string]any{"id": "turn-9", "status": "completed"}},
	}}
	svc.handleLiveEvent(context.Background(), app, appserver.Event{Method: "turn/completed", Params: map[string]any{"threadId": parent.ID}})
	releaseLinkedWorkerForTest(t, svc, 100, 0, parent.ID)
	child, err := svc.store.GetThread(context.Background(), "child")
	if err != nil || child == nil {
		t.Fatalf("child=%#v err=%v", child, err)
	}
	setTerminal(t, svc, *child)
	if err := svc.minimalRouter.DrainNext(context.Background(), "child"); err != nil {
		t.Fatal(err)
	}
	calls := app.turnCalls()
	if app.forkCallCount() != 1 || len(calls) != 2 || calls[0].message != "first" || calls[1].message != "second" || calls[1].threadID != "child" {
		t.Fatalf("forks=%d calls=%#v", app.forkCallCount(), calls)
	}
}

func TestPCActiveDrainForksOnWorkerAndRehomesSourceBacklog(t *testing.T) {
	svc, _ := newMinimalService(t)
	live := newRouterSession()
	useRouterSession(svc, live)
	workers := installWorkerFactory(svc, func(index int, worker *routerSession) {
		if index == 0 {
			worker.forkIDs = []string{"linked-1"}
		}
	})
	parent := seedActivePCReply(t, svc, live, 509)
	for _, prompt := range []string{"first", "second"} {
		submitMinimal(t, svc, model.InboundText{ChatID: 100, UserID: 7, ReplyToMessageID: 509, Text: prompt, ReceivedAt: svc.now()})
	}
	live.threadReads[parent.ID] = map[string]any{"thread": map[string]any{
		"id": parent.ID, "cwd": parent.CWD, "status": "completed", "source": "vscode", "originator": "Codex Desktop",
		"turns": []any{map[string]any{"id": "turn-9", "status": "completed"}},
	}}

	svc.handleLiveEvent(context.Background(), live, appserver.Event{Method: "turn/completed", Params: map[string]any{"threadId": parent.ID}})

	worker := workers.Single(t)
	if got := worker.callSequence(); !reflect.DeepEqual(got, []string{"thread/fork:parent:turn-9", "thread/name/set:linked-1", "turn/start:linked-1"}) {
		t.Fatalf("worker calls=%#v", got)
	}
	assertRouterSessionNoMutations(t, live)
	queued, err := svc.store.ClaimPendingCommand(context.Background(), "linked-1")
	if err != nil {
		t.Fatal(err)
	}
	if queued == nil || queued.Prompt != "second" || queued.SourceThreadID != parent.ID || queued.SourceTurnID != "turn-9" {
		t.Fatalf("re-homed queue=%#v", queued)
	}
}

func TestConcurrentSameAnchorRepliesCreateOneFork(t *testing.T) {
	svc, _ := newMinimalService(t)
	app := newRouterSession()
	useRouterSession(svc, app)
	parent := seedActivePCReply(t, svc, app, 509)
	parent.Status, parent.ActiveTurnID = "completed", ""
	if err := svc.store.UpsertThread(context.Background(), parent); err != nil {
		t.Fatal(err)
	}
	app.threadReads[parent.ID] = map[string]any{"thread": map[string]any{
		"id": parent.ID, "cwd": parent.CWD, "status": "completed",
		"turns": []any{map[string]any{"id": "turn-9", "status": "completed"}},
	}}
	start, errs := make(chan struct{}), make(chan error, 2)
	for _, prompt := range []string{"one", "two"} {
		prompt := prompt
		go func() {
			<-start
			_, err := svc.HandleInboundText(context.Background(), model.InboundText{ChatID: 100, UserID: 7, ReplyToMessageID: 509, Text: prompt, ReceivedAt: svc.now()})
			errs <- err
		}()
	}
	close(start)
	for range 2 {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
	queued, err := svc.store.ClaimPendingCommand(context.Background(), "child")
	if err != nil || queued == nil || app.forkCallCount() != 1 || len(app.turnCalls()) != 1 {
		t.Fatalf("queued=%#v err=%v forks=%d turns=%#v", queued, err, app.forkCallCount(), app.turnCalls())
	}
}

func TestConcurrentSameAnchorRepliesCreateOneWorkerForkAndQueueSecond(t *testing.T) {
	svc, _ := newMinimalService(t)
	live := newRouterSession()
	useRouterSession(svc, live)
	workers := installWorkerFactory(svc, func(index int, worker *routerSession) {
		worker.forkIDs = []string{"linked-1"}
	})
	parent := seedActivePCReply(t, svc, live, 509)
	parent.Status, parent.ActiveTurnID = "completed", ""
	if err := svc.store.UpsertThread(context.Background(), parent); err != nil {
		t.Fatal(err)
	}
	live.threadReads[parent.ID] = map[string]any{"thread": map[string]any{
		"id": parent.ID, "cwd": parent.CWD, "status": "completed", "source": "vscode", "originator": "Codex Desktop",
		"turns": []any{map[string]any{"id": "turn-9", "status": "completed"}},
	}}
	start, errs := make(chan struct{}), make(chan error, 2)
	for _, prompt := range []string{"one", "two"} {
		prompt := prompt
		go func() {
			<-start
			_, err := svc.HandleInboundText(context.Background(), model.InboundText{ChatID: 100, UserID: 7, ReplyToMessageID: 509, Text: prompt, ReceivedAt: svc.now()})
			errs <- err
		}()
	}
	close(start)
	for range 2 {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}

	worker := workers.Single(t)
	if got := worker.callSequence(); !reflect.DeepEqual(got, []string{"thread/fork:parent:turn-9", "thread/name/set:linked-1", "turn/start:linked-1"}) {
		t.Fatalf("worker calls=%#v", got)
	}
	queued, err := svc.store.ClaimPendingCommand(context.Background(), "linked-1")
	if err != nil {
		t.Fatal(err)
	}
	if queued == nil || (queued.Prompt != "one" && queued.Prompt != "two") || queued.SourceThreadID != parent.ID || queued.SourceTurnID != "turn-9" {
		t.Fatalf("queued concurrent reply=%#v", queued)
	}
	assertRouterSessionNoMutations(t, live)
}

func TestPCActiveDrainDefiniteForkFailureReleasesClaimedCommand(t *testing.T) {
	svc, _ := newMinimalService(t)
	app := newRouterSession()
	useRouterSession(svc, app)
	parent := model.Thread{ID: "parent", CWD: svc.cfg.Projects[0].CanonicalPath, Status: "completed"}
	if err := svc.store.UpsertThread(context.Background(), parent); err != nil {
		t.Fatal(err)
	}
	if err := svc.store.EnqueuePendingCommand(context.Background(), model.PendingCommand{
		ThreadID: parent.ID, SourceThreadID: parent.ID, SourceTurnID: "turn-9",
		ProjectID: "bridge", ChatID: 100, Prompt: "retry protected",
	}); err != nil {
		t.Fatal(err)
	}
	app.resumeErrByThread = map[string]error{"parent": &appserver.RPCError{Code: -32600, Message: "thread parent already has an active writer"}}
	app.forkErrs = []error{&appserver.RPCError{Code: -32602, Message: "lastTurnId is invalid"}}

	if err := svc.minimalRouter.DrainNext(context.Background(), parent.ID); err != nil {
		t.Fatal(err)
	}

	retry, err := svc.store.ClaimPendingCommand(context.Background(), parent.ID)
	if err != nil || retry == nil || retry.Prompt != "retry protected" || app.forkCallCount() != 1 || len(app.turnCalls()) != 0 {
		t.Fatalf("retry=%#v err=%v forks=%d turns=%#v", retry, err, app.forkCallCount(), app.turnCalls())
	}
}

func TestPCActiveDrainAmbiguousCreatingContinuationReleasesClaimAndStopsFIFO(t *testing.T) {
	svc, _ := newMinimalService(t)
	app := newRouterSession()
	useRouterSession(svc, app)
	parent := model.Thread{ID: "parent", CWD: svc.cfg.Projects[0].CanonicalPath, Status: "completed"}
	if err := svc.store.UpsertThread(context.Background(), parent); err != nil {
		t.Fatal(err)
	}
	key := model.MinimalContinuationKey{ChatID: 100, SourceThreadID: parent.ID, SourceTurnID: "turn-9"}
	if _, created, err := svc.store.ClaimMinimalContinuation(context.Background(), model.MinimalContinuation{Key: key, ProjectID: "bridge"}); err != nil || !created {
		t.Fatalf("creating continuation created=%t err=%v", created, err)
	}
	for _, prompt := range []string{"first protected", "second protected"} {
		if err := svc.store.EnqueuePendingCommand(context.Background(), model.PendingCommand{
			ThreadID: parent.ID, SourceThreadID: parent.ID, SourceTurnID: "turn-9",
			ProjectID: "bridge", ChatID: 100, Prompt: prompt,
		}); err != nil {
			t.Fatal(err)
		}
	}
	app.resumeErrByThread = map[string]error{"parent": &appserver.RPCError{Code: -32600, Message: "thread parent already has an active writer"}}
	app.forkIDs = []string{"must-not-create"}

	if err := svc.minimalRouter.DrainNext(context.Background(), parent.ID); err != nil {
		t.Fatal(err)
	}
	first, err := svc.store.ClaimPendingCommand(context.Background(), parent.ID)
	if err != nil || first == nil || first.Prompt != "first protected" {
		t.Fatalf("first retry=%#v err=%v", first, err)
	}
	if err := svc.store.ReleaseClaimedPendingCommand(context.Background(), first.ID); err != nil {
		t.Fatal(err)
	}
	if err := svc.minimalRouter.DrainNext(context.Background(), parent.ID); err != nil {
		t.Fatal(err)
	}
	again, err := svc.store.ClaimPendingCommand(context.Background(), parent.ID)
	if err != nil || again == nil || again.ID != first.ID || again.Prompt != "first protected" {
		t.Fatalf("repeated retry=%#v err=%v, want same protected first row", again, err)
	}
	if app.forkCallCount() != 0 || len(app.turnCalls()) != 0 {
		t.Fatalf("forks=%d turns=%#v, want no remote side effects before repair", app.forkCallCount(), app.turnCalls())
	}
}

func TestPCActiveDrainActivationFailureDoesNotStartChildTurn(t *testing.T) {
	svc, _ := newMinimalService(t)
	app := newRouterSession()
	useRouterSession(svc, app)
	parent := model.Thread{ID: "parent", CWD: svc.cfg.Projects[0].CanonicalPath, Status: "completed"}
	if err := svc.store.UpsertThread(context.Background(), parent); err != nil {
		t.Fatal(err)
	}
	otherKey := model.MinimalContinuationKey{ChatID: 100, SourceThreadID: "other-parent", SourceTurnID: "turn-other"}
	other, created, err := svc.store.ClaimMinimalContinuation(context.Background(), model.MinimalContinuation{Key: otherKey, ProjectID: "bridge"})
	if err != nil || !created {
		t.Fatalf("other continuation=%#v created=%t err=%v", other, created, err)
	}
	if err := svc.store.ActivateMinimalContinuation(context.Background(), *other, model.Thread{ID: "child", CWD: svc.cfg.Projects[0].CanonicalPath, Status: "completed"}); err != nil {
		t.Fatal(err)
	}
	if err := svc.store.EnqueuePendingCommand(context.Background(), model.PendingCommand{
		ThreadID: parent.ID, SourceThreadID: parent.ID, SourceTurnID: "turn-9",
		ProjectID: "bridge", ChatID: 100, Prompt: "must not start",
	}); err != nil {
		t.Fatal(err)
	}
	app.resumeErrByThread = map[string]error{"parent": &appserver.RPCError{Code: -32600, Message: "thread parent already has an active writer"}}
	app.forkIDs = []string{"child"}

	if err := svc.minimalRouter.DrainNext(context.Background(), parent.ID); err != nil {
		t.Fatal(err)
	}

	if calls := app.turnCalls(); len(calls) != 0 {
		t.Fatalf("turn calls=%#v, want no child turn after activation failure", calls)
	}
	if retry, err := svc.store.ClaimPendingCommand(context.Background(), parent.ID); err != nil || retry == nil || retry.Prompt != "must not start" {
		t.Fatalf("retry=%#v err=%v, want protected prompt released for explicit repair retry", retry, err)
	}
}

func TestPCActiveDrainPreStartForkValidationFailureReleasesClaimAndStopsFIFO(t *testing.T) {
	for _, tc := range []struct {
		name          string
		firstChildID  string
		payloadCWD    string
		prepareSecond func(*routerSession)
	}{
		{
			name:         "empty child",
			firstChildID: "",
			prepareSecond: func(app *routerSession) {
				app.forkIDs = []string{"", "child-after-repair"}
			},
		},
		{
			name:         "parent child",
			firstChildID: "parent",
			prepareSecond: func(app *routerSession) {
				app.forkIDs = []string{"parent", "child-after-repair"}
			},
		},
		{
			name:         "out of registry child",
			firstChildID: "child-outside",
			payloadCWD:   "outside",
			prepareSecond: func(app *routerSession) {
				app.forkPayloadCWD = ""
				app.forkIDs = []string{"child-outside", "child-after-repair"}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc, _ := newMinimalService(t)
			app := newRouterSession()
			useRouterSession(svc, app)
			parent := model.Thread{ID: "parent", CWD: svc.cfg.Projects[0].CanonicalPath, Status: "completed"}
			if err := svc.store.UpsertThread(context.Background(), parent); err != nil {
				t.Fatal(err)
			}
			for _, prompt := range []string{"A survives validation failure", "B remains behind A"} {
				if err := svc.store.EnqueuePendingCommand(context.Background(), model.PendingCommand{
					ThreadID: parent.ID, SourceThreadID: parent.ID, SourceTurnID: "turn-9",
					ProjectID: "bridge", ChatID: 100, Prompt: prompt,
				}); err != nil {
					t.Fatal(err)
				}
			}
			if tc.payloadCWD == "outside" {
				outside := filepath.Join(t.TempDir(), "outside")
				if err := os.MkdirAll(outside, 0o755); err != nil {
					t.Fatal(err)
				}
				app.forkPayloadCWD = outside
			}
			app.resumeErrByThread = map[string]error{"parent": &appserver.RPCError{Code: -32600, Message: "thread parent already has an active writer"}}
			app.forkIDs = []string{tc.firstChildID}

			if err := svc.minimalRouter.DrainNext(context.Background(), parent.ID); err != nil {
				t.Fatal(err)
			}
			first, err := svc.store.ClaimPendingCommand(context.Background(), parent.ID)
			if err != nil || first == nil || first.Prompt != "A survives validation failure" {
				t.Fatalf("first retry=%#v err=%v", first, err)
			}
			if err := svc.store.ReleaseClaimedPendingCommand(context.Background(), first.ID); err != nil {
				t.Fatal(err)
			}
			if err := svc.minimalRouter.DrainNext(context.Background(), parent.ID); err != nil {
				t.Fatal(err)
			}
			again, err := svc.store.ClaimPendingCommand(context.Background(), parent.ID)
			if err != nil || again == nil || again.ID != first.ID || again.Prompt != "A survives validation failure" {
				t.Fatalf("repeated retry=%#v err=%v", again, err)
			}
			if err := svc.store.ReleaseClaimedPendingCommand(context.Background(), again.ID); err != nil {
				t.Fatal(err)
			}
			if len(app.turnCalls()) != 0 || app.forkCallCount() != 1 {
				t.Fatalf("before repair forks=%d turns=%#v, want one fork attempt and no child turn", app.forkCallCount(), app.turnCalls())
			}

			tc.prepareSecond(app)
			if _, err := svc.handleCommand(context.Background(), 100, 0, "/repair", 0); err != nil {
				t.Fatal(err)
			}
			if err := svc.minimalRouter.DrainNext(context.Background(), parent.ID); err != nil {
				t.Fatal(err)
			}
			calls := app.turnCalls()
			if app.forkCallCount() != 2 || len(calls) != 1 || calls[0].threadID != "child-after-repair" || calls[0].message != "A survives validation failure" {
				t.Fatalf("after repair forks=%d calls=%#v, want A first on repaired child", app.forkCallCount(), calls)
			}
			releaseLinkedWorkerForTest(t, svc, 100, 0, parent.ID)
			child := model.Thread{ID: "child-after-repair", CWD: parent.CWD, Status: "completed"}
			if err := svc.store.UpsertThread(context.Background(), child); err != nil {
				t.Fatal(err)
			}
			if err := svc.minimalRouter.DrainNext(context.Background(), child.ID); err != nil {
				t.Fatal(err)
			}
			calls = app.turnCalls()
			if len(calls) != 2 || calls[1].threadID != child.ID || calls[1].message != "B remains behind A" {
				t.Fatalf("calls=%#v, want B preserved behind A", calls)
			}
		})
	}
}

func TestPCActiveDrainPreStartReusedChildValidationFailureReleasesClaimAndStopsFIFO(t *testing.T) {
	svc, _ := newMinimalService(t)
	app := newRouterSession()
	useRouterSession(svc, app)
	parent := model.Thread{ID: "parent", CWD: svc.cfg.Projects[0].CanonicalPath, Status: "completed"}
	child := model.Thread{ID: "child", CWD: svc.cfg.Projects[0].CanonicalPath, Status: "completed"}
	if err := svc.store.UpsertThread(context.Background(), parent); err != nil {
		t.Fatal(err)
	}
	claim, created, err := svc.store.ClaimMinimalContinuation(context.Background(), model.MinimalContinuation{
		Key:       model.MinimalContinuationKey{ChatID: 100, SourceThreadID: parent.ID, SourceTurnID: "turn-9"},
		ProjectID: "bridge",
	})
	if err != nil || !created {
		t.Fatalf("claim=%#v created=%t err=%v", claim, created, err)
	}
	if err := svc.store.ActivateMinimalContinuation(context.Background(), *claim, child); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "outside")
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	child.CWD = outside
	if err := svc.store.UpsertThread(context.Background(), child); err != nil {
		t.Fatal(err)
	}
	for _, prompt := range []string{"A survives reused validation failure", "B remains behind reused A"} {
		if err := svc.store.EnqueuePendingCommand(context.Background(), model.PendingCommand{
			ThreadID: parent.ID, SourceThreadID: parent.ID, SourceTurnID: "turn-9",
			ProjectID: "bridge", ChatID: 100, Prompt: prompt,
		}); err != nil {
			t.Fatal(err)
		}
	}
	app.resumeErrByThread = map[string]error{"parent": &appserver.RPCError{Code: -32600, Message: "thread parent already has an active writer"}}

	if err := svc.minimalRouter.DrainNext(context.Background(), parent.ID); err != nil {
		t.Fatal(err)
	}
	first, err := svc.store.ClaimPendingCommand(context.Background(), parent.ID)
	if err != nil || first == nil || first.Prompt != "A survives reused validation failure" {
		t.Fatalf("first retry=%#v err=%v", first, err)
	}
	if err := svc.store.ReleaseClaimedPendingCommand(context.Background(), first.ID); err != nil {
		t.Fatal(err)
	}
	if len(app.turnCalls()) != 0 || app.forkCallCount() != 0 {
		t.Fatalf("pre-start reused validation made remote calls: forks=%d turns=%#v", app.forkCallCount(), app.turnCalls())
	}
	child.CWD = parent.CWD
	if err := svc.store.UpsertThread(context.Background(), child); err != nil {
		t.Fatal(err)
	}
	if err := svc.minimalRouter.DrainNext(context.Background(), parent.ID); err != nil {
		t.Fatal(err)
	}
	calls := app.turnCalls()
	if len(calls) != 1 || calls[0].threadID != child.ID || calls[0].message != "A survives reused validation failure" {
		t.Fatalf("calls=%#v, want A first after child validation is repaired", calls)
	}
	releaseLinkedWorkerForTest(t, svc, 100, 0, parent.ID)
	child.Status = "completed"
	child.ActiveTurnID = ""
	if err := svc.store.UpsertThread(context.Background(), child); err != nil {
		t.Fatal(err)
	}
	if err := svc.minimalRouter.DrainNext(context.Background(), child.ID); err != nil {
		t.Fatal(err)
	}
	calls = app.turnCalls()
	if len(calls) != 2 || calls[1].threadID != child.ID || calls[1].message != "B remains behind reused A" {
		t.Fatalf("calls=%#v, want B preserved behind reused A", calls)
	}
}

func TestPCActiveDrainPreStartCapabilityFailureReleasesClaimAndStopsFIFO(t *testing.T) {
	svc, _ := newMinimalService(t)
	app := &stubSession{}
	useRouterSession(svc, (*routerSession)(nil))
	svc.mu.Lock()
	svc.live, svc.liveConnected = app, true
	svc.mu.Unlock()
	parent := model.Thread{ID: "parent", CWD: svc.cfg.Projects[0].CanonicalPath, Status: "completed"}
	if err := svc.store.UpsertThread(context.Background(), parent); err != nil {
		t.Fatal(err)
	}
	for _, prompt := range []string{"A survives missing fork capability", "B remains behind capability failure"} {
		if err := svc.store.EnqueuePendingCommand(context.Background(), model.PendingCommand{
			ThreadID: parent.ID, SourceThreadID: parent.ID, SourceTurnID: "turn-9",
			ProjectID: "bridge", ChatID: 100, Prompt: prompt,
		}); err != nil {
			t.Fatal(err)
		}
	}
	app.threadResumeErr = &appserver.RPCError{Code: -32600, Message: "thread parent already has an active writer"}

	if err := svc.minimalRouter.DrainNext(context.Background(), parent.ID); err != nil {
		t.Fatal(err)
	}
	first, err := svc.store.ClaimPendingCommand(context.Background(), parent.ID)
	if err != nil || first == nil || first.Prompt != "A survives missing fork capability" {
		t.Fatalf("first retry=%#v err=%v", first, err)
	}
	if err := svc.store.ReleaseClaimedPendingCommand(context.Background(), first.ID); err != nil {
		t.Fatal(err)
	}
	if err := svc.minimalRouter.DrainNext(context.Background(), parent.ID); err != nil {
		t.Fatal(err)
	}
	again, err := svc.store.ClaimPendingCommand(context.Background(), parent.ID)
	if err != nil || again == nil || again.ID != first.ID || again.Prompt != "A survives missing fork capability" {
		t.Fatalf("repeated retry=%#v err=%v, want same protected first row", again, err)
	}
}

func TestPCActiveDrainPreStartActiveReusedChildEnqueueFailureReleasesClaimAndStopsFIFO(t *testing.T) {
	svc, _ := newMinimalService(t)
	app := newRouterSession()
	useRouterSession(svc, app)
	parent := model.Thread{ID: "parent", CWD: svc.cfg.Projects[0].CanonicalPath, Status: "completed"}
	child := model.Thread{ID: "child", CWD: svc.cfg.Projects[0].CanonicalPath, Status: "inProgress", ActiveTurnID: "child-active"}
	if err := svc.store.UpsertThread(context.Background(), parent); err != nil {
		t.Fatal(err)
	}
	claim, created, err := svc.store.ClaimMinimalContinuation(context.Background(), model.MinimalContinuation{
		Key:       model.MinimalContinuationKey{ChatID: 100, SourceThreadID: parent.ID, SourceTurnID: "turn-9"},
		ProjectID: "bridge",
	})
	if err != nil || !created {
		t.Fatalf("claim=%#v created=%t err=%v", claim, created, err)
	}
	if err := svc.store.ActivateMinimalContinuation(context.Background(), *claim, child); err != nil {
		t.Fatal(err)
	}
	for _, prompt := range []string{"A survives active child enqueue failure", "B remains behind active enqueue failure"} {
		if err := svc.store.EnqueuePendingCommand(context.Background(), model.PendingCommand{
			ThreadID: parent.ID, SourceThreadID: parent.ID, SourceTurnID: "turn-9",
			ProjectID: "bridge", ChatID: 100, Prompt: prompt,
		}); err != nil {
			t.Fatal(err)
		}
	}
	app.threadReads = map[string]map[string]any{child.ID: {"thread": map[string]any{
		"id": child.ID, "cwd": child.CWD, "status": "active", "activeTurnId": "child-active",
		"turns": []any{map[string]any{"id": "child-active", "status": "inProgress"}},
	}}}
	app.resumeErrByThread = map[string]error{"parent": &appserver.RPCError{Code: -32600, Message: "thread parent already has an active writer"}}
	svc.minimalRouter.enqueuePending = func(_ context.Context, command model.PendingCommand) error {
		if command.ThreadID == child.ID && command.Prompt == "A survives active child enqueue failure" {
			return errors.New("injected active child pending protect failure")
		}
		return svc.store.EnqueuePendingCommand(context.Background(), command)
	}

	if err := svc.minimalRouter.DrainNext(context.Background(), parent.ID); err != nil {
		t.Fatal(err)
	}
	first, err := svc.store.ClaimPendingCommandForSource(context.Background(), 100, 0, parent.ID, "turn-9")
	if err != nil || first == nil || first.Prompt != "A survives active child enqueue failure" {
		t.Fatalf("first exact-source retry=%#v err=%v", first, err)
	}
	if first.ThreadID != child.ID {
		t.Fatalf("first retry thread=%q, want rehomed child", first.ThreadID)
	}
	if err := svc.store.ReleaseClaimedPendingCommand(context.Background(), first.ID); err != nil {
		t.Fatal(err)
	}
	again, err := svc.store.ClaimPendingCommandForSource(context.Background(), 100, 0, parent.ID, "turn-9")
	if err != nil || again == nil || again.ID != first.ID || again.Prompt != "A survives active child enqueue failure" {
		t.Fatalf("repeated exact-source retry=%#v err=%v", again, err)
	}
	if len(app.turnCalls()) != 0 {
		t.Fatalf("turn calls=%#v, want no child TurnStart before enqueue succeeds", app.turnCalls())
	}
	active, err := svc.store.ActiveMinimalContinuationForFork(context.Background(), 100, 0, child.ID)
	if err != nil || active == nil || active.Status != model.MinimalContinuationActive {
		t.Fatalf("active continuation=%#v err=%v, want active state preserved", active, err)
	}
}

func TestDirectReplyAfterTerminalDoesNotOvertakeEarlierExactAnchorQueue(t *testing.T) {
	svc, _ := newMinimalService(t)
	app := newRouterSession()
	useRouterSession(svc, app)
	parent := model.Thread{ID: "parent", CWD: svc.cfg.Projects[0].CanonicalPath, Status: "completed"}
	if err := svc.store.UpsertThread(context.Background(), parent); err != nil {
		t.Fatal(err)
	}
	if err := svc.store.PutMessageRoute(context.Background(), model.MessageRoute{ChatID: 100, MessageID: 501, ThreadID: parent.ID, TurnID: "turn-9", CreatedAt: model.NowString()}); err != nil {
		t.Fatal(err)
	}
	if err := svc.store.EnqueuePendingCommand(context.Background(), model.PendingCommand{
		ThreadID: parent.ID, SourceThreadID: parent.ID, SourceTurnID: "turn-9",
		ProjectID: "bridge", ChatID: 100, Prompt: "A queued while PC active",
	}); err != nil {
		t.Fatal(err)
	}
	app.threadReads = map[string]map[string]any{parent.ID: {"thread": map[string]any{
		"id": parent.ID, "cwd": parent.CWD, "status": "completed",
		"turns": []any{map[string]any{"id": "turn-9", "status": "completed"}},
	}}}
	app.resumeErrByThread = map[string]error{"parent": &appserver.RPCError{Code: -32600, Message: "thread parent already has an active writer"}}
	app.forkIDs = []string{"child"}

	response := submitMinimal(t, svc, model.InboundText{ChatID: 100, UserID: 7, ReplyToMessageID: 501, Text: "B direct after terminal", ReceivedAt: svc.now()})

	if !strings.Contains(response.Text, "대기열") {
		t.Fatalf("response=%#v, want B queued behind existing anchor work", response)
	}
	calls := app.turnCalls()
	if app.forkCallCount() != 1 || len(calls) != 1 || calls[0].threadID != "child" || calls[0].message != "A queued while PC active" {
		t.Fatalf("forks=%d calls=%#v, want A to start first on one fork", app.forkCallCount(), calls)
	}
	releaseLinkedWorkerForTest(t, svc, 100, 0, parent.ID)
	child := model.Thread{ID: "child", CWD: parent.CWD, Status: "completed"}
	if err := svc.store.UpsertThread(context.Background(), child); err != nil {
		t.Fatal(err)
	}
	if err := svc.minimalRouter.DrainNext(context.Background(), child.ID); err != nil {
		t.Fatal(err)
	}
	calls = app.turnCalls()
	if len(calls) != 2 || calls[1].threadID != "child" || calls[1].message != "B direct after terminal" {
		t.Fatalf("calls=%#v, want B second on same child", calls)
	}
}

func TestDirectReplySequencedReadQueuesBehindExistingExactAnchorBacklog(t *testing.T) {
	svc, _ := newMinimalService(t)
	app := newRouterSession()
	useRouterSession(svc, app)
	parent := model.Thread{ID: "parent", CWD: svc.cfg.Projects[0].CanonicalPath, Status: "inProgress", ActiveTurnID: "turn-9"}
	if err := svc.store.UpsertThread(context.Background(), parent); err != nil {
		t.Fatal(err)
	}
	if err := svc.store.PutMessageRoute(context.Background(), model.MessageRoute{ChatID: 100, MessageID: 501, ThreadID: parent.ID, TurnID: "turn-9", CreatedAt: model.NowString()}); err != nil {
		t.Fatal(err)
	}
	if err := svc.store.EnqueuePendingCommand(context.Background(), model.PendingCommand{
		ThreadID: parent.ID, SourceThreadID: parent.ID, SourceTurnID: "turn-9",
		ProjectID: "bridge", ChatID: 100, Prompt: "A queued before observer drain",
	}); err != nil {
		t.Fatal(err)
	}
	activeRead := map[string]any{"thread": map[string]any{
		"id": parent.ID, "cwd": parent.CWD, "status": "active", "activeTurnId": "turn-9",
		"turns": []any{map[string]any{"id": "turn-9", "status": "inProgress"}},
	}}
	completedRead := map[string]any{"thread": map[string]any{
		"id": parent.ID, "cwd": parent.CWD, "status": "completed", "activeTurnId": "turn-9",
		"turns": []any{map[string]any{"id": "turn-9", "status": "completed"}},
	}}
	app.threadReadSeq = map[string][]map[string]any{parent.ID: []map[string]any{activeRead, completedRead}}
	app.resumeErrByThread = map[string]error{"parent": &appserver.RPCError{Code: -32600, Message: "thread parent already has an active writer"}}
	app.forkIDs = []string{"child"}

	response := submitMinimal(t, svc, model.InboundText{ChatID: 100, UserID: 7, ReplyToMessageID: 501, Text: "B direct after terminal read", ReceivedAt: svc.now()})

	if !strings.Contains(response.Text, "대기열") {
		t.Fatalf("response=%#v, want B queued behind A", response)
	}
	calls := app.turnCalls()
	if app.forkCallCount() != 1 || len(calls) != 1 || calls[0].threadID != "child" || calls[0].message != "A queued before observer drain" {
		t.Fatalf("forks=%d calls=%#v, want A started first on one fork", app.forkCallCount(), calls)
	}
	releaseLinkedWorkerForTest(t, svc, 100, 0, parent.ID)
	child := model.Thread{ID: "child", CWD: parent.CWD, Status: "completed"}
	if err := svc.store.UpsertThread(context.Background(), child); err != nil {
		t.Fatal(err)
	}
	if err := svc.minimalRouter.DrainNext(context.Background(), child.ID); err != nil {
		t.Fatal(err)
	}
	calls = app.turnCalls()
	if len(calls) != 2 || calls[1].threadID != child.ID || calls[1].message != "B direct after terminal read" {
		t.Fatalf("calls=%#v, want B second on same child", calls)
	}
}

func TestReplyRefreshKeepsNewerTelegramActiveTurnStartedByInlineDrain(t *testing.T) {
	svc, _ := newMinimalService(t)
	app := newRouterSession()
	useRouterSession(svc, app)
	parent := model.Thread{ID: "parent", CWD: svc.cfg.Projects[0].CanonicalPath, Status: "inProgress", ActiveTurnID: "turn-9"}
	if err := svc.store.UpsertThread(context.Background(), parent); err != nil {
		t.Fatal(err)
	}
	if err := svc.markTelegramOriginTurn(context.Background(), parent.ID, parent.ActiveTurnID); err != nil {
		t.Fatal(err)
	}
	if err := svc.store.PutMessageRoute(context.Background(), model.MessageRoute{ChatID: 100, MessageID: 501, ThreadID: parent.ID, TurnID: parent.ActiveTurnID, CreatedAt: model.NowString()}); err != nil {
		t.Fatal(err)
	}
	if err := svc.store.EnqueuePendingCommand(context.Background(), model.PendingCommand{
		ThreadID: parent.ID, ProjectID: "bridge", ChatID: 100, Prompt: "A starts from terminal refresh",
	}); err != nil {
		t.Fatal(err)
	}
	app.turnN = 1
	terminalRead := map[string]any{"thread": map[string]any{
		"id": parent.ID, "cwd": parent.CWD, "status": "completed",
		"turns": []any{map[string]any{"id": "turn-9", "status": "completed"}},
	}}
	app.threadReadSeq = map[string][]map[string]any{parent.ID: []map[string]any{terminalRead, terminalRead}}

	response := submitMinimal(t, svc, model.InboundText{ChatID: 100, UserID: 7, ReplyToMessageID: 501, Text: "B must queue behind T2", ReceivedAt: svc.now()})

	if response.TurnID != "turn-2" || !strings.Contains(response.Text, "대기열") {
		t.Fatalf("response=%#v, want B queued behind newer T2", response)
	}
	calls := app.turnCalls()
	if len(calls) != 1 || calls[0].threadID != parent.ID || calls[0].message != "A starts from terminal refresh" {
		t.Fatalf("turn calls=%#v, want only queued A to start as T2", calls)
	}
	if app.forkCallCount() != 0 {
		t.Fatalf("forks=%d, want no continuation fork beside T2", app.forkCallCount())
	}
	stored, err := svc.store.GetThread(context.Background(), parent.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored == nil || stored.ActiveTurnID != "turn-2" || !threadLooksActiveForInput(stored) {
		t.Fatalf("stored=%#v, want newer active T2 preserved", stored)
	}
	queued, err := svc.store.ClaimPendingCommand(context.Background(), parent.ID)
	if err != nil || queued == nil || queued.Prompt != "B must queue behind T2" {
		t.Fatalf("queued=%#v err=%v, want B behind T2", queued, err)
	}
}

func TestReplyRefreshTerminalWithoutDrainDoesNotResurrectOldTelegramActiveTurn(t *testing.T) {
	svc, _ := newMinimalService(t)
	app := newRouterSession()
	useRouterSession(svc, app)
	parent := model.Thread{ID: "parent", CWD: svc.cfg.Projects[0].CanonicalPath, Status: "inProgress", ActiveTurnID: "turn-9"}
	if err := svc.store.UpsertThread(context.Background(), parent); err != nil {
		t.Fatal(err)
	}
	if err := svc.markTelegramOriginTurn(context.Background(), parent.ID, parent.ActiveTurnID); err != nil {
		t.Fatal(err)
	}
	if err := svc.store.PutMessageRoute(context.Background(), model.MessageRoute{ChatID: 100, MessageID: 501, ThreadID: parent.ID, TurnID: parent.ActiveTurnID, CreatedAt: model.NowString()}); err != nil {
		t.Fatal(err)
	}
	terminalRead := map[string]any{"thread": map[string]any{
		"id": parent.ID, "cwd": parent.CWD, "status": "completed",
		"turns": []any{map[string]any{"id": "turn-9", "status": "completed"}},
	}}
	app.threadReadSeq = map[string][]map[string]any{parent.ID: []map[string]any{terminalRead, terminalRead}}
	app.fail = map[string]error{"B fails after terminal reconciliation": errors.New("injected post-terminal start failure")}

	response, err := svc.HandleInboundText(context.Background(), model.InboundText{ChatID: 100, UserID: 7, ReplyToMessageID: 501, Text: "B fails after terminal reconciliation", ReceivedAt: svc.now()})
	if err == nil || response != nil {
		t.Fatalf("response=%#v err=%v, want injected start failure", response, err)
	}
	stored, err := svc.store.GetThread(context.Background(), parent.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored == nil || stored.ActiveTurnID != "" || stored.Status != "completed" {
		t.Fatalf("stored=%#v, want terminal state preserved without resurrecting old T", stored)
	}
	if app.forkCallCount() != 0 {
		t.Fatalf("forks=%d, want no continuation fork after terminal reconciliation", app.forkCallCount())
	}
}

func TestSameChatBoundSubmitAndReplyUseSingleSelectionBeforeThreadOrder(t *testing.T) {
	svc, _ := newMinimalService(t)
	app := newRouterSession()
	useRouterSession(svc, app)
	parent := model.Thread{ID: "parent", CWD: svc.cfg.Projects[0].CanonicalPath, Status: "inProgress", ActiveTurnID: "pc-active"}
	if err := svc.store.UpsertThread(context.Background(), parent); err != nil {
		t.Fatal(err)
	}
	if err := svc.store.PutMessageRoute(context.Background(), model.MessageRoute{ChatID: 100, MessageID: 501, ThreadID: parent.ID, TurnID: "turn-9", CreatedAt: model.NowString()}); err != nil {
		t.Fatal(err)
	}
	mustSelect(t, svc, 100, "bridge")
	if err := svc.store.SetBinding(context.Background(), 100, 0, parent.ID, model.BindingModeBound); err != nil {
		t.Fatal(err)
	}
	activeRead := map[string]any{"thread": map[string]any{
		"id": parent.ID, "cwd": parent.CWD, "status": "active", "activeTurnId": "pc-active",
		"turns": []any{
			map[string]any{"id": "turn-9", "status": "completed"},
			map[string]any{"id": "pc-active", "status": "inProgress"},
		},
	}}
	app.threadReads = map[string]map[string]any{parent.ID: activeRead}
	app.resumeErrByThread = map[string]error{"parent": &appserver.RPCError{Code: -32600, Message: "thread parent already has an active writer"}}
	app.forkIDs = []string{"child"}
	selectionHeld := minimalSelectionLock(100, 0)
	defer func() { selectionHeld() }()
	selectionAttempted := make(chan struct{})
	threadAttempted := make(chan struct{})
	var selectionOnce sync.Once
	var threadOnce sync.Once
	svc.minimalRouter.selectionLockAttemptHook = func(chatID, topicID int64) {
		if chatID == 100 && topicID == 0 {
			selectionOnce.Do(func() { close(selectionAttempted) })
		}
	}
	svc.minimalRouter.threadLockAttemptHook = func(threadID string) {
		if threadID == parent.ID {
			threadOnce.Do(func() { close(threadAttempted) })
		}
	}
	replyDone := make(chan continuationSubmitResult, 1)
	go func() {
		response, err := svc.HandleInboundText(context.Background(), model.InboundText{ChatID: 100, UserID: 7, ReplyToMessageID: 501, Text: "B routed reply same chat", ReceivedAt: svc.now()})
		replyDone <- continuationSubmitResult{response: response, err: err}
	}()
	select {
	case <-selectionAttempted:
	case <-threadAttempted:
		t.Fatal("routed reply attempted source thread lock before same-chat selection gate")
	case result := <-replyDone:
		t.Fatalf("reply completed while selection gate was held: response=%#v err=%v", result.response, result.err)
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for routed reply to reach selection gate")
	}
	select {
	case <-threadAttempted:
		t.Fatal("routed reply acquired/attempted thread lock while selection gate remained held")
	default:
	}
	selectionHeld()
	selectionHeld = func() {}
	replyResult := waitSubmitResult(t, replyDone, "routed reply submit")
	if replyResult.err != nil {
		t.Fatalf("reply submit failed: %v", replyResult.err)
	}
	svc.minimalRouter.selectionLockAttemptHook = nil
	svc.minimalRouter.threadLockAttemptHook = nil

	drainThreadAttempts := 0
	drainSelectionAttempts := 0
	svc.minimalRouter.threadLockAttemptHook = func(threadID string) {
		if threadID == parent.ID {
			drainThreadAttempts++
		}
	}
	svc.minimalRouter.selectionLockAttemptHook = func(chatID, topicID int64) {
		if chatID == 100 && topicID == 0 {
			drainSelectionAttempts++
		}
	}
	if err := svc.minimalRouter.DrainNext(context.Background(), parent.ID); err != nil {
		t.Fatalf("observer-style DrainNext while selection hook installed failed: %v", err)
	}
	svc.minimalRouter.selectionLockAttemptHook = nil
	svc.minimalRouter.threadLockAttemptHook = nil
	if drainThreadAttempts == 0 {
		t.Fatal("DrainNext did not attempt the thread lock")
	}
	if drainSelectionAttempts != 0 {
		t.Fatalf("DrainNext attempted selection lock %d times, want 0", drainSelectionAttempts)
	}
}

func TestPCActiveDrainRemoteChildStartLocalPersistFailureKeepsFIFOBlocked(t *testing.T) {
	svc, _ := newMinimalService(t)
	app := newRouterSession()
	useRouterSession(svc, app)
	parent := model.Thread{ID: "parent", CWD: svc.cfg.Projects[0].CanonicalPath, Status: "completed"}
	if err := svc.store.UpsertThread(context.Background(), parent); err != nil {
		t.Fatal(err)
	}
	for _, prompt := range []string{"remote child started", "must stay blocked"} {
		if err := svc.store.EnqueuePendingCommand(context.Background(), model.PendingCommand{
			ThreadID: parent.ID, SourceThreadID: parent.ID, SourceTurnID: "turn-9",
			ProjectID: "bridge", ChatID: 100, Prompt: prompt,
		}); err != nil {
			t.Fatal(err)
		}
	}
	app.resumeErrByThread = map[string]error{"parent": &appserver.RPCError{Code: -32600, Message: "thread parent already has an active writer"}}
	app.forkIDs = []string{"child"}
	svc.minimalRouter.persistThread = func(_ context.Context, thread model.Thread) error {
		if thread.ID == "child" && thread.ActiveTurnID != "" {
			return errors.New("injected child persistence failure")
		}
		return svc.persistThread(context.Background(), thread)
	}

	err := svc.minimalRouter.DrainNext(context.Background(), parent.ID)
	if err == nil || !strings.Contains(err.Error(), "persist remotely started turn") {
		t.Fatalf("DrainNext error = %v, want remote-start/local-persist safety error", err)
	}
	calls := app.turnCalls()
	if app.forkCallCount() != 1 || len(calls) != 1 || calls[0].threadID != "child" || calls[0].message != "remote child started" {
		t.Fatalf("forks=%d calls=%#v", app.forkCallCount(), calls)
	}
	claim, claimErr := svc.store.ClaimPendingCommand(context.Background(), "child")
	if claimErr != nil || claim != nil {
		t.Fatalf("child claim=%#v err=%v, want claimed first row to block FIFO", claim, claimErr)
	}
	setTerminal(t, svc, model.Thread{ID: "child", CWD: svc.cfg.Projects[0].CanonicalPath})
	if drainErr := svc.minimalRouter.DrainNext(context.Background(), "child"); drainErr != nil {
		t.Fatal(drainErr)
	}
	if calls := app.turnCalls(); len(calls) != 1 {
		t.Fatalf("turn calls after child drain = %#v, want no later FIFO start", calls)
	}
	if app.forkCallCount() != 1 {
		t.Fatalf("fork calls=%d, want no duplicate fork", app.forkCallCount())
	}
}

func TestReplyOutsideRegistryFailsClosed(t *testing.T) {
	svc, _ := newMinimalService(t)
	app := newRouterSession()
	useRouterSession(svc, app)
	outside := filepath.Join(t.TempDir(), "outside")
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	seedReply(t, svc, 100, 503, model.Thread{ID: "outside", CWD: outside, Status: "completed"})
	mustSelect(t, svc, 100, "bridge")
	_, err := svc.HandleInboundText(context.Background(), model.InboundText{ChatID: 100, UserID: 7, ReplyToMessageID: 503, Text: "no guess", ReceivedAt: svc.now()})
	if err == nil || len(app.turnCalls()) != 0 {
		t.Fatalf("err=%v starts=%#v", err, app.turnCalls())
	}
}

func TestMinimalRouterFIFOAndDuplicateTerminalAreSerialized(t *testing.T) {
	svc, _ := newMinimalService(t)
	app := newRouterSession()
	useRouterSession(svc, app)
	thread := model.Thread{ID: "fifo", CWD: svc.cfg.Projects[0].CanonicalPath, Status: "inProgress", ActiveTurnID: "active"}
	seedReply(t, svc, 100, 504, thread)
	app.threadReads = map[string]map[string]any{thread.ID: activeThreadReadPayload(thread)}
	for _, prompt := range []string{"first", "second"} {
		submitMinimal(t, svc, model.InboundText{ChatID: 100, UserID: 7, ReplyToMessageID: 504, Text: prompt, ReceivedAt: svc.now()})
	}
	setTerminal(t, svc, thread)
	if err := svc.minimalRouter.DrainNext(context.Background(), thread.ID); err != nil {
		t.Fatal(err)
	}
	if err := svc.minimalRouter.DrainNext(context.Background(), thread.ID); err != nil {
		t.Fatal(err)
	}
	if calls := app.turnCalls(); len(calls) != 1 || calls[0].message != "first" {
		t.Fatalf("duplicate drain = %#v", calls)
	}
	setTerminal(t, svc, thread)
	if err := svc.minimalRouter.DrainNext(context.Background(), thread.ID); err != nil {
		t.Fatal(err)
	}
	if calls := app.turnCalls(); len(calls) != 2 || calls[1].message != "second" {
		t.Fatalf("FIFO = %#v", calls)
	}
}

func TestMinimalFIFOForActiveBoundPlainMessagesDrainsInOrder(t *testing.T) {
	svc, _ := newMinimalService(t)
	app := newRouterSession()
	project := svc.cfg.Projects[0]
	thread := model.Thread{ID: "bound-fifo", CWD: project.CanonicalPath, Status: "inProgress", ActiveTurnID: "active"}
	app.threadReads = map[string]map[string]any{
		thread.ID: {"thread": map[string]any{
			"id": thread.ID, "cwd": project.CanonicalPath, "status": "active", "activeTurnId": "active",
			"turns": []any{map[string]any{"id": "previous-completed", "status": "completed"}, map[string]any{"id": "active", "status": "inProgress"}},
		}},
	}
	useRouterSession(svc, app)
	ctx := context.Background()
	mustSelect(t, svc, 100, project.ID)
	if err := svc.store.UpsertThread(ctx, thread); err != nil {
		t.Fatal(err)
	}
	if err := svc.store.SetBinding(ctx, 100, 0, thread.ID, model.BindingModeBound); err != nil {
		t.Fatal(err)
	}

	for _, prompt := range []string{"first plain", "second plain"} {
		response := submitMinimal(t, svc, model.InboundText{ChatID: 100, UserID: 7, Text: prompt, ReceivedAt: svc.now()})
		if response.ThreadID != thread.ID || !strings.Contains(response.Text, "대기열") {
			t.Fatalf("queued response for %q = %#v", prompt, response)
		}
	}
	if got := app.ThreadStartCalls(); got != 0 {
		t.Fatalf("ThreadStart calls = %d, want 0", got)
	}
	if calls := app.turnCalls(); len(calls) != 0 {
		t.Fatalf("TurnStart before terminal refresh = %#v, want none", calls)
	}

	app.threadReads = map[string]map[string]any{
		thread.ID: threadReadPayload(thread.ID, "Active bound", project.CanonicalPath, "active", "completed", "done"),
	}
	svc.handleLiveEvent(ctx, app, appserver.Event{Method: "turn/completed", Params: map[string]any{"threadId": thread.ID}})
	if calls := app.turnCalls(); len(calls) != 1 || calls[0].threadID != thread.ID || calls[0].message != "first plain" {
		t.Fatalf("first drain = %#v", calls)
	}
	setTerminal(t, svc, thread)
	if err := svc.minimalRouter.DrainNext(ctx, thread.ID); err != nil {
		t.Fatal(err)
	}
	if calls := app.turnCalls(); len(calls) != 2 || calls[1].threadID != thread.ID || calls[1].message != "second plain" {
		t.Fatalf("second drain = %#v", calls)
	}
}

func TestMinimalRouterFailedStartContinuesFIFOWithSafeSilentError(t *testing.T) {
	svc, _ := newMinimalService(t)
	app := newRouterSession()
	app.fail["private first"] = errors.New("backend private first")
	useRouterSession(svc, app)
	sender := &recordingSender{}
	svc.SetSender(sender)
	thread := model.Thread{ID: "fail", CWD: svc.cfg.Projects[0].CanonicalPath, Status: "inProgress", ActiveTurnID: "active"}
	seedReply(t, svc, 7, 505, thread)
	app.threadReads = map[string]map[string]any{thread.ID: activeThreadReadPayload(thread)}
	for _, prompt := range []string{"private first", "safe second"} {
		submitMinimal(t, svc, model.InboundText{ChatID: 7, UserID: 7, ReplyToMessageID: 505, Text: prompt, ReceivedAt: svc.now()})
	}
	setTerminal(t, svc, thread)
	if err := svc.store.SetGlobalObserverTarget(context.Background(), 7, 0, true); err != nil {
		t.Fatal(err)
	}
	if err := svc.minimalRouter.DrainNext(context.Background(), thread.ID); err != nil {
		t.Fatal(err)
	}
	calls := app.turnCalls()
	if len(calls) != 2 || calls[1].message != "safe second" {
		t.Fatalf("starts = %#v", calls)
	}
	if len(sender.messages) != 1 || !sender.messages[0].options.Silent || strings.Contains(sender.messages[0].text, "private") || strings.Contains(sender.messages[0].text, thread.ID) {
		t.Fatalf("unsafe notification = %#v", sender.messages)
	}
}

func TestFailedStartWithoutObserverNotifiesCommandOriginSilently(t *testing.T) {
	svc, _ := newMinimalService(t)
	app := newRouterSession()
	app.fail["origin-failure"] = errors.New("remote failure with private detail")
	useRouterSession(svc, app)
	sender := &recordingSender{}
	svc.SetSender(sender)
	thread := model.Thread{ID: "origin-fallback", CWD: svc.cfg.Projects[0].CanonicalPath, Status: "completed"}
	if err := svc.store.UpsertThread(context.Background(), thread); err != nil {
		t.Fatal(err)
	}
	if err := svc.store.EnqueuePendingCommand(context.Background(), model.PendingCommand{
		ThreadID: "origin-fallback", ProjectID: "bridge", ChatID: 321, TopicID: 44, Prompt: "origin-failure",
	}); err != nil {
		t.Fatal(err)
	}
	if err := svc.minimalRouter.DrainNext(context.Background(), thread.ID); err != nil {
		t.Fatal(err)
	}
	if len(sender.messages) != 1 {
		t.Fatalf("origin failure messages = %#v", sender.messages)
	}
	message := sender.messages[0]
	if message.chatID != 321 || message.topicID != 44 || !message.options.Silent {
		t.Fatalf("origin failure route = %#v", message)
	}
	if strings.Contains(message.text, "origin-failure") || strings.Contains(message.text, "private detail") {
		t.Fatalf("origin failure notification leaks content: %q", message.text)
	}
}

func TestRemoteTurnStartSuccessWithLocalPersistFailureStopsFIFO(t *testing.T) {
	svc, _ := newMinimalService(t)
	app := newRouterSession()
	useRouterSession(svc, app)
	thread := model.Thread{ID: "remote-success-local-fail", CWD: svc.cfg.Projects[0].CanonicalPath, Status: "completed"}
	if err := svc.store.UpsertThread(context.Background(), thread); err != nil {
		t.Fatal(err)
	}
	for _, prompt := range []string{"remote-success-first", "must-not-start-second"} {
		if err := svc.store.EnqueuePendingCommand(context.Background(), model.PendingCommand{ThreadID: thread.ID, ProjectID: "bridge", Prompt: prompt}); err != nil {
			t.Fatal(err)
		}
	}
	svc.minimalRouter.persistThread = func(context.Context, model.Thread) error {
		return errors.New("injected local thread persistence failure")
	}
	err := svc.minimalRouter.DrainNext(context.Background(), thread.ID)
	if err == nil {
		t.Fatal("DrainNext succeeded after remote start/local persistence failure")
	}
	if calls := app.turnCalls(); len(calls) != 1 || calls[0].message != "remote-success-first" {
		t.Fatalf("remote-success FIFO calls = %#v", calls)
	}
}

func TestFailedStartFinalizationErrorHaltsFIFO(t *testing.T) {
	svc, _ := newMinimalService(t)
	app := newRouterSession()
	app.fail["cannot-start-first"] = errors.New(`remote start failed at C:\private\workspace`)
	useRouterSession(svc, app)
	sender := &recordingSender{}
	svc.SetSender(sender)
	thread := model.Thread{ID: "fail-finalization", CWD: svc.cfg.Projects[0].CanonicalPath, Status: "completed"}
	if err := svc.store.UpsertThread(context.Background(), thread); err != nil {
		t.Fatal(err)
	}
	for _, prompt := range []string{"cannot-start-first", "must-remain-pending"} {
		if err := svc.store.EnqueuePendingCommand(context.Background(), model.PendingCommand{
			ThreadID: thread.ID, ProjectID: "bridge", ChatID: 654, TopicID: 32, Prompt: prompt,
		}); err != nil {
			t.Fatal(err)
		}
	}
	svc.minimalRouter.failPending = func(context.Context, int64) error {
		return errors.New("injected failed-row finalization error")
	}
	err := svc.minimalRouter.DrainNext(context.Background(), thread.ID)
	if err == nil {
		t.Fatal("DrainNext ignored failed-row finalization error")
	}
	if !strings.Contains(err.Error(), "injected failed-row finalization error") {
		t.Fatalf("DrainNext error = %q", err)
	}
	if calls := app.turnCalls(); len(calls) != 1 || calls[0].message != "cannot-start-first" {
		t.Fatalf("finalization failure starts = %#v", calls)
	}
	if len(sender.messages) != 1 {
		t.Fatalf("finalization failure messages = %#v", sender.messages)
	}
	message := sender.messages[0]
	if message.chatID != 654 || message.topicID != 32 || !message.options.Silent {
		t.Fatalf("finalization failure notification route = %#v", message)
	}
	if strings.TrimSpace(message.text) == "" {
		t.Fatal("finalization failure notification is empty")
	}
	for _, private := range []string{"cannot-start-first", "remote start failed", `C:\private\workspace`, "injected failed-row finalization error"} {
		if strings.Contains(message.text, private) {
			t.Fatalf("finalization failure notification leaks %q: %q", private, message.text)
		}
	}
	next, err := svc.store.ClaimPendingCommand(context.Background(), thread.ID)
	if err != nil {
		t.Fatal(err)
	}
	if next != nil {
		t.Fatalf("later command became claimable after finalization error: %#v", next)
	}
}

func TestSuccessfulStartCompletionErrorHaltsFIFO(t *testing.T) {
	svc, _ := newMinimalService(t)
	app := newRouterSession()
	useRouterSession(svc, app)
	thread := model.Thread{ID: "complete-finalization", CWD: svc.cfg.Projects[0].CanonicalPath, Status: "completed"}
	if err := svc.store.UpsertThread(context.Background(), thread); err != nil {
		t.Fatal(err)
	}
	for _, prompt := range []string{"remote-start-completes", "must-not-run-after-complete-error"} {
		if err := svc.store.EnqueuePendingCommand(context.Background(), model.PendingCommand{ThreadID: thread.ID, ProjectID: "bridge", Prompt: prompt}); err != nil {
			t.Fatal(err)
		}
	}
	svc.minimalRouter.completePending = func(context.Context, int64) error {
		return errors.New("injected completed-row finalization error")
	}
	err := svc.minimalRouter.DrainNext(context.Background(), thread.ID)
	if err == nil {
		t.Fatal("DrainNext ignored completed-row finalization error")
	}
	if calls := app.turnCalls(); len(calls) != 1 || calls[0].message != "remote-start-completes" {
		t.Fatalf("completion failure starts = %#v", calls)
	}
}

func TestMinimalServiceStartupRecoversAbandonedClaimWithoutReplay(t *testing.T) {
	svc, _ := newMinimalService(t)
	ctx := context.Background()
	for _, prompt := range []string{"ambiguous-before-restart", "safe-after-restart"} {
		if err := svc.store.EnqueuePendingCommand(ctx, model.PendingCommand{ThreadID: "startup-recovery", ProjectID: "bridge", Prompt: prompt}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := svc.store.ClaimPendingCommand(ctx, "startup-recovery"); err != nil {
		t.Fatal(err)
	}
	if err := svc.Close(); err != nil {
		t.Fatal(err)
	}
	restarted, err := New(svc.cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = restarted.Close() })
	next, err := restarted.store.ClaimPendingCommand(ctx, "startup-recovery")
	if err != nil {
		t.Fatal(err)
	}
	if next == nil || next.Prompt != "safe-after-restart" {
		t.Fatalf("post-startup claim = %#v", next)
	}
}

func TestMinimalServiceStartupRecoversCreatingContinuationBeforeGenericClaimedCleanup(t *testing.T) {
	svc, _ := newMinimalService(t)
	ctx := context.Background()
	parent := model.Thread{ID: "startup-parent", CWD: svc.cfg.Projects[0].CanonicalPath, Status: "completed"}
	if err := svc.store.UpsertThread(ctx, parent); err != nil {
		t.Fatal(err)
	}
	key := model.MinimalContinuationKey{ChatID: 100, SourceThreadID: parent.ID, SourceTurnID: "turn-9"}
	if _, created, err := svc.store.ClaimMinimalContinuation(ctx, model.MinimalContinuation{Key: key, ProjectID: "bridge"}); err != nil || !created {
		t.Fatalf("creating continuation created=%t err=%v", created, err)
	}
	for _, prompt := range []string{"first survives restart", "second remains FIFO"} {
		if err := svc.store.EnqueuePendingCommand(ctx, model.PendingCommand{
			ThreadID: parent.ID, SourceThreadID: parent.ID, SourceTurnID: "turn-9",
			ProjectID: "bridge", ChatID: 100, Prompt: prompt,
		}); err != nil {
			t.Fatal(err)
		}
	}
	claimed, err := svc.store.ClaimPendingCommand(ctx, parent.ID)
	if err != nil || claimed == nil || claimed.Prompt != "first survives restart" {
		t.Fatalf("claimed before restart=%#v err=%v", claimed, err)
	}
	if err := svc.Close(); err != nil {
		t.Fatal(err)
	}
	restarted, err := New(svc.cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = restarted.Close() })
	app := newRouterSession()
	useRouterSession(restarted, app)
	app.resumeErrByThread = map[string]error{parent.ID: &appserver.RPCError{Code: -32600, Message: "thread " + parent.ID + " already has an active writer"}}
	app.forkIDs = []string{"child-after-repair"}
	first, err := restarted.store.ClaimPendingCommand(ctx, parent.ID)
	if err != nil || first == nil || first.ID != claimed.ID || first.Prompt != "first survives restart" {
		t.Fatalf("first after restart=%#v err=%v", first, err)
	}
	if err := restarted.store.ReleaseClaimedPendingCommand(ctx, first.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := restarted.handleCommand(ctx, 100, 0, "/repair", 0); err != nil {
		t.Fatal(err)
	}
	if err := restarted.minimalRouter.DrainNext(ctx, parent.ID); err != nil {
		t.Fatal(err)
	}
	calls := app.turnCalls()
	if app.forkCallCount() != 1 || len(calls) != 1 || calls[0].threadID != "child-after-repair" || calls[0].message != "first survives restart" {
		t.Fatalf("forks=%d calls=%#v, want first prompt retried after repair", app.forkCallCount(), calls)
	}
	releaseLinkedWorkerForTest(t, restarted, 100, 0, parent.ID)
	child := model.Thread{ID: "child-after-repair", CWD: parent.CWD, Status: "completed"}
	if err := restarted.store.UpsertThread(ctx, child); err != nil {
		t.Fatal(err)
	}
	if err := restarted.minimalRouter.DrainNext(ctx, child.ID); err != nil {
		t.Fatal(err)
	}
	calls = app.turnCalls()
	if len(calls) != 2 || calls[1].threadID != child.ID || calls[1].message != "second remains FIFO" {
		t.Fatalf("calls=%#v, want second prompt preserved in FIFO", calls)
	}
}

func TestMinimalTerminalLiveEventDrainsQueuedCommand(t *testing.T) {
	svc, _ := newMinimalService(t)
	app := newRouterSession()
	useRouterSession(svc, app)
	thread := model.Thread{ID: "live", CWD: svc.cfg.Projects[0].CanonicalPath, Status: "inProgress", ActiveTurnID: "active"}
	seedReply(t, svc, 100, 506, thread)
	app.threadReads = map[string]map[string]any{thread.ID: activeThreadReadPayload(thread)}
	submitMinimal(t, svc, model.InboundText{ChatID: 100, UserID: 7, ReplyToMessageID: 506, Text: "after terminal", ReceivedAt: svc.now()})
	app.threadReads = map[string]map[string]any{thread.ID: {"thread": map[string]any{"id": thread.ID, "cwd": thread.CWD, "status": "completed", "turns": []any{map[string]any{"id": "active", "status": "completed"}}}}}
	svc.handleLiveEvent(context.Background(), app, appserver.Event{Method: "turn/completed", Params: map[string]any{"threadId": thread.ID}})
	if calls := app.turnCalls(); len(calls) != 1 || calls[0].message != "after terminal" {
		t.Fatalf("terminal starts = %#v", calls)
	}
}

func TestDuplicateStaleTerminalRefreshDoesNotStartNextQueuedCommand(t *testing.T) {
	svc, _ := newMinimalService(t)
	app := newRouterSession()
	useRouterSession(svc, app)
	thread := model.Thread{ID: "stale-terminal", CWD: svc.cfg.Projects[0].CanonicalPath, Status: "inProgress", ActiveTurnID: "old-active"}
	if err := svc.store.UpsertThread(context.Background(), thread); err != nil {
		t.Fatal(err)
	}
	for _, prompt := range []string{"first-after-terminal", "second-after-terminal"} {
		if err := svc.store.EnqueuePendingCommand(context.Background(), model.PendingCommand{ThreadID: thread.ID, ProjectID: "bridge", Prompt: prompt}); err != nil {
			t.Fatal(err)
		}
	}
	app.threadReads = map[string]map[string]any{thread.ID: {"thread": map[string]any{
		"id": thread.ID, "cwd": thread.CWD, "status": "completed",
		"turns": []any{map[string]any{"id": "old-active", "status": "completed"}},
	}}}
	event := appserver.Event{Method: "turn/completed", Params: map[string]any{"threadId": thread.ID}}
	svc.handleLiveEvent(context.Background(), app, event)
	svc.handleLiveEvent(context.Background(), app, event)
	if calls := app.turnCalls(); len(calls) != 1 || calls[0].message != "first-after-terminal" {
		t.Fatalf("duplicate terminal starts = %#v", calls)
	}
}

func submitMinimal(t *testing.T, svc *Service, inbound model.InboundText) *DirectResponse {
	t.Helper()
	response, err := svc.HandleInboundText(context.Background(), inbound)
	if err != nil {
		t.Fatal(err)
	}
	if response == nil {
		t.Fatal("nil response")
	}
	return response
}
func mustSelect(t *testing.T, svc *Service, chatID int64, projectID string) {
	t.Helper()
	if err := svc.store.SetSelectedProject(context.Background(), chatID, 0, projectID); err != nil {
		t.Fatal(err)
	}
}
func seedReply(t *testing.T, svc *Service, chatID, messageID int64, thread model.Thread) {
	t.Helper()
	if err := svc.store.UpsertThread(context.Background(), thread); err != nil {
		t.Fatal(err)
	}
	if err := svc.store.PutMessageRoute(context.Background(), model.MessageRoute{ChatID: chatID, MessageID: messageID, ThreadID: thread.ID, TurnID: thread.ActiveTurnID, CreatedAt: model.NowString()}); err != nil {
		t.Fatal(err)
	}
}
func setTerminal(t *testing.T, svc *Service, thread model.Thread) {
	t.Helper()
	thread.Status, thread.ActiveTurnID = "completed", ""
	if err := svc.store.UpsertThread(context.Background(), thread); err != nil {
		t.Fatal(err)
	}
}
func activeThreadReadPayload(thread model.Thread) map[string]any {
	return map[string]any{"thread": map[string]any{
		"id": thread.ID, "cwd": thread.CWD, "status": "active", "activeTurnId": thread.ActiveTurnID,
		"turns": []any{map[string]any{"id": thread.ActiveTurnID, "status": "inProgress"}},
	}}
}
func createMinimalPickerRoute(t *testing.T, svc *Service, chatID, topicID int64, action, projectID string) string {
	t.Helper()
	button, err := svc.minimalPickerButton(context.Background(), chatID, topicID, "pick", action, projectID, "", 0)
	if err != nil {
		t.Fatal(err)
	}
	return button.CallbackData
}
func assertNotDone(t *testing.T, done <-chan error, message string) {
	t.Helper()
	select {
	case err := <-done:
		t.Fatalf("%s: err=%v", message, err)
	case <-time.After(100 * time.Millisecond):
	}
}
func useRouterSession(svc *Service, app *routerSession) {
	svc.mu.Lock()
	svc.live, svc.liveConnected = app, true
	svc.mu.Unlock()
	if app == nil {
		svc.minimalWorkerFactory = func() continuationSession { return nil }
	} else {
		svc.minimalWorkerFactory = func() continuationSession { return app }
	}
	svc.minimalWorkers = newMinimalLinkWorkerManager(svc.minimalWorkerFactory, time.Second, svc.logLifecycle)
}

type routerWorkerFactory struct {
	mu       sync.Mutex
	sessions []*routerSession
	config   func(int, *routerSession)
}

func installWorkerFactory(svc *Service, configs ...func(int, *routerSession)) *routerWorkerFactory {
	factory := &routerWorkerFactory{}
	if len(configs) > 0 {
		factory.config = configs[0]
	}
	svc.minimalWorkerFactory = factory.New
	svc.minimalWorkers = newMinimalLinkWorkerManager(factory.New, time.Second, svc.logLifecycle)
	return factory
}

func (f *routerWorkerFactory) New() continuationSession {
	f.mu.Lock()
	defer f.mu.Unlock()
	session := newRouterSession()
	session.events = make(chan appserver.Event, 1)
	session.recordNameCalls = true
	index := len(f.sessions)
	if f.config != nil {
		f.config(index, session)
	}
	f.sessions = append(f.sessions, session)
	return session
}

func (f *routerWorkerFactory) Sessions() []*routerSession {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]*routerSession, len(f.sessions))
	copy(out, f.sessions)
	return out
}

func (f *routerWorkerFactory) Single(t *testing.T) *routerSession {
	t.Helper()
	sessions := f.Sessions()
	if len(sessions) != 1 {
		t.Fatalf("worker sessions=%d, want 1", len(sessions))
	}
	return sessions[0]
}

func (f *routerWorkerFactory) Session(t *testing.T, index int) *routerSession {
	t.Helper()
	sessions := f.Sessions()
	if index < 0 || index >= len(sessions) {
		t.Fatalf("worker session index %d out of %d", index, len(sessions))
	}
	return sessions[index]
}

func seedPCSource(t *testing.T, svc *Service, live *routerSession, sourceID, sourceTurnID, title string) {
	t.Helper()
	ctx := context.Background()
	project := svc.cfg.Projects[0]
	if err := svc.store.SetSelectedProject(ctx, 7, 0, project.ID); err != nil {
		t.Fatal(err)
	}
	parent := model.Thread{ID: sourceID, CWD: project.CanonicalPath, Status: "completed", Title: title}
	if err := svc.store.UpsertThread(ctx, parent); err != nil {
		t.Fatal(err)
	}
	if err := svc.store.SetBinding(ctx, 7, 0, parent.ID, model.BindingModeBound); err != nil {
		t.Fatal(err)
	}
	if err := svc.store.PutMessageRoute(ctx, model.MessageRoute{ChatID: 7, MessageID: 501, ThreadID: parent.ID, TurnID: sourceTurnID, CreatedAt: model.NowString()}); err != nil {
		t.Fatal(err)
	}
	live.threadReads = map[string]map[string]any{parent.ID: {"thread": map[string]any{
		"id": parent.ID, "title": title, "cwd": parent.CWD, "status": "completed", "source": "vscode", "originator": "Codex Desktop",
		"turns": []any{map[string]any{"id": sourceTurnID, "status": "completed"}},
	}}}
}

func releaseLinkedWorkerForTest(t *testing.T, svc *Service, chatID, topicID int64, sourceID string) {
	t.Helper()
	ctx := context.Background()
	link, err := svc.store.GetMinimalLinkedThread(ctx, chatID, topicID, sourceID)
	if err != nil {
		t.Fatal(err)
	}
	if link == nil {
		t.Fatalf("linked thread for source %q is missing", sourceID)
	}
	if link.ActiveTurnID == "" {
		t.Fatalf("linked thread has no active turn to release: %#v", link)
	}
	if changed, err := svc.store.BeginMinimalLinkedRelease(ctx, model.MinimalLinkedRelease{LinkedThreadID: link.LinkedThreadID, TurnID: link.ActiveTurnID, WorkerGeneration: link.WorkerGeneration}); err != nil || !changed {
		t.Fatalf("begin linked release changed=%t err=%v", changed, err)
	}
	if changed, err := svc.store.FinishMinimalLinkedRelease(ctx, link.LinkedThreadID, link.WorkerGeneration, svc.now()); err != nil || !changed {
		t.Fatalf("finish linked release changed=%t err=%v", changed, err)
	}
	if closed, err := svc.minimalWorkers.Release(ctx, link.LinkedThreadID, link.WorkerGeneration); err != nil || !closed {
		t.Fatalf("release linked worker closed=%t err=%v", closed, err)
	}
}

func seedReadyLinkedThread(t *testing.T, svc *Service, chatID, topicID int64, sourceID, sourceTurnID, linkedID string) {
	t.Helper()
	ctx := context.Background()
	project := svc.cfg.Projects[0]
	source := model.Thread{ID: sourceID, CWD: project.CanonicalPath, Status: "completed", Title: "안클코"}
	if err := svc.store.UpsertThread(ctx, source); err != nil {
		t.Fatal(err)
	}
	link := model.MinimalLinkedThread{
		ChatKey:            model.ChatKey(chatID, topicID),
		ChatID:             chatID,
		TopicID:            topicID,
		ProjectID:          project.ID,
		SourceThreadID:     sourceID,
		LinkedThreadID:     linkedID,
		SourceAnchorTurnID: sourceTurnID,
		SourceTitle:        "안클코",
		DesiredTitle:       "안클코 · 텔레그램 연동",
		TitleState:         model.MinimalLinkedTitleSet,
		State:              model.MinimalLinkedTelegramRunning,
		WorkerGeneration:   1,
	}
	provenance := model.MinimalContinuation{
		Key: model.MinimalContinuationKey{ChatID: chatID, TopicID: topicID, SourceThreadID: sourceID, SourceTurnID: sourceTurnID},
	}
	child := model.Thread{ID: linkedID, CWD: project.CanonicalPath, Status: "completed"}
	if err := svc.store.ActivateMinimalLinkedThread(ctx, link, provenance, child); err != nil {
		t.Fatal(err)
	}
	if changed, err := svc.store.MarkMinimalLinkedTurnStarted(ctx, linkedID, 1, "linked-turn-0"); err != nil || !changed {
		t.Fatalf("mark linked turn changed=%t err=%v", changed, err)
	}
	if changed, err := svc.store.BeginMinimalLinkedRelease(ctx, model.MinimalLinkedRelease{LinkedThreadID: linkedID, TurnID: "linked-turn-0", WorkerGeneration: 1}); err != nil || !changed {
		t.Fatalf("begin ready seed release changed=%t err=%v", changed, err)
	}
	if changed, err := svc.store.FinishMinimalLinkedRelease(ctx, linkedID, 1, svc.now()); err != nil || !changed {
		t.Fatalf("finish ready seed release changed=%t err=%v", changed, err)
	}
}

func assertRouterSessionNoMutations(t *testing.T, session *routerSession) {
	t.Helper()
	if got := session.callSequence(); len(got) != 0 {
		t.Fatalf("global live session mutated: %v", got)
	}
	if got := session.ThreadSetNameCalls(); got != 0 {
		t.Fatalf("global live session renamed a thread: %d calls", got)
	}
}

type routerCall struct {
	threadID, message, cwd, approval, sandbox string
	roots                                     []string
	network                                   bool
}
type routerSession struct {
	stubSession
	mu                sync.Mutex
	threadIDs         []string
	threadN, turnN    int
	startCWDs         []string
	calls             []routerCall
	fail              map[string]error
	startHook         func() error
	readGate          chan struct{}
	readCount         int
	readBarrier       bool
	resumeErrByThread map[string]error
	resumeHook        func(threadID string) error
	threadReadSeq     map[string][]map[string]any
	readHook          func(threadID string)
	forkIDs           []string
	forkErrs          []error
	forkPayloadCWD    string
	forkCalls         []routerForkCall
	setNameHook       func(threadID, name string) error
	turnStartHook     func(threadID, message, turnID string)
	sequence          []string
	events            chan appserver.Event
	closeCalls        int
	setNameCalls      int
	recordNameCalls   bool
}

func newRouterSession(ids ...string) *routerSession {
	return &routerSession{threadIDs: ids, fail: map[string]error{}}
}

func (s *routerSession) Subscribe() <-chan appserver.Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.events == nil {
		s.events = make(chan appserver.Event, 1)
	}
	return s.events
}

func (s *routerSession) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closeCalls++
	return nil
}

func (s *routerSession) enableTwoReadBarrier() {
	s.readBarrier = true
	s.readGate = make(chan struct{})
}

func (s *routerSession) ThreadRead(ctx context.Context, threadID string, includeTurns bool) (map[string]any, error) {
	s.mu.Lock()
	payload := s.threadReads[threadID]
	if len(s.threadReadSeq[threadID]) > 0 {
		payload = s.threadReadSeq[threadID][0]
		s.threadReadSeq[threadID] = s.threadReadSeq[threadID][1:]
	}
	err := s.threadReadErr
	gate := s.readGate
	hook := s.readHook
	if s.readBarrier {
		s.readCount++
		if s.readCount == 2 {
			close(gate)
		}
	}
	s.mu.Unlock()
	if gate != nil {
		select {
		case <-gate:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if hook != nil {
		hook(threadID)
	}
	if err != nil {
		return nil, err
	}
	return payload, nil
}
func (s *routerSession) ThreadStart(ctx context.Context, cwd string) (map[string]any, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sequence = append(s.sequence, "thread/start")
	if s.startHook != nil {
		if err := s.startHook(); err != nil {
			return nil, err
		}
	}
	s.startCWDs = append(s.startCWDs, cwd)
	id := "reply-thread"
	if s.threadN < len(s.threadIDs) {
		id = s.threadIDs[s.threadN]
	}
	s.threadN++
	return map[string]any{"thread": map[string]any{"id": id, "cwd": cwd, "status": "idle"}}, nil
}
func (s *routerSession) ThreadResume(_ context.Context, threadID, cwd string) (map[string]any, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sequence = append(s.sequence, "thread/resume:"+threadID)
	s.threadResumeCalls = append(s.threadResumeCalls, threadResumeCall{threadID: threadID, cwd: cwd})
	if s.resumeHook != nil {
		if err := s.resumeHook(threadID); err != nil {
			return nil, err
		}
	}
	if err := s.resumeErrByThread[threadID]; err != nil {
		return nil, err
	}
	return map[string]any{"thread": map[string]any{"id": threadID, "cwd": cwd}}, nil
}

type routerForkCall struct {
	threadID string
	options  control.ThreadForkOptions
}

func (s *routerSession) ThreadFork(_ context.Context, threadID string, options control.ThreadForkOptions) (map[string]any, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.forkCalls = append(s.forkCalls, routerForkCall{threadID: threadID, options: options})
	s.sequence = append(s.sequence, "thread/fork:"+threadID+":"+options.LastTurnID)
	if len(s.forkCalls) <= len(s.forkErrs) && s.forkErrs[len(s.forkCalls)-1] != nil {
		return nil, s.forkErrs[len(s.forkCalls)-1]
	}
	id := ""
	if len(s.forkCalls) <= len(s.forkIDs) {
		id = s.forkIDs[len(s.forkCalls)-1]
	}
	cwd := options.CWD
	if strings.TrimSpace(s.forkPayloadCWD) != "" {
		cwd = s.forkPayloadCWD
	}
	return map[string]any{"thread": map[string]any{"id": id, "cwd": cwd, "status": "idle"}}, nil
}
func (s *routerSession) ThreadSetName(_ context.Context, threadID, name string) (map[string]any, error) {
	s.mu.Lock()
	s.setNameCalls++
	if s.recordNameCalls {
		s.sequence = append(s.sequence, "thread/name/set:"+threadID)
	}
	hook := s.setNameHook
	s.mu.Unlock()
	if hook != nil {
		if err := hook(threadID, name); err != nil {
			return nil, err
		}
	}
	return map[string]any{}, nil
}
func (s *routerSession) TurnStart(ctx context.Context, threadID, message, cwd string, options appserver.TurnStartOptions) (map[string]any, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sequence = append(s.sequence, "turn/start:"+threadID)
	s.calls = append(s.calls, routerCall{threadID: threadID, message: message, cwd: cwd, approval: options.ApprovalPolicy, sandbox: options.SandboxPolicy.Type, roots: append([]string(nil), options.SandboxPolicy.WritableRoots...), network: options.SandboxPolicy.NetworkAccess})
	if err := s.fail[message]; err != nil {
		return nil, err
	}
	s.turnN++
	turnID := "turn-" + string(rune('0'+s.turnN))
	if s.turnStartHook != nil {
		s.turnStartHook(threadID, message, turnID)
	}
	return map[string]any{"turn": map[string]any{"id": turnID}}, nil
}
func (s *routerSession) turnCalls() []routerCall {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]routerCall(nil), s.calls...)
}
func (s *routerSession) forkCallCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.forkCalls)
}
func (s *routerSession) callSequence() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.sequence...)
}
func (s *routerSession) ThreadStartCalls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.threadN
}
func (s *routerSession) ThreadResumeCalls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.threadResumeCalls)
}
func (s *routerSession) ThreadSetNameCalls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.setNameCalls
}
func (s *routerSession) Closed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closeCalls > 0
}
func (s *routerSession) threadStartCWDs() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.startCWDs...)
}

type blockingEditSender struct {
	recordingSender
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func newBlockingEditSender() *blockingEditSender {
	return &blockingEditSender{entered: make(chan struct{}), release: make(chan struct{})}
}

func (s *blockingEditSender) EditMessage(ctx context.Context, chatID, topicID, messageID int64, text string, buttons [][]model.ButtonSpec) error {
	s.once.Do(func() {
		close(s.entered)
	})
	select {
	case <-s.release:
	case <-ctx.Done():
		return ctx.Err()
	}
	return s.recordingSender.EditMessage(ctx, chatID, topicID, messageID, text, buttons)
}
