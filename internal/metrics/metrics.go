// Package metrics exposes the Prometheus series both subcommands publish.
package metrics

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

var (
	admissions = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "lra_admissions_total",
		Help: "Pod admissions seen, by outcome.",
	}, []string{"outcome"})

	flips = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "lra_data_locality_flips_total",
		Help: "dataLocality transitions the reconciler made, by direction.",
	}, []string{"direction"})

	local = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "lra_volume_local",
		Help: "1 when an attached volume has a running replica on the node it is attached to, else 0.",
	}, []string{"namespace", "pvc", "node"})

	unfixable = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "lra_volume_unfixable",
		Help: "1 when a volume is non-local and the reconciler will not move it (too large, or opted out).",
	}, []string{"namespace", "pvc", "reason"})

	buildInfo = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "lra_build_info",
		Help: "Build metadata, always 1.",
	}, []string{"version"})
)

func init() {
	prometheus.MustRegister(admissions, flips, local, unfixable, buildInfo)
}

// SetVersion stamps the build-info series.
func SetVersion(v string) { buildInfo.WithLabelValues(v).Set(1) }

// Admission records one admission outcome.
func Admission(skipped string, injected bool) {
	switch {
	case skipped != "":
		admissions.WithLabelValues(skipped).Inc()
	case injected:
		admissions.WithLabelValues("injected").Inc()
	default:
		admissions.WithLabelValues("noop").Inc()
	}
}

// Flip records a dataLocality transition. direction is "borrow" or "restore".
func Flip(direction string) { flips.WithLabelValues(direction).Inc() }

// ResetVolumes clears the per-volume gauges so a deleted volume stops reporting.
func ResetVolumes() {
	local.Reset()
	unfixable.Reset()
}

// SetLocal publishes whether one attached volume has a replica under its own pod.
func SetLocal(namespace, pvc, node string, isLocal bool) {
	v := 0.0
	if isLocal {
		v = 1
	}
	local.WithLabelValues(namespace, pvc, node).Set(v)
}

// SetUnfixable flags a volume the reconciler has decided it will not move.
func SetUnfixable(namespace, pvc, reason string) {
	unfixable.WithLabelValues(namespace, pvc, reason).Set(1)
}

// Serve runs the scrape endpoint until ctx is cancelled.
func Serve(ctx context.Context, addr string) error {
	mux := http.NewServeMux()
	mux.Handle("GET /metrics", promhttp.Handler())
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { fmt.Fprintln(w, "ok") })

	srv := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdown)
	}()

	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("metrics: %w", err)
	}
	return nil
}
