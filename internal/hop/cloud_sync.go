package hop

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const (
	maxPromptSyncBatchRecords = 50
	maxPromptSyncBatchBytes   = 100 << 20
	maxPromptSyncRawPrompt    = 16 << 20
)

type promptSyncRequest struct {
	SchemaVersion int                    `json:"schema_version"`
	Repository    PromptRepository       `json:"repository"`
	Prompts       []PortablePromptRecord `json:"prompts"`
	Tombstones    []string               `json:"tombstone_state_ids,omitempty"`
}

type promptSyncResponse struct {
	Repository PromptRepository         `json:"repository"`
	Synced     int                      `json:"synced"`
	Deleted    int                      `json:"deleted,omitempty"`
	Results    []promptSyncRecordResult `json:"results,omitempty"`
}

type promptSyncRecordResult struct {
	StateID  string `json:"state_id"`
	Status   string `json:"status"`
	Revision string `json:"revision,omitempty"`
	Error    string `json:"error,omitempty"`
}

type remotePromptRepository struct {
	Host       string
	Repository PromptRepository
}

// SyncPromptHistory uploads pending revisions and tombstones. Content-free
// SQLite receipts make partial progress durable without duplicating prompts.
func (s *Service) SyncPromptHistory(ctx context.Context) (*PromptSyncResult, error) {
	return s.syncPromptHistory(ctx, NewAuthClient())
}

func (s *Service) syncPromptHistory(ctx context.Context, auth *AuthClient) (*PromptSyncResult, error) {
	release, err := acquireProjectLock(ctx, s.Root, "prompt-sync")
	if err != nil {
		return nil, err
	}
	defer release()
	remote, configured, err := s.promptRemoteRepository(ctx)
	if err != nil {
		return nil, err
	}
	if !configured {
		return nil, nil
	}
	profile, err := auth.readProfile()
	if err != nil {
		if errors.Is(err, ErrNotAuthenticated) {
			return nil, ErrNotAuthenticated
		}
		return nil, err
	}
	serverURL, err := url.Parse(profile.Server)
	if err != nil {
		return nil, fmt.Errorf("parse signed-in Hop forge: %w", err)
	}
	if !strings.EqualFold(serverURL.Hostname(), remote.Host) {
		// A login for one forge must never receive prompts belonging to a
		// different remote. This is a normal skip for GitHub or another Gitea.
		return nil, nil
	}
	config, err := auth.Discover(ctx, profile.Server)
	if err != nil {
		return nil, err
	}
	credential, err := auth.loadCredential(profile.Server)
	if err != nil {
		return nil, err
	}
	ledger, _, err := s.buildPromptLedger(ctx, s.Root, promptExportOptions{PublishableOnly: false})
	if err != nil {
		return nil, err
	}
	receipts, err := s.Store.PromptSyncReceipts(ctx, profile.Server, remote.Repository.Owner, remote.Repository.Name)
	if err != nil {
		return nil, err
	}
	prompts := make([]PortablePromptRecord, 0, len(ledger.Prompts))
	var rejected []string
	for _, source := range ledger.Prompts {
		record := redactPortablePromptRecord(source)
		if len(record.Prompt) > maxPromptSyncRawPrompt {
			rejected = append(rejected, record.StateID+": prompt exceeds 16 MiB")
			continue
		}
		record.Revision, err = portablePromptRevision(record)
		if err != nil {
			rejected = append(rejected, record.StateID+": "+err.Error())
			continue
		}
		if receipt, ok := receipts[record.StateID]; ok && !receipt.Deleted && receipt.Revision == record.Revision {
			continue
		}
		prompts = append(prompts, record)
	}
	sort.SliceStable(prompts, func(i, j int) bool { return prompts[i].SourceUpdatedAt.After(prompts[j].SourceUpdatedAt) })
	suppressed, err := loadSuppressedPromptIDs(s.Root)
	if err != nil {
		return nil, err
	}
	var tombstones []string
	for id := range suppressed {
		if receipt, ok := receipts[id]; ok && !receipt.Deleted {
			tombstones = append(tombstones, id)
		}
	}
	sort.Strings(tombstones)
	batches, err := promptSyncBatches(remote.Repository, prompts)
	if err != nil {
		return nil, err
	}
	for start := 0; start < len(tombstones); start += maxPromptSyncBatchRecords {
		end := start + maxPromptSyncBatchRecords
		if end > len(tombstones) {
			end = len(tombstones)
		}
		payload, marshalErr := json.Marshal(promptSyncRequest{SchemaVersion: 1, Repository: remote.Repository, Tombstones: tombstones[start:end]})
		if marshalErr != nil {
			return nil, marshalErr
		}
		batches = append(batches, payload)
	}
	synced := 0
	deleted := 0
	for _, payload := range batches {
		body, requestErr := auth.hopRequest(ctx, credential, http.MethodPost, config.SyncEndpoint, payload)
		if requestErr != nil {
			return nil, fmt.Errorf("sync private prompt history: %w", requestErr)
		}
		var response promptSyncResponse
		if err := json.Unmarshal(body, &response); err != nil {
			return nil, fmt.Errorf("decode prompt sync response: %w", err)
		}
		var batch promptSyncRequest
		if err := json.Unmarshal(payload, &batch); err != nil {
			return nil, fmt.Errorf("decode local prompt sync batch: %w", err)
		}
		if response.Synced < 0 || response.Synced > len(batch.Prompts) || response.Deleted < 0 || response.Deleted > len(batch.Tombstones) {
			return nil, fmt.Errorf("prompt sync returned invalid count %d", response.Synced)
		}
		if response.Repository.Owner != "" && (!strings.EqualFold(response.Repository.Owner, remote.Repository.Owner) || !strings.EqualFold(response.Repository.Name, remote.Repository.Name)) {
			return nil, errors.New("prompt sync response identified a different repository")
		}
		now := time.Now().UTC()
		acks := make([]PromptSyncReceipt, 0, len(batch.Prompts)+len(batch.Tombstones))
		if len(response.Results) == 0 {
			if response.Synced != len(batch.Prompts) || response.Deleted != len(batch.Tombstones) {
				return nil, errors.New("prompt sync returned a partial result without per-record outcomes")
			}
			for _, record := range batch.Prompts {
				acks = append(acks, PromptSyncReceipt{StateID: record.StateID, Revision: record.Revision, SyncedAt: now})
			}
			for _, id := range batch.Tombstones {
				acks = append(acks, PromptSyncReceipt{StateID: id, Revision: receipts[id].Revision, Deleted: true, SyncedAt: now})
			}
		} else {
			byID := make(map[string]PortablePromptRecord, len(batch.Prompts))
			for _, record := range batch.Prompts {
				byID[record.StateID] = record
			}
			resultSynced, resultDeleted := 0, 0
			for _, outcome := range response.Results {
				switch outcome.Status {
				case "synced", "upserted":
					if record, ok := byID[outcome.StateID]; ok {
						acks = append(acks, PromptSyncReceipt{StateID: record.StateID, Revision: record.Revision, SyncedAt: now})
						resultSynced++
					}
				case "deleted":
					acks = append(acks, PromptSyncReceipt{StateID: outcome.StateID, Revision: receipts[outcome.StateID].Revision, Deleted: true, SyncedAt: now})
					resultDeleted++
				case "rejected", "failed":
					rejected = append(rejected, outcome.StateID+": "+outcome.Error)
				}
			}
			response.Synced, response.Deleted = resultSynced, resultDeleted
		}
		if err := s.Store.RecordPromptSyncReceipts(ctx, profile.Server, remote.Repository.Owner, remote.Repository.Name, acks); err != nil {
			return nil, err
		}
		synced += response.Synced
		deleted += response.Deleted
	}
	if len(rejected) > 0 {
		return &PromptSyncResult{Server: profile.Server, Repository: remote.Repository, Synced: synced, Deleted: deleted}, fmt.Errorf("some prompt records could not sync: %s", strings.Join(rejected, "; "))
	}
	return &PromptSyncResult{
		Server:     profile.Server,
		Repository: remote.Repository,
		Synced:     synced,
		Deleted:    deleted,
	}, nil
}

