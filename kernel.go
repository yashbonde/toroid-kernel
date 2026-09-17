package toroid

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/yashbonde/toroid-kernel/llm"
	"github.com/yashbonde/toroid-kernel/tools"
)

// Thinking controls the model's thinking budget.
type Thinking string

const (
	ThinkingNone Thinking = "none" // disable thinking
	ThinkingLow  Thinking = "low"  // gateway reasoning_effort=low
	ThinkingHigh Thinking = "high" // gateway reasoning_effort=high
)

// Kernel is the agentic orchestrator. It owns the tool loop (turns), history,
// compaction, subagents, events, and persistence; each turn's LLM call is one
// llm-step performed by the Step layer against the LiteLLM gateway.
type Kernel struct {
	Cfg  Config
	Step Step // one-llm-step backend; defaults to GatewayStep over the llm client

	Hooks              *HookRegistry
	Tools              *tools.Registry
	Store              *Store
	seq                atomic.Uint64
	SystemPromptPrefix string
	SystemPrompt       string
	History            []llm.Message
	Sessions           map[string]Usage // sessionID -> summed tokens + cost (self + subagents)
	usageMu            sync.Mutex
	// reservedUSD is budget promised to subagents that are still running. Their
	// spend does not reach runningCostUSD until they finish, so without a
	// reservation two concurrent children each read the same headroom and the
	// transcript cap is exceeded by however many run at once.
	reservedUSD    float64
	currentTokens  int
	runningCostUSD float64
	mcpClients     []io.Closer // open connections to configured MCP servers, closed in Close()

	// message queue — callers enqueue messages that are injected at the next
	// safe interruption point (turn boundary), causing the loop to continue
	// with the queued messages appended to history.
	messageQueue []string
	queueMu      sync.Mutex

	// reqSeq counts outbound LLM requests so parent_session_id is attached to
	// the first one only (see nextRequestMetadata).
	reqSeq atomic.Uint64

	// newSubKernel builds the child kernel for a subagent run. It is a field so
	// tests can substitute a kernel with a scripted Step; nil means NewKernel.
	newSubKernel func(context.Context, Config) (*Kernel, error)

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
	// Model is the primary agent model (provider/model form). All models are
	// reached through the LiteLLM gateway ("llmgateway/<name>"); ids with other
	// prefixes are passed to the gateway verbatim for it to route.
	Model string `json:"model" description:"primary llm model name" default:"lfm-2.5-2.6b:free"`
	// SmallerModel is an optional cheaper model for cost-sensitive work:
	// conversation compaction and subagents (sync + async). Empty means those
	// paths use Model.
	SmallerModel string `json:"smaller_model,omitempty" description:"cheaper model for compaction and subagents; empty = use Model"`
	// APIKey is the gateway bearer token. Defaults to $LLM_GATEWAY_KEY.
	APIKey    string `json:"api_key,omitempty" description:"API key for the gateway"`
	SessionID string `json:"session_id,omitempty" description:"unique identifier for the session"`
	WorkDir   string `json:"work_dir" description:"working directory" default:"current directory"`
	// MaxIter caps tool-call steps per turn. With prompt caching every extra
	// step re-reads the prefix at cache price, so a deep loop is affordable;
	// the repeat-call guard still stops genuine spins early.
	MaxIter        int       `json:"max_iter" description:"max tool-call iterations" default:"100"`
	MaxTokens      int       `json:"max_tokens,omitempty" description:"max output tokens per llm-step; 0 = provider default"`
	MaxRepeatCalls int       `json:"max_repeat_calls" description:"stop after this many consecutive identical tool calls with identical results (loop guard); 0 disables" default:"3"`
	Thinking       Thinking  `json:"thinking" description:"thinking budget: none | low | high" default:"none"`
	ThinkingWriter io.Writer `json:"-"`

	// Tools
	IncludeComputerTools bool `json:"include_computer_tools" description:"if true, include the computer tools" default:"true"`
	// IncludeSubagentTools opts into synchronous and background delegation.
	// It is separate from the core file/shell tools so ordinary runs keep a
	// smaller, more relevant tool prefix.
	IncludeSubagentTools bool            `json:"include_subagent_tools,omitempty" description:"register subagent and subagent_async tools" default:"true"`
	Tools                *tools.Registry `json:"tools,omitempty" description:"custom tools"`

	// Skills: if unset or true, scan ~/.toroid/skills/*.md at startup for
	// skill metadata (name + description only) and register the skill tool
	// so the model can load a skill's full body on demand.
	LoadSkills *bool `json:"load_skills,omitempty" description:"scan ~/.toroid/skills/ for skill metadata at startup and register the skill tool" default:"true"`

	// MCP: remote Model Context Protocol servers to connect at startup. Each
	// server's tools are discovered via tools/list and registered into the
	// kernel's registry, name-prefixed per server to avoid collisions.
	MCPServers []tools.MCPServerConfig `json:"mcp_servers,omitempty" description:"remote MCP servers to connect and register tools from"`

	// MaxSubagentDepth bounds delegation nesting (root = 0, its subagent = 1).
	// 0 uses DefaultMaxSubagentDepth.
	MaxSubagentDepth int `json:"max_subagent_depth,omitempty" description:"maximum subagent nesting depth; 0 = default (3)" default:"3"`
	// Depth is this kernel's own position in that tree. Set by the kernel when
	// it spawns a subagent; callers leave it zero.
	Depth int `json:"depth,omitempty" description:"delegation depth of this kernel; set internally"`

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
	TotalContextSize     int `json:"total_context_size" description:"total context window size" default:"200000"`

	// MaxTranscriptSpendUSD caps cumulative LLM spend across this kernel's
	// transcript. A non-positive value disables the limit.
	MaxTranscriptSpendUSD float64 `json:"max_transcript_spend_usd,omitempty" description:"maximum cumulative transcript spend in USD; 0 disables"`

	// logging flags
	AttachLoggerHooks *bool `json:"attach_logger_hooks,omitempty" description:"automatically attach logger hooks" default:"false"`
	ShowHistory       *bool `json:"show_history" description:"print history" default:"false"`
}

