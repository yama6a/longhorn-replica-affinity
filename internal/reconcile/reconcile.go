// Package reconcile moves data to the pod in the one case the webhook cannot help: a pod
// pinned by a hard constraint, on a node with no replica of its volume.
package reconcile

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"time"

	"go.uber.org/zap"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"

	"github.com/yama6a/longhorn-replica-affinity/internal/config"
	"github.com/yama6a/longhorn-replica-affinity/internal/index"
	"github.com/yama6a/longhorn-replica-affinity/internal/metrics"
)

// bestEffort is the only dataLocality that makes Longhorn rebuild a replica onto the
// pod's node.
const bestEffort = "best-effort"

// Store is the slice of the index the reconciler needs, as an interface so the decision
// logic is testable without informers.
type Store interface {
	AttachedVolumes() []index.Volume
	ReplicaNodes(volume string) []string
	ReplicaNodesOnDisk(volume string) []string
	ShareManagerPod(volume string) (name, node string, ok bool)
}

// Reconciler borrows a volume's dataLocality long enough to pull one replica local, then
// puts it back.
type Reconciler struct {
	Cfg   config.Config
	Index Store
	Dyn   dynamic.Interface
	Kube  kubernetes.Interface
	Log   *zap.Logger

	since    map[string]time.Time
	borrowed map[string]time.Time // when each in-flight borrow started, for the backstop
	moved    map[string]time.Time // when each share-manager was last deleted, for the cooldown
	now      func() time.Time     // swapped in tests to step over the dwell window
}

// Run reconciles on a ticker until ctx is cancelled.
func (r *Reconciler) Run(ctx context.Context) error {
	r.init()
	t := time.NewTicker(r.Cfg.ReconcileInterval)
	defer t.Stop()
	for {
		r.pass(ctx)
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		}
	}
}

func (r *Reconciler) init() {
	if r.since == nil {
		r.since = map[string]time.Time{}
	}
	if r.borrowed == nil {
		r.borrowed = map[string]time.Time{}
	}
	if r.moved == nil {
		r.moved = map[string]time.Time{}
	}
	if r.now == nil {
		r.now = time.Now
	}
}

func (r *Reconciler) pass(ctx context.Context) {
	r.init()
	metrics.ResetVolumes()
	seen := map[string]struct{}{}

	for _, v := range r.Index.AttachedVolumes() {
		seen[v.Name] = struct{}{}

		nodes := r.Index.ReplicaNodes(v.Name)
		if v.RWX() {
			// Longhorn stops every replica while it recreates a share-manager, so the
			// running-only view would drop this gauge to 0 for those seconds.
			nodes = r.Index.ReplicaNodesOnDisk(v.Name)
		}
		// For rwx the attached node is the share-manager's, so this measures the hop from
		// the share-manager to its replicas, which every consumer of the volume pays.
		isLocal := slices.Contains(nodes, v.AttachedNode)
		metrics.SetLocal(v.Namespace, v.PVCName, v.AttachedNode, v.AccessMode, isLocal)

		if v.RWX() {
			r.considerShareManagerMove(ctx, v)
			continue
		}

		if v.Restore != "" {
			r.considerRestore(ctx, v, isLocal, len(nodes))
			delete(r.since, v.Name)
			continue
		}
		if isLocal {
			delete(r.since, v.Name)
			continue
		}
		r.considerBorrow(ctx, v)
	}

	for _, m := range []map[string]time.Time{r.since, r.moved} {
		for name := range m {
			if _, ok := seen[name]; !ok {
				delete(m, name)
			}
		}
	}
}

