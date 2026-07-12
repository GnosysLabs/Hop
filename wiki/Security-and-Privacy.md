# Security and privacy

## Local data

Hop stores state in the project under `.hop/`. Prompt text, source trees,
commands, and check output are local unless the project or its filesystem is
copied elsewhere, or the user explicitly pairs Hop with a forge account.
SQLite data is not encrypted at rest.

`.hop/` is excluded through `.git/info/exclude`, so ordinary Git operations do
not publish it. Initialization refuses to hide a `.hop` directory that the
project already tracks.

## Private prompt sync

Prompt history remains local by default. `hop auth login FORGE_URL` starts a
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

Packaged installers download `checksums.txt` from the same published Gitea
Release and verify the selected archive before extraction. Gitea Releases are
created as drafts, after race tests, vetting, and cross-platform builds, then
must be reviewed before publication.

For stronger provenance before general availability, the release owner should
sign `checksums.txt` with an offline-controlled release key and publish the
public key independently. Checksum signing is listed as a launch gate in the
[release checklist](Release-Checklist).

## Release-machine trust

Release builds execute on a maintainer machine, not on the Gitea server. Use a
trusted, patched machine. On the forge selected by `hop auth login`, the release
workflow uses that existing OAuth grant through `hop auth exec`; the token stays
out of argv and is redacted from the child process's captured output. Releases
upload as drafts for review. Hop and its agents never create, rotate, list, or
revoke provider account tokens.

## Filesystem safety

Hop does not use `reset --hard`, move the active branch, or write the user's
real Git index. Visible-root synchronization fails closed when files, ignored
destinations, or staged state could be overwritten.

Automatic push uses the existing Git transport for other forges. For the forge
selected by `hop auth login`, Hop stores the OAuth grant only in the OS keychain
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

## Reporting a vulnerability

Before the public security contact is configured, disclose vulnerabilities
privately to the repository owner rather than opening a public issue. Add a
`SECURITY.md` with the final contact before the first public release.
