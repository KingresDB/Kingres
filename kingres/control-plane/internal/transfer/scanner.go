// SPDX-License-Identifier: AGPL-3.0-only

package transfer

import (
	"compress/gzip"
	"errors"
	"fmt"
	"io"
)

// MaxRecordBytes is the largest record (one CSV row) the scanner lets through; a record of more is a failure with its number (design "Limits").
const MaxRecordBytes = 16 << 20

// ErrRecordTooLarge is returned by the scanner for a record above MaxRecordBytes. Line is the number of the record.
type ErrRecordTooLarge struct{ Line int64 }

func (e *ErrRecordTooLarge) Error() string {
	return fmt.Sprintf("a record is larger than 16 MiB (line %d)", e.Line)
}

// ErrEndMarker is returned by the scanner for an unquoted line that is only a backslash and a dot: the console refuses such a file (422
// import_end_marker). On PostgreSQL 18 and 19 the line is data in CSV (spike S2); the refusal is the conservative choice of the first
// release and is dropped only when the same test passes on every supported major.
type ErrEndMarker struct{ Line int64 }

func (e *ErrEndMarker) Error() string {
	return fmt.Sprintf("the file has a line that is only backslash and dot outside quotes (line %d)", e.Line)
}

// Scanner is an io.Reader that passes every byte of the CSV through and knows where a record ends. PostgreSQL parses the CSV; the scanner
// tracks the quote state only, so that a line break inside quotes does not end a record, counts records and bytes, enforces the limit of one
// record and refuses an unquoted end-marker line. It holds a few bytes of state and no record: memory does not depend on the size of the file.
//
// The end of a record is found as PostgreSQL does: the style of the line break is the one of the first unquoted line break of the file (LF,
// CR LF or CR), and only that style ends a record afterwards. A file that mixes styles is refused by PostgreSQL, not counted differently here.
type Scanner struct {
	r     io.Reader
	quote byte

	err     error
	records int64
	bytes   int64
	// recLen is the length of the record so far; inQuote the quote state.
	recLen  int64
	inQuote bool
	// eol is 0 until the first unquoted line break: LF, CR (alone) or 'R' (CR LF).
	eol byte
	// pendingCR is true after an unquoted CR that may be the first half of CR LF.
	pendingCR bool
	// marker follows the end marker at the start of a record: 0 nothing, 1 saw the backslash, 2 saw backslash and dot (the record is only that so far).
	marker int
}

// NewScanner wraps r. quote is the quote character of the file (one byte).
func NewScanner(r io.Reader, quote byte) *Scanner { return &Scanner{r: r, quote: quote} }

// Records is the number of records of the stream: those that ended, and a last one that has bytes and no line break. Call it after the
// reader returned io.EOF.
func (s *Scanner) Records() int64 {
	if s.recLen > 0 {
		return s.records + 1
	}
	return s.records
}

// Bytes is the number of bytes that went through.
func (s *Scanner) Bytes() int64 { return s.bytes }

// Err is the first failure of the scanner itself (not of the reader).
func (s *Scanner) Err() error { return s.err }

// Read implements io.Reader.
func (s *Scanner) Read(p []byte) (int, error) {
	if s.err != nil {
		return 0, s.err
	}
	n, err := s.r.Read(p)
	if n > 0 {
		if serr := s.scan(p[:n]); serr != nil {
			s.err = serr
			return 0, serr
		}
		s.bytes += int64(n)
	}
	if err == io.EOF {
		if serr := s.eof(); serr != nil {
			s.err = serr
			return n, serr
		}
	}
	return n, err
}

// eof settles what the end of the stream leaves open: a CR that waited for its LF, and a last record that is only the end marker.
func (s *Scanner) eof() error {
	if s.pendingCR {
		s.pendingCR = false
		if s.eol == 0 || s.eol == '\r' {
			s.eol = '\r'
			return s.finish()
		}
		// A CR at the end of a file of another style is data.
		if err := s.data('\r'); err != nil {
			return err
		}
	}
	if s.marker == 2 && !s.inQuote {
		return &ErrEndMarker{Line: s.records + 1}
	}
	return nil
}

