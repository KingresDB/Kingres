// SPDX-License-Identifier: AGPL-3.0-only
// Portions Copyright (c) 2026, RooyeKhat Media Co

package ops

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/api"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/problem"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/sqlx"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/store"
)

func raw(s string) json.RawMessage { return json.RawMessage(s) }

func problemCodeOf(err error) problem.Code {
	var p *problem.Problem
	if errors.As(err, &p) {
		return p.Code
	}
	return ""
}

func TestValidateProofRequest(t *testing.T) {
	one := int64(1)
	neg := int64(-1)
	good := api.InclusionProofRequest{Schema: "public", Table: "accounts", PrimaryKey: map[string]json.RawMessage{"id": raw("42")}}
	pr, err := validateProofRequest(good)
	if err != nil || pr.op != "I" {
		t.Fatalf("good request: %+v %v", pr, err)
	}
	cases := []struct {
		name  string
		mut   func(*api.InclusionProofRequest)
		field string
	}{
		{"empty schema", func(r *api.InclusionProofRequest) { r.Schema = "" }, "/schema"},
		{"long table", func(r *api.InclusionProofRequest) { r.Table = strings.Repeat("t", 64) }, "/table"},
		{"bad op", func(r *api.InclusionProofRequest) { r.Op = "U" }, "/op"},
		{"op D without txid", func(r *api.InclusionProofRequest) { r.Op = "D" }, "/ledgerTxId"},
		{"negative txid", func(r *api.InclusionProofRequest) { r.LedgerTxID = &neg }, "/ledgerTxId"},
		{"no key", func(r *api.InclusionProofRequest) { r.PrimaryKey = nil }, "/primaryKey"},
		{"null value", func(r *api.InclusionProofRequest) { r.PrimaryKey = map[string]json.RawMessage{"id": raw("null")} }, "/primaryKey/id"},
		{"object value", func(r *api.InclusionProofRequest) { r.PrimaryKey = map[string]json.RawMessage{"id": raw(`{"a":1}`)} }, "/primaryKey/id"},
		{"array value", func(r *api.InclusionProofRequest) { r.PrimaryKey = map[string]json.RawMessage{"id": raw(`[1]`)} }, "/primaryKey/id"},
	}
	for _, c := range cases {
		r := good
		c.mut(&r)
		_, err := validateProofRequest(r)
		var p *problem.Problem
		if !errors.As(err, &p) || p.Code != problem.ValidationFailed {
			t.Errorf("%s: %v", c.name, err)
			continue
		}
		found := false
		for _, f := range p.Errors {
			if f.Field == c.field {
				found = true
			}
		}
		if !found {
			t.Errorf("%s: no error for %s: %+v", c.name, c.field, p.Errors)
		}
	}
	// A removed version of an updatable table, with a string for a big bigint.
	r := good
	r.LedgerTxID, r.Op = &one, "D"
	r.PrimaryKey = map[string]json.RawMessage{"id": raw(`"9007199254740993"`), "ok": raw("true")}
	if pr, err := validateProofRequest(r); err != nil || pr.op != "D" {
		t.Errorf("D request: %+v %v", pr, err)
	}
}

func TestCheckProofTarget(t *testing.T) {
	one := int64(1)
	pk := map[string]json.RawMessage{"id": raw("1")}
	base := proofTable{mode: "updatable", pkCols: []string{"id"}, extVerion: "0.2"}
	if err := checkProofTarget(base, proofRequest{pk: pk, op: "I"}); err != nil {
		t.Errorf("good: %v", err)
	}
	if c := problemCodeOf(checkProofTarget(proofTable{mode: "append_only", pkCols: []string{"id"}, extVerion: "0.2"}, proofRequest{pk: pk, op: "I", txid: &one})); c != problem.Unprocessable {
		t.Errorf("append-only with ledgerTxId: %q", c)
	}
	if c := problemCodeOf(checkProofTarget(proofTable{mode: "append_only", pkCols: []string{"id"}, extVerion: "0.1"}, proofRequest{pk: pk, op: "D", txid: &one})); c != problem.UnsupportedOp {
		t.Errorf("0.1 with D: %q", c)
	}
	if c := problemCodeOf(checkProofTarget(proofTable{mode: "append_only", extVerion: "0.2"}, proofRequest{pk: pk, op: "I"})); c != problem.Unprocessable {
		t.Errorf("no primary key: %q", c)
	}
	two := proofTable{mode: "append_only", pkCols: []string{"a", "b"}, extVerion: "0.2"}
	if c := problemCodeOf(checkProofTarget(two, proofRequest{pk: map[string]json.RawMessage{"a": raw("1")}, op: "I"})); c != problem.ValidationFailed {
		t.Errorf("missing column: %q", c)
	}
	if c := problemCodeOf(checkProofTarget(two, proofRequest{pk: map[string]json.RawMessage{"a": raw("1"), "b": raw("2"), "c": raw("3")}, op: "I"})); c != problem.ValidationFailed {
		t.Errorf("extra column: %q", c)
	}
	dist := proofTable{mode: "append_only", pkCols: []string{"id", "tenant"}, extVerion: "0.2", hasDist: true, distCol: "tenant"}
	if c := problemCodeOf(checkProofTarget(dist, proofRequest{pk: map[string]json.RawMessage{"id": raw("1"), "tenant": raw("2")}, op: "I"})); c != "" {
		t.Errorf("distribution column in the key: %q", c)
	}
	dist.pkCols = []string{"id"}
	if err := checkProofTarget(dist, proofRequest{pk: pk, op: "I"}); problemCodeOf(err) != problem.ValidationFailed || !strings.Contains(err.Error(), "Validation") && !strings.Contains(err.Error(), "primary key") {
		t.Errorf("distribution column missing from the key: %v", err)
	}
}

