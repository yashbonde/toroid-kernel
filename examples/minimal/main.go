// Minimal toroid-kernel example: construct a kernel, run one prompt, print the
// answer and the cost. Run with:
//
//	export GEMINI_TOKEN=your_api_key
//	go run ./examples/minimal
package main

import (
	"context"
	"fmt"
	"os"

	toroid "github.com/yashbonde/toroid-kernel"
)

func main() {
	ctx := context.Background()

	k, err := toroid.NewKernel(ctx, toroid.Config{
		Model:   "google/gemini-3-flash-preview",
		APIKey:  os.Getenv("GEMINI_TOKEN"),
		WorkDir: ".",
	})
	if err != nil {
		panic(err)
	}
	defer k.Close()

	out, _, err := k.Run(ctx, "List the files in the working directory and name the largest one.")
	if err != nil {
		panic(err)
	}

	fmt.Println(out)
	fmt.Printf("\ncost: $%.6f\n", k.RunningCostUSD())
}
