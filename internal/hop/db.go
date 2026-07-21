package hop

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

const schemaVersion = 7

var (
	ErrNotFound            = errors.New("hop: not found")
	ErrNotInitialized      = errors.New("hop: repository is not initialized")
	ErrAlreadyInitialized  = errors.New("hop: repository is already initialized")
	ErrAttemptHeadChanged  = errors.New("hop: attempt head changed")
	ErrAcceptedHeadChanged = errors.New("hop: accepted head changed")
	ErrMaterializedChanged = errors.New("hop: materialized head changed")
)

// HeadChangedError reports a failed compare-and-swap of an attempt or the
// repository's accepted head. Callers may use errors.Is with
// ErrAttemptHeadChanged or ErrAcceptedHeadChanged.
type HeadChangedError struct {
	Scope    string `json:"scope"`
	Expected string `json:"expected"`
	Actual   string `json:"actual"`
}

func (e *HeadChangedError) Error() string {
	return fmt.Sprintf("hop: %s head changed: expected %q, found %q", e.Scope, e.Expected, e.Actual)
}

func (e *HeadChangedError) Unwrap() error {
	switch e.Scope {
	case "accepted":
		return ErrAcceptedHeadChanged
	case "materialized":
		return ErrMaterializedChanged
	default:
		return ErrAttemptHeadChanged
	}
}

// Event is an immutable audit record. Payload is deliberately unstructured so
// newer clients can add detail without requiring a database migration.
type Event struct {
	ID        string          `json:"id"`
	Kind      string          `json:"kind"`
	TaskID    string          `json:"task_id,omitempty"`
	AttemptID string          `json:"attempt_id,omitempty"`
	StateID   string          `json:"state_id,omitempty"`
	Payload   json.RawMessage `json:"payload,omitempty"`
	CreatedAt time.Time       `json:"created_at"`
}

// Store owns Hop's durable state graph. One database connection is used per
// Store so connection-scoped SQLite settings (notably foreign_keys) cannot be
// lost when database/sql selects another connection.
type Store struct {
	db   *sql.DB
	path string
}

// OpenStore opens (and, when needed, creates) a Hop SQLite database, applies
// migrations, and enables foreign keys and WAL mode.
func OpenStore(path string) (*Store, error) {
	if strings.TrimSpace(path) == "" {
		return nil, errors.New("hop: database path is required")
	}
	if path != ":memory:" && !strings.HasPrefix(path, "file:") {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return nil, fmt.Errorf("create database directory: %w", err)
		}
	}

	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)

	store := &Store{db: db, path: path}
	if err := store.configure(context.Background()); err != nil {
		_ = db.Close()
		return nil, err
	}
	if err := store.migrate(context.Background()); err != nil {
		_ = db.Close()
		return nil, err
	}
	return store, nil
}

// Open is a convenience alias for OpenStore.
func Open(path string) (*Store, error) { return OpenStore(path) }

func (s *Store) Path() string { return s.path }

func (s *Store) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

func (s *Store) configure(ctx context.Context) error {
	for _, statement := range []string{
		"PRAGMA busy_timeout = 5000",
		"PRAGMA foreign_keys = ON",
		"PRAGMA synchronous = NORMAL",
	} {
		if _, err := s.db.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("configure sqlite (%s): %w", statement, err)
		}
	}
	var journalMode string
	if err := s.db.QueryRowContext(ctx, "PRAGMA journal_mode = WAL").Scan(&journalMode); err != nil {
		return fmt.Errorf("enable sqlite WAL: %w", err)
	}
	var foreignKeys int
	if err := s.db.QueryRowContext(ctx, "PRAGMA foreign_keys").Scan(&foreignKeys); err != nil {
		return fmt.Errorf("verify sqlite foreign keys: %w", err)
	}
	if foreignKeys != 1 {
		return errors.New("hop: sqlite foreign keys could not be enabled")
	}
	return nil
}

type migration struct {
	version    int
	statements []string
}

var migrations = []migration{
	{
		version: 1,
		statements: []string{
			`CREATE TABLE IF NOT EXISTS meta (
			key TEXT PRIMARY KEY,
			value TEXT NOT NULL
		) STRICT`,
			`CREATE TABLE IF NOT EXISTS tasks (
			id TEXT PRIMARY KEY,
			title TEXT NOT NULL,
			status TEXT NOT NULL,
			created_at TEXT NOT NULL
		) STRICT`,
			`CREATE TABLE IF NOT EXISTS states (
			id TEXT PRIMARY KEY,
			kind TEXT NOT NULL,
			task_id TEXT REFERENCES tasks(id) ON DELETE SET NULL DEFERRABLE INITIALLY DEFERRED,
			attempt_id TEXT REFERENCES attempts(id) ON DELETE SET NULL DEFERRABLE INITIALLY DEFERRED,
			canonical_anchor_id TEXT REFERENCES states(id) ON DELETE SET NULL DEFERRABLE INITIALLY DEFERRED,
			source_tree TEXT NOT NULL,
			git_commit TEXT NOT NULL,
			prompt TEXT NOT NULL,
			summary TEXT NOT NULL,
			agent TEXT NOT NULL,
			digest TEXT NOT NULL,
			created_at TEXT NOT NULL
		) STRICT`,
			`CREATE TABLE IF NOT EXISTS attempts (
			id TEXT PRIMARY KEY,
			task_id TEXT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE DEFERRABLE INITIALLY DEFERRED,
			agent TEXT NOT NULL,
			workspace TEXT NOT NULL,
			base_state_id TEXT NOT NULL REFERENCES states(id) ON DELETE RESTRICT DEFERRABLE INITIALLY DEFERRED,
			head_state_id TEXT REFERENCES states(id) ON DELETE SET NULL DEFERRABLE INITIALLY DEFERRED,
			status TEXT NOT NULL,
			created_at TEXT NOT NULL
		) STRICT`,
			`CREATE TABLE IF NOT EXISTS state_parents (
			state_id TEXT NOT NULL REFERENCES states(id) ON DELETE CASCADE,
			parent_state_id TEXT NOT NULL REFERENCES states(id) ON DELETE RESTRICT,
			role TEXT NOT NULL,
			parent_order INTEGER NOT NULL,
			PRIMARY KEY (state_id, parent_order),
			UNIQUE (state_id, parent_state_id, role)
		) STRICT`,
			`CREATE TABLE IF NOT EXISTS checks (
			id TEXT PRIMARY KEY,
			attempt_id TEXT NOT NULL REFERENCES attempts(id) ON DELETE CASCADE,
			state_id TEXT REFERENCES states(id) ON DELETE SET NULL,
			tree_hash TEXT NOT NULL,
			command_json TEXT NOT NULL,
			exit_code INTEGER NOT NULL,
			output TEXT NOT NULL,
			created_at TEXT NOT NULL
		) STRICT`,
			`CREATE TABLE IF NOT EXISTS events (
			id TEXT PRIMARY KEY,
			kind TEXT NOT NULL,
			task_id TEXT REFERENCES tasks(id) ON DELETE SET NULL,
			attempt_id TEXT REFERENCES attempts(id) ON DELETE SET NULL,
			state_id TEXT REFERENCES states(id) ON DELETE SET NULL,
			payload_json TEXT NOT NULL,
			created_at TEXT NOT NULL
		) STRICT`,
			`CREATE INDEX IF NOT EXISTS states_task_created_idx ON states(task_id, created_at, id)`,
			`CREATE INDEX IF NOT EXISTS states_attempt_created_idx ON states(attempt_id, created_at, id)`,
			`CREATE INDEX IF NOT EXISTS states_kind_created_idx ON states(kind, created_at, id)`,
			`CREATE INDEX IF NOT EXISTS state_parents_parent_idx ON state_parents(parent_state_id, state_id)`,
			`CREATE INDEX IF NOT EXISTS state_parents_role_idx ON state_parents(state_id, role, parent_order)`,
			`CREATE INDEX IF NOT EXISTS attempts_task_created_idx ON attempts(task_id, created_at, id)`,
			`CREATE INDEX IF NOT EXISTS attempts_status_created_idx ON attempts(status, created_at, id)`,
			`CREATE INDEX IF NOT EXISTS checks_attempt_tree_idx ON checks(attempt_id, tree_hash, created_at, id)`,
			`CREATE INDEX IF NOT EXISTS checks_tree_idx ON checks(tree_hash, created_at, id)`,
			`CREATE INDEX IF NOT EXISTS events_created_idx ON events(created_at, id)`,
			`CREATE INDEX IF NOT EXISTS events_task_idx ON events(task_id, created_at, id)`,
			`CREATE INDEX IF NOT EXISTS events_attempt_idx ON events(attempt_id, created_at, id)`,
		},
	},
	{
		version: 2,
		statements: []string{
			`CREATE TABLE IF NOT EXISTS agent_sessions (
				agent TEXT NOT NULL,
				session_id TEXT NOT NULL,
				head_state_id TEXT NOT NULL REFERENCES states(id) ON DELETE RESTRICT,
				created_at TEXT NOT NULL,
				updated_at TEXT NOT NULL,
				PRIMARY KEY (agent, session_id)
			) STRICT`,
			`CREATE INDEX IF NOT EXISTS agent_sessions_head_idx ON agent_sessions(head_state_id)`,
		},
	},
	{
		version: 3,
		statements: []string{
			`INSERT OR IGNORE INTO meta(key, value)
			 SELECT 'materialized_head', id
			 FROM states
			 WHERE kind = 'accepted'
			 ORDER BY created_at, id
			 LIMIT 1`,
		},
	},
	{
		version: 4,
		statements: []string{
			`CREATE TABLE IF NOT EXISTS prompt_sync_receipts (
				server TEXT NOT NULL,
				repository_owner TEXT NOT NULL,
				repository_name TEXT NOT NULL,
				state_id TEXT NOT NULL,
				revision TEXT NOT NULL,
				deleted INTEGER NOT NULL DEFAULT 0 CHECK (deleted IN (0, 1)),
				synced_at TEXT NOT NULL,
				PRIMARY KEY (server, repository_owner, repository_name, state_id)
			) STRICT`,
			`CREATE INDEX IF NOT EXISTS prompt_sync_receipts_state_idx ON prompt_sync_receipts(state_id)`,
		},
	},
	{
		version: 5,
		statements: []string{
			`CREATE TABLE IF NOT EXISTS prompt_completions (
				prompt_state_id TEXT PRIMARY KEY REFERENCES states(id) ON DELETE CASCADE,
				summary TEXT NOT NULL,
				final_response TEXT NOT NULL,
				completed_at TEXT NOT NULL
			) STRICT`,
			`CREATE INDEX IF NOT EXISTS prompt_completions_completed_idx ON prompt_completions(completed_at, prompt_state_id)`,
		},
	},
	{
		version: 6,
		statements: []string{
			`CREATE TABLE IF NOT EXISTS publications (
				accepted_state_id TEXT PRIMARY KEY REFERENCES states(id) ON DELETE CASCADE,
				commit_oid TEXT NOT NULL,
				status TEXT NOT NULL,
				remote TEXT NOT NULL,
				ref TEXT NOT NULL,
				remote_tip TEXT NOT NULL,
				attempted_at TEXT NOT NULL,
				error_category TEXT NOT NULL,
				error_message TEXT NOT NULL,
				retryable INTEGER NOT NULL CHECK (retryable IN (0, 1))
			) STRICT`,
			`CREATE INDEX IF NOT EXISTS publications_status_attempted_idx ON publications(status, attempted_at, accepted_state_id)`,
		},
	},
	{
		version: 7,
		statements: []string{
			`ALTER TABLE states ADD COLUMN provenance_json TEXT NOT NULL DEFAULT ''`,
		},
	},
}

