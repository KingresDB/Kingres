// SPDX-License-Identifier: AGPL-3.0-only

package transfer

import (
	"bytes"
	"compress/gzip"
	"errors"
	"io"
	"math/rand"
	"strings"
	"testing"
	"testing/iotest"
)

// scanAll reads r through a scanner with reads of at most chunk bytes and returns the records, the bytes, the bytes that came out and the error.
func scanAll(data string, quote byte, chunk int) (records, nbytes int64, out string, err error) {
	var src io.Reader = strings.NewReader(data)
	if chunk > 0 {
		src = &chunked{r: strings.NewReader(data), n: chunk}
	}
	s := NewScanner(src, quote)
	b, err := io.ReadAll(s)
	return s.Records(), s.Bytes(), string(b), err
}

type chunked struct {
	r io.Reader
	n int
}

func (c *chunked) Read(p []byte) (int, error) {
	if len(p) > c.n {
		p = p[:c.n]
	}
	return c.r.Read(p)
}

// reference counts the records of a whole file the way PostgreSQL ends them (one style of line break, quotes toggled), with the data in hand.
func reference(data string, quote byte) (records int64, endMarkerLine int64) {
	eol := ""
	inq := false
	recStart := 0
	i := 0
	for i < len(data) {
		c := data[i]
		switch {
		case c == quote:
			inq = !inq
			i++
		case inq:
			i++
		case c == '\n' && (eol == "" || eol == "\n"):
			eol = "\n"
			if data[recStart:i] == `\.` && endMarkerLine == 0 {
				endMarkerLine = records + 1
			}
			records++
			i++
			recStart = i
		case c == '\r' && eol != "\n":
			if i+1 < len(data) && data[i+1] == '\n' && eol != "\r" {
				eol = "\r\n"
				if data[recStart:i] == `\.` && endMarkerLine == 0 {
					endMarkerLine = records + 1
				}
				records++
				i += 2
				recStart = i
				continue
			}
			if eol == "" || eol == "\r" {
				eol = "\r"
				if data[recStart:i] == `\.` && endMarkerLine == 0 {
					endMarkerLine = records + 1
				}
				records++
				i++
				recStart = i
				continue
			}
			i++
		default:
			i++
		}
	}
	if recStart < len(data) {
		if data[recStart:] == `\.` && !inq && endMarkerLine == 0 {
			endMarkerLine = records + 1
		}
		records++
	}
	return records, endMarkerLine
}

func TestScannerRecords(t *testing.T) {
	cases := []struct {
		name string
		data string
		want int64
	}{
		{"empty", "", 0},
		{"one line", "a,b\n", 1},
		{"no final newline", "a,b\nc,d", 2},
		{"two lines", "a,b\nc,d\n", 2},
		{"empty line is a record", "a\n\nb\n", 3},
		{"newline in quotes", "a,\"x\ny\"\nb,c\n", 2},
		{"doubled quote in quotes", "\"he said \"\"hi\n\"\"\",b\nc,d\n", 2},
		{"unterminated quote at the end", "a,\"x\ny\nz", 1},
		{"CRLF", "a,b\r\nc,d\r\n", 2},
		{"CRLF in quotes", "a,\"x\r\ny\"\r\nb,c\r\n", 2},
		{"CR only", "a,b\rc,d\r", 2},
		{"CR only, no final", "a,b\rc,d", 2},
		{"LF in a CRLF file is data", "a\r\nb\nc\r\n", 2},
		{"CR in an LF file is data", "a\nb\rc\nd\n", 3},
		{"lone CR at the end of an LF file", "a\nb\r", 2},
		{"CRLF then a LF that is data", "a\r\nb\n", 2},
		{"BOM is data here", "\xef\xbb\xbfa\nb\n", 2},
		{"quote inside an unquoted field", "ab\"c\nd\"ef\ng\n", 2},
		{"header and rows", "id,name\n1,a\n2,b\n", 3},
		{"only a newline", "\n", 1},
		{"only a quote", "\"", 1},
	}
	for _, tc := range cases {
		for _, chunk := range []int{0, 1, 2, 3, 7} {
			n, nb, out, err := scanAll(tc.data, '"', chunk)
			if err != nil || n != tc.want || nb != int64(len(tc.data)) || out != tc.data {
				t.Errorf("%s (chunk %d): records %d, bytes %d, err %v, passed through %v; want %d records", tc.name, chunk, n, nb, err, out == tc.data, tc.want)
			}
		}
	}
}

