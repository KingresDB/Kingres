// SPDX-License-Identifier: AGPL-3.0-only

package cli

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/KingresDB/Kingres/kingres/verifier/internal/format"
	"github.com/KingresDB/Kingres/kingres/verifier/internal/testdir"
	"github.com/KingresDB/Kingres/kingres/verifier/internal/trust"
)

func fetchArgs(w *world, srv *httptest.Server, out string, more ...string) []string {
	return append([]string{"bundle", "fetch", "--source", srv.URL, "--out", out, "--now", nowArg19}, more...)
}

// dirBytes is the number of files and bytes below dir.
func dirBytes(t *testing.T, dir string) (files int, size int64) {
	t.Helper()
	_ = filepath.Walk(dir, func(p string, fi os.FileInfo, err error) error {
		if err == nil && !fi.IsDir() {
			files++
			size += fi.Size()
		}
		return nil
	})
	return
}

// V8-03: without --trust, what an unsigned checkpoint claims never drives unbounded writes.
func TestV803AnUnsignedCheckpointOfAHugeSizeDoesNotDriveWrites(t *testing.T) {
	chain := makeChain(0xa1, 1, nil)
	origin := testPrefix + "/c/" + chain.idHex + "/e1"
	var requests, tileRequests atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		p := strings.TrimPrefix(r.URL.Path, "/")
		switch {
		case p == "c/"+chain.idHex+"/e1/checkpoint":
			root := sha([]byte("a root"))
			_, _ = rw.Write([]byte(signText(cpText(origin, 1_000_000_000_000, root), origin, newSigner(77))))
		case strings.Contains(p, "/tile/"):
			tileRequests.Add(1)
			_, _ = rw.Write(make([]byte, 8192))
		default:
			http.NotFound(rw, r)
		}
	}))
	defer srv.Close()
	out := filepath.Join(testdir.New(t), "mirror")
	wantRun(t, 2, []string{"bundle", "fetch", "--source", srv.URL, "--out", out, "--log", "chain:" + chain.idHex + ":e1"}, "--max-files", "needs", "nothing was asked for")
	if tileRequests.Load() != 0 {
		t.Fatalf("%d tile request(s) were made for a size that is over the ceiling", tileRequests.Load())
	}
	files, _ := dirBytes(t, out)
	if files > 1 {
		t.Fatalf("%d files were written", files)
	}
	// a smaller claim that fits the file ceiling but not the byte ceiling stops at the byte ceiling
	srv2 := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		p := strings.TrimPrefix(r.URL.Path, "/")
		switch {
		case p == "c/"+chain.idHex+"/e1/checkpoint":
			root := sha([]byte("a root"))
			_, _ = rw.Write([]byte(signText(cpText(origin, 3000, root), origin, newSigner(77))))
		case strings.Contains(p, "/tile/"):
			if strings.Contains(p, "entries") {
				_, _ = rw.Write(make([]byte, 256*89))
			} else {
				_, _ = rw.Write(make([]byte, 8192))
			}
		default:
			http.NotFound(rw, r)
		}
	}))
	defer srv2.Close()
	out2 := filepath.Join(testdir.New(t), "mirror")
	wantRun(t, 2, []string{"bundle", "fetch", "--source", srv2.URL, "--out", out2, "--log", "chain:" + chain.idHex + ":e1", "--max-bytes", "20000"}, "--max-bytes")
	_, size := dirBytes(t, out2)
	if size > 20000 {
		t.Fatalf("%d bytes were written with a ceiling of 20000", size)
	}
	// the ceilings are stated in the output, and can be raised
	wantRun(t, 2, []string{"bundle", "fetch", "--source", srv2.URL, "--out", filepath.Join(testdir.New(t), "m"), "--log", "chain:" + chain.idHex + ":e1", "--max-files", "3"}, "ceiling")
}

