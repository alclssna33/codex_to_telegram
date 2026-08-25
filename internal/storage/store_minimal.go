package storage

import (
	"context"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/alclssna33/codex_to_telegram/internal/model"
)

func (s *Store) EnsureMinimalSchema(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `
	CREATE TABLE IF NOT EXISTS selected_projects (
		chat_key TEXT PRIMARY KEY,
		project_id TEXT NOT NULL,
		updated_at TEXT NOT NULL
	);
	CREATE TABLE IF NOT EXISTS pending_commands (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		thread_id TEXT NOT NULL,
		source_thread_id TEXT NOT NULL DEFAULT '',
		source_turn_id TEXT NOT NULL DEFAULT '',
		project_id TEXT NOT NULL,
		chat_id INTEGER NOT NULL DEFAULT 0,
		topic_id INTEGER NOT NULL DEFAULT 0,
		prompt_payload TEXT,
		status TEXT NOT NULL,
		created_at TEXT NOT NULL
	);
	CREATE INDEX IF NOT EXISTS idx_pending_commands_thread_fifo
		ON pending_commands(thread_id, status, id);
	CREATE TABLE IF NOT EXISTS minimal_thread_continuations (
		chat_key TEXT NOT NULL,
		chat_id INTEGER NOT NULL,
		topic_id INTEGER NOT NULL DEFAULT 0,
		project_id TEXT NOT NULL,
		source_thread_id TEXT NOT NULL,
		source_turn_id TEXT NOT NULL,
		fork_thread_id TEXT,
		status TEXT NOT NULL,
		failure_kind TEXT,
		created_at TEXT NOT NULL,
		updated_at TEXT NOT NULL,
		PRIMARY KEY(chat_key, source_thread_id, source_turn_id)
	);
	CREATE UNIQUE INDEX IF NOT EXISTS idx_minimal_continuation_fork
		ON minimal_thread_continuations(fork_thread_id)
		WHERE fork_thread_id IS NOT NULL AND fork_thread_id <> '';
	CREATE TABLE IF NOT EXISTS minimal_linked_threads (
		chat_key TEXT NOT NULL,
		chat_id INTEGER NOT NULL,
		topic_id INTEGER NOT NULL DEFAULT 0,
		project_id TEXT NOT NULL,
		source_thread_id TEXT NOT NULL,
		linked_thread_id TEXT,
		source_anchor_turn_id TEXT NOT NULL,
		source_title_payload TEXT,
		desired_title_payload TEXT,
		title_state TEXT NOT NULL,
		state TEXT NOT NULL,
		active_turn_id TEXT,
		worker_generation INTEGER NOT NULL DEFAULT 0,
		last_blocked_at TEXT,
		last_blocked_code TEXT,
		failure_kind TEXT,
		created_at TEXT NOT NULL,
		updated_at TEXT NOT NULL,
		released_at TEXT,
		PRIMARY KEY(chat_key, source_thread_id)
	);
	CREATE UNIQUE INDEX IF NOT EXISTS idx_minimal_linked_threads_linked_id
		ON minimal_linked_threads(linked_thread_id)
		WHERE linked_thread_id IS NOT NULL AND linked_thread_id <> '';
	CREATE TABLE IF NOT EXISTS voice_confirmations (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		project_id TEXT NOT NULL,
		target_kind TEXT NOT NULL,
		thread_id TEXT,
		source_turn_id TEXT NOT NULL DEFAULT '',
		transcript_payload TEXT,
		session_identity TEXT NOT NULL,
		status TEXT NOT NULL,
		expires_at TEXT NOT NULL,
		chat_id INTEGER NOT NULL DEFAULT 0,
		topic_id INTEGER NOT NULL DEFAULT 0,
		telegram_message_id INTEGER NOT NULL DEFAULT 0,
		created_at TEXT NOT NULL
	);
	CREATE TABLE IF NOT EXISTS voice_callback_routes (
		route_token TEXT PRIMARY KEY,
		voice_id INTEGER NOT NULL,
		action TEXT NOT NULL,
		status TEXT NOT NULL,
		chat_id INTEGER NOT NULL DEFAULT 0,
		topic_id INTEGER NOT NULL DEFAULT 0,
		telegram_message_id INTEGER NOT NULL DEFAULT 0,
		created_at TEXT NOT NULL,
		FOREIGN KEY(voice_id) REFERENCES voice_confirmations(id)
	);
	CREATE INDEX IF NOT EXISTS idx_voice_callback_routes_voice
		ON voice_callback_routes(voice_id, status);
	CREATE TABLE IF NOT EXISTS minimal_picker_routes (
		route_token TEXT PRIMARY KEY,
		action TEXT NOT NULL,
		project_id TEXT NOT NULL,
		thread_id TEXT,
		page INTEGER NOT NULL DEFAULT 0,
		chat_id INTEGER NOT NULL,
		topic_id INTEGER NOT NULL,
		status TEXT NOT NULL,
		expires_at TEXT NOT NULL,
		created_at TEXT NOT NULL
	);
	CREATE TABLE IF NOT EXISTS minimal_thread_observations (
		thread_id TEXT PRIMARY KEY,
		project_id TEXT NOT NULL,
		last_updated_at INTEGER NOT NULL,
		last_turn_id TEXT,
		last_turn_status TEXT,
		baseline_ready INTEGER NOT NULL,
		read_required INTEGER NOT NULL,
		retired INTEGER NOT NULL DEFAULT 0,
		discovery_seq INTEGER NOT NULL DEFAULT 0,
		updated_at TEXT NOT NULL
	);
	CREATE INDEX IF NOT EXISTS idx_minimal_thread_observations_due
		ON minimal_thread_observations(read_required, discovery_seq)`)
	if err != nil {
		return err
	}
	if err := s.ensureMinimalObservationDiscoverySequence(ctx); err != nil {
		return err
	}
	if err := s.ensureMinimalColumn(ctx, "minimal_thread_observations", "retired", "INTEGER NOT NULL DEFAULT 0"); err != nil {
		return err
	}
	for _, column := range []struct {
		name       string
		definition string
	}{
		{name: "chat_id", definition: "INTEGER NOT NULL DEFAULT 0"},
		{name: "topic_id", definition: "INTEGER NOT NULL DEFAULT 0"},
		{name: "source_thread_id", definition: "TEXT NOT NULL DEFAULT ''"},
		{name: "source_turn_id", definition: "TEXT NOT NULL DEFAULT ''"},
	} {
		if err := s.ensureMinimalColumn(ctx, "pending_commands", column.name, column.definition); err != nil {
			return err
		}
	}
	if err := s.ensureMinimalColumn(ctx, "voice_confirmations", "source_turn_id", "TEXT NOT NULL DEFAULT ''"); err != nil {
		return err
	}
	for _, column := range []struct {
		name       string
		definition string
	}{
		{name: "chat_id", definition: "INTEGER NOT NULL DEFAULT 0"},
		{name: "topic_id", definition: "INTEGER NOT NULL DEFAULT 0"},
		{name: "project_id", definition: "TEXT NOT NULL DEFAULT ''"},
		{name: "linked_thread_id", definition: "TEXT"},
		{name: "source_anchor_turn_id", definition: "TEXT NOT NULL DEFAULT ''"},
		{name: "source_title_payload", definition: "TEXT"},
		{name: "desired_title_payload", definition: "TEXT"},
		{name: "title_state", definition: "TEXT NOT NULL DEFAULT 'pending'"},
		{name: "state", definition: "TEXT NOT NULL DEFAULT 'ready'"},
		{name: "active_turn_id", definition: "TEXT"},
		{name: "worker_generation", definition: "INTEGER NOT NULL DEFAULT 0"},
		{name: "last_blocked_at", definition: "TEXT"},
		{name: "last_blocked_code", definition: "TEXT"},
		{name: "failure_kind", definition: "TEXT"},
		{name: "created_at", definition: "TEXT NOT NULL DEFAULT ''"},
		{name: "updated_at", definition: "TEXT NOT NULL DEFAULT ''"},
		{name: "released_at", definition: "TEXT"},
	} {
		if err := s.ensureMinimalColumn(ctx, "minimal_linked_threads", column.name, column.definition); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) ClaimMinimalContinuation(ctx context.Context, continuation model.MinimalContinuation) (*model.MinimalContinuation, bool, error) {
	seed, err := normalizeMinimalContinuation(continuation)
	if err != nil {
		return nil, false, err
	}
	if err := s.EnsureMinimalSchema(ctx); err != nil {
		return nil, false, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, false, err
	}
	defer rollback(tx)
	now := model.NowString()
	chatKey := model.ChatKey(seed.Key.ChatID, seed.Key.TopicID)
	result, err := tx.ExecContext(ctx, `
	UPDATE minimal_thread_continuations
	SET status = ?, failure_kind = NULL, updated_at = ?
	WHERE chat_key = ? AND source_thread_id = ? AND source_turn_id = ?
	  AND status = ? AND failure_kind = ? AND coalesce(fork_thread_id, '') = ''`,
		model.MinimalContinuationCreating, now, chatKey, seed.Key.SourceThreadID, seed.Key.SourceTurnID,
		model.MinimalContinuationFailed, model.MinimalContinuationFailureDefinite)
	if err != nil {
		return nil, false, err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return nil, false, err
	}
	created := changed == 1
	if !created {
		result, err = tx.ExecContext(ctx, `
		INSERT INTO minimal_thread_continuations(chat_key, chat_id, topic_id, project_id, source_thread_id, source_turn_id, fork_thread_id, status, failure_kind, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, NULL, ?, NULL, ?, ?)
		ON CONFLICT(chat_key, source_thread_id, source_turn_id) DO NOTHING`,
			chatKey, seed.Key.ChatID, seed.Key.TopicID, seed.ProjectID, seed.Key.SourceThreadID, seed.Key.SourceTurnID,
			model.MinimalContinuationCreating, now, now)
		if err != nil {
			return nil, false, err
		}
		changed, err = result.RowsAffected()
		if err != nil {
			return nil, false, err
		}
		created = changed == 1
	}
	row, err := getMinimalContinuationTx(ctx, tx, seed.Key)
	if err != nil {
		return nil, false, err
	}
	if err := tx.Commit(); err != nil {
		return nil, false, err
	}
	return row, created, nil
}

