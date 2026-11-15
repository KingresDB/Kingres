// SPDX-License-Identifier: AGPL-3.0-only
// Portions Copyright (c) 2026, RooyeKhat Media Co

package main

// End-to-end tests of createInclusionProof, listLedgerVerifications and
// getLedgerVerification against a real kingresd and PostgreSQL with the
// installed kingres_ledger, including the standalone verifier (kingres-verify)
// fed with the bytes the API returned.

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// rawCall performs a JSON request and returns the status and the body bytes
// exactly as the client receives them.
func (e *cp) rawCall(method, path string, body any) (int, []byte) {
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, e.base+path, rd)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if e.token != "" {
		req.Header.Set("Authorization", "Bearer "+e.token)
	}
	resp, err := e.client.Do(req)
	if err != nil {
		return 0, []byte(err.Error())
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, raw
}

func (e *cp) sql(t testing.TB, db, q string) {
	t.Helper()
	e.expect(t, 200, "POST", "/query", map[string]any{"database": db, "sql": q})
}

// proof asks for an inclusion proof and returns the status and the raw body.
func (e *cp) proof(db string, body map[string]any) (int, []byte) {
	return e.rawCall("POST", "/databases/"+db+"/ledger/inclusion-proofs", body)
}

func asMap(t testing.TB, raw []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("not a JSON object: %v: %s", err, raw)
	}
	return m
}

func wantProblemRaw(t testing.TB, st int, raw []byte, wantStatus int, code string) {
	t.Helper()
	m := asMap(t, raw)
	if st != wantStatus || (code != "" && problemCode(m) != code) {
		t.Errorf("got %d %v, want %d %s", st, m, wantStatus, code)
	}
}

func verifyBin(t testing.TB) string {
	t.Helper()
	p := os.Getenv("KINGRES_VERIFY_BIN")
	if p == "" {
		// PostgreSQL is available (the caller got a cluster): the verifier must be too.
		t.Fatal("KINGRES_VERIFY_BIN is not set; scripts/run-integration.ps1 builds kingres-verify and run-wsl-tests.sh exports it")
	}
	return p
}

// runVerify runs kingres-verify and returns its exit status and combined output.
func runVerify(t testing.TB, args ...string) (int, string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, verifyBin(t), args...)
	out, err := cmd.CombinedOutput()
	if err == nil {
		return 0, string(out)
	}
	if ee, ok := err.(*exec.ExitError); ok {
		return ee.ExitCode(), string(out)
	}
	t.Fatalf("running kingres-verify: %v", err)
	return -1, ""
}

func writeFile(t testing.TB, dir, name string, data []byte) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func int4send(v int) string { return fmt.Sprintf("%08x", uint32(v)) }

// columnsFile is the kingres-verify row-hash input of a row of (id int, text).
func columnsJSON(cols ...[2]string) []byte {
	type col struct {
		Name    string `json:"name"`
		SendHex string `json:"send_hex"`
	}
	var cs []col
	for _, c := range cols {
		cs = append(cs, col{c[0], c[1]})
	}
	b, _ := json.Marshal(cs)
	return b
}

// rowHash computes the row hash the way an auditor would: from the values.
func rowHash(t testing.TB, dir string, tableID int, name string, cols ...[2]string) string {
	t.Helper()
	f := writeFile(t, dir, name, columnsJSON(cols...))
	st, out := runVerify(t, "row-hash", "--table-id", fmt.Sprint(tableID), "--columns", f)
	if st != 0 {
		t.Fatalf("row-hash: %d %s", st, out)
	}
	h := strings.TrimSpace(out)
	if len(h) != 64 {
		t.Fatalf("row-hash output %q", out)
	}
	return h
}

// digestDocument returns the digest of the chain exactly as the API gave it.
func digestDocument(t testing.TB, raw []byte) []byte {
	t.Helper()
	var d struct {
		Chains []struct {
			Document json.RawMessage `json:"document"`
		} `json:"chains"`
	}
	if err := json.Unmarshal(raw, &d); err != nil || len(d.Chains) != 1 || len(d.Chains[0].Document) == 0 {
		t.Fatalf("digest without a document: %v %s", err, raw)
	}
	return d.Chains[0].Document
}

