package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"image"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"

	toroid "github.com/yashbonde/toroid-kernel"
	"github.com/yashbonde/toroid-kernel/llm"
)

func TestSplitSetupModel(t *testing.T) {
	for _, c := range []struct {
		in, id, tier string
		ok           bool
	}{
		{"@image-gen-agent", "@image-gen-agent", "", true},
		{"@image-gen-agent:frontier", "@image-gen-agent", "frontier", true},
		{"@image-gen-agent:flash", "@image-gen-agent", "flash", true},
		{"@image-gen-agent:huge", "", "", false},
		{"@", "", "", false},
		{"@Bad Name", "", "", false},
		{"@../x", "", "", false},
		{gatewayPrefix + "deepseek/deepseek-v4.1-flash", "", "", false},
	} {
		id, tier, ok := splitSetupModel(c.in)
		if id != c.id || tier != c.tier || ok != c.ok {
			t.Errorf("splitSetupModel(%q) = %q, %q, %v; want %q, %q, %v", c.in, id, tier, ok, c.id, c.tier, c.ok)
		}
	}
}

func TestSetupLLMModelFallbacks(t *testing.T) {
	a := agentSetup{FlashModel: "vendor/flash"}
	for _, tier := range []string{"", tierFlash, tierMid, tierFrontier} {
		if got := a.llmModel(tier); got != gatewayPrefix+"vendor/flash" {
			t.Errorf("only flash set: llmModel(%q) = %q", tier, got)
		}
	}
	a = agentSetup{FlashModel: gatewayPrefix + "vendor/flash", MidModel: "vendor/mid", FrontierModel: "vendor/top"}
	want := map[string]string{"": "vendor/mid", tierFlash: "vendor/flash", tierMid: "vendor/mid", tierFrontier: "vendor/top"}
	for tier, w := range want {
		if got := a.llmModel(tier); got != gatewayPrefix+w {
			t.Errorf("llmModel(%q) = %q, want %q", tier, got, gatewayPrefix+w)
		}
	}
	a.FrontierModel = ""
	if got := a.llmModel(tierFrontier); got != gatewayPrefix+"vendor/mid" {
		t.Errorf("empty frontier = %q, want mid", got)
	}
}

func TestAgentStoreSeedsAndKeepsEdits(t *testing.T) {
	path := filepath.Join(t.TempDir(), agentsDBName)
	st, err := openAgentStore(path)
	if err != nil {
		t.Fatal(err)
	}
	a, err := st.setup(defaultSetupID)
	if err != nil {
		t.Fatal(err)
	}
	if a.SystemPrompt != imageGenPrompt || a.ImageFinalModel != "openai/gpt-image-2.5-sunburst" || a.ImageDraftModel == "" ||
		strings.Join(a.Tools, ",") != "image_generate,image_recolour,palette_pick,image_look" || a.SpendCapUSD <= 0 {
		t.Fatalf("seeded setup = %+v", a)
	}
	if _, err := st.setup("@nope"); err != errNoSetup {
		t.Fatalf("missing setup: err = %v", err)
	}
	// An edit made in the database survives a restart at the same version...
	if _, err := st.db.Exec(`UPDATE agent_setups SET system_prompt = 'mine' WHERE id = ?`, defaultSetupID); err != nil {
		t.Fatal(err)
	}
	st.close()
	st, err = openAgentStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if a, _ := st.setup(defaultSetupID); a.SystemPrompt != "mine" {
		t.Fatalf("edit lost on reopen: %q", a.SystemPrompt)
	}
	// ...and a newer built-in version replaces it.
	if _, err := st.db.Exec(`UPDATE agent_setups SET version = 0 WHERE id = ?`, defaultSetupID); err != nil {
		t.Fatal(err)
	}
	st.close()
	st, err = openAgentStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.close()
	if a, _ := st.setup(defaultSetupID); a.SystemPrompt != imageGenPrompt {
		t.Fatal("older row not reseeded")
	}
}

func toolNames(k *toroid.Kernel) []string {
	var names []string
	for _, tl := range k.Cfg.Tools.Tools() {
		names = append(names, tl.Name)
	}
	sort.Strings(names)
	return names
}

