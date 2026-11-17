// SPDX-License-Identifier: AGPL-3.0-only

package cli

import (
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/KingresDB/Kingres/kingres/verifier/internal/statefile"
)

// exitRow says what one exit site of the source ends a run with, and how to
// make a run reach it. TestEveryExitSiteHasARowAndAScenario reads the sites
// from the source (sites_test.go), so a new place that can end a run with a
// status has no row and the test fails; a row whose scenario does not reach
// its site with its status fails too. A site that no portable scenario can
// reach is a row with a reason in why, and nothing else.
type exitRow struct {
	site    string
	status  int
	args    func(t *testing.T) []string
	why     string // set when no scenario reaches it: the reason
	unix    bool   // the scenario works only where file modes are permissions
	failOut bool   // the standard output refuses every write
}

type exitEnv struct {
	t      *testing.T
	p      *publication
	oldN   string // the note of size 300
	digest string
	proof  string
}

func newExitEnv(t *testing.T) *exitEnv {
	p := newPublication(t, 300, 700)
	return &exitEnv{t: t, p: p, oldN: filepath.Join(p.logDir, "old.note"), digest: fx("digests/d8.json"), proof: fx("proofs/itm1_v1_d.json")}
}

func (x *exitEnv) cp(more ...string) []string {
	return append([]string{"checkpoint", "--trust", x.p.trust, "--log", x.p.log(), "--source", x.p.dir}, more...)
}

func (x *exitEnv) co(more ...string) []string {
	return append([]string{"consistency", "--trust", x.p.trust, "--log", x.p.log(), "--source", x.p.dir}, more...)
}

// stateAt300 makes a state of size 300 in a fresh directory and returns its path.
func (x *exitEnv) stateAt300() string {
	st := filepath.Join(x.t.TempDir(), "state")
	if code, out, errs := run(x.t, x.cp("--note", x.oldN, "--state", st)...); code != 0 {
		x.t.Fatalf("seed: %d %s %s", code, out, errs)
	}
	return st
}

func (x *exitEnv) stateAt700() string {
	st := filepath.Join(x.t.TempDir(), "state")
	if code, out, errs := run(x.t, x.cp("--state", st)...); code != 0 {
		x.t.Fatalf("seed: %d %s %s", code, out, errs)
	}
	return st
}

