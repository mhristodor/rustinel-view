package server

import (
	"encoding/json"
	"fmt"
	"html/template"
	"net/url"
	"strings"
	"time"

	"github.com/mhristodor/rustinel-view/internal/store"
)

// funcMap returns the template helpers used by every template. Helpers are
// pure projections from the store types into display strings; no I/O.
func funcMap() template.FuncMap {
	return template.FuncMap{
		"fmtTime":      fmtTime,
		"fmtTimeFull":  fmtTimeFull,
		"catClass":     catClass,
		"alertSummary": alertSummary,
		"eventSummary": eventSummary,
		"fieldEntries": fieldEntries,
		"prettyJSON":   prettyJSON,
		"hasContent":   hasContent,
		"loaderQuery":  loaderQuery,
		"counts":       fmtCounts,
		"fmtSpan":      fmtSpan,
		"eventAction":  eventAction,
		"fmtBurstSpan": fmtBurstSpan,
		"detailURL":    detailURL,
		"linSevClass":  linSevClass,
		"truncStr":     truncate,
		"dict":         dict,
		"lower":        strings.ToLower,
		"upper":        strings.ToUpper,
		"fieldValue":   fieldValue,
		"colLabel":     colLabel,
		"icon":         icon,
		"iconsJSON":    iconsJSON,
	}
}

// iconPaths is the inline SVG icon set. Paths lifted from Lucide v0.525
// (ISC license) — chosen because Unicode glyph buttons proved unfixable
// for centering: codepoints fall back to whatever OS font supplies the
// symbol, and the ink box sits at a different baseline per font. SVG
// with a fixed viewBox has deterministic geometry — render box equals
// bounding box, every time. 24x24 viewBox is the Lucide standard.
var iconPaths = map[string]string{
	"chevron-left":  `<path d="m15 18-6-6 6-6"/>`,
	"chevron-right": `<path d="m9 18 6-6-6-6"/>`,
	"chevron-down":  `<path d="m6 9 6 6 6-6"/>`,
	"x":             `<path d="M18 6 6 18"/><path d="m6 6 12 12"/>`,
	"plus":          `<path d="M5 12h14"/><path d="M12 5v14"/>`,
	"info":          `<circle cx="12" cy="12" r="10"/><path d="M12 16v-4"/><path d="M12 8h.01"/>`,
	"pivot":         `<path d="M7 7h10v10"/><path d="M7 17 17 7"/>`,
	"copy":          `<rect width="14" height="14" x="8" y="8" rx="2" ry="2"/><path d="M4 16c-1.1 0-2-.9-2-2V4c0-1.1.9-2 2-2h10c1.1 0 2 .9 2 2"/>`,
	"download":      `<path d="M21 15v4a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2v-4"/><polyline points="7 10 12 15 17 10"/><line x1="12" x2="12" y1="15" y2="3"/>`,
}

// icon renders one named inline SVG. Safe as template.HTML: the markup
// is assembled only from the compile-time constant set above.
func icon(name string) template.HTML {
	p, ok := iconPaths[name]
	if !ok {
		return ""
	}
	return template.HTML(svgOpen + p + svgClose)
}

const (
	svgOpen  = `<svg class="ico" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true">`
	svgClose = `</svg>`
)

// iconsJSON exposes the icon set as a JSON object of pre-rendered SVG
// strings so client-side rebuilders (rebuildThead, etc.) can emit the
// same markup the server does — one source of truth.
func iconsJSON() (template.JS, error) {
	m := make(map[string]string, len(iconPaths))
	for name := range iconPaths {
		m[name] = string(icon(name))
	}
	b, err := json.Marshal(m)
	if err != nil {
		return "", err
	}
	return template.JS(b), nil
}

// fieldValue extracts a canonical query-schema field's display value
// from a row ("" when unknown or not applicable). Drives user-added
// timeline columns.
func fieldValue(r store.Row, field string) string {
	v, _ := store.FieldValue(r, field)
	return v
}

// colLabel maps a column id to its header label. CSS uppercases, so
// only word separation matters here.
func colLabel(c string) string {
	switch c {
	case "time", "kind", "process", "summary":
		return c
	}
	return strings.ReplaceAll(c, "_", " ")
}

