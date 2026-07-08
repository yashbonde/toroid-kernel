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

// discoverSkills scans ~/.toroid/skills/*.md for skill files and parses only
// their frontmatter (name, description) — never the full body — so startup
// cost stays proportional to the number of skills, not their combined size.
func discoverSkills() ([]SkillMeta, error) {
	dir, err := skillsDir()
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}

	var metas []SkillMeta
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		path := filepath.Join(dir, e.Name())
		b, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		name, description, ok := parseSkillFrontmatter(string(b))
		if !ok {
			continue
		}
		if name == "" {
			name = strings.TrimSuffix(e.Name(), ".md")
		}
		metas = append(metas, SkillMeta{Name: name, Description: description, Path: path})
	}
	return metas, nil
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
