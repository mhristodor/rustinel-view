package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/redis/go-redis/v9"
)

// Redis key constants. Kept as a private namespace so callers never type
// raw key strings.
const (
	keyTimeline = "timeline"
	keyMeta     = "meta"
	keyNextA    = "next:a"
	keyNextE    = "next:e"

	prefixAlert = "a:"
	prefixEvent = "e:"
)

// Store is the only handle callers use to talk to Redis. It wraps a
// go-redis client and exposes typed read/write methods over the schema
// documented in docs/superpowers/specs/2026-05-28-rustinel-view-redesign-design.md.
//
// Reads are served from a lazily-materialized in-memory snapshot of the
// full decoded dataset: the snapshot is immutable after ingest, so a
// one-time decode beats re-fetching + re-JSON-decoding tens of
// thousands of records from Redis on every query (~200-400ms → ~5ms).
// Any write invalidates the cache; the next read rebuilds it. Redis
// remains the source of truth — the cache is a pure read-through
// materialization.
type Store struct {
	rdb  *redis.Client
	mu   sync.Mutex            // serializes cache rebuilds
	snap atomic.Pointer[[]Row] // time-ordered decoded rows; nil = stale
}

// New wraps an existing go-redis client.
func New(rdb *redis.Client) *Store { return &Store{rdb: rdb} }

// invalidate drops the materialized snapshot. Called by every write
// path so reads never observe stale data.
func (s *Store) invalidate() { s.snap.Store(nil) }

