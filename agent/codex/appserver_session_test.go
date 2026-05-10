package codex

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/chenhg5/cc-connect/core"
)

type bufferWriteCloser struct {
	mu sync.Mutex
	b  strings.Builder
}

func (w *bufferWriteCloser) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.b.Write(p)
}

func (w *bufferWriteCloser) Close() error { return nil }

func (w *bufferWriteCloser) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.b.String()
}

func TestAppServerSession_BuildArgsHonorsConfig(t *testing.T) {
	s := &appServerSession{
		url:           "",
		model:         "gpt-5.5",
		effort:        "medium",
		modelProvider: "router",
		baseURL:       "https://router.example.com/v1",
	}

	args := s.buildAppServerArgs()

	if containsSequence(args, []string{"--listen"}) {
		t.Fatalf("stdio app-server args should not include --listen: %v", args)
	}
	for _, want := range [][]string{
		{"app-server"},
		{"-c", `model="gpt-5.5"`},
		{"-c", `model_reasoning_effort="medium"`},
		{"-c", `model_provider="router"`},
		{"-c", `openai_base_url="https://router.example.com/v1"`},
	} {
		if !containsSequence(args, want) {
			t.Fatalf("args missing %v: %v", want, args)
		}
	}
}

func TestAppServerSession_ApprovalRequestEmitsPermissionAndResponds(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	w := &bufferWriteCloser{}
	s := &appServerSession{
		ctx:              ctx,
		stdin:            w,
		events:           make(chan core.Event, 4),
		pendingApprovals: make(map[string]chan core.PermissionResult),
	}

	s.handleServerRequest(map[string]json.RawMessage{
		"id":     json.RawMessage(`1`),
		"method": json.RawMessage(`"item/commandExecution/requestApproval"`),
		"params": json.RawMessage(`{"command":"ls -la","cwd":"/tmp/project"}`),
	})

	select {
	case evt := <-s.events:
		if evt.Type != core.EventPermissionRequest {
			t.Fatalf("event type = %s, want permission request", evt.Type)
		}
		if evt.RequestID != "1" || evt.ToolName != "Bash" {
			t.Fatalf("unexpected permission event: %#v", evt)
		}
		if !strings.Contains(evt.ToolInput, "ls -la") || !strings.Contains(evt.ToolInput, "/tmp/project") {
			t.Fatalf("tool input = %q, want command and cwd", evt.ToolInput)
		}
	case <-time.After(time.Second):
		t.Fatal("permission event not emitted")
	}

	if err := s.RespondPermission("1", core.PermissionResult{Behavior: "allow"}); err != nil {
		t.Fatalf("RespondPermission: %v", err)
	}

	deadline := time.After(time.Second)
	for {
		if strings.Contains(w.String(), `"decision":"accept"`) {
			return
		}
		select {
		case <-deadline:
			t.Fatalf("approval response not written: %q", w.String())
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func TestAppServerSession_IdleStatusCompletesTurn(t *testing.T) {
	s := &appServerSession{
		events:      make(chan core.Event, 4),
		currentTurn: "turn-1",
		pendingMsgs: []string{"final text"},
	}

	s.handleNotification("thread/status/changed", json.RawMessage(`{"threadId":"thread-1","status":{"type":"idle"}}`))

	select {
	case evt := <-s.events:
		if evt.Type != core.EventText || evt.Content != "final text" {
			t.Fatalf("first event = %#v, want final text", evt)
		}
	case <-time.After(time.Second):
		t.Fatal("text event not emitted")
	}

	select {
	case evt := <-s.events:
		if evt.Type != core.EventResult || !evt.Done {
			t.Fatalf("second event = %#v, want done result", evt)
		}
	case <-time.After(time.Second):
		t.Fatal("result event not emitted")
	}
}
