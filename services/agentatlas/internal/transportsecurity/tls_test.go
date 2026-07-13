package transportsecurity

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	sdktls "github.com/astraclawteam/agentatlas/sdk/go/transportsecurity"
)

// ---------------------------------------------------------------------------
// Test PKI helpers. Every certificate is generated in-memory / in t.TempDir()
// per link (2026 GA Task 13A scope rule: no real certs or keys anywhere in
// the repo).
// ---------------------------------------------------------------------------

type testCA struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
	pem  []byte
}

func randomSerial(t *testing.T) *big.Int {
	t.Helper()
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		t.Fatalf("generate serial: %v", err)
	}
	return serial
}

func newTestCA(t *testing.T, cn string) *testCA {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate CA key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          randomSerial(t),
		Subject:               pkix.Name{CommonName: cn},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(48 * time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create CA cert: %v", err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("parse CA cert: %v", err)
	}
	return &testCA{
		cert: cert,
		key:  key,
		pem:  pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
	}
}

type leafOpts struct {
	dnsNames            []string
	spiffeID            string
	notBefore, notAfter time.Time
}

// issueLeaf mints a fresh key pair + leaf certificate signed by ca. Every
// leaf carries both ServerAuth and ClientAuth EKUs so the same identity can
// stand in for either side of a link in these tests.
func (ca *testCA) issueLeaf(t *testing.T, cn string, opts leafOpts) (certPEM, keyPEM []byte, cert *x509.Certificate, serialHex string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate leaf key: %v", err)
	}
	nb, na := opts.notBefore, opts.notAfter
	if nb.IsZero() {
		nb = time.Now().Add(-time.Hour)
	}
	if na.IsZero() {
		na = time.Now().Add(time.Hour)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          randomSerial(t),
		Subject:               pkix.Name{CommonName: cn},
		NotBefore:             nb,
		NotAfter:              na,
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		BasicConstraintsValid: true,
		DNSNames:              opts.dnsNames,
	}
	if opts.spiffeID != "" {
		u, err := url.Parse(opts.spiffeID)
		if err != nil {
			t.Fatalf("parse spiffe id %q: %v", opts.spiffeID, err)
		}
		tmpl.URIs = []*url.URL{u}
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.cert, &key.PublicKey, ca.key)
	if err != nil {
		t.Fatalf("create leaf cert: %v", err)
	}
	parsed, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("parse leaf cert: %v", err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatalf("marshal leaf key: %v", err)
	}
	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	return certPEM, keyPEM, parsed, fmt.Sprintf("%x", parsed.SerialNumber)
}

func writeFile(t *testing.T, dir, name string, data []byte) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, data, 0o600); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
	return p
}

func slug(s string) string {
	return strings.ReplaceAll(strings.ToLower(s), " ", "-")
}

type mgrOpts struct {
	mode                      Mode
	certPEM, keyPEM, trustPEM []byte
	expectDNS, expectSPIFFE   string
	revoked                   []string
}

// buildManager materializes o's PEM material as files under a fresh
// t.TempDir() and loads a Manager over them. expectDNS/expectSPIFFE encode
// "who this Manager expects to be talking to" — used both when this Manager
// plays server (as the expected client identity) and when it plays client
// (as the expected server identity), matching how one AgentAtlas link is a
// single pairwise relationship.
func buildManager(t *testing.T, link string, o mgrOpts) *Manager {
	t.Helper()
	dir := t.TempDir()
	certPath := writeFile(t, dir, "tls.crt", o.certPEM)
	keyPath := writeFile(t, dir, "tls.key", o.keyPEM)
	trustPath := writeFile(t, dir, "trust.pem", o.trustPEM)
	revPath := filepath.Join(dir, "revoked.txt")
	if err := os.WriteFile(revPath, []byte(strings.Join(o.revoked, "\n")), 0o600); err != nil {
		t.Fatalf("write revocation file: %v", err)
	}
	mgr, err := NewManager(link, LinkConfig{
		Mode:            o.mode,
		CertFile:        certPath,
		KeyFile:         keyPath,
		TrustBundleFile: trustPath,
		RevocationFile:  revPath,
		ServerName:      o.expectDNS,
		SPIFFEID:        o.expectSPIFFE,
	})
	if err != nil {
		t.Fatalf("NewManager(%s): %v", link, err)
	}
	return mgr
}

// ---------------------------------------------------------------------------
// TLS-level accept/dial helpers.
// ---------------------------------------------------------------------------

type handshakeResult struct {
	err   error
	state tls.ConnectionState
}

// serveLoop accepts connections on ln forever (until it is closed),
// performing the TLS handshake explicitly so a non-TLS dial surfaces as an
// observable handshake error rather than a silent hang, and reports each
// outcome on the returned channel. On a successful server-side handshake it
// writes a 3-byte "OK\n" rendezvous ack — see dialTLS's doc comment for why
// that is required to observe rejection reliably.
func serveLoop(ln net.Listener) <-chan handshakeResult {
	results := make(chan handshakeResult, 16)
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				tlsConn, ok := c.(*tls.Conn)
				if !ok {
					results <- handshakeResult{err: fmt.Errorf("accepted connection is not TLS")}
					return
				}
				_ = tlsConn.SetDeadline(time.Now().Add(5 * time.Second))
				hsErr := tlsConn.HandshakeContext(context.Background())
				res := handshakeResult{err: hsErr}
				if hsErr == nil {
					res.state = tlsConn.ConnectionState()
					_, _ = tlsConn.Write([]byte("OK\n"))
				}
				results <- res
			}(conn)
		}
	}()
	return results
}

func awaitResult(t *testing.T, results <-chan handshakeResult, timeout time.Duration) handshakeResult {
	t.Helper()
	select {
	case r := <-results:
		return r
	case <-time.After(timeout):
		t.Fatal("timed out waiting for server-side handshake result")
		return handshakeResult{}
	}
}

