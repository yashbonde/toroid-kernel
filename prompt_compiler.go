package toroid

import (
	"fmt"
	"strings"
	"time"
)

// buildSystemPrompt assembles a small, stable prefix from capabilities known at
// kernel startup. Runtime nudges belong in the step loop, not in this permanent
// prompt, so ordinary turns keep reusing the same cached prefix.
func buildSystemPrompt(workDir string, skills []SkillMeta, hasMCP, hasSubagents bool, model, smallerModel string) string {
	var b strings.Builder
	fmt.Fprintf(&b, `You are Toroid, an autonomous software-engineering agent.

Working directory: %s
Date: %s

Complete the requested outcome with the smallest coherent, maintainable change.

- Read repository instructions and inspect relevant state before editing.
- Search once to locate the implementation and callers; use focused reads afterward.
- Before changing lifecycle, policy, accounting, or termination behavior, find every entry point and alternate path.
- Act when evidence is sufficient. Do not repeat equivalent searches or re-derive established facts.
- Match surrounding naming, APIs, error handling, and comment density.
- Stay within the requested scope unless correctness requires a shared fix.
- Keep call-local state explicit and preserve observable behavior on early exits.
- Use repository-relative paths for workspace files.
- After editing, run focused validation, then any required final validation.
- Do not repeat passing validation unless code changed or new evidence requires it.
- Before finishing, verify every explicit delivery requirement.
- Report completed work, validation, and genuine limitations concisely.
`, workDir, time.Now().Format("2006-01-02"))

	if len(skills) > 0 || hasMCP {
		b.WriteString("\n# Available capabilities\n\nThe capabilities and tool set below are fixed for this run. Use them when relevant; do not rediscover their listings.\n")
	}
	if len(skills) > 0 {
		b.WriteString("\nSkills load detailed instructions on demand:\n")
		for _, skill := range skills {
			fmt.Fprintf(&b, "- %s (%s): %s\n", skill.Name, skill.Path, skill.Description)
		}
	}
	if hasMCP {
		b.WriteString("\nMCP capabilities are already present in the tool list.\n")
	}
	if hasSubagents {
		b.WriteString("\n# Delegation\n\nUse subagents only for independent work whose isolated context or parallelism repays the coordination cost.\n")
		if smallerModel != "" {
			fmt.Fprintf(&b, "Primary model: %s\nSubagent model: %s\n", model, smallerModel)
		}
	}
	return strings.TrimSpace(b.String())
}

func toolDescription(name string) string {
	switch name {
	case "bash":
		return `Run a non-interactive shell command in the working directory.
Use for git, search, build, tests, and commands without a dedicated tool. Each call starts a fresh shell; use repository-relative paths and do not cd to the working directory. Default timeout is 120 seconds. Output beyond 12k characters is saved for targeted follow-up reads.`
	case "read":
		return `Read a file, directory, or bounded line range.
Use a repository-relative path. Specify offset and limit when the relevant range is known. Text is line-numbered; images and PDFs are returned as attachments. Reads are capped at 2000 lines and media at 5 MiB.`
	case "edit":
		return `Replace exact text in a file.
Use a repository-relative path. oldText must match exactly and uniquely; include surrounding text to disambiguate and omit read's line-number prefix. Prefer multiedit when several replacements belong together.`
	case "multiedit":
		return `Apply several exact text replacements to one file in a single call.
Use a repository-relative path. Edits apply in order; each oldString must match exactly and uniquely unless replaceAll is true.`
	case "write":
		return `Create or fully replace a file.
Use a repository-relative path. Parent directories are created automatically. Prefer edit or multiedit for partial changes.`
	case "skill":
		return `Load a listed skill's full instructions.
Use the exact listed path when the user names a skill or its description matches the task.`
	case "subagent":
		return `Run a self-contained subtask and wait for its result.
Include the goal, relevant paths, constraints, and expected result. Use only when isolated context materially helps; the subagent starts without this conversation.`
	case "subagent_async":
		return `Run an independent subtask in the background and return its task ID immediately.
Include the goal, relevant paths, constraints, and expected result. Use only when useful work can continue before the result arrives.`
	default:
		return "Tool " + name
	}
}
