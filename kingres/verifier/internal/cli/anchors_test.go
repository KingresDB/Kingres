// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package cli

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/KingresDB/Kingres/kingres/verifier/internal/anchortest"
	"github.com/KingresDB/Kingres/kingres/verifier/internal/merkle"
	"github.com/KingresDB/Kingres/kingres/verifier/internal/otsproof"
	"github.com/KingresDB/Kingres/kingres/verifier/internal/signednote"
	"github.com/KingresDB/Kingres/kingres/verifier/internal/testdir"
)

// The anchors of the world's aggregate checkpoint of size 2 (and of size 1, an older one). The evidence is built by
// internal/anchortest from the documented formats; where a real mined header would be needed the test replaces the work
// check (parseHeader) by one at a very easy target, and says so. The real header of the genesis block is used where only
// a refusal is tested.

const anchorNow = "2026-11-19T12:00:00Z"

type anchorWorld struct {
	*world
	origin string
	text   [3][]byte // by size
}

func newAnchorWorld(t *testing.T) *anchorWorld {
	w, _, _ := stdWorld(t)
	aw := &anchorWorld{world: w, origin: w.prefix + "/a/" + w.aggID}
	note, err := os.ReadFile(filepath.Join(w.dir, "a", w.aggID, "checkpoint"))
	if err != nil {
		t.Fatal(err)
	}
	n, err := signednote.Parse(note)
	if err != nil {
		t.Fatal(err)
	}
	aw.text[2] = n.Text
	r1 := merkle.LeafHash(w.aggEnc[0])
	aw.text[1] = []byte(fmt.Sprintf("%s\n1\n%s\n", aw.origin, base64.StdEncoding.EncodeToString(r1[:])))
	return aw
}

func (aw *anchorWorld) digest(size int) [32]byte { return sha256.Sum256(aw.text[size]) }

func (aw *anchorWorld) put(size int, target string, evidence []byte) {
	aw.world.put(fmt.Sprintf("a/%s/anchors/%d/%s.json", aw.aggID, size, target), evidence)
}

// trustWith writes the trust file with these time-stamp roots.
func (aw *anchorWorld) trustWith(roots ...string) string {
	tf := loadTrust(aw.t, aw.trust)
	tf.TSARoots = roots
	data, err := tf.Marshal()
	if err != nil {
		aw.t.Fatal(err)
	}
	p := filepath.Join(testdir.New(aw.t), "trust.json")
	if err := os.WriteFile(p, data, 0o600); err != nil {
		aw.t.Fatal(err)
	}
	return p
}

func (aw *anchorWorld) args(trustFile string, more ...string) []string {
	return append([]string{"anchors", "--trust", trustFile, "--source", aw.dir, "--aggregate-id", aw.aggID, "--now", anchorNow}, more...)
}

func easyHeaders(t *testing.T) {
	old := parseHeader
	limit := new(big.Int).Lsh(big.NewInt(1), 255)
	parseHeader = func(h []byte) (*otsproof.Header, error) { return otsproof.ParseHeaderLimit(h, limit) }
	t.Cleanup(func() { parseHeader = old })
}

func hdrArg(height uint64, h []byte) string {
	return fmt.Sprintf("%d:%s", height, hex.EncodeToString(h))
}

func TestAnchorsTSAVerifiesOfflineAgainstTheAuditorsRoot(t *testing.T) {
	aw := newAnchorWorld(t)
	d := aw.digest(2)
	tok := anchortest.NewTSA(t, anchortest.TSAOpts{Digest: d, WithName: true})
	aw.put(2, "tsa1", anchortest.TSAFile(aw.origin, 2, d, tok))
	out := wantRun(t, 0, aw.args(aw.trustWith(tok.RootPEM), "--require", "tsa"), "VERIFIED:", "anchor tsa tsa1", "genTime 2026-11-19T11:00:00Z", "the authority's word", "no revocation status")
	if strings.Contains(out, "INCOMPLETE") {
		t.Fatal(out)
	}
	wantRun(t, 0, aw.args(aw.trustWith(tok.RootPEM), "--json"), `"verified": true`)
	// no root of the auditor: nothing can be verified, and it is not a pass
	wantRun(t, 3, aw.args(aw.trust), "INCOMPLETE", "no tsa_roots")
	// another authority's root
	other := anchortest.NewTSA(t, anchortest.TSAOpts{Digest: d})
	wantRun(t, 1, aw.args(aw.trustWith(other.RootPEM)), "FAILED at step anchor-tsa", "does not chain")
}

