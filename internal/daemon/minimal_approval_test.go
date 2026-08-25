package daemon

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/alclssna33/codex_to_telegram/internal/appserver"
	"github.com/alclssna33/codex_to_telegram/internal/control"
	"github.com/alclssna33/codex_to_telegram/internal/model"
	"github.com/alclssna33/codex_to_telegram/internal/storage"
)

func TestMinimalApprovalPresentationIsAudibleBoundAndContentMinimized(t *testing.T) {
	svc, app, sender := newApprovalTestService(t)
	raw := `powershell -Command "Set-Content C:\private\approval-marker.txt ok"`
	var logs bytes.Buffer
	svc.logger = log.New(&logs, "", 0)

	handleApprovalEvent(svc, app, approvalEvent("req-present", "item/commandExecution/requestApproval", map[string]any{
		"command": raw,
		"cwd":     `C:\untrusted\event-cwd`,
	}))
	if pending, err := svc.store.GetPendingApproval(context.Background(), "req-present"); err != nil || pending != nil {
		t.Fatalf("legacy pending approval = %#v, %v; minimal interception must precede generic persistence", pending, err)
	}
	if got := len(sender.messages); got != 0 {
		t.Fatalf("approval sent before durable queue processing: %d messages", got)
	}

	svc.processDeliveryBatch(context.Background())
	if got := len(sender.messages); got != 1 {
		t.Fatalf("messages = %d, want 1", got)
	}
	message := sender.messages[0]
	if message.messageID != 600 || message.options.Silent {
		t.Fatalf("message id/options = %d/%#v, want audible id 600", message.messageID, message.options)
	}
	if got, want := buttonLabels(message.buttons), [][]string{{"승인", "거부"}}; !equalStringRows(got, want) {
		t.Fatalf("buttons = %#v, want %#v", got, want)
	}
	for _, wanted := range []string{"프로젝트: Bridge", "요청: 명령 실행", raw} {
		if !strings.Contains(message.text, wanted) {
			t.Fatalf("approval text %q missing %q", message.text, wanted)
		}
	}
	for _, forbidden := range []string{"req-present", "thr-1", "turn-1", `C:\untrusted\event-cwd`} {
		if strings.Contains(message.text, forbidden) {
			t.Fatalf("approval text leaked internal/untrusted value %q: %q", forbidden, message.text)
		}
	}
	if strings.Contains(logs.String(), raw) || strings.Contains(logs.String(), `C:\private\approval-marker.txt`) {
		t.Fatalf("diagnostics leaked raw command/path: %q", logs.String())
	}
	approval, _ := svc.store.GetMinimalApproval(context.Background(), "req-present")
	if approval == nil || approval.TelegramMessageID != 600 || approval.ChatID != 7 || approval.RequestKind != "item/commandExecution/requestApproval" {
		t.Fatalf("bound approval = %#v", approval)
	}
}

func TestApprovalCallbackCanResolveOnlyOnceAndRequiresExactMessage(t *testing.T) {
	svc, app, sender := newApprovalTestService(t)
	approve, _ := presentApproval(t, svc, app, sender, "req-once", "item/commandExecution/requestApproval")

	if _, err := svc.HandleCallback(context.Background(), 7, 0, 600, 8, approve); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("wrong-user error = %v, want ErrUnauthorized", err)
	}
	if _, err := svc.HandleCallback(context.Background(), 7, 0, 601, 7, approve); !errors.Is(err, ErrCallbackMismatch) {
		t.Fatalf("wrong-message error = %v, want ErrCallbackMismatch", err)
	}
	if got := app.responseCount("req-once"); got != 0 {
		t.Fatalf("responses before exact callback = %d", got)
	}
	response, err := svc.HandleCallback(context.Background(), 7, 0, 600, 7, approve)
	if err != nil {
		t.Fatal(err)
	}
	if response == nil || response.CallbackText != "승인됨" {
		t.Fatalf("response = %#v", response)
	}
	if _, err := svc.HandleCallback(context.Background(), 7, 0, 600, 7, approve); !errors.Is(err, ErrCallbackConsumed) {
		t.Fatalf("second error = %v, want ErrCallbackConsumed", err)
	}
	if got := app.responseCount("req-once"); got != 1 {
		t.Fatalf("responses = %d, want 1", got)
	}
	if len(sender.edits) != 1 || sender.edits[0].text != "승인됨" || len(sender.edits[0].buttons) != 0 {
		t.Fatalf("resolved edits = %#v", sender.edits)
	}
}

func TestMinimalApprovalUsesExactPerKindResponseMaps(t *testing.T) {
	tests := []struct {
		name     string
		kind     string
		action   string
		wantText string
		want     map[string]any
	}{
		{name: "command approve", kind: "item/commandExecution/requestApproval", action: "approve", wantText: "승인됨", want: map[string]any{"decision": "accept"}},
		{name: "command deny", kind: "item/commandExecution/requestApproval", action: "deny", wantText: "거부됨", want: map[string]any{"decision": "decline"}},
		{name: "file approve", kind: "item/fileChange/requestApproval", action: "approve", wantText: "승인됨", want: map[string]any{"decision": "accept"}},
		{name: "file deny", kind: "item/fileChange/requestApproval", action: "deny", wantText: "거부됨", want: map[string]any{"decision": "decline"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			svc, app, sender := newApprovalTestService(t)
			approve, deny := presentApproval(t, svc, app, sender, "req-shape", tc.kind)
			token := approve
			if tc.action == "deny" {
				token = deny
			}
			response, err := svc.HandleCallback(context.Background(), 7, 0, 600, 7, token)
			if err != nil {
				t.Fatal(err)
			}
			if response == nil || response.CallbackText != tc.wantText {
				t.Fatalf("response = %#v", response)
			}
			calls := app.responsesFor("req-shape")
			if len(calls) != 1 || !equalStringAnyMap(calls[0].result, tc.want) {
				t.Fatalf("response calls = %#v, want %#v", calls, tc.want)
			}
		})
	}
}

func TestMinimalWorkerApprovalCallbacksUseExactWorkerSession(t *testing.T) {
	tests := []struct {
		name        string
		action      string
		buttonIndex int
		wantText    string
		want        map[string]any
	}{
		{name: "approve", action: "approve", buttonIndex: 0, wantText: "승인됨", want: map[string]any{"decision": "accept"}},
		{name: "deny", action: "deny", buttonIndex: 1, wantText: "거부됨", want: map[string]any{"decision": "decline"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			svc, global, sender := newApprovalTestService(t)
			worker, workerApp := acquireApprovalWorker(t, svc, "linked-worker-"+tc.action, "turn-worker")
			requestID := "req-worker-" + tc.action
			event := approvalEventIdentity(requestID, worker.ThreadID, "turn-worker", minimalCommandApprovalKind, map[string]any{"command": `powershell -Command "Write-Output worker"`})
			workerApp.setCurrentRequest(event)

			svc.handleMinimalLinkedWorkerEvent(context.Background(), worker, event)
			svc.processDeliveryBatch(context.Background())

			approval, err := svc.store.GetMinimalApprovalForSession(context.Background(), requestID, worker.SessionIdentity)
			if err != nil || approval == nil {
				t.Fatalf("worker approval=%#v err=%v", approval, err)
			}
			if approval.SessionIdentity != worker.SessionIdentity {
				t.Errorf("approval session identity=%q, want worker identity %q", approval.SessionIdentity, worker.SessionIdentity)
			}
			if len(sender.messages) != 1 || len(sender.messages[0].buttons) != 1 || len(sender.messages[0].buttons[0]) != 2 {
				t.Fatalf("worker approval presentation=%#v", sender.messages)
			}
			token := sender.messages[0].buttons[0][tc.buttonIndex].CallbackData
			response, err := svc.HandleCallback(context.Background(), 7, 0, 600, 7, token)
			if err != nil {
				t.Fatal(err)
			}
			if response == nil || response.CallbackText != tc.wantText {
				t.Fatalf("callback response=%#v, want %q", response, tc.wantText)
			}
			if got := global.responseCount(requestID); got != 0 {
				t.Fatalf("global live responses=%d, want 0", got)
			}
			calls := workerApp.responsesFor(requestID)
			if len(calls) != 1 || !equalStringAnyMap(calls[0].result, tc.want) {
				t.Fatalf("worker responses=%#v, want one %#v", calls, tc.want)
			}
		})
	}
}

func TestMinimalWorkerApprovalSameWireRequestIDScopesPerWorkerSession(t *testing.T) {
	svc, global, sender := newApprovalTestService(t)
	sender.messageIDs = []int64{601, 602}
	workers, apps := acquireApprovalWorkers(t, svc,
		approvalWorkerSpec{threadID: "linked-worker-a", turnID: "turn-a"},
		approvalWorkerSpec{threadID: "linked-worker-b", turnID: "turn-b"},
	)
	eventA := approvalEventIdentity("1", workers[0].ThreadID, "turn-a", minimalCommandApprovalKind, map[string]any{"command": `powershell -Command "Write-Output A"`})
	eventB := approvalEventIdentity("1", workers[1].ThreadID, "turn-b", minimalCommandApprovalKind, map[string]any{"command": `powershell -Command "Write-Output B"`})
	apps[0].setCurrentRequest(eventA)
	apps[1].setCurrentRequest(eventB)

	svc.handleMinimalLinkedWorkerEvent(context.Background(), workers[0], eventA)
	svc.handleMinimalLinkedWorkerEvent(context.Background(), workers[1], eventB)
	svc.processDeliveryBatch(context.Background())

	if len(sender.messages) != 2 {
		t.Fatalf("worker approval messages=%#v, want two independent presentations", sender.messages)
	}
	approveA := sender.messages[0].buttons[0][0].CallbackData
	denyB := sender.messages[1].buttons[0][1].CallbackData
	routeA, err := svc.store.GetMinimalApprovalRoute(context.Background(), approveA)
	if err != nil || routeA == nil {
		t.Fatalf("route A=%#v err=%v", routeA, err)
	}
	routeB, err := svc.store.GetMinimalApprovalRoute(context.Background(), denyB)
	if err != nil || routeB == nil {
		t.Fatalf("route B=%#v err=%v", routeB, err)
	}
	if routeA.WireRequestID != "1" || routeB.WireRequestID != "1" {
		t.Fatalf("wire ids A/B=%q/%q, want original wire id 1", routeA.WireRequestID, routeB.WireRequestID)
	}
	if routeA.RequestID == "1" || routeB.RequestID == "1" || routeA.RequestID == routeB.RequestID {
		t.Fatalf("durable request ids A/B=%q/%q, want scoped distinct non-wire ids", routeA.RequestID, routeB.RequestID)
	}
	if routeA.SessionIdentity != workers[0].SessionIdentity || routeB.SessionIdentity != workers[1].SessionIdentity {
		t.Fatalf("route identities A/B=%q/%q, want exact workers %q/%q", routeA.SessionIdentity, routeB.SessionIdentity, workers[0].SessionIdentity, workers[1].SessionIdentity)
	}

	if response, err := svc.HandleCallback(context.Background(), 7, 0, 601, 7, approveA); err != nil || response == nil || response.CallbackText != "승인됨" {
		t.Fatalf("approve A response=%#v err=%v, want approval", response, err)
	}
	if response, err := svc.HandleCallback(context.Background(), 7, 0, 602, 7, denyB); err != nil || response == nil || response.CallbackText != "거부됨" {
		t.Fatalf("deny B response=%#v err=%v, want denial", response, err)
	}
	if got := global.responseCount("1"); got != 0 {
		t.Fatalf("global live wire responses=%d, want 0", got)
	}
	if calls := apps[0].responsesFor("1"); len(calls) != 1 || calls[0].result["decision"] != "accept" {
		t.Fatalf("worker A responses=%#v, want accept on wire id 1", calls)
	}
	if calls := apps[1].responsesFor("1"); len(calls) != 1 || calls[0].result["decision"] != "decline" {
		t.Fatalf("worker B responses=%#v, want decline on wire id 1", calls)
	}
}

func TestMinimalWorkerApprovalResolvedWireExpiresOnlyExactWorkerSession(t *testing.T) {
	svc, _, _ := newApprovalTestService(t)
	workers, apps := acquireApprovalWorkers(t, svc,
		approvalWorkerSpec{threadID: "linked-resolve-approval-a", turnID: "turn-a"},
		approvalWorkerSpec{threadID: "linked-resolve-approval-b", turnID: "turn-b"},
	)
	eventA := approvalEventIdentity("1", workers[0].ThreadID, "turn-a", minimalCommandApprovalKind, map[string]any{"command": "A"})
	eventB := approvalEventIdentity("1", workers[1].ThreadID, "turn-b", minimalCommandApprovalKind, map[string]any{"command": "B"})
	apps[0].setCurrentRequest(eventA)
	apps[1].setCurrentRequest(eventB)
	svc.handleMinimalLinkedWorkerEvent(context.Background(), workers[0], eventA)
	svc.handleMinimalLinkedWorkerEvent(context.Background(), workers[1], eventB)

	svc.handleMinimalLinkedWorkerEvent(context.Background(), workers[0], appserver.Event{Channel: "notification", Method: "serverRequest/resolved", Params: map[string]any{"requestId": "1"}})

	approvalA, err := svc.store.GetMinimalApproval(context.Background(), model.ScopedRequestID(workers[0].SessionIdentity, "1"))
	if err != nil || approvalA == nil || approvalA.Status != "expired" {
		t.Fatalf("approval A after resolved=%#v err=%v, want expired", approvalA, err)
	}
	approvalB, err := svc.store.GetMinimalApproval(context.Background(), model.ScopedRequestID(workers[1].SessionIdentity, "1"))
	if err != nil || approvalB == nil || approvalB.Status != "pending" {
		t.Fatalf("approval B after resolved=%#v err=%v, want pending", approvalB, err)
	}
}

