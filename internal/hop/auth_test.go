package hop

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	keyring "github.com/zalando/go-keyring"
)

type memoryCredentialStore struct {
	mu     sync.Mutex
	values map[string]string
}

func newMemoryCredentialStore() *memoryCredentialStore {
	return &memoryCredentialStore{values: make(map[string]string)}
}

func (s *memoryCredentialStore) key(service, user string) string { return service + "\x00" + user }

func (s *memoryCredentialStore) Get(service, user string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	value, exists := s.values[s.key(service, user)]
	if !exists {
		return "", keyring.ErrNotFound
	}
	return value, nil
}

func (s *memoryCredentialStore) Set(service, user, password string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.values[s.key(service, user)] = password
	return nil
}

func (s *memoryCredentialStore) Delete(service, user string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := s.key(service, user)
	if _, exists := s.values[key]; !exists {
		return keyring.ErrNotFound
	}
	delete(s.values, key)
	return nil
}

func TestAuthLoginUsesPKCEAndStoresFullOAuthGrantOnlyInKeychain(t *testing.T) {
	credentials := newMemoryCredentialStore()
	var server *httptest.Server
	var challenge string
	var tokenCalls int
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/hop/api/v1/auth/config":
			writeAuthTestConfig(t, w, server.URL)
		case "/oauth/token":
			tokenCalls++
			if err := request.ParseForm(); err != nil {
				t.Error(err)
			}
			if request.Form.Get("grant_type") != "authorization_code" || request.Form.Get("code") != "test-code" {
				t.Errorf("unexpected token form: %v", request.Form)
			}
			verifier := request.Form.Get("code_verifier")
			digest := sha256.Sum256([]byte(verifier))
			if got := base64.RawURLEncoding.EncodeToString(digest[:]); got != challenge {
				t.Errorf("PKCE challenge = %q, want %q", got, challenge)
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprint(w, `{"access_token":"access-secret","refresh_token":"refresh-secret","token_type":"bearer","expires_in":3600}`)
		case "/hop/api/v1/auth/exchange":
			if got := request.Header.Get("Authorization"); got != "Bearer access-secret" {
				t.Errorf("Authorization = %q", got)
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprint(w, `{"token":"hop-secret","token_type":"Bearer","user":{"id":7,"login":"alice"}}`)
		case "/hop/api/v1/auth/me":
			if got := request.Header.Get("Authorization"); got != "Bearer hop-secret" {
				t.Errorf("me Authorization = %q", got)
			}
			_, _ = fmt.Fprint(w, `{"id":7,"login":"alice"}`)
		case "/api/v1/user":
			if got := request.Header.Get("Authorization"); got != "Bearer access-secret" {
				t.Errorf("Gitea user Authorization = %q", got)
			}
			_, _ = fmt.Fprint(w, `{"id":7,"login":"alice"}`)
		case "/hop/api/v1/auth/logout":
			if got := request.Header.Get("Authorization"); got != "Bearer hop-secret" {
				t.Errorf("logout Authorization = %q", got)
			}
			_, _ = fmt.Fprint(w, `{}`)
		default:
			http.NotFound(w, request)
		}
	}))
	defer server.Close()

	profilePath := filepath.Join(t.TempDir(), "hop", "auth.json")
	auth := &AuthClient{
		HTTP:            server.Client(),
		Credentials:     credentials,
		ProfilePath:     profilePath,
		CallbackTimeout: 5 * time.Second,
	}
	auth.OpenBrowser = func(target string) error {
		authorize, err := url.Parse(target)
		if err != nil {
			return err
		}
		query := authorize.Query()
		if query.Get("response_type") != "code" || query.Get("code_challenge_method") != "S256" {
			return fmt.Errorf("invalid authorize query: %s", authorize.RawQuery)
		}
		if query.Get("scope") != "all" {
			return fmt.Errorf("scope = %q", query.Get("scope"))
		}
		challenge = query.Get("code_challenge")
		callback, err := url.Parse(query.Get("redirect_uri"))
		if err != nil {
			return err
		}
		if callback.Scheme != "http" || callback.Hostname() != "127.0.0.1" || callback.Path != "/" {
			return fmt.Errorf("unsafe callback: %s", callback)
		}
		callbackQuery := callback.Query()
		callbackQuery.Set("code", "test-code")
		callbackQuery.Set("state", query.Get("state"))
		callback.RawQuery = callbackQuery.Encode()
		go func() {
			response, getErr := http.Get(callback.String())
			if getErr == nil {
				_ = response.Body.Close()
			}
		}()
		return nil
	}

	result, err := auth.Login(context.Background(), server.URL+"/", nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.UserID != 7 || result.Login != "alice" {
		t.Fatalf("login result = %#v", result)
	}
	if tokenCalls != 1 {
		t.Fatalf("token calls = %d, want 1", tokenCalls)
	}
	profile, err := os.ReadFile(profilePath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(profile), "access-secret") || strings.Contains(string(profile), "refresh-secret") {
		t.Fatalf("profile leaked OAuth credentials: %s", profile)
	}
	encodedResult, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encodedResult), "access-secret") || strings.Contains(string(encodedResult), "refresh-secret") {
		t.Fatalf("JSON result leaked OAuth credentials: %s", encodedResult)
	}
	if _, err := auth.Status(context.Background()); err != nil {
		t.Fatal(err)
	}
	stored, err := credentials.Get(keyringServiceName, credentialAccount(server.URL))
	if err != nil || !strings.Contains(stored, "hop-secret") || !strings.Contains(stored, "access-secret") || !strings.Contains(stored, "refresh-secret") {
		t.Fatalf("stored full OAuth credential = %q, %v", stored, err)
	}
	if _, err := auth.Logout(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := credentials.Get(keyringServiceName, credentialAccount(server.URL)); !errors.Is(err, keyring.ErrNotFound) {
		t.Fatalf("credentials remain after logout: %v", err)
	}
}

