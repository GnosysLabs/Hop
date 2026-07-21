package hop

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
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
	if err == nil || !strings.Contains(err.Error(), "private .hop path is already tracked") {
		t.Fatalf("InitProject error = %v, want tracked local-runtime refusal", err)
	}
	contents, readErr := os.ReadFile(filepath.Join(root, ".hop", "user-owned.txt"))
	if readErr != nil || string(contents) != "do not overwrite\n" {
		t.Fatalf("tracked .hop content changed: %q, %v", string(contents), readErr)
	}
}

func TestInitRefusesTrackedPromptRecords(t *testing.T) {
	root := t.TempDir()
	runGitTest(t, root, "init", "--quiet")
	writeTestFile(t, filepath.Join(root, ".hop", "records", "prompts.json"), `{"schema_version":1,"prompts":[]}`+"\n")
	runGitTest(t, root, "add", "-f", ".hop/records/prompts.json")

	_, _, err := InitProject(context.Background(), root)
	if err == nil || !strings.Contains(err.Error(), "private .hop path is already tracked") {
		t.Fatalf("InitProject error = %v, want tracked private-path refusal", err)
	}
}

func TestFindHopRootStopsAtNestedRepositoryBoundary(t *testing.T) {
	t.Setenv("HOP_ROOT", "")
	parent, _ := newTestProject(t, map[string]string{"parent.txt": "parent\n"})
	child := filepath.Join(parent.Root, "Coding Project")
	if err := os.MkdirAll(child, 0o755); err != nil {
		t.Fatal(err)
	}
	runGitTest(t, child, "init", "--quiet")
	writeTestFile(t, filepath.Join(child, "child.txt"), "child\n")

	if root, err := FindHopRoot(child); !errors.Is(err, ErrNotHopProject) {
		t.Fatalf("nested repository inherited Hop root %q, err=%v", root, err)
	}
	childService, _, err := InitProject(context.Background(), child)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = childService.Close() })
	childInfo, childErr := os.Stat(child)
	rootInfo, rootErr := os.Stat(childService.Root)
	if childErr != nil || rootErr != nil || !os.SameFile(childInfo, rootInfo) {
		t.Fatalf("nested repository Hop root = %q, want %q", childService.Root, child)
	}
}

func TestInitDoesNotRequireUserCacheWrite(t *testing.T) {
	t.Setenv("HOP_ROOT", "")
	root := t.TempDir()
	runGitTest(t, root, "init", "--quiet")
	writeTestFile(t, filepath.Join(root, "base.txt"), "base\n")

	blockedCache := filepath.Join(t.TempDir(), "not-a-directory")
	writeTestFile(t, blockedCache, "blocked\n")
	t.Setenv("HOME", blockedCache)
	t.Setenv("XDG_CACHE_HOME", blockedCache)
	t.Setenv("LOCALAPPDATA", blockedCache)
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")

	service, _, err := InitProject(context.Background(), root)
	if err != nil {
		t.Fatalf("InitProject required access outside the project: %v", err)
	}
	t.Cleanup(func() { _ = service.Close() })

	lockPath, err := repositoryInitLockPath(root)
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(canonicalExistingPath(root), ".hop", "bootstrap.lock")
	if lockPath != want {
		t.Fatalf("repository initialization lock = %q, want project-local %q", lockPath, want)
	}
}

func TestFindHopRootRetainsSameRepositoryAndManagedWorkspaces(t *testing.T) {
	t.Setenv("HOP_ROOT", "")
	service, _ := newTestProject(t, map[string]string{"base.txt": "base\n"})
	subdirectory := filepath.Join(service.Root, "nested", "source")
	if err := os.MkdirAll(subdirectory, 0o755); err != nil {
		t.Fatal(err)
	}
	if root, err := FindHopRoot(subdirectory); err != nil || canonicalExistingPath(root) != canonicalExistingPath(service.Root) {
		t.Fatalf("same-repository Hop root = %q, err=%v", root, err)
	}
	started, err := service.CreatePrompt(context.Background(), "Inspect managed workspace", "", "codex")
	if err != nil {
		t.Fatal(err)
	}
	if root, err := FindHopRoot(started.Workspace); err != nil || canonicalExistingPath(root) != canonicalExistingPath(service.Root) {
		t.Fatalf("managed-workspace Hop root = %q, err=%v", root, err)
	}
}

func TestConcurrentFirstBeginsInitializeExactlyOnce(t *testing.T) {
	t.Setenv("HOP_ROOT", "")
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

	type result struct {
		code   int
		stdout string
		stderr string
	}
	const workers = 8
	start := make(chan struct{})
	results := make(chan result, workers)
	var group sync.WaitGroup
	for index := 0; index < workers; index++ {
		group.Add(1)
		go func(index int) {
			defer group.Done()
			<-start
			var stdout, stderr bytes.Buffer
			code := RunCLI([]string{
				"begin", "--json", "--agent", fmt.Sprintf("agent-%d", index),
				"--session", fmt.Sprintf("session-%d", index), fmt.Sprintf("Prompt %d", index),
			}, &stdout, &stderr)
			results <- result{code: code, stdout: stdout.String(), stderr: stderr.String()}
		}(index)
	}
	close(start)
	group.Wait()
	close(results)

	var workspaces []string
	for result := range results {
		if result.code != 0 {
			t.Fatalf("concurrent first begin exited %d\nstdout: %s\nstderr: %s", result.code, result.stdout, result.stderr)
		}
		var response map[string]any
		if err := json.Unmarshal([]byte(result.stdout), &response); err != nil {
			t.Fatalf("decode concurrent begin output %q: %v", result.stdout, err)
		}
		data := objectField(t, response, "data")
		workspaces = append(workspaces, stringField(t, data, "workspace"))
	}
	for _, workspace := range workspaces {
		if info, err := os.Stat(workspace); err != nil || !info.IsDir() {
			t.Fatalf("concurrent begin workspace %q missing: %v", workspace, err)
		}
	}

	service, err := OpenProject(root)
	if err != nil {
		t.Fatal(err)
	}
	defer service.Close()
	graph, err := service.Store.Graph(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	initialAccepted := 0
	prompts := 0
	for _, row := range graph {
		if row.State.Kind == StateAccepted && row.State.TaskID == "" {
			initialAccepted++
		}
		if row.State.Kind == StatePrompt {
			prompts++
		}
	}
	if initialAccepted != 1 || prompts != workers {
		t.Fatalf("concurrent bootstrap created %d initial states and %d prompts, want 1 and %d", initialAccepted, prompts, workers)
	}
}

func TestConcurrentInitInExistingGitRepository(t *testing.T) {
	root := t.TempDir()
	runGitTest(t, root, "init", "--quiet")
	writeTestFile(t, filepath.Join(root, "base.txt"), "base\n")

	const workers = 8
	start := make(chan struct{})
	type result struct {
		stateID string
		err     error
	}
	results := make(chan result, workers)
	var group sync.WaitGroup
	for range workers {
		group.Add(1)
		go func() {
			defer group.Done()
			<-start
			service, initial, err := InitProject(context.Background(), root)
			if service != nil {
				_ = service.Close()
			}
			results <- result{stateID: initial.ID, err: err}
		}()
	}
	close(start)
	group.Wait()
	close(results)

	initialID := ""
	for result := range results {
		if result.err != nil {
			t.Fatalf("concurrent existing-repository init: %v", result.err)
		}
		if initialID == "" {
			initialID = result.stateID
		} else if result.stateID != initialID {
			t.Fatalf("concurrent init returned initial states %s and %s", initialID, result.stateID)
		}
	}
	exclude, err := os.ReadFile(filepath.Join(root, ".git", "info", "exclude"))
	if err != nil {
		t.Fatal(err)
	}
	if count := strings.Count(string(exclude), ".hop/\n"); count != 1 {
		t.Fatalf("concurrent initialization wrote .hop/ exclusion %d times", count)
	}
	if strings.Contains(string(exclude), "!.hop/records") {
		t.Fatalf("concurrent initialization exposed prompt records:\n%s", exclude)
	}
}

func TestProposeDoesNotPublishPromptLedger(t *testing.T) {
	ctx := context.Background()
	service, _ := newTestProject(t, map[string]string{"base.txt": "base\n"})
	result, err := service.CreatePrompt(ctx, "Keep this prompt private", "", "codex")
	if err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(result.Workspace, "feature.txt"), "done\n")
	proposal, err := service.Propose(ctx, result.Prompt.ID, "Land without publishing the prompt")
	if err != nil {
		t.Fatal(err)
	}

	materialized := filepath.Join(t.TempDir(), "proposal")
	if _, err := service.Repo.AddDetachedWorktree(ctx, materialized, proposal.Proposal.GitCommit); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = service.Repo.RemoveWorktree(ctx, materialized, true) })
	if _, err := os.Stat(filepath.Join(materialized, ".hop")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("proposal published private .hop content: %v", err)
	}
}

func TestExportOmitsActiveInternalAttempt(t *testing.T) {
	ctx := context.Background()
	service, _ := newTestProject(t, map[string]string{"base.txt": "base\n"})
	if _, err := service.CreatePrompt(ctx, "Read-only internal audit", "", "codex"); err != nil {
		t.Fatal(err)
	}
	ledger, err := service.ExportPromptLedger(ctx, service.Root)
	if err != nil {
		t.Fatal(err)
	}
	if len(ledger.Prompts) != 0 {
		t.Fatalf("exported active internal prompts: %#v", ledger.Prompts)
	}
}

func TestCompletePromptExportsSummaryAndFinalResponseWithoutProposal(t *testing.T) {
	ctx := context.Background()
	service, _ := newTestProject(t, map[string]string{"base.txt": "base\n"})
	started, err := service.CreatePrompt(ctx, "Inspect an external service", "", "codex")
	if err != nil {
		t.Fatal(err)
	}
	result, err := service.CompletePrompt(ctx, started.Prompt.ID, "Service is healthy", "The service is healthy.\n\nNo repository changes were required.")
	if err != nil {
		t.Fatal(err)
	}
	if result.Completion.StateID != started.Prompt.ID {
		t.Fatalf("completion = %#v", result.Completion)
	}
	ledger, err := service.ExportPromptLedger(ctx, service.Root)
	if err != nil {
		t.Fatal(err)
	}
	if len(ledger.Prompts) != 1 {
		t.Fatalf("exported prompts = %#v", ledger.Prompts)
	}
	record := ledger.Prompts[0]
	if record.Status != "completed" || record.ResponseSummary != result.Completion.Summary || record.FinalResponse != result.Completion.FinalResponse || record.CompletedAt == nil {
		t.Fatalf("completed portable prompt = %#v", record)
	}
	if record.Metadata.AttemptHeadKind != string(StatePrompt) {
		t.Fatalf("read-only completion changed state graph: %#v", record.Metadata)
	}
}

func TestCompletePromptClosesAndReclaimsSourceCleanAttempt(t *testing.T) {
	ctx := context.Background()
	service, _ := newTestProject(t, map[string]string{"base.txt": "base\n"})
	started, err := service.BeginPrompt(ctx, "Inspect the repository", "", "codex", "cleanup-session")
	if err != nil {
		t.Fatal(err)
	}
	result, err := service.CompletePrompt(ctx, started.Prompt.ID, "Inspection complete", "The repository is healthy.")
	if err != nil {
		t.Fatal(err)
	}
	if result.Cleanup == nil || result.Cleanup.Removed != 1 {
		t.Fatalf("cleanup = %#v", result.Cleanup)
	}
	if _, err := os.Stat(started.Workspace); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("completed read-only workspace still exists: %v", err)
	}
	attempt, err := service.Store.GetAttempt(ctx, started.Attempt.ID)
	if err != nil {
		t.Fatal(err)
	}
	if attempt.Status != "completed" {
		t.Fatalf("attempt status = %q, want completed", attempt.Status)
	}
	if stateID, exists, err := service.Store.AgentSessionHead(ctx, "codex", "cleanup-session"); err != nil || exists {
		t.Fatalf("terminal session pointer = %q, exists=%v, err=%v", stateID, exists, err)
	}
	next, err := service.BeginPrompt(ctx, "Start new work", "", "codex", "cleanup-session")
	if err != nil {
		t.Fatal(err)
	}
	if next.Attempt.ID == started.Attempt.ID || next.Workspace == started.Workspace {
		t.Fatalf("completed session reopened old attempt: %#v", next.Attempt)
	}
}

func TestBeginRecoversAcceptedSessionWhoseWorkspaceWasAlreadyReclaimed(t *testing.T) {
	ctx := context.Background()
	service, _ := newTestProject(t, map[string]string{"base.txt": "base\n"})
	started, err := service.BeginPrompt(ctx, "Land a feature", "", "codex", "v105-session")
	if err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(started.Workspace, "feature.txt"), "done\n")
	proposal, err := service.Propose(ctx, started.Prompt.ID, "Add feature")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Accept(ctx, proposal.Proposal.ID, nil); err != nil {
		t.Fatal(err)
	}
	// Reproduce v1.0.5: the worktree disappeared while the agent session still
	// pointed at its accepted attempt.
	if err := service.Repo.RemoveWorktree(ctx, started.Workspace, true); err != nil {
		t.Fatal(err)
	}
	if _, exists, err := service.Store.AgentSessionHead(ctx, "codex", "v105-session"); err != nil || !exists {
		t.Fatalf("expected stale v1.0.5 session pointer, exists=%v err=%v", exists, err)
	}
	next, err := service.BeginPrompt(ctx, "Start the next task", "", "codex", "v105-session")
	if err != nil {
		t.Fatal(err)
	}
	if next.Attempt.ID == started.Attempt.ID || next.Workspace == started.Workspace {
		t.Fatalf("reclaimed accepted workspace was reopened: %#v", next.Attempt)
	}
}

