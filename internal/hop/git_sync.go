package hop

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

func blockedGitSync(accepted, branch, previous, reason, action string) GitSyncResult {
	return GitSyncResult{
		Status: "blocked", BranchRef: branch, PreviousCommit: previous,
		AcceptedCommit: accepted, Reason: reason, SafeNextAction: action,
	}
}

func (r *Repository) AttachedHeadRef(ctx context.Context) (string, bool, error) {
	ref, attached, err := r.optionalGitOutput(ctx, "symbolic-ref", "--quiet", "HEAD")
	if err != nil {
		return "", false, fmt.Errorf("inspect attached HEAD: %w", err)
	}
	if !attached || ref == "" {
		return "", false, nil
	}
	if err := r.validateRef(ctx, ref); err != nil {
		return "", false, fmt.Errorf("validate attached HEAD ref: %w", err)
	}
	return ref, true, nil
}

func (r *Repository) gitOperationBlock(intendedRef string, ignoreIndexLock, ignoreHeadLock bool) (string, string, error) {
	type marker struct {
		path   string
		reason string
		action string
	}
	markers := []marker{
		{filepath.Join(r.gitDir, "MERGE_HEAD"), "a Git merge is in progress", "finish or abort the merge without discarding work, then run hop sync-git again"},
		{filepath.Join(r.gitDir, "CHERRY_PICK_HEAD"), "a Git cherry-pick is in progress", "finish or abort the cherry-pick without discarding work, then run hop sync-git again"},
		{filepath.Join(r.gitDir, "REVERT_HEAD"), "a Git revert is in progress", "finish or abort the revert without discarding work, then run hop sync-git again"},
		{filepath.Join(r.gitDir, "rebase-merge"), "a Git rebase is in progress", "finish or abort the rebase without discarding work, then run hop sync-git again"},
		{filepath.Join(r.gitDir, "rebase-apply"), "a Git rebase or apply operation is in progress", "finish or abort that Git operation without discarding work, then run hop sync-git again"},
		{filepath.Join(r.gitDir, "BISECT_LOG"), "a Git bisect is in progress", "finish or reset the bisect without discarding work, then run hop sync-git again"},
		{filepath.Join(r.gitDir, "sequencer"), "a sequenced Git operation is in progress", "finish or abort that Git operation without discarding work, then run hop sync-git again"},
		{filepath.Join(r.commonGitDir, "packed-refs.lock"), "Git has locked packed refs", "wait for the active Git operation to finish, then run hop sync-git again"},
	}
	if !ignoreHeadLock {
		markers = append(markers, marker{filepath.Join(r.gitDir, "HEAD.lock"), "Git has locked HEAD", "wait for the active Git operation to finish, then run hop sync-git again"})
	}
	if !ignoreIndexLock {
		markers = append(markers, marker{filepath.Join(r.gitDir, "index.lock"), "Git has locked the real index", "wait for the active Git operation to finish; if no Git process is running, inspect the stale index.lock before removing it, then run hop sync-git again"})
	}
	if strings.HasPrefix(intendedRef, "refs/") {
		markers = append(markers, marker{
			filepath.Join(r.commonGitDir, filepath.FromSlash(intendedRef)) + ".lock",
			"Git has locked " + intendedRef,
			"wait for the active Git operation to finish, then run hop sync-git again",
		})
	}
	for _, candidate := range markers {
		if _, err := os.Lstat(candidate.path); err == nil {
			return candidate.reason, candidate.action, nil
		} else if !errors.Is(err, os.ErrNotExist) {
			return "", "", fmt.Errorf("inspect Git operation marker %s: %w", candidate.path, err)
		}
	}
	return "", "", nil
}

