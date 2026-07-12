package hop

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
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
	if len(result.Paths) != 1 || result.Paths[0] != result.Path {
		t.Fatalf("skill paths = %#v, want only %s", result.Paths, result.Path)
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
		t.Fatal("installed OpenAI metadata does not permit implicit invocation")
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

func TestInstallDefaultSkillsWritesSharedCodexAndClaudeBundles(t *testing.T) {
	home := t.TempDir()
	codexHome := filepath.Join(home, "codex-home")
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("CODEX_HOME", codexHome)

	result, err := InstallDefaultSkills(false)
	if err != nil {
		t.Fatal(err)
	}
	codexTarget := filepath.Join(codexHome, "skills", "hop")
	sharedTarget := filepath.Join(home, ".agents", "skills", "hop")
	claudeTarget := filepath.Join(home, ".claude", "skills", "hop")
	if result.Path != codexTarget {
		t.Fatalf("legacy primary path = %s, want %s", result.Path, codexTarget)
	}
	wantPaths := []string{codexTarget, sharedTarget, claudeTarget}
	if len(result.Paths) != len(wantPaths) {
		t.Fatalf("default paths = %#v, want %#v", result.Paths, wantPaths)
	}
	for index, want := range wantPaths {
		if result.Paths[index] != want {
			t.Fatalf("default path %d = %s, want %s", index, result.Paths[index], want)
		}
	}
	var reference []byte
	for _, target := range wantPaths {
		skill, err := os.ReadFile(filepath.Join(target, "SKILL.md"))
		if err != nil {
			t.Fatal(err)
		}
		if reference == nil {
			reference = skill
		} else if !bytes.Equal(reference, skill) {
			t.Fatal("default skill bundles differ")
		}
	}
}

func TestInstallDefaultSkillsPreflightsAndDeduplicates(t *testing.T) {
	t.Run("preflight", func(t *testing.T) {
		home := t.TempDir()
		codexHome := filepath.Join(home, "codex-home")
		t.Setenv("HOME", home)
		t.Setenv("USERPROFILE", home)
		t.Setenv("CODEX_HOME", codexHome)
		codexTarget := filepath.Join(codexHome, "skills", "hop")
		if err := os.MkdirAll(codexTarget, 0o755); err != nil {
			t.Fatal(err)
		}
		unknown := filepath.Join(codexTarget, "user-note.txt")
		if err := os.WriteFile(unknown, []byte("keep me"), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := InstallDefaultSkills(false); err == nil || !strings.Contains(err.Error(), "--force") {
			t.Fatalf("default preflight error = %v", err)
		}
		sharedTarget := filepath.Join(home, ".agents", "skills", "hop")
		if _, err := os.Stat(sharedTarget); !os.IsNotExist(err) {
			t.Fatalf("partial shared install exists after preflight failure: %v", err)
		}
		if _, err := InstallDefaultSkills(true); err != nil {
			t.Fatal(err)
		}
		if contents, err := os.ReadFile(unknown); err != nil || string(contents) != "keep me" {
			t.Fatalf("force install removed unknown file: %q, %v", string(contents), err)
		}
	})

	t.Run("nested symlink", func(t *testing.T) {
		if runtime.GOOS == "windows" {
			t.Skip("symlink creation requires additional privileges on Windows")
		}
		home := t.TempDir()
		codexHome := filepath.Join(home, "codex-home")
		t.Setenv("HOME", home)
		t.Setenv("USERPROFILE", home)
		t.Setenv("CODEX_HOME", codexHome)
		sharedTarget := filepath.Join(home, ".agents", "skills", "hop")
		if err := os.MkdirAll(sharedTarget, 0o755); err != nil {
			t.Fatal(err)
		}
		outside := filepath.Join(home, "outside")
		if err := os.WriteFile(outside, []byte("do not overwrite"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outside, filepath.Join(sharedTarget, "SKILL.md")); err != nil {
			t.Fatal(err)
		}
		if _, err := InstallDefaultSkills(true); err == nil || !strings.Contains(err.Error(), "symlink") {
			t.Fatalf("nested symlink preflight error = %v", err)
		}
		codexTarget := filepath.Join(codexHome, "skills", "hop")
		if _, err := os.Stat(codexTarget); !os.IsNotExist(err) {
			t.Fatalf("partial Codex install exists after nested preflight failure: %v", err)
		}
		if contents, err := os.ReadFile(outside); err != nil || string(contents) != "do not overwrite" {
			t.Fatalf("nested symlink target changed: %q, %v", contents, err)
		}
	})

	t.Run("dangling parent symlink", func(t *testing.T) {
		if runtime.GOOS == "windows" {
			t.Skip("symlink creation requires additional privileges on Windows")
		}
		home := t.TempDir()
		codexHome := filepath.Join(home, "codex-home")
		t.Setenv("HOME", home)
		t.Setenv("USERPROFILE", home)
		t.Setenv("CODEX_HOME", codexHome)
		if err := os.Symlink(filepath.Join(home, "missing-agents-home"), filepath.Join(home, ".agents")); err != nil {
			t.Fatal(err)
		}
		if _, err := InstallDefaultSkills(true); err == nil || !strings.Contains(err.Error(), "ancestor") {
			t.Fatalf("dangling parent preflight error = %v", err)
		}
		codexTarget := filepath.Join(codexHome, "skills", "hop")
		if _, err := os.Stat(codexTarget); !os.IsNotExist(err) {
			t.Fatalf("partial Codex install exists after dangling-parent failure: %v", err)
		}
	})

	t.Run("deduplicate", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("HOME", home)
		t.Setenv("USERPROFILE", home)
		t.Setenv("CODEX_HOME", filepath.Join(home, ".agents"))
		result, err := InstallDefaultSkills(false)
		if err != nil {
			t.Fatal(err)
		}
		if len(result.Paths) != 2 {
			t.Fatalf("aliased Codex/shared paths were not deduplicated alongside Claude: %#v", result.Paths)
		}
	})
}

func TestSkillCLIWorksOutsideHopProject(t *testing.T) {
	var stdout, stderr strings.Builder
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("CODEX_HOME", filepath.Join(home, "codex-home"))
	base := filepath.Join(home, "custom-skills")
	code := RunCLI([]string{"skill", "install", "--path", base, "--json"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("skill install exited %d: %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), filepath.Join(base, "hop")) {
		t.Fatalf("skill install JSON omitted target: %s", stdout.String())
	}
	for _, unexpected := range []string{
		filepath.Join(home, ".agents", "skills", "hop"),
		filepath.Join(home, "codex-home", "skills", "hop"),
		filepath.Join(home, ".claude", "skills", "hop"),
	} {
		if _, err := os.Stat(unexpected); !os.IsNotExist(err) {
			t.Fatalf("explicit --path also installed default target %s: %v", unexpected, err)
		}
	}
	stdout.Reset()
	stderr.Reset()
	code = RunCLI([]string{"skill", "print"}, &stdout, &stderr)
	if code != 0 || !strings.Contains(stdout.String(), "Capture the current prompt first") {
		t.Fatalf("skill print exited %d\nstdout: %s\nstderr: %s", code, stdout.String(), stderr.String())
	}
}
