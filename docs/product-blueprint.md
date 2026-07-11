# Hop: prompt-native version control for coding agents

Concept blueprint v0.1 — July 11, 2026

## One-sentence thesis

Hop makes every prompt a durable, versioned project state, then provides the isolation, evidence, reconciliation, and policy needed to move selected prompt-states into the project’s accepted lineage.

The clean category distinction is:

- Agent orchestrators decide **who does the work**.
- Git records **what tree was saved**.
- Hop records **every intentful state**, then governs which of those states become accepted project history.

The initial product should be a Git-compatible change-control plane for coding agents, not a from-scratch replacement for Git storage or remotes.

## The user and the job

The first user is a solo technical founder, staff engineer, or tech lead supervising two to ten concurrent coding-agent tasks in one repository. They already use Git and GitHub, but they manually create worktrees, remember which agent owns what, inspect diffs, rebase stale results, run tests, clean up branches, and reconstruct why a change was made.

Their job-to-be-done is:

> When I delegate several changes to coding agents, help me run them safely in parallel, understand their live impact, and land or undo each result as one coherent unit—without manually coordinating branches, worktrees, context, and validation.

The first market should favor mixed-agent users. A developer using Codex, Claude Code, Cursor, and future agents has no neutral system of record spanning all of them.

## What Hop is—and is not

Hop is:

- A universal prompt-state graph, with tasks and proposals as useful views over it.
- An isolation and coordination layer for parallel work.
- An acceptance gateway that validates the final composed state.
- A provenance system connecting intent, execution, evidence, and resulting code.
- A structured project-knowledge system whose claims are sourced and versioned.
- A Git-compatible bridge to existing commits, remotes, pull requests, and CI.

Hop is not initially:

- A new content-addressed object store.
- A replacement for GitHub or pull requests.
- A general-purpose agent orchestrator.
- A promise to solve arbitrary semantic merge conflicts with an LLM.
- A distributed multi-repository database.
- A giant autonomous project wiki.

## The foundational model: every prompt is a state

Hop should not relegate prompts to metadata attached to code snapshots. Every prompt creates a durable state in the universal project graph, including prompts that change no files, fail, are cancelled, or only refine the project’s intent.

Acceptance does not make something a state. It creates another typed state in the same graph and determines what enters the canonical project lineage.

```text
A184  accepted project state
├── P185  prompt: “Add password reset”
│   └── C186  workspace checkpoint
│       └── P187  prompt: “Use Resend, not SendGrid”
│           └── R188  proposed result
└── P189  prompt: “Redesign account settings”
    └── R190  proposed result

A184 + R188  ──accept──>  A191
```

The prompt state is the exact project, workspace, and delivery context immediately after Hop durably accepts the instruction but before the agent causes any effects. Checkpoint and proposal states record its effects. This makes causality exact: no change can appear inside the state that supposedly caused it.

Two Hop states may reference the exact same source tree and still be distinct because their kind, prompt, context, evidence, occurrence, or parentage differs. This is the crucial departure from Git: the complete thing being versioned is not just source content, but **project intent plus project content plus the causal evidence connecting them**.

Tasks, attempts, and proposals remain useful, but they become views over the state graph:

- A **task** is a named subgraph describing one desired outcome.
- An **attempt** is a path through prompt-states produced by one agent approach.
- A **proposal** is a frozen descendant state containing the result of one or more prompts.
- “Try another approach” creates a sibling path from a chosen state.
- A follow-up prompt creates a child state, even if its source tree is unchanged.

This model preserves every turn while still supporting abandoned work, competing solutions, replay, and clean canonical history.

## The fundamental objects

### Project

The authority boundary containing accepted state, policies, component definitions, tasks, proposals, and knowledge.

### State

The primary Hop object. Hop uses one typed, immutable Merkle DAG. Every instruction becomes a `prompt` state before it is delivered. Work caused by that prompt produces descendant `checkpoint` and `proposal` states. Acceptance, replay, conflict resolution, and undo also produce states rather than rewriting old ones.

Initial state kinds:

- `prompt` — an instruction plus the exact context in which it was delivered
- `checkpoint` — immutable workspace progress at a tool or message boundary
- `proposal` — a frozen candidate outcome with evidence
- `accepted` — a canonical project revision and its acceptance receipt
- `replay`, `resolution`, `revert`, and `restore` — later specialized transitions

A materialized state manifest can contain:

```text
State {
  id
  occurrence_id
  kind
  parents[]: { role, state_id }
  canonical_anchor
  roots {
    project_tree
    knowledge
    policy
    conversation
    execution_context
    evidence
  }
  cause { prompt, attachments, actor, recipient }
  task_id
  attempt_id
  created_at
}
```

