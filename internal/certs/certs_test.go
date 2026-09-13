package certs

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"testing"

	admissionv1 "k8s.io/api/admissionregistration/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func testCfg() Config {
	return Config{
		Namespace:  "lra",
		SecretName: "lra-tls",
		Service:    "lra-webhook",
		WebhookRef: "lra",
	}
}

func webhookConfig() *admissionv1.MutatingWebhookConfiguration {
	return &admissionv1.MutatingWebhookConfiguration{
		ObjectMeta: metav1.ObjectMeta{Name: "lra"},
		Webhooks: []admissionv1.MutatingWebhook{
			{Name: "a.lra.io"},
			{Name: "b.lra.io"},
		},
	}
}

func leafOf(t *testing.T, b Bundle) *x509.Certificate {
	t.Helper()
	blk, _ := pem.Decode(b.TLSCert)
	if blk == nil {
		t.Fatal("tls.crt is not PEM")
	}
	c, err := x509.ParseCertificate(blk.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestEnsureCreatesSecretAndPublishesCA(t *testing.T) {
	t.Parallel()
	kc := fake.NewClientset(webhookConfig())
	cfg := testCfg()

	b, err := Ensure(context.Background(), kc, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(b.CACert) == 0 || len(b.TLSCert) == 0 || len(b.TLSKey) == 0 {
		t.Fatal("incomplete bundle")
	}

	sec, err := kc.CoreV1().Secrets("lra").Get(context.Background(), "lra-tls", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if sec.Type != corev1.SecretTypeTLS {
		t.Errorf("secret type = %q", sec.Type)
	}

	wh, err := kc.AdmissionregistrationV1().MutatingWebhookConfigurations().
		Get(context.Background(), "lra", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for i, w := range wh.Webhooks {
		if string(w.ClientConfig.CABundle) != string(b.CACert) {
			t.Errorf("webhook %d caBundle not published", i)
		}
	}
}

func TestLeafCoversEveryServiceName(t *testing.T) {
	t.Parallel()
	kc := fake.NewClientset(webhookConfig())
	b, err := Ensure(context.Background(), kc, testCfg())
	if err != nil {
		t.Fatal(err)
	}
	leaf := leafOf(t, b)
	// The apiserver dials the fully qualified Service name, so that one must verify.
	for _, name := range []string{"lra-webhook.lra.svc", "lra-webhook.lra.svc.cluster.local"} {
		if err := leaf.VerifyHostname(name); err != nil {
			t.Errorf("cert does not cover %s: %v", name, err)
		}
	}
}

func TestLeafIsSignedByTheStoredCA(t *testing.T) {
	t.Parallel()
	kc := fake.NewClientset(webhookConfig())
	b, err := Ensure(context.Background(), kc, testCfg())
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(b.CACert) {
		t.Fatal("ca.crt is not usable PEM")
	}
	if _, err := leafOf(t, b).Verify(x509.VerifyOptions{
		Roots:     pool,
		DNSName:   "lra-webhook.lra.svc",
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}); err != nil {
		t.Fatalf("leaf does not chain to the published CA: %v", err)
	}
}

func TestEnsureIsIdempotent(t *testing.T) {
	t.Parallel()
	kc := fake.NewClientset(webhookConfig())
	cfg := testCfg()

	first, err := Ensure(context.Background(), kc, cfg)
	if err != nil {
		t.Fatal(err)
	}
	second, err := Ensure(context.Background(), kc, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if string(first.TLSCert) != string(second.TLSCert) {
		t.Fatal("a second call regenerated the certificate; every restart would churn it")
	}
}

func TestRegeneratesOnCorruptSecret(t *testing.T) {
	t.Parallel()
	kc := fake.NewClientset(webhookConfig(), &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "lra-tls", Namespace: "lra"},
		Data:       map[string][]byte{caCertKey: []byte("x"), tlsCertKey: []byte("not pem"), tlsKeyKey: []byte("y")},
	})
	b, err := Ensure(context.Background(), kc, testCfg())
	if err != nil {
		t.Fatal(err)
	}
	if string(b.TLSCert) == "not pem" {
		t.Fatal("kept the corrupt certificate")
	}
	leafOf(t, b) // must parse
}

func TestRegeneratesWhenServiceRenamed(t *testing.T) {
	t.Parallel()
	kc := fake.NewClientset(webhookConfig())
	old := testCfg()
	if _, err := Ensure(context.Background(), kc, old); err != nil {
		t.Fatal(err)
	}

	renamed := old
	renamed.Service = "lra-webhook-v2"
	b, err := Ensure(context.Background(), kc, renamed)
	if err != nil {
		t.Fatal(err)
	}
	if err := leafOf(t, b).VerifyHostname("lra-webhook-v2.lra.svc"); err != nil {
		t.Fatalf("cert was not reissued for the new Service name: %v", err)
	}
}

func TestPublishSkipsWhenAlreadyCurrent(t *testing.T) {
	t.Parallel()
	kc := fake.NewClientset(webhookConfig())
	cfg := testCfg()
	if _, err := Ensure(context.Background(), kc, cfg); err != nil {
		t.Fatal(err)
	}

	// A second Ensure must not rewrite an identical caBundle.
	before, err := kc.AdmissionregistrationV1().MutatingWebhookConfigurations().
		Get(context.Background(), "lra", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Ensure(context.Background(), kc, cfg); err != nil {
		t.Fatal(err)
	}
	after, err := kc.AdmissionregistrationV1().MutatingWebhookConfigurations().
		Get(context.Background(), "lra", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if before.ResourceVersion != after.ResourceVersion {
		t.Error("caBundle was rewritten despite being unchanged")
	}
}

func TestEnsureWithoutWebhookRef(t *testing.T) {
	t.Parallel()
	kc := fake.NewClientset()
	cfg := testCfg()
	cfg.WebhookRef = "" // cert-manager-less but webhook config managed elsewhere
	if _, err := Ensure(context.Background(), kc, cfg); err != nil {
		t.Fatalf("should not need a webhook configuration to exist: %v", err)
	}
}

func TestMissingWebhookConfigurationIsAnError(t *testing.T) {
	t.Parallel()
	kc := fake.NewClientset()
	if _, err := Ensure(context.Background(), kc, testCfg()); err == nil {
		t.Fatal("a missing webhook configuration must not be silently ignored")
	}
}

func TestEnsureRejectsEmptyNamespace(t *testing.T) {
	t.Parallel()
	cfg := testCfg()
	cfg.Namespace = ""
	if _, err := Ensure(context.Background(), fake.NewClientset(), cfg); err == nil {
		t.Fatal("an empty namespace must be rejected, not written to the cluster scope")
	}
}