// rawDialTLS dials addr with cfg and attempts the handshake, returning the
// (possibly non-nil-but-unauthenticated, see dialTLS) connection and
// whatever error the CLIENT side of the handshake itself observed.
func rawDialTLS(addr string, cfg *tls.Config, timeout time.Duration) (*tls.Conn, error) {
	d := &net.Dialer{Timeout: timeout}
	rawConn, err := d.Dial("tcp", addr)
	if err != nil {
		return nil, err
	}
	tlsConn := tls.Client(rawConn, cfg)
	_ = tlsConn.SetDeadline(time.Now().Add(timeout))
	err = tlsConn.HandshakeContext(context.Background())
	return tlsConn, err
}

// assertServerRejected proves the peer (acting as TLS server, via
// serveLoop) rejected this connection, accounting for TLS 1.3's asymmetric
// completion: when a server must reject based on something about the
// CLIENT (missing certificate, wrong identity, expired, revoked), RFC
// 8446's flow lets the CLIENT's own HandshakeContext call return nil —
// the client sends its Finished flight and considers its side complete
// before the server has finished validating the client's Certificate
// message and decided to abort. The server's rejection alert/close only
// arrives once the client tries to USE the connection, which is exactly
// what real callers (an HTTP round trip, a database driver, ...) always do
// next — so this is what an application would actually observe, not a
// test-only technicality.
func assertServerRejected(t *testing.T, conn *tls.Conn, handshakeErr error) {
	t.Helper()
	if conn != nil {
		defer conn.Close()
	}
	if handshakeErr != nil {
		return
	}
	buf := make([]byte, 3)
	if _, err := io.ReadFull(conn, buf); err == nil {
		t.Fatal("expected the server to reject this connection, but it accepted it")
	}
}

// dialTLS dials addr using mgr's ClientTLSConfig and confirms the SERVER
// (not just this client's own view of the handshake — see
// assertServerRejected) accepted the connection, by reading serveLoop's
// rendezvous ack. Returns an error whenever either side would consider the
// connection rejected.
func dialTLS(addr string, mgr *Manager, timeout time.Duration) (*tls.Conn, error) {
	cfg, err := mgr.ClientTLSConfig()
	if err != nil {
		return nil, err
	}
	conn, err := rawDialTLS(addr, cfg, timeout)
	if err != nil {
		if conn != nil {
			conn.Close()
		}
		return nil, err
	}
	ack := make([]byte, 3)
	if _, err := io.ReadFull(conn, ack); err != nil {
		conn.Close()
		return nil, fmt.Errorf("server rejected the connection post-handshake: %w", err)
	}
	if string(ack) != "OK\n" {
		conn.Close()
		return nil, fmt.Errorf("unexpected server rendezvous ack: %q", ack)
	}
	_ = conn.SetDeadline(time.Time{})
	return conn, nil
}

// ---------------------------------------------------------------------------
// TestTLSProfile: the shared sdk/go/transportsecurity profile as wired by
// this package's Manager.
// ---------------------------------------------------------------------------

func TestTLSProfile(t *testing.T) {
	ca := newTestCA(t, "profile-root")
	certPEM, keyPEM, _, _ := ca.issueLeaf(t, "svc", leafOpts{
		dnsNames: []string{"svc.internal"},
		spiffeID: "spiffe://agentatlas.internal/ns/test/sa/svc",
	})
	mgr := buildManager(t, "svc", mgrOpts{
		mode: ModeMTLS, certPEM: certPEM, keyPEM: keyPEM, trustPEM: ca.pem,
		expectDNS: "peer.internal", expectSPIFFE: "spiffe://agentatlas.internal/ns/test/sa/peer",
	})

	serverCfg, err := mgr.ServerTLSConfig()
	if err != nil {
		t.Fatalf("server config: %v", err)
	}
	if serverCfg.MinVersion != tls.VersionTLS13 {
		t.Fatalf("server MinVersion = %#x, want TLS1.3 (%#x)", serverCfg.MinVersion, tls.VersionTLS13)
	}
	if serverCfg.MaxVersion != tls.VersionTLS13 {
		t.Fatalf("server MaxVersion = %#x, want TLS1.3", serverCfg.MaxVersion)
	}
	if len(serverCfg.CurvePreferences) == 0 {
		t.Fatal("server CurvePreferences not set")
	}
	if serverCfg.ClientAuth != tls.RequireAnyClientCert {
		t.Fatalf("mTLS server ClientAuth = %v, want RequireAnyClientCert", serverCfg.ClientAuth)
	}

	clientCfg, err := mgr.ClientTLSConfig()
	if err != nil {
		t.Fatalf("client config: %v", err)
	}
	if clientCfg.MinVersion != tls.VersionTLS13 {
		t.Fatalf("client MinVersion = %#x, want TLS1.3", clientCfg.MinVersion)
	}
	if clientCfg.GetClientCertificate == nil {
		t.Fatal("mTLS client must be configured to present a certificate")
	}

	tlsOnly := buildManager(t, "svc-tls-only", mgrOpts{
		mode: ModeTLS, certPEM: certPEM, keyPEM: keyPEM, trustPEM: ca.pem,
		expectDNS: "peer.internal", expectSPIFFE: "spiffe://agentatlas.internal/ns/test/sa/peer",
	})
	tlsOnlyServer, err := tlsOnly.ServerTLSConfig()
	if err != nil {
		t.Fatal(err)
	}
	if tlsOnlyServer.ClientAuth != tls.NoClientCert {
		t.Fatalf("TLS-only server ClientAuth = %v, want NoClientCert", tlsOnlyServer.ClientAuth)
	}
	tlsOnlyClient, err := tlsOnly.ClientTLSConfig()
	if err != nil {
		t.Fatal(err)
	}
	if tlsOnlyClient.GetClientCertificate != nil {
		t.Fatal("TLS-only (server-auth) client must not present a certificate")
	}

	// The shared profile's compat-mode gate: below TLS 1.2 is refused
	// outright; TLS 1.2 is refused without an explicit opt-in and accepted
	// (with an explicit cipher suite list) once opted in.
	if _, err := (sdktls.ServerConfig{GetCertificate: mgr.GetCertificate, MinVersion: tls.VersionTLS11}).Build(); err == nil {
		t.Fatal("TLS 1.1 floor must be rejected")
	}
	if _, err := (sdktls.ServerConfig{GetCertificate: mgr.GetCertificate, MinVersion: tls.VersionTLS12}).Build(); err == nil {
		t.Fatal("TLS 1.2 without AllowCompatMode must be rejected")
	}
	compat, err := (sdktls.ServerConfig{GetCertificate: mgr.GetCertificate, MinVersion: tls.VersionTLS12, AllowCompatMode: true}).Build()
	if err != nil {
		t.Fatalf("compat mode with explicit opt-in: %v", err)
	}
	if compat.MinVersion != tls.VersionTLS12 {
		t.Fatalf("compat MinVersion = %#x, want TLS1.2", compat.MinVersion)
	}
	if len(compat.CipherSuites) == 0 {
		t.Fatal("compat mode must pin an explicit cipher suite list")
	}
}

