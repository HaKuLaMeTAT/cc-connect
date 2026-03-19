package codex

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/chenhg5/cc-connect/core"
)

// listCodexSessions scans ~/.codex/sessions/ for JSONL transcript files
// whose cwd matches workDir.
func listCodexSessions(workDir string) ([]core.AgentSessionInfo, error) {
	absWorkDir, err := filepath.Abs(workDir)
	if err != nil {
		absWorkDir = workDir
	}

	homeDir, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}

	codexHome := os.Getenv("CODEX_HOME")
	if codexHome == "" {
		codexHome = filepath.Join(homeDir, ".codex")
	}
	sessionsDir := filepath.Join(codexHome, "sessions")

	var files []string
	_ = filepath.Walk(sessionsDir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		if strings.HasSuffix(path, ".jsonl") {
			files = append(files, path)
		}
		return nil
	})

	if len(files) == 0 {
		return nil, nil
	}

	var sessions []core.AgentSessionInfo
	for _, f := range files {
		info := parseCodexSessionFile(f, absWorkDir)
		if info != nil {
			patchSessionSource(info.ID)
			sessions = append(sessions, *info)
		}
	}

	sort.Slice(sessions, func(i, j int) bool {
		return sessions[i].ModifiedAt.After(sessions[j].ModifiedAt)
	})

	return sessions, nil
}

// parseCodexSessionFile reads a Codex JSONL transcript.
// Returns nil if the session's cwd doesn't match filterCwd.
func parseCodexSessionFile(path, filterCwd string) *core.AgentSessionInfo {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()

	stat, err := f.Stat()
	if err != nil {
		return nil
	}

	var sessionID string
	var sessionCwd string
	var summary string
	var msgCount int
	userMsgSeen := 0

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 256*1024), 256*1024)

	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			continue
		}

		var entry struct {
			Type    string          `json:"type"`
			Payload json.RawMessage `json:"payload"`
		}
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			continue
		}

		switch entry.Type {
		case "session_meta":
			var meta struct {
				ID  string `json:"id"`
				Cwd string `json:"cwd"`
			}
			if json.Unmarshal(entry.Payload, &meta) == nil {
				sessionID = meta.ID
				sessionCwd = meta.Cwd
			}

		case "response_item":
			var item struct {
				Role    string `json:"role"`
				Content []struct {
					Type string `json:"type"`
					Text string `json:"text"`
				} `json:"content"`
			}
			if json.Unmarshal(entry.Payload, &item) == nil {
				if item.Role == "user" {
					userMsgSeen++
					msgCount++
					// The actual user prompt is the last user response_item
					// (earlier ones are system/AGENTS.md instructions).
					// Pick the last content block that looks like a real prompt.
					for _, c := range item.Content {
						if c.Type == "input_text" && c.Text != "" && isUserPrompt(c.Text) {
							summary = c.Text
						}
					}
				} else if item.Role == "assistant" {
					msgCount++
				}
			}
		}
	}

	// Filter by cwd
	if filterCwd != "" && sessionCwd != "" && sessionCwd != filterCwd {
		return nil
	}

	if sessionID == "" {
		return nil
	}

	if len([]rune(summary)) > 60 {
		summary = string([]rune(summary)[:60]) + "..."
	}

	return &core.AgentSessionInfo{
		ID:           sessionID,
		Summary:      summary,
		MessageCount: msgCount,
		ModifiedAt:   stat.ModTime(),
	}
}

// findSessionFile locates the JSONL transcript for a given session ID.
func findSessionFile(sessionID string) string {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	codexHome := os.Getenv("CODEX_HOME")
	if codexHome == "" {
		codexHome = filepath.Join(homeDir, ".codex")
	}
	sessionsDir := filepath.Join(codexHome, "sessions")

	patterns := []string{
		filepath.Join(sessionsDir, "*"+sessionID+"*.jsonl"),
		filepath.Join(sessionsDir, "*", "*", "*", "*"+sessionID+"*.jsonl"),
	}
	for _, pattern := range patterns {
		matches, err := filepath.Glob(pattern)
		if err != nil || len(matches) == 0 {
			continue
		}
		sort.Strings(matches)
		return matches[0]
	}

	var found string
	_ = filepath.Walk(sessionsDir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || found != "" {
			return nil
		}
		if strings.Contains(filepath.Base(path), sessionID) {
			found = path
		}
		return nil
	})
	return found
}

