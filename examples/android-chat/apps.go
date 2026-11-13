package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	neturl "net/url"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/yashbonde/toroid-kernel/llm"
	"github.com/yashbonde/toroid-kernel/tools"
)

// Apps: the agent's third tool group. They let the agent use any phone app, but
// only on an off-screen virtual display — never the user's real screen. The
// Toroid System bridge (a separate, system-uid app) owns the displays and does
// the work; these tools forward JSON lines to it over the abstract Unix socket
// @toroid-system, exactly as the to-do and file tools talk to @toroidchat-tools.
//
// The bridge speaks in display ids, but the agent should not have to juggle
// them: appsBridge keeps a single active display per chat server, created on
// the first app_open and reused after, so the model's tools (screen_look, tap,
// …) need no display argument. app_close releases it.

// systemSocket is the bridge's address (abstract namespace: leading "@").
const systemSocket = "@toroid-system"

// errBridgeDown is returned when the bridge cannot be reached.
var errBridgeDown = errors.New("the phone's app bridge is not reachable (is the Toroid System service running?)")

// appsBridge is the Go end of the bridge protocol plus the active-display
// bookkeeping shared across the server (the live-view endpoint reads it too).
type appsBridge struct {
	host *toolHost

	mu      sync.Mutex
	display int // active display id, or 0 when none
	width   int
	height  int
	hasDisp bool

	// A model often calls app_open and screen_look (or tap) in one batch, and
	// the kernel runs them concurrently. openMu is held for a whole app_open
	// and opening counts the ones in flight, so the other tools wait for it
	// instead of failing with "open an app first".
	openMu   sync.Mutex
	opening  atomic.Int32
	openWait time.Duration // how long a tool waits for an app_open to start
}

func newAppsBridge(addr string) *appsBridge {
	return &appsBridge{host: &toolHost{addr: addr, timeout: toolTimeout}, openWait: 1500 * time.Millisecond}
}

// call forwards an op to the bridge, turning a dial failure into a clear,
// bridge-specific message.
func (b *appsBridge) call(ctx context.Context, op string, args any) (json.RawMessage, error) {
	raw, err := b.host.call(ctx, op, args)
	if err != nil {
		// A dial failure surfaces as the to-do host's generic message; replace
		// it so the model names the right service.
		if errors.Is(err, errToolHostDown) || errors.Is(err, errFileHostDown) {
			return nil, errBridgeDown
		}
		return nil, err
	}
	return raw, nil
}

// activeDisplay returns the current display id and its size, or ok=false when
// no app has been opened yet.
func (b *appsBridge) activeDisplay() (id, w, h int, ok bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.display, b.width, b.height, b.hasDisp
}

