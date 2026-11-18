// SPDX-License-Identifier: AGPL-3.0-only

package cli

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/KingresDB/Kingres/kingres/verifier/internal/checkpoint"
	"github.com/KingresDB/Kingres/kingres/verifier/internal/signednote"

	"github.com/KingresDB/Kingres/kingres/verifier/internal/testdir"
)

// The invariant of the state (L10-04, L10-05, L10-09): what is stored in the
// state, and what is saved as evidence of a fork, is exactly what was verified,
// in a canonical form that depends only on verified content: the text of the
// note and the signature lines of the keys that verified, written again in the
// order of name and key id. A later change of the trust file (a key added,
// retired, removed, a window changed) never makes a stored state unreadable and
// never makes it accept less.

// sigLine is the signature line of key s over text for the log origin.
func sigLine(origin, text string, s signer) string {
	sum := sha256.Sum256(append(append([]byte(origin), 0x0A, 0x01), s.pub...))
	raw := append(append([]byte{}, sum[:4]...), ed25519.Sign(s.priv, []byte(text))...)
	return "\xe2\x80\x94 " + origin + " " + base64.StdEncoding.EncodeToString(raw) + "\n"
}

// junkLine is a line that names key s (its name and key id) with a signature
// of zero bytes: what anybody can write without holding the key.
func junkLine(origin string, s signer) string {
	sum := sha256.Sum256(append(append([]byte(origin), 0x0A, 0x01), s.pub...))
	raw := append(append([]byte{}, sum[:4]...), make([]byte, 64)...)
	return "\xe2\x80\x94 " + origin + " " + base64.StdEncoding.EncodeToString(raw) + "\n"
}

// signedBy is the note of the first size entries of all, signed by every signer
// in signers and followed by the junk lines as they are.
func signedBy(size int, all []h32, signers []signer, junk ...string) string {
	text := fmt.Sprintf("%s\n%d\n%s\n", logOrigin(), size, base64.StdEncoding.EncodeToString(func() []byte { r := refMTH(all[:size]); return r[:] }()))
	out := text + "\n"
	for _, s := range signers {
		out += sigLine(logOrigin(), text, s)
	}
	return out + strings.Join(junk, "")
}

// publishNote writes the tiles of the first size entries and the given note as
// the checkpoint of the publication at root.
func publishNote(t *testing.T, root string, all []h32, size int, note string) {
	t.Helper()
	logDir := filepath.Join(root, "c", testChain, "e1")
	writeTiles(t, logDir, all, size)
	if err := os.WriteFile(filepath.Join(logDir, "checkpoint"), []byte(note), 0o644); err != nil {
		t.Fatal(err)
	}
}

// storedNote reads the note out of a state file.
func storedNote(t *testing.T, state string) string {
	t.Helper()
	b, err := os.ReadFile(state)
	if err != nil {
		t.Fatal(err)
	}
	var rec struct {
		Note string `json:"note"`
	}
	if err := json.Unmarshal(b, &rec); err != nil {
		t.Fatalf("not a state: %v", err)
	}
	return rec.Note
}

func sigLines(note string) []string {
	_, sigs, _ := strings.Cut(note, "\n\n")
	return strings.Split(strings.TrimSuffix(sigs, "\n"), "\n")
}

