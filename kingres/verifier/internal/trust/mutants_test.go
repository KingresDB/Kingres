// SPDX-License-Identifier: AGPL-3.0-only

package trust

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"strings"
	"testing"
	"time"
)

// These tests came from a mutation run of the package: the caps at their exact
// values, the origin check of the keys a held note is verified with, and the
// members that no command uses yet but that a file may carry.

func keyEntry(i int, extra string) string {
	pub := testKey(byte(i%250 + 1))
	if i >= 250 {
		s := make([]byte, 32)
		s[0], s[1] = byte(i), byte(i>>8)
		pub = ed25519PublicOf(s)
	}
	return fmt.Sprintf(`{"name":"key%d","public_key":%q,"fingerprint":%q%s}`, i, b64(pub), fp(pub), extra)
}

func fileWithKeys(n int) string {
	var keys []string
	for i := 0; i < n; i++ {
		keys = append(keys, keyEntry(i, ""))
	}
	return `{"format":1,"origin_prefix":"ledger.example.com/kingres","keys":[` + strings.Join(keys, ",") + `]}`
}

func TestCapsAreExact(t *testing.T) {
	if _, err := Parse([]byte(fileWithKeys(MaxKeys))); err != nil {
		t.Errorf("%d keys: %v", MaxKeys, err)
	}
	if _, err := Parse([]byte(fileWithKeys(MaxKeys + 1))); err == nil || !strings.Contains(err.Error(), "has 17 entries") {
		t.Errorf("%d keys: %v", MaxKeys+1, err)
	}
	// the label of a key: 200 bytes are read, 201 are not
	for _, c := range []struct {
		n  int
		ok bool
	}{{200, true}, {201, false}} {
		pub := testKey(1)
		j := fmt.Sprintf(`{"format":1,"origin_prefix":"p.example","keys":[{"name":%q,"public_key":%q,"fingerprint":%q}]}`, strings.Repeat("n", c.n), b64(pub), fp(pub))
		if _, err := Parse([]byte(j)); (err == nil) != c.ok {
			t.Errorf("a label of %d bytes: %v", c.n, err)
		}
	}
	// a name with a control character, or DEL, is refused; the space and the tilde are not controls
	for r, ok := range map[rune]bool{0x1f: false, 0x20: true, 0x7e: true, 0x7f: false} {
		pub := testKey(1)
		j := fmt.Sprintf(`{"format":1,"origin_prefix":"p.example","keys":[{"name":"a\u%04xb","public_key":%q,"fingerprint":%q}]}`, r, b64(pub), fp(pub))
		if _, err := Parse([]byte(j)); (err == nil) != ok {
			t.Errorf("a name with %U: %v", r, err)
		}
	}
	// the size of the file: the limit is on the bytes, before anything is read
	if _, err := Parse(make([]byte, MaxFileSize+1)); err == nil || !strings.Contains(err.Error(), "has 1048577 bytes") {
		t.Errorf("a file of %d bytes: %v", MaxFileSize+1, err)
	}
	if _, err := Parse(bytes.Repeat([]byte{' '}, MaxFileSize)); err == nil || strings.Contains(err.Error(), "expected at most") {
		t.Errorf("a file of %d bytes is read: %v", MaxFileSize, err)
	}
	// the format member: 1 is the only one, and 0 and 2 are named
	for _, v := range []string{"0", "2", "-1", "2147483648"} {
		j := strings.Replace(goodJSON(), `"format": 1`, `"format": `+v, 1)
		if _, err := Parse([]byte(j)); err == nil {
			t.Errorf("format %s accepted", v)
		}
	}
	if _, err := Parse([]byte(strings.Replace(goodJSON(), `"format": 1`, `"format": 2`, 1))); err == nil || !strings.Contains(err.Error(), "format is 2, expected 1") {
		t.Errorf("format 2: %v", err)
	}
}

func selfSigned(t *testing.T, n int64) string {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tpl := &x509.Certificate{SerialNumber: big.NewInt(n), Subject: pkix.Name{CommonName: fmt.Sprintf("root %d", n)}, NotBefore: time.Unix(0, 0), NotAfter: time.Unix(1<<31, 0), IsCA: true, BasicConstraintsValid: true}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &k.PublicKey, k)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
}

func withMembers(members string) string {
	pub := testKey(1)
	return fmt.Sprintf(`{"format":1,"origin_prefix":"p.example","keys":[{"name":"k","public_key":%q,"fingerprint":%q}],%s}`, b64(pub), fp(pub), members)
}