// NewKernel creates and wires up a new Kernel.
func NewKernel(ctx context.Context, cfg Config) (*Kernel, error) {
	// Provider routing: "openai/<model>" talks straight to the OpenAI API,
	// "anthropic/<model>" to the native Anthropic messages API; everything else
	// goes through the LiteLLM gateway.
	baseURL := os.Getenv(GatewayBaseURLEnv)
	switch {
	case strings.HasPrefix(cfg.Model, "openai/"):
		baseURL = OpenAIBaseURL
		if cfg.APIKey == "" {
			cfg.APIKey = os.Getenv(OpenAIKeyEnv)
		}
	case strings.HasPrefix(cfg.Model, "anthropic/"):
		if cfg.APIKey == "" {
			cfg.APIKey = os.Getenv(AnthropicKeyEnv)
		}
	}
	if cfg.APIKey == "" {
		cfg.APIKey = os.Getenv(GatewayKeyEnv)
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

	// Ask the gateway what this model can actually take before falling back to
	// the compiled-in defaults: an id the static catalog has never seen would
	// otherwise inherit a 200k window and no output ceiling, and the provider
	// would truncate mid-answer. Best-effort — an unreachable or silent gateway
	// leaves the configured values exactly as they are.
	if lim, ok := fetchModelLimits(ctx, baseURL, cfg.APIKey, cfg.Model); ok {
		applyModelLimits(&cfg, lim)
	}

	// Apply context-window defaults. The struct-tag `default` values are NOT
	// auto-applied, so a caller that omits these leaves them at 0 — which makes
	// overContextThreshold treat threshold = TotalContextSize-CompactionBufferSize
	// = 0 and fire on the very first non-empty turn, spuriously compacting and
	// restarting the loop every turn.
	if cfg.TotalContextSize <= 0 {
		cfg.TotalContextSize = 200000
	}
	if cfg.CompactionBufferSize <= 0 {
		cfg.CompactionBufferSize = 50000
	}
	if cfg.MaxIter <= 0 {
		cfg.MaxIter = 100
	}
	if cfg.SmallerModel == "" {
		cfg.SmallerModel = "deepseek-v4-flash"
	}

	// Kernel object
	k := &Kernel{
		Cfg:           cfg,
		Hooks:         &HookRegistry{},
		Sessions:      map[string]Usage{},
		currentTokens: 0,
		Tools:         tools.NewRegistry(),
	}

	// register all tools
	if cfg.IncludeComputerTools {
		k.Tools.Register(tools.NewReadTool(k, toolDescription("read")))
		k.Tools.Register(tools.NewWriteTool(k, toolDescription("write")))
		k.Tools.Register(tools.NewBashTool(k, toolDescription("bash")))
		k.Tools.Register(tools.NewEditTool(k, toolDescription("edit")))
		k.Tools.Register(tools.NewMultiEditTool(k, toolDescription("multiedit")))
	}
	if cfg.IncludeSubagentTools {
		k.Tools.Register(tools.NewSubagentTool(k, toolDescription("subagent")))
		k.Tools.Register(newSubagentAsyncTool(k, toolDescription("subagent_async")))
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
		if len(skills) > 0 {
			k.Tools.Register(tools.NewSkillTool(k, toolDescription("skill")))
		}
	}

	// MCP: connect each configured remote server and register its tools.
	// Connections stay open for the kernel's lifetime and are closed in Close().
	for _, sc := range cfg.MCPServers {
		client, err := tools.ConnectMCPServer(ctx, k, k.Tools, sc)
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
			if msgs, err2 := ReconstructHistory(cfg.TraceID, cfg.SessionID, "", cfg.WorkDir, cfg.Model); err2 == nil && len(msgs) > 0 {
				k.History = msgs
				k.Logf("[resume] reconstructed %d history messages from events for trace %s", len(msgs), cfg.TraceID)
			} else if err2 != nil {
				k.LogErr("[resume] failed to reconstruct history: %v", err2)
			}
		}
	}

	// Compile one stable system prefix after every startup capability is known.
	k.SystemPromptPrefix, k.SystemPrompt = buildSystemPrompt(cfg.WorkDir, skills, len(cfg.MCPServers) > 0,
		cfg.IncludeSubagentTools, cfg.Model, cfg.SmallerModel, cfg.TotalContextSize, cfg.CompactionBufferSize)

	// Step is the one-llm-step backend over the in-repo wire client (OpenAI
	// chat completions, or native Anthropic messages for anthropic/ ids). Tests
	// may replace it with a FauxStep. The kernel owns the tool loop — Step
	// performs individual LLM calls.
	var wire llm.Chat = llm.NewClient(baseURL, cfg.APIKey)
	if strings.HasPrefix(cfg.Model, "anthropic/") {
		wire = llm.NewAnthropicClient(cfg.APIKey)
	}
	k.Step = NewGatewayStep(wire)

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

	// Live reasoning output for hosts that want it.
	if cfg.Thinking != ThinkingNone && cfg.ThinkingWriter != nil {
		k.On(EventReasoning, func(_ context.Context, e Event) error {
			if p, ok := e.Payload.(*ReasoningPayload); ok {
				_, err := fmt.Fprint(cfg.ThinkingWriter, p.Text)
				return err
			}
			return nil
		})
	}

	k.Cfg = cfg
	return k, nil
}

