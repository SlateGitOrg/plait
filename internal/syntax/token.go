// Package syntax implements a lexer and recursive-descent parser for the HCL
// subset plait understands: blocks, attributes, and an expression language
// with references, operators, function calls, templates, heredocs, splats
// and for-expressions.
package syntax

import "fmt"

// Pos is a source position.
type Pos struct {
	File string
	Line int
	Col  int
}

func (p Pos) String() string {
	if p.File == "" {
		return fmt.Sprintf("%d:%d", p.Line, p.Col)
	}
	return fmt.Sprintf("%s:%d:%d", p.File, p.Line, p.Col)
}

// Error is a positioned syntax error.
type Error struct {
	Pos Pos
	Msg string
}

func (e *Error) Error() string { return e.Pos.String() + ": " + e.Msg }

type tokenKind uint8

const (
	tokEOF tokenKind = iota
	tokNewline
	tokIdent
	tokNumber
	tokTemplate // quoted string or heredoc, possibly with interpolations
	tokLBrace
	tokRBrace
	tokLBrack
	tokRBrack
	tokLParen
	tokRParen
	tokAssign
	tokComma
	tokDot
	tokColon
	tokQuestion
	tokArrow // =>
	tokPlus
	tokMinus
	tokStar
	tokSlash
	tokPercent
	tokEq
	tokNe
	tokLt
	tokGt
	tokLe
	tokGe
	tokAnd
	tokOr
	tokBang
	tokEllipsis
)

var tokenNames = map[tokenKind]string{
	tokEOF: "end of file", tokNewline: "newline", tokIdent: "identifier",
	tokNumber: "number", tokTemplate: "string", tokLBrace: "'{'", tokRBrace: "'}'",
	tokLBrack: "'['", tokRBrack: "']'", tokLParen: "'('", tokRParen: "')'",
	tokAssign: "'='", tokComma: "','", tokDot: "'.'", tokColon: "':'",
	tokQuestion: "'?'", tokArrow: "'=>'", tokPlus: "'+'", tokMinus: "'-'",
	tokStar: "'*'", tokSlash: "'/'", tokPercent: "'%'", tokEq: "'=='",
	tokNe: "'!='", tokLt: "'<'", tokGt: "'>'", tokLe: "'<='", tokGe: "'>='",
	tokAnd: "'&&'", tokOr: "'||'", tokBang: "'!'", tokEllipsis: "'...'",
}

func (k tokenKind) String() string {
	if n, ok := tokenNames[k]; ok {
		return n
	}
	return "token"
}

// templatePart is either a literal chunk or the raw source of an
// interpolation, with the position where that source starts.
type templatePart struct {
	lit    string
	src    string
	isExpr bool
	pos    Pos
}

type token struct {
	kind  tokenKind
	text  string
	parts []templatePart
	pos   Pos
}