// ensureDisplay returns the active display, creating one on the bridge when
// there is none or the cached one is gone. The cached id can go stale between
// turns: the bridge releases a display after 5 minutes idle (or when the chat
// app dies), so a reused id is confirmed against the bridge's display_list
// before it is handed back. Callers hold no lock.
func (b *appsBridge) ensureDisplay(ctx context.Context) (int, error) {
	b.mu.Lock()
	cached, has := b.display, b.hasDisp
	b.mu.Unlock()
	if has {
		if b.displayExists(ctx, cached) {
			return cached, nil
		}
		b.clearDisplay() // the bridge no longer has it; make a fresh one
	}

	raw, err := b.call(ctx, "display_create", map[string]any{"width": 480, "height": 854, "dpi": 240})
	if err != nil {
		return 0, err
	}
	var res struct {
		DisplayID int `json:"display_id"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return 0, fmt.Errorf("bad display_create reply: %v", err)
	}
	b.mu.Lock()
	b.display, b.width, b.height, b.hasDisp = res.DisplayID, 480, 854, true
	b.mu.Unlock()
	return res.DisplayID, nil
}

// displayExists reports whether the bridge still has display id.
func (b *appsBridge) displayExists(ctx context.Context, id int) bool {
	raw, err := b.call(ctx, "display_list", struct{}{})
	if err != nil {
		return false
	}
	var list []struct {
		DisplayID int `json:"display_id"`
	}
	if json.Unmarshal(raw, &list) != nil {
		return false
	}
	for _, d := range list {
		if d.DisplayID == id {
			return true
		}
	}
	return false
}

// requireDisplay returns the active display id, waiting for an app_open that
// is in flight (or about to start, in the same batch), else an error telling
// the model to open an app first.
func (b *appsBridge) requireDisplay(ctx context.Context) (int, error) {
	deadline := time.Now().Add(b.openWait)
	for {
		if b.opening.Load() > 0 {
			b.openMu.Lock() // wait for the app_open to finish
			b.openMu.Unlock()
		}
		if id, w, h, ok := b.activeDisplay(); ok {
			// A later turn can drive an app opened in an earlier one without
			// calling app_open, so every display tool shows the live view.
			if cs := streamFrom(ctx); cs != nil {
				cs.displayActive(id, w, h)
			}
			return id, nil
		}
		if time.Now().After(deadline) || ctx.Err() != nil {
			return 0, errors.New("no app is open yet; call app_open first")
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// clearDisplay forgets the active display (after app_close or when the bridge
// reports it gone).
func (b *appsBridge) clearDisplay() {
	b.mu.Lock()
	b.display, b.width, b.height, b.hasDisp = 0, 0, 0, false
	b.mu.Unlock()
}

// frameJPEG fetches the latest JPEG for the active display, for the live-view
// endpoint. It is cheap enough to call a few times a second.
func (b *appsBridge) frameJPEG(ctx context.Context, displayID int) ([]byte, error) {
	raw, err := b.call(ctx, "frame", map[string]any{"display_id": displayID})
	if err != nil {
		return nil, err
	}
	var res struct {
		Image string `json:"image_base64"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return nil, fmt.Errorf("bad frame reply: %v", err)
	}
	return base64.StdEncoding.DecodeString(res.Image)
}

// --- tool argument types ---

type appOpenArgs struct {
	App string `json:"app,omitempty" jsonschema:"description=The app to open: its name as app_list shows it (e.g. Calendar) or its package id (e.g. org.lineageos.etar). With url it names the browser; leave it out to use the default browser"`
	URL string `json:"url,omitempty" jsonschema:"description=Optional: an http(s) web address to open in the browser, e.g. https://news.ycombinator.com or a search such as https://duckduckgo.com/?q=minimal+dark+wallpapers"`
}

type tapArgs struct {
	X int `json:"x" jsonschema:"description=X in display pixels (0 is the left edge),minimum=0"`
	Y int `json:"y" jsonschema:"description=Y in display pixels (0 is the top edge),minimum=0"`
}

type swipeArgs struct {
	X1 int `json:"x1" jsonschema:"description=Start X in display pixels,minimum=0"`
	Y1 int `json:"y1" jsonschema:"description=Start Y in display pixels,minimum=0"`
	X2 int `json:"x2" jsonschema:"description=End X in display pixels,minimum=0"`
	Y2 int `json:"y2" jsonschema:"description=End Y in display pixels,minimum=0"`
	Ms int `json:"ms,omitempty" jsonschema:"description=Duration in milliseconds (default 300)"`
}

type typeArgs struct {
	Text    string `json:"text" jsonschema:"description=Text to type into the focused field,minLength=1"`
	Replace bool   `json:"replace,omitempty" jsonschema:"description=Select everything already in the field first so the text replaces it (instead of pressing del many times)"`
}

type keyArgs struct {
	Key string `json:"key" jsonschema:"description=A key: back, enter, del or tab"`
}

// screenLookArgs lets the model turn the coordinate grid off (it is on by
// default: it is what lets a cheap model hit small targets).
type screenLookArgs struct {
	Grid *bool `json:"grid,omitempty" jsonschema:"description=Draw a labelled coordinate grid on the screenshot (default true): faint lines every 50 px and coordinate labels every 100 px"`
}

// screenAskArgs is screen_look's arguments with the vision delegate on: the
// grid stays on (the vision model reads coordinates off it), and a question
// is passed through to the vision model.
type screenAskArgs struct {
	Question string `json:"question,omitempty" jsonschema:"description=Optional: what you need to know about the screen, e.g. where is the start date field? or is the title filled in? The vision model answers it first"`
}