// ---------------------------------------------------------------------------
// TestTLSLinkMatrix: the named-link matrix. AgentAtlas and gateway are
// server-role (AgentAtlas/parser-gateway accept inbound connections);
// AgentNexus, llmrouter, PostgreSQL, OpenSearch, NATS, object storage, and
// parser are client-role (AgentAtlas dials out). Every link gets a real
// started server/client TLS composition proving the core negative cases;
// AgentAtlas (server-role) and AgentNexus (client-role) additionally run
// the full negative-case suite as the two in-depth exemplars.
// ---------------------------------------------------------------------------

type linkRole int

const (
	roleServer linkRole = iota
	roleClient
)

type linkCase struct {
	name string
	role linkRole
	full bool
}

var namedLinks = []linkCase{
	{"AgentAtlas", roleServer, true},
	{"gateway", roleServer, false},
	{"AgentNexus", roleClient, true},
	{"llmrouter", roleClient, false},
	{"PostgreSQL", roleClient, false},
	{"OpenSearch", roleClient, false},
	{"NATS", roleClient, false},
	{"object storage", roleClient, false},
	{"parser", roleClient, false},
}

func TestTLSLinkMatrix(t *testing.T) {
	for _, lc := range namedLinks {
		lc := lc
		t.Run(lc.name, func(t *testing.T) {
			switch lc.role {
			case roleServer:
				runServerRoleCases(t, lc.name, lc.full)
			case roleClient:
				runClientRoleCases(t, lc.name, lc.full)
			}
		})
	}
}

// serverRoleFixture models AgentAtlas/gateway: this Manager IS the TLS
// server. A fresh fixture per subtest keeps negative cases isolated.
type serverRoleFixture struct {
	ca      *testCA
	mgr     *Manager
	addr    string
	results <-chan handshakeResult
}

func newServerRoleFixture(t *testing.T, link string) *serverRoleFixture {
	t.Helper()
	ca := newTestCA(t, link+"-root")
	certPEM, keyPEM, _, _ := ca.issueLeaf(t, link, leafOpts{
		dnsNames: []string{link + ".internal"},
		spiffeID: "spiffe://agentatlas.internal/ns/test/sa/" + slug(link),
	})
	mgr := buildManager(t, link, mgrOpts{
		mode: ModeMTLS, certPEM: certPEM, keyPEM: keyPEM, trustPEM: ca.pem,
		expectDNS:    "caller." + link + ".internal",
		expectSPIFFE: "spiffe://agentatlas.internal/ns/test/sa/caller-of-" + slug(link),
	})
	tlsCfg, err := mgr.ServerTLSConfig()
	if err != nil {
		t.Fatalf("%s: server config: %v", link, err)
	}
	ln, err := tls.Listen("tcp", "127.0.0.1:0", tlsCfg)
	if err != nil {
		t.Fatalf("%s: listen: %v", link, err)
	}
	t.Cleanup(func() { ln.Close() })
	return &serverRoleFixture{ca: ca, mgr: mgr, addr: ln.Addr().String(), results: serveLoop(ln)}
}

// issueGoodCaller mints a leaf that satisfies fx's expected client identity.
func (fx *serverRoleFixture) issueGoodCaller(t *testing.T, link string) *Manager {
	certPEM, keyPEM, _, _ := fx.ca.issueLeaf(t, "caller-of-"+slug(link), leafOpts{
		dnsNames: []string{"caller." + link + ".internal"},
		spiffeID: "spiffe://agentatlas.internal/ns/test/sa/caller-of-" + slug(link),
	})
	return buildManager(t, "caller-of-"+slug(link), mgrOpts{
		mode: ModeMTLS, certPEM: certPEM, keyPEM: keyPEM, trustPEM: fx.ca.pem,
		expectDNS: link + ".internal", expectSPIFFE: "spiffe://agentatlas.internal/ns/test/sa/" + slug(link),
	})
}

