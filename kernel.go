package toroid

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"text/template"
	"time"

	"github.com/yashbonde/toroid-kernel/tools"

	"charm.land/fantasy"
	"charm.land/fantasy/providers/anthropic"
	"charm.land/fantasy/providers/google"
)

// Thinking controls the model's thinking budget.
type Thinking string

const (
	ThinkingNone Thinking = "none" // disable thinking (budget=0)
	ThinkingLow  Thinking = "low"  // ~1k tokens
	ThinkingHigh Thinking = "high" // ~8k tokens
)

// errQueueInterrupt is returned from OnStepFinish to abort agent.Stream when
// queued messages are waiting. It is never propagated to callers.
var errQueueInterrupt = errors.New("queue interrupt")

// Kernel is the agentic orchestrator powered by Fantasy.
type Kernel struct {
	Cfg              Config
	Provider         fantasy.Provider
	LM               fantasy.LanguageModel
	SmallerLM        fantasy.LanguageModel // optional; used for compact + subagents when set
	Hooks            *HookRegistry
	Tools            *tools.Registry
	Store            *Store
	seq              atomic.Uint64
	SystemPrompt     string
	History          []fantasy.Message
	StepUsage        []Usage          // per-step token usage, index-aligned with StepHistoryStart
	StepHistoryStart []int            // history index where each step's messages begin
	Sessions         map[string]Usage // sessionID -> total tokens used (self + subagents)
	usageMu          sync.Mutex
	FantasyAgentOpts []fantasy.AgentOption
	currentTokens    int
	runningCostUSD   float64
	todoDB           *sql.DB
	mcpClients       []io.Closer // open connections to configured MCP servers, closed in Close()

	// message queue — callers enqueue messages that are injected at the next
	// safe interruption point (OnStepFinish), causing the stream to restart
	// with the queued messages appended to history.
	messageQueue []string
	queueMu      sync.Mutex

	// run-state — supports background agents (§12.4). runMu serializes the agent
	// loop so Stream and Wake never mutate history concurrently; running reports
	// whether a loop is currently active; lastWriter is reused when an idle
	// kernel is woken by a background completion; bgSeq numbers background tasks.
	runMu      sync.Mutex
	wakeMu     sync.Mutex
	running    atomic.Bool
	lastWriter io.Writer
	bgSeq      atomic.Uint64
}

// Config holds all options for creating a Kernel.
type Config struct {
	Provider fantasy.Provider `json:"provider,omitempty" description:"llm provider"`
	// Model is the primary agent model (provider/model form).
	Model string `json:"model" description:"primary llm model name" default:"anthropic/claude-haiku-4-5"`
	// SmallerModel is an optional cheaper model for cost-sensitive work:
	// conversation compaction and subagents (sync + async). Empty means those
	// paths use Model. Prefer the same provider (or llmgateway) as Model so the
	// single APIKey authenticates both.
	SmallerModel   string    `json:"smaller_model,omitempty" description:"cheaper model for compaction and subagents; empty = use Model"`
	APIKey         string    `json:"api_key,omitempty" description:"API key for the provider"`
	SessionID      string    `json:"session_id,omitempty" description:"unique identifier for the session"`
	WorkDir        string    `json:"work_dir" description:"working directory" default:"current directory"`
	// MaxIter caps tool-call steps per turn. Kept intentionally modest so a
	// thrashing model cannot burn dozens of full-context steps by default.
	MaxIter        int       `json:"max_iter" description:"max tool-call iterations" default:"25"`
	MaxRepeatCalls int       `json:"max_repeat_calls" description:"stop after this many consecutive identical tool calls with identical results (loop guard); 0 disables" default:"3"`
	Thinking       Thinking  `json:"thinking" description:"thinking budget: none | low | high" default:"none"`
	ThinkingWriter io.Writer `json:"-"`

	// Tools
	IncludeComputerTools bool            `json:"include_computer_tools" description:"if true, include the computer tools" default:"true"`
	Tools                *tools.Registry `json:"tools,omitempty" description:"custom tools"`

	// Skills: if unset or true, scan ~/.toroid/skills/*.md at startup for
	// skill metadata (name + description only) and register the skill tool
	// so the model can load a skill's full body on demand.
	LoadSkills *bool `json:"load_skills,omitempty" description:"scan ~/.toroid/skills/ for skill metadata at startup and register the skill tool" default:"true"`

	// MCP: remote Model Context Protocol servers to connect at startup. Each
	// server's tools are discovered via tools/list and registered into the
	// kernel's registry, name-prefixed per server to avoid collisions.
	MCPServers []tools.MCPServerConfig `json:"mcp_servers,omitempty" description:"remote MCP servers to connect and register tools from"`

	// Trace/span hierarchy
	TraceID         string `json:"trace_id,omitempty"`          // inherited from parent; root sets TraceID = SessionID
	ParentSpanID    string `json:"parent_span_id,omitempty"`    // parent kernel's SessionID
	PreviousTraceID string `json:"previous_trace_id,omitempty"` // previous trace id when compaction is triggered

	// Persistence
	Save bool `json:"save" description:"persist events, costs and metadata to the SQLite store" default:"false"`

	// Session management
	Resume bool `json:"resume" description:"if true, load existing session history and continue" default:"false"`

	// compaction / context hygiene — defaults favour earlier prune and a
	// modest effective window so long agentic turns do not re-pay max context
	// every step until the 300k ceiling.
	CompactionBufferSize int `json:"compaction_buffer_size" description:"tokens reserved below TotalContextSize before auto-compact fires" default:"50000"`
	ToolCallPrunedSize   int `json:"tool_call_prune" description:"token budget of recent tool history kept untrimmed" default:"20000"`
	TotalContextSize     int `json:"total_context_size" description:"total context window size" default:"200000"`

	// PromptCache requests provider prompt caching when supported (Anthropic
	// ephemeral cache_control breakpoints on the system message, last tool
	// definition, and recent messages). Default true. No-op for providers that
	// ignore these options (OpenAI often auto-caches; Google uses a different API).
	PromptCache *bool `json:"prompt_cache,omitempty" description:"request provider prompt caching when supported" default:"true"`

	// logging flags
	AttachLoggerHooks *bool `json:"attach_logger_hooks,omitempty" description:"automatically attach logger hooks" default:"false"`
	ShowHistory       *bool `json:"show_history" description:"print history" default:"false"`
}

