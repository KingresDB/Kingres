// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package ledgerkeys

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ed25519"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
)

// The key file (design 3.2.2): the line KINGRES-KEY-1, then one line of
// compact JSON, in this exact form (members in this order, standard base64
// with padding, a final newline):
//
//	KINGRES-KEY-1
//	{"seed":"<b64 of 32 bytes>"}
//
// or, with a passphrase,
//
//	KINGRES-KEY-1
//	{"kdf":"pbkdf2-hmac-sha256","iterations":600000,"salt":"<16 bytes>","nonce":"<12 bytes>","ciphertext":"<48 bytes>"}
//
// where ciphertext is the seed sealed with AES-256-GCM under the key
// PBKDF2-HMAC-SHA256(passphrase, salt, iterations, 32 bytes). The additional
// authenticated data is the header: the first line with its newline, the
// KDF name, a zero byte, the iteration count as a big endian u32, and the
// salt, so that none of the parameters can be changed without the open
// failing. A file that is not byte for byte in this form is refused.
const (
	// FileMagic is the first line of a key file.
	FileMagic = "KINGRES-KEY-1"
	// KDFName is the only key derivation function.
	KDFName = "pbkdf2-hmac-sha256"
	// Iterations is the PBKDF2 count that is written, and the least that is
	// read: a file with fewer is refused so that a weakened file is noticed.
	Iterations = 600000
	// MaxIterations is the most a file may ask for, so that a hostile file
	// cannot make a start hang.
	MaxIterations = 6000000
	// SaltSize and NonceSize are the sizes of the salt and the AES-GCM nonce.
	SaltSize  = 16
	NonceSize = 12
	// SeedSize is the size of the Ed25519 seed.
	SeedSize = ed25519.SeedSize
	// sealedSize is the seed with the GCM tag.
	sealedSize = SeedSize + 16
	// MaxFileSize is the most a key file may hold, read before parsing. A file
	// of the canonical form has under 300 bytes.
	MaxFileSize = 1024
	// FileMode is the mode of a key file.
	FileMode os.FileMode = 0o600
)

// minIterations is the lower bound on reading; tests lower it to keep the run
// short, nothing else changes it.
var minIterations uint32 = Iterations

// writeIterations is the count Encode writes; tests lower it with minIterations.
var writeIterations uint32 = Iterations

// Errors of key files. None carries content of a file.
var (
	// ErrMalformed marks a key file that is not in the canonical form.
	ErrMalformed = errors.New("ledgerkeys: the key file is malformed")
	// ErrWrongPassphrase is returned when the seed does not open: the
	// passphrase is wrong or the file was changed (the two cannot be told apart).
	ErrWrongPassphrase = errors.New("ledgerkeys: the passphrase is wrong or the key file was changed")
	// ErrPassphraseRequired is returned for an encrypted file opened with no
	// passphrase, and ErrPassphraseNotNeeded for a plain file opened with one.
	ErrPassphraseRequired  = errors.New("ledgerkeys: the key file is encrypted and needs a passphrase")
	ErrPassphraseNotNeeded = errors.New("ledgerkeys: the key file is not encrypted, a passphrase was given")
)

func malformed(why string) error { return fmt.Errorf("%w: %s", ErrMalformed, why) }

// File is a parsed key file: the envelope, not yet opened. A plain file holds
// the seed, so a File never prints, encodes or returns its content.
type File struct {
	encrypted  bool
	seed       *secretBytes // plain files only
	iterations uint32
	salt       [SaltSize]byte
	nonce      [NonceSize]byte
	sealed     [sealedSize]byte
}

// Encrypted reports whether the file is protected by a passphrase.
func (f File) Encrypted() bool { return f.encrypted }

// Iterations returns the PBKDF2 count of an encrypted file (0 for a plain one).
func (f File) Iterations() uint32 { return f.iterations }

// Destroy zeroes the seed of a plain file.
func (f File) Destroy() {
	if f.seed != nil {
		f.seed.destroy()
	}
}

func (File) label() string { return "File(" + redacted + ")" }

