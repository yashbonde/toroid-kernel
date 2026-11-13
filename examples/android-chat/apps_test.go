package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/yashbonde/toroid-kernel/tools"
)

// appsRegistry builds just the app-driving tools against a bridge pointed at a
// fake host, with the given image capability.
func appsRegistry(addr string, imageCapable bool) (*tools.Registry, *appsBridge) {
	b := &appsBridge{host: newToolHost(addr), openWait: 20 * time.Millisecond}
	r := tools.NewRegistry()
	look := lookText
	if imageCapable {
		look = lookImage
	}
	registerAppsTools(r, b, look)
	return r, b
}

func runAppTool(t *testing.T, r *tools.Registry, name, args string) string {
	t.Helper()
	res, err := r.Execute(context.Background(), name, args)
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	if res.IsError {
		t.Fatalf("%s: unexpected error result: %s", name, res.Content)
	}
	return res.Content
}

// TestAppOpenCreatesAndReusesDisplay checks that app_open creates a display
// once (via display_create) and reuses it, forwarding the package and display.
func TestAppOpenCreatesAndReusesDisplay(t *testing.T) {
	h := startFakeHost(t, func(op string, _ json.RawMessage) string {
		switch op {
		case "display_create":
			return okLine(`{"display_id":7}`)
		case "display_list":
			return okLine(`[{"display_id":7,"width":480,"height":854,"dpi":240}]`)
		case "app_open":
			return okLine(`{"opened":"org.lineageos.etar","display_id":7}`)
		}
		return okLine(`{}`)
	})
	r, b := appsRegistry(h.path, true)

	runAppTool(t, r, "app_open", `{"app":"org.lineageos.etar"}`)
	if id, _, _, ok := b.activeDisplay(); !ok || id != 7 {
		t.Fatalf("active display = %d ok=%v, want 7", id, ok)
	}
	runAppTool(t, r, "app_open", `{"app":"org.lineageos.etar"}`)

	reqs := h.requests()
	// Exactly one display_create across two app_opens.
	creates := 0
	for _, q := range reqs {
		if strings.Contains(q, `"op":"display_create"`) {
			creates++
		}
	}
	if creates != 1 {
		t.Fatalf("display_create called %d times, want 1 (%v)", creates, reqs)
	}
	if want := `{"op":"app_open","args":{"display_id":7,"package":"org.lineageos.etar"}}` + "\n"; reqs[1] != want {
		t.Fatalf("app_open forwarded %q\nwant        %q", reqs[1], want)
	}
}

// TestInputToolsForwardActiveDisplay checks tap/swipe/type/key inject the
// active display id and their arguments.
func TestInputToolsForwardActiveDisplay(t *testing.T) {
	h := startFakeHost(t, func(op string, _ json.RawMessage) string {
		if op == "display_create" {
			return okLine(`{"display_id":3}`)
		}
		return okLine(`{"ok":true}`)
	})
	r, _ := appsRegistry(h.path, true)
	runAppTool(t, r, "app_open", `{"app":"Calendar"}`)

	runAppTool(t, r, "tap", `{"x":412,"y":1290}`)
	runAppTool(t, r, "swipe", `{"x1":10,"y1":20,"x2":30,"y2":40}`)
	runAppTool(t, r, "type", `{"text":"Toroid demo"}`)
	runAppTool(t, r, "key", `{"key":"BACK"}`)

	reqs := h.requests()
	wants := []string{
		`{"op":"tap","args":{"display_id":3,"x":412,"y":1290}}`,
		`{"op":"swipe","args":{"display_id":3,"ms":300,"x1":10,"x2":30,"y1":20,"y2":40}}`,
		`{"op":"type","args":{"display_id":3,"text":"Toroid demo"}}`,
		`{"op":"key","args":{"display_id":3,"key":"back"}}`,
	}
	// requests after display_create (0) and app_open (1).
	for i, w := range wants {
		if reqs[2+i] != w+"\n" {
			t.Errorf("request %d = %q\nwant        %q", 2+i, reqs[2+i], w+"\n")
		}
	}
}

