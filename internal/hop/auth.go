package hop

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"time"

	keyring "github.com/zalando/go-keyring"
)

const (
	authProfileVersion = 1
	keyringServiceName = "Hop CLI"
	maxAuthResponse    = 1 << 20
)

var requiredOAuthScopes = []string{"all"}

// AuthConfig is the public OAuth and API discovery document served by a Hop
// forge. Tokens are exchanged directly with the forge's OAuth provider.
type AuthConfig struct {
	AuthorizationEndpoint string   `json:"authorization_endpoint"`
	TokenEndpoint         string   `json:"token_endpoint"`
	ExchangeEndpoint      string   `json:"exchange_endpoint"`
	MeEndpoint            string   `json:"me_endpoint"`
	LogoutEndpoint        string   `json:"logout_endpoint"`
	SyncEndpoint          string   `json:"sync_endpoint"`
	ClientID              string   `json:"client_id"`
	Scopes                []string `json:"scopes"`
}

type AuthUser struct {
	ID       int64  `json:"id"`
	Login    string `json:"login"`
	FullName string `json:"full_name,omitempty"`
}

type AuthResult struct {
	Forge     string     `json:"forge"`
	Login     string     `json:"login"`
	UserID    int64      `json:"user_id"`
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
}

type ForgeRepository struct {
	Name     string `json:"name"`
	FullName string `json:"full_name"`
	Private  bool   `json:"private"`
	HTMLURL  string `json:"html_url"`
	CloneURL string `json:"clone_url"`
	SSHURL   string `json:"ssh_url"`
}

type authProfile struct {
	Version int    `json:"version"`
	Server  string `json:"server"`
}

type hopCredential struct {
	Token             string    `json:"token,omitempty"`
	ExpiresAt         time.Time `json:"expires_at,omitempty"`
	OAuthAccessToken  string    `json:"oauth_access_token"`
	OAuthRefreshToken string    `json:"oauth_refresh_token"`
	OAuthTokenType    string    `json:"oauth_token_type,omitempty"`
	OAuthScope        string    `json:"oauth_scope,omitempty"`
	OAuthExpiresAt    time.Time `json:"oauth_expires_at,omitempty"`
	OAuthLogin        string    `json:"oauth_login"`
}

// oauthTokens is the wire representation used for initial and refresh token
// exchanges. The resulting grant is persisted only in the OS keychain.
type oauthTokens struct {
	AccessToken  string
	RefreshToken string
	TokenType    string
	Scope        string
	ExpiresAt    time.Time
}

type oauthTokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	TokenType    string `json:"token_type"`
	Scope        string `json:"scope"`
	ExpiresIn    int64  `json:"expires_in"`
}

type hopTokenResponse struct {
	Token     string    `json:"token"`
	TokenType string    `json:"token_type"`
	ExpiresAt time.Time `json:"expires_at,omitempty"`
	User      AuthUser  `json:"user"`
}

type credentialStore interface {
	Get(service, user string) (string, error)
	Set(service, user, password string) error
	Delete(service, user string) error
}

type osCredentialStore struct{}

func (osCredentialStore) Get(service, user string) (string, error) {
	return keyring.Get(service, user)
}

func (osCredentialStore) Set(service, user, password string) error {
	return keyring.Set(service, user, password)
}

func (osCredentialStore) Delete(service, user string) error {
	return keyring.Delete(service, user)
}

// AuthClient owns the OAuth browser flow and authenticated Hop API requests.
// Its dependencies are explicit so the security-sensitive flow can be tested
// without opening a browser or touching a developer's real keychain.
type AuthClient struct {
	HTTP            *http.Client
	Credentials     credentialStore
	ProfilePath     string
	OpenBrowser     func(string) error
	CallbackTimeout time.Duration
	Now             func() time.Time
}

func NewAuthClient() *AuthClient {
	return &AuthClient{
		HTTP: &http.Client{
			Timeout: 2 * time.Minute,
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				if len(via) >= 5 {
					return errors.New("too many HTTP redirects")
				}
				if len(via) > 0 && !sameOrigin(via[0].URL, req.URL) {
					return errors.New("refusing a cross-origin Hop API redirect")
				}
				return nil
			},
		},
		Credentials:     osCredentialStore{},
		ProfilePath:     defaultAuthProfilePath(),
		OpenBrowser:     openBrowser,
		CallbackTimeout: 3 * time.Minute,
		Now:             time.Now,
	}
}

func defaultAuthProfilePath() string {
	if configured := strings.TrimSpace(os.Getenv("HOP_AUTH_PROFILE")); configured != "" {
		return configured
	}
	root, err := os.UserConfigDir()
	if err != nil || root == "" {
		if home, homeErr := os.UserHomeDir(); homeErr == nil {
			root = filepath.Join(home, ".config")
		}
	}
	return filepath.Join(root, "hop", "auth.json")
}

