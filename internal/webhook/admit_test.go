package webhook

import (
	"encoding/json"
	"io"
	"log/slog"
	"testing"

	admissionv1 "k8s.io/api/admission/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/yama6a/longhorn-replica-affinity/internal/index"
)

type fakeLookup struct {
	synced   bool
	claims   map[string]string
	volumes  map[string]index.Volume
	replicas map[string][]string
	shares   map[string]string
}

func (f fakeLookup) Synced() bool { return f.synced }

func (f fakeLookup) VolumeNameForClaim(namespace, name string) (string, bool) {
	v, ok := f.claims[namespace+"/"+name]
	return v, ok
}

func (f fakeLookup) Volume(name string) (index.Volume, bool) {
	v, ok := f.volumes[name]
	return v, ok
}

func (f fakeLookup) ReplicaNodes(volume string) []string { return f.replicas[volume] }
func (f fakeLookup) ShareManagerNode(volume string) string {
	return f.shares[volume]
}

func podWithClaims(claims ...string) *corev1.Pod {
	p := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{GenerateName: "app-"}}
	for _, c := range claims {
		p.Spec.Volumes = append(p.Spec.Volumes, corev1.Volume{
			Name: c,
			VolumeSource: corev1.VolumeSource{
				PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: c},
			},
		})
	}
	return p
}

func review(t *testing.T, pod *corev1.Pod, ns string) *admissionv1.AdmissionRequest {
	t.Helper()
	raw, err := json.Marshal(pod)
	if err != nil {
		t.Fatal(err)
	}
	return &admissionv1.AdmissionRequest{UID: "uid-1", Namespace: ns, Object: runtime.RawExtension{Raw: raw}}
}

