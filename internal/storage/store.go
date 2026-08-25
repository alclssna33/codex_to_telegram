package storage

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/alclssna33/codex_to_telegram/internal/model"
	"github.com/alclssna33/codex_to_telegram/internal/securestore"

	_ "modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

type Store struct {
	db        *sql.DB
	path      string
	protector securestore.Protector
}

const (
	MinimalApprovalPCCleanupQueueKind              = "minimal_approval_pc_cleanup"
	MinimalApprovalDeliveryOutcomeDefinitelyUnsent = "delivery_outcome=definitely_unsent"
	MinimalApprovalDeliveryOutcomeUnknown          = "delivery_outcome=unknown"
	protectedPayloadEnvelopePrefix                 = "dpapi:v1:"
)

type MinimalApproval struct {
	RequestID         string
	WireRequestID     string
	ThreadID          string
	TurnID            string
	RequestKind       string
	ProjectName       string
	SessionIdentity   string
	SupersedeEventID  string
	Status            string
	ClaimState        string
	ClaimAction       string
	ChatID            int64
	TopicID           int64
	TelegramMessageID int64
	DeliveryQueueID   int64
	UpdatedAt         model.TimeString
}

type MinimalApprovalRoute struct {
	Token             string
	Action            string
	RequestID         string
	WireRequestID     string
	ThreadID          string
	TurnID            string
	RequestKind       string
	SessionIdentity   string
	Status            string
	ChatID            int64
	TopicID           int64
	TelegramMessageID int64
	CreatedAt         model.TimeString
}

type MinimalApprovalSeed struct {
	Approval                 MinimalApproval
	ApproveToken             string
	DenyToken                string
	Delivery                 model.DeliveryQueueItem
	SupersedeDeliveryEventID string
}

func Open(path string) (*Store, error) {
	return open(path, nil)
}

func OpenWithProtector(path string, protector securestore.Protector) (*Store, error) {
	if protector == nil {
		return nil, errors.New("securestore protector is required")
	}
	return open(path, protector)
}

func open(path string, protector securestore.Protector) (*Store, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	store := &Store{db: db, path: path, protector: protector}
	if err := store.initialize(context.Background()); err != nil {
		_ = db.Close()
		return nil, err
	}
	if protector != nil {
		if err := store.migrateProtectedDeliveryPayloads(context.Background()); err != nil {
			_ = db.Close()
			return nil, err
		}
		if err := store.migrateProtectedPendingAndCallbackPayloads(context.Background()); err != nil {
			_ = db.Close()
			return nil, err
		}
	}
	return store, nil
}

func (s *Store) Path() string {
	return s.path
}

func (s *Store) Close() error {
	return s.db.Close()
}

func (s *Store) initialize(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, `PRAGMA journal_mode=WAL`); err != nil {
		return err
	}
	if _, err := s.db.ExecContext(ctx, `PRAGMA foreign_keys=ON`); err != nil {
		return err
	}
	if _, err := s.db.ExecContext(ctx, `PRAGMA busy_timeout=5000`); err != nil {
		return err
	}
	schema := `
	CREATE TABLE IF NOT EXISTS threads (
		thread_id TEXT PRIMARY KEY,
		title TEXT NOT NULL,
		cwd TEXT,
		project_name TEXT NOT NULL,
		directory_name TEXT,
		updated_at INTEGER NOT NULL,
		status TEXT,
		last_preview TEXT,
		active_turn_id TEXT,
		preferred_model TEXT,
		permissions_mode TEXT,
		archived INTEGER NOT NULL DEFAULT 0,
		raw_json TEXT NOT NULL
	);

	CREATE TABLE IF NOT EXISTS thread_snapshots (
		thread_id TEXT PRIMARY KEY,
		last_live_event_at TEXT,
		last_poll_at TEXT,
		next_poll_after TEXT,
		last_seen_thread_status TEXT,
		last_seen_turn_id TEXT,
		last_seen_turn_status TEXT,
		last_progress_fp TEXT,
		last_progress_sent_at TEXT,
		last_final_fp TEXT,
		last_completion_fp TEXT,
		last_approval_fp TEXT,
		snapshot_json TEXT NOT NULL,
		updated_at TEXT NOT NULL
	);

	CREATE TABLE IF NOT EXISTS terminal_events (
		terminal_key TEXT PRIMARY KEY,
		thread_id TEXT NOT NULL,
		turn_id TEXT NOT NULL,
		status TEXT NOT NULL,
		delivery_status TEXT NOT NULL,
		updated_at TEXT NOT NULL
	);

	CREATE TABLE IF NOT EXISTS thread_bindings (
		chat_key TEXT PRIMARY KEY,
		chat_id INTEGER NOT NULL,
		topic_id INTEGER NOT NULL,
		thread_id TEXT NOT NULL,
		mode TEXT NOT NULL,
		created_at TEXT NOT NULL,
		updated_at TEXT NOT NULL
	);

	CREATE TABLE IF NOT EXISTS observer_targets (
		chat_key TEXT PRIMARY KEY,
		chat_id INTEGER NOT NULL,
		topic_id INTEGER NOT NULL,
		enabled INTEGER NOT NULL,
		created_at TEXT NOT NULL,
		updated_at TEXT NOT NULL
	);

	CREATE TABLE IF NOT EXISTS telegram_message_routes (
		chat_id INTEGER NOT NULL,
		topic_id INTEGER NOT NULL,
		message_id INTEGER NOT NULL,
		thread_id TEXT NOT NULL,
		turn_id TEXT,
		item_id TEXT,
		event_id TEXT,
		created_at TEXT NOT NULL,
		PRIMARY KEY(chat_id, topic_id, message_id)
	);

	CREATE TABLE IF NOT EXISTS callback_routes (
		route_token TEXT PRIMARY KEY,
		action TEXT NOT NULL,
		thread_id TEXT NOT NULL,
		turn_id TEXT,
		request_id TEXT,
		session_identity TEXT NOT NULL DEFAULT '',
		telegram_message_id INTEGER,
		status TEXT NOT NULL,
		expires_at TEXT,
		payload_json TEXT NOT NULL,
		created_at TEXT NOT NULL
	);

	CREATE TABLE IF NOT EXISTS pending_approvals (
		request_id TEXT PRIMARY KEY,
		wire_request_id TEXT NOT NULL DEFAULT '',
		thread_id TEXT NOT NULL,
		turn_id TEXT,
		item_id TEXT,
		prompt_kind TEXT NOT NULL,
		request_kind TEXT NOT NULL DEFAULT '',
		question TEXT,
		session_identity TEXT NOT NULL DEFAULT '',
		status TEXT NOT NULL,
		telegram_message_id INTEGER,
		payload_json TEXT NOT NULL,
		updated_at TEXT NOT NULL
	);

	CREATE TABLE IF NOT EXISTS minimal_approvals (
		request_id TEXT PRIMARY KEY,
		wire_request_id TEXT NOT NULL DEFAULT '',
		thread_id TEXT NOT NULL,
		turn_id TEXT NOT NULL,
		request_kind TEXT NOT NULL,
		project_name TEXT NOT NULL,
		session_identity TEXT NOT NULL DEFAULT '',
		supersede_event_id TEXT NOT NULL DEFAULT '',
		status TEXT NOT NULL,
		claim_state TEXT NOT NULL DEFAULT 'idle',
		claim_action TEXT,
		chat_id INTEGER NOT NULL DEFAULT 0,
		topic_id INTEGER NOT NULL DEFAULT 0,
		telegram_message_id INTEGER NOT NULL DEFAULT 0,
		delivery_queue_id INTEGER NOT NULL DEFAULT 0,
		updated_at TEXT NOT NULL
	);

	CREATE TABLE IF NOT EXISTS minimal_approval_routes (
		route_token TEXT PRIMARY KEY,
		action TEXT NOT NULL,
		request_id TEXT NOT NULL,
		thread_id TEXT NOT NULL,
		turn_id TEXT NOT NULL,
		request_kind TEXT NOT NULL,
		status TEXT NOT NULL,
		chat_id INTEGER NOT NULL DEFAULT 0,
		topic_id INTEGER NOT NULL DEFAULT 0,
		telegram_message_id INTEGER NOT NULL DEFAULT 0,
		created_at TEXT NOT NULL,
		FOREIGN KEY(request_id) REFERENCES minimal_approvals(request_id)
	);

	CREATE TABLE IF NOT EXISTS delivery_queue (
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
		payload_json TEXT,
		created_at TEXT NOT NULL,
		updated_at TEXT NOT NULL,
		group_id TEXT,
		sequence_no INTEGER NOT NULL DEFAULT 1,
		sequence_count INTEGER NOT NULL DEFAULT 1
	);

	CREATE UNIQUE INDEX IF NOT EXISTS idx_delivery_queue_event_target
		ON delivery_queue(event_id, chat_key);

	CREATE TABLE IF NOT EXISTS delivery_attempts (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		queue_id INTEGER NOT NULL,
		attempt_no INTEGER NOT NULL,
		status TEXT NOT NULL,
		error_text TEXT,
		created_at TEXT NOT NULL
	);

	CREATE TABLE IF NOT EXISTS daemon_state (
		key TEXT PRIMARY KEY,
		value TEXT NOT NULL,
		updated_at TEXT NOT NULL
	);

	CREATE TABLE IF NOT EXISTS thread_panels (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		chat_id INTEGER NOT NULL,
		topic_id INTEGER NOT NULL,
		project_name TEXT NOT NULL,
		thread_id TEXT NOT NULL,
		source_mode TEXT NOT NULL DEFAULT 'explicit',
		summary_message_id INTEGER NOT NULL DEFAULT 0,
		tool_message_id INTEGER NOT NULL DEFAULT 0,
		output_message_id INTEGER NOT NULL DEFAULT 0,
		current_turn_id TEXT,
		status TEXT,
		archive_enabled INTEGER NOT NULL DEFAULT 1,
		last_summary_hash TEXT,
		last_tool_hash TEXT,
		last_output_hash TEXT,
		last_final_notice_fp TEXT,
		run_notice_message_id INTEGER NOT NULL DEFAULT 0,
		last_run_notice_fp TEXT,
		user_message_id INTEGER NOT NULL DEFAULT 0,
		last_user_notice_fp TEXT,
		plan_prompt_message_id INTEGER NOT NULL DEFAULT 0,
		last_plan_prompt_fp TEXT,
		details_view_json TEXT,
		last_final_card_hash TEXT,
		is_current INTEGER NOT NULL DEFAULT 1,
		created_at TEXT NOT NULL,
		updated_at TEXT NOT NULL
	);

	CREATE TABLE IF NOT EXISTS chat_steer_state (
		chat_key TEXT PRIMARY KEY,
		chat_id INTEGER NOT NULL,
		topic_id INTEGER NOT NULL,
		thread_id TEXT NOT NULL,
		turn_id TEXT,
		panel_id INTEGER NOT NULL DEFAULT 0,
		expires_at TEXT NOT NULL,
		created_at TEXT NOT NULL,
		updated_at TEXT NOT NULL
	);

	CREATE INDEX IF NOT EXISTS idx_threads_updated_at ON threads(updated_at DESC);
	CREATE INDEX IF NOT EXISTS idx_threads_project_updated_at ON threads(project_name, updated_at DESC);
	CREATE INDEX IF NOT EXISTS idx_bindings_thread_id ON thread_bindings(thread_id);
	CREATE INDEX IF NOT EXISTS idx_observer_targets_enabled_updated_at ON observer_targets(enabled, updated_at DESC);
	CREATE INDEX IF NOT EXISTS idx_delivery_queue_status_available_at ON delivery_queue(status, available_at);
	CREATE INDEX IF NOT EXISTS idx_pending_approvals_status_updated_at ON pending_approvals(status, updated_at DESC);
	CREATE INDEX IF NOT EXISTS idx_minimal_approvals_status_updated_at ON minimal_approvals(status, updated_at DESC);
	CREATE INDEX IF NOT EXISTS idx_minimal_approval_routes_request ON minimal_approval_routes(request_id, status);
	CREATE INDEX IF NOT EXISTS idx_thread_panels_thread_current ON thread_panels(chat_id, topic_id, thread_id, is_current, updated_at DESC);
	CREATE INDEX IF NOT EXISTS idx_chat_steer_expires_at ON chat_steer_state(expires_at);
	`
	if _, err := s.db.ExecContext(ctx, schema); err != nil {
		return err
	}
	if err := s.ensureColumn(ctx, "thread_panels", "source_mode", `ALTER TABLE thread_panels ADD COLUMN source_mode TEXT NOT NULL DEFAULT 'explicit'`); err != nil {
		return err
	}
	if err := s.ensureColumn(ctx, "thread_panels", "last_final_notice_fp", `ALTER TABLE thread_panels ADD COLUMN last_final_notice_fp TEXT`); err != nil {
		return err
	}
	if err := s.ensureColumn(ctx, "thread_panels", "run_notice_message_id", `ALTER TABLE thread_panels ADD COLUMN run_notice_message_id INTEGER NOT NULL DEFAULT 0`); err != nil {
		return err
	}
	if err := s.ensureColumn(ctx, "thread_panels", "last_run_notice_fp", `ALTER TABLE thread_panels ADD COLUMN last_run_notice_fp TEXT`); err != nil {
		return err
	}
	if err := s.ensureColumn(ctx, "thread_panels", "user_message_id", `ALTER TABLE thread_panels ADD COLUMN user_message_id INTEGER NOT NULL DEFAULT 0`); err != nil {
		return err
	}
	if err := s.ensureColumn(ctx, "thread_panels", "last_user_notice_fp", `ALTER TABLE thread_panels ADD COLUMN last_user_notice_fp TEXT`); err != nil {
		return err
	}
	if err := s.ensureColumn(ctx, "thread_panels", "plan_prompt_message_id", `ALTER TABLE thread_panels ADD COLUMN plan_prompt_message_id INTEGER NOT NULL DEFAULT 0`); err != nil {
		return err
	}
	if err := s.ensureColumn(ctx, "thread_panels", "last_plan_prompt_fp", `ALTER TABLE thread_panels ADD COLUMN last_plan_prompt_fp TEXT`); err != nil {
		return err
	}
	if err := s.ensureColumn(ctx, "thread_panels", "details_view_json", `ALTER TABLE thread_panels ADD COLUMN details_view_json TEXT`); err != nil {
		return err
	}
	if err := s.ensureColumn(ctx, "thread_panels", "last_final_card_hash", `ALTER TABLE thread_panels ADD COLUMN last_final_card_hash TEXT`); err != nil {
		return err
	}
	for _, column := range []struct{ name, sql string }{
		{"group_id", `ALTER TABLE delivery_queue ADD COLUMN group_id TEXT`},
		{"sequence_no", `ALTER TABLE delivery_queue ADD COLUMN sequence_no INTEGER NOT NULL DEFAULT 1`},
		{"sequence_count", `ALTER TABLE delivery_queue ADD COLUMN sequence_count INTEGER NOT NULL DEFAULT 1`},
	} {
		if err := s.ensureColumn(ctx, "delivery_queue", column.name, column.sql); err != nil {
			return err
		}
	}
	if err := s.ensureColumn(ctx, "minimal_approvals", "session_identity", `ALTER TABLE minimal_approvals ADD COLUMN session_identity TEXT NOT NULL DEFAULT ''`); err != nil {
		return err
	}
	if err := s.ensureColumn(ctx, "minimal_approvals", "wire_request_id", `ALTER TABLE minimal_approvals ADD COLUMN wire_request_id TEXT NOT NULL DEFAULT ''`); err != nil {
		return err
	}
	if err := s.ensureColumn(ctx, "minimal_approvals", "supersede_event_id", `ALTER TABLE minimal_approvals ADD COLUMN supersede_event_id TEXT NOT NULL DEFAULT ''`); err != nil {
		return err
	}
	if err := s.ensureColumn(ctx, "callback_routes", "session_identity", `ALTER TABLE callback_routes ADD COLUMN session_identity TEXT NOT NULL DEFAULT ''`); err != nil {
		return err
	}
	if err := s.ensureColumn(ctx, "pending_approvals", "session_identity", `ALTER TABLE pending_approvals ADD COLUMN session_identity TEXT NOT NULL DEFAULT ''`); err != nil {
		return err
	}
	if err := s.ensureColumn(ctx, "pending_approvals", "wire_request_id", `ALTER TABLE pending_approvals ADD COLUMN wire_request_id TEXT NOT NULL DEFAULT ''`); err != nil {
		return err
	}
	if err := s.ensureColumn(ctx, "pending_approvals", "request_kind", `ALTER TABLE pending_approvals ADD COLUMN request_kind TEXT NOT NULL DEFAULT ''`); err != nil {
		return err
	}
	if err := s.ensureMinimalObservationDiscoverySequence(ctx); err != nil {
		return err
	}
	return nil
}

