package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	toroid "github.com/yashbonde/toroid-kernel"
	"github.com/yashbonde/toroid-kernel/llm"
	"github.com/yashbonde/toroid-kernel/tools"

	_ "modernc.org/sqlite"
)

// Agent setups: a model id that starts with "@" names a setup stored in
// SQLite (agents.db in the data dir) instead of an OpenRouter model. A setup
// is a whole agent: its own system prompt, its own tools, an LLM per class
// (Flash, Mid, Frontier), the image models its tools use, and its own spend
// cap. Every other model id goes down the chat path exactly as before; the
// database is only opened the first time an "@" id is used.
//
// "@name" runs on the setup's Mid model (its Flash model when Mid is empty);
// "@name:flash", "@name:mid" and "@name:frontier" pick a class. The database
// also keeps the images the image tools make (images.go) and the palettes
// palette_pick reads from them (palette.go).

const (
	setupPrefix  = "@"
	agentsDBName = "agents.db"
)

// agentSetup is one row of agent_setups. LLM ids are kernel ids
// ("llmgateway/<openrouter id>"); a bare OpenRouter id is accepted too. Image
// model ids are bare OpenRouter ids, as the images endpoint takes them.
type agentSetup struct {
	ID              string
	SystemPrompt    string
	FlashModel      string
	MidModel        string // "" falls back to FlashModel
	FrontierModel   string // "" falls back to MidModel, then FlashModel
	ImageDraftModel string
	ImageFinalModel string
	Tools           []string // tool names, see setupToolMakers
	SpendCapUSD     float64  // per chat; 0 means the server's cap
	Version         int      // a seeded row is rewritten only by a newer seed
}

// setupIDPattern is the shape of a setup id: "@" and a short lowercase name.
var setupIDPattern = regexp.MustCompile(`^@[a-z0-9][a-z0-9_-]{0,63}$`)

// isSetupModel reports whether id names an agent setup rather than a model.
func isSetupModel(id string) bool { return strings.HasPrefix(id, setupPrefix) }

// splitSetupModel splits "@name" or "@name:tier" into the setup id ("@name")
// and the class ("" for the default).
func splitSetupModel(id string) (setupID, tier string, ok bool) {
	if !isSetupModel(id) {
		return "", "", false
	}
	setupID, tier, _ = strings.Cut(id, ":")
	switch tier {
	case "", tierFlash, tierMid, tierFrontier:
	default:
		return "", "", false
	}
	if !setupIDPattern.MatchString(setupID) {
		return "", "", false
	}
	return setupID, tier, true
}

// llmModel is the kernel model id for a class: Mid by default, falling back
// to Flash when Mid is empty (and Frontier to Mid, then Flash).
func (a agentSetup) llmModel(tier string) string {
	m := a.MidModel
	switch tier {
	case tierFlash:
		m = a.FlashModel
	case tierFrontier:
		m = a.FrontierModel
		if m == "" {
			m = a.MidModel
		}
	}
	if m == "" {
		m = a.FlashModel
	}
	return kernelModelID(m)
}

// flash is the setup's cheapest LLM, for side calls such as palette roles.
func (a agentSetup) flash() string { return kernelModelID(a.FlashModel) }

// kernelModelID puts the gateway prefix on a bare OpenRouter id.
func kernelModelID(id string) string {
	if id == "" || strings.HasPrefix(id, gatewayPrefix) {
		return id
	}
	return gatewayPrefix + id
}

func (a agentSetup) hasTool(name string) bool {
	for _, t := range a.Tools {
		if t == name {
			return true
		}
	}
	return false
}

// agentStore is agents.db: the setups, and the images their tools make.
type agentStore struct {
	db *sql.DB
}

const agentsDDL = `
CREATE TABLE IF NOT EXISTS agent_setups (
	id                TEXT PRIMARY KEY,
	system_prompt     TEXT    NOT NULL,
	flash_model       TEXT    NOT NULL,
	mid_model         TEXT    NOT NULL DEFAULT '',
	frontier_model    TEXT    NOT NULL DEFAULT '',
	image_draft_model TEXT    NOT NULL DEFAULT '',
	image_final_model TEXT    NOT NULL DEFAULT '',
	tools             TEXT    NOT NULL DEFAULT '',
	spend_cap_usd     REAL    NOT NULL DEFAULT 0,
	version           INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS images (
	id         TEXT PRIMARY KEY,
	path       TEXT    NOT NULL UNIQUE,
	prompt     TEXT    NOT NULL DEFAULT '',
	model      TEXT    NOT NULL DEFAULT '',
	quality    TEXT    NOT NULL DEFAULT '',
	cost_usd   REAL    NOT NULL DEFAULT 0,
	parent_id  TEXT    NOT NULL DEFAULT '',
	chat_id    TEXT    NOT NULL DEFAULT '',
	setup_id   TEXT    NOT NULL DEFAULT '',
	created_at INTEGER NOT NULL DEFAULT 0,
	palette    TEXT    NOT NULL DEFAULT ''
);
`

