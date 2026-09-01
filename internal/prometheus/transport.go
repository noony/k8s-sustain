package prometheus

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"

	"github.com/prometheus/client_golang/api"
	"golang.org/x/net/http/httpguts"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// TransportConfig describes authentication and TLS settings for reaching Prometheus.
//
// The zero value means "no authentication, default transport" and is exactly
// what New has always used, so a client constructed without
// WithTransportConfig is byte-for-byte unchanged.
//
// It covers the two deployments that plain unauthenticated HTTP cannot reach:
// a Prometheus behind an auth proxy (bearer or basic credentials, custom CA)
// and a Thanos/Mimir/Cortex query gateway, which additionally needs a tenant
// header — hence Headers rather than a fixed X-Scope-OrgID field, since the
// header name differs across gateways and deployments.
type TransportConfig struct {
	// BearerToken is a static token sent as `Authorization: Bearer <token>`.
	// Mutually exclusive with BearerTokenFile.
	BearerToken string
	// BearerTokenFile is a path whose contents are sent as the bearer token.
	// It is re-read per request — see authRoundTripper.RoundTrip.
	BearerTokenFile string
	// BasicAuthUsername enables HTTP basic auth. Required whenever a password
	// or password file is set.
	BasicAuthUsername string
	// BasicAuthPassword is the static basic-auth password. Mutually exclusive
	// with BasicAuthPasswordFile.
	BasicAuthPassword string
	// BasicAuthPasswordFile is a path whose contents are the basic-auth
	// password. Re-read per request, like BearerTokenFile.
	BasicAuthPasswordFile string
	// Headers are applied verbatim to every request, e.g.
	// "X-Scope-OrgID": "tenant-a" for a multi-tenant Thanos/Mimir gateway.
	Headers map[string]string
	// TLS configures the transport's TLS client settings.
	TLS TLSConfig
}

// TLSConfig configures TLS for the Prometheus transport.
type TLSConfig struct {
	// CAFile is a PEM bundle appended to the system trust store (see
	// buildTLSConfig for why it is appended rather than replacing it).
	CAFile string
	// CertFile and KeyFile enable mutual TLS. Both must be set, or neither.
	CertFile string
	KeyFile  string
	// ServerName overrides the SNI / certificate hostname, for a Prometheus
	// reached through an address that does not match its certificate.
	ServerName string
	// InsecureSkipVerify disables server-certificate verification. Honoured,
	// but logged loudly at construction.
	InsecureSkipVerify bool
}

// isZero reports whether the config asks for nothing at all, in which case the
// client is built with no RoundTripper — identical to the pre-auth behaviour.
func (t TransportConfig) isZero() bool {
	return t.BearerToken == "" &&
		t.BearerTokenFile == "" &&
		t.BasicAuthUsername == "" &&
		t.BasicAuthPassword == "" &&
		t.BasicAuthPasswordFile == "" &&
		len(t.Headers) == 0 &&
		t.TLS == TLSConfig{}
}

// hasTLS reports whether any TLS field was set, i.e. whether the default
// transport has to be cloned and given a tls.Config at all.
func (t TransportConfig) hasTLS() bool {
	return t.TLS != TLSConfig{}
}

// authHeader is the header the bearer/basic layers own. Headers may set it
// only when neither of those layers is active — see validate.
const authHeader = "Authorization"

// WithTransportConfig configures authentication, custom headers, and TLS for
// every request the client makes. This is the normal way to reach an
// authenticated Prometheus or a multi-tenant Thanos/Mimir/Cortex gateway.
//
// Mutually exclusive with WithRoundTripper: that option replaces the whole
// transport, so silently dropping either one would be worse than refusing.
func WithTransportConfig(cfg TransportConfig) Option {
	return func(c *Client) {
		c.transport = cfg
		c.transportSet = true
	}
}

// WithRoundTripper installs an explicit http.RoundTripper, bypassing
// TransportConfig entirely. Escape hatch for transports this package does not
// model (SigV4, mTLS from an in-memory keypair, a recording transport in
// tests). Mutually exclusive with WithTransportConfig.
func WithRoundTripper(rt http.RoundTripper) Option {
	return func(c *Client) {
		c.roundTripper = rt
	}
}

