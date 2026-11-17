// SPDX-License-Identifier: AGPL-3.0-only

package cli

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/KingresDB/Kingres/kingres/verifier/internal/chain"
	"github.com/KingresDB/Kingres/kingres/verifier/internal/format"
	"github.com/KingresDB/Kingres/kingres/verifier/internal/proof"
)

// L10-07: an honest proof of an old row is never refused for its size. A proof
// carries one block header after another up to the block of the digest; at one
// block in 10 seconds that is 8,640 a day, and the old cap of 16 MiB (about
// 50,000 headers, six days) refused every row older than that.

// syntheticProof writes a valid proof whose chain has n headers after its block,
// built on the block of a fixture, and a digest at the last of them. The header
// of every block is in the form the fixtures have (about 340 bytes). It returns
// the paths and the size of the proof file.
func syntheticProof(t testing.TB, dir string, n int) (proofPath, digestPath string, size int64) {
	t.Helper()
	raw, err := os.ReadFile(fx("proofs/acc5.json"))
	if err != nil {
		t.Fatal(err)
	}
	base, err := proof.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	m["chain"] = []any{}
	body, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	pre, post, ok := bytes.Cut(body, []byte(`"chain":[]`))
	if !ok {
		t.Fatal("no chain member")
	}
	proofPath = filepath.Join(dir, "big-proof.json")
	f, err := os.Create(proofPath)
	if err != nil {
		t.Fatal(err)
	}
	w := bufio.NewWriterSize(f, 1<<20)
	w.Write(pre)
	w.WriteString(`"chain":[`)
	prev := base.Block
	var last format.Header
	for i := 1; i <= n; i++ {
		h := format.Header{Height: prev.Height + 1, HashVersion: 1, PrevHash: prev.BlockHash, TxMerkleRoot: sha([]byte("root"), []byte(strconv.Itoa(i))), TxCount: 1, ClosedAtUS: prev.ClosedAtUS + 10_000_000}
		h.BlockHash = h.ComputeHash()
		if i > 1 {
			w.WriteString(", ")
		}
		fmt.Fprintf(w, `{"height": %d, "tx_count": %d, "prev_hash": "%x", "block_hash": "%x", "closed_at_us": %d, "hash_version": 1, "tx_merkle_root": "%x"}`,
			h.Height, h.TxCount, h.PrevHash, h.BlockHash, h.ClosedAtUS, h.TxMerkleRoot)
		prev, last = h, h
	}
	if n == 0 {
		last = base.Block
	}
	w.WriteString(`]`)
	w.Write(post)
	w.WriteString("\n")
	if err := w.Flush(); err != nil {
		t.Fatal(err)
	}
	st, _ := f.Stat()
	size = st.Size()
	f.Close()
	dg, err := os.ReadFile(fx("digests/d5.json"))
	if err != nil {
		t.Fatal(err)
	}
	var d map[string]any
	if err := json.Unmarshal(dg, &d); err != nil {
		t.Fatal(err)
	}
	d["height"] = last.Height
	d["block_hash"] = fmt.Sprintf("%x", last.BlockHash)
	d["chain_id"] = fmt.Sprintf("%x", base.ChainID)
	delete(d, "block_closed_at")
	out, _ := json.Marshal(d)
	digestPath = filepath.Join(dir, "big-digest.json")
	if err := os.WriteFile(digestPath, out, 0o644); err != nil {
		t.Fatal(err)
	}
	return proofPath, digestPath, size
}

func TestHonestProofOfAnOldRowIsNotRefusedForItsSize(t *testing.T) {
	n := 60_009 // the reproduction of the review: 20 MB, refused by the cap of 16 MiB
	if testing.Short() {
		n = 1000
	}
	p, d, size := syntheticProof(t, t.TempDir(), n)
	if !testing.Short() && size <= 16<<20 {
		t.Fatalf("the proof is %d bytes, not over the old cap", size)
	}
	code, out, errs := run(t, "proof", "--proof", p, "--digest", d)
	if code != 0 || !strings.HasPrefix(out, "VERIFIED") || strings.Contains(errs+out, "larger than the limit") {
		t.Fatalf("status %d for a proof of %d bytes\n%.600s%s", code, size, out, errs)
	}
	if !strings.Contains(out, fmt.Sprintf("through %d later header", n)) {
		t.Errorf("the walk did not go through %d headers:\n%.800s", n, out)
	}
	// a damaged header deep in the chain is found by the second reading
	data, _ := os.ReadFile(p)
	cut := bytes.LastIndex(data, []byte(`"tx_merkle_root": "`)) + len(`"tx_merkle_root": "`)
	if data[cut] == '0' {
		data[cut] = '1'
	} else {
		data[cut] = '0'
	} // another hex digit: a header with a hash that is not its own
	bad := filepath.Join(t.TempDir(), "bad.json")
	if err := os.WriteFile(bad, data, 0o644); err != nil {
		t.Fatal(err)
	}
	code, out, _ = run(t, "proof", "--proof", bad, "--digest", d)
	if code != 1 || strings.Contains(out, "VERIFIED") || !strings.Contains(out, "FAILED at step chain-walk") {
		t.Fatalf("a damaged last header: %d\n%.600s", code, out)
	}
}

// The chain is read again from the file, so a file that is not the same on the
// second reading is refused: the verdict is about the bytes that were parsed.
func TestProofThatChangesBetweenTheTwoReadingsIsRefused(t *testing.T) {
	p, d, _ := syntheticProof(t, t.TempDir(), 50)
	good, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	dg, err := os.ReadFile(d)
	if err != nil {
		t.Fatal(err)
	}
	digest, err := chain.ParseDigest(dg)
	if err != nil {
		t.Fatal(err)
	}
	var opens int
	open := func() (io.ReadCloser, error) {
		opens++
		b := good
		if opens == 2 {
			// the second reading sees a proof that is longer by one header, which
			// would also verify, and is not the same file
			b = append(bytes.Clone(good), ' ')
		}
		return io.NopCloser(bytes.NewReader(b)), nil
	}
	pr, sum, err := proof.ParseStream(open)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := proof.VerifyStream(pr, sum, digest, proof.Expect{}, open); err == nil || !strings.Contains(err.Error(), "not the same") {
		t.Fatalf("a file that changed between the readings: %v", err)
	}
	// the same file twice is verified
	opens = 5
	if _, err := proof.VerifyStream(pr, sum, digest, proof.Expect{}, open); err != nil {
		t.Fatalf("the same file twice: %v", err)
	}
}

// TestWriteBigProofForMeasurement writes a proof of a row that is N blocks old
// into the directory named by KINGRES_BIGPROOF_DIR (N in KINGRES_BIGPROOF_N), so
// that the program can be run on it from outside and its peak memory read. It
// does nothing in an ordinary run.
func TestWriteBigProofForMeasurement(t *testing.T) {
	dir := os.Getenv("KINGRES_BIGPROOF_DIR")
	if dir == "" {
		t.Skip("set KINGRES_BIGPROOF_DIR and KINGRES_BIGPROOF_N to write the files")
	}
	n, err := strconv.Atoi(os.Getenv("KINGRES_BIGPROOF_N"))
	if err != nil {
		t.Fatal(err)
	}
	p, d, size := syntheticProof(t, dir, n)
	t.Logf("%s %s %d bytes, %d headers", p, d, size, n)
}
