package gemini

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestAvailableModels_IncludesSeenModels(t *testing.T) {
	homeDir := t.TempDir()
	t.Setenv("HOME", homeDir)

	workDir := filepath.Join(t.TempDir(), "cc-connect")
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		t.Fatalf("MkdirAll workDir: %v", err)
	}

	chatsDir := filepath.Join(homeDir, ".gemini", "tmp", geminiProjectHash(workDir), "chats")
	if err := os.MkdirAll(chatsDir, 0o755); err != nil {
		t.Fatalf("MkdirAll chatsDir: %v", err)
	}

	data := `{
  "sessionId": "session-1",
  "messages": [
    {"type": "assistant", "model": "gemini-3-flash-preview", "content": [{"text": "hi"}]},
    {"type": "assistant", "model": "gemini-3.1-pro-preview", "content": [{"text": "hi"}]}
  ]
}`
	if err := os.WriteFile(filepath.Join(chatsDir, "session-1.json"), []byte(data), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	a := &Agent{workDir: workDir, sessions: make(map[string]*geminiSession)}
	models := a.AvailableModels(context.Background())
	names := make(map[string]struct{}, len(models))
	for _, m := range models {
		names[m.Name] = struct{}{}
	}
	if _, ok := names["gemini-3-flash-preview"]; !ok {
		t.Fatalf("models missing gemini-3-flash-preview: %v", models)
	}
	if _, ok := names["gemini-3.1-pro-preview"]; !ok {
		t.Fatalf("models missing gemini-3.1-pro-preview: %v", models)
	}
	if _, ok := names["gemini-2.5-flash-lite"]; !ok {
		t.Fatalf("models missing default gemini-2.5-flash-lite: %v", models)
	}
}