func TestCompletePromptPreservesActiveWorkspaceWithSourceChanges(t *testing.T) {
	ctx := context.Background()
	service, _ := newTestProject(t, map[string]string{"base.txt": "base\n"})
	started, err := service.CreatePrompt(ctx, "Begin implementation", "", "codex")
	if err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(started.Workspace, "unfinished.txt"), "unfinished\n")
	result, err := service.CompletePrompt(ctx, started.Prompt.ID, "Work remains", "The incomplete source changes were preserved.")
	if err != nil {
		t.Fatal(err)
	}
	if result.Cleanup == nil || result.Cleanup.Removed != 0 {
		t.Fatalf("cleanup = %#v", result.Cleanup)
	}
	if _, err := os.Stat(started.Workspace); err != nil {
		t.Fatalf("active dirty workspace was removed: %v", err)
	}
	attempt, err := service.Store.GetAttempt(ctx, started.Attempt.ID)
	if err != nil {
		t.Fatal(err)
	}
	if attempt.Status != "active" {
		t.Fatalf("attempt status = %q, want active", attempt.Status)
	}
}

func TestGCAllParksDirtyAttemptAndResumesOriginalSession(t *testing.T) {
	ctx := context.Background()
	service, _ := newTestProject(t, map[string]string{"base.txt": "base\n"})
	started, err := service.BeginPrompt(ctx, "Start unfinished work", "", "codex", "parked-session")
	if err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(started.Workspace, "unfinished.txt"), "preserve me\n")

	cleanup, err := service.CleanupWorkspacesWithOptions(ctx, WorkspaceCleanupOptions{
		IncludeAbandoned: true,
		AbandonedAfter:   0,
	})
	if err != nil {
		t.Fatal(err)
	}
	if cleanup.Parked != 1 || cleanup.Removed != 1 || cleanup.ReclaimedBytes == 0 {
		t.Fatalf("cleanup = %#v", cleanup)
	}
	if _, err := os.Stat(started.Workspace); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("parked workspace still exists: %v", err)
	}
	parked, err := service.Store.GetAttempt(ctx, started.Attempt.ID)
	if err != nil {
		t.Fatal(err)
	}
	if parked.Status != "parked" {
		t.Fatalf("attempt status = %q, want parked", parked.Status)
	}
	parkedHead, err := service.Store.GetState(ctx, parked.HeadStateID)
	if err != nil {
		t.Fatal(err)
	}
	if parkedHead.Kind != StateCheckpoint {
		t.Fatalf("parked head kind = %q, want checkpoint", parkedHead.Kind)
	}

	resumed, err := service.BeginPrompt(ctx, "Resume the unfinished work", "", "codex", "parked-session")
	if err != nil {
		t.Fatal(err)
	}
	if resumed.Attempt.ID != started.Attempt.ID || resumed.Workspace != started.Workspace {
		t.Fatalf("resumed attempt = %#v, want original attempt %s", resumed.Attempt, started.Attempt.ID)
	}
	contents, err := os.ReadFile(filepath.Join(resumed.Workspace, "unfinished.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(contents) != "preserve me\n" {
		t.Fatalf("resumed contents = %q", contents)
	}
	if resumed.Attempt.Status != "active" {
		t.Fatalf("resumed attempt status = %q, want active", resumed.Attempt.Status)
	}
}

func TestParkedAttemptStatusUsesPortablePromptVocabulary(t *testing.T) {
	if got := portablePromptStatus("parked"); got != "active" {
		t.Fatalf("portable parked status = %q, want active", got)
	}
	if got := portablePromptStatus("completed"); got != "completed" {
		t.Fatalf("portable completed status = %q", got)
	}
}

func TestSchemaSevenMigrationPreservesExistingAcceptedState(t *testing.T) {
	t.Setenv("HOP_ROOT", "")
	ctx := context.Background()
	service, initial := newTestProject(t, map[string]string{"base.txt": "base\n"})
	root := service.Root
	if _, err := service.Store.db.ExecContext(ctx, "ALTER TABLE states DROP COLUMN provenance_json"); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Store.db.ExecContext(ctx, "PRAGMA user_version = 6"); err != nil {
		t.Fatal(err)
	}
	if err := service.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenProject(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	head, err := reopened.Store.AcceptedHead(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if head.ID != initial.ID || head.GitCommit != initial.GitCommit {
		t.Fatalf("migration changed accepted state: %#v, want %#v", head, initial)
	}
	var version int
	if err := reopened.Store.db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil || version != 7 {
		t.Fatalf("schema version = %d, err=%v", version, err)
	}
	if _, found, err := reopened.Store.PublicationForState(ctx, initial.ID); err != nil || found {
		t.Fatalf("legacy publication = found %t, err=%v", found, err)
	}
}

func TestGCAllExcludesCurrentAttempt(t *testing.T) {
	ctx := context.Background()
	service, _ := newTestProject(t, map[string]string{"base.txt": "base\n"})
	started, err := service.BeginPrompt(ctx, "Keep this workspace", "", "codex", "current-session")
	if err != nil {
		t.Fatal(err)
	}
	cleanup, err := service.CleanupWorkspacesWithOptions(ctx, WorkspaceCleanupOptions{
		IncludeAbandoned: true,
		AbandonedAfter:   0,
		ExcludeAttemptID: started.Attempt.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if cleanup.Parked != 0 || cleanup.Removed != 0 {
		t.Fatalf("cleanup = %#v", cleanup)
	}
	if _, err := os.Stat(started.Workspace); err != nil {
		t.Fatalf("excluded workspace was removed: %v", err)
	}
}

func TestGCAllArchivesDirtyTerminalWorkspace(t *testing.T) {
	ctx := context.Background()
	service, _ := newTestProject(t, map[string]string{"base.txt": "base\n"})
	started, err := service.CreatePrompt(ctx, "Land then preserve later work", "", "codex")
	if err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(started.Workspace, "feature.txt"), "landed\n")
	proposal, err := service.Propose(ctx, started.Prompt.ID, "Land feature")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Accept(ctx, proposal.Proposal.ID, nil); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(started.Workspace, "late.txt"), "archive me\n")

	cleanup, err := service.CleanupWorkspacesWithOptions(ctx, WorkspaceCleanupOptions{
		IncludeAbandoned:     true,
		AbandonedAfter:       0,
		ArchiveDirtyTerminal: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if cleanup.Removed != 1 || len(cleanup.Preserved) != 0 {
		t.Fatalf("cleanup = %#v", cleanup)
	}
	attempt, err := service.Store.GetAttempt(ctx, started.Attempt.ID)
	if err != nil {
		t.Fatal(err)
	}
	head, err := service.Store.GetState(ctx, attempt.HeadStateID)
	if err != nil {
		t.Fatal(err)
	}
	if head.Kind != StateCheckpoint {
		t.Fatalf("archived terminal head kind = %q, want checkpoint", head.Kind)
	}
	materialized := filepath.Join(t.TempDir(), "archived")
	if _, err := service.Repo.AddDetachedWorktree(ctx, materialized, head.GitCommit); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = service.Repo.RemoveWorktree(ctx, materialized, true) })
	contents, err := os.ReadFile(filepath.Join(materialized, "late.txt"))
	if err != nil || string(contents) != "archive me\n" {
		t.Fatalf("archived late work = %q, err=%v", contents, err)
	}
}

func TestCLIGCAllProtectsCallingWorkspace(t *testing.T) {
	t.Setenv("HOP_ROOT", "")
	t.Setenv("HOP_ATTEMPT_ID", "")
	ctx := context.Background()
	service, _ := newTestProject(t, map[string]string{"base.txt": "base\n"})
	started, err := service.BeginPrompt(ctx, "Keep the calling workspace", "", "codex", "gc-caller")
	if err != nil {
		t.Fatal(err)
	}
	root := service.Root
	if err := service.Close(); err != nil {
		t.Fatal(err)
	}
	previousDirectory, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(started.Workspace); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(previousDirectory) })

	var stdout, stderr bytes.Buffer
	if code := RunCLI([]string{"gc", "--all", "--json"}, &stdout, &stderr); code != 0 {
		t.Fatalf("hop gc exited %d\nstdout: %s\nstderr: %s", code, stdout.String(), stderr.String())
	}
	var response struct {
		Data WorkspaceCleanupResult `json:"data"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Data.Parked != 0 || response.Data.Removed != 0 {
		t.Fatalf("cleanup = %#v", response.Data)
	}
	if _, err := os.Stat(started.Workspace); err != nil {
		t.Fatalf("calling workspace was removed: %v", err)
	}

	reopened, err := OpenProject(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	attempt, err := reopened.Store.GetAttempt(ctx, started.Attempt.ID)
	if err != nil {
		t.Fatal(err)
	}
	if attempt.Status != "active" {
		t.Fatalf("calling attempt status = %q, want active", attempt.Status)
	}
}

func TestCLIBeginAutomaticallyParksDayOldAttempt(t *testing.T) {
	t.Setenv("HOP_ROOT", "")
	ctx := context.Background()
	service, _ := newTestProject(t, map[string]string{"base.txt": "base\n"})
	old, err := service.BeginPrompt(ctx, "Abandon this thread", "", "codex", "old-session")
	if err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(old.Workspace, "unfinished.txt"), "keep me\n")
	oldTime := time.Now().UTC().Add(-DefaultAbandonedAfter - time.Hour)
	if _, err := service.Store.db.ExecContext(ctx, `UPDATE attempts SET created_at = ? WHERE id = ?`, formatTime(oldTime), old.Attempt.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Store.db.ExecContext(ctx, `UPDATE states SET created_at = ? WHERE attempt_id = ?`, formatTime(oldTime), old.Attempt.ID); err != nil {
		t.Fatal(err)
	}
	if err := filepath.Walk(old.Workspace, func(path string, _ os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		return os.Chtimes(path, oldTime, oldTime)
	}); err != nil {
		t.Fatal(err)
	}
	root := service.Root
	if err := service.Close(); err != nil {
		t.Fatal(err)
	}
	previousDirectory, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(root); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(previousDirectory) })

	var stdout, stderr bytes.Buffer
	code := RunCLIWithInput([]string{"begin", "--json", "--agent", "codex", "--session", "new-session", "--heredoc"}, strings.NewReader("Start fresh\n"), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("hop begin exited %d\nstdout: %s\nstderr: %s", code, stdout.String(), stderr.String())
	}
	var response struct {
		Data BeginResult `json:"data"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Data.Cleanup == nil || response.Data.Cleanup.Parked != 1 || response.Data.Cleanup.Removed != 1 {
		t.Fatalf("begin cleanup = %#v", response.Data.Cleanup)
	}
	if _, err := os.Stat(old.Workspace); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("old workspace still exists: %v", err)
	}
	reopened, err := OpenProject(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	parked, err := reopened.Store.GetAttempt(ctx, old.Attempt.ID)
	if err != nil {
		t.Fatal(err)
	}
	if parked.Status != "parked" {
		t.Fatalf("old attempt status = %q, want parked", parked.Status)
	}
}

func TestCompletePromptReclaimsAcceptedWorkspaceButPreservesLateChanges(t *testing.T) {
	ctx := context.Background()
	service, _ := newTestProject(t, map[string]string{"base.txt": "base\n"})
	started, err := service.CreatePrompt(ctx, "Land a feature", "", "codex")
	if err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(started.Workspace, "feature.txt"), "done\n")
	proposal, err := service.Propose(ctx, started.Prompt.ID, "Add feature")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Accept(ctx, proposal.Proposal.ID, nil); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(started.Workspace, "late.txt"), "preserve me\n")
	result, err := service.CompletePrompt(ctx, started.Prompt.ID, "Feature landed", "The feature landed and later source edits remain available.")
	if err != nil {
		t.Fatal(err)
	}
	if result.Cleanup == nil || len(result.Cleanup.Preserved) != 1 {
		t.Fatalf("cleanup = %#v", result.Cleanup)
	}
	if _, err := os.Stat(filepath.Join(started.Workspace, "late.txt")); err != nil {
		t.Fatalf("late terminal source changes were removed: %v", err)
	}
	if err := os.Remove(filepath.Join(started.Workspace, "late.txt")); err != nil {
		t.Fatal(err)
	}
	cleanup, err := service.CleanupWorkspaces(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if cleanup.Removed != 1 {
		t.Fatalf("retry cleanup = %#v", cleanup)
	}
	if _, err := os.Stat(started.Workspace); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("source-clean accepted workspace still exists: %v", err)
	}
}

func TestExportRemovesPreviouslyPublishedReconciliationPrompt(t *testing.T) {
	ctx := context.Background()
	service, _ := newTestProject(t, map[string]string{"base.txt": "base\n"})
	result, err := service.CreatePrompt(ctx, "Resolve proposal R_TEST against accepted state A_TEST", "", "codex")
	if err != nil {
		t.Fatal(err)
	}
	recordDir := filepath.Join(service.Root, ".hop", "records", "prompts")
	if err := os.MkdirAll(recordDir, 0o755); err != nil {
		t.Fatal(err)
	}
	recordPath := filepath.Join(recordDir, result.Prompt.ID+".json")
	if err := os.WriteFile(recordPath, []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := service.ExportPromptLedger(ctx, service.Root); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(recordPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("internal reconciliation record still exists: %v", err)
	}
}

func TestProposeRespectsSuppressedPromptManifest(t *testing.T) {
	ctx := context.Background()
	service, _ := newTestProject(t, map[string]string{"base.txt": "base\n"})
	result, err := service.CreatePrompt(ctx, "Internal controller instruction", "", "codex")
	if err != nil {
		t.Fatal(err)
	}
	manifest := fmt.Sprintf("{\"prompt_ids\":[%q]}\n", result.Prompt.ID)
	writeTestFile(t, filepath.Join(result.Workspace, ".hop", "records", "suppressed.json"), manifest)
	writeTestFile(t, filepath.Join(result.Workspace, "feature.txt"), "done\n")
	proposal, err := service.Propose(ctx, result.Prompt.ID, "Apply the user-facing change")
	if err != nil {
		t.Fatal(err)
	}
	materialized := filepath.Join(t.TempDir(), "proposal")
	if _, err := service.Repo.AddDetachedWorktree(ctx, materialized, proposal.Proposal.GitCommit); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = service.Repo.RemoveWorktree(ctx, materialized, true) })
	if _, err := os.Stat(filepath.Join(materialized, ".hop", "records", "prompts", result.Prompt.ID+".json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("suppressed prompt was published: %v", err)
	}
}

