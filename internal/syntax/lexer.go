package syntax

import (
	"strings"
)

type lexer struct {
	src  []rune
	off  int
	line int
	col  int
	file string
}

func newLexer(src string, start Pos) *lexer {
	return &lexer{src: []rune(src), line: start.Line, col: start.Col, file: start.File}
}

func (l *lexer) pos() Pos { return Pos{File: l.file, Line: l.line, Col: l.col} }

func (l *lexer) peekRune(ahead int) rune {
	if l.off+ahead >= len(l.src) {
		return 0
	}
	return l.src[l.off+ahead]
}

func (l *lexer) next() rune {
	r := l.src[l.off]
	l.off++
	if r == '\n' {
		l.line++
		l.col = 1
	} else {
		l.col++
	}
	return r
}

func (l *lexer) errf(p Pos, format string, args ...any) error {
	return &Error{Pos: p, Msg: sprintf(format, args...)}
}

// tokenize lexes the whole input.
func (l *lexer) tokenize() ([]token, error) {
	var toks []token
	for {
		t, err := l.scan()
		if err != nil {
			return nil, err
		}
		toks = append(toks, t)
		if t.kind == tokEOF {
			return toks, nil
		}
	}
}

func (l *lexer) skipSpaceAndComments() error {
	for l.off < len(l.src) {
		r := l.peekRune(0)
		switch {
		case r == ' ' || r == '\t' || r == '\r':
			l.next()
		case r == '#' || (r == '/' && l.peekRune(1) == '/'):
			for l.off < len(l.src) && l.peekRune(0) != '\n' {
				l.next()
			}
		case r == '/' && l.peekRune(1) == '*':
			start := l.pos()
			l.next()
			l.next()
			for {
				if l.off >= len(l.src) {
					return l.errf(start, "unterminated block comment")
				}
				if l.peekRune(0) == '*' && l.peekRune(1) == '/' {
					l.next()
					l.next()
					break
				}
				l.next()
			}
		default:
			return nil
		}
	}
	return nil
}

func (l *lexer) scan() (token, error) {
	if err := l.skipSpaceAndComments(); err != nil {
		return token{}, err
	}
	p := l.pos()
	if l.off >= len(l.src) {
		return token{kind: tokEOF, pos: p}, nil
	}
	r := l.peekRune(0)
	switch {
	case r == '\n':
		l.next()
		return token{kind: tokNewline, pos: p}, nil
	case isIdentStart(r):
		start := l.off
		for l.off < len(l.src) && isIdentPart(l.peekRune(0)) {
			l.next()
		}
		return token{kind: tokIdent, text: string(l.src[start:l.off]), pos: p}, nil
	case r >= '0' && r <= '9':
		return l.scanNumber(p), nil
	case r == '"':
		parts, err := l.scanQuoted(p)
		if err != nil {
			return token{}, err
		}
		return token{kind: tokTemplate, parts: parts, pos: p}, nil
	case r == '<' && l.peekRune(1) == '<':
		parts, err := l.scanHeredoc(p)
		if err != nil {
			return token{}, err
		}
		return token{kind: tokTemplate, parts: parts, pos: p}, nil
	}
	two := string([]rune{r, l.peekRune(1)})
	twoTok := map[string]tokenKind{
		"==": tokEq, "!=": tokNe, "<=": tokLe, ">=": tokGe,
		"&&": tokAnd, "||": tokOr, "=>": tokArrow,
	}
	if k, ok := twoTok[two]; ok {
		l.next()
		l.next()
		return token{kind: k, text: two, pos: p}, nil
	}
	if r == '.' && l.peekRune(1) == '.' && l.peekRune(2) == '.' {
		l.next()
		l.next()
		l.next()
		return token{kind: tokEllipsis, text: "...", pos: p}, nil
	}
	oneTok := map[rune]tokenKind{
		'{': tokLBrace, '}': tokRBrace, '[': tokLBrack, ']': tokRBrack,
		'(': tokLParen, ')': tokRParen, '=': tokAssign, ',': tokComma,
		'.': tokDot, ':': tokColon, '?': tokQuestion, '+': tokPlus,
		'-': tokMinus, '*': tokStar, '/': tokSlash, '%': tokPercent,
		'<': tokLt, '>': tokGt, '!': tokBang,
	}
	if k, ok := oneTok[r]; ok {
		l.next()
		return token{kind: k, text: string(r), pos: p}, nil
	}
	return token{}, l.errf(p, "unexpected character %q", r)
}

func (l *lexer) scanNumber(p Pos) token {
	start := l.off
	for isDigit(l.peekRune(0)) {
		l.next()
	}
	if l.peekRune(0) == '.' && isDigit(l.peekRune(1)) {
		l.next()
		for isDigit(l.peekRune(0)) {
			l.next()
		}
	}
	if e := l.peekRune(0); e == 'e' || e == 'E' {
		n := 1
		if s := l.peekRune(1); s == '+' || s == '-' {
			n = 2
		}
		if isDigit(l.peekRune(n)) {
			for range n {
				l.next()
			}
			for isDigit(l.peekRune(0)) {
				l.next()
			}
		}
	}
	return token{kind: tokNumber, text: string(l.src[start:l.off]), pos: p}
}

