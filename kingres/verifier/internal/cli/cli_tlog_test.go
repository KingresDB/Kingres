// SPDX-License-Identifier: AGPL-3.0-only

package cli

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

const (
	testPrefix = "ledger.example.com/kingres"
	testChain  = "abababababababababababababababababababababababababababababababab"
)

// ---- a tiny independent reference: SHA-256 and the RFC 6962 definition ----

type h32 = [32]byte

func sha(parts ...[]byte) h32 {
	h := sha256.New()
	for _, p := range parts {
		h.Write(p)
	}
	var out h32
	h.Sum(out[:0])
	return out
}

func refMTH(h []h32) h32 {
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

func leaves(n int) []h32 {
	out := make([]h32, n)
	for i := range out {
		out[i] = sha([]byte{0}, []byte(fmt.Sprintf("entry %d", i)))
	}
	return out
}

// writeTiles writes the tiles of the tree of the first size leaves under dir.
func writeTiles(t *testing.T, dir string, all []h32, size int) {
	t.Helper()
	put := func(level, n, width int, hs []h32) {
		var data []byte
		for _, x := range hs {
			data = append(data, x[:]...)
		}
		name := fmt.Sprintf("tile/%d/%s", level, tileIndex(n))
		if width < 256 {
			name += ".p/" + strconv.Itoa(width)
		}
		p := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for level, span := 0, 1; size/span > 0; level, span = level+1, span*256 {
		count := size / span
		hs := make([]h32, count)
		for j := range hs {
			hs[j] = refMTH(all[j*span : (j+1)*span])
		}
		for n := 0; n*256 < count; n++ {
			end := min((n+1)*256, count)
			put(level, n, end-n*256, hs[n*256:end])
		}
	}
}

func tileIndex(n int) string {
	if n < 1000 {
		return fmt.Sprintf("%03d", n)
	}
	return fmt.Sprintf("x%03d/%03d", n/1000, n%1000)
}

// ---- signing with the standard library ----

type signer struct {
	priv ed25519.PrivateKey
	pub  ed25519.PublicKey
}

func newSigner(seed byte) signer {
	s := make([]byte, ed25519.SeedSize)
	for i := range s {
		s[i] = seed
	}
	k := ed25519.NewKeyFromSeed(s)
	return signer{k, k.Public().(ed25519.PublicKey)}
}

func (s signer) fp() string {
	sum := sha256.Sum256(s.pub)
	return hex.EncodeToString(sum[:])
}

func (s signer) note(origin string, size int, root h32) string {
	text := fmt.Sprintf("%s\n%d\n%s\n", origin, size, base64.StdEncoding.EncodeToString(root[:]))
	sum := sha256.Sum256(append(append([]byte(origin), 0x0A, 0x01), s.pub...))
	raw := append(append([]byte{}, sum[:4]...), ed25519.Sign(s.priv, []byte(text))...)
	return text + "\n\u2014 " + origin + " " + base64.StdEncoding.EncodeToString(raw) + "\n"
}

// publication is a directory with a chain log at two sizes.
type publication struct {
	dir, trust, logDir string
	signer             signer
	all                []h32
	origin             string
}

func newPublication(t *testing.T, oldSize, newSize int) *publication {
	t.Helper()
	p := &publication{dir: t.TempDir(), signer: newSigner(1), all: leaves(newSize)}
	p.origin = testPrefix + "/c/" + testChain + "/e1"
	p.logDir = filepath.Join(p.dir, "c", testChain, "e1")
	writeTiles(t, p.logDir, p.all, newSize)
	p.write(t, "checkpoint", p.signer.note(p.origin, newSize, refMTH(p.all[:newSize])))
	p.write(t, "old.note", p.signer.note(p.origin, oldSize, refMTH(p.all[:oldSize])))
	p.trust = filepath.Join(t.TempDir(), "trust.json")
	code, out, errs := run(t, "trust", "init", "--origin-prefix", testPrefix, "--public-key", base64.StdEncoding.EncodeToString(p.signer.pub),
		"--fingerprint", p.signer.fp(), "--out", p.trust)
	if code != 0 {
		t.Fatalf("trust init: %d %s %s", code, out, errs)
	}
	return p
}

func (p *publication) write(t *testing.T, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(p.logDir, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func (p *publication) log() string { return "chain:" + testChain + ":e1" }

func TestTrustInit(t *testing.T) {
	s := newSigner(9)
	pub := base64.StdEncoding.EncodeToString(s.pub)
	out := filepath.Join(t.TempDir(), "trust.json")
	base := []string{"trust", "init", "--origin-prefix", testPrefix, "--public-key", pub, "--out", out}
	if code, _, errs := run(t, base...); code != 2 || !strings.Contains(errs, "--fingerprint") {
		t.Fatalf("without fingerprint: %d %s", code, errs)
	}
	if code, _, _ := run(t, append(base, "--fingerprint", newSigner(8).fp())...); code != 2 {
		t.Fatalf("wrong fingerprint: %d", code)
	}
	if _, err := os.Stat(out); err == nil {
		t.Fatal("a file was written for a wrong fingerprint")
	}
	if code, _, errs := run(t, append(base, "--fingerprint", s.fp())...); code != 0 {
		t.Fatalf("init: %d %s", code, errs)
	}
	if code, _, _ := run(t, append(base, "--fingerprint", s.fp())...); code != 2 {
		t.Fatalf("overwrite: %d", code)
	}
	code, o, _ := run(t, "trust", "show", "--trust", out)
	if code != 0 || !strings.Contains(o, s.fp()) || !strings.Contains(o, testPrefix) {
		t.Fatalf("show: %d %s", code, o)
	}
	code, o, _ = run(t, "trust", "show", "--trust", out, "--json")
	var js map[string]any
	if code != 0 || json.Unmarshal([]byte(o), &js) != nil {
		t.Fatalf("show json: %d %s", code, o)
	}
	for _, args := range [][]string{
		{"trust"}, {"trust", "bogus"}, {"trust", "follow"}, {"trust", "show"}, {"trust", "show", "--trust", filepath.Join(t.TempDir(), "missing")},
		{"trust", "init"}, {"trust", "init", "--origin-prefix", "https://x", "--public-key", pub, "--fingerprint", s.fp(), "--out", "-"},
		{"trust", "init", "--origin-prefix", "a", "--public-key", "!!", "--fingerprint", s.fp(), "--out", "-"},
		{"trust", "init", "--origin-prefix", "a", "--public-key", pub, "--public-key-file", "x", "--fingerprint", s.fp()},
	} {
		if code, _, _ := run(t, args...); code != 2 {
			t.Errorf("%v: exit %d, want 2", args, code)
		}
	}
	// A key file works like the argument, and - writes to standard output.
	kf := filepath.Join(t.TempDir(), "key.pub")
	_ = os.WriteFile(kf, []byte(pub+"\n"), 0o644)
	if code, o, _ := run(t, "trust", "init", "--origin-prefix", testPrefix, "--public-key-file", kf, "--fingerprint", strings.ToUpper(s.fp()), "--out", "-"); code != 0 || !strings.Contains(o, `"format": 1`) {
		t.Fatalf("key file: %d %s", code, o)
	}
}

func TestCheckpointCommand(t *testing.T) {
	p := newPublication(t, 300, 700)
	good := []string{"checkpoint", "--trust", p.trust, "--log", p.log(), "--source", p.dir}
	code, out, errs := run(t, good...)
	if code != 0 || !strings.HasPrefix(out, "VERIFIED:") || !strings.Contains(out, "size 700") {
		t.Fatalf("%d %s %s", code, out, errs)
	}
	// The epoch may be left out when the source has exactly one.
	if code, out, _ := run(t, "checkpoint", "--trust", p.trust, "--log", "chain:"+testChain, "--source", p.dir); code != 0 {
		t.Fatalf("no epoch: %d %s", code, out)
	}
	// JSON form carries the same status.
	code, out, _ = run(t, append(good, "--json")...)
	var js jsonOutput
	if code != 0 || json.Unmarshal([]byte(out), &js) != nil || !js.Verified || js.Incomplete {
		t.Fatalf("json: %d %s", code, out)
	}
	if code, out, _ := run(t, append(good, "--min-size", "700")...); code != 0 {
		t.Fatalf("min-size ok: %d %s", code, out)
	}
	if code, out, _ := run(t, append(good, "--min-size", "701")...); code != 1 || strings.Contains(out, "VERIFIED") || !strings.Contains(out, "min-size") {
		t.Fatalf("min-size: %d %s", code, out)
	}

	good1, _ := os.ReadFile(filepath.Join(p.logDir, "checkpoint"))
	note := func(s string) []string {
		f := filepath.Join(t.TempDir(), "n")
		if err := os.WriteFile(f, []byte(s), 0o644); err != nil {
			t.Fatal(err)
		}
		return []string{"checkpoint", "--trust", p.trust, "--log", p.log(), "--note", f}
	}
	text := string(good1)
	sigStart := strings.Index(text, "\n\n") + 2
	other := newSigner(2)
	failures := map[string]string{
		"truncated":                  text[:len(text)-10],
		"no final newline":           strings.TrimSuffix(text, "\n"),
		"extra line after":           text + "extra\n",
		"extra blank line":           text + "\n",
		"wrong base64":               text[:len(text)-5] + "!\n",
		"signature of another key":   other.note(p.origin, 700, refMTH(p.all)),
		"changed size":               strings.Replace(text, "\n700\n", "\n701\n", 1),
		"size leading zero":          strings.Replace(text, "\n700\n", "\n0700\n", 1),
		"another origin, same sig":   strings.Replace(text, "/e1\n", "/e2\n", 1),
		"signed for another origin":  p.signer.note(testPrefix+"/c/"+testChain+"/e2", 700, refMTH(p.all)),
		"signed with wrong key name": strings.Replace(text, "\u2014 "+p.origin, "\u2014 "+p.origin+"x", 1),
		"text only":                  text[:sigStart],
		"empty":                      "",
	}
	for name, in := range failures {
		code, out, _ := run(t, note(in)...)
		if code != 1 || strings.Contains(out, "VERIFIED") || !strings.Contains(out, "FAILED at step") {
			t.Errorf("%s: exit %d, out %q", name, code, out)
		}
	}
	// A key that is not in the trust file.
	otherTrust := filepath.Join(t.TempDir(), "trust.json")
	if code, _, errs := run(t, "trust", "init", "--origin-prefix", testPrefix, "--public-key", base64.StdEncoding.EncodeToString(other.pub),
		"--fingerprint", other.fp(), "--out", otherTrust); code != 0 {
		t.Fatal(errs)
	}
	if code, out, _ := run(t, "checkpoint", "--trust", otherTrust, "--log", p.log(), "--note", filepath.Join(p.logDir, "checkpoint")); code != 1 || !strings.Contains(out, "step signature") {
		t.Errorf("untrusted key: %d %s", code, out)
	}
	// Input errors are status 2.
	for name, args := range map[string][]string{
		"no trust":           {"checkpoint", "--log", p.log(), "--source", p.dir},
		"no log":             {"checkpoint", "--trust", p.trust, "--source", p.dir},
		"bad log":            {"checkpoint", "--trust", p.trust, "--log", "chain:xyz", "--source", p.dir},
		"upper case id":      {"checkpoint", "--trust", p.trust, "--log", "chain:" + strings.ToUpper(testChain) + ":e1", "--source", p.dir},
		"epoch zero":         {"checkpoint", "--trust", p.trust, "--log", "chain:" + testChain + ":e0", "--source", p.dir},
		"epoch leading zero": {"checkpoint", "--trust", p.trust, "--log", "chain:" + testChain + ":e01", "--source", p.dir},
		"aggregate epoch":    {"checkpoint", "--trust", p.trust, "--log", "aggregate:" + testChain + ":e1", "--source", p.dir},
		"trust missing":      {"checkpoint", "--trust", filepath.Join(p.dir, "nope"), "--log", p.log(), "--source", p.dir},
		"trust is a note":    {"checkpoint", "--trust", filepath.Join(p.logDir, "checkpoint"), "--log", p.log(), "--source", p.dir},
		"bad now":            {"checkpoint", "--trust", p.trust, "--log", p.log(), "--source", p.dir, "--now", "yesterday"},
		"unknown flag":       {"checkpoint", "--bogus"},
		"epoch with note":    {"checkpoint", "--trust", p.trust, "--log", "chain:" + testChain, "--note", filepath.Join(p.logDir, "checkpoint")},
	} {
		if code, _, _ := run(t, args...); code != 2 {
			t.Errorf("%s: exit %d, want 2", name, code)
		}
	}
	// Two epochs: refuse to guess.
	if err := os.MkdirAll(filepath.Join(p.dir, "c", testChain, "e2"), 0o755); err != nil {
		t.Fatal(err)
	}
	if code, _, errs := run(t, "checkpoint", "--trust", p.trust, "--log", "chain:"+testChain, "--source", p.dir); code != 2 || !strings.Contains(errs, "epoch") {
		t.Errorf("two epochs: %d %s", code, errs)
	}
}

func TestCheckpointKeyWindow(t *testing.T) {
	p := newPublication(t, 1, 5)
	data, _ := os.ReadFile(p.trust)
	future := strings.Replace(string(data), `"not_before": null`, `"not_before": "2030-01-01T00:00:00Z"`, 1)
	f := filepath.Join(t.TempDir(), "t.json")
	_ = os.WriteFile(f, []byte(future), 0o644)
	args := []string{"checkpoint", "--trust", f, "--log", p.log(), "--source", p.dir}
	if code, _, errs := run(t, append(args, "--now", "2026-12-01T00:00:00Z")...); code != 2 {
		t.Errorf("key not yet valid: %d %s", code, errs)
	}
	if code, _, errs := run(t, append(args, "--now", "2031-01-01T00:00:00Z")...); code != 0 {
		t.Errorf("key valid: %d %s", code, errs)
	}
	// not_after is enforced on --now: after it the key is not used at all,
	// and the error says why and how to check older checkpoints.
	retired := strings.Replace(string(data), `"not_after": null`, `"not_after": "2027-01-01T00:00:00Z"`, 1)
	_ = os.WriteFile(f, []byte(retired), 0o644)
	code, _, errs := run(t, append(args, "--now", "2028-01-01T00:00:00Z")...)
	if code != 2 || !strings.Contains(errs, "not_after") || !strings.Contains(errs, "--now") {
		t.Errorf("retired key: %d %s", code, errs)
	}
	if code, out, _ := run(t, append(args, "--now", "2026-06-01T00:00:00Z")...); code != 0 || !strings.Contains(out, "fixed by --now") {
		t.Errorf("inside the window: %d %s", code, out)
	}
}

func TestConsistencyCommand(t *testing.T) {
	p := newPublication(t, 300, 700)
	oldRoot := refMTH(p.all[:300])
	args := func(more ...string) []string {
		return append([]string{"consistency", "--trust", p.trust, "--log", p.log(), "--source", p.dir}, more...)
	}
	oldNote := filepath.Join(p.logDir, "old.note")
	code, out, errs := run(t, args("--old", oldNote)...)
	if code != 0 || !strings.Contains(out, "VERIFIED: the log at size 700 extends the log at size 300") {
		t.Fatalf("%d %s %s", code, out, errs)
	}
	sr := "300:" + base64.StdEncoding.EncodeToString(oldRoot[:])
	if code, out, _ := run(t, args("--old", sr)...); code != 0 || !strings.Contains(out, "not authenticated") {
		t.Fatalf("size:root: %d %s", code, out)
	}
	if code, out, _ := run(t, args("--old", sr, "--json")...); code != 0 || !strings.Contains(out, `"verified": true`) {
		t.Fatalf("json: %d %s", code, out)
	}
	// Equal sizes, an empty old tree and an old size that is a power of two.
	cur := "700:" + base64.StdEncoding.EncodeToString(func() []byte { r := refMTH(p.all); return r[:] }())
	if code, out, _ := run(t, args("--old", cur)...); code != 0 {
		t.Fatalf("equal sizes: %d %s", code, out)
	}
	empty := sha()
	if code, out, _ := run(t, args("--old", "0:"+base64.StdEncoding.EncodeToString(empty[:]))...); code != 0 {
		t.Fatalf("empty old: %d %s", code, out)
	}
	r256 := refMTH(p.all[:256])
	if code, out, _ := run(t, args("--old", "256:"+base64.StdEncoding.EncodeToString(r256[:]))...); code != 0 {
		t.Fatalf("size 256: %d %s", code, out)
	}

	// Failures: status 1.
	badRoot := oldRoot
	badRoot[0] ^= 1
	fails := map[string][]string{
		"wrong old root":    args("--old", "300:"+base64.StdEncoding.EncodeToString(badRoot[:])),
		"old larger":        args("--old", "701:"+base64.StdEncoding.EncodeToString(oldRoot[:])),
		"equal size, other": args("--old", "700:"+base64.StdEncoding.EncodeToString(oldRoot[:])),
		"old note forged": func() []string {
			f := filepath.Join(t.TempDir(), "o")
			_ = os.WriteFile(f, []byte(newSigner(2).note(p.origin, 300, oldRoot)), 0o644)
			return args("--old", f)
		}(),
		"old note other origin": func() []string {
			f := filepath.Join(t.TempDir(), "o")
			_ = os.WriteFile(f, []byte(p.signer.note(testPrefix+"/c/"+testChain+"/e9", 300, oldRoot)), 0o644)
			return args("--old", f)
		}(),
	}
	for name, a := range fails {
		code, out, _ := run(t, a...)
		if code != 1 || strings.Contains(out, "VERIFIED") {
			t.Errorf("%s: exit %d, %s", name, code, out)
		}
	}
	// Input errors: status 2.
	for name, a := range map[string][]string{
		"no old":           args(),
		"old not a note":   args("--old", filepath.Join(p.logDir, "tile", "0", "000")),
		"old missing":      args("--old", filepath.Join(p.dir, "missing")),
		"size root junk":   args("--old", "300:!!!"),
		"size root short":  args("--old", "300:AAAA"),
		"size 0 not empty": args("--old", "0:"+base64.StdEncoding.EncodeToString(oldRoot[:])),
		"size leading 0":   args("--old", "0300:"+base64.StdEncoding.EncodeToString(oldRoot[:])),
	} {
		if code, _, _ := run(t, a...); code != 2 {
			t.Errorf("%s: exit %d, want 2", name, code)
		}
	}

	// Tiles: missing gives 3, a wrong size or content gives 1, never 0.
	hide := func(name string) (restore func()) {
		a, b := filepath.Join(p.logDir, filepath.FromSlash(name)), filepath.Join(p.logDir, filepath.FromSlash(name)+".hidden")
		if err := os.Rename(a, b); err != nil {
			t.Fatal(err)
		}
		return func() { _ = os.Rename(b, a) }
	}
	for _, tile := range []string{"tile/0/001", "tile/0/002.p/188", "tile/1/000.p/2"} {
		restore := hide(tile)
		code, out, _ := run(t, args("--old", oldNote)...)
		if code != 3 || !strings.HasPrefix(out, "INCOMPLETE:") || strings.Contains(out, "VERIFIED") || !strings.Contains(out, "missing:") {
			t.Errorf("missing %s: exit %d, %s", tile, code, out)
		}
		if code, out, _ := run(t, args("--old", oldNote, "--json")...); code != 3 || !strings.Contains(out, `"incomplete": true`) {
			t.Errorf("missing %s json: %d %s", tile, code, out)
		}
		restore()
	}
	if code, _, _ := run(t, args("--old", oldNote)...); code != 0 {
		t.Fatal("restored tiles do not verify")
	}
	for _, tile := range []string{"tile/0/001", "tile/0/002.p/188", "tile/1/000.p/2"} {
		path := filepath.Join(p.logDir, filepath.FromSlash(tile))
		orig, _ := os.ReadFile(path)
		for name, mut := range map[string][]byte{
			"short":   orig[:len(orig)-1],
			"long":    append(append([]byte{}, orig...), 0),
			"flipped": func() []byte { c := append([]byte{}, orig...); c[0] ^= 4; return c }(),
		} {
			_ = os.WriteFile(path, mut, 0o644)
			if code, out, _ := run(t, args("--old", oldNote)...); code != 1 || strings.Contains(out, "VERIFIED") {
				t.Errorf("%s tile %s: exit %d, %s", name, tile, code, out)
			}
		}
		_ = os.WriteFile(path, orig, 0o644)
	}
	// A hostile directory name cannot escape: the source is a directory and
	// tile names are numbers.
	if code, _, _ := run(t, "consistency", "--trust", p.trust, "--log", p.log(), "--source", filepath.Join(p.dir, "..", filepath.Base(p.dir)), "--old", oldNote); code != 0 {
		t.Errorf("source with .. in the path of the directory itself: %d", code)
	}
}

func TestParseLogSpec(t *testing.T) {
	for _, s := range []string{"chain:" + testChain, "chain:" + testChain + ":e7", "aggregate:" + testChain} {
		if _, err := parseLogSpec(s); err != nil {
			t.Errorf("%s: %v", s, err)
		}
	}
	for _, s := range []string{"", "chain", "chain:", "chain:00", "chain:" + testChain + ":", "chain:" + testChain + ":7", "chain:" + testChain + ":e", "chain:" + testChain + ":e1:e2",
		"c:" + testChain, "chain:" + testChain + "\n", "chain:" + testChain + ":e1234567890", "chain:../" + testChain[3:]} {
		if _, err := parseLogSpec(s); err == nil {
			t.Errorf("%q accepted", s)
		}
	}
}

func FuzzParseLogSpec(f *testing.F) {
	f.Add("chain:" + testChain + ":e1")
	f.Add("aggregate:" + testChain)
	f.Fuzz(func(t *testing.T, s string) {
		l, err := parseLogSpec(s)
		if err != nil {
			return
		}
		if strings.Contains(l.rel(), "..") || len(l.id) != 64 {
			t.Fatalf("%q gives %q", s, l.rel())
		}
	})
}

func TestUsageMentionsNewStatus(t *testing.T) {
	code, out, _ := run(t, "--help")
	if code != 0 || !strings.Contains(out, "checkpoint") || !strings.Contains(out, "3 incomplete") {
		t.Fatalf("%d %s", code, out)
	}
	for _, c := range []string{"checkpoint", "consistency"} {
		code, out, _ := run(t, c, "--help")
		if code != 0 || !strings.Contains(out, "Usage: kingres-verify "+c) || !strings.Contains(out, "3 incomplete") {
			t.Errorf("%s --help: %d %q", c, code, out)
		}
	}
}
