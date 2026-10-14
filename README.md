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

> **Status: early development.** There is nothing to install yet. Watch or star the repository to follow along.

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

## Try the development build

The engine runs on gVisor in production (Linux). For development there is a `host` runtime that runs commands directly on your machine with **no isolation**; use it only with code you trust.

```sh
make build
./bin/cairnd --runtime host --data ./data &        # engine on 127.0.0.1:7420
WS=$(./bin/cairn create --name demo)
./bin/cairn exec $WS -- sh -c 'echo hello > note.txt'
SAVE=$(./bin/cairn save $WS)
./bin/cairn exec $WS -- sh -c 'echo changed > note.txt'
./bin/cairn resume $SAVE && ./bin/cairn get $WS note.txt   # hello
./bin/cairn copy $SAVE --count 3
```

## Planned for v0.1

- Workspaces with CPU, memory and disk limits, started from templates
- Run commands, read and write files, control network access
- Quick save (files) and full save (memory and running processes)
- Resume and copy from any save
- Usage limits per workspace, user and project
- REST API, MCP server, Python and TypeScript SDKs, and a CLI
- One-command install on a single Linux server

## How it is tested

Everything Cairn must handle is written down as a tree of end-goal scenarios in [`spec/catalog.go`](spec/catalog.go), from "a coding agent picks up where it left off" down to "a flipped byte in a save is caught". `make test` runs them; [docs/test-plan.md](docs/test-plan.md) shows which pass, and [ROADMAP.md](ROADMAP.md) ties each milestone to the scenarios that gate it.

## Contributing

Cairn is built in the open, and contributions will be welcome once the first release is out. Start with [CONTRIBUTING.md](CONTRIBUTING.md), which explains the layout, the scenario-first workflow and the rules the engine must never break. We use the [Developer Certificate of Origin](https://developercertificate.org/) instead of a contributor license agreement: sign off your commits with `git commit -s`.

## License

Cairn is licensed under the [Apache License 2.0](LICENSE).

Cairn is built by [aqld](https://aqld.com).
