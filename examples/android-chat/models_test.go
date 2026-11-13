package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestCatalogRefreshUpdatesPricesAndDropsMissing(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/models" {
			http.NotFound(w, r)
			return
		}
		w.Write([]byte(`{"data":[
			{"id":"deepseek/deepseek-v4.1-flash","pricing":{"prompt":"0.0000001","completion":"0.0000002","input_cache_read":"0.00000001"}},
			{"id":"anthropic/claude-sonnet-5.5","pricing":{"prompt":"0.00000003","completion":"0.00000013"}},
			{"id":"someone/else","pricing":{"prompt":"0.001","completion":"0.002"}}
		]}`))
	}))
	defer ts.Close()

	c := newCatalog()
	if err := c.refresh(context.Background(), ts.Client(), ts.URL+"/api/v1/"); err != nil {
		t.Fatal(err)
	}
	got := c.list()
	if len(got) != 2 {
		t.Fatalf("kept %d models, want the 2 present in the live catalog: %+v", len(got), got)
	}
	m, ok := c.lookup(gatewayPrefix + "deepseek/deepseek-v4.1-flash")
	if !ok || !near(m.InPerMTok, 0.1) || !near(m.OutPerMTok, 0.2) || !near(m.CachePerMTok, 0.01) || m.Tier != tierFlash {
		t.Fatalf("deepseek entry = %+v", m)
	}
}

func TestCatalogRefreshFailureKeepsDefaults(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"data":[]}`))
	}))
	defer ts.Close()
	c := newCatalog()
	if err := c.refresh(context.Background(), ts.Client(), ts.URL); err == nil {
		t.Fatal("empty catalog accepted")
	}
	if len(c.list()) != len(defaultModels) {
		t.Fatalf("defaults changed after a failed refresh")
	}
}

func TestRequireLoopback(t *testing.T) {
	for addr, ok := range map[string]bool{
		"127.0.0.1:18765": true,
		"localhost:1":     true,
		"[::1]:80":        true,
		"0.0.0.0:18765":   false,
		":18765":          false,
		"10.0.0.2:80":     false,
	} {
		if err := requireLoopback(addr); (err == nil) != ok {
			t.Errorf("requireLoopback(%q) err=%v, want ok=%v", addr, err, ok)
		}
	}
}

func near(a, b float64) bool { d := a - b; return d < 1e-9 && d > -1e-9 }

func TestCuratedModelsHaveTiersAndFlashDefault(t *testing.T) {
	if defaultModels[0].Tier != tierFlash {
		t.Fatalf("default model %s is %q; want a Flash model first", defaultModels[0].ID, defaultModels[0].Tier)
	}
	known := map[string]bool{}
	for _, ti := range tiers {
		known[ti.ID] = true
	}
	seen := map[string]bool{}
	perTier := map[string]int{}
	for _, m := range defaultModels {
		if !known[m.Tier] {
			t.Errorf("%s has unknown tier %q", m.ID, m.Tier)
		}
		if seen[m.ID] {
			t.Errorf("%s listed twice", m.ID)
		}
		seen[m.ID] = true
		perTier[m.Tier]++
	}
	for _, ti := range tiers {
		if perTier[ti.ID] == 0 {
			t.Errorf("tier %s has no models", ti.ID)
		}
	}
	// Only the newest release of a family is offered.
	for _, old := range []string{"deepseek/deepseek-v4-flash", "qwen/qwen3.7-flash", "moonshotai/kimi-k2-0905"} {
		if seen[gatewayPrefix+old] {
			t.Errorf("superseded model %s is still listed", old)
		}
	}
}
