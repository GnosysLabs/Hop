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

func TestProductDocumentationUsesCanonicalGiteaHost(t *testing.T) {
	for _, relative := range []string{"README.md", "wiki/Home.md", "wiki/Installation.md"} {
		contents, err := os.ReadFile(filepath.Join("..", "..", filepath.FromSlash(relative)))
		if err != nil {
			t.Fatal(err)
		}
		text := string(contents)
		if !strings.Contains(text, "githop.xyz/GnosysLabs/Hop") {
			t.Errorf("%s does not name the canonical distribution host", relative)
		}
		if strings.Contains(text, "raw.githubusercontent.com/hop-vcs/hop") ||
			strings.Contains(text, "github.com/hop-vcs/hop") {
			t.Errorf("%s still points installation at GitHub", relative)
		}
	}
}

func TestDistributionDoesNotRequireGiteaActions(t *testing.T) {
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

func TestReleaseWorkflowRequiresPreProvisionedCredential(t *testing.T) {
	root := filepath.Clean(filepath.Join("..", ".."))
	script, err := os.ReadFile(filepath.Join(root, "scripts", "release-local.sh"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(script), "pre-existing scoped GITEA_TOKEN") {
		t.Fatal("release script does not require a pre-provisioned credential")
	}
	if strings.Contains(string(script), "/tokens") {
		t.Fatal("release script must not manage provider tokens")
	}
	checklist, err := os.ReadFile(filepath.Join(root, "wiki", "Release-Checklist.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(checklist), "must never create, rotate, list,") ||
		!strings.Contains(string(checklist), "or revoke account tokens") {
		t.Fatal("release checklist does not forbid agent token management")
	}
}