// fireTurnStarted emits EventTurnStarted just before the kernel issues the
// turn's llm-step: model, message count, tool names, schema name — what
// EventLLMStep used to carry as a separate event kind, folded in here because
// the two always fired together, at the same point, every time.
func (k *Kernel) fireTurnStarted(ctx context.Context, metadata llm.RequestMetadata, model string, c Context, schema string) {
	names := make([]string, 0, len(c.Tools))
	for _, t := range c.Tools {
		names = append(names, t.Name)
	}
	payload := TurnPayload{
		Model:    model,
		Messages: len(c.Messages),
		Tools:    names,
		Schema:   schema,
	}
	_ = k.FireInTurn(ctx, string(EventTurnStarted), metadata, &payload)
}

// fireTurnCompleted emits EventTurnCompleted with the turn's outcome: what
// EventTurnCompleted, EventAssistantTurn (content), and EventTurnCost (usage)
// used to report as three separate events at overlapping moments are now one.
// content is the turn's full structured content, already JSON-serialized by
// the caller (media-stripped for persistence).
func (k *Kernel) fireTurnCompleted(ctx context.Context, metadata llm.RequestMetadata, stopReason string, toolCalls int, content json.RawMessage, usage Usage, totalCostUSD float64) {
	payload := TurnPayload{
		StopReason:   stopReason,
		ToolCalls:    toolCalls,
		Content:      content,
		TurnUsage:    usage,
		TurnCostUSD:  usage.Cost,
		TotalCostUSD: totalCostUSD,
	}
	_ = k.FireInTurn(ctx, string(EventTurnCompleted), metadata, &payload)
}

func (k *Kernel) fireTurnFailed(ctx context.Context, metadata llm.RequestMetadata, err error) {
	payload := TurnPayload{StopReason: StopReasonError}
	if ctx.Err() != nil {
		payload.StopReason = StopReasonAborted
	}
	if err != nil {
		payload.Error = err.Error()
	}
	_ = k.FireInTurn(ctx, string(EventTurnFailed), metadata, &payload)
}

// fireGuard emits EventGuardTriggered — the single event kind covering every
// loop guard (previously silent, Logf-only, five separate inline blocks):
// the inspection, validation, and completion nudges, the repeat-call loop
// guard, and MaxIter.
func (k *Kernel) fireGuard(ctx context.Context, metadata llm.RequestMetadata, name, reason string) {
	_ = k.FireInTurn(ctx, string(EventGuardTriggered), metadata, &GuardTriggeredPayload{Name: name, Reason: reason})
}

// recordUsage adds a single LLM call's usage to session cost accounting and
// returns the running total, so the caller can fold it into TurnCompleted's
// payload instead of this firing its own event.
func (k *Kernel) recordUsage(u Usage) float64 {
	runningCost := k.UpdateUse(u, "")
	if k.Store != nil {
		_ = k.Store.AppendCost(k.Cfg.TraceID, k.Cfg.SessionID, u.Cost, runningCost)
	}
	return runningCost
}

// Implement tools.Agent interface
func (k *Kernel) WorkDir() string   { return k.Cfg.WorkDir }
func (k *Kernel) SessionID() string { return k.Cfg.SessionID }
func (k *Kernel) Model() string     { return k.Cfg.Model }

// TraceID is the transcript this kernel belongs to. A root kernel's equals its
// SessionID; a subagent inherits its parent's, so the whole delegation tree
// shares one trace.
func (k *Kernel) TraceID() string { return k.Cfg.TraceID }

// reserveBudget claims the transcript's remaining headroom for one subagent and
// reports how much it may spend. Checking and claiming happen under one lock, so
// concurrent children divide the cap instead of each seeing all of it. The first
// caller takes what is left; a second concurrent caller is refused rather than
// allowed to double the transcript's spend.
func (k *Kernel) reserveBudget(cap float64) (float64, bool) {
	k.usageMu.Lock()
	defer k.usageMu.Unlock()
	remaining := cap - k.runningCostUSD - k.reservedUSD
	if remaining <= 0 {
		return 0, false
	}
	k.reservedUSD += remaining
	return remaining, true
}

// releaseBudget returns an unspent reservation to the transcript. The child's
// actual spend has by then been added to runningCostUSD, so the headroom the
// next caller sees is correct.
func (k *Kernel) releaseBudget(amount float64) {
	k.usageMu.Lock()
	k.reservedUSD -= amount
	if k.reservedUSD < 0 {
		k.reservedUSD = 0
	}
	k.usageMu.Unlock()
}

// UsageSnapshot copies this kernel's per-session accounting. It is the reliable
// source of a run's spend: Run reports usage through EventStop, which a failed
// run never reaches, so a caller relying on that payload alone loses the cost of
// everything the run did before it failed.
func (k *Kernel) UsageSnapshot() map[string]Usage {
	k.usageMu.Lock()
	defer k.usageMu.Unlock()
	out := make(map[string]Usage, len(k.Sessions))
	for sid, u := range k.Sessions {
		out[sid] = u
	}
	return out
}

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

// Fire records an event scoped to the transcript only — used outside a turn
// (SessionStart, UserPromptSubmit, Stop, MasterIdle, SessionEnd, compaction,
// subagent start/stop). Use FireInTurn for anything happening inside a turn.
func (k *Kernel) Fire(ctx context.Context, kind string, payload any) error {
	return k.fireEvent(ctx, kind, "", "", "", payload)
}

// FireInTurn records an event carrying full chat/turn/llm-step scope on its
// envelope — used for everything fired inside a turn (TurnStarted,
// TurnCompleted, TurnFailed, Reasoning, PreToolUse, PostToolUse,
// PostToolUseFailure, GuardTriggered, QueueInterrupt), so a consumer can fold
// the whole event, not just Turn* payloads, back into the scope tree.
func (k *Kernel) FireInTurn(ctx context.Context, kind string, metadata llm.RequestMetadata, payload any) error {
	return k.fireEvent(ctx, kind, metadata.ChatID, metadata.TurnID, metadata.TraceID, payload)
}

