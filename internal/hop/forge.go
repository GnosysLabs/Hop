package hop

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"strings"
)

type ForgeKind string

const (
	ForgeGitHub  ForgeKind = "github"
	ForgeGitLab  ForgeKind = "gitlab"
	ForgeGitea   ForgeKind = "gitea"
	ForgeGeneric ForgeKind = "generic"
)

type ForgeInfo struct {
	Provider   ForgeKind `json:"provider"`
	Host       string    `json:"host,omitempty"`
	Repository string    `json:"repository,omitempty"`
	Remote     string    `json:"remote,omitempty"`
	CLI        string    `json:"collaboration_cli,omitempty"`
}

var stableForgeCommands = map[string]struct{}{
	"clone": {}, "whoami": {}, "issues": {}, "issue": {}, "i": {}, "pulls": {}, "pull": {}, "pr": {},
	"labels": {}, "releases": {}, "release": {}, "repos": {}, "actions": {}, "api": {},
	"open": {}, "notifications": {}, "ssh-keys": {}, "ssh-key": {},
}

func isForgeCommand(name string) bool {
	if _, ok := stableForgeCommands[name]; ok {
		return true
	}
	return isTeaCompatibleCommand(name)
}

func forgeKindForHost(host, override string) ForgeKind {
	switch strings.ToLower(strings.TrimSpace(override)) {
	case "github":
		return ForgeGitHub
	case "gitlab":
		return ForgeGitLab
	case "gitea":
		return ForgeGitea
	case "generic", "git":
		return ForgeGeneric
	}
	switch strings.ToLower(strings.TrimSpace(host)) {
	case "github.com":
		return ForgeGitHub
	case "gitlab.com":
		return ForgeGitLab
	case "githop.xyz":
		return ForgeGitea
	default:
		return ForgeGeneric
	}
}

func parseForgeLoginTarget(raw string) (ForgeInfo, error) {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || parsed.Hostname() == "" {
		return ForgeInfo{}, errors.New("forge login target must be an absolute URL")
	}
	host := strings.ToLower(parsed.Hostname())
	return ForgeInfo{Provider: forgeKindForHost(host, ""), Host: host}, nil
}

func detectForge(ctx context.Context, root string) (ForgeInfo, error) {
	info := ForgeInfo{Provider: forgeKindForHost("", os.Getenv("HOP_FORGE"))}
	repository, err := OpenRepository(root)
	if err != nil {
		return info, nil
	}
	remoteName := ""
	if destination, configured, discoverErr := repository.pushDestination(ctx); discoverErr != nil {
		return info, discoverErr
	} else if configured {
		remoteName = destination.remote
	}
	if remoteName == "" {
		output, listErr := repository.run(ctx, nil, nil, "remote")
		if listErr != nil {
			return info, listErr
		}
		remotes := nonemptyLines(output)
		switch {
		case containsString(remotes, "origin"):
			remoteName = "origin"
		case len(remotes) == 1:
			remoteName = remotes[0]
		default:
			return info, nil
		}
	}
	raw, err := repository.run(ctx, nil, nil, "remote", "get-url", "--push", remoteName)
	if err != nil {
		return info, err
	}
	parsed, err := parsePromptRemote(strings.TrimSpace(raw))
	if err != nil {
		info.Remote = remoteName
		return info, nil
	}
	info.Host = parsed.Host
	info.Repository = parsed.Repository.Owner + "/" + parsed.Repository.Name
	info.Remote = remoteName
	info.Provider = forgeKindForHost(info.Host, os.Getenv("HOP_FORGE"))
	switch info.Provider {
	case ForgeGitHub:
		info.CLI = "gh"
	case ForgeGitLab:
		info.CLI = "glab"
	case ForgeGitea:
		info.CLI = "embedded"
	}
	return info, nil
}

func runHostCLI(ctx context.Context, jsonOutput bool, stdout, stderr io.Writer) int {
	info, err := detectForge(ctx, ".")
	if err != nil {
		return printCLIError(fmt.Errorf("detect Git host: %w", err), jsonOutput, stdout, stderr)
	}
	if jsonOutput {
		writeJSON(stdout, map[string]any{"ok": true, "data": info})
		return 0
	}
	fmt.Fprintf(stdout, "Provider: %s\n", info.Provider)
	if info.Host != "" {
		fmt.Fprintf(stdout, "Host: %s\n", info.Host)
	}
	if info.Repository != "" {
		fmt.Fprintf(stdout, "Repository: %s\n", info.Repository)
	}
	if info.Remote != "" {
		fmt.Fprintf(stdout, "Remote: %s\n", info.Remote)
	}
	if info.CLI != "" {
		fmt.Fprintf(stdout, "Collaboration adapter: %s\n", info.CLI)
	}
	return 0
}