// Login performs Authorization Code + PKCE with a loopback callback. Gitea's
// public-client redirect registration accepts an arbitrary port on 127.0.0.1,
// with the callback rooted at http://127.0.0.1:<port>/.
func (c *AuthClient) Login(ctx context.Context, rawServer string, authorizationReady func(string)) (AuthResult, error) {
	previousProfile, _ := c.readProfile()
	server, err := normalizeForgeURL(rawServer)
	if err != nil {
		return AuthResult{}, err
	}
	config, err := c.Discover(ctx, server)
	if err != nil {
		return AuthResult{}, err
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return AuthResult{}, fmt.Errorf("start OAuth loopback callback: %w", err)
	}
	defer listener.Close()
	redirectURI := "http://" + listener.Addr().String() + "/"
	state, err := randomBase64URL(32)
	if err != nil {
		return AuthResult{}, fmt.Errorf("create OAuth state: %w", err)
	}
	verifier, err := randomBase64URL(48)
	if err != nil {
		return AuthResult{}, fmt.Errorf("create PKCE verifier: %w", err)
	}
	challengeBytes := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(challengeBytes[:])

	authorizeURL, err := url.Parse(config.AuthorizationEndpoint)
	if err != nil {
		return AuthResult{}, fmt.Errorf("parse authorization endpoint: %w", err)
	}
	query := authorizeURL.Query()
	query.Set("client_id", config.ClientID)
	query.Set("redirect_uri", redirectURI)
	query.Set("response_type", "code")
	query.Set("scope", strings.Join(requiredOAuthScopes, " "))
	query.Set("state", state)
	query.Set("code_challenge", challenge)
	query.Set("code_challenge_method", "S256")
	authorizeURL.RawQuery = query.Encode()

	type callbackResult struct {
		code string
		err  error
	}
	callback := make(chan callbackResult, 1)
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/" {
			http.NotFound(w, request)
			return
		}
		providedState := request.URL.Query().Get("state")
		if subtle.ConstantTimeCompare([]byte(providedState), []byte(state)) != 1 {
			http.Error(w, "Invalid OAuth state. You may close this tab and retry.", http.StatusBadRequest)
			return
		}
		if oauthError := request.URL.Query().Get("error"); oauthError != "" {
			description := strings.TrimSpace(request.URL.Query().Get("error_description"))
			if description == "" {
				description = oauthError
			}
			description, _ = RedactPromptSecrets(description)
			select {
			case callback <- callbackResult{err: fmt.Errorf("authorization was not completed: %s", description)}:
			default:
			}
			http.Error(w, "Hop authorization was not completed. You may close this tab.", http.StatusBadRequest)
			return
		}
		code := request.URL.Query().Get("code")
		if code == "" || len(code) > 4096 {
			http.Error(w, "Missing OAuth authorization code.", http.StatusBadRequest)
			return
		}
		select {
		case callback <- callbackResult{code: code}:
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.Header().Set("Cache-Control", "no-store")
			_, _ = io.WriteString(w, "<!doctype html><title>Hop signed in</title><p>Signed in to Hop. You may close this tab.</p>")
		default:
			http.Error(w, "OAuth callback was already handled.", http.StatusConflict)
		}
	})
	callbackServer := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       5 * time.Second,
	}
	serveDone := make(chan error, 1)
	go func() {
		serveErr := callbackServer.Serve(listener)
		if errors.Is(serveErr, http.ErrServerClosed) {
			serveErr = nil
		}
		serveDone <- serveErr
	}()

	if authorizationReady != nil {
		authorizationReady(authorizeURL.String())
	}
	if c.OpenBrowser == nil {
		c.OpenBrowser = openBrowser
	}
	// Browser launch is best effort. Keeping the callback alive lets users copy
	// the printed URL when a headless host has no opener installed.
	browserErr := c.OpenBrowser(authorizeURL.String())
	wait := c.CallbackTimeout
	if wait <= 0 {
		wait = 3 * time.Minute
	}
	waitCtx, cancel := context.WithTimeout(ctx, wait)
	defer cancel()
	var authorizationCode string
	select {
	case result := <-callback:
		if result.err != nil {
			_ = callbackServer.Close()
			return AuthResult{}, result.err
		}
		authorizationCode = result.code
	case serveErr := <-serveDone:
		if serveErr == nil {
			serveErr = errors.New("OAuth callback server stopped")
		}
		return AuthResult{}, serveErr
	case <-waitCtx.Done():
		_ = callbackServer.Close()
		if browserErr != nil {
			return AuthResult{}, fmt.Errorf("timed out waiting for browser authorization (browser launch failed: %v); visit %s", browserErr, authorizeURL.String())
		}
		return AuthResult{}, errors.New("timed out waiting for browser authorization")
	}
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 2*time.Second)
	_ = callbackServer.Shutdown(shutdownCtx)
	shutdownCancel()

	oauthToken, err := c.exchangeCode(ctx, config, authorizationCode, redirectURI, verifier)
	if err != nil {
		return AuthResult{}, err
	}
	credential, user, err := c.exchangeForHopToken(ctx, config, oauthToken.AccessToken)
	if err != nil {
		return AuthResult{}, err
	}
	credential.OAuthAccessToken = oauthToken.AccessToken
	credential.OAuthRefreshToken = oauthToken.RefreshToken
	credential.OAuthTokenType = oauthToken.TokenType
	credential.OAuthScope = oauthToken.Scope
	credential.OAuthExpiresAt = oauthToken.ExpiresAt
	credential.OAuthLogin = user.Login
	if err := c.storeCredential(server, credential); err != nil {
		return AuthResult{}, err
	}
	if err := c.writeProfile(authProfile{Version: authProfileVersion, Server: server}); err != nil {
		_ = c.deleteTokens(server)
		return AuthResult{}, err
	}
	if previousProfile.Server != "" && credentialAccount(previousProfile.Server) != credentialAccount(server) {
		_ = c.deleteTokens(previousProfile.Server)
	}
	return AuthResult{Forge: server, Login: user.Login, UserID: user.ID, ExpiresAt: credentialExpiry(credential)}, nil
}

