// SPDX-License-Identifier: AGPL-3.0-only

package cli

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/KingresDB/Kingres/kingres/verifier/internal/checkpoint"
	"github.com/KingresDB/Kingres/kingres/verifier/internal/signednote"
)

// noteText signs text (which starts with the origin) as s, with extra signature
// lines of keys nobody trusts after the real one.
func (s signer) noteText(text string, extraSigLines int) string {
	origin := text[:strings.IndexByte(text, '\n')]
	sum := sha256.Sum256(append(append([]byte(origin), 0x0A, 0x01), s.pub...))
	raw := append(append([]byte{}, sum[:4]...), ed25519.Sign(s.priv, []byte(text))...)
	out := text + "\n\u2014 " + origin + " " + base64.StdEncoding.EncodeToString(raw) + "\n"
	for i := 0; i < extraSigLines; i++ {
		name := fmt.Sprintf("%0250d", i)
		out += "\u2014 " + name + " " + base64.StdEncoding.EncodeToString(make([]byte, 68)) + "\n"
	}
	return out
}

// textWithExtensions is the checkpoint text of size entries with extension lines.
func textWithExtensions(size int, root h32, ext ...string) string {
	t := fmt.Sprintf("%s\n%d\n%s\n", logOrigin(), size, base64.StdEncoding.EncodeToString(root[:]))
	for _, e := range ext {
		t += e + "\n"
	}
	return t
}

// L9-04: a validly signed note with a long extension line made a state file
// of 4.2 MB that no later run could read. The note is bounded where it is
// parsed, so the checkpoint is refused before anything is stored.
func TestLongExtensionLineIsRefusedBeforeAnythingIsStored(t *testing.T) {
	s := newSigner(1)
	tr := trustOf(t, keySpec{"k", s, ""})
	all := leaves(300)
	root := t.TempDir()
	publishLog(t, root, s, all, 300)
	state := filepath.Join(t.TempDir(), "state")
	if code, out, _ := run(t, stateArgs(tr, root, state)...); code != 0 {
		t.Fatal(out)
	}
	kept := fileText(t, state)
	big := t.TempDir()
	publishLog(t, big, s, all, 300)
	long := s.noteText(textWithExtensions(300, refMTH(all), strings.Repeat("<", 700000)), 0)
	if err := os.WriteFile(filepath.Join(big, "c", testChain, "e1", "checkpoint"), []byte(long), 0o644); err != nil {
		t.Fatal(err)
	}
	fresh := filepath.Join(t.TempDir(), "fresh")
	for _, args := range [][]string{stateArgs(tr, big, state), stateArgs(tr, big, fresh)} {
		code, out, errs := run(t, args...)
		if code == 0 || strings.Contains(out, "VERIFIED") {
			t.Fatalf("a note of %d bytes was accepted: %d\n%s%s", len(long), code, out, errs)
		}
		if code != 1 || !strings.Contains(out, "FAILED at step") {
			t.Errorf("status %d, want 1 with a failed step:\n%s%s", code, out, errs)
		}
	}
	if fileText(t, state) != kept {
		t.Fatal("the state was changed")
	}
	if _, err := os.Stat(fresh); err == nil {
		t.Fatal("a state was created for a refused note")
	}
	// The same note as a held note is an input error.
	if code, _, _ := run(t, "consistency", "--trust", tr, "--log", logSpecArg(), "--source", root, "--old", writeNote(t, long)); code != 2 {
		t.Errorf("--old with an oversize note: %d, want 2", code)
	}
	// The protection is not stuck: the next honest run still uses the state.
	if code, out, _ := run(t, stateArgs(tr, root, state)...); code != 0 || !strings.Contains(out, "nothing to prove") {
		t.Fatalf("the state is stuck: %d\n%s", code, out)
	}
}

// The bounds themselves: each limit is met exactly and refused one over.
func TestNoteBoundsAreEnforcedBeforeAnythingIsStored(t *testing.T) {
	s := newSigner(1)
	tr := trustOf(t, keySpec{"k", s, ""})
	all := leaves(300)
	root := t.TempDir()
	publishLog(t, root, s, all, 300)
	r := refMTH(all)
	line := func(n int) string { return strings.Repeat("x", n) }
	cases := []struct {
		name string
		note string
		ok   bool
	}{
		{"extension line at the limit", s.noteText(textWithExtensions(300, r, line(signednote.MaxLineLength)), 0), true},
		{"extension line one over", s.noteText(textWithExtensions(300, r, line(signednote.MaxLineLength+1)), 0), false},
		{"extension lines at the limit", s.noteText(textWithExtensions(300, r, repeatLines(checkpoint.MaxExtensionLines)...), 0), true},
		{"one extension line too many", s.noteText(textWithExtensions(300, r, repeatLines(checkpoint.MaxExtensionLines+1)...), 0), false},
		{"signatures at the limit", s.noteText(textWithExtensions(300, r), signednote.MaxSignatures-1), true},
		{"one signature too many", s.noteText(textWithExtensions(300, r), signednote.MaxSignatures), false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			state := filepath.Join(t.TempDir(), "state")
			args := stateArgs(tr, root, state, "--note", writeNote(t, c.note))
			code, out, errs := run(t, args...)
			if c.ok != (code == 0) {
				t.Fatalf("status %d, accepted should be %v\n%s%s", code, c.ok, out, errs)
			}
			if c.ok {
				// Whatever was stored is read back by the next run.
				if code, out, errs := run(t, args...); code != 0 || !strings.Contains(out, "nothing to prove") {
					t.Fatalf("the state written for the largest allowed note is not read back: %d\n%s%s", code, out, errs)
				}
				if st, err := os.Stat(state); err != nil || st.Size() > maxStateFile {
					t.Fatalf("state %v, limit %d (%v)", st, maxStateFile, err)
				}
			} else if _, err := os.Stat(state); err == nil {
				t.Fatal("a state was written for a refused note")
			}
		})
	}
}

