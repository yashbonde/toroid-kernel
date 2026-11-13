package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha1"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	_ "image/jpeg" // decode generated JPEGs for their size
	"image/png"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/yashbonde/toroid-kernel/llm"
	"github.com/yashbonde/toroid-kernel/tools"
)

// Images: the tools of the @image-gen-agent setup. image_generate and
// image_recolour call OpenRouter's images endpoint; palette_pick reads an
// image's colours (palette.go). Every image they make is saved in shared
// storage at Pictures/Toroid, where the Gallery shows it, and recorded in
// agents.db with its prompt, model, cost and the image it came from. As with
// the file tools, the Go server does no shared-storage I/O itself: it writes
// the image into its private files dir and the tool host (Images.java)
// copies it out and tells the media scanner, or reads a shared image into a
// small private PNG for the server to look at.

const (
	imagesDir        = "Pictures/Toroid" // where images go, as the Gallery shows it
	imageTimeout     = 3 * time.Minute   // one images call; finals take ~30 s
	maxPromptLen     = 4000              // runes in an image prompt
	maxImageBytes    = 25 << 20          // decoded bytes in one generated image
	recolourMaxSide  = 1024              // pixels: the reference image sent to recolour
	paletteMaxSide   = 256               // pixels: enough to count colours
	defaultSetupID   = "@image-gen-agent"
	unknownImageCost = 0.05 // USD, the guess for a model not in imagePriceGuess
)

// imagePriceGuess is roughly what one 9:16 image costs on each model, used
// only to refuse a call the chat's remaining budget cannot cover. The real
// cost is OpenRouter's usage.cost.
var imagePriceGuess = map[string]float64{
	"openai/gpt-image-2.5-sunburst":        0.055,
	"black-forest-labs/flux-3-image-klein": 0.011,
	"black-forest-labs/flux-3-image":       0.07,
	"qwen/qwen-image-2.1-turbo":            0.0184,
	"qwen/qwen-image-2.1-pro":              0.046,
	"recraft/recraft-v4.1-flash":           0.007,
	"sourceful/riverflow-v2.6-pro":         0.16,
}

// imageShapes are the aspect ratios image_generate accepts; 9:16 (the
// default) is a phone wallpaper.
var imageShapes = []string{"9:16", "2:3", "1:1", "3:2"}

// shapeFor maps a shape onto the nearest one a model accepts, for the models
// that take only some of them.
var shapeFor = map[string]map[string]string{
	"recraft/recraft-v4.1-flash": {"2:3": "3:4", "3:2": "4:3"},
}

