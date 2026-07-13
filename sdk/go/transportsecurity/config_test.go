// Tests for the public shared TLS profile. This file is a DELIBERATE
// addition to GA Task 13A's closed file inventory (flagged in the task's
// POST-QUALITY-REVIEW evidence): a public chain-verifier SDK that Task 16A
// and the enterprise module depend on must carry its own direct
// guard-branch tests rather than being covered only indirectly through
// [core]/services/agentatlas/internal/transportsecurity. These are
// white-box (package transportsecurity) so they can exercise the
// unexported resolveMinVersion as well.
package transportsecurity

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"net/url"
	"testing"
	"time"
)

// --- in-memory test PKI (no files, no shared helpers) ----------------------

type miniCA struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
}

func newMiniCA(t *testing.T) *miniCA {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("ca key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "mini-root"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("ca cert: %v", err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("parse ca: %v", err)
	}
	return &miniCA{cert: cert, key: key}
}

func (ca *miniCA) pool() *x509.CertPool {
	p := x509.NewCertPool()
	p.AddCert(ca.cert)
	return p
}

type leafSpec struct {
	dns       []string
	spiffe    string
	eku       []x509.ExtKeyUsage
	notBefore time.Time
	notAfter  time.Time
}

func (ca *miniCA) leaf(t *testing.T, spec leafSpec) []byte {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("leaf key: %v", err)
	}
	nb, na := spec.notBefore, spec.notAfter
	if nb.IsZero() {
		nb = time.Now().Add(-time.Hour)
	}
	if na.IsZero() {
		na = time.Now().Add(time.Hour)
	}
	eku := spec.eku
	if eku == nil {
		eku = []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth}
	}
	serial, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "leaf"},
		NotBefore:             nb,
		NotAfter:              na,
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           eku,
		BasicConstraintsValid: true,
		DNSNames:              spec.dns,
	}
	if spec.spiffe != "" {
		u, err := url.Parse(spec.spiffe)
		if err != nil {
			t.Fatalf("spiffe parse: %v", err)
		}
		tmpl.URIs = []*url.URL{u}
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.cert, &key.PublicKey, ca.key)
	if err != nil {
		t.Fatalf("leaf cert: %v", err)
	}
	return der
}

// --- VerifyPeerChain guard/negative branches -------------------------------

func TestVerifyPeerChainGuards(t *testing.T) {
	ca := newMiniCA(t)
	good := ca.leaf(t, leafSpec{dns: []string{"svc.internal"}, spiffe: "spiffe://x/sa/svc"})
	rootsOK := func() *x509.CertPool { return ca.pool() }
	svcID := PeerIdentity{DNSName: "svc.internal", SPIFFEID: "spiffe://x/sa/svc"}

	t.Run("empty_cert", func(t *testing.T) {
		if err := VerifyPeerChain(nil, rootsOK, nil, svcID, PeerIsServer, nil); err == nil {
			t.Fatal("expected error for no peer certificate")
		}
	})
	t.Run("nil_roots_func", func(t *testing.T) {
		if err := VerifyPeerChain([][]byte{good}, nil, nil, svcID, PeerIsServer, nil); err == nil {
			t.Fatal("expected error for nil roots func")
		}
	})
	t.Run("nil_pool", func(t *testing.T) {
		if err := VerifyPeerChain([][]byte{good}, func() *x509.CertPool { return nil }, nil, svcID, PeerIsServer, nil); err == nil {
			t.Fatal("expected error for nil trust pool")
		}
	})
	t.Run("parse_failure", func(t *testing.T) {
		if err := VerifyPeerChain([][]byte{[]byte("not a certificate")}, rootsOK, nil, svcID, PeerIsServer, nil); err == nil {
			t.Fatal("expected parse error for garbage cert bytes")
		}
	})
	t.Run("expired", func(t *testing.T) {
		expired := ca.leaf(t, leafSpec{dns: []string{"svc.internal"}, spiffe: "spiffe://x/sa/svc", notBefore: time.Now().Add(-2 * time.Hour), notAfter: time.Now().Add(-time.Hour)})
		if err := VerifyPeerChain([][]byte{expired}, rootsOK, nil, svcID, PeerIsServer, nil); err == nil {
			t.Fatal("expected error for expired certificate")
		}
	})
	t.Run("wrong_eku", func(t *testing.T) {
		// Leaf carries only ClientAuth; verifying as a SERVER (wants
		// ServerAuth) must fail on EKU even though the chain is otherwise
		// valid.
		clientOnly := ca.leaf(t, leafSpec{dns: []string{"svc.internal"}, spiffe: "spiffe://x/sa/svc", eku: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}})
		if err := VerifyPeerChain([][]byte{clientOnly}, rootsOK, nil, svcID, PeerIsServer, nil); err == nil {
			t.Fatal("expected error: client-only EKU verified in server role")
		}
	})
	t.Run("revoked", func(t *testing.T) {
		revoked := func(*x509.Certificate) bool { return true }
		if err := VerifyPeerChain([][]byte{good}, rootsOK, revoked, svcID, PeerIsServer, nil); err == nil {
			t.Fatal("expected error for revoked leaf")
		}
	})
	t.Run("identity_mismatch", func(t *testing.T) {
		wantOther := PeerIdentity{DNSName: "other.internal", SPIFFEID: "spiffe://x/sa/other"}
		if err := VerifyPeerChain([][]byte{good}, rootsOK, nil, wantOther, PeerIsServer, nil); err == nil {
			t.Fatal("expected error for identity mismatch")
		}
	})
	t.Run("untrusted_ca", func(t *testing.T) {
		other := newMiniCA(t)
		otherLeaf := other.leaf(t, leafSpec{dns: []string{"svc.internal"}, spiffe: "spiffe://x/sa/svc"})
		if err := VerifyPeerChain([][]byte{otherLeaf}, rootsOK, nil, svcID, PeerIsServer, nil); err == nil {
			t.Fatal("expected error: leaf signed by a CA outside the trust pool")
		}
	})
	t.Run("happy_server_role", func(t *testing.T) {
		if err := VerifyPeerChain([][]byte{good}, rootsOK, nil, svcID, PeerIsServer, nil); err != nil {
			t.Fatalf("expected success: %v", err)
		}
	})
	t.Run("happy_client_role", func(t *testing.T) {
		if err := VerifyPeerChain([][]byte{good}, rootsOK, func(*x509.Certificate) bool { return false }, svcID, PeerIsClient, time.Now); err != nil {
			t.Fatalf("expected success in client role: %v", err)
		}
	})
}

