# E0 spike: can gVisor save a real agent's workspace and resume it on another server?

Throwaway scripts, not product code. They drive gVisor's `runsc` directly (the way the engine will) to checkpoint four real workloads and restore them, first on the same server, then on a second server with a different CPU type. The results decide whether E1 goes ahead as planned and set the save and resume time targets (scenarios G1.7 and G9.6 in [`spec/catalog.go`](../spec/catalog.go)).

## What passes

A restore passes only if it is **the same process continuing**, not a restart: same PID and start time, in-memory counters still rising, plus a workload-specific check.

| Workload | Proves | Check after restore |
| --- | --- | --- |
| `python-open-files` | open files and in-memory state | counter keeps rising; append offset equals file size; reader skipped no lines |
| `node-dev-server` | a listening server and a file watcher | same server answers with rising hit count; editing a watched file still fires the watcher |
| `package-install` | a network download interrupted mid-transfer | the checkpoint lands mid-download; after restore the install still finishes (pip must retry its broken connection) |
| `headless-browser` | a large, multi-process program | same Chromium process; the page's JavaScript counter keeps running |

## How it works

- Hosts are set up by the shared [`deploy/host-setup.sh`](../deploy/host-setup.sh) with versions pinned in [`deploy/versions.env`](../deploy/versions.env), identical on local VMs and droplets.
- `run.sh` runs **on a Linux host as root**. `prepare` builds each workload's image with Docker, exports it to an OCI bundle and creates a network namespace (runsc's user-space network stack takes its addresses from it). `runsc` runs with `--overlay2=root:memory`, so the workload's writes live in sandbox memory and travel inside the checkpoint image; the bundle stays the unchanged base.
- `cross.sh` runs **on your machine**. It checkpoints a workload on host A, streams the bundle and checkpoint image straight from A to B over SSH, restores on B and runs the check, in both directions.
- Every run appends one JSON line to `results/runs.jsonl`: workload, verdict, detail, checkpoint/restore/transfer times, image size, and both hosts' CPU model, kernel, runsc version and a hash of the CPU flags.

## Stage A: local dev VMs (free)

Proves the scripts work, that same-host save and restore works, and that a save moves between two machines. See [devenv/README.md](../devenv/README.md).

```sh
devenv/dev.sh up          # two Ubuntu 24.04 VMs, set up like the droplets
devenv/dev.sh spike       # same-host, every workload
devenv/dev.sh cross       # cairn-a -> cairn-b and back
```

Any other Linux machine works too: `sudo spike/run.sh setup && sudo spike/run.sh local-all`.

## Stage B: two DigitalOcean droplets (the gate)

Two droplets in one region with different CPU families (Premium Intel and Premium AMD, 2 vCPU / 4 GB each), billed hourly; a few hours of testing costs about a dollar. `do-up` shows the price and asks before creating anything.

```sh
doctl auth init                                         # once; you enter your own API token
doctl compute ssh-key list                              # pick your key's ID
devenv/dev.sh do-up <ssh-key-id> nyc3                   # creates cairn-spike-intel and cairn-spike-amd
devenv/dev.sh do-session root@<intel-ip> root@<amd-ip>  # setup + every test in one sitting
devenv/dev.sh do-down                                   # delete the droplets when done
```

## Reading the results

```sh
jq -r '[.mode, .workload, .verdict, .checkpoint_ms, .restore_ms, .image_bytes, .detail] | @tsv' spike/results/runs.jsonl
```

The gate passes when all four workloads pass `cross` in both directions. Record the outcome, the measured times (they become the G9.6 targets) and the CPU baseline decision in [docs/status.md](../docs/status.md). If cross-CPU restores fail, the failure message says which CPU features differ; that is the input to the CPU-baseline decision, not a reason to stop.

## Known risks this spike is meant to surface

- Checkpoint images include the sandbox's memory: size grows with what the workload holds (the browser is the big one).
- Open TCP connections do not survive a move; programs must retry (the `package-install` workload tests exactly this).
- A restore may refuse a host whose CPU lacks features the source had. Pinning saves to a common CPU baseline is the expected fix.
- Some programs use system calls gVisor does not implement; any such failure shows up in `/var/lib/cairn-spike/logs/<workload>.out`.
