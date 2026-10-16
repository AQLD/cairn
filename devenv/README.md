# Dev environment

Cairn runs on Linux (gVisor needs a Linux kernel). Development happens on two kinds of Linux hosts, set up by one script so they never drift apart:

| | Local dev VMs | DigitalOcean test droplets |
| --- | --- | --- |
| Hosts | `cairn-a`, `cairn-b` (Lima, on your Mac) | `cairn-spike-intel`, `cairn-spike-amd` |
| OS | Ubuntu 24.04 | Ubuntu 24.04 |
| Set up by | [`deploy/host-setup.sh`](../deploy/host-setup.sh) | the same script |
| Versions | [`deploy/versions.env`](../deploy/versions.env) (gVisor, Go, Ubuntu) | the same file |
| CPU | arm64 (Apple Silicon) | x86_64, Intel and AMD |
| Cost | free | about $0.18/hour for both (4 vCPU / 8 GB each) |
| Use for | everyday development, rehearsing every test | the cross-CPU gate and real-hardware numbers |

Everything runs the same way in both places except cross-CPU portability, which needs two different x86 CPUs.

## Local

Needs [Lima](https://lima-vm.io) 1.0 or newer, native arm64 on Apple Silicon (`brew install lima` on a native Homebrew, or the release tarball into `~/.local/lima`; `dev.sh` finds either).

```sh
devenv/dev.sh up            # create + start cairn-a and cairn-b, run host-setup (first time: a few minutes)
devenv/dev.sh test          # go test -short ./... on Linux
devenv/dev.sh spike         # E0 spike, same-host save/restore of every workload
devenv/dev.sh cross         # save on cairn-a, restore on cairn-b and back
devenv/dev.sh compat        # compatibility tests under gVisor vs plain Docker
devenv/dev.sh rehearse      # the whole DigitalOcean session, on the two VMs
devenv/dev.sh shell         # a shell in the repo on cairn-a
devenv/dev.sh stop          # stop both VMs (state kept); `up` starts them again
devenv/dev.sh delete        # remove them
```

The repo is mounted into both VMs at the same path as on your Mac, so edit on the Mac and run on Linux with no copying.

To run the acceptance suite on gVisor inside a VM (as root, with Docker for the base template):

```sh
devenv/dev.sh shell
sudo devenv/accept-runsc.sh "$PWD" -run TestEndGoals/G3   # extra args go to go test
```

## DigitalOcean: one paid hour, used fully

Queue work locally until it passes and `devenv/dev.sh rehearse` (the same session on the local VMs) is clean, then rent the droplets for one sitting:

```sh
doctl auth init                                     # once, with your own API token
devenv/dev.sh do-up <ssh-key-id> nyc3               # shows the hourly price, asks, creates both
devenv/dev.sh do-session root@<intel-ip> root@<amd-ip>
devenv/dev.sh do-down                               # asks, then deletes both
```

`do-session` copies the repo, runs host setup, then every step below, logging each to `spike/results/session-<time>/` and carrying on if one fails:

1. host info for both droplets (CPU model and flags)
2. the spike on each droplet alone, including the 1/2/4 GiB memory sweep
3. the cross-CPU spike, Intel → AMD and AMD → Intel
4. the compatibility tests on both
5. the full test suite on Linux
6. benchmarks on both

Add steps to `cmd_do_session` in `dev.sh` as the engine grows, so each paid hour tests everything.
