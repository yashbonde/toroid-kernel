package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"io"
	"math"
	"net/http"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"

	toroid "github.com/yashbonde/toroid-kernel"
)

// Palettes: palette_pick reads an image's dominant colours here in Go, then
// asks a cheap LLM which colour plays which UI role, in a dark and a light
// mode. What the LLM must not get wrong is enforced here (settle): a dark
// mode on a near-black, nearly neutral background, a light mode on near-white,
// text at WCAG 4.5:1 or more, accents that read, and warnings and errors that
// never look like the accent. The palette is stored with the image
// (agents.db); the Gallery applies one mode to every app.

const (
	paletteColours  = 12  // dominant colours read from an image
	minTextContrast = 4.5 // WCAG AA for body text
	paletteTries    = 3   // LLM attempts at a complete answer
	mergeDistance   = 28  // RGB distance under which two colours count as one
)

// paletteRoles are the roles every palette fills, in display order.
var paletteRoles = []string{"bg", "surface", "line", "text", "muted", "accent", "accent_hi", "warn", "bad", "tint1", "tint2", "tint3"}

// rgb is one colour, 8 bits a channel.
type rgb struct{ R, G, B uint8 }

func (c rgb) hex() string { return fmt.Sprintf("#%02x%02x%02x", c.R, c.G, c.B) }

var hexPattern = regexp.MustCompile(`^#?([0-9a-fA-F]{6})$`)

func parseHex(s string) (rgb, bool) {
	m := hexPattern.FindStringSubmatch(strings.TrimSpace(s))
	if m == nil {
		return rgb{}, false
	}
	var c rgb
	_, err := fmt.Sscanf(m[1], "%02x%02x%02x", &c.R, &c.G, &c.B)
	return c, err == nil
}

// luminance is WCAG relative luminance, 0 (black) to 1 (white).
func (c rgb) luminance() float64 {
	ch := func(v uint8) float64 {
		s := float64(v) / 255
		if s <= 0.04045 {
			return s / 12.92
		}
		return math.Pow((s+0.055)/1.055, 2.4)
	}
	return 0.2126*ch(c.R) + 0.7152*ch(c.G) + 0.0722*ch(c.B)
}

// contrast is the WCAG contrast ratio of two colours, 1 to 21.
func contrast(a, b rgb) float64 {
	la, lb := a.luminance(), b.luminance()
	if la < lb {
		la, lb = lb, la
	}
	return (la + 0.05) / (lb + 0.05)
}

// swatch is one dominant colour and the share of the image it covers.
type swatch struct {
	Hex   string  `json:"hex"`
	Share float64 `json:"share"` // 0..1 of all pixels
}

// dominantColours returns up to n colours that cover the most of img, largest
// first. Pixels are binned at 5 bits a channel, then bins closer than
// mergeDistance are merged (largest first, so each merged colour is pulled
// toward the bigger area). Fully transparent pixels are skipped.
func dominantColours(img image.Image, n int) []swatch {
	type bin struct {
		r, g, b, count float64
	}
	bins := map[uint16]*bin{}
	total := 0.0
	bnd := img.Bounds()
	for y := bnd.Min.Y; y < bnd.Max.Y; y++ {
		for x := bnd.Min.X; x < bnd.Max.X; x++ {
			r, g, b, a := img.At(x, y).RGBA()
			if a == 0 {
				continue
			}
			r8, g8, b8 := r>>8, g>>8, b>>8
			key := uint16(r8>>3)<<10 | uint16(g8>>3)<<5 | uint16(b8>>3)
			bn := bins[key]
			if bn == nil {
				bn = &bin{}
				bins[key] = bn
			}
			bn.r += float64(r8)
			bn.g += float64(g8)
			bn.b += float64(b8)
			bn.count++
			total++
		}
	}
	if total == 0 {
		return nil
	}
	list := make([]*bin, 0, len(bins))
	for _, bn := range bins {
		list = append(list, bn)
	}
	// Largest first; ties by colour so the result never depends on map order.
	sort.Slice(list, func(i, j int) bool {
		if list[i].count != list[j].count {
			return list[i].count > list[j].count
		}
		return list[i].r+list[i].g*256+list[i].b*65536 < list[j].r+list[j].g*256+list[j].b*65536
	})
	var clusters []*bin
	for _, bn := range list {
		mr, mg, mb := bn.r/bn.count, bn.g/bn.count, bn.b/bn.count
		var near *bin
		for _, c := range clusters {
			cr, cg, cb := c.r/c.count, c.g/c.count, c.b/c.count
			if math.Sqrt((mr-cr)*(mr-cr)+(mg-cg)*(mg-cg)+(mb-cb)*(mb-cb)) < mergeDistance {
				near = c
				break
			}
		}
		if near == nil {
			clusters = append(clusters, &bin{r: bn.r, g: bn.g, b: bn.b, count: bn.count})
			continue
		}
		near.r += bn.r
		near.g += bn.g
		near.b += bn.b
		near.count += bn.count
	}
	sort.SliceStable(clusters, func(i, j int) bool { return clusters[i].count > clusters[j].count })
	if len(clusters) > n {
		clusters = clusters[:n]
	}
	out := make([]swatch, len(clusters))
	for i, c := range clusters {
		col := rgb{uint8(math.Round(c.r / c.count)), uint8(math.Round(c.g / c.count)), uint8(math.Round(c.b / c.count))}
		out[i] = swatch{Hex: col.hex(), Share: math.Round(c.count/total*1000) / 1000}
	}
	return out
}

