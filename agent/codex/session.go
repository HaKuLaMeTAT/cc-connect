package codex

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
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

// codexSession manages a multi-turn Codex conversation.
// First Send() uses `codex exec`, subsequent ones use `codex exec resume <threadID>`.
type codexSession struct {
	workDir   string
	model     string
	effort    string
	mode      string
	extraEnv  []string
	events    chan core.Event
	threadID  atomic.Value // stores string — Codex thread_id
	ctx       context.Context
	cancel    context.CancelFunc
	wg        sync.WaitGroup
	alive     atomic.Bool
	closeOnce sync.Once

	pendingMsgs []string // buffered agent_message texts awaiting classification
}

func newCodexSession(ctx context.Context, workDir, model, effort, mode, resumeID string, extraEnv []string) (*codexSession, error) {
	sessionCtx, cancel := context.WithCancel(ctx)

	cs := &codexSession{
		workDir:  workDir,
		model:    model,
		effort:   effort,
		mode:     mode,
		extraEnv: extraEnv,
		events:   make(chan core.Event, 64),
		ctx:      sessionCtx,
		cancel:   cancel,
	}
	cs.alive.Store(true)

	if resumeID != "" && resumeID != core.ContinueSession {
		cs.threadID.Store(resumeID)
	}

	return cs, nil
}

// Send launches a codex subprocess.
// If a threadID exists (from a prior turn or resume), uses `codex exec resume <id> <prompt>`.
// Otherwise uses `codex exec <prompt>` to start a new conversation.
func (cs *codexSession) Send(prompt string, images []core.ImageAttachment) error {
	if len(images) > 0 {
		slog.Warn("codexSession: images not supported by Codex, ignoring")
	}
	if !cs.alive.Load() {
		return fmt.Errorf("session is closed")
	}

	isResume := cs.CurrentSessionID() != ""
	args := cs.buildExecArgs(prompt)

	slog.Debug("codexSession: launching", "resume", isResume, "args", core.RedactArgs(args))

	cmd := exec.CommandContext(cs.ctx, "codex", args...)
	cmd.Dir = cs.workDir
	cmd.Stdin = strings.NewReader(prompt)
	if len(cs.extraEnv) > 0 {
		cmd.Env = core.MergeEnv(os.Environ(), cs.extraEnv)
	}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("codexSession: stdout pipe: %w", err)
	}

	var stderrBuf bytes.Buffer
	cmd.Stderr = &stderrBuf

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("codexSession: start: %w", err)
	}

	cs.wg.Add(1)
	go cs.readLoop(cmd, stdout, &stderrBuf)

	return nil
}

func (cs *codexSession) buildExecArgs(prompt string) []string {
	tid := cs.CurrentSessionID()
	isResume := tid != ""

	var args []string
	switch cs.mode {
	case "suggest":
		args = append(args,
			"-c", `approval_policy="untrusted"`,
			"-c", `sandbox_mode="read-only"`,
		)
	case "auto-edit":
		args = append(args,
			"-c", `approval_policy="on-request"`,
			"-c", `sandbox_mode="workspace-write"`,
		)
	case "full-auto":
		args = append(args,
			"-c", `approval_policy="never"`,
			"-c", `sandbox_mode="workspace-write"`,
		)
	case "yolo":
		args = append(args, "--dangerously-bypass-approvals-and-sandbox")
	}

	if isResume {
		args = append(args, "exec", "resume", "--json", "--skip-git-repo-check")
	} else {
		args = append(args, "exec", "--json", "--skip-git-repo-check")
	}

	if cs.model != "" {
		args = append(args, "--model", cs.model)
	}
	if cs.effort != "" {
		args = append(args, "-c", fmt.Sprintf("model_reasoning_effort=%q", cs.effort))
	}

	if isResume {
		// `codex exec resume` does not accept `--cd`; cmd.Dir already sets cwd.
		args = append(args, tid, "-")
	} else {
		args = append(args, "--cd", cs.workDir, "-")
	}
	return args
}

