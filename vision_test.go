package toroid

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/yashbonde/toroid-kernel/llm"
	"github.com/yashbonde/toroid-kernel/tools"
)

const (
	visionTestMain   = "llmgateway/test/text-only"
	visionTestVision = "llmgateway/test/vision"
)

// visionGateway is a fake OpenAI-compatible gateway. The main model asks for
// one screenshot tool call and then answers; the vision model describes the
// image (or fails, when visionStatus is set). Every request body is kept so a
// test can check what each model was sent.
type visionGateway struct {
	t            *testing.T
	visionStatus int // non-zero: the vision model answers with this HTTP status

	mu     sync.Mutex
	main   []string // raw request bodies sent to the main model
	vision []string // raw request bodies sent to the vision model
}

func (g *visionGateway) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost || !strings.HasSuffix(r.URL.Path, "/chat/completions") {
		http.NotFound(w, r) // model-limit lookups: unknown, so defaults apply
		return
	}
	body, _ := io.ReadAll(r.Body)
	var req struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		g.t.Errorf("bad request json: %v", err)
	}
	w.Header().Set("Content-Type", "application/json")

	g.mu.Lock()
	defer g.mu.Unlock()
	switch req.Model {
	case strings.TrimPrefix(visionTestVision, "llmgateway/"):
		g.vision = append(g.vision, string(body))
		if g.visionStatus != 0 {
			w.WriteHeader(g.visionStatus)
			_, _ = io.WriteString(w, `{"error":{"message":"vision model unavailable"}}`)
			return
		}
		_, _ = io.WriteString(w, `{"choices":[{"finish_reason":"stop","message":{"role":"assistant",
			"content":"SCREEN: Calendar, month view\nANSWER: the + button is at 430,800"}}],
			"usage":{"prompt_tokens":900,"completion_tokens":40,"cost":0.002}}`)
	case strings.TrimPrefix(visionTestMain, "llmgateway/"):
		g.main = append(g.main, string(body))
		if len(g.main) == 1 {
			_, _ = io.WriteString(w, `{"choices":[{"finish_reason":"tool_calls","message":{"role":"assistant",
				"content":"I will look for the add button.",
				"tool_calls":[{"id":"call_1","type":"function","function":{"name":"screenshot","arguments":"{\"question\":\"where is the + button?\"}"}}]}}],
				"usage":{"prompt_tokens":500,"completion_tokens":20,"cost":0.001}}`)
			return
		}
		_, _ = io.WriteString(w, `{"choices":[{"finish_reason":"stop","message":{"role":"assistant","content":"Tap 430,800."}}],
			"usage":{"prompt_tokens":700,"completion_tokens":5,"cost":0.001}}`)
	default:
		g.t.Errorf("unexpected model %q", req.Model)
		http.Error(w, "unknown model", http.StatusBadRequest)
	}
}

// newVisionTestKernel builds a kernel on the fake gateway with one tool that
// returns a JPEG and a question, and Vision pointed at the vision model.
func newVisionTestKernel(t *testing.T, g *visionGateway, imageOverride bool) *Kernel {
	t.Helper()
	srv := httptest.NewServer(g)
	t.Cleanup(srv.Close)
	t.Setenv(GatewayBaseURLEnv, srv.URL)

	reg := tools.NewRegistry()
	reg.Register(&tools.ToolDef{
		Name: "screenshot",
		Handler: llm.NewTool("screenshot", "Take a screenshot.", func(_ context.Context, a struct {
			Question string `json:"question"`
		}) (llm.ToolResult, error) {
			res := llm.NewTextResult("Screen 480x854 px.")
			res.Files = []llm.FilePart{{Filename: "screen.jpg", MediaType: "image/jpeg", Data: []byte("\xff\xd8fake-jpeg")}}
			res.Question = a.Question
			return res, nil
		}),
	})
	loadSkills := false
	k, err := NewKernel(context.Background(), Config{
		Model:      visionTestMain,
		APIKey:     "test-key",
		WorkDir:    t.TempDir(),
		LoadSkills: &loadSkills,
		Tools:      reg,
		// Even a host that says the main model takes images must not have them
		// sent to it while a vision delegate is set.
		ImageInputOverride: &imageOverride,
		Vision:             &VisionDelegate{Model: visionTestVision, Prompt: "Describe the screen with coordinates.", ReasoningEffort: "minimal"},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = k.Close() })
	return k
}

