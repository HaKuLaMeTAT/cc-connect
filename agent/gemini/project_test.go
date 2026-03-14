package gemini

import (
	"os"
	"path/filepath"
	"testing"
)

func TestGeminiProjectHash_UsesProjectRootMapping(t *testing.T) {
	homeDir := t.TempDir()
	t.Setenv("HOME", homeDir)

	workDir := filepath.Join(t.TempDir(), "PowerQuant_Project")
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		t.Fatalf("MkdirAll workDir: %v", err)
	}

	tmpDir := filepath.Join(homeDir, ".gemini", "tmp", "powerquant-project")
	if err := os.MkdirAll(tmpDir, 0o755); err != nil {
		t.Fatalf("MkdirAll tmpDir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(tmpDir, ".project_root"), []byte(workDir), 0o644); err != nil {
		t.Fatalf("WriteFile .project_root: %v", err)
	}

	if got := geminiProjectHash(workDir); got != "powerquant-project" {
		t.Fatalf("geminiProjectHash(%q) = %q, want powerquant-project", workDir, got)
	}
}