// NewKernel creates and wires up a new Kernel.
func NewKernel(ctx context.Context, cfg Config) (*Kernel, error) {
	// priority cfg defaults
	if cfg.APIKey == "" {
		cfg.APIKey = os.Getenv("GEMINI_TOKEN")
	}
	if cfg.SessionID == "" {
		cfg.SessionID = NewSessionID()
	}
	if cfg.WorkDir == "" {
		cwd, err := os.Getwd()
		if err != nil {
			return nil, err
		}
		runDir, err := RunnerDir(cwd, cfg.SessionID)
		if err != nil {
			return nil, err
		}
		cfg.WorkDir = runDir
	}
	// Root kernel: TraceID == SessionID
	if cfg.TraceID == "" {
		cfg.TraceID = cfg.SessionID
	}

	// Apply context-window defaults. The struct-tag `default` values are NOT
	// auto-applied, so a caller that omits these leaves them at 0 — which makes
	// overContextThreshold treat threshold = TotalContextSize-CompactionBufferSize
	// = 0 and fire on the very first non-empty turn, spuriously compacting and
	// restarting the agent loop every turn. That restart re-enters agent.Stream
	// with an assistant-tail history and an empty prompt, which the provider
	// rejects ("prompt can't be empty when the last message is not a user or tool
	// message") — breaking structured-generation runs outright.
	if cfg.TotalContextSize <= 0 {
		cfg.TotalContextSize = 200000
	}
	if cfg.CompactionBufferSize <= 0 {
		cfg.CompactionBufferSize = 50000
	}
	if cfg.ToolCallPrunedSize <= 0 {
		cfg.ToolCallPrunedSize = 20000
	}
	if cfg.MaxIter <= 0 {
		cfg.MaxIter = 25
	}

	// Open the shared SQLite DB only when persistence is requested.
	var todoDB *sql.DB
	if cfg.Save {
		todoDB, _ = openDefaultSQL()
	}

	// Kernel object
	k := &Kernel{
		Cfg:           cfg,
		Hooks:         &HookRegistry{},
		Sessions:      map[string]Usage{},
		currentTokens: 0,
		todoDB:        todoDB,
		Tools:         tools.NewRegistry(),
	}

	getDescription := func(name string) string {
		b, _ := readPrompt(name + ".tool.tmpl")
		lines := strings.Split(string(b), "\n")
		if len(lines) > 1 {
			return lines[1]
		}
		return "Tool " + name
	}

	// register all tools
	if cfg.IncludeComputerTools {
		k.Tools.Register(tools.NewReadTool(k, getDescription("read")))
		k.Tools.Register(tools.NewWriteTool(k, getDescription("write")))
		k.Tools.Register(tools.NewLsTool(k, getDescription("ls")))
		k.Tools.Register(tools.NewBashTool(k, getDescription("bash")))
		k.Tools.Register(tools.NewEditTool(k, getDescription("edit")))
		k.Tools.Register(tools.NewGlobTool(k, getDescription("glob")))
		k.Tools.Register(tools.NewGrepTool(k, getDescription("grep")))
		k.Tools.Register(tools.NewMultiEditTool(k, getDescription("multiedit")))
		k.Tools.Register(tools.NewNotifyTool(k, getDescription("notify")))
		k.Tools.Register(tools.NewSubagentTool(k, getDescription("subagent")))
		k.Tools.Register(newSubagentAsyncTool(k, getDescription("subagent")))
		k.Tools.Register(tools.NewTodoWriteTool(k, k.todoDB, getDescription("todowrite")))
		k.Tools.Register(tools.NewTodoReadTool(k, k.todoDB, getDescription("todoread")))
	}

	if cfg.Tools != nil {
		for _, t := range cfg.Tools.Tools() {
			k.Tools.Register(t)
		}
	}

	ApplyDefaultDataTypes(&cfg) // cfg OR default_cfg

	// Skills: scan ~/.toroid/skills/ for metadata only (progressive
	// disclosure). Full skill bodies are loaded lazily via the skill tool,
	// either by the model recognizing a relevant skill or by the user naming
	// one explicitly in their prompt.
	var skills []SkillMeta
	if cfg.LoadSkills == nil || *cfg.LoadSkills {
		skills, _ = discoverSkills()
		k.Tools.Register(tools.NewSkillTool(k, getDescription("skill")))
	}

	// MCP: connect each configured remote server and register its tools.
	// Connections stay open for the kernel's lifetime and are closed in Close().
	for _, sc := range cfg.MCPServers {
		client, err := tools.ConnectMCPServer(ctx, k.Tools, sc)
		if err != nil {
			return nil, fmt.Errorf("mcp server %q: %w", sc.BaseURL, err)
		}
		k.mcpClients = append(k.mcpClients, client)
	}

	// Initialize the SQLite trace Store (only when save is set)
	if cfg.Save {
		store, err := NewStore()
		if err != nil {
			return nil, err
		}
		k.Store = store

		_ = store.SaveSpanMeta(SpanMeta{
			SpanID:       cfg.SessionID,
			TraceID:      cfg.TraceID,
			ParentSpanID: cfg.ParentSpanID,
			Model:        cfg.Model,
			StartedAt:    time.Now().UnixNano(),
		})
		if cfg.TraceID == cfg.SessionID {
			_ = store.SaveTraceMeta(TraceMeta{
				TraceID:         cfg.TraceID,
				PreviousTraceID: cfg.PreviousTraceID,
				StartedAt:       time.Now().UnixNano(),
			})
		} else {
			// Restore conversation history by replaying stored events (post-last-compaction only).
			if msgs, err2 := ReconstructHistory(cfg.TraceID, cfg.SessionID, "", cfg.WorkDir); err2 == nil && len(msgs) > 0 {
				k.History = msgs
				k.Logf("[resume] reconstructed %d history messages from events for trace %s", len(msgs), cfg.TraceID)
			} else if err2 != nil {
				k.LogErr("[resume] failed to reconstruct history: %v", err2)
			}
		}
	}

	// Load system prompt (includes SmallerModel routing note when set)
	systemPrompt, err := buildSystemPrompt(cfg.WorkDir, skills, cfg.Model, cfg.SmallerModel)
	if err != nil {
		return nil, err
	}
	k.SystemPrompt = systemPrompt

	// Load the primary model
	if cfg.Provider == nil {
		p, err := NewProviderFromLLMId(cfg.Model, cfg.APIKey)
		if err != nil {
			return nil, fmt.Errorf("failed to initialize provider for %q: %w", cfg.Model, err)
		}
		cfg.Provider = p
	}
	model, err := languageModel(ctx, cfg.Provider, cfg.Model)
	if err != nil {
		return nil, err
	}
	k.LM = model
	k.Provider = cfg.Provider

	// Optional cheaper model for compaction + subagents
	if cfg.SmallerModel != "" && cfg.SmallerModel != cfg.Model {
		smallLM, err := resolveLanguageModel(ctx, cfg.APIKey, cfg.SmallerModel)
		if err != nil {
			return nil, fmt.Errorf("failed to initialize SmallerModel %q: %w", cfg.SmallerModel, err)
		}
		k.SmallerLM = smallLM
	}

	// Build Fantasy Tools
	var fTools []fantasy.AgentTool
	for _, t := range k.Tools.Tools() {
		fTools = append(fTools, t.AgentTool)
	}
	// Anthropic: mark the last tool with ephemeral cache_control so the tools
	// block can participate in the cached prefix (max 4 breakpoints total).
	if promptCacheEnabled(cfg) && len(fTools) > 0 {
		fTools[len(fTools)-1].SetProviderOptions(anthropicEphemeralCacheOptions())
	}

	// Default AttachLoggerHooks if nil
	if cfg.AttachLoggerHooks != nil && *cfg.AttachLoggerHooks {
		k.OnAll(func(ctx context.Context, e Event) error {
			if e.Kind == EventReasoning {
				return nil
			}
			k.Logf(string(e.Kind) + " " + fmt.Sprintf("%v", e.Payload))
			return nil
		})
	}

	// Initialize Fantasy Agent
	// System prompt is owned by Fantasy (WithSystemPrompt only) — History must
	// not also carry a system message or every step double-bills the system text
	// and busts a stable cache prefix.
	opts := []fantasy.AgentOption{
		fantasy.WithSystemPrompt(systemPrompt),
		fantasy.WithTools(fTools...),
		fantasy.WithMaxRetries(5),
	}
	if promptCacheEnabled(cfg) {
		opts = append(opts, fantasy.WithPrepareStep(preparePromptCacheStep))
	}

	// Loop guards. MaxIter caps the absolute number of tool-call steps;
	// MaxRepeatCalls catches a model spinning on the same call before it ever
	// reaches that cap. Both are wired as fantasy stop conditions so the agent
	// halts cleanly at a step boundary (history stays consistent).
	var stops []fantasy.StopCondition
	if cfg.MaxIter > 0 {
		stops = append(stops, fantasy.StepCountIs(cfg.MaxIter))
	}
	if cfg.MaxRepeatCalls > 0 {
		stops = append(stops, k.repeatCallGuard(cfg.MaxRepeatCalls))
	}
	if len(stops) > 0 {
		opts = append(opts, fantasy.WithStopConditions(stops...))
	}

	// Handle thinking
	if cfg.Thinking != ThinkingNone {
		if cfg.ThinkingWriter != nil {
			k.On(EventReasoning, func(_ context.Context, e Event) error {
				if p, ok := e.Payload.(*ReasoningPayload); ok {
					_, err := fmt.Fprint(cfg.ThinkingWriter, p.Text)
					return err
				}
				return nil
			})
		}
		budget := int64(1024)
		if cfg.Thinking == ThinkingHigh {
			budget = 8192
		}

		config := &google.ThinkingConfig{
			IncludeThoughts: fantasy.Opt(true),
		}

		if strings.Contains(cfg.Model, "gemini-3") {
			level := google.ThinkingLevelLow
			if cfg.Thinking == ThinkingHigh {
				level = google.ThinkingLevelHigh
			}
			config.ThinkingLevel = fantasy.Opt(level)
		} else {
			config.ThinkingBudget = fantasy.Opt(budget)
		}

		opts = append(opts, fantasy.WithProviderOptions(fantasy.ProviderOptions{
			google.Name: &google.ProviderOptions{
				ThinkingConfig: config,
			},
		}))
	}
	k.FantasyAgentOpts = opts
	k.Cfg = cfg

	return k, nil
}

