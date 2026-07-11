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
)

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
		if !strings.HasPrefix(filepath.ToSlash(trackedPath), ".hop/records/") {
			return nil, State{}, fmt.Errorf("cannot initialize Hop: local .hop runtime path is already tracked (for example %s)", trackedPath)
		}
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
	accepted, exists, err := s.Store.AcceptedForTask(ctx, state.TaskID)
	if err != nil {
		return false, err
	}
	if !exists {
		return true, nil
	}
	attempt, err := s.Store.GetAttempt(ctx, state.AttemptID)
	if err != nil {
		return false, err
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

func (s *Service) RunCheck(ctx context.Context, stateID string, argv []string) (Check, error) {
	state, err := s.Store.GetState(ctx, stateID)
	if err != nil {
		return Check{}, err
	}
	if state.AttemptID != "" {
		attempt, err := s.Store.GetAttempt(ctx, state.AttemptID)
		if err != nil {
			return Check{}, err
		}
		if _, err := s.ExportPromptLedger(ctx, attempt.Workspace, attempt.ID); err != nil {
			return Check{}, err
		}
	}
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
	task, err := s.Store.GetTask(ctx, attempt.TaskID)
	if err != nil {
		return ProposalResult{}, err
	}
	reconciliationPrompt, reconciliation, err := s.Store.ReconciliationPromptForAttempt(ctx, attempt.ID)
	if err != nil {
		return ProposalResult{}, err
	}
	var reconciliationConflicts []string
	if reconciliation {
		conflicts, ok := decodeReconciliationConflicts(reconciliationPrompt.Summary)
		if !ok {
			return ProposalResult{}, fmt.Errorf("reconciliation prompt %s has invalid conflict metadata", reconciliationPrompt.ID)
		}
		reconciliationConflicts = conflicts
	}
	workspaceRepo, err := OpenRepository(attempt.Workspace)
	if err != nil {
		return ProposalResult{}, err
	}
	if _, err := s.ExportPromptLedger(ctx, attempt.Workspace, attempt.ID); err != nil {
		return ProposalResult{}, err
	}
	commit, tree, err := workspaceRepo.Snapshot(ctx, "hop: proposal\n")
	if err != nil {
		return ProposalResult{}, err
	}
	if reconciliation {
		unresolved, err := unresolvedReconciliationMarkers(ctx, workspaceRepo, tree, reconciliationConflicts)
		if err != nil {
			return ProposalResult{}, err
		}
		if len(unresolved) > 0 {
			return ProposalResult{}, fmt.Errorf("reconciliation still contains merge markers in: %s", strings.Join(unresolved, ", "))
		}
	}
	checks, err := s.Store.ListChecks(ctx, attempt.ID, tree)
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
	if strings.TrimSpace(summary) == "" {
		summary = task.Title
	}
	summary, _ = RedactPromptSecrets(summary)
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
	return ProposalResult{Proposal: proposal, Checks: checks}, nil
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
	pushCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	result, configured, err := s.Repo.PushAccepted(pushCtx, accepted.GitCommit)
	if err != nil {
		message, _ := RedactPromptSecrets(err.Error())
		return RemotePushResult{}, errors.New(message)
	}
	if !configured {
		return RemotePushResult{}, errors.New("hop: no unambiguous Git remote branch is configured for automatic push")
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
		result := AcceptResult{State: existing}
		if materialize {
			if _, err := s.syncLocked(ctx); err != nil {
				return result, &CommittedStateError{State: existing, Err: fmt.Errorf("repair visible root after prior acceptance: %w", err)}
			}
			result.MaterializedRoot = s.Root
		}
		s.attachAutomaticPush(ctx, &result)
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
	baseStateID := proposal.CanonicalAnchorID
	if baseStateID == "" {
		baseStateID = attempt.BaseStateID
	}
	base, err := s.Store.GetState(ctx, baseStateID)
	if err != nil {
		return AcceptResult{}, err
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

	acceptedID := newID("a")
	message := proposal.Summary
	if message == "" {
		message = "Accept " + proposal.ID
	}
	message += fmt.Sprintf("\n\nHop-State: %s\nHop-Proposal: %s\nHop-Task: %s\nHop-Attempt: %s\n", acceptedID, proposal.ID, proposal.TaskID, proposal.AttemptID)
	commit, err := s.Repo.CommitTree(ctx, finalTree, []string{current.GitCommit}, message)
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
	if validationRef != "" {
		_ = s.Repo.DeleteRef(ctx, "refs/hop/"+validationRef, commit)
		validationRef = ""
	}
	var warnings []string
	if err := s.Repo.UpdateHiddenRef(ctx, acceptedRef, commit); err != nil {
		warnings = append(warnings, "accepted state is durable in SQLite but refs/hop/accepted needs repair: "+err.Error())
	}
	result := AcceptResult{
		State:         accepted,
		ProposalPaths: proposalPaths,
		CurrentPaths:  currentPaths,
		Check:         recordedCheck,
		Warnings:      warnings,
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
	return result, nil
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
	pushCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	pushed, configured, err := s.Repo.PushAccepted(pushCtx, result.State.GitCommit)
	if err != nil {
		message, _ := RedactPromptSecrets(err.Error())
		result.Warnings = append(result.Warnings, "accepted state is local, but automatic push failed: "+message)
		return
	}
	if configured {
		result.RemotePush = &pushed
	}
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
	return s.syncLocked(ctx)
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
	if len(conflicts) == 0 {
		return RefreshResult{}, fmt.Errorf("proposal %s now merges cleanly; retry hop land", proposal.ID)
	}
	commit, err := s.Repo.CommitTree(ctx, conflictTree, []string{current.GitCommit, proposal.GitCommit},
		fmt.Sprintf("Reconcile %s against %s\n", proposal.ID, current.ID))
	if err != nil {
		return RefreshResult{}, err
	}
	summary, err := encodeReconciliationConflicts(conflicts)
	if err != nil {
		return RefreshResult{}, err
	}
	instruction := fmt.Sprintf(
		"Resolve proposal %s (%s) against accepted state %s (%s). Preserve both compatible intents. Inspect both input states for structural, delete/rename, mode, symlink, or binary conflicts that may have no text markers; resolve every conflict intentionally, remove all merge markers, run hop check, propose the result, and land it without asking the user to coordinate the merge. Conflict candidates: %s",
		proposal.ID, proposal.Summary, current.ID, current.Summary, strings.Join(conflicts, ", "))
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
	conflicts, ok := decodeReconciliationConflicts(prompt.Summary)
	if !ok || len(conflicts) == 0 {
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
		Workspace:    attempt.Workspace,
		Deliver:      s.deliveryEnvironment(prompt, attempt),
		ConflictTree: prompt.SourceTree,
		Conflicts:    conflicts,
		Reused:       reused,
	}, nil
}

func encodeReconciliationConflicts(paths []string) (string, error) {
	encoded, err := json.Marshal(paths)
	if err != nil {
		return "", fmt.Errorf("encode reconciliation conflicts: %w", err)
	}
	return reconciliationSummaryPrefix + string(encoded), nil
}

func decodeReconciliationConflicts(summary string) ([]string, bool) {
	if !strings.HasPrefix(summary, reconciliationSummaryPrefix) {
		return nil, false
	}
	var paths []string
	if err := json.Unmarshal([]byte(strings.TrimPrefix(summary, reconciliationSummaryPrefix)), &paths); err != nil {
		return nil, false
	}
	return paths, true
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
	undo := State{
		ID:                undoID,
		Kind:              StateAccepted,
		CanonicalAnchorID: current.ID,
		SourceTree:        previous.SourceTree,
		GitCommit:         commit,
		Summary:           "Undo " + current.ID,
		Agent:             "hop",
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
	if err := s.Repo.UpdateHiddenRef(ctx, acceptedRef, undo.GitCommit); err != nil {
		postCommitErrors = append(postCommitErrors, fmt.Errorf("repair refs/hop/accepted: %w", err))
	}
	if err := s.Repo.MaterializeTree(ctx, rootBase.SourceTree, undo.SourceTree); err != nil {
		postCommitErrors = append(postCommitErrors, fmt.Errorf("visible root %s was not synchronized: %w", s.Root, err))
	} else if err := s.Store.CASMaterializedHead(ctx, rootBase.ID, undo.ID); err != nil {
		postCommitErrors = append(postCommitErrors, fmt.Errorf("record visible root synchronization: %w", err))
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
	rootState, matches, err := s.visibleRootState(ctx)
	if err != nil {
		return Status{}, err
	}
	if !matches {
		status.RootStatus = "diverged"
		return status, nil
	}
	if err := s.Repo.CheckIndexSafe(ctx, rootState.SourceTree); err != nil {
		var conflict *RootConflictError
		if errors.As(err, &conflict) {
			status.RootStatus = "diverged"
			return status, nil
		}
		return Status{}, err
	}
	status.RootStateID = rootState.ID
	if rootState.ID == status.AcceptedHead.ID {
		status.RootStatus = "synchronized"
	} else {
		status.RootStatus = "stale"
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
	for _, row := range rows {
		state := row.State
		if err := s.Repo.VerifyObjects(ctx, state.GitCommit, state.SourceTree); err != nil {
			return fmt.Errorf("state %s references unavailable Git data: %w", state.ID, err)
		}
		name := "states/" + state.ID
		commit, exists, err := s.Repo.ReadHiddenRef(ctx, name)
		if err != nil {
			return err
		}
		if !exists || commit != state.GitCommit {
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
	commit, exists, err := s.Repo.ReadHiddenRef(ctx, acceptedRef)
	if err != nil {
		return err
	}
	if !exists || commit != head.GitCommit {
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
