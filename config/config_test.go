package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const relayConfigFixture = `
[relay]
timeout_secs = 300

[[projects]]
name = "alpha"

[projects.agent]
type = "codex"

[projects.agent.options]
work_dir = "/tmp/alpha"

[[projects.platforms]]
type = "telegram"

[projects.platforms.options]
bot_token = "token_xxx"
`

const relayConfigNegativeFixture = `
[relay]
timeout_secs = -1

[[projects]]
name = "alpha"

[projects.agent]
type = "codex"

[projects.agent.options]
work_dir = "/tmp/alpha"

[[projects.platforms]]
type = "telegram"

[projects.platforms.options]
bot_token = "token_xxx"
`

func TestSaveRuntimeOptions(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	content := `[[projects]]
name = "demo"

[projects.agent]
type = "codex"

[projects.agent.options]
work_dir = "/tmp/demo"

[[projects.platforms]]
type = "telegram"
`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	prevPath := ConfigPath
	ConfigPath = path
	defer func() {
		ConfigPath = prevPath
	}()

	if err := SaveProjectModel("demo", "o3"); err != nil {
		t.Fatalf("SaveProjectModel: %v", err)
	}
	if err := SaveProjectReasoningEffort("demo", "high"); err != nil {
		t.Fatalf("SaveProjectReasoningEffort: %v", err)
	}
	if err := SaveGlobalQuiet(true); err != nil {
		t.Fatalf("SaveGlobalQuiet: %v", err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	opts := cfg.Projects[0].Agent.Options
	if got, _ := opts["model"].(string); got != "o3" {
		t.Fatalf("model = %q, want %q", got, "o3")
	}
	if got, _ := opts["reasoning_effort"].(string); got != "high" {
		t.Fatalf("reasoning_effort = %q, want %q", got, "high")
	}
	if cfg.Quiet == nil || !*cfg.Quiet {
		t.Fatalf("quiet = %v, want true", cfg.Quiet)
	}
}

func TestLoadRelayTimeoutConfig(t *testing.T) {
	configPath := writeConfigFixture(t, relayConfigFixture)

	cfg, err := Load(configPath)
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	if cfg.Relay.TimeoutSecs == nil {
		t.Fatal("cfg.Relay.TimeoutSecs = nil, want non-nil")
	}
	if *cfg.Relay.TimeoutSecs != 300 {
		t.Fatalf("cfg.Relay.TimeoutSecs = %d, want 300", *cfg.Relay.TimeoutSecs)
	}
}

func TestLoadRejectsNegativeRelayTimeout(t *testing.T) {
	configPath := writeConfigFixture(t, relayConfigNegativeFixture)

	_, err := Load(configPath)
	if err == nil {
		t.Fatal("expected error for negative relay timeout, got nil")
	}
	if !strings.Contains(err.Error(), "relay.timeout_secs must be >= 0") {
		t.Fatalf("error = %q, want contains %q", err.Error(), "relay.timeout_secs must be >= 0")
	}
}

func writeConfigFixture(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}
