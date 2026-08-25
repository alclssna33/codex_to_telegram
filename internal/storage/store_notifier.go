package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/alclssna33/codex_to_telegram/internal/model"
)

const (
	notifierActivationStateKey = "notifier.activation_unix"
	notifierMigrationStateKey  = "notifier.migration.v1"
)

func (s *Store) EnsureNotifierSchema(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `
	CREATE TABLE IF NOT EXISTS notifier_observations (
		thread_id TEXT PRIMARY KEY,
		last_updated_at INTEGER NOT NULL DEFAULT 0,
		last_turn_id TEXT,
		last_turn_status TEXT,
		baseline_ready INTEGER NOT NULL DEFAULT 0,
		read_required INTEGER NOT NULL DEFAULT 1,
		defer_until INTEGER NOT NULL DEFAULT 0,
		retired INTEGER NOT NULL DEFAULT 0,
		discovery_seq INTEGER NOT NULL,
		updated_at TEXT NOT NULL
	);
	CREATE INDEX IF NOT EXISTS idx_notifier_observations_due
	ON notifier_observations(read_required, discovery_seq);
	CREATE INDEX IF NOT EXISTS idx_notifier_observations_due_recent
	ON notifier_observations(read_required, retired, last_updated_at DESC, thread_id ASC);`)
	if err != nil {
		return err
	}
	if err := s.ensureNotifierColumn(ctx, "notifier_observations", "defer_until", "INTEGER NOT NULL DEFAULT 0"); err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `CREATE INDEX IF NOT EXISTS idx_notifier_observations_due_deferred
	ON notifier_observations(read_required, retired, defer_until, last_updated_at DESC, thread_id ASC)`)
	return err
}

func (s *Store) EnsureNotifierActivation(ctx context.Context, now time.Time) (int64, error) {
	if now.IsZero() {
		now = time.Now().UTC()
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer rollback(tx)
	activation := now.UTC().Unix()
	updatedAt := now.UTC().Format(time.RFC3339Nano)
	if _, err := tx.ExecContext(ctx, `
	INSERT INTO daemon_state(key, value, updated_at)
	VALUES (?, ?, ?)
	ON CONFLICT(key) DO NOTHING`,
		notifierActivationStateKey, strconv.FormatInt(activation, 10), updatedAt); err != nil {
		return 0, err
	}
	var raw string
	if err := tx.QueryRowContext(ctx, `SELECT value FROM daemon_state WHERE key = ?`, notifierActivationStateKey).Scan(&raw); err != nil {
		return 0, err
	}
	stored, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return stored, nil
}

func (s *Store) MigrateNotifierProfile(ctx context.Context, now time.Time) (int64, error) {
	if now.IsZero() {
		now = time.Now().UTC()
	}
	updatedAt := now.UTC().Format(time.RFC3339Nano)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer rollback(tx)
	var state string
	err = tx.QueryRowContext(ctx, `SELECT value FROM daemon_state WHERE key = ?`, notifierMigrationStateKey).Scan(&state)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return 0, err
	}
	if state == "done" {
		if err := tx.Commit(); err != nil {
			return 0, err
		}
		return 0, nil
	}
	if _, err := tx.ExecContext(ctx, `
	UPDATE terminal_events
	SET delivery_status = ?, updated_at = ?
	WHERE terminal_key IN (
		SELECT DISTINCT group_id
		FROM delivery_queue
		WHERE status IN (?, ?)
		  AND kind != 'notifier_terminal'
		  AND coalesce(group_id, '') != ''
	)
	AND NOT EXISTS (
		SELECT 1
		FROM delivery_queue q
		WHERE q.group_id = terminal_events.terminal_key
		  AND q.kind = 'notifier_terminal'
		  AND q.status IN (?, ?)
	)`,
		model.DeliveryStatusDead, updatedAt,
		model.DeliveryStatusPending, model.DeliveryStatusRetry,
		model.DeliveryStatusPending, model.DeliveryStatusRetry); err != nil {
		return 0, err
	}
	result, err := tx.ExecContext(ctx, `
	UPDATE delivery_queue
	SET status = ?, payload_json = NULL, updated_at = ?
	WHERE status IN (?, ?)
	  AND kind != 'notifier_terminal'`,
		model.DeliveryStatusDead, updatedAt,
		model.DeliveryStatusPending, model.DeliveryStatusRetry)
	if err != nil {
		return 0, err
	}
	retired, err := result.RowsAffected()
	if err != nil {
		return 0, err
	}
	if _, err := tx.ExecContext(ctx, `
	INSERT INTO daemon_state(key, value, updated_at)
	VALUES (?, 'done', ?)
	ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`,
		notifierMigrationStateKey, updatedAt); err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return retired, nil
}

