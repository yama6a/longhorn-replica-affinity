// Package certs bootstraps the webhook's serving certificate without cert-manager: it
// generates a CA and a leaf, parks them in a Secret so every replica and every restart
// agrees, and publishes the CA into the webhook configuration's caBundle.
package certs

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
)

const (
	caCertKey  = "ca.crt"
	tlsCertKey = "tls.crt"
	tlsKeyKey  = "tls.key"

	validity = 10 * 365 * 24 * time.Hour
	// Rotate well before expiry so a cluster that is only reconciled occasionally still
	// swaps the cert long before anything stops trusting it.
	renewBefore = 90 * 24 * time.Hour
)

// Bundle is a serving keypair plus the CA that signed it.
type Bundle struct {
	CACert  []byte
	TLSCert []byte
	TLSKey  []byte
}

// Config names the objects to write.
type Config struct {
	Namespace  string
	SecretName string
	Service    string
	WebhookRef string // the MutatingWebhookConfiguration whose caBundle to publish into
}

func (c Config) validate() error {
	if c.Namespace == "" {
		return errors.New("namespace is empty: set LRA_NAMESPACE from fieldRef metadata.namespace")
	}
	if c.SecretName == "" || c.Service == "" {
		return errors.New("secret name and service name are both required")
	}
	return nil
}

// dnsNames are the only names the apiserver ever dials the webhook by.
func (c Config) dnsNames() []string {
	return []string{
		c.Service,
		fmt.Sprintf("%s.%s", c.Service, c.Namespace),
		fmt.Sprintf("%s.%s.svc", c.Service, c.Namespace),
		fmt.Sprintf("%s.%s.svc.cluster.local", c.Service, c.Namespace),
	}
}

// Ensure returns a usable bundle, generating and storing one if the Secret is missing or
// close to expiry, and publishes its CA to the webhook configuration.
//
// Safe to run from every replica at once: creation races resolve by re-reading the
// winner's Secret, and rotation uses the API server's own optimistic concurrency.
func Ensure(ctx context.Context, kc kubernetes.Interface, cfg Config) (Bundle, error) {
	if err := cfg.validate(); err != nil {
		return Bundle{}, err
	}
	b, found, err := load(ctx, kc, cfg)
	if err != nil {
		return Bundle{}, err
	}
	if found {
		ok, err := usable(b, cfg)
		if err != nil {
			return Bundle{}, err
		}
		if ok {
			return b, publish(ctx, kc, cfg, b.CACert)
		}
	}

	fresh, err := generate(cfg)
	if err != nil {
		return Bundle{}, err
	}
	stored, err := store(ctx, kc, cfg, fresh, found)
	if err != nil {
		return Bundle{}, err
	}
	return stored, publish(ctx, kc, cfg, stored.CACert)
}

func load(ctx context.Context, kc kubernetes.Interface, cfg Config) (Bundle, bool, error) {
	sec, err := kc.CoreV1().Secrets(cfg.Namespace).Get(ctx, cfg.SecretName, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return Bundle{}, false, nil
	}
	if err != nil {
		return Bundle{}, false, fmt.Errorf("get secret %s/%s: %w", cfg.Namespace, cfg.SecretName, err)
	}
	return Bundle{CACert: sec.Data[caCertKey], TLSCert: sec.Data[tlsCertKey], TLSKey: sec.Data[tlsKeyKey]}, true, nil
}

// usable reports whether a stored bundle is complete, parses, still covers every name the
// apiserver dials, and is not near expiry.
func usable(b Bundle, cfg Config) (bool, error) {
	if len(b.CACert) == 0 || len(b.TLSCert) == 0 || len(b.TLSKey) == 0 {
		return false, nil
	}
	block, _ := pem.Decode(b.TLSCert)
	if block == nil {
		return false, nil
	}
	leaf, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return false, nil //nolint:nilerr // a corrupt cert is a reason to regenerate, not to fail
	}
	if time.Now().Add(renewBefore).After(leaf.NotAfter) {
		return false, nil
	}
	for _, want := range cfg.dnsNames() {
		if err := leaf.VerifyHostname(want); err != nil {
			return false, nil //nolint:nilerr // the Service was renamed; regenerate for the new name
		}
	}
	return true, nil
}