// materializedRows returns the decoded, time-ordered full dataset,
// rebuilding the cache if a write invalidated it. Double-checked
// locking keeps concurrent readers from racing duplicate rebuilds.
func (s *Store) materializedRows(ctx context.Context) ([]Row, error) {
	if p := s.snap.Load(); p != nil {
		return *p, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if p := s.snap.Load(); p != nil {
		return *p, nil
	}
	rows, err := s.allRowsInRange(ctx, "-inf", "+inf")
	if err != nil {
		return nil, err
	}
	s.snap.Store(&rows)
	return rows, nil
}

// Warm eagerly builds the snapshot cache. Called once after ingest so
// the first page request doesn't pay the materialization cost.
func (s *Store) Warm(ctx context.Context) (int, error) {
	rows, err := s.materializedRows(ctx)
	return len(rows), err
}

// FlushAll wipes the database. Called at startup so each viewer run is a
// fresh snapshot of the input files.
func (s *Store) FlushAll(ctx context.Context) error {
	s.invalidate()
	return s.rdb.FlushAll(ctx).Err()
}

// AddAlert writes a single alert. Used by tests and small ingest paths. For
// the production ingest path use NewBatch which pipelines hundreds of
// records per round-trip.
func (s *Store) AddAlert(ctx context.Context, a *Alert) error {
	s.invalidate()
	id, err := s.rdb.Incr(ctx, keyNextA).Result()
	if err != nil {
		return fmt.Errorf("incr next:a: %w", err)
	}
	a.ID = id
	body, err := json.Marshal(a)
	if err != nil {
		return fmt.Errorf("marshal alert: %w", err)
	}
	if err := s.rdb.Set(ctx, prefixAlert+strconv.FormatInt(id, 10), body, 0).Err(); err != nil {
		return fmt.Errorf("set alert: %w", err)
	}
	return s.rdb.ZAdd(ctx, keyTimeline, redis.Z{
		Score:  float64(a.Timestamp.UnixNano()),
		Member: prefixAlert + strconv.FormatInt(id, 10),
	}).Err()
}

// AddEvent mirrors AddAlert for events.
func (s *Store) AddEvent(ctx context.Context, e *Event) error {
	s.invalidate()
	id, err := s.rdb.Incr(ctx, keyNextE).Result()
	if err != nil {
		return fmt.Errorf("incr next:e: %w", err)
	}
	e.ID = id
	body, err := json.Marshal(e)
	if err != nil {
		return fmt.Errorf("marshal event: %w", err)
	}
	if err := s.rdb.Set(ctx, prefixEvent+strconv.FormatInt(id, 10), body, 0).Err(); err != nil {
		return fmt.Errorf("set event: %w", err)
	}
	return s.rdb.ZAdd(ctx, keyTimeline, redis.Z{
		Score:  float64(e.Timestamp.UnixNano()),
		Member: prefixEvent + strconv.FormatInt(id, 10),
	}).Err()
}

// GetAlert fetches a single alert by id, returning ErrNotFound if absent.
func (s *Store) GetAlert(ctx context.Context, id int64) (*Alert, error) {
	body, err := s.rdb.Get(ctx, prefixAlert+strconv.FormatInt(id, 10)).Bytes()
	if errors.Is(err, redis.Nil) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	var a Alert
	if err := json.Unmarshal(body, &a); err != nil {
		return nil, fmt.Errorf("unmarshal alert %d: %w", id, err)
	}
	return &a, nil
}

// GetEvent fetches a single event by id.
func (s *Store) GetEvent(ctx context.Context, id int64) (*Event, error) {
	body, err := s.rdb.Get(ctx, prefixEvent+strconv.FormatInt(id, 10)).Bytes()
	if errors.Is(err, redis.Nil) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	var e Event
	if err := json.Unmarshal(body, &e); err != nil {
		return nil, fmt.Errorf("unmarshal event %d: %w", id, err)
	}
	return &e, nil
}

// ErrNotFound is returned by Get* when the requested id is not present.
var ErrNotFound = errors.New("record not found")

// WriteMeta sets the meta hash. Called once at end of ingest.
func (s *Store) WriteMeta(ctx context.Context, m Meta) error {
	return s.rdb.HSet(ctx, keyMeta, map[string]any{
		"alerts_parsed":  m.AlertsParsed,
		"alerts_failed":  m.AlertsFailed,
		"events_parsed":  m.EventsParsed,
		"events_failed":  m.EventsFailed,
		"started_at":     m.StartedAt.UnixNano(),
		"finished_at":    m.FinishedAt.UnixNano(),
		"ingest_ms":      m.IngestMillis,
		"sigma_count":    m.SigmaCount,
		"yara_count":     m.YaraCount,
		"dns_count":      m.DnsCount,
		"file_count":     m.FileCount,
		"network_count":  m.NetworkCount,
		"process_count":  m.ProcessCount,
		"earliest_event": m.EarliestEvent.UnixNano(),
		"latest_event":   m.LatestEvent.UnixNano(),
		"top_rule":       m.TopRule,
		"top_rule_count": m.TopRuleCount,
	}).Err()
}

// ReadMeta reads the meta hash. Returns zero-value Meta if unset.
func (s *Store) ReadMeta(ctx context.Context) (Meta, error) {
	m, err := s.rdb.HGetAll(ctx, keyMeta).Result()
	if err != nil {
		return Meta{}, err
	}
	out := Meta{
		AlertsParsed: atoi(m["alerts_parsed"]),
		AlertsFailed: atoi(m["alerts_failed"]),
		EventsParsed: atoi(m["events_parsed"]),
		EventsFailed: atoi(m["events_failed"]),
		IngestMillis: atoi64(m["ingest_ms"]),
		SigmaCount:   atoi(m["sigma_count"]),
		YaraCount:    atoi(m["yara_count"]),
		DnsCount:     atoi(m["dns_count"]),
		FileCount:    atoi(m["file_count"]),
		NetworkCount: atoi(m["network_count"]),
		ProcessCount: atoi(m["process_count"]),
		TopRule:      m["top_rule"],
		TopRuleCount: atoi(m["top_rule_count"]),
	}
	if v, ok := m["started_at"]; ok {
		out.StartedAt = nanosToTime(atoi64(v))
	}
	if v, ok := m["finished_at"]; ok {
		out.FinishedAt = nanosToTime(atoi64(v))
	}
	if v, ok := m["earliest_event"]; ok {
		out.EarliestEvent = nanosToTime(atoi64(v))
	}
	if v, ok := m["latest_event"]; ok {
		out.LatestEvent = nanosToTime(atoi64(v))
	}
	return out, nil
}

func atoi(s string) int     { n, _ := strconv.Atoi(s); return n }
func atoi64(s string) int64 { n, _ := strconv.ParseInt(s, 10, 64); return n }

func nanosToTime(n int64) time.Time {
	if n == 0 {
		return time.Time{}
	}
	return time.Unix(0, n).UTC()
}