// L10-04: a state is not refused for ever after a key is added. The genuine
// note of size 300 carries a line of key 2 that nobody signed; the state keeps
// only the line that verified, so that adding key 2 later changes nothing.
func TestStateKeepsOnlyTheLinesThatVerified(t *testing.T) {
	k1, k2 := newSigner(1), newSigner(2)
	all := leaves(700)
	root := testdir.New(t)
	trust1 := trustOf(t, keySpec{"k1", k1, ""})
	state := filepath.Join(testdir.New(t), "state")
	publishNote(t, root, all, 300, signedBy(300, all, []signer{k1}, junkLine(logOrigin(), k2)))
	if code, out, errs := run(t, stateArgs(trust1, root, state)...); code != 0 || !strings.Contains(out, "not counted") {
		t.Fatalf("first run: %d\n%s%s", code, out, errs)
	}
	if got := sigLines(storedNote(t, state)); len(got) != 1 {
		t.Fatalf("the state keeps %d signature lines, want the 1 that verified:\n%s", len(got), storedNote(t, state))
	}
	// A normal rotation: key 2 is added. The honest note of size 700 is signed by both.
	trust2 := trustOf(t, keySpec{"k1", k1, ""}, keySpec{"k2", k2, ""})
	publishNote(t, root, all, 700, signedBy(700, all, []signer{k1, k2}))
	if code, out, errs := run(t, stateArgs(trust2, root, state)...); code != 0 || stateSize(t, state) != 700 {
		t.Fatalf("after adding the key: %d\n%s%s", code, out, errs)
	}
	if code, out, errs := run(t, "consistency", "--trust", trust2, "--log", logSpecArg(), "--source", root, "--old", state); code != 0 {
		t.Fatalf("consistency from the state: %d\n%s%s", code, out, errs)
	}
}

// L10-05: a line with the id of a retired key does not stop the state from
// advancing, and the output does not call it a signature.
func TestRetiredKeyLineDoesNotStopTheState(t *testing.T) {
	k1, k3 := newSigner(1), newSigner(3)
	all := leaves(700)
	root := testdir.New(t)
	tr := trustOf(t, keySpec{"k1", k1, ""}, keySpec{"k3", k3, `,"not_after":"2020-01-01T00:00:00Z"`})
	state := filepath.Join(testdir.New(t), "state")
	publishNote(t, root, all, 300, signedBy(300, all, []signer{k1}))
	if code, out, errs := run(t, stateArgs(tr, root, state)...); code != 0 {
		t.Fatalf("seed: %d\n%s%s", code, out, errs)
	}
	publishNote(t, root, all, 700, signedBy(700, all, []signer{k1}, junkLine(logOrigin(), k3)))
	code, out, errs := run(t, stateArgs(tr, root, state)...)
	if code != 0 || stateSize(t, state) != 700 {
		t.Fatalf("a line of a retired key stopped the state: %d\n%s%s", code, out, errs)
	}
	if strings.Contains(out, "carries a signature of key") || !strings.Contains(out, "does not verify") {
		t.Errorf("the output calls a line that never verified a signature:\n%s", out)
	}
	if got := sigLines(storedNote(t, state)); len(got) != 1 {
		t.Errorf("the state keeps %d lines, want 1", len(got))
	}
	// Without --state the verdict is the same and the line is named for what it is.
	if code, out, _ := run(t, "checkpoint", "--trust", tr, "--log", logSpecArg(), "--source", root); code != 0 || strings.Contains(out, "carries a signature of key") {
		t.Errorf("without --state: %d\n%s", code, out)
	}
}

// A state written by the version before this round holds the note as it was
// served, junk lines included. It is still read, and when the state advances the
// new state is canonical.
func TestStateOfTheEarlierVersionIsStillRead(t *testing.T) {
	k1, k2 := newSigner(1), newSigner(2)
	all := leaves(700)
	root := testdir.New(t)
	// The earlier version stored the note as sent: one line of key 2 that nobody
	// signed, with key 2 absent from the trust file at the time.
	old := signedBy(300, all, []signer{k1}, junkLine(logOrigin(), k2))
	parsed, err := signednote.Parse([]byte(old))
	if err != nil {
		t.Fatal(err)
	}
	cp := mustCheckpoint(t, parsed.Text)
	data, err := encodeState(cp, []byte(old), nil, nowForTests)
	if err != nil {
		t.Fatal(err)
	}
	state := filepath.Join(testdir.New(t), "state")
	if err := os.WriteFile(state, data, 0o600); err != nil {
		t.Fatal(err)
	}
	// Key 2 is added: the earlier version refused this state for ever.
	tr := trustOf(t, keySpec{"k1", k1, ""}, keySpec{"k2", k2, ""})
	publishNote(t, root, all, 700, signedBy(700, all, []signer{k1, k2}))
	if code, out, errs := run(t, "consistency", "--trust", tr, "--log", logSpecArg(), "--source", root, "--old", state); code != 0 || !strings.Contains(out, "do not verify") {
		t.Fatalf("consistency from the old state: %d\n%s%s", code, out, errs)
	}
	code, out, errs := run(t, stateArgs(tr, root, state)...)
	if code != 0 || stateSize(t, state) != 700 {
		t.Fatalf("checkpoint --state from the old state: %d\n%s%s", code, out, errs)
	}
	if got := sigLines(storedNote(t, state)); len(got) != 2 {
		t.Errorf("the new state keeps %d lines, want the 2 that verified", len(got))
	}
}

