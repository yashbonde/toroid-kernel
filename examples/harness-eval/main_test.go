package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestParseArgs(t *testing.T) {
	tests := []struct {
		args          []string
		task, command string
		harnesses     []string
	}{
		{nil, "spend-limit", "run", nil},
		{[]string{"toroid", "pi"}, "spend-limit", "run", []string{"toroid", "pi"}},
		{[]string{"unicode-truncation", "claude"}, "unicode-truncation", "run", []string{"claude"}},
		{[]string{"recheck", "config-validation"}, "config-validation", "recheck", nil},
		{[]string{"recover", "markdown-media-paths", "toroid"}, "markdown-media-paths", "recover", []string{"toroid"}},
		{[]string{"rewrite", "spend-limit"}, "spend-limit", "rewrite", nil},
	}
	for _, tt := range tests {
		task, command, harnesses := parseArgs(tt.args)
		if task != tt.task || command != tt.command || !reflect.DeepEqual(harnesses, tt.harnesses) {
			t.Fatalf("parseArgs(%v) = %q, %q, %v", tt.args, task, command, harnesses)
		}
	}
}

func TestChooseNewBranchIgnoresUnchangedOldRuns(t *testing.T) {
	base := "bench/toroid/task"
	before := map[string]string{base: "a", base + "-2": "b"}
	refs := map[string]string{base: "a", base + "-2": "b"}
	if branch, sha := chooseNewBranch(base, refs, before); sha != "" || branch != base {
		t.Fatalf("unchanged refs scored as new: %q %q", branch, sha)
	}
	refs[base+"-3"] = "c"
	refs[base+"-junk"] = "ignored"
	if branch, sha := chooseNewBranch(base, refs, before); branch != base+"-3" || sha != "c" {
		t.Fatalf("new branch = %q %q", branch, sha)
	}
}

func TestAllPathsMatched(t *testing.T) {
	changed := []string{"kernel.go", "tools/edit.go", "tools/edit_test.go"}
	if !allPathsMatched(changed, []string{"kernel.go", "tools"}) {
		t.Fatal("expected exact file and directory requirements to match")
	}
	if allPathsMatched(changed, []string{"llm"}) {
		t.Fatal("unmodified directory matched")
	}
}

func TestCopyTree(t *testing.T) {
	src, dst := t.TempDir(), t.TempDir()
	path := filepath.Join(src, "tools", "hidden_test.go")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("package tools\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := copyTree(src, dst); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(dst, "tools", "hidden_test.go"))
	if err != nil || string(b) != "package tools\n" {
		t.Fatalf("copied file = %q, %v", b, err)
	}
}

func TestTraceProxyCapturesBodiesAndRedactsSecrets(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages" {
			t.Fatalf("upstream path = %q", r.URL.Path)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: done\n\n")
	}))
	defer upstream.Close()

	dir := t.TempDir()
	proxyURL, stop, err := startTraceProxy(upstream.URL, dir, "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	req, _ := http.NewRequest(http.MethodPost, proxyURL+"/v1/messages", strings.NewReader(`{"system":"secret prompt"}`))
	req.Header.Set("Authorization", "Bearer secret")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.ReadAll(resp.Body)
	_ = resp.Body.Close()

	requestBody, err := os.ReadFile(filepath.Join(dir, "0001-request.body"))
	if err != nil || string(requestBody) != `{"system":"secret prompt"}` {
		t.Fatalf("request body = %q, %v", requestBody, err)
	}
	responseBody, err := os.ReadFile(filepath.Join(dir, "0001-response.body"))
	if err != nil || string(responseBody) != "data: done\n\n" {
		t.Fatalf("response body = %q, %v", responseBody, err)
	}
	meta, err := os.ReadFile(filepath.Join(dir, "0001-request-meta.json"))
	if err != nil || strings.Contains(string(meta), "Bearer secret") || !strings.Contains(string(meta), "[REDACTED]") {
		t.Fatalf("request metadata was not redacted: %s (%v)", meta, err)
	}
}
