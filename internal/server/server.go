package server

import (
	"context"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/mhristodor/rustinel-view/internal/store"
)

//go:embed templates static
var assets embed.FS

// Server bundles the HTTP handlers and template set. Construct with New;
// run with ListenAndServe.
type Server struct {
	store    *store.Store
	mux      *http.ServeMux
	tmpl     *template.Template
	log      *slog.Logger
	assetVer string // content hash of embedded static assets — cache-busting query param
}

// New constructs a Server. Parses every template under templates/ and wires
// the routes. Any template parse error panics — these are bundled assets,
// not user input.
func New(s *store.Store, log *slog.Logger) *Server {
	t := template.New("").Funcs(funcMap())
	tmpl, err := t.ParseFS(assets, "templates/*.html")
	if err != nil {
		panic(fmt.Errorf("parse templates: %w", err))
	}

	srv := &Server{
		store:    s,
		mux:      http.NewServeMux(),
		tmpl:     tmpl,
		log:      log,
		assetVer: assetVersion(),
	}

	srv.mux.HandleFunc("GET /", srv.handlePage)
	srv.mux.HandleFunc("GET /timeline", srv.handleTimeline)
	srv.mux.HandleFunc("GET /count", srv.handleCount)
	srv.mux.HandleFunc("GET /density", srv.handleDensity)
	srv.mux.HandleFunc("GET /export", srv.handleExport)
	srv.mux.HandleFunc("GET /lineage/{pid}", srv.handleLineage)
	srv.mux.HandleFunc("GET /json/{kind}/{id}", srv.handleRecordJSON)
	srv.mux.HandleFunc("GET /query/schema", srv.handleQuerySchema)
	srv.mux.HandleFunc("GET /query/check", srv.handleQueryCheck)
	srv.mux.HandleFunc("GET /query/values", srv.handleQueryValues)
	srv.mux.HandleFunc("GET /detail/{kind}/{id}", srv.handleDetail)
	srv.mux.Handle("GET /static/", srv.staticHandler())

	return srv
}

// assetVersion returns a short content hash of the embedded JS + CSS.
// Templates append it as ?v=<hash> to static URLs, so browsers cache
// aggressively but re-fetch the moment a build ships changed assets.
func assetVersion() string {
	h := sha256.New()
	for _, name := range []string{"static/app.js", "static/app.css"} {
		if b, err := assets.ReadFile(name); err == nil {
			h.Write(b)
		}
	}
	return hex.EncodeToString(h.Sum(nil))[:8]
}

// ListenAndServe runs the HTTP server with sane timeouts and supports
// graceful shutdown via the supplied context.
func (s *Server) ListenAndServe(ctx context.Context, addr string) error {
	hs := &http.Server{
		Addr:              addr,
		Handler:           s.mux,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	errCh := make(chan error, 1)
	go func() {
		s.log.Info("http server listening", "addr", addr)
		if err := hs.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
		close(errCh)
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		s.log.Info("shutting down http server")
		return hs.Shutdown(shutdownCtx)
	}
}

// staticHandler serves the embedded static/ directory under /static/.
// Immutable caching is only safe when the requested ?v= matches this
// binary's asset hash. After a rebuild, a still-open page pinned to the
// old hash can request its assets from the new binary; stamping that
// mismatched response immutable would poison the browser cache with an
// HTML/JS version skew until the next hash change.
func (s *Server) staticHandler() http.Handler {
	sub, err := fs.Sub(assets, "static")
	if err != nil {
		panic(err)
	}
	files := http.StripPrefix("/static/", http.FileServerFS(sub))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("v") == s.assetVer {
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		} else {
			w.Header().Set("Cache-Control", "no-store")
		}
		files.ServeHTTP(w, r)
	})
}

// pageData is the top-level shape passed into layout.html.
type pageData struct {
	Meta     store.Meta
	Initial  fragmentData
	AssetVer string
	Cols     []string
}

