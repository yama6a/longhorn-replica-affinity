package index

import (
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic/dynamicinformer"
	dynamicfake "k8s.io/client-go/dynamic/fake"
)

func vol(obj map[string]any) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: obj}
}

func TestNestedIntAcceptsNumberAndString(t *testing.T) {
	t.Parallel()
	cases := map[string]struct {
		in   any
		want int64
	}{
		"int64":   {int64(42), 42},
		"float64": {float64(42), 42},
		"string":  {"42", 42},
		"empty":   {"", 0},
		"garbage": {"4x2", 0},
		"absent":  {nil, 0},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			u := vol(map[string]any{"status": map[string]any{}})
			if tc.in != nil {
				u.Object["status"].(map[string]any)["actualSize"] = tc.in
			}
			if got := nestedInt(u, "status", "actualSize"); got != tc.want {
				t.Errorf("got %d, want %d", got, tc.want)
			}
		})
	}
}

func TestToVolumeReadsKubernetesStatus(t *testing.T) {
	t.Parallel()
	u := vol(map[string]any{
		"metadata": map[string]any{
			"name":        "pvc-1",
			"annotations": map[string]any{"x/restore-data-locality": "disabled"},
		},
		"spec": map[string]any{"dataLocality": "best-effort", "accessMode": "rwx"},
		"status": map[string]any{
			"currentNodeID": "tc-w1",
			"actualSize":    int64(1024),
			"kubernetesStatus": map[string]any{
				"namespace": "media",
				"pvcName":   "media-downloads",
				"workloadsStatus": []any{
					map[string]any{"podName": "sonarr-1"},
					map[string]any{"podName": "qbittorrent-1"},
					map[string]any{"workloadName": "no-pod-name"},
				},
			},
		},
	})

	got := toVolume(u, "x/restore-data-locality")

	if got.Name != "pvc-1" || got.AttachedNode != "tc-w1" || got.PVCName != "media-downloads" {
		t.Fatalf("unexpected volume: %+v", got)
	}
	if !got.RWX() {
		t.Error("accessMode rwx should report RWX")
	}
	if got.Restore != "disabled" {
		t.Errorf("Restore = %q", got.Restore)
	}
	if len(got.PodNames) != 2 || got.PodNames[0] != "qbittorrent-1" {
		t.Errorf("PodNames = %v, want them sorted and pod-name-only", got.PodNames)
	}
}

func TestToVolumeTolerablyEmpty(t *testing.T) {
	t.Parallel()
	got := toVolume(vol(map[string]any{"metadata": map[string]any{"name": "v"}}), "k")
	if got.Name != "v" || got.AttachedNode != "" || got.RWX() {
		t.Fatalf("unexpected: %+v", got)
	}
}

func replicaStore(t *testing.T, objs ...*unstructured.Unstructured) *Index {
	t.Helper()
	inf := dynamicinformer.NewFilteredDynamicSharedInformerFactory(
		dynamicfake.NewSimpleDynamicClientWithCustomListKinds(
			runtime.NewScheme(),
			map[schema.GroupVersionResource]string{ReplicaGVR: "ReplicaList"},
		), 0, "longhorn-system", nil,
	).ForResource(ReplicaGVR).Informer()
	for _, o := range objs {
		if err := inf.GetStore().Add(o); err != nil {
			t.Fatal(err)
		}
	}
	return &Index{replicas: inf}
}

func replica(name, node, state string, spec map[string]any) *unstructured.Unstructured {
	s := map[string]any{"volumeName": "pvc-rwx", "nodeID": node, "active": true}
	for k, v := range spec {
		s[k] = v
	}
	return vol(map[string]any{
		"metadata": map[string]any{"name": name},
		"spec":     s,
		"status":   map[string]any{"currentState": state},
	})
}

func TestReplicaNodesOnDiskIgnoresStoppedProcesses(t *testing.T) {
	t.Parallel()
	// Longhorn tears the engine and every replica down before it recreates a
	// share-manager, so at that moment nothing is running but the data has not moved.
	i := replicaStore(t,
		replica("r-92e5efaf", "pi-cp2", "stopped", nil),
		replica("r-c0226189", "pi-cp1", "stopped", nil),
	)
	if got := i.ReplicaNodesOnDisk("pvc-rwx"); len(got) != 2 || got[0] != "pi-cp1" || got[1] != "pi-cp2" {
		t.Fatalf("want both nodes, got %v", got)
	}
	if got := i.ReplicaNodes("pvc-rwx"); len(got) != 0 {
		t.Fatalf("the running view must still be empty, got %v", got)
	}
}

func TestReplicaNodesOnDiskExcludesUnusable(t *testing.T) {
	t.Parallel()
	cases := map[string]*unstructured.Unstructured{
		"failed":   replica("r-failed", "tc-w1", "stopped", map[string]any{"failedAt": "2026-09-14T01:30:16Z"}),
		"inactive": replica("r-old", "tc-w1", "running", map[string]any{"active": false}),
		"no node":  replica("r-unscheduled", "", "stopped", nil),
	}
	for name, r := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if got := replicaStore(t, r).ReplicaNodesOnDisk("pvc-rwx"); len(got) != 0 {
				t.Fatalf("want no nodes, got %v", got)
			}
		})
	}
}

func TestReplicaNodesOnDiskExcludesDeleting(t *testing.T) {
	t.Parallel()
	r := replica("r-going", "tc-w1", "running", nil)
	r.SetDeletionTimestamp(&metav1.Time{Time: time.Now()})
	if got := replicaStore(t, r).ReplicaNodesOnDisk("pvc-rwx"); len(got) != 0 {
		t.Fatalf("a replica on its way out holds nothing, got %v", got)
	}
}

func TestReplicaNodesOnDiskScopesToTheVolume(t *testing.T) {
	t.Parallel()
	other := replica("r-other", "tc-w1", "stopped", map[string]any{"volumeName": "pvc-elsewhere"})
	i := replicaStore(t, replica("r-mine", "pi-cp1", "stopped", nil), other)
	if got := i.ReplicaNodesOnDisk("pvc-rwx"); len(got) != 1 || got[0] != "pi-cp1" {
		t.Fatalf("want only this volume's node, got %v", got)
	}
}
