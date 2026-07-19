package hop

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const (
	acceptedRef                 = "accepted"
	reconciliationSummaryPrefix = "hop-reconciliation-conflicts:"
	DefaultAbandonedAfter       = 24 * time.Hour
)

type WorkspaceCleanupOptions struct {
	IncludeAbandoned     bool
	AbandonedAfter       time.Duration
	ExcludeAttemptID     string
	ArchiveDirtyTerminal bool
	Now                  time.Time
}

type Service struct {
	Root  string
	Store *Store
	Repo  *Repository
}

func InitProject(ctx context.Context, path string) (*Service, State, error) {
	repo, err := EnsureRepository(path)
	if err != nil {
		return nil, State{}, err
	}
	root := repo.Root()
	trackedHopPaths, err := repo.TrackedPaths(ctx, ".hop")
	if err != nil {
		return nil, State{}, err
	}
	for _, trackedPath := range trackedHopPaths {
		return nil, State{}, fmt.Errorf("cannot initialize Hop: private .hop path is already tracked (for example %s)", trackedPath)
	}
	hopDir := filepath.Join(root, ".hop")
	if err := os.MkdirAll(filepath.Join(hopDir, "workspaces"), 0o755); err != nil {
		return nil, State{}, fmt.Errorf("create Hop project directory: %w", err)
	}
	releaseInit, err := acquireProjectLock(ctx, root, "init")
	if err != nil {
		return nil, State{}, err
	}
	defer releaseInit()
	store, err := OpenStore(filepath.Join(hopDir, "hop.db"))
	if err != nil {
		return nil, State{}, err
	}
	service := &Service{Root: root, Store: store, Repo: repo}
	initialized, err := store.IsInitialized(ctx)
	if err != nil {
		service.Close()
		return nil, State{}, err
	}
	if initialized {
		head, err := store.AcceptedHead(ctx)
		if err != nil {
			service.Close()
			return nil, State{}, err
		}
		// Derived-ref repair acquires accept.lock. Release init.lock first so a
		// validation child opening this project cannot invert those locks.
		releaseInit()
		if err := service.reconcileDerivedRefs(ctx, true); err != nil {
			service.Close()
			return nil, State{}, err
		}
		return service, head, nil
	}

	commit, tree, err := repo.Snapshot(ctx, "hop: initial project state\n")
	if err != nil {
		service.Close()
		return nil, State{}, err
	}
	initial := State{
		ID:         newID("a"),
		Kind:       StateAccepted,
		SourceTree: tree,
		GitCommit:  commit,
		Summary:    "Initial project state",
		Agent:      "hop",
		CreatedAt:  time.Now().UTC(),
	}
	initial.Digest, err = digestState(initial, nil)
	if err != nil {
		service.Close()
		return nil, State{}, err
	}
	if err := service.pinState(ctx, initial); err != nil {
		service.Close()
		return nil, State{}, err
	}
	initial, err = store.CreateInitialState(ctx, root, initial)
	if err != nil {
		service.Close()
		return nil, State{}, err
	}
	if err := repo.UpdateHiddenRef(ctx, acceptedRef, commit); err != nil {
		service.Close()
		return nil, State{}, err
	}
	return service, initial, nil
}

func OpenProject(start string) (*Service, error) {
	root, err := FindHopRoot(start)
	if err != nil {
		return nil, err
	}
	releaseInit, err := acquireProjectLock(context.Background(), root, "init")
	if err != nil {
		return nil, err
	}
	defer releaseInit()
	store, err := OpenStore(filepath.Join(root, ".hop", "hop.db"))
	if err != nil {
		return nil, err
	}
	repo, err := OpenRepository(root)
	if err != nil {
		store.Close()
		return nil, err
	}
	recordedRoot, err := store.RepositoryRoot(context.Background())
	if err != nil {
		store.Close()
		return nil, err
	}
	recordedInfo, recordedErr := os.Stat(recordedRoot)
	rootInfo, rootErr := os.Stat(root)
	if recordedErr != nil || rootErr != nil {
		store.Close()
		if recordedErr != nil {
			return nil, fmt.Errorf("inspect recorded Hop root: %w", recordedErr)
		}
		return nil, fmt.Errorf("inspect discovered Hop root: %w", rootErr)
	}
	if !os.SameFile(recordedInfo, rootInfo) {
		store.Close()
		return nil, fmt.Errorf("Hop database belongs to %s, not %s", recordedRoot, root)
	}
	service := &Service{Root: root, Store: store, Repo: repo}
	reconcileAccepted := os.Getenv("HOP_ACCEPTANCE_LOCK_HELD") != "1"
	// Never hold init.lock while derived-ref repair acquires accept.lock.
	releaseInit()
	if err := service.reconcileDerivedRefs(context.Background(), reconcileAccepted); err != nil {
		service.Close()
		return nil, err
	}
	return service, nil
}

func (s *Service) Close() error {
	if s == nil || s.Store == nil {
		return nil
	}
	return s.Store.Close()
}

func (s *Service) CreatePrompt(ctx context.Context, message, fromStateID, agent string) (PromptResult, error) {
	if strings.TrimSpace(message) == "" {
		return PromptResult{}, fmt.Errorf("prompt text is required")
	}
	message, redactions := RedactPromptSecrets(message)
	agent, _ = RedactPromptSecrets(agent)
	var result PromptResult
	var err error
	if fromStateID == "" {
		result, err = s.createInitialPrompt(ctx, message, agent)
	} else {
		result, err = s.createFollowupPrompt(ctx, message, fromStateID, agent)
	}
	if err != nil {
		return PromptResult{}, err
	}
	result.Redactions = redactions
	s.schedulePromptCloudSync()
	return result, nil
}

// BeginPrompt is the interactive-agent entry point. It resolves follow-up
// ancestry from a stable harness session ID, creates the prompt state before
// project effects, and advances the session pointer for the next turn.
func (s *Service) BeginPrompt(ctx context.Context, message, fromStateID, agent, sessionID string) (PromptResult, error) {
	if strings.TrimSpace(agent) == "" {
		agent = "agent"
	}
	if redactedAgent, findings := RedactPromptSecrets(agent); len(findings) > 0 || redactedAgent != agent {
		return PromptResult{}, errors.New("hop: refusing to use a potential credential as an agent name")
	}
	if redactedSession, findings := RedactPromptSecrets(sessionID); len(findings) > 0 || redactedSession != sessionID {
		return PromptResult{}, errors.New("hop: refusing to use a potential credential as an agent session ID")
	}
	// Serialize interactive prompt capture with other begins. Acceptance checks
	// the proposal's attempt head again inside its SQLite transaction, so a begin
	// racing a land makes that proposal stale without sharing accept.lock.
	release, err := acquireProjectLock(ctx, s.Root, "prompt")
	if err != nil {
		return PromptResult{}, err
	}
	defer release()

	if fromStateID == "" && sessionID != "" {
		if stateID, exists, err := s.Store.AgentSessionHead(ctx, agent, sessionID); err != nil {
			return PromptResult{}, err
		} else if exists {
			continuable, err := s.sessionStateContinuable(ctx, stateID)
			if err != nil {
				return PromptResult{}, err
			}
			if continuable {
				fromStateID = stateID
			}
		}
	}
	result, err := s.CreatePrompt(ctx, message, fromStateID, agent)
	if err != nil {
		return PromptResult{}, err
	}
	if sessionID != "" {
		if err := s.Store.SetAgentSessionHead(ctx, agent, sessionID, result.Prompt.ID); err != nil {
			return PromptResult{}, err
		}
	}
	return result, nil
}

// sessionStateContinuable decides whether an implicit interactive-session
// pointer still represents unfinished work. An immutable accepted state for
// the task ends ordinary session continuation even if an older Hop build later
// changed the task's mutable status back to active. A dirty post-proposal
// workspace is preserved by continuing it instead of silently abandoning work.
func (s *Service) sessionStateContinuable(ctx context.Context, stateID string) (bool, error) {
	state, err := s.Store.GetState(ctx, stateID)
	if err != nil {
		return false, err
	}
	if state.TaskID == "" || state.AttemptID == "" {
		return false, fmt.Errorf("session state %s does not belong to a task attempt", state.ID)
	}
	attempt, err := s.Store.GetAttempt(ctx, state.AttemptID)
	if err != nil {
		return false, err
	}
	switch attempt.Status {
	case "completed", "failed", "cancelled", "rejected":
		return false, nil
	}
	accepted, exists, err := s.Store.AcceptedForTask(ctx, state.TaskID)
	if err != nil {
		return false, err
	}
	if !exists {
		return true, nil
	}
	head, err := s.Store.GetState(ctx, attempt.HeadStateID)
	if err != nil {
		return false, err
	}
	covered, err := s.Store.StateDescendsFrom(ctx, accepted.ID, head.ID)
	if err != nil {
		return false, err
	}
	if !covered {
		return true, nil
	}
	if _, err := os.Stat(attempt.Workspace); errors.Is(err, os.ErrNotExist) {
		// v1.0.5 could reclaim an accepted workspace without clearing its
		// session pointer. The accepted state already covers the attempt head,
		// so rolling the next prompt onto current accepted state is safe.
		return false, nil
	} else if err != nil {
		return false, fmt.Errorf("inspect attempt workspace: %w", err)
	}
	workspaceRepo, err := OpenRepository(attempt.Workspace)
	if err != nil {
		return false, err
	}
	workspaceTree, err := workspaceRepo.WorktreeTree(ctx, head.SourceTree)
	if err != nil {
		return false, err
	}
	if workspaceTree != head.SourceTree {
		return true, nil
	}

	status := "completed"
	if attempt.ID == accepted.AttemptID {
		status = "accepted"
	}
	if attempt.Status != status {
		if err := s.Store.UpdateAttemptStatus(ctx, attempt.ID, status); err != nil {
			return false, err
		}
	}
	if err := s.Store.UpdateTaskStatus(ctx, state.TaskID, "accepted"); err != nil {
		return false, err
	}
	return false, nil
}

func (s *Service) createInitialPrompt(ctx context.Context, message, agent string) (PromptResult, error) {
	accepted, err := s.Store.AcceptedHead(ctx)
	if err != nil {
		return PromptResult{}, err
	}
	now := time.Now().UTC()
	task := Task{ID: newID("t"), Title: promptTitle(message), Status: "active", CreatedAt: now}
	attemptID := newID("at")
	workspace := filepath.Join(s.Root, ".hop", "workspaces", attemptID)
	attempt := Attempt{
		ID:          attemptID,
		TaskID:      task.ID,
		Agent:       agent,
		Workspace:   workspace,
		BaseStateID: accepted.ID,
		Status:      "active",
		CreatedAt:   now,
	}
	parents := canonicalizeParents([]Parent{
		{StateID: accepted.ID, Role: "run_parent", Order: 0},
		{StateID: accepted.ID, Role: "canonical_anchor", Order: 1},
	})
	prompt := State{
		ID:                newID("p"),
		Kind:              StatePrompt,
		TaskID:            task.ID,
		AttemptID:         attempt.ID,
		CanonicalAnchorID: accepted.ID,
		SourceTree:        accepted.SourceTree,
		GitCommit:         accepted.GitCommit,
		Prompt:            message,
		Agent:             agent,
		CreatedAt:         now,
		Parents:           parents,
	}
	prompt.Digest, err = digestState(prompt, parents)
	if err != nil {
		return PromptResult{}, err
	}
	if err := s.pinState(ctx, prompt); err != nil {
		return PromptResult{}, err
	}
	task, attempt, prompt, err = s.Store.CreateTaskAttemptPrompt(ctx, task, attempt, prompt, parents)
	if err != nil {
		return PromptResult{}, err
	}
	if err := os.MkdirAll(filepath.Dir(workspace), 0o755); err != nil {
		_ = s.Store.UpdateAttemptStatus(ctx, attempt.ID, "failed")
		return PromptResult{}, err
	}
	if _, err := s.Repo.AddDetachedWorktree(ctx, workspace, accepted.GitCommit); err != nil {
		_ = s.Store.UpdateAttemptStatus(ctx, attempt.ID, "failed")
		return PromptResult{}, err
	}
	return PromptResult{
		Prompt:    prompt,
		Task:      task,
		Attempt:   attempt,
		Workspace: workspace,
		Deliver:   s.deliveryEnvironment(prompt, attempt),
	}, nil
}

