package iflow

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/chenhg5/cc-connect/core"
)

func init() {
	core.RegisterAgent("iflow", New)
}

// Agent drives iFlow CLI one turn at a time using interactive `iflow -i`
// inside a PTY, then reconstructs streaming events from the transcript JSONL.
//
// Modes (maps to iFlow CLI flags):
//   - "default":   manual approval mode (--default)
//   - "auto-edit": auto-edit mode (--autoEdit)
//   - "plan":      read-only planning mode (--plan)
//   - "yolo":      auto-approve all tool calls (--yolo)
type Agent struct {
	workDir    string
	model      string
	mode       string
	cmd        string
	providers  []core.ProviderConfig
	activeIdx  int
	sessionEnv []string
	mu         sync.Mutex
}

func New(opts map[string]any) (core.Agent, error) {
	workDir, _ := opts["work_dir"].(string)
	if workDir == "" {
		workDir = "."
	}
	model, _ := opts["model"].(string)
	mode, _ := opts["mode"].(string)
	mode = normalizeMode(mode)
	cmd, _ := opts["cmd"].(string)
	if cmd == "" {
		cmd = "iflow"
	}

	if _, err := exec.LookPath(cmd); err != nil {
		return nil, fmt.Errorf("iflow: %q CLI not found in PATH, install with: npm i -g @iflow-ai/iflow-cli", cmd)
	}

	return &Agent{
		workDir:   workDir,
		model:     model,
		mode:      mode,
		cmd:       cmd,
		activeIdx: -1,
	}, nil
}

func normalizeMode(raw string) string {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "auto-edit", "auto_edit", "autoedit", "edit":
		return "auto-edit"
	case "plan":
		return "plan"
	case "yolo", "force", "auto", "bypasspermissions":
		return "yolo"
	default:
		return "default"
	}
}

func (a *Agent) Name() string { return "iflow" }

func (a *Agent) SetModel(model string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.model = model
	slog.Info("iflow: model changed", "model", model)
}

func (a *Agent) GetModel() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.model
}

func (a *Agent) AvailableModels(ctx context.Context) []core.ModelOption {
	return mergeIFlowModelOptions(
		a.fetchModelsFromAPI(ctx),
		readIFlowSeenModels(a.workDir),
		defaultIFlowModels(),
	)
}

func defaultIFlowModels() []core.ModelOption {
	return []core.ModelOption{
		{Name: "glm-5", Desc: "GLM-5"},
		{Name: "iflow-rome-30ba3b", Desc: "iFlow"},
		{Name: "qwen3-coder-plus", Desc: "Qwen3 Coder Plus"},
		{Name: "qwen3-max", Desc: "Qwen3 Max"},
		{Name: "qwen3-max-preview", Desc: "Qwen3 Max Preview"},
		{Name: "qwen3-vl-plus", Desc: "Qwen3 VL Plus"},
		{Name: "Qwen3-Coder", Desc: "Qwen3 Coder"},
		{Name: "Kimi-K2.5", Desc: "Kimi K2.5"},
		{Name: "DeepSeek-v3", Desc: "DeepSeek v3"},
		{Name: "kimi-k2", Desc: "Kimi K2"},
		{Name: "kimi-k2-0905", Desc: "Kimi K2 0905"},
		{Name: "deepseek-v3.2", Desc: "DeepSeek v3.2"},
		{Name: "deepseek-r1", Desc: "DeepSeek R1"},
		{Name: "qwen3-32b", Desc: "Qwen3 32B"},
		{Name: "qwen3-235b", Desc: "Qwen3 235B"},
		{Name: "qwen3-235b-a22b-thinking-2507", Desc: "Qwen3 235B Thinking"},
		{Name: "qwen3-235b-a22b-instruct", Desc: "Qwen3 235B Instruct"},
	}
}