// --- resolveMinVersion (unexported; white-box) -----------------------------

func TestResolveMinVersion(t *testing.T) {
	cases := []struct {
		name        string
		requested   uint16
		allowCompat bool
		wantErr     bool
		wantVersion uint16
	}{
		{"default_when_zero", 0, false, false, MinVersionDefault},
		{"tls13_explicit", tls.VersionTLS13, false, false, tls.VersionTLS13},
		{"below_1_2_rejected", tls.VersionTLS11, false, true, 0},
		{"tls12_without_optin_rejected", MinVersionCompat, false, true, 0},
		{"tls12_with_optin_ok", MinVersionCompat, true, false, MinVersionCompat},
		{"above_max_rejected", MaxVersion + 1, true, true, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := resolveMinVersion(tc.requested, tc.allowCompat)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got version %#x", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.wantVersion {
				t.Fatalf("version = %#x, want %#x", got, tc.wantVersion)
			}
		})
	}
}

// --- PeerIdentity.Matches --------------------------------------------------

func TestPeerIdentityMatches(t *testing.T) {
	ca := newMiniCA(t)
	der := ca.leaf(t, leafSpec{dns: []string{"svc.internal"}, spiffe: "spiffe://x/sa/svc"})
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name    string
		id      PeerIdentity
		wantErr bool
	}{
		{"neither_empty_matches_anything", PeerIdentity{}, false},
		{"spiffe_only_match", PeerIdentity{SPIFFEID: "spiffe://x/sa/svc"}, false},
		{"dns_only_match", PeerIdentity{DNSName: "svc.internal"}, false},
		{"both_present_both_match", PeerIdentity{SPIFFEID: "spiffe://x/sa/svc", DNSName: "svc.internal"}, false},
		{"both_configured_only_dns_present_on_cert_still_ok", PeerIdentity{SPIFFEID: "spiffe://x/sa/other", DNSName: "svc.internal"}, false},
		{"both_configured_only_spiffe_present_on_cert_still_ok", PeerIdentity{SPIFFEID: "spiffe://x/sa/svc", DNSName: "other.internal"}, false},
		{"spiffe_only_mismatch", PeerIdentity{SPIFFEID: "spiffe://x/sa/other"}, true},
		{"dns_only_mismatch", PeerIdentity{DNSName: "other.internal"}, true},
		{"both_mismatch", PeerIdentity{SPIFFEID: "spiffe://x/sa/other", DNSName: "other.internal"}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.id.Matches(cert)
			if tc.wantErr && err == nil {
				t.Fatal("expected mismatch error")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("expected match, got: %v", err)
			}
		})
	}

	t.Run("Empty", func(t *testing.T) {
		if !(PeerIdentity{}).Empty() {
			t.Fatal("zero PeerIdentity should be Empty")
		}
		if (PeerIdentity{DNSName: "x"}).Empty() {
			t.Fatal("DNS-set PeerIdentity is not Empty")
		}
		if (PeerIdentity{SPIFFEID: "x"}).Empty() {
			t.Fatal("SPIFFE-set PeerIdentity is not Empty")
		}
	})
}

// --- Build paths -----------------------------------------------------------

