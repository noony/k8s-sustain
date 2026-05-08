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
	path, _ := writeTestCertAndKey(t, notAfter, false)
	return path
}

// writeTestCertAndKey generates a self-signed keypair and writes both PEM
// blobs. When writeKey is false, the key path is "" (matching the
// metric-only mode of CertExpiry).
func writeTestCertAndKey(t *testing.T, notAfter time.Time, writeKey bool) (certPath, keyPath string) {
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
	certPath = filepath.Join(dir, "tls.crt")
	if err := os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatalf("write cert: %v", err)
	}
	if writeKey {
		keyDER, err := x509.MarshalECPrivateKey(key)
		if err != nil {
			t.Fatalf("marshal key: %v", err)
		}
		keyPath = filepath.Join(dir, "tls.key")
		if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
			t.Fatalf("write key: %v", err)
		}
	}
	return certPath, keyPath
}

func TestCertExpiry_GetCertificate_ReloadsOnRefresh(t *testing.T) {
	first := time.Now().Add(48 * time.Hour).Truncate(time.Second)
	certPath, keyPath := writeTestCertAndKey(t, first, true)

	c, err := NewCertExpiry(certPath, keyPath, logr.Discard(), prometheus.NewRegistry())
	if err != nil {
		t.Fatalf("NewCertExpiry: %v", err)
	}
	if _, err := c.GetCertificate(nil); err == nil {
		t.Fatal("GetCertificate should error before first Refresh")
	}
	if err := c.Refresh(); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	got1, err := c.GetCertificate(nil)
	if err != nil {
		t.Fatalf("GetCertificate after Refresh: %v", err)
	}
	if got1 == nil || len(got1.Certificate) == 0 {
		t.Fatal("expected non-nil certificate after Refresh")
	}

	// Rotate: overwrite both files with a freshly generated keypair on the
	// same paths and re-Refresh. The cached cert pointer must change.
	second := time.Now().Add(96 * time.Hour).Truncate(time.Second)
	newCertPath, newKeyPath := writeTestCertAndKey(t, second, true)
	if err := os.Rename(newCertPath, certPath); err != nil {
		t.Fatalf("rename cert: %v", err)
	}
	if err := os.Rename(newKeyPath, keyPath); err != nil {
		t.Fatalf("rename key: %v", err)
	}
	if err := c.Refresh(); err != nil {
		t.Fatalf("Refresh after rotation: %v", err)
	}
	got2, err := c.GetCertificate(nil)
	if err != nil {
		t.Fatalf("GetCertificate after rotation: %v", err)
	}
	if got1 == got2 {
		t.Error("expected hot-reloaded *tls.Certificate to be a new pointer")
	}
	if c.ExpiresAt().Unix() != second.Unix() {
		t.Errorf("ExpiresAt = %d, want %d", c.ExpiresAt().Unix(), second.Unix())
	}
}

func TestCertExpiry_RefreshSetsGauge(t *testing.T) {
	expiry := time.Now().Add(48 * time.Hour).Truncate(time.Second)
	path := writeTestCert(t, expiry)

	reg := prometheus.NewRegistry()
	c, err := NewCertExpiry(path, "", logr.Discard(), reg)
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
	c, err := NewCertExpiry("/does/not/exist.crt", "", logr.Discard(), prometheus.NewRegistry())
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
	c, err := NewCertExpiry(path, "", logr.Discard(), prometheus.NewRegistry())
	if err != nil {
		t.Fatalf("NewCertExpiry: %v", err)
	}
	if err := c.Refresh(); err == nil {
		t.Fatal("expected error for non-PEM content")
	}
}
