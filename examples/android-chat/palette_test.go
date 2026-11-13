package main

import (
	"context"
	"encoding/json"
	"image"
	"image/color"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	toroid "github.com/yashbonde/toroid-kernel"
)

func TestContrastRatio(t *testing.T) {
	black, white := rgb{0, 0, 0}, rgb{255, 255, 255}
	if c := contrast(black, white); math.Abs(c-21) > 1e-9 {
		t.Fatalf("black on white = %v, want 21", c)
	}
	if c := contrast(white, white); c != 1 {
		t.Fatalf("white on white = %v, want 1", c)
	}
	// #767676 on white is the classic just-passing AA grey (4.54:1).
	grey, _ := parseHex("#767676")
	if c := contrast(grey, white); c < 4.5 || c > 4.6 {
		t.Fatalf("#767676 on white = %v", c)
	}
	if c := contrast(white, grey); c < 4.5 || c > 4.6 {
		t.Fatal("contrast is not symmetric")
	}
}

func TestParseHex(t *testing.T) {
	if c, ok := parseHex("#7CC4A4"); !ok || c != (rgb{0x7c, 0xc4, 0xa4}) || c.hex() != "#7cc4a4" {
		t.Fatalf("parseHex = %v %v", c, ok)
	}
	if c, ok := parseHex("0e0f11"); !ok || c != (rgb{0x0e, 0x0f, 0x11}) {
		t.Fatal("bare hex refused")
	}
	for _, bad := range []string{"", "#fff", "#12345g", "rgb(1,2,3)"} {
		if _, ok := parseHex(bad); ok {
			t.Errorf("parseHex(%q) accepted", bad)
		}
	}
}

func fullRoles(bg, text string) map[string]string {
	m := map[string]string{}
	for _, r := range paletteRoles {
		m[r] = "#336699"
	}
	m["bg"], m["text"] = bg, text
	return m
}

func TestReadRoles(t *testing.T) {
	roles, err := readRoles(fullRoles("#0E0F11", "#C9F0DD"))
	if err != nil || roles["bg"] != "#0e0f11" {
		t.Fatalf("good palette: %v %v", roles, err)
	}
	m := fullRoles("#000000", "#ffffff")
	delete(m, "tint3")
	m["warn"] = "amber"
	if _, err := readRoles(m); err == nil || !strings.Contains(err.Error(), "warn, tint3") {
		t.Fatalf("missing roles: %v", err)
	}
}

func TestHSLRoundTrip(t *testing.T) {
	for _, h := range []string{"#0e0f11", "#7cc4a4", "#ffb347", "#c62828", "#808080", "#ffffff", "#000000"} {
		c, _ := parseHex(h)
		hh, s, l := c.hsl()
		if got := fromHSL(hh, s, l); got != c {
			t.Errorf("%s -> hsl -> %s", h, got.hex())
		}
	}
}

// checkMode fails t when a settled palette breaks one of its mode's rules.
func checkMode(t *testing.T, name string, m map[string]string, dark bool) {
	t.Helper()
	get := func(r string) rgb { c, _ := parseHex(m[r]); return c }
	_, bs, bl := get("bg").hsl()
	if dark && (bl > 0.07 || bs > 0.12) || !dark && (bl < 0.96 || bs > 0.25) {
		t.Errorf("%s: bg %s is not a %s near-neutral (s %.2f, l %.2f)", name, m["bg"], map[bool]string{true: "near-black", false: "near-white"}[dark], bs, bl)
	}
	if c := contrast(get("text"), get("bg")); c < minTextContrast {
		t.Errorf("%s: text %s on %s is %.2f:1", name, m["text"], m["bg"], c)
	}
	if c := contrast(get("muted"), get("bg")); c < minMutedContrast {
		t.Errorf("%s: muted %s on %s is %.2f:1", name, m["muted"], m["bg"], c)
	}
	for _, r := range []string{"accent", "accent_hi", "warn", "bad", "tint1", "tint2", "tint3"} {
		if c := contrast(get(r), get("surface")); c < minAccentContrast-0.01 {
			t.Errorf("%s: %s %s on surface %s is %.2f:1", name, r, m[r], m["surface"], c)
		}
	}
	if hueGap(get("warn"), get("accent")) < minWarnGap || hueGap(get("bad"), get("accent")) < minBadGap || hueGap(get("bad"), get("warn")) < minPairGap {
		t.Errorf("%s: accent %s, warn %s and bad %s are too alike", name, m["accent"], m["warn"], m["bad"])
	}
}

