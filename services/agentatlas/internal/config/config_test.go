package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAgentNexusServiceCredentialConfigDefaultsAndEnvironment(t *testing.T) {
	cfg := Default()
	if cfg.AgentNexus.ClientID != "agentatlas" || cfg.AgentNexus.SecretFile != "/run/secrets/agentatlas_agentnexus_service_secret" {
		t.Fatalf("AgentNexus defaults=%+v", cfg.AgentNexus)
	}
	if cfg.AgentNexus.BrowserClientID != "agentatlas" || cfg.AgentNexus.BrowserClientSecretFile == "" || cfg.AgentNexus.BrowserSessionEncryptionKeyFile == "" || cfg.AgentNexus.AtlasPublicURL == "" {
		t.Fatalf("browser BFF defaults=%+v", cfg.AgentNexus)
	}
	if cfg.AgentNexus.BrowserSessionEncryptionKeyID == "" || cfg.AgentNexus.BrowserSessionPreviousEncryptionKeyID == "" {
		t.Fatalf("browser key IDs missing=%+v", cfg.AgentNexus)
	}
	t.Setenv("ATLAS_NEXUS_CLIENT_ID", "agentatlas")
	t.Setenv("ATLAS_NEXUS_SERVICE_SECRET_FILE", "/run/secrets/custom_agentnexus_secret")
	cfg, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.AgentNexus.ClientID != "agentatlas" || cfg.AgentNexus.SecretFile != "/run/secrets/custom_agentnexus_secret" {
		t.Fatalf("AgentNexus env config=%+v", cfg.AgentNexus)
	}
}

func TestDeploymentProfilesReferenceAgentNexusSecretFilesOnly(t *testing.T) {
	root := filepath.Join("..", "..")
	compose := readDeploymentFile(t, filepath.Join(root, "deploy", "compose", "compose.yaml"))
	for _, required := range []string{"ATLAS_NEXUS_CLIENT_ID: agentatlas", "ATLAS_NEXUS_SERVICE_SECRET_FILE: /run/secrets/agentatlas_agentnexus_service_secret", "agentatlas_agentnexus_service_secret:", "ATLAS_NEXUS_SERVICE_SECRET_FILE_SOURCE", "ATLAS_NEXUS_BROWSER_CLIENT_SECRET_FILE", "ATLAS_BROWSER_SESSION_ENCRYPTION_KEY_ID", "ATLAS_BROWSER_SESSION_ENCRYPTION_KEY_FILE", "ATLAS_PUBLIC_URL", "agentatlas_browser_client_secret:", "agentatlas_browser_session_key:"} {
		if !strings.Contains(compose, required) {
			t.Errorf("Compose missing %q", required)
		}
	}
	rotation := readDeploymentFile(t, filepath.Join(root, "deploy", "compose", "compose.rotation.yaml"))
	for _, required := range []string{"ATLAS_BROWSER_SESSION_PREVIOUS_ENCRYPTION_KEY_ID", "ATLAS_BROWSER_SESSION_PREVIOUS_ENCRYPTION_KEY_FILE", "agentatlas_browser_session_previous_key:", "ATLAS_BROWSER_SESSION_PREVIOUS_ENCRYPTION_KEY_FILE_SOURCE"} {
		if !strings.Contains(rotation, required) {
			t.Errorf("Compose rotation override missing %q", required)
		}
	}
	values := readDeploymentFile(t, filepath.Join(root, "deploy", "helm", "agentatlas", "values.yaml"))
	for _, required := range []string{"nexusClientId: agentatlas", "nexusServiceSecretName:", "nexusServiceSecretKey:", "nexusBrowserClientSecretKey:", "browserSessionEncryptionKeyId:", "browserSessionEncryptionKey:", "browserSessionPreviousEncryptionKeyId:", "browserSessionPreviousEncryptionKey:", "atlasPublicUrl:"} {
		if !strings.Contains(values, required) {
			t.Errorf("Helm values missing %q", required)
		}
	}
	helpers := readDeploymentFile(t, filepath.Join(root, "deploy", "helm", "agentatlas", "templates", "_helpers.tpl"))
	for _, required := range []string{"ATLAS_NEXUS_SERVICE_SECRET_FILE", "/var/run/secrets/agentnexus/client-secret", ".Values.config.nexusServiceSecretName", ".Values.config.nexusServiceSecretKey"} {
		if !strings.Contains(helpers, required) {
			t.Errorf("Helm helpers missing %q", required)
		}
	}
	for _, forbidden := range []string{"ATLAS_NEXUS_SERVICE_SECRET:", "nexusServiceSecretValue:"} {
		if strings.Contains(compose, forbidden) || strings.Contains(values, forbidden) || strings.Contains(helpers, forbidden) {
			t.Errorf("deployment embeds raw AgentNexus secret setting %q", forbidden)
		}
	}
}

