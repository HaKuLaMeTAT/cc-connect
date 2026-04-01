package qwen

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

	"github.com/chenhg5/cc-connect/core"
)

func init() {
	core.RegisterAgent("qwen", New)
}

// Agent drives Qwen Code CLI using `qwen --output-format stream-json`.
//
// Permission modes (maps to Qwen Code's --approval-mode or --yolo):
//   - "default":  every tool call requires user approval
//   - "auto-edit": auto-approve file edit tools, ask for others
//   - "yolo":     auto-approve everything
type Agent struct {
	workDir      string
	model        string
	mode         string // "default" | "auto-edit" | "yolo"
	allowedTools []string
	providers    []core.ProviderConfig
	activeIdx    int // -1 = no provider set
	sessionEnv   []string

	mu sync.Mutex
}

func New(opts map[string]any) (core.Agent, error) {
	workDir, _ := opts["work_dir"].(string)
	if workDir == "" {
		workDir = "."
	}
	model, _ := opts["model"].(string)
	mode, _ := opts["mode"].(string)
	mode = normalizePermissionMode(mode)

	var allowedTools []string
	if tools, ok := opts["allowed_tools"].([]any); ok {
		for _, t := range tools {
			if s, ok := t.(string); ok {
				allowedTools = append(allowedTools, s)
			}
		}
	}

	if _, err := exec.LookPath("qwen"); err != nil {
		return nil, fmt.Errorf("qwen: 'qwen' CLI not found in PATH, please install Qwen Code first:\n  curl -fsSL https://qwen.com/install | bash\n\nAfter installation, run 'qwen' once to authenticate via OAuth (qwen.ai login)")
	}

	return &Agent{
		workDir:      workDir,
		model:        model,
		mode:         mode,
		allowedTools: allowedTools,
		activeIdx:    -1,
	}, nil
}

// normalizePermissionMode maps user-friendly aliases to Qwen Code values.
func normalizePermissionMode(raw string) string {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "auto-edit", "autoedit", "auto_edit", "edit":
		return "auto-edit"
	case "yolo", "bypass", "dangerously-skip-permissions", "auto":
		return "yolo"
	default:
		return "default"
	}
}

func (a *Agent) Name() string { return "qwen" }

func (a *Agent) SetModel(model string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.model = model
	slog.Info("qwen: model changed", "model", model)
}

func (a *Agent) GetModel() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.model
}

func (a *Agent) AvailableModels(ctx context.Context) []core.ModelOption {
	if models := a.fetchModelsFromAPI(ctx); len(models) > 0 {
		return models
	}
	return []core.ModelOption{
		{Name: "qwen-max", Desc: "Qwen Max (most capable)"},
		{Name: "qwen-plus", Desc: "Qwen Plus (balanced)"},
		{Name: "qwen-turbo", Desc: "Qwen Turbo (fast)"},
		{Name: "qwen-coder-plus", Desc: "Qwen Coder Plus (code-optimized)"},
		{Name: "qwen-coder-turbo", Desc: "Qwen Coder Turbo (fast coding)"},
	}
}

func (a *Agent) fetchModelsFromAPI(ctx context.Context) []core.ModelOption {
	a.mu.Lock()
	apiKey := ""
	baseURL := ""
	if a.activeIdx >= 0 && a.activeIdx < len(a.providers) {
		apiKey = a.providers[a.activeIdx].APIKey
		baseURL = a.providers[a.activeIdx].BaseURL
	}
	a.mu.Unlock()

	// Try API key first
	if apiKey == "" {
		apiKey = os.Getenv("DASHSCOPE_API_KEY")
	}
	if apiKey == "" {
		apiKey = os.Getenv("OPENAI_API_KEY")
	}

	// If no API key, check if OAuth is available (Qwen Code CLI uses OAuth by default)
	// OAuth tokens are stored in ~/.qwen/ by the CLI
	if apiKey == "" {
		// OAuth mode: return built-in model list
		return nil
	}

	if baseURL == "" {
		baseURL = os.Getenv("DASHSCOPE_BASE_URL")
	}
	if baseURL == "" {
		baseURL = os.Getenv("OPENAI_BASE_URL")
	}
	if baseURL == "" {
		baseURL = "https://dashscope.aliyuncs.com/compatible-mode/v1"
	}
	baseURL = strings.TrimRight(baseURL, "/")

	req, err := http.NewRequestWithContext(ctx, "GET", baseURL+"/models", nil)
	if err != nil {
		return nil
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		slog.Debug("qwen: failed to fetch models", "error", err)
		return nil
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil
	}

	var result struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil
	}

	var models []core.ModelOption
	for _, m := range result.Data {
		if isLikelyQwenModel(m.ID) {
			models = append(models, core.ModelOption{Name: m.ID})
		}
	}
	sort.Slice(models, func(i, j int) bool { return models[i].Name < models[j].Name })
	return models
}

