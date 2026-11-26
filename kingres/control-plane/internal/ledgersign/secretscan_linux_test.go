// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

//go:build linux

package ledgersign_test

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"os"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
	"unsafe"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/api"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/ledgerkeys"
)

const scanMask = 0x5a

// scanSelf counts the places in the readable private memory of this process where the text that mask hides (each
// byte xor scanMask, so that the test never holds the text itself) appears, not counting its own scan buffer. ok is
// false when the memory of the process cannot be read. It is the scan of internal/ledgerkeys, which is the same
// check that a core file of the process would be given: a core file is that memory.
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

// maskOfSeed reads the plain key file of a key and returns its seed as base64 text xor scanMask, zeroing everything it read,
// so that the test holds no copy of the text.
func maskOfSeed(t *testing.T, e *env, fp string) []byte {
	t.Helper()
	data, err := e.kr.File(fp)
	if err != nil {
		t.Fatal(err)
	}
	defer clear(data)
	i := bytes.IndexByte(data, '\n')
	var j struct {
		Seed json.RawMessage `json:"seed"`
	}
	if i < 0 || json.Unmarshal(data[i+1:], &j) != nil || len(j.Seed) < 3 {
		t.Fatal("the key file is not a plain key file")
	}
	text := j.Seed[1 : len(j.Seed)-1] // without the quotes
	mask := make([]byte, len(text))
	for k := range text {
		mask[k] = text[k] ^ scanMask
	}
	clear(j.Seed)
	return mask
}

// A key made, announced, used for signatures, listed, rotated and retired by the service leaves no base64 text of its seed in the
// memory of the process once the keyring is closed (K1-08 of the key library, now for the daemon's own path: the file is read
// into slices that are cleared, no string holds it). The path that sends a key file to another server holds it as a string for
// the length of the call (a JSON body); it is not part of this flow, which has no peer, and is named in the documentation.
//
// The runtime can leave a copy that no code of the package reaches (a stack that was copied while the text was in a local
// variable of the standard library), so a flow is tried up to three times with fresh keys.
func TestNoTextOfTheSeedRemainsAfterTheKeysAreMadeUsedAndClosed(t *testing.T) {
	var last int
	for attempt := 1; attempt <= 3; attempt++ {
		n, ok := flowLeavesCopies(t)
		if !ok {
			t.Skip("the memory of the process cannot be read here")
		}
		if n == 0 {
			controlSeesACopy(t)
			return
		}
		last = n
		t.Logf("attempt %d: %d copies of the base64 text of a seed in memory", attempt, n)
	}
	t.Fatalf("the base64 text of a seed is in memory (%d times in the last run) after the keys were closed", last)
}

func flowLeavesCopies(t *testing.T) (int, bool) {
	e := newEnv(t, memFactory)
	ctx := context.Background()
	e.create(0)
	k1 := e.signing().Keys[0].Fingerprint
	mask1 := maskOfSeed(t, e, k1)
	e.clk.Add(time.Hour)
	e.create(1)
	var k2 string
	for _, k := range e.signing().Keys {
		if k.Fingerprint != k1 {
			k2 = k.Fingerprint
		}
	}
	mask2 := maskOfSeed(t, e, k2)
	// Use the keys: Signers opens both, a retirement and a revocation sign events, the views are built.
	for i := 0; i < 3; i++ {
		signers, _, err := e.svc.Signers(ctx)
		if err != nil || len(signers) != 2 {
			t.Fatalf("Signers: %v %d", err, len(signers))
		}
	}
	_, _ = e.svc.Signing(ctx)
	_, _ = e.svc.TrustFile(ctx)
	_, _ = e.svc.Alarm(ctx)
	e.clk.Add(25 * time.Hour)
	e.mustDone(e.run("ledger.key.retire", map[string]any{"fingerprint": k1}))
	e.create(1)
	if _, err := e.svc.RevokeKey(ctx, "a", k2, api.RevokeLedgerSigningKeyRequest{CompromisedSince: e.clk.Now(), Reason: "scan", Confirm: k2}); err != nil {
		t.Fatal(err)
	}
	e.svc.Close()
	e.kr.Close()
	_ = ledgerkeys.Passphrase{}
	n1, ok := scanSelf(mask1)
	if !ok {
		return 0, false
	}
	n2, _ := scanSelf(mask2)
	clear(mask1)
	clear(mask2)
	return n1 + n2, true
}

// controlSeesACopy is the control of the scan: it does see a copy of a text that exists in the memory of the test.
func controlSeesACopy(t *testing.T) {
	t.Helper()
	text := make([]byte, 43)
	if _, err := rand.Read(text); err != nil {
		t.Fatal(err)
	}
	for i := range text {
		text[i] = 'A' + text[i]%26
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