Parent edges have roles such as `run_parent`, `canonical_anchor`, `canonical_parent`, `proposal_parent`, and `caused_by`. This lets Hop derive a linear canonical history while tasks, subagents, alternatives, and conversations branch freely.

All roots point to immutable, content-addressed data. `occurrence_id` keeps two identical prompts sent twice as distinct historical occurrences. Structural sharing makes a no-code prompt cheap: it adds a prompt blob, a conversation node, and a small manifest while reusing the complete project, knowledge, and policy roots.

A mutable `accepted_head` pointer identifies the current canonical `accepted` state. Task and attempt heads use separate pointers. Prompt creation never moves `accepted_head`.

### Task

A named subgraph of related prompt-states representing one stable desired outcome, its acceptance criteria, competing attempts, and status. A prompt is never hidden inside the task record; it always has its own state identity.

### Attempt

One path through a task’s state subgraph, produced by a human or agent following a particular approach. It records the base state, agent and harness identity, environment fingerprint, declared scope, and outcome.

### Claim

An expiring declaration that an attempt expects to read or change a resource. Selectors may refer to a component, file, symbol, API route, database schema, package, generated artifact, or other named contract.

Claims should normally be advisory warnings. Hard locks should be reserved for genuinely non-mergeable resources.

### Proposal

A frozen descendant state containing the candidate result of an attempt: source and knowledge roots, computed delta, summary, observed impact, risk signals, evidence, and proposed knowledge changes. Further feedback creates a new prompt and a new proposal descendant; it never mutates the old proposal.

### Evidence

A test, lint, build, review, benchmark, or policy result bound to an exact proposal tree and environment. Evidence becomes stale if the proposal is refreshed or composed with other work.

### Acceptance

The immutable transaction that evaluates a proposal against the current accepted head, materializes the final combined roots, runs required policy checks, and creates a new `accepted` state.

The accepted state has a `canonical_parent` edge to the previous accepted state and a `proposal_parent` edge to the reconciled proposal. Even when no replay was necessary, it can reuse every proposal root and add only a tiny acceptance manifest. The canonical pointer advances with compare-and-swap; the prompt and proposal lineages are never rebased away or rewritten.

### Knowledge fact

A typed, sourced, versioned assertion about the project. Each fact has provenance, a state from which it is valid, a status, and optionally supersession or expiration information.

### Event

An append-only audit record such as task creation, claim change, instruction, proposal submission, evidence attachment, acceptance, rejection, refresh, or undo.

## The state transition protocol

Prompt creation and canonical acceptance are separate transitions:

1. A user submits a prompt while viewing run state `S`.
2. Hop quiesces the workspace at a tool/message boundary and seals any preceding edits as checkpoint `C`.
3. Hop creates immutable prompt state `P`, with `run_parent=C` and a `canonical_anchor` to the accepted state visible at creation.
4. Hop compare-and-swaps the run head to `P`; only then may it deliver the prompt to the agent.
5. The agent receives `HOP_STATE_ID=P`, works in an isolated workspace, and declares expiring claims.
6. Work becomes descendant checkpoints and eventually frozen proposal `R`; failure or cancellation also produces an addressable terminal state.
7. By default the agent immediately nominates `R` for automatic acceptance; an explicit review-first request pauses here. Hop reconciles it against the current accepted head in a temporary integration workspace.
8. Hop evaluates textual overlap, symbol and contract risk, policies, and required tests on the exact final roots.
9. Hop creates accepted state `A`, linked to both the previous accepted state and `R`, then atomically advances `accepted_head` with compare-and-swap.
10. In Desktop mode, Hop materializes `A` into the selected visible root only when that root still matches accepted history; it preserves HEAD and the real Git index and blocks rather than overwriting divergence.
11. Every prompt, checkpoint, proposal, and acceptance remains addressable regardless of later outcomes.

The important invariants are:

- **Every instruction delivered to an agent or subagent has an immutable prompt-state ID first.**
- **Effects caused by a prompt exist only in descendant states.**
- **A prompt-state is never destroyed because its result was rejected, superseded, or failed.**
- **Checks must pass on the exact state that will actually become accepted**, not merely on the attempt’s original source tree.

## Conflict handling

Hop should distinguish several kinds of interaction:

