package toroid

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	bolt "go.etcd.io/bbolt"
)

// bucket keys
var (
	bktTraces   = []byte("t")
	bktSpans    = []byte("s")
	bktMeta     = []byte("m")
	bktCosts    = []byte("c")
	bktEvents   = []byte("e")
	bktMemories = []byte("mem")
)

// TraceMeta is stored per trace (root kernel run).
type TraceMeta struct {
	TraceID   string `json:"trace_id"`
	Title     string `json:"title,omitempty"`
	StartedAt int64  `json:"started_at"` // UnixNano
	EndedAt   int64  `json:"ended_at,omitempty"`
}

// SpanMeta is stored per span (kernel session, including subagents).
type SpanMeta struct {
	SpanID       string `json:"span_id"`
	TraceID      string `json:"trace_id"`
	ParentSpanID string `json:"parent_span_id,omitempty"`
	Model        string `json:"model,omitempty"`
	Title        string `json:"title,omitempty"`
	StartedAt    int64  `json:"started_at"` // UnixNano
	EndedAt      int64  `json:"ended_at,omitempty"`
}

// Store wraps a bbolt database for all persistence needs.
type Store struct {
	db *bolt.DB
}

var (
	dbMu       sync.Mutex
	defaultDB  *bolt.DB
)

func openDefaultDB() (*bolt.DB, error) {
	dbMu.Lock()
	defer dbMu.Unlock()

	if defaultDB != nil {
		return defaultDB, nil
	}

	path, err := BboltPath()
	if err != nil {
		return nil, err
	}
	db, err := bolt.Open(path, 0600, &bolt.Options{Timeout: 1 * time.Second})
	if err != nil {
		return nil, fmt.Errorf("cannot open %s (another swb process may be running): %w", path, err)
	}
	defaultDB = db
	return defaultDB, nil
}

// NewStore opens (or reuses) the singleton bbolt database (~/.swarmbuddy/traces.bbolt.db).
func NewStore() (*Store, error) {
	db, err := openDefaultDB()
	if err != nil {
		return nil, err
	}
	return &Store{db: db}, nil
}

// NewStoreReadWrite is an alias for NewStore. Both CLI and server share one file.
// Only one process should hold the lock at a time.
func NewStoreReadWrite() (*Store, error) {
	return NewStore()
}

// UpdateTraceTitle patches only the title field of an existing TraceMeta, preserving StartedAt.
func (s *Store) UpdateTraceTitle(traceID, title string) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		tb, err := tx.CreateBucketIfNotExists(bktTraces)
		if err != nil {
			return err
		}
		trb, err := tb.CreateBucketIfNotExists([]byte(traceID))
		if err != nil {
			return err
		}
		var meta TraceMeta
		if b := trb.Get(bktMeta); b != nil {
			_ = json.Unmarshal(b, &meta)
		}
		meta.TraceID = traceID
		meta.Title = title
		b, err := json.Marshal(meta)
		if err != nil {
			return err
		}
		return trb.Put(bktMeta, b)
	})
}

// SaveTraceMeta writes or updates trace metadata.
func (s *Store) SaveTraceMeta(meta TraceMeta) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		tb, err := tx.CreateBucketIfNotExists(bktTraces)
		if err != nil {
			return err
		}
		trb, err := tb.CreateBucketIfNotExists([]byte(meta.TraceID))
		if err != nil {
			return err
		}
		b, err := json.Marshal(meta)
		if err != nil {
			return err
		}
		return trb.Put(bktMeta, b)
	})
}

// LoadTraceMeta reads trace metadata by trace ID.
func (s *Store) LoadTraceMeta(traceID string) (TraceMeta, error) {
	var meta TraceMeta
	err := s.db.View(func(tx *bolt.Tx) error {
		tb := tx.Bucket(bktTraces)
		if tb == nil {
			return nil
		}
		trb := tb.Bucket([]byte(traceID))
		if trb == nil {
			return nil
		}
		b := trb.Get(bktMeta)
		if b == nil {
			return nil
		}
		return json.Unmarshal(b, &meta)
	})
	return meta, err
}

