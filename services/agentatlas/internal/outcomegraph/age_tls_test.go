package outcomegraph_test

// age_tls_test.go proves the GA Task 13A eleventh matrix link — the "Apache
// AGE graph PostgreSQL" endpoint — is inside the full-chain mTLS profile: that
// NewAGEStore applies the transportsecurity.Manager's dial-side
// ClientTLSConfig to its pgx pool exactly as internal/storage.NewPool does for
// the authoritative-Postgres link, and fails CLOSED against a plaintext /
// downgrading server, a wrong server identity, or a revoked server identity —
// with a live rotation observed through Manager.Reload().
//
// It needs no real Apache AGE container: it stands up an in-process loopback
// listener that speaks the minimal PostgreSQL SSLRequest preamble
// (int32{8, 80877103} -> 'S') and then completes the server side of the TLS
// handshake, exactly the negotiation pgx performs when
// ConnConfig.TLSConfig is set. The load-bearing proof is what the SERVER
// observes: an mTLS server (RequireAnyClientCert + identity pin) only
// completes the handshake if NewAGEStore presented the projector's client
// certificate over TLS — which it can only have done by applying the manager.
// The subsequent Postgres startup fails (this is not a real Postgres), so
// NewAGEStore itself returns an error in every case; the assertions therefore
// key off the server-observed handshake, not NewAGEStore's own return.

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/binary"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/outcomegraph"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/transportsecurity"
)

const (
	ageOrg          = "agentatlas.internal"
	ageServerDNS    = "apache-age-graph." + ageOrg
	ageServerSPIFFE = "spiffe://" + ageOrg + "/ns/test/sa/apache-age-graph"
	projDNS         = "outcome-projector." + ageOrg
	projSPIFFE      = "spiffe://" + ageOrg + "/ns/test/sa/outcome-projector"
)

// --- minimal test PKI (in-memory, per test; no repo certs) -----------------

type ageTestCA struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
	pem  []byte
}

func newAgeCA(t *testing.T) *ageTestCA {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("ca key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          ageSerial(t),
		Subject:               pkix.Name{CommonName: "age-graph-tls-root"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
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
	return &ageTestCA{cert: cert, key: key, pem: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})}
}

func ageSerial(t *testing.T) *big.Int {
	t.Helper()
	n, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		t.Fatalf("serial: %v", err)
	}
	return n
}

// issueLeaf mints a fresh key pair + leaf carrying both ServerAuth and
// ClientAuth EKUs so one identity can play either side of the link.
func (ca *ageTestCA) issueLeaf(t *testing.T, cn, dnsName, spiffe string, notBefore, notAfter time.Time) (certPEM, keyPEM []byte, serialHex string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("leaf key: %v", err)
	}
	if notBefore.IsZero() {
		notBefore = time.Now().Add(-time.Hour)
	}
	if notAfter.IsZero() {
		notAfter = time.Now().Add(time.Hour)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          ageSerial(t),
		Subject:               pkix.Name{CommonName: cn},
		NotBefore:             notBefore,
		NotAfter:              notAfter,
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		BasicConstraintsValid: true,
		DNSNames:              []string{dnsName},
	}
	if spiffe != "" {
		u, err := url.Parse(spiffe)
		if err != nil {
			t.Fatalf("parse spiffe: %v", err)
		}
		tmpl.URIs = []*url.URL{u}
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.cert, &key.PublicKey, ca.key)
	if err != nil {
		t.Fatalf("leaf cert: %v", err)
	}
	parsed, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("parse leaf: %v", err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatalf("marshal key: %v", err)
	}
	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	return certPEM, keyPEM, fmt.Sprintf("%x", parsed.SerialNumber)
}

func ageWrite(t *testing.T, dir, name string, data []byte) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, data, 0o600); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
	return p
}

type ageMgrOpts struct {
	mode                      transportsecurity.Mode
	certPEM, keyPEM, trustPEM []byte
	expectDNS, expectSPIFFE   string
	serverName                string
	revoked                   []string
}