1. **Claim overlap** — two active attempts announce overlapping intent. This is an early warning, not proof of conflict.
2. **Textual conflict** — patches cannot be applied mechanically.
3. **Structural conflict** — changes touch the same symbol, API, schema, dependency, migration sequence, generated file, or contract.
4. **Behavioral risk** — different files compose cleanly but may be incompatible in behavior.
5. **Validation failure** — the integrated tree builds or tests incorrectly.

Non-overlapping files are not sufficient evidence of compatibility. Database migrations, API callers and implementations, dependency upgrades, generated outputs, and shared invariants often conflict across files.

“Semantic merge” should not initially mean that a model silently rewrites conflicting code. The safer operation is **refresh**:

> Give the original agent the newer accepted state, the intervening task summaries, the conflict packet, and its original intent; ask it to produce a new proposal.

For many agent tasks, replaying or regenerating against the latest state is cheaper and safer than preserving every old hunk.

## Acceptance policy

Automatic acceptance should require all of the following:

- The proposal applies to the current accepted state or refreshes cleanly.
- No protected path or contract requires manual review.
- No active exclusive claim is violated.
- Required deterministic checks pass on the final tree.
- The proposal remains inside configured risk thresholds.
- Any required human or policy approval is present.

For ordinary local agent work, the task prompt supplies the acceptance authority
and Hop should auto-accept after deterministic checks pass. A separate landing
prompt is unnecessary ceremony. Human approval remains available when the user
explicitly requests review first or project policy protects the affected scope.

Model-generated compatibility analysis can explain risk and select extra checks, but it should not be the sole authority for acceptance.

## Undo and history

Hop can make undo feel simple without pretending collaborative history is linear.

- If no later accepted state depends on a local change, a pointer rewind may be safe.
- Once other work builds on it, `hop undo <task>` should create and validate a compensating proposal against the current state.
- The original acceptance remains in the ledger; the undo is a new accepted transition.

The primary history view should show prompt-states grouped into tasks, not commits or hashes:

```text
P185  Add password reset emails
  └─ P186  Use Resend, not SendGrid
     └─ R187  Codex · 14 files · 8 checks passed
        ✓ accepted as A188

P189  Redesign account settings
  └─ R190  Claude · integrated with A188
     ✓ accepted as A191
```

Raw Git commits and hashes remain available for interoperability and debugging.

## Project knowledge

`PROJECT.md` should be a generated briefing, not a shared scratchpad that every agent rewrites.

Useful fact types include:

- purpose
- invariant
- architectural decision
- component responsibility
- public contract
- capability
- operational constraint
- known issue
- deprecated behavior

Every accepted fact should include:

- the task and proposal that introduced it
- supporting code locations or evidence
- the accepted state from which it is valid
- `accepted`, `superseded`, or `invalidated` status
- optional confidence and expiration rules

Hop should expose two views:

- `hop context --json` for complete machine-readable context
- generated `PROJECT.md` for a concise human and agent briefing

Dynamic active work belongs in `hop board` and `hop context`, not in a file copied into every workspace. This prevents unrelated tasks from dirtying their trees whenever another task changes status.

## Human and agent experience

The default human-facing loop should be four verbs, with Review as an opt-in
pause before acceptance:

```text
Ask → Work → Accept → Undo
             ↑
        optional Review
```

A plausible CLI:

```bash
hop init
hop begin --agent codex --heredoc          # agent first action in Codex Desktop
hop run --agent codex "Add password reset emails"
hop run --agent claude "Redesign account settings"
hop prompt P185 "Use Resend, not SendGrid"
hop board
hop graph
hop state show P186
hop show <task>
hop diff <task>
hop compare <task>                 # compare multiple attempts
hop land R187
hop refresh R187
hop undo <task>
hop why src/auth/reset.ts
```

The machine-facing protocol should be explicit and JSON-first:

```bash
hop context --json
hop state inspect "$HOP_STATE_ID" --json
hop task inspect "$HOP_TASK_ID" --json
hop claim add component:auth --mode write --ttl 20m --json
hop claim add api:'POST /password-reset' --mode write --ttl 20m --json
hop progress --summary-file progress.json --json
hop state checkpoint --manifest checkpoint.json --json
hop propose --manifest result.json --json
```

Hop supports two capture strengths. In Codex Desktop, the skill makes `hop begin` the agent's first project action, providing a practical pre-effect boundary without changing the user's prompt-box workflow. A trusted prompt hook or orchestrator can durably create the prompt-state, task/attempt grouping, and workspace before model delivery, then inject `HOP_STATE_ID`, `HOP_TASK_ID`, `HOP_ATTEMPT_ID`, and the workspace path. The skill teaches and applies the workflow; a hook or process boundary can enforce the stronger pre-delivery invariant.

## The MVP

