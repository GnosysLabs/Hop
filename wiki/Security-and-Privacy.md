# Security and privacy

## Local data

Hop stores state in the project under `.hop/`. Prompt text, source trees,
commands, and check output are local unless the project or its filesystem is
copied elsewhere. SQLite data is not encrypted at rest.

`.hop/` is excluded through `.git/info/exclude`, so ordinary Git operations do
not publish it. Initialization refuses to hide a `.hop` directory that the
project already tracks.

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

## Runner trust

Release jobs execute on Gitea act runners. Only trusted, isolated runners should
receive release tags or release tokens. Do not run secret-bearing release jobs
from unreviewed fork pull requests.

## Filesystem safety

Hop does not use `reset --hard`, move the active branch, or write the user's
real Git index. Visible-root synchronization fails closed when files, ignored
destinations, or staged state could be overwritten.

## Reporting a vulnerability

Before the public security contact is configured, disclose vulnerabilities
privately to the repository owner rather than opening a public issue. Add a
`SECURITY.md` with the final contact before the first public release.