// imageGenPrompt is @image-gen-agent's system prompt.
const imageGenPrompt = `You are the image agent in the user's Gallery app on their phone. You make
images, mostly phone wallpapers, with your tools, and talk in short, plain lines.

Tools:
- image_generate(prompt, quality, model, shape): draws one image, 9:16 unless
  shape says otherwise. Always start with a "draft" (cheap, about 10 s). Make
  a "final" (about $0.05, about 30 s) only when the user asks for one or
  approves a draft; then reuse that draft's prompt, changed only as the user
  asked. model and shape are for when the user picked them (see Options).
- image_recolour(image, palette_description, quality): redraws an image in new
  colours, keeping its composition. image is an id from an earlier tool result.
- palette_pick(image): reads an image's colours into a UI palette and stores
  it. Only when the user asks for a palette or colours.
- image_look(image): describes an image (subject, layout, style, colours).
  Use it on an image the user attached before writing a prompt from it.
The user sees every image in the chat as soon as it is made. After a tool
call, say in a line or two what you made and offer one or two changes. Do not
repeat the prompt unless asked. If a tool fails, say plainly what failed.

How to write an image prompt. Unless the user asks for something else, an
image is a wallpaper: a vivid, richly detailed pixel-art picture. Write every prompt
in three labelled parts, in this order, each a few concrete sentences
(120-250 words in all):

<scene>: what is in the picture and where: one strong subject, few objects,
large calm areas, the viewpoint, and where each thing sits in the frame.
<design>: the style. By default, high-resolution pixel art that is super
colourful and richly detailed: fine, small pixels (it reads as pixel art up
close and as a lush painting from arm's length), a vivid, saturated palette of
many colours, carefully shaded forms, glowing light, atmosphere and lots of
small details (leaves, windows, stars, ripples, reflections), like modern
indie-game key art. Not chunky 8-bit or 16-bit blocks, not flat vector, not
photographic, unless the user asks. Always end with: full-bleed, no border, no
text, no letters or numbers, no clock, no watermark.
<colours>: the palette by name with rough area shares ("about 40% deep navy
and teal ..."), which forms are lit and which in shadow, and the lightest tone.

Rules for every wallpaper:
- The phone crops the 9:16 image to 720x1480, cutting a little off the left and
  right: keep the subject and anything important in the middle 80% of the width.
- The top third is for the phone's clock: keep it calm, open sky or a soft
  field of colour, with no subject, tower, tree, balloon or moon reaching into
  the band from about 12% to 28% down. Tall subjects stop below a third of the
  height. Say so in <scene>. Never draw a clock, a time, or any text, letters,
  numbers or logos anywhere.
- The bottom fifth sits under the app dock: keep it simple too.

Options. The user's message may end with an [Options] block the Gallery
adds from its pickers:
- "Content", "Style" and "Colours" each carry a full <scene>, <design> or
  <colours> text from one of the user's inspirations. Use each one as that
  part of your prompt, nearly verbatim; change it only as the user's own words
  ask, and to keep the wallpaper rules below when the shape is 9:16. Write any
  part that has no option yourself, in a way that fits the picked ones.
- "Shape" is the aspect ratio: pass it as shape. Anything but 9:16 is not a
  wallpaper: drop the cropping, clock and dock rules for it.
- "Model" is an image model id: pass it as model, with quality draft. It
  replaces the draft/final choice for that image.
Never show the [Options] block back to the user.

Attached image. A message may end with "[Image] <path>": the user opened that
picture in the Gallery and tapped Chat. It is what "this", "it" or "the
picture" means. Call image_look on the path before you write a prompt from it
or talk about what is in it; image_recolour and palette_pick take the path as
their image directly.

Example of the shape (not content to reuse):
<scene>: A vertical poster of a lone lighthouse on a dark, rocky headland at
night, standing in the centre of the lower half. A huge pale moon sits behind
it, slightly right of centre, about a third of the width across. The upper
third is an empty, smooth night sky. Calm sea fills the bottom, with one long
streak of moonlight running toward the viewer.
<design>: High-resolution, richly detailed pixel art with fine, small pixels
and a vivid palette of many colours: carefully shaded rocks, glittering
moonlight on every ripple, a few stars and soft glowing light, like modern
indie-game key art. Full-bleed, no border, no text, no letters or numbers, no
clock, no watermark.
<colours>: About 50% deep navy and indigo sky, 25% near-black blue rock and
sea, a cream-white moon and moonlight streak as the lightest tones, and one
small warm amber light at the top of the lighthouse.`

// seedSetups are written into agents.db at startup (see agentStore.seed).
// Bump Version when changing one so phones pick the change up.
var seedSetups = []agentSetup{{
	ID:              defaultSetupID,
	SystemPrompt:    imageGenPrompt,
	FlashModel:      gatewayPrefix + "z-ai/glm-5.3-flash",
	MidModel:        gatewayPrefix + "anthropic/claude-sonnet-5.5",
	FrontierModel:   gatewayPrefix + "anthropic/claude-opus-5.5",
	ImageDraftModel: "black-forest-labs/flux-3-image-klein",
	ImageFinalModel: "openai/gpt-image-2.5-sunburst",
	Tools:           []string{"image_generate", "image_recolour", "palette_pick", "image_look"},
	SpendCapUSD:     0.50,
	Version:         6,
}}

// imageRecord is one row of the images table.
type imageRecord struct {
	ID        string   `json:"id"`
	Path      string   `json:"path"`
	Prompt    string   `json:"prompt,omitempty"`
	Model     string   `json:"model,omitempty"`
	Quality   string   `json:"quality,omitempty"`
	CostUSD   float64  `json:"cost_usd,omitempty"`
	ParentID  string   `json:"parent_id,omitempty"`
	ChatID    string   `json:"chat_id,omitempty"`
	SetupID   string   `json:"setup_id,omitempty"`
	CreatedAt int64    `json:"created_at,omitempty"`
	Palette   *palette `json:"palette,omitempty"`
}

