// harness-eval runs the same agentic task through three coding harnesses
// (toroid, claude, pi), all pointed at the SAME model (llmgateway/glm-5p2 via
// the Razorpay LLM gateway), and measures each on a common set of grounded
// metrics: wall-clock, CPU, peak process-tree RAM, token usage, total cost
// (one shared pricing table applied to every harness's raw tokens), LLM turns,
// tool calls, and a task-success checklist.
//
// Each harness runs in its own throwaway local clone of this repo (checked out
// at master), so the three runs are isolated and each produces an independent
// bench/<harness>/spend-limit branch pushed to origin. Clones (not worktrees)
// because the task requires the agent to `git checkout master` itself, which a
// linked worktree cannot do while the main copy holds master.
//
// Usage (from repo root):
//
//	go run ./examples/harness-eval                # run all three
//	go run ./examples/harness-eval toroid pi      # run a subset
//
// Env required: LLM_GATEWAY_BASE_URL, LLM_GATEWAY_KEY (already used by toroid).
// pi requires ~/.pi/agent/models.json with a `razorpay` provider for glm-5p2.
package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// ---- shared, grounded pricing (USD per 1M tokens) --------------------------
// The three harnesses each report cost from their own (often wrong or zero)
// internal tables. For an apples-to-apples number we IGNORE their self-reported
// cost for ranking and recompute from this single table applied to each
// harness's raw token counts. glm-5p2 has no public price; these are GLM-class
// placeholders — change here and every harness re-costs identically.
// ponytail: assumed flat rate, swap for real gateway pricing when known.
type pricing struct{ in, out, cacheRead, cacheWrite float64 }

var glm = pricing{in: 0.60, out: 2.20, cacheRead: 0.11, cacheWrite: 0.60}

func (p pricing) cost(t tokens) float64 {
	return (float64(t.in)*p.in + float64(t.out)*p.out +
		float64(t.cacheRead)*p.cacheRead + float64(t.cacheWrite)*p.cacheWrite) / 1e6
}

type tokens struct{ in, out, cacheRead, cacheWrite, reasoning int64 }

func (t tokens) total() int64 { return t.in + t.out + t.cacheRead + t.cacheWrite }

// parsed is what each harness's stdout parser extracts.
type parsed struct {
	tok       tokens
	selfCost  float64 // harness's own reported cost, for reference
	turns     int     // LLM round-trips
	toolCalls int
}

type harness struct {
	name   string
	branch string
	bin    string
	args   []string // {{PROMPT}} placeholder is replaced with the prompt text
	env    []string // extra env on top of os.Environ()
	stdin  string   // path to redirect stdin from ("" = inherit /dev/null)
	// captureTarget/captureEnv enable an opt-in local recording proxy for this
	// harness. Set HARNESS_EVAL_CAPTURE=1 (or the harness-specific flag) to use it.
	captureTarget string
	captureEnv    string
	// buildPkg, if set, is `go build`-ed from inside the clean clone into `bin`
	// before the run (used for toroid, which is source in this repo).
	buildPkg string
	parse    func([]byte) parsed
}

type result struct {
	Task          string          `json:"task"`
	Harness       string          `json:"harness"`
	Branch        string          `json:"branch"`
	WallSec       float64         `json:"wall_sec"`
	CPUSec        float64         `json:"cpu_sec"`
	PeakRSSMB     float64         `json:"peak_rss_mb"`
	MeanRSSMB     float64         `json:"mean_rss_mb"`
	PeakProcs     int             `json:"peak_procs"`
	InTok         int64           `json:"input_tokens"`
	OutTok        int64           `json:"output_tokens"`
	CacheReadTok  int64           `json:"cache_read_tokens"`
	CacheWrite    int64           `json:"cache_write_tokens"`
	TotalTok      int64           `json:"total_tokens"`
	TotalCostUSD  float64         `json:"total_cost_usd"`
	LegacyCostUSD float64         `json:"norm_cost_usd,omitempty"`
	SelfCostUSD   float64         `json:"self_cost_usd"`
	Turns         int             `json:"turns"`
	ToolCalls     int             `json:"tool_calls"`
	TokPerSec     float64         `json:"output_tok_per_sec"`
	Success       map[string]bool `json:"success"`
	SuccessScore  int             `json:"success_score"`
	SuccessMax    int             `json:"success_max"`
	Note          string          `json:"note,omitempty"`
}

type taskSpec struct {
	ID              string   `json:"id"`
	Title           string   `json:"title"`
	Difficulty      string   `json:"difficulty,omitempty"`
	BranchSlug      string   `json:"branch_slug"`
	RequiredChanged []string `json:"required_changed"`
	RequiredFiles   []string `json:"required_files"`
	Verify          []string `json:"verify"`
	TimeoutMinutes  int      `json:"timeout_minutes"`
	dir             string
	prompt          string
}

const defaultSuiteBudgetUSD = 10.0

func (t taskSpec) timeout() time.Duration {
	if t.TimeoutMinutes <= 0 {
		return 20 * time.Minute
	}
	return time.Duration(t.TimeoutMinutes) * time.Minute
}

