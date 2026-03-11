package gemini

import (
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
	core.RegisterAgent("gemini", New)
}

// Agent drives the Gemini CLI in headless mode using -p --output-format stream-json.
//
// Modes (maps to Gemini CLI approval flags):
//   - "default":   standard approval mode (prompt for each tool use)
//   - "auto_edit": auto-approve edit tools, ask for others
//   - "yolo":      auto-approve all tools (-y / --approval-mode yolo)
//   - "plan":      read-only plan mode (--approval-mode plan)
type Agent struct {
	workDir    string
	model      string
	mode       string
	cmd        string // CLI binary name, default "gemini"
	timeout    time.Duration
	providers  []core.ProviderConfig
	activeIdx  int
	sessionEnv []string
	sessions   map[string]*geminiSession
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
		cmd = "gemini"
	}

	timeoutMins, ok := opts["timeout_mins"].(int64)
	if v, ok2 := opts["timeout_mins"]; ok && !ok2 {
		slog.Debug("gemini: timeout_mins should be int64, got %T", v)
	}
	var timeout time.Duration
	if timeoutMins > 0 {
		timeout = time.Duration(timeoutMins) * time.Minute
	}

	if _, err := exec.LookPath(cmd); err != nil {
		return nil, fmt.Errorf("gemini: %q CLI not found in PATH, install with: npm i -g @google/gemini-cli", cmd)
	}

	return &Agent{
		workDir:   workDir,
		model:     model,
		mode:      mode,
		cmd:       cmd,
		timeout:   timeout,
		activeIdx: -1,
		sessions:  make(map[string]*geminiSession),
	}, nil
}

func normalizeMode(raw string) string {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "yolo", "auto", "force", "bypasspermissions":
		return "yolo"
	case "auto_edit", "autoedit", "edit", "acceptedits":
		return "auto_edit"
	case "plan":
		return "plan"
	default:
		return "default"
	}
}

func (a *Agent) Name() string { return "gemini" }

func (a *Agent) HasSystemPromptSupport() bool { return true }

func (a *Agent) SetModel(model string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.model = model
	slog.Info("gemini: model changed", "model", model)
}

func (a *Agent) GetModel() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.model
}

func (a *Agent) AvailableModels(ctx context.Context) []core.ModelOption {
	models := mergeGeminiModelOptions(
		a.fetchModelsFromAPI(ctx),
		readGeminiSeenModels(a.workDir),
		defaultGeminiModels(),
	)
	if len(models) > 0 {
		return models
	}
	return defaultGeminiModels()
}

func defaultGeminiModels() []core.ModelOption {
	return []core.ModelOption{
		{Name: "gemini-2.5-pro", Desc: "Gemini 2.5 Pro (most capable)"},
		{Name: "gemini-2.5-flash", Desc: "Gemini 2.5 Flash (fast)"},
		{Name: "gemini-2.5-flash-lite", Desc: "Gemini 2.5 Flash Lite"},
		{Name: "gemini-2.0-flash", Desc: "Gemini 2.0 Flash (lightweight)"},
		{Name: "gemini-2.0-flash-lite", Desc: "Gemini 2.0 Flash Lite"},
		{Name: "gemini-1.5-pro", Desc: "Gemini 1.5 Pro"},
		{Name: "gemini-1.5-flash", Desc: "Gemini 1.5 Flash"},
		{Name: "gemini-3-flash-preview", Desc: "Gemini 3 Flash Preview"},
		{Name: "gemini-3.1-pro-preview", Desc: "Gemini 3.1 Pro Preview"},
	}
}