func (r *Repository) inspectGitSync(
	ctx context.Context,
	intendedRef, acceptedCommit, acceptedTree string,
	ignoreIndexLock, ignoreHeadLock bool,
) (GitSyncResult, string, error) {
	result := GitSyncResult{Status: "ready", BranchRef: intendedRef, AcceptedCommit: acceptedCommit}
	commitTree, err := r.resolveTree(ctx, acceptedCommit)
	if err != nil {
		return result, "", err
	}
	acceptedTree, err = r.resolveTree(ctx, acceptedTree)
	if err != nil {
		return result, "", err
	}
	if commitTree != acceptedTree {
		return blockedGitSync(acceptedCommit, intendedRef, "", "the accepted commit does not resolve to its recorded accepted tree", "run hop doctor and repair the accepted-state integrity problem before retrying"), "", nil
	}
	attachedRef, attached, err := r.AttachedHeadRef(ctx)
	if err != nil {
		return result, "", err
	}
	if !attached {
		return blockedGitSync(acceptedCommit, intendedRef, "", "HEAD is detached", "attach HEAD to the intended branch without discarding work, then run hop sync-git again"), "", nil
	}
	if intendedRef == "" {
		return blockedGitSync(acceptedCommit, attachedRef, "", "Hop has no durable record of which local branch this accepted state belongs to", "create a fresh accepted transition from the intended attached branch, then run hop sync-git again"), "", nil
	}
	if attachedRef != intendedRef {
		return blockedGitSync(acceptedCommit, intendedRef, "", fmt.Sprintf("HEAD is attached to %s, but this accepted state targets %s", attachedRef, intendedRef), "preserve any work, switch back to the intended branch, then run hop sync-git again"), "", nil
	}
	if reason, action, err := r.gitOperationBlock(intendedRef, ignoreIndexLock, ignoreHeadLock); err != nil {
		return result, "", err
	} else if reason != "" {
		return blockedGitSync(acceptedCommit, intendedRef, "", reason, action), "", nil
	}
	localTip, exists, err := r.optionalGitOutput(ctx, "rev-parse", "--verify", "--quiet", intendedRef+"^{commit}")
	if err != nil {
		return result, "", err
	}
	if !exists || localTip == "" {
		return blockedGitSync(acceptedCommit, intendedRef, "", "the intended local branch has no commit tip", "initialize or restore the intended branch without discarding work, then run hop sync-git again"), "", nil
	}
	result.PreviousCommit = localTip
	head, headExists, err := r.Head(ctx)
	if err != nil {
		return result, "", err
	}
	if !headExists || head != localTip {
		return blockedGitSync(acceptedCommit, intendedRef, localTip, "HEAD and the intended branch tip changed during validation", "retry hop sync-git after the concurrent Git operation finishes"), "", nil
	}
	actualTree, err := r.WorktreeTree(ctx, acceptedTree)
	if err != nil {
		return result, "", err
	}
	if actualTree != acceptedTree {
		paths, pathErr := r.ChangedPaths(ctx, acceptedTree, actualTree)
		if pathErr != nil {
			return result, "", pathErr
		}
		return blockedGitSync(acceptedCommit, intendedRef, localTip, "visible files do not exactly match the accepted tree: "+strings.Join(paths, ", "), "preserve those files in a Hop attempt, restore a proven accepted projection, then run hop sync-git again"), "", nil
	}
	headTree, err := r.resolveTree(ctx, localTip)
	if err != nil {
		return result, "", err
	}
	indexTree, err := r.userIndexTree(ctx)
	if err != nil {
		return blockedGitSync(acceptedCommit, intendedRef, localTip, "the real Git index could not be proven safe: "+err.Error(), "finish or preserve the staged Git operation, then run hop sync-git again"), "", nil
	}
	if indexTree != headTree && indexTree != acceptedTree {
		paths, pathErr := r.ChangedPaths(ctx, headTree, indexTree)
		if pathErr != nil {
			return result, "", pathErr
		}
		return blockedGitSync(acceptedCommit, intendedRef, localTip, "the real Git index contains staged changes: "+strings.Join(paths, ", "), "preserve or commit the staged work intentionally, then run hop sync-git again"), "", nil
	}
	ancestor, err := r.IsAncestor(ctx, localTip, acceptedCommit)
	if err != nil {
		return result, "", err
	}
	if !ancestor {
		acceptedIsAncestor, ancestorErr := r.IsAncestor(ctx, acceptedCommit, localTip)
		if ancestorErr != nil {
			return result, "", ancestorErr
		}
		if acceptedIsAncestor {
			return blockedGitSync(acceptedCommit, intendedRef, localTip, "the local branch contains commits newer than Hop's accepted commit", "preserve those local commits and reconcile them through a Hop attempt before retrying"), "", nil
		}
		return blockedGitSync(acceptedCommit, intendedRef, localTip, "the local branch and Hop's accepted commit have diverged", "preserve both histories and reconcile them through a Hop attempt before retrying"), "", nil
	}
	return result, indexTree, nil
}

