package main

import (
	"encoding/json"
	"errors"
	"io/fs"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
)

// Settings: the server-side preferences the UI can change, kept in
// files/settings.json next to the key file. Today that is the vision delegate:
// the separate model that looks at app screenshots so the chat model only
// plans and acts on text.

const settingsFileName = "settings.json"

// defaultVisionModel looks at screenshots unless the user picks another
// model or turns the delegate off. It is the same model as the default chat
// model (the picker's first), DeepSeek V4.1 Flash, which takes images at a
// steady, low price; OpenRouter reports its billed cost per call in
// usage.cost.
const defaultVisionModel = gatewayPrefix + "deepseek/deepseek-v4.1-flash"

// jevVisionModel is always offered as a choice. Jev Router picks a model per
// request, so it can be good, but it is slower (7-13 s a look here) and its
// price varies from call to call.
const jevVisionModel = gatewayPrefix + "typesafe/jev-router"

// appSettings is settings.json. Zero values mean the defaults, so an absent
// or empty file is a valid, default configuration.
type appSettings struct {
	// VisionOff turns the vision delegate off: screenshots then go to the chat
	// model itself when it takes images, and nowhere when it does not.
	VisionOff bool `json:"vision_off,omitempty"`
	// VisionModel overrides defaultVisionModel ("llmgateway/<openrouter id>").
	VisionModel string `json:"vision_model,omitempty"`
}

// settingsStore reads and writes settings.json. It caches the parsed file;
// the server is the only writer.
type settingsStore struct {
	path string

	mu     sync.Mutex
	loaded bool
	cur    appSettings
}

func newSettingsStore(dataDir string) *settingsStore {
	return &settingsStore{path: filepath.Join(dataDir, settingsFileName)}
}

// get returns the current settings, reading the file on first use. A missing
// or unreadable file gives the defaults.
func (st *settingsStore) get() appSettings {
	st.mu.Lock()
	defer st.mu.Unlock()
	if !st.loaded {
		st.loaded = true
		b, err := os.ReadFile(st.path)
		switch {
		case err == nil:
			if err := json.Unmarshal(b, &st.cur); err != nil {
				log.Printf("settings: %v (using defaults)", err)
				st.cur = appSettings{}
			}
		case !errors.Is(err, fs.ErrNotExist):
			log.Printf("settings: %v (using defaults)", err)
		}
	}
	return st.cur
}

func (st *settingsStore) set(v appSettings) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	if err := writeFileAtomic(st.path, append(b, '\n'), 0o600); err != nil {
		return err
	}
	st.cur, st.loaded = v, true
	return nil
}

// visionModel is the vision delegate's model id, or "" when it is off.
func (v appSettings) visionModel() string {
	if v.VisionOff {
		return ""
	}
	if v.VisionModel != "" {
		return v.VisionModel
	}
	return defaultVisionModel
}

// visionIDRe is the shape of an OpenRouter model id behind gatewayPrefix:
// "vendor/model", optionally with a ":variant".
var visionIDRe = regexp.MustCompile(`^llmgateway/[a-z0-9][a-z0-9._-]*/[a-z0-9][a-z0-9._:-]*$`)

// visionChoice is one model the settings UI can offer as the vision delegate.
type visionChoice struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// settingsView is GET /api/settings and the reply to POST.
type settingsView struct {
	VisionEnabled      bool           `json:"vision_enabled"`
	VisionModel        string         `json:"vision_model"` // the model used when enabled
	DefaultVisionModel string         `json:"default_vision_model"`
	VisionModels       []visionChoice `json:"vision_models"` // suggestions, default first
}

func (s *server) settingsView() settingsView {
	cur := s.settings.get()
	model := cur.VisionModel
	if model == "" {
		model = defaultVisionModel
	}
	defaultName := "DeepSeek V4.1 Flash"
	if m, ok := s.catalog.lookup(defaultVisionModel); ok {
		defaultName = m.Name
	}
	choices := []visionChoice{{ID: defaultVisionModel, Name: defaultName}, {ID: jevVisionModel, Name: "Jev Router (TypeSafe)"}}
	for _, m := range s.catalog.list() {
		if m.Vision && m.ID != defaultVisionModel && m.ID != jevVisionModel {
			choices = append(choices, visionChoice{ID: m.ID, Name: m.Name})
		}
	}
	return settingsView{
		VisionEnabled:      !cur.VisionOff,
		VisionModel:        model,
		DefaultVisionModel: defaultVisionModel,
		VisionModels:       choices,
	}
}

func (s *server) handleGetSettings(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.settingsView())
}

// handlePostSettings changes any of the fields sent; absent fields keep their
// value. vision_model "" restores the default. A chat picks the change up on
// its next message (ensureKernel rebuilds its kernel, keeping the history).
func (s *server) handlePostSettings(w http.ResponseWriter, r *http.Request) {
	var body struct {
		VisionEnabled *bool   `json:"vision_enabled"`
		VisionModel   *string `json:"vision_model"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad json"})
		return
	}
	next := s.settings.get()
	if body.VisionEnabled != nil {
		next.VisionOff = !*body.VisionEnabled
	}
	if body.VisionModel != nil {
		id := strings.TrimSpace(*body.VisionModel)
		if id != "" && !strings.HasPrefix(id, gatewayPrefix) {
			id = gatewayPrefix + id // accept a bare OpenRouter id too
		}
		if id != "" && !visionIDRe.MatchString(id) {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "vision_model must be an OpenRouter model id such as typesafe/jev-router"})
			return
		}
		if id != "" {
			if vision, known := s.catalog.acceptsImages(id); known && !vision {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": "that model does not take images on OpenRouter"})
				return
			}
		}
		if id == defaultVisionModel {
			id = ""
		}
		next.VisionModel = id
	}
	if err := s.settings.set(next); err != nil {
		log.Printf("settings: save: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "could not save settings"})
		return
	}
	log.Printf("settings: vision delegate %q", next.visionModel())
	writeJSON(w, http.StatusOK, s.settingsView())
}
