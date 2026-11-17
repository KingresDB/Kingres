// SPDX-License-Identifier: AGPL-3.0-only

package signednote

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"testing"
)

// L14-03 and L14-06 (M03): what Merge and MergeStored do with the text, the lines
// they hold and the lines they are given.

const mergeOrigin = "origin/m"

func mergeLine(k testKeyPair, text string, sig []byte) string {
	var id [4]byte
	binary.BigEndian.PutUint32(id[:], KeyID(mergeOrigin, k.pub))
	if sig == nil {
		sig = ed25519.Sign(k.priv, []byte(text))
	}
	return "\u2014 " + mergeOrigin + " " + base64.StdEncoding.EncodeToString(append(id[:], sig...)) + "\n"
}

func mergeNote(t *testing.T, text string, lines ...string) *Note {
	t.Helper()
	b := text + "\n"
	for _, l := range lines {
		b += l
	}
	n, err := Parse([]byte(b))
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// Notes with other texts are not the same checkpoint, and Merge says so before it
// looks at a line (M03: the check is reached here, by a test of its own).
func TestMergeRefusesNotesOfAnotherText(t *testing.T) {
	k1 := testKey(1)
	keys := []Key{{mergeOrigin, k1.pub}}
	a, b := "origin/m\n1\nAAAA\n", "origin/m\n2\nAAAA\n"
	na := mergeNote(t, a, mergeLine(k1, a, nil))
	nb := mergeNote(t, b, mergeLine(k1, b, nil))
	if _, _, err := Merge(keys, na, nb); !errors.Is(err, ErrDifferentText) {
		t.Fatalf("Merge of two texts: %v", err)
	}
	if _, err := MergeStored(keys, na, nb); !errors.Is(err, ErrDifferentText) {
		t.Fatalf("MergeStored of two texts: %v", err)
	}
	// the same text with the lines of both: fine
	if _, _, err := Merge(keys, na, na); err != nil {
		t.Fatal(err)
	}
}

func TestMergeStoredKeepsWhatIsStoredAndVerifiesWhatIsNew(t *testing.T) {
	text := "origin/m\n1\nAAAA\n"
	k1, k2, k3, k4, gone := testKey(1), testKey(2), testKey(3), testKey(4), testKey(9)
	keys := []Key{{mergeOrigin, k1.pub}, {mergeOrigin, k2.pub}, {mergeOrigin, k3.pub}, {mergeOrigin, k4.pub}}
	stored := mergeNote(t, text,
		mergeLine(k1, text, nil),              // verifies
		mergeLine(gone, text, nil),            // a key that is not in keys: kept as stored
		mergeLine(k2, text, make([]byte, 64))) // a key in keys, wrong: dropped
	served := mergeNote(t, text,
		mergeLine(k1, text, nil),              // already stored
		mergeLine(k2, text, nil),              // new and genuine: taken
		mergeLine(k3, text, make([]byte, 64)), // new and wrong: not taken
		mergeLine(testKey(8), text, nil))      // new and of a key that is not in keys: not taken
	st, err := MergeStored(keys, stored, served)
	if err != nil {
		t.Fatal(err)
	}
	if st.Added != 1 || len(st.Verified) != 2 || st.Unverifiable != 1 || st.Dropped != 0 {
		t.Fatalf("%+v", st)
	}
	out, err := Parse(st.Bytes)
	if err != nil || len(out.Signatures) != 3 {
		t.Fatalf("the result does not parse into 3 lines: %v\n%s", err, st.Bytes)
	}
	// the line of the key that is gone is in it, byte for byte
	if !bytes.Contains(st.Bytes, []byte(mergeLine(gone, text, nil))) {
		t.Fatalf("the stored line of a key that left was not kept:\n%s", st.Bytes)
	}
	// nothing new: nothing added, and the canonical form is the same
	again, err := MergeStored(keys, mustParse(t, st.Bytes), served)
	if err != nil || again.Added != 0 || !bytes.Equal(again.Bytes, st.Bytes) {
		t.Fatalf("a second merge: %+v %v", again, err)
	}
	// a line of a key that is gone makes nothing usable: with only such lines
	// there is no verified line and MergeStored refuses; VerifyHeld ignores it
	onlyGone := mergeNote(t, text, mergeLine(gone, text, nil))
	if _, err := MergeStored(keys, onlyGone, onlyGone); !errors.Is(err, ErrNoTrustedSignature) {
		t.Fatalf("only a line of a key that left: %v", err)
	}
	if _, _, err := VerifyHeld(onlyGone, keys); !errors.Is(err, ErrNoTrustedSignature) {
		t.Fatalf("VerifyHeld of only such a line: %v", err)
	}
}

func mustParse(t *testing.T, b []byte) *Note {
	t.Helper()
	n, err := Parse(b)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// Under MaxSignatures lines the lines that do not verify are left out first.
func TestMergeStoredDropsUnverifiableLinesBeforeVerifiedOnes(t *testing.T) {
	text := "origin/m\n1\nAAAA\n"
	var keys []Key
	var lines []string
	good := testKey(1)
	keys = append(keys, Key{mergeOrigin, good.pub})
	lines = append(lines, mergeLine(good, text, nil))
	for i := 0; i < MaxSignatures-1; i++ {
		lines = append(lines, mergeLine(testKey(byte(20+i)), text, nil)) // keys that are not in keys
	}
	stored := mergeNote(t, text, lines...)
	served := mergeNote(t, text, mergeLine(testKey(2), text, nil))
	keys = append(keys, Key{mergeOrigin, testKey(2).pub})
	st, err := MergeStored(keys, stored, served)
	if err != nil {
		t.Fatal(err)
	}
	if st.Added != 1 || st.Dropped != 1 || len(st.Verified) != 2 || st.Unverifiable != MaxSignatures-2 {
		t.Fatalf("%+v", st)
	}
	if n, err := Parse(st.Bytes); err != nil || len(n.Signatures) != MaxSignatures {
		t.Fatalf("the result is not a note that Parse takes: %v", err)
	}
}