// lookMode is how screen_look shows the screen to the chat model.
type lookMode int

const (
	lookText     lookMode = iota // text description only (a text-only model, no delegate)
	lookImage                    // the screenshot itself (a vision model, no delegate)
	lookDelegate                 // the screenshot, described by the kernel's vision delegate
)

// screenVisionPrompt is the vision delegate's system prompt for screen_look:
// a compact, structured reading of the screen with coordinates taken off the
// grid, so a text-only chat model can plan and tap from it.
const screenVisionPrompt = `You are the eyes of a phone-automation agent that cannot see. The attached image is a screenshot of an Android app on a background display; its size in pixels is in the tool's text output. A coordinate grid is drawn on it: faint lines every 50 px and an "x,y" label at every 100 px crossing. The agent will tap exactly the pixel coordinates you give, so read them off the grid carefully.

Reply in this plain-text format and nothing else:
ANSWER: <a direct answer to the question, with coordinates; "no question" if none was asked>
SCREEN: <app and screen, e.g. "Calendar: new event form"; name any open dialog, menu, picker or keyboard>
ELEMENTS: one line per visible control or text, top to bottom, left to right:
- <kind> "<exact label or text>" @ x,y [l,t,r,b] <state>
  kind: button, icon, field, text, tab, checkbox, switch, item, menu-item, date, time or other. x,y is the centre, [l,t,r,b] the bounds. state: selected, checked, disabled, focused, empty or nothing. Give an unlabelled icon a short description in the quotes ("+ add", "back arrow", "three-dot menu", "save tick").
NOTES: <anything that matters for the agent's plan: errors, loading, a value that differs from what it intended, content cut off at an edge or reachable by scrolling>

Rules: transcribe text exactly, including the current values of fields; never invent elements; ignore the grid lines and labels themselves; if a position is uncertain, say so. Be compact: list what the agent can act on or needs to read, at most about 30 element lines, and merge long lists of controls (e.g. a month grid of days) into one line with the first and last positions.
Content to read is the exception: on a web page, article, feed or list of results, transcribe every readable item in full, in order, with its number if it shows one (e.g. "3. <title> (site)"), however many there are; the agent is often asked to report them. Say which item is cut off at the top or bottom edge.
An on-screen keyboard is one line: its top edge and the position of its action key (bottom right, e.g. search, go, enter or a tick); never list its keys. Say if it covers a field the agent is typing into.
A blank or half-drawn page is still loading: say so in NOTES.`

// validKeys are the keys the bridge will send. home is not among them: the
// system policy intercepts it and could act on the user's screen.
var validKeys = map[string]bool{"back": true, "enter": true, "del": true, "tab": true}

// --- tool registration ---

