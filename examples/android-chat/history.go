package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/yashbonde/toroid-kernel/llm"
)

// Saved chats: one JSON file per chat at <data>/chats/<id>.json, rewritten
// atomically after every reply. They hold the plain text the user saw (no
// reasoning), which is enough to show the chat again and to rebuild a kernel's
// history when a chat is resumed after a restart.

// maxTitleRunes bounds a chat's title, taken from its first message.
const maxTitleRunes = 60

// chatIDPattern is every id this server accepts. Ids name files, so nothing
// that could step outside the chats dir gets through.
var chatIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

func validChatID(id string) bool { return chatIDPattern.MatchString(id) }

// chatMessage is one turn. Tools, Model and CostUSD are set on assistant
// turns; Tools has one summary line per tool call ("✓ added: Buy milk · Sat
// 14 Nov"), for showing the chat again. They are not replayed to the model.
type chatMessage struct {
	Role    string   `json:"role"` // "user" or "assistant"
	Text    string   `json:"text"`
	Tools   []string `json:"tools,omitempty"`
	Model   string   `json:"model,omitempty"`
	At      string   `json:"at"` // RFC 3339, local time
	CostUSD float64  `json:"cost_usd,omitempty"`
}

// chatRecord is a saved chat, as stored on disk and served by /api/chats/{id}.
type chatRecord struct {
	ID        string        `json:"id"`
	Title     string        `json:"title"`
	CreatedAt string        `json:"created_at"`
	UpdatedAt string        `json:"updated_at"`
	Model     string        `json:"model"`    // the model of the latest reply
	CostUSD   float64       `json:"cost_usd"` // the chat's total spend
	Messages  []chatMessage `json:"messages"`
}

// chatSummary is one row of /api/history. Day is the local calendar date the
// chat started on, so the UI can bucket chats without parsing time zones.
type chatSummary struct {
	ID           string  `json:"id"`
	Title        string  `json:"title"`
	CreatedAt    string  `json:"created_at"`
	UpdatedAt    string  `json:"updated_at"`
	Day          string  `json:"day"` // YYYY-MM-DD in localZone
	Model        string  `json:"model"`
	CostUSD      float64 `json:"cost_usd"`
	MessageCount int     `json:"message_count"`
}

// localZone is the zone chats are stamped and bucketed in. A function so tests
// can swap it without writing the global time.Local, which the runtime reads
// from other goroutines.
var localZone = func() *time.Location { return time.Local }

func stamp(t time.Time) string { return t.In(localZone()).Format(time.RFC3339) }

// chatTitle is text on one line, cut to maxTitleRunes on a rune boundary.
func chatTitle(text string) string {
	t := strings.Join(strings.Fields(text), " ")
	if utf8.RuneCountInString(t) <= maxTitleRunes {
		return t
	}
	r := []rune(t)
	return strings.TrimRight(string(r[:maxTitleRunes]), " ") + "…"
}

// add appends m, starting the chat's title and creation time on its first
// message.
func (c *chatRecord) add(m chatMessage) {
	if len(c.Messages) == 0 {
		c.CreatedAt = m.At
	}
	if c.Title == "" && m.Role == "user" {
		c.Title = chatTitle(m.Text)
	}
	c.Messages = append(c.Messages, m)
	c.UpdatedAt = m.At
}

// history rebuilds a kernel conversation from the saved turns. Turns of the
// same role in a row (a message whose reply failed, then a retry) are merged,
// so the rebuilt history always alternates.
func (c *chatRecord) history() []llm.Message {
	var out []llm.Message
	var role, text string
	flush := func() {
		switch role {
		case "user":
			out = append(out, llm.NewUserMessage(text))
		case "assistant":
			out = append(out, llm.NewAssistantText(text))
		}
	}
	for _, m := range c.Messages {
		if m.Text == "" || (m.Role != "user" && m.Role != "assistant") {
			continue
		}
		if m.Role == role {
			text += "\n\n" + m.Text
			continue
		}
		flush()
		role, text = m.Role, m.Text
	}
	flush()
	return out
}

func (c *chatRecord) summary() chatSummary {
	return chatSummary{
		ID: c.ID, Title: c.Title, CreatedAt: c.CreatedAt, UpdatedAt: c.UpdatedAt,
		Model: c.Model, CostUSD: c.CostUSD, MessageCount: len(c.Messages),
	}
}

// chatStore reads and writes the chats dir. list caches each file's summary
// by modification time and size, so opening the history view does not re-read
// every chat, yet files added or removed behind its back still show up.
type chatStore struct {
	dir string

	mu    sync.Mutex
	cache map[string]cachedSummary // by chat id
}

type cachedSummary struct {
	mod     time.Time
	size    int64
	created time.Time
	sum     chatSummary
}

func newChatStore(dir string) *chatStore {
	return &chatStore{dir: dir, cache: map[string]cachedSummary{}}
}

func (st *chatStore) path(id string) (string, error) {
	if !validChatID(id) {
		return "", fmt.Errorf("bad chat id")
	}
	return filepath.Join(st.dir, id+".json"), nil
}

// save writes rec, mode 600. A chat with no messages is never written.
func (st *chatStore) save(rec *chatRecord) error {
	if len(rec.Messages) == 0 {
		return nil
	}
	p, err := st.path(rec.ID)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(st.dir, 0o700); err != nil {
		return err
	}
	b, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	return writeFileAtomic(p, b, 0o600)
}

// load reads one chat; a missing chat is an error matching fs.ErrNotExist.
func (st *chatStore) load(id string) (*chatRecord, error) {
	p, err := st.path(id)
	if err != nil {
		return nil, err
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return nil, err
	}
	var rec chatRecord
	if err := json.Unmarshal(b, &rec); err != nil {
		return nil, fmt.Errorf("chat %s: %w", id, err)
	}
	rec.ID = id // the file name is the id
	return &rec, nil
}

func (st *chatStore) remove(id string) error {
	p, err := st.path(id)
	if err != nil {
		return err
	}
	return os.Remove(p)
}

// list returns every saved chat, newest first by creation time.
func (st *chatStore) list() ([]chatSummary, error) {
	entries, err := os.ReadDir(st.dir)
	if errors.Is(err, fs.ErrNotExist) {
		return []chatSummary{}, nil
	}
	if err != nil {
		return nil, err
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	next := make(map[string]cachedSummary, len(entries))
	for _, e := range entries {
		id, ok := strings.CutSuffix(e.Name(), ".json")
		if !ok || !e.Type().IsRegular() || !validChatID(id) {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue // removed since ReadDir
		}
		c, hit := st.cache[id]
		if !hit || !c.mod.Equal(info.ModTime()) || c.size != info.Size() {
			rec, err := st.load(id)
			if err != nil {
				log.Printf("history: skipping %s: %v", e.Name(), err)
				continue
			}
			created, _ := time.Parse(time.RFC3339, rec.CreatedAt)
			c = cachedSummary{mod: info.ModTime(), size: info.Size(), created: created, sum: rec.summary()}
		}
		next[id] = c
	}
	st.cache = next

	all := make([]cachedSummary, 0, len(next))
	for _, c := range next {
		all = append(all, c)
	}
	sort.Slice(all, func(i, j int) bool {
		if !all[i].created.Equal(all[j].created) {
			return all[i].created.After(all[j].created)
		}
		return all[i].sum.ID > all[j].sum.ID
	})
	out := make([]chatSummary, len(all))
	for i, c := range all {
		out[i] = c.sum
		out[i].Day = c.created.In(localZone()).Format(time.DateOnly)
	}
	return out, nil
}
