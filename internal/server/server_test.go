package server

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"github.com/mhristodor/rustinel-view/internal/store"
)

// fixture spins up a Server backed by miniredis seeded with a known mix of
// alerts and events, and returns an httptest.Server ready to hit.
func fixture(t *testing.T) (*httptest.Server, func()) {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	s := store.New(rdb)
	ctx := context.Background()

	batch := s.NewBatch()
	base := time.Date(2026, 5, 27, 19, 46, 0, 0, time.UTC)
	_ = batch.AddAlert(&store.Alert{
		Timestamp:       base,
		EDRRuleSeverity: "Critical",
		EDRRuleEngine:   "Sigma",
		RuleName:        "Test Critical Alert",
		ProcessName:     "rm",
		ProcessPID:      999,
	})
	_ = batch.AddEvent(&store.Event{
		Timestamp: base.Add(1 * time.Second),
		Category:  "Dns",
		EventID:   22,
		Fields:    map[string]string{"QueryName": "example.com", "ProcessId": "100"},
	})
	_ = batch.AddEvent(&store.Event{
		Timestamp: base.Add(2 * time.Second),
		Category:  "Process",
		Fields:    map[string]string{"Image": "/bin/cat", "CommandLine": "cat /etc/passwd", "ProcessId": "200"},
	})
	_ = batch.AddEvent(&store.Event{
		Timestamp: base.Add(3 * time.Second),
		Category:  "Process",
		Fields:    map[string]string{"Image": "/usr/bin/curl", "CommandLine": "curl example.org", "ProcessId": "300", "ParentProcessId": "200"},
	})
	_ = batch.Close(ctx)
	_ = s.WriteMeta(ctx, store.Meta{AlertsParsed: 1, EventsParsed: 2, IngestMillis: 10})

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := New(s, logger)
	hs := httptest.NewServer(srv.mux)

	return hs, func() { hs.Close(); _ = rdb.Close(); mr.Close() }
}

func get(t *testing.T, hs *httptest.Server, path string) string {
	t.Helper()
	res, err := http.Get(hs.URL + path)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(res.Body)
	if res.StatusCode != 200 {
		t.Fatalf("GET %s: status %d body=%s", path, res.StatusCode, body)
	}
	return string(body)
}

func TestHomePageHasMetaAndRows(t *testing.T) {
	hs, cleanup := fixture(t)
	defer cleanup()

	body := get(t, hs, "/")
	mustContain(t, body, "RUSTINEL")
	mustContain(t, body, "// VIEW")
	mustContain(t, body, ">1<")       // 1 alert counted
	mustContain(t, body, ">2<")       // 2 events counted
	mustContain(t, body, "row-alert") // an alert row rendered
	mustContain(t, body, "row-event") // an event row rendered
	mustContain(t, body, "badge-alert")
}

func TestTimelineFragment(t *testing.T) {
	hs, cleanup := fixture(t)
	defer cleanup()

	body := get(t, hs, "/timeline?src=alerts&src=events")
	mustContain(t, body, "data-detail-url=\"/detail/")
	mustContain(t, body, "Test Critical Alert")
	mustContain(t, body, "example.com")
}

func TestTimelineFilterSrc(t *testing.T) {
	hs, cleanup := fixture(t)
	defer cleanup()

	body := get(t, hs, "/timeline?src=alerts")
	mustContain(t, body, "Test Critical Alert")
	mustNotContain(t, body, "row-event")
}

func TestDetailAlert(t *testing.T) {
	hs, cleanup := fixture(t)
	defer cleanup()

	body := get(t, hs, "/detail/a/1")
	mustContain(t, body, "ALERT")
	mustContain(t, body, "Test Critical Alert")
	mustContain(t, body, "Sigma")
}

func TestDetailEventShowsFields(t *testing.T) {
	hs, cleanup := fixture(t)
	defer cleanup()

	body := get(t, hs, "/detail/e/1")
	mustContain(t, body, "DNS")
	mustContain(t, body, "QueryName")
	mustContain(t, body, "example.com")
}