func (cs *codexSession) readLoop(cmd *exec.Cmd, stdout io.ReadCloser, stderrBuf *bytes.Buffer) {
	defer cs.wg.Done()
	defer func() {
		if err := cmd.Wait(); err != nil {
			stderrMsg := strings.TrimSpace(stderrBuf.String())
			if stderrMsg != "" {
				slog.Error("codexSession: process failed", "error", err, "stderr", stderrMsg)
				evt := core.Event{Type: core.EventError, Error: fmt.Errorf("%s", stderrMsg)}
				select {
				case cs.events <- evt:
				case <-cs.ctx.Done():
					return
				}
			}
		}
		if tid := cs.CurrentSessionID(); tid != "" {
			patchSessionSource(tid)
		}
	}()

	if err := readJSONLines(stdout, func(line []byte) error {
		lineText := string(line)
		if lineText == "" {
			return nil
		}

		slog.Debug("codexSession: raw", "line", truncate(lineText, 500))

		var raw map[string]any
		if err := json.Unmarshal(line, &raw); err != nil {
			slog.Debug("codexSession: non-JSON line", "line", lineText)
			return nil
		}

		cs.handleEvent(raw)
		return nil
	}); err != nil {
		slog.Error("codexSession: read stdout error", "error", err)
		evt := core.Event{Type: core.EventError, Error: fmt.Errorf("read stdout: %w", err)}
		select {
		case cs.events <- evt:
		case <-cs.ctx.Done():
			return
		}
	}
}

func readJSONLines(r io.Reader, handle func([]byte) error) error {
	reader := bufio.NewReader(r)

	for {
		line, err := reader.ReadBytes('\n')
		if errors.Is(err, io.EOF) && len(line) == 0 {
			return nil
		}
		if err != nil && !errors.Is(err, io.EOF) {
			return err
		}

		line = bytes.TrimRight(line, "\r\n")
		if len(line) > 0 {
			if err := handle(line); err != nil {
				return err
			}
		}

		if errors.Is(err, io.EOF) {
			return nil
		}
	}
}

func (cs *codexSession) handleEvent(raw map[string]any) {
	eventType, _ := raw["type"].(string)

	switch eventType {
	case "thread.started":
		if tid, ok := raw["thread_id"].(string); ok {
			cs.threadID.Store(tid)
			slog.Debug("codexSession: thread started", "thread_id", tid)
		}

	case "turn.started":
		cs.pendingMsgs = cs.pendingMsgs[:0]
		slog.Debug("codexSession: turn started")

	case "item.started":
		cs.handleItemStarted(raw)

	case "item.completed":
		cs.handleItemCompleted(raw)

	case "turn.completed":
		cs.flushPendingAsText()
		evt := core.Event{Type: core.EventResult, SessionID: cs.CurrentSessionID(), Done: true}
		select {
		case cs.events <- evt:
		case <-cs.ctx.Done():
			return
		}

	case "turn.failed":
		errMsg := ""
		if errObj, ok := raw["error"].(map[string]any); ok {
			errMsg, _ = errObj["message"].(string)
		}
		if errMsg == "" {
			errMsg = "turn failed (no details)"
		}
		slog.Warn("codexSession: turn failed", "error", errMsg)
		evt := core.Event{Type: core.EventError, Error: fmt.Errorf("%s", errMsg)}
		select {
		case cs.events <- evt:
		case <-cs.ctx.Done():
			return
		}

	case "permission_request":
		cs.handlePermissionRequest(raw)

	case "error":
		msg, _ := raw["message"].(string)
		if strings.Contains(msg, "Reconnecting") || strings.Contains(msg, "Falling back") {
			slog.Debug("codexSession: transient error", "message", msg)
		} else {
			slog.Warn("codexSession: error event", "message", msg)
		}

	default:
		slog.Debug("codexSession: unhandled event type", "type", eventType)
	}
}

