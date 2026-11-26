package toroid

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestKernelSharesMetadataAcrossConcurrentStarts(t *testing.T) {
	var requests atomic.Int32
	started := make(chan string, 16)
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		started <- r.URL.Path
		select {
		case <-release:
		case <-r.Context().Done():
			return
		}
		if r.URL.Path == "/models" {
			fmt.Fprint(w, `{"data":[{"id":"test","pricing":{"input":1}}]}`)
		} else {
			fmt.Fprint(w, `{"max_input_tokens":100000,"max_output_tokens":512}`)
		}
	}))
	defer server.Close()
	t.Setenv(GatewayBaseURLEnv, server.URL)
	skills := false
	cfg := Config{WorkDir: t.TempDir(), APIKey: "first", IncludeSubagentTools: true, LoadSkills: &skills}
	WarmModelMetadata(cfg)
	// Neither cold request may depend on the other one's response.
	paths := map[string]bool{}
	for len(paths) < 2 {
		select {
		case path := <-started:
			paths[path] = true
		case <-time.After(time.Second):
			t.Fatal("discovery did not start both probes")
		}
	}
	if !paths["/models"] || !paths["/models/deepseek/deepseek-v4p1-flash"] {
		t.Fatalf("unexpected routes: %v", paths)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := NewKernel(ctx, cfg); err != context.Canceled {
		t.Fatalf("canceled kernel: %v", err)
	}
	results := make(chan error, 4)
	for range cap(results) {
		go func() {
			k, err := NewKernel(t.Context(), cfg)
			if err == nil {
				if k.Cfg.Model != DefaultModel || k.Cfg.MaxTokens != 512 || !strings.Contains(k.SystemPrompt, "Available gateway models") {
					err = fmt.Errorf("missing model metadata")
				}
				k.Close()
			}
			results <- err
		}()
	}
	close(release)
	for range cap(results) {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}
	check := func(want int32) {
		t.Helper()
		k, err := NewKernel(t.Context(), cfg)
		if err != nil {
			t.Fatal(err)
		}
		k.Close()
		if got := requests.Load(); got != want {
			t.Fatalf("gateway requests = %d, want %d", got, want)
		}
	}
	check(2)
	cfg.Model = "llmgateway/another"
	check(3) // new limits, same catalog
	cfg.APIKey = "second"
	check(5) // both probes are scoped to the provider credential
}

func TestMetadataExpiryAndRetry(t *testing.T) {
	var cache metadataCache[int]
	key := metadataKey{model: "test"}
	calls := 0
	fetch := func(ctx context.Context) (int, bool) { calls++; return calls, calls > 1 }
	for _, ttl := range []time.Duration{metadataRetry, metadataTTL} {
		start := time.Now()
		entry := cache.start(t.Context(), key, fetch)
		got := entry.wait(t.Context())
		if cache.start(t.Context(), key, fetch).wait(t.Context()) != got {
			t.Fatal("unexpected repeat lookup")
		}
		cache.mu.Lock()
		expires := entry.expires
		entry.expires = start.Add(-time.Second)
		cache.mu.Unlock()
		if expires.Before(start.Add(ttl)) || expires.After(time.Now().Add(ttl)) {
			t.Fatalf("incorrect expiry for result %d", got)
		}
	}
	if calls != 2 {
		t.Fatalf("expected a retry after expiry, got %d calls", calls)
	}
}

func TestInteractiveKernelCanStartBeforeDiscoveryCompletes(t *testing.T) {
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release
		fmt.Fprint(w, `{"max_output_tokens":512}`)
	}))
	defer server.Close()
	unblock := sync.OnceFunc(func() { close(release) })
	defer unblock()
	t.Setenv(GatewayBaseURLEnv, server.URL)
	cfg := Config{WorkDir: t.TempDir(), ModelMetadataWait: 20 * time.Millisecond}
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	k, err := NewKernel(ctx, cfg)
	if err != nil {
		t.Fatalf("interactive startup blocked on discovery: %v", err)
	}
	k.Close()
	unblock()
	cfg.ModelMetadataWait = 0
	k, err = NewKernel(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer k.Close()
	if k.Cfg.MaxTokens != 512 {
		t.Fatalf("subsequent kernel missed completed discovery: max tokens = %d", k.Cfg.MaxTokens)
	}
}
