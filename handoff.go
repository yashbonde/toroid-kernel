package toroid

import "github.com/yashbonde/toroid-kernel/llm"

// Cross-model handoff (M9). When a chat switches the model mid-transcript —
// compaction or a subagent running on SmallerModel — the accumulated history
// must remain valid for the target model. Tool calls and tool results are always
// preserved; only reasoning/thinking blocks are provider-shaped and can be
// rejected by a different wire API.
//
// In this port every in-scope model speaks openai-completions (llmgateway/*), so
// a handoff is same-API and history passes through untouched. The guard below
// only transforms when the source and target APIs actually differ — dropping
// reasoning blocks, the safest transform, since no cross-API thinking-block
// translation is in scope. Keeping the seam here means the rule lives in one
// place if a second wire API is ever added.

// TransformForHandoff returns history adapted for a target model when the chat
// switches models. For a same-API switch (the only case in this port) it returns
// history unchanged. For a cross-API switch it strips reasoning blocks so the
// target provider does not reject foreign thinking content, while preserving all
// tool calls and tool results.
func TransformForHandoff(history []llm.Message, from, to Model) []llm.Message {
	if from.API == to.API {
		return history // same wire: nothing to transform (the in-scope path)
	}
	out := make([]llm.Message, 0, len(history))
	for _, msg := range history {
		out = append(out, stripReasoning(msg))
	}
	return out
}

// stripReasoning removes reasoning/thinking parts from a message while keeping
// text, tool calls, and tool results intact. A message left with no content is
// still emitted (empty content) so message ordering/roles are preserved.
func stripReasoning(msg llm.Message) llm.Message {
	filtered := make([]llm.Part, 0, len(msg.Parts))
	for _, part := range msg.Parts {
		if _, isReasoning := part.(llm.ReasoningPart); isReasoning {
			continue // drop provider-specific thinking blocks on cross-API handoff
		}
		filtered = append(filtered, part)
	}
	msg.Parts = filtered
	return msg
}
