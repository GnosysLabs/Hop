package hop

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestInitPreservesGitBranchIndexAndWorkingTree(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	runGitTest(t, root, "init", "--quiet")
	writeTestFile(t, filepath.Join(root, "tracked.txt"), "committed\n")
	runGitTest(t, root, "add", "tracked.txt")
	runGitTest(t, root, "-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "--quiet", "-m", "initial")

	writeTestFile(t, filepath.Join(root, "tracked.txt"), "staged\n")
	runGitTest(t, root, "add", "tracked.txt")
	writeTestFile(t, filepath.Join(root, "tracked.txt"), "working\n")
	writeTestFile(t, filepath.Join(root, "untracked.txt"), "untracked\n")

	beforeHead := runGitTest(t, root, "rev-parse", "HEAD")
	beforeBranch := runGitTest(t, root, "symbolic-ref", "--short", "HEAD")
	beforeIndex := runGitTest(t, root, "diff", "--cached", "--binary")
	beforeStatus := runGitTest(t, root, "status", "--porcelain=v1")

	service, initial, err := InitProject(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	if got := runGitTest(t, root, "rev-parse", "HEAD"); got != beforeHead {
		t.Fatalf("HEAD moved from %s to %s", beforeHead, got)
	}
	if got := runGitTest(t, root, "symbolic-ref", "--short", "HEAD"); got != beforeBranch {
		t.Fatalf("branch changed from %s to %s", beforeBranch, got)
	}
	if got := runGitTest(t, root, "diff", "--cached", "--binary"); got != beforeIndex {
		t.Fatal("Hop init changed the user's index")
	}
	if got := runGitTest(t, root, "status", "--porcelain=v1"); got != beforeStatus {
		t.Fatalf("working status changed:\nwant %q\n got %q", beforeStatus, got)
	}
	assertTreeFiles(t, service, initial.GitCommit, map[string]string{
		"tracked.txt":   "working\n",
		"untracked.txt": "untracked\n",
	})
	if err := service.Close(); err != nil {
		t.Fatal(err)
	}

	service, again, err := InitProject(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = service.Close() })
	if again.ID != initial.ID {
		t.Fatalf("idempotent init created %s, want existing %s", again.ID, initial.ID)
	}
}

func TestInitRefusesAUserTrackedHopDirectory(t *testing.T) {
	root := t.TempDir()
	runGitTest(t, root, "init", "--quiet")
	writeTestFile(t, filepath.Join(root, ".hop", "user-owned.txt"), "do not overwrite\n")
	runGitTest(t, root, "add", "-f", ".hop/user-owned.txt")
	_, _, err := InitProject(context.Background(), root)
	if err == nil || !strings.Contains(err.Error(), ".hop is already tracked") {
		t.Fatalf("InitProject error = %v, want tracked .hop refusal", err)
	}
	contents, readErr := os.ReadFile(filepath.Join(root, ".hop", "user-owned.txt"))
	if readErr != nil || string(contents) != "do not overwrite\n" {
		t.Fatalf("tracked .hop content changed: %q, %v", string(contents), readErr)
	}
}

func TestCLIJSONWorkflow(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "base.txt"), "base\n")
	previousDirectory, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(root); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(previousDirectory) })

	runCLIJSONTest(t, []string{"init", "--json"})
	started := runCLIJSONTest(t, []string{"start", "--agent", "fake", "--json", "Add CLI file"})
	data := objectField(t, started, "data")
	prompt := objectField(t, data, "prompt")
	promptID := stringField(t, prompt, "id")
	workspace := stringField(t, data, "workspace")
	if promptID == "" || workspace == "" {
		t.Fatal("start JSON omitted prompt ID or workspace")
	}
	writeTestFile(t, filepath.Join(workspace, "cli.txt"), "cli\n")

	proposed := runCLIJSONTest(t, []string{"propose", "--summary", "CLI change", "--json", promptID})
	proposal := objectField(t, objectField(t, proposed, "data"), "proposal")
	proposalID := stringField(t, proposal, "id")
	accepted := runCLIJSONTest(t, []string{"land", proposalID, "--json", "--", "sh", "-c", "test -f cli.txt"})
	acceptedState := objectField(t, objectField(t, accepted, "data"), "state")
	if kind := stringField(t, acceptedState, "kind"); kind != string(StateAccepted) {
		t.Fatalf("landed kind = %q, want %q", kind, StateAccepted)
	}
	status := runCLIJSONTest(t, []string{"status", "--json"})
	statusData := objectField(t, status, "data")
	head := objectField(t, statusData, "accepted_head")
	if stringField(t, head, "id") != stringField(t, acceptedState, "id") {
		t.Fatal("status accepted head does not match landed state")
	}
}