// SyncBranchIndex fast-forwards one proven attached branch and replaces only
// its projection-only index. It never writes the worktree. The ref update is a
// compare-and-swap against the exact validated old object ID.
func (r *Repository) SyncBranchIndex(
	ctx context.Context,
	intendedRef, acceptedCommit, acceptedTree string,
	revalidateState func() error,
) (GitSyncResult, error) {
	result, indexTree, err := r.inspectGitSync(ctx, intendedRef, acceptedCommit, acceptedTree, false, false)
	if err != nil || result.Status == "blocked" {
		return result, err
	}
	if result.PreviousCommit == acceptedCommit {
		if indexTree == acceptedTree {
			result.Status = "already_synchronized"
			return result, nil
		}
	}

	temporaryIndex, cleanup, err := r.temporaryIndex(false)
	if err != nil {
		return result, err
	}
	defer cleanup()
	env := []string{"GIT_INDEX_FILE=" + temporaryIndex, "GIT_OPTIONAL_LOCKS=0"}
	if _, err := r.run(ctx, env, nil, "read-tree", acceptedTree); err != nil {
		return result, fmt.Errorf("prepare accepted Git index: %w", err)
	}
	if _, err := r.run(ctx, env, nil, "update-index", "--refresh"); err != nil {
		return blockedGitSync(acceptedCommit, intendedRef, result.PreviousCommit, "visible files changed while Hop was preparing the accepted index", "preserve the new files in a Hop attempt, then run hop sync-git again"), nil
	}
	prepared, err := os.ReadFile(temporaryIndex)
	if err != nil {
		return result, fmt.Errorf("read prepared accepted index: %w", err)
	}
	indexPath := filepath.Join(r.gitDir, "index")
	indexLock := indexPath + ".lock"
	mode := os.FileMode(0o600)
	if info, statErr := os.Stat(indexPath); statErr == nil {
		mode = info.Mode().Perm()
	} else if !errors.Is(statErr, os.ErrNotExist) {
		return result, fmt.Errorf("inspect real Git index mode: %w", statErr)
	}
	lockFile, err := os.OpenFile(indexLock, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
	if errors.Is(err, os.ErrExist) {
		return blockedGitSync(acceptedCommit, intendedRef, result.PreviousCommit, "Git locked the real index after Hop validated it", "wait for the active Git operation to finish, then run hop sync-git again"), nil
	}
	if err != nil {
		return result, fmt.Errorf("lock real Git index: %w", err)
	}
	lockInstalled := true
	defer func() {
		if lockInstalled {
			_ = os.Remove(indexLock)
		}
	}()
	if _, err := lockFile.Write(prepared); err != nil {
		_ = lockFile.Close()
		return result, fmt.Errorf("write accepted Git index lock: %w", err)
	}
	if err := lockFile.Sync(); err != nil {
		_ = lockFile.Close()
		return result, fmt.Errorf("sync accepted Git index lock: %w", err)
	}
	if err := lockFile.Close(); err != nil {
		return result, fmt.Errorf("close accepted Git index lock: %w", err)
	}
	if r.syncGitBeforeFinalValidation != nil {
		r.syncGitBeforeFinalValidation()
	}
	final, _, err := r.inspectGitSync(ctx, intendedRef, acceptedCommit, acceptedTree, true, false)
	if err != nil || final.Status == "blocked" {
		return final, err
	}
	if final.PreviousCommit != result.PreviousCommit {
		return blockedGitSync(acceptedCommit, intendedRef, final.PreviousCommit, "the local branch tip changed during final validation", "retry hop sync-git after the concurrent Git operation finishes"), nil
	}
	if revalidateState != nil {
		if err := revalidateState(); err != nil {
			return blockedGitSync(acceptedCommit, intendedRef, result.PreviousCommit, "Hop's accepted or materialized state changed during final validation", "retry hop sync-git against the new accepted state"), nil
		}
	}
	branchAdvanced := false
	rollbackBranch := func() error {
		if !branchAdvanced {
			return nil
		}
		_, rollbackErr := r.run(ctx, []string{"GIT_OPTIONAL_LOCKS=0"}, nil,
			"update-ref", intendedRef, result.PreviousCommit, acceptedCommit)
		return rollbackErr
	}
	if result.PreviousCommit != acceptedCommit {
		if _, err := r.run(ctx, []string{"GIT_OPTIONAL_LOCKS=0"}, nil,
			"update-ref", intendedRef, acceptedCommit, result.PreviousCommit); err != nil {
			return blockedGitSync(acceptedCommit, intendedRef, result.PreviousCommit, "the branch ref changed before its compare-and-swap could commit", "retry hop sync-git after the concurrent Git operation finishes"), nil
		}
		branchAdvanced = true
	}
	if r.syncGitAfterRefUpdate != nil {
		r.syncGitAfterRefUpdate()
	}
	headLock := filepath.Join(r.gitDir, "HEAD.lock")
	headLockFile, err := os.OpenFile(headLock, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
	if errors.Is(err, os.ErrExist) {
		if rollbackErr := rollbackBranch(); rollbackErr != nil {
			return result, fmt.Errorf("HEAD raced with Git synchronization and branch rollback failed safely: %w", rollbackErr)
		}
		return blockedGitSync(acceptedCommit, intendedRef, result.PreviousCommit, "Git locked HEAD during the branch/index transition", "wait for the active Git operation to finish, then run hop sync-git again"), nil
	}
	if err != nil {
		if rollbackErr := rollbackBranch(); rollbackErr != nil {
			return result, fmt.Errorf("lock Git HEAD and roll back branch: lock=%v rollback=%w", err, rollbackErr)
		}
		return result, fmt.Errorf("lock Git HEAD: %w", err)
	}
	headLockInstalled := true
	defer func() {
		if headLockInstalled {
			_ = os.Remove(headLock)
		}
	}()
	if err := headLockFile.Close(); err != nil {
		_ = os.Remove(headLock)
		headLockInstalled = false
		if rollbackErr := rollbackBranch(); rollbackErr != nil {
			return result, fmt.Errorf("close Git HEAD lock and roll back branch: close=%v rollback=%w", err, rollbackErr)
		}
		return result, fmt.Errorf("close Git HEAD lock: %w", err)
	}
	attachedRef, attached, headErr := r.AttachedHeadRef(ctx)
	headCommit, headExists, commitErr := r.Head(ctx)
	if headErr != nil || commitErr != nil || !attached || attachedRef != intendedRef || !headExists || headCommit != acceptedCommit {
		_ = os.Remove(headLock)
		headLockInstalled = false
		if rollbackErr := rollbackBranch(); rollbackErr != nil {
			return result, fmt.Errorf("HEAD changed during Git synchronization and branch rollback failed safely: %w", rollbackErr)
		}
		return blockedGitSync(acceptedCommit, intendedRef, result.PreviousCommit, "HEAD changed during the branch/index transition", "retry hop sync-git after the concurrent Git operation finishes"), nil
	}
	if err := replaceFileAtomic(indexLock, indexPath); err != nil {
		_ = os.Remove(headLock)
		headLockInstalled = false
		if rollbackErr := rollbackBranch(); rollbackErr != nil {
			return result, fmt.Errorf("atomically install accepted Git index and roll back branch: install=%v rollback=%w", err, rollbackErr)
		}
		return result, fmt.Errorf("atomically install accepted Git index (branch rolled back): %w", err)
	}
	lockInstalled = false
	if err := os.Remove(headLock); err != nil {
		return result, fmt.Errorf("release Git HEAD lock after synchronization: %w", err)
	}
	headLockInstalled = false
	status, err := r.run(ctx, []string{"GIT_OPTIONAL_LOCKS=0"}, nil, "status", "--porcelain=v1")
	if err != nil {
		return result, fmt.Errorf("verify synchronized Git status: %w", err)
	}
	result.Status = "synchronized"
	result.Changed = result.PreviousCommit != acceptedCommit
	if strings.TrimSpace(status) != "" {
		result.Reason = "Git synchronization completed, but new filesystem changes appeared immediately afterward"
		result.SafeNextAction = "preserve the newly reported changes in a Hop attempt; do not reset them"
	}
	return result, nil
}