// finish ends a record at an unquoted line break. A record that is only the end marker is refused.
func (s *Scanner) finish() error {
	if s.marker == 2 {
		return &ErrEndMarker{Line: s.records + 1}
	}
	s.records++
	s.recLen = 0
	s.marker = 0
	return nil
}

// data adds one byte of data to the record.
func (s *Scanner) data(c byte) error {
	s.recLen++
	if s.recLen > MaxRecordBytes {
		return &ErrRecordTooLarge{Line: s.records + 1}
	}
	switch {
	case s.marker == 0 && s.recLen == 1 && c == '\\':
		s.marker = 1
	case s.marker == 1 && s.recLen == 2 && c == '.':
		s.marker = 2
	default:
		s.marker = 0
	}
	return nil
}

func (s *Scanner) scan(b []byte) error {
	for _, c := range b {
		if s.pendingCR {
			s.pendingCR = false
			switch {
			case c == '\n' && s.eol != '\r':
				// CR LF: the record ends here; the LF belongs to the line break.
				s.eol = 'R'
				if err := s.finish(); err != nil {
					return err
				}
				continue
			case s.eol == 0 || s.eol == '\r':
				// A CR that is not followed by a LF ends the record in a file whose line breaks are CR.
				s.eol = '\r'
				if err := s.finish(); err != nil {
					return err
				}
			default:
				// A CR in a file of another style: data (PostgreSQL refuses such a file).
				if err := s.data('\r'); err != nil {
					return err
				}
			}
		}
		switch {
		case c == s.quote:
			s.inQuote = !s.inQuote
			if err := s.data(c); err != nil {
				return err
			}
			s.marker = 0
		case s.inQuote:
			if err := s.data(c); err != nil {
				return err
			}
			s.marker = 0
		case c == '\n' && (s.eol == 0 || s.eol == '\n'):
			s.eol = '\n'
			if err := s.finish(); err != nil {
				return err
			}
		case c == '\r' && s.eol != '\n':
			s.pendingCR = true
		default:
			if err := s.data(c); err != nil {
				return err
			}
		}
	}
	return nil
}

// ErrExpansion is returned by the limited reader when the decompressed stream is larger than the limit (a gzip bomb).
var ErrExpansion = errors.New("the decompressed file is larger than the limit")

// LimitedExpansion returns a reader of at most max bytes of r; one more byte is ErrExpansion, not a silent end.
func LimitedExpansion(r io.Reader, max int64) io.Reader { return &expansion{r: r, left: max} }

type expansion struct {
	r    io.Reader
	left int64
}

func (e *expansion) Read(p []byte) (int, error) {
	if e.left < 0 {
		return 0, ErrExpansion
	}
	if int64(len(p)) > e.left+1 {
		p = p[:e.left+1]
	}
	n, err := e.r.Read(p)
	if int64(n) > e.left {
		n = int(e.left)
		e.left = -1
		return n, ErrExpansion
	}
	e.left -= int64(n)
	return n, err
}

// Gunzip returns a reader of the decompressed stream of r; a stream that is not gzip is an error here.
func Gunzip(r io.Reader) (io.Reader, error) {
	zr, err := gzip.NewReader(r)
	if err != nil {
		return nil, err
	}
	return zr, nil
}

// StripBOM returns a reader of r without a UTF-8 byte order mark at its start. It reads at most three bytes ahead.
func StripBOM(r io.Reader) io.Reader { return &bomReader{r: r} }

type bomReader struct {
	r       io.Reader
	checked bool
	head    []byte
}

func (b *bomReader) Read(p []byte) (int, error) {
	if !b.checked {
		b.checked = true
		buf := make([]byte, 3)
		n, err := io.ReadFull(b.r, buf)
		buf = buf[:n]
		if !(n == 3 && buf[0] == 0xEF && buf[1] == 0xBB && buf[2] == 0xBF) {
			b.head = buf
		}
		if err != nil && err != io.EOF && err != io.ErrUnexpectedEOF {
			return 0, err
		}
	}
	if len(b.head) > 0 {
		n := copy(p, b.head)
		b.head = b.head[n:]
		return n, nil
	}
	return b.r.Read(p)
}
