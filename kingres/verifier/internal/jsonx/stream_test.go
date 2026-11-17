// SPDX-License-Identifier: AGPL-3.0-only

package jsonx

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"strings"
	"testing"
	"testing/iotest"
	"unicode/utf8"
)

// referenceChecks is the check of the first versions, over a buffer that holds
// the whole document: valid UTF-8, then checkEscapes. The streaming reader must
// refuse what it refuses, whatever the size of the pieces the document is read
// in.
func referenceChecks(data []byte, maxString int) error {
	if !utf8.Valid(data) {
		return errors.New("not utf-8")
	}
	return referenceEscapes(data, maxString)
}

func referenceEscapes(data []byte, maxString int) error {
	inStr := false
	start := 0
	space := 0
	for i := 0; i < len(data); i++ {
		c := data[i]
		if !inStr {
			if c == ' ' || c == 0x0a || c == 0x09 || c == 0x0d {
				if space++; space > MaxSpaceRun {
					return errors.New("space")
				}
				continue
			}
			space = 0
			if c == '"' {
				inStr = true
				start = i
			}
			continue
		}
		if i-start > maxString+1 {
			return errors.New("long string")
		}
		switch c {
		case '"':
			inStr = false
		case '\\':
			if i+1 >= len(data) {
				return nil
			}
			if data[i+1] != 'u' {
				i++
				continue
			}
			r, ok := refHex4(data, i+2)
			if !ok {
				return nil
			}
			i += 5
			switch {
			case r >= 0xD800 && r < 0xDC00:
				if i+6 < len(data) && data[i+1] == '\\' && data[i+2] == 'u' {
					if r2, ok2 := refHex4(data, i+3); ok2 && r2 >= 0xDC00 && r2 < 0xE000 {
						i += 6
						continue
					}
				}
				return errors.New("high")
			case r >= 0xDC00 && r < 0xE000:
				return errors.New("low")
			}
		}
	}
	return nil
}

func refHex4(data []byte, at int) (rune, bool) {
	if at+4 > len(data) {
		return 0, false
	}
	var r rune
	for _, c := range data[at : at+4] {
		d, ok := hexVal(c)
		if !ok {
			return 0, false
		}
		r = r<<4 | d
	}
	return r, true
}

