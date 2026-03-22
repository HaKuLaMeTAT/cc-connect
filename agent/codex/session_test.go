package codex

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/chenhg5/cc-connect/core"
)

func TestNormalizeReasoningEffort_RejectsMinimal(t *testing.T) {
	if got := normalizeReasoningEffort("minimal"); got != "" {
		t.Fatalf("normalizeReasoningEffort(minimal) = %q, want empty", got)
	}
	if got := normalizeReasoningEffort("min"); got != "" {
		t.Fatalf("normalizeReasoningEffort(min) = %q, want empty", got)
	}
}

func TestAvailableReasoningEfforts_ExcludesMinimal(t *testing.T) {
	agent := &Agent{}
	got := agent.AvailableReasoningEfforts()
	want := []string{"low", "medium", "high", "xhigh"}
	if len(got) != len(want) {
		t.Fatalf("AvailableReasoningEfforts len = %d, want %d, got=%v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("AvailableReasoningEfforts[%d] = %q, want %q, got=%v", i, got[i], want[i], got)
		}
	}
}

func TestBuildExecArgs_IncludesReasoningEffort(t *testing.T) {
	cs, err := newCodexSession(context.Background(), "/tmp/project", "o3", "high", "full-auto", "", nil)
	if err != nil {
		t.Fatalf("newCodexSession: %v", err)
	}

	args := cs.buildExecArgs("hello")

	want := []string{
		"-c",
		`approval_policy="never"`,
		"-c",
		`sandbox_mode="workspace-write"`,
		"exec",
		"--json",
		"--skip-git-repo-check",
		"--model",
		"o3",
		"-c",
		`model_reasoning_effort="high"`,
		"--cd",
		"/tmp/project",
		"hello",
	}
	if len(args) != len(want) {
		t.Fatalf("args len = %d, want %d, args=%v", len(args), len(want), args)
	}
	for i := range want {
		if args[i] != want[i] {
			t.Fatalf("args[%d] = %q, want %q, args=%v", i, args[i], want[i], args)
		}
	}
}

func TestBuildExecArgs_AutoEditUsesWorkspaceWriteSandbox(t *testing.T) {
	cs, err := newCodexSession(context.Background(), "/tmp/project", "o3", "", "auto-edit", "", nil)
	if err != nil {
		t.Fatalf("newCodexSession: %v", err)
	}

	args := cs.buildExecArgs("hello")

	want := []string{
		"-c",
		`approval_policy="on-request"`,
		"-c",
		`sandbox_mode="workspace-write"`,
		"exec",
		"--json",
		"--skip-git-repo-check",
		"--model",
		"o3",
		"--cd",
		"/tmp/project",
		"hello",
	}
	if len(args) != len(want) {
		t.Fatalf("args len = %d, want %d, args=%v", len(args), len(want), args)
	}
	for i := range want {
		if args[i] != want[i] {
			t.Fatalf("args[%d] = %q, want %q, args=%v", i, args[i], want[i], args)
		}
	}
}

func TestBuildExecArgs_ResumeOmitsCdFlag(t *testing.T) {
	cs, err := newCodexSession(context.Background(), "/tmp/project", "", "", "full-auto", "thread-abc", nil)
	if err != nil {
		t.Fatalf("newCodexSession: %v", err)
	}

	args := cs.buildExecArgs("hello")

	for i, arg := range args {
		if arg == "--cd" {
			t.Fatalf("resume args should not contain --cd, but found at index %d: %v", i, args)
		}
	}

	if !containsSequence(args, []string{"exec", "resume", "--json", "--skip-git-repo-check", "thread-abc", "hello"}) {
		t.Fatalf("resume args missing expected sequence: %v", args)
	}
}

func TestBuildExecArgs_SuggestUsesUntrustedReadOnly(t *testing.T) {
	cs, err := newCodexSession(context.Background(), "/tmp/project", "", "", "suggest", "", nil)
	if err != nil {
		t.Fatalf("newCodexSession: %v", err)
	}

	args := cs.buildExecArgs("hello")

	want := []string{
		"-c",
		`approval_policy="untrusted"`,
		"-c",
		`sandbox_mode="read-only"`,
		"exec",
		"--json",
		"--skip-git-repo-check",
		"--cd",
		"/tmp/project",
		"hello",
	}
	if len(args) != len(want) {
		t.Fatalf("args len = %d, want %d, args=%v", len(args), len(want), args)
	}
	for i := range want {
		if args[i] != want[i] {
			t.Fatalf("args[%d] = %q, want %q, args=%v", i, args[i], want[i], args)
		}
	}
}

