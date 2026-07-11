package hop

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const acceptedRef = "accepted"

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
	if len(trackedHopPaths) > 0 {
		return nil, State{}, fmt.Errorf("cannot initialize Hop: .hop is already tracked as project source (for example %s)", trackedHopPaths[0])
	}
	hopDir := filepath.Join(root, ".hop")
	if err := os.MkdirAll(filepath.Join(hopDir, "workspaces"), 0o755); err != nil {
		return nil, State{}, fmt.Errorf("create Hop project directory: %w", err)
	}
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
	if filepath.Clean(recordedRoot) != filepath.Clean(root) {
		store.Close()
		return nil, fmt.Errorf("Hop database belongs to %s, not %s", recordedRoot, root)
	}
	service := &Service{Root: root, Store: store, Repo: repo}
	reconcileAccepted := os.Getenv("HOP_ACCEPTANCE_LOCK_HELD") != "1"
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
	if fromStateID == "" {
		return s.createInitialPrompt(ctx, message, agent)
	}
	return s.createFollowupPrompt(ctx, message, fromStateID, agent)
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
	check := Check{
		ID:        checkID,
		AttemptID: attempt.ID,
		StateID:   checkpoint.ID,
		TreeHash:  checkpoint.SourceTree,
		Command:   append([]string(nil), argv...),
		ExitCode:  result.ExitCode,
		Output:    result.Output,
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
	workspaceRepo, err := OpenRepository(attempt.Workspace)
	if err != nil {
		return ProposalResult{}, err
	}
	commit, tree, err := workspaceRepo.Snapshot(ctx, "hop: proposal\n")
	if err != nil {
		return ProposalResult{}, err
	}
	if strings.TrimSpace(summary) == "" {
		summary = task.Title
	}
	parents := canonicalizeParents([]Parent{{StateID: attempt.HeadStateID, Role: "run_parent", Order: 0}})
	proposal := State{
		ID:                newID("r"),
		Kind:              StateProposal,
		TaskID:            attempt.TaskID,
		AttemptID:         attempt.ID,
		CanonicalAnchorID: attempt.BaseStateID,
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
	checks, err := s.Store.ListChecks(ctx, attempt.ID, tree)
	if err != nil {
		return ProposalResult{}, err
	}
	return ProposalResult{Proposal: proposal, Checks: checks}, nil
}

func (s *Service) Accept(ctx context.Context, proposalID string, checkCommand []string) (AcceptResult, error) {
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
	attempt, err := s.Store.GetAttempt(ctx, proposal.AttemptID)
	if err != nil {
		return AcceptResult{}, err
	}
	base, err := s.Store.GetState(ctx, attempt.BaseStateID)
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
	overlap := intersectPaths(proposalPaths, currentPaths)
	if len(overlap) > 0 {
		return AcceptResult{}, &ConflictError{Paths: overlap}
	}
	finalTree, mergeConflicts, err := s.Repo.ComposeTrees(ctx, base.GitCommit, current.GitCommit, proposal.GitCommit)
	if err != nil {
		return AcceptResult{}, err
	}
	if len(mergeConflicts) > 0 {
		return AcceptResult{}, &ConflictError{Paths: mergeConflicts}
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
		check := Check{
			ID:        checkID,
			AttemptID: proposal.AttemptID,
			TreeHash:  finalTree,
			Command:   append([]string(nil), checkCommand...),
			ExitCode:  result.ExitCode,
			Output:    result.Output,
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
	}
	var warnings []string
	if err := s.Repo.UpdateHiddenRef(ctx, acceptedRef, commit); err != nil {
		warnings = append(warnings, "accepted state is durable in SQLite but refs/hop/accepted needs repair: "+err.Error())
	}
	return AcceptResult{
		State:         accepted,
		ProposalPaths: proposalPaths,
		CurrentPaths:  currentPaths,
		Check:         recordedCheck,
		Warnings:      warnings,
	}, nil
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
	if err := s.Repo.UpdateHiddenRef(ctx, acceptedRef, undo.GitCommit); err != nil {
		return undo, &CommittedStateError{State: undo, Err: err}
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
	failed := State{
		ID:                newID("f"),
		Kind:              StateFailed,
		TaskID:            proposal.TaskID,
		AttemptID:         proposal.AttemptID,
		CanonicalAnchorID: current.ID,
		SourceTree:        tree,
		GitCommit:         commit,
		Summary:           "Final validation failed: " + shellQuote(command),
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
	return s.Store.Status(ctx)
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