func (st *agentStore) saveImage(r imageRecord) error {
	pal := ""
	if r.Palette != nil {
		b, _ := json.Marshal(r.Palette)
		pal = string(b)
	}
	_, err := st.db.Exec(`INSERT INTO images (id, path, prompt, model, quality, cost_usd, parent_id, chat_id, setup_id, created_at, palette)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET path = excluded.path, prompt = excluded.prompt, model = excluded.model,
			quality = excluded.quality, cost_usd = excluded.cost_usd, parent_id = excluded.parent_id,
			chat_id = excluded.chat_id, setup_id = excluded.setup_id, created_at = excluded.created_at, palette = excluded.palette`,
		r.ID, r.Path, r.Prompt, r.Model, r.Quality, r.CostUSD, r.ParentID, r.ChatID, r.SetupID, r.CreatedAt, pal)
	return err
}

func (st *agentStore) setPalette(id string, p palette) error {
	b, _ := json.Marshal(p)
	_, err := st.db.Exec(`UPDATE images SET palette = ? WHERE id = ?`, string(b), id)
	return err
}

// findImage looks an image up by id, file name or path.
func (st *agentStore) findImage(ref string) (imageRecord, error) {
	var r imageRecord
	var pal string
	err := st.db.QueryRow(`SELECT id, path, prompt, model, quality, cost_usd, parent_id, chat_id, setup_id, created_at, palette
		FROM images WHERE id = ? OR path = ? OR path LIKE '%/' || ? ORDER BY id = ? DESC LIMIT 1`, ref, ref, ref, ref).
		Scan(&r.ID, &r.Path, &r.Prompt, &r.Model, &r.Quality, &r.CostUSD, &r.ParentID, &r.ChatID, &r.SetupID, &r.CreatedAt, &pal)
	if err != nil {
		return imageRecord{}, err
	}
	if pal != "" {
		var p palette
		if json.Unmarshal([]byte(pal), &p) == nil {
			r.Palette = &p
		}
	}
	return r, nil
}

// resolveImage turns a tool's image argument into a record: a known image by
// id, file name or path, or any image in shared storage by its absolute path,
// which gets a row of its own ("photo-" and a hash of the path).
func (st *agentStore) resolveImage(ref string) (imageRecord, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return imageRecord{}, errors.New("image is required: an image id from an earlier result, or a file path")
	}
	if r, err := st.findImage(ref); err == nil {
		return r, nil
	} else if !errors.Is(err, sql.ErrNoRows) {
		return imageRecord{}, err
	}
	if !strings.HasPrefix(ref, "/storage/") && !strings.HasPrefix(ref, "/sdcard/") {
		return imageRecord{}, fmt.Errorf("unknown image %q: use an image id from an earlier tool result, or an absolute path such as /storage/emulated/0/DCIM/Camera/x.jpg", ref)
	}
	if strings.Contains(ref, "..") {
		return imageRecord{}, fmt.Errorf("bad path %q", ref)
	}
	sum := sha1.Sum([]byte(ref))
	r := imageRecord{ID: "photo-" + hex.EncodeToString(sum[:5]), Path: ref, CreatedAt: unixNow()}
	if err := st.saveImage(r); err != nil {
		return imageRecord{}, err
	}
	return r, nil
}

// imageTools is one setup kernel's image tools.
type imageTools struct {
	s     *server
	setup agentSetup
	key   string
	ref   *kernelRef
}

type imageGenerateArgs struct {
	Prompt  string `json:"prompt" jsonschema:"description=The image prompt in <scene>: … <design>: … <colours>: … form,minLength=1,maxLength=4000"`
	Quality string `json:"quality,omitempty" jsonschema:"description=draft (cheap and quick; the default) or final (high quality; only when the user asks or approves a draft),enum=draft,enum=final"`
	Model   string `json:"model,omitempty" jsonschema:"description=An image model id the user picked in [Options]; empty for the setup's draft or final model"`
	Shape   string `json:"shape,omitempty" jsonschema:"description=Aspect ratio; 9:16 (the default) is a phone wallpaper,enum=9:16,enum=2:3,enum=1:1,enum=3:2"`
}