func (s *Store) migrate(ctx context.Context) error {
	var current int
	if err := s.db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&current); err != nil {
		return fmt.Errorf("read schema version: %w", err)
	}
	if current > schemaVersion {
		return fmt.Errorf("hop: database schema version %d is newer than supported version %d", current, schemaVersion)
	}
	for _, m := range migrations {
		if m.version <= current {
			continue
		}
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("begin schema migration %d: %w", m.version, err)
		}
		ok := false
		defer func() {
			if !ok {
				_ = tx.Rollback()
			}
		}()
		for _, statement := range m.statements {
			if _, err := tx.ExecContext(ctx, statement); err != nil {
				_ = tx.Rollback()
				return fmt.Errorf("apply schema migration %d: %w", m.version, err)
			}
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO meta(key, value) VALUES ('schema_version', ?)
			 ON CONFLICT(key) DO UPDATE SET value = excluded.value`, strconv.Itoa(m.version)); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("record schema migration %d: %w", m.version, err)
		}
		if _, err := tx.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", m.version)); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("record sqlite schema version %d: %w", m.version, err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("commit schema migration %d: %w", m.version, err)
		}
		ok = true
		current = m.version
	}
	return nil
}

// CreateInitialState atomically records the first accepted state and repository
// root, and makes that state the accepted head.
func (s *Store) CreateInitialState(ctx context.Context, root string, initial State) (State, error) {
	if strings.TrimSpace(root) == "" {
		return State{}, errors.New("hop: repository root is required")
	}
	if initial.Kind == "" {
		initial.Kind = StateAccepted
	}
	if initial.Kind != StateAccepted {
		return State{}, fmt.Errorf("hop: initial state must have kind %q", StateAccepted)
	}
	if initial.ID == "" {
		initial.ID = newID(stateIDPrefix(initial.Kind))
	}
	if initial.CreatedAt.IsZero() {
		initial.CreatedAt = time.Now().UTC()
	}
	initial.TaskID = ""
	initial.AttemptID = ""
	initial.CanonicalAnchorID = ""
	initial.Parents = nil

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return State{}, fmt.Errorf("begin repository initialization: %w", err)
	}
	defer tx.Rollback()

	var existing string
	err = tx.QueryRowContext(ctx, `SELECT value FROM meta WHERE key = 'accepted_head'`).Scan(&existing)
	if err == nil {
		return State{}, ErrAlreadyInitialized
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return State{}, fmt.Errorf("check repository initialization: %w", err)
	}

	if err := insertStateTx(ctx, tx, initial, nil); err != nil {
		return State{}, err
	}
	for key, value := range map[string]string{
		"root":              filepath.Clean(root),
		"accepted_head":     initial.ID,
		"materialized_head": initial.ID,
	} {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO meta(key, value) VALUES (?, ?)
			 ON CONFLICT(key) DO UPDATE SET value = excluded.value`, key, value); err != nil {
			return State{}, fmt.Errorf("write repository metadata %q: %w", key, err)
		}
	}
	if err := insertEventTx(ctx, tx, Event{
		Kind:    "repository.initialized",
		StateID: initial.ID,
	}, map[string]string{"root": filepath.Clean(root)}); err != nil {
		return State{}, err
	}
	if err := tx.Commit(); err != nil {
		return State{}, fmt.Errorf("commit repository initialization: %w", err)
	}
	return initial, nil
}

// Initialize is a convenience alias for CreateInitialState.
func (s *Store) Initialize(ctx context.Context, root string, initial State) (State, error) {
	return s.CreateInitialState(ctx, root, initial)
}

// IsInitialized reports whether an accepted head has been installed.
func (s *Store) IsInitialized(ctx context.Context) (bool, error) {
	_, err := s.AcceptedHeadID(ctx)
	if errors.Is(err, ErrNotInitialized) {
		return false, nil
	}
	return err == nil, err
}

// AgentSessionHead returns the latest prompt state associated with an agent
// session. Session heads let interactive agents turn follow-up messages into
// descendants without asking the human to carry Hop state IDs between turns.
func (s *Store) AgentSessionHead(ctx context.Context, agent, sessionID string) (string, bool, error) {
	if strings.TrimSpace(agent) == "" || strings.TrimSpace(sessionID) == "" {
		return "", false, errors.New("hop: agent and session ID are required")
	}
	var stateID string
	err := s.db.QueryRowContext(ctx,
		`SELECT head_state_id FROM agent_sessions WHERE agent = ? AND session_id = ?`,
		agent, sessionID).Scan(&stateID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("read agent session head: %w", err)
	}
	return stateID, true, nil
}

// SetAgentSessionHead records the prompt state that should parent the next
// message in an interactive agent session.
func (s *Store) SetAgentSessionHead(ctx context.Context, agent, sessionID, stateID string) error {
	if strings.TrimSpace(agent) == "" || strings.TrimSpace(sessionID) == "" || strings.TrimSpace(stateID) == "" {
		return errors.New("hop: agent, session ID, and state ID are required")
	}
	if redacted, findings := RedactPromptSecrets(agent + "\n" + sessionID); len(findings) > 0 || redacted != agent+"\n"+sessionID {
		return errors.New("hop: refusing to persist a potential credential in agent session metadata")
	}
	now := formatTime(time.Now().UTC())
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO agent_sessions(agent, session_id, head_state_id, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?)
		 ON CONFLICT(agent, session_id) DO UPDATE SET
		   head_state_id = excluded.head_state_id,
		   updated_at = excluded.updated_at`,
		agent, sessionID, stateID, now, now); err != nil {
		return fmt.Errorf("record agent session head: %w", err)
	}
	return nil
}

// ClearAgentSessionsForAttempt prevents a terminal workspace that has been
// reclaimed from being reopened by a later prompt in the same agent session.
func (s *Store) ClearAgentSessionsForAttempt(ctx context.Context, attemptID string) error {
	if strings.TrimSpace(attemptID) == "" {
		return errors.New("hop: attempt ID is required")
	}
	if _, err := s.db.ExecContext(ctx,
		`DELETE FROM agent_sessions
		 WHERE head_state_id IN (SELECT id FROM states WHERE attempt_id = ?)`, attemptID); err != nil {
		return fmt.Errorf("clear terminal attempt sessions: %w", err)
	}
	return nil
}

// RetargetAgentSessions moves interactive sessions that currently point into
// one attempt to a successor prompt. Reconciliation uses this so a follow-up
// received while conflicts are being resolved reaches the reconciliation
// workspace instead of reopening the stale source workspace.
func (s *Store) RetargetAgentSessions(ctx context.Context, agent, fromAttemptID, expectedHeadID, toStateID string) error {
	if strings.TrimSpace(agent) == "" || strings.TrimSpace(fromAttemptID) == "" ||
		strings.TrimSpace(expectedHeadID) == "" || strings.TrimSpace(toStateID) == "" {
		return errors.New("hop: agent, source attempt, expected head, and target state are required")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin agent session retarget: %w", err)
	}
	defer tx.Rollback()
	attempt, err := attemptTx(ctx, tx, fromAttemptID)
	if err != nil {
		return err
	}
	if attempt.HeadStateID != expectedHeadID {
		return &HeadChangedError{Scope: "attempt", Expected: expectedHeadID, Actual: attempt.HeadStateID}
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE agent_sessions
		 SET head_state_id = ?, updated_at = ?
		 WHERE agent = ?
		   AND head_state_id IN (SELECT id FROM states WHERE attempt_id = ?)`,
		toStateID, formatTime(time.Now().UTC()), agent, fromAttemptID); err != nil {
		return fmt.Errorf("retarget agent sessions: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit agent session retarget: %w", err)
	}
	return nil
}

// CreateTaskAttemptPrompt atomically creates all three records. The prompt is
// durable and the attempt head points to it before the transaction becomes
// visible, so an orchestrator may safely deliver the prompt after this returns.
func (s *Store) CreateTaskAttemptPrompt(
	ctx context.Context,
	task Task,
	attempt Attempt,
	prompt State,
	parents []Parent,
) (Task, Attempt, State, error) {
	now := time.Now().UTC()
	if task.ID == "" {
		task.ID = newID("t")
	}
	if task.CreatedAt.IsZero() {
		task.CreatedAt = now
	}
	if task.Status == "" {
		task.Status = "active"
	}
	if task.Title == "" {
		task.Title = prompt.Prompt
	}
	task.Title, _ = RedactPromptSecrets(task.Title)
	if attempt.ID == "" {
		attempt.ID = newID("at")
	}
	if attempt.TaskID == "" {
		attempt.TaskID = task.ID
	}
	if attempt.TaskID != task.ID {
		return Task{}, Attempt{}, State{}, errors.New("hop: attempt task does not match task")
	}
	if attempt.CreatedAt.IsZero() {
		attempt.CreatedAt = now
	}
	if attempt.Status == "" {
		attempt.Status = "active"
	}
	if prompt.ID == "" {
		prompt.ID = newID("p")
	}
	if prompt.Kind == "" {
		prompt.Kind = StatePrompt
	}
	if prompt.Kind != StatePrompt {
		return Task{}, Attempt{}, State{}, fmt.Errorf("hop: task instruction must have kind %q", StatePrompt)
	}
	if prompt.TaskID == "" {
		prompt.TaskID = task.ID
	}
	if prompt.TaskID != task.ID {
		return Task{}, Attempt{}, State{}, errors.New("hop: prompt task does not match task")
	}
	if prompt.AttemptID == "" {
		prompt.AttemptID = attempt.ID
	}
	if prompt.AttemptID != attempt.ID {
		return Task{}, Attempt{}, State{}, errors.New("hop: prompt attempt does not match attempt")
	}
	if prompt.Agent == "" {
		prompt.Agent = attempt.Agent
	}
	if prompt.CreatedAt.IsZero() {
		prompt.CreatedAt = now
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Task{}, Attempt{}, State{}, fmt.Errorf("begin task creation: %w", err)
	}
	defer tx.Rollback()

	acceptedHead, err := metaTx(ctx, tx, "accepted_head")
	if errors.Is(err, sql.ErrNoRows) {
		return Task{}, Attempt{}, State{}, ErrNotInitialized
	}
	if err != nil {
		return Task{}, Attempt{}, State{}, fmt.Errorf("read accepted head: %w", err)
	}
	if attempt.BaseStateID == "" {
		attempt.BaseStateID = acceptedHead
	}
	base, err := stateTx(ctx, tx, attempt.BaseStateID)
	if err != nil {
		return Task{}, Attempt{}, State{}, fmt.Errorf("read attempt base state: %w", err)
	}
	if attempt.HeadStateID != "" && attempt.HeadStateID != prompt.ID {
		return Task{}, Attempt{}, State{}, errors.New("hop: new attempt head must be the prompt state")
	}
	attempt.HeadStateID = prompt.ID
	if prompt.CanonicalAnchorID == "" {
		prompt.CanonicalAnchorID = acceptedHead
	}
	if prompt.SourceTree == "" {
		prompt.SourceTree = base.SourceTree
	}
	if prompt.GitCommit == "" {
		prompt.GitCommit = base.GitCommit
	}

	parents = chooseParents(prompt.Parents, parents)
	parents = ensureParentRole(parents, "run_parent", attempt.BaseStateID)
	parents = ensureParentRole(parents, "canonical_anchor", prompt.CanonicalAnchorID)
	parents = canonicalizeParents(parents)
	prompt.Parents = parents

	if _, err := tx.ExecContext(ctx,
		`INSERT INTO tasks(id, title, status, created_at) VALUES (?, ?, ?, ?)`,
		task.ID, task.Title, task.Status, formatTime(task.CreatedAt)); err != nil {
		return Task{}, Attempt{}, State{}, fmt.Errorf("insert task: %w", err)
	}
	// head_state_id is temporarily NULL to break the intentional attempt/state
	// reference cycle; it is populated before this transaction commits.
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO attempts(id, task_id, agent, workspace, base_state_id, head_state_id, status, created_at)
		 VALUES (?, ?, ?, ?, ?, NULL, ?, ?)`,
		attempt.ID, attempt.TaskID, attempt.Agent, attempt.Workspace, attempt.BaseStateID,
		attempt.Status, formatTime(attempt.CreatedAt)); err != nil {
		return Task{}, Attempt{}, State{}, fmt.Errorf("insert attempt: %w", err)
	}
	if err := insertStateTx(ctx, tx, prompt, parents); err != nil {
		return Task{}, Attempt{}, State{}, err
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE attempts SET head_state_id = ? WHERE id = ?`, prompt.ID, attempt.ID); err != nil {
		return Task{}, Attempt{}, State{}, fmt.Errorf("install initial attempt head: %w", err)
	}
	if err := insertEventTx(ctx, tx, Event{
		Kind: "task.created", TaskID: task.ID,
	}, map[string]string{"title": task.Title}); err != nil {
		return Task{}, Attempt{}, State{}, err
	}
	if err := insertEventTx(ctx, tx, Event{
		Kind: "prompt.created", TaskID: task.ID, AttemptID: attempt.ID, StateID: prompt.ID,
	}, nil); err != nil {
		return Task{}, Attempt{}, State{}, err
	}
	if err := tx.Commit(); err != nil {
		return Task{}, Attempt{}, State{}, fmt.Errorf("commit task creation: %w", err)
	}
	return task, attempt, prompt, nil
}

