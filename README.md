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
- **Publishing is automatic.** When an upstream branch exists, each accepted
  transition is pushed without moving the local branch or force-pushing.
- **History stays local by default.** Detected credentials are redacted before
  prompts and evidence are persisted. Optional authenticated sync keeps prompt
  history private to the signed-in forge account and never puts it in Git.

## How it works

```text
prompt → durable intent → isolated agent work → validate + merge → accepted code
```

Hop ships an open [Agent Skills](https://agentskills.io/) bundle and a controller
protocol. The skill makes prompt capture the agent's first repository action; a
controller can capture before model delivery. Both use the same state,
workspace, validation, reconciliation, and landing protocol. Codex Desktop is
one bundled integration, not a boundary of the product.

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

The installer adds the Hop CLI and writes the same embedded skill version to
`~/.agents/skills/hop` and `${CODEX_HOME:-~/.codex}/skills/hop`. For CLI-only
installation, source builds, version pinning, custom locations, and
verification, see the
[installation guide](https://githop.xyz/GnosysLabs/Hop/wiki/Installation).

## Get started

1. Install Hop.
2. Open a Git project in an Agent Skills-compatible client, or make it the
   controller's working directory.
3. Ask the agent to make a change as you normally would.

That is the full user workflow. You do not run `hop init`, route prompts through
a terminal, or work inside `.hop` yourself. After a task, `hop status` shows the
accepted state and whether the visible project folder is synchronized.
When the repository has an unambiguous Git upstream, Hop also pushes the
accepted commit automatically; users do not run `git push` after each task.

To see private prompt history on a Hop-enabled Gitea forge, pair the CLI once:

```bash
hop auth login https://githop.xyz
```

Hop opens Gitea in the browser, uses OAuth Authorization Code + PKCE, and keeps
the resulting device credential in the operating-system keychain. Publishable
prompt records then sync after proposal, acceptance, and landing, and whenever
`hop sync` runs. Sync is idempotent and best-effort: offline failures never
block local work, and a later run resends from the private local database.

For example, Codex Desktop users restart Codex after installation, select a Git
project, and prompt normally. Other compatible runtimes can read the shared
skill bundle or receive a single-target installation with the explicit
`hop skill install --path /path/to/agent/skills --force` form.

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

## License

[MIT](LICENSE) © 2026 Gnosys Labs LLC.
