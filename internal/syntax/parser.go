package syntax

import (
	"strconv"

	"github.com/archit2501/plait/internal/value"
)

// ParseFile parses a configuration file.
func ParseFile(name, src string) (*File, error) {
	p, err := newParser(src, Pos{File: name, Line: 1, Col: 1})
	if err != nil {
		return nil, err
	}
	body, err := p.parseBody(true)
	if err != nil {
		return nil, err
	}
	return &File{Name: name, Body: body}, nil
}

// ParseExpr parses a standalone expression, as used for -var values and
// tests.
func ParseExpr(name, src string) (Expr, error) {
	p, err := newParser(src, Pos{File: name, Line: 1, Col: 1})
	if err != nil {
		return nil, err
	}
	p.ignoreNL = append(p.ignoreNL, true)
	e, err := p.parseExpr()
	if err != nil {
		return nil, err
	}
	if t := p.peek(); t.kind != tokEOF {
		return nil, p.unexpected(t, "end of expression")
	}
	return e, nil
}

type parser struct {
	toks     []token
	i        int
	ignoreNL []bool // stack: are newlines insignificant in the current nesting?
}

func newParser(src string, start Pos) (*parser, error) {
	toks, err := newLexer(src, start).tokenize()
	if err != nil {
		return nil, err
	}
	return &parser{toks: toks}, nil
}

func (p *parser) skippingNL() bool {
	return len(p.ignoreNL) > 0 && p.ignoreNL[len(p.ignoreNL)-1]
}

// peekIdx returns the index of the next significant token without
// consuming anything.
func (p *parser) peekIdx() int {
	i := p.i
	if p.skippingNL() {
		for p.toks[i].kind == tokNewline {
			i++
		}
	}
	return i
}

func (p *parser) peek() token { return p.toks[p.peekIdx()] }

func (p *parser) advance() token {
	i := p.peekIdx()
	t := p.toks[i]
	if t.kind != tokEOF {
		i++
	}
	p.i = i
	return t
}

func (p *parser) skipNewlines() {
	for p.toks[p.i].kind == tokNewline {
		p.i++
	}
}

func (p *parser) expect(k tokenKind, context string) (token, error) {
	t := p.peek()
	if t.kind != k {
		return t, p.unexpected(t, k.String()+" "+context)
	}
	return p.advance(), nil
}

func (p *parser) unexpected(t token, want string) error {
	got := t.kind.String()
	if t.kind == tokIdent || t.kind == tokNumber {
		got += " " + strconv.Quote(t.text)
	}
	return &Error{Pos: t.pos, Msg: "expected " + want + ", got " + got}
}

func (p *parser) push(ignore bool) { p.ignoreNL = append(p.ignoreNL, ignore) }
func (p *parser) pop()             { p.ignoreNL = p.ignoreNL[:len(p.ignoreNL)-1] }

// parseBody parses attributes and blocks until '}' (or EOF at top level).
func (p *parser) parseBody(top bool) (*Body, error) {
	p.push(false)
	defer p.pop()
	body := &Body{Pos: p.peek().pos}
	seen := map[string]Pos{}
	for {
		p.skipNewlines()
		t := p.peek()
		switch {
		case t.kind == tokEOF && top:
			return body, nil
		case t.kind == tokRBrace && !top:
			return body, nil
		case t.kind == tokEOF:
			return nil, p.unexpected(t, "'}' to close block")
		case t.kind != tokIdent:
			return nil, p.unexpected(t, "attribute or block")
		}
		name := p.advance()
		next := p.peek()
		if next.kind == tokAssign {
			p.advance()
			if prev, dup := seen[name.text]; dup {
				return nil, &Error{Pos: name.pos, Msg: sprintf("duplicate attribute %q (first set at %s)", name.text, prev)}
			}
			seen[name.text] = name.pos
			p.push(false)
			expr, err := p.parseExpr()
			p.pop()
			if err != nil {
				return nil, err
			}
			body.Attributes = append(body.Attributes, &Attribute{Name: name.text, Expr: expr, Pos: name.pos})
			if err := p.endOfLine(); err != nil {
				return nil, err
			}
			continue
		}
		blk := &Block{Type: name.text, Pos: name.pos}
		for {
			lt := p.peek()
			if lt.kind == tokLBrace {
				break
			}
			switch {
			case lt.kind == tokIdent:
				blk.Labels = append(blk.Labels, lt.text)
			case lt.kind == tokTemplate && len(lt.parts) <= 1 && (len(lt.parts) == 0 || !lt.parts[0].isExpr):
				lbl := ""
				if len(lt.parts) == 1 {
					lbl = lt.parts[0].lit
				}
				blk.Labels = append(blk.Labels, lbl)
			default:
				return nil, p.unexpected(lt, "block label or '{' after "+strconv.Quote(name.text))
			}
			p.advance()
		}
		p.advance() // '{'
		inner, err := p.parseBody(false)
		if err != nil {
			return nil, err
		}
		if _, err := p.expect(tokRBrace, "to close block "+strconv.Quote(name.text)); err != nil {
			return nil, err
		}
		blk.Body = inner
		body.Blocks = append(body.Blocks, blk)
		if err := p.endOfLine(); err != nil {
			return nil, err
		}
	}
}