// L10-09: the conflicting note is saved in its canonical form, so a publisher
// that serves the same signed text with a different number of unknown lines
// cannot use up the names for the evidence and turn status 1 into status 4.
func TestEvidenceIsOneFileWhateverTheUnverifiedLines(t *testing.T) {
	k1 := newSigner(1)
	all := leaves(700)
	tr := trustOf(t, keySpec{"k1", k1, ""})
	state := filepath.Join(testdir.New(t), "state")
	root := testdir.New(t)
	publishNote(t, root, all, 700, signedBy(700, all, []signer{k1}))
	if code, out, errs := run(t, stateArgs(tr, root, state)...); code != 0 {
		t.Fatalf("seed: %d\n%s%s", code, out, errs)
	}
	stateDir := filepath.Dir(state)
	for n := 0; n < 12; n++ {
		var junk []string
		for i := 0; i < n; i++ {
			junk = append(junk, junkLine(logOrigin(), newSigner(byte(100+i))))
		}
		// The size-300 note of the same log, served with n junk lines: a rollback.
		publishNote(t, root, all, 300, signedBy(300, all, []signer{k1}, junk...))
		code, out, errs := run(t, stateArgs(tr, root, state)...)
		if code != 1 || strings.Contains(out, "NOT saved") {
			t.Fatalf("variant %d: status %d, want 1\n%s%s", n, code, out, errs)
		}
	}
	var saved []string
	ents, _ := os.ReadDir(stateDir)
	for _, e := range ents {
		if strings.Contains(e.Name(), ".conflict-") {
			saved = append(saved, e.Name())
		}
	}
	if len(saved) != 1 {
		t.Fatalf("%d evidence files, want 1: %v", len(saved), saved)
	}
	kept := fileText(t, filepath.Join(stateDir, saved[0]))
	if got := sigLines(kept); len(got) != 1 || strings.Contains(kept, "AAAA") {
		t.Errorf("the evidence is not canonical:\n%s", kept)
	}
}