// registerAppsTools adds the app-driving tools. look decides whether
// screen_look attaches the JPEG: for a vision chat model, or for the kernel's
// vision delegate, which describes it to the chat model. cs signalling the
// live view runs through the request context, like the other tools' events.
func registerAppsTools(r *tools.Registry, b *appsBridge, look lookMode) {
	r.Register(&tools.ToolDef{
		Name:        "app_list",
		Description: "List the apps that can be opened (label and package id), so you can find e.g. \"Calendar\" -> org.lineageos.etar.",
		Handler: llm.NewTool("app_list", "List launchable apps.", func(ctx context.Context, _ struct{}) (llm.ToolResult, error) {
			raw, err := b.call(ctx, "app_list", struct{}{})
			if err != nil {
				return llm.NewErrorResult(err.Error()), nil
			}
			return llm.NewTextResult(string(raw)), nil
		}),
	})

	r.Register(&tools.ToolDef{
		Name: "app_open",
		Description: "Open an app on a background virtual display (never the user's screen) and make it the active app for the other app_* tools. " +
			"Creates the display on first use and reuses it after. Then use screen_look to see it. " +
			"For a web page or a web search, pass url: the page opens straight in the browser, so never type an address into the browser's address bar.",
		Handler: llm.NewTool("app_open", "Open an app, or a web page, on the background display.", func(ctx context.Context, a appOpenArgs) (llm.ToolResult, error) {
			app, url := strings.TrimSpace(a.App), strings.TrimSpace(a.URL)
			if app == "" && url == "" {
				return llm.NewErrorResult("app or url is required"), nil
			}
			if url != "" {
				var ok bool
				if url, ok = webURL(url); !ok {
					return llm.NewErrorResult("url must be an http(s) web address, e.g. https://news.ycombinator.com"), nil
				}
			}
			b.opening.Add(1)
			b.openMu.Lock()
			defer func() { b.openMu.Unlock(); b.opening.Add(-1) }()
			id, err := b.ensureDisplay(ctx)
			if err != nil {
				return llm.NewErrorResult(err.Error()), nil
			}
			if url != "" {
				return b.openURL(ctx, id, app, url), nil
			}
			raw, err := b.call(ctx, "app_open", map[string]any{"package": app, "display_id": id})
			if err != nil {
				// A human name needs resolving to a package; the bridge only
				// takes packages, so try app_list when the first attempt fails.
				if pkg, ok := b.resolvePackage(ctx, app); ok {
					raw, err = b.call(ctx, "app_open", map[string]any{"package": pkg, "display_id": id})
				}
				if err != nil {
					return llm.NewErrorResult(err.Error()), nil
				}
			}
			if cs := streamFrom(ctx); cs != nil {
				_, w, h, _ := b.activeDisplay()
				cs.displayActive(id, w, h)
			}
			return llm.NewTextResult(string(raw)), nil
		}),
	})

	look1 := func(ctx context.Context, grid bool) (json.RawMessage, error) {
		id, err := b.requireDisplay(ctx)
		if err != nil {
			return nil, err
		}
		return b.call(ctx, "screen_look", map[string]any{"display_id": id, "grid": grid})
	}
	if look == lookDelegate {
		r.Register(&tools.ToolDef{
			Name: "screen_look",
			Description: "Look at the active app's background screen. A vision model reads the screenshot and returns a description: each visible control and text with its centre x,y and bounds in display pixels, plus an answer to your question if you ask one. " +
				"Always look before tapping, and look again after to confirm.",
			Handler: llm.NewTool("screen_look", "Look at the active background screen.", func(ctx context.Context, a screenAskArgs) (llm.ToolResult, error) {
				raw, err := look1(ctx, true)
				if err != nil {
					return llm.NewErrorResult(err.Error()), nil
				}
				res, err := screenLookResult(raw, lookDelegate)
				res.Question = strings.TrimSpace(a.Question)
				return res, err
			}),
		})
	} else {
		r.Register(&tools.ToolDef{
			Name: "screen_look",
			Description: "Look at the active app's background screen: returns a text description of what is visible (texts, clickable elements and their pixel bounds when available) and, on a vision model, a screenshot. " +
				"Always look before tapping, and look again after to confirm.",
			Handler: llm.NewTool("screen_look", "Look at the active background screen.", func(ctx context.Context, a screenLookArgs) (llm.ToolResult, error) {
				raw, err := look1(ctx, look == lookImage && (a.Grid == nil || *a.Grid))
				if err != nil {
					return llm.NewErrorResult(err.Error()), nil
				}
				return screenLookResult(raw, look)
			}),
		})
	}

	r.Register(&tools.ToolDef{
		Name:        "tap",
		Description: "Tap the active background screen at (x, y) in display pixels. Look first to find the coordinates.",
		Handler: llm.NewTool("tap", "Tap the background screen.", func(ctx context.Context, a tapArgs) (llm.ToolResult, error) {
			id, err := b.requireDisplay(ctx)
			if err != nil {
				return llm.NewErrorResult(err.Error()), nil
			}
			if _, err := b.call(ctx, "tap", map[string]any{"display_id": id, "x": a.X, "y": a.Y}); err != nil {
				return llm.NewErrorResult(err.Error()), nil
			}
			return llm.NewTextResult(fmt.Sprintf(`{"tapped":[%d,%d]}`, a.X, a.Y)), nil
		}),
	})

	r.Register(&tools.ToolDef{
		Name: "swipe",
		Description: "Swipe the active background screen from (x1, y1) to (x2, y2) over ms milliseconds. Use to scroll. " +
			"A swipe is a drag: the content moves by about the swipe's length and stops, with no fling. " +
			"To scroll down about half a screen, swipe up from (240, 650) to (240, 250); to scroll back up, the reverse. " +
			"Start away from the edges, and look after each scroll.",
		Handler: llm.NewTool("swipe", "Swipe the background screen.", func(ctx context.Context, a swipeArgs) (llm.ToolResult, error) {
			id, err := b.requireDisplay(ctx)
			if err != nil {
				return llm.NewErrorResult(err.Error()), nil
			}
			ms := a.Ms
			if ms <= 0 {
				ms = 300
			}
			if _, err := b.call(ctx, "swipe", map[string]any{"display_id": id, "x1": a.X1, "y1": a.Y1, "x2": a.X2, "y2": a.Y2, "ms": ms}); err != nil {
				return llm.NewErrorResult(err.Error()), nil
			}
			return llm.NewTextResult(fmt.Sprintf(`{"swiped":[[%d,%d],[%d,%d]]}`, a.X1, a.Y1, a.X2, a.Y2)), nil
		}),
	})

	r.Register(&tools.ToolDef{
		Name:        "type",
		Description: "Type text into the focused field on the active background screen. Tap the field first so it has focus. Never type passwords or payment details.",
		Handler: llm.NewTool("type", "Type text on the background screen.", func(ctx context.Context, a typeArgs) (llm.ToolResult, error) {
			if a.Text == "" {
				return llm.NewErrorResult("text is required"), nil
			}
			id, err := b.requireDisplay(ctx)
			if err != nil {
				return llm.NewErrorResult(err.Error()), nil
			}
			args := map[string]any{"display_id": id, "text": a.Text}
			if a.Replace {
				args["replace"] = true
			}
			if _, err := b.call(ctx, "type", args); err != nil {
				return llm.NewErrorResult(err.Error()), nil
			}
			return llm.NewTextResult(fmt.Sprintf(`{"typed":%q}`, a.Text)), nil
		}),
	})

	r.Register(&tools.ToolDef{
		Name: "key",
		Description: "Press a key on the active background screen: back, enter, del or tab. " +
			"enter submits a web page's search box, but some fields ignore it (a browser's address bar does): then tap the keyboard's own action key, bottom right. " +
			"Keys are only sent when the bridge can confirm they reach this app and never the user's screen; " +
			"if one is refused, tap the app's own on-screen control instead (its back arrow, X or Cancel).",
		Handler: llm.NewTool("key", "Press a hardware key on the background screen.", func(ctx context.Context, a keyArgs) (llm.ToolResult, error) {
			key := strings.TrimSpace(strings.ToLower(a.Key))
			if !validKeys[key] {
				if key == "home" {
					return llm.NewErrorResult("home is not available on a background display; use app_close, then app_open"), nil
				}
				return llm.NewErrorResult("key must be one of: back, enter, del, tab"), nil
			}
			id, err := b.requireDisplay(ctx)
			if err != nil {
				return llm.NewErrorResult(err.Error()), nil
			}
			if _, err := b.call(ctx, "key", map[string]any{"display_id": id, "key": key}); err != nil {
				return llm.NewErrorResult(err.Error()), nil
			}
			return llm.NewTextResult(fmt.Sprintf(`{"key":%q}`, key)), nil
		}),
	})

	r.Register(&tools.ToolDef{
		Name:        "app_close",
		Description: "Close the background app and release its display. Use when you are done using apps.",
		Handler: llm.NewTool("app_close", "Close the background app and release its display.", func(ctx context.Context, _ struct{}) (llm.ToolResult, error) {
			id, _, _, ok := b.activeDisplay()
			if !ok {
				return llm.NewTextResult(`{"closed":false,"note":"no app was open"}`), nil
			}
			_, err := b.call(ctx, "display_release", map[string]any{"display_id": id})
			b.clearDisplay()
			if err != nil {
				return llm.NewErrorResult(err.Error()), nil
			}
			return llm.NewTextResult(`{"closed":true}`), nil
		}),
	})
}

