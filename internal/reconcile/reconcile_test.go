package reconcile

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	corev1 "k8s.io/api/core/v1"

	"github.com/yama6a/longhorn-replica-affinity/internal/config"
	"github.com/yama6a/longhorn-replica-affinity/internal/index"
)

const ns = "longhorn-system"

type fakeStore struct {
	volumes  []index.Volume
	replicas map[string][]string
}

func (f fakeStore) AttachedVolumes() []index.Volume     { return f.volumes }
func (f fakeStore) ReplicaNodes(volume string) []string { return f.replicas[volume] }

type patchRecord struct {
	Metadata struct {
		Annotations map[string]*string `json:"annotations"`
	} `json:"metadata"`
	Spec struct {
		DataLocality string `json:"dataLocality"`
	} `json:"spec"`
}

func labelledPod(name string) *corev1.Pod {
	return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name: name, Namespace: "media",
		Labels: map[string]string{"longhorn-replica-affinity/enabled": "true"},
	}}
}

func plainPod(name string) *corev1.Pod {
	return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "media"}}
}

// dynClient builds a fake dynamic client that records every patch against volumes.
func dynClient(t *testing.T, out *[]patchRecord) *dynamicfake.FakeDynamicClient {
	t.Helper()
	c := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(
		runtime.NewScheme(),
		map[schema.GroupVersionResource]string{index.VolumeGVR: "VolumeList"},
	)
	c.PrependReactor("patch", "volumes", func(a k8stesting.Action) (bool, runtime.Object, error) {
		pa, ok := a.(k8stesting.PatchAction)
		if !ok {
			return false, nil, nil
		}
		var rec patchRecord
		if err := json.Unmarshal(pa.GetPatch(), &rec); err != nil {
			t.Errorf("patch is not valid json: %v", err)
		}
		*out = append(*out, rec)
		return true, &unstructured.Unstructured{}, nil
	})
	return c
}

