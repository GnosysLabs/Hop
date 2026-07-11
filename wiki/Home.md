# Hop documentation

Hop is prompt-native version control for coding agents. It stores each prompt
as an immutable project state, gives agent work an isolated workspace, validates
the exact tree being accepted, and safely materializes accepted results into the
visible project folder.

## Start here

- [Installation](Installation)
- [Getting started](Getting-Started)
- [Core concepts](Core-Concepts)
- [Agent integrations and workflow](Agent-Workflow)
- [Parallel agents and conflict resolution](Parallel-Agents-and-Conflicts)

## Reference and operations

- [CLI reference](CLI-Reference)
- [Architecture](Architecture)
- [Security and privacy](Security-and-Privacy)
- [Troubleshooting](Troubleshooting)
- [Upgrading and uninstalling](Upgrading-and-Uninstalling)
- [Release checklist](Release-Checklist)

## Thirty-second install

macOS or Linux:

```bash
curl -fsSL https://githop.xyz/GnosysLabs/Hop/raw/branch/main/scripts/install.sh | sh
```

Windows PowerShell:

```powershell
irm https://githop.xyz/GnosysLabs/Hop/raw/branch/main/scripts/install.ps1 | iex
```

Open a Git project in a compatible agent client and work normally. The installed
skill bundle activates Hop before the agent inspects or changes the project;
controllers can invoke the same protocol directly. Codex Desktop is a bundled
integration, and there is no required manual `hop init` step.

Hop is currently an alpha. Keep Git history and normal backups, read release
notes before upgrading, and report unexpected behavior with `hop doctor` output
after removing private paths or data.
