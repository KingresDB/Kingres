// SPDX-License-Identifier: AGPL-3.0-only

package cli

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/KingresDB/Kingres/kingres/verifier/internal/aggregate"
	"github.com/KingresDB/Kingres/kingres/verifier/internal/testdir"
)

// bundleJSON writes bundle.json for the world (the index; the verifier must not believe it).
func (w *world) bundleJSON(proofs ...map[string]string) {
	w.t.Helper()
	var logs []map[string]any
	for _, c := range w.chains {
		size := countTiles(w.logDir("c/" + c.idHex + "/e1"))
		logs = append(logs, map[string]any{"kind": "chain", "origin": w.prefix + "/c/" + c.idHex + "/e1", "chain_id": c.idHex, "epoch": 1, "checkpoint": "c/" + c.idHex + "/e1/checkpoint", "size": size})
	}
	if len(w.leaves) > 0 {
		logs = append(logs, map[string]any{"kind": "aggregate", "origin": w.prefix + "/a/" + w.aggID, "checkpoint": "a/" + w.aggID + "/checkpoint", "size": len(w.leaves)})
	}
	doc := map[string]any{"format": 1, "created_at": "2026-11-19T10:00:00Z", "cluster_id": "00ff", "origin_prefix": w.prefix, "logs": logs, "anchors": []string{}, "proofs": append([]map[string]string{}, proofs...)}
	b, _ := json.Marshal(doc)
	w.put("bundle.json", b)
}

// countTiles returns the size of the log from its checkpoint (the second line).
func countTiles(dir string) int {
	b, err := os.ReadFile(filepath.Join(dir, "checkpoint"))
	if err != nil {
		return 0
	}
	var n int
	fmt.Sscanf(strings.Split(string(b), "\n")[1], "%d", &n)
	return n
}

func (w *world) bundleArgs(more ...string) []string {
	return append([]string{"bundle", "verify", "--trust", w.trust, "--now", "2026-11-19T12:00:00Z"}, append(more, w.dir)...)
}

func TestBundleVerifyAcceptsAWholeBundleAndItsTarball(t *testing.T) {
	w, _, _ := stdWorld(t)
	w.bundleJSON()
	out := wantRun(t, 0, w.bundleArgs(), "VERIFIED:", "2 chain log(s), 1 aggregate log(s)", "bundle-index")
	if !strings.Contains(out, "not proved: ") {
		t.Fatal(out)
	}
	tgz := filepath.Join(testdir.New(t), "bundle.tar.gz")
	writeTarGz(t, w.dir, tgz)
	args := append([]string{"bundle", "verify", "--trust", w.trust, "--now", "2026-11-19T12:00:00Z"}, tgz)
	wantRun(t, 0, args, "VERIFIED:")
}