func (s *Store) ensureColumn(ctx context.Context, tableName, columnName, alterSQL string) error {
	exists, err := s.hasColumn(ctx, tableName, columnName)
	if err != nil {
		return err
	}
	if exists {
		return nil
	}
	if _, err := s.db.ExecContext(ctx, alterSQL); err != nil {
		return err
	}
	exists, err = s.hasColumn(ctx, tableName, columnName)
	if err != nil {
		return err
	}
	if !exists {
		return fmt.Errorf("column %s.%s was not created", tableName, columnName)
	}
	return nil
}

func (s *Store) migrateProtectedDeliveryPayloads(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, `PRAGMA secure_delete=ON`); err != nil {
		return fmt.Errorf("enable sqlite secure delete: %w", err)
	}
	needed, err := s.protectedDeliveryPayloadMigrationNeeded(ctx)
	if err != nil {
		return err
	}
	if !needed {
		return nil
	}
	if err := checkpointWAL(ctx, s.db); err != nil {
		return fmt.Errorf("checkpoint sqlite before protected payload migration: %w", err)
	}

	payloadNotNull, err := s.deliveryPayloadNotNull(ctx)
	if err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer rollback(tx)
	changed := payloadNotNull
	if payloadNotNull {
		if err := rebuildDeliveryQueueWithNullablePayload(ctx, tx); err != nil {
			return err
		}
	}

	rows, err := tx.QueryContext(ctx, `
	SELECT id, kind, status, payload_json FROM delivery_queue
	WHERE status IN ('pending', 'retry', 'processing') AND payload_json IS NOT NULL`)
	if err != nil {
		return err
	}
	type livePayload struct {
		id      int64
		kind    string
		status  string
		payload string
	}
	var live []livePayload
	for rows.Next() {
		var item livePayload
		if err := rows.Scan(&item.id, &item.kind, &item.status, &item.payload); err != nil {
			_ = rows.Close()
			return err
		}
		live = append(live, item)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, item := range live {
		if item.kind == "minimal_approval" && item.status == model.DeliveryStatusProcessing {
			if _, err := tx.ExecContext(ctx, `UPDATE delivery_queue SET status=?,payload_json=NULL,updated_at=? WHERE id=?`, model.DeliveryStatusDead, string(model.NowString()), item.id); err != nil {
				return err
			}
			changed = true
			continue
		}
		alreadyProtected := strings.HasPrefix(item.payload, protectedPayloadEnvelopePrefix)
		if alreadyProtected {
			if _, err := s.protector.Unprotect(ctx, item.payload); err != nil {
				return fmt.Errorf("validate protected delivery payload: %w", err)
			}
		}
		if alreadyProtected && item.status != model.DeliveryStatusProcessing {
			continue
		}
		protected := item.payload
		if !alreadyProtected {
			protected, err = s.protector.Protect(ctx, []byte(item.payload))
			if err != nil {
				return fmt.Errorf("protect migrated delivery payload: %w", err)
			}
		}
		status := item.status
		if status == model.DeliveryStatusProcessing {
			status = model.DeliveryStatusRetry
		}
		if _, err := tx.ExecContext(ctx, `UPDATE delivery_queue SET status = ?, payload_json = ? WHERE id = ?`, status, protected, item.id); err != nil {
			return err
		}
		changed = true
	}
	result, err := tx.ExecContext(ctx, `
	UPDATE delivery_queue SET payload_json = NULL
	WHERE status IN ('delivered', 'dead') AND payload_json IS NOT NULL`)
	if err != nil {
		return err
	}
	terminalRows, err := result.RowsAffected()
	if err != nil {
		return err
	}
	changed = changed || terminalRows > 0
	if err := tx.Commit(); err != nil {
		return err
	}
	if !changed {
		return nil
	}
	return s.compactMigratedPayloads(ctx)
}

func (s *Store) migrateProtectedPendingAndCallbackPayloads(ctx context.Context) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer rollback(tx)
	changed := false
	pendingRows, err := tx.QueryContext(ctx, `SELECT request_id, coalesce(question,''), payload_json FROM pending_approvals`)
	if err != nil {
		return err
	}
	type pendingSensitivePayload struct {
		requestID string
		question  string
		payload   string
	}
	var pending []pendingSensitivePayload
	for pendingRows.Next() {
		var row pendingSensitivePayload
		if err := pendingRows.Scan(&row.requestID, &row.question, &row.payload); err != nil {
			_ = pendingRows.Close()
			return err
		}
		pending = append(pending, row)
	}
	if err := pendingRows.Err(); err != nil {
		_ = pendingRows.Close()
		return err
	}
	if err := pendingRows.Close(); err != nil {
		return err
	}
	for _, row := range pending {
		protectedQuestion, questionChanged, err := s.protectSensitiveText(ctx, row.question)
		if err != nil {
			return fmt.Errorf("protect migrated pending question: %w", err)
		}
		protectedPayload, payloadChanged, err := s.protectSensitiveText(ctx, row.payload)
		if err != nil {
			return fmt.Errorf("protect migrated pending payload: %w", err)
		}
		if !questionChanged && !payloadChanged {
			continue
		}
		if _, err := tx.ExecContext(ctx, `UPDATE pending_approvals SET question=?, payload_json=? WHERE request_id=?`, nullable(protectedQuestion), protectedPayload, row.requestID); err != nil {
			return err
		}
		changed = true
	}
	callbackRows, err := tx.QueryContext(ctx, `SELECT route_token, payload_json FROM callback_routes`)
	if err != nil {
		return err
	}
	type callbackSensitivePayload struct {
		token   string
		payload string
	}
	var callbacks []callbackSensitivePayload
	for callbackRows.Next() {
		var row callbackSensitivePayload
		if err := callbackRows.Scan(&row.token, &row.payload); err != nil {
			_ = callbackRows.Close()
			return err
		}
		callbacks = append(callbacks, row)
	}
	if err := callbackRows.Err(); err != nil {
		_ = callbackRows.Close()
		return err
	}
	if err := callbackRows.Close(); err != nil {
		return err
	}
	for _, row := range callbacks {
		protectedPayload, payloadChanged, err := s.protectSensitiveText(ctx, row.payload)
		if err != nil {
			return fmt.Errorf("protect migrated callback payload: %w", err)
		}
		if !payloadChanged {
			continue
		}
		if _, err := tx.ExecContext(ctx, `UPDATE callback_routes SET payload_json=? WHERE route_token=?`, protectedPayload, row.token); err != nil {
			return err
		}
		changed = true
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	if !changed {
		return nil
	}
	return s.compactMigratedPayloads(ctx)
}

func (s *Store) protectSensitiveText(ctx context.Context, value string) (string, bool, error) {
	if s.protector == nil || strings.TrimSpace(value) == "" {
		return value, false, nil
	}
	if strings.HasPrefix(value, protectedPayloadEnvelopePrefix) {
		if _, err := s.protector.Unprotect(ctx, value); err != nil {
			return "", false, err
		}
		return value, false, nil
	}
	protected, err := s.protector.Protect(ctx, []byte(value))
	if err != nil {
		return "", false, err
	}
	return protected, true, nil
}

func (s *Store) unprotectSensitiveText(ctx context.Context, value string) (string, error) {
	if s.protector == nil || strings.TrimSpace(value) == "" || !strings.HasPrefix(value, protectedPayloadEnvelopePrefix) {
		return value, nil
	}
	plaintext, err := s.protector.Unprotect(ctx, value)
	if err != nil {
		return "", err
	}
	return string(plaintext), nil
}

type protectedDeliveryPayload struct {
	id      int64
	kind    string
	status  string
	payload string
}

func (s *Store) protectedDeliveryPayloadMigrationNeeded(ctx context.Context) (bool, error) {
	payloadNotNull, err := s.deliveryPayloadNotNull(ctx)
	if err != nil {
		return false, err
	}
	needed := payloadNotNull
	rows, err := s.db.QueryContext(ctx, `
	SELECT id, kind, status, payload_json FROM delivery_queue
	WHERE status IN ('pending', 'retry', 'processing') AND payload_json IS NOT NULL`)
	if err != nil {
		return false, err
	}
	for rows.Next() {
		var item protectedDeliveryPayload
		if err := rows.Scan(&item.id, &item.kind, &item.status, &item.payload); err != nil {
			_ = rows.Close()
			return false, err
		}
		migrationNeeded, err := s.protectedDeliveryPayloadNeedsMigration(ctx, item)
		if err != nil {
			_ = rows.Close()
			return false, err
		}
		needed = needed || migrationNeeded
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return false, err
	}
	if err := rows.Close(); err != nil {
		return false, err
	}
	var terminalRows int
	if err := s.db.QueryRowContext(ctx, `
	SELECT count(*) FROM delivery_queue
	WHERE status IN ('delivered', 'dead') AND payload_json IS NOT NULL`).Scan(&terminalRows); err != nil {
		return false, err
	}
	return needed || terminalRows > 0, nil
}

func (s *Store) protectedDeliveryPayloadNeedsMigration(ctx context.Context, item protectedDeliveryPayload) (bool, error) {
	if item.kind == "minimal_approval" && item.status == model.DeliveryStatusProcessing {
		return true, nil
	}
	if strings.HasPrefix(item.payload, protectedPayloadEnvelopePrefix) {
		if _, err := s.protector.Unprotect(ctx, item.payload); err != nil {
			return false, fmt.Errorf("validate protected delivery payload: %w", err)
		}
		return item.status == model.DeliveryStatusProcessing, nil
	}
	return true, nil
}

func (s *Store) deliveryPayloadNotNull(ctx context.Context) (bool, error) {
	rows, err := s.db.QueryContext(ctx, `PRAGMA table_info(delivery_queue)`)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	for rows.Next() {
		var cid, notNull, pk int
		var name, columnType string
		var defaultValue sql.NullString
		if err := rows.Scan(&cid, &name, &columnType, &notNull, &defaultValue, &pk); err != nil {
			return false, err
		}
		if name == "payload_json" {
			return notNull == 1, nil
		}
	}
	return false, rows.Err()
}

func rebuildDeliveryQueueWithNullablePayload(ctx context.Context, tx *sql.Tx) error {
	if _, err := tx.ExecContext(ctx, `DROP TABLE IF EXISTS delivery_queue_secure_new`); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `
	CREATE TABLE delivery_queue_secure_new (
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
		payload_json TEXT,
		created_at TEXT NOT NULL,
		updated_at TEXT NOT NULL,
		group_id TEXT,
		sequence_no INTEGER NOT NULL DEFAULT 1,
		sequence_count INTEGER NOT NULL DEFAULT 1
	)`); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `
	INSERT INTO delivery_queue_secure_new(
		id, event_id, chat_key, chat_id, topic_id, thread_id, kind, status,
		retry_count, available_at, last_error, payload_json, created_at, updated_at, group_id, sequence_no, sequence_count
	)
	SELECT id, event_id, chat_key, chat_id, topic_id, thread_id, kind, status,
		retry_count, available_at, last_error, payload_json, created_at, updated_at, group_id, sequence_no, sequence_count
	FROM delivery_queue`); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DROP TABLE delivery_queue`); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `ALTER TABLE delivery_queue_secure_new RENAME TO delivery_queue`); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `
	CREATE UNIQUE INDEX idx_delivery_queue_event_target ON delivery_queue(event_id, chat_key)`); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `
	CREATE INDEX idx_delivery_queue_status_available_at ON delivery_queue(status, available_at)`)
	return err
}

func (s *Store) compactMigratedPayloads(ctx context.Context) error {
	if err := checkpointWAL(ctx, s.db); err != nil {
		return fmt.Errorf("checkpoint migrated sqlite payloads: %w", err)
	}
	if err := setJournalMode(ctx, s.db, "DELETE"); err != nil {
		return fmt.Errorf("select sqlite delete journal for compaction: %w", err)
	}
	if _, err := s.db.ExecContext(ctx, `VACUUM`); err != nil {
		return fmt.Errorf("compact migrated sqlite payloads: %w", err)
	}
	if err := setJournalMode(ctx, s.db, "WAL"); err != nil {
		return fmt.Errorf("restore sqlite wal mode: %w", err)
	}
	return nil
}

var errSQLiteCheckpointBusy = errors.New("sqlite wal checkpoint busy")

func checkpointWAL(ctx context.Context, db *sql.DB) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, 2*time.Second)
		defer cancel()
	}
	restoreBusyTimeout, err := withCheckpointBusyTimeout(ctx, db, 50)
	if err != nil {
		return err
	}
	defer restoreBusyTimeout()
	var lastBusy error
	for attempt := 0; ; attempt++ {
		if err := ctx.Err(); err != nil {
			if lastBusy != nil {
				return fmt.Errorf("sqlite wal checkpoint remained busy before deadline: %v: %w", lastBusy, err)
			}
			return err
		}
		err := checkpointWALOnce(ctx, db)
		if err == nil {
			return nil
		}
		if !checkpointBusyError(err) {
			return err
		}
		lastBusy = err
		delay := time.Duration(10+attempt*10) * time.Millisecond
		if delay > 100*time.Millisecond {
			delay = 100 * time.Millisecond
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return fmt.Errorf("sqlite wal checkpoint remained busy before deadline: %v: %w", lastBusy, ctx.Err())
		case <-timer.C:
		}
	}
}

func withCheckpointBusyTimeout(ctx context.Context, db *sql.DB, timeoutMS int) (func(), error) {
	var previous int
	if err := db.QueryRowContext(ctx, `PRAGMA busy_timeout`).Scan(&previous); err != nil {
		return nil, err
	}
	if _, err := db.ExecContext(ctx, fmt.Sprintf(`PRAGMA busy_timeout=%d`, timeoutMS)); err != nil {
		return nil, err
	}
	return func() {
		restoreCtx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
		defer cancel()
		_, _ = db.ExecContext(restoreCtx, fmt.Sprintf(`PRAGMA busy_timeout=%d`, previous))
	}, nil
}

func checkpointWALOnce(ctx context.Context, db *sql.DB) error {
	var busy, logFrames, checkpointedFrames int
	if err := db.QueryRowContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`).Scan(&busy, &logFrames, &checkpointedFrames); err != nil {
		return err
	}
	if busy != 0 {
		return errSQLiteCheckpointBusy
	}
	return nil
}

