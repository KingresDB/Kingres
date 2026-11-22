// SPDX-License-Identifier: AGPL-3.0-only

// Package relay is the streaming relay of the data transfer (design data-export-import.md, "The streaming relay"): the two operations
// that move a body of any size (uploadDataChunk and downloadDataExport) are not forwarded like the other operations, whose body is read
// whole and whose answer is buffered (32 MiB). A server that is not the manager leader opens one call to the leader whose body is a frame
// and then the raw bytes, and whose answer is a frame and then the raw bytes. Nothing is buffered beyond one segment on either server.
//
// The package holds what is the same on both sides: the frame, the allowlists of headers, the idle deadlines that replace the timeouts of the
// ordinary operations, and the copy loop. It knows nothing of HTTP routing, identities or the staging store.
package relay

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// MaxFrame is the largest frame, request or response (4-byte length, then JSON).
const MaxFrame = 64 << 10

// SegmentSize is the unit of the copy loop: a server holds no more than this much of a body at a time.
const SegmentSize = 64 << 10

// IdleTimeout is how long a stream may go without a byte, read or written. The deadline moves with every read and every write.
const IdleTimeout = 60 * time.Second

// ChunkTimeout is the longest an upload chunk may take.
const ChunkTimeout = 15 * time.Minute

// Errors of the framing.
var (
	ErrFrameTooLarge = errors.New("relay: the frame is larger than 64 KiB")
	ErrFrameShort    = errors.New("relay: the stream ended inside a frame")
	ErrFrameInvalid  = errors.New("relay: the frame is not valid")
)

// RequestHeaders are the headers of the public request that travel in the frame of the request: those the two operations use.
var RequestHeaders = []string{"Range", "Upload-Offset", "Content-Length", "Content-Type"}

// ResponseHeaders are the headers of the leader's answer that the follower passes on: the ones the ordinary forward relays and the ones of
// a resumable transfer and a download.
var ResponseHeaders = []string{
	"Content-Type", "Location", "Retry-After", "Allow", "Content-Language", "Preference-Applied", "Content-Disposition",
	"Content-Length", "Content-Range", "Accept-Ranges", "ETag", "Upload-Offset", "Cache-Control", "X-Content-Type-Options",
}

// Response is the frame of the answer: the status and the headers of the allowlist. The raw bytes follow it.
type Response struct {
	Status int               `json:"status"`
	Header map[string]string `json:"header,omitempty"`
}

// Pick returns the headers of h that are in the allowlist, one value each, without control characters. A value with a line break is
// dropped, not repaired: it could never have come from a well-formed peer.
func Pick(h http.Header, allow []string) map[string]string {
	out := map[string]string{}
	for _, k := range allow {
		v := h.Get(k)
		if v == "" || len(v) > 4096 || strings.ContainsAny(v, "\r\n\x00") {
			continue
		}
		out[http.CanonicalHeaderKey(k)] = v
	}
	return out
}

// Allowed reports whether a header name is in the allowlist.
func Allowed(name string, allow []string) bool {
	for _, k := range allow {
		if strings.EqualFold(k, name) {
			return true
		}
	}
	return false
}

// WriteFrame writes v as one frame: 4 bytes of length (big endian), then JSON.
func WriteFrame(w io.Writer, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	if len(b) > MaxFrame {
		return ErrFrameTooLarge
	}
	var n [4]byte
	binary.BigEndian.PutUint32(n[:], uint32(len(b)))
	_, err = w.Write(append(n[:], b...))
	return err
}

// EncodeFrame returns the bytes of a frame.
func EncodeFrame(v any) ([]byte, error) {
	var b bytes.Buffer
	if err := WriteFrame(&b, v); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

// ReadFrame reads one frame into v. The length is checked before anything is allocated, the JSON is decoded strictly (unknown members and
// trailing data are refused) and the reader is left at the first byte after the frame.
func ReadFrame(r io.Reader, v any) error {
	var n [4]byte
	if _, err := io.ReadFull(r, n[:]); err != nil {
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return ErrFrameShort
		}
		return err
	}
	size := binary.BigEndian.Uint32(n[:])
	if size == 0 || size > MaxFrame {
		if size == 0 {
			return fmt.Errorf("%w: empty", ErrFrameInvalid)
		}
		return ErrFrameTooLarge
	}
	buf := make([]byte, size)
	if _, err := io.ReadFull(r, buf); err != nil {
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return ErrFrameShort
		}
		return err
	}
	dec := json.NewDecoder(bytes.NewReader(buf))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("%w: %v", ErrFrameInvalid, err)
	}
	if dec.More() {
		return fmt.Errorf("%w: data after the value", ErrFrameInvalid)
	}
	return nil
}

