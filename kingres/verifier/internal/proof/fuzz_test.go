// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package proof

import "testing"

// FuzzVerify checks that parsing and verifying arbitrary bytes as a proof
// never panics, and that a proof that verifies keeps verifying.
func FuzzVerify(f *testing.F) {
	for _, n := range []string{"proofs/acc5.json", "proofs/acc1_at_d0.json", "proofs/evt10.json", "proofs/itm1_v1_d.json", "proofs/itm1_v1_i.json", "digests/d5.json"} {
		f.Add(readFixture(f, n))
	}
	d5 := mustDigest(f, readFixture(f, "digests/d5.json"))
	f.Fuzz(func(t *testing.T, data []byte) {
		p, err := Parse(data)
		if err != nil {
			return
		}
		_, err1 := Verify(p, d5, Expect{})
		_, err2 := Verify(p, d5, Expect{})
		if (err1 == nil) != (err2 == nil) {
			t.Error("verification is not deterministic")
		}
		// the proof read in pieces and twice says what the proof in memory says
		sp, sum, serr := ParseStream(sourceOf(data))
		if serr != nil {
			t.Fatalf("Parse accepts what ParseStream refuses: %v", serr)
		}
		_, err3 := VerifyStream(sp, sum, d5, Expect{}, sourceOf(data))
		if (err1 == nil) != (err3 == nil) {
			t.Errorf("buffered %v, streamed %v", err1, err3)
		}
		if _, _, perr := ParseStream(sourceOf(data)); perr != nil {
			t.Fatalf("not deterministic: %v", perr)
		}
	})
}
