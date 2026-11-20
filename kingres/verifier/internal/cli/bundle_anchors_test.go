// SPDX-License-Identifier: AGPL-3.0-only

package cli

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/KingresDB/Kingres/kingres/verifier/internal/anchortest"
)

// Step 21: bundle verify verifies the anchors of every aggregate checkpoint of the bundle, as the command anchors does.

func (aw *anchorWorld) bundleArgs(trustFile string, more ...string) []string {
	return append([]string{"bundle", "verify", "--trust", trustFile, "--now", anchorNow}, append(more, aw.dir)...)
}

func TestBundleVerifyVerifiesTheAnchorsOfTheBundle(t *testing.T) {
	aw := newAnchorWorld(t)
	d := aw.digest(2)
	tok := anchortest.NewTSA(t, anchortest.TSAOpts{Digest: d, WithName: true})
	aw.put(2, "tsa1", anchortest.TSAFile(aw.origin, 2, d, tok))
	aw.world.bundleJSON()
	tf := aw.trustWith(tok.RootPEM)
	wantRun(t, 0, aw.bundleArgs(tf, "--require-anchors", "1"), "VERIFIED:", "ok  anchor tsa tsa1", "1 anchor(s) of the aggregate checkpoint(s) of the bundle verified", "genTime 2026-11-19T11:00:00Z")
	// the same without --require-anchors: every anchor of the bundle must verify
	wantRun(t, 0, aw.bundleArgs(tf), "VERIFIED:", "anchor tsa tsa1")
	// no root of the auditor: the anchor cannot be verified, and that is not a pass, with or without --require-anchors
	wantRun(t, 3, aw.bundleArgs(aw.trust), "INCOMPLETE", "no tsa_roots")
	wantRun(t, 3, aw.bundleArgs(aw.trust, "--require-anchors", "1"), "INCOMPLETE", "--require-anchors 1: 0 anchor(s) verified", "not required, and not verified: tsa anchor tsa1")
	// the count asked for is more than the bundle holds
	wantRun(t, 3, aw.bundleArgs(tf, "--require-anchors", "2"), "INCOMPLETE", "--require-anchors 2: 1 anchor(s) verified")
}

// With --require-anchors N, an anchor that cannot be checked is a note once N verified; without it, it makes the run status 3.
func TestBundleVerifyAnAnchorWithoutItsMeansIsANoteOnlyWhenTheCountIsMet(t *testing.T) {
	easyHeaders(t)
	aw := newAnchorWorld(t)
	d := aw.digest(2)
	tok := anchortest.NewTSA(t, anchortest.TSAOpts{Digest: d})
	aw.put(2, "tsa1", anchortest.TSAFile(aw.origin, 2, d, tok))
	frag, root := anchortest.Fragment(d, []byte{1, 2, 3}, []byte("0123456789abcdef0123456789abcdef"), anchortest.BitcoinAttestation(900001))
	aw.put(2, "ots1", anchortest.OTSFile(aw.origin, 2, d, [][]byte{frag}, 900001, &root))
	aw.world.bundleJSON()
	tf := aw.trustWith(tok.RootPEM)
	wantRun(t, 3, aw.bundleArgs(tf), "INCOMPLETE", "no --btc-header")
	wantRun(t, 0, aw.bundleArgs(tf, "--require-anchors", "1"), "VERIFIED:", "not required, and not verified: ots anchor ots1")
	hdr := anchortest.Header(root, time.Date(2026, 11, 19, 12, 30, 0, 0, time.UTC), 0x207fffff)
	wantRun(t, 0, aw.bundleArgs(tf, "--btc-header", hdrArg(900001, hdr), "--require-anchors", "2"), "VERIFIED:", "2 anchor(s) of the aggregate checkpoint(s)")
	wantRun(t, 2, aw.bundleArgs(tf, "--btc-header", "900001:abcd"), "--btc-header")
}

// Evidence that stamps something else, or another checkpoint, is a failure of the bundle.
func TestBundleVerifyRefusesAnchorEvidenceThatStampsSomethingElse(t *testing.T) {
	aw := newAnchorWorld(t)
	good := anchortest.NewTSA(t, anchortest.TSAOpts{Digest: aw.digest(1)})
	aw.put(2, "tsa1", anchortest.TSAFile(aw.origin, 2, aw.digest(1), good))
	aw.world.bundleJSON()
	wantRun(t, 1, aw.bundleArgs(aw.trustWith(good.RootPEM)), "FAILED at step anchor-evidence", "stamps something else")
	// an anchor of an older size of the log is checked against that size, recomputed from the signed log
	d1 := aw.digest(1)
	if err := os.RemoveAll(filepath.Join(aw.dir, "a", aw.aggID, "anchors", "2")); err != nil {
		t.Fatal(err)
	}
	tok1 := anchortest.NewTSA(t, anchortest.TSAOpts{Digest: d1})
	aw.put(1, "tsa2", anchortest.TSAFile(aw.origin, 1, d1, tok1))
	wantRun(t, 0, aw.bundleArgs(aw.trustWith(tok1.RootPEM)), "VERIFIED:", "anchor tsa tsa2")
}