func TestOpenProjectWaitsForInitializationLock(t *testing.T) {
	t.Setenv("HOP_ROOT", "")
	service, _ := newTestProject(t, map[string]string{"base.txt": "base\n"})
	root := service.Root
	if err := service.Close(); err != nil {
		t.Fatal(err)
	}
	release, err := acquireProjectLock(context.Background(), root, "init")
	if err != nil {
		t.Fatal(err)
	}
	opened := make(chan error, 1)
	go func() {
		project, openErr := OpenProject(root)
		if openErr == nil {
			openErr = project.Close()
		}
		opened <- openErr
	}()
	select {
	case err := <-opened:
		release()
		t.Fatalf("OpenProject returned before initialization lock release: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	release()
	select {
	case err := <-opened:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("OpenProject did not resume after initialization lock release")
	}
}

func TestCLIJSONWorkflow(t *testing.T) {
	t.Setenv("HOP_ROOT", "")
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
	if contents, err := os.ReadFile(filepath.Join(root, "cli.txt")); err != nil || string(contents) != "cli\n" {
		t.Fatalf("visible root was not materialized: contents=%q err=%v", string(contents), err)
	}
	status := runCLIJSONTest(t, []string{"status", "--json"})
	statusData := objectField(t, status, "data")
	head := objectField(t, statusData, "accepted_head")
	if stringField(t, head, "id") != stringField(t, acceptedState, "id") {
		t.Fatal("status accepted head does not match landed state")
	}
	if stringField(t, statusData, "root_status") != "synchronized" {
		t.Fatalf("root status = %q", stringField(t, statusData, "root_status"))
	}
}

func TestCLICompleteRecordsExactFinalResponse(t *testing.T) {
	t.Setenv("HOP_ROOT", "")
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
	started := runCLIJSONTest(t, []string{"begin", "--agent", "codex", "--json", "Check the service"})
	promptID := stringField(t, objectField(t, objectField(t, started, "data"), "prompt"), "id")
	finalResponse := "The service is healthy.\n\n- Database: healthy\n- API: healthy"
	completed := runCLIJSONInputTest(t,
		[]string{"complete", "--summary", "Service checks pass", "--heredoc", "--json", promptID},
		finalResponse+"\n")
	completion := objectField(t, objectField(t, completed, "data"), "completion")
	if stringField(t, completion, "state_id") != promptID || stringField(t, completion, "summary") != "Service checks pass" || stringField(t, completion, "final_response") != finalResponse {
		t.Fatalf("completion = %#v", completion)
	}

	runCLIJSONTest(t, []string{"export", "--output", ".", "--json"})
	contents, err := os.ReadFile(filepath.Join(root, ".hop", "records", "prompts", promptID+".json"))
	if err != nil {
		t.Fatal(err)
	}
	var record PortablePromptRecord
	if err := json.Unmarshal(contents, &record); err != nil {
		t.Fatal(err)
	}
	if record.ResponseSummary != "Service checks pass" || record.FinalResponse != finalResponse {
		t.Fatalf("exported completion = %#v", record)
	}
}

func TestCLIExportOmitsActivePromptRecords(t *testing.T) {
	t.Setenv("HOP_ROOT", "")
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
	runCLIJSONTest(t, []string{"begin", "--agent", "codex", "--json", "Publish prompt records"})
	runCLIJSONTest(t, []string{"export", "--output", ".", "--json"})
	entries, err := filepath.Glob(filepath.Join(root, ".hop", "records", "prompts", "P_*.json"))
	if err != nil || len(entries) != 0 {
		t.Fatalf("portable prompt records = %v, err=%v", entries, err)
	}
}

func TestCLIBeginAutoInitializesAndContinuesCodexSession(t *testing.T) {
	t.Setenv("HOP_ROOT", "")
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

	firstPrompt := "  Add a Desktop-safe file\nwithout changing this spacing.  "
	started := runCLIJSONInputTest(t,
		[]string{"begin", "--agent", "codex", "--session", "thread-test", "--heredoc", "--json"},
		firstPrompt+"\n")
	first := objectField(t, started, "data")
	if initialized, _ := first["initialized"].(bool); !initialized {
		t.Fatal("begin did not report automatic Hop initialization")
	}
	firstState := objectField(t, first, "prompt")
	if got := stringField(t, firstState, "prompt"); got != firstPrompt {
		t.Fatalf("heredoc prompt = %q, want %q", got, firstPrompt)
	}
	firstAttempt := objectField(t, first, "attempt")
	workspace := stringField(t, first, "workspace")
	writeTestFile(t, filepath.Join(workspace, "desktop.txt"), "first turn\n")

	followupPrompt := "Now preserve this final newline.\n"
	followed := runCLIJSONInputTest(t,
		[]string{"begin", "--agent", "codex", "--session", "thread-test", "--stdin", "--json"},
		followupPrompt)
	second := objectField(t, followed, "data")
	if initialized, _ := second["initialized"].(bool); initialized {
		t.Fatal("follow-up begin unexpectedly reinitialized Hop")
	}
	secondState := objectField(t, second, "prompt")
	if got := stringField(t, secondState, "prompt"); got != followupPrompt {
		t.Fatalf("stdin prompt = %q, want %q", got, followupPrompt)
	}
	secondAttempt := objectField(t, second, "attempt")
	if stringField(t, secondAttempt, "id") != stringField(t, firstAttempt, "id") {
		t.Fatal("Codex session follow-up created a new attempt")
	}
	if stringField(t, second, "workspace") != workspace {
		t.Fatal("Codex session follow-up changed workspaces")
	}
	checkpoint := objectField(t, second, "checkpoint")
	if stringField(t, checkpoint, "kind") != string(StateCheckpoint) {
		t.Fatal("Codex session follow-up did not checkpoint prior effects")
	}

	service, err := OpenProject(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = service.Close() })
	head, exists, err := service.Store.AgentSessionHead(context.Background(), "codex", "thread-test")
	if err != nil {
		t.Fatal(err)
	}
	if !exists || head != stringField(t, secondState, "id") {
		t.Fatalf("session head = %q, %v; want second prompt", head, exists)
	}
	assertTreeFiles(t, service, stringField(t, checkpoint, "git_commit"), map[string]string{
		"base.txt":    "base\n",
		"desktop.txt": "first turn\n",
	})
}

func TestBeginRollsAcceptedReconciliationSessionToCurrentHead(t *testing.T) {
	ctx := context.Background()
	service, _ := newTestProject(t, map[string]string{"shared.txt": "color=base\n"})

	session, err := service.BeginPrompt(ctx, "Use blue", "", "codex", "thread-rollover")
	if err != nil {
		t.Fatal(err)
	}
	concurrent, err := service.CreatePrompt(ctx, "Use red", "", "other")
	if err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(session.Workspace, "shared.txt"), "color=blue\n")
	writeTestFile(t, filepath.Join(concurrent.Workspace, "shared.txt"), "color=red\n")

	sessionProposal, err := service.Propose(ctx, session.Prompt.ID, "Blue")
	if err != nil {
		t.Fatal(err)
	}
	concurrentProposal, err := service.Propose(ctx, concurrent.Prompt.ID, "Red")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Land(ctx, concurrentProposal.Proposal.ID, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Land(ctx, sessionProposal.Proposal.ID, nil); err == nil {
		t.Fatal("stale session proposal unexpectedly landed without reconciliation")
	} else {
		var conflict *ConflictError
		if !errors.As(err, &conflict) {
			t.Fatalf("land error = %v, want ConflictError", err)
		}
	}

	reconciliation, err := service.Refresh(ctx, sessionProposal.Proposal.ID)
	if err != nil {
		t.Fatal(err)
	}
	reconciliationHead, exists, err := service.Store.AgentSessionHead(ctx, "codex", "thread-rollover")
	if err != nil {
		t.Fatal(err)
	}
	if !exists || reconciliationHead != reconciliation.Prompt.ID {
		t.Fatalf("session head = %q, %v; want reconciliation prompt %s", reconciliationHead, exists, reconciliation.Prompt.ID)
	}
	writeTestFile(t, filepath.Join(reconciliation.Workspace, "shared.txt"), "color=red-blue\n")
	if _, err := service.RunCheck(ctx, reconciliation.Prompt.ID, []string{
		"sh", "-c", `test "$(cat shared.txt)" = "color=red-blue"`,
	}); err != nil {
		t.Fatal(err)
	}
	resolvedProposal, err := service.Propose(ctx, reconciliation.Prompt.ID, "Resolve red and blue")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Land(ctx, resolvedProposal.Proposal.ID, nil); err != nil {
		t.Fatal(err)
	}
	sourceAttempt, err := service.Store.GetAttempt(ctx, session.Attempt.ID)
	if err != nil {
		t.Fatal(err)
	}
	if sourceAttempt.Status != "completed" {
		t.Fatalf("source attempt status = %q, want completed", sourceAttempt.Status)
	}

	// Rollover must use the latest global accepted state, not merely this task's
	// accepted outcome.
	later, err := service.CreatePrompt(ctx, "Add a later accepted change", "", "other")
	if err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(later.Workspace, "later.txt"), "later\n")
	laterProposal, err := service.Propose(ctx, later.Prompt.ID, "Later accepted change")
	if err != nil {
		t.Fatal(err)
	}
	laterAccepted, err := service.Land(ctx, laterProposal.Proposal.ID, nil)
	if err != nil {
		t.Fatal(err)
	}

	next, err := service.BeginPrompt(ctx, "Next request", "", "codex", "thread-rollover")
	if err != nil {
		t.Fatal(err)
	}
	assertFreshRollover := func(label string, result PromptResult) {
		t.Helper()
		if result.Checkpoint != nil {
			t.Fatalf("%s continued accepted session with checkpoint %s", label, result.Checkpoint.ID)
		}
		if result.Attempt.ID == session.Attempt.ID || result.Attempt.ID == reconciliation.Attempt.ID {
			t.Fatalf("%s reused completed attempt %s", label, result.Attempt.ID)
		}
		if result.Task.ID == session.Task.ID {
			t.Fatalf("%s reopened accepted task %s", label, result.Task.ID)
		}
		if result.Attempt.BaseStateID != laterAccepted.State.ID {
			t.Fatalf("%s base = %s, want %s", label, result.Attempt.BaseStateID, laterAccepted.State.ID)
		}
		if result.Prompt.CanonicalAnchorID != laterAccepted.State.ID ||
			result.Prompt.SourceTree != laterAccepted.State.SourceTree ||
			result.Prompt.GitCommit != laterAccepted.State.GitCommit {
			t.Fatalf("%s prompt was not rooted at latest accepted head: %#v; accepted=%#v", label, result.Prompt, laterAccepted.State)
		}
		if contents, err := os.ReadFile(filepath.Join(result.Workspace, "shared.txt")); err != nil || string(contents) != "color=red-blue\n" {
			t.Fatalf("%s shared workspace = %q, err=%v", label, string(contents), err)
		}
		if contents, err := os.ReadFile(filepath.Join(result.Workspace, "later.txt")); err != nil || string(contents) != "later\n" {
			t.Fatalf("%s later workspace = %q, err=%v", label, string(contents), err)
		}
	}
	assertFreshRollover("normal reconciliation session", next)
	head, exists, err := service.Store.AgentSessionHead(ctx, "codex", "thread-rollover")
	if err != nil {
		t.Fatal(err)
	}
	if !exists || head != next.Prompt.ID {
		t.Fatalf("session head = %q, %v; want %s", head, exists, next.Prompt.ID)
	}

	// Older Hop builds could reactivate both records after acceptance. The
	// immutable accepted outcome, not these mutable statuses, must drive rollover.
	if err := service.Store.UpdateAttemptStatus(ctx, session.Attempt.ID, "active"); err != nil {
		t.Fatal(err)
	}
	if err := service.Store.UpdateTaskStatus(ctx, session.Task.ID, "active"); err != nil {
		t.Fatal(err)
	}
	if err := service.Store.SetAgentSessionHead(ctx, "codex", "legacy-thread-rollover", session.Prompt.ID); err != nil {
		t.Fatal(err)
	}
	legacyNext, err := service.BeginPrompt(ctx, "Legacy next request", "", "codex", "legacy-thread-rollover")
	if err != nil {
		t.Fatal(err)
	}
	assertFreshRollover("legacy session", legacyNext)
	sourceAttempt, err = service.Store.GetAttempt(ctx, session.Attempt.ID)
	if err != nil {
		t.Fatal(err)
	}
	if sourceAttempt.Status == "active" {
		t.Fatalf("source attempt %s was left active", sourceAttempt.ID)
	}
	head, exists, err = service.Store.AgentSessionHead(ctx, "codex", "legacy-thread-rollover")
	if err != nil {
		t.Fatal(err)
	}
	if !exists || head != legacyNext.Prompt.ID {
		t.Fatalf("legacy session head = %q, %v; want %s", head, exists, legacyNext.Prompt.ID)
	}
}

func TestLandRejectsProposalSupersededByFollowup(t *testing.T) {
	ctx := context.Background()
	service, initial := newTestProject(t, map[string]string{"base.txt": "base\n"})
	started, err := service.BeginPrompt(ctx, "Create a change", "", "codex", "thread-stale-proposal")
	if err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(started.Workspace, "change.txt"), "first\n")
	proposal, err := service.Propose(ctx, started.Prompt.ID, "First change")
	if err != nil {
		t.Fatal(err)
	}
	followup, err := service.BeginPrompt(ctx, "Refine that change", "", "codex", "thread-stale-proposal")
	if err != nil {
		t.Fatal(err)
	}
	if followup.Attempt.ID != started.Attempt.ID || followup.Checkpoint == nil {
		t.Fatalf("follow-up did not advance the original attempt: %#v", followup)
	}
	if _, err := service.Land(ctx, proposal.Proposal.ID, nil); !errors.Is(err, ErrAttemptHeadChanged) {
		t.Fatalf("superseded proposal land error = %v, want ErrAttemptHeadChanged", err)
	}
	parents := canonicalizeParents([]Parent{
		{StateID: initial.ID, Role: "canonical_parent", Order: 0},
		{StateID: proposal.Proposal.ID, Role: "proposal_parent", Order: 1},
	})
	accepted := State{
		ID:                newID("a"),
		Kind:              StateAccepted,
		TaskID:            proposal.Proposal.TaskID,
		AttemptID:         proposal.Proposal.AttemptID,
		CanonicalAnchorID: initial.ID,
		SourceTree:        proposal.Proposal.SourceTree,
		GitCommit:         proposal.Proposal.GitCommit,
		Summary:           proposal.Proposal.Summary,
		Agent:             proposal.Proposal.Agent,
		CreatedAt:         time.Now().UTC(),
		Parents:           parents,
	}
	accepted.Digest, err = digestState(accepted, parents)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Store.CASAccept(ctx, initial.ID, accepted, parents); !errors.Is(err, ErrAttemptHeadChanged) {
		t.Fatalf("transactional stale proposal error = %v, want ErrAttemptHeadChanged", err)
	}
	head, err := service.Store.AcceptedHead(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if head.ID != initial.ID {
		t.Fatalf("superseded proposal advanced accepted head to %s", head.ID)
	}
}