// V8-03: with --trust a verified checkpoint is bounded too, and the ceilings are in the output.
func TestV803TheCeilingsAreStatedAndApplyWithATrustFile(t *testing.T) {
	w, a, _ := stdWorld(t)
	srv := serve(t, w)
	out := filepath.Join(testdir.New(t), "mirror")
	text := wantRun(t, 0, fetchArgs(w, srv, out, "--trust", w.trust, "--log", "chain:"+a.idHex+":e1"), "FETCHED:", "ceilings of this run: at most 100000 file(s)")
	_ = text
	out2 := filepath.Join(testdir.New(t), "mirror")
	wantRun(t, 2, fetchArgs(w, srv, out2, "--trust", w.trust, "--log", "chain:"+a.idHex+":e1", "--max-files", "1"), "--max-files")
}

// V8-06: what the output directory holds is compared before it is replaced.
func TestV806FetchComparesWhatItHoldsAndNeverReplacesSilently(t *testing.T) {
	w, a, _ := stdWorld(t)
	srv := serve(t, w)
	logArg := []string{"--trust", w.trust, "--log", "chain:" + a.idHex + ":e1"}
	rel := "c/" + a.idHex + "/e1/checkpoint"

	// the first fetch, then a rollback
	out := filepath.Join(testdir.New(t), "mirror")
	wantRun(t, 0, fetchArgs(w, srv, out, logArg...), "FETCHED:")
	held, _ := os.ReadFile(filepath.Join(out, filepath.FromSlash(rel)))
	w.publishChain(a, 3)
	wantRun(t, 1, fetchArgs(w, srv, out, logArg...), "FAILED at step rollback", "size 3", "size 5")
	after, _ := os.ReadFile(filepath.Join(out, filepath.FromSlash(rel)))
	if !bytes.Equal(held, after) {
		t.Fatal("the held checkpoint was replaced by a smaller one")
	}
	if _, err := os.Stat(filepath.Join(out, filepath.FromSlash(rel)+".conflict")); err != nil {
		t.Fatalf("the conflicting note was not saved: %v", err)
	}
	// without --trust the same rule holds
	wantRun(t, 1, fetchArgs(w, srv, out, "--log", "chain:"+a.idHex+":e1"), "FAILED at step rollback")

	// a fork: the same size and another root
	w.publishChain(a, 5)
	other := makeChainTx(0xa1, 5, nil, func(i int) uint64 { return uint64(900 + i) })
	w.publishChain(other, 5)
	wantRun(t, 1, fetchArgs(w, srv, out, logArg...), "FAILED at step fork", "two histories")
	after, _ = os.ReadFile(filepath.Join(out, filepath.FromSlash(rel)))
	if !bytes.Equal(held, after) {
		t.Fatal("the held checkpoint was replaced by a fork")
	}

	// an extension of the held checkpoint: proved from the tiles of the copy
	w.publishChain(a, 5)
	out2 := filepath.Join(testdir.New(t), "mirror")
	w.publishChain(a, 3)
	wantRun(t, 0, fetchArgs(w, srv, out2, logArg...), "FETCHED:")
	w.publishChain(a, 5)
	wantRun(t, 0, fetchArgs(w, srv, out2, logArg...), "FETCHED:", "extends")

	// a larger checkpoint of another history: the tiles bind to it, but it does not extend the held one
	out3 := filepath.Join(testdir.New(t), "mirror")
	w.publishChain(a, 3)
	wantRun(t, 0, fetchArgs(w, srv, out3, logArg...), "FETCHED:")
	heldSmall, _ := os.ReadFile(filepath.Join(out3, filepath.FromSlash(rel)))
	w.publishChain(other, 5)
	wantRun(t, 1, fetchArgs(w, srv, out3, logArg...), "FAILED at step fork", "does not extend the copy you hold")
	after, _ = os.ReadFile(filepath.Join(out3, filepath.FromSlash(rel)))
	if !bytes.Equal(heldSmall, after) {
		t.Fatal("the held checkpoint was not put back")
	}
	// without --trust the extension is not checked, and the output says so
	out4 := filepath.Join(testdir.New(t), "mirror")
	w.publishChain(a, 3)
	wantRun(t, 0, fetchArgs(w, srv, out4, "--log", "chain:"+a.idHex+":e1"), "FETCHED:")
	w.publishChain(other, 5)
	wantRun(t, 0, fetchArgs(w, srv, out4, "--log", "chain:"+a.idHex+":e1"), "FETCHED:", "that it extends the held one was not checked")
}