// TestSettle: a navy dark mode, a grey light mode, unreadable accents and a
// warning the colour of the accent all come out obeying their mode's rules.
func TestSettle(t *testing.T) {
	dark := map[string]string{
		"bg": "#152235", "surface": "#27383e", "line": "#3f4740", "text": "#f9e7a5", "muted": "#3a3a3a",
		"accent": "#4a3a10", "accent_hi": "#4a3a10", "warn": "#c6a05c", "bad": "#964a2f",
		"tint1": "#6c7358", "tint2": "#202020", "tint3": "#615f47",
	}
	fixes := settle(dark, true)
	if len(fixes) == 0 {
		t.Fatal("nothing fixed")
	}
	checkMode(t, "dark", dark, true)
	if h, _, _ := (func() rgb { c, _ := parseHex(dark["bg"]); return c }()).hsl(); h < 180 || h > 260 {
		t.Errorf("dark bg %s lost the image's faint blue hue", dark["bg"])
	}

	light := map[string]string{
		"bg": "#9a9a9a", "surface": "#d0d0d0", "line": "#bbbbbb", "text": "#555555", "muted": "#cccccc",
		"accent": "#f5d584", "accent_hi": "#f5d584", "warn": "#f5d584", "bad": "#ffcccc",
		"tint1": "#eeeeaa", "tint2": "#aaeeee", "tint3": "#eeaaee",
	}
	settle(light, false)
	checkMode(t, "light", light, false)

	// A palette that already keeps the rules is left alone.
	good := map[string]string{
		"bg": "#0e0e0f", "surface": "#1a1a1c", "line": "#3a3a3d", "text": "#ededea", "muted": "#a3a3a0",
		"accent": "#7cc4a4", "accent_hi": "#c9f0dd", "warn": "#e8b04b", "bad": "#ef5350",
		"tint1": "#6fc3df", "tint2": "#b388eb", "tint3": "#e0a3c0",
	}
	if fixes := settle(good, true); len(fixes) != 0 {
		t.Fatalf("a good palette was changed: %v", fixes)
	}
}

func TestDominantColours(t *testing.T) {
	// 60% navy, 30% near-navy (merged into it), 10% amber, and a few
	// transparent pixels that do not count.
	img := image.NewNRGBA(image.Rect(0, 0, 10, 11))
	for y := 0; y < 10; y++ {
		for x := 0; x < 10; x++ {
			c := color.NRGBA{0x10, 0x20, 0x50, 0xff}
			switch {
			case x >= 9:
				c = color.NRGBA{0xff, 0xb3, 0x47, 0xff}
			case x >= 6:
				c = color.NRGBA{0x14, 0x24, 0x56, 0xff}
			}
			img.SetNRGBA(x, y, c)
		}
	}
	got := dominantColours(img, 12)
	if len(got) != 2 {
		t.Fatalf("got %d colours: %+v", len(got), got)
	}
	if got[0].Share != 0.9 || got[1].Share != 0.1 || got[1].Hex != "#ffb347" {
		t.Fatalf("colours = %+v", got)
	}
	if !strings.HasPrefix(got[0].Hex, "#1") {
		t.Fatalf("merged navy = %s", got[0].Hex)
	}

	// Many distinct colours are cut to the n largest.
	many := image.NewNRGBA(image.Rect(0, 0, 20, 1))
	for x := 0; x < 20; x++ {
		many.SetNRGBA(x, 0, color.NRGBA{uint8(x * 12), uint8(255 - x*12), uint8(x * 5 % 256), 0xff})
	}
	if got := dominantColours(many, 12); len(got) > 12 {
		t.Fatalf("kept %d colours", len(got))
	}
	if got := dominantColours(image.NewNRGBA(image.Rect(0, 0, 2, 2)), 12); got != nil {
		t.Fatalf("transparent image gave %v", got)
	}
}

