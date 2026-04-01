package qwen

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
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/chenhg5/cc-connect/core"
)

// qwenSession manages a multi-turn Qwen Code conversation.
// Each Send() spawns `qwen --output-format stream-json -p <prompt>`.
// Subsequent turns use --continue or --resume to resume the conversation.
type qwenSession struct {
	workDir   string
	model     string
	mode      string
	tools     []string
	extraEnv  []string
	events    chan core.Event
	sessionID atomic.Value // stores string
	ctx       context.Context
	cancel    context.CancelFunc
	wg        sync.WaitGroup
	alive     atomic.Bool
}

func newQwenSession(ctx context.Context, workDir, model, resumeID, mode string, tools, extraEnv []string) (*qwenSession, error) {
	sessionCtx, cancel := context.WithCancel(ctx)

	qs := &qwenSession{
		workDir:  workDir,
		model:    model,
		mode:     mode,
		tools:    tools,
		extraEnv: extraEnv,
		events:   make(chan core.Event, 64),
		ctx:      sessionCtx,
		cancel:   cancel,
	}
	qs.alive.Store(true)

	if resumeID != "" {
		qs.sessionID.Store(resumeID)
	}

	return qs, nil
}

func (qs *qwenSession) Send(prompt string, images []core.ImageAttachment) error {
	if len(images) > 0 {
		slog.Warn("qwenSession: images not supported, ignoring")
	}
	if !qs.alive.Load() {
		return fmt.Errorf("session is closed")
	}

	args := []string{
		"--output-format", "stream-json",
		"--input-format", "stream-json",
		"-p", prompt,
	}

	sid := qs.CurrentSessionID()
	if sid != "" {
		args = append(args, "--resume", sid)
	}

	// Permission mode
	switch qs.mode {
	case "yolo":
		args = append(args, "--yolo")
	case "auto-edit":
		args = append(args, "--approval-mode", "auto-edit")
	}

	// Model
	if qs.model != "" {
		args = append(args, "--model", qs.model)
	}

	slog.Debug("qwenSession: launching", "resume", sid != "", "args_len", len(args))

	cmd := exec.CommandContext(qs.ctx, "qwen", args...)
	cmd.Dir = qs.workDir
	if len(qs.extraEnv) > 0 {
		cmd.Env = core.MergeEnv(os.Environ(), qs.extraEnv)
	}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("qwenSession: stdout pipe: %w", err)
	}

	var stderrBuf bytes.Buffer
	cmd.Stderr = &stderrBuf

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("qwenSession: start: %w", err)
	}

	qs.wg.Add(1)
	go qs.readLoop(cmd, stdout, &stderrBuf)

	return nil
}

func (qs *qwenSession) readLoop(cmd *exec.Cmd, stdout io.ReadCloser, stderrBuf *bytes.Buffer) {
	defer qs.wg.Done()
	defer func() {
		if err := cmd.Wait(); err != nil {
			stderrMsg := strings.TrimSpace(stderrBuf.String())
			if stderrMsg != "" {
				slog.Error("qwenSession: process failed", "error", err, "stderr", truncStr(stderrMsg, 200))
				evt := core.Event{Type: core.EventError, Error: fmt.Errorf("%s", stderrMsg)}
				select {
				case qs.events <- evt:
				case <-qs.ctx.Done():
					return
				}
			}
		}
	}()

	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024)

	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			continue
		}

		var raw streamEvent
		if err := json.Unmarshal([]byte(line), &raw); err != nil {
			slog.Debug("qwenSession: non-JSON line", "line", truncStr(line, 100))
			continue
		}

		qs.handleEvent(&raw)
	}

	if err := scanner.Err(); err != nil {
		slog.Error("qwenSession: scanner error", "error", err)
		evt := core.Event{Type: core.EventError, Error: fmt.Errorf("read stdout: %w", err)}
		select {
		case qs.events <- evt:
		case <-qs.ctx.Done():
			return
		}
	}
}

// ── stream-json event structures ─────────────────────────────

type streamEvent struct {
	Type      string         `json:"type"`
	Subtype   string         `json:"subtype"`
	SessionID string         `json:"session_id"`
	Done      bool           `json:"done"`
	Message   *streamMessage `json:"message"`
	ToolCall  *toolCall      `json:"tool_call,omitempty"`
}

