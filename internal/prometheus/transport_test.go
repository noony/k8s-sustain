package prometheus

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/api"
)

// promOK is the minimal successful instant-query body the client_golang API
// accepts, so a test server can answer Ping/QueryInstant without pulling in a
// real Prometheus.
const promOK = `{"status":"success","data":{"resultType":"vector","result":[]}}`

// recordingServer starts an httptest server that records the headers of every
// request it receives and answers with promOK.
type recordingServer struct {
	*httptest.Server
	mu      sync.Mutex
	records []http.Header
}

func newRecordingServer(t *testing.T, tls bool) *recordingServer {
	t.Helper()
	rec := &recordingServer{}
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.mu.Lock()
		rec.records = append(rec.records, r.Header.Clone())
		rec.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(promOK))
	})
	if tls {
		rec.Server = httptest.NewTLSServer(handler)
	} else {
		rec.Server = httptest.NewServer(handler)
	}
	t.Cleanup(rec.Close)
	return rec
}

func (r *recordingServer) header(t *testing.T, i int, name string) string {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	if i >= len(r.records) {
		t.Fatalf("only %d request(s) recorded, wanted index %d", len(r.records), i)
	}
	return r.records[i].Get(name)
}

func (r *recordingServer) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.records)
}

func writeFile(t *testing.T, name, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	return path
}

// TestTransportAppliesCredentialsAndHeaders drives the full New -> query path
// so the assertions cover the RoundTripper as the api client actually uses it,
// not just as constructed.
func TestTransportAppliesCredentialsAndHeaders(t *testing.T) {
	tokenFile := writeFile(t, "token", "file-token\n")
	passwordFile := writeFile(t, "password", "  file-password\n")

	tests := []struct {
		name       string
		cfg        TransportConfig
		wantAuth   string
		wantHeader map[string]string
	}{
		{
			name:     "inline bearer token",
			cfg:      TransportConfig{BearerToken: "inline-token"},
			wantAuth: "Bearer inline-token",
		},
		{
			name: "bearer token from file, trailing newline trimmed",
			cfg:  TransportConfig{BearerTokenFile: tokenFile},
			// The trailing newline a Secret/projected-token file always carries
			// must not become part of the credential.
			wantAuth: "Bearer file-token",
		},
		{
			name:     "inline basic auth",
			cfg:      TransportConfig{BasicAuthUsername: "user", BasicAuthPassword: "pass"},
			wantAuth: "Basic " + basic("user", "pass"),
		},
		{
			name:     "basic auth password from file",
			cfg:      TransportConfig{BasicAuthUsername: "user", BasicAuthPasswordFile: passwordFile},
			wantAuth: "Basic " + basic("user", "file-password"),
		},
		{
			name:       "custom headers only",
			cfg:        TransportConfig{Headers: map[string]string{"X-Scope-OrgID": "tenant-a"}},
			wantHeader: map[string]string{"X-Scope-OrgID": "tenant-a"},
		},
		{
			name: "tenant header alongside bearer auth",
			cfg: TransportConfig{
				BearerToken: "t",
				Headers:     map[string]string{"X-Scope-OrgID": "tenant-b", "X-Extra": "v"},
			},
			wantAuth:   "Bearer t",
			wantHeader: map[string]string{"X-Scope-OrgID": "tenant-b", "X-Extra": "v"},
		},
		{
			name: "Authorization header alone is allowed",
			cfg: TransportConfig{
				Headers: map[string]string{"Authorization": "Custom abc"},
			},
			wantAuth: "Custom abc",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv := newRecordingServer(t, false)

			c, err := New(srv.URL, WithTransportConfig(tc.cfg))
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			if err := c.Ping(context.Background()); err != nil {
				t.Fatalf("Ping: %v", err)
			}

			if got := srv.header(t, 0, "Authorization"); got != tc.wantAuth {
				t.Errorf("Authorization = %q, want %q", got, tc.wantAuth)
			}
			for name, want := range tc.wantHeader {
				if got := srv.header(t, 0, name); got != want {
					t.Errorf("header %s = %q, want %q", name, got, want)
				}
			}
		})
	}
}

// basic builds the base64 payload of an HTTP basic-auth header, using the
// stdlib's own encoder so the expectation cannot drift from SetBasicAuth.
func basic(user, password string) string {
	r, _ := http.NewRequest(http.MethodGet, "http://example.invalid", nil)
	r.SetBasicAuth(user, password)
	return strings.TrimPrefix(r.Header.Get("Authorization"), "Basic ")
}

