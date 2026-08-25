package storage

import (
	"bytes"
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/alclssna33/codex_to_telegram/internal/model"
	"github.com/alclssna33/codex_to_telegram/internal/securestore"
)

func TestNotifierObservationDoesNotPersistContent(t *testing.T) {
	t.Parallel()

	store := openNotifierTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 8, 24, 9, 0, 0, 0, time.UTC)

	if err := store.ObserveNotifierThread(ctx, "thread-1", 100, now); err != nil {
		t.Fatal(err)
	}
	due, err := store.ListNotifierObservationsDue(ctx, 10)
	if err != nil || len(due) != 1 || due[0].ThreadID != "thread-1" || due[0].BaselineReady {
		t.Fatalf("due=%#v err=%v", due, err)
	}
	if err := store.RecordNotifierRead(ctx, "thread-1", "turn-1", "inProgress", 101, true, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}

	if _, err := store.db.ExecContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(store.Path())
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"private prompt marker", "private final marker", `C:\Users\private`, "private title marker"} {
		if bytes.Contains(raw, []byte(forbidden)) {
			t.Fatalf("database contains %q", forbidden)
		}
	}
}

func TestNotifierObservationPostActivationChangesAreDueMostRecentFirst(t *testing.T) {
	t.Parallel()

	store := openNotifierTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 8, 24, 9, 0, 0, 0, time.UTC)

	activation, err := store.EnsureNotifierActivation(ctx, now)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.ObserveNotifierThread(ctx, "thread-b", activation-1, now); err != nil {
		t.Fatal(err)
	}
	if err := store.RecordNotifierRead(ctx, "thread-b", "turn-b", "inProgress", activation-1, false, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	first, err := store.NotifierObservation(ctx, "thread-b")
	if err != nil || first == nil {
		t.Fatalf("first observation=%#v err=%v", first, err)
	}
	if err := store.ObserveNotifierThread(ctx, "thread-a", activation-1, now.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := store.RecordNotifierRead(ctx, "thread-a", "turn-a", "inProgress", activation-1, false, now.Add(3*time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := store.ObserveNotifierThread(ctx, "thread-b", activation+1, now.Add(4*time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := store.ObserveNotifierThread(ctx, "thread-a", activation+2, now.Add(5*time.Second)); err != nil {
		t.Fatal(err)
	}

	updated, err := store.NotifierObservation(ctx, "thread-b")
	if err != nil || updated == nil {
		t.Fatalf("updated observation=%#v err=%v", updated, err)
	}
	if updated.DiscoverySeq != first.DiscoverySeq {
		t.Fatalf("discovery seq changed from %d to %d", first.DiscoverySeq, updated.DiscoverySeq)
	}
	if !updated.ReadRequired || updated.LastTurnID != "turn-b" || updated.LastTurnStatus != "inProgress" {
		t.Fatalf("updated observation=%#v, want due with retained baseline", updated)
	}

	due, err := store.ListNotifierObservationsDue(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(due) != 2 || due[0].ThreadID != "thread-a" || due[1].ThreadID != "thread-b" {
		t.Fatalf("due order=%#v, want newest changed thread-a then thread-b", due)
	}
}

func TestNotifierObservationTerminalReadClearsDueAndCountsActiveRows(t *testing.T) {
	t.Parallel()

	store := openNotifierTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 8, 24, 9, 0, 0, 0, time.UTC)

	if err := store.ObserveNotifierThread(ctx, "thread-active", 100, now); err != nil {
		t.Fatal(err)
	}
	if err := store.ObserveNotifierThread(ctx, "thread-terminal", 101, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := store.RecordNotifierRead(ctx, "thread-active", "turn-active", "running", 100, true, now.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := store.RecordNotifierRead(ctx, "thread-terminal", "turn-terminal", "completed", 101, false, now.Add(3*time.Second)); err != nil {
		t.Fatal(err)
	}

	due, err := store.ListNotifierObservationsDue(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(due) != 1 || due[0].ThreadID != "thread-active" || !due[0].BaselineReady {
		t.Fatalf("due=%#v, want only active baseline due", due)
	}
	tracked, active, err := store.CountNotifierObservations(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if tracked != 2 || active != 1 {
		t.Fatalf("counts tracked=%d active=%d, want 2/1", tracked, active)
	}
}

func TestNotifierObservationDeferPersistsUntilRetryTime(t *testing.T) {
	t.Parallel()

	store := openNotifierTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 8, 25, 9, 0, 0, 0, time.UTC)
	if err := store.ObserveNotifierThread(ctx, "slow-thread", now.Unix(), now); err != nil {
		t.Fatal(err)
	}
	retryAt := now.Add(30 * time.Second)
	if err := store.DeferNotifierRead(ctx, "slow-thread", retryAt, now); err != nil {
		t.Fatal(err)
	}
	if due, err := store.ListNotifierObservationsDueAt(ctx, 10, now.Add(29*time.Second)); err != nil || len(due) != 0 {
		t.Fatalf("due before retry=%#v err=%v, want none", due, err)
	}
	if due, err := store.ListNotifierObservationsDueAt(ctx, 10, retryAt); err != nil || len(due) != 1 || due[0].ThreadID != "slow-thread" {
		t.Fatalf("due at retry=%#v err=%v, want slow-thread", due, err)
	}
	if err := store.ObserveNotifierThread(ctx, "slow-thread", now.Unix()+1, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if due, err := store.ListNotifierObservationsDueAt(ctx, 10, now.Add(time.Second)); err != nil || len(due) != 1 || due[0].ThreadID != "slow-thread" {
		t.Fatalf("due after update=%#v err=%v, want immediate slow-thread", due, err)
	}
}

func TestNotifierObservationPersistsAcrossRestart(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "state.sqlite")
	protector := securestore.NewDeterministicTestProtector()
	store, err := OpenWithProtector(path, protector)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	now := time.Date(2026, 8, 24, 9, 0, 0, 0, time.UTC)
	if err := store.ObserveNotifierThread(ctx, "thread-1", 100, now); err != nil {
		t.Fatal(err)
	}
	if err := store.RecordNotifierRead(ctx, "thread-1", "turn-1", "running", 100, true, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	before, err := store.NotifierObservation(ctx, "thread-1")
	if err != nil || before == nil {
		t.Fatalf("before observation=%#v err=%v", before, err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := OpenWithProtector(path, protector)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	after, err := reopened.NotifierObservation(ctx, "thread-1")
	if err != nil || after == nil {
		t.Fatalf("after observation=%#v err=%v", after, err)
	}
	if *after != *before {
		t.Fatalf("after observation=%#v, want %#v", after, before)
	}
	due, err := reopened.ListNotifierObservationsDue(ctx, 10)
	if err != nil || len(due) != 1 || due[0].ThreadID != "thread-1" {
		t.Fatalf("reopened due=%#v err=%v", due, err)
	}
}

func TestEnsureNotifierActivationIsStableAcrossRestart(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "state.sqlite")
	protector := securestore.NewDeterministicTestProtector()
	store, err := OpenWithProtector(path, protector)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	firstNow := time.Date(2026, 8, 24, 9, 0, 0, 500, time.FixedZone("KST", 9*60*60))
	first, err := store.EnsureNotifierActivation(ctx, firstNow)
	if err != nil {
		t.Fatal(err)
	}
	if first != firstNow.UTC().Unix() {
		t.Fatalf("activation=%d, want %d", first, firstNow.UTC().Unix())
	}
	later, err := store.EnsureNotifierActivation(ctx, firstNow.Add(2*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if later != first {
		t.Fatalf("same-process activation advanced to %d, want %d", later, first)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := OpenWithProtector(path, protector)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	restarted, err := reopened.EnsureNotifierActivation(ctx, firstNow.Add(24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if restarted != first {
		t.Fatalf("restarted activation=%d, want original %d", restarted, first)
	}
}

func TestNotifierMigrationRetiresInteractiveDeliveriesOnce(t *testing.T) {
	t.Parallel()

	store := openNotifierTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 8, 24, 9, 0, 0, 0, time.UTC)
	seedNotifierDelivery(t, store, "legacy-command", "minimal_approval", model.DeliveryStatusPending, "", "private command marker")
	seedNotifierDelivery(t, store, "legacy-retry", "observer", model.DeliveryStatusRetry, "", "private retry marker")
	seedNotifierDelivery(t, store, "notifier-terminal", "notifier_terminal", model.DeliveryStatusPending, "", "private notifier marker")
	if got := deliveryStatus(t, store, "legacy-retry"); got != model.DeliveryStatusRetry {
		t.Fatalf("test setup retry status=%q", got)
	}

	retired, err := store.MigrateNotifierProfile(ctx, now)
	if err != nil || retired != 2 {
		t.Fatalf("retired=%d err=%v", retired, err)
	}
	if got := deliveryStatus(t, store, "legacy-command"); got != model.DeliveryStatusDead {
		t.Fatalf("legacy status=%q", got)
	}
	if got := deliveryStatus(t, store, "legacy-retry"); got != model.DeliveryStatusDead {
		t.Fatalf("retry status=%q", got)
	}
	if got := deliveryStatus(t, store, "notifier-terminal"); got != model.DeliveryStatusPending {
		t.Fatalf("notifier status=%q", got)
	}
	if payload := deliveryPayload(t, store, "legacy-command"); payload.Valid {
		t.Fatalf("legacy payload still present: %q", payload.String)
	}
	if payload := deliveryPayload(t, store, "legacy-retry"); payload.Valid {
		t.Fatalf("retry payload still present: %q", payload.String)
	}
	if payload := deliveryPayload(t, store, "notifier-terminal"); !payload.Valid {
		t.Fatal("notifier terminal payload was cleared")
	}
	if second, err := store.MigrateNotifierProfile(ctx, now.Add(time.Second)); err != nil || second != 0 {
		t.Fatalf("second=%d err=%v", second, err)
	}
}

func TestNotifierMigrationPreservesTargetSecretsAndNotifierDelivery(t *testing.T) {
	t.Parallel()

	store := openNotifierTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 8, 24, 9, 30, 0, 0, time.UTC)
	if err := store.SetGlobalObserverTarget(ctx, 7001, 42, true); err != nil {
		t.Fatal(err)
	}
	secretState := map[string]string{
		"telegram.bot_token":              "protected-token-state",
		"codex.reasoning_effort":          "high",
		"turn_terminal.defer.thread.turn": `{"last_reason":"secret deferred content"}`,
	}
	for key, value := range secretState {
		if err := store.SetState(ctx, key, value); err != nil {
			t.Fatal(err)
		}
	}
	seedNotifierDelivery(t, store, "notifier-terminal", "notifier_terminal", model.DeliveryStatusPending, "notifier-group", "private notifier marker")
	seedNotifierDelivery(t, store, "interactive-pending", "minimal_approval", model.DeliveryStatusPending, "legacy-terminal", "private approval marker")
	seedNotifierDelivery(t, store, "interactive-retry", "observer", model.DeliveryStatusRetry, "legacy-terminal", "private observer marker")
	seedNotifierTerminalEvent(t, store, "legacy-terminal", model.DeliveryStatusPending)
	seedNotifierTerminalEvent(t, store, "notifier-group", model.DeliveryStatusPending)
	if got := deliveryStatus(t, store, "interactive-retry"); got != model.DeliveryStatusRetry {
		t.Fatalf("test setup retry status=%q", got)
	}

	notifierBefore := deliveryRawRow(t, store, "notifier-terminal")
	stateBefore, err := store.ListState(ctx)
	if err != nil {
		t.Fatal(err)
	}

	retired, err := store.MigrateNotifierProfile(ctx, now)
	if err != nil || retired != 2 {
		t.Fatalf("retired=%d err=%v", retired, err)
	}
	target, configured, err := store.GetGlobalObserverTarget(ctx)
	if err != nil || !configured || target == nil || target.ChatID != 7001 || target.TopicID != 42 || !target.Enabled {
		t.Fatalf("target=%#v configured=%t err=%v", target, configured, err)
	}
	for key, want := range secretState {
		got, err := store.GetState(ctx, key)
		if err != nil || got != want {
			t.Fatalf("state %q=%q err=%v, want %q", key, got, err, want)
		}
	}
	if got := terminalDeliveryStatus(t, store, "legacy-terminal"); got != model.DeliveryStatusDead {
		t.Fatalf("legacy terminal status=%q", got)
	}
	if got := terminalDeliveryStatus(t, store, "notifier-group"); got != model.DeliveryStatusPending {
		t.Fatalf("notifier terminal group status=%q", got)
	}
	for _, eventID := range []string{"interactive-pending", "interactive-retry"} {
		if got := deliveryStatus(t, store, eventID); got != model.DeliveryStatusDead {
			t.Fatalf("%s status=%q", eventID, got)
		}
		if payload := deliveryPayload(t, store, eventID); payload.Valid {
			t.Fatalf("%s payload still present: %q", eventID, payload.String)
		}
	}
	if after := deliveryRawRow(t, store, "notifier-terminal"); after != notifierBefore {
		t.Fatalf("notifier delivery changed\nbefore=%#v\nafter=%#v", notifierBefore, after)
	}
	stateAfter, err := store.ListState(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for key, want := range stateBefore {
		if key == "notifier.migration.v1" {
			continue
		}
		if got := stateAfter[key]; got != want {
			t.Fatalf("state %q changed from %q to %q", key, want, got)
		}
	}
	if got := stateAfter["notifier.migration.v1"]; got != "done" {
		t.Fatalf("migration state=%q, want done", got)
	}
}

func TestNotifierMigrationRollsBackWhenStateCannotBeRecorded(t *testing.T) {
	t.Parallel()

	store := openNotifierTestStore(t)
	ctx := context.Background()
	seedNotifierDelivery(t, store, "interactive-pending", "minimal_approval", model.DeliveryStatusPending, "legacy-terminal", "private approval marker")
	seedNotifierTerminalEvent(t, store, "legacy-terminal", model.DeliveryStatusPending)
	beforePayload := deliveryPayload(t, store, "interactive-pending")
	if !beforePayload.Valid {
		t.Fatal("test setup did not seed a protected delivery payload")
	}
	if _, err := store.db.ExecContext(ctx, `
	CREATE TRIGGER fail_notifier_migration_state
	BEFORE INSERT ON daemon_state
	WHEN NEW.key = 'notifier.migration.v1'
	BEGIN
		SELECT RAISE(ABORT, 'injected migration state failure');
	END`); err != nil {
		t.Fatal(err)
	}

	retired, err := store.MigrateNotifierProfile(ctx, time.Date(2026, 8, 24, 10, 0, 0, 0, time.UTC))
	if err == nil || retired != 0 {
		t.Fatalf("retired=%d err=%v, want rollback error", retired, err)
	}
	if got := deliveryStatus(t, store, "interactive-pending"); got != model.DeliveryStatusPending {
		t.Fatalf("delivery status after rollback=%q", got)
	}
	if got := terminalDeliveryStatus(t, store, "legacy-terminal"); got != model.DeliveryStatusPending {
		t.Fatalf("terminal status after rollback=%q", got)
	}
	if afterPayload := deliveryPayload(t, store, "interactive-pending"); afterPayload != beforePayload {
		t.Fatalf("payload after rollback=%#v, want %#v", afterPayload, beforePayload)
	}
	if got, err := store.GetState(ctx, "notifier.migration.v1"); err != nil || got != "" {
		t.Fatalf("migration state after rollback=%q err=%v", got, err)
	}
}

func seedNotifierDelivery(t *testing.T, store *Store, eventID, kind, status, groupID, marker string) {
	t.Helper()
	if err := store.EnqueueDelivery(context.Background(), model.DeliveryQueueItem{
		EventID:       eventID,
		ChatKey:       model.ChatKey(7001, 42),
		ChatID:        7001,
		TopicID:       42,
		ThreadID:      "thread-" + eventID,
		Kind:          kind,
		Status:        status,
		AvailableAt:   model.TimeString("2026-08-24T09:00:00Z"),
		PayloadJSON:   MustJSON(model.DeliveryPayload{Text: marker, ThreadID: "thread-" + eventID, EventID: eventID}),
		GroupID:       groupID,
		SequenceNo:    1,
		SequenceCount: 1,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(context.Background(), `UPDATE delivery_queue SET status=? WHERE event_id=?`, status, eventID); err != nil {
		t.Fatal(err)
	}
	if groupID == "" {
		return
	}
	if _, err := store.db.ExecContext(context.Background(), `UPDATE delivery_queue SET group_id=? WHERE event_id=?`, groupID, eventID); err != nil {
		t.Fatal(err)
	}
}

func seedNotifierTerminalEvent(t *testing.T, store *Store, terminalKey, status string) {
	t.Helper()
	if _, err := store.db.ExecContext(context.Background(), `
	INSERT INTO terminal_events(terminal_key, thread_id, turn_id, status, delivery_status, updated_at)
	VALUES (?, ?, ?, ?, ?, ?)`,
		terminalKey, "thread-"+terminalKey, "turn-"+terminalKey, "completed", status, "2026-08-24T09:00:00Z"); err != nil {
		t.Fatal(err)
	}
}

func deliveryStatus(t *testing.T, store *Store, eventID string) string {
	t.Helper()
	var status string
	if err := store.db.QueryRowContext(context.Background(), `SELECT status FROM delivery_queue WHERE event_id=?`, eventID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	return status
}

func terminalDeliveryStatus(t *testing.T, store *Store, terminalKey string) string {
	t.Helper()
	var status string
	if err := store.db.QueryRowContext(context.Background(), `SELECT delivery_status FROM terminal_events WHERE terminal_key=?`, terminalKey).Scan(&status); err != nil {
		t.Fatal(err)
	}
	return status
}

func deliveryPayload(t *testing.T, store *Store, eventID string) sql.NullString {
	t.Helper()
	var payload sql.NullString
	if err := store.db.QueryRowContext(context.Background(), `SELECT payload_json FROM delivery_queue WHERE event_id=?`, eventID).Scan(&payload); err != nil {
		t.Fatal(err)
	}
	return payload
}

type notifierRawDeliveryRow struct {
	Status        string
	RetryCount    int
	AvailableAt   string
	LastError     sql.NullString
	PayloadJSON   sql.NullString
	UpdatedAt     string
	GroupID       sql.NullString
	SequenceNo    int
	SequenceCount int
}

func deliveryRawRow(t *testing.T, store *Store, eventID string) notifierRawDeliveryRow {
	t.Helper()
	var row notifierRawDeliveryRow
	if err := store.db.QueryRowContext(context.Background(), `
	SELECT status, retry_count, available_at, last_error, payload_json, updated_at, group_id, sequence_no, sequence_count
	FROM delivery_queue WHERE event_id=?`, eventID).Scan(
		&row.Status, &row.RetryCount, &row.AvailableAt, &row.LastError, &row.PayloadJSON, &row.UpdatedAt, &row.GroupID, &row.SequenceNo, &row.SequenceCount,
	); err != nil {
		t.Fatal(err)
	}
	return row
}

func openNotifierTestStore(t *testing.T) *Store {
	t.Helper()

	store, err := OpenWithProtector(filepath.Join(t.TempDir(), "state.sqlite"), securestore.NewDeterministicTestProtector())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}