The smallest complete product is a **parallel-agent landing queue** backed by Git.

### It must do

1. Initialize Hop inside an existing Git repository.
2. Turn every submitted prompt into a durable child state before project effects, with a pre-delivery mode where the harness supports it.
3. Create a task/attempt grouping and isolated Git worktree from the prompt’s parent state.
4. Attach Codex Desktop through a skill/session binding and support thin controller adapters for other agents.
5. Capture immutable checkpoint, proposal, failure, and cancellation states beneath each prompt-state.
6. Record claims, actual changed files, agent/environment identity, and status.
7. Show the universal state graph and detect claim/file overlap.
8. Nominate a sealed state with its structured summary, commands, and test evidence.
9. Materialize it on the current accepted head in an integration workspace.
10. Run configured checks on that exact final state.
11. Auto-accept successful ordinary local work without another user prompt,
    then advance the accepted head atomically, export a normal Git-compatible
    commit, and safely synchronize the visible Desktop root.
12. Undo an accepted state through a compensating prompt/integration state.
13. Generate a small `PROJECT.md` from accepted facts.
14. Install a vendor-neutral agent skill and expose stable JSON CLI output.
15. Redact credentials before prompt text reaches state digests, titles, events,
    validation evidence, or any durable database/write-ahead-log page.

### It should not do yet

- distributed Hop remotes
- multi-repository atomic tasks
- autonomous task decomposition
- general semantic merging
- organization identity and permissions
- a full hosted dashboard
- vector-database project memory
- symbol-level locking for every language
- bespoke content storage

### Pragmatic implementation

- Use Git blobs, trees, hidden refs, and worktrees as the source-content and compatibility substrate.
- Store the Hop state graph, prompt envelopes, lifecycle events, and coordination data in SQLite with WAL mode.
- Keep an append-only event table and derive task/proposal views from it.
- Give each historical occurrence a stable ULID while content-addressing immutable state manifests and every source, knowledge, thread, evidence, and artifact root.
- Deduplicate roots so a prompt that changes one file—or no files—adds only a small manifest plus new chunks rather than copying the repository.
- Use a temporary integration worktree for final checks.
- Emit an ordinary Git commit for each accepted source transition, with `Hop-State`, `Hop-Task`, and `Hop-Attempt` trailers.
- Preserve metadata in a Hop receipt referenced by an internal Git ref; add remote synchronization later.
- Shell out to the installed Git CLI before taking on the complexity of a custom Git implementation.
- Assign each attempt isolated temp/cache paths and, where possible, a port range; filesystem isolation alone does not isolate development servers and local services.

## Suggested six-week build sequence

### Week 1: ledger and state

- `hop init`, state IDs, prompt envelopes, accepted-state pointer, SQLite event store
- Git worktree creation and cleanup
- `hop board`, `hop graph`, `hop state show`, stable JSON output

### Week 2: attempts and proposals

- Codex and Claude adapters
- skill-driven pre-effect capture, controller-grade pre-delivery capture, checkpoint states, and proposal states
- claims with leases
- proposal nomination and receipts that reference sealed states

### Week 3: integration

- temporary integration workspace
- three-way application onto current head
- configured checks and evidence binding
- atomic land with Git export

### Week 4: coordination

- live file-footprint observation at command boundaries
- overlap and stale-base warnings
- refresh packets and agent-assisted re-proposal
- safe workspace retention and cleanup

### Week 5: history and knowledge

- prompt-state log grouped by task, plus `hop why`
- compensating undo
- sourced knowledge facts
- generated `PROJECT.md`

### Week 6: polish and dogfooding

- installable skill
- crash recovery and `hop doctor`
- demo UI or terminal board
- real use on one active repository
- instrumentation for validation metrics

## The demo that proves the idea

Do not demo only “two agents in two worktrees”; existing tools already do that.

Demo this:

1. Start an authentication task and a settings task from the same accepted state.
2. Show both agents’ declared and observed impact live.
3. Land the independent settings task with one command.
4. Show that authentication is now based on a stale state.
5. Refresh it automatically against the new accepted state.
6. Run its checks on the final combined tree and land it.
7. Ask `hop why` on a changed symbol and show intent, attempt, evidence, and decision.
8. Undo the authentication task against the current head without erasing history.

The demo promise is:

> Run coding agents in parallel, see collisions before landing, and accept each task with confidence.

## Competitive boundary

The isolation layer is not the moat:

