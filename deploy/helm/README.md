# Cairn on Kubernetes

Run the Cairn engine inside an existing cluster. Workspaces run in gVisor
sandboxes inside the engine pod, so agent code never touches the node's kernel,
and your cluster's storage, network policies and monitoring apply as usual.

```
            your cluster
 ┌────────────────────────────────────────┐
 │  StatefulSet cairn (1 replica)         │
 │  ┌──────────────────────────────────┐  │
 │  │ cairnd ── runsc ── workspace     │  │
 │  │                └── workspace     │  │
 │  └──────────────┬───────────────────┘  │
 │                 │ PersistentVolume     │
 │                 ▼ workspaces, saves,   │
 │                   templates, database  │
 └────────────────────────────────────────┘
```

## Install

Build the image (or use a published one when releases exist):

```sh
docker build -f deploy/docker/Dockerfile -t <registry>/cairnd:<tag> .
docker push <registry>/cairnd:<tag>
```

Install the chart:

```sh
helm install cairn deploy/helm/cairn -n cairn --create-namespace \
  --set image.repository=<registry>/cairnd --set image.tag=<tag> \
  --set token.value="$(openssl rand -hex 32)"
```

or keep the token in a Secret you manage (key `token`):

```sh
kubectl -n cairn create secret generic cairn-token --from-literal=token="$(openssl rand -hex 32)"
helm install cairn deploy/helm/cairn -n cairn --set token.existingSecret=cairn-token ...
```

Then check it end to end (needs the `cairn` CLI on your PATH):

```sh
deploy/helm/smoke.sh cairn cairn
```

The smoke test creates a workspace, checks it runs under gVisor, full-saves a
running process and resumes it, restarts the engine pod, and checks the
workspace and save survived.

## Templates

Each entry in `templates` is a container image turned into a template when the
pod starts: an init container copies the image's filesystem into a tar file and
the engine imports it (again only if it changed). Any image with `tar` works:

```yaml
templates:
  - name: python
    image: python:3.12-slim
  - name: node
    image: node:22-slim
  - name: ours
    image: registry.example.com/agents/toolchain:2026.10
defaultTemplate: python
```

Workspaces created from a template keep their own copy; changing or removing a
template never changes an existing workspace.

## What it needs from the cluster

| Need | Why |
|---|---|
| A privileged pod | gVisor creates namespaces, cgroups and mounts for each sandbox. The workspaces themselves are unprivileged and run on gVisor's kernel. |
| A ReadWriteOnce volume | Workspaces, saves and the database live on it. Local SSD-backed storage gives the fastest saves. |
| Linux nodes, amd64 or arm64 | The image carries gVisor for both. |

Workspaces with network switched on reach the outside through the engine pod's
network, so the pod's NetworkPolicies and egress rules apply to them. Cloud
metadata services (169.254.0.0/16, and 100.100.100.200 on Alibaba Cloud) and
the pod itself are unreachable from workspaces regardless.

## Operating it

- **Health**: `GET /healthz` (used by the probes).
- **Metrics**: `GET /metrics` in Prometheus format; the pod carries the usual
  `prometheus.io/*` annotations. Useful series: `cairn_workspaces{state}`,
  `cairn_saves{state}`, `cairn_save_bytes`, `cairn_http_requests_total`,
  `cairn_http_request_duration_seconds`.
- **Logs**: one JSON line per request with a `request_id`, also returned to the
  caller in the `X-Request-Id` header.
- **Saves in object storage** (`saveStore`): every save is copied to an
  S3-compatible bucket in the background (retried until it succeeds), so saves
  outlive the node and the volume. A save missing locally is downloaded and
  checked against its checksums before use; one damaged locally is fetched
  again. A new install on the same bucket learns its saves and can copy them
  into new workspaces, running processes included: a lost cluster loses only
  the work since the last save. `kubectl create secret generic cairn-s3
  --from-literal=AWS_ACCESS_KEY_ID=... --from-literal=AWS_SECRET_ACCESS_KEY=...`
  then `--set saveStore.url=s3://bucket/cairn?region=...,saveStore.existingSecret=cairn-s3`.
- **State database**: checked at startup and backed up every hour
  (`state.db.backup` on the volume, consistent and written atomically). If the
  disk ever damages it, the engine refuses to start rather than act on wrong
  records; set `restoreState: true` and it rebuilds the database from the
  backup plus the volume (workspaces and saves made since the backup come
  back, ones deleted since stay deleted). It only acts on a damaged or missing
  database, so it can stay on.
- **Integrity**: every save's checksums are re-verified every `scrubInterval`
  (default 6h); a save that fails is marked `corrupt` and never resumed.
  `cairn verify SAVE` checks one on demand.
- **Disk**: the engine refuses new work with `507 insufficient_storage` before
  the volume fills, instead of writing broken saves. Workspace disks are
  sparse, so their sizes can add up to more than the volume: alert on
  `cairn_volume_free_bytes`. If the volume does fill, a workspace writing at
  that moment loses that write and its disk is repaired automatically on its
  next use once there is room (its processes restart, its files stay).
- **Pod restarts and node drains**: on shutdown the engine pauses every running
  workspace (a full save each), so after the pod comes back each workspace wakes
  on first use with its processes as they were. Give the pod time to do it
  (`pause.gracePeriod`, default 180 s). If the pod is killed without warning,
  workspaces come back with their files, as after a reboot.
- **Idle workspaces** are paused after `pause.idle` (default 30m), freeing their
  memory and CPU; the next command wakes them transparently. `cairn pause WS`
  pauses one now.
- **Memory limits** are enforced inside each sandbox: over its limit, a
  workspace's biggest process is killed (exit 137, with a message saying why);
  the workspace and its files stay.
- **Bursts of requests**: a workspace runs at most 64 commands at once
  (`--max-commands`); more are refused with `429 too_many_requests` and a
  `Retry-After` header. Heavy operations (create, save, resume, copy, template
  import) run a few at a time (`--max-operations`, default twice the CPUs);
  more wait their turn instead of failing.
- **Disk limits**: each workspace's files live in a sparse ext4 image of its
  size (`disk_mb`, default `disk.defaultMB`), loop-mounted inside the pod.
  Writing past it fails with "No space left on device" in the workspace and
  `insufficient_storage` through the API; other workspaces are unaffected. The
  images take volume space only for what is stored, so their sizes may add up
  to more than the volume: the engine's own free-space check still guards the
  volume as a whole.

## Upgrading gVisor

A full save (and so every workspace paused for a shutdown) restores only with
the gVisor release that made it. The image installs the release pinned in
`deploy/versions.env` plus every release listed in `RUNSC_KEEP`; when you
upgrade `RUNSC_RELEASE`, move the old one into `RUNSC_KEEP` so workspaces
paused before the rollout wake with their processes. Workspaces are always
started fresh on the newest release. If a save's release is missing, resuming
it fails with `unsupported_by_runtime` naming the release, and a paused
workspace wakes with its files but not its processes.

## Limits today

- One replica. A single engine owns its workspaces and saves; running more than
  one engine against the same data is not supported yet. Two installs may share
  a bucket only if they use different prefixes, except to take over from a
  lost one.
- Workspaces created before disk limits existed keep plain folders without a
  size limit.