func TestTLSDefaultsToOffOnEveryLink(t *testing.T) {
	cfg := Default()
	for _, link := range cfg.tlsLinkEnvs() {
		if link.cfg.Mode != TLSModeOff {
			t.Fatalf("%s: default mode = %q, want %q", link.prefix, link.cfg.Mode, TLSModeOff)
		}
		if link.cfg.CertFile != "" || link.cfg.KeyFile != "" || link.cfg.TrustBundleFile != "" {
			t.Fatalf("%s: default carries unexpected file paths: %+v", link.prefix, link.cfg)
		}
	}
}

// tlsLinkPrefixes is the fixed set of per-link env prefixes; tests that
// enable TLS globally use it to also pin an identity per link (required
// since the MAJOR-1 fail-closed rule rejects an enabled link with no
// identity).
var tlsLinkPrefixes = []string{
	"ATLAS_TLS_AGENTATLAS_", "ATLAS_TLS_GATEWAY_", "ATLAS_TLS_AGENTNEXUS_",
	"ATLAS_TLS_LLMROUTER_", "ATLAS_TLS_POSTGRES_", "ATLAS_TLS_OPENSEARCH_",
	"ATLAS_TLS_NATS_", "ATLAS_TLS_OBJECT_STORAGE_", "ATLAS_TLS_PARSER_",
	"ATLAS_TLS_AGE_GRAPH_",
}

func setEveryLinkIdentity(t *testing.T) {
	t.Helper()
	for _, prefix := range tlsLinkPrefixes {
		t.Setenv(prefix+"SPIFFE_ID", "spiffe://agentatlas.internal/ns/prod/sa/peer")
	}
}

func TestTLSGlobalModeSeedsEveryLinkAndPerLinkOverridesWin(t *testing.T) {
	t.Setenv("ATLAS_TLS_MODE", "mtls")
	setEveryLinkIdentity(t) // fail-closed rule: an enabled link must pin an identity
	cfg, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	for _, link := range cfg.tlsLinkEnvs() {
		if link.cfg.Mode != TLSModeMTLS {
			t.Fatalf("%s: mode = %q, want %q after ATLAS_TLS_MODE=mtls", link.prefix, link.cfg.Mode, TLSModeMTLS)
		}
	}
	if cfg.TLS.AgentNexus.Mode != TLSModeMTLS {
		t.Fatalf("AgentNexus mode = %q", cfg.TLS.AgentNexus.Mode)
	}

	// A per-link override wins over the global default.
	t.Setenv("ATLAS_TLS_AGENTNEXUS_MODE", "tls")
	cfg, err = Load("")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.TLS.AgentNexus.Mode != TLSModeTLS {
		t.Fatalf("AgentNexus mode = %q, want override %q", cfg.TLS.AgentNexus.Mode, TLSModeTLS)
	}
	if cfg.TLS.Postgres.Mode != TLSModeMTLS {
		t.Fatalf("Postgres mode = %q, want the un-overridden global %q", cfg.TLS.Postgres.Mode, TLSModeMTLS)
	}
}

