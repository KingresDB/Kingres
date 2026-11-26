// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package trust

import (
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"strings"
	"testing"

	"github.com/KingresDB/Kingres/kingres/verifier/internal/edkey"
)

// Correction 155: every public key the verifier reads passes the three tests.

func keyFile(pub []byte, client bool) string {
	b64 := base64.StdEncoding.EncodeToString(pub)
	fp := Fingerprint(pub)
	if client {
		return fmt.Sprintf(`{"format":1,"origin_prefix":"p.example/k","keys":[{"name":"k","public_key":"%s","fingerprint":"%s"}],"client_keys":[{"key_id":"%s","public_key":"%s","label":"c"}]}`,
			base64.StdEncoding.EncodeToString(append([]byte{3}, make([]byte, 31)...)), Fingerprint(append([]byte{3}, make([]byte, 31)...)), fp[:32], b64)
	}
	return fmt.Sprintf(`{"format":1,"origin_prefix":"p.example/k","keys":[{"name":"k","public_key":"%s","fingerprint":"%s"}]}`, b64, fp)
}

func TestKeysThatFailTheThreeTestsAreRefusedEverywhereTheyAreRead(t *testing.T) {
	z := strings.Repeat("00", 31)
	for name, h := range map[string]string{
		"identity (small order)":  "01" + z,
		"order 4 point":           strings.Repeat("00", 32),
		"order 8 point":           "26e8958fc2b227b045c3f489f2ef98f0d5dfac05d3c63339b13802886d53fc05",
		"not canonical (y = p+3)": "f0" + strings.Repeat("ff", 30) + "7f",
		"not on the curve":        "02" + z,
	} {
		pub, _ := hex.DecodeString(h)
		if _, err := Parse([]byte(keyFile(pub, false))); err == nil || !strings.Contains(err.Error(), "public_key") {
			t.Errorf("server key, %s: %v", name, err)
		}
		if _, err := Parse([]byte(keyFile(pub, true))); err == nil || !strings.Contains(err.Error(), "public_key") {
			t.Errorf("client key, %s: %v", name, err)
		}
		if _, err := New("p.example/k", "k", pub, Fingerprint(pub)); err == nil {
			t.Errorf("trust init, %s: accepted", name)
		}
	}
	// the one good point of the vectors is accepted in all three places
	good, _ := hex.DecodeString("03" + z)
	if _, err := Parse([]byte(keyFile(good, false))); err != nil {
		t.Error(err)
	}
	if _, err := Parse([]byte(keyFile(good, true))); err != nil {
		t.Error(err)
	}
	if _, err := New("p.example/k", "k", good, Fingerprint(good)); err != nil {
		t.Error(err)
	}
	_ = edkey.Size
}