func TestAnchorsTSARefusesATokenForAnotherDigestOrAnotherCheckpoint(t *testing.T) {
	aw := newAnchorWorld(t)
	d := aw.digest(2)
	// a good token for another digest, with evidence that claims the right payload
	wrong := anchortest.NewTSA(t, anchortest.TSAOpts{Digest: aw.digest(1)})
	aw.put(2, "tsa1", anchortest.TSAFile(aw.origin, 2, d, wrong))
	wantRun(t, 1, aw.args(aw.trustWith(wrong.RootPEM)), "FAILED at step anchor-tsa", "stamps something else")
	// the evidence file for the other checkpoint's digest
	good := anchortest.NewTSA(t, anchortest.TSAOpts{Digest: aw.digest(1)})
	aw.put(2, "tsa1", anchortest.TSAFile(aw.origin, 2, aw.digest(1), good))
	wantRun(t, 1, aw.args(aw.trustWith(good.RootPEM)), "FAILED at step anchor-evidence", "stamps something else")
	// the evidence of another size or origin
	aw.put(2, "tsa1", anchortest.TSAFile(aw.origin, 3, d, good))
	wantRun(t, 1, aw.args(aw.trustWith(good.RootPEM)), "FAILED at step anchor-evidence", "of size 3")
	// evidence whose summary disagrees with the token
	tok := anchortest.NewTSA(t, anchortest.TSAOpts{Digest: d})
	lied := *tok
	lied.Serial = "1"
	aw.put(2, "tsa1", anchortest.TSAFile(aw.origin, 2, d, &lied))
	wantRun(t, 1, aw.args(aw.trustWith(tok.RootPEM)), "FAILED at step anchor-tsa", "does not match the token")
}

func TestAnchorsOfAnOlderCheckpointAreChecked(t *testing.T) {
	aw := newAnchorWorld(t)
	d := aw.digest(1)
	tok := anchortest.NewTSA(t, anchortest.TSAOpts{Digest: d})
	aw.put(1, "tsa1", anchortest.TSAFile(aw.origin, 1, d, tok))
	out := wantRun(t, 0, aw.args(aw.trustWith(tok.RootPEM), "--size", "1"), "VERIFIED:", "aggregate checkpoint of size 1", "recomputed from the signed log")
	_ = out
	// the digest of a log that is not the signed one's prefix is not what the evidence says
	bad := sha256.Sum256([]byte("not a prefix"))
	tok2 := anchortest.NewTSA(t, anchortest.TSAOpts{Digest: bad})
	aw.put(1, "tsa1", anchortest.TSAFile(aw.origin, 1, bad, tok2))
	wantRun(t, 1, aw.args(aw.trustWith(tok2.RootPEM), "--size", "1"), "FAILED at step anchor-evidence", "recomputed from the signed log")
	// an anchor for a size beyond the signed checkpoint: not checked, status 3
	wantRun(t, 3, aw.args(aw.trust, "--size", "9"), "larger than the signed aggregate checkpoint")
	wantRun(t, 2, aw.args(aw.trust, "--size", "0"), "--size")
}

