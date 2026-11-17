// SPDX-License-Identifier: AGPL-3.0-only

package ledgerlog

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"math/rand"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/ledgerkeys"
)

var bg = context.Background()

// longTests reports whether KINGRES_LONG_TESTS=1: the large cases (65,539 and
// more leaves, the 66,000-leaf case, long random sizes, the publication
// cross-check with thirteen logs, the 64 MiB batch) run only then.
func longTests() bool { return os.Getenv("KINGRES_LONG_TESTS") == "1" }

// skipUnlessLong skips the test unless KINGRES_LONG_TESTS=1, naming the variable.
func skipUnlessLong(t testing.TB, what string) {
	t.Helper()
	if !longTests() {
		t.Skipf("%s: set KINGRES_LONG_TESTS=1 to run it", what)
	}
}

// seedFor returns the seed of a randomised test: the value of the environment
// variable LEDGERLOG_SEED when set, else the clock. The seed is printed when
// the test fails, with the command that repeats it.
func seedFor(t testing.TB) int64 {
	t.Helper()
	seed := time.Now().UnixNano()
	if v := os.Getenv("LEDGERLOG_SEED"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			t.Fatalf("LEDGERLOG_SEED=%q: %v", v, err)
		}
		seed = n
	}
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("seed %d: repeat with LEDGERLOG_SEED=%d", seed, seed)
		}
	})
	return seed
}

func newRand(t testing.TB) *rand.Rand { return rand.New(rand.NewSource(seedFor(t))) }

// entryData is the entry of leaf i for tests: deterministic, of varying size.
func entryData(i int) []byte {
	b := make([]byte, 8)
	binary.BigEndian.PutUint64(b, uint64(i))
	pad := i % 37
	out := append([]byte("entry-"), b...)
	for j := 0; j < pad; j++ {
		out = append(out, byte(i+j))
	}
	return out
}

func entriesRange(from, to int) [][]byte {
	out := make([][]byte, 0, to-from)
	for i := from; i < to; i++ {
		out = append(out, entryData(i))
	}
	return out
}

func leafHashesOf(n int) []Hash {
	out := make([]Hash, n)
	for i := range out {
		out[i] = LeafHash(entryData(i))
	}
	return out
}

// ---- reference implementation, from the RFC definitions only ----

func sha(parts ...[]byte) Hash {
	h := sha256.New()
	for _, p := range parts {
		h.Write(p)
	}
	var out Hash
	h.Sum(out[:0])
	return out
}

func refMTH(h []Hash) Hash {
	switch len(h) {
	case 0:
		return sha()
	case 1:
		return h[0]
	}
	k := 1
	for k*2 < len(h) {
		k *= 2
	}
	l, r := refMTH(h[:k]), refMTH(h[k:])
	return sha([]byte{1}, l[:], r[:])
}

func refPath(m int, h []Hash) []Hash {
	if len(h) <= 1 {
		return nil
	}
	k := 1
	for k*2 < len(h) {
		k *= 2
	}
	if m < k {
		return append(refPath(m, h[:k]), refMTH(h[k:]))
	}
	return append(refPath(m-k, h[k:]), refMTH(h[:k]))
}

func refProof(m int, h []Hash) []Hash { return refSub(m, h, true) }

func refSub(m int, h []Hash, b bool) []Hash {
	n := len(h)
	if m == n {
		if b {
			return nil
		}
		return []Hash{refMTH(h)}
	}
	k := 1
	for k*2 < n {
		k *= 2
	}
	if m <= k {
		return append(refSub(m, h[:k], b), refMTH(h[k:]))
	}
	return append(refSub(m-k, h[k:], false), refMTH(h[:k]))
}

// refTiles builds every tile and bundle of the tree of size n straight from the
// definition (tile(l, k)[i] = MTH of the 256^l leaves under hash 256k+i), as a
// map from key to content, with the full and the partial tile of each level.
func refTiles(t testing.TB, n int) map[TileKey][]byte {
	t.Helper()
	h := leafHashesOf(n)
	out := map[TileKey][]byte{}
	for level := 0; level <= 7; level++ {
		span := 1 << (8 * level)
		count := n / span
		if count == 0 {
			break
		}
		hs := make([]Hash, count)
		for j := range hs {
			hs[j] = refMTH(h[j*span : (j+1)*span])
		}
		for k := 0; k*256 < count; k++ {
			end := min((k+1)*256, count)
			data, err := EncodeTile(hs[k*256 : end])
			if err != nil {
				t.Fatal(err)
			}
			out[TileKey{level, uint64(k), end - k*256}] = data
		}
	}
	for k := 0; k*256 < n; k++ {
		end := min((k+1)*256, n)
		data, err := EncodeBundle(entriesRange(k*256, end))
		if err != nil {
			t.Fatal(err)
		}
		out[TileKey{EntryLevel, uint64(k), end - k*256}] = data
	}
	return out
}

// requiredOf returns the content of the tiles Required(size) lists, from a store.
func requiredOf(t testing.TB, s Store, size uint64) map[TileKey][]byte {
	t.Helper()
	out := map[TileKey][]byte{}
	for _, k := range Required(size) {
		d, err := s.GetTile(bg, k)
		if err != nil {
			t.Fatalf("size %d: tile %s: %v", size, k.Path(), err)
		}
		out[k] = d
	}
	return out
}

// testKey returns a key from a fixed seed.
func testKey(t testing.TB, n byte) *ledgerkeys.FileSigner {
	t.Helper()
	seed := make([]byte, ledgerkeys.SeedSize)
	for i := range seed {
		seed[i] = n + byte(i)
	}
	s, err := ledgerkeys.NewFileSigner(seed)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

const testPrefix = "ledger.test/kingres"

func testOrigin(i int) string {
	var id [32]byte
	binary.BigEndian.PutUint64(id[:8], uint64(i))
	o, err := ChainOrigin(testPrefix, id, 1)
	if err != nil {
		panic(err)
	}
	return o
}

// mustAppend appends entries from to to, in calls of at most MaxBatch.
func mustAppend(t testing.TB, l *Log, from, to int) Appended {
	t.Helper()
	var a Appended
	for from < to {
		end := min(from+MaxBatch, to)
		var err error
		a, err = l.Append(bg, entriesRange(from, end))
		if err != nil {
			t.Fatalf("append %d..%d: %v", from, end, err)
		}
		from = end
	}
	return a
}

func fmtHash(h Hash) string { return fmt.Sprintf("%x", h[:6]) }
