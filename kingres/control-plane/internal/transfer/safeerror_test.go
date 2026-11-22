// SPDX-License-Identifier: AGPL-3.0-only

package transfer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/problem"
)

const canary = "CANARY-9f3a-secret@example.test"

// canaryErr is a PgError whose every free-text field carries the canary, the way a COPY error carries the failing row.
func canaryErr(state string) *pgconn.PgError {
	return &pgconn.PgError{
		Severity: "ERROR " + canary, Code: state,
		Message:          `invalid input syntax for type integer: "` + canary + `"`,
		Detail:           "Key (email)=(" + canary + ") already exists.",
		Hint:             canary,
		Position:         7,
		InternalPosition: 3,
		InternalQuery:    canary,
		Where:            `COPY orders, line 42, column id: "` + canary + `"`,
		SchemaName:       "public", TableName: "orders", ColumnName: "id", DataTypeName: canary, ConstraintName: "orders_id_key",
		File: canary, Line: 9, Routine: canary,
	}
}

// Every SQLSTATE of the design table gets its fixed sentence, the names and the absolute line, and never a value (DXR-03).
func TestSafeErrorTableOfStates(t *testing.T) {
	want := map[string]string{
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
		"XX000": "the database refused the statement (SQLSTATE XX000)",
		"XX001": "the database refused the statement (SQLSTATE XX001)",
		"42P01": "the database refused the statement (SQLSTATE 42P01)",
	}
	for state, sentence := range want {
		e := SafeError(canaryErr(state))
		if e.Code != CodeSQLError || e.SQLState != state || e.Sentence != sentence {
			t.Errorf("%s: %+v", state, e)
		}
		msg := e.Error()
		if strings.Contains(msg, canary) || strings.Contains(msg, "CANARY") || strings.Contains(msg, "invalid input") || strings.Contains(msg, "already exists") {
			t.Errorf("%s: the text carries server text: %q", state, msg)
		}
		if !strings.Contains(msg, "line 42") || !strings.Contains(msg, "orders_id_key") || !strings.Contains(msg, "column id") || !strings.Contains(msg, "public.orders") {
			t.Errorf("%s: names or line are missing: %q", state, msg)
		}
		blob, _ := json.Marshal(e)
		p, _ := json.Marshal(e.Problem())
		if strings.Contains(string(blob), "CANARY") || strings.Contains(string(p), "CANARY") {
			t.Errorf("%s: a serialisation carries the canary: %s %s", state, blob, p)
		}
	}
}

// The state of a bucket that moved and of a broken connection are retryable and carry no name.
func TestSafeErrorRetryable(t *testing.T) {
	for _, state := range []string{"KS001", "08006", "08003", "57P01", "40001", "40P01"} {
		e := SafeError(canaryErr(state))
		if e.Code != CodeCopyInterrupted || !e.Retryable || e.Table != "" || e.Line != 0 {
			t.Errorf("%s: %+v", state, e)
		}
		if strings.Contains(e.Error(), "CANARY") {
			t.Errorf("%s: %q", state, e.Error())
		}
	}
	if e := SafeError(canaryErr("KS001")); e.Sentence != "a bucket moved; repeat" {
		t.Errorf("KS001 sentence: %q", e.Sentence)
	}
	if e := SafeError(canaryErr("23505")); e.Retryable {
		t.Error("a unique violation is not retryable")
	}
}

// Errors that are not from PostgreSQL: the context, the network, a problem of this server, anything else.
func TestSafeErrorOtherErrors(t *testing.T) {
	cases := []struct {
		err   error
		code  string
		retry bool
	}{
		{context.Canceled, CodeCancelled, false},
		{fmt.Errorf("wrapped %s: %w", canary, context.Canceled), CodeCancelled, false},
		{context.DeadlineExceeded, CodeTimeout, false},
		{io.EOF, CodeCopyInterrupted, true},
		{fmt.Errorf("read tcp %s: %w", canary, io.ErrUnexpectedEOF), CodeCopyInterrupted, true},
		{problem.New(problem.Forbidden, canary), CodeForbidden, false},
		{problem.New(problem.ServiceUnavailable, canary), CodeSessionLost, true},
		{problem.New(problem.NotFound, canary), CodeInternal, false},
		{errors.New("password authentication failed for user " + canary), CodeInternal, false},
		{Errorf(CodeOutcomeUnknown, false, "N rows may have been loaded"), CodeOutcomeUnknown, false},
	}
	for _, tc := range cases {
		e := SafeError(tc.err)
		if e.Code != tc.code || e.Retryable != tc.retry {
			t.Errorf("%v: %+v", tc.err, e)
		}
		if strings.Contains(e.Error(), canary) {
			t.Errorf("%v: the text carries the error text: %q", tc.err, e.Error())
		}
	}
	if SafeError(nil) != nil {
		t.Error("nil stays nil")
	}
	own := Errorf(CodeRowCountMismatch, false, "the server counted fewer rows")
	if SafeError(fmt.Errorf("step: %w", own)) != own {
		t.Error("an Error is returned as it is, also when wrapped")
	}
}

