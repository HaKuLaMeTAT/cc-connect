package feishu

import (
	"strings"
	"testing"
)

func TestSanitizingLoggerSanitize(t *testing.T) {
	logger := &sanitizingLogger{}
	input := "wss://example.com/ws?device_id=abc&ticket=def&conn_id=ghi&token=jkl&plain=ok"
	got := logger.sanitize(input)

	for _, secret := range []string{"abc", "def", "ghi", "jkl"} {
		if strings.Contains(got, secret) {
			t.Fatalf("sanitize(%q) leaked %q: %q", input, secret, got)
		}
	}
	for _, want := range []string{"device_id=***", "ticket=***", "conn_id=***", "token=***", "plain=ok"} {
		if !strings.Contains(got, want) {
			t.Fatalf("sanitize(%q) = %q, want contains %q", input, got, want)
		}
	}
}