// openAgentStore opens (creating if needed) the database at path and seeds
// the built-in setups.
func openAgentStore(path string) (*agentStore, error) {
	dsn := "file:" + path + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1) // one writer; the store is small and calls are short
	if _, err := db.Exec(agentsDDL); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("init schema: %w", err)
	}
	_ = os.Chmod(path, 0o600)
	st := &agentStore{db: db}
	for _, a := range seedSetups {
		if err := st.seed(a); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("seed %s: %w", a.ID, err)
		}
	}
	return st, nil
}

func (st *agentStore) close() error { return st.db.Close() }

// seed writes a built-in setup. A row already there is rewritten only when
// the seed's version is newer, so edits made in the database survive a
// restart until the built-in setup itself changes.
func (st *agentStore) seed(a agentSetup) error {
	_, err := st.db.Exec(`
INSERT INTO agent_setups (id, system_prompt, flash_model, mid_model, frontier_model,
	image_draft_model, image_final_model, tools, spend_cap_usd, version)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(id) DO UPDATE SET
	system_prompt = excluded.system_prompt, flash_model = excluded.flash_model,
	mid_model = excluded.mid_model, frontier_model = excluded.frontier_model,
	image_draft_model = excluded.image_draft_model, image_final_model = excluded.image_final_model,
	tools = excluded.tools, spend_cap_usd = excluded.spend_cap_usd, version = excluded.version
WHERE agent_setups.version < excluded.version`,
		a.ID, a.SystemPrompt, a.FlashModel, a.MidModel, a.FrontierModel,
		a.ImageDraftModel, a.ImageFinalModel, strings.Join(a.Tools, ","), a.SpendCapUSD, a.Version)
	return err
}

var errNoSetup = errors.New("no such agent setup")

// setup reads one setup by its "@" id.
func (st *agentStore) setup(id string) (agentSetup, error) {
	var a agentSetup
	var toolList string
	err := st.db.QueryRow(`SELECT id, system_prompt, flash_model, mid_model, frontier_model,
		image_draft_model, image_final_model, tools, spend_cap_usd, version
		FROM agent_setups WHERE id = ?`, id).Scan(&a.ID, &a.SystemPrompt, &a.FlashModel, &a.MidModel,
		&a.FrontierModel, &a.ImageDraftModel, &a.ImageFinalModel, &toolList, &a.SpendCapUSD, &a.Version)
	if errors.Is(err, sql.ErrNoRows) {
		return agentSetup{}, errNoSetup
	}
	if err != nil {
		return agentSetup{}, err
	}
	for _, t := range strings.Split(toolList, ",") {
		if t = strings.TrimSpace(t); t != "" {
			a.Tools = append(a.Tools, t)
		}
	}
	if a.FlashModel == "" {
		return agentSetup{}, fmt.Errorf("setup %s has no flash model", id)
	}
	return a, nil
}

// agents opens agents.db on first use. Chats on ordinary models never call it.
func (s *server) agentStore() (*agentStore, error) {
	s.agentsOnce.Do(func() {
		s.agents, s.agentsErr = openAgentStore(filepath.Join(s.dataDir, agentsDBName))
	})
	return s.agents, s.agentsErr
}

// setupFor resolves an "@" model id to its setup and the LLM it runs on.
func (s *server) setupFor(model string) (agentSetup, string, error) {
	id, tier, ok := splitSetupModel(model)
	if !ok {
		return agentSetup{}, "", errNoSetup
	}
	st, err := s.agentStore()
	if err != nil {
		return agentSetup{}, "", err
	}
	a, err := st.setup(id)
	if err != nil {
		return agentSetup{}, "", err
	}
	return a, a.llmModel(tier), nil
}

// knownModel reports whether a chat may use model: a curated model, or an
// "@" id naming a stored setup.
func (s *server) knownModel(model string) bool {
	if isSetupModel(model) {
		_, _, err := s.setupFor(model)
		return err == nil
	}
	_, ok := s.catalog.lookup(model)
	return ok
}