func (s *Store) ObserveNotifierThread(ctx context.Context, threadID string, lastUpdatedAt int64, now time.Time) error {
	threadID = strings.TrimSpace(threadID)
	if threadID == "" {
		return errors.New("notifier observation thread id is required")
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	if err := s.EnsureNotifierSchema(ctx); err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer rollback(tx)
	if _, err := tx.ExecContext(ctx, `
	INSERT INTO notifier_observations(thread_id, last_updated_at, baseline_ready, read_required, discovery_seq, updated_at)
	VALUES (?, ?, 0, 1, (SELECT coalesce(max(discovery_seq), 0) + 1 FROM notifier_observations), ?)
	ON CONFLICT(thread_id) DO UPDATE SET
		last_updated_at = excluded.last_updated_at,
		read_required = 1,
		defer_until = 0,
		updated_at = excluded.updated_at
	-- Codex can return a slightly older thread/list timestamp than the
	-- previously stored thread/read timestamp. Any changed list watermark
	-- therefore needs a bounded re-read; identical values stay idle.
	WHERE excluded.last_updated_at <> notifier_observations.last_updated_at`,
		threadID, lastUpdatedAt, now.UTC().Format(time.RFC3339Nano)); err != nil {
		return err
	}
	return tx.Commit()
}

// BaselineNotifierObservationsAtOrBefore drops pre-activation history without
// reading it. A later thread/list update re-arms the observation normally.
func (s *Store) BaselineNotifierObservationsAtOrBefore(ctx context.Context, activationUnix int64, now time.Time) error {
	if now.IsZero() {
		now = time.Now().UTC()
	}
	if err := s.EnsureNotifierSchema(ctx); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, `
	UPDATE notifier_observations
	SET baseline_ready = 1,
	    read_required = 0,
	    updated_at = ?
	WHERE retired = 0
	  AND last_updated_at <= ?`, now.UTC().Format(time.RFC3339Nano), activationUnix)
	return err
}

func (s *Store) ListNotifierObservationsDue(ctx context.Context, limit int) ([]model.NotifierObservation, error) {
	return s.ListNotifierObservationsDueAt(ctx, limit, time.Now().UTC())
}

