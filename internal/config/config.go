// Package config resolves runtime settings from the environment.
package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config holds every tunable for both subcommands.
type Config struct {
	LabelKey   string
	LabelValue string

	LonghornNamespace string
	Weight            int32

	SkipRWX bool

	ListenAddr  string
	MetricsAddr string
	CertFile    string
	KeyFile     string

	ReconcileInterval time.Duration
	Dwell             time.Duration
	MaxMoveBytes      int64
	FlipDataLocality  bool
}

// RestoreAnnotation is the key under which the reconciler parks a volume's original
// dataLocality while it borrows the field. On the object, not in memory, so a restart
// mid-flip still restores the right value.
func (c Config) RestoreAnnotation() string {
	prefix, _, ok := strings.Cut(c.LabelKey, "/")
	if !ok {
		return "restore-data-locality"
	}
	return prefix + "/restore-data-locality"
}

// Load reads the LRA_* environment, applying defaults for anything unset.
func Load() (Config, error) {
	c := Config{
		LabelKey:          env("LRA_LABEL_KEY", "longhorn-replica-affinity/enabled"),
		LabelValue:        env("LRA_LABEL_VALUE", "true"),
		LonghornNamespace: env("LRA_LONGHORN_NAMESPACE", "longhorn-system"),
		ListenAddr:        env("LRA_LISTEN", ":8443"),
		MetricsAddr:       env("LRA_METRICS_LISTEN", ":9100"),
		CertFile:          env("LRA_TLS_CERT_FILE", "/tls/tls.crt"),
		KeyFile:           env("LRA_TLS_KEY_FILE", "/tls/tls.key"),
	}

	var err error
	if c.Weight, err = envInt32("LRA_WEIGHT", 30); err != nil {
		return c, err
	}
	if c.Weight < 1 || c.Weight > 100 {
		return c, fmt.Errorf("LRA_WEIGHT must be 1-100, got %d", c.Weight)
	}
	if c.SkipRWX, err = envBool("LRA_SKIP_RWX", false); err != nil {
		return c, err
	}
	if c.ReconcileInterval, err = envDuration("LRA_RECONCILE_INTERVAL", time.Minute); err != nil {
		return c, err
	}
	if c.Dwell, err = envDuration("LRA_DWELL", 30*time.Minute); err != nil {
		return c, err
	}
	if c.MaxMoveBytes, err = envInt64("LRA_MAX_MOVE_BYTES", 5<<30); err != nil {
		return c, err
	}
	if c.FlipDataLocality, err = envBool("LRA_FLIP_DATA_LOCALITY", true); err != nil {
		return c, err
	}
	return c, nil
}

func env(k, def string) string {
	if v, ok := os.LookupEnv(k); ok && v != "" {
		return v
	}
	return def
}

func envInt32(k string, def int32) (int32, error) {
	v, ok := os.LookupEnv(k)
	if !ok || v == "" {
		return def, nil
	}
	n, err := strconv.ParseInt(v, 10, 32)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", k, err)
	}
	return int32(n), nil
}

func envInt64(k string, def int64) (int64, error) {
	v, ok := os.LookupEnv(k)
	if !ok || v == "" {
		return def, nil
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", k, err)
	}
	return n, nil
}

func envBool(k string, def bool) (bool, error) {
	v, ok := os.LookupEnv(k)
	if !ok || v == "" {
		return def, nil
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return false, fmt.Errorf("%s: %w", k, err)
	}
	return b, nil
}

func envDuration(k string, def time.Duration) (time.Duration, error) {
	v, ok := os.LookupEnv(k)
	if !ok || v == "" {
		return def, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", k, err)
	}
	return d, nil
}
