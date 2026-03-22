package gemini

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/chenhg5/cc-connect/core"
)

// geminiSession manages multi-turn conversations with the Gemini CLI.
// Each Send() launches a new `gemini -p ... --output-format stream-json` process
// with --resume for conversation continuity.
type geminiSession struct {
	cmd      string
	workDir  string
	model    string
	mode     string
	timeout  time.Duration
	extraEnv []string
	events   chan core.Event
	chatID   atomic.Value // stores string — Gemini session ID
	ctx      context.Context
	cancel   context.CancelFunc
	wg       sync.WaitGroup
	alive    atomic.Bool

	usage atomic.Pointer[core.ContextUsage]

	turnMu     sync.Mutex
	turnActive bool

	turnSeq   atomic.Uint64
	timerMu   sync.Mutex
	turnTimer *time.Timer

	tempFilesMu    sync.Mutex
	pendingBatches [][]string
}

func newGeminiSession(ctx context.Context, cmd, workDir, model, mode, resumeID string, extraEnv []string, timeout time.Duration) (*geminiSession, error) {
	sessionCtx, cancel := context.WithCancel(ctx)

	gs := &geminiSession{
		cmd:      cmd,
		workDir:  workDir,
		model:    model,
		mode:     mode,
		timeout:  timeout,
		extraEnv: extraEnv,
		events:   make(chan core.Event, 64),
		ctx:      sessionCtx,
		cancel:   cancel,
	}
	gs.alive.Store(true)

	if resumeID != "" && resumeID != core.ContinueSession {
		gs.chatID.Store(resumeID)
	}

	return gs, nil
}

func (gs *geminiSession) buildArgs() []string {
	args := []string{
		"--output-format", "stream-json",
	}

	switch gs.mode {
	case "yolo":
		args = append(args, "-y")
	case "auto_edit":
		args = append(args, "--approval-mode", "auto_edit")
	case "plan":
		args = append(args, "--approval-mode", "plan")
	}

	if chatID := gs.CurrentSessionID(); chatID != "" {
		args = append(args, "--resume", chatID)
	}
	if gs.model != "" {
		args = append(args, "-m", gs.model)
	}

	return args
}

func (gs *geminiSession) buildEnv() []string {
	env := os.Environ()
	if filepath.IsAbs(gs.cmd) {
		cmdDir := filepath.Dir(gs.cmd)
		if cmdDir != "" {
			curPath := ""
			for _, item := range env {
				if strings.HasPrefix(item, "PATH=") {
					curPath = strings.TrimPrefix(item, "PATH=")
					break
				}
			}
			pathValue := cmdDir
			if curPath != "" {
				pathValue += string(filepath.ListSeparator) + curPath
			}
			env = core.MergeEnv(env, []string{"PATH=" + pathValue})
		}
	}
	if len(gs.extraEnv) > 0 {
		env = core.MergeEnv(env, gs.extraEnv)
	}
	return env
}

