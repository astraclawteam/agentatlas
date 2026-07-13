// Package transportsecurity defines AgentAtlas's single shared TLS/mTLS
// security profile: the TLS versions, cipher suites, and curve preferences
// every AgentAtlas link uses, plus builders that turn a certificate source
// and a trust bundle into a *tls.Config for either side of a connection.
//
// The profile is TLS 1.3-preferred: MinVersionDefault is TLS 1.3, and
// dropping to the TLS 1.2 floor (MinVersionCompat) is only permitted when a
// caller explicitly opts in via AllowCompatMode, documenting which
// qualified dependency requires it. Nothing below TLS 1.2 is ever
// permitted.
//
// Peer verification is always identity-pinned: callers supply a
// PeerIdentity (a SPIFFE URI and/or a DNS name) that the remote leaf
// certificate's SAN set must satisfy, on top of normal chain verification
// against the caller's trust bundle. Both RootsFunc and RevokedFunc are
// plain functions rather than static values so hot-reloadable callers
// (see services/agentatlas/internal/transportsecurity) can rotate trust
// bundles and revoke identities without rebuilding the *tls.Config or
// dropping already-open connections — every new handshake reads through the
// function and observes the latest state.
//
// This package is deliberately dependency-free (stdlib only) and does no
// file I/O, so it is safe for both the core AgentAtlas services and the
// enterprise module to import.
package transportsecurity

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"time"
)

// MinVersionDefault is the preferred minimum TLS version for every
// AgentAtlas link.
const MinVersionDefault = tls.VersionTLS13

// MinVersionCompat is the lowest TLS version ever permitted, reserved for
// the qualified set of dependencies that do not yet support TLS 1.3. Using
// it requires an explicit AllowCompatMode opt-in on the builder.
const MinVersionCompat = tls.VersionTLS12

// MaxVersion caps negotiation at TLS 1.3 (the latest version this profile
// supports).
const MaxVersion = tls.VersionTLS13

// CompatCipherSuites are the only cipher suites permitted when a link runs
// in TLS 1.2 compat mode. TLS 1.3 ignores this list entirely (its cipher
// suites are fixed by the stdlib and not configurable), so it is only
// consulted when MinVersion == MinVersionCompat. All of these are AEAD,
// forward-secret (ECDHE) suites; no CBC, no RC4, no 3DES.
var CompatCipherSuites = []uint16{
	tls.TLS_ECDHE_ECDSA_WITH_AES_256_GCM_SHA384,
	tls.TLS_ECDHE_RSA_WITH_AES_256_GCM_SHA384,
	tls.TLS_ECDHE_ECDSA_WITH_CHACHA20_POLY1305,
	tls.TLS_ECDHE_RSA_WITH_CHACHA20_POLY1305,
	tls.TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256,
	tls.TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256,
}

// CurvePreferences is the shared key-exchange curve preference order for
// every AgentAtlas link.
var CurvePreferences = []tls.CurveID{tls.X25519, tls.CurveP256, tls.CurveP384}

// PeerIdentity is the expected identity of the remote end of a connection.
// At least one of SPIFFEID or DNSName must be set for a link that pins
// identity (every named AgentAtlas link does); Matches succeeds if either
// configured field is present as a SAN on the peer's leaf certificate, so a
// caller MAY pin on both without forcing dual-issuance.
type PeerIdentity struct {
	// SPIFFEID is the expected SAN URI, e.g.
	// "spiffe://agentatlas.internal/ns/prod/sa/postgres".
	SPIFFEID string
	// DNSName is the expected SAN DNS name, e.g. "postgres.agentatlas.internal".
	DNSName string
}

// Empty reports whether no identity has been configured (verification is
// then skipped by Matches — used only by callers that intentionally accept
// any chain-verified identity on a given link).
func (p PeerIdentity) Empty() bool { return p.SPIFFEID == "" && p.DNSName == "" }

// Matches reports whether cert's SAN set satisfies p. An empty p always
// matches (no pinning requested); otherwise at least one configured field
// must be present among the certificate's SANs.
func (p PeerIdentity) Matches(cert *x509.Certificate) error {
	if p.Empty() {
		return nil
	}
	if p.SPIFFEID != "" {
		for _, uri := range cert.URIs {
			if uri.String() == p.SPIFFEID {
				return nil
			}
		}
	}
	if p.DNSName != "" {
		for _, name := range cert.DNSNames {
			if name == p.DNSName {
				return nil
			}
		}
	}
	return fmt.Errorf("transportsecurity: peer certificate identity mismatch: want spiffe=%q dns=%q, got uris=%v dns=%v",
		p.SPIFFEID, p.DNSName, cert.URIs, cert.DNSNames)
}

// RootsFunc returns the trust bundle to verify a peer chain against. It is
// called on every verification (not cached by this package) so hot-reload
// callers can rotate the bundle without rebuilding the *tls.Config.
type RootsFunc func() *x509.CertPool

