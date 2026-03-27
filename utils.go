// Utils

package toroid

import (
	"context"
	"crypto/rand"
	"embed"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"time"

	tsize "github.com/kopoli/go-terminal-size"
)

//go:embed assets/*.json
var assetsFS embed.FS

//go:embed prompts/*.tmpl prompts/*.txt
var promptFS embed.FS

// NewSessionID generates a monotonic, human-readable session ID.
// Format: <unix_seconds>-<4char_random>
func NewSessionID() string {
	const charset = "abcdefghijklmnopqrstuvwxyz0123456789"
	const randLen = 4

	now := time.Now().Unix()

	b := make([]byte, randLen)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("%d", now)
	}

	for i := range b {
		b[i] = charset[int(b[i])%len(charset)]
	}

	return fmt.Sprintf("%d-%s", now, string(b))
}

// readPrompt loads a prompt file from ~/.swarmbuddy/prompts/<name> if present,
// falling back to the embedded copy. This allows prompt updates without recompiling.
func readPrompt(name string) ([]byte, error) {
	if dir, err := swarmbuddyHome(); err == nil {
		if b, err := os.ReadFile(filepath.Join(dir, "prompts", name)); err == nil {
			return b, nil
		}
	}
	return promptFS.ReadFile("prompts/" + name)
}

func readAssets(name string) ([]byte, error) {
	if dir, err := swarmbuddyHome(); err == nil {
		if b, err := os.ReadFile(filepath.Join(dir, "assets", name)); err == nil {
			return b, nil
		}
	}
	return assetsFS.ReadFile("assets/" + name)
}

const (
	colorReset  = "\033[0m"
	colorRed    = "\033[31m"
	colorGreen  = "\033[32m"
	colorYellow = "\033[33m"
	colorBlue   = "\033[34m"
	colorCyan   = "\033[36m"
	colorGray   = "\033[90m"
)

var (
	width    int
	logWidth int
)

// "YY/MM/DD HH:MM:SS [LEVL]" = 25 chars
const logPrefix = 25

func init() {
	size, _ := tsize.GetSize()
	width = size.Width
	logWidth = width - logPrefix
	if logWidth < 40 {
		logWidth = 120 // fallback for non-tty (background process, server)
	}
}

// takes the string and adds new lines in places that would exceed the terminal width
func wrapInLogWidth(x string) string {
	indent := strings.Repeat(" ", logPrefix)
	var b strings.Builder
	lines := strings.Split(
		strings.TrimSpace(strings.ReplaceAll(strings.ReplaceAll(x, "\r", ""), "\n", "\\n")),
		"\n",
	)
	for i, line := range lines {
		if i == len(lines)-1 && line == "" {
			break
		}
		if i > 0 {
			b.WriteString(indent)
		}
		if len(line) > logWidth {
			for j := 0; j < len(line); j += logWidth {
				chunk := line[j:min(j+logWidth, len(line))]
				if j > 0 {
					b.WriteString(indent)
				}
				b.WriteString(chunk)
				b.WriteByte('\n')
			}
		} else {
			b.WriteString(line)
			b.WriteByte('\n')
		}
	}
	return b.String()
}

func logLine(level, color, msg string) {
	slog.Log(context.Background(), slog.LevelInfo, fmt.Sprintf("%s", wrapInLogWidth(msg)))
}

func LogInfo(msg string, args ...any) {
	logLine("INFO", colorCyan, fmt.Sprintf(msg, args...))
}

func LogError(msg string, args ...any) {
	logLine("ERRO", colorRed, fmt.Sprintf(msg, args...))
}

func LogDebug(msg string, args ...any) {
	logLine("DBUG", colorGray, fmt.Sprintf(msg, args...))
}

func PrettyPrintHistory(kernel *Kernel) {
	LogInfo("Printing History: %d (tokens: %d)", len(kernel.history), kernel.currentTokens)
	var b strings.Builder
	indent := strings.Repeat(" ", logPrefix)
	for _, msg := range kernel.history {
		message := indent + colorGray + string(msg.Role) + colorReset + ": " + strings.ReplaceAll(fmt.Sprintf("%v", msg.Content), "\n", "\\n")
		if len(message) > logWidth {
			message = message[:logWidth-3] + "..."
		}
		b.WriteString(fmt.Sprintf("%s\n", message))
	}
	fmt.Fprint(os.Stdout, b.String())
}

func ApplyDefaults(cfg any) {
	v := reflect.ValueOf(cfg).Elem()
	t := v.Type()

	for i := 0; i < v.NumField(); i++ {
		field := v.Field(i)
		fieldType := t.Field(i)
		defaultTag := fieldType.Tag.Get("default")

		// skip if no default tag
		if defaultTag == "" {
			continue
		}

		// check if field is zero
		isZero := false
		switch field.Kind() {
		case reflect.String:
			isZero = field.String() == ""
		case reflect.Int, reflect.Int64:
			isZero = field.Int() == 0
		case reflect.Bool:
			// bool zero is false, but we can't easily distinguish "false" from "not set"
			// unless it's a pointer.
			isZero = !field.Bool()
		case reflect.Ptr:
			isZero = field.IsNil()
		}

		if isZero {
			switch field.Kind() {
			case reflect.String:
				field.SetString(defaultTag)
			case reflect.Int, reflect.Int64:
				val, _ := strconv.ParseInt(defaultTag, 10, 64)
				field.SetInt(val)
			case reflect.Bool:
				val, _ := strconv.ParseBool(defaultTag)
				field.SetBool(val)
			case reflect.Ptr:
				if field.Type().Elem().Kind() == reflect.Bool {
					val, _ := strconv.ParseBool(defaultTag)
					field.Set(reflect.ValueOf(&val))
				}
			}
		}
	}
}

// swarmbuddyHome returns ~/.swarmbuddy, creating it if needed.
func swarmbuddyHome() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(home, ".swarmbuddy")
	if err := os.MkdirAll(dir, 0755); err != nil {
		return "", err
	}
	return dir, nil
}

// BboltPath returns ~/.swarmbuddy/traces.bbolt.db
func BboltPath() (string, error) {
	dir, err := swarmbuddyHome()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "traces.bbolt.db"), nil
}

// SqlitePath returns ~/.swarmbuddy/sql.db
func SqlitePath() (string, error) {
	dir, err := swarmbuddyHome()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "sql.db"), nil
}

// StorageDir returns ~/.swarmbuddy/storage/, creating it if needed.
func StorageDir() (string, error) {
	dir, err := swarmbuddyHome()
	if err != nil {
		return "", err
	}
	p := filepath.Join(dir, "storage")
	if err := os.MkdirAll(p, 0755); err != nil {
		return "", err
	}
	return p, nil
}

// TracesDir returns ~/.swarmbuddy/traces/, creating it if needed.
func TracesDir() (string, error) {
	dir, err := swarmbuddyHome()
	if err != nil {
		return "", err
	}
	p := filepath.Join(dir, "traces")
	if err := os.MkdirAll(p, 0755); err != nil {
		return "", err
	}
	return p, nil
}

// ConfigPath returns ~/.swarmbuddy/config.json
func ConfigPath() (string, error) {
	dir, err := swarmbuddyHome()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "config.json"), nil
}

// RunnerDir returns {cwd}/.swarmbuddy_tmp/{traceID}/, creating it if needed.
func RunnerDir(cwd, traceID string) (string, error) {
	p := filepath.Join(cwd, ".swarmbuddy_tmp", traceID)
	if err := os.MkdirAll(p, 0755); err != nil {
		return "", err
	}
	return p, nil
}