// languageModel resolves a LanguageModel from an already-built provider and a
// provider/model id (strips the provider/ prefix for the API call).
func languageModel(ctx context.Context, p fantasy.Provider, modelID string) (fantasy.LanguageModel, error) {
	name := modelID
	if _, after, ok := strings.Cut(modelID, "/"); ok {
		name = after
	}
	return p.LanguageModel(ctx, name)
}

// resolveLanguageModel builds a provider + language model for modelID using apiKey.
func resolveLanguageModel(ctx context.Context, apiKey, modelID string) (fantasy.LanguageModel, error) {
	p, err := NewProviderFromLLMId(modelID, apiKey)
	if err != nil {
		return nil, err
	}
	return languageModel(ctx, p, modelID)
}

func promptCacheEnabled(cfg Config) bool {
	return cfg.PromptCache == nil || *cfg.PromptCache
}

func anthropicEphemeralCacheOptions() fantasy.ProviderOptions {
	return fantasy.ProviderOptions{
		anthropic.Name: &anthropic.ProviderCacheControlOptions{
			CacheControl: anthropic.CacheControl{Type: "ephemeral"},
		},
	}
}

// preparePromptCacheStep stamps Anthropic ephemeral cache_control breakpoints
// on the stable system message and the last two messages of each step. Fantasy's
// own provider tests use this pattern; non-Anthropic providers ignore the option.
// Anthropic allows at most four breakpoints — system + last 2 leaves room for
// the last-tool breakpoint set at agent construction.
func preparePromptCacheStep(ctx context.Context, options fantasy.PrepareStepFunctionOptions) (context.Context, fantasy.PrepareStepResult, error) {
	msgs := make([]fantasy.Message, len(options.Messages))
	copy(msgs, options.Messages)
	for i := range msgs {
		msgs[i].ProviderOptions = nil
	}
	cacheOpts := anthropicEphemeralCacheOptions()

	lastSystem := -1
	for i, msg := range msgs {
		if msg.Role == fantasy.MessageRoleSystem {
			lastSystem = i
		}
	}
	if lastSystem >= 0 {
		msgs[lastSystem].ProviderOptions = cacheOpts
	}
	// Last two messages (often the growing tail) get breakpoints so incremental
	// turns can still hit a recent cached prefix within Anthropic's limit of 4.
	for i := range msgs {
		if i > len(msgs)-3 {
			msgs[i].ProviderOptions = cacheOpts
		}
	}
	return ctx, fantasy.PrepareStepResult{Messages: msgs}, nil
}

