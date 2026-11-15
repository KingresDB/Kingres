// SPDX-License-Identifier: AGPL-3.0-only

package chain

import "testing"

// FuzzParseDigest checks that ParseDigest never panics.
func FuzzParseDigest(f *testing.F) {
	f.Add([]byte(goodDigest))
	f.Add(fixture(f, "digests/d5.json"))
	f.Fuzz(func(t *testing.T, data []byte) {
		_, _ = ParseDigest(data)
	})
}

// FuzzParseBlocks checks that ParseBlocks and VerifyBlocks never panic.
func FuzzParseBlocks(f *testing.F) {
	f.Add(fixture(f, "blocks.jsonl"))
	f.Fuzz(func(t *testing.T, data []byte) {
		hs, err := ParseBlocks(data)
		if err != nil {
			return
		}
		_ = VerifyBlocks(hs, [32]byte{})
		_ = CheckDigestOnBlocks(hs, Digest{})
	})
}