func (s *Service) createFollowupPrompt(ctx context.Context, message, fromStateID, agent string) (PromptResult, error) {
	from, err := s.Store.GetState(ctx, fromStateID)
	if err != nil {
		return PromptResult{}, err
	}
	if from.AttemptID == "" {
		return PromptResult{}, fmt.Errorf("state %s does not belong to an attempt", from.ID)
	}
	attempt, err := s.Store.GetAttempt(ctx, from.AttemptID)
	if err != nil {
		return PromptResult{}, err
	}
	task, err := s.Store.GetTask(ctx, attempt.TaskID)
	if err != nil {
		return PromptResult{}, err
	}
	checkpoint, err := s.checkpointAttempt(ctx, attempt)
	if err != nil {
		return PromptResult{}, err
	}
	accepted, err := s.Store.AcceptedHead(ctx)
	if err != nil {
		return PromptResult{}, err
	}
	if agent == "" {
		agent = attempt.Agent
	}
	parents := canonicalizeParents([]Parent{
		{StateID: checkpoint.ID, Role: "run_parent", Order: 0},
		{StateID: accepted.ID, Role: "canonical_anchor", Order: 1},
	})
	prompt := State{
		ID:                newID("p"),
		Kind:              StatePrompt,
		TaskID:            attempt.TaskID,
		AttemptID:         attempt.ID,
		CanonicalAnchorID: accepted.ID,
		SourceTree:        checkpoint.SourceTree,
		GitCommit:         checkpoint.GitCommit,
		Prompt:            message,
		Agent:             agent,
		CreatedAt:         time.Now().UTC(),
		Parents:           parents,
	}
	prompt.Digest, err = digestState(prompt, parents)
	if err != nil {
		return PromptResult{}, err
	}
	if err := s.pinState(ctx, prompt); err != nil {
		return PromptResult{}, err
	}
	prompt, err = s.Store.AppendState(ctx, prompt, parents, checkpoint.ID)
	if err != nil {
		return PromptResult{}, mapHeadError(err)
	}
	_ = s.Store.UpdateAttemptStatus(ctx, attempt.ID, "active")
	_ = s.Store.UpdateTaskStatus(ctx, task.ID, "active")
	attempt, err = s.Store.GetAttempt(ctx, attempt.ID)
	if err != nil {
		return PromptResult{}, err
	}
	return PromptResult{
		Prompt:     prompt,
		Checkpoint: &checkpoint,
		Task:       task,
		Attempt:    attempt,
		Workspace:  attempt.Workspace,
		Deliver:    s.deliveryEnvironment(prompt, attempt),
	}, nil
}

func (s *Service) Checkpoint(ctx context.Context, stateID string) (State, error) {
	state, err := s.Store.GetState(ctx, stateID)
	if err != nil {
		return State{}, err
	}
	if state.AttemptID == "" {
		return State{}, fmt.Errorf("state %s does not belong to an attempt", state.ID)
	}
	attempt, err := s.Store.GetAttempt(ctx, state.AttemptID)
	if err != nil {
		return State{}, err
	}
	return s.checkpointAttempt(ctx, attempt)
}

func (s *Service) checkpointAttempt(ctx context.Context, attempt Attempt) (State, error) {
	var err error
	attempt, err = s.ensureAttemptWorkspace(ctx, attempt)
	if err != nil {
		return State{}, err
	}
	return s.checkpointExistingAttempt(ctx, attempt)
}

func (s *Service) checkpointExistingAttempt(ctx context.Context, attempt Attempt) (State, error) {
	workspaceRepo, err := OpenRepository(attempt.Workspace)
	if err != nil {
		return State{}, err
	}
	commit, tree, err := workspaceRepo.Snapshot(ctx, "hop: workspace checkpoint\n")
	if err != nil {
		return State{}, err
	}
	head, err := s.Store.GetState(ctx, attempt.HeadStateID)
	if err != nil {
		return State{}, err
	}
	parents := canonicalizeParents([]Parent{{StateID: head.ID, Role: "run_parent", Order: 0}})
	checkpoint := State{
		ID:                newID("c"),
		Kind:              StateCheckpoint,
		TaskID:            attempt.TaskID,
		AttemptID:         attempt.ID,
		CanonicalAnchorID: head.CanonicalAnchorID,
		SourceTree:        tree,
		GitCommit:         commit,
		Agent:             attempt.Agent,
		CreatedAt:         time.Now().UTC(),
		Parents:           parents,
	}
	baseStateID := checkpoint.CanonicalAnchorID
	if baseStateID == "" {
		baseStateID = attempt.BaseStateID
	}
	base, err := s.Store.GetState(ctx, baseStateID)
	if err != nil {
		return State{}, err
	}
	checkpoint.Provenance, err = s.buildProvenance(ctx, "checkpoint", base, tree, []ProvenanceInput{{
		Role: "workspace", StateID: head.ID, BaseTree: base.SourceTree, CandidateTree: tree,
	}})
	if err != nil {
		return State{}, err
	}
	checkpoint.Digest, err = digestState(checkpoint, parents)
	if err != nil {
		return State{}, err
	}
	if err := s.pinState(ctx, checkpoint); err != nil {
		return State{}, err
	}
	checkpoint, err = s.Store.AppendState(ctx, checkpoint, parents, head.ID)
	if err != nil {
		return State{}, mapHeadError(err)
	}
	return checkpoint, nil
}

// ensureAttemptWorkspace rehydrates a parked attempt from its immutable head.
// This makes resuming the original agent session lossless while allowing the
// bulky checkout to stay absent when the thread is abandoned.
func (s *Service) ensureAttemptWorkspace(ctx context.Context, attempt Attempt) (Attempt, error) {
	if attempt.Status != "parked" {
		return attempt, nil
	}
	head, err := s.Store.GetState(ctx, attempt.HeadStateID)
	if err != nil {
		return Attempt{}, err
	}
	created := false
	if _, err := os.Lstat(attempt.Workspace); errors.Is(err, os.ErrNotExist) {
		if err := os.MkdirAll(filepath.Dir(attempt.Workspace), 0o755); err != nil {
			return Attempt{}, fmt.Errorf("create parked workspace parent: %w", err)
		}
		if _, err := s.Repo.AddDetachedWorktree(ctx, attempt.Workspace, head.GitCommit); err != nil {
			return Attempt{}, fmt.Errorf("rehydrate parked workspace: %w", err)
		}
		created = true
	} else if err != nil {
		return Attempt{}, fmt.Errorf("inspect parked workspace: %w", err)
	}
	reactivated, err := s.Store.ReactivateParkedAttempt(ctx, attempt.ID, head.ID)
	if err != nil {
		if created {
			_ = s.Repo.RemoveWorktree(ctx, attempt.Workspace, true)
		}
		return Attempt{}, err
	}
	if !reactivated {
		current, currentErr := s.Store.GetAttempt(ctx, attempt.ID)
		if currentErr == nil && current.Status != "parked" {
			return current, nil
		}
		if created {
			_ = s.Repo.RemoveWorktree(ctx, attempt.Workspace, true)
		}
		return Attempt{}, &HeadChangedError{Scope: "attempt", Expected: head.ID, Actual: current.HeadStateID}
	}
	attempt.Status = "active"
	return attempt, nil
}

func (s *Service) RunCheck(ctx context.Context, stateID string, argv []string) (Check, error) {
	checkpoint, err := s.Checkpoint(ctx, stateID)
	if err != nil {
		return Check{}, err
	}
	attempt, err := s.Store.GetAttempt(ctx, checkpoint.AttemptID)
	if err != nil {
		return Check{}, err
	}
	checkID := newID("evidence")
	checkPath := filepath.Join(s.Root, ".hop", "checks", checkID)
	if err := os.MkdirAll(filepath.Dir(checkPath), 0o755); err != nil {
		return Check{}, err
	}
	if _, err := s.Repo.AddDetachedWorktree(ctx, checkPath, checkpoint.GitCommit); err != nil {
		return Check{}, err
	}
	env := []string{
		"HOP_ROOT=" + s.Root,
		"HOP_STATE_ID=" + checkpoint.ID,
		"HOP_TASK_ID=" + checkpoint.TaskID,
		"HOP_ATTEMPT_ID=" + attempt.ID,
		"HOP_WORKSPACE=" + checkPath,
	}
	result, runErr := runWorkspaceCommand(ctx, checkPath, env, argv)
	removeErr := s.Repo.RemoveWorktree(ctx, checkPath, true)
	if runErr != nil {
		return Check{}, runErr
	}
	if removeErr != nil {
		return Check{}, removeErr
	}
	storedCommand, _ := redactSecretStrings(argv)
	storedOutput, _ := RedactPromptSecrets(result.Output)
	check := Check{
		ID:        checkID,
		AttemptID: attempt.ID,
		StateID:   checkpoint.ID,
		TreeHash:  checkpoint.SourceTree,
		Command:   storedCommand,
		ExitCode:  result.ExitCode,
		Output:    storedOutput,
		CreatedAt: time.Now().UTC(),
	}
	check, err = s.Store.AddCheck(ctx, check)
	if err != nil {
		return Check{}, err
	}
	if check.ExitCode != 0 {
		return check, &CheckFailedError{Check: check}
	}
	return check, nil
}

func (s *Service) Propose(ctx context.Context, stateID, summary string) (ProposalResult, error) {
	state, err := s.Store.GetState(ctx, stateID)
	if err != nil {
		return ProposalResult{}, err
	}
	if state.AttemptID == "" {
		return ProposalResult{}, fmt.Errorf("state %s does not belong to an attempt", state.ID)
	}
	attempt, err := s.Store.GetAttempt(ctx, state.AttemptID)
	if err != nil {
		return ProposalResult{}, err
	}
	attempt, err = s.ensureAttemptWorkspace(ctx, attempt)
	if err != nil {
		return ProposalResult{}, err
	}
	task, err := s.Store.GetTask(ctx, attempt.TaskID)
	if err != nil {
		return ProposalResult{}, err
	}
	if strings.TrimSpace(summary) == "" {
		summary = task.Title
	}
	summary, _ = RedactPromptSecrets(summary)
	reconciliationPrompt, reconciliation, err := s.Store.ReconciliationPromptForAttempt(ctx, attempt.ID)
	if err != nil {
		return ProposalResult{}, err
	}
	var reconciliationConflicts []string
	var reconciliationRemoteTip string
	if reconciliation {
		metadata, ok := decodeReconciliationMetadata(reconciliationPrompt.Summary)
		if !ok {
			return ProposalResult{}, fmt.Errorf("reconciliation prompt %s has invalid conflict metadata", reconciliationPrompt.ID)
		}
		reconciliationConflicts = metadata.Conflicts
		reconciliationRemoteTip = metadata.RemoteTip
	}
	workspaceRepo, err := OpenRepository(attempt.Workspace)
	if err != nil {
		return ProposalResult{}, err
	}
	_, validationTree, err := workspaceRepo.Snapshot(ctx, "hop: proposal validation tree\n")
	if err != nil {
		return ProposalResult{}, err
	}
	if reconciliation {
		unresolved, err := unresolvedReconciliationMarkers(ctx, workspaceRepo, validationTree, reconciliationConflicts)
		if err != nil {
			return ProposalResult{}, err
		}
		if len(unresolved) > 0 {
			return ProposalResult{}, fmt.Errorf("reconciliation still contains merge markers in: %s", strings.Join(unresolved, ", "))
		}
	}
	checks, err := s.Store.ListChecks(ctx, attempt.ID, validationTree)
	if err != nil {
		return ProposalResult{}, err
	}
	if reconciliation {
		validated := false
		for _, check := range checks {
			if check.ExitCode == 0 {
				validated = true
				break
			}
		}
		if !validated {
			return ProposalResult{}, fmt.Errorf("reconciliation must pass hop check on the resolved tree before proposing")
		}
	}
	commit, tree, err := workspaceRepo.Snapshot(ctx, "hop: proposal\n")
	if err != nil {
		return ProposalResult{}, err
	}
	canonicalAnchorID := attempt.BaseStateID
	if reconciliation && reconciliationPrompt.CanonicalAnchorID != "" {
		canonicalAnchorID = reconciliationPrompt.CanonicalAnchorID
	}
	parents := canonicalizeParents([]Parent{{StateID: attempt.HeadStateID, Role: "run_parent", Order: 0}})
	proposal := State{
		ID:                newID("r"),
		Kind:              StateProposal,
		TaskID:            attempt.TaskID,
		AttemptID:         attempt.ID,
		CanonicalAnchorID: canonicalAnchorID,
		SourceTree:        tree,
		GitCommit:         commit,
		Summary:           summary,
		Agent:             attempt.Agent,
		CreatedAt:         time.Now().UTC(),
		Parents:           parents,
	}
	base, err := s.Store.GetState(ctx, canonicalAnchorID)
	if err != nil {
		return ProposalResult{}, err
	}
	provenanceInputs := []ProvenanceInput{{
		Role: "workspace", StateID: attempt.HeadStateID, BaseTree: base.SourceTree, CandidateTree: tree,
	}}
	if reconciliationRemoteTip != "" {
		mergeBase, mergeErr := s.Repo.MergeBase(ctx, base.GitCommit, reconciliationRemoteTip)
		if mergeErr != nil {
			return ProposalResult{}, mergeErr
		}
		mergeBaseTree, treeErr := s.Repo.resolveTree(ctx, mergeBase)
		if treeErr != nil {
			return ProposalResult{}, treeErr
		}
		remoteTree, treeErr := s.Repo.resolveTree(ctx, reconciliationRemoteTip)
		if treeErr != nil {
			return ProposalResult{}, treeErr
		}
		provenanceInputs = append(provenanceInputs, ProvenanceInput{
			Role: "reconciliation_remote", BaseTree: mergeBaseTree, CandidateTree: remoteTree,
		})
	}
	proposal.Provenance, err = s.buildProvenance(ctx, "proposal", base, tree, provenanceInputs)
	if err != nil {
		return ProposalResult{}, err
	}
	proposal.Digest, err = digestState(proposal, parents)
	if err != nil {
		return ProposalResult{}, err
	}
	if err := s.pinState(ctx, proposal); err != nil {
		return ProposalResult{}, err
	}
	proposal, err = s.Store.AppendState(ctx, proposal, parents, attempt.HeadStateID)
	if err != nil {
		return ProposalResult{}, mapHeadError(err)
	}
	_ = s.Store.UpdateAttemptStatus(ctx, attempt.ID, "proposed")
	_ = s.Store.UpdateTaskStatus(ctx, task.ID, "proposed")
	result := ProposalResult{Proposal: proposal, Checks: checks}
	s.attachPromptCloudSync(ctx, &result.PromptSync, &result.Warnings)
	return result, nil
}

