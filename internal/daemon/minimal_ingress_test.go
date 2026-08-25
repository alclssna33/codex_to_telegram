package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/alclssna33/codex_to_telegram/internal/config"
	"github.com/alclssna33/codex_to_telegram/internal/model"
)

func TestMinimalIngressExpiresOldMessageWithoutStartingCodex(t *testing.T) {
	svc, app := newMinimalService(t)
	response, err := svc.HandleInboundText(context.Background(), model.InboundText{
		ChatID:     100,
		UserID:     7,
		Text:       "run tests",
		ReceivedAt: svc.now().Add(-11 * time.Minute),
	})
	if err != nil {
		t.Fatal(err)
	}
	if app.ThreadStartCalls() != 0 {
		t.Fatal("expired command executed")
	}
	if response == nil || !strings.Contains(response.Text, "만료") {
		t.Fatalf("response = %#v", response)
	}
}

func TestLegacyProfileIgnoresMinimalProjectRegistry(t *testing.T) {
	root := t.TempDir()
	svc, err := New(config.Config{
		Profile:  "legacy",
		Projects: []model.Project{{ID: "ignored", DisplayName: "Ignored", CanonicalPath: filepath.Join(root, "missing")}},
		Paths: config.Paths{
			Home:    root,
			DataDir: filepath.Join(root, "data"),
			LogDir:  filepath.Join(root, "logs"),
			DBPath:  filepath.Join(root, "data", "state.sqlite"),
		},
	})
	if err != nil {
		t.Fatalf("legacy daemon.New rejected minimal-only project data: %v", err)
	}
	t.Cleanup(func() { _ = svc.Close() })
}

func TestMinimalIngressRejectsUnauthorizedUserBeforeContentHandling(t *testing.T) {
	svc, _ := newMinimalService(t)
	response, err := svc.HandleInboundText(context.Background(), model.InboundText{
		ChatID:     100,
		UserID:     999,
		Text:       "/start arbitrary private content",
		ReceivedAt: svc.now(),
	})
	if response != nil || !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("response = %#v, err = %v", response, err)
	}
}

func TestEveryCallbackChecksAllowedUser(t *testing.T) {
	svc, _ := newMinimalService(t)
	if _, err := svc.HandleCallback(context.Background(), 100, 0, 1, 999, "opaque"); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("err = %v", err)
	}
}

