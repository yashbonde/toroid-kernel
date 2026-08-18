package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	toroid "github.com/yashbonde/toroid-kernel"
)

func TestRunModels(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("method = %s, want GET", r.Method)
		}
		if r.URL.Path != "/v1/models" {
			t.Errorf("path = %s, want /v1/models", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer gateway-secret" {
			t.Errorf("authorization = %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"object":"list","data":[{"id":"model-b","context_length":200000},{"id":"model-a"},{"id":""}]}`)
	}))
	defer server.Close()

	t.Setenv(toroid.GatewayBaseURLEnv, server.URL+"/v1/")
	t.Setenv(toroid.GatewayKeyEnv, "gateway-secret")

	var out strings.Builder
	if err := runModels(context.Background(), &out, nil); err != nil {
		t.Fatalf("runModels: %v", err)
	}
	if got, want := out.String(), "model-b (200000)\nmodel-a\n"; got != want {
		t.Fatalf("output = %q, want %q", got, want)
	}
}

func TestRunModelsRequiresGatewayEnvironment(t *testing.T) {
	t.Setenv(toroid.GatewayBaseURLEnv, "")
	t.Setenv(toroid.GatewayKeyEnv, "")

	err := runModels(context.Background(), io.Discard, nil)
	if err == nil || !strings.Contains(err.Error(), toroid.GatewayBaseURLEnv) {
		t.Fatalf("error = %v, want missing %s", err, toroid.GatewayBaseURLEnv)
	}
}

func TestFetchModelsReportsGatewayError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "invalid key", http.StatusUnauthorized)
	}))
	defer server.Close()

	_, err := fetchModels(context.Background(), server.Client(), server.URL, "bad-key")
	if err == nil || !strings.Contains(err.Error(), "401 Unauthorized: invalid key") {
		t.Fatalf("error = %v, want gateway status and message", err)
	}
}

func TestRunModelsRejectsArguments(t *testing.T) {
	t.Setenv(toroid.GatewayBaseURLEnv, "unused")
	t.Setenv(toroid.GatewayKeyEnv, "unused")

	err := runModels(context.Background(), io.Discard, []string{"extra"})
	if err == nil || err.Error() != "takes no arguments" {
		t.Fatalf("error = %v, want argument error", err)
	}
}
