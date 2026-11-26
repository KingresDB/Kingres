// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package chain

import (
	"fmt"
	"sort"
)

// NamedDigest is a digest and the name of the file it came from.
type NamedDigest struct {
	Name string
	Digest
}

// CheckDigestSet checks several digests of one chain for mutual consistency
// as far as digests alone allow:
//
//   - all have the same chain id;
//   - sorted by height, block times do not decrease (every digest must carry
//     block_closed_at), and equal heights have equal block hashes;
//   - when every digest carries generated_at, sorted by it (ties by input
//     order) the heights do not decrease, which would otherwise mean the
//     database went back to an earlier state.
//
// It returns one message per problem; no message means the set is consistent.
// This proves nothing about the blocks between the digests, about any row, or
// that the digests are authentic.
func CheckDigestSet(ds []NamedDigest) []string {
	var problems []string
	if len(ds) < 2 {
		return []string{fmt.Sprintf("%d digest(s) given, expected at least 2", len(ds))}
	}
	for _, d := range ds {
		if d.ChainID != ds[0].ChainID {
			problems = append(problems, fmt.Sprintf("%s: chain_id %x differs from %x of %s", d.Name, d.ChainID, ds[0].ChainID, ds[0].Name))
		}
		if !d.HasClosedAt {
			problems = append(problems, fmt.Sprintf("%s: no block_closed_at, expected it for the block time check", d.Name))
		}
	}
	byHeight := append([]NamedDigest(nil), ds...)
	sort.SliceStable(byHeight, func(i, j int) bool { return byHeight[i].Height < byHeight[j].Height })
	for i := 1; i < len(byHeight); i++ {
		a, b := byHeight[i-1], byHeight[i]
		if a.Height == b.Height && a.BlockHash != b.BlockHash {
			problems = append(problems, fmt.Sprintf("height %d: %s has block_hash %x but %s has %x", a.Height, a.Name, a.BlockHash, b.Name, b.BlockHash))
		}
		if a.Height == b.Height && a.HasClosedAt && b.HasClosedAt && a.ClosedAtUS != b.ClosedAtUS {
			problems = append(problems, fmt.Sprintf("height %d: %s and %s have different block_closed_at", a.Height, a.Name, b.Name))
		}
		if a.HasClosedAt && b.HasClosedAt && b.ClosedAtUS < a.ClosedAtUS {
			problems = append(problems, fmt.Sprintf("%s (height %d) has an earlier block time than %s (height %d), expected block times that never go back", b.Name, b.Height, a.Name, a.Height))
		}
	}
	all := true
	for _, d := range ds {
		all = all && d.HasGeneratedAt
	}
	if all {
		byGen := append([]NamedDigest(nil), ds...)
		sort.SliceStable(byGen, func(i, j int) bool { return byGen[i].GeneratedAtUS < byGen[j].GeneratedAtUS })
		for i := 1; i < len(byGen); i++ {
			if byGen[i].Height < byGen[i-1].Height {
				problems = append(problems, fmt.Sprintf("%s was generated after %s but has a smaller height (%d < %d): the ledger went back", byGen[i].Name, byGen[i-1].Name, byGen[i].Height, byGen[i-1].Height))
			}
		}
	}
	return problems
}