func TestMinimalLiveResolvedWireCannotExpireWorkerApproval(t *testing.T) {
	svc, global, _ := newApprovalTestService(t)
	worker, workerApp := acquireApprovalWorker(t, svc, "linked-live-resolve-worker", "turn-worker")
	event := approvalEventIdentity("1", worker.ThreadID, "turn-worker", minimalCommandApprovalKind, map[string]any{"command": "worker"})
	workerApp.setCurrentRequest(event)
	svc.handleMinimalLinkedWorkerEvent(context.Background(), worker, event)

	svc.handleLiveEvent(context.Background(), global, appserver.Event{Channel: "notification", Method: "serverRequest/resolved", Params: map[string]any{"requestId": "1"}})

	approval, err := svc.store.GetMinimalApproval(context.Background(), model.ScopedRequestID(worker.SessionIdentity, "1"))
	if err != nil || approval == nil || approval.Status != "pending" {
		t.Fatalf("worker approval after live resolved=%#v err=%v, want pending", approval, err)
	}
}

func TestMinimalStaleWorkerResolvedWireCannotExpireActiveWorkerApproval(t *testing.T) {
	svc, _, _ := newApprovalTestService(t)
	worker, workerApp := acquireApprovalWorker(t, svc, "linked-stale-resolve-worker", "turn-worker")
	event := approvalEventIdentity("1", worker.ThreadID, "turn-worker", minimalCommandApprovalKind, map[string]any{"command": "worker"})
	workerApp.setCurrentRequest(event)
	svc.handleMinimalLinkedWorkerEvent(context.Background(), worker, event)

	if consumed := svc.handleMinimalApprovalResolved(context.Background(), "minimal-link-worker:stale:1", appserver.Event{Channel: "notification", Method: "serverRequest/resolved", Params: map[string]any{"requestId": "1"}}); consumed {
		t.Fatal("stale worker resolved event consumed active worker approval")
	}
	approval, err := svc.store.GetMinimalApproval(context.Background(), model.ScopedRequestID(worker.SessionIdentity, "1"))
	if err != nil || approval == nil || approval.Status != "pending" {
		t.Fatalf("worker approval after stale resolved=%#v err=%v, want pending", approval, err)
	}
}

func TestMinimalLegacyApprovalResolvedWireStillExpiresEmptyIdentityRow(t *testing.T) {
	svc, app, _ := newApprovalTestService(t)
	db, err := sql.Open("sqlite", svc.store.Path())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	now := string(model.NowString())
	if _, err := db.ExecContext(context.Background(), `
		INSERT INTO minimal_approvals(request_id,wire_request_id,thread_id,turn_id,request_kind,project_name,session_identity,status,claim_state,updated_at)
		VALUES(?,?,?,?,?,?,?,?,?,?)`,
		"legacy-minimal", "legacy-wire", "thr-1", "turn-1", minimalCommandApprovalKind, "Bridge", "", "pending", "idle", now,
	); err != nil {
		t.Fatal(err)
	}
	for _, route := range []struct{ token, action string }{
		{"legacy-minimal-approve", "approve"},
		{"legacy-minimal-deny", "deny"},
	} {
		if _, err := db.ExecContext(context.Background(), `
			INSERT INTO minimal_approval_routes(route_token,action,request_id,thread_id,turn_id,request_kind,status,created_at)
			VALUES(?,?,?,?,?,?,?,?)`,
			route.token, route.action, "legacy-minimal", "thr-1", "turn-1", minimalCommandApprovalKind, "active", now,
		); err != nil {
			t.Fatal(err)
		}
	}

	svc.handleLiveEvent(context.Background(), app, appserver.Event{Channel: "notification", Method: "serverRequest/resolved", Params: map[string]any{"requestId": "legacy-wire"}})

	approval, err := svc.store.GetMinimalApproval(context.Background(), "legacy-minimal")
	if err != nil || approval == nil || approval.Status != "expired" {
		t.Fatalf("legacy approval after live resolved=%#v err=%v, want expired", approval, err)
	}
}

func TestMinimalLiveResolvedWireExpiresExactLiveSessionScopedApproval(t *testing.T) {
	svc, app, _ := newApprovalTestService(t)
	liveIdentity := svc.minimalApprovalSessionIdentity()
	created, err := svc.store.CreateMinimalApproval(context.Background(), storage.MinimalApprovalSeed{
		Approval: storage.MinimalApproval{
			RequestID:       "1",
			WireRequestID:   "1",
			ThreadID:        "thr-1",
			TurnID:          "turn-1",
			RequestKind:     minimalCommandApprovalKind,
			ProjectName:     svc.cfg.Projects[0].DisplayName,
			SessionIdentity: liveIdentity,
			Status:          "pending",
		},
		ApproveToken: "01010101010101010101010101010101",
		DenyToken:    "02020202020202020202020202020202",
		Delivery: model.DeliveryQueueItem{
			EventID:  "approval-live-scoped",
			ChatKey:  model.ChatKey(7, 0),
			ChatID:   7,
			ThreadID: "thr-1",
			Kind:     minimalApprovalQueueKind,
			Status:   model.DeliveryStatusPending,
			PayloadJSON: storage.MustJSON(model.DeliveryPayload{
				Text:     "approval",
				ThreadID: "thr-1",
				TurnID:   "turn-1",
				EventID:  "1",
				Buttons: [][]model.ButtonSpec{{
					{Text: "승인", CallbackData: "01010101010101010101010101010101"},
					{Text: "거부", CallbackData: "02020202020202020202020202020202"},
				}},
			}),
		},
	})
	if err != nil || !created {
		t.Fatalf("create live scoped approval=%v, %v", created, err)
	}

	svc.handleLiveEvent(context.Background(), app, appserver.Event{Channel: "notification", Method: "serverRequest/resolved", Params: map[string]any{"requestId": "1"}})

	approval, err := svc.store.GetMinimalApproval(context.Background(), model.ScopedRequestID(liveIdentity, "1"))
	if err != nil || approval == nil || approval.Status != "expired" {
		t.Fatalf("live scoped approval after resolved=%#v err=%v, want expired", approval, err)
	}
}

func TestMinimalWorkerUserInputChoiceUsesExactWorkerSession(t *testing.T) {
	svc, global, _ := newApprovalTestService(t)
	worker, workerApp := acquireApprovalWorker(t, svc, "linked-input-worker", "turn-input")
	emitWorkerUserInput(t, svc, worker, "req-worker-input", "Use worker")
	pending, err := svc.store.GetLatestPendingApprovalForThread(context.Background(), worker.ThreadID)
	if err != nil || pending == nil {
		t.Fatalf("pending worker input=%#v err=%v", pending, err)
	}
	if pending.SessionIdentity != worker.SessionIdentity {
		t.Errorf("pending session identity=%q, want worker identity %q", pending.SessionIdentity, worker.SessionIdentity)
	}
	prompt := effectivePlanPrompt(pending, nil)
	if prompt == nil {
		t.Fatalf("effective plan prompt for pending input=%#v", pending)
	}
	buttons := svc.planPromptButtons(context.Background(), prompt)
	token := callbackTokenForButton(buttons, "Use worker")
	if token == "" {
		t.Fatalf("rendered input buttons=%#v, want worker choice", buttons)
	}
	route, err := svc.store.GetCallbackRoute(context.Background(), token)
	if err != nil || route == nil {
		t.Fatalf("choice route=%#v err=%v", route, err)
	}
	if route.SessionIdentity != worker.SessionIdentity {
		t.Fatalf("choice route session identity=%q, want worker identity %q", route.SessionIdentity, worker.SessionIdentity)
	}

	callback, err := svc.HandleCallback(context.Background(), 7, 0, 0, 7, token)
	if err != nil {
		t.Fatal(err)
	}
	if callback == nil || callback.CallbackText == "" {
		t.Fatalf("callback=%#v, want acknowledgement", callback)
	}
	if got := global.responseCount("req-worker-input"); got != 0 {
		t.Fatalf("global live responses=%d, want 0", got)
	}
	calls := workerApp.responsesFor("req-worker-input")
	if len(calls) != 1 {
		t.Fatalf("worker input responses=%#v, want one", calls)
	}
	answers, _ := calls[0].result["answers"].(map[string]any)
	choice, _ := answers["choice"].(map[string]any)
	values, _ := choice["answers"].([]string)
	if len(values) != 1 || values[0] != "Use worker" {
		t.Fatalf("worker input payload=%#v, want selected choice", calls[0].result)
	}
}

func TestMinimalWorkerUserInputSameWireRequestIDScopesPerWorkerSession(t *testing.T) {
	svc, global, _ := newApprovalTestService(t)
	workers, apps := acquireApprovalWorkers(t, svc,
		approvalWorkerSpec{threadID: "linked-input-a", turnID: "turn-input-a"},
		approvalWorkerSpec{threadID: "linked-input-b", turnID: "turn-input-b"},
	)
	eventA := workerUserInputEvent(workers[0], "1", "turn-input-a", "item-input-a", "Use worker A")
	eventB := workerUserInputEvent(workers[1], "1", "turn-input-b", "item-input-b", "Use worker B")
	apps[0].setCurrentRequest(eventA)
	apps[1].setCurrentRequest(eventB)

	svc.handleMinimalLinkedWorkerEvent(context.Background(), workers[0], eventA)
	svc.handleMinimalLinkedWorkerEvent(context.Background(), workers[1], eventB)

	pendingA, err := svc.store.GetLatestPendingApprovalForThread(context.Background(), workers[0].ThreadID)
	if err != nil || pendingA == nil {
		t.Fatalf("pending A=%#v err=%v", pendingA, err)
	}
	pendingB, err := svc.store.GetLatestPendingApprovalForThread(context.Background(), workers[1].ThreadID)
	if err != nil || pendingB == nil {
		t.Fatalf("pending B=%#v err=%v", pendingB, err)
	}
	if pendingA.WireRequestID != "1" || pendingB.WireRequestID != "1" {
		t.Fatalf("pending wire ids A/B=%q/%q, want original wire id 1", pendingA.WireRequestID, pendingB.WireRequestID)
	}
	if pendingA.RequestID == "1" || pendingB.RequestID == "1" || pendingA.RequestID == pendingB.RequestID {
		t.Fatalf("pending durable ids A/B=%q/%q, want scoped distinct non-wire ids", pendingA.RequestID, pendingB.RequestID)
	}
	if pendingA.RequestKind != "item/tool/requestUserInput" || pendingB.RequestKind != "item/tool/requestUserInput" {
		t.Fatalf("pending request kinds A/B=%q/%q, want App Server method", pendingA.RequestKind, pendingB.RequestKind)
	}

	tokenA := callbackTokenForButton(svc.planPromptButtons(context.Background(), effectivePlanPrompt(pendingA, nil)), "Use worker A")
	tokenB := callbackTokenForButton(svc.planPromptButtons(context.Background(), effectivePlanPrompt(pendingB, nil)), "Use worker B")
	if tokenA == "" || tokenB == "" {
		t.Fatalf("tokens A/B=%q/%q, want both worker choices", tokenA, tokenB)
	}
	if _, err := svc.HandleCallback(context.Background(), 7, 0, 0, 7, tokenA); err != nil {
		t.Fatalf("choice A callback: %v", err)
	}
	if _, err := svc.HandleCallback(context.Background(), 7, 0, 0, 7, tokenB); err != nil {
		t.Fatalf("choice B callback: %v", err)
	}
	if got := global.responseCount("1"); got != 0 {
		t.Fatalf("global live wire responses=%d, want 0", got)
	}
	if calls := apps[0].responsesFor("1"); len(calls) != 1 {
		t.Fatalf("worker A wire responses=%#v, want one", calls)
	}
	if calls := apps[1].responsesFor("1"); len(calls) != 1 {
		t.Fatalf("worker B wire responses=%#v, want one", calls)
	}
}

func TestMinimalWorkerUserInputResolvedWireCompletesOnlyExactWorkerSession(t *testing.T) {
	svc, _, _ := newApprovalTestService(t)
	workers, apps := acquireApprovalWorkers(t, svc,
		approvalWorkerSpec{threadID: "linked-resolve-input-a", turnID: "turn-input-a"},
		approvalWorkerSpec{threadID: "linked-resolve-input-b", turnID: "turn-input-b"},
	)
	eventA := workerUserInputEvent(workers[0], "1", "turn-input-a", "item-input-a", "Use A")
	eventB := workerUserInputEvent(workers[1], "1", "turn-input-b", "item-input-b", "Use B")
	apps[0].setCurrentRequest(eventA)
	apps[1].setCurrentRequest(eventB)
	svc.handleMinimalLinkedWorkerEvent(context.Background(), workers[0], eventA)
	svc.handleMinimalLinkedWorkerEvent(context.Background(), workers[1], eventB)

	svc.handleMinimalLinkedWorkerEvent(context.Background(), workers[0], appserver.Event{Channel: "notification", Method: "serverRequest/resolved", Params: map[string]any{"requestId": "1"}})

	pendingA, err := svc.store.GetPendingApprovalForSession(context.Background(), "1", workers[0].SessionIdentity)
	if err != nil || pendingA == nil || pendingA.Status != "resolved" {
		t.Fatalf("pending A after resolved=%#v err=%v, want resolved", pendingA, err)
	}
	pendingB, err := svc.store.GetPendingApprovalForSession(context.Background(), "1", workers[1].SessionIdentity)
	if err != nil || pendingB == nil || pendingB.Status != "pending" {
		t.Fatalf("pending B after resolved=%#v err=%v, want pending", pendingB, err)
	}
}

func TestMinimalLiveResolvedWireCannotCompleteWorkerUserInput(t *testing.T) {
	svc, global, _ := newApprovalTestService(t)
	worker, workerApp := acquireApprovalWorker(t, svc, "linked-live-resolve-input", "turn-input")
	event := workerUserInputEvent(worker, "1", "turn-input", "item-input", "Use worker")
	workerApp.setCurrentRequest(event)
	svc.handleMinimalLinkedWorkerEvent(context.Background(), worker, event)

	svc.handleLiveEvent(context.Background(), global, appserver.Event{Channel: "notification", Method: "serverRequest/resolved", Params: map[string]any{"requestId": "1"}})

	pending, err := svc.store.GetPendingApprovalForSession(context.Background(), "1", worker.SessionIdentity)
	if err != nil || pending == nil || pending.Status != "pending" {
		t.Fatalf("worker pending after live resolved=%#v err=%v, want pending", pending, err)
	}
}