func (a *Agent) fetchModelsFromAPI(ctx context.Context) []core.ModelOption {
	apiKey := os.Getenv("GEMINI_API_KEY")
	if apiKey == "" {
		apiKey = os.Getenv("GOOGLE_API_KEY")
	}
	if apiKey == "" {
		return nil
	}

	url := "https://generativelanguage.googleapis.com/v1beta/models?key=" + apiKey
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		slog.Debug("gemini: failed to fetch models", "error", err)
		return nil
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil
	}

	var result struct {
		Models []struct {
			Name        string `json:"name"`
			DisplayName string `json:"displayName"`
			Description string `json:"description"`
		} `json:"models"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil
	}

	var models []core.ModelOption
	for _, m := range result.Models {
		id := strings.TrimPrefix(m.Name, "models/")
		if !strings.HasPrefix(id, "gemini-") {
			continue
		}
		models = append(models, core.ModelOption{Name: id, Desc: m.DisplayName})
	}
	sort.Slice(models, func(i, j int) bool { return models[i].Name > models[j].Name })
	return models
}

func readGeminiSeenModels(workDir string) []core.ModelOption {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return nil
	}

	projName := geminiProjectHash(workDir)
	chatsDir := filepath.Join(homeDir, ".gemini", "tmp", projName, "chats")
	entries, err := os.ReadDir(chatsDir)
	if err != nil {
		return nil
	}

	var models []core.ModelOption
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(chatsDir, entry.Name()))
		if err != nil {
			continue
		}
		var sf sessionFile
		if json.Unmarshal(data, &sf) != nil {
			continue
		}
		for _, msg := range sf.Messages {
			if strings.TrimSpace(msg.Model) == "" {
				continue
			}
			models = append(models, core.ModelOption{Name: msg.Model})
		}
	}

	if settingsModel := readGeminiConfiguredModel(); settingsModel != "" {
		models = append(models, core.ModelOption{Name: settingsModel})
	}
	return models
}

func readGeminiConfiguredModel() string {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	path := filepath.Join(homeDir, ".gemini", "settings.json")
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	var cfg struct {
		Model struct {
			Name string `json:"name"`
		} `json:"model"`
	}
	if json.Unmarshal(data, &cfg) != nil {
		return ""
	}
	return strings.TrimSpace(cfg.Model.Name)
}

func mergeGeminiModelOptions(groups ...[]core.ModelOption) []core.ModelOption {
	seen := make(map[string]struct{})
	var merged []core.ModelOption
	for _, group := range groups {
		for _, model := range group {
			name := strings.TrimSpace(model.Name)
			if name == "" || !strings.HasPrefix(name, "gemini-") {
				continue
			}
			if _, ok := seen[name]; ok {
				continue
			}
			seen[name] = struct{}{}
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
	timeout := a.timeout
	extraEnv := a.providerEnvLocked()
	extraEnv = append(extraEnv, a.sessionEnv...)
	if a.activeIdx >= 0 && a.activeIdx < len(a.providers) {
		if m := a.providers[a.activeIdx].Model; m != "" {
			model = m
		}
	}
	a.mu.Unlock()

	gs, err := newGeminiSession(ctx, cmd, workDir, model, mode, sessionID, extraEnv, timeout)
	if err != nil {
		return nil, err
	}

	a.mu.Lock()
	a.sessions[sessionID] = gs
	a.mu.Unlock()

	return gs, nil
}

func (a *Agent) GetContextUsage(_ context.Context, sessionID string) (*core.ContextUsage, error) {
	a.mu.Lock()
	gs, ok := a.sessions[sessionID]
	if !ok || gs == nil {
		for key, sess := range a.sessions {
			if sess == nil {
				continue
			}
			if sess.CurrentSessionID() == sessionID {
				gs = sess
				ok = true
				if key != sessionID {
					a.sessions[sessionID] = sess
				}
				break
			}
		}
	}
	a.mu.Unlock()

	if ok && gs != nil {
		u := gs.usage.Load()
		if u != nil {
			return u, nil
		}
	}

	u, err := getGeminiContextUsage(a.workDir, sessionID)
	if err == nil {
		return u, nil
	}
	if ok && gs != nil {
		return &core.ContextUsage{}, nil
	}
	return nil, fmt.Errorf("session not active: %s", sessionID)
}

// ListSessions reads sessions from ~/.gemini/tmp/<project_hash>/chats/.
func (a *Agent) ListSessions(_ context.Context) ([]core.AgentSessionInfo, error) {
	return listGeminiSessions(a.workDir)
}

func (a *Agent) GetSessionHistory(_ context.Context, sessionID string, limit int) ([]core.HistoryEntry, error) {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("gemini: cannot determine home dir: %w", err)
	}

	projName := geminiProjectHash(a.workDir)
	path := filepath.Join(homeDir, ".gemini", "tmp", projName, "chats", sessionID+".json")
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("gemini: read session file: %w", err)
	}

	var sf sessionFile
	if err := json.Unmarshal(data, &sf); err != nil {
		return nil, fmt.Errorf("gemini: unmarshal session file: %w", err)
	}

	var history []core.HistoryEntry
	for _, msg := range sf.Messages {
		if msg.Type != "user" && msg.Type != "assistant" {
			continue
		}

		var content strings.Builder
		for _, c := range msg.Content {
			if c.Text != "" {
				if content.Len() > 0 {
					content.WriteString("\n")
				}
				content.WriteString(c.Text)
			}
		}

		role := msg.Type
		if role == "" {
			continue
		}

		history = append(history, core.HistoryEntry{
			Role:    role,
			Content: content.String(),
			// Gemini CLI JSON doesn't store per-message timestamps,
			// using session LastUpdated as a fallback if needed, but HistoryEntry
			// doesn't strictly require it for display.
		})
	}

	if limit > 0 && len(history) > limit {
		history = history[len(history)-limit:]
	}

	return history, nil
}

func (a *Agent) DeleteSession(_ context.Context, sessionID string) error {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("gemini: cannot determine home dir: %w", err)
	}
	projName := geminiProjectHash(a.workDir)
	path := filepath.Join(homeDir, ".gemini", "tmp", projName, "chats", sessionID+".json")
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return fmt.Errorf("session file not found: %s", sessionID)
	}
	return os.Remove(path)
}

func (a *Agent) Stop() error { return nil }

// ── ModeSwitcher ────────────────────────────────────────────────

func (a *Agent) SetMode(mode string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.mode = normalizeMode(mode)
	slog.Info("gemini: mode changed", "mode", a.mode)
}

func (a *Agent) GetMode() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.mode
}

func (a *Agent) PermissionModes() []core.PermissionModeInfo {
	return []core.PermissionModeInfo{
		{Key: "default", Name: "Default", NameZh: "默认", Desc: "Prompt for approval on each tool use", DescZh: "每次工具调用都需要确认"},
		{Key: "auto_edit", Name: "Auto Edit", NameZh: "自动编辑", Desc: "Auto-approve edit tools, ask for others", DescZh: "编辑工具自动通过，其他仍需确认"},
		{Key: "yolo", Name: "YOLO", NameZh: "全自动", Desc: "Auto-approve all tool calls", DescZh: "自动批准所有工具调用"},
		{Key: "plan", Name: "Plan", NameZh: "规划模式", Desc: "Read-only plan mode, no execution", DescZh: "只读规划模式，不做修改"},
	}
}

// ── CommandProvider implementation ────────────────────────────

func (a *Agent) CommandDirs() []string {
	absDir, err := filepath.Abs(a.workDir)
	if err != nil {
		absDir = a.workDir
	}
	dirs := []string{filepath.Join(absDir, ".gemini", "commands")}
	if home, err := os.UserHomeDir(); err == nil {
		dirs = append(dirs, filepath.Join(home, ".gemini", "commands"))
	}
	return dirs
}

// ── SkillProvider implementation ──────────────────────────────

func (a *Agent) SkillDirs() []string {
	absDir, err := filepath.Abs(a.workDir)
	if err != nil {
		absDir = a.workDir
	}
	dirs := []string{filepath.Join(absDir, ".gemini", "skills")}
	if home, err := os.UserHomeDir(); err == nil {
		dirs = append(dirs, filepath.Join(home, ".gemini", "skills"))
	}
	return dirs
}

// ── ContextCompressor implementation ──────────────────────────

func (a *Agent) CompressCommand() string { return "/compress" }

// ── MemoryFileProvider implementation ─────────────────────────

func (a *Agent) ProjectMemoryFile() string {
	absDir, err := filepath.Abs(a.workDir)
	if err != nil {
		absDir = a.workDir
	}
	return filepath.Join(absDir, "GEMINI.md")
}

func (a *Agent) GlobalMemoryFile() string {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(homeDir, ".gemini", "GEMINI.md")
}

// ── ProviderSwitcher ────────────────────────────────────────────

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
		slog.Info("gemini: provider cleared")
		return true
	}
	for i, p := range a.providers {
		if p.Name == name {
			a.activeIdx = i
			slog.Info("gemini: provider switched", "provider", name)
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
		env = append(env, "GEMINI_API_KEY="+p.APIKey)
	}
	for k, v := range p.Env {
		env = append(env, k+"="+v)
	}
	return env
}

// ── Session listing ─────────────────────────────────────────────

// geminiProjectHash computes the directory name Gemini CLI uses under ~/.gemini/tmp/.
// Gemini uses a hash of the absolute project path.
func geminiProjectHash(workDir string) string {
	abs, err := filepath.Abs(workDir)
	if err != nil {
		abs = workDir
	}
	return filepath.Base(abs)
}

// sessionFile represents the JSON structure of a Gemini CLI session file.
type sessionFile struct {
	SessionID   string    `json:"sessionId"`
	ProjectHash string    `json:"projectHash"`
	StartTime   time.Time `json:"startTime"`
	LastUpdated time.Time `json:"lastUpdated"`
	Messages    []struct {
		Type   string `json:"type"`
		Model  string `json:"model"`
		Tokens *struct {
			Input  int `json:"input"`
			Output int `json:"output"`
			Total  int `json:"total"`
		} `json:"tokens"`
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
	} `json:"messages"`
	Kind string `json:"kind"`
}

func getGeminiContextUsage(workDir, sessionID string) (*core.ContextUsage, error) {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("gemini: cannot determine home dir: %w", err)
	}

	projName := geminiProjectHash(workDir)
	path := filepath.Join(homeDir, ".gemini", "tmp", projName, "chats", sessionID+".json")
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("gemini: read session file: %w", err)
	}

	var sf sessionFile
	if err := json.Unmarshal(data, &sf); err != nil {
		return nil, fmt.Errorf("gemini: unmarshal session file: %w", err)
	}

	usage := &core.ContextUsage{}
	for _, msg := range sf.Messages {
		if msg.Type != "assistant" || msg.Tokens == nil {
			continue
		}
		usage.PromptTokens = msg.Tokens.Input
		usage.CompletionTokens = msg.Tokens.Output
		usage.TotalTokens = msg.Tokens.Total
	}
	return usage, nil
}

func listGeminiSessions(workDir string) ([]core.AgentSessionInfo, error) {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("gemini: cannot determine home dir: %w", err)
	}

	projName := geminiProjectHash(workDir)
	chatsDir := filepath.Join(homeDir, ".gemini", "tmp", projName, "chats")

	entries, err := os.ReadDir(chatsDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("gemini: read chats dir: %w", err)
	}

	var sessions []core.AgentSessionInfo
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}

		data, err := os.ReadFile(filepath.Join(chatsDir, entry.Name()))
		if err != nil {
			continue
		}

		var sf sessionFile
		if json.Unmarshal(data, &sf) != nil || sf.SessionID == "" {
			continue
		}

		summary := extractSessionSummary(&sf)
		if utf8.RuneCountInString(summary) > 60 {
			summary = string([]rune(summary)[:60]) + "..."
		}

		msgCount := len(sf.Messages)
		modTime := sf.LastUpdated
		if modTime.IsZero() {
			modTime = sf.StartTime
		}

		sessions = append(sessions, core.AgentSessionInfo{
			ID:           sf.SessionID,
			Summary:      summary,
			MessageCount: msgCount,
			ModifiedAt:   modTime,
		})
	}

	sort.Slice(sessions, func(i, j int) bool {
		return sessions[i].ModifiedAt.After(sessions[j].ModifiedAt)
	})

	return sessions, nil
}

// extractSessionSummary picks the first meaningful user text as the session summary.
func extractSessionSummary(sf *sessionFile) string {
	for _, msg := range sf.Messages {
		if msg.Type != "user" {
			continue
		}
		for _, c := range msg.Content {
			text := strings.TrimSpace(c.Text)
			if text == "" {
				continue
			}
			for _, line := range strings.Split(text, "\n") {
				line = strings.TrimSpace(line)
				if line == "" {
					continue
				}
				if strings.HasPrefix(line, "{") && strings.HasSuffix(line, "}") {
					continue
				}
				return line
			}
		}
	}
	if len(sf.SessionID) > 12 {
		return sf.SessionID[:12] + "..."
	}
	return sf.SessionID
}