func (k *Kernel) fireEvent(ctx context.Context, kind, chatID, turnID, llmStepID string, payload any) error {
	event := Event{
		Kind:         EventKind(kind),
		SessionID:    k.Cfg.SessionID,
		TraceID:      k.Cfg.TraceID,
		SpanID:       k.Cfg.SessionID,
		TranscriptID: k.Cfg.TraceID,
		ChatID:       chatID,
		TurnID:       turnID,
		LLMStepID:    llmStepID,
		EmitTS:       time.Now().UnixNano(),
		Seq:          k.seq.Add(1),
		Payload:      payload,
	}
	if k.Store != nil {
		_ = k.Store.AppendEvent(k.Cfg.TraceID, k.Cfg.SessionID, event)
	}
	return k.Hooks.Fire(ctx, event)
}

func (k *Kernel) UpdateUse(u Usage, key string) float64 {
	k.usageMu.Lock()
	defer k.usageMu.Unlock()
	id := key
	if id == "" {
		id = k.Cfg.SessionID
	}
	// Accumulate — a session's entry is the SUM of its llm-steps, not the last
	// one, so Stop/Run usage payloads and subagent rollups report real totals.
	s, seen := k.Sessions[id]
	s.Input += u.Input
	s.Output += u.Output
	s.Reasoning += u.Reasoning
	s.CacheRead += u.CacheRead
	s.CacheWrite += u.CacheWrite
	s.Cost += u.Cost
	if !seen {
		s.PricingOK = u.PricingOK
	} else {
		s.PricingOK = s.PricingOK && u.PricingOK
	}
	k.Sessions[id] = s
	k.runningCostUSD += u.Cost
	// u is a single step's usage: its input-side tokens (fresh + cache-read +
	// cache-write are disjoint slices of the prompt) are the size of the request
	// that step sent, plus what it generated — a valid snapshot of context-window
	// occupancy. Never feed a summed multi-step total (double-counts re-reads).
	k.currentTokens = int(u.Input + u.CacheRead + u.CacheWrite + u.Output)
	return k.runningCostUSD
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
// injected into the conversation at the next turn boundary, causing the loop
// to continue with the message appended to history.
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
		EventPreToolUse,
		EventPostToolUse,
		EventPostToolUseFailure,
		EventSubagentStart,
		EventSubagentStop,
		EventMasterIdle,
		EventReasoning,
		EventTurnStarted,
		EventTurnCompleted,
		EventTurnFailed,
		EventGuardTriggered,
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
	schema            Schema
	schemaName        string
	schemaDescription string
	maxTurnSpendUSD   float64
}

// WithSchema enables structured generation for this Run/Stream call.
// The model will produce a JSON object matching the given schema instead of
// free text. Run returns the raw JSON; Stream writes it to the writer.
func WithSchema(schema Schema, name, description string) RunOption {
	return func(o *runOptions) {
		o.schema = schema
		o.schemaName = name
		o.schemaDescription = description
	}
}

// WithMaxTurnSpendUSD caps LLM spend for one Run or Stream call. A
// non-positive value disables the limit.
func WithMaxTurnSpendUSD(usd float64) RunOption {
	return func(o *runOptions) { o.maxTurnSpendUSD = usd }
}

type spendBudget struct {
	turnStartUSD float64
	turnMaxUSD   float64
	hit          bool
}

func (b *spendBudget) reached(k *Kernel) bool {
	total := k.RunningCostUSD()
	b.hit = b.hit ||
		(b.turnMaxUSD > 0 && total-b.turnStartUSD >= b.turnMaxUSD) ||
		(k.Cfg.MaxTranscriptSpendUSD > 0 && total >= k.Cfg.MaxTranscriptSpendUSD)
	return b.hit
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
	ctx = k.withRequestChat(ctx)

	var ro runOptions
	for _, o := range opts {
		o(&ro)
	}

	// A loop may already be running — a background delegation completing wakes
	// one without any caller asking. Hand it the prompt through the queue rather
	// than racing it for History.
	if !k.claimLoop() {
		k.Enqueue(prompt)
		return nil
	}
	// Everything below mutates History. Release the claim on any path that does
	// not reach streamCurrent, which owns the flag from there on.
	entered := false
	defer func() {
		if !entered {
			k.running.Store(false)
		}
	}()

	budget := &spendBudget{
		turnStartUSD: k.RunningCostUSD(),
		turnMaxUSD:   ro.maxTurnSpendUSD,
	}
	// Fire session start only once. System prompt is sent as the single leading
	// system message by the Step layer — do not also put it in History (double
	// bill + cache bust).
	if len(k.History) == 0 {
		_ = k.Fire(ctx, string(EventSessionStart), nil)
	}

	// Auto-compact if approaching context limit; important to do this before adding user prompt
	if !budget.reached(k) && k.overContextThreshold() {
		k.Logf("auto-compacting: currentTokens=%d threshold=%d", k.currentTokens, k.Cfg.TotalContextSize-k.Cfg.CompactionBufferSize)
		if err := k.Compact(ctx); err != nil {
			return err
		}
	}

	// append user message; important to do this before history validation.
	// parseUserMessage inlines any markdown image refs as file parts and returns
	// the portable (~-rooted) prompt to persist for cross-process resume.
	userMsg, storedPrompt, mediaWarnings := parseUserMessage(prompt, k.Cfg.WorkDir, ResolveModel(k.Cfg.Model))
	k.History = append(k.History, userMsg)
	_ = k.Fire(ctx, string(EventUserPromptSubmit), &UserPromptPayload{Prompt: storedPrompt})
	// Surface any media that could not be inlined (unsupported model, oversized)
	// so a dropped image is visible rather than silently missing (M8).
	for _, w := range mediaWarnings {
		k.LogErr("multimodal: %s", w)
	}

	// history validation — last message must be the user turn we just appended.
	if len(k.History) == 0 || k.History[len(k.History)-1].Role != llm.RoleUser {
		role := llm.Role("")
		if len(k.History) > 0 {
			role = k.History[len(k.History)-1].Role
		}
		k.LogErr("Last history item must be a user message. Got: '%s'", role)
		panic("Last item is not a user message.")
	}

	// When schema is set, discard the free-text output from the agent loop;
	// only the structured JSON from the object llm-step goes to the caller's writer.
	loopWriter := w
	if ro.schema != nil {
		loopWriter = io.Discard
	}
	// streamCurrent owns the running flag from here: it clears it on every exit.
	entered = true
	if err := k.streamCurrent(ctx, loopWriter, budget); err != nil {
		return err
	}

	if ro.schema != nil {
		if budget.reached(k) {
			return nil
		}
		// The structured-output pass is one more llm-step in the SAME chat (M7).
		// After the agentic loop the last message is assistant, so append a user
		// turn asking the model to emit structured output. Run it through the Step
		// layer so its usage is priced and billed like any other llm-step.
		schemaMsgs := append(k.History, llm.NewUserMessage("Now return your findings in the required JSON format."))
		res, err := k.Step.CompleteObject(ctx, ResolveModel(k.Cfg.Model), Context{
			SystemPrefix: k.SystemPromptPrefix,
			System:       k.SystemPrompt,
			Messages:     schemaMsgs,
			Metadata:     k.nextRequestMetadata(ctx),
		}, ro.schema, ro.schemaName, ro.schemaDescription, k.stepOpts())
		if err != nil {
			return err
		}
		// Bill the object llm-step and roll it into transcript totals (M7).
		k.recordUsage(res.Usage)
		budget.reached(k)
		b, err := json.Marshal(res.Object)
		if err != nil {
			return err
		}
		_, err = w.Write(b)
		return err
	}

	return nil
}