func run(t *testing.T, store Store, pods []*corev1.Pod, tweak func(*Reconciler)) []patchRecord {
	t.Helper()
	var patches []patchRecord
	objs := make([]runtime.Object, 0, len(pods))
	for _, p := range pods {
		objs = append(objs, p)
	}
	r := &Reconciler{
		Cfg: config.Config{
			LabelKey: "longhorn-replica-affinity/enabled", LabelValue: "true",
			LonghornNamespace: ns, Dwell: time.Hour, MaxMoveBytes: 5 << 30, FlipDataLocality: true,
		},
		Index: store,
		Kube:  fake.NewClientset(objs...),
		Dyn:   dynClient(t, &patches),
		Log:   slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	if tweak != nil {
		tweak(r)
	}
	r.pass(context.Background())
	return patches
}

func misplaced() index.Volume {
	return index.Volume{
		Name: "pvc-1", AttachedNode: "tc-w1", DataLocality: "disabled", AccessMode: "rwo",
		ActualSize: 700 << 20, Namespace: "media", PVCName: "plex-config", PodNames: []string{"plex-1"},
	}
}

func store(v index.Volume, replicaNodes ...string) fakeStore {
	return fakeStore{volumes: []index.Volume{v}, replicas: map[string][]string{v.Name: replicaNodes}}
}

func TestDwellMustElapseBeforeBorrowing(t *testing.T) {
	t.Parallel()
	patches := run(t, store(misplaced(), "pi-cp1", "pi-cp3"), []*corev1.Pod{labelledPod("plex-1")}, nil)
	if len(patches) != 0 {
		t.Fatalf("first sighting must only start the clock, got %v", patches)
	}
}

func TestBorrowsAfterDwell(t *testing.T) {
	t.Parallel()
	patches := run(t, store(misplaced(), "pi-cp1", "pi-cp3"), []*corev1.Pod{labelledPod("plex-1")},
		func(r *Reconciler) {
			r.since = map[string]time.Time{"pvc-1": time.Now().Add(-2 * time.Hour)}
		})
	if len(patches) != 1 {
		t.Fatalf("want 1 patch, got %d", len(patches))
	}
	if patches[0].Spec.DataLocality != bestEffort {
		t.Errorf("dataLocality = %q, want %q", patches[0].Spec.DataLocality, bestEffort)
	}
	got := patches[0].Metadata.Annotations["longhorn-replica-affinity/restore-data-locality"]
	if got == nil || *got != "disabled" {
		t.Errorf("original dataLocality must be parked in the annotation, got %v", got)
	}
}

func TestNeverBorrowsForUnlabelledPod(t *testing.T) {
	t.Parallel()
	patches := run(t, store(misplaced(), "pi-cp1"), []*corev1.Pod{plainPod("plex-1")},
		func(r *Reconciler) { r.since = map[string]time.Time{"pvc-1": time.Now().Add(-2 * time.Hour)} })
	if len(patches) != 0 {
		t.Fatalf("opt-in is mandatory, got %v", patches)
	}
}

func TestNeverBorrowsOversizedVolume(t *testing.T) {
	t.Parallel()
	v := misplaced()
	v.ActualSize = 10 << 30
	patches := run(t, store(v, "pi-cp1"), []*corev1.Pod{labelledPod("plex-1")},
		func(r *Reconciler) { r.since = map[string]time.Time{"pvc-1": time.Now().Add(-2 * time.Hour)} })
	if len(patches) != 0 {
		t.Fatalf("a volume over the ceiling must never be copied, got %v", patches)
	}
}

func TestNeverBorrowsWhenLonghornAlreadyManagesLocality(t *testing.T) {
	t.Parallel()
	v := misplaced()
	v.DataLocality = bestEffort
	patches := run(t, store(v, "pi-cp1"), []*corev1.Pod{labelledPod("plex-1")},
		func(r *Reconciler) { r.since = map[string]time.Time{"pvc-1": time.Now().Add(-2 * time.Hour)} })
	if len(patches) != 0 {
		t.Fatalf("best-effort is already Longhorn's job, got %v", patches)
	}
}

func TestNeverBorrowsWhenDisabled(t *testing.T) {
	t.Parallel()
	patches := run(t, store(misplaced(), "pi-cp1"), []*corev1.Pod{labelledPod("plex-1")},
		func(r *Reconciler) {
			r.Cfg.FlipDataLocality = false
			r.since = map[string]time.Time{"pvc-1": time.Now().Add(-2 * time.Hour)}
		})
	if len(patches) != 0 {
		t.Fatalf("want no patches when flipping is off, got %v", patches)
	}
}

func TestNoBorrowWhenAlreadyLocal(t *testing.T) {
	t.Parallel()
	patches := run(t, store(misplaced(), "tc-w1", "pi-cp1"), []*corev1.Pod{labelledPod("plex-1")},
		func(r *Reconciler) { r.since = map[string]time.Time{"pvc-1": time.Now().Add(-2 * time.Hour)} })
	if len(patches) != 0 {
		t.Fatalf("volume is already local, got %v", patches)
	}
}

func TestRestoresOnceReplicaIsLocal(t *testing.T) {
	t.Parallel()
	v := misplaced()
	v.DataLocality = bestEffort
	v.Restore = "disabled"
	patches := run(t, store(v, "tc-w1"), []*corev1.Pod{labelledPod("plex-1")}, nil)
	if len(patches) != 1 {
		t.Fatalf("want 1 restore patch, got %d", len(patches))
	}
	if patches[0].Spec.DataLocality != "disabled" {
		t.Errorf("want the parked value restored, got %q", patches[0].Spec.DataLocality)
	}
	if got, ok := patches[0].Metadata.Annotations["longhorn-replica-affinity/restore-data-locality"]; !ok || got != nil {
		t.Errorf("restore annotation must be cleared, got %v", got)
	}
}

func TestRestoreKeepsNonDefaultOriginal(t *testing.T) {
	t.Parallel()
	// A volume whose StorageClass asked for best-effort must go back to best-effort,
	// not to disabled.
	v := misplaced()
	v.DataLocality = bestEffort
	v.Restore = bestEffort
	patches := run(t, store(v, "tc-w1"), []*corev1.Pod{labelledPod("plex-1")}, nil)
	if len(patches) != 1 || patches[0].Spec.DataLocality != bestEffort {
		t.Fatalf("want best-effort restored, got %v", patches)
	}
}

func TestDwellResetsWhenVolumeBecomesLocal(t *testing.T) {
	t.Parallel()
	r := &Reconciler{
		Cfg: config.Config{
			LabelKey: "longhorn-replica-affinity/enabled", LabelValue: "true",
			LonghornNamespace: ns, Dwell: time.Hour, MaxMoveBytes: 5 << 30, FlipDataLocality: true,
		},
		Index: store(misplaced(), "tc-w1"),
		Kube:  fake.NewClientset(labelledPod("plex-1")),
		Log:   slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	var patches []patchRecord
	r.Dyn = dynClient(t, &patches)
	r.since = map[string]time.Time{"pvc-1": time.Now().Add(-2 * time.Hour)}
	r.pass(context.Background())
	if _, still := r.since["pvc-1"]; still {
		t.Fatal("dwell timer must clear once the volume is local")
	}
}

func TestForgetsVanishedVolumes(t *testing.T) {
	t.Parallel()
	r := &Reconciler{
		Cfg:   config.Config{LonghornNamespace: ns, Dwell: time.Hour, MaxMoveBytes: 5 << 30},
		Index: fakeStore{},
		Kube:  fake.NewClientset(),
		Log:   slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	var patches []patchRecord
	r.Dyn = dynClient(t, &patches)
	r.since = map[string]time.Time{"gone": time.Now()}
	r.pass(context.Background())
	if len(r.since) != 0 {
		t.Fatalf("dwell state leaked for a deleted volume: %v", r.since)
	}
}
