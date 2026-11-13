package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yashbonde/toroid-kernel/tools"
)

// TestVisionDelegateDefault checks a new chat kernel hands screenshots
// to the default vision model (DeepSeek V4.1 Flash) with the screen prompt, uses the delegate's system prompt, and that
// turning the delegate off in settings restores the old behaviour.
func TestVisionDelegateDefault(t *testing.T) {
	srv := newServer(t.TempDir(), "tok", 0.5)
	srv.tools = newToolHost(filepath.Join(t.TempDir(), "none"))

	k, err := srv.newKernelWithKey(context.Background(), defaultModels[0].ID, nil, 0.5, "test-key")
	if err != nil {
		t.Fatal(err)
	}
	if k.Cfg.Vision == nil || k.Cfg.Vision.Model != "llmgateway/deepseek/deepseek-v4.1-flash" || k.Cfg.Vision.Prompt != screenVisionPrompt {
		t.Fatalf("vision delegate = %+v", k.Cfg.Vision)
	}
	if k.SystemPromptPrefix != chatSystemPrompt || !strings.Contains(chatSystemPrompt, "a separate vision model") {
		t.Fatal("delegate chat does not use the delegate system prompt")
	}
	_ = k.Close()

	if err := srv.settings.set(appSettings{VisionOff: true}); err != nil {
		t.Fatal(err)
	}
	k, err = srv.newKernelWithKey(context.Background(), defaultModels[0].ID, nil, 0.5, "test-key")
	if err != nil {
		t.Fatal(err)
	}
	defer k.Close()
	if k.Cfg.Vision != nil {
		t.Fatalf("delegate still set when off: %+v", k.Cfg.Vision)
	}
	if k.SystemPromptPrefix != chatSystemPromptNoDelegate || strings.Contains(k.SystemPromptPrefix, "a separate vision model") ||
		!strings.Contains(k.SystemPromptPrefix, "a screenshot on a\n  vision model") {
		t.Fatal("no-delegate chat should keep the original screen_look rules")
	}
}

// TestSettingsEndpoint checks GET reports the default, POST can turn the
// delegate off and pick another model, a bad or text-only id is refused, and
// the choice survives a restart.
func TestSettingsEndpoint(t *testing.T) {
	dir := t.TempDir()
	srv := newServer(dir, "tok", 0.5)
	srv.catalog.liveVision = map[string]bool{"typesafe/jev-router": true, "google/gemini-3.8-flash": true}
	ts := httptest.NewServer(srv.routes())
	defer ts.Close()

	call := func(method, body string) (int, settingsView) {
		req, _ := http.NewRequest(method, ts.URL+"/api/settings", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer tok")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var v settingsView
		b, _ := io.ReadAll(resp.Body)
		_ = json.Unmarshal(b, &v)
		return resp.StatusCode, v
	}

	code, v := call(http.MethodGet, "")
	if code != http.StatusOK || !v.VisionEnabled || v.VisionModel != defaultVisionModel || len(v.VisionModels) == 0 || v.VisionModels[0].ID != defaultVisionModel {
		t.Fatalf("GET = %d %+v", code, v)
	}
	if code, v = call(http.MethodPost, `{"vision_enabled":false}`); code != http.StatusOK || v.VisionEnabled {
		t.Fatalf("turn off = %d %+v", code, v)
	}
	if code, v = call(http.MethodPost, `{"vision_enabled":true,"vision_model":"google/gemini-3.8-flash"}`); code != http.StatusOK || !v.VisionEnabled || v.VisionModel != "llmgateway/google/gemini-3.8-flash" {
		t.Fatalf("pick model = %d %+v", code, v)
	}
	if code, _ = call(http.MethodPost, `{"vision_model":"deepseek/deepseek-v4.1-flash"}`); code != http.StatusBadRequest {
		t.Fatalf("text-only model accepted: %d", code)
	}
	if code, _ = call(http.MethodPost, `{"vision_model":"../../etc"}`); code != http.StatusBadRequest {
		t.Fatalf("bad id accepted: %d", code)
	}
	if got := newServer(dir, "tok", 0.5).settings.get().visionModel(); got != "llmgateway/google/gemini-3.8-flash" {
		t.Fatalf("after restart vision model = %q", got)
	}
	if code, v = call(http.MethodPost, `{"vision_model":""}`); code != http.StatusOK || v.VisionModel != defaultVisionModel {
		t.Fatalf("reset = %d %+v", code, v)
	}

	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/api/settings", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("no token: %d", resp.StatusCode)
	}
}

// TestScreenLookDelegateMode checks that with the delegate on, screen_look
// always asks for the grid, attaches the screenshot for the kernel to hand
// on, and carries the model's question.
func TestScreenLookDelegateMode(t *testing.T) {
	jpeg := base64.StdEncoding.EncodeToString([]byte("\xff\xd8 shot"))
	h := startFakeHost(t, func(op string, _ json.RawMessage) string {
		if op == "display_create" {
			return okLine(`{"display_id":1}`)
		}
		return okLine(`{"image_base64":"` + jpeg + `","width":480,"height":854,"elements":[],"grid":true}`)
	})
	b := &appsBridge{host: newToolHost(h.path), openWait: 20 * time.Millisecond}
	r := tools.NewRegistry()
	registerAppsTools(r, b, lookDelegate)
	runAppTool(t, r, "app_open", `{"app":"Calendar"}`)

	res, err := r.Execute(context.Background(), "screen_look", `{"question":"where is the start date field?"}`)
	if err != nil || res.IsError {
		t.Fatalf("screen_look: %+v %v", res, err)
	}
	if res.Question != "where is the start date field?" || len(res.Files) != 1 {
		t.Fatalf("question %q, %d files", res.Question, len(res.Files))
	}
	if !strings.Contains(res.Content, "vision model's description below") {
		t.Fatalf("content = %q", res.Content)
	}
	reqs := h.requests()
	if got := reqs[len(reqs)-1]; !strings.Contains(got, `"grid":true`) {
		t.Fatalf("forwarded %q, want the grid on", got)
	}
	tool, _ := r.Lookup("screen_look")
	if props, _ := tool.Handler.Parameters()["properties"].(map[string]any); props["question"] == nil || props["grid"] != nil {
		t.Fatalf("schema properties = %v", props)
	}
}