func main() {
	repoRoot, err := repoRootDir()
	must(err)
	evalDir := filepath.Join(repoRoot, "examples", "harness-eval")
	if len(os.Args) > 1 && os.Args[1] == "proxy" {
		runTraceProxyCommand(os.Args[2:])
		return
	}

	if len(os.Args) > 1 && os.Args[1] == "list" {
		listTasks(evalDir)
		return
	}

	taskID, command, pick := parseArgs(os.Args[1:])
	task := loadTask(evalDir, taskID)
	resultDir := filepath.Join(evalDir, "results", task.ID)
	outDir := filepath.Join(resultDir, "out") // raw logs, scoped per task
	must(os.MkdirAll(outDir, 0o755))
	// workDir holds the per-harness clones and the toroid binary. It MUST live
	// OUTSIDE the repo: an agent that finds its clone nested inside the repo can
	// infer the real repo root from its own pwd and escape to the main working
	// tree (observed corrupting results + uncommitted work). A temp path unrelated
	// to the repo gives it nothing to climb to.
	workDir := filepath.Join(os.TempDir(), "harness-eval-work")
	must(os.MkdirAll(workDir, 0o755))

	// recheck: re-verify already-pushed branches and rewrite reports, without
	// re-running any agent (keeps the captured cost/time/RAM metrics).
	if command == "recheck" {
		recheck(repoRoot, resultDir, outDir, workDir, task)
		return
	}
	if command == "recover" {
		if len(pick) != 1 {
			fatal("usage: harness-eval recover <task> <harness>")
		}
		recoverInterrupted(evalDir, resultDir, outDir, task, pick[0])
		return
	}
	if command == "rewrite" {
		writeReport(resultDir, task, orderResults(loadResults(resultDir)))
		return
	}
	if len(pick) == 0 {
		pick = []string{"toroid", "claude", "pi"}
	}

	// The toroid one-shot binary is built later from inside its clean clone (the
	// dirty main tree may not compile), so we measure the agent, not `go build`.
	toroidBin := filepath.Join(workDir, "toroid-oneshot")

	claudeBin, piBin := "claude", "pi"
	if containsName(pick, "claude") {
		claudeBin = mustLook("claude")
	}
	if containsName(pick, "pi") {
		piBin = mustLook("pi")
	}
	gwKey := os.Getenv("LLM_GATEWAY_KEY")
	gwURL := os.Getenv("LLM_GATEWAY_BASE_URL")
	if gwKey == "" || gwURL == "" {
		fatal("LLM_GATEWAY_KEY and LLM_GATEWAY_BASE_URL must be set")
	}
	// claude speaks the Anthropic API; the gateway exposes /v1/messages, so
	// ANTHROPIC_BASE_URL is the gateway root (claude appends /v1/messages).
	anthropicBase := strings.TrimSuffix(gwURL, "/v1")

	all := map[string]harness{
		"toroid": {
			name:   "toroid",
			branch: "bench/toroid/" + task.BranchSlug,
			bin:    toroidBin,
			// master's committed non-interactive runner: prompt is positional,
			// emits the same NDJSON Event stream (TurnCost / PreToolUse).
			args:          []string{"--model", "llmgateway/glm-5p2", "--thinking", "low", "--run", "{{PROMPT}}"},
			buildPkg:      "./examples/cli",
			captureTarget: gwURL,
			captureEnv:    "LLM_GATEWAY_BASE_URL",
			parse:         parseToroid,
		},
		"claude": {
			name:   "claude",
			branch: "bench/claude/" + task.BranchSlug,
			bin:    claudeBin,
			args: []string{"-p", "{{PROMPT}}", "--model", "glm-5p2",
				"--output-format", "stream-json", "--verbose",
				"--dangerously-skip-permissions"},
			env: []string{
				"ANTHROPIC_BASE_URL=" + anthropicBase,
				"ANTHROPIC_AUTH_TOKEN=" + gwKey,
			},
			captureTarget: anthropicBase,
			captureEnv:    "ANTHROPIC_BASE_URL",
			parse:         parseClaude,
		},
		"pi": {
			name:   "pi",
			branch: "bench/pi/" + task.BranchSlug,
			bin:    piBin,
			args: []string{"{{PROMPT}}", "--provider", "razorpay", "--model", "glm-5p2",
				"--mode", "json", "--approve", "--no-session"},
			stdin: os.DevNull, // pi's json mode only exits when stdin is closed
			parse: parsePi,
		},
	}

	// Merge with any prior results so running a subset doesn't drop other rows.
	byName := loadResults(resultDir)
	budget := evalBudget()
	spent := ledgerSpend(evalDir)
	for _, name := range pick {
		h, ok := all[name]
		if !ok {
			fatal("unknown harness: " + name)
		}
		remaining := budget - spent
		if remaining <= 0 {
			fatal(fmt.Sprintf("$%.2f suite budget exhausted (ledger total: $%.4f)", budget, spent))
		}
		fmt.Fprintf(os.Stderr, "\n=== running harness: %s ===\n", name)
		res := runHarness(repoRoot, outDir, workDir, h, task, remaining)
		byName[name] = res
		appendLedger(evalDir, res)
		spent += res.TotalCostUSD
		writeReport(resultDir, task, orderResults(byName)) // incremental: survive a crash
	}
	fmt.Fprintf(os.Stderr, "\nwrote %s\n", filepath.Join(resultDir, "results.md"))
}