// SaveSpanMeta writes or updates span metadata.
func (s *Store) SaveSpanMeta(meta SpanMeta) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		tb, err := tx.CreateBucketIfNotExists(bktTraces)
		if err != nil {
			return err
		}
		trb, err := tb.CreateBucketIfNotExists([]byte(meta.TraceID))
		if err != nil {
			return err
		}
		sb, err := trb.CreateBucketIfNotExists(bktSpans)
		if err != nil {
			return err
		}
		spb, err := sb.CreateBucketIfNotExists([]byte(meta.SpanID))
		if err != nil {
			return err
		}
		b, err := json.Marshal(meta)
		if err != nil {
			return err
		}
		return spb.Put(bktMeta, b)
	})
}

// LoadLastTotalUSD returns the total_usd from the most recent cost entry for a span, or 0 if none.
func (s *Store) LoadLastTotalUSD(traceID, spanID string) float64 {
	var last float64
	_ = s.db.View(func(tx *bolt.Tx) error {
		tb := tx.Bucket(bktTraces)
		if tb == nil {
			return nil
		}
		trb := tb.Bucket([]byte(traceID))
		if trb == nil {
			return nil
		}
		sb := trb.Bucket(bktSpans)
		if sb == nil {
			return nil
		}
		spb := sb.Bucket([]byte(spanID))
		if spb == nil {
			return nil
		}
		cb := spb.Bucket(bktCosts)
		if cb == nil {
			return nil
		}
		// ForEach visits in key order (ascending time); last value wins
		return cb.ForEach(func(k, v []byte) error {
			var rec map[string]float64
			if err := json.Unmarshal(v, &rec); err == nil {
				last = rec["total_usd"]
			}
			return nil
		})
	})
	return last
}

// AppendCost records a turn cost under a span's cost bucket.
func (s *Store) AppendCost(traceID, spanID string, turnUSD, totalUSD float64) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		tb, err := tx.CreateBucketIfNotExists(bktTraces)
		if err != nil {
			return err
		}
		trb, err := tb.CreateBucketIfNotExists([]byte(traceID))
		if err != nil {
			return err
		}
		sb, err := trb.CreateBucketIfNotExists(bktSpans)
		if err != nil {
			return err
		}
		spb, err := sb.CreateBucketIfNotExists([]byte(spanID))
		if err != nil {
			return err
		}
		cb, err := spb.CreateBucketIfNotExists(bktCosts)
		if err != nil {
			return err
		}
		// key = 8-byte big-endian UnixNano for natural ordering
		var key [8]byte
		binary.BigEndian.PutUint64(key[:], uint64(time.Now().UnixNano()))
		val, err := json.Marshal(map[string]float64{"turn_usd": turnUSD, "total_usd": totalUSD})
		if err != nil {
			return err
		}
		return cb.Put(key[:], val)
	})
}

// AppendEvent records a session event under a span's event bucket.
func (s *Store) AppendEvent(traceID, spanID string, event Event) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		tb, err := tx.CreateBucketIfNotExists(bktTraces)
		if err != nil {
			return err
		}
		trb, err := tb.CreateBucketIfNotExists([]byte(traceID))
		if err != nil {
			return err
		}
		sb, err := trb.CreateBucketIfNotExists(bktSpans)
		if err != nil {
			return err
		}
		spb, err := sb.CreateBucketIfNotExists([]byte(spanID))
		if err != nil {
			return err
		}
		eb, err := spb.CreateBucketIfNotExists(bktEvents)
		if err != nil {
			return err
		}
		// key = 8nd big-endian UnixNano for natural ordering
		var key [8]byte
		binary.BigEndian.PutUint64(key[:], uint64(event.EmitTS))
		val, err := json.Marshal(event)
		if err != nil {
			return err
		}
		return eb.Put(key[:], val)
	})
}

// LoadTraceTotal returns the sum of the last total_usd across all spans for a trace.
// This represents the cumulative cost of all previous runs under this trace ID.
func (s *Store) LoadTraceTotal(traceID string) float64 {
	var total float64
	_ = s.db.View(func(tx *bolt.Tx) error {
		tb := tx.Bucket(bktTraces)
		if tb == nil {
			return nil
		}
		trb := tb.Bucket([]byte(traceID))
		if trb == nil {
			return nil
		}
		sb := trb.Bucket(bktSpans)
		if sb == nil {
			return nil
		}
		return sb.ForEach(func(spanKey, _ []byte) error {
			spb := sb.Bucket(spanKey)
			if spb == nil {
				return nil
			}
			cb := spb.Bucket(bktCosts)
			if cb == nil {
				return nil
			}
			// last key has highest time — walk to find it
			var lastTotal float64
			_ = cb.ForEach(func(k, v []byte) error {
				var rec map[string]float64
				if err := json.Unmarshal(v, &rec); err == nil {
					lastTotal = rec["total_usd"]
				}
				return nil
			})
			total += lastTotal
			return nil
		})
	})
	return total
}

