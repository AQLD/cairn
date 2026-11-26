<p align="center">
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset=".github/assets/cairn-logo-dark.svg">
    <img src=".github/assets/cairn-logo-light.svg" alt="Cairn" width="96">
  </picture>
</p>

<h1 align="center">Cairn</h1>

<p align="center">
  Every AI agent gets its own computer that never forgets.
</p>

<p align="center">
  <a href="LICENSE">Apache-2.0</a> · <a href="https://aqld.com">aqld.com</a>
</p>

---

Cairn is an open-source engine that gives each AI agent a private Linux workspace. The agent can save its workspace at any moment, resume it days later on another server, or copy it to try several ideas at once. It runs on your own servers.

> **Status: early development.** [The latest release](https://github.com/AQLD/cairn/releases/latest) runs on your laptop (below) and on a single Linux server; expect breaking changes before 1.0. Workspaces run on gVisor: see [docs/compatibility.md](docs/compatibility.md) for the known differences from a regular Linux machine.

## How it works

1. **Start:** the agent gets its own private Linux computer, walled off from everything else.
2. **Save:** at any moment, everything is saved, including programs that are still running.
3. **Resume or copy:** pick up exactly where it stopped, even days later, or make copies to try several ideas at once.

## Use cases

- **Your coding agent picks up where it left off.** Files, installed tools and the running app are saved when the agent stops. Tomorrow it continues instead of starting over.
- **Try several fixes at once.** Save before a risky step, make copies, let each try a different fix, and keep the winner.
- **Every one of your users gets their own computer.** Each user's agent runs walled off from everyone else, with a usage limit you set. Hitting the limit saves the work instead of losing it.
- **Your assistant waits for your answer.** When an agent needs an approval or a password, its computer is saved and switched off until you reply.
- **Agents that stay inside your walls.** Install it on your own servers, on ordinary virtual machines, and nothing leaves your network.

## Run it on your laptop

Needs Docker (Docker Desktop, OrbStack or colima on macOS; Docker Engine on Linux).

```sh
curl -fsSL https://raw.githubusercontent.com/AQLD/cairn/main/install.sh | sh   # or: go install github.com/AQLD/cairn/cmd/cairn@latest
cairn up                      # engine in Docker with real gVisor workspaces (under a minute the first time)
```

From a checkout of this repository, `go run ./cmd/cairn up --build` builds the engine image locally instead of pulling it.

On Linux with gVisor's `runsc` installed, `sudo cairn up` runs the engine straight on the host instead of in Docker: a background `cairnd` from next to the CLI, with its workspaces, saves, log and pid file in `/var/lib/cairn-up/cairn`. `sudo cairn down` and `sudo cairn down --purge` work the same way, and the client config goes to your own `~/.config/cairn`. `--docker` keeps the engine in Docker; an engine already running in Docker stays there.

`cairn up` writes `~/.config/cairn/config.json`, so the CLI and the Python SDK find the engine with nothing to set. Then:

```sh
ws=$(cairn create)                                   # python:3.12-slim by default
cairn exec $ws -- sh -c 'nohup python3 -m http.server 8000 >/dev/null 2>&1 &'
cairn exec $ws -- sh -c 'echo hello > note.txt'
save=$(cairn save $ws --full)                        # files, memory and the running server; it keeps running
cairn exec $ws -- sh -c 'echo broken > note.txt'
cairn resume $save                                   # rewind: note.txt says hello, the same server still runs
cairn copy $save --count 3                           # three independent copies to try three ideas
```

`cairn down` stops the engine and pauses every workspace (their processes come back on `cairn up`); `cairn down --purge` deletes everything. The engine container is privileged because gVisor builds each sandbox's namespaces and cgroups. Workspaces run on gVisor's own kernel, so agent code never reaches your machine's kernel directly; a flaw that let code escape gVisor would land in that privileged container, which is root on Docker's VM (macOS) or on the machine itself (Docker Engine on Linux).

### Connect your coding agent

```sh
cd ~/code/my-project
cairn setup --project
```

`cairn setup` finds the coding agents on your machine (Claude Code, Codex, pi and Cursor) and connects each one: it installs the Cairn skill where the agent loads [Agent Skills](https://agentskills.io) and registers `cairn mcp` where the agent speaks MCP (pi uses the skill and the CLI). With `--project` it also gives the repository its own workspace with the code pushed in, and adds a short Cairn section to `AGENTS.md` (Claude Code's `CLAUDE.md` imports it) telling the agent to run builds, tests and servers there, save before risky steps, and rewind instead of repairing by hand. Run it again any time; it only changes what is out of date.

Inside the project, `.` names its workspace and `cairn run` copies your edits in before running:

```sh
cairn run -- pytest -q                 # local edits, run in the workspace
cairn save . --full                    # what an agent does before a risky step
cairn log . -f                         # watch everything the agent runs
cairn doctor                           # what is wrong, and the command that fixes it
```

See [docs/agents.md](docs/agents.md) for each agent, the MCP tools and other MCP clients.

For working on Cairn itself there is also a `host` runtime (`make run`) that runs commands directly on your machine with **no isolation** and no full saves; use it only with code you trust.

## What it does

- Workspaces with CPU, memory and disk limits, started from templates or registry images
- Run commands (including background servers), read and write files and folders, network on or off
- Quick save (files) and full save (memory and running processes), with saves in a local folder or S3-compatible storage; saves of templated workspaces hold only what changed
- Resume, rewind and copy from any save; list the files a step changed (`cairn diff`)
- Old saves pruned by policy (the newest 20 and anything under 7 days kept), pinned saves kept for good (`cairn pin`)
- A warm pool for instant creates (`cairnd --warm N`), and workspaces that share their template read-only (`cairnd --shared-templates`, opt-in)
- A volume that fills never loses a workspace's write: below a reserve, writes wait until there is room
- Outbound allow-lists per workspace (`cairn create --allow pypi.org --allow '*.github.com'`): only those names and ranges are reachable
- Secrets a workspace uses but never holds (`cairn secret set GITHUB_TOKEN --host github.com`, then `secrets` on create): it gets a placeholder, swapped for the value only on requests to the secret's hosts, so no disk, save or copy carries it
- The API over HTTPS (`cairnd --tls-cert --tls-key`, renewed certificates picked up without a restart; clients trust a private CA with `CAIRN_CA_CERT`)
- REST API, CLI, MCP server and Python SDK; `cairn setup` connects Claude Code, Codex, pi and Cursor
- Signed releases with a bill of materials ([docs/verify.md](docs/verify.md))
- Runs on a laptop with Docker (`cairn up`), on a Linux server, or on Kubernetes ([deploy/helm](deploy/helm))

Not yet: usage limits per user and project, a TypeScript SDK. [ROADMAP.md](ROADMAP.md) has the order.

## Security

What Cairn protects, from whom, and where it stops: [docs/threat-model.md](docs/threat-model.md). Report vulnerabilities privately: [SECURITY.md](SECURITY.md).

## How it is tested

Everything Cairn must handle is written down as a tree of end-goal scenarios in [`spec/catalog.go`](spec/catalog.go), from "a coding agent picks up where it left off" down to "a flipped byte in a save is caught". `make test` runs them; [docs/test-plan.md](docs/test-plan.md) shows which pass, and [ROADMAP.md](ROADMAP.md) ties each milestone to the scenarios that gate it.

## Contributing

Cairn is built in the open, and contributions will be welcome once the first release is out. Start with [CONTRIBUTING.md](CONTRIBUTING.md), which explains the layout, the scenario-first workflow and the rules the engine must never break. We use the [Developer Certificate of Origin](https://developercertificate.org/) instead of a contributor license agreement: sign off your commits with `git commit -s`.

## License

Cairn is licensed under the [Apache License 2.0](LICENSE).

Cairn is built by [aqld](https://aqld.com).
