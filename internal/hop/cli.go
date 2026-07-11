package hop

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"runtime/debug"
	"strings"
)

const usageText = `Hop — prompt-native version control

Usage:
  hop init [path]
  hop begin [--agent NAME] [--session ID] [--from STATE] (--stdin | --heredoc | "instruction")
  hop prompt [--from STATE] [--agent NAME] (--stdin | --heredoc | "instruction")
  hop checkpoint STATE
  hop check STATE -- COMMAND [ARG...]
  hop propose [--summary TEXT] STATE
  hop accept STATE [-- COMMAND [ARG...]]
  hop land STATE [-- COMMAND [ARG...]]
  hop refresh PROPOSAL
  hop sync
  hop status
  hop graph
  hop state STATE
  hop env STATE
  hop diff STATE
  hop history
  hop undo
  hop doctor [--repair]
  hop skill install [--path SKILLS_DIR] [--force]
  hop skill print
  hop version

Add --json anywhere for machine-readable output.
`

// Version is replaced by GoReleaser through -ldflags. Source installs made by
// `go install module@version` fall back to the module version in Go build info.
var Version = "dev"

func effectiveVersion() string {
	if Version != "" && Version != "dev" {
		return strings.TrimPrefix(Version, "v")
	}
	if info, ok := debug.ReadBuildInfo(); ok && info.Main.Version != "" && info.Main.Version != "(devel)" {
		return strings.TrimPrefix(info.Main.Version, "v")
	}
	return "dev"
}

func RunCLI(args []string, stdout, stderr io.Writer) int {
	return RunCLIWithInput(args, os.Stdin, stdout, stderr)
}

