package hop

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestTeaCommandSurfaceIsAvailableAsNativeHopCommands(t *testing.T) {
	want := []string{
		"clone", "whoami", "issues", "pulls", "labels", "milestones",
		"releases", "times", "organizations", "repos", "branches",
		"actions", "wiki", "webhooks", "comments", "open",
		"notifications", "ssh-keys", "admin", "api", "man",
	}
	commands := teaCompatibleCommandNames()
	for _, name := range want {
		if _, exists := commands[name]; !exists {
			t.Errorf("native Hop command %q is missing", name)
		}
	}
	for _, excluded := range []string{"login", "logout"} {
		if _, exists := commands[excluded]; exists {
			t.Errorf("Tea credential command %q must be replaced by Hop OAuth", excluded)
		}
	}
}

func TestTeaCompatibleHelpIsHopBrandedAndNeedsNoLogin(t *testing.T) {
	var stdout, stderr bytes.Buffer
	auth := &AuthClient{ProfilePath: filepath.Join(t.TempDir(), "missing.json")}
	code := runTeaCompatibleCLI(context.Background(), []string{"comments", "--help"}, strings.NewReader(""), &stdout, &stderr, auth)
	if code != 0 {
		t.Fatalf("help exit = %d, stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "hop comments") || strings.Contains(stdout.String(), "tea comments") {
		t.Fatalf("help is not Hop-branded:\n%s", stdout.String())
	}
}

func TestTeaCompatibleCommandUsesHopOAuthWithoutTeaLogin(t *testing.T) {
	credentials := newMemoryCredentialStore()
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.Header.Get("Authorization") != "token oauth-access" {
			t.Errorf("Authorization = %q", request.Header.Get("Authorization"))
		}
		switch request.URL.Path {
		case "/api/v1/version":
			_, _ = fmt.Fprint(w, `{"version":"1.26.4"}`)
		case "/api/v1/user":
			_, _ = fmt.Fprint(w, `{"id":1,"login":"alice","is_admin":true}`)
		default:
			http.NotFound(w, request)
		}
	}))
	defer server.Close()
	auth := &AuthClient{
		HTTP:        server.Client(),
		Credentials: credentials,
		ProfilePath: filepath.Join(t.TempDir(), "auth.json"),
	}
	if err := auth.writeProfile(authProfile{Version: authProfileVersion, Server: server.URL}); err != nil {
		t.Fatal(err)
	}
	if err := auth.storeCredential(server.URL, hopCredential{
		OAuthAccessToken: "oauth-access", OAuthRefreshToken: "oauth-refresh",
		OAuthExpiresAt: time.Now().Add(time.Hour), OAuthLogin: "alice", OAuthScope: "all",
	}); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GITEA_INSTANCE_URL", "sentinel-url")
	t.Setenv("GITEA_TOKEN", "sentinel-token")
	var stdout, stderr bytes.Buffer
	code := runTeaCompatibleCLI(context.Background(), []string{"whoami"}, strings.NewReader(""), &stdout, &stderr, auth)
	if code != 0 {
		t.Fatalf("whoami exit = %d, stderr=%s", code, stderr.String())
	}
	if strings.Contains(stdout.String(), "oauth-access") || strings.Contains(stderr.String(), "oauth-access") {
		t.Fatal("native command output exposed the OAuth token")
	}
	if got := environmentValue("GITEA_INSTANCE_URL"); got != "sentinel-url" {
		t.Fatalf("forge environment was not restored: %q", got)
	}
	if got := environmentValue("GITEA_TOKEN"); got != "sentinel-token" {
		t.Fatalf("token environment was not restored: %q", got)
	}
}

func environmentValue(name string) string {
	return os.Getenv(name)
}