// palette is what palette_pick stores and returns: the same colours as a
// dark mode (Roles) and a light mode (Light).
type palette struct {
	Colours       []swatch          `json:"colours"`
	Roles         map[string]string `json:"roles"`           // dark mode: role -> "#rrggbb"
	Light         map[string]string `json:"light,omitempty"` // light mode
	Contrast      float64           `json:"contrast"`        // text on bg, dark mode
	LightContrast float64           `json:"light_contrast,omitempty"`
	Fixed         bool              `json:"fixed,omitempty"` // code changed a colour the model chose
	Fixes         []string          `json:"fixes,omitempty"` // what it changed, for the logs
	Model         string            `json:"model,omitempty"`
	CostUSD       float64           `json:"cost_usd"`
	Tries         int               `json:"tries"`
}

// readRoles checks that an LLM's role map names every role as a hex colour,
// and returns the roles as lowercase "#rrggbb".
func readRoles(raw map[string]string) (map[string]string, error) {
	roles := map[string]string{}
	var missing []string
	for _, r := range paletteRoles {
		c, ok := parseHex(raw[r])
		if !ok {
			missing = append(missing, r)
			continue
		}
		roles[r] = c.hex()
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("these roles are missing or not a #rrggbb colour: %s", strings.Join(missing, ", "))
	}
	return roles, nil
}

// hsl is c as hue (0-360), saturation and lightness (0-1).
func (c rgb) hsl() (h, s, l float64) {
	r, g, b := float64(c.R)/255, float64(c.G)/255, float64(c.B)/255
	mx, mn := math.Max(r, math.Max(g, b)), math.Min(r, math.Min(g, b))
	l = (mx + mn) / 2
	if mx == mn {
		return 0, 0, l
	}
	d := mx - mn
	if l > 0.5 {
		s = d / (2 - mx - mn)
	} else {
		s = d / (mx + mn)
	}
	switch mx {
	case r:
		h = math.Mod((g-b)/d+6, 6)
	case g:
		h = (b-r)/d + 2
	default:
		h = (r-g)/d + 4
	}
	return h * 60, s, l
}

func fromHSL(h, s, l float64) rgb {
	s, l = math.Max(0, math.Min(1, s)), math.Max(0, math.Min(1, l))
	c := (1 - math.Abs(2*l-1)) * s
	x := c * (1 - math.Abs(math.Mod(h/60, 2)-1))
	m := l - c/2
	var r, g, b float64
	switch {
	case h < 60:
		r, g, b = c, x, 0
	case h < 120:
		r, g, b = x, c, 0
	case h < 180:
		r, g, b = 0, c, x
	case h < 240:
		r, g, b = 0, x, c
	case h < 300:
		r, g, b = x, 0, c
	default:
		r, g, b = c, 0, x
	}
	v := func(f float64) uint8 { return uint8(math.Round((f + m) * 255)) }
	return rgb{v(r), v(g), v(b)}
}

