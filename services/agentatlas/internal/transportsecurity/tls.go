// Package transportsecurity is AgentAtlas's own hot-reloadable wiring
// around the shared sdk/go/transportsecurity profile: it loads a link's
// leaf certificate, trust bundle, and revocation list from disk, and builds
// client/server *tls.Config values that observe reload/rotation on every
// new handshake without dropping already-open connections or requiring the
// caller to rebuild anything.
//
// A Manager represents ONE named link's ONE pairwise relationship (e.g.
// AgentAtlas's outbound identity for the AgentNexus link, or AgentAtlas's
// own inbound server identity) — LinkConfig.ServerName/SPIFFEID always mean
// "the identity I expect from my peer on this link", used as the expected
// CLIENT identity when this Manager builds a server config and as the
// expected SERVER identity when it builds a client config.
package transportsecurity

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	sdktls "github.com/astraclawteam/agentatlas/sdk/go/transportsecurity"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/config"
)

// ErrIdentityRequired is the named fail-closed startup error for a link that
// enables TLS (Mode != ModeOff) without pinning ANY expected peer identity
// (both ServerName and SPIFFEID empty). Per-link identity pinning is the
// premise of GA Task 13A: a link with no expected identity would accept ANY
// peer whose certificate merely chains to the trust bundle, silently
// defeating the whole point. NewManager fails closed on this rather than
// serving a permissive link.
var ErrIdentityRequired = errors.New("transportsecurity: TLS is enabled for this link but no expected peer identity is configured (set server_name and/or spiffe_id) — refusing to accept any chain-verified peer")

// FromLinkTLS converts a config.LinkTLS value into a LinkConfig — the one
// place cmd/*/main.go composition roots go from the parsed YAML/env config
// surface (package config, which does not depend on this package) to this
// package's runtime type.
func FromLinkTLS(l config.LinkTLS) LinkConfig {
	return LinkConfig{
		Mode:            Mode(l.Mode),
		CertFile:        l.CertFile,
		KeyFile:         l.KeyFile,
		TrustBundleFile: l.TrustBundleFile,
		RevocationFile:  l.RevocationFile,
		ServerName:      l.ServerName,
		SPIFFEID:        l.SPIFFEID,
	}
}

// Mode selects how a link enforces transport security.
type Mode string

const (
	// ModeOff disables TLS entirely for the link (plaintext; the
	// backward-compatible default for every existing deployment).
	ModeOff Mode = "off"
	// ModeTLS is server-auth-only TLS: the server presents a certificate,
	// the client verifies it, but no client certificate is required or
	// presented.
	ModeTLS Mode = "tls"
	// ModeMTLS is mutual TLS: both sides present and verify a certificate.
	// Every named AgentAtlas link runs in this mode.
	ModeMTLS Mode = "mtls"
)

// LinkConfig is one link's file-based transport-security configuration. It
// mirrors config.LinkTLS field-for-field so cmd/*/main.go can pass
// cfg.TLS.<Link> straight through without an import cycle back to package
// config.
type LinkConfig struct {
	Mode Mode
	// CertFile/KeyFile are this side's own leaf certificate and private
	// key (PEM). Required whenever Mode != ModeOff.
	CertFile string
	KeyFile  string
	// TrustBundleFile is one or more concatenated PEM CA certificates used
	// to verify the peer. Required whenever Mode != ModeOff (even
	// server-auth-only clients must verify the server).
	TrustBundleFile string
	// RevocationFile is optional: a plain text file, one hex-encoded
	// certificate serial number per line (blank lines and lines starting
	// with '#' are ignored), naming peer identities that must be rejected
	// even though they still chain-verify. Production deployments
	// generate this file from the enterprise PKI's exported CRL via an
	// operational sync step (Task 16A's cross-deployment concern; out of
	// scope here) or from cmd/atlasctl's `certificates export-crl`.
	RevocationFile string
	// ServerName / SPIFFEID together describe the identity THIS Manager
	// expects from its peer on this link (see package doc).
	ServerName string
	SPIFFEID   string
}

func (c LinkConfig) peerIdentity() sdktls.PeerIdentity {
	return sdktls.PeerIdentity{SPIFFEID: c.SPIFFEID, DNSName: c.ServerName}
}

