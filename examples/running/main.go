// Pattern: RUNNING A KERNEL — the one example to read first.
//
// This single program walks through every core usage pattern:
//
//  1. BLOCKING vs STREAMING — Run returns the full text at once (plus a
//     UsagePayload); Stream writes it to an io.Writer as it is produced.
//     Both drive the same tool-calling loop underneath.
//  2. EVENTS — the synchronous event bus (On) is the integration surface:
//     tool calls, tool results, per-turn cost, subagent lifecycle.
//  3. GUARDRAILS — MaxIter (absolute step cap) and MaxRepeatCalls (a "spin"
//     guard keyed on call + result) stop runaway loops without killing
//     legitimate polling.
//  4. DELEGATION — synchronous subagents (the `subagent` tool / RunSubagent)
//     and asynchronous background agents (SpawnBackground / `subagent_async`)
//     that wake an idle kernel on completion.
//  5. OBSERVABILITY — with Save:true every span/cost/event lands in the
//     SQLite store; OTELSpans exports the persisted trace as spec-valid
//     OpenTelemetry spans for any OTLP backend.
//  6. MULTIMODAL — images inline in a prompt (![](path)) and via the read
//     tool returning media bytes a vision model can see.
//  7. STRUCTURED OUTPUT — WithSchema runs the full agentic loop (tool calls
//     included) and then coerces the result into a JSON object matching the
//     schema. No-network mode shows the loop-guard + structured path with a
//     scripted FauxStep.
//
//     export LLM_GATEWAY_BASE_URL=... LLM_GATEWAY_KEY=...
//     go run ./examples/running
//
// To exercise the no-network guardrails scenarios without an API key:
//
//     go run ./examples/running --guardrails
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"

	toroid "github.com/yashbonde/toroid-kernel"
	"github.com/yashbonde/toroid-kernel/llm"
	tools "github.com/yashbonde/toroid-kernel/tools"
)

