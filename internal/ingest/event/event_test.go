package event

import (
	"context"
	"strings"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"github.com/mhristodor/rustinel-view/internal/store"
)

func newBatch(t *testing.T) (*store.Batch, func()) {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	s := store.New(rdb)
	return s.NewBatch(), func() { _ = rdb.Close(); mr.Close() }
}

// Real lines lifted from the rustinel.log sample.
var samples = map[string]string{
	"Dns":     `2026-05-27T19:46:40.097476Z TRACE engine: Normalized event normalized_json={"timestamp":"2026-05-27T19:46:40Z","platform":"linux","provider":"ebpf","category":"Dns","event_id":22,"opcode":0,"fields":{"QueryName":".","RecordType":"OTHER","ProcessId":"173369"}}`,
	"File":    `2026-05-27T19:46:50.123456Z TRACE engine: Normalized event normalized_json={"timestamp":"2026-05-27T19:46:50Z","platform":"linux","provider":"ebpf","category":"File","event_id":11,"opcode":64,"fields":{"TargetFilename":"/sys/fs/cgroup/system.slice/x","ProcessId":"1","Image":"systemd","User":"root"}}`,
	"Network": `2026-05-27T19:46:40.200000Z TRACE engine: Normalized event normalized_json={"timestamp":"2026-05-27T19:46:40Z","platform":"linux","provider":"ebpf","category":"Network","event_id":3,"opcode":0,"fields":{"DestinationIp":"2a01:4f8:1c17:4f88::","SourceIp":"192.168.1.182","DestinationPort":"443","SourcePort":"43558","ProcessId":"2886","Protocol":"tcp"}}`,
	"Process": `2026-05-27T19:46:50.300000Z TRACE engine: Normalized event normalized_json={"timestamp":"2026-05-27T19:46:50Z","platform":"linux","provider":"ebpf","category":"Process","event_id":1,"opcode":1,"fields":{"Image":"/proc/self/fd/16","CommandLine":"/usr/lib/systemd/systemd-executor","ProcessId":"442059","ParentProcessId":"1","User":"root"}}`,
}

func TestParseEachCategoryRoundTrips(t *testing.T) {
	for cat, line := range samples {
		t.Run(cat, func(t *testing.T) {
			b, cleanup := newBatch(t)
			defer cleanup()

			res := Parse(context.Background(), strings.NewReader(line), b)
			_ = b.Close(context.Background())

			if res.Parsed != 1 || res.Failed != 0 {
				t.Fatalf("%s: expected 1 parsed / 0 failed, got %+v", cat, res)
			}
		})
	}
}

func TestParseSkipsNonNormalizedLines(t *testing.T) {
	b, cleanup := newBatch(t)
	defer cleanup()

	input := strings.Join([]string{
		"2026-05-27T19:46:21.967128Z INFO rustinel: starting",
		samples["Dns"],
		"2026-05-27T19:46:21.967300Z INFO rustinel: another non-event line",
		samples["Process"],
	}, "\n")

	res := Parse(context.Background(), strings.NewReader(input), b)
	_ = b.Close(context.Background())

	if res.Parsed != 2 || res.Failed != 0 {
		t.Errorf("expected 2 parsed / 0 failed, got %+v", res)
	}
}

// File events targeting /dev/null are pure sensor noise (shell redirects,
// health-check scripts, dd benchmarks) — never written to the store, so
// they never reach the lineage view either. This is an ingest-layer drop,
// independent of the user-controllable DropGarbage toggle.
func TestParseDropsDevNullFileEvents(t *testing.T) {
	devnull := `2026-05-27T19:46:50.000000Z TRACE engine: Normalized event normalized_json={"timestamp":"2026-05-27T19:46:50Z","platform":"linux","provider":"ebpf","category":"File","event_id":11,"opcode":64,"fields":{"TargetFilename":"/dev/null","ProcessId":"123","Image":"/bin/sh"}}`

	b, cleanup := newBatch(t)
	defer cleanup()

	input := strings.Join([]string{samples["File"], devnull, devnull, samples["Process"]}, "\n")
	res := Parse(context.Background(), strings.NewReader(input), b)
	_ = b.Close(context.Background())

	if res.Parsed != 2 {
		t.Errorf("expected 2 parsed (File + Process), got %d (%+v)", res.Parsed, res)
	}
	if res.Skipped != 2 {
		t.Errorf("expected 2 skipped (/dev/null x2), got %d (%+v)", res.Skipped, res)
	}
	if res.Failed != 0 {
		t.Errorf("expected 0 failed, got %d", res.Failed)
	}
}

func TestDecodePayloadCategoryFieldsPreserved(t *testing.T) {
	// Strip the rustinel log prefix to get the bare payload.
	payload := samples["Network"]
	idx := strings.Index(payload, "normalized_json=")
	payload = payload[idx+len("normalized_json="):]

	e, err := decodePayload([]byte(payload))
	if err != nil {
		t.Fatalf("decodePayload: %v", err)
	}
	if e.Category != "Network" {
		t.Errorf("category: %q", e.Category)
	}
	if e.Fields["DestinationIp"] != "2a01:4f8:1c17:4f88::" {
		t.Errorf("DestinationIp: %q", e.Fields["DestinationIp"])
	}
	if e.Fields["Protocol"] != "tcp" {
		t.Errorf("Protocol: %q", e.Fields["Protocol"])
	}
}