func TestAnchorsOTSAgainstTheAuditorsHeader(t *testing.T) {
	easyHeaders(t)
	aw := newAnchorWorld(t)
	d := aw.digest(2)
	frag, root := anchortest.Fragment(d, []byte{1, 2, 3}, []byte("0123456789abcdef0123456789abcdef"), anchortest.BitcoinAttestation(900001))
	aw.put(2, "ots1", anchortest.OTSFile(aw.origin, 2, d, [][]byte{frag}, 900001, &root))
	hdr := anchortest.Header(root, time.Date(2026, 11, 19, 12, 30, 0, 0, time.UTC), 0x207fffff)
	wantRun(t, 0, aw.args(aw.trust, "--btc-header", hdrArg(900001, hdr), "--require", "ots"), "VERIFIED:", "anchor ots ots1", "no later than 2026-11-19T12:30:00Z", "best chain is not shown")
	// without a header: valid tree, status 3, the header the cluster kept is not used
	wantRun(t, 3, aw.args(aw.trust), "INCOMPLETE", "not checked against Bitcoin", "no --btc-header")
	// a header of another block's merkle root: status 1
	other := anchortest.Header([32]byte{9}, time.Date(2026, 11, 19, 12, 30, 0, 0, time.UTC), 0x207fffff)
	wantRun(t, 1, aw.args(aw.trust, "--btc-header", hdrArg(900001, other)), "FAILED at step anchor-ots", "a different header, or the evidence is of another object")
	// a header at another height than the attestation: the attestation is unchecked, so 3
	wantRun(t, 3, aw.args(aw.trust, "--btc-header", hdrArg(5, hdr)), "no --btc-header was given")
	// a header that is not mined (the real genesis block with a bit changed, then at the real limit)
	genesis, _ := hex.DecodeString("0100000000000000000000000000000000000000000000000000000000000000000000003ba3edfd7a7b12b27ac72c3e67768f617fc81bc3888a51323a9fb8aa4b1e5e4a29ab5f49ffff001d1dac2b7c")
	broken := append([]byte{}, genesis...)
	broken[76] ^= 1
	wantRun(t, 1, aw.args(aw.trust, "--btc-header", hdrArg(900001, broken)), "FAILED at step anchor-ots", "does not meet the target")
	wantRun(t, 2, aw.args(aw.trust, "--btc-header", "900001:abcd"), "--btc-header")
	wantRun(t, 2, aw.args(aw.trust, "--btc-header", hdrArg(900001, hdr), "--btc-header", hdrArg(900001, other)), "two different headers")
}

func TestAnchorsOTSRealWorkLimitRefusesAnEasyHeader(t *testing.T) {
	aw := newAnchorWorld(t)
	d := aw.digest(2)
	frag, root := anchortest.Fragment(d, []byte{1}, []byte("0123456789abcdef0123456789abcdef"), anchortest.BitcoinAttestation(900001))
	aw.put(2, "ots1", anchortest.OTSFile(aw.origin, 2, d, [][]byte{frag}, 900001, &root))
	hdr := anchortest.Header(root, time.Date(2026, 11, 19, 12, 30, 0, 0, time.UTC), 0x207fffff)
	wantRun(t, 1, aw.args(aw.trust, "--btc-header", hdrArg(900001, hdr)), "FAILED at step anchor-ots", "easier than difficulty 1")
}

func TestAnchorsOTSEvidenceThatDoesNotHoldWhatItClaims(t *testing.T) {
	easyHeaders(t)
	aw := newAnchorWorld(t)
	d := aw.digest(2)
	frag, root := anchortest.Fragment(d, []byte{1}, []byte("0123456789abcdef0123456789abcdef"), anchortest.BitcoinAttestation(900001))
	// the evidence names another merkle root than the fragment reaches
	bad := root
	bad[0] ^= 1
	aw.put(2, "ots1", anchortest.OTSFile(aw.origin, 2, d, [][]byte{frag}, 900001, &bad))
	wantRun(t, 1, aw.args(aw.trust), "FAILED at step anchor-ots", "no fragment holds it")
	// a fragment for another digest (the tree from OUR digest reaches another root than the header has)
	frag2, root2 := anchortest.Fragment(aw.digest(1), []byte{1}, []byte("0123456789abcdef0123456789abcdef"), anchortest.BitcoinAttestation(900001))
	aw.put(2, "ots1", anchortest.OTSFile(aw.origin, 2, d, [][]byte{frag2}, 0, nil))
	hdr := anchortest.Header(root2, time.Date(2026, 11, 19, 12, 30, 0, 0, time.UTC), 0x207fffff)
	wantRun(t, 1, aw.args(aw.trust, "--btc-header", hdrArg(900001, hdr)), "FAILED at step anchor-ots", "different header")
	// an operation this program does not compute: status 1, by name
	aw.put(2, "ots1", anchortest.OTSFile(aw.origin, 2, d, [][]byte{append([]byte{0x03}, anchortest.BitcoinAttestation(1)...)}, 0, nil))
	wantRun(t, 1, aw.args(aw.trust), "FAILED at step anchor-ots", "unsupported operation RIPEMD-160")
	// pending only: not an anchor yet
	aw.put(2, "ots1", anchortest.OTSFile(aw.origin, 2, d, [][]byte{anchortest.PendingAttestation("https://cal.example.com")}, 0, nil))
	wantRun(t, 3, aw.args(aw.trust), "INCOMPLETE", "no Bitcoin attestation yet")
	// the digest of the evidence is not the recomputed one
	aw.put(2, "ots1", anchortest.OTSFile(aw.origin, 2, d, [][]byte{frag}, 0, nil))
	raw, _ := os.ReadFile(filepath.Join(aw.dir, "a", aw.aggID, "anchors", "2", "ots1.json"))
	_ = raw
}

