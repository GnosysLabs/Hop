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
	State            State    `json:"state"`
	ProposalPaths    []string `json:"proposal_paths"`
	CurrentPaths     []string `json:"current_paths"`
	Check            *Check   `json:"check,omitempty"`
	MaterializedRoot string   `json:"materialized_root,omitempty"`
	Warnings         []string `json:"warnings,omitempty"`
}

type SyncResult struct {
	State     State  `json:"state"`
	Root      string `json:"root"`
	FromState string `json:"from_state"`
	Changed   bool   `json:"changed"`
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
	Initialized bool   `json:"initialized"`
	SessionID   string `json:"session_id,omitempty"`
}

type EnvironmentResult struct {
	State     State             `json:"state"`
	Attempt   Attempt           `json:"attempt"`
	Workspace string            `json:"workspace"`
	Variables map[string]string `json:"variables"`
}

type ProposalResult struct {
	Proposal State   `json:"proposal"`
	Checks   []Check `json:"checks"`
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
	Paths []string
}

func (e *ConflictError) Error() string {
	return "proposal overlaps changes accepted since its canonical anchor"
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