// appOps is the set of op names the apps tools use, for routing tool-line text.
var appOps = map[string]bool{
	"app_list": true, "app_open": true, "screen_look": true, "tap": true,
	"swipe": true, "type": true, "key": true, "app_close": true,
}

// appStartText is the "… doing" line shown while an app tool runs.
func appStartText(name string) (string, bool) {
	switch name {
	case "app_list":
		return "… listing apps", true
	case "app_open":
		return "… opening an app in the background", true
	case "screen_look":
		return "… looking at the screen", true
	case "tap":
		return "… tapping", true
	case "swipe":
		return "… swiping", true
	case "type":
		return "… typing", true
	case "key":
		return "… pressing a key", true
	case "app_close":
		return "… closing the app", true
	}
	return "", false
}

// appSummary is the one-line summary of a finished app tool call, e.g.
// "✓ opened Calendar (in background)" or "✓ tapped (412, 1290)".
func appSummary(name, result string) (string, bool) {
	if !appOps[name] {
		return "", false
	}
	switch name {
	case "app_list":
		return "✓ listed apps", true
	case "app_open":
		var r struct {
			Opened string `json:"opened"`
			Label  string `json:"label"`
			URL    string `json:"url"`
		}
		_ = json.Unmarshal([]byte(result), &r)
		if r.URL != "" { // a web page: name the page, not the browser
			page := strings.TrimPrefix(strings.TrimPrefix(r.URL, "https://"), "http://")
			return "✓ opened " + oneLine(page, 60) + " (in background)", true
		}
		label := r.Label
		if label == "" {
			label = r.Opened
		}
		if label == "" {
			label = "app"
		}
		return "✓ opened " + oneLine(label, 60) + " (in background)", true
	case "screen_look":
		return "✓ looked at the screen", true
	case "tap":
		var r struct {
			Tapped []int `json:"tapped"`
		}
		if json.Unmarshal([]byte(result), &r) == nil && len(r.Tapped) == 2 {
			return fmt.Sprintf("✓ tapped (%d, %d)", r.Tapped[0], r.Tapped[1]), true
		}
		return "✓ tapped", true
	case "swipe":
		return "✓ swiped", true
	case "type":
		var r struct {
			Typed string `json:"typed"`
		}
		if json.Unmarshal([]byte(result), &r) == nil && r.Typed != "" {
			return "✓ typed: " + oneLine(r.Typed, 40), true
		}
		return "✓ typed", true
	case "key":
		var r struct {
			Key string `json:"key"`
		}
		_ = json.Unmarshal([]byte(result), &r)
		if r.Key != "" {
			return "✓ pressed " + r.Key, true
		}
		return "✓ pressed a key", true
	case "app_close":
		return "✓ closed the app (in background)", true
	}
	return "", false
}

