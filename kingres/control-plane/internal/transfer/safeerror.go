// SPDX-License-Identifier: AGPL-3.0-only

// Package transfer holds what the data transfer of the control plane (table CSV export and import, design
// kingres/docs/design/data-export-import.md) shares: the codes of a failed transfer, the one function that makes every error of a
// transfer safe to show (SafeError), the cluster setting, the record of a staged file and the check of the role between two steps.
package transfer

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"regexp"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/problem"
)

// The codes of a failed transfer task (TaskError.code of contract 0.19.0). None carries server text.
const (
	CodeExportTooLarge   = "export_too_large"
	CodeInsufficientDisk = "insufficient_disk"
	CodeQuotaExceeded    = "transfer_quota_exceeded"
	CodeTimeout          = "transfer_timeout"
	// CodeLeaderChanged: the leader that held the file and the role connection is gone; repeat the operation.
	CodeLeaderChanged = "transfer_leader_changed"
	// CodeSessionLost: the role is not connectable on this server any more (a restart empties the map of roles).
	CodeSessionLost = "transfer_session_lost"
	CodeKeyLost     = "transfer_key_lost"
	// CodeCopyInterrupted: a bucket moved, a group changed its primary or a connection broke before anything was committed.
	CodeCopyInterrupted = "copy_interrupted"
	// CodeRowCountMismatch: the server counted another number of rows than the control plane did, so the transaction was rolled back.
	CodeRowCountMismatch = "row_count_mismatch"
	// CodeOutcomeUnknown: the commit was sent and its outcome is not known; the rows may be in the table.
	CodeOutcomeUnknown = "import_outcome_unknown"
	CodeForbidden      = "forbidden"
	CodeCancelled      = "cancelled"
	CodeSQLError       = "sql_error"
	// CodeInternal is every failure the server does not describe to the client.
	CodeInternal = "internal_error"
)

// Error is the only error a transfer task records, answers or logs. It is built from fixed sentences and identifiers; it never holds a
// value of the data, a row, a key or the text of PostgreSQL (review finding DXR-03): the contract's task and problem are readable by
// kingres_readonly and the text of a COPY error carries the failing row.
type Error struct {
	// Code is a transfer code, or CodeSQLError with SQLState.
	Code string
	// Retryable says whether the same operation may succeed when repeated.
	Retryable bool
	// SQLState is the five characters of the state of PostgreSQL, for CodeSQLError.
	SQLState string
	// Sentence is a fixed text.
	Sentence string
	// Schema, Table, Column and Constraint are identifiers taken from the error's own fields.
	Schema, Table, Column, Constraint string
	// Line is the absolute line number of the record in the file (0: unknown).
	Line int
}

// Errorf makes an Error from a code and a sentence that the caller wrote; it must not contain data. It is the way the transfer code
// itself fails ("the upload is not complete"); an error that comes from outside goes through SafeError.
func Errorf(code string, retryable bool, format string, args ...any) *Error {
	return &Error{Code: code, Retryable: retryable, Sentence: fmt.Sprintf(format, args...)}
}

// TaskCode implements task.Coded.
func (e *Error) TaskCode() string { return e.Code }

// TaskRetryable implements task.Coded.
func (e *Error) TaskRetryable() bool { return e.Retryable }

// Error returns the sentence with the names and the line: no value.
func (e *Error) Error() string {
	var b strings.Builder
	b.WriteString(e.Sentence)
	if e.Constraint != "" {
		b.WriteString(" (constraint " + e.Constraint + ")")
	}
	switch {
	case e.Column != "" && e.Table != "":
		b.WriteString(" in column " + e.Column + " of " + qualified(e.Schema, e.Table))
	case e.Column != "":
		b.WriteString(" in column " + e.Column)
	case e.Table != "":
		b.WriteString(" in " + qualified(e.Schema, e.Table))
	}
	if e.Line > 0 {
		b.WriteString(", line " + strconv.Itoa(e.Line))
	} else if strings.HasPrefix(e.SQLState, "23") {
		// A constraint of a table of the router is checked by a shard group, which does not say which row it was (spike S2).
		b.WriteString(", the row that failed is not known")
	}
	if !strings.HasSuffix(e.Sentence, ".") {
		b.WriteString(".")
	}
	return b.String()
}