func getCodexContextUsage(sessionID string) (*core.ContextUsage, error) {
	path := findSessionFile(sessionID)
	if path == "" {
		return nil, fmt.Errorf("session file not found for %s", sessionID)
	}

	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	usage := &core.ContextUsage{}
	var sessionRateLimits codexRateLimitsSnapshot

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 256*1024), 1024*1024)

	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			continue
		}

		var raw struct {
			Timestamp string          `json:"timestamp"`
			Type      string          `json:"type"`
			Payload   json.RawMessage `json:"payload"`
		}
		if json.Unmarshal([]byte(line), &raw) != nil || raw.Type != "event_msg" {
			continue
		}

		var payload struct {
			Type string `json:"type"`
			Info struct {
				LastTokenUsage  map[string]any `json:"last_token_usage"`
				TotalTokenUsage map[string]any `json:"total_token_usage"`
			} `json:"info"`
			RateLimits *codexRateLimits `json:"rate_limits"`
		}
		if json.Unmarshal(raw.Payload, &payload) != nil || payload.Type != "token_count" {
			continue
		}
		tokens := payload.Info.LastTokenUsage
		if len(tokens) == 0 {
			tokens = payload.Info.TotalTokenUsage
		}
		if len(tokens) == 0 {
			continue
		}

		usage = usageFromTokenMap(tokens)
		ts, _ := time.Parse(time.RFC3339Nano, raw.Timestamp)
		if payload.RateLimits != nil {
			sessionRateLimits = codexRateLimitsSnapshot{
				RateLimits: payload.RateLimits,
				Timestamp:  ts,
			}
		}
		applyCodexRateLimits(usage, sessionRateLimits.RateLimits)
	}

	if err := scanner.Err(); err != nil {
		return nil, err
	}

	globalRateLimits := latestCodexGlobalRateLimits()
	if shouldApplyGlobalCodexRateLimits(globalRateLimits, sessionRateLimits) {
		applyCodexRateLimits(usage, globalRateLimits.RateLimits)
	}

	return usage, nil
}

type codexRateLimits struct {
	Primary   *codexRateLimitWindow `json:"primary"`
	Secondary *codexRateLimitWindow `json:"secondary"`
	PlanType  string                `json:"plan_type"`
}

type codexRateLimitWindow struct {
	UsedPercent   float64 `json:"used_percent"`
	WindowMinutes int     `json:"window_minutes"`
	ResetsAt      int64   `json:"resets_at"`
}

type codexRateLimitsSnapshot struct {
	RateLimits *codexRateLimits
	Timestamp  time.Time
}

func usageFromTokenMap(tokens map[string]any) *core.ContextUsage {
	if tokens == nil {
		return &core.ContextUsage{}
	}

	usage := &core.ContextUsage{
		PromptTokens:     tokenMapInt(tokens, "prompt_tokens", "input_tokens", "inputTokens"),
		CompletionTokens: tokenMapInt(tokens, "completion_tokens", "output_tokens", "outputTokens"),
		TotalTokens:      tokenMapInt(tokens, "total_tokens", "totalTokens"),
	}
	if usage.TotalTokens == 0 {
		usage.TotalTokens = usage.PromptTokens + usage.CompletionTokens
	}
	return usage
}

func tokenMapInt(tokens map[string]any, keys ...string) int {
	for _, key := range keys {
		v, ok := tokens[key]
		if !ok {
			continue
		}
		switch n := v.(type) {
		case float64:
			return int(n)
		case float32:
			return int(n)
		case int:
			return n
		case int64:
			return int(n)
		case json.Number:
			i, err := n.Int64()
			if err == nil {
				return int(i)
			}
		}
	}
	return 0
}