// hueGap is how far apart two colours' hues are, 0-180 degrees; 180 when
// either is too grey to have a hue worth comparing.
func hueGap(a, b rgb) float64 {
	ha, sa, _ := a.hsl()
	hb, sb, _ := b.hsl()
	if sa < 0.2 || sb < 0.2 {
		return 180
	}
	d := math.Abs(ha - hb)
	return math.Min(d, 360-d)
}

// The rules a mode's palette must keep, enforced by settle.
const (
	minAccentContrast = 4.5 // accents, warn, bad and tints on surface: apps also use them as text
	minMutedContrast  = 4.0 // secondary text on bg
	// Degrees of hue between accent, warn and bad. A warning may sit nearer
	// the accent than an error may, since amber accents are common.
	minWarnGap = 15.0 // warn from accent
	minBadGap  = 30.0 // bad from accent
	minPairGap = 20.0 // warn from bad
)

// Plain warning and error colours (dark mode, light mode), used when the
// image's own are too close to its accent: the one furthest from it wins.
var warnChoices = map[bool][]string{
	true:  {"#e8b04b", "#e6c84a", "#f08c3a"}, // amber, yellow, orange
	false: {"#a16207", "#8a6d00", "#c2410c"},
}
var badChoices = map[bool][]string{
	true:  {"#ef5350", "#e5486e", "#f06292"}, // red, crimson, pink-red
	false: {"#c62828", "#be123c", "#ad1457"},
}

// furthest is the choice whose hue is furthest from every colour in from.
func furthest(choices []string, from ...rgb) rgb {
	var best rgb
	bestGap := -1.0
	for _, h := range choices {
		c, _ := parseHex(h)
		gap := 180.0
		for _, f := range from {
			gap = math.Min(gap, hueGap(c, f))
		}
		if gap > bestGap {
			best, bestGap = c, gap
		}
	}
	return best
}

// settle makes roles obey a mode's rules, changing as little as it can, and
// returns what it changed. Dark mode: bg and surface are near-black and
// nearly grey (a faint tint of the image's hue at most, never a visibly
// coloured background like navy), text near-white. Light mode: bg and surface
// near-white, text near-black. In both, muted text and every accent read on
// their background, accent_hi stands out from accent (lighter in dark mode,
// deeper in light mode), and accent, warn and bad have clearly different hues.
func settle(roles map[string]string, dark bool) []string {
	var fixes []string
	get := func(r string) rgb { c, _ := parseHex(roles[r]); return c }
	set := func(r string, c rgb) {
		if c.hex() != roles[r] {
			fixes = append(fixes, fmt.Sprintf("%s %s->%s", r, roles[r], c.hex()))
			roles[r] = c.hex()
		}
	}
	clamp := func(v, lo, hi float64) float64 { return math.Max(lo, math.Min(hi, v)) }
	// shape keeps a colour's hue and caps its saturation and lightness range.
	shape := func(r string, maxS, minL, maxL float64) {
		h, s, l := get(r).hsl()
		set(r, fromHSL(h, math.Min(s, maxS), clamp(l, minL, maxL)))
	}
	// reach steps a colour's lightness away from against until it reaches min
	// contrast: up in dark mode, down in light mode.
	reach := func(r string, against rgb, min float64) {
		c := get(r)
		h, s, l := c.hsl()
		for i := 0; i < 50 && contrast(c, against) < min; i++ {
			if dark {
				l += 0.02
			} else {
				l -= 0.02
			}
			c = fromHSL(h, s, l)
		}
		set(r, c)
	}
	if dark {
		shape("bg", 0.12, 0.03, 0.065)
		_, _, bl := get("bg").hsl()
		shape("surface", 0.12, bl+0.04, bl+0.08)
		shape("line", 0.15, 0.17, 0.26)
		shape("text", 0.30, 0.88, 0.97)
	} else {
		shape("bg", 0.25, 0.96, 0.985)
		_, _, bl := get("bg").hsl()
		shape("surface", 0.25, bl-0.06, bl-0.03)
		shape("line", 0.20, 0.78, 0.86)
		shape("text", 0.30, 0.05, 0.14)
	}
	// Swap in plain warn and bad colours first (only the hue matters here),
	// so the contrast steps below apply to them too.
	if hueGap(get("warn"), get("accent")) < minWarnGap {
		set("warn", furthest(warnChoices[dark], get("accent")))
	}
	if hueGap(get("bad"), get("accent")) < minBadGap || hueGap(get("bad"), get("warn")) < minPairGap {
		set("bad", furthest(badChoices[dark], get("accent"), get("warn")))
	}
	bg, surface := get("bg"), get("surface")
	reach("text", bg, minTextContrast)
	reach("muted", bg, minMutedContrast)
	for _, r := range []string{"accent", "accent_hi", "warn", "bad", "tint1", "tint2", "tint3"} {
		reach(r, surface, minAccentContrast)
	}
	// accent_hi must be a visibly different step of the accent.
	_, _, la := get("accent").hsl()
	h, sat, l := get("accent_hi").hsl()
	if dark && l < la+0.08 {
		set("accent_hi", fromHSL(h, sat, math.Min(la+0.12, 0.92)))
	} else if !dark && l > la-0.08 {
		set("accent_hi", fromHSL(h, sat, math.Max(la-0.12, 0.12)))
	}
	return fixes
}