// TestTLSEnabledLinkRequiresIdentity is the config-layer half of the
// MAJOR-1 fail-closed rule (internal/transportsecurity.NewManager enforces
// the same at composition time): enabling TLS on a link with no
// server_name AND no spiffe_id must be rejected at Load time, because such
// a link would accept any chain-verified peer.
func TestTLSEnabledLinkRequiresIdentity(t *testing.T) {
	for _, mode := range []string{"mtls", "tls"} {
		t.Run(mode+"_without_identity_rejected", func(t *testing.T) {
			t.Setenv("ATLAS_TLS_AGENTNEXUS_MODE", mode)
			// no SERVER_NAME, no SPIFFE_ID
			if _, err := Load(""); err == nil {
				t.Fatalf("expected Load to reject %s link with no pinned identity", mode)
			}
		})
	}

	t.Run("server_name_alone_is_accepted", func(t *testing.T) {
		t.Setenv("ATLAS_TLS_AGENTNEXUS_MODE", "mtls")
		t.Setenv("ATLAS_TLS_AGENTNEXUS_SERVER_NAME", "agentnexus.internal")
		if _, err := Load(""); err != nil {
			t.Fatalf("a DNS-name-only identity should satisfy the rule: %v", err)
		}
	})

	t.Run("spiffe_id_alone_is_accepted", func(t *testing.T) {
		t.Setenv("ATLAS_TLS_AGENTNEXUS_MODE", "tls")
		t.Setenv("ATLAS_TLS_AGENTNEXUS_SPIFFE_ID", "spiffe://agentatlas.internal/ns/prod/sa/agentnexus")
		if _, err := Load(""); err != nil {
			t.Fatalf("a SPIFFE-id-only identity should satisfy the rule: %v", err)
		}
	})

	t.Run("off_link_never_needs_identity", func(t *testing.T) {
		// The default config has every link off and no identities; it must
		// keep loading cleanly (backward compatibility).
		if _, err := Load(""); err != nil {
			t.Fatalf("all-off default config must load: %v", err)
		}
	})
}

// TestEveryDeployLinkPinsAnIdentity guards against a deploy-manifest
// regression: every link the wired deploy surfaces enable must carry an
// identity, or the MAJOR-1 rule would fail-closed at startup. It re-derives
// the identities from the same values the compose env block and Helm values
// set (server_name/spiffe_id per link) and asserts the fail-closed
// validation passes for a fully-enabled mtls posture.
func TestEveryDeployLinkPinsAnIdentity(t *testing.T) {
	t.Setenv("ATLAS_TLS_MODE", "mtls")
	// The identities below mirror deploy/compose/compose.yaml's
	// ATLAS_TLS_<LINK>_SERVER_NAME / _SPIFFE_ID and
	// deploy/helm/agentatlas/values.yaml's tls.links[].serverName/spiffeId.
	identities := map[string][2]string{
		"ATLAS_TLS_AGENTATLAS_":     {"caller.agentatlas.internal", "spiffe://agentatlas.internal/ns/prod/sa/caller-of-agentatlas"},
		"ATLAS_TLS_GATEWAY_":        {"caller.gateway.internal", "spiffe://agentatlas.internal/ns/prod/sa/caller-of-gateway"},
		"ATLAS_TLS_AGENTNEXUS_":     {"agentnexus.internal", "spiffe://agentatlas.internal/ns/prod/sa/agentnexus"},
		"ATLAS_TLS_LLMROUTER_":      {"llmrouter.internal", "spiffe://agentatlas.internal/ns/prod/sa/llmrouter"},
		"ATLAS_TLS_POSTGRES_":       {"postgresql.internal", "spiffe://agentatlas.internal/ns/prod/sa/postgresql"},
		"ATLAS_TLS_OPENSEARCH_":     {"opensearch.internal", "spiffe://agentatlas.internal/ns/prod/sa/opensearch"},
		"ATLAS_TLS_NATS_":           {"nats.internal", "spiffe://agentatlas.internal/ns/prod/sa/nats"},
		"ATLAS_TLS_OBJECT_STORAGE_": {"object-storage.internal", "spiffe://agentatlas.internal/ns/prod/sa/object-storage"},
		"ATLAS_TLS_PARSER_":         {"parser.internal", "spiffe://agentatlas.internal/ns/prod/sa/parser"},
		// The Apache AGE graph PostgreSQL link (GA Task 13A's eleventh link).
		// Deployment manifests wire it in Task 14/15; this identity is the
		// code-level shape the fail-closed rule requires once the link is on.
		"ATLAS_TLS_AGE_GRAPH_": {"apache-age-graph.internal", "spiffe://agentatlas.internal/ns/prod/sa/apache-age-graph"},
	}
	for prefix, id := range identities {
		t.Setenv(prefix+"SERVER_NAME", id[0])
		t.Setenv(prefix+"SPIFFE_ID", id[1])
	}
	cfg, err := Load("")
	if err != nil {
		t.Fatalf("fully-enabled mtls posture with the wired deploy identities must load: %v", err)
	}
	for _, link := range cfg.tlsLinkEnvs() {
		if link.cfg.ServerName == "" && link.cfg.SPIFFEID == "" {
			t.Fatalf("%s: enabled deploy link pins no identity", link.prefix)
		}
	}
}