// cheapLM returns the language model for cost-sensitive work (compact, etc.).
func (k *Kernel) cheapLM() fantasy.LanguageModel {
	if k.SmallerLM != nil {
		return k.SmallerLM
	}
	return k.LM
}

// cheapModelID returns the model id used for cost-sensitive work and its pricing.
func (k *Kernel) cheapModelID() string {
	if k.Cfg.SmallerModel != "" {
		return k.Cfg.SmallerModel
	}
	return k.Cfg.Model
}

// recordUsage adds a single LLM call's usage to session cost accounting.
func (k *Kernel) recordUsage(ctx context.Context, u Usage) {
	runningCost := k.UpdateUse(u, "")
	if k.Store != nil {
		_ = k.Store.AppendCost(k.Cfg.TraceID, k.Cfg.SessionID, u.Cost, runningCost)
	}
	_ = k.Fire(ctx, string(EventTurnCost), &TurnCostPayload{
		TurnUsage:    u,
		TurnCostUSD:  u.Cost,
		TotalCostUSD: runningCost,
	})
}

func buildSystemPrompt(workDir string, skills []SkillMeta, model, smallerModel string) (string, error) {
	raw, err := readPrompt("system.tmpl")
	if err != nil {
		return "", err
	}
	tmpl, err := template.New("system").Parse(string(raw))
	if err != nil {
		return "", err
	}

	var buf strings.Builder
	err = tmpl.Execute(&buf, map[string]any{
		"WorkDir":      workDir,
		"Date":         time.Now().Format("2006-01-02 15:04:05"),
		"Skills":       skills,
		"Model":        model,
		"SmallerModel": smallerModel,
	})
	return buf.String(), err
}

// Implement tools.Agent interface
func (k *Kernel) WorkDir() string   { return k.Cfg.WorkDir }
func (k *Kernel) SessionID() string { return k.Cfg.SessionID }
func (k *Kernel) Model() string     { return k.Cfg.Model }

// RunningCostUSD returns the cumulative LLM cost so far in this session.
func (k *Kernel) RunningCostUSD() float64 {
	k.usageMu.Lock()
	defer k.usageMu.Unlock()
	return k.runningCostUSD
}

// ContextUsage returns (used tokens, total context window size).
func (k *Kernel) ContextUsage() (int, int) {
	k.usageMu.Lock()
	defer k.usageMu.Unlock()
	return k.currentTokens, k.Cfg.TotalContextSize
}

func (k *Kernel) Logf(msg string, args ...any) {
	LogInfo("["+k.Cfg.TraceID+"] "+msg, args...)
}

func (k *Kernel) LogErr(msg string, args ...any) {
	LogError("["+k.Cfg.TraceID+"] "+msg, args...)
}

// FireTraceLog emits an EventTraceLog with the given severity and message.
func (k *Kernel) FireTraceLog(ctx context.Context, logType, message string) error {
	return k.Fire(ctx, string(EventTraceLog), &TraceLogPayload{Type: logType, Message: message})
}

func (k *Kernel) Fire(ctx context.Context, kind string, payload any) error {
	event := Event{
		Kind:      EventKind(kind),
		SessionID: k.Cfg.SessionID,
		TraceID:   k.Cfg.TraceID,
		SpanID:    k.Cfg.SessionID,
		EmitTS:    time.Now().UnixNano(),
		Seq:       k.seq.Add(1),
		Payload:   payload,
	}
	if k.Store != nil && event.Kind != EventReasoning {
		_ = k.Store.AppendEvent(k.Cfg.TraceID, k.Cfg.SessionID, event)
	}
	return k.Hooks.Fire(ctx, event)
}

func (k *Kernel) UpdateUse(u Usage, key string) float64 {
	k.usageMu.Lock()
	defer k.usageMu.Unlock()
	if len(key) == 0 {
		k.Sessions[k.Cfg.SessionID] = u
	} else {
		k.Sessions[key] = u
	}
	k.runningCostUSD += u.Cost
	// u is a single step's usage here; its input-side tokens are the size of the
	// request that step sent (the whole conversation so far), so this is a valid
	// snapshot of context-window occupancy. See windowTokens.
	k.currentTokens = windowTokens(u)
	return k.runningCostUSD
}

// windowTokens estimates how full the context window is from a single request's
// usage: the input-side tokens (fresh + cache-read + cache-write are disjoint
// slices of the prompt) plus the tokens generated, which become part of the
// window on the next turn. It must be fed a single step's usage, never an
// agent's summed TotalUsage (which double-counts re-read context across steps).
func windowTokens(u Usage) int {
	return int(u.Input + u.CacheRead + u.CacheWrite + u.Output)
}

// overContextThreshold reports whether context-window occupancy has reached the
// point where we should compact. Shared by the pre-turn check and the in-loop
// check so both use identical logic.
func (k *Kernel) overContextThreshold() bool {
	k.usageMu.Lock()
	defer k.usageMu.Unlock()
	// A non-positive context size means "unbounded" — never report over threshold.
	// Without this guard a zero TotalContextSize makes the threshold 0 and fires on
	// every turn (spurious compaction). NewKernel applies a default, but guard here
	// too so the invariant holds regardless of how Cfg was constructed.
	if k.Cfg.TotalContextSize <= 0 {
		return false
	}
	return k.currentTokens > 0 && k.currentTokens >= k.Cfg.TotalContextSize-k.Cfg.CompactionBufferSize
}

// Enqueue adds a message to the kernel's message queue. It is safe to call
// from any goroutine, including while Stream is running. The message will be
// injected into the conversation at the next OnStepFinish boundary, causing
// the current stream to restart with the message appended to history.
func (k *Kernel) Enqueue(msg string) {
	k.queueMu.Lock()
	k.messageQueue = append(k.messageQueue, msg)
	k.queueMu.Unlock()
}

// drainQueue pops all queued messages and returns them.
func (k *Kernel) drainQueue() []string {
	k.queueMu.Lock()
	msgs := k.messageQueue
	k.messageQueue = nil
	k.queueMu.Unlock()
	return msgs
}

// On registers a hook for an event kind.
func (k *Kernel) On(kind EventKind, fn HookFn) {
	k.Hooks.On(kind, fn)
}

func (k *Kernel) OnAll(fn HookFn) {
	for _, kind := range []EventKind{
		EventSessionStart,
		EventUserPromptSubmit,
		EventPermissionRequest,
		EventPreToolUse,
		EventPostToolUse,
		EventPostToolUseFailure,
		EventSubagentStart,
		EventSubagentStop,
		EventMasterIdle,
		EventNotification,
		EventTaskCompleted,
		EventReasoning,
		EventStop,
		EventPreCompact,
		EventPostCompact,
		EventQueueInterrupt,
		EventSessionEnd,
	} {
		k.On(kind, fn)
	}
}

