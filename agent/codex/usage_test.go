package codex

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"
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

	primaryReset := strconv.FormatInt(time.Now().Add(2*time.Hour).Unix(), 10)
	secondaryReset := strconv.FormatInt(time.Now().Add(7*24*time.Hour).Unix(), 10)
	path := filepath.Join(sessionsDir, "rollout-"+sessionID+".jsonl")
	data := "" +
		"{\"timestamp\":\"2026-03-11T12:58:46Z\",\"type\":\"session_meta\",\"payload\":{\"id\":\"" + sessionID + "\",\"cwd\":\"/tmp\"}}\n" +
		"{\"timestamp\":\"2026-03-11T12:59:45Z\",\"type\":\"event_msg\",\"payload\":{\"type\":\"token_count\",\"info\":{\"last_token_usage\":{\"input_tokens\":333,\"output_tokens\":44,\"total_tokens\":377}},\"rate_limits\":{\"primary\":{\"used_percent\":2.0,\"window_minutes\":300,\"resets_at\":" + primaryReset + "},\"secondary\":{\"used_percent\":3.0,\"window_minutes\":10080,\"resets_at\":" + secondaryReset + "},\"plan_type\":\"plus\"}}}\n"
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
	if usage.DailyQuota != nil {
		t.Fatalf("DailyQuota = %#v, want nil for non-daily 300 min window", usage.DailyQuota)
	}
	if usage.WeeklyQuota == nil || usage.WeeklyQuota.WindowMinutes != 10080 || usage.WeeklyQuota.UsedPercent != 90 {
		t.Fatalf("WeeklyQuota = %#v, want 10080 min / 90%% display", usage.WeeklyQuota)
	}
	if len(usage.OtherQuotas) != 1 || usage.OtherQuotas[0].Label != "primary" || usage.OtherQuotas[0].WindowMinutes != 300 || usage.OtherQuotas[0].UsedPercent != 90 {
		t.Fatalf("OtherQuotas = %#v, want primary 300 min / 90%% display", usage.OtherQuotas)
	}
}

func TestGetCodexContextUsage_UsesLatestGlobalRateLimits(t *testing.T) {
	homeDir := t.TempDir()
	t.Setenv("HOME", homeDir)
	t.Setenv("CODEX_HOME", filepath.Join(homeDir, ".codex"))

	sessionID := "session-local"
	sessionsDir := filepath.Join(homeDir, ".codex", "sessions", "2026", "03", "11")
	if err := os.MkdirAll(sessionsDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}

	primaryReset := strconv.FormatInt(time.Now().Add(2*time.Hour).Unix(), 10)
	secondaryReset := strconv.FormatInt(time.Now().Add(7*24*time.Hour).Unix(), 10)
	localPath := filepath.Join(sessionsDir, "rollout-"+sessionID+".jsonl")
	localData := "" +
		"{\"timestamp\":\"2026-03-11T12:58:46Z\",\"type\":\"session_meta\",\"payload\":{\"id\":\"" + sessionID + "\",\"cwd\":\"/tmp\"}}\n" +
		"{\"timestamp\":\"2026-03-11T12:59:45Z\",\"type\":\"event_msg\",\"payload\":{\"type\":\"token_count\",\"info\":{\"last_token_usage\":{\"input_tokens\":333,\"output_tokens\":44,\"total_tokens\":377}},\"rate_limits\":{\"primary\":{\"used_percent\":94.0,\"window_minutes\":300,\"resets_at\":" + primaryReset + "},\"secondary\":{\"used_percent\":98.0,\"window_minutes\":10081,\"resets_at\":" + secondaryReset + "},\"plan_type\":\"plus\"}}}\n"
	if err := os.WriteFile(localPath, []byte(localData), 0o644); err != nil {
		t.Fatalf("WriteFile local: %v", err)
	}

	globalPath := filepath.Join(sessionsDir, "rollout-global.jsonl")
	globalData := "" +
		"{\"timestamp\":\"2026-03-11T14:13:00.899Z\",\"type\":\"event_msg\",\"payload\":{\"type\":\"token_count\",\"info\":{\"last_token_usage\":{\"input_tokens\":1,\"output_tokens\":1,\"total_tokens\":2}},\"rate_limits\":{\"primary\":{\"used_percent\":18.0,\"window_minutes\":300,\"resets_at\":" + primaryReset + "},\"secondary\":{\"used_percent\":8.0,\"window_minutes\":10081,\"resets_at\":" + secondaryReset + "},\"plan_type\":\"plus\"}}}\n"
	if err := os.WriteFile(globalPath, []byte(globalData), 0o644); err != nil {
		t.Fatalf("WriteFile global: %v", err)
	}

	usage, err := getCodexContextUsage(sessionID)
	if err != nil {
		t.Fatalf("getCodexContextUsage: %v", err)
	}
	if usage.WeeklyQuota == nil || usage.WeeklyQuota.UsedPercent != 90 {
		t.Fatalf("WeeklyQuota = %#v, want global 90", usage.WeeklyQuota)
	}
	if len(usage.OtherQuotas) == 0 || usage.OtherQuotas[0].UsedPercent != 80 {
		t.Fatalf("OtherQuotas = %#v, want global primary 80", usage.OtherQuotas)
	}
}

