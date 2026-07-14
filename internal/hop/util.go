package hop

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

const maxRecordedOutput = 128 * 1024

var ErrNotHopProject = errors.New("not inside a Hop project")

func FindHopRoot(start string) (string, error) {
	if configured := os.Getenv("HOP_ROOT"); configured != "" {
		return requireHopRoot(configured)
	}

	abs, err := filepath.Abs(start)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(abs)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		abs = filepath.Dir(abs)
	}
	gitRoot, err := FindRepositoryRoot(abs)
	if err != nil {
		return "", fmt.Errorf("%w; run 'hop init' first", ErrNotHopProject)
	}
	gitRoot = canonicalExistingPath(gitRoot)
	if exists, err := hopDatabaseExists(gitRoot); err != nil {
		return "", err
	} else if exists {
		return gitRoot, nil
	}
	// Hop-created worktrees intentionally live below the canonical repository's
	// private .hop directory. They are the only case allowed to cross the
	// current Git root while discovering Hop state.
	if managedRoot, ok := managedHopRoot(gitRoot); ok {
		if exists, err := hopDatabaseExists(managedRoot); err != nil {
			return "", err
		} else if exists {
			return managedRoot, nil
		}
	}
	return "", fmt.Errorf("%w; run 'hop init' first", ErrNotHopProject)
}

func hopDatabaseExists(root string) (bool, error) {
	_, err := os.Stat(filepath.Join(root, ".hop", "hop.db"))
	if err == nil {
		return true, nil
	}
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	return false, err
}

func canonicalExistingPath(path string) string {
	resolved, err := filepath.EvalSymlinks(path)
	if err == nil {
		return filepath.Clean(resolved)
	}
	return filepath.Clean(path)
}

func managedHopRoot(gitRoot string) (string, bool) {
	collection := filepath.Dir(gitRoot)
	dotHop := filepath.Dir(collection)
	if filepath.Base(dotHop) != ".hop" {
		return "", false
	}
	switch filepath.Base(collection) {
	case "workspaces", "checks", "integration":
		return canonicalExistingPath(filepath.Dir(dotHop)), true
	default:
		return "", false
	}
}

func requireHopRoot(root string) (string, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	if _, err := os.Stat(filepath.Join(abs, ".hop", "hop.db")); err != nil {
		return "", fmt.Errorf("HOP_ROOT does not contain .hop/hop.db: %s", abs)
	}
	return abs, nil
}

func digestState(state State, parents []Parent) (string, error) {
	copyState := state
	copyState.Digest = ""
	copyState.Parents = append([]Parent(nil), parents...)
	sort.Slice(copyState.Parents, func(i, j int) bool {
		if copyState.Parents[i].Order != copyState.Parents[j].Order {
			return copyState.Parents[i].Order < copyState.Parents[j].Order
		}
		if copyState.Parents[i].Role != copyState.Parents[j].Role {
			return copyState.Parents[i].Role < copyState.Parents[j].Role
		}
		return copyState.Parents[i].StateID < copyState.Parents[j].StateID
	})
	payload, err := json.Marshal(copyState)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(payload)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

type commandResult struct {
	ExitCode int
	Output   string
}

func runWorkspaceCommand(ctx context.Context, workspace string, env []string, argv []string) (commandResult, error) {
	if len(argv) == 0 {
		return commandResult{}, fmt.Errorf("no command provided")
	}
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Dir = workspace
	cmd.Env = append(os.Environ(), env...)
	var output bytes.Buffer
	cmd.Stdout = &output
	cmd.Stderr = &output
	err := cmd.Run()
	result := commandResult{Output: truncateRecordedOutput(output.String())}
	if err == nil {
		return result, nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		result.ExitCode = exitErr.ExitCode()
		return result, nil
	}
	return result, err
}

func truncateRecordedOutput(output string) string {
	if len(output) <= maxRecordedOutput {
		return output
	}
	const marker = "\n… output truncated by Hop …\n"
	keep := maxRecordedOutput - len(marker)
	return output[:keep/2] + marker + output[len(output)-(keep-keep/2):]
}

func acquireProjectLock(ctx context.Context, root, name string) (func(), error) {
	path := filepath.Join(root, ".hop", name+".lock")
	return acquireFileLock(ctx, path, "Hop "+name)
}

func acquireFileLock(ctx context.Context, path, description string) (func(), error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("create %s lock directory: %w", description, err)
	}
	for {
		file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err == nil {
			_, _ = file.WriteString(fmt.Sprintf("pid=%d\n", os.Getpid()))
			_ = file.Close()
			stopHeartbeat := make(chan struct{})
			go func() {
				ticker := time.NewTicker(30 * time.Second)
				defer ticker.Stop()
				for {
					select {
					case <-ticker.C:
						now := time.Now()
						_ = os.Chtimes(path, now, now)
					case <-stopHeartbeat:
						return
					}
				}
			}()
			var once sync.Once
			return func() {
				once.Do(func() {
					close(stopHeartbeat)
					_ = os.Remove(path)
				})
			}, nil
		}
		if !errors.Is(err, fs.ErrExist) {
			return nil, err
		}
		if info, statErr := os.Stat(path); statErr == nil && time.Since(info.ModTime()) > 5*time.Minute {
			_ = os.Remove(path)
			continue
		}
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("wait for %s lock: %w", description, ctx.Err())
		case <-time.After(50 * time.Millisecond):
		}
	}
}

func repositoryInitLockPath(path string) (string, error) {
	cache, err := os.UserCacheDir()
	if err != nil {
		return "", fmt.Errorf("locate user cache for repository initialization lock: %w", err)
	}
	canonical, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", fmt.Errorf("resolve repository initialization path: %w", err)
	}
	digest := sha256.Sum256([]byte(filepath.Clean(canonical)))
	return filepath.Join(cache, "hop", "locks", "repository-"+hex.EncodeToString(digest[:])+".lock"), nil
}

func shellQuote(argv []string) string {
	quoted := make([]string, len(argv))
	for i, arg := range argv {
		if arg != "" && !strings.ContainsAny(arg, " \t\n\"'\\$`;&|<>()[]{}*?!") {
			quoted[i] = arg
			continue
		}
		quoted[i] = "'" + strings.ReplaceAll(arg, "'", "'\"'\"'") + "'"
	}
	return strings.Join(quoted, " ")
}
