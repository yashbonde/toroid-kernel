package toroid

import (
	"context"
	"database/sql"
	"encoding/json"
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
	_ "modernc.org/sqlite"
)

// Thinking controls the model's thinking budget.
type Thinking string

const (
	ThinkingNone Thinking = "none" // disable thinking (budget=0)
	ThinkingLow  Thinking = "low"  // ~1k tokens
	ThinkingHigh Thinking = "high" // ~8k tokens
)

// Kernel is the agentic orchestrator powered by Fantasy.
type Kernel struct {
	cfg              Config
	provider         fantasy.Provider
	model            fantasy.LanguageModel
	hooks            *HookRegistry
	tools            *tools.Registry
	store            *Store
	seq              atomic.Uint64
	systemPrompt     string
	title            string
	history          []fantasy.Message
	stepUsage        []Usage          // per-step token usage, index-aligned with stepHistoryStart
	stepHistoryStart []int            // history index where each step's messages begin
	usage            map[string]Usage // sessionID -> total tokens used (self + subagents)
	usageMu          sync.Mutex
	fantasyAgentOpts []fantasy.AgentOption
	currentTokens    int
	runningCostUSD   float64
	todoDB           *sql.DB
}

// Config holds all options for creating a Kernel.
type Config struct {
	Provider       fantasy.Provider `json:"provider,omitempty" description:"llm provider"`
	Model          string           `json:"model" description:"llm model name" default:"gemini-3-flash-preview"`
	APIKey         string           `json:"api_key,omitempty" description:"API key for the provider"`
	SessionID      string           `json:"session_id,omitempty" description:"unique identifier for the session"`
	WorkDir        string           `json:"work_dir" description:"working directory" default:"current directory"`
	MaxIter        int              `json:"max_iter" description:"max tool-call iterations" default:"50"`
	Thinking       Thinking         `json:"thinking" description:"thinking budget: none | low | high" default:"none"`
	ThinkingWriter io.Writer        `json:"-"`

	// Trace/span hierarchy
	TraceID      string `json:"trace_id,omitempty"`       // inherited from parent; root sets TraceID = SessionID
	ParentSpanID string `json:"parent_span_id,omitempty"` // parent kernel's SessionID

	// Persistence
	Save bool `json:"save" description:"persist events, costs and metadata to the bbolt store" default:"false"`

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

	ApplyDefaults(&cfg) // cfg OR default_cfg

	// Open todo SQLite DB
	var todoDB *sql.DB
	if sqlitePath, err := SqlitePath(); err == nil {
		if db, err := sql.Open("sqlite", sqlitePath); err == nil {
			todoDB = db
		}
	}

	// Kernel object
	k := &Kernel{
		cfg:           cfg,
		hooks:         &HookRegistry{},
		usage:         map[string]Usage{},
		currentTokens: 0,
		todoDB:        todoDB,
	}
	k.tools = initTools(k)

	// Initialize bbolt Store (only when --save is set)
	if cfg.Save {
		store, err := NewStore()
		if err != nil {
			return nil, err
		}
		k.store = store

		_ = store.SaveSpanMeta(SpanMeta{
			SpanID:       cfg.SessionID,
			TraceID:      cfg.TraceID,
			ParentSpanID: cfg.ParentSpanID,
			Model:        cfg.Model,
			StartedAt:    time.Now().UnixNano(),
		})
		if cfg.TraceID == cfg.SessionID {
			_ = store.SaveTraceMeta(TraceMeta{
				TraceID:   cfg.TraceID,
				StartedAt: time.Now().UnixNano(),
			})
		} else {
			// Load existing title so we can update it
			meta, _ := store.LoadTraceMeta(cfg.TraceID)
			k.title = meta.Title
			// Restore conversation history by replaying stored events (post-last-compaction only).
			if msgs, err2 := ReconstructHistory(cfg.TraceID, cfg.SessionID, ""); err2 == nil && len(msgs) > 0 {
				k.history = msgs
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
	k.systemPrompt = systemPrompt

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
	k.model = model

	// Build Fantasy Tools
	var fTools []fantasy.AgentTool
	for _, t := range k.tools.Tools() {
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
	k.fantasyAgentOpts = opts

	return k, nil
}

func initTools(ag *Kernel) *tools.Registry {
	r := tools.NewRegistry()

	getDescription := func(name string) string {
		b, _ := readPrompt(name + ".tool.tmpl")
		lines := strings.Split(string(b), "\n")
		if len(lines) > 1 {
			return lines[1]
		}
		return "Tool " + name
	}

	r.Register(tools.NewReadTool(ag, getDescription("read")))
	r.Register(tools.NewWriteTool(ag, getDescription("write")))
	r.Register(tools.NewLsTool(ag, getDescription("ls")))
	r.Register(tools.NewBashTool(ag, getDescription("bash")))
	r.Register(tools.NewEditTool(ag, getDescription("edit")))
	r.Register(tools.NewGlobTool(ag, getDescription("glob")))
	r.Register(tools.NewGrepTool(ag, getDescription("grep")))
	r.Register(tools.NewMultiEditTool(ag, getDescription("multiedit")))
	r.Register(tools.NewNotifyTool(ag, getDescription("notify")))
	r.Register(tools.NewSubagentTool(ag, getDescription("subagent")))
	r.Register(tools.NewTodoWriteTool(ag, ag.todoDB, getDescription("todowrite")))
	r.Register(tools.NewTodoReadTool(ag, ag.todoDB, getDescription("todoread")))
	return r
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
func (k *Kernel) WorkDir() string   { return k.cfg.WorkDir }
func (k *Kernel) SessionID() string { return k.cfg.SessionID }
func (k *Kernel) Model() string     { return k.cfg.Model }

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
	return k.currentTokens, k.cfg.TotalContextSize
}

func (k *Kernel) Logf(msg string, args ...any) {
	LogInfo("["+k.cfg.TraceID+"] "+msg, args...)
}

func (k *Kernel) LogErr(msg string, args ...any) {
	LogError("["+k.cfg.TraceID+"] "+msg, args...)
}

// FireTraceLog emits an EventTraceLog with the given severity and message.
func (k *Kernel) FireTraceLog(ctx context.Context, logType, message string) error {
	return k.Fire(ctx, string(EventTraceLog), &TraceLogPayload{Type: logType, Message: message})
}

func (k *Kernel) Fire(ctx context.Context, kind string, payload any) error {
	event := Event{
		Kind:      EventKind(kind),
		SessionID: k.cfg.SessionID,
		TraceID:   k.cfg.TraceID,
		SpanID:    k.cfg.SessionID,
		EmitTS:    time.Now().UnixNano(),
		Seq:       k.seq.Add(1),
		Payload:   payload,
	}
	if k.store != nil && event.Kind != EventToken && event.Kind != EventReasoning {
		_ = k.store.AppendEvent(k.cfg.TraceID, k.cfg.SessionID, event)
	}
	return k.hooks.Fire(ctx, event)
}

// TODO: Improve this function, idea is to use this as a single place for all usage udpates
func (k *Kernel) UpdateUse(u Usage, key string) {
	k.usageMu.Lock()
	if len(key) == 0 {
		k.usage[k.cfg.SessionID] = u
	} else {
		k.usage[key] = u
	}
	k.currentTokens = int(u.Input + u.Output + u.CacheRead + u.CacheWrite)
	k.usageMu.Unlock()
}

// On registers a hook for an event kind.
func (k *Kernel) On(kind EventKind, fn HookFn) {
	k.hooks.On(kind, fn)
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
	if len(k.history) == 0 {
		_ = k.Fire(ctx, string(EventSessionStart), nil)
		if k.systemPrompt != "" {
			k.history = append(k.history, fantasy.NewSystemMessage(k.systemPrompt))
		}
	}

	// Auto-compact if approaching context limit; important to do this before adding user prompt
	if k.currentTokens > 0 && k.currentTokens >= k.cfg.TotalContextSize-k.cfg.CompactionBufferSize {
		k.Logf("auto-compacting: currentTokens=%d threshold=%d", k.currentTokens, k.cfg.TotalContextSize-k.cfg.CompactionBufferSize)
		if err := k.Compact(ctx); err != nil {
			return err
		}
	}

	// append user message; important to do this before history validation
	k.history = append(k.history, fantasy.NewUserMessage(prompt))

	// history validation
	if len(k.history) > 0 {
		if k.systemPrompt != "" && k.history[0].Role != fantasy.MessageRoleSystem {
			k.LogErr("Kernel provided with system prompt. SysPrompt should be first item in history, found: '%s'", k.history[len(k.history)-1].Role)
			panic("Kernel provided with system prompt. SysPrompt should be first item in history")
		}
		if k.history[len(k.history)-1].Role != fantasy.MessageRoleUser {
			k.LogErr("Last item (%d) is 'user' message. Got: '%s'", len(k.history)-1, k.history[len(k.history)-1].Role)
			panic("Last item is 'user' message.")
		}

		// Shoot out a coroutine to generate title, last message is always user so this
		// works fine. Why not use a subagent?
		// Because subagents are meant for complex tasks that requires the full prompt
		// and intelligence. We can get away with a very small prompt and thus less cost
		if k.cfg.GenerateTitle && k.title == "" {
			go func() {
				ctx := context.Background()
				agent := fantasy.NewAgent(k.model, k.fantasyAgentOpts...)
				titlePrompt, err := readPrompt("title.kernel.tmpl")
				if err != nil {
					k.LogErr("Failed to read title prompt: %v", err)
					return
				}
				// feed the last message and
				resp, err := agent.Generate(ctx, fantasy.AgentCall{
					Prompt:   string(titlePrompt),
					Messages: k.history[len(k.history)-1:],
				})
				if err != nil {
					k.LogErr("Failed to generate title: %v", err)
					return
				}
				title := strings.SplitN(strings.TrimSpace(resp.Response.Content.Text()), "\n", 2)[0]
				k.title = title
				if k.store != nil {
					_ = k.store.SaveTraceMeta(TraceMeta{
						TraceID:   k.cfg.TraceID,
						Title:     title,
						StartedAt: time.Now().UnixNano(),
					})
					_ = k.store.SaveSpanMeta(SpanMeta{
						SpanID:       k.cfg.SessionID,
						TraceID:      k.cfg.TraceID,
						ParentSpanID: k.cfg.ParentSpanID,
						Model:        k.cfg.Model,
						Title:        title,
					})
				}
				_ = k.Fire(ctx, string(EventTitle), &TitlePayload{
					Title: title,
				})
				u := Usage{}
				u.FromFantasyUsage(resp.TotalUsage, k.cfg.Model)
				_ = k.Fire(ctx, string(EventTurnCost), &TurnCostPayload{
					TurnUsage:    u,
					TurnCostUSD:  u.Cost,
					TotalCostUSD: u.Cost, // this is the overall expense
				})
			}()
		}

		// Walk stepUsage backwards, accumulating tokens. Steps whose cumulative
		// token total exceeds ToolCallPrunedSize get their history messages trimmed:
		// tool call args are cleared, tool results are truncated to 30 chars.
		if len(k.stepUsage) > 0 && len(k.stepHistoryStart) == len(k.stepUsage) {
			var accumulated int
			for i := len(k.stepUsage) - 1; i >= 0; i-- {
				u := k.stepUsage[i]
				accumulated += int(u.Input + u.Output)
				if accumulated <= k.cfg.ToolCallPrunedSize {
					continue
				}
				// This step is beyond the budget — trim its history messages.
				start := k.stepHistoryStart[i]
				end := len(k.history)
				if i+1 < len(k.stepHistoryStart) {
					end = k.stepHistoryStart[i+1]
				}
				for j := start; j < end; j++ {
					msg := &k.history[j]
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
									if len(txt.Text) > 30 {
										txt.Text = txt.Text[:30] + "… [trimmed]"
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

	// Build Agent and handle streaming and events
	agent := fantasy.NewAgent(k.model, k.fantasyAgentOpts...)
	result, err := agent.Stream(ctx, fantasy.AgentStreamCall{
		Prompt:   prompt,
		Messages: k.history,

		// Per-step cost accounting
		OnStepFinish: func(step fantasy.StepResult) error {
			u := Usage{}
			u.FromFantasyUsage(step.Usage, k.cfg.Model)
			k.usageMu.Lock()
			k.runningCostUSD += u.Cost
			runningCost := k.runningCostUSD
			k.currentTokens = int(u.Input + u.Output + u.CacheRead + u.CacheWrite)
			k.usageMu.Unlock()
			k.stepUsage = append(k.stepUsage, u)
			if k.store != nil {
				_ = k.store.AppendCost(k.cfg.TraceID, k.cfg.SessionID, u.Cost, runningCost)
			}
			_ = k.Fire(ctx, string(EventTurnCost), &TurnCostPayload{
				TurnUsage:    u,
				TurnCostUSD:  u.Cost,
				TotalCostUSD: runningCost,
			})
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
	if err != nil {
		return err
	}

	// Append step messages to history and fire EventAssistantTurn with full content blocks.
	var allStepMsgs []fantasy.Message
	for _, step := range result.Steps {
		k.stepHistoryStart = append(k.stepHistoryStart, len(k.history))
		k.history = append(k.history, step.Messages...)
		allStepMsgs = append(allStepMsgs, step.Messages...)
	}
	if len(allStepMsgs) > 0 {
		if msgBytes, err2 := json.Marshal(allStepMsgs); err2 == nil {
			_ = k.Fire(ctx, string(EventAssistantTurn), &AssistantTurnPayload{
				Messages: json.RawMessage(msgBytes),
			})
		}
	}

	// Update Usage (Per-Turn)
	var u Usage
	u.FromFantasyUsage(result.TotalUsage, k.cfg.Model)
	k.UpdateUse(u, "")

	if k.cfg.ShowHistory != nil && *k.cfg.ShowHistory {
		// Print history (after usage update so currentTokens is accurate)
		PrettyPrintHistory(k)
	}

	// Fire stop with usage
	usageSnapshot := make(map[string]Usage)
	k.usageMu.Lock()
	for k, v := range k.usage {
		usageSnapshot[k] = v
	}
	k.usageMu.Unlock()

	_ = k.Fire(ctx, string(EventStop), &UsagePayload{Tokens: usageSnapshot})

	// write response and exit
	w.Write([]byte(result.Response.Content.Text()))
	return nil
}

// Compact summarizes the current history and resets it.
func (k *Kernel) Compact(ctx context.Context) error {
	if len(k.history) == 0 {
		return nil
	}

	_ = k.Fire(ctx, string(EventPreCompact), &CompactPayload{
		MessageCount: len(k.history),
	})

	prompt, err := readPrompt("compact.kernel.tmpl")
	if err != nil {
		return err
	}

	// 1. Generate summary by calling the LLM
	agent := fantasy.NewAgent(k.model, fantasy.WithMaxRetries(5))
	result, err := agent.Generate(ctx, fantasy.AgentCall{
		Prompt:   string(prompt),
		Messages: k.history,
	})
	if err != nil {
		return err
	}
	summary := result.Response.Content.Text()

	// Fire post-compact event so history can be reconstructed from events alone.
	_ = k.Fire(ctx, string(EventPostCompact), &CompactSummaryPayload{Summary: summary})

	// 2. Reset history
	if k.systemPrompt != "" {
		k.history = []fantasy.Message{fantasy.NewSystemMessage(k.systemPrompt)}
	} else {
		k.history = []fantasy.Message{}
	}
	k.history = append(k.history, fantasy.NewUserMessage(
		"Tell me the summary of our conversation.",
	))
	msg := fantasy.NewUserMessage(
		"Here is a summary of our previous interaction for your reference:\n\n" + summary,
	)
	msg.Role = fantasy.MessageRoleAssistant
	k.history = append(k.history, msg)

	return nil
}

// RunSubagent runs a subagent synchronously and returns its output.
func (k *Kernel) RunSubagent(ctx context.Context, task string) (string, error) {
	// Inherit provider, model, and key from parent, but clean up
	subCfg := k.cfg
	subCfg.SessionID = NewSessionID()     // new session ID
	subCfg.TraceID = k.cfg.TraceID        // inherit trace
	subCfg.ParentSpanID = k.cfg.SessionID // parent span = current session
	subCfg.GenerateTitle = false          // don't cascade titleGeneration coroutines

	// Create an independent Kernel instance for the subagent
	subKernel, err := NewKernel(ctx, subCfg)
	if err != nil {
		return "", fmt.Errorf("failed to initialize subagent: %w", err)
	}

	// Fire an event to let the system know a subagent is starting
	_ = k.Fire(ctx, string(EventSubagentStart), &SubagentPayload{
		SessionID: subKernel.cfg.SessionID,
		Prompt:    task,
	})

	// Run the subagent on the task
	output, usage, err := subKernel.Run(ctx, task)

	// Fire stop event for the subagent
	_ = k.Fire(ctx, string(EventSubagentStop), &SubagentPayload{
		SessionID:    subKernel.cfg.SessionID,
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
