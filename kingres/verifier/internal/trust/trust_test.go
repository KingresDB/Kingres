// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package trust

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"strings"
	"testing"
	"time"
)

func testKey(seed byte) ed25519.PublicKey {
	s := make([]byte, ed25519.SeedSize)
	for i := range s {
		s[i] = seed
	}
	return ed25519.NewKeyFromSeed(s).Public().(ed25519.PublicKey)
}

func fp(pub []byte) string {
	sum := sha256.Sum256(pub)
	return hex.EncodeToString(sum[:])
}

func b64(b []byte) string { return base64.StdEncoding.EncodeToString(b) }

func goodJSON() string {
	pub := testKey(1)
	return fmt.Sprintf(`{"format": 1, "origin_prefix": "ledger.example.com/kingres",
 "keys": [{"name": "2026", "public_key": %q, "fingerprint": %q, "not_before": "2026-12-01T00:00:00Z", "not_after": null}],
 "tsa_roots": [], "client_keys": [], "witnesses": []}`, b64(pub), fp(pub))
}

func TestParseGood(t *testing.T) {
	f, err := Parse([]byte(goodJSON()))
	if err != nil {
		t.Fatal(err)
	}
	if f.OriginPrefix != "ledger.example.com/kingres" || len(f.Keys) != 1 || f.Keys[0].NotBefore == nil || f.Keys[0].NotAfter != nil {
		t.Fatalf("%+v", f)
	}
	// Written and read back gives the same file.
	out, err := f.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	g, err := Parse(out)
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	out2, _ := g.Marshal()
	if string(out) != string(out2) {
		t.Fatalf("not stable:\n%s\n%s", out, out2)
	}
	// Only the three required members are enough.
	pub := testKey(1)
	min := fmt.Sprintf(`{"format":1,"origin_prefix":"a.example","keys":[{"name":"k","public_key":%q,"fingerprint":%q}]}`, b64(pub), fp(pub))
	if _, err := Parse([]byte(min)); err != nil {
		t.Fatal(err)
	}
	// No keys is refused: every command refuses it, so trust show must too.
	if _, err := Parse([]byte(`{"format":1,"origin_prefix":"a.example","keys":[]}`)); err == nil {
		t.Fatal("a trust file without keys was accepted")
	}
}

