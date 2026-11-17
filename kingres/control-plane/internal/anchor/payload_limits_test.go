// SPDX-License-Identifier: AGPL-3.0-only

package anchor

import (
	"encoding/base64"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// sizedNote returns a well-formed signed note of exactly total bytes (total
// at least 100): the head, then signature lines of at most 999 bytes.
func sizedNote(total int) []byte {
	b := note("1", root32(), "")
	b = append(b, []byte("\xe2\x80\x94 n " + strings.Repeat("A", 992) + "\n")[:0]...)
	for rest := total - len(b); rest > 0; {
		var l int
		switch {
		case rest >= 999+11:
			l = 999
		default:
			l = rest
		}
		// line = em dash (3) + space + name + space + base64 (4k) + newline
		k := 1
		if l == 999 {
			k = 248
		}
		nameLen := l - 3 - 1 - 1 - 4*k - 1
		b = append(b, []byte("\xe2\x80\x94 "+strings.Repeat("n", nameLen)+" "+strings.Repeat("A", 4*k)+"\n")...)
		rest -= l
	}
	return b
}

func originNote(origin string) []byte {
	return []byte(origin + "\n1\n" + base64.StdEncoding.EncodeToString(root32()) + "\n\n" + sigLine)
}

// A2-04: NewPayload applies the limits of the verifier, each at its
// boundary.
func TestPayloadLimitsOfTheVerifier(t *testing.T) {
	ok := func(name string, n []byte) {
		t.Helper()
		if _, err := NewPayload("ab", n, t0); err != nil {
			t.Errorf("%s refused: %v", name, err)
		}
	}
	refused := func(name string, n []byte) {
		t.Helper()
		if _, err := NewPayload("ab", n, t0); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	// origin: 255 bytes pass, 256 do not
	ok("origin 255", originNote(strings.Repeat("o", 255)))
	refused("origin 256", originNote(strings.Repeat("o", 256)))
	// the origin rule of the verifier: no plus sign, no Unicode space, UTF-8 allowed
	refused("origin with a plus sign", originNote("kingres+x"))
	refused("origin with an em space", originNote("kingres\u2003x"))
	refused("origin with a no-break space", originNote("kingres\u00a0x"))
	refused("origin with a line separator", originNote("kingres\u2028x"))
	ok("origin with a letter outside ASCII", originNote("kingres.\u00e9xample/agg"))
	// size: 2^62-1 passes, 2^62 does not
	sized := func(size string) []byte {
		return []byte("o\n" + size + "\n" + base64.StdEncoding.EncodeToString(root32()) + "\n\n" + sigLine)
	}
	ok("size 2^62-1", sized("4611686018427387903"))
	refused("size 2^62", sized("4611686018427387904"))
	// signature lines: 32 pass, 33 do not
	ok("32 signature lines", note("1", root32(), strings.Repeat(sigLine, 32)))
	refused("33 signature lines", note("1", root32(), strings.Repeat(sigLine, 33)))
	// line length: 1024 bytes pass, 1025 do not (a signature line)
	line := func(n int) string { // a signature line of n bytes without the newline
		k := (n - 8) / 4 // base64 digits come in fours; the name takes up the rest
		return sigLine[:4] + strings.Repeat("n", n-3-1-1-4*k) + " " + strings.Repeat("A", 4*k) + "\n"
	}
	ok("line of 1024", note("1", root32(), line(1024)))
	refused("line of 1025", note("1", root32(), line(1025)))
	// the same limit holds on the text lines (the origin is the longest one)
	if MaxNoteLineLen < MaxOriginLen {
		t.Error("the line limit is below the origin limit")
	}
	// a note of exactly MaxSignedNote passes; the verifier's own cap is higher
	ok("note of MaxSignedNote", sizedNote(MaxSignedNote))
	refused("note of MaxSignedNote+1", sizedNote(MaxSignedNote+1))
	// the time of the first attempt is required
	if _, err := NewPayload("ab", note("1", root32(), sigLine), time.Time{}); err == nil {
		t.Error("a zero first attempt accepted")
	}
	p, err := NewPayload("ab", note("1", root32(), sigLine), time.Date(2026, 12, 1, 10, 0, 0, 999, time.FixedZone("x", 3600)))
	if err != nil || !p.FirstAttempt.Equal(time.Date(2026, 12, 1, 9, 0, 0, 0, time.UTC)) {
		t.Errorf("first attempt not normalised: %v %v", p.FirstAttempt, err)
	}
	// Check refuses a payload without it, ParsePayload gives none
	q, err := ParsePayload("ab", note("1", root32(), sigLine))
	if err != nil || !q.FirstAttempt.IsZero() || q.Check() == nil {
		t.Errorf("ParsePayload: %v %v", q.FirstAttempt, err)
	}
}

// The limits are literals here on purpose: raising a constant must fail this
// test, not follow it.
func TestPayloadLimitConstants(t *testing.T) {
	if MaxOriginLen != 255 || MaxSignatureLines != 32 || MaxNoteLineLen != 1024 || TreeSizeLimit != 1<<62 || MaxSignedNote != 16<<10 {
		t.Fatalf("%d %d %d %d %d", MaxOriginLen, MaxSignatureLines, MaxNoteLineLen, TreeSizeLimit, MaxSignedNote)
	}
}

// constIn evaluates the integer constant name that file declares, for the
// forms the verifier uses (a literal, a shift, a conversion, a product).
func constIn(t *testing.T, file, name string) uint64 {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, file, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var eval func(e ast.Expr) uint64
	eval = func(e ast.Expr) uint64 {
		switch x := e.(type) {
		case *ast.BasicLit:
			v, err := strconv.ParseUint(x.Value, 0, 64)
			if err != nil {
				t.Fatalf("%s: %v", file, err)
			}
			return v
		case *ast.ParenExpr:
			return eval(x.X)
		case *ast.CallExpr:
			if len(x.Args) != 1 {
				t.Fatalf("%s: unsupported call in %s", file, name)
			}
			return eval(x.Args[0])
		case *ast.BinaryExpr:
			a, b := eval(x.X), eval(x.Y)
			switch x.Op {
			case token.SHL:
				return a << b
			case token.MUL:
				return a * b
			}
		}
		t.Fatalf("%s: unsupported expression for %s", file, name)
		return 0
	}
	for _, d := range f.Decls {
		gd, ok := d.(*ast.GenDecl)
		if !ok || gd.Tok != token.CONST {
			continue
		}
		for _, s := range gd.Specs {
			vs := s.(*ast.ValueSpec)
			for i, n := range vs.Names {
				if n.Name == name && i < len(vs.Values) {
					return eval(vs.Values[i])
				}
			}
		}
	}
	t.Fatalf("%s: constant %s not found", file, name)
	return 0
}

// A2-04: the constants of this package are those of the verifier, read from
// its source in the sibling module. The test is skipped when that directory
// is not there (a test binary run elsewhere).
func TestPayloadLimitsEqualTheVerifiers(t *testing.T) {
	dir := filepath.Join("..", "..", "..", "verifier", "internal")
	sn := filepath.Join(dir, "signednote", "signednote.go")
	cp := filepath.Join(dir, "checkpoint", "checkpoint.go")
	tl := filepath.Join(dir, "tlog", "tile.go")
	for _, f := range []string{sn, cp, tl} {
		if _, err := os.Stat(f); err != nil {
			t.Skipf("the verifier source is not beside this package: %v", err)
		}
	}
	for _, c := range []struct {
		what string
		mine uint64
		file string
		name string
	}{
		{"origin length", MaxOriginLen, cp, "MaxOriginLength"},
		{"signature lines", MaxSignatureLines, sn, "MaxSignatures"},
		{"line length", MaxNoteLineLen, sn, "MaxLineLength"},
		{"tree size", TreeSizeLimit, tl, "MaxTreeSize"},
	} {
		if got := constIn(t, c.file, c.name); got != c.mine {
			t.Errorf("%s: this package says %d, the verifier's %s is %d", c.what, c.mine, c.name, got)
		}
	}
	// the verifier takes a note of any size up to its own cap, which must not
	// be below ours (a note we anchor must be readable)
	if v := constIn(t, sn, "MaxNoteSize"); v < MaxSignedNote {
		t.Errorf("the verifier's MaxNoteSize %d is below MaxSignedNote %d", v, MaxSignedNote)
	}
	if v := constIn(t, sn, "MaxTextLines"); v < 3 {
		t.Errorf("the verifier's MaxTextLines %s is below 3", fmt.Sprint(v))
	}
}

// A2-04: an origin that is valid UTF-8 outside ASCII is a valid origin for the
// verifier, so the evidence file must carry it (it was once printable ASCII
// only).
func TestEvidenceFileCarriesAUTF8Origin(t *testing.T) {
	ev, err := EncodeEvidence(TSAEvidence{TSAURL: "https://tsa.example/tsr", TokenDERB64: "MAMCAQE=", GenTime: "2026-12-01T10:00:02Z", Serial: "4242", Policy: "1.2.3.4.5", RecordedBy: RecordedByCluster})
	if err != nil {
		t.Fatal(err)
	}
	digest := strings.Repeat("ab", 32)
	for origin, want := range map[string]bool{"kingres.\u00e9xample": true, "a b": false, "a+b": false, strings.Repeat("o", 255): true, strings.Repeat("o", 256): false} {
		b, err := EncodeEvidenceFile(EvidenceFile{Kind: KindTSA, Origin: origin, Size: 1, PayloadSHA256: digest, CreatedAt: t0, Evidence: ev})
		if (err == nil) != want {
			t.Errorf("origin %q: %v", origin, err)
			continue
		}
		if err == nil {
			f, err := ParseEvidenceFile(b)
			if err != nil || f.Origin != origin {
				t.Errorf("origin %q does not round trip: %v", origin, err)
			}
		}
	}
}