func (c *AuthClient) Status(ctx context.Context) (AuthResult, error) {
	profile, err := c.readProfile()
	if err != nil {
		return AuthResult{}, err
	}
	body, credential, err := c.giteaRequest(ctx, profile.Server, http.MethodGet, "api/v1/user", nil)
	if err != nil {
		return AuthResult{}, err
	}
	var user AuthUser
	if err := json.Unmarshal(body, &user); err != nil {
		return AuthResult{}, fmt.Errorf("decode signed-in Hop user: %w", err)
	}
	if user.ID <= 0 || strings.TrimSpace(user.Login) == "" {
		return AuthResult{}, errors.New("Hop user response is missing id or login")
	}
	return AuthResult{Forge: profile.Server, Login: user.Login, UserID: user.ID, ExpiresAt: credentialExpiry(credential)}, nil
}

// CreateRepository creates a repository through the signed-in user's Gitea
// OAuth grant. The credential never leaves the keychain-backed AuthClient.
func (c *AuthClient) CreateRepository(ctx context.Context, owner, name string, private bool) (ForgeRepository, error) {
	owner = strings.TrimSpace(owner)
	name = strings.TrimSpace(name)
	if err := validateForgePathSegment("repository owner", owner); err != nil {
		return ForgeRepository{}, err
	}
	if err := validateForgePathSegment("repository name", name); err != nil {
		return ForgeRepository{}, err
	}
	profile, err := c.readProfile()
	if err != nil {
		return ForgeRepository{}, err
	}
	status, err := c.Status(ctx)
	if err != nil {
		return ForgeRepository{}, err
	}
	payload, err := json.Marshal(map[string]any{
		"name":      name,
		"private":   private,
		"auto_init": false,
	})
	if err != nil {
		return ForgeRepository{}, fmt.Errorf("encode repository request: %w", err)
	}
	suffix := "api/v1/user/repos"
	if !strings.EqualFold(owner, status.Login) {
		suffix = "api/v1/orgs/" + url.PathEscape(owner) + "/repos"
	}
	body, _, err := c.giteaRequest(ctx, profile.Server, http.MethodPost, suffix, payload)
	if err != nil {
		return ForgeRepository{}, fmt.Errorf("create Gitea repository %s/%s: %w", owner, name, err)
	}
	var repository ForgeRepository
	if err := json.Unmarshal(body, &repository); err != nil {
		return ForgeRepository{}, fmt.Errorf("decode created Gitea repository: %w", err)
	}
	if repository.FullName == "" || repository.CloneURL == "" {
		return ForgeRepository{}, errors.New("Gitea returned an incomplete repository response")
	}
	return repository, nil
}

// ForgeAPI performs an arbitrary same-forge Gitea API operation with the
// existing OAuth grant. It accepts only relative /api/v1 paths, preventing a
// caller from forwarding the credential to another origin.
func (c *AuthClient) ForgeAPI(ctx context.Context, method, apiPath string, body []byte) ([]byte, error) {
	method = strings.ToUpper(strings.TrimSpace(method))
	switch method {
	case http.MethodGet, http.MethodPost, http.MethodPatch, http.MethodPut, http.MethodDelete:
	default:
		return nil, fmt.Errorf("unsupported forge API method %q", method)
	}
	relative, err := url.Parse(strings.TrimSpace(apiPath))
	if err != nil || relative.IsAbs() || relative.Host != "" || relative.User != nil || relative.Fragment != "" {
		return nil, errors.New("forge API path must be a relative /api/v1 path")
	}
	cleanPath := "/" + strings.TrimLeft(relative.Path, "/")
	if !strings.HasPrefix(cleanPath, "/api/v1/") {
		return nil, errors.New("forge API path must start with /api/v1/")
	}
	profile, err := c.readProfile()
	if err != nil {
		return nil, err
	}
	base, err := url.Parse(profile.Server)
	if err != nil {
		return nil, err
	}
	base.Path = strings.TrimRight(base.Path, "/") + cleanPath
	base.RawQuery = relative.RawQuery
	response, _, err := c.oauthEndpointRequest(ctx, profile.Server, method, base.String(), body)
	if err != nil {
		return nil, fmt.Errorf("Gitea API %s %s: %w", method, cleanPath, err)
	}
	return response, nil
}