// CreateAttemptPrompt atomically adds a new attempt and its initial prompt to
// an existing task. Reconciliation uses a fresh workspace so a frozen proposal
// and any concurrent work in its original attempt are never overwritten.
func (s *Store) CreateAttemptPrompt(
	ctx context.Context,
	attempt Attempt,
	prompt State,
	parents []Parent,
	sessionSourceAttemptID string,
	expectedSourceHeadID string,
) (Attempt, State, error) {
	now := time.Now().UTC()
	if attempt.ID == "" {
		attempt.ID = newID("at")
	}
	if attempt.TaskID == "" {
		return Attempt{}, State{}, errors.New("hop: reconciliation attempt requires a task ID")
	}
	if attempt.BaseStateID == "" {
		return Attempt{}, State{}, errors.New("hop: reconciliation attempt requires a base state")
	}
	if strings.TrimSpace(attempt.Workspace) == "" {
		return Attempt{}, State{}, errors.New("hop: reconciliation attempt requires a workspace")
	}
	if attempt.CreatedAt.IsZero() {
		attempt.CreatedAt = now
	}
	if attempt.Status == "" {
		attempt.Status = "reconciling"
	}
	if prompt.ID == "" {
		prompt.ID = newID("p")
	}
	if prompt.Kind == "" {
		prompt.Kind = StatePrompt
	}
	if prompt.Kind != StatePrompt {
		return Attempt{}, State{}, fmt.Errorf("hop: attempt instruction must have kind %q", StatePrompt)
	}
	if prompt.TaskID == "" {
		prompt.TaskID = attempt.TaskID
	}
	if prompt.TaskID != attempt.TaskID {
		return Attempt{}, State{}, errors.New("hop: prompt task does not match reconciliation attempt")
	}
	if prompt.AttemptID == "" {
		prompt.AttemptID = attempt.ID
	}
	if prompt.AttemptID != attempt.ID {
		return Attempt{}, State{}, errors.New("hop: prompt attempt does not match reconciliation attempt")
	}
	if prompt.Agent == "" {
		prompt.Agent = attempt.Agent
	}
	if prompt.CreatedAt.IsZero() {
		prompt.CreatedAt = now
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Attempt{}, State{}, fmt.Errorf("begin reconciliation attempt creation: %w", err)
	}
	defer tx.Rollback()

	var taskExists int
	if err := tx.QueryRowContext(ctx, `SELECT 1 FROM tasks WHERE id = ?`, attempt.TaskID).Scan(&taskExists); err != nil {
		return Attempt{}, State{}, dbNotFound("task", attempt.TaskID, err)
	}
	if sessionSourceAttemptID != "" {
		if expectedSourceHeadID == "" {
			return Attempt{}, State{}, errors.New("hop: reconciliation source head is required")
		}
		sourceAttempt, err := attemptTx(ctx, tx, sessionSourceAttemptID)
		if err != nil {
			return Attempt{}, State{}, err
		}
		if sourceAttempt.HeadStateID != expectedSourceHeadID {
			return Attempt{}, State{}, &HeadChangedError{
				Scope: "attempt", Expected: expectedSourceHeadID, Actual: sourceAttempt.HeadStateID,
			}
		}
	}
	base, err := stateTx(ctx, tx, attempt.BaseStateID)
	if err != nil {
		return Attempt{}, State{}, fmt.Errorf("read reconciliation base state: %w", err)
	}
	if prompt.CanonicalAnchorID == "" {
		prompt.CanonicalAnchorID = attempt.BaseStateID
	}
	if prompt.SourceTree == "" {
		prompt.SourceTree = base.SourceTree
	}
	if prompt.GitCommit == "" {
		prompt.GitCommit = base.GitCommit
	}
	parents = chooseParents(prompt.Parents, parents)
	parents = ensureParentRole(parents, "run_parent", attempt.BaseStateID)
	parents = ensureParentRole(parents, "canonical_anchor", prompt.CanonicalAnchorID)
	parents = canonicalizeParents(parents)
	prompt.Parents = parents
	attempt.HeadStateID = prompt.ID

	if _, err := tx.ExecContext(ctx,
		`INSERT INTO attempts(id, task_id, agent, workspace, base_state_id, head_state_id, status, created_at)
		 VALUES (?, ?, ?, ?, ?, NULL, ?, ?)`,
		attempt.ID, attempt.TaskID, attempt.Agent, attempt.Workspace, attempt.BaseStateID,
		attempt.Status, formatTime(attempt.CreatedAt)); err != nil {
		return Attempt{}, State{}, fmt.Errorf("insert reconciliation attempt: %w", err)
	}
	if err := insertStateTx(ctx, tx, prompt, parents); err != nil {
		return Attempt{}, State{}, err
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE attempts SET head_state_id = ? WHERE id = ?`, prompt.ID, attempt.ID); err != nil {
		return Attempt{}, State{}, fmt.Errorf("install reconciliation attempt head: %w", err)
	}
	if sessionSourceAttemptID != "" {
		if _, err := tx.ExecContext(ctx,
			`UPDATE agent_sessions
			 SET head_state_id = ?, updated_at = ?
			 WHERE agent = ?
			   AND head_state_id IN (SELECT id FROM states WHERE attempt_id = ?)`,
			prompt.ID, formatTime(now), attempt.Agent, sessionSourceAttemptID); err != nil {
			return Attempt{}, State{}, fmt.Errorf("retarget reconciliation sessions: %w", err)
		}
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE tasks SET status = 'reconciling' WHERE id = ?`, attempt.TaskID); err != nil {
		return Attempt{}, State{}, fmt.Errorf("mark task reconciling: %w", err)
	}
	if err := insertEventTx(ctx, tx, Event{
		Kind: "attempt.created", TaskID: attempt.TaskID, AttemptID: attempt.ID,
	}, map[string]string{"purpose": "reconciliation"}); err != nil {
		return Attempt{}, State{}, err
	}
	if err := insertEventTx(ctx, tx, Event{
		Kind: "prompt.created", TaskID: attempt.TaskID, AttemptID: attempt.ID, StateID: prompt.ID,
	}, map[string]string{"purpose": "reconciliation"}); err != nil {
		return Attempt{}, State{}, err
	}
	if err := tx.Commit(); err != nil {
		return Attempt{}, State{}, fmt.Errorf("commit reconciliation attempt creation: %w", err)
	}
	return attempt, prompt, nil
}

// AppendState atomically appends an immutable state and compare-and-swaps the
// owning attempt's head. An empty expectedAttemptHeadID means "the head observed
// by this transaction"; callers that already observed a head should pass it.
func (s *Store) AppendState(
	ctx context.Context,
	state State,
	parents []Parent,
	expectedAttemptHeadID string,
) (State, error) {
	if state.AttemptID == "" {
		return State{}, errors.New("hop: appended state requires an attempt ID")
	}
	if state.ID == "" {
		state.ID = newID(stateIDPrefix(state.Kind))
	}
	if state.Kind == "" {
		state.Kind = StateCheckpoint
	}
	if state.CreatedAt.IsZero() {
		state.CreatedAt = time.Now().UTC()
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return State{}, fmt.Errorf("begin state append: %w", err)
	}
	defer tx.Rollback()

	attempt, err := attemptTx(ctx, tx, state.AttemptID)
	if err != nil {
		return State{}, err
	}
	if expectedAttemptHeadID == "" {
		expectedAttemptHeadID = attempt.HeadStateID
	}
	if attempt.HeadStateID != expectedAttemptHeadID {
		return State{}, &HeadChangedError{Scope: "attempt", Expected: expectedAttemptHeadID, Actual: attempt.HeadStateID}
	}
	if state.TaskID == "" {
		state.TaskID = attempt.TaskID
	}
	if state.TaskID != attempt.TaskID {
		return State{}, errors.New("hop: appended state task does not match attempt")
	}
	if state.Agent == "" {
		state.Agent = attempt.Agent
	}
	head, err := stateTx(ctx, tx, attempt.HeadStateID)
	if err != nil {
		return State{}, fmt.Errorf("read attempt head state: %w", err)
	}
	if state.SourceTree == "" {
		state.SourceTree = head.SourceTree
	}
	if state.GitCommit == "" {
		state.GitCommit = head.GitCommit
	}
	if state.CanonicalAnchorID == "" {
		state.CanonicalAnchorID = head.CanonicalAnchorID
		if state.CanonicalAnchorID == "" {
			state.CanonicalAnchorID = attempt.BaseStateID
		}
	}
	parents = chooseParents(state.Parents, parents)
	parents = ensureParentRole(parents, "run_parent", expectedAttemptHeadID)
	if state.Kind == StatePrompt {
		parents = ensureParentRole(parents, "canonical_anchor", state.CanonicalAnchorID)
	}
	parents = canonicalizeParents(parents)
	state.Parents = parents
	if state.Kind == StateCheckpoint || state.Kind == StateProposal {
		if state.Provenance == nil {
			return State{}, &ProvenanceError{Operation: string(state.Kind), Reason: "tree-producing attempt state has no durable authorization proof"}
		}
		base, err := stateTx(ctx, tx, state.Provenance.BaseStateID)
		if err != nil {
			return State{}, fmt.Errorf("read provenance base: %w", err)
		}
		if err := validateStoredProvenance(state, base); err != nil {
			return State{}, err
		}
	}

	if err := insertStateTx(ctx, tx, state, parents); err != nil {
		return State{}, err
	}
	result, err := tx.ExecContext(ctx,
		`UPDATE attempts SET head_state_id = ? WHERE id = ? AND head_state_id = ?`,
		state.ID, state.AttemptID, expectedAttemptHeadID)
	if err != nil {
		return State{}, fmt.Errorf("advance attempt head: %w", err)
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return State{}, fmt.Errorf("inspect attempt head update: %w", err)
	}
	if changed != 1 {
		actual, _ := attemptHeadTx(ctx, tx, state.AttemptID)
		return State{}, &HeadChangedError{Scope: "attempt", Expected: expectedAttemptHeadID, Actual: actual}
	}
	if err := insertEventTx(ctx, tx, Event{
		Kind: "state.appended", TaskID: state.TaskID, AttemptID: state.AttemptID, StateID: state.ID,
	}, map[string]string{"kind": string(state.Kind)}); err != nil {
		return State{}, err
	}
	if err := tx.Commit(); err != nil {
		return State{}, fmt.Errorf("commit state append: %w", err)
	}
	return state, nil
}

