package storage

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alclssna33/codex_to_telegram/internal/model"
	"github.com/alclssna33/codex_to_telegram/internal/securestore"

	sqlite3 "modernc.org/sqlite/lib"
)

func storageThreadIDs(threads []model.Thread) []string {
	out := make([]string, 0, len(threads))
	for _, thread := range threads {
		out = append(out, thread.ID)
	}
	return out
}

func TestGlobalObserverTargetPersistsAndObserveOffDisablesMonitoring(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "state.sqlite")
	store, err := Open(path)
	if err != nil {
		t.Fatalf("Open(%s) failed: %v", path, err)
	}
	t.Cleanup(func() {
		_ = store.Close()
	})
	ctx := context.Background()

	if err := store.SetGlobalObserverTarget(ctx, 123456789, 9, true); err != nil {
		t.Fatalf("SetGlobalObserverTarget(enable) failed: %v", err)
	}
	enabledRaw, err := store.GetState(ctx, "observer.global_enabled")
	if err != nil {
		t.Fatalf("GetState(observer.global_enabled) failed: %v", err)
	}
	if enabledRaw != "true" {
		t.Fatalf("observer.global_enabled = %q, want true", enabledRaw)
	}
	chatIDRaw, err := store.GetState(ctx, "observer.global_chat_id")
	if err != nil {
		t.Fatalf("GetState(observer.global_chat_id) failed: %v", err)
	}
	if chatIDRaw != "123456789" {
		t.Fatalf("observer.global_chat_id = %q, want 123456789", chatIDRaw)
	}
	topicIDRaw, err := store.GetState(ctx, "observer.global_topic_id")
	if err != nil {
		t.Fatalf("GetState(observer.global_topic_id) failed: %v", err)
	}
	if topicIDRaw != "9" {
		t.Fatalf("observer.global_topic_id = %q, want 9", topicIDRaw)
	}
	sinceRaw, err := store.GetState(ctx, "observer.global_since_unix")
	if err != nil {
		t.Fatalf("GetState(observer.global_since_unix) failed: %v", err)
	}
	if sinceRaw == "" {
		t.Fatal("observer.global_since_unix must be set when global observer is enabled")
	}

	reopened, err := Open(path)
	if err != nil {
		t.Fatalf("Open(reopened) failed: %v", err)
	}
	defer reopened.Close()

	target, configured, err := reopened.GetGlobalObserverTarget(ctx)
	if err != nil {
		t.Fatalf("GetGlobalObserverTarget(reopened) failed: %v", err)
	}
	if !configured {
		t.Fatal("GetGlobalObserverTarget(reopened) should report configured=true")
	}
	if target == nil {
		t.Fatal("GetGlobalObserverTarget(reopened) returned nil target")
	}
	if target.ChatID != 123456789 || target.TopicID != 9 || !target.Enabled {
		t.Fatalf("reopened global target = %#v, want enabled target 123456789:9", target)
	}
	sinceUnix, ok, err := reopened.GetGlobalObserverSinceUnix(ctx)
	if err != nil {
		t.Fatalf("GetGlobalObserverSinceUnix(reopened) failed: %v", err)
	}
	if !ok || sinceUnix <= 0 {
		t.Fatalf("GetGlobalObserverSinceUnix(reopened) = %d ok=%t, want positive value", sinceUnix, ok)
	}

	if err := reopened.SetGlobalObserverTarget(ctx, 123456789, 9, false); err != nil {
		t.Fatalf("SetGlobalObserverTarget(disable) failed: %v", err)
	}
	target, configured, err = reopened.GetGlobalObserverTarget(ctx)
	if err != nil {
		t.Fatalf("GetGlobalObserverTarget(disabled) failed: %v", err)
	}
	if !configured {
		t.Fatal("GetGlobalObserverTarget(disabled) should remain configured")
	}
	if target != nil {
		t.Fatalf("disabled global target = %#v, want nil", target)
	}
	enabledRaw, err = reopened.GetState(ctx, "observer.global_enabled")
	if err != nil {
		t.Fatalf("GetState(observer.global_enabled after disable) failed: %v", err)
	}
	if enabledRaw != "false" {
		t.Fatalf("observer.global_enabled after disable = %q, want false", enabledRaw)
	}
}

func TestBindingAndGlobalObserverCanCoexist(t *testing.T) {
	t.Parallel()

	store := openTestStore(t)
	ctx := context.Background()

	if err := store.SetGlobalObserverTarget(ctx, 123456789, 0, true); err != nil {
		t.Fatalf("SetGlobalObserverTarget failed: %v", err)
	}
	if err := store.SetBinding(ctx, 123456789, 0, "thread-1", model.BindingModeBound); err != nil {
		t.Fatalf("SetBinding failed: %v", err)
	}

	binding, err := store.GetBinding(ctx, 123456789, 0)
	if err != nil {
		t.Fatalf("GetBinding failed: %v", err)
	}
	if binding == nil || binding.ThreadID != "thread-1" {
		t.Fatalf("binding = %#v, want thread-1", binding)
	}

	target, configured, err := store.GetGlobalObserverTarget(ctx)
	if err != nil {
		t.Fatalf("GetGlobalObserverTarget failed: %v", err)
	}
	if !configured || target == nil {
		t.Fatalf("global observer target = %#v configured=%t, want enabled target", target, configured)
	}
	if target.ChatID != 123456789 || target.TopicID != 0 {
		t.Fatalf("global observer target = %#v, want 123456789:0", target)
	}
}

func TestCallbackRouteSessionIdentityMigrationDefaultsLegacyRows(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "state.sqlite")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`
		CREATE TABLE callback_routes (
			route_token TEXT PRIMARY KEY,
			action TEXT NOT NULL,
			thread_id TEXT NOT NULL,
			turn_id TEXT,
			request_id TEXT,
			telegram_message_id INTEGER,
			status TEXT NOT NULL,
			expires_at TEXT,
			payload_json TEXT NOT NULL,
			created_at TEXT NOT NULL
		);
		INSERT INTO callback_routes(route_token,action,thread_id,turn_id,request_id,telegram_message_id,status,expires_at,payload_json,created_at)
		VALUES('legacy-token','answer_choice','thread-legacy','turn-legacy','request-legacy',700,'active',NULL,'{"text":"Yes"}','2026-08-23T00:00:00Z');
	`); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	ctx := context.Background()
	legacy, err := store.GetCallbackRoute(ctx, "legacy-token")
	if err != nil || legacy == nil || legacy.SessionIdentity != "" {
		t.Fatalf("legacy route=%#v err=%v, want default empty session identity", legacy, err)
	}
	if err := store.PutCallbackRoute(ctx, model.CallbackRoute{
		Token:           "worker-token",
		Action:          "answer_choice",
		ThreadID:        "thread-worker",
		TurnID:          "turn-worker",
		RequestID:       "request-worker",
		SessionIdentity: "minimal-link-worker:2",
		Status:          model.CallbackStatusActive,
		PayloadJSON:     `{"text":"No"}`,
		CreatedAt:       model.NowString(),
	}); err != nil {
		t.Fatal(err)
	}
	worker, err := store.GetCallbackRoute(ctx, "worker-token")
	if err != nil || worker == nil || worker.SessionIdentity != "minimal-link-worker:2" {
		t.Fatalf("worker route=%#v err=%v, want persisted session identity", worker, err)
	}
}

func TestPendingApprovalWireRequestIDMigrationDefaultsLegacyRows(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "state.sqlite")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`
		CREATE TABLE pending_approvals (
			request_id TEXT PRIMARY KEY,
			thread_id TEXT NOT NULL,
			turn_id TEXT,
			item_id TEXT,
			prompt_kind TEXT NOT NULL,
			question TEXT,
			status TEXT NOT NULL,
			telegram_message_id INTEGER,
			payload_json TEXT NOT NULL,
			updated_at TEXT NOT NULL
		);
		INSERT INTO pending_approvals(request_id,thread_id,turn_id,item_id,prompt_kind,question,status,telegram_message_id,payload_json,updated_at)
		VALUES('legacy-input','thread-legacy','turn-legacy','item-legacy','user_input','Choose.','pending',0,'{"questions":[]}','2026-08-23T00:00:00Z');
	`); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	legacy, err := store.GetPendingApproval(context.Background(), "legacy-input")
	if err != nil || legacy == nil {
		t.Fatalf("legacy pending=%#v err=%v", legacy, err)
	}
	if legacy.WireRequestID != "legacy-input" {
		t.Fatalf("legacy wire request id=%q, want request_id fallback", legacy.WireRequestID)
	}
	if legacy.RequestKind != "user_input" {
		t.Fatalf("legacy request kind=%q, want prompt_kind fallback", legacy.RequestKind)
	}
	if legacy.SessionIdentity != "" {
		t.Fatalf("legacy session identity=%q, want empty", legacy.SessionIdentity)
	}
}

func TestPendingApprovalScopedWireRequestIDUpsertRefreshesLogicalFields(t *testing.T) {
	t.Parallel()

	store := openTestStore(t)
	ctx := context.Background()
	identity := "minimal-link-worker:scope-a"
	durable := model.ScopedRequestID(identity, "1")
	if err := store.SavePendingApproval(ctx, model.PendingApproval{
		RequestID:       "1",
		WireRequestID:   "1",
		ThreadID:        "thread-old",
		TurnID:          "turn-old",
		ItemID:          "item-old",
		PromptKind:      "user_input",
		RequestKind:     "item/tool/requestUserInput",
		Question:        "Old question?",
		SessionIdentity: identity,
		Status:          "resolved:choice",
		PayloadJSON:     `{"questions":[{"id":"choice","question":"Old?","options":[{"label":"Old option"}]}]}`,
		UpdatedAt:       model.NowString(),
	}); err != nil {
		t.Fatal(err)
	}
	var rawRows int
	if err := store.db.QueryRowContext(ctx, `SELECT count(*) FROM pending_approvals WHERE request_id='1'`).Scan(&rawRows); err != nil || rawRows != 0 {
		t.Fatalf("raw worker primary-key rows=%d err=%v, want none", rawRows, err)
	}
	if err := store.SavePendingApproval(ctx, model.PendingApproval{
		RequestID:       "1",
		WireRequestID:   "1",
		ThreadID:        "thread-new",
		TurnID:          "turn-new",
		ItemID:          "item-new",
		PromptKind:      "user_input",
		RequestKind:     "item/tool/requestUserInput",
		Question:        "New question?",
		SessionIdentity: identity,
		Status:          "pending",
		PayloadJSON:     `{"questions":[{"id":"choice","question":"New?","options":[{"label":"New option"}]}]}`,
		UpdatedAt:       model.NowString(),
	}); err != nil {
		t.Fatal(err)
	}

	loaded, err := store.GetPendingApproval(ctx, durable)
	if err != nil || loaded == nil {
		t.Fatalf("scoped pending=%#v err=%v", loaded, err)
	}
	if loaded.RequestID != durable || loaded.WireRequestID != "1" {
		t.Fatalf("request ids=%q/%q, want durable %q and wire 1", loaded.RequestID, loaded.WireRequestID, durable)
	}
	if loaded.ThreadID != "thread-new" || loaded.TurnID != "turn-new" || loaded.ItemID != "item-new" || loaded.Question != "New question?" || loaded.Status != "pending" {
		t.Fatalf("loaded pending=%#v, want refreshed logical fields", loaded)
	}
	if strings.Contains(loaded.PayloadJSON, "Old option") || !strings.Contains(loaded.PayloadJSON, "New option") {
		t.Fatalf("payload=%s, want new option only", loaded.PayloadJSON)
	}
}

