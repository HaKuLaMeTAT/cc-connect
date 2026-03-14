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

	"github.com/chenhg5/cc-connect/core"
)

func init() {
	core.RegisterAgent("gemini", New)
}

// Agent drives the Gemini CLI in headless mode using -p --output-format stream-json.
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
		{Name: "gemini-3.1-pro-preview", Desc: "Gemini 3.1 Pro Preview"},
		{Name: "gemini-3-flash-preview", Desc: "Gemini 3 Flash Preview"},
		{Name: "gemini-2.5-pro", Desc: "Gemini 2.5 Pro"},
		{Name: "gemini-2.5-flash", Desc: "Gemini 2.5 Flash"},
		{Name: "gemini-2.5-flash-lite", Desc: "Gemini 2.5 Flash Lite"},
		{Name: "gemini-2.0-flash-thinking-exp", Desc: "Gemini 2.0 Thinking (Experimental)"},
		{Name: "gemini-2.0-flash-lite-preview-02-05", Desc: "Gemini 2.0 Flash Lite"},
		{Name: "gemini-2.0-flash", Desc: "Gemini 2.0 Flash"},
		{Name: "gemini-2.0-pro-exp-02-05", Desc: "Gemini 2.0 Pro Exp"},
		{Name: "gemini-1.5-pro", Desc: "Gemini 1.5 Pro"},
		{Name: "gemini-1.5-flash", Desc: "Gemini 1.5 Flash"},
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
	return models
}

