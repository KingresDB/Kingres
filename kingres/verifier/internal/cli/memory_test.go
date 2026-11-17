// SPDX-License-Identifier: AGPL-3.0-only

package cli

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strings"
	"testing"
	"time"

	"github.com/KingresDB/Kingres/kingres/verifier/internal/signednote"
	"github.com/KingresDB/Kingres/kingres/verifier/internal/trust"
)

// L9-05. The verifier runs on an auditor's machine over files a hostile server
// or publisher wrote. The invariant: memory is bounded by a constant plus a small
// multiple of the largest single value, never by a multiple of the input.
//
// Two measurements guard it. This file samples the peak of the heap of
// the process while a command runs, in the process of the test, on every system The file
// memory_linux_test.go runs the command in a child process and reads its peak
// resident set from the system (not possible the same way on Windows, where
// that test is skipped, and the first one still runs).

// hostileShapes writes, in dir, one file of exactly size bytes for each way to
// make a JSON reader hold a multiple of its input, and returns name -> path.
func hostileShapes(t testing.TB, dir string, size int) map[string]string {
	t.Helper()
	fill := func(prefix, unit, suffix string) []byte {
		b := make([]byte, 0, size)
		b = append(b, prefix...)
		for len(b)+len(unit)+len(suffix) <= size {
			b = append(b, unit...)
		}
		b = append(b, suffix...)
		for len(b) < size {
			b = append(b, ' ')
		}
		return b
	}
	shapes := map[string][]byte{
		// eight million zeros in an unknown member: 126 bytes of memory per byte
		// of input in the tree the first version built
		"zeros": fill(`{"x":[`, "0,", "0]}"),
		// a nesting that the depth cap stops at the 33rd level
		"deep-array":  bytes.Repeat([]byte("["), size),
		"deep-object": bytes.Repeat([]byte(`{"a":`), size/5),
		// a million members, each different
		"members": func() []byte {
			var b bytes.Buffer
			b.WriteString("{")
			for i := 0; b.Len() < size-32; i++ {
				b.WriteString(`"k` + string(rune('a'+i%26)) + string(rune('a'+(i/26)%26)) + string(rune('a'+(i/676)%26)) + string(rune('a'+(i/17576)%26)) + `":0,`)
			}
			b.WriteString(`"z":0}`)
			return b.Bytes()
		}(),
		// the same key a million times
		"duplicate-keys": fill("{", `"a":0,`, `"a":0}`),
		// one string of nearly the whole file
		"huge-string": fill(`{"x":"`, "a", `"}`),
		// a file that is all whitespace, and one that is all newlines
		"whitespace": bytes.Repeat([]byte(" "), size),
		"newlines":   bytes.Repeat([]byte("\n"), size),
		// a million short objects, one per line: an export that is all one kind
		"objects": fill("", "{}\n", ""),
	}
	out := map[string]string{}
	for name, data := range shapes {
		if len(data) > size {
			data = data[:size]
		}
		p := filepath.Join(dir, name+".json")
		if err := os.WriteFile(p, data, 0o644); err != nil {
			t.Fatal(err)
		}
		out[name] = p
	}
	return out
}