// CompletePrompt records the exact user-visible response for one prompt and
// immediately attempts private sync. Completion is independent of Git state,
// so read-only and external-operation turns are represented correctly.
func (s *Service) CompletePrompt(ctx context.Context, stateID, summary, finalResponse string) (CompletionResult, error) {
	if len(summary) > 1<<20 {
		return CompletionResult{}, errors.New("completion summary exceeds 1 MiB")
	}
	if len(finalResponse) > 16<<20 {
		return CompletionResult{}, errors.New("final response exceeds 16 MiB")
	}
	summary, _ = RedactPromptSecrets(summary)
	finalResponse, _ = RedactPromptSecrets(finalResponse)
	completion, err := s.Store.PutPromptCompletion(ctx, PromptCompletion{
		StateID: stateID, Summary: summary, FinalResponse: finalResponse, CompletedAt: time.Now().UTC(),
	})
	if err != nil {
		return CompletionResult{}, err
	}
	result := CompletionResult{Completion: completion}
	if err := s.finalizeSourceCleanAttempt(ctx, stateID); err != nil {
		message, _ := RedactPromptSecrets(err.Error())
		result.Warnings = append(result.Warnings, "completion is stored; clean attempt finalization failed: "+message)
	}
	promptSync, syncErr := s.SyncPromptHistory(ctx)
	if syncErr == nil {
		result.PromptSync = promptSync
	} else if !errors.Is(syncErr, ErrNotAuthenticated) {
		message, _ := RedactPromptSecrets(syncErr.Error())
		result.Warnings = append(result.Warnings, "completion is stored locally; cloud sync failed: "+message)
	}
	excludeAttemptID := ""
	if state, stateErr := s.Store.GetState(ctx, stateID); stateErr == nil && state.AttemptID != "" {
		if attempt, attemptErr := s.Store.GetAttempt(ctx, state.AttemptID); attemptErr == nil && !isTerminalAttemptStatus(attempt.Status) {
			excludeAttemptID = attempt.ID
		}
	}
	cleanup, cleanupErr := s.CleanupWorkspacesWithOptions(ctx, WorkspaceCleanupOptions{
		IncludeAbandoned: true,
		AbandonedAfter:   DefaultAbandonedAfter,
		ExcludeAttemptID: excludeAttemptID,
	})
	if cleanupErr != nil {
		message, _ := RedactPromptSecrets(cleanupErr.Error())
		result.Warnings = append(result.Warnings, "completion is stored; workspace cleanup failed: "+message)
	} else {
		result.Cleanup = &cleanup
	}
	return result, nil
}

func (s *Service) finalizeSourceCleanAttempt(ctx context.Context, stateID string) error {
	state, err := s.Store.GetState(ctx, stateID)
	if err != nil {
		return err
	}
	attempt, err := s.Store.GetAttempt(ctx, state.AttemptID)
	if err != nil {
		return err
	}
	if attempt.Status != "active" || attempt.HeadStateID != state.ID {
		return nil
	}
	base, err := s.Store.GetState(ctx, attempt.BaseStateID)
	if err != nil {
		return err
	}
	workspaceRepo, err := OpenRepository(attempt.Workspace)
	if err != nil {
		return err
	}
	tree, err := workspaceRepo.WorktreeTree(ctx, base.SourceTree)
	if err != nil {
		return err
	}
	if tree != base.SourceTree {
		return nil
	}
	_, err = s.Store.CompleteCleanAttempt(ctx, attempt.ID, state.ID)
	return err
}

// CleanupWorkspaces removes terminal worktrees and parks attempts that have
// been inactive for the default retention window. Parking checkpoints the
// exact tree before removing the checkout, and the original session can later
// rehydrate it without losing work.
func (s *Service) CleanupWorkspaces(ctx context.Context) (WorkspaceCleanupResult, error) {
	return s.CleanupWorkspacesWithOptions(ctx, WorkspaceCleanupOptions{
		IncludeAbandoned: true,
		AbandonedAfter:   DefaultAbandonedAfter,
	})
}

// AttemptContainingPath identifies the managed attempt whose checkout contains
// path. GC uses it to protect the workspace from which it is currently run.
func (s *Service) AttemptContainingPath(ctx context.Context, path string) (string, bool, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", false, err
	}
	abs = canonicalExistingPath(abs)
	attempts, err := s.Store.ListAttempts(ctx, "", "")
	if err != nil {
		return "", false, err
	}
	for _, attempt := range attempts {
		workspace := canonicalExistingPath(attempt.Workspace)
		rel, err := filepath.Rel(workspace, abs)
		if err != nil {
			continue
		}
		if rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))) {
			return attempt.ID, true, nil
		}
	}
	return "", false, nil
}

func (s *Service) CleanupWorkspacesWithOptions(ctx context.Context, options WorkspaceCleanupOptions) (WorkspaceCleanupResult, error) {
	if options.AbandonedAfter < 0 {
		return WorkspaceCleanupResult{}, errors.New("abandoned retention cannot be negative")
	}
	if options.Now.IsZero() {
		options.Now = time.Now().UTC()
	}
	// A follow-up and GC must not race workspace checkpoint/removal. The new
	// prompt is already durable before CLI-triggered cleanup reaches this lock.
	releasePrompt, err := acquireProjectLock(ctx, s.Root, "prompt")
	if err != nil {
		return WorkspaceCleanupResult{}, err
	}
	defer releasePrompt()
	releaseGC, err := acquireProjectLock(ctx, s.Root, "gc")
	if err != nil {
		return WorkspaceCleanupResult{}, err
	}
	defer releaseGC()
	attempts, err := s.Store.ListAttempts(ctx, "", "")
	if err != nil {
		return WorkspaceCleanupResult{}, err
	}
	result := WorkspaceCleanupResult{}
	for _, attempt := range attempts {
		if attempt.ID == options.ExcludeAttemptID {
			continue
		}
		expected := filepath.Join(s.Root, ".hop", "workspaces", attempt.ID)
		if filepath.Base(filepath.Clean(attempt.Workspace)) != attempt.ID ||
			canonicalExistingPath(filepath.Dir(attempt.Workspace)) != canonicalExistingPath(filepath.Dir(expected)) {
			if isTerminalAttemptStatus(attempt.Status) || attempt.Status == "parked" || options.IncludeAbandoned {
				preserveCleanupIssue(&result, attempt, "workspace is outside Hop's managed workspace directory")
			}
			continue
		}

		_, statErr := os.Lstat(attempt.Workspace)
		missing := errors.Is(statErr, os.ErrNotExist)
		if statErr != nil && !missing {
			preserveCleanupIssue(&result, attempt, statErr.Error())
			continue
		}

		if isTerminalAttemptStatus(attempt.Status) {
			result.Scanned++
			if missing {
				continue
			}
			if err := s.removeTerminalWorkspace(ctx, attempt, options.ArchiveDirtyTerminal, &result); err != nil {
				return result, err
			}
			continue
		}

		if attempt.Status == "parked" {
			if missing {
				continue
			}
			result.Scanned++
			if err := s.removeParkedWorkspace(ctx, attempt, options.ArchiveDirtyTerminal, &result); err != nil {
				return result, err
			}
			continue
		}

		if !options.IncludeAbandoned {
			continue
		}
		result.AbandonedScanned++
		head, err := s.Store.GetState(ctx, attempt.HeadStateID)
		if err != nil {
			return result, err
		}
		lastActivity := head.CreatedAt
		if attempt.CreatedAt.After(lastActivity) {
			lastActivity = attempt.CreatedAt
		}
		if !missing {
			_, latest, err := directoryStats(attempt.Workspace)
			if err != nil {
				preserveCleanupIssue(&result, attempt, "cannot measure worktree activity: "+err.Error())
				continue
			}
			if latest.After(lastActivity) {
				lastActivity = latest
			}
		}
		if options.AbandonedAfter > 0 && options.Now.Sub(lastActivity) < options.AbandonedAfter {
			continue
		}
		if err := s.parkWorkspace(ctx, attempt, missing, &result); err != nil {
			return result, err
		}
	}
	if err := s.Repo.PruneWorktrees(ctx); err != nil {
		return result, err
	}
	return result, nil
}

func (s *Service) parkWorkspace(ctx context.Context, attempt Attempt, missing bool, result *WorkspaceCleanupResult) error {
	head, err := s.Store.GetState(ctx, attempt.HeadStateID)
	if err != nil {
		return err
	}
	var bytes int64
	if !missing {
		workspaceRepo, err := OpenRepository(attempt.Workspace)
		if err != nil {
			preserveCleanupIssue(result, attempt, "cannot inspect abandoned worktree: "+err.Error())
			return nil
		}
		tree, err := workspaceRepo.WorktreeTree(ctx, head.SourceTree)
		if err != nil {
			preserveCleanupIssue(result, attempt, "cannot snapshot abandoned worktree: "+err.Error())
			return nil
		}
		if tree != head.SourceTree {
			head, err = s.checkpointExistingAttempt(ctx, attempt)
			if err != nil {
				preserveCleanupIssue(result, attempt, "cannot checkpoint abandoned worktree: "+err.Error())
				return nil
			}
			attempt.HeadStateID = head.ID
		}
		bytes, _, err = directoryStats(attempt.Workspace)
		if err != nil {
			preserveCleanupIssue(result, attempt, "cannot measure abandoned worktree: "+err.Error())
			return nil
		}
	}
	parked, err := s.Store.ParkAttempt(ctx, attempt.ID, head.ID)
	if err != nil {
		return err
	}
	if !parked {
		preserveCleanupIssue(result, attempt, "attempt changed while it was being parked")
		return nil
	}
	result.Parked++
	if missing {
		return nil
	}
	workspaceRepo, err := OpenRepository(attempt.Workspace)
	if err != nil {
		preserveCleanupIssue(result, attempt, "cannot verify parked worktree: "+err.Error())
		return nil
	}
	tree, err := workspaceRepo.WorktreeTree(ctx, head.SourceTree)
	if err != nil {
		preserveCleanupIssue(result, attempt, "cannot verify parked worktree: "+err.Error())
		return nil
	}
	if tree != head.SourceTree {
		_, _ = s.Store.ReactivateParkedAttempt(ctx, attempt.ID, head.ID)
		result.Parked--
		preserveCleanupIssue(result, attempt, "worktree changed while it was being parked")
		return nil
	}
	if err := s.Repo.RemoveWorktree(ctx, attempt.Workspace, true); err != nil {
		preserveCleanupIssue(result, attempt, err.Error())
		return nil
	}
	result.Removed++
	result.ReclaimedBytes += bytes
	return nil
}

func (s *Service) removeTerminalWorkspace(ctx context.Context, attempt Attempt, archiveDirty bool, result *WorkspaceCleanupResult) error {
	head, err := s.Store.GetState(ctx, attempt.HeadStateID)
	if err != nil {
		return err
	}
	workspaceRepo, err := OpenRepository(attempt.Workspace)
	if err != nil {
		preserveCleanupIssue(result, attempt, "cannot safely inspect worktree: "+err.Error())
		return nil
	}
	tree, err := workspaceRepo.WorktreeTree(ctx, head.SourceTree)
	if err != nil {
		preserveCleanupIssue(result, attempt, "cannot safely snapshot worktree: "+err.Error())
		return nil
	}
	if tree != head.SourceTree {
		if !archiveDirty {
			preserveCleanupIssue(result, attempt, "terminal worktree contains unrecorded source changes")
			return nil
		}
		head, err = s.checkpointExistingAttempt(ctx, attempt)
		if err != nil {
			preserveCleanupIssue(result, attempt, "cannot checkpoint terminal worktree: "+err.Error())
			return nil
		}
		attempt.HeadStateID = head.ID
	}
	bytes, _, err := directoryStats(attempt.Workspace)
	if err != nil {
		preserveCleanupIssue(result, attempt, "cannot measure worktree: "+err.Error())
		return nil
	}
	if err := s.Store.ClearAgentSessionsForAttempt(ctx, attempt.ID); err != nil {
		return err
	}
	if err := s.Repo.RemoveWorktree(ctx, attempt.Workspace, true); err != nil {
		preserveCleanupIssue(result, attempt, err.Error())
		return nil
	}
	result.Removed++
	result.ReclaimedBytes += bytes
	return nil
}