func (cs *codexSession) handlePermissionRequest(raw map[string]any) {
	toolName, _ := raw["tool_name"].(string)
	if toolName == "" {
		toolName, _ = raw["name"].(string)
	}
	requestID, _ := raw["request_id"].(string)
	params, _ := raw["parameters"].(map[string]any)
	if params == nil {
		params, _ = raw["input"].(map[string]any)
	}

	input := formatPermissionPreview(toolName, params)
	evt := core.Event{
		Type:         core.EventPermissionRequest,
		ToolName:     toolName,
		ToolInput:    input,
		ToolInputRaw: params,
		RequestID:    requestID,
	}
	select {
	case cs.events <- evt:
	case <-cs.ctx.Done():
	}
}

// flushPendingAsThinking emits all buffered agent_messages as EventThinking.
func (cs *codexSession) flushPendingAsThinking() {
	for _, text := range cs.pendingMsgs {
		evt := core.Event{Type: core.EventThinking, Content: text}
		select {
		case cs.events <- evt:
		case <-cs.ctx.Done():
			return
		}
	}
	cs.pendingMsgs = cs.pendingMsgs[:0]
}

// flushPendingAsText emits all buffered agent_messages as EventText (final response).
func (cs *codexSession) flushPendingAsText() {
	for _, text := range cs.pendingMsgs {
		evt := core.Event{Type: core.EventText, Content: text}
		select {
		case cs.events <- evt:
		case <-cs.ctx.Done():
			return
		}
	}
	cs.pendingMsgs = cs.pendingMsgs[:0]
}

var codexToolNames = map[string]string{
	"web_search":       "WebSearch",
	"file_search":      "FileSearch",
	"code_interpreter": "CodeInterpreter",
	"computer_use":     "ComputerUse",
	"mcp_tool":         "MCP",
}

func (cs *codexSession) handleItemStarted(raw map[string]any) {
	item, ok := raw["item"].(map[string]any)
	if !ok {
		slog.Debug("codexSession: item.started missing item field")
		return
	}
	itemType, _ := item["type"].(string)
	slog.Debug("codexSession: item.started", "item_type", itemType)

	if itemType == "agent_message" || itemType == "message" || itemType == "reasoning" {
		return
	}

	// Any non-message item is a tool use; flush pending messages as thinking first.
	cs.flushPendingAsThinking()

	switch itemType {
	case "command_execution":
		command, _ := item["command"].(string)
		evt := core.Event{Type: core.EventToolUse, ToolName: "Bash", ToolInput: command}
		select {
		case cs.events <- evt:
		case <-cs.ctx.Done():
			return
		}
	case "function_call":
		name, _ := item["name"].(string)
		args, _ := item["arguments"].(string)
		evt := core.Event{Type: core.EventToolUse, ToolName: name, ToolInput: args}
		select {
		case cs.events <- evt:
		case <-cs.ctx.Done():
			return
		}
	}
	// Other tool types (web_search etc.) have empty fields at start;
	// their EventToolUse is emitted from handleItemCompleted instead.
}

func (cs *codexSession) handleItemCompleted(raw map[string]any) {
	item, ok := raw["item"].(map[string]any)
	if !ok {
		slog.Debug("codexSession: item.completed missing item field")
		return
	}
	itemType, _ := item["type"].(string)
	slog.Debug("codexSession: item.completed", "item_type", itemType)

	switch itemType {
	case "reasoning":
		text := extractItemText(item, "summary", "summary_text")
		if text != "" {
			evt := core.Event{Type: core.EventThinking, Content: text}
			select {
			case cs.events <- evt:
			case <-cs.ctx.Done():
				return
			}
		}

	case "agent_message", "message":
		text := extractItemText(item, "content", "output_text")
		if text != "" {
			cs.pendingMsgs = append(cs.pendingMsgs, text)
		}

	case "command_execution":
		command, _ := item["command"].(string)
		status, _ := item["status"].(string)
		output, _ := item["aggregated_output"].(string)
		exitCode, _ := item["exit_code"].(float64)

		slog.Debug("codexSession: command completed",
			"command", truncate(command, 100),
			"status", status,
			"exit_code", int(exitCode),
			"output_len", len(output),
		)

	case "function_call":
		name, _ := item["name"].(string)
		status, _ := item["status"].(string)
		output, _ := item["output"].(string)
		slog.Debug("codexSession: function_call completed",
			"name", name, "status", status, "output_len", len(output),
		)

	case "function_call_output":
		slog.Debug("codexSession: function_call_output")

	case "error":
		msg, _ := item["message"].(string)
		if msg != "" && !strings.Contains(msg, "Falling back") {
			slog.Warn("codexSession: item error", "message", msg)
		}

	default:
		if toolName, known := codexToolNames[itemType]; known {
			input := codexExtractToolInput(item)
			evt := core.Event{Type: core.EventToolUse, ToolName: toolName, ToolInput: input}
			select {
			case cs.events <- evt:
			case <-cs.ctx.Done():
				return
			}
		} else {
			slog.Debug("codexSession: unhandled item type", "item_type", itemType)
		}
	}
}