const paletteSystemPrompt = `You turn an image's dominant colours into the UI colour palette of a phone
whose wallpaper is that image: one palette for dark mode and one for light
mode. Answer with one JSON object and nothing else, of this shape:
{"dark": {role: "#rrggbb", ...}, "light": {role: "#rrggbb", ...}}
Each mode names all twelve roles:

bg        the app background
surface   panels, tiles and bars, one step off bg
line      borders and dividers
text      body text
muted     secondary text, quieter than text but easy to read
accent    anything you can tap: the image's most vivid, characteristic colour
          (its neon, its sunset, its brightest flower), saturated, not a
          greyed-down version
accent_hi a second step of the accent for highlights and live activity
warn      warnings: amber or yellow-ish
bad       errors: red-ish
tint1..3  three more colours from the image, for category tints

Dark mode: bg must be really dark and nearly neutral, close to black
(#0a0a0a to #141414), with at most a faint hint of the image's hue. Never a
visibly coloured background such as dark blue, navy, teal or brown, even if
the image is mostly that colour. surface is a little lighter (#161616 to
#222222 range), also nearly neutral. text is near-white (it may be slightly
warm or cool). accent and the tints are the image's colours made bright enough
to read on surface.

Light mode: bg is near-white (#f5f5f3 to #fbfaf8), may be faintly warm or
cool but never visibly tinted; surface a little darker (#ebebe8 range). text is
near-black. accent and the tints are the image's colours made deep enough to
read on white.

In both modes: accent, warn and bad must be three clearly different hues, so a
warning never looks like a button or an error. Prefer colours that appear in
the image; change their lightness and saturation as each mode needs, keeping
the hue.`

// pickPalette counts img's colours, asks model (a kernel id) for a dark and a
// light palette (again, with the reason, while an answer is not a complete
// pair of role maps), then makes each mode keep its rules (settle).
func pickPalette(ctx context.Context, img image.Image, model, key string) (palette, error) {
	p := palette{Colours: dominantColours(img, paletteColours), Model: model}
	if len(p.Colours) == 0 {
		return p, errors.New("the image has no visible pixels")
	}
	var list strings.Builder
	for _, c := range p.Colours {
		col, _ := parseHex(c.Hex)
		fmt.Fprintf(&list, "%s  %4.1f%% of the image  luminance %.3f\n", c.Hex, c.Share*100, col.luminance())
	}
	user := "Dominant colours, largest area first:\n" + list.String()
	for p.Tries < paletteTries {
		p.Tries++
		out, cost, err := llmJSON(ctx, model, key, paletteSystemPrompt, user)
		p.CostUSD += cost
		if err != nil {
			return p, err
		}
		var raw map[string]map[string]string
		if err := json.Unmarshal([]byte(out), &raw); err != nil {
			user += "\nYour last answer was not a JSON object with \"dark\" and \"light\" role maps. Answer with the JSON object only."
			continue
		}
		dark, errD := readRoles(raw["dark"])
		light, errL := readRoles(raw["light"])
		if errD != nil || errL != nil {
			why := errD
			if why == nil {
				why = errL
			}
			user += "\nYour last answer was rejected: " + why.Error() + " (in both modes). Answer again with the whole JSON object."
			continue
		}
		for _, f := range settle(dark, true) {
			p.Fixes = append(p.Fixes, "dark "+f)
		}
		for _, f := range settle(light, false) {
			p.Fixes = append(p.Fixes, "light "+f)
		}
		p.Roles, p.Light, p.Fixed = dark, light, len(p.Fixes) > 0
		ratio := func(m map[string]string) float64 {
			t, _ := parseHex(m["text"])
			b, _ := parseHex(m["bg"])
			return math.Round(contrast(t, b)*100) / 100
		}
		p.Contrast, p.LightContrast = ratio(dark), ratio(light)
		return p, nil
	}
	return p, errors.New("the model did not return a usable palette")
}

