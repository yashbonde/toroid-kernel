package toroid

import (
	"context"
	"fmt"
	"io"

	"github.com/yashbonde/toroid-kernel/tools"

	"charm.land/fantasy"
)

// Background agents (§12.4).
//
// A background agent is an asynchronous subagent: the parent fires it off, gets
// an id back immediately, and keeps working. When the child finishes, its result
// is Enqueue()d and — if the parent has since gone idle — the kernel is woken and
// re-enters its loop to process the result, the way a background-task completion
// notification wakes the agent in Claude Code. This reuses the existing message
// queue + step-boundary interrupt machinery; the only new primitive is Wake.

// wakeMu serializes wake attempts (see Wake). It is a package-free field on the
// Kernel via the embedded sync types declared in kernel.go's struct; we keep the
// extra mutex local to this file to avoid touching that struct further.

// SpawnBackground starts task as an asynchronous subagent and returns a short
// task id. The call returns immediately; the result is delivered later via the
// message queue, waking the kernel if it is idle.
func (k *Kernel) SpawnBackground(task string) string {
	id := fmt.Sprintf("bg-%d", k.bgSeq.Add(1))
	go func() {
		bctx := context.Background()
		out, err := k.RunSubagent(bctx, task)
		result := out
		status := "completed"
		if err != nil {
			result = fmt.Sprintf("background task %s failed: %v", id, err)
			status = "failed"
		}
		_ = k.Fire(bctx, string(EventTaskCompleted), &TaskPayload{TaskID: id, Title: task, Status: status})
		k.Enqueue(fmt.Sprintf("[background task %s %s]\n%s", id, status, result))
		// If a loop is already running it will drain the queue at the next step
		// boundary; otherwise wake the idle kernel to process the result.
		if !k.running.Load() {
			_ = k.Wake(bctx)
		}
	}()
	return id
}

// Wake re-enters the agent loop to process any queued messages when the kernel
// is idle. It is a no-op if a loop is already running (that loop will drain the
// queue itself) or if there is nothing queued. Output is streamed to the writer
// from the kernel's most recent Stream call, falling back to io.Discard.
func (k *Kernel) Wake(ctx context.Context) error {
	k.wakeMu.Lock()
	defer k.wakeMu.Unlock()

	if k.running.Load() {
		return nil // a live loop will drain the queue
	}
	msgs := k.drainQueue()
	if len(msgs) == 0 {
		return nil
	}
	if len(k.history) == 0 && k.systemPrompt != "" {
		k.history = append(k.history, fantasy.NewSystemMessage(k.systemPrompt))
	}
	for _, m := range msgs {
		k.history = append(k.history, fantasy.NewUserMessage(m))
	}
	w := k.lastWriter
	if w == nil {
		w = io.Discard
	}
	return k.streamCurrent(ctx, w, "")
}

// SubagentAsyncArgs is the argument schema for the subagent_async tool.
type SubagentAsyncArgs struct {
	Task string `json:"task" jsonschema:"description=Full description of the subtask to run in the background"`
}

// newSubagentAsyncTool builds the subagent_async tool, which delegates a subtask
// to a background agent and returns immediately.
func newSubagentAsyncTool(k *Kernel, desc string) *tools.ToolDef {
	fTool := fantasy.NewAgentTool("subagent_async", desc, func(ctx context.Context, args SubagentAsyncArgs, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
		id := k.SpawnBackground(args.Task)
		return fantasy.ToolResponse{
			Type:    "text",
			Content: fmt.Sprintf("Started background task %s. You'll be notified when it completes — continue with other work in the meantime.", id),
		}, nil
	})
	return &tools.ToolDef{
		Name:        "subagent_async",
		Description: desc,
		Template:    "subagent.tool.tmpl",
		AgentTool:   fTool,
	}
}
