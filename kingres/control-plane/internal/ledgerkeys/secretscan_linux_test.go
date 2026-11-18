// SPDX-License-Identifier: AGPL-3.0-only

//go:build linux

package ledgerkeys

import (
	"bufio"
	"crypto/rand"
	"encoding/base64"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"unsafe"
)

const scanMask = 0x5a

// scanSelf counts the places in the readable private memory of this process
// where the text that mask hides (each byte xor scanMask, so that the test
// never holds the text itself) appears, not counting the scan buffer. ok is
// false when the memory of the process cannot be read.
func scanSelf(mask []byte) (count int, ok bool) {
	maps, err := os.Open("/proc/self/maps")
	if err != nil {
		return 0, false
	}
	defer maps.Close()
	mem, err := os.Open("/proc/self/mem")
	if err != nil {
		return 0, false
	}
	defer mem.Close()
	var regions [][2]uint64
	sc := bufio.NewScanner(maps)
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) < 2 || len(f[1]) < 4 || f[1][:2] != "rw" || f[1][3] != 'p' {
			continue
		}
		if len(f) >= 6 && strings.HasPrefix(f[5], "[v") {
			continue
		}
		lohi := strings.SplitN(f[0], "-", 2)
		lo, err1 := strconv.ParseUint(lohi[0], 16, 64)
		hi, err2 := strconv.ParseUint(lohi[1], 16, 64)
		if err1 != nil || err2 != nil || hi >= 1<<47 {
			continue
		}
		regions = append(regions, [2]uint64{lo, hi})
	}
	const chunk = 1 << 20
	buf := make([]byte, chunk+len(mask))
	bufLo := uint64(uintptr(unsafe.Pointer(&buf[0])))
	for _, r := range regions {
		for at := r[0]; at < r[1]; at += chunk {
			n := uint64(len(buf))
			if at+n > r[1] {
				n = r[1] - at
			}
			read, _ := mem.ReadAt(buf[:n], int64(at))
			for i := 0; i+len(mask) <= read; i++ {
				if buf[i]^scanMask != mask[0] {
					continue
				}
				match := true
				for j, m := range mask {
					if buf[i+j]^scanMask != m {
						match = false
						break
					}
				}
				if !match {
					continue
				}
				// The scan buffer holds what it read; those copies do not count.
				if addr := at + uint64(i); addr >= bufLo && addr < bufLo+uint64(len(buf)) {
					continue
				}
				count++
			}
		}
	}
	clear(buf)
	return count, true
}

// TestPlainKeyFileLeavesNoTextOfTheSeed (K1-08): after a plain key file was
// written, read, parsed and opened, and everything was closed and destroyed,
// the base64 text of the seed is nowhere in the memory of the process: the
// plain path works on byte slices that it zeroes, not on strings.
//
// The runtime can leave a copy that no code of the package can reach (a stack
// that was copied while the text was in a local variable of the standard
// library), so a flow is tried up to three times with a fresh key, and the test
// fails when each of them leaves a copy. The code that kept strings left some
// thirty copies each time.
func TestPlainKeyFileLeavesNoTextOfTheSeed(t *testing.T) {
	var last int
	for attempt := 1; attempt <= 3; attempt++ {
		n, ok := plainFlowLeavesCopies(t)
		if !ok {
			t.Skip("the memory of the process cannot be read here")
		}
		if n == 0 {
			break
		}
		last = n
		t.Logf("attempt %d: %d copies of the base64 text of the seed in memory", attempt, n)
		if attempt == 3 {
			t.Fatalf("the base64 text of the seed is in memory (%d times in the last run) after the key was closed", last)
		}
	}
	// Control: the scan does see a copy that exists.
	text := make([]byte, base64.StdEncoding.EncodedLen(SeedSize))
	if _, err := rand.Read(text); err != nil {
		t.Fatal(err)
	}
	mask := make([]byte, len(text))
	for i := range text {
		mask[i] = text[i] ^ scanMask
	}
	probe := string(text)
	clear(text)
	if n, _ := scanSelf(mask); n == 0 {
		t.Fatal("the scan does not see a copy that exists")
	}
	runtime.KeepAlive(probe)
}

// plainFlowLeavesCopies writes, reads, parses and opens a plain key file of a
// new random key, closes everything, and counts the copies of the base64 text
// of its seed in the memory of the process.
func plainFlowLeavesCopies(t *testing.T) (copies int, ok bool) {
	seed := make([]byte, SeedSize)
	if _, err := rand.Read(seed); err != nil {
		t.Fatal(err)
	}
	mask := make([]byte, base64.StdEncoding.EncodedLen(SeedSize))
	base64.StdEncoding.Encode(mask, seed)
	for i := range mask {
		mask[i] ^= scanMask
	}
	path := filepath.Join(t.TempDir(), "plain.key")
	func() {
		s, err := NewFileSigner(seed)
		if err != nil {
			t.Fatal(err)
		}
		defer s.Close()
		clear(seed)
		if err := WriteFile(path, *s, Passphrase{}, false); err != nil {
			t.Fatal(err)
		}
		for i := 0; i < 3; i++ {
			r, err := ReadFile(path, Passphrase{})
			if err != nil {
				t.Fatal(err)
			}
			if !r.SameKey(*s) {
				t.Fatal("the key read is not the key written")
			}
			r.Close()
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		f, err := Parse(raw)
		if err != nil {
			t.Fatal(err)
		}
		clear(raw)
		o, err := f.Open(Passphrase{})
		if err != nil {
			t.Fatal(err)
		}
		o.Close()
		f.Destroy()
	}()
	return scanSelf(mask)
}

// TestParsePlainLeavesNoRawSeed (K2-12): parsePlain decodes the seed into a
// buffer of its own and copies it into the secret it returns; once the secret
// is destroyed the raw 32 bytes are nowhere in the memory of the process, so
// the buffer was cleared. No crypto/ed25519 call is made on this path, so the
// count is exactly zero (a run that removes the clearing leaves the buffer in
// freed memory, and the scan finds it). A whole flow does call the library, and
// the copies that it leaves are the ones the package doc names.
func TestParsePlainLeavesNoRawSeed(t *testing.T) {
	seed := make([]byte, SeedSize)
	if _, err := rand.Read(seed); err != nil {
		t.Fatal(err)
	}
	mask := make([]byte, SeedSize)
	for i := range mask {
		mask[i] = seed[i] ^ scanMask
	}
	rest := marshalPlain(seed)[len(FileMagic)+1+len(plainHead):]
	clear(seed)
	s, err := parsePlain(rest)
	if err != nil {
		t.Fatal(err)
	}
	// Control: while the secret is alive the scan sees the one copy it holds.
	n, ok := scanSelf(mask)
	if !ok {
		t.Skip("the memory of the process cannot be read here")
	}
	if n < 1 {
		t.Fatalf("the scan does not see the copy that the secret holds (%d)", n)
	}
	s.destroy()
	clear(rest)
	if n, _ := scanSelf(mask); n != 0 {
		t.Errorf("the raw seed is in memory %d times after the secret was destroyed: parsePlain left a copy", n)
	}
}