// dict builds a map from alternating key/value template args so a
// recursive sub-template can be called with multiple named parameters
// in one line.
func dict(values ...any) (map[string]any, error) {
	if len(values)%2 != 0 {
		return nil, fmt.Errorf("dict needs even arg count, got %d", len(values))
	}
	d := make(map[string]any, len(values)/2)
	for i := 0; i < len(values); i += 2 {
		key, ok := values[i].(string)
		if !ok {
			return nil, fmt.Errorf("dict key #%d not a string", i)
		}
		d[key] = values[i+1]
	}
	return d, nil
}

// linSevClass maps a severity string ("Critical", "High", ...) to the
// CSS modifier used by .lin-node — separated from sevClass because the
// lineage page uses a slightly different palette modifier naming.
func linSevClass(s string) string {
	switch strings.ToLower(s) {
	case "critical":
		return "sev-critical"
	case "high":
		return "sev-high"
	case "medium":
		return "sev-medium"
	case "low":
		return "sev-low"
	case "info":
		return "sev-info"
	}
	return ""
}

// eventAction returns the display-formatted action name for an event
// (spaces, not underscores) — the canonical id form lives on Event.Action()
// in the store package so filter and display use one source of truth.
func eventAction(e *store.Event) string {
	id := e.Action()
	if id == "" {
		return strings.ToUpper(e.Category)
	}
	return strings.ReplaceAll(id, "_", " ")
}

// fmtSpan renders the snapshot span between two timestamps in compact form
// (e.g., "4m 23s", "2h 5m", "1d 3h"). Zero values fall back to "—".
func fmtSpan(start, end any) string {
	a, oka := timeValue(start)
	b, okb := timeValue(end)
	if !oka || !okb {
		return "—"
	}
	d := b.Sub(a)
	if d <= 0 {
		return "0s"
	}
	if d < 60*1_000_000_000 { // <60s
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	if d < 3600*1_000_000_000 { // <1h
		m := int(d.Minutes())
		s := int(d.Seconds()) - m*60
		if s == 0 {
			return fmt.Sprintf("%dm", m)
		}
		return fmt.Sprintf("%dm %ds", m, s)
	}
	if d.Hours() < 24 {
		h := int(d.Hours())
		m := int(d.Minutes()) - h*60
		if m == 0 {
			return fmt.Sprintf("%dh", h)
		}
		return fmt.Sprintf("%dh %dm", h, m)
	}
	days := int(d.Hours() / 24)
	hrs := int(d.Hours()) - days*24
	return fmt.Sprintf("%dd %dh", days, hrs)
}

// fmtTime renders the HH:MM:SS.mmm time portion for the table.
func fmtTime(t any) string {
	tt, ok := timeValue(t)
	if !ok {
		return "—"
	}
	return tt.Format("15:04:05.000")
}

// fmtTimeFull renders date+time for the detail panel.
func fmtTimeFull(t any) string {
	tt, ok := timeValue(t)
	if !ok {
		return "—"
	}
	return tt.Format("2006-01-02 15:04:05.000 MST")
}

func catClass(c string) string {
	switch strings.ToLower(c) {
	case "process":
		return "cat-process"
	case "file":
		return "cat-file"
	case "network":
		return "cat-network"
	case "dns":
		return "cat-dns"
	}
	return ""
}

// alertSummary projects the alert into the table's Summary cell.
func alertSummary(a *store.Alert) string {
	if a.RuleName != "" {
		return a.RuleName
	}
	if a.EventAction != "" {
		return a.EventAction
	}
	return "(no rule)"
}

// eventSummary projects an event into the table's Summary cell. The
// projection is category-specific so the row reads meaningfully.
func eventSummary(e *store.Event) string {
	switch strings.ToLower(e.Category) {
	case "dns":
		q := e.Fields["QueryName"]
		rt := e.Fields["RecordType"]
		if q == "" {
			q = "—"
		}
		if rt != "" {
			return fmt.Sprintf("%s (%s)", q, rt)
		}
		return q
	case "network":
		src, sp := e.Fields["SourceIp"], e.Fields["SourcePort"]
		dst, dp := e.Fields["DestinationIp"], e.Fields["DestinationPort"]
		proto := strings.ToUpper(e.Fields["Protocol"])
		return fmt.Sprintf("%s:%s → %s:%s %s",
			truncate(src, 28), sp, truncate(dst, 28), dp, proto)
	case "file":
		return e.Fields["TargetFilename"]
	case "process":
		if cl := e.Fields["CommandLine"]; cl != "" {
			return cl
		}
		return e.Fields["Image"]
	}
	return e.Fields["Image"]
}

// fieldEntries returns the event's Fields map as a sorted slice of
// {Key,Value} pairs for the detail panel.
func fieldEntries(m map[string]string) []kv {
	out := make([]kv, 0, len(m))
	for k, v := range m {
		out = append(out, kv{K: k, V: v})
	}
	// stable order: known keys first, then alphabetical
	weight := func(k string) int {
		switch k {
		case "ProcessId":
			return 0
		case "ParentProcessId":
			return 1
		case "Image":
			return 2
		case "ParentImage":
			return 3
		case "CommandLine":
			return 4
		case "ParentCommandLine":
			return 5
		case "CurrentDirectory":
			return 6
		case "User":
			return 7
		}
		return 99
	}
	// insertion sort — slice is tiny (≤10 elements typically)
	for i := 1; i < len(out); i++ {
		for j := i; j > 0; j-- {
			a, b := out[j-1], out[j]
			if weight(a.K) > weight(b.K) ||
				(weight(a.K) == weight(b.K) && a.K > b.K) {
				out[j-1], out[j] = b, a
			}
		}
	}
	return out
}

type kv struct{ K, V string }

// prettyJSON renders a struct as indented JSON for the "Copy JSON" block.
func prettyJSON(v any) string {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Sprintf("%+v", v)
	}
	return string(b)
}

