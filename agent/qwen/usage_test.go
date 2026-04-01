package qwen

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestGetQwenContextUsage_UsesAssistantUsageMetadata(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	workDir := filepath.Join(home, "work")
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		t.Fatalf("MkdirAll workDir: %v", err)
	}

	projectDir := filepath.Join(home, ".qwen", "projects", strings.ReplaceAll(workDir, string(filepath.Separator), "-"))
	projectDir = filepath.Clean(projectDir)
	chatDir := filepath.Join(projectDir, "chats")
	if err := os.MkdirAll(chatDir, 0o755); err != nil {
		t.Fatalf("MkdirAll chatDir: %v", err)
	}

	sessionID := "session-usage"
	data := "" +
		`{"type":"system","systemPayload":{"uiEvent":{"event.name":"qwen-code.api_response","input_token_count":11,"output_token_count":2,"total_token_count":13}}}` + "\n" +
		`{"type":"assistant","usageMetadata":{"promptTokenCount":21,"candidatesTokenCount":5,"totalTokenCount":26}}` + "\n"
	if err := os.WriteFile(filepath.Join(chatDir, sessionID+".jsonl"), []byte(data), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	usage, err := getQwenContextUsage(workDir, sessionID)
	if err != nil {
		t.Fatalf("getQwenContextUsage: %v", err)
	}
	if usage.PromptTokens != 21 || usage.CompletionTokens != 5 || usage.TotalTokens != 26 {
		t.Fatalf("usage = %#v, want prompt=21 completion=5 total=26", usage)
	}
}

func TestAgentGetContextUsage_FallsBackToSystemTelemetry(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	workDir := filepath.Join(home, "work")
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		t.Fatalf("MkdirAll workDir: %v", err)
	}

	projectDir := filepath.Join(home, ".qwen", "projects", strings.ReplaceAll(workDir, string(filepath.Separator), "-"))
	projectDir = filepath.Clean(projectDir)
	chatDir := filepath.Join(projectDir, "chats")
	if err := os.MkdirAll(chatDir, 0o755); err != nil {
		t.Fatalf("MkdirAll chatDir: %v", err)
	}

	sessionID := "session-telemetry"
	data := `{"type":"system","systemPayload":{"uiEvent":{"event.name":"qwen-code.api_response","input_token_count":34,"output_token_count":8,"total_token_count":42}}}` + "\n"
	if err := os.WriteFile(filepath.Join(chatDir, sessionID+".jsonl"), []byte(data), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	a := &Agent{workDir: workDir}
	usage, err := a.GetContextUsage(context.Background(), sessionID)
	if err != nil {
		t.Fatalf("GetContextUsage: %v", err)
	}
	if usage.PromptTokens != 34 || usage.CompletionTokens != 8 || usage.TotalTokens != 42 {
		t.Fatalf("usage = %#v, want prompt=34 completion=8 total=42", usage)
	}
}