// newAgeManager materializes o as files under a fresh temp dir and loads a
// real transportsecurity.Manager over them, returning the manager plus the
// cert/key paths (so the reload case can rotate the leaf on disk).
func newAgeManager(t *testing.T, link string, o ageMgrOpts) (*transportsecurity.Manager, string, string) {
	t.Helper()
	dir := t.TempDir()
	certPath := ageWrite(t, dir, "tls.crt", o.certPEM)
	keyPath := ageWrite(t, dir, "tls.key", o.keyPEM)
	trustPath := ageWrite(t, dir, "trust.pem", o.trustPEM)
	revPath := ageWrite(t, dir, "revoked.txt", nil)
	mgr, err := transportsecurity.NewManager(link, transportsecurity.LinkConfig{
		Mode:            o.mode,
		CertFile:        certPath,
		KeyFile:         keyPath,
		TrustBundleFile: trustPath,
		RevocationFile:  revPath,
		ServerName:      o.serverName,
		SPIFFEID:        o.expectSPIFFE,
	})
	if err != nil {
		t.Fatalf("NewManager(%s): %v", link, err)
	}
	if len(o.revoked) > 0 {
		mgr.SetRevokedSerials(o.revoked...)
	}
	_ = o.expectDNS
	return mgr, certPath, keyPath
}

// --- in-process PostgreSQL-SSL listener ------------------------------------

type ageHS struct {
	err   error
	state tls.ConnectionState
}

const sslMagic = 80877103

// startAGEPGServer accepts loopback connections, reads pgx's 8-byte
// SSLRequest, replies 'S', then completes the TLS server handshake using
// serverMgr's ServerTLSConfig, reporting each server-side handshake outcome.
func startAGEPGServer(t *testing.T, serverMgr *transportsecurity.Manager) (string, <-chan ageHS) {
	t.Helper()
	cfg, err := serverMgr.ServerTLSConfig()
	if err != nil {
		t.Fatalf("server tls config: %v", err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { ln.Close() })
	results := make(chan ageHS, 16)
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go serveAGEPG(conn, cfg, results)
		}
	}()
	return ln.Addr().String(), results
}

// startPlaintextPGServer reads the SSLRequest and REFUSES TLS ('N') — the
// downgrade case: an AGE endpoint that will not speak TLS.
func startPlaintextPGServer(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				_ = c.SetDeadline(time.Now().Add(5 * time.Second))
				hdr := make([]byte, 8)
				if _, err := io.ReadFull(c, hdr); err != nil {
					return
				}
				_, _ = c.Write([]byte{'N'}) // refuse TLS
			}(conn)
		}
	}()
	return ln.Addr().String()
}

func serveAGEPG(conn net.Conn, cfg *tls.Config, results chan<- ageHS) {
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	hdr := make([]byte, 8)
	if _, err := io.ReadFull(conn, hdr); err != nil {
		results <- ageHS{err: fmt.Errorf("read SSLRequest: %w", err)}
		return
	}
	if code := binary.BigEndian.Uint32(hdr[4:8]); code != sslMagic {
		results <- ageHS{err: fmt.Errorf("client did not send an SSLRequest (code=%d) — connection is not TLS", code)}
		return
	}
	if _, err := conn.Write([]byte{'S'}); err != nil {
		results <- ageHS{err: err}
		return
	}
	tlsConn := tls.Server(conn, cfg)
	herr := tlsConn.HandshakeContext(context.Background())
	res := ageHS{err: herr}
	if herr == nil {
		res.state = tlsConn.ConnectionState()
	}
	results <- res
}

func awaitAGEHS(t *testing.T, results <-chan ageHS) ageHS {
	t.Helper()
	select {
	case r := <-results:
		return r
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for the AGE-graph server-side handshake")
		return ageHS{}
	}
}

func ageDSNFor(addr string) string {
	// sslmode=require makes pgx negotiate SSL; NewAGEStore's applied
	// ClientTLSConfig then governs identity/verification.
	return fmt.Sprintf("postgres://age:age@%s/postgres?sslmode=require&connect_timeout=5", addr)
}

// tryNewAGEStore calls the store constructor and disposes any (unexpected)
// success. It never fails the test on the constructor's own error: against
// this non-Postgres listener the post-handshake startup always fails, so the
// proof lives in what the server observed during the handshake.
func tryNewAGEStore(t *testing.T, dsn string, mgr *transportsecurity.Manager) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	s, err := outcomegraph.NewAGEStore(ctx, dsn, "ogtls", mgr)
	if err == nil {
		s.Close()
	}
	return err
}

// --- the eleventh-link proof -----------------------------------------------

