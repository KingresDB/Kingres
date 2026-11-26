// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package ledgerkeys

import (
	"bytes"
	"encoding/base64"
	"testing"
)

// FuzzParseKeyFile: Parse never panics, and what it accepts is canonical, so
// marshalling it gives the input back.
func FuzzParseKeyFile(f *testing.F) {
	seed := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{1}, 32))
	salt := base64.StdEncoding.EncodeToString(make([]byte, 16))
	nonce := base64.StdEncoding.EncodeToString(make([]byte, 12))
	ct := base64.StdEncoding.EncodeToString(make([]byte, 48))
	f.Add([]byte(FileMagic + "\n" + `{"seed":"` + seed + `"}` + "\n"))
	f.Add([]byte(FileMagic + "\n" + `{"kdf":"` + KDFName + `","iterations":600000,"salt":"` + salt + `","nonce":"` + nonce + `","ciphertext":"` + ct + `"}` + "\n"))
	f.Add([]byte(FileMagic + "\n" + `{"seed":"` + seed + `","seed":"` + seed + `"}` + "\n"))
	f.Add([]byte(""))
	f.Add([]byte(FileMagic + "\n{"))
	f.Fuzz(func(t *testing.T, data []byte) {
		file, err := Parse(data)
		if err != nil {
			return
		}
		if !bytes.Equal(file.Marshal(), data) {
			t.Fatal("serialise(parse(b)) != b")
		}
		again, err := Parse(file.Marshal())
		if err != nil || !bytes.Equal(again.Marshal(), data) {
			t.Fatal("parse(serialise(parse(b))) fails")
		}
		if !file.Encrypted() {
			if s, err := file.Open(Passphrase{}); err != nil || s == nil {
				t.Fatalf("a plain file that parsed does not open: %v", err)
			}
		}
	})
}

func FuzzParseState(f *testing.F) {
	f.Add("active")
	f.Add("")
	f.Fuzz(func(t *testing.T, s string) {
		st, err := ParseState(s)
		if err == nil && st.String() != s {
			t.Fatalf("ParseState(%q).String() = %q", s, st.String())
		}
	})
}
