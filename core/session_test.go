package core

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestSessionManager_GetOrCreateActive(t *testing.T) {
	sm := NewSessionManager("")
	s1 := sm.GetOrCreateActive("user1")
	if s1 == nil {
		t.Fatal("expected non-nil session")
	}
	s2 := sm.GetOrCreateActive("user1")
	if s1.ID != s2.ID {
		t.Error("same user should get same active session")
	}

	s3 := sm.GetOrCreateActive("user2")
	if s3.ID == s1.ID {
		t.Error("different user should get different session")
	}
}

func TestSessionManager_NewSession(t *testing.T) {
	sm := NewSessionManager("")
	s1 := sm.NewSession("user1", "chat-a")
	s2 := sm.NewSession("user1", "chat-b")

	if s1.ID == s2.ID {
		t.Error("new sessions should have different IDs")
	}
	if s1.Name != "chat-a" || s2.Name != "chat-b" {
		t.Error("session names should match")
	}

	active := sm.GetOrCreateActive("user1")
	if active.ID != s2.ID {
		t.Error("latest session should be active")
	}
}

func TestSessionManager_SwitchSession(t *testing.T) {
	sm := NewSessionManager("")
	s1 := sm.NewSession("user1", "first")
	s2 := sm.NewSession("user1", "second")

	if sm.ActiveSessionID("user1") != s2.ID {
		t.Error("active should be s2")
	}

	switched, err := sm.SwitchSession("user1", s1.ID)
	if err != nil {
		t.Fatalf("SwitchSession: %v", err)
	}
	if switched.ID != s1.ID {
		t.Error("should have switched to s1")
	}
	if sm.ActiveSessionID("user1") != s1.ID {
		t.Error("active should now be s1")
	}
}

func TestSessionManager_SwitchByName(t *testing.T) {
	sm := NewSessionManager("")
	sm.NewSession("user1", "alpha")
	sm.NewSession("user1", "beta")

	switched, err := sm.SwitchSession("user1", "alpha")
	if err != nil {
		t.Fatalf("SwitchSession by name: %v", err)
	}
	if switched.Name != "alpha" {
		t.Error("should have switched to alpha")
	}
}

func TestSessionManager_SwitchNotFound(t *testing.T) {
	sm := NewSessionManager("")
	sm.NewSession("user1", "only")

	_, err := sm.SwitchSession("user1", "nonexistent")
	if err == nil {
		t.Error("expected error for nonexistent session")
	}
}

func TestSessionManager_ListSessions(t *testing.T) {
	sm := NewSessionManager("")
	sm.NewSession("user1", "a")
	sm.NewSession("user1", "b")
	sm.NewSession("user2", "c")

	list := sm.ListSessions("user1")
	if len(list) != 2 {
		t.Errorf("user1 should have 2 sessions, got %d", len(list))
	}

	list2 := sm.ListSessions("user2")
	if len(list2) != 1 {
		t.Errorf("user2 should have 1 session, got %d", len(list2))
	}
}

func TestSessionManager_SessionNames(t *testing.T) {
	sm := NewSessionManager("")
	sm.SetSessionName("agent-123", "my-chat")

	if got := sm.GetSessionName("agent-123"); got != "my-chat" {
		t.Errorf("got %q, want my-chat", got)
	}

	sm.SetSessionName("agent-123", "")
	if got := sm.GetSessionName("agent-123"); got != "" {
		t.Errorf("got %q, want empty after clear", got)
	}
}

func TestSessionManager_Persistence(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sessions.json")

	sm1 := NewSessionManager(path)
	sm1.NewSession("user1", "persisted")
	sm1.SetSessionName("agent-x", "custom-name")

	sm2 := NewSessionManager(path)
	list := sm2.ListSessions("user1")
	if len(list) != 1 {
		t.Fatalf("expected 1 session after reload, got %d", len(list))
	}
	if list[0].Name != "persisted" {
		t.Errorf("session name = %q, want persisted", list[0].Name)
	}
	if got := sm2.GetSessionName("agent-x"); got != "custom-name" {
		t.Errorf("session name after reload = %q, want custom-name", got)
	}
}

func TestSessionManager_GetOrCreateActive_Persists(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sessions.json")

	sm1 := NewSessionManager(path)
	s := sm1.GetOrCreateActive("user1")
	if s == nil {
		t.Fatal("expected non-nil session")
	}

	// Reload from disk — session should survive
	sm2 := NewSessionManager(path)
	list := sm2.ListSessions("user1")
	if len(list) != 1 {
		t.Fatalf("expected 1 session after reload, got %d", len(list))
	}
	if list[0].ID != s.ID {
		t.Errorf("reloaded session ID = %q, want %q", list[0].ID, s.ID)
	}
}

func TestSession_TryLockUnlock(t *testing.T) {
	s := &Session{}
	if !s.TryLock() {
		t.Error("first TryLock should succeed")
	}
	if s.TryLock() {
		t.Error("second TryLock should fail")
	}
	s.Unlock()
	if !s.TryLock() {
		t.Error("TryLock after Unlock should succeed")
	}
}

func TestSession_History(t *testing.T) {
	s := &Session{}
	s.AddHistory("user", "hello")
	s.AddHistory("assistant", "hi there")
	s.AddHistory("user", "bye")

	all := s.GetHistory(0)
	if len(all) != 3 {
		t.Errorf("expected 3 entries, got %d", len(all))
	}

	last2 := s.GetHistory(2)
	if len(last2) != 2 {
		t.Errorf("expected 2 entries, got %d", len(last2))
	}
	if last2[0].Content != "hi there" {
		t.Errorf("expected 'hi there', got %q", last2[0].Content)
	}

	s.ClearHistory()
	if h := s.GetHistory(0); len(h) != 0 {
		t.Errorf("expected empty history after clear, got %d", len(h))
	}
}

