package config

import "testing"

func TestLoadDefaults(t *testing.T) {
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if c.Weight != 30 {
		t.Errorf("Weight = %d, want 30", c.Weight)
	}
	if c.LabelKey != "longhorn-replica-affinity/enabled" {
		t.Errorf("LabelKey = %q", c.LabelKey)
	}
	if !c.FlipDataLocality {
		t.Error("FlipDataLocality should default on")
	}
	if c.MaxMoveBytes != 5<<30 {
		t.Errorf("MaxMoveBytes = %d, want 5Gi", c.MaxMoveBytes)
	}
}

func TestWeightMustBeInRange(t *testing.T) {
	for _, v := range []string{"0", "101", "-1"} {
		t.Setenv("LRA_WEIGHT", v)
		if _, err := Load(); err == nil {
			t.Errorf("LRA_WEIGHT=%s should be rejected", v)
		}
	}
}

func TestWeightMustBeNumeric(t *testing.T) {
	t.Setenv("LRA_WEIGHT", "heavy")
	if _, err := Load(); err == nil {
		t.Error("non-numeric weight should be rejected")
	}
}

func TestBadDurationRejected(t *testing.T) {
	t.Setenv("LRA_DWELL", "half an hour")
	if _, err := Load(); err == nil {
		t.Error("unparseable duration should be rejected")
	}
}

func TestOverrides(t *testing.T) {
	t.Setenv("LRA_WEIGHT", "75")
	t.Setenv("LRA_DWELL", "5m")
	t.Setenv("LRA_SKIP_RWX", "true")
	t.Setenv("LRA_MAX_MOVE_BYTES", "1024")
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if c.Weight != 75 || c.Dwell.Minutes() != 5 || !c.SkipRWX || c.MaxMoveBytes != 1024 {
		t.Fatalf("overrides not applied: %+v", c)
	}
}

func TestRestoreAnnotationFollowsLabelPrefix(t *testing.T) {
	t.Setenv("LRA_LABEL_KEY", "example.com/on")
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if got := c.RestoreAnnotation(); got != "example.com/restore-data-locality" {
		t.Errorf("RestoreAnnotation = %q", got)
	}
}

func TestRestoreAnnotationWithBareLabelKey(t *testing.T) {
	t.Setenv("LRA_LABEL_KEY", "bare")
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if got := c.RestoreAnnotation(); got != "restore-data-locality" {
		t.Errorf("RestoreAnnotation = %q", got)
	}
}

func TestTLSModeDefaultsToSelfSigned(t *testing.T) {
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if c.TLSMode != TLSModeSelfSigned {
		t.Errorf("TLSMode = %q, want %q", c.TLSMode, TLSModeSelfSigned)
	}
}

func TestUnknownTLSModeRejected(t *testing.T) {
	t.Setenv("LRA_TLS_MODE", "acme")
	if _, err := Load(); err == nil {
		t.Error("an unknown TLS mode should be rejected at startup")
	}
}

func TestProvidedTLSModeNeedsNoNamespace(t *testing.T) {
	t.Setenv("LRA_TLS_MODE", "provided")
	if _, err := Load(); err != nil {
		t.Errorf("provided mode reads files and needs no namespace: %v", err)
	}
}