func main() {
	guardrailsOnly := flag.Bool("guardrails", false, "run the no-network loop-guard scenarios (scripted FauxStep), then exit")
	flag.Parse()
	ctx := context.Background()

	if *guardrailsOnly {
		runGuardrails(ctx)
		return
	}

	// Prefer llmgateway when configured, fall back to Anthropic.
	apiKey := os.Getenv("LLM_GATEWAY_KEY")
	model := "llmgateway/claude-haiku-4-5"
	if apiKey == "" {
		fmt.Println("set LLM_GATEWAY_KEY to run this example")
		return
	}
	// Allow overriding the model via env to run the example across providers.
	if m := os.Getenv("TOROID_MODEL"); m != "" {
		model = m
	}

	k, err := toroid.NewKernel(ctx, toroid.Config{
		Model:                model,
		APIKey:               apiKey,
		WorkDir:              ".",
		IncludeComputerTools: true,
		IncludeSubagentTools: true, // expose `subagent` + `subagent_async`
		Save:                 true, // persist the trace so OTELSpans works below
		MaxIter:              100,  // absolute step cap
		MaxRepeatCalls:       3,    // spin guard: stop after 3 identical call+result pairs
	})
	if err != nil {
		panic(err)
	}
	defer k.Close()

	// GetUser tool: fetches a random user profile from randomuser.me
	type GetUserArgs struct {
		Gender   bool `json:"gender,omitempty"`
		Name     bool `json:"name,omitempty"`
		Location bool `json:"location,omitempty"`
		Email    bool `json:"email,omitempty"`
		Dob      bool `json:"dob,omitempty"`
		Cell     bool `json:"cell,omitempty"`
	}

	k.Tools.Register(&tools.ToolDef{
		Name:        "get_user",
		Description: "Fetch the current user profile from randomuser.me",
		Handler: llm.NewTool(
			"get_user",
			"Fetch the current user profile from randomuser.me",
			func(ctx context.Context, args GetUserArgs) (llm.ToolResult, error) {
				var fields []string
				if args.Gender {
					fields = append(fields, "gender")
				}
				if args.Name {
					fields = append(fields, "name")
				}
				if args.Location {
					fields = append(fields, "location")
				}
				if args.Email {
					fields = append(fields, "email")
				}
				if args.Dob {
					fields = append(fields, "dob")
				}
				if args.Cell {
					fields = append(fields, "cell")
				}
				url := "https://randomuser.me/api/"
				if len(fields) > 0 {
					url += "?inc=" + strings.Join(fields, ",")
				}
				resp, err := http.Get(url)
				if err != nil {
					return llm.ToolResult{}, err
				}
				defer resp.Body.Close()
				body, err := io.ReadAll(resp.Body)
				if err != nil {
					return llm.ToolResult{}, err
				}
				return llm.NewTextResult(string(body)), nil
			}),
	})

	// --- EVENTS: observe the lifecycle — tool calls, results, cost, subagents. ---
	k.On(toroid.EventPreToolUse, func(_ context.Context, e toroid.Event) error {
		if p, ok := e.Payload.(*toroid.ToolUsePayload); ok {
			fmt.Printf("→ tool %s args=%s\n", p.Name, p.Args)
		}
		return nil
	})
	k.On(toroid.EventPostToolUse, func(_ context.Context, e toroid.Event) error {
		if p, ok := e.Payload.(*toroid.ToolUseResultPayload); ok {
			res := strings.TrimSpace(p.Result)
			if len(res) > 80 {
				res = res[:80]
			}
			fmt.Printf("← tool %s done -> %s\n", p.Name, res)
		}
		return nil
	})
	k.On(toroid.EventTurnCompleted, func(_ context.Context, e toroid.Event) error {
		if p, ok := e.Payload.(*toroid.TurnPayload); ok {
			fmt.Printf("$ turn=$%.6f total=$%.6f\n", p.TurnCostUSD, p.TotalCostUSD)
		}
		return nil
	})
	k.On(toroid.EventSubagentStart, func(_ context.Context, e toroid.Event) error {
		if p, ok := e.Payload.(*toroid.SubagentPayload); ok {
			fmt.Printf("[subagent started] %s\n", p.Prompt)
		}
		return nil
	})

	// --- BLOCKING: Run returns the complete answer at once. ---
	fmt.Println("== blocking (Run) ==")
	out, usage, err := k.Run(ctx, "In one sentence, what is in this directory?")
	if err != nil {
		panic(err)
	}
	fmt.Println(out)
	fmt.Printf("sessions billed: %d | cost: $%.6f\n", len(usage.Tokens), k.RunningCostUSD())

	// --- STREAMING: Stream writes the final text to its io.Writer. ---
	fmt.Println("\n== streaming (Stream) ==")
	if err := k.Stream(ctx, "Count from 1 to 5, one number per line. Then fetch the details for the current user.", os.Stdout); err != nil {
		panic(err)
	}
	fmt.Println()

	// --- DELEGATION: synchronous subagent driven by the model. ---
	fmt.Println("\n== delegation: subagent (synchronous) ==")
	out, _, err = k.Run(ctx,
		"Use a subagent to read the first 10 lines of go.mod, then summarise the module's dependencies.")
	if err != nil {
		panic(err)
	}
	fmt.Println(out)

	// --- DELEGATION: asynchronous background agent (fire-and-wake). ---
	fmt.Println("\n== delegation: background (asynchronous) ==")
	done := make(chan struct{})
	k.On(toroid.EventSubagentStop, func(_ context.Context, e toroid.Event) error {
		if p, ok := e.Payload.(*toroid.SubagentPayload); ok && p.Async {
			fmt.Printf("[background %s] done\n", p.TaskID)
		}
		return nil
	})
	// MasterIdle fires after the woken turn finishes processing the result.
	k.On(toroid.EventMasterIdle, func(_ context.Context, _ toroid.Event) error {
		select {
		case <-done:
		default:
			close(done)
		}
		return nil
	})
	id := k.SpawnBackground("Count the number of .go files in the working directory.")
	fmt.Printf("spawned background task %s; doing other work…\n", id)
	<-done // the completed task wakes the idle kernel; wait for that turn to end
	fmt.Println("background work absorbed by the main kernel")

	// --- MULTIMODAL: two ways to get an image in front of the model. Gated on
	// TOROID_IMAGES so text-only models (glm/kimi) can skip; defaults on to keep
	// the example's normal behaviour for an image-capable default model.
	imagesOK := true
	if v := os.Getenv("TOROID_IMAGES"); v != "" {
		imagesOK = v == "1" || v == "true"
	}
	if imagesOK {
		// img.jpg sits next to this file; resolve an absolute path so it works
		// regardless of the directory you run from.
		_, thisFile, _, _ := runtime.Caller(0)
		imgPath := filepath.Join(filepath.Dir(thisFile), "..", "img.jpg")

		// (1) INLINE ATTACH: a markdown image ref in the prompt is parsed into a
		// file part and sent with the turn — the model sees the image directly, no
		// tool call. Text before/after the ref stays interleaved around the image.
		fmt.Println("\n== multimodal: inline attach ==")
		out, _, err = k.Run(ctx, fmt.Sprintf("Describe this image in one sentence: ![](%s)", imgPath))
		if err != nil {
			panic(err)
		}
		fmt.Println(out)

		// (2) TOOL READ: plain prose with no image ref. The model calls the read
		// tool, which returns the image bytes as a media attachment (Piece 1).
		fmt.Println("\n== multimodal: tool read ==")
		out, _, err = k.Run(ctx, fmt.Sprintf("Read the image at %s and tell me in one sentence what you see.", imgPath))
		if err != nil {
			panic(err)
		}
		fmt.Println(out)
	} else {
		fmt.Println("\n== multimodal: skipped (text-only model) ==")
	}

	// --- STRUCTURED OUTPUT AFTER TOOLS: WithSchema runs the full agentic loop
	// (tool calls included) and then coerces the result into the requested
	// schema. Here we ask the model to fetch a random user and summarise it.
	fmt.Println("\n== structured output after tools (Stream + WithSchema) ==")
	type UserSummary struct {
		Name     string `json:"name"`
		Email    string `json:"email"`
		Location string `json:"location"`
		Age      int    `json:"age"`
	}
	var buf strings.Builder
	if err := k.Stream(ctx,
		"Fetch a random user profile and summarise it.",
		&buf,
		toroid.WithSchema(
			toroid.GenerateSchema(reflect.TypeOf(UserSummary{})),
			"user_summary",
			"A structured summary of the fetched user",
		),
	); err != nil {
		panic(err)
	}
	var summary UserSummary
	if err := json.Unmarshal([]byte(buf.String()), &summary); err != nil {
		fmt.Printf("raw output: %s\n", buf.String())
		panic(err)
	}
	fmt.Printf("  Name:     %s\n", summary.Name)
	fmt.Printf("  Email:    %s\n", summary.Email)
	fmt.Printf("  Location: %s\n", summary.Location)
	fmt.Printf("  Age:      %d\n", summary.Age)

	// --- STRUCTURED GENERATION (single-turn): Run with WithSchema and no tool
	// interaction needed — the model returns a JSON object matching the schema.
	fmt.Println("\n== structured generation (Run + WithSchema, single-turn) ==")
	type DirectoryInfo struct {
		ProjectType string   `json:"project_type"`
		MainFiles   []string `json:"main_files"`
		Description string   `json:"description"`
	}
	dirSchema := toroid.GenerateSchema(reflect.TypeOf(DirectoryInfo{}))
	// Structured generation is single-turn with no tools — embed the facts directly.
	out, _, err = k.Run(ctx,
		"This is a Go library called toroid-kernel. Its root files include kernel.go, store.go, utils.go, multimodal.go, otlp.go, provider.go, and an examples/ directory. Return structured info about this project.",
		toroid.WithSchema(dirSchema, "DirectoryInfo", "Structured summary of a project directory"),
	)
	if err != nil {
		panic(err)
	}
	fmt.Println(out)

	// --- OBSERVABILITY: export the persisted trace as OpenTelemetry spans. ---
	fmt.Println("\n== OTEL export ==")
	traceID := k.SessionID() // root kernel: SessionID == TraceID
	spans, err := toroid.OTELSpans(traceID)
	if err != nil {
		panic(err)
	}
	fmt.Printf("OTEL spans for trace %s:\n", traceID)
	for _, s := range spans {
		fmt.Printf("  span=%s parent=%s name=%-9s cost=$%.4f\n",
			s.SpanID, s.ParentSpanID, s.Name, s.CostUSD)
		// GenAI semantic-convention attributes (model, token usage, cost) — this
		// is what an OTLP backend reads to render model/token/cost dashboards.
		for _, a := range s.Attributes {
			fmt.Printf("      %s = %v\n", a.Key, a.Value)
		}
		// Span events carry the full payload as a JSON attribute (no longer dropped).
		for _, e := range s.Events {
			fmt.Printf("      · %s @%d %s\n", e.Name, e.TimeUnix, e.Attribute)
		}
	}

	sessions, err := toroid.ListSessions()
	if err != nil {
		panic(err)
	}
	fmt.Printf("\n%d session(s) in the store (persisted at ~/.toroid/sql.db)\n", len(sessions))
}

