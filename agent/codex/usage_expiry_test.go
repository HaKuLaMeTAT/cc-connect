package codex

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

func TestGetCodexContextUsage_DropsExpiredQuotaWindows(t *testing.T) {
	homeDir := t.TempDir()
	t.Setenv("HOME", homeDir)
	t.Setenv("CODEX_HOME", filepath.Join(homeDir, ".codex"))

	sessionID := "session-expired-usage"
	sessionsDir := filepath.Join(homeDir, ".codex", "sessions", "2026", "03", "14")
	if err := os.MkdirAll(sessionsDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}

	now := time.Now().UTC()
	staleReset := now.Add(-90 * time.Minute).Unix()
	weeklyReset := now.Add(4 * 24 * time.Hour).Unix()
	initialTs := now.Add(-2 * time.Hour).Format(time.RFC3339Nano)
	laterTs := now.Add(-15 * time.Minute).Format(time.RFC3339Nano)

	path := filepath.Join(sessionsDir, "rollout-"+sessionID+".jsonl")
	data := "" +
		"{\"timestamp\":\"" + initialTs + "\",\"type\":\"session_meta\",\"payload\":{\"id\":\"" + sessionID + "\",\"cwd\":\"/tmp\"}}\n" +
		"{\"timestamp\":\"" + initialTs + "\",\"type\":\"event_msg\",\"payload\":{\"type\":\"token_count\",\"info\":{\"last_token_usage\":{\"input_tokens\":100,\"output_tokens\":20,\"total_tokens\":120}},\"rate_limits\":{\"primary\":{\"used_percent\":57.0,\"window_minutes\":300,\"resets_at\":" + itoa64(staleReset) + "},\"secondary\":{\"used_percent\":50.0,\"window_minutes\":10080,\"resets_at\":" + itoa64(weeklyReset) + "},\"plan_type\":\"plus\"}}}\n" +
		"{\"timestamp\":\"" + laterTs + "\",\"type\":\"event_msg\",\"payload\":{\"type\":\"token_count\",\"info\":{\"last_token_usage\":{\"input_tokens\":474982,\"output_tokens\":216,\"total_tokens\":475198}},\"rate_limits\":null}}\n"
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	usage, err := getCodexContextUsage(sessionID)
	if err != nil {
		t.Fatalf("getCodexContextUsage: %v", err)
	}
	if usage.PromptTokens != 474982 || usage.CompletionTokens != 216 || usage.TotalTokens != 475198 {
		t.Fatalf("usage tokens = %#v, want latest token_count payload", usage)
	}
	if usage.WeeklyQuota == nil {
		t.Fatal("WeeklyQuota = nil, want non-expired weekly quota")
	}
	if len(usage.OtherQuotas) != 0 {
		t.Fatalf("OtherQuotas = %#v, want expired primary quota filtered out", usage.OtherQuotas)
	}
}

func itoa64(v int64) string {
	return strconv.FormatInt(v, 10)
}