func TestMinimalCallbackRejectsActiveLegacyRouteWithoutExecutingHandler(t *testing.T) {
	svc, _ := newMinimalService(t)
	ctx := context.Background()
	route := model.CallbackRoute{
		Token:     "active-legacy-observer-token",
		Action:    "observe_all",
		Status:    model.CallbackStatusActive,
		CreatedAt: model.TimeString(svc.now().Format(time.RFC3339Nano)),
	}
	if err := svc.store.PutCallbackRoute(ctx, route); err != nil {
		t.Fatal(err)
	}

	response, err := svc.HandleCallback(ctx, 7, 0, 55, 7, route.Token)
	if err != nil {
		t.Fatal(err)
	}
	if response == nil || !strings.Contains(response.Text, "/start") {
		t.Fatalf("response = %#v, want minimal stale guidance", response)
	}
	if strings.Contains(response.Text, "/show") || strings.Contains(response.Text, "/repair") {
		t.Fatalf("response exposes legacy guidance: %#v", response)
	}
	target, configured, err := svc.store.GetGlobalObserverTarget(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if configured || target != nil {
		t.Fatalf("legacy observer handler executed: target=%#v configured=%t", target, configured)
	}
}

func TestMinimalAnswerChoiceEmptyRequestFailsClosedWithoutGlobalLive(t *testing.T) {
	svc, _ := newMinimalService(t)
	global := newRouterSession()
	useRouterSession(svc, global)
	ctx := context.Background()
	thread := model.Thread{
		ID:           "pc-plan-thread",
		CWD:          svc.cfg.Projects[0].CanonicalPath,
		Status:       "active",
		ActiveTurnID: "turn-pc",
	}
	if err := svc.store.UpsertThread(ctx, thread); err != nil {
		t.Fatal(err)
	}
	route := model.CallbackRoute{
		Token:       "empty-request-answer-choice",
		Action:      "answer_choice",
		Status:      model.CallbackStatusActive,
		ThreadID:    thread.ID,
		TurnID:      thread.ActiveTurnID,
		PayloadJSON: `{"text":"Option A"}`,
		ExpiresAt:   svc.now().Add(10 * time.Minute).Format(time.RFC3339Nano),
		CreatedAt:   model.TimeString(svc.now().Format(time.RFC3339Nano)),
	}
	if err := svc.store.PutCallbackRoute(ctx, route); err != nil {
		t.Fatal(err)
	}

	response, err := svc.HandleCallback(ctx, 100, 0, 55, 7, route.Token)
	if err != nil {
		t.Fatal(err)
	}
	if response == nil || !strings.Contains(response.Text, "Codex") {
		t.Fatalf("response = %#v, want PC-only/fail-closed guidance", response)
	}
	assertRouterSessionNoMutations(t, global)
}

func TestMinimalCallbackMissingOrConsumedTokenUsesMinimalStaleGuidance(t *testing.T) {
	svc, _ := newMinimalService(t)
	ctx := context.Background()
	consumed := model.CallbackRoute{
		Token:       "consumed-minimal-token",
		Action:      minimalProjectPickAction,
		Status:      model.CallbackStatusExpired,
		ExpiresAt:   svc.now().Add(10 * time.Minute).Format(time.RFC3339Nano),
		PayloadJSON: `{"action":"minimal_project_pick","project_id":"bridge"}`,
		CreatedAt:   model.TimeString(svc.now().Format(time.RFC3339Nano)),
	}
	if err := svc.store.PutCallbackRoute(ctx, consumed); err != nil {
		t.Fatal(err)
	}

	for _, token := range []string{"missing-minimal-token", consumed.Token} {
		response, err := svc.HandleCallback(ctx, 7, 0, 55, 7, token)
		if err != nil {
			t.Fatalf("HandleCallback(%q): %v", token, err)
		}
		if response == nil || !strings.Contains(response.Text, "/start") {
			t.Fatalf("HandleCallback(%q) response = %#v, want minimal stale guidance", token, response)
		}
		if strings.Contains(response.Text, "/show") || strings.Contains(response.Text, "/repair") {
			t.Fatalf("HandleCallback(%q) exposes legacy guidance: %#v", token, response)
		}
	}
}

func TestMinimalHandleMessageUsesCanonicalIngressWithoutGlobalLiveMutation(t *testing.T) {
	cases := []struct {
		name  string
		text  string
		bound bool
	}{
		{name: "new", text: "/new bridge legacy bypass"},
		{name: "newchat", text: "/newchat legacy bypass"},
		{name: "newthread", text: "/newthread legacy bypass"},
		{name: "reply", text: "/reply legacy-thread legacy bypass"},
		{name: "plan", text: "/plan legacy-thread legacy bypass"},
		{name: "plain", text: "legacy bypass", bound: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			svc, _ := newMinimalService(t)
			global := newRouterSession()
			useRouterSession(svc, global)
			workers := installWorkerFactory(svc)
			ctx := context.Background()
			project := svc.cfg.Projects[0]
			if err := svc.store.SetSelectedProject(ctx, 100, 0, project.ID); err != nil {
				t.Fatal(err)
			}
			thread := model.Thread{ID: "legacy-thread", CWD: project.CanonicalPath, Status: "completed"}
			if err := svc.store.UpsertThread(ctx, thread); err != nil {
				t.Fatal(err)
			}
			global.threadReads = map[string]map[string]any{thread.ID: {"thread": map[string]any{
				"id": thread.ID, "cwd": thread.CWD, "status": "completed",
				"turns": []any{map[string]any{"id": "turn-legacy", "status": "completed"}},
			}}}
			if tc.bound {
				if err := svc.store.SetBinding(ctx, 100, 0, thread.ID, model.BindingModeBound); err != nil {
					t.Fatal(err)
				}
			}

			response, err := svc.HandleMessage(ctx, 100, 0, 7, tc.text, 0)
			if err != nil {
				t.Fatal(err)
			}
			if response == nil {
				t.Fatal("HandleMessage returned nil response")
			}
			assertRouterSessionNoMutations(t, global)
			if len(workers.Sessions()) == 0 {
				t.Fatal("HandleMessage did not enter canonical minimal router")
			}
		})
	}
}