// Format implements fmt.Formatter.
func (f File) Format(s fmt.State, _ rune) { _, _ = s.Write([]byte(f.label())) }

// String implements fmt.Stringer.
func (f File) String() string { return f.label() }

// GoString implements fmt.GoStringer.
func (f File) GoString() string { return "ledgerkeys." + f.label() }

// MarshalJSON implements json.Marshaler.
func (f File) MarshalJSON() ([]byte, error) { return []byte(`"` + f.label() + `"`), nil }

// MarshalText implements encoding.TextMarshaler.
func (f File) MarshalText() ([]byte, error) { return []byte(f.label()), nil }

// header is the authenticated header of an encrypted file.
func header(iterations uint32, salt []byte) []byte {
	h := []byte(FileMagic + "\n" + KDFName)
	h = append(h, 0)
	h = binary.BigEndian.AppendUint32(h, iterations)
	return append(h, salt...)
}

// pbkdf2 is PBKDF2-HMAC-SHA256 for a 32 byte key (one block). It is written
// with crypto/hmac so that the passphrase stays in a byte slice the caller can
// zero (crypto/pbkdf2 takes a string, which cannot be zeroed); a test checks
// it against crypto/pbkdf2 and the RFC 7914 vector.
func pbkdf2(password, salt []byte, iterations int) []byte {
	prf := hmac.New(sha256.New, password)
	prf.Write(salt)
	prf.Write([]byte{0, 0, 0, 1})
	u := prf.Sum(nil)
	t := append([]byte(nil), u...)
	for i := 1; i < iterations; i++ {
		prf.Reset()
		prf.Write(u)
		u = prf.Sum(u[:0])
		for j := range t {
			t[j] ^= u[j]
		}
	}
	clear(u)
	return t
}

func deriveAEAD(p Passphrase, iterations uint32, salt []byte) (cipher.AEAD, error) {
	var key []byte
	err := p.use(func(pw []byte) error {
		key = pbkdf2(pw, salt, int(iterations))
		return nil
	})
	if err != nil {
		return nil, err
	}
	defer clear(key)
	blk, err := aes.NewCipher(key)
	if err != nil {
		return nil, errors.New("ledgerkeys: cannot set up the cipher")
	}
	return cipher.NewGCM(blk)
}

// Encode writes the key file for s. With a passphrase that IsSet the seed is
// sealed; with the zero Passphrase it is written in clear (design 3.2.2: as
// ca.key is, and the console says so). r supplies the salt and nonce
// (crypto/rand when nil).
func Encode(s FileSigner, pass Passphrase, r io.Reader) ([]byte, error) {
	if r == nil {
		r = rand.Reader
	}
	seed, err := s.seedBytes()
	if err != nil {
		return nil, err
	}
	defer clear(seed)
	if pass.s == nil {
		return marshalPlain(seed), nil
	}
	f := File{encrypted: true, iterations: writeIterations}
	if _, err := io.ReadFull(r, f.salt[:]); err != nil {
		return nil, errors.New("ledgerkeys: cannot read random bytes for a key file")
	}
	if _, err := io.ReadFull(r, f.nonce[:]); err != nil {
		return nil, errors.New("ledgerkeys: cannot read random bytes for a key file")
	}
	aead, err := deriveAEAD(pass, f.iterations, f.salt[:])
	if err != nil {
		return nil, err
	}
	copy(f.sealed[:], aead.Seal(nil, f.nonce[:], seed, header(f.iterations, f.salt[:])))
	return f.marshal(), nil
}

// plainHead starts the second line of a plain file.
const plainHead = `{"seed":"`

// marshalPlain writes the plain file into a byte slice, so that no string holds
// the base64 text of the seed. The caller zeroes the result.
func marshalPlain(seed []byte) []byte {
	enc := base64.StdEncoding
	out := make([]byte, 0, len(FileMagic)+1+len(plainHead)+enc.EncodedLen(len(seed))+3)
	out = append(out, FileMagic+"\n"+plainHead...)
	n := len(out)
	out = out[:n+enc.EncodedLen(len(seed))]
	enc.Encode(out[n:], seed)
	return append(out, `"}`+"\n"...)
}