// takeNames fills every name the evidence of a conflict of this size and root
// could be saved under with a file of other content.
func takeNames(t *testing.T, state string, size int, root h32) {
	t.Helper()
	base := statefile.ConflictPath(state, uint64(size), root[:])
	names := []string{base}
	for i := 2; i <= 8; i++ {
		names = append(names, fmt.Sprintf("%s-%d", base, i))
	}
	for _, n := range names {
		if err := os.WriteFile(n, []byte("not the evidence"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

// forkSource is a publication whose log of size 400 forks the honest one at 5.
func (x *exitEnv) forkSource() (dir string, root h32) {
	dir = x.t.TempDir()
	all := leavesSalt(400, 5, "evil")
	publishLog(x.t, dir, x.p.signer, all, 400)
	return dir, refMTH(all[:400])
}

func (x *exitEnv) damageTiles() {
	_ = filepath.Walk(filepath.Join(x.p.logDir, "tile"), func(f string, fi os.FileInfo, err error) error {
		if err == nil && !fi.IsDir() {
			_ = os.WriteFile(f, []byte("short"), 0o644)
		}
		return nil
	})
}

func flagCases(cmd string, argv ...string) []exitRow {
	return []exitRow{
		{site: "run" + cmd + ":parseFlags#1", status: 2, args: func(*testing.T) []string { return append(append([]string{}, argv...), "--nope") }},
		{site: "run" + cmd + ":parseFlags#1", status: 0, args: func(*testing.T) []string { return append(append([]string{}, argv...), "--help") }},
	}
}

func exitRows(x *exitEnv) []exitRow {
	t := x.t
	d0 := fx("digests/d0.json")
	tmp := func(name, content string) string { return writeTemp(t, t.TempDir(), name, content) }
	missing := func() string { return filepath.Join(t.TempDir(), "no-such-file") }
	zero := strings.Repeat("00", 32)
	b64 := func(b []byte) string { return base64.StdEncoding.EncodeToString(b) }
	someRoot := sha([]byte("some root"))
	nonEmptyRoot := b64(someRoot[:])
	var rows []exitRow
	add := func(r ...exitRow) { rows = append(rows, r...) }
	row := func(site string, status int, args ...string) exitRow {
		return exitRow{site: site, status: status, args: func(*testing.T) []string { return args }}
	}
	fn := func(site string, status int, f func(t *testing.T) []string) exitRow {
		return exitRow{site: site, status: status, args: f}
	}
	never := func(site string, status int, why string) exitRow {
		return exitRow{site: site, status: status, why: why}
	}

	// ---- Run and the flag parser ----
	add(exitRow{site: "Run:exitUsage#1", status: 2, failOut: true, args: func(*testing.T) []string { return []string{"version"} }})
	add(row("dispatch:exitUsage#1", 2), row("dispatch:exitUsage#2", 2, "nonsense"), row("parseFlags:exitUsage#1", 2, "proof", "--nope"))
	add(flagCases("Proof", "proof")...)
	add(flagCases("RowHash", "row-hash")...)
	add(flagCases("DigestChain", "digest-chain")...)
	add(flagCases("Blocks", "blocks")...)
	add(flagCases("Checkpoint", "checkpoint")...)
	add(flagCases("Consistency", "consistency")...)
	add(flagCases("TrustInit", "trust", "init")...)
	add(flagCases("TrustShow", "trust", "show")...)

	// ---- proof ----
	add(
		row("runProof:inputError#1", 2, "proof", "--digest", d0),
		row("runProof:inputError#2", 2, "proof", "--proof", x.proof, "--digest", x.digest, "--table-id", "-5"),
		row("runProof:inputError#3", 2, "proof", "--proof", x.proof, "--digest", x.digest, "--row-hash", "zz"),
		row("runProof:inputError#4", 2, "proof", "--proof", x.proof, "--digest", x.digest, "--op", "X"),
		fn("runProof:inputError#5", 2, func(t *testing.T) []string { return []string{"proof", "--proof", x.proof, "--digest", missing()} }),
		fn("runProof:inputError#6", 2, func(t *testing.T) []string {
			return []string{"proof", "--proof", x.proof, "--digest", tmp("d", "garbage")}
		}),
		fn("runProof:inputError#7", 2, func(t *testing.T) []string { return []string{"proof", "--proof", missing(), "--digest", x.digest} }),
		fn("runProof:finishFailed#1", 1, func(t *testing.T) []string {
			return []string{"proof", "--proof", tmp("p", "garbage"), "--digest", x.digest}
		}),
		row("runProof:finishFailed#2", 1, "proof", "--proof", x.proof, "--digest", x.digest, "--op", "I"),
	)

	// ---- row-hash ----
	add(
		row("runRowHash:inputError#1", 2, "row-hash", "--columns", fx("columns/acc1.json")),
		row("runRowHash:inputError#2", 2, "row-hash", "--table-id", "1"),
		fn("runRowHash:inputError#3", 2, func(t *testing.T) []string { return []string{"row-hash", "--table-id", "1", "--columns", missing()} }),
		fn("runRowHash:inputError#4", 2, func(t *testing.T) []string {
			return []string{"row-hash", "--table-id", "1", "--columns", tmp("c", "garbage")}
		}),
		fn("runRowHash:inputError#5", 2, func(t *testing.T) []string {
			return []string{"row-hash", "--table-id", "1", "--columns", tmp("c", `[{"name":"`+strings.Repeat("a", 70000)+`","send_hex":""}]`)}
		}),
	)

	// ---- digest-chain ----
	add(
		row("runDigestChain:inputError#1", 2, "digest-chain"),
		fn("runDigestChain:inputError#2", 2, func(t *testing.T) []string { return []string{"digest-chain", missing()} }),
		fn("runDigestChain:inputError#3", 2, func(t *testing.T) []string { return []string{"digest-chain", tmp("d.json", "garbage")} }),
		exitRow{site: "runDigestChain:inputError#4", status: 2, unix: true, args: func(t *testing.T) []string {
			d := t.TempDir()
			dir := filepath.Join(d, "digests")
			if err := os.Mkdir(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			writeTemp(t, dir, "a.json", "{}")
			if err := os.Chmod(dir, 0o000); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })
			if _, err := os.ReadDir(dir); err == nil {
				t.Skip("this user can read a directory without permission")
			}
			return []string{"digest-chain", dir}
		}},
		fn("runDigestChain:inputError#5", 2, func(t *testing.T) []string {
			d := t.TempDir()
			writeTemp(t, d, "d.json", "garbage")
			return []string{"digest-chain", d}
		}),
		fn("runDigestChain:inputError#6", 2, func(t *testing.T) []string { return []string{"digest-chain", "--digests", t.TempDir()} }),
		row("runDigestChain:finishProblems#1", 1, "digest-chain", fx("digests/d8.json"), fx("digests/other_chain.json")),
	)

	// ---- blocks ----
	blocks := fx("blocks.jsonl")
	add(
		row("runBlocks:inputError#1", 2, "blocks", "--chain-id", zero),
		fn("runBlocks:inputError#2", 2, func(t *testing.T) []string { return []string{"blocks", "--blocks", blocks, "--digest", missing()} }),
		fn("runBlocks:inputError#3", 2, func(t *testing.T) []string {
			return []string{"blocks", "--blocks", blocks, "--digest", tmp("d", "garbage")}
		}),
		row("runBlocks:inputError#4", 2, "blocks", "--blocks", blocks, "--digest", d0, "--chain-id", "zz"),
		row("runBlocks:inputError#5", 2, "blocks", "--blocks", blocks, "--digest", d0, "--chain-id", zero),
		row("runBlocks:inputError#6", 2, "blocks", "--blocks", blocks, "--chain-id", "zz"),
		row("runBlocks:inputError#7", 2, "blocks", "--blocks", blocks),
		fn("runBlocks:inputError#8", 2, func(t *testing.T) []string { return []string{"blocks", "--blocks", missing(), "--chain-id", zero} }),
		never("runBlocks:inputError#9", 2, "the export grows past its limit of 1 GiB while it is read"),
		never("runBlocks:inputError#10", 2, "guard: every error of StreamBlocks is a StreamError"),
		fn("runBlocks:finishFailed#1", 1, func(t *testing.T) []string {
			return []string{"blocks", "--blocks", tmp("b", "garbage\n"), "--chain-id", zero}
		}),
	)

	// ---- checkpoint ----
	noTrust := func(t *testing.T) []string { return []string{"checkpoint", "--log", x.p.log(), "--source", x.p.dir} }
	add(
		fn("runCheckpoint:finishPub#1", 2, noTrust),
		fn("runCheckpoint:finishPub#1", 3, func(t *testing.T) []string {
			dir := t.TempDir()
			return []string{"checkpoint", "--trust", x.p.trust, "--log", "chain:" + testChain, "--source", dir}
		}),
		fn("runCheckpoint:finishPub#1", 1, func(t *testing.T) []string {
			dir := t.TempDir()
			if err := os.MkdirAll(filepath.Join(dir, "c"), 0o755); err != nil {
				t.Fatal(err)
			}
			writeTemp(t, filepath.Join(dir, "c"), testChain, "a file where the chain directory should be")
			return []string{"checkpoint", "--trust", x.p.trust, "--log", "chain:" + testChain, "--source", dir}
		}),
		fn("runCheckpoint:finishPub#2", 3, func(t *testing.T) []string {
			_ = os.Remove(filepath.Join(x.p.logDir, "checkpoint"))
			return x.cp()
		}),
		fn("runCheckpoint:finishPub#2", 1, func(t *testing.T) []string {
			_ = os.Remove(filepath.Join(x.p.logDir, "checkpoint"))
			_ = os.Mkdir(filepath.Join(x.p.logDir, "checkpoint"), 0o755)
			return x.cp()
		}),
		exitRow{site: "runCheckpoint:finishPub#2", status: 2, unix: true, args: func(t *testing.T) []string {
			f := filepath.Join(x.p.logDir, "checkpoint")
			if err := os.Chmod(f, 0o000); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.Chmod(f, 0o644) })
			if _, err := os.ReadFile(f); err == nil {
				t.Skip("this user can read a file without permission")
			}
			return x.cp()
		}},
		fn("runCheckpoint:inputError#1", 2, func(t *testing.T) []string { return x.cp("--trust", trustWith(t, x.p, `,"revoked":true`)) }),
		never("runCheckpoint:inputError#2", 2, "guard: a note that verifyNote accepted always parses again"),
		fn("runCheckpoint:finishFailed#1", 1, func(t *testing.T) []string {
			x.p.write(t, "checkpoint", "garbage\n")
			return x.cp()
		}),
		fn("runCheckpoint:finishFailed#2", 1, func(t *testing.T) []string { return x.cp("--min-size", "701") }),
	)

	// ---- checkpoint --state ----
	add(
		fn("applyState:inputError#1", 2, func(t *testing.T) []string { return x.cp("--state", filepath.Join(t.TempDir(), "no", "dir", "s")) }),
		fn("applyState:fail#1", 2, func(t *testing.T) []string { return x.cp("--state", filepath.Join(t.TempDir(), "no", "dir", "s")) }),
		exitRow{site: "applyState:fail#2", status: 2, unix: true, args: func(t *testing.T) []string {
			d := t.TempDir()
			if err := os.Chmod(d, 0o777); err != nil {
				t.Fatal(err)
			}
			return x.cp("--state", filepath.Join(d, "s"))
		}},
		fn("applyState:fail#3", 2, func(t *testing.T) []string {
			st := filepath.Join(t.TempDir(), "s")
			release := holdLock(t, st)
			t.Cleanup(release)
			return x.cp("--state", st)
		}),
		fn("applyState:fail#4", 2, func(t *testing.T) []string {
			d := t.TempDir()
			if err := os.Mkdir(filepath.Join(d, ".kingres-state.lock"), 0o700); err != nil {
				t.Fatal(err)
			}
			return x.cp("--state", filepath.Join(d, "s"))
		}),
		fn("applyState:fail#5", 2, func(t *testing.T) []string { return x.cp("--state", t.TempDir()) }),
		fn("applyState:fail#6", 2, func(t *testing.T) []string { return x.cp("--state", tmp("s", "")) }),
		fn("applyState:fail#7", 2, func(t *testing.T) []string { return x.cp("--state", tmp("s", "{garbage")) }),
		fn("applyState:fail#8", 2, func(t *testing.T) []string {
			return x.cp("--state", tmp("s", newSigner(7).note(x.p.origin, 300, refMTH(x.p.all[:300]))))
		}),
		never("applyState:fail#9", 2, "guard: verifyHeld returns only input errors"),
		fn("applyState:fail#10", 2, func(t *testing.T) []string {
			st := x.stateAt300()
			return []string{"checkpoint", "--trust", x.p.trust, "--log", x.p.log(), "--source", x.p.trust, "--note", filepath.Join(x.p.logDir, "checkpoint"), "--state", st}
		}),
		never("applyState:fail#11", 2, "guard: the state is written in the canonical form of the verified note, which reads back by construction (TestReadBackRefusesWhatALaterRunWouldNotRead calls readBack with a file that does not)"),
		exitRow{site: "applyState:fail#12", status: 2, unix: true, args: func(t *testing.T) []string {
			d := t.TempDir()
			st := filepath.Join(d, "s")
			if err := os.WriteFile(filepath.Join(d, ".kingres-state.lock"), nil, 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(d, 0o500); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.Chmod(d, 0o700) })
			if f, err := os.Create(filepath.Join(d, "probe")); err == nil {
				f.Close()
				t.Skip("this user can write a directory without write permission")
			}
			return x.cp("--state", st)
		}},
		fn("applyState:finishIncomplete#1", 3, func(t *testing.T) []string {
			st := x.stateAt300()
			_ = os.RemoveAll(filepath.Join(x.p.logDir, "tile"))
			return x.cp("--state", st)
		}),
		fn("applyState:finishFailed#1", 1, func(t *testing.T) []string {
			st := x.stateAt300()
			x.damageTiles()
			return x.cp("--state", st)
		}),
		never("applyState:finishFailed#2", 1, "guard: an error of the proof that is neither a missing tile, a damaged one nor a fork"),
	)
	// the three conflicts, with the evidence saved (1) and with its names taken (4)
	rollback := func(taken bool) func(t *testing.T) []string {
		return func(t *testing.T) []string {
			st := x.stateAt700()
			if taken {
				takeNames(t, st, 300, refMTH(x.p.all[:300]))
			}
			return x.cp("--note", x.oldN, "--state", st)
		}
	}
	sameSize := func(taken bool) func(t *testing.T) []string {
		return func(t *testing.T) []string {
			st := x.stateAt700()
			dir := t.TempDir()
			all := leavesSalt(700, 3, "other")
			publishLog(t, dir, x.p.signer, all, 700)
			if taken {
				takeNames(t, st, 700, refMTH(all))
			}
			return []string{"checkpoint", "--trust", x.p.trust, "--log", x.p.log(), "--source", dir, "--state", st}
		}
	}
	fork := func(taken bool) func(t *testing.T) []string {
		return func(t *testing.T) []string {
			st := x.stateAt300()
			dir, root := x.forkSource()
			if taken {
				takeNames(t, st, 400, root)
			}
			return []string{"checkpoint", "--trust", x.p.trust, "--log", x.p.log(), "--source", dir, "--state", st}
		}
	}
	add(
		fn("applyState:conflict#1", 1, rollback(false)), fn("applyState:conflict#1", 4, rollback(true)),
		fn("applyState:conflict#2", 1, sameSize(false)), fn("applyState:conflict#2", 4, sameSize(true)),
		fn("applyState:conflict#3", 1, fork(false)), fn("applyState:conflict#3", 4, fork(true)),
		fn("conflict:finishFailedWith#1", 1, fork(false)), fn("conflict:finishFailedWith#1", 4, fork(true)),
	)

	// ---- finishPub, the router of the publication problems ----
	add(
		fn("finishPub:finishIncomplete#1", 3, func(t *testing.T) []string {
			_ = os.Remove(filepath.Join(x.p.logDir, "checkpoint"))
			return x.cp()
		}),
		fn("finishPub:finishFailed#1", 1, func(t *testing.T) []string {
			_ = os.Remove(filepath.Join(x.p.logDir, "checkpoint"))
			_ = os.Mkdir(filepath.Join(x.p.logDir, "checkpoint"), 0o755)
			return x.cp()
		}),
		fn("finishPub:inputError#1", 2, noTrust),
	)

	// ---- consistency ----
	root300 := func() string { r := refMTH(x.p.all[:300]); return "300:" + b64(r[:]) }
	add(
		fn("runConsistency:inputError#1", 2, func(t *testing.T) []string { return x.co() }),
		fn("runConsistency:finishPub#1", 2, func(t *testing.T) []string {
			return []string{"consistency", "--log", x.p.log(), "--source", x.p.dir, "--old", x.oldN}
		}),
		fn("runConsistency:finishPub#1", 3, func(t *testing.T) []string {
			return []string{"consistency", "--trust", x.p.trust, "--log", "chain:" + testChain, "--source", t.TempDir(), "--old", x.oldN}
		}),
		fn("runConsistency:finishPub#1", 1, func(t *testing.T) []string {
			dir := t.TempDir()
			_ = os.MkdirAll(filepath.Join(dir, "c"), 0o755)
			writeTemp(t, filepath.Join(dir, "c"), testChain, "a file")
			return []string{"consistency", "--trust", x.p.trust, "--log", "chain:" + testChain, "--source", dir, "--old", x.oldN}
		}),
		fn("runConsistency:inputError#2", 2, func(t *testing.T) []string { return x.co("--old", "300:AAAA") }),
		fn("runConsistency:inputError#3", 2, func(t *testing.T) []string { return x.co("--old", "0:"+b64(make([]byte, 32))) }),
		fn("runConsistency:inputError#4", 2, func(t *testing.T) []string { return x.co("--old", missing()) }),
		fn("runConsistency:inputError#5", 2, func(t *testing.T) []string { return x.co("--old", tmp("o", "{garbage")) }),
		fn("runConsistency:inputError#6", 2, func(t *testing.T) []string { return x.co("--old", tmp("o", "garbage")) }),
		fn("runConsistency:inputError#7", 2, func(t *testing.T) []string {
			return x.co("--old", tmp("o", newSigner(7).note(x.p.origin, 300, refMTH(x.p.all[:300]))))
		}),
		never("runConsistency:inputError#8", 2, "guard: verifyHeld returns only input errors"),
		fn("runConsistency:finishPub#2", 3, func(t *testing.T) []string {
			_ = os.Remove(filepath.Join(x.p.logDir, "checkpoint"))
			return x.co("--old", x.oldN)
		}),
		fn("runConsistency:finishPub#2", 1, func(t *testing.T) []string {
			_ = os.Remove(filepath.Join(x.p.logDir, "checkpoint"))
			_ = os.Mkdir(filepath.Join(x.p.logDir, "checkpoint"), 0o755)
			return x.co("--old", x.oldN)
		}),
		fn("runConsistency:finishPub#2", 2, func(t *testing.T) []string { return x.co("--old", x.oldN, "--new", missing()) }),
		fn("runConsistency:inputError#9", 2, func(t *testing.T) []string {
			return x.co("--old", root300(), "--trust", trustWith(t, x.p, `,"revoked":true`))
		}),
		fn("runConsistency:finishFailed#1", 1, func(t *testing.T) []string {
			return x.co("--old", root300(), "--new", tmp("n", newSigner(7).note(x.p.origin, 700, refMTH(x.p.all))))
		}),
		fn("runConsistency:finishFailed#2", 1, func(t *testing.T) []string { return x.co("--old", "701:"+nonEmptyRoot) }),
		fn("runConsistency:finishFailed#3", 1, func(t *testing.T) []string { return x.co("--old", "700:"+nonEmptyRoot) }),
		fn("runConsistency:inputError#10", 2, func(t *testing.T) []string {
			return []string{"consistency", "--trust", x.p.trust, "--log", x.p.log(), "--source", x.p.trust, "--old", root300(),
				"--new", tmp("n", x.p.signer.note(x.p.origin, 700, refMTH(x.p.all)))}
		}),
		never("runConsistency:inputError#11", 2, "guard: the tree size of a verified checkpoint is always valid"),
		never("runConsistency:inputError#12", 2, "guard: an error of the proof that is neither a missing tile nor a damaged one"),
		fn("runConsistency:finishIncomplete#1", 3, func(t *testing.T) []string {
			_ = os.RemoveAll(filepath.Join(x.p.logDir, "tile"))
			return x.co("--old", x.oldN)
		}),
		fn("runConsistency:finishFailed#4", 1, func(t *testing.T) []string {
			x.damageTiles()
			return x.co("--old", x.oldN)
		}),
		fn("runConsistency:finishFailed#5", 1, func(t *testing.T) []string { return x.co("--old", "300:"+nonEmptyRoot) }),
	)

	// ---- trust ----
	add(
		row("runTrust:exitUsage#1", 2, "trust"),
		row("runTrust:exitUsage#2", 2, "trust", "follow"),
		row("runTrust:exitUsage#3", 2, "trust", "nonsense"),
	)
	pubB64 := b64(x.p.signer.pub)
	init := func(more ...string) []string {
		return append([]string{"trust", "init", "--origin-prefix", testPrefix, "--fingerprint", x.p.signer.fp()}, more...)
	}
	add(
		row("runTrustInit:inputError#1", 2, "trust", "init", "--public-key", pubB64, "--fingerprint", x.p.signer.fp()),
		row("runTrustInit:inputError#2", 2, "trust", "init", "--origin-prefix", testPrefix, "--public-key", pubB64),
		row("runTrustInit:inputError#3", 2, init()...),
		fn("runTrustInit:inputError#4", 2, func(t *testing.T) []string { return init("--public-key-file", missing(), "--out", missing()) }),
		fn("runTrustInit:inputError#5", 2, func(t *testing.T) []string { return init("--public-key", "!!", "--out", missing()) }),
		fn("runTrustInit:inputError#6", 2, func(t *testing.T) []string {
			return []string{"trust", "init", "--origin-prefix", testPrefix, "--public-key", pubB64, "--fingerprint", newSigner(8).fp(), "--out", missing()}
		}),
		never("runTrustInit:inputError#7", 2, "guard: Marshal of a file that trust.New built does not fail"),
		fn("runTrustInit:inputError#8", 2, func(t *testing.T) []string {
			return init("--public-key", pubB64, "--out", tmp("exists.json", "x"))
		}),
		fn("runTrustInit:inputError#9", 2, func(t *testing.T) []string {
			failTrustWrite(t)
			return init("--public-key", pubB64, "--out", filepath.Join(t.TempDir(), "never.json"))
		}),
		never("runTrustInit:inputError#10", 2, "the disk fails while the file is closed"),
		row("runTrustShow:inputError#1", 2, "trust", "show"),
		fn("runTrustShow:inputError#2", 2, func(t *testing.T) []string { return []string{"trust", "show", "--trust", missing()} }),
		fn("runTrustShow:inputError#3", 2, func(t *testing.T) []string { return []string{"trust", "show", "--trust", tmp("t", "garbage")} }),
		fn("runTrustShow:inputError#4", 2, func(t *testing.T) []string {
			return []string{"trust", "show", "--trust", x.p.trust, "--now", "yesterday"}
		}),
		fn("runTrustShow:finishInputWith#1", 2, func(t *testing.T) []string {
			return []string{"trust", "show", "--trust", trustWith(t, x.p, `,"revoked":true`)}
		}),
	)
	return rows
}

