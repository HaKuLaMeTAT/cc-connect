package core

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestSplitMessage_PreservesUTF8(t *testing.T) {
	text := "第一行🙂\n第二行你好\n第三行abc"
	chunks := splitMessage(text, 4)
	if len(chunks) < 2 {
		t.Fatalf("expected multiple chunks, got %d", len(chunks))
	}

	var rebuilt strings.Builder
	for i, chunk := range chunks {
		if !utf8.ValidString(chunk) {
			t.Fatalf("chunk %d is not valid UTF-8: %q", i, chunk)
		}
		if len([]rune(chunk)) > 4 {
			t.Fatalf("chunk %d has %d runes, want <= 4", i, len([]rune(chunk)))
		}
		rebuilt.WriteString(chunk)
	}

	if rebuilt.String() != text {
		t.Fatalf("rebuilt text mismatch:\n got: %q\nwant: %q", rebuilt.String(), text)
	}
}

func TestRuneSafeTruncationHelpers(t *testing.T) {
	src := "中文🙂abc"

	if got := truncateStr(src, 3); got != "中文🙂..." {
		t.Fatalf("truncateStr = %q, want %q", got, "中文🙂...")
	}
	if got := truncateRelay(src, 3); got != "中文🙂…" {
		t.Fatalf("truncateRelay = %q, want %q", got, "中文🙂…")
	}
}
