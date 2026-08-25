package daemon

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/alclssna33/codex_to_telegram/internal/model"
	"github.com/alclssna33/codex_to_telegram/internal/storage"
)

func TestNotifierIngressStartSetsOwnerDMTargetOnly(t *testing.T) {
	svc := newNotifierServiceFromConstructor(t)
	ctx := context.Background()

	response, err := svc.HandleInboundText(ctx, model.InboundText{
		ChatID:     7,
		UserID:     7,
		Text:       "/start",
		ReceivedAt: svc.now(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if response == nil || !strings.Contains(response.Text, "Codex 완료 알림") || strings.Contains(response.Text, "작업폴더") {
		t.Fatalf("response = %#v, want notifier management start response", response)
	}
	target, configured, err := svc.store.GetGlobalObserverTarget(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !configured || target == nil || !target.Enabled || target.ChatID != 7 || target.TopicID != 0 {
		t.Fatalf("target = %#v configured=%t, want enabled owner DM topic 0", target, configured)
	}

	if _, err := svc.HandleInboundText(ctx, model.InboundText{ChatID: -100, TopicID: 10, UserID: 7, Text: "/start", ReceivedAt: svc.now()}); err != nil {
		t.Fatal(err)
	}
	target, configured, err = svc.store.GetGlobalObserverTarget(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !configured || target == nil || target.ChatID != 7 || target.TopicID != 0 {
		t.Fatalf("group /start moved target: target=%#v configured=%t", target, configured)
	}
}

func TestNotifierIngressHelpListsManagementOnlyCommands(t *testing.T) {
	svc := newNotifierServiceFromConstructor(t)

	response, err := svc.HandleInboundText(context.Background(), model.InboundText{
		ChatID:     7,
		UserID:     7,
		Text:       "/help",
		ReceivedAt: svc.now(),
	})
	if err != nil {
		t.Fatal(err)
	}
	want := "관리 명령:\n/start\n/help\n/status\n/repair"
	if response == nil || response.Text != want {
		t.Fatalf("help response = %#v, want %q", response, want)
	}
	for _, forbidden := range []string{"/threads", "/projects", "/bind", "/reply", "/plan", "/approve", "/deny"} {
		if strings.Contains(response.Text, forbidden) {
			t.Fatalf("help exposed forbidden command %q in %q", forbidden, response.Text)
		}
	}
}

func TestNotifierPlainAndUnsupportedSlashStayManagementOnly(t *testing.T) {
	for _, text := range []string{"run tests", "/newchat should not route", "/reply thread secret"} {
		t.Run(text, func(t *testing.T) {
			svc := newNotifierServiceFromConstructor(t)
			global := newRouterSession()
			useRouterSession(svc, global)

			response, err := svc.HandleInboundText(context.Background(), model.InboundText{
				ChatID:     7,
				UserID:     7,
				Text:       text,
				ReceivedAt: svc.now(),
			})
			if err != nil {
				t.Fatal(err)
			}
			if response == nil || response.Text != "현재 봇은 Codex 완료 알림 전용입니다." {
				t.Fatalf("response = %#v, want notifier-only guidance", response)
			}
			assertRouterSessionNoMutations(t, global)
		})
	}
}

func TestNotifierCallbackReturnsInactiveManagementResponseWithoutConsumingRoutes(t *testing.T) {
	svc := newNotifierServiceFromConstructor(t)
	ctx := context.Background()
	route := model.CallbackRoute{
		Token:       "active-legacy-notifier-token",
		Action:      "observe_all",
		Status:      model.CallbackStatusActive,
		PayloadJSON: storage.MustJSON(map[string]any{"private": "must-not-load"}),
		CreatedAt:   model.TimeString(svc.now().Format(time.RFC3339Nano)),
	}
	if err := svc.store.PutCallbackRoute(ctx, route); err != nil {
		t.Fatal(err)
	}

	response, err := svc.HandleCallback(ctx, 7, 0, 44, 7, route.Token)
	if err != nil {
		t.Fatal(err)
	}
	if response == nil || response.Text != "" || !strings.Contains(response.CallbackText, "알림 전용") {
		t.Fatalf("callback response = %#v, want callback-only notifier inactive response", response)
	}
	stored, err := svc.store.GetCallbackRoute(ctx, route.Token)
	if err != nil {
		t.Fatal(err)
	}
	if stored == nil || stored.Status != model.CallbackStatusActive {
		t.Fatalf("callback route was consumed or changed: %#v", stored)
	}
	target, configured, err := svc.store.GetGlobalObserverTarget(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if configured || target != nil {
		t.Fatalf("legacy callback route executed: target=%#v configured=%t", target, configured)
	}
}

func TestNotifierStatusRendersNotifierOnlyOperationalFields(t *testing.T) {
	svc := newNotifierServiceFromConstructor(t)
	ctx := context.Background()
	startedAt := time.Date(2026, 8, 24, 9, 30, 0, 0, time.UTC)
	svc.mu.Lock()
	svc.ready = true
	svc.phase = "ready"
	svc.pollConnected = true
	svc.startedAt = startedAt
	svc.lastError = "token=PRIVATESECRET123 path=C:\\Users\\Alice\\state.sqlite"
	svc.mu.Unlock()
	if err := svc.store.SetGlobalObserverTarget(ctx, 7, 0, true); err != nil {
		t.Fatal(err)
	}
	if err := svc.store.ObserveNotifierThread(ctx, "thread-active", 101, svc.now()); err != nil {
		t.Fatal(err)
	}
	if err := svc.store.ObserveNotifierThread(ctx, "thread-terminal", 102, svc.now()); err != nil {
		t.Fatal(err)
	}
	if err := svc.store.RecordNotifierRead(ctx, "thread-terminal", "turn-1", "completed", 102, false, svc.now()); err != nil {
		t.Fatal(err)
	}
	if err := svc.store.SetState(ctx, notifierSweepCursorKey, "cursor-next"); err != nil {
		t.Fatal(err)
	}

	status, err := svc.StatusSnapshot(ctx, 7, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"Notifier status",
		"Ready: true",
		"Phase: ready",
		"Poll app-server: true",
		"Tracked observations: 2",
		"Active observations: 1",
		"Delivery backlog: 0",
		"Global target: on -> 7:0",
		"Sweep cursor: cursor-next",
		"Started: 2026-08-24T09:30:00Z",
		"Last error: token=<redacted>",
	} {
		if !strings.Contains(status, want) {
			t.Fatalf("status missing %q:\n%s", want, status)
		}
	}
	for _, forbidden := range []string{"Panel mode:", "Mode: Bound", "Mode: Unbound", "Use /threads", "Thread:", "Selected project", "작업폴더"} {
		if strings.Contains(status, forbidden) {
			t.Fatalf("status exposed non-notifier field %q:\n%s", forbidden, status)
		}
	}
	if strings.Contains(status, "PRIVATESECRET123") || strings.Contains(status, "state.sqlite") {
		t.Fatalf("status leaked unsanitized error:\n%s", status)
	}
}