func TestGetCodexContextUsage_PrefersNewerSessionRateLimits(t *testing.T) {
	homeDir := t.TempDir()
	t.Setenv("HOME", homeDir)
	t.Setenv("CODEX_HOME", filepath.Join(homeDir, ".codex"))

	sessionID := "session-newer-local"
	sessionsDir := filepath.Join(homeDir, ".codex", "sessions", "2026", "03", "11")
	if err := os.MkdirAll(sessionsDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}

	primaryReset := strconv.FormatInt(time.Now().Add(2*time.Hour).Unix(), 10)
	secondaryReset := strconv.FormatInt(time.Now().Add(7*24*time.Hour).Unix(), 10)
	localPath := filepath.Join(sessionsDir, "rollout-"+sessionID+".jsonl")
	localData := "" +
		"{\"timestamp\":\"2026-03-11T15:00:00Z\",\"type\":\"session_meta\",\"payload\":{\"id\":\"" + sessionID + "\",\"cwd\":\"/tmp\"}}\n" +
		"{\"timestamp\":\"2026-03-11T15:10:00Z\",\"type\":\"event_msg\",\"payload\":{\"type\":\"token_count\",\"info\":{\"last_token_usage\":{\"input_tokens\":333,\"output_tokens\":44,\"total_tokens\":377}},\"rate_limits\":{\"primary\":{\"used_percent\":12.0,\"window_minutes\":300,\"resets_at\":" + primaryReset + "},\"secondary\":{\"used_percent\":24.0,\"window_minutes\":10081,\"resets_at\":" + secondaryReset + "},\"plan_type\":\"plus\"}}}\n"
	if err := os.WriteFile(localPath, []byte(localData), 0o644); err != nil {
		t.Fatalf("WriteFile local: %v", err)
	}

	globalPath := filepath.Join(sessionsDir, "rollout-global.jsonl")
	globalData := "" +
		"{\"timestamp\":\"2026-03-10T23:59:00Z\",\"type\":\"event_msg\",\"payload\":{\"type\":\"token_count\",\"info\":{\"last_token_usage\":{\"input_tokens\":1,\"output_tokens\":1,\"total_tokens\":2}},\"rate_limits\":{\"primary\":{\"used_percent\":88.0,\"window_minutes\":300,\"resets_at\":" + primaryReset + "},\"secondary\":{\"used_percent\":92.0,\"window_minutes\":10081,\"resets_at\":" + secondaryReset + "},\"plan_type\":\"plus\"}}}\n"
	if err := os.WriteFile(globalPath, []byte(globalData), 0o644); err != nil {
		t.Fatalf("WriteFile global: %v", err)
	}

	usage, err := getCodexContextUsage(sessionID)
	if err != nil {
		t.Fatalf("getCodexContextUsage: %v", err)
	}
	if usage.WeeklyQuota == nil || usage.WeeklyQuota.UsedPercent != 70 {
		t.Fatalf("WeeklyQuota = %#v, want local 70", usage.WeeklyQuota)
	}
	if len(usage.OtherQuotas) == 0 || usage.OtherQuotas[0].UsedPercent != 80 {
		t.Fatalf("OtherQuotas = %#v, want local primary 80", usage.OtherQuotas)
	}
}
