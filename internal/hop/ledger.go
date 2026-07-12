package hop

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// PortablePromptLedger is an explicit local export of prompt state. It is kept
// beneath the ignored .hop directory because prompts and metadata can be private.
type PortablePromptLedger struct {
	SchemaVersion int                    `json:"schema_version"`
	GeneratedAt   time.Time              `json:"generated_at"`
	Prompts       []PortablePromptRecord `json:"prompts"`
}

type PortablePromptRecord struct {
	ID              string                 `json:"id"`
	TaskID          string                 `json:"task_id,omitempty"`
	AttemptID       string                 `json:"attempt_id,omitempty"`
	StateID         string                 `json:"state_id"`
	Prompt          string                 `json:"prompt"`
	AgentName       string                 `json:"agent_name,omitempty"`
	Status          string                 `json:"status"`
	ResponseSummary string                 `json:"response_summary,omitempty"`
	CreatedAt       time.Time              `json:"created_at"`
	CompletedAt     *time.Time             `json:"completed_at,omitempty"`
	Metadata        PortablePromptMetadata `json:"metadata"`
}

type PortablePromptMetadata struct {
	SourceTree      string `json:"source_tree"`
	GitCommit       string `json:"git_commit"`
	AttemptHead     string `json:"attempt_head,omitempty"`
	AttemptHeadKind string `json:"attempt_head_kind,omitempty"`
}

type promptExportOptions struct {
	AttemptIDs      []string
	PublishableOnly bool
	Status          string
	ResponseSummary string
	Overwrite       bool
}

type suppressedPromptManifest struct {
	PromptIDs []string `json:"prompt_ids"`
}

// ExportPromptLedger writes local prompt files beneath .hop/records/prompts.
// Passing attemptIDs restricts the export to those attempts.
func (s *Service) ExportPromptLedger(ctx context.Context, destinationRoot string, attemptIDs ...string) (PortablePromptLedger, error) {
	return s.exportPromptLedger(ctx, destinationRoot, promptExportOptions{AttemptIDs: attemptIDs, PublishableOnly: true})
}

