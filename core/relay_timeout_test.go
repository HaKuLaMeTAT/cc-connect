package core

import "testing"

func TestRelayTimeoutForProject_UsesExtendedTimeoutForGemini(t *testing.T) {
	if got := relayTimeoutForProject("gemini"); got != geminiRelayTimeout {
		t.Fatalf("relayTimeoutForProject(gemini) = %v, want %v", got, geminiRelayTimeout)
	}
	if got := relayTimeoutForProject("GeMiNi"); got != geminiRelayTimeout {
		t.Fatalf("relayTimeoutForProject(GeMiNi) = %v, want %v", got, geminiRelayTimeout)
	}
	if got := relayTimeoutForProject("codex"); got != relayTimeout {
		t.Fatalf("relayTimeoutForProject(codex) = %v, want %v", got, relayTimeout)
	}
	if geminiRelayTimeout <= relayTimeout {
		t.Fatalf("geminiRelayTimeout = %v, want > relayTimeout %v", geminiRelayTimeout, relayTimeout)
	}
}
