package store

import (
	"testing"
	"time"
)

// Test fixtures — one alert and three events with distinct, queryable
// shapes. Field values chosen so that substring / wildcard / enum
// behavior can each be isolated.

func qaAlert() Row {
	return Row{Kind: "a", Alert: &Alert{
		ID:                 1,
		Timestamp:          time.Date(2026, 5, 27, 19, 46, 53, 0, time.UTC),
		RuleName:           "File Deletion",
		EDRRuleSeverity:    "Low",
		EDRRuleEngine:      "Sigma",
		EventAction:        "process-start",
		ProcessPID:         442119,
		ProcessParentPID:   441942,
		ProcessName:        "rm",
		ProcessExecutable:  "/usr/bin/rm",
		ProcessCommandLine: "rm -rf /tmp/lesspipe.16813",
		UserName:           "mhristodor",
	}}
}

func qaProcEvent() Row {
	return Row{Kind: "e", Event: &Event{
		ID: 10, Category: "Process", EventID: 1, Opcode: 1,
		Timestamp: time.Date(2026, 5, 27, 19, 46, 50, 0, time.UTC),
		Fields: map[string]string{
			"Image":           "/usr/bin/bash",
			"CommandLine":     "bash /opt/run.sh --verbose",
			"ProcessId":       "442059",
			"ParentProcessId": "1",
			"User":            "root",
		},
	}}
}

func qaDnsEvent() Row {
	return Row{Kind: "e", Event: &Event{
		ID: 11, Category: "Dns", EventID: 22,
		Timestamp: time.Date(2026, 5, 27, 19, 46, 51, 0, time.UTC),
		Fields: map[string]string{
			"QueryName":  "telemetry.example.com",
			"RecordType": "A",
			"ProcessId":  "3123",
		},
	}}
}

func qaNetEvent() Row {
	return Row{Kind: "e", Event: &Event{
		ID: 12, Category: "Network", EventID: 3,
		Timestamp: time.Date(2026, 5, 27, 19, 46, 52, 0, time.UTC),
		Fields: map[string]string{
			"SourceIp": "192.168.1.17", "SourcePort": "43558",
			"DestinationIp": "8.8.8.8", "DestinationPort": "443",
			"Protocol": "tcp", "ProcessId": "2886",
		},
	}}
}

// TestParseQueryErrors locks the parser's error behavior: structurally
// broken input must error, empty input must mean "match everything".
func TestParseQueryErrors(t *testing.T) {
	cases := []struct {
		src     string
		wantErr bool
	}{
		{"", false},    // empty → nil expr, no error
		{"   ", false}, // whitespace-only → nil expr
		{"pid:442119", false},
		{"(pid:1", true},     // unbalanced paren
		{")", true},          // stray close paren
		{"pid:1 )", true},    // trailing close paren
		{"pid:", true},       // dangling colon, no value
		{"NOT", true},        // NOT with no operand
		{"a:1 AND", true},    // trailing operator
		{"dst_port:>", true}, // comparison operator with no number
	}
	for _, tc := range cases {
		expr, err := ParseQuery(tc.src)
		if tc.wantErr && err == nil {
			t.Errorf("ParseQuery(%q): expected error, got none (expr=%v)", tc.src, expr)
		}
		if !tc.wantErr && err != nil {
			t.Errorf("ParseQuery(%q): unexpected error: %v", tc.src, err)
		}
		if tc.src == "" && expr != nil {
			t.Errorf("ParseQuery(empty): expected nil expr, got %v", expr)
		}
	}
}