func TestBeginKeepsUnfinishedStateCreatedAfterAcceptedOutcome(t *testing.T) {
	ctx := context.Background()
	service, _ := newTestProject(t, map[string]string{"base.txt": "base\n"})
	started, err := service.BeginPrompt(ctx, "Land the first change", "", "codex", "thread-post-accept")
	if err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(started.Workspace, "landed.txt"), "landed\n")
	proposal, err := service.Propose(ctx, started.Prompt.ID, "Landed change")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Land(ctx, proposal.Proposal.ID, nil); err != nil {
		t.Fatal(err)
	}

	// Although agents must not mutate a frozen proposal, preserve any such
	// residual work rather than silently rolling it away.
	writeTestFile(t, filepath.Join(started.Workspace, "pending.txt"), "pending\n")
	continued, err := service.BeginPrompt(ctx, "Preserve the pending work", "", "codex", "thread-post-accept")
	if err != nil {
		t.Fatal(err)
	}
	if continued.Attempt.ID != started.Attempt.ID || continued.Checkpoint == nil {
		t.Fatalf("dirty accepted workspace was not preserved: %#v", continued)
	}
	assertTreeFiles(t, service, continued.Checkpoint.GitCommit, map[string]string{
		"base.txt":    "base\n",
		"landed.txt":  "landed\n",
		"pending.txt": "pending\n",
	})
	again, err := service.BeginPrompt(ctx, "Continue that pending work", "", "codex", "thread-post-accept")
	if err != nil {
		t.Fatal(err)
	}
	if again.Attempt.ID != started.Attempt.ID || again.Checkpoint == nil {
		t.Fatalf("post-acceptance prompt lineage was abandoned: %#v", again)
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

func TestCLILandConflictReturnsAutomaticReconciliation(t *testing.T) {
	t.Setenv("HOP_ROOT", "")
	ctx := context.Background()
	service, _ := newTestProject(t, map[string]string{"shared.txt": "base\n"})
	first, err := service.CreatePrompt(ctx, "First", "", "one")
	if err != nil {
		t.Fatal(err)
	}
	second, err := service.CreatePrompt(ctx, "Second", "", "two")
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
	if _, err := service.Land(ctx, firstProposal.Proposal.ID, nil); err != nil {
		t.Fatal(err)
	}
	root := service.Root
	if err := service.Close(); err != nil {
		t.Fatal(err)
	}
	previousDirectory, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(root); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(previousDirectory) })

	var stdout, stderr bytes.Buffer
	code := RunCLI([]string{"land", secondProposal.Proposal.ID, "--json"}, &stdout, &stderr)
	if code != 20 {
		t.Fatalf("land exit = %d, want 20\nstdout: %s\nstderr: %s", code, stdout.String(), stderr.String())
	}
	var response map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response["next_command"] == nil {
		t.Fatalf("conflict response omitted next command: %s", stdout.String())
	}
	reconciliation := objectField(t, response, "reconciliation")
	prompt := objectField(t, reconciliation, "prompt")
	if stringField(t, prompt, "id") == "" || stringField(t, reconciliation, "workspace") == "" {
		t.Fatalf("reconciliation response omitted prompt/workspace: %s", stdout.String())
	}
	if status := stringField(t, objectField(t, reconciliation, "task"), "status"); status != "reconciling" {
		t.Fatalf("reconciliation task status = %q, want reconciling", status)
	}
	if status := stringField(t, objectField(t, reconciliation, "attempt"), "status"); status != "reconciling" {
		t.Fatalf("reconciliation attempt status = %q, want reconciling", status)
	}
	conflicts, ok := reconciliation["conflicts"].([]any)
	if !ok || len(conflicts) != 1 || conflicts[0] != "shared.txt" {
		t.Fatalf("conflicts = %#v", reconciliation["conflicts"])
	}
	if contents, err := os.ReadFile(filepath.Join(root, "shared.txt")); err != nil || string(contents) != "first\n" {
		t.Fatalf("conflicted land changed visible root: %q, %v", string(contents), err)
	}
}

func TestCLIVersionUsesReleaseLinkerValue(t *testing.T) {
	previous := Version
	Version = "v1.2.3"
	t.Cleanup(func() { Version = previous })

	var stdout, stderr bytes.Buffer
	if code := RunCLI([]string{"version", "--json"}, &stdout, &stderr); code != 0 {
		t.Fatalf("version exited %d: %s", code, stderr.String())
	}
	var response map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if got, _ := response["version"].(string); got != "1.2.3" {
		t.Fatalf("version = %q, want 1.2.3", got)
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
	case <-time.After(10 * time.Second):
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
	var committed *CommittedStateError
	if err != nil && !errors.As(err, &committed) {
		t.Fatal(err)
	}
	if committed != nil {
		undo = committed.State
		if !strings.Contains(committed.Error(), "intended local branch has no commit tip") {
			t.Fatalf("unborn undo warning = %v", committed)
		}
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

func TestAcceptedCommitAttributesPromptingUserAndRetainsHopCommitter(t *testing.T) {
	ctx := context.Background()
	service, _ := newTestProject(t, map[string]string{"base.txt": "base\n"})
	runGitTest(t, service.Root, "config", "user.name", "Prompting User")
	runGitTest(t, service.Root, "config", "user.email", "prompter@example.com")

	attempt, err := service.CreatePrompt(ctx, "Add authored change", "", "codex")
	if err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(attempt.Workspace, "authored.txt"), "authored\n")
	proposal, err := service.Propose(ctx, attempt.Prompt.ID, "Add authored change")
	if err != nil {
		t.Fatal(err)
	}
	accepted, err := service.Accept(ctx, proposal.Proposal.ID, nil)
	if err != nil {
		t.Fatal(err)
	}

	metadata := runGitTest(t, service.Root, "show", "-s", "--format=%an <%ae>%n%cn <%ce>", accepted.State.GitCommit)
	want := "Prompting User <prompter@example.com>\nHop <hop@localhost>"
	if metadata != want {
		t.Fatalf("accepted commit identity = %q, want %q", metadata, want)
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

func TestOverlappingSameFileIndependentHunksAutoMerge(t *testing.T) {
	ctx := context.Background()
	service, _ := newTestProject(t, map[string]string{
		"shared.txt": "header\nmiddle\nfooter\n",
	})
	first, err := service.CreatePrompt(ctx, "Change header", "", "one")
	if err != nil {
		t.Fatal(err)
	}
	second, err := service.CreatePrompt(ctx, "Change footer", "", "two")
	if err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(first.Workspace, "shared.txt"), "HEADER\nmiddle\nfooter\n")
	writeTestFile(t, filepath.Join(second.Workspace, "shared.txt"), "header\nmiddle\nFOOTER\n")
	firstProposal, err := service.Propose(ctx, first.Prompt.ID, "Header")
	if err != nil {
		t.Fatal(err)
	}
	secondProposal, err := service.Propose(ctx, second.Prompt.ID, "Footer")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Accept(ctx, firstProposal.Proposal.ID, nil); err != nil {
		t.Fatal(err)
	}
	merged, err := service.Accept(ctx, secondProposal.Proposal.ID, []string{
		"sh", "-c", `grep -qx HEADER shared.txt && grep -qx FOOTER shared.txt`,
	})
	if err != nil {
		var failed *CheckFailedError
		if errors.As(err, &failed) {
			contents, showErr := service.Repo.run(ctx, nil, nil, "show", failed.Check.TreeHash+":shared.txt")
			t.Fatalf("mergeable same-file proposal failed validation with shared.txt=%q (show error %v): %v", contents, showErr, err)
		}
		t.Fatalf("mergeable same-file proposal was blocked: %v", err)
	}
	assertTreeFiles(t, service, merged.State.GitCommit, map[string]string{
		"shared.txt": "HEADER\nmiddle\nFOOTER\n",
	})
	if len(merged.ProposalPaths) != 1 || len(merged.CurrentPaths) != 1 ||
		merged.ProposalPaths[0] != "shared.txt" || merged.CurrentPaths[0] != "shared.txt" {
		t.Fatalf("overlap audit paths were not retained: proposal=%v current=%v", merged.ProposalPaths, merged.CurrentPaths)
	}
}

func TestSnapshotCapturesRacySameSizeRewrite(t *testing.T) {
	ctx := context.Background()
	service, _ := newTestProject(t, map[string]string{"shared.txt": "lower\n"})
	runGitTest(t, service.Root, "config", "core.trustctime", "false")
	runGitTest(t, service.Root, "config", "core.checkStat", "minimal")
	prompt, err := service.CreatePrompt(ctx, "Uppercase the value", "", "agent")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(prompt.Workspace, "shared.txt")
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, path, "UPPER\n")
	if err := os.Chtimes(path, before.ModTime(), before.ModTime()); err != nil {
		t.Fatal(err)
	}
	proposal, err := service.Propose(ctx, prompt.Prompt.ID, "Uppercase")
	if err != nil {
		t.Fatal(err)
	}
	assertTreeFiles(t, service, proposal.Proposal.GitCommit, map[string]string{"shared.txt": "UPPER\n"})
}

func TestSameAttemptFollowupCoalescesAlreadyAcceptedEdit(t *testing.T) {
	ctx := context.Background()
	service, initial := newTestProject(t, map[string]string{
		"hop-home.css": "body { color: black; }\n",
		"footer.html":  `<link rel="stylesheet" href="/css/hop-home.css?v=2">` + "\n",
	})
	first, err := service.CreatePrompt(ctx, "Add the primary color", "", "agent")
	if err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(first.Workspace, "hop-home.css"), "body { color: black; }\n:root { --color-primary: #724bdb; }\n")
	firstProposal, err := service.Propose(ctx, first.Prompt.ID, "Add primary color")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Land(ctx, firstProposal.Proposal.ID, nil); err != nil {
		t.Fatal(err)
	}

	followup, err := service.CreatePrompt(ctx, "Bust the stylesheet cache", firstProposal.Proposal.ID, "agent")
	if err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(followup.Workspace, "footer.html"), `<link rel="stylesheet" href="/css/hop-home.css?v=3">`+"\n")
	secondProposal, err := service.Propose(ctx, followup.Prompt.ID, "Bump stylesheet cache key")
	if err != nil {
		t.Fatal(err)
	}
	if secondProposal.Proposal.CanonicalAnchorID != initial.ID {
		t.Fatalf("test no longer exercises stale attempt base: proposal anchor = %s, want %s", secondProposal.Proposal.CanonicalAnchorID, initial.ID)
	}
	landed, err := service.Land(ctx, secondProposal.Proposal.ID, nil)
	if err != nil {
		t.Fatalf("same-attempt follow-up was blocked: %v", err)
	}
	assertTreeFiles(t, service, landed.State.GitCommit, map[string]string{
		"hop-home.css": "body { color: black; }\n:root { --color-primary: #724bdb; }\n",
		"footer.html":  `<link rel="stylesheet" href="/css/hop-home.css?v=3">` + "\n",
	})
}

func TestIdenticalSameFileChangesCoalesceAutomatically(t *testing.T) {
	ctx := context.Background()
	service, _ := newTestProject(t, map[string]string{"shared.css": "body {}\n"})
	first, err := service.CreatePrompt(ctx, "Add primary color", "", "one")
	if err != nil {
		t.Fatal(err)
	}
	second, err := service.CreatePrompt(ctx, "Add the same primary color", "", "two")
	if err != nil {
		t.Fatal(err)
	}
	want := "body {}\n:root { --primary: purple; }\n"
	writeTestFile(t, filepath.Join(first.Workspace, "shared.css"), want)
	writeTestFile(t, filepath.Join(second.Workspace, "shared.css"), want)
	firstProposal, err := service.Propose(ctx, first.Prompt.ID, "Primary color")
	if err != nil {
		t.Fatal(err)
	}
	secondProposal, err := service.Propose(ctx, second.Prompt.ID, "Same primary color")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Accept(ctx, firstProposal.Proposal.ID, nil); err != nil {
		t.Fatal(err)
	}
	secondAccepted, err := service.Accept(ctx, secondProposal.Proposal.ID, nil)
	if err != nil {
		t.Fatalf("identical same-file change was blocked: %v", err)
	}
	assertTreeFiles(t, service, secondAccepted.State.GitCommit, map[string]string{"shared.css": want})
}

func TestConcurrentSameFileCompatibleAcceptancesSerializeAndMerge(t *testing.T) {
	ctx := context.Background()
	service, _ := newTestProject(t, map[string]string{"shared.txt": "one\ntwo\nthree\n"})
	first, err := service.CreatePrompt(ctx, "Uppercase one", "", "one")
	if err != nil {
		t.Fatal(err)
	}
	second, err := service.CreatePrompt(ctx, "Uppercase three", "", "two")
	if err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(first.Workspace, "shared.txt"), "ONE\ntwo\nthree\n")
	writeTestFile(t, filepath.Join(second.Workspace, "shared.txt"), "one\ntwo\nTHREE\n")
	firstProposal, err := service.Propose(ctx, first.Prompt.ID, "One")
	if err != nil {
		t.Fatal(err)
	}
	secondProposal, err := service.Propose(ctx, second.Prompt.ID, "Three")
	if err != nil {
		t.Fatal(err)
	}
	errs := make(chan error, 2)
	var wg sync.WaitGroup
	for _, proposalID := range []string{firstProposal.Proposal.ID, secondProposal.Proposal.ID} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, acceptErr := service.Accept(ctx, proposalID, nil)
			errs <- acceptErr
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent compatible acceptance: %v", err)
		}
	}
	head, err := service.Store.AcceptedHead(ctx)
	if err != nil {
		t.Fatal(err)
	}
	assertTreeFiles(t, service, head.GitCommit, map[string]string{
		"shared.txt": "ONE\ntwo\nTHREE\n",
	})
}

