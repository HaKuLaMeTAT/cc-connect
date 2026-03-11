package codex

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestGetCodexContextUsage_UsesLastTokenUsage(t *testing.T) {
	homeDir := t.TempDir()
	t.Setenv("HOME", homeDir)
	t.Setenv("CODEX_HOME", filepath.Join(homeDir, ".codex"))

	sessionID := "session-123"
	sessionsDir := filepath.Join(homeDir, ".codex", "sessions", "2026", "03", "11")
	if err := os.MkdirAll(sessionsDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}

	path := filepath.Join(sessionsDir, "rollout-"+sessionID+".jsonl")
	data := "" +
		"{\"timestamp\":\"2026-03-11T12:58:46Z\",\"type\":\"session_meta\",\"payload\":{\"id\":\"" + sessionID + "\",\"cwd\":\"/tmp\"}}\n" +
		"{\"timestamp\":\"2026-03-11T12:58:59Z\",\"type\":\"event_msg\",\"payload\":{\"type\":\"token_count\",\"info\":{\"last_token_usage\":{\"input_tokens\":111,\"output_tokens\":22,\"total_tokens\":133}}}}\n" +
		"{\"timestamp\":\"2026-03-11T12:59:45Z\",\"type\":\"event_msg\",\"payload\":{\"type\":\"token_count\",\"info\":{\"last_token_usage\":{\"input_tokens\":333,\"output_tokens\":44,\"total_tokens\":377}}}}\n"
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	agent := &Agent{}
	usage, err := agent.GetContextUsage(context.Background(), sessionID)
	if err != nil {
		t.Fatalf("GetContextUsage: %v", err)
	}
	if usage.PromptTokens != 333 || usage.CompletionTokens != 44 || usage.TotalTokens != 377 {
		t.Fatalf("usage = %#v, want prompt=333 completion=44 total=377", usage)
	}
}

func TestUsageFromTokenMap_ComputesTotalWhenMissing(t *testing.T) {
	usage := usageFromTokenMap(map[string]any{
		"prompt_tokens":     float64(12),
		"completion_tokens": float64(5),
	})
	if usage.PromptTokens != 12 || usage.CompletionTokens != 5 || usage.TotalTokens != 17 {
		t.Fatalf("usage = %#v, want prompt=12 completion=5 total=17", usage)
	}
}

func TestGetCodexContextUsage_ParsesQuotaWindows(t *testing.T) {
	homeDir := t.TempDir()
	t.Setenv("HOME", homeDir)
	t.Setenv("CODEX_HOME", filepath.Join(homeDir, ".codex"))

	sessionID := "session-quota"
	sessionsDir := filepath.Join(homeDir, ".codex", "sessions", "2026", "03", "11")
	if err := os.MkdirAll(sessionsDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}

	path := filepath.Join(sessionsDir, "rollout-"+sessionID+".jsonl")
	data := "" +
		"{\"timestamp\":\"2026-03-11T12:58:46Z\",\"type\":\"session_meta\",\"payload\":{\"id\":\"" + sessionID + "\",\"cwd\":\"/tmp\"}}\n" +
		"{\"timestamp\":\"2026-03-11T12:59:45Z\",\"type\":\"event_msg\",\"payload\":{\"type\":\"token_count\",\"info\":{\"last_token_usage\":{\"input_tokens\":333,\"output_tokens\":44,\"total_tokens\":377}},\"rate_limits\":{\"primary\":{\"used_percent\":2.0,\"window_minutes\":300,\"resets_at\":1773250997},\"secondary\":{\"used_percent\":3.0,\"window_minutes\":10080,\"resets_at\":1773815495},\"plan_type\":\"plus\"}}}\n"
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	usage, err := getCodexContextUsage(sessionID)
	if err != nil {
		t.Fatalf("getCodexContextUsage: %v", err)
	}
	if usage.PlanType != "plus" {
		t.Fatalf("PlanType = %q, want plus", usage.PlanType)
	}
	if usage.DailyQuota == nil || usage.DailyQuota.WindowMinutes != 300 || usage.DailyQuota.UsedPercent != 2 {
		t.Fatalf("DailyQuota = %#v, want 300 min / 2%%", usage.DailyQuota)
	}
	if usage.WeeklyQuota == nil || usage.WeeklyQuota.WindowMinutes != 10080 || usage.WeeklyQuota.UsedPercent != 3 {
		t.Fatalf("WeeklyQuota = %#v, want 10080 min / 3%%", usage.WeeklyQuota)
	}
}