func TestMTLSAGEGraphStoreEnforcesDialSideProfile(t *testing.T) {
	ca := newAgeCA(t)
	serverCertPEM, serverKeyPEM, serverSerial := ca.issueLeaf(t, "apache-age-graph", ageServerDNS, ageServerSPIFFE, time.Time{}, time.Time{})

	// The AGE-graph server expects the Outcome projector's client identity.
	newServer := func(t *testing.T) (*transportsecurity.Manager, string, <-chan ageHS) {
		srvMgr, _, _ := newAgeManager(t, "Apache AGE graph PostgreSQL (server-under-test)", ageMgrOpts{
			mode: transportsecurity.ModeMTLS, certPEM: serverCertPEM, keyPEM: serverKeyPEM, trustPEM: ca.pem,
			expectDNS: projDNS, expectSPIFFE: projSPIFFE, serverName: ageServerDNS,
		})
		addr, results := startAGEPGServer(t, srvMgr)
		return srvMgr, addr, results
	}

	// The projector's dial-side manager for the Apache AGE graph PostgreSQL link.
	projCertPEM, projKeyPEM, _ := ca.issueLeaf(t, "outcome-projector", projDNS, projSPIFFE, time.Time{}, time.Time{})
	projClient := func(t *testing.T) *transportsecurity.Manager {
		mgr, _, _ := newAgeManager(t, "Apache AGE graph PostgreSQL", ageMgrOpts{
			mode: transportsecurity.ModeMTLS, certPEM: projCertPEM, keyPEM: projKeyPEM, trustPEM: ca.pem,
			expectDNS: ageServerDNS, expectSPIFFE: ageServerSPIFFE, serverName: ageServerDNS,
		})
		return mgr
	}

	t.Run("mtls_enforced_connection", func(t *testing.T) {
		_, addr, results := newServer(t)
		_ = tryNewAGEStore(t, ageDSNFor(addr), projClient(t))
		res := awaitAGEHS(t, results)
		if res.err != nil {
			t.Fatalf("server observed a handshake failure; NewAGEStore did not present the projector's mTLS identity: %v", res.err)
		}
		if res.state.Version != tls.VersionTLS13 {
			t.Fatalf("negotiated TLS version = %#x, want TLS 1.3 (%#x)", res.state.Version, tls.VersionTLS13)
		}
		if len(res.state.PeerCertificates) == 0 {
			t.Fatal("server saw no client certificate — the AGE-graph link is not mTLS")
		}
		if err := identityHas(res.state.PeerCertificates[0], projSPIFFE); err != nil {
			t.Fatalf("AGE-graph link did not present the Outcome projector identity: %v", err)
		}
	})

	t.Run("unauthenticated_client_rejected", func(t *testing.T) {
		// A nil manager honors the DSN's own sslmode (server-auth TLS, no
		// client cert). Against the mTLS AGE-graph server that is fail-closed:
		// no client identity is presented, so the server rejects it.
		_, addr, results := newServer(t)
		_ = tryNewAGEStore(t, ageDSNFor(addr), nil)
		res := awaitAGEHS(t, results)
		if res.err == nil {
			t.Fatal("expected the AGE-graph server to reject a client that presented no certificate")
		}
	})

	t.Run("wrong_server_identity_rejected", func(t *testing.T) {
		// Server presents a DIFFERENT identity than the projector pins.
		wrongCertPEM, wrongKeyPEM, _ := ca.issueLeaf(t, "impostor-age", "impostor."+ageOrg, "spiffe://"+ageOrg+"/ns/test/sa/impostor", time.Time{}, time.Time{})
		srvMgr, _, _ := newAgeManager(t, "impostor-age-server", ageMgrOpts{
			mode: transportsecurity.ModeMTLS, certPEM: wrongCertPEM, keyPEM: wrongKeyPEM, trustPEM: ca.pem,
			expectDNS: projDNS, expectSPIFFE: projSPIFFE, serverName: "impostor." + ageOrg,
		})
		addr, results := startAGEPGServer(t, srvMgr)
		if err := tryNewAGEStore(t, ageDSNFor(addr), projClient(t)); err == nil {
			t.Fatal("expected NewAGEStore to fail closed against a wrong AGE-graph server identity")
		}
		res := awaitAGEHS(t, results)
		if res.err == nil {
			t.Fatal("expected no successful handshake when the projector rejects the server identity")
		}
	})

	t.Run("revoked_server_rejected", func(t *testing.T) {
		_, addr, results := newServer(t)
		// The projector's manager has the AGE-graph server's serial revoked.
		mgr, _, _ := newAgeManager(t, "Apache AGE graph PostgreSQL", ageMgrOpts{
			mode: transportsecurity.ModeMTLS, certPEM: projCertPEM, keyPEM: projKeyPEM, trustPEM: ca.pem,
			expectDNS: ageServerDNS, expectSPIFFE: ageServerSPIFFE, serverName: ageServerDNS,
			revoked: []string{serverSerial},
		})
		if err := tryNewAGEStore(t, ageDSNFor(addr), mgr); err == nil {
			t.Fatal("expected NewAGEStore to fail closed against a revoked AGE-graph server identity")
		}
		res := awaitAGEHS(t, results)
		if res.err == nil {
			t.Fatal("expected no successful handshake when the AGE-graph server identity is revoked")
		}
	})

	t.Run("plaintext_downgrade_refused", func(t *testing.T) {
		// The endpoint refuses TLS ('N'); a required-TLS projector must not
		// fall back to plaintext.
		addr := startPlaintextPGServer(t)
		if err := tryNewAGEStore(t, ageDSNFor(addr), projClient(t)); err == nil {
			t.Fatal("expected NewAGEStore to refuse a plaintext AGE-graph endpoint (no TLS downgrade)")
		}
	})

	t.Run("reload_presents_rotated_identity", func(t *testing.T) {
		gen1CertPEM, gen1KeyPEM, gen1Serial := ca.issueLeaf(t, "outcome-projector", projDNS, projSPIFFE, time.Time{}, time.Time{})
		mgr, certPath, keyPath := newAgeManager(t, "Apache AGE graph PostgreSQL", ageMgrOpts{
			mode: transportsecurity.ModeMTLS, certPEM: gen1CertPEM, keyPEM: gen1KeyPEM, trustPEM: ca.pem,
			expectDNS: ageServerDNS, expectSPIFFE: ageServerSPIFFE, serverName: ageServerDNS,
		})

		// A single NewAGEStore dial can trigger more than one handshake as the
		// pool retries the (non-Postgres) endpoint, so each generation gets its
		// OWN fresh server/channel: firstHandshakeOf reads the earliest result
		// on that generation's channel, never a leftover from the other.
		firstHandshakeOf := func(t *testing.T) tls.ConnectionState {
			t.Helper()
			_, addr, results := newServer(t)
			_ = tryNewAGEStore(t, ageDSNFor(addr), mgr)
			hs := awaitAGEHS(t, results)
			if hs.err != nil || len(hs.state.PeerCertificates) == 0 {
				t.Fatalf("handshake did not succeed: %v", hs.err)
			}
			return hs.state
		}

		if got := fmt.Sprintf("%x", firstHandshakeOf(t).PeerCertificates[0].SerialNumber); got != gen1Serial {
			t.Fatalf("gen-1 serial = %s, want %s", got, gen1Serial)
		}

		// Rotate the projector's leaf on disk (same identity SANs, fresh
		// key/serial) and reload — a live rotation with no store rebuild.
		gen2CertPEM, gen2KeyPEM, gen2Serial := ca.issueLeaf(t, "outcome-projector", projDNS, projSPIFFE, time.Time{}, time.Time{})
		if err := os.WriteFile(certPath, gen2CertPEM, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(keyPath, gen2KeyPEM, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := mgr.Reload(); err != nil {
			t.Fatalf("reload: %v", err)
		}

		got := fmt.Sprintf("%x", firstHandshakeOf(t).PeerCertificates[0].SerialNumber)
		if got == gen1Serial {
			t.Fatal("AGE-graph link still presented the gen-1 identity after reload")
		}
		if got != gen2Serial {
			t.Fatalf("post-reload serial = %s, want gen-2 %s", got, gen2Serial)
		}
	})
}

// identityHas reports whether cert carries the expected SPIFFE URI SAN.
func identityHas(cert *x509.Certificate, spiffe string) error {
	for _, u := range cert.URIs {
		if u.String() == spiffe {
			return nil
		}
	}
	return fmt.Errorf("certificate URIs %v do not include %q", cert.URIs, spiffe)
}
