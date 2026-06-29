package store

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

// newTestStore spins a miniredis + go-redis client for each test, returning
// a Store ready to use and a cleanup func.
func newTestStore(t *testing.T) (*Store, func()) {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	return New(rdb), func() {
		_ = rdb.Close()
		mr.Close()
	}
}

func TestAddAlertAndGet(t *testing.T) {
	s, cleanup := newTestStore(t)
	defer cleanup()
	ctx := context.Background()

	a := &Alert{
		Timestamp:       time.Date(2026, 5, 27, 19, 46, 53, 0, time.UTC),
		RuleName:        "File Deletion",
		EDRRuleSeverity: "Low",
		ProcessName:     "rm",
		ProcessPID:      442119,
	}
	if err := s.AddAlert(ctx, a); err != nil {
		t.Fatalf("AddAlert: %v", err)
	}
	if a.ID == 0 {
		t.Fatal("expected ID to be assigned")
	}

	got, err := s.GetAlert(ctx, a.ID)
	if err != nil {
		t.Fatalf("GetAlert: %v", err)
	}
	if got.RuleName != a.RuleName || got.ProcessPID != a.ProcessPID {
		t.Errorf("round trip mismatch: got %+v", got)
	}
}

func TestAddEventAndGet(t *testing.T) {
	s, cleanup := newTestStore(t)
	defer cleanup()
	ctx := context.Background()

	e := &Event{
		Timestamp: time.Date(2026, 5, 27, 19, 46, 40, 0, time.UTC),
		Category:  "Dns",
		EventID:   22,
		Fields:    map[string]string{"QueryName": "example.com", "ProcessId": "1234"},
	}
	if err := s.AddEvent(ctx, e); err != nil {
		t.Fatalf("AddEvent: %v", err)
	}
	got, err := s.GetEvent(ctx, e.ID)
	if err != nil {
		t.Fatalf("GetEvent: %v", err)
	}
	if got.Category != "Dns" || got.Fields["QueryName"] != "example.com" {
		t.Errorf("round trip mismatch: got %+v", got)
	}
}

func TestBatchIngestAndTimelineOrder(t *testing.T) {
	s, cleanup := newTestStore(t)
	defer cleanup()
	ctx := context.Background()

	batch := s.NewBatch()
	base := time.Date(2026, 5, 27, 19, 46, 0, 0, time.UTC)
	for i := 0; i < 1000; i++ {
		if i%5 == 0 {
			a := &Alert{Timestamp: base.Add(time.Duration(i) * time.Second), RuleName: "r"}
			if err := batch.AddAlert(a); err != nil {
				t.Fatalf("AddAlert %d: %v", i, err)
			}
		} else {
			e := &Event{Timestamp: base.Add(time.Duration(i) * time.Second), Category: "Dns"}
			if err := batch.AddEvent(e); err != nil {
				t.Fatalf("AddEvent %d: %v", i, err)
			}
		}
	}
	if err := batch.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}

	page, err := s.TimelinePage(ctx, Filters{Limit: 50})
	if err != nil {
		t.Fatalf("TimelinePage: %v", err)
	}
	if len(page.Rows) != 50 {
		t.Errorf("expected 50 rows, got %d", len(page.Rows))
	}
	for i := 1; i < len(page.Rows); i++ {
		prev := page.Rows[i-1].Timestamp()
		curr := page.Rows[i].Timestamp()
		if curr.Before(prev) {
			t.Errorf("rows not in chronological order at i=%d", i)
		}
	}
	if !page.HasMore {
		t.Error("expected HasMore=true with 1000 records and limit=50")
	}
}