// OAuthAccessToken returns a current access token for a deliberately launched
// child process. Callers must keep it out of argv, output, and durable files.
func (c *AuthClient) OAuthAccessToken(ctx context.Context) (string, error) {
	profile, err := c.readProfile()
	if err != nil {
		return "", err
	}
	credential, err := c.oauthCredential(ctx, profile.Server, false, "")
	if err != nil {
		return "", err
	}
	if credential.OAuthAccessToken == "" {
		return "", ErrNotAuthenticated
	}
	return credential.OAuthAccessToken, nil
}

func validateForgePathSegment(label, value string) error {
	if value == "" || value == "." || value == ".." || strings.ContainsAny(value, "/\\\x00\r\n") {
		return fmt.Errorf("invalid %s %q", label, value)
	}
	return nil
}

func credentialExpiry(credential hopCredential) *time.Time {
	if !credential.OAuthExpiresAt.IsZero() {
		expires := credential.OAuthExpiresAt
		return &expires
	}
	if credential.ExpiresAt.IsZero() {
		return nil
	}
	expires := credential.ExpiresAt
	return &expires
}

func (c *AuthClient) Logout(ctx context.Context) (string, error) {
	profile, err := c.readProfile()
	if errors.Is(err, ErrNotAuthenticated) {
		return "", nil
	}
	if err != nil {
		// Logout is also the recovery path for a corrupted or manually edited
		// non-secret profile. Best-effort removal must not contact its URL.
		contents, readErr := os.ReadFile(c.ProfilePath)
		if readErr != nil {
			return "", err
		}
		var unsafeProfile authProfile
		_ = json.Unmarshal(contents, &unsafeProfile)
		if unsafeProfile.Server != "" {
			_ = c.deleteTokens(unsafeProfile.Server)
		}
		if removeErr := os.Remove(c.ProfilePath); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
			return "", fmt.Errorf("remove invalid Hop auth profile: %w", removeErr)
		}
		return unsafeProfile.Server, nil
	}
	credential, err := c.loadCredential(profile.Server)
	if err != nil && !errors.Is(err, ErrNotAuthenticated) {
		return "", err
	}
	if err == nil {
		// The Hop upload token is transitional. Revoke it when the endpoint is
		// available, but never make local OAuth sign-out depend on the network.
		if credential.Token != "" {
			if config, discoverErr := c.Discover(ctx, profile.Server); discoverErr == nil {
				_, _ = c.hopRequest(ctx, credential, http.MethodPost, config.LogoutEndpoint, []byte(`{}`))
			}
		}
	}
	if err := c.deleteTokens(profile.Server); err != nil {
		return "", err
	}
	if err := os.Remove(c.ProfilePath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("remove Hop auth profile: %w", err)
	}
	return profile.Server, nil
}

var ErrNotAuthenticated = errors.New("not signed in; run hop auth login https://your-forge.example")

func (c *AuthClient) Discover(ctx context.Context, server string) (AuthConfig, error) {
	base, err := url.Parse(server)
	if err != nil {
		return AuthConfig{}, fmt.Errorf("parse Hop server: %w", err)
	}
	discovery := *base
	discovery.Path = strings.TrimRight(discovery.Path, "/") + "/hop/api/v1/auth/config"
	discovery.RawPath = ""
	discovery.RawQuery = ""
	discovery.Fragment = ""
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, discovery.String(), nil)
	if err != nil {
		return AuthConfig{}, err
	}
	request.Header.Set("Accept", "application/json")
	response, err := c.httpClient().Do(request)
	if err != nil {
		return AuthConfig{}, fmt.Errorf("discover Hop OAuth configuration: %w", err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, maxAuthResponse+1))
	if err != nil {
		return AuthConfig{}, fmt.Errorf("read Hop OAuth configuration: %w", err)
	}
	if len(body) > maxAuthResponse {
		return AuthConfig{}, errors.New("Hop OAuth configuration response is too large")
	}
	if response.StatusCode != http.StatusOK {
		return AuthConfig{}, responseError("discover Hop OAuth configuration", response.StatusCode, body)
	}
	var config AuthConfig
	if err := json.Unmarshal(body, &config); err != nil {
		return AuthConfig{}, fmt.Errorf("decode Hop OAuth configuration: %w", err)
	}
	if err := validateAuthConfig(base, &config); err != nil {
		return AuthConfig{}, err
	}
	return config, nil
}

