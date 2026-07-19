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
	ID                string           `json:"id"`
	Kind              StateKind        `json:"kind"`
	TaskID            string           `json:"task_id,omitempty"`
	AttemptID         string           `json:"attempt_id,omitempty"`
	CanonicalAnchorID string           `json:"canonical_anchor_id,omitempty"`
	SourceTree        string           `json:"source_tree"`
	GitCommit         string           `json:"git_commit"`
	Prompt            string           `json:"prompt,omitempty"`
	Summary           string           `json:"summary,omitempty"`
	Agent             string           `json:"agent,omitempty"`
	Provenance        *StateProvenance `json:"provenance,omitempty"`
	Digest            string           `json:"digest"`
	CreatedAt         time.Time        `json:"created_at"`
	Parents           []Parent         `json:"parents,omitempty"`
}

// StateProvenance is the durable authorization proof for a tree-producing
// state. It binds the exact base and candidate trees, every authorized input,
// and the exact object/mode delta that was observed.
type StateProvenance struct {
	Version           int               `json:"version"`
	Operation         string            `json:"operation"`
	BaseStateID       string            `json:"base_state_id,omitempty"`
	BaseTree          string            `json:"base_tree"`
	CandidateTree     string            `json:"candidate_tree"`
	Inputs            []ProvenanceInput `json:"inputs,omitempty"`
	Manifest          []TreeDelta       `json:"manifest,omitempty"`
	ManifestDigest    string            `json:"manifest_digest"`
	CompositionDigest string            `json:"composition_digest"`
}

type ProvenanceInput struct {
	Role          string `json:"role"`
	StateID       string `json:"state_id,omitempty"`
	BaseTree      string `json:"base_tree"`
	CandidateTree string `json:"candidate_tree"`
}

type TreeDelta struct {
	Status  string `json:"status"`
	OldPath string `json:"old_path,omitempty"`
	NewPath string `json:"new_path,omitempty"`
	OldMode string `json:"old_mode,omitempty"`
	NewMode string `json:"new_mode,omitempty"`
	OldOID  string `json:"old_oid,omitempty"`
	NewOID  string `json:"new_oid,omitempty"`
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
	Root               string            `json:"root"`
	AcceptedHead       State             `json:"accepted_head"`
	Attempts           []Attempt         `json:"attempts"`
	RootStatus         string            `json:"root_status"`
	RootStateID        string            `json:"root_state_id,omitempty"`
	AcceptedProvenance string            `json:"accepted_provenance"`
	Git                GitStatus         `json:"git"`
	Publication        PublicationStatus `json:"publication"`
	Warnings           []string          `json:"warnings,omitempty"`
}

// GitStatus separates Hop's accepted-tree projection from the user's actual
// branch, index, and worktree state. A projected root can look dirty to raw Git
// solely because Hop intentionally does not move HEAD or the real index.
type GitStatus struct {
	Branch                        string   `json:"branch,omitempty"`
	LocalTip                      string   `json:"local_tip,omitempty"`
	AcceptedTip                   string   `json:"accepted_tip"`
	LocalAhead                    int      `json:"local_ahead"`
	LocalBehind                   int      `json:"local_behind"`
	UpstreamRef                   string   `json:"upstream_ref,omitempty"`
	UpstreamTip                   string   `json:"upstream_tip,omitempty"`
	LocalTrackingTip              string   `json:"local_tracking_tip,omitempty"`
	LocalTrackingRefMayBeStale    bool     `json:"local_tracking_ref_may_be_stale"`
	UpstreamObservation           string   `json:"upstream_observation,omitempty"`
	UpstreamObservationMayBeStale bool     `json:"upstream_observation_may_be_stale"`
	AcceptedAheadUpstream         int      `json:"accepted_ahead_upstream"`
	AcceptedBehindUpstream        int      `json:"accepted_behind_upstream"`
	ProjectionOverStaleRef        bool     `json:"projection_over_stale_ref"`
	ProjectionOnlyChanges         bool     `json:"projection_only_changes"`
	UserWorktreeChanged           bool     `json:"user_worktree_changed"`
	UserWorktreePaths             []string `json:"user_worktree_paths,omitempty"`
	UserIndexChanged              bool     `json:"user_index_changed"`
	UserIndexPaths                []string `json:"user_index_paths,omitempty"`
}

// PublicationStatus is the durable outcome of publishing one accepted state.
// Failures never roll back acceptance and remain visible until a retry succeeds.
type PublicationStatus struct {
	AcceptedStateID string     `json:"accepted_state_id"`
	Commit          string     `json:"commit"`
	Status          string     `json:"status"`
	Remote          string     `json:"remote,omitempty"`
	Ref             string     `json:"ref,omitempty"`
	RemoteTip       string     `json:"remote_tip,omitempty"`
	AttemptedAt     *time.Time `json:"attempted_at,omitempty"`
	ErrorCategory   string     `json:"error_category,omitempty"`
	ErrorMessage    string     `json:"error_message,omitempty"`
	Retryable       bool       `json:"retryable"`
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
	Warnings       []string `json:"warnings,omitempty"`
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

type RemoteDivergedError struct {
	RemoteTip   string `json:"remote_tip"`
	AcceptedTip string `json:"accepted_tip"`
}

func (e *RemoteDivergedError) Error() string {
	return "remote branch has diverged from the accepted state; reconcile it before retrying publication"
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

type ProvenanceError struct {
	Operation string   `json:"operation,omitempty"`
	Paths     []string `json:"paths,omitempty"`
	Reason    string   `json:"reason"`
}

func (e *ProvenanceError) Error() string {
	if len(e.Paths) > 0 {
		return fmt.Sprintf("hop: %s provenance verification failed for %v: %s", e.Operation, e.Paths, e.Reason)
	}
	return fmt.Sprintf("hop: %s provenance verification failed: %s", e.Operation, e.Reason)
}

func (e *RootConflictError) Error() string {
	if e.Reason != "" {
		return e.Reason
	}
	return "visible project root has out-of-band changes; refusing to overwrite it"
}
