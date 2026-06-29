package store

// DropGarbage filters out events that are pure sensor noise carrying no
// forensic signal. Alerts are never dropped — they are high-stakes
// records and any apparent malformation should still surface to the user.
//
// The rules are deliberately narrow and well-defined. This is not "smart"
// filtering — it is the removal of patterns that are unambiguous noise.
//
// Dropped:
//   - DNS events with QueryName "" or ".", regardless of RecordType
//   - DNS events with RecordType "OTHER" (sensor couldn't classify)
//   - Any event whose every meaningful field is empty
//
// Note: /dev/null File events are filtered earlier, at ingest
// (internal/ingest/event), so they never reach this layer. They also
// don't appear in the lineage view, which bypasses this toggle.
//
// Anything carrying even a single populated key field (Image, ProcessId,
// CommandLine, TargetFilename, QueryName, SourceIp, DestinationIp) is
// kept — the heuristic is intentionally conservative.
func DropGarbage(rows []Row) []Row {
	out := rows[:0]
	for _, r := range rows {
		if !isGarbage(r) {
			out = append(out, r)
		}
	}
	return out
}

func isGarbage(r Row) bool {
	if r.Kind != "e" || r.Event == nil {
		return false
	}
	e := r.Event

	if e.Category == "Dns" {
		q := e.Fields["QueryName"]
		if q == "" || q == "." {
			return true
		}
		if e.Fields["RecordType"] == "OTHER" {
			return true
		}
	}

	if e.Fields["Image"] == "" &&
		e.Fields["ProcessId"] == "" &&
		e.Fields["CommandLine"] == "" &&
		e.Fields["TargetFilename"] == "" &&
		e.Fields["QueryName"] == "" &&
		e.Fields["SourceIp"] == "" &&
		e.Fields["DestinationIp"] == "" {
		return true
	}

	return false
}