func writeTarGz(t *testing.T, dir, out string) {
	t.Helper()
	f, err := os.Create(out)
	if err != nil {
		t.Fatal(err)
	}
	zw := gzip.NewWriter(f)
	tw := tar.NewWriter(zw)
	var names []string
	_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			names = append(names, p)
		}
		return nil
	})
	sort.Strings(names)
	for _, p := range names {
		rel, _ := filepath.Rel(dir, p)
		b, _ := os.ReadFile(p)
		if err := tw.WriteHeader(&tar.Header{Name: "./" + filepath.ToSlash(rel), Mode: 0o644, Size: int64(len(b)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		_, _ = tw.Write(b)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

// Every byte of every file that carries a hash or a signature is mutated, one at a time, and the bundle
// is never verified: the checkpoint notes, the tiles, the entry bundles, of the chains and of the
// aggregate (design 10, step 7: "mutation tests (every byte of a tile, a note, a leaf) rejected").
func TestBundleVerifyRejectsEveryByteOfEveryFile(t *testing.T) {
	w := newWorld(t)
	a, b := makeChain(0xa1, 3, nil), makeChain(0xb2, 2, nil)
	w.addChain(a, 3)
	w.addChain(b, 2)
	w.appendRound(10_000_000, member(a, 2, 0), member(b, 2, 0))
	w.appendRound(20_000_000, member(a, 3, 0), member(b, 2, 0))
	// the events are part of the bundle: every byte of them is mutated too (V8-02: 825 positions of the event files were accepted)
	w.event(0, "key-added", []signer{w.s1}, keyAddedLines(w.s1)...)
	w.event(1, "key-added", []signer{w.s1, w.s2}, keyAddedLines(w.s2)...)
	w.event(2, "key-retired", []signer{w.s2}, "key: "+b64pub(w.s1), "max-tree-size: unbounded")
	w.bundleJSON()
	var files []string
	_ = filepath.WalkDir(w.dir, func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() && filepath.Base(p) != "bundle.json" {
			rel, _ := filepath.Rel(w.dir, p)
			files = append(files, rel)
		}
		return nil
	})
	if len(files) < 10 {
		t.Fatalf("only %d files", len(files))
	}
	// workers each have a copy of the bundle: the mutation of one byte is made in the copy and undone
	const workers = 4
	var wg sync.WaitGroup
	var total atomic.Int64
	fail := make(chan string, workers)
	for k := 0; k < workers; k++ {
		dir := testdir.New(t)
		copyDir(t, w.dir, dir)
		wg.Add(1)
		go func(k int, dir string) {
			defer wg.Done()
			args := []string{"bundle", "verify", "--trust", w.trust, "--now", "2026-11-19T12:00:00Z", dir}
			n := 0
			for _, rel := range files {
				p := filepath.Join(dir, rel)
				orig, _ := os.ReadFile(p)
				for i := range orig {
					if !sweepPosition(i, len(orig)) {
						continue
					}
					if n++; n%workers != k {
						continue
					}
					mut := append([]byte{}, orig...)
					mut[i] ^= 0x01
					_ = os.WriteFile(p, mut, 0o644)
					var out, errb bytes.Buffer
					code := Run(args, &out, &errb, "test")
					total.Add(1)
					if code == 0 || strings.Contains(out.String(), "VERIFIED:") {
						fail <- fmt.Sprintf("byte %d of %s flipped: status %d: %s", i, rel, code, out.String())
						return
					}
				}
				_ = os.WriteFile(p, orig, 0o644)
			}
		}(k, dir)
	}
	wg.Wait()
	close(fail)
	if msg, ok := <-fail; ok {
		t.Fatal(msg)
	}
	wantRun(t, 0, w.bundleArgs(), "VERIFIED:")
	t.Logf("%d single-byte mutations of %d files, all refused", total.Load(), len(files))
}

func copyDir(t *testing.T, from, to string) {
	t.Helper()
	err := filepath.WalkDir(from, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(from, p)
		if d.IsDir() {
			return os.MkdirAll(filepath.Join(to, rel), 0o755)
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(to, rel), b, 0o644)
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestBundleVerifyRefusesALyingIndexAndAMissingOne(t *testing.T) {
	w, _, _ := stdWorld(t)
	w.bundleJSON()
	// another size than the signed checkpoint says
	b, _ := os.ReadFile(filepath.Join(w.dir, "bundle.json"))
	w.put("bundle.json", []byte(strings.Replace(string(b), `"size":5`, `"size":4`, 1)))
	wantRun(t, 1, w.bundleArgs(), "FAILED at step bundle-index", "size 4")
	// another prefix
	w.bundleJSON()
	b, _ = os.ReadFile(filepath.Join(w.dir, "bundle.json"))
	w.put("bundle.json", []byte(strings.ReplaceAll(string(b), testPrefix, "other.example.com/x")))
	wantRun(t, 1, w.bundleArgs(), "FAILED at step bundle-index")
	// a path that leaves the bundle
	w.bundleJSON(map[string]string{"file": "../../etc/passwd"})
	wantRun(t, 1, w.bundleArgs(), "FAILED at step bundle-index", "..")
	// no index at all
	_ = os.Remove(filepath.Join(w.dir, "bundle.json"))
	wantRun(t, 2, w.bundleArgs(), "not a bundle")
	// a log that the layout holds and the index does not name is verified anyway
	w.bundleJSON()
	b, _ = os.ReadFile(filepath.Join(w.dir, "bundle.json"))
	var doc map[string]any
	_ = json.Unmarshal(b, &doc)
	doc["logs"] = doc["logs"].([]any)[:1]
	b, _ = json.Marshal(doc)
	w.put("bundle.json", b)
	wantRun(t, 0, w.bundleArgs(), "is in the layout and not in bundle.json")
}

func TestBundleVerifyWithoutAnchorsSaysSoAndASillyIndexPathIsNotRead(t *testing.T) {
	w, _, _ := stdWorld(t)
	w.bundleJSON()
	wantRun(t, 3, w.bundleArgs("--require-anchors", "1"), "INCOMPLETE", "--require-anchors 1: 0 anchor(s) verified")
	b, _ := os.ReadFile(filepath.Join(w.dir, "bundle.json"))
	w.put("bundle.json", []byte(strings.Replace(string(b), `"anchors":[]`, `"anchors":["a/x/anchors/2/1.json"]`, 1)))
	wantRun(t, 3, w.bundleArgs(), "INCOMPLETE", "1 anchor file(s)", "is not a path of the layout")
}

func TestBundleVerifyChecksProofsOnBothLegsAndRefusesATamperedOne(t *testing.T) {
	w, a, _ := stdWorld(t)
	w.put("proofs/p1.json", a.proofJSON(3, nil, 0))
	w.bundleJSON(map[string]string{"file": "proofs/p1.json"})
	wantRun(t, 0, w.bundleArgs(), "VERIFIED:", "proof 1 log-inclusion", "proof 1 aggregate-leg")
	// every byte of the proof, on the cheaper command that runs the same legs
	orig := a.proofJSON(3, nil, 0)
	for i := range orig {
		mut := append([]byte{}, orig...)
		mut[i] ^= 0x01
		w.put("proofs/p1.json", mut)
		if code, out, _ := run(t, "proof", "--proof", filepath.Join(w.dir, "proofs", "p1.json"), "--bundle", w.dir, "--trust", w.trust, "--log-leg", "aggregate", "--now", "2026-11-19T12:00:00Z"); code == 0 {
			t.Fatalf("byte %d of the proof flipped: status 0\n%s", i, out)
		}
	}
	// a block beyond the signed chain log
	c := makeChain(0xa1, 9, nil)
	w.put("proofs/p1.json", c.proofJSON(7, nil, 0))
	wantRun(t, 1, w.bundleArgs(), "log-inclusion")
	// a missing proof file is "could not check"
	_ = os.Remove(filepath.Join(w.dir, "proofs", "p1.json"))
	wantRun(t, 3, w.bundleArgs(), "INCOMPLETE")
}

func TestProofLegAgainstTheChainAndTheAggregate(t *testing.T) {
	w, a, b := stdWorld(t)
	pf := filepath.Join(testdir.New(t), "p.json")
	if err := os.WriteFile(pf, a.proofJSON(3, nil, 0), 0o600); err != nil {
		t.Fatal(err)
	}
	base := []string{"proof", "--proof", pf, "--bundle", w.dir, "--trust", w.trust, "--now", "2026-11-19T12:00:00Z"}
	wantRun(t, 0, append(base, "--log-leg", "chain"), "VERIFIED:", "signature of key", "log-inclusion")
	wantRun(t, 0, append(base, "--log-leg", "aggregate"), "VERIFIED:", "aggregate-leg", "no anchor was checked")
	wantRun(t, 0, append(base, "--log-leg", "chain", "--table-id", "7", "--row-hash", fmt.Sprintf("%x", a.rows[3]), "--op", "I"), "VERIFIED:", "binding")
	wantRun(t, 1, append(base, "--log-leg", "chain", "--table-id", "8"), "FAILED at step binding")
	wantRun(t, 2, append(base, "--log-leg", "nothing"), "--log-leg")
	wantRun(t, 2, append(base, "--log-leg", "chain", "--digest", fx("digests/d0.json")), "two ways")
	// the aggregate names the head of b at 3 blocks: a proof of a block of b is shown through it
	pb := filepath.Join(testdir.New(t), "pb.json")
	_ = os.WriteFile(pb, b.proofJSON(2, nil, 0), 0o600)
	wantRun(t, 0, []string{"proof", "--proof", pb, "--bundle", w.dir, "--trust", w.trust, "--log-leg", "aggregate", "--now", "2026-11-19T12:00:00Z"}, "VERIFIED:")
	// an aggregate round that does not cover the block (the round names 3 blocks of a)
	w2 := newWorld(t)
	w2.addChain(a, 5)
	w2.appendRound(10_000_000, member(a, 3, 0))
	wantRun(t, 1, []string{"proof", "--proof", pf, "--bundle", w2.dir, "--trust", w2.trust, "--log-leg", "aggregate", "--now", "2026-11-19T12:00:00Z"}, "FAILED at step aggregate-leg")
	_ = aggregate.Tag
}

// A last block rewritten consistently in its tile and its entry bundle, with the signed checkpoint left as it was:
// the leaf hash of the entry is the hash of the tile, nothing links to the block, and only the root of the tiles against the
// signed root shows it.
func TestBundleVerifyRefusesALastBlockRewrittenInTheTileAndTheBundle(t *testing.T) {
	w := newWorld(t)
	a := makeChain(0xa1, 5, nil)
	w.addChain(a, 5)
	w.bundleJSON()
	alt := makeChain(0xa1, 5, map[int][]extraEntry{4: {{table: 9, op: 0x49, row: rowHashOf(1, 1)}}})
	rel := "c/" + a.idHex + "/e1"
	cp, _ := os.ReadFile(w.logDir(rel + "/checkpoint"))
	writeTiles(t, w.logDir(rel), alt.leaves, 5)
	writeEntries(t, w.logDir(rel), alt.entries, 5)
	w.put(rel+"/checkpoint", cp)
	wantRun(t, 1, w.bundleArgs(), "FAILED at step tiles", "do not belong to the checkpoint")
}

// A proof whose block is well formed, with a valid path to its own header, of another history of the same chain.
func TestProofLegRefusesABlockOfAnotherHistoryOfTheChain(t *testing.T) {
	w, a, _ := stdWorld(t)
	alt := makeChain(0xa1, 5, map[int][]extraEntry{3: {{table: 9, op: 0x49, row: rowHashOf(1, 1)}}})
	pf := filepath.Join(testdir.New(t), "p.json")
	if err := os.WriteFile(pf, alt.proofJSON(3, []extraEntry{{table: 9, op: 0x49, row: rowHashOf(1, 1)}}, 0), 0o600); err != nil {
		t.Fatal(err)
	}
	_ = a
	wantRun(t, 1, []string{"proof", "--proof", pf, "--bundle", w.dir, "--trust", w.trust, "--log-leg", "chain", "--now", "2026-11-19T12:00:00Z"}, "FAILED at step log-inclusion")
}

// The last block of a full tile rewritten in the tile and the entry bundle, the tile of the next level and the
// checkpoint left alone: the leaf hashes and the links agree, the root of the tiles agrees (it is read from the
// level above), and only the check that a full tile hashes to the entry of the next level shows it.
func TestBundleVerifyRefusesAFullTileThatDoesNotHashToTheLevelAbove(t *testing.T) {
	w := newWorld(t)
	a := makeChain(0xa1, 512, nil)
	w.addChain(a, 512)
	w.bundleJSON()
	wantRun(t, 0, w.bundleArgs(), "VERIFIED:")
	alt := makeChain(0xa1, 512, map[int][]extraEntry{511: {{table: 9, op: 0x49, row: rowHashOf(1, 1)}}})
	rel := "c/" + a.idHex + "/e1"
	var tile []byte
	for _, h := range alt.leaves[256:] {
		tile = append(tile, h[:]...)
	}
	w.put(rel+"/tile/0/001", tile)
	w.put(rel+"/tile/entries/001", entryBundle(alt.entries[256:]))
	wantRun(t, 1, w.bundleArgs(), "FAILED at step tiles")
}

// An entry that is a well formed block, not the one the tile has the hash of.
func TestBundleVerifyRefusesAnEntryThatIsNotWhatTheTileHashes(t *testing.T) {
	w := newWorld(t)
	a := makeChain(0xa1, 5, nil)
	w.addChain(a, 5)
	w.bundleJSON()
	alt := makeChain(0xa1, 5, map[int][]extraEntry{4: {{table: 9, op: 0x49, row: rowHashOf(1, 1)}}})
	rel := "c/" + a.idHex + "/e1"
	w.put(rel+"/tile/entries/000.p/5", entryBundle(alt.entries))
	wantRun(t, 1, w.bundleArgs(), "FAILED at step")
}

// What a publisher writes never reaches the terminal raw: the notes of bundle.json and a name from a bundle
// pass through the one function of the output layer.
func TestBundleTextFromThePublisherIsEscaped(t *testing.T) {
	w, _, _ := stdWorld(t)
	w.bundleJSON()
	b, _ := os.ReadFile(filepath.Join(w.dir, "bundle.json"))
	w.put("bundle.json", []byte(strings.Replace(string(b), `"proofs":[]`, `"notes":"\u001b[31mred‮\nVERIFIED: forged","proofs":[]`, 1)))
	all := wantRun(t, 0, w.bundleArgs(), "VERIFIED:")
	if strings.Contains(all, "\x1b") || strings.Contains(all, "‮") {
		t.Fatalf("a control character reached the output: %q", all)
	}
	for _, l := range strings.Split(all, "\n") {
		if strings.HasPrefix(l, "VERIFIED: forged") {
			t.Fatalf("a line was forged: %q", l)
		}
	}
	if !strings.Contains(all, `\x1b[31mred`) {
		t.Fatalf("the notes are not shown escaped: %s", all)
	}
}
