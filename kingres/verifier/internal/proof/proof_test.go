// SPDX-License-Identifier: AGPL-3.0-only

package proof

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/KingresDB/Kingres/kingres/verifier/internal/chain"
)

const fixtureDir = "../../testdata/fixtures"

func readFixture(t testing.TB, rel string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(fixtureDir, rel))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func mustDigest(t testing.TB, data []byte) chain.Digest {
	t.Helper()
	d, err := chain.ParseDigest(data)
	if err != nil {
		t.Fatalf("digest: %v", err)
	}
	return d
}

// verifyBytes parses and verifies; it returns the proof (nil when parsing
// failed) and the error.
func verifyBytes(proofData []byte, d chain.Digest) (*Proof, error) {
	p, err := Parse(proofData)
	if err != nil {
		return nil, &Error{Step: StepParse, Err: err}
	}
	_, err = Verify(p, d, Expect{})
	return p, err
}

func TestVerifyFixtures(t *testing.T) {
	d5 := mustDigest(t, readFixture(t, "digests/d5.json"))
	d0 := mustDigest(t, readFixture(t, "digests/d0.json"))
	tests := []struct {
		name   string
		proof  string
		digest chain.Digest
		wantOK bool
		step   string
	}{
		{"block 1 row against the latest digest", "proofs/acc5.json", d5, true, ""},
		{"block 0 row against the latest digest", "proofs/acc1.json", d5, true, ""},
		{"proof made at the digest, empty chain", "proofs/acc1_at_d0.json", d0, true, ""},
		{"proof made at the digest, against a later digest", "proofs/acc1_at_d0.json", d5, false, StepChainWalk},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := verifyBytes(readFixture(t, tt.proof), tt.digest)
			if tt.wantOK {
				if err != nil {
					t.Fatalf("verification failed: %v", err)
				}
				return
			}
			if !IsStep(err, tt.step) {
				t.Errorf("err = %v, want a failure at step %s", err, tt.step)
			}
		})
	}
}

func TestVerifyBinding(t *testing.T) {
	d5 := mustDigest(t, readFixture(t, "digests/d5.json"))
	p, err := Parse(readFixture(t, "proofs/acc5.json"))
	if err != nil {
		t.Fatal(err)
	}
	table, row := p.TableID, p.RowHash
	otherTable := table + 1
	otherRow := row
	otherRow[0] ^= 1
	tests := []struct {
		name string
		exp  Expect
		ok   bool
	}{
		{"matching table and row", Expect{TableID: &table, RowHash: &row}, true},
		{"table only", Expect{TableID: &table}, true},
		{"other table", Expect{TableID: &otherTable}, false},
		{"other row", Expect{RowHash: &otherRow}, false},
		{"right table, other row", Expect{TableID: &table, RowHash: &otherRow}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Verify(p, d5, tt.exp)
			if (err == nil) != tt.ok {
				t.Fatalf("err = %v, want ok = %v", err, tt.ok)
			}
			if err != nil && !IsStep(err, StepBinding) {
				t.Errorf("err = %v, want step %s", err, StepBinding)
			}
		})
	}
}

func TestVerifyDigestMismatches(t *testing.T) {
	proofData := readFixture(t, "proofs/acc5.json")
	d5 := mustDigest(t, readFixture(t, "digests/d5.json"))
	d0 := mustDigest(t, readFixture(t, "digests/d0.json"))
	d1 := mustDigest(t, readFixture(t, "digests/d1.json"))
	other := mustDigest(t, readFixture(t, "digests/other_chain.json"))
	if _, err := verifyBytes(proofData, d1); err != nil {
		t.Errorf("digest of the proof's own block: %v", err)
	}
	if _, err := verifyBytes(proofData, d0); !IsStep(err, StepChainWalk) {
		t.Errorf("earlier digest: err = %v, want step %s", err, StepChainWalk)
	}
	if _, err := verifyBytes(proofData, other); !IsStep(err, StepChainID) {
		t.Errorf("digest of another chain: err = %v, want step %s", err, StepChainID)
	}
	wrongHash := d5
	wrongHash.BlockHash[5] ^= 0x80
	if _, err := verifyBytes(proofData, wrongHash); !IsStep(err, StepChainWalk) {
		t.Errorf("wrong digest hash: err = %v, want step %s", err, StepChainWalk)
	}
	wrongTime := d5
	wrongTime.ClosedAtUS++
	if _, err := verifyBytes(proofData, wrongTime); !IsStep(err, StepDigestTime) {
		t.Errorf("wrong digest block time: err = %v, want step %s", err, StepDigestTime)
	}
}