func TestServerConfigBuild(t *testing.T) {
	ca := newMiniCA(t)
	getCert := func(*tls.ClientHelloInfo) (*tls.Certificate, error) { return &tls.Certificate{}, nil }
	roots := func() *x509.CertPool { return ca.pool() }

	t.Run("nil_get_certificate_rejected", func(t *testing.T) {
		if _, err := (ServerConfig{}).Build(); err == nil {
			t.Fatal("expected error for nil GetCertificate")
		}
	})
	t.Run("mtls_requires_roots", func(t *testing.T) {
		if _, err := (ServerConfig{GetCertificate: getCert, RequireClientCert: true}).Build(); err == nil {
			t.Fatal("expected error: RequireClientCert without Roots")
		}
	})
	t.Run("tls13_default", func(t *testing.T) {
		cfg, err := (ServerConfig{GetCertificate: getCert}).Build()
		if err != nil {
			t.Fatal(err)
		}
		if cfg.MinVersion != tls.VersionTLS13 || cfg.MaxVersion != tls.VersionTLS13 {
			t.Fatalf("versions = %#x/%#x", cfg.MinVersion, cfg.MaxVersion)
		}
		if cfg.ClientAuth != tls.NoClientCert {
			t.Fatalf("server-auth-only ClientAuth = %v", cfg.ClientAuth)
		}
		if len(cfg.CipherSuites) != 0 {
			t.Fatal("TLS 1.3 config must not pin CipherSuites")
		}
	})
	t.Run("mtls_sets_require_any_client_cert_and_verify", func(t *testing.T) {
		cfg, err := (ServerConfig{GetCertificate: getCert, Roots: roots, RequireClientCert: true, ExpectedClientIdentity: PeerIdentity{DNSName: "svc.internal"}}).Build()
		if err != nil {
			t.Fatal(err)
		}
		if cfg.ClientAuth != tls.RequireAnyClientCert {
			t.Fatalf("mTLS ClientAuth = %v", cfg.ClientAuth)
		}
		if cfg.VerifyPeerCertificate == nil {
			t.Fatal("mTLS must install VerifyPeerCertificate")
		}
	})
	t.Run("compat_cipher_path", func(t *testing.T) {
		cfg, err := (ServerConfig{GetCertificate: getCert, MinVersion: MinVersionCompat, AllowCompatMode: true}).Build()
		if err != nil {
			t.Fatal(err)
		}
		if cfg.MinVersion != MinVersionCompat {
			t.Fatalf("MinVersion = %#x", cfg.MinVersion)
		}
		if len(cfg.CipherSuites) == 0 {
			t.Fatal("compat mode must pin an explicit cipher suite list")
		}
	})
	t.Run("compat_without_optin_rejected", func(t *testing.T) {
		if _, err := (ServerConfig{GetCertificate: getCert, MinVersion: MinVersionCompat}).Build(); err == nil {
			t.Fatal("expected error: TLS 1.2 without AllowCompatMode")
		}
	})
}

func TestClientConfigBuild(t *testing.T) {
	ca := newMiniCA(t)
	roots := func() *x509.CertPool { return ca.pool() }

	t.Run("nil_roots_rejected", func(t *testing.T) {
		if _, err := (ClientConfig{}).Build(); err == nil {
			t.Fatal("expected error: client without Roots")
		}
	})
	t.Run("tls13_default_pins_identity_and_skips_stdlib_verify", func(t *testing.T) {
		cfg, err := (ClientConfig{Roots: roots, ServerName: "svc.internal", ExpectedServerIdentity: PeerIdentity{DNSName: "svc.internal"}}).Build()
		if err != nil {
			t.Fatal(err)
		}
		if cfg.MinVersion != tls.VersionTLS13 {
			t.Fatalf("MinVersion = %#x", cfg.MinVersion)
		}
		if !cfg.InsecureSkipVerify {
			t.Fatal("client must set InsecureSkipVerify (stdlib verify replaced by VerifyPeerCertificate)")
		}
		if cfg.VerifyPeerCertificate == nil {
			t.Fatal("client must install VerifyPeerCertificate")
		}
		if cfg.GetClientCertificate != nil {
			t.Fatal("server-auth-only client must not present a certificate")
		}
	})
	t.Run("mtls_client_presents_cert", func(t *testing.T) {
		getClientCert := func(*tls.CertificateRequestInfo) (*tls.Certificate, error) { return &tls.Certificate{}, nil }
		cfg, err := (ClientConfig{GetClientCertificate: getClientCert, Roots: roots, ExpectedServerIdentity: PeerIdentity{SPIFFEID: "spiffe://x/sa/svc"}}).Build()
		if err != nil {
			t.Fatal(err)
		}
		if cfg.GetClientCertificate == nil {
			t.Fatal("mTLS client must present a certificate")
		}
	})
	t.Run("compat_cipher_path", func(t *testing.T) {
		cfg, err := (ClientConfig{Roots: roots, ExpectedServerIdentity: PeerIdentity{DNSName: "svc.internal"}, MinVersion: MinVersionCompat, AllowCompatMode: true}).Build()
		if err != nil {
			t.Fatal(err)
		}
		if len(cfg.CipherSuites) == 0 {
			t.Fatal("client compat mode must pin an explicit cipher suite list")
		}
	})
	t.Run("compat_without_optin_rejected", func(t *testing.T) {
		if _, err := (ClientConfig{Roots: roots, ExpectedServerIdentity: PeerIdentity{DNSName: "svc.internal"}, MinVersion: MinVersionCompat}).Build(); err == nil {
			t.Fatal("expected error: client TLS 1.2 without AllowCompatMode")
		}
	})
}