func (s *Store) ListNotifierObservationsDueAt(ctx context.Context, limit int, now time.Time) ([]model.NotifierObservation, error) {
	if limit <= 0 {
		limit = 50
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	if err := s.EnsureNotifierSchema(ctx); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `
	SELECT thread_id, last_updated_at, coalesce(last_turn_id, ''), coalesce(last_turn_status, ''),
	       baseline_ready, read_required, defer_until, discovery_seq
	FROM notifier_observations
	WHERE read_required = 1
	  AND retired = 0
	  AND defer_until <= ?
	ORDER BY last_updated_at DESC, thread_id ASC
	LIMIT ?`, now.UTC().Unix(), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []model.NotifierObservation{}
	for rows.Next() {
		observation, err := scanNotifierObservation(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, observation)
	}
	return out, rows.Err()
}

// DeferNotifierRead preserves a required read but prevents one slow Codex
// thread from retrying on every observer cycle.
func (s *Store) DeferNotifierRead(ctx context.Context, threadID string, retryAt, now time.Time) error {
	threadID = strings.TrimSpace(threadID)
	if threadID == "" {
		return errors.New("notifier observation thread id is required")
	}
	if retryAt.IsZero() {
		retryAt = time.Now().UTC()
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	if err := s.EnsureNotifierSchema(ctx); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, `
	UPDATE notifier_observations
	SET read_required = 1, defer_until = ?, updated_at = ?
	WHERE thread_id = ? AND retired = 0`,
		retryAt.UTC().Unix(), now.UTC().Format(time.RFC3339Nano), threadID)
	return err
}

func (s *Store) RecordNotifierRead(ctx context.Context, threadID, turnID, status string, lastUpdatedAt int64, keepPolling bool, now time.Time) error {
	threadID = strings.TrimSpace(threadID)
	turnID = strings.TrimSpace(turnID)
	status = strings.TrimSpace(status)
	if threadID == "" {
		return errors.New("notifier observation thread id is required")
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	if err := s.EnsureNotifierSchema(ctx); err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer rollback(tx)
	if _, err := tx.ExecContext(ctx, `
	INSERT INTO notifier_observations(
		thread_id, last_updated_at, last_turn_id, last_turn_status,
		baseline_ready, read_required, discovery_seq, updated_at
	)
	VALUES (?, ?, ?, ?, 1, ?, (SELECT coalesce(max(discovery_seq), 0) + 1 FROM notifier_observations), ?)
	ON CONFLICT(thread_id) DO UPDATE SET
		-- last_updated_at is the thread/list discovery watermark. A detailed
		-- thread/read response may carry a slightly newer timestamp, but must
		-- not suppress a later list update that needs another read.
		last_updated_at = notifier_observations.last_updated_at,
		last_turn_id = excluded.last_turn_id,
		last_turn_status = excluded.last_turn_status,
		baseline_ready = 1,
		read_required = excluded.read_required,
		defer_until = 0,
		updated_at = excluded.updated_at`,
		threadID, lastUpdatedAt, nullable(turnID), nullable(status), boolToInt(keepPolling), now.UTC().Format(time.RFC3339Nano)); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) NotifierObservation(ctx context.Context, threadID string) (*model.NotifierObservation, error) {
	threadID = strings.TrimSpace(threadID)
	if threadID == "" {
		return nil, nil
	}
	if err := s.EnsureNotifierSchema(ctx); err != nil {
		return nil, err
	}
	row := s.db.QueryRowContext(ctx, `
	SELECT thread_id, last_updated_at, coalesce(last_turn_id, ''), coalesce(last_turn_status, ''),
	       baseline_ready, read_required, defer_until, discovery_seq
	FROM notifier_observations
	WHERE thread_id = ?`, threadID)
	observation, err := scanNotifierObservation(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &observation, nil
}

func (s *Store) CountNotifierObservations(ctx context.Context) (int, int, error) {
	if err := s.EnsureNotifierSchema(ctx); err != nil {
		return 0, 0, err
	}
	var tracked, active int
	if err := s.db.QueryRowContext(ctx, `
	SELECT count(*),
	       coalesce(sum(CASE WHEN read_required = 1 THEN 1 ELSE 0 END), 0)
	FROM notifier_observations
	WHERE retired = 0`).Scan(&tracked, &active); err != nil {
		return 0, 0, err
	}
	return tracked, active, nil
}

type notifierObservationScanner interface {
	Scan(...any) error
}

func scanNotifierObservation(scanner notifierObservationScanner) (model.NotifierObservation, error) {
	var observation model.NotifierObservation
	var baselineReady, readRequired int
	if err := scanner.Scan(
		&observation.ThreadID,
		&observation.LastUpdatedAt,
		&observation.LastTurnID,
		&observation.LastTurnStatus,
		&baselineReady,
		&readRequired,
		&observation.DeferUntil,
		&observation.DiscoverySeq,
	); err != nil {
		return model.NotifierObservation{}, err
	}
	observation.BaselineReady = baselineReady != 0
	observation.ReadRequired = readRequired != 0
	return observation, nil
}

func (s *Store) ensureNotifierColumn(ctx context.Context, tableName, columnName, definition string) error {
	present, err := s.hasColumn(ctx, tableName, columnName)
	if err != nil || present {
		return err
	}
	_, err = s.db.ExecContext(ctx, fmt.Sprintf("ALTER TABLE %s ADD COLUMN %s %s", tableName, columnName, definition))
	return err
}
