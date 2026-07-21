package config

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestComposePreviousBrowserKeyIsOptionalAndRotationOverrideIsExplicit(t *testing.T) {
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker CLI not installed")
	}
	composeDir := filepath.Join("..", "..", "deploy", "compose")
	requiredEnv := append(os.Environ(),
		"ATLAS_NEXUS_SERVICE_SECRET_FILE_SOURCE=C:/secrets/nexus-service",
		"ATLAS_APPROVAL_AUTHORITY=oa.example.customer",
		"ATLAS_NEXUS_BROWSER_CLIENT_SECRET_FILE_SOURCE=C:/secrets/browser-client",
		"ATLAS_BROWSER_SESSION_ENCRYPTION_KEY_FILE_SOURCE=C:/secrets/browser-current",
	)

	base := runDeploymentCommand(t, composeDir, requiredEnv, "docker", "compose", "-f", "compose.yaml", "config")
	if strings.Contains(base, "ATLAS_BROWSER_SESSION_PREVIOUS_ENCRYPTION_KEY") || strings.Contains(base, "agentatlas_browser_session_previous_key") {
		t.Fatal("base Compose render requires the optional previous browser key")
	}

	rotationEnv := append(requiredEnv,
		"ATLAS_BROWSER_SESSION_PREVIOUS_ENCRYPTION_KEY_ID=2026-06",
		"ATLAS_BROWSER_SESSION_PREVIOUS_ENCRYPTION_KEY_FILE_SOURCE=C:/secrets/browser-previous",
	)
	rotation := runDeploymentCommand(t, composeDir, rotationEnv, "docker", "compose", "-f", "compose.yaml", "-f", "compose.rotation.yaml", "config")
	for _, want := range []string{"ATLAS_BROWSER_SESSION_PREVIOUS_ENCRYPTION_KEY_ID: 2026-06", "ATLAS_BROWSER_SESSION_PREVIOUS_ENCRYPTION_KEY_FILE: /run/secrets/agentatlas_browser_session_previous_key", "agentatlas_browser_session_previous_key"} {
		if !strings.Contains(rotation, want) {
			t.Fatalf("rotation Compose render missing %q", want)
		}
	}
}

func TestHelmPreviousBrowserKeyIsOptionalAndConditionallyProjected(t *testing.T) {
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("helm CLI not installed")
	}
	chartDir := filepath.Join("..", "..", "deploy", "helm")
	base := runDeploymentCommand(t, chartDir, os.Environ(), "helm", "template", "atlas-test", "agentatlas")
	if strings.Contains(base, "ATLAS_BROWSER_SESSION_PREVIOUS_ENCRYPTION_KEY") || strings.Contains(base, "browser-session-previous-key") {
		t.Fatal("base Helm render projects the optional previous browser key")
	}
	runDeploymentCommand(t, chartDir, os.Environ(), "helm", "lint", "agentatlas")

	args := []string{"template", "atlas-test", "agentatlas", "--set", "config.browserSessionPreviousEncryptionKeyId=2026-06", "--set", "config.browserSessionPreviousEncryptionKey=browser-session-previous-key"}
	rotation := runDeploymentCommand(t, chartDir, os.Environ(), "helm", args...)
	for _, want := range []string{"ATLAS_BROWSER_SESSION_PREVIOUS_ENCRYPTION_KEY_FILE", "ATLAS_BROWSER_SESSION_PREVIOUS_ENCRYPTION_KEY_ID", "browser-session-previous-key"} {
		if !strings.Contains(rotation, want) {
			t.Fatalf("rotation Helm render missing %q", want)
		}
	}
	runDeploymentCommand(t, chartDir, os.Environ(), "helm", "lint", "agentatlas", "--set", "config.browserSessionPreviousEncryptionKeyId=2026-06", "--set", "config.browserSessionPreviousEncryptionKey=browser-session-previous-key")
}

func runDeploymentCommand(t *testing.T, dir string, env []string, name string, args ...string) string {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s %s: %v\n%s", name, strings.Join(args, " "), err, out)
	}
	return string(out)
}
