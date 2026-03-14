package core

import (
	"context"
	"sync"
	"testing"
	"time"
)

type relayTypingPlatform struct {
	mu      sync.Mutex
	started int
	stopped int
	sent    []string
}

func (p *relayTypingPlatform) Name() string               { return "telegram" }
func (p *relayTypingPlatform) Start(MessageHandler) error { return nil }
func (p *relayTypingPlatform) Reply(_ context.Context, _ any, content string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.sent = append(p.sent, content)
	return nil
}
func (p *relayTypingPlatform) Send(_ context.Context, _ any, content string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.sent = append(p.sent, content)
	return nil
}
func (p *relayTypingPlatform) Stop() error { return nil }
func (p *relayTypingPlatform) ReconstructReplyCtx(sessionKey string) (any, error) {
	return sessionKey, nil
}
func (p *relayTypingPlatform) StartTyping(_ context.Context, _ any) (stop func()) {
	p.mu.Lock()
	p.started++
	p.mu.Unlock()
	return func() {
		p.mu.Lock()
		p.stopped++
		p.mu.Unlock()
	}
}

type relayResultAgent struct{}

func (a *relayResultAgent) Name() string { return "relay-result" }
func (a *relayResultAgent) StartSession(_ context.Context, _ string) (AgentSession, error) {
	return &relayResultSession{events: make(chan Event, 1)}, nil
}
func (a *relayResultAgent) ListSessions(_ context.Context) ([]AgentSessionInfo, error) { return nil, nil }
func (a *relayResultAgent) Stop() error                                                { return nil }

type relayResultSession struct {
	events chan Event
}

func (s *relayResultSession) Send(_ string, _ []ImageAttachment) error {
	go func() {
		time.Sleep(10 * time.Millisecond)
		s.events <- Event{Type: EventResult, Content: "relay ok", SessionID: "relay-session", Done: true}
		close(s.events)
	}()
	return nil
}
func (s *relayResultSession) RespondPermission(_ string, _ PermissionResult) error { return nil }
func (s *relayResultSession) Events() <-chan Event                                 { return s.events }
func (s *relayResultSession) CurrentSessionID() string                             { return "relay-session" }
func (s *relayResultSession) Alive() bool                                          { return true }
func (s *relayResultSession) Close() error                                         { return nil }

func TestSendToOtherBot_StartsAndStopsTypingIndicator(t *testing.T) {
	source := NewEngine("codex", &stubAgent{}, nil, "", LangEnglish)
	targetPlatform := &relayTypingPlatform{}
	target := NewEngine("gemini", &relayResultAgent{}, []Platform{targetPlatform}, "", LangEnglish)

	rm := NewRelayManager("")
	rm.RegisterEngine("codex", source)
	rm.RegisterEngine("gemini", target)
	source.SetRelayManager(rm)
	target.SetRelayManager(rm)

	if err := source.sendToOtherBot("telegram:123:456", "gemini", "hello"); err != nil {
		t.Fatalf("sendToOtherBot returned error: %v", err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		targetPlatform.mu.Lock()
		started := targetPlatform.started
		stopped := targetPlatform.stopped
		sent := append([]string(nil), targetPlatform.sent...)
		targetPlatform.mu.Unlock()

		if started == 1 && stopped == 1 && len(sent) == 1 && sent[0] == "relay ok" {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}

	targetPlatform.mu.Lock()
	defer targetPlatform.mu.Unlock()
	t.Fatalf("typing state mismatch: started=%d stopped=%d sent=%v", targetPlatform.started, targetPlatform.stopped, targetPlatform.sent)
}