// memCommands are the commands that read a file, with the place of the hostile
// file marked @. size is the largest file the command reads (its cap), which
// the hostile file is made to match.
func memCommands(t testing.TB) []struct {
	name string
	args []string
	size int
} {
	zeroHash := strings.Repeat("00", 32)
	d0 := fx("digests/d0.json")
	trustFile := filepath.Join(t.TempDir(), "trust.json")
	// a trust file of the right shape is only needed for the flags that precede
	// the hostile file: the hostile file is read first or the run ends at the
	// missing trust file, either way the memory is measured.
	if err := os.WriteFile(trustFile, []byte(`{"format":1,"origin_prefix":"x.example","keys":[]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	log := "chain:" + strings.Repeat("ab", 32) + ":e1"
	return []struct {
		name string
		args []string
		size int
	}{
		{"proof file", []string{"proof", "--proof", "@", "--digest", d0}, maxFileSize},
		{"proof digest", []string{"proof", "--proof", fx("proofs/acc1.json"), "--digest", "@"}, maxDigestFile},
		{"row-hash columns", []string{"row-hash", "--table-id", "1", "--columns", "@"}, maxFileSize},
		{"digest-chain file", []string{"digest-chain", "@"}, maxDigestFile},
		{"blocks export", []string{"blocks", "--blocks", "@", "--chain-id", zeroHash}, maxBlocksSize / 64},
		{"blocks digest", []string{"blocks", "--blocks", fx("blocks.jsonl"), "--digest", "@"}, maxDigestFile},
		{"checkpoint trust", []string{"checkpoint", "--trust", "@", "--log", log, "--note", "@"}, trust.MaxFileSize},
		{"consistency old", []string{"consistency", "--trust", trustFile, "--log", log, "--old", "@"}, maxStateFile},
		{"trust show", []string{"trust", "show", "--trust", "@"}, trust.MaxFileSize},
		{"consistency new", []string{"consistency", "--trust", trustFile, "--log", log, "--old", "1:" + base64.StdEncoding.EncodeToString(make([]byte, 32)), "--new", "@"}, signednote.MaxNoteSize},
		{"trust init key file", []string{"trust", "init", "--origin-prefix", "x.example", "--public-key-file", "@", "--fingerprint", zeroHash, "--out", filepath.Join(t.TempDir(), "out.json")}, 4096},
		{"checkpoint note", []string{"checkpoint", "--trust", trustFile, "--log", log, "--note", "@"}, signednote.MaxNoteSize},
	}
}

// forEachHostileFile runs fn for every command that reads a file and every
// hostile shape, at the largest size the command accepts (its cap) and at 16
// MiB (over the cap: the refusal must also be cheap).
// onThisSystem scales the large files down on Windows. The test writes some
// hundreds of megabytes of hostile files, which a Windows machine with a file
// scanner reads again as they are written, and a scanner that holds a file for a
// moment made the removal of a test directory fail at the end of other tests
// (L9-13). The Linux test, which measures the process itself, keeps the sizes of
// the caps; 4 MiB shows a blow-up of a hundred times as well as 16 MiB does.
func onThisSystem(size int) int {
	if runtime.GOOS == "windows" && size > 4<<20 {
		return 4 << 20
	}
	return size
}

func forEachHostileFile(t *testing.T, fn func(t *testing.T, name string, args []string, size int)) {
	dir := t.TempDir()
	bySize := map[int]map[string]string{}
	for _, c := range memCommands(t) {
		for _, sz := range []int{c.size, 16 << 20} {
			sz = onThisSystem(sz)
			if bySize[sz] == nil {
				sd := filepath.Join(dir, fmt.Sprint(sz))
				if err := os.Mkdir(sd, 0o755); err != nil {
					t.Fatal(err)
				}
				bySize[sz] = hostileShapes(t, sd, sz)
			}
			for shape, path := range bySize[sz] {
				args := make([]string, len(c.args))
				for i, a := range c.args {
					args[i] = a
					if a == "@" {
						args[i] = path
					}
				}
				t.Run(fmt.Sprintf("%s/%d/%s", c.name, sz, shape), func(t *testing.T) { fn(t, c.name+"|"+shape, args, sz) })
			}
		}
	}
}

// TestAllocationDoesNotGrowWithTheShapeOfTheInput measures the peak of the heap
// while each command reads each hostile file. The bound is a constant that
// holds the file itself a few times; the first version needed 2 to 3 GiB for
// the zeros.
func TestAllocationDoesNotGrowWithTheShapeOfTheInput(t *testing.T) {
	if testing.Short() {
		t.Skip("reads files of 16 MiB")
	}
	forEachHostileFile(t, func(t *testing.T, _ string, args []string, size int) {
		// the file read once, the decoder's buffer for one token, a copy of one
		// value: about four times the file in the worst case (one string of the
		// whole file), and a constant for the values the caps allow
		bound := uint64(5*size + 8<<20)
		var code int
		var out, errs string
		got := peakHeap(func() { code, out, errs = run(t, args...) })
		if got > bound {
			t.Errorf("peak heap %d KiB for a file of %d KiB (bound %d KiB)", got>>10, size>>10, bound>>10)
		}
		if code == 0 || strings.Contains(out, "VERIFIED") {
			t.Errorf("a hostile file was accepted: %d\n%.300s", code, out+errs)
		}
	})
}

// longChainProof writes a proof whose chain of headers fills a file of about
// size bytes: the one member of a proof that grows with the file, which the
// parser reads element by element. The headers are those of the fixture's
// block, repeated: the proof is not walked, because the fixture block is the digest block, but the parse
// of the file is what is measured.
func longChainProof(t testing.TB, dir string, size int) (path string, elements int) {
	t.Helper()
	raw, err := os.ReadFile(fx("proofs/acc1.json"))
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	var hdr bytes.Buffer
	if err := json.Compact(&hdr, doc["block"]); err != nil {
		t.Fatal(err)
	}
	chain := []byte("[")
	for len(chain) < size {
		if len(chain) > 1 {
			chain = append(chain, ',')
		}
		chain = append(chain, hdr.Bytes()...)
		elements++
	}
	chain = append(chain, ']')
	doc["chain"] = chain
	out, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	path = filepath.Join(dir, "long-chain-proof.json")
	if err := os.WriteFile(path, out, 0o644); err != nil {
		t.Fatal(err)
	}
	return path, elements
}

func TestAllocationOfAProofWithALongChain(t *testing.T) {
	if testing.Short() {
		t.Skip("reads a file of 16 MiB")
	}
	size := onThisSystem(15 << 20)
	path, n := longChainProof(t, t.TempDir(), size)
	var code int
	var out, errs string
	got := peakHeap(func() { code, out, errs = run(t, "proof", "--proof", path, "--digest", fx("digests/d0.json")) })
	t.Logf("%d headers, peak heap %d KiB", n, got>>10)
	// the file once, the headers kept (about 100 bytes each) and the slice that
	// holds them while it grows
	if got > uint64(3*size) {
		t.Errorf("peak heap %d MiB for a proof of %d MiB", got>>20, size>>20)
	}
	// The fixture block is the digest block, so the chain is not walked: headers
	// beyond the ones needed are read and ignored, and the proof stands.
	if code != 0 {
		t.Errorf("status %d: %s %s", code, out, errs)
	}
}

// peakHeap runs fn and returns how far the live heap rose above where it
// was, sampled every millisecond with the collector made eager (a garbage
// collection at every 20 percent of growth), so that what is counted is
// close to what is kept and not what a lazy collector has not swept yet.
func peakHeap(fn func()) uint64 {
	old := debug.SetGCPercent(20)
	defer debug.SetGCPercent(old)
	runtime.GC()
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	base, peak := ms.HeapAlloc, ms.HeapAlloc
	stop, done := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		for {
			var m runtime.MemStats
			runtime.ReadMemStats(&m)
			if m.HeapAlloc > peak {
				peak = m.HeapAlloc
			}
			select {
			case <-stop:
				return
			case <-time.After(time.Millisecond):
			}
		}
	}()
	fn()
	close(stop)
	<-done
	if peak < base {
		return 0
	}
	return peak - base
}