// V8-06: an event whose text is not the held one, and events the source no longer serves, are findings.
func TestV806FetchComparesTheEvents(t *testing.T) {
	w, a, _ := stdWorld(t)
	w.event(0, "key-added", []signer{w.s1}, keyAddedLines(w.s1)...)
	w.event(1, "key-added", []signer{w.s1, w.s2}, keyAddedLines(w.s2)...)
	srv := serve(t, w)
	logArg := []string{"--trust", w.trust, "--log", "chain:" + a.idHex + ":e1"}
	out := filepath.Join(testdir.New(t), "mirror")
	wantRun(t, 0, fetchArgs(w, srv, out, logArg...), "FETCHED:", "2 event(s) copied")
	held, _ := os.ReadFile(filepath.Join(out, "events", "000001"))
	// an event gone
	if err := os.Remove(filepath.Join(w.dir, "events", "000001")); err != nil {
		t.Fatal(err)
	}
	wantRun(t, 1, fetchArgs(w, srv, out, logArg...), "FAILED at step events", "does not serve it any more")
	// an event with another text
	w.event(1, "key-added", []signer{w.s1, w.s2}, keyAddedLines(newSigner(5))...)
	wantRun(t, 1, fetchArgs(w, srv, out, logArg...), "FAILED at step events", "its text changed")
	after, _ := os.ReadFile(filepath.Join(out, "events", "000001"))
	if !bytes.Equal(held, after) {
		t.Fatal("the held event was replaced")
	}
}

// V8-06: a tile that is here with the size it must have is kept without being read, and counted as kept.
func TestV806KeptTilesAreNotCountedAsFetched(t *testing.T) {
	w, a, _ := stdWorld(t)
	srv := serve(t, w)
	out := filepath.Join(testdir.New(t), "mirror")
	args := fetchArgs(w, srv, out, "--log", "chain:"+a.idHex+":e1")
	wantRun(t, 0, args, "FETCHED:", "NOT verified")
	// the full tiles of a log of 5 entries: none; use more blocks so that a full tile exists
	big := makeChain(0xb7, 300, nil)
	w.addChain(big, 300)
	args = fetchArgs(w, srv, out, "--log", "chain:"+big.idHex+":e1")
	wantRun(t, 0, args, "FETCHED:")
	second := wantRun(t, 0, args, "FETCHED:", "were kept without being read", "the tiles that were kept were not compared with anything")
	_ = second
}

// logs.json: the format of the current publisher is read, another format is refused, and bundle fetch finds the logs by it.
func writeLogsJSON(w *world, state string) {
	var parts []string
	for _, c := range w.chains {
		parts = append(parts, fmt.Sprintf(`{"kind":"chain","origin":"%s/c/%s/e1","state":"%s","size":%d}`, w.prefix, c.idHex, state, 5))
	}
	parts = append(parts, fmt.Sprintf(`{"kind":"aggregate","origin":"%s/a/%s","state":"active","size":%d}`, w.prefix, w.aggID, len(w.leaves)))
	w.put("logs.json", []byte(`{"format":1,"logs":[`+strings.Join(parts, ",")+"]}\n"))
}

