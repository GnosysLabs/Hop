package hop

import (
	"fmt"
	"time"
)

type StateKind string

const (
	StatePrompt     StateKind = "prompt"
	StateCheckpoint StateKind = "checkpoint"
	StateProposal   StateKind = "proposal"
	StateAccepted   StateKind = "accepted"
	StateFailed     StateKind = "failed"
	StateCancelled  StateKind = "cancelled"
)

type Parent struct {
	StateID string `json:"state_id"`
	Role    string `json:"role"`
	Order   int    `json:"order"`
}

type State struct {
	ID                string    `json:"id"`
	Kind              StateKind `json:"kind"`
	TaskID            string    `json:"task_id,omitempty"`
	AttemptID         string    `json:"attempt_id,omitempty"`
	CanonicalAnchorID string    `json:"canonical_anchor_id,omitempty"`
	SourceTree        string    `json:"source_tree"`
	GitCommit         string    `json:"git_commit"`
	Prompt            string    `json:"prompt,omitempty"`
	Summary           string    `json:"summary,omitempty"`
	Agent             string    `json:"agent,omitempty"`
	Digest            string    `json:"digest"`
	CreatedAt         time.Time `json:"created_at"`
	Parents           []Parent  `json:"parents,omitempty"`
}

type Task struct {
	ID        string    `json:"id"`
	Title     string    `json:"title"`
	Status    string    `json:"status"`
	CreatedAt time.Time `json:"created_at"`
}

type Attempt struct {
	ID          string    `json:"id"`
	TaskID      string    `json:"task_id"`
	Agent       string    `json:"agent,omitempty"`
	Workspace   string    `json:"workspace"`
	BaseStateID string    `json:"base_state_id"`
	HeadStateID string    `json:"head_state_id"`
	Status      string    `json:"status"`
	CreatedAt   time.Time `json:"created_at"`
}

type Check struct {
	ID        string    `json:"id"`
	AttemptID string    `json:"attempt_id"`
	StateID   string    `json:"state_id,omitempty"`
	TreeHash  string    `json:"tree_hash"`
	Command   []string  `json:"command"`
	ExitCode  int       `json:"exit_code"`
	Output    string    `json:"output"`
	CreatedAt time.Time `json:"created_at"`
}

type GraphRow struct {
	State   State    `json:"state"`
	Parents []Parent `json:"parents"`
}

type Status struct {
	Root         string    `json:"root"`
	AcceptedHead State     `json:"accepted_head"`
	Attempts     []Attempt `json:"attempts"`
	RootStatus   string    `json:"root_status"`
	RootStateID  string    `json:"root_state_id,omitempty"`
}

type AcceptResult struct {
	State             State             `json:"state"`
	CapturedRoot      *State            `json:"captured_root,omitempty"`
	CapturedRootPaths []string          `json:"captured_root_paths,omitempty"`
	ProposalPaths     []string          `json:"proposal_paths"`
	CurrentPaths      []string          `json:"current_paths"`
	Check             *Check            `json:"check,omitempty"`
	MaterializedRoot  string            `json:"materialized_root,omitempty"`
	RemotePush        *RemotePushResult `json:"remote_push,omitempty"`
	PromptSync        *PromptSyncResult `json:"prompt_sync,omitempty"`
	Warnings          []string          `json:"warnings,omitempty"`
}

type RemotePushResult struct {
	Remote string `json:"remote"`
	Ref    string `json:"ref"`
	Commit string `json:"commit"`
}

type SyncResult struct {
	State      State             `json:"state"`
	Root       string            `json:"root"`
	FromState  string            `json:"from_state"`
	Changed    bool              `json:"changed"`
	PromptSync *PromptSyncResult `json:"prompt_sync,omitempty"`
	Warnings   []string          `json:"warnings,omitempty"`
}

type RefreshResult struct {
	Prompt       State    `json:"prompt"`
	Task         Task     `json:"task"`
	Attempt      Attempt  `json:"attempt"`
	Proposal     State    `json:"proposal"`
	AcceptedHead State    `json:"accepted_head"`
	RemoteTip    string   `json:"remote_tip,omitempty"`
	Workspace    string   `json:"workspace"`
	Deliver      []string `json:"deliver"`
	ConflictTree string   `json:"conflict_tree"`
	Conflicts    []string `json:"conflicts"`
	Reused       bool     `json:"reused"`
}

type PromptRedaction struct {
	Kind  string `json:"kind"`
	Count int    `json:"count"`
}

