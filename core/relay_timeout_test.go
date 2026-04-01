package core

import "testing"

func TestRelayTimeoutForProject_UsesExtendedTimeoutForGemini(t *testing.T) {
	if got := relayTimeoutForProject("gemini"); got != geminiRelayTimeout {
		t.Fatalf("relayTimeoutForProject(gemini) = %v, want %v", got, geminiRelayTimeout)
	}
	if got := relayTimeoutForProject("GeMiNi"); got != geminiRelayTimeout {
		t.Fatalf("relayTimeoutForProject(GeMiNi) = %v, want %v", got, geminiRelayTimeout)
	}
	if got := relayTimeoutForProject("codex"); got != codexRelayTimeout {
		t.Fatalf("relayTimeoutForProject(codex) = %v, want %v", got, codexRelayTimeout)
	}
	if got := relayTimeoutForProject("CoDeX"); got != codexRelayTimeout {
		t.Fatalf("relayTimeoutForProject(CoDeX) = %v, want %v", got, codexRelayTimeout)
	}
	if got := relayTimeoutForProject("iflow"); got != relayTimeout {
		t.Fatalf("relayTimeoutForProject(iflow) = %v, want %v", got, relayTimeout)
	}
	if geminiRelayTimeout <= relayTimeout {
		t.Fatalf("geminiRelayTimeout = %v, want > relayTimeout %v", geminiRelayTimeout, relayTimeout)
	}
	if codexRelayTimeout <= relayTimeout {
		t.Fatalf("codexRelayTimeout = %v, want > relayTimeout %v", codexRelayTimeout, relayTimeout)
	}
}
