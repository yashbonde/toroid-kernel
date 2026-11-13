package toroid

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/yashbonde/toroid-kernel/llm"
)

// VisionDelegate hands every image a tool returns to a separate vision model,
// so the main model plans and acts on text alone. Set it in Config.Vision.
//
// When it is set, tool-result media never reaches the main model, whatever
// that model accepts. Each tool result that carries images is sent to Model in
// one non-streaming call, together with the tool's name, arguments and text,
// the main model's latest assistant text (its current intent), the user's
// latest request, and any question the tool result carries
// (llm.ToolResult.Question). The vision model's answer replaces the images in
// the tool result, after a VisionMarker line. A failed vision call leaves the
// tool's own text plus a note saying the image could not be described; the
// turn carries on.
//
// The vision call's tokens and cost are added to the turn's usage and the
// session's running cost, so spend limits include it. The stable system prefix
// is never touched, so the main model's prompt cache stays warm.
type VisionDelegate struct {
	// Model is the vision model id, in the same provider/model form as
	// Config.Model, e.g. "llmgateway/typesafe/jev-router".
	Model string `json:"model"`
	// Prompt is the vision model's system prompt. Empty uses a generic prompt
	// (DefaultVisionPrompt) that asks for a compact, factual description.
	Prompt string `json:"prompt,omitempty"`
	// MaxTokens caps the vision model's output per call. 0 uses
	// DefaultVisionMaxTokens.
	MaxTokens int `json:"max_tokens,omitempty"`
	// ReasoningEffort is sent to the vision model as reasoning_effort, e.g.
	// "minimal" or "low", which keeps a reasoning model (or a router that
	// picks one) quick and cheap. Empty lets the model choose.
	ReasoningEffort string `json:"reasoning_effort,omitempty"`
}

// DefaultVisionMaxTokens is the vision call's output cap when
// VisionDelegate.MaxTokens is 0. It leaves room for a router that picks a
// reasoning model.
const DefaultVisionMaxTokens = 2048

// VisionMarker starts the vision model's description inside a tool result, so
// the main model (and anyone reading the history) can tell it apart from the
// tool's own output.
const VisionMarker = "[image described by vision model"

// DefaultVisionPrompt is the vision model's system prompt when
// VisionDelegate.Prompt is empty.
const DefaultVisionPrompt = `You are the eyes of an AI agent that cannot see images. A tool the agent called returned the image(s) attached below. Describe them for the agent so it can plan its next action from your text alone.
- If a question is given, answer it first, directly.
- Then describe what matters for the agent's goal: the layout, every visible piece of text (transcribed exactly), controls and their state.
- Give positions in the image's own pixel coordinates when they help the agent act.
- Be compact and factual. Say plainly when something is unclear, cut off or not visible; never guess.`

// visionRequest is everything one vision call needs about the tool result.
type visionRequest struct {
	tool     string
	args     string
	toolText string
	question string
	intent   string // the main model's latest assistant text
	request  string // the user's latest message
	files    []llm.FilePart
}

// visionOutcome is one vision call's result, kept per tool call so runToolCalls
// can bill and report it in call order after the parallel wave finishes.
type visionOutcome struct {
	model string
	usage Usage
	err   error
}

// visionEnabled reports whether tool-result media goes to a vision delegate.
func (k *Kernel) visionEnabled() bool {
	return k.Cfg.Vision != nil && strings.TrimSpace(k.Cfg.Vision.Model) != ""
}

// visionStepFor returns the Step that reaches the vision model: the kernel's
// own Step when both models use the same wire (so a test's FauxStep, or the
// gateway client, serves both), else a Step built for the vision model's route.
func (k *Kernel) visionStepFor() Step {
	if k.visionStep != nil {
		return k.visionStep
	}
	return k.Step
}

// routeOf names the wire a model id uses: "openai", "anthropic", or "gateway".
func routeOf(model string) string {
	switch {
	case strings.HasPrefix(model, "openai/"):
		return "openai"
	case strings.HasPrefix(model, "anthropic/"):
		return "anthropic"
	default:
		return "gateway"
	}
}

// newVisionStep builds a Step for a vision model whose route differs from the
// main model's, with that route's own credential from the environment (the
// main model's Config.APIKey belongs to a different provider).
func newVisionStep(model string) Step {
	switch routeOf(model) {
	case "openai":
		return NewGatewayStep(llm.NewClient(OpenAIBaseURL, os.Getenv(OpenAIKeyEnv)))
	case "anthropic":
		return NewGatewayStep(llm.NewAnthropicClient(os.Getenv(AnthropicKeyEnv)))
	default:
		return NewGatewayStep(llm.NewClient(os.Getenv(GatewayBaseURLEnv), os.Getenv(GatewayKeyEnv)))
	}
}

