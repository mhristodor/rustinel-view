package alert

import (
	"context"
	"strings"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"github.com/mhristodor/rustinel-view/internal/store"
)

const sampleAlert = `{"@timestamp":"2026-05-27T19:46:53Z","ecs.version":"9.3.0","event.kind":"alert","event.category":["process"],"event.severity":25,"edr.rule.severity":"Low","edr.rule.engine":"Sigma","rule.name":"File Deletion","process.executable":"/usr/bin/rm","process.name":"rm","process.command_line":"rm -rf /tmp/lesspipe.16813","process.pid":442119,"user.name":"mhristodor","related.user":["mhristodor"]}`

const malformedAlert = `{not json at all`

func newBatch(t *testing.T) (*store.Batch, func()) {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	s := store.New(rdb)
	return s.NewBatch(), func() { _ = rdb.Close(); mr.Close() }
}

func TestParseGoldenLine(t *testing.T) {
	b, cleanup := newBatch(t)
	defer cleanup()

	res := Parse(context.Background(), strings.NewReader(sampleAlert), b)
	_ = b.Close(context.Background())

	if res.Parsed != 1 || res.Failed != 0 {
		t.Fatalf("expected 1 parsed / 0 failed, got %+v", res)
	}
}

func TestParseMalformedLineCountedNotAborted(t *testing.T) {
	b, cleanup := newBatch(t)
	defer cleanup()

	input := sampleAlert + "\n" + malformedAlert + "\n" + sampleAlert
	res := Parse(context.Background(), strings.NewReader(input), b)
	_ = b.Close(context.Background())

	if res.Parsed != 2 {
		t.Errorf("expected 2 parsed, got %d", res.Parsed)
	}
	if res.Failed != 1 {
		t.Errorf("expected 1 failed, got %d", res.Failed)
	}
	if len(res.Errors) == 0 {
		t.Error("expected error message recorded for malformed line")
	}
}

func TestParseFieldRoundTrip(t *testing.T) {
	a, err := decodeLine([]byte(sampleAlert))
	if err != nil {
		t.Fatalf("decodeLine: %v", err)
	}
	if a.RuleName != "File Deletion" {
		t.Errorf("rule.name: %q", a.RuleName)
	}
	if a.EDRRuleSeverity != "Low" {
		t.Errorf("edr.rule.severity: %q", a.EDRRuleSeverity)
	}
	if a.ProcessPID != 442119 {
		t.Errorf("process.pid: %d", a.ProcessPID)
	}
	if a.ProcessCommandLine != "rm -rf /tmp/lesspipe.16813" {
		t.Errorf("process.command_line: %q", a.ProcessCommandLine)
	}
	if len(a.EventCategory) != 1 || a.EventCategory[0] != "process" {
		t.Errorf("event.category: %v", a.EventCategory)
	}
}