func TestTrueConflictCreatesAgentReconciliationWorkspace(t *testing.T) {
	ctx := context.Background()
	service, _ := newTestProject(t, map[string]string{"shared.txt": "color=base\n"})
	first, err := service.CreatePrompt(ctx, "Use red", "", "one")
	if err != nil {
		t.Fatal(err)
	}
	second, err := service.CreatePrompt(ctx, "Use blue with fallback", "", "two")
	if err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(first.Workspace, "shared.txt"), "color=red\n")
	writeTestFile(t, filepath.Join(second.Workspace, "shared.txt"), "color=blue\n")
	firstProposal, err := service.Propose(ctx, first.Prompt.ID, "Red")
	if err != nil {
		t.Fatal(err)
	}
	secondProposal, err := service.Propose(ctx, second.Prompt.ID, "Blue")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Land(ctx, firstProposal.Proposal.ID, nil); err != nil {
		t.Fatal(err)
	}
	_, err = service.Land(ctx, secondProposal.Proposal.ID, nil)
	var conflict *ConflictError
	if !errors.As(err, &conflict) {
		t.Fatalf("land error = %v, want ConflictError", err)
	}
	refresh, err := service.Refresh(ctx, secondProposal.Proposal.ID)
	if err != nil {
		t.Fatal(err)
	}
	if refresh.Task.ID != second.Task.ID {
		t.Fatalf("reconciliation escaped original task: %#v", refresh)
	}
	if refresh.Attempt.ID == second.Attempt.ID || refresh.Attempt.BaseStateID != refresh.AcceptedHead.ID {
		t.Fatalf("reconciliation did not receive a fresh attempt at the accepted head: %#v", refresh.Attempt)
	}
	if len(refresh.Conflicts) != 1 || refresh.Conflicts[0] != "shared.txt" {
		t.Fatalf("conflicts = %#v", refresh.Conflicts)
	}
	contents, err := os.ReadFile(filepath.Join(refresh.Workspace, "shared.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(contents, []byte("<<<<<<< ")) ||
		!bytes.Contains(contents, []byte("=======")) ||
		!bytes.Contains(contents, []byte(">>>>>>> ")) {
		t.Fatalf("reconciliation workspace lacks useful diff3 markers:\n%s", contents)
	}
	if _, err := service.Propose(ctx, refresh.Prompt.ID, "Unresolved"); err == nil || !strings.Contains(err.Error(), "merge markers") {
		t.Fatalf("unresolved proposal error = %v", err)
	}
	writeTestFile(t, filepath.Join(refresh.Workspace, "shared.txt"), "color=red-blue-fallback\n")
	if _, err := service.Propose(ctx, refresh.Prompt.ID, "Unchecked resolution"); err == nil || !strings.Contains(err.Error(), "must pass hop check") {
		t.Fatalf("unchecked reconciliation proposal error = %v", err)
	}
	if _, err := service.RunCheck(ctx, refresh.Prompt.ID, []string{
		"sh", "-c", `test "$(cat shared.txt)" = "color=red-blue-fallback"`,
	}); err != nil {
		t.Fatal(err)
	}
	resolvedProposal, err := service.Propose(ctx, refresh.Prompt.ID, "Resolve both color intents")
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := service.Land(ctx, resolvedProposal.Proposal.ID, []string{
		"sh", "-c", `test "$(cat shared.txt)" = "color=red-blue-fallback"`,
	})
	if err != nil {
		t.Fatal(err)
	}
	if contents, err := os.ReadFile(filepath.Join(service.Root, "shared.txt")); err != nil || string(contents) != "color=red-blue-fallback\n" {
		t.Fatalf("visible resolution = %q, err=%v", string(contents), err)
	}
	if resolved.MaterializedRoot != service.Root {
		t.Fatalf("resolved root = %q", resolved.MaterializedRoot)
	}
}

func TestMarkerlessModifyDeleteConflictRequiresCheckedResolution(t *testing.T) {
	ctx := context.Background()
	service, _ := newTestProject(t, map[string]string{"shared.txt": "base\n"})
	modify, err := service.CreatePrompt(ctx, "Modify the shared file", "", "modifier")
	if err != nil {
		t.Fatal(err)
	}
	remove, err := service.CreatePrompt(ctx, "Remove the shared file", "", "remover")
	if err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(modify.Workspace, "shared.txt"), "accepted modification\n")
	if err := os.Remove(filepath.Join(remove.Workspace, "shared.txt")); err != nil {
		t.Fatal(err)
	}
	modifyProposal, err := service.Propose(ctx, modify.Prompt.ID, "Modify")
	if err != nil {
		t.Fatal(err)
	}
	removeProposal, err := service.Propose(ctx, remove.Prompt.ID, "Remove")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Land(ctx, modifyProposal.Proposal.ID, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Land(ctx, removeProposal.Proposal.ID, nil); err == nil {
		t.Fatal("modify/delete proposal unexpectedly landed without reconciliation")
	}
	refresh, err := service.Refresh(ctx, removeProposal.Proposal.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Propose(ctx, refresh.Prompt.ID, "Unchecked structural resolution"); err == nil || !strings.Contains(err.Error(), "must pass hop check") {
		t.Fatalf("unchecked markerless reconciliation error = %v", err)
	}
	if err := os.Remove(filepath.Join(refresh.Workspace, "shared.txt")); err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	if _, err := service.RunCheck(ctx, refresh.Prompt.ID, []string{"sh", "-c", "test ! -e shared.txt"}); err != nil {
		t.Fatal(err)
	}
	resolvedProposal, err := service.Propose(ctx, refresh.Prompt.ID, "Intentionally remove shared file")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Land(ctx, resolvedProposal.Proposal.ID, []string{"sh", "-c", "test ! -e shared.txt"}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(service.Root, "shared.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("resolved visible root retained deleted file: %v", err)
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

func TestLandMaterializesVisibleRootAndSafelyFastForwardsGitState(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	runGitTest(t, root, "init", "--quiet")
	writeTestFile(t, filepath.Join(root, "base.txt"), "base\n")
	writeTestFile(t, filepath.Join(root, "remove.txt"), "remove\n")
	runGitTest(t, root, "add", "base.txt", "remove.txt")
	runGitTest(t, root, "-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "--quiet", "-m", "initial")

	service, _, err := InitProject(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = service.Close() })
	started, err := service.CreatePrompt(ctx, "Materialize the result", "", "agent")
	if err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(started.Workspace, "base.txt"), "landed\n")
	writeTestFile(t, filepath.Join(started.Workspace, "nested", "new.txt"), "new\n")
	if err := os.Remove(filepath.Join(started.Workspace, "remove.txt")); err != nil {
		t.Fatal(err)
	}
	proposal, err := service.Propose(ctx, started.Prompt.ID, "Materialized change")
	if err != nil {
		t.Fatal(err)
	}

	beforeHead := runGitTest(t, root, "rev-parse", "HEAD")
	beforeBranch := runGitTest(t, root, "symbolic-ref", "--short", "HEAD")
	result, err := service.Land(ctx, proposal.Proposal.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.MaterializedRoot != service.Root {
		t.Fatalf("materialized root = %q, want %q", result.MaterializedRoot, service.Root)
	}
	if got := runGitTest(t, root, "rev-parse", "HEAD"); got != result.State.GitCommit {
		t.Fatalf("HEAD = %s, want accepted commit %s (previously %s)", got, result.State.GitCommit, beforeHead)
	}
	if got := runGitTest(t, root, "symbolic-ref", "--short", "HEAD"); got != beforeBranch {
		t.Fatalf("branch changed from %s to %s", beforeBranch, got)
	}
	if got := runGitTest(t, root, "write-tree"); got != result.State.SourceTree {
		t.Fatalf("real index tree = %s, want accepted tree %s", got, result.State.SourceTree)
	}
	if got := runGitTest(t, root, "status", "--porcelain=v1"); got != "" {
		t.Fatalf("raw Git status after safe landing = %q", got)
	}
	if result.GitSync == nil || result.GitSync.Status != "synchronized" {
		t.Fatalf("Git synchronization result = %#v", result.GitSync)
	}
	if contents, err := os.ReadFile(filepath.Join(root, "base.txt")); err != nil || string(contents) != "landed\n" {
		t.Fatalf("base.txt = %q, err=%v", string(contents), err)
	}
	if contents, err := os.ReadFile(filepath.Join(root, "nested", "new.txt")); err != nil || string(contents) != "new\n" {
		t.Fatalf("nested/new.txt = %q, err=%v", string(contents), err)
	}
	if _, err := os.Stat(filepath.Join(root, "remove.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("removed file still exists: %v", err)
	}
	status, err := service.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if status.RootStatus != "synchronized" || status.RootStateID != result.State.ID {
		t.Fatalf("root status = %#v", status)
	}
}

func TestLandAdoptsCleanExternalGitCommitAndLandsProposalOnce(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	runGitTest(t, root, "init", "--quiet")
	writeTestFile(t, filepath.Join(root, "base.txt"), "base\n")
	runGitTest(t, root, "add", "base.txt")
	runGitTest(t, root, "-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "--quiet", "-m", "initial")

	service, _, err := InitProject(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = service.Close() })
	remote := filepath.Join(t.TempDir(), "remote.git")
	runGitTest(t, root, "init", "--quiet", "--bare", remote)
	runGitTest(t, root, "remote", "add", "origin", remote)
	branch := runGitTest(t, root, "symbolic-ref", "--short", "HEAD")
	runGitTest(t, root, "config", "branch."+branch+".remote", "origin")
	runGitTest(t, root, "config", "branch."+branch+".merge", "refs/heads/"+branch)
	seed, err := service.CreatePrompt(ctx, "Establish a Hop baseline", "", "agent")
	if err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(seed.Workspace, "baseline.txt"), "baseline\n")
	seedProposal, err := service.Propose(ctx, seed.Prompt.ID, "Establish Hop baseline")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Land(ctx, seedProposal.Proposal.ID, nil); err != nil {
		t.Fatal(err)
	}
	started, err := service.CreatePrompt(ctx, "Add proposal work", "", "agent")
	if err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(started.Workspace, "proposal.txt"), "proposal\n")
	proposal, err := service.Propose(ctx, started.Prompt.ID, "Add proposal work")
	if err != nil {
		t.Fatal(err)
	}

	// Reproduce the real multi-agent failure: another agent makes a normal,
	// clean Git commit after this proposal was frozen but before it lands.
	writeTestFile(t, filepath.Join(root, "external.txt"), "external\n")
	runGitTest(t, root, "add", "external.txt")
	runGitTest(t, root, "-c", "user.name=Other", "-c", "user.email=other@example.com", "commit", "--quiet", "-m", "external agent commit")
	externalCommit := runGitTest(t, root, "rev-parse", "HEAD")
	runGitTest(t, root, "push", "--quiet", "origin", branch)
	beforeBytes, err := os.ReadFile(filepath.Join(root, "external.txt"))
	if err != nil {
		t.Fatal(err)
	}

	landed, err := service.Land(ctx, proposal.Proposal.ID, nil)
	if err != nil {
		t.Fatalf("single land after clean external commit: %v", err)
	}
	if landed.CapturedRoot == nil || landed.CapturedRoot.Summary != "Adopt clean Git branch advancement" {
		t.Fatalf("adopted baseline = %#v", landed.CapturedRoot)
	}
	if landed.CapturedRoot.GitCommit != externalCommit {
		t.Fatalf("adopted commit = %s, want external commit %s", landed.CapturedRoot.GitCommit, externalCommit)
	}
	if !slices.Equal(landed.CapturedRootPaths, []string{"external.txt"}) {
		t.Fatalf("adopted paths = %v", landed.CapturedRootPaths)
	}
	afterBytes, err := os.ReadFile(filepath.Join(root, "external.txt"))
	if err != nil || !slices.Equal(beforeBytes, afterBytes) {
		t.Fatalf("external file bytes changed: before=%q after=%q err=%v", beforeBytes, afterBytes, err)
	}
	if contents, err := os.ReadFile(filepath.Join(root, "proposal.txt")); err != nil || string(contents) != "proposal\n" {
		t.Fatalf("proposal file = %q, err=%v", string(contents), err)
	}
	if got := runGitTest(t, root, "status", "--porcelain=v1"); got != "" {
		t.Fatalf("raw Git status after one-pass land = %q", got)
	}
	if got := runGitTest(t, root, "rev-parse", "HEAD"); got != landed.State.GitCommit {
		t.Fatalf("HEAD = %s, want final accepted commit %s", got, landed.State.GitCommit)
	}
	if got := runGitTest(t, root, "merge-base", "--is-ancestor", externalCommit, landed.State.GitCommit); got != "" {
		t.Fatalf("external commit is not an ancestor of final accepted commit")
	}
}

func TestLandAutomaticallyPushesAcceptedCommit(t *testing.T) {
	ctx := context.Background()
	service, initial := newTestProject(t, map[string]string{"base.txt": "base\n"})
	remote := filepath.Join(t.TempDir(), "remote.git")
	runGitTest(t, service.Root, "init", "--quiet", "--bare", remote)
	runGitTest(t, service.Root, "remote", "add", "origin", remote)
	branch := runGitTest(t, service.Root, "symbolic-ref", "--short", "HEAD")
	runGitTest(t, service.Root, "config", "branch."+branch+".remote", "origin")
	runGitTest(t, service.Root, "config", "branch."+branch+".merge", "refs/heads/"+branch)

	started, err := service.CreatePrompt(ctx, "Publish accepted work", "", "agent")
	if err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(started.Workspace, "published.txt"), "published\n")
	proposal, err := service.Propose(ctx, started.Prompt.ID, "Published change")
	if err != nil {
		t.Fatal(err)
	}
	result, err := service.Land(ctx, proposal.Proposal.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.RemotePush == nil {
		t.Fatal("land did not report an automatic remote push")
	}
	wantRef := "refs/heads/" + branch
	if result.RemotePush.Remote != "origin" || result.RemotePush.Ref != wantRef || result.RemotePush.Commit != result.State.GitCommit {
		t.Fatalf("automatic push = %#v", result.RemotePush)
	}
	if got := runGitTest(t, service.Root, "--git-dir", remote, "rev-parse", wantRef); got != result.State.GitCommit {
		t.Fatalf("remote branch = %s, want accepted commit %s", got, result.State.GitCommit)
	}
	runGitTest(t, service.Root, "update-ref", "refs/remotes/origin/"+branch, initial.GitCommit)
	status, err := service.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if status.Publication.Status != "current" || status.Publication.RemoteTip != result.State.GitCommit || status.Git.UpstreamObservation != "last_authoritative_remote_check" || !status.Git.LocalTrackingRefMayBeStale {
		t.Fatalf("publication status = %#v", status)
	}
	retried, err := service.Push(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if retried.Commit != result.State.GitCommit || retried.Remote != "origin" || retried.Ref != wantRef {
		t.Fatalf("explicit push retry = %#v", retried)
	}
}

func TestStatusExplainsProjectionOverStaleBranchWithoutCallingItUserWork(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "base.txt"), "base\n")
	runGitTest(t, root, "init", "--quiet")
	runGitTest(t, root, "add", "base.txt")
	runGitTest(t, root, "-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "--quiet", "-m", "base")
	service, _, err := InitProject(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = service.Close() })
	started, err := service.CreatePrompt(ctx, "Add projected work", "", "agent")
	if err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(started.Workspace, "projected.txt"), "projected\n")
	proposal, err := service.Propose(ctx, started.Prompt.ID, "Add projected work")
	if err != nil {
		t.Fatal(err)
	}
	staleHead := runGitTest(t, service.Root, "rev-parse", "HEAD")
	branchRef := runGitTest(t, service.Root, "symbolic-ref", "HEAD")
	landed, err := service.Land(ctx, proposal.Proposal.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	runGitTest(t, service.Root, "update-ref", branchRef, staleHead, landed.State.GitCommit)
	runGitTest(t, service.Root, "read-tree", staleHead)
	beforeHead := runGitTest(t, service.Root, "rev-parse", "HEAD")
	beforeIndex := runGitTest(t, service.Root, "ls-files", "--stage")
	beforeRefs := runGitTest(t, service.Root, "show-ref")
	status, err := service.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if status.RootStatus != "synchronized" || !status.Git.ProjectionOverStaleRef || !status.Git.ProjectionOnlyChanges {
		t.Fatalf("projection status = %#v", status)
	}
	if status.Git.LocalTip != beforeHead || status.Git.AcceptedTip != landed.State.GitCommit || status.Git.UserIndexChanged || status.Git.UserWorktreeChanged {
		t.Fatalf("Git status misclassified projection = %#v", status.Git)
	}
	if status.Publication.Status != "not_configured" {
		t.Fatalf("publication = %#v, want not_configured", status.Publication)
	}
	doctor, err := service.Doctor(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	if !doctor.OK || len(doctor.Warnings) == 0 {
		t.Fatalf("doctor did not explain projection: %#v", doctor)
	}
	if got := runGitTest(t, service.Root, "rev-parse", "HEAD"); got != beforeHead {
		t.Fatalf("status moved HEAD from %s to %s", beforeHead, got)
	}
	if got := runGitTest(t, service.Root, "ls-files", "--stage"); got != beforeIndex {
		t.Fatalf("status changed index:\nwant %s\n got %s", beforeIndex, got)
	}
	if got := runGitTest(t, service.Root, "show-ref"); got != beforeRefs {
		t.Fatalf("status changed refs:\nwant %s\n got %s", beforeRefs, got)
	}
}

func TestPublicationFailureIsDurableAndRetryClearsIt(t *testing.T) {
	ctx := context.Background()
	service, initial := newTestProject(t, map[string]string{"base.txt": "base\n"})
	missing := filepath.Join(t.TempDir(), "missing.git")
	runGitTest(t, service.Root, "remote", "add", "origin", missing)
	started, err := service.CreatePrompt(ctx, "Accept while publishing is offline", "", "agent")
	if err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(started.Workspace, "local.txt"), "local\n")
	proposal, err := service.Propose(ctx, started.Prompt.ID, "Keep local acceptance")
	if err != nil {
		t.Fatal(err)
	}
	landed, err := service.Land(ctx, proposal.Proposal.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	if landed.State.ID == initial.ID || len(landed.Warnings) == 0 {
		t.Fatalf("acceptance did not survive push failure: %#v", landed)
	}
	status, err := service.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if status.Publication.Status != "failed" || !status.Publication.Retryable || status.Publication.ErrorCategory == "" || status.Publication.ErrorMessage == "" {
		t.Fatalf("durable failed publication = %#v", status.Publication)
	}
	remote := filepath.Join(t.TempDir(), "remote.git")
	runGitTest(t, service.Root, "init", "--quiet", "--bare", remote)
	runGitTest(t, service.Root, "remote", "set-url", "origin", remote)
	if _, err := service.Push(ctx); err != nil {
		t.Fatal(err)
	}
	status, err = service.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if status.Publication.Status != "current" || status.Publication.ErrorCategory != "" || status.Publication.Retryable {
		t.Fatalf("retry did not clear publication warning: %#v", status.Publication)
	}
	branch := runGitTest(t, service.Root, "symbolic-ref", "--short", "HEAD")
	if got := runGitTest(t, service.Root, "--git-dir", remote, "rev-parse", "refs/heads/"+branch); got != landed.State.GitCommit {
		t.Fatalf("remote = %s, want %s", got, landed.State.GitCommit)
	}
}

func TestPushRejectsDivergedRemoteWithoutForceAndRecordsIt(t *testing.T) {
	ctx := context.Background()
	service, _ := newTestProject(t, map[string]string{"base.txt": "base\n"})
	missing := filepath.Join(t.TempDir(), "missing.git")
	runGitTest(t, service.Root, "remote", "add", "origin", missing)
	started, err := service.CreatePrompt(ctx, "Create local accepted work", "", "agent")
	if err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(started.Workspace, "local.txt"), "local\n")
	proposal, err := service.Propose(ctx, started.Prompt.ID, "Local accepted work")
	if err != nil {
		t.Fatal(err)
	}
	landed, err := service.Land(ctx, proposal.Proposal.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	remote := filepath.Join(t.TempDir(), "remote.git")
	runGitTest(t, service.Root, "init", "--quiet", "--bare", remote)
	other := filepath.Join(t.TempDir(), "other")
	runGitTest(t, service.Root, "init", "--quiet", other)
	writeTestFile(t, filepath.Join(other, "remote.txt"), "remote\n")
	runGitTest(t, other, "add", "remote.txt")
	runGitTest(t, other, "-c", "user.name=Remote", "-c", "user.email=remote@example.com", "commit", "--quiet", "-m", "unrelated remote")
	branch := runGitTest(t, service.Root, "symbolic-ref", "--short", "HEAD")
	runGitTest(t, other, "remote", "add", "origin", remote)
	runGitTest(t, other, "push", "--quiet", "origin", "HEAD:refs/heads/"+branch)
	remoteBefore := runGitTest(t, service.Root, "--git-dir", remote, "rev-parse", "refs/heads/"+branch)
	runGitTest(t, service.Root, "remote", "set-url", "origin", remote)
	_, err = service.Push(ctx)
	var diverged *RemoteDivergedError
	if !errors.As(err, &diverged) && (err == nil || !strings.Contains(err.Error(), "diverged")) {
		t.Fatalf("push error = %v, want divergence", err)
	}
	if got := runGitTest(t, service.Root, "--git-dir", remote, "rev-parse", "refs/heads/"+branch); got != remoteBefore {
		t.Fatalf("diverged remote moved from %s to %s", remoteBefore, got)
	}
	status, err := service.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if status.AcceptedHead.ID != landed.State.ID || status.Publication.Status != "failed" || status.Publication.ErrorCategory != "diverged" || status.Publication.Retryable {
		t.Fatalf("diverged publication status = %#v", status)
	}
}

func TestStatusDistinguishesRealIndexWorktreeAndIgnoredFiles(t *testing.T) {
	ctx := context.Background()
	service, _ := newTestProject(t, map[string]string{".gitignore": "ignored.tmp\n", "base.txt": "base\n"})
	started, err := service.CreatePrompt(ctx, "Create projection", "", "agent")
	if err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(started.Workspace, "projected.txt"), "projected\n")
	proposal, err := service.Propose(ctx, started.Prompt.ID, "Create projection")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Land(ctx, proposal.Proposal.ID, nil); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(service.Root, "base.txt"), "user worktree\n")
	writeTestFile(t, filepath.Join(service.Root, "staged.txt"), "user index\n")
	writeTestFile(t, filepath.Join(service.Root, "ignored.tmp"), "ignored\n")
	runGitTest(t, service.Root, "add", "staged.txt")
	status, err := service.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !status.Git.UserWorktreeChanged || !slices.Contains(status.Git.UserWorktreePaths, "base.txt") {
		t.Fatalf("worktree changes = %#v", status.Git)
	}
	if !status.Git.UserIndexChanged || !slices.Contains(status.Git.UserIndexPaths, "staged.txt") {
		t.Fatalf("index changes = %#v", status.Git)
	}
	if slices.Contains(status.Git.UserWorktreePaths, "ignored.tmp") || slices.Contains(status.Git.UserIndexPaths, "ignored.tmp") {
		t.Fatalf("ignored file was misclassified as source work: %#v", status.Git)
	}
	if contents, err := os.ReadFile(filepath.Join(service.Root, "ignored.tmp")); err != nil || string(contents) != "ignored\n" {
		t.Fatalf("status changed ignored file: %q, %v", contents, err)
	}
}

func TestPushTagPublishesAnnotatedTag(t *testing.T) {
	ctx := context.Background()
	service, _ := newTestProject(t, map[string]string{"base.txt": "base\n"})
	remote := filepath.Join(t.TempDir(), "remote.git")
	runGitTest(t, service.Root, "init", "--quiet", "--bare", remote)
	runGitTest(t, service.Root, "remote", "add", "origin", remote)
	accepted, err := service.Store.AcceptedHead(ctx)
	if err != nil {
		t.Fatal(err)
	}
	runGitTest(t, service.Root, "tag", "-a", "v1.0.0-test", "-m", "test release", accepted.GitCommit)

	result, err := service.PushTag(ctx, "v1.0.0-test")
	if err != nil {
		t.Fatal(err)
	}
	if result.Remote != "origin" || result.Ref != "refs/tags/v1.0.0-test" {
		t.Fatalf("tag push = %#v", result)
	}
	if got := runGitTest(t, service.Root, "--git-dir", remote, "cat-file", "-t", result.Ref); got != "tag" {
		t.Fatalf("remote tag type = %s, want tag", got)
	}
}

func TestAutomaticPushReconcilesCompatibleDivergedRemote(t *testing.T) {
	ctx := context.Background()
	service, _ := newTestProject(t, map[string]string{"base.txt": "base\n"})
	remote := filepath.Join(t.TempDir(), "remote.git")
	runGitTest(t, service.Root, "init", "--quiet", "--bare", remote)
	runGitTest(t, service.Root, "remote", "add", "origin", remote)
	branch := runGitTest(t, service.Root, "symbolic-ref", "--short", "HEAD")

	first, err := service.CreatePrompt(ctx, "Publish first work", "", "agent")
	if err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(first.Workspace, "first.txt"), "first\n")
	firstProposal, err := service.Propose(ctx, first.Prompt.ID, "First published change")
	if err != nil {
		t.Fatal(err)
	}
	firstLanded, err := service.Land(ctx, firstProposal.Proposal.ID, nil)
	if err != nil {
		t.Fatal(err)
	}

	other := filepath.Join(t.TempDir(), "other")
	runGitTest(t, service.Root, "clone", "--quiet", "--branch", branch, remote, other)
	writeTestFile(t, filepath.Join(other, "remote.txt"), "remote\n")
	runGitTest(t, other, "add", "remote.txt")
	runGitTest(t, other, "-c", "user.name=Remote", "-c", "user.email=remote@example.com", "commit", "--quiet", "-m", "remote change")
	runGitTest(t, other, "push", "--quiet", "origin", branch)
	remoteTip := runGitTest(t, service.Root, "--git-dir", remote, "rev-parse", "refs/heads/"+branch)
	if remoteTip == firstLanded.State.GitCommit {
		t.Fatal("test did not advance the remote independently")
	}

	second, err := service.CreatePrompt(ctx, "Publish local work", "", "agent")
	if err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(second.Workspace, "local.txt"), "local\n")
	secondProposal, err := service.Propose(ctx, second.Prompt.ID, "Local accepted change")
	if err != nil {
		t.Fatal(err)
	}
	secondLanded, err := service.Land(ctx, secondProposal.Proposal.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	if secondLanded.RemotePush == nil {
		t.Fatal("compatible remote advancement was not reconciled and pushed")
	}
	if got := runGitTest(t, service.Root, "--git-dir", remote, "rev-parse", "refs/heads/"+branch); got != secondLanded.State.GitCommit {
		t.Fatalf("remote branch = %s, want reconciled accepted commit %s", got, secondLanded.State.GitCommit)
	}
	assertTreeFiles(t, service, secondLanded.State.GitCommit, map[string]string{
		"base.txt":   "base\n",
		"first.txt":  "first\n",
		"local.txt":  "local\n",
		"remote.txt": "remote\n",
	})
	parents := strings.Fields(runGitTest(t, service.Root, "show", "-s", "--format=%P", secondLanded.State.GitCommit))
	if len(parents) != 2 || parents[0] != remoteTip || parents[1] != firstLanded.State.GitCommit {
		t.Fatalf("reconciled parents = %v, want [%s %s]", parents, remoteTip, firstLanded.State.GitCommit)
	}
}

func TestRemotePushConflictReconcilesThroughAgentWorkspace(t *testing.T) {
	ctx := context.Background()
	service, _ := newTestProject(t, map[string]string{"shared.txt": "base\n"})
	remote := filepath.Join(t.TempDir(), "remote.git")
	runGitTest(t, service.Root, "init", "--quiet", "--bare", remote)
	runGitTest(t, service.Root, "remote", "add", "origin", remote)
	branch := runGitTest(t, service.Root, "symbolic-ref", "--short", "HEAD")

	seed, err := service.CreatePrompt(ctx, "Publish the initial branch", "", "agent")
	if err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(seed.Workspace, "seed.txt"), "seed\n")
	seedProposal, err := service.Propose(ctx, seed.Prompt.ID, "Publish the initial branch")
	if err != nil {
		t.Fatal(err)
	}
	seedLanded, err := service.Land(ctx, seedProposal.Proposal.ID, nil)
	if err != nil {
		t.Fatal(err)
	}

	other := filepath.Join(t.TempDir(), "other")
	runGitTest(t, service.Root, "clone", "--quiet", "--branch", branch, remote, other)
	writeTestFile(t, filepath.Join(other, "shared.txt"), "remote\n")
	runGitTest(t, other, "add", "shared.txt")
	runGitTest(t, other, "-c", "user.name=Remote", "-c", "user.email=remote@example.com", "commit", "--quiet", "-m", "remote change")
	runGitTest(t, other, "push", "--quiet", "origin", branch)
	remoteTip := runGitTest(t, service.Root, "--git-dir", remote, "rev-parse", "refs/heads/"+branch)

	local, err := service.CreatePrompt(ctx, "Change the shared value locally", "", "agent")
	if err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(local.Workspace, "shared.txt"), "local\n")
	localProposal, err := service.Propose(ctx, local.Prompt.ID, "Change the shared value locally")
	if err != nil {
		t.Fatal(err)
	}
	_, err = service.Land(ctx, localProposal.Proposal.ID, nil)
	var conflict *ConflictError
	if !errors.As(err, &conflict) {
		t.Fatalf("land error = %v, want ConflictError", err)
	}
	if conflict.RemoteTip != remoteTip || len(conflict.Paths) != 1 || conflict.Paths[0] != "shared.txt" {
		t.Fatalf("remote conflict = %#v, want tip %s and shared.txt", conflict, remoteTip)
	}
	if head, headErr := service.Store.AcceptedHead(ctx); headErr != nil || head.ID != seedLanded.State.ID {
		t.Fatalf("blocked land advanced accepted head to %s, err=%v", head.ID, headErr)
	}

	refresh, err := service.Refresh(ctx, localProposal.Proposal.ID)
	if err != nil {
		t.Fatal(err)
	}
	if refresh.RemoteTip != remoteTip {
		t.Fatalf("reconciliation remote tip = %s, want %s", refresh.RemoteTip, remoteTip)
	}
	if len(refresh.Conflicts) != 1 || refresh.Conflicts[0] != "shared.txt" {
		t.Fatalf("reconciliation conflicts = %#v", refresh.Conflicts)
	}
	contents, err := os.ReadFile(filepath.Join(refresh.Workspace, "shared.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(contents, []byte("<<<<<<< ")) || !bytes.Contains(contents, []byte(">>>>>>> ")) {
		t.Fatalf("remote reconciliation workspace lacks conflict markers:\n%s", contents)
	}

	// The remote can advance again while the agent resolves the original
	// conflict. Landing must merge that newer compatible work rather than
	// assuming the recorded remote tip is still current.
	writeTestFile(t, filepath.Join(other, "after.txt"), "after\n")
	runGitTest(t, other, "add", "after.txt")
	runGitTest(t, other, "-c", "user.name=Remote", "-c", "user.email=remote@example.com", "commit", "--quiet", "-m", "later remote change")
	runGitTest(t, other, "push", "--quiet", "origin", branch)
	advancedRemoteTip := runGitTest(t, service.Root, "--git-dir", remote, "rev-parse", "refs/heads/"+branch)

	writeTestFile(t, filepath.Join(refresh.Workspace, "shared.txt"), "remote + local\n")
	if _, err := service.RunCheck(ctx, refresh.Prompt.ID, []string{
		"sh", "-c", `test "$(cat shared.txt)" = "remote + local"`,
	}); err != nil {
		t.Fatal(err)
	}
	resolvedProposal, err := service.Propose(ctx, refresh.Prompt.ID, "Reconcile remote and local shared values")
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := service.Land(ctx, resolvedProposal.Proposal.ID, []string{
		"sh", "-c", `test "$(cat shared.txt)" = "remote + local"`,
	})
	if err != nil {
		t.Fatal(err)
	}
	if resolved.RemotePush == nil {
		t.Fatal("resolved remote conflict was not pushed")
	}
	if got := runGitTest(t, service.Root, "--git-dir", remote, "rev-parse", "refs/heads/"+branch); got != resolved.State.GitCommit {
		t.Fatalf("remote branch = %s, want reconciled accepted commit %s", got, resolved.State.GitCommit)
	}
	if contents, err := os.ReadFile(filepath.Join(service.Root, "shared.txt")); err != nil || string(contents) != "remote + local\n" {
		t.Fatalf("visible reconciled value = %q, err=%v", string(contents), err)
	}
	if contents, err := os.ReadFile(filepath.Join(service.Root, "after.txt")); err != nil || string(contents) != "after\n" {
		t.Fatalf("later remote value = %q, err=%v", string(contents), err)
	}
	parents := strings.Fields(runGitTest(t, service.Root, "show", "-s", "--format=%P", resolved.State.GitCommit))
	if len(parents) != 2 || parents[0] != advancedRemoteTip || parents[1] != seedLanded.State.GitCommit {
		t.Fatalf("reconciled parents = %v, want [%s %s]", parents, advancedRemoteTip, seedLanded.State.GitCommit)
	}
}