func TestAuthStatusUsesGiteaOAuthToken(t *testing.T) {
	credentials := newMemoryCredentialStore()
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/hop/api/v1/auth/config":
			writeAuthTestConfig(t, w, server.URL)
		case "/api/v1/user":
			if request.Header.Get("Authorization") != "Bearer oauth-access" {
				t.Errorf("request did not use Gitea OAuth token")
			}
			_, _ = fmt.Fprint(w, `{"id":9,"login":"bob"}`)
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
		Token: "hop-access", OAuthAccessToken: "oauth-access", OAuthRefreshToken: "oauth-refresh",
		OAuthExpiresAt: time.Now().Add(time.Hour), OAuthLogin: "bob",
	}); err != nil {
		t.Fatal(err)
	}
	result, err := auth.Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if result.Login != "bob" {
		t.Fatalf("status = %#v", result)
	}
}

func TestAuthRetriesUnauthorizedWithRotatingOAuthGrant(t *testing.T) {
	credentials := newMemoryCredentialStore()
	var server *httptest.Server
	refreshes := 0
	userRequests := 0
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/hop/api/v1/auth/config":
			writeAuthTestConfig(t, w, server.URL)
		case "/oauth/token":
			refreshes++
			if err := request.ParseForm(); err != nil {
				t.Error(err)
			}
			if request.Form.Get("grant_type") != "refresh_token" || request.Form.Get("refresh_token") != "refresh-old" || request.Form.Get("client_id") != "public-client" {
				t.Errorf("refresh form = %v", request.Form)
			}
			_, _ = fmt.Fprint(w, `{"access_token":"access-new","refresh_token":"refresh-new","token_type":"bearer","scope":"all","expires_in":3600}`)
		case "/api/v1/user":
			userRequests++
			if request.Header.Get("Authorization") == "Bearer access-old" {
				http.Error(w, "expired", http.StatusUnauthorized)
				return
			}
			if request.Header.Get("Authorization") != "Bearer access-new" {
				t.Errorf("retried user request authorization = %q", request.Header.Get("Authorization"))
			}
			_, _ = fmt.Fprint(w, `{"id":11,"login":"carol"}`)
		default:
			http.NotFound(w, request)
		}
	}))
	defer server.Close()
	profilePath := filepath.Join(t.TempDir(), "auth.json")
	auth := &AuthClient{HTTP: server.Client(), Credentials: credentials, ProfilePath: profilePath, Now: time.Now}
	if err := auth.writeProfile(authProfile{Version: authProfileVersion, Server: server.URL}); err != nil {
		t.Fatal(err)
	}
	if err := auth.storeCredential(server.URL, hopCredential{
		OAuthAccessToken: "access-old", OAuthRefreshToken: "refresh-old", OAuthScope: "all",
		OAuthExpiresAt: time.Now().Add(time.Hour), OAuthLogin: "carol",
	}); err != nil {
		t.Fatal(err)
	}
	result, err := auth.Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if result.Login != "carol" || refreshes != 1 || userRequests != 2 {
		t.Fatalf("result=%#v refreshes=%d users=%d", result, refreshes, userRequests)
	}
	stored, err := auth.loadCredential(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	if stored.OAuthAccessToken != "access-new" || stored.OAuthRefreshToken != "refresh-new" || stored.OAuthScope != "all" {
		t.Fatalf("rotated credential = %#v", stored)
	}
}