type PromptResult struct {
	Prompt     State             `json:"prompt"`
	Checkpoint *State            `json:"checkpoint,omitempty"`
	Task       Task              `json:"task"`
	Attempt    Attempt           `json:"attempt"`
	Workspace  string            `json:"workspace"`
	Deliver    []string          `json:"deliver"`
	Redactions []PromptRedaction `json:"redactions,omitempty"`
}

type BeginResult struct {
	PromptResult
	Initialized bool                    `json:"initialized"`
	SessionID   string                  `json:"session_id,omitempty"`
	Cleanup     *WorkspaceCleanupResult `json:"cleanup,omitempty"`
	Warnings    []string                `json:"warnings,omitempty"`
}

type EnvironmentResult struct {
	State     State             `json:"state"`
	Attempt   Attempt           `json:"attempt"`
	Workspace string            `json:"workspace"`
	Variables map[string]string `json:"variables"`
}

type ProposalResult struct {
	Proposal   State             `json:"proposal"`
	Checks     []Check           `json:"checks"`
	PromptSync *PromptSyncResult `json:"prompt_sync,omitempty"`
	Warnings   []string          `json:"warnings,omitempty"`
}

// PromptCompletion records the user-visible answer produced for one prompt.
// It is separate from the immutable Git state graph because read-only and
// external-operation turns can complete without creating a proposal.
type PromptCompletion struct {
	StateID       string    `json:"state_id"`
	Summary       string    `json:"summary"`
	FinalResponse string    `json:"final_response"`
	CompletedAt   time.Time `json:"completed_at"`
}

type CompletionResult struct {
	Completion PromptCompletion        `json:"completion"`
	PromptSync *PromptSyncResult       `json:"prompt_sync,omitempty"`
	Cleanup    *WorkspaceCleanupResult `json:"cleanup,omitempty"`
	Warnings   []string                `json:"warnings,omitempty"`
}

type WorkspaceCleanupIssue struct {
	AttemptID string `json:"attempt_id"`
	Workspace string `json:"workspace"`
	Reason    string `json:"reason"`
}

type WorkspaceCleanupResult struct {
	Scanned          int                     `json:"scanned"`
	AbandonedScanned int                     `json:"abandoned_scanned"`
	Parked           int                     `json:"parked"`
	Removed          int                     `json:"removed"`
	ReclaimedBytes   int64                   `json:"reclaimed_bytes"`
	Preserved        []WorkspaceCleanupIssue `json:"preserved,omitempty"`
}

type PromptRepository struct {
	Owner string `json:"owner"`
	Name  string `json:"name"`
}

type PromptSyncResult struct {
	Server     string           `json:"server"`
	Repository PromptRepository `json:"repository"`
	Synced     int              `json:"synced"`
	Deleted    int              `json:"deleted,omitempty"`
}

type PromptSyncReceipt struct {
	StateID  string    `json:"state_id"`
	Revision string    `json:"revision"`
	Deleted  bool      `json:"deleted,omitempty"`
	SyncedAt time.Time `json:"synced_at"`
}

type DoctorReport struct {
	OK             bool     `json:"ok"`
	AcceptedState  string   `json:"accepted_state"`
	AcceptedCommit string   `json:"accepted_commit"`
	RefCommit      string   `json:"ref_commit,omitempty"`
	Repaired       bool     `json:"repaired"`
	Problems       []string `json:"problems,omitempty"`
}

type StaleHeadError struct {
	Expected string
	Actual   string
}

func (e *StaleHeadError) Error() string {
	return "accepted head changed while the operation was running"
}

type CheckFailedError struct {
	Check Check
}

func (e *CheckFailedError) Error() string {
	return "validation command failed"
}

// CommittedStateError means the authoritative SQLite transition succeeded but
// a derived ref or visible-root synchronization needs repair. Callers must not
// retry the state transition.
type CommittedStateError struct {
	State State
	Err   error
}

func (e *CommittedStateError) Error() string {
	return fmt.Sprintf("state %s is committed, but post-acceptance synchronization needs repair: %v", e.State.ID, e.Err)
}

func (e *CommittedStateError) Unwrap() error { return e.Err }

type ConflictError struct {
	Paths     []string `json:"paths"`
	RemoteTip string   `json:"remote_tip,omitempty"`
}

func (e *ConflictError) Error() string {
	if e.RemoteTip != "" {
		return "automatic merge with the remote branch has genuine unresolved conflicts"
	}
	return "automatic three-way merge has genuine unresolved conflicts"
}

type RootConflictError struct {
	Paths  []string
	Reason string
}

func (e *RootConflictError) Error() string {
	if e.Reason != "" {
		return e.Reason
	}
	return "visible project root has out-of-band changes; refusing to overwrite it"
}