func runServerRoleCases(t *testing.T, link string, full bool) {
	t.Run("good", func(t *testing.T) {
		fx := newServerRoleFixture(t, link)
		caller := fx.issueGoodCaller(t, link)
		conn, err := dialTLS(fx.addr, caller, 5*time.Second)
		if err != nil {
			t.Fatalf("expected successful mTLS dial, got: %v", err)
		}
		defer conn.Close()
		if conn.ConnectionState().Version < tls.VersionTLS12 {
			t.Fatalf("negotiated version too low: %#x", conn.ConnectionState().Version)
		}
		res := awaitResult(t, fx.results, 5*time.Second)
		if res.err != nil {
			t.Fatalf("server observed handshake error: %v", res.err)
		}
	})

	t.Run("missing_client_cert", func(t *testing.T) {
		fx := newServerRoleFixture(t, link)
		pool := x509.NewCertPool()
		pool.AddCert(fx.ca.cert)
		bare := &tls.Config{RootCAs: pool, ServerName: link + ".internal", MinVersion: tls.VersionTLS12}
		conn, err := rawDialTLS(fx.addr, bare, 5*time.Second)
		assertServerRejected(t, conn, err)
	})

	t.Run("wrong_identity", func(t *testing.T) {
		fx := newServerRoleFixture(t, link)
		certPEM, keyPEM, _, _ := fx.ca.issueLeaf(t, "impostor", leafOpts{
			dnsNames: []string{"impostor.internal"},
			spiffeID: "spiffe://agentatlas.internal/ns/test/sa/impostor",
		})
		impostor := buildManager(t, "impostor", mgrOpts{
			mode: ModeMTLS, certPEM: certPEM, keyPEM: keyPEM, trustPEM: fx.ca.pem,
			expectDNS: link + ".internal", expectSPIFFE: "spiffe://agentatlas.internal/ns/test/sa/" + slug(link),
		})
		if _, err := dialTLS(fx.addr, impostor, 5*time.Second); err == nil {
			t.Fatal("expected rejection: client identity does not match what the server expects")
		}
	})

	if !full {
		return
	}

	t.Run("plaintext", func(t *testing.T) {
		fx := newServerRoleFixture(t, link)
		conn, err := net.DialTimeout("tcp", fx.addr, 5*time.Second)
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		defer conn.Close()
		_ = conn.SetDeadline(time.Now().Add(2 * time.Second))
		if _, err := conn.Write([]byte("GET / HTTP/1.1\r\nHost: x\r\n\r\n")); err != nil {
			return // rejected already at the transport level
		}
		buf := make([]byte, 64)
		if _, err := conn.Read(buf); err == nil {
			t.Fatal("expected the plaintext connection to be rejected, got a plausible response")
		}
	})

	t.Run("expired_cert", func(t *testing.T) {
		fx := newServerRoleFixture(t, link)
		certPEM, keyPEM, _, _ := fx.ca.issueLeaf(t, "caller-of-"+slug(link), leafOpts{
			dnsNames:  []string{"caller." + link + ".internal"},
			spiffeID:  "spiffe://agentatlas.internal/ns/test/sa/caller-of-" + slug(link),
			notBefore: time.Now().Add(-2 * time.Hour), notAfter: time.Now().Add(-time.Hour),
		})
		expired := buildManager(t, "caller-of-"+slug(link), mgrOpts{
			mode: ModeMTLS, certPEM: certPEM, keyPEM: keyPEM, trustPEM: fx.ca.pem,
			expectDNS: link + ".internal", expectSPIFFE: "spiffe://agentatlas.internal/ns/test/sa/" + slug(link),
		})
		if _, err := dialTLS(fx.addr, expired, 5*time.Second); err == nil {
			t.Fatal("expected rejection: expired client certificate")
		}
	})

	t.Run("revoked_cert", func(t *testing.T) {
		fx := newServerRoleFixture(t, link)
		certPEM, keyPEM, _, serial := fx.ca.issueLeaf(t, "caller-of-"+slug(link), leafOpts{
			dnsNames: []string{"caller." + link + ".internal"},
			spiffeID: "spiffe://agentatlas.internal/ns/test/sa/caller-of-" + slug(link),
		})
		fx.mgr.SetRevokedSerials(serial)
		revokedCaller := buildManager(t, "caller-of-"+slug(link), mgrOpts{
			mode: ModeMTLS, certPEM: certPEM, keyPEM: keyPEM, trustPEM: fx.ca.pem,
			expectDNS: link + ".internal", expectSPIFFE: "spiffe://agentatlas.internal/ns/test/sa/" + slug(link),
		})
		if _, err := dialTLS(fx.addr, revokedCaller, 5*time.Second); err == nil {
			t.Fatal("expected rejection: revoked client certificate")
		}
	})

	t.Run("stale_trust_bundle", func(t *testing.T) {
		fx := newServerRoleFixture(t, link) // server trusts fx.ca only
		otherCA := newTestCA(t, "other-root")
		certPEM, keyPEM, _, _ := otherCA.issueLeaf(t, "caller-of-"+slug(link), leafOpts{
			dnsNames: []string{"caller." + link + ".internal"},
			spiffeID: "spiffe://agentatlas.internal/ns/test/sa/caller-of-" + slug(link),
		})
		// The caller's OWN trust bundle correctly trusts the real server
		// CA (fx.ca), so this isolates the case to exactly one problem:
		// the caller's certificate itself is signed by a CA (otherCA) the
		// SERVER's trust bundle does not (yet) contain.
		untrusted := buildManager(t, "caller-of-"+slug(link), mgrOpts{
			mode: ModeMTLS, certPEM: certPEM, keyPEM: keyPEM, trustPEM: fx.ca.pem,
			expectDNS: link + ".internal", expectSPIFFE: "spiffe://agentatlas.internal/ns/test/sa/" + slug(link),
		})
		if _, err := dialTLS(fx.addr, untrusted, 5*time.Second); err == nil {
			t.Fatal("expected rejection: client cert signed by a CA outside the server's (stale) trust bundle")
		}
	})
}

// clientRoleFixture models AgentAtlas dialing OUT to a dependency
// (AgentNexus/llmrouter/PostgreSQL/OpenSearch/NATS/object storage/parser).
type clientRoleFixture struct {
	ca                        *testCA
	depCertPEM, depKeyPEM     []byte
	depSerial                 string
	atlasCertPEM, atlasKeyPEM []byte
	atlasMgr                  *Manager
}