// parsePlainDone is a seam for the tests: when set, parsePlain calls it with its
// two working buffers (the decoded seed and its base64 text) once they have been
// cleared, so that a test sees whether they were. It is nil in the program.
var parsePlainDone func(seed, again []byte)

// parsePlain reads the rest of a plain file after plainHead: exactly 44 base64
// characters, the closing quote, brace and newline. It works on byte slices
// that it zeroes, so no string holds the seed.
func parsePlain(rest []byte) (*secretBytes, error) {
	enc := base64.StdEncoding
	if len(rest) != enc.EncodedLen(SeedSize)+3 || string(rest[len(rest)-3:]) != `"}`+"\n" {
		return nil, malformed("it is not in the canonical form (spaces, line endings or text after the object)")
	}
	var seed, again []byte
	if hook := parsePlainDone; hook != nil {
		// Registered first, so it runs after the buffers are cleared.
		defer func() { hook(seed, again) }()
	}
	// n is taken from the length so that the buffers below are made on the
	// heap, where they are zeroed once, and not on a stack, whose old copies
	// the runtime leaves behind when it grows or shrinks the stack.
	n := len(rest) - 3
	seed = make([]byte, enc.DecodedLen(n))
	defer clear(seed)
	m, err := enc.Strict().Decode(seed, rest[:n])
	if err != nil || m != SeedSize {
		return nil, malformed(fmt.Sprintf("seed is not canonical base64 of %d bytes", SeedSize))
	}
	again = make([]byte, n)
	defer clear(again)
	enc.Encode(again, seed[:m])
	if !bytes.Equal(again, rest[:n]) {
		return nil, malformed(fmt.Sprintf("seed is not canonical base64 of %d bytes", SeedSize))
	}
	return newSecretBytes(seed[:m]), nil
}

func (f File) marshal() []byte {
	if !f.encrypted {
		var out []byte
		_ = f.seed.use(func(b []byte) error { out = marshalPlain(b); return nil })
		return out
	}
	e := base64.StdEncoding.EncodeToString
	return []byte(FileMagic + "\n" +
		`{"kdf":"` + KDFName + `","iterations":` + strconv.FormatUint(uint64(f.iterations), 10) +
		`,"salt":"` + e(f.salt[:]) + `","nonce":"` + e(f.nonce[:]) + `","ciphertext":"` + e(f.sealed[:]) + `"}` + "\n")
}

// Marshal writes the file in its canonical form. For a plain file this
// contains the seed.
func (f File) Marshal() []byte { return f.marshal() }