type sqliteErrorCoder interface {
	Code() int
}

func checkpointBusyError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, errSQLiteCheckpointBusy) {
		return true
	}
	var sqliteErr sqliteErrorCoder
	if !errors.As(err, &sqliteErr) {
		return false
	}
	return sqliteErr.Code()&0xff == sqlite3.SQLITE_BUSY
}

func setJournalMode(ctx context.Context, db *sql.DB, mode string) error {
	var pragma string
	switch strings.ToUpper(mode) {
	case "DELETE":
		pragma = `PRAGMA journal_mode=DELETE`
	case "WAL":
		pragma = `PRAGMA journal_mode=WAL`
	default:
		return fmt.Errorf("unsupported sqlite journal mode %s", mode)
	}
	var selected string
	if err := db.QueryRowContext(ctx, pragma).Scan(&selected); err != nil {
		return err
	}
	if !strings.EqualFold(selected, mode) {
		return fmt.Errorf("sqlite selected journal mode %s, want %s", selected, mode)
	}
	return nil
}

func (s *Store) hasColumn(ctx context.Context, tableName, columnName string) (bool, error) {
	rows, err := s.db.QueryContext(ctx, fmt.Sprintf(`PRAGMA table_info(%s)`, tableName))
	if err != nil {
		return false, err
	}
	defer rows.Close()
	for rows.Next() {
		var cid int
		var name, columnType string
		var notNull, pk int
		var defaultValue sql.NullString
		if err := rows.Scan(&cid, &name, &columnType, &notNull, &defaultValue, &pk); err != nil {
			return false, err
		}
		if strings.EqualFold(name, columnName) {
			return true, nil
		}
	}
	return false, rows.Err()
}

func (s *Store) UpsertThread(ctx context.Context, thread model.Thread) error {
	raw := thread.Raw
	if len(raw) == 0 {
		raw = []byte("{}")
	}
	_, err := s.db.ExecContext(ctx, `
	INSERT INTO threads(thread_id, title, cwd, project_name, directory_name, updated_at, status, last_preview, active_turn_id, preferred_model, permissions_mode, archived, raw_json)
	VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	ON CONFLICT(thread_id) DO UPDATE SET
		title = excluded.title,
		cwd = excluded.cwd,
		project_name = excluded.project_name,
		directory_name = excluded.directory_name,
		updated_at = excluded.updated_at,
		status = excluded.status,
		last_preview = excluded.last_preview,
		active_turn_id = excluded.active_turn_id,
		preferred_model = excluded.preferred_model,
		permissions_mode = excluded.permissions_mode,
		archived = excluded.archived,
		raw_json = excluded.raw_json`,
		thread.ID, thread.Title, nullable(thread.CWD), thread.ProjectName, nullable(thread.DirectoryName), thread.UpdatedAt,
		nullable(thread.Status), nullable(thread.LastPreview), nullable(thread.ActiveTurnID), nullable(thread.PreferredModel),
		nullable(thread.PermissionsMode), boolToInt(thread.Archived), string(raw),
	)
	return err
}

func (s *Store) GetThread(ctx context.Context, threadID string) (*model.Thread, error) {
	row := s.db.QueryRowContext(ctx, `
	SELECT thread_id, title, cwd, project_name, directory_name, updated_at, status, last_preview, active_turn_id, preferred_model, permissions_mode, archived, raw_json
	FROM threads WHERE thread_id = ?`, threadID)
	return scanThread(row)
}

func (s *Store) ListThreads(ctx context.Context, limit int, search string) ([]model.Thread, error) {
	if limit <= 0 {
		limit = 10
	}
	query := `
	SELECT thread_id, title, cwd, project_name, directory_name, updated_at, status, last_preview, active_turn_id, preferred_model, permissions_mode, archived, raw_json
	FROM threads WHERE ` + visibleThreadPredicateSQL
	args := make([]any, 0, 2)
	if trimmed := strings.TrimSpace(search); trimmed != "" {
		query += ` AND (lower(title) LIKE ? OR lower(project_name) LIKE ? OR lower(last_preview) LIKE ? OR lower(thread_id) LIKE ?)`
		pattern := "%" + strings.ToLower(trimmed) + "%"
		args = append(args, pattern, pattern, pattern, pattern)
	}
	query += ` ORDER BY updated_at DESC LIMIT ?`
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []model.Thread{}
	for rows.Next() {
		thread, err := scanThread(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *thread)
	}
	return out, rows.Err()
}

const visibleThreadPredicateSQL = `(
	lower(trim(cast(coalesce(json_extract(raw_json, '$.ephemeral'), json_extract(raw_json, '$.thread.ephemeral'), '') as text))) NOT IN ('1', 'true', 'yes')
	AND trim(cast(coalesce(json_extract(raw_json, '$.source.subAgent'), json_extract(raw_json, '$.thread.source.subAgent'), '') as text)) = ''
)`

func (s *Store) CountThreads(ctx context.Context) (int, error) {
	row := s.db.QueryRowContext(ctx, `SELECT count(*) FROM threads`)
	var count int
	return count, row.Scan(&count)
}

func (s *Store) ListProjectGroups(ctx context.Context) (map[string][]model.Thread, error) {
	rows, err := s.ListThreads(ctx, 500, "")
	if err != nil {
		return nil, err
	}
	grouped := map[string][]model.Thread{}
	for _, thread := range rows {
		grouped[thread.ProjectName] = append(grouped[thread.ProjectName], thread)
	}
	return grouped, nil
}

func (s *Store) UpsertSnapshot(ctx context.Context, threadID string, snapshot model.ThreadSnapshotState) error {
	payload, err := json.Marshal(snapshot)
	if err != nil {
		return err
	}
	updatedAt := string(model.NowString())
	_, err = s.db.ExecContext(ctx, `
	INSERT INTO thread_snapshots(
		thread_id, last_live_event_at, last_poll_at, next_poll_after, last_seen_thread_status, last_seen_turn_id, last_seen_turn_status,
		last_progress_fp, last_progress_sent_at, last_final_fp, last_completion_fp, last_approval_fp, snapshot_json, updated_at
	)
	VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	ON CONFLICT(thread_id) DO UPDATE SET
		last_live_event_at = excluded.last_live_event_at,
		last_poll_at = excluded.last_poll_at,
		next_poll_after = excluded.next_poll_after,
		last_seen_thread_status = excluded.last_seen_thread_status,
		last_seen_turn_id = excluded.last_seen_turn_id,
		last_seen_turn_status = excluded.last_seen_turn_status,
		last_progress_fp = excluded.last_progress_fp,
		last_progress_sent_at = excluded.last_progress_sent_at,
		last_final_fp = excluded.last_final_fp,
		last_completion_fp = excluded.last_completion_fp,
		last_approval_fp = excluded.last_approval_fp,
		snapshot_json = excluded.snapshot_json,
		updated_at = excluded.updated_at`,
		threadID,
		nullable(string(snapshot.LastRichLiveEventAt)),
		nullable(string(snapshot.LastPollAt)),
		nullable(string(snapshot.NextPollAfter)),
		nullable(snapshot.LastSeenThreadStatus),
		nullable(snapshot.LastSeenTurnID),
		nullable(snapshot.LastSeenTurnStatus),
		nullable(snapshot.LastProgressFP),
		nullable(string(snapshot.LastProgressSentAt)),
		nullable(snapshot.LastFinalFP),
		nullable(snapshot.LastCompletionFP),
		nullable(snapshot.LastApprovalFP),
		string(payload),
		updatedAt,
	)
	return err
}

func (s *Store) GetSnapshot(ctx context.Context, threadID string) (*model.ThreadSnapshotState, error) {
	row := s.db.QueryRowContext(ctx, `SELECT snapshot_json FROM thread_snapshots WHERE thread_id = ?`, threadID)
	var payload string
	if err := row.Scan(&payload); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	var snapshot model.ThreadSnapshotState
	if err := json.Unmarshal([]byte(payload), &snapshot); err != nil {
		return nil, err
	}
	return &snapshot, nil
}

func (s *Store) MarkLiveEvent(ctx context.Context, threadID string, when model.TimeString) error {
	snapshot, err := s.GetSnapshot(ctx, threadID)
	if err != nil {
		return err
	}
	if snapshot == nil {
		snapshot = &model.ThreadSnapshotState{}
	}
	snapshot.LastRichLiveEventAt = when
	return s.UpsertSnapshot(ctx, threadID, *snapshot)
}

func (s *Store) SetBinding(ctx context.Context, chatID, topicID int64, threadID, mode string) error {
	now := string(model.NowString())
	chatKey := model.ChatKey(chatID, topicID)
	_, err := s.db.ExecContext(ctx, `
	INSERT INTO thread_bindings(chat_key, chat_id, topic_id, thread_id, mode, created_at, updated_at)
	VALUES (?, ?, ?, ?, ?, ?, ?)
	ON CONFLICT(chat_key) DO UPDATE SET thread_id = excluded.thread_id, mode = excluded.mode, updated_at = excluded.updated_at`,
		chatKey, chatID, topicID, threadID, mode, now, now,
	)
	return err
}

func (s *Store) ClearBinding(ctx context.Context, chatID, topicID int64) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM thread_bindings WHERE chat_key = ?`, model.ChatKey(chatID, topicID))
	return err
}

func (s *Store) GetBinding(ctx context.Context, chatID, topicID int64) (*model.ThreadBinding, error) {
	row := s.db.QueryRowContext(ctx, `
	SELECT chat_key, chat_id, topic_id, thread_id, mode, created_at, updated_at
	FROM thread_bindings WHERE chat_key = ?`, model.ChatKey(chatID, topicID))
	var binding model.ThreadBinding
	err := row.Scan(&binding.ChatKey, &binding.ChatID, &binding.TopicID, &binding.ThreadID, &binding.Mode, &binding.CreatedAt, &binding.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &binding, nil
}

func (s *Store) ListBoundThreadIDs(ctx context.Context) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT DISTINCT thread_id FROM thread_bindings ORDER BY updated_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var threadID string
		if err := rows.Scan(&threadID); err != nil {
			return nil, err
		}
		out = append(out, threadID)
	}
	return out, rows.Err()
}

func (s *Store) SetObserverTarget(ctx context.Context, chatID, topicID int64, enabled bool) error {
	now := string(model.NowString())
	chatKey := model.ChatKey(chatID, topicID)
	_, err := s.db.ExecContext(ctx, `
	INSERT INTO observer_targets(chat_key, chat_id, topic_id, enabled, created_at, updated_at)
	VALUES (?, ?, ?, ?, ?, ?)
	ON CONFLICT(chat_key) DO UPDATE SET enabled = excluded.enabled, updated_at = excluded.updated_at`,
		chatKey, chatID, topicID, boolToInt(enabled), now, now,
	)
	return err
}

func (s *Store) IsObserverTarget(ctx context.Context, chatID, topicID int64) (bool, error) {
	row := s.db.QueryRowContext(ctx, `SELECT enabled FROM observer_targets WHERE chat_key = ?`, model.ChatKey(chatID, topicID))
	var enabled int
	err := row.Scan(&enabled)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return enabled == 1, err
}

func (s *Store) ListObserverTargets(ctx context.Context) ([]model.ObserverTarget, error) {
	rows, err := s.db.QueryContext(ctx, `
	SELECT chat_key, chat_id, topic_id, enabled, created_at, updated_at
	FROM observer_targets WHERE enabled = 1 ORDER BY updated_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []model.ObserverTarget{}
	for rows.Next() {
		var target model.ObserverTarget
		var enabled int
		if err := rows.Scan(&target.ChatKey, &target.ChatID, &target.TopicID, &enabled, &target.CreatedAt, &target.UpdatedAt); err != nil {
			return nil, err
		}
		target.Enabled = enabled == 1
		out = append(out, target)
	}
	return out, rows.Err()
}

