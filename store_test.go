package toroid

import (
	"os"
	"path/filepath"
	"testing"
)

// TestStoreRoundTrip exercises the SQLite-backed store end to end against a
// temporary database, verifying meta/cost/event persistence, trace totals,
// reconstruction, listing, and deletion.
func TestStoreRoundTrip(t *testing.T) {
	// Point the store at a temp dir so we never touch ~/.swarmbuddy.
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	// reset singleton so it reopens under the temp HOME
	dbMu.Lock()
	defaultDB = nil
	dbMu.Unlock()

	st, err := NewStore()
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	defer st.Close()

	trace := NewSessionID()
	if err := st.SaveTraceMeta(TraceMeta{TraceID: trace, Title: "t1", StartedAt: 100}); err != nil {
		t.Fatalf("SaveTraceMeta: %v", err)
	}
	if err := st.SaveSpanMeta(SpanMeta{SpanID: trace, TraceID: trace, Model: "m", StartedAt: 100}); err != nil {
		t.Fatalf("SaveSpanMeta: %v", err)
	}
	// title-only save must not wipe started_at
	if err := st.SaveTraceMeta(TraceMeta{TraceID: trace, Title: "t2"}); err != nil {
		t.Fatalf("SaveTraceMeta update: %v", err)
	}
	tm, _ := st.LoadTraceMeta(trace)
	if tm.Title != "t2" || tm.StartedAt != 100 {
		t.Fatalf("trace meta = %+v, want title t2 started_at 100", tm)
	}

	if err := st.AppendCost(trace, trace, 0.5, 0.5); err != nil {
		t.Fatalf("AppendCost: %v", err)
	}
	if err := st.AppendCost(trace, trace, 0.25, 0.75); err != nil {
		t.Fatalf("AppendCost: %v", err)
	}
	if got := st.LoadLastTotalUSD(trace, trace); got != 0.75 {
		t.Fatalf("LoadLastTotalUSD = %v, want 0.75", got)
	}
	if got := st.LoadTraceTotal(trace); got != 0.75 {
		t.Fatalf("LoadTraceTotal = %v, want 0.75", got)
	}

	ev := Event{Kind: EventUserPromptSubmit, TraceID: trace, SpanID: trace, EmitTS: 200, Seq: 1,
		Payload: &UserPromptPayload{Prompt: "hello"}}
	if err := st.AppendEvent(trace, trace, ev); err != nil {
		t.Fatalf("AppendEvent: %v", err)
	}

	td, err := LoadTraceData(trace)
	if err != nil {
		t.Fatalf("LoadTraceData: %v", err)
	}
	if len(td.Spans) != 1 || len(td.Spans[0].Events) != 1 || len(td.Spans[0].Costs) != 2 {
		t.Fatalf("LoadTraceData spans=%d events=%d costs=%d", len(td.Spans),
			len(td.Spans[0].Events), len(td.Spans[0].Costs))
	}

	sessions, err := ListSessions()
	if err != nil || len(sessions) != 1 || sessions[0].ID != trace {
		t.Fatalf("ListSessions = %+v err=%v", sessions, err)
	}

	if err := DeleteSession(trace); err != nil {
		t.Fatalf("DeleteSession: %v", err)
	}
	if sessions, _ := ListSessions(); len(sessions) != 0 {
		t.Fatalf("after delete, sessions = %+v", sessions)
	}

	// sanity: the DB file was actually created under the temp HOME
	if _, err := os.Stat(filepath.Join(tmp, ".swarmbuddy", "sql.db")); err != nil {
		t.Fatalf("sql.db not created: %v", err)
	}
}