func (s *Service) removeParkedWorkspace(ctx context.Context, attempt Attempt, archiveDirty bool, result *WorkspaceCleanupResult) error {
	head, err := s.Store.GetState(ctx, attempt.HeadStateID)
	if err != nil {
		return err
	}
	workspaceRepo, err := OpenRepository(attempt.Workspace)
	if err != nil {
		preserveCleanupIssue(result, attempt, "cannot safely inspect parked worktree: "+err.Error())
		return nil
	}
	tree, err := workspaceRepo.WorktreeTree(ctx, head.SourceTree)
	if err != nil {
		preserveCleanupIssue(result, attempt, "cannot safely snapshot parked worktree: "+err.Error())
		return nil
	}
	if tree != head.SourceTree {
		if !archiveDirty {
			preserveCleanupIssue(result, attempt, "parked worktree contains later source changes")
			return nil
		}
		head, err = s.checkpointExistingAttempt(ctx, attempt)
		if err != nil {
			preserveCleanupIssue(result, attempt, "cannot checkpoint parked worktree: "+err.Error())
			return nil
		}
		attempt.HeadStateID = head.ID
	}
	bytes, _, err := directoryStats(attempt.Workspace)
	if err != nil {
		preserveCleanupIssue(result, attempt, "cannot measure parked worktree: "+err.Error())
		return nil
	}
	if err := s.Repo.RemoveWorktree(ctx, attempt.Workspace, true); err != nil {
		preserveCleanupIssue(result, attempt, err.Error())
		return nil
	}
	result.Removed++
	result.ReclaimedBytes += bytes
	return nil
}

func preserveCleanupIssue(result *WorkspaceCleanupResult, attempt Attempt, reason string) {
	result.Preserved = append(result.Preserved, WorkspaceCleanupIssue{
		AttemptID: attempt.ID, Workspace: attempt.Workspace, Reason: reason,
	})
}

func directorySize(root string) (int64, error) {
	size, _, err := directoryStats(root)
	return size, err
}

func directoryStats(root string) (int64, time.Time, error) {
	var size int64
	var latest time.Time
	err := filepath.Walk(root, func(_ string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.IsDir() {
			size += info.Size()
		}
		if info.ModTime().After(latest) {
			latest = info.ModTime()
		}
		return nil
	})
	return size, latest, err
}

// Accept advances Hop's internal accepted lineage without changing the visible
// project root. Controllers use this lower-level operation deliberately.
func (s *Service) Accept(ctx context.Context, proposalID string, checkCommand []string) (AcceptResult, error) {
	return s.accept(ctx, proposalID, checkCommand, false)
}

// Land accepts a proposal and projects the resulting accepted tree into the
// visible project root while preserving the user's HEAD, branch, and index.
func (s *Service) Land(ctx context.Context, proposalID string, checkCommand []string) (AcceptResult, error) {
	return s.accept(ctx, proposalID, checkCommand, true)
}

// Push publishes the current accepted commit to the repository's inferred
// upstream branch. Land and Accept call the same operation automatically; this
// explicit form is the retry/recovery surface for an agent after a warning.
func (s *Service) Push(ctx context.Context) (RemotePushResult, error) {
	release, err := acquireProjectLock(ctx, s.Root, "accept")
	if err != nil {
		return RemotePushResult{}, err
	}
	defer release()
	accepted, err := s.Store.AcceptedHead(ctx)
	if err != nil {
		return RemotePushResult{}, err
	}
	result, configured, err := s.publishAccepted(ctx, accepted)
	if err != nil {
		return RemotePushResult{}, err
	}
	if !configured {
		return RemotePushResult{}, errors.New("hop: no unambiguous Git remote branch is configured for automatic push")
	}
	return result, nil
}

// PushTag publishes an existing annotated tag to the same unambiguous remote
// used for accepted-state pushes. It never creates, rewrites, or force-pushes
// a tag; release tooling remains responsible for signing and verification.
func (s *Service) PushTag(ctx context.Context, tag string) (RemotePushResult, error) {
	release, err := acquireProjectLock(ctx, s.Root, "accept")
	if err != nil {
		return RemotePushResult{}, err
	}
	defer release()
	pushCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	result, configured, err := s.Repo.PushTag(pushCtx, tag)
	if err != nil {
		message, _ := RedactPromptSecrets(err.Error())
		return RemotePushResult{}, errors.New(message)
	}
	if !configured {
		return RemotePushResult{}, errors.New("hop: no unambiguous Git remote is configured for tag push")
	}
	return result, nil
}

func (s *Service) accept(ctx context.Context, proposalID string, checkCommand []string, materialize bool) (AcceptResult, error) {
	release, err := acquireProjectLock(ctx, s.Root, "accept")
	if err != nil {
		return AcceptResult{}, err
	}
	defer release()

	proposal, err := s.Store.GetState(ctx, proposalID)
	if err != nil {
		return AcceptResult{}, err
	}
	if proposal.Kind != StateProposal {
		return AcceptResult{}, fmt.Errorf("state %s is %s, not a proposal", proposal.ID, proposal.Kind)
	}
	if existing, exists, err := s.Store.AcceptedForProposal(ctx, proposal.ID); err != nil {
		return AcceptResult{}, err
	} else if exists {
		parent, parentErr := s.Store.ParentByRole(ctx, existing.ID, "canonical_parent")
		if parentErr != nil {
			return AcceptResult{}, parentErr
		}
		base, baseErr := s.Store.GetState(ctx, parent.StateID)
		if baseErr != nil {
			return AcceptResult{}, baseErr
		}
		if proofErr := s.verifyStoredProvenance(ctx, existing, base); proofErr != nil {
			return AcceptResult{}, fmt.Errorf("existing acceptance cannot be safely retried: %w", proofErr)
		}
		result := AcceptResult{State: existing}
		if materialize {
			captured, paths, captureErr := s.captureVisibleRoot(ctx)
			if captureErr != nil {
				return result, captureErr
			}
			result.CapturedRoot = captured
			result.CapturedRootPaths = paths
			if _, err := s.syncLocked(ctx); err != nil {
				return result, &CommittedStateError{State: existing, Err: fmt.Errorf("repair visible root after prior acceptance: %w", err)}
			}
			result.MaterializedRoot = s.Root
		}
		s.attachAutomaticPush(ctx, &result)
		s.attachPromptCloudSync(ctx, &result.PromptSync, &result.Warnings)
		return result, nil
	}
	attempt, err := s.Store.GetAttempt(ctx, proposal.AttemptID)
	if err != nil {
		return AcceptResult{}, err
	}
	if attempt.HeadStateID != proposal.ID {
		return AcceptResult{}, &HeadChangedError{
			Scope: "attempt", Expected: proposal.ID, Actual: attempt.HeadStateID,
		}
	}
	var capturedRoot *State
	var capturedRootPaths []string
	if materialize {
		captured, paths, captureErr := s.captureVisibleRoot(ctx)
		if captureErr != nil {
			return AcceptResult{}, captureErr
		}
		capturedRoot = captured
		capturedRootPaths = paths
	}
	baseStateID := proposal.CanonicalAnchorID
	if baseStateID == "" {
		baseStateID = attempt.BaseStateID
	}
	base, err := s.Store.GetState(ctx, baseStateID)
	if err != nil {
		return AcceptResult{}, err
	}
	if err := s.verifyStoredProvenance(ctx, proposal, base); err != nil {
		return AcceptResult{}, fmt.Errorf("proposal authorization proof is invalid: %w", err)
	}
	current, err := s.Store.AcceptedHead(ctx)
	if err != nil {
		return AcceptResult{}, err
	}
	proposalPaths, err := s.Repo.ChangedPaths(ctx, base.GitCommit, proposal.GitCommit)
	if err != nil {
		return AcceptResult{}, err
	}
	currentPaths, err := s.Repo.ChangedPaths(ctx, base.GitCommit, current.GitCommit)
	if err != nil {
		return AcceptResult{}, err
	}
	proposalPaths = withoutPortableHopRecords(proposalPaths)
	currentPaths = withoutPortableHopRecords(currentPaths)
	acceptInputs := []ProvenanceInput{{
		Role: "proposal", StateID: proposal.ID, BaseTree: base.SourceTree, CandidateTree: proposal.SourceTree,
	}}
	finalTree := proposal.SourceTree
	if current.SourceTree != base.SourceTree {
		var mergeConflicts []string
		finalTree, mergeConflicts, err = s.Repo.ComposeTrees(ctx, base.GitCommit, current.GitCommit, proposal.GitCommit)
		if err != nil {
			return AcceptResult{}, err
		}
		if len(mergeConflicts) > 0 {
			return AcceptResult{}, &ConflictError{Paths: mergeConflicts}
		}
	}
	commitParents := []string{current.GitCommit}
	incorporatedRemoteTip, err := s.proposalRemoteReconciliationTip(ctx, proposal)
	if err != nil {
		return AcceptResult{}, err
	}
	// The ephemeral candidate is a semantic merge of the current accepted tree
	// and the proposal. Keeping both parents lets a proposal produced by an
	// earlier reconciliation carry that input ancestry into any later remote
	// comparison, including nested accepted-state reconciliations.
	candidateParents := []string{current.GitCommit, proposal.GitCommit}
	if incorporatedRemoteTip != "" {
		commitParents = candidateParents
	}
	remoteCtx, cancelRemote := context.WithTimeout(ctx, 30*time.Second)
	remoteTip, _, remoteExists, err := s.Repo.FetchPushTip(remoteCtx)
	cancelRemote()
	remoteObservationWarning := ""
	if err != nil {
		message, _ := RedactPromptSecrets(err.Error())
		remoteObservationWarning = "accepted locally without remote reconciliation because the upstream could not be inspected: " + message
		remoteExists = false
	}
	if remoteExists && remoteTip != current.GitCommit {
		if remoteTip != incorporatedRemoteTip {
			candidate, candidateErr := s.Repo.CommitTree(ctx, finalTree, candidateParents, "Hop remote reconciliation candidate\n")
			if candidateErr != nil {
				return AcceptResult{}, candidateErr
			}
			mergeBase, mergeErr := s.Repo.MergeBase(ctx, candidate, remoteTip)
			if mergeErr != nil {
				return AcceptResult{}, mergeErr
			}
			var remoteConflicts []string
			finalTree, remoteConflicts, err = s.Repo.ComposeTrees(ctx, mergeBase, remoteTip, candidate)
			if err != nil {
				return AcceptResult{}, err
			}
			if len(remoteConflicts) > 0 {
				return AcceptResult{}, &ConflictError{Paths: remoteConflicts, RemoteTip: remoteTip}
			}
			mergeBaseTree, treeErr := s.Repo.resolveTree(ctx, mergeBase)
			if treeErr != nil {
				return AcceptResult{}, treeErr
			}
			remoteTree, treeErr := s.Repo.resolveTree(ctx, remoteTip)
			if treeErr != nil {
				return AcceptResult{}, treeErr
			}
			acceptInputs = append(acceptInputs, ProvenanceInput{
				Role: "remote", BaseTree: mergeBaseTree, CandidateTree: remoteTree,
			})
		}
		commitParents = []string{remoteTip, current.GitCommit}
	}

	acceptedID := newID("a")
	message := proposal.Summary
	if message == "" {
		message = "Accept " + proposal.ID
	}
	message += fmt.Sprintf("\n\nHop-State: %s\nHop-Proposal: %s\nHop-Task: %s\nHop-Attempt: %s\n", acceptedID, proposal.ID, proposal.TaskID, proposal.AttemptID)
	author, configured, err := s.Repo.ConfiguredUserIdentity(ctx)
	if err != nil {
		return AcceptResult{}, err
	}
	if !configured {
		author = s.Repo.SyntheticIdentity()
	}
	commit, err := s.Repo.CommitTreeWithOptions(ctx, finalTree, CommitOptions{
		Message:   message,
		Parents:   commitParents,
		Author:    author,
		Committer: s.Repo.SyntheticIdentity(),
	})
	if err != nil {
		return AcceptResult{}, err
	}

	var recordedCheck *Check
	validationRef := ""
	defer func() {
		if validationRef != "" {
			_ = s.Repo.DeleteRef(ctx, "refs/hop/"+validationRef, commit)
		}
	}()
	if len(checkCommand) > 0 {
		checkID := newID("evidence")
		validationRef = "validation/" + checkID
		if err := s.Repo.UpdateHiddenRef(ctx, validationRef, commit); err != nil {
			return AcceptResult{}, fmt.Errorf("pin final validation tree: %w", err)
		}
		integrationPath := filepath.Join(s.Root, ".hop", "integration", acceptedID)
		if err := os.MkdirAll(filepath.Dir(integrationPath), 0o755); err != nil {
			return AcceptResult{}, err
		}
		if _, err := s.Repo.AddDetachedWorktree(ctx, integrationPath, commit); err != nil {
			return AcceptResult{}, err
		}
		result, runErr := runWorkspaceCommand(ctx, integrationPath, []string{
			"HOP_ROOT=" + s.Root,
			"HOP_STATE_ID=" + acceptedID,
			"HOP_TASK_ID=" + proposal.TaskID,
			"HOP_ATTEMPT_ID=" + proposal.AttemptID,
			"HOP_ACCEPTANCE_LOCK_HELD=1",
		}, checkCommand)
		removeErr := s.Repo.RemoveWorktree(ctx, integrationPath, true)
		if runErr != nil {
			return AcceptResult{}, runErr
		}
		if removeErr != nil {
			return AcceptResult{}, removeErr
		}
		storedCommand, _ := redactSecretStrings(checkCommand)
		storedOutput, _ := RedactPromptSecrets(result.Output)
		check := Check{
			ID:        checkID,
			AttemptID: proposal.AttemptID,
			TreeHash:  finalTree,
			Command:   storedCommand,
			ExitCode:  result.ExitCode,
			Output:    storedOutput,
			CreatedAt: time.Now().UTC(),
		}
		if check.ExitCode != 0 {
			if failed, recorded := s.recordValidationFailure(ctx, proposal, current, commit, finalTree, checkCommand); recorded {
				check.StateID = failed.ID
				_ = s.Repo.DeleteRef(ctx, "refs/hop/"+validationRef, commit)
				validationRef = ""
			}
		}
		check, err = s.Store.AddCheck(ctx, check)
		if err != nil {
			return AcceptResult{}, err
		}
		recordedCheck = &check
		if check.ExitCode != 0 {
			return AcceptResult{}, &CheckFailedError{Check: check}
		}
	}

	var rootBase State
	if materialize {
		rootBase, err = s.prepareRootMaterialization(ctx, finalTree)
		if err != nil {
			return AcceptResult{}, err
		}
	}

	parents := canonicalizeParents([]Parent{
		{StateID: current.ID, Role: "canonical_parent", Order: 0},
		{StateID: proposal.ID, Role: "proposal_parent", Order: 1},
	})
	operation := "accept"
	if materialize {
		operation = "land"
	}
	provenance, err := s.verifyAcceptance(ctx, current, finalTree, acceptInputs, operation)
	if err != nil {
		return AcceptResult{}, err
	}
	accepted := State{
		ID:                acceptedID,
		Kind:              StateAccepted,
		TaskID:            proposal.TaskID,
		AttemptID:         proposal.AttemptID,
		CanonicalAnchorID: current.ID,
		SourceTree:        finalTree,
		GitCommit:         commit,
		Summary:           proposal.Summary,
		Agent:             proposal.Agent,
		Provenance:        provenance,
		CreatedAt:         time.Now().UTC(),
		Parents:           parents,
	}
	accepted.Digest, err = digestState(accepted, parents)
	if err != nil {
		return AcceptResult{}, err
	}
	if err := s.pinState(ctx, accepted); err != nil {
		return AcceptResult{}, err
	}
	accepted, err = s.Store.CASAccept(ctx, current.ID, accepted, parents)
	if err != nil {
		return AcceptResult{}, mapHeadError(err)
	}
	publicationRecordWarning := ""
	if err := s.recordPendingPublication(ctx, accepted, remoteTip); err != nil {
		publicationRecordWarning = "accepted state is durable, but publication status could not be initialized: " + err.Error()
	}
	if validationRef != "" {
		_ = s.Repo.DeleteRef(ctx, "refs/hop/"+validationRef, commit)
		validationRef = ""
	}
	var warnings []string
	if publicationRecordWarning != "" {
		warnings = append(warnings, publicationRecordWarning)
	}
	if remoteObservationWarning != "" {
		warnings = append(warnings, remoteObservationWarning)
	}
	if err := s.Repo.UpdateHiddenRef(ctx, acceptedRef, commit); err != nil {
		warnings = append(warnings, "accepted state is durable in SQLite but refs/hop/accepted needs repair: "+err.Error())
	}
	result := AcceptResult{
		State:             accepted,
		CapturedRoot:      capturedRoot,
		CapturedRootPaths: capturedRootPaths,
		ProposalPaths:     proposalPaths,
		CurrentPaths:      currentPaths,
		Check:             recordedCheck,
		Warnings:          warnings,
	}
	if materialize {
		if err := s.Repo.MaterializeTree(ctx, rootBase.SourceTree, accepted.SourceTree); err != nil {
			return result, &CommittedStateError{State: accepted, Err: fmt.Errorf("visible root %s was not synchronized: %w", s.Root, err)}
		}
		if err := s.Store.CASMaterializedHead(ctx, rootBase.ID, accepted.ID); err != nil {
			return result, &CommittedStateError{State: accepted, Err: fmt.Errorf("record visible root synchronization: %w", err)}
		}
		result.MaterializedRoot = s.Root
	}
	s.attachAutomaticPush(ctx, &result)
	s.attachPromptCloudSync(ctx, &result.PromptSync, &result.Warnings)
	return result, nil
}