// a signed note served the way a store serves an object version
func serveObject(t *testing.T, body []byte, hdr map[string]string) *httptest.Server {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for k, v := range hdr {
			w.Header().Set(k, v)
		}
		_, _ = w.Write(body)
	}))
	t.Cleanup(s.Close)
	return s
}

func TestAnchorsWORM(t *testing.T) {
	aw := newAnchorWorld(t)
	d := aw.digest(2)
	note, err := os.ReadFile(filepath.Join(aw.dir, "a", aw.aggID, "checkpoint"))
	if err != nil {
		t.Fatal(err)
	}
	ev := anchortest.WORMEvidence{Key: fmt.Sprintf("kingres-anchors/%s/%020d.note", aw.aggID, 2), VersionID: "v1", Body: note, LockMode: "COMPLIANCE",
		RetainUntil: time.Date(2036, 1, 1, 0, 0, 0, 0, time.UTC), LastModified: time.Date(2026, 11, 19, 11, 0, 0, 0, time.UTC)}
	aw.put(2, "w1", anchortest.WORMFile(aw.origin, 2, d, ev))
	hdr := map[string]string{"x-amz-version-id": "v1", "x-amz-object-lock-mode": "COMPLIANCE", "x-amz-object-lock-retain-until-date": "2036-01-01T00:00:00Z", "Last-Modified": "Thu, 19 Nov 2026 11:00:00 GMT"}
	// not re-fetched: the evidence is the cluster's claim, status 3
	wantRun(t, 3, aw.args(aw.trust), "INCOMPLETE", "the object was not fetched", "not an anchor")
	wantRun(t, 3, aw.args(aw.trust, "--require", "worm"), "no worm anchor was verified")
	// re-fetched
	srv := serveObject(t, note, hdr)
	wantRun(t, 0, aw.args(aw.trust, "--worm-url", srv.URL, "--require", "worm"), "VERIFIED:", "anchor worm w1", "the store confirms the lock COMPLIANCE", "the store's word")
	wantRun(t, 0, aw.args(aw.trust, "--worm-url", "w1="+srv.URL), "VERIFIED:")
	// a store that returns no lock headers: verified bytes, the lock is said to be the cluster's word
	bare := serveObject(t, note, nil)
	wantRun(t, 0, aw.args(aw.trust, "--worm-url", bare.URL), "VERIFIED:", "was NOT confirmed")
	// another object
	wantRun(t, 1, aw.args(aw.trust, "--worm-url", serveObject(t, append([]byte{}, append(note, 'x')...), hdr).URL), "FAILED at step anchor-worm", "not the object that was recorded")
	// a header that disagrees
	h2 := map[string]string{}
	for k, v := range hdr {
		h2[k] = v
	}
	h2["x-amz-object-lock-mode"] = "GOVERNANCE"
	wantRun(t, 1, aw.args(aw.trust, "--worm-url", serveObject(t, note, h2).URL), "FAILED at step anchor-worm", "lock mode is GOVERNANCE")
	// the evidence of an object of another checkpoint (the key)
	ev2 := ev
	ev2.Key = fmt.Sprintf("kingres-anchors/%s/%020d.note", aw.aggID, 1)
	aw.put(2, "w1", anchortest.WORMFile(aw.origin, 2, d, ev2))
	wantRun(t, 1, aw.args(aw.trust, "--worm-url", srv.URL), "FAILED at step anchor-worm", "does not end in")
	// the object signed by a key the auditor does not trust (and the evidence made for those bytes)
	forged := []byte(signText(string(aw.text[2]), aw.origin, newSigner(66)))
	ev3 := ev
	ev3.Body = forged
	aw.put(2, "w1", anchortest.WORMFile(aw.origin, 2, d, ev3))
	wantRun(t, 1, aw.args(aw.trust, "--worm-url", serveObject(t, forged, hdr).URL), "FAILED at step", "signed by a key of your trust file")
	// a store that cannot be reached is "could not check"
	aw.put(2, "w1", anchortest.WORMFile(aw.origin, 2, d, ev))
	dead := httptest.NewServer(http.NotFoundHandler())
	url := dead.URL
	dead.Close()
	wantRun(t, 3, aw.args(aw.trust, "--worm-url", url), "INCOMPLETE", "could not be reached")
	wantRun(t, 2, aw.args(aw.trust, "--worm-url", "ftp://x/y"), "--worm-url")
}

