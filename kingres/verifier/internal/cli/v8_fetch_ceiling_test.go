// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package cli

import (
	"fmt"
	"path/filepath"
	"testing"

	"github.com/KingresDB/Kingres/kingres/verifier/internal/testdir"
)

// V8-03: the ceiling on the number of files holds at the write itself, not only in the check made before the first request. The
// check that precedes the requests counts the tiles and entry bundles of the size; the checkpoint is a file too, so a ceiling equal
// to that count passes the check and is reached by the last write. Without the check in the write, one file more is written.
func TestV803TheFileCeilingHoldsAtTheWriteAsWell(t *testing.T) {
	w, a, _ := stdWorld(t)
	srv := serve(t, w)
	logArg := []string{"--trust", w.trust, "--log", "chain:" + a.idHex + ":e1"}
	full := 0
	for n := 1; n <= 60 && full == 0; n++ {
		out := filepath.Join(testdir.New(t), "mirror")
		code, text, errs := run(t, fetchArgs(w, srv, out, append(logArg, "--max-files", fmt.Sprint(n))...)...)
		if files, _ := dirBytes(t, out); files > n {
			t.Fatalf("%d file(s) were written under a ceiling of %d (status %d)\n%s%s", files, n, code, text, errs)
		}
		if code == 0 {
			full = n
		}
	}
	if full < 2 {
		t.Fatalf("the smallest ceiling that fetches the log is %d", full)
	}
}
