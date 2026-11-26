// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package statefile

import (
	"bytes"
	"errors"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/KingresDB/Kingres/kingres/verifier/internal/safefile"

	"github.com/KingresDB/Kingres/kingres/verifier/internal/testdir"
)

// L14-04. The measurement of the window in which a writer that ignores the lock
// can be overwritten, as the review made it: a second writer (temporary file and
// rename, no flush) starts at a random time between 0 and twice the duration of
// the call; a run is lost when the call returned nil, the rival's rename
// succeeded, and the rival's bytes are not in the file at the end (the call renamed
// over them after it had read the state).
//
// It runs only when KINGRES_WINDOW_ATTEMPTS is set to the number of attempts for
// each of the two versions (a few thousand take about a minute), and logs the
// figures that the README quotes.

// replaceReadingFirst is the version before L14-04: the state is read, and then
// the temporary file is created, written, flushed and renamed.
func replaceReadingFirst(path string, data, was []byte, existed bool, limit int64) error {
	now, err := safefile.ReadArg(path, limit)
	switch {
	case errors.Is(err, os.ErrNotExist):
		if existed {
			return ErrStateChanged
		}
	case err != nil:
		return ErrStateChanged
	case !existed:
		return ErrStateChanged
	case !bytes.Equal(now, was):
		return ErrStateChanged
	}
	return Replace(path, data)
}

type windowResult struct {
	attempts, lost, refused, rivalLast, other int
	meanCall                                  time.Duration
}

func measureWindow(t *testing.T, attempts int, call func(path string, data, was []byte) error) windowResult {
	dir := testdir.New(t)
	p := filepath.Join(dir, "state")
	was := []byte("the state that was read\n")
	data := bytes.Repeat([]byte("n"), 700)
	rival := []byte("the rival's bytes\n")
	reset := func() {
		if err := os.WriteFile(p, was, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// the duration of one call, with nothing else running
	var total time.Duration
	const calib = 10
	for i := 0; i < calib; i++ {
		reset()
		start := time.Now()
		if err := call(p, data, was); err != nil {
			t.Fatal(err)
		}
		total += time.Since(start)
	}
	dur := total / calib
	rng := rand.New(rand.NewSource(14))
	res := windowResult{attempts: attempts, meanCall: dur}
	for i := 0; i < attempts; i++ {
		reset()
		delay := time.Duration(rng.Int63n(int64(2 * dur)))
		type rv struct{ err error }
		done := make(chan rv, 1)
		go func() {
			time.Sleep(delay)
			tmp := filepath.Join(dir, "rival.tmp")
			if err := os.WriteFile(tmp, rival, 0o600); err != nil {
				done <- rv{err}
				return
			}
			done <- rv{os.Rename(tmp, p)}
		}()
		err := call(p, data, was)
		r := <-done
		final, _ := os.ReadFile(p)
		switch {
		case r.err != nil:
			res.other++
		case errors.Is(err, ErrStateChanged):
			res.refused++
		case err != nil:
			res.other++
		case bytes.Equal(final, data):
			res.lost++
		case bytes.Equal(final, rival):
			res.rivalLast++
		default:
			res.other++
		}
	}
	return res
}

func TestMeasureTheWindowOfAWriterThatIgnoresTheLock(t *testing.T) {
	n, _ := strconv.Atoi(os.Getenv("KINGRES_WINDOW_ATTEMPTS"))
	if n <= 0 {
		t.Skip("set KINGRES_WINDOW_ATTEMPTS to measure")
	}
	for _, v := range []struct {
		name string
		call func(path string, data, was []byte) error
	}{
		{"before (read, then create, write, flush, rename)", func(p string, d, w []byte) error { return replaceReadingFirst(p, d, w, true, 1<<20) }},
		{"after (create, write, flush, then read, rename)", func(p string, d, w []byte) error { return ReplaceIfUnchanged(p, d, w, true, 1<<20) }},
	} {
		r := measureWindow(t, n, v.call)
		fmt.Printf("window %-50s call %v: %d attempts, %d lost (%.2f%%), %d refused, %d rival last, %d other\n",
			v.name, r.meanCall.Round(100*time.Microsecond), r.attempts, r.lost, 100*float64(r.lost)/float64(r.attempts), r.refused, r.rivalLast, r.other)
	}
}