// RecordState inserts immutable attempt evidence without advancing the attempt
// head. It is used for observations about a frozen state, such as a failed
// final-tree validation, that must remain durable but must not invalidate the
// proposal being observed.
func (s *Store) RecordState(
	ctx context.Context,
	state State,
	parents []Parent,
	expectedAttemptHeadID string,
) (State, error) {
	if state.AttemptID == "" {
		return State{}, errors.New("hop: recorded state requires an attempt ID")
	}
	if expectedAttemptHeadID == "" {
		return State{}, errors.New("hop: recorded state requires the expected attempt head")
	}
	if state.ID == "" {
		state.ID = newID(stateIDPrefix(state.Kind))
	}
	if state.Kind == "" {
		return State{}, errors.New("hop: recorded state kind is required")
	}
	if state.CreatedAt.IsZero() {
		state.CreatedAt = time.Now().UTC()
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return State{}, fmt.Errorf("begin state recording: %w", err)
	}
	defer tx.Rollback()
	attempt, err := attemptTx(ctx, tx, state.AttemptID)
	if err != nil {
		return State{}, err
	}
	if attempt.HeadStateID != expectedAttemptHeadID {
		return State{}, &HeadChangedError{Scope: "attempt", Expected: expectedAttemptHeadID, Actual: attempt.HeadStateID}
	}
	head, err := stateTx(ctx, tx, expectedAttemptHeadID)
	if err != nil {
		return State{}, fmt.Errorf("read recorded state head: %w", err)
	}
	if state.TaskID == "" {
		state.TaskID = attempt.TaskID
	}
	if state.TaskID != attempt.TaskID {
		return State{}, errors.New("hop: recorded state task does not match attempt")
	}
	if state.Agent == "" {
		state.Agent = attempt.Agent
	}
	if state.SourceTree == "" {
		state.SourceTree = head.SourceTree
	}
	if state.GitCommit == "" {
		state.GitCommit = head.GitCommit
	}
	if state.CanonicalAnchorID == "" {
		state.CanonicalAnchorID = head.CanonicalAnchorID
		if state.CanonicalAnchorID == "" {
			state.CanonicalAnchorID = attempt.BaseStateID
		}
	}
	parents = chooseParents(state.Parents, parents)
	parents = ensureParentRole(parents, "run_parent", expectedAttemptHeadID)
	parents = canonicalizeParents(parents)
	state.Parents = parents
	if err := insertStateTx(ctx, tx, state, parents); err != nil {
		return State{}, err
	}
	if err := insertEventTx(ctx, tx, Event{
		Kind: "state.recorded", TaskID: state.TaskID, AttemptID: state.AttemptID, StateID: state.ID,
	}, map[string]string{"kind": string(state.Kind)}); err != nil {
		return State{}, err
	}
	if err := tx.Commit(); err != nil {
		return State{}, fmt.Errorf("commit state recording: %w", err)
	}
	return state, nil
}

// CASAccept atomically inserts an accepted state and advances accepted_head only
// if it still equals expectedHeadID. Any inserted state is rolled back when the
// compare-and-swap fails.
func (s *Store) CASAccept(
	ctx context.Context,
	expectedHeadID string,
	accepted State,
	parents []Parent,
) (State, error) {
	if expectedHeadID == "" {
		return State{}, errors.New("hop: expected accepted head is required")
	}
	if accepted.ID == "" {
		accepted.ID = newID("a")
	}
	if accepted.Kind == "" {
		accepted.Kind = StateAccepted
	}
	if accepted.Kind != StateAccepted {
		return State{}, fmt.Errorf("hop: accepted state must have kind %q", StateAccepted)
	}
	if accepted.CreatedAt.IsZero() {
		accepted.CreatedAt = time.Now().UTC()
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return State{}, fmt.Errorf("begin acceptance: %w", err)
	}
	defer tx.Rollback()

	actual, err := metaTx(ctx, tx, "accepted_head")
	if errors.Is(err, sql.ErrNoRows) {
		return State{}, ErrNotInitialized
	}
	if err != nil {
		return State{}, fmt.Errorf("read accepted head: %w", err)
	}
	if actual != expectedHeadID {
		return State{}, &HeadChangedError{Scope: "accepted", Expected: expectedHeadID, Actual: actual}
	}
	current, err := stateTx(ctx, tx, expectedHeadID)
	if err != nil {
		return State{}, fmt.Errorf("read current accepted state: %w", err)
	}

	parents = chooseParents(accepted.Parents, parents)
	parents = ensureParentRole(parents, "canonical_parent", expectedHeadID)
	parents = canonicalizeParents(parents)
	accepted.Parents = parents
	if accepted.CanonicalAnchorID == "" {
		accepted.CanonicalAnchorID = expectedHeadID
	}

	// When the caller supplied a proposal parent but omitted repeated fields,
	// inherit them from that immutable proposal.
	if proposalParent, ok := parentWithRole(parents, "proposal_parent"); ok {
		proposal, err := stateTx(ctx, tx, proposalParent.StateID)
		if err != nil {
			return State{}, fmt.Errorf("read proposal parent: %w", err)
		}
		if proposal.AttemptID != "" {
			attempt, err := attemptTx(ctx, tx, proposal.AttemptID)
			if err != nil {
				return State{}, err
			}
			if attempt.HeadStateID != proposal.ID {
				return State{}, &HeadChangedError{
					Scope: "attempt", Expected: proposal.ID, Actual: attempt.HeadStateID,
				}
			}
		}
		if accepted.TaskID == "" {
			accepted.TaskID = proposal.TaskID
		}
		if accepted.AttemptID == "" {
			accepted.AttemptID = proposal.AttemptID
		}
		if accepted.SourceTree == "" {
			accepted.SourceTree = proposal.SourceTree
		}
		if accepted.GitCommit == "" {
			accepted.GitCommit = proposal.GitCommit
		}
		if accepted.Agent == "" {
			accepted.Agent = proposal.Agent
		}
	}
	if accepted.SourceTree == "" {
		accepted.SourceTree = current.SourceTree
	}
	if accepted.GitCommit == "" {
		accepted.GitCommit = current.GitCommit
	}
	if err := validateStoredProvenance(accepted, current); err != nil {
		return State{}, err
	}

	if err := insertStateTx(ctx, tx, accepted, parents); err != nil {
		return State{}, err
	}
	result, err := tx.ExecContext(ctx,
		`UPDATE meta SET value = ? WHERE key = 'accepted_head' AND value = ?`,
		accepted.ID, expectedHeadID)
	if err != nil {
		return State{}, fmt.Errorf("advance accepted head: %w", err)
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return State{}, fmt.Errorf("inspect accepted head update: %w", err)
	}
	if changed != 1 {
		actual, _ := metaTx(ctx, tx, "accepted_head")
		return State{}, &HeadChangedError{Scope: "accepted", Expected: expectedHeadID, Actual: actual}
	}
	if accepted.AttemptID != "" {
		if _, err := tx.ExecContext(ctx,
			`UPDATE attempts SET status = 'accepted' WHERE id = ?`, accepted.AttemptID); err != nil {
			return State{}, fmt.Errorf("mark attempt accepted: %w", err)
		}
	}
	if accepted.TaskID != "" {
		if accepted.AttemptID != "" {
			if _, err := tx.ExecContext(ctx,
				`UPDATE attempts
				 SET status = 'completed'
				 WHERE task_id = ?
				   AND id != ?
				   AND status NOT IN ('accepted', 'completed', 'failed', 'cancelled', 'rejected')`,
				accepted.TaskID, accepted.AttemptID); err != nil {
				return State{}, fmt.Errorf("complete superseded task attempts: %w", err)
			}
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE tasks SET status = 'accepted' WHERE id = ?`, accepted.TaskID); err != nil {
			return State{}, fmt.Errorf("mark task accepted: %w", err)
		}
	}
	if err := insertEventTx(ctx, tx, Event{
		Kind: "state.accepted", TaskID: accepted.TaskID, AttemptID: accepted.AttemptID, StateID: accepted.ID,
	}, map[string]string{"previous_head": expectedHeadID}); err != nil {
		return State{}, err
	}
	if err := tx.Commit(); err != nil {
		return State{}, fmt.Errorf("commit acceptance: %w", err)
	}
	return accepted, nil
}

// AcceptedHeadID returns the current canonical pointer without loading the
// state body.
func (s *Store) AcceptedHeadID(ctx context.Context) (string, error) {
	var id string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM meta WHERE key = 'accepted_head'`).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotInitialized
	}
	if err != nil {
		return "", fmt.Errorf("read accepted head: %w", err)
	}
	return id, nil
}

func (s *Store) AcceptedHead(ctx context.Context) (State, error) {
	id, err := s.AcceptedHeadID(ctx)
	if err != nil {
		return State{}, err
	}
	return s.GetState(ctx, id)
}

// AcceptedForProposal returns the immutable accepted child already created for
// proposalID. It makes retry after a post-commit projection failure
// idempotent instead of creating a second acceptance transition.
func (s *Store) AcceptedForProposal(ctx context.Context, proposalID string) (State, bool, error) {
	var id string
	err := s.db.QueryRowContext(ctx,
		`SELECT p.state_id
		 FROM state_parents p
		 JOIN states s ON s.id = p.state_id
		 WHERE p.parent_state_id = ? AND p.role = 'proposal_parent' AND s.kind = 'accepted'
		 ORDER BY s.created_at, s.id
		 LIMIT 1`, proposalID).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return State{}, false, nil
	}
	if err != nil {
		return State{}, false, fmt.Errorf("find accepted state for proposal %s: %w", proposalID, err)
	}
	state, err := s.GetState(ctx, id)
	if err != nil {
		return State{}, false, err
	}
	return state, true, nil
}