type imageRecolourArgs struct {
	Image              string `json:"image" jsonschema:"description=The image id from an earlier tool result (or an absolute file path),minLength=1"`
	PaletteDescription string `json:"palette_description" jsonschema:"description=The new colours in words, e.g. dusk: coral sky, deep teal shadows, one cream highlight,minLength=1,maxLength=1000"`
	Quality            string `json:"quality,omitempty" jsonschema:"description=draft (the default) or final,enum=draft,enum=final"`
}

type lookArgs struct {
	Image string `json:"image" jsonschema:"description=The image id from an earlier tool result or the absolute path of an attached image,minLength=1"`
}

// imageVisionPrompt is the vision delegate's system prompt for image_look.
const imageVisionPrompt = `You are the eyes of an agent that writes prompts for image models and cannot see. Describe the attached picture so it could write a prompt that recreates or varies it: the subject and every main object, where each sits in the frame, the viewpoint, the style or medium (photo, pixel art, painting, poster, line art; its texture and level of detail), and the colours by name with rough area shares and which tone is lightest. Plain sentences, under 180 words, no preamble.`

type paletteArgs struct {
	Image string `json:"image" jsonschema:"description=The image id from an earlier tool result (or an absolute file path),minLength=1"`
}

// imageResult is what image_generate and image_recolour return to the model
// and send to the client as an "image" event.
type imageResult struct {
	ID       string  `json:"id"`
	File     string  `json:"file"`
	Path     string  `json:"path"`
	Model    string  `json:"model"`
	Quality  string  `json:"quality"`
	CostUSD  float64 `json:"cost_usd"`
	Width    int     `json:"width,omitempty"`
	Height   int     `json:"height,omitempty"`
	ParentID string  `json:"parent_id,omitempty"`
	Note     string  `json:"note,omitempty"`
}

func checkQuality(q string) (string, error) {
	switch q = strings.ToLower(strings.TrimSpace(q)); q {
	case "":
		return "draft", nil
	case "draft", "final":
		return q, nil
	}
	return "", fmt.Errorf("quality must be draft or final, not %q", q)
}

// checkShape returns a valid shape, 9:16 when empty.
func checkShape(sh string) (string, error) {
	sh = strings.TrimSpace(sh)
	if sh == "" {
		return "9:16", nil
	}
	for _, ok := range imageShapes {
		if sh == ok {
			return sh, nil
		}
	}
	return "", fmt.Errorf("shape must be one of %s, not %q", strings.Join(imageShapes, ", "), sh)
}

func (it *imageTools) modelFor(quality string) string {
	if quality == "final" || it.setup.ImageDraftModel == "" {
		return it.setup.ImageFinalModel
	}
	return it.setup.ImageDraftModel
}

func (it *imageTools) generateTool() *tools.ToolDef {
	const name = "image_generate"
	desc := "Draw one image (9:16 unless shape says otherwise) from a prompt and save it to the user's Gallery (Pictures/Toroid). " +
		"quality draft is cheap and quick; final is high quality, for when the user asks or approves a draft. " +
		"model picks a specific image model (only one the user chose). Returns the image id, file, model and cost."
	return &tools.ToolDef{Name: name, Description: desc,
		Handler: llm.NewTool(name, desc, func(ctx context.Context, a imageGenerateArgs) (llm.ToolResult, error) {
			prompt := strings.TrimSpace(a.Prompt)
			if prompt == "" || utf8.RuneCountInString(prompt) > maxPromptLen {
				return llm.NewErrorResult(fmt.Sprintf("prompt must be 1 to %d characters", maxPromptLen)), nil
			}
			q, err := checkQuality(a.Quality)
			if err != nil {
				return llm.NewErrorResult(err.Error()), nil
			}
			shape, err := checkShape(a.Shape)
			if err != nil {
				return llm.NewErrorResult(err.Error()), nil
			}
			model := strings.TrimSpace(a.Model)
			if model != "" {
				if _, ok := imagePriceGuess[model]; !ok {
					return llm.NewErrorResult(fmt.Sprintf("unknown image model %q", model)), nil
				}
			}
			res, err := it.make(ctx, name, prompt, q, model, shape, nil, "")
			if err != nil {
				return llm.NewErrorResult(err.Error()), nil
			}
			return jsonResult(res), nil
		})}
}

