// Package event parses raw rustinel log lines that contain
// `normalized_json={...}` payloads and writes each parsed event to the
// store via a pipelined batch. The 50,000-event ring buffer that previously
// lived in this package is gone; events are streamed straight to Redis with
// no in-process cap.
package event

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"time"

	"github.com/mhristodor/rustinel-view/internal/store"
)

// Result summarises a single parse run.
type Result struct {
	Parsed         int
	Failed         int
	Skipped        int // events dropped at ingest as pure noise (e.g. /dev/null writes)
	Errors         []string
	CategoryCounts map[string]int // category → N
	MinTime        time.Time      // earliest event timestamp observed
	MaxTime        time.Time      // latest event timestamp observed
}

// 4 MB buffer: log lines with large payloads can exceed the default 64 KB
// scanner buffer.
const scannerBufferSize = 4 << 20

var normalizedPrefix = []byte("normalized_json=")

// Parse streams r, extracts every `normalized_json={...}` payload, decodes
// it into a store.Event, and writes it to the supplied batch.
func Parse(ctx context.Context, r io.Reader, batch *store.Batch) Result {
	res := Result{CategoryCounts: make(map[string]int)}

	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, scannerBufferSize), scannerBufferSize)

	lineNum := 0
	for scanner.Scan() {
		if ctx.Err() != nil {
			res.Errors = append(res.Errors, fmt.Sprintf("context cancelled at line %d", lineNum))
			return res
		}
		lineNum++
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}

		head, payload, ok := bytes.Cut(line, normalizedPrefix)
		if !ok {
			// Line has no normalized payload — log header, metrics line, etc.
			continue
		}

		e, err := decodePayload(payload)
		if err != nil {
			res.Failed++
			res.Errors = append(res.Errors, fmt.Sprintf("line %d: %v", lineNum, err))
			continue
		}

		// Ingest-layer noise drop: events that carry no forensic signal and
		// must never appear in the timeline OR the lineage view. Unlike
		// store.DropGarbage (toggle-controlled), this filter is unconditional
		// because the event never enters the data pool — there is no escape
		// hatch by design.
		if isIngestNoise(e) {
			res.Skipped++
			continue
		}

		// Prefer the microsecond-precision timestamp from the log-line prefix
		// over the second-precision one inside normalized_json. The prefix is
		// the first whitespace-delimited token on the line — an RFC3339Nano
		// string emitted by the rustinel logger.
		if prefix := leadingTimestamp(head); !prefix.IsZero() {
			e.Timestamp = prefix
		}
		if err := batch.AddEvent(e); err != nil {
			res.Failed++
			res.Errors = append(res.Errors, fmt.Sprintf("line %d: write: %v", lineNum, err))
			continue
		}
		res.Parsed++
		if e.Category != "" {
			res.CategoryCounts[e.Category]++
		}
		if !e.Timestamp.IsZero() {
			if res.MinTime.IsZero() || e.Timestamp.Before(res.MinTime) {
				res.MinTime = e.Timestamp
			}
			if e.Timestamp.After(res.MaxTime) {
				res.MaxTime = e.Timestamp
			}
		}
	}
	if err := scanner.Err(); err != nil {
		res.Errors = append(res.Errors, fmt.Sprintf("scanner: %v", err))
	}
	return res
}

// raw decodes the wire format. Timestamp is a string before conversion.
type raw struct {
	Timestamp string            `json:"timestamp"`
	Platform  string            `json:"platform"`
	Provider  string            `json:"provider"`
	Category  string            `json:"category"`
	EventID   int               `json:"event_id"`
	Opcode    int               `json:"opcode"`
	Fields    map[string]string `json:"fields"`
}

// leadingTimestamp extracts the RFC3339Nano timestamp that begins each
// rustinel log line. Returns the zero time if the leading token cannot be
// parsed — callers fall back to the in-payload second-precision timestamp.
func leadingTimestamp(head []byte) time.Time {
	tok, _, _ := bytes.Cut(head, []byte(" "))
	if len(tok) == 0 {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339Nano, string(tok))
	if err != nil {
		return time.Time{}
	}
	return t.UTC()
}

// isIngestNoise classifies events that should never enter the store.
//
// Currently:
//   - File events whose TargetFilename is exactly /dev/null. Writes to
//     the bit bucket carry no forensic signal (shell redirects, dd
//     benchmarks, health-check scripts) and were polluting the lineage
//     view, which bypasses the store-side DropGarbage toggle.
func isIngestNoise(e *store.Event) bool {
	if e == nil {
		return false
	}
	if e.Category == "File" && e.Fields["TargetFilename"] == "/dev/null" {
		return true
	}
	return false
}

func decodePayload(data []byte) (*store.Event, error) {
	var rw raw
	if err := json.Unmarshal(data, &rw); err != nil {
		return nil, fmt.Errorf("unmarshal: %w", err)
	}
	ts, err := time.Parse(time.RFC3339, rw.Timestamp)
	if err != nil {
		return nil, fmt.Errorf("parse timestamp %q: %w", rw.Timestamp, err)
	}
	return &store.Event{
		Timestamp: ts,
		Platform:  rw.Platform,
		Provider:  rw.Provider,
		Category:  rw.Category,
		EventID:   rw.EventID,
		Opcode:    rw.Opcode,
		Fields:    rw.Fields,
	}, nil
}
