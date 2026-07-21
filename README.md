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
  clean commits from other tools become concurrent inputs automatically, and
  genuine conflicts return to an agent for reconciliation.
- **Validation follows the code.** Checks run against immutable work and the
  exact final tree before it becomes accepted.
- **Accepted work is visible and Git stays truthful.** Successful results appear
  in the selected project folder; when safe, Hop fast-forwards the intended
  local branch/index so ordinary Git status stays clean.
- **Publishing is automatic.** When an upstream branch exists, each accepted
  transition is pushed without force-pushing.
- **History stays local by default.** Detected credentials are redacted before
  prompts and evidence are persisted. Optional authenticated sync keeps prompt
  history private and never puts it in Git.

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
curl -fsSL https://raw.githubusercontent.com/GnosysLabs/Hop/main/scripts/install.sh | sh
```

### Windows PowerShell

```powershell
irm https://raw.githubusercontent.com/GnosysLabs/Hop/main/scripts/install.ps1 | iex
```

The installer adds the Hop CLI and writes the same embedded skill version to
`~/.agents/skills/hop`, `${CODEX_HOME:-~/.codex}/skills/hop`, and
`~/.claude/skills/hop`. For CLI-only installation, source builds, version
pinning, custom locations, and verification, see the
[installation guide](wiki/Installation.md).

After the first installation, Hop upgrades itself and refreshes its agent skill:

```bash
hop update
```

The standalone installers remain available as a no-Node bootstrap and recovery
path. Package-manager installations should be upgraded by their package manager.

## Get started

1. Install Hop.
2. Open a Git project in an Agent Skills-compatible client, or make it the
   controller's working directory.
3. Ask the agent to make a change as you normally would.

That is the full user workflow. You do not run `hop init`, route prompts through
a terminal, or work inside `.hop` yourself. After a task, `hop status` shows the
accepted state, branch projection, real changes, and durable publication state.
Ordinary Git status is normally clean after landing. If safety checks block the
branch/index update, `hop status` distinguishes the remaining projection from
real work and `hop sync-git` explains the safe next action.

Before an interactive agent returns its answer, `hop complete` records the
concise outcome and exact final response against that prompt. This works for
code changes, read-only diagnostics, and external operations alike, and makes
both fields available to private prompt-history sync.
When the repository has an unambiguous Git upstream, Hop also pushes the
accepted commit automatically; users do not run `git push` after each task.

Hop works with any normal Git remote: GitHub, GitLab, Gitea, SSH servers, and
local bare repositories. `hop host` reports the detected provider. Core
commands such as `hop land`, `hop push`, and `hop push-tag` use Git itself and
do not require a forge API or hosted CI.

Optional collaboration commands adapt to the host. On GitHub they use the
existing authenticated `gh` CLI; on GitLab they use `glab`; Gitea retains its
embedded adapter. Hop never creates access tokens. Use the provider's normal
credential setup, such as `gh auth login`, and keep secrets in the OS keychain.

For example, Codex Desktop users restart Codex after installation, select a Git
project, and prompt normally. Other compatible runtimes can read the shared
skill bundle or receive a single-target installation with the explicit
`hop skill install --path /path/to/agent/skills --force` form.

Hop follows semantic versioning. Backward-incompatible state-model or CLI
changes require a new major release.

## Documentation

- [Getting started](wiki/Getting-Started.md)
- [Agent workflow](wiki/Agent-Workflow.md)
- [Parallel agents and conflicts](wiki/Parallel-Agents-and-Conflicts.md)
- [Core concepts](wiki/Core-Concepts.md)
- [CLI reference](wiki/CLI-Reference.md)
- [Security and privacy](wiki/Security-and-Privacy.md)
- [Architecture](wiki/Architecture.md)
- [Product blueprint](docs/product-blueprint.md)

## License

[MIT](LICENSE) © 2026 Gnosys Labs LLC.