func (p *parser) endOfLine() error {
	t := p.toks[p.i]
	switch t.kind {
	case tokNewline:
		p.i++
		return nil
	case tokEOF, tokRBrace:
		return nil
	}
	return p.unexpected(t, "newline")
}

func (p *parser) parseExpr() (Expr, error) {
	cond, err := p.parseBinary(0)
	if err != nil {
		return nil, err
	}
	if p.peek().kind != tokQuestion {
		return cond, nil
	}
	q := p.advance()
	p.push(true)
	defer p.pop()
	t, err := p.parseExpr()
	if err != nil {
		return nil, err
	}
	if _, err := p.expect(tokColon, "in conditional expression"); err != nil {
		return nil, err
	}
	f, err := p.parseExpr()
	if err != nil {
		return nil, err
	}
	return &ConditionalExpr{Cond: cond, True: t, False: f, Pos: q.pos}, nil
}

// binary operator precedence, lowest first.
var binaryLevels = [][]tokenKind{
	{tokOr},
	{tokAnd},
	{tokEq, tokNe},
	{tokLt, tokGt, tokLe, tokGe},
	{tokPlus, tokMinus},
	{tokStar, tokSlash, tokPercent},
}

func (p *parser) parseBinary(level int) (Expr, error) {
	if level == len(binaryLevels) {
		return p.parseUnary()
	}
	lhs, err := p.parseBinary(level + 1)
	if err != nil {
		return nil, err
	}
	for {
		t := p.peek()
		if !containsKind(binaryLevels[level], t.kind) {
			return lhs, nil
		}
		p.advance()
		rhs, err := p.parseBinary(level + 1)
		if err != nil {
			return nil, err
		}
		lhs = &BinaryExpr{Op: t.text, LHS: lhs, RHS: rhs, Pos: t.pos}
	}
}

func containsKind(ks []tokenKind, k tokenKind) bool {
	for _, x := range ks {
		if x == k {
			return true
		}
	}
	return false
}

func (p *parser) parseUnary() (Expr, error) {
	t := p.peek()
	if t.kind == tokMinus || t.kind == tokBang {
		p.advance()
		x, err := p.parseUnary()
		if err != nil {
			return nil, err
		}
		return &UnaryExpr{Op: t.text, X: x, Pos: t.pos}, nil
	}
	return p.parsePostfix()
}

