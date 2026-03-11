package gemini

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestGetGeminiContextUsage_ReadsSessionFile(t *testing.T) {
	homeDir := t.TempDir()
	t.Setenv("HOME", homeDir)

	workDir := filepath.Join(t.TempDir(), "cc-connect")
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		t.Fatalf("MkdirAll workDir: %v", err)
	}

	sessionID := "session-usage"
	chatsDir := filepath.Join(homeDir, ".gemini", "tmp", geminiProjectHash(workDir), "chats")
	if err := os.MkdirAll(chatsDir, 0o755); err != nil {
		t.Fatalf("MkdirAll chatsDir: %v", err)
	}

	data := `{
  "sessionId": "session-usage",
  "messages": [
    {"type": "user", "content": [{"text": "hello"}]},
    {"type": "assistant", "content": [{"text": "world"}], "tokens": {"input": 11, "output": 7, "total": 18}}
  ]
}`
	if err := os.WriteFile(filepath.Join(chatsDir, sessionID+".json"), []byte(data), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	usage, err := getGeminiContextUsage(workDir, sessionID)
	if err != nil {
		t.Fatalf("getGeminiContextUsage: %v", err)
	}
	if usage.PromptTokens != 11 || usage.CompletionTokens != 7 || usage.TotalTokens != 18 {
		t.Fatalf("usage = %#v, want prompt=11 completion=7 total=18", usage)
	}
}

func TestAgentGetContextUsage_FallsBackToSessionFile(t *testing.T) {
	homeDir := t.TempDir()
	t.Setenv("HOME", homeDir)

	workDir := filepath.Join(t.TempDir(), "cc-connect")
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		t.Fatalf("MkdirAll workDir: %v", err)
	}

	sessionID := "session-usage"
	chatsDir := filepath.Join(homeDir, ".gemini", "tmp", geminiProjectHash(workDir), "chats")
	if err := os.MkdirAll(chatsDir, 0o755); err != nil {
		t.Fatalf("MkdirAll chatsDir: %v", err)
	}

	data := `{
  "sessionId": "session-usage",
  "messages": [
    {"type": "assistant", "content": [{"text": "world"}], "tokens": {"input": 21, "output": 5, "total": 26}}
  ]
}`
	if err := os.WriteFile(filepath.Join(chatsDir, sessionID+".json"), []byte(data), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	a := &Agent{workDir: workDir, sessions: make(map[string]*geminiSession)}
	usage, err := a.GetContextUsage(context.Background(), sessionID)
	if err != nil {
		t.Fatalf("GetContextUsage: %v", err)
	}
	if usage.PromptTokens != 21 || usage.CompletionTokens != 5 || usage.TotalTokens != 26 {
		t.Fatalf("usage = %#v, want prompt=21 completion=5 total=26", usage)
	}
}