- Git already supports multiple linked working trees through `git worktree`.
- GitButler already offers parallel and stacked branches, agent sessions, an installable agent skill, and automatic commits associated with prompts.
- Jujutsu already treats the working copy as a commit, records an operation log, supports undo, and represents conflicts as first-class state.
- BitterGit already frames the agent run as a signed provenance and review unit around ordinary Git.

So “save a code snapshot per prompt” is not enough by itself. Hop’s stronger claim is that every prompt—including no-op, failed, interrupted, follow-up, and subagent prompts—is an immutable causal state with exact delivery context; code results, evidence, knowledge, and canonical acceptance are typed descendants in the same graph.

Hop’s defensible layer is the combined graph of:

```text
every prompt as a state
  → task and attempt paths
  → live resource claims and observed impact
  → sealed states nominated as proposals
  → state-bound validation evidence
  → policy decisions
  → accepted-state lineage
  → sourced project knowledge
```

That graph can eventually answer questions existing source control cannot answer naturally:

- Which active tasks are likely to interfere before their diffs exist?
- Which agent attempt best satisfied this task’s acceptance criteria?
- Is this test evidence still valid for the tree we are about to accept?
- What accepted prompt-state introduced this contract, and why?
- Can this old task be replayed safely on today’s project?
- Which project facts were invalidated by this change?

Adjacent references: [Git worktrees](https://git-scm.com/docs/git-worktree.html), [GitButler agent sessions](https://blog.gitbutler.com/gitbutler-agent-assist), [GitButler’s agent skill](https://docs.gitbutler.com/ai-agents/getting-started), [Jujutsu’s model](https://github.com/jj-vcs/jj), and [BitterGit provenance](https://bittergit.com/).

## Product risks

### It becomes a worktree wrapper

If users value only launching isolated agents, this becomes a feature inside an IDE or Git client. The acceptance ledger, evidence validity, refresh workflow, and impact map must create recurring value.

### Scope declarations are inaccurate

Treat claims as predictions, compare them with observed edits, and improve them continuously. Never make safety depend on an agent accurately naming every file in advance.

### “Semantic merge” destroys trust

Use models to explain, plan, and regenerate. Use deterministic application, policies, and tests to decide what is accepted.

### Knowledge becomes hallucinated authority

Only accepted prompt-states or integration states can introduce facts. Require provenance and allow supersession or invalidation. Keep the generated briefing small.

### Prompt history leaks secrets

Make secret removal a pre-persistence boundary, not a display filter. Retain a
typed redaction marker and count, but never the credential, a reversible form,
or a credential hash. Apply the same sanitizer to summaries, recorded commands,
and check output. Separate private transcripts from shareable receipts, support
configurable retention, and encrypt sensitive local records. Never require
hidden model reasoning to be stored.

### Claims create deadlocks

Default to soft leases that expire. Use hard locks only for specifically configured critical resources.

### “Replace Git” blocks adoption

Keep normal commits, remotes, and pull requests as an escape hatch. Become the preferred interface first; earn the right to replace deeper layers later.

## Validation plan

Recruit ten to fifteen developers already running at least three concurrent agent sessions. Observe their existing workflow before pitching the solution.

Measure:

- human coordination and landing time per accepted task
- collisions found before versus after submission
- percentage of proposals landed without manual worktree or rebase work
- refresh success rate for stale proposals
- regressions and rollbacks per accepted task
- repeat usage after one week
- number of times users query history, evidence, or project knowledge after landing

The decisive signal is not “people like the branchless UI.” It is that they repeatedly rely on Hop’s acceptance ledger and cross-agent impact model. A strong early target would be cutting supervision time per accepted task roughly in half while catching meaningful collisions earlier.

## Naming and positioning

“Hop” is memorable, friendly, and verbable: hop into an attempt, hop back, hop between solutions. It is also crowded. Apache Hop already ships a `hop` CLI for its orchestration platform, and `hop.dev` is in use. Keep Hop as the prototype codename until package, domain, trademark, and search clearance are complete.

Recommended category:

> Prompt-native version control for coding agents

Recommended headline:

> Run coding agents in parallel. Land changes with confidence.

Recommended explanatory line:

> Hop saves every prompt as a project state, gives its work an isolated path, and lets you safely accept, compare, replay, or undo the result—while remaining compatible with Git and GitHub.

## The next three decisions

1. **Choose the initial product boundary.** Recommendation: local CLI and ledger that can launch or attach agents; no hosted service yet.
2. **Choose the acceptance contract.** Define the exact deterministic conditions under which Hop may advance canonical state.
3. **Build the collision demo before the UI.** If stale proposals, final-tree validation, task history, and undo do not feel dramatically better from the CLI, a visual client will not create the moat.