func RunCLIWithInput(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	jsonOutput, args := removeFlag(args, "--json")
	if len(args) == 0 || args[0] == "help" || args[0] == "--help" || args[0] == "-h" {
		fmt.Fprint(stdout, usageText)
		return 0
	}

	ctx := context.Background()
	command := args[0]
	commandArgs := args[1:]
	if command == "version" || command == "--version" {
		version := effectiveVersion()
		if jsonOutput {
			writeJSON(stdout, map[string]any{"ok": true, "version": version})
		} else {
			fmt.Fprintf(stdout, "hop %s\n", version)
		}
		return 0
	}
	if command == "skill" {
		return runSkillCLI(commandArgs, jsonOutput, stdout, stderr)
	}

	if command == "init" {
		path := "."
		if len(commandArgs) > 1 {
			fmt.Fprintln(stderr, "hop init accepts at most one path")
			return 2
		}
		if len(commandArgs) == 1 {
			path = commandArgs[0]
		}
		service, state, err := InitProject(ctx, path)
		if err != nil {
			return printCLIError(err, jsonOutput, stdout, stderr)
		}
		defer service.Close()
		if jsonOutput {
			writeJSON(stdout, map[string]any{"ok": true, "root": service.Root, "state": state})
		} else {
			fmt.Fprintf(stdout, "Initialized Hop at %s\nAccepted %s · tree %s\n", service.Root, state.ID, shortHash(state.SourceTree))
		}
		return 0
	}

	if command == "begin" {
		fs := flag.NewFlagSet("begin", flag.ContinueOnError)
		fs.SetOutput(stderr)
		from := fs.String("from", "", "continue from an explicit Hop state")
		sessionDefault := os.Getenv("CODEX_THREAD_ID")
		session := fs.String("session", sessionDefault, "stable interactive-agent session ID")
		agentDefault := os.Getenv("HOP_AGENT")
		if agentDefault == "" {
			if sessionDefault != "" {
				agentDefault = "codex"
			} else {
				agentDefault = "agent"
			}
		}
		agent := fs.String("agent", agentDefault, "agent or harness name")
		stdinPrompt := fs.Bool("stdin", false, "read exact prompt bytes from stdin")
		heredocPrompt := fs.Bool("heredoc", false, "read prompt from stdin and remove one shell-added final newline")
		if err := fs.Parse(commandArgs); err != nil {
			return 2
		}
		message, err := promptMessage(stdin, fs.Args(), *stdinPrompt, *heredocPrompt)
		if err != nil {
			fmt.Fprintf(stderr, "hop begin: %v\n", err)
			return 2
		}

		service, err := OpenProject(".")
		initialized := false
		if errors.Is(err, ErrNotHopProject) {
			service, _, err = InitProject(ctx, ".")
			initialized = err == nil
		}
		if err != nil {
			return printCLIError(err, jsonOutput, stdout, stderr)
		}
		defer service.Close()

		result, err := service.BeginPrompt(ctx, message, *from, *agent, *session)
		if err != nil {
			return printCLIError(err, jsonOutput, stdout, stderr)
		}
		begin := BeginResult{PromptResult: result, Initialized: initialized, SessionID: *session}
		if jsonOutput {
			writeJSON(stdout, map[string]any{"ok": true, "data": begin})
		} else {
			writeRedactionNotice(stderr, result.Redactions)
			if initialized {
				fmt.Fprintf(stdout, "Initialized Hop at %s\n", service.Root)
			}
			if result.Checkpoint != nil {
				fmt.Fprintf(stdout, "Checkpointed %s before the follow-up\n", result.Checkpoint.ID)
			}
			fmt.Fprintf(stdout, "Captured prompt state %s before project effects\nWorkspace: %s\n", result.Prompt.ID, result.Workspace)
			fmt.Fprintf(stdout, "Use HOP_STATE_ID=%s HOP_TASK_ID=%s HOP_ATTEMPT_ID=%s for this turn.\n", result.Prompt.ID, result.Task.ID, result.Attempt.ID)
		}
		return 0
	}

	service, err := OpenProject(".")
	if err != nil {
		return printCLIError(err, jsonOutput, stdout, stderr)
	}
	defer service.Close()

	var value any
	switch command {
	case "prompt", "start":
		fs := flag.NewFlagSet("prompt", flag.ContinueOnError)
		fs.SetOutput(stderr)
		from := fs.String("from", "", "continue an existing attempt")
		agent := fs.String("agent", "", "agent or harness name")
		stdinPrompt := fs.Bool("stdin", false, "read exact prompt bytes from stdin")
		heredocPrompt := fs.Bool("heredoc", false, "read prompt from stdin and remove one shell-added final newline")
		if err := fs.Parse(commandArgs); err != nil {
			return 2
		}
		message, err := promptMessage(stdin, fs.Args(), *stdinPrompt, *heredocPrompt)
		if err != nil {
			fmt.Fprintf(stderr, "hop prompt: %v\n", err)
			return 2
		}
		result, err := service.CreatePrompt(ctx, message, *from, *agent)
		if err != nil {
			return printCLIError(err, jsonOutput, stdout, stderr)
		}
		value = result
		if !jsonOutput {
			writeRedactionNotice(stderr, result.Redactions)
			if result.Checkpoint != nil {
				fmt.Fprintf(stdout, "Checkpointed %s before the follow-up\n", result.Checkpoint.ID)
			}
			fmt.Fprintf(stdout, "Created prompt state %s before delivery\nWorkspace: %s\n", result.Prompt.ID, result.Workspace)
			fmt.Fprintf(stdout, "Environment: HOP_ROOT=%s HOP_STATE_ID=%s HOP_TASK_ID=%s HOP_ATTEMPT_ID=%s\n", service.Root, result.Prompt.ID, result.Task.ID, result.Attempt.ID)
		}

	case "checkpoint":
		if len(commandArgs) != 1 {
			fmt.Fprintln(stderr, "usage: hop checkpoint STATE")
			return 2
		}
		state, err := service.Checkpoint(ctx, commandArgs[0])
		if err != nil {
			return printCLIError(err, jsonOutput, stdout, stderr)
		}
		value = state
		if !jsonOutput {
			fmt.Fprintf(stdout, "Created checkpoint %s · tree %s\n", state.ID, shortHash(state.SourceTree))
		}

	case "check":
		stateID, argv, ok := splitCommand(commandArgs)
		if !ok {
			fmt.Fprintln(stderr, "usage: hop check STATE -- COMMAND [ARG...]")
			return 2
		}
		check, err := service.RunCheck(ctx, stateID, argv)
		value = check
		if !jsonOutput {
			fmt.Fprintf(stdout, "$ %s\n%s", shellQuote(check.Command), check.Output)
			if check.Output != "" && !strings.HasSuffix(check.Output, "\n") {
				fmt.Fprintln(stdout)
			}
			fmt.Fprintf(stdout, "Check %s · exit %d · tree %s\n", check.ID, check.ExitCode, shortHash(check.TreeHash))
		}
		if err != nil {
			return printCLIError(err, jsonOutput, stdout, stderr)
		}

	case "propose", "submit":
		fs := flag.NewFlagSet("propose", flag.ContinueOnError)
		fs.SetOutput(stderr)
		summary := fs.String("summary", "", "result summary")
		if err := fs.Parse(commandArgs); err != nil {
			return 2
		}
		if len(fs.Args()) != 1 {
			fmt.Fprintln(stderr, "usage: hop propose [--summary TEXT] STATE")
			return 2
		}
		result, err := service.Propose(ctx, fs.Args()[0], *summary)
		if err != nil {
			return printCLIError(err, jsonOutput, stdout, stderr)
		}
		value = result
		if !jsonOutput {
			fmt.Fprintf(stdout, "Created proposal %s · tree %s · %d matching checks\n", result.Proposal.ID, shortHash(result.Proposal.SourceTree), len(result.Checks))
		}

	case "accept":
		stateID, argv, ok := splitOptionalCommand(commandArgs)
		if !ok {
			fmt.Fprintln(stderr, "usage: hop accept STATE [-- COMMAND [ARG...]]")
			return 2
		}
		result, err := service.Accept(ctx, stateID, argv)
		value = result
		if err != nil {
			return printCLIError(err, jsonOutput, stdout, stderr)
		}
		if !jsonOutput {
			fmt.Fprintf(stdout, "Accepted internally as %s · tree %s · visible root unchanged\n", result.State.ID, shortHash(result.State.SourceTree))
			if result.Check == nil {
				fmt.Fprintln(stdout, "No final-state validation command was supplied.")
			}
			for _, warning := range result.Warnings {
				fmt.Fprintf(stderr, "Warning: %s\n", warning)
			}
		}

	case "land":
		stateID, argv, ok := splitOptionalCommand(commandArgs)
		if !ok {
			fmt.Fprintln(stderr, "usage: hop land STATE [-- COMMAND [ARG...]]")
			return 2
		}
		result, err := service.Land(ctx, stateID, argv)
		value = result
		if err != nil {
			var conflict *ConflictError
			if errors.As(err, &conflict) {
				refresh, refreshErr := service.Refresh(ctx, stateID)
				if refreshErr != nil {
					preparationErr := fmt.Errorf("automatic merge conflict detected (%v), but reconciliation preparation failed: %v", err, refreshErr)
					return printCLIError(preparationErr, jsonOutput, stdout, stderr)
				}
				if jsonOutput {
					writeJSON(stdout, map[string]any{
						"ok":             false,
						"error":          err.Error(),
						"exit_code":      20,
						"conflict":       conflict,
						"reconciliation": refresh,
						"next_command":   "resolve the returned workspace, then check, propose, and land using the returned prompt state",
					})
					return 20
				}
				fmt.Fprintf(stderr, "hop: %v\n", err)
				printRefreshSummary(stdout, refresh)
				return 20
			}
			return printCLIError(err, jsonOutput, stdout, stderr)
		}
		if !jsonOutput {
			fmt.Fprintf(stdout, "Landed as %s · tree %s\n", result.State.ID, shortHash(result.State.SourceTree))
			fmt.Fprintf(stdout, "Synchronized visible root: %s\n", result.MaterializedRoot)
			if result.Check == nil {
				fmt.Fprintln(stdout, "No final-state validation command was supplied.")
			}
			for _, warning := range result.Warnings {
				fmt.Fprintf(stderr, "Warning: %s\n", warning)
			}
		}

	case "refresh", "reconcile":
		if len(commandArgs) != 1 {
			fmt.Fprintln(stderr, "usage: hop refresh PROPOSAL")
			return 2
		}
		result, err := service.Refresh(ctx, commandArgs[0])
		value = result
		if err != nil {
			return printCLIError(err, jsonOutput, stdout, stderr)
		}
		if !jsonOutput {
			printRefreshSummary(stdout, result)
		}

	case "sync":
		if len(commandArgs) != 0 {
			fmt.Fprintln(stderr, "usage: hop sync")
			return 2
		}
		result, err := service.Sync(ctx)
		value = result
		if err != nil {
			return printCLIError(err, jsonOutput, stdout, stderr)
		}
		if !jsonOutput {
			if result.Changed {
				fmt.Fprintf(stdout, "Synchronized %s to accepted state %s\n", result.Root, result.State.ID)
			} else {
				fmt.Fprintf(stdout, "Visible root already matches accepted state %s\n", result.State.ID)
			}
		}

	case "status":
		status, err := service.Status(ctx)
		if err != nil {
			return printCLIError(err, jsonOutput, stdout, stderr)
		}
		value = status
		if !jsonOutput {
			fmt.Fprintf(stdout, "Accepted: %s · tree %s\n", status.AcceptedHead.ID, shortHash(status.AcceptedHead.SourceTree))
			switch status.RootStatus {
			case "synchronized":
				fmt.Fprintln(stdout, "Root: synchronized")
			case "stale":
				fmt.Fprintf(stdout, "Root: stale at %s\n", status.RootStateID)
			default:
				fmt.Fprintln(stdout, "Root: diverged; Hop will not overwrite it")
			}
			if len(status.Attempts) == 0 {
				fmt.Fprintln(stdout, "No attempts yet.")
			}
			for _, attempt := range status.Attempts {
				fmt.Fprintf(stdout, "%s  %-10s  head=%s  %s\n", attempt.ID, attempt.Status, attempt.HeadStateID, attempt.Workspace)
			}
		}

	case "graph":
		rows, err := service.Graph(ctx)
		if err != nil {
			return printCLIError(err, jsonOutput, stdout, stderr)
		}
		value = rows
		if !jsonOutput {
			for _, row := range rows {
				parents := make([]string, 0, len(row.Parents))
				for _, parent := range row.Parents {
					parents = append(parents, parent.Role+"="+parent.StateID)
				}
				label := row.State.Summary
				if label == "" {
					label = row.State.Prompt
				}
				fmt.Fprintf(stdout, "%-10s %-28s %-50s %s\n", row.State.Kind, row.State.ID, strings.Join(parents, " "), label)
			}
		}

	case "state", "inspect":
		if len(commandArgs) != 1 {
			fmt.Fprintln(stderr, "usage: hop state STATE")
			return 2
		}
		state, err := service.State(ctx, commandArgs[0])
		if err != nil {
			return printCLIError(err, jsonOutput, stdout, stderr)
		}
		value = state
		if !jsonOutput {
			fmt.Fprintf(stdout, "%s %s\nTree: %s\nCommit: %s\nDigest: %s\n", state.Kind, state.ID, state.SourceTree, state.GitCommit, state.Digest)
			if state.Prompt != "" {
				fmt.Fprintf(stdout, "Prompt: %s\n", state.Prompt)
			}
			if state.Summary != "" {
				fmt.Fprintf(stdout, "Summary: %s\n", state.Summary)
			}
			for _, parent := range state.Parents {
				fmt.Fprintf(stdout, "Parent: %-18s %s\n", parent.Role, parent.StateID)
			}
		}

	case "env":
		if len(commandArgs) != 1 {
			fmt.Fprintln(stderr, "usage: hop env STATE")
			return 2
		}
		result, err := service.EnvironmentForState(ctx, commandArgs[0])
		if err != nil {
			return printCLIError(err, jsonOutput, stdout, stderr)
		}
		value = result
		if !jsonOutput {
			for _, name := range []string{"HOP_ROOT", "HOP_STATE_ID", "HOP_TASK_ID", "HOP_ATTEMPT_ID", "HOP_WORKSPACE"} {
				fmt.Fprintf(stdout, "export %s=%s\n", name, shellQuote([]string{result.Variables[name]}))
			}
			fmt.Fprintf(stdout, "cd %s\n", shellQuote([]string{result.Workspace}))
		}

	case "diff":
		if len(commandArgs) != 1 {
			fmt.Fprintln(stderr, "usage: hop diff STATE")
			return 2
		}
		diff, err := service.Diff(ctx, commandArgs[0])
		if err != nil {
			return printCLIError(err, jsonOutput, stdout, stderr)
		}
		value = map[string]string{"state": commandArgs[0], "diff": diff}
		if !jsonOutput {
			fmt.Fprint(stdout, diff)
		}

	case "history":
		states, err := service.History(ctx)
		if err != nil {
			return printCLIError(err, jsonOutput, stdout, stderr)
		}
		value = states
		if !jsonOutput {
			for _, state := range states {
				fmt.Fprintf(stdout, "%s  %s  %s\n", state.ID, shortHash(state.SourceTree), state.Summary)
			}
		}

	case "undo":
		if len(commandArgs) != 0 {
			fmt.Fprintln(stderr, "usage: hop undo")
			return 2
		}
		state, err := service.Undo(ctx)
		var committed *CommittedStateError
		if err != nil && !errors.As(err, &committed) {
			return printCLIError(err, jsonOutput, stdout, stderr)
		}
		if committed != nil {
			state = committed.State
			value = map[string]any{"state": state, "warnings": []string{committed.Error()}}
		} else {
			value = state
		}
		if !jsonOutput {
			fmt.Fprintf(stdout, "Created forward undo state %s · tree %s\n", state.ID, shortHash(state.SourceTree))
			if committed != nil {
				fmt.Fprintf(stderr, "Warning: %s\n", committed.Error())
			}
		}

	case "doctor":
		fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
		fs.SetOutput(stderr)
		repair := fs.Bool("repair", false, "repair the internal accepted ref")
		if err := fs.Parse(commandArgs); err != nil || len(fs.Args()) != 0 {
			return 2
		}
		report, err := service.Doctor(ctx, *repair)
		if err != nil {
			return printCLIError(err, jsonOutput, stdout, stderr)
		}
		value = report
		if !jsonOutput {
			if report.OK {
				fmt.Fprintf(stdout, "Hop project is healthy · %s\n", report.AcceptedState)
			} else {
				for _, problem := range report.Problems {
					fmt.Fprintf(stdout, "Problem: %s\n", problem)
				}
			}
		}

	default:
		fmt.Fprintf(stderr, "unknown command %q\n\n%s", command, usageText)
		return 2
	}

	if jsonOutput {
		writeJSON(stdout, map[string]any{"ok": true, "data": value})
	}
	return 0
}

