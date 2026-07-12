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
`127.0.0.1` callback. Hop requests only `read:user` and `read:repository` and
stores access and refresh credentials in the operating-system keychain. The
non-secret forge selection is stored in the user's config directory; tokens
are never written to `.hop`, Git, command output, or JSON output.

After pairing, Hop derives the repository owner and name from its configured
Git remote and sends redacted, publishable portable prompt records to the same
forge origin. The server derives account identity from the bearer token; local
payloads cannot select a user ID. Sync is private, idempotent, batched, and
best-effort. Proposal, acceptance, and landing remain successful if the forge
is offline, and later sync attempts resend records from SQLite.

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
trusted, patched machine; keep the release token out of shell history and source
files; scope it to the Hop repository; export it only for the publish command;
and unset it immediately afterward. Releases upload as drafts for review. The
token must be provisioned by the user outside the agent session: Hop and its
agents never create, rotate, list, or revoke provider account tokens.

## Filesystem safety

Hop does not use `reset --hard`, move the active branch, or write the user's
real Git index. Visible-root synchronization fails closed when files, ignored
destinations, or staged state could be overwritten.

Automatic push delegates authentication to the user's existing Git transport
and credential configuration. Hop does not store remote passwords, SSH private
keys, or access tokens. It disables terminal credential prompting in the
background push path and redacts detected credentials from returned errors.

## Reporting a vulnerability

Before the public security contact is configured, disclose vulnerabilities
privately to the repository owner rather than opening a public issue. Add a
`SECURITY.md` with the final contact before the first public release.
