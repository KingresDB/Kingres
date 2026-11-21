// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026 RooyeKhat Media Co

package ops

import (
	"errors"
	"testing"
)

// The list of schemas returned "the local PostgreSQL instance is not available" for every database: it ended with
// "return out, PGProblem(rows.Err())", and the mapping of no error was an error.
func TestPGProblemOfNoErrorIsNoError(t *testing.T) {
	if err := PGProblem(nil); err != nil {
		t.Fatalf("PGProblem(nil) = %v", err)
	}
	if err := PGProblem(errors.New("connection reset")); err == nil {
		t.Fatal("a real error must stay an error")
	}
}
