package main

import (
	"context"
	"encoding/json"
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
		io.WriteString(w, `{"object":"list","data":[{"id":"model-b","context_length":200000,"pricing":{"input":1400000,"output":4400000}},{"id":"model-a"},{"id":""}]}`)
	}))
	defer server.Close()

	t.Setenv(toroid.GatewayBaseURLEnv, server.URL+"/v1/")
	t.Setenv(toroid.GatewayKeyEnv, "gateway-secret")

	var out strings.Builder
	if err := runModels(context.Background(), &out, nil); err != nil {
		t.Fatalf("runModels: %v", err)
	}
	got := out.String()
	if !strings.HasPrefix(got, "\x1b[90mGET "+server.URL+"/v1/models\x1b[0m\n") {
		t.Fatalf("output = %q, want grey GET line prefix", got)
	}
	if want := "model-b (200000)  \x1b[90m$1.40 / $4.40 per MTok\x1b[0m\nmodel-a\n"; !strings.HasSuffix(got, want) {
		t.Fatalf("output = %q, want suffix %q", got, want)
	}
}

func TestPriceSuffix(t *testing.T) {
	cases := []struct {
		name string
		p    *modelPricing
		want string
	}{
		{"nil", nil, ""},
		{"zero", &modelPricing{}, "$0.00 / $0.00 per MTok"},
		{"values", &modelPricing{Input: 220000, Output: 660000, CacheRead: 7000, CacheWrite: 0}, "$0.22 / $0.66 per MTok"},
	}
	for _, c := range cases {
		if got := priceSuffix(c.p); got != c.want {
			t.Errorf("%s: priceSuffix = %q, want %q", c.name, got, c.want)
		}
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

func TestAssistantTextFromTurn(t *testing.T) {
	p := &toroid.TurnPayload{Content: json.RawMessage(
		`[{"role":"assistant","parts":[{"kind":"text","text":"I'll list the files."},{"kind":"tool_call","id":"c1","name":"bash","arguments":"{}"}]},` +
			`{"role":"tool","parts":[{"kind":"tool_result","tool_call_id":"c1","content":"..."}]}]`)}
	if got, want := assistantTextFromTurn(p), "I'll list the files."; got != want {
		t.Fatalf("assistantTextFromTurn = %q, want %q", got, want)
	}
	if got := assistantTextFromTurn(&toroid.TurnPayload{}); got != "" {
		t.Fatalf("empty payload = %q, want empty", got)
	}
}