// ---- generic single-field mutation ----

type site struct {
	path []any // string keys and int indexes
}

func decodeAny(t testing.TB, data []byte) any {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		t.Fatal(err)
	}
	return v
}

func collect(v any, path []any, out *[]site) {
	*out = append(*out, site{path: append([]any(nil), path...)})
	switch x := v.(type) {
	case map[string]any:
		for k, c := range x {
			collect(c, append(path, k), out)
		}
	case []any:
		for i, c := range x {
			collect(c, append(path, i), out)
		}
	}
}

func get(v any, path []any) any {
	for _, p := range path {
		switch k := p.(type) {
		case string:
			v = v.(map[string]any)[k]
		case int:
			v = v.([]any)[k]
		}
	}
	return v
}

// deepCopy copies a decoded JSON value.
func deepCopy(v any) any {
	switch x := v.(type) {
	case map[string]any:
		m := make(map[string]any, len(x))
		for k, c := range x {
			m[k] = deepCopy(c)
		}
		return m
	case []any:
		a := make([]any, len(x))
		for i, c := range x {
			a[i] = deepCopy(c)
		}
		return a
	}
	return v
}

// setAt replaces (or, with remove, deletes) the value at path in a copy of
// root.
func setAt(root any, path []any, val any, remove bool) any {
	root = deepCopy(root)
	if len(path) == 0 {
		return val
	}
	parent := get(root, path[:len(path)-1])
	switch k := path[len(path)-1].(type) {
	case string:
		m := parent.(map[string]any)
		if remove {
			delete(m, k)
		} else {
			m[k] = val
		}
	case int:
		a := parent.([]any)
		if remove {
			// Removing an element needs the grandparent; handled by the caller.
			panic("use removeElem")
		}
		a[k] = val
	}
	return root
}

func removeElem(root any, path []any) any {
	root = deepCopy(root)
	idx := path[len(path)-1].(int)
	gp := path[:len(path)-1]
	arr := get(root, gp).([]any)
	arr = append(arr[:idx:idx], arr[idx+1:]...)
	return setAt(root, gp, arr, false)
}

type mutation struct {
	name string
	root any
}

// mutationsAt lists single-field mutations of root at s.
func mutationsAt(root any, s site) []mutation {
	var out []mutation
	add := func(name string, val any) {
		if len(s.path) == 0 {
			out = append(out, mutation{name, val})
			return
		}
		out = append(out, mutation{name, setAt(root, s.path, val, false)})
	}
	cur := get(root, s.path)
	add("null", nil)
	add("string-x", "x")
	add("number-1", json.Number("1"))
	add("true", true)
	add("empty-array", []any{})
	add("empty-object", map[string]any{})
	switch x := cur.(type) {
	case string:
		if len(x) > 0 {
			c := x[0]
			r := byte('0')
			if c == '0' {
				r = '1'
			}
			add("first-char", string(r)+x[1:])
			add("last-char", x[:len(x)-1]+string(func() byte {
				if x[len(x)-1] == '0' {
					return '1'
				}
				return '0'
			}()))
			add("truncate", x[:len(x)-1])
		}
		add("append", x+"00")
		add("empty", "")
		add("upper", strings.ToUpper(x)+"Q")
	case json.Number:
		n, ok := new(big.Int).SetString(string(x), 10)
		if ok {
			add("plus-1", json.Number(new(big.Int).Add(n, big.NewInt(1)).String()))
			add("minus-1", json.Number(new(big.Int).Sub(n, big.NewInt(1)).String()))
		}
		add("zero", json.Number("0"))
		add("negative", json.Number("-1"))
		add("fraction", json.Number(string(x)+".5"))
		add("integral-fraction", json.Number(string(x)+".0"))
		add("exponent", json.Number(string(x)+"e0"))
		add("huge", json.Number("99999999999999999999999999"))
		add("max-int64", json.Number("9223372036854775807"))
		add("max-int64+1", json.Number("9223372036854775808"))
		add("as-string", string(x))
	case []any:
		if len(x) > 0 {
			out = append(out, mutation{"drop-last", setAt(root, s.path, x[:len(x)-1:len(x)-1], false)})
			out = append(out, mutation{"drop-first", setAt(root, s.path, x[1:], false)})
			out = append(out, mutation{"duplicate-first", setAt(root, s.path, append(append([]any{}, x...), x[0]), false)})
		}
		if len(x) > 1 {
			sw := append([]any{}, x...)
			sw[0], sw[1] = sw[1], sw[0]
			out = append(out, mutation{"swap-first-two", setAt(root, s.path, sw, false)})
		}
	}
	// Removing the member or element.
	if len(s.path) > 0 {
		switch s.path[len(s.path)-1].(type) {
		case string:
			out = append(out, mutation{"remove", setAt(root, s.path, nil, true)})
		case int:
			out = append(out, mutation{"remove", removeElem(root, s.path)})
		}
	}
	return out
}