func newClientRoleFixture(t *testing.T, link string) *clientRoleFixture {
	t.Helper()
	ca := newTestCA(t, link+"-root")
	depCertPEM, depKeyPEM, _, depSerial := ca.issueLeaf(t, link, leafOpts{
		dnsNames: []string{link + ".internal"},
		spiffeID: "spiffe://agentatlas.internal/ns/test/sa/" + slug(link),
	})
	atlasCertPEM, atlasKeyPEM, _, _ := ca.issueLeaf(t, "AgentAtlas", leafOpts{
		dnsNames: []string{"agentatlas.internal"},
		spiffeID: "spiffe://agentatlas.internal/ns/test/sa/agentatlas",
	})
	atlasMgr := buildManager(t, link, mgrOpts{
		mode: ModeMTLS, certPEM: atlasCertPEM, keyPEM: atlasKeyPEM, trustPEM: ca.pem,
		expectDNS: link + ".internal", expectSPIFFE: "spiffe://agentatlas.internal/ns/test/sa/" + slug(link),
	})
	return &clientRoleFixture{
		ca: ca, depCertPEM: depCertPEM, depKeyPEM: depKeyPEM, depSerial: depSerial,
		atlasCertPEM: atlasCertPEM, atlasKeyPEM: atlasKeyPEM, atlasMgr: atlasMgr,
	}
}

// startServer stands up an in-process TLS listener playing the dependency,
// presenting certPEM/keyPEM and requiring (and verifying) a client
// certificate signed by trustCA.
func (fx *clientRoleFixture) startServer(t *testing.T, certPEM, keyPEM []byte, trustCA *testCA) (addr string, results <-chan handshakeResult) {
	t.Helper()
	mgr := buildManager(t, "dependency-under-test", mgrOpts{
		mode: ModeMTLS, certPEM: certPEM, keyPEM: keyPEM, trustPEM: trustCA.pem,
		expectDNS: "agentatlas.internal", expectSPIFFE: "spiffe://agentatlas.internal/ns/test/sa/agentatlas",
	})
	cfg, err := mgr.ServerTLSConfig()
	if err != nil {
		t.Fatalf("dependency server config: %v", err)
	}
	ln, err := tls.Listen("tcp", "127.0.0.1:0", cfg)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { ln.Close() })
	return ln.Addr().String(), serveLoop(ln)
}

func runClientRoleCases(t *testing.T, link string, full bool) {
	t.Run("good", func(t *testing.T) {
		fx := newClientRoleFixture(t, link)
		addr, results := fx.startServer(t, fx.depCertPEM, fx.depKeyPEM, fx.ca)
		conn, err := dialTLS(addr, fx.atlasMgr, 5*time.Second)
		if err != nil {
			t.Fatalf("expected successful dial to %s, got: %v", link, err)
		}
		defer conn.Close()
		res := awaitResult(t, results, 5*time.Second)
		if res.err != nil {
			t.Fatalf("%s observed handshake error: %v", link, res.err)
		}
	})

	t.Run("missing_client_cert_required_by_server", func(t *testing.T) {
		fx := newClientRoleFixture(t, link)
		addr, _ := fx.startServer(t, fx.depCertPEM, fx.depKeyPEM, fx.ca)
		noCert := buildManager(t, link, mgrOpts{
			mode: ModeTLS, certPEM: fx.atlasCertPEM, keyPEM: fx.atlasKeyPEM, trustPEM: fx.ca.pem,
			expectDNS: link + ".internal", expectSPIFFE: "spiffe://agentatlas.internal/ns/test/sa/" + slug(link),
		})
		if _, err := dialTLS(addr, noCert, 5*time.Second); err == nil {
			t.Fatalf("expected rejection: %s requires a client certificate", link)
		}
	})

	t.Run("hostname_mismatch", func(t *testing.T) {
		fx := newClientRoleFixture(t, link)
		wrongHostCertPEM, wrongHostKeyPEM, _, _ := fx.ca.issueLeaf(t, link, leafOpts{
			dnsNames: []string{"totally-different-host.internal"},
			spiffeID: "spiffe://agentatlas.internal/ns/test/sa/totally-different",
		})
		addr, _ := fx.startServer(t, wrongHostCertPEM, wrongHostKeyPEM, fx.ca)
		if _, err := dialTLS(addr, fx.atlasMgr, 5*time.Second); err == nil {
			t.Fatalf("expected rejection: %s certificate hostname does not match expected", link)
		}
	})

	if !full {
		return
	}

	t.Run("wrong_identity", func(t *testing.T) {
		fx := newClientRoleFixture(t, link)
		impostorCertPEM, impostorKeyPEM, _, _ := fx.ca.issueLeaf(t, "impostor-"+slug(link), leafOpts{
			dnsNames: []string{"impostor.internal"},
			spiffeID: "spiffe://agentatlas.internal/ns/test/sa/impostor",
		})
		addr, _ := fx.startServer(t, impostorCertPEM, impostorKeyPEM, fx.ca)
		if _, err := dialTLS(addr, fx.atlasMgr, 5*time.Second); err == nil {
			t.Fatalf("expected rejection: %s identity does not match expected SPIFFE ID", link)
		}
	})

	t.Run("plaintext", func(t *testing.T) {
		fx := newClientRoleFixture(t, link)
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		defer ln.Close()
		go func() {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			defer conn.Close()
			buf := make([]byte, 4096)
			_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
			_, _ = conn.Read(buf) // drain the ClientHello, never speak TLS back
		}()
		if _, err := dialTLS(ln.Addr().String(), fx.atlasMgr, 3*time.Second); err == nil {
			t.Fatalf("expected rejection: %s endpoint does not speak TLS", link)
		}
	})

	t.Run("expired_cert", func(t *testing.T) {
		fx := newClientRoleFixture(t, link)
		expiredCertPEM, expiredKeyPEM, _, _ := fx.ca.issueLeaf(t, link, leafOpts{
			dnsNames:  []string{link + ".internal"},
			spiffeID:  "spiffe://agentatlas.internal/ns/test/sa/" + slug(link),
			notBefore: time.Now().Add(-2 * time.Hour), notAfter: time.Now().Add(-time.Hour),
		})
		addr, _ := fx.startServer(t, expiredCertPEM, expiredKeyPEM, fx.ca)
		if _, err := dialTLS(addr, fx.atlasMgr, 5*time.Second); err == nil {
			t.Fatalf("expected rejection: %s certificate expired", link)
		}
	})

	t.Run("revoked_cert", func(t *testing.T) {
		fx := newClientRoleFixture(t, link)
		addr, _ := fx.startServer(t, fx.depCertPEM, fx.depKeyPEM, fx.ca)
		fx.atlasMgr.SetRevokedSerials(fx.depSerial)
		if _, err := dialTLS(addr, fx.atlasMgr, 5*time.Second); err == nil {
			t.Fatalf("expected rejection: %s certificate is revoked", link)
		}
	})

	t.Run("stale_trust_bundle", func(t *testing.T) {
		fx := newClientRoleFixture(t, link)
		otherCA := newTestCA(t, "other-root-for-"+slug(link))
		newCertPEM, newKeyPEM, _, _ := otherCA.issueLeaf(t, link, leafOpts{
			dnsNames: []string{link + ".internal"},
			spiffeID: "spiffe://agentatlas.internal/ns/test/sa/" + slug(link),
		})
		addr, _ := fx.startServer(t, newCertPEM, newKeyPEM, otherCA)
		if _, err := dialTLS(addr, fx.atlasMgr, 5*time.Second); err == nil {
			t.Fatalf("expected rejection: %s rotated to a CA outside AgentAtlas's (stale) trust bundle", link)
		}
	})
}

