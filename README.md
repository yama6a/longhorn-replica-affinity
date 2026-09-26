# longhorn-replica-affinity

Soft pod-follows-volume scheduling for [Longhorn](https://longhorn.io). A mutating webhook moves pods toward nodes
that already hold a replica of their data. The volume stays where it is.

## Why

A Longhorn PV can attach to any node and carries no `nodeAffinity`. So the scheduler cannot see where replicas
live, pods land anywhere, and their IO crosses the network for the life of the pod.

- Longhorn's `dataLocality: best-effort` moves the volume instead. Each pod move costs a full replica rebuild.
- Moving the pod costs one restart and no bytes.
- PV `nodeAffinity` has no `Preferred` field, so a soft preference needs a webhook.

Upstream designed both approaches and shipped neither:

- [longhorn#12398](https://github.com/longhorn/longhorn/issues/12398), a webhook. The PR closed unmerged.
- [longhorn#12591](https://github.com/longhorn/longhorn/issues/12591), a scheduler extender. Abandoned, because it
  adds latency to every pod the scheduler places.

## `webhook`

Mutating admission on pod `CREATE`. For each PVC, it finds the nodes that hold a replica. It then appends one
`preferredDuringSchedulingIgnoredDuringExecution` term per node, weighted by how many of the pod's volumes that
node holds.

Which replicas count depends on the pod:

| Pod | Replicas counted | Why |
|---|---|---|
| ordinary consumer | `status.currentState == running` | a replica that serves no reads has nothing to offer |
| share-manager | `spec.active`, `spec.nodeID` set, no `spec.failedAt`, not deleting | Longhorn stops every replica before it recreates the share-manager, so none is running at admission. The data on disk has not moved |

- The preference is soft. A node with no replica can still take the pod.
- `requiredDuringScheduling` and existing preferred terms stay as they are.
- For RWX, a consumer prefers the share-manager's node, not a replica node. The consumer mounts nfs-ganesha, so
  that is the hop to save.
- The admission path makes no API calls. Informers hold the replica map in memory.
- Deploy with `failurePolicy: Ignore`. The webhook improves placement and must never block it.

## RWX: two hops, both fixed by moving a pod

One share-manager pod runs nfs-ganesha for each RWX volume. So a consumer's IO takes two hops:

```
consumer  ->  share-manager  ->  replica
          (1)                (2)
```

1. Each consumer prefers the share-manager's node. That removes hop 1.
2. The share-manager prefers a node that holds a replica. That removes hop 2 for every consumer at once.

Both are pod moves, and the volume never moves. An RWX volume is often the largest in the cluster and is shared,
so copying it after the share-manager is the worst trade. Longhorn also documents `strict-local` as incompatible
with RWX, so no supported way to pin one exists.

Hop 2 needs a second webhook entry, scoped to Longhorn's namespace and to share-manager pods only. So no other
Longhorn pod reaches the webhook, and `failurePolicy: Ignore` still applies. `shareManager.enabled=false` turns
it off.

### Moving a share-manager that landed wrong

The webhook acts only when a pod is created. A share-manager already on a node with no replica stays there until
something recreates it, which can take a day or more. So the reconciler deletes it. Longhorn recreates the pod,
and the webhook places the new one on a replica node.

The delete drops the NFS export, and every consumer's mount stalls until nfs-ganesha is back. So the reconciler
guards it:

- The share-manager's node must hold none of the volume's replicas for a full `LRA_DWELL` first.
- At most one delete per volume per `LRA_MAX_BORROW`. Without that, a volume whose replica nodes the scheduler
  refuses would lose its share-manager on every tick.
- Inside that window, a share-manager still off its data reports
  `lra_volume_unfixable{reason="rwx-share-manager-moves"}` and waits.
- `LRA_MOVE_SHARE_MANAGER=false` keeps only the report. The chart then also drops `delete` on pods from the
  reconciler's RBAC.

## `reconcile`

For pods that a hard constraint keeps off their data, such as a device plugin, a node selector or the
architecture. A labelled pod still off its data means the preference lost to something hard.

1. Park the volume's `dataLocality` in an annotation.
2. Set `best-effort`. Longhorn adds a local replica, rebuilds it, then drops a remote one.
3. Once the replica is local and the count is back to `numberOfReplicas`, restore the parked value.

- Step 3 matters. `best-effort` left on would copy the volume on every future reschedule.
- Restoring the parked value keeps a volume whose StorageClass asked for `best-effort` on `best-effort`.
- The annotation lives on the volume, so a restart mid-flip still restores the right value.
- Step 3 waits for the whole cycle. Restoring between the rebuild and the trim leaves the volume over-replicated
  for good, because `dataLocality: disabled` gives Longhorn no reason to drop the surplus.
- `LRA_MAX_BORROW` is the backstop. Past it, the reconciler restores anyway, because `best-effort` held forever
  is worse than one surplus replica.

`LRA_DWELL`, `LRA_MAX_MOVE_BYTES` and `LRA_MAX_BORROW` guard every move.

## Install

```bash
helm install longhorn-replica-affinity \
  oci://ghcr.io/yama6a/charts/longhorn-replica-affinity \
  --namespace longhorn-replica-affinity --create-namespace
```

The chart ships at the same version as the image and defaults to it. It has no dependencies, because TLS
bootstraps itself (see TLS below).

It deploys two workloads with separate RBAC, because only one of them writes:

| | Replicas | RBAC |
|---|---|---|
| webhook | 2 | read pods, PVCs, `replicas.longhorn.io`, `volumes.longhorn.io` |
| reconciler | 1 | those reads, plus `patch` on `volumes.longhorn.io` |

The reconciler holds its dwell timers in memory, so run one. `reconciler.enabled=false` keeps only the webhook.

The chart ships no NetworkPolicy. The webhook needs ingress from the API server on 8443, and egress to DNS and
the API server.

## TLS

The API server calls a mutating webhook only over HTTPS, and it must trust the certificate. So the webhook needs a
serving cert and a published `caBundle`. Two modes provide them.

### self-signed (default)

- The webhook mints its own CA and leaf on startup.
- It stores them in a Secret, so every replica and every restart agree.
- It patches the CA into this chart's `MutatingWebhookConfiguration`.
- It rotates the cert in-process 90 days before expiry.

The cost is RBAC. The pod holds `get` and `patch` on one named `mutatingwebhookconfigurations`, and `get` and
`update` on one named Secret. `resourceNames` scopes both, but it is still a cluster-scoped write on an admission
object.

With Argo CD, the pod's `caBundle` write fights `selfHeal`, so tell Argo CD to ignore it:

```yaml
ignoreDifferences:
  - group: admissionregistration.k8s.io
    kind: MutatingWebhookConfiguration
    name: longhorn-replica-affinity
    jqPathExpressions: [".webhooks[].clientConfig.caBundle"]
```

### provided

The pod mounts the keypair from a Secret, and something else owns the `caBundle`. Use this when policy forbids a
workload holding `patch` on a `MutatingWebhookConfiguration`, or when you already issue certificates centrally.

With cert-manager, the chart renders a self-signed `Issuer`, a `Certificate` and the `inject-ca-from` annotation:

```yaml
tls:
  mode: provided
  certManager:
    enabled: true
    # optional, to use your own issuer instead of the rendered self-signed one
    # issuerRef: {name: my-issuer, kind: ClusterIssuer}
```

Without cert-manager, point at a Secret you manage and patch the `caBundle` yourself:

```yaml
tls:
  mode: provided
  secretName: my-webhook-tls
  certManager: {enabled: false}
```

- The certificate must cover `<release>-webhook.<namespace>.svc`.
- The Secret needs `tls.crt` and `tls.key`.
- The webhook reads the files again every 60 seconds, so rotation needs no restart.

## A real deployment

[yama6a/offgrid#50](https://github.com/yama6a/offgrid/pull/50) runs this on a 4-node Talos cluster:

- the [Argo CD Application](https://github.com/yama6a/offgrid/blob/main/argo_apps/platform/apps/templates/03_longhorn_replica_affinity.yaml),
  with the `ignoreDifferences` that self-signed mode needs
- a [wrapper chart](https://github.com/yama6a/offgrid/tree/main/argo_apps/platform/charts/03_longhorn_replica_affinity)
  that adds only a `CiliumNetworkPolicy`
- the opt-in label reaching pods through CNPG `inheritedMetadata`, a RabbitMQ `override.statefulSet`, and
  VictoriaMetrics `podMetadata`
- [docs/15_replica_affinity.md](https://github.com/yama6a/offgrid/blob/main/docs/15_replica_affinity.md) for the
  reasons

There, 15 of 31 attached volumes had a replica on their pod's node before, and 30 of 31 after. The one that never
converges is a CloudNativePG instance. Its required hostname anti-affinity outranks the preference, which is
correct.

## Opting in

Label the pod. `spec.affinity` is immutable, so nothing changes until the pod is recreated.

| Owner | Where |
|---|---|
| Deployment, StatefulSet, DaemonSet | `spec.template.metadata.labels` |
| CloudNativePG | `Cluster.spec.inheritedMetadata.labels` |
| RabbitMQ operator | `RabbitmqCluster.spec.override.statefulSet.spec.template.metadata.labels` |
| VictoriaMetrics operator | `spec.podMetadata.labels` |

## Configuration

| Variable | Default | Meaning |
|---|---|---|
| `LRA_LABEL_KEY` | `longhorn-replica-affinity/enabled` | opt-in label key |
| `LRA_LABEL_VALUE` | `true` | opt-in label value |
| `LRA_WEIGHT` | `30` | 1-100, per volume a node holds, multiplied by that count and capped at 100. Keep it below any hand-written preference that should win |
| `LRA_SKIP_RWX` | `false` | ignore RWX instead of preferring the share-manager's node |
| `LRA_LONGHORN_NAMESPACE` | `longhorn-system` | where the Longhorn CRs live |
| `LRA_LISTEN` | `:8443` | webhook TLS listener |
| `LRA_METRICS_LISTEN` | `:9100` | metrics listener |
| `LRA_TLS_MODE` | `self-signed` | `self-signed` or `provided`, see TLS |
| `LRA_TLS_CERT_FILE` | `/tls/tls.crt` | provided mode. Read again every minute |
| `LRA_TLS_KEY_FILE` | `/tls/tls.key` | provided mode |
| `LRA_TLS_SECRET` | `longhorn-replica-affinity-tls` | self-signed mode: the Secret that holds the generated keypair |
| `LRA_SERVICE_NAME` | `longhorn-replica-affinity-webhook` | self-signed mode: the Service the cert covers |
| `LRA_WEBHOOK_NAME` | `longhorn-replica-affinity` | self-signed mode: the webhook configuration whose `caBundle` gets the CA |
| `LRA_NAMESPACE` | | self-signed mode, required. Set it from `fieldRef` `metadata.namespace` |
| `LRA_RECONCILE_INTERVAL` | `1m` | reconciler tick |
| `LRA_DWELL` | `30m` | how long a pod sits off its data before anything moves |
| `LRA_MAX_BORROW` | `1h` | stop waiting for Longhorn to drop the surplus replica, and restore anyway |
| `LRA_MAX_MOVE_BYTES` | `5368709120` | never move a volume larger than this. Actual size, not provisioned |
| `LRA_FLIP_DATA_LOCALITY` | `true` | `false` makes `reconcile` observe only |
| `LRA_MOVE_SHARE_MANAGER` | `true` | delete a share-manager that sat off its data for `LRA_DWELL`, so Longhorn recreates it on a replica node. Costs a short NFS stall |
| `LRA_LOG_LEVEL` | `info` | `debug` logs every skipped admission |

## Metrics

`:9100/metrics`. Watch `sum(lra_volume_local) / count(lra_volume_local)`.

| Series | Meaning |
|---|---|
| `lra_volume_local{namespace,pvc,node,access_mode}` | 1 when an attached volume has a replica on its node. Counts running replicas, except for RWX, which counts replicas on disk so it does not drop to 0 while Longhorn restarts them |
| `lra_admissions_total{outcome}` | `injected`, `no-local-replica`, `pre-scheduled`, `cache-cold`, `decode` |
| `lra_data_locality_flips_total{direction}` | `borrow` or `restore` |
| `lra_volume_unfixable{namespace,pvc,access_mode,reason}` | a volume the reconciler will not move: `too-large`, `longhorn-managed`, `rwx-share-manager-moves` |
| `lra_share_manager_moves_total{namespace,pvc}` | share-managers deleted to get them onto a replica node |
| `lra_build_info{version}` | always 1 |

## Releases

Every merge to `main` cuts a release. CI fails a PR without exactly one of these labels:

| Label | On merge |
|---|---|
| `patch` | `v1.2.3` to `v1.2.4` |
| `minor` | `v1.2.3` to `v1.3.0` |
| `major` | `v1.2.3` to `v2.0.0` |
| `skip-release` | nothing built or tagged |

- Renovate labels its PRs `patch`, because a dependency bump changes no flag, env or behaviour. Relabel by hand
  when one does.
- The bump comes from every PR merged since the last release, and the strongest label wins. So a failed or
  cancelled run is absorbed by the next one. `skip-release` suppresses a release only when nothing else in that
  backlog asked for one.
- The release builds `linux/amd64` and `linux/arm64` into one manifest list, scans it and signs a provenance
  attestation.
- It packages the chart at the same version, with the image as its `appVersion`, and pushes both to GHCR.
- It creates the git tag last. So every tag has an image, and no chart points at an image that was never built.
- No floating `:latest`.

## Development

```bash
make ci         # tidy and generate drift, fmt, lint, vet, race tests, govulncheck, chart
make build
make chart      # helm lint, unit tests, render
make image      # multi-arch, no push
```

- Every decision is a pure function over an interface, so the tests need no cluster.
- Every values combination the chart supports is a helm-unittest case under
  `charts/longhorn-replica-affinity/tests/`.
- CI also runs kubeconform over the rendered chart, a `values.schema.json` drift check, actionlint, yamllint and a
  cross-compile of both release targets.

## Prior art

| Project | Mechanism | Soft? |
|---|---|---|
| Longhorn `dataLocality` | rebuilds a replica onto the pod's node | the volume moves, not the pod |
| Longhorn `strict-local` | `Required` nodeAffinity on the PV | no. PV affinity is immutable before Kubernetes 1.35 |
| [naver/longhorn-scheduler](https://github.com/naver/longhorn-scheduler) | a second scheduler with a plugin | no, it only filters |
| [linstor-affinity-controller](https://github.com/piraeusdatastore/linstor-affinity-controller) | syncs PV nodeAffinity, and recreates PVs to get around immutability | no |
| TopoLVM, OpenEBS LocalPV | PV nodeAffinity from CSI topology | no, and the storage is node-local |
| StorageOS pod locality | webhook plus scheduler extender | yes, but discontinued |

## License

MIT.
