// SPDX-License-Identifier: AGPL-3.0-only

package cli

import (
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/KingresDB/Kingres/kingres/verifier/internal/format"
	"github.com/KingresDB/Kingres/kingres/verifier/internal/merkle"
	"github.com/KingresDB/Kingres/kingres/verifier/internal/testdir"
	"github.com/KingresDB/Kingres/kingres/verifier/internal/trust"
)

// Tests of the fixes of the eighth review (V8-01 to V8-12). Each test names its finding.

func b64pub(s signer) string { return base64.StdEncoding.EncodeToString(s.pub) }

// pinBoth writes a trust file that pins the keys of the signers (the first is the key of the world).
func pinBoth(t *testing.T, w *world, more ...signer) string {
	t.Helper()
	tf := loadTrust(t, w.trust)
	for i, s := range more {
		tf.Keys = append(tf.Keys, trust.Key{Name: fmt.Sprintf("extra-%d", i), Public: s.pub, Fingerprint: s.fp()})
	}
	data, err := tf.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(testdir.New(t), "trust.json")
	if err := os.WriteFile(p, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func (w *world) revokeS1BySecond() {
	w.event(0, "key-revoked", []signer{w.s2}, "key: "+b64pub(w.s1), "compromised-since: 2026-11-18T00:00:00Z", "reason: stolen laptop")
}

const nowArg19 = "2026-11-19T12:00:00Z"

// commandsOf lists, for a world with two chains and an aggregate, one run of every command that judges a signature.
func commandsOf(t *testing.T, w *world, a *chainFix, tr string) map[string][]string {
	t.Helper()
	w.put("p/row.json", a.proofJSON(3, nil, 0))
	w.bundleJSON()
	root := merkle.EmptyRoot()
	return map[string][]string{
		"checkpoint":   {"checkpoint", "--trust", tr, "--log", "chain:" + a.idHex + ":e1", "--source", w.dir, "--now", nowArg19},
		"consistency":  {"consistency", "--trust", tr, "--log", "chain:" + a.idHex + ":e1", "--source", w.dir, "--old", "0:" + base64.StdEncoding.EncodeToString(root[:]), "--now", nowArg19},
		"aggregate":    {"aggregate", "--trust", tr, "--source", w.dir, "--aggregate-id", w.aggID, "--now", nowArg19},
		"proof":        {"proof", "--proof", filepath.Join(w.dir, "p", "row.json"), "--bundle", w.dir, "--trust", tr, "--log-leg", "chain", "--now", nowArg19},
		"bundle":       {"bundle", "verify", "--trust", tr, "--now", nowArg19, w.dir},
		"monitor":      {"monitor", "--trust", tr, "--source", w.dir, "--state", filepath.Join(testdir.New(t), "st"), "--init", "--once", "--log", "chain:" + a.idHex + ":e1", "--now", nowArg19},
		"aggregate-up": {"aggregate", "--trust", tr, "--source", w.dir, "--aggregate-id", w.aggID, "--now", nowArg19},
	}
}

// V8-01: a verified key-revoked event for the key that signed the checkpoints counts in every command that judges a signature.
func TestV801AKeyRevokedByAVerifiedEventVouchesForNothingInAnyCommand(t *testing.T) {
	w, a, _ := stdWorld(t)
	tr := pinBoth(t, w, w.s2)
	cmds := commandsOf(t, w, a, tr)
	// without the event, every command verifies the checkpoints that the key signed (the control)
	for name, args := range cmds {
		code, out, errs := run(t, args...)
		if code != 0 {
			t.Fatalf("control %s: status %d\n%s%s", name, code, out, errs)
		}
	}
	w.revokeS1BySecond()
	for name, args := range cmds {
		if name == "monitor" {
			args[6] = filepath.Join(testdir.New(t), "st2")
		}
		code, out, errs := run(t, args...)
		if code != 1 {
			t.Fatalf("%s: status %d, want 1 (a checkpoint that only a revoked key signed is not verified)\n%s%s", name, code, out, errs)
		}
		if strings.Contains(out, "VERIFIED:") && !strings.Contains(out, "NOT VERIFIED") && !strings.Contains(out, "FAILED") {
			t.Fatalf("%s printed VERIFIED:\n%s", name, out)
		}
		if !strings.Contains(out+errs, "revoked by the verified event 000000") {
			t.Fatalf("%s: the output does not say which event revoked the key:\n%s%s", name, out, errs)
		}
	}
}

// V8-01: the same rule says which keys vouched: a checkpoint signed by the revoked key and by a good one is verified by the good one only.
func TestV801TheOutputNamesTheKeysThatVouched(t *testing.T) {
	w, a, b := stdWorld(t)
	tr := pinBoth(t, w, w.s2)
	w.publishChain(a, 5, w.s1, w.s2)
	w.revokeS1BySecond()
	out := wantRun(t, 0, []string{"checkpoint", "--trust", tr, "--log", "chain:" + a.idHex + ":e1", "--source", w.dir, "--now", nowArg19}, "VERIFIED:", "revoked by the verified event 000000", "compromised since 2026-11-18T00:00:00Z")
	if !strings.Contains(out, "verified by key "+w.s2.fp()[:16]) || strings.Contains(out, "verified by key "+w.s1.fp()[:16]) {
		t.Fatalf("the signers named are not the one key that counts:\n%s", out)
	}
	// the chain that only the revoked key signed is refused
	wantRun(t, 1, []string{"checkpoint", "--trust", tr, "--log", "chain:" + b.idHex + ":e1", "--source", w.dir, "--now", nowArg19}, "revoked by the verified event")
}

// V8-01: a verified key-retired event limits the key to the size it names, in every command.
func TestV801ARetiredKeyVouchesOnlyUpToTheBoundOfTheEvent(t *testing.T) {
	w, a, b := stdWorld(t) // chain a has 5 blocks, chain b 3
	tr := pinBoth(t, w, w.s2)
	w.event(0, "key-retired", []signer{w.s2}, "key: "+b64pub(w.s1), "max-tree-size: 3")
	wantRun(t, 0, []string{"checkpoint", "--trust", tr, "--log", "chain:" + b.idHex + ":e1", "--source", w.dir, "--now", nowArg19}, "VERIFIED:", "tree size 3 or less")
	wantRun(t, 1, []string{"checkpoint", "--trust", tr, "--log", "chain:" + a.idHex + ":e1", "--source", w.dir, "--now", nowArg19}, "key-size", "the verified event 000000")
	// the aggregate reads the chain log of 5 blocks: refused as well
	wantRun(t, 1, []string{"aggregate", "--trust", tr, "--source", w.dir, "--aggregate-id", w.aggID, "--now", nowArg19}, "FAILED at step key-size")
}

// V8-01: a key that an event adds is not used by these commands, and the output says so.
func TestV801AKeyAddedByAnEventIsNotUsedHere(t *testing.T) {
	w, a, _ := stdWorld(t)
	w.event(0, "key-added", []signer{w.s1, w.s2}, keyAddedLines(w.s2)...)
	w.publishChain(a, 5, w.s2)
	out := wantRun(t, 1, []string{"checkpoint", "--trust", w.trust, "--log", "chain:" + a.idHex + ":e1", "--source", w.dir, "--now", nowArg19}, "added key "+w.s2.fp()[:16], "trust follow")
	_ = out
}

// V8-02: an event that no authorised key signed is not counted as verified, in bundle verify or anywhere else.
func TestV802AnEventNoAuthorisedKeySignedIsNeverCountedAsVerified(t *testing.T) {
	w, _, _ := stdWorld(t)
	w.event(0, "key-added", []signer{w.s1}, keyAddedLines(w.s1)...)
	w.event(1, "key-added", []signer{w.s1, w.s2}, keyAddedLines(w.s2)...)
	w.bundleJSON()
	wantRun(t, 0, w.bundleArgs(), "VERIFIED:", "2 event(s) read: 2 verified under the trust file, 0 NOT verified")
	// one character of the key name of the signature line of event 0
	raw, _ := os.ReadFile(filepath.Join(w.dir, "events", "000000"))
	bad := strings.Replace(string(raw), "\u2014 "+w.prefix, "\u2014 d"+w.prefix[1:], 1)
	if bad == string(raw) {
		t.Fatal("the event was not changed")
	}
	w.put("events/000000", []byte(bad))
	out := wantRun(t, 3, w.bundleArgs(), "INCOMPLETE", "1 NOT verified", "event 000000", "are not verified")
	if strings.Contains(out, "2 verified") || strings.Contains(out, "read and verified") {
		t.Fatalf("the output calls an event verified that no key signed:\n%s", out)
	}
	// the other commands that read the events list it in their notes
	wantRun(t, 0, w.aggArgs(), "event 000000 (key-added) was read and is NOT verified")
}

// V8-04, V8-05, V8-09, V8-10: the flags of an aggregate member are judged together. The table is the one of the README.
func TestV804TheFlagsOfAMemberAreJudgedTogether(t *testing.T) {
	const (
		stale, retired, diverged = 1, 2, 4
	)
	type row struct {
		flags byte
		event string
		want  int
		step  string
	}
	rows := []row{
		{0, "none", 0, ""},
		{stale, "none", 3, ""},
		{retired, "none", 3, ""},
		{retired, "good", 0, ""},
		{retired, "unauthorised", 1, "chain-retired"},
		{retired, "other-head", 1, "chain-retired"},
		{retired, "continues", 1, "chain-retired"},
		{stale | retired, "none", 3, ""},
		{stale | retired, "good", 3, ""},
		{stale | retired, "unauthorised", 1, "chain-retired"},
		{stale | retired, "other-head", 1, "chain-retired"},
		{stale | retired, "continues", 1, "chain-retired"},
		{diverged, "none", 1, "diverged"},
		{diverged, "good", 1, "diverged"},
		{stale | diverged, "none", 1, "diverged"},
		{retired | diverged, "none", 1, "diverged"},
		{retired | diverged, "good", 1, "diverged"},
		{retired | diverged, "unauthorised", 1, "diverged"},
		{retired | diverged, "continues", 1, "diverged"},
		{stale | retired | diverged, "none", 1, "diverged"},
		{stale | retired | diverged, "good", 1, "diverged"},
	}
	for _, rw := range rows {
		name := fmt.Sprintf("flags=%d/%s", rw.flags, rw.event)
		t.Run(name, func(t *testing.T) {
			w := newWorld(t)
			a := makeChain(0xa1, 5, nil)
			published := 3
			if rw.event == "continues" {
				published = 5
			}
			w.addChain(a, published)
			w.appendRound(1_000_000, member(a, 3, rw.flags))
			switch rw.event {
			case "good", "continues":
				w.event(0, "chain-retired", []signer{w.s1}, chainRetiredLines(a, 2, "dropped")...)
			case "other-head":
				w.event(0, "chain-retired", []signer{w.s1}, chainRetiredLines(a, 1, "dropped")...)
			case "unauthorised":
				w.event(0, "chain-retired", []signer{newSigner(9)}, chainRetiredLines(a, 2, "dropped")...)
			}
			out := ""
			code, o, e := run(t, w.aggArgs()...)
			out = o + e
			if code != rw.want {
				t.Fatalf("aggregate: status %d, want %d\n%s", code, rw.want, out)
			}
			if rw.want == 1 && !strings.Contains(out, "FAILED at step "+rw.step) {
				t.Fatalf("the step %s is not named:\n%s", rw.step, out)
			}
			if rw.want != 0 && strings.Contains(o, "VERIFIED:") && !strings.Contains(o, "NOT VERIFIED") {
				t.Fatalf("VERIFIED with status %d:\n%s", code, out)
			}
			// a bundle that holds the same aggregate ends the same way
			w.bundleJSON()
			code, o, e = run(t, w.bundleArgs()...)
			if code != rw.want {
				t.Fatalf("bundle verify: status %d, want %d\n%s%s", code, rw.want, o, e)
			}
		})
	}
}

// V8-10: the aggregate leg of a proof does not drop the stale flag of the member.
func TestV810TheAggregateLegOfAProofReportsAStaleMember(t *testing.T) {
	w := newWorld(t)
	a := makeChain(0xa1, 3, nil)
	w.addChain(a, 3)
	w.appendRound(1_000_000, member(a, 3, 1))
	w.put("p/row.json", a.proofJSON(1, nil, 0))
	args := []string{"proof", "--proof", filepath.Join(w.dir, "p", "row.json"), "--bundle", w.dir, "--trust", w.trust, "--now", nowArg19}
	wantRun(t, 0, append(args, "--log-leg", "chain"), "VERIFIED:")
	wantRun(t, 3, append(args, "--log-leg", "aggregate"), "INCOMPLETE", "flag stale")
}

// V8-11: events after a gap are reported.
func TestV811EventsAfterAGapAreReported(t *testing.T) {
	w, _, _ := stdWorld(t)
	w.event(0, "key-added", []signer{w.s1}, keyAddedLines(w.s1)...)
	w.event(1, "key-added", []signer{w.s1, w.s2}, keyAddedLines(w.s2)...)
	w.put("events/000003", []byte("the event 000002 is missing\n"))
	wantRun(t, 1, w.followArgs(newOut(t)), "without a gap", "event 000002 is absent and event 000003 is present")
	wantRun(t, 1, w.aggArgs(), "FAILED at step events", "without a gap")
	// the same in a .tar.gz source
	tgz := filepath.Join(testdir.New(t), "pub.tar.gz")
	writeTarGz(t, w.dir, tgz)
	wantRun(t, 1, []string{"aggregate", "--trust", w.trust, "--source", tgz, "--aggregate-id", w.aggID, "--now", nowArg19}, "without a gap")
}

// V8-12: a consistency check from size 0 says that nothing was compared.
func TestV812ConsistencyFromSizeZeroSaysNothingWasCompared(t *testing.T) {
	w, a, _ := stdWorld(t)
	root := merkle.EmptyRoot()
	out := wantRun(t, 0, []string{"consistency", "--trust", w.trust, "--log", "chain:" + a.idHex + ":e1", "--source", w.dir, "--old", "0:" + base64.StdEncoding.EncodeToString(root[:]), "--now", nowArg19},
		"NOTHING WAS COMPARED", "an empty tree is a prefix of every tree")
	if !strings.Contains(out, "any second root would be accepted") {
		t.Fatal(out)
	}
	wantRun(t, 0, []string{"consistency", "--trust", w.trust, "--log", "chain:" + a.idHex + ":e1", "--source", w.dir, "--old", "0:" + base64.StdEncoding.EncodeToString(root[:]), "--now", nowArg19, "--json"}, `"compared": false`)
}

var _ = format.OpInsert