func TestMinimalLegacyUserInputResolvedWireStillCompletesEmptyIdentityRow(t *testing.T) {
	svc, app, _ := newApprovalTestService(t)
	if err := svc.store.SavePendingApproval(context.Background(), model.PendingApproval{
		RequestID:     "legacy-input",
		WireRequestID: "legacy-wire",
		ThreadID:      "thr-1",
		TurnID:        "turn-1",
		PromptKind:    "user_input",
		RequestKind:   "item/tool/requestUserInput",
		Question:      "Legacy?",
		Status:        "pending",
		PayloadJSON:   `{"questions":[{"id":"choice","question":"Legacy?"}]}`,
		UpdatedAt:     model.NowString(),
	}); err != nil {
		t.Fatal(err)
	}

	svc.handleLiveEvent(context.Background(), app, appserver.Event{Channel: "notification", Method: "serverRequest/resolved", Params: map[string]any{"requestId": "legacy-wire"}})

	pending, err := svc.store.GetPendingApproval(context.Background(), "legacy-input")
	if err != nil || pending == nil || pending.Status != "resolved" {
		t.Fatalf("legacy pending after live resolved=%#v err=%v, want resolved", pending, err)
	}
}

func TestMinimalLiveResolvedWireCompletesExactLiveSessionScopedUserInput(t *testing.T) {
	svc, app, _ := newApprovalTestService(t)
	liveIdentity := svc.minimalApprovalSessionIdentity()
	if err := svc.store.SavePendingApproval(context.Background(), model.PendingApproval{
		RequestID:       "1",
		WireRequestID:   "1",
		ThreadID:        "thr-1",
		TurnID:          "turn-1",
		PromptKind:      "user_input",
		RequestKind:     "item/tool/requestUserInput",
		Question:        "Live scoped?",
		SessionIdentity: liveIdentity,
		Status:          "pending",
		PayloadJSON:     `{"questions":[{"id":"choice","question":"Live scoped?"}]}`,
		UpdatedAt:       model.NowString(),
	}); err != nil {
		t.Fatal(err)
	}

	svc.handleLiveEvent(context.Background(), app, appserver.Event{Channel: "notification", Method: "serverRequest/resolved", Params: map[string]any{"requestId": "1"}})

	pending, err := svc.store.GetPendingApprovalForSession(context.Background(), "1", liveIdentity)
	if err != nil || pending == nil || pending.Status != "resolved" {
		t.Fatalf("live scoped pending after live resolved=%#v err=%v, want resolved", pending, err)
	}
}

func TestMinimalWorkerUserInputChoiceRequiresExactThreadTurnAndKind(t *testing.T) {
	svc, global, _ := newApprovalTestService(t)
	worker, workerApp := acquireApprovalWorker(t, svc, "linked-input-mismatch", "turn-input")
	event := workerUserInputEvent(worker, "req-worker-mismatch", "turn-input", "item-input", "Use exact")
	workerApp.setCurrentRequest(event)
	svc.handleMinimalLinkedWorkerEvent(context.Background(), worker, event)
	pending, err := svc.store.GetLatestPendingApprovalForThread(context.Background(), worker.ThreadID)
	if err != nil || pending == nil {
		t.Fatalf("pending mismatch=%#v err=%v", pending, err)
	}
	token := callbackTokenForButton(svc.planPromptButtons(context.Background(), effectivePlanPrompt(pending, nil)), "Use exact")
	if token == "" {
		t.Fatalf("rendered input buttons for mismatch=%#v, want choice", pending)
	}
	workerApp.setCurrentRequest(workerUserInputEvent(worker, "req-worker-mismatch", "turn-other", "item-input", "Use exact"))

	response, err := svc.HandleCallback(context.Background(), 7, 0, 0, 7, token)
	if err != nil {
		t.Fatal(err)
	}
	if response == nil || !strings.Contains(response.Text, "Could not send answer") {
		t.Fatalf("mismatch response=%#v, want failed exact send", response)
	}
	if got := global.responseCount("req-worker-mismatch"); got != 0 {
		t.Fatalf("global live responses=%d, want 0", got)
	}
	if got := workerApp.responseCount("req-worker-mismatch"); got != 0 {
		t.Fatalf("worker responses=%d, want 0 after logical mismatch", got)
	}
	stillPending, _ := svc.store.GetPendingApproval(context.Background(), pending.RequestID)
	if stillPending == nil || stillPending.Status != "pending" {
		t.Fatalf("pending after mismatch=%#v, want still pending", stillPending)
	}
}

func TestMinimalWorkerUserInputReusedWireRequestIDRefreshesLogicalFields(t *testing.T) {
	svc, global, _ := newApprovalTestService(t)
	worker, workerApp := acquireApprovalWorker(t, svc, "linked-input-reuse", "turn-old")
	oldEvent := workerUserInputEvent(worker, "reuse-wire", "turn-old", "item-old", "Old option")
	workerApp.setCurrentRequest(oldEvent)
	svc.handleMinimalLinkedWorkerEvent(context.Background(), worker, oldEvent)
	oldPending, err := svc.store.GetLatestPendingApprovalForThread(context.Background(), worker.ThreadID)
	if err != nil || oldPending == nil {
		t.Fatalf("old pending=%#v err=%v", oldPending, err)
	}
	_ = svc.store.UpdatePendingApprovalStatus(context.Background(), oldPending.RequestID, "resolved:choice")

	newEvent := workerUserInputEvent(worker, "reuse-wire", "turn-new", "item-new", "New option")
	workerApp.setCurrentRequest(newEvent)
	svc.handleMinimalLinkedWorkerEvent(context.Background(), worker, newEvent)

	reused, err := svc.store.GetPendingApproval(context.Background(), oldPending.RequestID)
	if err != nil || reused == nil {
		t.Fatalf("reused pending=%#v err=%v", reused, err)
	}
	if reused.WireRequestID != "reuse-wire" || reused.TurnID != "turn-new" || reused.ItemID != "item-new" || reused.RequestKind != "item/tool/requestUserInput" || reused.Status != "pending" {
		t.Fatalf("reused pending=%#v, want refreshed wire/logical fields", reused)
	}
	if strings.Contains(reused.PayloadJSON, "Old option") || !strings.Contains(reused.PayloadJSON, "New option") {
		t.Fatalf("reused payload=%s, want new option only", reused.PayloadJSON)
	}
	token := callbackTokenForButton(svc.planPromptButtons(context.Background(), effectivePlanPrompt(reused, nil)), "New option")
	if token == "" {
		t.Fatalf("rendered reused buttons=%#v, want new choice", reused)
	}
	if _, err := svc.HandleCallback(context.Background(), 7, 0, 0, 7, token); err != nil {
		t.Fatalf("reused choice callback: %v", err)
	}
	if got := global.responseCount("reuse-wire"); got != 0 {
		t.Fatalf("global live responses=%d, want 0", got)
	}
	if calls := workerApp.responsesFor("reuse-wire"); len(calls) != 1 {
		t.Fatalf("worker wire responses=%#v, want one", calls)
	}
}

func TestMinimalWorkerUserInputReplyUsesExactWorkerSession(t *testing.T) {
	svc, global, _ := newApprovalTestService(t)
	worker, workerApp := acquireApprovalWorker(t, svc, "linked-input-reply", "turn-input")
	emitWorkerUserInput(t, svc, worker, "req-worker-reply", "Use reply")
	if err := svc.store.PutMessageRoute(context.Background(), model.MessageRoute{
		ChatID:    7,
		MessageID: 701,
		ThreadID:  worker.ThreadID,
		TurnID:    "turn-input",
		EventID:   "plan_request:" + model.ScopedRequestID(worker.SessionIdentity, "req-worker-reply"),
		CreatedAt: model.NowString(),
	}); err != nil {
		t.Fatal(err)
	}

	response, err := svc.HandleMessage(context.Background(), 7, 0, 7, "Use reply", 701)
	if err != nil {
		t.Fatal(err)
	}
	if response == nil || response.ThreadID != worker.ThreadID || response.TurnID != "turn-input" {
		t.Fatalf("reply response=%#v, want worker thread/turn", response)
	}
	if got := global.responseCount("req-worker-reply"); got != 0 {
		t.Fatalf("global live responses=%d, want 0", got)
	}
	calls := workerApp.responsesFor("req-worker-reply")
	if len(calls) != 1 {
		t.Fatalf("worker reply responses=%#v, want one", calls)
	}
}

func TestMinimalLinkWorkerManagerSessionIdentityUniqueAcrossLifetimes(t *testing.T) {
	managerOne := newMinimalLinkWorkerManager(func() continuationSession {
		return &approvalWorkerSession{approvalSession: &approvalSession{}, events: make(chan appserver.Event)}
	}, 0, nil)
	managerTwo := newMinimalLinkWorkerManager(func() continuationSession {
		return &approvalWorkerSession{approvalSession: &approvalSession{}, events: make(chan appserver.Event)}
	}, 0, nil)
	workerOne, err := managerOne.Acquire(context.Background(), "link:one", "linked-same")
	if err != nil {
		t.Fatal(err)
	}
	workerTwo, err := managerTwo.Acquire(context.Background(), "link:two", "linked-same")
	if err != nil {
		t.Fatal(err)
	}
	if workerOne.Generation != 1 || workerTwo.Generation != 1 {
		t.Fatalf("worker generations=%d/%d, want both generation 1", workerOne.Generation, workerTwo.Generation)
	}
	if workerOne.SessionIdentity == workerTwo.SessionIdentity {
		t.Fatalf("manager identities both %q, want unique across manager lifetimes", workerOne.SessionIdentity)
	}
	if !minimalWorkerCallbackIdentity(workerOne.SessionIdentity) || !minimalWorkerCallbackIdentity(workerTwo.SessionIdentity) {
		t.Fatalf("worker identity prefixes=%q/%q, want minimal-link-worker prefix", workerOne.SessionIdentity, workerTwo.SessionIdentity)
	}
}

func TestStaleManagerWorkerIdentityDoesNotResolveNewManagerGenerationOne(t *testing.T) {
	svc, _, _ := newApprovalTestService(t)
	workerOne, _ := acquireApprovalWorker(t, svc, "linked-stale-manager", "turn-one")
	staleIdentity := workerOne.SessionIdentity
	workerTwo, _ := acquireApprovalWorker(t, svc, "linked-stale-manager", "turn-two")
	if workerTwo.Generation != 1 {
		t.Fatalf("fresh manager worker generation=%d, want 1", workerTwo.Generation)
	}
	if _, ok := svc.sessionForCallbackIdentity(staleIdentity, workerTwo.ThreadID, "turn-two"); ok {
		t.Fatalf("stale identity %q resolved to fresh manager worker %q", staleIdentity, workerTwo.SessionIdentity)
	}
}

func TestMinimalLiveUserInputReplyStillUsesGlobalLiveSession(t *testing.T) {
	svc, global, _ := newApprovalTestService(t)
	if err := svc.store.SavePendingApproval(context.Background(), model.PendingApproval{
		RequestID:       "req-live-input",
		ThreadID:        "thr-1",
		TurnID:          "turn-1",
		PromptKind:      "user_input",
		RequestKind:     "item/tool/requestUserInput",
		Question:        "Choose.",
		SessionIdentity: svc.minimalApprovalSessionIdentity(),
		PayloadJSON:     `{"questions":[{"id":"choice","question":"Choose."}]}`,
		Status:          "pending",
		UpdatedAt:       model.NowString(),
	}); err != nil {
		t.Fatal(err)
	}
	if err := svc.store.PutMessageRoute(context.Background(), model.MessageRoute{
		ChatID:    7,
		MessageID: 702,
		ThreadID:  "thr-1",
		TurnID:    "turn-1",
		EventID:   "plan_request:req-live-input",
		CreatedAt: model.NowString(),
	}); err != nil {
		t.Fatal(err)
	}
	global.mu.Lock()
	global.currentRequest = approvalRequestIdentity{requestID: "req-live-input", threadID: "thr-1", turnID: "turn-1", requestKind: "item/tool/requestUserInput"}
	global.mu.Unlock()

	response, err := svc.HandleMessage(context.Background(), 7, 0, 7, "Use live", 702)
	if err != nil {
		t.Fatal(err)
	}
	if response == nil || response.ThreadID != "thr-1" || response.TurnID != "turn-1" {
		t.Fatalf("live reply response=%#v, want live thread/turn", response)
	}
	if got := global.responseCount("req-live-input"); got != 1 {
		t.Fatalf("global live responses=%d, want 1", got)
	}
}

