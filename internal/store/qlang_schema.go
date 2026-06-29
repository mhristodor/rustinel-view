package store

import (
	"path"
	"strings"
)

// QLang is a tiny KQL-style query language layered on top of the
// existing per-row filters. A query is a boolean expression of
// field:value tests, optionally with bare-term substring searches,
// AND/OR/NOT, and parens. See the README's query-language section (or the
// embedded schema below) for the canonical fields.
//
// Fields are *unified* across alert and event records: a single
// canonical name like `pid` resolves to Alert.ProcessPID for alert
// rows and to Fields["ProcessId"] for event rows. This lets users
// type a query that filters both kinds with one expression.

// QField is the metadata for one canonical query field.
type QField struct {
	Name     string   // canonical name, e.g. "pid"
	Type     QType    // value type for autocomplete + parser hints
	Desc     string   // one-liner, surfaced in the UI
	Examples []string // example values for autocomplete
	Enum     []string // closed-set values, when applicable

	// extract returns the field's string value for this row plus an
	// applicable flag. If !applicable, the field doesn't exist for this
	// row kind; field:value tests for missing fields evaluate false.
	extract func(r Row) (val string, applicable bool)
}

// QType describes the value shape for a canonical field. Used by the
// frontend to drive autocomplete and value validation.
type QType string

const (
	QString QType = "string"
	QInt    QType = "int"
	QEnum   QType = "enum"
	QIP     QType = "ip"
)

// QSchema is the full canonical-field registry the parser + evaluator
// + autocomplete share. Order matters only for autocomplete display.
var QSchema = []QField{
	{
		Name: "kind", Type: QEnum, Desc: "record kind",
		Enum: []string{"alert", "event"},
		extract: func(r Row) (string, bool) {
			switch r.Kind {
			case "a":
				return "alert", true
			case "e":
				return "event", true
			}
			return "", false
		},
	},
	{
		Name: "pid", Type: QInt, Desc: "process pid", Examples: []string{"442059"},
		extract: func(r Row) (string, bool) {
			switch r.Kind {
			case "a":
				if r.Alert.ProcessPID == 0 {
					return "", false
				}
				return itoaq(r.Alert.ProcessPID), true
			case "e":
				v := r.Event.Fields["ProcessId"]
				return v, v != ""
			}
			return "", false
		},
	},
	{
		Name: "parent_pid", Type: QInt, Desc: "parent process pid",
		extract: func(r Row) (string, bool) {
			switch r.Kind {
			case "a":
				if r.Alert.ProcessParentPID == 0 {
					return "", false
				}
				return itoaq(r.Alert.ProcessParentPID), true
			case "e":
				v := r.Event.Fields["ParentProcessId"]
				return v, v != ""
			}
			return "", false
		},
	},
	{
		Name: "name", Type: QString, Desc: "process name (basename of image)",
		Examples: []string{"bash", "rm", "curl"},
		extract: func(r Row) (string, bool) {
			switch r.Kind {
			case "a":
				if r.Alert.ProcessName != "" {
					return r.Alert.ProcessName, true
				}
				return baseNameOf(r.Alert.ProcessExecutable), r.Alert.ProcessExecutable != ""
			case "e":
				img := r.Event.Fields["Image"]
				if img == "" {
					return "", false
				}
				return baseNameOf(img), true
			}
			return "", false
		},
	},
	{
		Name: "image", Type: QString, Desc: "process image (full path)",
		Examples: []string{"/usr/bin/bash", "/usr/bin/curl"},
		extract: func(r Row) (string, bool) {
			switch r.Kind {
			case "a":
				v := r.Alert.ProcessExecutable
				return v, v != ""
			case "e":
				v := r.Event.Fields["Image"]
				return v, v != ""
			}
			return "", false
		},
	},
	{
		Name: "parent_image", Type: QString, Desc: "parent process image",
		extract: func(r Row) (string, bool) {
			switch r.Kind {
			case "a":
				v := r.Alert.ProcessParentExecutable
				return v, v != ""
			case "e":
				v := r.Event.Fields["ParentImage"]
				return v, v != ""
			}
			return "", false
		},
	},
	{
		Name: "cmdline", Type: QString, Desc: "process command line",
		Examples: []string{`"rm -rf /tmp"`},
		extract: func(r Row) (string, bool) {
			switch r.Kind {
			case "a":
				v := r.Alert.ProcessCommandLine
				return v, v != ""
			case "e":
				v := r.Event.Fields["CommandLine"]
				return v, v != ""
			}
			return "", false
		},
	},
	{
		Name: "parent_cmdline", Type: QString, Desc: "parent process command line",
		extract: func(r Row) (string, bool) {
			switch r.Kind {
			case "a":
				v := r.Alert.ProcessParentCommandLine
				return v, v != ""
			case "e":
				v := r.Event.Fields["ParentCommandLine"]
				return v, v != ""
			}
			return "", false
		},
	},
	{
		Name: "user", Type: QString, Desc: "user that owns the process",
		Examples: []string{"root", "mhristodor"},
		extract: func(r Row) (string, bool) {
			switch r.Kind {
			case "a":
				v := r.Alert.UserName
				return v, v != ""
			case "e":
				v := r.Event.Fields["User"]
				return v, v != ""
			}
			return "", false
		},
	},
	{
		Name: "category", Type: QEnum, Desc: "event category (events only)",
		Enum: []string{"Process", "File", "Network", "Dns"},
		extract: func(r Row) (string, bool) {
			if r.Kind != "e" {
				return "", false
			}
			return r.Event.Category, r.Event.Category != ""
		},
	},
	{
		Name: "action", Type: QString, Desc: "canonical action name",
		Examples: []string{"PROCESS_CREATE", "FILE_DELETE", "DNS_QUERY"},
		extract: func(r Row) (string, bool) {
			switch r.Kind {
			case "a":
				v := r.Alert.EventAction
				return v, v != ""
			case "e":
				v := r.Event.Action()
				return v, v != ""
			}
			return "", false
		},
	},
	{
		Name: "severity", Type: QEnum, Desc: "alert severity (alerts only)",
		Enum: []string{"Critical", "High", "Medium", "Low", "Info"},
		extract: func(r Row) (string, bool) {
			if r.Kind != "a" {
				return "", false
			}
			return r.Alert.EDRRuleSeverity, r.Alert.EDRRuleSeverity != ""
		},
	},
	{
		Name: "rule", Type: QString, Desc: "alert rule name (alerts only)",
		extract: func(r Row) (string, bool) {
			if r.Kind != "a" {
				return "", false
			}
			return r.Alert.RuleName, r.Alert.RuleName != ""
		},
	},
	{
		Name: "engine", Type: QEnum, Desc: "detection engine (alerts only)",
		Enum: []string{"Sigma", "Yara"},
		extract: func(r Row) (string, bool) {
			if r.Kind != "a" {
				return "", false
			}
			return r.Alert.EDRRuleEngine, r.Alert.EDRRuleEngine != ""
		},
	},
	{
		Name: "target", Type: QString, Desc: "file target path (file events)",
		Examples: []string{"/etc/passwd"},
		extract: func(r Row) (string, bool) {
			if r.Kind != "e" || r.Event.Category != "File" {
				return "", false
			}
			v := r.Event.Fields["TargetFilename"]
			return v, v != ""
		},
	},
	{
		Name: "query", Type: QString, Desc: "DNS query name (DNS events)",
		Examples: []string{"google.com"},
		extract: func(r Row) (string, bool) {
			if r.Kind != "e" || r.Event.Category != "Dns" {
				return "", false
			}
			v := r.Event.Fields["QueryName"]
			return v, v != ""
		},
	},
	{
		Name: "record", Type: QEnum, Desc: "DNS record type (DNS events)",
		Enum: []string{"A", "AAAA", "MX", "TXT", "CNAME", "OTHER"},
		extract: func(r Row) (string, bool) {
			if r.Kind != "e" || r.Event.Category != "Dns" {
				return "", false
			}
			v := r.Event.Fields["RecordType"]
			return v, v != ""
		},
	},
	{
		Name: "src", Type: QIP, Desc: "source IP (network events)",
		extract: func(r Row) (string, bool) {
			if r.Kind != "e" || r.Event.Category != "Network" {
				return "", false
			}
			v := r.Event.Fields["SourceIp"]
			return v, v != ""
		},
	},
	{
		Name: "dst", Type: QIP, Desc: "destination IP (network events)",
		Examples: []string{"8.8.8.8"},
		extract: func(r Row) (string, bool) {
			if r.Kind != "e" || r.Event.Category != "Network" {
				return "", false
			}
			v := r.Event.Fields["DestinationIp"]
			return v, v != ""
		},
	},
	{
		Name: "src_port", Type: QInt, Desc: "source port (network events)",
		extract: func(r Row) (string, bool) {
			if r.Kind != "e" || r.Event.Category != "Network" {
				return "", false
			}
			v := r.Event.Fields["SourcePort"]
			return v, v != ""
		},
	},
	{
		Name: "dst_port", Type: QInt, Desc: "destination port (network events)",
		Examples: []string{"443", "80", "53"},
		extract: func(r Row) (string, bool) {
			if r.Kind != "e" || r.Event.Category != "Network" {
				return "", false
			}
			v := r.Event.Fields["DestinationPort"]
			return v, v != ""
		},
	},
	{
		Name: "proto", Type: QEnum, Desc: "network protocol (network events)",
		Enum: []string{"tcp", "udp", "icmp"},
		extract: func(r Row) (string, bool) {
			if r.Kind != "e" || r.Event.Category != "Network" {
				return "", false
			}
			return strings.ToLower(r.Event.Fields["Protocol"]), r.Event.Fields["Protocol"] != ""
		},
	},
}