// TestAtModelRouting: an "@" id builds the setup's agent, an unknown one is
// refused like an unknown model, and an ordinary model id builds the same
// chat kernel as before without ever opening agents.db.
func TestAtModelRouting(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("TOROID_HOME", dir)
	srv := newServer(dir, "tok", 0.5)

	plain, err := srv.newKernelWithKey(context.Background(), defaultModels[0].ID, nil, 0.5, "test-key")
	if err != nil {
		t.Fatal(err)
	}
	if plain.SystemPromptPrefix != chatSystemPrompt || plain.Cfg.Model != defaultModels[0].ID || plain.Cfg.Vision == nil {
		t.Fatalf("plain kernel changed: prefix ok=%v model=%s vision=%v", plain.SystemPromptPrefix == chatSystemPrompt, plain.Cfg.Model, plain.Cfg.Vision)
	}
	for _, n := range toolNames(plain) {
		if strings.HasPrefix(n, "image_") || n == "palette_pick" {
			t.Fatalf("plain kernel has %s", n)
		}
	}
	if !srv.knownModel(defaultModels[0].ID) || srv.capFor(defaultModels[0].ID) != 0.5 {
		t.Fatal("plain model routing changed")
	}
	if _, err := os.Stat(filepath.Join(dir, agentsDBName)); !os.IsNotExist(err) {
		t.Fatalf("agents.db opened for an ordinary model (stat err %v)", err)
	}

	k, err := srv.newKernelWithKey(context.Background(), defaultSetupID, nil, 0.5, "test-key")
	if err != nil {
		t.Fatal(err)
	}
	vm := srv.settings.get().visionModel()
	if k.SystemPromptPrefix != imageGenPrompt || (vm != "" && (k.Cfg.Vision == nil || k.Cfg.Vision.Model != vm || k.Cfg.Vision.Prompt != imageVisionPrompt)) {
		t.Fatalf("setup kernel prompt/vision wrong")
	}
	if k.Cfg.Model != strings.TrimPrefix(seedSetups[0].MidModel, gatewayPrefix) && k.Cfg.Model != seedSetups[0].MidModel {
		t.Fatalf("setup kernel model = %s, want the Mid model", k.Cfg.Model)
	}
	if got := strings.Join(toolNames(k), ","); got != "image_generate,image_look,image_recolour,palette_pick" {
		t.Fatalf("setup tools = %s", got)
	}
	if k2, _ := srv.newKernelWithKey(context.Background(), defaultSetupID+":frontier", nil, 0.5, "test-key"); k2 == nil || k2.Cfg.Model != seedSetups[0].FrontierModel {
		t.Fatal("frontier class not used")
	}
	if !srv.knownModel(defaultSetupID) || srv.knownModel("@nope") || srv.capFor(defaultSetupID) != seedSetups[0].SpendCapUSD {
		t.Fatal("setup routing wrong")
	}

	ts := httptest.NewServer(srv.routes())
	defer ts.Close()
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/chat", strings.NewReader(`{"session":"s1","model":"@nope","message":"hi"}`))
	req.Header.Set("Authorization", "Bearer tok")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest || !strings.Contains(string(b), "unknown model") {
		t.Fatalf("@nope: %d %s", resp.StatusCode, b)
	}
}

// imageStep calls image_generate on its first turn and answers on the next.
type imageStep struct{ turns int }

func (s *imageStep) msg() *toroid.AssistantMessage {
	usage := toroid.Usage{Input: 1000, Output: 50, Cost: 0.0001, PricingOK: true}
	if s.turns == 1 {
		return &toroid.AssistantMessage{
			Content: []llm.Part{llm.ToolCallPart{ID: "call_1", Name: "image_generate",
				Arguments: `{"prompt":"<scene>: a pine tree. <design>: pixel art. <colours>: navy.","quality":"draft"}`}},
			Usage: usage, StopReason: toroid.StopReasonToolUse,
		}
	}
	return &toroid.AssistantMessage{Content: []llm.Part{llm.TextPart{Text: "Here is a draft."}}, Usage: usage, StopReason: toroid.StopReasonStop}
}

func (s *imageStep) Complete(context.Context, toroid.Model, toroid.Context, toroid.StepOptions) (*toroid.AssistantMessage, error) {
	s.turns++
	return s.msg(), nil
}

func (s *imageStep) Stream(context.Context, toroid.Model, toroid.Context, toroid.StepOptions) (*toroid.StreamResult, error) {
	s.turns++
	m := s.msg()
	ch := make(chan toroid.StreamDelta, 1)
	ch <- toroid.StreamDelta{Text: llm.TextOf(m.Content)}
	close(ch)
	return &toroid.StreamResult{Deltas: ch, Final: func() (*toroid.AssistantMessage, error) { return m, nil }}, nil
}

