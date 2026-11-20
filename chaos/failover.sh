#!/usr/bin/env bash
# Reliability experiment: hand leadership to another member and measure how
# long the cluster needs to accept writes on a new primary.
#
# Modes:
#   switchover (default) - planned switchover through patronictl. Deterministic
#                          and safe: the old leader is drained before the new one
#                          is promoted. This is what `make chaos` runs.
#   delete               - delete the leader pod. On a single-node kind cluster
#                          the pod is usually recreated before the Patroni TTL
#                          expires, so it may simply reacquire the lock and no
#                          failover happens. Useful on multi-node clusters.
#
# Note: freezing PID 1 inside the container is not an option - the kernel
# ignores SIGSTOP for the init process of a PID namespace.
set -euo pipefail

NAMESPACE="${1:-pgc-demo}"
CLUSTER="${2:-demo}"
MODE="${MODE:-switchover}"
TIMEOUT="${TIMEOUT:-180}"

leader="$(kubectl -n "$NAMESPACE" get pgc "$CLUSTER" -o jsonpath='{.status.leader}')"
if [[ -z "$leader" ]]; then
  echo "error: cluster $CLUSTER has no leader in status" >&2
  exit 1
fi

echo "current leader: $leader (mode: $MODE)"
start=$(date +%s)

case "$MODE" in
  switchover)
    candidate="$(kubectl -n "$NAMESPACE" get pods \
      -l "app.kubernetes.io/name=$CLUSTER,cluster-name=$CLUSTER,role=replica" \
      -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}' \
      | grep -v "^${leader}$" | head -1)"
    if [[ -z "$candidate" ]]; then
      echo "error: no candidate replica found" >&2
      exit 1
    fi
    echo "switching over to $candidate..."
    kubectl -n "$NAMESPACE" exec "$leader" -- \
      patronictl -c /config/patroni.yml switchover --force --candidate "$candidate" >/dev/null
    ;;
  delete)
    kubectl -n "$NAMESPACE" delete pod "$leader" --wait=false
    echo "deleted $leader, waiting for a new leader..."
    ;;
  *)
    echo "error: unknown MODE '$MODE' (expected switchover or delete)" >&2
    exit 1
    ;;
esac

deadline=$((start + TIMEOUT))
while (( $(date +%s) < deadline )); do
  current="$(kubectl -n "$NAMESPACE" get pgc "$CLUSTER" -o jsonpath='{.status.leader}' 2>/dev/null || true)"
  if [[ -n "$current" && "$current" != "$leader" ]]; then
    end=$(date +%s)
    echo "new leader: $current"
    echo "leadership change observed after $((end - start))s (includes a 5s status poll interval)"
    exit 0
  fi
  sleep 1
done

echo "error: no new leader after ${TIMEOUT}s" >&2
exit 1
