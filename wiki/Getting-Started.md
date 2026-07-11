# Getting started

## Use Hop from Codex Desktop

1. [Install Hop](Installation).
2. Restart Codex Desktop if it was already open.
3. Select an existing Git project as the Codex working directory.
4. Ask Codex to make a normal change.

That is the full user workflow. Do not manually create `.hop`, route the prompt
through a terminal, or tell Codex to work inside `.hop/workspaces`. The Hop skill
does that coordination for the agent.

The skill is eligible for implicit activation on every repository task. Mention
`$hop` in the task if you want deterministic explicit activation.

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

A normal Desktop result reports `Root: synchronized`.

## Ask for review before landing

Automatic landing is the default because the original task authorizes the
local code change. To stop at a proposal, say one of the following in the task:

- `review first`
- `proposal only`
- `do not land`

## Use another agent runtime

Install the embedded skill into that runtime's skills directory:

```bash
hop skill install --path /path/to/agent/skills --force
```

Controllers that can persist a prompt before model delivery should use
`hop start`, `hop env`, and `hop prompt`; see [Agent workflow](Agent-Workflow).