func mergeGeminiModelOptions(groups ...[]core.ModelOption) []core.ModelOption {
	seen := make(map[string]struct{})
	var merged []core.ModelOption
	for _, group := range groups {
		for _, model := range group {
			name := strings.TrimSpace(model.Name)
			if name == "" {
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
	sessionID = strings.TrimSpace(sessionID)
	a.mu.Lock()
	gs, ok := a.sessions[sessionID]
	if !ok || gs == nil {
		// Fallback: search by actual gemini session ID
		for _, sess := range a.sessions {
			if sess != nil && sess.CurrentSessionID() == sessionID {
				gs = sess
				ok = true
				break
			}
		}
	}
	a.mu.Unlock()

	if ok && gs != nil {
		if u := gs.usage.Load(); u != nil {
			slog.Info("gemini: usage from active session", "session_id", sessionID)
			return u, nil
		}
	}

	// Try disk fallback
	u, err := getGeminiContextUsage(a.workDir, sessionID)
	if err == nil {
		slog.Info("gemini: usage from disk", "session_id", sessionID, "work_dir", a.workDir)
		return u, nil
	}

	slog.Warn("gemini: usage unavailable", "session_id", sessionID, "work_dir", a.workDir, "error", err)
	return nil, fmt.Errorf("session not active: %s", sessionID)
}

func getGeminiContextUsage(workDir, sessionID string) (*core.ContextUsage, error) {
	path, err := findGeminiSessionFile(workDir, sessionID)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	var sf sessionFile
	if err := json.Unmarshal(data, &sf); err != nil {
		return nil, err
	}

	usage := &core.ContextUsage{}
	for _, msg := range sf.Messages {
		if geminiHistoryRole(msg.Type) != "assistant" || msg.Tokens == nil {
			continue
		}
		usage.PromptTokens = msg.Tokens.Input
		usage.CompletionTokens = msg.Tokens.Output
		usage.TotalTokens = msg.Tokens.Total
	}
	// Fallback to top-level stats if per-message tokens are missing
	if usage.TotalTokens == 0 {
		usage.PromptTokens = sf.Stats.PromptTokens
		usage.CompletionTokens = sf.Stats.CompletionTokens
		usage.TotalTokens = sf.Stats.TotalTokens
	}
	return usage, nil
}

func findGeminiSessionFile(workDir, sessionID string) (string, error) {
	sessionID = strings.TrimSpace(sessionID)
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}

	// 1. Direct project-based path (fastest)
	projName := geminiProjectHash(workDir)
	directPath := filepath.Join(homeDir, ".gemini", "tmp", projName, "chats", sessionID+".json")
	if _, err := os.Stat(directPath); err == nil {
		return directPath, nil
	}

	// 2. Fuzzy scan within current project
	chatsDir := filepath.Join(homeDir, ".gemini", "tmp", projName, "chats")
	if path, found := scanGeminiDir(chatsDir, sessionID); found {
		return path, nil
	}

	// 3. Global scan
	tmpDir := filepath.Join(homeDir, ".gemini", "tmp")
	roots, _ := os.ReadDir(tmpDir)
	for _, root := range roots {
		if !root.IsDir() || root.Name() == projName {
			continue
		}
		if path, found := scanGeminiDir(filepath.Join(tmpDir, root.Name(), "chats"), sessionID); found {
			return path, nil
		}
	}

	return "", os.ErrNotExist
}

func scanGeminiDir(dir, sessionID string) (string, bool) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", false
	}
	shortID := sessionID
	if len(shortID) > 8 {
		shortID = shortID[:8]
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		if !strings.Contains(entry.Name(), shortID) {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		var meta struct {
			SessionID string `json:"sessionId"`
		}
		if json.Unmarshal(data, &meta) == nil && strings.TrimSpace(meta.SessionID) == sessionID {
			slog.Info("gemini: found session file", "session_id", sessionID, "path", path)
			return path, true
		}
	}
	return "", false
}

func (a *Agent) ListSessions(_ context.Context) ([]core.AgentSessionInfo, error) {
	return listGeminiSessions(a.workDir)
}

func (a *Agent) GetSessionHistory(_ context.Context, sessionID string, limit int) ([]core.HistoryEntry, error) {
	path, err := findGeminiSessionFile(a.workDir, sessionID)
	if err != nil {
		return nil, nil
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	var sf sessionFile
	if err := json.Unmarshal(data, &sf); err != nil {
		return nil, err
	}

	var history []core.HistoryEntry
	for _, msg := range sf.Messages {
		role := geminiHistoryRole(msg.Type)
		if role == "" {
			continue
		}
		content := extractGeminiMessageText(msg.Content)
		if strings.TrimSpace(content) == "" {
			continue
		}
		history = append(history, core.HistoryEntry{
			Role:    role,
			Content: content,
		})
	}

	if limit > 0 && len(history) > limit {
		history = history[len(history)-limit:]
	}
	return history, nil
}

func (a *Agent) DeleteSession(_ context.Context, sessionID string) error {
	path, err := findGeminiSessionFile(a.workDir, sessionID)
	if err != nil {
		return err
	}
	return os.Remove(path)
}

func (a *Agent) Stop() error { return nil }

func (a *Agent) SetMode(mode string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.mode = normalizeMode(mode)
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

func (a *Agent) CommandDirs() []string {
	absDir, _ := filepath.Abs(a.workDir)
	dirs := []string{filepath.Join(absDir, ".gemini", "commands")}
	if home, err := os.UserHomeDir(); err == nil {
		dirs = append(dirs, filepath.Join(home, ".gemini", "commands"))
	}
	return dirs
}

func (a *Agent) SkillDirs() []string {
	absDir, _ := filepath.Abs(a.workDir)
	dirs := []string{filepath.Join(absDir, ".gemini", "skills")}
	if home, err := os.UserHomeDir(); err == nil {
		dirs = append(dirs, filepath.Join(home, ".gemini", "skills"))
	}
	return dirs
}

func (a *Agent) CompressCommand() string { return "/compress" }

func (a *Agent) ProjectMemoryFile() string {
	absDir, _ := filepath.Abs(a.workDir)
	return filepath.Join(absDir, "GEMINI.md")
}

func (a *Agent) GlobalMemoryFile() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".gemini", "GEMINI.md")
}

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
		return true
	}
	for i, p := range a.providers {
		if p.Name == name {
			a.activeIdx = i
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

func geminiProjectHash(workDir string) string {
	abs, _ := filepath.Abs(workDir)
	homeDir, _ := os.UserHomeDir()
	entries, _ := os.ReadDir(filepath.Join(homeDir, ".gemini", "tmp"))
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		rootPath := filepath.Join(homeDir, ".gemini", "tmp", entry.Name(), ".project_root")
		data, _ := os.ReadFile(rootPath)
		if strings.TrimSpace(string(data)) == abs {
			return entry.Name()
		}
	}
	return filepath.Base(abs)
}

type sessionFile struct {
	SessionID   string    `json:"sessionId"`
	LastUpdated time.Time `json:"lastUpdated"`
	StartTime   time.Time `json:"startTime"`
	Messages    []struct {
		Type   string `json:"type"`
		Model  string `json:"model"`
		Tokens *struct {
			Input  int `json:"input"`
			Output int `json:"output"`
			Total  int `json:"total"`
		} `json:"tokens"`
		Content any `json:"content"`
	} `json:"messages"`
	Stats struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
		TotalTokens      int `json:"total_tokens"`
	} `json:"stats"`
}

