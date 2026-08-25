package storage

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alclssna33/codex_to_telegram/internal/model"
	"github.com/alclssna33/codex_to_telegram/internal/securestore"
)

func TestVoiceConfirmationEncryptsTranscriptAndCreatesExactlyTwoOpaqueRoutes(t *testing.T) {
	t.Parallel()

	dbPath := filepath.Join(t.TempDir(), "state.sqlite")
	store, err := OpenWithProtector(dbPath, securestore.NewDeterministicTestProtector())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	ctx := context.Background()
	const transcript = "PRIVATE_VOICE_TRANSCRIPT_19e7ab"
	confirmation := model.VoiceConfirmation{
		ProjectID: "p1", TargetKind: model.VoiceTargetNew, Transcript: transcript,
		SessionIdentity: "session-a", ExpiresAt: model.TimeString(time.Now().Add(time.Minute).UTC().Format(time.RFC3339Nano)),
	}
	executeToken := strings.Repeat("a1", 16)
	cancelToken := strings.Repeat("b2", 16)
	id, err := store.CreateVoiceConfirmation(ctx, confirmation, executeToken, cancelToken)
	if err != nil {
		t.Fatal(err)
	}
	if id == 0 {
		t.Fatal("voice confirmation id is zero")
	}
	var protected string
	if err := store.db.QueryRowContext(ctx, `SELECT transcript_payload FROM voice_confirmations WHERE id = ?`, id).Scan(&protected); err != nil {
		t.Fatal(err)
	}
	if protected == transcript || !strings.HasPrefix(protected, "dpapi:v1:") {
		t.Fatalf("stored transcript = %q, want protected envelope", protected)
	}
	rows, err := store.db.QueryContext(ctx, `SELECT route_token, action FROM voice_callback_routes WHERE voice_id = ? ORDER BY action`, id)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var routes int
	for rows.Next() {
		var token, action string
		if err := rows.Scan(&token, &action); err != nil {
			t.Fatal(err)
		}
		if len(token) != 32 {
			t.Fatalf("token length = %d, want 32", len(token))
		}
		if _, err := hex.DecodeString(token); err != nil {
			t.Fatalf("token is not hex: %v", err)
		}
		if action != model.VoiceActionCancel && action != model.VoiceActionExecute {
			t.Fatalf("action = %q", action)
		}
		routes++
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if routes != 2 {
		t.Fatalf("voice callback routes = %d, want 2", routes)
	}
	if _, err := store.db.ExecContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{dbPath, dbPath + "-wal", dbPath + "-shm"} {
		data, err := os.ReadFile(path)
		if err != nil && os.IsNotExist(err) {
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(data, []byte(transcript)) {
			t.Fatalf("plaintext transcript found in %s", filepath.Base(path))
		}
	}
}

func TestMinimalSchemaAddsVoiceSourceTurnIDToExistingConfirmations(t *testing.T) {
	t.Parallel()

	store, err := Open(filepath.Join(t.TempDir(), "state.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	ctx := context.Background()
	if _, err := store.db.ExecContext(ctx, `
	CREATE TABLE voice_confirmations (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		project_id TEXT NOT NULL,
		target_kind TEXT NOT NULL,
		thread_id TEXT,
		transcript_payload TEXT,
		session_identity TEXT NOT NULL,
		status TEXT NOT NULL,
		expires_at TEXT NOT NULL,
		chat_id INTEGER NOT NULL DEFAULT 0,
		topic_id INTEGER NOT NULL DEFAULT 0,
		telegram_message_id INTEGER NOT NULL DEFAULT 0,
		created_at TEXT NOT NULL
	);
	INSERT INTO voice_confirmations(project_id, target_kind, thread_id, transcript_payload, session_identity, status, expires_at, created_at)
	VALUES ('p1', 'thread', 'parent', 'protected', 'session-a', 'pending', '2099-01-01T00:00:00Z', '2026-08-22T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}

	if err := store.EnsureMinimalSchema(ctx); err != nil {
		t.Fatal(err)
	}

	var sourceTurnID string
	if err := store.db.QueryRowContext(ctx, `SELECT source_turn_id FROM voice_confirmations WHERE id = 1`).Scan(&sourceTurnID); err != nil {
		t.Fatal(err)
	}
	if sourceTurnID != "" {
		t.Fatalf("migrated source_turn_id = %q, want empty default", sourceTurnID)
	}
}

func TestVoiceConfirmationRequiresExactPreviewBindingAndConsumesSiblings(t *testing.T) {
	t.Parallel()

	store := newProtectedMinimalStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	executeToken := strings.Repeat("c3", 16)
	cancelToken := strings.Repeat("d4", 16)
	id, err := store.CreateVoiceConfirmation(ctx, model.VoiceConfirmation{
		ProjectID: "p1", TargetKind: model.VoiceTargetThread, ThreadID: "thread-frozen", Transcript: "execute me",
		SourceTurnID:    "turn-frozen",
		SessionIdentity: "session-a", ExpiresAt: model.TimeString(now.Add(time.Minute).Format(time.RFC3339Nano)),
	}, executeToken, cancelToken)
	if err != nil {
		t.Fatal(err)
	}
	claim := model.VoiceClaim{Token: executeToken, Action: model.VoiceActionExecute, SessionIdentity: "session-a", ChatID: 7, TopicID: 9, MessageID: 11, Now: now}
	if got, err := store.ConsumeVoiceConfirmation(ctx, claim); err != nil || got != nil {
		t.Fatalf("unbound claim = %#v, %v; want stale", got, err)
	}
	if err := store.BindVoiceConfirmation(ctx, id, 7, 9, 11); err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(*model.VoiceClaim){
		func(c *model.VoiceClaim) { c.ChatID++ },
		func(c *model.VoiceClaim) { c.TopicID++ },
		func(c *model.VoiceClaim) { c.MessageID++ },
		func(c *model.VoiceClaim) { c.SessionIdentity = "restarted-session" },
	} {
		wrong := claim
		mutate(&wrong)
		if got, err := store.ConsumeVoiceConfirmation(ctx, wrong); err != nil || got != nil {
			t.Fatalf("wrong binding claim = %#v, %v; want stale", got, err)
		}
	}
	got, err := store.ConsumeVoiceConfirmation(ctx, claim)
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || got.Transcript != "execute me" || got.ProjectID != "p1" || got.ThreadID != "thread-frozen" || got.SourceTurnID != "turn-frozen" || got.TargetKind != model.VoiceTargetThread {
		t.Fatalf("claimed voice = %#v", got)
	}
	if second, err := store.ConsumeVoiceConfirmation(ctx, claim); err != nil || second != nil {
		t.Fatalf("double execute = %#v, %v; want stale", second, err)
	}
	if sibling, err := store.ConsumeVoiceConfirmation(ctx, model.VoiceClaim{Token: cancelToken, Action: model.VoiceActionCancel, SessionIdentity: "session-a", ChatID: 7, TopicID: 9, MessageID: 11, Now: now}); err != nil || sibling != nil {
		t.Fatalf("sibling claim = %#v, %v; want stale", sibling, err)
	}
	var payload any
	var status string
	if err := store.db.QueryRowContext(ctx, `SELECT transcript_payload, status FROM voice_confirmations WHERE id = ?`, id).Scan(&payload, &status); err != nil {
		t.Fatal(err)
	}
	if payload != nil || status != model.VoiceStatusExecuted {
		t.Fatalf("terminal payload=%#v status=%q", payload, status)
	}
}

func TestVoiceConfirmationCancelExpiryMissingSiblingAndRestartFailClosed(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name       string
		action     string
		nowOffset  time.Duration
		deletePeer bool
		recover    bool
		wantStatus string
	}{
		{name: "cancel", action: model.VoiceActionCancel, wantStatus: model.VoiceStatusCancelled},
		{name: "expired", action: model.VoiceActionExecute, nowOffset: 2 * time.Minute, wantStatus: model.VoiceStatusExpired},
		{name: "missing sibling", action: model.VoiceActionExecute, deletePeer: true, wantStatus: model.VoiceStatusExpired},
		{name: "restart recovery", action: model.VoiceActionExecute, recover: true, wantStatus: model.VoiceStatusExpired},
	} {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			store := newProtectedMinimalStore(t)
			ctx := context.Background()
			now := time.Now().UTC()
			executeToken := strings.Repeat("e5", 16)
			cancelToken := strings.Repeat("f6", 16)
			id, err := store.CreateVoiceConfirmation(ctx, model.VoiceConfirmation{
				ProjectID: "p1", TargetKind: model.VoiceTargetNew, Transcript: "must disappear",
				SessionIdentity: "session-a", ExpiresAt: model.TimeString(now.Add(time.Minute).Format(time.RFC3339Nano)),
			}, executeToken, cancelToken)
			if err != nil {
				t.Fatal(err)
			}
			if err := store.BindVoiceConfirmation(ctx, id, 7, 0, 11); err != nil {
				t.Fatal(err)
			}
			if test.deletePeer {
				if _, err := store.db.ExecContext(ctx, `DELETE FROM voice_callback_routes WHERE route_token = ?`, cancelToken); err != nil {
					t.Fatal(err)
				}
			}
			if test.recover {
				if recovered, err := store.RecoverVoiceConfirmations(ctx); err != nil || recovered != 1 {
					t.Fatalf("RecoverVoiceConfirmations = %d, %v", recovered, err)
				}
			} else {
				token := executeToken
				if test.action == model.VoiceActionCancel {
					token = cancelToken
				}
				got, err := store.ConsumeVoiceConfirmation(ctx, model.VoiceClaim{
					Token: token, Action: test.action, SessionIdentity: "session-a", ChatID: 7, MessageID: 11, Now: now.Add(test.nowOffset),
				})
				if err != nil {
					t.Fatal(err)
				}
				if test.action == model.VoiceActionCancel && got == nil {
					t.Fatal("cancel did not win")
				}
				if test.action == model.VoiceActionExecute && got != nil {
					t.Fatalf("execute unexpectedly claimed %#v", got)
				}
			}
			var payload any
			var status string
			if err := store.db.QueryRowContext(ctx, `SELECT transcript_payload, status FROM voice_confirmations WHERE id = ?`, id).Scan(&payload, &status); err != nil {
				t.Fatal(err)
			}
			if payload != nil || status != test.wantStatus {
				t.Fatalf("terminal payload=%#v status=%q, want %q", payload, status, test.wantStatus)
			}
		})
	}
}

func TestVoiceConfirmationExecuteAndCancelHaveOneAtomicWinner(t *testing.T) {
	t.Parallel()

	store := newProtectedMinimalStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	executeToken := strings.Repeat("17", 16)
	cancelToken := strings.Repeat("28", 16)
	id, err := store.CreateVoiceConfirmation(ctx, model.VoiceConfirmation{
		ProjectID: "p1", TargetKind: model.VoiceTargetNew, Transcript: "one winner",
		SessionIdentity: "session-a", ExpiresAt: model.TimeString(now.Add(time.Minute).Format(time.RFC3339Nano)),
	}, executeToken, cancelToken)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.BindVoiceConfirmation(ctx, id, 7, 0, 11); err != nil {
		t.Fatal(err)
	}
	claims := []model.VoiceClaim{
		{Token: executeToken, Action: model.VoiceActionExecute, SessionIdentity: "session-a", ChatID: 7, MessageID: 11, Now: now},
		{Token: cancelToken, Action: model.VoiceActionCancel, SessionIdentity: "session-a", ChatID: 7, MessageID: 11, Now: now},
	}
	results := make(chan *model.VoiceConfirmation, len(claims))
	errs := make(chan error, len(claims))
	var wg sync.WaitGroup
	for _, claim := range claims {
		claim := claim
		wg.Add(1)
		go func() {
			defer wg.Done()
			result, err := store.ConsumeVoiceConfirmation(ctx, claim)
			results <- result
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
		t.Fatalf("winners = %d, want exactly one", winners)
	}
}

func newProtectedMinimalStore(t *testing.T) *Store {
	t.Helper()
	store, err := OpenWithProtector(filepath.Join(t.TempDir(), "state.sqlite"), securestore.NewDeterministicTestProtector())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func TestSelectedProjectStoresOnlyProjectID(t *testing.T) {
	t.Parallel()

	store, err := Open(filepath.Join(t.TempDir(), "state.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	ctx := context.Background()
	if err := store.EnsureMinimalSchema(ctx); err != nil {
		t.Fatal(err)
	}
	if err := store.SetSelectedProject(ctx, 100, 0, "bridge"); err != nil {
		t.Fatal(err)
	}

	projectID, err := store.GetSelectedProject(ctx, 100, 0)
	if err != nil {
		t.Fatal(err)
	}
	if projectID != "bridge" {
		t.Fatalf("project id = %q, want bridge", projectID)
	}

	rows, err := store.db.QueryContext(ctx, `PRAGMA table_info(selected_projects)`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var columns []string
	for rows.Next() {
		var cid, notNull, primaryKey int
		var name, columnType string
		var defaultValue any
		if err := rows.Scan(&cid, &name, &columnType, &notNull, &defaultValue, &primaryKey); err != nil {
			t.Fatal(err)
		}
		columns = append(columns, name)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if want := []string{"chat_key", "project_id", "updated_at"}; !reflect.DeepEqual(columns, want) {
		t.Fatalf("selected_projects columns = %v, want %v", columns, want)
	}
}

func TestSelectedProjectIsScopedByChatKeyAndCanBeReplaced(t *testing.T) {
	t.Parallel()

	store, err := Open(filepath.Join(t.TempDir(), "state.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	ctx := context.Background()
	if err := store.EnsureMinimalSchema(ctx); err != nil {
		t.Fatal(err)
	}
	if err := store.SetSelectedProject(ctx, 100, 9, "first"); err != nil {
		t.Fatal(err)
	}
	if err := store.SetSelectedProject(ctx, 100, 9, "second"); err != nil {
		t.Fatal(err)
	}
	if err := store.SetSelectedProject(ctx, 100, 10, "other-topic"); err != nil {
		t.Fatal(err)
	}

	got, err := store.GetSelectedProject(ctx, 100, 9)
	if err != nil {
		t.Fatal(err)
	}
	if got != "second" {
		t.Fatalf("selected project = %q, want second", got)
	}
	other, err := store.GetSelectedProject(ctx, 100, 10)
	if err != nil {
		t.Fatal(err)
	}
	if other != "other-topic" {
		t.Fatalf("other topic project = %q, want other-topic", other)
	}
}

func TestMinimalPickerRouteConsumeRejectsStaleSelectedProject(t *testing.T) {
	t.Parallel()

	store, err := Open(filepath.Join(t.TempDir(), "state.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	ctx := context.Background()
	now := time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC)
	stale := model.MinimalPickerRoute{
		Token:     "11111111111111111111111111111111",
		Action:    "minimal_existing_select",
		ProjectID: "project-a",
		ThreadID:  "thread-a",
		ChatID:    100,
		TopicID:   9,
		Status:    model.CallbackStatusActive,
		ExpiresAt: model.TimeString(now.Add(time.Minute).Format(time.RFC3339Nano)),
	}
	fresh := model.MinimalPickerRoute{
		Token:     "22222222222222222222222222222222",
		Action:    "minimal_existing_select",
		ProjectID: "project-b",
		ThreadID:  "thread-b",
		ChatID:    100,
		TopicID:   9,
		Status:    model.CallbackStatusActive,
		ExpiresAt: model.TimeString(now.Add(time.Minute).Format(time.RFC3339Nano)),
	}
	if err := store.SetSelectedProject(ctx, 100, 9, "project-a"); err != nil {
		t.Fatal(err)
	}
	if err := store.CreateMinimalPickerRoutes(ctx, []model.MinimalPickerRoute{stale, fresh}); err != nil {
		t.Fatal(err)
	}
	if err := store.SetSelectedProject(ctx, 100, 9, "project-b"); err != nil {
		t.Fatal(err)
	}

	consumed, err := store.ConsumeMinimalPickerRoute(ctx, stale.Token, 100, 9, now)
	if err != nil {
		t.Fatal(err)
	}
	if consumed != nil {
		t.Fatalf("stale route consumed as current = %#v, want nil", consumed)
	}
	var staleStatus string
	if err := store.db.QueryRowContext(ctx, `SELECT status FROM minimal_picker_routes WHERE route_token = ?`, stale.Token).Scan(&staleStatus); err != nil {
		t.Fatal(err)
	}
	if staleStatus != "consumed" {
		t.Fatalf("stale route status = %q, want consumed terminal state", staleStatus)
	}
	current, err := store.ConsumeMinimalPickerRoute(ctx, fresh.Token, 100, 9, now)
	if err != nil {
		t.Fatal(err)
	}
	if current == nil || current.ProjectID != "project-b" || current.ThreadID != "thread-b" {
		t.Fatalf("fresh current route = %#v, want project-b/thread-b", current)
	}
}

func TestRetireMinimalObservationIsIdempotentAndDurable(t *testing.T) {
	t.Parallel()

	dbPath := filepath.Join(t.TempDir(), "state.sqlite")
	store, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	thread := model.Thread{
		ID:           "observed-thread",
		Title:        "observed-thread",
		CWD:          "/registered/project",
		UpdatedAt:    100,
		Status:       "inProgress",
		ActiveTurnID: "turn-1",
	}
	if err := store.UpsertThread(ctx, thread); err != nil {
		t.Fatal(err)
	}
	if err := store.ObserveMinimalThread(ctx, thread, "project-a", time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC)); err != nil {
		t.Fatal(err)
	}
	if due, err := store.ListMinimalObservedThreadsDue(ctx, 10); err != nil || len(due) != 1 || due[0].ID != thread.ID {
		t.Fatalf("initial due = %#v, %v; want observed-thread", due, err)
	}
	now := time.Date(2026, 8, 22, 12, 1, 0, 0, time.UTC)
	if err := store.RetireMinimalObservation(ctx, thread.ID, now); err != nil {
		t.Fatal(err)
	}
	if err := store.RetireMinimalObservation(ctx, thread.ID, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if due, err := store.ListMinimalObservedThreadsDue(ctx, 10); err != nil || len(due) != 0 {
		t.Fatalf("due after retire = %#v, %v; want none", due, err)
	}
	if observed, readRequired, err := store.MinimalObservationReadRequired(ctx, thread.ID); err != nil || !observed || readRequired {
		t.Fatalf("read required after retire = observed:%t readRequired:%t err:%v; want observed retired", observed, readRequired, err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	if due, err := reopened.ListMinimalObservedThreadsDue(ctx, 10); err != nil || len(due) != 0 {
		t.Fatalf("due after restart = %#v, %v; want none", due, err)
	}
}

func TestRetiredMinimalObservationCannotBeRevivedByListObservation(t *testing.T) {
	t.Parallel()

	dbPath := filepath.Join(t.TempDir(), "state.sqlite")
	store, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	ctx := context.Background()
	now := time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC)
	active := model.Thread{
		ID:           "retired-list-thread",
		Title:        "retired-list-thread",
		CWD:          "/registered/project",
		UpdatedAt:    100,
		Status:       "inProgress",
		ActiveTurnID: "turn-1",
	}
	if err := store.UpsertThread(ctx, active); err != nil {
		t.Fatal(err)
	}
	if err := store.ObserveMinimalThread(ctx, active, "project-a", now); err != nil {
		t.Fatal(err)
	}
	if err := store.RetireMinimalObservation(ctx, active.ID, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	staleList := active
	staleList.UpdatedAt = 200
	staleList.Status = "completed"
	staleList.ActiveTurnID = ""
	if err := store.ObserveMinimalThread(ctx, staleList, "project-a", now.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}

	observed, readRequired, err := store.MinimalObservationReadRequired(ctx, active.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !observed || readRequired {
		t.Fatalf("retired list observation = observed:%t readRequired:%t, want observed non-due", observed, readRequired)
	}
	if due, err := store.ListMinimalObservedThreadsDue(ctx, 10); err != nil || len(due) != 0 {
		t.Fatalf("due after stale list observe = %#v, %v; want none", due, err)
	}
}

func TestPendingCommandRequiresProtector(t *testing.T) {
	t.Parallel()
	store, err := Open(filepath.Join(t.TempDir(), "state.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	err = store.EnqueuePendingCommand(context.Background(), model.PendingCommand{ThreadID: "thread-1", ProjectID: "bridge", Prompt: "private prompt"})
	if err == nil {
		t.Fatal("EnqueuePendingCommand succeeded without a protector")
	}
}

func TestMinimalLinkedThreadProtectsTitlesAndTransitionsByGeneration(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "state.sqlite")
	store, err := OpenWithProtector(dbPath, securestore.NewDeterministicTestProtector())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	ctx := context.Background()

	link := model.MinimalLinkedThread{
		ChatID: 7, ProjectID: "p1", SourceThreadID: "source-1",
		SourceAnchorTurnID: "source-turn-1", SourceTitle: "PRIVATE_TITLE_47ac",
		DesiredTitle: "PRIVATE_TITLE_47ac · 텔레그램 연동",
		TitleState:   model.MinimalLinkedTitlePending,
		State:        model.MinimalLinkedTelegramRunning, WorkerGeneration: 11,
	}
	provenance := model.MinimalContinuation{
		Key:       model.MinimalContinuationKey{ChatID: 7, SourceThreadID: "source-1", SourceTurnID: "source-turn-1"},
		ProjectID: "p1", Status: model.MinimalContinuationCreating,
	}
	child := model.Thread{ID: "linked-1", CWD: t.TempDir(), Status: "completed"}
	if err := store.ActivateMinimalLinkedThread(ctx, link, provenance, child); err != nil {
		t.Fatal(err)
	}

	got, err := store.GetMinimalLinkedThread(ctx, 7, 0, "source-1")
	if err != nil {
		t.Fatal(err)
	}
	if got.LinkedThreadID != "linked-1" || got.SourceTitle != link.SourceTitle || got.DesiredTitle != link.DesiredTitle {
		t.Fatalf("link=%#v", got)
	}
	if bytes.Contains(readRawSQLiteFile(t, dbPath), []byte("PRIVATE_TITLE_47ac")) {
		t.Fatal("title leaked")
	}
	if changed, err := store.MarkMinimalLinkedTurnStarted(ctx, "linked-1", 10, "turn-1"); err != nil || changed {
		t.Fatalf("stale turn start changed=%t err=%v, want false nil", changed, err)
	}
	if changed, err := store.MarkMinimalLinkedTurnStarted(ctx, "linked-1", 11, "turn-1"); err != nil || !changed {
		t.Fatalf("turn start changed=%t err=%v, want true nil", changed, err)
	}
	if changed, err := store.MarkMinimalLinkedTurnStarted(ctx, "linked-1", 11, "turn-2"); err != nil || changed {
		t.Fatalf("second turn start changed=%t err=%v, want false nil", changed, err)
	}
	got, err = store.GetMinimalLinkedThreadByLinkedID(ctx, "linked-1")
	if err != nil {
		t.Fatal(err)
	}
	if got.ActiveTurnID != "turn-1" {
		t.Fatalf("active turn = %q, want first turn unchanged", got.ActiveTurnID)
	}
	if changed, err := store.MarkMinimalLinkedTitleSet(ctx, "linked-1", 11); err != nil || !changed {
		t.Fatalf("title set changed=%t err=%v, want true nil", changed, err)
	}
	if changed, err := store.BeginMinimalLinkedRelease(ctx, model.MinimalLinkedRelease{LinkedThreadID: "linked-1", TurnID: "turn-1", WorkerGeneration: 10}); err != nil || changed {
		t.Fatalf("stale release begin changed=%t err=%v, want false nil", changed, err)
	}
	if changed, err := store.BeginMinimalLinkedRelease(ctx, model.MinimalLinkedRelease{LinkedThreadID: "linked-1", TurnID: "turn-1", WorkerGeneration: 11}); err != nil || !changed {
		t.Fatalf("release begin changed=%t err=%v, want true nil", changed, err)
	}
	if changed, err := store.FinishMinimalLinkedRelease(ctx, "linked-1", 10, time.Now()); err != nil || changed {
		t.Fatalf("stale release finish changed=%t err=%v, want false nil", changed, err)
	}
	releasedAt := time.Date(2026, 8, 23, 1, 2, 3, 0, time.UTC)
	if changed, err := store.FinishMinimalLinkedRelease(ctx, "linked-1", 11, releasedAt); err != nil || !changed {
		t.Fatalf("release finish changed=%t err=%v, want true nil", changed, err)
	}

	got, err = store.GetMinimalLinkedThreadByLinkedID(ctx, "linked-1")
	if err != nil {
		t.Fatal(err)
	}
	if got.State != model.MinimalLinkedReady || got.ActiveTurnID != "" || got.TitleState != model.MinimalLinkedTitleSet || got.ReleasedAt != model.TimeString(releasedAt.Format(time.RFC3339Nano)) {
		t.Fatalf("released link=%#v", got)
	}
	if changed, err := store.ClaimMinimalLinkedWorker(ctx, "linked-1", 12); err != nil || !changed {
		t.Fatalf("claim worker changed=%t err=%v, want true nil", changed, err)
	}
	got, err = store.GetMinimalLinkedThreadByLinkedID(ctx, "linked-1")
	if err != nil {
		t.Fatal(err)
	}
	if got.State != model.MinimalLinkedTelegramRunning || got.WorkerGeneration != 12 {
		t.Fatalf("reclaimed link=%#v", got)
	}
}

func TestMinimalLinkedTitleHydrationFillsLegacyPayloadsWithoutOverwrite(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "state.sqlite")
	store, err := OpenWithProtector(dbPath, securestore.NewDeterministicTestProtector())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	ctx := context.Background()

	link := model.MinimalLinkedThread{
		ChatID:             7,
		ProjectID:          "p1",
		SourceThreadID:     "source-legacy-title",
		SourceAnchorTurnID: "source-turn-1",
		TitleState:         model.MinimalLinkedTitlePending,
		State:              model.MinimalLinkedTelegramRunning,
		WorkerGeneration:   3,
	}
	provenance := model.MinimalContinuation{
		Key:       model.MinimalContinuationKey{ChatID: 7, SourceThreadID: "source-legacy-title", SourceTurnID: "source-turn-1"},
		ProjectID: "p1",
		Status:    model.MinimalContinuationCreating,
	}
	if err := store.ActivateMinimalLinkedThread(ctx, link, provenance, model.Thread{ID: "linked-legacy-title", CWD: "/tmp/project"}); err != nil {
		t.Fatal(err)
	}

	changed, err := store.HydrateMinimalLinkedTitles(ctx, "linked-legacy-title", "PRIVATE_LEGACY_TITLE_6c91", "PRIVATE_LEGACY_TITLE_6c91 · 텔레그램 연동")
	if err != nil || !changed {
		t.Fatalf("hydrate changed=%t err=%v, want true nil", changed, err)
	}
	got, err := store.GetMinimalLinkedThreadByLinkedID(ctx, "linked-legacy-title")
	if err != nil {
		t.Fatal(err)
	}
	if got.SourceTitle != "PRIVATE_LEGACY_TITLE_6c91" || got.DesiredTitle != "PRIVATE_LEGACY_TITLE_6c91 · 텔레그램 연동" || got.TitleState != model.MinimalLinkedTitlePending {
		t.Fatalf("hydrated link=%#v", got)
	}
	if bytes.Contains(readRawSQLiteFile(t, dbPath), []byte("PRIVATE_LEGACY_TITLE_6c91")) {
		t.Fatal("hydrated title leaked")
	}

	changed, err = store.HydrateMinimalLinkedTitles(ctx, "linked-legacy-title", "PRIVATE_REPLACEMENT_TITLE_8d10", "PRIVATE_REPLACEMENT_TITLE_8d10 · 텔레그램 연동")
	if err != nil || changed {
		t.Fatalf("second hydrate changed=%t err=%v, want false nil", changed, err)
	}
	got, err = store.GetMinimalLinkedThreadByLinkedID(ctx, "linked-legacy-title")
	if err != nil {
		t.Fatal(err)
	}
	if got.SourceTitle != "PRIVATE_LEGACY_TITLE_6c91" || got.DesiredTitle != "PRIVATE_LEGACY_TITLE_6c91 · 텔레그램 연동" {
		t.Fatalf("second hydrate overwrote title: %#v", got)
	}
}

func TestMinimalLinkedWorkerClaimRequiresNewerNonzeroGeneration(t *testing.T) {
	t.Parallel()

	store := newProtectedMinimalStore(t)
	ctx := context.Background()
	releasedAt := time.Date(2026, 8, 23, 2, 0, 0, 0, time.UTC)
	activateReleasedMinimalLinkedThread(t, store, "linked-generation", 5, releasedAt)

	if changed, err := store.ClaimMinimalLinkedWorker(ctx, "linked-generation", 0); err == nil || changed {
		t.Fatalf("zero generation claim changed=%t err=%v, want error and no change", changed, err)
	}
	if changed, err := store.ClaimMinimalLinkedWorker(ctx, "linked-generation", 4); err != nil || changed {
		t.Fatalf("older generation claim changed=%t err=%v, want false nil", changed, err)
	}
	if changed, err := store.ClaimMinimalLinkedWorker(ctx, "linked-generation", 5); err != nil || changed {
		t.Fatalf("equal generation claim changed=%t err=%v, want false nil", changed, err)
	}
	got, err := store.GetMinimalLinkedThreadByLinkedID(ctx, "linked-generation")
	if err != nil {
		t.Fatal(err)
	}
	if got.State != model.MinimalLinkedReady || got.WorkerGeneration != 5 {
		t.Fatalf("link after stale claims=%#v, want ready generation 5", got)
	}

	if changed, err := store.ClaimMinimalLinkedWorker(ctx, "linked-generation", 6); err != nil || !changed {
		t.Fatalf("newer generation claim changed=%t err=%v, want true nil", changed, err)
	}
	got, err = store.GetMinimalLinkedThreadByLinkedID(ctx, "linked-generation")
	if err != nil {
		t.Fatal(err)
	}
	if got.State != model.MinimalLinkedTelegramRunning || got.WorkerGeneration != 6 {
		t.Fatalf("link after newer claim=%#v, want running generation 6", got)
	}
}

func TestMinimalLinkedReleaseRejectsZeroGeneration(t *testing.T) {
	t.Parallel()

	store := newProtectedMinimalStore(t)
	ctx := context.Background()
	activateRunningMinimalLinkedThread(t, store, "linked-zero-release", 5)
	if changed, err := store.MarkMinimalLinkedTurnStarted(ctx, "linked-zero-release", 5, "turn-1"); err != nil || !changed {
		t.Fatalf("turn start changed=%t err=%v, want true nil", changed, err)
	}

	if changed, err := store.BeginMinimalLinkedRelease(ctx, model.MinimalLinkedRelease{LinkedThreadID: "linked-zero-release", TurnID: "turn-1"}); err == nil || changed {
		t.Fatalf("zero generation release changed=%t err=%v, want error and no change", changed, err)
	}
	got, err := store.GetMinimalLinkedThreadByLinkedID(ctx, "linked-zero-release")
	if err != nil {
		t.Fatal(err)
	}
	if got.State != model.MinimalLinkedTelegramRunning || got.ActiveTurnID != "turn-1" {
		t.Fatalf("link after zero release=%#v, want running turn-1", got)
	}
}

func TestMinimalLinkedBlockedRecordsOnlyReadyOwnershipProbe(t *testing.T) {
	t.Parallel()

	store := newProtectedMinimalStore(t)
	ctx := context.Background()
	blockedAt := time.Date(2026, 8, 23, 3, 0, 0, 0, time.UTC)
	activateReleasedMinimalLinkedThread(t, store, "linked-ready-block", 5, blockedAt.Add(-time.Minute))

	if err := store.RecordMinimalLinkedBlocked(ctx, "linked-ready-block", "active_writer", blockedAt); err != nil {
		t.Fatal(err)
	}
	got, err := store.GetMinimalLinkedThreadByLinkedID(ctx, "linked-ready-block")
	if err != nil {
		t.Fatal(err)
	}
	if got.LastBlockedCode != "active_writer" || got.LastBlockedAt != model.TimeString(blockedAt.Format(time.RFC3339Nano)) {
		t.Fatalf("ready blocked metadata=%#v", got)
	}

	activateRunningMinimalLinkedThread(t, store, "linked-running-block", 5)
	if err := store.RecordMinimalLinkedBlocked(ctx, "linked-running-block", "active_writer", blockedAt); err == nil {
		t.Fatal("running link accepted blocked probe metadata")
	}
	assertMinimalLinkedBlockEmpty(t, store, "linked-running-block")

	activateRunningMinimalLinkedThread(t, store, "linked-release-block", 5)
	if changed, err := store.MarkMinimalLinkedTurnStarted(ctx, "linked-release-block", 5, "turn-1"); err != nil || !changed {
		t.Fatalf("release turn start changed=%t err=%v, want true nil", changed, err)
	}
	if changed, err := store.BeginMinimalLinkedRelease(ctx, model.MinimalLinkedRelease{LinkedThreadID: "linked-release-block", TurnID: "turn-1", WorkerGeneration: 5}); err != nil || !changed {
		t.Fatalf("release begin changed=%t err=%v, want true nil", changed, err)
	}
	if err := store.RecordMinimalLinkedBlocked(ctx, "linked-release-block", "active_writer", blockedAt); err == nil {
		t.Fatal("release_pending link accepted blocked probe metadata")
	}
	assertMinimalLinkedBlockEmpty(t, store, "linked-release-block")

	activateRunningMinimalLinkedThread(t, store, "linked-failed-block", 5)
	if err := store.FailMinimalLinkedThread(ctx, "linked-failed-block", 5, "ambiguous"); err != nil {
		t.Fatal(err)
	}
	if err := store.RecordMinimalLinkedBlocked(ctx, "linked-failed-block", "active_writer", blockedAt); err == nil {
		t.Fatal("failed link accepted blocked probe metadata")
	}
	assertMinimalLinkedBlockEmpty(t, store, "linked-failed-block")
}

func TestMinimalLinkedThreadEnforcesCanonicalSourceAndLinkedID(t *testing.T) {
	t.Parallel()

	store := newProtectedMinimalStore(t)
	ctx := context.Background()
	link := model.MinimalLinkedThread{
		ChatID: 7, TopicID: 3, ProjectID: "p1", SourceThreadID: "source-1", SourceAnchorTurnID: "turn-1",
		State: model.MinimalLinkedTelegramRunning, WorkerGeneration: 1,
	}
	provenance := model.MinimalContinuation{
		Key:       model.MinimalContinuationKey{ChatID: 7, TopicID: 3, SourceThreadID: "source-1", SourceTurnID: "turn-1"},
		ProjectID: "p1",
	}
	if err := store.ActivateMinimalLinkedThread(ctx, link, provenance, model.Thread{ID: "linked-unique", CWD: "/tmp/project"}); err != nil {
		t.Fatal(err)
	}

	if err := store.ActivateMinimalLinkedThread(ctx, link, provenance, model.Thread{ID: "linked-other", CWD: "/tmp/project"}); err == nil {
		t.Fatal("second canonical link for the same chat/source succeeded")
	}
	otherSource := model.MinimalLinkedThread{
		ChatID: 7, TopicID: 3, ProjectID: "p1", SourceThreadID: "source-2", SourceAnchorTurnID: "turn-2",
		State: model.MinimalLinkedTelegramRunning, WorkerGeneration: 1,
	}
	otherProvenance := model.MinimalContinuation{
		Key:       model.MinimalContinuationKey{ChatID: 7, TopicID: 3, SourceThreadID: "source-2", SourceTurnID: "turn-2"},
		ProjectID: "p1",
	}
	if err := store.ActivateMinimalLinkedThread(ctx, otherSource, otherProvenance, model.Thread{ID: "linked-unique", CWD: "/tmp/project"}); err == nil {
		t.Fatal("second canonical link for the same linked thread succeeded")
	}
}

func TestAdoptMinimalLinkedThreadsPrefersThreadBindingAndPreservesLegacyRows(t *testing.T) {
	t.Parallel()

	store := newProtectedMinimalStore(t)
	ctx := context.Background()
	first, created, err := store.ClaimMinimalContinuation(ctx, model.MinimalContinuation{
		Key:       model.MinimalContinuationKey{ChatID: 7, TopicID: 3, SourceThreadID: "source-1", SourceTurnID: "turn-old"},
		ProjectID: "p1",
	})
	if err != nil || !created {
		t.Fatalf("first=%#v created=%t err=%v", first, created, err)
	}
	if err := store.ActivateMinimalContinuation(ctx, *first, model.Thread{ID: "legacy-bound", CWD: "/tmp/project", UpdatedAt: 100}); err != nil {
		t.Fatal(err)
	}
	second, created, err := store.ClaimMinimalContinuation(ctx, model.MinimalContinuation{
		Key:       model.MinimalContinuationKey{ChatID: 7, TopicID: 3, SourceThreadID: "source-1", SourceTurnID: "turn-new"},
		ProjectID: "p1",
	})
	if err != nil || !created {
		t.Fatalf("second=%#v created=%t err=%v", second, created, err)
	}
	if err := store.ActivateMinimalContinuation(ctx, *second, model.Thread{ID: "legacy-latest", CWD: "/tmp/project", UpdatedAt: 200}); err != nil {
		t.Fatal(err)
	}
	if err := store.SetBinding(ctx, 7, 3, "legacy-bound", model.BindingModeBound); err != nil {
		t.Fatal(err)
	}

	adopted, err := store.AdoptMinimalLinkedThreads(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if adopted != 1 {
		t.Fatalf("adopted=%d, want 1", adopted)
	}
	got, err := store.GetMinimalLinkedThread(ctx, 7, 3, "source-1")
	if err != nil {
		t.Fatal(err)
	}
	if got.LinkedThreadID != "legacy-bound" || got.SourceAnchorTurnID != "turn-old" || got.State != model.MinimalLinkedReady {
		t.Fatalf("adopted link=%#v", got)
	}
	assertMinimalContinuationCount(t, store, 7, 3, "source-1", 2)
}

func TestAdoptMinimalLinkedThreadsFallsBackToNewestThenChildID(t *testing.T) {
	t.Parallel()

	store := newProtectedMinimalStore(t)
	ctx := context.Background()
	for _, seed := range []struct {
		sourceTurnID string
		childID      string
		updatedAt    string
	}{
		{sourceTurnID: "turn-old", childID: "legacy-old", updatedAt: "2026-08-23T00:00:00Z"},
		{sourceTurnID: "turn-a", childID: "legacy-a", updatedAt: "2026-08-23T01:00:00Z"},
		{sourceTurnID: "turn-z", childID: "legacy-z", updatedAt: "2026-08-23T01:00:00Z"},
	} {
		claimed, created, err := store.ClaimMinimalContinuation(ctx, model.MinimalContinuation{
			Key:       model.MinimalContinuationKey{ChatID: 8, TopicID: 4, SourceThreadID: "source-2", SourceTurnID: seed.sourceTurnID},
			ProjectID: "p1",
		})
		if err != nil || !created {
			t.Fatalf("claim %s=%#v created=%t err=%v", seed.sourceTurnID, claimed, created, err)
		}
		if err := store.ActivateMinimalContinuation(ctx, *claimed, model.Thread{ID: seed.childID, CWD: "/tmp/project", UpdatedAt: 100}); err != nil {
			t.Fatal(err)
		}
		if _, err := store.db.ExecContext(ctx, `UPDATE minimal_thread_continuations SET updated_at = ? WHERE fork_thread_id = ?`, seed.updatedAt, seed.childID); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.SetBinding(ctx, 8, 4, "unrelated-thread", model.BindingModeBound); err != nil {
		t.Fatal(err)
	}

	adopted, err := store.AdoptMinimalLinkedThreads(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if adopted != 1 {
		t.Fatalf("adopted=%d, want 1", adopted)
	}
	got, err := store.GetMinimalLinkedThread(ctx, 8, 4, "source-2")
	if err != nil {
		t.Fatal(err)
	}
	if got.LinkedThreadID != "legacy-z" || got.SourceAnchorTurnID != "turn-z" || got.ProjectID != "p1" || got.State != model.MinimalLinkedReady {
		t.Fatalf("adopted link=%#v", got)
	}
	assertMinimalContinuationCount(t, store, 8, 4, "source-2", 3)
}

func assertMinimalContinuationCount(t *testing.T, store *Store, chatID, topicID int64, sourceThreadID string, want int) {
	t.Helper()
	var got int
	err := store.db.QueryRowContext(context.Background(), `
	SELECT count(*) FROM minimal_thread_continuations
	WHERE chat_key = ? AND source_thread_id = ?`, model.ChatKey(chatID, topicID), sourceThreadID).Scan(&got)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("legacy rows=%d, want %d", got, want)
	}
}

func activateRunningMinimalLinkedThread(t *testing.T, store *Store, linkedID string, generation uint64) {
	t.Helper()
	ctx := context.Background()
	sourceID := "source-" + linkedID
	anchorID := "turn-" + linkedID
	link := model.MinimalLinkedThread{
		ChatID: 7, TopicID: 3, ProjectID: "p1", SourceThreadID: sourceID, SourceAnchorTurnID: anchorID,
		State: model.MinimalLinkedTelegramRunning, WorkerGeneration: generation,
	}
	provenance := model.MinimalContinuation{
		Key:       model.MinimalContinuationKey{ChatID: 7, TopicID: 3, SourceThreadID: sourceID, SourceTurnID: anchorID},
		ProjectID: "p1",
	}
	if err := store.ActivateMinimalLinkedThread(ctx, link, provenance, model.Thread{ID: linkedID, CWD: "/tmp/project"}); err != nil {
		t.Fatal(err)
	}
}

func activateReleasedMinimalLinkedThread(t *testing.T, store *Store, linkedID string, generation uint64, releasedAt time.Time) {
	t.Helper()
	ctx := context.Background()
	activateRunningMinimalLinkedThread(t, store, linkedID, generation)
	if changed, err := store.MarkMinimalLinkedTurnStarted(ctx, linkedID, generation, "turn-1"); err != nil || !changed {
		t.Fatalf("turn start changed=%t err=%v, want true nil", changed, err)
	}
	if changed, err := store.BeginMinimalLinkedRelease(ctx, model.MinimalLinkedRelease{LinkedThreadID: linkedID, TurnID: "turn-1", WorkerGeneration: generation}); err != nil || !changed {
		t.Fatalf("release begin changed=%t err=%v, want true nil", changed, err)
	}
	if changed, err := store.FinishMinimalLinkedRelease(ctx, linkedID, generation, releasedAt); err != nil || !changed {
		t.Fatalf("release finish changed=%t err=%v, want true nil", changed, err)
	}
}

func assertMinimalLinkedBlockEmpty(t *testing.T, store *Store, linkedID string) {
	t.Helper()
	got, err := store.GetMinimalLinkedThreadByLinkedID(context.Background(), linkedID)
	if err != nil {
		t.Fatal(err)
	}
	if got.LastBlockedAt != "" || got.LastBlockedCode != "" {
		t.Fatalf("blocked metadata for %s = at:%q code:%q, want empty", linkedID, got.LastBlockedAt, got.LastBlockedCode)
	}
}

func TestMinimalContinuationActivationRehomesQueueAndBindingAtomically(t *testing.T) {
	t.Parallel()

	store := newProtectedMinimalStore(t)
	ctx := context.Background()
	key := model.MinimalContinuationKey{ChatID: 7, TopicID: 3, SourceThreadID: "parent", SourceTurnID: "turn-1"}
	claimed, created, err := store.ClaimMinimalContinuation(ctx, model.MinimalContinuation{Key: key, ProjectID: "bridge"})
	if err != nil || !created || claimed.Status != model.MinimalContinuationCreating {
		t.Fatalf("claim=%#v created=%t err=%v", claimed, created, err)
	}
	if err := store.EnqueuePendingCommand(ctx, model.PendingCommand{
		ThreadID: "parent", SourceThreadID: "parent", SourceTurnID: "turn-1",
		ProjectID: "bridge", ChatID: 7, TopicID: 3, Prompt: "private queued prompt",
	}); err != nil {
		t.Fatal(err)
	}
	child := model.Thread{ID: "child", CWD: t.TempDir(), Status: "completed"}
	if err := store.ActivateMinimalContinuation(ctx, *claimed, child); err != nil {
		t.Fatal(err)
	}
	command, err := store.ClaimPendingCommand(ctx, "child")
	if err != nil || command == nil || command.SourceThreadID != "parent" || command.SourceTurnID != "turn-1" {
		t.Fatalf("command=%#v err=%v", command, err)
	}
	binding, _ := store.GetBinding(ctx, 7, 3)
	if binding == nil || binding.ThreadID != "child" {
		t.Fatalf("binding=%#v", binding)
	}
}

func TestMinimalContinuationActivationRehomesOnlyMatchingChatTopicQueue(t *testing.T) {
	t.Parallel()

	store := newProtectedMinimalStore(t)
	ctx := context.Background()
	key := model.MinimalContinuationKey{ChatID: 7, TopicID: 3, SourceThreadID: "parent", SourceTurnID: "turn-1"}
	claimed, created, err := store.ClaimMinimalContinuation(ctx, model.MinimalContinuation{Key: key, ProjectID: "bridge"})
	if err != nil || !created {
		t.Fatalf("claim=%#v created=%t err=%v", claimed, created, err)
	}
	for _, command := range []model.PendingCommand{
		{
			ThreadID: "parent", SourceThreadID: "parent", SourceTurnID: "turn-1",
			ProjectID: "bridge", ChatID: 7, TopicID: 3, Prompt: "matching chat",
		},
		{
			ThreadID: "parent", SourceThreadID: "parent", SourceTurnID: "turn-1",
			ProjectID: "bridge", ChatID: 8, TopicID: 3, Prompt: "other chat",
		},
	} {
		if err := store.EnqueuePendingCommand(ctx, command); err != nil {
			t.Fatal(err)
		}
	}
	child := model.Thread{ID: "child", CWD: t.TempDir(), Status: "completed"}
	if err := store.ActivateMinimalContinuation(ctx, *claimed, child); err != nil {
		t.Fatal(err)
	}
	moved, err := store.ClaimPendingCommand(ctx, "child")
	if err != nil || moved == nil || moved.ChatID != 7 || moved.TopicID != 3 || moved.Prompt != "matching chat" {
		t.Fatalf("moved command=%#v err=%v", moved, err)
	}
	leaked, err := store.ClaimPendingCommand(ctx, "child")
	if err != nil || leaked != nil {
		t.Fatalf("leaked command=%#v err=%v, want no second child claim", leaked, err)
	}
	other, err := store.ClaimPendingCommand(ctx, "parent")
	if err != nil || other == nil || other.ChatID != 8 || other.TopicID != 3 || other.Prompt != "other chat" {
		t.Fatalf("other command=%#v err=%v", other, err)
	}
}

func TestMinimalContinuationActivationRehomesOnlyExactSourceTurnQueue(t *testing.T) {
	t.Parallel()

	store := newProtectedMinimalStore(t)
	ctx := context.Background()
	key := model.MinimalContinuationKey{ChatID: 7, SourceThreadID: "parent", SourceTurnID: "turn-1"}
	claimed, created, err := store.ClaimMinimalContinuation(ctx, model.MinimalContinuation{Key: key, ProjectID: "bridge"})
	if err != nil || !created {
		t.Fatalf("claim=%#v created=%t err=%v", claimed, created, err)
	}
	for _, command := range []model.PendingCommand{
		{
			ThreadID: "parent", SourceThreadID: "parent", SourceTurnID: "turn-1",
			ProjectID: "bridge", ChatID: 7, Prompt: "older source",
		},
		{
			ThreadID: "parent", SourceThreadID: "parent", SourceTurnID: "turn-2",
			ProjectID: "bridge", ChatID: 7, Prompt: "newer source",
		},
	} {
		if err := store.EnqueuePendingCommand(ctx, command); err != nil {
			t.Fatal(err)
		}
	}

	child := model.Thread{ID: "child", CWD: t.TempDir(), Status: "completed"}
	if err := store.ActivateMinimalContinuation(ctx, *claimed, child); err != nil {
		t.Fatal(err)
	}

	moved, err := store.ClaimPendingCommand(ctx, "child")
	if err != nil || moved == nil || moved.Prompt != "older source" || moved.SourceTurnID != "turn-1" {
		t.Fatalf("moved command=%#v err=%v", moved, err)
	}
	olderLeaked, err := store.ClaimPendingCommand(ctx, "child")
	if err != nil || olderLeaked != nil {
		t.Fatalf("extra child command=%#v err=%v, want none", olderLeaked, err)
	}
	newer, err := store.ClaimPendingCommand(ctx, "parent")
	if err != nil || newer == nil || newer.Prompt != "newer source" || newer.SourceTurnID != "turn-2" {
		t.Fatalf("newer command=%#v err=%v", newer, err)
	}
}

func TestMinimalContinuationExactAnchorReusesOnlyExactRow(t *testing.T) {
	t.Parallel()

	store := newProtectedMinimalStore(t)
	ctx := context.Background()
	seed := model.MinimalContinuation{
		Key:       model.MinimalContinuationKey{ChatID: 7, SourceThreadID: "parent", SourceTurnID: "turn-1"},
		ProjectID: "bridge",
	}
	first, firstCreated, err := store.ClaimMinimalContinuation(ctx, seed)
	if err != nil || !firstCreated {
		t.Fatalf("first=%#v created=%t err=%v", first, firstCreated, err)
	}
	second, secondCreated, err := store.ClaimMinimalContinuation(ctx, seed)
	if err != nil || secondCreated || second.CreatedAt != first.CreatedAt {
		t.Fatalf("second=%#v created=%t err=%v", second, secondCreated, err)
	}
	seed.Key.SourceTurnID = "turn-2"
	newer, newerCreated, err := store.ClaimMinimalContinuation(ctx, seed)
	if err != nil || !newerCreated || newer.Key.SourceTurnID != "turn-2" {
		t.Fatalf("newer=%#v created=%t err=%v", newer, newerCreated, err)
	}
}

func TestActiveMinimalContinuationForForkUsesExactBoundChild(t *testing.T) {
	t.Parallel()

	store := newProtectedMinimalStore(t)
	ctx := context.Background()
	first, created, err := store.ClaimMinimalContinuation(ctx, model.MinimalContinuation{
		Key:       model.MinimalContinuationKey{ChatID: 7, TopicID: 3, SourceThreadID: "parent-old", SourceTurnID: "turn-old"},
		ProjectID: "bridge",
	})
	if err != nil || !created {
		t.Fatalf("first=%#v created=%t err=%v", first, created, err)
	}
	if err := store.ActivateMinimalContinuation(ctx, *first, model.Thread{ID: "child-old", CWD: "/tmp/project"}); err != nil {
		t.Fatal(err)
	}
	second, created, err := store.ClaimMinimalContinuation(ctx, model.MinimalContinuation{
		Key:       model.MinimalContinuationKey{ChatID: 7, TopicID: 3, SourceThreadID: "parent-new", SourceTurnID: "turn-new"},
		ProjectID: "bridge",
	})
	if err != nil || !created {
		t.Fatalf("second=%#v created=%t err=%v", second, created, err)
	}
	if err := store.ActivateMinimalContinuation(ctx, *second, model.Thread{ID: "child-new", CWD: "/tmp/project"}); err != nil {
		t.Fatal(err)
	}

	got, err := store.ActiveMinimalContinuationForFork(ctx, 7, 3, "child-old")

	if err != nil {
		t.Fatal(err)
	}
	if got == nil || got.Key.SourceThreadID != "parent-old" || got.Key.SourceTurnID != "turn-old" || got.ForkThreadID != "child-old" {
		t.Fatalf("continuation=%#v, want exact old fork", got)
	}
	missing, err := store.ActiveMinimalContinuationForFork(ctx, 7, 3, "normal-thread")
	if err != nil {
		t.Fatal(err)
	}
	if missing != nil {
		t.Fatalf("normal thread matched continuation: %#v", missing)
	}
}

func TestMinimalContinuationFailureRecoveryAndRepairRearm(t *testing.T) {
	t.Parallel()

	store := newProtectedMinimalStore(t)
	ctx := context.Background()
	definiteSeed := model.MinimalContinuation{
		Key:       model.MinimalContinuationKey{ChatID: 7, TopicID: 3, SourceThreadID: "parent", SourceTurnID: "definite-failure"},
		ProjectID: "bridge",
	}
	definite, created, err := store.ClaimMinimalContinuation(ctx, definiteSeed)
	if err != nil || !created {
		t.Fatalf("definite claim=%#v created=%t err=%v", definite, created, err)
	}
	if err := store.FailMinimalContinuation(ctx, *definite, model.MinimalContinuationFailureDefinite); err != nil {
		t.Fatal(err)
	}
	reclaimed, recreated, err := store.ClaimMinimalContinuation(ctx, definiteSeed)
	if err != nil || !recreated || reclaimed.Status != model.MinimalContinuationCreating {
		t.Fatalf("definite retry=%#v created=%t err=%v", reclaimed, recreated, err)
	}

	ambiguousSeed := model.MinimalContinuation{
		Key:       model.MinimalContinuationKey{ChatID: 7, TopicID: 3, SourceThreadID: "parent", SourceTurnID: "ambiguous-crash"},
		ProjectID: "bridge",
	}
	ambiguous, created, err := store.ClaimMinimalContinuation(ctx, ambiguousSeed)
	if err != nil || !created {
		t.Fatalf("ambiguous claim=%#v created=%t err=%v", ambiguous, created, err)
	}
	otherSeed := model.MinimalContinuation{
		Key:       model.MinimalContinuationKey{ChatID: 8, TopicID: 3, SourceThreadID: "parent", SourceTurnID: "other-chat"},
		ProjectID: "bridge",
	}
	if _, _, err := store.ClaimMinimalContinuation(ctx, otherSeed); err != nil {
		t.Fatal(err)
	}
	recovered, err := store.RecoverCreatingMinimalContinuations(ctx)
	if err != nil || recovered != 3 {
		t.Fatalf("recovered=%d err=%v", recovered, err)
	}
	stillAmbiguous, recreated, err := store.ClaimMinimalContinuation(ctx, ambiguousSeed)
	if err != nil || recreated || stillAmbiguous.Status != model.MinimalContinuationFailed || stillAmbiguous.FailureKind != model.MinimalContinuationFailureAmbiguous {
		t.Fatalf("ambiguous reuse=%#v created=%t err=%v", stillAmbiguous, recreated, err)
	}
	rearmed, err := store.RearmAmbiguousMinimalContinuations(ctx, 7, 3)
	if err != nil || rearmed != 2 {
		t.Fatalf("rearmed=%d err=%v", rearmed, err)
	}
	retryAfterRepair, recreated, err := store.ClaimMinimalContinuation(ctx, ambiguousSeed)
	if err != nil || !recreated || retryAfterRepair.Status != model.MinimalContinuationCreating {
		t.Fatalf("repair retry=%#v created=%t err=%v", retryAfterRepair, recreated, err)
	}
	other, err := store.GetMinimalContinuation(ctx, otherSeed.Key)
	if err != nil || other == nil || other.FailureKind != model.MinimalContinuationFailureAmbiguous {
		t.Fatalf("other chat continuation=%#v err=%v", other, err)
	}
}

func TestMinimalContinuationPromptRemainsProtected(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "state.sqlite")
	store, err := OpenWithProtector(path, securestore.NewDeterministicTestProtector())
	if err != nil {
		t.Fatal(err)
	}
	const prompt = "CONTINUATION_PRIVATE_PROMPT_6f23d8"
	if err := store.EnqueuePendingCommand(context.Background(), model.PendingCommand{
		ThreadID: "parent", SourceThreadID: "parent", SourceTurnID: "turn-1",
		ProjectID: "bridge", ChatID: 7, Prompt: prompt,
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	for _, candidate := range []string{path, path + "-wal", path + "-shm"} {
		data, err := os.ReadFile(candidate)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil || bytes.Contains(data, []byte(prompt)) {
			t.Fatalf("path=%s err=%v plaintext=%t", candidate, err, bytes.Contains(data, []byte(prompt)))
		}
	}
}

func TestReleaseClaimedPendingCommandRetainsProtectedPrompt(t *testing.T) {
	t.Parallel()

	store := newProtectedMinimalStore(t)
	ctx := context.Background()
	if err := store.EnqueuePendingCommand(ctx, model.PendingCommand{ThreadID: "parent", ProjectID: "bridge", Prompt: "retry me"}); err != nil {
		t.Fatal(err)
	}
	first, err := store.ClaimPendingCommand(ctx, "parent")
	if err != nil || first == nil {
		t.Fatalf("first=%#v err=%v", first, err)
	}
	if err := store.ReleaseClaimedPendingCommand(ctx, first.ID); err != nil {
		t.Fatal(err)
	}
	second, err := store.ClaimPendingCommand(ctx, "parent")
	if err != nil || second == nil || second.ID != first.ID || second.Prompt != "retry me" {
		t.Fatalf("second=%#v err=%v", second, err)
	}
}

func TestPendingCommandExactSourceBacklogIsChatTopicIsolated(t *testing.T) {
	t.Parallel()

	store := newProtectedMinimalStore(t)
	ctx := context.Background()
	if err := store.EnqueuePendingCommand(ctx, model.PendingCommand{
		ThreadID: "parent", SourceThreadID: "parent", SourceTurnID: "turn-1",
		ProjectID: "bridge", ChatID: 7, TopicID: 3, Prompt: "same topic",
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.EnqueuePendingCommand(ctx, model.PendingCommand{
		ThreadID: "parent", SourceThreadID: "parent", SourceTurnID: "turn-1",
		ProjectID: "bridge", ChatID: 7, TopicID: 4, Prompt: "different topic",
	}); err != nil {
		t.Fatal(err)
	}
	if ok, err := store.HasPendingCommandBacklogForSource(ctx, 7, 3, "parent", "turn-1"); err != nil || !ok {
		t.Fatalf("same-topic backlog=%t err=%v, want true", ok, err)
	}
	if ok, err := store.HasPendingCommandBacklogForSource(ctx, 7, 5, "parent", "turn-1"); err != nil || ok {
		t.Fatalf("different-topic backlog=%t err=%v, want false", ok, err)
	}
	claimed, err := store.ClaimPendingCommand(ctx, "parent")
	if err != nil || claimed == nil {
		t.Fatalf("claimed=%#v err=%v", claimed, err)
	}
	if ok, err := store.HasPendingCommandBacklogForSource(ctx, 7, 3, "parent", "turn-1"); err != nil || !ok {
		t.Fatalf("claimed same-topic backlog=%t err=%v, want true", ok, err)
	}
	if err := store.CompletePendingCommand(ctx, claimed.ID); err != nil {
		t.Fatal(err)
	}
	if ok, err := store.HasPendingCommandBacklogForSource(ctx, 7, 3, "parent", "turn-1"); err != nil || ok {
		t.Fatalf("completed same-topic backlog=%t err=%v, want false after terminal finalization", ok, err)
	}
}

func TestClaimPendingCommandForSourceUsesOldestExactAnchorAndRespectsClaimed(t *testing.T) {
	t.Parallel()

	store := newProtectedMinimalStore(t)
	ctx := context.Background()
	if err := store.EnqueuePendingCommand(ctx, model.PendingCommand{
		ThreadID: "child", SourceThreadID: "parent", SourceTurnID: "turn-1",
		ProjectID: "bridge", ChatID: 7, TopicID: 3, Prompt: "A",
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.EnqueuePendingCommand(ctx, model.PendingCommand{
		ThreadID: "parent", SourceThreadID: "parent", SourceTurnID: "turn-1",
		ProjectID: "bridge", ChatID: 7, TopicID: 3, Prompt: "B",
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.EnqueuePendingCommand(ctx, model.PendingCommand{
		ThreadID: "parent", SourceThreadID: "parent", SourceTurnID: "turn-1",
		ProjectID: "bridge", ChatID: 7, TopicID: 4, Prompt: "different topic",
	}); err != nil {
		t.Fatal(err)
	}

	first, err := store.ClaimPendingCommandForSource(ctx, 7, 3, "parent", "turn-1")
	if err != nil {
		t.Fatal(err)
	}
	if first == nil || first.ThreadID != "child" || first.Prompt != "A" {
		t.Fatalf("first exact-source claim = %#v, want oldest child A", first)
	}
	blocked, err := store.ClaimPendingCommandForSource(ctx, 7, 3, "parent", "turn-1")
	if err != nil || blocked != nil {
		t.Fatalf("second claim while first claimed = %#v err=%v, want nil", blocked, err)
	}
	if err := store.CompletePendingCommand(ctx, first.ID); err != nil {
		t.Fatal(err)
	}
	second, err := store.ClaimPendingCommandForSource(ctx, 7, 3, "parent", "turn-1")
	if err != nil {
		t.Fatal(err)
	}
	if second == nil || second.ThreadID != "parent" || second.Prompt != "B" {
		t.Fatalf("second exact-source claim = %#v, want B after A completed", second)
	}
}

func TestPendingCommandIsEncryptedClaimedAndNulled(t *testing.T) {
	t.Parallel()
	dbPath := filepath.Join(t.TempDir(), "state.sqlite")
	store, err := OpenWithProtector(dbPath, securestore.NewDeterministicTestProtector())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	ctx := context.Background()
	const prompt = "PRIVATE_PENDING_PROMPT_6f41a7"
	if err := store.EnqueuePendingCommand(ctx, model.PendingCommand{ThreadID: "thread-1", ProjectID: "bridge", Prompt: prompt}); err != nil {
		t.Fatal(err)
	}
	var stored string
	if err := store.db.QueryRowContext(ctx, `SELECT prompt_payload FROM pending_commands WHERE thread_id = ?`, "thread-1").Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored == prompt || !bytes.HasPrefix([]byte(stored), []byte("dpapi:v1:")) {
		t.Fatalf("stored payload = %q, want protected envelope", stored)
	}
	claimed, err := store.ClaimPendingCommand(ctx, "thread-1")
	if err != nil {
		t.Fatal(err)
	}
	if claimed == nil || claimed.Prompt != prompt || claimed.Status != model.PendingCommandStatusClaimed {
		t.Fatalf("claimed = %#v", claimed)
	}
	if err := store.CompletePendingCommand(ctx, claimed.ID); err != nil {
		t.Fatal(err)
	}
	var status string
	var payload any
	if err := store.db.QueryRowContext(ctx, `SELECT status, prompt_payload FROM pending_commands WHERE id = ?`, claimed.ID).Scan(&status, &payload); err != nil {
		t.Fatal(err)
	}
	if status != model.PendingCommandStatusCompleted || payload != nil {
		t.Fatalf("terminal row status=%q payload=%#v", status, payload)
	}
	if _, err := store.db.ExecContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{dbPath, dbPath + "-wal", dbPath + "-shm"} {
		data, err := os.ReadFile(path)
		if err != nil && os.IsNotExist(err) {
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(data, []byte(prompt)) {
			t.Fatalf("plaintext prompt found in %s", filepath.Base(path))
		}
	}
}

func TestPendingCommandClaimIsAtomicPerThread(t *testing.T) {
	t.Parallel()
	store, err := OpenWithProtector(filepath.Join(t.TempDir(), "state.sqlite"), securestore.NewDeterministicTestProtector())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	ctx := context.Background()
	for _, prompt := range []string{"first", "second"} {
		if err := store.EnqueuePendingCommand(ctx, model.PendingCommand{ThreadID: "thread-1", ProjectID: "bridge", Prompt: prompt}); err != nil {
			t.Fatal(err)
		}
	}
	var wg sync.WaitGroup
	claims := make(chan *model.PendingCommand, 2)
	errs := make(chan error, 2)
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			claim, err := store.ClaimPendingCommand(ctx, "thread-1")
			claims <- claim
			errs <- err
		}()
	}
	wg.Wait()
	close(claims)
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	claimed := 0
	for claim := range claims {
		if claim != nil {
			claimed++
			if claim.Prompt != "first" {
				t.Fatalf("claimed prompt = %q, want FIFO first", claim.Prompt)
			}
		}
	}
	if claimed != 1 {
		t.Fatalf("claims = %d, want exactly one", claimed)
	}
}

func TestFailedPendingCommandNullsPayloadAndAllowsNextClaim(t *testing.T) {
	t.Parallel()
	store, err := OpenWithProtector(filepath.Join(t.TempDir(), "state.sqlite"), securestore.NewDeterministicTestProtector())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	ctx := context.Background()
	for _, prompt := range []string{"first", "second"} {
		if err := store.EnqueuePendingCommand(ctx, model.PendingCommand{ThreadID: "thread-1", ProjectID: "bridge", Prompt: prompt}); err != nil {
			t.Fatal(err)
		}
	}
	first, err := store.ClaimPendingCommand(ctx, "thread-1")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.FailPendingCommand(ctx, first.ID); err != nil {
		t.Fatal(err)
	}
	second, err := store.ClaimPendingCommand(ctx, "thread-1")
	if err != nil {
		t.Fatal(err)
	}
	if second == nil || second.Prompt != "second" {
		t.Fatalf("second claim = %#v", second)
	}
	var payload any
	if err := store.db.QueryRowContext(ctx, `SELECT prompt_payload FROM pending_commands WHERE id = ?`, first.ID).Scan(&payload); err != nil {
		t.Fatal(err)
	}
	if payload != nil {
		t.Fatalf("failed payload = %#v, want NULL", payload)
	}
}

func TestPendingCommandFinalizationRequiresOneClaimedRow(t *testing.T) {
	t.Parallel()
	store, err := OpenWithProtector(filepath.Join(t.TempDir(), "state.sqlite"), securestore.NewDeterministicTestProtector())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	ctx := context.Background()
	if err := store.EnqueuePendingCommand(ctx, model.PendingCommand{ThreadID: "thread-finalize", ProjectID: "bridge", Prompt: "pending"}); err != nil {
		t.Fatal(err)
	}
	var pendingID int64
	if err := store.db.QueryRowContext(ctx, `SELECT id FROM pending_commands WHERE thread_id = ?`, "thread-finalize").Scan(&pendingID); err != nil {
		t.Fatal(err)
	}
	if err := store.CompletePendingCommand(ctx, pendingID); err == nil {
		t.Fatal("CompletePendingCommand accepted an unclaimed row")
	}
	if err := store.FailPendingCommand(ctx, pendingID+1000); err == nil {
		t.Fatal("FailPendingCommand accepted a missing row")
	}
}

func TestRecoverClaimedPendingCommandsFailsAndNullsAbandonedRows(t *testing.T) {
	t.Parallel()
	store, err := OpenWithProtector(filepath.Join(t.TempDir(), "state.sqlite"), securestore.NewDeterministicTestProtector())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	ctx := context.Background()
	for _, prompt := range []string{"ambiguous-remote-outcome", "safe-later-command"} {
		if err := store.EnqueuePendingCommand(ctx, model.PendingCommand{ThreadID: "thread-recovery", ProjectID: "bridge", Prompt: prompt}); err != nil {
			t.Fatal(err)
		}
	}
	abandoned, err := store.ClaimPendingCommand(ctx, "thread-recovery")
	if err != nil {
		t.Fatal(err)
	}
	recovered, err := store.RecoverClaimedPendingCommands(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if recovered != 1 {
		t.Fatalf("recovered rows = %d, want 1", recovered)
	}
	var status string
	var payload any
	if err := store.db.QueryRowContext(ctx, `SELECT status, prompt_payload FROM pending_commands WHERE id = ?`, abandoned.ID).Scan(&status, &payload); err != nil {
		t.Fatal(err)
	}
	if status != model.PendingCommandStatusFailed || payload != nil {
		t.Fatalf("abandoned row status=%q payload=%#v", status, payload)
	}
	next, err := store.ClaimPendingCommand(ctx, "thread-recovery")
	if err != nil {
		t.Fatal(err)
	}
	if next == nil || next.Prompt != "safe-later-command" {
		t.Fatalf("next claim = %#v", next)
	}
}