func TestTLSPerLinkFileAndIdentityEnvOverrides(t *testing.T) {
	t.Setenv("ATLAS_TLS_AGENTNEXUS_MODE", "mtls")
	t.Setenv("ATLAS_TLS_AGENTNEXUS_CERT_FILE", "/run/secrets/agentatlas_tls/agentnexus/tls.crt")
	t.Setenv("ATLAS_TLS_AGENTNEXUS_KEY_FILE", "/run/secrets/agentatlas_tls/agentnexus/tls.key")
	t.Setenv("ATLAS_TLS_AGENTNEXUS_TRUST_BUNDLE_FILE", "/run/secrets/agentatlas_tls/agentnexus/ca.crt")
	t.Setenv("ATLAS_TLS_AGENTNEXUS_REVOCATION_FILE", "/run/secrets/agentatlas_tls/agentnexus/revoked.txt")
	t.Setenv("ATLAS_TLS_AGENTNEXUS_SERVER_NAME", "agentnexus.internal")
	t.Setenv("ATLAS_TLS_AGENTNEXUS_SPIFFE_ID", "spiffe://agentatlas.internal/ns/prod/sa/agentnexus")

	cfg, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	got := cfg.TLS.AgentNexus
	want := LinkTLS{
		Mode:            TLSModeMTLS,
		CertFile:        "/run/secrets/agentatlas_tls/agentnexus/tls.crt",
		KeyFile:         "/run/secrets/agentatlas_tls/agentnexus/tls.key",
		TrustBundleFile: "/run/secrets/agentatlas_tls/agentnexus/ca.crt",
		RevocationFile:  "/run/secrets/agentatlas_tls/agentnexus/revoked.txt",
		ServerName:      "agentnexus.internal",
		SPIFFEID:        "spiffe://agentatlas.internal/ns/prod/sa/agentnexus",
	}
	if got != want {
		t.Fatalf("AgentNexus TLS config = %+v, want %+v", got, want)
	}
	// Every other link is untouched.
	if cfg.TLS.Postgres.Mode != TLSModeOff || cfg.TLS.Postgres.CertFile != "" {
		t.Fatalf("Postgres TLS config leaked AgentNexus overrides: %+v", cfg.TLS.Postgres)
	}
}

func TestTLSConfigYAMLRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "atlas.yaml")
	yamlBody := "tls:\n" +
		"  agentnexus:\n" +
		"    mode: mtls\n" +
		"    cert_file: /etc/agentatlas/tls/agentnexus/tls.crt\n" +
		"    key_file: /etc/agentatlas/tls/agentnexus/tls.key\n" +
		"    trust_bundle_file: /etc/agentatlas/tls/agentnexus/ca.crt\n" +
		"    server_name: agentnexus.internal\n" +
		"    spiffe_id: spiffe://agentatlas.internal/ns/prod/sa/agentnexus\n"
	if err := os.WriteFile(path, []byte(yamlBody), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.TLS.AgentNexus.Mode != TLSModeMTLS || cfg.TLS.AgentNexus.CertFile != "/etc/agentatlas/tls/agentnexus/tls.crt" {
		t.Fatalf("TLS config not loaded from YAML: %+v", cfg.TLS.AgentNexus)
	}
	// Every field not set in this YAML file stays at its (off) default —
	// existing config files without a tls: block keep working unchanged.
	if cfg.TLS.NATS.Mode != TLSModeOff {
		t.Fatalf("NATS mode = %q, want default %q for a link absent from the YAML file", cfg.TLS.NATS.Mode, TLSModeOff)
	}
	if cfg.Postgres.DSN == "" {
		t.Fatal("unrelated existing config (Postgres DSN) should still carry its default")
	}
}

