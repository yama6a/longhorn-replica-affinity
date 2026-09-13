package index

import (
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
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