func (s *Service) exportPromptLedger(ctx context.Context, destinationRoot string, options promptExportOptions) (PortablePromptLedger, error) {
	if destinationRoot == "" {
		destinationRoot = s.Root
	}
	graph, err := s.Store.Graph(ctx, "")
	if err != nil {
		return PortablePromptLedger{}, err
	}
	suppressed, err := loadSuppressedPromptIDs(destinationRoot)
	if err != nil {
		return PortablePromptLedger{}, err
	}
	ledger := PortablePromptLedger{
		SchemaVersion: 1,
		GeneratedAt:   time.Now().UTC(),
		Prompts:       make([]PortablePromptRecord, 0),
	}
	wantedAttempts := make(map[string]struct{}, len(options.AttemptIDs))
	for _, attemptID := range options.AttemptIDs {
		wantedAttempts[attemptID] = struct{}{}
	}
	lastPromptByAttempt := make(map[string]string)
	excludedPromptIDs := make(map[string]struct{})
	for _, row := range graph {
		if row.State.Kind == StatePrompt && row.State.Prompt != "" && row.State.AttemptID != "" {
			lastPromptByAttempt[row.State.AttemptID] = row.State.ID
		}
	}
	for _, row := range graph {
		state := row.State
		if state.Kind != StatePrompt || state.Prompt == "" {
			continue
		}
		if _, hidden := suppressed[state.ID]; hidden {
			excludedPromptIDs[state.ID] = struct{}{}
			continue
		}
		if _, reconciliation := decodeReconciliationConflicts(state.Summary); reconciliation || strings.HasPrefix(state.Prompt, "Resolve proposal ") {
			excludedPromptIDs[state.ID] = struct{}{}
			continue
		}
		if len(wantedAttempts) > 0 {
			if _, wanted := wantedAttempts[state.AttemptID]; !wanted {
				continue
			}
		}
		if state.AttemptID == "" {
			continue
		}
		attempt, err := s.Store.GetAttempt(ctx, state.AttemptID)
		if err != nil {
			return PortablePromptLedger{}, fmt.Errorf("read attempt for prompt %s: %w", state.ID, err)
		}
		var head State
		if attempt.HeadStateID != "" {
			head, err = s.Store.GetState(ctx, attempt.HeadStateID)
			if err != nil {
				return PortablePromptLedger{}, fmt.Errorf("read attempt head for prompt %s: %w", state.ID, err)
			}
		}
		if options.PublishableOnly && head.Kind != StateProposal && head.Kind != StateAccepted {
			continue
		}

		record := PortablePromptRecord{
			ID:        state.ID,
			TaskID:    state.TaskID,
			AttemptID: state.AttemptID,
			StateID:   state.ID,
			Prompt:    state.Prompt,
			AgentName: state.Agent,
			Status:    attempt.Status,
			CreatedAt: state.CreatedAt,
			Metadata: PortablePromptMetadata{
				SourceTree: state.SourceTree,
				GitCommit:  state.GitCommit,
			},
		}
		record.Metadata.AttemptHead = attempt.HeadStateID
		record.Metadata.AttemptHeadKind = string(head.Kind)
		if lastPromptByAttempt[state.AttemptID] == state.ID {
			record.ResponseSummary = head.Summary
			if options.ResponseSummary != "" {
				record.ResponseSummary = options.ResponseSummary
			}
		}
		if options.Status != "" {
			record.Status = options.Status
		}
		if head.Kind == StateAccepted || head.Kind == StateFailed || head.Kind == StateCancelled {
			completedAt := head.CreatedAt
			record.CompletedAt = &completedAt
		}
		ledger.Prompts = append(ledger.Prompts, record)
	}

	outputDir := filepath.Join(destinationRoot, ".hop", "records", "prompts")
	if err := os.MkdirAll(outputDir, 0o755); err != nil {
		return PortablePromptLedger{}, fmt.Errorf("create prompt ledger directory: %w", err)
	}
	for promptID := range excludedPromptIDs {
		outputPath := filepath.Join(outputDir, promptID+".json")
		if err := os.Remove(outputPath); err != nil && !os.IsNotExist(err) {
			return PortablePromptLedger{}, fmt.Errorf("remove excluded prompt record %s: %w", promptID, err)
		}
	}
	for _, record := range ledger.Prompts {
		outputPath := filepath.Join(outputDir, record.ID+".json")
		_, statErr := os.Stat(outputPath)
		if statErr == nil && !options.Overwrite {
			continue // Prompt records are immutable once published.
		}
		if statErr != nil && !os.IsNotExist(statErr) {
			return PortablePromptLedger{}, fmt.Errorf("inspect prompt record %s: %w", record.ID, statErr)
		}
		contents, err := json.MarshalIndent(record, "", "  ")
		if err != nil {
			return PortablePromptLedger{}, fmt.Errorf("encode prompt record %s: %w", record.ID, err)
		}
		temporary, err := os.CreateTemp(outputDir, ".prompt-*.tmp")
		if err != nil {
			return PortablePromptLedger{}, fmt.Errorf("create prompt record %s: %w", record.ID, err)
		}
		temporaryPath := temporary.Name()
		if _, err := temporary.Write(append(contents, '\n')); err != nil {
			_ = temporary.Close()
			_ = os.Remove(temporaryPath)
			return PortablePromptLedger{}, fmt.Errorf("write prompt record %s: %w", record.ID, err)
		}
		if err := temporary.Chmod(0o644); err != nil {
			_ = temporary.Close()
			_ = os.Remove(temporaryPath)
			return PortablePromptLedger{}, fmt.Errorf("set prompt record permissions for %s: %w", record.ID, err)
		}
		if err := temporary.Close(); err != nil {
			_ = os.Remove(temporaryPath)
			return PortablePromptLedger{}, fmt.Errorf("close prompt record %s: %w", record.ID, err)
		}
		if err := os.Rename(temporaryPath, outputPath); err != nil {
			_ = os.Remove(temporaryPath)
			return PortablePromptLedger{}, fmt.Errorf("write prompt record %s: %w", record.ID, err)
		}
	}
	return ledger, nil
}

func loadSuppressedPromptIDs(destinationRoot string) (map[string]struct{}, error) {
	path := filepath.Join(destinationRoot, ".hop", "records", "suppressed.json")
	contents, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return map[string]struct{}{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read suppressed prompt manifest: %w", err)
	}
	var manifest suppressedPromptManifest
	if err := json.Unmarshal(contents, &manifest); err != nil {
		return nil, fmt.Errorf("decode suppressed prompt manifest: %w", err)
	}
	result := make(map[string]struct{}, len(manifest.PromptIDs))
	for _, id := range manifest.PromptIDs {
		if !strings.HasPrefix(id, "P_") {
			return nil, fmt.Errorf("invalid suppressed prompt ID %q", id)
		}
		result[id] = struct{}{}
	}
	return result, nil
}
