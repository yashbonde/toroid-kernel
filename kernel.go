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
	Hooks            *HookRegistry
	Tools            *tools.Registry
	Store            *Store
	seq              atomic.Uint64
	SystemPrompt     string
	Title            string
	History          []fantasy.Message
	StepUsage        []Usage          // per-step token usage, index-aligned with StepHistoryStart
	StepHistoryStart []int            // history index where each step's messages begin
	Sessions         map[string]Usage // sessionID -> total tokens used (self + subagents)
	usageMu          sync.Mutex
	FantasyAgentOpts []fantasy.AgentOption
	currentTokens    int
	runningCostUSD   float64
	todoDB           *sql.DB

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
	Provider       fantasy.Provider `json:"provider,omitempty" description:"llm provider"`
	Model          string           `json:"model" description:"llm model name" default:"anthropic/claude-haiku-4-5"`
	APIKey         string           `json:"api_key,omitempty" description:"API key for the provider"`
	SessionID      string           `json:"session_id,omitempty" description:"unique identifier for the session"`
	WorkDir        string           `json:"work_dir" description:"working directory" default:"current directory"`
	MaxIter        int              `json:"max_iter" description:"max tool-call iterations" default:"50"`
	MaxRepeatCalls int              `json:"max_repeat_calls" description:"stop after this many consecutive identical tool calls with identical results (loop guard); 0 disables" default:"3"`
	Thinking       Thinking         `json:"thinking" description:"thinking budget: none | low | high" default:"none"`
	ThinkingWriter io.Writer        `json:"-"`

	// Tools
	ComputerTools bool            `json:"computer_tools" description:"if true, include the computer tools" default:"true"`
	Tools         *tools.Registry `json:"tools,omitempty" description:"custom tools"`

	// Trace/span hierarchy
	TraceID         string `json:"trace_id,omitempty"`          // inherited from parent; root sets TraceID = SessionID
	ParentSpanID    string `json:"parent_span_id,omitempty"`    // parent kernel's SessionID
	PreviousTraceID string `json:"previous_trace_id,omitempty"` // previous trace id when compaction is triggered

	// Persistence
	Save bool `json:"save" description:"persist events, costs and metadata to the SQLite store" default:"false"`

	// Session management
	Resume        bool `json:"resume" description:"if true, load existing session history and continue" default:"false"`
	GenerateTitle bool `json:"generate_title" description:"if true, generate title for the session" default:"false"`

	// compaction
	CompactionBufferSize int `json:"compaction_buffer_size" description:"buffer size for history compaction" default:"30000"`
	ToolCallPrunedSize   int `json:"tool_call_prune" description:"token limit for tool call after pruning" default:"40000"`
	TotalContextSize     int `json:"total_context_size" description:"total context window size" default:"300000"`

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

	// register all tools
	if cfg.ComputerTools {
		getDescription := func(name string) string {
			b, _ := readPrompt(name + ".tool.tmpl")
			lines := strings.Split(string(b), "\n")
			if len(lines) > 1 {
				return lines[1]
			}
			return "Tool " + name
		}
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
			// Load existing title so we can update it
			meta, _ := store.LoadTraceMeta(cfg.TraceID)
			k.Title = meta.Title
			// Restore conversation history by replaying stored events (post-last-compaction only).
			if msgs, err2 := ReconstructHistory(cfg.TraceID, cfg.SessionID, "", cfg.WorkDir); err2 == nil && len(msgs) > 0 {
				k.History = msgs
				k.Logf("[resume] reconstructed %d history messages from events for trace %s", len(msgs), cfg.TraceID)
			} else if err2 != nil {
				k.LogErr("[resume] failed to reconstruct history: %v", err2)
			}
		}
	}

	// Load system prompt
	systemPrompt, err := buildSystemPrompt(cfg.WorkDir)
	if err != nil {
		return nil, err
	}
	k.SystemPrompt = systemPrompt

	// Load the model
	if cfg.Provider == nil {
		p, err := NewProviderFromLLMId(cfg.Model, cfg.APIKey)
		if err != nil {
			return nil, fmt.Errorf("failed to initialize provider for %q: %w", cfg.Model, err)
		}
		cfg.Provider = p
	}
	// Strip the "provider/" prefix before passing to LanguageModel
	modelName := cfg.Model
	if _, after, ok := strings.Cut(cfg.Model, "/"); ok {
		modelName = after
	}
	model, err := cfg.Provider.LanguageModel(ctx, modelName)
	if err != nil {
		return nil, err
	}
	k.LM = model

	// Build Fantasy Tools
	var fTools []fantasy.AgentTool
	for _, t := range k.Tools.Tools() {
		fTools = append(fTools, t.AgentTool)
	}

	// Default AttachLoggerHooks if nil
	if cfg.AttachLoggerHooks != nil && *cfg.AttachLoggerHooks {
		k.OnAll(func(ctx context.Context, e Event) error {
			if e.Kind == EventToken || e.Kind == EventReasoning {
				return nil
			}
			k.Logf(string(e.Kind) + " " + fmt.Sprintf("%v", e.Payload))
			return nil
		})
	}

	// Initialize Fantasy Agent
	opts := []fantasy.AgentOption{
		fantasy.WithSystemPrompt(systemPrompt),
		fantasy.WithTools(fTools...),
		fantasy.WithMaxRetries(5),
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

	return k, nil
}

