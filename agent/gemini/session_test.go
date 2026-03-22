package gemini

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/chenhg5/cc-connect/core"
)

func TestGeminiSessionSend_ClosesStdinAndResumesNextTurn(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	logPath := filepath.Join(tmpDir, "args.log")
	scriptPath := filepath.Join(tmpDir, "fake-gemini.sh")
	script := `#!/bin/sh
set -eu
printf '%s\n' "$*" >> "$FAKE_GEMINI_LOG"
input=$(cat)
printf '{"type":"init","session_id":"sid-1","model":"fake-model"}\n'
printf '{"type":"message","role":"assistant","content":"ok"}\n'
printf '{"type":"result","status":"success","stats":{"prompt_tokens":1,"completion_tokens":2,"total_tokens":3}}\n'
`
	if err := os.WriteFile(scriptPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake gemini script: %v", err)
	}

	gs, err := newGeminiSession(
		context.Background(),
		scriptPath,
		tmpDir,
		"fake-model",
		"default",
		"",
		[]string{"FAKE_GEMINI_LOG=" + logPath},
		2*time.Second,
	)
	if err != nil {
		t.Fatalf("newGeminiSession: %v", err)
	}
	defer gs.Close()

	if err := gs.Send("hello", nil); err != nil {
		t.Fatalf("first Send: %v", err)
	}
	waitForGeminiTerminalEvent(t, gs.Events())

	if got := gs.CurrentSessionID(); got != "sid-1" {
		t.Fatalf("CurrentSessionID = %q, want sid-1", got)
	}
	if usage := gs.usage.Load(); usage == nil || usage.TotalTokens != 3 {
		t.Fatalf("usage = %#v, want total_tokens=3", usage)
	}

	if err := gs.Send("again", nil); err != nil {
		t.Fatalf("second Send: %v", err)
	}
	waitForGeminiTerminalEvent(t, gs.Events())

	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read args log: %v", err)
	}
	lines := strings.FieldsFunc(strings.TrimSpace(string(data)), func(r rune) bool { return r == '\n' || r == '\r' })
	if len(lines) != 2 {
		t.Fatalf("got %d arg log lines, want 2: %q", len(lines), string(data))
	}
	if strings.Contains(lines[0], "--resume") {
		t.Fatalf("first turn unexpectedly resumed: %q", lines[0])
	}
	if !strings.Contains(lines[1], "--resume sid-1") {
		t.Fatalf("second turn missing --resume sid-1: %q", lines[1])
	}
}

func waitForGeminiTerminalEvent(t *testing.T, events <-chan core.Event) {
	t.Helper()

	deadline := time.After(3 * time.Second)
	for {
		select {
		case evt := <-events:
			if evt.Type == core.EventError {
				t.Fatalf("unexpected EventError: %v", evt.Error)
			}
			if evt.Type == core.EventResult {
				return
			}
		case <-deadline:
			t.Fatal("timed out waiting for Gemini terminal event")
		}
	}
}

func TestGeminiSession_ContinueSessionTreatedAsFresh(t *testing.T) {
	s, err := newGeminiSession(context.Background(), "echo", "/tmp", "", "default", core.ContinueSession, nil, 0)
	if err != nil {
		t.Fatalf("newGeminiSession: %v", err)
	}
	defer s.Close()

	if got := s.CurrentSessionID(); got != "" {
		t.Errorf("ContinueSession should be treated as fresh: chatID = %q, want empty", got)
	}
}
