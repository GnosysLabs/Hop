package hop

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestParsePromptRemote(t *testing.T) {
	tests := []struct {
		remote string
		host   string
		owner  string
		name   string
	}{
		{"git@githop.xyz:GnosysLabs/Hop.git", "githop.xyz", "GnosysLabs", "Hop"},
		{"ssh://git@githop.xyz/GnosysLabs/Hop.git", "githop.xyz", "GnosysLabs", "Hop"},
		{"https://githop.xyz/GnosysLabs/Hop.git", "githop.xyz", "GnosysLabs", "Hop"},
		{"https://forge.example/gitea/owner/repository", "forge.example", "owner", "repository"},
	}
	for _, test := range tests {
		t.Run(test.remote, func(t *testing.T) {
			parsed, err := parsePromptRemote(test.remote)
			if err != nil {
				t.Fatal(err)
			}
			if parsed.Host != test.host || parsed.Repository.Owner != test.owner || parsed.Repository.Name != test.name {
				t.Fatalf("parsed = %#v", parsed)
			}
		})
	}
	for _, invalid := range []string{
		"/tmp/repository.git",
		"file:///tmp/repository.git",
		"https://forge.example/only-one-segment",
		"https://forge.example/owner/%2Fetc",
		"https://forge.example/owner/..",
	} {
		if _, err := parsePromptRemote(invalid); err == nil {
			t.Errorf("parsePromptRemote(%q) succeeded", invalid)
		}
	}
}

func TestPromptCloudSyncUsesAuthenticatedRepositoryAndRedactedPortableRecords(t *testing.T) {
	ctx := context.Background()
	credentials := newMemoryCredentialStore()
	var server *httptest.Server
	var requests []promptSyncRequest
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/hop/api/v1/auth/config":
			writeAuthTestConfig(t, w, server.URL)
		case "/hop/api/v1/sync/prompts":
			if request.Header.Get("Authorization") != "Bearer sync-access" {
				t.Errorf("Authorization = %q", request.Header.Get("Authorization"))
			}
			var payload promptSyncRequest
			if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
				t.Error(err)
			}
			requests = append(requests, payload)
			results := make([]map[string]string, 0, len(payload.Prompts)+len(payload.Tombstones))
			for _, record := range payload.Prompts {
				results = append(results, map[string]string{"state_id": record.StateID, "status": "synced", "revision": record.Revision})
			}
			for _, id := range payload.Tombstones {
				results = append(results, map[string]string{"state_id": id, "status": "deleted"})
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"repository": payload.Repository, "synced": len(payload.Prompts), "results": results})
		default:
			http.NotFound(w, request)
		}
	}))
	defer server.Close()

	profilePath := filepath.Join(t.TempDir(), "auth.json")
	t.Setenv("HOP_AUTH_PROFILE", profilePath)
	root := t.TempDir()
	service, _, err := InitProject(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = service.Close() })
	runGitTest(t, service.Root, "remote", "add", "origin", server.URL+"/PrivateOrg/SecretRepo.git")
	secret := "ghp_abcdefghijklmnopqrstuvwxyzABCDEFGHIJ"
	started, err := service.CreatePrompt(ctx, "Implement this with "+secret, "", "agent")
	if err != nil {
		t.Fatal(err)
	}
	completion, err := service.Store.PutPromptCompletion(ctx, PromptCompletion{
		StateID: started.Prompt.ID, Summary: "Implemented and tested",
		FinalResponse: "Implemented the private feature.\n\nAll tests pass.",
	})
	if err != nil {
		t.Fatal(err)
	}

	auth := &AuthClient{
		HTTP:        server.Client(),
		Credentials: credentials,
		ProfilePath: profilePath,
	}
	if err := auth.writeProfile(authProfile{Version: authProfileVersion, Server: server.URL}); err != nil {
		t.Fatal(err)
	}
	if err := auth.storeCredential(server.URL, hopCredential{
		OAuthAccessToken: "sync-access", OAuthRefreshToken: "sync-refresh", OAuthLogin: "alice",
		OAuthExpiresAt: time.Now().Add(time.Hour), OAuthScope: "all",
	}); err != nil {
		t.Fatal(err)
	}
	for attempt := 0; attempt < 2; attempt++ {
		result, err := service.syncPromptHistory(ctx, auth)
		if err != nil {
			t.Fatal(err)
		}
		wantSynced := 1
		if attempt == 1 {
			wantSynced = 0
		}
		if result == nil || result.Repository.Owner != "PrivateOrg" || result.Repository.Name != "SecretRepo" || result.Synced != wantSynced {
			t.Fatalf("sync result = %#v", result)
		}
	}
	if len(requests) != 1 {
		t.Fatalf("sync requests = %d, want one durable upload", len(requests))
	}
	if err := os.MkdirAll(filepath.Join(root, ".hop", "records"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".hop", "records", "suppressed.json"), []byte(fmt.Sprintf(`{"prompt_ids":[%q]}`, started.Prompt.ID)), 0o600); err != nil {
		t.Fatal(err)
	}
	deletedResult, err := service.syncPromptHistory(ctx, auth)
	if err != nil {
		t.Fatal(err)
	}
	if deletedResult.Deleted != 1 || len(requests) != 2 || len(requests[1].Tombstones) != 1 || requests[1].Tombstones[0] != started.Prompt.ID {
		t.Fatalf("tombstone sync result=%#v requests=%#v", deletedResult, requests)
	}
	request := requests[0]
	if request.SchemaVersion != 1 || len(request.Prompts) != 1 {
		t.Fatalf("payload = %#v", request)
	}
	encoded, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), secret) || !strings.Contains(request.Prompts[0].Prompt, redactedMarkerPrefix) {
		t.Fatalf("prompt sync did not redact credential: %s", encoded)
	}
	if request.Prompts[0].ResponseSummary != completion.Summary || request.Prompts[0].FinalResponse != completion.FinalResponse {
		t.Fatalf("prompt sync omitted completion: %#v", request.Prompts[0])
	}
}