// SaveMemories writes the agent's persistent memory JSON blob for a span.
func (s *Store) SaveMemories(spanID string, mem map[string]any) error {
	b, err := json.MarshalIndent(mem, "", "  ")
	if err != nil {
		return err
	}
	return s.db.Update(func(tx *bolt.Tx) error {
		mb, err := tx.CreateBucketIfNotExists(bktMemories)
		if err != nil {
			return err
		}
		return mb.Put([]byte(spanID), b)
	})
}

// LoadMemories reads the agent's persistent memory JSON blob for a span.
func (s *Store) LoadMemories(spanID string) (map[string]any, error) {
	var mem map[string]any
	err := s.db.View(func(tx *bolt.Tx) error {
		mb := tx.Bucket(bktMemories)
		if mb == nil {
			return nil
		}
		b := mb.Get([]byte(spanID))
		if b == nil {
			return nil
		}
		return json.Unmarshal(b, &mem)
	})
	if mem == nil {
		mem = map[string]any{}
	}
	return mem, err
}

// CostEvent is a single turn cost record stored under a span.
type CostEvent struct {
	TS       int64   `json:"ts"` // UnixNano (from bucket key)
	TurnUSD  float64 `json:"turn_usd"`
	TotalUSD float64 `json:"total_usd"`
}

// SpanData is a span with its cost events and session events, used for visualization.
type SpanData struct {
	SpanMeta
	Costs  []CostEvent `json:"costs"`
	Events []Event     `json:"events"`
}

// TraceData is the full trace for visualization.
type TraceData struct {
	Trace TraceMeta  `json:"trace"`
	Spans []SpanData `json:"spans"`
}

// LoadTraceData reads the full trace + all spans + costs for a given trace ID.
func LoadTraceData(traceID string) (TraceData, error) {
	db, err := openDefaultDB()
	if err != nil {
		return TraceData{}, err
	}
	var td TraceData
	err = db.View(func(tx *bolt.Tx) error {
		tb := tx.Bucket(bktTraces)
		if tb == nil {
			return nil
		}
		trb := tb.Bucket([]byte(traceID))
		if trb == nil {
			return nil
		}
		// trace meta
		if b := trb.Get(bktMeta); b != nil {
			_ = json.Unmarshal(b, &td.Trace)
		}
		// spans
		sb := trb.Bucket(bktSpans)
		if sb == nil {
			return nil
		}
		return sb.ForEach(func(spanKey, v []byte) error {
			if v != nil {
				return nil // skip non-bucket
			}
			spb := sb.Bucket(spanKey)
			if spb == nil {
				return nil
			}
			var sd SpanData
			if b := spb.Get(bktMeta); b != nil {
				_ = json.Unmarshal(b, &sd.SpanMeta)
			}
			// costs
			cb := spb.Bucket(bktCosts)
			if cb != nil {
				_ = cb.ForEach(func(k, v []byte) error {
					ts := int64(binary.BigEndian.Uint64(k))
					var rec map[string]float64
					if err := json.Unmarshal(v, &rec); err == nil {
						sd.Costs = append(sd.Costs, CostEvent{
							TS:       ts,
							TurnUSD:  rec["turn_usd"],
							TotalUSD: rec["total_usd"],
						})
					}
					return nil
				})
			}
			// events
			eb := spb.Bucket(bktEvents)
			if eb != nil {
				_ = eb.ForEach(func(k, v []byte) error {
					var ev Event
					if err := json.Unmarshal(v, &ev); err == nil {
						sd.Events = append(sd.Events, ev)
					}
					return nil
				})
			}
			td.Spans = append(td.Spans, sd)
			return nil
		})
	})
	return td, err
}

// SessionInfo holds metadata for listing traces/sessions.
type SessionInfo struct {
	ID          string // trace ID (root span ID)
	Title       string
	StartedAt   int64   // UnixNano
	DurationNs  int64   // last cost event ts - started_at (wall time)
	AgentTimeNs int64   // wall time minus total tool execution time
	TotalUSD    float64 // sum of all turn_usd across all spans
}

func (s SessionInfo) StartedAtFmt() string {
	return time.Unix(0, s.StartedAt).Format("Jan 02, 15:04:05")
}

