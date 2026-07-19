package hop

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

var markdownLink = regexp.MustCompile(`\[[^]]+\]\(([^)]+)\)`)

func TestDocumentationLinksResolve(t *testing.T) {
	root := filepath.Clean(filepath.Join("..", ".."))
	files := []string{filepath.Join(root, "README.md")}
	wiki, err := filepath.Glob(filepath.Join(root, "wiki", "*.md"))
	if err != nil {
		t.Fatal(err)
	}
	files = append(files, wiki...)
	if len(wiki) < 10 {
		t.Fatalf("wiki contains %d pages, want at least 10", len(wiki))
	}
	for _, file := range files {
		contents, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		for _, match := range markdownLink.FindAllStringSubmatch(string(contents), -1) {
			target := match[1]
			if strings.HasPrefix(target, "http://") || strings.HasPrefix(target, "https://") ||
				strings.HasPrefix(target, "#") || strings.HasPrefix(target, "mailto:") {
				continue
			}
			if anchor := strings.IndexByte(target, '#'); anchor >= 0 {
				target = target[:anchor]
			}
			if target == "" {
				continue
			}
			resolved := filepath.Clean(filepath.Join(filepath.Dir(file), filepath.FromSlash(target)))
			if filepath.Dir(file) == filepath.Join(root, "wiki") && filepath.Ext(resolved) == "" {
				resolved += ".md"
			}
			if _, err := os.Stat(resolved); err != nil {
				t.Errorf("%s links to missing %s: %v", file, target, err)
			}
		}
	}
}

func TestProductDocumentationUsesCanonicalGitHubHost(t *testing.T) {
	for _, relative := range []string{"README.md", "wiki/Home.md", "wiki/Installation.md"} {
		contents, err := os.ReadFile(filepath.Join("..", "..", filepath.FromSlash(relative)))
		if err != nil {
			t.Fatal(err)
		}
		text := string(contents)
		if !strings.Contains(text, "github.com/GnosysLabs/Hop") && !strings.Contains(text, "raw.githubusercontent.com/GnosysLabs/Hop") {
			t.Errorf("%s does not name the canonical distribution host", relative)
		}
		if strings.Contains(text, "githop.xyz/GnosysLabs/Hop/raw") {
			t.Errorf("%s still points installation at the retired forge", relative)
		}
	}
}

func TestDistributionDoesNotRequireHostedActions(t *testing.T) {
	root := filepath.Clean(filepath.Join("..", ".."))
	workflows, err := filepath.Glob(filepath.Join(root, ".gitea", "workflows", "*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(workflows) != 0 {
		t.Fatalf("Gitea Actions workflows remain enabled in source: %v", workflows)
	}
	contents, err := os.ReadFile(filepath.Join(root, "wiki", "Release-Checklist.md"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(contents), ".gitea/workflows/") {
		t.Fatal("release checklist still depends on a Gitea Actions workflow")
	}
}

func TestReleaseWorkflowUsesExistingGitHubCredential(t *testing.T) {
	root := filepath.Clean(filepath.Join("..", ".."))
	script, err := os.ReadFile(filepath.Join(root, "scripts", "release-local.sh"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(script), "gh auth status") || !strings.Contains(string(script), "gh release create") {
		t.Fatal("release script does not use the existing GitHub CLI credential")
	}
	if strings.Contains(string(script), "/tokens") {
		t.Fatal("release script must not manage provider tokens")
	}
	checklist, err := os.ReadFile(filepath.Join(root, "wiki", "Release-Checklist.md"))
	if err != nil {
		t.Fatal(err)
	}
	normalizedChecklist := strings.Join(strings.Fields(string(checklist)), " ")
	if !strings.Contains(normalizedChecklist, "must never create, rotate, list, or revoke account tokens") {
		t.Fatal("release checklist does not forbid agent token management")
	}
}

func TestAgentSkillIsForgeNeutral(t *testing.T) {
	root := filepath.Clean(filepath.Join("..", ".."))
	for _, relative := range []string{
		"skills/hop/SKILL.md",
		"skills/hop/references/protocol.md",
	} {
		contents, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(relative)))
		if err != nil {
			t.Fatal(err)
		}
		text := string(contents)
		if !strings.Contains(text, "hop host") {
			t.Errorf("%s does not direct agents to detect the Git host", relative)
		}
		if !strings.Contains(text, "never create") && !strings.Contains(text, "Do not request, create") {
			t.Errorf("%s does not preserve the token-management boundary", relative)
		}
	}
}

func TestAgentSkillDocumentsHostAwareCommands(t *testing.T) {
	contents, err := os.ReadFile(filepath.Join("..", "..", "skills", "hop", "SKILL.md"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(contents)
	for _, command := range []string{"hop clone", "hop issues", "hop pulls", "hop releases", "hop repos", "hop actions"} {
		if !strings.Contains(text, command) {
			t.Errorf("agent skill omits native command %q", command)
		}
	}
	for _, provider := range []string{"GitHub", "GitLab", "Gitea"} {
		if !strings.Contains(text, provider) {
			t.Errorf("agent skill omits %s adapter", provider)
		}
	}
}

func TestAgentSkillClassifiesStaleProjectionThroughHop(t *testing.T) {
	root := filepath.Clean(filepath.Join("..", ".."))
	for _, relative := range []string{
		"skills/hop/SKILL.md",
		"skills/hop/references/protocol.md",
		"wiki/Troubleshooting.md",
	} {
		contents, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(relative)))
		if err != nil {
			t.Fatal(err)
		}
		text := string(contents)
		if !strings.Contains(text, "hop status") || !strings.Contains(text, "hop sync-git") {
			t.Errorf("%s does not route stale-projection diagnosis through Hop", relative)
		}
		lower := strings.ToLower(strings.Join(strings.Fields(text), " "))
		if !strings.Contains(lower, "not uncommitted user work") &&
			!strings.Contains(lower, "never describe those paths as user edits or uncommitted work") &&
			!strings.Contains(lower, "never call projection-only paths uncommitted user work") {
			t.Errorf("%s does not forbid calling projection-only paths user work", relative)
		}
	}
}

func TestReleaseIncludesTeaLicenseNotice(t *testing.T) {
	root := filepath.Clean(filepath.Join("..", ".."))
	notice, err := os.ReadFile(filepath.Join(root, "THIRD_PARTY_NOTICES.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(notice), "Gitea Authors") || !strings.Contains(string(notice), "Permission is hereby granted") {
		t.Fatal("Tea's required MIT notice is incomplete")
	}
	config, err := os.ReadFile(filepath.Join(root, ".goreleaser.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(config), "THIRD_PARTY_NOTICES*") {
		t.Fatal("release archives omit third-party notices")
	}
}
