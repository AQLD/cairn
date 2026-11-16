# Using Cairn from a coding agent

`cairn mcp` serves the [Model Context Protocol](https://modelcontextprotocol.io) on stdin and stdout, so Claude Code, Cursor, Codex, VS Code, Windsurf, Goose and other MCP clients can use Cairn workspaces with no code. The agent gets a private Linux machine per task, where it can run installs, tests and servers, save before risky steps, rewind when something breaks and copy a save to try several approaches at once.

## Connect your agent

You need the `cairn` CLI and a running engine. `cairn up` starts one locally in Docker. The MCP server finds the engine through `CAIRN_ENDPOINT` and `CAIRN_TOKEN`, or the config file `cairn up` writes.

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
| `workspace_create` | A new workspace for the task; later calls use it by default. `network=true` for package installs and `git clone`. |
| `exec` | Runs a shell command and returns its exit code and output. `background=true` starts a server or watcher that keeps running between calls. |
| `write_file`, `read_file` | One file at a time. |
| `push_dir`, `pull_dir` | A whole folder between your machine and the workspace (your project in, the finished work out), merging with what is there. |
| `session_run` | Python or shell code in an interpreter that stays alive, like a notebook. |
| `save` | Saves the workspace. `full=true` also keeps memory and running processes; the workspace carries on at once. |
| `rewind` | Puts the workspace back exactly as it was at a save, running processes included for full saves. |
| `copy` | New, independent workspaces from a save, to try approaches in parallel. |
| `list_saves` | A workspace's saves, each with the save it came from. |
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
