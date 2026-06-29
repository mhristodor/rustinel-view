package store

import (
	"fmt"
	"strings"
	"unicode"
)

// QExpr is the parsed query AST. A nil QExpr means "match everything"
// (used as the default Filters.Query state).
type QExpr interface {
	isQExpr()
}

// QFieldTerm is a single `field:value` test. Op is ":" for v1; numeric
// comparisons (">", "<") deferred.
type QFieldTerm struct {
	Name     string
	Op       string
	Value    string
	Wildcard bool // true if Value contains '*'
}

// QBareTerm is a free-text substring search across all known string
// fields of the row. Generated when a value appears without a leading
// "field:" qualifier.
type QBareTerm struct {
	Value string
}

type QAnd struct{ Children []QExpr }
type QOr struct{ Children []QExpr }
type QNot struct{ Child QExpr }

func (QFieldTerm) isQExpr() {}
func (QBareTerm) isQExpr()  {}
func (QAnd) isQExpr()       {}
func (QOr) isQExpr()        {}
func (QNot) isQExpr()       {}

// ParseQuery parses a KQL-style query string into an AST. An empty or
// whitespace-only input returns nil (match everything). A parse error
// is reported with the index of the offending token so the UI can
// underline it.
func ParseQuery(src string) (QExpr, error) {
	src = strings.TrimSpace(src)
	if src == "" {
		return nil, nil
	}
	p := &qparser{tokens: tokenize(src)}
	expr, err := p.parseOr()
	if err != nil {
		return nil, err
	}
	if p.pos != len(p.tokens) {
		return nil, fmt.Errorf("unexpected token %q at position %d", p.peek().val, p.peek().start)
	}
	return expr, nil
}

// ---------- lexer ----------

type qtokKind int

const (
	tokEOF qtokKind = iota
	tokIdent
	tokString // quoted "..."
	tokColon
	tokLParen
	tokRParen
)

type qtok struct {
	kind  qtokKind
	val   string
	start int
}

func tokenize(src string) []qtok {
	var out []qtok
	i := 0
	for i < len(src) {
		r := rune(src[i])
		if unicode.IsSpace(r) {
			i++
			continue
		}
		switch r {
		case '(':
			out = append(out, qtok{kind: tokLParen, val: "(", start: i})
			i++
		case ')':
			out = append(out, qtok{kind: tokRParen, val: ")", start: i})
			i++
		case ':':
			out = append(out, qtok{kind: tokColon, val: ":", start: i})
			i++
		case '"':
			start := i
			i++
			var sb strings.Builder
			for i < len(src) && src[i] != '"' {
				if src[i] == '\\' && i+1 < len(src) {
					sb.WriteByte(src[i+1])
					i += 2
					continue
				}
				sb.WriteByte(src[i])
				i++
			}
			if i < len(src) && src[i] == '"' {
				i++
			}
			out = append(out, qtok{kind: tokString, val: sb.String(), start: start})
		default:
			start := i
			for i < len(src) {
				c := src[i]
				if c == ' ' || c == '\t' || c == '(' || c == ')' || c == ':' || c == '"' {
					break
				}
				i++
			}
			out = append(out, qtok{kind: tokIdent, val: src[start:i], start: start})
		}
	}
	return out
}

// ---------- parser ----------

type qparser struct {
	tokens []qtok
	pos    int
}

func (p *qparser) peek() qtok {
	if p.pos >= len(p.tokens) {
		return qtok{kind: tokEOF}
	}
	return p.tokens[p.pos]
}

func (p *qparser) advance() qtok {
	t := p.peek()
	if p.pos < len(p.tokens) {
		p.pos++
	}
	return t
}

// parseOr — lowest precedence: a OR b (OR c)*
func (p *qparser) parseOr() (QExpr, error) {
	left, err := p.parseAnd()
	if err != nil {
		return nil, err
	}
	for {
		tk := p.peek()
		if tk.kind == tokIdent && strings.EqualFold(tk.val, "OR") {
			p.advance()
			right, err := p.parseAnd()
			if err != nil {
				return nil, err
			}
			left = mergeOr(left, right)
			continue
		}
		break
	}
	return left, nil
}

func mergeOr(a, b QExpr) QExpr {
	if ao, ok := a.(QOr); ok {
		ao.Children = append(ao.Children, b)
		return ao
	}
	return QOr{Children: []QExpr{a, b}}
}

// parseAnd — next: implicit-AND between adjacent factors, or explicit AND.
func (p *qparser) parseAnd() (QExpr, error) {
	left, err := p.parseUnary()
	if err != nil {
		return nil, err
	}
	for {
		tk := p.peek()
		if tk.kind == tokEOF || tk.kind == tokRParen {
			break
		}
		if tk.kind == tokIdent && strings.EqualFold(tk.val, "OR") {
			break
		}
		if tk.kind == tokIdent && strings.EqualFold(tk.val, "AND") {
			p.advance()
		}
		right, err := p.parseUnary()
		if err != nil {
			return nil, err
		}
		left = mergeAnd(left, right)
	}
	return left, nil
}

func mergeAnd(a, b QExpr) QExpr {
	if aa, ok := a.(QAnd); ok {
		aa.Children = append(aa.Children, b)
		return aa
	}
	return QAnd{Children: []QExpr{a, b}}
}

// parseUnary — handles a leading NOT.
func (p *qparser) parseUnary() (QExpr, error) {
	tk := p.peek()
	if tk.kind == tokIdent && strings.EqualFold(tk.val, "NOT") {
		p.advance()
		inner, err := p.parsePrimary()
		if err != nil {
			return nil, err
		}
		return QNot{Child: inner}, nil
	}
	return p.parsePrimary()
}

// parsePrimary — grouping, field:value, or bare term.
func (p *qparser) parsePrimary() (QExpr, error) {
	tk := p.peek()
	switch tk.kind {
	case tokLParen:
		p.advance()
		inner, err := p.parseOr()
		if err != nil {
			return nil, err
		}
		if p.peek().kind != tokRParen {
			return nil, fmt.Errorf("expected ')' at position %d", p.peek().start)
		}
		p.advance()
		return inner, nil
	case tokIdent:
		// Ident followed by colon = field:value. Otherwise bare term.
		if p.pos+1 < len(p.tokens) && p.tokens[p.pos+1].kind == tokColon {
			name := tk.val
			p.advance() // ident
			p.advance() // colon
			valTok := p.peek()
			if valTok.kind != tokIdent && valTok.kind != tokString {
				return nil, fmt.Errorf("expected value after ':' at position %d", valTok.start)
			}
			p.advance()
			val := valTok.val
			op := ":"
			// Numeric comparison prefixes (>=, <=, >, <) are recognized
			// only on unquoted values — a quoted ">1024" is a literal.
			if valTok.kind == tokIdent {
				for _, cand := range []string{">=", "<=", ">", "<"} {
					if strings.HasPrefix(val, cand) {
						op = cand
						val = val[len(cand):]
						break
					}
				}
				if op != ":" && val == "" {
					return nil, fmt.Errorf("expected number after %q at position %d", op, valTok.start)
				}
			}
			wild := op == ":" && strings.Contains(val, "*")
			return QFieldTerm{Name: name, Op: op, Value: val, Wildcard: wild}, nil
		}
		p.advance()
		return QBareTerm{Value: tk.val}, nil
	case tokString:
		p.advance()
		return QBareTerm{Value: tk.val}, nil
	default:
		return nil, fmt.Errorf("unexpected token %q at position %d", tk.val, tk.start)
	}
}