func applyCodexRateLimits(usage *core.ContextUsage, limits *codexRateLimits) {
	if usage == nil || limits == nil {
		return
	}
	usage.DailyQuota = nil
	usage.WeeklyQuota = nil
	usage.OtherQuotas = nil
	if limits.PlanType != "" {
		usage.PlanType = limits.PlanType
	}

	windows := make([]core.UsageQuota, 0, 2)
	if limits.Primary != nil {
		if quota, ok := quotaFromCodexWindow("primary", limits.Primary, time.Now()); ok {
			windows = append(windows, quota)
		}
	}
	if limits.Secondary != nil {
		if quota, ok := quotaFromCodexWindow("secondary", limits.Secondary, time.Now()); ok {
			windows = append(windows, quota)
		}
	}
	if len(windows) == 0 {
		return
	}

	dailyIdx := findQuotaByWindow(windows, 18*60, 30*60)
	weeklyIdx := findQuotaByWindow(windows, 6*24*60, 8*24*60)

	used := make(map[int]struct{}, 2)
	if dailyIdx >= 0 {
		usage.DailyQuota = &windows[dailyIdx]
		used[dailyIdx] = struct{}{}
	}
	if weeklyIdx >= 0 {
		usage.WeeklyQuota = &windows[weeklyIdx]
		used[weeklyIdx] = struct{}{}
	}
	for i, quota := range windows {
		if _, ok := used[i]; ok {
			continue
		}
		usage.OtherQuotas = append(usage.OtherQuotas, quota)
	}
}

func quotaFromCodexWindow(label string, window *codexRateLimitWindow, now time.Time) (core.UsageQuota, bool) {
	quota := core.UsageQuota{
		Label:         label,
		WindowMinutes: window.WindowMinutes,
		UsedPercent:   codexQuotaDisplayPercent(window.UsedPercent),
	}
	if window.ResetsAt > 0 {
		quota.ResetsAt = time.Unix(window.ResetsAt, 0)
		if !now.IsZero() && quota.ResetsAt.Before(now) {
			return core.UsageQuota{}, false
		}
	}
	return quota, true
}

func codexQuotaDisplayPercent(usedPercent float64) float64 {
	remaining := 100 - usedPercent
	if remaining < 0 {
		remaining = 0
	}
	if remaining >= 100 {
		return 100
	}
	bucket := math.Floor(remaining/10.0) * 10
	if bucket == 0 && remaining > 0 {
		return 10
	}
	return bucket
}

func findQuotaByWindow(quotas []core.UsageQuota, minMinutes, maxMinutes int) int {
	for i, quota := range quotas {
		if quota.WindowMinutes >= minMinutes && quota.WindowMinutes <= maxMinutes {
			return i
		}
	}
	return -1
}

func shouldApplyGlobalCodexRateLimits(global, session codexRateLimitsSnapshot) bool {
	if global.RateLimits == nil {
		return false
	}
	if session.RateLimits == nil {
		return true
	}
	if global.Timestamp.IsZero() || session.Timestamp.IsZero() {
		return false
	}
	return global.Timestamp.After(session.Timestamp)
}

func latestCodexGlobalRateLimits() codexRateLimitsSnapshot {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return codexRateLimitsSnapshot{}
	}
	codexHome := os.Getenv("CODEX_HOME")
	if codexHome == "" {
		codexHome = filepath.Join(homeDir, ".codex")
	}

	var dirs []string
	for _, dir := range []string{
		filepath.Join(codexHome, "sessions"),
		filepath.Join(codexHome, "archived_sessions"),
	} {
		if fi, err := os.Stat(dir); err == nil && fi.IsDir() {
			dirs = append(dirs, dir)
		}
	}

	best := codexRateLimitsSnapshot{}
	for _, dir := range dirs {
		_ = filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
			if err != nil || info.IsDir() || !strings.HasSuffix(path, ".jsonl") {
				return nil
			}

			f, err := os.Open(path)
			if err != nil {
				return nil
			}
			defer f.Close()

			scanner := bufio.NewScanner(f)
			scanner.Buffer(make([]byte, 256*1024), 1024*1024)
			for scanner.Scan() {
				line := scanner.Text()
				if line == "" {
					continue
				}
				var raw struct {
					Timestamp string          `json:"timestamp"`
					Type      string          `json:"type"`
					Payload   json.RawMessage `json:"payload"`
				}
				if json.Unmarshal([]byte(line), &raw) != nil || raw.Type != "event_msg" {
					continue
				}
				var payload struct {
					Type       string           `json:"type"`
					RateLimits *codexRateLimits `json:"rate_limits"`
				}
				if json.Unmarshal(raw.Payload, &payload) != nil || payload.Type != "token_count" || payload.RateLimits == nil {
					continue
				}
				ts, err := time.Parse(time.RFC3339Nano, raw.Timestamp)
				if err != nil {
					continue
				}
				if best.RateLimits == nil || ts.After(best.Timestamp) {
					copy := *payload.RateLimits
					best = codexRateLimitsSnapshot{
						RateLimits: &copy,
						Timestamp:  ts,
					}
				}
			}
			return nil
		})
	}
	return best
}

