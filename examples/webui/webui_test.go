package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	toroid "github.com/yashbonde/toroid-kernel"
	"github.com/yashbonde/toroid-kernel/llm"
)

func TestSharedWorkspace(t *testing.T) {
	t.Setenv("TOROID_HOME", t.TempDir())
	t.Setenv(toroid.GatewayBaseURLEnv, "")
	store, err := toroid.NewStore()
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.DB().Exec(webSchema); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-16 * 24 * time.Hour).UnixNano()
	for _, id := range []string{"recent", "old", "old-pinned", "swarm"} {
		start := old
		if id == "recent" || id == "swarm" {
			start = time.Now().UnixNano()
		}
		if err = store.SaveTraceMeta(toroid.TraceMeta{TraceID: id, StartedAt: start}); err != nil {
			t.Fatal(err)
		}
		if err = store.SaveSpanMeta(toroid.SpanMeta{TraceID: id, SpanID: id, StartedAt: start}); err != nil {
			t.Fatal(err)
		}
	}
	if err = store.SaveSpanMeta(toroid.SpanMeta{TraceID: "recent", SpanID: "child", ParentSpanID: "recent"}); err != nil {
		t.Fatal(err)
	}
	if _, err = store.DB().Exec(`INSERT INTO webui_chats(trace_id,pinned) VALUES('old-pinned',1),('swarm',1)`); err != nil {
		t.Fatal(err)
	}
	if _, err = store.DB().Exec(`INSERT INTO webui_created_chats(trace_id) VALUES('recent'),('old'),('old-pinned')`); err != nil {
		t.Fatal(err)
	}
	// A pre-consolidation CLI conversation must still display and resume.
	for i, event := range []toroid.Event{
		{Kind: toroid.EventUserPromptSubmit, Payload: toroid.UserPromptPayload{Prompt: "First prompt"}},
		{Kind: "AssistantTurn", Payload: map[string]any{"messages": []any{map[string]any{"role": "assistant", "parts": []any{map[string]string{"kind": "text", "text": "Earlier answer"}}}}}},
	} {
		event.Seq = uint64(i + 1)
		if err = store.AppendEvent("recent", "recent", event); err != nil {
			t.Fatal(err)
		}
	}
	var kernels []*toroid.Kernel
	a := app{db: store.DB(), host: "127.0.0.1:8000", launchKey: rand.Text(), workdir: t.TempDir(), base: agent{"base", "base", settings{Model: "llmgateway/test", Thinking: toroid.ThinkingNone}},
		newKernel: func(ctx context.Context, cfg toroid.Config) (*toroid.Kernel, error) {
			k, err := toroid.NewKernel(ctx, cfg)
			if err == nil {
				k.Step = &toroid.FauxStep{Replies: []toroid.FauxReply{{Text: "First second third"}}}
				kernels = append(kernels, k)
			}
			return k, err
		},
	}
	h := a.routes()
	t.Run("chat usage counts billed work once", func(t *testing.T) {
		const id = "usage-fixture"
		add := func(seq int, kind toroid.EventKind, payload any) {
			t.Helper()
			e := toroid.Event{Seq: uint64(seq), EmitTS: int64(seq * 10), Kind: kind, Payload: payload}
			if err := store.AppendEvent(id, id, e); err != nil {
				t.Fatal(err)
			}
		}
		catalog := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Write([]byte(`{"data":[{"id":"test","max_input_tokens":1048576,"pricing":{"unit":"micro_usd_per_million_tokens","input":220000,"output":660000,"cache_read":7000,"cache_write":0}}]}`))
		}))
		defer catalog.Close()
		t.Setenv(toroid.GatewayBaseURLEnv, catalog.URL)
		add(1, toroid.EventTurnStarted, toroid.TurnPayload{Model: "llmgateway/test", ContextWindow: 200000})
		add(2, toroid.EventTurnCompleted, toroid.TurnPayload{StopReason: "stop", TurnUsage: toroid.Usage{
			Input: 482, CacheRead: 97850, Output: 508, Reasoning: 150, Cost: .7, PricingOK: true,
		}})
		child := toroid.Event{Seq: 1, EmitTS: 25, Kind: toroid.EventTurnCompleted, Payload: toroid.TurnPayload{
			StopReason: "stop", TurnUsage: toroid.Usage{Cost: .125, PricingOK: true, CostSource: "gateway"},
		}}
		if err := store.AppendEvent(id, "child", child); err != nil {
			t.Fatal(err)
		}
		u, err := a.savedUsage(t.Context(), id)
		if err != nil || u.SpendUSD == nil || *u.SpendUSD != .126127 || !u.EstimatedSpend ||
			u.Used == nil || *u.Used != 98840 || u.Window != 200000 {
			t.Fatalf("historical usage %+v, %v", u, err)
		}
		add(3, toroid.EventPostCompact, toroid.CompactSummaryPayload{TokensAfter: 40, TurnUsage: &toroid.Usage{PricingOK: true, CostSource: "gateway"}})
		u, err = a.savedUsage(t.Context(), id)
		if err != nil || u.SpendUSD == nil || *u.SpendUSD != .126127 || u.Used == nil || *u.Used != 40 {
			t.Fatalf("compaction %+v, %v", u, err)
		}
		add(4, toroid.EventTurnCompleted, toroid.TurnPayload{StopReason: "stop", TurnUsage: toroid.Usage{Input: 32}})
		u, err = a.spend(t.Context(), id)
		if err != nil || u.SpendUSD != nil {
			t.Fatalf("unknown price %+v, %v", u, err)
		}
	})
	unlock := httptest.NewRequest("POST", "http://"+a.host+"/api/unlock", strings.NewReader(`{"key":"`+a.launchKey+`"}`))
	unlock.Header.Set("Content-Type", "application/json")
	unlocked := httptest.NewRecorder()
	h.ServeHTTP(unlocked, unlock)
	if unlocked.Code != 204 {
		t.Fatalf("unlock: %d", unlocked.Code)
	}
	cookie := unlocked.Result().Cookies()[0]
	call := func(method, path string, body any) *httptest.ResponseRecorder {
		t.Helper()
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		r := httptest.NewRequest(method, "http://"+a.host+"/api/"+path, bytes.NewReader(b))
		r.Header.Set("Content-Type", "application/json")
		r.AddCookie(cookie)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	assertOK := func(w *httptest.ResponseRecorder) {
		t.Helper()
		if w.Code >= 300 {
			t.Fatalf("HTTP %d: %s", w.Code, w.Body)
		}
	}
	list, err := a.chats(t.Context())
	if err != nil || len(list) != 2 || list[0].ID != "old-pinned" || list[1].ID != "recent" {
		t.Fatalf("sidebar: %v, %v", list, err)
	}
	assertOK(call("PUT", "chats/old-pinned/pin", map[string]bool{"pinned": false}))
	list, err = a.chats(t.Context())
	if err != nil || len(list) != 1 {
		t.Fatalf("unpin: %v, %v", list, err)
	}
	if w := call("GET", "chats/child", nil); w.Code != 404 {
		t.Fatalf("child detail = %d", w.Code)
	}
	if w := call("PUT", "chats/child/pin", map[string]bool{"pinned": true}); w.Code != 404 {
		t.Fatalf("child pin = %d", w.Code)
	}

	draft := a.base.Settings
	draft.System = "Check sources."
	w := call("POST", "turns", turnRequest{Chat: "recent", Agent: "base", Draft: &draft, Text: "Continue", Comments: []comment{{Quote: "Earlier answer", Note: "Explain.", Anchor: &commentAnchor{Kind: "assistant"}}}})
	assertOK(w)
	if strings.Count(w.Body.String(), `"type":"delta"`) != 3 || !strings.Contains(w.Body.String(), `"type":"done"`) {
		t.Fatalf("stream: %s", w.Body)
	}
	if len(kernels) != 1 || !strings.Contains(kernels[0].SystemPrompt, draft.System) {
		t.Fatal("draft settings missing")
	}
	m, err := a.messages(t.Context(), "recent")
	if err != nil || len(m) != 4 || m[1].Text != "Earlier answer" || len(m[2].Comments) != 1 || m[2].Comments[0].Anchor.Kind != "assistant" || m[2].Text != "Continue\n\n> Earlier answer\nExplain." || m[3].Text != "First second third" {
		t.Fatalf("history = %v, %v", m, err)
	}

	w = call("POST", "agents", map[string]any{"name": "Reviewer", "settings": draft, "preview": "recent"})
	assertOK(w)
	var named agent
	if err = json.Unmarshal(w.Body.Bytes(), &named); err != nil {
		t.Fatal(err)
	}
	id, d, err := a.chatProfile(t.Context(), "recent")
	if err != nil || id != named.ID || d != nil {
		t.Fatalf("saved draft: %s %v %v", id, d, err)
	}
	assertOK(call("POST", "turns", turnRequest{Chat: "recent", Agent: "base", Text: "Next prompt"}))
	k := kernels[1]
	if len(k.History) != 6 || k.History[1].Text() != "Earlier answer" || k.History[4].Text() != "Next prompt" || strings.Contains(k.SystemPrompt, draft.System) {
		t.Fatalf("agent switch did not preserve history and replace instructions")
	}

	assertOK(call("POST", "turns", turnRequest{Chat: "swarm", Agent: "base", Text: "Resume an external chat"}))
	assertOK(call("POST", "turns", turnRequest{Agent: "base", Text: "New UI chat"}))
	list, err = a.chats(t.Context())
	if err != nil || len(list) != 2 || list[0].Title != "New UI chat" || list[1].ID != "recent" {
		t.Fatalf("UI origin after create and resume: %+v, %v", list, err)
	}

	t.Run("path and image attachments survive replay", func(t *testing.T) {
		image := filepath.Join(a.workdir, "preview (1).png")
		file := filepath.Join(a.workdir, "private description.txt")
		bytesOnDisk := []byte("\x89PNG\r\n\x1a\nfixture")
		if err := os.WriteFile(image, bytesOnDisk, 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(file, []byte("contents stay on disk"), 0600); err != nil {
			t.Fatal(err)
		}
		req := turnRequest{Agent: "base", Attachments: []string{file, a.workdir, image}}
		if w := call("POST", "turns", req); w.Code != 400 {
			t.Fatalf("text-only model accepted image: %s", w.Body)
		}
		req.Model = "llmgateway/gpt-4o"
		assertOK(call("POST", "turns", req))
		k := kernels[len(kernels)-1]
		id := k.Cfg.SessionID
		history, err := toroid.ReconstructHistory(id, id, "", a.workdir, req.Model)
		if err != nil || len(history) == 0 {
			t.Fatalf("replay: %v", err)
		}
		for _, msg := range []llm.Message{k.History[0], history[0]} {
			if msg.Text() != strings.Join(req.Attachments, "\n\n") || len(msg.Parts) != 4 {
				t.Fatalf("unexpected path representation: %#v", msg)
			}
			part, ok := msg.Parts[3].(llm.FilePart)
			if !ok || part.MediaType != "image/png" || !bytes.Equal(part.Data, bytesOnDisk) {
				t.Fatalf("missing image input: %#v", part)
			}
		}
		saved, err := a.messages(t.Context(), id)
		if err != nil || len(saved) == 0 || len(saved[0].Attachments) != 3 || saved[0].Attachments[1].Kind != "folder" {
			t.Fatalf("saved transcript: %v, %v", saved, err)
		}
	})

	t.Run("model and folder follow the chat", func(t *testing.T) {
		dir, err := filepath.EvalSymlinks(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		w := call("POST", "turns", turnRequest{Agent: named.ID, Workspace: dir, Model: "llmgateway/override", Text: "Folder test"})
		assertOK(w)
		k := kernels[len(kernels)-1]
		if k.Cfg.WorkDir != dir || k.Cfg.Model != "llmgateway/override" {
			t.Fatal("kernel did not receive chat overrides")
		}
		result := call("GET", "chats/"+k.SessionID(), nil)
		var detail struct{ Options chatOptions }
		if err = json.Unmarshal(result.Body.Bytes(), &detail); err != nil || detail.Options.Workspace != dir || detail.Options.Model != k.Cfg.Model {
			t.Fatalf("saved options: %+v, %v", detail, err)
		}
		if w = call("POST", "turns", turnRequest{Chat: k.SessionID(), Agent: named.ID, Workspace: a.workdir, Text: "Move"}); w.Code != 409 {
			t.Fatalf("changed locked folder: %d", w.Code)
		}
		assertOK(call("POST", "turns", turnRequest{Chat: k.SessionID(), Agent: named.ID, Text: "Resume"}))
		if kernels[len(kernels)-1].Cfg.WorkDir != dir {
			t.Fatal("resume changed the kernel cwd")
		}
		assertOK(call("DELETE", "agents/"+named.ID, nil))
		agentID, _, err := a.chatProfile(t.Context(), k.SessionID())
		if err != nil || agentID != "base" {
			t.Fatalf("fallback after delete: %s, %v", agentID, err)
		}
		if w = call("DELETE", "agents/base", nil); w.Code != 400 {
			t.Fatalf("deleted base: %d", w.Code)
		}
	})
	// Interrupted steps have only lifecycle events. Completed steps can also
	// contain structured copies of the same calls and results.
	for i, event := range []toroid.Event{
		{Kind: toroid.EventPreToolUse, Payload: toroid.ToolUsePayload{CallID: "a", Name: "bash", Args: `{"command":"pwd"}`}},
		{Kind: toroid.EventPreToolUse, Payload: toroid.ToolUsePayload{CallID: "b", Name: "read", Args: `{"path":"file"}`}},
		{Kind: toroid.EventPostToolUseFailure, Payload: toroid.ToolUseResultPayload{CallID: "b", Error: "missing"}},
		{Kind: toroid.EventPostToolUse, Payload: toroid.ToolUseResultPayload{CallID: "a", Result: "/tmp"}},
		{Kind: toroid.EventTurnCompleted, Payload: toroid.TurnPayload{Content: json.RawMessage(`[{"role":"assistant","parts":[{"kind":"text","text":"Checking."},{"kind":"tool_call","id":"a","name":"bash","arguments":"{}"}]},{"role":"tool","parts":[{"kind":"tool_result","tool_call_id":"a","content":"/tmp"}]}]`)}},
		{Kind: toroid.EventPreToolUse, Payload: toroid.ToolUsePayload{CallID: "c", Name: "bash", Args: "{}"}},
		{Kind: toroid.EventTurnFailed, Payload: toroid.TurnPayload{}},
	} {
		event.Seq = uint64(i + 1)
		if err = store.AppendEvent("old", "old", event); err != nil {
			t.Fatal(err)
		}
	}
	m, err = a.messages(t.Context(), "old")
	if err != nil || len(m) != 5 || m[0].Text != "Checking." || m[1].Call.ID != "a" || *m[1].Call.Output != "/tmp" ||
		m[2].Call.Arguments != `{"path":"file"}` || !m[2].Call.Failed || *m[2].Call.Output != "missing" ||
		m[3].Call.ID != "c" || m[3].Call.Output != nil || m[4].Role != "status" {
		t.Fatalf("tool history: %+v, %v", m, err)
	}
	t.Run("fork each message boundary", func(t *testing.T) {
		source := "old"
		original, err := a.messages(t.Context(), source)
		if err != nil {
			t.Fatal(err)
		}
		for i, message := range original {
			w := call("POST", "chats/"+source+"/fork", map[string]string{"point": message.Point})
			assertOK(w)
			var branch struct{ ID string }
			if err := json.Unmarshal(w.Body.Bytes(), &branch); err != nil {
				t.Fatal(err)
			}
			fork, err := a.messages(t.Context(), branch.ID)
			if err != nil || len(fork) != i+1 {
				t.Fatalf("fork at %s: got %d messages, %v", message.Point, len(fork), err)
			}
			for n, got := range fork {
				want := original[n]
				if got.Role != want.Role || got.Text != want.Text || (want.Call != nil && (got.Call.ID != want.Call.ID || got.Call.Name != want.Call.Name)) {
					t.Fatalf("unexpected message %d in fork %d", n, i)
				}
			}
			history, err := toroid.ReconstructHistory(branch.ID, branch.ID, "", a.workdir, a.base.Settings.Model)
			if err != nil {
				t.Fatal(err)
			}
			if i == 0 && (len(history) != 1 || len(history[0].ToolCalls()) != 0) {
				t.Fatal("fork of assistant text included later calls")
			}
			if i == 1 && (len(history) != 3 || history[2].Role != "tool") {
				t.Fatal("tool result was not replayable")
			}
		}
		source = "recent"
		original, err = a.messages(t.Context(), source)
		if err != nil {
			t.Fatal(err)
		}
		w := call("POST", "chats/recent/fork", map[string]string{"point": original[0].Point})
		assertOK(w)
		var branch struct{ ID string }
		if err = json.Unmarshal(w.Body.Bytes(), &branch); err != nil {
			t.Fatal(err)
		}
		assertOK(call("PUT", "chats/"+branch.ID+"/title", map[string]string{"title": " New branch "}))
		assertOK(call("POST", "turns", turnRequest{Chat: branch.ID, Agent: "base", Text: "Independent prompt"}))
		history := kernels[len(kernels)-1].History
		if len(history) != 3 || history[0].Text() != "First prompt" || history[1].Text() != "Independent prompt" {
			t.Fatal("branch resumed with wrong history")
		}
		unchanged, err := a.messages(t.Context(), source)
		if err != nil || len(unchanged) != len(original) {
			t.Fatal("source changed")
		}
		for _, check := range []struct {
			method, path string
			body         any
			status       int
		}{
			{"POST", "chats/recent/fork", map[string]string{"point": "absent"}, 409},
			{"PUT", "chats/recent/title", map[string]string{"title": "   "}, 400},
			{"PUT", "chats/swarm/title", map[string]string{"title": "Foreign"}, 404},
			{"POST", "chats/swarm/fork", map[string]string{"point": "0.0.user"}, 404},
		} {
			if got := call(check.method, check.path, check.body).Code; got != check.status {
				t.Fatalf("%s: %d", check.path, got)
			}
		}
	})
	t.Run("paired browser survives restart", func(t *testing.T) {
		restarted := app{db: a.db, host: a.host, launchKey: rand.Text(), base: a.base}
		r := httptest.NewRequest("GET", "http://"+a.host+"/api/workspace", nil)
		r.AddCookie(cookie)
		w := httptest.NewRecorder()
		restarted.routes().ServeHTTP(w, r)
		assertOK(w)
	})
	r := httptest.NewRequest("GET", "http://"+a.host+"/api/workspace", nil)
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 401 {
		t.Fatal("workspace accepted an unauthenticated request")
	}
	r.AddCookie(cookie)
	r.Header.Set("Origin", "https://example.org")
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 403 {
		t.Fatal("workspace accepted a foreign origin")
	}
}