// ---------------------------------------------------------------------------
// TestCertificateReloadRebuildsPoolWithoutRevokedIdentity
// ---------------------------------------------------------------------------

func TestCertificateReloadRebuildsPoolWithoutRevokedIdentity(t *testing.T) {
	ca := newTestCA(t, "rotation-root")
	dir := t.TempDir()

	gen1CertPEM, gen1KeyPEM, _, gen1Serial := ca.issueLeaf(t, "AgentNexus", leafOpts{
		dnsNames: []string{"agentnexus.internal"},
		spiffeID: "spiffe://agentatlas.internal/ns/test/sa/agentnexus",
	})
	certPath := writeFile(t, dir, "tls.crt", gen1CertPEM)
	keyPath := writeFile(t, dir, "tls.key", gen1KeyPEM)
	trustPath := writeFile(t, dir, "trust.pem", ca.pem)
	revPath := filepath.Join(dir, "revoked.txt")
	if err := os.WriteFile(revPath, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	serverMgr, err := NewManager("AgentNexus", LinkConfig{
		Mode: ModeMTLS, CertFile: certPath, KeyFile: keyPath, TrustBundleFile: trustPath, RevocationFile: revPath,
		ServerName: "agentatlas.internal", SPIFFEID: "spiffe://agentatlas.internal/ns/test/sa/agentatlas",
	})
	if err != nil {
		t.Fatal(err)
	}

	plainLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ln, err := serverMgr.WrapListener(plainLn)
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	httpSrv := &http.Server{Handler: mux}
	go httpSrv.Serve(ln)
	defer httpSrv.Close()
	addr := ln.Addr().String()

	atlasCertPEM, atlasKeyPEM, _, _ := ca.issueLeaf(t, "AgentAtlas", leafOpts{
		dnsNames: []string{"agentatlas.internal"},
		spiffeID: "spiffe://agentatlas.internal/ns/test/sa/agentatlas",
	})
	atlasMgr := buildManager(t, "AgentNexus", mgrOpts{
		mode: ModeMTLS, certPEM: atlasCertPEM, keyPEM: atlasKeyPEM, trustPEM: ca.pem,
		expectDNS: "agentnexus.internal", expectSPIFFE: "spiffe://agentatlas.internal/ns/test/sa/agentnexus",
	})

	transport := &http.Transport{}
	if err := atlasMgr.ConfigureTransport(transport); err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Transport: transport, Timeout: 5 * time.Second}

	mustGet := func() *http.Response {
		t.Helper()
		resp, err := client.Get("https://" + addr + "/")
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		return resp
	}

	resp := mustGet()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	resp.Body.Close()

	// Rotate the server's leaf (fresh key + cert, same CA) and reload.
	gen2CertPEM, gen2KeyPEM, _, gen2Serial := ca.issueLeaf(t, "AgentNexus", leafOpts{
		dnsNames: []string{"agentnexus.internal"},
		spiffeID: "spiffe://agentatlas.internal/ns/test/sa/agentnexus",
	})
	if err := os.WriteFile(certPath, gen2CertPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, gen2KeyPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := serverMgr.Reload(); err != nil {
		t.Fatalf("reload: %v", err)
	}

	// Force the client's pool to rebuild instead of reusing the
	// pre-rotation connection, then prove the NEW leaf is what gets served.
	transport.CloseIdleConnections()
	resp2 := mustGet()
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("status after rotation = %d", resp2.StatusCode)
	}
	if resp2.TLS == nil || len(resp2.TLS.PeerCertificates) == 0 {
		t.Fatal("no peer certificate observed after rotation")
	}
	gotSerial := fmt.Sprintf("%x", resp2.TLS.PeerCertificates[0].SerialNumber)
	if gotSerial != gen2Serial {
		t.Fatalf("post-rotation serial = %s, want %s (gen1 was %s)", gotSerial, gen2Serial, gen1Serial)
	}
	resp2.Body.Close()

	// Revoke the RETIRED gen-1 identity and prove a stale peer still
	// holding the old (correctly-chained, not-yet-expired) leaf can no
	// longer connect: the rebuilt pool must not accept the revoked
	// identity even though it would otherwise pass chain verification.
	if err := os.WriteFile(revPath, []byte(gen1Serial+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := atlasMgr.Reload(); err != nil {
		t.Fatalf("reload client revocation: %v", err)
	}

	gen1TLSCert, err := tls.X509KeyPair(gen1CertPEM, gen1KeyPEM)
	if err != nil {
		t.Fatal(err)
	}
	staleLn, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{gen1TLSCert}, MinVersion: tls.VersionTLS12})
	if err != nil {
		t.Fatal(err)
	}
	defer staleLn.Close()
	go func() {
		for {
			conn, err := staleLn.Accept()
			if err != nil {
				return
			}
			conn.Close()
		}
	}()
	if _, err := dialTLS(staleLn.Addr().String(), atlasMgr, 3*time.Second); err == nil {
		t.Fatal("expected rejection: the gen-1 identity was revoked after rotation")
	}

	// The revocation must be scoped to gen-1's serial only: the currently
	// active (rotated-to) identity keeps working.
	resp3 := mustGet()
	if resp3.StatusCode != http.StatusOK {
		t.Fatalf("status after revoking the retired identity = %d", resp3.StatusCode)
	}
	resp3.Body.Close()
}