func listGeminiSessions(workDir string) ([]core.AgentSessionInfo, error) {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("gemini: cannot determine home dir: %w", err)
	}

	tmpDir := filepath.Join(homeDir, ".gemini", "tmp")
	roots, err := os.ReadDir(tmpDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("gemini: read tmp dir: %w", err)
	}

	currentProj := geminiProjectHash(workDir)
	var sessions []core.AgentSessionInfo

	for _, root := range roots {
		if !root.IsDir() {
			continue
		}

		projName := root.Name()
		chatsDir := filepath.Join(tmpDir, projName, "chats")
		entries, err := os.ReadDir(chatsDir)
		if err != nil {
			continue
		}

		for _, entry := range entries {
			if !strings.HasSuffix(entry.Name(), ".json") {
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
			if projName != currentProj {
				summary = fmt.Sprintf("[%s] %s", projName, summary)
			}

			modTime := sf.LastUpdated
			if modTime.IsZero() {
				modTime = sf.StartTime
			}

			sessions = append(sessions, core.AgentSessionInfo{
				ID:           sf.SessionID,
				Summary:      summary,
				MessageCount: len(sf.Messages),
				ModifiedAt:   modTime,
			})
		}
	}

	if len(sessions) == 0 {
		// Final fallback: glob every Gemini chat file regardless of tmp root layout.
		matches, _ := filepath.Glob(filepath.Join(tmpDir, "*", "chats", "*.json"))
		for _, path := range matches {
			data, err := os.ReadFile(path)
			if err != nil {
				continue
			}
			var sf sessionFile
			if json.Unmarshal(data, &sf) != nil || sf.SessionID == "" {
				continue
			}

			projName := filepath.Base(filepath.Dir(filepath.Dir(path)))
			summary := extractSessionSummary(&sf)
			if projName != currentProj {
				summary = fmt.Sprintf("[%s] %s", projName, summary)
			}
			modTime := sf.LastUpdated
			if modTime.IsZero() {
				modTime = sf.StartTime
			}
			sessions = append(sessions, core.AgentSessionInfo{
				ID:           sf.SessionID,
				Summary:      summary,
				MessageCount: len(sf.Messages),
				ModifiedAt:   modTime,
			})
		}
	}

	sort.Slice(sessions, func(i, j int) bool {
		return sessions[i].ModifiedAt.After(sessions[j].ModifiedAt)
	})

	slog.Info("gemini: list sessions",
		"work_dir", workDir,
		"project", currentProj,
		"count", len(sessions),
	)

	return sessions, nil
}

func extractSessionSummary(sf *sessionFile) string {
	for _, msg := range sf.Messages {
		if geminiHistoryRole(msg.Type) != "user" {
			continue
		}
		if t := strings.TrimSpace(extractGeminiMessageText(msg.Content)); t != "" {
			return t
		}
	}
	return sf.SessionID
}

func geminiHistoryRole(rawType string) string {
	switch strings.ToLower(strings.TrimSpace(rawType)) {
	case "user":
		return "user"
	case "assistant", "gemini":
		return "assistant"
	default:
		return ""
	}
}

func extractGeminiMessageText(content any) string {
	switch v := content.(type) {
	case string:
		return strings.TrimSpace(v)
	case []any:
		var parts []string
		for _, item := range v {
			m, ok := item.(map[string]any)
			if !ok {
				continue
			}
			if text, _ := m["text"].(string); strings.TrimSpace(text) != "" {
				parts = append(parts, strings.TrimSpace(text))
			}
		}
		return strings.Join(parts, "\n")
	default:
		return ""
	}
}