func TestEveryExitSiteHasARowAndAScenario(t *testing.T) {
	sites := deriveSites(t)
	byKey := map[string]site{}
	for _, s := range sites {
		byKey[s.key] = s
	}
	rows := exitRows(newExitEnv(t))

	// 1. every site has a row for each status it can end a run with, and every
	// row names a site of the source
	have := map[string]map[int]bool{}
	for _, r := range rows {
		s, ok := byKey[r.site]
		if !ok {
			t.Errorf("row for %s, which is not a site of the source (the code changed: update exitRows)", r.site)
			continue
		}
		allowed := false
		for _, a := range s.allowed {
			if a == r.status {
				allowed = true
			}
		}
		if !allowed {
			t.Errorf("%s: a row says status %d, the call can give only %v", r.site, r.status, s.allowed)
		}
		if have[r.site] == nil {
			have[r.site] = map[int]bool{}
		}
		have[r.site][r.status] = true
	}
	for _, s := range sites {
		for _, a := range s.allowed {
			if !have[s.key][a] {
				t.Errorf("%s (%s:%d) can end a run with status %d and has no row: add one to exitRows, with a scenario or the reason none exists", s.key, s.file, s.start, a)
			}
		}
	}

	// 2. every scenario reaches its site with its status
	rec := newSiteRecorder(sites)
	trace = rec.record
	defer func() { trace = nil }()
	for i, r := range rows {
		i, r := i, r
		if r.args == nil {
			if r.why == "" {
				t.Errorf("%s status %d: neither a scenario nor a reason", r.site, r.status)
			}
			continue
		}
		if r.unix && runtime.GOOS == "windows" {
			continue
		}
		t.Run(fmt.Sprintf("%03d %s -> %d", i, r.site, r.status), func(t *testing.T) {
			// every scenario gets a publication of its own: some damage it
			row := exitRows(newExitEnv(t))[i]
			rec.mu.Lock()
			rec.hits = map[string]map[int]bool{}
			rec.mu.Unlock()
			args := row.args(t)
			var code int
			var out, errs string
			if row.failOut {
				code, out, errs = runFailingStdout(t, args...)
			} else {
				code, out, errs = run(t, args...)
			}
			if code != r.status {
				t.Fatalf("status %d, want %d\n%.400s%.400s", code, r.status, out, errs)
			}
			rec.mu.Lock()
			hit := rec.hits[r.site][r.status]
			rec.mu.Unlock()
			if !hit {
				t.Errorf("the run ended with status %d but not through %s", code, r.site)
			}
			if code != 0 && (verdictLine(out, "VERIFIED:") || verdictLine(out, "CONSISTENT:")) {
				t.Errorf("a success verdict with status %d", code)
			}
		})
	}
}