func validateAuthConfig(base *url.URL, config *AuthConfig) error {
	if strings.TrimSpace(config.ClientID) == "" {
		return errors.New("Hop OAuth configuration has no client_id")
	}
	if len(config.Scopes) != 1 || !slices.Equal(config.Scopes, requiredOAuthScopes) {
		return errors.New("Hop OAuth configuration must advertise exactly the Gitea all scope")
	}
	for label, raw := range map[string]*string{
		"authorization_endpoint": &config.AuthorizationEndpoint,
		"token_endpoint":         &config.TokenEndpoint,
		"exchange_endpoint":      &config.ExchangeEndpoint,
		"me_endpoint":            &config.MeEndpoint,
		"logout_endpoint":        &config.LogoutEndpoint,
		"sync_endpoint":          &config.SyncEndpoint,
	} {
		resolved, err := resolveTrustedEndpoint(base, *raw)
		if err != nil {
			return fmt.Errorf("invalid %s: %w", label, err)
		}
		*raw = resolved
	}
	return nil
}

func resolveTrustedEndpoint(base *url.URL, raw string) (string, error) {
	endpoint, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return "", err
	}
	endpoint = base.ResolveReference(endpoint)
	if endpoint.User != nil || endpoint.Fragment != "" || endpoint.RawQuery != "" {
		return "", errors.New("endpoint must not contain credentials, a query, or a fragment")
	}
	if !sameOrigin(base, endpoint) {
		return "", errors.New("endpoint is not on the selected forge origin")
	}
	if !secureHTTPURL(endpoint) {
		return "", errors.New("endpoint must use HTTPS (HTTP is allowed only for loopback testing)")
	}
	return endpoint.String(), nil
}

func normalizeForgeURL(raw string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return "", fmt.Errorf("parse forge URL: %w", err)
	}
	if parsed.Scheme == "" || parsed.Host == "" {
		return "", errors.New("forge URL must include https:// and a host")
	}
	if parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", errors.New("forge URL must not include credentials, a query, or a fragment")
	}
	if !secureHTTPURL(parsed) {
		return "", errors.New("forge URL must use HTTPS (HTTP is allowed only for loopback testing)")
	}
	parsed.Path = strings.TrimRight(parsed.Path, "/")
	parsed.RawPath = ""
	return parsed.String(), nil
}

func secureHTTPURL(endpoint *url.URL) bool {
	if endpoint.Scheme == "https" {
		return true
	}
	if endpoint.Scheme != "http" {
		return false
	}
	host := strings.ToLower(endpoint.Hostname())
	return host == "127.0.0.1" || host == "::1" || host == "localhost"
}

func sameOrigin(left, right *url.URL) bool {
	return strings.EqualFold(left.Scheme, right.Scheme) && strings.EqualFold(left.Host, right.Host)
}

func (c *AuthClient) exchangeCode(ctx context.Context, config AuthConfig, code, redirectURI, verifier string) (oauthTokens, error) {
	form := url.Values{
		"grant_type":    {"authorization_code"},
		"client_id":     {config.ClientID},
		"code":          {code},
		"redirect_uri":  {redirectURI},
		"code_verifier": {verifier},
	}
	return c.exchangeToken(ctx, config.TokenEndpoint, form, oauthTokens{})
}

func (c *AuthClient) exchangeToken(ctx context.Context, endpoint string, form url.Values, previous oauthTokens) (oauthTokens, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return oauthTokens{}, err
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response, err := c.httpClient().Do(request)
	if err != nil {
		return oauthTokens{}, fmt.Errorf("exchange OAuth token: %w", err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, maxAuthResponse+1))
	if err != nil {
		return oauthTokens{}, fmt.Errorf("read OAuth token response: %w", err)
	}
	if len(body) > maxAuthResponse {
		return oauthTokens{}, errors.New("OAuth token response is too large")
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return oauthTokens{}, responseError("exchange OAuth token", response.StatusCode, body)
	}
	var tokenResponse oauthTokenResponse
	if err := json.Unmarshal(body, &tokenResponse); err != nil {
		return oauthTokens{}, fmt.Errorf("decode OAuth token response: %w", err)
	}
	if strings.TrimSpace(tokenResponse.AccessToken) == "" {
		return oauthTokens{}, errors.New("OAuth token response has no access_token")
	}
	if tokenResponse.RefreshToken == "" {
		tokenResponse.RefreshToken = previous.RefreshToken
	}
	if tokenResponse.Scope == "" {
		tokenResponse.Scope = previous.Scope
	}
	if tokenResponse.TokenType == "" {
		tokenResponse.TokenType = previous.TokenType
	}
	tokens := oauthTokens{
		AccessToken:  tokenResponse.AccessToken,
		RefreshToken: tokenResponse.RefreshToken,
		TokenType:    tokenResponse.TokenType,
		Scope:        tokenResponse.Scope,
	}
	if tokenResponse.ExpiresIn > 0 {
		tokens.ExpiresAt = c.now().UTC().Add(time.Duration(tokenResponse.ExpiresIn) * time.Second)
	}
	return tokens, nil
}

