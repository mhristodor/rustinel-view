package store

import "time"

// Alert is the canonical in-memory representation of an ECS alert as
// produced by the rustinel pipeline. Field tags use ECS dot-notation so the
// struct can be JSON-marshalled directly into the storage layer and pulled
// back out by templates without an intermediate shape.
type Alert struct {
	ID         int64     `json:"id"`
	Timestamp  time.Time `json:"@timestamp"`
	ECSVersion string    `json:"ecs.version,omitempty"`

	EventKind     string   `json:"event.kind,omitempty"`
	EventCategory []string `json:"event.category,omitempty"`
	EventType     []string `json:"event.type,omitempty"`
	EventAction   string   `json:"event.action,omitempty"`
	EventCode     string   `json:"event.code,omitempty"`
	EventSeverity int      `json:"event.severity,omitempty"`
	EventModule   string   `json:"event.module,omitempty"`
	EventDataset  string   `json:"event.dataset,omitempty"`
	EventProvider string   `json:"event.provider,omitempty"`

	HostOSType   string `json:"host.os.type,omitempty"`
	HostOSFamily string `json:"host.os.family,omitempty"`

	RuleName        string `json:"rule.name,omitempty"`
	RuleDescription string `json:"rule.description,omitempty"`

	EDRRuleSeverity string `json:"edr.rule.severity,omitempty"`
	EDRRuleEngine   string `json:"edr.rule.engine,omitempty"`

	ProcessExecutable       string `json:"process.executable,omitempty"`
	ProcessName             string `json:"process.name,omitempty"`
	ProcessCommandLine      string `json:"process.command_line,omitempty"`
	ProcessPID              int    `json:"process.pid,omitempty"`
	ProcessWorkingDirectory string `json:"process.working_directory,omitempty"`

	ProcessParentExecutable  string `json:"process.parent.executable,omitempty"`
	ProcessParentName        string `json:"process.parent.name,omitempty"`
	ProcessParentCommandLine string `json:"process.parent.command_line,omitempty"`
	ProcessParentPID         int    `json:"process.parent.pid,omitempty"`

	UserName    string   `json:"user.name,omitempty"`
	RelatedUser []string `json:"related.user,omitempty"`
}

// Event is the canonical in-memory representation of an eBPF event lifted
// from a rustinel `normalized_json=` log line. Fields is intentionally a
// map[string]string — the keys vary by category (Dns, File, Network,
// Process) and we don't want a separate struct per category for an MVP.
type Event struct {
	ID        int64             `json:"id"`
	Timestamp time.Time         `json:"timestamp"`
	Platform  string            `json:"platform,omitempty"`
	Provider  string            `json:"provider,omitempty"`
	Category  string            `json:"category"`
	EventID   int               `json:"event_id"`
	Opcode    int               `json:"opcode"`
	Fields    map[string]string `json:"fields,omitempty"`
}

// Meta is the ingest summary written once at startup. Rendered into the
// header on every page load; never updated during the server's lifetime.
type Meta struct {
	AlertsParsed int       `json:"alerts_parsed"`
	AlertsFailed int       `json:"alerts_failed"`
	EventsParsed int       `json:"events_parsed"`
	EventsFailed int       `json:"events_failed"`
	StartedAt    time.Time `json:"started_at"`
	FinishedAt   time.Time `json:"finished_at"`
	IngestMillis int64     `json:"ingest_ms"`

	// Alert breakdown by detection engine (Sigma, Yara, ...)
	SigmaCount int `json:"sigma_count"`
	YaraCount  int `json:"yara_count"`

	// Event breakdown by eBPF category
	DnsCount     int `json:"dns_count"`
	FileCount    int `json:"file_count"`
	NetworkCount int `json:"network_count"`
	ProcessCount int `json:"process_count"`

	// Snapshot span: earliest and latest event timestamps observed.
	EarliestEvent time.Time `json:"earliest_event"`
	LatestEvent   time.Time `json:"latest_event"`

	// Most-triggered rule name across all alerts (for the "what's noisy"
	// header glance) plus the count.
	TopRule      string `json:"top_rule"`
	TopRuleCount int    `json:"top_rule_count"`
}