func TestStaleWorkerApprovalCallbackExpiresAfterReleaseWithoutGlobalFallback(t *testing.T) {
	svc, global, sender := newApprovalTestService(t)
	worker, workerApp := acquireApprovalWorker(t, svc, "linked-stale-worker", "turn-stale")
	seedRunningLinkedTerminal(t, svc, worker.ThreadID, "turn-stale", worker.Generation)
	created, err := svc.store.CreateMinimalApproval(context.Background(), storage.MinimalApprovalSeed{
		Approval: storage.MinimalApproval{
			RequestID:       "req-stale-worker",
			ThreadID:        worker.ThreadID,
			TurnID:          "turn-stale",
			RequestKind:     minimalCommandApprovalKind,
			ProjectName:     svc.cfg.Projects[0].DisplayName,
			SessionIdentity: worker.SessionIdentity,
			Status:          "pending",
		},
		ApproveToken: "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee",
		DenyToken:    "ffffffffffffffffffffffffffffffff",
		Delivery: model.DeliveryQueueItem{
			EventID:  "approval-stale-worker",
			ChatKey:  model.ChatKey(7, 0),
			ChatID:   7,
			ThreadID: worker.ThreadID,
			Kind:     minimalApprovalQueueKind,
			Status:   model.DeliveryStatusPending,
			PayloadJSON: storage.MustJSON(model.DeliveryPayload{
				Text:     "approval",
				ThreadID: worker.ThreadID,
				TurnID:   "turn-stale",
				EventID:  "req-stale-worker",
				Buttons: [][]model.ButtonSpec{{
					{Text: "승인", CallbackData: "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"},
					{Text: "거부", CallbackData: "ffffffffffffffffffffffffffffffff"},
				}},
			}),
		},
	})
	if err != nil || !created {
		t.Fatalf("CreateMinimalApproval created=%t err=%v", created, err)
	}
	svc.processDeliveryBatch(context.Background())
	if len(sender.messages) != 1 {
		t.Fatalf("messages=%#v, want delivered worker approval", sender.messages)
	}
	token := sender.messages[0].buttons[0][0].CallbackData
	if changed, err := svc.store.BeginMinimalLinkedRelease(context.Background(), model.MinimalLinkedRelease{LinkedThreadID: worker.ThreadID, TurnID: "turn-stale", WorkerGeneration: worker.Generation}); err != nil || !changed {
		t.Fatalf("begin release changed=%t err=%v, want true nil", changed, err)
	}
	if closed, err := svc.minimalWorkers.Release(context.Background(), worker.ThreadID, worker.Generation); err != nil || !closed {
		t.Fatalf("worker release closed=%t err=%v, want true nil", closed, err)
	}
	if changed, err := svc.store.FinishMinimalLinkedReleaseWithReadyDelivery(context.Background(), worker.ThreadID, worker.Generation, worker.SessionIdentity, "turn-stale", svc.now(), nil); err != nil || !changed {
		t.Fatalf("finish release changed=%t err=%v, want true nil", changed, err)
	}

	if _, err := svc.HandleCallback(context.Background(), 7, 0, 600, 7, token); !errors.Is(err, ErrCallbackConsumed) {
		t.Fatalf("stale callback error=%v, want ErrCallbackConsumed", err)
	}
	if got := global.responseCount("req-stale-worker"); got != 0 {
		t.Fatalf("global live responses=%d, want 0", got)
	}
	if got := workerApp.responseCount("req-stale-worker"); got != 0 {
		t.Fatalf("released worker responses=%d, want 0", got)
	}
	if _, ok := svc.minimalWorkers.BySessionIdentity(worker.SessionIdentity); ok {
		t.Fatal("stale callback reacquired or retained released worker identity")
	}
}

func TestMinimalApprovalSuppressesUnregisteredOrUnreadableAuthoritativeCWD(t *testing.T) {
	tests := []struct {
		name    string
		payload map[string]any
		err     error
	}{
		{name: "unregistered", payload: approvalThreadRead(`C:\outside\not-registered`)},
		{name: "unreadable", err: errors.New("thread read unavailable")},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			svc, app, sender := newApprovalTestService(t)
			app.threadReads["thr-1"] = tc.payload
			app.threadReadErr = tc.err
			handleApprovalEvent(svc, app, approvalEvent("req-suppress", "item/commandExecution/requestApproval", map[string]any{"command": "safe"}))
			svc.processDeliveryBatch(context.Background())
			approval, err := svc.store.GetMinimalApproval(context.Background(), "req-suppress")
			if err != nil || approval != nil || len(sender.messages) != 0 {
				t.Fatalf("suppressed approval/message = %#v/%d, %v", approval, len(sender.messages), err)
			}
		})
	}
}

func TestMinimalApprovalRequiresExactRegisteredCWD(t *testing.T) {
	svc, app, sender := newApprovalTestService(t)
	project, err := svc.projectRegistry.Resolve("bridge")
	if err != nil {
		t.Fatal(err)
	}
	child := filepath.Join(project.CanonicalPath, "child")
	if err := ensureTestDir(child); err != nil {
		t.Fatal(err)
	}
	app.threadReads["thr-1"] = approvalThreadRead(child)

	handleApprovalEvent(svc, app, approvalEvent("req-child-cwd", "item/commandExecution/requestApproval", map[string]any{"command": "safe"}))
	svc.processDeliveryBatch(context.Background())

	approval, err := svc.store.GetMinimalApproval(context.Background(), "req-child-cwd")
	if err != nil || approval != nil || len(sender.messages) != 0 {
		t.Fatalf("child-cwd approval/message = %#v/%d, %v; want exact registered cwd only", approval, len(sender.messages), err)
	}
}

func TestMinimalApprovalUnknownKindsFailClosedAndUserInputGetsNoApprovalButtons(t *testing.T) {
	tests := []struct {
		name   string
		method string
	}{
		{name: "unknown approval", method: "item/permissions/requestApproval"},
		{name: "request user input", method: "item/tool/requestUserInput"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			svc, app, sender := newApprovalTestService(t)
			handleApprovalEvent(svc, app, approvalEvent("req-unknown", tc.method, map[string]any{"question": "choose"}))
			svc.processDeliveryBatch(context.Background())
			approval, err := svc.store.GetMinimalApproval(context.Background(), "req-unknown")
			if err != nil || approval != nil || len(sender.messages) != 0 {
				t.Fatalf("approval/message = %#v/%d, %v", approval, len(sender.messages), err)
			}
		})
	}
}

func TestMinimalApprovalBrokerLeavesUserInputResolutionOnLegacyPath(t *testing.T) {
	svc, app, sender := newApprovalTestService(t)
	handleApprovalEvent(svc, app, approvalEvent("req-input", "item/tool/requestUserInput", map[string]any{"question": "choose"}))
	pending, err := svc.store.GetPendingApproval(context.Background(), "req-input")
	if err != nil || pending == nil || pending.PromptKind != "user_input" || pending.Status != "pending" {
		t.Fatalf("pending input = %#v, %v", pending, err)
	}
	svc.handleLiveEvent(context.Background(), app, appserver.Event{Channel: "notification", Method: "serverRequest/resolved", Params: map[string]any{"requestId": "req-input", "threadId": "thr-1"}})
	pending, err = svc.store.GetPendingApproval(context.Background(), "req-input")
	if err != nil || pending == nil || pending.Status != "resolved" || len(sender.messages) != 0 {
		t.Fatalf("resolved input/message = %#v/%d, %v", pending, len(sender.messages), err)
	}
}

func TestMinimalApprovalRestartPreservesDeliveredRouteAndDoesNotReplayPresentation(t *testing.T) {
	svc, app, sender := newApprovalTestService(t)
	approve, _ := presentApproval(t, svc, app, sender, "req-restart", "item/commandExecution/requestApproval")
	cfg := svc.cfg
	if err := svc.Close(); err != nil {
		t.Fatal(err)
	}

	restarted, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = restarted.Close() })
	restartedSender := &approvalTestSender{}
	restartedApp := newApprovalSession(t, restarted)
	restarted.SetSender(restartedSender)
	restarted.mu.Lock()
	restarted.live = restartedApp
	restarted.liveConnected = true
	restarted.mu.Unlock()

	if _, err := restarted.HandleCallback(context.Background(), 7, 0, 600, 7, approve); !errors.Is(err, ErrCallbackMismatch) {
		t.Fatalf("callback before authoritative replay = %v", err)
	}
	if got := restartedApp.responseCount("req-restart"); got != 0 {
		t.Fatalf("responses before authoritative replay = %d", got)
	}
	handleApprovalEvent(restarted, restartedApp, approvalEvent("req-restart", "item/commandExecution/requestApproval", map[string]any{"command": "duplicate replay"}))
	restarted.processDeliveryBatch(context.Background())
	if got := len(restartedSender.messages); got != 0 {
		t.Fatalf("restart replayed approval presentation: %d", got)
	}
	if _, err := restarted.HandleCallback(context.Background(), 7, 0, 600, 7, approve); err != nil {
		t.Fatalf("callback after restart failed: %v", err)
	}
	if got := restartedApp.responseCount("req-restart"); got != 1 {
		t.Fatalf("responses after restart = %d", got)
	}
}

func TestMinimalApprovalBridgeOwnedRequestSuppressesPollOnlyNotice(t *testing.T) {
	svc, app, sender := newApprovalTestService(t)
	ctx := context.Background()
	project, err := svc.projectRegistry.Resolve("bridge")
	if err != nil {
		t.Fatal(err)
	}
	thread := model.Thread{
		ID:           "thr-1",
		Title:        "Private bridge approval title",
		ProjectName:  project.DisplayName,
		CWD:          project.CanonicalPath,
		UpdatedAt:    minimalTestNow.Unix(),
		Status:       "active[waitingOnApproval]",
		ActiveTurnID: "turn-1",
	}
	if err := svc.store.UpsertThread(ctx, thread); err != nil {
		t.Fatalf("UpsertThread failed: %v", err)
	}
	presentApproval(t, svc, app, sender, "req-bridge-owned", "item/commandExecution/requestApproval")
	if len(sender.messages) != 1 || len(sender.messages[0].buttons) == 0 {
		t.Fatalf("bridge-owned approval presentation = %#v", sender.messages)
	}
	storedSnapshot, err := svc.store.GetSnapshot(ctx, thread.ID)
	if err != nil {
		t.Fatalf("GetSnapshot failed: %v", err)
	}
	if storedSnapshot == nil || storedSnapshot.LastApprovalFP != thread.ActiveTurnID {
		t.Fatalf("bridge-owned approval snapshot = %#v, want approval fingerprint %q", storedSnapshot, thread.ActiveTurnID)
	}
	app.threadReads["thr-1"] = minimalWaitingApprovalPayload(thread, "PRIVATE_APPROVAL_BODY")
	svc.poll = app
	svc.pollConnected = true

	svc.pollTracked(ctx)
	svc.processDeliveryBatch(ctx)

	if len(sender.messages) != 1 {
		t.Fatalf("poll created duplicate PC-only notice for bridge-owned approval: %#v", sender.messages)
	}
}

func TestMinimalApprovalPollBeforeLiveRequestDeliversOnlyActionableCard(t *testing.T) {
	svc, app, sender := newApprovalTestService(t)
	sender.messageIDs = []int64{610, 611}
	ctx := context.Background()
	project, err := svc.projectRegistry.Resolve("bridge")
	if err != nil {
		t.Fatal(err)
	}
	thread := model.Thread{
		ID:           "thr-1",
		Title:        "Private bridge approval title",
		ProjectName:  project.DisplayName,
		CWD:          project.CanonicalPath,
		UpdatedAt:    minimalTestNow.Unix(),
		Status:       "active[waitingOnApproval]",
		ActiveTurnID: "turn-1",
	}
	if err := svc.store.UpsertThread(ctx, thread); err != nil {
		t.Fatalf("UpsertThread failed: %v", err)
	}
	app.threadReads["thr-1"] = minimalWaitingApprovalPayload(thread, "PRIVATE_APPROVAL_BODY")
	svc.poll = app
	svc.pollConnected = true

	svc.pollTracked(ctx)
	if len(sender.messages) != 0 {
		t.Fatalf("poll-only notice was sent before outbox delivery: %#v", sender.messages)
	}
	handleApprovalEvent(svc, app, approvalEvent("req-poll-live-race", "item/commandExecution/requestApproval", map[string]any{
		"command": "safe command",
	}))
	svc.processDeliveryBatch(ctx)
	svc.processDeliveryBatch(ctx)

	visible := visibleRecordedMessages(sender.messages, sender.deletes)
	if len(visible) != 1 {
		t.Fatalf("poll/live race left visible messages %#v, want exactly one actionable approval card", visible)
	}
	message := visible[0]
	if got, want := buttonLabels(message.buttons), [][]string{{"승인", "거부"}}; !equalStringRows(got, want) {
		t.Fatalf("buttons = %#v, want %#v", got, want)
	}
	if strings.Contains(message.text, "PC에서 직접") {
		t.Fatalf("delivered PC-only notice instead of actionable card: %q", message.text)
	}
	for _, forbidden := range []string{"PRIVATE_APPROVAL_BODY", project.CanonicalPath, thread.Title} {
		if strings.Contains(message.text, forbidden) {
			t.Fatalf("approval race message leaked %q: %q", forbidden, message.text)
		}
	}
}

func TestMinimalApprovalCleanupWaitsForPollOnlyRouteCreatedAfterLiveRequest(t *testing.T) {
	svc, app, _ := newApprovalTestService(t)
	raceSender := &approvalRaceSender{messageIDs: []int64{610, 611}}
	svc.SetSender(raceSender)
	ctx := context.Background()
	project, err := svc.projectRegistry.Resolve("bridge")
	if err != nil {
		t.Fatal(err)
	}
	thread := model.Thread{
		ID:           "thr-1",
		Title:        "Private bridge approval title",
		ProjectName:  project.DisplayName,
		CWD:          project.CanonicalPath,
		UpdatedAt:    minimalTestNow.Unix(),
		Status:       "active[waitingOnApproval]",
		ActiveTurnID: "turn-1",
	}
	if err := svc.store.UpsertThread(ctx, thread); err != nil {
		t.Fatalf("UpsertThread failed: %v", err)
	}
	app.threadReads["thr-1"] = minimalWaitingApprovalPayload(thread, "PRIVATE_APPROVAL_BODY")
	svc.poll = app
	svc.pollConnected = true
	raceSender.afterPollOnlySend = func() {
		handleApprovalEvent(svc, app, approvalEvent("req-route-late-cleanup", "item/commandExecution/requestApproval", map[string]any{
			"command": "safe command",
		}))
	}

	svc.pollTracked(ctx)
	svc.processDeliveryBatch(ctx)
	if len(raceSender.messages) != 1 || len(raceSender.messages[0].buttons) != 0 {
		t.Fatalf("initial poll-only notice = %#v, want one no-button notice", raceSender.messages)
	}
	if len(raceSender.deletes) != 0 {
		t.Fatalf("cleanup ran before actionable delivery committed: %#v", raceSender.deletes)
	}

	svc.processDeliveryBatch(ctx)
	svc.processDeliveryBatch(ctx)

	if got := countDeletesForMessage(raceSender.deletes, 610); got != 1 {
		t.Fatalf("stale poll-only deletes = %#v, want message 610 deleted exactly once after route/action delivery", raceSender.deletes)
	}
	visible := visibleRecordedMessages(raceSender.messages, raceSender.deletes)
	if len(visible) != 1 {
		t.Fatalf("visible messages after delayed cleanup = %#v, want exactly one actionable card", visible)
	}
	if got, want := buttonLabels(visible[0].buttons), [][]string{{"승인", "거부"}}; !equalStringRows(got, want) {
		t.Fatalf("visible buttons = %#v, want %#v", got, want)
	}
	if strings.Contains(visible[0].text, "PC에서 직접") {
		t.Fatalf("visible message is still PC-only notice: %q", visible[0].text)
	}
}

