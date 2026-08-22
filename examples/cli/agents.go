package main

import (
	"os"
	"path/filepath"
	"strings"
)

// withAgentsReminder appends AGENTS.md under <system-reminder> tags to the
// user prompt when present. It is intentionally attached as user content (not
// the kernel system prompt) so it does not bust the cached system prefix.
func withAgentsReminder(prompt, workDir string) string {
	p := filepath.Join(workDir, "AGENTS.md")
	b, err := os.ReadFile(p)
	if err != nil {
		return prompt
	}
	content := strings.TrimSpace(string(b))
	if content == "" {
		return prompt
	}
	reminder := "<system-reminder>\n" + content + "\n</system-reminder>"
	if strings.TrimSpace(prompt) == "" {
		return reminder
	}
	return prompt + "\n\n" + reminder
}