// Status reports a Manager's hot-reloadable health WITHOUT ever including
// key material — safe to embed directly in a /healthz response so a probe
// can distinguish "my certificate is bad" from "a dependency is down".
type Status struct {
	Link     string    `json:"link"`
	Mode     Mode      `json:"mode"`
	Ready    bool      `json:"ready"`
	NotAfter time.Time `json:"not_after,omitempty"`
	Detail   string    `json:"detail,omitempty"`
}

// Manager owns one link's hot-reloadable identity, trust bundle, and
// revoked-serial set. All exported accessors are safe for concurrent use;
// Reload() atomically swaps in newly-loaded material, or — on error —
// leaves the previously-loaded (last-known-good) material in place so
// serving continues uninterrupted through a bad reload (overlapping
// rotation).
type Manager struct {
	link string
	cfg  LinkConfig

	mu      sync.RWMutex
	cert    *tls.Certificate
	leaf    *x509.Certificate
	roots   *x509.CertPool
	revoked map[string]struct{}
	ready   bool
	detail  string
}

// NewManager loads cert/key/trust-bundle/revocation material for link from
// the paths in cfg. When cfg.Mode == ModeOff, no files are read and no
// error is ever returned (TLS disabled is always a valid, backward
// compatible posture). Otherwise this is the fail-closed startup contract:
// any missing or invalid required file produces a named, sanitized error
// (never containing key material) and NewManager returns nil.
func NewManager(link string, cfg LinkConfig) (*Manager, error) {
	m := &Manager{link: link, cfg: cfg, revoked: map[string]struct{}{}}
	if cfg.Mode == ModeOff || cfg.Mode == "" {
		m.cfg.Mode = ModeOff
		return m, nil
	}
	if cfg.Mode != ModeTLS && cfg.Mode != ModeMTLS {
		return nil, fmt.Errorf("transportsecurity[%s]: unknown mode %q", link, cfg.Mode)
	}
	// Fail closed on an identity-less TLS link BEFORE loading any material:
	// a link with no pinned peer identity would accept any chain-verified
	// peer, defeating per-link pinning (GA Task 13A's premise).
	if cfg.peerIdentity().Empty() {
		return nil, fmt.Errorf("transportsecurity[%s]: %w", link, ErrIdentityRequired)
	}
	if err := m.Reload(); err != nil {
		// MINOR-4 (post-quality-review): sanitize the RETURNED startup error
		// too, for symmetry with Status().Detail — underlying loader errors
		// carry no key bytes today, but keep the "never surface material"
		// invariant uniform across both surfaces. (Uses %s, not %w: the
		// sanitized form is a string; nothing calls errors.Is on the
		// fail-closed startup error, which is terminal.)
		return nil, fmt.Errorf("transportsecurity[%s]: fail-closed startup: %s", link, sanitizeError(err))
	}
	return m, nil
}

// Reload re-reads cert/key/trust-bundle/revocation from the configured
// paths and atomically swaps them in on success. On failure the Manager
// keeps serving the last-known-good material (if any) — the overlapping
// rotation contract — and Status().Detail records the sanitized failure;
// Reload also returns that error so callers driving reload on a timer/signal
// can log/alert.
func (m *Manager) Reload() error {
	if m.cfg.Mode == ModeOff {
		return nil
	}
	cert, leaf, err := loadKeyPair(m.link, m.cfg.CertFile, m.cfg.KeyFile)
	if err != nil {
		m.recordFailure(err)
		return err
	}
	roots, err := loadTrustBundle(m.link, m.cfg.TrustBundleFile)
	if err != nil {
		m.recordFailure(err)
		return err
	}
	revoked, err := loadRevocationFile(m.cfg.RevocationFile)
	if err != nil {
		m.recordFailure(fmt.Errorf("load revocation file: %w", err))
		return err
	}

	m.mu.Lock()
	m.cert = cert
	m.leaf = leaf
	m.roots = roots
	m.revoked = revoked
	m.ready = true
	m.detail = ""
	m.mu.Unlock()
	return nil
}

func (m *Manager) recordFailure(err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	// Sanitize defensively: stdlib parse errors never include PEM/key
	// bytes, but strip anything that looks like it anyway before it ever
	// reaches Status()/logs.
	m.detail = sanitizeError(err)
	// Ready reflects whether THIS Manager has ANY usable material loaded
	// (last-known-good), not whether the most recent reload succeeded.
	m.ready = m.cert != nil
}

