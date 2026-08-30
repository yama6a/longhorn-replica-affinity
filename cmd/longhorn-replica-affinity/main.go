// Command longhorn-replica-affinity schedules pods onto nodes that already hold a
// Longhorn replica of their data.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"golang.org/x/sync/errgroup"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"github.com/yama6a/longhorn-replica-affinity/internal/config"
	"github.com/yama6a/longhorn-replica-affinity/internal/index"
	"github.com/yama6a/longhorn-replica-affinity/internal/metrics"
	"github.com/yama6a/longhorn-replica-affinity/internal/reconcile"
	"github.com/yama6a/longhorn-replica-affinity/internal/webhook"
)

// version is stamped at build time with -ldflags.
var version = "dev"

const usage = `longhorn-replica-affinity %s

  webhook     serve the pod mutating admission endpoint
  reconcile   pull a replica local for pods that cannot move
  version     print the version

Configuration is by LRA_* environment variables; see the README.
`

func main() {
	if err := run(); err != nil {
		slog.Error("exit", "err", err)
		os.Exit(1)
	}
}

func run() error {
	if len(os.Args) < 2 {
		fmt.Fprintf(os.Stderr, usage, version)
		return errors.New("no subcommand")
	}
	if os.Args[1] == "version" {
		fmt.Println(version)
		return nil
	}

	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: logLevel()}))
	slog.SetDefault(log)
	metrics.SetVersion(version)

	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("config: %w", err)
	}

	restCfg, err := rest.InClusterConfig()
	if err != nil {
		return fmt.Errorf("in-cluster config: %w", err)
	}
	kc, err := kubernetes.NewForConfig(restCfg)
	if err != nil {
		return fmt.Errorf("kube client: %w", err)
	}
	dc, err := dynamic.NewForConfig(restCfg)
	if err != nil {
		return fmt.Errorf("dynamic client: %w", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	idx := index.New(dc, kc, cfg.LonghornNamespace, cfg.RestoreAnnotation())
	if err := idx.Run(ctx); err != nil {
		return fmt.Errorf("warm caches: %w", err)
	}
	log.Info("caches warm", "version", version, "mode", os.Args[1])

	g, gctx := errgroup.WithContext(ctx)
	g.Go(func() error { return metrics.Serve(gctx, cfg.MetricsAddr) })

	switch os.Args[1] {
	case "webhook":
		s := &webhook.Server{
			Addr: cfg.ListenAddr, CertFile: cfg.CertFile, KeyFile: cfg.KeyFile, Log: log,
			Admitter: &webhook.Admitter{Index: idx, Weight: cfg.Weight, SkipRWX: cfg.SkipRWX, Log: log},
		}
		g.Go(func() error { return s.Serve(gctx) })
	case "reconcile":
		rec := &reconcile.Reconciler{Cfg: cfg, Index: idx, Dyn: dc, Kube: kc, Log: log}
		g.Go(func() error { return rec.Run(gctx) })
	default:
		fmt.Fprintf(os.Stderr, usage, version)
		return fmt.Errorf("unknown subcommand %q", os.Args[1])
	}

	if err := g.Wait(); err != nil {
		return fmt.Errorf("run %s: %w", os.Args[1], err)
	}
	return nil
}

func logLevel() slog.Level {
	var l slog.Level
	if err := l.UnmarshalText([]byte(os.Getenv("LRA_LOG_LEVEL"))); err != nil {
		return slog.LevelInfo
	}
	return l
}