func isLikelyQwenModel(id string) bool {
	id = strings.ToLower(id)
	if id == "" {
		return false
	}
	// Filter out non-chat models
	for _, banned := range []string{"embedding", "moderation", "transcribe", "tts", "realtime", "audio", "vision", "search", "image", "whisper"} {
		if strings.Contains(id, banned) {
			return false
		}
	}
	return strings.Contains(id, "qwen") || strings.Contains(id, "dashscope")
}

func (a *Agent) SetSessionEnv(env []string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.sessionEnv = env
}

// StartSession creates a persistent interactive Qwen Code session.
func (a *Agent) StartSession(ctx context.Context, sessionID string) (core.AgentSession, error) {
	a.mu.Lock()
	tools := make([]string, len(a.allowedTools))
	copy(tools, a.allowedTools)
	model := a.model
	extraEnv := a.providerEnvLocked()
	extraEnv = append(extraEnv, a.sessionEnv...)

	if a.activeIdx >= 0 && a.activeIdx < len(a.providers) {
		if m := a.providers[a.activeIdx].Model; m != "" {
			model = m
		}
	}
	a.mu.Unlock()

	return newQwenSession(ctx, a.workDir, model, sessionID, a.mode, tools, extraEnv)
}

func (a *Agent) ListSessions(ctx context.Context) ([]core.AgentSessionInfo, error) {
	// Qwen Code stores sessions in ~/.qwen/projects/{projectKey}/
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("qwen: cannot determine home dir: %w", err)
	}

	absWorkDir, err := filepath.Abs(a.workDir)
	if err != nil {
		return nil, fmt.Errorf("qwen: resolve work_dir: %w", err)
	}

	projectDir := findProjectDir(homeDir, absWorkDir)
	if projectDir == "" {
		return nil, nil
	}

	entries, err := os.ReadDir(projectDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("qwen: read project dir: %w", err)
	}

	var sessions []core.AgentSessionInfo
	for _, entry := range entries {
		name := entry.Name()
		if !entry.IsDir() {
			continue
		}

		// Look for session metadata files
		metaPath := filepath.Join(projectDir, name, "session.json")
		info, err := os.Stat(metaPath)
		if err != nil {
			continue
		}

		summary, msgCount := scanSessionMeta(metaPath)

		sessions = append(sessions, core.AgentSessionInfo{
			ID:           name,
			Summary:      summary,
			MessageCount: msgCount,
			ModifiedAt:   info.ModTime(),
		})
	}

	sort.Slice(sessions, func(i, j int) bool {
		return sessions[i].ModifiedAt.After(sessions[j].ModifiedAt)
	})

	return sessions, nil
}

func (a *Agent) GetContextUsage(_ context.Context, sessionID string) (*core.ContextUsage, error) {
	return getQwenContextUsage(a.workDir, sessionID)
}

func (a *Agent) DeleteSession(_ context.Context, sessionID string) error {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("qwen: cannot determine home dir: %w", err)
	}
	absWorkDir, err := filepath.Abs(a.workDir)
	if err != nil {
		return fmt.Errorf("qwen: resolve work_dir: %w", err)
	}
	projectDir := findProjectDir(homeDir, absWorkDir)
	if projectDir == "" {
		return fmt.Errorf("session not found")
	}
	path := filepath.Join(projectDir, sessionID)
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return fmt.Errorf("session directory not found: %s", sessionID)
	}
	return os.RemoveAll(path)
}