// ForTarget names the table of the task in the error: the router's errors from the shard groups name a bucket partition or nothing, so the
// table is the one the task works on, which the caller knows and the error does not. It returns e.
func (e *Error) ForTarget(schema, table string) *Error {
	e.Schema, e.Table = safeText(schema), safeText(table)
	return e
}

// bucketRE matches the name of a bucket partition of a distributed table (<table>_bNNNN) and of the indexes and constraints made from it.
var bucketRE = regexp.MustCompile("_b[0-9]{4}(_|$)")

func qualified(schema, table string) string {
	if schema == "" {
		return table
	}
	return schema + "." + table
}

// Problem is the HTTP problem of a transfer error that surfaces in a request (not in a task): sql_error with the state for a database
// error, and the code the transfer code stands for otherwise. The detail is the safe text.
func (e *Error) Problem() *problem.Problem {
	if e.Code == CodeSQLError {
		return problem.SQL(e.Error(), e.SQLState, 0)
	}
	code := problem.InternalError
	switch e.Code {
	case CodeForbidden:
		code = problem.Forbidden
	case CodeQuotaExceeded:
		code = problem.TransferQuotaExceeded
	case CodeInsufficientDisk:
		code = problem.InsufficientDisk
	case CodeCancelled, CodeTimeout, CodeLeaderChanged, CodeSessionLost, CodeCopyInterrupted, CodeRowCountMismatch, CodeOutcomeUnknown, CodeKeyLost, CodeExportTooLarge:
		code = problem.Conflict
	}
	return problem.New(code, e.Error())
}

// sentences is the fixed text per SQLSTATE of the design table "Error text".
var sentences = map[string]string{
	"22P02": "a value does not have the type of its column",
	"22001": "a value is too long for its column",
	"22003": "a number is out of range",
	"22007": "a date or time is not valid",
	"22008": "a date or time is not valid",
	"22021": "the data is not valid in the encoding",
	"22P04": "the CSV is malformed",
	"23502": "a required column is empty",
	"23503": "a foreign key is violated",
	"23505": "a unique constraint is violated",
	"23514": "a check constraint is violated",
	"42501": "the role does not have the privilege",
	"57014": "the statement was cancelled",
	"KS001": "a bucket moved; repeat",
}

// sentenceOf is the sentence of a state; every state without one is named by its five characters.
func sentenceOf(state string) string {
	if s, ok := sentences[state]; ok {
		return s
	}
	return "the database refused the statement (SQLSTATE " + state + ")"
}

var stateRE = regexp.MustCompile(`^[0-9A-Z]{5}$`)

// lineRE matches the context of a COPY error, "COPY name, line N", at its start. The relation name of the error's own field is checked
// against the text, so a name that looks like a context cannot move the number.
var lineRE = regexp.MustCompile(`^COPY ([^\n]*?), line ([0-9]{1,18})(?:,|:|$)`)

// lineOf extracts the absolute line number of a COPY context. It accepts only the form "line <digits>" after the relation name that the
// error itself carries; the text after it (the column and the value) is never read.
func lineOf(where, table string) int {
	m := lineRE.FindStringSubmatchIndex(where)
	if m == nil {
		return 0
	}
	rel := where[m[2]:m[3]]
	// The router's remote context names the relation as the user wrote it; the local one as PostgreSQL does. Either is accepted when it
	// ends with the table of the error, or when the error names no table.
	if table != "" && rel != table && !strings.HasSuffix(rel, "."+table) && rel != `"`+table+`"` {
		return 0
	}
	n, err := strconv.Atoi(where[m[4]:m[5]])
	if err != nil || n < 1 {
		return 0
	}
	return n
}

// safeText makes an identifier fit for a message: at most 63 runes, valid UTF-8, no control or format character, no line break. It is not
// a defence against a hostile identifier (a name is the table owner's choice); it keeps a name from carrying text of its own shape.
func safeText(s string) string {
	if s == "" {
		return ""
	}
	var b strings.Builder
	n := 0
	for _, r := range s {
		if n >= 63 {
			break
		}
		switch {
		case r == utf8.RuneError, unicode.IsControl(r), unicode.Is(unicode.Cf, r), unicode.Is(unicode.Zl, r), unicode.Is(unicode.Zp, r):
			b.WriteRune('?')
		default:
			b.WriteRune(r)
		}
		n++
	}
	return b.String()
}