// TestBearerTokenFileIsRereadPerRequest is the regression test for the reason
// BearerTokenFile exists at all: kubelet rotates projected service-account
// tokens in place, so a token captured once at construction starts returning
// 401 an hour into the process's life.
func TestBearerTokenFileIsRereadPerRequest(t *testing.T) {
	srv := newRecordingServer(t, false)
	dir := t.TempDir()
	path := filepath.Join(dir, "token")
	if err := os.WriteFile(path, []byte("token-v1\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	c, err := New(srv.URL, WithTransportConfig(TransportConfig{BearerTokenFile: path}))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := c.Ping(context.Background()); err != nil {
		t.Fatalf("first Ping: %v", err)
	}

	// Rotate the token on disk, the way the kubelet does.
	if err := os.WriteFile(path, []byte("token-v2-rotated\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := c.Ping(context.Background()); err != nil {
		t.Fatalf("second Ping: %v", err)
	}

	if srv.count() != 2 {
		t.Fatalf("recorded %d requests, want 2", srv.count())
	}
	if got, want := srv.header(t, 0, "Authorization"), "Bearer token-v1"; got != want {
		t.Errorf("first request Authorization = %q, want %q", got, want)
	}
	if got, want := srv.header(t, 1, "Authorization"), "Bearer token-v2-rotated"; got != want {
		t.Errorf("second request Authorization = %q, want %q (token file not re-read)", got, want)
	}
}

// TestRoundTripDoesNotMutateRequest pins the http.RoundTripper contract: the
// caller's request must come back untouched, both because the interface
// forbids mutation and because a shared Header map written from several
// in-flight queries would race.
func TestRoundTripDoesNotMutateRequest(t *testing.T) {
	srv := newRecordingServer(t, false)

	rt, err := newTransportRoundTripper(TransportConfig{
		BearerToken: "secret",
		Headers:     map[string]string{"X-Scope-OrgID": "tenant-a"},
	})
	if err != nil {
		t.Fatalf("newTransportRoundTripper: %v", err)
	}

	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := rt.RoundTrip(req)
	if err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	defer resp.Body.Close()

	if got := req.Header.Get("Authorization"); got != "" {
		t.Errorf("caller request was mutated: Authorization = %q", got)
	}
	if got := req.Header.Get("X-Scope-OrgID"); got != "" {
		t.Errorf("caller request was mutated: X-Scope-OrgID = %q", got)
	}
	if got := srv.header(t, 0, "Authorization"); got != "Bearer secret" {
		t.Errorf("sent Authorization = %q, want %q", got, "Bearer secret")
	}
}

// TestTransportConfigErrors covers every combination New must refuse rather
// than resolve silently, plus the file/PEM error paths.
func TestTransportConfigErrors(t *testing.T) {
	caFile := writeFile(t, "ca.pem", "not a certificate")
	certFile := writeFile(t, "tls.crt", "garbage")

	tests := []struct {
		name    string
		cfg     TransportConfig
		wantSub string
	}{
		{
			name:    "bearer token and bearer token file",
			cfg:     TransportConfig{BearerToken: "a", BearerTokenFile: "/nope"},
			wantSub: "bearer token and bearer token file are mutually exclusive",
		},
		{
			name:    "basic password and password file",
			cfg:     TransportConfig{BasicAuthUsername: "u", BasicAuthPassword: "p", BasicAuthPasswordFile: "/nope"},
			wantSub: "password and password file are mutually exclusive",
		},
		{
			name:    "bearer and basic together",
			cfg:     TransportConfig{BearerToken: "a", BasicAuthUsername: "u"},
			wantSub: "bearer token and basic auth are mutually exclusive",
		},
		{
			name:    "password without username",
			cfg:     TransportConfig{BasicAuthPassword: "p"},
			wantSub: "password set without a username",
		},
		{
			name:    "empty header name",
			cfg:     TransportConfig{Headers: map[string]string{"  ": "v"}},
			wantSub: "empty header name",
		},
		{
			name:    "header name with a space",
			cfg:     TransportConfig{Headers: map[string]string{"X Bad Name": "v"}},
			wantSub: "invalid header name",
		},
		{
			name:    "header value with CRLF",
			cfg:     TransportConfig{Headers: map[string]string{"X-Scope-OrgID": "a\r\nInjected: b"}},
			wantSub: "invalid value for header",
		},
		{
			name: "Authorization header conflicts with bearer",
			cfg: TransportConfig{
				BearerToken: "a",
				Headers:     map[string]string{"authorization": "Custom x"},
			},
			wantSub: "conflicts with the configured bearer/basic auth",
		},
		{
			name: "Authorization header conflicts with basic",
			cfg: TransportConfig{
				BasicAuthUsername: "u",
				Headers:           map[string]string{"Authorization": "Custom x"},
			},
			wantSub: "conflicts with the configured bearer/basic auth",
		},
		{
			name:    "cert without key",
			cfg:     TransportConfig{TLS: TLSConfig{CertFile: "/a.crt"}},
			wantSub: "cert file and key file must be set together",
		},
		{
			name:    "key without cert",
			cfg:     TransportConfig{TLS: TLSConfig{KeyFile: "/a.key"}},
			wantSub: "cert file and key file must be set together",
		},
		{
			name:    "missing bearer token file",
			cfg:     TransportConfig{BearerTokenFile: "/definitely/not/here"},
			wantSub: "reading prometheus bearer token file",
		},
		{
			name:    "missing basic auth password file",
			cfg:     TransportConfig{BasicAuthUsername: "u", BasicAuthPasswordFile: "/definitely/not/here"},
			wantSub: "reading prometheus basic auth password file",
		},
		{
			name:    "missing CA file",
			cfg:     TransportConfig{TLS: TLSConfig{CAFile: "/definitely/not/here"}},
			wantSub: "reading prometheus TLS CA file",
		},
		{
			name:    "CA file with no PEM certificate",
			cfg:     TransportConfig{TLS: TLSConfig{CAFile: caFile}},
			wantSub: "contains no valid PEM certificate",
		},
		{
			name:    "unusable key pair",
			cfg:     TransportConfig{TLS: TLSConfig{CertFile: certFile, KeyFile: certFile}},
			wantSub: "loading prometheus TLS key pair",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c, err := New("http://localhost:9090", WithTransportConfig(tc.cfg))
			if err == nil {
				t.Fatalf("New succeeded, want error containing %q (client=%v)", tc.wantSub, c)
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Fatalf("error = %v, want it to contain %q", err, tc.wantSub)
			}
			if !strings.HasPrefix(err.Error(), "creating prometheus client: ") {
				t.Errorf("error %v is not wrapped by New's own prefix", err)
			}
		})
	}
}

// TestNewWithoutOptionsSendsNoAuth pins the compatibility guarantee: every
// pre-existing caller of New(addr) must keep talking plain, unauthenticated
// HTTP with the api package's own default transport.
func TestNewWithoutOptionsSendsNoAuth(t *testing.T) {
	srv := newRecordingServer(t, false)

	c, err := New(srv.URL)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if c.roundTripper != nil || c.transportSet {
		t.Fatalf("New(addr) recorded a transport: rt=%v set=%v", c.roundTripper, c.transportSet)
	}
	if err := c.Ping(context.Background()); err != nil {
		t.Fatalf("Ping: %v", err)
	}
	if got := srv.header(t, 0, "Authorization"); got != "" {
		t.Errorf("unauthenticated client sent Authorization = %q", got)
	}
}

// TestEmptyTransportConfigInstallsNoRoundTripper ensures the "option present
// but empty" case (a chart rendering all the auth flags as empty strings)
// still resolves to the plain default transport.
func TestEmptyTransportConfigInstallsNoRoundTripper(t *testing.T) {
	c := &Client{}
	WithTransportConfig(TransportConfig{})(c)
	rt, err := c.resolveRoundTripper()
	if err != nil {
		t.Fatalf("resolveRoundTripper: %v", err)
	}
	if rt != nil {
		t.Fatalf("empty TransportConfig produced RoundTripper %T, want nil", rt)
	}
}

// TestWithRoundTripperIsUsed covers the escape hatch, and
// TestWithRoundTripperConflictsWithTransportConfig covers refusing to silently
// drop one of two transports.
func TestWithRoundTripperIsUsed(t *testing.T) {
	srv := newRecordingServer(t, false)
	stub := roundTripperFunc(func(r *http.Request) (*http.Response, error) {
		r = r.Clone(r.Context())
		r.Header.Set("X-Stub", "yes")
		return http.DefaultTransport.RoundTrip(r)
	})

	c, err := New(srv.URL, WithRoundTripper(stub))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := c.Ping(context.Background()); err != nil {
		t.Fatalf("Ping: %v", err)
	}
	if got := srv.header(t, 0, "X-Stub"); got != "yes" {
		t.Errorf("X-Stub = %q, want %q (explicit RoundTripper not used)", got, "yes")
	}
}

func TestWithRoundTripperConflictsWithTransportConfig(t *testing.T) {
	_, err := New("http://localhost:9090",
		WithRoundTripper(http.DefaultTransport),
		WithTransportConfig(TransportConfig{BearerToken: "x"}),
	)
	if err == nil {
		t.Fatal("New succeeded, want a mutual-exclusion error")
	}
	if !strings.Contains(err.Error(), "mutually exclusive") {
		t.Fatalf("error = %v, want a mutual-exclusion error", err)
	}
}

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// TestTLSCAFileVerifiesServer exercises the happy TLS path end to end: the
// httptest server's own certificate, written out as a CA bundle, must be
// enough for the client to trust it — proving CAFile is actually installed on
// the transport and not silently dropped.
func TestTLSCAFileVerifiesServer(t *testing.T) {
	srv := newRecordingServer(t, true)

	caPath := writeFile(t, "ca.pem", string(pem.EncodeToMemory(&pem.Block{
		Type:  "CERTIFICATE",
		Bytes: srv.Certificate().Raw,
	})))

	c, err := New(srv.URL, WithTransportConfig(TransportConfig{
		BearerToken: "tls-token",
		TLS:         TLSConfig{CAFile: caPath, ServerName: "example.com"},
	}))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := c.Ping(context.Background()); err != nil {
		t.Fatalf("Ping over TLS with the configured CA: %v", err)
	}
	if got := srv.header(t, 0, "Authorization"); got != "Bearer tls-token" {
		t.Errorf("Authorization = %q, want %q", got, "Bearer tls-token")
	}
}

// TestTLSWithoutCAFileFailsVerification is the negative control for the test
// above: without the CA the same server must NOT be trusted, which is what
// makes the positive result meaningful.
func TestTLSWithoutCAFileFailsVerification(t *testing.T) {
	srv := newRecordingServer(t, true)

	c, err := New(srv.URL, WithTransportConfig(TransportConfig{BearerToken: "x"}))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := c.Ping(context.Background()); err == nil {
		t.Fatal("Ping succeeded against an untrusted TLS server, want a verification failure")
	}
}

// TestTLSInsecureSkipVerify pins that the escape hatch actually works — an
// operator who sets it has to get a working connection, not a silent no-op.
func TestTLSInsecureSkipVerify(t *testing.T) {
	srv := newRecordingServer(t, true)

	c, err := New(srv.URL, WithTransportConfig(TransportConfig{
		TLS: TLSConfig{InsecureSkipVerify: true},
	}))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := c.Ping(context.Background()); err != nil {
		t.Fatalf("Ping with insecure-skip-verify: %v", err)
	}
}

// TestBaseRoundTripperDoesNotMutateDefault guards the shared package-level
// api.DefaultRoundTripper, which every other client_golang consumer in the
// process uses: applying TLS settings must clone it, never write through it.
func TestBaseRoundTripperDoesNotMutateDefault(t *testing.T) {
	def, ok := api.DefaultRoundTripper.(*http.Transport)
	if !ok {
		t.Skip("api.DefaultRoundTripper is not an *http.Transport")
	}
	before := def.TLSClientConfig

	if _, err := baseRoundTripper(TransportConfig{TLS: TLSConfig{InsecureSkipVerify: true}}); err != nil {
		t.Fatalf("baseRoundTripper: %v", err)
	}
	if def.TLSClientConfig != before {
		t.Fatal("baseRoundTripper mutated the shared api.DefaultRoundTripper")
	}
}

// TestReadSecretFileTrimsWhitespace documents why the trim exists: Secret
// mounts and `echo > token` both leave a trailing newline that would be sent
// as part of the credential.
func TestReadSecretFileTrimsWhitespace(t *testing.T) {
	path := writeFile(t, "secret", "\n  value  \n")
	got, err := readSecretFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if got != "value" {
		t.Fatalf("readSecretFile = %q, want %q", got, "value")
	}
}

// TestRoundTripSurfacesTokenFileErrors covers the file disappearing AFTER
// construction (a Secret unmounted mid-flight): the request must fail loudly
// rather than silently going out unauthenticated.
func TestRoundTripSurfacesTokenFileErrors(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "token")
	if err := os.WriteFile(path, []byte("t"), 0o600); err != nil {
		t.Fatal(err)
	}
	rt, err := newTransportRoundTripper(TransportConfig{BearerTokenFile: path})
	if err != nil {
		t.Fatalf("newTransportRoundTripper: %v", err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}

	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, "http://localhost:1", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := rt.RoundTrip(req)
	if err == nil {
		resp.Body.Close()
		t.Fatal("RoundTrip succeeded with a missing token file, want an error")
	}
	if !strings.Contains(err.Error(), "reading prometheus bearer token file") {
		t.Fatalf("error = %v, want it to name the token file", err)
	}
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("error = %v, want it to wrap os.ErrNotExist", err)
	}
}