// llmJSON makes one chat completion on the gateway asking for a JSON object,
// with reasoning off (or low, for a model that requires it), and returns the
// message text and OpenRouter's billed cost.
func llmJSON(ctx context.Context, model, key, system, user string) (string, float64, error) {
	out, cost, err := llmJSONOnce(ctx, model, key, system, user, false)
	if err != nil && strings.Contains(strings.ToLower(err.Error()), "reasoning is mandatory") {
		// Some models (GLM 5.3 Flash) refuse to skip reasoning: let them think
		// briefly instead, with room for it.
		return llmJSONOnce(ctx, model, key, system, user, true)
	}
	return out, cost, err
}

// llmJSONOnce is one llmJSON request; think lets the model reason (at low
// effort) instead of turning reasoning off.
func llmJSONOnce(ctx context.Context, model, key, system, user string, think bool) (string, float64, error) {
	req := map[string]any{
		"model": strings.TrimPrefix(model, gatewayPrefix),
		"messages": []map[string]string{
			{"role": "system", "content": system},
			{"role": "user", "content": user},
		},
		"response_format": map[string]string{"type": "json_object"},
		// Reasoning models would otherwise spend the whole budget thinking
		// about twelve colours and return no answer.
		"reasoning":   map[string]bool{"enabled": false},
		"max_tokens":  2500,
		"temperature": 0.2,
	}
	if think {
		req["reasoning"] = map[string]string{"effort": "low"}
		req["max_tokens"] = 12000
	}
	body, _ := json.Marshal(req)
	ctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	hreq, err := http.NewRequestWithContext(ctx, http.MethodPost, gatewayURL("/chat/completions"), bytes.NewReader(body))
	if err != nil {
		return "", 0, err
	}
	hreq.Header.Set("Authorization", "Bearer "+key)
	hreq.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(hreq)
	if err != nil {
		return "", 0, fmt.Errorf("palette model: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return "", 0, fmt.Errorf("palette model: %s: %s", resp.Status, oneLine(apiError(raw), 200))
	}
	var out struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
		Usage struct {
			Cost float64 `json:"cost"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(raw, &out); err != nil || len(out.Choices) == 0 {
		return "", 0, errors.New("palette model: unreadable reply")
	}
	return stripFence(out.Choices[0].Message.Content), out.Usage.Cost, nil
}

// stripFence drops a ```json fence some models put around JSON anyway.
func stripFence(s string) string {
	s = strings.TrimSpace(s)
	if strings.HasPrefix(s, "```") {
		s = strings.TrimPrefix(s, "```json")
		s = strings.TrimPrefix(s, "```")
		s = strings.TrimSuffix(strings.TrimSpace(s), "```")
	}
	return strings.TrimSpace(s)
}

// apiError is the message in an OpenRouter error body, or the body itself.
func apiError(raw []byte) string {
	var e struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(raw, &e) == nil && e.Error.Message != "" {
		return e.Error.Message
	}
	return string(raw)
}

// gatewayURL is path on the gateway (OpenRouter by default).
func gatewayURL(path string) string {
	base := os.Getenv(toroid.GatewayBaseURLEnv)
	if base == "" {
		base = openRouterBaseURL
	}
	return strings.TrimRight(base, "/") + path
}
