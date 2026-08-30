# longhorn-replica-affinity

Soft pod-follows-volume scheduling for [Longhorn](https://longhorn.io). A mutating webhook
nudges pods onto nodes that already hold a replica of their data, instead of hauling the
volume across the network to the pod.

## Why

A Longhorn PV is network-attachable and carries no `nodeAffinity`, so the scheduler cannot
see where replicas live. Pods land anywhere and their IO crosses the network for the life
of the pod.

Longhorn's `dataLocality: best-effort` fixes that by moving the **volume**: a full replica
rebuild onto the pod's node, per pod move. Moving the pod instead costs one restart and
zero bytes.

Designed but never shipped upstream:
[longhorn#12398](https://github.com/longhorn/longhorn/issues/12398) (webhook, PR closed
unmerged) and [longhorn#12591](https://github.com/longhorn/longhorn/issues/12591)
(scheduler extender, abandoned for adding head-of-line latency to unrelated pods).

## `webhook`

Mutating admission on pod `CREATE`. Per PVC, finds the nodes with a running replica and
appends a `preferredDuringSchedulingIgnoredDuringExecution` term for each, weighted by how
many of the pod's volumes that node holds.

- Soft. A node with no replica can still take the pod.
- `requiredDuringScheduling` and existing preferred terms are left alone.
- RWX targets the share-manager's node, not the replica nodes: the consumer mounts
  nfs-ganesha, so that is the hop worth saving.
- No API calls on the admission path. Informers hold the replica map in memory.
- Deploy with `failurePolicy: Ignore`. This optimises placement; it must never block it.

## `reconcile`

For pods pinned by a hard constraint (device plugin, node selector, architecture) that
cannot move to their data. The opt-in label is the test: a labelled pod still off its data
means the preference lost to something hard.

1. Park the volume's `dataLocality` in an annotation.
2. Set `best-effort`. Longhorn rebuilds a replica onto the pod's node.
3. Once it is running, restore the parked value and clear the annotation.

Step 3 is the point. Leaving `best-effort` on would drag a copy on every future
reschedule. Restoring the parked value rather than a default keeps a volume whose
StorageClass asked for `best-effort` on `best-effort`. The annotation is on the object, so
a restart mid-flip still restores correctly.

Guarded by `LRA_DWELL` and `LRA_MAX_MOVE_BYTES`.

## Install

Needs a TLS serving cert; cert-manager with a self-signed `Issuer` plus
`cert-manager.io/inject-ca-from` is the fewest moving parts.

Two Deployments off one image, so their RBAC can differ:

| | replicas | RBAC |
|---|---|---|
| `webhook` | 2 | read pods, PVCs, `replicas.longhorn.io`, `volumes.longhorn.io` |
| `reconcile` | 1 | those reads, plus `patch` on `volumes.longhorn.io` |

The reconciler holds dwell timers in memory, so run one.

```yaml
image: ghcr.io/yama6a/longhorn-replica-affinity:v0.1.0
```

```yaml
# MutatingWebhookConfiguration
failurePolicy: Ignore
sideEffects: None
timeoutSeconds: 5
objectSelector:            # the apiserver skips the call entirely for everything else
  matchLabels:
    longhorn-replica-affinity/enabled: "true"
namespaceSelector:
  matchExpressions:
    - {key: kubernetes.io/metadata.name, operator: NotIn, values: [kube-system, longhorn-system]}
rules:
  - operations: [CREATE]
    apiGroups: [""]
    apiVersions: [v1]
    resources: [pods]
```

## Opting in

Label the **pod**. `spec.affinity` is immutable, so nothing changes until a pod is
recreated.

| Owner | Where |
|---|---|
| Deployment / StatefulSet / DaemonSet | `spec.template.metadata.labels` |
| CloudNativePG | `Cluster.spec.inheritedMetadata.labels` |
| RabbitMQ operator | `RabbitmqCluster.spec.override.statefulSet.spec.template.metadata.labels` |
| VictoriaMetrics operator | `spec.podMetadata.labels` |

## Configuration

| Variable | Default | Meaning |
|---|---|---|
| `LRA_LABEL_KEY` | `longhorn-replica-affinity/enabled` | opt-in label key |
| `LRA_LABEL_VALUE` | `true` | opt-in label value |
| `LRA_WEIGHT` | `30` | weight per volume a node holds, 1-100, multiplied by that count and capped at 100. Keep under any hand-written preference that should win |
| `LRA_SKIP_RWX` | `false` | ignore RWX instead of targeting the share-manager |
| `LRA_LONGHORN_NAMESPACE` | `longhorn-system` | where the Longhorn CRs live |
| `LRA_LISTEN` | `:8443` | webhook TLS listener |
| `LRA_METRICS_LISTEN` | `:9100` | metrics listener |
| `LRA_TLS_CERT_FILE` | `/tls/tls.crt` | reloaded every minute, so rotation needs no restart |
| `LRA_TLS_KEY_FILE` | `/tls/tls.key` | |
| `LRA_RECONCILE_INTERVAL` | `1m` | reconciler tick |
| `LRA_DWELL` | `30m` | how long a pod sits off its data before the data moves |
| `LRA_MAX_MOVE_BYTES` | `5368709120` | never move a volume larger than this (actual, not provisioned) |
| `LRA_FLIP_DATA_LOCALITY` | `true` | false makes `reconcile` observe-only |
| `LRA_LOG_LEVEL` | `info` | `debug` logs every skipped admission |

## Metrics

`:9100/metrics`. Watch `sum(lra_volume_local) / count(lra_volume_local)`.

| Series | Meaning |
|---|---|
| `lra_volume_local{namespace,pvc,node}` | 1 when an attached volume has a running replica on its own node |
| `lra_admissions_total{outcome}` | `injected`, `no-local-replica`, `pre-scheduled`, `cache-cold`, `decode` |
| `lra_data_locality_flips_total{direction}` | `borrow` / `restore` |
| `lra_volume_unfixable{namespace,pvc,reason}` | not moving this one: `too-large`, `longhorn-managed` |
| `lra_build_info{version}` | always 1 |

## Releases

Every merge to `main` cuts one. CI fails a PR without exactly one of:

| Label | On merge |
|---|---|
| `patch` | `v1.2.3` to `v1.2.4` |
| `minor` | `v1.2.3` to `v1.3.0` |
| `major` | `v1.2.3` to `v2.0.0` |
| `skip-release` | nothing built or tagged |

Renovate labels its own PRs `patch`: a dependency bump does not move this tool's flags,
env or behaviour. Relabel by hand on the rare one that does.

A direct push to `main` has no PR, so it ships nothing.

Builds `linux/amd64` and `linux/arm64` into one manifest list, pushes to GHCR, and only
then tags, so a tag always has an image. No floating `:latest`.

## Development

```bash
make ci      # vet + lint + test
make build
make image   # multi-arch, no push
```

Everything that decides anything is a pure function over an interface, so the tests need
no cluster.

## Prior art

| Project | Mechanism | Soft? |
|---|---|---|
| Longhorn `dataLocality` | rebuilds a replica onto the pod's node | the volume moves, not the pod |
| Longhorn `strict-local` | `Required` nodeAffinity on the PV | no, and PV affinity is immutable before Kubernetes 1.35 |
| [naver/longhorn-scheduler](https://github.com/naver/longhorn-scheduler) | scheduler plugin, second scheduler | no, Filter-only |
| [linstor-affinity-controller](https://github.com/piraeusdatastore/linstor-affinity-controller) | syncs PV nodeAffinity, recreating PVs to dodge immutability | no |
| TopoLVM, OpenEBS LocalPV | PV nodeAffinity from CSI topology | no, and the storage is genuinely node-local |
| StorageOS pod locality | webhook plus scheduler extender | yes, discontinued |

PV `nodeAffinity` has no `Preferred` field, which is why this is a webhook.

## License

MIT.