// TestAppToolsValidateArgs checks argument validation that never reaches the
// bridge, and the "open an app first" guard.
func TestAppToolsValidateArgs(t *testing.T) {
	h := startFakeHost(t, func(string, json.RawMessage) string { return okLine(`{}`) })
	r, _ := appsRegistry(h.path, true)

	// No display open yet: tap/screen_look must refuse without a round trip.
	for _, tc := range []struct{ name, args string }{
		{"tap", `{"x":1,"y":2}`},
		{"screen_look", `{}`},
		{"type", `{"text":"x"}`},
		{"key", `{"key":"back"}`},
	} {
		res, err := r.Execute(context.Background(), tc.name, tc.args)
		if err != nil {
			t.Fatal(err)
		}
		if !res.IsError || !strings.Contains(res.Content, "app_open first") {
			t.Errorf("%s without a display: %+v", tc.name, res)
		}
	}
	if n := len(h.requests()); n != 0 {
		t.Fatalf("validation reached the bridge %d times, want 0", n)
	}

	// Open a display, then test a bad key and empty text.
	h2 := startFakeHost(t, func(op string, _ json.RawMessage) string {
		if op == "display_create" {
			return okLine(`{"display_id":1}`)
		}
		return okLine(`{}`)
	})
	r2, _ := appsRegistry(h2.path, true)
	runAppTool(t, r2, "app_open", `{"app":"Calendar"}`)
	res, _ := r2.Execute(context.Background(), "key", `{"key":"frobnicate"}`)
	if !res.IsError || !strings.Contains(res.Content, "back, enter, del, tab") {
		t.Errorf("bad key: %+v", res)
	}
	res, _ = r2.Execute(context.Background(), "type", `{"text":""}`)
	if !res.IsError {
		t.Errorf("empty text should error: %+v", res)
	}
}

// TestScreenLookImageGating checks that the screenshot is attached only on a
// vision model, and the text description is always present.
func TestScreenLookImageGating(t *testing.T) {
	jpeg := []byte("\xff\xd8\xff\xe0 fake jpeg")
	reply := okLine(`{"image_base64":"` + base64.StdEncoding.EncodeToString(jpeg) +
		`","width":480,"height":854,"elements":[{"text":"OK","clickable":true,"bounds":[0,0,10,10]}],"text_tree_available":true}`)

	for _, vision := range []bool{true, false} {
		h := startFakeHost(t, func(op string, _ json.RawMessage) string {
			if op == "display_create" {
				return okLine(`{"display_id":1}`)
			}
			if op == "screen_look" {
				return reply
			}
			return okLine(`{}`)
		})
		r, _ := appsRegistry(h.path, vision)
		runAppTool(t, r, "app_open", `{"app":"Calendar"}`)
		res, err := r.Execute(context.Background(), "screen_look", `{}`)
		if err != nil || res.IsError {
			t.Fatalf("screen_look vision=%v: %+v %v", vision, res, err)
		}
		if !strings.Contains(res.Content, "480x854") || !strings.Contains(res.Content, `"OK"`) {
			t.Errorf("vision=%v: text description missing: %q", vision, res.Content)
		}
		if vision {
			if len(res.Files) != 1 || res.Files[0].MediaType != "image/jpeg" || string(res.Files[0].Data) != string(jpeg) {
				t.Errorf("vision model should attach the JPEG, got %d files", len(res.Files))
			}
		} else {
			if len(res.Files) != 0 {
				t.Errorf("text-only model must not attach an image, got %d files", len(res.Files))
			}
		}
	}
}

// TestScreenLookNoTextTree checks the image-only fallback message when the
// bridge reports no text tree (the Android 10 secondary-display case).
func TestScreenLookNoTextTree(t *testing.T) {
	h := startFakeHost(t, func(op string, _ json.RawMessage) string {
		if op == "display_create" {
			return okLine(`{"display_id":1}`)
		}
		return okLine(`{"image_base64":"","width":480,"height":854,"elements":[],"text_tree_available":false}`)
	})
	r, _ := appsRegistry(h.path, true)
	runAppTool(t, r, "app_open", `{"app":"Calendar"}`)
	out := runAppTool(t, r, "screen_look", `{}`)
	if !strings.Contains(out, "No text tree is available") {
		t.Fatalf("expected image-only note, got %q", out)
	}
}

