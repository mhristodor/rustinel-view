package store

import (
	"context"
	"strconv"
	"testing"
	"time"
)

// seedChain writes Process events forming the chain 1 → 10 → 100 → 200,
// where each pair is (pid, ppid).
func seedChain(t *testing.T, s *Store) {
	t.Helper()
	ctx := context.Background()
	base := time.Date(2026, 5, 27, 19, 46, 0, 0, time.UTC)
	batch := s.NewBatch()
	pairs := [][2]int{{1, 0}, {10, 1}, {100, 10}, {200, 100}}
	for i, p := range pairs {
		fields := map[string]string{
			"Image":     "/bin/proc" + strconv.Itoa(p[0]),
			"ProcessId": strconv.Itoa(p[0]),
		}
		if p[1] > 0 {
			fields["ParentProcessId"] = strconv.Itoa(p[1])
		}
		if err := batch.AddEvent(&Event{
			Timestamp: base.Add(time.Duration(i) * time.Second),
			Category:  "Process",
			Fields:    fields,
		}); err != nil {
			t.Fatalf("AddEvent pid=%d: %v", p[0], err)
		}
	}
	if err := batch.Close(ctx); err != nil {
		t.Fatalf("batch close: %v", err)
	}
}

func TestBuildLineageForDepthAndAncestorFlags(t *testing.T) {
	s, cleanup := newTestStore(t)
	defer cleanup()
	seedChain(t, s)

	view, err := s.BuildLineageFor(context.Background(), 100)
	if err != nil {
		t.Fatalf("BuildLineageFor: %v", err)
	}

	if len(view.Parents) != 2 {
		t.Fatalf("expected 2 parents, got %d", len(view.Parents))
	}
	if view.Parents[0].PID != 1 || view.Parents[1].PID != 10 {
		t.Fatalf("parent chain wrong: got %d, %d", view.Parents[0].PID, view.Parents[1].PID)
	}

	for i, p := range view.Parents {
		if !p.IsAncestor {
			t.Errorf("Parents[%d] (pid %d): IsAncestor = false, want true", i, p.PID)
		}
		if p.Depth != i {
			t.Errorf("Parents[%d] (pid %d): Depth = %d, want %d", i, p.PID, p.Depth, i)
		}
	}

	if view.Origin.IsAncestor {
		t.Error("origin: IsAncestor = true, want false")
	}
	if view.Origin.Depth != 2 {
		t.Errorf("origin Depth = %d, want 2", view.Origin.Depth)
	}

	if len(view.Origin.Children) != 1 || view.Origin.Children[0].PID != 200 {
		t.Fatalf("expected origin child pid 200, got %+v", view.Origin.Children)
	}
	child := view.Origin.Children[0]
	if child.Depth != 3 {
		t.Errorf("child Depth = %d, want 3", child.Depth)
	}
	if child.IsAncestor {
		t.Error("child: IsAncestor = true, want false")
	}
}