func TestLogsJSONFindsTheLogsOfAPublication(t *testing.T) {
	w, a, b := stdWorld(t)
	writeLogsJSON(w, "active")
	srv := serve(t, w)
	out := filepath.Join(testdir.New(t), "mirror")
	text := wantRun(t, 0, fetchArgs(w, srv, out, "--trust", w.trust), "FETCHED:", "logs.json (format 1) lists 3 log(s)", "log chain:"+a.idHex[:8], "log chain:"+b.idHex[:8], "log aggregate:"+w.aggID[:8])
	_ = text
	if _, err := os.Stat(filepath.Join(out, "logs.json")); err != nil {
		t.Fatal(err)
	}
	// a log that the publisher lists as diverged is said in a note
	writeLogsJSON(w, "diverged")
	wantRun(t, 0, fetchArgs(w, srv, filepath.Join(testdir.New(t), "m")), "FETCHED:", "as diverged")
	// another prefix than the one pinned
	w.put("logs.json", []byte(strings.ReplaceAll(readFile(t, filepath.Join(w.dir, "logs.json")), w.prefix, "other.example.com/x")))
	wantRun(t, 1, fetchArgs(w, srv, filepath.Join(testdir.New(t), "m"), "--trust", w.trust), "FAILED at step logs-json", "another cluster")
	// an unknown format is refused, not guessed
	w.put("logs.json", []byte(`{"format":2,"logs":[]}`+"\n"))
	wantRun(t, 2, fetchArgs(w, srv, filepath.Join(testdir.New(t), "m")), "format 2", "does not guess")
	// no logs.json and no --log
	if err := os.Remove(filepath.Join(w.dir, "logs.json")); err != nil {
		t.Fatal(err)
	}
	wantRun(t, 2, fetchArgs(w, srv, filepath.Join(testdir.New(t), "m")), "name the logs to fetch with --log")
}

