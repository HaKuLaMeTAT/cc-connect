package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSessionStorePath_UsesLegacyFileInDataDir(t *testing.T) {
	dataDir := t.TempDir()
	legacy := filepath.Join(dataDir, "demo.json")
	if err := os.WriteFile(legacy, []byte("{}"), 0o644); err != nil {
		t.Fatalf("write legacy file: %v", err)
	}

	got := sessionStorePath(dataDir, "demo", "")
	if got != legacy {
		t.Fatalf("sessionStorePath() = %q, want %q", got, legacy)
	}
}

func TestSessionStorePath_PrefersDataDirSessionsPathWhenNoLegacyFile(t *testing.T) {
	dataDir := t.TempDir()

	got := sessionStorePath(dataDir, "demo", "")
	want := filepath.Join(dataDir, "sessions", "demo.json")
	if got != want {
		t.Fatalf("sessionStorePath() = %q, want %q", got, want)
	}
}