// validate rejects combinations that would otherwise resolve silently and
// wrongly at request time — a token that is quietly ignored because basic auth
// also happened to be set is an outage nobody can read off a config dump.
func (t TransportConfig) validate() error {
	if t.BearerToken != "" && t.BearerTokenFile != "" {
		return errors.New("bearer token and bearer token file are mutually exclusive")
	}
	if t.BasicAuthPassword != "" && t.BasicAuthPasswordFile != "" {
		return errors.New("basic auth password and password file are mutually exclusive")
	}
	bearer := t.BearerToken != "" || t.BearerTokenFile != ""
	basic := t.BasicAuthUsername != ""
	if bearer && basic {
		return errors.New("bearer token and basic auth are mutually exclusive")
	}
	if !basic && (t.BasicAuthPassword != "" || t.BasicAuthPasswordFile != "") {
		return errors.New("basic auth password set without a username")
	}
	for name, value := range t.Headers {
		if strings.TrimSpace(name) == "" {
			return errors.New("empty header name")
		}
		// http.Transport rejects a malformed name or value on EVERY request,
		// which would trip the circuit breaker and read as a Prometheus outage.
		// Catch it here so it fails the process at startup like every other
		// config error.
		if !httpguts.ValidHeaderFieldName(name) {
			return fmt.Errorf("invalid header name %q: must be a valid HTTP token (no spaces or control characters)", name)
		}
		if !httpguts.ValidHeaderFieldValue(value) {
			return fmt.Errorf("invalid value for header %q: must not contain control characters (CR, LF, NUL)", name)
		}
		if http.CanonicalHeaderKey(name) == authHeader && (bearer || basic) {
			return fmt.Errorf("header %q conflicts with the configured bearer/basic auth: set one or the other", name)
		}
	}
	if (t.TLS.CertFile == "") != (t.TLS.KeyFile == "") {
		return errors.New("tls cert file and key file must be set together")
	}
	return nil
}

// buildTLSConfig turns the TLS settings into a *tls.Config, reading every file
// eagerly so a typo in a path fails at startup instead of on the first query.
func buildTLSConfig(cfg TLSConfig) (*tls.Config, error) {
	//nolint:gosec // G402: InsecureSkipVerify is an explicit, documented operator opt-in, warned about at construction.
	out := &tls.Config{
		MinVersion:         tls.VersionTLS12,
		ServerName:         cfg.ServerName,
		InsecureSkipVerify: cfg.InsecureSkipVerify,
	}

	if cfg.CAFile != "" {
		// The custom CA is APPENDED to a copy of the system pool rather than
		// installed as the only root. A single Prometheus address is commonly
		// fronted by a public-CA ingress while an internal CA signs something
		// else on the same path (redirects, sidecar proxies); replacing the
		// pool would break those with a bewildering "unknown authority" for a
		// certificate the host already trusts. Appending is strictly additive:
		// it grants trust to the operator's CA without revoking any.
		pool, err := x509.SystemCertPool()
		if err != nil || pool == nil {
			// A platform without a readable system store is not an error here;
			// fall back to a pool containing only the configured CA. It IS
			// worth a log line, though: from here on the CA file is the only
			// root, so a public-CA ingress in front of the same address will
			// fail with "unknown authority" — the failure appending exists to
			// prevent — and nothing else in the startup log would explain why.
			log.Log.WithName("prometheus").Info(
				"WARNING: system certificate pool unavailable; the Prometheus TLS CA file is the ONLY trusted root",
				"caFile", cfg.CAFile, "error", err)
			pool = x509.NewCertPool()
		}
		pem, err := os.ReadFile(cfg.CAFile)
		if err != nil {
			return nil, fmt.Errorf("reading prometheus TLS CA file %q: %w", cfg.CAFile, err)
		}
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("prometheus TLS CA file %q contains no valid PEM certificate", cfg.CAFile)
		}
		out.RootCAs = pool
	}

	if cfg.CertFile != "" {
		// Read once now so a broken pair fails at startup, then again on every
		// handshake via GetClientCertificate so a rotated pair is picked up
		// without a restart — the same property the bearer-token and password
		// files have (see authRoundTripper.RoundTrip). A handshake happens
		// once per connection, not per request, so the reload cost is nil.
		if _, err := loadKeyPair(cfg.CertFile, cfg.KeyFile); err != nil {
			return nil, err
		}
		certFile, keyFile := cfg.CertFile, cfg.KeyFile
		out.GetClientCertificate = func(*tls.CertificateRequestInfo) (*tls.Certificate, error) {
			return loadKeyPair(certFile, keyFile)
		}
	}

	return out, nil
}

// loadKeyPair reads the mTLS client certificate and key from disk.
func loadKeyPair(certFile, keyFile string) (*tls.Certificate, error) {
	pair, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, fmt.Errorf("loading prometheus TLS key pair (%q, %q): %w", certFile, keyFile, err)
	}
	return &pair, nil
}

// baseRoundTripper returns the transport the auth layer wraps: a clone of
// api.DefaultRoundTripper so the Prometheus client's own defaults (env proxy,
// dial/handshake timeouts, idle-conn pooling, HTTP/2) are preserved, with the
// TLS config applied when one is configured.
func baseRoundTripper(cfg TransportConfig) (http.RoundTripper, error) {
	if !cfg.hasTLS() {
		return api.DefaultRoundTripper, nil
	}
	base, ok := api.DefaultRoundTripper.(*http.Transport)
	if !ok {
		return nil, fmt.Errorf("cannot apply TLS settings: api.DefaultRoundTripper is %T, not *http.Transport", api.DefaultRoundTripper)
	}
	tlsCfg, err := buildTLSConfig(cfg.TLS)
	if err != nil {
		return nil, err
	}
	// Clone so the package-level default is never mutated — it is shared with
	// every other client_golang consumer in this process.
	transport := base.Clone()
	transport.TLSClientConfig = tlsCfg
	return transport, nil
}