func sanitizeError(err error) string {
	msg := err.Error()
	if strings.Contains(msg, "PRIVATE KEY") || strings.Contains(msg, "BEGIN CERTIFICATE") {
		return "certificate material failed validation (detail suppressed to avoid leaking key material)"
	}
	return msg
}

func loadKeyPair(link, certFile, keyFile string) (*tls.Certificate, *x509.Certificate, error) {
	if strings.TrimSpace(certFile) == "" || strings.TrimSpace(keyFile) == "" {
		return nil, nil, fmt.Errorf("cert_file and key_file are both required")
	}
	if err := checkSecretFilePerms(certFile); err != nil {
		return nil, nil, fmt.Errorf("cert_file: %w", err)
	}
	if err := checkSecretFilePerms(keyFile); err != nil {
		return nil, nil, fmt.Errorf("key_file: %w", err)
	}
	certPEM, err := os.ReadFile(certFile)
	if err != nil {
		return nil, nil, fmt.Errorf("read cert_file: %w", err)
	}
	keyPEM, err := os.ReadFile(keyFile)
	if err != nil {
		return nil, nil, fmt.Errorf("read key_file: %w", err)
	}
	pair, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return nil, nil, fmt.Errorf("load key pair: %w", err)
	}
	leaf := pair.Leaf
	if leaf == nil {
		leaf, err = x509.ParseCertificate(pair.Certificate[0])
		if err != nil {
			return nil, nil, fmt.Errorf("parse leaf certificate: %w", err)
		}
	}
	_ = link
	return &pair, leaf, nil
}

func loadTrustBundle(link, trustFile string) (*x509.CertPool, error) {
	if strings.TrimSpace(trustFile) == "" {
		return nil, fmt.Errorf("trust_bundle_file is required")
	}
	if err := checkSecretFilePerms(trustFile); err != nil {
		return nil, fmt.Errorf("trust_bundle_file: %w", err)
	}
	raw, err := os.ReadFile(trustFile)
	if err != nil {
		return nil, fmt.Errorf("read trust_bundle_file: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(raw) {
		return nil, fmt.Errorf("trust_bundle_file contains no usable CA certificates")
	}
	_ = link
	return pool, nil
}

// loadRevocationFile parses a plain-text revoked-serial list: one
// hex-encoded serial number per line, blank lines and '#' comments
// ignored. An empty/unset path is valid (no revocation checking).
func loadRevocationFile(path string) (map[string]struct{}, error) {
	out := map[string]struct{}{}
	if strings.TrimSpace(path) == "" {
		return out, nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return out, nil // optional file: absence is not an error
		}
		return nil, err
	}
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		out[strings.ToLower(line)] = struct{}{}
	}
	return out, nil
}

// checkSecretFilePerms mirrors internal/nexusclient's service-secret
// loading discipline: the path must be absolute and canonical, must name a
// regular (non-symlink) file, and — off Windows, where POSIX permission
// bits are not meaningful — must not be group/world accessible.
func checkSecretFilePerms(path string) error {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return fmt.Errorf("path must be canonical and absolute")
	}
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("must be a regular non-symlink file")
	}
	if runtime.GOOS != "windows" && info.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("file permissions are too broad")
	}
	return nil
}

// GetCertificate implements the server-side tls.Config.GetCertificate hook:
// called per handshake, so rotation via Reload() is observed immediately by
// every NEW connection.
func (m *Manager) GetCertificate(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.cert == nil {
		return nil, fmt.Errorf("transportsecurity[%s]: no certificate loaded", m.link)
	}
	return m.cert, nil
}

// GetClientCertificate implements the client-side
// tls.Config.GetClientCertificate hook, same rotation guarantee.
func (m *Manager) GetClientCertificate(*tls.CertificateRequestInfo) (*tls.Certificate, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.cert == nil {
		return nil, fmt.Errorf("transportsecurity[%s]: no certificate loaded", m.link)
	}
	return m.cert, nil
}

// Roots implements sdktls.RootsFunc: it reads the CURRENT trust bundle on
// every call, which is what lets Reload() rotate trust live.
func (m *Manager) Roots() *x509.CertPool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.roots
}

// IsRevoked implements sdktls.RevokedFunc against the current revocation
// set.
func (m *Manager) IsRevoked(cert *x509.Certificate) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if len(m.revoked) == 0 {
		return false
	}
	_, ok := m.revoked[strings.ToLower(fmt.Sprintf("%x", cert.SerialNumber))]
	return ok
}

