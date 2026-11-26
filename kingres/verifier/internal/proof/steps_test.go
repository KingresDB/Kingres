// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package proof

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/KingresDB/Kingres/kingres/verifier/internal/format"
)

// These tests came from a mutation run of the package: the steps a successful
// verification lists, the binding to the operation, the range of the fields, and
// the check that the two readings of a proof are the same bytes.

func stepNames(res *Result) []string {
	var out []string
	for _, s := range res.Steps {
		out = append(out, s.Name)
	}
	return out
}

func TestVerifyListsEveryStepInOrder(t *testing.T) {
	data := readFixture(t, "proofs/acc5.json")
	d5 := mustDigest(t, readFixture(t, "digests/d5.json"))
	p, err := Parse(data)
	if err != nil {
		t.Fatal(err)
	}
	res, err := Verify(p, d5, Expect{})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{StepChainID, StepEntryPath, StepTxPath, StepBlockHash, StepChainWalk, StepDigestTime}
	if got := stepNames(res); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("steps %v, want %v", got, want)
	}
	for _, s := range res.Steps {
		if s.Detail == "" {
			t.Errorf("step %s has no detail", s.Name)
		}
	}
	if d := res.Steps[4].Detail; !strings.Contains(d, fmt.Sprintf("through %d later header(s)", d5.Height-p.Block.Height)) {
		t.Errorf("chain walk detail: %q", d)
	}
	// with expectations the binding step comes first, for any one of them
	tid, rh, op := p.TableID, p.RowHash, p.Op
	for name, exp := range map[string]Expect{"table": {TableID: &tid}, "row": {RowHash: &rh}, "op": {Op: &op}} {
		res, err := Verify(p, d5, exp)
		if err != nil || len(res.Steps) != len(want)+1 || res.Steps[0].Name != StepBinding {
			t.Errorf("expectation %s: %v %v", name, err, res)
		}
	}
	// a digest without block_closed_at has no digest-time step
	d := d5
	d.HasClosedAt = false
	res, err = Verify(p, d, Expect{})
	if err != nil || len(res.Steps) != len(want)-1 {
		t.Errorf("a digest without a block time: %v %v", err, stepNames(res))
	}
}

func TestVerifyBindsTheOperation(t *testing.T) {
	data := readFixture(t, "proofs/acc5.json")
	d5 := mustDigest(t, readFixture(t, "digests/d5.json"))
	p, err := Parse(data)
	if err != nil {
		t.Fatal(err)
	}
	if p.Op != format.OpInsert {
		t.Fatalf("the fixture is for operation %c", p.Op)
	}
	del := format.OpDelete
	if _, err := Verify(p, d5, Expect{Op: &del}); !IsStep(err, StepBinding) || !strings.Contains(err.Error(), "operation I, expected D") {
		t.Errorf("a proof of an insert against an expected removal: %v", err)
	}
	ins := format.OpInsert
	if _, err := Verify(p, d5, Expect{Op: &ins}); err != nil {
		t.Errorf("the right operation: %v", err)
	}
}

func fixtureWith(t *testing.T, name string, set map[string]any) []byte {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(readFixture(t, name), &m); err != nil {
		t.Fatal(err)
	}
	for k, v := range set {
		if v == nil {
			delete(m, k)
		} else {
			m[k] = v
		}
	}
	out, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestParseRangesOfTheFields(t *testing.T) {
	// zero is a valid table id and ledger transaction id (and so is the largest)
	for _, v := range []float64{0, 1, 2147483647} {
		if _, err := Parse(fixtureWith(t, "proofs/acc5.json", map[string]any{"table_id": v})); err != nil {
			t.Errorf("table_id %v: %v", v, err)
		}
	}
	for _, v := range []float64{-1, 2147483648} {
		if _, err := Parse(fixtureWith(t, "proofs/acc5.json", map[string]any{"table_id": v})); err == nil {
			t.Errorf("table_id %v accepted", v)
		}
	}
	if _, err := Parse(fixtureWith(t, "proofs/acc5.json", map[string]any{"ledger_txid": 0})); err != nil {
		t.Errorf("ledger_txid 0: %v", err)
	}
	if _, err := Parse(fixtureWith(t, "proofs/acc5.json", map[string]any{"ledger_txid": -1})); err == nil {
		t.Error("ledger_txid -1 accepted")
	}
	// relation is optional, informational, and a string when present
	p, err := Parse(fixtureWith(t, "proofs/acc5.json", map[string]any{"relation": "public.accounts"}))
	if err != nil || p.Relation != "public.accounts" {
		t.Errorf("relation: %v %q", err, p.Relation)
	}
	if _, err := Parse(fixtureWith(t, "proofs/acc5.json", map[string]any{"relation": 7})); err == nil {
		t.Error("a relation that is a number was accepted")
	}
	p, err = Parse(fixtureWith(t, "proofs/acc5.json", map[string]any{"relation": nil}))
	if err != nil || p.Relation != "" {
		t.Errorf("no relation: %v %q", err, p.Relation)
	}
}

// Two readings of a file that have the same number of headers and other bytes
// are refused: the count alone does not say that the file is the same.
func TestStreamedProofWithOtherBytesAndTheSameCountIsRefused(t *testing.T) {
	data := readFixture(t, "proofs/acc5.json")
	d5 := mustDigest(t, readFixture(t, "digests/d5.json"))
	p, sum, err := ParseStream(sourceOf(data))
	if err != nil {
		t.Fatal(err)
	}
	other := append(bytes.Clone(data), ' ')
	if _, err := VerifyStream(p, sum, d5, Expect{}, sourceOf(other)); err == nil || !IsStep(err, StepParse) || !strings.Contains(err.Error(), "not the same") {
		t.Errorf("other bytes: %v", err)
	}
	// and a second reading with fewer headers than the first counted
	var m map[string]json.RawMessage
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatal(err)
	}
	var headers []json.RawMessage
	if err := json.Unmarshal(m["chain"], &headers); err != nil {
		t.Fatal(err)
	}
	// the same proof with the last header of the chain dropped: it no longer reaches the digest
	cut, _ := json.Marshal(headers[:len(headers)-1])
	m["chain"] = cut
	shorter, _ := json.Marshal(m)
	if _, err := VerifyStream(p, sum, d5, Expect{}, sourceOf(shorter)); err == nil {
		t.Error("a second reading with a header less was accepted")
	}
}

// A path of 64 hashes is the longest read (a tree of 2^64 leaves); 65 are refused,
// for each of the two paths of a proof.
func TestPathLengthLimitIsExact(t *testing.T) {
	zero := strings.Repeat("0", 64)
	path := func(n int) []any {
		out := make([]any, n)
		for i := range out {
			out[i] = zero
		}
		return out
	}
	for _, name := range []string{"entry_path", "tx_path"} {
		if _, err := Parse(fixtureWith(t, "proofs/acc5.json", map[string]any{name: path(64)})); err != nil {
			t.Errorf("%s of 64 hashes: %v", name, err)
		}
		if _, err := Parse(fixtureWith(t, "proofs/acc5.json", map[string]any{name: path(65)})); err == nil || !strings.Contains(err.Error(), "has 65 hashes") {
			t.Errorf("%s of 65 hashes: %v", name, err)
		}
	}
}
