# Using Cairn from a coding agent

`cairn mcp` serves the [Model Context Protocol](https://modelcontextprotocol.io) on stdin and stdout, so Claude Code, Cursor, Codex, VS Code, Windsurf, Goose and other MCP clients can use Cairn workspaces with no code. The agent gets a private Linux machine per task, where it can run installs, tests and servers, save before risky steps, rewind when something breaks and copy a save to try several approaches at once.

## Connect your agent

```sh
cairn up                    # an engine (skip if you have one)
cd ~/code/my-project
cairn setup --project       # connect every agent found, and give this project a workspace
```

| Agent | What `cairn setup` does |
| --- | --- |
| Claude Code | skill in `~/.claude/skills/cairn/`; `claude mcp add --scope user cairn -- cairn mcp`; with `--project`, `CLAUDE.md` imports `AGENTS.md` |
| Codex | skill in `~/.agents/skills/cairn/`; `[mcp_servers.cairn]` in `~/.codex/config.toml` |
| pi | skill in `~/.agents/skills/cairn/` (pi has no MCP by design: it uses the skill and the `cairn` CLI) |
| Cursor | `cairn` in `~/.cursor/mcp.json` |

`--agents claude,pi` connects only those. Every step is safe to repeat, and a skill named `cairn` that setup did not write is never replaced. `cairn doctor` checks the engine, each agent and the project, and prints the command that fixes each problem.

With `--project`, the repository (the git root around the current folder) gets a workspace named after it, with the code at `/root/NAME`. Its template fits the project: `package.json` → `dev-node` (`node:22`), `go.mod` → `dev-go`, `Cargo.toml` → `dev-rust`, anything else → `dev-python` (`python:3.12`); full images, so git, curl and compilers are there. A template the engine lacks is pulled the first time; `--template NAME[=IMAGE]` picks another. Setup then installs the dependencies (`pip install -r requirements.txt`, `npm ci`/`npm install`, `go mod download`, `cargo fetch`; again only when those files change, `--no-install` to skip) and makes a full **baseline** save, which `cairn resume .` returns to. The pin lives in `.cairn/` (which ignores itself, so it never reaches the repository). From anywhere in the project:

- `.` names the workspace: `cairn exec . -- make`, `cairn save . --full`, `cairn log . -f`.
- `cairn run -- COMMAND` copies local edits in (leaving out `node_modules`, `.venv` and other rebuildable folders), runs in the matching folder of the workspace, then copies back the files the command created or changed (up to 500 files / 200 MB; `--no-pull` leaves them there). Files deleted on one side are not deleted on the other.
- `cairn mcp`, started by the agent in the project, uses the workspace by default.

A managed section between `<!-- cairn:start -->` and `<!-- cairn:end -->` in `AGENTS.md` tells agents to work in the workspace; edit outside the markers, and rerun setup to refresh it.

### Other MCP clients

`cairn mcp install CLIENT` prints the entry that registers Cairn with CLIENT. Add `--write` to add it for you (existing entries are kept), and `--project` for the project's own config file instead of the user-wide one:

| Client | Command | Where it goes |
| --- | --- | --- |
| Claude Code | `cairn mcp install claude --write` | runs `claude mcp add --scope user cairn -- cairn mcp`; `--project` writes `.mcp.json` |
| Cursor | `cairn mcp install cursor --write` | `~/.cursor/mcp.json`; `--project` writes `.cursor/mcp.json` |
| Codex CLI | `cairn mcp install codex --write` | `~/.codex/config.toml` (`[mcp_servers.cairn]`, with a 10-minute tool timeout) |
| VS Code | `cairn mcp install vscode --write` | `.vscode/mcp.json` (key `servers`) |
| Windsurf | `cairn mcp install windsurf` | prints the entry for `~/.codeium/windsurf/mcp_config.json` |
| Goose | `cairn mcp install goose` | prints the entry for `~/.config/goose/config.yaml` |

The entry launches the absolute path of the `cairn` binary you ran, so desktop apps that start without your shell's `PATH` still find it. `cairn mcp --workspace ID` (or `CAIRN_WORKSPACE=ID`) pins every tool call to one workspace.

## Tools

| Tool | What it does |
| --- | --- |
| `workspace_create` | A new workspace for the task; later calls use it by default. `network=true` for package installs and `git clone`; `allow=["pypi.org", "*.github.com"]` to reach only what the task needs; `secrets=["GITHUB_TOKEN"]` for credentials the agent uses but never sees (see [design/secrets.md](design/secrets.md)). |
| `exec` | Runs a shell command and returns its exit code and output. `background=true` starts a server or watcher that keeps running between calls. |
| `write_file`, `read_file` | One file at a time. |
| `push_dir`, `pull_dir` | A whole folder between your machine and the workspace (your project in, the finished work out), merging with what is there. |
| `session_run` | Python or shell code in an interpreter that stays alive, like a notebook. |
| `save` | Saves the workspace. `full=true` also keeps memory and running processes; the workspace carries on at once. `pin=true` keeps the save however old it gets (the engine prunes old saves beyond the newest 20 and older than 7 days). |
| `rewind` | Puts the workspace back exactly as it was at a save, running processes included for full saves. |
| `copy` | New, independent workspaces from a save, to try approaches in parallel. |
| `list_saves` | A workspace's saves, each with the save it came from. |
| `diff` | Files added, modified and removed between two saves (by default, what a save's step changed since the save it came from). |
| `workspace_list`, `pause`, `workspace_delete` | Housekeeping. |

Tool descriptions tell the agent when to use each one (save before deleting files, migrations or upgrades; rewind instead of repairing by hand), and a failing command's result names the last save to rewind to. A command exiting non-zero is a normal result; tool errors (`isError`) are engine errors such as `not_found`, with Cairn's error code in the message. Long output is cut to its first and last parts, about 20,000 characters in all.

## Try it

Ask your agent something like:

> Create a Cairn workspace with network, push this repo into it, start the dev server in the background, and make a full save. Then delete the app folder, rewind to the save, and show me the server still answers with the same pid. Finally copy the save three times and try a different fix for the failing test in each copy.

Behind that request the agent calls:

1. `workspace_create {network: true}`, then `push_dir {local_dir: "."}`.
2. `exec {command: "npm install && npm run dev", cwd: "/root/app", background: true}` starts the server; it keeps running.
3. `save {full: true}` records files, memory and the server process.
4. `exec {command: "rm -rf /root/app"}` breaks everything; `rewind {save: ...}` brings back the files and the same server process.
5. `copy {save: ..., count: 3}`, then `exec` in each copy with `workspace=<id>`; `pull_dir` brings the winning fix back to your machine.

Full saves and processes that keep running between calls need the gVisor runtime (what `cairn up` runs). On the development-only `host` runtime, quick saves, rewinds and copies of files work, but processes do not survive and nothing is isolated.