// AcceptedForTask returns the latest immutable accepted outcome produced by a
// task. Unlike the mutable task status, this remains authoritative when an
// older client has accidentally reactivated an already accepted task.
func (s *Store) AcceptedForTask(ctx context.Context, taskID string) (State, bool, error) {
	if strings.TrimSpace(taskID) == "" {
		return State{}, false, errors.New("hop: task ID is required")
	}
	var id string
	err := s.db.QueryRowContext(ctx,
		`SELECT id
		 FROM states
		 WHERE task_id = ? AND kind = 'accepted'
		 ORDER BY created_at DESC, id DESC
		 LIMIT 1`, taskID).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return State{}, false, nil
	}
	if err != nil {
		return State{}, false, fmt.Errorf("find accepted state for task %s: %w", taskID, err)
	}
	state, err := s.GetState(ctx, id)
	if err != nil {
		return State{}, false, err
	}
	return state, true, nil
}

// ReconciliationPrompt returns the prompt already created to reconcile a
// proposal against a particular accepted head. The pair is unique by
// convention and makes `hop refresh` safe to retry.
func (s *Store) ReconciliationPrompt(ctx context.Context, proposalID, acceptedID string) (State, bool, error) {
	var id string
	err := s.db.QueryRowContext(ctx,
		`SELECT s.id
		 FROM states s
		 JOIN state_parents source
		   ON source.state_id = s.id
		  AND source.parent_state_id = ?
		  AND source.role = 'reconciliation_source'
		 JOIN state_parents anchor
		   ON anchor.state_id = s.id
		  AND anchor.parent_state_id = ?
		  AND anchor.role = 'canonical_anchor'
		 WHERE s.kind = 'prompt'
		 ORDER BY s.created_at DESC, s.id DESC
		 LIMIT 1`, proposalID, acceptedID).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return State{}, false, nil
	}
	if err != nil {
		return State{}, false, fmt.Errorf("find reconciliation prompt: %w", err)
	}
	state, err := s.GetState(ctx, id)
	if err != nil {
		return State{}, false, err
	}
	return state, true, nil
}

// ReconciliationPromptForAttempt identifies attempts created specifically to
// resolve a prior proposal. It lets proposal creation enforce reconciliation
// evidence even when the caller names a later checkpoint instead of the
// attempt's initial prompt.
func (s *Store) ReconciliationPromptForAttempt(ctx context.Context, attemptID string) (State, bool, error) {
	var id string
	err := s.db.QueryRowContext(ctx,
		`SELECT s.id
		 FROM states s
		 JOIN state_parents source
		   ON source.state_id = s.id
		  AND source.role = 'reconciliation_source'
		 WHERE s.attempt_id = ? AND s.kind = 'prompt'
		 ORDER BY s.created_at, s.id
		 LIMIT 1`, attemptID).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return State{}, false, nil
	}
	if err != nil {
		return State{}, false, fmt.Errorf("find reconciliation prompt for attempt %s: %w", attemptID, err)
	}
	state, err := s.GetState(ctx, id)
	if err != nil {
		return State{}, false, err
	}
	return state, true, nil
}

// MaterializedHead is the accepted state currently projected into the visible
// project root. It may trail AcceptedHead after controller-only acceptance or
// an interrupted post-acceptance synchronization.
func (s *Store) MaterializedHead(ctx context.Context) (State, error) {
	var id string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM meta WHERE key = 'materialized_head'`).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return State{}, ErrNotInitialized
	}
	if err != nil {
		return State{}, fmt.Errorf("read materialized head: %w", err)
	}
	return s.GetState(ctx, id)
}

// CASMaterializedHead advances the recoverable visible-root projection marker.
// Filesystem materialization must be verified before calling this method.
func (s *Store) CASMaterializedHead(ctx context.Context, expectedID, nextID string) error {
	if expectedID == "" || nextID == "" {
		return errors.New("hop: expected and next materialized heads are required")
	}
	if expectedID == nextID {
		return nil
	}
	next, err := s.GetState(ctx, nextID)
	if err != nil {
		return err
	}
	if next.Kind != StateAccepted {
		return fmt.Errorf("hop: materialized head must be an accepted state, found %s", next.Kind)
	}
	result, err := s.db.ExecContext(ctx,
		`UPDATE meta SET value = ? WHERE key = 'materialized_head' AND value = ?`,
		nextID, expectedID)
	if err != nil {
		return fmt.Errorf("advance materialized head: %w", err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("inspect materialized head update: %w", err)
	}
	if rows != 1 {
		var actual string
		_ = s.db.QueryRowContext(ctx, `SELECT value FROM meta WHERE key = 'materialized_head'`).Scan(&actual)
		return &HeadChangedError{Scope: "materialized", Expected: expectedID, Actual: actual}
	}
	return nil
}

func (s *Store) RepositoryRoot(ctx context.Context) (string, error) {
	var root string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM meta WHERE key = 'root'`).Scan(&root)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotInitialized
	}
	if err != nil {
		return "", fmt.Errorf("read repository root: %w", err)
	}
	return root, nil
}

func (s *Store) GetState(ctx context.Context, id string) (State, error) {
	state, err := scanState(s.db.QueryRowContext(ctx, `SELECT `+stateColumns+` FROM states WHERE id = ?`, id))
	if err != nil {
		return State{}, dbNotFound("state", id, err)
	}
	parents, err := s.GetParents(ctx, id)
	if err != nil {
		return State{}, err
	}
	state.Parents = parents
	return state, nil
}

func (s *Store) GetParents(ctx context.Context, stateID string) ([]Parent, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT parent_state_id, role, parent_order
		 FROM state_parents WHERE state_id = ? ORDER BY parent_order`, stateID)
	if err != nil {
		return nil, fmt.Errorf("query state parents: %w", err)
	}
	defer rows.Close()
	var parents []Parent
	for rows.Next() {
		var parent Parent
		if err := rows.Scan(&parent.StateID, &parent.Role, &parent.Order); err != nil {
			return nil, fmt.Errorf("scan state parent: %w", err)
		}
		parents = append(parents, parent)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate state parents: %w", err)
	}
	return parents, nil
}

// StateDescendsFrom reports whether descendantID is the same state as, or has
// any typed-parent path to, ancestorID. Session rollover uses state ancestry so
// a mutable task status cannot hide unfinished prompts created after an older
// accepted outcome.
func (s *Store) StateDescendsFrom(ctx context.Context, descendantID, ancestorID string) (bool, error) {
	if strings.TrimSpace(descendantID) == "" || strings.TrimSpace(ancestorID) == "" {
		return false, errors.New("hop: descendant and ancestor state IDs are required")
	}
	var found int
	err := s.db.QueryRowContext(ctx,
		`WITH RECURSIVE ancestry(id) AS (
			SELECT ?
			UNION
			SELECT parents.parent_state_id
			FROM state_parents parents
			JOIN ancestry ON parents.state_id = ancestry.id
		 )
		 SELECT 1 FROM ancestry WHERE id = ? LIMIT 1`,
		descendantID, ancestorID).Scan(&found)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("inspect state ancestry: %w", err)
	}
	return found == 1, nil
}

func (s *Store) ParentByRole(ctx context.Context, stateID, role string) (Parent, error) {
	var parent Parent
	err := s.db.QueryRowContext(ctx,
		`SELECT parent_state_id, role, parent_order
		 FROM state_parents WHERE state_id = ? AND role = ? ORDER BY parent_order LIMIT 1`,
		stateID, role).Scan(&parent.StateID, &parent.Role, &parent.Order)
	if err != nil {
		return Parent{}, dbNotFound("parent", role, err)
	}
	return parent, nil
}

func (s *Store) ParentsByRole(ctx context.Context, stateID, role string) ([]Parent, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT parent_state_id, role, parent_order
		 FROM state_parents WHERE state_id = ? AND role = ? ORDER BY parent_order`,
		stateID, role)
	if err != nil {
		return nil, fmt.Errorf("query state parents by role: %w", err)
	}
	defer rows.Close()
	var parents []Parent
	for rows.Next() {
		var parent Parent
		if err := rows.Scan(&parent.StateID, &parent.Role, &parent.Order); err != nil {
			return nil, fmt.Errorf("scan state parent: %w", err)
		}
		parents = append(parents, parent)
	}
	return parents, rows.Err()
}

func (s *Store) GetTask(ctx context.Context, id string) (Task, error) {
	return scanTask(s.db.QueryRowContext(ctx,
		`SELECT id, title, status, created_at FROM tasks WHERE id = ?`, id), id)
}

func (s *Store) ListTasks(ctx context.Context, status string) ([]Task, error) {
	query := `SELECT id, title, status, created_at FROM tasks`
	var args []any
	if status != "" {
		query += ` WHERE status = ?`
		args = append(args, status)
	}
	query += ` ORDER BY created_at, id`
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("query tasks: %w", err)
	}
	defer rows.Close()
	var tasks []Task
	for rows.Next() {
		task, err := scanTask(rows, "")
		if err != nil {
			return nil, err
		}
		tasks = append(tasks, task)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate tasks: %w", err)
	}
	return tasks, nil
}

func (s *Store) GetAttempt(ctx context.Context, id string) (Attempt, error) {
	attempt, err := scanAttempt(s.db.QueryRowContext(ctx,
		`SELECT `+attemptColumns+` FROM attempts WHERE id = ?`, id))
	if err != nil {
		return Attempt{}, dbNotFound("attempt", id, err)
	}
	return attempt, nil
}

// ListAttempts filters by task and status when either value is non-empty.
func (s *Store) ListAttempts(ctx context.Context, taskID, status string) ([]Attempt, error) {
	query := `SELECT ` + attemptColumns + ` FROM attempts WHERE 1 = 1`
	var args []any
	if taskID != "" {
		query += ` AND task_id = ?`
		args = append(args, taskID)
	}
	if status != "" {
		query += ` AND status = ?`
		args = append(args, status)
	}
	query += ` ORDER BY created_at, id`
	return s.queryAttempts(ctx, query, args...)
}

func (s *Store) ActiveAttempts(ctx context.Context) ([]Attempt, error) {
	return s.queryAttempts(ctx,
		`SELECT `+attemptColumns+` FROM attempts
		 WHERE status NOT IN ('accepted', 'completed', 'failed', 'cancelled', 'rejected')
		 ORDER BY created_at, id`)
}

func (s *Store) queryAttempts(ctx context.Context, query string, args ...any) ([]Attempt, error) {
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("query attempts: %w", err)
	}
	defer rows.Close()
	var attempts []Attempt
	for rows.Next() {
		attempt, err := scanAttempt(rows)
		if err != nil {
			return nil, err
		}
		attempts = append(attempts, attempt)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate attempts: %w", err)
	}
	return attempts, nil
}

func (s *Store) UpdateAttemptStatus(ctx context.Context, id, status string) error {
	if status == "" {
		return errors.New("hop: attempt status is required")
	}
	result, err := s.db.ExecContext(ctx, `UPDATE attempts SET status = ? WHERE id = ?`, status, id)
	if err != nil {
		return fmt.Errorf("update attempt status: %w", err)
	}
	return requireOneRow(result, "attempt", id)
}

