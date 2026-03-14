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
	if _, ok := names["gemini-1.5-flash"]; !ok {
		if _, ok2 := names["gemini-2.0-flash"]; !ok2 {
			t.Fatalf("models missing common Gemini fallback models: %v", models)
		}
	}
}

func TestExtractSessionSummary_HandlesGeminiContentShape(t *testing.T) {
	sf := &sessionFile{
		SessionID: "sid-1",
		Messages: []struct {
			Type   string `json:"type"`
			Model  string `json:"model"`
			Tokens *struct {
				Input  int `json:"input"`
				Output int `json:"output"`
				Total  int `json:"total"`
			} `json:"tokens"`
			Content any `json:"content"`
		}{
			{Type: "user", Content: []any{map[string]any{"text": "hello world"}}},
			{Type: "gemini", Content: "assistant reply"},
		},
	}

	if got := extractSessionSummary(sf); got != "hello world" {
		t.Fatalf("extractSessionSummary = %q, want hello world", got)
	}
}