// The scanner against the reference on generated files, with reads split at every size: the same count, and an end-marker line found at the
// same place.
func TestScannerAgainstReference(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	alphabet := []string{"a", "b", ",", "\"", "\n", "\r", "\r\n", "\\.", "\\", ".", " ", "\"\""}
	for i := 0; i < 3000; i++ {
		var b strings.Builder
		for j, n := 0, rng.Intn(40); j < n; j++ {
			b.WriteString(alphabet[rng.Intn(len(alphabet))])
		}
		data := b.String()
		wantN, wantMarker := reference(data, '"')
		for _, chunk := range []int{0, 1, 3, 5} {
			n, _, out, err := scanAll(data, '"', chunk)
			var em *ErrEndMarker
			gotMarker := int64(0)
			if errors.As(err, &em) {
				gotMarker = em.Line
			} else if err != nil {
				t.Fatalf("%q (chunk %d): %v", data, chunk, err)
			}
			if gotMarker != wantMarker {
				t.Fatalf("%q (chunk %d): end marker at %d, reference %d", data, chunk, gotMarker, wantMarker)
			}
			if wantMarker == 0 && (n != wantN || out != data) {
				t.Fatalf("%q (chunk %d): %d records, reference %d", data, chunk, n, wantN)
			}
		}
	}
}

// The end marker: a line that is only a backslash and a dot, outside quotes, ends the import with its line number; quoted, inside a field,
// with other bytes on the line or indented it is data.
func TestScannerEndMarker(t *testing.T) {
	refused := map[string]int64{
		"a\n\\.\nb\n":      2,
		"\\.\n":            1,
		"a\n\\.":           2,
		"a\r\n\\.\r\nb":    2,
		"a\r\\.\rb":        2,
		"\"x\ny\"\n\\.\nz": 2,
	}
	for data, line := range refused {
		_, _, _, err := scanAll(data, '"', 0)
		var em *ErrEndMarker
		if !errors.As(err, &em) || em.Line != line {
			t.Errorf("%q: %v, want the end marker at line %d", data, err, line)
		}
	}
	for _, data := range []string{
		"a\n\"\\.\"\nb\n", "a\n x\\.\n", "a\n\\.x\n", "a\n\\\\.\n", "a\n .\\\n", "a\n\"\n\\.\n\"\n", "a,\\.\n", "a\n\\. \n", "\\\n.\n", "a\n\\x\n",
	} {
		if _, _, _, err := scanAll(data, '"', 0); err != nil {
			t.Errorf("%q: %v", data, err)
		}
	}
	// Another quote character moves what is quoted.
	if _, _, _, err := scanAll("a\n'\n\\.\n'\n", '\'', 0); err != nil {
		t.Errorf("quoted with ': %v", err)
	}
	if _, _, _, err := scanAll("a\n\"\n\\.\n\"\n", '\'', 0); err == nil {
		t.Error("with quote ' a \" does not quote")
	}
	// Once refused the scanner stays refused.
	s := NewScanner(strings.NewReader("\\.\nabc\n"), '"')
	_, _ = io.ReadAll(s)
	if _, err := s.Read(make([]byte, 4)); err == nil || s.Err() == nil {
		t.Error("the scanner went on after a refusal")
	}
}

// A record above 16 MiB fails with its number; a record of exactly 16 MiB passes; a very long record split across reads is counted right.
func TestScannerRecordLimit(t *testing.T) {
	line := func(n int) string { return strings.Repeat("x", n) }
	ok := "a\n" + line(MaxRecordBytes) + "\nb\n"
	if n, _, _, err := scanAll(ok, '"', 1<<20); err != nil || n != 3 {
		t.Errorf("a record of the limit: %d %v", n, err)
	}
	bad := "a\nb\n" + line(MaxRecordBytes+1) + "\n"
	_, _, _, err := scanAll(bad, '"', 1<<20)
	var tl *ErrRecordTooLarge
	if !errors.As(err, &tl) || tl.Line != 3 {
		t.Errorf("a record above the limit: %v", err)
	}
	// Inside quotes the limit counts too (a quoted field is part of the record), and a quote does not reset it.
	quoted := "\"" + line(MaxRecordBytes) + "\"\n"
	if _, _, _, err := scanAll(quoted, '"', 1<<20); !errors.As(err, &tl) {
		t.Errorf("a quoted record above the limit: %v", err)
	}
	// Many small records are not a long record.
	many := strings.Repeat("abcdefgh\n", 3<<20/9)
	if n, _, _, err := scanAll(many, '"', 4096); err != nil || n != int64(3<<20/9) {
		t.Errorf("many records: %d %v", n, err)
	}
	// The scanner holds no record: bytes in, bytes out, with a reader that gives one byte at a time.
	n, nb, _, err := scanAll("a,b\nc,d\n", '"', 1)
	if err != nil || n != 2 || nb != 8 {
		t.Errorf("%d %d %v", n, nb, err)
	}
}