func TestParseRejections(t *testing.T) {
	pub := testKey(1)
	other := testKey(2)
	key := func(extra string) string {
		return fmt.Sprintf(`{"name":"k","public_key":%q,"fingerprint":%q%s}`, b64(pub), fp(pub), extra)
	}
	file := func(body string) string { return `{"format":1,"origin_prefix":"a.example",` + body + `}` }
	cases := map[string]string{
		"empty":                "",
		"not an object":        "[]",
		"trailing data":        goodJSON() + "{}",
		"duplicate key":        `{"format":1,"format":1,"origin_prefix":"a.example","keys":[]}`,
		"no format":            `{"origin_prefix":"a.example","keys":[]}`,
		"format 2":             `{"format":2,"origin_prefix":"a.example","keys":[]}`,
		"format string":        `{"format":"1","origin_prefix":"a.example","keys":[]}`,
		"format float":         `{"format":1.0,"origin_prefix":"a.example","keys":[]}`,
		"no prefix":            `{"format":1,"keys":[]}`,
		"empty prefix":         `{"format":1,"origin_prefix":"","keys":[]}`,
		"prefix with scheme":   `{"format":1,"origin_prefix":"https://a.example","keys":[]}`,
		"prefix trailing /":    `{"format":1,"origin_prefix":"a.example/","keys":[]}`,
		"prefix leading /":     `{"format":1,"origin_prefix":"/a.example","keys":[]}`,
		"prefix double /":      `{"format":1,"origin_prefix":"a.example//x","keys":[]}`,
		"prefix with space":    `{"format":1,"origin_prefix":"a example","keys":[]}`,
		"prefix with plus":     `{"format":1,"origin_prefix":"a+b","keys":[]}`,
		"prefix too long":      `{"format":1,"origin_prefix":"` + strings.Repeat("a", 161) + `","keys":[]}`,
		"no keys member":       `{"format":1,"origin_prefix":"a.example"}`,
		"keys not array":       `{"format":1,"origin_prefix":"a.example","keys":{}}`,
		"unknown member":       `{"format":1,"origin_prefix":"a.example","keys":[],"extra":1}`,
		"unknown key member":   file(`"keys":[` + key(`,"extra":1`) + `]`),
		"wrong fingerprint":    file(fmt.Sprintf(`"keys":[{"name":"k","public_key":%q,"fingerprint":%q}]`, b64(pub), fp(other))),
		"upper case fp":        file(fmt.Sprintf(`"keys":[{"name":"k","public_key":%q,"fingerprint":%q}]`, b64(pub), strings.ToUpper(fp(pub)))),
		"no fingerprint":       file(fmt.Sprintf(`"keys":[{"name":"k","public_key":%q}]`, b64(pub))),
		"short key":            file(fmt.Sprintf(`"keys":[{"name":"k","public_key":%q,"fingerprint":%q}]`, b64(pub[:31]), fp(pub[:31]))),
		"key bad base64":       file(fmt.Sprintf(`"keys":[{"name":"k","public_key":"!!!","fingerprint":%q}]`, fp(pub))),
		"key no padding":       file(fmt.Sprintf(`"keys":[{"name":"k","public_key":%q,"fingerprint":%q}]`, strings.TrimRight(b64(pub), "="), fp(pub))),
		"empty name":           file(fmt.Sprintf(`"keys":[{"name":"","public_key":%q,"fingerprint":%q}]`, b64(pub), fp(pub))),
		"name control char":    file(fmt.Sprintf(`"keys":[{"name":"a\u0001b","public_key":%q,"fingerprint":%q}]`, b64(pub), fp(pub))),
		"same key twice":       file(`"keys":[` + key("") + `,` + strings.Replace(key(""), `"k"`, `"k2"`, 1) + `]`),
		"same name twice":      file(fmt.Sprintf(`"keys":[%s,{"name":"k","public_key":%q,"fingerprint":%q}]`, key(""), b64(other), fp(other))),
		"not_before not utc":   file(`"keys":[` + key(`,"not_before":"2026-12-01T00:00:00+01:00"`) + `]`),
		"not_before date only": file(`"keys":[` + key(`,"not_before":"2026-12-01"`) + `]`),
		"not_before number":    file(`"keys":[` + key(`,"not_before":5`) + `]`),
		"window reversed":      file(`"keys":[` + key(`,"not_before":"2027-01-01T00:00:00Z","not_after":"2026-01-01T00:00:00Z"`) + `]`),
		"too many keys":        file(`"keys":[` + strings.TrimSuffix(strings.Repeat(key("")+",", 17), ",") + `]`),
		"witnesses used":       file(`"keys":[],"witnesses":["x"]`),
		"witnesses not array":  file(`"keys":[],"witnesses":{}`),
		"tsa root not string":  file(`"keys":[],"tsa_roots":[1]`),
		"tsa root not pem":     file(`"keys":[],"tsa_roots":["hello"]`),
		"tsa root bad cert":    file(`"keys":[],"tsa_roots":["-----BEGIN CERTIFICATE-----\nAAAA\n-----END CERTIFICATE-----\n"]`),
		"tsa root wrong type":  file(`"keys":[],"tsa_roots":["-----BEGIN PRIVATE KEY-----\nAAAA\n-----END PRIVATE KEY-----\n"]`),
		"client key bad id":    file(fmt.Sprintf(`"keys":[],"client_keys":[{"key_id":"00","public_key":%q,"label":"x"}]`, b64(pub))),
		"client key wrong id":  file(fmt.Sprintf(`"keys":[],"client_keys":[{"key_id":%q,"public_key":%q,"label":"x"}]`, fp(other)[:32], b64(pub))),
		"client key no label":  file(fmt.Sprintf(`"keys":[],"client_keys":[{"key_id":%q,"public_key":%q}]`, fp(pub)[:32], b64(pub))),
		"client key upper id":  file(fmt.Sprintf(`"keys":[],"client_keys":[{"key_id":%q,"public_key":%q,"label":"x"}]`, strings.ToUpper(fp(pub)[:32]), b64(pub))),
		"client key twice":     file(fmt.Sprintf(`"keys":[],"client_keys":[{"key_id":%[1]q,"public_key":%[2]q,"label":"x"},{"key_id":%[1]q,"public_key":%[2]q,"label":"y"}]`, fp(pub)[:32], b64(pub))),
		"invalid utf8":         "{\"format\":1,\"origin_prefix\":\"a\xff\",\"keys\":[]}",
	}
	cases["no keys"] = `{"format":1,"origin_prefix":"a.example","keys":[]}`
	cases["revoked with a size limit"] = file(`"keys":[` + key(`,"revoked":true,"max_tree_size":5`) + `]`)
	cases["revoked not a bool"] = file(`"keys":[` + key(`,"revoked":"yes"`) + `]`)
	cases["max_tree_size zero"] = file(`"keys":[` + key(`,"max_tree_size":0`) + `]`)
	cases["max_tree_size too large"] = file(`"keys":[` + key(`,"max_tree_size":4611686018427387904`) + `]`)
	cases["max_tree_size float"] = file(`"keys":[` + key(`,"max_tree_size":5.5`) + `]`)
	for name, in := range cases {
		// Every case that says "keys":[] is about another member, so give it
		// a valid key: otherwise it would be refused for having no key.
		if name != "no keys" {
			in = strings.ReplaceAll(in, `"keys":[]`, `"keys":[`+key("")+`]`)
		}
		if _, err := Parse([]byte(in)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if _, err := Parse(make([]byte, MaxFileSize+1)); err == nil {
		t.Error("oversized file accepted")
	}
	// A valid client key is accepted.
	ok := file(fmt.Sprintf(`"keys":[`+key("")+`],"client_keys":[{"key_id":%q,"public_key":%q,"label":"teller-7"}]`, fp(pub)[:32], b64(pub)))
	if f, err := Parse([]byte(ok)); err != nil || len(f.ClientKeys) != 1 {
		t.Fatalf("client key: %v", err)
	}
}

func TestNew(t *testing.T) {
	pub := testKey(3)
	f, err := New("ledger.example.com/kingres", "key-1", pub, strings.ToUpper(fp(pub)))
	if err != nil || f.Keys[0].Fingerprint != fp(pub) {
		t.Fatalf("%v", err)
	}
	var colons []string
	for i := 0; i < len(fp(pub)); i += 2 {
		colons = append(colons, fp(pub)[i:i+2])
	}
	if _, err := New("a.example", "k", pub, strings.Join(colons, ":")); err != nil {
		t.Fatalf("colon form: %v", err)
	}
	for name, args := range map[string][3]string{
		"wrong fingerprint": {"a.example", "k", fp(testKey(4))},
		"empty fingerprint": {"a.example", "k", ""},
		"short fingerprint": {"a.example", "k", fp(pub)[:60]},
		"bad prefix":        {"https://a.example", "k", fp(pub)},
		"bad name":          {"a.example", "", fp(pub)},
	} {
		if _, err := New(args[0], args[1], pub, args[2]); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if _, err := New("a.example", "k", pub[:31], fp(pub)); err == nil {
		t.Error("short key accepted")
	}
}

func TestSignerKeys(t *testing.T) {
	pub := testKey(1)
	f, _ := Parse([]byte(goodJSON()))
	origin := "ledger.example.com/kingres/c/00/e1"
	now := time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)
	keys, notes, err := f.SignerKeys(origin, now)
	if err != nil || len(keys) != 1 || keys[0].Name != origin || !keys[0].Public.Equal(pub) || len(notes) != 0 {
		t.Fatalf("%v %v %v", keys, notes, err)
	}
	if _, _, err := f.SignerKeys("ledger.example.com/kingresX/c/00/e1", now); err == nil {
		t.Error("origin outside the prefix accepted")
	}
	if _, _, err := f.SignerKeys("other.example/c/00/e1", now); err == nil {
		t.Error("foreign origin accepted")
	}
	if _, _, err := f.SignerKeys("ledger.example.com/kingres", now); err != nil {
		t.Errorf("the prefix itself: %v", err)
	}
	// Before not_before: unusable, and it says why.
	early := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	if _, notes, err := f.SignerKeys(origin, early); err == nil || len(notes) != 1 {
		t.Errorf("early: %v %v", notes, err)
	}
	// After not_after the key is not used and the error says why; before it
	// (here: --now inside the window) it is used.
	f.Keys[0].NotAfter = ptr(time.Date(2026, 12, 31, 0, 0, 0, 0, time.UTC))
	if _, notes, err := f.SignerKeys(origin, now); err == nil || len(notes) != 1 || !strings.Contains(err.Error(), "not_after") {
		t.Errorf("expired: %v %v", notes, err)
	}
	if keys, _, err := f.SignerKeys(origin, time.Date(2026, 12, 15, 0, 0, 0, 0, time.UTC)); err != nil || len(keys) != 1 {
		t.Errorf("inside the window: %v %v", keys, err)
	}
	// Revoked is revoked at every time.
	f.Keys[0].NotAfter = nil
	f.Keys[0].Revoked = true
	if _, _, err := f.SignerKeys(origin, time.Date(2026, 12, 15, 0, 0, 0, 0, time.UTC)); err == nil {
		t.Error("a revoked key was used")
	}
	f.Keys[0].Revoked = false
	// A size limit keeps the key, says so, and applies by size.
	lim := uint64(100)
	f.Keys[0].MaxTreeSize = &lim
	if keys, notes, err := f.SignerKeys(origin, now); err != nil || len(keys) != 1 || len(notes) != 1 {
		t.Errorf("limited: %v %v %v", keys, notes, err)
	}
	if !f.LimitFor(pub, 100) || f.LimitFor(pub, 101) || f.LimitFor(testKey(2), 1) {
		t.Error("size limit applied wrongly")
	}
	empty := &File{OriginPrefix: "a.example"}
	if _, _, err := empty.SignerKeys("a.example/x", now); err == nil {
		t.Error("file without keys gave keys")
	}
}

func ptr(t time.Time) *time.Time { return &t }

// FuzzParse checks that the trust file parser never panics and that a file it
// accepts can be written and read again.
func FuzzParse(f *testing.F) {
	f.Add([]byte(goodJSON()))
	f.Add([]byte(`{"format":1,"origin_prefix":"a","keys":[],"x":1}`))
	f.Add([]byte(`{`))
	f.Fuzz(func(t *testing.T, data []byte) {
		tf, err := Parse(data)
		if err != nil {
			return
		}
		out, err := tf.Marshal()
		if err != nil {
			t.Fatal(err)
		}
		if _, err := Parse(out); err != nil {
			t.Fatalf("written file does not parse: %v\n%s", err, out)
		}
	})
}

// The members that retire a key are written and read back, and the standing of
// a key follows them.
func TestRetiringMembersRoundTrip(t *testing.T) {
	pub := testKey(1)
	in := fmt.Sprintf(`{"format":1,"origin_prefix":"a.example","keys":[
 {"name":"old","public_key":%q,"fingerprint":%q,"not_after":"2026-12-31T00:00:00Z","max_tree_size":500},
 {"name":"bad","public_key":%q,"fingerprint":%q,"revoked":true},
 {"name":"off","public_key":%q,"fingerprint":%q,"revoked":false,"max_tree_size":null}]}`,
		b64(pub), fp(pub), b64(testKey(2)), fp(testKey(2)), b64(testKey(3)), fp(testKey(3)))
	f, err := Parse([]byte(in))
	if err != nil {
		t.Fatal(err)
	}
	out, err := f.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	g, err := Parse(out)
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if g.Keys[0].MaxTreeSize == nil || *g.Keys[0].MaxTreeSize != 500 || !g.Keys[1].Revoked || g.Keys[2].Revoked || g.Keys[2].MaxTreeSize != nil {
		t.Fatalf("members lost: %+v", g.Keys)
	}
	now := time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)
	for i, want := range []Standing{Expired, Revoked, Usable} {
		if got := g.Keys[i].StandingAt(now); got != want {
			t.Errorf("key %d: %v, want %v", i, got, want)
		}
	}
	// A revoked key stays revoked at a time before its window would have mattered.
	if g.Keys[1].StandingAt(time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)) != Revoked {
		t.Error("revoked key came back at an earlier time")
	}
}
