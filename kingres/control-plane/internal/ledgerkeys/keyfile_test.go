// SPDX-License-Identifier: AGPL-3.0-only

package ledgerkeys

import (
	"bytes"
	"context"
	"crypto/ed25519"
	stdpbkdf2 "crypto/pbkdf2"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"math/rand"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func testPass(t testing.TB, s string) Passphrase {
	t.Helper()
	p, err := NewPassphrase([]byte(s))
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// seededKey makes a key from a fixed seed so that the tests do not depend on
// the system's random source.
func seededKey(t testing.TB, n byte) *FileSigner {
	t.Helper()
	seed := bytes.Repeat([]byte{n}, SeedSize)
	s, err := NewFileSigner(seed)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestPBKDF2MatchesStandardLibrary(t *testing.T) {
	// RFC 7914 section 11, the first 32 bytes of the first vector.
	got := pbkdf2([]byte("passwd"), []byte("salt"), 1)
	if hex.EncodeToString(got) != "55ac046e56e3089fec1691c22544b605f94185216dde0465e68b9d57c20dacbc" {
		t.Fatalf("RFC 7914 vector: %x", got)
	}
	rng := rand.New(rand.NewSource(1))
	for i := 0; i < 20; i++ {
		pw := make([]byte, 1+rng.Intn(100))
		salt := make([]byte, 16)
		rng.Read(pw)
		rng.Read(salt)
		iter := 1 + rng.Intn(300)
		want, err := stdpbkdf2.Key(sha256.New, string(pw), salt, iter, 32)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(pbkdf2(pw, salt, iter), want) {
			t.Fatalf("iteration %d: differs from crypto/pbkdf2", i)
		}
	}
}

func TestPlainRoundTrip(t *testing.T) {
	s := seededKey(t, 7)
	data, err := Encode(*s, Passphrase{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := FileMagic + "\n" + `{"seed":"` + base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{7}, 32)) + `"}` + "\n"
	if string(data) != want {
		t.Fatalf("plain file:\n%s\nwant\n%s", data, want)
	}
	f, err := Parse(data)
	if err != nil {
		t.Fatal(err)
	}
	if f.Encrypted() || !bytes.Equal(f.Marshal(), data) {
		t.Fatal("parse then marshal is not the identity")
	}
	s2, err := f.Open(Passphrase{})
	if err != nil || !s.SameKey(*s2) || !bytes.Equal(s.Public(), s2.Public()) {
		t.Fatalf("open: %v", err)
	}
	if _, err := f.Open(testPass(t, "x")); !errors.Is(err, ErrPassphraseNotNeeded) {
		t.Fatalf("a passphrase for a plain file: %v", err)
	}
}

func TestEncryptedRoundTripAndTamper(t *testing.T) {
	s := seededKey(t, 9)
	pass := testPass(t, "correct horse")
	data, err := Encode(*s, pass, bytes.NewReader(bytes.Repeat([]byte{0xAB}, 64)))
	if err != nil {
		t.Fatal(err)
	}
	f, err := Parse(data)
	if err != nil || !f.Encrypted() || f.Iterations() != Iterations {
		t.Fatalf("parse: %v", err)
	}
	if !bytes.Equal(f.Marshal(), data) {
		t.Fatal("parse then marshal is not the identity")
	}
	if len(data) > MaxFileSize {
		t.Fatalf("the canonical encrypted file has %d bytes, more than MaxFileSize", len(data))
	}
	if _, err := f.Open(Passphrase{}); !errors.Is(err, ErrPassphraseRequired) {
		t.Fatalf("no passphrase: %v", err)
	}
	if _, err := f.Open(testPass(t, "wrong")); !errors.Is(err, ErrWrongPassphrase) {
		t.Fatalf("wrong passphrase: %v", err)
	}
	got, err := f.Open(pass)
	if err != nil || !got.SameKey(*s) {
		t.Fatalf("open: %v", err)
	}
	// Every member that the header or the box covers: a change that stays in
	// the canonical form must fail to open.
	text := string(data)
	first := func(member string) string {
		i := strings.Index(text, `"`+member+`":"`)
		if i < 0 {
			t.Fatalf("test bug: %q not in file", member)
		}
		i += len(member) + 4
		c := "A"
		if text[i] == 'A' {
			c = "B"
		}
		return text[:i] + c + text[i+1:]
	}
	for name, mutated := range map[string]string{
		"iterations": strings.Replace(text, `"iterations":600000`, `"iterations":600001`, 1),
		"salt":       first("salt"),
		"nonce":      first("nonce"),
		"ciphertext": first("ciphertext"),
	} {
		pf, err := Parse([]byte(mutated))
		if err != nil {
			t.Fatalf("%s: the mutated file should still parse: %v", name, err)
		}
		if _, err := pf.Open(pass); !errors.Is(err, ErrWrongPassphrase) {
			t.Errorf("%s: opened a changed file: %v", name, err)
		}
	}
}

func TestParseRefusesMalformed(t *testing.T) {
	seed := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{1}, 32))
	good := FileMagic + "\n" + `{"seed":"` + seed + `"}` + "\n"
	if _, err := Parse([]byte(good)); err != nil {
		t.Fatal(err)
	}
	salt := base64.StdEncoding.EncodeToString(make([]byte, 16))
	nonce := base64.StdEncoding.EncodeToString(make([]byte, 12))
	ct := base64.StdEncoding.EncodeToString(make([]byte, 48))
	enc := func(kdf, iter, s, n, c string) string {
		return FileMagic + "\n" + `{"kdf":"` + kdf + `","iterations":` + iter + `,"salt":"` + s + `","nonce":"` + n + `","ciphertext":"` + c + `"}` + "\n"
	}
	if _, err := Parse([]byte(enc(KDFName, "600000", salt, nonce, ct))); err != nil {
		t.Fatal(err)
	}
	cases := map[string]string{
		"empty":                "",
		"magic only":           FileMagic + "\n",
		"wrong magic":          "KINGRES-KEY-2\n" + `{"seed":"` + seed + `"}` + "\n",
		"no newline after":     strings.TrimSuffix(good, "\n"),
		"crlf":                 strings.ReplaceAll(good, "\n", "\r\n"),
		"space after colon":    FileMagic + "\n" + `{"seed": "` + seed + `"}` + "\n",
		"leading space":        FileMagic + "\n " + `{"seed":"` + seed + `"}` + "\n",
		"trailing text":        good + "x",
		"second line":          good + good,
		"duplicate key":        FileMagic + "\n" + `{"seed":"` + seed + `","seed":"` + seed + `"}` + "\n",
		"unknown member":       FileMagic + "\n" + `{"seed":"` + seed + `","x":"y"}` + "\n",
		"wrong member":         FileMagic + "\n" + `{"key":"` + seed + `"}` + "\n",
		"seed number":          FileMagic + "\n" + `{"seed":5}` + "\n",
		"seed null":            FileMagic + "\n" + `{"seed":null}` + "\n",
		"seed array":           FileMagic + "\n" + `{"seed":["a"]}` + "\n",
		"seed nested":          FileMagic + "\n" + `{"seed":{"a":"b"}}` + "\n",
		"seed short":           FileMagic + "\n" + `{"seed":"` + base64.StdEncoding.EncodeToString(make([]byte, 31)) + `"}` + "\n",
		"seed long":            FileMagic + "\n" + `{"seed":"` + base64.StdEncoding.EncodeToString(make([]byte, 33)) + `"}` + "\n",
		"seed url alphabet":    FileMagic + "\n" + `{"seed":"` + base64.URLEncoding.EncodeToString(bytes.Repeat([]byte{0xfb}, 32)) + `"}` + "\n",
		"seed unpadded":        FileMagic + "\n" + `{"seed":"` + strings.TrimRight(seed, "=") + `"}` + "\n",
		"seed escaped":         FileMagic + "\n" + `{"seed":"` + strings.Replace(seed, seed[:1], `\u`+hex.EncodeToString([]byte{0, seed[0]}), 1) + `"}` + "\n",
		"seed trailing bits":   FileMagic + "\n" + `{"seed":"` + seed[:len(seed)-2] + `B="}` + "\n",
		"not an object":        FileMagic + "\n" + `["seed"]` + "\n",
		"empty object":         FileMagic + "\n" + `{}` + "\n",
		"unterminated":         FileMagic + "\n" + `{"seed":"` + seed + `"` + "\n",
		"kdf other":            enc("scrypt", "600000", salt, nonce, ct),
		"iterations low":       enc(KDFName, "599999", salt, nonce, ct),
		"iterations high":      enc(KDFName, "6000001", salt, nonce, ct),
		"iterations zero":      enc(KDFName, "0", salt, nonce, ct),
		"iterations negative":  enc(KDFName, "-600000", salt, nonce, ct),
		"iterations float":     enc(KDFName, "600000.0", salt, nonce, ct),
		"iterations exp":       enc(KDFName, "6e5", salt, nonce, ct),
		"iterations leading 0": enc(KDFName, "0600000", salt, nonce, ct),
		"iterations huge":      enc(KDFName, "99999999999999999999", salt, nonce, ct),
		"iterations string":    strings.Replace(enc(KDFName, "600000", salt, nonce, ct), `"iterations":600000`, `"iterations":"600000"`, 1),
		"salt short":           enc(KDFName, "600000", base64.StdEncoding.EncodeToString(make([]byte, 15)), nonce, ct),
		"nonce long":           enc(KDFName, "600000", salt, base64.StdEncoding.EncodeToString(make([]byte, 13)), ct),
		"ciphertext short":     enc(KDFName, "600000", salt, nonce, base64.StdEncoding.EncodeToString(make([]byte, 47))),
		"order changed":        FileMagic + "\n" + `{"iterations":600000,"kdf":"` + KDFName + `","salt":"` + salt + `","nonce":"` + nonce + `","ciphertext":"` + ct + `"}` + "\n",
		"member missing":       FileMagic + "\n" + `{"kdf":"` + KDFName + `","iterations":600000,"salt":"` + salt + `","nonce":"` + nonce + `"}` + "\n",
		"seed and kdf":         FileMagic + "\n" + `{"seed":"` + seed + `","kdf":"` + KDFName + `"}` + "\n",
		"control char":         FileMagic + "\n" + `{"seed":"` + seed + "\x00" + `"}` + "\n",
	}
	for name, in := range cases {
		_, err := Parse([]byte(in))
		if !errors.Is(err, ErrMalformed) {
			t.Errorf("%s: err = %v, want ErrMalformed", name, err)
		} else if strings.Contains(err.Error(), seed) {
			t.Errorf("%s: the error repeats the seed", name)
		}
	}
}

func TestFileSizeBoundary(t *testing.T) {
	seed := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{1}, 32))
	good := FileMagic + "\n" + `{"seed":"` + seed + `"}` + "\n"
	pad := MaxFileSize - len(good)
	if pad < 0 {
		t.Fatal("test bug")
	}
	// MaxFileSize bytes: refused as non-canonical, not as too large; one more
	// byte: refused as too large.
	_, err := Parse([]byte(good + strings.Repeat("x", pad)))
	if !errors.Is(err, ErrMalformed) || strings.Contains(err.Error(), "at most") {
		t.Errorf("MaxFileSize bytes: %v", err)
	}
	_, err = Parse([]byte(good + strings.Repeat("x", pad+1)))
	if !errors.Is(err, ErrMalformed) || !strings.Contains(err.Error(), "at most") {
		t.Errorf("MaxFileSize+1 bytes: %v", err)
	}
}