func TestReconciliationMetadataDecodesLegacyConflictArray(t *testing.T) {
	metadata, ok := decodeReconciliationMetadata(reconciliationSummaryPrefix + `["shared.txt"]`)
	if !ok || len(metadata.Conflicts) != 1 || metadata.Conflicts[0] != "shared.txt" || metadata.RemoteTip != "" {
		t.Fatalf("legacy reconciliation metadata = %#v, ok=%v", metadata, ok)
	}
}

func TestLandMaterializesEmptyUnbornRootAcrossRepeatedLands(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	service, _, err := InitProject(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = service.Close() })
	for index, name := range []string{"one.txt", "two.txt"} {
		started, err := service.CreatePrompt(ctx, "Add "+name, "", "agent")
		if err != nil {
			t.Fatal(err)
		}
		writeTestFile(t, filepath.Join(started.Workspace, name), name+"\n")
		proposal, err := service.Propose(ctx, started.Prompt.ID, "Add "+name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := service.Land(ctx, proposal.Proposal.ID, nil); err != nil {
			t.Fatalf("land %d: %v", index+1, err)
		}
	}
	for _, name := range []string{"one.txt", "two.txt"} {
		contents, err := os.ReadFile(filepath.Join(service.Root, name))
		if err != nil || string(contents) != name+"\n" {
			t.Fatalf("%s = %q, err=%v", name, string(contents), err)
		}
	}
	if _, exists, err := service.Repo.Head(ctx); err != nil || exists {
		t.Fatalf("unborn HEAD changed: exists=%v err=%v", exists, err)
	}
	if _, err := os.Stat(filepath.Join(service.Repo.GitDir(), "index")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("real index was created or changed: %v", err)
	}
}