// captureVisibleRoot promotes ordinary nonignored worktree edits into an
// explicit accepted transition before landing another proposal. That makes
// out-of-band work a normal concurrent input to Hop's existing three-way merge
// and reconciliation flow instead of forcing an agent to wait for a human.
//
// The real Git index and ignored-file collisions remain fail-closed. When the
// accepted head is newer than the projected root, compatible edits are first
// composed with that accepted advancement; genuine conflicts still require a
// deliberate reconciliation rather than guessing which intent wins.
func (s *Service) captureVisibleRoot(ctx context.Context) (*State, []string, error) {
	materialized, err := s.Store.MaterializedHead(ctx)
	if err != nil {
		return nil, nil, err
	}
	if err := s.Repo.CheckIndexSafe(ctx, materialized.SourceTree); err != nil {
		return nil, nil, err
	}
	actualTree, err := s.Repo.WorktreeTree(ctx, materialized.SourceTree)
	if err != nil {
		return nil, nil, err
	}
	if actualTree == materialized.SourceTree {
		return nil, nil, nil
	}
	gitStatus, err := s.Repo.GitStatusForAccepted(ctx, materialized.GitCommit, materialized.SourceTree)
	if err != nil {
		return nil, nil, err
	}
	if gitStatus.ProjectionOverStaleRef {
		return nil, nil, &RootConflictError{
			Paths:  gitStatus.UserWorktreePaths,
			Reason: "visible project root differs from its Hop materialization while the selected Git branch/index is stale; Hop cannot prove which paths were deliberately edited, so it refused to capture or delete anything (restore/sync the accepted projection, or explicitly commit/adopt the intended changes from a current base)",
		}
	}
	paths, err := s.Repo.ChangedPaths(ctx, materialized.SourceTree, actualTree)
	if err != nil {
		return nil, nil, err
	}
	current, err := s.Store.AcceptedHead(ctx)
	if err != nil {
		return nil, nil, err
	}

	rootCommit, err := s.Repo.CommitTree(ctx, actualTree, []string{materialized.GitCommit}, "Hop visible-root snapshot\n")
	if err != nil {
		return nil, nil, err
	}
	finalTree := actualTree
	if current.ID != materialized.ID {
		var conflicts []string
		finalTree, conflicts, err = s.Repo.ComposeTrees(ctx, materialized.GitCommit, current.GitCommit, rootCommit)
		if err != nil {
			return nil, nil, err
		}
		if len(conflicts) > 0 {
			return nil, nil, &RootConflictError{
				Paths:  conflicts,
				Reason: "visible project root changes conflict with a newer accepted state",
			}
		}
	}

	capturedID := newID("a")
	commit, err := s.Repo.CommitTree(ctx, finalTree, []string{current.GitCommit}, fmt.Sprintf(
		"Capture visible project changes\n\nHop-State: %s\nHop-Captured-From: %s\n", capturedID, materialized.ID))
	if err != nil {
		return nil, nil, err
	}
	parents := canonicalizeParents([]Parent{{StateID: current.ID, Role: "canonical_parent", Order: 0}})
	provenance, err := s.verifyAcceptance(ctx, current, finalTree, []ProvenanceInput{{
		Role: "visible_root", StateID: materialized.ID, BaseTree: materialized.SourceTree, CandidateTree: actualTree,
	}}, "capture-visible-root")
	if err != nil {
		return nil, nil, err
	}
	// The project lock serializes Hop, not arbitrary editors or Git commands.
	// Re-snapshot immediately before the accepted CAS so a racing filesystem
	// change cannot enter under a proof built for an older tree.
	verifiedTree, err := s.Repo.WorktreeTree(ctx, materialized.SourceTree)
	if err != nil {
		return nil, nil, err
	}
	if verifiedTree != actualTree {
		return nil, nil, &RootConflictError{Reason: "visible project root changed while Hop was proving its authorization manifest; retry from a stable root"}
	}
	captured := State{
		ID:                capturedID,
		Kind:              StateAccepted,
		CanonicalAnchorID: current.ID,
		SourceTree:        finalTree,
		GitCommit:         commit,
		Summary:           "Capture out-of-band visible project changes",
		Agent:             "hop",
		Provenance:        provenance,
		CreatedAt:         time.Now().UTC(),
		Parents:           parents,
	}
	captured.Digest, err = digestState(captured, parents)
	if err != nil {
		return nil, nil, err
	}
	if err := s.pinState(ctx, captured); err != nil {
		return nil, nil, err
	}
	captured, err = s.Store.CASAccept(ctx, current.ID, captured, parents)
	if err != nil {
		return nil, nil, mapHeadError(err)
	}
	if err := s.Repo.UpdateHiddenRef(ctx, acceptedRef, captured.GitCommit); err != nil {
		return &captured, paths, &CommittedStateError{State: captured, Err: fmt.Errorf("repair refs/hop/accepted: %w", err)}
	}
	if err := s.Repo.MaterializeTree(ctx, actualTree, captured.SourceTree); err != nil {
		return &captured, paths, &CommittedStateError{State: captured, Err: fmt.Errorf("synchronize captured visible root: %w", err)}
	}
	if err := s.Store.CASMaterializedHead(ctx, materialized.ID, captured.ID); err != nil {
		return &captured, paths, &CommittedStateError{State: captured, Err: fmt.Errorf("record captured visible root: %w", err)}
	}
	return &captured, paths, nil
}

func withoutPortableHopRecords(paths []string) []string {
	filtered := make([]string, 0, len(paths))
	for _, path := range paths {
		if strings.HasPrefix(path, ".hop/records/") {
			continue
		}
		filtered = append(filtered, path)
	}
	return filtered
}

func (s *Service) attachAutomaticPush(ctx context.Context, result *AcceptResult) {
	pushed, configured, err := s.publishAccepted(ctx, result.State)
	if err != nil {
		result.Warnings = append(result.Warnings, "accepted state is local, but automatic push failed and remains pending: "+err.Error())
		return
	}
	if configured {
		result.RemotePush = &pushed
	}
}

func (s *Service) publishAccepted(ctx context.Context, accepted State) (RemotePushResult, bool, error) {
	remote, ref, configured, targetErr := s.Repo.PublicationTarget(ctx)
	now := time.Now().UTC()
	publication := PublicationStatus{
		AcceptedStateID: accepted.ID,
		Commit:          accepted.GitCommit,
		Status:          "not_configured",
		Remote:          remote,
		Ref:             ref,
		AttemptedAt:     &now,
	}
	if existing, found, readErr := s.Store.PublicationForState(ctx, accepted.ID); readErr != nil {
		return RemotePushResult{}, configured, readErr
	} else if found && existing.Commit == accepted.GitCommit {
		publication.RemoteTip = existing.RemoteTip
	}
	if targetErr != nil {
		message, _ := RedactPromptSecrets(targetErr.Error())
		publication.Status = "failed"
		publication.ErrorCategory = "configuration"
		publication.ErrorMessage = message
		publication.Retryable = false
		if err := s.Store.PutPublication(ctx, publication); err != nil {
			return RemotePushResult{}, false, err
		}
		return RemotePushResult{}, false, errors.New(message)
	}
	if !configured {
		if err := s.Store.PutPublication(ctx, publication); err != nil {
			return RemotePushResult{}, false, err
		}
		return RemotePushResult{}, false, nil
	}
	publication.Status = "pending"
	publication.Retryable = true
	if err := s.Store.PutPublication(ctx, publication); err != nil {
		return RemotePushResult{}, true, err
	}
	pushCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	result, _, pushErr := s.Repo.PushAccepted(pushCtx, accepted.GitCommit)
	if pushErr != nil {
		message, _ := RedactPromptSecrets(pushErr.Error())
		publication.Status = "failed"
		publication.ErrorCategory, publication.Retryable = publicationFailure(pushErr)
		publication.ErrorMessage = message
		var diverged *RemoteDivergedError
		if errors.As(pushErr, &diverged) {
			publication.RemoteTip = diverged.RemoteTip
		}
		if err := s.Store.PutPublication(ctx, publication); err != nil {
			return RemotePushResult{}, true, fmt.Errorf("%s; additionally could not record publication failure: %w", message, err)
		}
		return RemotePushResult{}, true, errors.New(message)
	}
	publication.Status = "current"
	publication.RemoteTip = accepted.GitCommit
	publication.ErrorCategory = ""
	publication.ErrorMessage = ""
	publication.Retryable = false
	if err := s.Store.PutPublication(ctx, publication); err != nil {
		return result, true, err
	}
	return result, true, nil
}

