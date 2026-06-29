package store

import (
	"context"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Caps protecting the lineage builder from runaway snapshots. No
// per-event cap — every event under every process is rendered.
const (
	// LineageMaxDepth bounds how deep the descendant BFS walks. Beyond
	// this depth, the user can re-pivot to a deeper node to drill further.
	LineageMaxDepth = 8

	// LineageMaxNodes is the total-node ceiling on the rendered tree.
	// Once reached, further descendants are dropped and the affected
	// node's ChildrenTruncated count records how many children were left
	// out.
	LineageMaxNodes = 300
)

// LineageView is the payload assembled by BuildLineageFor and rendered
// by the lineage page template.
type LineageView struct {
	OriginPID    int
	OriginExists bool
	Parents      []*LinNode
	Origin       *LinNode
	BuildMillis  int64
	Truncated    bool
	NodeCount    int
}

// LinNode is one process in the lineage tree.
type LinNode struct {
	PID         int
	Image       string
	ProcessName string
	User        string
	CommandLine string
	ParentPID   int
	FirstSeen   time.Time
	LastSeen    time.Time
	IsOrigin    bool // true on the pivot's anchor process; used for visual highlighting
	IsAncestor  bool // true on parent-chain nodes above the origin; styles the spine
	Depth       int  // 0 at the rendered root (topmost ancestor), +1 per level down

	// Per-category event aggregates. No cap is applied — Events holds
	// every event in the category and EventCounts mirrors len().
	EventCounts map[string]int
	Events      map[string][]LinEvent

	// Alerts attached to this PID — always uncapped. MaxAlertSev drives
	// the node's visual loudness.
	Alerts      []LinAlert
	AlertCount  int
	MaxAlertSev string

	Children          []*LinNode
	ChildrenTruncated int
}

// LinEvent is one event inlined under its parent process.
type LinEvent struct {
	ID        int64
	Timestamp time.Time
	Category  string
	Action    string
	Summary   string
}

// LinAlert is one alert attached to a process node.
type LinAlert struct {
	ID        int64
	Timestamp time.Time
	RuleName  string
	Severity  string
	Engine    string
}

// BuildLineageFor assembles a pivot-rooted lineage tree for the given
// origin PID. The result includes:
//   - the full parent chain (single line, no siblings) upward to the
//     root or until a parent PID is no longer present in the snapshot;
//   - the origin process itself;
//   - the complete descendant subtree, bounded by LineageMaxDepth and
//     LineageMaxNodes.
//
// All rows are read in one pass; build cost is roughly O(N) for indexing
// plus O(visited × records-per-PID) for population.
func (s *Store) BuildLineageFor(ctx context.Context, originPID int) (LineageView, error) {
	start := time.Now()
	view := LineageView{OriginPID: originPID}

	// Served from the materialized snapshot — no Redis round trips, no
	// JSON decode. The remaining cost is the PID indexing + tree
	// assembly, linear in dataset size.
	rows, err := s.materializedRows(ctx)
	if err != nil {
		return view, err
	}

	byPID := map[int][]int{}       // pid → row index list
	byParent := map[int][]int{}    // ppid → child pid list (de-duped via seen)
	parentSeen := map[int64]bool{} // edge dedup: (ppid<<32)|pid

	for i, r := range rows {
		pid := pidOf(r)
		if pid <= 0 {
			continue
		}
		byPID[pid] = append(byPID[pid], i)
		ppid := parentPIDOf(r)
		if ppid <= 0 || ppid == pid {
			continue
		}
		edge := (int64(ppid) << 32) | int64(pid)
		if parentSeen[edge] {
			continue
		}
		parentSeen[edge] = true
		byParent[ppid] = append(byParent[ppid], pid)
	}

	// Build origin first; we always render even if there are no rows so
	// the user gets a navigable empty state instead of a 404.
	view.Origin = buildLinNode(originPID, byPID[originPID], rows)
	view.OriginExists = len(byPID[originPID]) > 0

	// Upward walk — pure chain.
	cur := originPID
	chainSeen := map[int]bool{cur: true}
	parents := []*LinNode{}
	for {
		ppid := mostRecentParentPID(byPID[cur], rows)
		if ppid <= 0 || ppid == cur || chainSeen[ppid] {
			break
		}
		chainSeen[ppid] = true
		parent := buildLinNode(ppid, byPID[ppid], rows)
		parent.IsAncestor = true
		parents = append([]*LinNode{parent}, parents...)
		cur = ppid
	}
	view.Parents = parents

	// Descendant BFS with depth + node-count caps.
	type queueItem struct {
		node  *LinNode
		depth int
	}
	totalNodes := 1 + len(parents) // origin + parents already counted toward cap
	queue := []queueItem{{node: view.Origin, depth: 0}}
	// Avoid revisiting PIDs we've already placed in the tree.
	placed := map[int]bool{originPID: true}
	for _, p := range parents {
		placed[p.PID] = true
	}

	for len(queue) > 0 {
		head := queue[0]
		queue = queue[1:]
		if head.depth >= LineageMaxDepth {
			head.node.ChildrenTruncated += len(byParent[head.node.PID])
			view.Truncated = view.Truncated || head.node.ChildrenTruncated > 0
			continue
		}
		// Sort children by PID for deterministic ordering.
		childPIDs := append([]int(nil), byParent[head.node.PID]...)
		sort.Ints(childPIDs)
		for _, childPID := range childPIDs {
			if placed[childPID] {
				continue
			}
			if totalNodes >= LineageMaxNodes {
				head.node.ChildrenTruncated++
				view.Truncated = true
				continue
			}
			placed[childPID] = true
			child := buildLinNode(childPID, byPID[childPID], rows)
			head.node.Children = append(head.node.Children, child)
			totalNodes++
			queue = append(queue, queueItem{node: child, depth: head.depth + 1})
		}
	}

	view.NodeCount = totalNodes
	view.BuildMillis = time.Since(start).Milliseconds()

	// Stitch the parent chain into the Children graph so the template
	// walks a single connected tree from the topmost ancestor down to
	// every descendant. parents[0] becomes the visual root; each parent
	// has its successor as its sole child; the deepest parent points at
	// origin which already has its descendant subtree.
	view.Origin.IsOrigin = true
	if len(view.Parents) > 0 {
		for i := 0; i < len(view.Parents)-1; i++ {
			view.Parents[i].Children = []*LinNode{view.Parents[i+1]}
		}
		view.Parents[len(view.Parents)-1].Children = []*LinNode{view.Origin}
	}

	root := view.Origin
	if len(view.Parents) > 0 {
		root = view.Parents[0]
	}
	assignLinDepth(root, 0)
	return view, nil
}

// assignLinDepth walks the stitched tree top-down, recording each node's
// distance from the rendered root. The template exposes it as a CSS var
// so rails can shift hue per level. Bounded by the stitched chain length
// plus LineageMaxDepth, so recursion is safe.
func assignLinDepth(n *LinNode, d int) {
	n.Depth = d
	for _, c := range n.Children {
		assignLinDepth(c, d+1)
	}
}

// mostRecentParentPID returns the most-recently-observed ParentProcessId
// across the supplied row indexes. "Most recent" wins over "first seen"
// to handle PID reuse where the latest known parent is the one that
// matters for the chain.
func mostRecentParentPID(rowIdxs []int, rows []Row) int {
	if len(rowIdxs) == 0 {
		return 0
	}
	var pickedPPID int
	var pickedTs time.Time
	for _, idx := range rowIdxs {
		ppid := parentPIDOf(rows[idx])
		if ppid <= 0 {
			continue
		}
		ts := rows[idx].Timestamp()
		if pickedPPID == 0 || ts.After(pickedTs) {
			pickedPPID = ppid
			pickedTs = ts
		}
	}
	return pickedPPID
}

// buildLinNode walks every row belonging to this PID and assembles the
// LinNode. Cheap fields (Image, ProcessName, CommandLine, User) are
// taken from the most-informative event (longest CommandLine wins as
// a heuristic); time bounds come from FirstSeen/LastSeen across all
// rows.
func buildLinNode(pid int, rowIdxs []int, rows []Row) *LinNode {
	n := &LinNode{
		PID:         pid,
		EventCounts: map[string]int{},
		Events:      map[string][]LinEvent{},
	}
	if len(rowIdxs) == 0 {
		return n
	}
	for _, idx := range rowIdxs {
		r := rows[idx]
		ts := r.Timestamp()
		if n.FirstSeen.IsZero() || ts.Before(n.FirstSeen) {
			n.FirstSeen = ts
		}
		if ts.After(n.LastSeen) {
			n.LastSeen = ts
		}
		if n.ParentPID == 0 {
			if pp := parentPIDOf(r); pp > 0 && pp != pid {
				n.ParentPID = pp
			}
		}
		applyToLinNode(n, r)
	}
	return n
}

func applyToLinNode(n *LinNode, r Row) {
	switch r.Kind {
	case "a":
		a := r.Alert
		if a == nil {
			return
		}
		if n.Image == "" && a.ProcessExecutable != "" {
			n.Image = a.ProcessExecutable
		}
		if n.ProcessName == "" && a.ProcessName != "" {
			n.ProcessName = a.ProcessName
		}
		if n.User == "" && a.UserName != "" {
			n.User = a.UserName
		}
		if a.ProcessCommandLine != "" && len(a.ProcessCommandLine) > len(n.CommandLine) {
			n.CommandLine = a.ProcessCommandLine
		}
		n.AlertCount++
		n.Alerts = append(n.Alerts, LinAlert{
			ID:        a.ID,
			Timestamp: a.Timestamp,
			RuleName:  a.RuleName,
			Severity:  a.EDRRuleSeverity,
			Engine:    a.EDRRuleEngine,
		})
		if linSeverityRank(a.EDRRuleSeverity) > linSeverityRank(n.MaxAlertSev) {
			n.MaxAlertSev = a.EDRRuleSeverity
		}
	case "e":
		e := r.Event
		if e == nil {
			return
		}
		if img := e.Fields["Image"]; img != "" {
			n.Image = img
			if n.ProcessName == "" {
				n.ProcessName = linBaseName(img)
			}
		}
		if cl := e.Fields["CommandLine"]; cl != "" && len(cl) > len(n.CommandLine) {
			n.CommandLine = cl
		}
		if u := e.Fields["User"]; u != "" && n.User == "" {
			n.User = u
		}
		cat := e.Category
		n.EventCounts[cat]++
		// Lineage shows every event under every process — no per-
		// category cap. Caller can pivot to a child if the page gets
		// too dense.
		n.Events[cat] = append(n.Events[cat], LinEvent{
			ID:        e.ID,
			Timestamp: e.Timestamp,
			Category:  cat,
			Action:    e.Action(),
			Summary:   linEventSummary(e),
		})
	}
}

func linSeverityRank(s string) int {
	switch strings.ToLower(s) {
	case "critical":
		return 5
	case "high":
		return 4
	case "medium":
		return 3
	case "low":
		return 2
	case "info":
		return 1
	}
	return 0
}

func linBaseName(p string) string {
	if p == "" {
		return ""
	}
	if !strings.Contains(p, "/") {
		return p
	}
	return path.Base(p)
}

// linEventSummary mirrors the timeline-side eventSummary projection so
// inline event lines under a process node read identically to their
// timeline counterparts.
func linEventSummary(e *Event) string {
	switch e.Category {
	case "Dns":
		q := e.Fields["QueryName"]
		rt := e.Fields["RecordType"]
		if rt != "" && q != "" {
			return q + " (" + rt + ")"
		}
		return q
	case "Network":
		return e.Fields["SourceIp"] + ":" + e.Fields["SourcePort"] +
			" → " + e.Fields["DestinationIp"] + ":" + e.Fields["DestinationPort"] +
			" " + strings.ToUpper(e.Fields["Protocol"])
	case "File":
		return e.Fields["TargetFilename"]
	case "Process":
		if cl := e.Fields["CommandLine"]; cl != "" {
			return cl
		}
		return e.Fields["Image"]
	}
	return e.Fields["Image"]
}

// pidOf returns the row's identifying PID (the process the event/alert
// is about).
func pidOf(r Row) int {
	switch r.Kind {
	case "a":
		if r.Alert == nil {
			return 0
		}
		return r.Alert.ProcessPID
	case "e":
		if r.Event == nil {
			return 0
		}
		p, _ := strconv.Atoi(r.Event.Fields["ProcessId"])
		return p
	}
	return 0
}

func parentPIDOf(r Row) int {
	switch r.Kind {
	case "a":
		if r.Alert == nil {
			return 0
		}
		return r.Alert.ProcessParentPID
	case "e":
		if r.Event == nil {
			return 0
		}
		p, _ := strconv.Atoi(r.Event.Fields["ParentProcessId"])
		return p
	}
	return 0
}