func TestPendingApprovalWireLookupRequiresExactSessionOrLegacyEmptyIdentity(t *testing.T) {
	store := newProtectedMinimalStore(t)
	ctx := context.Background()
	workerIdentity := "minimal-link-worker:wire-scope:1"
	staleIdentity := "minimal-link-worker:wire-scope:stale"
	if err := store.SavePendingApproval(ctx, model.PendingApproval{
		RequestID:       "1",
		WireRequestID:   "1",
		ThreadID:        "thread-worker",
		TurnID:          "turn-worker",
		PromptKind:      "user_input",
		RequestKind:     "item/tool/requestUserInput",
		Question:        "Worker choice?",
		SessionIdentity: workerIdentity,
		Status:          "pending",
		PayloadJSON:     `{"questions":[{"id":"choice","question":"Worker choice?"}]}`,
		UpdatedAt:       model.NowString(),
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.SavePendingApproval(ctx, model.PendingApproval{
		RequestID:     "legacy-durable",
		WireRequestID: "1",
		ThreadID:      "thread-legacy",
		TurnID:        "turn-legacy",
		PromptKind:    "user_input",
		RequestKind:   "item/tool/requestUserInput",
		Question:      "Legacy choice?",
		Status:        "pending",
		PayloadJSON:   `{"questions":[{"id":"choice","question":"Legacy choice?"}]}`,
		UpdatedAt:     model.NowString(),
	}); err != nil {
		t.Fatal(err)
	}

	legacy, err := store.GetPendingApproval(ctx, "1")
	if err != nil || legacy == nil || legacy.RequestID != "legacy-durable" || legacy.SessionIdentity != "" {
		t.Fatalf("session-blind wire lookup=%#v err=%v, want legacy empty-identity row only", legacy, err)
	}
	worker, err := store.GetPendingApprovalForSession(ctx, "1", workerIdentity)
	if err != nil || worker == nil || worker.SessionIdentity != workerIdentity || worker.WireRequestID != "1" {
		t.Fatalf("worker session lookup=%#v err=%v, want exact worker row", worker, err)
	}
	stale, err := store.GetPendingApprovalForSession(ctx, "1", staleIdentity)
	if err != nil || stale != nil {
		t.Fatalf("stale session lookup=%#v err=%v, want no row", stale, err)
	}
	if err := store.UpdatePendingApprovalStatusForSession(ctx, "1", staleIdentity, "resolved:stale"); err != nil {
		t.Fatal(err)
	}
	worker, err = store.GetPendingApprovalForSession(ctx, "1", workerIdentity)
	if err != nil || worker == nil || worker.Status != "pending" {
		t.Fatalf("worker after stale update=%#v err=%v, want still pending", worker, err)
	}
	if err := store.UpdatePendingApprovalStatusForSession(ctx, "1", workerIdentity, "resolved:worker"); err != nil {
		t.Fatal(err)
	}
	worker, err = store.GetPendingApprovalForSession(ctx, "1", workerIdentity)
	if err != nil || worker == nil || worker.Status != "resolved:worker" {
		t.Fatalf("worker after exact update=%#v err=%v, want resolved", worker, err)
	}
}

func TestMinimalApprovalWireLookupRequiresExactSessionOrLegacyEmptyIdentity(t *testing.T) {
	store := newProtectedMinimalStore(t)
	ctx := context.Background()
	workerIdentity := "minimal-link-worker:minimal-wire:1"
	staleIdentity := "minimal-link-worker:minimal-wire:stale"
	workerID := model.ScopedRequestID(workerIdentity, "1")
	created, err := store.CreateMinimalApproval(ctx, MinimalApprovalSeed{
		Approval:     MinimalApproval{RequestID: "1", WireRequestID: "1", ThreadID: "thread-worker", TurnID: "turn-worker", RequestKind: "item/commandExecution/requestApproval", ProjectName: "Bridge", SessionIdentity: workerIdentity, Status: "pending"},
		ApproveToken: "abababababababababababababababab",
		DenyToken:    "bcbcbcbcbcbcbcbcbcbcbcbcbcbcbcbc",
		Delivery:     model.DeliveryQueueItem{EventID: "worker-approval", ChatKey: model.ChatKey(7, 0), ChatID: 7, ThreadID: "thread-worker", Kind: "minimal_approval", Status: model.DeliveryStatusPending, AvailableAt: model.TimeString("2000-01-01T00:00:00Z"), PayloadJSON: MustJSON(model.DeliveryPayload{Text: "approval", ThreadID: "thread-worker", TurnID: "turn-worker", EventID: workerID})},
	})
	if err != nil || !created {
		t.Fatalf("CreateMinimalApproval created=%t err=%v", created, err)
	}
	if _, err := store.db.ExecContext(ctx, `
		INSERT INTO minimal_approvals(request_id,wire_request_id,thread_id,turn_id,request_kind,project_name,session_identity,status,claim_state,updated_at)
		VALUES(?,?,?,?,?,?,?,?,?,?)`,
		"legacy-minimal", "1", "thread-legacy", "turn-legacy", "item/commandExecution/requestApproval", "Bridge", "", "pending", "idle", string(model.NowString()),
	); err != nil {
		t.Fatal(err)
	}

	legacy, err := store.GetMinimalApproval(ctx, "1")
	if err != nil || legacy == nil || legacy.RequestID != "legacy-minimal" || legacy.SessionIdentity != "" {
		t.Fatalf("session-blind minimal lookup=%#v err=%v, want legacy empty-identity row only", legacy, err)
	}
	worker, err := store.GetMinimalApprovalForSession(ctx, "1", workerIdentity)
	if err != nil || worker == nil || worker.RequestID != workerID || worker.SessionIdentity != workerIdentity {
		t.Fatalf("worker minimal lookup=%#v err=%v, want exact worker row", worker, err)
	}
	stale, err := store.GetMinimalApprovalForSession(ctx, "1", staleIdentity)
	if err != nil || stale != nil {
		t.Fatalf("stale minimal lookup=%#v err=%v, want no row", stale, err)
	}
	if expired, err := store.ExpireMinimalApprovalForSession(ctx, "1", staleIdentity); err != nil || expired {
		t.Fatalf("stale expire changed=%t err=%v, want false nil", expired, err)
	}
	worker, err = store.GetMinimalApprovalForSession(ctx, "1", workerIdentity)
	if err != nil || worker == nil || worker.Status != "pending" {
		t.Fatalf("worker after stale expire=%#v err=%v, want still pending", worker, err)
	}
	if expired, err := store.ExpireMinimalApprovalForSession(ctx, "1", workerIdentity); err != nil || !expired {
		t.Fatalf("exact expire changed=%t err=%v, want true nil", expired, err)
	}
	worker, err = store.GetMinimalApprovalForSession(ctx, "1", workerIdentity)
	if err != nil || worker == nil || worker.Status != "expired" {
		t.Fatalf("worker after exact expire=%#v err=%v, want expired", worker, err)
	}
}

func TestProtectedPendingApprovalAndCallbackRoutePayloadsAtRest(t *testing.T) {
	store := newProtectedMinimalStore(t)
	ctx := context.Background()
	identity := "minimal-link-worker:protected:1"
	pendingPayload := `{"questions":[{"id":"choice","question":"Secret pending marker 2167.","options":[{"label":"Blue"}]}]}`
	callbackPayload := `{"text":"Secret callback marker 3791."}`
	if err := store.SavePendingApproval(ctx, model.PendingApproval{
		RequestID:       "protected-pending",
		WireRequestID:   "protected-pending",
		ThreadID:        "thread-protected",
		TurnID:          "turn-protected",
		PromptKind:      "user_input",
		RequestKind:     "item/tool/requestUserInput",
		Question:        "Secret pending marker 2167.",
		SessionIdentity: identity,
		Status:          "pending",
		PayloadJSON:     pendingPayload,
		UpdatedAt:       model.NowString(),
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.PutCallbackRoute(ctx, model.CallbackRoute{
		Token:           "protected-callback",
		Action:          "answer_choice",
		ThreadID:        "thread-protected",
		TurnID:          "turn-protected",
		RequestID:       model.ScopedRequestID(identity, "protected-pending"),
		SessionIdentity: identity,
		Status:          model.CallbackStatusActive,
		PayloadJSON:     callbackPayload,
		CreatedAt:       model.NowString(),
	}); err != nil {
		t.Fatal(err)
	}
	var rawQuestion, rawPending, rawCallback string
	if err := store.db.QueryRowContext(ctx, `SELECT coalesce(question,''), payload_json FROM pending_approvals WHERE request_id=?`, model.ScopedRequestID(identity, "protected-pending")).Scan(&rawQuestion, &rawPending); err != nil {
		t.Fatal(err)
	}
	if err := store.db.QueryRowContext(ctx, `SELECT payload_json FROM callback_routes WHERE route_token=?`, "protected-callback").Scan(&rawCallback); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(rawQuestion, "Secret pending marker") || rawQuestion == "Secret pending marker 2167." {
		t.Fatalf("pending question stored plaintext: %q", rawQuestion)
	}
	if strings.Contains(rawPending, "Secret pending marker") || rawPending == pendingPayload {
		t.Fatalf("pending payload stored plaintext: %q", rawPending)
	}
	if strings.Contains(rawCallback, "Secret callback marker") || rawCallback == callbackPayload {
		t.Fatalf("callback payload stored plaintext: %q", rawCallback)
	}
	pending, err := store.GetPendingApprovalForSession(ctx, "protected-pending", identity)
	if err != nil || pending == nil || pending.PayloadJSON != pendingPayload {
		t.Fatalf("pending round trip=%#v err=%v", pending, err)
	}
	callback, err := store.GetCallbackRoute(ctx, "protected-callback")
	if err != nil || callback == nil || callback.PayloadJSON != callbackPayload {
		t.Fatalf("callback round trip=%#v err=%v", callback, err)
	}
}

func TestProtectedLatestPendingApprovalForThreadUnprotectsOnlyInMemory(t *testing.T) {
	store := newProtectedMinimalStore(t)
	ctx := context.Background()
	sentinel := "Panel pending sentinel 6317."
	payload := `{"questions":[{"id":"choice","question":"Panel pending sentinel 6317.","options":[{"label":"Plain button"}]}]}`
	if err := store.SavePendingApproval(ctx, model.PendingApproval{
		RequestID:       "panel-protected-input",
		WireRequestID:   "panel-protected-input",
		ThreadID:        "thread-panel-protected",
		TurnID:          "turn-panel-protected",
		PromptKind:      "user_input",
		RequestKind:     "item/tool/requestUserInput",
		Question:        sentinel,
		SessionIdentity: "minimal-link-worker:panel-protected:1",
		Status:          "pending",
		PayloadJSON:     payload,
		UpdatedAt:       model.NowString(),
	}); err != nil {
		t.Fatal(err)
	}

	raw := readRawSQLiteFile(t, store.Path())
	if bytes.Contains(raw, []byte(sentinel)) || bytes.Contains(raw, []byte("Plain button")) {
		t.Fatal("protected panel pending payload leaked into sqlite db/wal/shm")
	}
	pending, err := store.GetLatestPendingApprovalForThread(ctx, "thread-panel-protected")
	if err != nil || pending == nil {
		t.Fatalf("latest protected pending=%#v err=%v", pending, err)
	}
	if pending.Question != sentinel || !strings.Contains(pending.PayloadJSON, sentinel) || !strings.Contains(pending.PayloadJSON, "Plain button") {
		t.Fatalf("latest pending plaintext question/payload=%#v", pending)
	}
}

func TestProtectedLatestPendingApprovalForThreadDecryptFailureFailsClosed(t *testing.T) {
	store := newProtectedMinimalStore(t)
	ctx := context.Background()
	if err := store.SavePendingApproval(ctx, model.PendingApproval{
		RequestID:       "panel-corrupt-input",
		WireRequestID:   "panel-corrupt-input",
		ThreadID:        "thread-panel-corrupt",
		TurnID:          "turn-panel-corrupt",
		PromptKind:      "user_input",
		RequestKind:     "item/tool/requestUserInput",
		Question:        "Corrupt protected question.",
		SessionIdentity: "minimal-link-worker:panel-corrupt:1",
		Status:          "pending",
		PayloadJSON:     `{"questions":[{"id":"choice","question":"Corrupt protected question.","options":[{"label":"Bad button"}]}]}`,
		UpdatedAt:       model.NowString(),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `UPDATE pending_approvals SET payload_json=? WHERE thread_id=?`, "dpapi:v1:not-readable!", "thread-panel-corrupt"); err != nil {
		t.Fatal(err)
	}

	pending, err := store.GetLatestPendingApprovalForThread(ctx, "thread-panel-corrupt")
	if err == nil || pending != nil {
		t.Fatalf("corrupt protected pending=%#v err=%v, want nil plus decrypt error", pending, err)
	}
}

func TestOpenWithProtectorMigratesPendingApprovalAndCallbackPayloads(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.sqlite")
	ctx := context.Background()
	pendingPayload := `{"questions":[{"id":"choice","question":"Legacy pending secret marker 4812."}]}`
	callbackPayload := `{"text":"Legacy callback secret marker 5912."}`
	legacy, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := legacy.SavePendingApproval(ctx, model.PendingApproval{
		RequestID:   "legacy-pending-input",
		ThreadID:    "thread-legacy",
		TurnID:      "turn-legacy",
		PromptKind:  "user_input",
		RequestKind: "item/tool/requestUserInput",
		Question:    "Legacy pending secret marker 4812.",
		Status:      "pending",
		PayloadJSON: pendingPayload,
		UpdatedAt:   model.NowString(),
	}); err != nil {
		t.Fatal(err)
	}
	if err := legacy.PutCallbackRoute(ctx, model.CallbackRoute{
		Token:       "legacy-callback-route",
		Action:      "answer_choice",
		ThreadID:    "thread-legacy",
		TurnID:      "turn-legacy",
		RequestID:   "legacy-pending-input",
		Status:      model.CallbackStatusActive,
		PayloadJSON: callbackPayload,
		CreatedAt:   model.NowString(),
	}); err != nil {
		t.Fatal(err)
	}
	if err := legacy.Close(); err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(readRawSQLiteFile(t, path), []byte("Legacy pending secret marker")) || !bytes.Contains(readRawSQLiteFile(t, path), []byte("Legacy callback secret marker")) {
		t.Fatal("test setup did not persist legacy plaintext markers")
	}

	protected, err := OpenWithProtector(path, securestore.NewDeterministicTestProtector())
	if err != nil {
		t.Fatal(err)
	}
	defer protected.Close()
	raw := readRawSQLiteFile(t, path)
	if bytes.Contains(raw, []byte("Legacy pending secret marker")) || bytes.Contains(raw, []byte("Legacy callback secret marker")) {
		t.Fatal("legacy pending/callback plaintext marker remained in sqlite files")
	}
	pending, err := protected.GetPendingApproval(ctx, "legacy-pending-input")
	if err != nil || pending == nil || pending.Question != "Legacy pending secret marker 4812." || pending.PayloadJSON != pendingPayload {
		t.Fatalf("migrated pending=%#v err=%v", pending, err)
	}
	callback, err := protected.GetCallbackRoute(ctx, "legacy-callback-route")
	if err != nil || callback == nil || callback.PayloadJSON != callbackPayload {
		t.Fatalf("migrated callback=%#v err=%v", callback, err)
	}
}

func TestListThreadsFiltersInternalAppServerThreads(t *testing.T) {
	t.Parallel()

	store := openTestStore(t)
	ctx := context.Background()
	threads := []model.Thread{
		{
			ID:            "visible-thread",
			Title:         "Visible work",
			ProjectName:   "codex-tg",
			DirectoryName: "codex-tg",
			UpdatedAt:     10,
			LastPreview:   "normal user request",
			Raw:           json.RawMessage(`{"id":"visible-thread","preview":"normal user request"}`),
		},
		{
			ID:            "ephemeral-thread",
			Title:         "01900000-0000-7000-8000-000000000014",
			ProjectName:   "memories",
			DirectoryName: "memories",
			UpdatedAt:     30,
			Raw:           json.RawMessage(`{"thread":{"id":"ephemeral-thread","ephemeral":true,"source":{"subAgent":"memory_consolidation"}}}`),
		},
		{
			ID:            "sub-agent-thread",
			Title:         "01900000-0000-7000-8000-000000000015",
			ProjectName:   "memories",
			DirectoryName: "memories",
			UpdatedAt:     20,
			Raw:           json.RawMessage(`{"id":"sub-agent-thread","source":{"subAgent":"memory_consolidation"}}`),
		},
	}
	for _, thread := range threads {
		if err := store.UpsertThread(ctx, thread); err != nil {
			t.Fatalf("UpsertThread(%s) failed: %v", thread.ID, err)
		}
	}

	listed, err := store.ListThreads(ctx, 10, "")
	if err != nil {
		t.Fatalf("ListThreads failed: %v", err)
	}
	if len(listed) != 1 || listed[0].ID != "visible-thread" {
		t.Fatalf("listed threads = %#v, want only visible-thread", listed)
	}

	searched, err := store.ListThreads(ctx, 10, "memories")
	if err != nil {
		t.Fatalf("ListThreads(search) failed: %v", err)
	}
	if len(searched) != 0 {
		t.Fatalf("searched internal threads = %#v, want none", searched)
	}

	grouped, err := store.ListProjectGroups(ctx)
	if err != nil {
		t.Fatalf("ListProjectGroups failed: %v", err)
	}
	if _, ok := grouped["memories"]; ok {
		t.Fatalf("project groups include internal memories project: %#v", grouped)
	}
}

func TestDeliveryQueueClaimRetryAndComplete(t *testing.T) {
	t.Parallel()

	store := openTestStore(t)
	ctx := context.Background()
	item := model.DeliveryQueueItem{
		EventID:     "event-1",
		ChatKey:     model.ChatKey(123456789, 0),
		ChatID:      123456789,
		TopicID:     0,
		ThreadID:    "thread-1",
		Kind:        "observer",
		Status:      model.DeliveryStatusPending,
		AvailableAt: model.NowString(),
		PayloadJSON: `{"text":"hello"}`,
		CreatedAt:   model.NowString(),
		UpdatedAt:   model.NowString(),
	}
	if err := store.EnqueueDelivery(ctx, item); err != nil {
		t.Fatalf("EnqueueDelivery failed: %v", err)
	}

	batch, err := store.ClaimDeliveryBatch(ctx, 10)
	if err != nil {
		t.Fatalf("ClaimDeliveryBatch failed: %v", err)
	}
	if len(batch) != 1 {
		t.Fatalf("ClaimDeliveryBatch len = %d, want 1", len(batch))
	}
	if batch[0].Status != model.DeliveryStatusPending {
		t.Fatalf("Claimed status = %q, want %q", batch[0].Status, model.DeliveryStatusPending)
	}

	retryAt := time.Now().UTC().Add(5 * time.Second)
	if err := store.FailDelivery(ctx, batch[0].ID, 1, retryAt, "temporary failure", false); err != nil {
		t.Fatalf("FailDelivery failed: %v", err)
	}
	if err := store.RecordDeliveryAttempt(ctx, batch[0].ID, 1, "send_error", "temporary failure"); err != nil {
		t.Fatalf("RecordDeliveryAttempt failed: %v", err)
	}

	backlog, err := store.DeliveryQueueBacklog(ctx)
	if err != nil {
		t.Fatalf("DeliveryQueueBacklog failed: %v", err)
	}
	if backlog != 1 {
		t.Fatalf("DeliveryQueueBacklog = %d, want 1", backlog)
	}
}

func TestDeliveryPayloadIsNotStoredAsPlaintext(t *testing.T) {
	store := openTestStore(t, securestore.NewDeterministicTestProtector())
	secret := "private final answer marker"
	if err := store.EnqueueDelivery(context.Background(), model.DeliveryQueueItem{
		EventID:     "e1",
		ChatKey:     "1:0",
		Status:      model.DeliveryStatusPending,
		PayloadJSON: secret,
	}); err != nil {
		t.Fatal(err)
	}
	raw := readRawSQLiteFile(t, store.Path())
	if bytes.Contains(raw, []byte(secret)) {
		t.Fatal("plaintext leaked to sqlite")
	}
}

func TestTerminalChunksAreAtomicOrderedAndContentFree(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t, securestore.NewDeterministicTestProtector())
	items := []model.DeliveryQueueItem{
		{EventID: "terminal:1", ChatKey: "7:0", ChatID: 7, ThreadID: "thr", Kind: "terminal", Status: model.DeliveryStatusPending, GroupID: "thr:turn:completed", SequenceNo: 1, SequenceCount: 2, PayloadJSON: `{"text":"first secret"}`},
		{EventID: "terminal:2", ChatKey: "7:0", ChatID: 7, ThreadID: "thr", Kind: "terminal", Status: model.DeliveryStatusPending, GroupID: "thr:turn:completed", SequenceNo: 2, SequenceCount: 2, PayloadJSON: `{"text":"second secret"}`},
	}
	inserted, err := store.EnqueueTerminalEvent(ctx, model.TerminalEvent{TerminalKey: "thr:turn:completed", ThreadID: "thr", TurnID: "turn", Status: "completed"}, items)
	if err != nil || !inserted {
		t.Fatalf("enqueue terminal = %t, %v", inserted, err)
	}
	batch, err := store.ClaimDeliveryBatch(ctx, 10)
	if err != nil || len(batch) != 1 || batch[0].SequenceNo != 1 {
		t.Fatalf("first claim = %#v, %v", batch, err)
	}
	if err := store.CompleteDelivery(ctx, batch[0].ID); err != nil {
		t.Fatal(err)
	}
	batch, err = store.ClaimDeliveryBatch(ctx, 10)
	if err != nil || len(batch) != 1 || batch[0].SequenceNo != 2 {
		t.Fatalf("second claim = %#v, %v", batch, err)
	}
	if err := store.CompleteDelivery(ctx, batch[0].ID); err != nil {
		t.Fatal(err)
	}
	var deliveryStatus string
	var textColumns int
	if err := store.db.QueryRowContext(ctx, `SELECT delivery_status FROM terminal_events WHERE terminal_key=?`, "thr:turn:completed").Scan(&deliveryStatus); err != nil {
		t.Fatal(err)
	}
	if err := store.db.QueryRowContext(ctx, `SELECT count(*) FROM pragma_table_info('terminal_events') WHERE lower(name) LIKE '%text%' OR lower(name) LIKE '%payload%'`).Scan(&textColumns); err != nil {
		t.Fatal(err)
	}
	if deliveryStatus != model.DeliveryStatusDelivered || textColumns != 0 {
		t.Fatalf("terminal status=%q text columns=%d", deliveryStatus, textColumns)
	}
}

func TestTerminalDeliveryWithoutProtectorFailsClosed(t *testing.T) {
	store := openTestStore(t)
	inserted, err := store.EnqueueTerminalEvent(context.Background(), model.TerminalEvent{TerminalKey: "thr:turn:completed", ThreadID: "thr", TurnID: "turn", Status: "completed"}, []model.DeliveryQueueItem{{PayloadJSON: `{"text":"secret"}`}})
	if err == nil || inserted {
		t.Fatalf("enqueue terminal = %t, %v, want fail closed", inserted, err)
	}
	var count int
	if scanErr := store.db.QueryRow(`SELECT count(*) FROM terminal_events`).Scan(&count); scanErr != nil || count != 0 {
		t.Fatalf("terminal rows=%d err=%v", count, scanErr)
	}
}

func TestTerminalGroupFirstChunkDeadClearsWholeGroup(t *testing.T) {
	store := openTestStore(t, securestore.NewDeterministicTestProtector())
	ctx := context.Background()
	enqueueThreeTerminalChunks(t, store, "dead-first")
	batch, err := store.ClaimDeliveryBatch(ctx, 10)
	if err != nil || len(batch) != 1 {
		t.Fatalf("claim=%#v err=%v", batch, err)
	}
	if err := store.FailDelivery(ctx, batch[0].ID, 5, time.Now(), "exhausted", true); err != nil {
		t.Fatal(err)
	}
	assertDeadTerminalGroup(t, store, "dead-first", 0)
}

func TestTerminalGroupMiddleChunkDeadClearsRemainingGroup(t *testing.T) {
	store := openTestStore(t, securestore.NewDeterministicTestProtector())
	ctx := context.Background()
	enqueueThreeTerminalChunks(t, store, "dead-middle")
	first, err := store.ClaimDeliveryBatch(ctx, 10)
	if err != nil || len(first) != 1 {
		t.Fatalf("first=%#v err=%v", first, err)
	}
	if err := store.CompleteDelivery(ctx, first[0].ID); err != nil {
		t.Fatal(err)
	}
	middle, err := store.ClaimDeliveryBatch(ctx, 10)
	if err != nil || len(middle) != 1 || middle[0].SequenceNo != 2 {
		t.Fatalf("middle=%#v err=%v", middle, err)
	}
	if err := store.FailDelivery(ctx, middle[0].ID, 5, time.Now(), "exhausted", true); err != nil {
		t.Fatal(err)
	}
	assertDeadTerminalGroup(t, store, "dead-middle", 1)
}

func TestTerminalGroupProcessingRecoveryRetriesFirstBeforeLaterChunks(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "state.sqlite")
	protector := securestore.NewDeterministicTestProtector()
	store, err := OpenWithProtector(path, protector)
	if err != nil {
		t.Fatal(err)
	}
	enqueueThreeTerminalChunks(t, store, "recover-processing")
	first, err := store.ClaimDeliveryBatch(ctx, 10)
	if err != nil || len(first) != 1 || first[0].SequenceNo != 1 {
		t.Fatalf("first=%#v err=%v", first, err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = OpenWithProtector(path, protector)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	recovered, err := store.ClaimDeliveryBatch(ctx, 10)
	if err != nil || len(recovered) != 1 || recovered[0].ID != first[0].ID || recovered[0].SequenceNo != 1 {
		t.Fatalf("recovered=%#v err=%v", recovered, err)
	}
	var laterStatus string
	if err := store.db.QueryRow(`SELECT status FROM delivery_queue WHERE group_id=? AND sequence_no=2`, "recover-processing").Scan(&laterStatus); err != nil {
		t.Fatal(err)
	}
	if laterStatus != model.DeliveryStatusPending {
		t.Fatalf("later status=%q", laterStatus)
	}
}

func TestTerminalEventAndChunksRollbackOnPartialInsertFailure(t *testing.T) {
	store := openTestStore(t, securestore.NewDeterministicTestProtector())
	ctx := context.Background()
	items := []model.DeliveryQueueItem{
		{EventID: "duplicate", ChatKey: "7:0", ChatID: 7, ThreadID: "thr", Kind: "terminal", GroupID: "rollback", SequenceNo: 1, SequenceCount: 2, PayloadJSON: `{"text":"one"}`},
		{EventID: "duplicate", ChatKey: "7:0", ChatID: 7, ThreadID: "thr", Kind: "terminal", GroupID: "rollback", SequenceNo: 2, SequenceCount: 2, PayloadJSON: `{"text":"two"}`},
	}
	inserted, err := store.EnqueueTerminalEvent(ctx, model.TerminalEvent{TerminalKey: "rollback", ThreadID: "thr", TurnID: "turn", Status: "completed"}, items)
	if err == nil || inserted {
		t.Fatalf("inserted=%t err=%v", inserted, err)
	}
	var events, chunks int
	if err := store.db.QueryRow(`SELECT count(*) FROM terminal_events WHERE terminal_key='rollback'`).Scan(&events); err != nil {
		t.Fatal(err)
	}
	if err := store.db.QueryRow(`SELECT count(*) FROM delivery_queue WHERE group_id='rollback'`).Scan(&chunks); err != nil {
		t.Fatal(err)
	}
	if events != 0 || chunks != 0 {
		t.Fatalf("events=%d chunks=%d, want rollback", events, chunks)
	}
}

func TestTerminalEventWithLinkedReleaseCommitsOutboxAndReleasePendingTogether(t *testing.T) {
	store := newProtectedMinimalStore(t)
	ctx := context.Background()
	activateRunningMinimalLinkedThread(t, store, "linked-terminal", 9)
	if changed, err := store.MarkMinimalLinkedTurnStarted(ctx, "linked-terminal", 9, "turn-terminal"); err != nil || !changed {
		t.Fatalf("turn start changed=%t err=%v, want true nil", changed, err)
	}

	inserted, err := store.EnqueueTerminalEventWithLinkedRelease(ctx,
		model.TerminalEvent{TerminalKey: "linked-terminal:turn-terminal:completed", ThreadID: "linked-terminal", TurnID: "turn-terminal", Status: "completed"},
		[]model.DeliveryQueueItem{{
			EventID: "linked-terminal:turn-terminal:completed:1", ChatKey: "7:0", ChatID: 7,
			ThreadID: "linked-terminal", Kind: "terminal", GroupID: "linked-terminal:turn-terminal:completed",
			SequenceNo: 1, SequenceCount: 1, PayloadJSON: `{"text":"done"}`,
		}},
		&model.MinimalLinkedRelease{LinkedThreadID: "linked-terminal", TurnID: "turn-terminal", WorkerGeneration: 9},
	)
	if err != nil || !inserted {
		t.Fatalf("enqueue linked release inserted=%t err=%v, want true nil", inserted, err)
	}
	link, err := store.GetMinimalLinkedThreadByLinkedID(ctx, "linked-terminal")
	if err != nil {
		t.Fatal(err)
	}
	if link.State != model.MinimalLinkedReleasePending || link.ActiveTurnID != "turn-terminal" || link.WorkerGeneration != 9 {
		t.Fatalf("link after terminal release enqueue=%#v, want release_pending turn-terminal gen 9", link)
	}
	var terminalRows, deliveryRows int
	if err := store.db.QueryRowContext(ctx, `SELECT count(*) FROM terminal_events WHERE terminal_key=?`, "linked-terminal:turn-terminal:completed").Scan(&terminalRows); err != nil {
		t.Fatal(err)
	}
	if err := store.db.QueryRowContext(ctx, `SELECT count(*) FROM delivery_queue WHERE group_id=? AND status=?`, "linked-terminal:turn-terminal:completed", model.DeliveryStatusPending).Scan(&deliveryRows); err != nil {
		t.Fatal(err)
	}
	if terminalRows != 1 || deliveryRows != 1 {
		t.Fatalf("terminal rows=%d delivery rows=%d, want 1/1", terminalRows, deliveryRows)
	}
}

func TestTerminalEventWithLinkedReleaseRejectsStaleGenerationWithoutProjection(t *testing.T) {
	store := newProtectedMinimalStore(t)
	ctx := context.Background()
	activateRunningMinimalLinkedThread(t, store, "linked-stale", 9)
	if changed, err := store.MarkMinimalLinkedTurnStarted(ctx, "linked-stale", 9, "turn-terminal"); err != nil || !changed {
		t.Fatalf("turn start changed=%t err=%v, want true nil", changed, err)
	}

	inserted, err := store.EnqueueTerminalEventWithLinkedRelease(ctx,
		model.TerminalEvent{TerminalKey: "linked-stale:turn-terminal:completed", ThreadID: "linked-stale", TurnID: "turn-terminal", Status: "completed"},
		[]model.DeliveryQueueItem{{
			EventID: "linked-stale:turn-terminal:completed:1", ChatKey: "7:0", ChatID: 7,
			ThreadID: "linked-stale", Kind: "terminal", GroupID: "linked-stale:turn-terminal:completed",
			SequenceNo: 1, SequenceCount: 1, PayloadJSON: `{"text":"done"}`,
		}},
		&model.MinimalLinkedRelease{LinkedThreadID: "linked-stale", TurnID: "turn-terminal", WorkerGeneration: 10},
	)
	if err != nil || inserted {
		t.Fatalf("stale linked release inserted=%t err=%v, want false nil", inserted, err)
	}
	link, err := store.GetMinimalLinkedThreadByLinkedID(ctx, "linked-stale")
	if err != nil {
		t.Fatal(err)
	}
	if link.State != model.MinimalLinkedTelegramRunning || link.ActiveTurnID != "turn-terminal" || link.WorkerGeneration != 9 {
		t.Fatalf("link after stale enqueue=%#v, want unchanged running turn-terminal gen 9", link)
	}
	assertNoTerminalProjection(t, store, "linked-stale:turn-terminal:completed")
}

func TestTerminalEventWithLinkedReleaseRollsBackReleaseOnDeliveryFailure(t *testing.T) {
	store := newProtectedMinimalStore(t)
	ctx := context.Background()
	activateRunningMinimalLinkedThread(t, store, "linked-rollback", 11)
	if changed, err := store.MarkMinimalLinkedTurnStarted(ctx, "linked-rollback", 11, "turn-terminal"); err != nil || !changed {
		t.Fatalf("turn start changed=%t err=%v, want true nil", changed, err)
	}
	items := []model.DeliveryQueueItem{
		{EventID: "duplicate-terminal-chunk", ChatKey: "7:0", ChatID: 7, ThreadID: "linked-rollback", Kind: "terminal", GroupID: "linked-rollback:turn-terminal:completed", SequenceNo: 1, SequenceCount: 2, PayloadJSON: `{"text":"one"}`},
		{EventID: "duplicate-terminal-chunk", ChatKey: "7:0", ChatID: 7, ThreadID: "linked-rollback", Kind: "terminal", GroupID: "linked-rollback:turn-terminal:completed", SequenceNo: 2, SequenceCount: 2, PayloadJSON: `{"text":"two"}`},
	}

	inserted, err := store.EnqueueTerminalEventWithLinkedRelease(ctx,
		model.TerminalEvent{TerminalKey: "linked-rollback:turn-terminal:completed", ThreadID: "linked-rollback", TurnID: "turn-terminal", Status: "completed"},
		items,
		&model.MinimalLinkedRelease{LinkedThreadID: "linked-rollback", TurnID: "turn-terminal", WorkerGeneration: 11},
	)
	if err == nil || inserted {
		t.Fatalf("rollback enqueue inserted=%t err=%v, want insert failure", inserted, err)
	}
	link, err := store.GetMinimalLinkedThreadByLinkedID(ctx, "linked-rollback")
	if err != nil {
		t.Fatal(err)
	}
	if link.State != model.MinimalLinkedTelegramRunning || link.ActiveTurnID != "turn-terminal" || link.WorkerGeneration != 11 {
		t.Fatalf("link after failed enqueue=%#v, want unchanged running turn-terminal gen 11", link)
	}
	assertNoTerminalProjection(t, store, "linked-rollback:turn-terminal:completed")
}

func TestTerminalEventWithLinkedReleaseDuplicateDoesNotRelease(t *testing.T) {
	store := newProtectedMinimalStore(t)
	ctx := context.Background()
	activateRunningMinimalLinkedThread(t, store, "linked-duplicate", 12)
	if changed, err := store.MarkMinimalLinkedTurnStarted(ctx, "linked-duplicate", 12, "turn-terminal"); err != nil || !changed {
		t.Fatalf("turn start changed=%t err=%v, want true nil", changed, err)
	}
	event := model.TerminalEvent{TerminalKey: "linked-duplicate:turn-terminal:completed", ThreadID: "linked-duplicate", TurnID: "turn-terminal", Status: "completed"}
	items := []model.DeliveryQueueItem{{
		EventID: "linked-duplicate:turn-terminal:completed:1", ChatKey: "7:0", ChatID: 7,
		ThreadID: "linked-duplicate", Kind: "terminal", GroupID: "linked-duplicate:turn-terminal:completed",
		SequenceNo: 1, SequenceCount: 1, PayloadJSON: `{"text":"done"}`,
	}}
	if inserted, err := store.EnqueueTerminalEvent(ctx, event, items); err != nil || !inserted {
		t.Fatalf("initial enqueue inserted=%t err=%v, want true nil", inserted, err)
	}

	inserted, err := store.EnqueueTerminalEventWithLinkedRelease(ctx, event, items, &model.MinimalLinkedRelease{
		LinkedThreadID: "linked-duplicate", TurnID: "turn-terminal", WorkerGeneration: 12,
	})
	if err != nil || inserted {
		t.Fatalf("duplicate linked release inserted=%t err=%v, want false nil", inserted, err)
	}
	link, err := store.GetMinimalLinkedThreadByLinkedID(ctx, "linked-duplicate")
	if err != nil {
		t.Fatal(err)
	}
	if link.State != model.MinimalLinkedTelegramRunning || link.ActiveTurnID != "turn-terminal" || link.WorkerGeneration != 12 {
		t.Fatalf("link after duplicate terminal=%#v, want unchanged running turn-terminal gen 12", link)
	}
	var deliveryRows int
	if err := store.db.QueryRowContext(ctx, `SELECT count(*) FROM delivery_queue WHERE group_id=?`, event.TerminalKey).Scan(&deliveryRows); err != nil {
		t.Fatal(err)
	}
	if deliveryRows != 1 {
		t.Fatalf("duplicate terminal delivery rows=%d, want 1", deliveryRows)
	}
}

func TestFinishMinimalLinkedReleaseWithReadyDeliveryCommitsExpiryAndReadyTogether(t *testing.T) {
	store := newProtectedMinimalStore(t)
	ctx := context.Background()
	activateRunningMinimalLinkedThread(t, store, "linked-ready-atomic", 13)
	if changed, err := store.MarkMinimalLinkedTurnStarted(ctx, "linked-ready-atomic", 13, "turn-ready"); err != nil || !changed {
		t.Fatalf("turn start changed=%t err=%v, want true nil", changed, err)
	}
	if changed, err := store.BeginMinimalLinkedRelease(ctx, model.MinimalLinkedRelease{LinkedThreadID: "linked-ready-atomic", TurnID: "turn-ready", WorkerGeneration: 13}); err != nil || !changed {
		t.Fatalf("begin release changed=%t err=%v, want true nil", changed, err)
	}
	seedLogicalApproval(t, store, "req-ready-expire", "linked-ready-atomic", "turn-ready", "item/commandExecution/requestApproval", "minimal-link-worker:13", "11111111111111111111111111111111", "22222222222222222222222222222222", "approval-ready-expire")

	changed, err := store.FinishMinimalLinkedReleaseWithReadyDelivery(ctx, "linked-ready-atomic", 13, "minimal-link-worker:13", "turn-ready", time.Date(2026, 8, 23, 6, 0, 0, 0, time.UTC), readyDeliveryItem("linked-ready-atomic", "turn-ready"))
	if err != nil || !changed {
		t.Fatalf("finish release changed=%t err=%v, want true nil", changed, err)
	}
	link, err := store.GetMinimalLinkedThreadByLinkedID(ctx, "linked-ready-atomic")
	if err != nil {
		t.Fatal(err)
	}
	if link.State != model.MinimalLinkedReady || link.ActiveTurnID != "" || link.WorkerGeneration != 0 {
		t.Fatalf("link after atomic ready=%#v, want ready cleared turn/generation", link)
	}
	approval, err := store.GetMinimalApproval(ctx, "req-ready-expire")
	if err != nil || approval == nil || approval.Status != "expired" {
		t.Fatalf("approval after atomic ready=%#v err=%v, want expired", approval, err)
	}
	assertMinimalApprovalRouteStatus(t, store, "11111111111111111111111111111111", "expired")
	assertMinimalApprovalRouteStatus(t, store, "22222222222222222222222222222222", "expired")
	assertDeliveryKindStatus(t, store, "minimal_approval", model.DeliveryStatusDead, 1)
	assertDeliveryKindStatus(t, store, "handoff_ready", model.DeliveryStatusPending, 1)

	changed, err = store.FinishMinimalLinkedReleaseWithReadyDelivery(ctx, "linked-ready-atomic", 13, "minimal-link-worker:13", "turn-ready", time.Now(), readyDeliveryItem("linked-ready-atomic", "turn-ready"))
	if err != nil || changed {
		t.Fatalf("repeat finish changed=%t err=%v, want false nil", changed, err)
	}
	assertDeliveryKindStatus(t, store, "handoff_ready", model.DeliveryStatusPending, 1)
}

func TestFinishMinimalLinkedReleaseWithReadyDeliveryExpiresOnlyExactWorkerSession(t *testing.T) {
	store := newProtectedMinimalStore(t)
	ctx := context.Background()
	activateRunningMinimalLinkedThread(t, store, "linked-ready-session", 17)
	if changed, err := store.MarkMinimalLinkedTurnStarted(ctx, "linked-ready-session", 17, "turn-ready"); err != nil || !changed {
		t.Fatalf("turn start changed=%t err=%v, want true nil", changed, err)
	}
	if changed, err := store.BeginMinimalLinkedRelease(ctx, model.MinimalLinkedRelease{LinkedThreadID: "linked-ready-session", TurnID: "turn-ready", WorkerGeneration: 17}); err != nil || !changed {
		t.Fatalf("begin release changed=%t err=%v, want true nil", changed, err)
	}
	seedLogicalApproval(t, store, "req-worker-expire", "linked-ready-session", "turn-ready", "item/commandExecution/requestApproval", "minimal-link-worker:17", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", "approval-worker-expire")
	seedLogicalApproval(t, store, "req-live-survives", "linked-ready-session", "turn-ready", "item/commandExecution/requestApproval", "live-session:1", "cccccccccccccccccccccccccccccccc", "dddddddddddddddddddddddddddddddd", "approval-live-survives")
	if err := store.PutCallbackRoute(ctx, model.CallbackRoute{Token: "worker-callback", Action: "answer_choice", ThreadID: "linked-ready-session", TurnID: "turn-ready", RequestID: "req-worker-input", SessionIdentity: "minimal-link-worker:17", Status: model.CallbackStatusActive, PayloadJSON: `{"text":"Yes"}`, CreatedAt: model.NowString()}); err != nil {
		t.Fatal(err)
	}
	if err := store.PutCallbackRoute(ctx, model.CallbackRoute{Token: "live-callback", Action: "answer_choice", ThreadID: "linked-ready-session", TurnID: "turn-ready", RequestID: "req-live-input", SessionIdentity: "live-session:1", Status: model.CallbackStatusActive, PayloadJSON: `{"text":"No"}`, CreatedAt: model.NowString()}); err != nil {
		t.Fatal(err)
	}

	changed, err := store.FinishMinimalLinkedReleaseWithReadyDelivery(ctx, "linked-ready-session", 17, "minimal-link-worker:17", "turn-ready", time.Now(), readyDeliveryItem("linked-ready-session", "turn-ready"))
	if err != nil || !changed {
		t.Fatalf("finish release changed=%t err=%v, want true nil", changed, err)
	}

	workerApproval, _ := store.GetMinimalApproval(ctx, "req-worker-expire")
	liveApproval, _ := store.GetMinimalApproval(ctx, "req-live-survives")
	if workerApproval == nil || workerApproval.Status != "expired" {
		t.Fatalf("worker approval=%#v, want expired", workerApproval)
	}
	if liveApproval == nil || liveApproval.Status != "pending" {
		t.Fatalf("live approval=%#v, want pending", liveApproval)
	}
	assertMinimalApprovalRouteStatus(t, store, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "expired")
	assertMinimalApprovalRouteStatus(t, store, "cccccccccccccccccccccccccccccccc", "active")
	workerCallback, err := store.GetCallbackRoute(ctx, "worker-callback")
	if err != nil || workerCallback == nil || workerCallback.Status != model.CallbackStatusExpired {
		t.Fatalf("worker callback=%#v err=%v, want expired", workerCallback, err)
	}
	liveCallback, err := store.GetCallbackRoute(ctx, "live-callback")
	if err != nil || liveCallback == nil || liveCallback.Status != model.CallbackStatusActive {
		t.Fatalf("live callback=%#v err=%v, want active", liveCallback, err)
	}
}

func TestFinishMinimalLinkedReleaseWithReadyDeliveryExpiresClaimedAndStructuredForExactSession(t *testing.T) {
	store := newProtectedMinimalStore(t)
	ctx := context.Background()
	identity := "minimal-link-worker:release:21"
	approvalID := model.ScopedRequestID(identity, "approval-wire")
	inputID := model.ScopedRequestID(identity, "input-wire")
	activateRunningMinimalLinkedThread(t, store, "linked-ready-claimed", 21)
	if changed, err := store.MarkMinimalLinkedTurnStarted(ctx, "linked-ready-claimed", 21, "turn-ready"); err != nil || !changed {
		t.Fatalf("turn start changed=%t err=%v, want true nil", changed, err)
	}
	if changed, err := store.BeginMinimalLinkedRelease(ctx, model.MinimalLinkedRelease{LinkedThreadID: "linked-ready-claimed", TurnID: "turn-ready", WorkerGeneration: 21}); err != nil || !changed {
		t.Fatalf("begin release changed=%t err=%v, want true nil", changed, err)
	}
	created, err := store.CreateMinimalApproval(ctx, MinimalApprovalSeed{
		Approval:     MinimalApproval{RequestID: approvalID, WireRequestID: "approval-wire", ThreadID: "linked-ready-claimed", TurnID: "turn-ready", RequestKind: "item/commandExecution/requestApproval", ProjectName: "Bridge", SessionIdentity: identity, Status: "pending"},
		ApproveToken: "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee",
		DenyToken:    "ffffffffffffffffffffffffffffffff",
		Delivery:     model.DeliveryQueueItem{EventID: "approval-ready-claimed", ChatKey: model.ChatKey(7, 0), ChatID: 7, TopicID: 0, ThreadID: "linked-ready-claimed", Kind: "minimal_approval", Status: model.DeliveryStatusPending, AvailableAt: model.TimeString("2000-01-01T00:00:00Z"), PayloadJSON: MustJSON(model.DeliveryPayload{Text: "approval", ThreadID: "linked-ready-claimed", TurnID: "turn-ready", EventID: approvalID})},
	})
	if err != nil || !created {
		t.Fatalf("CreateMinimalApproval created=%t err=%v", created, err)
	}
	completeStorageMinimalApprovalDelivery(t, store, approvalID, 600)
	if route, claimed, err := store.ClaimMinimalApproval(ctx, "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee", 7, 0, 600, identity); err != nil || !claimed || route == nil {
		t.Fatalf("claim before release route=%#v claimed=%t err=%v", route, claimed, err)
	}
	if err := store.SavePendingApproval(ctx, model.PendingApproval{
		RequestID:       inputID,
		WireRequestID:   "input-wire",
		ThreadID:        "linked-ready-claimed",
		TurnID:          "turn-ready",
		ItemID:          "item-input",
		PromptKind:      "user_input",
		RequestKind:     "item/tool/requestUserInput",
		Question:        "Choose.",
		SessionIdentity: identity,
		Status:          "pending",
		PayloadJSON:     `{"questions":[{"id":"choice","question":"Choose."}]}`,
		UpdatedAt:       model.NowString(),
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.PutCallbackRoute(ctx, model.CallbackRoute{Token: "input-callback", Action: "answer_choice", ThreadID: "linked-ready-claimed", TurnID: "turn-ready", RequestID: inputID, SessionIdentity: identity, Status: model.CallbackStatusActive, PayloadJSON: `{"text":"Yes"}`, CreatedAt: model.NowString()}); err != nil {
		t.Fatal(err)
	}

	changed, err := store.FinishMinimalLinkedReleaseWithReadyDelivery(ctx, "linked-ready-claimed", 21, identity, "turn-ready", time.Now(), readyDeliveryItem("linked-ready-claimed", "turn-ready"))
	if err != nil || !changed {
		t.Fatalf("finish release changed=%t err=%v, want true nil", changed, err)
	}
	if restored, err := store.RestoreMinimalApprovalClaim(ctx, approvalID, "approve"); err != nil || restored {
		t.Fatalf("restore after release restored=%t err=%v, want false nil", restored, err)
	}
	approval, err := store.GetMinimalApproval(ctx, approvalID)
	if err != nil || approval == nil || approval.Status != "expired" {
		t.Fatalf("approval after release/restore=%#v err=%v, want expired", approval, err)
	}
	assertMinimalApprovalRouteStatus(t, store, "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee", "expired")
	assertMinimalApprovalRouteStatus(t, store, "ffffffffffffffffffffffffffffffff", "expired")
	pending, err := store.GetPendingApproval(ctx, inputID)
	if err != nil || pending == nil || pending.Status != "expired" {
		t.Fatalf("pending input after release=%#v err=%v, want expired", pending, err)
	}
	callback, err := store.GetCallbackRoute(ctx, "input-callback")
	if err != nil || callback == nil || callback.Status != model.CallbackStatusExpired {
		t.Fatalf("input callback after release=%#v err=%v, want expired", callback, err)
	}
}

func TestFinishMinimalLinkedReleaseWithReadyDeliveryRejectsStaleActiveTurn(t *testing.T) {
	store := newProtectedMinimalStore(t)
	ctx := context.Background()
	activateRunningMinimalLinkedThread(t, store, "linked-ready-stale", 14)
	if changed, err := store.MarkMinimalLinkedTurnStarted(ctx, "linked-ready-stale", 14, "turn-new"); err != nil || !changed {
		t.Fatalf("turn start changed=%t err=%v, want true nil", changed, err)
	}
	if changed, err := store.BeginMinimalLinkedRelease(ctx, model.MinimalLinkedRelease{LinkedThreadID: "linked-ready-stale", TurnID: "turn-new", WorkerGeneration: 14}); err != nil || !changed {
		t.Fatalf("begin release changed=%t err=%v, want true nil", changed, err)
	}

	changed, err := store.FinishMinimalLinkedReleaseWithReadyDelivery(ctx, "linked-ready-stale", 14, "minimal-link-worker:14", "turn-old", time.Now(), readyDeliveryItem("linked-ready-stale", "turn-old"))
	if err != nil || changed {
		t.Fatalf("stale finish changed=%t err=%v, want false nil", changed, err)
	}
	link, err := store.GetMinimalLinkedThreadByLinkedID(ctx, "linked-ready-stale")
	if err != nil {
		t.Fatal(err)
	}
	if link.State != model.MinimalLinkedReleasePending || link.ActiveTurnID != "turn-new" || link.WorkerGeneration != 14 {
		t.Fatalf("link after stale finish=%#v, want release_pending turn-new gen 14", link)
	}
	assertDeliveryKindStatus(t, store, "handoff_ready", model.DeliveryStatusPending, 0)
}

func TestFinishMinimalLinkedReleaseWithReadyDeliveryRollsBackOnReadyDeliveryFailure(t *testing.T) {
	store := newProtectedMinimalStore(t)
	ctx := context.Background()
	activateRunningMinimalLinkedThread(t, store, "linked-ready-rollback", 15)
	if changed, err := store.MarkMinimalLinkedTurnStarted(ctx, "linked-ready-rollback", 15, "turn-ready"); err != nil || !changed {
		t.Fatalf("turn start changed=%t err=%v, want true nil", changed, err)
	}
	if changed, err := store.BeginMinimalLinkedRelease(ctx, model.MinimalLinkedRelease{LinkedThreadID: "linked-ready-rollback", TurnID: "turn-ready", WorkerGeneration: 15}); err != nil || !changed {
		t.Fatalf("begin release changed=%t err=%v, want true nil", changed, err)
	}
	seedLogicalApproval(t, store, "req-ready-rollback", "linked-ready-rollback", "turn-ready", "item/commandExecution/requestApproval", "minimal-link-worker:15", "33333333333333333333333333333333", "44444444444444444444444444444444", "approval-ready-rollback")
	if _, err := store.db.Exec(`CREATE TRIGGER reject_handoff_ready BEFORE INSERT ON delivery_queue WHEN NEW.kind='handoff_ready' BEGIN SELECT RAISE(ABORT, 'forced handoff ready failure'); END`); err != nil {
		t.Fatal(err)
	}

	changed, err := store.FinishMinimalLinkedReleaseWithReadyDelivery(ctx, "linked-ready-rollback", 15, "minimal-link-worker:15", "turn-ready", time.Now(), readyDeliveryItem("linked-ready-rollback", "turn-ready"))
	if err == nil || changed {
		t.Fatalf("failed ready finish changed=%t err=%v, want DB error", changed, err)
	}
	link, err := store.GetMinimalLinkedThreadByLinkedID(ctx, "linked-ready-rollback")
	if err != nil {
		t.Fatal(err)
	}
	if link.State != model.MinimalLinkedReleasePending || link.ActiveTurnID != "turn-ready" || link.WorkerGeneration != 15 {
		t.Fatalf("link after failed ready finish=%#v, want release_pending turn-ready gen 15", link)
	}
	approval, err := store.GetMinimalApproval(ctx, "req-ready-rollback")
	if err != nil || approval == nil || approval.Status != "pending" {
		t.Fatalf("approval after failed ready finish=%#v err=%v, want pending", approval, err)
	}
	assertMinimalApprovalRouteStatus(t, store, "33333333333333333333333333333333", "active")
	assertMinimalApprovalRouteStatus(t, store, "44444444444444444444444444444444", "active")
	assertDeliveryKindStatus(t, store, "minimal_approval", model.DeliveryStatusPending, 1)
	assertDeliveryKindStatus(t, store, "handoff_ready", model.DeliveryStatusPending, 0)
}

func TestFinishMinimalLinkedReleaseWithReadyDeliveryProtectorFailureDoesNotMutateRelease(t *testing.T) {
	store := newProtectedMinimalStore(t)
	ctx := context.Background()
	activateRunningMinimalLinkedThread(t, store, "linked-ready-protect", 16)
	if changed, err := store.MarkMinimalLinkedTurnStarted(ctx, "linked-ready-protect", 16, "turn-ready"); err != nil || !changed {
		t.Fatalf("turn start changed=%t err=%v, want true nil", changed, err)
	}
	if changed, err := store.BeginMinimalLinkedRelease(ctx, model.MinimalLinkedRelease{LinkedThreadID: "linked-ready-protect", TurnID: "turn-ready", WorkerGeneration: 16}); err != nil || !changed {
		t.Fatalf("begin release changed=%t err=%v, want true nil", changed, err)
	}
	store.protector = failingProtector{err: errors.New("protect failed")}

	changed, err := store.FinishMinimalLinkedReleaseWithReadyDelivery(ctx, "linked-ready-protect", 16, "minimal-link-worker:16", "turn-ready", time.Now(), readyDeliveryItem("linked-ready-protect", "turn-ready"))
	if err == nil || changed {
		t.Fatalf("protector finish changed=%t err=%v, want protection error", changed, err)
	}
	link, err := store.GetMinimalLinkedThreadByLinkedID(ctx, "linked-ready-protect")
	if err != nil {
		t.Fatal(err)
	}
	if link.State != model.MinimalLinkedReleasePending || link.ActiveTurnID != "turn-ready" || link.WorkerGeneration != 16 {
		t.Fatalf("link after protector failure=%#v, want release_pending turn-ready gen 16", link)
	}
	assertDeliveryKindStatus(t, store, "handoff_ready", model.DeliveryStatusPending, 0)
}

func readyDeliveryItem(linkedID, turnID string) *model.DeliveryQueueItem {
	eventID := linkedID + ":" + turnID + ":handoff_ready"
	return &model.DeliveryQueueItem{
		EventID:     eventID,
		ChatKey:     model.ChatKey(7, 3),
		ChatID:      7,
		TopicID:     3,
		ThreadID:    linkedID,
		Kind:        "handoff_ready",
		Status:      model.DeliveryStatusPending,
		PayloadJSON: MustJSON(model.DeliveryPayload{Text: "ready", ThreadID: linkedID, TurnID: turnID, EventID: eventID}),
	}
}

func assertDeliveryKindStatus(t *testing.T, store *Store, kind, status string, want int) {
	t.Helper()
	var count int
	if err := store.db.QueryRow(`SELECT count(*) FROM delivery_queue WHERE kind=? AND status=?`, kind, status).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != want {
		t.Fatalf("delivery kind=%s status=%s count=%d, want %d", kind, status, count, want)
	}
}

type failingProtector struct {
	err error
}

func (p failingProtector) Protect(context.Context, []byte) (string, error) {
	return "", p.err
}

func (p failingProtector) Unprotect(context.Context, string) ([]byte, error) {
	return nil, p.err
}

func assertNoTerminalProjection(t *testing.T, store *Store, terminalKey string) {
	t.Helper()
	var events, chunks int
	if err := store.db.QueryRow(`SELECT count(*) FROM terminal_events WHERE terminal_key=?`, terminalKey).Scan(&events); err != nil {
		t.Fatal(err)
	}
	if err := store.db.QueryRow(`SELECT count(*) FROM delivery_queue WHERE group_id=?`, terminalKey).Scan(&chunks); err != nil {
		t.Fatal(err)
	}
	if events != 0 || chunks != 0 {
		t.Fatalf("terminal %q projected events=%d chunks=%d, want none", terminalKey, events, chunks)
	}
}

func enqueueThreeTerminalChunks(t *testing.T, store *Store, group string) {
	t.Helper()
	items := make([]model.DeliveryQueueItem, 3)
	for i := range items {
		items[i] = model.DeliveryQueueItem{EventID: fmt.Sprintf("%s:%d", group, i+1), ChatKey: "7:0", ChatID: 7, ThreadID: "thr", Kind: "terminal", GroupID: group, SequenceNo: i + 1, SequenceCount: 3, PayloadJSON: fmt.Sprintf(`{"text":"chunk-%d"}`, i+1)}
	}
	inserted, err := store.EnqueueTerminalEvent(context.Background(), model.TerminalEvent{TerminalKey: group, ThreadID: "thr", TurnID: "turn", Status: "completed"}, items)
	if err != nil || !inserted {
		t.Fatalf("enqueue=%t err=%v", inserted, err)
	}
}

func assertDeadTerminalGroup(t *testing.T, store *Store, group string, delivered int) {
	t.Helper()
	var terminalStatus string
	if err := store.db.QueryRow(`SELECT delivery_status FROM terminal_events WHERE terminal_key=?`, group).Scan(&terminalStatus); err != nil {
		t.Fatal(err)
	}
	var deadCount, deliveredCount, payloadCount int
	if err := store.db.QueryRow(`SELECT count(*) FROM delivery_queue WHERE group_id=? AND status='dead'`, group).Scan(&deadCount); err != nil {
		t.Fatal(err)
	}
	if err := store.db.QueryRow(`SELECT count(*) FROM delivery_queue WHERE group_id=? AND status='delivered'`, group).Scan(&deliveredCount); err != nil {
		t.Fatal(err)
	}
	if err := store.db.QueryRow(`SELECT count(*) FROM delivery_queue WHERE group_id=? AND payload_json IS NOT NULL`, group).Scan(&payloadCount); err != nil {
		t.Fatal(err)
	}
	if terminalStatus != model.DeliveryStatusDead || deadCount != 3-delivered || deliveredCount != delivered || payloadCount != 0 {
		t.Fatalf("terminal=%q dead=%d delivered=%d payloads=%d", terminalStatus, deadCount, deliveredCount, payloadCount)
	}
	batch, err := store.ClaimDeliveryBatch(context.Background(), 10)
	if err != nil || len(batch) != 0 {
		t.Fatalf("post-dead claim=%#v err=%v", batch, err)
	}
}

func TestProtectedDeliveryPayloadRoundTripsThroughClaim(t *testing.T) {
	store := openTestStore(t, securestore.NewDeterministicTestProtector())
	ctx := context.Background()
	if err := store.EnqueueDelivery(ctx, model.DeliveryQueueItem{
		EventID:     "e1",
		ChatKey:     "1:0",
		Status:      model.DeliveryStatusPending,
		PayloadJSON: `{"text":"private final answer"}`,
	}); err != nil {
		t.Fatal(err)
	}

	batch, err := store.ClaimDeliveryBatch(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(batch) != 1 || batch[0].PayloadJSON != `{"text":"private final answer"}` {
		t.Fatalf("claimed payload = %q, want original payload", batch[0].PayloadJSON)
	}
}

func TestCompleteDeliveryDiscardsPayload(t *testing.T) {
	store := openTestStore(t, securestore.NewDeterministicTestProtector())
	ctx := context.Background()
	if err := store.EnqueueDelivery(ctx, model.DeliveryQueueItem{
		EventID:     "e1",
		ChatKey:     "1:0",
		Status:      model.DeliveryStatusPending,
		PayloadJSON: `{"text":"private final answer"}`,
	}); err != nil {
		t.Fatal(err)
	}
	batch, err := store.ClaimDeliveryBatch(ctx, 1)
	if err != nil || len(batch) != 1 {
		t.Fatalf("ClaimDeliveryBatch = %#v, %v", batch, err)
	}
	if err := store.CompleteDelivery(ctx, batch[0].ID); err != nil {
		t.Fatal(err)
	}

	var payload any
	if err := store.db.QueryRowContext(ctx, `SELECT payload_json FROM delivery_queue WHERE id = ?`, batch[0].ID).Scan(&payload); err != nil {
		t.Fatal(err)
	}
	if payload != nil {
		t.Fatalf("delivered payload = %v, want NULL", payload)
	}
}

func TestLegacyCompleteDeliveryRetainsPayload(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	payload := `{"text":"legacy payload"}`
	if err := store.EnqueueDelivery(ctx, model.DeliveryQueueItem{
		EventID:     "e1",
		ChatKey:     "1:0",
		Status:      model.DeliveryStatusPending,
		PayloadJSON: payload,
	}); err != nil {
		t.Fatal(err)
	}
	batch, err := store.ClaimDeliveryBatch(ctx, 1)
	if err != nil || len(batch) != 1 {
		t.Fatalf("ClaimDeliveryBatch = %#v, %v", batch, err)
	}
	if err := store.CompleteDelivery(ctx, batch[0].ID); err != nil {
		t.Fatal(err)
	}

	var stored string
	if err := store.db.QueryRowContext(ctx, `SELECT payload_json FROM delivery_queue WHERE id = ?`, batch[0].ID).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored != payload {
		t.Fatalf("legacy delivered payload = %q, want original payload", stored)
	}
}

func TestDeadDeliveryDiscardsPayload(t *testing.T) {
	store := openTestStore(t, securestore.NewDeterministicTestProtector())
	ctx := context.Background()
	if err := store.EnqueueDelivery(ctx, model.DeliveryQueueItem{
		EventID:     "e1",
		ChatKey:     "1:0",
		Status:      model.DeliveryStatusPending,
		PayloadJSON: `{"text":"private final answer"}`,
	}); err != nil {
		t.Fatal(err)
	}
	batch, err := store.ClaimDeliveryBatch(ctx, 1)
	if err != nil || len(batch) != 1 {
		t.Fatalf("ClaimDeliveryBatch = %#v, %v", batch, err)
	}
	if err := store.FailDelivery(ctx, batch[0].ID, 5, time.Now(), "permanent failure", true); err != nil {
		t.Fatal(err)
	}

	var payload any
	if err := store.db.QueryRowContext(ctx, `SELECT payload_json FROM delivery_queue WHERE id = ?`, batch[0].ID).Scan(&payload); err != nil {
		t.Fatal(err)
	}
	if payload != nil {
		t.Fatalf("dead payload = %v, want NULL", payload)
	}
}

func TestOpenWithProtectorMigratesPlaintextWithoutLeavingRawMarker(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.sqlite")
	legacy, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	secret := "one-time migration secret marker"
	if err := legacy.EnqueueDelivery(context.Background(), model.DeliveryQueueItem{
		EventID:     "e1",
		ChatKey:     "1:0",
		Status:      model.DeliveryStatusPending,
		PayloadJSON: secret,
	}); err != nil {
		t.Fatal(err)
	}
	if err := legacy.Close(); err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(readRawSQLiteFile(t, path), []byte(secret)) {
		t.Fatal("test setup did not persist plaintext marker")
	}

	protected, err := OpenWithProtector(path, securestore.NewDeterministicTestProtector())
	if err != nil {
		t.Fatal(err)
	}
	defer protected.Close()
	if bytes.Contains(readRawSQLiteFile(t, path), []byte(secret)) {
		t.Fatal("plaintext migration marker remained in sqlite files")
	}
}

func TestOpenWithProtectorRecoversPlaintextProcessingPayload(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.sqlite")
	legacy, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	secret := "interrupted processing secret marker"
	if err := legacy.EnqueueDelivery(context.Background(), model.DeliveryQueueItem{
		EventID:     "e1",
		ChatKey:     "1:0",
		Status:      model.DeliveryStatusProcessing,
		PayloadJSON: secret,
	}); err != nil {
		t.Fatal(err)
	}
	if err := legacy.Close(); err != nil {
		t.Fatal(err)
	}

	protected, err := OpenWithProtector(path, securestore.NewDeterministicTestProtector())
	if err != nil {
		t.Fatal(err)
	}
	defer protected.Close()
	if bytes.Contains(readRawSQLiteFile(t, path), []byte(secret)) {
		t.Fatal("plaintext processing marker remained in sqlite files")
	}
	batch, err := protected.ClaimDeliveryBatch(context.Background(), 1)
	if err != nil || len(batch) != 1 {
		t.Fatalf("ClaimDeliveryBatch = %#v, %v, want recovered delivery", batch, err)
	}
	if batch[0].PayloadJSON != secret {
		t.Fatal("recovered processing payload did not round trip")
	}
}

func TestOpenWithProtectorSkipsCheckpointWhenPayloadMigrationNotNeeded(t *testing.T) {
	root, err := os.MkdirTemp("", "open-protected-current-*")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.RemoveAll(root) }()
	path := filepath.Join(root, "state.sqlite")
	store, err := OpenWithProtector(path, securestore.NewDeterministicTestProtector())
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	checkpointDB := openCheckpointTestDB(t, path, 1)
	defer checkpointDB.Close()
	readerDB := openCheckpointTestDB(t, path, 1)
	defer readerDB.Close()
	seedCheckpointBusyDB(t, checkpointDB)
	_, releaseReader := holdCheckpointReader(t, readerDB)
	defer releaseReader()
	if _, err := checkpointDB.ExecContext(context.Background(), `INSERT INTO checkpoint_busy(v) VALUES (2)`); err != nil {
		t.Fatal(err)
	}

	reopened, err := OpenWithProtector(path, securestore.NewDeterministicTestProtector())
	if err != nil {
		t.Fatalf("OpenWithProtector failed on current DB without payload migration: %v", err)
	}
	if err := reopened.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestOpenWithProtectorKeepsCheckpointForPlaintextMigration(t *testing.T) {
	root, err := os.MkdirTemp("", "open-protected-migration-*")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.RemoveAll(root) }()
	path := filepath.Join(root, "state.sqlite")
	legacy, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := legacy.EnqueueDelivery(context.Background(), model.DeliveryQueueItem{
		EventID:     "e1",
		ChatKey:     "1:0",
		Status:      model.DeliveryStatusPending,
		PayloadJSON: "plaintext migration still needs checkpoint",
	}); err != nil {
		t.Fatal(err)
	}
	if err := legacy.Close(); err != nil {
		t.Fatal(err)
	}

	checkpointDB := openCheckpointTestDB(t, path, 1)
	defer checkpointDB.Close()
	readerDB := openCheckpointTestDB(t, path, 1)
	defer readerDB.Close()
	seedCheckpointBusyDB(t, checkpointDB)
	_, releaseReader := holdCheckpointReader(t, readerDB)
	defer releaseReader()
	if _, err := checkpointDB.ExecContext(context.Background(), `INSERT INTO checkpoint_busy(v) VALUES (2)`); err != nil {
		t.Fatal(err)
	}

	reopened, err := OpenWithProtector(path, securestore.NewDeterministicTestProtector())
	if reopened != nil {
		_ = reopened.Close()
	}
	if err == nil {
		t.Fatal("OpenWithProtector skipped checkpoint for plaintext payload migration")
	}
	if !strings.Contains(err.Error(), "checkpoint sqlite before protected payload migration") {
		t.Fatalf("OpenWithProtector error = %v, want migration checkpoint failure", err)
	}
}

func TestOpenWithProtectorMigratesLegacyNotNullDeliverySchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.sqlite")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`
	CREATE TABLE delivery_queue (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		event_id TEXT NOT NULL,
		chat_key TEXT NOT NULL,
		chat_id INTEGER NOT NULL,
		topic_id INTEGER NOT NULL,
		thread_id TEXT NOT NULL,
		kind TEXT NOT NULL,
		status TEXT NOT NULL,
		retry_count INTEGER NOT NULL DEFAULT 0,
		available_at TEXT NOT NULL,
		last_error TEXT,
		payload_json TEXT NOT NULL,
		created_at TEXT NOT NULL,
		updated_at TEXT NOT NULL
	)`); err != nil {
		t.Fatal(err)
	}
	now := string(model.NowString())
	markers := []struct {
		status string
		value  string
	}{
		{model.DeliveryStatusPending, "legacy pending marker"},
		{model.DeliveryStatusDelivered, "legacy delivered marker"},
		{model.DeliveryStatusDead, "legacy dead marker"},
	}
	for i, marker := range markers {
		if _, err := db.Exec(`
		INSERT INTO delivery_queue(event_id, chat_key, chat_id, topic_id, thread_id, kind, status, retry_count, available_at, payload_json, created_at, updated_at)
		VALUES (?, ?, 1, 0, '', '', ?, 0, ?, ?, ?, ?)`,
			fmt.Sprintf("e%d", i), fmt.Sprintf("1:%d", i), marker.status, now, marker.value, now, now); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	store, err := OpenWithProtector(path, securestore.NewDeterministicTestProtector())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	notNull, err := store.deliveryPayloadNotNull(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if notNull {
		t.Fatal("migrated payload_json remains NOT NULL")
	}
	var terminalWithPayload int
	if err := store.db.QueryRow(`
	SELECT count(*) FROM delivery_queue
	WHERE status IN ('delivered', 'dead') AND payload_json IS NOT NULL`).Scan(&terminalWithPayload); err != nil {
		t.Fatal(err)
	}
	if terminalWithPayload != 0 {
		t.Fatalf("terminal rows with payload = %d, want 0", terminalWithPayload)
	}
	raw := readRawSQLiteFile(t, path)
	for _, marker := range markers {
		if bytes.Contains(raw, []byte(marker.value)) {
			t.Fatalf("legacy %s marker remained in sqlite files", marker.status)
		}
	}
}

func TestOpenWithProtectorRejectsUnreadableLiveEnvelope(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.sqlite")
	legacy, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := legacy.EnqueueDelivery(context.Background(), model.DeliveryQueueItem{
		EventID:     "e1",
		ChatKey:     "1:0",
		Status:      model.DeliveryStatusPending,
		PayloadJSON: "dpapi:v1:not-readable!",
	}); err != nil {
		t.Fatal(err)
	}
	if err := legacy.Close(); err != nil {
		t.Fatal(err)
	}

	store, err := OpenWithProtector(path, securestore.NewDeterministicTestProtector())
	if store != nil {
		_ = store.Close()
		t.Fatal("protected store opened with unreadable live envelope")
	}
	if err == nil {
		t.Fatal("OpenWithProtector accepted unreadable live envelope")
	}
}

func TestThreadPanelLifecycleKeepsSingleCurrentPanelPerChatThread(t *testing.T) {
	t.Parallel()

	store := openTestStore(t)
	ctx := context.Background()

	first, err := store.CreateThreadPanel(ctx, model.ThreadPanel{
		ChatID:              123456789,
		TopicID:             0,
		ProjectName:         "Codex",
		ThreadID:            "thread-1",
		SummaryMessageID:    101,
		ToolMessageID:       102,
		OutputMessageID:     103,
		CurrentTurnID:       "turn-1",
		Status:              "inProgress",
		ArchiveEnabled:      true,
		LastSummaryHash:     "summary-1",
		LastToolHash:        "tool-1",
		LastOutputHash:      "output-1",
		RunNoticeMessageID:  99,
		LastRunNoticeFP:     "run-fp-1",
		UserMessageID:       100,
		LastUserNoticeFP:    "user-fp-1",
		PlanPromptMessageID: 110,
		LastPlanPromptFP:    "plan-fp-1",
	})
	if err != nil {
		t.Fatalf("CreateThreadPanel(first) failed: %v", err)
	}

	second, err := store.CreateThreadPanel(ctx, model.ThreadPanel{
		ChatID:           123456789,
		TopicID:          0,
		ProjectName:      "Codex",
		ThreadID:         "thread-1",
		SummaryMessageID: 201,
		ToolMessageID:    202,
		OutputMessageID:  203,
		CurrentTurnID:    "turn-2",
		Status:           "completed",
		ArchiveEnabled:   true,
		LastSummaryHash:  "summary-2",
		LastToolHash:     "tool-2",
		LastOutputHash:   "output-2",
	})
	if err != nil {
		t.Fatalf("CreateThreadPanel(second) failed: %v", err)
	}

	current, err := store.GetCurrentThreadPanel(ctx, 123456789, 0, "thread-1")
	if err != nil {
		t.Fatalf("GetCurrentThreadPanel failed: %v", err)
	}
	if current == nil || current.ID != second.ID {
		t.Fatalf("current panel = %#v, want second panel %#v", current, second)
	}

	firstLoaded, err := store.GetThreadPanelByID(ctx, first.ID)
	if err != nil {
		t.Fatalf("GetThreadPanelByID(first) failed: %v", err)
	}
	if firstLoaded == nil {
		t.Fatal("GetThreadPanelByID(first) returned nil")
	}
	if firstLoaded.IsCurrent {
		t.Fatalf("first panel should no longer be current: %#v", firstLoaded)
	}
	if firstLoaded.UserMessageID != 100 || firstLoaded.LastUserNoticeFP != "user-fp-1" {
		t.Fatalf("first user notice state = id %d fp %q, want 100/user-fp-1", firstLoaded.UserMessageID, firstLoaded.LastUserNoticeFP)
	}
	if firstLoaded.RunNoticeMessageID != 99 || firstLoaded.LastRunNoticeFP != "run-fp-1" {
		t.Fatalf("first run notice state = id %d fp %q, want 99/run-fp-1", firstLoaded.RunNoticeMessageID, firstLoaded.LastRunNoticeFP)
	}
	if firstLoaded.PlanPromptMessageID != 110 || firstLoaded.LastPlanPromptFP != "plan-fp-1" {
		t.Fatalf("first plan prompt state = id %d fp %q, want 110/plan-fp-1", firstLoaded.PlanPromptMessageID, firstLoaded.LastPlanPromptFP)
	}

	if err := store.UpdateThreadPanelUserNotice(ctx, second.ID, 250, "user-fp-2"); err != nil {
		t.Fatalf("UpdateThreadPanelUserNotice failed: %v", err)
	}
	secondLoaded, err := store.GetThreadPanelByID(ctx, second.ID)
	if err != nil {
		t.Fatalf("GetThreadPanelByID(second) failed: %v", err)
	}
	if secondLoaded.UserMessageID != 250 || secondLoaded.LastUserNoticeFP != "user-fp-2" {
		t.Fatalf("second user notice state = id %d fp %q, want 250/user-fp-2", secondLoaded.UserMessageID, secondLoaded.LastUserNoticeFP)
	}
	if err := store.UpdateThreadPanelRunNotice(ctx, second.ID, 240, "run-fp-2"); err != nil {
		t.Fatalf("UpdateThreadPanelRunNotice failed: %v", err)
	}
	secondLoaded, err = store.GetThreadPanelByID(ctx, second.ID)
	if err != nil {
		t.Fatalf("GetThreadPanelByID(second after run notice) failed: %v", err)
	}
	if secondLoaded.RunNoticeMessageID != 240 || secondLoaded.LastRunNoticeFP != "run-fp-2" {
		t.Fatalf("second run notice state = id %d fp %q, want 240/run-fp-2", secondLoaded.RunNoticeMessageID, secondLoaded.LastRunNoticeFP)
	}
	if err := store.UpdateThreadPanelPlanPrompt(ctx, second.ID, 260, "plan-fp-2"); err != nil {
		t.Fatalf("UpdateThreadPanelPlanPrompt failed: %v", err)
	}
	secondLoaded, err = store.GetThreadPanelByID(ctx, second.ID)
	if err != nil {
		t.Fatalf("GetThreadPanelByID(second after plan prompt) failed: %v", err)
	}
	if secondLoaded.PlanPromptMessageID != 260 || secondLoaded.LastPlanPromptFP != "plan-fp-2" {
		t.Fatalf("second plan prompt state = id %d fp %q, want 260/plan-fp-2", secondLoaded.PlanPromptMessageID, secondLoaded.LastPlanPromptFP)
	}
}

func TestThreadPanelSourceModePersistsStableAndPerRunValues(t *testing.T) {
	t.Parallel()

	store := openTestStore(t)
	ctx := context.Background()

	stable, err := store.CreateThreadPanel(ctx, model.ThreadPanel{
		ChatID:           123456789,
		TopicID:          0,
		ProjectName:      "Codex",
		ThreadID:         "thread-1",
		SourceMode:       "stable",
		SummaryMessageID: 301,
		ToolMessageID:    302,
		OutputMessageID:  303,
		CurrentTurnID:    "turn-1",
		Status:           "completed",
		ArchiveEnabled:   true,
	})
	if err != nil {
		t.Fatalf("CreateThreadPanel(stable) failed: %v", err)
	}
	if stable.SourceMode != "stable" {
		t.Fatalf("stable panel SourceMode = %q, want stable", stable.SourceMode)
	}

	perRun, err := store.CreateThreadPanel(ctx, model.ThreadPanel{
		ChatID:           123456789,
		TopicID:          0,
		ProjectName:      "Codex",
		ThreadID:         "thread-1",
		SourceMode:       "per_run",
		SummaryMessageID: 401,
		ToolMessageID:    402,
		OutputMessageID:  403,
		CurrentTurnID:    "turn-2",
		Status:           "completed",
		ArchiveEnabled:   true,
	})
	if err != nil {
		t.Fatalf("CreateThreadPanel(per_run) failed: %v", err)
	}
	if perRun.SourceMode != "per_run" {
		t.Fatalf("per_run panel SourceMode = %q, want per_run", perRun.SourceMode)
	}

	current, err := store.GetCurrentThreadPanel(ctx, 123456789, 0, "thread-1")
	if err != nil {
		t.Fatalf("GetCurrentThreadPanel failed: %v", err)
	}
	if current == nil {
		t.Fatal("GetCurrentThreadPanel returned nil")
	}
	if current.ID != perRun.ID {
		t.Fatalf("current panel ID = %d, want %d", current.ID, perRun.ID)
	}
	if current.SourceMode != "per_run" {
		t.Fatalf("current panel SourceMode = %q, want per_run", current.SourceMode)
	}

	stableLoaded, err := store.GetThreadPanelByID(ctx, stable.ID)
	if err != nil {
		t.Fatalf("GetThreadPanelByID(stable) failed: %v", err)
	}
	if stableLoaded == nil {
		t.Fatal("GetThreadPanelByID(stable) returned nil")
	}
	if stableLoaded.SourceMode != "stable" {
		t.Fatalf("stable panel reloaded SourceMode = %q, want stable", stableLoaded.SourceMode)
	}
}

func TestSteerStateArmLoadAndClear(t *testing.T) {
	t.Parallel()

	store := openTestStore(t)
	ctx := context.Background()

	state := model.SteerState{
		ChatKey:   model.ChatKey(123456789, 0),
		ChatID:    123456789,
		TopicID:   0,
		ThreadID:  "thread-1",
		TurnID:    "turn-1",
		PanelID:   77,
		ExpiresAt: model.NowString(),
		CreatedAt: model.NowString(),
		UpdatedAt: model.NowString(),
	}
	if err := store.ArmSteerState(ctx, state); err != nil {
		t.Fatalf("ArmSteerState failed: %v", err)
	}

	loaded, err := store.GetSteerState(ctx, 123456789, 0)
	if err != nil {
		t.Fatalf("GetSteerState failed: %v", err)
	}
	if loaded == nil {
		t.Fatal("GetSteerState returned nil")
	}
	if loaded.ThreadID != "thread-1" || loaded.TurnID != "turn-1" || loaded.PanelID != 77 {
		t.Fatalf("loaded steer state = %#v, want thread-1/turn-1/panel 77", loaded)
	}

	if err := store.ClearSteerState(ctx, 123456789, 0); err != nil {
		t.Fatalf("ClearSteerState failed: %v", err)
	}
	loaded, err = store.GetSteerState(ctx, 123456789, 0)
	if err != nil {
		t.Fatalf("GetSteerState(after clear) failed: %v", err)
	}
	if loaded != nil {
		t.Fatalf("GetSteerState(after clear) = %#v, want nil", loaded)
	}
}

func openTestStore(t *testing.T, protectors ...securestore.Protector) *Store {
	t.Helper()

	path := filepath.Join(t.TempDir(), "state.sqlite")
	var (
		store *Store
		err   error
	)
	if len(protectors) == 0 {
		store, err = Open(path)
	} else {
		store, err = OpenWithProtector(path, protectors[0])
	}
	if err != nil {
		t.Fatalf("Open(%s) failed: %v", path, err)
	}
	t.Cleanup(func() {
		_ = store.Close()
	})
	return store
}

func readRawSQLiteFile(t *testing.T, path string) []byte {
	t.Helper()

	var raw []byte
	for _, candidate := range []string{path, path + "-wal", path + "-shm"} {
		contents, err := os.ReadFile(candidate)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			t.Fatalf("read sqlite file %s: %v", candidate, err)
		}
		raw = append(raw, contents...)
	}
	return raw
}

func TestMinimalPickerRouteConsumesOnlyMatchingChatTopicOnce(t *testing.T) {
	t.Parallel()

	store := openTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)
	route := model.MinimalPickerRoute{
		Token:     "0123456789abcdef0123456789abcdef",
		Action:    "minimal_existing_open",
		ProjectID: "p1",
		ThreadID:  "thread-1",
		ChatID:    7,
		TopicID:   3,
		Status:    "active",
		ExpiresAt: model.TimeString(now.Add(time.Minute).Format(time.RFC3339Nano)),
	}
	if err := store.CreateMinimalPickerRoutes(ctx, []model.MinimalPickerRoute{route}); err != nil {
		t.Fatalf("CreateMinimalPickerRoutes failed: %v", err)
	}
	got, err := store.ConsumeMinimalPickerRoute(ctx, route.Token, 7, 3, now)
	if err != nil {
		t.Fatalf("ConsumeMinimalPickerRoute failed: %v", err)
	}
	if got == nil || got.ThreadID != "thread-1" || got.ProjectID != "p1" || got.Action != "minimal_existing_open" {
		t.Fatalf("consumed route = %#v, want thread-1/p1", got)
	}
	again, err := store.ConsumeMinimalPickerRoute(ctx, route.Token, 7, 3, now)
	if err != nil {
		t.Fatalf("ConsumeMinimalPickerRoute(second) failed: %v", err)
	}
	if again != nil {
		t.Fatalf("second consume = %#v, want nil", again)
	}
}

func TestMinimalPickerRouteRejectsWrongChatTopicExpiryAndRaces(t *testing.T) {
	t.Parallel()

	store := openTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)
	routes := []model.MinimalPickerRoute{
		{Token: "11111111111111111111111111111111", Action: "minimal_existing_open", ProjectID: "p1", ThreadID: "thread-wrong-chat", ChatID: 7, TopicID: 3, Status: "active", ExpiresAt: model.TimeString(now.Add(time.Minute).Format(time.RFC3339Nano))},
		{Token: "22222222222222222222222222222222", Action: "minimal_existing_open", ProjectID: "p1", ThreadID: "thread-wrong-topic", ChatID: 7, TopicID: 3, Status: "active", ExpiresAt: model.TimeString(now.Add(time.Minute).Format(time.RFC3339Nano))},
		{Token: "33333333333333333333333333333333", Action: "minimal_existing_open", ProjectID: "p1", ThreadID: "thread-expired", ChatID: 7, TopicID: 3, Status: "active", ExpiresAt: model.TimeString(now.Add(-time.Minute).Format(time.RFC3339Nano))},
		{Token: "44444444444444444444444444444444", Action: "minimal_existing_open", ProjectID: "p1", ThreadID: "thread-race", ChatID: 7, TopicID: 3, Status: "active", ExpiresAt: model.TimeString(now.Add(time.Minute).Format(time.RFC3339Nano))},
	}
	if err := store.CreateMinimalPickerRoutes(ctx, routes); err != nil {
		t.Fatalf("CreateMinimalPickerRoutes failed: %v", err)
	}
	for _, test := range []struct {
		name    string
		token   string
		chatID  int64
		topicID int64
	}{
		{name: "wrong chat", token: routes[0].Token, chatID: 8, topicID: 3},
		{name: "wrong topic", token: routes[1].Token, chatID: 7, topicID: 4},
		{name: "expired", token: routes[2].Token, chatID: 7, topicID: 3},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, err := store.ConsumeMinimalPickerRoute(ctx, test.token, test.chatID, test.topicID, now)
			if err != nil {
				t.Fatalf("ConsumeMinimalPickerRoute failed: %v", err)
			}
			if got != nil {
				t.Fatalf("consume = %#v, want nil", got)
			}
		})
	}

	results := make(chan *model.MinimalPickerRoute, 2)
	errs := make(chan error, 2)
	var wg sync.WaitGroup
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			route, err := store.ConsumeMinimalPickerRoute(ctx, routes[3].Token, 7, 3, now)
			results <- route
			errs <- err
		}()
	}
	wg.Wait()
	close(results)
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	winners := 0
	for result := range results {
		if result != nil {
			winners++
		}
	}
	if winners != 1 {
		t.Fatalf("concurrent route consumers = %d, want exactly one", winners)
	}
}

func TestMinimalPickerRouteRejectsDuplicateToken(t *testing.T) {
	t.Parallel()

	store := openTestStore(t)
	ctx := context.Background()
	future := model.TimeString(time.Now().UTC().Add(time.Minute).Format(time.RFC3339Nano))
	route := model.MinimalPickerRoute{Token: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Action: "minimal_existing_open", ProjectID: "p1", ThreadID: "thread-1", ChatID: 7, Status: "active", ExpiresAt: future}
	if err := store.CreateMinimalPickerRoutes(ctx, []model.MinimalPickerRoute{route}); err != nil {
		t.Fatalf("CreateMinimalPickerRoutes(first) failed: %v", err)
	}
	if err := store.CreateMinimalPickerRoutes(ctx, []model.MinimalPickerRoute{route}); err == nil {
		t.Fatal("CreateMinimalPickerRoutes accepted duplicate route token")
	}
}

func TestMinimalThreadObservationBaselinesTerminalThenClaimsActiveTransitionOnce(t *testing.T) {
	t.Parallel()

	store := openTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)
	thread := model.Thread{ID: "thread-1", Status: "completed", ActiveTurnID: "turn-1", UpdatedAt: 10}
	if err := store.ObserveMinimalThread(ctx, thread, "p1", now); err != nil {
		t.Fatalf("ObserveMinimalThread(terminal baseline) failed: %v", err)
	}
	baseline, err := store.ClaimMinimalTerminalTransition(ctx, "thread-1", "turn-1", "completed", now)
	if err != nil {
		t.Fatalf("ClaimMinimalTerminalTransition(baseline) failed: %v", err)
	}
	if baseline != nil {
		t.Fatalf("baseline claim = %#v, want nil", baseline)
	}

	active := model.Thread{ID: "thread-1", Status: "running", ActiveTurnID: "turn-2", UpdatedAt: 20}
	if err := store.ObserveMinimalThread(ctx, active, "p1", now.Add(time.Minute)); err != nil {
		t.Fatalf("ObserveMinimalThread(active) failed: %v", err)
	}
	claim, err := store.ClaimMinimalTerminalTransition(ctx, "thread-1", "turn-2", "completed", now.Add(2*time.Minute))
	if err != nil {
		t.Fatalf("ClaimMinimalTerminalTransition(active transition) failed: %v", err)
	}
	if claim == nil || claim.ThreadID != "thread-1" || claim.TurnID != "turn-2" || claim.Status != "completed" {
		t.Fatalf("transition claim = %#v, want thread-1/turn-2/completed", claim)
	}
	duplicate, err := store.ClaimMinimalTerminalTransition(ctx, "thread-1", "turn-2", "completed", now.Add(3*time.Minute))
	if err != nil {
		t.Fatalf("ClaimMinimalTerminalTransition(duplicate) failed: %v", err)
	}
	if duplicate != nil {
		t.Fatalf("duplicate transition claim = %#v, want nil", duplicate)
	}

	later := model.Thread{ID: "thread-1", Status: "running", ActiveTurnID: "turn-3", UpdatedAt: 30}
	if err := store.ObserveMinimalThread(ctx, later, "p1", now.Add(4*time.Minute)); err != nil {
		t.Fatalf("ObserveMinimalThread(later active) failed: %v", err)
	}
	laterClaim, err := store.ClaimMinimalTerminalTransition(ctx, "thread-1", "turn-3", "failed", now.Add(5*time.Minute))
	if err != nil {
		t.Fatalf("ClaimMinimalTerminalTransition(later turn) failed: %v", err)
	}
	if laterClaim == nil || laterClaim.ThreadID != "thread-1" || laterClaim.TurnID != "turn-3" || laterClaim.Status != "failed" {
		t.Fatalf("later transition claim = %#v, want thread-1/turn-3/failed", laterClaim)
	}
}

func TestMinimalObservedThreadsDueKeepsRegisteredRowsObservableAfterClaim(t *testing.T) {
	t.Parallel()

	store := openTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)
	terminal := model.Thread{ID: "observed-terminal", Title: "observed-terminal", Status: "completed", ActiveTurnID: "turn-old", UpdatedAt: 10}
	active := model.Thread{ID: "observed-active", Title: "observed-active", Status: "inProgress", ActiveTurnID: "turn-active", UpdatedAt: 20}
	for _, thread := range []model.Thread{terminal, active} {
		if err := store.UpsertThread(ctx, thread); err != nil {
			t.Fatalf("UpsertThread(%s) failed: %v", thread.ID, err)
		}
		if err := store.ObserveMinimalThread(ctx, thread, "p1", now); err != nil {
			t.Fatalf("ObserveMinimalThread(%s) failed: %v", thread.ID, err)
		}
	}
	due, err := store.ListMinimalObservedThreadsDue(ctx, 10)
	if err != nil {
		t.Fatalf("ListMinimalObservedThreadsDue failed: %v", err)
	}
	if got, want := storageThreadIDs(due), []string{"observed-active"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("due observed threads = %v, want %v", got, want)
	}
	if claim, err := store.ClaimMinimalTerminalTransition(ctx, "observed-active", "turn-active", "completed", now.Add(time.Minute)); err != nil || claim == nil {
		t.Fatalf("ClaimMinimalTerminalTransition(active) = %#v, %v; want claim", claim, err)
	}
	due, err = store.ListMinimalObservedThreadsDue(ctx, 10)
	if err != nil {
		t.Fatalf("ListMinimalObservedThreadsDue(after claim) failed: %v", err)
	}
	if got, want := storageThreadIDs(due), []string{"observed-active"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("due observed threads after claim = %v, want %v", got, want)
	}
	later := model.Thread{ID: "observed-active", Title: "observed-active", Status: "running", ActiveTurnID: "turn-later", UpdatedAt: 30}
	if err := store.UpsertThread(ctx, later); err != nil {
		t.Fatalf("UpsertThread(later) failed: %v", err)
	}
	if err := store.ObserveMinimalThread(ctx, later, "p1", now.Add(2*time.Minute)); err != nil {
		t.Fatalf("ObserveMinimalThread(later) failed: %v", err)
	}
	due, err = store.ListMinimalObservedThreadsDue(ctx, 10)
	if err != nil {
		t.Fatalf("ListMinimalObservedThreadsDue(later) failed: %v", err)
	}
	if got, want := storageThreadIDs(due), []string{"observed-active"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("due observed threads after later turn = %v, want %v", got, want)
	}
}

func TestMinimalObservedThreadsDueUsesImmutableDiscoverySequence(t *testing.T) {
	t.Parallel()

	store := openTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)
	first := model.Thread{ID: "discovery-a", Title: "discovery-a", Status: "inProgress", ActiveTurnID: "turn-a", UpdatedAt: 100}
	second := model.Thread{ID: "discovery-b", Title: "discovery-b", Status: "inProgress", ActiveTurnID: "turn-b", UpdatedAt: 200}
	if err := store.UpsertThread(ctx, first); err != nil {
		t.Fatalf("UpsertThread(first) failed: %v", err)
	}
	if err := store.ObserveMinimalThread(ctx, first, "p1", now); err != nil {
		t.Fatalf("ObserveMinimalThread(first) failed: %v", err)
	}
	if err := store.UpsertThread(ctx, second); err != nil {
		t.Fatalf("UpsertThread(second) failed: %v", err)
	}
	if err := store.ObserveMinimalThread(ctx, second, "p1", now.Add(time.Minute)); err != nil {
		t.Fatalf("ObserveMinimalThread(second) failed: %v", err)
	}

	firstTerminal := model.Thread{ID: first.ID, Title: first.Title, Status: "completed", UpdatedAt: 300}
	secondTerminal := model.Thread{ID: second.ID, Title: second.Title, Status: "completed", UpdatedAt: 400}
	for _, thread := range []model.Thread{firstTerminal, secondTerminal} {
		if err := store.UpsertThread(ctx, thread); err != nil {
			t.Fatalf("UpsertThread(%s terminal) failed: %v", thread.ID, err)
		}
		if err := store.ObserveMinimalThread(ctx, thread, "p1", now.Add(2*time.Minute)); err != nil {
			t.Fatalf("ObserveMinimalThread(%s terminal) failed: %v", thread.ID, err)
		}
	}

	due, err := store.ListMinimalObservedThreadsDue(ctx, 10)
	if err != nil {
		t.Fatalf("ListMinimalObservedThreadsDue failed: %v", err)
	}
	if got, want := storageThreadIDs(due), []string{"discovery-a", "discovery-b"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("due observed threads = %v, want discovery order %v", got, want)
	}
}

func TestMinimalObservationDiscoverySequenceBackfillsAndSurvivesRestart(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "legacy.sqlite")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("sql.Open(%s) failed: %v", path, err)
	}
	if _, err := db.Exec(`
	CREATE TABLE minimal_thread_observations (
		thread_id TEXT PRIMARY KEY,
		project_id TEXT NOT NULL,
		last_updated_at INTEGER NOT NULL,
		last_turn_id TEXT,
		last_turn_status TEXT,
		baseline_ready INTEGER NOT NULL,
		read_required INTEGER NOT NULL,
		updated_at TEXT NOT NULL
	);
	INSERT INTO minimal_thread_observations(thread_id, project_id, last_updated_at, last_turn_id, last_turn_status, baseline_ready, read_required, updated_at)
	VALUES
		('legacy-a', 'p1', 100, 'turn-a', 'running', 1, 1, '2026-08-21T12:00:00Z'),
		('legacy-b', 'p1', 200, 'turn-b', 'running', 1, 1, '2026-08-21T12:01:00Z')`); err != nil {
		_ = db.Close()
		t.Fatalf("seed legacy minimal observations failed: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	store, err := Open(path)
	if err != nil {
		t.Fatalf("Open(%s) failed: %v", path, err)
	}
	t.Cleanup(func() { _ = store.Close() })
	ctx := context.Background()
	if err := store.UpsertThread(ctx, model.Thread{ID: "legacy-a", Title: "legacy-a", Status: "completed", UpdatedAt: 300}); err != nil {
		t.Fatalf("UpsertThread(legacy-a) failed: %v", err)
	}
	if err := store.UpsertThread(ctx, model.Thread{ID: "legacy-b", Title: "legacy-b", Status: "completed", UpdatedAt: 400}); err != nil {
		t.Fatalf("UpsertThread(legacy-b) failed: %v", err)
	}
	due, err := store.ListMinimalObservedThreadsDue(ctx, 10)
	if err != nil {
		t.Fatalf("ListMinimalObservedThreadsDue(legacy) failed: %v", err)
	}
	if got, want := storageThreadIDs(due), []string{"legacy-a", "legacy-b"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("legacy due observed threads = %v, want stable backfill order %v", got, want)
	}
	seqBefore := minimalObservationDiscoverySeqs(t, store, "legacy-a", "legacy-b")
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := Open(path)
	if err != nil {
		t.Fatalf("Open(restart %s) failed: %v", path, err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	seqAfter := minimalObservationDiscoverySeqs(t, reopened, "legacy-a", "legacy-b")
	if !reflect.DeepEqual(seqAfter, seqBefore) {
		t.Fatalf("discovery sequence changed after restart: before=%v after=%v", seqBefore, seqAfter)
	}
	if !(seqAfter["legacy-a"] > 0 && seqAfter["legacy-b"] > seqAfter["legacy-a"]) {
		t.Fatalf("legacy discovery sequences = %v, want positive ascending stable values", seqAfter)
	}
}

func minimalObservationDiscoverySeqs(t *testing.T, store *Store, threadIDs ...string) map[string]int64 {
	t.Helper()
	out := make(map[string]int64, len(threadIDs))
	for _, threadID := range threadIDs {
		var seq int64
		if err := store.db.QueryRow(`SELECT discovery_seq FROM minimal_thread_observations WHERE thread_id = ?`, threadID).Scan(&seq); err != nil {
			t.Fatalf("select discovery_seq(%s) failed: %v", threadID, err)
		}
		out[threadID] = seq
	}
	return out
}

func TestMinimalThreadObservationTerminalListRowWithoutTurnIDKeepsActiveTupleDue(t *testing.T) {
	t.Parallel()

	store := openTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)
	active := model.Thread{ID: "observed-no-list-turn", Title: "observed-no-list-turn", Status: "inProgress", ActiveTurnID: "turn-active", UpdatedAt: 20}
	if err := store.UpsertThread(ctx, active); err != nil {
		t.Fatalf("UpsertThread(active) failed: %v", err)
	}
	if err := store.ObserveMinimalThread(ctx, active, "p1", now); err != nil {
		t.Fatalf("ObserveMinimalThread(active) failed: %v", err)
	}
	listTerminal := model.Thread{ID: active.ID, Title: active.Title, Status: "completed", UpdatedAt: 30}
	if err := store.UpsertThread(ctx, listTerminal); err != nil {
		t.Fatalf("UpsertThread(list terminal) failed: %v", err)
	}
	if err := store.ObserveMinimalThread(ctx, listTerminal, "p1", now.Add(time.Minute)); err != nil {
		t.Fatalf("ObserveMinimalThread(list terminal) failed: %v", err)
	}
	due, err := store.ListMinimalObservedThreadsDue(ctx, 10)
	if err != nil {
		t.Fatalf("ListMinimalObservedThreadsDue failed: %v", err)
	}
	if got, want := storageThreadIDs(due), []string{active.ID}; !reflect.DeepEqual(got, want) {
		t.Fatalf("due observed threads = %v, want %v", got, want)
	}
	claim, err := store.ClaimMinimalTerminalTransition(ctx, active.ID, active.ActiveTurnID, "completed", now.Add(2*time.Minute))
	if err != nil {
		t.Fatalf("ClaimMinimalTerminalTransition failed: %v", err)
	}
	if claim == nil || claim.ThreadID != active.ID || claim.TurnID != active.ActiveTurnID {
		t.Fatalf("claim after terminal list row without turn = %#v, want active tuple", claim)
	}
}

func TestMinimalThreadObservationActiveListRowWithoutTurnIDKeepsKnownActiveTuple(t *testing.T) {
	t.Parallel()

	store := openTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)
	active := model.Thread{ID: "observed-active-list", Title: "observed-active-list", Status: "inProgress", ActiveTurnID: "turn-active", UpdatedAt: 20}
	if err := store.UpsertThread(ctx, active); err != nil {
		t.Fatalf("UpsertThread(active) failed: %v", err)
	}
	if err := store.ObserveMinimalThread(ctx, active, "p1", now); err != nil {
		t.Fatalf("ObserveMinimalThread(active) failed: %v", err)
	}
	listActive := model.Thread{ID: active.ID, Title: active.Title, Status: "inProgress", UpdatedAt: 30}
	if err := store.UpsertThread(ctx, listActive); err != nil {
		t.Fatalf("UpsertThread(list active) failed: %v", err)
	}
	if err := store.ObserveMinimalThread(ctx, listActive, "p1", now.Add(time.Minute)); err != nil {
		t.Fatalf("ObserveMinimalThread(list active) failed: %v", err)
	}
	claim, err := store.ClaimMinimalTerminalTransition(ctx, active.ID, active.ActiveTurnID, "completed", now.Add(2*time.Minute))
	if err != nil {
		t.Fatalf("ClaimMinimalTerminalTransition failed: %v", err)
	}
	if claim == nil || claim.ThreadID != active.ID || claim.TurnID != active.ActiveTurnID {
		t.Fatalf("claim after active list row without turn = %#v, want active tuple", claim)
	}
}

func TestMinimalThreadObservationIgnoresOlderSnapshotAfterTerminalClaim(t *testing.T) {
	t.Parallel()

	store := openTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)
	active := model.Thread{ID: "thread-stale", Status: "running", ActiveTurnID: "turn-1", UpdatedAt: 100}
	if err := store.ObserveMinimalThread(ctx, active, "p1", now); err != nil {
		t.Fatalf("ObserveMinimalThread(active) failed: %v", err)
	}
	first, err := store.ClaimMinimalTerminalTransition(ctx, "thread-stale", "turn-1", "completed", now.Add(time.Minute))
	if err != nil {
		t.Fatalf("ClaimMinimalTerminalTransition(first) failed: %v", err)
	}
	if first == nil {
		t.Fatal("first terminal transition was not claimed")
	}

	stale := model.Thread{ID: "thread-stale", Status: "running", ActiveTurnID: "turn-1", UpdatedAt: 90}
	if err := store.ObserveMinimalThread(ctx, stale, "p1", now.Add(2*time.Minute)); err != nil {
		t.Fatalf("ObserveMinimalThread(stale) failed: %v", err)
	}
	again, err := store.ClaimMinimalTerminalTransition(ctx, "thread-stale", "turn-1", "completed", now.Add(3*time.Minute))
	if err != nil {
		t.Fatalf("ClaimMinimalTerminalTransition(replay) failed: %v", err)
	}
	if again != nil {
		t.Fatalf("stale snapshot re-armed terminal claim = %#v, want nil", again)
	}
}

func TestMinimalThreadObservationIgnoresEqualTimestampSnapshotAfterTerminalClaim(t *testing.T) {
	t.Parallel()

	store := openTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)
	active := model.Thread{ID: "thread-equal-stale", Status: "running", ActiveTurnID: "turn-1", UpdatedAt: 100}
	if err := store.ObserveMinimalThread(ctx, active, "p1", now); err != nil {
		t.Fatalf("ObserveMinimalThread(active) failed: %v", err)
	}
	first, err := store.ClaimMinimalTerminalTransition(ctx, "thread-equal-stale", "turn-1", "completed", now.Add(time.Minute))
	if err != nil {
		t.Fatalf("ClaimMinimalTerminalTransition(first) failed: %v", err)
	}
	if first == nil {
		t.Fatal("first terminal transition was not claimed")
	}

	replay := model.Thread{ID: "thread-equal-stale", Status: "running", ActiveTurnID: "turn-1", UpdatedAt: 100}
	if err := store.ObserveMinimalThread(ctx, replay, "p1", now.Add(2*time.Minute)); err != nil {
		t.Fatalf("ObserveMinimalThread(equal timestamp replay) failed: %v", err)
	}
	again, err := store.ClaimMinimalTerminalTransition(ctx, "thread-equal-stale", "turn-1", "completed", now.Add(3*time.Minute))
	if err != nil {
		t.Fatalf("ClaimMinimalTerminalTransition(replay) failed: %v", err)
	}
	if again != nil {
		t.Fatalf("equal timestamp replay re-armed terminal claim = %#v, want nil", again)
	}
}

func TestMinimalThreadObservationAcceptsEqualTimestampDistinctTurnAfterTerminalClaim(t *testing.T) {
	t.Parallel()

	store := openTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)
	firstTurn := model.Thread{ID: "thread-equal-next", Status: "running", ActiveTurnID: "turn-1", UpdatedAt: 100}
	if err := store.ObserveMinimalThread(ctx, firstTurn, "p1", now); err != nil {
		t.Fatalf("ObserveMinimalThread(first turn) failed: %v", err)
	}
	first, err := store.ClaimMinimalTerminalTransition(ctx, "thread-equal-next", "turn-1", "completed", now.Add(time.Minute))
	if err != nil {
		t.Fatalf("ClaimMinimalTerminalTransition(first) failed: %v", err)
	}
	if first == nil {
		t.Fatal("first terminal transition was not claimed")
	}

	secondTurn := model.Thread{ID: "thread-equal-next", Status: "running", ActiveTurnID: "turn-2", UpdatedAt: 100}
	if err := store.ObserveMinimalThread(ctx, secondTurn, "p1", now.Add(2*time.Minute)); err != nil {
		t.Fatalf("ObserveMinimalThread(second turn) failed: %v", err)
	}
	second, err := store.ClaimMinimalTerminalTransition(ctx, "thread-equal-next", "turn-2", "completed", now.Add(3*time.Minute))
	if err != nil {
		t.Fatalf("ClaimMinimalTerminalTransition(second) failed: %v", err)
	}
	if second == nil || second.ThreadID != "thread-equal-next" || second.TurnID != "turn-2" || second.Status != "completed" {
		t.Fatalf("second terminal transition = %#v, want thread-equal-next/turn-2/completed", second)
	}
}

func TestMinimalThreadObservationRequiresKnownNonterminalTupleAndStoresNoBodies(t *testing.T) {
	t.Parallel()

	store := openTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)
	if claim, err := store.ClaimMinimalTerminalTransition(ctx, "unknown-thread", "turn-1", "completed", now); err != nil || claim != nil {
		t.Fatalf("unknown claim = %#v, %v; want nil", claim, err)
	}
	thread := model.Thread{
		ID:            "thread-private",
		Title:         "PRIVATE_TITLE_SENTINEL_b8760d",
		CWD:           `C:\PRIVATE_PATH_SENTINEL_b8760d`,
		Status:        "running",
		LastPreview:   "PRIVATE_PROMPT_SENTINEL_b8760d",
		ActiveTurnID:  "turn-private",
		UpdatedAt:     99,
		Raw:           []byte(`{"summary":"PRIVATE_SUMMARY_SENTINEL_b8760d","final":"PRIVATE_FINAL_SENTINEL_b8760d"}`),
		ProjectName:   "PRIVATE_PROJECT_NAME_SENTINEL_b8760d",
		DirectoryName: "PRIVATE_DIRECTORY_SENTINEL_b8760d",
	}
	if err := store.ObserveMinimalThread(ctx, thread, "p-private", now); err != nil {
		t.Fatalf("ObserveMinimalThread failed: %v", err)
	}
	wrongTurn, err := store.ClaimMinimalTerminalTransition(ctx, "thread-private", "turn-other", "completed", now.Add(time.Minute))
	if err != nil {
		t.Fatalf("ClaimMinimalTerminalTransition(wrong turn) failed: %v", err)
	}
	if wrongTurn != nil {
		t.Fatalf("wrong turn claim = %#v, want nil", wrongTurn)
	}
	if _, err := store.db.ExecContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		t.Fatal(err)
	}
	raw := readRawSQLiteFile(t, store.Path())
	for _, sentinel := range []string{
		"PRIVATE_TITLE_SENTINEL_b8760d",
		"PRIVATE_PATH_SENTINEL_b8760d",
		"PRIVATE_PROMPT_SENTINEL_b8760d",
		"PRIVATE_SUMMARY_SENTINEL_b8760d",
		"PRIVATE_FINAL_SENTINEL_b8760d",
		"PRIVATE_PROJECT_NAME_SENTINEL_b8760d",
		"PRIVATE_DIRECTORY_SENTINEL_b8760d",
	} {
		if bytes.Contains(raw, []byte(sentinel)) {
			t.Fatalf("minimal observation stored plaintext sentinel %q in SQLite/WAL/SHM", sentinel)
		}
	}
}

func TestCheckpointWALRetriesTransientBusyAndHonorsPersistentBusy(t *testing.T) {
	t.Parallel()

	transientPath := filepath.Join(t.TempDir(), "transient.sqlite")
	checkpointDB := openCheckpointTestDB(t, transientPath, 5000)
	readerDB := openCheckpointTestDB(t, transientPath, 1)
	seedCheckpointBusyDB(t, checkpointDB)
	readerRows, releaseReader := holdCheckpointReader(t, readerDB)
	t.Cleanup(releaseReader)
	if _, err := checkpointDB.ExecContext(context.Background(), `INSERT INTO checkpoint_busy(v) VALUES (2)`); err != nil {
		t.Fatal(err)
	}
	released := make(chan struct{})
	go func() {
		time.Sleep(40 * time.Millisecond)
		releaseReader()
		close(released)
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	if err := checkpointWAL(ctx, checkpointDB); err != nil {
		t.Fatalf("transient busy checkpoint failed after reader release: %v", err)
	}
	select {
	case <-released:
	default:
		t.Fatal("checkpoint returned before reader was released")
	}
	_ = readerRows.Close()

	ctx, subprocessCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer subprocessCancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestCheckpointWALPersistentBusyHelper$", "-test.v")
	cmd.Env = append(os.Environ(), "CTR_GO_CHECKPOINT_PERSISTENT_BUSY_HELPER=1")
	output, err := cmd.CombinedOutput()
	if ctx.Err() != nil {
		t.Fatalf("persistent busy helper exceeded deadline: %v\n%s", ctx.Err(), output)
	}
	if err != nil {
		t.Fatalf("persistent busy helper failed: %v\n%s", err, output)
	}
}

func TestCheckpointBusyErrorUsesSQLiteCodeNotMessageText(t *testing.T) {
	t.Parallel()

	if !checkpointBusyError(sqliteCodeTestError{code: sqlite3.SQLITE_BUSY, text: "database is locked (SQLITE_BUSY)"}) {
		t.Fatal("SQLITE_BUSY code was not classified as retryable")
	}
	if checkpointBusyError(fmt.Errorf("wrapped locked table: %w", sqliteCodeTestError{code: sqlite3.SQLITE_LOCKED, text: "database table is locked (SQLITE_LOCKED)"})) {
		t.Fatal("SQLITE_LOCKED code was classified as retryable")
	}
	if checkpointBusyError(fmt.Errorf("plain text busy but not sqlite coded")) {
		t.Fatal("uncoded busy text was classified as retryable")
	}
}

type sqliteCodeTestError struct {
	code int
	text string
}

func (e sqliteCodeTestError) Error() string {
	return e.text
}

func (e sqliteCodeTestError) Code() int {
	return e.code
}

func TestCheckpointWALPersistentBusyHelper(t *testing.T) {
	if os.Getenv("CTR_GO_CHECKPOINT_PERSISTENT_BUSY_HELPER") != "1" {
		t.Skip("helper process only")
	}
	root, err := os.MkdirTemp("", "checkpoint-persistent-busy-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	persistentPath := filepath.Join(root, "persistent.sqlite")
	persistentCheckpointDB := openCheckpointTestDB(t, persistentPath, 1)
	persistentReaderDB := openCheckpointTestDB(t, persistentPath, 1)
	seedCheckpointBusyDB(t, persistentCheckpointDB)
	_, releasePersistentReader := holdCheckpointReader(t, persistentReaderDB)
	t.Cleanup(releasePersistentReader)
	if _, err := persistentCheckpointDB.ExecContext(context.Background(), `INSERT INTO checkpoint_busy(v) VALUES (2)`); err != nil {
		t.Fatal(err)
	}
	persistentCtx, persistentCancel := context.WithTimeout(context.Background(), 80*time.Millisecond)
	defer persistentCancel()
	started := time.Now()
	err = checkpointWAL(persistentCtx, persistentCheckpointDB)
	if err == nil {
		t.Fatal("persistent busy checkpoint succeeded; want useful error")
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("persistent busy checkpoint took %s, want bounded by context deadline; err=%v", elapsed, err)
	}
	if !strings.Contains(strings.ToLower(err.Error()), "busy") && !strings.Contains(strings.ToLower(err.Error()), "deadline") && !strings.Contains(strings.ToLower(err.Error()), "context") {
		t.Fatalf("persistent busy error = %v, want busy/deadline context", err)
	}
}

func openCheckpointTestDB(t *testing.T, path string, busyTimeoutMS int) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	if _, err := db.ExecContext(context.Background(), `PRAGMA journal_mode=WAL`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(context.Background(), fmt.Sprintf(`PRAGMA busy_timeout=%d`, busyTimeoutMS)); err != nil {
		t.Fatal(err)
	}
	return db
}

func seedCheckpointBusyDB(t *testing.T, db *sql.DB) {
	t.Helper()
	if _, err := db.ExecContext(context.Background(), `CREATE TABLE checkpoint_busy(v INTEGER NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(context.Background(), `INSERT INTO checkpoint_busy(v) VALUES (1)`); err != nil {
		t.Fatal(err)
	}
}

func holdCheckpointReader(t *testing.T, db *sql.DB) (*sql.Rows, func()) {
	t.Helper()
	tx, err := db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	rows, err := tx.QueryContext(context.Background(), `SELECT v FROM checkpoint_busy`)
	if err != nil {
		_ = tx.Rollback()
		t.Fatal(err)
	}
	if !rows.Next() {
		_ = rows.Close()
		_ = tx.Rollback()
		t.Fatal("checkpoint reader did not see seed row")
	}
	var once sync.Once
	release := func() {
		once.Do(func() {
			_ = rows.Close()
			_ = tx.Rollback()
		})
	}
	return rows, release
}

func TestMinimalApprovalPersistsRecordRoutesAndProtectedDeliveryAtomically(t *testing.T) {
	t.Parallel()

	store := openTestStore(t, securestore.NewDeterministicTestProtector())
	ctx := context.Background()
	rawCommand := `powershell -Command "Set-Content C:\private\marker.txt ok"`
	payload := model.DeliveryPayload{
		Text: "Bridge\n명령 실행\n" + rawCommand,
		Buttons: [][]model.ButtonSpec{{
			{Text: "승인", CallbackData: "11111111111111111111111111111111"},
			{Text: "거부", CallbackData: "22222222222222222222222222222222"},
		}},
		ThreadID: "thr-1",
		TurnID:   "turn-1",
		EventID:  "approval-event-1",
	}
	created, err := store.CreateMinimalApproval(ctx, MinimalApprovalSeed{
		Approval: MinimalApproval{
			RequestID:       "req-1",
			ThreadID:        "thr-1",
			TurnID:          "turn-1",
			RequestKind:     "item/commandExecution/requestApproval",
			ProjectName:     "Bridge",
			SessionIdentity: "session-a",
			Status:          "pending",
		},
		ApproveToken: "11111111111111111111111111111111",
		DenyToken:    "22222222222222222222222222222222",
		Delivery: model.DeliveryQueueItem{
			EventID:     "approval-event-1",
			ChatKey:     model.ChatKey(7, 0),
			ChatID:      7,
			TopicID:     0,
			ThreadID:    "thr-1",
			Kind:        "minimal_approval",
			Status:      model.DeliveryStatusPending,
			PayloadJSON: MustJSON(payload),
		},
	})
	if err != nil {
		t.Fatalf("CreateMinimalApproval failed: %v", err)
	}
	if !created {
		t.Fatal("CreateMinimalApproval reported duplicate for new request")
	}

	approval, err := store.GetMinimalApproval(ctx, "req-1")
	if err != nil || approval == nil {
		t.Fatalf("GetMinimalApproval = %#v, %v", approval, err)
	}
	if approval.Status != "pending" || approval.TelegramMessageID != 0 || approval.ProjectName != "Bridge" {
		t.Fatalf("approval = %#v", approval)
	}
	for _, token := range []string{"11111111111111111111111111111111", "22222222222222222222222222222222"} {
		route, routeErr := store.GetMinimalApprovalRoute(ctx, token)
		if routeErr != nil || route == nil || route.RequestID != "req-1" || route.Status != "active" {
			t.Fatalf("route %s = %#v, %v", token, route, routeErr)
		}
	}
	items, err := store.ClaimDeliveryBatch(ctx, 10)
	if err != nil || len(items) != 1 {
		t.Fatalf("ClaimDeliveryBatch = %#v, %v", items, err)
	}
	var claimedPayload model.DeliveryPayload
	if err := json.Unmarshal([]byte(items[0].PayloadJSON), &claimedPayload); err != nil || !strings.Contains(claimedPayload.Text, rawCommand) {
		t.Fatalf("decrypted delivery omitted summary: %q, %v", items[0].PayloadJSON, err)
	}
	if raw := readRawSQLiteFile(t, store.Path()); bytes.Contains(raw, []byte(rawCommand)) || bytes.Contains(raw, []byte(`C:\private\marker.txt`)) {
		t.Fatal("raw command or private path leaked into SQLite/WAL/SHM")
	}
}

func TestMinimalApprovalDeliveryBindsMessageAndBothRoutesInOneTransaction(t *testing.T) {
	t.Parallel()

	store := seedStorageMinimalApproval(t, "req-bind")
	ctx := context.Background()
	items, err := store.ClaimDeliveryBatch(ctx, 1)
	if err != nil || len(items) != 1 {
		t.Fatalf("ClaimDeliveryBatch = %#v, %v", items, err)
	}
	if err := store.CompleteMinimalApprovalDelivery(ctx, items[0].ID, "req-bind", 7, 0, 600); err != nil {
		t.Fatalf("CompleteMinimalApprovalDelivery failed: %v", err)
	}
	approval, _ := store.GetMinimalApproval(ctx, "req-bind")
	if approval == nil || approval.TelegramMessageID != 600 || approval.ChatID != 7 {
		t.Fatalf("approval = %#v", approval)
	}
	for _, token := range []string{"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"} {
		route, _ := store.GetMinimalApprovalRoute(ctx, token)
		if route == nil || route.TelegramMessageID != 600 || route.ChatID != 7 || route.Status != "active" {
			t.Fatalf("route %s = %#v", token, route)
		}
	}
	if items, err := store.ClaimDeliveryBatch(ctx, 1); err != nil || len(items) != 0 {
		t.Fatalf("delivered item replayed: %#v, %v", items, err)
	}
}

func TestMinimalApprovalClaimDisablesSiblingAndRecoveryNeverReactivatesAmbiguousClaim(t *testing.T) {
	t.Parallel()

	store := seedStorageMinimalApproval(t, "req-race")
	ctx := context.Background()
	items, _ := store.ClaimDeliveryBatch(ctx, 1)
	if err := store.CompleteMinimalApprovalDelivery(ctx, items[0].ID, "req-race", 7, 0, 600); err != nil {
		t.Fatal(err)
	}
	claim, claimed, err := store.ClaimMinimalApproval(ctx, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", 7, 0, 600, "session-a")
	if err != nil || !claimed || claim == nil || claim.Action != "approve" {
		t.Fatalf("first claim = %#v, %t, %v", claim, claimed, err)
	}
	if _, claimed, err := store.ClaimMinimalApproval(ctx, "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", 7, 0, 600, "session-a"); err != nil || claimed {
		t.Fatalf("sibling claim = %t, %v; want consumed", claimed, err)
	}
	approval, _ := store.GetMinimalApproval(ctx, "req-race")
	if approval == nil || approval.Status != "pending" || approval.ClaimState != "claimed" {
		t.Fatalf("public/internal claim state = %#v", approval)
	}
	if n, err := store.RecoverMinimalApprovalClaims(ctx); err != nil || n != 1 {
		t.Fatalf("RecoverMinimalApprovalClaims = %d, %v", n, err)
	}
	approval, _ = store.GetMinimalApproval(ctx, "req-race")
	if approval == nil || approval.Status != "cancelled" || approval.ClaimState != "idle" {
		t.Fatalf("recovered approval = %#v", approval)
	}
	if _, claimed, err := store.ClaimMinimalApproval(ctx, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", 7, 0, 600, "session-a"); err != nil || claimed {
		t.Fatalf("recovered route reactivated: %t, %v", claimed, err)
	}
}

func seedStorageMinimalApproval(t *testing.T, requestID string) *Store {
	t.Helper()
	store := openTestStore(t, securestore.NewDeterministicTestProtector())
	_, err := store.CreateMinimalApproval(context.Background(), MinimalApprovalSeed{
		Approval:     MinimalApproval{RequestID: requestID, ThreadID: "thr-1", TurnID: "turn-1", RequestKind: "item/commandExecution/requestApproval", ProjectName: "Bridge", SessionIdentity: "session-a", Status: "pending"},
		ApproveToken: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		DenyToken:    "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		Delivery: model.DeliveryQueueItem{
			EventID: requestID + "-event", ChatKey: model.ChatKey(7, 0), ChatID: 7, ThreadID: "thr-1", Kind: "minimal_approval", Status: model.DeliveryStatusPending,
			PayloadJSON: MustJSON(model.DeliveryPayload{Text: "approval", ThreadID: "thr-1", TurnID: "turn-1"}),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return store
}

func TestMinimalApprovalReopenAfterAcceptedSendNeverRetriesAndCallbackSelfBindsOnce(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "state.sqlite")
	protector := securestore.NewDeterministicTestProtector()
	store, err := OpenWithProtector(path, protector)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	_, err = store.CreateMinimalApproval(ctx, MinimalApprovalSeed{
		Approval:     MinimalApproval{RequestID: "req-crash", ThreadID: "thr-1", TurnID: "turn-1", RequestKind: "item/commandExecution/requestApproval", ProjectName: "Bridge", SessionIdentity: "session-a", Status: "pending"},
		ApproveToken: "11111111111111111111111111111111",
		DenyToken:    "22222222222222222222222222222222",
		Delivery:     model.DeliveryQueueItem{EventID: "approval-crash", ChatKey: model.ChatKey(7, 3), ChatID: 7, TopicID: 3, ThreadID: "thr-1", Kind: "minimal_approval", Status: model.DeliveryStatusPending, PayloadJSON: MustJSON(model.DeliveryPayload{Text: "approval"})},
	})
	if err != nil {
		t.Fatal(err)
	}
	items, err := store.ClaimDeliveryBatch(ctx, 1)
	if err != nil || len(items) != 1 {
		t.Fatalf("ClaimDeliveryBatch = %#v, %v", items, err)
	}
	// Telegram accepted this send as message 600, but the process crashed before
	// CompleteMinimalApprovalDelivery could bind that id.
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = OpenWithProtector(path, protector)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if items, err := store.ClaimDeliveryBatch(ctx, 1); err != nil || len(items) != 0 {
		t.Fatalf("ambiguous accepted send retried: %#v, %v", items, err)
	}
	if backlog, err := store.DeliveryQueueBacklog(ctx); err != nil || backlog != 0 {
		t.Fatalf("ambiguous delivery backlog = %d, %v", backlog, err)
	}
	route, _ := store.GetMinimalApprovalRoute(ctx, "11111111111111111111111111111111")
	if route == nil || route.ChatID != 7 || route.TopicID != 3 || route.TelegramMessageID != 0 || route.Status != "active" {
		t.Fatalf("preserved ambiguous route = %#v", route)
	}
	for _, negative := range []struct {
		name      string
		token     string
		chatID    int64
		topicID   int64
		messageID int64
		sessionID string
	}{
		{name: "wrong chat", token: route.Token, chatID: 8, topicID: 3, messageID: 600, sessionID: "session-a"},
		{name: "wrong topic", token: route.Token, chatID: 7, topicID: 4, messageID: 600, sessionID: "session-a"},
		{name: "zero message", token: route.Token, chatID: 7, topicID: 3, messageID: 0, sessionID: "session-a"},
		{name: "wrong token", token: "33333333333333333333333333333333", chatID: 7, topicID: 3, messageID: 600, sessionID: "session-a"},
		{name: "wrong session", token: route.Token, chatID: 7, topicID: 3, messageID: 600, sessionID: "session-b"},
	} {
		t.Run(negative.name, func(t *testing.T) {
			if _, claimed, err := store.ClaimMinimalApproval(ctx, negative.token, negative.chatID, negative.topicID, negative.messageID, negative.sessionID); err != nil || claimed {
				t.Fatalf("negative claim = %t, %v", claimed, err)
			}
		})
	}
	claim, claimed, err := store.ClaimMinimalApproval(ctx, route.Token, 7, 3, 600, "session-a")
	if err != nil || !claimed || claim == nil {
		t.Fatalf("self-bind claim = %#v, %t, %v", claim, claimed, err)
	}
	approval, _ := store.GetMinimalApproval(ctx, "req-crash")
	if approval == nil || approval.TelegramMessageID != 600 || approval.ChatID != 7 || approval.TopicID != 3 {
		t.Fatalf("self-bound approval = %#v", approval)
	}
	if _, claimed, err := store.ClaimMinimalApproval(ctx, route.Token, 7, 3, 601, "session-a"); err != nil || claimed {
		t.Fatalf("bound route accepted wrong message: %t, %v", claimed, err)
	}
}

func TestMinimalApprovalRequestIDReuseReplacesMismatchedLogicalIdentity(t *testing.T) {
	t.Parallel()

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
			store := openTestStore(t, securestore.NewDeterministicTestProtector())
			ctx := context.Background()
			seedLogicalApproval(t, store, "req-reused", "thr-1", "turn-1", "item/commandExecution/requestApproval", "session-a", "11111111111111111111111111111111", "22222222222222222222222222222222", "event-old")
			created := seedLogicalApproval(t, store, "req-reused", tc.threadID, tc.turnID, tc.requestKind, "session-a", "33333333333333333333333333333333", "44444444444444444444444444444444", "event-new")
			if !created {
				t.Fatal("mismatched logical identity was suppressed as replay")
			}
			approval, _ := store.GetMinimalApproval(ctx, "req-reused")
			if approval == nil || approval.ThreadID != tc.threadID || approval.TurnID != tc.turnID || approval.RequestKind != tc.requestKind {
				t.Fatalf("replacement approval = %#v", approval)
			}
			oldRoute, _ := store.GetMinimalApprovalRoute(ctx, "11111111111111111111111111111111")
			if oldRoute == nil || oldRoute.Status != "expired" {
				t.Fatalf("old route = %#v", oldRoute)
			}
			if _, claimed, err := store.ClaimMinimalApproval(ctx, oldRoute.Token, 7, 0, 600, "session-a"); err != nil || claimed {
				t.Fatalf("stale route claimed = %t, %v", claimed, err)
			}
		})
	}
}

func TestMinimalApprovalRequestIDCanBeReusedAcrossMultipleLogicalIdentities(t *testing.T) {
	store := openTestStore(t, securestore.NewDeterministicTestProtector())
	seedLogicalApproval(t, store, "req-reused-many", "thr-1", "turn-1", "item/commandExecution/requestApproval", "session-a", "11111111111111111111111111111111", "22222222222222222222222222222222", "event-reused-many-1")
	if created := seedLogicalApproval(t, store, "req-reused-many", "thr-2", "turn-1", "item/commandExecution/requestApproval", "session-a", "33333333333333333333333333333333", "44444444444444444444444444444444", "event-reused-many-2"); !created {
		t.Fatal("first logical replacement was suppressed")
	}
	if created := seedLogicalApproval(t, store, "req-reused-many", "thr-3", "turn-1", "item/commandExecution/requestApproval", "session-a", "55555555555555555555555555555555", "66666666666666666666666666666666", "event-reused-many-3"); !created {
		t.Fatal("second logical replacement was suppressed")
	}
	approval, _ := store.GetMinimalApproval(context.Background(), "req-reused-many")
	if approval == nil || approval.ThreadID != "thr-3" || approval.Status != "pending" {
		t.Fatalf("multiply reused approval = %#v", approval)
	}
	for _, token := range []string{"11111111111111111111111111111111", "33333333333333333333333333333333"} {
		route, _ := store.GetMinimalApprovalRoute(context.Background(), token)
		if route == nil || route.Status != "expired" {
			t.Fatalf("historical route %q = %#v", token, route)
		}
	}
	assertMinimalApprovalRouteStatus(t, store, "55555555555555555555555555555555", "active")
}

func TestMinimalApprovalExactReplayRefreshesSessionWithoutReplacingRoutes(t *testing.T) {
	t.Parallel()

	store := openTestStore(t, securestore.NewDeterministicTestProtector())
	created := seedLogicalApproval(t, store, "req-replay", "thr-1", "turn-1", "item/commandExecution/requestApproval", "session-a", "11111111111111111111111111111111", "22222222222222222222222222222222", "event-a")
	if !created {
		t.Fatal("first seed was duplicate")
	}
	created = seedLogicalApproval(t, store, "req-replay", "thr-1", "turn-1", "item/commandExecution/requestApproval", "session-b", "33333333333333333333333333333333", "44444444444444444444444444444444", "event-b")
	if created {
		t.Fatal("exact replay replaced presentation")
	}
	approval, _ := store.GetMinimalApproval(context.Background(), "req-replay")
	if approval == nil || approval.SessionIdentity != "session-b" {
		t.Fatalf("replayed session identity = %#v", approval)
	}
	oldRoute, _ := store.GetMinimalApprovalRoute(context.Background(), "11111111111111111111111111111111")
	newRoute, _ := store.GetMinimalApprovalRoute(context.Background(), "33333333333333333333333333333333")
	if oldRoute == nil || oldRoute.Status != "active" || newRoute != nil {
		t.Fatalf("replay routes old/new = %#v/%#v", oldRoute, newRoute)
	}
}

func TestMinimalApprovalReplayAfterExpiryDoesNotRearmDelivery(t *testing.T) {
	t.Parallel()

	store := openTestStore(t, securestore.NewDeterministicTestProtector())
	ctx := context.Background()
	created := seedLogicalApproval(t, store, "req-expired-replay", "thr-1", "turn-1", "item/commandExecution/requestApproval", "session-a", "11111111111111111111111111111111", "22222222222222222222222222222222", "event-expired-replay")
	if !created {
		t.Fatal("first seed was duplicate")
	}
	items, err := store.ClaimDeliveryBatch(ctx, 1)
	if err != nil || len(items) != 1 {
		t.Fatalf("ClaimDeliveryBatch = %#v, %v", items, err)
	}
	if err := store.FailDelivery(ctx, items[0].ID, 1, time.Now().UTC(), "definite send failure", true); err != nil {
		t.Fatalf("dead-letter delivery failed: %v", err)
	}
	if changed, err := store.ExpireMinimalApproval(ctx, "req-expired-replay"); err != nil || !changed {
		t.Fatalf("ExpireMinimalApproval = %t, %v", changed, err)
	}

	created, err = store.CreateMinimalApproval(ctx, MinimalApprovalSeed{
		Approval:     MinimalApproval{RequestID: "req-expired-replay", ThreadID: "thr-1", TurnID: "turn-1", RequestKind: "item/commandExecution/requestApproval", ProjectName: "Bridge", SessionIdentity: "session-b", Status: "pending"},
		ApproveToken: "33333333333333333333333333333333",
		DenyToken:    "44444444444444444444444444444444",
		Delivery:     model.DeliveryQueueItem{EventID: "event-expired-replay", ChatKey: model.ChatKey(7, 0), ChatID: 7, TopicID: 0, ThreadID: "thr-1", Kind: "minimal_approval", Status: model.DeliveryStatusPending, PayloadJSON: MustJSON(model.DeliveryPayload{Text: "replayed approval"})},
	})
	if err != nil {
		t.Fatalf("expired exact replay returned error: %v", err)
	}
	if created {
		t.Fatal("expired exact replay created a replacement approval")
	}
	var activeQueue int
	if err := store.db.QueryRowContext(ctx, `SELECT count(*) FROM delivery_queue WHERE id=? AND kind='minimal_approval' AND status IN ('pending','retry','processing')`, items[0].ID).Scan(&activeQueue); err != nil {
		t.Fatal(err)
	}
	if activeQueue != 0 {
		t.Fatalf("expired replay active delivery rows = %d, want 0", activeQueue)
	}
	if newRoute, _ := store.GetMinimalApprovalRoute(ctx, "33333333333333333333333333333333"); newRoute != nil {
		t.Fatalf("expired replay inserted route: %#v", newRoute)
	}
}

func TestMinimalApprovalTransactionsRollBackWhenSiblingRouteIsMissing(t *testing.T) {
	ctx := context.Background()

	t.Run("seed", func(t *testing.T) {
		store := openTestStore(t, securestore.NewDeterministicTestProtector())
		seedLogicalApproval(t, store, "req-existing-route", "thr-1", "turn-1", "item/commandExecution/requestApproval", "session-a", "11111111111111111111111111111111", "22222222222222222222222222222222", "event-existing-route")
		created, err := store.CreateMinimalApproval(ctx, MinimalApprovalSeed{
			Approval:     MinimalApproval{RequestID: "req-seed-corrupt", ThreadID: "thr-1", TurnID: "turn-2", RequestKind: "item/commandExecution/requestApproval", ProjectName: "Bridge", SessionIdentity: "session-a", Status: "pending"},
			ApproveToken: "33333333333333333333333333333333",
			DenyToken:    "11111111111111111111111111111111",
			Delivery:     model.DeliveryQueueItem{EventID: "event-seed-corrupt", ChatKey: model.ChatKey(7, 0), ChatID: 7, ThreadID: "thr-1", Kind: "minimal_approval", Status: model.DeliveryStatusPending, PayloadJSON: MustJSON(model.DeliveryPayload{Text: "approval"})},
		})
		if err == nil || created {
			t.Fatalf("seed with colliding sibling = %t, %v", created, err)
		}
		approval, _ := store.GetMinimalApproval(ctx, "req-seed-corrupt")
		newRoute, _ := store.GetMinimalApprovalRoute(ctx, "33333333333333333333333333333333")
		if approval != nil || newRoute != nil {
			t.Fatalf("failed seed partially committed = %#v/%#v", approval, newRoute)
		}
		if backlog, err := store.DeliveryQueueBacklog(ctx); err != nil || backlog != 1 {
			t.Fatalf("failed seed delivery backlog = %d, %v", backlog, err)
		}
	})

	t.Run("delivery bind", func(t *testing.T) {
		store := seedStorageMinimalApproval(t, "req-bind-corrupt")
		items, err := store.ClaimDeliveryBatch(ctx, 1)
		if err != nil || len(items) != 1 {
			t.Fatalf("ClaimDeliveryBatch = %#v, %v", items, err)
		}
		deleteMinimalApprovalRoute(t, store, "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb")
		if err := store.CompleteMinimalApprovalDelivery(ctx, items[0].ID, "req-bind-corrupt", 7, 0, 600); err == nil {
			t.Fatal("delivery bind succeeded with one sibling route")
		}
		approval, _ := store.GetMinimalApproval(ctx, "req-bind-corrupt")
		if approval == nil || approval.TelegramMessageID != 0 {
			t.Fatalf("failed bind partially committed approval = %#v", approval)
		}
		assertMinimalApprovalRouteStatus(t, store, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "active")
	})

	t.Run("request replacement", func(t *testing.T) {
		store := seedStorageMinimalApproval(t, "req-replace-corrupt")
		completeStorageMinimalApprovalDelivery(t, store, "req-replace-corrupt", 600)
		deleteMinimalApprovalRoute(t, store, "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb")
		created, err := store.CreateMinimalApproval(ctx, MinimalApprovalSeed{
			Approval:     MinimalApproval{RequestID: "req-replace-corrupt", ThreadID: "thr-2", TurnID: "turn-1", RequestKind: "item/commandExecution/requestApproval", ProjectName: "Bridge", SessionIdentity: "session-a", Status: "pending"},
			ApproveToken: "33333333333333333333333333333333",
			DenyToken:    "44444444444444444444444444444444",
			Delivery:     model.DeliveryQueueItem{EventID: "event-replace-corrupt", ChatKey: model.ChatKey(7, 0), ChatID: 7, ThreadID: "thr-2", Kind: "minimal_approval", Status: model.DeliveryStatusPending, PayloadJSON: MustJSON(model.DeliveryPayload{Text: "replacement"})},
		})
		if err == nil || created {
			t.Fatalf("replacement with one sibling = %t, %v", created, err)
		}
		approval, _ := store.GetMinimalApproval(ctx, "req-replace-corrupt")
		if approval == nil || approval.ThreadID != "thr-1" || approval.Status != "pending" || approval.TelegramMessageID != 600 {
			t.Fatalf("failed replacement partially committed approval = %#v", approval)
		}
		assertMinimalApprovalRouteStatus(t, store, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "active")
		if route, _ := store.GetMinimalApprovalRoute(ctx, "33333333333333333333333333333333"); route != nil {
			t.Fatalf("failed replacement created route = %#v", route)
		}
		if backlog, err := store.DeliveryQueueBacklog(ctx); err != nil || backlog != 0 {
			t.Fatalf("failed replacement backlog = %d, %v", backlog, err)
		}
	})

	t.Run("claim", func(t *testing.T) {
		store := seedStorageMinimalApproval(t, "req-claim-corrupt")
		completeStorageMinimalApprovalDelivery(t, store, "req-claim-corrupt", 600)
		deleteMinimalApprovalRoute(t, store, "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb")
		if _, claimed, err := store.ClaimMinimalApproval(ctx, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", 7, 0, 600, "session-a"); err == nil || claimed {
			t.Fatalf("claim with one sibling = %t, %v", claimed, err)
		}
		approval, _ := store.GetMinimalApproval(ctx, "req-claim-corrupt")
		if approval == nil || approval.ClaimState != "idle" {
			t.Fatalf("failed claim partially committed approval = %#v", approval)
		}
		assertMinimalApprovalRouteStatus(t, store, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "active")
	})

	t.Run("restore", func(t *testing.T) {
		store := seedStorageMinimalApproval(t, "req-restore-corrupt")
		completeStorageMinimalApprovalDelivery(t, store, "req-restore-corrupt", 600)
		if _, claimed, err := store.ClaimMinimalApproval(ctx, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", 7, 0, 600, "session-a"); err != nil || !claimed {
			t.Fatalf("initial claim = %t, %v", claimed, err)
		}
		deleteMinimalApprovalRoute(t, store, "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb")
		if restored, err := store.RestoreMinimalApprovalClaim(ctx, "req-restore-corrupt", "approve"); err == nil || restored {
			t.Fatalf("restore with one sibling = %t, %v", restored, err)
		}
		approval, _ := store.GetMinimalApproval(ctx, "req-restore-corrupt")
		if approval == nil || approval.ClaimState != "claimed" {
			t.Fatalf("failed restore partially committed approval = %#v", approval)
		}
		assertMinimalApprovalRouteStatus(t, store, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "claimed")
	})

	t.Run("finish", func(t *testing.T) {
		store := seedStorageMinimalApproval(t, "req-finish-corrupt")
		completeStorageMinimalApprovalDelivery(t, store, "req-finish-corrupt", 600)
		if _, claimed, err := store.ClaimMinimalApproval(ctx, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", 7, 0, 600, "session-a"); err != nil || !claimed {
			t.Fatalf("initial claim = %t, %v", claimed, err)
		}
		deleteMinimalApprovalRoute(t, store, "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb")
		if finished, err := store.FinishMinimalApprovalClaim(ctx, "req-finish-corrupt", "approve", "approved"); err == nil || finished {
			t.Fatalf("finish with one sibling = %t, %v", finished, err)
		}
		approval, _ := store.GetMinimalApproval(ctx, "req-finish-corrupt")
		if approval == nil || approval.Status != "pending" || approval.ClaimState != "claimed" {
			t.Fatalf("failed finish partially committed approval = %#v", approval)
		}
		assertMinimalApprovalRouteStatus(t, store, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "claimed")
	})

	t.Run("expiration", func(t *testing.T) {
		store := seedStorageMinimalApproval(t, "req-expire-corrupt")
		completeStorageMinimalApprovalDelivery(t, store, "req-expire-corrupt", 600)
		deleteMinimalApprovalRoute(t, store, "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb")
		if expired, err := store.ExpireMinimalApproval(ctx, "req-expire-corrupt"); err == nil || expired {
			t.Fatalf("expiration with one sibling = %t, %v", expired, err)
		}
		approval, _ := store.GetMinimalApproval(ctx, "req-expire-corrupt")
		if approval == nil || approval.Status != "pending" {
			t.Fatalf("failed expiration partially committed approval = %#v", approval)
		}
		assertMinimalApprovalRouteStatus(t, store, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "active")
	})

	t.Run("cancellation", func(t *testing.T) {
		store := seedStorageMinimalApproval(t, "req-cancel-corrupt")
		completeStorageMinimalApprovalDelivery(t, store, "req-cancel-corrupt", 600)
		if _, claimed, err := store.ClaimMinimalApproval(ctx, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", 7, 0, 600, "session-a"); err != nil || !claimed {
			t.Fatalf("initial claim = %t, %v", claimed, err)
		}
		deleteMinimalApprovalRoute(t, store, "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb")
		if cancelled, err := store.CancelMinimalApprovalClaimWithInactiveEdit(ctx, "req-cancel-corrupt", "approve", "승인됨"); err == nil || cancelled {
			t.Fatalf("cancellation with one sibling = %t, %v", cancelled, err)
		}
		approval, _ := store.GetMinimalApproval(ctx, "req-cancel-corrupt")
		if approval == nil || approval.Status != "pending" || approval.ClaimState != "claimed" {
			t.Fatalf("failed cancellation partially committed approval = %#v", approval)
		}
		assertMinimalApprovalRouteStatus(t, store, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "claimed")
		if backlog, err := store.DeliveryQueueBacklog(ctx); err != nil || backlog != 0 {
			t.Fatalf("failed cancellation backlog = %d, %v", backlog, err)
		}
	})

	t.Run("recovery", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "state.sqlite")
		protector := securestore.NewDeterministicTestProtector()
		store, err := OpenWithProtector(path, protector)
		if err != nil {
			t.Fatal(err)
		}
		seedLogicalApproval(t, store, "req-recover-corrupt", "thr-1", "turn-1", "item/commandExecution/requestApproval", "session-a", "11111111111111111111111111111111", "22222222222222222222222222222222", "event-recover-corrupt")
		completeStorageMinimalApprovalDelivery(t, store, "req-recover-corrupt", 600)
		if _, claimed, err := store.ClaimMinimalApproval(ctx, "11111111111111111111111111111111", 7, 0, 600, "session-a"); err != nil || !claimed {
			t.Fatalf("initial claim = %t, %v", claimed, err)
		}
		deleteMinimalApprovalRoute(t, store, "22222222222222222222222222222222")
		if err := store.Close(); err != nil {
			t.Fatal(err)
		}
		store, err = OpenWithProtector(path, protector)
		if err != nil {
			t.Fatal(err)
		}
		defer store.Close()
		if recovered, err := store.RecoverMinimalApprovalClaims(ctx); err == nil || recovered != 0 {
			t.Fatalf("recovery with one sibling = %d, %v", recovered, err)
		}
		approval, _ := store.GetMinimalApproval(ctx, "req-recover-corrupt")
		if approval == nil || approval.Status != "pending" || approval.ClaimState != "claimed" {
			t.Fatalf("failed recovery partially committed approval = %#v", approval)
		}
		assertMinimalApprovalRouteStatus(t, store, "11111111111111111111111111111111", "claimed")
		if backlog, err := store.DeliveryQueueBacklog(ctx); err != nil || backlog != 0 {
			t.Fatalf("failed recovery backlog = %d, %v", backlog, err)
		}
	})
}

func TestMinimalApprovalReplacementQueuesOldKeyboardRemovalAcrossRestartAndReplay(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.sqlite")
	protector := securestore.NewDeterministicTestProtector()
	store, err := OpenWithProtector(path, protector)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	seedLogicalApproval(t, store, "req-replace-edit", "thr-1", "turn-1", "item/commandExecution/requestApproval", "session-a", "11111111111111111111111111111111", "22222222222222222222222222222222", "event-replace-edit-old")
	completeStorageMinimalApprovalDelivery(t, store, "req-replace-edit", 600)
	if created := seedLogicalApproval(t, store, "req-replace-edit", "thr-2", "turn-1", "item/commandExecution/requestApproval", "session-a", "33333333333333333333333333333333", "44444444444444444444444444444444", "event-replace-edit-new"); !created {
		t.Fatal("mismatched logical request was not created")
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = OpenWithProtector(path, protector)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if created := seedLogicalApproval(t, store, "req-replace-edit", "thr-2", "turn-1", "item/commandExecution/requestApproval", "session-b", "55555555555555555555555555555555", "66666666666666666666666666666666", "event-replace-edit-replay"); created {
		t.Fatal("exact replay recreated replacement presentation")
	}
	items, err := store.ClaimDeliveryBatch(ctx, 10)
	if err != nil || len(items) != 2 {
		t.Fatalf("replacement deliveries = %#v, %v", items, err)
	}
	if items[0].Kind != "minimal_approval_inactive_edit" || items[1].Kind != "minimal_approval" {
		t.Fatalf("replacement delivery order/kinds = %q/%q", items[0].Kind, items[1].Kind)
	}
	var inactive model.DeliveryPayload
	if err := json.Unmarshal([]byte(items[0].PayloadJSON), &inactive); err != nil {
		t.Fatal(err)
	}
	if inactive.Mode != "edit" || inactive.MessageID != 600 || inactive.Text != "요청이 더 이상 활성 상태가 아닙니다." || len(inactive.Buttons) != 0 {
		t.Fatalf("replacement inactive edit = %#v", inactive)
	}
	oldRoute, _ := store.GetMinimalApprovalRoute(ctx, "11111111111111111111111111111111")
	approval, _ := store.GetMinimalApproval(ctx, "req-replace-edit")
	if oldRoute == nil || oldRoute.Status != "expired" || approval == nil || approval.ThreadID != "thr-2" || approval.SessionIdentity != "session-b" || approval.TelegramMessageID != 0 {
		t.Fatalf("replacement state old/new = %#v/%#v", oldRoute, approval)
	}
}

func TestMinimalApprovalReplacementEnsuresInactiveEditForCancelledOldBinding(t *testing.T) {
	for _, tc := range []struct {
		name          string
		durableCancel bool
	}{
		{name: "cancelled without queued edit"},
		{name: "cancelled with existing queued edit", durableCancel: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := seedStorageMinimalApproval(t, "req-replace-cancelled")
			ctx := context.Background()
			completeStorageMinimalApprovalDelivery(t, store, "req-replace-cancelled", 600)
			if _, claimed, err := store.ClaimMinimalApproval(ctx, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", 7, 0, 600, "session-a"); err != nil || !claimed {
				t.Fatalf("claim = %t, %v", claimed, err)
			}
			if tc.durableCancel {
				if changed, err := store.CancelMinimalApprovalClaimWithInactiveEdit(ctx, "req-replace-cancelled", "approve", "승인됨"); err != nil || !changed {
					t.Fatalf("durable cancellation = %t, %v", changed, err)
				}
			} else if changed, err := store.FinishMinimalApprovalClaim(ctx, "req-replace-cancelled", "approve", "cancelled"); err != nil || !changed {
				t.Fatalf("plain cancellation = %t, %v", changed, err)
			}
			created := seedLogicalApproval(t, store, "req-replace-cancelled", "thr-2", "turn-1", "item/commandExecution/requestApproval", "session-a", "33333333333333333333333333333333", "44444444444444444444444444444444", "event-replace-cancelled-new")
			if !created {
				t.Fatal("replacement was not created")
			}
			items, err := store.ClaimDeliveryBatch(ctx, 10)
			if err != nil || len(items) != 2 || items[0].Kind != "minimal_approval_inactive_edit" || items[1].Kind != "minimal_approval" {
				t.Fatalf("cancelled replacement deliveries = %#v, %v", items, err)
			}
		})
	}
}

func TestRecoverMinimalApprovalClaimsDurablyQueuesKeyboardRemoval(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.sqlite")
	protector := securestore.NewDeterministicTestProtector()
	store, err := OpenWithProtector(path, protector)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	seedLogicalApproval(t, store, "req-recover-edit", "thr-1", "turn-1", "item/commandExecution/requestApproval", "session-a", "11111111111111111111111111111111", "22222222222222222222222222222222", "event-recover-edit")
	completeStorageMinimalApprovalDelivery(t, store, "req-recover-edit", 600)
	if _, claimed, err := store.ClaimMinimalApproval(ctx, "11111111111111111111111111111111", 7, 0, 600, "session-a"); err != nil || !claimed {
		t.Fatalf("claim before crash = %t, %v", claimed, err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = OpenWithProtector(path, protector)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	recovered, err := store.RecoverMinimalApprovalClaims(ctx)
	if err != nil || recovered != 1 {
		t.Fatalf("RecoverMinimalApprovalClaims = %d, %v", recovered, err)
	}
	approval, _ := store.GetMinimalApproval(ctx, "req-recover-edit")
	if approval == nil || approval.Status != "cancelled" || approval.ClaimState != "idle" {
		t.Fatalf("recovered approval = %#v", approval)
	}
	items, err := store.ClaimDeliveryBatch(ctx, 10)
	if err != nil || len(items) != 1 || items[0].Kind != "minimal_approval_inactive_edit" {
		t.Fatalf("recovery inactive edit = %#v, %v", items, err)
	}
	var payload model.DeliveryPayload
	if err := json.Unmarshal([]byte(items[0].PayloadJSON), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Mode != "edit" || payload.MessageID != 600 || payload.Text != "처리 결과를 확인할 수 없어 버튼이 비활성화되었습니다." || len(payload.Buttons) != 0 {
		t.Fatalf("recovery inactive payload = %#v", payload)
	}
}

func completeStorageMinimalApprovalDelivery(t *testing.T, store *Store, requestID string, messageID int64) {
	t.Helper()
	items, err := store.ClaimDeliveryBatch(context.Background(), 1)
	if err != nil || len(items) != 1 {
		t.Fatalf("ClaimDeliveryBatch = %#v, %v", items, err)
	}
	if err := store.CompleteMinimalApprovalDelivery(context.Background(), items[0].ID, requestID, 7, 0, messageID); err != nil {
		t.Fatal(err)
	}
}

func deleteMinimalApprovalRoute(t *testing.T, store *Store, token string) {
	t.Helper()
	if _, err := store.db.Exec(`DELETE FROM minimal_approval_routes WHERE route_token=?`, token); err != nil {
		t.Fatal(err)
	}
}

func assertMinimalApprovalRouteStatus(t *testing.T, store *Store, token, want string) {
	t.Helper()
	route, err := store.GetMinimalApprovalRoute(context.Background(), token)
	if err != nil || route == nil || route.Status != want {
		t.Fatalf("route %q = %#v, %v; want status %q", token, route, err, want)
	}
}

func seedLogicalApproval(t *testing.T, store *Store, requestID, threadID, turnID, kind, sessionID, approveToken, denyToken, eventID string) bool {
	t.Helper()
	created, err := store.CreateMinimalApproval(context.Background(), MinimalApprovalSeed{
		Approval:     MinimalApproval{RequestID: requestID, ThreadID: threadID, TurnID: turnID, RequestKind: kind, ProjectName: "Bridge", SessionIdentity: sessionID, Status: "pending"},
		ApproveToken: approveToken,
		DenyToken:    denyToken,
		Delivery:     model.DeliveryQueueItem{EventID: eventID, ChatKey: model.ChatKey(7, 0), ChatID: 7, TopicID: 0, ThreadID: threadID, Kind: "minimal_approval", Status: model.DeliveryStatusPending, PayloadJSON: MustJSON(model.DeliveryPayload{Text: "approval"})},
	})
	if err != nil {
		t.Fatal(err)
	}
	return created
}