type streamMessage struct {
	ID      string          `json:"id"`
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

type toolCall struct {
	ID       string          `json:"id"`
	Name     string          `json:"name"`
	Input    json.RawMessage `json:"input"`
	Status   string          `json:"status,omitempty"`
	Result   json.RawMessage `json:"result,omitempty"`
}

type contentItem struct {
	Type     string `json:"type"`
	Text     string `json:"text"`
	Thinking string `json:"thinking,omitempty"`
}

// ── event handling ───────────────────────────────────────────

func (qs *qwenSession) handleEvent(ev *streamEvent) {
	if ev.SessionID != "" {
		qs.sessionID.Store(ev.SessionID)
	}

	switch ev.Type {
	case "system":
		slog.Debug("qwenSession: init", "session_id", ev.SessionID)

	case "assistant":
		qs.handleAssistant(ev)

	case "tool_call":
		qs.handleToolCall(ev)

	case "tool_result":
		qs.handleToolResult(ev)

	case "result":
		qs.handleResult(ev)
	}
}

func (qs *qwenSession) handleAssistant(ev *streamEvent) {
	if ev.Message == nil {
		return
	}

	var items []contentItem
	if err := json.Unmarshal(ev.Message.Content, &items); err != nil {
		// Try plain text
		var text string
		if err2 := json.Unmarshal(ev.Message.Content, &text); err2 == nil && text != "" {
			evt := core.Event{Type: core.EventText, Content: text}
			select {
			case qs.events <- evt:
			case <-qs.ctx.Done():
				return
			}
		}
		return
	}

	for _, item := range items {
		switch item.Type {
		case "text":
			if item.Text != "" {
				evt := core.Event{Type: core.EventText, Content: item.Text}
				select {
				case qs.events <- evt:
				case <-qs.ctx.Done():
					return
				}
			}
		case "thinking":
			if item.Thinking != "" {
				// Optionally emit thinking as debug text
				slog.Debug("qwenSession: thinking", "content", item.Thinking)
			}
		}
	}
}

func (qs *qwenSession) handleToolCall(ev *streamEvent) {
	if ev.ToolCall == nil {
		return
	}

	tc := ev.ToolCall
	inputPreview := extractToolPreview(tc.Input)

	evt := core.Event{
		Type:      core.EventToolUse,
		ToolName:  tc.Name,
		ToolInput: inputPreview,
		RequestID: tc.ID,
	}
	select {
	case qs.events <- evt:
	case <-qs.ctx.Done():
		return
	}
}

func (qs *qwenSession) handleToolResult(ev *streamEvent) {
	if ev.ToolCall == nil {
		return
	}

	tc := ev.ToolCall
	resultPreview := extractToolPreview(tc.Result)

	evt := core.Event{
		Type:     core.EventToolResult,
		ToolName: tc.Name,
		Content:  resultPreview,
	}
	select {
	case qs.events <- evt:
	case <-qs.ctx.Done():
		return
	}
}

func (qs *qwenSession) handleResult(ev *streamEvent) {
	var finalText string
	if ev.Message != nil {
		var items []contentItem
		if err := json.Unmarshal(ev.Message.Content, &items); err == nil {
			for _, item := range items {
				if item.Type == "text" && item.Text != "" {
					finalText = item.Text
				}
			}
		} else {
			// Try plain text
			json.Unmarshal(ev.Message.Content, &finalText)
		}
	}

	evt := core.Event{
		Type:      core.EventResult,
		Content:   finalText,
		SessionID: qs.CurrentSessionID(),
		Done:      ev.Done || true,
	}
	select {
	case qs.events <- evt:
	case <-qs.ctx.Done():
		return
	}
}

func (qs *qwenSession) RespondPermission(requestID string, result core.PermissionResult) error {
	// Qwen Code doesn't support dynamic permission responses via stdio
	// Permission decisions are handled via --approval-mode or --yolo flags
	slog.Debug("qwenSession: RespondPermission not supported, using approval mode", "requestID", requestID)
	return nil
}

func (qs *qwenSession) Events() <-chan core.Event {
	return qs.events
}

func (qs *qwenSession) CurrentSessionID() string {
	v, _ := qs.sessionID.Load().(string)
	return v
}

func (qs *qwenSession) Alive() bool {
	return qs.alive.Load()
}

func (qs *qwenSession) Close() error {
	qs.alive.Store(false)
	qs.cancel()
	done := make(chan struct{})
	go func() {
		qs.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(8 * time.Second):
		slog.Warn("qwenSession: close timed out, abandoning wg.Wait")
	}
	close(qs.events)
	return nil
}

// ── helpers ──────────────────────────────────────────────────

// extractToolPreview parses the JSON input/result of a tool call and returns a short preview string.
func extractToolPreview(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}

	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return string(raw)
	}

	// Common tool input patterns
	if cmd, ok := m["command"].(string); ok {
		return cmd
	}
	if file, ok := m["file_path"].(string); ok {
		return file
	}
	if pattern, ok := m["pattern"].(string); ok {
		return pattern
	}
	if query, ok := m["query"].(string); ok {
		return query
	}
	if content, ok := m["content"].(string); ok {
		return content
	}

	// Fallback: marshal back to JSON
	b, _ := json.Marshal(m)
	return string(b)
}

func truncStr(s string, maxRunes int) string {
	if utf8.RuneCountInString(s) <= maxRunes {
		return s
	}
	return string([]rune(s)[:maxRunes]) + "..."
}