// considerShareManagerMove deletes a share-manager that has sat on a node holding none of
// its volume's replicas for longer than the dwell. Longhorn recreates it and the
// sharemanager webhook entry puts the new one on a replica node, which collapses the
// share-manager-to-replica hop for every consumer at once. The volume itself never moves.
//
// The delete drops the NFS export, so every consumer's mount stalls until ganesha is back.
// That is why it waits out the dwell and then holds off for MaxBorrow.
func (r *Reconciler) considerShareManagerMove(ctx context.Context, v index.Volume) {
	pod, node, ok := r.Index.ShareManagerPod(v.Name)
	if !ok {
		delete(r.since, v.Name) // mid-recreation; nothing to judge and nothing to delete
		return
	}

	nodes := r.Index.ReplicaNodesOnDisk(v.Name)
	if len(nodes) == 0 || slices.Contains(nodes, node) {
		delete(r.since, v.Name)
		return
	}

	if !r.Cfg.MoveShareManager {
		metrics.SetUnfixable(v.Namespace, v.PVCName, v.AccessMode, "rwx-share-manager-moves")
		return
	}

	first, started := r.since[v.Name]
	if !started {
		r.since[v.Name] = r.now()
		return
	}
	if r.now().Sub(first) < r.Cfg.Dwell {
		return
	}

	// One delete per MaxBorrow. A volume whose replica nodes the scheduler will not take
	// would otherwise have its share-manager deleted on every pass, which is an outage
	// loop rather than a fix.
	if last, ever := r.moved[v.Name]; ever && r.now().Sub(last) < r.Cfg.MaxBorrow {
		metrics.SetUnfixable(v.Namespace, v.PVCName, v.AccessMode, "rwx-share-manager-moves")
		return
	}

	err := r.Kube.CoreV1().Pods(r.Cfg.LonghornNamespace).Delete(ctx, pod, metav1.DeleteOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		r.Log.Error("delete share-manager",
			zap.String("volume", v.Name), zap.String("pod", pod), zap.Error(err))
		return
	}
	r.moved[v.Name] = r.now()
	delete(r.since, v.Name)
	metrics.ShareManagerMove(v.Namespace, v.PVCName)
	r.Log.Info("deleting share-manager so Longhorn recreates it on a node holding a replica",
		zap.String("volume", v.Name), zap.String("pvc", v.Namespace+"/"+v.PVCName),
		zap.String("node", node), zap.Strings("replica_nodes", nodes))
}

func (r *Reconciler) considerBorrow(ctx context.Context, v index.Volume) {
	if !r.borrowable(ctx, v) {
		return
	}

	first, ok := r.since[v.Name]
	if !ok {
		r.since[v.Name] = r.now()
		return
	}
	if r.now().Sub(first) < r.Cfg.Dwell {
		return
	}

	if err := r.patch(ctx, v.Name, bestEffort, v.DataLocality); err != nil {
		r.Log.Error("borrow dataLocality", zap.String("volume", v.Name), zap.Error(err))
		return
	}
	r.borrowed[v.Name] = r.now()
	metrics.Flip("borrow")
	r.Log.Info("borrowing dataLocality to pull a replica local",
		zap.String("volume", v.Name), zap.String("pvc", v.Namespace+"/"+v.PVCName),
		zap.String("node", v.AttachedNode), zap.Int64("bytes", v.ActualSize),
		zap.String("restore_to", v.DataLocality))
}

// borrowable reports whether the volume is one the reconciler may act on at all, before
// the dwell window is considered.
func (r *Reconciler) borrowable(ctx context.Context, v index.Volume) bool {
	if !r.Cfg.FlipDataLocality || v.Restore != "" {
		return false // already borrowed; Longhorn is mid-rebuild
	}

	// Never move an rwx volume. pass routes those to considerShareManagerMove, and copying
	// a shared volume around would be the exact thing this project exists to avoid.
	if v.RWX() {
		return false
	}

	opted, err := r.optedIn(ctx, v)
	if err != nil {
		r.Log.Error("check opt-in", zap.String("volume", v.Name), zap.Error(err))
		return false
	}
	if !opted {
		return false
	}

	if v.DataLocality != "disabled" {
		// Already best-effort or strict-local. Longhorn owns the outcome; nothing to borrow.
		metrics.SetUnfixable(v.Namespace, v.PVCName, v.AccessMode, "longhorn-managed")
		return false
	}
	if v.ActualSize > r.Cfg.MaxMoveBytes {
		metrics.SetUnfixable(v.Namespace, v.PVCName, v.AccessMode, "too-large")
		return false
	}
	return true
}

