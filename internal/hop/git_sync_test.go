package hop

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

type gitSyncFixture struct {
	service  *Service
	initial  State
	accepted State
	localTip string
	remote   string
}

func newGitSyncFixture(t *testing.T, branchInProof bool, publicationStatus string) gitSyncFixture {
	t.Helper()
	ctx := context.Background()
	root := t.TempDir()
	runGitTest(t, root, "init", "--quiet", "--initial-branch=main")
	for index := 0; index < 129; index++ {
		writeTestFile(t, filepath.Join(root, "legacy", fmt.Sprintf("file-%03d.txt", index)), "legacy\n")
	}
	writeTestFile(t, filepath.Join(root, "app.txt"), "stale B\n")
	writeTestFile(t, filepath.Join(root, "mode.txt"), "mode\n")
	submoduleRoot := t.TempDir()
	runGitTest(t, submoduleRoot, "init", "--quiet", "--initial-branch=main")
	writeTestFile(t, filepath.Join(submoduleRoot, "sub.txt"), "gitlink target\n")
	runGitTest(t, submoduleRoot, "add", "sub.txt")
	runGitTest(t, submoduleRoot, "-c", "user.name=Replay", "-c", "user.email=replay@example.com", "commit", "--quiet", "-m", "submodule")
	runGitTest(t, root, "-c", "protocol.file.allow=always", "submodule", "add", "--quiet", submoduleRoot, "module")
	if runtime.GOOS != "windows" {
		if err := os.Symlink("app.txt", filepath.Join(root, "link")); err != nil {
			t.Fatal(err)
		}
	}
	runGitTest(t, root, "add", "-A")
	runGitTest(t, root, "-c", "user.name=Replay", "-c", "user.email=replay@example.com", "commit", "--quiet", "-m", "stale branch B")
	localTip := runGitTest(t, root, "rev-parse", "HEAD")
	remote := filepath.Join(t.TempDir(), "origin.git")
	runGitTest(t, root, "init", "--quiet", "--bare", remote)
	runGitTest(t, root, "remote", "add", "origin", remote)
	runGitTest(t, root, "push", "--quiet", "--set-upstream", "origin", "main")

	service, initial, err := InitProject(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = service.Close() })
	writeTestFile(t, filepath.Join(root, "app.txt"), "accepted A\n")
	if err := os.Chmod(filepath.Join(root, "mode.txt"), 0o755); err != nil {
		t.Fatal(err)
	}
	for index := 0; index < 40; index++ {
		if err := os.Remove(filepath.Join(root, "legacy", fmt.Sprintf("file-%03d.txt", index))); err != nil {
			t.Fatal(err)
		}
	}
	writeTestFile(t, filepath.Join(root, "accepted-only.txt"), "accepted\n")
	candidateTree, err := service.Repo.WorktreeTree(ctx, initial.SourceTree)
	if err != nil {
		t.Fatal(err)
	}
	parent := initial.GitCommit
	for index := 0; index < 40; index++ {
		parent, err = service.Repo.CommitTree(ctx, initial.SourceTree, []string{parent}, fmt.Sprintf("accepted history %d\n", index))
		if err != nil {
			t.Fatal(err)
		}
	}
	acceptedID := newID("a")
	commit, err := service.Repo.CommitTree(ctx, candidateTree, []string{parent}, "accepted A\n")
	if err != nil {
		t.Fatal(err)
	}
	proof, err := service.verifyAcceptance(ctx, initial, candidateTree, []ProvenanceInput{{
		Role: "proposal", BaseTree: initial.SourceTree, CandidateTree: candidateTree,
	}}, "land")
	if err != nil {
		t.Fatal(err)
	}
	if branchInProof {
		proof.BranchRef = "refs/heads/main"
		proof.CompositionDigest, err = compositionDigest(proof)
		if err != nil {
			t.Fatal(err)
		}
	}
	parents := canonicalizeParents([]Parent{{StateID: initial.ID, Role: "canonical_parent", Order: 0}})
	accepted := State{
		ID: acceptedID, Kind: StateAccepted, CanonicalAnchorID: initial.ID,
		SourceTree: candidateTree, GitCommit: commit, Summary: "accepted A",
		Agent: "test", Provenance: proof, CreatedAt: time.Now().UTC(), Parents: parents,
	}
	accepted.Digest, err = digestState(accepted, parents)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.pinState(ctx, accepted); err != nil {
		t.Fatal(err)
	}
	accepted, err = service.Store.CASAccept(ctx, initial.ID, accepted, parents)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.Repo.UpdateHiddenRef(ctx, acceptedRef, accepted.GitCommit); err != nil {
		t.Fatal(err)
	}
	if err := service.Store.CASMaterializedHead(ctx, initial.ID, accepted.ID); err != nil {
		t.Fatal(err)
	}
	remoteTip := localTip
	if publicationStatus == "current" {
		remoteTip = accepted.GitCommit
		runGitTest(t, root, "push", "--quiet", "origin", accepted.GitCommit+":refs/heads/main")
		runGitTest(t, root, "update-ref", "refs/remotes/origin/main", accepted.GitCommit)
	}
	now := time.Now().UTC()
	if err := service.Store.PutPublication(ctx, PublicationStatus{
		AcceptedStateID: accepted.ID, Commit: accepted.GitCommit, Status: publicationStatus,
		Remote: "origin", Ref: "refs/heads/main", RemoteTip: remoteTip,
		AttemptedAt: &now, Retryable: publicationStatus != "current",
	}); err != nil {
		t.Fatal(err)
	}
	return gitSyncFixture{service: service, initial: initial, accepted: accepted, localTip: localTip, remote: remote}
}