func TestAnchorsReportsTheAbsenceOfAnchorsAndMixesKinds(t *testing.T) {
	aw := newAnchorWorld(t)
	wantRun(t, 3, aw.args(aw.trust), "INCOMPLETE", "holds no anchor")
	d := aw.digest(2)
	tok := anchortest.NewTSA(t, anchortest.TSAOpts{Digest: d})
	aw.put(2, "tsa1", anchortest.TSAFile(aw.origin, 2, d, tok))
	frag, root := anchortest.Fragment(d, []byte{1}, []byte("0123456789abcdef0123456789abcdef"), anchortest.BitcoinAttestation(900001))
	aw.put(2, "ots1", anchortest.OTSFile(aw.origin, 2, d, [][]byte{frag}, 900001, &root))
	tr := aw.trustWith(tok.RootPEM)
	// the time-stamp verifies, the OpenTimestamps one cannot be checked: without --require that is 3, with --require tsa it is a note
	wantRun(t, 3, aw.args(tr), "INCOMPLETE", "ots anchor ots1")
	wantRun(t, 0, aw.args(tr, "--require", "tsa"), "VERIFIED:", "not required, and not verified")
	wantRun(t, 3, aw.args(tr, "--require", "tsa,ots"), "--require ots")
	wantRun(t, 0, aw.args(tr, "--require", "any"), "VERIFIED:")
	wantRun(t, 2, aw.args(tr, "--require", "gcs"), "--require")
	// a failure of any evidence file is 1 whatever --require says
	bad := anchortest.NewTSA(t, anchortest.TSAOpts{Digest: d, BadSignature: true})
	aw.put(2, "tsa2", anchortest.TSAFile(aw.origin, 2, d, bad))
	wantRun(t, 1, aw.args(aw.trustWith(tok.RootPEM, bad.RootPEM), "--require", "tsa"), "FAILED at step anchor-tsa")
	// a malformed evidence file is a failure
	aw.put(2, "tsa2", []byte(`{"format":1}`))
	wantRun(t, 1, aw.args(tr, "--require", "tsa"), "FAILED at step anchor-evidence")
	// --target names the files when the source cannot list
	wantRun(t, 0, aw.args(tr, "--require", "tsa", "--target", "tsa1"), "VERIFIED:")
	wantRun(t, 3, aw.args(tr, "--target", "nothere"), "INCOMPLETE")
	wantRun(t, 2, aw.args(tr, "--target", "../x"), "--target")
}

func TestAnchorsKeyEventsStillApply(t *testing.T) {
	aw := newAnchorWorld(t)
	tr := pinBoth(t, aw.world, aw.s2)
	aw.world.event(0, "key-revoked", []signer{aw.s2}, "key: "+b64pub(aw.s1), "compromised-since: 2026-11-18T00:00:00Z", "reason: stolen laptop")
	wantRun(t, 1, aw.args(tr), "revoked by the verified event")
}

// A tile that was changed makes the root of the first leaf something the signed log does not contain: an anchor of that
// "checkpoint" must not verify even when its token stamps exactly that digest (the prefix is proved from the signed root).
func TestAnchorsOfAnOlderCheckpointNeedTheProofThatItIsAPrefix(t *testing.T) {
	aw := newAnchorWorld(t)
	tile := filepath.Join(aw.dir, "a", aw.aggID, "tile", "0", "000.p", "2")
	b, err := os.ReadFile(tile)
	if err != nil {
		t.Fatal(err)
	}
	b[0] ^= 1
	if err := os.WriteFile(tile, b, 0o644); err != nil {
		t.Fatal(err)
	}
	var forged [32]byte
	copy(forged[:], b[:32])
	text := []byte(fmt.Sprintf("%s\n1\n%s\n", aw.origin, base64.StdEncoding.EncodeToString(forged[:])))
	d := sha256.Sum256(text)
	tok := anchortest.NewTSA(t, anchortest.TSAOpts{Digest: d})
	aw.put(1, "tsa1", anchortest.TSAFile(aw.origin, 1, d, tok))
	wantRun(t, 1, aw.args(aw.trustWith(tok.RootPEM), "--size", "1"), "FAILED at step tiles", "not a prefix of the signed log")
}