func TestGitRemoteAuthorizationRewritesSSHWithoutCredentialInURL(t *testing.T) {
	credentials := newMemoryCredentialStore()
	profilePath := filepath.Join(t.TempDir(), "auth.json")
	auth := &AuthClient{Credentials: credentials, ProfilePath: profilePath, Now: time.Now}
	if err := auth.writeProfile(authProfile{Version: authProfileVersion, Server: "https://githop.example"}); err != nil {
		t.Fatal(err)
	}
	secret := "oauth-access-secret"
	if err := auth.storeCredential("https://githop.example", hopCredential{
		OAuthAccessToken: secret, OAuthRefreshToken: "refresh-secret", OAuthLogin: "alice",
		OAuthExpiresAt: time.Now().Add(time.Hour), OAuthScope: "all",
	}); err != nil {
		t.Fatal(err)
	}
	target, env, ok, err := auth.gitRemoteAuthorization(context.Background(), "git@githop.example:Private/Repo.git")
	if err != nil {
		t.Fatal(err)
	}
	if !ok || target != "https://githop.example/Private/Repo.git" || strings.Contains(target, secret) {
		t.Fatalf("target=%q authenticated=%v", target, ok)
	}
	joined := strings.Join(env, "\n")
	if !strings.Contains(joined, "GIT_TERMINAL_PROMPT=0") || !strings.Contains(joined, "http.followRedirects") || strings.Contains(joined, "alice:"+secret) {
		t.Fatalf("Git auth environment is unsafe or incomplete: %s", joined)
	}
	other, otherEnv, otherOK, err := auth.gitRemoteAuthorization(context.Background(), "git@other.example:Private/Repo.git")
	if err != nil || otherOK || other != "git@other.example:Private/Repo.git" || len(otherEnv) != 0 {
		t.Fatalf("cross-forge remote=%q env=%v ok=%v err=%v", other, otherEnv, otherOK, err)
	}
}

func TestAuthConfigurationRejectsCrossOriginEndpoints(t *testing.T) {
	base, err := url.Parse("https://forge.example")
	if err != nil {
		t.Fatal(err)
	}
	config := AuthConfig{
		AuthorizationEndpoint: "https://evil.example/oauth/authorize",
		TokenEndpoint:         "https://forge.example/oauth/token",
		ExchangeEndpoint:      "https://forge.example/hop/api/v1/auth/exchange",
		MeEndpoint:            "https://forge.example/hop/api/v1/auth/me",
		LogoutEndpoint:        "https://forge.example/hop/api/v1/auth/logout",
		SyncEndpoint:          "https://forge.example/hop/api/v1/sync/prompts",
		ClientID:              "public-client",
		Scopes:                append([]string(nil), requiredOAuthScopes...),
	}
	if err := validateAuthConfig(base, &config); err == nil || !strings.Contains(err.Error(), "selected forge origin") {
		t.Fatalf("validateAuthConfig error = %v", err)
	}
}

func TestNormalizeForgeURLSecurity(t *testing.T) {
	for _, raw := range []string{
		"http://forge.example",
		"https://user:password@forge.example",
		"https://forge.example?token=secret",
		"javascript:alert(1)",
	} {
		if _, err := normalizeForgeURL(raw); err == nil {
			t.Errorf("normalizeForgeURL(%q) succeeded", raw)
		}
	}
	if got, err := normalizeForgeURL("https://FORGE.example/"); err != nil || got != "https://FORGE.example" {
		t.Fatalf("normalize secure forge = %q, %v", got, err)
	}
	if _, err := normalizeForgeURL("http://127.0.0.1:8080"); err != nil {
		t.Fatalf("loopback HTTP rejected: %v", err)
	}
}

func TestAuthResponseErrorsRedactAuthorizationHeaders(t *testing.T) {
	secret := "ghp_abcdefghijklmnopqrstuvwxyzABCDEFGHIJ"
	err := responseError("test request", http.StatusUnauthorized, []byte("Authorization: Bearer "+secret))
	if strings.Contains(err.Error(), secret) || !strings.Contains(err.Error(), redactedMarkerPrefix) {
		t.Fatalf("response error leaked authorization credential: %v", err)
	}
}

func TestAuthCLIWorksOutsideAHopProject(t *testing.T) {
	t.Setenv("HOP_AUTH_PROFILE", filepath.Join(t.TempDir(), "missing-auth.json"))
	var stdout, stderr bytes.Buffer
	if code := RunCLI([]string{"auth", "status", "--json"}, &stdout, &stderr); code != 1 {
		t.Fatalf("auth status exit = %d, stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "not signed in") || strings.Contains(stdout.String(), "Hop project") {
		t.Fatalf("auth status output = %s", stdout.String())
	}
	stdout.Reset()
	stderr.Reset()
	if code := RunCLI([]string{"auth", "logout", "--json"}, &stdout, &stderr); code != 0 {
		t.Fatalf("auth logout exit = %d, stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
}

func writeAuthTestConfig(t *testing.T, w http.ResponseWriter, base string) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(AuthConfig{
		AuthorizationEndpoint: base + "/oauth/authorize",
		TokenEndpoint:         base + "/oauth/token",
		ExchangeEndpoint:      base + "/hop/api/v1/auth/exchange",
		MeEndpoint:            base + "/hop/api/v1/auth/me",
		LogoutEndpoint:        base + "/hop/api/v1/auth/logout",
		SyncEndpoint:          base + "/hop/api/v1/sync/prompts",
		ClientID:              "public-client",
		Scopes:                append([]string(nil), requiredOAuthScopes...),
	}); err != nil {
		t.Error(err)
	}
}