func evalBudget() float64 {
	raw := strings.TrimSpace(os.Getenv("HARNESS_EVAL_BUDGET_USD"))
	if raw == "" {
		return defaultSuiteBudgetUSD
	}
	v, err := strconv.ParseFloat(raw, 64)
	if err != nil || v <= 0 {
		fatal("HARNESS_EVAL_BUDGET_USD must be a positive number")
	}
	return v
}

type ledgerEntry struct {
	At            time.Time `json:"at"`
	Task          string    `json:"task"`
	Harness       string    `json:"harness"`
	TotalCostUSD  float64   `json:"total_cost_usd"`
	LegacyCostUSD float64   `json:"norm_cost_usd,omitempty"`
}

func ledgerPath(evalDir string) string {
	return filepath.Join(evalDir, "results", "spend-ledger.jsonl")
}

func ledgerSpend(evalDir string) float64 {
	f, err := os.Open(ledgerPath(evalDir))
	if err != nil {
		return 0
	}
	defer f.Close()
	var total float64
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var entry ledgerEntry
		if json.Unmarshal(sc.Bytes(), &entry) == nil {
			cost := entry.TotalCostUSD
			if cost == 0 {
				cost = entry.LegacyCostUSD
			}
			total += cost
		}
	}
	return total
}

func appendLedger(evalDir string, res result) {
	path := ledgerPath(evalDir)
	_ = os.MkdirAll(filepath.Dir(path), 0o755)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		fatal("open spend ledger: " + err.Error())
	}
	defer f.Close()
	b, _ := json.Marshal(ledgerEntry{At: time.Now().UTC(), Task: res.Task, Harness: res.Harness, TotalCostUSD: res.TotalCostUSD})
	_, _ = f.Write(append(b, '\n'))
}

func containsName(names []string, want string) bool {
	for _, name := range names {
		if name == want {
			return true
		}
	}
	return false
}

func parseArgs(args []string) (taskID, command string, harnesses []string) {
	taskID, command = "spend-limit", "run"
	if len(args) == 0 {
		return
	}
	if args[0] == "recheck" {
		command = "recheck"
		if len(args) > 1 {
			taskID = args[1]
		}
		return
	}
	if args[0] == "recover" {
		command = "recover"
		if len(args) > 1 {
			taskID = args[1]
		}
		if len(args) > 2 {
			harnesses = args[2:]
		}
		return
	}
	if args[0] == "rewrite" {
		command = "rewrite"
		if len(args) > 1 {
			taskID = args[1]
		}
		return
	}
	// Backward compatibility: `harness-eval toroid pi` runs the original task.
	if args[0] == "toroid" || args[0] == "claude" || args[0] == "pi" {
		return taskID, command, args
	}
	return args[0], command, args[1:]
}

func recoverInterrupted(evalDir, resultDir, outDir string, task taskSpec, harnessName string) {
	parser := parserFor(harnessName)
	if parser == nil {
		fatal("unknown harness: " + harnessName)
	}
	raw, err := os.ReadFile(filepath.Join(outDir, harnessName+".stdout.log"))
	must(err)
	res := result{
		Task: task.ID, Harness: harnessName,
		Success: map[string]bool{}, Note: "interrupted run recovered from partial log",
	}
	parsed := parser(raw)
	if harnessName == "claude" && parsed.tok.total() == 0 {
		parsed = parseClaudePartial(raw)
	}
	applyParsed(&res, parsed)
	setScore(&res)
	byName := loadResults(resultDir)
	byName[harnessName] = res
	writeReport(resultDir, task, orderResults(byName))
	appendLedger(evalDir, res)
	fmt.Fprintf(os.Stderr, "recovered %s/%s ($%.4f total cost)\n", task.ID, harnessName, res.TotalCostUSD)
}

func loadTask(evalDir, id string) taskSpec {
	dir := filepath.Join(evalDir, "tasks", id)
	b, err := os.ReadFile(filepath.Join(dir, "task.json"))
	must(err)
	var task taskSpec
	must(json.Unmarshal(b, &task))
	if task.ID == "" || task.ID != id || task.BranchSlug == "" {
		fatal("invalid task manifest: " + filepath.Join(dir, "task.json"))
	}
	prompt, err := os.ReadFile(filepath.Join(dir, "prompt.txt"))
	must(err)
	task.dir, task.prompt = dir, strings.TrimSpace(string(prompt))
	return task
}

func listTasks(evalDir string) {
	entries, err := os.ReadDir(filepath.Join(evalDir, "tasks"))
	must(err)
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		task := loadTask(evalDir, entry.Name())
		difficulty := ""
		if task.Difficulty != "" {
			difficulty = " [" + task.Difficulty + "]"
		}
		fmt.Printf("%-24s %s%s\n", task.ID, task.Title, difficulty)
	}
}

// harnessOrder is the stable display/report order.
var harnessOrder = []string{"toroid", "claude", "pi"}

func loadResults(evalDir string) map[string]result {
	m := map[string]result{}
	b, err := os.ReadFile(filepath.Join(evalDir, "metrics.json"))
	if err != nil {
		return m
	}
	var rs []result
	if json.Unmarshal(b, &rs) == nil {
		for _, r := range rs {
			normalizeResultCost(&r)
			m[r.Harness] = r
		}
	}
	return m
}

