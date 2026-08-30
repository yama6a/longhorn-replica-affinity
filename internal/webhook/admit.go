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
	Namespace string
	Pod       string
	Nodes     []string
	Volumes   int
	Skipped   string
}

// Lookup is the slice of the replica index the admission path needs. An interface so
// the decision logic can be tested without informers.
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
		// Allowed anyway: this webhook exists to optimise placement, never to block it.
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

	// A pod created with a node already chosen never reaches the scheduler, so affinity
	// on it is dead weight the API server will carry for the pod's whole life.
	if pod.Spec.NodeName != "" {
		d.Skipped = "pre-scheduled"
		return resp, d
	}

	nodes, matched := a.targets(&pod, req.Namespace)
	d.Nodes, d.Volumes = nodes, matched
	if len(nodes) == 0 {
		if d.Skipped == "" {
			d.Skipped = "no-local-replica"
		}
		return resp, d
	}

	merged := Merge(pod.Spec.Affinity, Terms(nodes, a.Weight))
	patch, err := json.Marshal([]map[string]any{{
		"op":    "add",
		"path":  "/spec/affinity",
		"value": merged,
	}})
	if err != nil {
		a.Log.Error("marshal patch", "err", err)
		d.Skipped = "patch"
		return resp, d
	}

	pt := admissionv1.PatchTypeJSONPatch
	resp.Patch = patch
	resp.PatchType = &pt
	return resp, d
}

// targets collects every node holding data for the pod, one entry per volume. A node
// appearing for two volumes is returned twice on purpose: Terms turns each into its own
// weighted term and the scheduler sums them, so the node with the most of the pod's data
// wins.
func (a *Admitter) targets(pod *corev1.Pod, namespace string) (nodes []string, matched int) {
	for _, v := range pod.Spec.Volumes {
		if v.PersistentVolumeClaim == nil {
			continue
		}
		volName, ok := a.Index.VolumeNameForClaim(namespace, v.PersistentVolumeClaim.ClaimName)
		if !ok {
			// Immediate binding usually wins the race, but a first-ever pod can be
			// admitted before its PVC binds. Nothing to prefer yet.
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
			// The consumer mounts nfs-ganesha, not a replica, so the share-manager's
			// node is the only placement that saves it a hop.
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