// authRoundTripper injects credentials and static headers into every request.
type authRoundTripper struct {
	base http.RoundTripper
	cfg  TransportConfig
}

// readSecretFile reads a credential file and trims surrounding whitespace,
// which a token/password file almost always carries as a trailing newline and
// which would otherwise be sent as part of the credential.
func readSecretFile(path string) (string, error) {
	b, err := os.ReadFile(path) //nolint:gosec // G304: operator-supplied config path, by design.
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(b)), nil
}

// RoundTrip applies headers and credentials to a CLONE of req.
//
// Cloning is mandatory, not defensive: http.RoundTripper's contract forbids
// modifying the request, and the caller (and any retry logic above us) may
// reuse it. Mutating the shared Header map would also race under -race with
// concurrent in-flight queries built from the same request.
//
// Credential FILES are re-read on every request rather than cached. Kubernetes
// projected service-account tokens are rotated in place roughly hourly, so a
// token read once at construction silently starts returning 401 after the
// first rotation — the failure mode this file exists to avoid. A read of a
// small local file costs microseconds and this client issues at most a handful
// of queries per reconcile (bounded by --prometheus-max-inflight, default 8),
// so a cache with its own staleness window and invalidation rules would add
// a correctness hazard to save nothing measurable.
func (a *authRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	r := req.Clone(req.Context())

	for name, value := range a.cfg.Headers {
		r.Header.Set(name, value)
	}

	switch {
	case a.cfg.BearerToken != "":
		r.Header.Set(authHeader, "Bearer "+a.cfg.BearerToken)
	case a.cfg.BearerTokenFile != "":
		token, err := readSecretFile(a.cfg.BearerTokenFile)
		if err != nil {
			return nil, fmt.Errorf("reading prometheus bearer token file %q: %w", a.cfg.BearerTokenFile, err)
		}
		r.Header.Set(authHeader, "Bearer "+token)
	case a.cfg.BasicAuthUsername != "":
		password := a.cfg.BasicAuthPassword
		if a.cfg.BasicAuthPasswordFile != "" {
			p, err := readSecretFile(a.cfg.BasicAuthPasswordFile)
			if err != nil {
				return nil, fmt.Errorf("reading prometheus basic auth password file %q: %w", a.cfg.BasicAuthPasswordFile, err)
			}
			password = p
		}
		r.SetBasicAuth(a.cfg.BasicAuthUsername, password)
	}

	return a.base.RoundTrip(r)
}

// newTransportRoundTripper validates cfg and returns the RoundTripper it
// describes, or nil when cfg asks for nothing.
//
// Every file it names is read once here so an unreadable CA, a malformed key
// pair, or a missing token file fails the process at startup — visible in a
// CrashLoopBackOff — instead of degrading into per-query errors that look like
// a Prometheus outage.
func newTransportRoundTripper(cfg TransportConfig) (http.RoundTripper, error) {
	if cfg.isZero() {
		return nil, nil
	}
	if err := cfg.validate(); err != nil {
		return nil, fmt.Errorf("invalid prometheus transport config: %w", err)
	}
	if cfg.TLS.InsecureSkipVerify {
		log.Log.WithName("prometheus").Info(
			"WARNING: prometheus TLS certificate verification is DISABLED (insecure-skip-verify); the connection is vulnerable to interception")
	}
	if cfg.BearerTokenFile != "" {
		if _, err := readSecretFile(cfg.BearerTokenFile); err != nil {
			return nil, fmt.Errorf("reading prometheus bearer token file %q: %w", cfg.BearerTokenFile, err)
		}
	}
	if cfg.BasicAuthPasswordFile != "" {
		if _, err := readSecretFile(cfg.BasicAuthPasswordFile); err != nil {
			return nil, fmt.Errorf("reading prometheus basic auth password file %q: %w", cfg.BasicAuthPasswordFile, err)
		}
	}
	base, err := baseRoundTripper(cfg)
	if err != nil {
		return nil, err
	}
	return &authRoundTripper{base: base, cfg: cfg}, nil
}

// resolveRoundTripper picks the effective RoundTripper for a client whose
// options have already been applied. It returns nil when neither transport
// option was used, so New falls through to api.Config's own default.
func (c *Client) resolveRoundTripper() (http.RoundTripper, error) {
	if c.roundTripper != nil {
		if c.transportSet {
			return nil, errors.New("invalid prometheus transport config: WithRoundTripper and WithTransportConfig are mutually exclusive")
		}
		return c.roundTripper, nil
	}
	return newTransportRoundTripper(c.transport)
}