// Run runs the agent loop and returns the final text response.
// RunOption configures a single Run or Stream call.
type RunOption func(*runOptions)

type runOptions struct {
	schema            *fantasy.Schema
	schemaName        string
	schemaDescription string
}

// WithSchema enables structured generation for this Run/Stream call.
// The model will produce a JSON object matching the given schema instead of
// free text. Run returns the raw JSON; Stream writes it to the writer.
func WithSchema(schema fantasy.Schema, name, description string) RunOption {
	return func(o *runOptions) {
		o.schema = &schema
		o.schemaName = name
		o.schemaDescription = description
	}
}

func (k *Kernel) Run(ctx context.Context, prompt string, opts ...RunOption) (string, UsagePayload, error) {
	var buf strings.Builder
	var usage UsagePayload
	k.On(EventStop, func(ctx context.Context, e Event) error {
		usage = *e.Payload.(*UsagePayload)
		return nil
	})
	err := k.Stream(ctx, prompt, &buf, opts...)
	return buf.String(), usage, err
}

// Stream runs the agent loop and streams the response to the writer.
func (k *Kernel) Stream(ctx context.Context, prompt string, w io.Writer, opts ...RunOption) error {
	// One chat = one Stream/Run: give all of this run's turns a shared gateway
	// trace id so the upstream groups them under a single trace (preserved if the
	// context already carries one, e.g. a subagent running inside a parent chat).
	ctx = WithGatewayTrace(ctx)

	var ro runOptions
	for _, o := range opts {
		o(&ro)
	}
	// Fire session start only once. System prompt is injected by Fantasy via
	// WithSystemPrompt — do not also put it in History (double bill + cache bust).
	if len(k.History) == 0 {
		_ = k.Fire(ctx, string(EventSessionStart), nil)
	}

	// Auto-compact if approaching context limit; important to do this before adding user prompt
	if k.overContextThreshold() {
		k.Logf("auto-compacting: currentTokens=%d threshold=%d", k.currentTokens, k.Cfg.TotalContextSize-k.Cfg.CompactionBufferSize)
		if err := k.Compact(ctx); err != nil {
			return err
		}
	}

	// append user message; important to do this before history validation.
	// parseUserMessage inlines any markdown image refs as file parts and returns
	// the portable (~-rooted) prompt to persist for cross-process resume.
	userMsg, storedPrompt := parseUserMessage(prompt, k.Cfg.WorkDir)
	k.History = append(k.History, userMsg)
	_ = k.Fire(ctx, string(EventUserPromptSubmit), &UserPromptPayload{Prompt: storedPrompt})

	// history validation — last message must be the user turn we just appended.
	if len(k.History) == 0 || k.History[len(k.History)-1].Role != fantasy.MessageRoleUser {
		role := fantasy.MessageRole("")
		if len(k.History) > 0 {
			role = k.History[len(k.History)-1].Role
		}
		k.LogErr("Last history item must be a user message. Got: '%s'", role)
		panic("Last item is not a user message.")
	}

	// Walk StepUsage backwards, accumulating tokens. Steps whose cumulative
	// token total exceeds ToolCallPrunedSize get their history messages trimmed:
	// tool call args are cleared, tool results are truncated.
	k.pruneOldToolCalls()

	// When schema is set, discard the free-text output from the agent loop;
	// only the structured JSON from GenerateObject goes to the caller's writer.
	loopWriter := w
	if ro.schema != nil {
		loopWriter = io.Discard
	}
	if err := k.streamCurrent(ctx, loopWriter); err != nil {
		return err
	}

	if ro.schema != nil {
		// GenerateObject requires the last message to be user/tool role.
		// After the agentic loop the last message is assistant, so append a
		// user turn that asks the model to emit structured output.
		historyForSchema := append(k.History, fantasy.NewUserMessage("Now return your findings in the required JSON format."))
		resp, err := k.LM.GenerateObject(ctx, fantasy.ObjectCall{
			Prompt:            historyForSchema,
			Schema:            *ro.schema,
			SchemaName:        ro.schemaName,
			SchemaDescription: ro.schemaDescription,
		})
		if err != nil {
			return err
		}
		b, err := json.Marshal(resp.Object)
		if err != nil {
			return err
		}
		_, err = w.Write(b)
		return err
	}

	return nil
}

// repeatCallGuard returns a stop condition that halts the agent when the last n
// steps all issued the exact same tool calls (name + input) AND received the
// exact same results.
//
// Keying on the result — not just the arguments — is what keeps legitimate
// polling alive: a poll that observes changing state (a job that flips from
// "pending" to "done", a file that grows, a queue that drains) produces a
// different signature each step and is never tripped. Only a call that repeats
// with identical args and identical output — i.e. making no progress — counts as
// a stuck loop. A poll that genuinely needs to wait should yield between checks
// (sleep/backoff) rather than spin the synchronous agent loop; this guard is the
// backstop for the spin case.
func (k *Kernel) repeatCallGuard(n int) fantasy.StopCondition {
	return func(steps []fantasy.StepResult) bool {
		if n <= 1 || len(steps) < n {
			return false
		}
		var last string
		for i := 0; i < n; i++ {
			sig := stepCallSignature(steps[len(steps)-1-i])
			if sig == "" {
				return false // a step with no tool calls isn't a spin
			}
			if i == 0 {
				last = sig
			} else if sig != last {
				return false
			}
		}
		k.Logf("loop guard: stopping after %d consecutive identical tool calls with identical results", n)
		return true
	}
}

// stepCallSignature builds a stable signature of every tool call in a step,
// pairing each call's (name, input) with its result so that progress-making
// repeats produce distinct signatures. Returns "" if the step made no calls.
func stepCallSignature(step fantasy.StepResult) string {
	results := map[string]string{}
	for _, msg := range step.Messages {
		if msg.Role != fantasy.MessageRoleTool {
			continue
		}
		for _, part := range msg.Content {
			if tr, ok := fantasy.AsMessagePart[fantasy.ToolResultPart](part); ok {
				if txt, ok := tr.Output.(fantasy.ToolResultOutputContentText); ok {
					results[tr.ToolCallID] = txt.Text
				} else {
					results[tr.ToolCallID] = fmt.Sprintf("%v", tr.Output)
				}
			}
		}
	}
	var b strings.Builder
	for _, tc := range step.Content.ToolCalls() {
		b.WriteString(tc.ToolName)
		b.WriteByte('\x00')
		b.WriteString(tc.Input)
		b.WriteByte('\x00')
		b.WriteString(results[tc.ToolCallID])
		b.WriteByte('\n')
	}
	return b.String()
}