func TestMinimalHandleMessageRejectsUnauthorizedUserBeforeReplyShortcut(t *testing.T) {
	svc, app := newMinimalUserInputReplyShortcutTest(t, "req-unauthorized-reply", 710)

	response, err := svc.HandleMessage(context.Background(), 7, 0, 999, "Use reply", 710)
	if response != nil || !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("response=%#v err=%v, want unauthorized rejection", response, err)
	}
	if len(app.respondRequestCalls) != 0 {
		t.Fatalf("reply shortcut responded before auth: %#v", app.respondRequestCalls)
	}
}

func TestMinimalHandleMessageAuthorizedUserKeepsReplyShortcut(t *testing.T) {
	svc, app := newMinimalUserInputReplyShortcutTest(t, "req-authorized-reply", 711)

	response, err := svc.HandleMessage(context.Background(), 7, 0, 7, "Use reply", 711)
	if err != nil {
		t.Fatal(err)
	}
	if response == nil || response.ThreadID != "input-thread" || response.TurnID != "input-turn" {
		t.Fatalf("response=%#v, want input thread/turn", response)
	}
	if len(app.respondRequestCalls) != 1 || app.respondRequestCalls[0].requestID != "req-authorized-reply" {
		t.Fatalf("respond calls=%#v, want one authorized request response", app.respondRequestCalls)
	}
}