func newAdmitter(l Lookup, skipRWX bool) *Admitter {
	return &Admitter{
		Index: l, Weight: 30, SkipRWX: skipRWX,
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

func patchedTerms(t *testing.T, resp *admissionv1.AdmissionResponse) []corev1.PreferredSchedulingTerm {
	t.Helper()
	if resp.Patch == nil {
		return nil
	}
	var ops []struct {
		Op    string          `json:"op"`
		Path  string          `json:"path"`
		Value corev1.Affinity `json:"value"`
	}
	if err := json.Unmarshal(resp.Patch, &ops); err != nil {
		t.Fatal(err)
	}
	if len(ops) != 1 || ops[0].Op != "add" || ops[0].Path != "/spec/affinity" {
		t.Fatalf("unexpected patch: %s", resp.Patch)
	}
	return ops[0].Value.NodeAffinity.PreferredDuringSchedulingIgnoredDuringExecution
}

func patchedHostnames(t *testing.T, resp *admissionv1.AdmissionResponse) []string {
	t.Helper()
	return hostnames(t, patchedTerms(t, resp))
}

func rwoLookup() fakeLookup {
	return fakeLookup{
		synced:   true,
		claims:   map[string]string{"media/config": "pvc-1", "media/data": "pvc-2"},
		volumes:  map[string]index.Volume{"pvc-1": {Name: "pvc-1", AccessMode: "rwo"}, "pvc-2": {Name: "pvc-2", AccessMode: "rwo"}},
		replicas: map[string][]string{"pvc-1": {"pi-cp1", "pi-cp3"}, "pvc-2": {"pi-cp1"}},
	}
}

func TestReviewAlwaysAllows(t *testing.T) {
	t.Parallel()
	a := newAdmitter(fakeLookup{synced: false}, false)
	resp, d := a.Review(review(t, podWithClaims("config"), "media"))
	if !resp.Allowed {
		t.Fatal("admission must never deny")
	}
	if d.Skipped != "cache-cold" {
		t.Fatalf("want cache-cold, got %q", d.Skipped)
	}
}

func TestReviewGarbagePodIsAllowed(t *testing.T) {
	t.Parallel()
	a := newAdmitter(rwoLookup(), false)
	req := &admissionv1.AdmissionRequest{UID: "x", Namespace: "media", Object: runtime.RawExtension{Raw: []byte("{{{")}}
	resp, d := a.Review(req)
	if !resp.Allowed || resp.Patch != nil {
		t.Fatal("an undecodable pod must be allowed unpatched")
	}
	if d.Skipped != "decode" {
		t.Fatalf("want decode, got %q", d.Skipped)
	}
}

func TestReviewInjectsReplicaNodes(t *testing.T) {
	t.Parallel()
	a := newAdmitter(rwoLookup(), false)
	resp, d := a.Review(review(t, podWithClaims("config"), "media"))
	if d.Volumes != 1 {
		t.Fatalf("want 1 matched volume, got %d", d.Volumes)
	}
	got := patchedHostnames(t, resp)
	if len(got) != 2 || got[0] != "pi-cp1" || got[1] != "pi-cp3" {
		t.Fatalf("want [pi-cp1 pi-cp3], got %v", got)
	}
}

func TestReviewWeighsNodeHoldingMostVolumes(t *testing.T) {
	t.Parallel()
	// pi-cp1 holds both of the pod's volumes, pi-cp3 only one, so pi-cp1 must outrank it.
	a := newAdmitter(rwoLookup(), false)
	resp, _ := a.Review(review(t, podWithClaims("config", "data"), "media"))
	weights := map[string]int32{}
	for _, term := range patchedTerms(t, resp) {
		weights[term.Preference.MatchExpressions[0].Values[0]] = term.Weight
	}
	if weights["pi-cp1"] != 60 {
		t.Errorf("pi-cp1 weight = %d, want 60", weights["pi-cp1"])
	}
	if weights["pi-cp3"] != 30 {
		t.Errorf("pi-cp3 weight = %d, want 30", weights["pi-cp3"])
	}
}

func TestReviewNoPatchWithoutReplicas(t *testing.T) {
	t.Parallel()
	l := rwoLookup()
	l.replicas = map[string][]string{}
	a := newAdmitter(l, false)
	resp, d := a.Review(review(t, podWithClaims("config"), "media"))
	if resp.Patch != nil {
		t.Fatal("no replicas means no preference to express")
	}
	if d.Skipped != "no-local-replica" {
		t.Fatalf("want no-local-replica, got %q", d.Skipped)
	}
}

func TestReviewIgnoresUnboundClaim(t *testing.T) {
	t.Parallel()
	l := rwoLookup()
	l.claims = map[string]string{}
	a := newAdmitter(l, false)
	resp, _ := a.Review(review(t, podWithClaims("config"), "media"))
	if resp.Patch != nil {
		t.Fatal("an unbound PVC has no replicas to prefer yet")
	}
}

func TestReviewIgnoresNonPVCVolumes(t *testing.T) {
	t.Parallel()
	a := newAdmitter(rwoLookup(), false)
	pod := podWithClaims("config")
	pod.Spec.Volumes = append(pod.Spec.Volumes, corev1.Volume{
		Name:         "tmp",
		VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
	})
	resp, d := a.Review(review(t, pod, "media"))
	if d.Volumes != 1 {
		t.Fatalf("emptyDir must not count, got %d", d.Volumes)
	}
	if len(patchedHostnames(t, resp)) != 2 {
		t.Fatal("replica nodes lost")
	}
}

func TestReviewRWXPrefersShareManager(t *testing.T) {
	t.Parallel()
	l := fakeLookup{
		synced:   true,
		claims:   map[string]string{"media/downloads": "pvc-rwx"},
		volumes:  map[string]index.Volume{"pvc-rwx": {Name: "pvc-rwx", AccessMode: "rwx"}},
		replicas: map[string][]string{"pvc-rwx": {"pi-cp2", "tc-w1"}},
		shares:   map[string]string{"pvc-rwx": "tc-w1"},
	}
	a := newAdmitter(l, false)
	resp, _ := a.Review(review(t, podWithClaims("downloads"), "media"))
	got := patchedHostnames(t, resp)
	if len(got) != 1 || got[0] != "tc-w1" {
		t.Fatalf("RWX must target the share-manager node only, got %v", got)
	}
}

func TestReviewRWXSkippedWhenConfigured(t *testing.T) {
	t.Parallel()
	l := fakeLookup{
		synced:  true,
		claims:  map[string]string{"media/downloads": "pvc-rwx"},
		volumes: map[string]index.Volume{"pvc-rwx": {Name: "pvc-rwx", AccessMode: "rwx"}},
		shares:  map[string]string{"pvc-rwx": "tc-w1"},
	}
	a := newAdmitter(l, true)
	resp, _ := a.Review(review(t, podWithClaims("downloads"), "media"))
	if resp.Patch != nil {
		t.Fatal("SkipRWX must suppress the patch")
	}
}

func TestReviewRWXWithoutShareManager(t *testing.T) {
	t.Parallel()
	l := fakeLookup{
		synced:  true,
		claims:  map[string]string{"media/downloads": "pvc-rwx"},
		volumes: map[string]index.Volume{"pvc-rwx": {Name: "pvc-rwx", AccessMode: "rwx"}},
	}
	a := newAdmitter(l, false)
	resp, _ := a.Review(review(t, podWithClaims("downloads"), "media"))
	if resp.Patch != nil {
		t.Fatal("no share-manager means no target")
	}
}

func TestReviewSkipsPreScheduledPod(t *testing.T) {
	t.Parallel()
	a := newAdmitter(rwoLookup(), false)
	pod := podWithClaims("config")
	pod.Spec.NodeName = "tc-w1"
	resp, d := a.Review(review(t, pod, "media"))
	if resp.Patch != nil {
		t.Fatal("a pod that bypasses the scheduler must not be patched")
	}
	if d.Skipped != "pre-scheduled" {
		t.Fatalf("want pre-scheduled, got %q", d.Skipped)
	}
}

func TestReviewEchoesUID(t *testing.T) {
	t.Parallel()
	a := newAdmitter(rwoLookup(), false)
	resp, _ := a.Review(review(t, podWithClaims("config"), "media"))
	if resp.UID != "uid-1" {
		t.Fatalf("UID must be echoed, got %q", resp.UID)
	}
}
