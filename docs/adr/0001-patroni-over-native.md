# ADR 0001: Patroni over native Kubernetes replication

## Status

Accepted.

## Context

The operator has to run a highly available PostgreSQL cluster inside Kubernetes.
There are two realistic options:

1. Build HA on top of native Kubernetes primitives: a primary and replicas as
   StatefulSet pods, leader election through a ConfigMap or Lease, and promote a
   replica when the primary disappears.
2. Delegate HA to Patroni, which already implements leader election, quorum,
   failover, switchover, fencing and pg_rewind, and can use Kubernetes itself as
   the distributed configuration store (DCS).

## Decision

Use Patroni with the `kubernetes` DCS backend.

## Rationale

- Failover correctness is hard. Patroni encodes years of production experience
  (quorum, TTL, `maximum_lag_on_failover`, split-brain protection) that we
  cannot reasonably reproduce in a pet project, let alone in a first version.
- The Kubernetes DCS reuses the cluster's own API server as the source of truth:
  no etcd or Consul to operate.
- Patroni exposes a REST API (`/cluster`, `/primary`, `/replica`, `/readiness`,
  `/liveness`). The operator uses it to observe state and to publish metrics
  instead of guessing from pod readiness.
- It matches the stack used by the real DBaaS teams this project is modelled
  after, which keeps the design discussion honest.

## Consequences

- Every PostgreSQL pod needs RBAC permissions to manage the DCS objects and
  patch pod labels; the operator provisions a dedicated Role and RoleBinding.
  The DCS backend is ConfigMaps, which avoids fighting Kubernetes over the
  Endpoints object that belongs to the headless Service.
- Patroni writes role labels on pods, which lets the operator expose stable
  `-primary` and `-replicas` Services.
- The operator stays small: it renders configuration and observes state, while
  Patroni owns the hard part of the state machine.

## Alternatives rejected

- **CloudNativePG**: a mature operator, but using it would mean not writing the
  controller at all, which defeats the purpose of the project.
- **Stolon**: less active and a smaller ecosystem than Patroni.
- **Native replication with a custom leader election**: too much correctness
  risk and no clear benefit for a first version.