// TestEvalQuery is the core behavioral table: each entry parses a query
// and evaluates it against a fixture row. Covers field matching across
// both record kinds, wildcards, quoting, boolean operators, operator
// precedence, presence semantics, and bare-term substring search.
func TestEvalQuery(t *testing.T) {
	alert := qaAlert()
	proc := qaProcEvent()
	dns := qaDnsEvent()
	net := qaNetEvent()

	cases := []struct {
		name  string
		query string
		row   Row
		want  bool
	}{
		// --- unified pid across kinds ---
		{"pid matches alert", "pid:442119", alert, true},
		{"pid matches event", "pid:442059", proc, true},
		{"pid mismatch", "pid:999", proc, false},
		{"parent_pid alert", "parent_pid:441942", alert, true},
		{"parent_pid event", "parent_pid:1", proc, true},

		// --- kind discriminator ---
		{"kind alert", "kind:alert", alert, true},
		{"kind event", "kind:event", proc, true},
		{"kind mismatch", "kind:alert", proc, false},

		// --- case-insensitive string equality ---
		{"name CI match", "name:BASH", proc, true},
		{"name CI alert", "name:RM", alert, true},
		{"enum CI", "category:dns", dns, true},
		{"proto CI", "proto:TCP", net, true},

		// --- alert-only fields never match events ---
		{"severity on alert", "severity:Low", alert, true},
		{"severity on event", "severity:Low", proc, false},
		{"rule quoted", `rule:"File Deletion"`, alert, true},
		{"engine", "engine:Sigma", alert, true},

		// --- event-only fields never match alerts ---
		{"query on dns", "query:telemetry.example.com", dns, true},
		{"query on alert", "query:telemetry.example.com", alert, false},
		{"dst on net", "dst:8.8.8.8", net, true},
		{"dst_port", "dst_port:443", net, true},
		{"target only on file events", "target:/etc/passwd", proc, false},

		// --- presence semantics: field:* means "field present" ---
		{"presence on owner", "severity:*", alert, true},
		{"presence on non-owner", "severity:*", proc, false},
		{"absence via NOT", "NOT severity:*", proc, true},

		// --- wildcards ---
		{"prefix wildcard", "image:/usr/*", proc, true},
		{"suffix wildcard", "image:*bash", proc, true},
		{"contains wildcard", "image:*bin*", proc, true},
		{"multi-star ordered", "cmdline:*run.sh*verbose*", proc, true},
		{"multi-star wrong order", "cmdline:*verbose*run.sh*", proc, false},
		{"wildcard no match", "image:*zsh", proc, false},

		// --- quoting ---
		{"quoted value with spaces", `cmdline:"rm -rf /tmp/lesspipe.16813"`, alert, true},
		{"quoted bare term", `"rm -rf"`, alert, true},

		// --- boolean operators ---
		{"explicit AND both true", "pid:442059 AND name:bash", proc, true},
		{"explicit AND one false", "pid:442059 AND name:zsh", proc, false},
		{"implicit AND", "pid:442059 name:bash", proc, true},
		{"OR first true", "name:bash OR name:zsh", proc, true},
		{"OR second true", "name:zsh OR name:bash", proc, true},
		{"OR both false", "name:zsh OR name:fish", proc, false},
		{"NOT true operand", "NOT category:Dns", proc, true},
		{"NOT false operand", "NOT category:Dns", dns, false},
		{"lowercase operators", "name:bash and not category:Dns", proc, true},

		// --- precedence: AND binds tighter than OR ---
		// a OR b AND c  ==  a OR (b AND c)
		// row: proc (name=bash). a=false (name:zsh), b=true (name:bash),
		// c=false (pid:999) → false OR (true AND false) = false.
		{"precedence AND over OR", "name:zsh OR name:bash AND pid:999", proc, false},
		// With parens forcing (a OR b) AND c, c=true: (false OR true) AND true = true.
		{"parens override", "(name:zsh OR name:bash) AND pid:442059", proc, true},

		// --- numeric comparisons (net fixture: dst_port=443, src_port=43558) ---
		{"gt true", "dst_port:>100", net, true},
		{"gt false (equal)", "dst_port:>443", net, false},
		{"gte true (equal)", "dst_port:>=443", net, true},
		{"lt true", "dst_port:<1024", net, true},
		{"lt false", "src_port:<1024", net, false},
		{"lte boundary", "dst_port:<=443", net, true},
		{"comparison on non-numeric value", "name:>10", proc, false},
		{"comparison on absent field", "dst_port:>0", proc, false},
		{"quoted comparison is literal", `dst_port:">100"`, net, false},
		{"comparison composes with AND", "dst_port:>400 AND proto:tcp", net, true},

		// --- operator words as values ---
		{"value spelled AND", "name:AND", proc, false},

		// --- bare term substring across all fields ---
		{"bare term hits image", "bash", proc, true},
		{"bare term hits alert name", "lesspipe", alert, true},
		{"bare term miss", "kubernetes", proc, false},

		// --- nil expr matches everything ---
		{"empty query alert", "", alert, true},
		{"empty query event", "", dns, true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			expr, err := ParseQuery(tc.query)
			if err != nil {
				t.Fatalf("ParseQuery(%q): %v", tc.query, err)
			}
			if got := EvalQuery(expr, tc.row); got != tc.want {
				t.Errorf("EvalQuery(%q) on %s = %v, want %v", tc.query, tc.name, got, tc.want)
			}
		})
	}
}

// TestWildcardMatch pins the glob semantics directly — the evaluator
// tests above exercise it indirectly, but edge cases around empty
// segments deserve explicit coverage.
func TestWildcardMatch(t *testing.T) {
	cases := []struct {
		s, pat string
		want   bool
	}{
		{"abc", "abc", true},
		{"abc", "abd", false},
		{"abc", "*", true},
		{"", "*", true},
		{"abc", "a*", true},
		{"abc", "*c", true},
		{"abc", "*b*", true},
		{"abc", "a*c", true},
		{"abc", "a*b*c", true},
		{"abc", "c*a", false},
		{"aXbXc", "a*b*c", true},
	}
	for _, tc := range cases {
		if got := wildcardMatch(tc.s, tc.pat); got != tc.want {
			t.Errorf("wildcardMatch(%q, %q) = %v, want %v", tc.s, tc.pat, got, tc.want)
		}
	}
}