// streamCurrent runs the agent loop over the current in-memory history until it
// stops (no pending queued messages). Stream prepares history (compaction,
// appending the user message, pruning) and then calls this; Wake calls it after
// injecting a background completion. runMu serializes the loop so the two never
// race on history.
func (k *Kernel) streamCurrent(ctx context.Context, w io.Writer) error {
	k.runMu.Lock()
	defer k.runMu.Unlock()
	k.running.Store(true)
	k.lastWriter = w
	defer k.running.Store(false)

	// Build Agent and handle streaming and events.
	// The loop restarts agent.Stream() whenever queued messages are injected
	// at a step boundary. A flag is set in OnStepFinish; the stream is allowed
	// to finish normally so all step messages are collected before restarting.
	agent := fantasy.NewAgent(k.LM, k.FantasyAgentOpts...)
	var result *fantasy.AgentResult
	for {
		var shouldInterrupt bool
		var shouldCompact bool
		var interruptedWith []string

		var streamErr error
		result, streamErr = agent.Stream(ctx, fantasy.AgentStreamCall{
			// The user turn always lives in k.History (appended by Stream, or a
			// background completion injected by Wake), so Prompt stays empty —
			// otherwise fantasy would append a duplicate user message.
			Messages: k.History,

			// Per-step cost accounting
			OnStepFinish: func(step fantasy.StepResult) error {
				u := Usage{}
				u.FromFantasyUsage(step.Usage, k.Cfg.Model)
				k.StepUsage = append(k.StepUsage, u)
				k.recordUsage(ctx, u)

				// Check queue at this safe step boundary. We set a flag and let
				// the stream finish naturally so all steps are collected before
				// we inject the queued messages and restart.
				if !shouldInterrupt {
					if queued := k.drainQueue(); len(queued) > 0 {
						shouldInterrupt = true
						interruptedWith = queued
					}
				}
				// Also check context pressure at this boundary. A long agentic turn
				// can balloon the window far past the limit within a single Stream
				// call; checking only between Runs lets it overflow. We flag it and
				// let the stream finish so all step messages are collected, then
				// compact and restart the loop below.
				if !shouldInterrupt && !shouldCompact && k.overContextThreshold() {
					shouldCompact = true
				}
				return nil
			},

			// Live reasoning streaming
			OnReasoningDelta: func(id, text string) error {
				return k.Fire(ctx, string(EventReasoning), &ReasoningPayload{Text: text})
			},

			// Tool call fired the moment the LLM finishes emitting it
			OnToolCall: func(tc fantasy.ToolCallContent) error {
				return k.Fire(ctx, string(EventPreToolUse), &ToolUsePayload{
					CallID: tc.ToolCallID,
					Name:   tc.ToolName,
					Args:   tc.Input,
				})
			},

			// Tool result fired immediately after execution completes
			OnToolResult: func(tr fantasy.ToolResultContent) error {
				resStr := fmt.Sprintf("%v", tr.Result)
				payload := &ToolUseResultPayload{CallID: tr.ToolCallID, Name: tr.ToolName, Result: resStr}
				if strings.HasPrefix(resStr, "Error:") {
					payload.Error = resStr
					return k.Fire(ctx, string(EventPostToolUseFailure), payload)
				}
				return k.Fire(ctx, string(EventPostToolUse), payload)
			},
		})
		if streamErr != nil {
			return streamErr
		}

		if shouldInterrupt {
			// Append this iteration's step messages to history, inject queued
			// messages, fire the interrupt event, then restart.
			for _, step := range result.Steps {
				k.StepHistoryStart = append(k.StepHistoryStart, len(k.History))
				k.History = append(k.History, step.Messages...)
			}
			for _, qm := range interruptedWith {
				k.History = append(k.History, fantasy.NewUserMessage(qm))
			}
			_ = k.Fire(ctx, string(EventQueueInterrupt), &QueueInterruptPayload{
				Messages: interruptedWith,
			})
			continue
		}

		if shouldCompact {
			// Collect this iteration's step messages into history first so the
			// summary covers everything generated so far, then compact (which
			// resets History to the summary) and restart the loop on the smaller
			// window. A queue interrupt takes precedence and is handled above.
			for _, step := range result.Steps {
				k.StepHistoryStart = append(k.StepHistoryStart, len(k.History))
				k.History = append(k.History, step.Messages...)
			}
			if err := k.Compact(ctx); err != nil {
				return err
			}
			continue
		}
		break
	}

	// Append final step messages to history and fire EventAssistantTurn.
	var allStepMsgs []fantasy.Message
	for _, step := range result.Steps {
		k.StepHistoryStart = append(k.StepHistoryStart, len(k.History))
		k.History = append(k.History, step.Messages...)
		allStepMsgs = append(allStepMsgs, step.Messages...)
	}
	if len(allStepMsgs) > 0 {
		if msgBytes, err2 := json.Marshal(allStepMsgs); err2 == nil {
			_ = k.Fire(ctx, string(EventAssistantTurn), &AssistantTurnPayload{
				Messages: json.RawMessage(msgBytes),
			})
		}
	}

	// Update Usage (Per-Turn) — update session map and token count without adding to cost again
	var u Usage
	u.FromFantasyUsage(result.TotalUsage, k.Cfg.Model)
	k.usageMu.Lock()
	k.Sessions[k.Cfg.SessionID] = u
	// currentTokens must track context-window occupancy, NOT the turn's cumulative
	// billed tokens. result.TotalUsage sums every step, so in a multi-step turn it
	// counts each step's re-read of the (cached) context over and over — wildly
	// over-stating how full the window is. The window occupancy is the last step's
	// request size (its input-side tokens) plus what it generated.
	if n := len(result.Steps); n > 0 {
		var last Usage
		last.FromFantasyUsage(result.Steps[n-1].Usage, k.Cfg.Model)
		k.currentTokens = windowTokens(last)
	}
	k.usageMu.Unlock()

	if k.Cfg.ShowHistory != nil && *k.Cfg.ShowHistory {
		// Print history (after usage update so currentTokens is accurate)
		PrettyPrintHistory(k)
	}

	// Fire stop with usage
	usageSnapshot := make(map[string]Usage)
	k.usageMu.Lock()
	for sid, v := range k.Sessions {
		usageSnapshot[sid] = v
	}
	k.usageMu.Unlock()

	_ = k.Fire(ctx, string(EventStop), &UsagePayload{Tokens: usageSnapshot})

	// Record span (and, for a root kernel, trace) end time so the persisted
	// graph carries OTEL-style start/end timestamps.
	if k.Store != nil {
		now := time.Now().UnixNano()
		_ = k.Store.SaveSpanMeta(SpanMeta{SpanID: k.Cfg.SessionID, TraceID: k.Cfg.TraceID, EndedAt: now})
		if k.Cfg.TraceID == k.Cfg.SessionID {
			_ = k.Store.SaveTraceMeta(TraceMeta{TraceID: k.Cfg.TraceID, EndedAt: now})
		}
	}

	// write response and exit
	w.Write([]byte(result.Response.Content.Text()))

	// If no background completions are queued, the kernel is now idle.
	k.queueMu.Lock()
	idle := len(k.messageQueue) == 0
	k.queueMu.Unlock()
	if idle {
		_ = k.Fire(ctx, string(EventMasterIdle), nil)
	}
	return nil
}

