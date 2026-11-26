// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package ledgerkeys

import (
	"bytes"
	"crypto/subtle"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"runtime"
	"sync"
)

// redacted is what every type that holds a secret prints, whatever the verb.
const redacted = "[REDACTED]"

// Limits of a passphrase.
const (
	// MaxPassphraseSize is the longest passphrase accepted, in bytes.
	MaxPassphraseSize = 1024
	// MaxPassphraseFileSize is the largest passphrase file read: the longest
	// passphrase and a line ending, with room for a stray second line to be
	// noticed and refused.
	MaxPassphraseFileSize = MaxPassphraseSize + 2
)

// secretBytes is a buffer that holds a secret and that can be zeroed. Every
// type of this package that holds one wraps it and implements fmt.Formatter,
// fmt.Stringer, fmt.GoStringer, json.Marshaler, encoding.TextMarshaler and
// slog.LogValuer so that no formatting verb and no encoder prints the content.
type secretBytes struct {
	mu sync.RWMutex
	b  []byte
}

func newSecretBytes(b []byte) *secretBytes {
	return &secretBytes{b: append([]byte(nil), b...)}
}

// use runs f with the content. It returns ErrDestroyed after destroy.
func (s *secretBytes) use(f func(b []byte) error) error {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.b == nil {
		return ErrDestroyed
	}
	return f(s.b)
}

func (s *secretBytes) destroy() {
	s.mu.Lock()
	defer s.mu.Unlock()
	clear(s.b)
	s.b = nil
}

func (s *secretBytes) equal(o []byte) bool {
	ok := false
	_ = s.use(func(b []byte) error {
		ok = subtle.ConstantTimeCompare(b, o) == 1
		return nil
	})
	return ok
}

// ErrDestroyed is returned when a secret is used after Destroy or Close.
var ErrDestroyed = errors.New("ledgerkeys: the secret was destroyed")

// Passphrase is a passphrase that protects a key file. It never prints its
// content (any verb, JSON, text, structured log) and is zeroed by Destroy. A
// Passphrase is safe for concurrent use.
type Passphrase struct{ s *secretBytes }

// NewPassphrase copies b into a Passphrase. The caller should zero b. The
// passphrase must be 1 to MaxPassphraseSize bytes.
func NewPassphrase(b []byte) (Passphrase, error) {
	if len(b) == 0 {
		return Passphrase{}, errors.New("ledgerkeys: the passphrase is empty")
	}
	if len(b) > MaxPassphraseSize {
		return Passphrase{}, fmt.Errorf("ledgerkeys: the passphrase has %d bytes, expected at most %d", len(b), MaxPassphraseSize)
	}
	return Passphrase{s: newSecretBytes(b)}, nil
}

// ReadPassphraseFile reads a passphrase file (design 3.2.2): a regular file,
// not a link, of at most MaxPassphraseFileSize bytes, readable by its owner
// only on systems that have modes, whose content is the passphrase followed
// by at most one line ending. It does not keep the content beyond the
// returned Passphrase; the buffer it read into is zeroed.
func ReadPassphraseFile(path string) (Passphrase, error) {
	data, err := readPrivateFile(path, MaxPassphraseFileSize)
	if err != nil {
		return Passphrase{}, err
	}
	defer clear(data)
	switch {
	case bytes.HasSuffix(data, []byte("\r\n")):
		data = data[:len(data)-2]
	case bytes.HasSuffix(data, []byte("\n")):
		data = data[:len(data)-1]
	}
	if bytes.ContainsAny(data, "\r\n") {
		return Passphrase{}, errors.New("ledgerkeys: the passphrase file has more than one line")
	}
	return NewPassphrase(data)
}

// Destroy zeroes the passphrase. Using it afterwards gives ErrDestroyed.
func (p Passphrase) Destroy() {
	if p.s != nil {
		p.s.destroy()
	}
}

// IsSet reports whether the passphrase holds a value.
func (p Passphrase) IsSet() bool {
	if p.s == nil {
		return false
	}
	return p.s.use(func([]byte) error { return nil }) == nil
}

// use runs f with the passphrase bytes.
func (p Passphrase) use(f func(b []byte) error) error {
	if p.s == nil {
		return errors.New("ledgerkeys: no passphrase was given")
	}
	return p.s.use(f)
}

// Equal compares two passphrases in constant time.
func (p Passphrase) Equal(o Passphrase) bool {
	if p.s == nil || o.s == nil {
		return false
	}
	ok := false
	_ = o.s.use(func(ob []byte) error {
		ok = p.s.equal(ob)
		return nil
	})
	return ok
}

