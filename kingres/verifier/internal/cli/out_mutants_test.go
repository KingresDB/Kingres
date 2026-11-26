// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package cli

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/KingresDB/Kingres/kingres/verifier/internal/testdir"
)

// From a mutation run of cli_out.go and cli.go: the JSON says when a string was
// not UTF-8 wherever the string is, input errors are JSON too, and the lines of a
// proof (entry, "not proved") are there.

func printed(v jsonOutput) (string, map[string]any) {
	var out bytes.Buffer
	e := &env{stdout: &out}
	e.printJSON(v)
	var m map[string]any
	_ = json.Unmarshal(out.Bytes(), &m)
	return out.String(), m
}

func TestInvalidUTF8IsReportedWhereverTheStringIs(t *testing.T) {
	bad := "x\xffy"
	pb := &bad
	for name, v := range map[string]jsonOutput{
		"error":         {Error: bad},
		"step name":     {Steps: []stepJSON{{Name: bad, OK: true}}},
		"step detail":   {Steps: []stepJSON{{Name: "ok", OK: true, Detail: bad}}},
		"second step":   {Steps: []stepJSON{{Name: "a", Detail: "b"}, {Name: "c", Detail: bad}}},
		"note":          {Notes: []string{"fine", bad}},
		"map value":     {Info: map[string]any{"k": bad}},
		"map key":       {Info: map[string]any{bad: 1}},
		"nested":        {Info: map[string]any{"a": []any{map[string]any{"b": bad}}}},
		"pointer":       {Info: map[string]any{"p": pb}},
		"struct fields": {Info: struct{ A, B string }{"ok", bad}},
	} {
		text, m := printed(v)
		if m["invalid_utf8"] != true {
			t.Errorf("%s: invalid_utf8 is not set:\n%s", name, text)
		}
	}
	// a valid string, a nil pointer and a real U+FFFD do not set it
	var np *string
	for name, v := range map[string]jsonOutput{
		"plain":       {Error: "fine", Info: map[string]any{"p": np}},
		"real fffd":   {Error: "a\ufffdb"},
		"empty value": {},
	} {
		if _, m := printed(v); m["invalid_utf8"] == true {
			t.Errorf("%s: invalid_utf8 is set", name)
		}
	}
}

func TestInputErrorsAreJSONToo(t *testing.T) {
	code, out, errs := run(t, "proof", "--proof", fx("proofs/acc5.json"), "--digest", fx("digests/missing.json"), "--json")
	var m map[string]any
	if code != 2 || json.Unmarshal([]byte(out), &m) != nil || m["input_error"] != true || m["error"] == "" || m["verified"] != false || errs == "" {
		t.Fatalf("an input error with --json: %d\n%s%s", code, out, errs)
	}
	if code, out, _ := run(t, "proof", "--proof", fx("proofs/acc5.json"), "--digest", fx("digests/missing.json")); code != 2 || out != "" {
		t.Errorf("without --json nothing is printed on the standard output: %d %q", code, out)
	}
	// an input error after steps ran says the steps too (finishInputWith)
	p := newPublication(t, 300, 700)
	code, out, _ = run(t, "trust", "show", "--trust", trustWith(t, p, `,"revoked":true`), "--json")
	m = nil
	if code != 2 || json.Unmarshal([]byte(out), &m) != nil || m["input_error"] != true {
		t.Errorf("trust show with no usable key, --json: %d\n%s", code, out)
	}
}

func TestIncompleteSaysItIsAnAlert(t *testing.T) {
	p := newPublication(t, 300, 700)
	_ = p
	code, out, _ := run(t, "checkpoint", "--trust", p.trust, "--log", "chain:"+testChain, "--source", testdir.New(t))
	if code != 3 || !strings.Contains(out, "INCOMPLETE: a check that was asked for could not be made") || !strings.Contains(out, "a publisher must serve what its signed checkpoint needs, and one that withholds it may be hiding a failure.") {
		t.Errorf("status 3: %d\n%s", code, out)
	}
}

func TestProofOutputLines(t *testing.T) {
	rows := manifestRows(t)
	r := rows[4]
	good := []string{"proof", "--proof", fx("proofs/" + r.Label + ".json"), "--digest", fx("digests/d5.json")}
	code, out, _ := run(t, good...)
	if code != 0 || !strings.Contains(out, "  ok  entry") || !strings.Contains(out, "written by") || !strings.Contains(out, "  ok  operation") ||
		!strings.Contains(out, "this holds only if the digest was obtained and kept outside the database's control") ||
		!strings.Contains(out, "no --table-id and --row-hash were given") {
		t.Fatalf("an unbound proof: %d\n%s", code, out)
	}
	// with only one of the two the line is still there, with both it is not
	for name, extra := range map[string][]string{"table only": {"--table-id", "1"}, "row only": {"--row-hash", r.RowHash}} {
		_, out, _ := run(t, append(append([]string{}, good...), extra...)...)
		if !strings.Contains(out, "no --table-id and --row-hash were given") {
			t.Errorf("%s: the line about the binding is gone:\n%s", name, out)
		}
	}
	_, out, _ = run(t, append(append([]string{}, good...), "--table-id", "1", "--row-hash", r.RowHash)...)
	if strings.Contains(out, "no --table-id and --row-hash were given") || !strings.Contains(out, "the row hash must be recomputed from data you hold") {
		t.Errorf("a bound proof:\n%s", out)
	}
	// help lists the flags
	_, out, _ = run(t, "proof", "--help")
	for _, f := range []string{"-proof", "-digest", "-table-id", "-row-hash", "-op", "-json"} {
		if !strings.Contains(out, f) {
			t.Errorf("--help does not list %s:\n%s", f, out)
		}
	}
	// the message of a table id that is not given
	if code, _, errs := run(t, "row-hash", "--columns", fx("columns/acc1.json")); code != 2 || !strings.Contains(errs, "got -1") {
		t.Errorf("row-hash without --table-id: %d %s", code, errs)
	}
	// the headline of blocks with a digest
	code, out, _ = run(t, "blocks", "--blocks", fx("blocks.jsonl"), "--digest", fx("digests/d8.json"))
	if code != 0 || !strings.Contains(out, "form an unbroken chain from the genesis hash and reach the digest at height") {
		t.Errorf("blocks with a digest: %d\n%s", code, out)
	}
}