func runForgeCommand(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	info, err := detectForge(ctx, ".")
	if err != nil {
		return printCLIError(fmt.Errorf("detect Git host: %w", err), false, stdout, stderr)
	}
	if len(args) == 0 {
		return 2
	}
	switch info.Provider {
	case ForgeGitea:
		return runTeaCompatibleCLI(ctx, args, stdin, stdout, stderr, NewAuthClient())
	case ForgeGitHub:
		translated, ok := githubCommand(args)
		if !ok {
			return unsupportedForgeCommand(info, args[0], stderr)
		}
		return runProviderCLI(ctx, "gh", translated, stdin, stdout, stderr)
	case ForgeGitLab:
		translated, ok := gitlabCommand(args)
		if !ok {
			return unsupportedForgeCommand(info, args[0], stderr)
		}
		return runProviderCLI(ctx, "glab", translated, stdin, stdout, stderr)
	default:
		return unsupportedForgeCommand(info, args[0], stderr)
	}
}

func githubCommand(args []string) ([]string, bool) {
	rest := args[1:]
	switch args[0] {
	case "clone":
		return append([]string{"repo", "clone"}, rest...), true
	case "whoami":
		return []string{"api", "user", "--jq", ".login"}, true
	case "issues", "issue", "i":
		return append([]string{"issue"}, rest...), true
	case "pulls", "pull", "pr":
		return append([]string{"pr"}, rest...), true
	case "labels":
		return append([]string{"label"}, rest...), true
	case "releases", "release":
		return append([]string{"release"}, rest...), true
	case "repos":
		return append([]string{"repo"}, rest...), true
	case "actions":
		return append([]string{"run"}, rest...), true
	case "api":
		return append([]string{"api"}, rest...), true
	case "open":
		return append([]string{"repo", "view", "--web"}, rest...), true
	case "notifications":
		return append([]string{"api", "notifications"}, rest...), true
	case "ssh-keys", "ssh-key":
		return append([]string{"ssh-key"}, rest...), true
	default:
		return nil, false
	}
}

func gitlabCommand(args []string) ([]string, bool) {
	rest := args[1:]
	switch args[0] {
	case "clone", "repos":
		return append([]string{"repo"}, rest...), true
	case "whoami":
		return []string{"api", "user"}, true
	case "issues", "issue", "i":
		return append([]string{"issue"}, rest...), true
	case "pulls", "pull", "pr":
		return append([]string{"mr"}, rest...), true
	case "releases", "release":
		return append([]string{"release"}, rest...), true
	case "actions":
		return append([]string{"ci"}, rest...), true
	case "api":
		return append([]string{"api"}, rest...), true
	default:
		return nil, false
	}
}

func runProviderCLI(ctx context.Context, binary string, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if _, err := exec.LookPath(binary); err != nil {
		fmt.Fprintf(stderr, "hop: %s is not installed; core Hop and Git workflows still work, but this collaboration command requires %s\n", binary, binary)
		return 1
	}
	command := exec.CommandContext(ctx, binary, args...)
	command.Stdin, command.Stdout, command.Stderr = stdin, stdout, stderr
	if err := command.Run(); err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return exitErr.ExitCode()
		}
		fmt.Fprintf(stderr, "hop: run %s: %v\n", binary, err)
		return 1
	}
	return 0
}

func runProviderAuthCLI(ctx context.Context, provider ForgeKind, host string, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if host == "" {
		if provider == ForgeGitHub {
			host = "github.com"
		} else {
			host = "gitlab.com"
		}
	}
	binary := "gh"
	if provider == ForgeGitLab {
		binary = "glab"
	}
	var translated []string
	switch args[0] {
	case "login":
		if len(args) > 2 {
			fmt.Fprintln(stderr, "usage: hop auth login [FORGE_URL]")
			return 2
		}
		translated = []string{"auth", "login", "--hostname", host}
		if provider == ForgeGitHub {
			translated = append(translated, "--web", "--git-protocol", "https")
		}
	case "status":
		if len(args) != 1 {
			return 2
		}
		translated = []string{"auth", "status", "--hostname", host}
	case "logout":
		if len(args) != 1 {
			return 2
		}
		translated = []string{"auth", "logout", "--hostname", host}
	default:
		return 2
	}
	return runProviderCLI(ctx, binary, translated, stdin, stdout, stderr)
}

func unsupportedForgeCommand(info ForgeInfo, command string, stderr io.Writer) int {
	host := info.Host
	if host == "" {
		host = "the configured Git remote"
	}
	fmt.Fprintf(stderr, "hop: %q is not available for %s (%s); core Hop version-control commands remain fully available\n", command, host, info.Provider)
	return 2
}
