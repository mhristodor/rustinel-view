package store

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

// Limits for cursor pagination.
const (
	defaultLimit = 500
	maxLimit     = 5_000
)

// TimelinePage runs one paginated, filtered timeline query against the
// in-memory snapshot. The dataset is time-ordered, so the page start is
// a binary search (cursor exclusive, From inclusive) followed by a
// linear filtered walk until the page fills or the To bound is hit.
//
// This replaced a Redis-side buffered/refill pagination loop — with the
// snapshot materialized in memory there are no round trips to amortize,
// so the whole page assembly is one O(log n) search + O(scanned) walk.
func (s *Store) TimelinePage(ctx context.Context, f Filters) (Page, error) {
	limit := f.Limit
	if limit <= 0 {
		limit = defaultLimit
	}
	if limit > maxLimit {
		limit = maxLimit
	}

	// Normalize source toggle: if neither was explicitly set, return both.
	if !f.IncludeAlerts && !f.IncludeEvents {
		f.IncludeAlerts, f.IncludeEvents = true, true
	}

	rows, err := s.materializedRows(ctx)
	if err != nil {
		return Page{}, err
	}

	window := rowsBetween(rows, f.From, f.To)
	start := 0
	if f.Cursor > 0 {
		// First row strictly after the cursor (exclusive resume point).
		start = sort.Search(len(window), func(i int) bool {
			return window[i].Timestamp().UnixNano() > f.Cursor
		})
	}

	out := make([]Row, 0, limit)
	hasMore := false
	for i := start; i < len(window); i++ {
		r := window[i]
		if r.Kind == "a" && !f.IncludeAlerts {
			continue
		}
		if r.Kind == "e" && !f.IncludeEvents {
			continue
		}
		if !rowMatches(r, f) {
			continue
		}
		out = append(out, r)
		if len(out) >= limit {
			hasMore = true
			break
		}
	}

	var next int64
	if len(out) > 0 {
		next = out[len(out)-1].Timestamp().UnixNano()
	}
	// Garbage drop runs BEFORE collapse so the collapse pass only folds
	// real-signal rows. If we collapsed first, a 200-event burst of DNS
	// root-query garbage could fold into one row that the garbage pass
	// then drops — losing the visible "×200" count of noise and wasting
	// the collapse work.
	if f.DropGarbage {
		out = DropGarbage(out)
	}
	if f.Collapse {
		// Collapse runs once on the assembled page. Bursts that straddle a
		// page boundary will split into two rows (head on this page, tail
		// on the next) — documented in the spec; acceptable to keep the
		// query stateless.
		out = CollapseRows(out)
	}
	return Page{
		Rows:       out,
		NextCursor: next,
		HasMore:    hasMore,
	}, nil
}

// rowsBetween returns the subslice of the time-ordered rows whose
// timestamps fall inside [from, to] (both inclusive; zero values mean
// unbounded). Binary search keeps time-windowed queries O(log n) at
// the bounds instead of scanning the whole dataset.
func rowsBetween(rows []Row, from, to time.Time) []Row {
	lo := 0
	if !from.IsZero() {
		lo = sort.Search(len(rows), func(i int) bool {
			return !rows[i].Timestamp().Before(from)
		})
	}
	hi := len(rows)
	if !to.IsZero() {
		hi = sort.Search(len(rows), func(i int) bool {
			return rows[i].Timestamp().After(to)
		})
	}
	if lo > hi {
		lo = hi
	}
	return rows[lo:hi]
}

// allRowsInRange fetches and decodes every timeline member whose score
// falls inside [min, max] (ZRangeArgs string forms: "-inf", "(123", …).
// No filtering is applied — this is the shared low-level scan used by
// every full-dataset consumer (count, density, lineage).
func (s *Store) allRowsInRange(ctx context.Context, min, max string) ([]Row, error) {
	members, err := s.rdb.ZRangeArgs(ctx, redis.ZRangeArgs{
		Key:     keyTimeline,
		Start:   min,
		Stop:    max,
		ByScore: true,
	}).Result()
	if err != nil {
		return nil, fmt.Errorf("zrange scan: %w", err)
	}
	if len(members) == 0 {
		return nil, nil
	}
	bodies, err := s.rdb.MGet(ctx, members...).Result()
	if err != nil {
		return nil, fmt.Errorf("mget scan: %w", err)
	}
	rows := make([]Row, 0, len(members))
	for i, m := range members {
		raw, _ := bodies[i].(string)
		if raw == "" {
			continue
		}
		row, ok := decodeRow(m, []byte(raw))
		if !ok {
			continue
		}
		rows = append(rows, row)
	}
	return rows, nil
}