func normalizeResultCost(r *result) {
	if r.TotalCostUSD == 0 {
		r.TotalCostUSD = r.LegacyCostUSD
	}
	r.LegacyCostUSD = 0
}

func orderResults(m map[string]result) []result {
	var out []result
	for _, name := range harnessOrder {
		if r, ok := m[name]; ok {
			out = append(out, r)
		}
	}
	return out
}

// runHarness clones the repo (into workDir, OUTSIDE the repo), runs one harness
// against it while sampling RSS, parses its output, checks task success, and
// cleans up the clone. Raw logs go to outDir.
func runHarness(repoRoot, outDir, workDir string, h harness, task taskSpec, budgetUSD float64) result {
	res := result{Task: task.ID, Harness: h.name, Branch: h.branch, Success: map[string]bool{}}

	clone := filepath.Join(workDir, "clone-"+h.name)
	_ = os.RemoveAll(clone)
	if err := run(repoRoot, "git", "clone", "--quiet", repoRoot, clone); err != nil {
		res.Note = "clone failed: " + err.Error()
		return res
	}
	// Point origin at GitHub so the agent's push lands on the real remote, not
	// the local clone source.
	githubURL := strings.TrimSpace(gitOut(repoRoot, "remote", "get-url", "origin"))
	_ = run(clone, "git", "remote", "set-url", "origin", githubURL)
	defer os.RemoveAll(clone)
	branchesBefore := matchingBranches(githubURL, h.branch)

	// Build in-repo harnesses (toroid) from the working source tree so iterative
	// prompt/kernel improvements can be evaluated before they are committed. The
	// agent still works only in its isolated clean clone.
	if h.buildPkg != "" {
		fmt.Fprintf(os.Stderr, "building %s from working tree…\n", h.name)
		b := exec.Command("go", "build", "-o", h.bin, h.buildPkg)
		b.Dir = repoRoot
		b.Stderr = os.Stderr
		if err := b.Run(); err != nil {
			res.Note = "build failed: " + err.Error()
			return res
		}
	}

	prompt := taskPrompt(task, h.name)
	args := make([]string, len(h.args))
	for i, a := range h.args {
		args[i] = strings.ReplaceAll(a, "{{PROMPT}}", prompt)
	}
	// Claude can enforce an additional self-priced ceiling. Its GLM pricing is
	// not comparable to our shared-rate ledger, but a conservative second brake
	// is preferable to an unbounded process.
	if h.name == "claude" {
		args = append(args, "--max-budget-usd", strconv.FormatFloat(budgetUSD, 'f', 4, 64))
	}

	cmd := exec.Command(h.bin, args...)
	cmd.Dir = clone
	cmdEnv := append(os.Environ(), h.env...)
	captureFlag := "HARNESS_EVAL_CAPTURE_" + strings.ToUpper(h.name)
	if h.captureTarget != "" && (envTrue("HARNESS_EVAL_CAPTURE") || envTrue(captureFlag)) {
		traceDir := filepath.Join(outDir, h.name+"-trace")
		_ = os.RemoveAll(traceDir)
		proxyURL, stopProxy, err := startTraceProxy(h.captureTarget, traceDir, "127.0.0.1:0")
		if err != nil {
			res.Note = "capture proxy failed: " + err.Error()
			return res
		}
		defer stopProxy()
		cmdEnv = setEnv(cmdEnv, h.captureEnv, proxyURL)
		fmt.Fprintf(os.Stderr, "capturing %s API traffic in %s\n", h.name, traceDir)
	}
	cmd.Env = cmdEnv
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true} // own process group -> sample the whole tree
	if h.stdin != "" {
		f, err := os.Open(h.stdin)
		if err == nil {
			cmd.Stdin = f
			defer f.Close()
		}
	}
	var stdout bytes.Buffer
	stderrF, _ := os.Create(filepath.Join(outDir, h.name+".stderr.log"))
	defer stderrF.Close()
	// Tee stdout to a log file live so runs can be monitored and partial output
	// survives a crash; the buffer is what the parser consumes.
	stdoutF, _ := os.Create(filepath.Join(outDir, h.name+".stdout.log"))
	defer stdoutF.Close()
	cmd.Stdout = io.MultiWriter(&stdout, stdoutF)
	cmd.Stderr = stderrF

	start := time.Now()
	if err := cmd.Start(); err != nil {
		res.Note = "start failed: " + err.Error()
		return res
	}
	pgid := cmd.Process.Pid

	// Kill the whole group on timeout.
	timer := time.AfterFunc(task.timeout(), func() { _ = syscall.Kill(-pgid, syscall.SIGKILL) })
	// Sample process-tree RSS until the sampler is stopped.
	stopSampler := make(chan struct{})
	var samplerDone sync.WaitGroup
	samplerDone.Add(1)
	var peakRSS, sumRSS float64
	var samples, peakProcs int
	var budgetExceeded atomic.Bool
	go func() {
		defer samplerDone.Done()
		t := time.NewTicker(100 * time.Millisecond)
		defer t.Stop()
		for {
			select {
			case <-stopSampler:
				return
			case <-t.C:
				if raw, err := os.ReadFile(filepath.Join(outDir, h.name+".stdout.log")); err == nil {
					if glm.cost(h.parse(raw).tok) >= budgetUSD {
						budgetExceeded.Store(true)
						_ = syscall.Kill(-pgid, syscall.SIGKILL)
					}
				}
				rssMB, procs := sampleGroupRSS(pgid)
				if procs == 0 {
					continue
				}
				samples++
				sumRSS += rssMB
				if rssMB > peakRSS {
					peakRSS = rssMB
				}
				if procs > peakProcs {
					peakProcs = procs
				}
			}
		}
	}()

	waitErr := cmd.Wait()
	timer.Stop()
	close(stopSampler)
	samplerDone.Wait()

	res.WallSec = round(time.Since(start).Seconds(), 1)
	if st, ok := cmd.ProcessState.SysUsage().(*syscall.Rusage); ok {
		res.CPUSec = round(float64(st.Utime.Nano()+st.Stime.Nano())/1e9, 1)
	}
	res.PeakRSSMB = round(peakRSS, 1)
	if samples > 0 {
		res.MeanRSSMB = round(sumRSS/float64(samples), 1)
	}
	res.PeakProcs = peakProcs
	if waitErr != nil {
		res.Note = strings.TrimSpace(res.Note + " exit: " + waitErr.Error())
	}
	if budgetExceeded.Load() {
		res.Note = strings.TrimSpace(res.Note + fmt.Sprintf(" total-cost budget brake reached ($%.4f)", budgetUSD))
	}

	applyParsed(&res, h.parse(stdout.Bytes()))

	checkSuccess(githubURL, h.branch, workDir, task, branchesBefore, &res)
	return res
}

