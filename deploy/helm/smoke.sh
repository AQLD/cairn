#!/usr/bin/env bash
# Smoke test for a Cairn install on Kubernetes: create a workspace, run a
# command, full-save a running process and resume it, then delete the engine
# pod and check the workspace comes back with that process still running
# (the engine pauses workspaces on shutdown).
#
#   deploy/helm/smoke.sh [NAMESPACE] [RELEASE]
#
# Needs kubectl (pointed at the cluster) and the cairn CLI on PATH.
set -euo pipefail
ns=${1:-cairn}
release=${2:-cairn}
# the chart's resource names (see cairn.fullname)
[[ $release == *cairn* ]] || release=$release-cairn
port=17420

fail() { echo "FAIL: $*" >&2; exit 1; }
forward() {
  kubectl -n "$ns" port-forward "svc/$release" "$port:7420" >/dev/null 2>&1 &
  pf=$!
  for _ in $(seq 50); do curl -fs "http://127.0.0.1:$port/healthz" >/dev/null && return; sleep 0.2; done
  fail "engine did not answer through port-forward"
}
trap 'kill ${pf:-} 2>/dev/null || true' EXIT

export CAIRN_ENDPOINT=http://127.0.0.1:$port
if [[ -z ${CAIRN_TOKEN:-} ]]; then
  # whichever Secret the release uses (chart-made or existingSecret)
  secret=$(kubectl -n "$ns" get statefulset "$release" \
    -o jsonpath='{.spec.template.spec.containers[0].env[?(@.name=="CAIRN_TOKEN")].valueFrom.secretKeyRef.name}')
  CAIRN_TOKEN=$(kubectl -n "$ns" get secret "$secret" -o jsonpath='{.data.token}' | base64 -d)
fi
export CAIRN_TOKEN
forward

ws=$(cairn create)
echo "workspace $ws"
[[ $(cairn exec "$ws" -- python3 -c 'print("hi")') == hi ]] || fail "exec"
[[ $(cairn exec "$ws" -- uname -r) == *gvisor* ]] || fail "workspace is not running under gVisor"

cairn exec "$ws" -- sh -c 'echo kept > /root/file; nohup sh -c "n=0; while :; do n=\$((n+1)); echo \$\$ \$n > /tmp/counter; sleep 0.1; done" >/dev/null 2>&1 &'
sleep 1
read -r pid1 n1 < <(cairn exec "$ws" -- cat /tmp/counter)
sv=$(cairn save "$ws" --full)
cairn resume "$sv" >/dev/null
sleep 0.5
read -r pid2 n2 < <(cairn exec "$ws" -- cat /tmp/counter)
[[ $pid1 == "$pid2" && $n2 -gt $n1 ]] || fail "process did not survive full save ($pid1 $n1 -> $pid2 $n2)"
echo "full save kept process $pid1 running ($n1 -> $n2)"

set +e
cairn exec "$ws" -- no-such-command >/dev/null 2>&1
rc=$?
set -e
[[ $rc == 127 ]] || fail "unknown command exited $rc, want 127"

kill "$pf"
kubectl -n "$ns" delete pod "$release-0" >/dev/null
kubectl -n "$ns" rollout status "statefulset/$release" --timeout=180s >/dev/null
forward
[[ $(cairn exec "$ws" -- cat /root/file) == kept ]] || fail "workspace lost its files across a pod restart"
sleep 0.5
read -r pid3 n3 < <(cairn exec "$ws" -- cat /tmp/counter)
[[ $pid3 == "$pid1" && $n3 -gt $n2 ]] || fail "process did not survive the pod restart ($pid1 -> $pid3, $n2 -> $n3)"
echo "pod restart kept process $pid1 running ($n2 -> $n3)"
[[ $(cairn verify "$sv") == "$sv ready" ]] || fail "save does not verify after the restart"
cairn rm "$ws"
echo "PASS"
