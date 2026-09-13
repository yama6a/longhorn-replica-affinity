package webhook

import (
	corev1 "k8s.io/api/core/v1"
)

// HostnameLabel is what the injected terms match on. Longhorn's nodeID is this name, so
// replica nodeIDs are usable verbatim.
const HostnameLabel = "kubernetes.io/hostname"

// MaxWeight is the ceiling the API puts on a single preferred term.
const MaxWeight = 100

// Terms builds one term per distinct node, weighted by how many of the pod's volumes it
// holds. Aggregating here rather than one term per volume keeps terms unique, which is
// what lets Merge stay idempotent.
func Terms(nodes []string, weight int32) []corev1.PreferredSchedulingTerm {
	counts := map[string]int32{}
	var order []string
	for _, n := range nodes {
		if n == "" {
			continue
		}
		if _, seen := counts[n]; !seen {
			order = append(order, n)
		}
		counts[n]++
	}

	terms := make([]corev1.PreferredSchedulingTerm, 0, len(order))
	for _, n := range order {
		w := weight * counts[n]
		if w > MaxWeight {
			w = MaxWeight
		}
		terms = append(terms, corev1.PreferredSchedulingTerm{
			Weight: w,
			Preference: corev1.NodeSelectorTerm{
				MatchExpressions: []corev1.NodeSelectorRequirement{{
					Key:      HostnameLabel,
					Operator: corev1.NodeSelectorOpIn,
					Values:   []string{n},
				}},
			},
		})
	}
	return terms
}

// Merge appends terms to a pod's soft node affinity. requiredDuringScheduling is never
// touched: a constraint the author wrote is not ours to weaken.
func Merge(in *corev1.Affinity, terms []corev1.PreferredSchedulingTerm) *corev1.Affinity {
	if len(terms) == 0 {
		return in
	}
	out := in.DeepCopy()
	if out == nil {
		out = &corev1.Affinity{}
	}
	if out.NodeAffinity == nil {
		out.NodeAffinity = &corev1.NodeAffinity{}
	}
	existing := out.NodeAffinity.PreferredDuringSchedulingIgnoredDuringExecution
	for _, t := range terms {
		if containsTerm(existing, t) {
			continue
		}
		existing = append(existing, t)
	}
	out.NodeAffinity.PreferredDuringSchedulingIgnoredDuringExecution = existing
	return out
}

func containsTerm(haystack []corev1.PreferredSchedulingTerm, needle corev1.PreferredSchedulingTerm) bool {
	for _, h := range haystack {
		if equalTerm(h, needle) {
			return true
		}
	}
	return false
}

func equalTerm(a, b corev1.PreferredSchedulingTerm) bool {
	if a.Weight != b.Weight || len(a.Preference.MatchExpressions) != len(b.Preference.MatchExpressions) {
		return false
	}
	if len(a.Preference.MatchFields) != len(b.Preference.MatchFields) {
		return false
	}
	for i := range a.Preference.MatchExpressions {
		if !equalRequirement(a.Preference.MatchExpressions[i], b.Preference.MatchExpressions[i]) {
			return false
		}
	}
	for i := range a.Preference.MatchFields {
		if !equalRequirement(a.Preference.MatchFields[i], b.Preference.MatchFields[i]) {
			return false
		}
	}
	return true
}

func equalRequirement(a, b corev1.NodeSelectorRequirement) bool {
	if a.Key != b.Key || a.Operator != b.Operator || len(a.Values) != len(b.Values) {
		return false
	}
	for i := range a.Values {
		if a.Values[i] != b.Values[i] {
			return false
		}
	}
	return true
}