func (p *parser) parsePostfix() (Expr, error) {
	prim, err := p.parsePrimary()
	if err != nil {
		return nil, err
	}
	var steps []Step
	for {
		t := p.toks[p.i] // postfix steps never cross a newline at statement level
		if p.skippingNL() {
			t = p.peek()
		}
		switch t.kind {
		case tokDot:
			p.advance()
			n := p.advance()
			switch n.kind {
			case tokIdent:
				steps = append(steps, AttrStep{Name: n.text})
			case tokNumber:
				idx, err := strconv.Atoi(n.text)
				if err != nil {
					return nil, &Error{Pos: n.pos, Msg: "invalid legacy index " + n.text}
				}
				steps = append(steps, IndexStep{Key: &LiteralExpr{Val: value.Int(idx), Pos: n.pos}})
			case tokStar:
				steps = append(steps, SplatStep{})
			default:
				return nil, p.unexpected(n, "attribute name after '.'")
			}
		case tokLBrack:
			p.advance()
			p.push(true)
			if p.peek().kind == tokStar {
				p.advance()
				steps = append(steps, SplatStep{})
			} else {
				k, err := p.parseExpr()
				if err != nil {
					p.pop()
					return nil, err
				}
				steps = append(steps, IndexStep{Key: k})
			}
			_, err := p.expect(tokRBrack, "to close index")
			p.pop()
			if err != nil {
				return nil, err
			}
		default:
			if len(steps) == 0 {
				return prim, nil
			}
			if tr, ok := prim.(*TraversalExpr); ok && tr.Source == nil && len(tr.Steps) == 0 {
				tr.Steps = steps
				return tr, nil
			}
			return &TraversalExpr{Source: prim, Steps: steps, Pos: prim.Start()}, nil
		}
	}
}

func (p *parser) parsePrimary() (Expr, error) {
	t := p.peek()
	switch t.kind {
	case tokNumber:
		p.advance()
		f, err := strconv.ParseFloat(t.text, 64)
		if err != nil {
			return nil, &Error{Pos: t.pos, Msg: "invalid number " + t.text}
		}
		return &LiteralExpr{Val: value.Number(f), Pos: t.pos}, nil
	case tokTemplate:
		p.advance()
		return p.buildTemplate(t)
	case tokIdent:
		p.advance()
		switch t.text {
		case "true":
			return &LiteralExpr{Val: value.True, Pos: t.pos}, nil
		case "false":
			return &LiteralExpr{Val: value.False, Pos: t.pos}, nil
		case "null":
			return &LiteralExpr{Val: value.Null, Pos: t.pos}, nil
		}
		if p.toks[p.i].kind == tokLParen {
			return p.parseCall(t)
		}
		return &TraversalExpr{Root: t.text, Pos: t.pos}, nil
	case tokLParen:
		p.advance()
		p.push(true)
		defer p.pop()
		e, err := p.parseExpr()
		if err != nil {
			return nil, err
		}
		if _, err := p.expect(tokRParen, "to close parenthesis"); err != nil {
			return nil, err
		}
		return e, nil
	case tokLBrack:
		return p.parseTuple()
	case tokLBrace:
		return p.parseObject()
	}
	return nil, p.unexpected(t, "expression")
}

func (p *parser) parseCall(name token) (Expr, error) {
	p.advance() // '('
	p.push(true)
	defer p.pop()
	call := &FuncCallExpr{Name: name.text, Pos: name.pos}
	for p.peek().kind != tokRParen {
		a, err := p.parseExpr()
		if err != nil {
			return nil, err
		}
		call.Args = append(call.Args, a)
		if p.peek().kind == tokEllipsis {
			p.advance()
			call.ExpandFinal = true
			if p.peek().kind != tokRParen {
				return nil, p.unexpected(p.peek(), "')' after '...'")
			}
			break
		}
		if p.peek().kind == tokComma {
			p.advance()
			continue
		}
		if p.peek().kind != tokRParen {
			return nil, p.unexpected(p.peek(), "',' or ')' in argument list")
		}
	}
	p.advance()
	return call, nil
}

func (p *parser) parseTuple() (Expr, error) {
	open := p.advance()
	p.push(true)
	defer p.pop()
	if t := p.peek(); t.kind == tokIdent && t.text == "for" {
		return p.parseFor(open, false)
	}
	tup := &TupleExpr{Pos: open.pos}
	for p.peek().kind != tokRBrack {
		e, err := p.parseExpr()
		if err != nil {
			return nil, err
		}
		tup.Elems = append(tup.Elems, e)
		if p.peek().kind == tokComma {
			p.advance()
			continue
		}
		if p.peek().kind != tokRBrack {
			return nil, p.unexpected(p.peek(), "',' or ']' in list")
		}
	}
	p.advance()
	return tup, nil
}