// considerRestore ends a borrow, but only once Longhorn has finished the whole
// best-effort cycle. It adds the local replica, rebuilds it, and only THEN deletes a
// remote one to get back to numberOfReplicas. Restoring between those last two steps
// leaves the volume permanently over-replicated, because a volume on dataLocality
// disabled gives Longhorn no reason to trim.
func (r *Reconciler) considerRestore(ctx context.Context, v index.Volume, isLocal bool, have int) {
	held := time.Duration(0)
	if started, ok := r.borrowed[v.Name]; ok {
		held = r.now().Sub(started)
	}
	overReplicated := v.WantReplicas > 0 && have > v.WantReplicas

	switch {
	case isLocal && !overReplicated:
	case held > r.Cfg.MaxBorrow:
		// Give up waiting rather than leave best-effort on: that would drag a copy on
		// every future reschedule, which is worse than one surplus replica.
		r.Log.Warn("borrow held too long, restoring anyway",
			zap.String("volume", v.Name), zap.String("pvc", v.Namespace+"/"+v.PVCName),
			zap.Bool("local", isLocal), zap.Int("replicas", have),
			zap.Int("want", v.WantReplicas), zap.Duration("held", held))
	default:
		return // still rebuilding, or Longhorn has not trimmed the surplus yet
	}

	if err := r.patch(ctx, v.Name, v.Restore, ""); err != nil {
		r.Log.Error("restore dataLocality", zap.String("volume", v.Name), zap.Error(err))
		return
	}
	delete(r.borrowed, v.Name)
	metrics.Flip("restore")
	r.Log.Info("replica is local and replica count is back to normal, dataLocality restored",
		zap.String("volume", v.Name), zap.String("pvc", v.Namespace+"/"+v.PVCName),
		zap.String("value", v.Restore))
}

// patch sets spec.dataLocality. A non-empty restore parks the previous value in the
// annotation; an empty one clears it, ending the borrow.
func (r *Reconciler) patch(ctx context.Context, name, locality, restore string) error {
	ann := map[string]any{r.Cfg.RestoreAnnotation(): nil}
	if restore != "" {
		ann[r.Cfg.RestoreAnnotation()] = restore
	}
	body, err := json.Marshal(map[string]any{
		"metadata": map[string]any{"annotations": ann},
		"spec":     map[string]any{"dataLocality": locality},
	})
	if err != nil {
		return fmt.Errorf("marshal patch: %w", err)
	}
	_, err = r.Dyn.Resource(index.VolumeGVR).Namespace(r.Cfg.LonghornNamespace).
		Patch(ctx, name, types.MergePatchType, body, metav1.PatchOptions{})
	if err != nil {
		return fmt.Errorf("patch volume %s: %w", name, err)
	}
	return nil
}

// optedIn reports whether any pod consuming the volume carries the opt-in label. A
// labelled pod still off its data means the preference lost to something hard.
func (r *Reconciler) optedIn(ctx context.Context, v index.Volume) (bool, error) {
	if v.Namespace == "" || len(v.PodNames) == 0 {
		return false, nil
	}
	for _, name := range v.PodNames {
		pod, err := r.Kube.CoreV1().Pods(v.Namespace).Get(ctx, name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			continue
		}
		if err != nil {
			return false, fmt.Errorf("get pod %s/%s: %w", v.Namespace, name, err)
		}
		if pod.Labels[r.Cfg.LabelKey] == r.Cfg.LabelValue {
			return true, nil
		}
	}
	return false, nil
}
