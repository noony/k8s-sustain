package webhook

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/prometheus/client_golang/prometheus"
)

// writeTestCert generates a self-signed cert with the given expiry and writes
// it (PEM-encoded) to a temp file, returning the path.
func writeTestCert(t *testing.T, notAfter time.Time) string {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("genkey: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "test"},
		NotBefore:    notAfter.Add(-time.Hour),
		NotAfter:     notAfter,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create cert: %v", err)
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "tls.crt")
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	if err := os.WriteFile(path, pemBytes, 0o600); err != nil {
		t.Fatalf("write cert: %v", err)
	}
	return path
}

func TestCertExpiry_RefreshSetsGauge(t *testing.T) {
	expiry := time.Now().Add(48 * time.Hour).Truncate(time.Second)
	path := writeTestCert(t, expiry)

	reg := prometheus.NewRegistry()
	c, err := NewCertExpiry(path, logr.Discard(), reg)
	if err != nil {
		t.Fatalf("NewCertExpiry: %v", err)
	}
	if err := c.Refresh(); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if got, want := c.ExpiresAt().Unix(), expiry.Unix(); got != want {
		t.Errorf("ExpiresAt = %d, want %d", got, want)
	}
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	var found bool
	for _, mf := range mfs {
		if mf.GetName() == "k8s_sustain_webhook_cert_expiry_seconds" {
			if len(mf.Metric) != 1 {
				t.Fatalf("expected 1 sample, got %d", len(mf.Metric))
			}
			if v := mf.Metric[0].GetGauge().GetValue(); int64(v) != expiry.Unix() {
				t.Errorf("gauge = %v, want %v", v, expiry.Unix())
			}
			found = true
		}
	}
	if !found {
		t.Fatal("cert expiry gauge not registered")
	}
}

func TestCertExpiry_MissingFile_Error(t *testing.T) {
	c, err := NewCertExpiry("/does/not/exist.crt", logr.Discard(), prometheus.NewRegistry())
	if err != nil {
		t.Fatalf("NewCertExpiry: %v", err)
	}
	if err := c.Refresh(); err == nil {
		t.Fatal("expected error for missing cert file")
	}
}

func TestCertExpiry_BadPEM_Error(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "tls.crt")
	if err := os.WriteFile(path, []byte("not a pem block"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	c, err := NewCertExpiry(path, logr.Discard(), prometheus.NewRegistry())
	if err != nil {
		t.Fatalf("NewCertExpiry: %v", err)
	}
	if err := c.Refresh(); err == nil {
		t.Fatal("expected error for non-PEM content")
	}
}