// Timeline columns. Built-ins have bespoke rendering (badges, hybrid
// process cell, summary projection + burst chip); everything else is a
// canonical query-schema field rendered through store.FieldValue.
// Default set favors triage context over identifiers: `user` shows
// privilege context at a glance; PID remains available via the columns
// picker and in every detail panel.
var defaultCols = []string{"time", "kind", "process", "user", "summary"}

func isBuiltinCol(c string) bool {
	switch c {
	case "time", "kind", "process", "summary":
		return true
	}
	return false
}

// parseColumns reads the `cols` CSV param, dropping anything that isn't
// a built-in or a known query-schema field. Empty / fully-invalid input
// falls back to the default set so the table always renders.
func parseColumns(r *http.Request) []string {
	raw := strings.TrimSpace(r.URL.Query().Get("cols"))
	if raw == "" {
		return defaultCols
	}
	var out []string
	for c := range strings.SplitSeq(raw, ",") {
		c = strings.TrimSpace(c)
		if c == "" {
			continue
		}
		if isBuiltinCol(c) || store.IsQueryField(c) {
			out = append(out, c)
		}
	}
	if len(out) == 0 {
		return defaultCols
	}
	return out
}

// fragmentData is what rows.html and the initial render both consume.
type fragmentData struct {
	Rows       []store.Row
	NextCursor int64
	HasMore    bool
	Empty      bool
	Cols       []string
}