func corpus() []string {
	return []string{
		`{"a":"x"}`, `["\ud83d\ude00"]`, `["\ud83d"]`, `["\ude00"]`, `["\ud83dx"]`, `["\ud83d\n"]`, `["\ud83d\u0041"]`,
		`["\ud83d\ud83d"]`, `["\u00zz"]`, `["\uZZZZ\ud800"]`, `["\ud800`, `["\ud800\`, `["\ud800\u`, `["\ud800\ude`,
		"[\"\xc3\xa9\"]", "[\"\xe6\x97\xa5\"]", "[\"\xff\"]", "[\"\xc3\"]", "[\"\xe6\x97\"]", "[\"\xf0\x9f\x98\"]", "[\"\xf0\x9f\x98\x80\"]",
		`["a\"b\\"]`, `["\\u"]`, `["\u0041"]`, `{"\ud83d\ude00":1}`, `["` + strings.Repeat("a", 1<<16) + `"]`,
		`["` + strings.Repeat("a", 1<<16+3) + `"]`, `["` + strings.Repeat(`\n`, 1<<15+3) + `"]`,
		strings.Repeat(" ", MaxSpaceRun) + "[]", strings.Repeat(" ", MaxSpaceRun+1) + "[]", "[" + strings.Repeat("\n", MaxSpaceRun+1) + "]",
		"[1,\r\n\t2]", `[1] x`, `[1]`, ``, ` `, `[`, `{"a":`, `"\`, `"\u`, `["\ud800\udc00\ud800\udc00"]`,
	}
}

func parseWith(r io.Reader) (string, error) {
	v, err := ParseReader(r, DefaultLimits, "", nil)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%v", v.kind), nil
}

func TestStreamingChecksAgreeWithTheBufferedOnes(t *testing.T) {
	inputs := corpus()
	rng := rand.New(rand.NewSource(1))
	for i := 0; i < 400; i++ {
		var b strings.Builder
		b.WriteString(`["`)
		for j := rng.Intn(30); j > 0; j-- {
			switch rng.Intn(8) {
			case 0:
				b.WriteString(`\ud83d`)
			case 1:
				b.WriteString(`\ude00`)
			case 2:
				b.WriteString(`\u00e9`)
			case 3:
				b.WriteString("\xc3\xa9")
			case 4:
				b.WriteString("\xe6\x97\xa5")
			case 5:
				b.WriteString(`\`)
			case 6:
				b.WriteByte(byte(rng.Intn(256)))
			default:
				b.WriteString("a")
			}
		}
		b.WriteString(`"]`)
		inputs = append(inputs, b.String())
	}
	for _, in := range inputs {
		data := []byte(in)
		whole, wholeErr := parseWith(bytes.NewReader(data))
		oracle := referenceChecks(data, DefaultLimits.MaxString)
		if oracle != nil && wholeErr == nil {
			t.Fatalf("the reference refuses %.60q (%v) and the stream takes it", in, oracle)
		}
		// whatever the pieces, the same verdict and the same message
		for _, mk := range []func(io.Reader) io.Reader{
			iotest.OneByteReader, iotest.HalfReader, iotest.DataErrReader,
			func(r io.Reader) io.Reader { return &chunkReader{r: r, rng: rand.New(rand.NewSource(int64(len(in))))} },
		} {
			got, err := parseWith(mk(bytes.NewReader(data)))
			if (err == nil) != (wholeErr == nil) || got != whole {
				t.Fatalf("%.60q: read whole %q %v, in pieces %q %v", in, whole, wholeErr, got, err)
			}
			// the text of an error of the decoder quotes what it had read so far, which
			// depends on the pieces; the errors of the checks do not
			// (and which of two faults in one document is reported first depends on how
			// far the decoder had read when the check found its own)
			if err != nil && !strings.Contains(wholeErr.Error(), "invalid JSON") && !strings.Contains(err.Error(), "invalid JSON") && err.Error() != wholeErr.Error() {
				t.Fatalf("%.60q: the message depends on the pieces:\n%v\n%v", in, wholeErr, err)
			}
		}
	}
}

// chunkReader gives between one and seven bytes at a time.
type chunkReader struct {
	r   io.Reader
	rng *rand.Rand
}

func (c *chunkReader) Read(p []byte) (int, error) {
	n := 1 + c.rng.Intn(7)
	if n < len(p) {
		p = p[:n]
	}
	return c.r.Read(p)
}

// The memory of a document is a constant: ParseReader never asks the reader for
// more than the decoder needs, and a stream that never ends is refused at the
// first limit, not read to the end.
func TestParseReaderDoesNotReadAnEndlessInputToTheEnd(t *testing.T) {
	endless := io.MultiReader(strings.NewReader(`["`), zeros{})
	if _, err := ParseReader(endless, DefaultLimits, "", nil); err == nil || !strings.Contains(err.Error(), "longer than") {
		t.Fatalf("an endless string: %v", err)
	}
	spaces := io.MultiReader(strings.NewReader(`[`), spacesReader{})
	if _, err := ParseReader(spaces, DefaultLimits, "", nil); err == nil || !strings.Contains(err.Error(), "white space") {
		t.Fatalf("endless white space: %v", err)
	}
}

type zeros struct{}

func (zeros) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 'a'
	}
	return len(p), nil
}

type spacesReader struct{}

func (spacesReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = ' '
	}
	return len(p), nil
}

// L10-18: the README says nesting 32 and the code accepted 33 arrays when the
// innermost was empty.
func TestNestingLimitIsExactlyTheDocumentedOne(t *testing.T) {
	for _, c := range []struct {
		name       string
		open, shut string
	}{{"arrays", "[", "]"}, {"objects", `{"a":`, "}"}} {
		for depth := MaxDepth - 1; depth <= MaxDepth+1; depth++ {
			inner := "1"
			if c.name == "arrays" {
				inner = ""
			}
			doc := strings.Repeat(c.open, depth) + inner + strings.Repeat(c.shut, depth)
			_, err := Parse([]byte(doc))
			if want := depth > MaxDepth; (err != nil) != want {
				t.Errorf("%s nested %d deep: error %v, want error %v", c.name, depth, err, want)
			}
		}
	}
}

// FuzzParseReader checks that a document read in pieces of any size gets the
// verdict of the document read whole, and that nothing the reference checks
// refuse is taken.
func FuzzParseReader(f *testing.F) {
	for _, s := range corpus() {
		f.Add([]byte(s), uint8(1))
	}
	f.Fuzz(func(t *testing.T, data []byte, piece uint8) {
		_, wholeErr := parseWith(bytes.NewReader(data))
		if oracle := referenceChecks(data, DefaultLimits.MaxString); oracle != nil && wholeErr == nil {
			t.Fatalf("the reference refuses %.40q (%v) and the stream takes it", data, oracle)
		}
		r := &chunkReader{r: bytes.NewReader(data), rng: rand.New(rand.NewSource(int64(piece)))}
		_, err := parseWith(r)
		if (err == nil) != (wholeErr == nil) {
			t.Fatalf("whole %v, in pieces %v", wholeErr, err)
		}
	})
}