func TestTSARootsAndClientKeys(t *testing.T) {
	one, two := selfSigned(t, 1), selfSigned(t, 2)
	quote := func(s string) string { return fmt.Sprintf("%q", s) }
	// one root, two certificates in one root
	f, err := Parse([]byte(withMembers(`"tsa_roots":[` + quote(one) + `,` + quote(one+two) + `]`)))
	if err != nil || len(f.TSARoots) != 2 || f.TSARoots[0] != one || f.TSARoots[1] != one+two {
		t.Fatalf("roots: %v %v", f, err)
	}
	for name, bad := range map[string]string{
		"no certificate":       quote("not pem"),
		"a key block":          quote(string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: []byte("x")}))),
		"text after the PEM":   quote(one + "trailing text"),
		"a block that is junk": quote(string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: []byte("junk")}))),
		"not a string":         "7",
	} {
		if _, err := Parse([]byte(withMembers(`"tsa_roots":[` + bad + `]`))); err == nil {
			t.Errorf("tsa root: %s accepted", name)
		}
	}
	if _, err := Parse([]byte(withMembers(`"tsa_roots":[` + quote(one+"\n\n") + `]`))); err != nil {
		t.Errorf("white space after the PEM: %v", err)
	}
	// the number of roots: 32 are read, 33 are not
	roots := func(n int) string {
		var parts []string
		for i := 0; i < n; i++ {
			parts = append(parts, quote(one))
		}
		return `"tsa_roots":[` + strings.Join(parts, ",") + `]`
	}
	if _, err := Parse([]byte(withMembers(roots(MaxTSARoots)))); err != nil {
		t.Errorf("%d roots: %v", MaxTSARoots, err)
	}
	if _, err := Parse([]byte(withMembers(roots(MaxTSARoots + 1)))); err == nil || !strings.Contains(err.Error(), "has 33 entries") {
		t.Errorf("%d roots: %v", MaxTSARoots+1, err)
	}
	// client keys: 1,024 are read, 1,025 are not, and each keeps its key
	client := func(i int) string {
		s := make([]byte, 32)
		s[0], s[1] = byte(i), byte(i>>8)
		pub := ed25519PublicOf(s)
		return fmt.Sprintf(`{"key_id":%q,"public_key":%q,"label":"c%d"}`, fp(pub)[:32], b64(pub), i)
	}
	clients := func(n int) string {
		var parts []string
		for i := 0; i < n; i++ {
			parts = append(parts, client(i))
		}
		return `"client_keys":[` + strings.Join(parts, ",") + `]`
	}
	g, err := Parse([]byte(withMembers(clients(MaxClientKeys))))
	if err != nil || len(g.ClientKeys) != MaxClientKeys {
		t.Fatalf("%d client keys: %v", MaxClientKeys, err)
	}
	if len(g.ClientKeys[7].Public) != 32 || g.ClientKeys[7].KeyID != fp(g.ClientKeys[7].Public)[:32] {
		t.Errorf("a client key lost its public key: %+v", g.ClientKeys[7])
	}
	if _, err := Parse([]byte(withMembers(clients(MaxClientKeys + 1)))); err == nil || !strings.Contains(err.Error(), "has 1025 entries") {
		t.Errorf("%d client keys: %v", MaxClientKeys+1, err)
	}
	// a client key id that is short, a key that is short, a key id that is not the key's
	good := client(3)
	for name, bad := range map[string]string{
		"short id":    strings.Replace(good, `"key_id":"`, `"key_id":"00`, 1),
		"wrong id":    strings.Replace(good, `"key_id":"`+good[len(`{"key_id":"`):len(`{"key_id":"`)+2], `"key_id":"zz`, 1),
		"short key":   `{"key_id":"00000000000000000000000000000000","public_key":"AAAA","label":"x"}`,
		"empty label": strings.Replace(good, `"label":"c3"`, `"label":""`, 1),
	} {
		if _, err := Parse([]byte(withMembers(`"client_keys":[` + bad + `]`))); err == nil {
			t.Errorf("client key: %s accepted", name)
		}
	}
	if _, err := Parse([]byte(withMembers(`"client_keys":[` + good + `,` + good + `]`))); err == nil || !strings.Contains(err.Error(), "twice") {
		t.Errorf("a client key twice: %v", err)
	}
	// the public key of a key of the file must be 32 bytes
	short := fmt.Sprintf(`{"format":1,"origin_prefix":"p.example","keys":[{"name":"k","public_key":%q,"fingerprint":%q}]}`, b64(make([]byte, 31)), fp(make([]byte, 31)))
	if _, err := Parse([]byte(short)); err == nil {
		t.Error("a public key of 31 bytes accepted")
	}
}