func TestDoctorRejectsCommitTreeAndDigestMismatch(t *testing.T) {
	ctx := context.Background()
	service, initial := newTestProject(t, map[string]string{"base.txt": "base\n"})
	emptyTree, err := service.Repo.EmptyTree(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Store.db.ExecContext(ctx,
		`UPDATE states SET source_tree = ?, digest = 'tampered' WHERE id = ?`, emptyTree, initial.ID); err != nil {
		t.Fatal(err)
	}
	report, err := service.Doctor(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	if report.OK {
		t.Fatal("doctor approved a state whose commit tree and digest were tampered")
	}
	joined := strings.Join(report.Problems, "\n")
	if !strings.Contains(joined, "records tree") || !strings.Contains(joined, "digest mismatch") {
		t.Fatalf("doctor problems did not explain both mismatches: %s", joined)
	}
}

func TestOpenProjectInsideFinalValidationDoesNotReacquireAcceptanceLock(t *testing.T) {
	ctx := context.Background()
	service, _ := newTestProject(t, map[string]string{"base.txt": "base\n"})
	release, err := acquireProjectLock(ctx, service.Root, "accept")
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	previous, hadPrevious := os.LookupEnv("HOP_ACCEPTANCE_LOCK_HELD")
	if err := os.Setenv("HOP_ACCEPTANCE_LOCK_HELD", "1"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if hadPrevious {
			_ = os.Setenv("HOP_ACCEPTANCE_LOCK_HELD", previous)
		} else {
			_ = os.Unsetenv("HOP_ACCEPTANCE_LOCK_HELD")
		}
	})
	opened := make(chan error, 1)
	go func() {
		nested, openErr := OpenProject(service.Root)
		if openErr == nil {
			openErr = nested.Close()
		}
		opened <- openErr
	}()
	select {
	case openErr := <-opened:
		if openErr != nil {
			t.Fatal(openErr)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("OpenProject deadlocked by reacquiring the acceptance lock")
	}
}

func TestPromptFollowupAndProposalAreImmutable(t *testing.T) {
	ctx := context.Background()
	service, initial := newTestProject(t, map[string]string{"app.txt": "initial\n"})

	exactPrompt := "  Change the app\nwithout normalizing this text.  "
	started, err := service.CreatePrompt(ctx, exactPrompt, "", "test-agent")
	if err != nil {
		t.Fatal(err)
	}
	if started.Prompt.SourceTree != initial.SourceTree {
		t.Fatalf("prompt tree = %s, want accepted tree %s", started.Prompt.SourceTree, initial.SourceTree)
	}
	if started.Prompt.Prompt != exactPrompt {
		t.Fatalf("stored prompt = %q, want exact %q", started.Prompt.Prompt, exactPrompt)
	}
	writeTestFile(t, filepath.Join(started.Workspace, "app.txt"), "first change\n")

	followup, err := service.CreatePrompt(ctx, "Use the other approach", started.Prompt.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	if followup.Checkpoint == nil {
		t.Fatal("follow-up did not create a checkpoint")
	}
	if followup.Prompt.SourceTree != followup.Checkpoint.SourceTree {
		t.Fatal("follow-up prompt did not preserve the checkpoint tree")
	}
	if followup.Prompt.SourceTree == initial.SourceTree {
		t.Fatal("checkpoint failed to capture workspace edits")
	}
	parent, err := service.Store.ParentByRole(ctx, followup.Prompt.ID, "run_parent")
	if err != nil {
		t.Fatal(err)
	}
	if parent.StateID != followup.Checkpoint.ID {
		t.Fatalf("follow-up parent = %s, want checkpoint %s", parent.StateID, followup.Checkpoint.ID)
	}

	writeTestFile(t, filepath.Join(started.Workspace, "app.txt"), "proposed\n")
	proposed, err := service.Propose(ctx, followup.Prompt.ID, "Changed the app")
	if err != nil {
		t.Fatal(err)
	}
	frozenTree := proposed.Proposal.SourceTree
	writeTestFile(t, filepath.Join(started.Workspace, "app.txt"), "changed after proposal\n")
	stored, err := service.State(ctx, proposed.Proposal.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.SourceTree != frozenTree {
		t.Fatal("proposal tree changed after later workspace edits")
	}
}

func TestCheckRunsAgainstTheRecordedCheckpointTree(t *testing.T) {
	ctx := context.Background()
	service, _ := newTestProject(t, map[string]string{"app.txt": "initial\n"})
	started, err := service.CreatePrompt(ctx, "Check exact state", "", "test")
	if err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(started.Workspace, "app.txt"), "checkpointed\n")
	signal := filepath.Join(t.TempDir(), "check-started")
	type outcome struct {
		check Check
		err   error
	}
	result := make(chan outcome, 1)
	go func() {
		check, checkErr := service.RunCheck(ctx, started.Prompt.ID, []string{
			"sh", "-c", `touch "$1"; sleep 0.2; cat app.txt`, "hop-check", signal,
		})
		result <- outcome{check: check, err: checkErr}
	}()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(signal); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("validation command did not start")
		}
		time.Sleep(10 * time.Millisecond)
	}
	writeTestFile(t, filepath.Join(started.Workspace, "app.txt"), "raced edit\n")
	finished := <-result
	if finished.err != nil {
		t.Fatal(finished.err)
	}
	if finished.check.Output != "checkpointed\n" {
		t.Fatalf("check observed %q, want checkpointed tree", finished.check.Output)
	}
}

func TestDisjointProposalsLandAndUndoMovesForward(t *testing.T) {
	ctx := context.Background()
	service, initial := newTestProject(t, map[string]string{"base.txt": "base\n"})

	first, err := service.CreatePrompt(ctx, "Add one", "", "agent-one")
	if err != nil {
		t.Fatal(err)
	}
	second, err := service.CreatePrompt(ctx, "Add two", "", "agent-two")
	if err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(first.Workspace, "one.txt"), "one\n")
	writeTestFile(t, filepath.Join(second.Workspace, "two.txt"), "two\n")

	proposalOne, err := service.Propose(ctx, first.Prompt.ID, "Add one")
	if err != nil {
		t.Fatal(err)
	}
	proposalTwo, err := service.Propose(ctx, second.Prompt.ID, "Add two")
	if err != nil {
		t.Fatal(err)
	}
	acceptedOne, err := service.Accept(ctx, proposalOne.Proposal.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	acceptedTwo, err := service.Accept(ctx, proposalTwo.Proposal.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	if acceptedTwo.State.SourceTree == acceptedOne.State.SourceTree {
		t.Fatal("second disjoint acceptance did not change the accepted tree")
	}
	assertTreeFiles(t, service, acceptedTwo.State.GitCommit, map[string]string{
		"base.txt": "base\n",
		"one.txt":  "one\n",
		"two.txt":  "two\n",
	})

	undo, err := service.Undo(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if undo.ID == acceptedOne.State.ID || undo.ID == acceptedTwo.State.ID {
		t.Fatal("undo rewrote an old state instead of creating a new one")
	}
	if undo.SourceTree != acceptedOne.State.SourceTree {
		t.Fatalf("undo tree = %s, want previous accepted tree %s", undo.SourceTree, acceptedOne.State.SourceTree)
	}
	if undo.SourceTree == initial.SourceTree {
		t.Fatal("undo erased more than the latest accepted transition")
	}
	assertTreeFiles(t, service, undo.GitCommit, map[string]string{
		"base.txt": "base\n",
		"one.txt":  "one\n",
	})
	assertTreeMissing(t, service, undo.GitCommit, "two.txt")
	report, err := service.Doctor(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	if !report.OK {
		t.Fatalf("doctor reported problems: %#v", report.Problems)
	}
}

func TestConcurrentDisjointAcceptancesSerialize(t *testing.T) {
	ctx := context.Background()
	service, _ := newTestProject(t, map[string]string{"base.txt": "base\n"})

	first, err := service.CreatePrompt(ctx, "Add alpha", "", "alpha")
	if err != nil {
		t.Fatal(err)
	}
	second, err := service.CreatePrompt(ctx, "Add beta", "", "beta")
	if err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(first.Workspace, "alpha.txt"), "alpha\n")
	writeTestFile(t, filepath.Join(second.Workspace, "beta.txt"), "beta\n")
	alpha, err := service.Propose(ctx, first.Prompt.ID, "Alpha")
	if err != nil {
		t.Fatal(err)
	}
	beta, err := service.Propose(ctx, second.Prompt.ID, "Beta")
	if err != nil {
		t.Fatal(err)
	}

	proposalIDs := []string{alpha.Proposal.ID, beta.Proposal.ID}
	errs := make(chan error, len(proposalIDs))
	var wg sync.WaitGroup
	for _, id := range proposalIDs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, acceptErr := service.Accept(ctx, id, nil)
			errs <- acceptErr
		}()
	}
	wg.Wait()
	close(errs)
	for acceptErr := range errs {
		if acceptErr != nil {
			t.Fatalf("concurrent acceptance: %v", acceptErr)
		}
	}
	head, err := service.Store.AcceptedHead(ctx)
	if err != nil {
		t.Fatal(err)
	}
	assertTreeFiles(t, service, head.GitCommit, map[string]string{
		"base.txt":  "base\n",
		"alpha.txt": "alpha\n",
		"beta.txt":  "beta\n",
	})
	history, err := service.History(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(history) != 3 {
		t.Fatalf("canonical history has %d states, want initial plus two acceptances", len(history))
	}
}

func TestOverlappingProposalAndFailedFinalCheckDoNotMoveHead(t *testing.T) {
	ctx := context.Background()
	service, _ := newTestProject(t, map[string]string{"shared.txt": "base\n"})

	first, err := service.CreatePrompt(ctx, "First edit", "", "one")
	if err != nil {
		t.Fatal(err)
	}
	second, err := service.CreatePrompt(ctx, "Second edit", "", "two")
	if err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(first.Workspace, "shared.txt"), "first\n")
	writeTestFile(t, filepath.Join(second.Workspace, "shared.txt"), "second\n")
	firstProposal, err := service.Propose(ctx, first.Prompt.ID, "First")
	if err != nil {
		t.Fatal(err)
	}
	secondProposal, err := service.Propose(ctx, second.Prompt.ID, "Second")
	if err != nil {
		t.Fatal(err)
	}
	firstAccepted, err := service.Accept(ctx, firstProposal.Proposal.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, err = service.Accept(ctx, secondProposal.Proposal.ID, nil)
	var conflict *ConflictError
	if !errors.As(err, &conflict) {
		t.Fatalf("second acceptance error = %v, want ConflictError", err)
	}
	if len(conflict.Paths) != 1 || conflict.Paths[0] != "shared.txt" {
		t.Fatalf("conflict paths = %#v", conflict.Paths)
	}
	head, err := service.Store.AcceptedHead(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if head.ID != firstAccepted.State.ID {
		t.Fatalf("blocked proposal moved accepted head to %s", head.ID)
	}

	third, err := service.CreatePrompt(ctx, "Add safe file", "", "three")
	if err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(third.Workspace, "safe.txt"), "safe\n")
	thirdProposal, err := service.Propose(ctx, third.Prompt.ID, "Safe")
	if err != nil {
		t.Fatal(err)
	}
	_, err = service.Accept(ctx, thirdProposal.Proposal.ID, []string{"sh", "-c", "exit 9"})
	var checkFailed *CheckFailedError
	if !errors.As(err, &checkFailed) {
		t.Fatalf("failed validation error = %v, want CheckFailedError", err)
	}
	if checkFailed.Check.StateID == "" {
		t.Fatal("failed final-tree validation was not attached to a durable state")
	}
	failedState, err := service.Store.GetState(ctx, checkFailed.Check.StateID)
	if err != nil {
		t.Fatal(err)
	}
	if failedState.Kind != StateFailed || failedState.SourceTree != checkFailed.Check.TreeHash {
		t.Fatalf("failed state = %#v, check tree = %s", failedState, checkFailed.Check.TreeHash)
	}
	head, err = service.Store.AcceptedHead(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if head.ID != firstAccepted.State.ID {
		t.Fatal("failed final-tree validation moved accepted head")
	}
}

func newTestProject(t *testing.T, files map[string]string) (*Service, State) {
	t.Helper()
	root := t.TempDir()
	for path, contents := range files {
		writeTestFile(t, filepath.Join(root, path), contents)
	}
	service, initial, err := InitProject(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = service.Close() })
	return service, initial
}

func writeTestFile(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
		t.Fatal(err)
	}
}

func assertTreeFiles(t *testing.T, service *Service, commit string, expected map[string]string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "materialized")
	if _, err := service.Repo.AddDetachedWorktree(context.Background(), path, commit); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = service.Repo.RemoveWorktree(context.Background(), path, true) })
	for name, want := range expected {
		contents, err := os.ReadFile(filepath.Join(path, name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		if string(contents) != want {
			t.Fatalf("%s = %q, want %q", name, string(contents), want)
		}
	}
}

func assertTreeMissing(t *testing.T, service *Service, commit string, name string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "materialized")
	if _, err := service.Repo.AddDetachedWorktree(context.Background(), path, commit); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = service.Repo.RemoveWorktree(context.Background(), path, true) })
	if _, err := os.Stat(filepath.Join(path, name)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expected %s to be absent, stat error = %v", name, err)
	}
}