// schemeRe matches a URL scheme such as "https:" or "javascript:" (a bare
// host never has one: "news.ycombinator.com/x" has a dot before any colon).
var schemeRe = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9+-]*:`)

// webURL checks that u is an http(s) address, adding https:// to a bare host
// such as news.ycombinator.com. Any other scheme (file:, javascript:,
// intent:, ...) is refused; the bridge checks again.
func webURL(u string) (string, bool) {
	if strings.ContainsAny(u, " \t\n") {
		return "", false
	}
	if m := schemeRe.FindString(u); m != "" {
		scheme := strings.ToLower(m)
		if scheme != "http:" && scheme != "https:" {
			return "", false
		}
	} else {
		u = "https://" + u
	}
	p, err := neturl.Parse(u)
	if err != nil || p.Host == "" {
		return "", false
	}
	return u, true
}

// openURL opens a web address in a browser on display id through the bridge's
// open_url op (ACTION_VIEW), so the agent never drives the address bar. app,
// when set, names the browser. Callers hold openMu.
func (b *appsBridge) openURL(ctx context.Context, id int, app, url string) llm.ToolResult {
	args := map[string]any{"url": url, "display_id": id}
	if app != "" {
		pkg := app
		if p, ok := b.resolvePackage(ctx, app); ok {
			pkg = p
		}
		args["package"] = pkg
	}
	raw, err := b.call(ctx, "open_url", args)
	if err != nil {
		return llm.NewErrorResult(err.Error())
	}
	if cs := streamFrom(ctx); cs != nil {
		_, w, h, _ := b.activeDisplay()
		cs.displayActive(id, w, h)
	}
	return llm.NewTextResult(string(raw))
}

// resolvePackage looks up a human app name in app_list and returns its package.
func (b *appsBridge) resolvePackage(ctx context.Context, name string) (string, bool) {
	raw, err := b.call(ctx, "app_list", struct{}{})
	if err != nil {
		return "", false
	}
	var apps []struct {
		Label   string `json:"label"`
		Package string `json:"package"`
	}
	if json.Unmarshal(raw, &apps) != nil {
		return "", false
	}
	for _, a := range apps {
		if strings.EqualFold(a.Label, name) {
			return a.Package, true
		}
	}
	return "", false
}

// screenLookResult turns the bridge's screen_look reply into a tool result: the
// text description always, and the JPEG as an image for a vision model or the
// vision delegate.
func screenLookResult(raw json.RawMessage, look lookMode) (llm.ToolResult, error) {
	imageCapable := look != lookText
	var shot struct {
		Image    string          `json:"image_base64"`
		Width    int             `json:"width"`
		Height   int             `json:"height"`
		Elements json.RawMessage `json:"elements"`
		TreeOK   bool            `json:"text_tree_available"`
		Grid     bool            `json:"grid"`
		// Focused is the window in front on the display ("pkg/Activity");
		// JSON null means none (the app closed), absent means unknown.
		Focused json.RawMessage `json:"focused_window"`
		// Settled is false when the screen was still redrawing (a page
		// loading, an animation) when it was captured; absent means unknown.
		Settled *bool `json:"settled"`
	}
	if err := json.Unmarshal(raw, &shot); err != nil {
		return llm.NewErrorResult("bad screen_look reply: " + err.Error()), nil
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "Background screen %dx%d px.\n", shot.Width, shot.Height)
	switch f := strings.TrimSpace(string(shot.Focused)); {
	case f == "null":
		sb.WriteString("No app window is in front on the background display: the app has closed, and the screenshot may be its last frame. Call app_open to open it again.\n")
	case strings.HasPrefix(f, `"`):
		var w string
		if json.Unmarshal(shot.Focused, &w) == nil && w != "" {
			fmt.Fprintf(&sb, "In front: %s.\n", w)
		}
	}
	if shot.Settled != nil && !*shot.Settled {
		sb.WriteString("The screen was still changing when captured (a page loading or an animation): if something you expect is missing or blank, look again.\n")
	}
	if shot.TreeOK && len(shot.Elements) > 0 && string(shot.Elements) != "[]" {
		sb.WriteString("Visible elements (text/desc, clickable, bounds [l,t,r,b]):\n")
		sb.Write(shot.Elements)
		sb.WriteString("\n")
	} else if look == lookDelegate {
		sb.WriteString("No text tree is available on this Android version for a background display; the vision model's description below is the only reading of the screen.\n")
	} else {
		sb.WriteString("No text tree is available on this Android version for a background display; use the screenshot to read the screen.\n")
	}

	// With the delegate, the kernel appends the vision model's description.
	if look != lookDelegate && shot.Grid && imageCapable {
		sb.WriteString("The screenshot has a coordinate grid: faint lines every 50 px and an \"x,y\" label at every 100 px crossing. " +
			"Read a target's centre off the grid and tap those pixel coordinates.\n")
	}
	res := llm.NewTextResult(sb.String())
	if imageCapable && shot.Image != "" {
		if data, err := base64.StdEncoding.DecodeString(shot.Image); err == nil {
			res.Files = []llm.FilePart{{Filename: "screen.jpg", MediaType: "image/jpeg", Data: data}}
		}
	}
	if look == lookDelegate && len(res.Files) == 0 {
		res.Content += "No screenshot came back from the display, so nothing could be described; look again.\n"
	}
	return res, nil
}
