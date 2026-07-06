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
}

type AgentNexus struct {
	BaseURL string `yaml:"base_url"`
}

type ParserEndpoint struct {
	BaseURL string `yaml:"base_url"`
}

type Parsers struct {
	Docling ParserEndpoint `yaml:"docling"`
	MinerU  ParserEndpoint `yaml:"mineru"`
	ASR     ParserEndpoint `yaml:"asr"`
	Video   ParserEndpoint `yaml:"video"`
}

type Config struct {
	Postgres      Postgres      `yaml:"postgres"`
	OpenSearch    OpenSearch    `yaml:"opensearch"`
	NATS          NATS          `yaml:"nats"`
	ObjectStorage ObjectStorage `yaml:"object_storage"`
	LLMRouter     LLMRouter     `yaml:"llmrouter"`
	AgentNexus    AgentNexus    `yaml:"agentnexus"`
	Parsers       Parsers       `yaml:"parsers"`
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
		AgentNexus: AgentNexus{BaseURL: "http://localhost:8100"},
		Parsers: Parsers{
			Docling: ParserEndpoint{BaseURL: "http://localhost:5001"},
			MinerU:  ParserEndpoint{BaseURL: "http://localhost:5002"},
			ASR:     ParserEndpoint{BaseURL: "http://localhost:5003"},
			Video:   ParserEndpoint{BaseURL: "http://localhost:5004"},
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
	set(&c.AgentNexus.BaseURL, "ATLAS_NEXUS_BASE_URL")
	set(&c.Parsers.Docling.BaseURL, "ATLAS_DOCLING_URL")
	set(&c.Parsers.MinerU.BaseURL, "ATLAS_MINERU_URL")
	set(&c.Parsers.ASR.BaseURL, "ATLAS_ASR_URL")
	set(&c.Parsers.Video.BaseURL, "ATLAS_VIDEO_URL")
}
