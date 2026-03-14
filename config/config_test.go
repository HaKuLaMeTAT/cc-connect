package config

import (
	"os"
	"path/filepath"
	"testing"
)

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