func (s *Service) recordPendingPublication(ctx context.Context, accepted State, observedRemoteTip string) error {
	remote, ref, configured, err := s.Repo.PublicationTarget(ctx)
	now := time.Now().UTC()
	publication := PublicationStatus{
		AcceptedStateID: accepted.ID,
		Commit:          accepted.GitCommit,
		Status:          "not_configured",
		Remote:          remote,
		Ref:             ref,
		RemoteTip:       observedRemoteTip,
		AttemptedAt:     &now,
	}
	if err != nil {
		message, _ := RedactPromptSecrets(err.Error())
		publication.Status = "failed"
		publication.ErrorCategory = "configuration"
		publication.ErrorMessage = message
	} else if configured {
		publication.Status = "pending"
		publication.Retryable = true
	}
	return s.Store.PutPublication(ctx, publication)
}

func publicationFailure(err error) (string, bool) {
	var diverged *RemoteDivergedError
	if errors.As(err, &diverged) {
		return "diverged", false
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return "timeout", true
	}
	message := strings.ToLower(err.Error())
	for _, marker := range []string{"authentication", "authorization", "permission denied", "credentials", "401", "403"} {
		if strings.Contains(message, marker) {
			return "authentication", true
		}
	}
	for _, marker := range []string{"could not resolve", "connection", "network", "timed out", "timeout"} {
		if strings.Contains(message, marker) {
			return "network", true
		}
	}
	return "git", true
}

// Sync projects the current accepted tree into a visible root that still
// matches its durable materialized head. It is useful after controller-only accepts or
// when upgrading a project created before automatic materialization existed.
func (s *Service) Sync(ctx context.Context) (SyncResult, error) {
	release, err := acquireProjectLock(ctx, s.Root, "accept")
	if err != nil {
		return SyncResult{}, err
	}
	defer release()
	result, err := s.syncLocked(ctx)
	if err != nil {
		return SyncResult{}, err
	}
	promptSync, syncErr := s.SyncPromptHistory(ctx)
	if syncErr != nil && !errors.Is(syncErr, ErrNotAuthenticated) {
		message, _ := RedactPromptSecrets(syncErr.Error())
		result.Warnings = append(result.Warnings, "private prompt history remains local; cloud sync failed: "+message)
	} else {
		result.PromptSync = promptSync
	}
	return result, nil
}

// Refresh prepares an agent-editable three-way conflict tree in the original
// attempt workspace. It appends an internal reconciliation prompt so the agent
// can resolve genuine conflicts without asking the user to coordinate them.
func (s *Service) Refresh(ctx context.Context, proposalID string) (RefreshResult, error) {
	release, err := acquireProjectLock(ctx, s.Root, "accept")
	if err != nil {
		return RefreshResult{}, err
	}
	defer release()

	proposal, err := s.Store.GetState(ctx, proposalID)
	if err != nil {
		return RefreshResult{}, err
	}
	if proposal.Kind != StateProposal {
		return RefreshResult{}, fmt.Errorf("state %s is %s, not a proposal", proposal.ID, proposal.Kind)
	}
	sourceAttempt, err := s.Store.GetAttempt(ctx, proposal.AttemptID)
	if err != nil {
		return RefreshResult{}, err
	}
	if sourceAttempt.HeadStateID != proposal.ID {
		return RefreshResult{}, &HeadChangedError{
			Scope: "attempt", Expected: proposal.ID, Actual: sourceAttempt.HeadStateID,
		}
	}
	task, err := s.Store.GetTask(ctx, proposal.TaskID)
	if err != nil {
		return RefreshResult{}, err
	}
	current, err := s.Store.AcceptedHead(ctx)
	if err != nil {
		return RefreshResult{}, err
	}
	if existing, exists, err := s.Store.ReconciliationPrompt(ctx, proposal.ID, current.ID); err != nil {
		return RefreshResult{}, err
	} else if exists {
		if err := s.Store.RetargetAgentSessions(ctx, sourceAttempt.Agent, sourceAttempt.ID, proposal.ID, existing.ID); err != nil {
			return RefreshResult{}, err
		}
		return s.reconciliationResult(ctx, existing, task, proposal, current, true)
	}
	baseStateID := proposal.CanonicalAnchorID
	if baseStateID == "" {
		baseStateID = sourceAttempt.BaseStateID
	}
	base, err := s.Store.GetState(ctx, baseStateID)
	if err != nil {
		return RefreshResult{}, err
	}
	conflictTree, conflicts, err := s.Repo.ComposeTrees(ctx, base.GitCommit, current.GitCommit, proposal.GitCommit)
	if err != nil {
		return RefreshResult{}, err
	}
	metadata := reconciliationMetadata{Conflicts: conflicts}
	conflictParents := []string{current.GitCommit, proposal.GitCommit}
	target := fmt.Sprintf("accepted state %s (%s)", current.ID, current.Summary)
	if len(conflicts) == 0 {
		incorporatedRemoteTip, metadataErr := s.proposalRemoteReconciliationTip(ctx, proposal)
		if metadataErr != nil {
			return RefreshResult{}, metadataErr
		}
		candidateParents := []string{current.GitCommit, proposal.GitCommit}
		candidate, candidateErr := s.Repo.CommitTree(ctx, conflictTree, candidateParents, "Hop remote reconciliation candidate\n")
		if candidateErr != nil {
			return RefreshResult{}, candidateErr
		}
		remoteCtx, cancelRemote := context.WithTimeout(ctx, 30*time.Second)
		remoteTip, _, remoteExists, remoteErr := s.Repo.FetchPushTip(remoteCtx)
		cancelRemote()
		if remoteErr != nil {
			return RefreshResult{}, remoteErr
		}
		if remoteExists && remoteTip != current.GitCommit && remoteTip != incorporatedRemoteTip {
			mergeBase, mergeErr := s.Repo.MergeBase(ctx, candidate, remoteTip)
			if mergeErr != nil {
				return RefreshResult{}, mergeErr
			}
			conflictTree, conflicts, err = s.Repo.ComposeTrees(ctx, mergeBase, remoteTip, candidate)
			if err != nil {
				return RefreshResult{}, err
			}
			if len(conflicts) > 0 {
				metadata = reconciliationMetadata{Conflicts: conflicts, RemoteTip: remoteTip}
				conflictParents = []string{remoteTip, candidate}
				target = fmt.Sprintf("remote branch commit %s while retaining accepted state %s (%s)", remoteTip, current.ID, current.Summary)
			}
		}
	}
	if len(conflicts) == 0 {
		return RefreshResult{}, fmt.Errorf("proposal %s now merges cleanly; retry hop land", proposal.ID)
	}
	commit, err := s.Repo.CommitTree(ctx, conflictTree, conflictParents,
		fmt.Sprintf("Reconcile %s against %s\n", proposal.ID, target))
	if err != nil {
		return RefreshResult{}, err
	}
	summary, err := encodeReconciliationMetadata(metadata)
	if err != nil {
		return RefreshResult{}, err
	}
	instruction := fmt.Sprintf(
		"Resolve proposal %s (%s) against %s. Preserve both compatible intents. Inspect every input commit/state for structural, delete/rename, mode, symlink, or binary conflicts that may have no text markers; resolve every conflict intentionally, remove all merge markers, run hop check, propose the result, and land it without asking the user to coordinate the merge. Conflict candidates: %s",
		proposal.ID, proposal.Summary, target, strings.Join(conflicts, ", "))
	instruction, _ = RedactPromptSecrets(instruction)
	attemptID := newID("at")
	workspace := filepath.Join(s.Root, ".hop", "workspaces", attemptID)
	reconciliationAttempt := Attempt{
		ID:          attemptID,
		TaskID:      proposal.TaskID,
		Agent:       sourceAttempt.Agent,
		Workspace:   workspace,
		BaseStateID: current.ID,
		Status:      "reconciling",
		CreatedAt:   time.Now().UTC(),
	}
	parents := canonicalizeParents([]Parent{
		{StateID: proposal.ID, Role: "run_parent", Order: 0},
		{StateID: current.ID, Role: "canonical_anchor", Order: 1},
		{StateID: proposal.ID, Role: "reconciliation_source", Order: 2},
	})
	prompt := State{
		ID:                newID("p"),
		Kind:              StatePrompt,
		TaskID:            proposal.TaskID,
		AttemptID:         reconciliationAttempt.ID,
		CanonicalAnchorID: current.ID,
		SourceTree:        conflictTree,
		GitCommit:         commit,
		Prompt:            instruction,
		Summary:           summary,
		Agent:             reconciliationAttempt.Agent,
		CreatedAt:         time.Now().UTC(),
		Parents:           parents,
	}
	prompt.Digest, err = digestState(prompt, parents)
	if err != nil {
		return RefreshResult{}, err
	}
	if err := s.pinState(ctx, prompt); err != nil {
		return RefreshResult{}, err
	}
	promptPinned := true
	defer func() {
		if promptPinned {
			_ = s.Repo.DeleteRef(context.Background(), "refs/hop/states/"+prompt.ID, prompt.GitCommit)
		}
	}()
	if err := os.MkdirAll(filepath.Dir(workspace), 0o755); err != nil {
		return RefreshResult{}, fmt.Errorf("create reconciliation workspace directory: %w", err)
	}
	if _, err := s.Repo.AddDetachedWorktree(ctx, workspace, prompt.GitCommit); err != nil {
		_ = s.Repo.RemoveWorktree(context.Background(), workspace, true)
		return RefreshResult{}, fmt.Errorf("create reconciliation workspace: %w", err)
	}
	workspaceInstalled := true
	defer func() {
		if workspaceInstalled {
			_ = s.Repo.RemoveWorktree(context.Background(), workspace, true)
		}
	}()
	reconciliationAttempt, prompt, err = s.Store.CreateAttemptPrompt(
		ctx, reconciliationAttempt, prompt, parents, sourceAttempt.ID, proposal.ID,
	)
	if err != nil {
		return RefreshResult{}, err
	}
	workspaceInstalled = false
	promptPinned = false
	task.Status = "reconciling"
	return s.reconciliationResult(ctx, prompt, task, proposal, current, false)
}

func (s *Service) reconciliationResult(
	ctx context.Context,
	prompt State,
	task Task,
	proposal State,
	current State,
	reused bool,
) (RefreshResult, error) {
	metadata, ok := decodeReconciliationMetadata(prompt.Summary)
	if !ok || len(metadata.Conflicts) == 0 {
		return RefreshResult{}, fmt.Errorf("reconciliation prompt %s has no conflict metadata", prompt.ID)
	}
	attempt, err := s.Store.GetAttempt(ctx, prompt.AttemptID)
	if err != nil {
		return RefreshResult{}, err
	}
	if _, err := OpenRepository(attempt.Workspace); err != nil {
		return RefreshResult{}, fmt.Errorf("open prepared reconciliation workspace: %w", err)
	}
	task, err = s.Store.GetTask(ctx, task.ID)
	if err != nil {
		return RefreshResult{}, err
	}
	return RefreshResult{
		Prompt:       prompt,
		Task:         task,
		Attempt:      attempt,
		Proposal:     proposal,
		AcceptedHead: current,
		RemoteTip:    metadata.RemoteTip,
		Workspace:    attempt.Workspace,
		Deliver:      s.deliveryEnvironment(prompt, attempt),
		ConflictTree: prompt.SourceTree,
		Conflicts:    metadata.Conflicts,
		Reused:       reused,
	}, nil
}

type reconciliationMetadata struct {
	Conflicts []string `json:"conflicts"`
	RemoteTip string   `json:"remote_tip,omitempty"`
}

func encodeReconciliationMetadata(metadata reconciliationMetadata) (string, error) {
	encoded, err := json.Marshal(metadata)
	if err != nil {
		return "", fmt.Errorf("encode reconciliation conflicts: %w", err)
	}
	return reconciliationSummaryPrefix + string(encoded), nil
}

func decodeReconciliationConflicts(summary string) ([]string, bool) {
	metadata, ok := decodeReconciliationMetadata(summary)
	return metadata.Conflicts, ok
}

func decodeReconciliationMetadata(summary string) (reconciliationMetadata, bool) {
	if !strings.HasPrefix(summary, reconciliationSummaryPrefix) {
		return reconciliationMetadata{}, false
	}
	payload := []byte(strings.TrimPrefix(summary, reconciliationSummaryPrefix))
	var metadata reconciliationMetadata
	if err := json.Unmarshal(payload, &metadata); err == nil && len(metadata.Conflicts) > 0 {
		return metadata, true
	}
	// Reconciliation prompts created before remote-conflict support stored a
	// bare path array. Continue accepting those durable states.
	var paths []string
	if err := json.Unmarshal(payload, &paths); err != nil || len(paths) == 0 {
		return reconciliationMetadata{}, false
	}
	return reconciliationMetadata{Conflicts: paths}, true
}