// ParkAttempt atomically marks an unfinished attempt as compacted after its
// current head has been durably checkpointed. Agent-session pointers are kept
// so a later message can rehydrate and resume the exact attempt.
func (s *Store) ParkAttempt(ctx context.Context, id, expectedHeadID string) (bool, error) {
	if strings.TrimSpace(id) == "" || strings.TrimSpace(expectedHeadID) == "" {
		return false, errors.New("hop: attempt ID and expected head are required")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("begin attempt parking: %w", err)
	}
	defer tx.Rollback()
	attempt, err := attemptTx(ctx, tx, id)
	if err != nil {
		return false, err
	}
	if attempt.Status == "parked" && attempt.HeadStateID == expectedHeadID {
		return true, nil
	}
	if attempt.HeadStateID != expectedHeadID || isTerminalAttemptStatus(attempt.Status) {
		return false, nil
	}
	result, err := tx.ExecContext(ctx,
		`UPDATE attempts SET status = 'parked'
		 WHERE id = ? AND head_state_id = ?
		   AND status NOT IN ('accepted', 'completed', 'failed', 'cancelled', 'rejected', 'parked')`,
		attempt.ID, expectedHeadID)
	if err != nil {
		return false, fmt.Errorf("park attempt: %w", err)
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("inspect parked attempt: %w", err)
	}
	if changed != 1 {
		return false, nil
	}
	if err := insertEventTx(ctx, tx, Event{
		Kind: "attempt.parked", TaskID: attempt.TaskID, AttemptID: attempt.ID, StateID: expectedHeadID,
	}, map[string]string{"reason": "inactive workspace compacted"}); err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("commit attempt parking: %w", err)
	}
	return true, nil
}

// ReactivateParkedAttempt restores the mutable status after its exact head has
// been materialized back into the managed workspace.
func (s *Store) ReactivateParkedAttempt(ctx context.Context, id, expectedHeadID string) (bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("begin parked-attempt reactivation: %w", err)
	}
	defer tx.Rollback()
	attempt, err := attemptTx(ctx, tx, id)
	if err != nil {
		return false, err
	}
	if attempt.Status != "parked" || attempt.HeadStateID != expectedHeadID {
		return false, nil
	}
	result, err := tx.ExecContext(ctx,
		`UPDATE attempts SET status = 'active'
		 WHERE id = ? AND status = 'parked' AND head_state_id = ?`, attempt.ID, expectedHeadID)
	if err != nil {
		return false, fmt.Errorf("reactivate parked attempt: %w", err)
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("inspect parked-attempt reactivation: %w", err)
	}
	if changed != 1 {
		return false, nil
	}
	if err := insertEventTx(ctx, tx, Event{
		Kind: "attempt.reactivated", TaskID: attempt.TaskID, AttemptID: attempt.ID, StateID: expectedHeadID,
	}, nil); err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("commit parked-attempt reactivation: %w", err)
	}
	return true, nil
}

func isTerminalAttemptStatus(status string) bool {
	switch status {
	case "accepted", "completed", "failed", "cancelled", "rejected":
		return true
	default:
		return false
	}
}

// CompleteCleanAttempt atomically closes a source-clean, single-attempt task.
// The caller verifies the workspace tree first; the expected head prevents a
// concurrent follow-up from being closed by a racing completion.
func (s *Store) CompleteCleanAttempt(ctx context.Context, id, expectedHeadID string) (bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("begin clean attempt completion: %w", err)
	}
	defer tx.Rollback()
	attempt, err := attemptTx(ctx, tx, id)
	if err != nil {
		return false, err
	}
	if attempt.Status != "active" || attempt.HeadStateID != expectedHeadID {
		return false, nil
	}
	var competing int
	if err := tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM attempts
		 WHERE task_id = ? AND id != ?
		   AND status NOT IN ('accepted', 'completed', 'failed', 'cancelled', 'rejected')`,
		attempt.TaskID, attempt.ID).Scan(&competing); err != nil {
		return false, fmt.Errorf("count competing attempts: %w", err)
	}
	if competing != 0 {
		return false, nil
	}
	result, err := tx.ExecContext(ctx,
		`UPDATE attempts SET status = 'completed'
		 WHERE id = ? AND status = 'active' AND head_state_id = ?`,
		attempt.ID, expectedHeadID)
	if err != nil {
		return false, fmt.Errorf("complete clean attempt: %w", err)
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("inspect clean attempt completion: %w", err)
	}
	if changed != 1 {
		return false, nil
	}
	if _, err := tx.ExecContext(ctx, `UPDATE tasks SET status = 'completed' WHERE id = ?`, attempt.TaskID); err != nil {
		return false, fmt.Errorf("complete clean task: %w", err)
	}
	if err := insertEventTx(ctx, tx, Event{
		Kind: "attempt.completed", TaskID: attempt.TaskID, AttemptID: attempt.ID, StateID: expectedHeadID,
	}, map[string]string{"reason": "completed without source changes"}); err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("commit clean attempt completion: %w", err)
	}
	return true, nil
}

func (s *Store) UpdateTaskStatus(ctx context.Context, id, status string) error {
	if status == "" {
		return errors.New("hop: task status is required")
	}
	result, err := s.db.ExecContext(ctx, `UPDATE tasks SET status = ? WHERE id = ?`, status, id)
	if err != nil {
		return fmt.Errorf("update task status: %w", err)
	}
	return requireOneRow(result, "task", id)
}

// UpdateAttemptHead performs a standalone compare-and-swap. AppendState should
// normally be preferred because it cannot point at a state that was not created
// as part of the same logical operation.
func (s *Store) UpdateAttemptHead(ctx context.Context, attemptID, expectedHeadID, nextHeadID string) error {
	result, err := s.db.ExecContext(ctx,
		`UPDATE attempts SET head_state_id = ? WHERE id = ? AND head_state_id = ?`,
		nextHeadID, attemptID, expectedHeadID)
	if err != nil {
		return fmt.Errorf("advance attempt head: %w", err)
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("inspect attempt head update: %w", err)
	}
	if changed == 1 {
		return nil
	}
	attempt, getErr := s.GetAttempt(ctx, attemptID)
	if getErr != nil {
		return getErr
	}
	return &HeadChangedError{Scope: "attempt", Expected: expectedHeadID, Actual: attempt.HeadStateID}
}

// AddCheck records evidence bound to a tree. If StateID is present, TreeHash is
// filled from that state when empty and otherwise must match its source tree.
func (s *Store) AddCheck(ctx context.Context, check Check) (Check, error) {
	if check.ID == "" {
		check.ID = newID("check")
	}
	if check.AttemptID == "" {
		return Check{}, errors.New("hop: check attempt ID is required")
	}
	if check.CreatedAt.IsZero() {
		check.CreatedAt = time.Now().UTC()
	}
	check.Command, _ = redactSecretStrings(check.Command)
	check.Output, _ = RedactPromptSecrets(check.Output)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Check{}, fmt.Errorf("begin check creation: %w", err)
	}
	defer tx.Rollback()

	if _, err := attemptTx(ctx, tx, check.AttemptID); err != nil {
		return Check{}, err
	}
	if check.StateID != "" {
		state, err := stateTx(ctx, tx, check.StateID)
		if err != nil {
			return Check{}, err
		}
		if state.AttemptID != "" && state.AttemptID != check.AttemptID {
			return Check{}, errors.New("hop: check state belongs to another attempt")
		}
		if check.TreeHash == "" {
			check.TreeHash = state.SourceTree
		} else if check.TreeHash != state.SourceTree {
			return Check{}, errors.New("hop: check tree hash does not match state source tree")
		}
	}
	if check.TreeHash == "" {
		return Check{}, errors.New("hop: check tree hash is required")
	}
	command, err := json.Marshal(check.Command)
	if err != nil {
		return Check{}, fmt.Errorf("encode check command: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO checks(id, attempt_id, state_id, tree_hash, command_json, exit_code, output, created_at)
		 VALUES (?, ?, NULLIF(?, ''), ?, ?, ?, ?, ?)`,
		check.ID, check.AttemptID, check.StateID, check.TreeHash, string(command), check.ExitCode,
		check.Output, formatTime(check.CreatedAt)); err != nil {
		return Check{}, fmt.Errorf("insert check: %w", err)
	}
	attempt, err := attemptTx(ctx, tx, check.AttemptID)
	if err != nil {
		return Check{}, err
	}
	if err := insertEventTx(ctx, tx, Event{
		Kind: "check.recorded", TaskID: attempt.TaskID, AttemptID: check.AttemptID, StateID: check.StateID,
	}, map[string]any{"check_id": check.ID, "tree_hash": check.TreeHash, "exit_code": check.ExitCode}); err != nil {
		return Check{}, err
	}
	if err := tx.Commit(); err != nil {
		return Check{}, fmt.Errorf("commit check creation: %w", err)
	}
	return check, nil
}

// CreateCheck is a convenience alias for AddCheck.
func (s *Store) CreateCheck(ctx context.Context, check Check) (Check, error) {
	return s.AddCheck(ctx, check)
}

func (s *Store) GetCheck(ctx context.Context, id string) (Check, error) {
	check, err := scanCheck(s.db.QueryRowContext(ctx,
		`SELECT `+checkColumns+` FROM checks WHERE id = ?`, id))
	if err != nil {
		return Check{}, dbNotFound("check", id, err)
	}
	return check, nil
}

// ListChecks filters by attempt and/or exact source tree. Passing both empty
// lists all checks.
func (s *Store) ListChecks(ctx context.Context, attemptID, treeHash string) ([]Check, error) {
	query := `SELECT ` + checkColumns + ` FROM checks WHERE 1 = 1`
	var args []any
	if attemptID != "" {
		query += ` AND attempt_id = ?`
		args = append(args, attemptID)
	}
	if treeHash != "" {
		query += ` AND tree_hash = ?`
		args = append(args, treeHash)
	}
	query += ` ORDER BY created_at, id`
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("query checks: %w", err)
	}
	defer rows.Close()
	var checks []Check
	for rows.Next() {
		check, err := scanCheck(rows)
		if err != nil {
			return nil, err
		}
		checks = append(checks, check)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate checks: %w", err)
	}
	return checks, nil
}

func (s *Store) ChecksForTree(ctx context.Context, treeHash string) ([]Check, error) {
	return s.ListChecks(ctx, "", treeHash)
}

// Graph returns states in creation order with their typed parent edges. An
// empty taskID returns the repository-wide graph.
func (s *Store) Graph(ctx context.Context, taskID string) ([]GraphRow, error) {
	query := `SELECT ` + stateColumns + ` FROM states`
	var args []any
	if taskID != "" {
		query += ` WHERE task_id = ?`
		args = append(args, taskID)
	}
	query += ` ORDER BY created_at, id`
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("query state graph: %w", err)
	}
	var states []State
	for rows.Next() {
		state, err := scanState(rows)
		if err != nil {
			_ = rows.Close()
			return nil, err
		}
		states = append(states, state)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, fmt.Errorf("iterate state graph: %w", err)
	}
	if err := rows.Close(); err != nil {
		return nil, fmt.Errorf("close state graph rows: %w", err)
	}

	graph := make([]GraphRow, 0, len(states))
	for _, state := range states {
		parents, err := s.GetParents(ctx, state.ID)
		if err != nil {
			return nil, err
		}
		state.Parents = parents
		graph = append(graph, GraphRow{State: state, Parents: parents})
	}
	return graph, nil
}