// TestHeaderValuesMayContainCommasAndQuotes pins that the transport itself
// imposes no CSV-style restriction on header values: the flag layer is what
// used to split on commas, and it no longer does (StringArray).
func TestHeaderValuesMayContainCommasAndQuotes(t *testing.T) {
	srv := newRecordingServer(t, false)

	c, err := New(srv.URL, WithTransportConfig(TransportConfig{
		Headers: map[string]string{"Accept": `application/json,text/plain; q="0.5"`},
	}))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := c.Ping(context.Background()); err != nil {
		t.Fatalf("Ping: %v", err)
	}
	if got, want := srv.header(t, 0, "Accept"), `application/json,text/plain; q="0.5"`; got != want {
		t.Errorf("Accept = %q, want %q", got, want)
	}
}

// TestClientCertificateIsReloadedPerHandshake pins the rotation property for
// mTLS: the key pair named by CertFile/KeyFile is read again on every TLS
// handshake, so a certificate rotated in place (cert-manager renewing a
// Secret, a mesh sidecar refreshing its pair) is used without a restart.
func TestClientCertificateIsReloadedPerHandshake(t *testing.T) {
	certPEM1, keyPEM1 := selfSignedPair(t, "first")
	certPEM2, keyPEM2 := selfSignedPair(t, "second")
	certPath := writeFile(t, "tls.crt", certPEM1)
	keyPath := writeFile(t, "tls.key", keyPEM1)

	tlsCfg, err := buildTLSConfig(TLSConfig{CertFile: certPath, KeyFile: keyPath})
	if err != nil {
		t.Fatalf("buildTLSConfig: %v", err)
	}
	if tlsCfg.GetClientCertificate == nil {
		t.Fatal("GetClientCertificate is nil: the key pair would be loaded once and never rotated")
	}
	if len(tlsCfg.Certificates) != 0 {
		t.Fatalf("Certificates = %d entries, want 0: a static entry would shadow the per-handshake reload", len(tlsCfg.Certificates))
	}

	first, err := tlsCfg.GetClientCertificate(&tls.CertificateRequestInfo{})
	if err != nil {
		t.Fatalf("GetClientCertificate (before rotation): %v", err)
	}

	// Rotate in place: same paths, new material.
	if err := os.WriteFile(certPath, []byte(certPEM2), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, []byte(keyPEM2), 0o600); err != nil {
		t.Fatal(err)
	}

	second, err := tlsCfg.GetClientCertificate(&tls.CertificateRequestInfo{})
	if err != nil {
		t.Fatalf("GetClientCertificate (after rotation): %v", err)
	}
	if bytes.Equal(first.Certificate[0], second.Certificate[0]) {
		t.Fatal("client certificate did not change after the files were rotated")
	}
	if got := commonName(t, second); got != "second" {
		t.Errorf("CN after rotation = %q, want %q", got, "second")
	}

	// A broken rotation must surface as a handshake error naming the files,
	// not a silent fallback to stale material.
	if err := os.WriteFile(keyPath, []byte("garbage"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := tlsCfg.GetClientCertificate(&tls.CertificateRequestInfo{}); err == nil {
		t.Fatal("GetClientCertificate succeeded with a corrupt key file, want an error")
	} else if !strings.Contains(err.Error(), "loading prometheus TLS key pair") {
		t.Errorf("error = %v, want it to name the key pair files", err)
	}
}

// selfSignedPair returns a PEM-encoded self-signed certificate and key with
// the given common name.
func selfSignedPair(t *testing.T, cn string) (certPEM, keyPEM string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: cn},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	certPEM = string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
	keyPEM = string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}))
	return certPEM, keyPEM
}

func commonName(t *testing.T, c *tls.Certificate) string {
	t.Helper()
	leaf, err := x509.ParseCertificate(c.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	return leaf.Subject.CommonName
}