func (c *AuthClient) exchangeForHopToken(ctx context.Context, config AuthConfig, temporaryGiteaToken string) (hopCredential, AuthUser, error) {
	deviceName, err := os.Hostname()
	if err != nil || strings.TrimSpace(deviceName) == "" {
		deviceName = runtime.GOOS + " device"
	}
	payload, err := json.Marshal(map[string]string{"device_name": deviceName})
	if err != nil {
		return hopCredential{}, AuthUser{}, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, config.ExchangeEndpoint, strings.NewReader(string(payload)))
	if err != nil {
		return hopCredential{}, AuthUser{}, err
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+temporaryGiteaToken)
	response, err := c.httpClient().Do(request)
	if err != nil {
		return hopCredential{}, AuthUser{}, fmt.Errorf("exchange temporary Gitea authorization for Hop token: %w", err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, maxAuthResponse+1))
	if err != nil {
		return hopCredential{}, AuthUser{}, err
	}
	if len(body) > maxAuthResponse {
		return hopCredential{}, AuthUser{}, errors.New("Hop token response is too large")
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return hopCredential{}, AuthUser{}, responseError("exchange temporary Gitea authorization for Hop token", response.StatusCode, body)
	}
	var result hopTokenResponse
	if err := json.Unmarshal(body, &result); err != nil {
		return hopCredential{}, AuthUser{}, fmt.Errorf("decode Hop token response: %w", err)
	}
	if strings.TrimSpace(result.Token) == "" || result.User.ID <= 0 || strings.TrimSpace(result.User.Login) == "" {
		return hopCredential{}, AuthUser{}, errors.New("Hop token response is missing token or user identity")
	}
	return hopCredential{Token: result.Token, ExpiresAt: result.ExpiresAt}, result.User, nil
}

