# Roadmap

Cairn is built in six milestones. Each answers one question and ends at a gate: a set of end-goal scenarios from [`spec/catalog.go`](spec/catalog.go) that must all pass. The next milestone starts only when the gate passes. Live status per scenario is in [docs/test-plan.md](docs/test-plan.md).

How the order is chosen:

1. **Riskiest assumption first.** If gVisor cannot save a real agent's workspace and resume it on another server, nothing else matters.
2. **Foundations before features.** The state store, the workspace lifecycle and the save format are the hardest things to change later.
3. **Only what a real agent needs.** A feature joins the engine only if an agent needs it to do real work.

> Scenarios marked implemented pass on the development `host` runtime. A milestone's gate counts only runs on the real `runsc` runtime on the Linux test bench.

## E0: Prove it can work

**Question:** can gVisor save a real agent's workspace and resume it on another server?

| # | Builds |
| --- | --- |
| 1 | Save and resume real agent workloads across two servers (scripts, no product code) |
| 2 | Portable CPU profile for saves |

**Gate:** G1.7 (golden workloads resume correctly on a second server) and G9.6 (measured full save and resume times become the targets).
**If it fails:** fall back to saving files only and restarting programs, and rethink before E1.

## E1: One server, done right

**Question:** can one server run, save and resume workspaces without ever losing track of one?

| # | Builds | Status |
| --- | --- | --- |
| 3 | Workspace lifecycle with CPU, memory and disk limits | lifecycle done; limits need `runsc` |
| 4 | State store and crash recovery | done (SQLite, failpoint-tested) |
| 5 | Commands with streamed output and exit codes | exit codes, output, stdin, timeouts done; streaming to do |
| 6 | Files: read, write, upload, download | done |
| 7 | Full save and resume on the same server | needs `runsc` |
| 8 | Quick save of files while running | done |
| 9 | Save manifest: runtime version, CPU profile, sizes, checksums | done (format v1) |
| 10 | Minimal REST API and command-line tool | done |
| 11 | Network off by default, switched on per workspace | needs `runsc` |

**Gate:** every E1 scenario, including G6.2 at `CAIRN_CYCLES=10000` (10,000 save-and-resume cycles, nothing lost) and the kill tests G7.1–G7.6 (clean recovery whenever the engine dies mid-operation).

## E2: Never lose a save

**Question:** do saves survive a lost server, storage faults and gVisor upgrades, and can one save become many?

| # | Builds |
| --- | --- |
| 12 | Saves in object storage, verified by checksum |
| 13 | Resume on another server |
| 14 | Upgrade-safe restore that keeps older gVisor builds |
| 15 | Copy one save into many workspaces, each with its own identity |
| 16 | Soft delete and retention for saves |

**Gate:** G2 (twenty copies side by side, each isolated), G6 (every corruption caught, saves restore on another CPU type and after an upgrade), G7.5.

## E3: Agents live in it

**Question:** can real agents do real work across saves and resumes?

| # | Builds |
| --- | --- |
| 17 | MCP server |
| 18 | One SDK (TypeScript or Python, whichever early users need) |
| 19 | Base templates for Python and Node |
| 20 | Auto-pause idle workspaces |

**Gate:** G1 (including G1.9: a real coding agent works in workspaces daily for two weeks), G4, G5.5 (a fresh server runs its first workspace within five minutes).

## E4: Bulletproof

**Question:** is it safe for strangers, measured, and cheap enough to run?

| # | Builds |
| --- | --- |
| 21 | Quotas per workspace and project, saving when a limit is hit |
| 22 | Outbound allow-list |
| 23 | Metrics, health checks and structured logs |
| 24 | Density and CPU-sharing benchmarks |

**Gate:** G3 and G8 (limit and escape tests), G10.5 (72-hour soak with no leaks), G9.7 (density measured against seven workspaces per 8 vCPU / 32 GiB server). Passing E4 opens the first public release.

## E5: Many servers

**Question:** does it keep working when servers come and go?

| # | Builds |
| --- | --- |
| 25 | Multi-server scheduler with leases and fencing |
| 26 | Automatic recovery from a lost server |

**Gate:** G7.7 and G7.8 (a lost server never leaves a workspace running twice, and every workspace on it resumes elsewhere from its last save).

## Not before E4

Preview URLs for dev servers, secret injection through an outbound proxy, a Kubernetes install, a second SDK, GPU workloads, and live migration between servers.