// ---------------------------------------------------------------------------
// TestMTLSFailClosedStartup
// ---------------------------------------------------------------------------

func TestMTLSFailClosedStartup(t *testing.T) {
	ca := newTestCA(t, "startup-root")
	certPEM, keyPEM, _, _ := ca.issueLeaf(t, "svc", leafOpts{
		dnsNames: []string{"svc.internal"},
		spiffeID: "spiffe://agentatlas.internal/ns/test/sa/svc",
	})

	newDir := func(t *testing.T) string { t.Helper(); return t.TempDir() }

	cases := []struct {
		name string
		cfg  func(t *testing.T) LinkConfig
	}{
		{"missing_cert_file", func(t *testing.T) LinkConfig {
			dir := newDir(t)
			return LinkConfig{Mode: ModeMTLS, CertFile: filepath.Join(dir, "missing.crt"), KeyFile: writeFile(t, dir, "tls.key", keyPEM), TrustBundleFile: writeFile(t, dir, "trust.pem", ca.pem), ServerName: "peer.internal"}
		}},
		{"missing_key_file", func(t *testing.T) LinkConfig {
			dir := newDir(t)
			return LinkConfig{Mode: ModeMTLS, CertFile: writeFile(t, dir, "tls.crt", certPEM), KeyFile: filepath.Join(dir, "missing.key"), TrustBundleFile: writeFile(t, dir, "trust.pem", ca.pem), ServerName: "peer.internal"}
		}},
		{"missing_trust_bundle", func(t *testing.T) LinkConfig {
			dir := newDir(t)
			return LinkConfig{Mode: ModeMTLS, CertFile: writeFile(t, dir, "tls.crt", certPEM), KeyFile: writeFile(t, dir, "tls.key", keyPEM), TrustBundleFile: filepath.Join(dir, "missing-trust.pem"), ServerName: "peer.internal"}
		}},
		{"corrupt_cert_pem", func(t *testing.T) LinkConfig {
			dir := newDir(t)
			return LinkConfig{Mode: ModeMTLS, CertFile: writeFile(t, dir, "tls.crt", []byte("not a certificate")), KeyFile: writeFile(t, dir, "tls.key", keyPEM), TrustBundleFile: writeFile(t, dir, "trust.pem", ca.pem), ServerName: "peer.internal"}
		}},
		{"mismatched_key", func(t *testing.T) LinkConfig {
			dir := newDir(t)
			_, otherKeyPEM, _, _ := ca.issueLeaf(t, "other", leafOpts{dnsNames: []string{"other.internal"}})
			return LinkConfig{Mode: ModeMTLS, CertFile: writeFile(t, dir, "tls.crt", certPEM), KeyFile: writeFile(t, dir, "tls.key", otherKeyPEM), TrustBundleFile: writeFile(t, dir, "trust.pem", ca.pem), ServerName: "peer.internal"}
		}},
		{"corrupt_trust_bundle", func(t *testing.T) LinkConfig {
			dir := newDir(t)
			return LinkConfig{Mode: ModeMTLS, CertFile: writeFile(t, dir, "tls.crt", certPEM), KeyFile: writeFile(t, dir, "tls.key", keyPEM), TrustBundleFile: writeFile(t, dir, "trust.pem", []byte("not a trust bundle")), ServerName: "peer.internal"}
		}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := NewManager("test", tc.cfg(t))
			if err == nil {
				t.Fatal("expected a fail-closed startup error, got nil")
			}
			msg := err.Error()
			for _, forbidden := range []string{"PRIVATE KEY", "BEGIN CERTIFICATE"} {
				if strings.Contains(msg, forbidden) {
					t.Fatalf("startup error leaks material: %s", msg)
				}
			}
		})
	}

	t.Run("mode_off_never_fails_closed", func(t *testing.T) {
		if _, err := NewManager("off-link", LinkConfig{Mode: ModeOff, CertFile: "/does/not/exist", KeyFile: "/does/not/exist", TrustBundleFile: "/does/not/exist"}); err != nil {
			t.Fatalf("ModeOff must not fail closed on unusable paths: %v", err)
		}
	})
}

