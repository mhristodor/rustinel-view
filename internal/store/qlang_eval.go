package store

import (
	"strconv"
	"strings"
)

// EvalQuery evaluates a parsed query AST against one row. A nil expr
// matches everything (used as the no-query state).
func EvalQuery(expr QExpr, r Row) bool {
	if expr == nil {
		return true
	}
	switch e := expr.(type) {
	case QFieldTerm:
		return evalField(e, r)
	case QBareTerm:
		return evalBare(e, r)
	case QAnd:
		for _, c := range e.Children {
			if !EvalQuery(c, r) {
				return false
			}
		}
		return true
	case QOr:
		for _, c := range e.Children {
			if EvalQuery(c, r) {
				return true
			}
		}
		return false
	case QNot:
		return !EvalQuery(e.Child, r)
	}
	return false
}

// evalField resolves the field's value off the row and tests it
// against the term's value. Missing fields evaluate false — a row that
// doesn't have the field never matches a positive `field:value` test.
// (Negation handles the "I want rows that DON'T have this field" case
// via `NOT field:*`.)
func evalField(t QFieldTerm, r Row) bool {
	def := qSchemaByName(t.Name)
	if def == nil || def.extract == nil {
		return false
	}
	val, ok := def.extract(r)
	if !ok {
		// Field absent on this row — a positive test can never match.
		// (`field:*` therefore means "field is present": present values
		// wildcard-match `*` below; absent rows fail here. `NOT field:*`
		// selects rows lacking the field.)
		return false
	}
	return matchValue(val, t)
}

// matchValue dispatches on the term's operator: numeric comparison for
// >, >=, <, <=; case-insensitive equality or `*` wildcard matching for
// the default `:` operator.
func matchValue(rowVal string, t QFieldTerm) bool {
	if t.Op != ":" {
		return matchNumeric(rowVal, t)
	}
	want := t.Value
	got := rowVal
	if !t.Wildcard {
		// IPs are exact; enums are case-insensitive; strings are CI.
		return strings.EqualFold(got, want)
	}
	got = strings.ToLower(got)
	want = strings.ToLower(want)
	return wildcardMatch(got, want)
}

// matchNumeric applies a >, >=, < or <= comparison. Both sides must
// parse as integers; rows whose field value isn't numeric never match
// a comparison term.
func matchNumeric(rowVal string, t QFieldTerm) bool {
	got, err1 := strconv.ParseInt(strings.TrimSpace(rowVal), 10, 64)
	want, err2 := strconv.ParseInt(t.Value, 10, 64)
	if err1 != nil || err2 != nil {
		return false
	}
	switch t.Op {
	case ">":
		return got > want
	case ">=":
		return got >= want
	case "<":
		return got < want
	case "<=":
		return got <= want
	}
	return false
}

// wildcardMatch handles `*` glob-style matching. Multiple stars allowed:
// `*foo*bar*` matches anything containing foo followed (anywhere later)
// by bar.
func wildcardMatch(s, pat string) bool {
	parts := strings.Split(pat, "*")
	if len(parts) == 1 {
		return s == pat
	}
	// First part must be a prefix (unless empty).
	if parts[0] != "" {
		if !strings.HasPrefix(s, parts[0]) {
			return false
		}
		s = s[len(parts[0]):]
	}
	// Middle parts must be found in order, leaving suffix.
	for i := 1; i < len(parts)-1; i++ {
		idx := strings.Index(s, parts[i])
		if idx < 0 {
			return false
		}
		s = s[idx+len(parts[i]):]
	}
	// Last part must be a suffix (unless empty).
	last := parts[len(parts)-1]
	if last == "" {
		return true
	}
	return strings.HasSuffix(s, last)
}

// evalBare does case-insensitive substring search across every known
// string field of the row. This is the "/foo" Splunk-style free-text
// search; it's deliberately broad so users can grep without thinking.
func evalBare(t QBareTerm, r Row) bool {
	needle := strings.ToLower(t.Value)
	if needle == "" {
		return true
	}
	for i := range QSchema {
		if QSchema[i].extract == nil {
			continue
		}
		v, ok := QSchema[i].extract(r)
		if !ok {
			continue
		}
		if strings.Contains(strings.ToLower(v), needle) {
			return true
		}
	}
	return false
}
