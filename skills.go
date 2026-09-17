package toroid

import (
	"os"
	"path/filepath"
	"strings"
)

// SkillMeta is the discovery-time metadata for a skill file — a name and
// short description, enough for the model to know a skill exists and decide
// whether it's relevant. The full body is only read when the model (or the
// user, by naming the file directly) calls the skill tool with Path.
type SkillMeta struct {
	Name        string
	Description string
	Path        string
}

// skillsDir returns ~/.toroid/skills, creating it if needed.
func skillsDir() (string, error) {
	home, err := toroidHome()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(home, "skills")
	if err := os.MkdirAll(dir, 0755); err != nil {
		return "", err
	}
	return dir, nil
}

// agentsSkillsDir returns ~/.agents/skills. Unlike skillsDir this dir is NOT
// created: the ~/.agents skills tree is optional and, if absent or unreadable,
// is skipped silently so its absence never fails kernel startup.
func agentsSkillsDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".agents", "skills"), nil
}

// discoverSkills scans both ~/.toroid/skills (flat *.md files) and
// ~/.agents/skills (nested <name>/SKILL.md directories) for skill files and
// parses only their frontmatter (name, description) — never the full body — so
// startup cost stays proportional to the number of skills, not their combined
// size. A failure resolving the toroid dir is fatal (as before); an absent or
// unreadable ~/.agents/skills is skipped. When the same name appears in both
// sources the toroid dir wins (it is scanned first).
func discoverSkills() ([]SkillMeta, error) {
	toroidDir, err := skillsDir()
	if err != nil {
		return nil, err
	}
	dirs := []string{toroidDir}
	if agentsDir, err := agentsSkillsDir(); err == nil {
		dirs = append(dirs, agentsDir)
	}
	return discoverSkillsFrom(dirs), nil
}

// discoverSkillsFrom scans the given source dirs for skills, deduplicating by
// name with the first-listed dir winning. Both layouts are supported within a
// dir: flat *.md files (e.g. ~/.toroid/skills) and nested <name>/SKILL.md
// directories (e.g. ~/.agents/skills). Missing/unreadable dirs and malformed
// skill files are skipped silently.
func discoverSkillsFrom(dirs []string) []SkillMeta {
	var metas []SkillMeta
	seen := make(map[string]bool)
	for _, dir := range dirs {
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			var path string
			if e.IsDir() {
				path = filepath.Join(dir, e.Name(), "SKILL.md")
			} else if strings.HasSuffix(e.Name(), ".md") {
				path = filepath.Join(dir, e.Name())
			} else {
				continue // neither a flat *.md nor a nested skill dir
			}
			b, err := os.ReadFile(path)
			if err != nil {
				continue
			}
			name, description, ok := parseSkillFrontmatter(string(b))
			if !ok {
				continue
			}
			// Default the name from the file/dir name when the frontmatter
			// omits it (mirrors the existing flat-file behavior).
			if name == "" {
				if e.IsDir() {
					name = e.Name()
				} else {
					name = strings.TrimSuffix(e.Name(), ".md")
				}
			}
			if seen[name] {
				continue
			}
			seen[name] = true
			metas = append(metas, SkillMeta{Name: name, Description: description, Path: path})
		}
	}
	return metas
}

// parseSkillFrontmatter reads a leading "---\nkey: value\n...\n---" block and
// pulls out name/description. ok is false when there's no frontmatter block
// (or it's unterminated), in which case the file is skipped.
func parseSkillFrontmatter(content string) (name, description string, ok bool) {
	lines := strings.Split(content, "\n")
	if len(lines) == 0 || strings.TrimSpace(lines[0]) != "---" {
		return "", "", false
	}
	for _, line := range lines[1:] {
		trimmed := strings.TrimSpace(line)
		if trimmed == "---" {
			return name, description, true
		}
		key, val, found := strings.Cut(trimmed, ":")
		if !found {
			continue
		}
		switch strings.TrimSpace(key) {
		case "name":
			name = strings.TrimSpace(val)
		case "description":
			description = strings.TrimSpace(val)
		}
	}
	return "", "", false // unterminated frontmatter
}