// The pipeline pieces in order: a gzip stream is decompressed and counted against the limit, the BOM is removed.
func TestExpansionLimitAndBOM(t *testing.T) {
	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	_, _ = zw.Write(bytes.Repeat([]byte("0123456789"), 1000))
	_ = zw.Close()
	small := gz.Len()
	r, err := Gunzip(bytes.NewReader(gz.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	if b, err := io.ReadAll(LimitedExpansion(r, 10000)); err != nil || len(b) != 10000 {
		t.Errorf("exactly at the limit: %d %v", len(b), err)
	}
	r, _ = Gunzip(bytes.NewReader(gz.Bytes()))
	if b, err := io.ReadAll(LimitedExpansion(r, 9999)); !errors.Is(err, ErrExpansion) || len(b) != 9999 {
		t.Errorf("one byte above the limit: %d %v", len(b), err)
	}
	if small > 200 {
		t.Errorf("the test stream is not a bomb (%d bytes)", small)
	}
	// A stream that is not gzip, and a truncated one.
	if _, err := Gunzip(strings.NewReader("not gzip at all")); err == nil {
		t.Error("a plain stream was taken for gzip")
	}
	r, _ = Gunzip(bytes.NewReader(gz.Bytes()[:small/2]))
	if _, err := io.ReadAll(r); err == nil {
		t.Error("a truncated gzip stream ended without an error")
	}
	// The limit holds for reads of any size.
	r, _ = Gunzip(bytes.NewReader(gz.Bytes()))
	if _, err := io.ReadAll(iotest.OneByteReader(LimitedExpansion(r, 50))); !errors.Is(err, ErrExpansion) {
		t.Errorf("one byte reads: %v", err)
	}
	for _, tc := range []struct{ in, want string }{
		{"\xef\xbb\xbfa,b\n", "a,b\n"}, {"a,b\n", "a,b\n"}, {"", ""}, {"\xef\xbb", "\xef\xbb"}, {"\xef\xbb\xbf", ""}, {"\xef\xbb\xbf\xef\xbb\xbfx", "\xef\xbb\xbfx"}, {"ab", "ab"},
	} {
		got, err := io.ReadAll(StripBOM(iotest.OneByteReader(strings.NewReader(tc.in))))
		if err != nil || string(got) != tc.want {
			t.Errorf("StripBOM(%q) = %q, %v; want %q", tc.in, got, err, tc.want)
		}
	}
}

// FuzzScanner: whatever the bytes and however they are split, the scanner passes every byte through, never panics and, when it finishes
// without a refusal, counts what the reference counts.
func FuzzScanner(f *testing.F) {
	f.Add("a,b\nc,d\n", 3)
	f.Add("\"x\r\ny\"\r\n\\.\r\n", 1)
	f.Add("\\.", 2)
	f.Add("\r\r\n\n\"\"\"", 4)
	f.Fuzz(func(t *testing.T, data string, chunk int) {
		if chunk < 1 || chunk > 64 {
			chunk = 1 + (chunk%64+64)%64
		}
		wantN, wantMarker := reference(data, '"')
		n, nb, out, err := scanAll(data, '"', chunk)
		var em *ErrEndMarker
		switch {
		case errors.As(err, &em):
			if wantMarker != em.Line {
				t.Fatalf("%q: end marker at %d, reference %d", data, em.Line, wantMarker)
			}
		case err != nil:
			t.Fatalf("%q: %v", data, err)
		default:
			if wantMarker != 0 || n != wantN || nb != int64(len(data)) || out != data {
				t.Fatalf("%q chunk %d: %d records (reference %d, marker %d), %d bytes", data, chunk, n, wantN, wantMarker, nb)
			}
		}
	})
}