func readFile(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// V8-07: a missing or emptied monitor record is a first run only when the user says so.
func TestV807AMissingOrEmptiedMonitorRecordIsNotAFirstRun(t *testing.T) {
	w, a, st := monitorWorld(t)
	srv := serve(t, w)
	args := func(more ...string) []string {
		return append([]string{"monitor", "--trust", w.trust, "--source", srv.URL, "--state", st, "--once", "--interval", "1ms", "--now", nowArg19,
			"--log", "chain:" + a.idHex + ":e1", "--log", "aggregate:" + w.aggID}, more...)
	}
	// no record, nothing in the directory: not a first run unless it is said
	wantRun(t, 2, args(), "holds no monitor.json", "--init")
	wantRun(t, 0, args("--init"), "VERIFIED:")
	wantRun(t, 0, args(), "VERIFIED:")
	// --init on a state that exists is refused
	wantRun(t, 2, args("--init"), "--init", "already holds")
	// the record deleted: the log states remain
	rec := filepath.Join(st, "monitor.json")
	saved, _ := os.ReadFile(rec)
	if err := os.Remove(rec); err != nil {
		t.Fatal(err)
	}
	wantRun(t, 2, args(), "log state file(s) and no monitor.json", "was lost or removed")
	wantRun(t, 2, args("--init"), "not a first run")
	// the record emptied to {"format":1}
	if err := os.WriteFile(rec, []byte(`{"format":1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	wantRun(t, 2, args(), "incomplete", "emptied")
	// members that are null
	if err := os.WriteFile(rec, []byte(`{"format":1,"audited":null,"events":null,"retired":null}`), 0o600); err != nil {
		t.Fatal(err)
	}
	wantRun(t, 2, args(), "incomplete")
	// the real record back: runs again
	if err := os.WriteFile(rec, saved, 0o600); err != nil {
		t.Fatal(err)
	}
	wantRun(t, 0, args(), "VERIFIED:")
}

// V8-08, M20: the attestation row is an insert, in the transaction and in the block of the entry it attests.
type attVariant struct {
	attOp    byte
	attBlock int
	sameTx   bool
}

func newAttVariant(t *testing.T, v attVariant) *attFix {
	t.Helper()
	f := &attFix{w: newWorld(t)}
	_, f.key, _ = ed25519.GenerateKey(bytes.NewReader(bytes.Repeat([]byte{7}, 64)))
	pub := f.key.Public().(ed25519.PublicKey)
	sum := sha256.Sum256(pub)
	copy(f.keyID[:], sum[:16])
	copy(f.nonce[:], bytes.Repeat([]byte{0x5a}, 16))
	chainID := makeChain(0xc3, 1, nil).id
	row := rowHashOf(0xc3, 2)
	sig := ed25519.Sign(f.key, message(chainID, tableUser, format.OpInsert, row, f.keyID, f.nonce))
	cols := attCols(102, 0, f.keyID, f.nonce, sig)
	f.attCols = cols
	attRow := rowHashOfCols(t, tableAtt, cols)
	extra := []extraEntry{{table: tableAtt, op: v.attOp, row: attRow}}
	txid := func(i int) uint64 {
		if i == 3 && v.sameTx {
			return 102
		}
		return uint64(100 + i)
	}
	f.chain = makeChainTx(0xc3, 5, map[int][]extraEntry{v.attBlock: extra}, txid)
	f.w.addChain(f.chain, 5)
	f.w.appendRound(1_000, member(f.chain, 5, 0))
	f.w.put("p/row.json", f.chain.proofJSON(2, nil, 0))
	f.w.put("p/att.json", f.chain.proofJSON(v.attBlock, extra, 1))
	f.w.put("p/att.columns.json", cols)
	f.trustPinned = f.pinTrust(true)
	return f
}

func TestV808TheAttestationRowIsAnInsertInTheTransactionOfTheEntry(t *testing.T) {
	// the control: the attestation row in the block of the entry, an insert
	f := newAttVariant(t, attVariant{attOp: format.OpInsert, attBlock: 2, sameTx: true})
	// block 2 holds the row at entry 0 and the attestation at entry 1: proofJSON(2, nil, 0) has no extra entry, so the row's block must hold it too
	f.w.put("p/row.json", f.chain.proofJSON(2, []extraEntry{{table: tableAtt, op: format.OpInsert, row: rowHashOfCols(t, tableAtt, f.attCols)}}, 0))
	wantRun(t, 0, f.args(f.trustPinned), "VERIFIED:")
	// the entry of the attestation row is a delete
	f = newAttVariant(t, attVariant{attOp: format.OpDelete, attBlock: 2, sameTx: true})
	f.w.put("p/row.json", f.chain.proofJSON(2, []extraEntry{{table: tableAtt, op: format.OpDelete, row: rowHashOfCols(t, tableAtt, f.attCols)}}, 0))
	wantRun(t, 1, f.args(f.trustPinned), "FAILED at step attestation-row", "an attestation is an insert")
}

// M20 of the eighth review: an attestation row that is in another block of the same chain, in a transaction that has the
// number of the entry's (a forged publication; an honest chain never repeats a transaction id), is not accepted: only the block check stops it.
func TestV808TheAttestationRowMustBeInTheBlockOfItsEntry(t *testing.T) {
	f := newAttVariant(t, attVariant{attOp: format.OpInsert, attBlock: 3, sameTx: true})
	wantRun(t, 1, f.args(f.trustPinned), "FAILED at step attestation-entry", "ledger_attest runs in the transaction of the entry, so both are in one block")
}

// V8-08: the transaction of the attestation row's own entry must be the transaction of the entry it attests.
func TestV808TheAttestationRowInAnotherTransactionIsRefused(t *testing.T) {
	f := newAttVariant(t, attVariant{attOp: format.OpInsert, attBlock: 3, sameTx: false})
	wantRun(t, 1, f.args(f.trustPinned), "FAILED at step attestation-row", "in transaction 103")
}

var _ = hex.EncodeToString
var _ = base64.StdEncoding
var _ trust.Key