// describeMedia sends one tool result's media to the vision model and returns
// its text answer. metadata is the turn's; the vision call gets its own
// llm-step id within that turn.
func (k *Kernel) describeMedia(ctx context.Context, metadata llm.RequestMetadata, r visionRequest) (string, visionOutcome) {
	vd := k.Cfg.Vision
	out := visionOutcome{model: vd.Model}
	prompt := vd.Prompt
	if strings.TrimSpace(prompt) == "" {
		prompt = DefaultVisionPrompt
	}
	maxTokens := int64(vd.MaxTokens)
	if maxTokens <= 0 {
		maxTokens = DefaultVisionMaxTokens
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "Tool: %s\n", r.tool)
	if a := strings.TrimSpace(r.args); a != "" && a != "{}" && a != "null" {
		fmt.Fprintf(&sb, "Arguments: %s\n", clip(a, 1000))
	}
	if q := strings.TrimSpace(r.question); q != "" {
		fmt.Fprintf(&sb, "Question: %s\n", clip(q, 1000))
	}
	if s := strings.TrimSpace(r.request); s != "" {
		fmt.Fprintf(&sb, "\nThe user's latest request:\n%s\n", clip(s, 1500))
	}
	if s := strings.TrimSpace(r.intent); s != "" {
		fmt.Fprintf(&sb, "\nThe agent's current plan (its latest message):\n%s\n", clipTail(s, 1500))
	}
	if s := strings.TrimSpace(r.toolText); s != "" {
		fmt.Fprintf(&sb, "\nThe tool's own text output:\n%s\n", clip(s, 4000))
	}
	parts := []llm.Part{llm.TextPart{Text: sb.String()}}
	for _, f := range r.files {
		parts = append(parts, f)
	}

	md := metadata
	md.TraceID = NewSessionID() // its own llm-step within the turn
	md.ParentSessionID = ""
	res, err := k.visionStepFor().Complete(ctx, ResolveModel(vd.Model), Context{
		System:   prompt,
		Messages: []llm.Message{{Role: llm.RoleUser, Parts: parts}},
		Metadata: md,
	}, StepOptions{MaxOutputTokens: &maxTokens, DisablePromptCache: true, ReasoningEffort: vd.ReasoningEffort})
	if err != nil {
		out.err = err
		return "", out
	}
	out.usage = res.Usage
	text := strings.TrimSpace(res.Text())
	if text == "" {
		out.err = errors.New("the vision model returned no text")
		if res.StopReason == StopReasonLength {
			out.err = errors.New("the vision model ran out of output tokens before answering")
		}
		return "", out
	}
	return text, out
}

// withVisionText appends the vision model's answer (or its failure) to a tool
// result's own text.
func withVisionText(toolText, model, description string, err error) string {
	var sb strings.Builder
	sb.WriteString(toolText)
	if toolText != "" && !strings.HasSuffix(toolText, "\n") {
		sb.WriteString("\n")
	}
	if err != nil {
		fmt.Fprintf(&sb, "\n[the image could not be described: vision model %s failed: %s. The image was not sent to you; rely on the text above, or look again.]", model, oneLineErr(err))
		return sb.String()
	}
	fmt.Fprintf(&sb, "\n%s %s; the image itself was not sent to you]\n%s", VisionMarker, model, description)
	return sb.String()
}

// latestIntent is the main model's most recent stated intent: this turn's
// assistant text, else its reasoning, else the last non-empty assistant text
// in history.
func latestIntent(turn []llm.Part, history []llm.Message) string {
	if t := strings.TrimSpace(llm.TextOf(turn)); t != "" {
		return t
	}
	for _, p := range turn {
		if rp, ok := p.(llm.ReasoningPart); ok && strings.TrimSpace(rp.Text) != "" {
			return strings.TrimSpace(rp.Text)
		}
	}
	for i := len(history) - 1; i >= 0; i-- {
		if history[i].Role == llm.RoleAssistant {
			if t := strings.TrimSpace(history[i].Text()); t != "" {
				return t
			}
		}
	}
	return ""
}

// latestUserRequest is the text of the most recent user message in history.
func latestUserRequest(history []llm.Message) string {
	for i := len(history) - 1; i >= 0; i-- {
		if history[i].Role == llm.RoleUser {
			if t := strings.TrimSpace(history[i].Text()); t != "" {
				return t
			}
		}
	}
	return ""
}

// addUsage sums two usages; the sum is priced only when both are.
func addUsage(a, b Usage) Usage {
	return Usage{
		Input:      a.Input + b.Input,
		Output:     a.Output + b.Output,
		Reasoning:  a.Reasoning + b.Reasoning,
		CacheRead:  a.CacheRead + b.CacheRead,
		CacheWrite: a.CacheWrite + b.CacheWrite,
		Cost:       a.Cost + b.Cost,
		PricingOK:  a.PricingOK && b.PricingOK,
	}
}

func clip(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

// clipTail keeps the end of s: the latest part of a plan is the most relevant.
func clipTail(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return "…" + string(r[len(r)-n:])
}

func oneLineErr(err error) string {
	return clip(strings.Join(strings.Fields(err.Error()), " "), 300)
}