// paletteGateway answers each palette call with the next of answers.
func paletteGateway(t *testing.T, answers []string, prompts *[]string) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Model    string `json:"model"`
			Messages []struct {
				Content string `json:"content"`
			} `json:"messages"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body.Model != "vendor/flash" {
			t.Errorf("model = %q, want the bare id", body.Model)
		}
		*prompts = append(*prompts, body.Messages[1].Content)
		json.NewEncoder(w).Encode(map[string]any{
			"choices": []any{map[string]any{"message": map[string]string{"content": "```json\n" + answers[len(*prompts)-1] + "\n```"}}},
			"usage":   map[string]any{"cost": 0.0001},
		})
	}))
}

// TestPickPaletteRetriesOnIncompleteAnswer: an answer missing the light mode
// is sent back with the reason; the second, complete one is settled into a
// dark and a light palette that keep their rules.
func TestPickPaletteRetriesOnIncompleteAnswer(t *testing.T) {
	dark, _ := json.Marshal(fullRoles("#14203a", "#e6e6e3"))
	light, _ := json.Marshal(fullRoles("#f6f6f4", "#1a1a1a"))
	answers := []string{`{"dark": ` + string(dark) + `}`, `{"dark": ` + string(dark) + `, "light": ` + string(light) + `}`}
	var prompts []string
	gw := paletteGateway(t, answers, &prompts)
	defer gw.Close()
	t.Setenv(toroid.GatewayBaseURLEnv, gw.URL)

	img := image.NewNRGBA(image.Rect(0, 0, 4, 4))
	for i := 0; i < len(img.Pix); i += 4 {
		img.Pix[i], img.Pix[i+1], img.Pix[i+2], img.Pix[i+3] = 0x14, 0x20, 0x3a, 0xff
	}
	p, err := pickPalette(context.Background(), img, gatewayPrefix+"vendor/flash", "k")
	if err != nil {
		t.Fatal(err)
	}
	if p.Tries != 2 || p.Light == nil || !p.Fixed || !near(p.CostUSD, 0.0002) || p.Contrast < minTextContrast || p.LightContrast < minTextContrast {
		t.Fatalf("palette = %+v", p)
	}
	if !strings.Contains(prompts[0], "#14203a") || !strings.Contains(prompts[1], "missing") {
		t.Fatalf("prompts = %q", prompts)
	}
	checkMode(t, "dark", p.Roles, true)
	checkMode(t, "light", p.Light, false)
}

// TestPickPaletteGivesUp: a model that never answers with both modes fails
// after paletteTries calls.
func TestPickPaletteGivesUp(t *testing.T) {
	var prompts []string
	answers := []string{"not json", "{}", `{"dark": {}}`}
	gw := paletteGateway(t, answers, &prompts)
	defer gw.Close()
	t.Setenv(toroid.GatewayBaseURLEnv, gw.URL)
	img := image.NewNRGBA(image.Rect(0, 0, 1, 1))
	img.Pix = []uint8{0x0e, 0x0f, 0x11, 0xff}
	if _, err := pickPalette(context.Background(), img, "vendor/flash", "k"); err == nil || len(prompts) != paletteTries {
		t.Fatalf("err = %v after %d calls", err, len(prompts))
	}
}

// TestLLMJSONReasoningFallback: a model that refuses to skip reasoning is
// asked again with low reasoning effort.
func TestLLMJSONReasoningFallback(t *testing.T) {
	var bodies []map[string]any
	gw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var b map[string]any
		_ = json.NewDecoder(r.Body).Decode(&b)
		bodies = append(bodies, b)
		if len(bodies) == 1 {
			http.Error(w, `{"error":{"message":"Reasoning is mandatory for this endpoint and cannot be disabled."}}`, http.StatusBadRequest)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": map[string]string{"content": `{"ok":true}`}}}})
	}))
	defer gw.Close()
	t.Setenv(toroid.GatewayBaseURLEnv, gw.URL)
	out, _, err := llmJSON(context.Background(), "vendor/glm", "k", "s", "u")
	if err != nil || out != `{"ok":true}` || len(bodies) != 2 {
		t.Fatalf("out=%q err=%v calls=%d", out, err, len(bodies))
	}
	if r, _ := bodies[1]["reasoning"].(map[string]any); r["effort"] != "low" {
		t.Fatalf("second request reasoning = %v", bodies[1]["reasoning"])
	}
}