// streamCurrent runs the agent loop over the current in-memory history until it
// stops (no pending queued messages). Stream prepares history (compaction,
// appending the user message, pruning) and then calls this; Wake calls it after
// injecting a background completion. runMu serializes the loop so the two never
// race on history.
func (k *Kernel) streamCurrent(ctx context.Context, w io.Writer, budget *spendBudget) error {
	k.runMu.Lock()
	k.running.Store(true)
	k.lastWriter = w

	if err := k.streamViaStep(ctx, w, budget); err != nil {
		k.running.Store(false)
		k.runMu.Unlock()
		return err
	}

	if k.Cfg.ShowHistory != nil && *k.Cfg.ShowHistory {
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

	// Mark not-running and release runMu *before* asking tryResume whether to
	// resume: tryResume may itself re-enter streamCurrent (a background
	// goroutine's result arrived while we were finishing this turn), and that
	// recursive call needs to acquire runMu fresh, not find it still held by
	// this call's own now-finished execution.
	k.running.Store(false)
	k.runMu.Unlock()
	return k.tryResume(ctx)
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

// stepOpts returns StepOptions derived from kernel config. When cfg.MaxTokens
// is > 0 it sets the per-step output-token cap; zero leaves it to the provider.
func (k *Kernel) stepOpts() StepOptions {
	opts := StepOptions{Thinking: k.Cfg.Thinking}
	if k.Cfg.MaxTokens > 0 {
		mt := int64(k.Cfg.MaxTokens)
		opts.MaxOutputTokens = &mt
	}
	return opts
}

// trimForCompact shrinks history in place just before the compaction summarize
// call: tool-call args are cleared, tool results truncated, and media/reasoning
// parts stripped — the summary needs none of them, and un-stripped images would
// make the one call meant to REDUCE cost re-pay for every blob in the
// transcript. History is never mutated mid-chat outside compaction: rewriting
// an already-sent message changes the byte prefix and busts the prompt cache.
func (k *Kernel) trimForCompact(maxResultChars int) {
	for j := range k.History {
		msg := &k.History[j]
		kept := msg.Parts[:0]
		for _, part := range msg.Parts {
			switch p := part.(type) {
			case llm.ReasoningPart:
				continue // never needed for the summary
			case llm.ToolCallPart:
				p.Arguments = "{}"
				kept = append(kept, p)
			case llm.ToolResultPart:
				if len(p.Content) > maxResultChars {
					p.Content = p.Content[:maxResultChars] + "… [trimmed]"
				}
				if len(p.Files) > 0 {
					p.Files = nil
					p.Content += " [media omitted]"
				}
				kept = append(kept, p)
			case llm.FilePart:
				kept = append(kept, llm.TextPart{Text: "[media omitted]"})
			default:
				kept = append(kept, part)
			}
		}
		msg.Parts = kept
	}
}

// Compact summarizes the current history and resets it.
func (k *Kernel) Compact(ctx context.Context) error {
	if len(k.History) == 0 {
		return nil
	}

	// Shrink fat tool results (and strip media/reasoning) before paying for a
	// full-history summarize call.
	k.trimForCompact(2000)

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

	// Summarization is one llm-step on the cost-sensitive model, billed from the
	// model catalog like any other. Cross-model
	// handoff (M9): when the compact model differs from the main chat model,
	// adapt history for the target's wire API — a no-op while all in-scope
	// models share openai-completions.
	compactModelID := k.Cfg.Model
	if k.Cfg.SmallerModel != "" {
		compactModelID = k.Cfg.SmallerModel
	}
	compactModel := ResolveModel(compactModelID)
	histForCompact := TransformForHandoff(k.History, ResolveModel(k.Cfg.Model), compactModel)
	compactCtx := Context{
		SystemPrefix: k.SystemPromptPrefix,
		System:       k.SystemPrompt,
		Messages:     append(histForCompact, llm.NewUserMessage(string(prompt))),
		Metadata:     k.nextRequestMetadata(ctx),
	}
	opts := k.stepOpts()
	opts.DisablePromptCache = true
	res, err := k.Step.Complete(ctx, compactModel, compactCtx, opts)
	if err != nil {
		return err
	}
	summary := res.Text()

	// Bill the compact llm-step. PreCompact/PostCompact already fully describe
	// this call (message/token counts, the summary) — no separate debug-view
	// event needed for a one-off request outside the turn loop.
	k.recordUsage(res.Usage)

	// Reset history. System stays out of History — the Step layer re-injects it
	// as the single leading system message on every call.
	k.History = []llm.Message{
		llm.NewUserMessage("Tell me the summary of our conversation."),
	}
	msg := llm.NewUserMessage(
		"Here is a summary of our previous interaction for your reference:\n\n" + summary,
	)
	msg.Role = llm.RoleAssistant
	k.History = append(k.History, msg)

	// Reset the occupancy gauge to reflect the new, tiny history. Without this it
	// keeps the stale pre-compaction value, which would immediately re-trigger the
	// in-loop threshold check (and misreport context usage until the next turn's
	// usage lands). ~4 chars/token is a coarse estimate of the summary's size; the
	// next stream replaces it with the real measured value.
	k.usageMu.Lock()
	k.currentTokens = (len(k.SystemPromptPrefix) + len(k.SystemPrompt) + len(summary)) / 4
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

// DefaultMaxSubagentDepth bounds how deep delegation may nest. The root kernel
// is depth 0, a subagent it spawns is depth 1, and so on. Without a ceiling a
// prompt that keeps saying "delegate this" can fan out an unbounded tree of
// kernels, each with its own budget.
const DefaultMaxSubagentDepth = 3

// SubagentOpts tunes one delegated run. The zero value is the default subagent:
// synchronous, on the kernel's own default model.
type SubagentOpts struct {
	// Model overrides the model for this subagent only, in the same
	// provider/model form as Config.Model. Empty selects the default
	// (SmallerModel when set, else the parent's Model). A model that fails to
	// run falls back to that default automatically.
	Model string
	// TaskID marks the run as asynchronous and labels it in subagent events.
	TaskID string
}

// SubagentResult is everything one subagent run produced. Callers use this
// instead of reading the parent's Sessions map: that map accumulates every
// child the session ever ran, so scraping it attributes earlier children's
// spend to the latest run.
type SubagentResult struct {
	SessionID string `json:"session_id"`
	// Model is what actually answered — not necessarily RequestedModel, if an
	// override failed and the run fell back.
	Model          string `json:"model"`
	RequestedModel string `json:"requested_model,omitempty"`
	FellBack       bool   `json:"fell_back,omitempty"`
	Output         string `json:"output,omitempty"`
	// Usage is this run's spend alone, summed across the child and any
	// descendants it spawned. Session entries are disjoint, so the sum is exact.
	Usage     Usage `json:"usage"`
	Steps     int   `json:"steps"`
	ToolCalls int   `json:"tool_calls"`
}

// RunSubagent runs a subagent synchronously and returns its output. It satisfies
// tools.Agent; callers wanting a model override or the run's accounting should
// use RunSubagentWith.
func (k *Kernel) RunSubagent(ctx context.Context, task string) (string, error) {
	res, err := k.runSubagent(ctx, task, SubagentOpts{})
	return res.Output, err
}

// RunSubagentModel runs a synchronous subagent on a specific model, falling back
// to the default when that model cannot run. It satisfies tools.SubagentRunner,
// which is how the subagent tool exposes its optional model argument.
func (k *Kernel) RunSubagentModel(ctx context.Context, task, model string) (string, error) {
	res, err := k.runSubagent(ctx, task, SubagentOpts{Model: model})
	return res.Output, err
}

// RunSubagentWith runs a synchronous subagent and returns its full result.
func (k *Kernel) RunSubagentWith(ctx context.Context, task string, opts SubagentOpts) (SubagentResult, error) {
	return k.runSubagent(ctx, task, opts)
}

// modelProvider is the provider prefix of a model id ("openai" in
// "openai/gpt-5"), or "" when the id carries none.
func modelProvider(model string) string {
	if i := strings.Index(model, "/"); i > 0 {
		return model[:i]
	}
	return ""
}

// maxSubagentDepth is the configured ceiling, or the default when unset.
func (k *Kernel) maxSubagentDepth() int {
	if k.Cfg.MaxSubagentDepth > 0 {
		return k.Cfg.MaxSubagentDepth
	}
	return DefaultMaxSubagentDepth
}

// defaultSubagentModel is the model a subagent runs on when the caller names
// none: the cheaper model when configured, else the parent's own.
func (k *Kernel) defaultSubagentModel() string {
	if k.Cfg.SmallerModel != "" {
		return k.Cfg.SmallerModel
	}
	return k.Cfg.Model
}

// runSubagent is shared by the synchronous (RunSubagentWith) and asynchronous
// (SpawnBackground) subagent paths. opts.TaskID is empty on the synchronous
// path; non-empty on the async path, where it also sets SubagentPayload.Async —
// what EventTaskCompleted used to report as a separate event kind, now just
// a field on the same EventSubagentStop every subagent already fires.
func (k *Kernel) runSubagent(ctx context.Context, task string, opts SubagentOpts) (SubagentResult, error) {
	depth := k.Cfg.Depth + 1
	if maxDepth := k.maxSubagentDepth(); depth > maxDepth {
		return SubagentResult{}, fmt.Errorf("subagent depth limit reached (%d): do this work directly instead of delegating again", maxDepth)
	}

	fallback := k.defaultSubagentModel()
	requested := strings.TrimSpace(opts.Model)
	model := fallback
	if requested != "" {
		model = requested
	}

	res, err := k.runSubagentOn(ctx, task, model, depth, opts.TaskID)
	// A model the caller named may not exist, may be unreachable, or may reject
	// the request. That is a routing mistake, not a reason to lose the task, so
	// retry once on the default.
	//
	// The retry is deliberately narrow: only when the requested model never
	// completed a single turn. A child that answered and then failed later (loop
	// guard, spend cap, a genuinely impossible task) would fail the same way on
	// any model, and re-running it would just bill the work twice. Cancellation
	// is the caller's decision and is never retried.
	if err != nil && requested != "" && model != fallback && ctx.Err() == nil && res.Steps == 0 {
		res, err = k.runSubagentOn(ctx, task, fallback, depth, opts.TaskID)
		res.FellBack = true
	}
	res.RequestedModel = requested
	return res, err
}

// runSubagentOn performs one subagent attempt on an exact model.
func (k *Kernel) runSubagentOn(ctx context.Context, task, model string, depth int, taskID string) (SubagentResult, error) {
	// Inherit config from parent, then pin this child's identity and model.
	subCfg := k.Cfg
	subCfg.SessionID = NewSessionID()
	subCfg.TraceID = k.Cfg.TraceID
	subCfg.ParentSpanID = k.Cfg.SessionID
	subCfg.Depth = depth
	subCfg.Model = model
	// Credentials are provider-specific and Cfg.APIKey is already resolved for
	// the parent's provider. Carrying it across a provider switch sends the
	// gateway's key to (say) OpenAI, so the override fails and silently falls
	// back even when the right key is in the environment. Clearing it makes
	// NewKernel resolve the key for the provider actually being used.
	if modelProvider(model) != modelProvider(k.Cfg.Model) {
		subCfg.APIKey = ""
	}
	// A child inherits the parent's SmallerModel only as a default for its own
	// children; its own model is already resolved above and must not be
	// re-overridden when it in turn spawns work.
	if subCfg.MaxTranscriptSpendUSD > 0 {
		remaining, ok := k.reserveBudget(subCfg.MaxTranscriptSpendUSD)
		if !ok {
			// Report this rather than returning an empty success, which a caller
			// records as a task that ran and produced nothing.
			return SubagentResult{Model: model}, fmt.Errorf("subagent not started: transcript spend limit of $%.4f already committed", k.Cfg.MaxTranscriptSpendUSD)
		}
		// Released once the child's real spend has been rolled into the parent.
		defer k.releaseBudget(remaining)
		subCfg.MaxTranscriptSpendUSD = remaining
	}
	// A child may delegate further only while it stays under the ceiling; at the
	// last allowed level its subagent tools are withheld, so the limit binds
	// before a run is wasted discovering it.
	subCfg.IncludeSubagentTools = k.Cfg.IncludeSubagentTools && depth < k.maxSubagentDepth()

	res := SubagentResult{SessionID: subCfg.SessionID, Model: model}

	// Create an independent Kernel instance for the subagent
	build := k.newSubKernel
	if build == nil {
		build = NewKernel
	}
	subKernel, err := build(ctx, subCfg)
	if err != nil {
		return res, fmt.Errorf("failed to initialize subagent: %w", err)
	}

	// Count the child's own turns and tool calls. The usage payload carries
	// tokens but not shape, and "how many steps and tools did this cost" is the
	// question a delegation record exists to answer.
	var steps, toolCalls int
	subKernel.On(EventTurnCompleted, func(_ context.Context, e Event) error {
		if p, ok := e.Payload.(*TurnPayload); ok {
			steps++
			toolCalls += p.ToolCalls
		}
		return nil
	})

	async := taskID != ""

	// Fire an event to let the system know a subagent is starting
	_ = k.Fire(ctx, string(EventSubagentStart), &SubagentPayload{
		SessionID: subKernel.Cfg.SessionID,
		Prompt:    task,
		Async:     async,
		TaskID:    taskID,
		Model:     model,
	})

	// Run the subagent on the task
	output, _, runErr := subKernel.Run(ctx, task)
	res.Steps, res.ToolCalls = steps, toolCalls

	// Take the child's accounting from the child itself rather than from Run's
	// usage payload: that payload is populated by EventStop, which a run that
	// failed part-way never fires, silently dropping everything it had already
	// spent.
	childUsage := subKernel.UsageSnapshot()

	// Roll child spend into the parent total so RunningCostUSD is honest, and
	// sum it into the result so the caller does not have to read k.Sessions —
	// entries are per-session and disjoint, so this sum is the run's true cost.
	first := true
	for sid, childU := range childUsage {
		k.usageMu.Lock()
		k.Sessions[sid] = childU
		k.runningCostUSD += childU.Cost
		total := k.runningCostUSD
		k.usageMu.Unlock()
		res.Usage.Input += childU.Input
		res.Usage.Output += childU.Output
		res.Usage.Reasoning += childU.Reasoning
		res.Usage.CacheRead += childU.CacheRead
		res.Usage.CacheWrite += childU.CacheWrite
		res.Usage.Cost += childU.Cost
		// Unknown pricing anywhere makes the whole rollup's cost a floor, not a
		// total — never report it as if it were priced.
		if first {
			res.Usage.PricingOK, first = childU.PricingOK, false
		} else {
			res.Usage.PricingOK = res.Usage.PricingOK && childU.PricingOK
		}
		if k.Store != nil {
			_ = k.Store.AppendCost(k.Cfg.TraceID, k.Cfg.SessionID, childU.Cost, total)
		}
	}

	// Fire stop event for the subagent
	_ = k.Fire(ctx, string(EventSubagentStop), &SubagentPayload{
		SessionID:    subKernel.Cfg.SessionID,
		Prompt:       task,
		Output:       output,
		UsagePayload: UsagePayload{Tokens: childUsage},
		Async:        async,
		TaskID:       taskID,
		Model:        model,
	})

	if runErr != nil {
		return res, fmt.Errorf("subagent failed: %w", runErr)
	}

	res.Output = fmt.Sprintf("Subagent completed task. Output:\n%s", output)
	return res, nil
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
// queue + turn-boundary interrupt machinery; the only new primitive is Wake.

// SpawnBackground starts task as an asynchronous subagent and returns a short
// task id. The call returns immediately; the result is delivered later via the
// message queue, waking the kernel if it is idle.
func (k *Kernel) SpawnBackground(task string) string {
	return k.SpawnBackgroundWith(task, SubagentOpts{}, nil)
}

// SpawnBackgroundWith is SpawnBackground with a model override and an optional
// completion callback. done runs on the background goroutine once the child has
// finished, before the parent is woken, and receives the run's own accounting —
// which is what a caller recording the task needs and cannot get from the
// parent's cumulative Sessions map.
func (k *Kernel) SpawnBackgroundWith(task string, opts SubagentOpts, done func(SubagentResult, error)) string {
	// A caller that already has an id for this task (a delegation record, say)
	// passes it in so one id labels the record, the events, and the completion
	// message; otherwise the kernel numbers it.
	id := strings.TrimSpace(opts.TaskID)
	if id == "" {
		id = fmt.Sprintf("bg-%d", k.bgSeq.Add(1))
	}
	opts.TaskID = id
	go func() {
		bctx := context.Background()
		res, err := k.runSubagent(bctx, task, opts)
		if done != nil {
			done(res, err)
		}
		result := res.Output
		status := "completed"
		if err != nil {
			result = fmt.Sprintf("background task %s failed: %v", id, err)
			status = "failed"
		}
		k.Enqueue(fmt.Sprintf("[background task %s %s]\n%s", id, status, result))
		// tryResume is the single decision point: if a loop is already running
		// it will drain the queue itself at its next turn boundary and this is
		// a no-op; otherwise it drains and resumes right here.
		_ = k.tryResume(bctx)
	}()
	return id
}

// claimLoop reports whether this caller now owns the agent loop. It claims under
// wakeMu — the same lock tryResume uses — so a caller preparing history and a
// finishing background task can never both decide the loop is theirs.
//
// Without this, Stream's prologue (compaction, appending the user message) runs
// unsynchronized against a background completion's append in tryResume: two
// goroutines appending to the same History slice, which loses turns or corrupts
// the slice outright. A caller that does not win the claim queues its prompt
// instead, and the live loop picks it up at its next turn boundary.
func (k *Kernel) claimLoop() bool {
	k.wakeMu.Lock()
	defer k.wakeMu.Unlock()
	if k.running.Load() {
		return false
	}
	k.running.Store(true)
	return true
}

// tryResume is the single answer to "should the kernel resume?" — called both
// at the end of a turn (streamCurrent, once its own run has ended) and by a
// finishing background goroutine (SpawnBackground). It replaces two
// independent, unlocked checks — running.Load() in SpawnBackground and a
// queue-length read in streamCurrent — with one function under one lock, so
// there is no longer a gap between them for a message to land in unseen.
//
// If the kernel is idle and the queue is empty, it fires MasterIdle. If the
// kernel is idle and something is queued, it drains and resumes the loop
// itself, streaming to the writer from the kernel's most recent Stream call
// (falling back to io.Discard). If the kernel is still running, it is a
// no-op: that live loop will drain the queue itself at its next turn
// boundary (QueueInterrupt).
func (k *Kernel) tryResume(ctx context.Context) error {
	k.wakeMu.Lock()

	if k.running.Load() {
		k.wakeMu.Unlock()
		return nil // a live loop will drain the queue itself
	}
	msgs := k.drainQueue()
	if len(msgs) == 0 {
		k.wakeMu.Unlock()
		_ = k.Fire(ctx, string(EventMasterIdle), nil)
		return nil
	}
	// Mark running while still holding the lock so a concurrent tryResume (e.g. a
	// second finishing background goroutine) sees the loop as active and neither
	// double-resumes nor fires a spurious MasterIdle.
	k.running.Store(true)
	w := k.lastWriter
	if w == nil {
		w = io.Discard
	}
	// Release the lock before resuming: streamCurrent calls tryResume again at its
	// own tail (mutual recursion), which must not deadlock on this non-reentrant
	// mutex. The atomic running/queue decision above is already made.
	k.wakeMu.Unlock()

	ctx = WithGatewayTrace(ctx)
	ctx = k.withRequestChat(ctx)
	// System prompt is Step-owned; only inject the queued user messages.
	for _, m := range msgs {
		k.History = append(k.History, llm.NewUserMessage(m))
	}
	return k.streamCurrent(ctx, w, &spendBudget{turnStartUSD: k.RunningCostUSD()})
}

// SubagentAsyncArgs is the argument schema for the subagent_async tool.
type SubagentAsyncArgs struct {
	Task  string `json:"task" jsonschema:"description=Independent background subtask with goal paths constraints and expected result,minLength=1"`
	Model string `json:"model,omitempty" jsonschema:"description=Model id to run this subtask on. Omit unless the user named a specific model."`
}

// newSubagentAsyncTool builds the subagent_async tool, which delegates a subtask
// to a background agent and returns immediately.
func newSubagentAsyncTool(k *Kernel, desc string) *tools.ToolDef {
	h := llm.NewTool("subagent_async", desc, func(ctx context.Context, args SubagentAsyncArgs) (llm.ToolResult, error) {
		id := k.SpawnBackgroundWith(args.Task, SubagentOpts{Model: args.Model}, nil)
		return llm.NewTextResult(fmt.Sprintf("Started background task %s. You'll be notified when it completes — continue with other work in the meantime.", id)), nil
	})
	return &tools.ToolDef{
		Name:        "subagent_async",
		Description: desc,
		Handler:     h,
	}
}