// hasContent is used in templates to test whether to render optional fields.
func hasContent(s any) bool {
	switch v := s.(type) {
	case string:
		return v != ""
	case int:
		return v != 0
	case []string:
		return len(v) > 0
	}
	return false
}

// loaderQuery builds the URL the loader row HTMX-gets for the next page.
// Only carries the cursor — hx-include picks up the rest from the filter
// form on each fire.
func loaderQuery(cursor int64) template.URL {
	v := url.Values{}
	v.Set("cursor", itoa64(cursor))
	return template.URL("/timeline?" + v.Encode())
}

// fmtCounts is a small helper that formats integer counts with thousand
// separators so the header reads "482,109" not "482109".
func fmtCounts(n int) string {
	if n < 0 {
		return "-" + fmtCounts(-n)
	}
	if n < 1000 {
		return itoa(n)
	}
	return fmtCounts(n/1000) + "," + zeroPad(n%1000)
}

func zeroPad(n int) string {
	s := itoa(n)
	for len(s) < 3 {
		s = "0" + s
	}
	return s
}

// truncate cuts a string to n runes with a trailing ellipsis.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}

// fmtBurstSpan formats a burst duration for the row chip ("3.2s", "850ms",
// "1m 12s"). Picks the unit that reads cleanest.
func fmtBurstSpan(d time.Duration) string {
	if d <= 0 {
		return "0"
	}
	if d < time.Second {
		return fmt.Sprintf("%dms", d.Milliseconds())
	}
	if d < time.Minute {
		return fmt.Sprintf("%.1fs", d.Seconds())
	}
	m := int(d.Minutes())
	s := int(d.Seconds()) - m*60
	if s == 0 {
		return fmt.Sprintf("%dm", m)
	}
	return fmt.Sprintf("%dm %ds", m, s)
}

// detailURL produces the click-target URL for a row. For a burst row, it
// encodes the burst's MemberIDs as a `members=` param so the detail
// handler can render the underlying records as a stacked list.
func detailURL(r store.Row) template.URL {
	var idStr string
	switch r.Kind {
	case "a":
		idStr = itoa64(r.Alert.ID)
	case "e":
		idStr = itoa64(r.Event.ID)
	default:
		return ""
	}
	base := "/detail/" + r.Kind + "/" + idStr
	if r.Burst == nil || len(r.Burst.MemberIDs) == 0 {
		return template.URL(base)
	}
	v := url.Values{}
	parts := make([]string, len(r.Burst.MemberIDs))
	for i, id := range r.Burst.MemberIDs {
		parts[i] = itoa64(id)
	}
	v.Set("members", strings.Join(parts, ","))
	v.Set("count", itoa(r.Burst.Count))
	v.Set("span", fmtBurstSpan(r.Burst.Span))
	return template.URL(base + "?" + v.Encode())
}
