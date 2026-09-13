// Package webhook serves the pod mutating admission endpoint.
package webhook

import (
	"encoding/json"
	"log/slog"

	admissionv1 "k8s.io/api/admission/v1"
	corev1 "k8s.io/api/core/v1"

	"github.com/yama6a/longhorn-replica-affinity/internal/index"
)

// Decision records what a single admission produced, for logging and metrics.
type Decision struct {
	Namespace    string
	Pod          string
	Nodes        []string
	Volumes      int
	ShareManager bool
	Skipped      string
}

// Lookup is the slice of the index the admission path needs, as an interface so the
// decision logic is testable without informers.
type Lookup interface {
	Synced() bool
	VolumeNameForClaim(namespace, name string) (string, bool)
	Volume(name string) (index.Volume, bool)
	ReplicaNodes(volume string) []string
	ShareManagerNode(volume string) string
}

// Admitter turns a pod into the set of nodes that already hold its data.
type Admitter struct {
	Index   Lookup
	Weight  int32
	SkipRWX bool
	Log     *slog.Logger
}

// Review handles one AdmissionReview and returns the response to send back.
func (a *Admitter) Review(req *admissionv1.AdmissionRequest) (*admissionv1.AdmissionResponse, Decision) {
	resp := &admissionv1.AdmissionResponse{UID: req.UID, Allowed: true}

	var pod corev1.Pod
	if err := json.Unmarshal(req.Object.Raw, &pod); err != nil {
		// Allowed anyway: this optimises placement, it never blocks it.
		a.Log.Error("decode pod", "err", err)
		return resp, Decision{Namespace: req.Namespace, Skipped: "decode"}
	}

	name := pod.Name
	if name == "" {
		name = pod.GenerateName + "*"
	}
	d := Decision{Namespace: req.Namespace, Pod: name}

	if !a.Index.Synced() {
		d.Skipped = "cache-cold"
		return resp, d
	}

	// Never reaches the scheduler, so affinity on it is dead weight for the pod's life.
	if pod.Spec.NodeName != "" {
		d.Skipped = "pre-scheduled"
		return resp, d
	}

	// A share-manager is itself a pod, so the RWX hop from it to its replicas is fixed by
	// moving it, not by dragging the volume to it.
	if index.IsShareManager(podName(&pod, req)) {
		d.ShareManager = true
		nodes := a.Index.ReplicaNodes(index.VolumeForShareManager(podName(&pod, req)))
		d.Nodes, d.Volumes = nodes, len(nodes)
		if len(nodes) == 0 {
			d.Skipped = "no-local-replica"
			return resp, d
		}
		return a.patch(resp, &pod, nodes, &d), d
	}

	nodes, matched := a.targets(&pod, req.Namespace)
	d.Nodes, d.Volumes = nodes, matched
	if len(nodes) == 0 {
		if d.Skipped == "" {
			d.Skipped = "no-local-replica"
		}
		return resp, d
	}

	return a.patch(resp, &pod, nodes, &d), d
}

func (a *Admitter) patch(resp *admissionv1.AdmissionResponse, pod *corev1.Pod, nodes []string, d *Decision) *admissionv1.AdmissionResponse {
	merged := Merge(pod.Spec.Affinity, Terms(nodes, a.Weight))
	patch, err := json.Marshal([]map[string]any{{
		"op":    "add",
		"path":  "/spec/affinity",
		"value": merged,
	}})
	if err != nil {
		a.Log.Error("marshal patch", "err", err)
		d.Skipped = "patch"
		return resp
	}
	pt := admissionv1.PatchTypeJSONPatch
	resp.Patch = patch
	resp.PatchType = &pt
	return resp
}

// podName prefers the object's own name, falling back to the request's for a pod the
// controller named but has not stamped into the object yet.
func podName(pod *corev1.Pod, req *admissionv1.AdmissionRequest) string {
	if pod.Name != "" {
		return pod.Name
	}
	if req.Name != "" {
		return req.Name
	}
	return pod.GenerateName
}

// targets returns one entry per volume per node. Repeats are deliberate: Terms weights a
// node by how often it appears, so the node holding most of the pod's data wins.
func (a *Admitter) targets(pod *corev1.Pod, namespace string) (nodes []string, matched int) {
	for _, v := range pod.Spec.Volumes {
		if v.PersistentVolumeClaim == nil {
			continue
		}
		volName, ok := a.Index.VolumeNameForClaim(namespace, v.PersistentVolumeClaim.ClaimName)
		if !ok {
			// A first-ever pod can be admitted before its PVC binds.
			continue
		}
		vol, ok := a.Index.Volume(volName)
		if !ok {
			continue
		}

		if vol.RWX() {
			if a.SkipRWX {
				continue
			}
			// The consumer mounts nfs-ganesha, not a replica.
			if n := a.Index.ShareManagerNode(volName); n != "" {
				nodes = append(nodes, n)
				matched++
			}
			continue
		}

		if rn := a.Index.ReplicaNodes(volName); len(rn) > 0 {
			nodes = append(nodes, rn...)
			matched++
		}
	}
	return nodes, matched
}
