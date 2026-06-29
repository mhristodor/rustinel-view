package store

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"

	"github.com/redis/go-redis/v9"
)

// Default flush threshold for pipelined batches. 500 records per flush is a
// sweet spot on loopback: large enough that per-op overhead disappears,
// small enough that a flush completes in single-digit ms and doesn't
// monopolize the client's send buffer.
const batchFlushSize = 500

// Batch accumulates writes locally and flushes them to Redis in pipelined
// chunks. Required for ingesting large event files in reasonable time;
// serial writes take ~10x longer over loopback.
type Batch struct {
	s       *Store
	pending []op
}

// op is a write enqueued in the batch. Either alert or event is non-nil.
type op struct {
	isAlert bool
	alert   *Alert
	event   *Event
}

// NewBatch opens a new batch writer. Always pair with Close(ctx) to flush
// the tail.
func (s *Store) NewBatch() *Batch {
	return &Batch{s: s, pending: make([]op, 0, batchFlushSize)}
}

// AddAlert queues an alert write. The alert's ID is assigned during Flush.
func (b *Batch) AddAlert(a *Alert) error {
	b.pending = append(b.pending, op{isAlert: true, alert: a})
	if len(b.pending) >= batchFlushSize {
		return b.flush(context.Background())
	}
	return nil
}

// AddEvent queues an event write.
func (b *Batch) AddEvent(e *Event) error {
	b.pending = append(b.pending, op{isAlert: false, event: e})
	if len(b.pending) >= batchFlushSize {
		return b.flush(context.Background())
	}
	return nil
}

// Close flushes any remaining queued writes. Always defer this.
func (b *Batch) Close(ctx context.Context) error {
	if len(b.pending) == 0 {
		return nil
	}
	return b.flush(ctx)
}

// flush issues a single pipelined batch. We reserve IDs with INCRBY at the
// front of the pipeline (one round trip), then SET + ZADD per record. Total
// round trips per batch: 1 (id reserve) + 1 (pipelined writes) = 2.
func (b *Batch) flush(ctx context.Context) error {
	if len(b.pending) == 0 {
		return nil
	}
	b.s.invalidate()

	var alertN, eventN int64
	for _, o := range b.pending {
		if o.isAlert {
			alertN++
		} else {
			eventN++
		}
	}

	// Reserve ID ranges. After INCRBY N, the returned value is the LAST id in
	// the reserved range, so first-id = returned - N + 1.
	var alertStart, eventStart int64
	if alertN > 0 {
		end, err := b.s.rdb.IncrBy(ctx, keyNextA, alertN).Result()
		if err != nil {
			return fmt.Errorf("incrby next:a: %w", err)
		}
		alertStart = end - alertN + 1
	}
	if eventN > 0 {
		end, err := b.s.rdb.IncrBy(ctx, keyNextE, eventN).Result()
		if err != nil {
			return fmt.Errorf("incrby next:e: %w", err)
		}
		eventStart = end - eventN + 1
	}

	pipe := b.s.rdb.Pipeline()
	for _, o := range b.pending {
		if o.isAlert {
			o.alert.ID = alertStart
			alertStart++
			body, err := json.Marshal(o.alert)
			if err != nil {
				return fmt.Errorf("marshal alert id=%d: %w", o.alert.ID, err)
			}
			key := prefixAlert + strconv.FormatInt(o.alert.ID, 10)
			pipe.Set(ctx, key, body, 0)
			pipe.ZAdd(ctx, keyTimeline, redis.Z{
				Score:  float64(o.alert.Timestamp.UnixNano()),
				Member: key,
			})
		} else {
			o.event.ID = eventStart
			eventStart++
			body, err := json.Marshal(o.event)
			if err != nil {
				return fmt.Errorf("marshal event id=%d: %w", o.event.ID, err)
			}
			key := prefixEvent + strconv.FormatInt(o.event.ID, 10)
			pipe.Set(ctx, key, body, 0)
			pipe.ZAdd(ctx, keyTimeline, redis.Z{
				Score:  float64(o.event.Timestamp.UnixNano()),
				Member: key,
			})
		}
	}

	if _, err := pipe.Exec(ctx); err != nil {
		return fmt.Errorf("pipeline exec: %w", err)
	}
	b.pending = b.pending[:0]
	return nil
}
