package webhook

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/go-logr/logr"
	"github.com/prometheus/client_golang/prometheus"
)

// CertExpiry exposes the webhook TLS certificate's NotAfter as a Prometheus
// gauge and, when a key file is supplied, hot-reloads the keypair so cert
// rotation (cert-manager) takes effect without a process restart. The
// http.Server should plug GetCertificate into its TLSConfig.
//
// The gauge name `k8s_sustain_webhook_cert_expiry_seconds` reports the unix
// timestamp at which the leaf cert expires. Operators alert when
// (gauge - time()) drops below a threshold.
type CertExpiry struct {
	certFile string
	keyFile  string
	gauge    prometheus.Gauge
	log      logr.Logger

	mu      sync.Mutex
	expires time.Time

	// cert holds the most recently loaded *tls.Certificate. atomic.Pointer
	// lets GetCertificate read it without taking the lock on every TLS
	// handshake. Nil until the first successful Refresh with a key file.
	cert atomic.Pointer[tls.Certificate]
}

// NewCertExpiry returns a CertExpiry watching certFile and registering the
// gauge with the supplied registerer (use prometheus.DefaultRegisterer to
// expose via promhttp.Handler). If keyFile is non-empty, Refresh additionally
// loads the X509 keypair so GetCertificate returns the latest one on each
// handshake — wire it into http.Server.TLSConfig.GetCertificate to pick up
// cert-manager rotations without restarting.
func NewCertExpiry(certFile, keyFile string, log logr.Logger, registerer prometheus.Registerer) (*CertExpiry, error) {
	g := prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "k8s_sustain_webhook_cert_expiry_seconds",
		Help: "Unix timestamp at which the webhook TLS certificate expires.",
	})
	if registerer != nil {
		if err := registerer.Register(g); err != nil {
			// AlreadyRegisteredError is fine in tests/restarts; surface anything else.
			var are prometheus.AlreadyRegisteredError
			if !errors.As(err, &are) {
				return nil, fmt.Errorf("registering cert expiry gauge: %w", err)
			}
			existing, ok := are.ExistingCollector.(prometheus.Gauge)
			if !ok {
				return nil, fmt.Errorf("registering cert expiry gauge: %w", err)
			}
			g = existing
		}
	}
	return &CertExpiry{certFile: certFile, keyFile: keyFile, gauge: g, log: log}, nil
}

// Refresh reads the cert (and key, if configured) from disk and updates both
// the gauge and the cached keypair. Call once at startup and then on a timer.
// On error, the gauge and the cached cert are left untouched so transient
// read failures don't reset alerting state or break in-flight handshakes.
func (c *CertExpiry) Refresh() error {
	exp, err := readCertNotAfter(c.certFile)
	if err != nil {
		c.log.Error(err, "failed to read TLS cert expiry; gauge not updated", "certFile", c.certFile)
		return err
	}

	if c.keyFile != "" {
		pair, err := tls.LoadX509KeyPair(c.certFile, c.keyFile)
		if err != nil {
			c.log.Error(err, "failed to reload TLS keypair; serving stale cert", "certFile", c.certFile, "keyFile", c.keyFile)
			return err
		}
		c.cert.Store(&pair)
	}

	c.mu.Lock()
	c.expires = exp
	c.mu.Unlock()
	c.gauge.Set(float64(exp.Unix()))

	remaining := time.Until(exp)
	switch {
	case remaining < 0:
		c.log.Error(nil, "TLS cert is already expired", "expiresAt", exp)
	case remaining < 7*24*time.Hour:
		c.log.Info("TLS cert expires soon", "expiresAt", exp, "remaining", remaining.Round(time.Hour))
	default:
		c.log.Info("TLS cert expiry observed", "expiresAt", exp, "remaining", remaining.Round(time.Hour))
	}
	return nil
}

// Run blocks until ctx is cancelled, refreshing the gauge and reloading the
// cached keypair every interval. Intended to be invoked in its own goroutine.
func (c *CertExpiry) Run(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			_ = c.Refresh()
		}
	}
}

// ExpiresAt returns the most recently observed expiry time. Useful in tests.
func (c *CertExpiry) ExpiresAt() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.expires
}

// GetCertificate is suitable for tls.Config.GetCertificate. Returns the most
// recently loaded keypair. Returns an error if no successful keypair load has
// happened yet — callers (the http.Server) will fail the handshake, which is
// the correct behaviour: better than serving an unconfigured cert.
func (c *CertExpiry) GetCertificate(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	cert := c.cert.Load()
	if cert == nil {
		return nil, fmt.Errorf("webhook TLS certificate not yet loaded")
	}
	return cert, nil
}

func readCertNotAfter(path string) (time.Time, error) {
	raw, err := os.ReadFile(path) //nolint:gosec // path is operator-controlled, mounted Secret
	if err != nil {
		return time.Time{}, fmt.Errorf("reading cert: %w", err)
	}
	block, _ := pem.Decode(raw)
	if block == nil {
		return time.Time{}, fmt.Errorf("no PEM block in %s", path)
	}
	leaf, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return time.Time{}, fmt.Errorf("parsing cert: %w", err)
	}
	return leaf.NotAfter, nil
}