func TestAcceptLeavesVisibleRootStaleUntilSync(t *testing.T) {
	ctx := context.Background()
	service, initial := newTestProject(t, map[string]string{"base.txt": "base\n"})
	started, err := service.CreatePrompt(ctx, "Add accepted file", "", "controller")
	if err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(started.Workspace, "accepted.txt"), "accepted\n")
	proposal, err := service.Propose(ctx, started.Prompt.ID, "Controller accept")
	if err != nil {
		t.Fatal(err)
	}
	accepted, err := service.Accept(ctx, proposal.Proposal.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	if accepted.MaterializedRoot != "" {
		t.Fatal("controller accept unexpectedly materialized the visible root")
	}
	materialized, err := service.Store.MaterializedHead(ctx)
	if err != nil || materialized.ID != initial.ID {
		t.Fatalf("controller accept moved materialized head to %s: %v", materialized.ID, err)
	}
	if _, err := os.Stat(filepath.Join(service.Root, "accepted.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("controller accept changed visible root: %v", err)
	}
	status, err := service.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if status.RootStatus != "stale" {
		t.Fatalf("root status = %q, want stale", status.RootStatus)
	}
	synced, err := service.Sync(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !synced.Changed || synced.State.ID != accepted.State.ID {
		t.Fatalf("sync result = %#v", synced)
	}
	materialized, err = service.Store.MaterializedHead(ctx)
	if err != nil || materialized.ID != accepted.State.ID {
		t.Fatalf("sync materialized head = %s, err=%v", materialized.ID, err)
	}
	if contents, err := os.ReadFile(filepath.Join(service.Root, "accepted.txt")); err != nil || string(contents) != "accepted\n" {
		t.Fatalf("accepted.txt = %q, err=%v", string(contents), err)
	}
	retried, err := service.Land(ctx, proposal.Proposal.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	if retried.State.ID != accepted.State.ID {
		t.Fatalf("retry created accepted state %s, want existing %s", retried.State.ID, accepted.State.ID)
	}
	history, err := service.History(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(history) != 2 {
		t.Fatalf("retry created duplicate acceptance; history has %d states", len(history))
	}
}

func TestLandCapturesDivergedVisibleRootAndMergesProposal(t *testing.T) {
	ctx := context.Background()
	service, _ := newTestProject(t, map[string]string{"base.txt": "base\n"})
	started, err := service.CreatePrompt(ctx, "Add landed file", "", "agent")
	if err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(started.Workspace, "landed.txt"), "landed\n")
	proposal, err := service.Propose(ctx, started.Prompt.ID, "Landed file")
	if err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(service.Root, "local.txt"), "do not overwrite\n")
	landed, err := service.Land(ctx, proposal.Proposal.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	if landed.CapturedRoot == nil || landed.CapturedRoot.Summary != "Capture out-of-band visible project changes" {
		t.Fatalf("captured root = %#v", landed.CapturedRoot)
	}
	if !slices.Equal(landed.CapturedRootPaths, []string{"local.txt"}) {
		t.Fatalf("captured root paths = %v", landed.CapturedRootPaths)
	}
	if contents, err := os.ReadFile(filepath.Join(service.Root, "local.txt")); err != nil || string(contents) != "do not overwrite\n" {
		t.Fatalf("local file changed: %q, %v", string(contents), err)
	}
	if contents, err := os.ReadFile(filepath.Join(service.Root, "landed.txt")); err != nil || string(contents) != "landed\n" {
		t.Fatalf("landed file = %q, %v", string(contents), err)
	}
	status, err := service.Status(ctx)
	if err != nil || status.RootStatus != "synchronized" || status.AcceptedHead.ID != landed.State.ID {
		t.Fatalf("status = %#v, err=%v", status, err)
	}
}

func TestLandRejectsUnprovenVisibleRootOverStaleBranch(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	runGitTest(t, root, "init", "--quiet")
	writeTestFile(t, filepath.Join(root, "base.txt"), "base\n")
	writeTestFile(t, filepath.Join(root, "keep.txt"), "keep\n")
	runGitTest(t, root, "add", "base.txt", "keep.txt")
	runGitTest(t, root, "-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "--quiet", "-m", "stale branch B")
	service, initial, err := InitProject(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = service.Close() })

	seed, err := service.CreatePrompt(ctx, "Advance the accepted tree", "", "agent")
	if err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(seed.Workspace, "base.txt"), "accepted\n")
	writeTestFile(t, filepath.Join(seed.Workspace, "accepted-only.txt"), "must survive\n")
	seedProposal, err := service.Propose(ctx, seed.Prompt.ID, "Advance accepted tree")
	if err != nil {
		t.Fatal(err)
	}
	accepted, err := service.Land(ctx, seedProposal.Proposal.ID, nil)
	if err != nil {
		t.Fatal(err)
	}

	work, err := service.CreatePrompt(ctx, "Add the requested feature", "", "agent")
	if err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(work.Workspace, "feature.txt"), "feature\n")
	proposal, err := service.Propose(ctx, work.Prompt.ID, "Add feature")
	if err != nil {
		t.Fatal(err)
	}

	// Reproduce the 1.0.10 incident: the durable materialized marker still says
	// the root is accepted A, while an external stale-branch projection replaces
	// the visible files with B. Two genuine visible edits are then made on B.
	branchRef := runGitTest(t, service.Root, "symbolic-ref", "HEAD")
	runGitTest(t, service.Root, "update-ref", branchRef, initial.GitCommit, accepted.State.GitCommit)
	runGitTest(t, service.Root, "read-tree", initial.SourceTree)
	if err := service.Repo.MaterializeTree(ctx, accepted.State.SourceTree, initial.SourceTree); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(service.Root, "base.txt"), "visible edit one\n")
	writeTestFile(t, filepath.Join(service.Root, "visible-two.txt"), "visible edit two\n")

	_, err = service.Land(ctx, proposal.Proposal.ID, nil)
	var conflict *RootConflictError
	if !errors.As(err, &conflict) {
		t.Fatalf("land error = %v, want RootConflictError", err)
	}
	head, headErr := service.Store.AcceptedHead(ctx)
	if headErr != nil {
		t.Fatal(headErr)
	}
	if head.ID != accepted.State.ID {
		t.Fatalf("unproven root advanced accepted head to %s, want %s", head.ID, accepted.State.ID)
	}
	if _, statErr := os.Stat(filepath.Join(service.Root, "accepted-only.txt")); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("test setup no longer reproduces stale B projection: %v", statErr)
	}
}

