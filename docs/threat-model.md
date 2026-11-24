# Threat model

This document covers what Cairn protects, from whom, how, and where it stops. Each defence names the scenario that proves it (see [test-plan.md](test-plan.md)). A defence without a passing scenario isn't claimed.

## What Cairn protects

| Asset | Why it matters |
| --- | --- |
| The host | Workspaces run code nobody reviewed: an agent's commands, the packages it installs, the pages its browser opens. |
| Other workspaces | One engine serves many agents. One workspace must not read, change or slow another. |
| Saves and copies | They hold everything a workspace had, memory included, and they travel: to object storage, to other servers, into copies. |
| Credentials | API tokens and keys, secrets workspaces use, cloud credentials of the host. |
| The record | What happened, and who did it. |

## Who might attack

1. **Code inside a workspace.** This is the main threat, and it's assumed hostile. The code can be prompt-injected, malicious from a package install, or just broken. It controls everything inside its sandbox: root inside the workspace, any syscall gVisor offers, any outbound connection the workspace's network allows.
2. **A caller with a key.** An agent or a CI job holding an API key, possibly compromised. It can do whatever its key allows and should be able to do nothing more.
3. **The network between callers and the engine.** It can see and change traffic that isn't encrypted.
4. **Whoever gets a save or a backup.** Object storage, a copied data folder, a diagnostics bundle sent with a bug report.

The host's operator (root on the server) is trusted. Cairn doesn't defend against the machine it runs on.

## Defences

### Code inside a workspace

- **Isolation.** Each workspace is a gVisor sandbox (`runsc`). Its syscalls are handled by gVisor's own kernel in user space, not the host's. Host files and processes are invisible (G8.7).
  - The engine drives `runsc` directly, without Docker, and process 1 inside is `cairn-init` (see CONTRIBUTING).
  - The `host` runtime has no isolation. It is for development only, refuses network control and secrets, and `cairnd` refuses non-localhost listening without a token.
- **Resources.** Memory is limited inside the sandbox: the biggest process is killed, never the workspace (G8.4, G3.2). Process count is capped, so a fork bomb is contained (G8.3). Disk is a fixed-size image per workspace (G8.5). On a host volume that nearly fills, writes wait rather than fail silently (G6.4.4), and a frozen disk never outlives the engine (G6.4.5).
- **Network.**
  - Off by default (G5.3).
  - When on: cloud metadata (169.254.0.0/16, 100.100.100.200) and the host itself are unreachable (G8.6). The firewall rules are re-applied in a fixed order on every start and adopt.
  - With an allow-list, only listed names and ranges are reachable. DNS goes through a forwarder that answers only listed names, and connections straight to an IP address are dropped (G5.4).
- **Files through the API.** Every access goes through `os.Root`. Paths can't escape the workspace (G8.1) or follow a symlink out (G8.2). Folder archives are checked the same way (G8.8).
- **Secrets.** A workspace holds a placeholder, never the value (G5.9.1). The engine's proxy swaps it in only on requests to the secret's hosts and refuses it anywhere else (G5.9.3). Direct connections on ports 80 and 443 are dropped, so the proxy can't be skipped (G5.9.9). The proxy never connects to an address a workspace couldn't reach itself: loopback, metadata, other workspaces, the host. It checks the address it actually dials, after DNS (G5.9.3).

### A caller with a key

- **Authentication.** With `--token`, every request needs a token (G5.2). `cairnd` won't listen beyond localhost without one.
- **Keys per caller** (G5.7):
  - The operator's token makes keys with a scope: read, write or admin.
  - A key is stored only as its SHA-256 and shown once (G5.7.1).
  - Revoking a key stops it at once and affects no other key (G5.7.2).
  - A read key changes nothing (G5.7.3). Only admin keys manage keys, secrets, templates, the audit log and diagnostics (G5.7.4).
  - An own key sees only the workspaces it made; others look absent (G5.7.5).
- **Accountability.** Every action is recorded with the key that made it, across workspaces, and the record outlives restarts and deletions (G5.8).

### The network

- **TLS.** The API can be served over TLS 1.2+, and renewed certificates are picked up without a restart (G5.6). Clients verify the engine and refuse an unknown CA, with no silent fallback (G5.6.2).

### Saves, backups and bundles

- **Integrity.** Every save is checksummed and verified before anything restores from it. Corruption is reported, never restored (G6.1). In object storage the manifest goes up last, and a save is downloaded and verified before use (G6.5).
- **Secrets stay out.** Workspaces never hold secret values, so saves and copies can't either. Every byte of quick and full saves and of copies is scanned to check, with a canary proving the scan sees memory (G5.9.2). Copies get their own placeholders (G5.9.8).
- **Secrets at rest in the engine** are sealed with AES-256-GCM, using a key that must live outside the data folder. A wrong key is refused at start (G5.9.7).
- **Diagnostics** hold facts about the engine and host, never workspace names, files, commands, outputs, secrets or tokens (G10.9.2).

## Known limits

These are what Cairn doesn't defend today:

- **gVisor is the boundary.** A gVisor escape is a host compromise. Keep `runsc` current (`deploy/versions.env`), and note that checkpoints don't restore across gVisor releases ([compatibility.md](compatibility.md)).
- **Secrets cover HTTP(S) only.** SSH keys and database passwords on raw TCP aren't covered. Clients that pin certificates refuse the proxy, so they fail closed.
- **Data fetched with a secret lands in the workspace.** A private repository cloned with a token is in the workspace, its saves and its copies. That's intended.
- **Outbound traffic without an allow-list.** A workspace with network on and no allow-list can send what it holds anywhere. Use an allow-list for anything that reads private data (G12.4).
- **Allow-lists are IPv4.** Workspaces have no IPv6.
- **Open TCP connections don't survive moving to another server.** Tools must retry.
- **No rate limits per key** beyond the engine-wide limits on concurrent commands and operations.
- **The operator is trusted.** Root on the host can read workspaces, saves and, with the key file, secrets.

## Reporting a vulnerability

See [SECURITY.md](../SECURITY.md).