// Regression: ?cat=Process&cat=Network should OR the categories.
// Previously q.Get("cat") returned only the first value, so selecting
// Process + Network silently dropped the second.
func TestTimelineMultiCatOR(t *testing.T) {
	hs, cleanup := fixture(t)
	defer cleanup()

	// The fixture seeds one Process event and one Dns event.
	body := get(t, hs, "/timeline?cat=Process&cat=Dns")
	mustContain(t, body, "cat-process")
	mustContain(t, body, "cat-dns")

	// Single-category filter still works exclusively.
	body = get(t, hs, "/timeline?cat=Process")
	mustContain(t, body, "cat-process")
	mustNotContain(t, body, "cat-dns")
}

// Regression: ?src=alerts&src=events should include both, not drop one.
func TestTimelineMultiSrcOR(t *testing.T) {
	hs, cleanup := fixture(t)
	defer cleanup()
	body := get(t, hs, "/timeline?src=alerts&src=events")
	mustContain(t, body, "row-alert")
	mustContain(t, body, "row-event")
}

// Filename filter should match against TargetFilename + Image + CommandLine.
func TestTimelineFilenameFilter(t *testing.T) {
	hs, cleanup := fixture(t)
	defer cleanup()
	// The fixture seeds an event whose CommandLine contains "/etc/passwd".
	body := get(t, hs, "/timeline?fname=passwd")
	mustContain(t, body, "/bin/cat")
	mustNotContain(t, body, "example.com")
}

func TestTimelineBadParamsSoftDegrades(t *testing.T) {
	hs, cleanup := fixture(t)
	defer cleanup()

	// Garbage cursor should NOT 4xx — bookmarkable URL semantics.
	res, err := http.Get(hs.URL + "/timeline?cursor=not-a-number&from=garbage")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		t.Errorf("expected soft-degrade 200, got %d", res.StatusCode)
	}
}

// Time range: no calendar/popover UI any more — brush-drag on the
// density strip drives the from/to inputs, which stay in the form as
// hidden carriers.
func TestTimelineTimeRangeHiddenInputs(t *testing.T) {
	hs, cleanup := fixture(t)
	defer cleanup()
	body := get(t, hs, "/")
	mustContain(t, body, `name="from"`)
	mustContain(t, body, `name="to"`)
	mustNotContain(t, body, `id="time-trigger"`)
	mustNotContain(t, body, `id="time-panel"`)
	mustNotContain(t, body, `data-time-preset`)
}

// Timeline header column controls: every column th carries hover tools
// (move left/right, remove) and the header row ends with an add button.
func TestTimelineTheadColumnTools(t *testing.T) {
	hs, cleanup := fixture(t)
	defer cleanup()

	body := get(t, hs, "/")
	mustContain(t, body, `data-col-move="left"`)
	mustContain(t, body, `data-col-move="right"`)
	mustContain(t, body, "data-col-remove")
	mustContain(t, body, "col-add-btn")
	// The page-header columns button is gone — the header + is the only
	// opener — but the shared picker popover must survive.
	mustNotContain(t, body, `id="cols-btn"`)
	mustContain(t, body, `id="cols-popover"`)
}

// Lineage tree chrome: branch wrappers carry the connector rails and a
// per-depth CSS var, ancestors are styled as the spine, and the sticky
// bar offers a breadcrumb + origin jump.
func TestLineagePageTreeChrome(t *testing.T) {
	hs, cleanup := fixture(t)
	defer cleanup()

	// PID 300's parent chain is 200 → 300 (seeded in fixture).
	body := get(t, hs, "/lineage/300")
	mustContain(t, body, "lin-branch")           // connector wrapper per node
	mustContain(t, body, "is-ancestor")          // parent chain spine class
	mustContain(t, body, "--d:1")                // origin depth var (root=0)
	mustContain(t, body, `data-crumb-pid="200"`) // breadcrumb ancestor
	mustContain(t, body, `data-crumb-pid="300"`) // breadcrumb origin entry
	mustContain(t, body, "lin-goto-origin")      // ◎ origin button in bar
}

func mustContain(t *testing.T, hay, needle string) {
	t.Helper()
	if !strings.Contains(hay, needle) {
		t.Errorf("expected response to contain %q, body was:\n%s", needle, hay)
	}
}

func mustNotContain(t *testing.T, hay, needle string) {
	t.Helper()
	if strings.Contains(hay, needle) {
		t.Errorf("expected response NOT to contain %q, body was:\n%s", needle, hay)
	}
}