func (s *imageStep) CompleteObject(context.Context, toroid.Model, toroid.Context, toroid.Schema, string, string, toroid.StepOptions) (*toroid.ObjectResult, error) {
	return nil, nil
}

func tinyPNG(t *testing.T) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 9, 16))
	for i := range img.Pix {
		img.Pix[i] = 0x40
	}
	var b bytes.Buffer
	if err := png.Encode(&b, img); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// TestImageGenerateThroughChat drives an "@image-gen-agent" chat whose model
// calls image_generate: the image request drops the parameter the model
// refuses, the image's cost counts toward the chat's spend, the client gets
// an "image" event, and the image is published and recorded.
func TestImageGenerateThroughChat(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("TOROID_HOME", dir)
	var mu sync.Mutex
	var bodies []map[string]any
	gw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/images" {
			http.NotFound(w, r)
			return
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		bodies = append(bodies, body)
		mu.Unlock()
		if _, ok := body["quality"]; ok {
			w.WriteHeader(http.StatusBadRequest)
			w.Write([]byte(`{"error":{"message":"FLUX 3 Image does not support: quality.","code":400}}`))
			return
		}
		json.NewEncoder(w).Encode(map[string]any{
			"data":  []any{map[string]string{"b64_json": base64.StdEncoding.EncodeToString(tinyPNG(t)), "media_type": "image/png"}},
			"usage": map[string]any{"cost": 0.009},
		})
	}))
	defer gw.Close()
	t.Setenv(toroid.GatewayBaseURLEnv, gw.URL)

	var published string
	h := startFakeHost(t, func(op string, args json.RawMessage) string {
		if op != "image_publish" {
			return `{"ok":false,"error":"unexpected op ` + op + `"}` + "\n"
		}
		var a struct{ Src, Name string }
		_ = json.Unmarshal(args, &a)
		if _, err := os.Stat(a.Src); err != nil {
			return `{"ok":false,"error":"no src"}` + "\n"
		}
		published = "/storage/emulated/0/Pictures/Toroid/" + a.Name
		return okLine(`{"path":"` + published + `"}`)
	})
	srv := newServer(dir, "tok", 0.5)
	srv.tools = newToolHost(h.path)
	sess := srv.sessions.get("s1")
	k, err := srv.newKernelWithKey(context.Background(), defaultSetupID, nil, 0.5, "test-key")
	if err != nil {
		t.Fatal(err)
	}
	k.Step = &imageStep{}
	sess.kernel, sess.model = k, defaultSetupID

	ts := httptest.NewServer(srv.routes())
	defer ts.Close()
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/chat", strings.NewReader(`{"session":"s1","model":"@image-gen-agent","message":"a pine tree wallpaper"}`))
	req.Header.Set("Authorization", "Bearer tok")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	var img imageResult
	var done doneEvent
	var tools []toolEvent
	for _, e := range parseSSE(t, string(raw)) {
		switch e.name {
		case "image":
			_ = json.Unmarshal([]byte(e.data), &img)
		case "done":
			_ = json.Unmarshal([]byte(e.data), &done)
		case "tool":
			var te toolEvent
			_ = json.Unmarshal([]byte(e.data), &te)
			tools = append(tools, te)
		case "error":
			t.Fatalf("error event: %s", e.data)
		}
	}
	if len(bodies) != 2 || bodies[1]["aspect_ratio"] != "9:16" || bodies[1]["model"] != seedSetups[0].ImageDraftModel {
		t.Fatalf("image requests = %v", bodies)
	}
	if img.Path != published || img.Quality != "draft" || img.CostUSD != 0.009 || img.Width != 9 || img.Height != 16 {
		t.Fatalf("image event = %+v (published %q)", img, published)
	}
	if !near(done.ReplyCostUSD, 0.0092) || done.SpendCapUSD != seedSetups[0].SpendCapUSD {
		t.Fatalf("done = %+v, want two LLM steps plus the $0.009 image", done)
	}
	if len(tools) != 2 || tools[1].Text != "✓ drew draft: "+img.File+" · $0.009" {
		t.Fatalf("tool events = %+v", tools)
	}
	if _, err := os.Stat(filepath.Join(dir, "images", img.File)); !os.IsNotExist(err) {
		t.Fatal("private copy kept after publishing")
	}
	st, _ := srv.agentStore()
	rec, err := st.findImage(img.ID)
	if err != nil || rec.Path != published || rec.ChatID != "s1" || rec.SetupID != defaultSetupID ||
		!strings.HasPrefix(rec.Prompt, "<scene>") || rec.Model != seedSetups[0].ImageDraftModel {
		t.Fatalf("record = %+v, %v", rec, err)
	}
}