func TestIterationBoundaries(t *testing.T) {
	salt := base64.StdEncoding.EncodeToString(make([]byte, 16))
	nonce := base64.StdEncoding.EncodeToString(make([]byte, 12))
	ct := base64.StdEncoding.EncodeToString(make([]byte, 48))
	mk := func(it string) []byte {
		return []byte(FileMagic + "\n" + `{"kdf":"` + KDFName + `","iterations":` + it + `,"salt":"` + salt + `","nonce":"` + nonce + `","ciphertext":"` + ct + `"}` + "\n")
	}
	for _, it := range []string{"600000", "600001", "6000000"} {
		if _, err := Parse(mk(it)); err != nil {
			t.Errorf("iterations %s: %v", it, err)
		}
	}
	for _, it := range []string{"599999", "6000001"} {
		if _, err := Parse(mk(it)); !errors.Is(err, ErrMalformed) {
			t.Errorf("iterations %s: %v", it, err)
		}
	}
}

func TestPassphraseBoundaries(t *testing.T) {
	if _, err := NewPassphrase(nil); err == nil {
		t.Error("an empty passphrase was accepted")
	}
	if _, err := NewPassphrase(make([]byte, MaxPassphraseSize)); err != nil {
		t.Errorf("MaxPassphraseSize bytes: %v", err)
	}
	if _, err := NewPassphrase(make([]byte, MaxPassphraseSize+1)); err == nil {
		t.Error("a passphrase over the limit was accepted")
	}
	dir := t.TempDir()
	write := func(name, content string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	long := strings.Repeat("p", MaxPassphraseSize)
	for name, tc := range map[string]struct {
		content string
		ok      bool
	}{
		"plain":            {"secret", true},
		"lf":               {"secret\n", true},
		"crlf":             {"secret\r\n", true},
		"max":              {long, true},
		"max with lf":      {long + "\n", true},
		"max plus one":     {long + "p", false},
		"max plus one lf":  {long + "p\n", false},
		"empty":            {"", false},
		"only newline":     {"\n", false},
		"two lines":        {"a\nb\n", false},
		"cr inside":        {"a\rb", false},
		"double newline":   {"a\n\n", false},
		"trailing spaces":  {"a  ", true},
		"far over the cap": {strings.Repeat("p", 10*MaxPassphraseFileSize), false},
	} {
		p, err := ReadPassphraseFile(write(strings.ReplaceAll(name, " ", "_"), tc.content))
		if (err == nil) != tc.ok {
			t.Errorf("%s: err = %v, want ok=%v", name, err, tc.ok)
		}
		p.Destroy()
	}
	if _, err := ReadPassphraseFile(filepath.Join(dir, "missing")); err == nil {
		t.Error("a missing passphrase file was accepted")
	}
	if _, err := ReadPassphraseFile(dir); err == nil {
		t.Error("a directory was accepted as a passphrase file")
	}
	if runtime.GOOS != "windows" {
		loose := write("loose", "secret")
		if err := os.Chmod(loose, 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := ReadPassphraseFile(loose); err == nil {
			t.Error("a passphrase file readable by others was accepted")
		}
	}
}

func TestPassphraseDestroyAndEqual(t *testing.T) {
	a, b := testPass(t, "same"), testPass(t, "same")
	if !a.Equal(b) || a.Equal(testPass(t, "other")) || a.Equal(Passphrase{}) {
		t.Fatal("Equal is wrong")
	}
	a.Destroy()
	if a.IsSet() || a.Equal(b) {
		t.Fatal("a destroyed passphrase is still set")
	}
	s := seededKey(t, 3)
	if _, err := Encode(*s, a, nil); !errors.Is(err, ErrDestroyed) {
		t.Fatalf("encode with a destroyed passphrase: %v", err)
	}
}

func TestSignerCloseZeroes(t *testing.T) {
	s := seededKey(t, 5)
	msg := []byte("hello")
	sig, err := s.Sign(context.Background(), msg)
	if err != nil || len(sig) != ed25519.SignatureSize || !ed25519.Verify(s.Public(), msg, sig) {
		t.Fatalf("sign: %v", err)
	}
	copyOfS := *s
	_ = s.Close()
	if _, err := s.Sign(context.Background(), msg); !errors.Is(err, ErrDestroyed) {
		t.Fatalf("sign after close: %v", err)
	}
	if _, err := copyOfS.Sign(context.Background(), msg); !errors.Is(err, ErrDestroyed) {
		t.Fatalf("a copy signs after close: %v", err)
	}
	if _, err := Encode(*s, Passphrase{}, nil); !errors.Is(err, ErrDestroyed) {
		t.Fatalf("encode after close: %v", err)
	}
}

func TestSignBoundsAndContext(t *testing.T) {
	s := seededKey(t, 5)
	if _, err := s.Sign(context.Background(), make([]byte, MaxMessageSize)); err != nil {
		t.Errorf("MaxMessageSize: %v", err)
	}
	if _, err := s.Sign(context.Background(), make([]byte, MaxMessageSize+1)); !errors.Is(err, ErrMessageTooLarge) {
		t.Errorf("MaxMessageSize+1: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := s.Sign(ctx, []byte("x")); !errors.Is(err, context.Canceled) {
		t.Errorf("cancelled context: %v", err)
	}
}

type badSigner struct {
	pub ed25519.PublicKey
	sig []byte
	err error
}

func (b badSigner) Public() ed25519.PublicKey { return b.pub }
func (b badSigner) Sign(context.Context, []byte) ([]byte, error) {
	return b.sig, b.err
}

func TestVerifyingSigner(t *testing.T) {
	s := seededKey(t, 5)
	good, err := VerifyingSigner(s)
	if err != nil {
		t.Fatal(err)
	}
	sig, err := good.Sign(context.Background(), []byte("m"))
	if err != nil || !ed25519.Verify(s.Public(), []byte("m"), sig) {
		t.Fatalf("good signer: %v", err)
	}
	other := seededKey(t, 6)
	wrong, _ := other.Sign(context.Background(), []byte("m"))
	for name, bs := range map[string]badSigner{
		"other key": {pub: s.Public(), sig: wrong},
		"short":     {pub: s.Public(), sig: make([]byte, 63)},
		"long":      {pub: s.Public(), sig: make([]byte, 65)},
		"nil":       {pub: s.Public()},
		"zeros":     {pub: s.Public(), sig: make([]byte, 64)},
	} {
		v, err := VerifyingSigner(bs)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := v.Sign(context.Background(), []byte("m")); !errors.Is(err, ErrBadSignature) {
			t.Errorf("%s: %v", name, err)
		}
	}
	boom := errors.New("boom")
	v, _ := VerifyingSigner(badSigner{pub: s.Public(), err: boom})
	if _, err := v.Sign(context.Background(), []byte("m")); !errors.Is(err, boom) {
		t.Errorf("an error of the signer is passed on: %v", err)
	}
	if _, err := VerifyingSigner(badSigner{pub: make([]byte, 31)}); err == nil {
		t.Error("a short public key was accepted")
	}
}

func TestKeyIDMatchesDefinition(t *testing.T) {
	pub := seededKey(t, 5).Public()
	name := "ledger.example.com/kingres/a/00"
	h := sha256.New()
	h.Write([]byte(name + "\n"))
	h.Write([]byte{0x01})
	h.Write(pub)
	sum := h.Sum(nil)
	want := uint32(sum[0])<<24 | uint32(sum[1])<<16 | uint32(sum[2])<<8 | uint32(sum[3])
	if KeyID(name, pub) != want {
		t.Fatal("KeyID differs from SHA-256(name || 0x0A || 0x01 || key)[:4]")
	}
	for _, bad := range []string{"", "a b", "a+b", "a\nb", "a\x00b", "a\u00a0b", strings.Repeat("x", MaxKeyNameSize+1), "\xff"} {
		if ValidKeyName(bad) {
			t.Errorf("%q accepted as a key name", bad)
		}
	}
	if !ValidKeyName(strings.Repeat("x", MaxKeyNameSize)) || !ValidKeyName(name) {
		t.Error("a valid name was refused")
	}
}

func TestWriteFileAndReadFile(t *testing.T) {
	fastKDF(t)
	dir := t.TempDir()
	s := seededKey(t, 11)
	path := filepath.Join(dir, FileName(s.Public()))
	pass := testPass(t, "pw")
	if err := WriteFile(path, *s, pass, false); err != nil {
		t.Fatal(err)
	}
	if err := WriteFile(path, *s, pass, false); err == nil {
		t.Fatal("a key file was overwritten without replace")
	}
	if runtime.GOOS != "windows" {
		fi, err := os.Stat(path)
		if err != nil || fi.Mode().Perm() != 0o600 {
			t.Fatalf("mode %v %v", fi.Mode(), err)
		}
	}
	got, err := ReadFile(path, pass)
	if err != nil || !got.SameKey(*s) {
		t.Fatalf("read: %v", err)
	}
	if _, err := ReadNamed(dir, s.Fingerprint(), pass); err != nil {
		t.Fatal(err)
	}
	// Replace with a plain file (a change of passphrase) keeps one file.
	if err := WriteFile(path, *s, Passphrase{}, true); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadFile(path, pass); !errors.Is(err, ErrPassphraseNotNeeded) {
		t.Fatalf("read of the replaced file: %v", err)
	}
	if _, err := ReadFile(path, Passphrase{}); err != nil {
		t.Fatal(err)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Fatalf("the directory holds %d entries after the writes, expected the key file only", len(entries))
	}
	// A file under another key's name is refused.
	other := seededKey(t, 12)
	if err := WriteFile(filepath.Join(dir, s.Fingerprint()+".key"), *other, Passphrase{}, true); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadNamed(dir, s.Fingerprint(), Passphrase{}); err == nil {
		t.Fatal("a key file with another key than its name says was accepted")
	}
	// Missing, directory, too large.
	if _, err := ReadFile(filepath.Join(dir, "missing.key"), Passphrase{}); err == nil {
		t.Fatal("missing file accepted")
	}
	if _, err := ReadFile(dir, Passphrase{}); err == nil {
		t.Fatal("directory accepted")
	}
	big := filepath.Join(dir, "big.key")
	if err := os.WriteFile(big, make([]byte, MaxFileSize+1), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadFile(big, Passphrase{}); err == nil {
		t.Fatal("oversized file accepted")
	}
	if runtime.GOOS != "windows" {
		loose := filepath.Join(dir, "loose.key")
		data, _ := Encode(*s, Passphrase{}, nil)
		if err := os.WriteFile(loose, data, 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := ReadFile(loose, Passphrase{}); err == nil {
			t.Fatal("a key file readable by others was accepted")
		}
	}
	link := filepath.Join(dir, "link.key")
	if err := os.Symlink(path, link); err == nil {
		if _, err := ReadFile(link, Passphrase{}); err == nil {
			t.Fatal("a link was accepted as a key file")
		}
	}
}

func TestWriteFileFailsInMissingDirectory(t *testing.T) {
	s := seededKey(t, 1)
	if err := WriteFile(filepath.Join(t.TempDir(), "no", "such", "k.key"), *s, Passphrase{}, false); err == nil {
		t.Fatal("a write into a missing directory succeeded")
	}
}

// fastKDF lowers the PBKDF2 count that is written and the least that is read,
// for tests that are not about the count: the 600,000 iterations of the real
// value cost most of the time of this package's tests. TestEncryptedRoundTripAndTamper,
// the boundary tests and the fuzz seeds use the real value.
func fastKDF(t testing.TB) {
	t.Helper()
	oldMin, oldWrite := minIterations, writeIterations
	minIterations, writeIterations = 1000, 1000
	t.Cleanup(func() { minIterations, writeIterations = oldMin, oldWrite })
}
