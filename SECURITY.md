# Security policy

## Reporting a vulnerability

Please report security problems privately, using GitHub's private vulnerability reporting: open the repository's **Security** tab and choose **Report a vulnerability**. Don't open a public issue for a vulnerability.

Include what you can:

- what an attacker can do, and from where: code inside a workspace, a caller with an API key, the network, or someone holding a save;
- the steps or a proof of concept;
- the Cairn version (`cairn version`) and runtime (`runsc --version`), and the host OS and kernel;
- a diagnostics bundle (`cairn diagnostics`) if it helps. It holds no workspace contents.

We'll acknowledge the report, work out a fix and agree a disclosure date with you. Fixes are released as soon as they're ready, with credit unless you prefer otherwise.

## What's in scope

Anything that breaks a promise in the [threat model](docs/threat-model.md), for example:

- code in a workspace reaching the host, another workspace, cloud metadata, or anything its allow-list doesn't permit;
- a secret's value reaching a workspace, a save, a copy, a log or a diagnostics bundle;
- a key doing more than its scope allows, or an own key reaching another key's workspaces;
- a save restoring after being changed, or a corrupt save restoring silently.

Known limits listed in the threat model, such as gVisor escapes in a current release, are still worth reporting. They go to gVisor as well.

## Supported versions

Security fixes go into the latest release. Keep gVisor at the release pinned in `deploy/versions.env` or newer.