func (s *Store) PutMessageRoute(ctx context.Context, route model.MessageRoute) error {
	_, err := s.db.ExecContext(ctx, `
	INSERT OR REPLACE INTO telegram_message_routes(chat_id, topic_id, message_id, thread_id, turn_id, item_id, event_id, created_at)
	VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		route.ChatID, route.TopicID, route.MessageID, route.ThreadID, nullable(route.TurnID), nullable(route.ItemID), nullable(route.EventID), route.CreatedAt,
	)
	return err
}

func (s *Store) ResolveMessageRoute(ctx context.Context, chatID, topicID, messageID int64) (*model.MessageRoute, error) {
	row := s.db.QueryRowContext(ctx, `
	SELECT chat_id, topic_id, message_id, thread_id, coalesce(turn_id, ''), coalesce(item_id, ''), coalesce(event_id, ''), created_at
	FROM telegram_message_routes WHERE chat_id = ? AND topic_id = ? AND message_id = ?`,
		chatID, topicID, messageID,
	)
	var route model.MessageRoute
	err := row.Scan(&route.ChatID, &route.TopicID, &route.MessageID, &route.ThreadID, &route.TurnID, &route.ItemID, &route.EventID, &route.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &route, nil
}

func (s *Store) ResolveMessageRouteByEvent(ctx context.Context, eventID string, chatID, topicID int64) (*model.MessageRoute, error) {
	eventID = strings.TrimSpace(eventID)
	if eventID == "" {
		return nil, nil
	}
	row := s.db.QueryRowContext(ctx, `
	SELECT chat_id, topic_id, message_id, thread_id, coalesce(turn_id, ''), coalesce(item_id, ''), coalesce(event_id, ''), created_at
	FROM telegram_message_routes WHERE event_id = ? AND chat_id = ? AND topic_id = ? ORDER BY message_id DESC LIMIT 1`,
		eventID, chatID, topicID,
	)
	var route model.MessageRoute
	err := row.Scan(&route.ChatID, &route.TopicID, &route.MessageID, &route.ThreadID, &route.TurnID, &route.ItemID, &route.EventID, &route.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &route, nil
}

func (s *Store) ResolveMessageRouteByEventIdentity(ctx context.Context, eventID, threadID, turnID string) (*model.MessageRoute, error) {
	eventID = strings.TrimSpace(eventID)
	threadID = strings.TrimSpace(threadID)
	turnID = strings.TrimSpace(turnID)
	if eventID == "" {
		return nil, nil
	}
	row := s.db.QueryRowContext(ctx, `
	SELECT chat_id, topic_id, message_id, thread_id, coalesce(turn_id, ''), coalesce(item_id, ''), coalesce(event_id, ''), created_at
	FROM telegram_message_routes
	WHERE event_id = ? AND (? = '' OR thread_id = ?) AND (? = '' OR coalesce(turn_id, '') = ?)
	ORDER BY created_at DESC, message_id DESC LIMIT 1`,
		eventID, threadID, threadID, turnID, turnID,
	)
	var route model.MessageRoute
	err := row.Scan(&route.ChatID, &route.TopicID, &route.MessageID, &route.ThreadID, &route.TurnID, &route.ItemID, &route.EventID, &route.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &route, nil
}

func (s *Store) PutCallbackRoute(ctx context.Context, route model.CallbackRoute) error {
	protectedPayload, _, err := s.protectSensitiveText(ctx, route.PayloadJSON)
	if err != nil {
		return fmt.Errorf("protect callback route payload: %w", err)
	}
	_, err = s.db.ExecContext(ctx, `
	INSERT OR REPLACE INTO callback_routes(route_token, action, thread_id, turn_id, request_id, session_identity, telegram_message_id, status, expires_at, payload_json, created_at)
	VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		route.Token, route.Action, route.ThreadID, nullable(route.TurnID), nullable(route.RequestID), strings.TrimSpace(route.SessionIdentity), route.TelegramMessageID, route.Status, nullable(route.ExpiresAt), protectedPayload, route.CreatedAt,
	)
	return err
}

func (s *Store) GetCallbackRoute(ctx context.Context, token string) (*model.CallbackRoute, error) {
	row := s.db.QueryRowContext(ctx, `
	SELECT route_token, action, thread_id, coalesce(turn_id,''), coalesce(request_id,''), coalesce(session_identity,''), coalesce(telegram_message_id,0), status, coalesce(expires_at,''), payload_json, created_at
	FROM callback_routes WHERE route_token = ?`, token)
	var route model.CallbackRoute
	err := row.Scan(&route.Token, &route.Action, &route.ThreadID, &route.TurnID, &route.RequestID, &route.SessionIdentity, &route.TelegramMessageID, &route.Status, &route.ExpiresAt, &route.PayloadJSON, &route.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	route.PayloadJSON, err = s.unprotectSensitiveText(ctx, route.PayloadJSON)
	if err != nil {
		return nil, fmt.Errorf("unprotect callback route payload: %w", err)
	}
	return &route, nil
}

func (s *Store) ExpireCallbackRoute(ctx context.Context, token string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE callback_routes SET status = ? WHERE route_token = ?`, model.CallbackStatusExpired, token)
	return err
}

func (s *Store) SavePendingApproval(ctx context.Context, approval model.PendingApproval) error {
	approval.SessionIdentity = strings.TrimSpace(approval.SessionIdentity)
	approval.RequestID, approval.WireRequestID = model.NormalizeRequestIdentity(approval.SessionIdentity, approval.RequestID, approval.WireRequestID)
	if strings.TrimSpace(approval.RequestKind) == "" {
		approval.RequestKind = approval.PromptKind
	}
	protectedQuestion, _, err := s.protectSensitiveText(ctx, approval.Question)
	if err != nil {
		return fmt.Errorf("protect pending approval question: %w", err)
	}
	protectedPayload, _, err := s.protectSensitiveText(ctx, approval.PayloadJSON)
	if err != nil {
		return fmt.Errorf("protect pending approval payload: %w", err)
	}
	_, err = s.db.ExecContext(ctx, `
	INSERT INTO pending_approvals(request_id, wire_request_id, thread_id, turn_id, item_id, prompt_kind, request_kind, question, session_identity, status, telegram_message_id, payload_json, updated_at)
	VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	ON CONFLICT(request_id) DO UPDATE SET
		wire_request_id = excluded.wire_request_id,
		thread_id = excluded.thread_id,
		turn_id = excluded.turn_id,
		item_id = excluded.item_id,
		prompt_kind = excluded.prompt_kind,
		request_kind = excluded.request_kind,
		question = excluded.question,
		status = excluded.status,
		session_identity = excluded.session_identity,
		telegram_message_id = excluded.telegram_message_id,
		payload_json = excluded.payload_json,
		updated_at = excluded.updated_at`,
		approval.RequestID, approval.WireRequestID, approval.ThreadID, nullable(approval.TurnID), nullable(approval.ItemID), approval.PromptKind, approval.RequestKind, nullable(protectedQuestion), approval.SessionIdentity, approval.Status,
		approval.TelegramMessageID, protectedPayload, approval.UpdatedAt,
	)
	return err
}

func resolvePendingApprovalRequestID(ctx context.Context, q minimalApprovalRequestLookup, requestID string) (string, bool, error) {
	return resolveApprovalRequestID(ctx, q, "pending_approvals", requestID, "", true)
}

func resolvePendingApprovalRequestIDForSession(ctx context.Context, q minimalApprovalRequestLookup, requestID, sessionIdentity string) (string, bool, error) {
	return resolveApprovalRequestID(ctx, q, "pending_approvals", requestID, sessionIdentity, false)
}

func (s *Store) GetPendingApproval(ctx context.Context, requestID string) (*model.PendingApproval, error) {
	resolvedID, ok, err := resolvePendingApprovalRequestID(ctx, s.db, requestID)
	if err != nil || !ok {
		return nil, err
	}
	return s.getPendingApprovalByResolvedID(ctx, resolvedID)
}

func (s *Store) GetPendingApprovalForSession(ctx context.Context, requestID, sessionIdentity string) (*model.PendingApproval, error) {
	resolvedID, ok, err := resolvePendingApprovalRequestIDForSession(ctx, s.db, requestID, sessionIdentity)
	if err != nil || !ok {
		return nil, err
	}
	return s.getPendingApprovalByResolvedID(ctx, resolvedID)
}

func (s *Store) getPendingApprovalByResolvedID(ctx context.Context, resolvedID string) (*model.PendingApproval, error) {
	row := s.db.QueryRowContext(ctx, `
	SELECT request_id, coalesce(nullif(wire_request_id,''),request_id), thread_id, coalesce(turn_id,''), coalesce(item_id,''), prompt_kind, coalesce(nullif(request_kind,''),prompt_kind), coalesce(question,''), coalesce(session_identity,''), status, coalesce(telegram_message_id,0), payload_json, updated_at
	FROM pending_approvals WHERE request_id = ?`, resolvedID)
	var approval model.PendingApproval
	err := row.Scan(&approval.RequestID, &approval.WireRequestID, &approval.ThreadID, &approval.TurnID, &approval.ItemID, &approval.PromptKind, &approval.RequestKind, &approval.Question, &approval.SessionIdentity, &approval.Status, &approval.TelegramMessageID, &approval.PayloadJSON, &approval.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	approval.Question, err = s.unprotectSensitiveText(ctx, approval.Question)
	if err != nil {
		return nil, fmt.Errorf("unprotect pending approval question: %w", err)
	}
	approval.PayloadJSON, err = s.unprotectSensitiveText(ctx, approval.PayloadJSON)
	if err != nil {
		return nil, fmt.Errorf("unprotect pending approval payload: %w", err)
	}
	return &approval, nil
}

func (s *Store) UpdatePendingApprovalStatus(ctx context.Context, requestID, status string) error {
	resolvedID, ok, err := resolvePendingApprovalRequestID(ctx, s.db, requestID)
	if err != nil {
		return err
	}
	if !ok {
		return nil
	}
	_, err = s.db.ExecContext(ctx, `UPDATE pending_approvals SET status = ?, updated_at = ? WHERE request_id = ?`, status, string(model.NowString()), resolvedID)
	return err
}

func (s *Store) UpdatePendingApprovalStatusForSession(ctx context.Context, requestID, sessionIdentity, status string) error {
	resolvedID, ok, err := resolvePendingApprovalRequestIDForSession(ctx, s.db, requestID, sessionIdentity)
	if err != nil {
		return err
	}
	if !ok {
		return nil
	}
	_, err = s.db.ExecContext(ctx, `UPDATE pending_approvals SET status = ?, updated_at = ? WHERE request_id = ?`, status, string(model.NowString()), resolvedID)
	return err
}

func (s *Store) MarkAllPendingApprovals(ctx context.Context, status string) (int64, error) {
	result, err := s.db.ExecContext(ctx, `UPDATE pending_approvals SET status = ?, updated_at = ? WHERE status = 'pending'`, status, string(model.NowString()))
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}

type minimalApprovalRequestLookup interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

func resolveApprovalRequestID(ctx context.Context, q minimalApprovalRequestLookup, tableName, requestID, sessionIdentity string, allowLegacyWire bool) (string, bool, error) {
	requestID = strings.TrimSpace(requestID)
	sessionIdentity = strings.TrimSpace(sessionIdentity)
	if requestID == "" {
		return "", false, nil
	}
	if sessionIdentity != "" {
		scopedID := model.ScopedRequestID(sessionIdentity, requestID)
		if scopedID != "" && scopedID != requestID {
			if durable, ok, err := resolveApprovalRequestIDExact(ctx, q, tableName, scopedID, sessionIdentity, false); err != nil || ok {
				return durable, ok, err
			}
		}
	}
	if durable, ok, err := resolveApprovalRequestIDExact(ctx, q, tableName, requestID, sessionIdentity, allowLegacyWire); err != nil || ok {
		return durable, ok, err
	}
	if !allowLegacyWire {
		return "", false, nil
	}
	rows, err := q.QueryContext(ctx, fmt.Sprintf(`SELECT request_id FROM %s WHERE wire_request_id=? AND coalesce(session_identity,'')='' ORDER BY updated_at DESC LIMIT 2`, tableName), requestID)
	if err != nil {
		return "", false, err
	}
	defer rows.Close()
	var matches []string
	for rows.Next() {
		var match string
		if err := rows.Scan(&match); err != nil {
			return "", false, err
		}
		matches = append(matches, match)
	}
	if err := rows.Err(); err != nil {
		return "", false, err
	}
	if len(matches) != 1 {
		return "", false, nil
	}
	return matches[0], true, nil
}

func resolveApprovalRequestIDExact(ctx context.Context, q minimalApprovalRequestLookup, tableName, requestID, sessionIdentity string, allowLegacy bool) (string, bool, error) {
	var durable string
	var rowSession string
	err := q.QueryRowContext(ctx, fmt.Sprintf(`SELECT request_id, coalesce(session_identity,'') FROM %s WHERE request_id=?`, tableName), requestID).Scan(&durable, &rowSession)
	if err == nil {
		if sessionIdentity == "" || rowSession == sessionIdentity || (allowLegacy && rowSession == "") {
			return durable, true, nil
		}
		return "", false, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return "", false, err
	}
	return "", false, nil
}

func resolveMinimalApprovalRequestID(ctx context.Context, q minimalApprovalRequestLookup, requestID string) (string, bool, error) {
	return resolveApprovalRequestID(ctx, q, "minimal_approvals", requestID, "", true)
}

func resolveMinimalApprovalRequestIDForSession(ctx context.Context, q minimalApprovalRequestLookup, requestID, sessionIdentity string) (string, bool, error) {
	return resolveApprovalRequestID(ctx, q, "minimal_approvals", requestID, sessionIdentity, false)
}

func (s *Store) CreateMinimalApproval(ctx context.Context, seed MinimalApprovalSeed) (bool, error) {
	if s.protector == nil {
		return false, errors.New("securestore protector is required for minimal approval delivery")
	}
	seed.Approval.SessionIdentity = strings.TrimSpace(seed.Approval.SessionIdentity)
	seed.Approval.RequestID, seed.Approval.WireRequestID = model.NormalizeRequestIdentity(seed.Approval.SessionIdentity, seed.Approval.RequestID, seed.Approval.WireRequestID)
	if strings.TrimSpace(seed.Approval.RequestID) == "" || strings.TrimSpace(seed.Approval.ThreadID) == "" || strings.TrimSpace(seed.Approval.TurnID) == "" || strings.TrimSpace(seed.Approval.RequestKind) == "" || strings.TrimSpace(seed.Approval.SessionIdentity) == "" {
		return false, errors.New("minimal approval identity is incomplete")
	}
	if seed.Delivery.ChatID == 0 || strings.TrimSpace(seed.ApproveToken) == "" || strings.TrimSpace(seed.DenyToken) == "" || seed.ApproveToken == seed.DenyToken {
		return false, errors.New("minimal approval routes are invalid")
	}
	protected, err := s.protector.Protect(ctx, []byte(seed.Delivery.PayloadJSON))
	if err != nil {
		return false, fmt.Errorf("protect minimal approval delivery: %w", err)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer rollback(tx)
	now := string(model.NowString())
	status := strings.TrimSpace(seed.Approval.Status)
	if status == "" {
		status = "pending"
	}
	var existing MinimalApproval
	err = tx.QueryRowContext(ctx, `
		SELECT request_id,coalesce(nullif(wire_request_id,''),request_id),thread_id,turn_id,request_kind,session_identity,supersede_event_id,status,claim_state,coalesce(claim_action,''),chat_id,topic_id,telegram_message_id,delivery_queue_id
		FROM minimal_approvals WHERE request_id=?`, seed.Approval.RequestID).
		Scan(&existing.RequestID, &existing.WireRequestID, &existing.ThreadID, &existing.TurnID, &existing.RequestKind, &existing.SessionIdentity, &existing.SupersedeEventID, &existing.Status, &existing.ClaimState, &existing.ClaimAction, &existing.ChatID, &existing.TopicID, &existing.TelegramMessageID, &existing.DeliveryQueueID)
	exists := err == nil
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return false, err
	}
	if exists && existing.ThreadID == seed.Approval.ThreadID && existing.TurnID == seed.Approval.TurnID && existing.RequestKind == seed.Approval.RequestKind {
		if existing.Status != "pending" {
			if existing.DeliveryQueueID > 0 {
				if _, err := tx.ExecContext(ctx, `UPDATE delivery_queue SET status=?,payload_json=NULL,updated_at=? WHERE id=? AND status IN ('pending','retry','processing')`, model.DeliveryStatusDead, now, existing.DeliveryQueueID); err != nil {
					return false, err
				}
			}
			return false, tx.Commit()
		}
		result, err := tx.ExecContext(ctx, `UPDATE minimal_approvals SET wire_request_id=?,session_identity=?,supersede_event_id=?,updated_at=? WHERE request_id=? AND status='pending'`, seed.Approval.WireRequestID, seed.Approval.SessionIdentity, seed.SupersedeDeliveryEventID, now, seed.Approval.RequestID)
		if err != nil {
			return false, err
		}
		if n, err := result.RowsAffected(); err != nil || n != 1 {
			if err != nil {
				return false, err
			}
			return false, errors.New("minimal approval replay lost its pending request")
		}
		if err := s.rearmMinimalApprovalDeliveryIfDeadTx(ctx, tx, existing, seed, protected, seed.Delivery.AvailableAt, now); err != nil {
			return false, err
		}
		return false, tx.Commit()
	} else if exists {
		if existing.Status == "pending" {
			routeResult, err := tx.ExecContext(ctx, `UPDATE minimal_approval_routes SET status='expired' WHERE request_id=? AND status IN ('active','claimed')`, seed.Approval.RequestID)
			if err != nil {
				return false, err
			}
			if n, err := routeResult.RowsAffected(); err != nil || n != 2 {
				if err != nil {
					return false, err
				}
				return false, fmt.Errorf("expired %d replaced minimal approval routes, want 2", n)
			}
		}
		if existing.ChatID != 0 && existing.TelegramMessageID > 0 {
			if err := s.enqueueMinimalApprovalInactiveEditTx(ctx, tx, existing, "요청이 더 이상 활성 상태가 아닙니다.", now); err != nil {
				return false, err
			}
		}
		if existing.DeliveryQueueID > 0 {
			if _, err := tx.ExecContext(ctx, `UPDATE delivery_queue SET status='dead',payload_json=NULL,updated_at=? WHERE id=? AND status IN ('pending','retry','processing')`, now, existing.DeliveryQueueID); err != nil {
				return false, err
			}
		}
		result, err := tx.ExecContext(ctx, `
			UPDATE minimal_approvals SET wire_request_id=?,thread_id=?,turn_id=?,request_kind=?,project_name=?,session_identity=?,supersede_event_id=?,status=?,claim_state='idle',claim_action=NULL,chat_id=?,topic_id=?,telegram_message_id=0,delivery_queue_id=0,updated_at=?
			WHERE request_id=?`, seed.Approval.WireRequestID, seed.Approval.ThreadID, seed.Approval.TurnID, seed.Approval.RequestKind, seed.Approval.ProjectName, seed.Approval.SessionIdentity, seed.SupersedeDeliveryEventID, status, seed.Delivery.ChatID, seed.Delivery.TopicID, now, seed.Approval.RequestID)
		if err != nil {
			return false, err
		}
		if n, err := result.RowsAffected(); err != nil || n != 1 {
			if err != nil {
				return false, err
			}
			return false, errors.New("minimal approval replacement lost its request")
		}
	} else {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO minimal_approvals(request_id,wire_request_id,thread_id,turn_id,request_kind,project_name,session_identity,supersede_event_id,status,claim_state,chat_id,topic_id,updated_at)
			VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)`, seed.Approval.RequestID, seed.Approval.WireRequestID, seed.Approval.ThreadID, seed.Approval.TurnID, seed.Approval.RequestKind, seed.Approval.ProjectName, seed.Approval.SessionIdentity, seed.SupersedeDeliveryEventID, status, "idle", seed.Delivery.ChatID, seed.Delivery.TopicID, now); err != nil {
			return false, err
		}
	}
	if err := s.insertMinimalApprovalRoutesTx(ctx, tx, seed, now); err != nil {
		return false, err
	}
	var activeRoutes int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM minimal_approval_routes WHERE request_id=? AND thread_id=? AND turn_id=? AND request_kind=? AND status='active'`, seed.Approval.RequestID, seed.Approval.ThreadID, seed.Approval.TurnID, seed.Approval.RequestKind).Scan(&activeRoutes); err != nil {
		return false, err
	}
	if activeRoutes != 2 {
		return false, fmt.Errorf("seeded %d minimal approval routes, want 2", activeRoutes)
	}
	available := seed.Delivery.AvailableAt
	if available == "" {
		available = model.TimeString(now)
	}
	deliveryResult, err := tx.ExecContext(ctx, `
		INSERT INTO delivery_queue(event_id,chat_key,chat_id,topic_id,thread_id,kind,status,retry_count,available_at,last_error,payload_json,created_at,updated_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		seed.Delivery.EventID, seed.Delivery.ChatKey, seed.Delivery.ChatID, seed.Delivery.TopicID, seed.Delivery.ThreadID, seed.Delivery.Kind,
		model.DeliveryStatusPending, 0, available, nil, protected, now, now)
	if err != nil {
		return false, err
	}
	queueID, err := deliveryResult.LastInsertId()
	if err != nil {
		return false, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE minimal_approvals SET delivery_queue_id=? WHERE request_id=?`, queueID, seed.Approval.RequestID); err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return true, nil
}

func (s *Store) insertMinimalApprovalRoutesTx(ctx context.Context, tx *sql.Tx, seed MinimalApprovalSeed, now string) error {
	for _, route := range []struct{ token, action string }{{seed.ApproveToken, "approve"}, {seed.DenyToken, "deny"}} {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO minimal_approval_routes(route_token,action,request_id,thread_id,turn_id,request_kind,status,chat_id,topic_id,created_at)
			VALUES(?,?,?,?,?,?,?,?,?,?)`, route.token, route.action, seed.Approval.RequestID, seed.Approval.ThreadID, seed.Approval.TurnID, seed.Approval.RequestKind, "active", seed.Delivery.ChatID, seed.Delivery.TopicID, now); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) rearmMinimalApprovalDeliveryIfDeadTx(ctx context.Context, tx *sql.Tx, approval MinimalApproval, seed MinimalApprovalSeed, protected string, availableAt model.TimeString, now string) error {
	if approval.DeliveryQueueID <= 0 || strings.TrimSpace(protected) == "" {
		return nil
	}
	var status, lastError string
	err := tx.QueryRowContext(ctx, `SELECT status,coalesce(last_error,'') FROM delivery_queue WHERE id=? AND kind='minimal_approval'`, approval.DeliveryQueueID).Scan(&status, &lastError)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if status != model.DeliveryStatusDead || !strings.Contains(lastError, MinimalApprovalDeliveryOutcomeDefinitelyUnsent) {
		return nil
	}
	routeResult, err := tx.ExecContext(ctx, `UPDATE minimal_approval_routes SET status='expired' WHERE request_id=? AND status IN ('active','claimed')`, approval.RequestID)
	if err != nil {
		return err
	}
	if n, err := routeResult.RowsAffected(); err != nil || n != 2 {
		if err != nil {
			return err
		}
		return fmt.Errorf("expired %d replayed minimal approval routes, want 2", n)
	}
	if err := s.insertMinimalApprovalRoutesTx(ctx, tx, seed, now); err != nil {
		return err
	}
	available := availableAt
	if available == "" {
		available = model.TimeString(now)
	}
	result, err := tx.ExecContext(ctx, `
		UPDATE delivery_queue SET status=?,retry_count=0,available_at=?,last_error=NULL,payload_json=?,updated_at=?
		WHERE id=? AND kind='minimal_approval' AND status=? AND coalesce(last_error,'')<>''`,
		model.DeliveryStatusPending, available, protected, now, approval.DeliveryQueueID, model.DeliveryStatusDead)
	if err != nil {
		return err
	}
	if n, err := result.RowsAffected(); err != nil || n != 1 {
		if err != nil {
			return err
		}
		return errors.New("minimal approval replay lost its dead delivery")
	}
	return nil
}

