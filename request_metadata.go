package toroid

import (
	"context"

	"github.com/yashbonde/toroid-kernel/llm"
)

// requestHierarchyScope is stable for one product chat. It lives in context so
// compaction, the agent loop, and a post-loop schema request share the chat ID
// without adding mutable per-run state to Kernel.
type requestHierarchyScope struct {
	transcriptID string
	chatID       string
}

type requestHierarchyKey struct{}

// withRequestChat starts a fresh chat beneath this kernel's transcript.
func (k *Kernel) withRequestChat(ctx context.Context) context.Context {
	return context.WithValue(ctx, requestHierarchyKey{}, requestHierarchyScope{
		transcriptID: k.Cfg.TraceID,
		chatID:       NewSessionID(),
	})
}

// nextRequestMetadata returns complete hierarchy metadata for one LLM request.
// A turn currently contains one LLM request, but the IDs remain separate so a
// receiver does not have to assume that implementation detail. Standalone
// Compact calls synthesize a chat scope so no metadata field is ever blank.
func (k *Kernel) nextRequestMetadata(ctx context.Context) llm.RequestMetadata {
	scope, ok := ctx.Value(requestHierarchyKey{}).(requestHierarchyScope)
	if !ok || scope.transcriptID != k.Cfg.TraceID || scope.chatID == "" {
		scope = requestHierarchyScope{
			transcriptID: k.Cfg.TraceID,
			chatID:       NewSessionID(),
		}
	}
	return llm.RequestMetadata{
		TranscriptID: scope.transcriptID,
		ChatID:       scope.chatID,
		TurnID:       NewSessionID(),
		TraceID:      NewSessionID(),
	}
}