func runSkillCLI(args []string, jsonOutput bool, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "usage: hop skill install [--path SKILLS_DIR] [--force] | hop skill print")
		return 2
	}
	switch args[0] {
	case "install":
		fs := flag.NewFlagSet("skill install", flag.ContinueOnError)
		fs.SetOutput(stderr)
		path := fs.String("path", "", "parent skills directory")
		force := fs.Bool("force", false, "update an existing Hop skill")
		if err := fs.Parse(args[1:]); err != nil || len(fs.Args()) != 0 {
			return 2
		}
		result, err := InstallSkill(*path, *force)
		if err != nil {
			return printCLIError(err, jsonOutput, stdout, stderr)
		}
		if jsonOutput {
			writeJSON(stdout, map[string]any{"ok": true, "data": result})
		} else {
			fmt.Fprintf(stdout, "Installed Hop skill at %s\n", result.Path)
		}
		return 0
	case "print":
		if len(args) != 1 {
			fmt.Fprintln(stderr, "usage: hop skill print")
			return 2
		}
		contents, err := EmbeddedSkillText()
		if err != nil {
			return printCLIError(err, jsonOutput, stdout, stderr)
		}
		if jsonOutput {
			writeJSON(stdout, map[string]any{"ok": true, "data": map[string]string{"skill": contents}})
		} else {
			fmt.Fprint(stdout, contents)
		}
		return 0
	default:
		fmt.Fprintf(stderr, "unknown skill command %q\n", args[0])
		return 2
	}
}