func buildSystemPrompt(workDir string) (string, error) {
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
		"WorkDir": workDir,
		"Date":    time.Now().Format("2006-01-02 15:04:05"),
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
	if k.Store != nil && event.Kind != EventToken && event.Kind != EventReasoning {
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
	k.currentTokens = int(u.Input + u.Output + u.CacheRead + u.CacheWrite)
	return k.runningCostUSD
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
		EventToken,
		EventPermissionRequest,
		EventPreToolUse,
		EventPostToolUse,
		EventPostToolUseFailure,
		EventSubagentStart,
		EventSubagentStop,
		EventMasterIdle,
		EventNotification,
		EventTaskCompleted,
		EventTitle,
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
func (k *Kernel) Run(ctx context.Context, prompt string) (string, UsagePayload, error) {
	var buf strings.Builder
	var usage UsagePayload
	k.On(EventStop, func(ctx context.Context, e Event) error {
		usage = *e.Payload.(*UsagePayload)
		return nil
	})
	err := k.Stream(ctx, prompt, &buf)
	return buf.String(), usage, err
}

// Stream runs the agent loop and streams the response to the writer.
func (k *Kernel) Stream(ctx context.Context, prompt string, w io.Writer) error {
	// Fire session start only once
	if len(k.History) == 0 {
		_ = k.Fire(ctx, string(EventSessionStart), nil)
		if k.SystemPrompt != "" {
			k.History = append(k.History, fantasy.NewSystemMessage(k.SystemPrompt))
		}
	}

	// Auto-compact if approaching context limit; important to do this before adding user prompt
	if k.currentTokens > 0 && k.currentTokens >= k.Cfg.TotalContextSize-k.Cfg.CompactionBufferSize {
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

	// history validation
	if len(k.History) > 0 {
		if k.SystemPrompt != "" && k.History[0].Role != fantasy.MessageRoleSystem {
			k.LogErr("Kernel provided with system prompt. SysPrompt should be first item in history, found: '%s'", k.History[len(k.History)-1].Role)
			panic("Kernel provided with system prompt. SysPrompt should be first item in history")
		}
		if k.History[len(k.History)-1].Role != fantasy.MessageRoleUser {
			k.LogErr("Last item (%d) is 'user' message. Got: '%s'", len(k.History)-1, k.History[len(k.History)-1].Role)
			panic("Last item is 'user' message.")
		}

		// Shoot out a coroutine to generate title, last message is always user so this
		// works fine. Why not use a subagent?
		// Because subagents are meant for complex tasks that requires the full prompt
		// and intelligence. We can get away with a very small prompt and thus less cost
		if k.Cfg.GenerateTitle && k.Title == "" {
			go func() {
				ctx := context.Background()
				agent := fantasy.NewAgent(k.LM, k.FantasyAgentOpts...)
				titlePrompt, err := readPrompt("title.kernel.tmpl")
				if err != nil {
					k.LogErr("Failed to read title prompt: %v", err)
					return
				}
				// feed the last message and
				resp, err := agent.Generate(ctx, fantasy.AgentCall{
					Prompt:   string(titlePrompt),
					Messages: k.History[len(k.History)-1:],
				})
				if err != nil {
					k.LogErr("Failed to generate title: %v", err)
					return
				}
				title := strings.SplitN(strings.TrimSpace(resp.Response.Content.Text()), "\n", 2)[0]
				k.Title = title
				if k.Store != nil {
					_ = k.Store.SaveTraceMeta(TraceMeta{
						TraceID:   k.Cfg.TraceID,
						Title:     title,
						StartedAt: time.Now().UnixNano(),
					})
					_ = k.Store.SaveSpanMeta(SpanMeta{
						SpanID:       k.Cfg.SessionID,
						TraceID:      k.Cfg.TraceID,
						ParentSpanID: k.Cfg.ParentSpanID,
						Model:        k.Cfg.Model,
						Title:        title,
					})
				}
				_ = k.Fire(ctx, string(EventTitle), &TitlePayload{
					Title: title,
				})
				u := Usage{}
				u.FromFantasyUsage(resp.TotalUsage, k.Cfg.Model)
				_ = k.Fire(ctx, string(EventTurnCost), &TurnCostPayload{
					TurnUsage:    u,
					TurnCostUSD:  u.Cost,
					TotalCostUSD: u.Cost, // this is the overall expense
				})
			}()
		}

		// Walk StepUsage backwards, accumulating tokens. Steps whose cumulative
		// token total exceeds ToolCallPrunedSize get their history messages trimmed:
		// tool call args are cleared, tool results are truncated to 30 chars.
		if len(k.StepUsage) > 0 && len(k.StepHistoryStart) == len(k.StepUsage) {
			var accumulated int
			for i := len(k.StepUsage) - 1; i >= 0; i-- {
				u := k.StepUsage[i]
				accumulated += int(u.Input + u.Output)
				if accumulated <= k.Cfg.ToolCallPrunedSize {
					continue
				}
				// This step is beyond the budget — trim its history messages.
				start := k.StepHistoryStart[i]
				end := len(k.History)
				if i+1 < len(k.StepHistoryStart) {
					end = k.StepHistoryStart[i+1]
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
									if len(txt.Text) > 800 {
										txt.Text = txt.Text[:800] + "… [trimmed]"
										tr.Output = txt
										msg.Content[p] = tr
									}
								}
							}
						}
					}
				}
			}
		}
	}

	return k.streamCurrent(ctx, w)
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
				runningCost := k.UpdateUse(u, "")
				k.StepUsage = append(k.StepUsage, u)
				if k.Store != nil {
					_ = k.Store.AppendCost(k.Cfg.TraceID, k.Cfg.SessionID, u.Cost, runningCost)
				}
				_ = k.Fire(ctx, string(EventTurnCost), &TurnCostPayload{
					TurnUsage:    u,
					TurnCostUSD:  u.Cost,
					TotalCostUSD: runningCost,
				})

				// Check queue at this safe step boundary. We set a flag and let
				// the stream finish naturally so all steps are collected before
				// we inject the queued messages and restart.
				if !shouldInterrupt {
					if queued := k.drainQueue(); len(queued) > 0 {
						shouldInterrupt = true
						interruptedWith = queued
					}
				}
				return nil
			},

			// Live text streaming — fires for each token delta as it arrives
			OnTextDelta: func(id, text string) error {
				return k.Fire(ctx, string(EventToken), &TokenPayload{Text: text})
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
	k.currentTokens = int(u.Input + u.Output + u.CacheRead + u.CacheWrite)
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
	if k.Store != nil {
		return k.Store.Close()
	}
	return nil
}