func getQwenContextUsage(workDir, sessionID string) (*core.ContextUsage, error) {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("qwen: cannot determine home dir: %w", err)
	}
	absWorkDir, err := filepath.Abs(workDir)
	if err != nil {
		return nil, fmt.Errorf("qwen: resolve work_dir: %w", err)
	}

	projectDir := findProjectDir(homeDir, absWorkDir)
	if projectDir == "" {
		return nil, fmt.Errorf("qwen: project directory not found")
	}

	chatPath := filepath.Join(projectDir, "chats", sessionID+".jsonl")
	f, err := os.Open(chatPath)
	if err != nil {
		return nil, fmt.Errorf("qwen: open chat transcript: %w", err)
	}
	defer f.Close()

	usage := &core.ContextUsage{}
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 256*1024), 1024*1024)

	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}

		var entry struct {
			Type          string `json:"type"`
			UsageMetadata *struct {
				PromptTokenCount     int `json:"promptTokenCount"`
				CandidatesTokenCount int `json:"candidatesTokenCount"`
				TotalTokenCount      int `json:"totalTokenCount"`
			} `json:"usageMetadata"`
			SystemPayload *struct {
				UIEvent *struct {
					EventName        string `json:"event.name"`
					InputTokenCount  int    `json:"input_token_count"`
					OutputTokenCount int    `json:"output_token_count"`
					TotalTokenCount  int    `json:"total_token_count"`
				} `json:"uiEvent"`
			} `json:"systemPayload"`
		}
		if err := json.Unmarshal(line, &entry); err != nil {
			continue
		}

		switch {
		case entry.UsageMetadata != nil:
			usage.PromptTokens = entry.UsageMetadata.PromptTokenCount
			usage.CompletionTokens = entry.UsageMetadata.CandidatesTokenCount
			usage.TotalTokens = entry.UsageMetadata.TotalTokenCount
		case entry.Type == "system" && entry.SystemPayload != nil && entry.SystemPayload.UIEvent != nil &&
			entry.SystemPayload.UIEvent.EventName == "qwen-code.api_response":
			usage.PromptTokens = entry.SystemPayload.UIEvent.InputTokenCount
			usage.CompletionTokens = entry.SystemPayload.UIEvent.OutputTokenCount
			usage.TotalTokens = entry.SystemPayload.UIEvent.TotalTokenCount
		}
	}

	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("qwen: scan chat transcript: %w", err)
	}
	if usage.TotalTokens == 0 {
		usage.TotalTokens = usage.PromptTokens + usage.CompletionTokens
	}
	return usage, nil
}

func scanSessionMeta(path string) (string, int) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", 0
	}

	var session struct {
		Messages []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(b, &session); err != nil {
		return "", 0
	}

	var summary string
	count := 0
	for _, msg := range session.Messages {
		if msg.Role == "user" || msg.Role == "assistant" {
			count++
			if msg.Role == "user" && msg.Content != "" && summary == "" {
				summary = msg.Content
			}
		}
	}

	summary = strings.TrimSpace(summary)
	if len([]rune(summary)) > 40 {
		summary = string([]rune(summary)[:40]) + "..."
	}
	return summary, count
}

func (a *Agent) Stop() error { return nil }

// SetMode changes the permission mode for future sessions.
func (a *Agent) SetMode(mode string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.mode = normalizePermissionMode(mode)
	slog.Info("qwen: permission mode changed", "mode", a.mode)
}

// GetMode returns the current permission mode.
func (a *Agent) GetMode() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.mode
}

// PermissionModes returns all supported permission modes.
func (a *Agent) PermissionModes() []core.PermissionModeInfo {
	return []core.PermissionModeInfo{
		{Key: "default", Name: "Default", NameZh: "默认", Desc: "Ask permission for every tool call", DescZh: "每次工具调用都需确认"},
		{Key: "auto-edit", Name: "Auto Edit", NameZh: "自动编辑", Desc: "Auto-approve file edits, ask for others", DescZh: "自动允许文件编辑，其他需确认"},
		{Key: "yolo", Name: "YOLO", NameZh: "YOLO 模式", Desc: "Auto-approve everything", DescZh: "全部自动通过"},
	}
}

// AddAllowedTools adds tools to the pre-allowed list (takes effect on next session).
func (a *Agent) AddAllowedTools(tools ...string) error {
	a.mu.Lock()
	defer a.mu.Unlock()

	existing := make(map[string]bool)
	for _, t := range a.allowedTools {
		existing[t] = true
	}
	for _, tool := range tools {
		if !existing[tool] {
			a.allowedTools = append(a.allowedTools, tool)
			existing[tool] = true
		}
	}
	slog.Info("qwen: updated allowed tools", "tools", tools, "total", len(a.allowedTools))
	return nil
}