// matchingRows runs the complete filter pipeline (src toggle, rowMatches,
// optional garbage drop and burst collapse) over the entire dataset in
// [f.From, f.To]. Shared by TimelineCount and TimelineDensity, which only
// differ in how they aggregate the result.
//
// Serves from the in-memory snapshot: time bounds via binary search,
// then a linear filtered walk. On 40k events this runs in single-digit
// milliseconds (no Redis round trips, no JSON decode).
//
// NOTE: the filtered result is always copied into a fresh slice — the
// window subslice shares backing memory with the materialized snapshot,
// and an in-place `window[:0]` append would corrupt the cache.
func (s *Store) matchingRows(ctx context.Context, f Filters) ([]Row, error) {
	if !f.IncludeAlerts && !f.IncludeEvents {
		f.IncludeAlerts, f.IncludeEvents = true, true
	}
	all, err := s.materializedRows(ctx)
	if err != nil {
		return nil, err
	}
	window := rowsBetween(all, f.From, f.To)
	rows := make([]Row, 0, len(window))
	for _, r := range window {
		if r.Kind == "a" && !f.IncludeAlerts {
			continue
		}
		if r.Kind == "e" && !f.IncludeEvents {
			continue
		}
		if !rowMatches(r, f) {
			continue
		}
		rows = append(rows, r)
	}
	if f.DropGarbage {
		rows = DropGarbage(rows)
	}
	if f.Collapse {
		rows = CollapseRows(rows)
	}
	return rows, nil
}

// TimelineCount returns the post-pipeline row count for the supplied
// filter set. Used by the /count endpoint to feed the SIEM-style
// results bar.
func (s *Store) TimelineCount(ctx context.Context, f Filters) (int, error) {
	rows, err := s.matchingRows(ctx, f)
	if err != nil {
		return 0, err
	}
	return len(rows), nil
}

// MatchingRows is the exported full-pipeline query — every row matching
// the filter set, in time order. Used by the /export endpoint.
func (s *Store) MatchingRows(ctx context.Context, f Filters) ([]Row, error) {
	return s.matchingRows(ctx, f)
}

// TopFieldValues returns the n most frequent values of a canonical query
// field across the whole snapshot, most-common first. Feeds the query
// input's value autocomplete with real data instead of static examples.
func (s *Store) TopFieldValues(ctx context.Context, field string, n int) ([]string, error) {
	def := qSchemaByName(field)
	if def == nil || def.extract == nil {
		return nil, nil
	}
	rows, err := s.materializedRows(ctx)
	if err != nil {
		return nil, err
	}
	counts := map[string]int{}
	for _, r := range rows {
		if v, ok := def.extract(r); ok && v != "" {
			counts[v]++
		}
	}
	type vc struct {
		v string
		c int
	}
	all := make([]vc, 0, len(counts))
	for v, c := range counts {
		all = append(all, vc{v, c})
	}
	sort.Slice(all, func(i, j int) bool {
		if all[i].c != all[j].c {
			return all[i].c > all[j].c
		}
		return all[i].v < all[j].v // stable tiebreak for determinism
	})
	if n <= 0 || n > len(all) {
		n = len(all)
	}
	out := make([]string, 0, n)
	for _, e := range all[:n] {
		out = append(out, e.v)
	}
	return out, nil
}

// Density is a per-bucket activity summary for a filter set. Used by the
// mini-timeline above the table — clicking a bucket jumps the timeline
// to that point in time.
type Density struct {
	FromNs  int64  `json:"from_ns"`
	ToNs    int64  `json:"to_ns"`
	Buckets []DBin `json:"buckets"`
	Max     int    `json:"max"`
}

// DBin holds the per-bucket alert + event counts. Two channels so the
// renderer can stack them.
type DBin struct {
	A int `json:"a"`
	E int `json:"e"`
}

