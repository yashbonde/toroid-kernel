package toroid

import (
	"crypto/rand"
	"encoding/binary"
	"hash/fnv"
	"strconv"
	"sync"
	"time"

	oteltrace "go.opentelemetry.io/otel/trace"
)

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
