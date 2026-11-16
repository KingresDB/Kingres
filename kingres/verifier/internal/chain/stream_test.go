// SPDX-License-Identifier: AGPL-3.0-only

package chain

import (
	"bytes"
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
	if n, err := StreamBlocks(strings.NewReader(good), d.ChainID, &d); err != nil || n < 2 {
		t.Fatalf("fixture: %d %v", n, err)
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
