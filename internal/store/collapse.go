package store

import (
	"path"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Time windows per row category. Picked to match the temporal scale of the
// noise each category exhibits in real snapshots: alert detections cluster
// over ~tens of seconds; DNS/network chatter bursts over a few seconds;
// file/process bursts are sub-second to a couple of seconds.
const (
	windowAlert   = 10 * time.Second
	windowDns     = 5 * time.Second
	windowNetwork = 5 * time.Second
	windowFile    = 2 * time.Second
	windowProcess = 2 * time.Second
)

// CollapseRows folds adjacent rows that share a per-category collapse key
// within that category's time window into single rows carrying a Burst.
//
// Input must be time-ordered (the timeline query already guarantees this).
// Output is also time-ordered and may contain a mix of single rows
// (Burst == nil) and burst rows (Burst != nil; Burst.Count >= 2).
//
// The pass is pure: it never mutates the underlying Alert/Event records,
// only the Burst pointer on the wrapping Row.
func CollapseRows(rows []Row) []Row {
	if len(rows) < 2 {
		return rows
	}

	open := map[string]*pendingBurst{}
	out := make([]Row, 0, len(rows))

	for i := range rows {
		r := rows[i]
		key, w := collapseKey(r), windowFor(r)
		// Empty key → never collapsible (e.g. a row missing all key fields).
		if key == "" {
			flushExpired(&out, open, r.Timestamp(), 0)
			out = append(out, r)
			continue
		}

		if b, ok := open[key]; ok && r.Timestamp().Sub(b.LastSeen) <= w {
			b.Count++
			b.LastSeen = r.Timestamp()
			if len(b.MemberIDs) < BurstMemberCap {
				b.MemberIDs = append(b.MemberIDs, idOf(r))
			}
			continue
		}

		// New row breaks any existing burst with this same key, plus any
		// burst whose window has now expired against r.Timestamp.
		flushExpired(&out, open, r.Timestamp(), 0)
		if b, ok := open[key]; ok {
			out = append(out, finalizeBurst(b))
			delete(open, key)
		}
		open[key] = &pendingBurst{
			HeadRow:  r,
			Count:    1,
			LastSeen: r.Timestamp(),
			Window:   w,
		}
	}

	// End of input: flush all remaining open bursts in time order.
	flushExpired(&out, open, time.Time{}, finalFlushMarker)

	return out
}

type pendingBurst struct {
	HeadRow   Row
	Count     int
	LastSeen  time.Time
	Window    time.Duration
	MemberIDs []int64
}

const finalFlushMarker = 1 // sentinel: forces all bursts to flush regardless of window

// flushExpired emits — in HeadRow.Timestamp order — every open burst that
// has either timed out against `now` or is being forced out by the
// finalFlushMarker. Time-ordered emission keeps the output stream
// time-monotonic, matching the input invariant.
func flushExpired(out *[]Row, open map[string]*pendingBurst, now time.Time, force int) {
	if len(open) == 0 {
		return
	}
	var expired []*pendingBurst
	var expiredKeys []string
	for k, b := range open {
		if force == finalFlushMarker || now.Sub(b.LastSeen) > b.Window {
			expired = append(expired, b)
			expiredKeys = append(expiredKeys, k)
		}
	}
	if len(expired) == 0 {
		return
	}
	sort.SliceStable(expired, func(i, j int) bool {
		return expired[i].HeadRow.Timestamp().Before(expired[j].HeadRow.Timestamp())
	})
	for _, b := range expired {
		*out = append(*out, finalizeBurst(b))
	}
	for _, k := range expiredKeys {
		delete(open, k)
	}
}

// finalizeBurst converts a pendingBurst into the output Row. If the burst
// has Count == 1 we return the head row untouched (no Burst attached, so
// templates render it as a plain row). For Count >= 2 we attach the Burst
// summary.
func finalizeBurst(b *pendingBurst) Row {
	if b.Count <= 1 {
		return b.HeadRow
	}
	r := b.HeadRow
	r.Burst = &Burst{
		Count:     b.Count,
		Span:      b.LastSeen.Sub(b.HeadRow.Timestamp()),
		LastSeen:  b.LastSeen,
		MemberIDs: b.MemberIDs,
	}
	return r
}

func idOf(r Row) int64 {
	switch r.Kind {
	case "a":
		return r.Alert.ID
	case "e":
		return r.Event.ID
	}
	return 0
}

func windowFor(r Row) time.Duration {
	switch r.Kind {
	case "a":
		return windowAlert
	case "e":
		switch r.Event.Category {
		case "Dns":
			return windowDns
		case "Network":
			return windowNetwork
		case "File":
			return windowFile
		case "Process":
			return windowProcess
		}
	}
	return windowAlert
}

// collapseKey computes the equality key used to decide whether two rows
// can fold into the same burst. The key is a single string so we can use
// it as a map key cheaply; the field order is fixed per category and
// every separator (\x1f, ASCII unit separator) is a character that can
// never legitimately appear in any of the captured fields.
func collapseKey(r Row) string {
	const sep = "\x1f"
	switch r.Kind {
	case "a":
		a := r.Alert
		if a.RuleName == "" {
			return ""
		}
		return "a" + sep + a.RuleName + sep + strconv.Itoa(a.ProcessPID)
	case "e":
		e := r.Event
		switch e.Category {
		case "Dns":
			q, rt := e.Fields["QueryName"], e.Fields["RecordType"]
			if q == "" && rt == "" {
				return ""
			}
			return "e/d" + sep + q + sep + rt
		case "Network":
			sip, sp := e.Fields["SourceIp"], e.Fields["SourcePort"]
			dip, dp := e.Fields["DestinationIp"], e.Fields["DestinationPort"]
			proto := e.Fields["Protocol"]
			if sip == "" && dip == "" {
				return ""
			}
			return "e/n" + sep + sip + sep + sp + sep + dip + sep + dp + sep + proto
		case "File":
			img, pid := e.Fields["Image"], e.Fields["ProcessId"]
			act := e.Action()
			parent := parentDir(e.Fields["TargetFilename"])
			if img == "" && pid == "" && parent == "" {
				return ""
			}
			return "e/f" + sep + img + sep + pid + sep + act + sep + parent
		case "Process":
			img, cmd := e.Fields["Image"], e.Fields["CommandLine"]
			act := e.Action()
			if img == "" && cmd == "" {
				return ""
			}
			return "e/p" + sep + img + sep + cmd + sep + act
		}
	}
	return ""
}

// parentDir returns the directory portion of a TargetFilename. Used as
// part of the File collapse key so several writes into the same directory
// in close succession collapse, but writes that span directories do not.
func parentDir(p string) string {
	p = strings.TrimSpace(p)
	if p == "" {
		return ""
	}
	d := path.Dir(p)
	if d == "." || d == "/" {
		return d
	}
	return d
}