// TimelineDensity returns per-bucket activity counts for the supplied
// filter set. Bucket count is capped to a reasonable visual maximum so
// the response stays small even when callers ask for thousands.
//
// Density honors every filter, including DropGarbage and Collapse — so
// the bar always reflects exactly the rows the timeline would show.
func (s *Store) TimelineDensity(ctx context.Context, f Filters, nBuckets int) (Density, error) {
	if nBuckets <= 0 {
		nBuckets = 240
	}
	if nBuckets > 2000 {
		nBuckets = 2000
	}

	rows, err := s.matchingRows(ctx, f)
	if err != nil {
		return Density{}, err
	}
	if len(rows) == 0 {
		return Density{}, nil
	}

	fromNs := rows[0].Timestamp().UnixNano()
	toNs := rows[len(rows)-1].Timestamp().UnixNano()
	if toNs <= fromNs {
		// Single-instant snapshot — produce one bucket holding everything.
		bin := DBin{}
		for _, r := range rows {
			if r.Kind == "a" {
				bin.A++
			} else {
				bin.E++
			}
		}
		mx := bin.A + bin.E
		return Density{
			FromNs:  fromNs,
			ToNs:    fromNs,
			Buckets: []DBin{bin},
			Max:     mx,
		}, nil
	}

	spanNs := toNs - fromNs
	bucketNs := spanNs / int64(nBuckets)
	if bucketNs <= 0 {
		bucketNs = 1
	}
	buckets := make([]DBin, nBuckets)
	mx := 0
	for _, r := range rows {
		idx := (r.Timestamp().UnixNano() - fromNs) / bucketNs
		if idx >= int64(nBuckets) {
			idx = int64(nBuckets - 1) // clamp the very last record into the final bucket
		}
		if r.Kind == "a" {
			buckets[idx].A++
		} else {
			buckets[idx].E++
		}
		total := buckets[idx].A + buckets[idx].E
		if total > mx {
			mx = total
		}
	}
	return Density{
		FromNs:  fromNs,
		ToNs:    toNs,
		Buckets: buckets,
		Max:     mx,
	}, nil
}

func isAlertMember(m string) bool { return strings.HasPrefix(m, prefixAlert) }
func isEventMember(m string) bool { return strings.HasPrefix(m, prefixEvent) }

// decodeRow unmarshals a member's body into a Row. Returns ok=false if the
// body fails to decode (we'd rather skip a single bad row than fail the
// whole page).
func decodeRow(member string, body []byte) (Row, bool) {
	switch {
	case isAlertMember(member):
		var a Alert
		if err := json.Unmarshal(body, &a); err != nil {
			return Row{}, false
		}
		return Row{Kind: "a", Alert: &a}, true
	case isEventMember(member):
		var e Event
		if err := json.Unmarshal(body, &e); err != nil {
			return Row{}, false
		}
		return Row{Kind: "e", Event: &e}, true
	}
	return Row{}, false
}

// rowMatches applies the per-row filters (severity, category, PID,
// substring, filename) plus the parsed KQL query if present. Time range
// and source toggle are handled upstream.
func rowMatches(r Row, f Filters) bool {
	if f.QueryExpr != nil && !EvalQuery(f.QueryExpr, r) {
		return false
	}
	if r.Kind == "a" {
		if len(f.Severities) > 0 && !containsCI(f.Severities, r.Alert.EDRRuleSeverity) {
			return false
		}
		if f.Filename != "" && !alertMatchesFilename(r.Alert, f.Filename) {
			return false
		}
		// Category filter is events-only — alerts pass through.
	}
	if r.Kind == "e" {
		if len(f.Categories) > 0 && !containsCI(f.Categories, r.Event.Category) {
			return false
		}
		if len(f.Actions) > 0 && !containsCI(f.Actions, r.Event.Action()) {
			return false
		}
		if f.Filename != "" && !eventMatchesFilename(r.Event, f.Filename) {
			return false
		}
	}
	return true
}

func containsCI(haystack []string, needle string) bool {
	for _, h := range haystack {
		if strings.EqualFold(h, needle) {
			return true
		}
	}
	return false
}

// alertMatchesFilename narrows the substring to the path/executable-like
// fields specifically, so a user searching for "/etc/passwd" doesn't
// accidentally match a rule name that mentions "passwd".
func alertMatchesFilename(a *Alert, q string) bool {
	q = strings.ToLower(q)
	hay := strings.ToLower(a.ProcessExecutable + "\n" +
		a.ProcessCommandLine + "\n" +
		a.ProcessWorkingDirectory + "\n" +
		a.ProcessParentExecutable + "\n" +
		a.ProcessParentCommandLine)
	return strings.Contains(hay, q)
}

// eventMatchesFilename targets the path-like event fields:
// TargetFilename, Image, ParentImage, CommandLine, CurrentDirectory.
func eventMatchesFilename(e *Event, q string) bool {
	q = strings.ToLower(q)
	keys := []string{"TargetFilename", "Image", "ParentImage", "CommandLine", "ParentCommandLine", "CurrentDirectory"}
	for _, k := range keys {
		if v := e.Fields[k]; v != "" {
			if strings.Contains(strings.ToLower(v), q) {
				return true
			}
		}
	}
	return false
}