// Close releases kernel-held resources. It flushes the trace store; the shared
// SQLite handle itself is process-global and is released on exit. Safe to call
// multiple times and on a nil-store kernel.
func (k *Kernel) Close() error {
	for _, c := range k.mcpClients {
		_ = c.Close()
	}
	if k.Store != nil {
		return k.Store.Close()
	}
	return nil
}

// pruneOldToolCalls walks StepUsage backwards and trims history for steps whose
// cumulative token total exceeds ToolCallPrunedSize.
func (k *Kernel) pruneOldToolCalls() {
	if len(k.StepUsage) == 0 || len(k.StepHistoryStart) != len(k.StepUsage) {
		return
	}
	var accumulated int
	for i := len(k.StepUsage) - 1; i >= 0; i-- {
		u := k.StepUsage[i]
		accumulated += int(u.Input + u.Output)
		if accumulated <= k.Cfg.ToolCallPrunedSize {
			continue
		}
		start := k.StepHistoryStart[i]
		end := len(k.History)
		if i+1 < len(k.StepHistoryStart) {
			end = k.StepHistoryStart[i+1]
		}
		k.trimHistoryRange(start, end, 800)
	}
}

// trimHistoryRange clears tool-call args and truncates tool results in [start, end).
func (k *Kernel) trimHistoryRange(start, end, maxResultChars int) {
	if maxResultChars <= 0 {
		maxResultChars = 800
	}
	if start < 0 {
		start = 0
	}
	if end > len(k.History) {
		end = len(k.History)
	}
	for j := start; j < end; j++ {
		msg := &k.History[j]
		for p, part := range msg.Content {
			switch msg.Role {
			case fantasy.MessageRoleAssistant:
				if tc, ok := fantasy.AsMessagePart[fantasy.ToolCallPart](part); ok {
					tc.Input = "{}"
					msg.Content[p] = tc
				}
			case fantasy.MessageRoleTool:
				if tr, ok := fantasy.AsMessagePart[fantasy.ToolResultPart](part); ok {
					if txt, ok := tr.Output.(fantasy.ToolResultOutputContentText); ok {
						if len(txt.Text) > maxResultChars {
							txt.Text = txt.Text[:maxResultChars] + "… [trimmed]"
							tr.Output = txt
							msg.Content[p] = tr
						}
					}
				}
			}
		}
	}
}

// trimAllToolResults caps every tool result in history. Used before Compact so
// the summary call is not dominated by a single unbounded grep/MCP dump.
func (k *Kernel) trimAllToolResults(maxResultChars int) {
	k.trimHistoryRange(0, len(k.History), maxResultChars)
}

// Compact summarizes the current history and resets it.
func (k *Kernel) Compact(ctx context.Context) error {
	if len(k.History) == 0 {
		return nil
	}

	// Shrink fat tool results before paying for a full-history summarize call.
	// Age-based prune may not have touched the recent (largest) dumps yet.
	k.trimAllToolResults(2000)

	messagesBefore := len(k.History)
	tokensBefore := k.currentTokens
	_ = k.Fire(ctx, string(EventPreCompact), &CompactPayload{
		MessageCount: messagesBefore,
		TokenCount:   tokensBefore,
	})

	prompt, err := readPrompt("compact.kernel.tmpl")
	if err != nil {
		return err
	}

	// Prefer SmallerModel when configured — summarization is cheaper work.
	agentOpts := []fantasy.AgentOption{fantasy.WithMaxRetries(5)}
	if k.SystemPrompt != "" {
		agentOpts = append(agentOpts, fantasy.WithSystemPrompt(k.SystemPrompt))
	}
	agent := fantasy.NewAgent(k.cheapLM(), agentOpts...)
	result, err := agent.Generate(ctx, fantasy.AgentCall{
		Prompt:   string(prompt),
		Messages: k.History,
	})
	if err != nil {
		return err
	}
	summary := result.Response.Content.Text()

	// Bill the compact call (previously invisible to runningCostUSD).
	var u Usage
	u.FromFantasyUsage(result.TotalUsage, k.cheapModelID())
	k.recordUsage(ctx, u)

	// Reset history. System stays out of History — Fantasy re-injects it via
	// WithSystemPrompt on the main agent.
	k.History = []fantasy.Message{
		fantasy.NewUserMessage("Tell me the summary of our conversation."),
	}
	msg := fantasy.NewUserMessage(
		"Here is a summary of our previous interaction for your reference:\n\n" + summary,
	)
	msg.Role = fantasy.MessageRoleAssistant
	k.History = append(k.History, msg)

	// Reset the occupancy gauge to reflect the new, tiny history. Without this it
	// keeps the stale pre-compaction value, which would immediately re-trigger the
	// in-loop threshold check (and misreport context usage until the next turn's
	// usage lands). ~4 chars/token is a coarse estimate of the summary's size; the
	// next stream replaces it with the real measured value.
	k.usageMu.Lock()
	k.currentTokens = (len(k.SystemPrompt) + len(summary)) / 4
	k.usageMu.Unlock()

	// Fire post-compact event so history can be reconstructed from events alone.
	// It carries the before/after diff (messages and tokens collapsed).
	_ = k.Fire(ctx, string(EventPostCompact), &CompactSummaryPayload{
		Summary:        summary,
		MessagesBefore: messagesBefore,
		MessagesAfter:  len(k.History),
		TokensBefore:   tokensBefore,
	})

	return nil
}

