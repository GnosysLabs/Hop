# Hop

**Prompt-native version control for coding agents.**

Hop remembers why code changed, not just what changed. Every instruction becomes
a durable project state before an agent starts working. Agents get isolated
workspaces, validate the final result, and safely land accepted work in the
project folder you opened.

Git remains the source-tree and interoperability layer. Hop adds the context
agent workflows are missing: prompts, lineage, checkpoints, validation evidence,
and safe multi-agent integration.

## Why Hop

- **Intent is versioned.** Each prompt is connected to the code it produced.
- **Agents stay isolated.** Parallel tasks do not edit the same working folder.
- **Integration is intelligent.** Compatible changes merge automatically;
  genuine conflicts return to an agent for reconciliation.
- **Validation follows the code.** Checks run against immutable work and the
  exact final tree before it becomes accepted.
- **Accepted work is visible.** Successful results appear in the selected
  project folder without moving your active Git branch or index.
- **History stays local by default.** Detected credentials are redacted before
  prompts and evidence are persisted.

## How it works

```text
prompt → durable intent → isolated agent work → validate + merge → accepted code
```

You keep using Codex Desktop normally. The installed Hop skill handles this
lifecycle for the agent—there is no prompt wrapper, special app, or manual
branch workflow.

## Install

Hop requires Git 2.40 or newer.

### macOS and Linux

```bash
curl -fsSL https://githop.xyz/GnosysLabs/Hop/raw/branch/main/scripts/install.sh | sh
```

### Windows PowerShell

```powershell
irm https://githop.xyz/GnosysLabs/Hop/raw/branch/main/scripts/install.ps1 | iex
```

The installer adds both the Hop CLI and its Codex skill. For source builds,
version pinning, custom locations, and verification, see the
[installation guide](https://githop.xyz/GnosysLabs/Hop/wiki/Installation).

## Get started

1. Install Hop.
2. Restart Codex Desktop if it was already open.
3. Open a Git project in Codex Desktop.
4. Ask Codex to make a change as you normally would.

That is the full user workflow. You do not run `hop init`, route prompts through
a terminal, or work inside `.hop` yourself. After a task, `hop status` shows the
accepted state and whether the visible project folder is synchronized.

Hop is currently an early alpha. Expect its state model and CLI to evolve before
1.0.

## Documentation

- [Getting started](https://githop.xyz/GnosysLabs/Hop/wiki/Getting-Started)
- [Agent workflow](https://githop.xyz/GnosysLabs/Hop/wiki/Agent-Workflow)
- [Parallel agents and conflicts](https://githop.xyz/GnosysLabs/Hop/wiki/Parallel-Agents-and-Conflicts)
- [Core concepts](https://githop.xyz/GnosysLabs/Hop/wiki/Core-Concepts)
- [CLI reference](https://githop.xyz/GnosysLabs/Hop/wiki/CLI-Reference)
- [Security and privacy](https://githop.xyz/GnosysLabs/Hop/wiki/Security-and-Privacy)
- [Architecture](https://githop.xyz/GnosysLabs/Hop/wiki/Architecture)
- [Product blueprint](docs/product-blueprint.md)