// TestDisplayFrameEndpointAuth checks the live-view endpoint needs the token,
// only serves the active display, and never display 0.
func TestDisplayFrameEndpointAuth(t *testing.T) {
	jpeg := []byte("\xff\xd8\xff\xe0 frame")
	h := startFakeHost(t, func(op string, _ json.RawMessage) string {
		if op == "frame" {
			return okLine(`{"image_base64":"` + base64.StdEncoding.EncodeToString(jpeg) + `","width":480,"height":854}`)
		}
		return okLine(`{}`)
	})
	srv := newServer(t.TempDir(), "tok", 0.5)
	srv.apps = &appsBridge{host: newToolHost(h.path)}
	// Pretend an app is open on display 5.
	srv.apps.mu.Lock()
	srv.apps.display, srv.apps.width, srv.apps.height, srv.apps.hasDisp = 5, 480, 854, true
	srv.apps.mu.Unlock()

	ts := httptest.NewServer(srv.routes())
	defer ts.Close()

	get := func(path, token string) (int, []byte) {
		req, _ := http.NewRequest(http.MethodGet, ts.URL+path, nil)
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, body
	}

	if code, _ := get("/api/display/5/frame.jpg", ""); code != http.StatusUnauthorized {
		t.Errorf("no token: status %d, want 401", code)
	}
	if code, body := get("/api/display/5/frame.jpg", "tok"); code != http.StatusOK || string(body) != string(jpeg) {
		t.Errorf("active display: status %d, %d bytes", code, len(body))
	}
	if code, _ := get("/api/display/9/frame.jpg", "tok"); code != http.StatusNotFound {
		t.Errorf("inactive display: status %d, want 404", code)
	}
	if code, _ := get("/api/display/0/frame.jpg", "tok"); code != http.StatusBadRequest {
		t.Errorf("display 0: status %d, want 400", code)
	}
}

// TestVisionGatingFromCatalog checks visionFor reflects the catalog's Vision
// flag (fallback families here; OpenRouter overrides at refresh).
func TestVisionGatingFromCatalog(t *testing.T) {
	c := newCatalog()
	var visionID, textID string
	for _, m := range c.list() {
		if m.Vision && visionID == "" {
			visionID = m.ID
		}
		if !m.Vision && textID == "" {
			textID = m.ID
		}
	}
	if visionID == "" || textID == "" {
		t.Skip("catalog has no mix of vision and text-only models to test")
	}
	if !c.visionFor(visionID) {
		t.Errorf("%s should be vision", visionID)
	}
	if c.visionFor(textID) {
		t.Errorf("%s should be text-only", textID)
	}
	if c.visionFor("llmgateway/not-a-real-model") {
		t.Error("unknown model must be treated as text-only")
	}
}

// TestHomeKeyRefused checks home never reaches the bridge: the system policy
// intercepts it and could act on the user's screen.
func TestHomeKeyRefused(t *testing.T) {
	h := startFakeHost(t, func(op string, _ json.RawMessage) string {
		if op == "display_create" {
			return okLine(`{"display_id":2}`)
		}
		return okLine(`{}`)
	})
	r, _ := appsRegistry(h.path, true)
	runAppTool(t, r, "app_open", `{"app":"Calendar"}`)
	before := len(h.requests())
	res, _ := r.Execute(context.Background(), "key", `{"key":"home"}`)
	if !res.IsError || !strings.Contains(res.Content, "app_close") {
		t.Fatalf("home: %+v", res)
	}
	if len(h.requests()) != before {
		t.Fatal("home reached the bridge")
	}
}

// TestScreenLookGridFlag checks the grid is asked for on a vision model by
// default, can be turned off, and is never asked for on a text-only model.
func TestScreenLookGridFlag(t *testing.T) {
	for _, tc := range []struct {
		vision bool
		args   string
		want   string
	}{
		{true, `{}`, `"grid":true`},
		{true, `{"grid":false}`, `"grid":false`},
		{false, `{}`, `"grid":false`},
	} {
		h := startFakeHost(t, func(op string, _ json.RawMessage) string {
			if op == "display_create" {
				return okLine(`{"display_id":1}`)
			}
			return okLine(`{"image_base64":"","width":480,"height":854,"elements":[],"grid":true}`)
		})
		r, _ := appsRegistry(h.path, tc.vision)
		runAppTool(t, r, "app_open", `{"app":"Calendar"}`)
		runAppTool(t, r, "screen_look", tc.args)
		reqs := h.requests()
		if got := reqs[len(reqs)-1]; !strings.Contains(got, tc.want) {
			t.Errorf("vision=%v args=%s: forwarded %q, want %s", tc.vision, tc.args, got, tc.want)
		}
	}
}

