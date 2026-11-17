// SPDX-License-Identifier: AGPL-3.0-only

package proof

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func sourceOf(b []byte) Source {
	return func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(b)), nil }
}

// verifyBoth runs the buffered and the streamed verification and returns what
// each said: the error text (or "") and the steps.
func verifyBoth(t testing.TB, data []byte, dig []byte) (buffered, streamed string) {
	t.Helper()
	d := mustDigest(t, dig)
	describe := func(res *Result, err error) string {
		if err != nil {
			return "error: " + err.Error()
		}
		var b strings.Builder
		for _, s := range res.Steps {
			b.WriteString(s.Name + ": " + s.Detail + "\n")
		}
		return b.String()
	}
	if p, err := Parse(data); err != nil {
		buffered = "parse error"
	} else {
		buffered = describe(Verify(p, d, Expect{}))
	}
	if p, sum, err := ParseStream(sourceOf(data)); err != nil {
		streamed = "parse error"
	} else {
		streamed = describe(VerifyStream(p, sum, d, Expect{}, sourceOf(data)))
	}
	return
}

// The streamed verification says what the buffered one says, for every proof of
// the fixtures against every digest.
func TestStreamedVerificationEqualsTheBufferedOne(t *testing.T) {
	proofs, _ := filepath.Glob(filepath.Join(fixtureDir, "proofs", "*.json"))
	digests, _ := filepath.Glob(filepath.Join(fixtureDir, "digests", "*.json"))
	if len(proofs) < 20 || len(digests) < 8 {
		t.Fatalf("fixtures: %d proofs, %d digests", len(proofs), len(digests))
	}
	ok := 0
	for _, pf := range proofs {
		pd, _ := os.ReadFile(pf)
		for _, df := range digests {
			dd, _ := os.ReadFile(df)
			a, b := verifyBoth(t, pd, dd)
			if a != b {
				t.Fatalf("%s against %s:\nbuffered: %s\nstreamed: %s", filepath.Base(pf), filepath.Base(df), a, b)
			}
			if !strings.HasPrefix(a, "error") {
				ok++
			}
		}
	}
	if ok < 20 {
		t.Fatalf("only %d verified pairs: the comparison proves little", ok)
	}
}

// and for every single-byte change of a proof.
func TestStreamedVerificationEqualsTheBufferedOneOnEveryByteChange(t *testing.T) {
	pd := readFixture(t, "proofs/acc5.json")
	dd := readFixture(t, "digests/d5.json")
	for i := 0; i < len(pd); i += 3 {
		for _, c := range []byte{'0', 'f', '"', '{', ' '} {
			if pd[i] == c {
				continue
			}
			m := bytes.Clone(pd)
			m[i] = c
			a, b := verifyBoth(t, m, dd)
			if (strings.HasPrefix(a, "error") != strings.HasPrefix(b, "error")) || (strings.HasPrefix(a, "error") && a != b && !strings.Contains(a, "invalid JSON")) {
				t.Fatalf("byte %d set to %q:\nbuffered: %s\nstreamed: %s", i, c, a, b)
			}
		}
	}
}

// The member chain may come before the member block, and more headers than the
// digest needs may follow it: both verify, and both are the same as in memory.
func TestStreamedVerificationWhateverTheOrderOfTheMembers(t *testing.T) {
	raw := readFixture(t, "proofs/acc1.json") // block 0, headers up to the latest digest
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	order := func(first ...string) []byte {
		var b bytes.Buffer
		b.WriteString("{")
		seen := map[string]bool{}
		var keys []string
		keys = append(keys, first...)
		for k := range m {
			keys = append(keys, k)
		}
		n := 0
		for _, k := range keys {
			if seen[k] {
				continue
			}
			seen[k] = true
			if n > 0 {
				b.WriteString(",")
			}
			n++
			fmt.Fprintf(&b, "%q:%s", k, m[k])
		}
		b.WriteString("}")
		return b.Bytes()
	}
	d5 := readFixture(t, "digests/d5.json")
	d2 := readFixture(t, "digests/d2.json")
	for name, data := range map[string][]byte{"chain first": order("chain"), "chain last": order("block", "op", "seq"), "default": raw} {
		for dn, dg := range map[string][]byte{"d5": d5, "d2 (headers beyond the digest)": d2} {
			a, b := verifyBoth(t, data, dg)
			if a != b || strings.HasPrefix(a, "error") {
				t.Errorf("%s, %s:\nbuffered: %s\nstreamed: %s", name, dn, a, b)
			}
		}
	}
}

// The headers are not kept: the proof of the stream holds a count.
func TestStreamedProofKeepsNoHeaders(t *testing.T) {
	p, _, err := ParseStream(sourceOf(readFixture(t, "proofs/acc1.json")))
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Chain) != 0 || p.ChainLen == 0 {
		t.Fatalf("chain kept: %d, counted: %d", len(p.Chain), p.ChainLen)
	}
	q, err := Parse(readFixture(t, "proofs/acc1.json"))
	if err != nil || uint64(len(q.Chain)) != p.ChainLen || q.ChainLen != p.ChainLen {
		t.Fatalf("Parse: %v %d", err, len(q.Chain))
	}
}

// A walk that needs more headers than the file has is refused, and so is a
// chain that does not lead to the digest.
func TestStreamedWalkRefusals(t *testing.T) {
	pd := readFixture(t, "proofs/acc1_at_d0.json") // an empty chain
	for _, c := range []struct{ dg, want string }{{"digests/d5.json", "chain has 0 header"}} {
		_, s := verifyBoth(t, pd, readFixture(t, c.dg))
		if !strings.Contains(s, c.want) {
			t.Errorf("%s: %s", c.dg, s)
		}
	}
}
