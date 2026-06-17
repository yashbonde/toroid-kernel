// Utils

package toroid

import (
	"context"
	"crypto/rand"
	"embed"
	"encoding/binary"
	"fmt"
	"hash/fnv"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"time"

	tsize "github.com/kopoli/go-terminal-size"
	oteltrace "go.opentelemetry.io/otel/trace"
)

//go:embed assets/*.json
var assetsFS embed.FS

//go:embed prompts/*.tmpl prompts/*.txt
var promptFS embed.FS

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

// Snowflake IDs ---------------------------------------------------------------
//
// Span IDs are Snowflake IDs: a 64-bit value of
//
//	[ 42 bits ms-since-epoch | 12 bits node | 10 bits sequence ]
//
// They are time-ordered (k-sortable), which keeps the SQLite indices in store.go
// cheap and gives spans a free chronological ordering. A Snowflake is exactly
// 64 bits — the size of an OpenTelemetry span ID — and forms the high half of the
// 128-bit OTEL trace ID (see OTELIDs). See the architecture doc §12.2.

// snowflakeEpoch is 2024-01-01T00:00:00Z in milliseconds. Keeping a recent epoch
// maximises the usable lifetime of the 42-bit timestamp field.
const snowflakeEpoch int64 = 1704067200000

type snowflakeGen struct {
	mu       sync.Mutex
	node     int64 // 12 bits
	lastMS   int64
	sequence int64 // 10 bits
}

var defaultSnowflake = newSnowflakeGen()

func newSnowflakeGen() *snowflakeGen {
	var b [2]byte
	_, _ = rand.Read(b[:])
	node := int64(binary.BigEndian.Uint16(b[:])) & 0xFFF // 12 bits
	return &snowflakeGen{node: node}
}

// next returns the next monotonically increasing Snowflake ID.
func (g *snowflakeGen) next() int64 {
	g.mu.Lock()
	defer g.mu.Unlock()

	ms := time.Now().UnixMilli() - snowflakeEpoch
	if ms == g.lastMS {
		g.sequence = (g.sequence + 1) & 0x3FF // 10 bits
		if g.sequence == 0 {
			// sequence exhausted this ms — spin to the next millisecond
			for ms <= g.lastMS {
				ms = time.Now().UnixMilli() - snowflakeEpoch
			}
		}
	} else {
		g.sequence = 0
	}
	g.lastMS = ms

	return (ms << 22) | (g.node << 10) | g.sequence
}

// NewSnowflake returns a fresh time-ordered 64-bit Snowflake ID.
func NewSnowflake() int64 { return defaultSnowflake.next() }

// NewSessionID generates a unique, time-ordered session/span ID.
//
// The ID is the zero-padded 16-character hex encoding of a Snowflake, so it
// remains lexicographically monotonic (existing call sites that sort IDs as
// strings keep working) and converts losslessly to an OpenTelemetry span ID.
func NewSessionID() string {
	return strconv.FormatInt(NewSnowflake()|(1<<62), 16) // set bit 62 so it is always 16 hex chars and positive
}

// snowflakeFromID recovers the 64-bit value encoded by NewSessionID. It returns
// (value, true) for Snowflake-formatted IDs and (hash, false) for any other
// string (e.g. legacy IDs), so callers always get a usable 64-bit value.
func snowflakeFromID(id string) (uint64, bool) {
	if v, err := strconv.ParseUint(id, 16, 64); err == nil {
		return v, true
	}
	h := fnv.New64a()
	_, _ = h.Write([]byte(id))
	return h.Sum64(), false
}

// OTEL mapping ----------------------------------------------------------------

// OTELIDs maps a kernel (traceID, spanID) pair to OpenTelemetry IDs.
//
//   - The span ID is the 64-bit Snowflake encoded big-endian (8 bytes).
//   - The trace ID is 128 bits: the trace's Snowflake high half plus a
//     deterministic FNV-derived low half, so the same traceID always yields the
//     same OTEL trace ID (stable across processes and resumes).
func OTELIDs(traceID, spanID string) (oteltrace.TraceID, oteltrace.SpanID) {
	var tid oteltrace.TraceID
	var sid oteltrace.SpanID

	spanVal, _ := snowflakeFromID(spanID)
	binary.BigEndian.PutUint64(sid[:], spanVal)

	traceVal, _ := snowflakeFromID(traceID)
	binary.BigEndian.PutUint64(tid[0:8], traceVal)
	// low half: FNV hash of the trace string keeps it deterministic and distinct.
	h := fnv.New64a()
	_, _ = h.Write([]byte(traceID))
	binary.BigEndian.PutUint64(tid[8:16], h.Sum64())

	return tid, sid
}
