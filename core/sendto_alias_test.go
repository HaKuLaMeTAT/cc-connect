package core

import "testing"

func TestSendToCommandAliases_ShortFormsResolve(t *testing.T) {
	if got := matchPrefix("st", builtinCommands); got != "sendto" {
		t.Fatalf("matchPrefix(st) = %q, want sendto", got)
	}
	if got := matchPrefix("sd", builtinCommands); got != "sendto" {
		t.Fatalf("matchPrefix(sd) = %q, want sendto", got)
	}
	if got := matchPrefix("fwd", builtinCommands); got != "" {
		t.Fatalf("matchPrefix(fwd) = %q, want empty after alias removal", got)
	}
}

func TestExtractSendToArgs_SupportsShortAliases(t *testing.T) {
	tests := []struct {
		input string
		want  []string
	}{
		{input: "记录一下 /st @bz_iflow_bot", want: []string{"@bz_iflow_bot", "记录一下"}},
		{input: "继续 /sd @bz_iflow_bot", want: []string{"@bz_iflow_bot", "继续"}},
		{input: "/st @bz_iflow_bot continue", want: []string{"@bz_iflow_bot", "continue"}},
	}

	for _, tc := range tests {
		got, ok := extractSendToArgs(tc.input)
		if !ok {
			t.Fatalf("extractSendToArgs(%q) reported not found", tc.input)
		}
		if len(got) != len(tc.want) {
			t.Fatalf("extractSendToArgs(%q) len = %d, want %d (%v)", tc.input, len(got), len(tc.want), got)
		}
		for i := range tc.want {
			if got[i] != tc.want[i] {
				t.Fatalf("extractSendToArgs(%q)[%d] = %q, want %q", tc.input, i, got[i], tc.want[i])
			}
		}
	}
}

func TestExtractSendToArgs_DoesNotMatchRemovedFwdAlias(t *testing.T) {
	if _, ok := extractSendToArgs("/fwd @bz_iflow_bot continue"); ok {
		t.Fatal("removed /fwd alias should not be parsed as sendto")
	}
}