func TestFilterSrc(t *testing.T) {
	s, cleanup := newTestStore(t)
	defer cleanup()
	ctx := context.Background()

	batch := s.NewBatch()
	base := time.Date(2026, 5, 27, 19, 46, 0, 0, time.UTC)
	for i := 0; i < 100; i++ {
		if i%2 == 0 {
			_ = batch.AddAlert(&Alert{Timestamp: base.Add(time.Duration(i) * time.Second), EDRRuleSeverity: "Low"})
		} else {
			_ = batch.AddEvent(&Event{Timestamp: base.Add(time.Duration(i) * time.Second), Category: "Dns"})
		}
	}
	_ = batch.Close(ctx)

	alertsOnly, _ := s.TimelinePage(ctx, Filters{IncludeAlerts: true, Limit: 100})
	if got := len(alertsOnly.Rows); got != 50 {
		t.Errorf("alerts-only: expected 50, got %d", got)
	}
	for _, r := range alertsOnly.Rows {
		if r.Kind != "a" {
			t.Errorf("expected only alerts, got kind=%s", r.Kind)
		}
	}

	eventsOnly, _ := s.TimelinePage(ctx, Filters{IncludeEvents: true, Limit: 100})
	if got := len(eventsOnly.Rows); got != 50 {
		t.Errorf("events-only: expected 50, got %d", got)
	}
	for _, r := range eventsOnly.Rows {
		if r.Kind != "e" {
			t.Errorf("expected only events, got kind=%s", r.Kind)
		}
	}
}

func TestFilterSeverity(t *testing.T) {
	s, cleanup := newTestStore(t)
	defer cleanup()
	ctx := context.Background()

	batch := s.NewBatch()
	base := time.Date(2026, 5, 27, 19, 46, 0, 0, time.UTC)
	sevs := []string{"Critical", "High", "Medium", "Low", "Critical"}
	for i, sev := range sevs {
		_ = batch.AddAlert(&Alert{Timestamp: base.Add(time.Duration(i) * time.Second), EDRRuleSeverity: sev})
	}
	_ = batch.Close(ctx)

	page, _ := s.TimelinePage(ctx, Filters{IncludeAlerts: true, Severities: []string{"Critical"}, Limit: 100})
	if len(page.Rows) != 2 {
		t.Errorf("expected 2 Critical, got %d", len(page.Rows))
	}
}

func TestCursorPagination(t *testing.T) {
	s, cleanup := newTestStore(t)
	defer cleanup()
	ctx := context.Background()

	batch := s.NewBatch()
	base := time.Date(2026, 5, 27, 19, 46, 0, 0, time.UTC)
	for i := 0; i < 50; i++ {
		_ = batch.AddEvent(&Event{Timestamp: base.Add(time.Duration(i) * time.Second), Category: "Dns"})
	}
	_ = batch.Close(ctx)

	p1, _ := s.TimelinePage(ctx, Filters{Limit: 20})
	if len(p1.Rows) != 20 {
		t.Fatalf("page 1: expected 20, got %d", len(p1.Rows))
	}
	p2, _ := s.TimelinePage(ctx, Filters{Limit: 20, Cursor: p1.NextCursor})
	if len(p2.Rows) != 20 {
		t.Fatalf("page 2: expected 20, got %d", len(p2.Rows))
	}
	p3, _ := s.TimelinePage(ctx, Filters{Limit: 20, Cursor: p2.NextCursor})
	if len(p3.Rows) != 10 {
		t.Fatalf("page 3: expected 10, got %d", len(p3.Rows))
	}
	if p3.HasMore {
		t.Error("expected HasMore=false on last page")
	}

	// Verify no overlap across pages.
	seen := make(map[int64]bool)
	for _, p := range []Page{p1, p2, p3} {
		for _, r := range p.Rows {
			id := r.Event.ID
			if seen[id] {
				t.Errorf("duplicate event id %d across pages", id)
			}
			seen[id] = true
		}
	}
	if len(seen) != 50 {
		t.Errorf("expected 50 unique ids across pages, got %d", len(seen))
	}
}

func TestMetaRoundTrip(t *testing.T) {
	s, cleanup := newTestStore(t)
	defer cleanup()
	ctx := context.Background()

	in := Meta{
		AlertsParsed: 328, AlertsFailed: 0,
		EventsParsed: 40783, EventsFailed: 2,
		StartedAt:    time.Date(2026, 5, 28, 0, 22, 41, 0, time.UTC),
		FinishedAt:   time.Date(2026, 5, 28, 0, 22, 42, 577_000_000, time.UTC),
		IngestMillis: 933,
	}
	if err := s.WriteMeta(ctx, in); err != nil {
		t.Fatalf("WriteMeta: %v", err)
	}
	out, err := s.ReadMeta(ctx)
	if err != nil {
		t.Fatalf("ReadMeta: %v", err)
	}
	if out.AlertsParsed != in.AlertsParsed || out.EventsParsed != in.EventsParsed ||
		out.IngestMillis != in.IngestMillis {
		t.Errorf("meta mismatch: got %+v want %+v", out, in)
	}
}