// TestAppSummaryUsesLabel checks tool lines name the app, not its package.
func TestAppSummaryUsesLabel(t *testing.T) {
	got, _ := appSummary("app_open", `{"opened":"org.lineageos.etar","label":"Calendar","display_id":3}`)
	if got != "✓ opened Calendar (in background)" {
		t.Fatalf("summary = %q", got)
	}
	got, _ = appSummary("app_open", `{"opened":"org.lineageos.etar"}`)
	if got != "✓ opened org.lineageos.etar (in background)" {
		t.Fatalf("fallback summary = %q", got)
	}
}

// TestScreenLookReportsClosedApp checks a display with no focused window is
// reported as closed, so a stale last frame is not mistaken for the app.
func TestScreenLookReportsClosedApp(t *testing.T) {
	res, _ := screenLookResult(json.RawMessage(`{"width":480,"height":854,"focused_window":null}`), lookImage)
	if !strings.Contains(res.Content, "the app has closed") {
		t.Fatalf("closed app not reported: %q", res.Content)
	}
	res, _ = screenLookResult(json.RawMessage(`{"width":480,"height":854,"focused_window":"org.lineageos.etar/com.android.calendar.AllInOneActivity"}`), lookImage)
	if !strings.Contains(res.Content, "In front: org.lineageos.etar/") {
		t.Fatalf("focused window not reported: %q", res.Content)
	}
	res, _ = screenLookResult(json.RawMessage(`{"width":480,"height":854}`), lookImage)
	if strings.Contains(res.Content, "closed") || strings.Contains(res.Content, "In front") {
		t.Fatalf("unknown focus should say nothing: %q", res.Content)
	}
}

// TestBatchedLookWaitsForOpen checks a screen_look run concurrently with
// app_open (as the kernel runs one batch) waits for the open instead of
// failing.
func TestBatchedLookWaitsForOpen(t *testing.T) {
	h := startFakeHost(t, func(op string, _ json.RawMessage) string {
		switch op {
		case "display_create":
			time.Sleep(100 * time.Millisecond) // a slow open
			return okLine(`{"display_id":4}`)
		case "screen_look":
			return okLine(`{"image_base64":"","width":480,"height":854}`)
		}
		return okLine(`{"opened":"org.lineageos.etar","label":"Calendar"}`)
	})
	b := &appsBridge{host: newToolHost(h.path), openWait: 500 * time.Millisecond}
	r := tools.NewRegistry()
	registerAppsTools(r, b, lookImage)
	done := make(chan llmResult, 1)
	go func() {
		res, err := r.Execute(context.Background(), "app_open", `{"app":"Calendar"}`)
		done <- llmResult{res.IsError, res.Content, err}
	}()
	time.Sleep(10 * time.Millisecond) // screen_look lands while app_open is in flight
	res, err := r.Execute(context.Background(), "screen_look", `{}`)
	if err != nil || res.IsError {
		t.Fatalf("batched screen_look failed: %+v %v", res, err)
	}
	if o := <-done; o.err != nil || o.isErr {
		t.Fatalf("app_open: %+v", o)
	}
}

type llmResult struct {
	isErr   bool
	content string
	err     error
}

// TestTypeReplaceForwarded checks replace reaches the bridge only when set.
func TestTypeReplaceForwarded(t *testing.T) {
	h := startFakeHost(t, func(op string, _ json.RawMessage) string {
		if op == "display_create" {
			return okLine(`{"display_id":1}`)
		}
		return okLine(`{}`)
	})
	r, _ := appsRegistry(h.path, true)
	runAppTool(t, r, "app_open", `{"app":"Calendar"}`)
	runAppTool(t, r, "type", `{"text":"Toroid demo 2","replace":true}`)
	runAppTool(t, r, "type", `{"text":"x"}`)
	reqs := h.requests()
	if got := reqs[len(reqs)-2]; got != `{"op":"type","args":{"display_id":1,"replace":true,"text":"Toroid demo 2"}}`+"\n" {
		t.Errorf("replace type forwarded %q", got)
	}
	if got := reqs[len(reqs)-1]; strings.Contains(got, "replace") {
		t.Errorf("plain type forwarded replace: %q", got)
	}
}