func (c *AuthClient) hopRequest(ctx context.Context, credential hopCredential, method, endpoint string, body []byte) ([]byte, error) {
	if credential.Token == "" {
		return nil, ErrNotAuthenticated
	}
	request, err := http.NewRequestWithContext(ctx, method, endpoint, strings.NewReader(string(body)))
	if err != nil {
		return nil, err
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("Authorization", "Bearer "+credential.Token)
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := c.httpClient().Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	responseBody, err := io.ReadAll(io.LimitReader(response.Body, (16<<20)+1))
	if err != nil {
		return nil, err
	}
	if len(responseBody) > 16<<20 {
		return nil, errors.New("authenticated Hop response is too large")
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, responseError("authenticated Hop request", response.StatusCode, responseBody)
	}
	return responseBody, nil
}

// giteaRequest uses the user's full OAuth grant directly and retries once with
// a rotated refresh grant when Gitea reports that the access token expired.
func (c *AuthClient) giteaRequest(ctx context.Context, server, method, suffix string, body []byte) ([]byte, hopCredential, error) {
	endpoint, err := appendEndpointPath(server, suffix)
	if err != nil {
		return nil, hopCredential{}, err
	}
	return c.oauthEndpointRequest(ctx, server, method, endpoint, body)
}

func (c *AuthClient) oauthEndpointRequest(ctx context.Context, server, method, endpoint string, body []byte) ([]byte, hopCredential, error) {
	credential, err := c.oauthCredential(ctx, server, false, "")
	if err != nil {
		return nil, hopCredential{}, err
	}
	responseBody, status, err := c.bearerRequest(ctx, credential.OAuthAccessToken, method, endpoint, body)
	if err != nil {
		return nil, hopCredential{}, err
	}
	if status == http.StatusUnauthorized {
		credential, err = c.oauthCredential(ctx, server, true, credential.OAuthAccessToken)
		if err != nil {
			return nil, hopCredential{}, err
		}
		responseBody, status, err = c.bearerRequest(ctx, credential.OAuthAccessToken, method, endpoint, body)
		if err != nil {
			return nil, hopCredential{}, err
		}
	}
	if status < 200 || status >= 300 {
		return nil, hopCredential{}, responseError("authenticated Gitea request", status, responseBody)
	}
	return responseBody, credential, nil
}

func (c *AuthClient) bearerRequest(ctx context.Context, token, method, endpoint string, body []byte) ([]byte, int, error) {
	if token == "" {
		return nil, 0, ErrNotAuthenticated
	}
	request, err := http.NewRequestWithContext(ctx, method, endpoint, strings.NewReader(string(body)))
	if err != nil {
		return nil, 0, err
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("Authorization", "Bearer "+token)
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := c.httpClient().Do(request)
	if err != nil {
		return nil, 0, err
	}
	defer response.Body.Close()
	responseBody, err := io.ReadAll(io.LimitReader(response.Body, (16<<20)+1))
	if err != nil {
		return nil, 0, err
	}
	if len(responseBody) > 16<<20 {
		return nil, 0, errors.New("authenticated Gitea response is too large")
	}
	return responseBody, response.StatusCode, nil
}

func (c *AuthClient) oauthCredential(ctx context.Context, server string, force bool, failedToken string) (hopCredential, error) {
	credential, err := c.loadCredential(server)
	if err != nil {
		return hopCredential{}, err
	}
	if credential.OAuthAccessToken == "" {
		return hopCredential{}, errors.New("stored login predates full repository access; run hop auth login again")
	}
	refreshSoon := !credential.OAuthExpiresAt.IsZero() && !credential.OAuthExpiresAt.After(c.now().UTC().Add(5*time.Minute))
	if !force && !refreshSoon {
		return credential, nil
	}
	if credential.OAuthRefreshToken == "" {
		return hopCredential{}, errors.New("Gitea OAuth session cannot be refreshed; run hop auth login again")
	}
	lockPath := c.ProfilePath + ".refresh.lock"
	release, err := acquireFileLock(ctx, lockPath, "Hop OAuth refresh")
	if err != nil {
		return hopCredential{}, err
	}
	defer release()
	credential, err = c.loadCredential(server)
	if err != nil {
		return hopCredential{}, err
	}
	if force && failedToken != "" && credential.OAuthAccessToken != failedToken {
		return credential, nil
	}
	refreshSoon = !credential.OAuthExpiresAt.IsZero() && !credential.OAuthExpiresAt.After(c.now().UTC().Add(5*time.Minute))
	if !force && !refreshSoon {
		return credential, nil
	}
	config, err := c.Discover(ctx, server)
	if err != nil {
		return hopCredential{}, err
	}
	previous := oauthTokens{
		AccessToken:  credential.OAuthAccessToken,
		RefreshToken: credential.OAuthRefreshToken,
		TokenType:    credential.OAuthTokenType,
		Scope:        credential.OAuthScope,
		ExpiresAt:    credential.OAuthExpiresAt,
	}
	refreshed, err := c.exchangeToken(ctx, config.TokenEndpoint, url.Values{
		"grant_type":    {"refresh_token"},
		"client_id":     {config.ClientID},
		"refresh_token": {credential.OAuthRefreshToken},
	}, previous)
	if err != nil {
		return hopCredential{}, fmt.Errorf("refresh Gitea OAuth session: %w", err)
	}
	credential.OAuthAccessToken = refreshed.AccessToken
	credential.OAuthRefreshToken = refreshed.RefreshToken
	credential.OAuthTokenType = refreshed.TokenType
	credential.OAuthScope = refreshed.Scope
	credential.OAuthExpiresAt = refreshed.ExpiresAt
	if err := c.storeCredential(server, credential); err != nil {
		return hopCredential{}, err
	}
	return credential, nil
}

func (c *AuthClient) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

func appendEndpointPath(base, suffix string) (string, error) {
	parsed, err := url.Parse(base)
	if err != nil {
		return "", err
	}
	parsed.Path = strings.TrimRight(parsed.Path, "/") + "/" + strings.TrimLeft(suffix, "/")
	parsed.RawPath = ""
	return parsed.String(), nil
}

// gitRemoteAuthorization turns any same-forge SSH/HTTP remote into a clean
// HTTPS URL and provides a URL-scoped Basic header through Git's environment
// config. The OAuth token never appears in argv, the remote URL, or GitError.
func (c *AuthClient) gitRemoteAuthorization(ctx context.Context, rawRemote string) (string, []string, bool, error) {
	profile, err := c.readProfile()
	if errors.Is(err, ErrNotAuthenticated) {
		return rawRemote, nil, false, nil
	}
	if err != nil {
		return "", nil, false, err
	}
	remote, err := parsePromptRemote(rawRemote)
	if err != nil {
		return rawRemote, nil, false, nil
	}
	forge, err := url.Parse(profile.Server)
	if err != nil {
		return "", nil, false, err
	}
	if !strings.EqualFold(forge.Hostname(), remote.Host) {
		return rawRemote, nil, false, nil
	}
	credential, err := c.oauthCredential(ctx, profile.Server, false, "")
	if err != nil {
		return "", nil, false, err
	}
	if strings.TrimSpace(credential.OAuthLogin) == "" {
		return "", nil, false, errors.New("stored OAuth login is incomplete; run hop auth login again")
	}
	forge.Path = strings.TrimRight(forge.Path, "/") + "/" + remote.Repository.Owner + "/" + remote.Repository.Name + ".git"
	forge.RawPath = ""
	forge.RawQuery = ""
	forge.Fragment = ""
	forge.User = nil
	target := forge.String()
	basic := base64.StdEncoding.EncodeToString([]byte(credential.OAuthLogin + ":" + credential.OAuthAccessToken))
	headerKey := "http." + target + ".extraHeader"
	env := []string{
		"GIT_TERMINAL_PROMPT=0",
		"GIT_CONFIG_COUNT=3",
		"GIT_CONFIG_KEY_0=" + headerKey,
		"GIT_CONFIG_VALUE_0=",
		"GIT_CONFIG_KEY_1=" + headerKey,
		"GIT_CONFIG_VALUE_1=Authorization: Basic " + basic,
		"GIT_CONFIG_KEY_2=http.followRedirects",
		"GIT_CONFIG_VALUE_2=false",
	}
	return target, env, true, nil
}

func (c *AuthClient) storeCredential(server string, credential hopCredential) error {
	encoded, err := json.Marshal(credential)
	if err != nil {
		return fmt.Errorf("encode Hop credential: %w", err)
	}
	if c.Credentials == nil {
		return errors.New("OS credential store is unavailable")
	}
	if err := c.Credentials.Set(keyringServiceName, credentialAccount(server), string(encoded)); err != nil {
		return fmt.Errorf("store Hop credential in the OS keychain: %w", err)
	}
	return nil
}

func (c *AuthClient) loadCredential(server string) (hopCredential, error) {
	if c.Credentials == nil {
		return hopCredential{}, errors.New("OS credential store is unavailable")
	}
	encoded, err := c.Credentials.Get(keyringServiceName, credentialAccount(server))
	if errors.Is(err, keyring.ErrNotFound) {
		return hopCredential{}, ErrNotAuthenticated
	}
	if err != nil {
		return hopCredential{}, fmt.Errorf("read Hop credential from the OS keychain: %w", err)
	}
	var credential hopCredential
	if err := json.Unmarshal([]byte(encoded), &credential); err != nil || (credential.Token == "" && credential.OAuthAccessToken == "") {
		return hopCredential{}, errors.New("stored Hop credentials are invalid; run hop auth logout, then sign in again")
	}
	return credential, nil
}

func (c *AuthClient) deleteTokens(server string) error {
	if c.Credentials == nil {
		return errors.New("OS credential store is unavailable")
	}
	err := c.Credentials.Delete(keyringServiceName, credentialAccount(server))
	if errors.Is(err, keyring.ErrNotFound) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("delete OAuth credentials from the OS keychain: %w", err)
	}
	return nil
}

func credentialAccount(server string) string {
	parsed, err := url.Parse(server)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return strings.ToLower(strings.TrimRight(server, "/"))
	}
	return strings.ToLower(parsed.Scheme + "://" + parsed.Host)
}