// TestMTLSNewManagerRequiresPeerIdentity is the Manager-level half of the
// MAJOR-1 fail-closed rule (config.Load enforces the same at load time):
// enabling TLS on a link with NO expected peer identity (both ServerName
// and SPIFFEID empty) must fail closed with ErrIdentityRequired, BEFORE any
// cert material is even loaded — otherwise the link would accept any
// chain-verified peer, defeating per-link pinning. The check runs ahead of
// Reload, so it fires even when the (otherwise valid) cert files exist.
func TestMTLSNewManagerRequiresPeerIdentity(t *testing.T) {
	ca := newTestCA(t, "identity-guard-root")
	certPEM, keyPEM, _, _ := ca.issueLeaf(t, "svc", leafOpts{
		dnsNames: []string{"svc.internal"},
		spiffeID: "spiffe://agentatlas.internal/ns/test/sa/svc",
	})
	dir := t.TempDir()
	certPath := writeFile(t, dir, "tls.crt", certPEM)
	keyPath := writeFile(t, dir, "tls.key", keyPEM)
	trustPath := writeFile(t, dir, "trust.pem", ca.pem)

	base := func() LinkConfig {
		return LinkConfig{CertFile: certPath, KeyFile: keyPath, TrustBundleFile: trustPath}
	}

	for _, mode := range []Mode{ModeMTLS, ModeTLS} {
		t.Run(string(mode)+"_without_identity_fails_closed", func(t *testing.T) {
			cfg := base()
			cfg.Mode = mode // no ServerName, no SPIFFEID — valid cert material otherwise
			_, err := NewManager("identityless", cfg)
			if err == nil {
				t.Fatalf("expected %s link with no peer identity to fail closed", mode)
			}
			if !errors.Is(err, ErrIdentityRequired) {
				t.Fatalf("expected ErrIdentityRequired, got: %v", err)
			}
		})
	}

	t.Run("dns_name_alone_is_accepted", func(t *testing.T) {
		cfg := base()
		cfg.Mode = ModeMTLS
		cfg.ServerName = "peer.internal"
		if _, err := NewManager("dns-only", cfg); err != nil {
			t.Fatalf("a DNS-name-only identity should satisfy the rule: %v", err)
		}
	})

	t.Run("spiffe_id_alone_is_accepted", func(t *testing.T) {
		cfg := base()
		cfg.Mode = ModeTLS
		cfg.SPIFFEID = "spiffe://agentatlas.internal/ns/test/sa/peer"
		if _, err := NewManager("spiffe-only", cfg); err != nil {
			t.Fatalf("a SPIFFE-id-only identity should satisfy the rule: %v", err)
		}
	})

	t.Run("mode_off_never_needs_identity", func(t *testing.T) {
		if _, err := NewManager("off", LinkConfig{Mode: ModeOff}); err != nil {
			t.Fatalf("ModeOff must not require an identity: %v", err)
		}
	})
}

// ---------------------------------------------------------------------------
// TestMTLSStatusDoesNotLeakKeyMaterial
// ---------------------------------------------------------------------------

func TestMTLSStatusDoesNotLeakKeyMaterial(t *testing.T) {
	ca := newTestCA(t, "status-root")
	certPEM, keyPEM, cert, _ := ca.issueLeaf(t, "svc", leafOpts{
		dnsNames: []string{"svc.internal"},
		spiffeID: "spiffe://agentatlas.internal/ns/test/sa/svc",
	})
	mgr := buildManager(t, "svc", mgrOpts{
		mode: ModeMTLS, certPEM: certPEM, keyPEM: keyPEM, trustPEM: ca.pem,
		expectDNS: "peer.internal", expectSPIFFE: "spiffe://agentatlas.internal/ns/test/sa/peer",
	})

	st := mgr.Status()
	if !st.Ready {
		t.Fatalf("expected ready status: %+v", st)
	}
	if !st.NotAfter.Equal(cert.NotAfter) {
		t.Fatalf("NotAfter = %v, want %v", st.NotAfter, cert.NotAfter)
	}
	blob := fmt.Sprintf("%+v", st)
	for _, forbidden := range []string{"PRIVATE KEY", string(keyPEM)} {
		if strings.Contains(blob, forbidden) {
			t.Fatalf("status leaks key material: %s", blob)
		}
	}

	// Break the certificate file, reload, and confirm Status distinguishes
	// a certificate problem while the Manager keeps serving the
	// last-known-good certificate (overlapping-rotation semantics) —
	// still without leaking material.
	dir := t.TempDir()
	certPath := writeFile(t, dir, "tls.crt", certPEM)
	keyPath := writeFile(t, dir, "tls.key", keyPEM)
	trustPath := writeFile(t, dir, "trust.pem", ca.pem)
	reloadable, err := NewManager("svc2", LinkConfig{
		Mode: ModeMTLS, CertFile: certPath, KeyFile: keyPath, TrustBundleFile: trustPath,
		ServerName: "peer.internal", SPIFFEID: "spiffe://agentatlas.internal/ns/test/sa/peer",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(certPath, []byte("not a certificate"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := reloadable.Reload(); err == nil {
		t.Fatal("expected Reload to report the corrupt certificate")
	}
	st2 := reloadable.Status()
	if !st2.Ready {
		t.Fatalf("Manager must keep serving the last-known-good certificate after a failed reload: %+v", st2)
	}
	if st2.Detail == "" {
		t.Fatal("Status.Detail must explain the failed reload")
	}
	if strings.Contains(st2.Detail, "PRIVATE KEY") {
		t.Fatalf("Status.Detail leaks key material: %s", st2.Detail)
	}
	cfg, err := reloadable.ServerTLSConfig()
	if err != nil {
		t.Fatal(err)
	}
	gotCert, err := cfg.GetCertificate(&tls.ClientHelloInfo{})
	if err != nil {
		t.Fatalf("GetCertificate after failed reload: %v", err)
	}
	if gotCert == nil {
		t.Fatal("expected the last-known-good certificate to still be served after a failed reload")
	}
}