func TestInclusionProofsAndTheVerifier(t *testing.T) {
	e := shared(t)
	verifyBin(t)
	dir := t.TempDir()
	e.mustTask(t, "POST", "/databases", map[string]any{"name": "pfdb", "ledger": map[string]any{"mode": "optIn"}})
	e.sql(t, "pfdb", "create table public.acct (id int primary key, owner text)")
	e.sql(t, "pfdb", "create table public.inv (id int primary key, qty int)")
	e.sql(t, "pfdb", "create table public.plain (id int primary key)")
	e.sql(t, "pfdb", "create table public.nopk (v int)")
	e.sql(t, "pfdb", "create table public.pair (a int, b text, c int, primary key (a, b))")
	e.expect(t, 200, "PUT", "/databases/pfdb/schemas/public/tables/acct/ledger", map[string]any{"mode": "appendOnly"})
	e.expect(t, 200, "PUT", "/databases/pfdb/schemas/public/tables/inv/ledger", map[string]any{"mode": "updatable"})
	e.expect(t, 200, "PUT", "/databases/pfdb/schemas/public/tables/nopk/ledger", map[string]any{"mode": "appendOnly"})
	e.expect(t, 200, "PUT", "/databases/pfdb/schemas/public/tables/pair/ledger", map[string]any{"mode": "appendOnly"})
	e.sql(t, "pfdb", "insert into public.acct values (1, 'alice'), (2, 'bob')")
	e.sql(t, "pfdb", "insert into public.inv values (1, 10)")
	e.sql(t, "pfdb", "insert into public.pair values (5, 'x', 1)")

	// A row that is not in a block yet: 409 ledger_entry_pending. (The ledger
	// worker closes a block every 10 seconds; the request comes right after the
	// insert, so it has to be pending, unless the worker ran in between.)
	if st, raw := e.proof("pfdb", map[string]any{"schema": "public", "table": "acct", "primaryKey": map[string]any{"id": 1}}); st == 409 {
		wantProblemRaw(t, st, raw, 409, "ledger_entry_pending")
		if m := asMap(t, raw); !strings.Contains(fmt.Sprint(m["detail"]), "digest") {
			t.Errorf("the pending answer does not say what to do: %v", m)
		}
	} else if st == 200 {
		t.Log("the ledger worker closed the block before the first request; the pending answer was not observed")
	} else {
		t.Errorf("pending: %d %s", st, raw)
	}

	// A digest closes the block and is the trusted anchor of the proofs.
	st, draw := e.rawCall("POST", "/databases/pfdb/ledger/digests", nil)
	if st != 201 {
		t.Fatalf("digest: %d %s", st, draw)
	}
	if err := chk(t).Response("generateLedgerDigest", "201", draw); err != nil {
		t.Errorf("digest does not match the contract: %v\n%s", err, draw)
	}
	digest := writeFile(t, dir, "digest.json", digestDocument(t, draw))

	st, praw := e.proof("pfdb", map[string]any{"schema": "public", "table": "acct", "primaryKey": map[string]any{"id": 1}})
	if st != 200 {
		t.Fatalf("proof: %d %s", st, praw)
	}
	if err := chk(t).Response("createInclusionProof", "200", praw); err != nil {
		t.Errorf("proof does not match the contract: %v\n%s", err, praw)
	}
	proof := asMap(t, praw)
	if proof["op"] != "I" || proof["relation"] != "public.acct" || proof["shard_group_id"] != float64(0) || proof["format"] != float64(1) {
		t.Errorf("proof %v", proof)
	}
	tableID := int(proof["table_id"].(float64))
	good := rowHash(t, dir, tableID, "acct1.json", [2]string{"id", int4send(1)}, [2]string{"owner", hex.EncodeToString([]byte("alice"))})
	if good != proof["row_hash"] {
		t.Errorf("the row hash recomputed from the values (%s) is not the proof's (%v)", good, proof["row_hash"])
	}
	proofFile := writeFile(t, dir, "proof.json", praw)
	st2, out := runVerify(t, "proof", "--proof", proofFile, "--digest", digest, "--table-id", fmt.Sprint(tableID), "--row-hash", good, "--op", "I")
	if st2 != 0 || !strings.Contains(out, "VERIFIED") {
		t.Errorf("kingres-verify rejected the API's proof: %d\n%s", st2, out)
	}

	// Negative: a tampered proof (a flipped hash) is rejected, and so is a proof
	// for another row's hash.
	tampered := bytes.Replace(praw, []byte(proof["tx_root"].(string)), []byte(flipHex(proof["tx_root"].(string))), 1)
	if bytes.Equal(tampered, praw) {
		t.Fatal("test bug: nothing was tampered")
	}
	bad := writeFile(t, dir, "tampered.json", tampered)
	st2, out = runVerify(t, "proof", "--proof", bad, "--digest", digest)
	if st2 != 1 || strings.Contains(out, "VERIFIED") {
		t.Errorf("a tampered proof was not rejected: %d\n%s", st2, out)
	}
	other := rowHash(t, dir, tableID, "acct2.json", [2]string{"id", int4send(2)}, [2]string{"owner", hex.EncodeToString([]byte("bob"))})
	st2, out = runVerify(t, "proof", "--proof", proofFile, "--digest", digest, "--table-id", fmt.Sprint(tableID), "--row-hash", other)
	if st2 != 1 || strings.Contains(out, "VERIFIED") {
		t.Errorf("a proof was accepted for another row: %d\n%s", st2, out)
	}

	// A composite primary key, with a text value.
	e.expect(t, 201, "POST", "/databases/pfdb/ledger/digests", nil)
	st, praw = e.proof("pfdb", map[string]any{"schema": "public", "table": "pair", "primaryKey": map[string]any{"a": "5", "b": "x"}})
	if st != 200 {
		t.Fatalf("composite proof: %d %s", st, praw)
	}

	// An updatable table: the current version, the removed version and its removal.
	e.sql(t, "pfdb", "update public.inv set qty = 11 where id = 1")
	st, draw = e.rawCall("POST", "/databases/pfdb/ledger/digests", nil)
	if st != 201 {
		t.Fatalf("digest 2: %d %s", st, draw)
	}
	digest2 := writeFile(t, dir, "digest2.json", digestDocument(t, draw))
	sc := e.serviceConn(t, "pfdb")
	var txid int64
	if err := sc.QueryRow(context.Background(), "select ledger_txid from public.inv_ledger_history where id = 1").Scan(&txid); err != nil {
		t.Fatal(err)
	}
	st, praw = e.proof("pfdb", map[string]any{"schema": "public", "table": "inv", "primaryKey": map[string]any{"id": 1}})
	if st != 200 {
		t.Fatalf("current version: %d %s", st, praw)
	}
	cur := asMap(t, praw)
	invID := int(cur["table_id"].(float64))
	st, praw = e.proof("pfdb", map[string]any{"schema": "public", "table": "inv", "primaryKey": map[string]any{"id": 1}, "ledgerTxId": txid, "op": "D"})
	if st != 200 {
		t.Fatalf("removal proof: %d %s", st, praw)
	}
	if err := chk(t).Response("createInclusionProof", "200", praw); err != nil {
		t.Errorf("removal proof does not match the contract: %v", err)
	}
	rem := asMap(t, praw)
	if rem["op"] != "D" || rem["ledger_txid"] != float64(txid) || rem["row_hash"] == cur["row_hash"] {
		t.Errorf("removal proof %v (current %v)", rem, cur["row_hash"])
	}
	oldHash := rowHash(t, dir, invID, "inv_old.json", [2]string{"id", int4send(1)}, [2]string{"qty", int4send(10)})
	if oldHash != rem["row_hash"] {
		t.Errorf("hash of the removed version %s, proof %v", oldHash, rem["row_hash"])
	}
	remFile := writeFile(t, dir, "removal.json", praw)
	if st2, out = runVerify(t, "proof", "--proof", remFile, "--digest", digest2, "--table-id", fmt.Sprint(invID), "--row-hash", oldHash, "--op", "D"); st2 != 0 {
		t.Errorf("kingres-verify rejected the removal proof: %d\n%s", st2, out)
	}
	// The removed version was also written once (op I of the same hash).
	st, praw = e.proof("pfdb", map[string]any{"schema": "public", "table": "inv", "primaryKey": map[string]any{"id": 1}, "ledgerTxId": txid})
	if st != 200 || asMap(t, praw)["op"] != "I" {
		t.Errorf("insert proof of the removed version: %d %s", st, praw)
	}

	// Refusals.
	acct := map[string]any{"id": 1}
	cases := []struct {
		name   string
		db     string
		body   map[string]any
		status int
		code   string
	}{
		{"row not found", "pfdb", map[string]any{"schema": "public", "table": "acct", "primaryKey": map[string]any{"id": 999}}, 404, "not_found"},
		{"table not found", "pfdb", map[string]any{"schema": "public", "table": "nosuch", "primaryKey": acct}, 404, "not_found"},
		{"schema not found", "pfdb", map[string]any{"schema": "nosuch", "table": "acct", "primaryKey": acct}, 404, "not_found"},
		{"database not found", "nosuchdb", map[string]any{"schema": "public", "table": "acct", "primaryKey": acct}, 404, "not_found"},
		{"removed version not found", "pfdb", map[string]any{"schema": "public", "table": "inv", "primaryKey": map[string]any{"id": 1}, "ledgerTxId": 1}, 404, "not_found"},
		{"not a ledger table", "pfdb", map[string]any{"schema": "public", "table": "plain", "primaryKey": acct}, 422, "unprocessable"},
		{"no primary key", "pfdb", map[string]any{"schema": "public", "table": "nopk", "primaryKey": map[string]any{"v": 1}}, 422, "unprocessable"},
		{"ledgerTxId on an append-only table", "pfdb", map[string]any{"schema": "public", "table": "acct", "primaryKey": acct, "ledgerTxId": 5}, 422, "unprocessable"},
		{"op D on an append-only table", "pfdb", map[string]any{"schema": "public", "table": "acct", "primaryKey": acct, "op": "D", "ledgerTxId": 5}, 422, "unprocessable"},
		{"op D without ledgerTxId", "pfdb", map[string]any{"schema": "public", "table": "inv", "primaryKey": map[string]any{"id": 1}, "op": "D"}, 400, "validation_failed"},
		{"bad op", "pfdb", map[string]any{"schema": "public", "table": "acct", "primaryKey": acct, "op": "X"}, 400, "validation_failed"},
		{"missing key column", "pfdb", map[string]any{"schema": "public", "table": "pair", "primaryKey": map[string]any{"a": 5}}, 400, "validation_failed"},
		{"extra key column", "pfdb", map[string]any{"schema": "public", "table": "acct", "primaryKey": map[string]any{"id": 1, "owner": "alice"}}, 400, "validation_failed"},
		{"value of the wrong type", "pfdb", map[string]any{"schema": "public", "table": "acct", "primaryKey": map[string]any{"id": "abc"}}, 400, "validation_failed"},
		{"value out of range", "pfdb", map[string]any{"schema": "public", "table": "acct", "primaryKey": map[string]any{"id": "99999999999"}}, 400, "validation_failed"},
		{"null value", "pfdb", map[string]any{"schema": "public", "table": "acct", "primaryKey": map[string]any{"id": nil}}, 400, "validation_failed"},
		{"empty key", "pfdb", map[string]any{"schema": "public", "table": "acct", "primaryKey": map[string]any{}}, 400, "validation_failed"},
		{"unknown member", "pfdb", map[string]any{"schema": "public", "table": "acct", "primaryKey": acct, "extra": 1}, 400, "validation_failed"},
	}
	for _, c := range cases {
		st, raw := e.proof(c.db, c.body)
		if st != c.status || problemCode(asMap(t, raw)) != c.code {
			t.Errorf("%s: %d %s, want %d %s", c.name, st, raw, c.status, c.code)
		}
	}

	// The role's privileges decide: a role without SELECT is refused (403), the
	// same role with SELECT gets the proof, and the removed versions are read
	// from the history table, which only the owner of the table can read.
	reader := e.accountWithRole(t, "pf_reader", "kingres_readonly")
	r := e.as(reader)
	body := map[string]any{"schema": "public", "table": "acct", "primaryKey": acct}
	st, raw := r.proof("pfdb", body)
	wantProblemRaw(t, st, raw, 403, "forbidden")
	if _, err := sc.Exec(context.Background(), "grant select on public.acct, public.inv to pf_reader"); err != nil {
		t.Fatal(err)
	}
	if st, raw = r.proof("pfdb", body); st != 200 {
		t.Errorf("reader with SELECT: %d %s", st, raw)
	}
	st, raw = r.proof("pfdb", map[string]any{"schema": "public", "table": "inv", "primaryKey": map[string]any{"id": 1}, "ledgerTxId": txid})
	wantProblemRaw(t, st, raw, 403, "forbidden")
	// Row security applies: a policy that hides the row makes it "not found".
	if _, err := sc.Exec(context.Background(), "alter table public.acct enable row level security; create policy only_bob on public.acct for select to pf_reader using (owner = 'bob')"); err != nil {
		t.Fatal(err)
	}
	if st, raw = r.proof("pfdb", body); st != 404 {
		t.Errorf("row hidden by row security: %d %s", st, raw)
	}
	if st, raw = r.proof("pfdb", map[string]any{"schema": "public", "table": "acct", "primaryKey": map[string]any{"id": 2}}); st != 200 {
		t.Errorf("row visible through the policy: %d %s", st, raw)
	}
	// The cluster role decides who may ask at all.
	stats := e.accountWithRole(t, "pf_stats", "kingres_stats")
	st, raw = e.as(stats).proof("pfdb", body)
	wantProblemRaw(t, st, raw, 403, "forbidden")
	st, raw = e.anon().proof("pfdb", body)
	wantProblemRaw(t, st, raw, 401, "")
	if _, err := sc.Exec(context.Background(), "drop owned by pf_reader"); err != nil {
		t.Fatal(err)
	}
	e.expect(t, 204, "DELETE", "/roles/pf_reader", nil)
	e.expect(t, 204, "DELETE", "/roles/pf_stats", nil)
}