func fmtDuration(ns int64) string {
	if ns <= 0 {
		return "—"
	}
	d := time.Duration(ns)
	if d < time.Second {
		return fmt.Sprintf("%dms", d.Milliseconds())
	}
	return fmt.Sprintf("%.1fs", d.Seconds())
}

func (s SessionInfo) DurationFmt() string  { return fmtDuration(s.DurationNs) }
func (s SessionInfo) AgentTimeFmt() string { return fmtDuration(s.AgentTimeNs) }

// listSessionsFromDB reads all SessionInfo entries from one bbolt handle.
func listSessionsFromDB(db *bolt.DB) ([]SessionInfo, error) {
	var infos []SessionInfo
	err := db.View(func(tx *bolt.Tx) error {
		tb := tx.Bucket(bktTraces)
		if tb == nil {
			return nil
		}
		return tb.ForEach(func(k, v []byte) error {
			if v != nil {
				return nil // skip non-bucket entries
			}
			trb := tb.Bucket(k)
			if trb == nil {
				return nil
			}
			b := trb.Get(bktMeta)
			if b == nil {
				return nil
			}
			var meta TraceMeta
			if err := json.Unmarshal(b, &meta); err != nil {
				return nil
			}
			title := meta.Title
			if title == "" {
				title = "(no title)"
			}
			info := SessionInfo{
				ID:        meta.TraceID,
				Title:     title,
				StartedAt: meta.StartedAt,
			}
			// walk all spans to accumulate cost, wall time, and tool execution time
			var totalToolNs int64
			if sb := trb.Bucket(bktSpans); sb != nil {
				_ = sb.ForEach(func(spanKey, sv []byte) error {
					if sv != nil {
						return nil
					}
					spb := sb.Bucket(spanKey)
					if spb == nil {
						return nil
					}
					// cost events → TotalUSD + DurationNs
					cb := spb.Bucket(bktCosts)
					if cb != nil {
						_ = cb.ForEach(func(ck, cv []byte) error {
							ts := int64(binary.BigEndian.Uint64(ck))
							var rec map[string]float64
							if err := json.Unmarshal(cv, &rec); err == nil {
								info.TotalUSD += rec["turn_usd"]
								if d := ts - meta.StartedAt; d > info.DurationNs {
									info.DurationNs = d
								}
							}
							return nil
						})
					}
					// event bucket → sum step durations for agent time.
					// Each step: UserPromptSubmit (or prev TurnCost) → TurnCost.
					// This includes both LLM and tool time within a step, but
					// excludes idle time between resumed runs.
					eb := spb.Bucket(bktEvents)
					if eb != nil {
						var stepStart int64 // EmitTS of run start or previous step end
						_ = eb.ForEach(func(_, ev []byte) error {
							var e Event
							if err := json.Unmarshal(ev, &e); err != nil {
								return nil
							}
							switch e.Kind {
							case EventUserPromptSubmit:
								stepStart = e.EmitTS
							case EventTurnCost:
								if stepStart > 0 {
									totalToolNs += e.EmitTS - stepStart
								}
								stepStart = e.EmitTS
							}
							return nil
						})
					}
					return nil
				})
			}
			if totalToolNs > 0 {
				info.AgentTimeNs = totalToolNs
			}
			infos = append(infos, info)
			return nil
		})
	})
	if err != nil {
		return nil, err
	}
	return infos, nil
}

// ListSessions returns all traces sorted newest first.
func ListSessions() ([]SessionInfo, error) {
	db, err := openDefaultDB()
	if err != nil {
		return nil, err
	}
	infos, err := listSessionsFromDB(db)
	if err != nil {
		return nil, err
	}
	// sort newest-first (IDs are lexicographically monotonic)
	for i, j := 0, len(infos)-1; i < j; i, j = i+1, j-1 {
		infos[i], infos[j] = infos[j], infos[i]
	}
	return infos, nil
}

// DeleteSession removes all data associated with a trace ID.
func DeleteSession(id string) error {
	db, err := openDefaultDB()
	if err != nil {
		return err
	}
	return db.Update(func(tx *bolt.Tx) error {
		tb := tx.Bucket(bktTraces)
		if tb != nil {
			_ = tb.DeleteBucket([]byte(id))
		}
		mb := tx.Bucket(bktMemories)
		if mb != nil {
			_ = mb.Delete([]byte(id))
		}
		return nil
	})
}