func TestSession_ConcurrentHistory(t *testing.T) {
	s := &Session{}
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.AddHistory("user", "msg")
		}()
	}
	wg.Wait()
	if h := s.GetHistory(0); len(h) != 50 {
		t.Errorf("expected 50 entries, got %d", len(h))
	}
}

func TestSession_SetAgentSessionID_RejectsContinueSentinel(t *testing.T) {
	s := &Session{}
	s.SetAgentSessionID("real-id")
	s.SetAgentSessionID(ContinueSession)
	if got := s.GetAgentSessionID(); got != "real-id" {
		t.Fatalf("GetAgentSessionID = %q, want real-id", got)
	}
	s.SetAgentSessionID("")
	if got := s.GetAgentSessionID(); got != "" {
		t.Fatalf("GetAgentSessionID after clear = %q, want empty", got)
	}
}

func TestSession_CompareAndSetAgentSessionID_ReplacesContinueSentinel(t *testing.T) {
	s := &Session{}
	s.mu.Lock()
	s.AgentSessionID = ContinueSession
	s.mu.Unlock()

	if !s.CompareAndSetAgentSessionID("real-id") {
		t.Fatal("expected CompareAndSetAgentSessionID to replace ContinueSession")
	}
	if got := s.GetAgentSessionID(); got != "real-id" {
		t.Fatalf("GetAgentSessionID = %q, want real-id", got)
	}
	if s.CompareAndSetAgentSessionID("another-id") {
		t.Fatal("expected CompareAndSetAgentSessionID to fail once a real id is set")
	}
}

func TestSession_SetAgentInfo_NormalizesContinueSentinel(t *testing.T) {
	s := &Session{}
	s.SetAgentInfo(ContinueSession, "demo")
	if got := s.GetAgentSessionID(); got != "" {
		t.Fatalf("GetAgentSessionID = %q, want empty", got)
	}
	if got := s.GetName(); got != "demo" {
		t.Fatalf("GetName = %q, want demo", got)
	}
}

func TestSessionManager_Load_StripsContinueSentinel(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sessions.json")
	raw := `{
  "sessions": {
    "s1": {
      "id": "s1",
      "name": "default",
      "agent_session_id": "__continue__",
      "history": [],
      "created_at": "2020-01-01T00:00:00Z",
      "updated_at": "2020-01-01T00:00:00Z"
    }
  },
  "active_session": {"user1": "s1"},
  "user_sessions": {"user1": ["s1"]},
  "counter": 1
}`
	if err := os.WriteFile(path, []byte(raw), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	sm := NewSessionManager(path)
	if got := sm.GetOrCreateActive("user1").GetAgentSessionID(); got != "" {
		t.Fatalf("GetAgentSessionID = %q, want empty", got)
	}
}

func TestSessionManager_Save_StripsContinueSentinel(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sessions.json")

	sm := NewSessionManager(path)
	sm.NewSession("user1", "persisted")
	s := sm.GetOrCreateActive("user1")
	s.mu.Lock()
	s.AgentSessionID = ContinueSession
	s.mu.Unlock()
	sm.Save()

	reloaded := NewSessionManager(path)
	if got := reloaded.GetOrCreateActive("user1").GetAgentSessionID(); got != "" {
		t.Fatalf("GetAgentSessionID after reload = %q, want empty", got)
	}
}

func TestKnownAgentSessionIDs(t *testing.T) {
	sm := NewSessionManager("")
	s1 := sm.NewSession("user1", "a")
	s1.SetAgentSessionID("uuid-aaa")
	s2 := sm.NewSession("user1", "b")
	s2.SetAgentSessionID("uuid-bbb")
	sm.NewSession("user1", "c") // no agent session id

	known := sm.KnownAgentSessionIDs()
	if len(known) != 2 {
		t.Fatalf("KnownAgentSessionIDs len = %d, want 2", len(known))
	}
	if _, ok := known["uuid-aaa"]; !ok {
		t.Fatal("expected uuid-aaa in known set")
	}
	if _, ok := known["uuid-bbb"]; !ok {
		t.Fatal("expected uuid-bbb in known set")
	}
}

func TestFilterOwnedSessions_FiltersUnknown(t *testing.T) {
	all := []AgentSessionInfo{
		{ID: "owned-1"},
		{ID: "external-1"},
		{ID: "owned-2"},
		{ID: "external-2"},
	}
	known := map[string]struct{}{
		"owned-1": {},
		"owned-2": {},
	}
	filtered := filterOwnedSessions(all, known)
	if len(filtered) != 2 {
		t.Fatalf("filterOwnedSessions len = %d, want 2", len(filtered))
	}
	if filtered[0].ID != "owned-1" || filtered[1].ID != "owned-2" {
		t.Fatalf("filtered = %v, want owned-1 and owned-2", filtered)
	}
}

func TestFilterOwnedSessions_EmptyKnownReturnsAll(t *testing.T) {
	all := []AgentSessionInfo{
		{ID: "session-1"},
		{ID: "session-2"},
	}
	filtered := filterOwnedSessions(all, map[string]struct{}{})
	if len(filtered) != 2 {
		t.Fatalf("filterOwnedSessions with empty known = %d, want 2", len(filtered))
	}
}