// codexExtractToolInput extracts a human-readable input from a Codex tool item.
// For web_search, it reads action.queries[] or falls back to the top-level query.
func codexExtractToolInput(item map[string]any) string {
	if action, ok := item["action"].(map[string]any); ok {
		if queries, ok := action["queries"].([]any); ok && len(queries) > 0 {
			var parts []string
			for _, q := range queries {
				if s, ok := q.(string); ok && s != "" {
					parts = append(parts, s)
				}
			}
			if len(parts) > 0 {
				return strings.Join(parts, "\n")
			}
		}
		if q, _ := action["query"].(string); q != "" {
			return q
		}
	}
	if q, _ := item["query"].(string); q != "" {
		return q
	}
	if n, _ := item["name"].(string); n != "" {
		return n
	}
	return ""
}

func formatPermissionPreview(toolName string, params map[string]any) string {
	if len(params) == 0 {
		return ""
	}
	if toolName == "exec_command" || toolName == "Bash" {
		if cmd, _ := params["cmd"].(string); cmd != "" {
			return cmd
		}
	}
	b, err := json.Marshal(params)
	if err != nil {
		return ""
	}
	return string(b)
}

// Codex exec mode does not expose a stdin approval response channel.
func (cs *codexSession) RespondPermission(_ string, _ core.PermissionResult) error {
	return fmt.Errorf("codex: permission responses are not supported in exec mode")
}

func (cs *codexSession) Events() <-chan core.Event {
	return cs.events
}

func (cs *codexSession) CurrentSessionID() string {
	v, _ := cs.threadID.Load().(string)
	return v
}

func (cs *codexSession) Alive() bool {
	return cs.alive.Load()
}

func (cs *codexSession) Close() error {
	cs.alive.Store(false)
	cs.cancel()
	done := make(chan struct{})
	go func() {
		cs.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		cs.closeOnce.Do(func() {
			close(cs.events)
		})
		return nil
	case <-time.After(8 * time.Second):
		slog.Warn("codexSession: close timed out, deferring events channel close until readLoop exits", "wait", 8*time.Second)
		go func() {
			<-done
			cs.closeOnce.Do(func() {
				close(cs.events)
			})
		}()
		return nil
	}
}

// extractItemText extracts text from an item's array field (e.g. "summary" or "content").
// It looks for elements matching the given elementType and concatenates their "text" fields.
// Falls back to the item's top-level "text" field if the array is missing or empty.
func extractItemText(item map[string]any, arrayField, elementType string) string {
	if arr, ok := item[arrayField].([]any); ok {
		var parts []string
		for _, elem := range arr {
			m, ok := elem.(map[string]any)
			if !ok {
				continue
			}
			if elementType != "" {
				if t, _ := m["type"].(string); t != elementType {
					continue
				}
			}
			if t, _ := m["text"].(string); t != "" {
				parts = append(parts, t)
			}
		}
		if len(parts) > 0 {
			return strings.Join(parts, "\n")
		}
	}
	text, _ := item["text"].(string)
	return text
}

func truncate(s string, maxRunes int) string {
	if utf8.RuneCountInString(s) <= maxRunes {
		return s
	}
	return string([]rune(s)[:maxRunes]) + "..."
}