func TestVisionDelegateDescribesToolImages(t *testing.T) {
	g := &visionGateway{t: t}
	k := newVisionTestKernel(t, g, true)

	var turns []*TurnPayload
	var described []*VisionPayload
	k.On(EventTurnCompleted, func(_ context.Context, e Event) error {
		turns = append(turns, e.Payload.(*TurnPayload))
		return nil
	})
	k.On(EventVisionDescribed, func(_ context.Context, e Event) error {
		described = append(described, e.Payload.(*VisionPayload))
		return nil
	})

	answer, _, err := k.Run(context.Background(), "add an event in the calendar")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(answer, "Tap 430,800.") {
		t.Fatalf("answer = %q", answer)
	}

	if len(g.main) != 2 || len(g.vision) != 1 {
		t.Fatalf("main calls = %d, vision calls = %d; want 2 and 1", len(g.main), len(g.vision))
	}
	for i, body := range g.main {
		if strings.Contains(body, "image_url") || strings.Contains(body, "base64") {
			t.Fatalf("main request %d carries an image: %s", i, body)
		}
	}
	// The main model gets the vision model's answer, marked, in the tool result.
	if !strings.Contains(g.main[1], VisionMarker) || !strings.Contains(g.main[1], "the + button is at 430,800") {
		t.Fatalf("second main request lacks the vision description: %s", g.main[1])
	}

	// The vision model gets the image, its prompt, the tool call, the question,
	// the main model's stated intent and the user's request.
	v := g.vision[0]
	for _, want := range []string{
		"image_url", "data:image/jpeg;base64,", "Describe the screen with coordinates.",
		"Tool: screenshot", "Question: where is the + button?",
		"I will look for the add button.", "add an event in the calendar", "Screen 480x854 px.",
		`"reasoning_effort":"minimal"`,
	} {
		if !strings.Contains(v, want) {
			t.Errorf("vision request lacks %q: %s", want, v)
		}
	}
	// The system prompt prefix belongs to the main model; the vision call must
	// not carry (or change) it.
	if strings.Contains(v, k.SystemPromptPrefix[:40]) {
		t.Errorf("vision request carries the main system prefix")
	}

	// Cost: two main steps at $0.001 plus one vision call at $0.002.
	if got := k.RunningCostUSD(); !approx(got, 0.004) {
		t.Fatalf("RunningCostUSD = %v, want 0.004", got)
	}
	if len(described) != 1 || !approx(described[0].CostUSD, 0.002) || described[0].Model != visionTestVision || described[0].Error != "" {
		t.Fatalf("VisionDescribed = %+v", described)
	}
	if len(turns) != 2 {
		t.Fatalf("got %d TurnCompleted events", len(turns))
	}
	if !approx(turns[0].TurnCostUSD, 0.003) || turns[0].TurnUsage.Input != 1400 || turns[0].TurnUsage.Output != 60 || !turns[0].TurnUsage.PricingOK {
		t.Fatalf("tool turn usage = %+v cost %v; want the vision call included", turns[0].TurnUsage, turns[0].TurnCostUSD)
	}
	if !approx(turns[0].TotalCostUSD, 0.003) || !approx(turns[1].TotalCostUSD, 0.004) {
		t.Fatalf("running totals = %v, %v", turns[0].TotalCostUSD, turns[1].TotalCostUSD)
	}
	// The vision request is not the main context: it must not move the
	// window-occupancy gauge (last main step: 700 in + 5 out).
	if used, _ := k.ContextUsage(); used != 705 {
		t.Fatalf("context usage = %d, want 705 from the main model's last step", used)
	}

	// History keeps the description and no media.
	for _, m := range k.History {
		for _, p := range m.Parts {
			if tr, ok := p.(llm.ToolResultPart); ok {
				if len(tr.Files) != 0 || !strings.Contains(tr.Content, VisionMarker) {
					t.Fatalf("tool result in history = %+v", tr)
				}
			}
		}
	}
}

func TestVisionDelegateFailureIsReportedAsText(t *testing.T) {
	g := &visionGateway{t: t, visionStatus: http.StatusBadRequest}
	k := newVisionTestKernel(t, g, false)

	var described []*VisionPayload
	k.On(EventVisionDescribed, func(_ context.Context, e Event) error {
		described = append(described, e.Payload.(*VisionPayload))
		return nil
	})
	answer, _, err := k.Run(context.Background(), "look at the screen")
	if err != nil {
		t.Fatalf("a failed vision call must not fail the turn: %v", err)
	}
	if !strings.HasSuffix(answer, "Tap 430,800.") {
		t.Fatalf("answer = %q", answer)
	}
	if len(g.main) != 2 {
		t.Fatalf("main calls = %d", len(g.main))
	}
	if strings.Contains(g.main[1], "image_url") {
		t.Fatalf("the image fell back to the main model: %s", g.main[1])
	}
	if !strings.Contains(g.main[1], "the image could not be described") || !strings.Contains(g.main[1], "Screen 480x854 px.") {
		t.Fatalf("tool result lacks the failure note or the tool text: %s", g.main[1])
	}
	if len(described) != 1 || described[0].Error == "" {
		t.Fatalf("VisionDescribed = %+v; want one with Error", described)
	}
	if got := k.RunningCostUSD(); !approx(got, 0.002) {
		t.Fatalf("RunningCostUSD = %v, want only the two main steps", got)
	}
}

func TestNoVisionDelegateKeepsImagesOnMainModel(t *testing.T) {
	g := &visionGateway{t: t}
	k := newVisionTestKernel(t, g, true)
	k.Cfg.Vision = nil

	if _, _, err := k.Run(context.Background(), "look"); err != nil {
		t.Fatal(err)
	}
	if len(g.vision) != 0 {
		t.Fatalf("vision model called %d times without a delegate", len(g.vision))
	}
	if !strings.Contains(g.main[1], "image_url") {
		t.Fatalf("an image-capable main model should get the image without a delegate: %s", g.main[1])
	}
}

func approx(a, b float64) bool {
	d := a - b
	return d < 1e-12 && d > -1e-12
}