func runGitTest(t *testing.T, root string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = root
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, stderr.String())
	}
	return strings.TrimSpace(stdout.String())
}

func runCLIJSONTest(t *testing.T, args []string) map[string]any {
	t.Helper()
	var stdout, stderr bytes.Buffer
	if code := RunCLI(args, &stdout, &stderr); code != 0 {
		t.Fatalf("hop %s exited %d\nstdout: %s\nstderr: %s", strings.Join(args, " "), code, stdout.String(), stderr.String())
	}
	var result map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatalf("decode JSON from hop %s: %v\n%s", strings.Join(args, " "), err, stdout.String())
	}
	if ok, _ := result["ok"].(bool); !ok {
		t.Fatalf("hop %s returned non-ok JSON: %#v", strings.Join(args, " "), result)
	}
	return result
}

func objectField(t *testing.T, object map[string]any, key string) map[string]any {
	t.Helper()
	value, ok := object[key].(map[string]any)
	if !ok {
		t.Fatalf("field %q is not an object: %#v", key, object[key])
	}
	return value
}

func stringField(t *testing.T, object map[string]any, key string) string {
	t.Helper()
	value, ok := object[key].(string)
	if !ok {
		t.Fatalf("field %q is not a string: %#v", key, object[key])
	}
	return value
}