// The line number is taken from the context only in the form "COPY <the table of the error>, line <digits>" at its start: a value or a
// relation name that looks like a context cannot move it.
func TestLineOf(t *testing.T) {
	cases := []struct {
		where, table string
		want         int
	}{
		{`COPY orders, line 5`, "orders", 5},
		{`COPY orders, line 5, column id: "x"`, "orders", 5},
		{`COPY orders, line 5: "x"`, "orders", 5},
		{`COPY orders, line 12, column id: "line 99"`, "orders", 12},
		{`COPY orders, line 12, column id: ", line 99, "`, "orders", 12},
		{`COPY orders, line 5`, "", 5},
		{`COPY public.orders, line 7`, "orders", 7},
		{`COPY "orders", line 7`, "orders", 7},
		{`COPY orders, line 5`, "other", 0},
		{`line 5`, "orders", 0},
		{`COPY orders, line 0`, "orders", 0},
		{`COPY orders, line x`, "orders", 0},
		{`COPY orders, line -3`, "orders", 0},
		{`COPY orders, line 99999999999999999999`, "orders", 0},
		{`SQL statement "COPY orders, line 5"`, "orders", 0},
		{"context\nCOPY orders, line 5", "orders", 0},
		{``, "orders", 0},
	}
	for _, tc := range cases {
		if got := lineOf(tc.where, tc.table); got != tc.want {
			t.Errorf("lineOf(%q, %q) = %d, want %d", tc.where, tc.table, got, tc.want)
		}
	}
}

// An identifier is cut and cleaned; it cannot carry a line break or a control character into a message.
func TestSafeText(t *testing.T) {
	if got := safeText("a\nb\x00c\u202ed"); strings.ContainsAny(got, "\n\x00\u202e") {
		t.Errorf("safeText kept a control character: %q", got)
	}
	if got := safeText(strings.Repeat("x", 200)); len(got) != 63 {
		t.Errorf("length %d", len(got))
	}
	if got := safeText("\xff\xfe"); !utf8.ValidString(got) {
		t.Errorf("invalid UTF-8 left: %q", got)
	}
	if safeText("") != "" || safeText("na\u00efve") != "na\u00efve" {
		t.Error("plain names are kept")
	}
	e := SafeError(&pgconn.PgError{Code: "23505", ConstraintName: "k\nINJECTED line", TableName: "t"})
	if strings.Contains(e.Error(), "\n") {
		t.Errorf("a line break reached the message: %q", e.Error())
	}
}

// A state that is not five characters of the state alphabet never reaches the text as it is.
func TestSafeErrorOddState(t *testing.T) {
	for _, st := range []string{"", "abc", "TOOLONGSTATE", "22P0\n", canary} {
		e := SafeError(&pgconn.PgError{Code: st, Message: canary})
		if e.SQLState != "XX000" || strings.Contains(e.Error(), "CANARY") {
			t.Errorf("%q: %+v %q", st, e, e.Error())
		}
	}
}

// The task engine records the error text of a step: the Error is its own Coded and its text is the safe one.
func TestErrorIsCoded(t *testing.T) {
	var err error = SafeError(canaryErr("23505"))
	c, ok := err.(interface {
		TaskCode() string
		TaskRetryable() bool
	})
	if !ok || c.TaskCode() != CodeSQLError || c.TaskRetryable() {
		t.Fatalf("not coded: %v", err)
	}
}

// FuzzSafeError feeds SafeError with arbitrary fields of a PostgreSQL error. Whatever the content of the free-text fields, they are not in
// the output (identifier fields may be, cut and cleaned), the state is five characters, and the output is bounded.
func FuzzSafeError(f *testing.F) {
	f.Add("22P02", "invalid input syntax", "Key (a)=(b)", "hint", "COPY t, line 3, column c: \"v\"", "t", "c", "k", "s")
	f.Add("23505", canary, canary, canary, "COPY t, line 1", "t", "", "t_pkey", "public")
	f.Add("XX000", "\x00\xff", "\n", "", "COPY , line 1", "", "", "", "")
	f.Fuzz(func(t *testing.T, state, msg, detail, hint, where, table, col, cons, schema string) {
		pe := &pgconn.PgError{Code: state, Message: msg, Detail: detail, Hint: hint, Where: where, TableName: table, ColumnName: col, ConstraintName: cons, SchemaName: schema,
			InternalQuery: msg, File: detail, Routine: hint, DataTypeName: where, Severity: msg}
		e := SafeError(pe)
		out := e.Error()
		if !stateRE.MatchString(e.SQLState) && e.SQLState != "" {
			t.Fatalf("state %q", e.SQLState)
		}
		if len(out) > 1024 {
			t.Fatalf("output of %d bytes", len(out))
		}
		if !utf8.ValidString(out) || strings.ContainsAny(out, "\n\r\x00") {
			t.Fatalf("output is not clean: %q", out)
		}
		// A run of the free text that is neither a fixed text nor in an identifier field must not be in the output.
		for _, free := range []string{msg, detail, hint, where} {
			if len(free) < 12 {
				continue
			}
			if strings.Contains(out, free) && free != table && free != col && free != cons && free != schema {
				t.Fatalf("free text %q is in the output %q", free, out)
			}
		}
	})
}