func (gs *geminiSession) Send(prompt string, images []core.ImageAttachment) error {
	if !gs.alive.Load() {
		return fmt.Errorf("session is closed")
	}
	for {
		gs.turnMu.Lock()
		if !gs.turnActive {
			gs.turnActive = true
			gs.turnMu.Unlock()
			break
		}
		gs.turnMu.Unlock()

		select {
		case <-gs.ctx.Done():
			return fmt.Errorf("session is closed")
		case <-time.After(10 * time.Millisecond):
		}
	}

	releaseTurn := func() {
		gs.turnMu.Lock()
		gs.turnActive = false
		gs.turnMu.Unlock()
	}

	// Gemini CLI supports @file references for images; save to temp files
	var imageRefs []string
	if len(images) > 0 {
		tmpDir := os.TempDir()
		for i, img := range images {
			ext := ".png"
			switch img.MimeType {
			case "image/jpeg":
				ext = ".jpg"
			case "image/gif":
				ext = ".gif"
			case "image/webp":
				ext = ".webp"
			}
			fname := fmt.Sprintf("cc-connect-img-%d%s", i, ext)
			fpath := filepath.Join(tmpDir, fname)
			if err := os.WriteFile(fpath, img.Data, 0o644); err != nil {
				slog.Warn("geminiSession: failed to save image", "error", err)
				continue
			}
			imageRefs = append(imageRefs, fpath)
		}
	}

	// Build the prompt with image file references
	fullPrompt := prompt
	if len(imageRefs) > 0 {
		fullPrompt = strings.Join(imageRefs, " ") + " " + prompt
	}

	gs.enqueueTempFiles(imageRefs)
	gs.startTurnTimer()

	args := gs.buildArgs()
	slog.Debug("geminiSession: launching turn process", "args", core.RedactArgs(args))

	cmd := exec.CommandContext(gs.ctx, gs.cmd, args...)
	cmd.Dir = gs.workDir
	cmd.Env = gs.buildEnv()

	stdin, err := cmd.StdinPipe()
	if err != nil {
		gs.finishTurn(false)
		releaseTurn()
		return fmt.Errorf("geminiSession: stdin pipe: %w", err)
	}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		gs.finishTurn(false)
		releaseTurn()
		_ = stdin.Close()
		return fmt.Errorf("geminiSession: stdout pipe: %w", err)
	}

	var stderrBuf bytes.Buffer
	cmd.Stderr = &stderrBuf

	if err := cmd.Start(); err != nil {
		gs.finishTurn(false)
		releaseTurn()
		_ = stdin.Close()
		return fmt.Errorf("geminiSession: start: %w", err)
	}

	if _, err := io.WriteString(stdin, fullPrompt+"\n"); err != nil {
		gs.finishTurn(false)
		releaseTurn()
		_ = stdin.Close()
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return fmt.Errorf("geminiSession: write to stdin: %w", err)
	}
	if err := stdin.Close(); err != nil {
		gs.finishTurn(false)
		releaseTurn()
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return fmt.Errorf("geminiSession: close stdin: %w", err)
	}

	gs.wg.Add(1)
	go func() {
		defer gs.wg.Done()
		defer releaseTurn()

		sawTerminal := gs.readLoop(stdout)
		err := cmd.Wait()
		if !sawTerminal {
			gs.finishTurn(true)
		}
		if gs.ctx.Err() != nil {
			return
		}
		if err != nil {
			stderrMsg := strings.TrimSpace(stderrBuf.String())
			if stderrMsg != "" {
				slog.Error("geminiSession: process failed", "error", err, "stderr", stderrMsg)
				select {
				case gs.events <- core.Event{Type: core.EventError, Error: fmt.Errorf("%s", stderrMsg)}:
				case <-gs.ctx.Done():
				}
				return
			}
			if !sawTerminal {
				select {
				case gs.events <- core.Event{Type: core.EventError, Error: fmt.Errorf("geminiSession: process exited: %w", err)}:
				case <-gs.ctx.Done():
				}
			}
			return
		}
		if !sawTerminal {
			select {
			case gs.events <- core.Event{Type: core.EventError, Error: fmt.Errorf("geminiSession: process exited without response")}:
			case <-gs.ctx.Done():
			}
		}
	}()

	return nil
}

func (gs *geminiSession) readLoop(stdout io.ReadCloser) bool {
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024)
	sawTerminal := false

	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			continue
		}

		slog.Debug("geminiSession: raw", "line", truncate(line, 500))

		var raw map[string]any
		if err := json.Unmarshal([]byte(line), &raw); err != nil {
			slog.Debug("geminiSession: non-JSON line", "line", line)
			continue
		}

		switch raw["type"] {
		case "error", "result":
			sawTerminal = true
		}
		gs.handleEvent(raw)
	}

	if err := scanner.Err(); err != nil && gs.ctx.Err() == nil {
		slog.Error("geminiSession: scanner error", "error", err)
		evt := core.Event{Type: core.EventError, Error: fmt.Errorf("read stdout: %w", err)}
		select {
		case gs.events <- evt:
		case <-gs.ctx.Done():
		}
		sawTerminal = true
	}
	return sawTerminal
}

// Gemini CLI stream-json event types:
//
//	init       — session_id, model
//	message    — role (user/assistant), content, delta
//	tool_use   — tool_name, tool_id, parameters
//	tool_result — tool_id, status, output, error
//	error      — severity, message
//	result     — status, stats (final event)
func (gs *geminiSession) handleEvent(raw map[string]any) {
	eventType, _ := raw["type"].(string)

	switch eventType {
	case "init":
		gs.handleInit(raw)
	case "message":
		gs.handleMessage(raw)
	case "tool_use":
		gs.handleToolUse(raw)
	case "tool_result":
		gs.handleToolResult(raw)
	case "permission_request":
		gs.handlePermissionRequest(raw)
	case "error":
		gs.handleError(raw)
	case "result":
		gs.handleResult(raw)
	default:
		slog.Debug("geminiSession: unhandled event", "type", eventType)
	}
}

