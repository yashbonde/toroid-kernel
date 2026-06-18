// Pattern: LANGFUSE OBSERVABILITY — run a real agent turn through an LLM gateway
// and push the persisted trace to Langfuse over OTLP.
//
// The kernel records every span/cost/event to its SQLite store (Save:true).
// After the run, toroid.LangfuseOTLP projects that trace into OpenTelemetry
// spans and POSTs them to the project's OTLP endpoint — the run then shows up in
// the Langfuse UI with model, token usage, and cost.
//
// This example talks to an OpenAI-compatible LLM gateway (e.g. a LiteLLM proxy)
// via the "llmgateway/<model>" provider prefix. Set:
//
//	export LLM_GATEWAY_BASE_URL=https://<your-litellm-host>/v1
//	export LITELLM_API_KEY=sk-…                  # bearer token for the gateway
//	export LANGFUSE_PUBLIC_KEY=pk-lf-…
//	export LANGFUSE_SECRET_KEY=sk-lf-…
//	export LANGFUSE_BASE_URL=https://<your-langfuse-host>
//	go run ./examples/langfuse
package main

import (
	"context"
	"fmt"
	"os"

	toroid "github.com/yashbonde/toroid-kernel"
)

func mustEnv(name string) string {
	v := os.Getenv(name)
	if v == "" {
		fmt.Printf("set %s to run this example\n", name)
		os.Exit(1)
	}
	return v
}

func main() {
	gatewayKey := mustEnv("LITELLM_API_KEY")
	_ = mustEnv("LLM_GATEWAY_BASE_URL") // read by the llmgateway provider
	lfBase := mustEnv("LANGFUSE_BASE_URL")
	lfPub := mustEnv("LANGFUSE_PUBLIC_KEY")
	lfSec := mustEnv("LANGFUSE_SECRET_KEY")

	ctx := context.Background()

	k, err := toroid.NewKernel(ctx, toroid.Config{
		Model:   "llmgateway/kimi-k2p6",
		APIKey:  gatewayKey,
		WorkDir: ".",
		Save:    true, // persist the trace so we can export it to Langfuse
	})
	if err != nil {
		panic(err)
	}
	defer k.Close()

	k.On(toroid.EventPreToolUse, func(_ context.Context, e toroid.Event) error {
		if p, ok := e.Payload.(*toroid.ToolUsePayload); ok {
			fmt.Printf("→ tool %s args=%s\n", p.Name, p.Args)
		}
		return nil
	})

	fmt.Println("== running kimi-k2p6 via llmgateway ==")
	out, usage, err := k.Run(ctx, "List the files in this directory, then say in one sentence what kind of project this is.")
	if err != nil {
		panic(err)
	}
	fmt.Println(out)
	fmt.Printf("sessions billed: %d | cost: $%.6f\n", len(usage.Tokens), k.RunningCostUSD())

	// Export the persisted trace to Langfuse.
	traceID := k.SessionID() // root kernel: SessionID == TraceID
	if err := toroid.LangfuseOTLP(ctx, traceID, lfBase, lfPub, lfSec); err != nil {
		panic(err)
	}
	fmt.Printf("\npushed trace to Langfuse: %s/project (trace id derived from %s)\n", lfBase, traceID)
}
