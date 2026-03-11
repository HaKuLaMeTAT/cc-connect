package iflow

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestAvailableModels_FetchesFromAPI(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/models" {
			t.Fatalf("path = %s, want /models", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"id":"glm-5","owned_by":"iflow"},{"id":"qwen3-coder-plus","owned_by":"iflow"}]}`))
	}))
	defer srv.Close()

	t.Setenv("IFLOW_API_KEY", "test-key")
	t.Setenv("IFLOW_BASE_URL", srv.URL)

	a := &Agent{activeIdx: -1}
	models := a.AvailableModels(context.Background())
	if len(models) < 2 {
		t.Fatalf("models len = %d, want >= 2, models=%v", len(models), models)
	}
	if models[0].Name != "glm-5" || models[1].Name != "qwen3-coder-plus" {
		t.Fatalf("leading models = %v, want glm-5/qwen3-coder-plus", models[:2])
	}
}