func (it *imageTools) recolourTool() *tools.ToolDef {
	const name = "image_recolour"
	desc := "Redraw an existing image in new colours, keeping its composition, shapes and framing. " +
		"Saves the new image to the user's Gallery and records the original as its parent. Returns the new image id, file, model and cost."
	return &tools.ToolDef{Name: name, Description: desc,
		Handler: llm.NewTool(name, desc, func(ctx context.Context, a imageRecolourArgs) (llm.ToolResult, error) {
			colours := strings.TrimSpace(a.PaletteDescription)
			if colours == "" || utf8.RuneCountInString(colours) > 1000 {
				return llm.NewErrorResult("palette_description must be 1 to 1000 characters"), nil
			}
			q, err := checkQuality(a.Quality)
			if err != nil {
				return llm.NewErrorResult(err.Error()), nil
			}
			st, err := it.s.agentStore()
			if err != nil {
				return llm.NewErrorResult(err.Error()), nil
			}
			src, err := st.resolveImage(a.Image)
			if err != nil {
				return llm.NewErrorResult(err.Error()), nil
			}
			ref, err := it.s.loadImagePNG(ctx, src.Path, recolourMaxSide)
			if err != nil {
				return llm.NewErrorResult(err.Error()), nil
			}
			prompt := "Recolour this image. Keep the composition exactly as it is: every shape, outline, texture, " +
				"the framing and the empty areas stay where they are. Change only the colours, to this palette: " + colours +
				". Keep its style and level of detail, full-bleed, with no text, no border and no watermark."
			res, err := it.make(ctx, name, prompt, q, "", "9:16", ref, src.ID)
			if err != nil {
				return llm.NewErrorResult(err.Error()), nil
			}
			return jsonResult(res), nil
		})}
}

func (it *imageTools) paletteTool() *tools.ToolDef {
	const name = "palette_pick"
	desc := "Read an image's dominant colours (about 12, with the share of the image each covers) and turn them into a UI palette " +
		"with the roles bg, surface, line, text, muted, accent, accent_hi, warn, bad, tint1, tint2, tint3. " +
		"Text is checked to read on bg (contrast at least 4.5:1). The palette is stored with the image; nothing is applied anywhere."
	return &tools.ToolDef{Name: name, Description: desc,
		Handler: llm.NewTool(name, desc, func(ctx context.Context, a paletteArgs) (llm.ToolResult, error) {
			if it.ref.remaining() <= 0 {
				return llm.NewErrorResult("this chat has reached its spend cap"), nil
			}
			if cs := streamFrom(ctx); cs != nil {
				cs.activity("tool", it.setup.flash(), name)
			}
			rec, p, err := it.s.paletteFor(ctx, a.Image, it.setup.flash(), it.key, true)
			it.ref.bill(p.CostUSD)
			if err != nil {
				return llm.NewErrorResult(err.Error()), nil
			}
			return jsonResult(map[string]any{"image": rec.ID, "roles": p.Roles, "light": p.Light, "colours": p.Colours, "contrast": p.Contrast, "fixed": p.Fixed}), nil
		})}
}

func (it *imageTools) lookTool() *tools.ToolDef {
	const name = "image_look"
	desc := "See an image: returns a description of its subject, layout, style and colours. " +
		"Takes an image id from an earlier tool result or the absolute path of an image the user attached."
	return &tools.ToolDef{Name: name, Description: desc,
		Handler: llm.NewTool(name, desc, func(ctx context.Context, a lookArgs) (llm.ToolResult, error) {
			st, err := it.s.agentStore()
			if err != nil {
				return llm.NewErrorResult(err.Error()), nil
			}
			src, err := st.resolveImage(a.Image)
			if err != nil {
				return llm.NewErrorResult(err.Error()), nil
			}
			data, err := it.s.loadImagePNG(ctx, src.Path, recolourMaxSide)
			if err != nil {
				return llm.NewErrorResult(err.Error()), nil
			}
			res := llm.NewTextResult(fmt.Sprintf("Image %s (%s).", src.ID, filepath.Base(src.Path)))
			res.Files = []llm.FilePart{{Filename: "image.png", MediaType: "image/png", Data: data}}
			return res, nil
		})}
}

func jsonResult(v any) llm.ToolResult {
	b, _ := json.Marshal(v)
	return llm.NewTextResult(string(b))
}

