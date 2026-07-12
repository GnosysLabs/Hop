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
`~/.agents/skills/hop` and `${CODEX_HOME:-~/.codex}/skills/hop`. Compatible
runtimes can use the shared bundle. An explicit `--path` installs only to the
requested skills directory.

### Codex Desktop example

Restart Codex Desktop after installing or upgrading the skill, select a Git
project, and ask Codex to work normally. The skill is eligible for implicit
activation on every repository task; mention `$hop` for deterministic explicit
activation.

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

## Sync private prompt history

Prompt history is local by default. Pair Hop with a Hop-enabled Gitea forge if
you want the private web history view:

```bash
hop auth login https://githop.xyz
```

Your browser asks Gitea to authorize this device. After approval, verify the
connection or remove it with:

```bash
hop auth status
hop auth logout
```

The repository's Git remote determines where its prompt records belong. After
pairing, Hop syncs publishable records after `propose`, `accept`, and `land` and
as part of `hop sync`. Network failures only produce a warning; the local
database remains the retry source.

## Ask for review before landing

Automatic landing is the default because the original task authorizes the
local code change. To stop at a proposal, say one of the following in the task:

- `review first`
- `proposal only`
- `do not land`

## Connect another agent runtime

If a compatible runtime does not read `~/.agents/skills`, install the embedded
bundle into that runtime's skills directory:

```bash
hop skill install --path /path/to/agent/skills --force
```

Controllers that can persist a prompt before model delivery should use
`hop start`, `hop env`, and `hop prompt`; see [Agent workflow](Agent-Workflow).
