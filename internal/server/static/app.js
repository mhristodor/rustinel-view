// rustinel-view virtualiser.
//
// Replaces the prior "HTMX-append + JS sliding-window trim" model with a
// real virtual list: server keeps returning HTML fragments via /timeline,
// the JS parses them into a flat row buffer, and only the rows currently
// in the viewport (plus a buffer) are inserted into the DOM. Total live
// <tr> count stays bounded regardless of fetched row count.
//
// Detail rows are open inline (afterend of their parent row) and their
// rendered height participates in the spacer + scroll math so scrolling
// past a tall expanded burst behaves correctly.
//
// Design contract — see docs/superpowers/specs/2026-06-08-performance-and-dedup-design.md.
(function () {
  'use strict';

  const ROW_HEIGHT = 28;            // matches CSS .row height
  const BUFFER_PX = 1200;           // px rendered above/below viewport — generous
  const PREFETCH_MARGIN_ROWS = 80;  // fetch next page when this close to buffer end
  // Filter changes refetch /timeline + /count + /density. With the
  // server's in-memory snapshot these each cost single-digit ms, so a
  // shorter debounce buys snappier perceived feedback without flooding.
  const FILTER_DEBOUNCE_MS = 180;

  const tbody = document.getElementById('tbody');
  const main = document.querySelector('.main');
  const form = document.getElementById('filters');

  // state.rows holds one entry per fetched data row (alert or event). It
  // does NOT hold detail rows — those are inline-injected after their
  // parent row and tracked via state.detailIdx / state.detailHeight.
  const state = {
    rows: [],
    nextCursor: 0,
    hasMore: true,
    inflight: null,         // AbortController for in-flight /timeline fetch
    countInflight: null,    // AbortController for in-flight /count fetch
    densityInflight: null,  // AbortController for in-flight /density fetch
    density: null,          // last fetched density payload (Density JSON)
    brush: null,            // active brush selection {startX, endX} during mouse-drag
    timeRangeNs: null,      // currently-applied {from, to} from a brush; null = no range
    schema: null,           // QSchema fetched from /query/schema, lazily on first focus
    valueCache: {},         // field name → top-N real values from /query/values
    valueFetching: {},      // field name → true while a fetch is in flight
    cols: null,             // active timeline columns (set in init)
    autoFitPending: false,  // run content-aware column sizing after the next page lands
    suggestIdx: 0,          // currently-highlighted autocomplete suggestion
    suggestItems: [],       // current suggestion list (field defs or value strings)
    suggestKind: 'field',   // 'field' | 'value' | 'op'
    suggestForField: null,  // when suggestKind='value', the QField we're suggesting for
    domStart: 0,            // index of first row currently in DOM
    domEnd: 0,              // one past last row currently in DOM
    openDetailFor: null,    // id of row whose detail is open
    detailIdx: -1,          // index in state.rows of the row with open detail
    detailHeight: 0,        // measured px height of the open detail row
    openDetailHtml: null,   // cached detail row HTML, re-injected on re-render
    emptyMarkerHtml: null,  // server-rendered empty state, shown when rows == 0
    selectedIdx: -1,        // keyboard-focused row index (-1 = none)
  };

  let topSpacer, botSpacer;
  let scrollScheduled = false;
  let filterDebounce = 0;

  function init() {
    // Tooltips are global — both pages benefit from viewport-clamped
    // hover tips on every [data-tip] element.
    initTooltips();
    // Detect which page we're on. The lineage page has its own
    // dedicated init path and skips everything timeline-specific.
    if (document.querySelector('.shell-lineage')) {
      initLineagePage();
      return;
    }
    if (!tbody || !main || !form) return;
    setupSpacers();
    ingestFromTbody();
    main.addEventListener('scroll', onScroll, { passive: true });
    form.addEventListener('input', onFormChange);
    form.addEventListener('change', onFormChange);
    form.addEventListener('reset', function () {
      clearTimeout(filterDebounce);
      setTimeout(triggerFilterChange, 0);
    });
    // Form-linked controls that live outside the form's DOM tree (the
    // dedup-noise switch in the header) don't bubble events to the form.
    // Wire them explicitly. The form attribute on the input keeps it in
    // the FormData round-trip.
    document.querySelectorAll('input[form="filters"]').forEach(function (el) {
      el.addEventListener('change', onFormChange);
    });
    initQueryInput();
    initColumns();
    // Time range is set entirely by brush-dragging on the density strip
    // (see applyTimeRange + onDensity* handlers). No popover UI to init.
    document.addEventListener('click', onDocClick);
    document.addEventListener('keydown', onKeyDown);
    // Brush-selection tracking on the density strip — must keep firing
    // even after the cursor leaves the SVG, so live at the document level.
    document.addEventListener('mousemove', onDensityMouseMove);
    document.addEventListener('mouseup', onDensityMouseUp);
    document.addEventListener('keydown', onDensityKey);
    render();
    refreshCount();
    refreshDensity();
    syncExportLink();
    autoFitColumns();
    window.addEventListener('resize', onWindowResize);
    maybeFetchMore();
  }

  // syncExportLink points the export anchor at /export with the current
  // filter set so a plain click downloads exactly what's on screen.
  function syncExportLink() {
    const link = document.getElementById('export-link');
    if (!link) return;
    const params = buildFilterUrl(0).split('?')[1] || '';
    link.href = '/export' + (params ? '?' + params : '');
  }

  let resizeT = 0;
  function onWindowResize() {
    clearTimeout(resizeT);
    resizeT = setTimeout(function () {
      renderDensity(state.density);
      autoFitColumns();
    }, 120);
  }

  // refreshCount calls /count to get the authoritative post-filter row
  // count for the current filter set. Runs at startup and on every filter
  // change. Independent of timeline pagination — JS doesn't block on the
  // count to render rows; both fetches race in parallel.
  function refreshCount() {
    const bar = document.getElementById('results-count');
    if (!bar) return;
    if (state.countInflight) {
      state.countInflight.abort();
      state.countInflight = null;
    }
    bar.innerHTML = '<span class="count-loading">computing…</span>';
    const ctrl = new AbortController();
    state.countInflight = ctrl;
    const url = '/count?' + buildFilterUrl(0).split('?')[1];
    fetch(url, { signal: ctrl.signal, headers: { 'Accept': 'text/html' } })
      .then(function (r) { return r.text(); })
      .then(function (html) {
        if (ctrl.signal.aborted) return;
        state.countInflight = null;
        bar.innerHTML = html;
      })
      .catch(function (err) {
        if (err && err.name === 'AbortError') return;
        state.countInflight = null;
        bar.innerHTML = '<span class="count-error">count failed</span>';
        console.error('rustinel-view: /count failed', err);
      });
  }

  function setupSpacers() {
    topSpacer = makeSpacer('vs-spacer-top');
    botSpacer = makeSpacer('vs-spacer-bot');
    tbody.insertBefore(topSpacer, tbody.firstChild);
    tbody.appendChild(botSpacer);
  }

  function makeSpacer(id) {
    const tr = document.createElement('tr');
    tr.id = id;
    tr.className = 'vs-spacer';
    const td = document.createElement('td');
    td.colSpan = 99;
    td.style.padding = '0';
    td.style.border = '0';
    td.style.height = '0px';
    tr.appendChild(td);
    return tr;
  }

  function ingestFromTbody() {
    const rows = Array.from(tbody.querySelectorAll('tr.row'));
    rows.forEach(function (tr) { pushRow(tr); tr.remove(); });
    const loader = tbody.querySelector('tr.row-loader');
    if (loader) { readLoader(loader); loader.remove(); }
    else { state.hasMore = false; }
    const empty = tbody.querySelector('tr.row-empty');
    if (empty) { state.emptyMarkerHtml = empty.outerHTML; empty.remove(); }
  }

  function pushRow(tr) {
    state.rows.push({
      html: tr.outerHTML,
      kind: tr.classList.contains('row-alert') ? 'a' : 'e',
      id: parseDetailId(tr.dataset.detailUrl || ''),
      detailUrl: tr.dataset.detailUrl || '',
    });
  }

  function parseDetailId(url) {
    const m = url.match(/\/detail\/[ae]\/(\d+)/);
    return m ? parseInt(m[1], 10) : 0;
  }

  function readLoader(loader) {
    const url = loader.getAttribute('hx-get') || '';
    if (!url) { state.hasMore = false; return; }
    try {
      const u = new URL(url, window.location.origin);
      const c = u.searchParams.get('cursor');
      state.nextCursor = c ? parseInt(c, 10) : 0;
      state.hasMore = true;
    } catch (e) { state.hasMore = false; }
  }

  function buildFilterUrl(cursor) {
    const fd = new FormData(form);
    const params = new URLSearchParams();
    fd.forEach(function (v, k) {
      if (v !== '' && v != null) params.append(k, String(v));
    });
    appendColsParam(params);
    if (cursor && cursor > 0) params.set('cursor', String(cursor));
    else params.delete('cursor');
    return '/timeline?' + params.toString();
  }

  function pageUrlFromForm() {
    const fd = new FormData(form);
    const params = new URLSearchParams();
    fd.forEach(function (v, k) {
      if (v !== '' && v != null) params.append(k, String(v));
    });
    appendColsParam(params);
    const qs = params.toString();
    return '/' + (qs ? '?' + qs : '');
  }

  // appendColsParam carries the column selection in every timeline URL —
  // only when it differs from the default, so plain URLs stay clean.
  function appendColsParam(params) {
    if (state.cols && !colsEqual(state.cols, DEFAULT_COLS)) {
      params.set('cols', state.cols.join(','));
    }
  }

  // ---------- scroll math (detail-aware) ----------

  // pixelToRowIdx maps a pixel offset from document top to a row index in
  // state.rows, accounting for the inline detail row.
  function pixelToRowIdx(p) {
    if (p < 0) p = 0;
    if (state.detailIdx < 0 || state.detailHeight === 0) {
      return Math.floor(p / ROW_HEIGHT);
    }
    // Detail occupies pixels [detailStart, detailStart + detailHeight).
    // The parent row is at (detailIdx * ROW_HEIGHT) and the detail follows it.
    const parentTop = state.detailIdx * ROW_HEIGHT;
    const detailStart = parentTop + ROW_HEIGHT;
    const detailEnd = detailStart + state.detailHeight;
    if (p < detailStart) return Math.floor(p / ROW_HEIGHT);
    if (p < detailEnd) return state.detailIdx; // viewport edge sits in detail
    return Math.floor((p - state.detailHeight) / ROW_HEIGHT);
  }

  // ---------- scroll + fetch ----------

  function onScroll() {
    if (scrollScheduled) return;
    scrollScheduled = true;
    requestAnimationFrame(function () {
      scrollScheduled = false;
      render();
      maybeFetchMore();
    });
  }

  function maybeFetchMore() {
    if (!state.hasMore || state.inflight) return;
    if (state.rows.length - state.domEnd <= PREFETCH_MARGIN_ROWS) {
      fetchPage();
    }
  }

  function fetchPage() {
    abortInflight();
    const ctrl = new AbortController();
    state.inflight = ctrl;
    const url = buildFilterUrl(state.nextCursor);
    fetch(url, { signal: ctrl.signal, headers: { 'Accept': 'text/html' } })
      .then(function (r) { return r.text(); })
      .then(function (html) {
        if (ctrl.signal.aborted) return;
        state.inflight = null;
        ingestFragment(html);
        render();
        // Content-aware column sizing runs once per filter/column change,
        // on the first page only — never during scroll pagination, so
        // the layout stays stable while scrolling.
        if (state.autoFitPending) {
          state.autoFitPending = false;
          autoFitColumns();
        }
        maybeFetchMore();
      })
      .catch(function (err) {
        if (err && err.name === 'AbortError') return;
        state.inflight = null;
        console.error('rustinel-view: /timeline fetch failed', err);
      });
  }

  function abortInflight() {
    if (state.inflight) { state.inflight.abort(); state.inflight = null; }
  }

  function ingestFragment(html) {
    const doc = new DOMParser().parseFromString(
      '<table><tbody>' + html + '</tbody></table>', 'text/html');
    const rows = Array.from(doc.querySelectorAll('tr.row'));
    rows.forEach(function (tr) { pushRow(tr); });
    const loader = doc.querySelector('tr.row-loader');
    if (loader) readLoader(loader);
    else state.hasMore = false;
    if (state.rows.length === 0) {
      const empty = doc.querySelector('tr.row-empty');
      if (empty) state.emptyMarkerHtml = empty.outerHTML;
    }
  }

  // ---------- filter form ----------

  function onFormChange() {
    clearTimeout(filterDebounce);
    filterDebounce = setTimeout(triggerFilterChange, FILTER_DEBOUNCE_MS);
  }

  function triggerFilterChange() {
    abortInflight();
    state.rows = [];
    state.nextCursor = 0;
    state.hasMore = true;
    state.openDetailFor = null;
    state.openDetailHtml = null;
    state.detailIdx = -1;
    state.detailHeight = 0;
    state.emptyMarkerHtml = null;
    state.domStart = 0;
    state.domEnd = 0;
    state.selectedIdx = -1;
    syncTimeRangeFromForm();
    state.autoFitPending = true;
    clearLiveRows();
    main.scrollTop = 0;
    history.replaceState(null, '', pageUrlFromForm());
    render();
    refreshCount();
    refreshDensity();
    syncExportLink();
    fetchPage();
  }

  // syncTimeRangeFromForm keeps state.timeRangeNs (used to draw the
  // persistent brush overlay) in step with the from/to inputs. Cleared
  // when either is blank — that way manually clearing the inputs makes
  // the overlay disappear.
  function syncTimeRangeFromForm() {
    const fromInput = form.querySelector('input[name="from"]');
    const toInput = form.querySelector('input[name="to"]');
    const fv = fromInput ? fromInput.value : '';
    const tv = toInput ? toInput.value : '';
    if (!fv || !tv) { state.timeRangeNs = null; return; }
    // datetime-local is "YYYY-MM-DDTHH:MM[:SS]" in local time.
    const fNs = Date.parse(fv) * 1e6;
    const tNs = Date.parse(tv) * 1e6;
    if (!isFinite(fNs) || !isFinite(tNs)) { state.timeRangeNs = null; return; }
    state.timeRangeNs = { from: fNs, to: tNs };
  }

  function clearLiveRows() {
    let cur = topSpacer.nextSibling;
    while (cur && cur !== botSpacer) {
      const next = cur.nextSibling;
      cur.remove();
      cur = next;
    }
  }

  // ---------- render ----------

  function render() {
    if (state.rows.length === 0) {
      clearLiveRows();
      updateSpacers(0, 0);
      if (state.emptyMarkerHtml) {
        const tpl = document.createElement('template');
        tpl.innerHTML = state.emptyMarkerHtml.trim();
        topSpacer.parentNode.insertBefore(tpl.content.firstElementChild, botSpacer);
      }
      state.domStart = 0;
      state.domEnd = 0;
      return;
    }

    const scrollTop = main.scrollTop;
    const viewportH = main.clientHeight;
    const viewBot = scrollTop + viewportH;

    let newStart = pixelToRowIdx(scrollTop - BUFFER_PX);
    let newEnd = pixelToRowIdx(viewBot + BUFFER_PX) + 1;
    if (newStart < 0) newStart = 0;
    if (newEnd > state.rows.length) newEnd = state.rows.length;
    if (newEnd < newStart) newEnd = newStart;

    if (newStart === state.domStart && newEnd === state.domEnd) {
      updateSpacers(newStart, newEnd);
      syncDetailRow();
      syncFocusedRow();
      return;
    }

    patchWindow(newStart, newEnd);
    state.domStart = newStart;
    state.domEnd = newEnd;
    updateSpacers(newStart, newEnd);
    syncDetailRow();
    syncFocusedRow();
  }

  // syncFocusedRow reconciles the keyboard-focus highlight against the
  // live DOM. Needed because patchWindow recycles surviving rows without
  // rebuilding them — buildRow's class assignment only covers fresh rows,
  // so j/k selection changes within a stable window were invisible.
  function syncFocusedRow() {
    tbody.querySelectorAll('tr.row.row-focused').forEach(function (el) {
      el.classList.remove('row-focused');
    });
    if (state.selectedIdx < 0) return;
    if (state.selectedIdx < state.domStart || state.selectedIdx >= state.domEnd) return;
    const el = findLiveRowAt(state.selectedIdx);
    if (el) el.classList.add('row-focused');
  }

  // patchWindow incrementally updates the live row range from
  // [state.domStart, state.domEnd) to [newStart, newEnd) without tearing
  // down rows that survive. Eliminates the flicker of full rebuilds.
  function patchWindow(newStart, newEnd) {
    const oldStart = state.domStart;
    const oldEnd = state.domEnd;

    // No overlap → fresh rebuild is cheaper than two large incremental edits.
    if (newEnd <= oldStart || newStart >= oldEnd || oldEnd === oldStart) {
      clearLiveRows();
      const frag = document.createDocumentFragment();
      for (let i = newStart; i < newEnd; i++) frag.appendChild(buildRow(i));
      topSpacer.parentNode.insertBefore(frag, botSpacer);
      return;
    }

    // Trim from front: rows in [oldStart, newStart) leave the window.
    for (let i = oldStart; i < newStart; i++) {
      removeFrontRow();
    }
    // Trim from back: rows in [newEnd, oldEnd) leave the window.
    for (let i = newEnd; i < oldEnd; i++) {
      removeBackRow();
    }
    // Prepend new rows in [newStart, max(oldStart, newStart)).
    const prependEnd = Math.max(oldStart, newStart);
    if (newStart < prependEnd) {
      const frag = document.createDocumentFragment();
      for (let i = newStart; i < prependEnd; i++) frag.appendChild(buildRow(i));
      topSpacer.parentNode.insertBefore(frag, topSpacer.nextSibling);
    }
    // Append new rows in [min(oldEnd, newEnd), newEnd).
    const appendStart = Math.min(oldEnd, newEnd);
    if (appendStart < newEnd) {
      const frag = document.createDocumentFragment();
      for (let i = appendStart; i < newEnd; i++) frag.appendChild(buildRow(i));
      topSpacer.parentNode.insertBefore(frag, botSpacer);
    }
  }

  function buildRow(idx) {
    const r = state.rows[idx];
    const el = makeRowEl(r.html);
    if (state.openDetailFor === r.id) {
      el.setAttribute('aria-selected', 'true');
    }
    if (state.selectedIdx === idx) {
      el.classList.add('row-focused');
    }
    return el;
  }

  // removeFrontRow removes the topmost live data row. Detail rows live
  // attached to their parent; we skip past any leading detail-rows in
  // search of an actual .row, then remove that .row and any .detail-row
  // that immediately follows it (they belong together).
  function removeFrontRow() {
    let cur = topSpacer.nextSibling;
    while (cur && cur !== botSpacer && !isDataRow(cur)) {
      const next = cur.nextSibling;
      cur.remove();
      cur = next;
    }
    if (cur && isDataRow(cur)) {
      const after = cur.nextSibling;
      cur.remove();
      if (after && after !== botSpacer && after.classList && after.classList.contains('detail-row')) {
        after.remove();
      }
    }
  }

  function removeBackRow() {
    let cur = botSpacer.previousSibling;
    // Trailing detail-row belongs to the row above it.
    if (cur && cur.classList && cur.classList.contains('detail-row')) {
      const trail = cur;
      cur = cur.previousSibling;
      trail.remove();
    }
    while (cur && cur !== topSpacer && !isDataRow(cur)) {
      const prev = cur.previousSibling;
      cur.remove();
      cur = prev;
    }
    if (cur && isDataRow(cur)) cur.remove();
  }

  function isDataRow(el) {
    return el && el.classList && el.classList.contains('row');
  }

  // syncDetailRow ensures the live DOM matches state.openDetailFor:
  // - if open detail's parent is in [domStart, domEnd), the detail row is
  //   present immediately after the parent;
  // - if the parent is out of window, the detail row is not in the DOM.
  // Idempotent — safe to call repeatedly.
  function syncDetailRow() {
    if (state.openDetailFor == null) {
      tbody.querySelectorAll('tr.detail-row').forEach(function (d) { d.remove(); });
      return;
    }
    const dIdx = state.detailIdx;
    if (dIdx < state.domStart || dIdx >= state.domEnd) {
      tbody.querySelectorAll('tr.detail-row').forEach(function (d) { d.remove(); });
      return;
    }
    // Parent in window — make sure detail row is right after parent.
    const parentRow = findLiveRowAt(dIdx);
    if (!parentRow) return;
    const sib = parentRow.nextSibling;
    if (sib && sib.classList && sib.classList.contains('detail-row')) return;
    if (!state.openDetailHtml) return;
    const dt = makeRowEl(state.openDetailHtml);
    if (!dt) return;
    parentRow.parentNode.insertBefore(dt, parentRow.nextSibling);
  }

  // findLiveRowAt returns the data-row element corresponding to state.rows[idx]
  // (where idx is in [domStart, domEnd)).
  function findLiveRowAt(idx) {
    const offset = idx - state.domStart;
    let count = 0;
    let cur = topSpacer.nextSibling;
    while (cur && cur !== botSpacer) {
      if (isDataRow(cur)) {
        if (count === offset) return cur;
        count++;
      }
      cur = cur.nextSibling;
    }
    return null;
  }

  function updateSpacers(firstVisible, lastVisible) {
    const total = state.rows.length;
    let topH = firstVisible * ROW_HEIGHT;
    let botH = Math.max(0, (total - lastVisible) * ROW_HEIGHT);
    if (state.detailIdx >= 0 && state.detailHeight > 0) {
      if (state.detailIdx < firstVisible) topH += state.detailHeight;
      else if (state.detailIdx >= lastVisible) botH += state.detailHeight;
    }
    topSpacer.firstElementChild.style.height = topH + 'px';
    botSpacer.firstElementChild.style.height = botH + 'px';
  }

  function makeRowEl(html) {
    const tpl = document.createElement('template');
    tpl.innerHTML = html.trim();
    return tpl.content.firstElementChild;
  }

  // ---------- click handling (detail open/close + popovers) ----------

  function onDocClick(e) {
    const expand = e.target.closest('.chip-expand');
    if (expand) {
      e.preventDefault();
      const id = expand.getAttribute('data-popover');
      const popover = document.getElementById(id);
      if (popover) {
        document.querySelectorAll('.action-popover.open').forEach(function (p) {
          if (p !== popover) p.classList.remove('open');
        });
        popover.classList.toggle('open');
      }
      return;
    }
    if (!e.target.closest('.action-popover, .chip-expand')) {
      document.querySelectorAll('.action-popover.open').forEach(function (p) {
        p.classList.remove('open');
      });
    }

    const copier = e.target.closest('[data-copy-json]');
    if (copier) {
      e.preventDefault();
      copyDetailJson(copier);
      return;
    }

    // Click-to-filter on a detail field value. Inside each q-click span:
    //   - `−` button → exclude (NOT field:value)
    //   - `+` button → include (field:value)
    //   - value text → include shortcut (same as `+`)
    const exclude = e.target.closest('.q-exclude');
    if (exclude) {
      e.preventDefault();
      e.stopPropagation();
      const qc = exclude.closest('[data-q-field], [data-q-key]');
      if (qc) addQueryTermFromDetail(qc, true);
      return;
    }
    const include = e.target.closest('.q-include');
    if (include) {
      e.preventDefault();
      e.stopPropagation();
      const qc = include.closest('[data-q-field], [data-q-key]');
      if (qc) addQueryTermFromDetail(qc, false);
      return;
    }
    const qclick = e.target.closest('[data-q-field], [data-q-key]');
    if (qclick) {
      e.preventDefault();
      e.stopPropagation();
      addQueryTermFromDetail(qclick, false);
      return;
    }

    const closer = e.target.closest('[data-close-detail]');
    if (closer) {
      e.preventDefault();
      closeDetail();
      return;
    }

    const row = e.target.closest('tr.row');
    if (!row || !row.dataset.detailUrl) return;
    const id = parseDetailId(row.dataset.detailUrl);
    // Keep keyboard focus in sync with the most-recently-clicked row so
    // j/k pick up from where the user just was.
    state.selectedIdx = indexOfRowId(id);
    syncFocusedRow();
    if (state.openDetailFor === id) {
      closeDetail();
      return;
    }
    openDetail(id, row.dataset.detailUrl, row);
  }

  function openDetail(id, url, sourceRow) {
    closeDetail();
    fetch(url, { headers: { 'Accept': 'text/html' } })
      .then(function (r) { return r.text(); })
      .then(function (html) {
        // Parse the fragment so we can mutate q-click spans cleanly,
        // then re-serialize for caching. syncDetailRow re-inserts from
        // the cached HTML so any modifications must live in the string.
        const tmp = makeRowEl(html);
        if (tmp) {
          annotateQClicks(tmp);
          html = tmp.outerHTML;
        }
        state.openDetailFor = id;
        state.openDetailHtml = html;
        state.detailIdx = indexOfRowId(id);
        sourceRow.setAttribute('aria-selected', 'true');
        const el = makeRowEl(html);
        if (!el) return;
        sourceRow.parentNode.insertBefore(el, sourceRow.nextSibling);
        // Measure rendered height now that it's in the DOM.
        state.detailHeight = el.offsetHeight || 0;
        updateSpacers(state.domStart, state.domEnd);
      })
      .catch(function (err) {
        console.error('rustinel-view: detail fetch failed', err);
      });
  }

  // annotateQClicks runs once per detail panel — injects the small `+`
  // and `−` action buttons that include / exclude the value from the
  // active query. Both are hidden until the q-click span (or buttons
  // themselves) are hovered. Click on the value text itself remains a
  // shortcut for the include action.
  function annotateQClicks(root) {
    root.querySelectorAll('.q-click').forEach(function (qc) {
      if (qc.querySelector('.q-include') || qc.querySelector('.q-exclude')) return;
      const inc = document.createElement('button');
      inc.type = 'button';
      inc.className = 'q-include';
      inc.textContent = '+';
      inc.setAttribute('aria-label', 'include in query');
      inc.setAttribute('data-tip', 'Include in query (AND)');
      inc.setAttribute('data-tip-pos', 'below');
      qc.appendChild(inc);
      const exc = document.createElement('button');
      exc.type = 'button';
      exc.className = 'q-exclude';
      exc.textContent = '−';
      exc.setAttribute('aria-label', 'exclude from query');
      exc.setAttribute('data-tip', 'Exclude from query (NOT)');
      exc.setAttribute('data-tip-pos', 'below');
      qc.appendChild(exc);
    });
  }

  function indexOfRowId(id) {
    for (let i = 0; i < state.rows.length; i++) {
      if (state.rows[i].id === id) return i;
    }
    return -1;
  }

  // copyDetailJson reads the hidden JSON source colocated with the copy
  // button and writes it to the clipboard. Visual feedback via a `copied`
  // class on the button for ~1.2s.
  function copyDetailJson(btn) {
    const body = btn.closest('.detail-body');
    if (!body) return;
    const src = body.querySelector('.detail-json-source');
    if (!src) return;
    const text = src.textContent || '';
    if (!navigator.clipboard || !navigator.clipboard.writeText) {
      // Fallback for non-secure contexts: select + execCommand
      legacyCopy(text);
      flashCopied(btn);
      return;
    }
    navigator.clipboard.writeText(text).then(function () {
      flashCopied(btn);
    }).catch(function (err) {
      console.error('rustinel-view: clipboard write failed', err);
    });
  }

  function flashCopied(btn) {
    btn.classList.add('copied');
    setTimeout(function () { btn.classList.remove('copied'); }, 1200);
  }

  function legacyCopy(text) {
    const ta = document.createElement('textarea');
    ta.value = text;
    ta.style.position = 'fixed';
    ta.style.opacity = '0';
    document.body.appendChild(ta);
    ta.select();
    try { document.execCommand('copy'); } catch (e) { /* ignore */ }
    document.body.removeChild(ta);
  }

  function closeDetail() {
    state.openDetailFor = null;
    state.openDetailHtml = null;
    state.detailIdx = -1;
    state.detailHeight = 0;
    tbody.querySelectorAll('tr[aria-selected]').forEach(function (s) {
      s.removeAttribute('aria-selected');
    });
    tbody.querySelectorAll('tr.detail-row').forEach(function (d) { d.remove(); });
    updateSpacers(state.domStart, state.domEnd);
  }

  // ---------- mini-timeline density strip ----------
  //
  // refreshDensity fetches /density with the current filter set and renders
  // the result into the SVG strip. The strip honors all filters (dedup,
  // garbage, src, q, etc) so its bars mirror exactly what the timeline
  // table would show. Hover for a tooltip; click a bar to jump the table
  // to that point in time.

  function refreshDensity() {
    if (state.densityInflight) {
      state.densityInflight.abort();
      state.densityInflight = null;
    }
    const ctrl = new AbortController();
    state.densityInflight = ctrl;
    const buckets = computeBucketCount();
    const url = '/density?' + buildFilterUrl(0).split('?')[1] + '&buckets=' + buckets;
    fetch(url, { signal: ctrl.signal, headers: { 'Accept': 'application/json' } })
      .then(function (r) { return r.json(); })
      .then(function (data) {
        if (ctrl.signal.aborted) return;
        state.densityInflight = null;
        state.density = data;
        renderDensity(data);
      })
      .catch(function (err) {
        if (err && err.name === 'AbortError') return;
        state.densityInflight = null;
        console.error('rustinel-view: /density failed', err);
      });
  }

  function computeBucketCount() {
    const strip = document.getElementById('density-strip');
    if (!strip) return 240;
    // One bar per ~5 px of horizontal width — readable without crowding.
    const w = strip.clientWidth - 52; // subtract padding
    if (w < 200) return 120;
    return Math.max(80, Math.min(480, Math.floor(w / 5)));
  }

  function renderDensity(data) {
    const svg = document.getElementById('density-svg');
    const empty = document.getElementById('density-empty');
    if (!svg) return;
    if (!data || !data.buckets || data.buckets.length === 0 || data.max === 0) {
      svg.innerHTML = '';
      if (empty) empty.hidden = false;
      return;
    }
    if (empty) empty.hidden = true;

    const W = svg.clientWidth || 800;
    const H = svg.clientHeight || 40;
    const n = data.buckets.length;
    const barW = W / n;
    const max = data.max;
    // Log-scale so a single tall bucket doesn't flatten all the others.
    const scale = function (v) {
      if (v <= 0) return 0;
      return (Math.log1p(v) / Math.log1p(max)) * (H - 2);
    };

    svg.setAttribute('viewBox', '0 0 ' + W + ' ' + H);
    svg.setAttribute('width', W);
    svg.setAttribute('height', H);
    const ns = 'http://www.w3.org/2000/svg';

    // Build with strings then innerHTML for one-shot insertion — faster
    // than appending one element at a time for n>200.
    const parts = [];
    for (let i = 0; i < n; i++) {
      const b = data.buckets[i];
      const a = b.a | 0;
      const e = b.e | 0;
      if (a === 0 && e === 0) continue;
      const x = (i * barW).toFixed(2);
      const w = Math.max(0.6, barW - 0.5).toFixed(2);
      const eh = scale(e);
      const ah = scale(a + e) - eh;
      // events drawn from the bottom up; alerts stacked above
      if (e > 0) {
        parts.push('<rect class="bar-event" data-i="' + i + '" x="' + x +
          '" y="' + (H - eh).toFixed(2) + '" width="' + w +
          '" height="' + eh.toFixed(2) + '"></rect>');
      }
      if (a > 0) {
        parts.push('<rect class="bar-alert" data-i="' + i + '" x="' + x +
          '" y="' + (H - eh - ah).toFixed(2) + '" width="' + w +
          '" height="' + ah.toFixed(2) + '"></rect>');
      }
    }
    // Wide invisible hit-test rectangles spanning the full strip height,
    // one per bucket. Hover + click delegated to these so very small bars
    // are still easy to grab.
    for (let i = 0; i < n; i++) {
      const x = (i * barW).toFixed(2);
      const w = barW.toFixed(2);
      parts.push('<rect class="bar-hit" data-i="' + i + '" x="' + x +
        '" y="0" width="' + w + '" height="' + H + '"></rect>');
    }
    svg.innerHTML = parts.join('');

    svg.onmousemove = onDensityHover;
    svg.onmouseleave = function () {
      const tip = document.getElementById('density-tip');
      if (tip) tip.hidden = true;
    };
    svg.onmousedown = onDensityMouseDown;
    // click + mouseup are handled by document-level listeners installed
    // once in init() — brush gestures must keep tracking the mouse even
    // when it leaves the strip.

    // Re-draw the persistent brush overlay if a time range is active.
    drawBrushOverlay();
  }

  function bucketTimeRange(i) {
    const d = state.density;
    if (!d) return null;
    const n = d.buckets.length;
    const span = d.to_ns - d.from_ns;
    const bw = n > 0 ? span / n : 0;
    return {
      start: d.from_ns + i * bw,
      end:   d.from_ns + (i + 1) * bw,
    };
  }

  function onDensityHover(e) {
    const hit = e.target.closest('rect.bar-hit');
    if (!hit) return;
    const i = parseInt(hit.getAttribute('data-i'), 10);
    if (isNaN(i)) return;
    const d = state.density;
    if (!d) return;
    const b = d.buckets[i] || { a: 0, e: 0 };
    const r = bucketTimeRange(i);
    if (!r) return;
    const tip = document.getElementById('density-tip');
    if (!tip) return;
    tip.innerHTML =
      '<span class="tip-time">' + fmtNsTime(r.start) + '</span>' +
      (b.a ? '<span class="tip-alert">' + b.a + ' alert' + (b.a > 1 ? 's' : '') + '</span>' : '') +
      (b.e ? '<span class="tip-event">' + b.e + ' event' + (b.e > 1 ? 's' : '') + '</span>' : '');
    const strip = document.getElementById('density-strip');
    const stripRect = strip.getBoundingClientRect();
    tip.style.left = (e.clientX - stripRect.left) + 'px';
    tip.hidden = false;
  }

  // Drag threshold below which a mouseup is treated as a click (jump),
  // above which it sets the time-range filter.
  const BRUSH_MIN_PX = 5;

  function onDensityMouseDown(e) {
    // Only main button.
    if (e.button !== 0) return;
    const svg = document.getElementById('density-svg');
    const rect = svg.getBoundingClientRect();
    const x = clampX(e.clientX - rect.left, rect.width);
    state.brush = { startX: x, endX: x, originX: e.clientX, rectW: rect.width };
    drawBrushOverlay();
    e.preventDefault();
  }

  function onDensityMouseMove(e) {
    if (!state.brush) return;
    const svg = document.getElementById('density-svg');
    if (!svg) return;
    const rect = svg.getBoundingClientRect();
    state.brush.endX = clampX(e.clientX - rect.left, rect.width);
    drawBrushOverlay();
  }

  function onDensityMouseUp(e) {
    if (!state.brush) return;
    const drag = Math.abs(state.brush.endX - state.brush.startX);
    if (drag < BRUSH_MIN_PX) {
      // Treat as a single click → jump to the bucket under the click.
      const bIdx = bucketAtX(state.brush.startX);
      const r = bucketTimeRange(bIdx);
      state.brush = null;
      drawBrushOverlay();
      if (r) jumpToTimestampNs(Math.floor(r.start));
      return;
    }
    // Brush — commit the time range to the form's from/to inputs and
    // refire the filter pipeline.
    const x0 = Math.min(state.brush.startX, state.brush.endX);
    const x1 = Math.max(state.brush.startX, state.brush.endX);
    const t0 = pixelToTimestampNs(x0);
    const t1 = pixelToTimestampNs(x1);
    state.brush = null;
    applyTimeRange(t0, t1);
  }

  function onDensityKey(e) {
    if (e.key === 'Escape' && state.brush) {
      state.brush = null;
      drawBrushOverlay();
    }
  }

  function clampX(x, w) {
    if (x < 0) return 0;
    if (x > w) return w;
    return x;
  }

  function bucketAtX(x) {
    const d = state.density;
    if (!d || !d.buckets || d.buckets.length === 0) return 0;
    const svg = document.getElementById('density-svg');
    const w = svg.clientWidth || 1;
    const idx = Math.floor((x / w) * d.buckets.length);
    return Math.max(0, Math.min(d.buckets.length - 1, idx));
  }

  function pixelToTimestampNs(x) {
    const d = state.density;
    if (!d) return 0;
    const svg = document.getElementById('density-svg');
    const w = svg.clientWidth || 1;
    const span = d.to_ns - d.from_ns;
    return Math.floor(d.from_ns + (x / w) * span);
  }

  // drawBrushOverlay synchronizes two visual layers on the strip SVG:
  //   - The active drag rectangle (state.brush) while the mouse is down.
  //   - The persistent selection (state.timeRangeNs) showing the
  //     currently-applied time-range filter.
  // Both are rendered as <rect> elements appended to / replaced in the SVG.
  function drawBrushOverlay() {
    const svg = document.getElementById('density-svg');
    if (!svg) return;
    // Remove old overlays.
    svg.querySelectorAll('rect.brush, rect.range').forEach(function (r) { r.remove(); });
    const H = svg.clientHeight || 40;

    // Persistent range first (so live brush draws on top).
    if (state.timeRangeNs && state.density) {
      const x0 = timestampToPixel(state.timeRangeNs.from);
      const x1 = timestampToPixel(state.timeRangeNs.to);
      if (x1 > x0) {
        const r = document.createElementNS('http://www.w3.org/2000/svg', 'rect');
        r.setAttribute('class', 'range');
        r.setAttribute('x', x0);
        r.setAttribute('y', 0);
        r.setAttribute('width', x1 - x0);
        r.setAttribute('height', H);
        svg.appendChild(r);
      }
    }
    if (state.brush) {
      const x0 = Math.min(state.brush.startX, state.brush.endX);
      const x1 = Math.max(state.brush.startX, state.brush.endX);
      if (x1 - x0 >= 1) {
        const r = document.createElementNS('http://www.w3.org/2000/svg', 'rect');
        r.setAttribute('class', 'brush');
        r.setAttribute('x', x0);
        r.setAttribute('y', 0);
        r.setAttribute('width', x1 - x0);
        r.setAttribute('height', H);
        svg.appendChild(r);
      }
    }
  }

  function timestampToPixel(ns) {
    const d = state.density;
    if (!d) return 0;
    const svg = document.getElementById('density-svg');
    const w = svg.clientWidth || 1;
    const span = d.to_ns - d.from_ns;
    if (span <= 0) return 0;
    return ((ns - d.from_ns) / span) * w;
  }

  // applyTimeRange writes the brushed range into the from/to inputs in
  // the filter form (using local-time YYYY-MM-DDTHH:MM:SS format that the
  // datetime-local input understands), records the active range for the
  // persistent overlay, and triggers a filter refresh.
  function applyTimeRange(fromNs, toNs) {
    const fromInput = form.querySelector('input[name="from"]');
    const toInput = form.querySelector('input[name="to"]');
    const fromS = formatForDatetimeLocal(fromNs);
    const toS = formatForDatetimeLocal(toNs);
    if (fromInput) fromInput.value = fromS;
    if (toInput) toInput.value = toS;
    state.timeRangeNs = { from: fromNs, to: toNs };
    triggerFilterChange();
  }

  function formatForDatetimeLocal(ns) {
    const d = new Date(ns / 1e6);
    const pad = function (n) { return String(n).padStart(2, '0'); };
    return d.getFullYear() + '-' + pad(d.getMonth() + 1) + '-' + pad(d.getDate()) +
      'T' + pad(d.getHours()) + ':' + pad(d.getMinutes()) + ':' + pad(d.getSeconds());
  }

  // jumpToTimestampNs resets the page state and refetches starting at the
  // given unix-nanosecond cursor. Used by mini-timeline clicks.
  function jumpToTimestampNs(ns) {
    abortInflight();
    state.rows = [];
    state.hasMore = true;
    state.nextCursor = ns > 0 ? ns - 1 : 0; // cursor is exclusive — back off by 1ns so the bucket's first row is included
    state.selectedIdx = -1;
    state.openDetailFor = null;
    state.openDetailHtml = null;
    state.detailIdx = -1;
    state.detailHeight = 0;
    state.domStart = 0;
    state.domEnd = 0;
    clearLiveRows();
    main.scrollTop = 0;
    render();
    fetchPage();
    // Count + density are unchanged (no filter changed), so don't refresh.
  }

  function fmtNsTime(ns) {
    const d = new Date(ns / 1e6);
    const h = String(d.getHours()).padStart(2, '0');
    const m = String(d.getMinutes()).padStart(2, '0');
    const s = String(d.getSeconds()).padStart(2, '0');
    const ms = String(d.getMilliseconds()).padStart(3, '0');
    return h + ':' + m + ':' + s + '.' + ms;
  }

  // ---------- keyboard navigation ----------
  //
  // Bindings (active when no input/textarea is focused):
  //   j / ↓       next row
  //   k / ↑       prev row
  //   Enter       toggle detail on focused row
  //   c           copy JSON of currently-open detail
  //   /           focus the substring search input
  //   g           jump to top
  //   G           jump to bottom
  //   Esc         close detail (or blur focused input)

  function onKeyDown(e) {
    // Typing in a form input → never intercept, except Esc which blurs.
    if (isTypingTarget(e.target)) {
      if (e.key === 'Escape') e.target.blur();
      return;
    }
    if (e.ctrlKey || e.metaKey || e.altKey) return;

    switch (e.key) {
      case 'j':
      case 'ArrowDown':
        e.preventDefault();
        moveSelection(1);
        break;
      case 'k':
      case 'ArrowUp':
        e.preventDefault();
        moveSelection(-1);
        break;
      case 'Enter':
        e.preventDefault();
        toggleSelectedDetail();
        break;
      case 'c':
        e.preventDefault();
        copyOpenDetail();
        break;
      case '/':
        e.preventDefault();
        focusFilterQuery();
        break;
      case 'Escape':
        if (state.openDetailFor != null) {
          e.preventDefault();
          closeDetail();
        }
        break;
      case 'g':
        e.preventDefault();
        jumpTo(0);
        break;
      case 'G':
        e.preventDefault();
        jumpTo(state.rows.length - 1);
        break;
    }
  }

  function isTypingTarget(el) {
    if (!el) return false;
    const tag = el.tagName;
    if (tag === 'INPUT') {
      // Checkboxes / radios don't take text — keyboard nav stays alive.
      const t = (el.type || '').toLowerCase();
      return t !== 'checkbox' && t !== 'radio' && t !== 'submit' && t !== 'button';
    }
    return tag === 'TEXTAREA' || tag === 'SELECT' || el.isContentEditable;
  }

  function moveSelection(delta) {
    if (state.rows.length === 0) return;
    let next;
    if (state.selectedIdx < 0) {
      // First key press: select the topmost row currently in the viewport
      // (not the data window) — what the user is actually looking at.
      next = pixelToRowIdx(main.scrollTop);
      if (next < 0) next = 0;
      if (next >= state.rows.length) next = state.rows.length - 1;
    } else {
      next = state.selectedIdx + delta;
    }
    if (next < 0) next = 0;
    if (next >= state.rows.length) next = state.rows.length - 1;
    state.selectedIdx = next;
    ensureSelectedVisible();
    render();
  }

  function rowIdxToPixelTop(idx) {
    let p = idx * ROW_HEIGHT;
    if (state.detailIdx >= 0 && state.detailHeight > 0 && state.detailIdx < idx) {
      p += state.detailHeight;
    }
    return p;
  }

  function ensureSelectedVisible() {
    const idx = state.selectedIdx;
    if (idx < 0) return;
    const top = rowIdxToPixelTop(idx);
    const bot = top + ROW_HEIGHT;
    const vTop = main.scrollTop;
    const vBot = vTop + main.clientHeight;
    if (top < vTop) main.scrollTop = top;
    else if (bot > vBot) main.scrollTop = bot - main.clientHeight;
  }

  function toggleSelectedDetail() {
    if (state.selectedIdx < 0) return;
    const r = state.rows[state.selectedIdx];
    if (!r || !r.detailUrl) return;
    if (state.openDetailFor === r.id) { closeDetail(); return; }
    // Make sure the row is in the live DOM before opening (openDetail
    // needs a source <tr> to insert the detail row after).
    ensureSelectedVisible();
    render();
    // Defer one frame so the freshly-rendered row is in the DOM.
    requestAnimationFrame(function () {
      const live = findLiveRowAt(state.selectedIdx);
      if (live) openDetail(r.id, r.detailUrl, live);
    });
  }

  function copyOpenDetail() {
    const btn = tbody.querySelector('.detail-row .detail-copy[data-copy-json]');
    if (btn) copyDetailJson(btn);
  }

  function focusFilterQuery() {
    // The query input lives outside the form (results bar) — target by id.
    const q = document.getElementById('query-input');
    if (q) { q.focus(); q.select(); }
  }

  function jumpTo(idx) {
    if (state.rows.length === 0) return;
    if (idx < 0) idx = 0;
    if (idx >= state.rows.length) idx = state.rows.length - 1;
    state.selectedIdx = idx;
    main.scrollTop = rowIdxToPixelTop(idx);
    render();
  }

  // ---------- click-to-filter from detail panels ----------
  //
  // Event Fields keys (the keys that live in the Event.Fields map) map
  // to canonical query field names. Alert-side fields carry their own
  // canonical name in data-q-field so they skip this table.

  const EVENT_FIELD_TO_QUERY = {
    'Image': 'image',
    'ProcessId': 'pid',
    'CommandLine': 'cmdline',
    'ParentImage': 'parent_image',
    'ParentProcessId': 'parent_pid',
    'ParentCommandLine': 'parent_cmdline',
    'User': 'user',
    'TargetFilename': 'target',
    'QueryName': 'query',
    'RecordType': 'record',
    'SourceIp': 'src',
    'DestinationIp': 'dst',
    'SourcePort': 'src_port',
    'DestinationPort': 'dst_port',
    'Protocol': 'proto',
  };

  function addQueryTermFromDetail(el, negate) {
    const value = el.dataset.qVal || '';
    if (!value) return;
    let field = el.dataset.qField;
    if (!field) {
      const key = el.dataset.qKey;
      if (!key) return;
      field = EVENT_FIELD_TO_QUERY[key];
      if (!field) return; // key not part of the canonical schema — skip
    }
    appendQueryTerm(field, value, !!negate);
  }

  function appendQueryTerm(field, value, negate) {
    const input = document.getElementById('query-input');
    if (!input) return;
    const body = field + ':' + quoteIfNeeded(value);
    const term = negate ? 'NOT ' + body : body;
    const cur = input.value.trim();
    // Don't double-add the exact same term.
    if (cur.indexOf(term) !== -1) return;
    input.value = cur ? cur + ' AND ' + term : term;
    // Drive the same path as typing: validate + refetch.
    onQueryInput();
  }

  function quoteIfNeeded(v) {
    const s = String(v);
    if (s.length === 0) return '""';
    if (/[\s"()]/.test(s)) {
      return '"' + s.replace(/\\/g, '\\\\').replace(/"/g, '\\"') + '"';
    }
    return s;
  }

  // ---------- timeline column management ----------
  //
  // Columns are server-rendered per the `cols` query param; the client
  // owns the column STATE (which + widths) and the thead DOM. Built-ins
  // (time/kind/process/summary) have bespoke rendering; every canonical
  // query-schema field is also available as a plain value column.
  // Selection persists in localStorage and in the URL (when non-default)
  // so reloads and shared links keep the layout.

  const BUILTIN_COLS = ['time', 'kind', 'process', 'summary'];
  // Must mirror the server's defaultCols — URL cleanliness checks compare
  // against this list.
  const DEFAULT_COLS = ['time', 'kind', 'process', 'user', 'summary'];

  function colsEqual(a, b) {
    return a.length === b.length && a.every(function (v, i) { return v === b[i]; });
  }

  function loadCols() {
    const urlCols = new URLSearchParams(location.search).get('cols');
    if (urlCols) {
      const parsed = urlCols.split(',').map(function (s) { return s.trim(); }).filter(Boolean);
      if (parsed.length) return parsed;
    }
    try {
      const saved = JSON.parse(localStorage.getItem('rv-cols'));
      if (Array.isArray(saved) && saved.length) return saved;
    } catch (e) { /* corrupted storage — fall through */ }
    return DEFAULT_COLS.slice();
  }

  function saveCols() {
    try { localStorage.setItem('rv-cols', JSON.stringify(state.cols)); } catch (e) { /* quota */ }
  }

  function initColumns() {
    // Manual column widths used to persist in rv-colw and could lock the
    // layout into a bad state forever; auto-fit is now the single
    // authority, so drop any leftover key.
    try { localStorage.removeItem('rv-colw'); } catch (e) { /* ignore */ }
    state.cols = loadCols();
    rebuildThead();
    // Server rendered the default set when the URL carried no cols param;
    // if the user's saved selection differs, refetch to match the thead.
    const urlHadCols = new URLSearchParams(location.search).has('cols');
    if (!urlHadCols && !colsEqual(state.cols, DEFAULT_COLS)) {
      triggerFilterChange();
    }

    // The column picker popover is opened only from the table header's
    // + button; its listeners bind here because the popover element is
    // static while the thead row gets rebuilt.
    const pop = document.getElementById('cols-popover');
    if (pop) {
      // Column toggles live inside the filter form — stop their change
      // events before the form's listener double-fires a filter refresh.
      pop.addEventListener('change', function (e) {
        const cb = e.target.closest('[data-col-toggle]');
        if (!cb) return;
        e.stopPropagation();
        toggleColumn(cb.value, cb.checked);
      });
      pop.addEventListener('click', function (e) {
        const reset = e.target.closest('[data-cols-reset]');
        if (!reset) return;
        e.preventDefault();
        e.stopPropagation();
        state.cols = DEFAULT_COLS.slice();
        saveCols();
        rebuildThead();
        buildColsPopover(pop);
        triggerFilterChange();
      });
    }

    // Column resize: drag the handle at the right edge of any header.
    document.addEventListener('mousedown', onColResizeStart);
    document.addEventListener('mousemove', onColResizeMove);
    document.addEventListener('mouseup', onColResizeEnd);

    // Header column tools (move/remove/add) — delegated on the thead,
    // which survives rebuildThead's row rewrites.
    const thead = document.getElementById('thead');
    if (thead) thead.addEventListener('click', onTheadClick);
  }

  function buildColsPopover(pop) {
    return ensureSchemaLoaded().then(function (schema) {
      const entries = BUILTIN_COLS.map(function (n) { return { name: n }; })
        .concat((schema || []).map(function (f) { return { name: f.name }; }));
      const parts = entries.map(function (c) {
        const checked = state.cols.indexOf(c.name) !== -1 ? ' checked' : '';
        return '<label class="mini"><input type="checkbox" data-col-toggle value="' +
          escapeHtml(c.name) + '"' + checked + '> ' +
          escapeHtml(c.name.replace(/_/g, ' ')) + '</label>';
      });
      parts.push('<button type="button" class="cols-reset" data-cols-reset>reset to default</button>');
      pop.innerHTML = parts.join('');
    });
  }

  function toggleColumn(name, on) {
    if (on) {
      if (state.cols.indexOf(name) !== -1) return;
      // New columns slot in before the flexible summary column when present.
      const i = state.cols.indexOf('summary');
      if (i >= 0) state.cols.splice(i, 0, name);
      else state.cols.push(name);
    } else {
      if (state.cols.length <= 1) return; // never drop the last column
      state.cols = state.cols.filter(function (c) { return c !== name; });
    }
    saveCols();
    rebuildThead();
    triggerFilterChange();
  }

  // ico pulls one of the pre-rendered SVG icons that the server embeds
  // as window.RV_ICONS in the page <head>. Single source of truth: any
  // dynamic builder uses the same inline SVG markup the server emits.
  function ico(name) {
    return (window.RV_ICONS && window.RV_ICONS[name]) || '';
  }

  // theadCellHtml mirrors the server-rendered th structure in layout.html
  // — label text node first (autoFit measures th.firstChild), then the
  // hover tools, then the resize handle.
  function theadCellHtml(c) {
    const removeBtn = state.cols.length > 1
      ? '<button type="button" class="col-tool" data-col-remove aria-label="remove column">' + ico('x') + '</button>'
      : '';
    return '<th class="col-' + escapeHtml(c) + '" data-col="' + escapeHtml(c) + '">' +
      escapeHtml(c.replace(/_/g, ' ')) +
      '<span class="col-tools">' +
        '<button type="button" class="col-tool" data-col-move="left" aria-label="move column left">' + ico('chevron-left') + '</button>' +
        removeBtn +
        '<button type="button" class="col-tool" data-col-move="right" aria-label="move column right">' + ico('chevron-right') + '</button>' +
      '</span>' +
      '<span class="col-resizer" data-col-resizer></span></th>';
  }

  function rebuildThead() {
    const tr = document.querySelector('#thead tr');
    if (!tr) return;
    tr.innerHTML = state.cols.map(theadCellHtml).join('') +
      '<th class="col-add"><button type="button" class="col-add-btn" id="col-add-btn" aria-label="add column">' + ico('plus') + '</button></th>';
  }

  function onTheadClick(e) {
    const add = e.target.closest('.col-add-btn');
    if (add) {
      e.preventDefault();
      e.stopPropagation(); // keep onDocClick from instantly closing the popover
      openColsPopoverFromHeader(add);
      return;
    }
    const mv = e.target.closest('[data-col-move]');
    if (mv) {
      e.preventDefault();
      const th = mv.closest('th[data-col]');
      if (th) moveColumn(th.dataset.col, mv.dataset.colMove === 'left' ? -1 : 1);
      return;
    }
    const rm = e.target.closest('[data-col-remove]');
    if (rm) {
      e.preventDefault();
      const th = rm.closest('th[data-col]');
      if (th) toggleColumn(th.dataset.col, false);
    }
  }

  // moveColumn shifts a column one slot left or right: state, then the
  // live DOM in place — header cells keep their inline widths as they
  // travel, data rows get their two cells swapped, detail/spacer rows
  // (colspan, no .row class) are untouched. No refetch, no scroll reset;
  // the next pagination fetch already carries the new order.
  function moveColumn(name, dir) {
    const i = state.cols.indexOf(name);
    const j = i + dir;
    if (i < 0 || j < 0 || j >= state.cols.length) return;
    const tmp = state.cols[i];
    state.cols[i] = state.cols[j];
    state.cols[j] = tmp;
    saveCols();
    const a = Math.min(i, j);
    const headTr = document.querySelector('#thead tr');
    if (headTr) headTr.insertBefore(headTr.children[a + 1], headTr.children[a]);
    tbody.querySelectorAll('tr.row').forEach(function (r) {
      if (r.children.length > a + 1) r.insertBefore(r.children[a + 1], r.children[a]);
    });
    history.replaceState(null, '', pageUrlFromForm());
  }

  // openColsPopoverFromHeader reuses the page-header column picker but
  // pins it under the + button at the end of the table header row.
  function openColsPopoverFromHeader(anchor) {
    const pop = document.getElementById('cols-popover');
    if (!pop) return;
    if (pop.classList.contains('open')) { pop.classList.remove('open'); return; }
    buildColsPopover(pop).then(function () {
      const r = anchor.getBoundingClientRect();
      pop.style.position = 'fixed';
      pop.style.top = (r.bottom + 6) + 'px';
      pop.style.left = Math.max(8, r.right - 220) + 'px';
      pop.classList.add('open');
    });
  }

  // contentWidth measures the laid-out width of a node's inline contents
  // via a Range. Unlike scrollWidth — which never reports less than the
  // element's current box — this can shrink, so repeated auto-fits don't
  // ratchet wide columns wider.
  const measRange = document.createRange();
  function contentWidth(node) {
    if (!node) return 0;
    measRange.selectNodeContents(node);
    return Math.ceil(measRange.getBoundingClientRect().width);
  }

  // autoFitColumns sizes every header so the table always fills — and
  // never exceeds — the container width, each column's share proportional
  // to its actual content width measured on the live rows. It is the
  // single authority on widths: manual drag-resizes hold only until the
  // next fit (filter change, column change, window resize, reload).
  //
  // Runs only at controlled moments — never during scroll pagination,
  // so the layout stays rock-stable while scrolling.
  function autoFitColumns() {
    const ths = Array.prototype.slice.call(document.querySelectorAll('#thead th[data-col]'));
    if (!ths.length || !main) return;
    const rows = Array.prototype.slice.call(tbody.querySelectorAll('tr.row')).slice(0, 60);
    let avail = main.clientWidth - 2;
    // The fixed add-column cell at the end of the header row is not a
    // data column — its width comes out of the fit budget.
    const addTh = document.querySelector('#thead th.col-add');
    if (addTh) avail -= Math.ceil(addTh.getBoundingClientRect().width);
    if (avail <= 0) return;
    const PAD = 30;       // cell padding + breathing room
    const MINW = 60;      // floor — keep every column grabbable
    const CAPF = 0.5;     // no single column hogs more than half the table

    // Desired width per column = max(header label, widest visible cell).
    // th.firstChild is the label text node — measuring the whole th would
    // include the absolutely-positioned resizer span and ratchet.
    const desired = ths.map(function (th, i) {
      let w = contentWidth(th.firstChild) + PAD;
      rows.forEach(function (r) {
        const c = r.children[i];
        if (c) w = Math.max(w, contentWidth(c) + PAD);
      });
      return Math.max(MINW, Math.min(w, Math.floor(avail * CAPF)));
    });

    // Scale every column — up or down — so the total exactly fills the
    // container. Exceeding it is never allowed: .main clips horizontal
    // overflow without a scrollbar, hiding the right side of open panels.
    const sum = desired.reduce(function (s, w) { return s + w; }, 0);
    const scale = sum > 0 ? avail / sum : 1;
    ths.forEach(function (th, i) {
      th.style.width = Math.max(MINW, Math.floor(desired[i] * scale)) + 'px';
    });
  }

  let colDrag = null; // { th, next, startX, startW, startWNext }

  function onColResizeStart(e) {
    const rz = e.target.closest('[data-col-resizer]');
    if (!rz) return;
    const th = rz.closest('th');
    if (!th) return;
    const ths = Array.prototype.slice.call(document.querySelectorAll('#thead th[data-col]'));
    const next = ths[ths.indexOf(th) + 1];
    if (!next) return; // last boundary — no neighbor to trade width with
    colDrag = {
      th: th,
      next: next,
      startX: e.clientX,
      startW: th.getBoundingClientRect().width,
      startWNext: next.getBoundingClientRect().width,
    };
    document.body.classList.add('col-resizing');
    e.preventDefault();
  }

  function onColResizeMove(e) {
    if (!colDrag) return;
    const MIN = 48;
    // Width is traded across the boundary, never added: growing the table
    // total would push it past the container, where overflow-x:hidden
    // clips it without a scrollbar.
    const d = Math.max(MIN - colDrag.startW,
      Math.min(e.clientX - colDrag.startX, colDrag.startWNext - MIN));
    colDrag.th.style.width = Math.round(colDrag.startW + d) + 'px';
    colDrag.next.style.width = Math.round(colDrag.startWNext - d) + 'px';
  }

  function onColResizeEnd() {
    if (!colDrag) return;
    document.body.classList.remove('col-resizing');
    colDrag = null;
  }

  // ---------- query language input + autocomplete ----------
  //
  // KQL-style: `field:value AND field:value`, supporting OR / NOT / parens
  // / wildcards / quoted strings / implicit-AND / bare-term substring
  // search. The schema is loaded once from /query/schema; the dropdown
  // shows field names matching what the user has typed at the cursor,
  // and after a `field:` we show the enum / example values for that field.

  function initQueryInput() {
    const input = document.getElementById('query-input');
    if (!input) return;
    input.addEventListener('input', onQueryInput);
    input.addEventListener('focus', onQueryFocus);
    input.addEventListener('blur', function () {
      // Defer hide so a click in the dropdown can still register.
      setTimeout(hideQuerySuggest, 100);
    });
    input.addEventListener('keydown', onQueryKey);
    const clearBtn = document.getElementById('query-clear');
    if (clearBtn) {
      clearBtn.addEventListener('click', clearQuery);
    }
    syncClearButtonVisibility();
    ensureSchemaLoaded();
  }

  // syncClearButtonVisibility shows the × button only when the input
  // has content. Run after every input event + after clearing so the
  // button always matches input state.
  function syncClearButtonVisibility() {
    const input = document.getElementById('query-input');
    const btn = document.getElementById('query-clear');
    if (!input || !btn) return;
    btn.hidden = input.value.length === 0;
  }

  function clearQuery() {
    const input = document.getElementById('query-input');
    if (!input) return;
    if (input.value === '') return;
    input.value = '';
    syncClearButtonVisibility();
    hideQuerySuggest();
    // Drive the same path as typing-to-empty: validate + refetch.
    onQueryInput();
    input.focus();
  }

  function ensureSchemaLoaded() {
    if (state.schema) return Promise.resolve(state.schema);
    return fetch('/query/schema', { headers: { 'Accept': 'application/json' } })
      .then(function (r) { return r.json(); })
      .then(function (data) {
        state.schema = data;
        return data;
      })
      .catch(function (err) {
        console.error('rustinel-view: /query/schema failed', err);
        state.schema = [];
        return [];
      });
  }

  function onQueryFocus() { ensureSchemaLoaded().then(updateQuerySuggest); }

  let queryCheckT = 0;
  function onQueryInput() {
    syncClearButtonVisibility();
    updateQuerySuggest();
    clearTimeout(queryCheckT);
    queryCheckT = setTimeout(checkQuerySyntax, 220);
    // The query input lives outside the form (via form="filters") so
    // the form's input listener doesn't fire for it — drive the filter
    // pipeline directly. onFormChange handles its own 300ms debounce.
    onFormChange();
  }

  function checkQuerySyntax() {
    const input = document.getElementById('query-input');
    const status = document.getElementById('query-status');
    if (!input || !status) return;
    const q = input.value;
    if (!q.trim()) {
      status.textContent = '';
      input.classList.remove('query-bad');
      return;
    }
    fetch('/query/check?q=' + encodeURIComponent(q))
      .then(function (r) { return r.json(); })
      .then(function (data) {
        if (data.ok) {
          status.textContent = '';
          input.classList.remove('query-bad');
        } else {
          status.textContent = data.err || 'syntax error';
          input.classList.add('query-bad');
        }
      })
      .catch(function () { /* network hiccup — leave UI alone */ });
  }

  // Detect the cursor's context: are we typing a field name, a value
  // (after `:`), or an operator-position (after a complete term)?
  function queryContext() {
    const input = document.getElementById('query-input');
    if (!input) return null;
    const caret = input.selectionStart || 0;
    const left = input.value.slice(0, caret);
    // Find the last unfinished token: scan back from caret until we hit
    // a whitespace, paren, or colon (which boundaries the token).
    let i = left.length - 1;
    let inQuote = false;
    for (; i >= 0; i--) {
      const c = left[i];
      if (c === '"') { inQuote = !inQuote; continue; }
      if (inQuote) continue;
      if (c === ' ' || c === '\t' || c === '(' || c === ')') break;
    }
    const tokStart = i + 1;
    const tok = left.slice(tokStart);
    // Is there a `:` inside the current token? If so we're past a field
    // boundary, suggesting a value.
    const colonIdx = tok.indexOf(':');
    if (colonIdx >= 0) {
      const fieldName = tok.slice(0, colonIdx);
      const partial = tok.slice(colonIdx + 1);
      return { kind: 'value', field: fieldName, partial: partial, tokStart: tokStart + colonIdx + 1, caret: caret };
    }
    return { kind: 'field', partial: tok, tokStart: tokStart, caret: caret };
  }

  function updateQuerySuggest() {
    const ctx = queryContext();
    if (!ctx || !state.schema) { hideQuerySuggest(); return; }
    let items = [];
    let kind = ctx.kind;
    let fieldDef = null;
    if (ctx.kind === 'field') {
      const partial = ctx.partial.toLowerCase();
      const fields = state.schema.filter(function (f) {
        return !partial || f.name.toLowerCase().indexOf(partial) === 0;
      });
      // Boolean operators are valid in field position too (after a
      // complete term). Mix them in if the partial matches.
      const ops = ['AND', 'OR', 'NOT'].filter(function (op) {
        return !partial || op.toLowerCase().indexOf(partial) === 0;
      });
      items = fields.map(function (f) { return { kind: 'field', name: f.name, desc: f.desc, type: f.type }; });
      items = items.concat(ops.map(function (op) {
        return { kind: 'op', name: op, desc: 'boolean operator' };
      }));
    } else {
      fieldDef = state.schema.find(function (f) { return f.name === ctx.field; });
      if (!fieldDef) { hideQuerySuggest(); return; }
      const partial = ctx.partial.toLowerCase();
      let pool = [];
      if (fieldDef.enum && fieldDef.enum.length) {
        pool = fieldDef.enum.slice();
      } else if (state.valueCache[ctx.field]) {
        // Real top-N values from the snapshot, fetched lazily per field.
        pool = state.valueCache[ctx.field].slice();
      } else {
        pool = (fieldDef.examples || []).slice();
        fetchFieldValues(ctx.field);
      }
      const filtered = pool.filter(function (v) {
        return !partial || v.toLowerCase().indexOf(partial) === 0;
      });
      items = filtered.map(function (v) { return { kind: 'value', name: v, desc: fieldDef.type }; });
    }
    if (items.length === 0) { hideQuerySuggest(); return; }
    state.suggestItems = items;
    state.suggestKind = kind;
    state.suggestForField = fieldDef;
    state.suggestIdx = 0;
    renderQuerySuggest(ctx);
  }

  function renderQuerySuggest(ctx) {
    const drop = document.getElementById('query-suggest');
    if (!drop) return;
    const parts = state.suggestItems.map(function (it, i) {
      const cls = (i === state.suggestIdx) ? 'qs-item active' : 'qs-item';
      const kindTag = it.kind === 'op' ? 'op' : (it.kind === 'value' ? (it.desc || 'value') : (it.type || 'field'));
      return '<div class="' + cls + '" data-i="' + i + '">' +
        '<span class="qs-name">' + escapeHtml(it.name) + '</span>' +
        '<span class="qs-kind">' + escapeHtml(kindTag) + '</span>' +
        (it.desc && it.kind !== 'value' ? '<span class="qs-desc">' + escapeHtml(it.desc) + '</span>' : '') +
        '</div>';
    });
    drop.innerHTML = parts.join('');
    drop.hidden = false;
    // Wire mouse selection.
    drop.querySelectorAll('.qs-item').forEach(function (el) {
      el.addEventListener('mousedown', function (e) {
        e.preventDefault();
        state.suggestIdx = parseInt(el.dataset.i, 10);
        applyQuerySuggest(ctx);
      });
    });
  }

  function hideQuerySuggest() {
    const drop = document.getElementById('query-suggest');
    if (drop) drop.hidden = true;
  }

  // fetchFieldValues lazily pulls the top-N real values for a field from
  // the server, once per field per page load. When the response lands,
  // the suggestion list refreshes so the examples get replaced by data.
  function fetchFieldValues(field) {
    if (state.valueFetching[field] || state.valueCache[field]) return;
    state.valueFetching[field] = true;
    fetch('/query/values?field=' + encodeURIComponent(field), {
      headers: { 'Accept': 'application/json' },
    })
      .then(function (r) { return r.json(); })
      .then(function (vals) {
        state.valueCache[field] = Array.isArray(vals) ? vals : [];
        delete state.valueFetching[field];
        updateQuerySuggest();
      })
      .catch(function () { delete state.valueFetching[field]; });
  }

  function escapeHtml(s) {
    return String(s).replace(/[&<>"']/g, function (c) {
      return { '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c];
    });
  }

  function onQueryKey(e) {
    const drop = document.getElementById('query-suggest');
    const open = drop && !drop.hidden;
    if (!open) {
      if (e.key === 'Escape') { e.target.blur(); }
      return;
    }
    if (e.key === 'ArrowDown') {
      e.preventDefault();
      state.suggestIdx = Math.min(state.suggestItems.length - 1, state.suggestIdx + 1);
      refreshSelectedSuggest();
    } else if (e.key === 'ArrowUp') {
      e.preventDefault();
      state.suggestIdx = Math.max(0, state.suggestIdx - 1);
      refreshSelectedSuggest();
    } else if (e.key === 'Enter' || e.key === 'Tab') {
      e.preventDefault();
      const ctx = queryContext();
      if (ctx) applyQuerySuggest(ctx);
    } else if (e.key === 'Escape') {
      hideQuerySuggest();
    }
  }

  function refreshSelectedSuggest() {
    const items = document.querySelectorAll('.qs-item');
    items.forEach(function (el, i) {
      el.classList.toggle('active', i === state.suggestIdx);
    });
  }

  // applyQuerySuggest splices the highlighted suggestion into the input
  // at the cursor's token position, adds the appropriate trailing char
  // (`:` after a field, ` ` after a value / operator), and re-checks
  // context so a follow-up suggestion list opens automatically.
  function applyQuerySuggest(ctx) {
    const input = document.getElementById('query-input');
    if (!input || !ctx) return;
    const it = state.suggestItems[state.suggestIdx];
    if (!it) return;
    const before = input.value.slice(0, ctx.tokStart);
    const after = input.value.slice(ctx.caret);
    let insert = it.name;
    if (it.kind === 'field') insert += ':';
    else if (it.kind === 'op') insert += ' ';
    else insert += ' '; // value
    input.value = before + insert + after;
    const newCaret = (before + insert).length;
    input.setSelectionRange(newCaret, newCaret);
    input.focus();
    updateQuerySuggest();
    // Treat applied suggestion as an edit — trigger filter pipeline.
    if (typeof onFormChange === 'function') onFormChange();
  }

  // ---------- pivot-rooted lineage page ----------
  //
  // The lineage page is a separate route at /lineage/{pid}. The DOM is
  // pre-rendered server-side; JS only handles the local interactions:
  //   - click node head  → toggle .lin-node-body visibility (default expanded)
  //   - click section head → toggle .lin-section-body (default collapsed)
  //   - pivot links are plain <a href>, no JS needed
  //
  // Note: stopPropagation on the pivot <a> in the template prevents
  // clicking the arrow from also toggling the node body.

  function initLineagePage() {
    document.addEventListener('click', onLineagePageClick);
    document.addEventListener('keydown', function (e) {
      if (e.key === 'Escape' && document.getElementById('lin-detail-modal-backdrop')) {
        e.preventDefault();
        closeLineageDetailModal();
      }
    });
    const expandAll = document.getElementById('lin-expand-all');
    const collapseAll = document.getElementById('lin-collapse-all');
    if (expandAll) expandAll.addEventListener('click', function () { setAllNodes(true); });
    if (collapseAll) collapseAll.addEventListener('click', function () { setAllNodes(false); });
    const gotoOrigin = document.getElementById('lin-goto-origin');
    if (gotoOrigin) gotoOrigin.addEventListener('click', function () { scrollToLineageNode(null); });
    document.addEventListener('mouseover', onLineageRailHover);
    // Land the user on their pivot: with the parent chain stitched into
    // the tree, the origin can sit many ancestor levels deep.
    const origin = document.querySelector('.lin-node.is-origin');
    if (origin) origin.scrollIntoView({ block: 'center' });
  }

  function onLineagePageClick(e) {
    // Modal close button.
    const closer = e.target.closest('[data-lin-detail-close]');
    if (closer) { e.preventDefault(); closeLineageDetailModal(); return; }
    // Click on modal backdrop (anywhere outside the inner panel).
    if (e.target.classList && e.target.classList.contains('lin-detail-modal-backdrop')) {
      closeLineageDetailModal();
      return;
    }

    // Breadcrumb / origin jump in the lineage bar — scrolls, never pivots.
    const crumb = e.target.closest('[data-crumb-pid]');
    if (crumb) {
      e.preventDefault();
      scrollToLineageNode(crumb.dataset.crumbPid);
      return;
    }

    // Process info button (ⓘ) — opens the node's full info in a modal.
    const infoBtn = e.target.closest('[data-node-info-btn]');
    if (infoBtn) {
      e.preventDefault();
      const node = infoBtn.closest('.lin-node');
      if (node) openProcessInfoModal(node);
      return;
    }

    // Pivot link inside a node head — let the browser navigate, but
    // bail before the toggle check below so we don't also flip the
    // node's collapsed state on the way out.
    if (e.target.closest('.lin-pivot')) return;

    // Per-event copy button — fetch the record's JSON from /json/{kind}/{id}
    // and write it to the clipboard. Visual feedback on the button.
    const copier = e.target.closest('[data-copy-event-json]');
    if (copier) {
      e.preventDefault();
      e.stopPropagation();
      copyEventJsonFromButton(copier);
      return;
    }

    // Click on a per-event row (but not the copy button) opens the full
    // detail panel for that event/alert in a modal.
    const evRow = e.target.closest('.lin-ev[data-event-id][data-event-kind]');
    if (evRow) {
      e.preventDefault();
      const id = evRow.dataset.eventId;
      const kind = evRow.dataset.eventKind;
      openLineageDetailModal(kind, id);
      return;
    }

    const head = e.target.closest('[data-node-toggle]');
    if (head) {
      const expanded = head.getAttribute('aria-expanded') === 'true';
      setNodeOpen(head, !expanded);
      return;
    }
    const sh = e.target.closest('[data-section-toggle]');
    if (sh) {
      const sec = sh.closest('.lin-section');
      const body = sec && sec.querySelector(':scope > .lin-section-body');
      const expanded = sh.getAttribute('aria-expanded') === 'true';
      sh.setAttribute('aria-expanded', expanded ? 'false' : 'true');
      if (body) body.hidden = expanded;
      return;
    }
  }

  // setNodeOpen flips the head's aria-expanded AND toggles the matching
  // body's hidden attribute. The body is the head's next sibling per
  // the template structure.
  function setNodeOpen(head, open) {
    head.setAttribute('aria-expanded', open ? 'true' : 'false');
    const body = head.nextElementSibling;
    if (body && body.classList.contains('lin-node-body')) {
      body.hidden = !open;
    }
  }

  // openProcessInfoModal shows a focused, modal-style view of a process
  // node's meta + alerts + per-category event sections. Reuses the
  // existing lin-node-body markup by cloning it into the modal — same
  // styling, same toggleable section state.
  function openProcessInfoModal(node) {
    if (!node) return;
    closeLineageDetailModal();
    const body = node.querySelector(':scope > .lin-node-body');
    if (!body) return;
    const pid = node.dataset.pid || '';
    const nameEl = node.querySelector(':scope > .lin-node-head .lin-name');
    const name = nameEl ? nameEl.textContent.trim() : '';
    const clone = body.cloneNode(true);
    clone.hidden = false;
    // Re-open every collapsible section inside the cloned body — the
    // user clicked "info" expecting to see everything.
    clone.querySelectorAll('[data-section-toggle]').forEach(function (sh) {
      sh.setAttribute('aria-expanded', 'true');
    });
    clone.querySelectorAll('.lin-section-body').forEach(function (b) { b.hidden = false; });

    const backdrop = document.createElement('div');
    backdrop.className = 'lin-detail-modal-backdrop';
    backdrop.id = 'lin-detail-modal-backdrop';
    const panel = document.createElement('div');
    panel.className = 'lin-detail-modal lin-process-modal';
    panel.innerHTML =
      '<div class="lin-process-modal-head">' +
        '<span class="lpm-key">process</span>' +
        '<span class="lpm-pid">' + escapeHtml(pid) + '</span>' +
        '<span class="lpm-name">' + escapeHtml(name) + '</span>' +
        '<button type="button" class="detail-close" data-lin-detail-close aria-label="close">' + ico('x') + '</button>' +
      '</div>' +
      '<div class="lin-detail-modal-body lin-process-modal-body"></div>';
    panel.querySelector('.lin-detail-modal-body').appendChild(clone);
    backdrop.appendChild(panel);
    document.body.appendChild(backdrop);
    document.body.classList.add('lin-modal-open');
    // The info modal is the user's "show me everything" surface — eagerly
    // fetch each event/alert's full /detail render and inject the field
    // list inline beneath the summary row, so they don't have to click
    // through to see all the fields.
    expandLineageEventFields(clone);
  }

  // expandLineageEventFields walks every event/alert row in the cloned
  // body and asks the server for that record's full detail HTML. The
  // header/copy/close chrome is stripped — only the field rows are
  // grafted in — so the modal reads as one continuous expanded view of
  // the process and everything it did.
  function expandLineageEventFields(clone) {
    const rows = clone.querySelectorAll('.lin-ev[data-event-id][data-event-kind]');
    rows.forEach(function (row) {
      const kind = row.dataset.eventKind;
      const id = row.dataset.eventId;
      if (!kind || !id) return;
      const fields = document.createElement('div');
      fields.className = 'lin-ev-fields';
      fields.innerHTML = '<div class="lin-ev-fields-loading">loading fields…</div>';
      row.insertAdjacentElement('afterend', fields);
      fetch('/detail/' + kind + '/' + id, { headers: { 'Accept': 'text/html' } })
        .then(function (r) { return r.text(); })
        .then(function (html) {
          const doc = new DOMParser().parseFromString(html, 'text/html');
          const body = doc.querySelector('.detail-body');
          if (!body) {
            fields.innerHTML = '<div class="lin-ev-fields-error">no detail available</div>';
            return;
          }
          // We re-render fields inside the lineage modal — strip the
          // detail chrome that doesn't belong here (header line, copy,
          // pivot, close, the hidden JSON source) and the click-to-
          // filter affordances (the modal has no query input).
          body.querySelectorAll(
            '.detail-header, .detail-copy, .detail-pivot, .detail-close, .detail-json-source'
          ).forEach(function (el) { el.remove(); });
          body.querySelectorAll('.q-click').forEach(function (el) {
            el.classList.remove('q-click');
            el.removeAttribute('data-q-field');
            el.removeAttribute('data-q-key');
            el.removeAttribute('data-q-val');
          });
          fields.innerHTML = '';
          fields.appendChild(body);
        })
        .catch(function () {
          fields.innerHTML = '<div class="lin-ev-fields-error">load failed</div>';
        });
    });
  }

  // openLineageDetailModal pulls the existing /detail/{kind}/{id} HTML,
  // extracts just the inner .detail-body (the table-row wrapper is for
  // timeline use), and shows it in a floating modal. Reuses the existing
  // detail template — same q-click annotations, same copy button, same
  // alert / event field rendering as the timeline detail panel.
  function openLineageDetailModal(kind, id) {
    if (!kind || !id) return;
    closeLineageDetailModal();
    const backdrop = document.createElement('div');
    backdrop.className = 'lin-detail-modal-backdrop';
    backdrop.id = 'lin-detail-modal-backdrop';
    const panel = document.createElement('div');
    panel.className = 'lin-detail-modal';
    panel.innerHTML = '<div class="lin-detail-modal-body"><div class="lin-detail-modal-loading">loading…</div></div>';
    backdrop.appendChild(panel);
    document.body.appendChild(backdrop);
    document.body.classList.add('lin-modal-open');

    fetch('/detail/' + kind + '/' + id, { headers: { 'Accept': 'text/html' } })
      .then(function (r) { return r.text(); })
      .then(function (html) {
        const doc = new DOMParser().parseFromString(html, 'text/html');
        const body = doc.querySelector('.detail-body');
        const inner = panel.querySelector('.lin-detail-modal-body');
        if (!inner) return;
        if (!body) {
          inner.innerHTML = '<div class="lin-detail-modal-error">record not found</div>';
          return;
        }
        // The lineage page has no query input to add to, so strip the
        // click-to-filter affordances. The fields just read as text.
        body.querySelectorAll('.q-click').forEach(function (el) {
          el.classList.remove('q-click');
          el.removeAttribute('data-q-field');
          el.removeAttribute('data-q-key');
          el.removeAttribute('data-q-val');
        });
        const closeBtn = body.querySelector('[data-close-detail]');
        if (closeBtn) {
          closeBtn.setAttribute('data-lin-detail-close', '');
          closeBtn.removeAttribute('data-close-detail');
        }
        inner.innerHTML = '';
        inner.appendChild(body);
      })
      .catch(function (err) {
        const inner = panel.querySelector('.lin-detail-modal-body');
        if (inner) inner.innerHTML = '<div class="lin-detail-modal-error">load failed</div>';
        console.error('rustinel-view: lineage modal fetch failed', err);
      });
  }

  function closeLineageDetailModal() {
    const m = document.getElementById('lin-detail-modal-backdrop');
    if (m) m.remove();
    document.body.classList.remove('lin-modal-open');
  }

  // copyEventJsonFromButton fetches the record's JSON from the server
  // and writes it to the clipboard. Visual feedback via the .copied
  // class on the button for ~1.2s.
  function copyEventJsonFromButton(btn) {
    const id = btn.dataset.id;
    const kind = btn.dataset.kind;
    if (!id || !kind) return;
    fetch('/json/' + kind + '/' + id)
      .then(function (r) { return r.text(); })
      .then(function (text) {
        if (navigator.clipboard && navigator.clipboard.writeText) {
          navigator.clipboard.writeText(text).then(function () { flashCopied(btn); });
        } else {
          legacyCopy(text);
          flashCopied(btn);
        }
      })
      .catch(function (err) { console.error('rustinel-view: copy event json failed', err); });
  }

  // scrollToLineageNode centers the node for the given pid (or the
  // origin when pid is falsy) and fires a one-shot outline pulse so the
  // eye lands on the right card after the scroll settles.
  function scrollToLineageNode(pid) {
    const node = pid
      ? document.querySelector('.lin-node[data-pid="' + pid + '"]')
      : document.querySelector('.lin-node.is-origin');
    if (!node) return;
    node.scrollIntoView({ behavior: 'smooth', block: 'center' });
    node.classList.remove('lin-flash');
    void node.offsetWidth; // restart the animation when re-triggered
    node.classList.add('lin-flash');
    setTimeout(function () { node.classList.remove('lin-flash'); }, 1300);
  }

  // Rail hover: while a node head is hovered, light the connector rails
  // from that node back up to the root so its ancestry pops out.
  let railHotHead = null;
  function onLineageRailHover(e) {
    const head = e.target.closest('.lin-node-head');
    if (head === railHotHead) return;
    railHotHead = head;
    document.querySelectorAll('.lin-branch.rail-hot').forEach(function (b) {
      b.classList.remove('rail-hot');
    });
    if (!head) return;
    let b = head.closest('.lin-branch');
    while (b) {
      b.classList.add('rail-hot');
      b = b.parentElement && b.parentElement.closest('.lin-branch');
    }
  }

  // setAllNodes drives every node + every section toggle on the page.
  // Expand-all opens both layers; collapse-all closes them.
  function setAllNodes(open) {
    document.querySelectorAll('[data-node-toggle]').forEach(function (h) {
      setNodeOpen(h, open);
    });
    document.querySelectorAll('[data-section-toggle]').forEach(function (sh) {
      sh.setAttribute('aria-expanded', open ? 'true' : 'false');
      const sec = sh.closest('.lin-section');
      const body = sec && sec.querySelector(':scope > .lin-section-body');
      if (body) body.hidden = !open;
    });
  }

  // Time range selection lives on the density strip (brush-drag in
  // onDensityMouseUp → applyTimeRange writes the hidden #time-from /
  // #time-to inputs). No popover, no presets, no panel JS. Reset
  // button clears the inputs via the existing form reset handler.

  // ---------- tooltips ----------
  //
  // A single floating <div id="rv-tip"> appended to <body>, positioned
  // by JS. The CSS [data-tip]::after pseudo-element used to overflow
  // the viewport near right/top edges — pure CSS can't read the
  // viewport, so it always placed the tip centered above (or below)
  // the trigger. This manager measures both the trigger and the tip,
  // then clamps the result inside the visible window. Words never
  // break mid-character because the CSS uses word-break: normal.

  let rvTip = null;
  let rvTipFor = null;      // element whose tip is currently SHOWN
  let rvTipPending = null;  // element whose tip is SCHEDULED but not yet shown
  let rvTipShowTimer = 0;
  let rvTipWatchRaf = 0;    // rAF id for the disappear-watchdog
  const RV_TIP_DELAY = 250;
  const RV_TIP_EDGE = 8;    // viewport margin (px) the tip will never cross

  function initTooltips() {
    rvTip = document.createElement('div');
    rvTip.id = 'rv-tip';
    rvTip.setAttribute('role', 'tooltip');
    document.body.appendChild(rvTip);

    // Pointerenter/leave don't bubble, so we delegate via mouseover/out
    // at the document. Hover state must survive DOM rebuilds (the
    // thead is rewritten on every column move, virtual scroll recycles
    // rows, etc.) — see the watchdog below for the cleanup cases.
    document.addEventListener('mouseover', onTipMouseOver);
    document.addEventListener('mouseout', onTipMouseOut);
    document.addEventListener('focusin', onTipFocusIn);
    document.addEventListener('focusout', onTipFocusOut);
    // Any click resolves the hover (modals, popovers, navigation can
    // replace UI under the cursor without firing mouseout) — drop the
    // tip immediately rather than letting it linger.
    document.addEventListener('click', hideTip, true);
    // Hide on scroll / resize: stale positions look worse than no tip.
    // `true` captures scrolls on any inner scroller (.main, modals),
    // not just the window — scroll events don't bubble otherwise.
    window.addEventListener('scroll', hideTip, true);
    window.addEventListener('resize', hideTip);
    // Tab-away or window blur: prevent a tip orphaned on a no-longer-
    // hovered element when the user comes back.
    window.addEventListener('blur', hideTip);
    document.addEventListener('visibilitychange', function () {
      if (document.hidden) hideTip();
    });
  }

  function onTipMouseOver(e) {
    const t = e.target.closest('[data-tip]');
    if (!t) { cancelPendingTip(); return; }
    if (t === rvTipFor || t === rvTipPending) return;
    cancelPendingTip();
    scheduleTip(t);
  }
  function onTipMouseOut(e) {
    const t = e.target.closest('[data-tip]');
    if (!t) return;
    // Hover may move into a descendant of the same trigger — only hide
    // when the pointer truly leaves the [data-tip] element.
    if (e.relatedTarget && t.contains(e.relatedTarget)) return;
    if (t === rvTipPending) cancelPendingTip();
    if (t === rvTipFor) hideTip();
  }
  function onTipFocusIn(e) {
    const t = e.target.closest && e.target.closest('[data-tip]');
    if (t) { cancelPendingTip(); scheduleTip(t); }
  }
  function onTipFocusOut(e) {
    const t = e.target.closest && e.target.closest('[data-tip]');
    if (!t) return;
    if (t === rvTipPending) cancelPendingTip();
    if (t === rvTipFor) hideTip();
  }

  function scheduleTip(t) {
    clearTimeout(rvTipShowTimer);
    rvTipPending = t;
    rvTipShowTimer = setTimeout(function () {
      // Pending may have been cleared (cursor left during the delay,
      // element was removed) — bail if so.
      if (rvTipPending !== t) return;
      rvTipPending = null;
      if (!t.isConnected) return;
      showTip(t);
    }, RV_TIP_DELAY);
  }

  function cancelPendingTip() {
    clearTimeout(rvTipShowTimer);
    rvTipPending = null;
  }

  function showTip(t) {
    const text = t.getAttribute('data-tip');
    if (!text || !rvTip) return;
    rvTip.textContent = text;
    rvTipFor = t;
    // Make it laid out but invisible so we can measure.
    rvTip.style.left = '0px';
    rvTip.style.top = '0px';
    rvTip.style.visibility = 'hidden';
    rvTip.classList.add('show');
    positionTip(t);
    rvTip.style.visibility = '';
    startTipWatchdog();
  }

  function hideTip() {
    cancelPendingTip();
    if (!rvTip) return;
    rvTip.classList.remove('show');
    rvTipFor = null;
    stopTipWatchdog();
  }

  // While a tip is showing, every animation frame check whether the
  // trigger is still attached and still visible. Catches:
  //   - rows recycled by virtual scroll (mouseout never fires)
  //   - thead rebuilt by column reorder
  //   - element hidden by `display: none`
  // Light-touch: a single rAF tick per frame while shown; stops the
  // moment a tip is hidden.
  function startTipWatchdog() {
    if (rvTipWatchRaf) return;
    rvTipWatchRaf = requestAnimationFrame(watchTip);
  }
  function stopTipWatchdog() {
    if (rvTipWatchRaf) cancelAnimationFrame(rvTipWatchRaf);
    rvTipWatchRaf = 0;
  }
  function watchTip() {
    rvTipWatchRaf = 0;
    if (!rvTipFor) return;
    if (!rvTipFor.isConnected) { hideTip(); return; }
    const r = rvTipFor.getBoundingClientRect();
    if (r.width === 0 && r.height === 0) { hideTip(); return; }
    rvTipWatchRaf = requestAnimationFrame(watchTip);
  }

  // positionTip places the tip on the side indicated by data-tip-pos
  // ("below" → under the trigger; otherwise above), centered horizontally
  // over the trigger. The horizontal center is then clamped so the tip
  // never crosses RV_TIP_EDGE from either viewport edge. If the chosen
  // side overflows top/bottom, the opposite side is used.
  function positionTip(t) {
    const r = t.getBoundingClientRect();
    const tw = rvTip.offsetWidth;
    const th = rvTip.offsetHeight;
    const vw = window.innerWidth;
    const vh = window.innerHeight;
    const wantsBelow = t.getAttribute('data-tip-pos') === 'below';
    const gap = 8;

    let top = wantsBelow ? r.bottom + gap : r.top - gap - th;
    if (top + th + RV_TIP_EDGE > vh) top = r.top - gap - th;          // flip up
    if (top < RV_TIP_EDGE) top = r.bottom + gap;                       // flip down
    top = Math.max(RV_TIP_EDGE, Math.min(top, vh - th - RV_TIP_EDGE));

    let left = r.left + (r.width - tw) / 2;
    left = Math.max(RV_TIP_EDGE, Math.min(left, vw - tw - RV_TIP_EDGE));

    rvTip.style.left = left + 'px';
    rvTip.style.top = top + 'px';
  }

  // ---------- boot ----------

  if (document.readyState === 'loading') {
    document.addEventListener('DOMContentLoaded', init);
  } else {
    init();
  }
})();