func TestHandleEvent_EmitsPermissionRequest(t *testing.T) {
	cs, err := newCodexSession(context.Background(), "/tmp/project", "", "", "auto-edit", "", nil)
	if err != nil {
		t.Fatalf("newCodexSession: %v", err)
	}
	defer cs.Close()

	cs.handleEvent(map[string]any{
		"type":       "permission_request",
		"tool_name":  "exec_command",
		"request_id": "req-1",
		"parameters": map[string]any{"cmd": "pwd"},
	})

	select {
	case evt := <-cs.Events():
		if evt.Type != core.EventPermissionRequest {
			t.Fatalf("event type = %q, want %q", evt.Type, core.EventPermissionRequest)
		}
		if evt.ToolName != "exec_command" {
			t.Fatalf("tool = %q, want exec_command", evt.ToolName)
		}
		if evt.ToolInput != "pwd" {
			t.Fatalf("tool input = %q, want pwd", evt.ToolInput)
		}
		if evt.RequestID != "req-1" {
			t.Fatalf("request id = %q, want req-1", evt.RequestID)
		}
	case <-context.Background().Done():
		t.Fatal("unexpected context cancellation")
	default:
		t.Fatal("expected permission event")
	}
}

func TestSend_HandlesLargeJSONLines(t *testing.T) {
	workDir := t.TempDir()
	binDir := filepath.Join(workDir, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatalf("mkdir bin: %v", err)
	}

	largeText := strings.Repeat("x", 11*1024*1024)
	encodedText, err := json.Marshal(largeText)
	if err != nil {
		t.Fatalf("marshal large text: %v", err)
	}

	payload := strings.Join([]string{
		`{"type":"thread.started","thread_id":"thread-large"}`,
		`{"type":"item.completed","item":{"type":"agent_message","content":[{"type":"output_text","text":` + string(encodedText) + `}]}}`,
		`{"type":"turn.completed"}`,
	}, "\n") + "\n"

	payloadFile := filepath.Join(workDir, "payload.jsonl")
	if err := os.WriteFile(payloadFile, []byte(payload), 0o644); err != nil {
		t.Fatalf("write payload: %v", err)
	}

	scriptPath := filepath.Join(binDir, "codex")
	script := "#!/bin/sh\ncat \"$CODEX_PAYLOAD_FILE\"\n"
	if err := os.WriteFile(scriptPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake codex: %v", err)
	}

	t.Setenv("CODEX_PAYLOAD_FILE", payloadFile)
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	cs, err := newCodexSession(context.Background(), workDir, "", "", "", "", nil)
	if err != nil {
		t.Fatalf("newCodexSession: %v", err)
	}
	defer cs.Close()

	if err := cs.Send("hello", nil); err != nil {
		t.Fatalf("Send: %v", err)
	}

	var gotTextLen int
	var gotResult bool
	timeout := time.After(5 * time.Second)

	for !gotResult {
		select {
		case evt := <-cs.Events():
			if evt.Type == core.EventError {
				t.Fatalf("unexpected error event: %v", evt.Error)
			}
			if evt.Type == core.EventText {
				gotTextLen = len(evt.Content)
			}
			if evt.Type == core.EventResult && evt.Done {
				gotResult = true
			}
		case <-timeout:
			t.Fatal("timed out waiting for large JSON line events")
		}
	}

	if gotTextLen != len(largeText) {
		t.Fatalf("text len = %d, want %d", gotTextLen, len(largeText))
	}
	if got := cs.CurrentSessionID(); got != "thread-large" {
		t.Fatalf("CurrentSessionID() = %q, want thread-large", got)
	}
}

func containsSequence(args, want []string) bool {
	if len(want) == 0 {
		return true
	}
	for i := 0; i+len(want) <= len(args); i++ {
		match := true
		for j := range want {
			if args[i+j] != want[j] {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}

func TestCodexSession_ContinueSessionTreatedAsFresh(t *testing.T) {
	s, err := newCodexSession(context.Background(), "/tmp", "", "", "full-auto", core.ContinueSession, nil)
	if err != nil {
		t.Fatalf("newCodexSession: %v", err)
	}
	defer s.Close()

	if got := s.CurrentSessionID(); got != "" {
		t.Errorf("ContinueSession should be treated as fresh: threadID = %q, want empty", got)
	}
}