func taskPrompt(task taskSpec, harnessName string) string {
	branch := "bench/" + harnessName + "/" + task.BranchSlug
	return fmt.Sprintf(`You are working inside a clean checkout of the toroid-kernel Go repository on master. Complete the coding task autonomously and do not ask for confirmation.

%s

Delivery requirements:
1. Create and switch to branch %s. If it exists locally or on origin, append the smallest available numeric suffix (-2, -3, ...); never overwrite or force-push an existing branch.
2. Implement the task and add directly relevant tests.
3. Run gofmt on changed Go files and make sure go test ./... and go build ./... pass.
4. Append a short entry to bench_log.txt with the implementation and exact branch name.
5. Commit all changes with a clear message and push the exact branch to origin with --no-verify.
6. Return to master.`, task.prompt, branch)
}

// applyParsed copies parsed token/turn/cost fields onto a result.
func applyParsed(res *result, p parsed) {
	res.InTok, res.OutTok = p.tok.in, p.tok.out
	res.CacheReadTok, res.CacheWrite = p.tok.cacheRead, p.tok.cacheWrite
	res.TotalTok = p.tok.total()
	res.TotalCostUSD = round(glm.cost(p.tok), 4)
	res.SelfCostUSD = round(p.selfCost, 4)
	res.Turns = p.turns
	res.ToolCalls = p.toolCalls
	if res.WallSec > 0 {
		res.TokPerSec = round(float64(p.tok.out)/res.WallSec, 1)
	}
}

func parserFor(name string) func([]byte) parsed {
	switch name {
	case "toroid":
		return parseToroid
	case "claude":
		return parseClaude
	case "pi":
		return parsePi
	}
	return nil
}

// checkSuccess verifies the task outcomes against the REAL remote, independent
// of whatever local branch state the agent left behind (it may delete its local
// branch after pushing). It clones the pushed branch fresh (into workDir,
// outside the repo) and builds it.
func checkSuccess(githubURL, branch, workDir string, task taskSpec, branchesBefore map[string]string, res *result) {
	if res.Success == nil {
		res.Success = map[string]bool{}
	}
	// 1. Branch on origin? (authoritative). The agent may have suffixed the name
	// (-2, -3, …) to avoid colliding with a prior run's branch, so resolve the
	// actual branch it pushed and verify that one.
	branch, branchSHA := resolveNewBranch(githubURL, branch, branchesBefore)
	res.Branch = branch
	res.Success["branch_pushed"] = branchSHA != ""
	if !res.Success["branch_pushed"] {
		setScore(res)
		return
	}

	// 2. Clone just that branch to inspect + build it.
	vdir := filepath.Join(workDir, "verify-"+res.Harness)
	_ = os.RemoveAll(vdir)
	defer os.RemoveAll(vdir)
	if err := run("", "git", "clone", "--quiet", "--single-branch", "--branch", branch, githubURL, vdir); err != nil {
		res.Note = strings.TrimSpace(res.Note + " verify-clone failed: " + err.Error())
		setScore(res)
		return
	}
	masterSHA := strings.TrimSpace(gitOut(vdir, "ls-remote", githubURL, "refs/heads/master"))
	if f := strings.Fields(masterSHA); len(f) > 0 {
		masterSHA = f[0]
	}
	res.Success["committed"] = branchSHA != masterSHA

	// Diff branch tip vs master.
	_ = run(vdir, "git", "fetch", "--quiet", "--depth", "1", "origin", "master")
	diff := gitOut(vdir, "diff", "--name-only", masterSHA+"..HEAD")
	if diff == "" { // shallow clone may lack the merge base; fall back to tree listing
		diff = gitOut(vdir, "show", "--stat", "--name-only", "HEAD")
	}
	changed := strings.Fields(diff)
	res.Success["required_changes"] = allPathsMatched(changed, task.RequiredChanged)
	res.Success["required_files"] = true
	for _, name := range task.RequiredFiles {
		if run(vdir, "git", "cat-file", "-e", "HEAD:"+name) != nil {
			res.Success["required_files"] = false
		}
	}

	// Build the branch as checked out (HEAD == the pushed branch).
	build := exec.Command("go", "build", "./...")
	build.Dir = vdir
	res.Success["builds"] = build.Run() == nil

	res.Success["task_tests"] = runTaskVerifier(vdir, task, res)
	setScore(res)
}

