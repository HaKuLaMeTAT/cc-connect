package codex

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPatchSessionSource(t *testing.T) {
	tmpDir := t.TempDir()

	sessionID := "test-session-abc123"
	sessionsDir := filepath.Join(tmpDir, ".codex", "sessions")
	if err := os.MkdirAll(sessionsDir, 0o755); err != nil {
		t.Fatal(err)
	}

	fname := filepath.Join(sessionsDir, "rollout-"+sessionID+".jsonl")
	line1 := `{"timestamp":"2026-01-01T00:00:00Z","type":"session_meta","payload":{"id":"` + sessionID + `","source":"exec","originator":"codex_exec","cwd":"/tmp"}}`
	line2 := `{"timestamp":"2026-01-01T00:00:01Z","type":"response_item","payload":{"role":"user"}}`
	content := line1 + "\n" + line2 + "\n"

	if err := os.WriteFile(fname, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	codexHome := filepath.Join(tmpDir, ".codex")

	patchSessionSource(sessionID, codexHome)

	data, err := os.ReadFile(fname)
	if err != nil {
		t.Fatal(err)
	}

	lines := strings.SplitN(string(data), "\n", 2)

	if !strings.Contains(lines[0], `"source":"cli"`) {
		t.Errorf("expected source:cli, got first line: %s", lines[0])
	}
	if !strings.Contains(lines[0], `"originator":"codex_cli_rs"`) {
		t.Errorf("expected originator:codex_cli_rs, got first line: %s", lines[0])
	}
	if strings.Contains(lines[0], `"source":"exec"`) {
		t.Error("source:exec was not replaced")
	}

	// Second line should be untouched
	if !strings.HasPrefix(lines[1], `{"timestamp":"2026-01-01T00:00:01Z"`) {
		t.Errorf("second line was corrupted: %s", lines[1])
	}
}

func TestPatchSessionSource_Idempotent(t *testing.T) {
	tmpDir := t.TempDir()
	sessionID := "test-idempotent-xyz"
	sessionsDir := filepath.Join(tmpDir, ".codex", "sessions")
	os.MkdirAll(sessionsDir, 0o755)

	fname := filepath.Join(sessionsDir, "rollout-"+sessionID+".jsonl")
	line1 := `{"type":"session_meta","payload":{"id":"` + sessionID + `","source":"cli","originator":"codex_cli_rs"}}`
	if err := os.WriteFile(fname, []byte(line1+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	codexHome := filepath.Join(tmpDir, ".codex")

	patchSessionSource(sessionID, codexHome)

	data, _ := os.ReadFile(fname)
	if string(data) != line1+"\n" {
		t.Errorf("file was modified when it shouldn't have been")
	}
}

func TestFindSessionFile_FindsNestedCodexSession(t *testing.T) {
	tmpDir := t.TempDir()
	sessionID := "nested-session-123"
	sessionsDir := filepath.Join(tmpDir, ".codex", "sessions", "2026", "03", "11")
	if err := os.MkdirAll(sessionsDir, 0o755); err != nil {
		t.Fatal(err)
	}

	fname := filepath.Join(sessionsDir, "rollout-"+sessionID+".jsonl")
	if err := os.WriteFile(fname, []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	codexHome := filepath.Join(tmpDir, ".codex")

	if got := findSessionFile(sessionID, codexHome); got != fname {
		t.Fatalf("findSessionFile(%q) = %q, want %q", sessionID, got, fname)
	}
}

func TestAgentSessionOperationsUseConfiguredCodexHome(t *testing.T) {
	tmpDir := t.TempDir()
	envHome := filepath.Join(tmpDir, "env-home")
	configuredHome := filepath.Join(tmpDir, "configured-home")
	workDir := filepath.Join(tmpDir, "workspace")
	sessionID := "configured-home-session"
	sessionsDir := filepath.Join(configuredHome, "sessions", "2026", "04", "20")
	if err := os.MkdirAll(sessionsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		t.Fatal(err)
	}

	t.Setenv("CODEX_HOME", envHome)

	fname := filepath.Join(sessionsDir, "rollout-"+sessionID+".jsonl")
	data := `{"timestamp":"2026-04-20T12:00:00Z","type":"session_meta","payload":{"id":"` + sessionID + `","source":"exec","originator":"codex_exec","cwd":"` + workDir + `"}}` + "\n" +
		`{"timestamp":"2026-04-20T12:00:01Z","type":"response_item","payload":{"role":"user","content":[{"type":"input_text","text":"hello from configured home"}]}}` + "\n"
	if err := os.WriteFile(fname, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}

	agent := &Agent{workDir: workDir, codexHome: configuredHome}
	sessions, err := agent.ListSessions(context.Background())
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	if len(sessions) != 1 || sessions[0].ID != sessionID {
		t.Fatalf("ListSessions = %#v, want configured session %q", sessions, sessionID)
	}

	history, err := agent.GetSessionHistory(context.Background(), sessionID, 10)
	if err != nil {
		t.Fatalf("GetSessionHistory: %v", err)
	}
	if len(history) != 1 || history[0].Content != "hello from configured home" {
		t.Fatalf("GetSessionHistory = %#v, want configured session history", history)
	}

	if err := agent.DeleteSession(context.Background(), sessionID); err != nil {
		t.Fatalf("DeleteSession: %v", err)
	}
	if _, err := os.Stat(fname); !os.IsNotExist(err) {
		t.Fatalf("session file still exists after DeleteSession: %v", err)
	}
}
