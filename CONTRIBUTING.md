# Contributing to Cairn

Cairn is an open-source engine (Go, Apache-2.0) that gives every AI agent a private Linux workspace it can **save** (quick: files; full: memory and processes), **resume** (even days later, on another server) and **copy** (one save → many independent workspaces). Production isolation is gVisor (`runsc`), driven directly, not through Docker.

Start with [ROADMAP.md](ROADMAP.md) (milestones E0–E5 and their gates) and [docs/test-plan.md](docs/test-plan.md) (every end-goal scenario and its status).

## Layout

| Path | What |
| --- | --- |
| `spec/` | **The end-goal catalog.** Every scenario the platform must handle, as a tree (G1–G10 → children). Source of truth for tests, benchmarks, the test plan and milestone gates. |
| `acceptance/` | Implementations of catalog scenarios, run through the public client. One file per goal (`g1_agent.go`…). |
| `bench/` | Benchmarks for performance scenarios (G9). |
| `internal/engine/` | Lifecycle, files, saves, resume, copy, crash recovery. |
| `internal/save/` | On-disk save format (folder + versioned manifest + checksums), tar in/out. |
| `internal/store/` | SQLite state (workspaces, saves). |
| `internal/runtime/` | `Runtime` interface; `host` (development, NO isolation) and `runsc` (gVisor). |
| `internal/api/` | REST API (`net/http`, Go 1.22+ routing). |
| `internal/failpoint/` | Named crash points for recovery tests. |
| `client/` | Public Go client. The CLI and the acceptance suite use it, so tests exercise the real API. |
| `cmd/cairnd`, `cmd/cairn` | Engine daemon and CLI. The CLI talks to the engine only through the API. |
| `internal/tools/testplan` | Generates `docs/test-plan.md` from the catalog. |

## Commands

```sh
make test        # unit + acceptance (host runtime, -short)
make accept      # full acceptance run, verbose summary
make bench       # benchmarks
make plan        # regenerate docs/test-plan.md (commit the result)
make run         # cairnd on ./data with the host runtime, localhost only
go test ./acceptance -run 'TestEndGoals/G6' -v                 # one goal
CAIRN_CYCLES=10000 go test ./acceptance -run G6.2 -timeout 0   # E1 loop gate
```

Go ≥ 1.25 (uses `os.Root`). macOS works for everything except `runsc`, which needs Linux.

## How work is done (scenario-first)

1. **Pick a scenario** from docs/test-plan.md (⏳ pending, current milestone first) or **add one** to `spec/catalog.go` as a child of the goal it serves. IDs are permanent; a child's ID extends its parent's (`G6.1` → `G6.1.6`). Never renumber.
2. **Write the scenario** in `acceptance/gN_*.go` with `register("G6.1.6", func(e *Env) {...})`, using only the public client plus `Env` helpers. Declare environment needs in the catalog (`Needs`), not with `if` checks, so unmet needs skip with a reason.
3. **Make it pass** in the engine. Prove the test is real: break the code it guards and watch it fail.
4. **`make plan`**, commit. One scenario (or one small family) per pull request.

A scenario that cannot run in the current environment must **skip with the reason**, never pass vacuously. Split a scenario into children whenever describing it needs more than one "and".

## Invariants

- **State before action.** Record a state change in the store before acting on it; every multi-step operation must be finishable or roll-back-able by `engine.recover` after a crash at any point. New crash points get a `failpoint` and a G7 scenario.
- **Saves are atomic.** Built in `saves/.tmp-*`, synced, renamed into place; recorded `pending` first, `ready` after commit. Nothing restores without `save.Verify` passing.
- **The save format is a contract.** Changing it means bumping `save.FormatVersion` and keeping a reader for every older version, with a scenario proving old saves still restore.
- **No path escapes.** Every workspace file access goes through `os.Root`; tar extraction rejects paths outside the target.
- **Capabilities are honest.** A runtime reports only what it enforces; unsupported operations return `ErrUnsupported` (`unsupported_by_runtime`), never a fake success.
- **The `host` runtime is not a sandbox.** Never make it the default or recommend it for untrusted code; `cairnd` refuses non-localhost without `--token`.
- **Stable error codes** (`not_found`, `invalid_argument`, `conflict`, `save_corrupt`, `unsupported_by_runtime`, `unauthorized`, `internal`). Adding one is an API change.

## Conventions

- Comments explain *why*; match the surrounding density. Errors wrap with `%w` and map to API codes in `internal/api`.
- No new dependencies without a reason in the pull request (the engine ships as one static binary; SQLite is pure Go).
- Commits: `type(scope): summary` (`feat(engine)`, `test(acceptance)`, `docs(roadmap)`), signed off under the [Developer Certificate of Origin](https://developercertificate.org/) (`git commit -s`).
