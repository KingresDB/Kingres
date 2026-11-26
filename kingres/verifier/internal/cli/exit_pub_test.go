// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package cli

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/KingresDB/Kingres/kingres/verifier/internal/aggregate"
	"github.com/KingresDB/Kingres/kingres/verifier/internal/testdir"
)

// publicationRows are the rows of exitRows for the commands of phase 7 step 7 and step 18: each exit site of
// their run functions with a scenario that reaches it with its status.
func publicationRows(x *exitEnv) []exitRow {
	fn := func(site string, status int, f func(t *testing.T) []string) exitRow {
		return exitRow{site: site, status: status, args: f}
	}
	var rows []exitRow
	add := func(r ...exitRow) { rows = append(rows, r...) }
	add(flagCases("Aggregate", "aggregate")...)
	add(flagCases("Anchors", "anchors")...)
	add(flagCases("BundleVerify", "bundle", "verify")...)
	add(flagCases("BundleFetch", "bundle", "fetch")...)
	add(flagCases("Monitor", "monitor")...)
	add(flagCases("Attestation", "attestation")...)
	add(flagCases("TrustFollow", "trust", "follow")...)

	// a world with one chain and an aggregate whose member is wrong
	badAggregate := func(t *testing.T) *world {
		w := newWorld(t)
		a := makeChain(0xa1, 5, nil)
		w.addChain(a, 5)
		m := member(a, 5, 0)
		m.LogRoot[0] ^= 1
		w.appendRound(1, m)
		return w
	}
	okWorld := func(t *testing.T) *world { w, _, _ := stdWorld(t); w.bundleJSON(); return w }

	add(
		fn("runBundle:exitUsage#1", 2, func(t *testing.T) []string { return []string{"bundle"} }),
		fn("runBundle:exitUsage#2", 2, func(t *testing.T) []string { return []string{"bundle", "nonsense"} }),
	)
	add(
		fn("runAggregate:inputError#1", 2, func(t *testing.T) []string { return []string{"aggregate"} }),
		fn("runAggregate:finishFailed#1", 1, func(t *testing.T) []string { return badAggregate(t).aggArgs() }),
		fn("runAggregate:finishIncomplete#1", 3, func(t *testing.T) []string {
			w := newWorld(t)
			a := makeChain(0xa1, 5, nil)
			w.addChain(a, 5)
			w.appendRound(1, member(a, 5, aggregate.FlagStale))
			return w.aggArgs()
		}),

		fn("runAnchors:inputError#1", 2, func(t *testing.T) []string { return []string{"anchors"} }),
		fn("runAnchors:finishFailed#1", 1, func(t *testing.T) []string {
			aw := newAnchorWorld(t)
			aw.put(2, "x1", []byte(`{"format":1}`))
			return aw.args(aw.trust)
		}),
		fn("runAnchors:finishIncomplete#1", 3, func(t *testing.T) []string {
			aw := newAnchorWorld(t)
			return aw.args(aw.trust)
		}),

		fn("runBundleVerify:inputError#1", 2, func(t *testing.T) []string { return []string{"bundle", "verify"} }),
		fn("runBundleVerify:finishFailed#1", 1, func(t *testing.T) []string {
			w := okWorld(t)
			b, _ := os.ReadFile(filepath.Join(w.dir, "bundle.json"))
			w.put("bundle.json", append([]byte("x"), b...))
			return w.bundleArgs()
		}),
		fn("runBundleVerify:finishIncomplete#1", 3, func(t *testing.T) []string { return okWorld(t).bundleArgs("--require-anchors", "1") }),

		fn("runBundleFetch:inputError#1", 2, func(t *testing.T) []string { return []string{"bundle", "fetch"} }),
		fn("runBundleFetch:finishFailed#1", 1, func(t *testing.T) []string {
			w := newWorld(t)
			a := makeChain(0xa1, 5, nil)
			w.addChain(a, 5)
			w.put("c/"+a.idHex+"/e1/checkpoint", []byte("garbage"))
			return []string{"bundle", "fetch", "--source", w.dir, "--out", filepath.Join(testdir.New(t), "out"), "--log", "chain:" + a.idHex + ":e1"}
		}),
		fn("runBundleFetch:finishIncomplete#1", 3, func(t *testing.T) []string {
			w := newWorld(t)
			return []string{"bundle", "fetch", "--source", w.dir, "--out", filepath.Join(testdir.New(t), "out"), "--log", "chain:" + makeChain(0xa1, 1, nil).idHex + ":e1"}
		}),

		fn("runMonitor:inputError#1", 2, func(t *testing.T) []string { return []string{"monitor"} }),
		fn("runMonitor:finishFailed#1", 1, func(t *testing.T) []string {
			w, _, _ := stdWorld(t)
			w.trust = w.writeTrust(newSigner(9)) // pins another key: the checkpoint does not verify
			return []string{"monitor", "--trust", w.trust, "--source", w.dir, "--state", filepath.Join(testdir.New(t), "s"), "--init", "--once", "--log", "aggregate:" + w.aggID}
		}),
		fn("runMonitor:finishIncomplete#1", 3, func(t *testing.T) []string {
			w := newWorld(t)
			return []string{"monitor", "--trust", w.trust, "--source", w.dir, "--state", filepath.Join(testdir.New(t), "s"), "--init", "--once", "--log", "aggregate:" + w.aggID}
		}),

		fn("runAttestation:inputError#1", 2, func(t *testing.T) []string { return []string{"attestation"} }),
		fn("runAttestation:finishFailed#1", 1, func(t *testing.T) []string {
			f := newAtt(t, nil)
			return f.args(f.trustUnpinned)
		}),
		fn("runAttestation:finishIncomplete#1", 3, func(t *testing.T) []string {
			f := newAtt(t, nil)
			_ = os.Remove(filepath.Join(f.w.dir, "c", f.chain.idHex, "e1", "checkpoint"))
			return f.args(f.trustPinned)
		}),

		fn("runTrustFollow:inputError#1", 2, func(t *testing.T) []string { return []string{"trust", "follow"} }),
		fn("runTrustFollow:finishFailed#1", 1, func(t *testing.T) []string {
			w := newWorld(t)
			w.put("events/000000", []byte("garbage"))
			return w.followArgs(newOut(t))
		}),
		fn("runTrustFollow:finishIncomplete#1", 3, func(t *testing.T) []string { return newWorld(t).followArgs(newOut(t)) }),

		fn("runProofLeg:inputError#1", 2, func(t *testing.T) []string {
			return []string{"proof", "--proof", x.proof, "--bundle", x.p.dir}
		}),
		fn("runProofLeg:finishFailed#1", 1, func(t *testing.T) []string {
			w, a, _ := stdWorld(t)
			pf := filepath.Join(testdir.New(t), "p.json")
			_ = os.WriteFile(pf, a.proofJSON(3, nil, 0), 0o600)
			return []string{"proof", "--proof", pf, "--bundle", w.dir, "--trust", w.trust, "--log-leg", "chain", "--table-id", "8", "--now", "2026-11-19T12:00:00Z"}
		}),
		fn("runProofLeg:finishIncomplete#1", 3, func(t *testing.T) []string {
			w, a, _ := stdWorld(t)
			_ = os.Remove(filepath.Join(w.dir, "c", a.idHex, "e1", "checkpoint"))
			pf := filepath.Join(testdir.New(t), "p.json")
			_ = os.WriteFile(pf, a.proofJSON(3, nil, 0), 0o600)
			return []string{"proof", "--proof", pf, "--bundle", w.dir, "--trust", w.trust, "--log-leg", "chain", "--now", "2026-11-19T12:00:00Z"}
		}),
	)
	return rows
}
