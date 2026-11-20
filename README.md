# PostgreSQL Reliability Operator

A small Kubernetes operator that runs a highly available PostgreSQL cluster on
top of [Patroni](https://patroni.readthedocs.io/) and measures its reliability
with SLO-grade metrics.

The project is deliberately scoped: it is not a DBaaS replacement, but a
readable, test-covered controller that demonstrates the three skills a platform
team needs at once — Go development, Kubernetes operations, and SRE practices.

```
PostgresCluster (CRD)
        │
        ▼
   Operator (Go, controller-runtime)
        ├── Secret            (generated credentials)
        ├── ServiceAccount    (Patroni identity)
        ├── Role/RoleBinding  (Patroni → Kubernetes DCS)
        ├── ConfigMap         (patroni.yml)
        ├── StatefulSet       (3 × PostgreSQL + Patroni)
        ├── Service (headless)         internal Patroni traffic
        ├── Service (primary)          reads/writes endpoint
        ├── Service (replicas)         read-only endpoint
        └── PodDisruptionBudget
        │
        ▼
   Patroni REST API ──► status.conditions + Prometheus metrics
        │
        ▼
   Grafana dashboard, alert rules, chaos experiment
```

## Why this project

- **Go**: CRD, controller-runtime reconciler, idempotent resource management,
  typed HTTP client, table-driven tests, `-race` in CI.
- **DevOps**: reproducible kind cluster, Docker images, plain YAML manifests,
  GitHub Actions pipeline, RBAC for a workload that talks to the Kubernetes API.
- **SRE**: leader/health conditions, failover measurement, `pgc_*` metrics,
  alert rules with a runbook, and a chaos script that kills the leader on
  purpose.

## Requirements

- Go 1.24+
- Docker
- kind, kubectl

## Quickstart

```bash
make demo
```

This single command creates a kind cluster, builds both images, loads them into
kind, installs the CRD and the operator, and creates the sample cluster
(`pgc-demo/demo`). Wait until the resource reports `Ready`:

```bash
kubectl -n pgc-demo get pgc
# NAME   PHASE   LEADER   REPLICAS   AGE
# demo   Ready   demo-0   3          2m
```

Connect to the primary:

```bash
kubectl -n pgc-demo run psql --rm -it --restart=Never \
  --image=postgres:16 --env=PGPASSWORD="$(kubectl -n pgc-demo get secret demo -o jsonpath='{.data.superuser-password}' | base64 -d)" \
  -- psql -h demo-primary -U postgres -c 'select inet_server_addr(), pg_is_in_recovery();'
```

## The failover experiment

```bash
make failover
# current leader: demo-0 (mode: switchover)
# switching over to demo-1...
# new leader: demo-1
# leadership change observed after 9s (includes a 5s status poll interval)
```

Verified against a real kind cluster (PostgreSQL 16 + Patroni 4.0.4, 3 members):
the switchover itself completes in a few seconds, the operator observes it
within one reconcile interval, the `-primary` Service follows the new leader,
and the table created before the switchover is readable on the new primary.

Modes:

- `switchover` (default, used by `make failover`) — planned, deterministic
  leadership handover through Patroni.
- `delete` — deletes the leader pod. On a single-node kind cluster the pod is
  usually recreated before the Patroni TTL expires, so it may reacquire the lock
  and no failover happens. Useful on a multi-node cluster.

Freezing PID 1 inside the container is not an option: the kernel ignores
`SIGSTOP` for the init process of a PID namespace.

The same event is visible in the operator metrics and events:

```bash
kubectl -n pgc-demo describe pgc demo
kubectl -n pgc-demo get events --sort-by=.lastTimestamp | tail
# Leader changed to demo-1; observed failover window 8.1s
```

## Metrics and SLO

| Metric | Type | Meaning |
| --- | --- | --- |
| `pgc_leader_is_healthy` | gauge | 1 when a leader is elected |
| `pgc_replicas_in_sync` | gauge | replicas with zero replication lag |
| `pgc_failover_duration_seconds` | gauge | observed leader failover window |

The failover metric is intentionally honest about its precision: it measures the
time between the last observation of the old leader and the first observation of
the new one, so it is bounded by the reconcile interval (5s) and resets when the
operator restarts. For a precise number use `chaos/kill-leader.sh`, which
timestamps the event externally.

The SLO used by the alert rules is:

- 99.9% of the time a leader exists,
- at least two in-sync replicas in steady state,
- failover under 30 seconds.

## API

```yaml
apiVersion: db.example.com/v1
kind: PostgresCluster
metadata:
  name: demo
spec:
  replicas: 3            # 1..5, default 3
  image: pgc/postgres-patroni:16
  storageSize: 1Gi
  patroni:
    ttl: 20
    loopWait: 10
    retryTimeout: 10
  resources:
    requests: { cpu: 100m, memory: 256Mi }
    limits:   { cpu: "1",  memory: 1Gi }
```

Status exposes `phase`, `leader`, a per-member replica list, and three
conditions: `Ready`, `LeaderElected`, `ReplicasInSync`.

## Design decisions

- **Patroni owns HA.** The operator renders configuration and observes state; it
  does not reimplement quorum. See
  [ADR 0001](docs/adr/0001-patroni-over-native.md).
- **Kubernetes is the DCS.** No etcd or Consul to operate. Patroni uses
  ConfigMaps as the DCS and patches pod role labels, which is why the operator
  provisions a dedicated Role and RoleBinding per cluster. ConfigMaps are used
  instead of Endpoints so Patroni does not fight Kubernetes over the Endpoints
  object that belongs to the headless Service.
- **Credentials are generated once.** The operator creates a Secret with random
  passwords and never rewrites it, so pod restarts do not rot credentials.
- **Patroni drives the Services.** Role labels written by Patroni make
  `-primary` and `-replicas` Services stable without extra proxy logic.
- **No webhooks in v1.** Defaults are applied in the controller, which keeps the
  project runnable without cert-manager.
- **Conservative updates.** The StatefulSet uses the `OnDelete` strategy: image,
  env, probe and resource changes are written to the spec, but pods are only
  recreated when deleted, so a running primary is never restarted implicitly.
- **Authenticated control plane.** The Patroni REST API is protected with
  credentials from the generated Secret (`PATRONI_RESTAPI_USERNAME/PASSWORD`
  map to `restapi.authentication`). Read-only health endpoints such as
  `GET /cluster` are public by design; mutating endpoints (`POST /reload`,
  `POST /switchover`, `PATCH /config`, ...) return 401 without credentials. The
  operator always sends basic auth.

## Known limitations

- No backups yet: WAL-G/PITR is the next milestone.
- The failover metric is sampled, not event-sourced: it is bounded by the 5s
  reconcile interval and resets when the operator restarts.
- On a single-node kind cluster an unplanned pod deletion may be absorbed by the
  fast StatefulSet restart before the Patroni TTL expires; use switchover or a
  multi-node cluster to exercise unplanned failover.
- Single operator replica (leader election is wired but off by default).
- Storage class is whatever the cluster provides; no per-cluster class selector.
- Storage size is immutable after creation (StatefulSet PVC templates cannot be
  resized by the operator).
- Deleting the generated Secret makes the operator generate new passwords while
  running pods keep the old ones; recreate the cluster in that case.
- No YugabyteDB support — it is a different engine with a different API.

## Roadmap

- WAL-G base backups and automated PITR verification.
- Helm chart and Terraform module (deliberately left out of v1 to keep the
  runtime story simple).
- Multi-cluster dashboards and error-budget reporting.
- Admission webhook for validation and defaults.

## Repository layout

```
api/v1/                  CRD types and deepcopy
cmd/                     operator entrypoint
internal/controller/     reconciler and resource builders
internal/patroni/        Patroni REST client
internal/metrics/        Prometheus metrics
config/                  CRD, RBAC, operator Deployment, samples
images/postgres-patroni/ PostgreSQL + Patroni image
chaos/                   failure-injection scripts
monitoring/              Grafana dashboard and Prometheus alert rules
docs/adr/                architecture decision records
```

## Development

```bash
make fmt vet test     # format, vet, unit tests with -race
make build            # bin/operator
make image            # pgc/postgres-patroni:16
make demo             # full local environment
make chaos            # kill the leader and measure failover
make undeploy         # remove everything
make kind-down        # delete the kind cluster
```