func TestMinimalApprovalPCCleanupUsesOriginalNoticeTargetAfterObserverMoves(t *testing.T) {
	svc, app, _ := newApprovalTestService(t)
	raceSender := &approvalRaceSender{messageIDs: []int64{610, 611}}
	svc.SetSender(raceSender)
	ctx := context.Background()
	project, err := svc.projectRegistry.Resolve("bridge")
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.store.SetGlobalObserverTarget(ctx, 7, 0, true); err != nil {
		t.Fatalf("SetGlobalObserverTarget(A) failed: %v", err)
	}
	thread := model.Thread{
		ID:           "thr-1",
		Title:        "Private bridge approval title",
		ProjectName:  project.DisplayName,
		CWD:          project.CanonicalPath,
		UpdatedAt:    minimalTestNow.Unix(),
		Status:       "active[waitingOnApproval]",
		ActiveTurnID: "turn-1",
	}
	if err := svc.store.UpsertThread(ctx, thread); err != nil {
		t.Fatalf("UpsertThread failed: %v", err)
	}
	app.threadReads["thr-1"] = minimalWaitingApprovalPayload(thread, "PRIVATE_APPROVAL_BODY")
	svc.poll = app
	svc.pollConnected = true

	svc.pollTracked(ctx)
	svc.processDeliveryBatch(ctx)
	if len(raceSender.messages) != 1 || raceSender.messages[0].chatID != 7 || raceSender.messages[0].messageID != 610 || len(raceSender.messages[0].buttons) != 0 {
		t.Fatalf("initial poll-only notice = %#v, want chat A message 610 without buttons", raceSender.messages)
	}

	if err := svc.store.SetGlobalObserverTarget(ctx, 8, 0, true); err != nil {
		t.Fatalf("SetGlobalObserverTarget(B) failed: %v", err)
	}
	handleApprovalEvent(svc, app, approvalEvent("req-cleanup-target-move", "item/commandExecution/requestApproval", map[string]any{
		"command": "safe command",
	}))
	svc.processDeliveryBatch(ctx)
	if len(raceSender.messages) != 2 || raceSender.messages[1].chatID != 8 || raceSender.messages[1].messageID != 611 || len(raceSender.messages[1].buttons) != 1 {
		t.Fatalf("actionable approval after observer move = %#v, want chat B message 611 with buttons", raceSender.messages)
	}
	svc.processDeliveryBatch(ctx)

	if got := countDeletesForMessage(raceSender.deletes, 610); got != 1 {
		t.Fatalf("cleanup deletes after observer move = %#v, want original chat A message 610 deleted once", raceSender.deletes)
	}
	if got := countDeletesForMessage(raceSender.deletes, 611); got != 0 {
		t.Fatalf("cleanup deleted actionable chat B card %d times: %#v", got, raceSender.deletes)
	}
	svc.processDeliveryBatch(ctx)
	if got := countDeletesForMessage(raceSender.deletes, 610); got != 1 {
		t.Fatalf("cleanup is not idempotent after observer move: deletes = %#v, want original chat A message once", raceSender.deletes)
	}
	visible := visibleRecordedMessages(raceSender.messages, raceSender.deletes)
	if len(visible) != 1 || visible[0].chatID != 8 || visible[0].messageID != 611 || len(visible[0].buttons) != 1 {
		t.Fatalf("visible messages after moved-target cleanup = %#v, want only actionable chat B card", visible)
	}
}

func TestMinimalApprovalLegacyPCCleanupResolvesOriginalNoticeTargetAfterUpgrade(t *testing.T) {
	svc, app, _ := newApprovalTestService(t)
	raceSender := &approvalRaceSender{messageIDs: []int64{610, 611}}
	svc.SetSender(raceSender)
	ctx := context.Background()
	project, err := svc.projectRegistry.Resolve("bridge")
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.store.SetGlobalObserverTarget(ctx, 7, 0, true); err != nil {
		t.Fatalf("SetGlobalObserverTarget(A) failed: %v", err)
	}
	thread := model.Thread{
		ID:           "thr-1",
		Title:        "Private bridge approval title",
		ProjectName:  project.DisplayName,
		CWD:          project.CanonicalPath,
		UpdatedAt:    minimalTestNow.Unix(),
		Status:       "active[waitingOnApproval]",
		ActiveTurnID: "turn-1",
	}
	if err := svc.store.UpsertThread(ctx, thread); err != nil {
		t.Fatalf("UpsertThread failed: %v", err)
	}
	app.threadReads["thr-1"] = minimalWaitingApprovalPayload(thread, "PRIVATE_APPROVAL_BODY")
	svc.poll = app
	svc.pollConnected = true

	svc.pollTracked(ctx)
	svc.processDeliveryBatch(ctx)
	if len(raceSender.messages) != 1 || raceSender.messages[0].chatID != 7 || raceSender.messages[0].messageID != 610 || len(raceSender.messages[0].buttons) != 0 {
		t.Fatalf("initial poll-only notice = %#v, want chat A message 610 without buttons", raceSender.messages)
	}

	if err := svc.store.SetGlobalObserverTarget(ctx, 8, 0, true); err != nil {
		t.Fatalf("SetGlobalObserverTarget(B) failed: %v", err)
	}
	requestID := "req-legacy-cleanup-upgrade"
	handleApprovalEvent(svc, app, approvalEvent(requestID, "item/commandExecution/requestApproval", map[string]any{
		"command": "safe command",
	}))
	svc.processDeliveryBatch(ctx)
	if len(raceSender.messages) != 2 || raceSender.messages[1].chatID != 8 || raceSender.messages[1].messageID != 611 || len(raceSender.messages[1].buttons) != 1 {
		t.Fatalf("actionable approval after observer move = %#v, want chat B message 611 with buttons", raceSender.messages)
	}

	fresh, err := svc.store.ClaimDeliveryBatch(ctx, 10)
	if err != nil {
		t.Fatalf("claim fresh cleanup failed: %v", err)
	}
	if len(fresh) != 1 || fresh[0].Kind != minimalApprovalPCCleanupQueueKind || fresh[0].ChatID != 7 {
		t.Fatalf("fresh cleanup rows = %#v, want one snapped chat A cleanup row", fresh)
	}
	if err := svc.store.CompleteDelivery(ctx, fresh[0].ID); err != nil {
		t.Fatalf("neutralize fresh cleanup failed: %v", err)
	}

	pcEventID := minimalPollOnlyApprovalEventID("thr-1", "turn-1")
	legacyEventID := "legacy-cleanup-after-upgrade"
	if err := svc.store.EnqueueDelivery(ctx, model.DeliveryQueueItem{
		EventID:     legacyEventID,
		ChatKey:     model.ChatKey(8, 0),
		ChatID:      8,
		TopicID:     0,
		ThreadID:    "thr-1",
		Kind:        minimalApprovalPCCleanupQueueKind,
		Status:      model.DeliveryStatusPending,
		AvailableAt: model.NowString(),
		PayloadJSON: storage.MustJSON(model.DeliveryPayload{
			Mode:     "delete_message",
			ThreadID: "thr-1",
			TurnID:   "turn-1",
			ItemID:   requestID,
			EventID:  pcEventID,
		}),
	}); err != nil {
		t.Fatalf("enqueue legacy cleanup failed: %v", err)
	}
	cfg := svc.cfg
	if err := svc.Close(); err != nil {
		t.Fatal(err)
	}
	restarted, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = restarted.Close() })
	restarted.SetSender(raceSender)

	restarted.processDeliveryBatch(ctx)

	if got := countDeletesForMessage(raceSender.deletes, 610); got != 1 {
		t.Fatalf("legacy cleanup deletes after upgrade = %#v, want original chat A message 610 deleted once", raceSender.deletes)
	}
	if got := countDeletesForMessage(raceSender.deletes, 611); got != 0 {
		t.Fatalf("legacy cleanup deleted actionable chat B card %d times: %#v", got, raceSender.deletes)
	}
	status, retryCount, lastError := deliveryQueueState(t, restarted.store.Path(), legacyEventID)
	if status != model.DeliveryStatusDelivered || retryCount != 0 || lastError != "" {
		t.Fatalf("legacy cleanup queue state = status %q retry %d last_error %q, want delivered/no retry/no error", status, retryCount, lastError)
	}
	restarted.processDeliveryBatch(ctx)
	if got := countDeletesForMessage(raceSender.deletes, 610); got != 1 {
		t.Fatalf("legacy cleanup is not idempotent after upgrade: deletes = %#v, want original chat A message once", raceSender.deletes)
	}
}

func TestMinimalApprovalKeepsPollOnlyNoticeUntilActionDeliveryReplayRecovers(t *testing.T) {
	svc, app, _ := newApprovalTestService(t)
	raceSender := &approvalRaceSender{messageIDs: []int64{610, 611}, failActionSends: 1, failActionErr: ErrApprovalTransportUnavailable}
	svc.SetSender(raceSender)
	svc.cfg.DeliveryMaxAttempts = 1
	ctx := context.Background()
	project, err := svc.projectRegistry.Resolve("bridge")
	if err != nil {
		t.Fatal(err)
	}
	thread := model.Thread{
		ID:           "thr-1",
		Title:        "Private bridge approval title",
		ProjectName:  project.DisplayName,
		CWD:          project.CanonicalPath,
		UpdatedAt:    minimalTestNow.Unix(),
		Status:       "active[waitingOnApproval]",
		ActiveTurnID: "turn-1",
	}
	if err := svc.store.UpsertThread(ctx, thread); err != nil {
		t.Fatalf("UpsertThread failed: %v", err)
	}
	app.threadReads["thr-1"] = minimalWaitingApprovalPayload(thread, "PRIVATE_APPROVAL_BODY")
	svc.poll = app
	svc.pollConnected = true

	svc.pollTracked(ctx)
	svc.processDeliveryBatch(ctx)
	if len(raceSender.messages) != 1 || len(raceSender.messages[0].buttons) != 0 {
		t.Fatalf("initial poll-only notice = %#v, want one no-button notice", raceSender.messages)
	}

	handleApprovalEvent(svc, app, approvalEvent("req-action-replay-recovers", "item/commandExecution/requestApproval", map[string]any{
		"command": "safe command",
	}))
	if len(raceSender.deletes) != 0 {
		t.Fatalf("poll-only notice deleted before actionable delivery committed: %#v", raceSender.deletes)
	}
	svc.processDeliveryBatch(ctx)
	visibleAfterFailure := visibleRecordedMessages(raceSender.messages, raceSender.deletes)
	if len(visibleAfterFailure) != 1 || len(visibleAfterFailure[0].buttons) != 0 {
		t.Fatalf("visible messages after failed actionable delivery = %#v, want original PC-only notice retained", visibleAfterFailure)
	}

	handleApprovalEvent(svc, app, approvalEvent("req-action-replay-recovers", "item/commandExecution/requestApproval", map[string]any{
		"command": "safe command",
	}))
	svc.processDeliveryBatch(ctx)
	svc.processDeliveryBatch(ctx)

	if got := countDeletesForMessage(raceSender.deletes, 610); got != 1 {
		t.Fatalf("poll-only cleanup after replay deletes = %#v, want message 610 deleted exactly once", raceSender.deletes)
	}
	visible := visibleRecordedMessages(raceSender.messages, raceSender.deletes)
	if len(visible) != 1 {
		t.Fatalf("visible messages after replay recovery = %#v, want exactly one actionable card", visible)
	}
	if got, want := buttonLabels(visible[0].buttons), [][]string{{"승인", "거부"}}; !equalStringRows(got, want) {
		t.Fatalf("visible buttons = %#v, want %#v", got, want)
	}
	if strings.Contains(visible[0].text, "PC에서 직접") {
		t.Fatalf("visible message is still PC-only notice: %q", visible[0].text)
	}
}

func TestMinimalApprovalReplayStoresUsableCallbackRoutes(t *testing.T) {
	svc, app, _ := newApprovalTestService(t)
	raceSender := &approvalRaceSender{messageIDs: []int64{610}, failActionSends: 1, failActionErr: ErrApprovalTransportUnavailable}
	svc.SetSender(raceSender)
	svc.cfg.DeliveryMaxAttempts = 1
	ctx := context.Background()

	handleApprovalEvent(svc, app, approvalEvent("req-replay-routes", "item/commandExecution/requestApproval", map[string]any{
		"command": "safe command",
	}))
	svc.processDeliveryBatch(ctx)
	if len(raceSender.messages) != 0 {
		t.Fatalf("failed first delivery still sent messages: %#v", raceSender.messages)
	}

	handleApprovalEvent(svc, app, approvalEvent("req-replay-routes", "item/commandExecution/requestApproval", map[string]any{
		"command": "safe command",
	}))
	svc.processDeliveryBatch(ctx)
	if len(raceSender.messages) != 1 || len(raceSender.messages[0].buttons) != 1 || len(raceSender.messages[0].buttons[0]) != 2 {
		t.Fatalf("replayed approval presentation = %#v, want one actionable card", raceSender.messages)
	}
	approve := raceSender.messages[0].buttons[0][0].CallbackData
	response, err := svc.HandleCallback(ctx, 7, 0, 610, 7, approve)
	if err != nil {
		t.Fatalf("replayed approve callback failed: %v", err)
	}
	if response == nil || response.CallbackText != "승인됨" {
		t.Fatalf("replayed approve response = %#v", response)
	}
	if got := app.responseCount("req-replay-routes"); got != 1 {
		t.Fatalf("App Server responses after replayed approve = %d, want 1", got)
	}
}

