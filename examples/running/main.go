// Pattern: RUNNING A KERNEL — blocking vs streaming.
//
// There are two ways to drive the agent loop, and they share the same tool-calling
// machinery underneath:
//
//   - Run    blocks and returns the full final text in one call, plus a
//     UsagePayload (per-session token totals). Use it when you just want
//     the answer.
//
//   - Stream writes the final text to an io.Writer as it is produced. Subscribe
//     to EventToken to observe individual token deltas (e.g. to render a
//     live UI). Use it for interactive/TUI hosts.
//
//     export ANTHROPIC_API_KEY=your_api_key
//     go run ./examples/running
package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"

	"charm.land/fantasy"
	fantasyschema "charm.land/fantasy/schema"
	toroid "github.com/yashbonde/toroid-kernel"
	tools "github.com/yashbonde/toroid-kernel/tools"
)

func main() {
	ctx := context.Background()

	// Prefer llmgateway when configured, fall back to Anthropic.
	apiKey := os.Getenv("LLM_GATEWAY_KEY")
	model := "llmgateway/claude-haiku-4-5"
	if apiKey == "" {
		apiKey = os.Getenv("ANTHROPIC_API_KEY")
		model = "anthropic/claude-haiku-4-5"
	}
	if apiKey == "" {
		fmt.Println("set LLM_GATEWAY_KEY or ANTHROPIC_API_KEY to run this example")
		return
	}

	k, err := toroid.NewKernel(ctx, toroid.Config{
		Model:   model,
		APIKey:  apiKey,
		WorkDir: ".",
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
		AgentTool: fantasy.NewAgentTool(
			"get_user",
			"Fetch the current user profile from randomuser.me",
			func(ctx context.Context, args GetUserArgs, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
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
					return fantasy.ToolResponse{}, err
				}
				defer resp.Body.Close()
				body, err := io.ReadAll(resp.Body)
				if err != nil {
					return fantasy.ToolResponse{}, err
				}
				return fantasy.ToolResponse{Type: "text", Content: string(body)}, nil
			}),
	})

	// --- BLOCKING: Run returns the complete answer at once. ---
	fmt.Println("== blocking (Run) ==")
	out, usage, err := k.Run(ctx, "In one sentence, what is in this directory?")
	if err != nil {
		panic(err)
	}
	fmt.Println(out)
	fmt.Printf("sessions billed: %d | cost: $%.6f\n", len(usage.Tokens), k.RunningCostUSD())

	// --- STREAMING: render token deltas as they arrive via EventToken. ---
	// Stream also writes the final text to its io.Writer, but a UI typically wants
	// the per-token deltas — so we stream to io.Discard and render from the hook.
	fmt.Println("\n== streaming (Stream) ==")
	k.On(toroid.EventToken, func(_ context.Context, e toroid.Event) error {
		if p, ok := e.Payload.(*toroid.TokenPayload); ok {
			fmt.Print(p.Text)
		}
		return nil
	})
	if err := k.Stream(ctx, "Count from 1 to 5, one number per line. Then fetch the details for the current user.", io.Discard); err != nil {
		panic(err)
	}
	fmt.Println()

	// --- MULTIMODAL: two ways to get an image in front of the model. ---
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

	// --- STRUCTURED GENERATION: Run with WithSchema bypasses the agent loop
	// and calls GenerateObject directly, returning a JSON object that strictly
	// matches the schema. No tool calls, no history — single-turn only.
	fmt.Println("\n== structured generation (Run + WithSchema) ==")
	type DirectoryInfo struct {
		ProjectType string   `json:"project_type"`
		MainFiles   []string `json:"main_files"`
		Description string   `json:"description"`
	}
	dirSchema := fantasyschema.Generate(reflect.TypeOf(DirectoryInfo{}))
	// Structured generation is single-turn with no tools — embed the facts directly.
	out, _, err = k.Run(ctx,
		"This is a Go library called toroid-kernel. Its root files include kernel.go, store.go, utils.go, multimodal.go, otlp.go, provider.go, and an examples/ directory. Return structured info about this project.",
		toroid.WithSchema(dirSchema, "DirectoryInfo", "Structured summary of a project directory"),
	)
	if err != nil {
		panic(err)
	}
	fmt.Println(out)
}
