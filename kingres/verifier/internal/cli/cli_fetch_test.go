// SPDX-License-Identifier: AGPL-3.0-only

package cli

import (
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/KingresDB/Kingres/kingres/verifier/internal/testdir"
)

func fetchWorld(t *testing.T) (*world, *chainFix, *httptest.Server) {
	w := newWorld(t)
	a := makeChain(0xa1, 300, nil) // a full tile and a partial one
	w.addChain(a, 300)
	w.appendRound(10_000_000, member(a, 300, 0))
	w.event(0, "key-added", []signer{w.s1}, keyAddedLines(w.s1)...)
	w.put("keyring.json", []byte(`{"format":1}`))
	return w, a, serve(t, w)
}

func filesOf(dir string) map[string]bool {
	out := map[string]bool{}
	_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			rel, _ := filepath.Rel(dir, p)
			out[filepath.ToSlash(rel)] = true
		}
		return nil
	})
	return out
}

func TestBundleFetchCopiesTheLogsAndTheCopyVerifies(t *testing.T) {
	w, a, srv := fetchWorld(t)
	out := filepath.Join(testdir.New(t), "mirror")
	args := []string{"bundle", "fetch", "--source", srv.URL, "--out", out, "--trust", w.trust, "--now", "2026-11-19T12:00:00Z",
		"--log", "chain:" + a.idHex + ":e1", "--log", "aggregate:" + w.aggID}
	wantRun(t, 0, args, "FETCHED:", "the tiles copied lead to the signed root", "events")
	got := filesOf(out)
	for _, want := range []string{"keyring.json", "events/000000", "c/" + a.idHex + "/e1/checkpoint", "c/" + a.idHex + "/e1/tile/0/000", "c/" + a.idHex + "/e1/tile/0/001.p/44",
		"c/" + a.idHex + "/e1/tile/entries/000", "a/" + w.aggID + "/checkpoint"} {
		if !got[want] {
			t.Errorf("%s was not copied (%d files)", want, len(got))
		}
	}
	for f := range got {
		if strings.HasSuffix(f, ".part") || strings.Contains(f, "..") {
			t.Errorf("an odd file: %s", f)
		}
	}
	// the copy verifies as a publication (a bundle.json is what a bundle adds)
	m := &world{t: t, dir: out, prefix: w.prefix, trust: w.trust, chains: []*chainFix{a}, aggID: w.aggID, leaves: w.leaves}
	m.bundleJSON()
	wantRun(t, 0, m.bundleArgs(), "VERIFIED:")
	// without the trust file nothing is verified and the output says so
	out2 := filepath.Join(testdir.New(t), "mirror2")
	wantRun(t, 0, []string{"bundle", "fetch", "--source", srv.URL, "--out", out2, "--log", "chain:" + a.idHex + ":e1"}, "FETCHED:", "NOT verified", "no --trust was given")
	// again over a copy that is there: nothing breaks
	wantRun(t, 0, args, "FETCHED:")
}

func TestBundleFetchSinceSizeSkipsTilesThatEndBeforeIt(t *testing.T) {
	w, a, srv := fetchWorld(t)
	out := filepath.Join(testdir.New(t), "mirror")
	wantRun(t, 0, []string{"bundle", "fetch", "--source", srv.URL, "--out", out, "--since-size", "256", "--log", "chain:" + a.idHex + ":e1"}, "FETCHED:")
	got := filesOf(out)
	rel := "c/" + a.idHex + "/e1/"
	if got[rel+"tile/0/000"] || got[rel+"tile/entries/000"] {
		t.Error("the full tile and bundle that end at leaf 256 were fetched again")
	}
	if !got[rel+"tile/0/001.p/44"] || !got[rel+"tile/entries/001.p/44"] || !got[rel+"checkpoint"] {
		t.Errorf("what is new was not fetched: %v", got)
	}
	_ = w
}

func TestBundleFetchRefusesWhatItCannotTrust(t *testing.T) {
	w, a, srv := fetchWorld(t)
	out := filepath.Join(testdir.New(t), "mirror")
	log := "chain:" + a.idHex + ":e1"
	// a trust file that pins another key: the checkpoint does not verify
	other := w.writeTrust(newSigner(9))
	wantRun(t, 1, []string{"bundle", "fetch", "--source", srv.URL, "--out", out, "--trust", other, "--now", "2026-11-19T12:00:00Z", "--log", log}, "FAILED at step signature")
	// a tile that does not belong to the checkpoint
	w.put("c/"+a.idHex+"/e1/tile/0/000", make([]byte, 8192))
	wantRun(t, 1, []string{"bundle", "fetch", "--source", srv.URL, "--out", filepath.Join(testdir.New(t), "m2"), "--trust", w.trust, "--now", "2026-11-19T12:00:00Z", "--log", log}, "FAILED at step")
	// a server that redirects, one that is down, one that has no such log
	moved := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) { http.Redirect(rw, r, "http://evil.example/", 302) }))
	defer moved.Close()
	wantRun(t, 2, []string{"bundle", "fetch", "--source", moved.URL, "--out", filepath.Join(testdir.New(t), "m3"), "--log", log}, "redirect")
	empty := httptest.NewServer(http.NotFoundHandler())
	defer empty.Close()
	wantRun(t, 3, []string{"bundle", "fetch", "--source", empty.URL, "--out", filepath.Join(testdir.New(t), "m4"), "--log", log}, "INCOMPLETE")
	// usage
	wantRun(t, 2, []string{"bundle", "fetch", "--source", srv.URL, "--out", out}, "--log")
	wantRun(t, 2, []string{"bundle", "fetch", "--source", srv.URL, "--out", out, "--log", "chain:" + a.idHex}, "give the epoch")
	// nothing is written outside the output directory
	parent := filepath.Dir(out)
	before := len(filesOf(parent))
	wantRun(t, 0, []string{"bundle", "fetch", "--source", srv.URL, "--out", filepath.Join(parent, "m5"), "--log", log}, "FETCHED:")
	for f := range filesOf(parent) {
		if !strings.HasPrefix(f, "m5/") && !strings.HasPrefix(f, "mirror/") && !strings.HasPrefix(f, "m") {
			t.Errorf("a file outside the output directories: %s", f)
		}
	}
	_ = before
	_ = os.Stdout
}

func TestAggregateReadsChainLogsFromAnotherSource(t *testing.T) {
	w, a, _ := stdWorld(t)
	chains := testdir.New(t)
	if err := os.Rename(filepath.Join(w.dir, "c"), filepath.Join(chains, "c")); err != nil {
		t.Fatal(err)
	}
	wantRun(t, 3, w.aggArgs(), "INCOMPLETE")
	wantRun(t, 0, w.aggArgs("--chains-from", chains), "VERIFIED:")
	_ = a
}