func (a *Agent) fetchModelsFromAPI(ctx context.Context) []core.ModelOption {
	apiKey, baseURL := a.iflowCredentials()
	if apiKey == "" || baseURL == "" {
		return nil
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(baseURL, "/")+"/models", nil)
	if err != nil {
		return nil
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		slog.Debug("iflow: failed to fetch models", "error", err)
		return nil
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil
	}

	var result struct {
		Data []struct {
			ID          string `json:"id"`
			DisplayName string `json:"display_name"`
			OwnedBy     string `json:"owned_by"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil
	}

	var models []core.ModelOption
	for _, m := range result.Data {
		name := strings.TrimSpace(m.ID)
		if name == "" {
			continue
		}
		desc := strings.TrimSpace(m.DisplayName)
		if desc == "" {
			desc = strings.TrimSpace(m.OwnedBy)
		}
		models = append(models, core.ModelOption{Name: name, Desc: desc})
	}
	sort.Slice(models, func(i, j int) bool { return models[i].Name < models[j].Name })
	return models
}

func (a *Agent) iflowCredentials() (apiKey, baseURL string) {
	a.mu.Lock()
	if a.activeIdx >= 0 && a.activeIdx < len(a.providers) {
		apiKey = strings.TrimSpace(a.providers[a.activeIdx].APIKey)
		baseURL = strings.TrimSpace(a.providers[a.activeIdx].BaseURL)
	}
	a.mu.Unlock()

	if apiKey == "" {
		apiKey = strings.TrimSpace(os.Getenv("IFLOW_API_KEY"))
	}
	if apiKey == "" {
		apiKey = strings.TrimSpace(os.Getenv("IFLOW_apiKey"))
	}
	if baseURL == "" {
		baseURL = strings.TrimSpace(os.Getenv("IFLOW_BASE_URL"))
	}
	if baseURL == "" {
		baseURL = strings.TrimSpace(os.Getenv("IFLOW_baseUrl"))
	}
	if apiKey == "" || baseURL == "" {
		if cfgKey, cfgURL := readIFlowSettingsCredentials(); apiKey == "" || baseURL == "" {
			if apiKey == "" {
				apiKey = cfgKey
			}
			if baseURL == "" {
				baseURL = cfgURL
			}
		}
	}
	return apiKey, baseURL
}

func readIFlowSettingsCredentials() (apiKey, baseURL string) {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return "", ""
	}
	data, err := os.ReadFile(filepath.Join(homeDir, ".iflow", "settings.json"))
	if err != nil {
		return "", ""
	}
	var cfg struct {
		APIKey  string `json:"apiKey"`
		BaseURL string `json:"baseUrl"`
	}
	if json.Unmarshal(data, &cfg) != nil {
		return "", ""
	}
	return strings.TrimSpace(cfg.APIKey), strings.TrimSpace(cfg.BaseURL)
}

func readIFlowSeenModels(workDir string) []core.ModelOption {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return nil
	}

	sessionDir := filepath.Join(homeDir, ".iflow", "projects", iflowProjectKey(iflowResolvedWorkDir(workDir)))
	entries, err := os.ReadDir(sessionDir)
	if err != nil {
		return nil
	}

	var models []core.ModelOption
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".jsonl") {
			continue
		}
		path := filepath.Join(sessionDir, entry.Name())
		f, err := os.Open(path)
		if err != nil {
			continue
		}
		scanner := bufio.NewScanner(f)
		scanner.Buffer(make([]byte, 1024*64), 1024*1024)
		for scanner.Scan() {
			var item struct {
				Message struct {
					Model string `json:"model"`
				} `json:"message"`
			}
			if json.Unmarshal([]byte(strings.TrimSpace(scanner.Text())), &item) == nil && strings.TrimSpace(item.Message.Model) != "" {
				models = append(models, core.ModelOption{Name: strings.TrimSpace(item.Message.Model)})
			}
		}
		f.Close()
	}

	if settingsModel := readIFlowConfiguredModel(); settingsModel != "" {
		models = append(models, core.ModelOption{Name: settingsModel})
	}
	return models
}

func readIFlowConfiguredModel() string {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	data, err := os.ReadFile(filepath.Join(homeDir, ".iflow", "settings.json"))
	if err != nil {
		return ""
	}
	var cfg struct {
		ModelName string `json:"modelName"`
	}
	if json.Unmarshal(data, &cfg) != nil {
		return ""
	}
	return strings.TrimSpace(cfg.ModelName)
}

func mergeIFlowModelOptions(groups ...[]core.ModelOption) []core.ModelOption {
	seen := make(map[string]struct{})
	var merged []core.ModelOption
	for _, group := range groups {
		for _, model := range group {
			name := strings.TrimSpace(model.Name)
			if name == "" {
				continue
			}
			if _, ok := seen[strings.ToLower(name)]; ok {
				continue
			}
			seen[strings.ToLower(name)] = struct{}{}
			model.Name = name
			merged = append(merged, model)
		}
	}
	return merged
}

func (a *Agent) SetSessionEnv(env []string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.sessionEnv = env
}

func (a *Agent) StartSession(ctx context.Context, sessionID string) (core.AgentSession, error) {
	a.mu.Lock()
	model := a.model
	mode := a.mode
	cmd := a.cmd
	workDir := a.workDir
	extraEnv := a.providerEnvLocked()
	extraEnv = append(extraEnv, a.sessionEnv...)
	if a.activeIdx >= 0 && a.activeIdx < len(a.providers) {
		if m := a.providers[a.activeIdx].Model; m != "" {
			model = m
		}
	}
	a.mu.Unlock()

	return newIFlowSession(ctx, cmd, workDir, model, mode, sessionID, extraEnv)
}

func (a *Agent) GetContextUsage(_ context.Context, sessionID string) (*core.ContextUsage, error) {
	return getIFlowContextUsage(a.workDir, sessionID)
}

func (a *Agent) ListSessions(_ context.Context) ([]core.AgentSessionInfo, error) {
	return listIFlowSessions(a.workDir)
}

func (a *Agent) DeleteSession(_ context.Context, sessionID string) error {
	if sessionID == "" {
		return fmt.Errorf("session id is required")
	}

	homeDir, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("iflow: cannot determine home dir: %w", err)
	}

	absDir := iflowResolvedWorkDir(a.workDir)

	candidates := []string{
		filepath.Join(homeDir, ".iflow", "projects", iflowProjectKey(absDir), sessionID+".jsonl"),
	}
	for _, p := range candidates {
		if _, err := os.Stat(p); err == nil {
			return os.Remove(p)
		}
	}

	matches, _ := filepath.Glob(filepath.Join(homeDir, ".iflow", "projects", "*", sessionID+".jsonl"))
	if len(matches) == 0 {
		return fmt.Errorf("session file not found: %s", sessionID)
	}
	return os.Remove(matches[0])
}

func (a *Agent) Stop() error { return nil }

// -- ModeSwitcher --

func (a *Agent) SetMode(mode string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.mode = normalizeMode(mode)
	slog.Info("iflow: mode changed", "mode", a.mode)
}

func (a *Agent) GetMode() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.mode
}

func (a *Agent) PermissionModes() []core.PermissionModeInfo {
	return []core.PermissionModeInfo{
		{Key: "default", Name: "Default", NameZh: "默认", Desc: "Manual approval mode", DescZh: "手动审批模式"},
		{Key: "auto-edit", Name: "Auto Edit", NameZh: "自动编辑", Desc: "Auto-edit mode", DescZh: "自动编辑模式"},
		{Key: "plan", Name: "Plan", NameZh: "规划模式", Desc: "Read-only planning mode", DescZh: "只读规划模式"},
		{Key: "yolo", Name: "YOLO", NameZh: "全自动", Desc: "Auto-approve all tool calls", DescZh: "自动批准所有工具调用"},
	}
}

// -- ContextCompressor --

func (a *Agent) CompressCommand() string { return "/compress" }

// -- MemoryFileProvider --

func (a *Agent) ProjectMemoryFile() string {
	absDir, err := filepath.Abs(a.workDir)
	if err != nil {
		absDir = a.workDir
	}
	return filepath.Join(absDir, "IFLOW.md")
}

func (a *Agent) GlobalMemoryFile() string {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(homeDir, ".iflow", "IFLOW.md")
}

// -- ProviderSwitcher --

func (a *Agent) SetProviders(providers []core.ProviderConfig) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.providers = providers
}

func (a *Agent) SetActiveProvider(name string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if name == "" {
		a.activeIdx = -1
		slog.Info("iflow: provider cleared")
		return true
	}
	for i, p := range a.providers {
		if p.Name == name {
			a.activeIdx = i
			slog.Info("iflow: provider switched", "provider", name)
			return true
		}
	}
	return false
}

func (a *Agent) GetActiveProvider() *core.ProviderConfig {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.activeIdx < 0 || a.activeIdx >= len(a.providers) {
		return nil
	}
	p := a.providers[a.activeIdx]
	return &p
}

func (a *Agent) ListProviders() []core.ProviderConfig {
	a.mu.Lock()
	defer a.mu.Unlock()
	result := make([]core.ProviderConfig, len(a.providers))
	copy(result, a.providers)
	return result
}

func (a *Agent) providerEnvLocked() []string {
	if a.activeIdx < 0 || a.activeIdx >= len(a.providers) {
		return nil
	}
	p := a.providers[a.activeIdx]
	var env []string
	if p.APIKey != "" {
		env = append(env,
			"IFLOW_API_KEY="+p.APIKey,
			"IFLOW_apiKey="+p.APIKey,
		)
	}
	if p.BaseURL != "" {
		env = append(env,
			"IFLOW_BASE_URL="+p.BaseURL,
			"IFLOW_baseUrl="+p.BaseURL,
		)
	}
	for k, v := range p.Env {
		env = append(env, k+"="+v)
	}
	return env
}

// -- Session listing helpers --

type iflowTranscriptLine struct {
	SessionID string    `json:"sessionId"`
	Type      string    `json:"type"`
	Timestamp time.Time `json:"timestamp"`
	Message   struct {
		Role    string         `json:"role"`
		Content any            `json:"content"`
		Usage   map[string]any `json:"usage"`
	} `json:"message"`
}

func getIFlowContextUsage(workDir, sessionID string) (*core.ContextUsage, error) {
	path := findIFlowSessionFile(workDir, sessionID)
	if path == "" {
		return nil, fmt.Errorf("session file not found: %s", sessionID)
	}

	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	usage := &core.ContextUsage{}

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 1024*64), 1024*1024)

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}

		var item iflowTranscriptLine
		if err := json.Unmarshal([]byte(line), &item); err != nil {
			continue
		}
		if item.Type != "assistant" || item.Message.Role != "assistant" || len(item.Message.Usage) == 0 {
			continue
		}

		usage = iflowUsageFromMap(item.Message.Usage)
	}

	if err := scanner.Err(); err != nil {
		return nil, err
	}

	return usage, nil
}

func findIFlowSessionFile(workDir, sessionID string) string {
	if sessionID == "" {
		return ""
	}

	homeDir, err := os.UserHomeDir()
	if err != nil {
		return ""
	}

	absDir := iflowResolvedWorkDir(workDir)
	candidates := []string{
		filepath.Join(homeDir, ".iflow", "projects", iflowProjectKey(absDir), sessionID+".jsonl"),
	}
	for _, path := range candidates {
		if _, err := os.Stat(path); err == nil {
			return path
		}
	}

	matches, _ := filepath.Glob(filepath.Join(homeDir, ".iflow", "projects", "*", sessionID+".jsonl"))
	if len(matches) > 0 {
		return matches[0]
	}
	return ""
}

func iflowUsageFromMap(tokens map[string]any) *core.ContextUsage {
	if tokens == nil {
		return &core.ContextUsage{}
	}

	usage := &core.ContextUsage{
		PromptTokens:     iflowTokenInt(tokens, "prompt_tokens", "input_tokens", "inputTokens"),
		CompletionTokens: iflowTokenInt(tokens, "completion_tokens", "output_tokens", "outputTokens"),
		TotalTokens:      iflowTokenInt(tokens, "total_tokens", "totalTokens"),
	}
	if usage.TotalTokens == 0 {
		usage.TotalTokens = usage.PromptTokens + usage.CompletionTokens
	}
	return usage
}

func iflowTokenInt(tokens map[string]any, keys ...string) int {
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

func listIFlowSessions(workDir string) ([]core.AgentSessionInfo, error) {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("iflow: cannot determine home dir: %w", err)
	}

	absDir := iflowResolvedWorkDir(workDir)

	sessionDir := filepath.Join(homeDir, ".iflow", "projects", iflowProjectKey(absDir))
	entries, err := os.ReadDir(sessionDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("iflow: read sessions dir: %w", err)
	}

	var sessions []core.AgentSessionInfo
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasPrefix(name, "session-") || !strings.HasSuffix(name, ".jsonl") {
			continue
		}

		path := filepath.Join(sessionDir, name)
		sid, summary, msgCount, modifiedAt := parseIFlowSessionFile(path)
		if sid == "" {
			sid = strings.TrimSuffix(name, ".jsonl")
		}
		if summary == "" {
			summary = sid
			if utf8.RuneCountInString(summary) > 12 {
				summary = string([]rune(summary)[:12]) + "..."
			}
		}
		if modifiedAt.IsZero() {
			if fi, err := e.Info(); err == nil {
				modifiedAt = fi.ModTime()
			}
		}

		sessions = append(sessions, core.AgentSessionInfo{
			ID:           sid,
			Summary:      summary,
			MessageCount: msgCount,
			ModifiedAt:   modifiedAt,
		})
	}

	sort.Slice(sessions, func(i, j int) bool {
		return sessions[i].ModifiedAt.After(sessions[j].ModifiedAt)
	})
	return sessions, nil
}

func parseIFlowSessionFile(path string) (sid, summary string, msgCount int, modifiedAt time.Time) {
	f, err := os.Open(path)
	if err != nil {
		return "", "", 0, time.Time{}
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 1024*64), 1024*1024)

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		msgCount++

		var item iflowTranscriptLine
		if err := json.Unmarshal([]byte(line), &item); err != nil {
			continue
		}
		if sid == "" && item.SessionID != "" {
			sid = item.SessionID
		}
		if !item.Timestamp.IsZero() && item.Timestamp.After(modifiedAt) {
			modifiedAt = item.Timestamp
		}
		if summary == "" && item.Type == "user" {
			if text := extractIFlowContentText(item.Message.Content); text != "" {
				summary = firstNonEmptyLine(text)
				if utf8.RuneCountInString(summary) > 60 {
					summary = string([]rune(summary)[:60]) + "..."
				}
			}
		}
	}

	return sid, summary, msgCount, modifiedAt
}

func extractIFlowContentText(content any) string {
	switch v := content.(type) {
	case string:
		return strings.TrimSpace(v)
	case []any:
		for _, it := range v {
			m, ok := it.(map[string]any)
			if !ok {
				continue
			}
			if t, _ := m["text"].(string); t != "" {
				return strings.TrimSpace(t)
			}
		}
	}
	return ""
}

func firstNonEmptyLine(s string) string {
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			return line
		}
	}
	return ""
}

func iflowProjectKey(absDir string) string {
	if absDir == "" {
		return ""
	}
	key := filepath.Clean(absDir)
	key = strings.ReplaceAll(key, "\\", "-")
	key = strings.ReplaceAll(key, "/", "-")
	key = strings.ReplaceAll(key, ":", "-")
	return key
}

func iflowResolvedWorkDir(workDir string) string {
	absDir, err := filepath.Abs(workDir)
	if err != nil {
		absDir = workDir
	}
	if resolved, err := filepath.EvalSymlinks(absDir); err == nil && resolved != "" {
		absDir = resolved
	}
	return absDir
}
