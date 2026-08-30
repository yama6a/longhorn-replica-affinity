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

## RWX: two hops, both fixed by moving a pod

An RWX volume is served by exactly one share-manager pod running nfs-ganesha, so there are
two hops, and neither is fixed by moving data:

```
consumer  ->  share-manager  ->  replica
          (1)                (2)
```

1. Every consumer preferring the share-manager's node collapses this one.
2. The share-manager preferring a node that holds a replica collapses this one, for all
   consumers at once.

Both are pod moves. The volume never moves, which matters most here: an RWX volume is
usually the largest thing in the cluster and is shared, so copying it to chase whichever
node the share-manager landed on is the worst possible trade.

The reconciler therefore refuses to touch RWX volumes at all and reports
`lra_volume_unfixable{reason="rwx-share-manager-moves"}` instead. Longhorn also documents
`strict-local` as incompatible with RWX, so there is no supported way to pin one anyway.

Hop 2 needs a webhook entry scoped to Longhorn's own namespace, which is why it is a
separate entry rather than a hole in the namespace exclusion: the `objectSelector` means
the API server never calls this for any other Longhorn pod, so the storage layer's own
bootstrap is untouched, and `failurePolicy: Ignore` still applies. Turn it off with
`shareManager.enabled=false`.

## `reconcile`

For pods pinned by a hard constraint (device plugin, node selector, architecture) that
cannot move to their data. The opt-in label is the test: a labelled pod still off its data
means the preference lost to something hard.

1. Park the volume's `dataLocality` in an annotation.
2. Set `best-effort`. Longhorn adds a local replica, rebuilds it, then drops a remote one.
3. Once the replica is local **and** the count is back to `numberOfReplicas`, restore the parked value.

Step 3 is the point. Leaving `best-effort` on would drag a copy on every future
reschedule. Restoring the parked value rather than a default keeps a volume whose
StorageClass asked for `best-effort` on `best-effort`. The annotation is on the object, so
a restart mid-flip still restores correctly.

Step 3 waits for the whole cycle. Restoring between the rebuild and the trim leaves the
volume permanently over-replicated, because `dataLocality: disabled` gives Longhorn no
reason to drop the surplus. `LRA_MAX_BORROW` is the backstop: past it, restore anyway,
since holding `best-effort` forever is worse than one extra replica.

Guarded by `LRA_DWELL`, `LRA_MAX_MOVE_BYTES` and `LRA_MAX_BORROW`.

## Install

```bash
helm install longhorn-replica-affinity \
  oci://ghcr.io/yama6a/charts/longhorn-replica-affinity \
  --namespace longhorn-replica-affinity --create-namespace
```

The chart ships at the same version as the image and defaults to it, so the two can never
drift. No dependencies: TLS is self-bootstrapped (see below).

It deploys two workloads with separate RBAC, because only one of them writes:

| | replicas | RBAC |
|---|---|---|
| webhook | 2 | read pods, PVCs, `replicas.longhorn.io`, `volumes.longhorn.io` |
| reconciler | 1 | those reads, plus `patch` on `volumes.longhorn.io` |

The reconciler holds dwell timers in memory, so run one. Disable it with
`reconciler.enabled=false` if you only want the scheduling half.

The chart ships no NetworkPolicy. Bring your own to match whatever your cluster enforces;
the webhook needs ingress from the API server on 8443 and egress to DNS and the API server.

## TLS

The API server only ever calls a mutating webhook over HTTPS and has to trust the
certificate, so a serving cert and a published `caBundle` are not optional. Two ways to
get them.

### self-signed (default)

The webhook mints its own CA and leaf on startup, stores them in a Secret so every replica
and every restart agree, and patches the CA into this chart's
`MutatingWebhookConfiguration`. Rotation happens in-process 90 days before expiry.
Nothing else is required.

The cost is RBAC: the pod holds `get`/`patch` on one named
`mutatingwebhookconfigurations` and `get`/`update` on one named Secret. Both are scoped
with `resourceNames`, but it is still a cluster-scoped write on an admission object.

**With ArgoCD**, the pod writing `caBundle` will fight `selfHeal`, so tell Argo to ignore it:

```yaml
ignoreDifferences:
  - group: admissionregistration.k8s.io
    kind: MutatingWebhookConfiguration
    name: longhorn-replica-affinity
    jqPathExpressions: [".webhooks[].clientConfig.caBundle"]
```

### provided

The keypair is mounted from a Secret and something else owns the `caBundle`. Use this when
policy forbids a workload holding `patch` on a `MutatingWebhookConfiguration`, or when you
already centralise certificate issuance.

With cert-manager, which renders a self-signed `Issuer`, a `Certificate` and the
`inject-ca-from` annotation for you:

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

The certificate must cover
`<release>-webhook.<namespace>.svc`, and the Secret needs the usual `tls.crt` / `tls.key`.
The file is re-read every 60 seconds, so rotation needs no restart either way.

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
| `LRA_TLS_MODE` | `self-signed` | `self-signed` or `provided`, see TLS above |
| `LRA_TLS_CERT_FILE` | `/tls/tls.crt` | `provided` mode; re-read every minute so rotation needs no restart |
| `LRA_TLS_KEY_FILE` | `/tls/tls.key` | `provided` mode |
| `LRA_TLS_SECRET` | `longhorn-replica-affinity-tls` | `self-signed` mode: where the generated keypair is kept |
| `LRA_SERVICE_NAME` | `longhorn-replica-affinity-webhook` | `self-signed` mode: the Service name to issue for |
| `LRA_WEBHOOK_NAME` | `longhorn-replica-affinity` | `self-signed` mode: whose `caBundle` to publish into |
| `LRA_NAMESPACE` | | `self-signed` mode, required; set it from `fieldRef` `metadata.namespace` |
| `LRA_RECONCILE_INTERVAL` | `1m` | reconciler tick |
| `LRA_DWELL` | `30m` | how long a pod sits off its data before the data moves |
| `LRA_MAX_BORROW` | `1h` | give up waiting for Longhorn to trim the surplus replica and restore anyway |
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
| `lra_volume_unfixable{namespace,pvc,access_mode,reason}` | not moving this one: `too-large`, `longhorn-managed`, `rwx-share-manager-moves` |
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

The bump is recomputed from every PR merged since the last release, strongest label
winning, so a run that fails or gets cancelled by the concurrency queue is absorbed by the
next one rather than losing its release. `skip-release` only suppresses when nothing else
in the backlog asked for a release.

Builds `linux/amd64` and `linux/arm64` into one manifest list, packages the chart at the
same version with the image pinned as its `appVersion`, pushes both to GHCR, and only then
creates the git tag. So a tag always has an image, and a chart can never reference an image
that was never built. No floating `:latest`.

## Development

```bash
make ci         # fmt + vet + lint + race tests + govulncheck + chart render
make build
make chart      # helm lint + render every values combination
make image      # multi-arch, no push
```

Everything that decides anything is a pure function over an interface, so the tests need
no cluster. CI additionally runs kubeconform over every rendered chart combination,
actionlint, yamllint, a `go mod tidy` check and a cross-compile of both release targets.

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