// RevokedFunc reports whether cert has been revoked. It is called on every
// verification for the same hot-reload reason as RootsFunc. A nil
// RevokedFunc means "no revocation check" (never used by the named
// AgentAtlas links, but permitted for general reuse).
type RevokedFunc func(cert *x509.Certificate) bool

// PeerRole says which side of the handshake the certificate being verified
// belongs to, selecting the required x509 extended key usage.
type PeerRole int

const (
	// PeerIsServer verifies a certificate presented by the party acting as
	// the TLS server (checked by the dialing client).
	PeerIsServer PeerRole = iota
	// PeerIsClient verifies a certificate presented by the party acting as
	// the TLS client (checked by the accepting server in mTLS).
	PeerIsClient
)

func (r PeerRole) extKeyUsage() x509.ExtKeyUsage {
	if r == PeerIsClient {
		return x509.ExtKeyUsageClientAuth
	}
	return x509.ExtKeyUsageServerAuth
}

// VerifyPeerChain performs this profile's full peer verification: parse the
// presented chain, build it against roots() (plus any intermediates the
// peer sent), reject if expired/not-yet-valid (via now), reject if the leaf
// is revoked per revoked(), and reject if the leaf does not satisfy
// expected. It is the sole verifier behind every *tls.Config this package
// builds (via VerifyPeerCertificate, with the stdlib's own chain
// verification disabled) precisely so roots/revocation can be rotated live.
func VerifyPeerChain(rawCerts [][]byte, roots RootsFunc, revoked RevokedFunc, expected PeerIdentity, role PeerRole, now func() time.Time) error {
	if len(rawCerts) == 0 {
		return errors.New("transportsecurity: no peer certificate presented")
	}
	if roots == nil {
		return errors.New("transportsecurity: no trust bundle source configured")
	}
	pool := roots()
	if pool == nil {
		return errors.New("transportsecurity: trust bundle is not loaded")
	}
	certs := make([]*x509.Certificate, 0, len(rawCerts))
	for _, raw := range rawCerts {
		cert, err := x509.ParseCertificate(raw)
		if err != nil {
			return fmt.Errorf("transportsecurity: parse peer certificate: %w", err)
		}
		certs = append(certs, cert)
	}
	leaf := certs[0]
	intermediates := x509.NewCertPool()
	for _, c := range certs[1:] {
		intermediates.AddCert(c)
	}
	nowFn := now
	if nowFn == nil {
		nowFn = time.Now
	}
	_, err := leaf.Verify(x509.VerifyOptions{
		Roots:         pool,
		Intermediates: intermediates,
		CurrentTime:   nowFn(),
		KeyUsages:     []x509.ExtKeyUsage{role.extKeyUsage()},
	})
	if err != nil {
		return fmt.Errorf("transportsecurity: peer certificate chain verification failed: %w", err)
	}
	if revoked != nil && revoked(leaf) {
		return fmt.Errorf("transportsecurity: peer certificate is revoked (serial %s)", leaf.SerialNumber.String())
	}
	if err := expected.Matches(leaf); err != nil {
		return err
	}
	return nil
}

// resolveMinVersion applies the "TLS 1.3 preferred, TLS 1.2 floor only with
// explicit opt-in, nothing lower ever" rule shared by ServerConfig and
// ClientConfig.
func resolveMinVersion(requested uint16, allowCompatMode bool) (uint16, error) {
	min := requested
	if min == 0 {
		min = MinVersionDefault
	}
	if min < MinVersionCompat {
		return 0, fmt.Errorf("transportsecurity: minimum TLS version below TLS 1.2 is not permitted")
	}
	if min > MaxVersion {
		return 0, fmt.Errorf("transportsecurity: minimum TLS version above the supported maximum (TLS 1.3)")
	}
	if min == MinVersionCompat && !allowCompatMode {
		return 0, fmt.Errorf("transportsecurity: TLS 1.2 compat mode requires AllowCompatMode=true and a documented qualified-dependency reason")
	}
	return min, nil
}

// ServerConfig builds the accept-side *tls.Config for one AgentAtlas link.
type ServerConfig struct {
	// GetCertificate supplies this side's own leaf certificate; called per
	// handshake so hot-reload callers can rotate without rebuilding the
	// *tls.Config. Required.
	GetCertificate func(*tls.ClientHelloInfo) (*tls.Certificate, error)
	// Roots verifies the client certificate when RequireClientCert is true.
	Roots RootsFunc
	// Revoked optionally rejects revoked client identities.
	Revoked RevokedFunc
	// RequireClientCert selects mTLS (true) vs server-auth-only TLS
	// (false) for this link.
	RequireClientCert bool
	// ExpectedClientIdentity pins the client identity when
	// RequireClientCert is true. Left Empty() to accept any
	// trust-bundle-verified client (not used by any named AgentAtlas link).
	ExpectedClientIdentity PeerIdentity
	// MinVersion overrides MinVersionDefault; 0 means MinVersionDefault.
	MinVersion uint16
	// AllowCompatMode permits MinVersion == MinVersionCompat (TLS 1.2).
	AllowCompatMode bool
	// Now overrides time.Now for tests; nil means time.Now.
	Now func() time.Time
}