// scanQuoted lexes a double-quoted template, processing escapes in literal
// segments and capturing interpolation sources verbatim.
func (l *lexer) scanQuoted(p Pos) ([]templatePart, error) {
	l.next() // opening quote
	var parts []templatePart
	var lit strings.Builder
	flush := func() {
		if lit.Len() > 0 {
			parts = append(parts, templatePart{lit: lit.String()})
			lit.Reset()
		}
	}
	for {
		if l.off >= len(l.src) || l.peekRune(0) == '\n' {
			return nil, l.errf(p, "unterminated string")
		}
		r := l.peekRune(0)
		switch {
		case r == '"':
			l.next()
			flush()
			return parts, nil
		case r == '\\':
			ep := l.pos()
			l.next()
			if l.off >= len(l.src) {
				return nil, l.errf(ep, "unterminated escape")
			}
			e := l.next()
			switch e {
			case 'n':
				lit.WriteRune('\n')
			case 't':
				lit.WriteRune('\t')
			case 'r':
				lit.WriteRune('\r')
			case '"':
				lit.WriteRune('"')
			case '\\':
				lit.WriteRune('\\')
			default:
				return nil, l.errf(ep, "invalid escape sequence \\%c", e)
			}
		case r == '$' && l.peekRune(1) == '$' && l.peekRune(2) == '{':
			l.next()
			l.next()
			l.next()
			lit.WriteString("${")
		case r == '$' && l.peekRune(1) == '{':
			flush()
			part, err := l.scanInterpolation()
			if err != nil {
				return nil, err
			}
			parts = append(parts, part)
		default:
			lit.WriteRune(l.next())
		}
	}
}

// scanInterpolation consumes "${ ... }" and returns the inner source. Braces
// and quoted strings inside the interpolation are balanced.
func (l *lexer) scanInterpolation() (templatePart, error) {
	start := l.pos()
	l.next()
	l.next()
	inner := l.pos()
	begin := l.off
	depth := 0
	for {
		if l.off >= len(l.src) {
			return templatePart{}, l.errf(start, "unterminated interpolation")
		}
		r := l.peekRune(0)
		switch r {
		case '{':
			depth++
		case '}':
			if depth == 0 {
				src := string(l.src[begin:l.off])
				l.next()
				return templatePart{src: src, isExpr: true, pos: inner}, nil
			}
			depth--
		case '"':
			// Skip a nested quoted string, honouring escapes.
			l.next()
			for l.off < len(l.src) && l.peekRune(0) != '"' {
				if l.peekRune(0) == '\\' {
					l.next()
				}
				if l.off < len(l.src) {
					l.next()
				}
			}
		}
		if l.off < len(l.src) {
			l.next()
		}
	}
}

// scanHeredoc lexes <<EOT or <<-EOT heredocs. The indented form strips the
// smallest common leading whitespace from every line.
func (l *lexer) scanHeredoc(p Pos) ([]templatePart, error) {
	l.next()
	l.next()
	indent := false
	if l.peekRune(0) == '-' {
		indent = true
		l.next()
	}
	if !isIdentStart(l.peekRune(0)) {
		return nil, l.errf(p, "heredoc requires a delimiter identifier")
	}
	start := l.off
	for isIdentPart(l.peekRune(0)) {
		l.next()
	}
	delim := string(l.src[start:l.off])
	if l.peekRune(0) == '\r' {
		l.next()
	}
	if l.peekRune(0) != '\n' {
		return nil, l.errf(p, "heredoc delimiter must be followed by a newline")
	}
	l.next()
	bodyPos := l.pos()
	var lines []string
	for {
		if l.off >= len(l.src) {
			return nil, l.errf(p, "unterminated heredoc, expected %q", delim)
		}
		ls := l.off
		for l.off < len(l.src) && l.peekRune(0) != '\n' {
			l.next()
		}
		line := string(l.src[ls:l.off])
		if strings.TrimSpace(line) == delim {
			break
		}
		lines = append(lines, line)
		if l.off < len(l.src) {
			l.next()
		}
	}
	if indent {
		lines = dedent(lines)
	}
	body := ""
	if len(lines) > 0 {
		body = strings.Join(lines, "\n") + "\n"
	}
	return splitTemplate(body, bodyPos)
}

func dedent(lines []string) []string {
	minIndent := -1
	for _, ln := range lines {
		if strings.TrimSpace(ln) == "" {
			continue
		}
		n := len(ln) - len(strings.TrimLeft(ln, " \t"))
		if minIndent < 0 || n < minIndent {
			minIndent = n
		}
	}
	out := make([]string, len(lines))
	for i, ln := range lines {
		if len(ln) >= minIndent && minIndent > 0 {
			out[i] = ln[minIndent:]
		} else {
			out[i] = strings.TrimLeft(ln, " \t")
		}
	}
	return out
}

// splitTemplate splits raw heredoc text into literal and interpolation
// parts. Escapes are not processed in heredocs, except $${.
func splitTemplate(text string, start Pos) ([]templatePart, error) {
	sub := newLexer(text, start)
	var parts []templatePart
	var lit strings.Builder
	for sub.off < len(sub.src) {
		r := sub.peekRune(0)
		switch {
		case r == '$' && sub.peekRune(1) == '$' && sub.peekRune(2) == '{':
			sub.next()
			sub.next()
			sub.next()
			lit.WriteString("${")
		case r == '$' && sub.peekRune(1) == '{':
			if lit.Len() > 0 {
				parts = append(parts, templatePart{lit: lit.String()})
				lit.Reset()
			}
			part, err := sub.scanInterpolation()
			if err != nil {
				return nil, err
			}
			parts = append(parts, part)
		default:
			lit.WriteRune(sub.next())
		}
	}
	if lit.Len() > 0 || len(parts) == 0 {
		parts = append(parts, templatePart{lit: lit.String()})
	}
	return parts, nil
}

func isIdentStart(r rune) bool {
	return r == '_' || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z')
}

func isIdentPart(r rune) bool { return isIdentStart(r) || isDigit(r) || r == '-' }

func isDigit(r rune) bool { return r >= '0' && r <= '9' }