// handlePage renders the full layout with the first timeline page. The
// very first page load (no query string at all) uses the MVP default
// filter set: cat=Process,File. Subsequent loads honor whatever the
// filter form posted via HTMX.
func (s *Server) handlePage(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	// HTML carries the asset-version pins — it must never be served stale.
	w.Header().Set("Cache-Control", "no-store")
	ctx := r.Context()
	meta, err := s.store.ReadMeta(ctx)
	if err != nil {
		s.log.Warn("read meta", "err", err)
	}
	filters := parseFilters(r)
	if r.URL.RawQuery == "" {
		filters.Categories = []string{"Process", "File"}
	}
	page, err := s.store.TimelinePage(ctx, filters)
	if err != nil {
		s.log.Error("initial timeline page", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	cols := parseColumns(r)
	data := pageData{
		Meta:     meta,
		Initial:  toFragment(page, cols),
		AssetVer: s.assetVer,
		Cols:     cols,
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.tmpl.ExecuteTemplate(w, "layout", data); err != nil {
		s.log.Error("execute layout", "err", err)
	}
}

// handleCount runs the same filter/dedup/garbage pipeline as
// /timeline but counts the entire matching set and returns an HTML
// fragment with the count and search time. JS swaps it into the
// results bar above the table.
func (s *Server) handleCount(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	start := time.Now()
	n, err := s.store.TimelineCount(ctx, parseFilters(r))
	elapsedMs := time.Since(start).Milliseconds()
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err != nil {
		s.log.Error("count", "err", err)
		_ = s.tmpl.ExecuteTemplate(w, "count-error", err.Error())
		return
	}
	if err := s.tmpl.ExecuteTemplate(w, "count", countView{
		Count:     n,
		Formatted: fmtCounts(n),
		Millis:    elapsedMs,
	}); err != nil {
		s.log.Error("execute count", "err", err)
	}
}

// countView is what the count template renders.
type countView struct {
	Count     int
	Formatted string
	Millis    int64
}

// handleExport streams every row matching the current filter set as
// NDJSON — one raw record per line, exactly what the /json endpoint
// returns per-record. Collapse is forced OFF: an export wants the
// underlying data, not folded burst rows. Garbage drop and all other
// filters apply as requested.
func (s *Server) handleExport(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	f := parseFilters(r)
	f.Collapse = false
	rows, err := s.store.MatchingRows(ctx, f)
	if err != nil {
		s.log.Error("export", "err", err)
		http.Error(w, "export failed", http.StatusInternalServerError)
		return
	}
	filename := "rustinel-export-" + time.Now().Format("20060102-150405") + ".ndjson"
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.Header().Set("Content-Disposition", `attachment; filename="`+filename+`"`)
	enc := json.NewEncoder(w)
	for _, row := range rows {
		switch row.Kind {
		case "a":
			_ = enc.Encode(row.Alert)
		case "e":
			_ = enc.Encode(row.Event)
		}
	}
	s.log.Info("export served", "rows", len(rows))
}

// handleQueryValues returns the top-N real values for one canonical
// query field as JSON. The autocomplete fetches this lazily (once per
// field per page load) so value suggestions reflect the actual snapshot
// instead of static examples.
func (s *Server) handleQueryValues(w http.ResponseWriter, r *http.Request) {
	field := strings.TrimSpace(r.URL.Query().Get("field"))
	w.Header().Set("Content-Type", "application/json")
	if field == "" {
		_, _ = w.Write([]byte("[]"))
		return
	}
	vals, err := s.store.TopFieldValues(r.Context(), field, 20)
	if err != nil {
		s.log.Error("query values", "field", field, "err", err)
		_, _ = w.Write([]byte("[]"))
		return
	}
	if vals == nil {
		vals = []string{}
	}
	_ = json.NewEncoder(w).Encode(vals)
}

// handleQuerySchema returns the canonical-field schema as JSON. The
// JS autocomplete reads this once at page load and uses it to suggest
// field names + value hints as the user types.
func (s *Server) handleQuerySchema(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	type fieldOut struct {
		Name     string   `json:"name"`
		Type     string   `json:"type"`
		Desc     string   `json:"desc"`
		Enum     []string `json:"enum,omitempty"`
		Examples []string `json:"examples,omitempty"`
	}
	out := make([]fieldOut, 0, len(store.QSchema))
	for _, f := range store.QSchema {
		out = append(out, fieldOut{
			Name:     f.Name,
			Type:     string(f.Type),
			Desc:     f.Desc,
			Enum:     f.Enum,
			Examples: f.Examples,
		})
	}
	_ = json.NewEncoder(w).Encode(out)
}

// handleQueryCheck parses the supplied query and returns either an
// error or a parse-OK indicator as JSON. Used by the input for live
// red-underline on syntax errors.
func (s *Server) handleQueryCheck(w http.ResponseWriter, r *http.Request) {
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	w.Header().Set("Content-Type", "application/json")
	if q == "" {
		_, _ = w.Write([]byte(`{"ok":true}`))
		return
	}
	_, err := store.ParseQuery(q)
	if err != nil {
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "err": err.Error()})
		return
	}
	_, _ = w.Write([]byte(`{"ok":true}`))
}

// handleRecordJSON returns just the JSON body of a single alert or event
// record. Used by the per-event copy buttons in the lineage view (and
// available as a general-purpose record-fetch endpoint). Responds with
// pretty-printed JSON so a clipboard paste lands as a readable block.
func (s *Server) handleRecordJSON(w http.ResponseWriter, r *http.Request) {
	kind := r.PathValue("kind")
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		http.NotFound(w, r)
		return
	}
	ctx := r.Context()
	w.Header().Set("Content-Type", "application/json")
	switch kind {
	case "a":
		a, err := s.store.GetAlert(ctx, id)
		if errors.Is(err, store.ErrNotFound) || err != nil {
			http.NotFound(w, r)
			return
		}
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		_ = enc.Encode(a)
	case "e":
		e, err := s.store.GetEvent(ctx, id)
		if errors.Is(err, store.ErrNotFound) || err != nil {
			http.NotFound(w, r)
			return
		}
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		_ = enc.Encode(e)
	default:
		http.NotFound(w, r)
	}
}