// capFor is the per-chat spend cap for model: a setup's own cap when it sets
// one, else the server's.
func (s *server) capFor(model string) float64 {
	if isSetupModel(model) {
		if a, _, err := s.setupFor(model); err == nil && a.SpendCapUSD > 0 {
			return a.SpendCapUSD
		}
	}
	return s.spendCap
}

// kernelRef lets a setup's tools reach the kernel they belong to, which is
// built after them.
type kernelRef struct {
	mu sync.Mutex
	k  *toroid.Kernel
}

func (r *kernelRef) set(k *toroid.Kernel) {
	r.mu.Lock()
	r.k = k
	r.mu.Unlock()
}

func (r *kernelRef) get() *toroid.Kernel {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.k
}

// bill adds spend outside the kernel's own LLM steps (an image, a palette
// call) to the chat, so it shows in the reply's cost and counts toward the
// spend cap.
func (r *kernelRef) bill(cost float64) {
	if k := r.get(); k != nil && cost > 0 {
		k.UpdateUse(toroid.Usage{Cost: cost, PricingOK: true}, "")
	}
}

// remaining is what the chat may still spend, or +Inf with no cap.
func (r *kernelRef) remaining() float64 {
	k := r.get()
	if k == nil || k.Cfg.MaxTranscriptSpendUSD <= 0 {
		return 1e9
	}
	return k.Cfg.MaxTranscriptSpendUSD - k.RunningCostUSD()
}

// setupToolMakers builds each tool a setup may name.
var setupToolMakers = map[string]func(it *imageTools) *tools.ToolDef{
	"image_generate": (*imageTools).generateTool,
	"image_recolour": (*imageTools).recolourTool,
	"image_look":     (*imageTools).lookTool,
	"palette_pick":   (*imageTools).paletteTool,
}

// newSetupKernel builds the kernel for an "@" model: the setup's prompt and
// tools on its LLM, with the same streaming hooks as a chat kernel.
func (s *server) newSetupKernel(ctx context.Context, model string, history []llm.Message, budget float64, key string) (*toroid.Kernel, error) {
	a, llmModel, err := s.setupFor(model)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(s.workDir, 0o700); err != nil {
		return nil, err
	}
	ref := &kernelRef{}
	it := &imageTools{s: s, setup: a, key: key, ref: ref}
	reg := tools.NewRegistry()
	for _, name := range a.Tools {
		mk, ok := setupToolMakers[name]
		if !ok {
			return nil, fmt.Errorf("setup %s names unknown tool %q", a.ID, name)
		}
		reg.Register(mk(it))
	}
	noSkills, noImages := false, false
	// image_look hands back pixels; the vision delegate (when one is set in
	// Settings) describes them, so the text-only LLM reads words, never images.
	var vision *toroid.VisionDelegate
	if vm := s.settings.get().visionModel(); vm != "" {
		vision = &toroid.VisionDelegate{Model: vm, Prompt: imageVisionPrompt, MaxTokens: 900, ReasoningEffort: "minimal"}
	}
	k, err := toroid.NewKernel(ctx, toroid.Config{
		Model:                llmModel,
		SmallerModel:         a.flash(),
		APIKey:               key,
		WorkDir:              s.workDir,
		Thinking:             toroid.ThinkingNone,
		Tools:                reg,
		IncludeComputerTools: false,
		IncludeSubagentTools: false,
		LoadSkills:           &noSkills,
		// The LLM only writes prompts: image_look's pixels go to the vision
		// delegate, and without one they are dropped.
		ImageInputOverride:    &noImages,
		Vision:                vision,
		MaxIter:               12,
		MaxTokens:             4096,
		TotalContextSize:      32000,
		CompactionBufferSize:  8000,
		MaxTranscriptSpendUSD: budget,
	})
	if err != nil {
		return nil, err
	}
	ref.set(k)
	k.SystemPromptPrefix = a.SystemPrompt
	k.SystemPrompt = ""
	k.History = history
	s.hookKernel(k, llmModel)
	return k, nil
}

// chatIDKey carries the chat a reply belongs to, for the image records.
type chatIDKey struct{}

func withChatID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, chatIDKey{}, id)
}

func chatIDFrom(ctx context.Context) string {
	id, _ := ctx.Value(chatIDKey{}).(string)
	return id
}

// unixNow is time.Now in Unix seconds; a variable for tests.
var unixNow = func() int64 { return time.Now().Unix() }