// make runs one images call, bills it to the chat, saves the image to the
// Gallery and records it. model overrides the setup's model for quality when
// set; shape is the aspect ratio.
func (it *imageTools) make(ctx context.Context, tool, prompt, quality, model, shape string, ref []byte, parentID string) (imageResult, error) {
	if model == "" {
		model = it.modelFor(quality)
	}
	if model == "" {
		return imageResult{}, fmt.Errorf("setup %s has no %s image model", it.setup.ID, quality)
	}
	guess, ok := imagePriceGuess[model]
	if !ok {
		guess = unknownImageCost
	}
	if left := it.ref.remaining(); left < guess {
		return imageResult{}, fmt.Errorf("not enough budget left in this chat for a %s image (about $%.3f, $%.3f left); start a new chat", quality, guess, max(left, 0))
	}
	if cs := streamFrom(ctx); cs != nil {
		cs.activity("image", model, tool)
	}
	started := time.Now()
	if m, ok := shapeFor[model][shape]; ok {
		shape = m
	}
	data, mediaType, cost, err := callImages(ctx, model, it.key, prompt, shape, ref)
	it.ref.bill(cost)
	if err != nil {
		return imageResult{}, err
	}
	res, err := it.s.storeImage(ctx, data, mediaType, imageRecord{
		Prompt: prompt, Model: model, Quality: quality, CostUSD: roundUSD(cost),
		ParentID: parentID, ChatID: chatIDFrom(ctx), SetupID: it.setup.ID,
	})
	if err != nil {
		return imageResult{}, err
	}
	log.Printf("image %s: %s %s in %s, $%.4f", res.File, model, quality, time.Since(started).Round(time.Millisecond), cost)
	if cs := streamFrom(ctx); cs != nil {
		_ = cs.sse.event("image", res)
	}
	return res, nil
}

// callImages posts one request to the gateway's images endpoint and returns
// the decoded image, its media type and OpenRouter's billed cost. A model
// that refuses a parameter ("does not support: quality") is asked again
// without it.
func callImages(ctx context.Context, model, key, prompt, shape string, ref []byte) ([]byte, string, float64, error) {
	body := map[string]any{"model": model, "prompt": prompt, "aspect_ratio": shape, "n": 1, "quality": "high"}
	if ref != nil {
		body["input_references"] = []any{map[string]any{
			"type":      "image_url",
			"image_url": map[string]string{"url": "data:image/png;base64," + base64.StdEncoding.EncodeToString(ref)},
		}}
	}
	ctx, cancel := context.WithTimeout(ctx, imageTimeout)
	defer cancel()
	for attempt := 0; ; attempt++ {
		raw, status, err := postJSON(ctx, gatewayURL("/images"), key, body)
		if err != nil {
			return nil, "", 0, fmt.Errorf("image model %s: %w", model, err)
		}
		if status == http.StatusBadRequest && attempt < 2 {
			if drop := unsupportedParams(apiError(raw)); len(drop) > 0 {
				dropped := false
				for _, p := range drop {
					if p == "quality" || p == "aspect_ratio" || p == "n" {
						if _, ok := body[p]; ok {
							delete(body, p)
							dropped = true
						}
					}
				}
				if dropped {
					continue
				}
			}
		}
		if status != http.StatusOK {
			return nil, "", 0, fmt.Errorf("image model %s: HTTP %d: %s", model, status, oneLine(apiError(raw), 200))
		}
		var out struct {
			Data []struct {
				B64       string `json:"b64_json"`
				URL       string `json:"url"`
				MediaType string `json:"media_type"`
			} `json:"data"`
			Usage struct {
				Cost float64 `json:"cost"`
			} `json:"usage"`
		}
		if err := json.Unmarshal(raw, &out); err != nil {
			return nil, "", 0, fmt.Errorf("image model %s: unreadable reply", model)
		}
		cost := out.Usage.Cost
		if len(out.Data) == 0 {
			return nil, "", cost, fmt.Errorf("image model %s returned no image", model)
		}
		d := out.Data[0]
		b64 := d.B64
		if b64 == "" && strings.HasPrefix(d.URL, "data:") {
			if _, after, ok := strings.Cut(d.URL, ","); ok {
				b64 = after
			}
		}
		img, err := base64.StdEncoding.DecodeString(b64)
		if err != nil || len(img) == 0 {
			return nil, "", cost, fmt.Errorf("image model %s returned no usable image", model)
		}
		if len(img) > maxImageBytes {
			return nil, "", cost, fmt.Errorf("image model %s returned an image over %d MB", model, maxImageBytes>>20)
		}
		mt := d.MediaType
		if mt == "" {
			mt = http.DetectContentType(img)
		}
		return img, mt, cost, nil
	}
}

