package webhook

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
)

func hostnames(t *testing.T, terms []corev1.PreferredSchedulingTerm) []string {
	t.Helper()
	var out []string
	for _, term := range terms {
		for _, e := range term.Preference.MatchExpressions {
			if e.Key == HostnameLabel {
				out = append(out, e.Values...)
			}
		}
	}
	return out
}

func TestTermsOnePerNode(t *testing.T) {
	t.Parallel()
	terms := Terms([]string{"a", "b"}, 30)
	if len(terms) != 2 {
		t.Fatalf("want 2 terms, got %d", len(terms))
	}
	for _, term := range terms {
		if term.Weight != 30 {
			t.Errorf("weight = %d, want 30", term.Weight)
		}
	}
}

func TestTermsWeightsByVolumeCount(t *testing.T) {
	t.Parallel()
	// Two volumes on node a, one on b: a must outrank b, and each node appears once.
	terms := Terms([]string{"a", "a", "b"}, 30)
	if len(terms) != 2 {
		t.Fatalf("want 2 terms, got %d", len(terms))
	}
	if terms[0].Weight != 60 {
		t.Errorf("node a weight = %d, want 60", terms[0].Weight)
	}
	if terms[1].Weight != 30 {
		t.Errorf("node b weight = %d, want 30", terms[1].Weight)
	}
}

func TestTermsWeightCappedAtMax(t *testing.T) {
	t.Parallel()
	terms := Terms([]string{"a", "a", "a", "a", "a"}, 30)
	if terms[0].Weight != MaxWeight {
		t.Fatalf("weight = %d, want it capped at %d", terms[0].Weight, MaxWeight)
	}
}

func TestTermsSkipsEmpty(t *testing.T) {
	t.Parallel()
	if terms := Terms([]string{"", "a", ""}, 10); len(terms) != 1 {
		t.Fatalf("want 1 term, got %d", len(terms))
	}
}

func TestTermsEmptyInput(t *testing.T) {
	t.Parallel()
	if terms := Terms(nil, 30); len(terms) != 0 {
		t.Fatalf("want no terms, got %d", len(terms))
	}
}

func TestMergeIntoNilAffinity(t *testing.T) {
	t.Parallel()
	got := Merge(nil, Terms([]string{"a"}, 30))
	if got == nil || got.NodeAffinity == nil {
		t.Fatal("want a nodeAffinity built from nothing")
	}
	if n := len(got.NodeAffinity.PreferredDuringSchedulingIgnoredDuringExecution); n != 1 {
		t.Fatalf("want 1 preferred term, got %d", n)
	}
}

func TestMergeNoTermsIsIdentity(t *testing.T) {
	t.Parallel()
	if got := Merge(nil, nil); got != nil {
		t.Fatalf("want nil affinity untouched, got %+v", got)
	}
}

func TestMergePreservesRequiredAndExistingPreferred(t *testing.T) {
	t.Parallel()
	required := &corev1.NodeSelector{NodeSelectorTerms: []corev1.NodeSelectorTerm{{
		MatchExpressions: []corev1.NodeSelectorRequirement{{
			Key: "gpu", Operator: corev1.NodeSelectorOpExists,
		}},
	}}}
	in := &corev1.Affinity{NodeAffinity: &corev1.NodeAffinity{
		RequiredDuringSchedulingIgnoredDuringExecution:  required,
		PreferredDuringSchedulingIgnoredDuringExecution: Terms([]string{"existing"}, 100),
	}}

	got := Merge(in, Terms([]string{"a"}, 30))

	if got.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution == nil {
		t.Fatal("required affinity was dropped")
	}
	names := hostnames(t, got.NodeAffinity.PreferredDuringSchedulingIgnoredDuringExecution)
	if len(names) != 2 || names[0] != "existing" || names[1] != "a" {
		t.Fatalf("want [existing a], got %v", names)
	}
}

func TestMergeDoesNotMutateInput(t *testing.T) {
	t.Parallel()
	in := &corev1.Affinity{NodeAffinity: &corev1.NodeAffinity{
		PreferredDuringSchedulingIgnoredDuringExecution: Terms([]string{"existing"}, 100),
	}}
	_ = Merge(in, Terms([]string{"a"}, 30))
	if n := len(in.NodeAffinity.PreferredDuringSchedulingIgnoredDuringExecution); n != 1 {
		t.Fatalf("input mutated: now has %d terms", n)
	}
}

func TestMergeIsIdempotent(t *testing.T) {
	t.Parallel()
	terms := Terms([]string{"a", "b"}, 30)
	once := Merge(nil, terms)
	twice := Merge(once, terms)
	if n := len(twice.NodeAffinity.PreferredDuringSchedulingIgnoredDuringExecution); n != 2 {
		t.Fatalf("want 2 terms after re-merging the same set, got %d", n)
	}
}

func TestMergePodAffinityUntouched(t *testing.T) {
	t.Parallel()
	in := &corev1.Affinity{PodAntiAffinity: &corev1.PodAntiAffinity{
		RequiredDuringSchedulingIgnoredDuringExecution: []corev1.PodAffinityTerm{{TopologyKey: "kubernetes.io/hostname"}},
	}}
	got := Merge(in, Terms([]string{"a"}, 30))
	if got.PodAntiAffinity == nil || len(got.PodAntiAffinity.RequiredDuringSchedulingIgnoredDuringExecution) != 1 {
		t.Fatal("podAntiAffinity was dropped")
	}
}