// qSchemaByName returns the QField by canonical name or nil if unknown.
func qSchemaByName(name string) *QField {
	for i := range QSchema {
		if QSchema[i].Name == name {
			return &QSchema[i]
		}
	}
	return nil
}

// FieldValue extracts the canonical field's display value from a row.
// ok=false when the field is unknown or not applicable to the row's
// kind. Exposed for the column renderer, which lets users add any
// query-schema field as a timeline column.
func FieldValue(r Row, field string) (string, bool) {
	def := qSchemaByName(field)
	if def == nil || def.extract == nil {
		return "", false
	}
	return def.extract(r)
}

// IsQueryField reports whether the name is a canonical query-schema
// field. Used to validate user-supplied column lists.
func IsQueryField(name string) bool {
	return qSchemaByName(name) != nil
}

// baseNameOf returns the trailing basename of a slash-separated path.
// "/usr/bin/bash" → "bash". Plain "bash" → "bash". Used by name extractor.
func baseNameOf(p string) string {
	if p == "" {
		return ""
	}
	if !strings.Contains(p, "/") {
		return p
	}
	return path.Base(p)
}

// itoaq is a tiny helper used by qlang only — avoids an extra strconv
// import in this file.
func itoaq(n int) string {
	if n == 0 {
		return "0"
	}
	const digits = "0123456789"
	var buf [20]byte
	i := len(buf)
	neg := n < 0
	if neg {
		n = -n
	}
	for n > 0 {
		i--
		buf[i] = digits[n%10]
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
