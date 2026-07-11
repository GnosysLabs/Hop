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
		if !strings.Contains(text, "githop.xyz") {
			t.Errorf("%s does not name the canonical distribution host", relative)
		}
		if strings.Contains(text, "raw.githubusercontent.com/hop-vcs/hop") ||
			strings.Contains(text, "github.com/hop-vcs/hop") {
			t.Errorf("%s still points installation at GitHub", relative)
		}
	}
}
