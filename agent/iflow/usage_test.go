package iflow

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestGetIFlowContextUsage_UsesLatestAssistantUsage(t *testing.T) {
	homeDir := t.TempDir()
	t.Setenv("HOME", homeDir)

	workDir := filepath.Join(t.TempDir(), "work")
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		t.Fatalf("MkdirAll workDir: %v", err)
	}

	sessionID := "session-usage"
	projectDir := filepath.Join(homeDir, ".iflow", "projects", iflowProjectKey(iflowResolvedWorkDir(workDir)))
	if err := os.MkdirAll(projectDir, 0o755); err != nil {
		t.Fatalf("MkdirAll projectDir: %v", err)
	}

	path := filepath.Join(projectDir, sessionID+".jsonl")
	data := "" +
		"{\"sessionId\":\"" + sessionID + "\",\"type\":\"assistant\",\"message\":{\"role\":\"assistant\",\"content\":[{\"type\":\"text\",\"text\":\"one\"}],\"usage\":{\"input_tokens\":10,\"output_tokens\":2}}}\n" +
		"{\"sessionId\":\"" + sessionID + "\",\"type\":\"assistant\",\"message\":{\"role\":\"assistant\",\"content\":[{\"type\":\"text\",\"text\":\"two\"}],\"usage\":{\"prompt_tokens\":30,\"completion_tokens\":7,\"total_tokens\":37}}}\n"
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	agent := &Agent{workDir: workDir}
	usage, err := agent.GetContextUsage(context.Background(), sessionID)
	if err != nil {
		t.Fatalf("GetContextUsage: %v", err)
	}
	if usage.PromptTokens != 30 || usage.CompletionTokens != 7 || usage.TotalTokens != 37 {
		t.Fatalf("usage = %#v, want prompt=30 completion=7 total=37", usage)
	}
}

func TestIFlowUsageFromMap_ComputesTotalWhenMissing(t *testing.T) {
	usage := iflowUsageFromMap(map[string]any{
		"input_tokens":  float64(9),
		"output_tokens": float64(4),
	})
	if usage.PromptTokens != 9 || usage.CompletionTokens != 4 || usage.TotalTokens != 13 {
		t.Fatalf("usage = %#v, want prompt=9 completion=4 total=13", usage)
	}
}
