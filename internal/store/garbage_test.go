package store

import (
	"testing"
	"time"
)

// /dev/null File events used to be dropped here; that rule moved to the
// ingest layer (internal/ingest/event) so the lineage view, which
// bypasses this toggle, also stops seeing them. Store-side rules now
// cover only categorical noise (empty fields, DNS edge cases). The
// always-keep test below remains the meaningful invariant.

// Alerts are never dropped — even one referencing /dev/null in any field
// must surface, because alert semantics are higher-stakes than event
// noise rules.
func TestDropGarbageNeverDropsAlerts(t *testing.T) {
	row := Row{
		Kind: "a",
		Alert: &Alert{
			Timestamp:          time.Now(),
			RuleName:           "weird /dev/null write",
			ProcessExecutable:  "/bin/dd",
			ProcessCommandLine: "dd if=/dev/zero of=/dev/null",
		},
	}
	if isGarbage(row) {
		t.Error("alert classified as garbage; alerts must never be dropped")
	}
}