// IdleBody wraps the body of a request so that the read and the write deadline of the connection move with every read: a stream that makes progress is
// never ended by the 30 s read timeout of the server, one that stops for IdleTimeout is.
type IdleBody struct {
	r    io.Reader
	rc   *http.ResponseController
	idle time.Duration
	// N counts the bytes read.
	N int64
}

// NewIdleBody wraps r. rc is the response controller of the request's own response writer (it reaches the connection).
func NewIdleBody(r io.Reader, rc *http.ResponseController, idle time.Duration) *IdleBody {
	b := &IdleBody{r: r, rc: rc, idle: idle}
	b.arm()
	return b
}

func (b *IdleBody) arm() {
	if b.rc != nil {
		// The write deadline moves with the reads too: the answer of an upload is written when its last byte was read, minutes after the
		// request began, and the write timeout of the server counts from the start of the request.
		// A connection that does not support deadlines (a wrapper) keeps the server's limits; the call then fails and is ignored.
		t := time.Now().Add(b.idle)
		_ = b.rc.SetReadDeadline(t)
		_ = b.rc.SetWriteDeadline(t)
	}
}

// Read implements io.Reader.
func (b *IdleBody) Read(p []byte) (int, error) {
	n, err := b.r.Read(p)
	b.N += int64(n)
	switch {
	case n > 0:
		b.arm()
	case err != nil && !errors.Is(err, io.EOF) && b.rc != nil:
		// The read ended with an error (the idle time ran out, the client went away): the write deadline is the same one and has run out
		// too, so give the answer that follows, the error of the request, a time of its own to be written.
		_ = b.rc.SetWriteDeadline(time.Now().Add(b.idle))
	}
	return n, err
}

// IdleWriter wraps a response writer so that the write deadline moves with every write, and flushes after each one, so that a download is
// sent as it is read.
type IdleWriter struct {
	w    io.Writer
	rc   *http.ResponseController
	idle time.Duration
	// N counts the bytes written.
	N int64
}

// NewIdleWriter wraps w; rc may be nil.
func NewIdleWriter(w io.Writer, rc *http.ResponseController, idle time.Duration) *IdleWriter {
	x := &IdleWriter{w: w, rc: rc, idle: idle}
	x.arm()
	return x
}

func (x *IdleWriter) arm() {
	if x.rc != nil {
		_ = x.rc.SetWriteDeadline(time.Now().Add(x.idle))
	}
}

// Write implements io.Writer.
func (x *IdleWriter) Write(p []byte) (int, error) {
	x.arm()
	n, err := x.w.Write(p)
	x.N += int64(n)
	if err == nil && x.rc != nil {
		err = x.rc.Flush()
		if errors.Is(err, http.ErrNotSupported) {
			err = nil
		}
	}
	return n, err
}

var segments = sync.Pool{New: func() any { b := make([]byte, SegmentSize); return &b }}

// Copy moves src to dst in segments of SegmentSize and returns the bytes moved. It never holds more than one segment. A short write or
// a read error ends it with that error.
func Copy(dst io.Writer, src io.Reader) (int64, error) {
	bp := segments.Get().(*[]byte)
	defer segments.Put(bp)
	var total int64
	buf := *bp
	for {
		n, rerr := src.Read(buf)
		if n > 0 {
			w, werr := dst.Write(buf[:n])
			total += int64(w)
			if werr != nil {
				return total, werr
			}
			if w != n {
				return total, io.ErrShortWrite
			}
		}
		if rerr != nil {
			if errors.Is(rerr, io.EOF) {
				return total, nil
			}
			return total, rerr
		}
	}
}

