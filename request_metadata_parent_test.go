package toroid

import (
	"context"
	"testing"
)

// A subagent's first request carries parent_session_id — that is the message
// establishing the parent→child edge. Later requests in the same session carry
// only session_id, and a root kernel never carries a parent.
func TestParentSessionIDOnlyOnFirstRequest(t *testing.T) {
	child := &Kernel{Cfg: Config{SessionID: "child-1", TraceID: "trace-1", ParentSpanID: "parent-1"}}
	ctx := context.Background()

	first := child.nextRequestMetadata(ctx)
	if first.SessionID != "child-1" {
		t.Fatalf("session_id = %q, want child-1", first.SessionID)
	}
	if first.ParentSessionID != "parent-1" {
		t.Fatalf("first request parent_session_id = %q, want parent-1", first.ParentSessionID)
	}

	for i := range 3 {
		next := child.nextRequestMetadata(ctx)
		if next.ParentSessionID != "" {
			t.Fatalf("request %d carried parent_session_id %q; only the first should", i+2, next.ParentSessionID)
		}
		if next.SessionID != "child-1" {
			t.Fatalf("request %d session_id = %q, want child-1", i+2, next.SessionID)
		}
	}
}

func TestRootKernelHasNoParentSessionID(t *testing.T) {
	root := &Kernel{Cfg: Config{SessionID: "root-1", TraceID: "root-1"}}
	md := root.nextRequestMetadata(context.Background())
	if md.ParentSessionID != "" {
		t.Fatalf("root parent_session_id = %q, want empty", md.ParentSessionID)
	}
	if md.SessionID != "root-1" {
		t.Fatalf("root session_id = %q, want root-1", md.SessionID)
	}
}