func (gs *geminiSession) handlePermissionRequest(raw map[string]any) {
	toolName, _ := raw["tool_name"].(string)
	requestID, _ := raw["request_id"].(string)
	params, _ := raw["parameters"].(map[string]any)

	input := formatToolParams(toolName, params)

	slog.Debug("geminiSession: permission_request", "tool", toolName, "id", requestID)
	evt := core.Event{
		Type:         core.EventPermissionRequest,
		ToolName:     toolName,
		ToolInput:    input,
		ToolInputRaw: params,
		RequestID:    requestID,
	}
	select {
	case gs.events <- evt:
	case <-gs.ctx.Done():
		return
	}
}

func (gs *geminiSession) handleInit(raw map[string]any) {
	sid, _ := raw["session_id"].(string)
	model, _ := raw["model"].(string)

	if sid != "" {
		gs.chatID.Store(sid)
		slog.Debug("geminiSession: session init", "session_id", sid, "model", model)

		evt := core.Event{Type: core.EventText, SessionID: sid, Content: "", ToolName: model}
		select {
		case gs.events <- evt:
		case <-gs.ctx.Done():
			return
		}
	}
}

func (gs *geminiSession) handleMessage(raw map[string]any) {
	role, _ := raw["role"].(string)
	content, _ := raw["content"].(string)

	if role == "user" || content == "" {
		return
	}

	evt := core.Event{Type: core.EventText, Content: content}
	select {
	case gs.events <- evt:
	case <-gs.ctx.Done():
	}
}

func (gs *geminiSession) handleToolUse(raw map[string]any) {
	toolName, _ := raw["tool_name"].(string)
	toolID, _ := raw["tool_id"].(string)
	params, _ := raw["parameters"].(map[string]any)

	input := formatToolParams(toolName, params)

	slog.Debug("geminiSession: tool_use", "tool", toolName, "id", toolID)
	evt := core.Event{Type: core.EventToolUse, ToolName: toolName, ToolInput: input}
	select {
	case gs.events <- evt:
	case <-gs.ctx.Done():
		return
	}
}

func (gs *geminiSession) handleToolResult(raw map[string]any) {
	toolID, _ := raw["tool_id"].(string)
	status, _ := raw["status"].(string)
	output, _ := raw["output"].(string)

	slog.Debug("geminiSession: tool_result", "tool_id", toolID, "status", status)

	if status == "error" {
		errObj, _ := raw["error"].(map[string]any)
		if errObj != nil {
			errMsg, _ := errObj["message"].(string)
			if errMsg != "" {
				output = "Error: " + errMsg
			}
		}
	}

	if output != "" {
		evt := core.Event{Type: core.EventToolResult, ToolName: toolID, Content: truncate(output, 500)}
		select {
		case gs.events <- evt:
		case <-gs.ctx.Done():
			return
		}
	}
}

func (gs *geminiSession) handleError(raw map[string]any) {
	severity, _ := raw["severity"].(string)
	message, _ := raw["message"].(string)

	if message != "" {
		gs.finishTurn(true)
		slog.Warn("geminiSession: error event", "severity", severity, "message", message)
		evt := core.Event{Type: core.EventError, Error: fmt.Errorf("[%s] %s", severity, message)}
		select {
		case gs.events <- evt:
		case <-gs.ctx.Done():
			return
		}
	}
}

func (gs *geminiSession) handleResult(raw map[string]any) {
	gs.finishTurn(true)
	status, _ := raw["status"].(string)

	var errMsg string
	if status == "error" {
		errObj, _ := raw["error"].(map[string]any)
		if errObj != nil {
			errMsg, _ = errObj["message"].(string)
		}
	}

	// Capture usage stats if available
	if stats, ok := raw["stats"].(map[string]any); ok {
		u := &core.ContextUsage{}
		if pt, ok := stats["prompt_tokens"].(float64); ok {
			u.PromptTokens = int(pt)
		}
		if ct, ok := stats["completion_tokens"].(float64); ok {
			u.CompletionTokens = int(ct)
		}
		if tt, ok := stats["total_tokens"].(float64); ok {
			u.TotalTokens = int(tt)
		}
		gs.usage.Store(u)
	}

	sid := gs.CurrentSessionID()

	if errMsg != "" {
		evt := core.Event{Type: core.EventResult, Content: errMsg, SessionID: sid, Done: true, Error: fmt.Errorf("%s", errMsg)}
		select {
		case gs.events <- evt:
		case <-gs.ctx.Done():
			return
		}
	} else {
		evt := core.Event{Type: core.EventResult, SessionID: sid, Done: true}
		select {
		case gs.events <- evt:
		case <-gs.ctx.Done():
			return
		}
	}
}

