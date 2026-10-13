# Status

The working log for whoever (person or agent) picks up the engine next. Read it at the start of a session; update it at the end. Keep it short: move finished items to the decisions log or delete them.

**Current milestone:** E0 (prove gVisor save/resume across servers) in parallel with E1 groundwork on the `host` runtime.
**Scenario counts:** see the table at the top of [test-plan.md](test-plan.md) (`make plan`).

## In flight

One line per active task: `scenario/feature · owner (person, agent or branch) · started`. Claim a task here before starting so parallel agents don't collide.

- E0 spike · stage A (local VMs) **passed** · 2026-10-12. Same-host and cross-host (cairn-a ↔ cairn-b, same CPU, both directions): all 4 workloads pass. Stage B (DigitalOcean, cross-CPU Intel ↔ AMD, x86 numbers) next: `devenv/dev.sh do-up` then `do-session`.

## Next up (in order)

1. **Run the E0 spike** ([spike/README.md](../spike/README.md)): stage A on any Linux machine (`run.sh local-all`), then stage B on two droplets (`provision.sh`, `cross.sh all`, `destroy.sh`). Output: pass/fail per workload and direction, save/resume times (G9.6), and the CPU baseline decision below.
2. **`runsc` runtime driver** (`internal/runtime/runsc.go`): start/exec/stop, then checkpoint/restore, using what the spike proved (bundle per workspace, overlay in memory, network re-applied before every start/restore). Turn on capabilities one at a time as their scenarios pass in the dev VMs (`devenv/dev.sh test` with `CAIRN_TEST_RUNTIME=runsc`).
3. **Linux test bench in CI:** a self-hosted runner (or two droplets) that runs the acceptance suite with `CAIRN_TEST_RUNTIME=runsc CAIRN_ENV_LINUX_BENCH=1`.

## Open decisions

- [ ] CPU baseline every full save is pinned to (decide from the E0 droplet types).
- [x] Streaming format for exec output: NDJSON on the exec endpoint (decided 2026-10-12). Interactive shells (PTY, stdin streaming) will need a WebSocket later.
- [ ] Workspace identity on full-save copies: how hostname, machine-id, MAC/IP and RNG state are refreshed (G2.6).
- [ ] Which SDK first (TypeScript or Python), decided by the first design partners.

## Findings

- 2026-10-12 · runsc (release-20261005.0, systrap, arm64 Ubuntu 24.04 VM) saves and restores live processes on the same host: python 25–116 ms checkpoint / 55–73 ms restore (20 MB image); node 30–57 / 187–190 ms (8 MB); headless Chromium 374 / 354 ms (162 MB); pip mid-download 1.4 s / 470 ms (199 MB) and the same pip process finished the download after restore. Inotify watchers and listening sockets survive.
- 2026-10-12 · runsc moves a network namespace's address and routes into its own stack, leaving the namespace bare. **Network config must be re-applied before every start and restore**, not set up once. The engine's runsc driver must own this.
- 2026-10-12 · Cross-host (two VMs, relay transfer through the Mac): all 4 workloads pass both ways. Checkpoint 40 ms–1.5 s, restore 90 ms–1.1 s, images 8–152 MB. Open TCP connections do not survive a move: pip's in-flight download failed once and its retry finished. Agents' tools must retry; the SDK should say so.
- 2026-10-12 · With `--overlay2=root:memory` the workspace's writes travel inside the checkpoint image; the bundle stays the unchanged base. Image size ≈ workload memory + its writes.

## Decisions log

Newest first. One line each: date · decision · why.

- 2026-10-12 · State-store schema changes go through numbered migrations recorded in `PRAGMA user_version`; never edit a shipped migration; a newer database is refused · engine upgrades must keep state (G10.6).
- 2026-10-12 · Deleted saves are kept 7 days (`cairnd --retention`) then purged hourly; delete/undelete are idempotent · mistakes are recoverable and agents retry.
- 2026-10-12 · Exec output is lossless: NDJSON stream with base64 chunks, or base64 in the JSON response when not valid UTF-8 · agents handle binary output (archives, images).

- 2026-10-12 · Local dev = two Lima VMs (Ubuntu 24.04, vz) set up by `deploy/host-setup.sh`, versions pinned in `deploy/versions.env`; DigitalOcean runs go through `dev.sh do-session` · identical hosts locally and on real hardware, so paid hours only cover what local can't (cross-CPU, x86 numbers).
- 2026-10-12 · gVisor installed per release under `/opt/gvisor/<release>/`, `runsc` symlinked · old releases stay installed, the basis for upgrade-safe restore.

- 2026-10-12 · Scenario catalog (`spec/`) is the single source of truth for tests, benchmarks, test plan and milestone gates · keeps the plan and the code from drifting.
- 2026-10-12 · `Runtime` interface with an explicit, capability-reporting `host` dev runtime · build and test API/state/saves/recovery on macOS while gVisor work happens on Linux; tests skip (never pass) on missing capabilities.
- 2026-10-12 · Save format v1: one folder, `manifest.json` + `rootfs.tar` (+ `memory.img`), SHA-256 per file, temp-folder-then-rename, `pending`→`ready` in the store · crash-safe and verifiable; format changes need a version bump.
- 2026-10-12 · SQLite via `modernc.org/sqlite` (pure Go), WAL + `synchronous=FULL` · one static binary; committed state survives power loss.
- 2026-10-12 · Resume restores the workspace the save belongs to; copies are new workspaces · matches "resume where it stopped" vs "try several ideas" semantics.
- Earlier (engine plan): Go for the engine; drive `runsc` directly, not via Docker; SQLite on one server; CLI talks only to the REST API.