// RunSubagent runs a subagent synchronously and returns its output.
func (k *Kernel) RunSubagent(ctx context.Context, task string) (string, error) {
	// Inherit config from parent; pin the child to SmallerModel when set so
	// exploratory side work does not burn the primary model.
	subCfg := k.Cfg
	subCfg.SessionID = NewSessionID()
	subCfg.TraceID = k.Cfg.TraceID
	subCfg.ParentSpanID = k.Cfg.SessionID
	if k.Cfg.SmallerModel != "" {
		subCfg.Model = k.Cfg.SmallerModel
		// Force provider re-resolve for the cheaper model (may differ in prefix).
		subCfg.Provider = nil
	}

	// Create an independent Kernel instance for the subagent
	subKernel, err := NewKernel(ctx, subCfg)
	if err != nil {
		return "", fmt.Errorf("failed to initialize subagent: %w", err)
	}

	// Fire an event to let the system know a subagent is starting
	_ = k.Fire(ctx, string(EventSubagentStart), &SubagentPayload{
		SessionID: subKernel.Cfg.SessionID,
		Prompt:    task,
	})

	// Run the subagent on the task
	output, usage, err := subKernel.Run(ctx, task)

	// Fire stop event for the subagent
	_ = k.Fire(ctx, string(EventSubagentStop), &SubagentPayload{
		SessionID:    subKernel.Cfg.SessionID,
		Prompt:       task,
		Output:       output,
		UsagePayload: usage,
	})

	// Roll child spend into the parent total so RunningCostUSD is honest.
	// Per-session breakdown remains in usage.Tokens / SubagentStop payload.
	for sid, childU := range usage.Tokens {
		k.usageMu.Lock()
		k.Sessions[sid] = childU
		k.runningCostUSD += childU.Cost
		total := k.runningCostUSD
		k.usageMu.Unlock()
		if k.Store != nil {
			_ = k.Store.AppendCost(k.Cfg.TraceID, k.Cfg.SessionID, childU.Cost, total)
		}
	}

	if err != nil {
		return "", fmt.Errorf("subagent failed: %w", err)
	}

	return fmt.Sprintf("Subagent completed task. Output:\n%s", output), nil
}

// -- Hooks

type HookFn func(ctx context.Context, e Event) error

type HookRegistry struct {
	hooks []struct {
		kind EventKind
		fn   HookFn
	}
}

func (r *HookRegistry) On(kind EventKind, fn HookFn) {
	r.hooks = append(r.hooks, struct {
		kind EventKind
		fn   HookFn
	}{kind, fn})
}

// Fire runs all registered hooks for the event kind in order.
// A non-nil error from any hook aborts the chain and is returned.
func (r *HookRegistry) Fire(ctx context.Context, e Event) error {
	for _, h := range r.hooks {
		if h.kind == e.Kind {
			if err := h.fn(ctx, e); err != nil {
				return err
			}
		}
	}
	return nil
}

// Background agents (§12.4).
//
// A background agent is an asynchronous subagent: the parent fires it off, gets
// an id back immediately, and keeps working. When the child finishes, its result
// is Enqueue()d and — if the parent has since gone idle — the kernel is woken and
// re-enters its loop to process the result, the way a background-task completion
// notification wakes the agent in Claude Code. This reuses the existing message
// queue + step-boundary interrupt machinery; the only new primitive is Wake.

// wakeMu serializes wake attempts (see Wake). It is a package-free field on the
// Kernel via the embedded sync types declared in kernel.go's struct; we keep the
// extra mutex local to this file to avoid touching that struct further.

// SpawnBackground starts task as an asynchronous subagent and returns a short
// task id. The call returns immediately; the result is delivered later via the
// message queue, waking the kernel if it is idle.
func (k *Kernel) SpawnBackground(task string) string {
	id := fmt.Sprintf("bg-%d", k.bgSeq.Add(1))
	go func() {
		bctx := context.Background()
		out, err := k.RunSubagent(bctx, task)
		result := out
		status := "completed"
		if err != nil {
			result = fmt.Sprintf("background task %s failed: %v", id, err)
			status = "failed"
		}
		_ = k.Fire(bctx, string(EventTaskCompleted), &TaskPayload{TaskID: id, Title: task, Status: status})
		k.Enqueue(fmt.Sprintf("[background task %s %s]\n%s", id, status, result))
		// If a loop is already running it will drain the queue at the next step
		// boundary; otherwise wake the idle kernel to process the result.
		if !k.running.Load() {
			_ = k.Wake(bctx)
		}
	}()
	return id
}

// Wake re-enters the agent loop to process any queued messages when the kernel
// is idle. It is a no-op if a loop is already running (that loop will drain the
// queue itself) or if there is nothing queued. Output is streamed to the writer
// from the kernel's most recent Stream call, falling back to io.Discard.
func (k *Kernel) Wake(ctx context.Context) error {
	k.wakeMu.Lock()
	defer k.wakeMu.Unlock()

	if k.running.Load() {
		return nil // a live loop will drain the queue
	}
	msgs := k.drainQueue()
	if len(msgs) == 0 {
		return nil
	}
	// System prompt is Fantasy-owned; only inject the queued user messages.
	for _, m := range msgs {
		k.History = append(k.History, fantasy.NewUserMessage(m))
	}
	w := k.lastWriter
	if w == nil {
		w = io.Discard
	}
	return k.streamCurrent(ctx, w)
}

// SubagentAsyncArgs is the argument schema for the subagent_async tool.
type SubagentAsyncArgs struct {
	Task string `json:"task" jsonschema:"description=Full description of the subtask to run in the background"`
}

// newSubagentAsyncTool builds the subagent_async tool, which delegates a subtask
// to a background agent and returns immediately.
func newSubagentAsyncTool(k *Kernel, desc string) *tools.ToolDef {
	fTool := fantasy.NewAgentTool("subagent_async", desc, func(ctx context.Context, args SubagentAsyncArgs, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
		id := k.SpawnBackground(args.Task)
		return fantasy.ToolResponse{
			Type:    "text",
			Content: fmt.Sprintf("Started background task %s. You'll be notified when it completes — continue with other work in the meantime.", id),
		}, nil
	})
	return &tools.ToolDef{
		Name:        "subagent_async",
		Description: desc,
		Template:    "subagent.tool.tmpl",
		AgentTool:   fTool,
	}
}