// Parse parses a key file. It refuses everything that is not the canonical
// form: a wrong magic, a different member order, unknown or duplicate or
// missing members, whitespace, non-canonical base64, a wrong size of salt,
// nonce or ciphertext, an iteration count outside Iterations to MaxIterations,
// another KDF, trailing bytes, a file over MaxFileSize. The error says what
// is wrong and never repeats the content.
func Parse(data []byte) (File, error) {
	var f File
	if len(data) > MaxFileSize {
		return f, malformed(fmt.Sprintf("it has %d bytes, expected at most %d", len(data), MaxFileSize))
	}
	rest, ok := bytes.CutPrefix(data, []byte(FileMagic+"\n"))
	if !ok {
		return f, malformed("the first line is not " + FileMagic)
	}
	if plain, ok := bytes.CutPrefix(rest, []byte(plainHead)); ok {
		seed, err := parsePlain(plain)
		if err != nil {
			return f, err
		}
		f.seed = seed
		return f, nil
	}
	// Only an encrypted file is left, and it holds no secret, so its members
	// may be read with encoding/json. A file that starts any other way is not
	// in the canonical form and is refused before a decoder could copy a seed
	// into a string.
	if !bytes.HasPrefix(rest, []byte(`{"kdf":`)) {
		return f, malformed("the second line is not the object of a plain or of an encrypted file")
	}
	members, err := parseObject(rest)
	if err != nil {
		return f, err
	}
	switch len(members) {
	case 5:
		want := [...]string{"kdf", "iterations", "salt", "nonce", "ciphertext"}
		for i, m := range members {
			if m.key != want[i] {
				return f, malformed(`the members of an encrypted file are kdf, iterations, salt, nonce, ciphertext, in this order`)
			}
			if (i == 1) == m.isString {
				return f, malformed(fmt.Sprintf("the member %q has the wrong type", m.key))
			}
		}
		if members[0].str != KDFName {
			return f, malformed("the kdf is not " + KDFName)
		}
		it, err := strconv.ParseUint(members[1].num, 10, 32)
		if err != nil || strconv.FormatUint(it, 10) != members[1].num {
			return f, malformed("iterations is not a canonical number")
		}
		if it < uint64(minIterations) || it > MaxIterations {
			return f, malformed(fmt.Sprintf("iterations is %d, expected %d to %d", it, minIterations, MaxIterations))
		}
		f.encrypted, f.iterations = true, uint32(it)
		for _, c := range []struct {
			m    member
			dst  []byte
			name string
		}{{members[2], f.salt[:], "salt"}, {members[3], f.nonce[:], "nonce"}, {members[4], f.sealed[:], "ciphertext"}} {
			b, err := b64Exact(c.m.str, len(c.dst), c.name)
			if err != nil {
				return f, err
			}
			copy(c.dst, b)
		}
	default:
		return f, malformed("expected the members of a plain or of an encrypted file")
	}
	if !bytes.Equal(f.marshal(), data) {
		f.Destroy()
		return File{}, malformed("it is not in the canonical form (spaces, line endings or text after the object)")
	}
	return f, nil
}

type member struct {
	key      string
	isString bool
	str      string
	num      string
}

// parseObject reads one JSON object of string and number members. It
// refuses duplicate keys and any other value type, and does not echo values.
func parseObject(b []byte) ([]member, error) {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	tok, err := dec.Token()
	if err != nil || tok != json.Delim('{') {
		return nil, malformed("the second line is not a JSON object")
	}
	var out []member
	seen := map[string]bool{}
	for dec.More() {
		kt, err := dec.Token()
		k, ok := kt.(string)
		if err != nil || !ok {
			return nil, malformed("the JSON object is not well formed")
		}
		if seen[k] {
			return nil, malformed(fmt.Sprintf("the member %q appears twice", clip(k)))
		}
		seen[k] = true
		if len(out) >= 5 {
			return nil, malformed("the JSON object has too many members")
		}
		vt, err := dec.Token()
		if err != nil {
			return nil, malformed("the JSON object is not well formed")
		}
		switch v := vt.(type) {
		case string:
			out = append(out, member{key: k, isString: true, str: v})
		case json.Number:
			out = append(out, member{key: k, num: v.String()})
		default:
			return nil, malformed(fmt.Sprintf("the member %q is neither a string nor a number", clip(k)))
		}
	}
	if tok, err := dec.Token(); err != nil || tok != json.Delim('}') {
		return nil, malformed("the JSON object is not well formed")
	}
	return out, nil
}

func b64Exact(s string, n int, name string) ([]byte, error) {
	b, err := base64.StdEncoding.Strict().DecodeString(s)
	if err != nil || len(b) != n || base64.StdEncoding.EncodeToString(b) != s {
		clear(b)
		return nil, malformed(fmt.Sprintf("%s is not canonical base64 of %d bytes", name, n))
	}
	return b, nil
}

// Open returns the signer of the file. A plain file needs the zero
// Passphrase, an encrypted one a Passphrase that IsSet.
func (f File) Open(pass Passphrase) (*FileSigner, error) {
	if !f.encrypted {
		if pass.s != nil {
			return nil, ErrPassphraseNotNeeded
		}
		var s *FileSigner
		err := f.seed.use(func(b []byte) error {
			var err error
			s, err = NewFileSigner(b)
			return err
		})
		return s, err
	}
	if pass.s == nil {
		return nil, ErrPassphraseRequired
	}
	aead, err := deriveAEAD(pass, f.iterations, f.salt[:])
	if err != nil {
		return nil, err
	}
	seed, err := aead.Open(nil, f.nonce[:], f.sealed[:], header(f.iterations, f.salt[:]))
	if err != nil {
		return nil, ErrWrongPassphrase
	}
	defer clear(seed)
	return NewFileSigner(seed)
}

