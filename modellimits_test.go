package toroid

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestApplyModelLimits pins the clamping rules: a configured value that fits is
// kept, one that overshoots is lowered, an unset one adopts the model ceiling,
// and a limit the gateway did not report changes nothing.
func TestApplyModelLimits(t *testing.T) {
	flashAbl := modelLimits{MaxInputTokens: 262144, MaxOutputTokens: 8192}

	t.Run("unset knobs adopt the model ceilings", func(t *testing.T) {
		cfg := Config{}
		applyModelLimits(&cfg, flashAbl)
		if cfg.TotalContextSize != 262144 || cfg.MaxTokens != 8192 {
			t.Fatalf("got context=%d maxTokens=%d, want 262144/8192", cfg.TotalContextSize, cfg.MaxTokens)
		}
	})

	t.Run("oversized knobs are lowered to fit", func(t *testing.T) {
		cfg := Config{TotalContextSize: 900000, MaxTokens: 64000}
		applyModelLimits(&cfg, flashAbl)
		if cfg.TotalContextSize != 262144 || cfg.MaxTokens != 8192 {
			t.Fatalf("got context=%d maxTokens=%d, want 262144/8192", cfg.TotalContextSize, cfg.MaxTokens)
		}
	})

	t.Run("knobs that fit are honoured", func(t *testing.T) {
		cfg := Config{TotalContextSize: 24000, MaxTokens: 4096}
		applyModelLimits(&cfg, flashAbl)
		if cfg.TotalContextSize != 24000 || cfg.MaxTokens != 4096 {
			t.Fatalf("got context=%d maxTokens=%d, want 24000/4096", cfg.TotalContextSize, cfg.MaxTokens)
		}
	})

	t.Run("compaction buffer never swallows the window", func(t *testing.T) {
		cfg := Config{CompactionBufferSize: 50000}
		applyModelLimits(&cfg, modelLimits{MaxInputTokens: 32768})
		if cfg.CompactionBufferSize >= cfg.TotalContextSize {
			t.Fatalf("buffer %d must stay below context %d", cfg.CompactionBufferSize, cfg.TotalContextSize)
		}
	})

	t.Run("unreported limits leave config untouched", func(t *testing.T) {
		cfg := Config{TotalContextSize: 200000, MaxTokens: 4096}
		applyModelLimits(&cfg, modelLimits{})
		if cfg.TotalContextSize != 200000 || cfg.MaxTokens != 4096 {
			t.Fatalf("got context=%d maxTokens=%d, want them unchanged", cfg.TotalContextSize, cfg.MaxTokens)
		}
	})

	t.Run("max_tokens spelling is accepted for the output ceiling", func(t *testing.T) {
		cfg := Config{}
		applyModelLimits(&cfg, modelLimits{MaxTokens: 16384})
		if cfg.MaxTokens != 16384 {
			t.Fatalf("got maxTokens=%d, want 16384", cfg.MaxTokens)
		}
	})
}

// TestFetchModelLimits covers the probe against a stub gateway, including the
// failure modes that must leave the caller's configuration alone.
func TestFetchModelLimits(t *testing.T) {
	var gotPath, gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotAuth = r.URL.Path, r.Header.Get("Authorization")
		switch r.URL.Path {
		case "/v1/models/deepseek-v4-flash-abl":
			w.Write([]byte(`{"id":"deepseek-v4-flash-abl","max_input_tokens":262144,"max_output_tokens":8192}`))
		case "/v1/models/no-limits":
			w.Write([]byte(`{"id":"no-limits","owned_by":"butter"}`))
		default:
			http.Error(w, `{"error":"model not found"}`, http.StatusNotFound)
		}
	}))
	defer srv.Close()
	base := srv.URL + "/v1"

	lim, ok := fetchModelLimits(context.Background(), base, "sk-test", "llmgateway/deepseek-v4-flash-abl")
	if !ok || lim.MaxInputTokens != 262144 || lim.outputCeiling() != 8192 {
		t.Fatalf("got %+v ok=%v, want 262144/8192", lim, ok)
	}
	if gotPath != "/v1/models/deepseek-v4-flash-abl" {
		t.Errorf("gateway prefix must be stripped from the path, got %q", gotPath)
	}
	if gotAuth != "Bearer sk-test" {
		t.Errorf("got auth %q, want bearer token", gotAuth)
	}

	if _, ok := fetchModelLimits(context.Background(), base, "sk-test", "nope"); ok {
		t.Error("a 404 must report failure so the caller keeps its defaults")
	}
	if _, ok := fetchModelLimits(context.Background(), base, "sk-test", "no-limits"); ok {
		t.Error("a document with no usable limits must report failure")
	}
	if _, ok := fetchModelLimits(context.Background(), "", "sk-test", "x"); ok {
		t.Error("an empty base URL must report failure")
	}
}