func generate(cfg Config) (Bundle, error) {
	caKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return Bundle{}, fmt.Errorf("generate ca key: %w", err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return Bundle{}, fmt.Errorf("serial: %w", err)
	}
	now := time.Now().Add(-5 * time.Minute) // tolerate a little clock skew between nodes
	caTmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: cfg.Service + "-ca"},
		NotBefore:             now,
		NotAfter:              now.Add(validity),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	if err != nil {
		return Bundle{}, fmt.Errorf("create ca: %w", err)
	}
	ca, err := x509.ParseCertificate(caDER)
	if err != nil {
		return Bundle{}, fmt.Errorf("parse ca: %w", err)
	}

	leafKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return Bundle{}, fmt.Errorf("generate leaf key: %w", err)
	}
	leafSerial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return Bundle{}, fmt.Errorf("leaf serial: %w", err)
	}
	leafTmpl := &x509.Certificate{
		SerialNumber: leafSerial,
		Subject:      pkix.Name{CommonName: cfg.dnsNames()[2]},
		DNSNames:     cfg.dnsNames(),
		NotBefore:    now,
		NotAfter:     now.Add(validity),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	leafDER, err := x509.CreateCertificate(rand.Reader, leafTmpl, ca, &leafKey.PublicKey, caKey)
	if err != nil {
		return Bundle{}, fmt.Errorf("create leaf: %w", err)
	}

	return Bundle{
		CACert:  pemBlock("CERTIFICATE", caDER),
		TLSCert: pemBlock("CERTIFICATE", leafDER),
		TLSKey:  pemBlock("RSA PRIVATE KEY", x509.MarshalPKCS1PrivateKey(leafKey)),
	}, nil
}

func pemBlock(kind string, der []byte) []byte {
	var buf bytes.Buffer
	_ = pem.Encode(&buf, &pem.Block{Type: kind, Bytes: der})
	return buf.Bytes()
}

// store writes the bundle. On a create race the other replica's Secret wins and is
// returned instead, so both serve the same certificate.
func store(ctx context.Context, kc kubernetes.Interface, cfg Config, b Bundle, exists bool) (Bundle, error) {
	sec := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: cfg.SecretName, Namespace: cfg.Namespace},
		Type:       corev1.SecretTypeTLS,
		Data:       map[string][]byte{caCertKey: b.CACert, tlsCertKey: b.TLSCert, tlsKeyKey: b.TLSKey},
	}

	if !exists {
		_, err := kc.CoreV1().Secrets(cfg.Namespace).Create(ctx, sec, metav1.CreateOptions{})
		if apierrors.IsAlreadyExists(err) {
			winner, _, rerr := load(ctx, kc, cfg)
			if rerr != nil {
				return Bundle{}, rerr
			}
			return winner, nil
		}
		if err != nil {
			return Bundle{}, fmt.Errorf("create secret: %w", err)
		}
		return b, nil
	}

	_, err := kc.CoreV1().Secrets(cfg.Namespace).Update(ctx, sec, metav1.UpdateOptions{})
	if apierrors.IsConflict(err) {
		winner, _, rerr := load(ctx, kc, cfg)
		if rerr != nil {
			return Bundle{}, rerr
		}
		return winner, nil
	}
	if err != nil {
		return Bundle{}, fmt.Errorf("update secret: %w", err)
	}
	return b, nil
}

// publish writes the CA into every webhook entry's caBundle. Without this the apiserver
// has no reason to trust the cert and every call fails its TLS handshake.
func publish(ctx context.Context, kc kubernetes.Interface, cfg Config, ca []byte) error {
	if cfg.WebhookRef == "" {
		return nil
	}
	cur, err := kc.AdmissionregistrationV1().MutatingWebhookConfigurations().
		Get(ctx, cfg.WebhookRef, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("get webhook configuration %s: %w", cfg.WebhookRef, err)
	}

	stale := false
	patches := make([]map[string]any, 0, len(cur.Webhooks))
	for i, w := range cur.Webhooks {
		if !bytes.Equal(w.ClientConfig.CABundle, ca) {
			stale = true
		}
		patches = append(patches, map[string]any{
			"op":    "replace",
			"path":  fmt.Sprintf("/webhooks/%d/clientConfig/caBundle", i),
			"value": ca,
		})
	}
	if !stale || len(patches) == 0 {
		return nil
	}

	body, err := json.Marshal(patches)
	if err != nil {
		return fmt.Errorf("marshal caBundle patch: %w", err)
	}
	_, err = kc.AdmissionregistrationV1().MutatingWebhookConfigurations().
		Patch(ctx, cfg.WebhookRef, types.JSONPatchType, body, metav1.PatchOptions{})
	if err != nil {
		return fmt.Errorf("patch caBundle on %s: %w", cfg.WebhookRef, err)
	}
	return nil
}