func allPathsMatched(changed, required []string) bool {
	for _, want := range required {
		found := false
		for _, got := range changed {
			if got == want || strings.HasPrefix(got, strings.TrimSuffix(want, "/")+"/") {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

func runTaskVerifier(vdir string, task taskSpec, res *result) bool {
	overlay := filepath.Join(task.dir, "_verify-overlay")
	if info, err := os.Stat(overlay); err == nil && info.IsDir() {
		if err := copyTree(overlay, vdir); err != nil {
			res.Note = strings.TrimSpace(res.Note + " verifier overlay: " + err.Error())
			return false
		}
	}
	verify := task.Verify
	if len(verify) == 0 {
		verify = []string{"go", "test", "./..."}
	}
	cmd := exec.Command(verify[0], verify[1:]...)
	cmd.Dir = vdir
	output, err := cmd.CombinedOutput()
	if err != nil {
		msg := strings.TrimSpace(string(output))
		if len(msg) > 500 {
			msg = msg[len(msg)-500:]
		}
		res.Note = strings.TrimSpace(res.Note + " verifier failed: " + msg)
		return false
	}
	return true
}

func copyTree(src, dst string) error {
	return filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil || rel == "." {
			return err
		}
		target := filepath.Join(dst, rel)
		if info.IsDir() {
			return os.MkdirAll(target, info.Mode())
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(target, b, info.Mode())
	})
}

// recheck reloads metrics.json and re-verifies each harness's pushed branch.
func recheck(repoRoot, resultDir, outDir, workDir string, task taskSpec) {
	b, err := os.ReadFile(filepath.Join(resultDir, "metrics.json"))
	must(err)
	var results []result
	must(json.Unmarshal(b, &results))
	githubURL := strings.TrimSpace(gitOut(repoRoot, "remote", "get-url", "origin"))
	for i := range results {
		normalizeResultCost(&results[i])
		results[i].Success = map[string]bool{}
		fmt.Fprintf(os.Stderr, "rechecking %s (%s)…\n", results[i].Harness, results[i].Branch)
		// Re-parse tokens from the saved stdout log (picks up parser fixes)
		// without re-running the agent.
		if pf := parserFor(results[i].Harness); pf != nil {
			if raw, err := os.ReadFile(filepath.Join(outDir, results[i].Harness+".stdout.log")); err == nil {
				applyParsed(&results[i], pf(raw))
			}
		}
		// Recheck targets the exact recorded branch, so an empty snapshot makes
		// that branch count as newly observed.
		checkSuccess(githubURL, results[i].Branch, workDir, task, map[string]string{}, &results[i])
	}
	writeReport(resultDir, task, results)
	fmt.Fprintln(os.Stderr, "rechecked; rewrote results.md")
}

// resolveBranch finds the branch the agent actually pushed for a given base
// name. With the "append -N on collision" rule, many runs accumulate branches
// (base, base-2, base-3, …); the one this run created is the highest-numbered.
// Returns (chosenBranch, tipSHA); tipSHA is "" if none exist.
func matchingBranches(githubURL, base string) map[string]string {
	out := gitOut("", "ls-remote", "--heads", githubURL, base, base+"-*")
	refs := map[string]string{}
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) >= 2 {
			refs[strings.TrimPrefix(f[1], "refs/heads/")] = f[0]
		}
	}
	return refs
}

func resolveNewBranch(githubURL, base string, before map[string]string) (string, string) {
	refs := matchingBranches(githubURL, base)
	return chooseNewBranch(base, refs, before)
}

func chooseNewBranch(base string, refs, before map[string]string) (string, string) {
	best, bestSHA, bestN := base, "", -1
	for name, sha := range refs {
		if oldSHA, existed := before[name]; existed && oldSHA == sha {
			continue
		}
		n := 0
		switch {
		case name == base:
			n = 1
		case strings.HasPrefix(name, base+"-"):
			v, err := strconv.Atoi(name[len(base)+1:])
			if err != nil {
				continue // non-numeric suffix — not ours
			}
			n = v
		default:
			continue
		}
		if n > bestN {
			bestN, best, bestSHA = n, name, sha
		}
	}
	return best, bestSHA
}

var successKeys = []string{"branch_pushed", "committed", "required_changes", "required_files", "builds", "task_tests"}

func setScore(res *result) {
	res.SuccessScore, res.SuccessMax = 0, len(successKeys)
	for _, key := range successKeys {
		if res.Success[key] {
			res.SuccessScore++
		}
	}
}

// ---- parsers ---------------------------------------------------------------

// parseToroid reads toroid's NDJSON event stream.
func parseToroid(b []byte) parsed {
	var p parsed
	forEachJSONLine(b, func(m map[string]any) {
		switch str(m["kind"]) {
		case "PreToolUse":
			p.toolCalls++
		case "TurnCost":
			p.turns++
			pl, _ := m["payload"].(map[string]any)
			if pl == nil {
				return
			}
			if u, ok := pl["turn_usage"].(map[string]any); ok {
				p.tok.in += intv(u["Input"])
				p.tok.out += intv(u["Output"])
				p.tok.reasoning += intv(u["Reasoning"])
				p.tok.cacheRead += intv(u["CacheRead"])
				p.tok.cacheWrite += intv(u["CacheWrite"])
			}
			p.selfCost = floatv(pl["total_cost_usd"]) // cumulative; keep last
		}
	})
	return p
}

// parseClaude reads claude's --output-format stream-json NDJSON.
func parseClaude(b []byte) parsed {
	var p parsed
	forEachJSONLine(b, func(m map[string]any) {
		switch str(m["type"]) {
		case "assistant":
			msg, _ := m["message"].(map[string]any)
			if msg == nil {
				return
			}
			if content, ok := msg["content"].([]any); ok {
				for _, c := range content {
					if cm, ok := c.(map[string]any); ok && str(cm["type"]) == "tool_use" {
						p.toolCalls++
					}
				}
			}
		case "result":
			p.turns = int(intv(m["num_turns"]))
			p.selfCost = floatv(m["total_cost_usd"])
			// modelUsage is Claude Code's authoritative per-model rollup and,
			// unlike result.usage, reports cached-input reads (which dominate a
			// cached agent run). Sum across models.
			if mu, ok := m["modelUsage"].(map[string]any); ok {
				for _, v := range mu {
					u, ok := v.(map[string]any)
					if !ok {
						continue
					}
					p.tok.in += intv(u["inputTokens"])
					p.tok.out += intv(u["outputTokens"])
					p.tok.cacheRead += intv(u["cacheReadInputTokens"])
					p.tok.cacheWrite += intv(u["cacheCreationInputTokens"])
				}
			}
		}
	})
	return p
}

// parseClaudePartial recovers usage from per-assistant messages when an
// interrupted Claude run never emitted its authoritative final result event.
func parseClaudePartial(b []byte) parsed {
	var p parsed
	forEachJSONLine(b, func(m map[string]any) {
		if str(m["type"]) != "assistant" {
			return
		}
		msg, _ := m["message"].(map[string]any)
		if msg == nil {
			return
		}
		p.turns++
		u, _ := msg["usage"].(map[string]any)
		p.tok.in += intv(u["input_tokens"])
		p.tok.out += intv(u["output_tokens"])
		p.tok.cacheRead += intv(u["cache_read_input_tokens"])
		p.tok.cacheWrite += intv(u["cache_creation_input_tokens"])
		if content, ok := msg["content"].([]any); ok {
			for _, c := range content {
				if cm, ok := c.(map[string]any); ok && str(cm["type"]) == "tool_use" {
					p.toolCalls++
				}
			}
		}
	})
	return p
}

// parsePi reads pi's --mode json NDJSON. Each assistant API call ends with a
// message_end carrying that call's usage; sum them for the transcript total.
func parsePi(b []byte) parsed {
	var p parsed
	forEachJSONLine(b, func(m map[string]any) {
		switch str(m["type"]) {
		case "tool_execution_start": // pi's actual tool-invocation event
			p.toolCalls++
		case "turn_end":
			p.turns++
		case "message_end":
			msg, _ := m["message"].(map[string]any)
			if msg == nil {
				return
			}
			u, _ := msg["usage"].(map[string]any)
			if u == nil {
				return
			}
			p.tok.in += intv(u["input"])
			p.tok.out += intv(u["output"])
			p.tok.reasoning += intv(u["reasoning"])
			p.tok.cacheRead += intv(u["cacheRead"])
			p.tok.cacheWrite += intv(u["cacheWrite"])
			if cost, ok := u["cost"].(map[string]any); ok {
				p.selfCost += floatv(cost["total"])
			}
		}
	})
	return p
}

// ---- report ----------------------------------------------------------------

func writeReport(resultDir string, task taskSpec, rs []result) {
	_ = os.MkdirAll(resultDir, 0o755)
	// JSON
	jb, _ := json.MarshalIndent(rs, "", "  ")
	_ = os.WriteFile(filepath.Join(resultDir, "metrics.json"), jb, 0o644)

	// Markdown table
	var sb strings.Builder
	sb.WriteString("# Harness eval: " + task.Title + "\n\n")
	sb.WriteString("Task `" + task.ID + "`; toroid vs claude vs pi; same model (`llmgateway/glm-5p2` via Razorpay gateway), isolated clones.\n")
	if task.Difficulty != "" {
		sb.WriteString("Difficulty: **" + task.Difficulty + "**.\n")
	}
	sb.WriteString(fmt.Sprintf("Total cost uses one shared rate (in $%.2f / out $%.2f / cacheRead $%.2f per 1M tok).\n\n",
		glm.in, glm.out, glm.cacheRead))
	sb.WriteString("| metric | " + colHeaders(rs) + " |\n")
	sb.WriteString("|---|" + strings.Repeat("---|", len(rs)) + "\n")
	rowF := func(label string, f func(result) string) {
		sb.WriteString("| " + label + " | ")
		cells := make([]string, len(rs))
		for i, r := range rs {
			cells[i] = f(r)
		}
		sb.WriteString(strings.Join(cells, " | ") + " |\n")
	}
	rowF("success /6", func(r result) string { return fmt.Sprintf("%d", r.SuccessScore) })
	rowF("wall time (s)", func(r result) string { return fmt.Sprintf("%.1f", r.WallSec) })
	rowF("CPU time (s)", func(r result) string { return fmt.Sprintf("%.1f", r.CPUSec) })
	rowF("peak RAM (MB)", func(r result) string { return fmt.Sprintf("%.0f", r.PeakRSSMB) })
	rowF("mean RAM (MB)", func(r result) string { return fmt.Sprintf("%.0f", r.MeanRSSMB) })
	rowF("peak procs", func(r result) string { return fmt.Sprintf("%d", r.PeakProcs) })
	rowF("LLM turns", func(r result) string { return fmt.Sprintf("%d", r.Turns) })
	rowF("tool calls", func(r result) string { return fmt.Sprintf("%d", r.ToolCalls) })
	rowF("input tok", func(r result) string { return fmt.Sprintf("%d", r.InTok) })
	rowF("output tok", func(r result) string { return fmt.Sprintf("%d", r.OutTok) })
	rowF("cache read tok", func(r result) string { return fmt.Sprintf("%d", r.CacheReadTok) })
	rowF("total tok", func(r result) string { return fmt.Sprintf("%d", r.TotalTok) })
	rowF("out tok/s", func(r result) string { return fmt.Sprintf("%.1f", r.TokPerSec) })
	rowF("total cost ($)", func(r result) string { return fmt.Sprintf("%.4f", r.TotalCostUSD) })
	rowF("self-reported cost ($)", func(r result) string { return fmt.Sprintf("%.4f", r.SelfCostUSD) })
	for _, k := range successKeys {
		key := k
		rowF("✓ "+key, func(r result) string {
			if r.Success[key] {
				return "yes"
			}
			return "no"
		})
	}
	sb.WriteString("\n")
	for _, r := range rs {
		if r.Note != "" {
			sb.WriteString(fmt.Sprintf("- **%s** note: %s\n", r.Harness, r.Note))
		}
	}
	_ = os.WriteFile(filepath.Join(resultDir, "results.md"), []byte(sb.String()), 0o644)
}

func colHeaders(rs []result) string {
	h := make([]string, len(rs))
	for i, r := range rs {
		h[i] = r.Harness
	}
	return strings.Join(h, " | ")
}

// ---- process-tree RSS sampling ---------------------------------------------

// sampleGroupRSS sums RSS (MB) over every process whose process-group id equals
// pgid, and returns the process count. macOS/Linux `ps` both support these cols.
func sampleGroupRSS(pgid int) (float64, int) {
	out, err := exec.Command("ps", "-Ao", "pgid=,rss=").Output()
	if err != nil {
		return 0, 0
	}
	var totalKB, procs int64
	sc := bufio.NewScanner(bytes.NewReader(out))
	target := strconv.Itoa(pgid)
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) < 2 {
			continue
		}
		if f[0] != target {
			continue
		}
		kb, _ := strconv.ParseInt(f[1], 10, 64)
		totalKB += kb
		procs++
	}
	return float64(totalKB) / 1024.0, int(procs)
}