// The keys a held note is verified with are the keys of the file for the origin of
// the log, and only for an origin below the trusted prefix.
func TestAllKeysAndSignerKeysCheckTheOrigin(t *testing.T) {
	f, err := Parse([]byte(goodJSON()))
	if err != nil {
		t.Fatal(err)
	}
	prefix := f.OriginPrefix
	now := time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)
	for origin, ok := range map[string]bool{
		prefix:                         true,
		prefix + "/c/abc/e1":           true,
		prefix + "x":                   false,
		prefix + "x/c/abc/e1":          false,
		"other.example.com/c/abc/e1":   false,
		prefix[:len(prefix)-1] + "/c/": false,
	} {
		if _, err := f.AllKeys(origin); (err == nil) != ok {
			t.Errorf("AllKeys(%q): %v, want ok=%v", origin, err, ok)
		}
		if _, _, err := f.SignerKeys(origin, now); (err == nil) != ok {
			t.Errorf("SignerKeys(%q): %v, want ok=%v", origin, err, ok)
		}
	}
	// AllKeys returns every key whatever its standing, as note keys of the origin
	two := fmt.Sprintf(`{"format":1,"origin_prefix":"p.example","keys":[%s,%s,%s]}`,
		keyEntry(1, `,"revoked":true`), keyEntry(2, `,"not_after":"2020-01-01T00:00:00Z"`), keyEntry(3, `,"max_tree_size":5`))
	g, err := Parse([]byte(two))
	if err != nil {
		t.Fatal(err)
	}
	all, err := g.AllKeys("p.example/c/x/e1")
	if err != nil || len(all) != 3 {
		t.Fatalf("AllKeys: %v %v", all, err)
	}
	for i, k := range all {
		if k.Name != "p.example/c/x/e1" || !bytes.Equal(k.Public, g.Keys[i].Public) {
			t.Errorf("key %d: %q", i, k.Name)
		}
	}
	// SignerKeys leaves out the revoked and the retired key, and says why of each
	sk, notes, err := g.SignerKeys("p.example/c/x/e1", now)
	if err != nil || len(sk) != 1 || len(notes) != 3 {
		t.Fatalf("SignerKeys: %d keys, %d notes: %v", len(sk), len(notes), err)
	}
	joined := strings.Join(notes, "\n")
	for _, want := range []string{"is revoked", "has not_after", "counts only for checkpoints of tree size 5 or less"} {
		if !strings.Contains(joined, want) {
			t.Errorf("no note says %q:\n%s", want, joined)
		}
	}
	// a limited key that is usable is still named as limited, a key without a limit is not
	for _, k := range g.Keys {
		why := k.Why(now)
		if k.MaxTreeSize == nil && strings.Contains(why, "counts only") {
			t.Errorf("%s", why)
		}
		if k.MaxTreeSize != nil && !strings.Contains(why, "counts only for checkpoints of tree size 5 or less") {
			t.Errorf("%s", why)
		}
		if !strings.Contains(why, k.Fingerprint[:16]) || strings.Contains(why, k.Fingerprint[:17]) {
			t.Errorf("the key is named by 16 characters of its fingerprint: %s", why)
		}
	}
}

// ed25519PublicOf is the public key of the seed.
func ed25519PublicOf(seed []byte) []byte {
	return ed25519.NewKeyFromSeed(seed).Public().(ed25519.PublicKey)
}

func TestKeyMembersAndMalformedMembers(t *testing.T) {
	pub := testKey(1)
	withMax := func(v string) string {
		return fmt.Sprintf(`{"format":1,"origin_prefix":"p.example","keys":[{"name":"k","public_key":%q,"fingerprint":%q,"max_tree_size":%s}]}`, b64(pub), fp(pub), v)
	}
	for v, ok := range map[string]bool{"0": false, "1": true, "4611686018427387903": true, "4611686018427387904": false, "-1": false} {
		if _, err := Parse([]byte(withMax(v))); (err == nil) != ok {
			t.Errorf("max_tree_size %s: %v, want ok=%v", v, err, ok)
		}
	}
	// a root that holds no certificate at all, and the member that is not text
	if _, err := Parse([]byte(withMembers(`"tsa_roots":[""]`))); err == nil {
		t.Error("an empty tsa root was accepted")
	}
	good := func(i int) (string, string) {
		s := make([]byte, 32)
		s[0] = byte(i)
		p := ed25519PublicOf(s)
		return fp(p)[:32], b64(p)
	}
	id, key := good(5)
	for name, c := range map[string][3]string{
		"id not hex":     {"zzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzz", key, "x"},
		"id too short":   {id[:30], key, "x"},
		"key not base64": {id, "!!!", "x"},
		"key too short":  {id, b64(make([]byte, 31)), "x"},
	} {
		j := fmt.Sprintf(`"client_keys":[{"key_id":%q,"public_key":%q,"label":%q}]`, c[0], c[1], c[2])
		if _, err := Parse([]byte(withMembers(j))); err == nil {
			t.Errorf("client key with %s accepted", name)
		}
	}
}
