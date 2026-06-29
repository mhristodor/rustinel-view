// Package alert parses NDJSON ECS alert logs and writes each record to the
// store via a pipelined batch. ECS keys are flat dot-notation strings, so we
// decode each line into a map[string]json.RawMessage first, then pull each
// field into the typed store.Alert.
package alert

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"time"

	"github.com/mhristodor/rustinel-view/internal/store"
)

// Result summarises a single parse run.
type Result struct {
	Parsed       int
	Failed       int
	Errors       []string
	EngineCounts map[string]int // edr.rule.engine → N ("Sigma", "Yara", …)
	RuleCounts   map[string]int // rule.name → N (for top-rule glance in header)
}

// Parse streams r line by line, decodes each NDJSON line into a store.Alert,
// and writes it to the supplied batch. Line-level errors are collected
// into Result.Errors so a single malformed line never aborts the run.
func Parse(ctx context.Context, r io.Reader, batch *store.Batch) Result {
	res := Result{
		EngineCounts: make(map[string]int),
		RuleCounts:   make(map[string]int),
	}

	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 1<<20), 1<<20)

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
		a, err := decodeLine(line)
		if err != nil {
			res.Failed++
			res.Errors = append(res.Errors, fmt.Sprintf("line %d: %v", lineNum, err))
			continue
		}
		if err := batch.AddAlert(a); err != nil {
			res.Failed++
			res.Errors = append(res.Errors, fmt.Sprintf("line %d: write: %v", lineNum, err))
			continue
		}
		res.Parsed++
		if a.EDRRuleEngine != "" {
			res.EngineCounts[a.EDRRuleEngine]++
		}
		if a.RuleName != "" {
			res.RuleCounts[a.RuleName]++
		}
	}
	if err := scanner.Err(); err != nil {
		res.Errors = append(res.Errors, fmt.Sprintf("scanner: %v", err))
	}
	return res
}

// decodeLine handles ECS flat-key NDJSON. We pull into a raw map first so we
// can convert the timestamp string into time.Time and ignore unknown keys
// without complaint.
func decodeLine(line []byte) (*store.Alert, error) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(line, &raw); err != nil {
		return nil, fmt.Errorf("unmarshal: %w", err)
	}

	a := &store.Alert{}
	pull := func(key string, dst any) {
		if v, ok := raw[key]; ok {
			_ = json.Unmarshal(v, dst)
		}
	}

	var tsStr string
	pull("@timestamp", &tsStr)
	if tsStr == "" {
		return nil, fmt.Errorf("missing @timestamp")
	}
	t, err := time.Parse(time.RFC3339, tsStr)
	if err != nil {
		return nil, fmt.Errorf("parse timestamp %q: %w", tsStr, err)
	}
	a.Timestamp = t

	pull("ecs.version", &a.ECSVersion)
	pull("event.kind", &a.EventKind)
	pull("event.category", &a.EventCategory)
	pull("event.type", &a.EventType)
	pull("event.action", &a.EventAction)
	pull("event.code", &a.EventCode)
	pull("event.severity", &a.EventSeverity)
	pull("event.module", &a.EventModule)
	pull("event.dataset", &a.EventDataset)
	pull("event.provider", &a.EventProvider)
	pull("host.os.type", &a.HostOSType)
	pull("host.os.family", &a.HostOSFamily)
	pull("rule.name", &a.RuleName)
	pull("rule.description", &a.RuleDescription)
	pull("edr.rule.severity", &a.EDRRuleSeverity)
	pull("edr.rule.engine", &a.EDRRuleEngine)
	pull("process.executable", &a.ProcessExecutable)
	pull("process.name", &a.ProcessName)
	pull("process.command_line", &a.ProcessCommandLine)
	pull("process.pid", &a.ProcessPID)
	pull("process.working_directory", &a.ProcessWorkingDirectory)
	pull("process.parent.executable", &a.ProcessParentExecutable)
	pull("process.parent.name", &a.ProcessParentName)
	pull("process.parent.command_line", &a.ProcessParentCommandLine)
	pull("process.parent.pid", &a.ProcessParentPID)
	pull("user.name", &a.UserName)
	pull("related.user", &a.RelatedUser)

	return a, nil
}
