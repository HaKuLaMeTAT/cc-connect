package codex

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
)

func TestAvailableModels_FallbackToModelsCache(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "forbidden", http.StatusForbidden)
	}))
	defer srv.Close()

	tmp := t.TempDir()
	cache := `{
  "models": [
    {"slug":"gpt-5.4","description":"Latest frontier agentic coding model.","visibility":"list","supported_in_api":true},
    {"slug":"gpt-5.3-codex","description":"Great for coding","visibility":"list","supported_in_api":true},
    {"slug":"gpt-5","description":"Hidden but supported","visibility":"hide","supported_in_api":true},
    {"slug":"hidden-internal","visibility":"hidden","supported_in_api":true},
    {"slug":"tool-only","visibility":"list","supported_in_api":false}
  ]
}`
	if err := os.WriteFile(tmp+"/models_cache.json", []byte(cache), 0o600); err != nil {
		t.Fatalf("write models_cache.json: %v", err)
	}

	t.Setenv("CODEX_HOME", tmp)
	t.Setenv("OPENAI_API_KEY", "test-key")
	t.Setenv("OPENAI_BASE_URL", srv.URL)

	a := &Agent{activeIdx: -1}
	models := a.AvailableModels(context.Background())

	names := make(map[string]struct{}, len(models))
	for _, m := range models {
		names[m.Name] = struct{}{}
	}
	for _, want := range []string{"gpt-5", "gpt-5.3-codex", "gpt-5.4"} {
		if _, ok := names[want]; !ok {
			t.Fatalf("models missing %q: %v", want, models)
		}
	}
}