// TestImageRefusedOverBudget: an image the chat cannot afford is not drawn.
func TestImageRefusedOverBudget(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("TOROID_HOME", dir)
	called := false
	gw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/images" { // building the kernel looks up the model's limits too
			called = true
		}
	}))
	defer gw.Close()
	t.Setenv(toroid.GatewayBaseURLEnv, gw.URL)
	srv := newServer(dir, "tok", 0.5)
	k, err := srv.newKernelWithKey(context.Background(), defaultSetupID, nil, 0.02, "test-key")
	if err != nil {
		t.Fatal(err)
	}
	ref := &kernelRef{}
	ref.set(k)
	a, _, _ := srv.setupFor(defaultSetupID)
	it := &imageTools{s: srv, setup: a, key: "k", ref: ref}
	if _, err := it.make(context.Background(), "image_generate", "p", "final", "", "9:16", nil, ""); err == nil || !strings.Contains(err.Error(), "budget") {
		t.Fatalf("final over a $0.02 budget: err = %v", err)
	}
	if called {
		t.Fatal("the images endpoint was called anyway")
	}
}

func TestUnsupportedParams(t *testing.T) {
	if got := unsupportedParams("Black Forest Labs FLUX 3 Image does not support: quality."); len(got) != 1 || got[0] != "quality" {
		t.Fatalf("got %q", got)
	}
	if got := unsupportedParams("x does not support: quality, n"); len(got) != 2 || got[1] != "n" {
		t.Fatalf("got %q", got)
	}
	if got := unsupportedParams("rate limited"); got != nil {
		t.Fatalf("got %q", got)
	}
}

func TestImageSummary(t *testing.T) {
	if s, _ := imageSummary("image_recolour", `{"file":"toroid-1.png","quality":"final","cost_usd":0.035}`); s != "✓ recoloured final: toroid-1.png · $0.035" {
		t.Fatalf("got %q", s)
	}
	if s := toolSummary("palette_pick", `{"roles":{"bg":"#000000","text":"#ffffff","accent":"#7cc4a4"},"contrast":21}`, ""); s != "✓ palette: bg #000000 · text #ffffff · accent #7cc4a4 (21.0:1)" {
		t.Fatalf("got %q", s)
	}
}

// TestImagePickedModelAndShape: a model and shape picked in the Gallery's
// options reach the images call, mapped to what the model accepts.
func TestImagePickedModelAndShape(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("TOROID_HOME", dir)
	var bodies []map[string]any
	gw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/images" {
			return
		}
		var b map[string]any
		_ = json.NewDecoder(r.Body).Decode(&b)
		bodies = append(bodies, b)
		http.Error(w, `{"error":{"message":"stop here"}}`, http.StatusInternalServerError)
	}))
	defer gw.Close()
	t.Setenv(toroid.GatewayBaseURLEnv, gw.URL)
	srv := newServer(dir, "tok", 0.5)
	k, err := srv.newKernelWithKey(context.Background(), defaultSetupID, nil, 0.5, "test-key")
	if err != nil {
		t.Fatal(err)
	}
	ref := &kernelRef{}
	ref.set(k)
	a, _, _ := srv.setupFor(defaultSetupID)
	it := &imageTools{s: srv, setup: a, key: "k", ref: ref}
	cases := []struct{ model, shape, wantModel, wantShape string }{
		{"", "9:16", a.ImageDraftModel, "9:16"},
		{"qwen/qwen-image-2.1-turbo", "1:1", "qwen/qwen-image-2.1-turbo", "1:1"},
		{"recraft/recraft-v4.1-flash", "2:3", "recraft/recraft-v4.1-flash", "3:4"},
	}
	for _, c := range cases {
		bodies = nil
		if _, err := it.make(context.Background(), "image_generate", "p", "draft", c.model, c.shape, nil, ""); err == nil {
			t.Fatalf("%s: want the gateway's error", c.model)
		}
		if len(bodies) != 1 || bodies[0]["model"] != c.wantModel || bodies[0]["aspect_ratio"] != c.wantShape {
			t.Fatalf("%s %s: bodies = %v", c.model, c.shape, bodies)
		}
	}
	if _, err := checkShape("16:9"); err == nil {
		t.Fatal("16:9 accepted")
	}
	if sh, err := checkShape(""); err != nil || sh != "9:16" {
		t.Fatalf("empty shape = %q, %v", sh, err)
	}
}