func TestMinimalApprovalTelegramHTTPRejectionReplaysUsableCard(t *testing.T) {
	svc, app, _ := newApprovalTestService(t)
	raceSender := &approvalRaceSender{
		messageIDs:      []int64{610},
		failActionSends: 1,
		failActionErr:   errors.New("telegram sendMessage: 400 Bad Request: chat not found"),
	}
	svc.SetSender(raceSender)
	svc.cfg.DeliveryMaxAttempts = 1
	ctx := context.Background()

	handleApprovalEvent(svc, app, approvalEvent("req-telegram-rejection-replay", "item/commandExecution/requestApproval", map[string]any{
		"command": "safe command",
	}))
	svc.processDeliveryBatch(ctx)
	if len(raceSender.messages) != 0 {
		t.Fatalf("rejected first delivery still sent messages: %#v", raceSender.messages)
	}

	handleApprovalEvent(svc, app, approvalEvent("req-telegram-rejection-replay", "item/commandExecution/requestApproval", map[string]any{
		"command": "safe command",
	}))
	svc.processDeliveryBatch(ctx)
	if len(raceSender.messages) != 1 || len(raceSender.messages[0].buttons) != 1 || len(raceSender.messages[0].buttons[0]) != 2 {
		t.Fatalf("replayed Telegram rejection presentation = %#v, want one actionable card", raceSender.messages)
	}
	approve := raceSender.messages[0].buttons[0][0].CallbackData
	response, err := svc.HandleCallback(ctx, 7, 0, 610, 7, approve)
	if err != nil {
		t.Fatalf("replayed Telegram rejection approve callback failed: %v", err)
	}
	if response == nil || response.CallbackText != "승인됨" {
		t.Fatalf("replayed Telegram rejection approve response = %#v", response)
	}
	if got := app.responseCount("req-telegram-rejection-replay"); got != 1 {
		t.Fatalf("App Server responses after replayed Telegram rejection approve = %d, want 1", got)
	}
}

func TestMinimalApprovalAmbiguousAcceptedFailureDoesNotReplaySecondCard(t *testing.T) {
	svc, app, _ := newApprovalTestService(t)
	raceSender := &approvalRaceSender{
		messageIDs:           []int64{610, 611},
		ambiguousActionSends: 1,
		ambiguousActionErr:   errors.New("telegram send failed after an unknown byte count"),
	}
	svc.SetSender(raceSender)
	svc.cfg.DeliveryMaxAttempts = 1
	ctx := context.Background()

	handleApprovalEvent(svc, app, approvalEvent("req-ambiguous-delivery", "item/commandExecution/requestApproval", map[string]any{
		"command": "safe command",
	}))
	svc.processDeliveryBatch(ctx)
	if len(raceSender.messages) != 1 || raceSender.messages[0].messageID != 610 || len(raceSender.messages[0].buttons) != 1 {
		t.Fatalf("ambiguous first delivery = %#v, want one possibly accepted actionable card", raceSender.messages)
	}
	approve := raceSender.messages[0].buttons[0][0].CallbackData

	handleApprovalEvent(svc, app, approvalEvent("req-ambiguous-delivery", "item/commandExecution/requestApproval", map[string]any{
		"command": "safe command",
	}))
	svc.processDeliveryBatch(ctx)
	if len(raceSender.messages) != 1 {
		t.Fatalf("ambiguous exact replay minted second card: %#v", raceSender.messages)
	}
	if _, err := svc.HandleCallback(ctx, 7, 0, 610, 7, approve); err != nil {
		t.Fatalf("original ambiguous card did not remain recoverable: %v", err)
	}
	if got := app.responseCount("req-ambiguous-delivery"); got != 1 {
		t.Fatalf("App Server responses from original card = %d, want 1", got)
	}
}

func TestMinimalApprovalPCCleanupWaitsForActionableRoute(t *testing.T) {
	svc, app, _ := newApprovalTestService(t)
	raceSender := &approvalRaceSender{messageIDs: []int64{610, 611}, failActionSends: 1, failActionErr: ErrApprovalTransportUnavailable}
	svc.SetSender(raceSender)
	svc.cfg.DeliveryMaxAttempts = 1
	svc.cfg.DeliveryRetryBase = 0
	ctx := context.Background()
	project, err := svc.projectRegistry.Resolve("bridge")
	if err != nil {
		t.Fatal(err)
	}
	thread := model.Thread{
		ID:           "thr-1",
		Title:        "Private bridge approval title",
		ProjectName:  project.DisplayName,
		CWD:          project.CanonicalPath,
		UpdatedAt:    minimalTestNow.Unix(),
		Status:       "active[waitingOnApproval]",
		ActiveTurnID: "turn-1",
	}
	if err := svc.store.UpsertThread(ctx, thread); err != nil {
		t.Fatalf("UpsertThread failed: %v", err)
	}
	app.threadReads["thr-1"] = minimalWaitingApprovalPayload(thread, "PRIVATE_APPROVAL_BODY")
	svc.poll = app
	svc.pollConnected = true

	svc.pollTracked(ctx)
	svc.processDeliveryBatch(ctx)
	if len(raceSender.messages) != 1 || len(raceSender.messages[0].buttons) != 0 {
		t.Fatalf("initial poll-only notice = %#v, want one PC-only notice", raceSender.messages)
	}

	handleApprovalEvent(svc, app, approvalEvent("req-cleanup-waits", "item/commandExecution/requestApproval", map[string]any{
		"command": "safe command",
	}))
	svc.processDeliveryBatch(ctx)
	if len(raceSender.deletes) != 0 {
		t.Fatalf("failed actionable delivery deleted PC notice early: %#v", raceSender.deletes)
	}

	pcEventID := minimalPollOnlyApprovalEventID("thr-1", "turn-1")
	if err := svc.store.EnqueueDelivery(ctx, model.DeliveryQueueItem{
		EventID:     "test-cleanup-before-action-route",
		ChatKey:     model.ChatKey(7, 0),
		ChatID:      7,
		TopicID:     0,
		ThreadID:    "thr-1",
		Kind:        minimalApprovalPCCleanupQueueKind,
		Status:      model.DeliveryStatusPending,
		AvailableAt: model.NowString(),
		PayloadJSON: storage.MustJSON(model.DeliveryPayload{
			Mode:     "delete_message",
			ThreadID: "thr-1",
			TurnID:   "turn-1",
			ItemID:   "req-cleanup-waits",
			EventID:  pcEventID,
		}),
	}); err != nil {
		t.Fatalf("enqueue early cleanup failed: %v", err)
	}
	svc.processDeliveryBatch(ctx)
	if len(raceSender.deletes) != 0 {
		t.Fatalf("cleanup deleted PC notice before actionable route existed: %#v", raceSender.deletes)
	}
	visibleAfterEarlyCleanup := visibleRecordedMessages(raceSender.messages, raceSender.deletes)
	if len(visibleAfterEarlyCleanup) != 1 || len(visibleAfterEarlyCleanup[0].buttons) != 0 {
		t.Fatalf("visible messages before actionable recovery = %#v, want PC-only notice retained", visibleAfterEarlyCleanup)
	}

	handleApprovalEvent(svc, app, approvalEvent("req-cleanup-waits", "item/commandExecution/requestApproval", map[string]any{
		"command": "safe command",
	}))
	svc.processDeliveryBatch(ctx)
	svc.processDeliveryBatch(ctx)

	if got := countDeletesForMessage(raceSender.deletes, 610); got != 1 {
		t.Fatalf("cleanup deletes after actionable route = %#v, want PC notice deleted once", raceSender.deletes)
	}
	visible := visibleRecordedMessages(raceSender.messages, raceSender.deletes)
	if len(visible) != 1 {
		t.Fatalf("visible messages after cleanup recovery = %#v, want exactly one actionable card", visible)
	}
	if got, want := buttonLabels(visible[0].buttons), [][]string{{"승인", "거부"}}; !equalStringRows(got, want) {
		t.Fatalf("visible buttons = %#v, want %#v", got, want)
	}
}

func TestMinimalApprovalLiveGenerationChangeRequiresExactReplay(t *testing.T) {
	svc, app, sender := newApprovalTestService(t)
	approve, _ := presentApproval(t, svc, app, sender, "req-generation", "item/commandExecution/requestApproval")
	svc.mu.Lock()
	svc.liveGeneration++
	svc.mu.Unlock()
	if _, err := svc.HandleCallback(context.Background(), 7, 0, 600, 7, approve); !errors.Is(err, ErrCallbackMismatch) {
		t.Fatalf("callback from prior live generation = %v", err)
	}
	if got := app.responseCount("req-generation"); got != 0 {
		t.Fatalf("prior-generation responses = %d", got)
	}
	handleApprovalEvent(svc, app, approvalEvent("req-generation", "item/commandExecution/requestApproval", map[string]any{"command": "same request"}))
	svc.processDeliveryBatch(context.Background())
	if len(sender.messages) != 1 {
		t.Fatalf("exact generation replay presentations = %d", len(sender.messages))
	}
	if _, err := svc.HandleCallback(context.Background(), 7, 0, 600, 7, approve); err != nil {
		t.Fatalf("callback after exact generation replay = %v", err)
	}
	if got := app.responseCount("req-generation"); got != 1 {
		t.Fatalf("post-replay responses = %d", got)
	}
}

func TestMinimalApprovalRestartAfterTelegramAcceptanceDoesNotResendAndSelfBindsOriginalMessage(t *testing.T) {
	svc, app, sender := newApprovalTestService(t)
	handleApprovalEvent(svc, app, approvalEvent("req-accepted-unbound", "item/commandExecution/requestApproval", map[string]any{"command": "safe command"}))
	items, err := svc.store.ClaimDeliveryBatch(context.Background(), 1)
	if err != nil || len(items) != 1 {
		t.Fatalf("ClaimDeliveryBatch = %#v, %v", items, err)
	}
	var payload model.DeliveryPayload
	if err := json.Unmarshal([]byte(items[0].PayloadJSON), &payload); err != nil {
		t.Fatal(err)
	}
	messageID, err := sender.SendMessage(context.Background(), items[0].ChatID, items[0].TopicID, payload.Text, payload.Buttons, notifySendOptions())
	if err != nil || messageID != 600 {
		t.Fatalf("simulated Telegram acceptance = %d, %v", messageID, err)
	}
	approve := payload.Buttons[0][0].CallbackData
	cfg := svc.cfg
	if err := svc.Close(); err != nil {
		t.Fatal(err)
	}

	restarted, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = restarted.Close() })
	restartedSender := &approvalTestSender{}
	restartedApp := newApprovalSession(t, restarted)
	restarted.SetSender(restartedSender)
	restarted.mu.Lock()
	restarted.live = restartedApp
	restarted.liveConnected = true
	restarted.mu.Unlock()
	if backlog, err := restarted.store.DeliveryQueueBacklog(context.Background()); err != nil || backlog != 0 {
		t.Fatalf("ambiguous accepted delivery backlog = %d, %v", backlog, err)
	}
	handleApprovalEvent(restarted, restartedApp, approvalEvent("req-accepted-unbound", "item/commandExecution/requestApproval", map[string]any{"command": "safe command"}))
	restarted.processDeliveryBatch(context.Background())
	if len(restartedSender.messages) != 0 {
		t.Fatalf("ambiguous accepted delivery resent: %#v", restartedSender.messages)
	}
	if _, err := restarted.HandleCallback(context.Background(), 7, 0, messageID, 7, approve); err != nil {
		t.Fatalf("original accepted callback = %v", err)
	}
	approval, _ := restarted.store.GetMinimalApproval(context.Background(), "req-accepted-unbound")
	if approval == nil || approval.Status != "approved" || approval.TelegramMessageID != messageID {
		t.Fatalf("self-bound accepted approval = %#v", approval)
	}
	if got := restartedApp.responseCount("req-accepted-unbound"); got != 1 {
		t.Fatalf("original accepted responses = %d", got)
	}
}

func TestMinimalApprovalReusedRequestIDExpiresStaleTokenForEveryIdentityComponent(t *testing.T) {
	tests := []struct {
		name        string
		threadID    string
		turnID      string
		requestKind string
	}{
		{name: "thread", threadID: "thr-2", turnID: "turn-1", requestKind: "item/commandExecution/requestApproval"},
		{name: "turn", threadID: "thr-1", turnID: "turn-2", requestKind: "item/commandExecution/requestApproval"},
		{name: "kind", threadID: "thr-1", turnID: "turn-1", requestKind: "item/fileChange/requestApproval"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			svc, app, sender := newApprovalTestService(t)
			staleApprove, _ := presentApproval(t, svc, app, sender, "req-reused-live", "item/commandExecution/requestApproval")
			project, err := svc.projectRegistry.Resolve("bridge")
			if err != nil {
				t.Fatal(err)
			}
			app.threadReads = map[string]map[string]any{tc.threadID: approvalThreadReadIdentity(tc.threadID, tc.turnID, project.CanonicalPath)}
			extra := map[string]any{"command": "new command"}
			if tc.requestKind == "item/fileChange/requestApproval" {
				extra = map[string]any{"reason": "new file change"}
			}
			handleApprovalEvent(svc, app, approvalEventIdentity("req-reused-live", tc.threadID, tc.turnID, tc.requestKind, extra))
			svc.processDeliveryBatch(context.Background())
			if len(sender.messages) != 2 {
				t.Fatalf("replacement presentations = %d, want 2", len(sender.messages))
			}
			if len(sender.edits) != 1 || sender.edits[0].messageID != 600 || sender.edits[0].text != "요청이 더 이상 활성 상태가 아닙니다." || len(sender.edits[0].buttons) != 0 {
				t.Fatalf("old replacement keyboard edit = %#v", sender.edits)
			}
			if _, err := svc.HandleCallback(context.Background(), 7, 0, 600, 7, staleApprove); !errors.Is(err, ErrCallbackConsumed) {
				t.Fatalf("stale callback = %v", err)
			}
			if got := app.responseCount("req-reused-live"); got != 0 {
				t.Fatalf("stale callback responses = %d", got)
			}
			freshApprove := sender.messages[1].buttons[0][0].CallbackData
			if _, err := svc.HandleCallback(context.Background(), 7, 0, 600, 7, freshApprove); err != nil {
				t.Fatalf("fresh callback = %v", err)
			}
			if got := app.responseCount("req-reused-live"); got != 1 {
				t.Fatalf("fresh callback responses = %d", got)
			}
		})
	}
}