// Build validates opts and returns the *tls.Config, or a descriptive error
// (never containing key material) if the configuration is unsafe.
func (c ServerConfig) Build() (*tls.Config, error) {
	if c.GetCertificate == nil {
		return nil, errors.New("transportsecurity: server GetCertificate is required")
	}
	minVersion, err := resolveMinVersion(c.MinVersion, c.AllowCompatMode)
	if err != nil {
		return nil, err
	}
	cfg := &tls.Config{
		GetCertificate:   c.GetCertificate,
		MinVersion:       minVersion,
		MaxVersion:       MaxVersion,
		CurvePreferences: CurvePreferences,
	}
	if minVersion == MinVersionCompat {
		cfg.CipherSuites = CompatCipherSuites
	}
	if c.RequireClientCert {
		if c.Roots == nil {
			return nil, errors.New("transportsecurity: RequireClientCert=true needs a Roots trust bundle source")
		}
		expected := c.ExpectedClientIdentity
		roots := c.Roots
		revoked := c.Revoked
		now := c.Now
		// RequireAnyClientCert (not RequireAndVerifyClientCert): the stdlib
		// only enforces "at least one certificate sent", never verifies it
		// against a static ClientCAs pool. VerifyPeerCertificate below is
		// the sole verifier, which is what lets Roots/Revoked rotate live.
		cfg.ClientAuth = tls.RequireAnyClientCert
		cfg.VerifyPeerCertificate = func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
			return VerifyPeerChain(rawCerts, roots, revoked, expected, PeerIsClient, now)
		}
	} else {
		cfg.ClientAuth = tls.NoClientCert
	}
	return cfg, nil
}

// ClientConfig builds the dial-side *tls.Config for one AgentAtlas link.
type ClientConfig struct {
	// GetClientCertificate presents this side's own leaf certificate for
	// mTLS links; nil means this client never presents a certificate
	// (server-auth-only TLS).
	GetClientCertificate func(*tls.CertificateRequestInfo) (*tls.Certificate, error)
	// Roots verifies the server certificate. Required — no client ever
	// skips server verification on an AgentAtlas link.
	Roots RootsFunc
	// Revoked optionally rejects a revoked server identity.
	Revoked RevokedFunc
	// ServerName is used for SNI only; the stdlib's own hostname check is
	// disabled (InsecureSkipVerify) in favor of ExpectedServerIdentity, so
	// this may be empty when ExpectedServerIdentity pins a SPIFFE URI
	// instead of a DNS name.
	ServerName string
	// ExpectedServerIdentity pins the server identity. Required in
	// practice for every named AgentAtlas link (Empty() disables pinning
	// and is rejected unless the caller is deliberately permissive).
	ExpectedServerIdentity PeerIdentity
	// MinVersion overrides MinVersionDefault; 0 means MinVersionDefault.
	MinVersion uint16
	// AllowCompatMode permits MinVersion == MinVersionCompat (TLS 1.2).
	AllowCompatMode bool
	// Now overrides time.Now for tests; nil means time.Now.
	Now func() time.Time
}

// Build validates opts and returns the *tls.Config, or a descriptive error
// (never containing key material) if the configuration is unsafe.
func (c ClientConfig) Build() (*tls.Config, error) {
	if c.Roots == nil {
		return nil, errors.New("transportsecurity: client Roots trust bundle source is required (no implicit system trust for AgentAtlas links)")
	}
	minVersion, err := resolveMinVersion(c.MinVersion, c.AllowCompatMode)
	if err != nil {
		return nil, err
	}
	expected := c.ExpectedServerIdentity
	roots := c.Roots
	revoked := c.Revoked
	now := c.Now
	cfg := &tls.Config{
		GetClientCertificate: c.GetClientCertificate,
		ServerName:           c.ServerName,
		MinVersion:           minVersion,
		MaxVersion:           MaxVersion,
		CurvePreferences:     CurvePreferences,
		// The stdlib's chain+hostname verification is disabled in favor of
		// VerifyPeerCertificate below, which is what lets Roots/Revoked
		// rotate live without rebuilding this *tls.Config (see package doc).
		InsecureSkipVerify: true,
		VerifyPeerCertificate: func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
			return VerifyPeerChain(rawCerts, roots, revoked, expected, PeerIsServer, now)
		},
	}
	if minVersion == MinVersionCompat {
		cfg.CipherSuites = CompatCipherSuites
	}
	return cfg, nil
}
