package core

import (
	"context"
	"errors"
	"testing"
	"time"
)

type controllableAgent struct {
	nextSession *controllableSession
}

func (a *controllableAgent) Name() string { return "controllable" }
func (a *controllableAgent) StartSession(_ context.Context, _ string) (AgentSession, error) {
	return a.nextSession, nil
}
func (a *controllableAgent) ListSessions(_ context.Context) ([]AgentSessionInfo, error) {
	return nil, nil
}
func (a *controllableAgent) Stop() error { return nil }

type controllableSession struct {
	id     string
	events chan Event
}

func newControllableSession(id string) *controllableSession {
	return &controllableSession{
		id:     id,
		events: make(chan Event, 8),
	}
}

func (s *controllableSession) Send(_ string, _ []ImageAttachment) error             { return nil }
func (s *controllableSession) RespondPermission(_ string, _ PermissionResult) error { return nil }
func (s *controllableSession) Events() <-chan Event                                 { return s.events }
func (s *controllableSession) CurrentSessionID() string                             { return s.id }
func (s *controllableSession) Alive() bool                                          { return true }
func (s *controllableSession) Close() error                                         { return nil }

func TestRelayManager_RelayContextUsesProjectDefaultTimeout(t *testing.T) {
	rm := NewRelayManager("")

	ctx, cancel := rm.relayContext(context.Background(), "gemini")
	defer cancel()

	deadline, ok := ctx.Deadline()
	if !ok {
		t.Fatal("expected relay context deadline")
	}
	if remaining := time.Until(deadline); remaining <= 0 || remaining > geminiRelayTimeout {
		t.Fatalf("time until deadline = %v, want within (0, %v]", remaining, geminiRelayTimeout)
	}
}

func TestRelayManager_RelayContextHonorsConfiguredTimeout(t *testing.T) {
	rm := NewRelayManager("")
	rm.SetTimeout(3 * time.Second)

	ctx, cancel := rm.relayContext(context.Background(), "codex")
	defer cancel()

	deadline, ok := ctx.Deadline()
	if !ok {
		t.Fatal("expected relay context deadline")
	}
	if remaining := time.Until(deadline); remaining <= 0 || remaining > 3*time.Second {
		t.Fatalf("time until deadline = %v, want within (0, 3s]", remaining)
	}
}

func TestRelayManager_RelayContextDisablesTimeoutAtZero(t *testing.T) {
	rm := NewRelayManager("")
	rm.SetTimeout(0)

	baseCtx := context.Background()
	ctx, cancel := rm.relayContext(baseCtx, "codex")
	defer cancel()

	if ctx != baseCtx {
		t.Fatal("expected relayContext to return the original context when timeout is disabled")
	}
	if _, ok := ctx.Deadline(); ok {
		t.Fatal("expected no deadline when timeout is disabled")
	}
}

func TestHandleRelay_ReturnsPartialOnTimeout(t *testing.T) {
	e := newTestEngine()
	session := newControllableSession("relay-session")
	e.agent = &controllableAgent{nextSession: session}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()

	type relayResult struct {
		resp string
		err  error
	}
	done := make(chan relayResult, 1)
	go func() {
		resp, err := e.HandleRelay(ctx, "source", "chat-1", "hello")
		done <- relayResult{resp: resp, err: err}
	}()

	session.events <- Event{Type: EventText, Content: "partial response", SessionID: "relay-session"}
	time.Sleep(40 * time.Millisecond)
	session.events <- Event{Type: EventThinking, Content: "still working"}

	got := <-done
	if got.err != nil {
		t.Fatalf("HandleRelay() error = %v, want nil", got.err)
	}
	if got.resp != "partial response" {
		t.Fatalf("HandleRelay() response = %q, want %q", got.resp, "partial response")
	}
}

func TestHandleRelay_TimeoutWithoutTextReturnsContextError(t *testing.T) {
	e := newTestEngine()
	session := newControllableSession("relay-session")
	e.agent = &controllableAgent{nextSession: session}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()

	type relayResult struct {
		resp string
		err  error
	}
	done := make(chan relayResult, 1)
	go func() {
		resp, err := e.HandleRelay(ctx, "source", "chat-1", "hello")
		done <- relayResult{resp: resp, err: err}
	}()

	time.Sleep(40 * time.Millisecond)
	session.events <- Event{Type: EventThinking, Content: "still working"}

	got := <-done
	if got.resp != "" {
		t.Fatalf("HandleRelay() response = %q, want empty", got.resp)
	}
	if !errors.Is(got.err, context.DeadlineExceeded) {
		t.Fatalf("HandleRelay() error = %v, want context deadline exceeded", got.err)
	}
}