func printCLIError(err error, jsonOutput bool, stdout, stderr io.Writer) int {
	code := 1
	var conflict *ConflictError
	var rootConflict *RootConflictError
	var stale *StaleHeadError
	var failed *CheckFailedError
	switch {
	case errors.As(err, &conflict):
		code = 20
	case errors.As(err, &rootConflict):
		code = 23
	case errors.As(err, &stale):
		code = 21
	case errors.As(err, &failed):
		code = 22
	}
	if jsonOutput {
		writeJSON(stdout, map[string]any{"ok": false, "error": err.Error(), "exit_code": code})
	} else {
		fmt.Fprintf(stderr, "hop: %v\n", err)
		if conflict != nil && len(conflict.Paths) > 0 {
			fmt.Fprintln(stderr, "Merge conflicts:")
			for _, path := range conflict.Paths {
				fmt.Fprintf(stderr, "  %s\n", path)
			}
		}
		if rootConflict != nil && len(rootConflict.Paths) > 0 {
			fmt.Fprintln(stderr, "Visible-root conflicts:")
			for _, path := range rootConflict.Paths {
				fmt.Fprintf(stderr, "  %s\n", path)
			}
		}
	}
	return code
}

func printRefreshSummary(w io.Writer, result RefreshResult) {
	action := "Prepared"
	if result.Reused {
		action = "Reusing"
	}
	fmt.Fprintf(w, "%s reconciliation prompt %s\n", action, result.Prompt.ID)
	fmt.Fprintf(w, "Reconciliation attempt: %s\n", result.Attempt.ID)
	fmt.Fprintf(w, "Workspace: %s\n", result.Workspace)
	fmt.Fprintf(w, "Accepted input: %s · commit %s\n", result.AcceptedHead.ID, shortHash(result.AcceptedHead.GitCommit))
	fmt.Fprintf(w, "Proposal input: %s · commit %s\n", result.Proposal.ID, shortHash(result.Proposal.GitCommit))
	fmt.Fprintln(w, "Resolve these genuine merge conflicts while preserving both intents (structural conflicts may have no text markers):")
	for _, path := range result.Conflicts {
		fmt.Fprintf(w, "  %s\n", path)
	}
	fmt.Fprintf(w, "Continue automatically with: hop check %s -- <test-command>, then propose and land again.\n", result.Prompt.ID)
}