// TestAppOpenURL checks that app_open with a url goes to the bridge's
// open_url op (never app_open), adds https:// to a bare host, passes a named
// browser through, refuses other schemes, and names the page in the tool line.
func TestAppOpenURL(t *testing.T) {
	h := startFakeHost(t, func(op string, args json.RawMessage) string {
		switch op {
		case "display_create":
			return okLine(`{"display_id":5}`)
		case "display_list":
			return okLine(`[{"display_id":5}]`)
		case "app_list":
			return okLine(`[{"label":"Browser","package":"org.lineageos.jelly"}]`)
		case "open_url":
			var a struct {
				URL string `json:"url"`
			}
			_ = json.Unmarshal(args, &a)
			return okLine(`{"opened":"org.lineageos.jelly","label":"Browser","url":` + strconvQuote(a.URL) + `,"display_id":5,"ready":true}`)
		}
		return okLine(`{}`)
	})
	r, b := appsRegistry(h.path, false)

	out := runAppTool(t, r, "app_open", `{"url":"news.ycombinator.com"}`)
	if id, _, _, ok := b.activeDisplay(); !ok || id != 5 {
		t.Fatalf("active display = %d ok=%v, want 5", id, ok)
	}
	if line, _ := appSummary("app_open", out); line != "✓ opened news.ycombinator.com (in background)" {
		t.Errorf("tool line = %q", line)
	}
	runAppTool(t, r, "app_open", `{"app":"Browser","url":"https://duckduckgo.com/?q=pune+weather"}`)

	reqs := h.requests()
	var opens []string
	for _, q := range reqs {
		if strings.Contains(q, `"op":"app_open"`) {
			t.Fatalf("a url must not use app_open: %q", q)
		}
		if strings.Contains(q, `"op":"open_url"`) {
			opens = append(opens, strings.TrimSpace(q))
		}
	}
	want := []string{
		`{"op":"open_url","args":{"display_id":5,"url":"https://news.ycombinator.com"}}`,
		`{"op":"open_url","args":{"display_id":5,"package":"org.lineageos.jelly","url":"https://duckduckgo.com/?q=pune+weather"}}`,
	}
	if len(opens) != 2 || opens[0] != want[0] || opens[1] != want[1] {
		t.Fatalf("open_url requests = %q\nwant %q", opens, want)
	}

	for _, bad := range []string{`{"url":"file:///sdcard/x.html"}`, `{"url":"javascript:alert(1)"}`, `{}`} {
		res, _ := r.Execute(context.Background(), "app_open", bad)
		if !res.IsError {
			t.Errorf("app_open %s should fail: %+v", bad, res)
		}
	}
}

func strconvQuote(s string) string { b, _ := json.Marshal(s); return string(b) }

// TestScreenLookUnsettled checks that a capture taken while the screen was
// still redrawing says so, and a settled one does not.
func TestScreenLookUnsettled(t *testing.T) {
	for _, tc := range []struct {
		settled string
		want    bool
	}{{`,"settled":false`, true}, {`,"settled":true`, false}, {``, false}} {
		raw := json.RawMessage(`{"image_base64":"","width":480,"height":854` + tc.settled + `}`)
		res, err := screenLookResult(raw, lookText)
		if err != nil {
			t.Fatal(err)
		}
		if got := strings.Contains(res.Content, "still changing"); got != tc.want {
			t.Errorf("settled%s: note shown = %v, want %v\n%s", tc.settled, got, tc.want, res.Content)
		}
	}
}

// TestGuardNote checks that only the guards that end a reply add a note.
func TestGuardNote(t *testing.T) {
	if guardNote("max_iter") == "" || guardNote("loop_guard") == "" {
		t.Fatal("max_iter and loop_guard must explain the stop")
	}
	for _, n := range []string{"inspection", "validation", "completion", ""} {
		if guardNote(n) != "" {
			t.Errorf("guard %q should add no note", n)
		}
	}
}
