// Package index keeps an in-memory view of Longhorn replica placement, so the admission
// path never calls the API server.
package index

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/dynamic/dynamicinformer"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
)

var (
	ReplicaGVR = schema.GroupVersionResource{Group: "longhorn.io", Version: "v1beta2", Resource: "replicas"}
	VolumeGVR  = schema.GroupVersionResource{Group: "longhorn.io", Version: "v1beta2", Resource: "volumes"}
)

// ShareManagerSelector matches the nfs-ganesha pod Longhorn runs per RWX volume.
const ShareManagerSelector = "longhorn.io/component=share-manager"

// ShareManagerPrefix is how Longhorn names that pod: share-manager-<volume>.
const ShareManagerPrefix = "share-manager-"

// VolumeForShareManager returns the volume a share-manager pod serves, or "" if the pod
// is not one. Longhorn derives the pod name from the volume name, so this needs no lookup.
func VolumeForShareManager(podName string) string {
	return strings.TrimPrefix(podName, ShareManagerPrefix)
}

// IsShareManager reports whether a pod name is a share-manager's.
func IsShareManager(podName string) bool {
	return strings.HasPrefix(podName, ShareManagerPrefix)
}

// Volume is the subset of a Longhorn Volume this program acts on.
type Volume struct {
	Name         string
	AttachedNode string
	DataLocality string
	AccessMode   string
	ActualSize   int64
	WantReplicas int
	Namespace    string
	PVCName      string
	PodNames     []string
	Restore      string
}

// RWX reports whether the volume is served through a share-manager.
func (v Volume) RWX() bool { return v.AccessMode == "rwx" }

// Index answers "which nodes hold this volume" without touching the API server.
type Index struct {
	replicas   cache.SharedIndexInformer
	volumes    cache.SharedIndexInformer
	pods       cache.SharedIndexInformer
	pvcs       cache.SharedIndexInformer
	restoreKey string

	mu     sync.RWMutex
	synced bool
}

// New wires informers scoped to the Longhorn namespace.
func New(dc dynamic.Interface, kc kubernetes.Interface, namespace, restoreKey string) *Index {
	df := dynamicinformer.NewFilteredDynamicSharedInformerFactory(dc, 0, namespace, nil)
	kf := informers.NewSharedInformerFactoryWithOptions(kc, 0,
		informers.WithNamespace(namespace),
		informers.WithTweakListOptions(func(o *metav1.ListOptions) { o.LabelSelector = ShareManagerSelector }),
	)
	all := informers.NewSharedInformerFactory(kc, 0)
	return &Index{
		replicas:   df.ForResource(ReplicaGVR).Informer(),
		volumes:    df.ForResource(VolumeGVR).Informer(),
		pods:       kf.Core().V1().Pods().Informer(),
		pvcs:       all.Core().V1().PersistentVolumeClaims().Informer(),
		restoreKey: restoreKey,
	}
}

// Run starts the informers and blocks until their caches are warm or ctx is done.
func (i *Index) Run(ctx context.Context) error {
	for _, inf := range []cache.SharedIndexInformer{i.replicas, i.volumes, i.pods, i.pvcs} {
		go inf.Run(ctx.Done())
	}
	if !cache.WaitForCacheSync(ctx.Done(), i.replicas.HasSynced, i.volumes.HasSynced, i.pods.HasSynced, i.pvcs.HasSynced) {
		return fmt.Errorf("informer caches did not sync: %w", ctx.Err())
	}
	i.mu.Lock()
	i.synced = true
	i.mu.Unlock()
	return nil
}

// Synced reports whether every cache has warmed.
func (i *Index) Synced() bool {
	i.mu.RLock()
	defer i.mu.RUnlock()
	return i.synced
}

// ReplicaNodes returns the nodes running a replica of volume, sorted. Only "running"
// counts: a rebuilding or failed replica has nothing to read yet.
func (i *Index) ReplicaNodes(volume string) []string {
	seen := map[string]struct{}{}
	for _, obj := range i.replicas.GetStore().List() {
		u, ok := obj.(*unstructured.Unstructured)
		if !ok {
			continue
		}
		if nestedString(u, "spec", "volumeName") != volume {
			continue
		}
		if nestedString(u, "status", "currentState") != "running" {
			continue
		}
		if node := nestedString(u, "spec", "nodeID"); node != "" {
			seen[node] = struct{}{}
		}
	}
	return sortedKeys(seen)
}

