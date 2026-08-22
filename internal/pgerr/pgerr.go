// Package pgerr defines lightsql's structured error type and the SQLSTATE codes
// it reports.
//
// Every error that can reach a caller carries a SQLSTATE code, because that is
// the only portable way for application code to tell a unique violation from a
// not-null violation. Both pgx and lib/pq expose their error codes through an
// interface{ SQLState() string }, so *Error satisfies the same de-facto contract
// and existing errors.As-based handling keeps working against lightsql.
package pgerr

import (
	"fmt"
	"strings"

	"github.com/oxisto/lightsql/internal/token"
)

// SQLSTATE codes, named after the PostgreSQL condition names they correspond to.
const (
	SyntaxError             = "42601"
	UndefinedColumn         = "42703"
	UndefinedTable          = "42P01"
	UndefinedFunction       = "42883"
	UndefinedObject         = "42704"
	AmbiguousColumn         = "42702"
	GroupingError           = "42803"
	DuplicateTable          = "42P07"
	DuplicateColumn         = "42701"
	DatatypeMismatch        = "42804"
	InvalidTableDefinition  = "42P16"
	InvalidTextForType      = "22P02"
	NumericValueOutOfRange  = "22003"
	DivisionByZero          = "22012"
	CardinalityViolation    = "21000"
	NotNullViolation        = "23502"
	ForeignKeyViolation     = "23503"
	UniqueViolation         = "23505"
	CheckViolation          = "23514"
	SerializationFailure    = "40001"
	InvalidTransactionState = "25000"
	InFailedTransaction     = "25P02"
	NoActiveTransaction     = "25P01"
	ReadOnlySQLTransaction  = "25006"
	FeatureNotSupported     = "0A000"
	InternalError           = "XX000"
	// DataCorrupted reports a stored record that cannot be read back. A crash
	// may truncate the write-ahead log mid-write, so this is an expected
	// outcome of recovery rather than a sign of a bug.
	DataCorrupted = "XX001"
)

// Error is a lightsql error. The zero value is not useful; construct with New,
// Newf or Syntaxf.
type Error struct {
	// Code is the five-character SQLSTATE.
	Code string
	// Message is the primary human-readable description, lower-case and without
	// a trailing period, following PostgreSQL's convention.
	Message string
	// Detail optionally elaborates; Hint optionally suggests a fix.
	Detail, Hint string
	// Pos is the byte offset of the offending token in the statement text, or
	// token.NoPos when the error is not tied to a location.
	Pos token.Pos
	// Err is an optional wrapped cause.
	Err error
}

// New returns an *Error with the given code and message, at no position.
func New(code, msg string) *Error {
	return &Error{Code: code, Message: msg, Pos: token.NoPos}
}

// Newf is New with printf-style formatting.
func Newf(code, format string, args ...any) *Error {
	return &Error{Code: code, Message: fmt.Sprintf(format, args...), Pos: token.NoPos}
}

// Syntaxf returns a syntax error anchored at pos.
func Syntaxf(pos token.Pos, format string, args ...any) *Error {
	return &Error{Code: SyntaxError, Message: fmt.Sprintf(format, args...), Pos: pos}
}

// At returns a copy of e anchored at pos.
func (e *Error) At(pos token.Pos) *Error {
	c := *e
	c.Pos = pos
	return &c
}

// WithDetail returns a copy of e with the given detail text.
func (e *Error) WithDetail(format string, args ...any) *Error {
	c := *e
	c.Detail = fmt.Sprintf(format, args...)
	return &c
}

// WithHint returns a copy of e with the given hint text.
func (e *Error) WithHint(format string, args ...any) *Error {
	c := *e
	c.Hint = fmt.Sprintf(format, args...)
	return &c
}

// SQLState returns the SQLSTATE code. This method is the interface that pgx and
// lib/pq also satisfy.
func (e *Error) SQLState() string { return e.Code }

func (e *Error) Error() string {
	var b strings.Builder
	b.WriteString("ERROR: ")
	b.WriteString(e.Message)
	b.WriteString(" (SQLSTATE ")
	b.WriteString(e.Code)
	b.WriteByte(')')
	if e.Pos.IsValid() {
		// PostgreSQL reports a 1-based character position; match it so error
		// text is directly comparable in parity tests.
		fmt.Fprintf(&b, " at position %d", int(e.Pos)+1)
	}
	if e.Detail != "" {
		b.WriteString("\nDETAIL: ")
		b.WriteString(e.Detail)
	}
	if e.Hint != "" {
		b.WriteString("\nHINT: ")
		b.WriteString(e.Hint)
	}
	return b.String()
}

func (e *Error) Unwrap() error { return e.Err }

// Is reports equality by SQLSTATE, so errors.Is(err, pgerr.New(pgerr.UniqueViolation, ""))
// matches any unique violation regardless of message.
func (e *Error) Is(target error) bool {
	t, ok := target.(*Error)
	return ok && t.Code == e.Code
}