// TestTLSAgeGraphLinkParsesLikeOtherLinks covers the GA Task 13A eleventh
// link (the Apache AGE graph PostgreSQL endpoint the atlas-outcome-projector
// dials): it defaults to off, honors the same per-link env/file/identity
// overrides as every other link, round-trips through YAML, and stays isolated
// from the authoritative Postgres link.
func TestTLSAgeGraphLinkParsesLikeOtherLinks(t *testing.T) {
	t.Run("defaults_off", func(t *testing.T) {
		cfg := Default()
		if cfg.TLS.AgeGraph.Mode != TLSModeOff || cfg.TLS.AgeGraph.CertFile != "" {
			t.Fatalf("AgeGraph default = %+v, want off with no files", cfg.TLS.AgeGraph)
		}
	})

	t.Run("per_link_env_overrides", func(t *testing.T) {
		t.Setenv("ATLAS_TLS_AGE_GRAPH_MODE", "mtls")
		t.Setenv("ATLAS_TLS_AGE_GRAPH_CERT_FILE", "/run/secrets/agentatlas_tls/age_graph/tls.crt")
		t.Setenv("ATLAS_TLS_AGE_GRAPH_KEY_FILE", "/run/secrets/agentatlas_tls/age_graph/tls.key")
		t.Setenv("ATLAS_TLS_AGE_GRAPH_TRUST_BUNDLE_FILE", "/run/secrets/agentatlas_tls/age_graph/ca.crt")
		t.Setenv("ATLAS_TLS_AGE_GRAPH_REVOCATION_FILE", "/run/secrets/agentatlas_tls/age_graph/revoked.txt")
		t.Setenv("ATLAS_TLS_AGE_GRAPH_SERVER_NAME", "apache-age-graph.internal")
		t.Setenv("ATLAS_TLS_AGE_GRAPH_SPIFFE_ID", "spiffe://agentatlas.internal/ns/prod/sa/apache-age-graph")

		cfg, err := Load("")
		if err != nil {
			t.Fatal(err)
		}
		want := LinkTLS{
			Mode:            TLSModeMTLS,
			CertFile:        "/run/secrets/agentatlas_tls/age_graph/tls.crt",
			KeyFile:         "/run/secrets/agentatlas_tls/age_graph/tls.key",
			TrustBundleFile: "/run/secrets/agentatlas_tls/age_graph/ca.crt",
			RevocationFile:  "/run/secrets/agentatlas_tls/age_graph/revoked.txt",
			ServerName:      "apache-age-graph.internal",
			SPIFFEID:        "spiffe://agentatlas.internal/ns/prod/sa/apache-age-graph",
		}
		if cfg.TLS.AgeGraph != want {
			t.Fatalf("AgeGraph TLS config = %+v, want %+v", cfg.TLS.AgeGraph, want)
		}
		// The AGE-graph link is independent of the authoritative Postgres link.
		if cfg.TLS.Postgres.Mode != TLSModeOff || cfg.TLS.Postgres.CertFile != "" {
			t.Fatalf("authoritative Postgres link leaked AGE-graph overrides: %+v", cfg.TLS.Postgres)
		}
	})

	t.Run("yaml_round_trip", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "atlas.yaml")
		yamlBody := "tls:\n" +
			"  age_graph:\n" +
			"    mode: mtls\n" +
			"    cert_file: /etc/agentatlas/tls/age_graph/tls.crt\n" +
			"    key_file: /etc/agentatlas/tls/age_graph/tls.key\n" +
			"    trust_bundle_file: /etc/agentatlas/tls/age_graph/ca.crt\n" +
			"    spiffe_id: spiffe://agentatlas.internal/ns/prod/sa/apache-age-graph\n"
		if err := os.WriteFile(path, []byte(yamlBody), 0o600); err != nil {
			t.Fatal(err)
		}
		cfg, err := Load(path)
		if err != nil {
			t.Fatal(err)
		}
		if cfg.TLS.AgeGraph.Mode != TLSModeMTLS || cfg.TLS.AgeGraph.CertFile != "/etc/agentatlas/tls/age_graph/tls.crt" {
			t.Fatalf("AgeGraph not loaded from YAML: %+v", cfg.TLS.AgeGraph)
		}
	})
}

func readDeploymentFile(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}