// Decode parses and opens a key file in one step.
func Decode(data []byte, pass Passphrase) (*FileSigner, error) {
	f, err := Parse(data)
	if err != nil {
		return nil, err
	}
	defer f.Destroy()
	return f.Open(pass)
}

// FileName is the name of the key file of a public key below the signing
// directory: <fingerprint hex>.key (design 3.2.2).
func FileName(pub ed25519.PublicKey) string { return Fingerprint(pub) + ".key" }

// ReadFile reads and opens a key file. The file must be a regular file, not a
// link, at most MaxFileSize bytes, and on systems with modes readable by its
// owner only.
func ReadFile(path string, pass Passphrase) (*FileSigner, error) {
	data, err := readPrivateFile(path, MaxFileSize)
	if err != nil {
		return nil, err
	}
	defer clear(data)
	s, err := Decode(data, pass)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return s, nil
}

// ReadNamed reads <dir>/<fingerprint>.key and checks that the key in it has
// that fingerprint, so a file moved or copied under another name is noticed.
func ReadNamed(dir, fingerprint string, pass Passphrase) (*FileSigner, error) {
	s, err := ReadFile(filepath.Join(dir, fingerprint+".key"), pass)
	if err != nil {
		return nil, err
	}
	if s.Fingerprint() != fingerprint {
		_ = s.Close()
		return nil, fmt.Errorf("ledgerkeys: the key in %s.key has another fingerprint than its name says", fingerprint)
	}
	return s, nil
}

// WriteFile writes the key file of s to path atomically and durably with
// mode 0600: the content is written to a temporary file in the same
// directory, flushed, and renamed into place, so a crash leaves the old file
// or the new one, never part of one. With replace false it fails when path
// exists (a key file is named by its fingerprint and is not overwritten by
// mistake); with replace true it replaces the file (a change of passphrase).
// The directory must exist.
func WriteFile(path string, s FileSigner, pass Passphrase, replace bool) error {
	data, err := Encode(s, pass, nil)
	if err != nil {
		return err
	}
	defer clear(data)
	return writeAtomic(path, data, replace)
}

func writeAtomic(path string, data []byte, replace bool) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".key-*")
	if err != nil {
		return fmt.Errorf("ledgerkeys: %w", pathErr(err))
	}
	name := tmp.Name()
	fail := func(err error) error {
		_ = tmp.Close()
		_ = os.Remove(name)
		return fmt.Errorf("ledgerkeys: %w", pathErr(err))
	}
	if runtime.GOOS != "windows" {
		if err := tmp.Chmod(FileMode); err != nil {
			return fail(err)
		}
	}
	if _, err := tmp.Write(data); err != nil {
		return fail(err)
	}
	if err := tmp.Sync(); err != nil {
		return fail(err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(name)
		return fmt.Errorf("ledgerkeys: %w", pathErr(err))
	}
	if replace {
		if err := os.Rename(name, path); err != nil {
			_ = os.Remove(name)
			return fmt.Errorf("ledgerkeys: %w", pathErr(err))
		}
	} else {
		// A hard link fails when the target exists and never replaces it.
		err := os.Link(name, path)
		_ = os.Remove(name)
		if err != nil {
			if errors.Is(err, os.ErrExist) {
				return fmt.Errorf("ledgerkeys: %s already exists", path)
			}
			return fmt.Errorf("ledgerkeys: %w", pathErr(err))
		}
	}
	if runtime.GOOS != "windows" {
		d, err := os.Open(dir) // #nosec G304 -- the directory of the key file
		if err != nil {
			return fmt.Errorf("ledgerkeys: %w", pathErr(err))
		}
		serr := d.Sync()
		_ = d.Close()
		if serr != nil {
			return fmt.Errorf("ledgerkeys: flushing %s: %w", dir, serr)
		}
	}
	return nil
}
