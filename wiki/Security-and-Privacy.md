# Security and privacy

## Local data

Hop stores state in the project under `.hop/`. Prompt text, source trees,
commands, and check output are local unless the project or its filesystem is
copied elsewhere, or the user explicitly pairs Hop with a forge account.
SQLite data is not encrypted at rest.

`.hop/` is excluded through `.git/info/exclude`, so ordinary Git operations do
not publish it. Initialization refuses to hide a `.hop` directory that the
project already tracks.

## Optional legacy prompt sync

Prompt history remains local by default. The existing Gitea compatibility
adapter can pair with a Hop-enabled server. `hop auth login FORGE_URL` starts a
browser-based OAuth Authorization Code flow with PKCE and a temporary
`127.0.0.1` callback. Hop requests Gitea's `all` scope and stores the access and
rotating refresh credentials in the operating-system keychain. The
non-secret forge selection is stored in the user's config directory; tokens
are never written to `.hop`, Git, command output, or JSON output.

After pairing, Hop derives the repository owner and name from its configured
Git remote and sends redacted portable prompt records to the same forge origin.
The server derives account identity and private-repository access from that
OAuth bearer; local payloads cannot select a user ID. Hop also uses the grant
for its own same-forge Git fetch/push operations. SSH-form remotes are rewritten
to HTTPS only for the individual Git invocation, and the token is passed outside
the process arguments. Sync is private, idempotent, batched, and best-effort.
Proposal, acceptance, and landing remain successful if the forge is offline,
and later sync attempts resend records from SQLite.

`hop auth logout` removes the local keychain credential. It does not delete
local prompt history or previously synced server records.

## Credential redaction

Before persistence, Hop redacts high-confidence provider keys, contextual
tokens/passwords, private keys, authorization headers, and credential-bearing
URLs. The same sanitizer is applied to proposal summaries and recorded check
commands/output.

Detection is defense in depth, not a guarantee. Use environment variables or a
secret manager. Rotate any real credential pasted into any agent prompt even
when Hop reports a redaction.

## Installer and release integrity

Packaged installers download `checksums.txt` from the same published GitHub
Release and verify the selected archive before extraction. Releases are
created as drafts, after race tests, vetting, and cross-platform builds, then
must be reviewed before publication.

For stronger provenance before general availability, the release owner should
sign `checksums.txt` with an offline-controlled release key and publish the
public key independently. Checksum signing is listed as a launch gate in the
[release checklist](Release-Checklist).

## Release-machine trust

Release builds execute on a trusted maintainer machine, not GitHub Actions.
Publication uses the maintainer's existing authenticated `gh` session and
uploads a draft for review. Hop and its agents never create, rotate, list, or
revoke provider account tokens.

## Filesystem safety

Hop does not use `reset --hard`, force a ref, or rewrite visible files while
synchronizing Git. It may fast-forward the recorded attached branch and
atomically replace a proven projection-only real index after revalidating the
visible accepted tree, ancestry, locks, operations, and durable Hop state.
Ordinary nonignored visible-root edits can be captured when the active
branch/index tree proves the materialized base. If the visible Hop tree
is projected over a genuinely different stale branch tree, implicit capture is
blocked: Hop cannot safely distinguish deliberate edits from an external stale
checkout and will not infer mass changes or deletions. Synchronization also
remains fail-closed for ignored destinations, staged/index state, and
filesystem and ref races. If any condition is unprovable, Hop preserves the
branch, index, and files and returns the exact blocking reason and safe action.

New checkpoints, proposals, captures, reconciliation results, remote
compositions, lands, accepts, retries, and undos carry exact-tree authorization
proofs. Before accepted state advances, Hop recomputes the changed-path
manifest and verifies exact Git object IDs and modes. `hop status` reports
`accepted_provenance` as `verified`, `legacy_unverified`, or `invalid`; `hop
doctor` recomputes proofs and reports tampering or missing Git inputs.

Automatic push uses the existing Git transport for every host. For a legacy
Gitea forge selected by `hop auth login`, Hop stores the OAuth grant only in the OS keychain
and passes a repository-scoped authorization header to its Git subprocess via
the environment. It never embeds the token in a remote URL or command argument,
never persists it in Git configuration, disables terminal prompting, and leaves
the configured remote unchanged.

`hop repo create` and `hop forge api` use the same grant for repository and
Gitea API operations without exposing it to the agent. `hop auth exec` is an
explicit compatibility boundary for trusted child tools that require a token
environment variable. The child receives the credential for its lifetime, so
agents must use it only for an authorized operation and must never print or
persist the variable.

Hop's native Gitea collaboration commands embed the open-source Gitea command
engine in-process. Before invoking it, Hop refreshes the keychain-backed OAuth
grant and supplies it only through a temporary process environment. The prior
environment is restored after the command. No Tea executable, Tea configuration,
or second credential store is used.

## Reporting a vulnerability

Before the public security contact is configured, disclose vulnerabilities
privately to the repository owner rather than opening a public issue. Add a
`SECURITY.md` with the final contact before the first public release.
