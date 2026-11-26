// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package chain

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

// StreamBlocks must say what ParseBlocks, VerifyBlocks and CheckDigestOnBlocks
// say together, for the fixture and for every damage the others reject.
func TestStreamBlocksAgreesWithTheWholeFileChecks(t *testing.T) {
	good := string(fixture(t, "blocks.jsonl"))
	d, err := ParseDigest(fixture(t, "digests/d5.json"))
	if err != nil {
		t.Fatal(err)
	}
	if s, err := StreamBlocks(strings.NewReader(good), d.ChainID, &d); err != nil || s.Blocks < 2 {
		t.Fatalf("fixture: %d %v", s.Blocks, err)
	}
	lines := strings.Split(strings.TrimSuffix(good, "\n"), "\n")
	other := d.ChainID
	other[0] ^= 1
	for _, tt := range []struct {
		name, in string
		cid      [32]byte
		step     string
	}{
		{"empty", "", d.ChainID, "parse"},
		{"blank line", lines[0] + "\n\n" + lines[1] + "\n", d.ChainID, "parse"},
		{"not json", "x\n", d.ChainID, "parse"},
		{"another chain", good, other, "chain"},
		{"a block missing", strings.Join(append(append([]string{}, lines[:2]...), lines[3:]...), "\n") + "\n", d.ChainID, "chain"},
		{"ends before the digest", strings.Join(lines[:d.Height], "\n") + "\n", d.ChainID, "digest"},
		{"line too long", lines[0] + strings.Repeat(" ", maxBlockLine) + "\n", d.ChainID, "parse"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, err := StreamBlocks(strings.NewReader(tt.in), tt.cid, &d)
			se, ok := err.(*StreamError)
			if !ok || se.Step != tt.step {
				t.Fatalf("got %v, want step %s", err, tt.step)
			}
		})
	}
	if _, err := StreamBlocks(bytes.NewReader(bytes.ReplaceAll([]byte(good), []byte("\n"), []byte("\r\n"))), d.ChainID, nil); err != nil {
		t.Errorf("CRLF: %v", err)
	}
}

// errAfter is a reader that gives its data and then fails: a file system that
// loses the file, a limit that is reached.
type errAfter struct {
	data []byte
	err  error
}

func (r *errAfter) Read(p []byte) (int, error) {
	if len(r.data) == 0 {
		return 0, r.err
	}
	n := copy(p, r.data)
	r.data = r.data[n:]
	return n, nil
}

// L9-11 (B2): an export that ends because the reader failed is not an export
// that ended. A valid prefix followed by a read error is a failure, with no
// digest as with one, and not "verified up to the cut".
func TestStreamBlocksReadErrorAfterAValidPrefix(t *testing.T) {
	good := string(fixture(t, "blocks.jsonl"))
	lines := strings.SplitAfter(strings.TrimSuffix(good, "\n"), "\n")
	d, err := ParseDigest(fixture(t, "digests/d0.json"))
	if err != nil {
		t.Fatal(err)
	}
	boom := errors.New("the disk went away")
	prefix := strings.Join(lines[:4], "")
	for _, withDigest := range []bool{false, true} {
		var dp *Digest
		if withDigest {
			dp = &d
		}
		s, err := StreamBlocks(&errAfter{data: []byte(prefix), err: boom}, d.ChainID, dp)
		var se *StreamError
		if !errors.As(err, &se) || se.Step != "parse" || !errors.Is(err, boom) {
			t.Errorf("digest=%v: a read error after four blocks gave %+v, %v", withDigest, s, err)
		}
		if s.Blocks != 4 {
			t.Errorf("digest=%v: %d blocks before the error, want 4", withDigest, s.Blocks)
		}
	}
	// the same with a reader that ends cleanly is a prefix, which is valid
	s, err := StreamBlocks(strings.NewReader(prefix), d.ChainID, nil)
	if err != nil || s.Blocks != 4 || s.Head.Height != 3 {
		t.Errorf("a clean end: %+v %v", s, err)
	}
	// a line longer than the limit after the prefix is a failure, not a cut
	if _, err := StreamBlocks(strings.NewReader(prefix+strings.Repeat("x", maxBlockLine+10)), d.ChainID, nil); err == nil {
		t.Error("a line over the limit after a prefix was accepted")
	}
}
