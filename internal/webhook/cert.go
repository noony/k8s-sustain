package webhook

import (
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/go-logr/logr"
	"github.com/prometheus/client_golang/prometheus"
)

// CertExpiry exposes the webhook TLS certificate's NotAfter as a Prometheus
// gauge and logs renewal status. cert-manager rotates the cert on disk
// transparently; the watcher re-reads it periodically so the metric stays
// fresh without a process restart.
//
// The gauge name `k8s_sustain_webhook_cert_expiry_seconds` reports the unix
// timestamp at which the leaf cert expires. Operators alert when
// (gauge - time()) drops below a threshold.
type CertExpiry struct {
	certFile string
	gauge    prometheus.Gauge
	log      logr.Logger

	mu      sync.Mutex
	expires time.Time
}

// NewCertExpiry returns a CertExpiry watching certFile and registering the
// gauge with the supplied registerer (use prometheus.DefaultRegisterer to
// expose via promhttp.Handler).
func NewCertExpiry(certFile string, log logr.Logger, registerer prometheus.Registerer) (*CertExpiry, error) {
	g := prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "k8s_sustain_webhook_cert_expiry_seconds",
		Help: "Unix timestamp at which the webhook TLS certificate expires.",
	})
	if registerer != nil {
		if err := registerer.Register(g); err != nil {
			// AlreadyRegisteredError is fine in tests/restarts; surface anything else.
			if are, ok := err.(prometheus.AlreadyRegisteredError); ok {
				if existing, ok2 := are.ExistingCollector.(prometheus.Gauge); ok2 {
					g = existing
				} else {
					return nil, fmt.Errorf("registering cert expiry gauge: %w", err)
				}
			} else {
				return nil, fmt.Errorf("registering cert expiry gauge: %w", err)
			}
		}
	}
	return &CertExpiry{certFile: certFile, gauge: g, log: log}, nil
}

// Refresh reads the cert from disk and updates the gauge. Call once at startup
// and then on a timer. Errors are logged; the gauge is left untouched on error
// so transient read failures don't reset alerting state.
func (c *CertExpiry) Refresh() error {
	exp, err := readCertNotAfter(c.certFile)
	if err != nil {
		c.log.Error(err, "failed to read TLS cert expiry; gauge not updated", "certFile", c.certFile)
		return err
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

// Run blocks until ctxDone fires, refreshing the gauge every interval.
// Intended to be invoked in its own goroutine.
func (c *CertExpiry) Run(ctxDone <-chan struct{}, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctxDone:
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