// Status 4 shows the note, so that it is not lost with the run: every name for
// the evidence is taken by a file with other content.
func TestStatus4ShowsTheNoteToKeep(t *testing.T) {
	k1 := newSigner(1)
	all := leaves(700)
	tr := trustOf(t, keySpec{"k1", k1, ""})
	state := filepath.Join(testdir.New(t), "state")
	root := testdir.New(t)
	publishNote(t, root, all, 700, signedBy(700, all, []signer{k1}))
	if code, out, errs := run(t, stateArgs(tr, root, state)...); code != 0 {
		t.Fatalf("seed: %d\n%s%s", code, out, errs)
	}
	publishNote(t, root, all, 300, signedBy(300, all, []signer{k1}))
	base := rollbackName(t, root, state, all)
	if err := os.WriteFile(base, []byte("other"), 0o600); err != nil {
		t.Fatal(err)
	}
	for i := 2; i <= 8; i++ {
		if err := os.WriteFile(fmt.Sprintf("%s-%d", base, i), []byte("other"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	code, out, _ := run(t, append(stateArgs(tr, root, state), "--json")...)
	if code != 4 {
		t.Fatalf("status %d, want 4\n%s", code, out)
	}
	var got struct {
		Info struct {
			State struct {
				NoteBase64 string `json:"note_base64"`
			} `json:"state"`
		} `json:"info"`
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatal(err)
	}
	raw, err := base64.StdEncoding.DecodeString(got.Info.State.NoteBase64)
	if err != nil || len(raw) == 0 {
		t.Fatalf("no note in the output: %v\n%s", err, out)
	}
	if n, err := signednote.Parse(raw); err != nil || len(n.Signatures) != 1 {
		t.Errorf("the note shown does not parse: %v", err)
	}
	_, text, _ := run(t, stateArgs(tr, root, state)...)
	if _, out, _ := run(t, stateArgs(tr, root, state)...); !strings.Contains(out, "decode it into a file") {
		t.Errorf("the text output does not show the note:\n%s%s", out, text)
	}
}

// rollbackName is the first name SaveConflict tries for the size-300 note.
func rollbackName(t *testing.T, root, state string, all []h32) string {
	t.Helper()
	r := refMTH(all[:300])
	return filepath.Join(filepath.Dir(state), fmt.Sprintf("%s.conflict-%d-%x", filepath.Base(state), 300, r[:]))
}

// ---- the property ----

// pool is the keys of the property test.
type pool struct{ keys []signer }

func newPool(n int) pool {
	var p pool
	for i := 0; i < n; i++ {
		p.keys = append(p.keys, newSigner(byte(10+i)))
	}
	return p
}

// trustVariant is a trust file over some keys of the pool, each with a random
// standing: usable, retired (not_after in the past), not yet valid, revoked.
type standing int

const (
	stUsable standing = iota
	stRetired
	stFuture
	stRevoked
	// stLimited is a key that counts only up to tree size 400 (max_tree_size).
	stLimited
)

func (s standing) extra() string {
	switch s {
	case stRetired:
		return `,"not_after":"2020-01-01T00:00:00Z"`
	case stFuture:
		return `,"not_before":"2999-01-01T00:00:00Z"`
	case stRevoked:
		return `,"revoked":true`
	case stLimited:
		return `,"max_tree_size":400`
	}
	return ""
}

func trustWithStandings(t *testing.T, p pool, st map[int]standing) string {
	var specs []keySpec
	for i := 0; i < len(p.keys); i++ {
		if s, ok := st[i]; ok {
			specs = append(specs, keySpec{fmt.Sprintf("key%d", i), p.keys[i], s.extra()})
		}
	}
	return trustOf(t, specs...)
}

// TestStateSurvivesEveryChangeOfTheTrustFile is the property of the invariant.
// For a note that verifies, with any junk a publisher without a key can add (lines
// of keys the file does not know, of keys it knows but cannot use now, in any
// number and order), and for any later trust file that still holds a key that
// signed it and counts it: the state written from it is read back and extends
// correctly. And over any sequence of trust files no run ends in a state that
// the trust file of that run does not accept, and a run that refuses leaves the
// state as it was.
func TestStateSurvivesEveryChangeOfTheTrustFile(t *testing.T) {
	const keys = 5
	p := newPool(keys + 1) // the last key signs the new note of the later trust files
	all := leaves(1100)
	sizes := []int{255, 256, 300, 400, 513, 700, 900, 1100}
	for seed := int64(0); seed < 25; seed++ {
		rng := rand.New(rand.NewSource(seed))
		state := filepath.Join(testdir.New(t), "state")
		root := testdir.New(t)
		var counted map[int]bool // keys that signed the note in the state, usable and unrevoked when it was written
		for step, size := range sizes {
			// A random trust file: every key present or not, with a random standing.
			st := map[int]standing{}
			for i := 0; i < keys; i++ {
				if rng.Intn(5) > 0 || i == 0 {
					st[i] = standing(rng.Intn(4))
				}
			}
			if step > 0 && step%3 == 0 {
				// now and then keep one key that signed the held note, as usable
				for i := range counted {
					st[i] = stUsable
					break
				}
			}
			// The publisher signs with a random non-empty set of the keys and adds junk.
			var signers []signer
			var signedIdx []int
			for i := 0; i < keys; i++ {
				if rng.Intn(2) == 0 {
					signers = append(signers, p.keys[i])
					signedIdx = append(signedIdx, i)
				}
			}
			if len(signers) == 0 {
				signers = []signer{p.keys[0]}
				signedIdx = []int{0}
			}
			var junk []string
			for i := 0; i < keys; i++ {
				// a line naming a key that does not sign, if the file cannot use that key
				// (an unknown key, or one retired, not yet valid or revoked): any publisher
				// can write it
				signs := false
				for _, j := range signedIdx {
					if j == i {
						signs = true
					}
				}
				if signs {
					continue
				}
				if s, in := st[i]; !in || s != stUsable {
					if rng.Intn(2) == 0 {
						junk = append(junk, junkLine(logOrigin(), p.keys[i]))
					}
				}
			}
			for i := rng.Intn(4); i > 0; i-- {
				junk = append(junk, junkLine(logOrigin(), newSigner(byte(200+rng.Intn(40)))))
			}
			junk = uniqueStrings(junk)
			rng.Shuffle(len(junk), func(a, b int) { junk[a], junk[b] = junk[b], junk[a] })
			note := signedBy(size, all, signers, junk...)
			publishNote(t, root, all, size, note)
			tr := trustWithStandings(t, p, st)
			before, _ := os.ReadFile(state)
			code, out, errs := run(t, stateArgs(tr, root, state)...)
			after, _ := os.ReadFile(state)
			// does any usable, unrevoked key of this file sign the note?
			usableSigner := false
			for _, i := range signedIdx {
				if s, in := st[i]; in && s == stUsable {
					usableSigner = true
				}
			}
			switch code {
			case 0:
				if !usableSigner {
					t.Fatalf("seed %d step %d: status 0 with no usable signer\n%s", seed, step, out)
				}
				if stateSize(t, state) != uint64(size) {
					t.Fatalf("seed %d step %d: status 0 and the state is at %d, want %d\n%s", seed, step, stateSize(t, state), size, out)
				}
				// no run ends in a state that its own trust file does not accept
				if c, o, e := run(t, "consistency", "--trust", tr, "--log", logSpecArg(), "--source", root, "--old", state); c != 0 {
					t.Fatalf("seed %d step %d: the state just written is refused by the trust file that wrote it: %d\n%s%s", seed, step, c, o, e)
				}
				if got := sigLines(storedNote(t, state)); len(got) != countInFile(signedIdx, st) {
					t.Fatalf("seed %d step %d: the state keeps %d lines, %d verify under a key of the trust file (any standing):\n%s", seed, step, len(got), countInFile(signedIdx, st), storedNote(t, state))
				}
				counted = map[int]bool{}
				for _, i := range signedIdx {
					if _, in := st[i]; in {
						counted[i] = true
					}
				}
			default:
				// a refusal never moves the state
				if string(before) != string(after) {
					t.Fatalf("seed %d step %d: status %d and the state changed", seed, step, code)
				}
				if strings.Contains(out, "VERIFIED") {
					t.Fatalf("seed %d step %d: VERIFIED with status %d", seed, step, code)
				}
				// The only honest reasons: no usable key signed the new note, or the
				// state held cannot be used with this trust file (all its signers are
				// revoked or gone from it).
				if usableSigner && !heldUnusable(counted, st) && code != 3 {
					t.Fatalf("seed %d step %d: status %d with a usable signer and a usable state\n%s%s", seed, step, code, out, errs)
				}
			}
			// The state accepts every later trust file that holds a key that signed it
			// and counts it: try three variants with the key present as the only one,
			// with others added, with other windows.
			if code == 0 {
				for v := 0; v < 3; v++ {
					alt := map[int]standing{}
					for i := range counted {
						if rng.Intn(2) == 0 || len(alt) == 0 {
							alt[i] = stUsable
						}
					}
					for i := 0; i < keys; i++ {
						if _, in := alt[i]; !in && !counted[i] && rng.Intn(2) == 0 {
							alt[i] = standing(rng.Intn(4))
						}
						if counted[i] && alt[i] == stUsable && rng.Intn(3) == 0 {
							alt[i] = stRetired // a window: the held note is judged when it was kept
						}
					}
					// a counted key that stays must not be revoked
					if len(alt) == 0 {
						continue
					}
					keepsOne := false
					for i := range counted {
						if _, in := alt[i]; in {
							keepsOne = true
						}
					}
					if !keepsOne {
						continue
					}
					alt[keys] = stUsable
					at := trustWithStandings(t, p, alt)
					newNote := writeNote(t, signedBy(size, all, []signer{p.keys[keys]}))
					if c, o, e := run(t, "consistency", "--trust", at, "--log", logSpecArg(), "--source", root, "--old", state, "--new", newNote); c != 0 {
						t.Fatalf("seed %d step %d: a later trust file %v that holds a signing key refuses the state: %d\n%s%s", seed, step, alt, c, o, e)
					}
				}
			}
		}
	}
}

// countInFile is how many of the signing keys are in the trust file at all:
// the state keeps every line that verifies under a key of the file, whatever the
// standing of the key (L12-01).
func countInFile(signed []int, st map[int]standing) int {
	n := 0
	for _, i := range signed {
		if _, in := st[i]; in {
			n++
		}
	}
	return n
}

// heldUnusable says whether the state held cannot serve under this trust file:
// none of the keys that signed it is in the file (a key that is revoked, retired
// or limited is still there, and still verifies the state).
func heldUnusable(counted map[int]bool, st map[int]standing) bool {
	if len(counted) == 0 {
		return false
	}
	for i := range counted {
		if _, in := st[i]; in {
			return false
		}
	}
	return true
}

var nowForTests = time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)

func mustCheckpoint(t *testing.T, text []byte) *checkpoint.Checkpoint {
	t.Helper()
	cp, err := checkpoint.Parse(text)
	if err != nil {
		t.Fatal(err)
	}
	return cp
}

// uniqueStrings drops repeats: two lines of one key are a malformed note.
func uniqueStrings(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

// TestReadBackRefusesWhatALaterRunWouldNotRead calls the guard that stands
// behind the canonical form: a file that a later run would refuse is not
// stored. By construction it is never reached with a note that came through
// canonicalOf; here it is given notes that did not.
func TestReadBackRefusesWhatALaterRunWouldNotRead(t *testing.T) {
	k1, k2 := newSigner(1), newSigner(2)
	all := leaves(300)
	root := testdir.New(t)
	tr := trustOf(t, keySpec{"k1", k1, ""}, keySpec{"k2", k2, `,"revoked":true`})
	good := signedBy(300, all, []signer{k1})
	onlyRevoked := signedBy(300, all, []signer{k2})
	publishNote(t, root, all, 300, good)
	in, err := loadLogInputs(tr, logSpecArg(), root, nowArg{}, true)
	if err != nil {
		t.Fatal(err)
	}
	defer in.close()
	cp := mustCheckpoint(t, []byte(strings.SplitN(good, "\n\n", 2)[0]+"\n"))
	enc := func(note string) []byte {
		b, err := encodeState(cp, []byte(note), nil, nowForTests)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	if err := in.readBack(enc(good), cp, []byte(good)); err != nil {
		t.Fatalf("a good file is refused: %v", err)
	}
	// A note of a revoked key only is a state a later run reads (L12-01).
	if err := in.readBack(enc(onlyRevoked), cp, []byte(onlyRevoked)); err != nil {
		t.Fatalf("a note of a revoked key only: %v", err)
	}
	// A note that no key of the file signed is not.
	k3 := newSigner(3)
	nobody := signedBy(300, all, []signer{k3})
	if err := in.readBack(enc(nobody), cp, []byte(nobody)); err == nil || !strings.Contains(err.Error(), "cannot be recorded") || strings.Contains(err.Error(), "must not be deleted") {
		t.Fatalf("a note that no key of the file signed: %v", err)
	}
	// the file reads back, but as another note than the one verified
	if err := in.readBack(enc(good), cp, []byte(good+"x")); err == nil {
		t.Fatal("a file that differs from the verified note was accepted")
	}
	// the record says another size than the note
	other := *cp
	other.Size = 299
	if err := in.readBack(enc(good), &other, []byte(good)); err == nil {
		t.Fatal("a file with another size was accepted")
	}
}

// FuzzStateFromNote drives the property with bytes: which keys sign, what junk is
// added to the note (the raw bytes become signature lines that any publisher
// can write), and which trust file reads it. Whatever the note, a run that
// ends with status 0 leaves a state that the same trust file reads back, a run
// that does not leaves the state as it was, and no run prints VERIFIED with a
// status other than 0.
func FuzzStateFromNote(f *testing.F) {
	f.Add([]byte{1, 0, 0, 0, 0, 0}, []byte{}, []byte{0, 0, 0})
	f.Add([]byte{3, 1, 2, 0, 0, 0}, []byte("zero line"), []byte{1, 0, 2, 3})
	f.Add([]byte{0, 0, 0, 0, 0, 0}, []byte("\u2014 x y\n"), []byte{3, 3, 3, 3})
	p := newPool(4)
	all := leaves(700)
	f.Fuzz(func(t *testing.T, signBits, junkRaw, standBits []byte) {
		if len(signBits) == 0 {
			signBits = []byte{1}
		}
		var signers []signer
		for i := 0; i < 4; i++ {
			if signBits[0]>>uint(i)&1 == 1 {
				signers = append(signers, p.keys[i])
			}
		}
		if len(signers) == 0 {
			signers = []signer{p.keys[0]}
		}
		var junk []string
		for i, b := range junkRaw {
			if i > 8 {
				break
			}
			switch b % 3 {
			case 0:
				junk = append(junk, junkLine(logOrigin(), newSigner(byte(100+i))))
			case 1:
				junk = append(junk, junkLine(logOrigin(), p.keys[int(b)%4]))
			default:
				junk = append(junk, "\u2014 "+logOrigin()+" "+base64.StdEncoding.EncodeToString(append([]byte{b, b, b, b}, junkRaw[:i]...))+"\n")
			}
		}
		junk = uniqueStrings(junk)
		for _, size := range []int{300, 700} {
			note := signedBy(size, all, signers, junk...)
			if _, err := signednote.Parse([]byte(note)); err != nil {
				return // a duplicate line or a malformed one: a refused note, not the property
			}
		}
		st := map[int]standing{}
		for i := 0; i < 4; i++ {
			if i < len(standBits) {
				if standBits[i]%5 != 4 {
					st[i] = standing(standBits[i] % 4)
				}
			} else {
				st[i] = stUsable
			}
		}
		root := testdir.New(t)
		state := filepath.Join(testdir.New(t), "state")
		tr := trustWithStandings(t, p, st)
		for _, size := range []int{300, 700} {
			publishNote(t, root, all, size, signedBy(size, all, signers, junk...))
			before, _ := os.ReadFile(state)
			code, out, _ := run(t, stateArgs(tr, root, state)...)
			after, _ := os.ReadFile(state)
			if code == 0 {
				if c, o, e := run(t, "consistency", "--trust", tr, "--log", logSpecArg(), "--source", root, "--old", state); c != 0 {
					t.Fatalf("size %d: the state just written is refused: %d\n%s%s", size, c, o, e)
				}
				if stateSize(t, state) != uint64(size) {
					t.Fatalf("size %d: status 0 and the state is at %d", size, stateSize(t, state))
				}
			} else {
				if string(before) != string(after) {
					t.Fatalf("size %d: status %d and the state changed", size, code)
				}
				if strings.Contains(out, "VERIFIED") {
					t.Fatalf("VERIFIED with status %d", code)
				}
			}
		}
	})
}