// verdictLine reports whether a line of out starts with the verdict word (a
// line "INCONSISTENT:" does not).
func verdictLine(out, word string) bool {
	for _, l := range strings.Split(out, "\n") {
		if strings.HasPrefix(l, word) {
			return true
		}
	}
	return false
}

// commandOfSite names the command a site belongs to, "" for a function that is
// shared by every command (the flag parser and Run are counted for each).
func commandOfSite(site string) string {
	fn, _, _ := strings.Cut(site, ":")
	switch fn {
	case "runProof":
		return "proof"
	case "runRowHash":
		return "row-hash"
	case "runDigestChain":
		return "digest-chain"
	case "runBlocks":
		return "blocks"
	case "runCheckpoint", "applyState", "conflict":
		return "checkpoint"
	case "runConsistency":
		return "consistency"
	case "runTrustInit":
		return "trust init"
	case "runTrustShow":
		return "trust show"
	case "runTrust":
		return "trust"
	}
	return ""
}

// statusTable is the table of the README, made from the rows: for each command
// the statuses a run can end with (0 for a success).
func statusTable(rows []exitRow) []string {
	order := []string{"proof", "row-hash", "digest-chain", "blocks", "checkpoint", "consistency", "trust init", "trust show", "trust"}
	got := map[string]map[int]bool{}
	for _, c := range order {
		got[c] = map[int]bool{}
	}
	for _, r := range rows {
		if c := commandOfSite(r.site); c != "" {
			got[c][r.status] = true
		}
	}
	got["checkpoint"][0], got["consistency"][0] = true, true
	for _, c := range order {
		if c != "trust" {
			got[c][0] = true
		}
	}
	var lines []string
	for _, c := range order {
		var s []string
		for st := 0; st <= 4; st++ {
			if got[c][st] {
				s = append(s, fmt.Sprint(st))
			}
		}
		lines = append(lines, fmt.Sprintf("| `%s` | %s |", c, strings.Join(s, ", ")))
	}
	return lines
}