// SetRevokedSerials merges additional hex-encoded serial numbers into the
// in-memory revoked set without touching disk — an ops/test hook alongside
// the file-based RevocationFile + Reload() path.
func (m *Manager) SetRevokedSerials(serials ...string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.revoked == nil {
		m.revoked = map[string]struct{}{}
	}
	for _, s := range serials {
		m.revoked[strings.ToLower(s)] = struct{}{}
	}
}

// Mode reports the link's configured mode.
func (m *Manager) Mode() Mode { return m.cfg.Mode }

// ServerTLSConfig builds this link's accept-side *tls.Config: mTLS
// (RequireAnyClientCert + manual verification) when Mode == ModeMTLS,
// server-auth-only TLS when Mode == ModeTLS.
func (m *Manager) ServerTLSConfig() (*tls.Config, error) {
	if m.cfg.Mode == ModeOff {
		return nil, fmt.Errorf("transportsecurity[%s]: TLS is disabled for this link", m.link)
	}
	return sdktls.ServerConfig{
		GetCertificate:         m.GetCertificate,
		Roots:                  m.Roots,
		Revoked:                m.IsRevoked,
		RequireClientCert:      m.cfg.Mode == ModeMTLS,
		ExpectedClientIdentity: m.cfg.peerIdentity(),
	}.Build()
}

// ClientTLSConfig builds this link's dial-side *tls.Config, pinning the
// expected peer identity and presenting this Manager's own certificate only
// when Mode == ModeMTLS.
func (m *Manager) ClientTLSConfig() (*tls.Config, error) {
	if m.cfg.Mode == ModeOff {
		return nil, fmt.Errorf("transportsecurity[%s]: TLS is disabled for this link", m.link)
	}
	var getClientCert func(*tls.CertificateRequestInfo) (*tls.Certificate, error)
	if m.cfg.Mode == ModeMTLS {
		getClientCert = m.GetClientCertificate
	}
	return sdktls.ClientConfig{
		GetClientCertificate:   getClientCert,
		Roots:                  m.Roots,
		Revoked:                m.IsRevoked,
		ServerName:             m.cfg.ServerName,
		ExpectedServerIdentity: m.cfg.peerIdentity(),
	}.Build()
}

// ClientTLSConfigOrNil returns ClientTLSConfig(), or (nil, nil) when this
// link's Mode is ModeOff — the nil-safe form for callers that take a
// *tls.Config directly rather than an *http.Transport (pgxpool's
// ConnConfig.TLSConfig, nats.go's nats.Secure, minio's low-level TLS
// field). Callers can therefore call this unconditionally and only apply
// the result when non-nil, matching ConfigureTransport's/WrapListener's
// "always safe to call" contract for http.Transport/net.Listener callers.
func (m *Manager) ClientTLSConfigOrNil() (*tls.Config, error) {
	if m.cfg.Mode == ModeOff {
		return nil, nil
	}
	return m.ClientTLSConfig()
}

// Status reports current health without leaking key material.
func (m *Manager) Status() Status {
	m.mu.RLock()
	defer m.mu.RUnlock()
	st := Status{Link: m.link, Mode: m.cfg.Mode, Detail: m.detail}
	if m.cfg.Mode == ModeOff {
		st.Ready = true
		return st
	}
	st.Ready = m.ready
	if m.leaf != nil {
		st.NotAfter = m.leaf.NotAfter
	}
	return st
}

// WrapListener wraps inner in a TLS listener built from ServerTLSConfig
// when Mode != ModeOff; when TLS is disabled it returns inner unchanged, so
// cmd/*/main.go can call this unconditionally.
func (m *Manager) WrapListener(inner net.Listener) (net.Listener, error) {
	if m.cfg.Mode == ModeOff {
		return inner, nil
	}
	cfg, err := m.ServerTLSConfig()
	if err != nil {
		return nil, err
	}
	return tls.NewListener(inner, cfg), nil
}

// ConfigureTransport sets t.TLSClientConfig from ClientTLSConfig() when
// Mode != ModeOff; a no-op when TLS is disabled, so callers can call this
// unconditionally when composing an *http.Client.
func (m *Manager) ConfigureTransport(t *http.Transport) error {
	if m.cfg.Mode == ModeOff {
		return nil
	}
	cfg, err := m.ClientTLSConfig()
	if err != nil {
		return err
	}
	t.TLSClientConfig = cfg
	return nil
}