func (gs *geminiSession) RespondPermission(requestID string, result core.PermissionResult) error {
	return fmt.Errorf("geminiSession: permission responses are not supported in non-interactive mode")
}

func (gs *geminiSession) Events() <-chan core.Event {
	return gs.events
}

func (gs *geminiSession) CurrentSessionID() string {
	v, _ := gs.chatID.Load().(string)
	return v
}

func (gs *geminiSession) Alive() bool {
	return gs.alive.Load()
}

func (gs *geminiSession) Close() error {
	gs.alive.Store(false)
	gs.stopTurnTimer()
	gs.cleanupAllTempFiles()
	gs.cancel()
	done := make(chan struct{})
	go func() {
		gs.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(8 * time.Second):
		slog.Warn("geminiSession: close timed out, abandoning wg.Wait")
	}
	close(gs.events)
	return nil
}

func (gs *geminiSession) startTurnTimer() {
	if gs.timeout <= 0 {
		return
	}
	seq := gs.turnSeq.Add(1)
	timer := time.AfterFunc(gs.timeout, func() {
		if gs.turnSeq.Load() != seq {
			return
		}
		gs.finishTurn(true)
		select {
		case gs.events <- core.Event{Type: core.EventError, Error: fmt.Errorf("geminiSession: turn timed out after %s", gs.timeout)}:
		case <-gs.ctx.Done():
		}
		gs.cancel()
	})

	gs.timerMu.Lock()
	if gs.turnTimer != nil {
		gs.turnTimer.Stop()
	}
	gs.turnTimer = timer
	gs.timerMu.Unlock()
}

func (gs *geminiSession) stopTurnTimer() {
	gs.turnSeq.Add(1)
	gs.timerMu.Lock()
	if gs.turnTimer != nil {
		gs.turnTimer.Stop()
		gs.turnTimer = nil
	}
	gs.timerMu.Unlock()
}

func (gs *geminiSession) finishTurn(cleanupFiles bool) {
	gs.stopTurnTimer()
	if cleanupFiles {
		gs.cleanupCompletedTempFiles()
	}
}

func (gs *geminiSession) enqueueTempFiles(paths []string) {
	if len(paths) == 0 {
		return
	}
	gs.tempFilesMu.Lock()
	gs.pendingBatches = append(gs.pendingBatches, paths)
	gs.tempFilesMu.Unlock()
}

func (gs *geminiSession) cleanupCompletedTempFiles() {
	gs.tempFilesMu.Lock()
	if len(gs.pendingBatches) == 0 {
		gs.tempFilesMu.Unlock()
		return
	}
	paths := gs.pendingBatches[0]
	gs.pendingBatches = gs.pendingBatches[1:]
	gs.tempFilesMu.Unlock()

	for _, path := range paths {
		_ = os.Remove(path)
	}
}

func (gs *geminiSession) cleanupAllTempFiles() {
	gs.tempFilesMu.Lock()
	batches := gs.pendingBatches
	gs.pendingBatches = nil
	gs.tempFilesMu.Unlock()

	for _, paths := range batches {
		for _, path := range paths {
			_ = os.Remove(path)
		}
	}
}

// formatToolParams extracts a human-readable summary from tool parameters.
func formatToolParams(toolName string, params map[string]any) string {
	if params == nil {
		return ""
	}

	switch toolName {
	case "shell", "run_shell_command":
		if cmd, ok := params["command"].(string); ok {
			return cmd
		}
	case "write_file", "read_file", "replace":
		if p, ok := params["file_path"].(string); ok {
			return p
		}
		if p, ok := params["path"].(string); ok {
			return p
		}
	case "web_fetch":
		if u, ok := params["url"].(string); ok {
			return u
		}
	case "google_web_search":
		if q, ok := params["query"].(string); ok {
			return q
		}
	}

	b, _ := json.Marshal(params)
	return string(b)
}

func truncate(s string, maxRunes int) string {
	if utf8.RuneCountInString(s) <= maxRunes {
		return s
	}
	return string([]rune(s)[:maxRunes]) + "..."
}