// ---- small helpers ---------------------------------------------------------

func forEachJSONLine(b []byte, fn func(map[string]any)) {
	sc := bufio.NewScanner(bytes.NewReader(b))
	sc.Buffer(make([]byte, 1024*1024), 16*1024*1024) // agent streams have big lines
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 || line[0] != '{' {
			continue
		}
		var m map[string]any
		if json.Unmarshal(line, &m) == nil {
			fn(m)
		}
	}
}

func str(v any) string { s, _ := v.(string); return s }
func floatv(v any) float64 {
	switch n := v.(type) {
	case float64:
		return n
	case json.Number:
		f, _ := n.Float64()
		return f
	}
	return 0
}
func intv(v any) int64 { return int64(floatv(v)) }

func round(f float64, places int) float64 {
	p := 1.0
	for i := 0; i < places; i++ {
		p *= 10
	}
	return float64(int64(f*p+0.5)) / p
}

func repoRootDir() (string, error) {
	out, err := exec.Command("git", "rev-parse", "--show-toplevel").Output()
	return strings.TrimSpace(string(out)), err
}

func run(dir, name string, args ...string) error {
	c := exec.Command(name, args...)
	c.Dir = dir
	return c.Run()
}

func gitOut(dir string, args ...string) string {
	c := exec.Command("git", args...)
	c.Dir = dir
	out, _ := c.Output()
	return string(out)
}

func mustLook(name string) string {
	p, err := exec.LookPath(name)
	if err != nil {
		fatal(name + " not found on PATH")
	}
	return p
}

func must(err error) {
	if err != nil {
		fatal(err.Error())
	}
}
func fatal(msg string) {
	fmt.Fprintln(os.Stderr, "harness-eval:", msg)
	os.Exit(1)
}