// Row carries one decoded record produced by a timeline query. Exactly one
// of Alert / Event is non-nil; Kind discriminates ("a" or "e"). Using a
// struct-with-pointers rather than a sealed interface so html/template can
// dispatch with {{if .Alert}} / {{if .Event}} without reflection helpers.
//
// Burst is set when the collapse pass folded one or more later rows into
// this row. The Alert/Event field carries the burst's head record (its
// first occurrence in time order); Burst carries the group statistics.
type Row struct {
	Kind  string
	Alert *Alert
	Event *Event
	Burst *Burst
}

// Burst summarises a group of adjacent same-key rows that the collapse
// pass folded into the row carrying it.
type Burst struct {
	Count     int           // including the head record
	Span      time.Duration // first .. last
	LastSeen  time.Time
	MemberIDs []int64 // first up to BurstMemberCap follow-on member IDs
}

// BurstMemberCap bounds the per-burst MemberIDs slice. The detail panel
// renders these; bursts beyond the cap render a "+N not shown" footer.
const BurstMemberCap = 50

// Timestamp returns the row's timestamp regardless of kind. Used for cursor
// math in the query layer.
func (r Row) Timestamp() time.Time {
	switch r.Kind {
	case "a":
		return r.Alert.Timestamp
	case "e":
		return r.Event.Timestamp
	}
	return time.Time{}
}

// Action returns the canonical action identifier for an event, mapping
// Sysmon-compatible (event_id, opcode) pairs to underscore-cased names.
// Used both by the filter matcher (single source of truth) and by the
// display helper (which formats it for the badge).
//
//	event_id  category  → action
//	1         Process   → PROCESS_CREATE
//	5         Process   → PROCESS_TERMINATE
//	3         Network   → NETWORK_CONNECT
//	11        File      → FILE_CREATE
//	23        File      → FILE_DELETE
//	65        File      → FILE_CHANGE
//	71        File      → FILE_RENAME
//	22        Dns       → DNS_QUERY
//
// Unknown event_ids fall back to opcode for process events (1→EXEC,
// 2→EXIT), then to the bare uppercase category.
func (e *Event) Action() string {
	switch e.Category {
	case "Process":
		switch e.EventID {
		case 1:
			return "PROCESS_CREATE"
		case 5:
			return "PROCESS_TERMINATE"
		}
		switch e.Opcode {
		case 1:
			return "PROCESS_EXEC"
		case 2:
			return "PROCESS_EXIT"
		}
		return "PROCESS"
	case "File":
		switch e.EventID {
		case 11:
			return "FILE_CREATE"
		case 23:
			return "FILE_DELETE"
		case 65:
			return "FILE_CHANGE"
		case 71:
			return "FILE_RENAME"
		}
		return "FILE"
	case "Network":
		if e.EventID == 3 {
			return "NETWORK_CONNECT"
		}
		return "NETWORK"
	case "Dns":
		if e.EventID == 22 {
			return "DNS_QUERY"
		}
		return "DNS"
	}
	if e.Category == "" {
		return "EVENT"
	}
	return ""
}

// Filters captures one timeline query. Empty slices / zero values mean "no
// constraint" so the zero Filters value returns everything.
type Filters struct {
	From, To      time.Time
	IncludeAlerts bool
	IncludeEvents bool
	Severities    []string // alerts only: Critical, High, Medium, Low, Info
	Categories    []string // events only: Dns, File, Network, Process
	Actions       []string // events only: PROCESS_CREATE, FILE_DELETE, …
	Filename      string   // case-insensitive substring targeting path-like fields (URL `fname` param; superseded by the query language's `target:` for UI use)
	Cursor        int64    // unix nanos; >= 0; 0 means "from beginning"
	Limit         int      // default 200, hard cap 1000
	Collapse      bool     // true → collapse same-key bursts at query time
	DropGarbage   bool     // true → drop sensor-noise events (DNS root queries, empty-field events)
	// QueryExpr is the parsed KQL-style query AST. Nil means no
	// language-level filtering; the existing chip-style filters (src,
	// cat, etc) still apply. QueryStr is the original source so the
	// URL persists round-trip.
	QueryExpr QExpr
	QueryStr  string
}

// Page is a single timeline page response.
type Page struct {
	Rows       []Row
	NextCursor int64 // unix nanos of last emitted row; 0 if Rows empty
	HasMore    bool
}