// GetAllowedTools returns the current list of pre-allowed tools.
func (a *Agent) GetAllowedTools() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	result := make([]string, len(a.allowedTools))
	copy(result, a.allowedTools)
	return result
}

// ── CommandProvider implementation ────────────────────────────

func (a *Agent) CommandDirs() []string {
	absDir, err := filepath.Abs(a.workDir)
	if err != nil {
		absDir = a.workDir
	}
	dirs := []string{filepath.Join(absDir, ".qwen", "commands")}
	if home, err := os.UserHomeDir(); err == nil {
		dirs = append(dirs, filepath.Join(home, ".qwen", "commands"))
	}
	return dirs
}

// ── SkillProvider implementation ──────────────────────────────

func (a *Agent) SkillDirs() []string {
	absDir, err := filepath.Abs(a.workDir)
	if err != nil {
		absDir = a.workDir
	}
	dirs := []string{filepath.Join(absDir, ".qwen", "skills")}
	if home, err := os.UserHomeDir(); err == nil {
		dirs = append(dirs, filepath.Join(home, ".qwen", "skills"))
	}
	return dirs
}

// ── ContextCompressor implementation ──────────────────────────

func (a *Agent) CompressCommand() string { return "/compact" }

// ── MemoryFileProvider implementation ─────────────────────────

func (a *Agent) ProjectMemoryFile() string {
	absDir, err := filepath.Abs(a.workDir)
	if err != nil {
		absDir = a.workDir
	}
	return filepath.Join(absDir, "QWEN.md")
}

func (a *Agent) GlobalMemoryFile() string {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(homeDir, ".qwen", "QWEN.md")
}

func (a *Agent) HasSystemPromptSupport() bool { return true }

// ── ProviderSwitcher implementation ──────────────────────────

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
		slog.Info("qwen: provider cleared")
		return true
	}
	for i, p := range a.providers {
		if p.Name == name {
			a.activeIdx = i
			slog.Info("qwen: provider switched", "provider", name)
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

	// Support both API key and OAuth modes
	// OAuth: Qwen Code CLI handles auth internally via ~/.qwen/ tokens
	// API Key: pass via environment variables
	if p.APIKey != "" {
		env = append(env, "DASHSCOPE_API_KEY="+p.APIKey)
	}
	if p.BaseURL != "" {
		env = append(env, "DASHSCOPE_BASE_URL="+p.BaseURL)
	}
	for k, v := range p.Env {
		env = append(env, k+"="+v)
	}
	return env
}

// findProjectDir locates the Qwen Code session directory for a given work dir.
// Qwen Code stores sessions at ~/.qwen/projects/{projectKey}/ where projectKey
// is derived from the absolute path.
func findProjectDir(homeDir, absWorkDir string) string {
	projectsBase := filepath.Join(homeDir, ".qwen", "projects")

	// Build candidate keys: different ways Qwen Code might encode the path.
	candidates := []string{
		strings.ReplaceAll(absWorkDir, string(filepath.Separator), "-"),
		strings.NewReplacer("/", "-", "\\", "-", ":", "-").Replace(absWorkDir),
		strings.NewReplacer("/", "-", "\\", "-", ":", "-", "_", "-").Replace(absWorkDir),
	}
	fwd := strings.ReplaceAll(absWorkDir, "\\", "/")
	candidates = append(candidates, strings.ReplaceAll(fwd, "/", "-"))

	for _, key := range candidates {
		dir := filepath.Join(projectsBase, key)
		if _, err := os.Stat(dir); err == nil {
			return dir
		}
	}

	// Fallback: scan the projects directory
	entries, err := os.ReadDir(projectsBase)
	if err != nil {
		return ""
	}

	normWork := strings.ToLower(strings.NewReplacer("/", "-", "\\", "-", ":", "-", "_", "-").Replace(absWorkDir))
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		normEntry := strings.ToLower(entry.Name())
		if normEntry == normWork {
			return filepath.Join(projectsBase, entry.Name())
		}
	}

	return ""
}