// runGuardrails exercises the loop guards with NO network and NO API key: it
// drives the kernel with a scripted FauxStep so the guard behaviour is
// deterministic.
//
//   - MaxIter: an absolute cap on tool-call steps. Without it the loop is
//     unbounded.
//   - MaxRepeatCalls: a "spin" guard. If the model issues the SAME tool call
//     (name + input) and gets the SAME result N steps in a row, it is making
//     no progress and the loop is stopped early. Crucially the guard keys on
//     the RESULT too, so legitimate polling — same args, but a result that
//     changes over time — is never killed.
func runGuardrails(ctx context.Context) {
	// newKernel builds a kernel wired to a scripted Step and a single custom
	// tool. We disable the built-in computer tools so only our tool is in play.
	newKernel := func(cfg toroid.Config, step toroid.Step, tool *tools.ToolDef) *toroid.Kernel {
		cfg.Model = "mock/mock-model"
		cfg.IncludeComputerTools = false
		reg := tools.NewRegistry()
		reg.Register(tool)
		cfg.Tools = reg
		k, err := toroid.NewKernel(ctx, cfg)
		if err != nil {
			panic(err)
		}
		k.Step = step // no network: every llm-step comes from the script
		return k
	}
	// toolCallReply scripts one turn where the model asks for a tool.
	toolCallReply := func(name, input string) toroid.FauxReply {
		return toroid.FauxReply{
			ToolCalls: []toroid.FauxToolCall{{ID: "call", Name: name, Input: input}},
			Usage:     llm.Usage{Input: 5, Output: 5},
		}
	}

	// -----------------------------------------------------------------------
	// Scenario 1: a STUCK loop. The model calls ping({}) forever and the tool
	// always returns "pong". Identical call + identical result => the repeat
	// guard trips. We set MaxRepeatCalls=3 and MaxIter=50, so the guard fires
	// long before MaxIter, proving it is the thing stopping the loop.
	// -----------------------------------------------------------------------
	pingCalls := 0
	pingTool := &tools.ToolDef{
		Name:        "ping",
		Description: "always returns pong",
		Handler: llm.NewTool("ping", "always returns pong",
			func(ctx context.Context, _ struct{}) (llm.ToolResult, error) {
				pingCalls++
				return llm.NewTextResult("pong"), nil
			}),
	}
	// A single scripted reply repeats forever once the script is exhausted.
	stuck := &toroid.FauxStep{Replies: []toroid.FauxReply{toolCallReply("ping", "{}")}}
	k1 := newKernel(toroid.Config{MaxIter: 50, MaxRepeatCalls: 3}, stuck, pingTool)
	defer k1.Close()

	fmt.Println("== scenario 1: stuck loop (identical call + identical result) ==")
	if _, _, err := k1.Run(ctx, "ping repeatedly"); err != nil {
		panic(err)
	}
	fmt.Printf("ping tool ran %d times (guard MaxRepeatCalls=3, MaxIter=50)\n", pingCalls)
	if pingCalls == 3 {
		fmt.Print("PASS: repeat guard stopped the spin at 3, well before MaxIter\n\n")
	} else {
		fmt.Printf("FAIL: expected 3, got %d\n\n", pingCalls)
	}

	// -----------------------------------------------------------------------
	// Scenario 2: legitimate POLLING. The model calls poll({}) with the SAME
	// args each time, but the tool reports changing state ("pending 1",
	// "pending 2", ... "done"). Because the guard keys on the result, the
	// changing output means it never trips — the loop runs until the model
	// stops once the job is done. Same MaxRepeatCalls=3 as scenario 1.
	// -----------------------------------------------------------------------
	pollCalls := 0
	pollTool := &tools.ToolDef{
		Name:        "poll",
		Description: "polls a job; result changes over time",
		Handler: llm.NewTool("poll", "polls a job; result changes over time",
			func(ctx context.Context, _ struct{}) (llm.ToolResult, error) {
				pollCalls++
				if pollCalls >= 4 {
					return llm.NewTextResult("status: done"), nil
				}
				return llm.NewTextResult(fmt.Sprintf("status: pending %d", pollCalls)), nil
			}),
	}
	poller := &toroid.FauxStep{Replies: []toroid.FauxReply{
		toolCallReply("poll", "{}"),
		toolCallReply("poll", "{}"),
		toolCallReply("poll", "{}"),
		toolCallReply("poll", "{}"), // 4th call observes "status: done"
		{Text: "job finished", Usage: llm.Usage{Input: 5, Output: 5}},
	}}
	k2 := newKernel(toroid.Config{MaxIter: 50, MaxRepeatCalls: 3}, poller, pollTool)
	defer k2.Close()

	fmt.Println("== scenario 2: polling (identical args, changing result) ==")
	out, _, err := k2.Run(ctx, "poll the job until done")
	if err != nil {
		panic(err)
	}
	fmt.Printf("poll tool ran %d times, final answer: %q\n", pollCalls, out)
	if pollCalls == 4 {
		fmt.Println("PASS: guard left the progressing poll alone; it ran to completion")
	} else {
		fmt.Printf("FAIL: expected 4 poll calls, got %d\n", pollCalls)
	}
}
