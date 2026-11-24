# Secrets: credentials a workspace uses but never holds

Status: design (G5.9). The build follows in its own pull requests, one scenario family each.

## The problem

Agents need credentials: a token to clone private code, an API key to call a model or an internal service. Today the only way in is the workspace itself: an environment variable on a command, or a file. Either way the credential ends up where Cairn's other features copy it:

- **Disk.** A file, a shell history, a `.git/config` with the token in the remote URL. Quick saves copy the disk, and copies of a save carry it to new workspaces.
- **Memory.** A full save is a checkpoint of every process's memory. A token read into a process stays in that process's heap, and so in the save and in every copy made from it, even if no file ever held it.
- **Reach.** Code in the workspace can read a credential it was handed and send it anywhere the network allows.

Scrubbing can't fix this. A full save captures memory, and nothing can reliably find and remove a token in a process's heap (it may be encoded, split or copied). So the credential must never enter the workspace at all.

## The design: placeholders, swapped on the way out

A workspace gets a **placeholder** for each secret instead of its value: an environment variable whose value is a random token like `cairn-secret-7f3a…` that means nothing outside this engine. The engine swaps in the real value only on outbound requests, only to the hosts the secret is bound to, and only after the request has left the sandbox.

```
workspace (gVisor)                       engine (host)                      github.com
  git clone https://x:$GITHUB_TOKEN@…  →  egress proxy for this workspace  →  Authorization: real token
  sees: cairn-secret-7f3a…                  - host on the secret's list?
                                           - swap placeholder → value
                                           - TLS to the real host, verified
```

1. **Creating a secret.** `POST /v1/secrets {name, value, hosts: ["github.com", "*.githubusercontent.com"]}`. The value is write-only: the API never returns it, and logs and the activity log never show it. `cairn secret set NAME --host github.com` reads the value from stdin, never from argv.
2. **Giving it to a workspace.** `secrets: ["GITHUB_TOKEN"]` on create (or later). The workspace gets `GITHUB_TOKEN=cairn-secret-<random>` in every command's environment. The placeholder is per workspace and per secret, so a placeholder copied out of one workspace is useless in another.
3. **The proxy.** A workspace with secrets has its outbound HTTP(S) go through a proxy the engine runs on the workspace's host-side address, the same place as the allow-list's DNS forwarder (G5.4). The firewall makes the proxy the only way out on ports 80 and 443, so a client that ignores `HTTPS_PROXY` gets nowhere instead of going around it. `HTTP_PROXY` and `HTTPS_PROXY` are set for every command.
4. **TLS.** To read and rewrite a request's headers over HTTPS, the proxy terminates TLS. Each workspace gets its own CA, made at create. The CA's certificate is installed in the workspace's trust store (the system bundle plus `SSL_CERT_FILE`, `NODE_EXTRA_CA_CERTS`, `REQUESTS_CA_BUNDLE`, `GIT_SSL_CAINFO`), and its key stays on the host. The proxy makes a leaf certificate for each host on the fly. Upstream, the proxy is a normal client: it verifies the real host's certificate against the host's trust store and fails closed.
5. **The swap.** Only for a request to a host on the secret's list:
   - Placeholders in any header value are replaced with the value. That covers `Authorization: Bearer`, `token`, `x-api-key` and any custom header.
   - `Authorization: Basic` is decoded, the placeholder replaced in `user:password`, and the header encoded again. That is how git sends `https://x:TOKEN@host`.
   - Bodies and URLs are never rewritten: a value placed there would come back in a response or a log.
   A request carrying a placeholder to a host that is **not** on that secret's list is refused (HTTP 403 from the proxy) and recorded in the activity log by secret name, so a prompt-injected agent can't send the placeholder somewhere useful. Even if it did, the placeholder alone is worthless.
6. **Responses.** Responses pass through unchanged. If an upstream echoes the real value back (some APIs do on token-introspection endpoints), the proxy replaces it with the placeholder before the workspace sees it, so the value can't reach memory that way either.