// Dial is what the client side needs from the transport: one POST to the leader's stream call with the body given. The peer client provides
// it over the mutual TLS connection; a test provides it over any HTTP client.
type Dial func(ctx context.Context, body io.Reader) (*http.Response, error)

// Call sends the frame and then body through dial and returns the frame of the answer and the reader of its raw bytes. The caller closes
// the reader. A transport error is returned as it is; an answer that is not a frame (the leader refused the call with a plain error, for
// instance not_leader) is returned with its status and the first bytes as the error text.
func Call(ctx context.Context, dial Dial, req any, body io.Reader) (Response, io.ReadCloser, error) {
	frame, err := EncodeFrame(req)
	if err != nil {
		return Response{}, nil, err
	}
	var rd io.Reader = bytes.NewReader(frame)
	if body != nil {
		rd = io.MultiReader(rd, body)
	}
	resp, err := dial(ctx, rd)
	if err != nil {
		return Response{}, nil, err
	}
	if resp.StatusCode != http.StatusOK {
		defer func() { _ = resp.Body.Close() }()
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return Response{}, nil, &CallError{Status: resp.StatusCode, Text: string(b)}
	}
	var out Response
	if err := ReadFrame(resp.Body, &out); err != nil {
		_ = resp.Body.Close()
		return Response{}, nil, err
	}
	if out.Status < 100 || out.Status > 599 {
		_ = resp.Body.Close()
		return Response{}, nil, fmt.Errorf("%w: status %d", ErrFrameInvalid, out.Status)
	}
	return out, resp.Body, nil
}

// CallError is an answer of the leader to the stream call that is not a frame: the call itself was refused.
type CallError struct {
	Status int
	// Text is the beginning of the answer; the leader's refusals are small JSON documents.
	Text string
}

func (e *CallError) Error() string {
	return fmt.Sprintf("relay: the leader refused the call with status %d", e.Status)
}

// ResponseWriter is the http.ResponseWriter the leader runs a relayed operation against: its first WriteHeader writes the frame of the
// answer to the internal call's response, and what follows is the raw body. An operation that wrote nothing is answered 200 by Finish.
type ResponseWriter struct {
	out    http.ResponseWriter
	header http.Header
	sent   bool
	status int
	// N counts the body bytes written.
	N int64
}

// NewResponseWriter makes the writer over the response of the internal call.
func NewResponseWriter(out http.ResponseWriter) *ResponseWriter {
	return &ResponseWriter{out: out, header: http.Header{}, status: http.StatusOK}
}

// Header implements http.ResponseWriter; the headers set before WriteHeader go into the frame (only those of the allowlist).
func (w *ResponseWriter) Header() http.Header { return w.header }

// WriteHeader implements http.ResponseWriter.
func (w *ResponseWriter) WriteHeader(code int) {
	if w.sent {
		return
	}
	w.sent = true
	w.status = code
	w.out.Header().Set("Content-Type", "application/octet-stream")
	w.out.Header().Set("Cache-Control", "no-store")
	w.out.WriteHeader(http.StatusOK)
	_ = WriteFrame(w.out, Response{Status: code, Header: Pick(w.header, ResponseHeaders)})
}

// Write implements http.ResponseWriter.
func (w *ResponseWriter) Write(p []byte) (int, error) {
	if !w.sent {
		w.WriteHeader(http.StatusOK)
	}
	n, err := w.out.Write(p)
	w.N += int64(n)
	return n, err
}

// Flush sends what was written; the frame goes out with the first flush, even before the first byte of the body.
func (w *ResponseWriter) Flush() {
	if f, ok := w.out.(http.Flusher); ok {
		f.Flush()
	}
}

// Unwrap lets http.ResponseController reach the connection.
func (w *ResponseWriter) Unwrap() http.ResponseWriter { return w.out }

// Finish writes the frame of a 200 answer when the operation wrote nothing.
func (w *ResponseWriter) Finish() {
	if !w.sent {
		w.WriteHeader(http.StatusOK)
	}
}

// Status is the status the operation answered.
func (w *ResponseWriter) Status() int { return w.status }

// Sent reports whether the frame of the answer has gone out (after that the status cannot change).
func (w *ResponseWriter) Sent() bool { return w.sent }