func (s *Store) GetMinimalContinuation(ctx context.Context, key model.MinimalContinuationKey) (*model.MinimalContinuation, error) {
	normalized, err := normalizeMinimalContinuationKey(key)
	if err != nil {
		return nil, err
	}
	if err := s.EnsureMinimalSchema(ctx); err != nil {
		return nil, err
	}
	return getMinimalContinuationTx(ctx, s.db, normalized)
}

func (s *Store) ActiveMinimalContinuationForFork(ctx context.Context, chatID, topicID int64, forkThreadID string) (*model.MinimalContinuation, error) {
	forkThreadID = strings.TrimSpace(forkThreadID)
	if chatID == 0 || forkThreadID == "" {
		return nil, nil
	}
	if err := s.EnsureMinimalSchema(ctx); err != nil {
		return nil, err
	}
	var continuation model.MinimalContinuation
	var failureKind sql.NullString
	err := s.db.QueryRowContext(ctx, `
	SELECT chat_id, topic_id, project_id, source_thread_id, source_turn_id, fork_thread_id, status, coalesce(failure_kind, '')
	FROM minimal_thread_continuations
	WHERE chat_key = ? AND fork_thread_id = ? AND status = ?
	ORDER BY updated_at DESC
	LIMIT 1`,
		model.ChatKey(chatID, topicID), forkThreadID, model.MinimalContinuationActive,
	).Scan(
		&continuation.Key.ChatID, &continuation.Key.TopicID, &continuation.ProjectID, &continuation.Key.SourceThreadID,
		&continuation.Key.SourceTurnID, &continuation.ForkThreadID, &continuation.Status, &failureKind,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	continuation.FailureKind = failureKind.String
	return &continuation, nil
}

func (s *Store) ActiveMinimalContinuationByFork(ctx context.Context, forkThreadID string) (*model.MinimalContinuation, error) {
	forkThreadID = strings.TrimSpace(forkThreadID)
	if forkThreadID == "" {
		return nil, nil
	}
	if err := s.EnsureMinimalSchema(ctx); err != nil {
		return nil, err
	}
	var continuation model.MinimalContinuation
	var failureKind sql.NullString
	err := s.db.QueryRowContext(ctx, `
	SELECT chat_id, topic_id, project_id, source_thread_id, source_turn_id, fork_thread_id, status, coalesce(failure_kind, '')
	FROM minimal_thread_continuations
	WHERE fork_thread_id = ? AND status = ?
	ORDER BY updated_at DESC
	LIMIT 1`,
		forkThreadID, model.MinimalContinuationActive,
	).Scan(
		&continuation.Key.ChatID, &continuation.Key.TopicID, &continuation.ProjectID, &continuation.Key.SourceThreadID,
		&continuation.Key.SourceTurnID, &continuation.ForkThreadID, &continuation.Status, &failureKind,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	continuation.FailureKind = failureKind.String
	return &continuation, nil
}

func (s *Store) ActivateMinimalContinuation(ctx context.Context, continuation model.MinimalContinuation, child model.Thread) error {
	seed, err := normalizeMinimalContinuation(continuation)
	if err != nil {
		return err
	}
	child.ID = strings.TrimSpace(child.ID)
	if child.ID == "" {
		return errors.New("child thread id is required")
	}
	if err := s.EnsureMinimalSchema(ctx); err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer rollback(tx)
	if err := upsertThreadTx(ctx, tx, child); err != nil {
		return err
	}
	now := model.NowString()
	chatKey := model.ChatKey(seed.Key.ChatID, seed.Key.TopicID)
	result, err := tx.ExecContext(ctx, `
	UPDATE minimal_thread_continuations
	SET fork_thread_id = ?, status = ?, failure_kind = NULL, updated_at = ?
	WHERE chat_key = ? AND source_thread_id = ? AND source_turn_id = ? AND status = ?`,
		child.ID, model.MinimalContinuationActive, now, chatKey, seed.Key.SourceThreadID, seed.Key.SourceTurnID, model.MinimalContinuationCreating)
	if err != nil {
		return err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if changed != 1 {
		return fmt.Errorf("activate minimal continuation changed %d rows, want 1", changed)
	}
	if _, err := tx.ExecContext(ctx, `
	UPDATE pending_commands
	SET thread_id = ?
	WHERE chat_id = ? AND topic_id = ? AND source_thread_id = ? AND source_turn_id = ? AND status IN (?, ?)`,
		child.ID, seed.Key.ChatID, seed.Key.TopicID, seed.Key.SourceThreadID, seed.Key.SourceTurnID,
		model.PendingCommandStatusPending, model.PendingCommandStatusClaimed); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `
	INSERT INTO thread_bindings(chat_key, chat_id, topic_id, thread_id, mode, created_at, updated_at)
	VALUES (?, ?, ?, ?, ?, ?, ?)
	ON CONFLICT(chat_key) DO UPDATE SET thread_id = excluded.thread_id, mode = excluded.mode, updated_at = excluded.updated_at`,
		chatKey, seed.Key.ChatID, seed.Key.TopicID, child.ID, model.BindingModeBound, now, now); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) FailMinimalContinuation(ctx context.Context, continuation model.MinimalContinuation, failureKind string) error {
	seed, err := normalizeMinimalContinuation(continuation)
	if err != nil {
		return err
	}
	failureKind = strings.TrimSpace(failureKind)
	if failureKind != model.MinimalContinuationFailureDefinite && failureKind != model.MinimalContinuationFailureAmbiguous {
		return errors.New("minimal continuation failure kind must be definite or ambiguous")
	}
	if err := s.EnsureMinimalSchema(ctx); err != nil {
		return err
	}
	result, err := s.db.ExecContext(ctx, `
	UPDATE minimal_thread_continuations
	SET status = ?, failure_kind = ?, updated_at = ?
	WHERE chat_key = ? AND source_thread_id = ? AND source_turn_id = ? AND status = ?`,
		model.MinimalContinuationFailed, failureKind, model.NowString(),
		model.ChatKey(seed.Key.ChatID, seed.Key.TopicID), seed.Key.SourceThreadID, seed.Key.SourceTurnID, model.MinimalContinuationCreating)
	if err != nil {
		return err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if changed != 1 {
		return fmt.Errorf("fail minimal continuation changed %d rows, want 1", changed)
	}
	return nil
}

func (s *Store) RecoverCreatingMinimalContinuations(ctx context.Context) (int64, error) {
	if err := s.EnsureMinimalSchema(ctx); err != nil {
		return 0, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer rollback(tx)
	if _, err := tx.ExecContext(ctx, `
	UPDATE pending_commands
	SET status = ?
	WHERE status = ?
	  AND EXISTS (
		SELECT 1 FROM minimal_thread_continuations c
		WHERE c.status = ?
		  AND c.chat_id = pending_commands.chat_id
		  AND c.topic_id = pending_commands.topic_id
		  AND c.source_thread_id = pending_commands.source_thread_id
		  AND c.source_turn_id = pending_commands.source_turn_id
	  )`,
		model.PendingCommandStatusPending, model.PendingCommandStatusClaimed, model.MinimalContinuationCreating); err != nil {
		return 0, err
	}
	result, err := tx.ExecContext(ctx, `
	UPDATE minimal_thread_continuations
	SET status = ?, failure_kind = ?, updated_at = ?
	WHERE status = ?`,
		model.MinimalContinuationFailed, model.MinimalContinuationFailureAmbiguous, model.NowString(), model.MinimalContinuationCreating)
	if err != nil {
		return 0, err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return changed, nil
}

func (s *Store) RearmAmbiguousMinimalContinuations(ctx context.Context, chatID, topicID int64) (int64, error) {
	if chatID == 0 {
		return 0, errors.New("chat id is required")
	}
	if err := s.EnsureMinimalSchema(ctx); err != nil {
		return 0, err
	}
	result, err := s.db.ExecContext(ctx, `
	UPDATE minimal_thread_continuations
	SET failure_kind = ?, updated_at = ?
	WHERE chat_key = ? AND status = ? AND failure_kind = ?`,
		model.MinimalContinuationFailureDefinite, model.NowString(), model.ChatKey(chatID, topicID),
		model.MinimalContinuationFailed, model.MinimalContinuationFailureAmbiguous)
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}

func (s *Store) AdoptMinimalLinkedThreads(ctx context.Context) (int64, error) {
	if err := s.EnsureMinimalSchema(ctx); err != nil {
		return 0, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer rollback(tx)
	rows, err := tx.QueryContext(ctx, `
	SELECT c.chat_key, c.chat_id, c.topic_id, c.project_id, c.source_thread_id, c.source_turn_id, c.fork_thread_id
	FROM minimal_thread_continuations c
	LEFT JOIN thread_bindings b ON b.chat_key = c.chat_key
	WHERE c.status = ? AND coalesce(c.fork_thread_id, '') <> ''
	  AND NOT EXISTS (
		SELECT 1 FROM minimal_linked_threads l
		WHERE l.chat_key = c.chat_key AND l.source_thread_id = c.source_thread_id
	  )
	ORDER BY c.chat_key ASC, c.source_thread_id ASC,
	         CASE WHEN b.thread_id = c.fork_thread_id THEN 0 ELSE 1 END,
	         c.updated_at DESC,
	         c.fork_thread_id DESC`, model.MinimalContinuationActive)
	if err != nil {
		return 0, err
	}
	type adoptionCandidate struct {
		chatKey        string
		chatID         int64
		topicID        int64
		projectID      string
		sourceThreadID string
		sourceTurnID   string
		forkThreadID   string
	}
	var candidates []adoptionCandidate
	seen := map[string]bool{}
	for rows.Next() {
		var candidate adoptionCandidate
		if err := rows.Scan(&candidate.chatKey, &candidate.chatID, &candidate.topicID, &candidate.projectID, &candidate.sourceThreadID, &candidate.sourceTurnID, &candidate.forkThreadID); err != nil {
			_ = rows.Close()
			return 0, err
		}
		key := candidate.chatKey + "\x00" + candidate.sourceThreadID
		if seen[key] {
			continue
		}
		seen[key] = true
		candidates = append(candidates, candidate)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return 0, err
	}
	if err := rows.Close(); err != nil {
		return 0, err
	}
	now := string(model.NowString())
	var adopted int64
	for _, candidate := range candidates {
		result, err := tx.ExecContext(ctx, `
		INSERT INTO minimal_linked_threads(
			chat_key, chat_id, topic_id, project_id, source_thread_id, linked_thread_id,
			source_anchor_turn_id, source_title_payload, desired_title_payload, title_state,
			state, active_turn_id, worker_generation, last_blocked_at, last_blocked_code,
			failure_kind, created_at, updated_at, released_at
		)
		VALUES (?, ?, ?, ?, ?, ?, ?, NULL, NULL, ?, ?, NULL, 0, NULL, NULL, NULL, ?, ?, NULL)
		ON CONFLICT(chat_key, source_thread_id) DO NOTHING`,
			candidate.chatKey, candidate.chatID, candidate.topicID, candidate.projectID, candidate.sourceThreadID, candidate.forkThreadID,
			candidate.sourceTurnID, model.MinimalLinkedTitlePending, model.MinimalLinkedReady, now, now)
		if err != nil {
			return 0, err
		}
		changed, err := result.RowsAffected()
		if err != nil {
			return 0, err
		}
		adopted += changed
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return adopted, nil
}

func (s *Store) GetMinimalLinkedThread(ctx context.Context, chatID, topicID int64, sourceID string) (*model.MinimalLinkedThread, error) {
	sourceID = strings.TrimSpace(sourceID)
	if chatID == 0 || sourceID == "" {
		return nil, nil
	}
	if err := s.EnsureMinimalSchema(ctx); err != nil {
		return nil, err
	}
	row := s.db.QueryRowContext(ctx, `
	SELECT chat_key, chat_id, topic_id, project_id, source_thread_id, coalesce(linked_thread_id, ''),
	       source_anchor_turn_id, source_title_payload, desired_title_payload, title_state, state,
	       coalesce(active_turn_id, ''), worker_generation, coalesce(last_blocked_at, ''),
	       coalesce(last_blocked_code, ''), coalesce(failure_kind, ''), created_at, updated_at,
	       coalesce(released_at, '')
	FROM minimal_linked_threads
	WHERE chat_key = ? AND source_thread_id = ?`, model.ChatKey(chatID, topicID), sourceID)
	return s.scanMinimalLinkedThread(ctx, row)
}

func (s *Store) GetMinimalLinkedThreadByLinkedID(ctx context.Context, linkedID string) (*model.MinimalLinkedThread, error) {
	linkedID = strings.TrimSpace(linkedID)
	if linkedID == "" {
		return nil, nil
	}
	if err := s.EnsureMinimalSchema(ctx); err != nil {
		return nil, err
	}
	row := s.db.QueryRowContext(ctx, `
	SELECT chat_key, chat_id, topic_id, project_id, source_thread_id, coalesce(linked_thread_id, ''),
	       source_anchor_turn_id, source_title_payload, desired_title_payload, title_state, state,
	       coalesce(active_turn_id, ''), worker_generation, coalesce(last_blocked_at, ''),
	       coalesce(last_blocked_code, ''), coalesce(failure_kind, ''), created_at, updated_at,
	       coalesce(released_at, '')
	FROM minimal_linked_threads
	WHERE linked_thread_id = ?`, linkedID)
	return s.scanMinimalLinkedThread(ctx, row)
}

func (s *Store) HydrateMinimalLinkedTitles(ctx context.Context, linkedID, sourceTitle, desiredTitle string) (bool, error) {
	linkedID = strings.TrimSpace(linkedID)
	sourceTitle = strings.TrimSpace(sourceTitle)
	desiredTitle = strings.TrimSpace(desiredTitle)
	if linkedID == "" || sourceTitle == "" || desiredTitle == "" {
		return false, nil
	}
	protectedSourceTitle, err := s.protectMinimalLinkedTitle(ctx, sourceTitle)
	if err != nil {
		return false, err
	}
	protectedDesiredTitle, err := s.protectMinimalLinkedTitle(ctx, desiredTitle)
	if err != nil {
		return false, err
	}
	if err := s.EnsureMinimalSchema(ctx); err != nil {
		return false, err
	}
	result, err := s.db.ExecContext(ctx, `
	UPDATE minimal_linked_threads
	SET source_title_payload = CASE
			WHEN coalesce(source_title_payload, '') = '' THEN ?
			ELSE source_title_payload
		END,
	    desired_title_payload = CASE
			WHEN coalesce(desired_title_payload, '') = '' THEN ?
			ELSE desired_title_payload
		END,
	    updated_at = ?
	WHERE linked_thread_id = ?
	  AND (coalesce(source_title_payload, '') = '' OR coalesce(desired_title_payload, '') = '')`,
		protectedSourceTitle, protectedDesiredTitle, string(model.NowString()), linkedID)
	if err != nil {
		return false, err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	return changed == 1, nil
}

func (s *Store) ActivateMinimalLinkedThread(ctx context.Context, link model.MinimalLinkedThread, provenance model.MinimalContinuation, child model.Thread) error {
	child.ID = strings.TrimSpace(child.ID)
	if child.ID == "" {
		return errors.New("linked child thread id is required")
	}
	normalized, err := normalizeMinimalLinkedActivation(link, child.ID)
	if err != nil {
		return err
	}
	provenance = fillMinimalLinkedProvenance(normalized, provenance)
	seed, err := normalizeMinimalContinuation(provenance)
	if err != nil {
		return err
	}
	if seed.Key.ChatID != normalized.ChatID || seed.Key.TopicID != normalized.TopicID || seed.Key.SourceThreadID != normalized.SourceThreadID || seed.Key.SourceTurnID != normalized.SourceAnchorTurnID || seed.ProjectID != normalized.ProjectID {
		return errors.New("minimal linked provenance does not match link")
	}
	protectedSourceTitle, err := s.protectMinimalLinkedTitle(ctx, normalized.SourceTitle)
	if err != nil {
		return err
	}
	protectedDesiredTitle, err := s.protectMinimalLinkedTitle(ctx, normalized.DesiredTitle)
	if err != nil {
		return err
	}
	generation, err := minimalLinkedGenerationInt64(normalized.WorkerGeneration)
	if err != nil {
		return err
	}
	if err := s.EnsureMinimalSchema(ctx); err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer rollback(tx)
	if err := upsertThreadTx(ctx, tx, child); err != nil {
		return err
	}
	now := string(model.NowString())
	result, err := tx.ExecContext(ctx, `
	INSERT INTO minimal_thread_continuations(chat_key, chat_id, topic_id, project_id, source_thread_id, source_turn_id, fork_thread_id, status, failure_kind, created_at, updated_at)
	VALUES (?, ?, ?, ?, ?, ?, ?, ?, NULL, ?, ?)
	ON CONFLICT(chat_key, source_thread_id, source_turn_id) DO UPDATE SET
		fork_thread_id = excluded.fork_thread_id,
		status = excluded.status,
		failure_kind = NULL,
		updated_at = excluded.updated_at
	WHERE minimal_thread_continuations.status IN (?, ?)
	  AND coalesce(minimal_thread_continuations.fork_thread_id, '') IN ('', excluded.fork_thread_id)`,
		normalized.ChatKey, normalized.ChatID, normalized.TopicID, normalized.ProjectID, normalized.SourceThreadID,
		normalized.SourceAnchorTurnID, normalized.LinkedThreadID, model.MinimalContinuationActive, now, now,
		model.MinimalContinuationCreating, model.MinimalContinuationActive)
	if err != nil {
		return err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if changed != 1 {
		return fmt.Errorf("activate minimal linked provenance changed %d rows, want 1", changed)
	}
	if _, err := tx.ExecContext(ctx, `
	UPDATE pending_commands
	SET thread_id = ?
	WHERE chat_id = ? AND topic_id = ? AND source_thread_id = ? AND source_turn_id = ? AND status IN (?, ?)`,
		normalized.LinkedThreadID, normalized.ChatID, normalized.TopicID, normalized.SourceThreadID, normalized.SourceAnchorTurnID,
		model.PendingCommandStatusPending, model.PendingCommandStatusClaimed); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `
	INSERT INTO thread_bindings(chat_key, chat_id, topic_id, thread_id, mode, created_at, updated_at)
	VALUES (?, ?, ?, ?, ?, ?, ?)
	ON CONFLICT(chat_key) DO UPDATE SET thread_id = excluded.thread_id, mode = excluded.mode, updated_at = excluded.updated_at`,
		normalized.ChatKey, normalized.ChatID, normalized.TopicID, normalized.LinkedThreadID, model.BindingModeBound, now, now); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `
	INSERT INTO minimal_linked_threads(
		chat_key, chat_id, topic_id, project_id, source_thread_id, linked_thread_id,
		source_anchor_turn_id, source_title_payload, desired_title_payload, title_state,
		state, active_turn_id, worker_generation, last_blocked_at, last_blocked_code,
		failure_kind, created_at, updated_at, released_at
	)
	VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, NULL, ?, NULL, NULL, NULL, ?, ?, NULL)`,
		normalized.ChatKey, normalized.ChatID, normalized.TopicID, normalized.ProjectID, normalized.SourceThreadID, normalized.LinkedThreadID,
		normalized.SourceAnchorTurnID, protectedSourceTitle, protectedDesiredTitle, normalized.TitleState,
		normalized.State, generation, now, now)
	if err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) ClaimMinimalLinkedWorker(ctx context.Context, linkedID string, generation uint64) (bool, error) {
	linkedID = strings.TrimSpace(linkedID)
	if linkedID == "" {
		return false, nil
	}
	if generation == 0 {
		return false, errors.New("minimal linked worker generation is required")
	}
	generationValue, err := minimalLinkedGenerationInt64(generation)
	if err != nil {
		return false, err
	}
	if err := s.EnsureMinimalSchema(ctx); err != nil {
		return false, err
	}
	result, err := s.db.ExecContext(ctx, `
	UPDATE minimal_linked_threads
	SET state = ?, active_turn_id = NULL, worker_generation = ?, last_blocked_at = NULL,
	    last_blocked_code = NULL, failure_kind = NULL, updated_at = ?
	WHERE linked_thread_id = ? AND state = ? AND worker_generation < ?`,
		model.MinimalLinkedTelegramRunning, generationValue, string(model.NowString()), linkedID, model.MinimalLinkedReady, generationValue)
	if err != nil {
		return false, err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	return changed == 1, nil
}

func (s *Store) MarkMinimalLinkedTurnStarted(ctx context.Context, linkedID string, generation uint64, turnID string) (bool, error) {
	linkedID = strings.TrimSpace(linkedID)
	turnID = strings.TrimSpace(turnID)
	if linkedID == "" || turnID == "" {
		return false, nil
	}
	generationValue, err := minimalLinkedGenerationInt64(generation)
	if err != nil {
		return false, err
	}
	if err := s.EnsureMinimalSchema(ctx); err != nil {
		return false, err
	}
	result, err := s.db.ExecContext(ctx, `
	UPDATE minimal_linked_threads
	SET active_turn_id = ?, updated_at = ?
	WHERE linked_thread_id = ? AND worker_generation = ? AND state = ? AND coalesce(active_turn_id, '') = ''`,
		turnID, string(model.NowString()), linkedID, generationValue, model.MinimalLinkedTelegramRunning)
	if err != nil {
		return false, err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	return changed == 1, nil
}

func (s *Store) MarkMinimalLinkedTitleSet(ctx context.Context, linkedID string, generation uint64) (bool, error) {
	linkedID = strings.TrimSpace(linkedID)
	if linkedID == "" {
		return false, nil
	}
	generationValue, err := minimalLinkedGenerationInt64(generation)
	if err != nil {
		return false, err
	}
	if err := s.EnsureMinimalSchema(ctx); err != nil {
		return false, err
	}
	result, err := s.db.ExecContext(ctx, `
	UPDATE minimal_linked_threads
	SET title_state = ?, updated_at = ?
	WHERE linked_thread_id = ? AND worker_generation = ? AND state = ?`,
		model.MinimalLinkedTitleSet, string(model.NowString()), linkedID, generationValue, model.MinimalLinkedTelegramRunning)
	if err != nil {
		return false, err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	return changed == 1, nil
}

func (s *Store) BeginMinimalLinkedRelease(ctx context.Context, release model.MinimalLinkedRelease) (bool, error) {
	linkedID := strings.TrimSpace(release.LinkedThreadID)
	turnID := strings.TrimSpace(release.TurnID)
	generation, err := minimalLinkedReleaseGeneration(release)
	if err != nil {
		return false, err
	}
	if linkedID == "" || turnID == "" {
		return false, nil
	}
	generationValue, err := minimalLinkedGenerationInt64(generation)
	if err != nil {
		return false, err
	}
	if err := s.EnsureMinimalSchema(ctx); err != nil {
		return false, err
	}
	result, err := s.db.ExecContext(ctx, `
	UPDATE minimal_linked_threads
	SET state = ?, updated_at = ?
	WHERE linked_thread_id = ? AND worker_generation = ? AND state = ? AND active_turn_id = ?`,
		model.MinimalLinkedReleasePending, string(model.NowString()), linkedID, generationValue, model.MinimalLinkedTelegramRunning, turnID)
	if err != nil {
		return false, err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	return changed == 1, nil
}

func (s *Store) FinishMinimalLinkedRelease(ctx context.Context, linkedID string, generation uint64, releasedAt time.Time) (bool, error) {
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
	SET state = ?, active_turn_id = NULL, released_at = ?, updated_at = ?
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

func (s *Store) RecordMinimalLinkedBlocked(ctx context.Context, linkedID, code string, at time.Time) error {
	linkedID = strings.TrimSpace(linkedID)
	if linkedID == "" {
		return errors.New("linked thread id is required")
	}
	code = sanitizeMinimalLinkedCode(code)
	if code == "" {
		return errors.New("minimal linked block code is required")
	}
	if at.IsZero() {
		at = time.Now().UTC()
	}
	if err := s.EnsureMinimalSchema(ctx); err != nil {
		return err
	}
	now := string(model.NowString())
	result, err := s.db.ExecContext(ctx, `
	UPDATE minimal_linked_threads
	SET last_blocked_at = ?, last_blocked_code = ?, updated_at = ?
	WHERE linked_thread_id = ? AND state = ?`,
		at.UTC().Format(time.RFC3339Nano), code, now, linkedID, model.MinimalLinkedReady)
	if err != nil {
		return err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if changed != 1 {
		return fmt.Errorf("record minimal linked blocked changed %d rows, want 1", changed)
	}
	return nil
}

func (s *Store) FailMinimalLinkedThread(ctx context.Context, linkedID string, generation uint64, kind string) error {
	linkedID = strings.TrimSpace(linkedID)
	kind = sanitizeMinimalLinkedCode(kind)
	if linkedID == "" || kind == "" {
		return errors.New("minimal linked failure requires linked id and kind")
	}
	generationValue, err := minimalLinkedGenerationInt64(generation)
	if err != nil {
		return err
	}
	if err := s.EnsureMinimalSchema(ctx); err != nil {
		return err
	}
	result, err := s.db.ExecContext(ctx, `
	UPDATE minimal_linked_threads
	SET state = ?, active_turn_id = NULL, failure_kind = ?, updated_at = ?
	WHERE linked_thread_id = ? AND worker_generation = ? AND state IN (?, ?)`,
		model.MinimalLinkedFailed, kind, string(model.NowString()), linkedID, generationValue,
		model.MinimalLinkedTelegramRunning, model.MinimalLinkedReleasePending)
	if err != nil {
		return err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if changed != 1 {
		return fmt.Errorf("fail minimal linked thread changed %d rows, want 1", changed)
	}
	return nil
}

func (s *Store) RecoverMinimalLinkedWorkers(ctx context.Context) (int64, error) {
	if err := s.EnsureMinimalSchema(ctx); err != nil {
		return 0, err
	}
	result, err := s.db.ExecContext(ctx, `
	UPDATE minimal_linked_threads
	SET state = ?, updated_at = ?
	WHERE state = ?`,
		model.MinimalLinkedReleasePending, string(model.NowString()), model.MinimalLinkedTelegramRunning)
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}

func (s *Store) CreateMinimalPickerRoutes(ctx context.Context, routes []model.MinimalPickerRoute) error {
	if len(routes) == 0 {
		return nil
	}
	if err := s.EnsureMinimalSchema(ctx); err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer rollback(tx)
	createdAt := string(model.NowString())
	for _, route := range routes {
		route.Token = strings.TrimSpace(route.Token)
		route.Action = strings.TrimSpace(route.Action)
		route.ProjectID = strings.TrimSpace(route.ProjectID)
		route.ThreadID = strings.TrimSpace(route.ThreadID)
		route.Status = strings.TrimSpace(route.Status)
		if route.Token == "" || route.Action == "" || route.ProjectID == "" || route.Status == "" || strings.TrimSpace(string(route.ExpiresAt)) == "" {
			return errors.New("minimal picker route token, action, project, status, and expiry are required")
		}
		if route.ChatID == 0 {
			return errors.New("minimal picker route chat id is required")
		}
		if route.Page < 0 {
			return errors.New("minimal picker route page cannot be negative")
		}
		if _, err := tx.ExecContext(ctx, `
		INSERT INTO minimal_picker_routes(route_token, action, project_id, thread_id, page, chat_id, topic_id, status, expires_at, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			route.Token, route.Action, route.ProjectID, nullable(route.ThreadID), route.Page, route.ChatID, route.TopicID, route.Status, route.ExpiresAt, createdAt); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) ConsumeMinimalPickerRoute(ctx context.Context, token string, chatID, topicID int64, now time.Time) (*model.MinimalPickerRoute, error) {
	token = strings.TrimSpace(token)
	if token == "" || chatID == 0 {
		return nil, nil
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	if err := s.EnsureMinimalSchema(ctx); err != nil {
		return nil, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer rollback(tx)
	nowString := now.UTC().Format(time.RFC3339Nano)
	if _, err := tx.ExecContext(ctx, `UPDATE minimal_picker_routes SET status = 'expired' WHERE route_token = ? AND status = 'active' AND expires_at <= ?`, token, nowString); err != nil {
		return nil, err
	}
	consumeSQL := `
	UPDATE minimal_picker_routes SET status = 'consumed'
	WHERE route_token = ? AND status = 'active' AND chat_id = ? AND topic_id = ? AND expires_at > ?`
	args := []any{token, chatID, topicID, nowString}
	chatKey := model.ChatKey(chatID, topicID)
	consumeSQL += ` AND (
			action NOT IN ('minimal_existing_open', 'minimal_existing_page', 'minimal_existing_back', 'minimal_existing_select')
			OR NOT EXISTS (
				SELECT 1 FROM selected_projects sp
				WHERE sp.chat_key = ?
			)
			OR EXISTS (
				SELECT 1 FROM selected_projects sp
				WHERE sp.chat_key = ? AND sp.project_id = minimal_picker_routes.project_id
			)
		)`
	args = append(args, chatKey, chatKey)
	result, err := tx.ExecContext(ctx, consumeSQL, args...)
	if err != nil {
		return nil, err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return nil, err
	}
	if changed == 0 {
		if _, err := tx.ExecContext(ctx, `
			UPDATE minimal_picker_routes SET status = 'consumed'
			WHERE route_token = ? AND status = 'active' AND chat_id = ? AND topic_id = ? AND expires_at > ?
			  AND action IN ('minimal_existing_open', 'minimal_existing_page', 'minimal_existing_back', 'minimal_existing_select')
			  AND EXISTS (
				SELECT 1 FROM selected_projects sp
				WHERE sp.chat_key = ?
			  )
			  AND NOT EXISTS (
				SELECT 1 FROM selected_projects sp
				WHERE sp.chat_key = ? AND sp.project_id = minimal_picker_routes.project_id
			  )`, token, chatID, topicID, nowString, chatKey, chatKey); err != nil {
			return nil, err
		}
		if err := tx.Commit(); err != nil {
			return nil, err
		}
		return nil, nil
	}
	var route model.MinimalPickerRoute
	err = tx.QueryRowContext(ctx, `
	SELECT route_token, action, project_id, coalesce(thread_id, ''), page, chat_id, topic_id, status, expires_at
	FROM minimal_picker_routes WHERE route_token = ?`, token).Scan(
		&route.Token, &route.Action, &route.ProjectID, &route.ThreadID, &route.Page, &route.ChatID, &route.TopicID, &route.Status, &route.ExpiresAt,
	)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return &route, nil
}

func (s *Store) ObserveMinimalThread(ctx context.Context, thread model.Thread, projectID string, now time.Time) error {
	thread.ID = strings.TrimSpace(thread.ID)
	projectID = strings.TrimSpace(projectID)
	if thread.ID == "" || projectID == "" {
		return errors.New("minimal observation thread id and project id are required")
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	if err := s.EnsureMinimalSchema(ctx); err != nil {
		return err
	}
	turnID := strings.TrimSpace(thread.ActiveTurnID)
	status := strings.TrimSpace(thread.Status)
	readRequired := boolToInt(minimalObservationReadRequired(status, turnID))
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer rollback(tx)
	var previousTurnID, previousStatus string
	var previousUpdatedAt int64
	var previousReadRequired, retired int
	err = tx.QueryRowContext(ctx, `
	SELECT coalesce(last_turn_id, ''), coalesce(last_turn_status, ''), last_updated_at, read_required, retired
	FROM minimal_thread_observations WHERE thread_id = ?`, thread.ID).Scan(&previousTurnID, &previousStatus, &previousUpdatedAt, &previousReadRequired, &retired)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if retired != 0 {
		return tx.Commit()
	}
	changed := errors.Is(err, sql.ErrNoRows) ||
		thread.UpdatedAt > previousUpdatedAt ||
		(thread.UpdatedAt == previousUpdatedAt && (turnID != previousTurnID || (!minimalTerminalStatus(previousStatus) && !strings.EqualFold(status, previousStatus)))) ||
		(readRequired != 0 && previousReadRequired == 0)
	if !changed {
		return tx.Commit()
	}
	if turnID == "" && previousTurnID != "" && !minimalTerminalStatus(previousStatus) && minimalObservationReadRequired(status, turnID) {
		_, err = tx.ExecContext(ctx, `
		UPDATE minimal_thread_observations
		SET project_id = ?, last_updated_at = ?, read_required = 1, updated_at = ?
		WHERE thread_id = ?`,
			projectID, thread.UpdatedAt, now.UTC().Format(time.RFC3339Nano), thread.ID)
		if err != nil {
			return err
		}
		return tx.Commit()
	}
	if minimalTerminalStatus(status) && previousTurnID != "" && !minimalTerminalStatus(previousStatus) && (turnID == "" || previousTurnID == turnID) {
		_, err = tx.ExecContext(ctx, `
		UPDATE minimal_thread_observations
		SET project_id = ?, last_updated_at = ?, read_required = 1, updated_at = ?
		WHERE thread_id = ?`,
			projectID, thread.UpdatedAt, now.UTC().Format(time.RFC3339Nano), thread.ID)
		if err != nil {
			return err
		}
		return tx.Commit()
	}
	_, err = tx.ExecContext(ctx, `
	INSERT INTO minimal_thread_observations(thread_id, project_id, last_updated_at, last_turn_id, last_turn_status, baseline_ready, read_required, discovery_seq, updated_at)
	VALUES (?, ?, ?, ?, ?, 1, ?, (SELECT coalesce(max(discovery_seq), 0) + 1 FROM minimal_thread_observations), ?)
	ON CONFLICT(thread_id) DO UPDATE SET
		project_id = excluded.project_id,
		last_updated_at = excluded.last_updated_at,
		last_turn_id = excluded.last_turn_id,
		last_turn_status = excluded.last_turn_status,
		baseline_ready = 1,
		read_required = excluded.read_required,
		updated_at = excluded.updated_at`,
		thread.ID, projectID, thread.UpdatedAt, nullable(turnID), nullable(status), readRequired, now.UTC().Format(time.RFC3339Nano))
	if err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) ListMinimalObservedThreadsDue(ctx context.Context, limit int) ([]model.Thread, error) {
	if limit <= 0 {
		limit = 50
	}
	if err := s.EnsureMinimalSchema(ctx); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `
	SELECT t.thread_id, t.title, t.cwd, t.project_name, t.directory_name, t.updated_at, t.status, t.last_preview,
	       t.active_turn_id, t.preferred_model, t.permissions_mode, t.archived, t.raw_json
	FROM minimal_thread_observations o
	JOIN threads t ON t.thread_id = o.thread_id
	WHERE o.read_required = 1
	  AND o.retired = 0
	ORDER BY o.discovery_seq ASC, t.thread_id ASC
	LIMIT ?`, limit)
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

func (s *Store) RetireMinimalObservation(ctx context.Context, threadID string, now time.Time) error {
	threadID = strings.TrimSpace(threadID)
	if threadID == "" {
		return nil
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	if err := s.EnsureMinimalSchema(ctx); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, `
	UPDATE minimal_thread_observations
	SET read_required = 0, retired = 1, updated_at = ?
	WHERE thread_id = ?`,
		now.UTC().Format(time.RFC3339Nano), threadID)
	return err
}

func (s *Store) MinimalObservationReadRequired(ctx context.Context, threadID string) (bool, bool, error) {
	threadID = strings.TrimSpace(threadID)
	if threadID == "" {
		return false, false, nil
	}
	if err := s.EnsureMinimalSchema(ctx); err != nil {
		return false, false, err
	}
	var readRequired, retired int
	err := s.db.QueryRowContext(ctx, `
	SELECT read_required, retired FROM minimal_thread_observations WHERE thread_id = ?`, threadID).Scan(&readRequired, &retired)
	if errors.Is(err, sql.ErrNoRows) {
		return false, false, nil
	}
	if err != nil {
		return false, false, err
	}
	return true, retired == 0 && readRequired != 0, nil
}

func minimalObservationReadRequired(status, turnID string) bool {
	if strings.TrimSpace(turnID) != "" && !minimalTerminalStatus(status) {
		return true
	}
	normalized := strings.ToLower(strings.TrimSpace(status))
	if normalized == "notloaded" || normalized == "not_loaded" {
		return true
	}
	return normalized == "active" ||
		strings.HasPrefix(normalized, "active[") ||
		strings.Contains(normalized, "waitingon") ||
		strings.Contains(normalized, "inprogress") ||
		strings.Contains(normalized, "running")
}

func (s *Store) ClaimMinimalTerminalTransition(ctx context.Context, threadID, turnID, outcome string, now time.Time) (*model.TerminalEvent, error) {
	return s.claimMinimalTerminalTransition(ctx, threadID, turnID, outcome, false, now)
}

func (s *Store) ClaimMinimalTerminalTransitionAfterBaseline(ctx context.Context, threadID, turnID, outcome string, threadUpdatedAt, baselineSinceUnix int64, now time.Time) (*model.TerminalEvent, error) {
	allowRecentNotLoaded := baselineSinceUnix > 0 && threadUpdatedAt >= baselineSinceUnix
	return s.claimMinimalTerminalTransition(ctx, threadID, turnID, outcome, allowRecentNotLoaded, now)
}

func (s *Store) claimMinimalTerminalTransition(ctx context.Context, threadID, turnID, outcome string, allowRecentNotLoaded bool, now time.Time) (*model.TerminalEvent, error) {
	threadID = strings.TrimSpace(threadID)
	turnID = strings.TrimSpace(turnID)
	outcome = strings.TrimSpace(strings.ToLower(outcome))
	if threadID == "" || turnID == "" || !minimalTerminalStatus(outcome) {
		return nil, nil
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	if err := s.EnsureMinimalSchema(ctx); err != nil {
		return nil, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer rollback(tx)
	result, err := tx.ExecContext(ctx, `
	UPDATE minimal_thread_observations
	SET last_turn_id = ?, last_turn_status = ?, read_required = 1, updated_at = ?
	WHERE thread_id = ? AND baseline_ready = 1 AND retired = 0
	  AND (
		(last_turn_id = ?
			AND coalesce(last_turn_status, '') != ''
			AND lower(last_turn_status) NOT IN ('completed', 'failed', 'interrupted', 'cancelled', 'canceled'))
		OR
		(coalesce(last_turn_id, '') != ?
			AND lower(coalesce(last_turn_status, '')) IN ('completed', 'failed', 'interrupted', 'cancelled', 'canceled'))
		OR
		(? = 1
			AND coalesce(last_turn_id, '') = ''
			AND lower(coalesce(last_turn_status, '')) IN ('notloaded', 'not_loaded'))
	  )`,
		turnID, outcome, now.UTC().Format(time.RFC3339Nano), threadID, turnID, turnID, boolToInt(allowRecentNotLoaded))
	if err != nil {
		return nil, err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return nil, err
	}
	if changed == 0 {
		if err := tx.Commit(); err != nil {
			return nil, err
		}
		return nil, nil
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return &model.TerminalEvent{
		TerminalKey:    fmt.Sprintf("%s:%s:%s", threadID, turnID, outcome),
		ThreadID:       threadID,
		TurnID:         turnID,
		Status:         outcome,
		DeliveryStatus: model.DeliveryStatusPending,
		UpdatedAt:      model.TimeString(now.UTC().Format(time.RFC3339Nano)),
	}, nil
}

func (s *Store) ReopenMinimalTerminalTransition(ctx context.Context, threadID, turnID string, now time.Time) error {
	threadID = strings.TrimSpace(threadID)
	turnID = strings.TrimSpace(turnID)
	if threadID == "" || turnID == "" {
		return nil
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	if err := s.EnsureMinimalSchema(ctx); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, `
	UPDATE minimal_thread_observations
	SET last_turn_status = 'inProgress', read_required = 1, updated_at = ?
	WHERE thread_id = ? AND last_turn_id = ?
	  AND retired = 0
	  AND lower(coalesce(last_turn_status, '')) IN ('completed', 'failed', 'interrupted', 'cancelled', 'canceled')`,
		now.UTC().Format(time.RFC3339Nano), threadID, turnID)
	return err
}

func minimalTerminalStatus(status string) bool {
	switch strings.TrimSpace(strings.ToLower(status)) {
	case "completed", "failed", "interrupted", "cancelled", "canceled":
		return true
	default:
		return false
	}
}

func (s *Store) CreateVoiceConfirmation(ctx context.Context, confirmation model.VoiceConfirmation, executeToken, cancelToken string) (int64, error) {
	confirmation.ProjectID = strings.TrimSpace(confirmation.ProjectID)
	confirmation.TargetKind = strings.TrimSpace(confirmation.TargetKind)
	confirmation.ThreadID = strings.TrimSpace(confirmation.ThreadID)
	confirmation.SourceTurnID = strings.TrimSpace(confirmation.SourceTurnID)
	confirmation.Transcript = strings.TrimSpace(confirmation.Transcript)
	confirmation.SessionIdentity = strings.TrimSpace(confirmation.SessionIdentity)
	if confirmation.ProjectID == "" || confirmation.Transcript == "" || confirmation.SessionIdentity == "" || strings.TrimSpace(string(confirmation.ExpiresAt)) == "" {
		return 0, errors.New("voice project, transcript, session, and expiry are required")
	}
	if confirmation.TargetKind != model.VoiceTargetNew && confirmation.TargetKind != model.VoiceTargetThread {
		return 0, errors.New("invalid voice target kind")
	}
	if confirmation.TargetKind == model.VoiceTargetThread && confirmation.ThreadID == "" {
		return 0, errors.New("voice thread target is required")
	}
	if !validVoiceToken(executeToken) || !validVoiceToken(cancelToken) || executeToken == cancelToken {
		return 0, errors.New("voice callback tokens must be distinct 16-byte hex values")
	}
	if s.protector == nil {
		return 0, errors.New("securestore protector is required for voice confirmations")
	}
	if err := s.EnsureMinimalSchema(ctx); err != nil {
		return 0, err
	}
	protected, err := s.protector.Protect(ctx, []byte(confirmation.Transcript))
	if err != nil {
		return 0, errors.New("protect voice transcript failed")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer rollback(tx)
	createdAt := model.NowString()
	result, err := tx.ExecContext(ctx, `
	INSERT INTO voice_confirmations(project_id, target_kind, thread_id, source_turn_id, transcript_payload, session_identity, status, expires_at, chat_id, topic_id, telegram_message_id, created_at)
	VALUES (?, ?, ?, ?, ?, ?, ?, ?, 0, 0, 0, ?)`,
		confirmation.ProjectID, confirmation.TargetKind, nullable(confirmation.ThreadID), confirmation.SourceTurnID, protected, confirmation.SessionIdentity,
		model.VoiceStatusPending, confirmation.ExpiresAt, createdAt,
	)
	if err != nil {
		return 0, err
	}
	voiceID, err := result.LastInsertId()
	if err != nil {
		return 0, err
	}
	for _, route := range []struct{ token, action string }{{executeToken, model.VoiceActionExecute}, {cancelToken, model.VoiceActionCancel}} {
		if _, err := tx.ExecContext(ctx, `
		INSERT INTO voice_callback_routes(route_token, voice_id, action, status, chat_id, topic_id, telegram_message_id, created_at)
		VALUES (?, ?, ?, ?, 0, 0, 0, ?)`, route.token, voiceID, route.action, model.VoiceRouteStatusActive, createdAt); err != nil {
			return 0, err
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return voiceID, nil
}

func (s *Store) BindVoiceConfirmation(ctx context.Context, voiceID, chatID, topicID, messageID int64) error {
	if voiceID <= 0 || chatID == 0 || messageID == 0 {
		return errors.New("voice preview binding is incomplete")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer rollback(tx)
	result, err := tx.ExecContext(ctx, `
	UPDATE voice_confirmations SET chat_id = ?, topic_id = ?, telegram_message_id = ?
	WHERE id = ? AND status = ? AND telegram_message_id = 0`, chatID, topicID, messageID, voiceID, model.VoiceStatusPending)
	if err != nil {
		return err
	}
	changed, err := result.RowsAffected()
	if err != nil || changed != 1 {
		return fmt.Errorf("bind voice confirmation changed %d rows", changed)
	}
	result, err = tx.ExecContext(ctx, `
	UPDATE voice_callback_routes SET chat_id = ?, topic_id = ?, telegram_message_id = ?
	WHERE voice_id = ? AND status = ? AND telegram_message_id = 0`, chatID, topicID, messageID, voiceID, model.VoiceRouteStatusActive)
	if err != nil {
		return err
	}
	changed, err = result.RowsAffected()
	if err != nil || changed != 2 {
		return fmt.Errorf("bind voice callback routes changed %d rows", changed)
	}
	return tx.Commit()
}

func (s *Store) GetVoiceConfirmationRoute(ctx context.Context, token string) (*model.VoiceCallbackRoute, error) {
	var route model.VoiceCallbackRoute
	err := s.db.QueryRowContext(ctx, `
	SELECT route_token, voice_id, action, status, chat_id, topic_id, telegram_message_id, created_at
	FROM voice_callback_routes WHERE route_token = ?`, strings.TrimSpace(token)).Scan(
		&route.Token, &route.VoiceID, &route.Action, &route.Status, &route.ChatID, &route.TopicID, &route.MessageID, &route.CreatedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &route, nil
}

func (s *Store) ConsumeVoiceConfirmation(ctx context.Context, claim model.VoiceClaim) (*model.VoiceConfirmation, error) {
	claim.Token = strings.TrimSpace(claim.Token)
	claim.Action = strings.TrimSpace(claim.Action)
	claim.SessionIdentity = strings.TrimSpace(claim.SessionIdentity)
	if claim.Now.IsZero() {
		claim.Now = time.Now().UTC()
	}
	if s.protector == nil {
		return nil, errors.New("securestore protector is required for voice confirmations")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer rollback(tx)
	var confirmation model.VoiceConfirmation
	var routeStatus, routeAction, protected string
	err = tx.QueryRowContext(ctx, `
	SELECT v.id, v.project_id, v.target_kind, coalesce(v.thread_id,''), coalesce(v.source_turn_id, ''), coalesce(v.transcript_payload,''),
	       v.session_identity, v.status, v.expires_at, v.chat_id, v.topic_id, v.telegram_message_id, v.created_at,
	       r.action, r.status
	FROM voice_callback_routes r JOIN voice_confirmations v ON v.id = r.voice_id
	WHERE r.route_token = ?`, claim.Token).Scan(
		&confirmation.ID, &confirmation.ProjectID, &confirmation.TargetKind, &confirmation.ThreadID, &confirmation.SourceTurnID, &protected,
		&confirmation.SessionIdentity, &confirmation.Status, &confirmation.ExpiresAt, &confirmation.ChatID, &confirmation.TopicID,
		&confirmation.MessageID, &confirmation.CreatedAt, &routeAction, &routeStatus,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if routeStatus != model.VoiceRouteStatusActive || routeAction != claim.Action || confirmation.Status != model.VoiceStatusPending ||
		confirmation.SessionIdentity != claim.SessionIdentity || confirmation.ChatID == 0 || confirmation.MessageID == 0 ||
		confirmation.ChatID != claim.ChatID || confirmation.TopicID != claim.TopicID || confirmation.MessageID != claim.MessageID {
		return nil, nil
	}
	var siblings int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM voice_callback_routes WHERE voice_id = ? AND status = ?`, confirmation.ID, model.VoiceRouteStatusActive).Scan(&siblings); err != nil {
		return nil, err
	}
	expiresAt, parseErr := time.Parse(time.RFC3339Nano, string(confirmation.ExpiresAt))
	if siblings != 2 || parseErr != nil || !claim.Now.Before(expiresAt) {
		if err := expireVoiceConfirmationTx(ctx, tx, confirmation.ID); err != nil {
			return nil, err
		}
		if err := tx.Commit(); err != nil {
			return nil, err
		}
		return nil, nil
	}
	terminalStatus := model.VoiceStatusCancelled
	if claim.Action == model.VoiceActionExecute {
		terminalStatus = model.VoiceStatusExecuted
	} else if claim.Action != model.VoiceActionCancel {
		return nil, nil
	}
	result, err := tx.ExecContext(ctx, `UPDATE voice_confirmations SET status = ?, transcript_payload = NULL WHERE id = ? AND status = ?`, terminalStatus, confirmation.ID, model.VoiceStatusPending)
	if err != nil {
		return nil, err
	}
	changed, err := result.RowsAffected()
	if err != nil || changed != 1 {
		return nil, nil
	}
	result, err = tx.ExecContext(ctx, `UPDATE voice_callback_routes SET status = ? WHERE voice_id = ? AND status = ?`, model.VoiceRouteStatusConsumed, confirmation.ID, model.VoiceRouteStatusActive)
	if err != nil {
		return nil, err
	}
	changed, err = result.RowsAffected()
	if err != nil || changed != 2 {
		return nil, errors.New("voice callback sibling consumption failed")
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	confirmation.Status = terminalStatus
	if claim.Action == model.VoiceActionExecute {
		plaintext, err := s.protector.Unprotect(ctx, protected)
		if err != nil {
			return nil, errors.New("unprotect voice transcript failed")
		}
		confirmation.Transcript = string(plaintext)
	}
	return &confirmation, nil
}

func (s *Store) RecoverVoiceConfirmations(ctx context.Context) (int64, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer rollback(tx)
	result, err := tx.ExecContext(ctx, `UPDATE voice_confirmations SET status = ?, transcript_payload = NULL WHERE status = ?`, model.VoiceStatusExpired, model.VoiceStatusPending)
	if err != nil {
		return 0, err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return 0, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE voice_callback_routes SET status = ? WHERE status = ?`, model.VoiceRouteStatusConsumed, model.VoiceRouteStatusActive); err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return changed, nil
}

func (s *Store) AbandonVoiceConfirmation(ctx context.Context, voiceID int64) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer rollback(tx)
	if _, err := tx.ExecContext(ctx, `UPDATE voice_confirmations SET status = ?, transcript_payload = NULL WHERE id = ? AND status = ?`, model.VoiceStatusFailed, voiceID, model.VoiceStatusPending); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE voice_callback_routes SET status = ? WHERE voice_id = ? AND status = ?`, model.VoiceRouteStatusConsumed, voiceID, model.VoiceRouteStatusActive); err != nil {
		return err
	}
	return tx.Commit()
}

func expireVoiceConfirmationTx(ctx context.Context, tx *sql.Tx, voiceID int64) error {
	if _, err := tx.ExecContext(ctx, `UPDATE voice_confirmations SET status = ?, transcript_payload = NULL WHERE id = ? AND status = ?`, model.VoiceStatusExpired, voiceID, model.VoiceStatusPending); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `UPDATE voice_callback_routes SET status = ? WHERE voice_id = ? AND status = ?`, model.VoiceRouteStatusConsumed, voiceID, model.VoiceRouteStatusActive)
	return err
}

func validVoiceToken(token string) bool {
	if len(token) != 32 {
		return false
	}
	decoded, err := hex.DecodeString(token)
	return err == nil && len(decoded) == 16
}

func (s *Store) ensureMinimalColumn(ctx context.Context, tableName, columnName, definition string) error {
	present, err := s.hasColumn(ctx, tableName, columnName)
	if err != nil || present {
		return err
	}
	_, err = s.db.ExecContext(ctx, fmt.Sprintf("ALTER TABLE %s ADD COLUMN %s %s", tableName, columnName, definition))
	return err
}

func (s *Store) ensureMinimalObservationDiscoverySequence(ctx context.Context) error {
	var tableCount int
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name = 'minimal_thread_observations'`).Scan(&tableCount); err != nil {
		return err
	}
	if tableCount == 0 {
		return nil
	}
	present, err := s.hasColumn(ctx, "minimal_thread_observations", "discovery_seq")
	if err != nil {
		return err
	}
	if !present {
		if _, err := s.db.ExecContext(ctx, `ALTER TABLE minimal_thread_observations ADD COLUMN discovery_seq INTEGER NOT NULL DEFAULT 0`); err != nil {
			return err
		}
	}
	if _, err := s.db.ExecContext(ctx, `CREATE INDEX IF NOT EXISTS idx_minimal_thread_observations_due ON minimal_thread_observations(read_required, discovery_seq)`); err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer rollback(tx)
	var offset int64
	if err := tx.QueryRowContext(ctx, `SELECT coalesce(max(discovery_seq), 0) FROM minimal_thread_observations WHERE discovery_seq > 0`).Scan(&offset); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `
	WITH ordered AS (
		SELECT thread_id, ? + row_number() OVER (ORDER BY updated_at ASC, thread_id ASC) AS seq
		FROM minimal_thread_observations
		WHERE discovery_seq <= 0
	)
	UPDATE minimal_thread_observations
	SET discovery_seq = (SELECT seq FROM ordered WHERE ordered.thread_id = minimal_thread_observations.thread_id)
	WHERE discovery_seq <= 0`, offset); err != nil {
		return err
	}
	return tx.Commit()
}

type minimalContinuationQuerier interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func getMinimalContinuationTx(ctx context.Context, q minimalContinuationQuerier, key model.MinimalContinuationKey) (*model.MinimalContinuation, error) {
	var continuation model.MinimalContinuation
	var forkThreadID, failureKind sql.NullString
	err := q.QueryRowContext(ctx, `
	SELECT chat_id, topic_id, project_id, source_thread_id, source_turn_id, coalesce(fork_thread_id, ''), status, coalesce(failure_kind, ''), created_at, updated_at
	FROM minimal_thread_continuations
	WHERE chat_key = ? AND source_thread_id = ? AND source_turn_id = ?`,
		model.ChatKey(key.ChatID, key.TopicID), key.SourceThreadID, key.SourceTurnID).Scan(
		&continuation.Key.ChatID, &continuation.Key.TopicID, &continuation.ProjectID, &continuation.Key.SourceThreadID,
		&continuation.Key.SourceTurnID, &forkThreadID, &continuation.Status, &failureKind, &continuation.CreatedAt, &continuation.UpdatedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	continuation.ForkThreadID = forkThreadID.String
	continuation.FailureKind = failureKind.String
	return &continuation, nil
}

type minimalLinkedScanner interface {
	Scan(...any) error
}

func (s *Store) scanMinimalLinkedThread(ctx context.Context, scanner minimalLinkedScanner) (*model.MinimalLinkedThread, error) {
	var link model.MinimalLinkedThread
	var sourceTitlePayload, desiredTitlePayload sql.NullString
	var generation int64
	err := scanner.Scan(
		&link.ChatKey, &link.ChatID, &link.TopicID, &link.ProjectID, &link.SourceThreadID, &link.LinkedThreadID,
		&link.SourceAnchorTurnID, &sourceTitlePayload, &desiredTitlePayload, &link.TitleState, &link.State,
		&link.ActiveTurnID, &generation, &link.LastBlockedAt, &link.LastBlockedCode, &link.FailureKind,
		&link.CreatedAt, &link.UpdatedAt, &link.ReleasedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if generation < 0 {
		return nil, errors.New("minimal linked generation is negative")
	}
	link.WorkerGeneration = uint64(generation)
	sourceTitle, err := s.unprotectMinimalLinkedTitle(ctx, sourceTitlePayload)
	if err != nil {
		return nil, err
	}
	desiredTitle, err := s.unprotectMinimalLinkedTitle(ctx, desiredTitlePayload)
	if err != nil {
		return nil, err
	}
	link.SourceTitle = sourceTitle
	link.DesiredTitle = desiredTitle
	return &link, nil
}

func normalizeMinimalLinkedActivation(link model.MinimalLinkedThread, linkedThreadID string) (model.MinimalLinkedThread, error) {
	linkedThreadID = strings.TrimSpace(linkedThreadID)
	link.ChatKey = model.ChatKey(link.ChatID, link.TopicID)
	link.ProjectID = strings.TrimSpace(link.ProjectID)
	link.SourceThreadID = strings.TrimSpace(link.SourceThreadID)
	link.LinkedThreadID = strings.TrimSpace(link.LinkedThreadID)
	link.SourceAnchorTurnID = strings.TrimSpace(link.SourceAnchorTurnID)
	link.TitleState = strings.TrimSpace(link.TitleState)
	link.State = strings.TrimSpace(link.State)
	if link.ChatID == 0 || link.ProjectID == "" || link.SourceThreadID == "" || link.SourceAnchorTurnID == "" || linkedThreadID == "" {
		return model.MinimalLinkedThread{}, errors.New("minimal linked chat, project, source, anchor, and child thread are required")
	}
	if link.LinkedThreadID != "" && link.LinkedThreadID != linkedThreadID {
		return model.MinimalLinkedThread{}, errors.New("minimal linked child id does not match link")
	}
	link.LinkedThreadID = linkedThreadID
	if link.TitleState == "" {
		link.TitleState = model.MinimalLinkedTitlePending
	}
	if link.State == "" {
		link.State = model.MinimalLinkedTelegramRunning
	}
	if !minimalLinkedTitleStateValid(link.TitleState) {
		return model.MinimalLinkedThread{}, errors.New("invalid minimal linked title state")
	}
	if link.State != model.MinimalLinkedTelegramRunning {
		return model.MinimalLinkedThread{}, errors.New("minimal linked activation requires telegram_running state")
	}
	return link, nil
}

func fillMinimalLinkedProvenance(link model.MinimalLinkedThread, provenance model.MinimalContinuation) model.MinimalContinuation {
	if provenance.Key.ChatID == 0 {
		provenance.Key.ChatID = link.ChatID
	}
	if provenance.Key.TopicID == 0 {
		provenance.Key.TopicID = link.TopicID
	}
	if strings.TrimSpace(provenance.Key.SourceThreadID) == "" {
		provenance.Key.SourceThreadID = link.SourceThreadID
	}
	if strings.TrimSpace(provenance.Key.SourceTurnID) == "" {
		provenance.Key.SourceTurnID = link.SourceAnchorTurnID
	}
	if strings.TrimSpace(provenance.ProjectID) == "" {
		provenance.ProjectID = link.ProjectID
	}
	return provenance
}

func (s *Store) protectMinimalLinkedTitle(ctx context.Context, title string) (any, error) {
	title = strings.TrimSpace(title)
	if title == "" {
		return nil, nil
	}
	if s.protector == nil {
		return nil, errors.New("securestore protector is required for minimal linked titles")
	}
	protected, err := s.protector.Protect(ctx, []byte(title))
	if err != nil {
		return nil, fmt.Errorf("protect minimal linked title: %w", err)
	}
	return protected, nil
}

func (s *Store) unprotectMinimalLinkedTitle(ctx context.Context, payload sql.NullString) (string, error) {
	if !payload.Valid || strings.TrimSpace(payload.String) == "" {
		return "", nil
	}
	if s.protector == nil {
		return "", errors.New("securestore protector is required for minimal linked titles")
	}
	plaintext, err := s.protector.Unprotect(ctx, payload.String)
	if err != nil {
		return "", fmt.Errorf("unprotect minimal linked title: %w", err)
	}
	return string(plaintext), nil
}

func minimalLinkedTitleStateValid(state string) bool {
	switch state {
	case model.MinimalLinkedTitlePending, model.MinimalLinkedTitleSet:
		return true
	default:
		return false
	}
}

func minimalLinkedGenerationInt64(generation uint64) (int64, error) {
	if generation > uint64(1<<63-1) {
		return 0, errors.New("minimal linked generation exceeds sqlite integer range")
	}
	return int64(generation), nil
}

func minimalLinkedReleaseGeneration(release model.MinimalLinkedRelease) (uint64, error) {
	if release.WorkerGeneration == 0 {
		return 0, errors.New("minimal linked release generation is required")
	}
	return release.WorkerGeneration, nil
}

func sanitizeMinimalLinkedCode(value string) string {
	value = strings.TrimSpace(value)
	if len(value) > 128 {
		value = value[:128]
	}
	return value
}

func normalizeMinimalContinuation(continuation model.MinimalContinuation) (model.MinimalContinuation, error) {
	key, err := normalizeMinimalContinuationKey(continuation.Key)
	if err != nil {
		return model.MinimalContinuation{}, err
	}
	continuation.Key = key
	continuation.ProjectID = strings.TrimSpace(continuation.ProjectID)
	if continuation.ProjectID == "" {
		return model.MinimalContinuation{}, errors.New("minimal continuation project id is required")
	}
	return continuation, nil
}

func normalizeMinimalContinuationKey(key model.MinimalContinuationKey) (model.MinimalContinuationKey, error) {
	key.SourceThreadID = strings.TrimSpace(key.SourceThreadID)
	key.SourceTurnID = strings.TrimSpace(key.SourceTurnID)
	if key.ChatID == 0 || key.SourceThreadID == "" || key.SourceTurnID == "" {
		return model.MinimalContinuationKey{}, errors.New("minimal continuation chat id, source thread id, and source turn id are required")
	}
	return key, nil
}

func upsertThreadTx(ctx context.Context, tx *sql.Tx, thread model.Thread) error {
	raw := thread.Raw
	if len(raw) == 0 {
		raw = []byte("{}")
	}
	_, err := tx.ExecContext(ctx, `
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

func (s *Store) EnqueuePendingCommand(ctx context.Context, command model.PendingCommand) error {
	threadID := strings.TrimSpace(command.ThreadID)
	sourceThreadID := strings.TrimSpace(command.SourceThreadID)
	sourceTurnID := strings.TrimSpace(command.SourceTurnID)
	projectID := strings.TrimSpace(command.ProjectID)
	prompt := strings.TrimSpace(command.Prompt)
	if threadID == "" || projectID == "" || prompt == "" {
		return errors.New("thread id, project id, and prompt are required")
	}
	if s.protector == nil {
		return errors.New("securestore protector is required for pending commands")
	}
	if err := s.EnsureMinimalSchema(ctx); err != nil {
		return err
	}
	protected, err := s.protector.Protect(ctx, []byte(prompt))
	if err != nil {
		return fmt.Errorf("protect pending command: %w", err)
	}
	_, err = s.db.ExecContext(ctx, `
	INSERT INTO pending_commands(thread_id, source_thread_id, source_turn_id, project_id, chat_id, topic_id, prompt_payload, status, created_at)
	VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		threadID, sourceThreadID, sourceTurnID, projectID, command.ChatID, command.TopicID, protected, model.PendingCommandStatusPending, model.NowString())
	return err
}

func (s *Store) ClaimPendingCommand(ctx context.Context, threadID string) (*model.PendingCommand, error) {
	threadID = strings.TrimSpace(threadID)
	if threadID == "" {
		return nil, errors.New("thread id is required")
	}
	if s.protector == nil {
		return nil, errors.New("securestore protector is required for pending commands")
	}
	if err := s.EnsureMinimalSchema(ctx); err != nil {
		return nil, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer rollback(tx)
	var command model.PendingCommand
	var protected string
	err = tx.QueryRowContext(ctx, `
	SELECT id, thread_id, source_thread_id, source_turn_id, project_id, chat_id, topic_id, prompt_payload, status, created_at
	FROM pending_commands
	WHERE thread_id = ? AND status = ?
	  AND NOT EXISTS (
		SELECT 1 FROM pending_commands claimed
		WHERE claimed.thread_id = pending_commands.thread_id AND claimed.status = ?
	  )
	ORDER BY id
	LIMIT 1`, threadID, model.PendingCommandStatusPending, model.PendingCommandStatusClaimed).Scan(
		&command.ID, &command.ThreadID, &command.SourceThreadID, &command.SourceTurnID, &command.ProjectID,
		&command.ChatID, &command.TopicID, &protected, &command.Status, &command.CreatedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	result, err := tx.ExecContext(ctx, `
	UPDATE pending_commands SET status = ? WHERE id = ? AND status = ?`,
		model.PendingCommandStatusClaimed, command.ID, model.PendingCommandStatusPending,
	)
	if err != nil {
		return nil, err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return nil, err
	}
	if changed != 1 {
		return nil, nil
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	command.Status = model.PendingCommandStatusClaimed
	plaintext, err := s.protector.Unprotect(ctx, protected)
	if err != nil {
		return &command, fmt.Errorf("unprotect pending command: %w", err)
	}
	command.Prompt = string(plaintext)
	return &command, nil
}

func (s *Store) ClaimPendingCommandForSource(ctx context.Context, chatID, topicID int64, sourceThreadID, sourceTurnID string) (*model.PendingCommand, error) {
	sourceThreadID = strings.TrimSpace(sourceThreadID)
	sourceTurnID = strings.TrimSpace(sourceTurnID)
	if chatID == 0 || sourceThreadID == "" || sourceTurnID == "" {
		return nil, errors.New("pending command source is required")
	}
	if s.protector == nil {
		return nil, errors.New("securestore protector is required for pending commands")
	}
	if err := s.EnsureMinimalSchema(ctx); err != nil {
		return nil, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer rollback(tx)
	var command model.PendingCommand
	var protected string
	err = tx.QueryRowContext(ctx, `
	SELECT id, thread_id, source_thread_id, source_turn_id, project_id, chat_id, topic_id, prompt_payload, status, created_at
	FROM pending_commands
	WHERE chat_id = ? AND topic_id = ? AND source_thread_id = ? AND source_turn_id = ? AND status = ?
	  AND NOT EXISTS (
		SELECT 1 FROM pending_commands claimed
		WHERE claimed.chat_id = pending_commands.chat_id
		  AND claimed.topic_id = pending_commands.topic_id
		  AND claimed.source_thread_id = pending_commands.source_thread_id
		  AND claimed.source_turn_id = pending_commands.source_turn_id
		  AND claimed.status = ?
	  )
	ORDER BY id
	LIMIT 1`, chatID, topicID, sourceThreadID, sourceTurnID, model.PendingCommandStatusPending, model.PendingCommandStatusClaimed).Scan(
		&command.ID, &command.ThreadID, &command.SourceThreadID, &command.SourceTurnID, &command.ProjectID,
		&command.ChatID, &command.TopicID, &protected, &command.Status, &command.CreatedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	result, err := tx.ExecContext(ctx, `
	UPDATE pending_commands SET status = ? WHERE id = ? AND status = ?`,
		model.PendingCommandStatusClaimed, command.ID, model.PendingCommandStatusPending,
	)
	if err != nil {
		return nil, err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return nil, err
	}
	if changed != 1 {
		return nil, nil
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	command.Status = model.PendingCommandStatusClaimed
	plaintext, err := s.protector.Unprotect(ctx, protected)
	if err != nil {
		return &command, fmt.Errorf("unprotect pending command: %w", err)
	}
	command.Prompt = string(plaintext)
	return &command, nil
}

func (s *Store) ReleaseClaimedPendingCommand(ctx context.Context, id int64) error {
	if err := s.EnsureMinimalSchema(ctx); err != nil {
		return err
	}
	result, err := s.db.ExecContext(ctx, `
	UPDATE pending_commands SET status = ? WHERE id = ? AND status = ?`,
		model.PendingCommandStatusPending, id, model.PendingCommandStatusClaimed,
	)
	if err != nil {
		return err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if changed != 1 {
		return fmt.Errorf("release pending command changed %d rows, want 1", changed)
	}
	return nil
}

func (s *Store) HasPendingCommandBacklogForSource(ctx context.Context, chatID, topicID int64, sourceThreadID, sourceTurnID string) (bool, error) {
	sourceThreadID = strings.TrimSpace(sourceThreadID)
	sourceTurnID = strings.TrimSpace(sourceTurnID)
	if chatID == 0 || sourceThreadID == "" || sourceTurnID == "" {
		return false, nil
	}
	if err := s.EnsureMinimalSchema(ctx); err != nil {
		return false, err
	}
	var exists int
	err := s.db.QueryRowContext(ctx, `
	SELECT 1
	FROM pending_commands
	WHERE chat_id = ? AND topic_id = ? AND source_thread_id = ? AND source_turn_id = ?
	  AND status IN (?, ?)
	ORDER BY id
	LIMIT 1`,
		chatID, topicID, sourceThreadID, sourceTurnID,
		model.PendingCommandStatusPending, model.PendingCommandStatusClaimed,
	).Scan(&exists)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return exists == 1, nil
}

func (s *Store) RehomePendingCommandsForSource(ctx context.Context, chatID, topicID int64, sourceThreadID, sourceTurnID, targetThreadID string) error {
	sourceThreadID = strings.TrimSpace(sourceThreadID)
	sourceTurnID = strings.TrimSpace(sourceTurnID)
	targetThreadID = strings.TrimSpace(targetThreadID)
	if chatID == 0 || sourceThreadID == "" || sourceTurnID == "" || targetThreadID == "" {
		return errors.New("pending command rehome source and target are required")
	}
	if err := s.EnsureMinimalSchema(ctx); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, `
	UPDATE pending_commands
	SET thread_id = ?
	WHERE chat_id = ? AND topic_id = ? AND source_thread_id = ? AND source_turn_id = ?
	  AND status IN (?, ?)`,
		targetThreadID, chatID, topicID, sourceThreadID, sourceTurnID,
		model.PendingCommandStatusPending, model.PendingCommandStatusClaimed,
	)
	return err
}

func (s *Store) CompletePendingCommand(ctx context.Context, id int64) error {
	result, err := s.db.ExecContext(ctx, `
	UPDATE pending_commands SET status = ?, prompt_payload = NULL WHERE id = ? AND status = ?`,
		model.PendingCommandStatusCompleted, id, model.PendingCommandStatusClaimed,
	)
	return requireOnePendingCommandFinalized(result, err)
}

func (s *Store) FailPendingCommand(ctx context.Context, id int64) error {
	result, err := s.db.ExecContext(ctx, `
	UPDATE pending_commands SET status = ?, prompt_payload = NULL WHERE id = ? AND status = ?`,
		model.PendingCommandStatusFailed, id, model.PendingCommandStatusClaimed,
	)
	return requireOnePendingCommandFinalized(result, err)
}

func (s *Store) RecoverClaimedPendingCommands(ctx context.Context) (int64, error) {
	result, err := s.db.ExecContext(ctx, `
	UPDATE pending_commands SET status = ?, prompt_payload = NULL WHERE status = ?`,
		model.PendingCommandStatusFailed, model.PendingCommandStatusClaimed,
	)
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}

func requireOnePendingCommandFinalized(result sql.Result, err error) error {
	if err != nil {
		return err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if changed != 1 {
		return fmt.Errorf("pending command finalization changed %d rows, want 1", changed)
	}
	return nil
}

func (s *Store) SetSelectedProject(ctx context.Context, chatID, topicID int64, projectID string) error {
	projectID = strings.TrimSpace(projectID)
	if projectID == "" {
		return errors.New("project id is required")
	}
	if err := s.EnsureMinimalSchema(ctx); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, `
	INSERT INTO selected_projects(chat_key, project_id, updated_at)
	VALUES (?, ?, ?)
	ON CONFLICT(chat_key) DO UPDATE SET project_id = excluded.project_id, updated_at = excluded.updated_at`,
		model.ChatKey(chatID, topicID), projectID, model.NowString(),
	)
	return err
}

func (s *Store) GetSelectedProject(ctx context.Context, chatID, topicID int64) (string, error) {
	if err := s.EnsureMinimalSchema(ctx); err != nil {
		return "", err
	}
	var projectID string
	err := s.db.QueryRowContext(ctx, `SELECT project_id FROM selected_projects WHERE chat_key = ?`, model.ChatKey(chatID, topicID)).Scan(&projectID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return projectID, err
}