func TestMinimalApprovalClaimedOldCallbackCannotRespondToSameGenerationReusedRequest(t *testing.T) {
	svc, app, sender := newApprovalTestService(t)
	staleApprove, _ := presentApproval(t, svc, app, sender, "req-claimed-reuse", "item/commandExecution/requestApproval")
	app.currentRequest = approvalRequestIdentity{requestID: "req-claimed-reuse", threadID: "thr-1", turnID: "turn-1", requestKind: "item/commandExecution/requestApproval"}
	project, err := svc.projectRegistry.Resolve("bridge")
	if err != nil {
		t.Fatal(err)
	}
	newIdentity := approvalRequestIdentity{requestID: "req-claimed-reuse", threadID: "thr-2", turnID: "turn-1", requestKind: "item/commandExecution/requestApproval"}
	app.beforeRespond = func() {
		app.mu.Lock()
		app.currentRequest = newIdentity
		app.threadReads = map[string]map[string]any{"thr-2": approvalThreadReadIdentity("thr-2", "turn-1", project.CanonicalPath)}
		app.mu.Unlock()
		handleApprovalEvent(svc, app, approvalEventIdentity(newIdentity.requestID, newIdentity.threadID, newIdentity.turnID, newIdentity.requestKind, map[string]any{"command": "new logical request"}))
	}

	if _, err := svc.HandleCallback(context.Background(), 7, 0, 600, 7, staleApprove); err == nil {
		t.Fatal("stale claimed callback unexpectedly responded")
	}
	if got := app.responseCount("req-claimed-reuse"); got != 0 {
		t.Fatalf("stale claimed callback responses = %d", got)
	}
	approval, _ := svc.store.GetMinimalApproval(context.Background(), "req-claimed-reuse")
	if approval == nil || approval.ThreadID != "thr-2" || approval.TurnID != "turn-1" || approval.RequestKind != newIdentity.requestKind {
		t.Fatalf("replacement approval = %#v", approval)
	}
	if _, err := svc.HandleCallback(context.Background(), 7, 0, 600, 7, staleApprove); !errors.Is(err, ErrCallbackConsumed) {
		t.Fatalf("stale token after replacement = %v", err)
	}
}

func TestMinimalApprovalStaleCleanupCannotEditDeliveredReplacement(t *testing.T) {
	svc, app, sender := newApprovalTestService(t)
	sender.messageIDs = []int64{600, 601}
	staleApprove, _ := presentApproval(t, svc, app, sender, "req-stale-cleanup", "item/commandExecution/requestApproval")
	project, err := svc.projectRegistry.Resolve("bridge")
	if err != nil {
		t.Fatal(err)
	}
	newIdentity := approvalRequestIdentity{requestID: "req-stale-cleanup", threadID: "thr-2", turnID: "turn-1", requestKind: "item/commandExecution/requestApproval"}
	app.beforeRespond = func() {
		app.mu.Lock()
		app.currentRequest = newIdentity
		app.threadReads = map[string]map[string]any{"thr-2": approvalThreadReadIdentity("thr-2", "turn-1", project.CanonicalPath)}
		app.mu.Unlock()
		handleApprovalEvent(svc, app, approvalEventIdentity(newIdentity.requestID, newIdentity.threadID, newIdentity.turnID, newIdentity.requestKind, map[string]any{"command": "new logical request"}))
		svc.processDeliveryBatch(context.Background())
	}

	if _, err := svc.HandleCallback(context.Background(), 7, 0, 600, 7, staleApprove); err == nil {
		t.Fatal("stale claimed callback unexpectedly responded")
	}
	if got := app.responseCount("req-stale-cleanup"); got != 0 {
		t.Fatalf("stale claimed callback responses = %d", got)
	}
	if len(sender.messages) != 2 || sender.messages[1].messageID != 601 || len(sender.messages[1].buttons) != 1 || len(sender.messages[1].buttons[0]) != 2 {
		t.Fatalf("fresh replacement presentation = %#v", sender.messages)
	}
	for _, edit := range sender.edits {
		if edit.messageID != 600 || len(edit.buttons) != 0 {
			t.Fatalf("stale cleanup edited fresh replacement = %#v", sender.edits)
		}
	}
	if len(sender.edits) == 0 || sender.edits[0].text != "요청이 더 이상 활성 상태가 아닙니다." {
		t.Fatalf("old inactive presentation edits = %#v", sender.edits)
	}
	approval, _ := svc.store.GetMinimalApproval(context.Background(), "req-stale-cleanup")
	if approval == nil || approval.ThreadID != "thr-2" || approval.Status != "pending" || approval.TelegramMessageID != 601 {
		t.Fatalf("fresh replacement approval = %#v", approval)
	}
	for _, button := range sender.messages[1].buttons[0] {
		route, err := svc.store.GetMinimalApprovalRoute(context.Background(), button.CallbackData)
		if err != nil || route == nil || route.Status != "active" || route.TelegramMessageID != 601 {
			t.Fatalf("fresh replacement route = %#v, %v", route, err)
		}
	}
}

func TestMinimalApprovalResolvedNotificationExpiresAndRemovesButtons(t *testing.T) {
	svc, app, sender := newApprovalTestService(t)
	approve, _ := presentApproval(t, svc, app, sender, "req-expired", "item/commandExecution/requestApproval")
	svc.handleLiveEvent(context.Background(), app, appserver.Event{Channel: "notification", Method: "serverRequest/resolved", Params: map[string]any{"requestId": "req-expired", "threadId": "thr-1"}})

	approval, _ := svc.store.GetMinimalApproval(context.Background(), "req-expired")
	if approval == nil || approval.Status != "expired" {
		t.Fatalf("approval = %#v", approval)
	}
	if len(sender.edits) != 1 || sender.edits[0].text != "요청이 더 이상 활성 상태가 아닙니다." || len(sender.edits[0].buttons) != 0 {
		t.Fatalf("expired edits = %#v", sender.edits)
	}
	if _, err := svc.HandleCallback(context.Background(), 7, 0, 600, 7, approve); !errors.Is(err, ErrCallbackConsumed) {
		t.Fatalf("expired callback error = %v", err)
	}
	if got := app.responseCount("req-expired"); got != 0 {
		t.Fatalf("expired response count = %d", got)
	}
}

func TestMinimalApprovalResolvedBeforeDeliveryNeverPresentsButtons(t *testing.T) {
	svc, app, sender := newApprovalTestService(t)
	handleApprovalEvent(svc, app, approvalEvent("req-resolved-before-send", "item/commandExecution/requestApproval", map[string]any{"command": "safe"}))
	svc.handleLiveEvent(context.Background(), app, appserver.Event{Channel: "notification", Method: "serverRequest/resolved", Params: map[string]any{"requestId": "req-resolved-before-send", "threadId": "thr-1"}})
	svc.processDeliveryBatch(context.Background())
	if len(sender.messages) != 0 {
		t.Fatalf("expired approval was presented: %#v", sender.messages)
	}
	if backlog, err := svc.store.DeliveryQueueBacklog(context.Background()); err != nil || backlog != 0 {
		t.Fatalf("expired delivery backlog = %d, %v", backlog, err)
	}
}

func TestMinimalApprovalCurrentSessionStaleBecomesExpired(t *testing.T) {
	svc, app, sender := newApprovalTestService(t)
	approve, _ := presentApproval(t, svc, app, sender, "req-stale", "item/commandExecution/requestApproval")
	app.respondErr = errors.New("app-server request is not pending in the current session")
	response, err := svc.HandleCallback(context.Background(), 7, 0, 600, 7, approve)
	if err != nil {
		t.Fatal(err)
	}
	if response == nil || !strings.Contains(response.CallbackText, "비활성") {
		t.Fatalf("response = %#v", response)
	}
	approval, _ := svc.store.GetMinimalApproval(context.Background(), "req-stale")
	if approval == nil || approval.Status != "expired" || len(sender.edits) != 1 || sender.edits[0].text != "요청이 더 이상 활성 상태가 아닙니다." {
		t.Fatalf("stale state/edit = %#v/%#v", approval, sender.edits)
	}
}

func TestMinimalApprovalDefiniteTransportFailureRestoresRoutesButAmbiguousFailureDoesNot(t *testing.T) {
	t.Run("definite", func(t *testing.T) {
		svc, app, sender := newApprovalTestService(t)
		approve, _ := presentApproval(t, svc, app, sender, "req-definite", "item/commandExecution/requestApproval")
		app.respondErr = ErrApprovalTransportUnavailable
		if _, err := svc.HandleCallback(context.Background(), 7, 0, 600, 7, approve); !errors.Is(err, ErrApprovalTransportUnavailable) {
			t.Fatalf("first error = %v", err)
		}
		app.respondErr = nil
		if _, err := svc.HandleCallback(context.Background(), 7, 0, 600, 7, approve); err != nil {
			t.Fatalf("restored callback failed: %v", err)
		}
		if got := app.responseCount("req-definite"); got != 2 {
			t.Fatalf("transport attempts = %d, want 2", got)
		}
	})

	t.Run("ambiguous", func(t *testing.T) {
		svc, app, sender := newApprovalTestService(t)
		approve, _ := presentApproval(t, svc, app, sender, "req-ambiguous", "item/commandExecution/requestApproval")
		app.respondErr = errors.New("write failed after an unknown byte count")
		if _, err := svc.HandleCallback(context.Background(), 7, 0, 600, 7, approve); err == nil {
			t.Fatal("ambiguous response unexpectedly succeeded")
		}
		approval, _ := svc.store.GetMinimalApproval(context.Background(), "req-ambiguous")
		if approval == nil || approval.Status != "cancelled" {
			t.Fatalf("ambiguous approval = %#v", approval)
		}
		app.respondErr = nil
		if _, err := svc.HandleCallback(context.Background(), 7, 0, 600, 7, approve); !errors.Is(err, ErrCallbackConsumed) {
			t.Fatalf("ambiguous callback retried: %v", err)
		}
	})
}

func TestMinimalApprovalSiblingTokenRaceHasOneWinner(t *testing.T) {
	svc, app, sender := newApprovalTestService(t)
	approve, deny := presentApproval(t, svc, app, sender, "req-race", "item/commandExecution/requestApproval")
	app.respondStarted = make(chan struct{})
	app.respondRelease = make(chan struct{})

	firstDone := make(chan error, 1)
	go func() {
		_, err := svc.HandleCallback(context.Background(), 7, 0, 600, 7, approve)
		firstDone <- err
	}()
	<-app.respondStarted
	if _, err := svc.HandleCallback(context.Background(), 7, 0, 600, 7, deny); !errors.Is(err, ErrCallbackConsumed) {
		t.Fatalf("sibling race error = %v", err)
	}
	close(app.respondRelease)
	if err := <-firstDone; err != nil {
		t.Fatalf("winner error = %v", err)
	}
	if got := app.responseCount("req-race"); got != 1 {
		t.Fatalf("responses = %d, want 1", got)
	}
}

func TestMinimalApprovalResolvedRaceDoesNotOverrideClaimWinner(t *testing.T) {
	svc, app, sender := newApprovalTestService(t)
	approve, _ := presentApproval(t, svc, app, sender, "req-live-race", "item/commandExecution/requestApproval")
	app.respondStarted = make(chan struct{})
	app.respondRelease = make(chan struct{})
	done := make(chan error, 1)
	go func() {
		_, err := svc.HandleCallback(context.Background(), 7, 0, 600, 7, approve)
		done <- err
	}()
	<-app.respondStarted
	svc.handleLiveEvent(context.Background(), app, appserver.Event{Channel: "notification", Method: "serverRequest/resolved", Params: map[string]any{"requestId": "req-live-race", "threadId": "thr-1"}})
	close(app.respondRelease)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	approval, _ := svc.store.GetMinimalApproval(context.Background(), "req-live-race")
	if approval == nil || approval.Status != "approved" {
		t.Fatalf("race approval = %#v", approval)
	}
}

func TestMinimalApprovalRemoteSuccessWithFinalizeFailureDurablyRemovesKeyboard(t *testing.T) {
	svc, app, sender := newApprovalTestService(t)
	approve, _ := presentApproval(t, svc, app, sender, "req-finalize-failure", "item/commandExecution/requestApproval")
	app.respondHook = func() {
		db, err := sql.Open("sqlite", svc.store.Path())
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		if _, err := db.Exec(`CREATE TRIGGER fail_minimal_approval_finish BEFORE UPDATE OF status ON minimal_approvals WHEN NEW.status='approved' BEGIN SELECT RAISE(ABORT, 'forced approval finish failure'); END`); err != nil {
			t.Fatal(err)
		}
	}
	sender.editErr = errors.New("telegram edit unavailable")
	response, err := svc.HandleCallback(context.Background(), 7, 0, 600, 7, approve)
	if err != nil {
		t.Fatalf("remote-success callback = %v", err)
	}
	if response == nil || response.CallbackText != "승인됨" {
		t.Fatalf("remote-success response = %#v", response)
	}
	if got := app.responseCount("req-finalize-failure"); got != 1 {
		t.Fatalf("App Server responses = %d", got)
	}
	approval, _ := svc.store.GetMinimalApproval(context.Background(), "req-finalize-failure")
	if approval == nil || approval.Status != "cancelled" || approval.ClaimState != "idle" {
		t.Fatalf("failed-finalize approval = %#v", approval)
	}
	if backlog, err := svc.store.DeliveryQueueBacklog(context.Background()); err != nil || backlog != 1 {
		t.Fatalf("durable inactive-edit backlog = %d, %v", backlog, err)
	}
	if _, err := svc.HandleCallback(context.Background(), 7, 0, 600, 7, approve); !errors.Is(err, ErrCallbackConsumed) {
		t.Fatalf("callback after remote success = %v", err)
	}
	sender.editErr = nil
	svc.processDeliveryBatch(context.Background())
	if len(sender.edits) != 1 || sender.edits[0].messageID != 600 || sender.edits[0].text != "승인됨" || len(sender.edits[0].buttons) != 0 {
		t.Fatalf("durable inactive edit = %#v", sender.edits)
	}
	if backlog, err := svc.store.DeliveryQueueBacklog(context.Background()); err != nil || backlog != 0 {
		t.Fatalf("inactive-edit backlog after delivery = %d, %v", backlog, err)
	}
}