func repeatLines(n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = fmt.Sprintf("extension %d", i)
	}
	return out
}

// The property: every state the code can write is read back, and gives the
// same checkpoint and the same note. The notes are generated: lines of the
// characters that grow most in JSON, of the longest allowed length.
func TestEveryStateTheCodeWritesReadsBack(t *testing.T) {
	s := newSigner(1)
	all := leaves(40)
	r := refMTH(all)
	alphabets := []string{"<", ">&", `"`, `\`, "\u2028\u2029", "\u00e9\u4e2d", "\U0001F600", "a<>&\"\\\u2028 \u00e9"}
	rnd := rand.New(rand.NewSource(7))
	key := signednote.Key{Name: logOrigin(), Public: s.pub}
	for i := 0; i < 200; i++ {
		var ext []string
		for j := rnd.Intn(checkpoint.MaxExtensionLines + 1); j > 0; j-- {
			alpha := []rune(alphabets[rnd.Intn(len(alphabets))])
			var b strings.Builder
			for b.Len() <= signednote.MaxLineLength-5 {
				b.WriteRune(alpha[rnd.Intn(len(alpha))])
			}
			ext = append(ext, strings.TrimSpace(b.String())+"x")
		}
		note := s.noteText(textWithExtensions(40, r, ext...), rnd.Intn(signednote.MaxSignatures))
		if len(note) > signednote.MaxNoteSize {
			t.Fatalf("the generator made a note of %d bytes", len(note))
		}
		checkStateRoundTrip(t, []byte(note), key)
	}
}

// checkStateRoundTrip: when the note verifies, the state made from it is
// within the limit and reads back to the same note and checkpoint.
func checkStateRoundTrip(t *testing.T, note []byte, key signednote.Key) {
	t.Helper()
	cp, signers, err := checkpoint.Verify(note, []signednote.Key{key}, logOrigin())
	if err != nil {
		return // a note that does not verify is never stored
	}
	data, err := encodeState(cp, note, signers, time.Now())
	if err != nil {
		t.Fatalf("a note that passed the parser cannot be stored (%d bytes): %v", len(note), err)
	}
	if len(data) > maxStateFile {
		t.Fatalf("state of %d bytes, limit %d", len(data), maxStateFile)
	}
	held, rec, err := decodeHeld(data)
	if err != nil {
		t.Fatalf("the state cannot be read back: %v", err)
	}
	if string(held) != string(note) {
		t.Fatal("the note read back is not the note written")
	}
	back, _, err := checkpoint.Verify(held, []signednote.Key{key}, logOrigin())
	if err != nil || back.Size != cp.Size || back.Root != cp.Root {
		t.Fatalf("the checkpoint read back differs: %v", err)
	}
	if err := rec.agrees(back); err != nil {
		t.Fatal(err)
	}
}

func FuzzStateRoundTrip(f *testing.F) {
	s := newSigner(1)
	r := refMTH(leaves(40))
	f.Add("<>&", "\u2028", 3)
	f.Add(strings.Repeat("\\\"", 500), "x", 40)
	key := signednote.Key{Name: logOrigin(), Public: s.pub}
	f.Fuzz(func(t *testing.T, a, b string, sigs int) {
		var ext []string
		for _, e := range []string{a, b} {
			for _, l := range strings.Split(e, "\n") {
				if l != "" {
					ext = append(ext, l)
				}
			}
		}
		if sigs < 0 {
			sigs = -sigs
		}
		note := s.noteText(textWithExtensions(40, r, ext...), sigs%signednote.MaxSignatures)
		checkStateRoundTrip(t, []byte(note), key)
	})
}

// L9-04, the dead end that is not about size: a note that is good as a new
// checkpoint but that a later run refuses as a held one. A corrupt signature of
// a key that is revoked in the trust file is ignored for a new note (a revoked
// key does not sign) and refused for a held one (a held note allows no wrong
// signature of any key of the file). Storing it would stick the state, so it is
// not stored.
func TestNoteThatWouldNotBeReadBackIsNotStored(t *testing.T) {
	k1, k2 := newSigner(1), newSigner(2)
	all := leaves(300)
	good := k2.note(logOrigin(), 300, refMTH(all))
	// k1's line signs another text: the signature is wrong for this one.
	other := strings.Split(k1.note(logOrigin(), 299, refMTH(all[:299])), "\n\n")[1]
	note := good + other
	tr := trustOf(t, keySpec{"k1", k1, `,"revoked":true`}, keySpec{"k2", k2, ""})
	root := t.TempDir()
	publishLog(t, root, k2, all, 300)
	file := writeNote(t, note)
	if code, out, errs := run(t, "checkpoint", "--trust", tr, "--log", logSpecArg(), "--source", root, "--note", file); code != 0 {
		t.Fatalf("without --state the note is a new checkpoint: %d\n%s%s", code, out, errs)
	}
	state := filepath.Join(t.TempDir(), "state")
	code, out, errs := run(t, stateArgs(tr, root, state, "--note", file)...)
	if code != 2 || strings.Contains(out, "VERIFIED") || !strings.Contains(errs, "cannot be recorded") {
		t.Fatalf("status %d\n%s%s", code, out, errs)
	}
	if _, err := os.Stat(state); err == nil {
		t.Fatal("a state that no later run accepts was written")
	}
	// A clean note is then recorded and used.
	if code, out, _ := run(t, stateArgs(tr, root, state)...); code != 0 {
		t.Fatal(out)
	}
}