## Where values live in the engine

- Secret values are kept in the state store encrypted with AES-256-GCM. The key comes from `cairnd --secrets-key FILE` (or `CAIRN_SECRETS_KEY_FILE`), never from the data folder. Without a key, `POST /v1/secrets` returns `unsupported_by_runtime`: a value is never stored in plaintext.
- Saves never contain values: the workspace never had them. A save records which secrets the workspace used, by name, so a resume or copy gets new placeholders for the same secrets. A copy gets them only if the caller has access to those secrets (G5.7 keys, later).
- Object storage holds saves only, so no secret material reaches the bucket.
- Revoking or rotating a secret is one API call. Workspaces keep their placeholders, and the next request uses the new value or is refused.

## What this does not cover

- **Non-HTTP protocols.** SSH keys, database passwords on raw TCP and SMTP aren't covered. For git, use HTTPS remotes. Databases are a later design: a per-protocol proxy, or short-lived credentials issued per workspace.
- **Certificate pinning.** A client that pins certificates (some mobile SDKs, a few CLIs) refuses the proxy's leaf certificate. It fails closed and the activity log says why.
- **What the response contains.** A secret's value stays out, but data fetched with it (a private repository's code) lands in the workspace on purpose. That's the point of the credential.
- **The host runtime.** It has no sandbox and no network control, so it refuses secrets (`unsupported_by_runtime`), like it refuses allow-lists.

## Why not the alternatives

| Alternative | Why not |
| --- | --- |
| Put the value in the environment, scrub saves | A full save holds process memory; a heap can't be scrubbed reliably. Copies multiply any miss. |
| Mount the value as a file, excluded from saves | The process that reads it still holds it in memory, so it ends up in full saves. Code in the workspace can also send it anywhere. |
| Short-lived tokens only | Good where the issuer supports them, and a later addition (the engine can mint them per request). Most API keys are long-lived, so this doesn't replace the proxy. |
| A proxy without TLS termination (CONNECT only) | It can't see headers, so it can't swap anything; it can only allow or deny hosts, which G5.4 already does. |

## Scenarios

| ID | Proves |
| --- | --- |
| G5.9.1 | A secret bound to a host is used on requests to it: the server receives the value, the workspace sees only the placeholder (env, `/proc/self/environ`). |
| G5.9.2 | No quick save, full save, copy or file read contains the value: every byte of the saves and the copy's disk is scanned for it, raw and base64. |
| G5.9.3 | A placeholder sent to a host off the secret's list is refused and logged by secret name; the value never leaves. |
| G5.9.4 | git clones a private repository over HTTPS with `https://x:$TOKEN@host`, with no credential on disk afterwards. |
| G5.9.5 | The value never appears in API responses, engine logs or the activity log; `cairn secret set` reads it from stdin. |
| G5.9.6 | Rotating a secret changes what running workspaces send without restarting them; deleting it makes their requests fail. |
| G5.9.7 | The store holds values only encrypted; without `--secrets-key`, creating a secret is refused. |
| G5.9.8 | A copy of a save gets new placeholders for the same secrets; a placeholder from the source workspace does nothing in the copy. |
| G5.9.9 | A client that bypasses the proxy (direct connection on 443) gets nowhere. |

Journeys this unblocks: G11.2 (fetch private code with a credential that never lands in the workspace) and G15.1 (call internal APIs with secrets the agent can't read back or leak).

## Build order

1. Engine-side secrets: store (encrypted), API, CLI, redaction (G5.9.5, G5.9.7).
2. The proxy with per-workspace CA, swap rules and firewall (G5.9.1, G5.9.3, G5.9.9).
3. Saves and copies: names recorded, new placeholders, the full scan (G5.9.2, G5.9.8).
4. git, rotation, journeys (G5.9.4, G5.9.6, G11.2, G15.1), MCP and the Python SDK.
