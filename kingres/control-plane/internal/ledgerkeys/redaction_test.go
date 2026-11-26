// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package ledgerkeys

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"
)

func assertNoSecret(t *testing.T, what, out string, secrets ...[]byte) {
	t.Helper()
	for _, s := range secrets {
		if len(s) < 8 {
			continue
		}
		for _, form := range forms(s) {
			if form != "" && strings.Contains(out, form) {
				t.Errorf("%s leaks a secret as %q", what, form)
			}
		}
	}
}

func forms(b []byte) []string {
	return []string{
		string(b),
		hex.EncodeToString(b),
		strings.ToUpper(hex.EncodeToString(b)),
		base64.StdEncoding.EncodeToString(b),
		base64.RawStdEncoding.EncodeToString(b),
		base64.URLEncoding.EncodeToString(b),
		base64.RawURLEncoding.EncodeToString(b),
		fmt.Sprint(b),
	}
}

// TestNothingPrintsASecret formats every type of the package that holds a
// secret with every verb, encodes it as JSON and text, logs it, and searches
// the output for the seed, the private key and the passphrase in every form.
func TestNothingPrintsASecret(t *testing.T) {
	fastKDF(t)
	seed := bytes.Repeat([]byte{0x5a, 0xc3, 0x91, 0x0f}, 8)
	pw := []byte("tr0ub4dor&3 horse battery")
	signer, err := NewFileSigner(seed)
	if err != nil {
		t.Fatal(err)
	}
	priv := ed25519.NewKeyFromSeed(seed)
	pass, _ := NewPassphrase(pw)
	plainBytes, _ := Encode(*signer, Passphrase{}, nil)
	plainFile, err := Parse(plainBytes)
	if err != nil {
		t.Fatal(err)
	}
	encBytes, err := Encode(*signer, pass, nil)
	if err != nil {
		t.Fatal(err)
	}
	encFile, err := Parse(encBytes)
	if err != nil {
		t.Fatal(err)
	}
	vp, _ := VerifyingSigner(signer)
	secrets := [][]byte{seed, priv, pw}
	// The ciphertext of an encrypted file is not a secret in the sense of
	// the seed, but nothing should print it either.
	_, ciphertext, _ := strings.Cut(string(encBytes), `"ciphertext":"`)
	ciphertext, _, _ = strings.Cut(ciphertext, `"`)
	secrets = append(secrets, []byte(ciphertext))

	values := map[string]any{
		"FileSigner":      *signer,
		"*FileSigner":     signer,
		"**FileSigner":    &signer,
		"Passphrase":      pass,
		"*Passphrase":     &pass,
		"plain File":      plainFile,
		"*plain File":     &plainFile,
		"encrypted File":  encFile,
		"*encrypted File": &encFile,
		"VerifyingSigner": vp,
		"struct holding": struct {
			S *FileSigner
			P Passphrase
			F File
		}{signer, pass, plainFile},
		"slice holding": []any{signer, pass, plainFile},
		"map holding":   map[string]any{"s": *signer, "p": pass},
	}
	verbs := []string{"%v", "%+v", "%#v", "%s", "%q", "%x", "%X", "%d", "%T", "%p", "%10.3v", "%-40s", "%08b", "%c", "%U"}
	for name, v := range values {
		for _, verb := range verbs {
			assertNoSecret(t, name+" with "+verb, fmt.Sprintf(verb, v), secrets...)
		}
		assertNoSecret(t, name+" Sprint", fmt.Sprint(v), secrets...)
		assertNoSecret(t, name+" Sprintln", fmt.Sprintln(v), secrets...)
		if j, err := json.Marshal(v); err == nil {
			assertNoSecret(t, name+" json", string(j), secrets...)
		}
		if j, err := json.MarshalIndent(v, "", " "); err == nil {
			assertNoSecret(t, name+" json indent", string(j), secrets...)
		}
		var buf bytes.Buffer
		for _, h := range []slog.Handler{slog.NewTextHandler(&buf, nil), slog.NewJSONHandler(&buf, nil)} {
			slog.New(h).Info("value", "v", v, slog.Any("w", v))
		}
		assertNoSecret(t, name+" slog", buf.String(), secrets...)
		if m, ok := v.(interface{ MarshalText() ([]byte, error) }); ok {
			b, _ := m.MarshalText()
			assertNoSecret(t, name+" MarshalText", string(b), secrets...)
		}
		if m, ok := v.(fmt.Stringer); ok {
			assertNoSecret(t, name+" String", m.String(), secrets...)
		}
		if m, ok := v.(fmt.GoStringer); ok {
			assertNoSecret(t, name+" GoString", m.GoString(), secrets...)
		}
	}
	var errs []error
	_, e1 := Parse([]byte(strings.Replace(string(plainBytes), `"}`, `x"}`, 1)))
	_, e2 := Parse(append(plainBytes, plainBytes...))
	_, e3 := encFile.Open(Passphrase{})
	wrong, _ := NewPassphrase([]byte("wrong-passphrase-value"))
	_, e4 := encFile.Open(wrong)
	_, e5 := plainFile.Open(pass)
	signer.Close()
	_, e6 := signer.Sign(context.Background(), nil)
	errs = append(errs, e1, e2, e3, e4, e5, e6)
	for i, e := range errs {
		if e == nil {
			t.Errorf("error %d is nil", i)
			continue
		}
		assertNoSecret(t, fmt.Sprintf("error %d (%v)", i, errors.Unwrap(e)), e.Error(), append(secrets, []byte("wrong-passphrase-value"))...)
	}
}

func TestStringsShowAMarker(t *testing.T) {
	s := seededKey(t, 4)
	p := testPass(t, "x")
	for _, out := range []string{fmt.Sprint(*s), fmt.Sprint(p), s.String(), p.String(), s.GoString(), p.GoString()} {
		if !strings.Contains(out, redacted) {
			t.Errorf("%q has no marker", out)
		}
	}
}