// Format implements fmt.Formatter: every verb prints the same marker.
func (Passphrase) Format(f fmt.State, _ rune) { _, _ = f.Write([]byte("Passphrase(" + redacted + ")")) }

// String implements fmt.Stringer.
func (Passphrase) String() string { return "Passphrase(" + redacted + ")" }

// GoString implements fmt.GoStringer.
func (Passphrase) GoString() string { return "ledgerkeys.Passphrase(" + redacted + ")" }

// MarshalJSON implements json.Marshaler.
func (Passphrase) MarshalJSON() ([]byte, error) { return []byte(`"` + redacted + `"`), nil }

// MarshalText implements encoding.TextMarshaler.
func (Passphrase) MarshalText() ([]byte, error) { return []byte(redacted), nil }

// LogValue implements slog.LogValuer.
func (Passphrase) LogValue() slog.Value { return slog.StringValue(redacted) }

// afterLstat, when set, is called between the Lstat and the open of
// readPrivateFile (a hook for tests of a file that is swapped in between).
var afterLstat func(path string)

// readPrivateFile reads a regular file that is not a link, at most limit
// bytes, and (where the system has modes) that no group or other user can
// access. The error never holds file content.
func readPrivateFile(path string, limit int64) ([]byte, error) {
	li, err := os.Lstat(path)
	if err != nil {
		return nil, fmt.Errorf("ledgerkeys: %w", pathErr(err))
	}
	if !li.Mode().IsRegular() {
		return nil, fmt.Errorf("ledgerkeys: %s is not a regular file (a link, directory or special file is refused)", path)
	}
	if afterLstat != nil {
		afterLstat(path)
	}
	f, err := os.Open(path) // #nosec G304 -- the caller names the file
	if err != nil {
		return nil, fmt.Errorf("ledgerkeys: %w", pathErr(err))
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("ledgerkeys: %w", pathErr(err))
	}
	if !fi.Mode().IsRegular() || !os.SameFile(li, fi) {
		return nil, fmt.Errorf("ledgerkeys: %s changed while it was opened", path)
	}
	if runtime.GOOS != "windows" && fi.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("ledgerkeys: %s has mode %04o, expected 0600: group and others must have no access", path, fi.Mode().Perm())
	}
	if fi.Size() > limit {
		return nil, fmt.Errorf("ledgerkeys: %s has %d bytes, expected at most %d", path, fi.Size(), limit)
	}
	buf := make([]byte, limit+1)
	n, err := readFull(f, buf)
	if err != nil {
		clear(buf)
		return nil, fmt.Errorf("ledgerkeys: reading %s: %w", path, pathErr(err))
	}
	if int64(n) > limit {
		clear(buf)
		return nil, fmt.Errorf("ledgerkeys: %s has more than %d bytes", path, limit)
	}
	out := append([]byte(nil), buf[:n]...)
	clear(buf)
	return out, nil
}

func readFull(f *os.File, buf []byte) (int, error) {
	n := 0
	for n < len(buf) {
		m, err := f.Read(buf[n:])
		n += m
		if err != nil {
			if errors.Is(err, io.EOF) {
				return n, nil
			}
			return n, err
		}
	}
	return n, nil
}

// pathErr strips the path out of an *os.PathError so that the caller adds it
// once; it never touches content.
func pathErr(err error) error {
	var pe *os.PathError
	if errors.As(err, &pe) {
		return fmt.Errorf("%s %s: %w", pe.Op, pe.Path, pe.Err)
	}
	return err
}

// DeriveKey returns a 32 byte key from the passphrase with PBKDF2-HMAC-SHA256 (the derivation of the key
// files), for a sealed file of another kind that uses the same passphrase file (the credentials of an anchor
// target, design 6.8). The caller zeroes the result. The salt must be at least SaltSize bytes and the
// iteration count at least 1000; a file that is read must refuse a count below Iterations before it calls
// this, as Parse does for a key file.
func (p Passphrase) DeriveKey(salt []byte, iterations int) ([]byte, error) {
	if len(salt) < SaltSize || iterations < 1000 {
		return nil, errors.New("ledgerkeys: the salt or the iteration count is too small")
	}
	var key []byte
	err := p.use(func(pw []byte) error {
		key = pbkdf2(pw, salt, iterations)
		return nil
	})
	return key, err
}