// unsupportedParams reads the parameter names out of an error such as
// "FLUX 3 Image does not support: quality." or "... does not support: quality, n".
func unsupportedParams(msg string) []string {
	_, after, ok := strings.Cut(msg, "does not support:")
	if !ok {
		return nil
	}
	var out []string
	for _, p := range strings.Split(after, ",") {
		if p = strings.Trim(strings.TrimSpace(p), ".\"'`"); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func postJSON(ctx context.Context, url, key string, body any) ([]byte, int, error) {
	b, err := json.Marshal(body)
	if err != nil {
		return nil, 0, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(b))
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	return raw, resp.StatusCode, err
}

// imageExt is the file extension for a media type.
func imageExt(mediaType string) string {
	switch strings.ToLower(strings.TrimSpace(strings.Split(mediaType, ";")[0])) {
	case "image/jpeg", "image/jpg":
		return ".jpg"
	case "image/webp":
		return ".webp"
	}
	return ".png"
}

// newImageName is "toroid-YYYYMMDD-HHMMSS-xxxx" plus ext.
func newImageName(now time.Time, ext string) string {
	var b [2]byte
	_, _ = rand.Read(b[:])
	return "toroid-" + now.Format("20060102-150405") + "-" + hex.EncodeToString(b[:]) + ext
}

// storeImage writes data into the private images dir, has the tool host copy
// it to Pictures/Toroid (and scan it), and records it. Without a reachable
// host (a laptop run) the image stays in the private dir.
func (s *server) storeImage(ctx context.Context, data []byte, mediaType string, rec imageRecord) (imageResult, error) {
	st, err := s.agentStore()
	if err != nil {
		return imageResult{}, err
	}
	name := newImageName(time.Now(), imageExt(mediaType))
	dir := filepath.Join(s.dataDir, "images")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return imageResult{}, err
	}
	private := filepath.Join(dir, name)
	if err := os.WriteFile(private, data, 0o600); err != nil {
		return imageResult{}, err
	}
	res := imageResult{
		ID: strings.TrimSuffix(name, filepath.Ext(name)), File: name, Path: private,
		Model: rec.Model, Quality: rec.Quality, CostUSD: rec.CostUSD, ParentID: rec.ParentID,
	}
	if cfg, _, err := image.DecodeConfig(bytes.NewReader(data)); err == nil {
		res.Width, res.Height = cfg.Width, cfg.Height
	}
	raw, err := s.tools.call(ctx, "image_publish", map[string]string{"src": private, "name": name})
	switch {
	case err == nil:
		var out struct {
			Path string `json:"path"`
		}
		if json.Unmarshal(raw, &out) == nil && out.Path != "" {
			res.Path = out.Path
			_ = os.Remove(private)
		}
	case errors.Is(err, errToolHostDown):
		res.Note = "saved in the app's private storage only: the phone's tool host is not reachable"
	default:
		res.Note = "saved in the app's private storage only: " + err.Error()
	}
	rec.ID, rec.Path, rec.CreatedAt = res.ID, res.Path, unixNow()
	if err := st.saveImage(rec); err != nil {
		return imageResult{}, err
	}
	return res, nil
}

// loadImagePNG has the tool host read an image from shared storage, scaled
// so its longer side is at most maxSide, into a private PNG, and returns it.
func (s *server) loadImagePNG(ctx context.Context, path string, maxSide int) ([]byte, error) {
	raw, err := s.tools.call(ctx, "image_load", map[string]any{"path": path, "max": maxSide})
	if errors.Is(err, errToolHostDown) {
		return nil, errors.New("the phone's image store is not reachable (is Toroid Chat's service running?)")
	}
	if err != nil {
		return nil, err
	}
	var out struct {
		Src string `json:"src"`
	}
	if err := json.Unmarshal(raw, &out); err != nil || !filepath.IsAbs(out.Src) {
		return nil, errors.New("bad reply from the image store")
	}
	defer os.Remove(out.Src)
	return os.ReadFile(out.Src)
}

// paletteFor returns the palette of an image: the stored one unless refresh
// (or it predates light mode), else a new one (stored with the image). model
// is the kernel id of the LLM that assigns roles.
func (s *server) paletteFor(ctx context.Context, ref, model, key string, refresh bool) (imageRecord, palette, error) {
	st, err := s.agentStore()
	if err != nil {
		return imageRecord{}, palette{}, err
	}
	rec, err := st.resolveImage(ref)
	if err != nil {
		return imageRecord{}, palette{}, err
	}
	if rec.Palette != nil && rec.Palette.Light != nil && !refresh {
		p := *rec.Palette
		p.CostUSD = 0
		return rec, p, nil
	}
	data, err := s.loadImagePNG(ctx, rec.Path, paletteMaxSide)
	if err != nil {
		return rec, palette{}, err
	}
	img, err := png.Decode(bytes.NewReader(data))
	if err != nil {
		return rec, palette{}, fmt.Errorf("could not read the image: %v", err)
	}
	p, err := pickPalette(ctx, img, model, key)
	if err != nil {
		return rec, p, err
	}
	p.CostUSD = roundUSD(p.CostUSD)
	if err := st.setPalette(rec.ID, p); err != nil {
		return rec, p, err
	}
	rec.Palette = &p
	log.Printf("palette %s: %d colours, contrast %.2f dark %.2f light, %d tries, %d fixes, $%.6f", rec.ID, len(p.Colours), p.Contrast, p.LightContrast, p.Tries, len(p.Fixes), p.CostUSD)
	return rec, p, nil
}

// handlePalette is POST /api/palette {"image": "<path or id>", "refresh":
// false, "setup": "@image-gen-agent"}: the Gallery's "Pick palette". The
// stored palette comes back at no cost unless refresh is set; a new one uses
// the setup's Flash model.
func (s *server) handlePalette(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Image   string `json:"image"`
		Refresh bool   `json:"refresh"`
		Setup   string `json:"setup"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8192)).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad json"})
		return
	}
	if body.Setup == "" {
		body.Setup = defaultSetupID
	}
	a, _, err := s.setupFor(body.Setup)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "unknown setup"})
		return
	}
	key, _ := s.readKey()
	if key == "" {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "no OpenRouter key"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
	defer cancel()
	rec, p, err := s.paletteFor(ctx, body.Image, a.flash(), key, body.Refresh)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": err.Error(), "cost_usd": p.CostUSD})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"image": rec, "palette": p})
}

// handleImageInfo is GET /api/image?ref=<path, file name or id>: what the
// database knows about an image (prompt, model, cost, parent, palette), or 404.
func (s *server) handleImageInfo(w http.ResponseWriter, r *http.Request) {
	st, err := s.agentStore()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "no image store"})
		return
	}
	rec, err := st.findImage(r.URL.Query().Get("ref"))
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "unknown image"})
		return
	}
	writeJSON(w, http.StatusOK, rec)
}

// imageStartText and imageSummary are the tool lines for the image tools.
func imageStartText(name string) (string, bool) {
	switch name {
	case "image_generate":
		return "… drawing an image", true
	case "image_recolour":
		return "… recolouring an image", true
	case "palette_pick":
		return "… reading the palette", true
	}
	return "", false
}

func imageSummary(name, result string) (string, bool) {
	switch name {
	case "image_generate", "image_recolour":
		var r imageResult
		if json.Unmarshal([]byte(result), &r) != nil || r.File == "" {
			return "✓ " + name, true
		}
		verb := "drew"
		if name == "image_recolour" {
			verb = "recoloured"
		}
		return fmt.Sprintf("✓ %s %s: %s · $%.3f", verb, r.Quality, r.File, r.CostUSD), true
	case "palette_pick":
		var r struct {
			Image    string            `json:"image"`
			Roles    map[string]string `json:"roles"`
			Contrast float64           `json:"contrast"`
		}
		if json.Unmarshal([]byte(result), &r) != nil || r.Roles == nil {
			return "✓ palette", true
		}
		return fmt.Sprintf("✓ palette: bg %s · text %s · accent %s (%.1f:1)", r.Roles["bg"], r.Roles["text"], r.Roles["accent"], r.Contrast), true
	}
	return "", false
}