// L9-09: the table of the README is the table of the code. The statuses of each
// command are those of its exit sites, which TestEveryExitSiteHasARowAndAScenario
// ties to the source; this ties the README to them.
func TestReadmeTableOfStatusesIsTheTableOfTheCode(t *testing.T) {
	readme, err := os.ReadFile("../../README.md")
	if err != nil {
		t.Fatal(err)
	}
	text := string(readme)
	for _, l := range statusTable(exitRows(newExitEnv(t))) {
		if !strings.Contains(text, l+"\n") {
			t.Errorf("the README has no line %q: update the table \"Which command ends with which status\"", l)
		}
	}
	for st := 0; st <= 4; st++ {
		if !strings.Contains(text, fmt.Sprintf("\n| %d | ", st)) {
			t.Errorf("the table of exit statuses has no row for %d", st)
		}
	}
	// 3 is not a pass, said where an auditor reads the table
	if !strings.Contains(text, "Treat 3 as an alert, never as \"probably fine\"") {
		t.Error("the README does not say that 3 is not a pass")
	}
	// the sentence about the blocks command agrees with the code: its parse
	// failure is a failed verification, and it never ends with 3
	if strings.Contains(text, "block export when it cannot be parsed") {
		t.Error("the README still lists the block export under status 2")
	}
	for _, l := range statusTable(exitRows(newExitEnv(t))) {
		if strings.HasPrefix(l, "| `blocks` |") && strings.Contains(l, "3") {
			t.Error("blocks can end with 3 in the table")
		}
	}
}

// failTrustWrite makes the write of a trust file fail for the rest of the test.
func failTrustWrite(t *testing.T) {
	t.Helper()
	old := trustWrite
	trustWrite = func(*os.File, []byte) (int, error) { return 0, errors.New("no space left on device") }
	t.Cleanup(func() { trustWrite = old })
}
