# longhorn-replica-affinity

Soft pod-follows-volume scheduling for [Longhorn](https://longhorn.io). A mutating webhook
nudges pods onto nodes that already hold a replica of their data, instead of hauling the
volume across the network to the pod.

## The problem

Longhorn replicates a volume onto some of your nodes. The Kubernetes scheduler knows
nothing about which ones, because a Longhorn PV is network-attachable and therefore
carries no `nodeAffinity`. So a pod routinely lands on a node holding no copy of its own
data, and every read and write crosses the network for the life of that pod.

Longhorn's own answer is `dataLocality: best-effort`, which moves the **volume**: it
rebuilds a full replica onto the pod's node, then drops a remote one. One whole volume
transfer per pod move. On a small cluster with 1GbE that is the expensive direction.

Moving the pod costs one restart and zero bytes.

This is the design Longhorn contributors wrote up in
[longhorn#12398](https://github.com/longhorn/longhorn/issues/12398) and never shipped
(the PR was closed unmerged, and the follow-up scheduler extender in
[longhorn#12591](https://github.com/longhorn/longhorn/issues/12591) was abandoned for
adding head-of-line latency to pods that do not use Longhorn). StorageOS shipped the same
shape as a product before it was discontinued. Nothing maintained does it today.

## What it does

Two subcommands off one binary.

### `webhook`

Mutating admission on pod `CREATE`. For each PVC the pod mounts, it finds the nodes
holding a running replica and appends a `preferredDuringSchedulingIgnoredDuringExecution`
node affinity term for each, weighted by how many of the pod's volumes that node holds.

- Soft, always. A node with no replica can still take the pod, exactly as today.
- `requiredDuringScheduling` is never touched, and existing preferred terms are kept.
- RWX volumes target the **share-manager** pod's node instead of the replica nodes: the
  consumer mounts nfs-ganesha, not a replica, so that is the placement that saves a hop.
- `failurePolicy: Ignore` is the intended deployment. This optimises placement; it must
  never be able to stop it.
- No API calls on the admission path. Informers keep the replica map in memory, so an
  API server blip cannot stall pod creation.

### `reconcile`

For the one case the webhook cannot fix: a pod pinned by a hard constraint (a device
plugin resource, a node selector, an architecture) sitting on a node with no replica.

The opt-in label is the test. A labelled pod that is *still* off its data means the
preference lost to a hard constraint, which is the definition of a pod that cannot move.
For those, and only those, the reconciler moves the data instead:

1. Park the volume's current `dataLocality` in an annotation.
2. Set `dataLocality: best-effort`. Longhorn rebuilds a replica onto the pod's node.
3. Once that replica is running, restore the parked value and clear the annotation.

Step 3 matters. Leaving `best-effort` on would re-enable volume-follows-pod forever,
dragging a copy on every future reschedule. Restoring the **parked** value rather than a
hardcoded default is what keeps a volume whose StorageClass asked for `best-effort` on
`best-effort`. The annotation lives on the object, not in memory, so a restart mid-flip
still restores the right value.

Guarded by a dwell time (a rolling update must not trigger a copy) and a size ceiling (a
600Gi share is not something to move quietly). Both configurable; the flip can be turned
off entirely.

## Install

Needs a TLS serving cert. cert-manager with a self-signed `Issuer` plus
`cert-manager.io/inject-ca-from` on the `MutatingWebhookConfiguration` is the least
moving parts.

```yaml
image: ghcr.io/yama6a/longhorn-replica-affinity:v0.1.0
```

Run the two subcommands as two Deployments, so their RBAC can differ:

| | replicas | RBAC |
|---|---|---|
| `webhook` | 2 | read pods, PVCs, `replicas.longhorn.io`, `volumes.longhorn.io` |
| `reconcile` | 1 | the same reads, plus `patch` on `volumes.longhorn.io` |

The webhook is stateless and safe to run several of. The reconciler holds its dwell
timers in memory, so run one.

`MutatingWebhookConfiguration` essentials:

```yaml
failurePolicy: Ignore       # optimise placement, never block it
sideEffects: None
timeoutSeconds: 5
objectSelector:             # the apiserver skips the call entirely for everything else
  matchLabels:
    longhorn-replica-affinity/enabled: "true"
namespaceSelector:          # never mutate what has to come up before this does
  matchExpressions:
    - {key: kubernetes.io/metadata.name, operator: NotIn, values: [kube-system, longhorn-system]}
rules:
  - operations: [CREATE]
    apiGroups: [""]
    apiVersions: [v1]
    resources: [pods]
```

## Opting a workload in

Label the **pod**, not the workload object. Where that lives depends on who creates the
pod:

| Owner | Where the label goes |
|---|---|
| Deployment / StatefulSet / DaemonSet | `spec.template.metadata.labels` |
| CloudNativePG | `Cluster.spec.inheritedMetadata.labels` |
| RabbitMQ cluster operator | `RabbitmqCluster.spec.override.statefulSet.spec.template.metadata.labels` |
| VictoriaMetrics operator | `spec.podMetadata.labels` |

Nothing happens until a pod is recreated: `spec.affinity` is immutable, so a running pod
keeps whatever it was scheduled with. Existing pods drift into place on ordinary churn.

## Configuration

Everything is `LRA_*` environment variables.

| Variable | Default | Meaning |
|---|---|---|
| `LRA_LABEL_KEY` | `longhorn-replica-affinity/enabled` | opt-in label key |
| `LRA_LABEL_VALUE` | `true` | opt-in label value |
| `LRA_WEIGHT` | `30` | weight per volume a node holds, 1-100. Multiplied by the number of the pod's volumes on that node, capped at 100. Keep it under any hand-written preference you want to win |
| `LRA_SKIP_RWX` | `false` | ignore RWX volumes instead of targeting the share-manager |
| `LRA_LONGHORN_NAMESPACE` | `longhorn-system` | where the Longhorn CRs live |
| `LRA_LISTEN` | `:8443` | webhook TLS listener |
| `LRA_METRICS_LISTEN` | `:9100` | metrics listener |
| `LRA_TLS_CERT_FILE` | `/tls/tls.crt` | reloaded every minute, so cert rotation needs no restart |
| `LRA_TLS_KEY_FILE` | `/tls/tls.key` | |
| `LRA_RECONCILE_INTERVAL` | `1m` | reconciler tick |
| `LRA_DWELL` | `30m` | how long a pod must sit off its data before the reconciler moves the data |
| `LRA_MAX_MOVE_BYTES` | `5368709120` | never move a volume larger than this (actual, not provisioned) |
| `LRA_FLIP_DATA_LOCALITY` | `true` | set false to make `reconcile` observe-only |
| `LRA_LOG_LEVEL` | `info` | `debug` logs every skipped admission |

## Metrics

Prometheus format on `:9100/metrics`.

| Series | Type | Meaning |
|---|---|---|
| `lra_volume_local{namespace,pvc,node}` | gauge | 1 when an attached volume has a running replica on its own node |
| `lra_admissions_total{outcome}` | counter | `injected`, `no-local-replica`, `cache-cold`, `decode` |
| `lra_data_locality_flips_total{direction}` | counter | `borrow` / `restore` |
| `lra_volume_unfixable{namespace,pvc,reason}` | gauge | the reconciler will not move this one: `too-large`, `longhorn-managed` |
| `lra_build_info{version}` | gauge | always 1 |

`sum(lra_volume_local) / count(lra_volume_local)` is the number worth watching.

## Releases

Every merge to `main` cuts a release, and the bump comes from a label on the PR. CI fails
a PR that does not carry exactly one of:

| Label | Effect on merge |
|---|---|
| `patch` | `v1.2.3` becomes `v1.2.4` |
| `minor` | `v1.2.3` becomes `v1.3.0` |
| `major` | `v1.2.3` becomes `v2.0.0` |
| `skip-release` | nothing is built or tagged |

Renovate labels its own PRs `patch` automatically. A dependency bump is a patch release
of this tool whatever the dependency's own bump type was: the public surface here is the
flags, the env and the behaviour, and a library moving does not move those. Relabel by
hand on the rare bump that does.

A direct push to `main` has no PR and therefore no label, so it ships nothing rather than
guessing.

The release job builds `linux/amd64` and `linux/arm64` into one manifest list, pushes it
to `ghcr.io/yama6a/longhorn-replica-affinity`, and only then creates the git tag, so a tag
always has an image behind it. There is no floating `:latest`.

## Development

```bash
make ci       # vet + lint + test
make build    # bin/longhorn-replica-affinity
make image    # multi-arch build, no push
```

Everything that decides anything is a pure function over an interface, so the tests need
no cluster and no envtest.

## Prior art

| Project | Mechanism | Soft? |
|---|---|---|
| Longhorn `dataLocality` | rebuilds a replica onto the pod's node | volume moves, not the pod |
| Longhorn `strict-local` | writes `Required` nodeAffinity onto the PV | no, and PV affinity is immutable before Kubernetes 1.35 |
| [naver/longhorn-scheduler](https://github.com/naver/longhorn-scheduler) | scheduler framework plugin, second scheduler | no, Filter-only |
| [linstor-affinity-controller](https://github.com/piraeusdatastore/linstor-affinity-controller) | syncs PV nodeAffinity, recreating PVs to dodge immutability | no |
| TopoLVM, OpenEBS LocalPV | PV nodeAffinity from CSI topology at provisioning | no, and the storage is genuinely node-local |
| StorageOS pod locality | webhook plus a scheduler extender, preferred or strict | yes, discontinued |

PV `nodeAffinity` has no `Preferred` field, so a soft preference has to live on the pod.
That is the whole reason this is a webhook.

## License

MIT.
