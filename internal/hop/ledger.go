package hop

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// PortablePromptLedger is the versioned, repository-safe subset of Hop's
// local state. It deliberately excludes SQLite, workspace paths, check output,
// and every other machine-local runtime detail.
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

// ExportPromptLedger writes immutable prompt files beneath
// .hop/records/prompts. Passing attemptIDs restricts the export to those
// attempts, which keeps concurrent proposals from writing the same files.
func (s *Service) ExportPromptLedger(ctx context.Context, destinationRoot string, attemptIDs ...string) (PortablePromptLedger, error) {
	if destinationRoot == "" {
		destinationRoot = s.Root
	}
	graph, err := s.Store.Graph(ctx, "")
	if err != nil {
		return PortablePromptLedger{}, err
	}
	ledger := PortablePromptLedger{
		SchemaVersion: 1,
		GeneratedAt:   time.Now().UTC(),
		Prompts:       make([]PortablePromptRecord, 0),
	}
	wantedAttempts := make(map[string]struct{}, len(attemptIDs))
	for _, attemptID := range attemptIDs {
		wantedAttempts[attemptID] = struct{}{}
	}
	for _, row := range graph {
		state := row.State
		if state.Kind != StatePrompt || state.Prompt == "" {
			continue
		}
		if len(wantedAttempts) > 0 {
			if _, wanted := wantedAttempts[state.AttemptID]; !wanted {
				continue
			}
		}
		record := PortablePromptRecord{
			ID:        state.ID,
			TaskID:    state.TaskID,
			AttemptID: state.AttemptID,
			StateID:   state.ID,
			Prompt:    state.Prompt,
			AgentName: state.Agent,
			Status:    "unknown",
			CreatedAt: state.CreatedAt,
			Metadata: PortablePromptMetadata{
				SourceTree: state.SourceTree,
				GitCommit:  state.GitCommit,
			},
		}
		if state.AttemptID != "" {
			attempt, err := s.Store.GetAttempt(ctx, state.AttemptID)
			if err != nil {
				return PortablePromptLedger{}, fmt.Errorf("read attempt for prompt %s: %w", state.ID, err)
			}
			record.Status = attempt.Status
			record.Metadata.AttemptHead = attempt.HeadStateID
			if attempt.HeadStateID != "" {
				head, err := s.Store.GetState(ctx, attempt.HeadStateID)
				if err != nil {
					return PortablePromptLedger{}, fmt.Errorf("read attempt head for prompt %s: %w", state.ID, err)
				}
				record.Metadata.AttemptHeadKind = string(head.Kind)
				record.ResponseSummary = head.Summary
				if head.Kind == StateAccepted || head.Kind == StateFailed || head.Kind == StateCancelled {
					completedAt := head.CreatedAt
					record.CompletedAt = &completedAt
				}
			}
		}
		ledger.Prompts = append(ledger.Prompts, record)
	}

	outputDir := filepath.Join(destinationRoot, ".hop", "records", "prompts")
	if err := os.MkdirAll(outputDir, 0o755); err != nil {
		return PortablePromptLedger{}, fmt.Errorf("create prompt ledger directory: %w", err)
	}
	for _, record := range ledger.Prompts {
		outputPath := filepath.Join(outputDir, record.ID+".json")
		if _, err := os.Stat(outputPath); err == nil {
			continue // Prompt records are immutable once published.
		} else if !os.IsNotExist(err) {
			return PortablePromptLedger{}, fmt.Errorf("inspect prompt record %s: %w", record.ID, err)
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
			return PortablePromptLedger{}, fmt.Errorf("publish prompt record %s: %w", record.ID, err)
		}
	}
	return ledger, nil
}
