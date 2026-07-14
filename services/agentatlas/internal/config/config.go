// Package config loads AgentAtlas service configuration from YAML with
// environment variable overlay (ATLAS_* keys), per the deployment profile
// rule: same config surface for Compose, Helm, and offline packages.
package config

import (
	"fmt"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

type Postgres struct {
	DSN string `yaml:"dsn"`
}

type OpenSearch struct {
	Addresses []string `yaml:"addresses"`
	Username  string   `yaml:"username"`
	Password  string   `yaml:"password"`
}

type NATS struct {
	URL string `yaml:"url"`
}

type ObjectStorage struct {
	Endpoint  string `yaml:"endpoint"`
	Region    string `yaml:"region"`
	Bucket    string `yaml:"bucket"`
	AccessKey string `yaml:"access_key"`
	SecretKey string `yaml:"secret_key"`
	UseSSL    bool   `yaml:"use_ssl"`
}

// LLMRouter points at any OpenAI-compatible Chat Completions endpoint.
// llmrouter is the default and recommended implementation.
type LLMRouter struct {
	BaseURL      string `yaml:"base_url"`
	APIKey       string `yaml:"api_key"`
	DefaultModel string `yaml:"default_model"`
	RerankModel  string `yaml:"rerank_model"`
}

type AgentNexus struct {
	BaseURL                                 string `yaml:"base_url"`
	ClientID                                string `yaml:"client_id"`
	SecretFile                              string `yaml:"secret_file"`
	ApprovalFactsSecretFile                 string `yaml:"approval_facts_secret_file"`
	BrowserClientID                         string `yaml:"browser_client_id"`
	BrowserClientSecretFile                 string `yaml:"browser_client_secret_file"`
	BrowserSessionEncryptionKeyID           string `yaml:"browser_session_encryption_key_id"`
	BrowserSessionEncryptionKeyFile         string `yaml:"browser_session_encryption_key_file"`
	BrowserSessionPreviousEncryptionKeyID   string `yaml:"browser_session_previous_encryption_key_id"`
	BrowserSessionPreviousEncryptionKeyFile string `yaml:"browser_session_previous_encryption_key_file"`
	AtlasPublicURL                          string `yaml:"atlas_public_url"`
}

type ParserEndpoint struct {
	BaseURL string `yaml:"base_url"`
}

type Parsers struct {
	// GatewayURL switches artifact parsing to the standalone parser-gateway
	// service over HTTP; empty keeps the in-process gateway (unit tests,
	// single-binary runs).
	GatewayURL string         `yaml:"gateway_url"`
	Docling    ParserEndpoint `yaml:"docling"`
	MinerU     ParserEndpoint `yaml:"mineru"`
	ASR        ParserEndpoint `yaml:"asr"`
	Video      ParserEndpoint `yaml:"video"`
}

// TLSMode selects how a link enforces transport security. The values
// mirror internal/transportsecurity.Mode's; kept as an independent string
// type here (rather than importing that package) so package config stays a
// dependency-free leaf — internal/transportsecurity.FromLinkTLS is the one
// place that converts a LinkTLS value into that package's runtime type, at
// each cmd/*/main.go composition root.
type TLSMode string

const (
	// TLSModeOff disables TLS for the link — the backward-compatible
	// default for every existing deployment.
	TLSModeOff TLSMode = "off"
	// TLSModeTLS is server-auth-only TLS (no client certificate).
	TLSModeTLS TLSMode = "tls"
	// TLSModeMTLS is mutual TLS: both sides present and verify a
	// certificate. This is the secure-default posture every named
	// AgentAtlas link should run in production (see
	// deploy/compose/compose.yaml and deploy/helm/agentatlas/values.yaml).
	TLSModeMTLS TLSMode = "mtls"
)

// LinkTLS configures one transport-security link: this service's own leaf
// certificate/key, the trust bundle used to verify the peer, an optional
// revoked-serial list, and the peer identity this link expects on the
// other end (a SPIFFE URI and/or a DNS name — at least one is required
// once Mode != TLSModeOff).
type LinkTLS struct {
	Mode            TLSMode `yaml:"mode"`
	CertFile        string  `yaml:"cert_file"`
	KeyFile         string  `yaml:"key_file"`
	TrustBundleFile string  `yaml:"trust_bundle_file"`
	// RevocationFile is optional even when Mode != TLSModeOff: a plain
	// text file of revoked certificate serial numbers, one hex-encoded
	// serial per line. Empty means "no revocation checking" for this link.
	RevocationFile string `yaml:"revocation_file"`
	ServerName     string `yaml:"server_name"`
	SPIFFEID       string `yaml:"spiffe_id"`
}

// TLS is the transport-security configuration surface for every named
// AgentAtlas link (the eleven links GA Task 13A's per-link matrix names
// after the Provable Outcome Graph revision): AgentAtlas and Gateway are
// this service's own SERVER identities (atlas-api/atlas-agent and
// parser-gateway accepting inbound connections); AgentNexus, LLMRouter,
// Postgres, OpenSearch, NATS, ObjectStorage, Parser, and AgeGraph are the
// CLIENT links AgentAtlas (and the atlas-outcome-projector) dial out on. The
// eleventh matrix name — the "Outcome projector" — is the projector's own
// dial-out identity (the client certificate it presents on its links), not a
// separate endpoint config here. AgeGraph is the Task 0I "Apache AGE graph
// PostgreSQL" endpoint: a SEPARATE PostgreSQL from Postgres (the
// authoritative WorkCase database), secured independently, and reached via
// ATLAS_OUTCOME_GRAPH_DSN. Every link defaults to TLSModeOff
// (backward-compatible plaintext); flipping to TLSModeMTLS per link, or
// globally via ATLAS_TLS_MODE, is the documented secure production posture.
type TLS struct {
	AgentAtlas    LinkTLS `yaml:"agentatlas"`
	Gateway       LinkTLS `yaml:"gateway"`
	AgentNexus    LinkTLS `yaml:"agentnexus"`
	LLMRouter     LinkTLS `yaml:"llmrouter"`
	Postgres      LinkTLS `yaml:"postgres"`
	OpenSearch    LinkTLS `yaml:"opensearch"`
	NATS          LinkTLS `yaml:"nats"`
	ObjectStorage LinkTLS `yaml:"object_storage"`
	Parser        LinkTLS `yaml:"parser"`
	// AgeGraph secures the Apache AGE graph PostgreSQL link (the
	// atlas-outcome-projector's dial-out to the separately-configured graph
	// read model). Distinct from Postgres so the authoritative and graph
	// databases carry independent identities and trust.
	AgeGraph LinkTLS `yaml:"age_graph"`
}

type Config struct {
	Postgres      Postgres      `yaml:"postgres"`
	OpenSearch    OpenSearch    `yaml:"opensearch"`
	NATS          NATS          `yaml:"nats"`
	ObjectStorage ObjectStorage `yaml:"object_storage"`
	LLMRouter     LLMRouter     `yaml:"llmrouter"`
	AgentNexus    AgentNexus    `yaml:"agentnexus"`
	Parsers       Parsers       `yaml:"parsers"`
	TLS           TLS           `yaml:"tls"`
}

func Default() *Config {
	return &Config{
		Postgres:   Postgres{DSN: "postgres://atlas:atlas@localhost:5432/agentatlas?sslmode=disable"},
		OpenSearch: OpenSearch{Addresses: []string{"http://localhost:9200"}},
		NATS:       NATS{URL: "nats://localhost:4222"},
		ObjectStorage: ObjectStorage{
			Endpoint: "http://localhost:9000",
			Region:   "us-east-1",
			Bucket:   "agentatlas",
		},
		LLMRouter:  LLMRouter{BaseURL: "http://localhost:8000/v1"},
		AgentNexus: AgentNexus{BaseURL: "http://localhost:8100", ClientID: "agentatlas", SecretFile: "/run/secrets/agentatlas_agentnexus_service_secret", BrowserClientID: "agentatlas", BrowserClientSecretFile: "/run/secrets/agentatlas_browser_client_secret", BrowserSessionEncryptionKeyID: "current", BrowserSessionEncryptionKeyFile: "/run/secrets/agentatlas_browser_session_key", BrowserSessionPreviousEncryptionKeyID: "previous", AtlasPublicURL: "https://atlas.local"},
		Parsers: Parsers{
			Docling: ParserEndpoint{BaseURL: "http://localhost:5001"},
			MinerU:  ParserEndpoint{BaseURL: "http://localhost:5002"},
			ASR:     ParserEndpoint{BaseURL: "http://localhost:5003"},
			Video:   ParserEndpoint{BaseURL: "http://localhost:5004"},
		},
		TLS: TLS{
			AgentAtlas:    LinkTLS{Mode: TLSModeOff},
			Gateway:       LinkTLS{Mode: TLSModeOff},
			AgentNexus:    LinkTLS{Mode: TLSModeOff},
			LLMRouter:     LinkTLS{Mode: TLSModeOff},
			Postgres:      LinkTLS{Mode: TLSModeOff},
			OpenSearch:    LinkTLS{Mode: TLSModeOff},
			NATS:          LinkTLS{Mode: TLSModeOff},
			ObjectStorage: LinkTLS{Mode: TLSModeOff},
			Parser:        LinkTLS{Mode: TLSModeOff},
			AgeGraph:      LinkTLS{Mode: TLSModeOff},
		},
	}
}

// Load builds config from defaults, then the YAML file at path (required to
// exist when path is non-empty), then ATLAS_* environment overrides.
func Load(path string) (*Config, error) {
	cfg := Default()
	if path != "" {
		raw, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read config %s: %w", path, err)
		}
		if err := yaml.Unmarshal(raw, cfg); err != nil {
			return nil, fmt.Errorf("parse config %s: %w", path, err)
		}
	}
	cfg.applyEnv()
	if err := cfg.TLS.validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

func (c *Config) applyEnv() {
	set := func(target *string, key string) {
		if v, ok := os.LookupEnv(key); ok {
			*target = v
		}
	}
	set(&c.Postgres.DSN, "ATLAS_POSTGRES_DSN")
	if v, ok := os.LookupEnv("ATLAS_OPENSEARCH_ADDRESSES"); ok {
		c.OpenSearch.Addresses = strings.Split(v, ",")
	}
	set(&c.OpenSearch.Username, "ATLAS_OPENSEARCH_USERNAME")
	set(&c.OpenSearch.Password, "ATLAS_OPENSEARCH_PASSWORD")
	set(&c.NATS.URL, "ATLAS_NATS_URL")
	set(&c.ObjectStorage.Endpoint, "ATLAS_OBJECT_ENDPOINT")
	set(&c.ObjectStorage.Region, "ATLAS_OBJECT_REGION")
	set(&c.ObjectStorage.Bucket, "ATLAS_OBJECT_BUCKET")
	set(&c.ObjectStorage.AccessKey, "ATLAS_OBJECT_ACCESS_KEY")
	set(&c.ObjectStorage.SecretKey, "ATLAS_OBJECT_SECRET_KEY")
	if v, ok := os.LookupEnv("ATLAS_OBJECT_USE_SSL"); ok {
		c.ObjectStorage.UseSSL = v == "true" || v == "1"
	}
	set(&c.LLMRouter.BaseURL, "ATLAS_LLMROUTER_BASE_URL")
	set(&c.LLMRouter.APIKey, "ATLAS_LLMROUTER_API_KEY")
	set(&c.LLMRouter.DefaultModel, "ATLAS_LLMROUTER_MODEL")
	set(&c.LLMRouter.RerankModel, "ATLAS_LLMROUTER_RERANK_MODEL")
	set(&c.AgentNexus.BaseURL, "ATLAS_NEXUS_BASE_URL")
	set(&c.AgentNexus.ClientID, "ATLAS_NEXUS_CLIENT_ID")
	set(&c.AgentNexus.SecretFile, "ATLAS_NEXUS_SERVICE_SECRET_FILE")
	set(&c.AgentNexus.ApprovalFactsSecretFile, "ATLAS_NEXUS_APPROVAL_FACTS_SECRET_FILE")
	set(&c.AgentNexus.BrowserClientID, "ATLAS_NEXUS_BROWSER_CLIENT_ID")
	set(&c.AgentNexus.BrowserClientSecretFile, "ATLAS_NEXUS_BROWSER_CLIENT_SECRET_FILE")
	set(&c.AgentNexus.BrowserSessionEncryptionKeyID, "ATLAS_BROWSER_SESSION_ENCRYPTION_KEY_ID")
	set(&c.AgentNexus.BrowserSessionEncryptionKeyFile, "ATLAS_BROWSER_SESSION_ENCRYPTION_KEY_FILE")
	set(&c.AgentNexus.BrowserSessionPreviousEncryptionKeyID, "ATLAS_BROWSER_SESSION_PREVIOUS_ENCRYPTION_KEY_ID")
	set(&c.AgentNexus.BrowserSessionPreviousEncryptionKeyFile, "ATLAS_BROWSER_SESSION_PREVIOUS_ENCRYPTION_KEY_FILE")
	set(&c.AgentNexus.AtlasPublicURL, "ATLAS_PUBLIC_URL")
	set(&c.Parsers.GatewayURL, "ATLAS_PARSER_GATEWAY_URL")
	set(&c.Parsers.Docling.BaseURL, "ATLAS_DOCLING_URL")
	set(&c.Parsers.MinerU.BaseURL, "ATLAS_MINERU_URL")
	set(&c.Parsers.ASR.BaseURL, "ATLAS_ASR_URL")
	set(&c.Parsers.Video.BaseURL, "ATLAS_VIDEO_URL")

	// ATLAS_TLS_MODE seeds every link's mode at once — the single on/off
	// switch for the secure-default posture — then each link's own
	// ATLAS_TLS_<LINK>_* variables can override individually. Same
	// same-keys-everywhere rule as the rest of this file: identical
	// behavior across Compose, Helm, and offline packages.
	links := c.tlsLinkEnvs()
	if v, ok := os.LookupEnv("ATLAS_TLS_MODE"); ok {
		for _, l := range links {
			l.cfg.Mode = TLSMode(v)
		}
	}
	for _, l := range links {
		applyLinkTLSEnv(l.cfg, l.prefix)
	}
}

type tlsLinkEnv struct {
	cfg    *LinkTLS
	name   string
	prefix string
}

func (c *Config) tlsLinkEnvs() []tlsLinkEnv {
	return []tlsLinkEnv{
		{&c.TLS.AgentAtlas, "agentatlas", "ATLAS_TLS_AGENTATLAS_"},
		{&c.TLS.Gateway, "gateway", "ATLAS_TLS_GATEWAY_"},
		{&c.TLS.AgentNexus, "agentnexus", "ATLAS_TLS_AGENTNEXUS_"},
		{&c.TLS.LLMRouter, "llmrouter", "ATLAS_TLS_LLMROUTER_"},
		{&c.TLS.Postgres, "postgres", "ATLAS_TLS_POSTGRES_"},
		{&c.TLS.OpenSearch, "opensearch", "ATLAS_TLS_OPENSEARCH_"},
		{&c.TLS.NATS, "nats", "ATLAS_TLS_NATS_"},
		{&c.TLS.ObjectStorage, "object_storage", "ATLAS_TLS_OBJECT_STORAGE_"},
		{&c.TLS.Parser, "parser", "ATLAS_TLS_PARSER_"},
		{&c.TLS.AgeGraph, "age_graph", "ATLAS_TLS_AGE_GRAPH_"},
	}
}

// validate enforces the fail-closed invariants at config-load time
// (defense-in-depth ahead of internal/transportsecurity.NewManager, which
// enforces the same identity rule again at composition time). A link that
// enables TLS (Mode != off) MUST pin an expected peer identity — at least
// one of server_name / spiffe_id — otherwise it would accept any
// chain-verified peer, defeating per-link identity pinning (GA Task 13A).
func (t *TLS) validate() error {
	for _, l := range (&Config{TLS: *t}).tlsLinkEnvs() {
		if l.cfg.Mode == "" || l.cfg.Mode == TLSModeOff {
			continue
		}
		if l.cfg.Mode != TLSModeTLS && l.cfg.Mode != TLSModeMTLS {
			return fmt.Errorf("config: tls.%s.mode %q is invalid (want off, tls, or mtls)", l.name, l.cfg.Mode)
		}
		if strings.TrimSpace(l.cfg.ServerName) == "" && strings.TrimSpace(l.cfg.SPIFFEID) == "" {
			return fmt.Errorf("config: tls.%s enables TLS (mode=%s) but pins no peer identity — set tls.%s.server_name and/or tls.%s.spiffe_id (a link with no expected identity would accept any chain-verified peer)", l.name, l.cfg.Mode, l.name, l.name)
		}
	}
	return nil
}

func applyLinkTLSEnv(l *LinkTLS, prefix string) {
	if v, ok := os.LookupEnv(prefix + "MODE"); ok {
		l.Mode = TLSMode(v)
	}
	setStr := func(target *string, key string) {
		if v, ok := os.LookupEnv(prefix + key); ok {
			*target = v
		}
	}
	setStr(&l.CertFile, "CERT_FILE")
	setStr(&l.KeyFile, "KEY_FILE")
	setStr(&l.TrustBundleFile, "TRUST_BUNDLE_FILE")
	setStr(&l.RevocationFile, "REVOCATION_FILE")
	setStr(&l.ServerName, "SERVER_NAME")
	setStr(&l.SPIFFEID, "SPIFFE_ID")
}