// ShareManagerNode returns the node running the share-manager for an RWX volume, or "".
// The consumer mounts nfs-ganesha, so this is the hop worth saving, not ReplicaNodes.
func (i *Index) ShareManagerNode(volume string) string {
	for _, obj := range i.pods.GetStore().List() {
		p, ok := obj.(*corev1.Pod)
		if !ok || p.Spec.NodeName == "" {
			continue
		}
		if p.Name == "share-manager-"+volume {
			return p.Spec.NodeName
		}
	}
	return ""
}

// VolumeNameForClaim resolves a PVC to its Longhorn volume. Longhorn's CSI volumeHandle
// equals the PV name, so no PV lookup is needed.
func (i *Index) VolumeNameForClaim(namespace, name string) (string, bool) {
	obj, ok, err := i.pvcs.GetStore().GetByKey(namespace + "/" + name)
	if err != nil || !ok {
		return "", false
	}
	pvc, ok := obj.(*corev1.PersistentVolumeClaim)
	if !ok || pvc.Spec.VolumeName == "" {
		return "", false
	}
	return pvc.Spec.VolumeName, true
}

// Volume looks up a Longhorn volume by name.
func (i *Index) Volume(name string) (Volume, bool) {
	for _, obj := range i.volumes.GetStore().List() {
		u, ok := obj.(*unstructured.Unstructured)
		if !ok || u.GetName() != name {
			continue
		}
		return toVolume(u, i.restoreKey), true
	}
	return Volume{}, false
}

// AttachedVolumes returns every volume currently attached to a node.
func (i *Index) AttachedVolumes() []Volume {
	var out []Volume
	for _, obj := range i.volumes.GetStore().List() {
		u, ok := obj.(*unstructured.Unstructured)
		if !ok {
			continue
		}
		v := toVolume(u, i.restoreKey)
		if v.AttachedNode == "" {
			continue
		}
		out = append(out, v)
	}
	sort.Slice(out, func(a, b int) bool { return out[a].Name < out[b].Name })
	return out
}

func toVolume(u *unstructured.Unstructured, restoreKey string) Volume {
	v := Volume{
		Name:         u.GetName(),
		AttachedNode: nestedString(u, "status", "currentNodeID"),
		DataLocality: nestedString(u, "spec", "dataLocality"),
		AccessMode:   nestedString(u, "spec", "accessMode"),
		ActualSize:   nestedInt(u, "status", "actualSize"),
		WantReplicas: int(nestedInt(u, "spec", "numberOfReplicas")),
		Namespace:    nestedString(u, "status", "kubernetesStatus", "namespace"),
		PVCName:      nestedString(u, "status", "kubernetesStatus", "pvcName"),
		Restore:      u.GetAnnotations()[restoreKey],
	}
	ws, _, _ := unstructured.NestedSlice(u.Object, "status", "kubernetesStatus", "workloadsStatus")
	for _, w := range ws {
		m, ok := w.(map[string]any)
		if !ok {
			continue
		}
		if name, ok := m["podName"].(string); ok && name != "" {
			v.PodNames = append(v.PodNames, name)
		}
	}
	sort.Strings(v.PodNames)
	return v
}

func nestedString(u *unstructured.Unstructured, fields ...string) string {
	s, _, _ := unstructured.NestedString(u.Object, fields...)
	return s
}

// nestedInt tolerates a JSON number or a quoted string: Longhorn writes actualSize as an
// int64 but size as a string.
func nestedInt(u *unstructured.Unstructured, fields ...string) int64 {
	val, found, err := unstructured.NestedFieldNoCopy(u.Object, fields...)
	if !found || err != nil {
		return 0
	}
	switch n := val.(type) {
	case int64:
		return n
	case float64:
		return int64(n)
	case string:
		var out int64
		for _, r := range n {
			if r < '0' || r > '9' {
				return 0
			}
			out = out*10 + int64(r-'0')
		}
		return out
	}
	return 0
}

func sortedKeys(m map[string]struct{}) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