// PutPromptCompletion durably records the answer for a prompt. Repeating the
// call updates that prompt in place so an interrupted final-delivery sequence
// can safely retry before the response is shown to the user.
func (s *Store) PutPromptCompletion(ctx context.Context, completion PromptCompletion) (PromptCompletion, error) {
	if strings.TrimSpace(completion.StateID) == "" {
		return PromptCompletion{}, errors.New("hop: prompt state ID is required")
	}
	if strings.TrimSpace(completion.Summary) == "" {
		return PromptCompletion{}, errors.New("hop: completion summary is required")
	}
	if strings.TrimSpace(completion.FinalResponse) == "" {
		return PromptCompletion{}, errors.New("hop: final response is required")
	}
	if completion.CompletedAt.IsZero() {
		completion.CompletedAt = time.Now().UTC()
	}
	for name, value := range map[string]string{"summary": completion.Summary, "final response": completion.FinalResponse} {
		if redacted, findings := RedactPromptSecrets(value); len(findings) > 0 || redacted != value {
			return PromptCompletion{}, fmt.Errorf("hop: refusing to persist an unredacted credential in completion %s", name)
		}
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return PromptCompletion{}, fmt.Errorf("begin prompt completion: %w", err)
	}
	defer tx.Rollback()
	state, err := stateTx(ctx, tx, completion.StateID)
	if err != nil {
		return PromptCompletion{}, err
	}
	if state.Kind != StatePrompt {
		return PromptCompletion{}, fmt.Errorf("state %s is %s, not a prompt", state.ID, state.Kind)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO prompt_completions(prompt_state_id, summary, final_response, completed_at)
		VALUES (?, ?, ?, ?)
		ON CONFLICT(prompt_state_id) DO UPDATE SET
			summary = excluded.summary,
			final_response = excluded.final_response,
			completed_at = excluded.completed_at`,
		completion.StateID, completion.Summary, completion.FinalResponse, formatTime(completion.CompletedAt)); err != nil {
		return PromptCompletion{}, fmt.Errorf("record prompt completion: %w", err)
	}
	if err := insertEventTx(ctx, tx, Event{
		Kind: "prompt.completed", TaskID: state.TaskID, AttemptID: state.AttemptID,
		StateID: state.ID, CreatedAt: completion.CompletedAt,
	}, map[string]string{"summary": completion.Summary}); err != nil {
		return PromptCompletion{}, err
	}
	if err := tx.Commit(); err != nil {
		return PromptCompletion{}, fmt.Errorf("commit prompt completion: %w", err)
	}
	return completion, nil
}

// PromptCompletions returns completion data keyed by prompt state ID.
func (s *Store) PromptCompletions(ctx context.Context) (map[string]PromptCompletion, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT prompt_state_id, summary, final_response, completed_at
		FROM prompt_completions ORDER BY completed_at, prompt_state_id`)
	if err != nil {
		return nil, fmt.Errorf("query prompt completions: %w", err)
	}
	defer rows.Close()
	result := make(map[string]PromptCompletion)
	for rows.Next() {
		var completion PromptCompletion
		var completedAt string
		if err := rows.Scan(&completion.StateID, &completion.Summary, &completion.FinalResponse, &completedAt); err != nil {
			return nil, fmt.Errorf("scan prompt completion: %w", err)
		}
		completion.CompletedAt, err = parseTime(completedAt)
		if err != nil {
			return nil, err
		}
		result[completion.StateID] = completion
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate prompt completions: %w", err)
	}
	return result, nil
}

// PromptSyncReceipts returns content-free acknowledgements for one private
// account/repository destination. The revision is a digest of the redacted
// wire record; no prompt text is duplicated in this retry metadata.
func (s *Store) PromptSyncReceipts(ctx context.Context, server, owner, name string) (map[string]PromptSyncReceipt, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT state_id, revision, deleted, synced_at
		FROM prompt_sync_receipts
		WHERE server = ? AND repository_owner = ? COLLATE NOCASE AND repository_name = ? COLLATE NOCASE`, server, owner, name)
	if err != nil {
		return nil, fmt.Errorf("query prompt sync receipts: %w", err)
	}
	defer rows.Close()
	result := make(map[string]PromptSyncReceipt)
	for rows.Next() {
		var receipt PromptSyncReceipt
		var deleted int
		var syncedAt string
		if err := rows.Scan(&receipt.StateID, &receipt.Revision, &deleted, &syncedAt); err != nil {
			return nil, fmt.Errorf("scan prompt sync receipt: %w", err)
		}
		receipt.Deleted = deleted != 0
		receipt.SyncedAt, err = parseTime(syncedAt)
		if err != nil {
			return nil, err
		}
		result[receipt.StateID] = receipt
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate prompt sync receipts: %w", err)
	}
	return result, nil
}

func (s *Store) RecordPromptSyncReceipts(ctx context.Context, server, owner, name string, receipts []PromptSyncReceipt) error {
	if len(receipts) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin prompt sync receipt update: %w", err)
	}
	defer tx.Rollback()
	for _, receipt := range receipts {
		when := receipt.SyncedAt
		if when.IsZero() {
			when = time.Now().UTC()
		}
		deleted := 0
		if receipt.Deleted {
			deleted = 1
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO prompt_sync_receipts
			(server, repository_owner, repository_name, state_id, revision, deleted, synced_at)
			VALUES (?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(server, repository_owner, repository_name, state_id)
			DO UPDATE SET revision = excluded.revision, deleted = excluded.deleted, synced_at = excluded.synced_at`,
			server, owner, name, receipt.StateID, receipt.Revision, deleted, formatTime(when)); err != nil {
			return fmt.Errorf("record prompt sync receipt %s: %w", receipt.StateID, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit prompt sync receipts: %w", err)
	}
	return nil
}

// AcceptedHistory follows canonical_parent edges from newest to oldest.
func (s *Store) AcceptedHistory(ctx context.Context, limit int) ([]State, error) {
	if limit <= 0 {
		limit = 1000
	}
	rows, err := s.db.QueryContext(ctx,
		`WITH RECURSIVE lineage(id, depth) AS (
			SELECT value, 0 FROM meta WHERE key = 'accepted_head'
			UNION ALL
			SELECT p.parent_state_id, lineage.depth + 1
			FROM lineage
			JOIN state_parents p ON p.state_id = lineage.id AND p.role = 'canonical_parent'
			WHERE lineage.depth < 9999
		)
		SELECT id FROM lineage ORDER BY depth LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("query accepted history: %w", err)
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("scan accepted history: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, fmt.Errorf("iterate accepted history: %w", err)
	}
	if err := rows.Close(); err != nil {
		return nil, fmt.Errorf("close accepted history rows: %w", err)
	}
	if len(ids) == 0 {
		return nil, ErrNotInitialized
	}
	history := make([]State, 0, len(ids))
	for _, id := range ids {
		state, err := s.GetState(ctx, id)
		if err != nil {
			return nil, err
		}
		history = append(history, state)
	}
	return history, nil
}

// History is a convenience alias for AcceptedHistory.
func (s *Store) History(ctx context.Context, limit int) ([]State, error) {
	return s.AcceptedHistory(ctx, limit)
}

func (s *Store) Status(ctx context.Context) (Status, error) {
	root, err := s.RepositoryRoot(ctx)
	if err != nil {
		return Status{}, err
	}
	head, err := s.AcceptedHead(ctx)
	if err != nil {
		return Status{}, err
	}
	attempts, err := s.ActiveAttempts(ctx)
	if err != nil {
		return Status{}, err
	}
	return Status{Root: root, AcceptedHead: head, Attempts: attempts}, nil
}

// PutPublication records the latest publishing outcome for an accepted state.
// The row is deliberately independent of the acceptance transaction: remote
// failure can never roll back or obscure the accepted state.
func (s *Store) PutPublication(ctx context.Context, publication PublicationStatus) error {
	if publication.AcceptedStateID == "" || publication.Commit == "" || publication.Status == "" {
		return errors.New("hop: publication state, commit, and status are required")
	}
	when := ""
	if publication.AttemptedAt != nil && !publication.AttemptedAt.IsZero() {
		when = formatTime(*publication.AttemptedAt)
	}
	retryable := 0
	if publication.Retryable {
		retryable = 1
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO publications(
			accepted_state_id, commit_oid, status, remote, ref, remote_tip,
			attempted_at, error_category, error_message, retryable
		 ) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(accepted_state_id) DO UPDATE SET
			commit_oid = excluded.commit_oid,
			status = excluded.status,
			remote = excluded.remote,
			ref = excluded.ref,
			remote_tip = excluded.remote_tip,
			attempted_at = excluded.attempted_at,
			error_category = excluded.error_category,
			error_message = excluded.error_message,
			retryable = excluded.retryable`,
		publication.AcceptedStateID, publication.Commit, publication.Status,
		publication.Remote, publication.Ref, publication.RemoteTip, when,
		publication.ErrorCategory, publication.ErrorMessage, retryable)
	if err != nil {
		return fmt.Errorf("record publication for %s: %w", publication.AcceptedStateID, err)
	}
	return nil
}

func (s *Store) PublicationForState(ctx context.Context, stateID string) (PublicationStatus, bool, error) {
	var publication PublicationStatus
	var attempted string
	var retryable int
	err := s.db.QueryRowContext(ctx,
		`SELECT accepted_state_id, commit_oid, status, remote, ref, remote_tip,
		        attempted_at, error_category, error_message, retryable
		 FROM publications WHERE accepted_state_id = ?`, stateID).Scan(
		&publication.AcceptedStateID, &publication.Commit, &publication.Status,
		&publication.Remote, &publication.Ref, &publication.RemoteTip, &attempted,
		&publication.ErrorCategory, &publication.ErrorMessage, &retryable)
	if errors.Is(err, sql.ErrNoRows) {
		return PublicationStatus{}, false, nil
	}
	if err != nil {
		return PublicationStatus{}, false, fmt.Errorf("read publication for %s: %w", stateID, err)
	}
	if attempted != "" {
		parsed, parseErr := parseTime(attempted)
		err = parseErr
		if err != nil {
			return PublicationStatus{}, false, fmt.Errorf("parse publication time for %s: %w", stateID, err)
		}
		publication.AttemptedAt = &parsed
	}
	publication.Retryable = retryable != 0
	return publication, true, nil
}

