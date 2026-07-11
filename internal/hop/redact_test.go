package hop

import (
	"bytes"
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRedactPromptSecrets(t *testing.T) {
	apiKey := "sk-proj-" + strings.Repeat("A7", 24)
	githubToken := "ghp_" + strings.Repeat("b9", 20)
	bearer := strings.Repeat("abcDEF123._-", 3)
	generic := strings.Repeat("customKey9", 4)
	privateKey := "-----BEGIN PRIVATE KEY-----\n" + strings.Repeat("base64material", 4) + "\n-----END PRIVATE KEY-----"

	tests := []struct {
		name   string
		prompt string
		secret string
		marker string
	}{
		{"provider API key", "Use " + apiKey + " for this request", apiKey, "[REDACTED:api_key]"},
		{"provider access token", "token=" + githubToken, githubToken, "[REDACTED:access_token]"},
		{"authorization header", "Authorization: Bearer " + bearer, bearer, "[REDACTED:auth_token]"},
		{"contextual generic key", "OPENAI_API_KEY=\"" + generic + "\"", generic, "[REDACTED:credential]"},
		{"natural language key", "my api key is " + generic, generic, "[REDACTED:credential]"},
		{"private key", "deploy with\n" + privateKey + "\nnow", privateKey, "[REDACTED:private_key]"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			redacted, findings := RedactPromptSecrets(test.prompt)
			if strings.Contains(redacted, test.secret) {
				t.Fatalf("redacted prompt still contains secret: %q", redacted)
			}
			if !strings.Contains(redacted, test.marker) {
				t.Fatalf("redacted prompt = %q, want marker %q", redacted, test.marker)
			}
			if len(findings) == 0 {
				t.Fatal("redaction metadata is empty")
			}
		})
	}

	plain := "Read OPENAI_API_KEY from the environment; the key is configuration; inspect commit 0123456789abcdef0123456789abcdef."
	if redacted, findings := RedactPromptSecrets(plain); redacted != plain || len(findings) != 0 {
		t.Fatalf("plain prompt changed to %q with findings %#v", redacted, findings)
	}
}

func TestPromptSecretNeverReachesDurableProjectBytes(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "base.txt"), "base\n")
	service, _, err := InitProject(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	secret := "sk-proj-" + strings.Repeat("NeverPersist7", 4)
	result, err := service.CreatePrompt(context.Background(), "Use this API key: "+secret, "", "test")
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Redactions) != 1 || result.Redactions[0].Count != 1 {
		t.Fatalf("redactions = %#v, want one finding", result.Redactions)
	}
	if strings.Contains(result.Prompt.Prompt, secret) || !strings.Contains(result.Prompt.Prompt, redactedMarkerPrefix) {
		t.Fatalf("stored prompt was not redacted: %q", result.Prompt.Prompt)
	}
	check, err := service.RunCheck(context.Background(), result.Prompt.ID, []string{
		"sh", "-c", "printf '%s' '" + secret + "'",
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(strings.Join(check.Command, " "), secret) || strings.Contains(check.Output, secret) {
		t.Fatalf("stored check leaked secret: %#v / %q", check.Command, check.Output)
	}
	proposal, err := service.Propose(context.Background(), result.Prompt.ID, "Completed with "+secret)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(proposal.Proposal.Summary, secret) {
		t.Fatalf("stored proposal summary leaked secret: %q", proposal.Proposal.Summary)
	}
	if err := service.Close(); err != nil {
		t.Fatal(err)
	}

	needle := []byte(secret)
	for _, tree := range []string{filepath.Join(root, ".hop"), filepath.Join(root, ".git", "refs", "hop")} {
		err := filepath.WalkDir(tree, func(path string, entry fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if entry.IsDir() {
				return nil
			}
			contents, readErr := os.ReadFile(path)
			if readErr != nil {
				return readErr
			}
			if bytes.Contains(contents, needle) {
				t.Errorf("secret leaked into durable file %s", path)
			}
			return nil
		})
		if err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
	}
}

func TestBeginJSONNeverEchoesPromptSecret(t *testing.T) {
	root := t.TempDir()
	previousDirectory, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(root); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(previousDirectory) })

	secret := "ghp_" + strings.Repeat("NoEcho7", 6)
	var stdout, stderr bytes.Buffer
	code := RunCLIWithInput(
		[]string{"begin", "--agent", "codex", "--session", "secret-test", "--stdin", "--json"},
		strings.NewReader("Use token="+secret), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("begin exited %d: %s", code, stderr.String())
	}
	if strings.Contains(stdout.String(), secret) || strings.Contains(stderr.String(), secret) {
		t.Fatal("begin output echoed the prompt secret")
	}
	if !strings.Contains(stdout.String(), redactedMarkerPrefix) || !strings.Contains(stdout.String(), `"redactions"`) {
		t.Fatalf("begin JSON omitted redaction evidence: %s", stdout.String())
	}
}
