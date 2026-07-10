// Pattern: STRUCTURED OUTPUT AFTER TOOL CALLS
//
// WithSchema now runs the full agentic loop (including tool calls) first,
// then does a final GenerateObject pass over the accumulated history to
// coerce the result into the requested schema.
//
//	export LLM_GATEWAY_BASE_URL=https://my-gateway.example.com/v1
//	export LLM_GATEWAY_KEY=sk-...
//	go run ./examples/structured-after-tools
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"reflect"
	"strings"

	toroid "github.com/yashbonde/toroid-kernel"
	"github.com/yashbonde/toroid-kernel/llm"
	tools "github.com/yashbonde/toroid-kernel/tools"
)

type UserSummary struct {
	Name     string `json:"name"`
	Email    string `json:"email"`
	Location string `json:"location"`
	Age      int    `json:"age"`
}

func main() {
	ctx := context.Background()

	apiKey := os.Getenv("LLM_GATEWAY_KEY")
	model := "llmgateway/glm-5p2"
	if m := os.Getenv("TOROID_MODEL"); m != "" {
		model = m
	}
	if apiKey == "" {
		fmt.Println("set LLM_GATEWAY_KEY to run this example")
		os.Exit(1)
	}

	k, err := toroid.NewKernel(ctx, toroid.Config{
		Model:                model,
		APIKey:               apiKey,
		WorkDir:              ".",
		TotalContextSize:     200_000, // avoid mid-run compaction
		IncludeComputerTools: true,
	})
	if err != nil {
		panic(err)
	}
	defer k.Close()

	// Register a tool that fetches a random user profile
	type GetUserArgs struct{}
	k.Tools.Register(&tools.ToolDef{
		Name:        "get_user",
		Description: "Fetch a random user profile from randomuser.me",
		Handler: llm.NewTool(
			"get_user",
			"Fetch a random user profile from randomuser.me",
			func(ctx context.Context, args GetUserArgs) (llm.ToolResult, error) {
				resp, err := http.Get("https://randomuser.me/api/?inc=name,email,location,dob")
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

	k.On(toroid.EventPreToolUse, func(_ context.Context, e toroid.Event) error {
		if p, ok := e.Payload.(*toroid.ToolUsePayload); ok {
			fmt.Printf("[tool call] %s\n", p.Name)
		}
		return nil
	})
	k.On(toroid.EventPostToolUse, func(_ context.Context, e toroid.Event) error {
		if p, ok := e.Payload.(*toroid.ToolUseResultPayload); ok {
			res := strings.TrimSpace(p.Result)
			if len(res) > 80 {
				res = res[:80]
			}
			fmt.Printf("[tool result] %s -> %s\n", p.Name, res)
		}
		return nil
	})

	fmt.Println("Running agent with tool calls + structured output...")
	var buf strings.Builder
	err = k.Stream(ctx,
		"Fetch a random user profile and summarise it.",
		&buf,
		toroid.WithSchema(
			toroid.GenerateSchema(reflect.TypeOf(UserSummary{})),
			"user_summary",
			"A structured summary of the fetched user",
		),
	)
	if err != nil {
		panic(err)
	}

	var result UserSummary
	if err := json.Unmarshal([]byte(buf.String()), &result); err != nil {
		fmt.Printf("raw output: %s\n", buf.String())
		panic(err)
	}

	fmt.Printf("\nStructured result:\n")
	fmt.Printf("  Name:     %s\n", result.Name)
	fmt.Printf("  Email:    %s\n", result.Email)
	fmt.Printf("  Location: %s\n", result.Location)
	fmt.Printf("  Age:      %d\n", result.Age)
}
