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

func readDeploymentFile(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}