func (c *AuthClient) readProfile() (authProfile, error) {
	contents, err := os.ReadFile(c.ProfilePath)
	if errors.Is(err, os.ErrNotExist) {
		return authProfile{}, ErrNotAuthenticated
	}
	if err != nil {
		return authProfile{}, fmt.Errorf("read Hop auth profile: %w", err)
	}
	var profile authProfile
	if err := json.Unmarshal(contents, &profile); err != nil {
		return authProfile{}, fmt.Errorf("decode Hop auth profile: %w", err)
	}
	if profile.Version != authProfileVersion || profile.Server == "" {
		return authProfile{}, errors.New("Hop auth profile is invalid; run hop auth logout, then sign in again")
	}
	normalized, err := normalizeForgeURL(profile.Server)
	if err != nil {
		return authProfile{}, errors.New("Hop auth profile has an unsafe forge URL; remove it with hop auth logout")
	}
	profile.Server = normalized
	return profile, nil
}

func (c *AuthClient) writeProfile(profile authProfile) error {
	if c.ProfilePath == "" {
		return errors.New("Hop auth profile path is unavailable")
	}
	if err := os.MkdirAll(filepath.Dir(c.ProfilePath), 0o700); err != nil {
		return fmt.Errorf("create Hop config directory: %w", err)
	}
	contents, err := json.MarshalIndent(profile, "", "  ")
	if err != nil {
		return err
	}
	temporary, err := os.CreateTemp(filepath.Dir(c.ProfilePath), ".auth-*.tmp")
	if err != nil {
		return fmt.Errorf("create Hop auth profile: %w", err)
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0o600); err != nil {
		_ = temporary.Close()
		return err
	}
	if _, err := temporary.Write(append(contents, '\n')); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := os.Rename(temporaryPath, c.ProfilePath); err != nil {
		return fmt.Errorf("write Hop auth profile: %w", err)
	}
	return nil
}

func (c *AuthClient) httpClient() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return &http.Client{Timeout: 2 * time.Minute}
}

func responseError(action string, status int, body []byte) error {
	message := strings.TrimSpace(string(body))
	if len(message) > 2048 {
		message = message[:2048]
	}
	message, _ = RedactPromptSecrets(message)
	if message == "" {
		return fmt.Errorf("%s: HTTP %d", action, status)
	}
	return fmt.Errorf("%s: HTTP %d: %s", action, status, message)
}

func randomBase64URL(bytes int) (string, error) {
	value := make([]byte, bytes)
	if _, err := rand.Read(value); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(value), nil
}

func openBrowser(target string) error {
	var command *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		command = exec.Command("open", target)
	case "windows":
		command = exec.Command("rundll32", "url.dll,FileProtocolHandler", target)
	default:
		command = exec.Command("xdg-open", target)
	}
	if err := command.Start(); err != nil {
		return err
	}
	return nil
}