func (s *Service) proposalRemoteReconciliationTip(ctx context.Context, proposal State) (string, error) {
	prompt, reconciliation, err := s.Store.ReconciliationPromptForAttempt(ctx, proposal.AttemptID)
	if err != nil || !reconciliation {
		return "", err
	}
	metadata, ok := decodeReconciliationMetadata(prompt.Summary)
	if !ok {
		return "", fmt.Errorf("reconciliation prompt %s has invalid conflict metadata", prompt.ID)
	}
	return metadata.RemoteTip, nil
}

func appendUniqueCommit(parents []string, commit string) []string {
	if commit == "" {
		return parents
	}
	for _, parent := range parents {
		if parent == commit {
			return parents
		}
	}
	return append(parents, commit)
}

func unresolvedReconciliationMarkers(ctx context.Context, repo *Repository, tree string, conflicts []string) ([]string, error) {
	args := []string{
		"grep", "-z", "-l", "-I",
		"-e", "^<<<<<<< ",
		"-e", "^>>>>>>> ",
		tree, "--",
	}
	args = append(args, conflicts...)
	output, err := repo.run(ctx, []string{"GIT_LITERAL_PATHSPECS=1", "GIT_OPTIONAL_LOCKS=0"}, nil, args...)
	if err != nil {
		if gitExitCode(err) == 1 {
			return nil, nil
		}
		return nil, fmt.Errorf("inspect reconciliation markers: %w", err)
	}
	unresolved := splitNull([]byte(output))
	sort.Strings(unresolved)
	return unresolved, nil
}

func (s *Service) syncLocked(ctx context.Context) (SyncResult, error) {
	current, err := s.Store.AcceptedHead(ctx)
	if err != nil {
		return SyncResult{}, err
	}
	rootBase, err := s.prepareRootMaterialization(ctx, current.SourceTree)
	if err != nil {
		return SyncResult{}, err
	}
	changed := rootBase.SourceTree != current.SourceTree
	if err := s.Repo.MaterializeTree(ctx, rootBase.SourceTree, current.SourceTree); err != nil {
		return SyncResult{}, err
	}
	if err := s.Store.CASMaterializedHead(ctx, rootBase.ID, current.ID); err != nil {
		return SyncResult{}, fmt.Errorf("record visible root synchronization: %w", err)
	}
	return SyncResult{
		State:     current,
		Root:      s.Root,
		FromState: rootBase.ID,
		Changed:   changed,
	}, nil
}

func (s *Service) prepareRootMaterialization(ctx context.Context, targetTree string) (State, error) {
	rootState, matches, err := s.visibleRootState(ctx)
	if err != nil {
		return State{}, err
	}
	if !matches {
		materialized, materializedErr := s.Store.MaterializedHead(ctx)
		if materializedErr != nil {
			return State{}, materializedErr
		}
		actual, snapshotErr := s.Repo.WorktreeTree(ctx, materialized.SourceTree)
		if snapshotErr != nil {
			return State{}, snapshotErr
		}
		paths, pathErr := s.Repo.ChangedPaths(ctx, materialized.SourceTree, actual)
		if pathErr != nil {
			return State{}, pathErr
		}
		if len(paths) == 0 {
			paths = []string{"."}
		}
		return State{}, &RootConflictError{Paths: paths}
	}
	if err := s.Repo.CheckIndexSafe(ctx, rootState.SourceTree); err != nil {
		return State{}, err
	}
	materialized, err := s.Store.MaterializedHead(ctx)
	if err != nil {
		return State{}, err
	}
	if rootState.ID != materialized.ID {
		if err := s.Store.CASMaterializedHead(ctx, materialized.ID, rootState.ID); err != nil {
			return State{}, fmt.Errorf("repair visible root projection marker: %w", err)
		}
	}
	conflicts, err := s.Repo.MaterializationConflicts(ctx, rootState.SourceTree, targetTree)
	if err != nil {
		return State{}, err
	}
	if len(conflicts) > 0 {
		return State{}, &RootConflictError{
			Paths:  conflicts,
			Reason: "visible project root contains ignored or untracked paths that landing would overwrite",
		}
	}
	return rootState, nil
}

func (s *Service) visibleRootState(ctx context.Context) (State, bool, error) {
	materialized, err := s.Store.MaterializedHead(ctx)
	if err != nil {
		return State{}, false, err
	}
	actualTree, err := s.Repo.WorktreeTree(ctx, materialized.SourceTree)
	if err != nil {
		return State{}, false, err
	}
	if actualTree == materialized.SourceTree {
		return materialized, true, nil
	}
	accepted, err := s.Store.AcceptedHead(ctx)
	if err != nil {
		return State{}, false, err
	}
	if accepted.ID == materialized.ID {
		return State{}, false, nil
	}
	actualTree, err = s.Repo.WorktreeTree(ctx, accepted.SourceTree)
	if err != nil {
		return State{}, false, err
	}
	if actualTree == accepted.SourceTree {
		return accepted, true, nil
	}
	return State{}, false, nil
}

func (s *Service) Undo(ctx context.Context) (State, error) {
	release, err := acquireProjectLock(ctx, s.Root, "accept")
	if err != nil {
		return State{}, err
	}
	defer release()
	current, err := s.Store.AcceptedHead(ctx)
	if err != nil {
		return State{}, err
	}
	canonicalParent, err := s.Store.ParentByRole(ctx, current.ID, "canonical_parent")
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return State{}, fmt.Errorf("initial accepted state cannot be undone")
		}
		return State{}, err
	}
	previous, err := s.Store.GetState(ctx, canonicalParent.StateID)
	if err != nil {
		return State{}, err
	}
	rootBase, err := s.prepareRootMaterialization(ctx, previous.SourceTree)
	if err != nil {
		return State{}, err
	}
	undoID := newID("a")
	commit, err := s.Repo.CommitTree(ctx, previous.SourceTree, []string{current.GitCommit}, fmt.Sprintf(
		"Undo %s\n\nHop-State: %s\nHop-Reverts: %s\n", current.ID, undoID, current.ID))
	if err != nil {
		return State{}, err
	}
	parents := canonicalizeParents([]Parent{
		{StateID: current.ID, Role: "canonical_parent", Order: 0},
		{StateID: current.ID, Role: "reverts", Order: 1},
		{StateID: previous.ID, Role: "restores", Order: 2},
	})
	provenance, err := s.verifyAcceptance(ctx, current, previous.SourceTree, []ProvenanceInput{{
		Role: "explicit_undo", StateID: previous.ID, BaseTree: current.SourceTree, CandidateTree: previous.SourceTree,
	}}, "undo")
	if err != nil {
		return State{}, err
	}
	undo := State{
		ID:                undoID,
		Kind:              StateAccepted,
		CanonicalAnchorID: current.ID,
		SourceTree:        previous.SourceTree,
		GitCommit:         commit,
		Summary:           "Undo " + current.ID,
		Agent:             "hop",
		Provenance:        provenance,
		CreatedAt:         time.Now().UTC(),
		Parents:           parents,
	}
	undo.Digest, err = digestState(undo, parents)
	if err != nil {
		return State{}, err
	}
	if err := s.pinState(ctx, undo); err != nil {
		return State{}, err
	}
	undo, err = s.Store.CASAccept(ctx, current.ID, undo, parents)
	if err != nil {
		return State{}, mapHeadError(err)
	}
	var postCommitErrors []error
	if err := s.recordPendingPublication(ctx, undo, ""); err != nil {
		postCommitErrors = append(postCommitErrors, fmt.Errorf("record pending publication: %w", err))
	}
	if err := s.Repo.UpdateHiddenRef(ctx, acceptedRef, undo.GitCommit); err != nil {
		postCommitErrors = append(postCommitErrors, fmt.Errorf("repair refs/hop/accepted: %w", err))
	}
	if err := s.Repo.MaterializeTree(ctx, rootBase.SourceTree, undo.SourceTree); err != nil {
		postCommitErrors = append(postCommitErrors, fmt.Errorf("visible root %s was not synchronized: %w", s.Root, err))
	} else if err := s.Store.CASMaterializedHead(ctx, rootBase.ID, undo.ID); err != nil {
		postCommitErrors = append(postCommitErrors, fmt.Errorf("record visible root synchronization: %w", err))
	}
	if _, _, err := s.publishAccepted(ctx, undo); err != nil {
		postCommitErrors = append(postCommitErrors, fmt.Errorf("automatic push failed and remains pending: %w", err))
	}
	if len(postCommitErrors) > 0 {
		return undo, &CommittedStateError{State: undo, Err: errors.Join(postCommitErrors...)}
	}
	return undo, nil
}

// recordValidationFailure turns a failed final-tree check into an immutable
// descendant state when the proposal is still the attempt head. If a follow-up
// raced with validation, the caller retains the validation ref and tree-bound
// evidence without rewriting the newer attempt lineage.
func (s *Service) recordValidationFailure(
	ctx context.Context,
	proposal State,
	current State,
	commit string,
	tree string,
	command []string,
) (State, bool) {
	attempt, err := s.Store.GetAttempt(ctx, proposal.AttemptID)
	if err != nil || attempt.HeadStateID != proposal.ID {
		return State{}, false
	}
	parents := canonicalizeParents([]Parent{
		{StateID: proposal.ID, Role: "run_parent", Order: 0},
		{StateID: current.ID, Role: "integration_parent", Order: 1},
	})
	storedCommand, _ := redactSecretStrings(command)
	failed := State{
		ID:                newID("f"),
		Kind:              StateFailed,
		TaskID:            proposal.TaskID,
		AttemptID:         proposal.AttemptID,
		CanonicalAnchorID: current.ID,
		SourceTree:        tree,
		GitCommit:         commit,
		Summary:           "Final validation failed: " + shellQuote(storedCommand),
		Agent:             "hop",
		CreatedAt:         time.Now().UTC(),
		Parents:           parents,
	}
	failed.Digest, err = digestState(failed, parents)
	if err != nil {
		return State{}, false
	}
	if err := s.pinState(ctx, failed); err != nil {
		return State{}, false
	}
	failed, err = s.Store.AppendState(ctx, failed, parents, proposal.ID)
	if err != nil {
		return State{}, false
	}
	_ = s.Store.UpdateAttemptStatus(ctx, proposal.AttemptID, "changes_requested")
	_ = s.Store.UpdateTaskStatus(ctx, proposal.TaskID, "changes_requested")
	return failed, true
}

func (s *Service) State(ctx context.Context, id string) (State, error) {
	return s.Store.GetState(ctx, id)
}

func (s *Service) EnvironmentForState(ctx context.Context, id string) (EnvironmentResult, error) {
	state, err := s.Store.GetState(ctx, id)
	if err != nil {
		return EnvironmentResult{}, err
	}
	if state.AttemptID == "" {
		return EnvironmentResult{}, fmt.Errorf("state %s does not belong to an agent attempt", id)
	}
	attempt, err := s.Store.GetAttempt(ctx, state.AttemptID)
	if err != nil {
		return EnvironmentResult{}, err
	}
	return EnvironmentResult{
		State:     state,
		Attempt:   attempt,
		Workspace: attempt.Workspace,
		Variables: map[string]string{
			"HOP_ROOT":       s.Root,
			"HOP_STATE_ID":   state.ID,
			"HOP_TASK_ID":    state.TaskID,
			"HOP_ATTEMPT_ID": attempt.ID,
			"HOP_WORKSPACE":  attempt.Workspace,
		},
	}, nil
}

func (s *Service) Graph(ctx context.Context) ([]GraphRow, error) {
	return s.Store.Graph(ctx, "")
}

func (s *Service) History(ctx context.Context) ([]State, error) {
	return s.Store.AcceptedHistory(ctx, 1000)
}