func (s *Store) GetMinimalApproval(ctx context.Context, requestID string) (*MinimalApproval, error) {
	resolvedID, ok, err := resolveMinimalApprovalRequestID(ctx, s.db, requestID)
	if err != nil || !ok {
		return nil, err
	}
	return s.getMinimalApprovalByResolvedID(ctx, resolvedID)
}

func (s *Store) GetMinimalApprovalForSession(ctx context.Context, requestID, sessionIdentity string) (*MinimalApproval, error) {
	resolvedID, ok, err := resolveMinimalApprovalRequestIDForSession(ctx, s.db, requestID, sessionIdentity)
	if err != nil || !ok {
		return nil, err
	}
	return s.getMinimalApprovalByResolvedID(ctx, resolvedID)
}

func (s *Store) getMinimalApprovalByResolvedID(ctx context.Context, resolvedID string) (*MinimalApproval, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT request_id,coalesce(nullif(wire_request_id,''),request_id),thread_id,turn_id,request_kind,project_name,session_identity,supersede_event_id,status,claim_state,coalesce(claim_action,''),chat_id,topic_id,telegram_message_id,delivery_queue_id,updated_at
		FROM minimal_approvals WHERE request_id=?`, resolvedID)
	var approval MinimalApproval
	if err := row.Scan(&approval.RequestID, &approval.WireRequestID, &approval.ThreadID, &approval.TurnID, &approval.RequestKind, &approval.ProjectName, &approval.SessionIdentity, &approval.SupersedeEventID, &approval.Status, &approval.ClaimState, &approval.ClaimAction, &approval.ChatID, &approval.TopicID, &approval.TelegramMessageID, &approval.DeliveryQueueID, &approval.UpdatedAt); errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	return &approval, nil
}

func (s *Store) GetMinimalApprovalRoute(ctx context.Context, token string) (*MinimalApprovalRoute, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT r.route_token,r.action,r.request_id,coalesce(nullif(a.wire_request_id,''),r.request_id),r.thread_id,r.turn_id,r.request_kind,coalesce(a.session_identity,''),r.status,r.chat_id,r.topic_id,r.telegram_message_id,r.created_at
		FROM minimal_approval_routes r LEFT JOIN minimal_approvals a ON a.request_id=r.request_id WHERE r.route_token=?`, token)
	var route MinimalApprovalRoute
	if err := row.Scan(&route.Token, &route.Action, &route.RequestID, &route.WireRequestID, &route.ThreadID, &route.TurnID, &route.RequestKind, &route.SessionIdentity, &route.Status, &route.ChatID, &route.TopicID, &route.TelegramMessageID, &route.CreatedAt); errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	return &route, nil
}

func (s *Store) CompleteMinimalApprovalDelivery(ctx context.Context, queueID int64, requestID string, chatID, topicID, messageID int64) error {
	if queueID <= 0 || messageID <= 0 {
		return errors.New("minimal approval delivery identity is incomplete")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer rollback(tx)
	resolvedID, ok, err := resolveMinimalApprovalRequestID(ctx, tx, requestID)
	if err != nil {
		return err
	}
	if !ok {
		return sql.ErrNoRows
	}
	requestID = resolvedID
	var approval MinimalApproval
	if err := tx.QueryRowContext(ctx, `
		SELECT request_id,coalesce(nullif(wire_request_id,''),request_id),thread_id,turn_id,request_kind,delivery_queue_id,status,coalesce(supersede_event_id,'') FROM minimal_approvals WHERE request_id=?`, requestID).
		Scan(&approval.RequestID, &approval.WireRequestID, &approval.ThreadID, &approval.TurnID, &approval.RequestKind, &approval.DeliveryQueueID, &approval.Status, &approval.SupersedeEventID); err != nil {
		return err
	}
	if approval.DeliveryQueueID != queueID || approval.Status != "pending" {
		return errors.New("minimal approval delivery does not match pending request")
	}
	var queueStatus string
	if err := tx.QueryRowContext(ctx, `SELECT status FROM delivery_queue WHERE id=? AND chat_id=? AND topic_id=? AND kind='minimal_approval'`, queueID, chatID, topicID).Scan(&queueStatus); err != nil {
		return err
	}
	if queueStatus != model.DeliveryStatusProcessing {
		return errors.New("minimal approval delivery is not processing")
	}
	now := string(model.NowString())
	if _, err := tx.ExecContext(ctx, `UPDATE minimal_approvals SET chat_id=?,topic_id=?,telegram_message_id=?,updated_at=? WHERE request_id=?`, chatID, topicID, messageID, now, requestID); err != nil {
		return err
	}
	routeResult, err := tx.ExecContext(ctx, `UPDATE minimal_approval_routes SET chat_id=?,topic_id=?,telegram_message_id=? WHERE request_id=? AND status='active'`, chatID, topicID, messageID, requestID)
	if err != nil {
		return err
	}
	if n, err := routeResult.RowsAffected(); err != nil || n != 2 {
		if err != nil {
			return err
		}
		return fmt.Errorf("bound %d minimal approval routes, want 2", n)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO telegram_message_routes(chat_id,topic_id,message_id,thread_id,turn_id,event_id,created_at) VALUES(?,?,?,?,?,?,?) ON CONFLICT(chat_id,topic_id,message_id) DO UPDATE SET thread_id=excluded.thread_id,turn_id=excluded.turn_id,event_id=excluded.event_id,created_at=excluded.created_at`, chatID, topicID, messageID, approval.ThreadID, approval.TurnID, requestID, now); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE delivery_queue SET status=?,payload_json=NULL,updated_at=? WHERE id=?`, model.DeliveryStatusDelivered, now, queueID); err != nil {
		return err
	}
	if err := s.enqueueMinimalApprovalPCCleanupTx(ctx, tx, approval, now); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) enqueueMinimalApprovalPCCleanupTx(ctx context.Context, tx *sql.Tx, approval MinimalApproval, now string) error {
	supersedeEventID := strings.TrimSpace(approval.SupersedeEventID)
	if supersedeEventID == "" {
		return nil
	}
	rows, err := tx.QueryContext(ctx, `
		SELECT chat_id,topic_id,message_id,thread_id,coalesce(turn_id,''),coalesce(item_id,''),coalesce(event_id,''),created_at
		FROM telegram_message_routes
		WHERE event_id=? AND thread_id=? AND coalesce(turn_id,'')=?
		ORDER BY chat_id,topic_id,message_id`, supersedeEventID, approval.ThreadID, approval.TurnID)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var route model.MessageRoute
		if err := rows.Scan(&route.ChatID, &route.TopicID, &route.MessageID, &route.ThreadID, &route.TurnID, &route.ItemID, &route.EventID, &route.CreatedAt); err != nil {
			return err
		}
		if route.ChatID == 0 || route.MessageID <= 0 {
			continue
		}
		payload := model.DeliveryPayload{
			Mode:      "delete_message",
			ThreadID:  approval.ThreadID,
			TurnID:    approval.TurnID,
			ItemID:    approval.RequestID,
			EventID:   supersedeEventID,
			MessageID: route.MessageID,
		}
		payloadBytes, err := json.Marshal(payload)
		if err != nil {
			return err
		}
		payloadJSON := string(payloadBytes)
		if s.protector != nil {
			payloadJSON, err = s.protector.Protect(ctx, payloadBytes)
			if err != nil {
				return fmt.Errorf("protect minimal approval cleanup delivery: %w", err)
			}
		}
		cleanupEventID := minimalApprovalPCCleanupEventID(approval.RequestID, supersedeEventID)
		_, err = tx.ExecContext(ctx, `
			INSERT INTO delivery_queue(event_id,chat_key,chat_id,topic_id,thread_id,kind,status,retry_count,available_at,last_error,payload_json,created_at,updated_at)
			VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(event_id,chat_key) DO NOTHING`,
			cleanupEventID, model.ChatKey(route.ChatID, route.TopicID), route.ChatID, route.TopicID, approval.ThreadID, MinimalApprovalPCCleanupQueueKind, model.DeliveryStatusPending, 0, now, nil, payloadJSON, now, now)
		if err != nil {
			return err
		}
	}
	return rows.Err()
}

