// SPDX-License-Identifier: AGPL-3.0-only

package cli

import (
	"encoding/base64"
	"strings"
	"testing"

	"github.com/KingresDB/Kingres/kingres/verifier/internal/checkpoint"
	"github.com/KingresDB/Kingres/kingres/verifier/internal/signednote"
)

// From a mutation run of state_record.go: the size limit of a state file at its
// exact value, the record never rewrites "<", ">" and "&", and a record that is
// not an object is refused for that.

func TestStateFileLimitIsExact(t *testing.T) {
	if maxStateFile != 2*(64<<10)+16<<10 {
		t.Fatalf("maxStateFile is %d", maxStateFile)
	}
	pad := func(n int) []byte { return []byte("{" + strings.Repeat(" ", n-1)) }
	if _, _, err := decodeHeld(pad(maxStateFile)); err == nil || strings.Contains(err.Error(), "larger than") {
		t.Errorf("a file of %d bytes is read: %v", maxStateFile, err)
	}
	if _, _, err := decodeHeld(pad(maxStateFile + 1)); err == nil || !strings.Contains(err.Error(), "larger than the limit") {
		t.Errorf("a file of %d bytes: %v", maxStateFile+1, err)
	}
	// a plain note has the limit of a note
	plain := func(n int) []byte { return []byte(strings.Repeat("x", n)) }
	if _, _, err := decodeHeld(plain(signednote.MaxNoteSize)); err != nil {
		t.Errorf("a note of %d bytes: %v", signednote.MaxNoteSize, err)
	}
	if _, _, err := decodeHeld(plain(signednote.MaxNoteSize + 1)); err == nil || !strings.Contains(err.Error(), "larger than the limit") {
		t.Errorf("a note of %d bytes: %v", signednote.MaxNoteSize+1, err)
	}
	// the record of the largest note stays within the limit
	if _, err := decodeRecord([]byte("[]")); err == nil || !strings.Contains(err.Error(), "expected a JSON object") {
		t.Errorf("an array: %v", err)
	}
	if _, err := decodeRecord([]byte("7")); err == nil || !strings.Contains(err.Error(), "expected a JSON object") {
		t.Errorf("a number: %v", err)
	}
}

func TestStateRecordDoesNotRewriteMarkup(t *testing.T) {
	k := newSigner(1)
	all := leaves(300)
	text := logOrigin() + "\n300\n" + rootOf(all, 300) + "\n<a href=\"x\">&amp;</a>\n"
	note := text + "\n" + sigLine(logOrigin(), text, k)
	n, err := signednote.Parse([]byte(note))
	if err != nil {
		t.Fatal(err)
	}
	cp, err := checkpoint.Parse(n.Text)
	if err != nil {
		t.Fatal(err)
	}
	data, err := encodeState(cp, []byte(note), nil, nowForTests)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `<a href=\"x\">&amp;</a>`) || strings.Contains(string(data), `\u003c`) || strings.Contains(string(data), `\u0026`) {
		t.Errorf("markup was rewritten:\n%s", data)
	}
}

// rootOf is the base64 root of the first size entries.
func rootOf(all []h32, size int) string {
	r := refMTH(all[:size])
	return base64.StdEncoding.EncodeToString(r[:])
}