func TestRedactPortablePromptRecordCoversSummaryAndAgent(t *testing.T) {
	secret := "ghp_abcdefghijklmnopqrstuvwxyzABCDEFGHIJ"
	record := PortablePromptRecord{
		Prompt:          "prompt " + secret,
		AgentName:       "agent " + secret,
		ResponseSummary: "summary " + secret,
		FinalResponse:   "final " + secret,
	}
	redacted := redactPortablePromptRecord(record)
	encoded, err := json.Marshal(redacted)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), secret) {
		t.Fatalf("portable record leaked credential: %s", encoded)
	}
}

func TestPromptSyncBatchesBoundCountAndSize(t *testing.T) {
	prompts := make([]PortablePromptRecord, 101)
	for index := range prompts {
		prompts[index] = PortablePromptRecord{
			ID:      fmt.Sprintf("P_%03d", index),
			StateID: fmt.Sprintf("P_%03d", index),
			Prompt:  strings.Repeat("x", 32<<10),
		}
	}
	batches, err := promptSyncBatches(PromptRepository{Owner: "owner", Name: "repo"}, prompts)
	if err != nil {
		t.Fatal(err)
	}
	if len(batches) != 3 {
		t.Fatalf("batches = %d, want 3", len(batches))
	}
	total := 0
	for _, encoded := range batches {
		if len(encoded) > maxPromptSyncBatchBytes {
			t.Fatalf("batch size = %d", len(encoded))
		}
		var batch promptSyncRequest
		if err := json.Unmarshal(encoded, &batch); err != nil {
			t.Fatal(err)
		}
		if len(batch.Prompts) > maxPromptSyncBatchRecords {
			t.Fatalf("batch records = %d", len(batch.Prompts))
		}
		total += len(batch.Prompts)
	}
	if total != len(prompts) {
		t.Fatalf("batched prompts = %d, want %d", total, len(prompts))
	}
	if empty, err := promptSyncBatches(PromptRepository{}, nil); err != nil || len(empty) != 0 {
		t.Fatalf("empty batches = %#v, %v", empty, err)
	}
}

func TestPromptCloudSyncDoesNotPostAnEmptyLedger(t *testing.T) {
	ctx := context.Background()
	credentials := newMemoryCredentialStore()
	posts := 0
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/hop/api/v1/auth/config":
			writeAuthTestConfig(t, w, server.URL)
		case "/hop/api/v1/sync/prompts":
			posts++
			http.Error(w, "empty sync must not be posted", http.StatusBadRequest)
		default:
			http.NotFound(w, request)
		}
	}))
	defer server.Close()

	root := t.TempDir()
	service, _, err := InitProject(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = service.Close() })
	runGitTest(t, service.Root, "remote", "add", "origin", server.URL+"/owner/repo.git")
	auth := &AuthClient{
		HTTP:        server.Client(),
		Credentials: credentials,
		ProfilePath: filepath.Join(t.TempDir(), "auth.json"),
	}
	if err := auth.writeProfile(authProfile{Version: authProfileVersion, Server: server.URL}); err != nil {
		t.Fatal(err)
	}
	if err := auth.storeCredential(server.URL, hopCredential{
		OAuthAccessToken: "sync-access", OAuthRefreshToken: "sync-refresh", OAuthLogin: "alice",
		OAuthExpiresAt: time.Now().Add(time.Hour), OAuthScope: "all",
	}); err != nil {
		t.Fatal(err)
	}
	result, err := service.syncPromptHistory(ctx, auth)
	if err != nil {
		t.Fatal(err)
	}
	if result == nil || result.Synced != 0 || posts != 0 {
		t.Fatalf("empty sync result = %#v, posts = %d", result, posts)
	}
}