func marshal(t testing.TB, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func pathString(p []any) string {
	var sb strings.Builder
	sb.WriteString("$")
	for _, e := range p {
		switch k := e.(type) {
		case string:
			sb.WriteString("." + k)
		case int:
			fmt.Fprintf(&sb, "[%d]", k)
		}
	}
	return sb.String()
}

func TestProofFieldMutations(t *testing.T) {
	cases := []struct{ proof, digest string }{
		{"proofs/acc5.json", "digests/d8.json"},       // block 1, all later headers (the proof reaches d8)
		{"proofs/acc1_at_d0.json", "digests/d0.json"}, // block 0, empty chain
		{"proofs/evt10.json", "digests/d8.json"},      // a block of the first ledger, with later headers
		{"proofs/itm1_v1_d.json", "digests/d8.json"},  // a D proof
	}
	for _, c := range cases {
		t.Run(c.proof, func(t *testing.T) {
			root := decodeAny(t, readFixture(t, c.proof))
			d := mustDigest(t, readFixture(t, c.digest))
			if _, err := verifyBytes(marshal(t, root), d); err != nil {
				t.Fatalf("unmodified proof does not verify: %v", err)
			}
			var sites []site
			collect(root, nil, &sites)
			total := 0
			base := string(marshal(t, root))
			for _, s := range sites {
				for _, m := range mutationsAt(root, s) {
					mb := marshal(t, m.root)
					if string(mb) == base {
						continue // the mutation produced the original value
					}
					total++
					_, err := verifyBytes(mb, d)
					// relation is informational. A header appended to chain after the
					// digest height is not looked at: a proof made after the digest
					// legitimately has such headers.
					allowed := pathString(s.path) == "$.relation" || (pathString(s.path) == "$.chain" && m.name == "duplicate-first")
					if err == nil && !allowed {
						t.Errorf("%s %s: the mutated proof verified", pathString(s.path), m.name)
					}
				}
			}
			t.Logf("%d single-field mutations rejected", total)
		})
	}
}

func TestDigestFieldMutations(t *testing.T) {
	proofData := readFixture(t, "proofs/acc5.json")
	root := decodeAny(t, readFixture(t, "digests/d5.json"))
	var sites []site
	collect(root, nil, &sites)
	for _, s := range sites {
		name := pathString(s.path)
		for _, m := range mutationsAt(root, s) {
			if string(marshal(t, m.root)) == string(marshal(t, root)) {
				continue
			}
			d, err := chain.ParseDigest(marshal(t, m.root))
			if err == nil {
				_, err = verifyBytes(proofData, d)
			}
			informational := name == "$.database" || name == "$.generated_at"
			optionalRemoved := m.name == "remove" && (name == "$.block_closed_at")
			if err == nil && !informational && !optionalRemoved {
				t.Errorf("digest %s %s: verified", name, m.name)
			}
		}
	}
}

// TestEveryByteMutation changes every byte of a fixture proof in several ways
// and requires that the result is rejected, unless it is the same proof (the
// informational relation field aside).
func TestEveryByteMutation(t *testing.T) {
	cases := []struct{ proof, digest string }{
		{"proofs/acc5.json", "digests/d8.json"},
		{"proofs/acc1_at_d0.json", "digests/d0.json"},
		{"proofs/itm1_v1_d.json", "digests/d8.json"},
	}
	for _, c := range cases {
		t.Run(c.proof, func(t *testing.T) {
			orig := readFixture(t, c.proof)
			d := mustDigest(t, readFixture(t, c.digest))
			want, err := verifyBytes(orig, d)
			if err != nil {
				t.Fatal(err)
			}
			want.Relation = ""
			accepted := 0
			for i := range orig {
				for _, edit := range []func(byte) byte{
					func(b byte) byte { return b ^ 0x01 },
					func(b byte) byte { return b ^ 0x20 },
					func(b byte) byte { return b ^ 0x80 },
					func(b byte) byte { return 'z' },
					func(b byte) byte { return '0' + (b+1)%10 },
				} {
					m := append([]byte(nil), orig...)
					m[i] = edit(m[i])
					if bytes.Equal(m, orig) {
						continue
					}
					p, err := verifyBytes(m, d)
					if err != nil {
						continue
					}
					p.Relation = ""
					if !reflect.DeepEqual(p, want) {
						t.Fatalf("byte %d (%q -> %q): verified with different content", i, orig[i], m[i])
					}
					accepted++
				}
			}
			t.Logf("%d bytes, %d byte edits left the proof's meaning unchanged", len(orig), accepted)
		})
	}
}

func TestParseRejectsNumbers(t *testing.T) {
	good := readFixture(t, "proofs/acc5.json")
	for _, tt := range []struct{ name, from, to string }{
		{"fractional seq", `"seq": 0`, `"seq": 0.5`},
		{"integral fraction", `"seq": 0`, `"seq": 0.0`},
		{"exponent", `"seq": 0`, `"seq": 0e0`},
		{"huge", `"seq": 0`, `"seq": 99999999999999999999`},
		{"beyond int4", `"seq": 0`, `"seq": 2147483648`},
		{"negative", `"seq": 0`, `"seq": -1`},
		{"huge table id", `"table_id": 1`, `"table_id": 4294967297`},
		{"huge txid", `"ledger_txid": 767`, `"ledger_txid": 9223372036854775808`},
		{"leading zero", `"seq": 0`, `"seq": 00`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if !bytes.Contains(good, []byte(tt.from)) {
				t.Fatalf("fixture does not contain %s", tt.from)
			}
			m := bytes.Replace(good, []byte(tt.from), []byte(tt.to), 1)
			if _, err := Parse(m); err == nil {
				t.Error("want an error")
			}
		})
	}
}

