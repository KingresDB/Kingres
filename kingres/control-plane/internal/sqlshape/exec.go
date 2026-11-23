// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package sqlshape

import (
	"bytes"
	"errors"
	"fmt"
	"io"
)

// This file is the execution mode of the grammar (phase 14c). Each hands every accepted statement to the importer together with the exact text that the
// grammar consumed. Three rules keep the text and the verdict together:
//
//   - the text is what the lexer read for the statement, from its first token to the terminator (comments inside it included, the terminator not): the bytes
//     that the parser saw are the bytes that are returned, in the same pass;
//   - a statement is returned only after parseRaw accepted ALL of it (a kind that is not in the grammar, a clause it does not know and a trailing token are
//     refusals), and a refusal stops the walk: nothing after a refused statement is returned;
//   - the text is lexed again, alone, and must give the same tokens, the same length and the same hash. A text that the lexer reads differently when it
//     stands alone (a defect that the corpus of lexcorpus.json is meant to catch) is a refusal.
//
// The caller runs the text as ONE statement of the extended protocol, so a server that read the text differently would refuse it as several statements.

// RefusedError is the error of Each for a statement that the grammar refused. It carries the refusal (kind, position, fingerprint, a fixed reason) and never the text.
type RefusedError struct {
	Refusal Refusal
}

func (e *RefusedError) Error() string {
	return "sqlshape: statement " + e.Refusal.Position + " (" + e.Refusal.Kind + ") is refused: " + e.Refusal.Reason
}

// mismatchReason is the reason of a RefusedError for a statement whose text does not lex again to the tokens that were parsed.
const mismatchReason = "the text of the statement does not read the same when it stands alone"

// Each reads one SQL file and calls fn for every accepted statement, in the order of the file, with the statement and its text. A line that starts with a
// backslash is a \restrict or \unrestrict line (the others are refused) and is skipped: fn is not called for it. The first refused statement ends the walk with a
// *RefusedError; so does a statement that fn itself rejects (fn's error is returned as it is). A file that ends inside a statement, a string or a comment, a read
// error and more than MaxStatements statements are the errors of ParseFile.
func Each(r io.Reader, file string, hints Hints, fn func(st Statement, text string) error) error {
	l := newLexer(r)
	for n := 1; ; n++ {
		raw, err := l.next()
		if err != nil {
			return err
		}
		if raw == nil {
			return nil
		}
		if n > MaxStatements {
			return ErrTooManyStatement
		}
		pos := fmt.Sprintf("%s#%d", file, n)
		st, ref := parseRaw(raw, pos, hints)
		if ref != nil {
			return &RefusedError{Refusal: *ref}
		}
		if raw.isMeta {
			continue
		}
		text := string(append([]byte(nil), l.raw...))
		if err := sameTokens(text, raw); err != nil {
			return &RefusedError{Refusal: Refusal{Kind: st.Kind, Position: pos, Fingerprint: fingerprint(raw.length, raw.hash), Reason: mismatchReason}}
		}
		if err := fn(*st, text); err != nil {
			return err
		}
	}
}

// sameTokens lexes the text of one statement alone and compares the result with what the first pass found.
func sameTokens(text string, first *rawStmt) error {
	if len(text) != first.length || bytes.IndexByte([]byte(text), 0) >= 0 {
		return errors.New("length")
	}
	l := newLexer(bytes.NewReader([]byte(text + ";")))
	again, err := l.next()
	if err != nil || again == nil || again.isMeta || again.length != first.length || again.hash != first.hash || len(again.toks) != len(first.toks) {
		return errors.New("shape")
	}
	for i := range again.toks {
		if again.toks[i] != first.toks[i] {
			return errors.New("token")
		}
	}
	// Nothing may follow: the terminator ended the statement and the input is at its end.
	if rest, err := l.next(); err != nil || rest != nil {
		return errors.New("rest")
	}
	return nil
}