// getSessionHistory reads the JSONL transcript and returns user/assistant messages.
func getSessionHistory(sessionID string, limit int) ([]core.HistoryEntry, error) {
	path := findSessionFile(sessionID)
	if path == "" {
		return nil, fmt.Errorf("session file not found for %s", sessionID)
	}

	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var entries []core.HistoryEntry

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 256*1024), 256*1024)

	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			continue
		}

		var raw struct {
			Timestamp string          `json:"timestamp"`
			Type      string          `json:"type"`
			Payload   json.RawMessage `json:"payload"`
		}
		if json.Unmarshal([]byte(line), &raw) != nil {
			continue
		}
		if raw.Type != "response_item" {
			continue
		}

		var item struct {
			Role    string `json:"role"`
			Type    string `json:"type"`
			Text    string `json:"text"`
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		}
		if json.Unmarshal(raw.Payload, &item) != nil {
			continue
		}

		ts, _ := time.Parse(time.RFC3339Nano, raw.Timestamp)

		switch {
		case item.Role == "user" && len(item.Content) > 0:
			for _, c := range item.Content {
				if c.Type == "input_text" && c.Text != "" && isUserPrompt(c.Text) {
					entries = append(entries, core.HistoryEntry{
						Role: "user", Content: c.Text, Timestamp: ts,
					})
				}
			}
		case item.Role == "assistant" && len(item.Content) > 0:
			for _, c := range item.Content {
				if c.Type == "output_text" && c.Text != "" {
					entries = append(entries, core.HistoryEntry{
						Role: "assistant", Content: c.Text, Timestamp: ts,
					})
				}
			}
		case item.Type == "reasoning" && item.Text != "":
			// skip reasoning items
		}
	}

	if limit > 0 && len(entries) > limit {
		entries = entries[len(entries)-limit:]
	}
	return entries, nil
}

// patchSessionSource rewrites the session_meta line in a Codex JSONL transcript
// so that source="cli" and originator="codex_cli_rs", making the session visible
// in the interactive `codex` terminal.
func patchSessionSource(sessionID string) {
	path := findSessionFile(sessionID)
	if path == "" {
		return
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return
	}

	idx := bytes.IndexByte(data, '\n')
	if idx < 0 {
		return
	}
	firstLine := data[:idx]

	// Only patch if it's actually an exec-sourced session
	if !bytes.Contains(firstLine, []byte(`"source":"exec"`)) {
		return
	}

	patched := bytes.Replace(firstLine, []byte(`"source":"exec"`), []byte(`"source":"cli"`), 1)
	patched = bytes.Replace(patched, []byte(`"originator":"codex_exec"`), []byte(`"originator":"codex_cli_rs"`), 1)

	if bytes.Equal(patched, firstLine) {
		return
	}

	out := make([]byte, 0, len(patched)+len(data)-idx)
	out = append(out, patched...)
	out = append(out, data[idx:]...)

	_ = os.WriteFile(path, out, 0o644)
}

// isUserPrompt returns true if the text looks like an actual user prompt
// rather than system context (AGENTS.md, environment_context, permissions, etc.)
func isUserPrompt(text string) bool {
	t := strings.TrimSpace(text)
	if t == "" {
		return false
	}
	// Skip XML-style system context
	if strings.HasPrefix(t, "<") {
		return false
	}
	// Skip AGENTS.md instructions injected by Codex
	if strings.HasPrefix(t, "# AGENTS.md") || strings.HasPrefix(t, "#AGENTS.md") {
		return false
	}
	return true
}