func TestParseRejectsStructure(t *testing.T) {
	good := string(readFixture(t, "proofs/acc5.json"))
	for _, tt := range []struct{ name, in string }{
		{"empty", ""},
		{"array", "[]"},
		{"duplicate key", strings.Replace(good, `"op": "I"`, `"op": "I", "op": "I"`, 1)},
		{"op X", strings.Replace(good, `"op": "I"`, `"op": "X"`, 1)},
		{"op lower case", strings.Replace(good, `"op": "I"`, `"op": "d"`, 1)},
		{"op empty", strings.Replace(good, `"op": "I"`, `"op": ""`, 1)},
		{"op two letters", strings.Replace(good, `"op": "I"`, `"op": "ID"`, 1)},
		{"format 2", strings.Replace(good, `"format": 1`, `"format": 2`, 1)},
		{"upper case hex", strings.Replace(good, `"row_hash": "5`, `"row_hash": "A`, 1)},
		{"short hash", strings.Replace(good, `"row_hash": "56`, `"row_hash": "5`, 1)},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := Parse([]byte(tt.in)); err == nil {
				t.Error("want an error")
			}
		})
	}
}

func TestErrorWrapping(t *testing.T) {
	e := &Error{Step: StepParse, Err: errors.New("x")}
	if !IsStep(e, StepParse) || IsStep(e, StepChainID) || IsStep(nil, StepParse) {
		t.Error("IsStep")
	}
	if e.Error() != "parse: x" {
		t.Errorf("Error() = %q", e.Error())
	}
}

func TestParseAcceptsInsertAndDelete(t *testing.T) {
	for name, want := range map[string]byte{
		"proofs/acc5.json":           0x49,
		"proofs/itm1_v1_i.json":      0x49,
		"proofs/itm1_v1_d.json":      0x44,
		"proofs/itm3_deleted_d.json": 0x44,
	} {
		p, err := Parse(readFixture(t, name))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if p.Op != want {
			t.Errorf("%s: operation byte %#x, want %#x", name, p.Op, want)
		}
	}
	if OpName(0x49) != "I" || OpName(0x44) != "D" || OpName(0x58) != "0x58" {
		t.Error("OpName")
	}
}
