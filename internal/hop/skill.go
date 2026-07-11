package hop

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	hopskill "githop.xyz/hop/hop/skills/hop"
)

type SkillInstallResult struct {
	Path  string   `json:"path"`
	Files []string `json:"files"`
}

func DefaultSkillBase() (string, error) {
	home := os.Getenv("CODEX_HOME")
	if home == "" {
		userHome, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("find home directory: %w", err)
		}
		home = filepath.Join(userHome, ".codex")
	}
	return filepath.Join(home, "skills"), nil
}

func EmbeddedSkillText() (string, error) {
	contents, err := hopskill.Files.ReadFile("SKILL.md")
	if err != nil {
		return "", fmt.Errorf("read embedded Hop skill: %w", err)
	}
	return string(contents), nil
}

// InstallSkill writes the embedded skill below a skills directory. Existing
// bundles require force; known files are overwritten without deleting unknown
// user files from the destination.
func InstallSkill(base string, force bool) (SkillInstallResult, error) {
	if strings.TrimSpace(base) == "" {
		var err error
		base, err = DefaultSkillBase()
		if err != nil {
			return SkillInstallResult{}, err
		}
	}
	absBase, err := filepath.Abs(base)
	if err != nil {
		return SkillInstallResult{}, fmt.Errorf("resolve skills directory: %w", err)
	}
	target := filepath.Join(absBase, "hop")
	if info, err := os.Lstat(target); err == nil {
		if info.Mode()&os.ModeSymlink != 0 {
			return SkillInstallResult{}, fmt.Errorf("refusing to install through symlink %s", target)
		}
		if !info.IsDir() {
			return SkillInstallResult{}, fmt.Errorf("skill target exists and is not a directory: %s", target)
		}
		if !force {
			return SkillInstallResult{}, fmt.Errorf("Hop skill already exists at %s; pass --force to update it", target)
		}
	} else if !os.IsNotExist(err) {
		return SkillInstallResult{}, fmt.Errorf("inspect skill target: %w", err)
	}
	if err := os.MkdirAll(target, 0o755); err != nil {
		return SkillInstallResult{}, fmt.Errorf("create skill target: %w", err)
	}

	result := SkillInstallResult{Path: target}
	err = fs.WalkDir(hopskill.Files, ".", func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if path == "." {
			return nil
		}
		destination := filepath.Join(target, filepath.FromSlash(path))
		if entry.IsDir() {
			return os.MkdirAll(destination, 0o755)
		}
		if info, statErr := os.Lstat(destination); statErr == nil && info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("refusing to overwrite symlink %s", destination)
		} else if statErr != nil && !os.IsNotExist(statErr) {
			return statErr
		}
		contents, readErr := hopskill.Files.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		if writeErr := os.WriteFile(destination, contents, 0o644); writeErr != nil {
			return writeErr
		}
		result.Files = append(result.Files, path)
		return nil
	})
	if err != nil {
		return SkillInstallResult{}, fmt.Errorf("install Hop skill: %w", err)
	}
	return result, nil
}