// Compact summarizes the current history and resets it.
func (k *Kernel) Compact(ctx context.Context) error {
	if len(k.History) == 0 {
		return nil
	}

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

	// 1. Generate summary by calling the LLM
	agent := fantasy.NewAgent(k.LM, fantasy.WithMaxRetries(5))
	result, err := agent.Generate(ctx, fantasy.AgentCall{
		Prompt:   string(prompt),
		Messages: k.History,
	})
	if err != nil {
		return err
	}
	summary := result.Response.Content.Text()

	// 2. Reset history
	if k.SystemPrompt != "" {
		k.History = []fantasy.Message{fantasy.NewSystemMessage(k.SystemPrompt)}
	} else {
		k.History = []fantasy.Message{}
	}
	k.History = append(k.History, fantasy.NewUserMessage(
		"Tell me the summary of our conversation.",
	))
	msg := fantasy.NewUserMessage(
		"Here is a summary of our previous interaction for your reference:\n\n" + summary,
	)
	msg.Role = fantasy.MessageRoleAssistant
	k.History = append(k.History, msg)

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
	// Inherit provider, model, and key from parent, but clean up
	subCfg := k.Cfg
	subCfg.SessionID = NewSessionID()     // new session ID
	subCfg.TraceID = k.Cfg.TraceID        // inherit trace
	subCfg.ParentSpanID = k.Cfg.SessionID // parent span = current session
	subCfg.GenerateTitle = false          // don't cascade titleGeneration coroutines

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
	if len(k.History) == 0 && k.SystemPrompt != "" {
		k.History = append(k.History, fantasy.NewSystemMessage(k.SystemPrompt))
	}
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