func flipHex(h string) string {
	b := []byte(h)
	if b[0] == 'a' {
		b[0] = 'b'
	} else {
		b[0] = 'a'
	}
	return string(b)
}

func TestInclusionProofOnLedger01(t *testing.T) {
	e := shared(t)
	e.mustTask(t, "POST", "/databases", map[string]any{"name": "pf01"})
	sc := e.serviceConn(t, "pf01")
	ctx := context.Background()
	if _, err := sc.Exec(ctx, "create extension kingres_ledger version '0.1'"); err != nil {
		t.Fatal(err)
	}
	e.sql(t, "pf01", "create table public.t (id int primary key)")
	e.expect(t, 200, "PUT", "/databases/pf01/schemas/public/tables/t/ledger", map[string]any{"mode": "appendOnly"})
	e.sql(t, "pf01", "insert into public.t values (1)")
	if _, err := sc.Exec(ctx, "select kingres.ledger_build_block()"); err != nil {
		t.Fatal(err)
	}
	key := map[string]any{"id": 1}
	// The insert proof works with 0.1; removed versions need 0.2.
	st, raw := e.proof("pf01", map[string]any{"schema": "public", "table": "t", "primaryKey": key})
	if st != 200 {
		t.Fatalf("0.1 proof: %d %s", st, raw)
	}
	if m := asMap(t, raw); m["op"] != nil && m["op"] != "I" {
		t.Errorf("0.1 proof %v", m)
	}
	for _, body := range []map[string]any{
		{"schema": "public", "table": "t", "primaryKey": key, "ledgerTxId": 1},
		{"schema": "public", "table": "t", "primaryKey": key, "op": "D", "ledgerTxId": 1},
	} {
		st, raw := e.proof("pf01", body)
		wantProblemRaw(t, st, raw, 422, "unsupported_operation")
	}
	// A list of verifications of a 0.1 database is empty.
	out := e.expect(t, 200, "GET", "/databases/pf01/ledger/verifications", nil)
	valid(t, "listLedgerVerifications", "200", out)
	if items := out["items"].([]any); len(items) != 0 || out["nextCursor"] != nil {
		t.Errorf("verifications of a 0.1 database: %v", out)
	}
	e.mustTask(t, "DELETE", "/databases/pf01", nil)
}

