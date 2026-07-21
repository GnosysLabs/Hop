package hop

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
)

const provenanceVersion = 1

func digestJSON(value any) (string, error) {
	payload, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(payload)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

func (r *Repository) TreeDelta(ctx context.Context, base, candidate string) ([]TreeDelta, error) {
	changes, err := r.ChangedPathDetails(ctx, base, candidate)
	if err != nil {
		return nil, err
	}
	baseEntries, err := r.TreeEntries(ctx, base)
	if err != nil {
		return nil, err
	}
	candidateEntries, err := r.TreeEntries(ctx, candidate)
	if err != nil {
		return nil, err
	}
	deltas := make([]TreeDelta, 0, len(changes))
	for _, change := range changes {
		oldPath, newPath := change.OldPath, change.NewPath
		switch change.Status[0] {
		case 'D':
			oldPath, newPath = change.NewPath, ""
		case 'A':
			oldPath = ""
		case 'R', 'C':
			// Both paths are already explicit.
		default:
			oldPath = change.NewPath
		}
		delta := TreeDelta{Status: change.Status, OldPath: oldPath, NewPath: newPath}
		if oldPath != "" {
			entry := baseEntries[oldPath]
			delta.OldMode, delta.OldOID = entry.Mode, entry.OID
		}
		if newPath != "" {
			entry := candidateEntries[newPath]
			delta.NewMode, delta.NewOID = entry.Mode, entry.OID
		}
		deltas = append(deltas, delta)
	}
	sort.Slice(deltas, func(i, j int) bool {
		if deltas[i].OldPath != deltas[j].OldPath {
			return deltas[i].OldPath < deltas[j].OldPath
		}
		if deltas[i].NewPath != deltas[j].NewPath {
			return deltas[i].NewPath < deltas[j].NewPath
		}
		return deltas[i].Status < deltas[j].Status
	})
	return deltas, nil
}

func (s *Service) buildProvenance(
	ctx context.Context,
	operation string,
	base State,
	candidateTree string,
	inputs []ProvenanceInput,
) (*StateProvenance, error) {
	manifest, err := s.Repo.TreeDelta(ctx, base.SourceTree, candidateTree)
	if err != nil {
		return nil, err
	}
	manifestDigest, err := digestJSON(manifest)
	if err != nil {
		return nil, err
	}
	proof := &StateProvenance{
		Version:        provenanceVersion,
		Operation:      operation,
		BaseStateID:    base.ID,
		BaseTree:       base.SourceTree,
		CandidateTree:  candidateTree,
		Inputs:         append([]ProvenanceInput(nil), inputs...),
		Manifest:       manifest,
		ManifestDigest: manifestDigest,
	}
	proof.CompositionDigest, err = compositionDigest(proof)
	if err != nil {
		return nil, err
	}
	return proof, nil
}

func compositionDigest(proof *StateProvenance) (string, error) {
	copyProof := *proof
	copyProof.CompositionDigest = ""
	return digestJSON(copyProof)
}

func provenancePaths(manifest []TreeDelta) map[string]struct{} {
	paths := make(map[string]struct{}, len(manifest)*2)
	for _, delta := range manifest {
		if delta.OldPath != "" {
			paths[delta.OldPath] = struct{}{}
		}
		if delta.NewPath != "" {
			paths[delta.NewPath] = struct{}{}
		}
	}
	return paths
}

// verifyAcceptance independently recomputes the candidate manifest and proves
// that every changed, deleted, renamed, or mode-changed path is authorized by
// at least one immutable input. Paths outside that set must therefore retain
// the canonical parent's exact object ID and mode.
func (s *Service) verifyAcceptance(ctx context.Context, current State, candidateTree string, inputs []ProvenanceInput, operation string) (*StateProvenance, error) {
	allowed := make(map[string]struct{})
	for _, input := range inputs {
		manifest, err := s.Repo.TreeDelta(ctx, input.BaseTree, input.CandidateTree)
		if err != nil {
			return nil, fmt.Errorf("verify %s input %s: %w", operation, input.Role, err)
		}
		for path := range provenancePaths(manifest) {
			allowed[path] = struct{}{}
		}
	}
	proof, err := s.buildProvenance(ctx, operation, current, candidateTree, inputs)
	if err != nil {
		return nil, err
	}
	var unauthorized []string
	for path := range provenancePaths(proof.Manifest) {
		if _, ok := allowed[path]; !ok {
			unauthorized = append(unauthorized, path)
		}
	}
	if len(unauthorized) > 0 {
		sort.Strings(unauthorized)
		return nil, &ProvenanceError{Operation: operation, Paths: unauthorized, Reason: "candidate changes paths that are absent from every authorized immutable input"}
	}
	return proof, nil
}

func validateStoredProvenance(state State, base State) error {
	proof := state.Provenance
	if proof == nil {
		return &ProvenanceError{Operation: string(state.Kind), Reason: "state has no durable authorization proof"}
	}
	if proof.Version != provenanceVersion || proof.BaseStateID != base.ID || proof.BaseTree != base.SourceTree || proof.CandidateTree != state.SourceTree {
		return &ProvenanceError{Operation: proof.Operation, Reason: "authorization proof does not bind the expected base state and candidate tree"}
	}
	manifest := proof.Manifest
	if manifest == nil {
		if proof.BaseTree != proof.CandidateTree {
			return &ProvenanceError{Operation: proof.Operation, Reason: "authorization manifest is missing for a changed candidate tree"}
		}
		// Empty manifests were historically built as a non-nil empty slice and
		// hashed as JSON `[]`. The json `omitempty` tag then omitted that slice
		// from SQLite, so decoding produced nil (`null`) and caused a false
		// digest mismatch. Equal immutable tree IDs independently prove that the
		// only valid manifest is empty, making this canonicalization lossless.
		manifest = []TreeDelta{}
	}
	manifestDigest, err := digestJSON(manifest)
	if err != nil || manifestDigest != proof.ManifestDigest {
		return &ProvenanceError{Operation: proof.Operation, Reason: "authorization manifest digest mismatch"}
	}
	composition, err := compositionDigest(proof)
	if err != nil || composition != proof.CompositionDigest {
		return &ProvenanceError{Operation: proof.Operation, Reason: "composition digest mismatch"}
	}
	return nil
}

func (s *Service) verifyStoredProvenance(ctx context.Context, state State, base State) error {
	if err := validateStoredProvenance(state, base); err != nil {
		return err
	}
	manifest, err := s.Repo.TreeDelta(ctx, base.SourceTree, state.SourceTree)
	if err != nil {
		return err
	}
	digest, err := digestJSON(manifest)
	if err != nil {
		return err
	}
	if digest != state.Provenance.ManifestDigest {
		return &ProvenanceError{Operation: state.Provenance.Operation, Reason: "stored manifest does not match the immutable Git trees"}
	}
	return nil
}