// SafeError turns any error of a transfer into an Error: the code, the SQLSTATE, a fixed sentence per state, the names of the schema, table,
// column and constraint from the error's own fields, and the absolute line number. The message, the detail, the hint, the context text and
// the failing row are never copied, for any class and for class XX. It is the only way an error leaves the transfer code; the task, the
// problem, the audit event and the log use its result.
func SafeError(err error) *Error {
	if err == nil {
		return nil
	}
	var te *Error
	if errors.As(err, &te) {
		return te
	}
	var pe *pgconn.PgError
	if errors.As(err, &pe) {
		return fromPg(pe)
	}
	switch {
	case errors.Is(err, context.Canceled):
		return &Error{Code: CodeCancelled, Sentence: "the operation was cancelled"}
	case errors.Is(err, context.DeadlineExceeded):
		return &Error{Code: CodeTimeout, Sentence: "the transfer ran out of time"}
	}
	var pp *problem.Problem
	if errors.As(err, &pp) {
		switch pp.Status {
		case 401, 403:
			return &Error{Code: CodeForbidden, Sentence: "the role is not allowed to do this"}
		case 503:
			return &Error{Code: CodeSessionLost, Retryable: true, Sentence: "the database cannot be reached right now"}
		}
		return &Error{Code: CodeInternal, Sentence: "the transfer failed (" + pp.Title + ")"}
	}
	if brokenConnection(err) {
		return &Error{Code: CodeCopyInterrupted, Retryable: true, Sentence: "the connection to the database broke before the statement ended"}
	}
	var ne net.Error
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) || errors.As(err, &ne) || pgconn.SafeToRetry(err) || pgconn.Timeout(err) {
		return &Error{Code: CodeCopyInterrupted, Retryable: true, Sentence: "the connection to the database broke before the statement ended"}
	}
	return &Error{Code: CodeInternal, Sentence: "the transfer failed for a reason the server does not report"}
}

func fromPg(pe *pgconn.PgError) *Error {
	state := pe.Code
	if !stateRE.MatchString(state) {
		state = "XX000"
	}
	e := &Error{
		Code:       CodeSQLError,
		SQLState:   state,
		Sentence:   sentenceOf(state),
		Schema:     safeText(pe.SchemaName),
		Table:      safeText(pe.TableName),
		Column:     safeText(pe.ColumnName),
		Constraint: safeText(pe.ConstraintName),
		Line:       lineOf(pe.Where, pe.TableName),
	}
	// A name that is a bucket partition of a distributed table belongs to the implementation of the router, not to the user's table.
	if bucketRE.MatchString(pe.TableName) {
		e.Schema, e.Table = "", ""
	}
	if bucketRE.MatchString(pe.ConstraintName) {
		e.Constraint = ""
	}
	switch {
	case state == "KS001", strings.HasPrefix(state, "08"), state == "57P01", state == "57P02", state == "57P03", state == "40001", state == "40P01":
		// A bucket that moved, a connection that failed, a server that shut down and a serialization failure leave nothing stored and
		// may succeed when repeated.
		e.Code, e.Retryable = CodeCopyInterrupted, true
		if state != "KS001" {
			e.Sentence = "the connection to the database broke before the statement ended"
		}
		e.SQLState = ""
		e.Schema, e.Table, e.Column, e.Constraint, e.Line = "", "", "", "", 0
		if state == "KS001" {
			e.SQLState = state
		}
	}
	return e
}

// brokenConnection recognises the failures of an established connection that the driver reports as plain errors. The text is only compared, never kept.
func brokenConnection(err error) bool {
	msg := strings.ToLower(err.Error())
	for _, s := range []string{"conn closed", "conn busy", "unexpected eof", "connection reset", "broken pipe", "use of closed network connection", "connection refused", "forcibly closed"} {
		if strings.Contains(msg, s) {
			return true
		}
	}
	return false
}