func TestSyncGitCleansSynapsisShapedProjectionWithoutRewritingFiles(t *testing.T) {
	ctx := context.Background()
	fixture := newGitSyncFixture(t, true, "current")
	root := fixture.service.Root
	beforeApp, err := os.ReadFile(filepath.Join(root, "app.txt"))
	if err != nil {
		t.Fatal(err)
	}
	beforeMode, err := os.Lstat(filepath.Join(root, "mode.txt"))
	if err != nil {
		t.Fatal(err)
	}
	beforeLink := ""
	beforeGitlink := runGitTest(t, filepath.Join(root, "module"), "rev-parse", "HEAD")
	beforeSubmoduleBytes, err := os.ReadFile(filepath.Join(root, "module", "sub.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" {
		beforeLink, err = os.Readlink(filepath.Join(root, "link"))
		if err != nil {
			t.Fatal(err)
		}
	}
	afterGitlink := runGitTest(t, filepath.Join(root, "module"), "rev-parse", "HEAD")
	afterSubmoduleBytes, err := os.ReadFile(filepath.Join(root, "module", "sub.txt"))
	if err != nil || afterGitlink != beforeGitlink || string(afterSubmoduleBytes) != string(beforeSubmoduleBytes) {
		t.Fatalf("gitlink changed from %s to %s or its bytes changed: %v", beforeGitlink, afterGitlink, err)
	}
	if raw := runGitTest(t, root, "status", "--porcelain=v1"); raw == "" {
		t.Fatal("fixture does not reproduce projection-only raw Git dirtiness")
	}
	result, err := fixture.service.SyncGit(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != "synchronized" || !result.Changed {
		t.Fatalf("sync result = %#v", result)
	}
	if got := runGitTest(t, root, "rev-parse", "HEAD"); got != fixture.accepted.GitCommit {
		t.Fatalf("HEAD = %s, want accepted %s", got, fixture.accepted.GitCommit)
	}
	if raw := runGitTest(t, root, "status", "--porcelain=v1"); raw != "" {
		t.Fatalf("raw Git status after sync = %q", raw)
	}
	if indexTree, err := fixture.service.Repo.userIndexTree(ctx); err != nil || indexTree != fixture.accepted.SourceTree {
		t.Fatalf("index tree = %s, err=%v", indexTree, err)
	}
	afterApp, _ := os.ReadFile(filepath.Join(root, "app.txt"))
	afterMode, _ := os.Lstat(filepath.Join(root, "mode.txt"))
	if string(afterApp) != string(beforeApp) || afterMode.Mode().Perm() != beforeMode.Mode().Perm() {
		t.Fatal("Git synchronization rewrote accepted file bytes or modes")
	}
	if runtime.GOOS != "windows" {
		afterLink, readErr := os.Readlink(filepath.Join(root, "link"))
		if readErr != nil || afterLink != beforeLink {
			t.Fatalf("symlink changed from %q to %q: %v", beforeLink, afterLink, readErr)
		}
	}
	if ahead, behind, err := fixture.service.Repo.DivergenceCounts(ctx, fixture.accepted.GitCommit, "refs/remotes/origin/main"); err != nil || ahead != 0 || behind != 0 {
		t.Fatalf("upstream divergence = %d ahead %d behind, err=%v", ahead, behind, err)
	}
	retried, err := fixture.service.SyncGit(ctx)
	if err != nil || retried.Status != "already_synchronized" || retried.Changed {
		t.Fatalf("idempotent sync = %#v, err=%v", retried, err)
	}
}

func TestSyncGitMigrationInfersV112BranchFromPublication(t *testing.T) {
	fixture := newGitSyncFixture(t, false, "current")
	result, err := fixture.service.SyncGit(context.Background())
	if err != nil || result.Status != "synchronized" {
		t.Fatalf("v1.1.2 migration sync = %#v, err=%v", result, err)
	}
	if raw := runGitTest(t, fixture.service.Root, "status", "--porcelain=v1"); raw != "" {
		t.Fatalf("migration raw status = %q", raw)
	}
}

func TestBeginPromptRepairsExistingSafeProjection(t *testing.T) {
	fixture := newGitSyncFixture(t, true, "current")
	result, err := fixture.service.BeginPrompt(context.Background(), "Inspect the accepted project", "", "agent", "sync-git-begin")
	if err != nil {
		t.Fatal(err)
	}
	if result.GitSync == nil || result.GitSync.Status != "synchronized" {
		t.Fatalf("begin Git synchronization = %#v", result.GitSync)
	}
	if _, err := fixture.service.Store.GetState(context.Background(), result.Prompt.ID); err != nil {
		t.Fatalf("begin did not persist prompt before repair: %v", err)
	}
	if raw := runGitTest(t, fixture.service.Root, "status", "--porcelain=v1"); raw != "" {
		t.Fatalf("begin left raw Git status = %q", raw)
	}
}

func TestBeginPromptPreservesInitialDirtyGitState(t *testing.T) {
	root := t.TempDir()
	runGitTest(t, root, "init", "--quiet", "--initial-branch=main")
	writeTestFile(t, filepath.Join(root, "tracked.txt"), "base\n")
	runGitTest(t, root, "add", "tracked.txt")
	runGitTest(t, root, "-c", "user.name=Replay", "-c", "user.email=replay@example.com", "commit", "--quiet", "-m", "base")
	writeTestFile(t, filepath.Join(root, "tracked.txt"), "staged\n")
	runGitTest(t, root, "add", "tracked.txt")
	writeTestFile(t, filepath.Join(root, "tracked.txt"), "working\n")
	beforeHead := runGitTest(t, root, "rev-parse", "HEAD")
	beforeIndex := runGitTest(t, root, "ls-files", "--stage")
	beforeStatus := runGitTest(t, root, "status", "--porcelain=v1")
	service, _, err := InitProject(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = service.Close() })
	result, err := service.BeginPrompt(context.Background(), "Preserve existing work", "", "agent", "initial-dirty")
	if err != nil {
		t.Fatal(err)
	}
	if result.GitSync != nil {
		t.Fatalf("initial begin attempted Git synchronization: %#v", result.GitSync)
	}
	if got := runGitTest(t, root, "rev-parse", "HEAD"); got != beforeHead {
		t.Fatalf("initial begin moved HEAD from %s to %s", beforeHead, got)
	}
	if got := runGitTest(t, root, "ls-files", "--stage"); got != beforeIndex {
		t.Fatal("initial begin changed the staged index")
	}
	if got := runGitTest(t, root, "status", "--porcelain=v1"); got != beforeStatus {
		t.Fatalf("initial begin changed Git status: got %q, want %q", got, beforeStatus)
	}
}

func TestSyncGitCLIReportsBlockedReasonAndAction(t *testing.T) {
	fixture := newGitSyncFixture(t, true, "current")
	writeTestFile(t, filepath.Join(fixture.service.Root, "app.txt"), "preserve this edit\n")
	previousRoot, hadRoot := os.LookupEnv("HOP_ROOT")
	if err := os.Setenv("HOP_ROOT", fixture.service.Root); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if hadRoot {
			_ = os.Setenv("HOP_ROOT", previousRoot)
		} else {
			_ = os.Unsetenv("HOP_ROOT")
		}
	}()
	var stdout, stderr bytes.Buffer
	if code := RunCLI([]string{"sync-git", "--json"}, &stdout, &stderr); code != 23 {
		t.Fatalf("sync-git exit = %d, want 23; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	var payload struct {
		OK       bool          `json:"ok"`
		Category string        `json:"category"`
		Data     GitSyncResult `json:"data"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.OK || payload.Category != "git_sync_blocked" || payload.Data.Reason == "" || payload.Data.SafeNextAction == "" {
		t.Fatalf("blocked CLI payload = %#v", payload)
	}
	contents, err := os.ReadFile(filepath.Join(fixture.service.Root, "app.txt"))
	if err != nil || string(contents) != "preserve this edit\n" {
		t.Fatalf("blocked CLI changed visible edit: %q, %v", contents, err)
	}
}

func TestSyncGitKeepsTruthfulLocalStateWhenPublicationFailed(t *testing.T) {
	fixture := newGitSyncFixture(t, true, "failed")
	result, err := fixture.service.SyncGit(context.Background())
	if err != nil || result.Status != "synchronized" {
		t.Fatalf("failed-publication sync = %#v, err=%v", result, err)
	}
	if raw := runGitTest(t, fixture.service.Root, "status", "--porcelain=v1"); raw != "" {
		t.Fatalf("failed-publication raw status = %q", raw)
	}
	status, err := fixture.service.Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if status.Publication.Status != "failed" || status.Git.AcceptedAheadUpstream == 0 || status.Git.LocalBehind != 0 {
		t.Fatalf("failed-publication status = %#v", status)
	}
}

func assertGitSyncBlockedWithoutLoss(t *testing.T, fixture gitSyncFixture, mutate func()) GitSyncResult {
	t.Helper()
	root := fixture.service.Root
	mutate()
	beforeIndex, err := os.ReadFile(filepath.Join(fixture.service.Repo.GitDir(), "index"))
	if err != nil {
		t.Fatal(err)
	}
	expectedMain := runGitTest(t, root, "rev-parse", "refs/heads/main")
	result, err := fixture.service.SyncGit(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != "blocked" || result.Reason == "" || result.SafeNextAction == "" {
		t.Fatalf("blocked result = %#v", result)
	}
	if got := runGitTest(t, root, "rev-parse", "refs/heads/main"); got != expectedMain {
		t.Fatalf("blocked sync moved main to %s, want %s", got, expectedMain)
	}
	afterIndex, err := os.ReadFile(filepath.Join(fixture.service.Repo.GitDir(), "index"))
	if err != nil {
		t.Fatal(err)
	}
	if string(afterIndex) != string(beforeIndex) {
		t.Fatal("blocked sync changed the real index")
	}
	return result
}

func TestSyncGitBlocksUnsafeRepositoryStates(t *testing.T) {
	t.Run("unstaged edit", func(t *testing.T) {
		fixture := newGitSyncFixture(t, true, "current")
		result := assertGitSyncBlockedWithoutLoss(t, fixture, func() {
			writeTestFile(t, filepath.Join(fixture.service.Root, "app.txt"), "user edit\n")
		})
		if !strings.Contains(result.Reason, "visible files") {
			t.Fatalf("reason = %q", result.Reason)
		}
		contents, _ := os.ReadFile(filepath.Join(fixture.service.Root, "app.txt"))
		if string(contents) != "user edit\n" {
			t.Fatal("unstaged edit was lost")
		}
	})
	t.Run("staged edit", func(t *testing.T) {
		fixture := newGitSyncFixture(t, true, "current")
		result := assertGitSyncBlockedWithoutLoss(t, fixture, func() {
			writeTestFile(t, filepath.Join(fixture.service.Root, "staged.txt"), "staged\n")
			runGitTest(t, fixture.service.Root, "add", "staged.txt")
			if err := os.Remove(filepath.Join(fixture.service.Root, "staged.txt")); err != nil {
				t.Fatal(err)
			}
		})
		if !strings.Contains(result.Reason, "staged changes") {
			t.Fatalf("reason = %q", result.Reason)
		}
	})
	t.Run("detached HEAD", func(t *testing.T) {
		fixture := newGitSyncFixture(t, true, "current")
		result := assertGitSyncBlockedWithoutLoss(t, fixture, func() {
			runGitTest(t, fixture.service.Root, "update-ref", "--no-deref", "HEAD", fixture.localTip)
		})
		if !strings.Contains(result.Reason, "detached") {
			t.Fatalf("reason = %q", result.Reason)
		}
	})
	t.Run("another branch", func(t *testing.T) {
		fixture := newGitSyncFixture(t, true, "current")
		result := assertGitSyncBlockedWithoutLoss(t, fixture, func() {
			runGitTest(t, fixture.service.Root, "update-ref", "refs/heads/feature", fixture.localTip)
			runGitTest(t, fixture.service.Root, "symbolic-ref", "HEAD", "refs/heads/feature")
		})
		if !strings.Contains(result.Reason, "targets refs/heads/main") {
			t.Fatalf("reason = %q", result.Reason)
		}
	})
	t.Run("local commit after accepted", func(t *testing.T) {
		fixture := newGitSyncFixture(t, true, "current")
		child, err := fixture.service.Repo.CommitTree(context.Background(), fixture.accepted.SourceTree, []string{fixture.accepted.GitCommit}, "local newer\n")
		if err != nil {
			t.Fatal(err)
		}
		fixture.localTip = child
		result := assertGitSyncBlockedWithoutLoss(t, fixture, func() {
			runGitTest(t, fixture.service.Root, "read-tree", fixture.accepted.SourceTree)
			runGitTest(t, fixture.service.Root, "update-ref", "refs/heads/main", child)
		})
		if !strings.Contains(result.Reason, "local branch contains commits newer") {
			t.Fatalf("reason = %q", result.Reason)
		}
	})
	t.Run("divergence", func(t *testing.T) {
		fixture := newGitSyncFixture(t, true, "current")
		divergent, err := fixture.service.Repo.CommitTree(context.Background(), fixture.initial.SourceTree, []string{fixture.localTip}, "divergent\n")
		if err != nil {
			t.Fatal(err)
		}
		fixture.localTip = divergent
		result := assertGitSyncBlockedWithoutLoss(t, fixture, func() {
			runGitTest(t, fixture.service.Root, "update-ref", "refs/heads/main", divergent)
		})
		if !strings.Contains(result.Reason, "diverged") {
			t.Fatalf("reason = %q", result.Reason)
		}
	})
}

func TestSyncGitBlocksGitOperationsAndLocks(t *testing.T) {
	markers := []struct {
		name string
		path string
	}{
		{"merge", "MERGE_HEAD"},
		{"rebase", "rebase-merge"},
		{"cherry-pick", "CHERRY_PICK_HEAD"},
		{"bisect", "BISECT_LOG"},
		{"index lock", "index.lock"},
	}
	for _, marker := range markers {
		t.Run(marker.name, func(t *testing.T) {
			fixture := newGitSyncFixture(t, true, "current")
			path := filepath.Join(fixture.service.Repo.GitDir(), marker.path)
			result := assertGitSyncBlockedWithoutLoss(t, fixture, func() {
				if strings.Contains(marker.path, "rebase-") {
					if err := os.MkdirAll(path, 0o755); err != nil {
						t.Fatal(err)
					}
				} else if err := os.WriteFile(path, []byte(fixture.localTip+"\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			})
			if !strings.Contains(strings.ToLower(result.Reason), strings.Split(marker.name, " ")[0]) {
				t.Fatalf("reason = %q", result.Reason)
			}
		})
	}
}

func TestSyncGitRevalidatesAfterPreparationRace(t *testing.T) {
	fixture := newGitSyncFixture(t, true, "current")
	fixture.service.Repo.syncGitBeforeFinalValidation = func() {
		_ = os.WriteFile(filepath.Join(fixture.service.Root, "raced.txt"), []byte("preserve\n"), 0o644)
	}
	result := assertGitSyncBlockedWithoutLoss(t, fixture, func() {})
	if !strings.Contains(result.Reason, "visible files") {
		t.Fatalf("race reason = %q", result.Reason)
	}
	if contents, err := os.ReadFile(filepath.Join(fixture.service.Root, "raced.txt")); err != nil || string(contents) != "preserve\n" {
		t.Fatalf("raced file = %q, err=%v", contents, err)
	}
	if _, err := os.Stat(filepath.Join(fixture.service.Repo.GitDir(), "index.lock")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Hop left index.lock after blocked race: %v", err)
	}
}

func TestSyncGitRevalidatesAcceptedStateBeforeMutation(t *testing.T) {
	fixture := newGitSyncFixture(t, true, "current")
	fixture.service.Repo.syncGitBeforeFinalValidation = func() {
		if err := fixture.service.Store.CASMaterializedHead(context.Background(), fixture.accepted.ID, fixture.initial.ID); err != nil {
			t.Errorf("race marker mutation: %v", err)
		}
	}
	result := assertGitSyncBlockedWithoutLoss(t, fixture, func() {})
	if !strings.Contains(strings.ToLower(result.Reason), "accepted or materialized state changed") {
		t.Fatalf("race reason = %q", result.Reason)
	}
}

func TestSyncGitDoesNotOverwriteConcurrentBranchUpdate(t *testing.T) {
	fixture := newGitSyncFixture(t, true, "current")
	divergent, err := fixture.service.Repo.CommitTree(context.Background(), fixture.initial.SourceTree, []string{fixture.localTip}, "concurrent branch update\n")
	if err != nil {
		t.Fatal(err)
	}
	gitDir := fixture.service.Repo.GitDir()
	beforeIndex, err := os.ReadFile(filepath.Join(gitDir, "index"))
	if err != nil {
		t.Fatal(err)
	}
	fixture.service.Repo.syncGitBeforeFinalValidation = func() {
		if updateErr := fixture.service.Repo.UpdateRef(context.Background(), "refs/heads/main", divergent, fixture.localTip); updateErr != nil {
			t.Errorf("concurrent ref update: %v", updateErr)
		}
	}
	result, err := fixture.service.SyncGit(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != "blocked" || !strings.Contains(result.Reason, "diverged") {
		t.Fatalf("race result = %#v", result)
	}
	if got := runGitTest(t, fixture.service.Root, "rev-parse", "refs/heads/main"); got != divergent {
		t.Fatalf("concurrent branch update was overwritten: got %s, want %s", got, divergent)
	}
	afterIndex, err := os.ReadFile(filepath.Join(gitDir, "index"))
	if err != nil || string(afterIndex) != string(beforeIndex) {
		t.Fatalf("concurrent ref race changed index: %v", err)
	}
}

func TestSyncGitRollsBackIfHeadSwitchesAfterRefCAS(t *testing.T) {
	fixture := newGitSyncFixture(t, true, "current")
	gitDir := fixture.service.Repo.GitDir()
	beforeIndex, err := os.ReadFile(filepath.Join(gitDir, "index"))
	if err != nil {
		t.Fatal(err)
	}
	runGitTest(t, fixture.service.Root, "update-ref", "refs/heads/feature", fixture.localTip)
	fixture.service.Repo.syncGitAfterRefUpdate = func() {
		if _, switchErr := fixture.service.Repo.run(context.Background(), nil, nil, "symbolic-ref", "HEAD", "refs/heads/feature"); switchErr != nil {
			t.Errorf("concurrent HEAD switch: %v", switchErr)
		}
	}
	result, err := fixture.service.SyncGit(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != "blocked" || !strings.Contains(result.Reason, "HEAD changed") {
		t.Fatalf("HEAD race result = %#v", result)
	}
	if got := runGitTest(t, fixture.service.Root, "rev-parse", "refs/heads/main"); got != fixture.localTip {
		t.Fatalf("main was not rolled back: got %s, want %s", got, fixture.localTip)
	}
	if got := runGitTest(t, fixture.service.Root, "symbolic-ref", "HEAD"); got != "refs/heads/feature" {
		t.Fatalf("concurrent HEAD switch was overwritten: %s", got)
	}
	afterIndex, err := os.ReadFile(filepath.Join(gitDir, "index"))
	if err != nil || string(afterIndex) != string(beforeIndex) {
		t.Fatalf("HEAD race changed index: %v", err)
	}
}