func TestRowHashSQLUsesOnlyIdentifiersAndParameters(t *testing.T) {
	rel, _ := sqlx.QualifiedIdent("pub lic", `we"ird`)
	hostile := `1'; DROP TABLE x; --`
	doc, _ := json.Marshal(map[string]string{`i"d`: hostile})
	st, err := rowHashSQL(rel, []string{`i"d`, "b"}, nil, false, doc)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(st.SQL, "DROP") || strings.Contains(st.SQL, hostile) {
		t.Errorf("value in the text: %s", st.SQL)
	}
	if !strings.Contains(st.SQL, `"pub lic"."we""ird"`) || !strings.Contains(st.SQL, `"i""d"`) {
		t.Errorf("identifiers not quoted: %s", st.SQL)
	}
	if len(st.Args) != 1 {
		t.Errorf("args = %v", st.Args)
	}
	tx := int64(7)
	st, err = rowHashSQL(rel, []string{"id"}, &tx, true, doc)
	if err != nil || !strings.Contains(st.SQL, "ledger_history_row_hash") || !strings.Contains(st.SQL, "ledger_txid") || len(st.Args) != 2 {
		t.Errorf("history statement: %q %v %v", st.SQL, st.Args, err)
	}
	if _, err := rowHashSQL(rel, []string{"bad\x00name"}, nil, false, doc); err == nil {
		t.Error("an invalid column name was accepted")
	}
}

func TestVerifyCursor(t *testing.T) {
	c := encodeVerifyCursor(42)
	if id, ok, err := decodeVerifyCursor(c); err != nil || !ok || id != 42 {
		t.Errorf("round trip: %d %v %v", id, ok, err)
	}
	if _, ok, err := decodeVerifyCursor(""); err != nil || ok {
		t.Errorf("empty: %v %v", ok, err)
	}
	for _, bad := range []string{"%%%", store.EncodeKey(5), store.EncodeOffset(5), encodeVerifyCursor(0), "dm5vcGU"} {
		if _, _, err := decodeVerifyCursor(bad); !errors.Is(err, store.ErrInvalidCursor) {
			t.Errorf("%q accepted: %v", bad, err)
		}
	}
}

func TestToVerificationMapsStates(t *testing.T) {
	at := time.Date(2026, 11, 14, 10, 0, 0, 0, time.UTC)
	fin := at.Add(time.Second)
	n, rows := int64(5), int64(9)
	task := int64(77)
	base := extVerification{id: 3, startedAt: at, finishedAt: &fin, startedBy: "kingresd", from: 0, blocks: &n, rows: &rows}
	link := verificationLink{id: 10, extID: 3, taskID: &task}

	clean := base
	clean.state = "clean"
	v := toVerification("shop", link, clean, nil)
	if v.State != "passed" || v.Scope != "full" || v.TaskID == nil || *v.TaskID != 77 || v.DigestID != nil ||
		len(v.Inconsistencies) != 0 || *v.BlocksChecked != 5 || *v.StartedBy != "system" || v.ID != 10 {
		t.Errorf("clean: %+v", v)
	}
	by := "alice"
	link2 := link
	link2.creator, link2.taskID = &by, nil
	prob := base
	prob.state, prob.from = "problems", 4
	prob.problems = []byte(`[{"check":"block_hash","object":"block 7","detail":"expected aa, found bb"},
		{"check":"table_rows","object":"public.acc","detail":"row hash x: 1 row(s)"},
		{"check":"trigger","object":"public.acc","detail":"missing"},
		{"check":"chain_id","object":null,"detail":"no chain"},
		{"check":"tx_block","object":"tx 5","detail":"gone"}]`)
	v = toVerification("shop", link2, prob, nil)
	if v.State != "failed" || v.Scope != "sinceLastVerified" || v.TaskID != nil || *v.StartedBy != "alice" || len(v.Inconsistencies) != 5 {
		t.Fatalf("problems: %+v", v)
	}
	i := v.Inconsistencies
	if i[0].Kind != "blockHashMismatch" || i[0].BlockHeight == nil || *i[0].BlockHeight != 7 || i[0].Table != nil {
		t.Errorf("block hash: %+v", i[0])
	}
	if i[1].Kind != "rowHashMismatch" || i[1].Table == nil || *i[1].Table != "public.acc" {
		t.Errorf("row hash: %+v", i[1])
	}
	if i[2].Kind != "other" || !strings.HasPrefix(i[2].Detail, "trigger: ") {
		t.Errorf("other: %+v", i[2])
	}
	if i[3].Kind != "other" || i[3].Table != nil || !strings.HasPrefix(i[3].Detail, "chain_id: ") {
		t.Errorf("no object: %+v", i[3])
	}
	if i[4].Kind != "missingBlock" {
		t.Errorf("tx_block: %+v", i[4])
	}

	failed := base
	failed.state = "failed"
	failed.problems = []byte(`[{"check":"verification_failed","object":null,"detail":"boom","sqlstate":"57014"}]`)
	v = toVerification("shop", verificationLink{id: 11, extID: 4}, failed, nil)
	if v.State != "failed" || len(v.Inconsistencies) != 0 || v.BlocksChecked != nil || v.RowsChecked != nil || v.TaskID != nil {
		t.Errorf("aborted run: %+v", v)
	}
	out, _ := json.Marshal(v)
	for _, want := range []string{`"taskId":null`, `"digestId":null`, `"inconsistencies":[]`} {
		if !strings.Contains(string(out), want) {
			t.Errorf("%s missing in %s", want, out)
		}
	}
}
