package webhook

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"time"

	admissionv1 "k8s.io/api/admission/v1"

	"github.com/yama6a/longhorn-replica-affinity/internal/metrics"
)

const maxBody = 3 << 20

// Server is the TLS admission endpoint.
type Server struct {
	Addr     string
	CertFile string
	KeyFile  string
	Admitter *Admitter
	Log      *slog.Logger

	mu   sync.RWMutex
	cert *tls.Certificate
}

// Serve blocks until ctx is cancelled or the listener fails.
func (s *Server) Serve(ctx context.Context) error {
	if err := s.loadCert(); err != nil {
		return err
	}
	// cert-manager rotates the Secret in place without restarting the pod.
	go s.watchCert(ctx)

	mux := http.NewServeMux()
	mux.HandleFunc("POST /mutate", s.handleMutate)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		if !s.Admitter.Index.Synced() {
			http.Error(w, "cache cold", http.StatusServiceUnavailable)
			return
		}
		fmt.Fprintln(w, "ok")
	})

	srv := &http.Server{
		Addr:              s.Addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		TLSConfig: &tls.Config{
			MinVersion:     tls.VersionTLS13,
			GetCertificate: func(*tls.ClientHelloInfo) (*tls.Certificate, error) { return s.current(), nil },
		},
	}

	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdown)
	}()

	s.Log.Info("webhook listening", "addr", s.Addr)
	if err := srv.ListenAndServeTLS("", ""); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("serve: %w", err)
	}
	return nil
}

func (s *Server) handleMutate(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBody))
	if err != nil {
		http.Error(w, "read body", http.StatusBadRequest)
		return
	}

	var review admissionv1.AdmissionReview
	if err := json.Unmarshal(body, &review); err != nil || review.Request == nil {
		http.Error(w, "decode review", http.StatusBadRequest)
		return
	}

	resp, d := s.Admitter.Review(review.Request)
	metrics.Admission(d.Skipped, len(d.Nodes) > 0)
	if d.Skipped != "" {
		s.Log.Debug("skipped", "ns", d.Namespace, "pod", d.Pod, "reason", d.Skipped)
	} else {
		s.Log.Info("injected", "ns", d.Namespace, "pod", d.Pod, "volumes", d.Volumes, "nodes", d.Nodes)
	}

	out := admissionv1.AdmissionReview{TypeMeta: review.TypeMeta, Response: resp}
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(out); err != nil {
		s.Log.Error("write response", "err", err)
	}
}

func (s *Server) loadCert() error {
	c, err := tls.LoadX509KeyPair(s.CertFile, s.KeyFile)
	if err != nil {
		return fmt.Errorf("load keypair: %w", err)
	}
	s.mu.Lock()
	s.cert = &c
	s.mu.Unlock()
	return nil
}

func (s *Server) watchCert(ctx context.Context) {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := s.loadCert(); err != nil {
				s.Log.Error("reload cert", "err", err)
			}
		}
	}
}

func (s *Server) current() *tls.Certificate {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.cert
}