func removeFlag(args []string, wanted string) (bool, []string) {
	found := false
	filtered := make([]string, 0, len(args))
	for i, arg := range args {
		if arg == "--" {
			filtered = append(filtered, args[i:]...)
			break
		}
		if arg == wanted {
			found = true
			continue
		}
		filtered = append(filtered, arg)
	}
	return found, filtered
}

func splitCommand(args []string) (string, []string, bool) {
	if len(args) < 3 || args[1] != "--" {
		return "", nil, false
	}
	return args[0], args[2:], len(args[2:]) > 0
}

func splitOptionalCommand(args []string) (string, []string, bool) {
	if len(args) == 1 {
		return args[0], nil, true
	}
	if len(args) >= 3 && args[1] == "--" {
		return args[0], args[2:], true
	}
	return "", nil, false
}

const maxPromptBytes = 16 << 20

func promptMessage(stdin io.Reader, args []string, rawStdin, heredoc bool) (string, error) {
	if rawStdin && heredoc {
		return "", errors.New("use only one of --stdin or --heredoc")
	}
	if !rawStdin && !heredoc {
		if len(args) != 1 || strings.TrimSpace(args[0]) == "" {
			return "", errors.New("provide exactly one non-empty prompt argument, or use --stdin/--heredoc")
		}
		return args[0], nil
	}
	if len(args) != 0 {
		return "", errors.New("do not combine a prompt argument with --stdin or --heredoc")
	}
	data, err := io.ReadAll(io.LimitReader(stdin, maxPromptBytes+1))
	if err != nil {
		return "", fmt.Errorf("read prompt from stdin: %w", err)
	}
	if len(data) > maxPromptBytes {
		return "", fmt.Errorf("prompt exceeds %d bytes", maxPromptBytes)
	}
	message := string(data)
	if heredoc {
		if strings.HasSuffix(message, "\r\n") {
			message = strings.TrimSuffix(message, "\r\n")
		} else {
			message = strings.TrimSuffix(message, "\n")
		}
	}
	if strings.TrimSpace(message) == "" {
		return "", errors.New("prompt text is required")
	}
	return message, nil
}

func writeRedactionNotice(w io.Writer, redactions []PromptRedaction) {
	total := 0
	for _, redaction := range redactions {
		total += redaction.Count
	}
	if total == 0 {
		return
	}
	noun := "credentials"
	if total == 1 {
		noun = "credential"
	}
	fmt.Fprintf(w, "Warning: redacted %d potential %s before storing the prompt.\n", total, noun)
}

func writeJSON(w io.Writer, value any) {
	encoder := json.NewEncoder(w)
	encoder.SetIndent("", "  ")
	_ = encoder.Encode(value)
}

func shortHash(hash string) string {
	if len(hash) <= 12 {
		return hash
	}
	return hash[:12]
}