func minimalApprovalPCCleanupEventID(requestID, supersedeEventID string) string {
	digest := sha256.Sum256([]byte(strings.Join([]string{requestID, supersedeEventID}, "\x00")))
	return fmt.Sprintf("minimal-approval-pc-cleanup:%x", digest[:12])
}

func (s *Store) MinimalApprovalPCCleanupReady(ctx context.Context, requestID, threadID, turnID, supersedeEventID string) (bool, bool, error) {
	requestID = strings.TrimSpace(requestID)
	threadID = strings.TrimSpace(threadID)
	turnID = strings.TrimSpace(turnID)
	supersedeEventID = strings.TrimSpace(supersedeEventID)
	if requestID == "" && (threadID == "" || turnID == "" || supersedeEventID == "") {
		return false, true, nil
	}
	if requestID == "" {
		err := s.db.QueryRowContext(ctx, `
			SELECT request_id FROM minimal_approvals
			WHERE thread_id=? AND turn_id=? AND supersede_event_id=?
			ORDER BY updated_at DESC LIMIT 1`, threadID, turnID, supersedeEventID).Scan(&requestID)
		if errors.Is(err, sql.ErrNoRows) {
			return false, true, nil
		}
		if err != nil {
			return false, false, err
		}
	} else if resolvedID, ok, err := resolveMinimalApprovalRequestID(ctx, s.db, requestID); err != nil {
		return false, false, err
	} else if !ok {
		return false, true, nil
	} else {
		requestID = resolvedID
	}
	var status string
	var messageID, approvalChatID, approvalTopicID int64
	err := s.db.QueryRowContext(ctx, `
		SELECT status,telegram_message_id,chat_id,topic_id FROM minimal_approvals
		WHERE request_id=? AND (?='' OR thread_id=?) AND (?='' OR turn_id=?) AND (?='' OR supersede_event_id=?)`,
		requestID, threadID, threadID, turnID, turnID, supersedeEventID, supersedeEventID).Scan(&status, &messageID, &approvalChatID, &approvalTopicID)
	if errors.Is(err, sql.ErrNoRows) {
		return false, true, nil
	}
	if err != nil {
		return false, false, err
	}
	if status != "pending" {
		return false, true, nil
	}
	if messageID <= 0 {
		return false, false, nil
	}
	var routeCount int
	if err := s.db.QueryRowContext(ctx, `
		SELECT count(*) FROM telegram_message_routes
		WHERE chat_id=? AND topic_id=? AND message_id=? AND event_id=?`,
		approvalChatID, approvalTopicID, messageID, requestID).Scan(&routeCount); err != nil {
		return false, false, err
	}
	return routeCount == 1, false, nil
}

func (s *Store) MinimalApprovalDeliveryActive(ctx context.Context, queueID int64, requestID string) (bool, error) {
	resolvedID, ok, err := resolveMinimalApprovalRequestID(ctx, s.db, requestID)
	if err != nil || !ok {
		return false, err
	}
	requestID = resolvedID
	var count int
	err = s.db.QueryRowContext(ctx, `
		SELECT count(*) FROM minimal_approvals a JOIN delivery_queue q ON q.id=a.delivery_queue_id
		WHERE a.request_id=? AND a.delivery_queue_id=? AND a.status='pending' AND a.claim_state='idle' AND q.status='processing'`, requestID, queueID).Scan(&count)
	return count == 1, err
}

