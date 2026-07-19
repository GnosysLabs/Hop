package hop

import (
	"context"
	"fmt"
)

func (s *Service) bindProvenanceBranch(ctx context.Context, proof *StateProvenance) error {
	if proof == nil {
		return nil
	}
	ref, attached, err := s.Repo.AttachedHeadRef(ctx)
	if err != nil {
		return err
	}
	if attached {
		proof.BranchRef = ref
	}
	proof.CompositionDigest, err = compositionDigest(proof)
	return err
}

func (s *Service) intendedBranchForAccepted(ctx context.Context, accepted State) (string, error) {
	if accepted.Provenance != nil && accepted.Provenance.BranchRef != "" {
		return accepted.Provenance.BranchRef, nil
	}
	attachedRef, attached, err := s.Repo.AttachedHeadRef(ctx)
	if err != nil {
		return "", err
	}
	if !attached {
		return "", nil
	}
	if accepted.CanonicalAnchorID == "" {
		return attachedRef, nil
	}
	publication, found, err := s.Store.PublicationForState(ctx, accepted.ID)
	if err != nil {
		return "", err
	}
	// v1.1.2 did not persist the local branch ref in state provenance. Its
	// durable publication destination is a safe migration bridge only when it
	// exactly matches the currently attached local branch.
	if found && publication.Ref != "" && publication.Ref == attachedRef {
		return attachedRef, nil
	}
	return "", nil
}

func (s *Service) syncGitLocked(ctx context.Context, accepted State) (GitSyncResult, error) {
	if accepted.CanonicalAnchorID == "" {
		return GitSyncResult{
			Status:         "not_applicable",
			AcceptedCommit: accepted.GitCommit,
			Reason:         "the initial Hop state preserves the repository's existing Git state; there is no landed projection to synchronize",
		}, nil
	}
	blocked := func(reason, action string) GitSyncResult {
		return blockedGitSync(accepted.GitCommit, "", "", reason, action)
	}
	current, err := s.Store.AcceptedHead(ctx)
	if err != nil {
		return GitSyncResult{}, err
	}
	if current.ID != accepted.ID || current.GitCommit != accepted.GitCommit {
		return blocked("Hop's accepted state changed before Git synchronization", "retry hop sync-git against the new accepted state"), nil
	}
	materialized, err := s.Store.MaterializedHead(ctx)
	if err != nil {
		return GitSyncResult{}, err
	}
	if materialized.ID != accepted.ID {
		return blocked("the visible root is not durably recorded at the accepted state", "run hop sync to safely materialize the accepted tree, then run hop sync-git again"), nil
	}
	if accepted.Provenance != nil {
		base, baseErr := s.Store.GetState(ctx, accepted.Provenance.BaseStateID)
		if baseErr != nil {
			return blocked("the accepted state's provenance base cannot be loaded", "run hop doctor and repair state integrity before retrying"), nil
		}
		if proofErr := s.verifyStoredProvenance(ctx, accepted, base); proofErr != nil {
			return blocked("the accepted state's authorization proof is invalid: "+proofErr.Error(), "run hop doctor and repair state integrity before retrying"), nil
		}
	} else if accepted.CanonicalAnchorID != "" {
		return blocked("the accepted state predates durable authorization manifests", "create and land a fresh proposal from this accepted tree before synchronizing the local branch"), nil
	}
	intendedRef, err := s.intendedBranchForAccepted(ctx, accepted)
	if err != nil {
		return GitSyncResult{}, err
	}
	return s.Repo.SyncBranchIndex(ctx, intendedRef, accepted.GitCommit, accepted.SourceTree, func() error {
		head, err := s.Store.AcceptedHead(ctx)
		if err != nil {
			return err
		}
		if head.ID != accepted.ID || head.GitCommit != accepted.GitCommit {
			return fmt.Errorf("accepted head changed")
		}
		root, err := s.Store.MaterializedHead(ctx)
		if err != nil {
			return err
		}
		if root.ID != accepted.ID {
			return fmt.Errorf("materialized head changed")
		}
		return nil
	})
}

func (s *Service) SyncGit(ctx context.Context) (GitSyncResult, error) {
	release, err := acquireProjectLock(ctx, s.Root, "accept")
	if err != nil {
		return GitSyncResult{}, err
	}
	defer release()
	accepted, err := s.Store.AcceptedHead(ctx)
	if err != nil {
		return GitSyncResult{}, err
	}
	return s.syncGitLocked(ctx, accepted)
}

func (s *Service) trySyncGit(ctx context.Context) *GitSyncResult {
	result, err := s.SyncGit(ctx)
	if err != nil {
		result = blockedGitSync("", "", "", "automatic Git synchronization failed: "+err.Error(), "run hop sync-git for a fresh diagnosis; no visible files were overwritten")
	}
	if result.Status == "not_applicable" {
		return nil
	}
	return &result
}

func gitSyncExplanation(result GitSyncResult) string {
	if result.Status != "blocked" {
		return ""
	}
	message := "Git branch/index synchronization was blocked: " + result.Reason
	if result.SafeNextAction != "" {
		message += ". Safe next action: " + result.SafeNextAction
	}
	return message
}