// AppendEvent adds an explicit audit event. Store transitions also add their
// own events in the same transactions as the state changes they describe.
func (s *Store) AppendEvent(ctx context.Context, event Event) (Event, error) {
	if event.Kind == "" {
		return Event{}, errors.New("hop: event kind is required")
	}
	if event.ID == "" {
		event.ID = newID("e")
	}
	if event.CreatedAt.IsZero() {
		event.CreatedAt = time.Now().UTC()
	}
	payload := event.Payload
	if len(payload) == 0 {
		payload = json.RawMessage(`{}`)
	}
	if !json.Valid(payload) {
		return Event{}, errors.New("hop: event payload is not valid JSON")
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO events(id, kind, task_id, attempt_id, state_id, payload_json, created_at)
		 VALUES (?, ?, NULLIF(?, ''), NULLIF(?, ''), NULLIF(?, ''), ?, ?)`,
		event.ID, event.Kind, event.TaskID, event.AttemptID, event.StateID, string(payload), formatTime(event.CreatedAt))
	if err != nil {
		return Event{}, fmt.Errorf("insert event: %w", err)
	}
	event.Payload = payload
	return event, nil
}

func (s *Store) ListEvents(ctx context.Context, limit int) ([]Event, error) {
	if limit <= 0 {
		limit = 1000
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, kind, COALESCE(task_id, ''), COALESCE(attempt_id, ''), COALESCE(state_id, ''), payload_json, created_at
		 FROM events ORDER BY created_at, id LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("query events: %w", err)
	}
	defer rows.Close()
	var events []Event
	for rows.Next() {
		var event Event
		var payload, created string
		if err := rows.Scan(&event.ID, &event.Kind, &event.TaskID, &event.AttemptID, &event.StateID, &payload, &created); err != nil {
			return nil, fmt.Errorf("scan event: %w", err)
		}
		createdAt, err := parseTime(created)
		if err != nil {
			return nil, err
		}
		event.CreatedAt = createdAt
		event.Payload = json.RawMessage(payload)
		events = append(events, event)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate events: %w", err)
	}
	return events, nil
}

const stateColumns = `id, kind, COALESCE(task_id, ''), COALESCE(attempt_id, ''),
	COALESCE(canonical_anchor_id, ''), source_tree, git_commit, prompt, summary, agent,
	provenance_json, digest, created_at`

const attemptColumns = `id, task_id, agent, workspace, base_state_id, COALESCE(head_state_id, ''), status, created_at`

const checkColumns = `id, attempt_id, COALESCE(state_id, ''), tree_hash, command_json, exit_code, output, created_at`

type scanner interface {
	Scan(dest ...any) error
}

func scanState(row scanner) (State, error) {
	var state State
	var kind, provenance, created string
	if err := row.Scan(
		&state.ID, &kind, &state.TaskID, &state.AttemptID, &state.CanonicalAnchorID,
		&state.SourceTree, &state.GitCommit, &state.Prompt, &state.Summary, &state.Agent, &provenance,
		&state.Digest, &created,
	); err != nil {
		return State{}, err
	}
	createdAt, err := parseTime(created)
	if err != nil {
		return State{}, err
	}
	state.Kind = StateKind(kind)
	if provenance != "" {
		state.Provenance = &StateProvenance{}
		if err := json.Unmarshal([]byte(provenance), state.Provenance); err != nil {
			return State{}, fmt.Errorf("decode state provenance: %w", err)
		}
	}
	state.CreatedAt = createdAt
	return state, nil
}

func scanTask(row scanner, requestedID string) (Task, error) {
	var task Task
	var created string
	if err := row.Scan(&task.ID, &task.Title, &task.Status, &created); err != nil {
		return Task{}, dbNotFound("task", requestedID, err)
	}
	createdAt, err := parseTime(created)
	if err != nil {
		return Task{}, err
	}
	task.CreatedAt = createdAt
	return task, nil
}

func scanAttempt(row scanner) (Attempt, error) {
	var attempt Attempt
	var created string
	if err := row.Scan(
		&attempt.ID, &attempt.TaskID, &attempt.Agent, &attempt.Workspace,
		&attempt.BaseStateID, &attempt.HeadStateID, &attempt.Status, &created,
	); err != nil {
		return Attempt{}, err
	}
	createdAt, err := parseTime(created)
	if err != nil {
		return Attempt{}, err
	}
	attempt.CreatedAt = createdAt
	return attempt, nil
}

func scanCheck(row scanner) (Check, error) {
	var check Check
	var command, created string
	if err := row.Scan(
		&check.ID, &check.AttemptID, &check.StateID, &check.TreeHash, &command,
		&check.ExitCode, &check.Output, &created,
	); err != nil {
		return Check{}, err
	}
	if err := json.Unmarshal([]byte(command), &check.Command); err != nil {
		return Check{}, fmt.Errorf("decode check command: %w", err)
	}
	createdAt, err := parseTime(created)
	if err != nil {
		return Check{}, err
	}
	check.CreatedAt = createdAt
	return check, nil
}

func stateTx(ctx context.Context, tx *sql.Tx, id string) (State, error) {
	state, err := scanState(tx.QueryRowContext(ctx,
		`SELECT `+stateColumns+` FROM states WHERE id = ?`, id))
	if err != nil {
		return State{}, dbNotFound("state", id, err)
	}
	return state, nil
}

func attemptTx(ctx context.Context, tx *sql.Tx, id string) (Attempt, error) {
	attempt, err := scanAttempt(tx.QueryRowContext(ctx,
		`SELECT `+attemptColumns+` FROM attempts WHERE id = ?`, id))
	if err != nil {
		return Attempt{}, dbNotFound("attempt", id, err)
	}
	return attempt, nil
}

func attemptHeadTx(ctx context.Context, tx *sql.Tx, id string) (string, error) {
	var head string
	err := tx.QueryRowContext(ctx,
		`SELECT COALESCE(head_state_id, '') FROM attempts WHERE id = ?`, id).Scan(&head)
	return head, err
}

func metaTx(ctx context.Context, tx *sql.Tx, key string) (string, error) {
	var value string
	err := tx.QueryRowContext(ctx, `SELECT value FROM meta WHERE key = ?`, key).Scan(&value)
	return value, err
}

func insertStateTx(ctx context.Context, tx *sql.Tx, state State, parents []Parent) error {
	if state.ID == "" {
		return errors.New("hop: state ID is required")
	}
	if state.Kind == "" {
		return errors.New("hop: state kind is required")
	}
	if state.CreatedAt.IsZero() {
		return errors.New("hop: state creation time is required")
	}
	for name, value := range map[string]string{
		"prompt":  state.Prompt,
		"summary": state.Summary,
		"agent":   state.Agent,
	} {
		if redacted, findings := RedactPromptSecrets(value); len(findings) > 0 || redacted != value {
			return fmt.Errorf("hop: refusing to persist an unredacted credential in state %s", name)
		}
	}
	provenance := ""
	if state.Provenance != nil {
		payload, err := json.Marshal(state.Provenance)
		if err != nil {
			return fmt.Errorf("encode state provenance: %w", err)
		}
		provenance = string(payload)
	}
	_, err := tx.ExecContext(ctx,
		`INSERT INTO states(
			id, kind, task_id, attempt_id, canonical_anchor_id, source_tree, git_commit,
			prompt, summary, agent, provenance_json, digest, created_at
		) VALUES (?, ?, NULLIF(?, ''), NULLIF(?, ''), NULLIF(?, ''), ?, ?, ?, ?, ?, ?, ?, ?)`,
		state.ID, string(state.Kind), state.TaskID, state.AttemptID, state.CanonicalAnchorID,
		state.SourceTree, state.GitCommit, state.Prompt, state.Summary, state.Agent, provenance,
		state.Digest, formatTime(state.CreatedAt))
	if err != nil {
		return fmt.Errorf("insert state %q: %w", state.ID, err)
	}
	for _, parent := range parents {
		if parent.StateID == "" {
			return errors.New("hop: parent state ID is required")
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO state_parents(state_id, parent_state_id, role, parent_order)
			 VALUES (?, ?, ?, ?)`, state.ID, parent.StateID, parent.Role, parent.Order); err != nil {
			return fmt.Errorf("insert parent for state %q: %w", state.ID, err)
		}
	}
	return nil
}

func insertEventTx(ctx context.Context, tx *sql.Tx, event Event, payload any) error {
	if event.ID == "" {
		event.ID = newID("e")
	}
	if event.CreatedAt.IsZero() {
		event.CreatedAt = time.Now().UTC()
	}
	var encoded []byte
	var err error
	if payload == nil {
		encoded = []byte(`{}`)
	} else {
		encoded, err = json.Marshal(payload)
		if err != nil {
			return fmt.Errorf("encode event payload: %w", err)
		}
	}
	if redacted, findings := RedactPromptSecrets(string(encoded)); len(findings) > 0 || redacted != string(encoded) {
		return fmt.Errorf("hop: refusing to persist an unredacted credential in event %q", event.Kind)
	}
	_, err = tx.ExecContext(ctx,
		`INSERT INTO events(id, kind, task_id, attempt_id, state_id, payload_json, created_at)
		 VALUES (?, ?, NULLIF(?, ''), NULLIF(?, ''), NULLIF(?, ''), ?, ?)`,
		event.ID, event.Kind, event.TaskID, event.AttemptID, event.StateID,
		string(encoded), formatTime(event.CreatedAt))
	if err != nil {
		return fmt.Errorf("insert event %q: %w", event.Kind, err)
	}
	return nil
}

func chooseParents(fromState, explicit []Parent) []Parent {
	if len(explicit) > 0 {
		return append([]Parent(nil), explicit...)
	}
	return append([]Parent(nil), fromState...)
}

func ensureParentRole(parents []Parent, role, stateID string) []Parent {
	if stateID == "" {
		return parents
	}
	for _, parent := range parents {
		if parent.Role == role {
			return parents
		}
	}
	return append(parents, Parent{StateID: stateID, Role: role, Order: len(parents)})
}

func parentWithRole(parents []Parent, role string) (Parent, bool) {
	for _, parent := range parents {
		if parent.Role == role {
			return parent, true
		}
	}
	return Parent{}, false
}

func canonicalizeParents(parents []Parent) []Parent {
	parents = append([]Parent(nil), parents...)
	used := make(map[int]bool, len(parents))
	for i := range parents {
		if parents[i].Role == "" {
			parents[i].Role = "parent"
		}
		order := parents[i].Order
		if order < 0 || used[order] {
			order = 0
			for used[order] {
				order++
			}
			parents[i].Order = order
		}
		used[order] = true
	}
	return parents
}

func stateIDPrefix(kind StateKind) string {
	switch kind {
	case StatePrompt:
		return "p"
	case StateCheckpoint:
		return "c"
	case StateProposal:
		return "r"
	case StateAccepted:
		return "a"
	case StateFailed:
		return "f"
	case StateCancelled:
		return "x"
	default:
		return "s"
	}
}

func formatTime(value time.Time) string {
	return value.UTC().Format(time.RFC3339Nano)
}

func parseTime(value string) (time.Time, error) {
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return time.Time{}, fmt.Errorf("decode database timestamp %q: %w", value, err)
	}
	return parsed, nil
}

func dbNotFound(kind, id string, err error) error {
	if errors.Is(err, sql.ErrNoRows) {
		if id == "" {
			return fmt.Errorf("%w: %s", ErrNotFound, kind)
		}
		return fmt.Errorf("%w: %s %q", ErrNotFound, kind, id)
	}
	return fmt.Errorf("read %s %q: %w", kind, id, err)
}

func requireOneRow(result sql.Result, kind, id string) error {
	changed, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("inspect %s update: %w", kind, err)
	}
	if changed != 1 {
		return fmt.Errorf("%w: %s %q", ErrNotFound, kind, id)
	}
	return nil
}
