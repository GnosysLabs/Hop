# Getting started

## Use Hop with an agent integration

1. [Install Hop](Installation).
2. Select an existing Git project in a compatible agent client, or make it the
   controller's working directory.
3. Ask the agent to make a normal change.

That is the full user workflow. Do not manually create `.hop`, route the prompt
through a terminal, or tell the agent to work inside `.hop/workspaces`. A Hop
integration does that coordination for the agent.

Without `--path`, `hop skill install` writes the same Hop-managed skill files to
`~/.agents/skills/hop`, `${CODEX_HOME:-~/.codex}/skills/hop`, and
`~/.claude/skills/hop`. Compatible runtimes can use the shared bundle. An
explicit `--path` installs only to the requested skills directory.

### Codex Desktop example

Restart Codex Desktop after installing or upgrading the skill, select a Git
project, and ask Codex to work normally. The skill is eligible for implicit
activation on every repository task; mention `$hop` for deterministic explicit
activation.

### Claude Code example

The installer writes Hop to Claude Code's personal skill directory at
`~/.claude/skills/hop`. If that top-level skills directory did not exist when
Claude Code started, restart Claude Code after installation, then work normally
in any Git repository.

## What happens on the first task

Before reading or changing project files, the agent runs `hop begin`. Hop then:

- initializes local state without moving the Git branch or index;
- stores the prompt after redacting detected credentials;
- creates an isolated attempt workspace;
- returns the state and workspace to the agent; and
- keeps all project-changing work inside that workspace.

The agent validates, proposes, and lands the result. A successful `hop land`
updates the visible project folder to the accepted tree.

## Confirm the result

From the selected project directory:

```bash
hop status
hop history
hop doctor
```

A normal interactive result reports `Root: synchronized`.

If the active Git branch has an upstream—or the repository has one unambiguous
`origin`/single-remote destination—landing also fast-forward pushes the accepted
commit automatically. Hop never force-pushes. Repositories without a remote
remain local without treating that as an error.

## Use any Git host

Prompt history stays local in `.hop/`; no forge account is required. Hop uses
the repository's existing Git remote for fetch and automatic push. Inspect what
it detected with:

```bash
hop host
```

GitHub, GitLab, Gitea, generic SSH/HTTPS Git servers, and local bare remotes all
work for core version control. Issues, pull requests, and releases use an
optional host adapter. On GitHub, authenticate the standard `gh` CLI once; on
GitLab use `glab`; Gitea keeps an embedded compatibility adapter.

Common host-aware commands are:

```bash
hop issues list
hop pulls list
hop releases list
```

Hop never creates access tokens. It uses the user's existing provider CLI, OS
keychain, or Git credential helper.

## Ask for review before landing

Automatic landing is the default because the original task authorizes the
local code change. To stop at a proposal, say one of the following in the task:

- `review first`
- `proposal only`
- `do not land`

## Connect another agent runtime

If another compatible runtime does not read `~/.agents/skills`, install the embedded
bundle into that runtime's skills directory:

```bash
hop skill install --path /path/to/agent/skills --force
```

Controllers that can persist a prompt before model delivery should use
`hop start`, `hop env`, and `hop prompt`; see [Agent workflow](Agent-Workflow).
