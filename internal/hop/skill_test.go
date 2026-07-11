package hop

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestInstallSkillBundle(t *testing.T) {
	base := t.TempDir()
	result, err := InstallSkill(base, false)
	if err != nil {
		t.Fatal(err)
	}
	if result.Path != filepath.Join(base, "hop") {
		t.Fatalf("skill path = %s", result.Path)
	}
	wantFiles := []string{"SKILL.md", "agents/openai.yaml", "references/protocol.md"}
	for _, relative := range wantFiles {
		contents, err := os.ReadFile(filepath.Join(result.Path, relative))
		if err != nil {
			t.Fatalf("read installed %s: %v", relative, err)
		}
		if len(contents) == 0 {
			t.Fatalf("installed %s is empty", relative)
		}
	}
	metadata, err := os.ReadFile(filepath.Join(result.Path, "agents", "openai.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(metadata), "allow_implicit_invocation: true") {
		t.Fatal("installed skill does not permit Desktop implicit invocation")
	}
	skill, err := os.ReadFile(filepath.Join(result.Path, "SKILL.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(skill), "Auto-accept by default") {
		t.Fatal("installed skill does not enable automatic acceptance")
	}
	if _, err := InstallSkill(base, false); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("second install error = %v, want existing-skill error", err)
	}
	if err := os.WriteFile(filepath.Join(result.Path, "SKILL.md"), []byte("tampered"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := InstallSkill(base, true); err != nil {
		t.Fatal(err)
	}
	contents, err := os.ReadFile(filepath.Join(result.Path, "SKILL.md"))
	if err != nil {
		t.Fatal(err)
	}
	if string(contents) == "tampered" || !strings.Contains(string(contents), "name: hop") {
		t.Fatal("forced skill update did not restore the embedded bundle")
	}
}

func TestSkillCLIWorksOutsideHopProject(t *testing.T) {
	var stdout, stderr strings.Builder
	base := t.TempDir()
	code := RunCLI([]string{"skill", "install", "--path", base, "--json"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("skill install exited %d: %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), filepath.Join(base, "hop")) {
		t.Fatalf("skill install JSON omitted target: %s", stdout.String())
	}
	stdout.Reset()
	stderr.Reset()
	code = RunCLI([]string{"skill", "print"}, &stdout, &stderr)
	if code != 0 || !strings.Contains(stdout.String(), "Capture the current prompt first") {
		t.Fatalf("skill print exited %d\nstdout: %s\nstderr: %s", code, stdout.String(), stderr.String())
	}
}
