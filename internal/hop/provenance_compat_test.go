package hop

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func TestStoredProvenanceAcceptsOmittedEmptyManifestAfterJSONRoundTrip(t *testing.T) {
	ctx := context.Background()
	service, base := newTestProject(t, map[string]string{"base.txt": "base\n"})
	proof, err := service.buildProvenance(ctx, "checkpoint", base, base.SourceTree, nil)
	if err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal(proof)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(payload), `"manifest"`) {
		t.Fatalf("fixture must reproduce omitted empty manifest: %s", payload)
	}
	var reloaded StateProvenance
	if err := json.Unmarshal(payload, &reloaded); err != nil {
		t.Fatal(err)
	}
	if reloaded.Manifest != nil {
		t.Fatalf("reloaded manifest = %#v, want nil omitted field", reloaded.Manifest)
	}
	state := State{Kind: StateCheckpoint, SourceTree: base.SourceTree, Provenance: &reloaded}
	if err := validateStoredProvenance(state, base); err != nil {
		t.Fatalf("validate JSON-round-tripped empty manifest: %v", err)
	}
	if err := service.verifyStoredProvenance(ctx, state, base); err != nil {
		t.Fatalf("verify JSON-round-tripped empty manifest: %v", err)
	}
}

func TestStoredProvenanceRejectsMissingManifestForChangedTree(t *testing.T) {
	service, base := newTestProject(t, map[string]string{"base.txt": "base\n"})
	changed := base
	changed.SourceTree = strings.Repeat("a", len(base.SourceTree))
	emptyDigest, err := digestJSON([]TreeDelta{})
	if err != nil {
		t.Fatal(err)
	}
	changed.Provenance = &StateProvenance{
		Version: provenanceVersion, Operation: "checkpoint",
		BaseStateID: base.ID, BaseTree: base.SourceTree, CandidateTree: changed.SourceTree,
		ManifestDigest: emptyDigest,
	}
	err = validateStoredProvenance(changed, base)
	if err == nil || !strings.Contains(err.Error(), "manifest is missing") {
		t.Fatalf("validate changed tree without manifest = %v, want missing-manifest error", err)
	}
	_ = service
}