// handleLineage renders the pivot-rooted lineage page for the PID
// captured in the URL path. Origin = the PID; parents walk upward in a
// single chain; descendants are a bounded BFS subtree. Build cost is
// reported in the page meta. See
// docs/superpowers/specs/2026-06-09-pivot-lineage-design.md.
func (s *Server) handleLineage(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	pidStr := r.PathValue("pid")
	pid, err := strconv.Atoi(pidStr)
	if err != nil || pid <= 0 {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusOK) // soft-degrade — never 4xx the user
		_ = s.tmpl.ExecuteTemplate(w, "lineage-page", lineagePageView{
			Meta:     s.readMeta(r.Context()),
			Error:    "invalid PID in URL",
			BackURL:  "/",
			AssetVer: s.assetVer,
		})
		return
	}
	ctx := r.Context()
	view, err := s.store.BuildLineageFor(ctx, pid)
	if err != nil {
		s.log.Error("build lineage", "pid", pid, "err", err)
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_ = s.tmpl.ExecuteTemplate(w, "lineage-page", lineagePageView{
			Meta:     s.readMeta(ctx),
			Error:    "failed to build lineage: " + err.Error(),
			BackURL:  "/",
			PID:      pid,
			AssetVer: s.assetVer,
		})
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.tmpl.ExecuteTemplate(w, "lineage-page", lineagePageView{
		Meta:     s.readMeta(ctx),
		View:     view,
		BackURL:  "/",
		PID:      pid,
		AssetVer: s.assetVer,
	}); err != nil {
		s.log.Error("execute lineage-page", "err", err)
	}
}

// lineagePageView is what the lineage-page template renders.
type lineagePageView struct {
	Meta     store.Meta
	View     store.LineageView
	Error    string
	BackURL  string
	PID      int
	AssetVer string
}

// readMeta is a small wrapper that swallows the meta-read error so the
// caller can ignore it in the rare miss case (the header still renders
// with zero-valued counts).
func (s *Server) readMeta(ctx context.Context) store.Meta {
	m, err := s.store.ReadMeta(ctx)
	if err != nil {
		s.log.Warn("read meta (lineage)", "err", err)
	}
	return m
}

// handleDensity returns a per-bucket density summary for the supplied
// filter set as JSON. Used by the mini-timeline strip above the table.
// Buckets count defaults to 240 and is capped at 2000 by the store.
func (s *Server) handleDensity(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	buckets := 240
	if b := r.URL.Query().Get("buckets"); b != "" {
		if n, err := strconv.Atoi(b); err == nil && n > 0 {
			buckets = n
		}
	}
	d, err := s.store.TimelineDensity(ctx, parseFilters(r), buckets)
	w.Header().Set("Content-Type", "application/json")
	if err != nil {
		s.log.Error("density", "err", err)
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":"density failed"}`))
		return
	}
	_ = json.NewEncoder(w).Encode(d)
}