func (s *Service) Status(ctx context.Context) (Status, error) {
	status, err := s.Store.Status(ctx)
	if err != nil {
		return Status{}, err
	}
	status.AcceptedProvenance = "legacy_unverified"
	if status.AcceptedHead.Provenance == nil && status.AcceptedHead.CanonicalAnchorID == "" {
		status.AcceptedProvenance = "verified"
	} else if status.AcceptedHead.Provenance != nil {
		base, baseErr := s.Store.GetState(ctx, status.AcceptedHead.Provenance.BaseStateID)
		if baseErr == nil {
			if proofErr := s.verifyStoredProvenance(ctx, status.AcceptedHead, base); proofErr == nil {
				status.AcceptedProvenance = "verified"
			} else {
				status.AcceptedProvenance = "invalid"
				status.Warnings = append(status.Warnings, proofErr.Error())
			}
		} else {
			status.AcceptedProvenance = "invalid"
			status.Warnings = append(status.Warnings, "accepted-state provenance base cannot be loaded: "+baseErr.Error())
		}
	}
	rootState, matches, err := s.visibleRootState(ctx)
	if err != nil {
		return Status{}, err
	}
	if !matches {
		status.RootStatus = "diverged"
	} else if err := s.Repo.CheckIndexSafe(ctx, rootState.SourceTree); err != nil {
		var conflict *RootConflictError
		if errors.As(err, &conflict) {
			status.RootStatus = "diverged"
		} else {
			return Status{}, err
		}
	} else {
		status.RootStateID = rootState.ID
		if rootState.ID == status.AcceptedHead.ID {
			status.RootStatus = "synchronized"
		} else {
			status.RootStatus = "stale"
		}
	}
	status.Git, err = s.Repo.GitStatusForAccepted(ctx, status.AcceptedHead.GitCommit, status.AcceptedHead.SourceTree)
	if err != nil {
		return Status{}, err
	}
	publication, found, err := s.Store.PublicationForState(ctx, status.AcceptedHead.ID)
	if err != nil {
		return Status{}, err
	}
	if !found {
		publication = PublicationStatus{
			AcceptedStateID: status.AcceptedHead.ID,
			Commit:          status.AcceptedHead.GitCommit,
			Status:          "unknown",
		}
		if _, _, configured, targetErr := s.Repo.PublicationTarget(ctx); targetErr == nil && !configured {
			publication.Status = "not_configured"
		}
	}
	status.Publication = publication
	if publication.RemoteTip != "" {
		status.Git.LocalTrackingRefMayBeStale = status.Git.LocalTrackingTip != "" && status.Git.LocalTrackingTip != publication.RemoteTip
		status.Git.UpstreamTip = publication.RemoteTip
		status.Git.UpstreamObservation = "last_authoritative_remote_check"
		status.Git.UpstreamObservationMayBeStale = publication.Status != "current"
		status.Git.AcceptedAheadUpstream, status.Git.AcceptedBehindUpstream, err = s.Repo.DivergenceCounts(ctx, status.AcceptedHead.GitCommit, publication.RemoteTip)
		if err != nil {
			return Status{}, err
		}
	}
	if status.Git.ProjectionOnlyChanges {
		status.Warnings = append(status.Warnings, "raw Git status reflects Hop's accepted-tree projection over a stale local branch/index; it is not uncommitted user work")
	}
	if publication.Status == "failed" || publication.Status == "pending" || publication.Status == "unknown" {
		status.Warnings = append(status.Warnings, "accepted state publication is "+publication.Status+"; run hop push to retry when appropriate")
	}
	return status, nil
}

func (s *Service) Diff(ctx context.Context, stateID string) (string, error) {
	state, err := s.Store.GetState(ctx, stateID)
	if err != nil {
		return "", err
	}
	var base State
	if state.Kind == StateAccepted {
		parent, parentErr := s.Store.ParentByRole(ctx, state.ID, "canonical_parent")
		if errors.Is(parentErr, ErrNotFound) {
			return s.Repo.Diff(ctx, "", state.GitCommit)
		}
		if parentErr != nil {
			return "", parentErr
		}
		base, err = s.Store.GetState(ctx, parent.StateID)
	} else if state.AttemptID != "" {
		attempt, attemptErr := s.Store.GetAttempt(ctx, state.AttemptID)
		if attemptErr != nil {
			return "", attemptErr
		}
		base, err = s.Store.GetState(ctx, attempt.BaseStateID)
	} else {
		return s.Repo.Diff(ctx, "", state.GitCommit)
	}
	if err != nil {
		return "", err
	}
	return s.Repo.Diff(ctx, base.GitCommit, state.GitCommit)
}

func (s *Service) Doctor(ctx context.Context, repair bool) (DoctorReport, error) {
	if repair {
		if os.Getenv("HOP_ACCEPTANCE_LOCK_HELD") == "1" {
			return DoctorReport{}, fmt.Errorf("doctor --repair cannot run inside a final-state validation command")
		}
		release, err := acquireProjectLock(ctx, s.Root, "accept")
		if err != nil {
			return DoctorReport{}, err
		}
		defer release()
	}
	head, err := s.Store.AcceptedHead(ctx)
	if err != nil {
		return DoctorReport{}, err
	}
	report := DoctorReport{OK: true, AcceptedState: head.ID, AcceptedCommit: head.GitCommit}
	if head.Kind != StateAccepted {
		report.OK = false
		report.Problems = append(report.Problems, fmt.Sprintf("accepted head %s has kind %s", head.ID, head.Kind))
	}
	if err := s.Repo.Verify(ctx); err != nil {
		report.OK = false
		report.Problems = append(report.Problems, "Git connectivity check failed: "+err.Error())
	}
	rows, err := s.Store.Graph(ctx, "")
	if err != nil {
		return DoctorReport{}, err
	}
	for _, row := range rows {
		state := row.State
		if err := s.Repo.VerifyObjects(ctx, state.GitCommit, state.SourceTree); err != nil {
			report.OK = false
			report.Problems = append(report.Problems, fmt.Sprintf("state %s references missing Git data: %v", state.ID, err))
			continue
		}
		commitTree, treeErr := s.Repo.resolveTree(ctx, state.GitCommit)
		if treeErr != nil || commitTree != state.SourceTree {
			report.OK = false
			if treeErr != nil {
				report.Problems = append(report.Problems, fmt.Sprintf("state %s commit tree cannot be resolved: %v", state.ID, treeErr))
			} else {
				report.Problems = append(report.Problems, fmt.Sprintf("state %s records tree %s but commit resolves to %s", state.ID, state.SourceTree, commitTree))
			}
		}
		expectedDigest, digestErr := digestState(state, row.Parents)
		if digestErr != nil || expectedDigest != state.Digest {
			report.OK = false
			if digestErr != nil {
				report.Problems = append(report.Problems, fmt.Sprintf("state %s digest cannot be recomputed: %v", state.ID, digestErr))
			} else {
				report.Problems = append(report.Problems, fmt.Sprintf("state %s digest mismatch", state.ID))
			}
		}
		if problem := stateParentProblem(state, row.Parents); problem != "" {
			report.OK = false
			report.Problems = append(report.Problems, problem)
		}
		if state.Provenance == nil {
			needsProof := state.Kind == StateCheckpoint || state.Kind == StateProposal ||
				(state.Kind == StateAccepted && !(state.CanonicalAnchorID == "" && len(row.Parents) == 0))
			if needsProof {
				report.Warnings = append(report.Warnings, fmt.Sprintf("state %s predates durable authorization manifests and is legacy-unverified", state.ID))
			}
		} else {
			base, baseErr := s.Store.GetState(ctx, state.Provenance.BaseStateID)
			if baseErr != nil {
				report.OK = false
				report.Problems = append(report.Problems, fmt.Sprintf("state %s provenance base cannot be loaded: %v", state.ID, baseErr))
			} else if proofErr := s.verifyStoredProvenance(ctx, state, base); proofErr != nil {
				report.OK = false
				report.Problems = append(report.Problems, fmt.Sprintf("state %s provenance is invalid: %v", state.ID, proofErr))
			}
		}
		refName := "states/" + state.ID
		refCommit, exists, readErr := s.Repo.ReadHiddenRef(ctx, refName)
		if readErr != nil {
			return DoctorReport{}, readErr
		}
		if !exists || refCommit != state.GitCommit {
			report.OK = false
			report.Problems = append(report.Problems, fmt.Sprintf("%s does not pin state %s", refName, state.ID))
			if repair {
				if err := s.Repo.UpdateHiddenRef(ctx, refName, state.GitCommit); err != nil {
					return DoctorReport{}, err
				}
				report.Repaired = true
			}
		}
	}
	refCommit, exists, err := s.Repo.ReadHiddenRef(ctx, acceptedRef)
	if err != nil {
		return DoctorReport{}, err
	}
	if exists {
		report.RefCommit = refCommit
	}
	if !exists || refCommit != head.GitCommit {
		report.OK = false
		report.Problems = append(report.Problems, "refs/hop/accepted does not match the SQLite accepted head")
		if repair {
			if err := s.Repo.UpdateHiddenRef(ctx, acceptedRef, head.GitCommit); err != nil {
				return DoctorReport{}, err
			}
			report.RefCommit = head.GitCommit
			report.Repaired = true
		}
	}
	status, statusErr := s.Status(ctx)
	if statusErr != nil {
		return DoctorReport{}, statusErr
	}
	if status.Git.ProjectionOnlyChanges {
		report.Warnings = append(report.Warnings, "raw Git status is projection-only because the visible accepted tree is newer than the unchanged local branch/index")
	}
	if status.Publication.Status == "failed" || status.Publication.Status == "pending" || status.Publication.Status == "unknown" {
		report.Warnings = append(report.Warnings, "accepted state publication is "+status.Publication.Status)
	}
	if repair && report.Repaired {
		verified, verifyErr := s.Doctor(ctx, false)
		if verifyErr != nil {
			return DoctorReport{}, verifyErr
		}
		verified.Repaired = true
		return verified, nil
	}
	return report, nil
}

func stateParentProblem(state State, parents []Parent) string {
	hasRole := func(role string) bool {
		for _, parent := range parents {
			if parent.Role == role {
				return true
			}
		}
		return false
	}
	switch state.Kind {
	case StatePrompt:
		if state.TaskID == "" || state.AttemptID == "" || !hasRole("run_parent") || !hasRole("canonical_anchor") {
			return fmt.Sprintf("prompt state %s is missing task, attempt, run-parent, or canonical-anchor provenance", state.ID)
		}
	case StateCheckpoint, StateProposal, StateFailed, StateCancelled:
		if state.TaskID == "" || state.AttemptID == "" || !hasRole("run_parent") {
			return fmt.Sprintf("%s state %s is missing task, attempt, or run-parent provenance", state.Kind, state.ID)
		}
	case StateAccepted:
		isInitial := state.CanonicalAnchorID == "" && len(parents) == 0
		if !isInitial && !hasRole("canonical_parent") {
			return fmt.Sprintf("accepted state %s is missing its canonical parent", state.ID)
		}
	}
	return ""
}

func (s *Service) pinState(ctx context.Context, state State) error {
	return s.Repo.UpdateHiddenRef(ctx, "states/"+state.ID, state.GitCommit)
}

// reconcileDerivedRefs repairs the small crash window between SQLite commits
// and derived Git-ref updates. SQLite is authoritative, but every referenced
// commit is pinned before normal operation resumes so Git GC cannot discard it.
func (s *Service) reconcileDerivedRefs(ctx context.Context, reconcileAccepted bool) error {
	rows, err := s.Store.Graph(ctx, "")
	if err != nil {
		return err
	}
	refs, err := s.Repo.ListHiddenRefs(ctx)
	if err != nil {
		return err
	}
	for _, row := range rows {
		state := row.State
		name := "states/" + state.ID
		if refs[name] != state.GitCommit {
			if err := s.Repo.VerifyObjects(ctx, state.GitCommit, state.SourceTree); err != nil {
				return fmt.Errorf("state %s references unavailable Git data: %w", state.ID, err)
			}
			if err := s.Repo.UpdateHiddenRef(ctx, name, state.GitCommit); err != nil {
				return fmt.Errorf("repair Git pin for state %s: %w", state.ID, err)
			}
		}
	}
	if !reconcileAccepted {
		return nil
	}
	release, err := acquireProjectLock(ctx, s.Root, "accept")
	if err != nil {
		return err
	}
	defer release()
	head, err := s.Store.AcceptedHead(ctx)
	if err != nil {
		return err
	}
	if refs[acceptedRef] != head.GitCommit {
		if err := s.Repo.UpdateHiddenRef(ctx, acceptedRef, head.GitCommit); err != nil {
			return fmt.Errorf("repair accepted Git ref: %w", err)
		}
	}
	return nil
}

func (s *Service) deliveryEnvironment(state State, attempt Attempt) []string {
	return []string{
		"HOP_ROOT=" + s.Root,
		"HOP_STATE_ID=" + state.ID,
		"HOP_TASK_ID=" + state.TaskID,
		"HOP_ATTEMPT_ID=" + attempt.ID,
		"HOP_WORKSPACE=" + attempt.Workspace,
	}
}

func promptTitle(prompt string) string {
	prompt = strings.TrimSpace(prompt)
	if newline := strings.IndexByte(prompt, '\n'); newline >= 0 {
		prompt = prompt[:newline]
	}
	if len(prompt) > 100 {
		prompt = prompt[:97] + "..."
	}
	return prompt
}

func intersectPaths(left, right []string) []string {
	rightSet := make(map[string]struct{}, len(right))
	for _, path := range right {
		rightSet[path] = struct{}{}
	}
	var intersection []string
	for _, path := range left {
		if _, ok := rightSet[path]; ok {
			intersection = append(intersection, path)
		}
	}
	sort.Strings(intersection)
	return intersection
}

func mapHeadError(err error) error {
	var changed *HeadChangedError
	if errors.As(err, &changed) {
		return &StaleHeadError{Expected: changed.Expected, Actual: changed.Actual}
	}
	return err
}