func newApprovalTestService(t *testing.T) (*Service, *approvalSession, *approvalTestSender) {
	t.Helper()
	svc, _ := newMinimalService(t)
	app := newApprovalSession(t, svc)
	sender := &approvalTestSender{}
	svc.SetSender(sender)
	svc.mu.Lock()
	svc.live = app
	svc.liveConnected = true
	svc.mu.Unlock()
	return svc, app, sender
}

func newApprovalSession(t *testing.T, svc *Service) *approvalSession {
	t.Helper()
	project, err := svc.projectRegistry.Resolve("bridge")
	if err != nil {
		t.Fatal(err)
	}
	return &approvalSession{stubSession: stubSession{threadReads: map[string]map[string]any{"thr-1": approvalThreadRead(project.CanonicalPath)}}}
}

func approvalThreadRead(cwd string) map[string]any {
	return approvalThreadReadIdentity("thr-1", "turn-1", cwd)
}

func approvalThreadReadIdentity(threadID, turnID, cwd string) map[string]any {
	return map[string]any{"thread": map[string]any{
		"id": threadID, "cwd": cwd, "status": "inProgress",
		"turns": []any{map[string]any{"id": turnID, "status": "inProgress", "items": []any{}}},
	}}
}

func approvalEvent(requestID, method string, extra map[string]any) appserver.Event {
	return approvalEventIdentity(requestID, "thr-1", "turn-1", method, extra)
}

func approvalEventIdentity(requestID, threadID, turnID, method string, extra map[string]any) appserver.Event {
	params := map[string]any{"threadId": threadID, "turnId": turnID, "itemId": "item-1"}
	for key, value := range extra {
		params[key] = value
	}
	return appserver.Event{Channel: "server_request", Method: method, ID: requestID, Params: params}
}

func handleApprovalEvent(svc *Service, app Session, event appserver.Event) {
	if approvalApp, ok := app.(*approvalSession); ok && event.Channel == "server_request" {
		approvalApp.mu.Lock()
		approvalApp.currentRequest = approvalRequestIdentity{
			requestID: minimalRPCID(event.ID), threadID: payloadMapString(event.Params, "threadId"), turnID: payloadMapString(event.Params, "turnId"), requestKind: event.Method,
		}
		approvalApp.mu.Unlock()
	}
	svc.handleLiveEvent(context.Background(), app, event)
}

func deliveryQueueState(t *testing.T, dbPath, eventID string) (string, int, string) {
	t.Helper()
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	var status, lastError string
	var retryCount int
	if err := db.QueryRow(`SELECT status,retry_count,coalesce(last_error,'') FROM delivery_queue WHERE event_id=?`, eventID).Scan(&status, &retryCount, &lastError); err != nil {
		t.Fatalf("delivery queue state for %s: %v", eventID, err)
	}
	return status, retryCount, lastError
}

func presentApproval(t *testing.T, svc *Service, app *approvalSession, sender *approvalTestSender, requestID, kind string) (string, string) {
	t.Helper()
	extra := map[string]any{"reason": `write C:\private\file.txt`}
	if kind == "item/commandExecution/requestApproval" {
		extra = map[string]any{"command": `powershell -Command "Write-Output ok"`}
	}
	handleApprovalEvent(svc, app, approvalEvent(requestID, kind, extra))
	svc.processDeliveryBatch(context.Background())
	if len(sender.messages) != 1 || len(sender.messages[0].buttons) != 1 || len(sender.messages[0].buttons[0]) != 2 {
		t.Fatalf("approval presentation = %#v", sender.messages)
	}
	return sender.messages[0].buttons[0][0].CallbackData, sender.messages[0].buttons[0][1].CallbackData
}

func acquireApprovalWorker(t *testing.T, svc *Service, threadID, turnID string) (*minimalLinkWorker, *approvalWorkerSession) {
	t.Helper()
	project, err := svc.projectRegistry.Resolve("bridge")
	if err != nil {
		t.Fatal(err)
	}
	workerApp := &approvalWorkerSession{
		approvalSession: &approvalSession{
			stubSession: stubSession{threadReads: map[string]map[string]any{threadID: approvalThreadReadIdentity(threadID, turnID, project.CanonicalPath)}},
		},
		events: make(chan appserver.Event, 1),
	}
	svc.minimalWorkers = newMinimalLinkWorkerManager(func() continuationSession { return workerApp }, 0, svc.logLifecycle)
	worker, err := svc.minimalWorkers.Acquire(context.Background(), "link:"+threadID, threadID)
	if err != nil {
		t.Fatal(err)
	}
	return worker, workerApp
}

type approvalWorkerSpec struct {
	threadID string
	turnID   string
}

func acquireApprovalWorkers(t *testing.T, svc *Service, specs ...approvalWorkerSpec) ([]*minimalLinkWorker, []*approvalWorkerSession) {
	t.Helper()
	project, err := svc.projectRegistry.Resolve("bridge")
	if err != nil {
		t.Fatal(err)
	}
	apps := make([]*approvalWorkerSession, 0, len(specs))
	for _, spec := range specs {
		apps = append(apps, &approvalWorkerSession{
			approvalSession: &approvalSession{
				stubSession: stubSession{threadReads: map[string]map[string]any{spec.threadID: approvalThreadReadIdentity(spec.threadID, spec.turnID, project.CanonicalPath)}},
			},
			events: make(chan appserver.Event, 1),
		})
	}
	var next int
	svc.minimalWorkers = newMinimalLinkWorkerManager(func() continuationSession {
		if next >= len(apps) {
			t.Fatalf("worker factory called %d times for %d apps", next+1, len(apps))
		}
		app := apps[next]
		next++
		return app
	}, 0, svc.logLifecycle)
	workers := make([]*minimalLinkWorker, 0, len(specs))
	for _, spec := range specs {
		worker, err := svc.minimalWorkers.Acquire(context.Background(), "link:"+spec.threadID, spec.threadID)
		if err != nil {
			t.Fatal(err)
		}
		workers = append(workers, worker)
	}
	return workers, apps
}

func emitWorkerUserInput(t *testing.T, svc *Service, worker *minimalLinkWorker, requestID, option string) {
	t.Helper()
	event := workerUserInputEvent(worker, requestID, "turn-input", "item-input", option)
	if workerApp, ok := worker.Session.(*approvalWorkerSession); ok {
		workerApp.setCurrentRequest(event)
	}
	svc.handleMinimalLinkedWorkerEvent(context.Background(), worker, event)
}

func workerUserInputEvent(worker *minimalLinkWorker, requestID, turnID, itemID, option string) appserver.Event {
	return appserver.Event{
		Channel: "server_request",
		Method:  "item/tool/requestUserInput",
		ID:      requestID,
		Params: map[string]any{
			"threadId": worker.ThreadID,
			"turnId":   turnID,
			"itemId":   itemID,
			"questions": []any{map[string]any{
				"id":       "choice",
				"question": "Choose.",
				"options":  []any{map[string]any{"label": option, "description": "Reply through worker."}},
			}},
		},
	}
}

type approvalWorkerSession struct {
	*approvalSession
	events chan appserver.Event
}

func (s *approvalWorkerSession) Subscribe() <-chan appserver.Event {
	return s.events
}

func (s *approvalWorkerSession) ThreadFork(context.Context, string, control.ThreadForkOptions) (map[string]any, error) {
	return nil, nil
}

func (s *approvalWorkerSession) ThreadSetName(context.Context, string, string) (map[string]any, error) {
	return nil, nil
}

func (s *approvalWorkerSession) setCurrentRequest(event appserver.Event) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.currentRequest = approvalRequestIdentity{
		requestID:   minimalRPCID(event.ID),
		threadID:    payloadMapString(event.Params, "threadId"),
		turnID:      payloadMapString(event.Params, "turnId"),
		requestKind: event.Method,
	}
}

type approvalSession struct {
	stubSession
	mu             sync.Mutex
	responses      []respondRequestCall
	respondErr     error
	respondHook    func()
	beforeRespond  func()
	currentRequest approvalRequestIdentity
	respondStarted chan struct{}
	respondRelease chan struct{}
	startedOnce    sync.Once
}

type approvalRequestIdentity struct {
	requestID   string
	threadID    string
	turnID      string
	requestKind string
}

func (s *approvalSession) RespondServerRequest(ctx context.Context, requestID string, result map[string]any) error {
	s.mu.Lock()
	before := s.beforeRespond
	s.beforeRespond = nil
	s.mu.Unlock()
	if before != nil {
		before()
	}
	s.mu.Lock()
	s.responses = append(s.responses, respondRequestCall{requestID: requestID, result: result})
	err := s.respondErr
	hook := s.respondHook
	started := s.respondStarted
	release := s.respondRelease
	s.mu.Unlock()
	if hook != nil {
		hook()
	}
	if started != nil {
		s.startedOnce.Do(func() { close(started) })
	}
	if release != nil {
		select {
		case <-release:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return err
}

func (s *approvalSession) RespondServerRequestExact(ctx context.Context, requestID, threadID, turnID, requestKind string, result map[string]any) error {
	s.mu.Lock()
	before := s.beforeRespond
	s.beforeRespond = nil
	s.mu.Unlock()
	if before != nil {
		before()
	}
	s.mu.Lock()
	current := s.currentRequest
	if current != (approvalRequestIdentity{requestID: requestID, threadID: threadID, turnID: turnID, requestKind: requestKind}) {
		s.mu.Unlock()
		return errors.New("app-server request logical identity changed")
	}
	s.responses = append(s.responses, respondRequestCall{requestID: requestID, result: result})
	err := s.respondErr
	hook := s.respondHook
	started := s.respondStarted
	release := s.respondRelease
	s.mu.Unlock()
	if hook != nil {
		hook()
	}
	if started != nil {
		s.startedOnce.Do(func() { close(started) })
	}
	if release != nil {
		select {
		case <-release:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return err
}

func (s *approvalSession) responsesFor(requestID string) []respondRequestCall {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []respondRequestCall
	for _, call := range s.responses {
		if call.requestID == requestID {
			out = append(out, call)
		}
	}
	return out
}

func (s *approvalSession) responseCount(requestID string) int {
	return len(s.responsesFor(requestID))
}

type approvalTestSender struct {
	recordingSender
	messageIDs []int64
}

func (s *approvalTestSender) SendMessage(ctx context.Context, chatID, topicID int64, text string, buttons [][]model.ButtonSpec, options model.SendOptions) (int64, error) {
	messageID := int64(600)
	if len(s.messages) < len(s.messageIDs) {
		messageID = s.messageIDs[len(s.messages)]
	}
	s.messages = append(s.messages, recordedMessage{chatID: chatID, topicID: topicID, messageID: messageID, text: text, buttons: buttons, options: options})
	return messageID, nil
}

type approvalRaceSender struct {
	recordingSender
	messageIDs           []int64
	afterPollOnlySend    func()
	failActionSends      int
	failActionErr        error
	ambiguousActionSends int
	ambiguousActionErr   error
}

func (s *approvalRaceSender) SendMessage(ctx context.Context, chatID, topicID int64, text string, buttons [][]model.ButtonSpec, options model.SendOptions) (int64, error) {
	if len(buttons) > 0 && s.failActionSends > 0 {
		s.failActionSends--
		err := s.failActionErr
		if err == nil {
			err = errors.New("injected actionable approval send failure")
		}
		return 0, err
	}
	messageID := int64(len(s.messages) + 1)
	if len(s.messages) < len(s.messageIDs) {
		messageID = s.messageIDs[len(s.messages)]
	}
	s.messages = append(s.messages, recordedMessage{chatID: chatID, topicID: topicID, messageID: messageID, text: text, buttons: buttons, options: options})
	if len(buttons) > 0 && s.ambiguousActionSends > 0 {
		s.ambiguousActionSends--
		err := s.ambiguousActionErr
		if err == nil {
			err = errors.New("injected ambiguous actionable approval send failure")
		}
		return messageID, err
	}
	if len(buttons) == 0 && s.afterPollOnlySend != nil {
		hook := s.afterPollOnlySend
		s.afterPollOnlySend = nil
		hook()
	}
	return messageID, nil
}

func visibleRecordedMessages(messages, deletes []recordedMessage) []recordedMessage {
	deleted := map[int64]bool{}
	for _, message := range deletes {
		deleted[message.messageID] = true
	}
	var visible []recordedMessage
	for _, message := range messages {
		if !deleted[message.messageID] {
			visible = append(visible, message)
		}
	}
	return visible
}

func countDeletesForMessage(deletes []recordedMessage, messageID int64) int {
	count := 0
	for _, message := range deletes {
		if message.messageID == messageID {
			count++
		}
	}
	return count
}

func equalStringRows(left, right [][]string) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if len(left[i]) != len(right[i]) {
			return false
		}
		for j := range left[i] {
			if left[i][j] != right[i][j] {
				return false
			}
		}
	}
	return true
}

func equalStringAnyMap(left, right map[string]any) bool {
	if len(left) != len(right) {
		return false
	}
	for key, value := range right {
		if left[key] != value {
			return false
		}
	}
	return true
}