func (s *Store) ClaimMinimalApproval(ctx context.Context, token string, chatID, topicID, messageID int64, sessionIdentity string) (*MinimalApprovalRoute, bool, error) {
	sessionIdentity = strings.TrimSpace(sessionIdentity)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, false, err
	}
	defer rollback(tx)
	var route MinimalApprovalRoute
	var approvalStatus, claimState, approvalThread, approvalTurn, approvalKind, approvalSession string
	var approvalChat, approvalTopic, approvalMessage int64
	err = tx.QueryRowContext(ctx, `
		SELECT r.route_token,r.action,r.request_id,coalesce(nullif(a.wire_request_id,''),r.request_id),r.thread_id,r.turn_id,r.request_kind,coalesce(a.session_identity,''),r.status,r.chat_id,r.topic_id,r.telegram_message_id,r.created_at,
		       a.status,a.claim_state,a.thread_id,a.turn_id,a.request_kind,a.session_identity,a.chat_id,a.topic_id,a.telegram_message_id
		FROM minimal_approval_routes r JOIN minimal_approvals a ON a.request_id=r.request_id WHERE r.route_token=?`, token).
		Scan(&route.Token, &route.Action, &route.RequestID, &route.WireRequestID, &route.ThreadID, &route.TurnID, &route.RequestKind, &route.SessionIdentity, &route.Status, &route.ChatID, &route.TopicID, &route.TelegramMessageID, &route.CreatedAt,
			&approvalStatus, &claimState, &approvalThread, &approvalTurn, &approvalKind, &approvalSession, &approvalChat, &approvalTopic, &approvalMessage)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, tx.Commit()
	}
	if err != nil {
		return nil, false, err
	}
	boundMessage := route.TelegramMessageID == messageID && approvalMessage == messageID
	unboundMessage := route.TelegramMessageID == 0 && approvalMessage == 0
	exact := messageID > 0 && approvalSession == sessionIdentity && route.Status == "active" && approvalStatus == "pending" && claimState == "idle" &&
		route.ChatID == chatID && route.TopicID == topicID && approvalChat == chatID && approvalTopic == topicID && (boundMessage || unboundMessage) &&
		route.ThreadID == approvalThread && route.TurnID == approvalTurn && route.RequestKind == approvalKind
	if !exact {
		return &route, false, tx.Commit()
	}
	if unboundMessage {
		now := string(model.NowString())
		approvalResult, err := tx.ExecContext(ctx, `UPDATE minimal_approvals SET telegram_message_id=?,updated_at=? WHERE request_id=? AND telegram_message_id=0 AND status='pending' AND claim_state='idle'`, messageID, now, route.RequestID)
		if err != nil {
			return nil, false, err
		}
		if n, err := approvalResult.RowsAffected(); err != nil || n != 1 {
			if err != nil {
				return nil, false, err
			}
			return &route, false, tx.Commit()
		}
		routeResult, err := tx.ExecContext(ctx, `UPDATE minimal_approval_routes SET telegram_message_id=? WHERE request_id=? AND status='active' AND telegram_message_id=0`, messageID, route.RequestID)
		if err != nil {
			return nil, false, err
		}
		if n, err := routeResult.RowsAffected(); err != nil || n != 2 {
			if err != nil {
				return nil, false, err
			}
			return nil, false, fmt.Errorf("self-bound %d minimal approval routes, want 2", n)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO telegram_message_routes(chat_id,topic_id,message_id,thread_id,turn_id,event_id,created_at) VALUES(?,?,?,?,?,?,?) ON CONFLICT(chat_id,topic_id,message_id) DO UPDATE SET thread_id=excluded.thread_id,turn_id=excluded.turn_id,event_id=excluded.event_id,created_at=excluded.created_at`, chatID, topicID, messageID, route.ThreadID, route.TurnID, route.RequestID, now); err != nil {
			return nil, false, err
		}
		route.TelegramMessageID = messageID
	}
	result, err := tx.ExecContext(ctx, `UPDATE minimal_approvals SET claim_state='claimed',claim_action=?,updated_at=? WHERE request_id=? AND status='pending' AND claim_state='idle'`, route.Action, string(model.NowString()), route.RequestID)
	if err != nil {
		return nil, false, err
	}
	if n, err := result.RowsAffected(); err != nil || n != 1 {
		if err != nil {
			return nil, false, err
		}
		return &route, false, tx.Commit()
	}
	routeResult, err := tx.ExecContext(ctx, `UPDATE minimal_approval_routes SET status='claimed' WHERE request_id=? AND status='active'`, route.RequestID)
	if err != nil {
		return nil, false, err
	}
	if n, err := routeResult.RowsAffected(); err != nil || n != 2 {
		if err != nil {
			return nil, false, err
		}
		return nil, false, fmt.Errorf("claimed %d minimal approval routes, want 2", n)
	}
	if err := tx.Commit(); err != nil {
		return nil, false, err
	}
	return &route, true, nil
}

func (s *Store) RestoreMinimalApprovalClaim(ctx context.Context, requestID, action string) (bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer rollback(tx)
	resolvedID, ok, err := resolveMinimalApprovalRequestID(ctx, tx, requestID)
	if err != nil {
		return false, err
	}
	if !ok {
		return false, tx.Commit()
	}
	requestID = resolvedID
	result, err := tx.ExecContext(ctx, `UPDATE minimal_approvals SET claim_state='idle',claim_action=NULL,updated_at=? WHERE request_id=? AND status='pending' AND claim_state='claimed' AND claim_action=?`, string(model.NowString()), requestID, action)
	if err != nil {
		return false, err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	if changed == 1 {
		routeResult, err := tx.ExecContext(ctx, `UPDATE minimal_approval_routes SET status='active' WHERE request_id=? AND status='claimed'`, requestID)
		if err != nil {
			return false, err
		}
		if n, err := routeResult.RowsAffected(); err != nil || n != 2 {
			if err != nil {
				return false, err
			}
			return false, fmt.Errorf("restored %d minimal approval routes, want 2", n)
		}
	}
	return changed == 1, tx.Commit()
}

func (s *Store) FinishMinimalApprovalClaim(ctx context.Context, requestID, action, status string) (bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer rollback(tx)
	resolvedID, ok, err := resolveMinimalApprovalRequestID(ctx, tx, requestID)
	if err != nil {
		return false, err
	}
	if !ok {
		return false, tx.Commit()
	}
	requestID = resolvedID
	result, err := tx.ExecContext(ctx, `UPDATE minimal_approvals SET status=?,claim_state='idle',claim_action=NULL,updated_at=? WHERE request_id=? AND status='pending' AND claim_state='claimed' AND claim_action=?`, status, string(model.NowString()), requestID, action)
	if err != nil {
		return false, err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	if changed == 1 {
		routeResult, err := tx.ExecContext(ctx, `UPDATE minimal_approval_routes SET status='consumed' WHERE request_id=? AND status='claimed'`, requestID)
		if err != nil {
			return false, err
		}
		if n, err := routeResult.RowsAffected(); err != nil || n != 2 {
			if err != nil {
				return false, err
			}
			return false, fmt.Errorf("finished %d minimal approval routes, want 2", n)
		}
	}
	return changed == 1, tx.Commit()
}

func (s *Store) ExpireMinimalApproval(ctx context.Context, requestID string) (bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer rollback(tx)
	resolvedID, ok, err := resolveMinimalApprovalRequestID(ctx, tx, requestID)
	if err != nil {
		return false, err
	}
	if !ok {
		return false, tx.Commit()
	}
	requestID = resolvedID
	result, err := tx.ExecContext(ctx, `UPDATE minimal_approvals SET status='expired',updated_at=? WHERE request_id=? AND status='pending' AND claim_state='idle'`, string(model.NowString()), requestID)
	if err != nil {
		return false, err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	if changed == 1 {
		routeResult, err := tx.ExecContext(ctx, `UPDATE minimal_approval_routes SET status='expired' WHERE request_id=? AND status='active'`, requestID)
		if err != nil {
			return false, err
		}
		if n, err := routeResult.RowsAffected(); err != nil || n != 2 {
			if err != nil {
				return false, err
			}
			return false, fmt.Errorf("expired %d minimal approval routes, want 2", n)
		}
		if _, err := tx.ExecContext(ctx, `UPDATE delivery_queue SET status='dead',payload_json=NULL,updated_at=? WHERE id=(SELECT delivery_queue_id FROM minimal_approvals WHERE request_id=?) AND status IN ('pending','retry','processing')`, string(model.NowString()), requestID); err != nil {
			return false, err
		}
	}
	return changed == 1, tx.Commit()
}

func (s *Store) ExpireMinimalApprovalForSession(ctx context.Context, requestID, sessionIdentity string) (bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer rollback(tx)
	resolvedID, ok, err := resolveMinimalApprovalRequestIDForSession(ctx, tx, requestID, sessionIdentity)
	if err != nil {
		return false, err
	}
	if !ok {
		return false, tx.Commit()
	}
	requestID = resolvedID
	result, err := tx.ExecContext(ctx, `UPDATE minimal_approvals SET status='expired',updated_at=? WHERE request_id=? AND status='pending' AND claim_state='idle'`, string(model.NowString()), requestID)
	if err != nil {
		return false, err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	if changed == 1 {
		routeResult, err := tx.ExecContext(ctx, `UPDATE minimal_approval_routes SET status='expired' WHERE request_id=? AND status='active'`, requestID)
		if err != nil {
			return false, err
		}
		if n, err := routeResult.RowsAffected(); err != nil || n != 2 {
			if err != nil {
				return false, err
			}
			return false, fmt.Errorf("expired %d minimal approval routes, want 2", n)
		}
		if _, err := tx.ExecContext(ctx, `UPDATE delivery_queue SET status='dead',payload_json=NULL,updated_at=? WHERE id=(SELECT delivery_queue_id FROM minimal_approvals WHERE request_id=?) AND status IN ('pending','retry','processing')`, string(model.NowString()), requestID); err != nil {
			return false, err
		}
	}
	return changed == 1, tx.Commit()
}

func (s *Store) ExpireMinimalApprovalsForSession(ctx context.Context, identity string) (int64, error) {
	identity = strings.TrimSpace(identity)
	if identity == "" {
		return 0, nil
	}
	if err := s.EnsureMinimalSchema(ctx); err != nil {
		return 0, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer rollback(tx)
	changed, err := s.expireMinimalApprovalsForSessionTx(ctx, tx, identity, string(model.NowString()))
	if err != nil {
		return 0, err
	}
	if pendingChanged, err := s.expirePendingApprovalsForSessionTx(ctx, tx, identity, string(model.NowString())); err != nil {
		return 0, err
	} else {
		changed += pendingChanged
	}
	if _, err := s.expireCallbackRoutesForSessionTx(ctx, tx, identity); err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return changed, nil
}

func (s *Store) expireMinimalApprovalsForSessionTx(ctx context.Context, tx *sql.Tx, identity, now string) (int64, error) {
	identity = strings.TrimSpace(identity)
	if identity == "" {
		return 0, nil
	}
	result, err := tx.ExecContext(ctx, `UPDATE minimal_approvals SET status='expired',claim_state='idle',claim_action=NULL,updated_at=? WHERE session_identity=? AND status='pending'`, now, identity)
	if err != nil {
		return 0, err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return 0, err
	}
	if changed > 0 {
		if _, err := tx.ExecContext(ctx, `
		UPDATE minimal_approval_routes
		SET status='expired'
		WHERE status IN ('active','claimed') AND request_id IN (
			SELECT request_id FROM minimal_approvals WHERE session_identity=? AND status='expired'
		)`, identity); err != nil {
			return 0, err
		}
		if _, err := tx.ExecContext(ctx, `
		UPDATE delivery_queue
		SET status='dead', payload_json=NULL, updated_at=?
		WHERE status IN ('pending','retry','processing') AND id IN (
			SELECT delivery_queue_id FROM minimal_approvals WHERE session_identity=? AND status='expired'
		)`, now, identity); err != nil {
			return 0, err
		}
	}
	return changed, nil
}

func (s *Store) expirePendingApprovalsForSessionTx(ctx context.Context, tx *sql.Tx, identity, now string) (int64, error) {
	identity = strings.TrimSpace(identity)
	if identity == "" {
		return 0, nil
	}
	result, err := tx.ExecContext(ctx, `UPDATE pending_approvals SET status='expired',updated_at=? WHERE session_identity=? AND status='pending'`, now, identity)
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}

func (s *Store) expireCallbackRoutesForSessionTx(ctx context.Context, tx *sql.Tx, identity string) (int64, error) {
	identity = strings.TrimSpace(identity)
	if identity == "" {
		return 0, nil
	}
	result, err := tx.ExecContext(ctx, `UPDATE callback_routes SET status=? WHERE session_identity=? AND status IN (?, 'claimed')`, model.CallbackStatusExpired, identity, model.CallbackStatusActive)
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}

func (s *Store) CancelMinimalApprovalClaimWithInactiveEdit(ctx context.Context, requestID, action, text string) (bool, error) {
	if s.protector == nil {
		return false, errors.New("securestore protector is required for minimal approval inactive edit")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer rollback(tx)
	resolvedID, ok, err := resolveMinimalApprovalRequestID(ctx, tx, requestID)
	if err != nil {
		return false, err
	}
	if !ok {
		return false, tx.Commit()
	}
	requestID = resolvedID
	var approval MinimalApproval
	if err := tx.QueryRowContext(ctx, `
		SELECT request_id,coalesce(nullif(wire_request_id,''),request_id),thread_id,turn_id,request_kind,session_identity,status,claim_state,coalesce(claim_action,''),chat_id,topic_id,telegram_message_id
		FROM minimal_approvals WHERE request_id=?`, requestID).
		Scan(&approval.RequestID, &approval.WireRequestID, &approval.ThreadID, &approval.TurnID, &approval.RequestKind, &approval.SessionIdentity, &approval.Status, &approval.ClaimState, &approval.ClaimAction, &approval.ChatID, &approval.TopicID, &approval.TelegramMessageID); errors.Is(err, sql.ErrNoRows) {
		return false, tx.Commit()
	} else if err != nil {
		return false, err
	}
	if approval.Status != "pending" || approval.ClaimState != "claimed" || approval.ClaimAction != action || approval.ChatID == 0 || approval.TelegramMessageID <= 0 || strings.TrimSpace(text) == "" {
		return false, tx.Commit()
	}
	now := string(model.NowString())
	result, err := tx.ExecContext(ctx, `UPDATE minimal_approvals SET status='cancelled',claim_state='idle',claim_action=NULL,updated_at=? WHERE request_id=? AND status='pending' AND claim_state='claimed' AND claim_action=?`, now, requestID, action)
	if err != nil {
		return false, err
	}
	if n, err := result.RowsAffected(); err != nil || n != 1 {
		if err != nil {
			return false, err
		}
		return false, tx.Commit()
	}
	routeResult, err := tx.ExecContext(ctx, `UPDATE minimal_approval_routes SET status='consumed' WHERE request_id=? AND status='claimed'`, requestID)
	if err != nil {
		return false, err
	}
	if n, err := routeResult.RowsAffected(); err != nil || n != 2 {
		if err != nil {
			return false, err
		}
		return false, fmt.Errorf("cancelled %d minimal approval routes, want 2", n)
	}
	if err := s.enqueueMinimalApprovalInactiveEditTx(ctx, tx, approval, text, now); err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return true, nil
}

func (s *Store) enqueueMinimalApprovalInactiveEditTx(ctx context.Context, tx *sql.Tx, approval MinimalApproval, text, now string) error {
	payload := MustJSON(model.DeliveryPayload{
		Mode:      "edit",
		Text:      text,
		ThreadID:  approval.ThreadID,
		TurnID:    approval.TurnID,
		EventID:   approval.RequestID,
		MessageID: approval.TelegramMessageID,
	})
	protected, err := s.protector.Protect(ctx, []byte(payload))
	if err != nil {
		return fmt.Errorf("protect minimal approval inactive edit: %w", err)
	}
	digest := sha256.Sum256([]byte(strings.Join([]string{approval.RequestID, approval.ThreadID, approval.TurnID, approval.RequestKind, approval.SessionIdentity}, "\x00")))
	_, err = tx.ExecContext(ctx, `
		INSERT INTO delivery_queue(event_id,chat_key,chat_id,topic_id,thread_id,kind,status,retry_count,available_at,last_error,payload_json,created_at,updated_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(event_id,chat_key) DO NOTHING`, fmt.Sprintf("minimal-approval-inactive:%x", digest[:12]), model.ChatKey(approval.ChatID, approval.TopicID), approval.ChatID, approval.TopicID, approval.ThreadID, "minimal_approval_inactive_edit", model.DeliveryStatusPending, 0, now, nil, protected, now, now)
	return err
}

func (s *Store) RecoverMinimalApprovalClaims(ctx context.Context) (int64, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT request_id,coalesce(claim_action,'') FROM minimal_approvals WHERE status='pending' AND claim_state='claimed'`)
	if err != nil {
		return 0, err
	}
	var claims []struct{ requestID, action string }
	for rows.Next() {
		var claim struct{ requestID, action string }
		if err := rows.Scan(&claim.requestID, &claim.action); err != nil {
			_ = rows.Close()
			return 0, err
		}
		claims = append(claims, claim)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return 0, err
	}
	if err := rows.Close(); err != nil {
		return 0, err
	}
	var recovered int64
	for _, claim := range claims {
		changed, err := s.CancelMinimalApprovalClaimWithInactiveEdit(ctx, claim.requestID, claim.action, "처리 결과를 확인할 수 없어 버튼이 비활성화되었습니다.")
		if err != nil {
			return 0, err
		}
		if changed {
			recovered++
		}
	}
	return recovered, nil
}

func (s *Store) EnqueueDelivery(ctx context.Context, item model.DeliveryQueueItem) error {
	now := string(model.NowString())
	if item.AvailableAt == "" {
		item.AvailableAt = model.TimeString(now)
	}
	if item.CreatedAt == "" {
		item.CreatedAt = model.TimeString(now)
	}
	if item.UpdatedAt == "" {
		item.UpdatedAt = model.TimeString(now)
	}
	payload := item.PayloadJSON
	if s.protector != nil {
		protected, err := s.protector.Protect(ctx, []byte(payload))
		if err != nil {
			return fmt.Errorf("protect delivery payload: %w", err)
		}
		payload = protected
	}
	_, err := s.db.ExecContext(ctx, `
	INSERT INTO delivery_queue(event_id, chat_key, chat_id, topic_id, thread_id, kind, status, retry_count, available_at, last_error, payload_json, created_at, updated_at)
	VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	ON CONFLICT(event_id, chat_key) DO NOTHING`,
		item.EventID, item.ChatKey, item.ChatID, item.TopicID, item.ThreadID, item.Kind, item.Status, item.RetryCount, item.AvailableAt, nullable(item.LastError), payload, item.CreatedAt, item.UpdatedAt,
	)
	return err
}

func (s *Store) EnqueueTerminalEvent(ctx context.Context, event model.TerminalEvent, items []model.DeliveryQueueItem) (bool, error) {
	return s.EnqueueTerminalEventWithLinkedRelease(ctx, event, items, nil)
}

func (s *Store) EnqueueTerminalEventWithLinkedRelease(ctx context.Context, event model.TerminalEvent, items []model.DeliveryQueueItem, release *model.MinimalLinkedRelease) (bool, error) {
	if s.protector == nil {
		return false, errors.New("securestore protector is required for terminal delivery")
	}
	var releaseLinkedID, releaseTurnID string
	var releaseGeneration int64
	if release != nil {
		releaseLinkedID = strings.TrimSpace(release.LinkedThreadID)
		releaseTurnID = strings.TrimSpace(release.TurnID)
		generation, err := minimalLinkedReleaseGeneration(*release)
		if err != nil {
			return false, err
		}
		releaseGeneration, err = minimalLinkedGenerationInt64(generation)
		if err != nil {
			return false, err
		}
		if releaseLinkedID == "" || releaseTurnID == "" {
			return false, nil
		}
		if err := s.EnsureMinimalSchema(ctx); err != nil {
			return false, err
		}
	}
	protected := make([]string, len(items))
	for i := range items {
		value, err := s.protector.Protect(ctx, []byte(items[i].PayloadJSON))
		if err != nil {
			return false, fmt.Errorf("protect terminal delivery payload: %w", err)
		}
		protected[i] = value
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer rollback(tx)
	now := string(model.NowString())
	result, err := tx.ExecContext(ctx, `INSERT INTO terminal_events(terminal_key,thread_id,turn_id,status,delivery_status,updated_at) VALUES(?,?,?,?,?,?) ON CONFLICT(terminal_key) DO NOTHING`, event.TerminalKey, event.ThreadID, event.TurnID, event.Status, model.DeliveryStatusPending, now)
	if err != nil {
		return false, err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	if changed == 0 {
		return false, tx.Commit()
	}
	for i, item := range items {
		available := item.AvailableAt
		if available == "" {
			available = model.TimeString(now)
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO delivery_queue(event_id,chat_key,chat_id,topic_id,thread_id,kind,status,retry_count,available_at,last_error,payload_json,created_at,updated_at,group_id,sequence_no,sequence_count) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, item.EventID, item.ChatKey, item.ChatID, item.TopicID, item.ThreadID, item.Kind, model.DeliveryStatusPending, 0, available, nil, protected[i], now, now, item.GroupID, item.SequenceNo, item.SequenceCount)
		if err != nil {
			return false, err
		}
	}
	if release != nil {
		result, err := tx.ExecContext(ctx, `
		UPDATE minimal_linked_threads
		SET state = ?, updated_at = ?
		WHERE linked_thread_id = ? AND worker_generation = ? AND state = ? AND active_turn_id = ?`,
			model.MinimalLinkedReleasePending, now, releaseLinkedID, releaseGeneration, model.MinimalLinkedTelegramRunning, releaseTurnID)
		if err != nil {
			return false, err
		}
		changed, err := result.RowsAffected()
		if err != nil {
			return false, err
		}
		if changed != 1 {
			return false, nil
		}
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return true, nil
}

func (s *Store) FinishMinimalLinkedReleaseAndClearWorker(ctx context.Context, linkedID string, generation uint64, releasedAt time.Time) (bool, error) {
	linkedID = strings.TrimSpace(linkedID)
	if linkedID == "" {
		return false, nil
	}
	if releasedAt.IsZero() {
		releasedAt = time.Now().UTC()
	}
	generationValue, err := minimalLinkedGenerationInt64(generation)
	if err != nil {
		return false, err
	}
	if err := s.EnsureMinimalSchema(ctx); err != nil {
		return false, err
	}
	releasedAtString := releasedAt.UTC().Format(time.RFC3339Nano)
	result, err := s.db.ExecContext(ctx, `
	UPDATE minimal_linked_threads
	SET state = ?, active_turn_id = NULL, worker_generation = 0, released_at = ?, updated_at = ?
	WHERE linked_thread_id = ? AND worker_generation = ? AND state = ?`,
		model.MinimalLinkedReady, releasedAtString, releasedAtString, linkedID, generationValue, model.MinimalLinkedReleasePending)
	if err != nil {
		return false, err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	return changed == 1, nil
}

func (s *Store) FinishMinimalLinkedReleaseWithReadyDelivery(ctx context.Context, linkedID string, generation uint64, sessionIdentity string, turnID string, releasedAt time.Time, ready *model.DeliveryQueueItem) (bool, error) {
	linkedID = strings.TrimSpace(linkedID)
	sessionIdentity = strings.TrimSpace(sessionIdentity)
	turnID = strings.TrimSpace(turnID)
	if linkedID == "" || turnID == "" {
		return false, nil
	}
	if releasedAt.IsZero() {
		releasedAt = time.Now().UTC()
	}
	generationValue, err := minimalLinkedGenerationInt64(generation)
	if err != nil {
		return false, err
	}
	var protectedReady string
	if ready != nil {
		if s.protector == nil {
			return false, errors.New("securestore protector is required for handoff ready delivery")
		}
		protectedReady, err = s.protector.Protect(ctx, []byte(ready.PayloadJSON))
		if err != nil {
			return false, fmt.Errorf("protect handoff ready delivery: %w", err)
		}
	}
	if err := s.EnsureMinimalSchema(ctx); err != nil {
		return false, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer rollback(tx)
	now := string(model.NowString())
	releasedAtString := releasedAt.UTC().Format(time.RFC3339Nano)
	if sessionIdentity == "" {
		sessionIdentity = minimalLinkedWorkerSessionIdentity(generation)
	}
	if _, err := s.expireMinimalApprovalsForSessionTx(ctx, tx, sessionIdentity, now); err != nil {
		return false, err
	}
	if _, err := s.expirePendingApprovalsForSessionTx(ctx, tx, sessionIdentity, now); err != nil {
		return false, err
	}
	if _, err := s.expireCallbackRoutesForSessionTx(ctx, tx, sessionIdentity); err != nil {
		return false, err
	}
	result, err := tx.ExecContext(ctx, `
	UPDATE minimal_linked_threads
	SET state = ?, active_turn_id = NULL, worker_generation = 0, released_at = ?, updated_at = ?
	WHERE linked_thread_id = ? AND worker_generation = ? AND state = ? AND active_turn_id = ?`,
		model.MinimalLinkedReady, releasedAtString, releasedAtString, linkedID, generationValue, model.MinimalLinkedReleasePending, turnID)
	if err != nil {
		return false, err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	if changed != 1 {
		return false, nil
	}
	if ready != nil {
		available := ready.AvailableAt
		if available == "" {
			available = model.TimeString(now)
		}
		created := ready.CreatedAt
		if created == "" {
			created = model.TimeString(now)
		}
		updated := ready.UpdatedAt
		if updated == "" {
			updated = model.TimeString(now)
		}
		_, err = tx.ExecContext(ctx, `
		INSERT INTO delivery_queue(event_id, chat_key, chat_id, topic_id, thread_id, kind, status, retry_count, available_at, last_error, payload_json, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(event_id, chat_key) DO NOTHING`,
			ready.EventID, ready.ChatKey, ready.ChatID, ready.TopicID, ready.ThreadID, ready.Kind, model.DeliveryStatusPending, 0, available, nil, protectedReady, created, updated,
		)
		if err != nil {
			return false, err
		}
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return true, nil
}

func minimalLinkedWorkerSessionIdentity(generation uint64) string {
	if generation == 0 {
		return ""
	}
	return fmt.Sprintf("minimal-link-worker:%d", generation)
}

func (s *Store) ExpireMinimalApprovalsForThreadTurn(ctx context.Context, threadID, turnID string) (int64, error) {
	threadID = strings.TrimSpace(threadID)
	turnID = strings.TrimSpace(turnID)
	if threadID == "" || turnID == "" {
		return 0, nil
	}
	if err := s.EnsureMinimalSchema(ctx); err != nil {
		return 0, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer rollback(tx)
	now := string(model.NowString())
	result, err := tx.ExecContext(ctx, `UPDATE minimal_approvals SET status='expired',updated_at=? WHERE thread_id=? AND turn_id=? AND status='pending' AND claim_state='idle'`, now, threadID, turnID)
	if err != nil {
		return 0, err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return 0, err
	}
	if changed > 0 {
		if _, err := tx.ExecContext(ctx, `
		UPDATE minimal_approval_routes
		SET status='expired'
		WHERE status='active' AND request_id IN (
			SELECT request_id FROM minimal_approvals WHERE thread_id=? AND turn_id=? AND status='expired'
		)`, threadID, turnID); err != nil {
			return 0, err
		}
		if _, err := tx.ExecContext(ctx, `
		UPDATE delivery_queue
		SET status='dead', payload_json=NULL, updated_at=?
		WHERE status IN ('pending','retry','processing') AND id IN (
			SELECT delivery_queue_id FROM minimal_approvals WHERE thread_id=? AND turn_id=? AND status='expired'
		)`, now, threadID, turnID); err != nil {
			return 0, err
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return changed, nil
}

func (s *Store) ClaimDeliveryBatch(ctx context.Context, limit int) ([]model.DeliveryQueueItem, error) {
	if limit <= 0 {
		limit = 10
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer rollback(tx)
	rows, err := tx.QueryContext(ctx, `
	SELECT q.id, q.event_id, q.chat_key, q.chat_id, q.topic_id, q.thread_id, q.kind, q.status, q.retry_count, q.available_at, coalesce(q.last_error,''), q.payload_json, q.created_at, q.updated_at, coalesce(q.group_id,''), q.sequence_no, q.sequence_count
	FROM delivery_queue q
	WHERE q.status IN ('pending', 'retry') AND q.available_at <= ?
	AND (q.group_id IS NULL OR NOT EXISTS (SELECT 1 FROM delivery_queue earlier WHERE earlier.group_id=q.group_id AND earlier.sequence_no < q.sequence_no AND earlier.status != 'delivered'))
	ORDER BY id
	LIMIT ?`, string(model.NowString()), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []model.DeliveryQueueItem{}
	ids := []int64{}
	for rows.Next() {
		var item model.DeliveryQueueItem
		if err := rows.Scan(&item.ID, &item.EventID, &item.ChatKey, &item.ChatID, &item.TopicID, &item.ThreadID, &item.Kind, &item.Status, &item.RetryCount, &item.AvailableAt, &item.LastError, &item.PayloadJSON, &item.CreatedAt, &item.UpdatedAt, &item.GroupID, &item.SequenceNo, &item.SequenceCount); err != nil {
			return nil, err
		}
		if s.protector != nil {
			plaintext, err := s.protector.Unprotect(ctx, item.PayloadJSON)
			if err != nil {
				return nil, fmt.Errorf("unprotect delivery payload: %w", err)
			}
			item.PayloadJSON = string(plaintext)
		}
		out = append(out, item)
		ids = append(ids, item.ID)
	}
	for _, id := range ids {
		if _, err := tx.ExecContext(ctx, `UPDATE delivery_queue SET status = ?, updated_at = ? WHERE id = ?`, model.DeliveryStatusProcessing, string(model.NowString()), id); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return out, nil
}

func (s *Store) CompleteDelivery(ctx context.Context, queueID int64) error {
	return s.completeDelivery(ctx, queueID, nil)
}

func (s *Store) CompleteDeliveryWithRoute(ctx context.Context, queueID int64, route model.MessageRoute) error {
	return s.completeDelivery(ctx, queueID, &route)
}

func (s *Store) completeDelivery(ctx context.Context, queueID int64, route *model.MessageRoute) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer rollback(tx)
	now := string(model.NowString())
	if route != nil {
		if route.CreatedAt == "" {
			route.CreatedAt = model.TimeString(now)
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO telegram_message_routes(chat_id,topic_id,message_id,thread_id,turn_id,item_id,event_id,created_at) VALUES(?,?,?,?,?,?,?,?) ON CONFLICT(chat_id,topic_id,message_id) DO UPDATE SET thread_id=excluded.thread_id,turn_id=excluded.turn_id,item_id=excluded.item_id,event_id=excluded.event_id,created_at=excluded.created_at`, route.ChatID, route.TopicID, route.MessageID, route.ThreadID, nullable(route.TurnID), nullable(route.ItemID), nullable(route.EventID), route.CreatedAt); err != nil {
			return err
		}
	}
	query := `UPDATE delivery_queue SET status=?, updated_at=? WHERE id=?`
	if s.protector != nil {
		query = `UPDATE delivery_queue SET status=?, payload_json=NULL, updated_at=? WHERE id=?`
	}
	if _, err = tx.ExecContext(ctx, query, model.DeliveryStatusDelivered, now, queueID); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE terminal_events SET delivery_status=?,updated_at=? WHERE terminal_key=(SELECT group_id FROM delivery_queue WHERE id=? AND sequence_no=sequence_count)`, model.DeliveryStatusDelivered, now, queueID); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) FailDelivery(ctx context.Context, queueID int64, retryCount int, availableAt time.Time, errText string, dead bool) error {
	status := model.DeliveryStatusRetry
	if dead {
		status = model.DeliveryStatusDead
	}
	if dead {
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer rollback(tx)
		now := string(model.NowString())
		query := `UPDATE delivery_queue SET status=?,retry_count=?,available_at=?,last_error=?,updated_at=? WHERE id=?`
		if s.protector != nil {
			query = `UPDATE delivery_queue SET status=?,retry_count=?,available_at=?,last_error=?,payload_json=NULL,updated_at=? WHERE id=?`
		}
		if _, err := tx.ExecContext(ctx, query, status, retryCount, availableAt.UTC().Format(time.RFC3339Nano), nullable(errText), now, queueID); err != nil {
			return err
		}
		var groupID sql.NullString
		if err := tx.QueryRowContext(ctx, `SELECT group_id FROM delivery_queue WHERE id=?`, queueID).Scan(&groupID); err != nil {
			return err
		}
		if groupID.Valid && strings.TrimSpace(groupID.String) != "" {
			if _, err := tx.ExecContext(ctx, `UPDATE terminal_events SET delivery_status=?,updated_at=? WHERE terminal_key=?`, model.DeliveryStatusDead, now, groupID.String); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `UPDATE delivery_queue SET status=?,payload_json=NULL,updated_at=? WHERE group_id=? AND status!='delivered'`, model.DeliveryStatusDead, now, groupID.String); err != nil {
				return err
			}
		}
		return tx.Commit()
	}
	_, err := s.db.ExecContext(ctx, `
	UPDATE delivery_queue SET status = ?, retry_count = ?, available_at = ?, last_error = ?, updated_at = ? WHERE id = ?`,
		status, retryCount, availableAt.UTC().Format(time.RFC3339Nano), nullable(errText), string(model.NowString()), queueID,
	)
	return err
}

func (s *Store) RecordDeliveryAttempt(ctx context.Context, queueID int64, attemptNo int, status, errText string) error {
	_, err := s.db.ExecContext(ctx, `
	INSERT INTO delivery_attempts(queue_id, attempt_no, status, error_text, created_at)
	VALUES (?, ?, ?, ?, ?)`,
		queueID, attemptNo, status, nullable(errText), string(model.NowString()),
	)
	return err
}

func (s *Store) DeliveryQueueBacklog(ctx context.Context) (int, error) {
	row := s.db.QueryRowContext(ctx, `SELECT count(*) FROM delivery_queue WHERE status IN ('pending', 'retry', 'processing')`)
	var count int
	return count, row.Scan(&count)
}

func (s *Store) SetState(ctx context.Context, key, value string) error {
	_, err := s.db.ExecContext(ctx, `
	INSERT INTO daemon_state(key, value, updated_at) VALUES (?, ?, ?)
	ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`,
		key, value, string(model.NowString()),
	)
	return err
}

func (s *Store) GetState(ctx context.Context, key string) (string, error) {
	row := s.db.QueryRowContext(ctx, `SELECT value FROM daemon_state WHERE key = ?`, key)
	var value string
	err := row.Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return value, err
}

func (s *Store) DeleteState(ctx context.Context, key string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM daemon_state WHERE key = ?`, key)
	return err
}

func (s *Store) ListState(ctx context.Context) (map[string]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT key, value FROM daemon_state ORDER BY key`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var key, value string
		if err := rows.Scan(&key, &value); err != nil {
			return nil, err
		}
		out[key] = value
	}
	return out, rows.Err()
}

func (s *Store) GetChatContext(ctx context.Context, chatID, topicID int64) (*model.ChatContext, error) {
	binding, err := s.GetBinding(ctx, chatID, topicID)
	if err != nil {
		return nil, err
	}
	globalTarget, _, err := s.GetGlobalObserverTarget(ctx)
	if err != nil {
		return nil, err
	}
	observerEnabled := globalTarget != nil && globalTarget.ChatID == chatID && globalTarget.TopicID == topicID
	contextState := &model.ChatContext{Mode: "unbound", Binding: binding, ObserverEnabled: observerEnabled, ObserverTarget: globalTarget}
	if observerEnabled {
		contextState.Mode = model.BindingModeObserver
	}
	if binding != nil {
		contextState.Mode = binding.Mode
		thread, err := s.GetThread(ctx, binding.ThreadID)
		if err != nil {
			return nil, err
		}
		contextState.Thread = thread
	}
	return contextState, nil
}

func scanThread(scanner interface{ Scan(...any) error }) (*model.Thread, error) {
	var thread model.Thread
	var cwd, directoryName, status, lastPreview, activeTurnID, preferredModel, permissionsMode sql.NullString
	var raw string
	var archived int
	if err := scanner.Scan(&thread.ID, &thread.Title, &cwd, &thread.ProjectName, &directoryName, &thread.UpdatedAt, &status, &lastPreview, &activeTurnID, &preferredModel, &permissionsMode, &archived, &raw); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	thread.CWD = cwd.String
	thread.DirectoryName = directoryName.String
	thread.Status = status.String
	thread.LastPreview = lastPreview.String
	thread.ActiveTurnID = activeTurnID.String
	thread.PreferredModel = preferredModel.String
	thread.PermissionsMode = permissionsMode.String
	thread.Archived = archived == 1
	thread.Raw = json.RawMessage(raw)
	return &thread, nil
}

func nullable(value string) any {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	return value
}

func boolToInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

func rollback(tx *sql.Tx) {
	_ = tx.Rollback()
}

func MustJSON(value any) string {
	payload, err := json.Marshal(value)
	if err != nil {
		panic(fmt.Errorf("marshal payload: %w", err))
	}
	return string(payload)
}
