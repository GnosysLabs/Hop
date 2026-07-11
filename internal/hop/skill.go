package hop

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	hopskill "githop.xyz/GnosysLabs/Hop/skills/hop"
)

type SkillInstallResult struct {
	Path  string   `json:"path"`
	Paths []string `json:"paths,omitempty"`
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

func DefaultSharedSkillBase() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("find home directory: %w", err)
	}
	return filepath.Join(home, ".agents", "skills"), nil
}

func DefaultSkillBases() ([]string, error) {
	codex, err := DefaultSkillBase()
	if err != nil {
		return nil, err
	}
	shared, err := DefaultSharedSkillBase()
	if err != nil {
		return nil, err
	}
	return []string{codex, shared}, nil
}

func EmbeddedSkillText() (string, error) {
	contents, err := hopskill.Files.ReadFile("SKILL.md")
	if err != nil {
		return "", fmt.Errorf("read embedded Hop skill: %w", err)
	}
	return string(contents), nil
}

// InstallSkill writes the embedded skill below one skills directory. Existing
// bundles require force; known files are overwritten without deleting unknown
// user files from the destination. An empty base retains the legacy Codex
// destination; the CLI uses InstallDefaultSkills for its no-path behavior.
func InstallSkill(base string, force bool) (SkillInstallResult, error) {
	if strings.TrimSpace(base) == "" {
		var err error
		base, err = DefaultSkillBase()
		if err != nil {
			return SkillInstallResult{}, err
		}
	}
	target, err := resolveSkillTarget(base)
	if err != nil {
		return SkillInstallResult{}, err
	}
	if err := preflightSkillTarget(target, force); err != nil {
		return SkillInstallResult{}, err
	}
	return writeSkillTarget(target)
}

// InstallDefaultSkills installs the same embedded files in the client-specific
// Codex directory and the cross-client .agents directory. Destinations are
// canonicalized and preflighted before any write to avoid partial upgrades when
// one existing bundle requires --force.
func InstallDefaultSkills(force bool) (SkillInstallResult, error) {
	bases, err := DefaultSkillBases()
	if err != nil {
		return SkillInstallResult{}, err
	}
	var targets []string
	seen := make(map[string]struct{}, len(bases))
	for _, base := range bases {
		target, err := resolveSkillTarget(base)
		if err != nil {
			return SkillInstallResult{}, err
		}
		key, err := canonicalPathKey(target)
		if err != nil {
			return SkillInstallResult{}, err
		}
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		targets = append(targets, target)
	}
	for _, target := range targets {
		if err := preflightSkillTarget(target, force); err != nil {
			return SkillInstallResult{}, err
		}
	}
	var combined SkillInstallResult
	for _, target := range targets {
		installed, err := writeSkillTarget(target)
		if err != nil {
			return SkillInstallResult{}, err
		}
		if combined.Path == "" {
			combined.Path = installed.Path
			combined.Files = installed.Files
		}
		combined.Paths = append(combined.Paths, installed.Path)
	}
	return combined, nil
}

func resolveSkillTarget(base string) (string, error) {
	absBase, err := filepath.Abs(base)
	if err != nil {
		return "", fmt.Errorf("resolve skills directory: %w", err)
	}
	return filepath.Join(absBase, "hop"), nil
}

func canonicalPathKey(path string) (string, error) {
	if err := preflightSkillAncestors(path); err != nil {
		return "", err
	}
	current := filepath.Clean(path)
	var suffix []string
	for {
		resolved, err := filepath.EvalSymlinks(current)
		if err == nil {
			for index := len(suffix) - 1; index >= 0; index-- {
				resolved = filepath.Join(resolved, suffix[index])
			}
			return filepath.Clean(resolved), nil
		}
		if !os.IsNotExist(err) {
			return "", fmt.Errorf("canonicalize skill destination: %w", err)
		}
		parent := filepath.Dir(current)
		if parent == current {
			return filepath.Clean(path), nil
		}
		suffix = append(suffix, filepath.Base(current))
		current = parent
	}
}

func preflightSkillTarget(target string, force bool) error {
	if err := preflightSkillAncestors(target); err != nil {
		return err
	}
	if info, err := os.Lstat(target); err == nil {
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("refusing to install through symlink %s", target)
		}
		if !info.IsDir() {
			return fmt.Errorf("skill target exists and is not a directory: %s", target)
		}
		if !force {
			return fmt.Errorf("Hop skill already exists at %s; pass --force to update it", target)
		}
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("inspect skill target: %w", err)
	}
	return fs.WalkDir(hopskill.Files, ".", func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil || path == "." {
			return walkErr
		}
		destination := filepath.Join(target, filepath.FromSlash(path))
		info, err := os.Lstat(destination)
		if os.IsNotExist(err) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("inspect skill destination %s: %w", destination, err)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("refusing to overwrite symlink %s", destination)
		}
		if entry.IsDir() && !info.IsDir() {
			return fmt.Errorf("skill directory destination is not a directory: %s", destination)
		}
		if !entry.IsDir() && info.IsDir() {
			return fmt.Errorf("skill file destination is a directory: %s", destination)
		}
		return nil
	})
}

// preflightSkillAncestors finds the nearest existing path component before any
// writes. This distinguishes an ordinary missing destination from a dangling
// parent symlink, which filepath.EvalSymlinks otherwise reports as os.ErrNotExist.
func preflightSkillAncestors(path string) error {
	target := filepath.Clean(path)
	current := target
	for {
		info, err := os.Lstat(current)
		if err == nil {
			if info.Mode()&os.ModeSymlink != 0 {
				resolved, resolveErr := filepath.EvalSymlinks(current)
				if resolveErr != nil {
					return fmt.Errorf("resolve skill destination ancestor %s: %w", current, resolveErr)
				}
				resolvedInfo, statErr := os.Stat(resolved)
				if statErr != nil {
					return fmt.Errorf("inspect resolved skill destination ancestor %s: %w", current, statErr)
				}
				if current != target && !resolvedInfo.IsDir() {
					return fmt.Errorf("skill destination ancestor is not a directory: %s", current)
				}
			} else if current != target && !info.IsDir() {
				return fmt.Errorf("skill destination ancestor is not a directory: %s", current)
			}
			return nil
		}
		if !os.IsNotExist(err) {
			return fmt.Errorf("inspect skill destination ancestor %s: %w", current, err)
		}
		parent := filepath.Dir(current)
		if parent == current {
			return nil
		}
		current = parent
	}
}

func writeSkillTarget(target string) (SkillInstallResult, error) {
	if err := os.MkdirAll(target, 0o755); err != nil {
		return SkillInstallResult{}, fmt.Errorf("create skill target: %w", err)
	}

	result := SkillInstallResult{Path: target, Paths: []string{target}}
	err := fs.WalkDir(hopskill.Files, ".", func(path string, entry fs.DirEntry, walkErr error) error {
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