func TestAcceptanceVerifierRejectsUnauthorizedDeletion(t *testing.T) {
	ctx := context.Background()
	service, base := newTestProject(t, map[string]string{"keep.txt": "keep\n"})
	started, err := service.CreatePrompt(ctx, "Add feature", "", "agent")
	if err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(started.Workspace, "feature.txt"), "feature\n")
	proposal, err := service.Propose(ctx, started.Prompt.ID, "Add feature")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(started.Workspace, "keep.txt")); err != nil {
		t.Fatal(err)
	}
	workspaceRepo, err := OpenRepository(started.Workspace)
	if err != nil {
		t.Fatal(err)
	}
	_, forgedTree, err := workspaceRepo.Snapshot(ctx, "forged candidate\n")
	if err != nil {
		t.Fatal(err)
	}
	_, err = service.verifyAcceptance(ctx, base, forgedTree, []ProvenanceInput{{
		Role: "proposal", StateID: proposal.Proposal.ID, BaseTree: base.SourceTree, CandidateTree: proposal.Proposal.SourceTree,
	}}, "test-forgery")
	var provenance *ProvenanceError
	if !errors.As(err, &provenance) || !slices.Contains(provenance.Paths, "keep.txt") {
		t.Fatalf("verification error = %#v, want unauthorized keep.txt deletion", err)
	}
}

func TestTreeDeltaPreservesRenameModeSymlinkAndGitlinkIdentity(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink identity requires a symlink-capable test filesystem")
	}
	ctx := context.Background()
	root := t.TempDir()
	runGitTest(t, root, "init", "--quiet")
	writeTestFile(t, filepath.Join(root, "old.txt"), "rename me\n")
	writeTestFile(t, filepath.Join(root, "mode.txt"), "mode\n")
	if err := os.Symlink("old.txt", filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	runGitTest(t, root, "add", "old.txt", "mode.txt", "link")
	runGitTest(t, root, "-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "--quiet", "-m", "gitlink target one")
	firstCommit := runGitTest(t, root, "rev-parse", "HEAD")
	writeTestFile(t, filepath.Join(root, "seed.txt"), "second\n")
	runGitTest(t, root, "add", "seed.txt")
	runGitTest(t, root, "-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "--quiet", "-m", "gitlink target two")
	secondCommit := runGitTest(t, root, "rev-parse", "HEAD")
	runGitTest(t, root, "update-index", "--add", "--cacheinfo", "160000,"+firstCommit+",module")
	baseTree := runGitTest(t, root, "write-tree")

	if err := os.Rename(filepath.Join(root, "old.txt"), filepath.Join(root, "renamed.txt")); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(root, "mode.txt"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("renamed.txt", filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	runGitTest(t, root, "add", "-A")
	runGitTest(t, root, "update-index", "--add", "--cacheinfo", "160000,"+secondCommit+",module")
	candidateTree := runGitTest(t, root, "write-tree")
	repo, err := OpenRepository(root)
	if err != nil {
		t.Fatal(err)
	}
	deltas, err := repo.TreeDelta(ctx, baseTree, candidateTree)
	if err != nil {
		t.Fatal(err)
	}
	byPath := make(map[string]TreeDelta)
	for _, delta := range deltas {
		path := delta.NewPath
		if path == "" {
			path = delta.OldPath
		}
		byPath[path] = delta
	}
	if rename := byPath["renamed.txt"]; !strings.HasPrefix(rename.Status, "R") || rename.OldPath != "old.txt" || rename.OldOID == "" || rename.NewOID == "" {
		t.Fatalf("rename delta = %#v", rename)
	}
	if mode := byPath["mode.txt"]; mode.OldMode != "100644" || mode.NewMode != "100755" || mode.OldOID != mode.NewOID {
		t.Fatalf("mode delta = %#v", mode)
	}
	if link := byPath["link"]; link.OldMode != "120000" || link.NewMode != "120000" || link.OldOID == link.NewOID {
		t.Fatalf("symlink delta = %#v", link)
	}
	if module := byPath["module"]; module.OldMode != "160000" || module.NewMode != "160000" || module.OldOID != firstCommit || module.NewOID != secondCommit {
		t.Fatalf("gitlink delta = %#v", module)
	}
}

func TestLandTurnsConflictingVisibleRootChangesIntoAgentReconciliation(t *testing.T) {
	ctx := context.Background()
	service, _ := newTestProject(t, map[string]string{"shared.txt": "base\n"})
	started, err := service.CreatePrompt(ctx, "Edit shared file", "", "agent")
	if err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(started.Workspace, "shared.txt"), "proposal\n")
	proposal, err := service.Propose(ctx, started.Prompt.ID, "Proposal edit")
	if err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(service.Root, "shared.txt"), "visible root\n")

	_, err = service.Land(ctx, proposal.Proposal.ID, nil)
	var conflict *ConflictError
	if !errors.As(err, &conflict) {
		t.Fatalf("land error = %v, want ConflictError", err)
	}
	head, err := service.Store.AcceptedHead(ctx)
	if err != nil || head.Summary != "Capture out-of-band visible project changes" {
		t.Fatalf("accepted head = %#v, err=%v", head, err)
	}
	refresh, err := service.Refresh(ctx, proposal.Proposal.ID)
	if err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(refresh.Workspace, "shared.txt"), "visible root + proposal\n")
	if _, err := service.RunCheck(ctx, refresh.Prompt.ID, []string{"sh", "-c", "test \"$(cat shared.txt)\" = \"visible root + proposal\""}); err != nil {
		t.Fatal(err)
	}
	resolved, err := service.Propose(ctx, refresh.Prompt.ID, "Reconcile visible root and proposal")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Land(ctx, resolved.Proposal.ID, nil); err != nil {
		t.Fatal(err)
	}
	if contents, err := os.ReadFile(filepath.Join(service.Root, "shared.txt")); err != nil || string(contents) != "visible root + proposal\n" {
		t.Fatalf("shared file = %q, %v", string(contents), err)
	}
}

func TestLandBlocksDivergentRealIndexWithoutChangingIt(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	runGitTest(t, root, "init", "--quiet")
	writeTestFile(t, filepath.Join(root, "base.txt"), "base\n")
	runGitTest(t, root, "add", "base.txt")
	runGitTest(t, root, "-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "--quiet", "-m", "initial")
	service, initial, err := InitProject(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = service.Close() })
	started, err := service.CreatePrompt(ctx, "Add landed file", "", "agent")
	if err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(started.Workspace, "landed.txt"), "landed\n")
	proposal, err := service.Propose(ctx, started.Prompt.ID, "Landed file")
	if err != nil {
		t.Fatal(err)
	}

	writeTestFile(t, filepath.Join(root, "base.txt"), "staged\n")
	runGitTest(t, root, "add", "base.txt")
	writeTestFile(t, filepath.Join(root, "base.txt"), "base\n")
	beforeIndex := runGitTest(t, root, "ls-files", "--stage")
	_, err = service.Land(ctx, proposal.Proposal.ID, nil)
	var conflict *RootConflictError
	if !errors.As(err, &conflict) {
		t.Fatalf("land error = %v, want RootConflictError", err)
	}
	if got := runGitTest(t, root, "ls-files", "--stage"); got != beforeIndex {
		t.Fatalf("real index changed:\nwant %s\n got %s", beforeIndex, got)
	}
	head, err := service.Store.AcceptedHead(ctx)
	if err != nil || head.ID != initial.ID {
		t.Fatalf("accepted head = %s, err=%v", head.ID, err)
	}
	if _, err := os.Stat(filepath.Join(root, "landed.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("blocked land materialized proposal: %v", err)
	}
}

func TestLandPreservesIgnoredFilesAndRejectsIgnoredDestination(t *testing.T) {
	t.Run("unrelated ignored content", func(t *testing.T) {
		ctx := context.Background()
		service, _ := newTestProject(t, map[string]string{".gitignore": "cache/\n", "base.txt": "base\n"})
		writeTestFile(t, filepath.Join(service.Root, "cache", "private.txt"), "private\n")
		started, err := service.CreatePrompt(ctx, "Add visible file", "", "agent")
		if err != nil {
			t.Fatal(err)
		}
		writeTestFile(t, filepath.Join(started.Workspace, "landed.txt"), "landed\n")
		proposal, err := service.Propose(ctx, started.Prompt.ID, "Visible file")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := service.Land(ctx, proposal.Proposal.ID, nil); err != nil {
			t.Fatal(err)
		}
		if contents, err := os.ReadFile(filepath.Join(service.Root, "cache", "private.txt")); err != nil || string(contents) != "private\n" {
			t.Fatalf("ignored file changed: %q, %v", string(contents), err)
		}
	})

	t.Run("ignored destination collision", func(t *testing.T) {
		ctx := context.Background()
		service, initial := newTestProject(t, map[string]string{".gitignore": "generated\n"})
		writeTestFile(t, filepath.Join(service.Root, "generated"), "private\n")
		started, err := service.CreatePrompt(ctx, "Generate tracked output", "", "agent")
		if err != nil {
			t.Fatal(err)
		}
		writeTestFile(t, filepath.Join(started.Workspace, ".gitignore"), "")
		writeTestFile(t, filepath.Join(started.Workspace, "generated"), "accepted\n")
		proposal, err := service.Propose(ctx, started.Prompt.ID, "Tracked output")
		if err != nil {
			t.Fatal(err)
		}
		_, err = service.Land(ctx, proposal.Proposal.ID, nil)
		var conflict *RootConflictError
		if !errors.As(err, &conflict) {
			t.Fatalf("land error = %v, want RootConflictError", err)
		}
		head, err := service.Store.AcceptedHead(ctx)
		if err != nil || head.ID != initial.ID {
			t.Fatalf("accepted head = %s, err=%v", head.ID, err)
		}
		if contents, err := os.ReadFile(filepath.Join(service.Root, "generated")); err != nil || string(contents) != "private\n" {
			t.Fatalf("ignored collision was overwritten: %q, %v", string(contents), err)
		}
	})
}

func TestLandMaterializesFileDirectoryTypeChanges(t *testing.T) {
	ctx := context.Background()
	service, _ := newTestProject(t, map[string]string{
		"directory/old.txt": "old\n",
		"file":              "old file\n",
	})
	started, err := service.CreatePrompt(ctx, "Change source entry types", "", "agent")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(started.Workspace, "directory")); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(started.Workspace, "directory"), "now a file\n")
	if err := os.Remove(filepath.Join(started.Workspace, "file")); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(started.Workspace, "file", "new.txt"), "now a directory\n")
	proposal, err := service.Propose(ctx, started.Prompt.ID, "Change entry types")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Land(ctx, proposal.Proposal.ID, nil); err != nil {
		t.Fatal(err)
	}
	if contents, err := os.ReadFile(filepath.Join(service.Root, "directory")); err != nil || string(contents) != "now a file\n" {
		t.Fatalf("directory replacement = %q, %v", string(contents), err)
	}
	if contents, err := os.ReadFile(filepath.Join(service.Root, "file", "new.txt")); err != nil || string(contents) != "now a directory\n" {
		t.Fatalf("file replacement = %q, %v", string(contents), err)
	}
}

func TestFailedLandValidationDoesNotMaterializeVisibleRoot(t *testing.T) {
	ctx := context.Background()
	service, initial := newTestProject(t, map[string]string{"base.txt": "base\n"})
	started, err := service.CreatePrompt(ctx, "Add invalid file", "", "agent")
	if err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(started.Workspace, "invalid.txt"), "invalid\n")
	proposal, err := service.Propose(ctx, started.Prompt.ID, "Invalid")
	if err != nil {
		t.Fatal(err)
	}
	_, err = service.Land(ctx, proposal.Proposal.ID, []string{"sh", "-c", "exit 7"})
	var failed *CheckFailedError
	if !errors.As(err, &failed) {
		t.Fatalf("land error = %v, want CheckFailedError", err)
	}
	if _, err := os.Stat(filepath.Join(service.Root, "invalid.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("failed land materialized file: %v", err)
	}
	head, err := service.Store.AcceptedHead(ctx)
	if err != nil || head.ID != initial.ID {
		t.Fatalf("accepted head = %s, err=%v", head.ID, err)
	}
	attempt, err := service.Store.GetAttempt(ctx, proposal.Proposal.AttemptID)
	if err != nil {
		t.Fatal(err)
	}
	if attempt.HeadStateID != proposal.Proposal.ID {
		t.Fatalf("failed validation invalidated proposal: attempt head = %s, want %s", attempt.HeadStateID, proposal.Proposal.ID)
	}
	retried, err := service.Land(ctx, proposal.Proposal.ID, []string{"sh", "-c", "exit 0"})
	if err != nil {
		t.Fatalf("retry same frozen proposal after correcting validation: %v", err)
	}
	if retried.State.SourceTree != proposal.Proposal.SourceTree {
		t.Fatalf("retried tree = %s, want frozen proposal tree %s", retried.State.SourceTree, proposal.Proposal.SourceTree)
	}
}

func TestLandHelpDoesNotParseHelpAsProposalID(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := RunCLI([]string{"land", "--help"}, &stdout, &stderr); code != 0 {
		t.Fatalf("hop land --help exit = %d, stderr=%q", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "Usage: hop land STATE") {
		t.Fatalf("hop land --help output = %q", stdout.String())
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
	configuredRoot, hadConfiguredRoot := os.LookupEnv("HOP_ROOT")
	if err := os.Unsetenv("HOP_ROOT"); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if hadConfiguredRoot {
			_ = os.Setenv("HOP_ROOT", configuredRoot)
		} else {
			_ = os.Unsetenv("HOP_ROOT")
		}
	}()
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

func runCLIJSONInputTest(t *testing.T, args []string, input string) map[string]any {
	t.Helper()
	configuredRoot, hadConfiguredRoot := os.LookupEnv("HOP_ROOT")
	if err := os.Unsetenv("HOP_ROOT"); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if hadConfiguredRoot {
			_ = os.Setenv("HOP_ROOT", configuredRoot)
		} else {
			_ = os.Unsetenv("HOP_ROOT")
		}
	}()
	var stdout, stderr bytes.Buffer
	if code := RunCLIWithInput(args, strings.NewReader(input), &stdout, &stderr); code != 0 {
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
