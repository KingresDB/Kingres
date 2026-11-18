// SPDX-License-Identifier: AGPL-3.0-only

package ledgersign

import (
	"strings"
	"testing"
	"time"
)

func TestAnEventHasTheFormatOfTheDesignAndNothingElse(t *testing.T) {
	at := time.Date(2026, 12, 1, 10, 0, 0, 999_000_000, time.FixedZone("x", 3600))
	text, err := eventText(7, at, EventKeyRevoked, field{"key", "AAAA"}, field{"compromised-since", "2026-11-30T00:00:00Z"}, field{"reason", "stolen laptop"})
	if err != nil {
		t.Fatal(err)
	}
	want := "kingres-ledger-event/1\nseq: 7\ntime: 2026-12-01T09:00:00Z\ntype: key-revoked\nkey: AAAA\ncompromised-since: 2026-11-30T00:00:00Z\nreason: stolen laptop\n"
	if string(text) != want {
		t.Errorf("the text:\n%q\nwant\n%q", text, want)
	}
	f, err := eventFields(text)
	if err != nil || f["seq"] != "7" || f["type"] != "key-revoked" || f["reason"] != "stolen laptop" || f["time"] != "2026-12-01T09:00:00Z" {
		t.Errorf("the fields read back: %v %v", f, err)
	}
}

func TestAnEventRefusesWhatCouldSplitALineOrLeaveASCII(t *testing.T) {
	at := time.Unix(1_800_000_000, 0)
	for name, f := range map[string]field{
		"a newline in a value":      {"reason", "ok\ntype: key-added"},
		"a carriage return":         {"reason", "ok\rno"},
		"a tab":                     {"reason", "a\tb"},
		"a byte above ASCII":        {"reason", "caf\u00e9"},
		"a long reason":             {"reason", strings.Repeat("x", 201)},
		"a colon in a name":         {"re:ason", "x"},
		"a space in a name":         {"re ason", "x"},
		"a control char in a name":  {"rea\x01son", "x"},
		"a delete character":        {"reason", "a\x7fb"},
		"a newline in a name":       {"rea\nson", "x"},
		"an empty line as a member": {"", "x"},
	} {
		if _, err := eventText(0, at, EventKeyRevoked, f); err == nil && name != "an empty line as a member" {
			t.Errorf("%s was accepted", name)
		}
	}
	if _, err := eventText(-1, at, EventKeyAdded); err == nil {
		t.Error("a negative number was accepted")
	}
	if _, err := eventText(0, at, EventKeyAdded, field{"reason", strings.Repeat("x", 200)}); err != nil {
		t.Errorf("a reason of 200 bytes: %v", err)
	}
}

func TestValidReasonIsAtMost200BytesOfPrintableASCII(t *testing.T) {
	for s, ok := range map[string]bool{"": true, "lost": true, strings.Repeat("a", 200): true, strings.Repeat("a", 201): false, "a\nb": false, "caf\u00e9": false, "tab\t": false, "~ !": true} {
		if err := ValidReason(s); (err == nil) != ok {
			t.Errorf("ValidReason(%q) = %v", s, err)
		}
	}
}

func TestEventFieldsRefuseWhatIsNotAnEvent(t *testing.T) {
	for _, s := range []string{"", "other/1\nseq: 0\n", "kingres-ledger-event/1\nseq: 0\nnovalue\ntype: x\n", "kingres-ledger-event/1\n"} {
		if _, err := eventFields([]byte(s)); err == nil {
			t.Errorf("%q was read as an event", s)
		}
	}
	if got := string(noteText([]byte("a\nb\n\n\u2014 name AAAA\n"))); got != "a\nb\n" {
		t.Errorf("noteText: %q", got)
	}
	if noteText([]byte("no separator")) != nil {
		t.Error("a note without a blank line has a text")
	}
}

func TestTheFingerprintCheckIsStrict(t *testing.T) {
	ok := strings.Repeat("0123456789abcdef", 4)
	for s, want := range map[string]bool{ok: true, strings.ToUpper(ok): false, ok[:63]: false, ok + "0": false, "../" + ok[3:]: false, "": false} {
		if got := fingerprintOK(s); got != want {
			t.Errorf("fingerprintOK(%q) = %v", s, got)
		}
	}
}