func TestInclusionProofOnADatabaseWithoutLedger(t *testing.T) {
	e := shared(t)
	e.mustTask(t, "POST", "/databases", map[string]any{"name": "pfplain"})
	e.sql(t, "pfplain", "create table public.t (id int primary key)")
	st, raw := e.proof("pfplain", map[string]any{"schema": "public", "table": "t", "primaryKey": map[string]any{"id": 1}})
	wantProblemRaw(t, st, raw, 409, "conflict")
	out := e.expect(t, 200, "GET", "/databases/pfplain/ledger/verifications", nil)
	valid(t, "listLedgerVerifications", "200", out)
	if len(out["items"].([]any)) != 0 {
		t.Errorf("verifications of a database without ledger: %v", out)
	}
	e.expect(t, 404, "GET", "/databases/nosuchdb/ledger/verifications", nil)
	e.mustTask(t, "DELETE", "/databases/pfplain", nil)
}

func TestLedgerVerificationRecords(t *testing.T) {
	e := shared(t)
	e.mustTask(t, "POST", "/databases", map[string]any{"name": "vrdb", "ledger": map[string]any{"mode": "optIn"}})
	e.mustTask(t, "POST", "/databases", map[string]any{"name": "vrdb2", "ledger": map[string]any{"mode": "optIn"}})
	e.sql(t, "vrdb", "create table public.t (id int primary key, v text)")
	e.expect(t, 200, "PUT", "/databases/vrdb/schemas/public/tables/t/ledger", map[string]any{"mode": "appendOnly"})
	e.sql(t, "vrdb", "insert into public.t values (1, 'a')")
	e.expect(t, 201, "POST", "/databases/vrdb/ledger/digests", nil)

	// No run yet.
	out := e.expect(t, 200, "GET", "/databases/vrdb/ledger/verifications", nil)
	valid(t, "listLedgerVerifications", "200", out)
	if len(out["items"].([]any)) != 0 || out["nextCursor"] != nil {
		t.Fatalf("before any run: %v", out)
	}

	// A run started through the API is linked to its task.
	t1 := e.mustTask(t, "POST", "/databases/vrdb/ledger/verifications", map[string]any{"scope": "full"})
	// A run recorded in SQL has no task.
	sc := e.serviceConn(t, "vrdb")
	if _, err := sc.Exec(context.Background(), "select kingres.ledger_verify_recorded()"); err != nil {
		t.Fatal(err)
	}
	e.sql(t, "vrdb", "insert into public.t values (2, 'b')")
	e.expect(t, 201, "POST", "/databases/vrdb/ledger/digests", nil)
	t2 := e.mustTask(t, "POST", "/databases/vrdb/ledger/verifications", map[string]any{"scope": "sinceLastVerified"})

	list := e.expect(t, 200, "GET", "/databases/vrdb/ledger/verifications", nil)
	valid(t, "listLedgerVerifications", "200", list)
	items := list["items"].([]any)
	if len(items) != 3 || list["nextCursor"] != nil {
		t.Fatalf("runs: %v", list)
	}
	// Newest first, ids descending.
	first, second, third := items[0].(map[string]any), items[1].(map[string]any), items[2].(map[string]any)
	if !(first["id"].(float64) > second["id"].(float64) && second["id"].(float64) > third["id"].(float64)) {
		t.Errorf("ids are not descending: %v", items)
	}
	if first["taskId"] != t2["id"] || first["state"] != "passed" || first["scope"] != "sinceLastVerified" || first["databaseName"] != "vrdb" {
		t.Errorf("newest run: %v (task %v)", first, t2["id"])
	}
	if second["taskId"] != nil || second["state"] != "passed" || second["startedBy"] != "system" {
		t.Errorf("run recorded in SQL: %v", second)
	}
	if third["taskId"] != t1["id"] || third["scope"] != "full" || third["digestId"] != nil {
		t.Errorf("first run: %v (task %v)", third, t1["id"])
	}
	if third["startedBy"] != adminUser || third["blocksChecked"] == nil || third["rowsChecked"] == nil || third["finishedAt"] == nil {
		t.Errorf("first run details: %v", third)
	}
	if inc, ok := third["inconsistencies"].([]any); !ok || len(inc) != 0 {
		t.Errorf("inconsistencies: %v", third["inconsistencies"])
	}

	// Get by id, the same record.
	for _, it := range items {
		m := it.(map[string]any)
		got := e.expect(t, 200, "GET", fmt.Sprintf("/databases/vrdb/ledger/verifications/%d", int64(m["id"].(float64))), nil)
		valid(t, "getLedgerVerification", "200", got)
		a, _ := json.Marshal(m)
		b, _ := json.Marshal(got)
		if string(a) != string(b) {
			t.Errorf("get differs from list:\n%s\n%s", a, b)
		}
	}
	// 404: unknown id, the id of another database, another database's name.
	e.expect(t, 404, "GET", "/databases/vrdb/ledger/verifications/999999", nil)
	e.expect(t, 404, "GET", fmt.Sprintf("/databases/vrdb2/ledger/verifications/%d", int64(first["id"].(float64))), nil)
	e.expect(t, 404, "GET", fmt.Sprintf("/databases/nosuchdb/ledger/verifications/%d", int64(first["id"].(float64))), nil)

	// Paging: one at a time, no item twice, the last page has no cursor.
	var seen []float64
	cursor, pages := "", 0
	for {
		path := "/databases/vrdb/ledger/verifications?limit=1"
		if cursor != "" {
			path += "&cursor=" + cursor
		}
		pg := e.expect(t, 200, "GET", path, nil)
		valid(t, "listLedgerVerifications", "200", pg)
		its := pg["items"].([]any)
		if len(its) != 1 {
			t.Fatalf("page %d: %v", pages, pg)
		}
		seen = append(seen, its[0].(map[string]any)["id"].(float64))
		pages++
		if pg["nextCursor"] == nil {
			break
		}
		cursor = pg["nextCursor"].(string)
		if pages > 5 {
			t.Fatal("paging does not end")
		}
	}
	if pages != 3 || seen[0] != first["id"] || seen[1] != second["id"] || seen[2] != third["id"] {
		t.Errorf("paging order %v over %d pages", seen, pages)
	}
	// A run that starts while a client pages appears only on a fresh first page.
	pg1 := e.expect(t, 200, "GET", "/databases/vrdb/ledger/verifications?limit=1", nil)
	if _, err := sc.Exec(context.Background(), "select kingres.ledger_verify_recorded()"); err != nil {
		t.Fatal(err)
	}
	pg2 := e.expect(t, 200, "GET", "/databases/vrdb/ledger/verifications?limit=1&cursor="+pg1["nextCursor"].(string), nil)
	if pg2["items"].([]any)[0].(map[string]any)["id"] != second["id"] {
		t.Errorf("a new run disturbed the page position: %v", pg2)
	}
	if fresh := e.expect(t, 200, "GET", "/databases/vrdb/ledger/verifications?limit=1", nil); fresh["items"].([]any)[0].(map[string]any)["id"].(float64) <= first["id"].(float64) {
		t.Errorf("the new run is not first on a fresh page: %v", fresh)
	}

	// Bad parameters and cursors (also a cursor of another list).
	for _, q := range []string{"limit=0", "limit=501", "limit=x", "cursor=%25%25", "cursor=" + "azE", "unknown=1"} {
		if st, o, _ := e.call("GET", "/databases/vrdb/ledger/verifications?"+q, nil, nil); st != 400 {
			t.Errorf("%s: %d %v", q, st, o)
		}
	}
	// Access: kingres_readonly reads, kingres_stats does not.
	ro := e.accountWithRole(t, "vr_ro", "kingres_readonly")
	if st, _, _ := e.as(ro).call("GET", "/databases/vrdb/ledger/verifications", nil, nil); st != 200 {
		t.Errorf("readonly: %d", st)
	}
	st := e.accountWithRole(t, "vr_stats", "kingres_stats")
	if s, _, _ := e.as(st).call("GET", "/databases/vrdb/ledger/verifications", nil, nil); s != 403 {
		t.Errorf("stats: %d", s)
	}
	e.expect(t, 204, "DELETE", "/roles/vr_ro", nil)
	e.expect(t, 204, "DELETE", "/roles/vr_stats", nil)
	e.mustTask(t, "DELETE", "/databases/vrdb", nil)
	e.mustTask(t, "DELETE", "/databases/vrdb2", nil)
}