func portablePromptRevision(record PortablePromptRecord) (string, error) {
	record.Revision = ""
	payload, err := json.Marshal(record)
	if err != nil {
		return "", fmt.Errorf("encode prompt revision: %w", err)
	}
	digest := sha256.Sum256(payload)
	return "sha256:" + hex.EncodeToString(digest[:]), nil
}

func promptSyncBatches(repository PromptRepository, prompts []PortablePromptRecord) ([][]byte, error) {
	if len(prompts) == 0 {
		return nil, nil
	}
	batches := make([][]byte, 0, (len(prompts)+maxPromptSyncBatchRecords-1)/maxPromptSyncBatchRecords)
	for start := 0; start < len(prompts); {
		end := start
		var selected []byte
		for end < len(prompts) && end-start < maxPromptSyncBatchRecords {
			candidate, err := json.Marshal(promptSyncRequest{
				SchemaVersion: 1,
				Repository:    repository,
				Prompts:       prompts[start : end+1],
			})
			if err != nil {
				return nil, fmt.Errorf("encode prompt sync request: %w", err)
			}
			if len(candidate) > maxPromptSyncBatchBytes {
				if end == start {
					return nil, fmt.Errorf("prompt %s is too large to sync safely", prompts[start].StateID)
				}
				break
			}
			selected = candidate
			end++
		}
		if len(selected) == 0 {
			return nil, errors.New("could not construct a prompt sync batch")
		}
		batches = append(batches, selected)
		start = end
	}
	return batches, nil
}