func (p *parser) parseObject() (Expr, error) {
	open := p.advance()
	p.push(false)
	defer p.pop()
	p.skipNewlines()
	if t := p.peek(); t.kind == tokIdent && t.text == "for" {
		p.push(true)
		defer p.pop()
		return p.parseFor(open, true)
	}
	obj := &ObjectExpr{Pos: open.pos}
	for {
		p.skipNewlines()
		t := p.peek()
		if t.kind == tokRBrace {
			p.advance()
			return obj, nil
		}
		var key Expr
		switch t.kind {
		case tokIdent:
			p.advance()
			key = &LiteralExpr{Val: value.String(t.text), Pos: t.pos}
		case tokTemplate:
			p.advance()
			k, err := p.buildTemplate(t)
			if err != nil {
				return nil, err
			}
			key = k
		case tokLParen:
			k, err := p.parsePrimary()
			if err != nil {
				return nil, err
			}
			key = k
		default:
			return nil, p.unexpected(t, "object key")
		}
		if sep := p.peek(); sep.kind != tokAssign && sep.kind != tokColon {
			return nil, p.unexpected(sep, "'=' or ':' after object key")
		}
		p.advance()
		p.push(false)
		val, err := p.parseExpr()
		p.pop()
		if err != nil {
			return nil, err
		}
		obj.Items = append(obj.Items, ObjectItem{Key: key, Val: val})
		switch p.toks[p.i].kind {
		case tokComma, tokNewline:
			p.i++
		case tokRBrace:
		default:
			return nil, p.unexpected(p.toks[p.i], "',', newline or '}' after object item")
		}
	}
}

// parseFor parses the remainder of a for-expression after its opening
// bracket; "for" is the current token.
func (p *parser) parseFor(open token, object bool) (Expr, error) {
	p.advance() // for
	fe := &ForExpr{Pos: open.pos}
	first, err := p.expect(tokIdent, "iteration variable after 'for'")
	if err != nil {
		return nil, err
	}
	fe.ValVar = first.text
	if p.peek().kind == tokComma {
		p.advance()
		second, err := p.expect(tokIdent, "second iteration variable")
		if err != nil {
			return nil, err
		}
		fe.KeyVar, fe.ValVar = first.text, second.text
	}
	if in := p.advance(); in.kind != tokIdent || in.text != "in" {
		return nil, p.unexpected(in, "'in'")
	}
	if fe.Coll, err = p.parseExpr(); err != nil {
		return nil, err
	}
	if _, err := p.expect(tokColon, "after for collection"); err != nil {
		return nil, err
	}
	first2, err := p.parseExpr()
	if err != nil {
		return nil, err
	}
	if object {
		if _, err := p.expect(tokArrow, "'=>' in object for-expression"); err != nil {
			return nil, err
		}
		fe.KeyExpr = first2
		if fe.ValExpr, err = p.parseExpr(); err != nil {
			return nil, err
		}
	} else {
		fe.ValExpr = first2
	}
	if t := p.peek(); t.kind == tokIdent && t.text == "if" {
		p.advance()
		if fe.Cond, err = p.parseExpr(); err != nil {
			return nil, err
		}
	}
	closeKind := tokRBrack
	if object {
		closeKind = tokRBrace
	}
	if _, err := p.expect(closeKind, "to close for-expression"); err != nil {
		return nil, err
	}
	return fe, nil
}

// buildTemplate converts a template token into an expression, parsing each
// interpolation with a sub-parser positioned at its source location.
func (p *parser) buildTemplate(t token) (Expr, error) {
	if len(t.parts) == 0 {
		return &LiteralExpr{Val: value.String(""), Pos: t.pos}, nil
	}
	if len(t.parts) == 1 && !t.parts[0].isExpr {
		return &LiteralExpr{Val: value.String(t.parts[0].lit), Pos: t.pos}, nil
	}
	tmpl := &TemplateExpr{Pos: t.pos}
	for _, part := range t.parts {
		if !part.isExpr {
			tmpl.Parts = append(tmpl.Parts, &LiteralExpr{Val: value.String(part.lit), Pos: t.pos})
			continue
		}
		sub, err := newParser(part.src, part.pos)
		if err != nil {
			return nil, err
		}
		sub.push(true)
		e, err := sub.parseExpr()
		if err != nil {
			return nil, err
		}
		if rest := sub.peek(); rest.kind != tokEOF {
			return nil, sub.unexpected(rest, "'}' to end interpolation")
		}
		tmpl.Parts = append(tmpl.Parts, e)
	}
	return tmpl, nil
}