// handleTimeline is the HTMX fragment endpoint. Returns the rows
// fragment — soft-degrades on bad query params (never 4xx).
func (s *Server) handleTimeline(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	page, err := s.store.TimelinePage(ctx, parseFilters(r))
	if err != nil {
		s.log.Error("timeline page", "err", err)
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_ = s.tmpl.ExecuteTemplate(w, "row-error", err.Error())
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.tmpl.ExecuteTemplate(w, "rows", toFragment(page, parseColumns(r))); err != nil {
		s.log.Error("execute rows", "err", err)
	}
}

// alertDetailView is what the detail-alert template renders. For a single
// (non-burst) row, Burst is nil and only Alert is read.
type alertDetailView struct {
	Alert *store.Alert
	Burst *burstDetailView
}

type eventDetailView struct {
	Event *store.Event
	Burst *burstDetailView
}

// burstDetailView carries the rendered burst-summary state for the detail
// panel. Members holds at most store.BurstMemberCap entries; Hidden counts
// any beyond that.
type burstDetailView struct {
	Count   int
	Span    string
	Members []burstMemberLine
	Hidden  int
}

type burstMemberLine struct {
	Time    time.Time
	Summary string
}

// handleDetail dispatches on {kind} to the alert or event detail template.
// When the URL carries ?members=<ids>, the row is a burst — the handler
// also fetches each member and builds a compact list for the panel.
func (s *Server) handleDetail(w http.ResponseWriter, r *http.Request) {
	kind := r.PathValue("kind")
	idStr := r.PathValue("id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil || id <= 0 {
		s.renderDetailNotFound(w)
		return
	}
	ctx := r.Context()
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	burst := parseBurstQuery(r)
	switch kind {
	case "a":
		a, err := s.store.GetAlert(ctx, id)
		if errors.Is(err, store.ErrNotFound) {
			s.renderDetailNotFound(w)
			return
		}
		if err != nil {
			s.log.Error("get alert", "id", id, "err", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		view := alertDetailView{Alert: a, Burst: s.buildAlertBurst(ctx, burst)}
		if err := s.tmpl.ExecuteTemplate(w, "detail-alert", view); err != nil {
			s.log.Error("execute detail-alert", "err", err)
		}
	case "e":
		e, err := s.store.GetEvent(ctx, id)
		if errors.Is(err, store.ErrNotFound) {
			s.renderDetailNotFound(w)
			return
		}
		if err != nil {
			s.log.Error("get event", "id", id, "err", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		view := eventDetailView{Event: e, Burst: s.buildEventBurst(ctx, burst)}
		if err := s.tmpl.ExecuteTemplate(w, "detail-event", view); err != nil {
			s.log.Error("execute detail-event", "err", err)
		}
	default:
		s.renderDetailNotFound(w)
	}
}

// burstQuery carries the decoded ?members=... &count=... &span=... params.
type burstQuery struct {
	memberIDs []int64
	count     int
	span      string
}

func parseBurstQuery(r *http.Request) burstQuery {
	q := r.URL.Query()
	mem := q.Get("members")
	if mem == "" {
		return burstQuery{}
	}
	out := burstQuery{span: q.Get("span")}
	if c, err := strconv.Atoi(q.Get("count")); err == nil {
		out.count = c
	}
	for p := range strings.SplitSeq(mem, ",") {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		if id, err := strconv.ParseInt(p, 10, 64); err == nil && id > 0 {
			out.memberIDs = append(out.memberIDs, id)
		}
	}
	return out
}

func (s *Server) buildAlertBurst(ctx context.Context, b burstQuery) *burstDetailView {
	if b.count <= 0 || len(b.memberIDs) == 0 {
		return nil
	}
	v := &burstDetailView{Count: b.count, Span: b.span}
	for _, id := range b.memberIDs {
		a, err := s.store.GetAlert(ctx, id)
		if err != nil {
			continue
		}
		v.Members = append(v.Members, burstMemberLine{
			Time:    a.Timestamp,
			Summary: alertSummary(a),
		})
	}
	// Hidden = full burst count minus head minus members we just rendered.
	if hidden := b.count - 1 - len(v.Members); hidden > 0 {
		v.Hidden = hidden
	}
	return v
}

func (s *Server) buildEventBurst(ctx context.Context, b burstQuery) *burstDetailView {
	if b.count <= 0 || len(b.memberIDs) == 0 {
		return nil
	}
	v := &burstDetailView{Count: b.count, Span: b.span}
	for _, id := range b.memberIDs {
		e, err := s.store.GetEvent(ctx, id)
		if err != nil {
			continue
		}
		v.Members = append(v.Members, burstMemberLine{
			Time:    e.Timestamp,
			Summary: eventSummary(e),
		})
	}
	if hidden := b.count - 1 - len(v.Members); hidden > 0 {
		v.Hidden = hidden
	}
	return v
}

func (s *Server) renderDetailNotFound(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = s.tmpl.ExecuteTemplate(w, "detail-not-found", nil)
}

// parseFilters extracts the timeline filter set from query params. Every
// param is optional and parse-errors soft-degrade to defaults — the URL
// is bookmarkable and back-button-restorable, so we never 4xx.
//
// HTML forms submit multi-value checkboxes as repeated params
// (?cat=Process&cat=Network), so every multi-select param must be read
// with q[…] (slice) instead of q.Get(…) (first only). collectMulti also
// accepts the legacy comma-joined form for hand-typed URLs.
func parseFilters(r *http.Request) store.Filters {
	q := r.URL.Query()
	f := store.Filters{
		From:       parseTime(q.Get("from")),
		To:         parseTime(q.Get("to")),
		Severities: collectMulti(q["sev"]),
		Categories: collectMulti(q["cat"]),
		Actions:    collectMulti(q["action"]),
		Filename:   strings.TrimSpace(q.Get("fname")),
		QueryStr:   strings.TrimSpace(q.Get("query")),
	}
	if f.QueryStr != "" {
		expr, err := store.ParseQuery(f.QueryStr)
		if err == nil {
			f.QueryExpr = expr
		}
		// Soft-degrade on parse error — URL stays bookmarkable; UI
		// surfaces the error separately via /query/check.
	}

	src := collectMulti(q["src"])
	if len(src) == 0 {
		f.IncludeAlerts, f.IncludeEvents = true, true
	} else {
		for _, v := range src {
			switch v {
			case "alerts":
				f.IncludeAlerts = true
			case "events":
				f.IncludeEvents = true
			case "both":
				f.IncludeAlerts, f.IncludeEvents = true, true
			}
		}
	}

	if c := q.Get("cursor"); c != "" {
		if n, err := strconv.ParseInt(c, 10, 64); err == nil && n > 0 {
			f.Cursor = n
		}
	}
	if l := q.Get("limit"); l != "" {
		if n, err := strconv.Atoi(l); err == nil && n > 0 {
			f.Limit = n
		}
	}
	// Collapse and DropGarbage both default ON when the filter form has
	// not been submitted (initial page load, hand-typed URL with no
	// params at all). Once the form has been submitted, the chip's value
	// is authoritative — checked → "1"; unchecked → param absent. We
	// can't distinguish "user unchecked it" from "form never submitted"
	// without a sentinel, so the form posts a hidden field "form=1" any
	// time it fires; absence of "form=1" means initial load → default ON.
	formSubmitted := q.Get("form") != ""
	switch q.Get("collapse") {
	case "1", "true", "on":
		f.Collapse = true
	case "0", "false", "off":
		f.Collapse = false
	default:
		f.Collapse = !formSubmitted
	}
	switch q.Get("garbage") {
	case "1", "true", "on":
		f.DropGarbage = true
	case "0", "false", "off":
		f.DropGarbage = false
	default:
		f.DropGarbage = !formSubmitted
	}
	return f
}

// collectMulti flattens a slice of query-param values (possibly each holding
// a comma-separated list) into a single de-duped string slice.
func collectMulti(values []string) []string {
	if len(values) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(values))
	out := make([]string, 0, len(values))
	for _, v := range values {
		for item := range strings.SplitSeq(v, ",") {
			item = strings.TrimSpace(item)
			if item == "" {
				continue
			}
			if _, dup := seen[item]; dup {
				continue
			}
			seen[item] = struct{}{}
			out = append(out, item)
		}
	}
	return out
}

// parseTime accepts RFC3339 (with timezone) and the browser-native
// datetime-local format ("2006-01-02T15:04" or with seconds). The
// datetime-local variants carry no timezone marker; <input type=
// "datetime-local"> emits local-time strings, so we interpret them in
// time.Local rather than UTC. Without this, a brush selection or hand-
// typed filter from a non-UTC host would be off by the host's offset.
// Soft-degrades to zero time on parse error.
func parseTime(s string) time.Time {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t
	}
	for _, layout := range []string{"2006-01-02T15:04:05", "2006-01-02T15:04"} {
		if t, err := time.ParseInLocation(layout, s, time.Local); err == nil {
			return t
		}
	}
	return time.Time{}
}

func toFragment(p store.Page, cols []string) fragmentData {
	return fragmentData{
		Rows:       p.Rows,
		NextCursor: p.NextCursor,
		HasMore:    p.HasMore,
		Empty:      len(p.Rows) == 0,
		Cols:       cols,
	}
}