func redactPortablePromptRecord(record PortablePromptRecord) PortablePromptRecord {
	record.Prompt, _ = RedactPromptSecrets(record.Prompt)
	record.AgentName, _ = RedactPromptSecrets(record.AgentName)
	record.ResponseSummary, _ = RedactPromptSecrets(record.ResponseSummary)
	record.Metadata.SourceTree, _ = RedactPromptSecrets(record.Metadata.SourceTree)
	record.Metadata.GitCommit, _ = RedactPromptSecrets(record.Metadata.GitCommit)
	record.Metadata.AttemptHead, _ = RedactPromptSecrets(record.Metadata.AttemptHead)
	record.Metadata.AttemptHeadKind, _ = RedactPromptSecrets(record.Metadata.AttemptHeadKind)
	return record
}

func (s *Service) attachPromptCloudSync(_ context.Context, _ **PromptSyncResult, _ *[]string) {
	s.schedulePromptCloudSync()
}

func (s *Service) schedulePromptCloudSync() {
	if os.Getenv("HOP_SYNC_WORKER") == "1" {
		return
	}
	executable, err := os.Executable()
	base := filepath.Base(executable)
	if err != nil || strings.HasSuffix(base, ".test") || strings.HasSuffix(base, ".test.exe") {
		return
	}
	command := exec.Command(executable, "sync-prompts-worker")
	command.Dir = s.Root
	command.Env = append(os.Environ(), "HOP_ROOT="+s.Root, "HOP_SYNC_WORKER=1")
	if command.Start() == nil {
		_ = command.Process.Release()
	}
}

func (s *Service) promptRemoteRepository(ctx context.Context) (remotePromptRepository, bool, error) {
	remoteName := ""
	if destination, configured, err := s.Repo.pushDestination(ctx); err != nil {
		return remotePromptRepository{}, false, err
	} else if configured {
		remoteName = destination.remote
	}
	if remoteName == "" {
		output, err := s.Repo.run(ctx, nil, nil, "remote")
		if err != nil {
			return remotePromptRepository{}, false, fmt.Errorf("list Git remotes for prompt sync: %w", err)
		}
		remotes := nonemptyLines(output)
		switch {
		case containsString(remotes, "origin"):
			remoteName = "origin"
		case len(remotes) == 1:
			remoteName = remotes[0]
		default:
			return remotePromptRepository{}, false, nil
		}
	}
	remoteURL, err := s.Repo.run(ctx, nil, nil, "remote", "get-url", "--push", remoteName)
	if err != nil {
		return remotePromptRepository{}, false, fmt.Errorf("read Git remote for prompt sync: %w", err)
	}
	parsed, err := parsePromptRemote(strings.TrimSpace(remoteURL))
	if err != nil {
		// Local paths and unsupported transports are valid Git remotes, but do
		// not identify a forge that can receive private prompt history.
		return remotePromptRepository{}, false, nil
	}
	return parsed, true, nil
}

func parsePromptRemote(raw string) (remotePromptRepository, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return remotePromptRepository{}, errors.New("empty Git remote")
	}
	var host, repositoryPath string
	if !strings.Contains(raw, "://") {
		colon := strings.IndexByte(raw, ':')
		if colon <= 0 || strings.Contains(raw[:colon], "/") {
			return remotePromptRepository{}, errors.New("Git remote is not a forge URL")
		}
		hostPart := raw[:colon]
		if at := strings.LastIndexByte(hostPart, '@'); at >= 0 {
			hostPart = hostPart[at+1:]
		}
		host = strings.ToLower(strings.TrimSpace(hostPart))
		repositoryPath = raw[colon+1:]
	} else {
		parsed, err := url.Parse(raw)
		if err != nil {
			return remotePromptRepository{}, err
		}
		switch parsed.Scheme {
		case "https", "http", "ssh", "git":
		default:
			return remotePromptRepository{}, errors.New("unsupported Git remote scheme")
		}
		host = strings.ToLower(parsed.Hostname())
		repositoryPath = parsed.Path
	}
	if host == "" {
		return remotePromptRepository{}, errors.New("Git remote has no host")
	}
	repositoryPath = strings.Trim(repositoryPath, "/")
	repositoryPath = strings.TrimSuffix(repositoryPath, ".git")
	segments := strings.Split(repositoryPath, "/")
	if len(segments) < 2 {
		return remotePromptRepository{}, errors.New("Git remote has no owner/repository path")
	}
	owner, err := decodeRepositorySegment(segments[len(segments)-2])
	if err != nil {
		return remotePromptRepository{}, err
	}
	name, err := decodeRepositorySegment(segments[len(segments)-1])
	if err != nil {
		return remotePromptRepository{}, err
	}
	return remotePromptRepository{
		Host:       host,
		Repository: PromptRepository{Owner: owner, Name: name},
	}, nil
}

func decodeRepositorySegment(segment string) (string, error) {
	decoded, err := url.PathUnescape(segment)
	if err != nil {
		return "", errors.New("Git remote contains invalid path escaping")
	}
	if decoded == "" || decoded == "." || decoded == ".." || path.Base(decoded) != decoded {
		return "", errors.New("Git remote contains an invalid repository path")
	}
	for _, character := range decoded {
		if character < 0x20 || character == 0x7f {
			return "", errors.New("Git remote contains control characters")
		}
	}
	return decoded, nil
}
