// Package reconcile moves data to the pod in the one case the webhook cannot help:
// a pod pinned by a hard constraint, sitting on a node with no replica of its volume.
package reconcile

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"slices"
	"time"

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
// node its pod already runs on.
const bestEffort = "best-effort"

// Store is the slice of the replica index the reconciler needs. An interface so the
// decision logic can be tested without informers.
type Store interface {
	AttachedVolumes() []index.Volume
	ReplicaNodes(volume string) []string
}

// Reconciler borrows a volume's dataLocality field long enough to pull one replica
// local, then puts it back.
type Reconciler struct {
	Cfg   config.Config
	Index Store
	Dyn   dynamic.Interface
	Kube  kubernetes.Interface
	Log   *slog.Logger

	since map[string]time.Time
	now   func() time.Time // swapped in tests to step over the dwell window
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
		isLocal := slices.Contains(r.Index.ReplicaNodes(v.Name), v.AttachedNode)
		metrics.SetLocal(v.Namespace, v.PVCName, v.AttachedNode, isLocal)

		if isLocal {
			delete(r.since, v.Name)
			if v.Restore != "" {
				r.restore(ctx, v)
			}
			continue
		}
		r.considerBorrow(ctx, v)
	}

	for name := range r.since {
		if _, ok := seen[name]; !ok {
			delete(r.since, name)
		}
	}
}

func (r *Reconciler) considerBorrow(ctx context.Context, v index.Volume) {
	if !r.Cfg.FlipDataLocality || v.Restore != "" {
		return // already borrowed; Longhorn is mid-rebuild
	}

	opted, err := r.optedIn(ctx, v)
	if err != nil {
		r.Log.Error("check opt-in", "volume", v.Name, "err", err)
		return
	}
	if !opted {
		return
	}

	if v.DataLocality != "disabled" {
		// Already best-effort or strict-local. Longhorn owns the outcome; nothing to borrow.
		metrics.SetUnfixable(v.Namespace, v.PVCName, "longhorn-managed")
		return
	}
	if v.ActualSize > r.Cfg.MaxMoveBytes {
		metrics.SetUnfixable(v.Namespace, v.PVCName, "too-large")
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
		r.Log.Error("borrow dataLocality", "volume", v.Name, "err", err)
		return
	}
	metrics.Flip("borrow")
	r.Log.Info("borrowing dataLocality to pull a replica local",
		"volume", v.Name, "pvc", v.Namespace+"/"+v.PVCName,
		"node", v.AttachedNode, "bytes", v.ActualSize, "restore_to", v.DataLocality)
}

func (r *Reconciler) restore(ctx context.Context, v index.Volume) {
	if err := r.patch(ctx, v.Name, v.Restore, ""); err != nil {
		r.Log.Error("restore dataLocality", "volume", v.Name, "err", err)
		return
	}
	metrics.Flip("restore")
	r.Log.Info("replica is local, dataLocality restored",
		"volume", v.Name, "pvc", v.Namespace+"/"+v.PVCName, "value", v.Restore)
}

// patch sets spec.dataLocality. A non-empty restore parks the previous value in the
// annotation; an empty one clears it, which is what ends a borrow.
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
// labelled pod that is still off its data means the preference lost to a hard
// constraint, which is the definition of a pod that cannot move.
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