func TestMinimalStartAndUnselectedTextReturnSameOpaqueProjectPicker(t *testing.T) {
	svc, _ := newMinimalService(t)
	ctx := context.Background()

	start, err := svc.HandleInboundText(ctx, model.InboundText{ChatID: 7, UserID: 7, Text: "/start", ReceivedAt: svc.now()})
	if err != nil {
		t.Fatal(err)
	}
	plain, err := svc.HandleInboundText(ctx, model.InboundText{ChatID: 7, UserID: 7, Text: "run tests", ReceivedAt: svc.now()})
	if err != nil {
		t.Fatal(err)
	}
	if start == nil || plain == nil || start.Text != plain.Text {
		t.Fatalf("start = %#v, plain = %#v", start, plain)
	}
	if got, want := buttonLabels(start.Buttons), buttonLabels(plain.Buttons); !reflect.DeepEqual(got, want) {
		t.Fatalf("picker labels differ: start=%v plain=%v", got, want)
	}
	if len(start.Buttons) != 2 || len(start.Buttons[0]) != 1 || len(start.Buttons[1]) != 1 {
		t.Fatalf("picker buttons = %#v, want one row per configured project", start.Buttons)
	}

	for _, row := range start.Buttons {
		for _, button := range row {
			if strings.Contains(button.CallbackData, "bridge") || strings.Contains(button.CallbackData, "Second") || strings.Contains(button.CallbackData, svc.cfg.Projects[0].CanonicalPath) {
				t.Fatalf("callback data is not opaque: %q", button.CallbackData)
			}
			route, err := svc.store.GetCallbackRoute(ctx, button.CallbackData)
			if err != nil {
				t.Fatal(err)
			}
			if route == nil || route.Action != minimalProjectPickAction {
				t.Fatalf("callback route = %#v", route)
			}
			var payload map[string]any
			if err := json.Unmarshal([]byte(route.PayloadJSON), &payload); err != nil {
				t.Fatal(err)
			}
			if len(payload) != 2 || payload["action"] != minimalProjectPickAction || strings.TrimSpace(payload["project_id"].(string)) == "" {
				t.Fatalf("callback payload = %#v", payload)
			}
			expiresAt, err := time.Parse(time.RFC3339Nano, route.ExpiresAt)
			if err != nil {
				t.Fatal(err)
			}
			if want := svc.now().Add(10 * time.Minute); !expiresAt.Equal(want) {
				t.Fatalf("expires at = %s, want %s", expiresAt, want)
			}
		}
	}

	target, configured, err := svc.store.GetGlobalObserverTarget(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !configured || target == nil || !target.Enabled || target.ChatID != 7 || target.TopicID != 0 {
		t.Fatalf("observer target = %#v configured=%t", target, configured)
	}
}

func TestMinimalStartDoesNotMoveObserverToGroup(t *testing.T) {
	svc, _ := newMinimalService(t)
	ctx := context.Background()

	if _, err := svc.HandleInboundText(ctx, model.InboundText{ChatID: 100, UserID: 7, Text: "/start", ReceivedAt: svc.now()}); err != nil {
		t.Fatal(err)
	}
	target, configured, err := svc.store.GetGlobalObserverTarget(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if configured || target != nil {
		t.Fatalf("group /start moved observer: target=%#v configured=%t", target, configured)
	}
}

func TestMinimalProjectsCallbackReopensPickerInPlace(t *testing.T) {
	svc, _ := newMinimalService(t)
	sender := &recordingSender{}
	svc.SetSender(sender)
	ctx := context.Background()
	route := model.CallbackRoute{
		Token:       "opaque-project-menu-token",
		Action:      minimalProjectsAction,
		Status:      model.CallbackStatusActive,
		ExpiresAt:   svc.now().Add(10 * time.Minute).Format(time.RFC3339Nano),
		PayloadJSON: `{"action":"minimal_projects","project_id":""}`,
		CreatedAt:   model.TimeString(svc.now().Format(time.RFC3339Nano)),
	}
	if err := svc.store.PutCallbackRoute(ctx, route); err != nil {
		t.Fatal(err)
	}

	response, err := svc.HandleCallback(ctx, 7, 0, 88, 7, route.Token)
	if err != nil {
		t.Fatal(err)
	}
	if response == nil || response.Text != "" || response.CallbackText == "" {
		t.Fatalf("callback response = %#v, want callback-only acknowledgement", response)
	}
	if len(sender.edits) != 1 || sender.edits[0].messageID != 88 || sender.edits[0].text != "작업폴더를 선택하세요." || len(sender.edits[0].buttons) != 2 {
		t.Fatalf("picker edit = %#v", sender.edits)
	}
}

func TestMinimalProjectPickPersistsIDAndEditsProjectActions(t *testing.T) {
	svc, _ := newMinimalService(t)
	sender := &recordingSender{}
	svc.SetSender(sender)
	ctx := context.Background()
	picker, err := svc.HandleInboundText(ctx, model.InboundText{ChatID: 7, UserID: 7, Text: "/start", ReceivedAt: svc.now()})
	if err != nil {
		t.Fatal(err)
	}
	token := picker.Buttons[0][0].CallbackData

	response, err := svc.HandleCallback(ctx, 7, 0, 55, 7, token)
	if err != nil {
		t.Fatal(err)
	}
	if response == nil || response.Text != "" || response.CallbackText == "" {
		t.Fatalf("callback response = %#v, want callback-only acknowledgement", response)
	}
	if len(sender.edits) != 1 {
		t.Fatalf("edits = %#v, want one picker edit", sender.edits)
	}
	edit := sender.edits[0]
	if edit.chatID != 7 || edit.topicID != 0 || edit.messageID != 55 {
		t.Fatalf("edit route = %#v", edit)
	}
	if edit.text != "작업폴더: Bridge\n[새 작업 시작] [기존 대화 열기]" {
		t.Fatalf("edit text = %q", edit.text)
	}
	if got, want := buttonLabels(edit.buttons), [][]string{{"새 작업 시작", "기존 대화 열기"}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("edit buttons = %#v, want %v", edit.buttons, want)
	}
	for _, row := range edit.buttons {
		for _, button := range row {
			route, err := svc.store.ConsumeMinimalPickerRoute(ctx, button.CallbackData, 7, 0, svc.now())
			if err != nil {
				t.Fatal(err)
			}
			if route == nil || route.ProjectID != "bridge" {
				t.Fatalf("minimal picker route for %q = %#v", button.Text, route)
			}
		}
	}
	projectID, err := svc.store.GetSelectedProject(ctx, 7, 0)
	if err != nil {
		t.Fatal(err)
	}
	if projectID != "bridge" {
		t.Fatalf("selected project = %q, want bridge", projectID)
	}
}

func TestMinimalProjectPickSelectionLockRoutingFirstDelaysPickerCallback(t *testing.T) {
	svc, _ := newMinimalService(t)
	app := newRouterSession("ingress-routing-first")
	startEntered := make(chan struct{})
	releaseStart := make(chan struct{})
	released := false
	defer func() {
		if !released {
			close(releaseStart)
		}
	}()
	app.startHook = func() error {
		close(startEntered)
		<-releaseStart
		return nil
	}
	useRouterSession(svc, app)
	ctx := context.Background()
	firstProject := svc.cfg.Projects[0]
	secondProject := svc.cfg.Projects[1]
	mustSelect(t, svc, 100, firstProject.ID)
	picker, err := svc.HandleInboundText(ctx, model.InboundText{ChatID: 100, UserID: 7, Text: "/start", ReceivedAt: svc.now()})
	if err != nil {
		t.Fatal(err)
	}
	secondProjectToken := picker.Buttons[1][0].CallbackData

	submitDone := make(chan error, 1)
	go func() {
		_, err := svc.HandleInboundText(ctx, model.InboundText{ChatID: 100, UserID: 7, Text: "route first via start picker", ReceivedAt: svc.now()})
		submitDone <- err
	}()
	<-startEntered

	callbackDone := make(chan error, 1)
	go func() {
		_, err := svc.HandleCallback(ctx, 100, 0, 55, 7, secondProjectToken)
		callbackDone <- err
	}()
	assertNotDone(t, callbackDone, "minimal project pick callback completed while routing decision was in flight")
	close(releaseStart)
	released = true
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

func TestMinimalProjectPickSelectionLockSelectionFirstRoutesUnderNewChoice(t *testing.T) {
	svc, _ := newMinimalService(t)
	app := newRouterSession("ingress-selection-first")
	useRouterSession(svc, app)
	sender := newBlockingEditSender()
	svc.SetSender(sender)
	released := false
	defer func() {
		if !released {
			close(sender.release)
		}
	}()
	ctx := context.Background()
	firstProject := svc.cfg.Projects[0]
	secondProject := svc.cfg.Projects[1]
	mustSelect(t, svc, 100, firstProject.ID)
	picker, err := svc.HandleInboundText(ctx, model.InboundText{ChatID: 100, UserID: 7, Text: "/start", ReceivedAt: svc.now()})
	if err != nil {
		t.Fatal(err)
	}
	secondProjectToken := picker.Buttons[1][0].CallbackData

	callbackDone := make(chan error, 1)
	go func() {
		_, err := svc.HandleCallback(ctx, 100, 0, 55, 7, secondProjectToken)
		callbackDone <- err
	}()
	<-sender.entered

	submitDone := make(chan error, 1)
	go func() {
		_, err := svc.HandleInboundText(ctx, model.InboundText{ChatID: 100, UserID: 7, Text: "selection first via start picker", ReceivedAt: svc.now()})
		submitDone <- err
	}()
	assertNotDone(t, submitDone, "plain prompt routed before minimal project pick callback finished")
	if got := app.ThreadStartCalls(); got != 0 {
		t.Fatalf("ThreadStart calls while picker callback is in flight = %d, want 0", got)
	}
	close(sender.release)
	released = true
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

func TestMinimalProjectCallbackExpiresAfterTenMinutes(t *testing.T) {
	svc, _ := newMinimalService(t)
	ctx := context.Background()
	picker, err := svc.HandleInboundText(ctx, model.InboundText{ChatID: 7, UserID: 7, Text: "/start", ReceivedAt: svc.now()})
	if err != nil {
		t.Fatal(err)
	}
	token := picker.Buttons[0][0].CallbackData
	svc.now = func() time.Time { return minimalTestNow.Add(11 * time.Minute) }

	response, err := svc.HandleCallback(ctx, 7, 0, 55, 7, token)
	if err != nil {
		t.Fatal(err)
	}
	if response == nil || !strings.Contains(response.Text, "만료") {
		t.Fatalf("response = %#v, want expiry notice", response)
	}
	projectID, err := svc.store.GetSelectedProject(ctx, 7, 0)
	if err != nil {
		t.Fatal(err)
	}
	if projectID != "" {
		t.Fatalf("expired callback selected project %q", projectID)
	}
}

var minimalTestNow = time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)

func newMinimalService(t *testing.T) (*Service, *minimalTestSession) {
	t.Helper()
	root := t.TempDir()
	firstProject := filepath.Join(root, "bridge")
	secondProject := filepath.Join(root, "second")
	for _, path := range []string{firstProject, secondProject} {
		if err := ensureTestDir(path); err != nil {
			t.Fatal(err)
		}
	}
	cfg := config.Config{
		Profile:        "minimal",
		CommandMaxAge:  10 * time.Minute,
		AllowedUserIDs: []int64{7},
		Projects: []model.Project{
			{ID: "bridge", DisplayName: "Bridge", CanonicalPath: firstProject},
			{ID: "second", DisplayName: "Second", CanonicalPath: secondProject},
		},
		Paths: config.Paths{
			Home:    root,
			DataDir: filepath.Join(root, "data"),
			LogDir:  filepath.Join(root, "logs"),
			DBPath:  filepath.Join(root, "data", "state.sqlite"),
		},
	}
	svc, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = svc.Close() })
	svc.now = func() time.Time { return minimalTestNow }
	app := &minimalTestSession{}
	svc.live = app
	return svc, app
}

func ensureTestDir(path string) error {
	return os.MkdirAll(path, 0o755)
}

type minimalTestSession struct {
	stubSession
}

func (s *minimalTestSession) ThreadStartCalls() int {
	return len(s.threadStartCalls)
}

func newMinimalUserInputReplyShortcutTest(t *testing.T, requestID string, messageID int64) (*Service, *minimalTestSession) {
	t.Helper()
	svc, app := newMinimalService(t)
	ctx := context.Background()
	svc.mu.Lock()
	svc.liveConnected = true
	svc.mu.Unlock()
	thread := model.Thread{ID: "input-thread", CWD: svc.cfg.Projects[0].CanonicalPath, Status: "active", ActiveTurnID: "input-turn"}
	if err := svc.store.UpsertThread(ctx, thread); err != nil {
		t.Fatal(err)
	}
	if err := svc.store.SavePendingApproval(ctx, model.PendingApproval{
		RequestID:       requestID,
		ThreadID:        thread.ID,
		TurnID:          thread.ActiveTurnID,
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
	if err := svc.store.PutMessageRoute(ctx, model.MessageRoute{
		ChatID:    7,
		MessageID: messageID,
		ThreadID:  thread.ID,
		TurnID:    thread.ActiveTurnID,
		EventID:   "plan_request:" + requestID,
		CreatedAt: model.NowString(),
	}); err != nil {
		t.Fatal(err)
	}
	return svc, app
}

func buttonLabels(rows [][]model.ButtonSpec) [][]string {
	out := make([][]string, 0, len(rows))
	for _, row := range rows {
		labels := make([]string, 0, len(row))
		for _, button := range row {
			labels = append(labels, button.Text)
		}
		out = append(out, labels)
	}
	return out
}
