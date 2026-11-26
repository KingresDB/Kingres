// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package ledgerlog

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/binary"
	"errors"
	"fmt"
	"io/fs"
	"math/rand"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/ledgerkeys"
)

// The tests of this file are the checks that the fixes of the review of the
// log library need (the numbering is that of the review).

// ---- K1-01: Reader is total for any TileGetter ----

// liar is a TileGetter that tells the truth except at one call, the at-th
// (counting from 1), where it answers as mode says.
type liar struct {
	inner TileGetter
	at    int
	mode  string
	calls int
}

func (l *liar) GetTile(ctx context.Context, k TileKey) ([]byte, error) {
	l.calls++
	d, err := l.inner.GetTile(ctx, k)
	if l.calls != l.at {
		return d, err
	}
	switch l.mode {
	case "short":
		return make([]byte, 31), nil
	case "empty":
		return nil, nil
	case "long":
		return append(bytes.Clone(d), 0), nil
	case "double":
		return append(bytes.Clone(d), d...), nil
	case "wrong width":
		w := k.Width - 1
		if w < 1 {
			w = 2
		}
		if k.Level == EntryLevel {
			return EncodeBundle(entriesRange(0, w))
		}
		return EncodeTile(leafHashesOf(w))
	case "huge":
		return make([]byte, 1<<20), nil
	case "error":
		return nil, errors.New("the getter failed")
	}
	panic("unknown mode " + l.mode)
}

func TestReaderIsTotalForAnyGetter(t *testing.T) {
	ctx := context.Background()
	scenarios := []struct {
		name string
		src  func(t *testing.T) TileGetter
		size uint64
	}{
		{"partial tiles", func(t *testing.T) TileGetter {
			s := NewMemStore()
			l, _ := Create(bg, testOrigin(1), s)
			mustAppend(t, l, 0, 1000)
			return s
		}, 1000},
		{"only full tiles (the partial tile was deleted)", func(t *testing.T) TileGetter {
			s := NewMemStore()
			l, _ := Create(bg, testOrigin(1), s)
			mustAppend(t, l, 0, 300)
			mustAppend(t, l, 300, 600)
			// The partial tile and bundle of size 300 are gone once the full
			// ones exist, as the reader may meet a store that pruned them.
			s.Delete(TileKey{0, 1, 44})
			s.Delete(TileKey{EntryLevel, 1, 44})
			return s
		}, 300},
	}
	for _, sc := range scenarios {
		src := sc.src(t)
		root := func() Hash {
			r, err := NewReader(ctx, src, sc.size)
			if err != nil {
				t.Fatal(err)
			}
			h, err := r.Root()
			if err != nil {
				t.Fatalf("%s: the honest root: %v", sc.name, err)
			}
			return h
		}()
		last := sc.size - 1
		ops := map[string]func(g TileGetter) error{
			"Root": func(g TileGetter) error {
				r, _ := NewReader(ctx, g, sc.size)
				_, err := r.Root()
				return err
			},
			"LeafHash": func(g TileGetter) error {
				r, _ := NewReader(ctx, g, sc.size)
				_, err := r.LeafHash(last)
				return err
			},
			"Entry": func(g TileGetter) error {
				r, _ := NewReader(ctx, g, sc.size)
				_, err := r.Entry(last)
				return err
			},
			"InclusionProof": func(g TileGetter) error {
				r, _ := NewReader(ctx, g, sc.size)
				_, err := r.InclusionProof(sc.size / 3)
				return err
			},
			"ConsistencyProof": func(g TileGetter) error {
				r, _ := NewReader(ctx, g, sc.size)
				_, err := r.ConsistencyProof(sc.size / 3)
				return err
			},
			"Audit": func(g TileGetter) error { return Audit(ctx, g, sc.size, root) },
		}
		for name, op := range ops {
			honest := &liar{inner: src}
			if err := op(honest); err != nil {
				t.Fatalf("%s %s: the honest run fails: %v", sc.name, name, err)
			}
			n := honest.calls
			if n == 0 {
				t.Fatalf("%s %s: no tile was read", sc.name, name)
			}
			for _, mode := range []string{"short", "empty", "long", "double", "wrong width", "huge", "error"} {
				for at := 1; at <= n; at++ {
					g := &liar{inner: src, at: at, mode: mode}
					var err error
					func() {
						defer func() {
							if r := recover(); r != nil {
								t.Errorf("%s %s: a getter that answers %q at call %d of %d made the reader panic: %v", sc.name, name, mode, at, n, r)
							}
						}()
						err = op(g)
					}()
					if err == nil {
						t.Errorf("%s %s: a getter that answers %q at call %d of %d was believed", sc.name, name, mode, at, n)
					}
				}
			}
		}
	}
}

// The case of the review, by name: the partial tile is not found and the full
// tile has 31 bytes.
func TestReaderShortFullTile(t *testing.T) {
	s := NewMemStore()
	l, _ := Create(bg, testOrigin(1), s)
	mustAppend(t, l, 0, 300)
	mustAppend(t, l, 300, 600)
	g := getterFunc(func(ctx context.Context, k TileKey) ([]byte, error) {
		if k.Width < TileWidth && k.Level <= 0 {
			return nil, fmt.Errorf("%w: %s", ErrNotFound, k.Path())
		}
		d, err := s.GetTile(ctx, k)
		if err == nil && k.Level == 0 {
			return d[:31], nil
		}
		return d, err
	})
	r, _ := NewReader(bg, g, 300)
	if _, err := r.Root(); !errors.Is(err, ErrTileMalformed) {
		t.Fatalf("a short full tile: %v", err)
	}
}

type getterFunc func(ctx context.Context, k TileKey) ([]byte, error)

func (f getterFunc) GetTile(ctx context.Context, k TileKey) ([]byte, error) { return f(ctx, k) }

// ---- K1-06: Text refuses what ParseCheckpoint refuses ----

func TestCheckpointTextRefusesNewlinesAndTooManyLines(t *testing.T) {
	c := goodCheckpoint()
	c.Extensions = []string{"a\nb"}
	if _, err := c.Text(); !errors.Is(err, ErrCheckpoint) {
		t.Errorf("an extension with a newline: %v", err)
	}
	// The case of the review: ten extensions of two lines each.
	c.Extensions = nil
	for i := 0; i < 10; i++ {
		c.Extensions = append(c.Extensions, "a\nb")
	}
	if _, err := c.Text(); !errors.Is(err, ErrCheckpoint) {
		t.Errorf("ten extensions of two lines: %v", err)
	}
	c.Extensions = nil
	for i := 0; i < MaxExtensionLines; i++ {
		c.Extensions = append(c.Extensions, fmt.Sprintf("ext %d", i))
	}
	text, err := c.Text()
	if err != nil {
		t.Fatalf("%d extensions: %v", MaxExtensionLines, err)
	}
	if _, err := ParseCheckpoint(text); err != nil {
		t.Errorf("the text of %d extensions is not parsed: %v", MaxExtensionLines, err)
	}
	c.Extensions = append(c.Extensions, "one more")
	if _, err := c.Text(); !errors.Is(err, ErrCheckpoint) {
		t.Errorf("%d extensions: %v", MaxExtensionLines+1, err)
	}
}

// Whatever Text accepts, ParseCheckpoint accepts and returns equal, and Text of
// that gives the same bytes.
func TestCheckpointTextThenParseProperty(t *testing.T) {
	rng := newRand(t)
	pool := []string{"a", "b c", "a\nb", "\n", "", " ", "x\x00y", "x\ty", "\x7f", "\xff", "\u00e9", "\u2028", "line\r", "a\n\nb",
		strings.Repeat("z", MaxLineLength), strings.Repeat("z", MaxLineLength+1), "https://example.test/x", "\u2014 sig", "A+B"}
	origins := []string{testOrigin(1), testOrigin(2), "x", "has space", "", "a+b", strings.Repeat("o", MaxOriginSize), strings.Repeat("o", MaxOriginSize+1), "t\u00e9st/log"}
	accepted := 0
	for i := 0; i < 20000; i++ {
		c := Checkpoint{Origin: origins[rng.Intn(len(origins))], Size: uint64(rng.Intn(5)) * uint64(rng.Intn(1000)), Root: LeafHash([]byte{byte(rng.Intn(256))})}
		if c.Size == 0 && rng.Intn(3) != 0 {
			c.Root = EmptyRoot()
		}
		for j := rng.Intn(20); j > 0; j-- {
			c.Extensions = append(c.Extensions, pool[rng.Intn(len(pool))])
		}
		text, err := c.Text()
		if err != nil {
			continue
		}
		accepted++
		back, err := ParseCheckpoint(text)
		if err != nil {
			t.Fatalf("Text accepted %+v and ParseCheckpoint refuses it: %v", c, err)
		}
		if len(c.Extensions) == 0 {
			c.Extensions = nil
		}
		if !reflect.DeepEqual(back, c) {
			t.Fatalf("Text then ParseCheckpoint gives %+v for %+v", back, c)
		}
		again, err := back.Text()
		if err != nil || !bytes.Equal(again, text) {
			t.Fatalf("Text of the parsed checkpoint differs: %v", err)
		}
	}
	if accepted < 1000 {
		t.Fatalf("only %d of the cases were accepted: the property is not exercised", accepted)
	}
}

// ---- K1-07 and K1-11: key id collisions, and re-signing what is verified ----

func collisionSigners(t testing.TB) (a, b *ledgerkeys.FileSigner) {
	t.Helper()
	mk := func(n uint64) *ledgerkeys.FileSigner {
		seed := make([]byte, ledgerkeys.SeedSize)
		binary.BigEndian.PutUint64(seed[24:], n)
		s, err := ledgerkeys.NewFileSigner(seed)
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	// Two seeds found once by search (83,653 tries): under the origin of
	// collisionOrigin their key ids are equal.
	a, b = mk(12362), mk(21524)
	if ledgerkeys.KeyID(collisionOrigin(), a.Public()) != ledgerkeys.KeyID(collisionOrigin(), b.Public()) {
		t.Fatal("the stored seeds do not collide any more")
	}
	return a, b
}

func collisionOrigin() string {
	o, err := ChainOrigin(testPrefix, chainID(200), 1)
	if err != nil {
		panic(err)
	}
	return o
}

func collisionCheckpoint() Checkpoint {
	return Checkpoint{Origin: collisionOrigin(), Size: 17, Root: LeafHash([]byte("root"))}
}

func wantCollision(t *testing.T, what string, err error, a, b *ledgerkeys.FileSigner, bothKnown bool) {
	t.Helper()
	var c *ledgerkeys.KeyIDCollision
	if !errors.Is(err, ledgerkeys.ErrKeyIDCollision) || !errors.As(err, &c) {
		t.Fatalf("%s: not a key id collision: %v", what, err)
	}
	msg := err.Error()
	if strings.Contains(msg, "already signed") {
		t.Errorf("%s: the wrong message: %s", what, msg)
	}
	if !strings.Contains(msg, a.Fingerprint()[:16]) && !strings.Contains(msg, b.Fingerprint()[:16]) {
		t.Errorf("%s: no fingerprint in %s", what, msg)
	}
	if bothKnown && (!strings.Contains(msg, a.Fingerprint()[:16]) || !strings.Contains(msg, b.Fingerprint()[:16])) {
		t.Errorf("%s: both fingerprints are expected in %s", what, msg)
	}
	if c.Name != collisionOrigin() {
		t.Errorf("%s: the name is %q", what, c.Name)
	}
}

func TestKeyIDCollisionIsReportedAsWhatItIs(t *testing.T) {
	a, b := collisionSigners(t)
	c := collisionCheckpoint()
	// Two signers that collide, in either order.
	_, err := SignCheckpoint(bg, c, a, b)
	wantCollision(t, "SignCheckpoint a b", err, a, b, true)
	_, err = SignCheckpoint(bg, c, b, a)
	wantCollision(t, "SignCheckpoint b a", err, a, b, true)
	// One after the other on a note.
	text, _ := c.Text()
	n, err := SignNote(bg, text, c.Origin, a)
	if err != nil {
		t.Fatal(err)
	}
	err = n.AddSignature(bg, c.Origin, b)
	wantCollision(t, "AddSignature", err, a, b, false)
	if len(n.Signatures) != 1 {
		t.Errorf("a refused signature was added: %d lines", len(n.Signatures))
	}
	// The same key twice keeps its own message.
	err = n.AddSignature(bg, c.Origin, a)
	if !errors.Is(err, ErrNote) || errors.Is(err, ledgerkeys.ErrKeyIDCollision) || !strings.Contains(err.Error(), "already signed by this key") {
		t.Errorf("the same key twice: %v", err)
	}
	if _, err := SignCheckpoint(bg, c, a, a); !errors.Is(err, ErrNote) || errors.Is(err, ledgerkeys.ErrKeyIDCollision) {
		t.Errorf("SignCheckpoint with the same signer twice: %v", err)
	}
	// Re-signing with the trusted keys known names both.
	one, _ := SignCheckpoint(bg, c, a)
	_, err = ResignCheckpoint(bg, one, usable(a.Public(), b.Public()), b)
	wantCollision(t, "ResignCheckpoint", err, a, b, true)
	// Two such keys among the trusted ones: the verifier cannot tell the
	// lines apart and refuses; so does VerifyNote and VerifyCheckpoint.
	parsed, _ := ParseNote(one)
	_, err = VerifyNote(parsed, []NoteKey{{c.Origin, a.Public()}, {c.Origin, b.Public()}})
	wantCollision(t, "VerifyNote", err, a, b, true)
	_, _, err = VerifyCheckpoint(one, c.Origin, a.Public(), b.Public())
	wantCollision(t, "VerifyCheckpoint", err, a, b, true)
	// One of them alone, or the same key twice among the trusted ones, is fine.
	if _, _, err := VerifyCheckpoint(one, c.Origin, a.Public(), a.Public()); err != nil {
		t.Errorf("the same trusted key twice: %v", err)
	}
	// Marshal still refuses a note with two lines of one name and key id.
	sa, _ := a.Sign(bg, text)
	sb, _ := b.Sign(bg, text)
	id := ledgerkeys.KeyID(c.Origin, a.Public())
	two := &Note{Text: text, Signatures: []NoteSignature{{c.Origin, id, sa}, {c.Origin, id, sb}}}
	if _, err := two.Marshal(); !errors.Is(err, ErrNote) {
		t.Errorf("Marshal of two lines of one name and id: %v", err)
	}
}

// The decision of K1-07: what the verifier does with two lines of one name and
// key id from two keys. It refuses the note (and, with both keys trusted, any
// note of that name), so the package does not write such a note.
func TestVerifierAndKeyIDCollision(t *testing.T) {
	verifierBin(t)
	a, b := collisionSigners(t)
	dir := t.TempDir()
	var set ledgerkeys.Set
	var err error
	for i, k := range []*ledgerkeys.FileSigner{a, b} {
		if set, err = set.Add(k.Public(), fmt.Sprintf("k%d", i), epoch0); err != nil {
			t.Fatal(err)
		}
		if set, err = set.Activate(k.Fingerprint(), epoch0.Add(time.Duration(i)*time.Hour), 0); err != nil {
			t.Fatal(err)
		}
	}
	tf, err := ledgerkeys.TrustFile(testPrefix, set, ledgerkeys.TrustOptions{})
	if err != nil {
		t.Fatal(err)
	}
	trust := filepath.Join(dir, "trust.json")
	if err := os.WriteFile(trust, tf, 0o644); err != nil {
		t.Fatal(err)
	}
	cp := collisionCheckpoint()
	text, _ := cp.Text()
	id := chainID(200)
	rel := fmt.Sprintf("c/%x/e1", id[:])
	logDir := filepath.Join(dir, filepath.FromSlash(rel))
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		t.Fatal(err)
	}
	line := func(s *ledgerkeys.FileSigner) []byte {
		sig, err := s.Sign(bg, text)
		if err != nil {
			t.Fatal(err)
		}
		return []byte(sigLine(NoteSignature{Name: cp.Origin, ID: ledgerkeys.KeyID(cp.Origin, s.Public()), Sig: sig}))
	}
	write := func(lines ...[]byte) {
		note := append(bytes.Clone(text), '\n')
		for _, l := range lines {
			note = append(note, l...)
		}
		if err := os.WriteFile(filepath.Join(logDir, "checkpoint"), note, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	run := func() (int, string) {
		return runVerifier(t, "checkpoint", "--trust", trust, "--source", dir, "--now", epoch0.Add(48*time.Hour).Format(time.RFC3339), "--log", fmt.Sprintf("chain:%x:e1", id[:]))
	}
	write(line(a), line(b))
	st2, out2 := run()
	t.Logf("two lines of one name and key id, both keys trusted: status %d: %s", st2, strings.TrimSpace(out2))
	if st2 == 0 {
		t.Errorf("the verifier accepts a note with two lines of one name and key id")
	}
	write(line(a))
	st1, out1 := run()
	t.Logf("one line, both colliding keys trusted: status %d: %s", st1, strings.TrimSpace(out1))
	if st1 == 0 {
		t.Errorf("the verifier accepts a note while it trusts two keys with one name and key id")
	}
}

func TestResignCheckpointVerifiesWhatItKeeps(t *testing.T) {
	k1, k2, stranger := testKey(t, 1), testKey(t, 2), testKey(t, 90)
	c := goodCheckpoint()
	one, err := SignCheckpoint(bg, c, k1)
	if err != nil {
		t.Fatal(err)
	}
	trusted := usable(k1.Public())
	two, err := ResignCheckpoint(bg, one, trusted, k2)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := VerifyCheckpoint(two, c.Origin, k1.Public(), k2.Public()); err != nil {
		t.Fatalf("the re-signed note: %v", err)
	}
	// Nothing verifies under the trusted keys: refused, and no signature of
	// the new key is attached to text that nobody verified.
	for name, trusted := range map[string][]ledgerkeys.TrustKey{
		"no trusted key":       nil,
		"another trusted key":  usable(stranger.Public()),
		"the new key only":     usable(k2.Public()),
		"a key of no size":     usable(ed25519.PublicKey{1}),
		"a trusted key, empty": {},
	} {
		out, err := ResignCheckpoint(bg, one, trusted, k2)
		if err == nil {
			t.Errorf("%s: the note was re-signed", name)
		}
		if out != nil {
			t.Errorf("%s: a note came back with the error", name)
		}
	}
	// A forged note: the text was changed after the signature.
	forged := bytes.Replace(one, []byte("\n17\n"), []byte("\n18\n"), 1)
	if bytes.Equal(forged, one) {
		t.Fatal("the test did not change the note")
	}
	if _, err := ResignCheckpoint(bg, forged, trusted, k2); !errors.Is(err, ErrBadSignature) {
		t.Errorf("a forged note: %v", err)
	}
	// A note that carries a bad line of a trusted key next to a good one.
	bad := bytes.Clone(two)
	i := bytes.Index(bad, []byte("\n\n")) + 2 + len("\u2014 ") + len(c.Origin) + 1 + 10
	bad[i] ^= 1
	if _, err := ResignCheckpoint(bg, bad, usable(k1.Public(), k2.Public()), testKey(t, 91)); err == nil {
		t.Error("a note with a damaged line was re-signed")
	}
	// A note signed by an untrusted key only: the unknown line is ignored by
	// VerifyNote, and with no trusted line the note is refused.
	foreign, _ := SignCheckpoint(bg, c, stranger)
	if _, err := ResignCheckpoint(bg, foreign, trusted, k2); !errors.Is(err, ErrNoTrustedSignature) {
		t.Errorf("a note of an untrusted key: %v", err)
	}
	// A note that is not valid at all.
	if _, err := ResignCheckpoint(bg, []byte("x"), trusted, k2); err == nil {
		t.Error("garbage was re-signed")
	}
}

// ---- K1-03: a tile is never published by a rename ----

func tempFilesBelow(t testing.TB, root string) []string {
	t.Helper()
	var out []string
	_ = filepath.WalkDir(root, func(p string, e fs.DirEntry, err error) error {
		if err == nil && !e.IsDir() && strings.HasPrefix(e.Name(), tempPrefix) {
			out = append(out, p)
		}
		return nil
	})
	return out
}

func TestPutTileFailsClosedWithoutHardLinks(t *testing.T) {
	d := newDirStore(t)
	k := TileKey{0, 0, 2}
	data, _ := EncodeTile([]Hash{{1}, {2}})
	d.link = func(oldname, newname string) error {
		return &os.LinkError{Op: "link", Old: oldname, New: newname, Err: noLinkErrnos[0]}
	}
	d.rename = func(string, string) error {
		t.Error("a tile was published with a rename")
		return errors.New("must not be called")
	}
	err := d.PutTile(bg, k, data)
	if !errors.Is(err, ErrHardLinksRequired) {
		t.Fatalf("PutTile without hard links: %v", err)
	}
	if !strings.Contains(err.Error(), "hard links") {
		t.Errorf("the error does not say what is needed: %v", err)
	}
	if _, err := os.Lstat(d.path(k.Path())); err == nil {
		t.Error("a tile exists although the link failed")
	}
	if tmp := tempFilesBelow(t, d.Root()); len(tmp) != 0 {
		t.Errorf("temporary files are left: %v", tmp)
	}
	// Every other error that means the same, also one that is not wrapped, is
	// the same failure, not a fallback (the classification itself is tested in
	// TestPutTileLinkErrorsAreClassified).
	d.link = func(string, string) error { return noLinkErrnos[len(noLinkErrnos)-1] }
	if err := d.PutTile(bg, k, data); !errors.Is(err, ErrHardLinksRequired) {
		t.Errorf("a refused link: %v", err)
	}
	// Through Append: the log reports it and keeps its state.
	l, _ := Create(bg, testOrigin(1), d)
	if _, err := l.Append(bg, entriesRange(0, 3)); !errors.Is(err, ErrHardLinksRequired) {
		t.Errorf("Append: %v", err)
	}
	if l.Size() != 0 {
		t.Error("a failed Append changed the log")
	}
}

// When the name exists at the link, the content decides, as it always did;
// and a name that vanished is not taken for a published tile.
func TestPutTileExistsAtTheLink(t *testing.T) {
	data, _ := EncodeTile([]Hash{{1}, {2}})
	other, _ := EncodeTile([]Hash{{1}, {3}})
	for name, tc := range map[string]struct {
		put  []byte
		want func(error) bool
	}{
		"same content":      {data, func(err error) bool { return err == nil }},
		"different content": {other, func(err error) bool { return errors.Is(err, ErrConflict) }},
		"vanished":          {nil, func(err error) bool { return err != nil && !errors.Is(err, ErrConflict) }},
	} {
		d := newDirStore(t)
		k := TileKey{0, 0, 2}
		d.link = func(string, string) error {
			if tc.put != nil {
				if err := os.WriteFile(d.path(k.Path()), tc.put, 0o644); err != nil {
					t.Error(err)
				}
			}
			return fs.ErrExist
		}
		d.rename = func(string, string) error { t.Error("rename was called"); return nil }
		// With the flushes on, a tile found at the link (another writer published
		// it, or this is the repeat of a crashed put) is made durable like any
		// other put that returns nil (K2-01).
		d.NoSync = false
		var steps []string
		d.Hook = func(step string) error {
			if strings.HasPrefix(step, "sync-dir ") {
				steps = append(steps, strings.TrimPrefix(step, "sync-dir "))
			}
			return nil
		}
		if err := d.PutTile(bg, k, data); !tc.want(err) {
			t.Errorf("%s: %v", name, err)
		}
		if name == "same content" {
			if want := []string{"tile/0/000.p", "tile/0", "tile", "."}; !reflect.DeepEqual(steps, want) {
				t.Errorf("%s: flushed %v, expected %v", name, steps, want)
			}
		} else if len(steps) != 0 {
			t.Errorf("%s: flushed %v although it failed", name, steps)
		}
		if tmp := tempFilesBelow(t, d.Root()); len(tmp) != 0 {
			t.Errorf("%s: temporary files are left: %v", name, tmp)
		}
	}
}

// ---- K1-04: PutCheckpoint and a blocked rename ----

var errBusy = errors.New("the file is in use")

func TestPutCheckpointRetriesABlockedRename(t *testing.T) {
	d := newDirStore(t)
	if err := d.PutCheckpoint(bg, []byte("old\n")); err != nil {
		t.Fatal(err)
	}
	d.renameBusy = func(err error) bool { return errors.Is(err, errBusy) }
	calls := 0
	d.rename = func(o, n string) error {
		calls++
		if calls <= 3 {
			return errBusy
		}
		return os.Rename(o, n)
	}
	d.CheckpointRetry = 5 * time.Second
	if err := d.PutCheckpoint(bg, []byte("new\n")); err != nil {
		t.Fatalf("a rename that is free at the fourth try: %v", err)
	}
	if calls != 4 {
		t.Errorf("%d rename calls, expected 4", calls)
	}
	if got, _ := d.GetCheckpoint(bg); string(got) != "new\n" {
		t.Errorf("the checkpoint is %q", got)
	}
	// Still held at the end of the time: typed error, old note whole, no
	// temporary file.
	d.rename = func(string, string) error { calls++; return errBusy }
	d.CheckpointRetry = 120 * time.Millisecond
	calls = 0
	start := time.Now()
	err := d.PutCheckpoint(bg, []byte("newer\n"))
	took := time.Since(start)
	if !errors.Is(err, ErrCheckpointBusy) || !strings.Contains(err.Error(), "retry may succeed") {
		t.Fatalf("a rename that stays blocked: %v", err)
	}
	if took < 100*time.Millisecond || took > 3*time.Second {
		t.Errorf("the retry took %v, expected about 120ms", took)
	}
	if calls < 3 {
		t.Errorf("only %d tries in the time", calls)
	}
	if got, _ := d.GetCheckpoint(bg); string(got) != "new\n" {
		t.Errorf("the old checkpoint is %q", got)
	}
	if tmp := tempFilesBelow(t, d.Root()); len(tmp) != 0 {
		t.Errorf("temporary files are left: %v", tmp)
	}
	// A negative time is one try.
	d.CheckpointRetry = -1
	calls = 0
	if err := d.PutCheckpoint(bg, []byte("newer\n")); !errors.Is(err, ErrCheckpointBusy) || calls != 1 {
		t.Errorf("a negative time: %d tries, %v", calls, err)
	}
	// A failure that is not a blocked rename is returned as it is, at once.
	d.CheckpointRetry = 5 * time.Second
	other := errors.New("disk on fire")
	calls = 0
	d.rename = func(string, string) error { calls++; return other }
	if err := d.PutCheckpoint(bg, []byte("newer\n")); !errors.Is(err, other) || errors.Is(err, ErrCheckpointBusy) || calls != 1 {
		t.Errorf("another failure: %d tries, %v", calls, err)
	}
	if tmp := tempFilesBelow(t, d.Root()); len(tmp) != 0 {
		t.Errorf("temporary files are left: %v", tmp)
	}
}

// With CheckpointRetry zero the time is DefaultCheckpointRetry, 3 seconds.
// It runs on a virtual clock (the retry's own seams), so the default run does
// not wait: the total of the waits is the time the store is willing to wait.
func TestPutCheckpointDefaultRetryIsThreeSeconds(t *testing.T) {
	for _, tc := range []struct {
		name  string
		retry time.Duration
		want  time.Duration // the most the waits add up to
	}{
		{"zero is the default of 3 seconds, the documented value", 0, 3 * time.Second},
		{"a given time is used", 700 * time.Millisecond, 700 * time.Millisecond},
		{"a negative time is no wait", -1, 0},
	} {
		d := newDirStore(t)
		d.CheckpointRetry = tc.retry
		d.renameBusy = func(err error) bool { return errors.Is(err, errBusy) }
		d.rename = func(string, string) error { return errBusy }
		clock := time.Unix(1_000_000, 0)
		var slept time.Duration
		d.now = func() time.Time { return clock }
		d.pause = func(_ context.Context, w time.Duration) error {
			slept += w
			clock = clock.Add(w)
			return nil
		}
		err := d.PutCheckpoint(bg, []byte("a\n"))
		if !errors.Is(err, ErrCheckpointBusy) {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if slept != tc.want {
			t.Errorf("%s: it waited %v in all, expected %v", tc.name, slept, tc.want)
		}
	}
	// And a rename that becomes free inside the default time succeeds.
	d := newDirStore(t)
	d.renameBusy = func(err error) bool { return errors.Is(err, errBusy) }
	clock := time.Unix(1_000_000, 0)
	start := clock
	d.now = func() time.Time { return clock }
	d.pause = func(_ context.Context, w time.Duration) error { clock = clock.Add(w); return nil }
	d.rename = func(o, n string) error {
		if clock.Sub(start) < 2200*time.Millisecond {
			return errBusy
		}
		return os.Rename(o, n)
	}
	if err := d.PutCheckpoint(bg, []byte("a\n")); err != nil {
		t.Fatalf("a rename that is free after 2.2 s, with the default time: %v", err)
	}
}

// Held for good, PutCheckpoint gives up after the default time, about 3 seconds.
func TestPutCheckpointGivesUpAfterTheDefaultRetry(t *testing.T) {
	skipUnlessLong(t, "the give-up after the default 3 seconds")
	d := newDirStore(t)
	d.renameBusy = func(err error) bool { return errors.Is(err, errBusy) }
	d.rename = func(string, string) error { return errBusy }
	start := time.Now()
	err := d.PutCheckpoint(bg, []byte("b\n"))
	took := time.Since(start)
	if !errors.Is(err, ErrCheckpointBusy) || took < 2900*time.Millisecond || took > 6*time.Second {
		t.Errorf("held for good: %v after %v", err, took)
	}
}

// Off Windows a rename replaces a file that is open, at once.
func TestPutCheckpointWhileOpenOffWindows(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("see the Windows test")
	}
	d := newDirStore(t)
	if err := d.PutCheckpoint(bg, []byte("old\n")); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(d.path("checkpoint"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	start := time.Now()
	if err := d.PutCheckpoint(bg, []byte("new\n")); err != nil {
		t.Fatal(err)
	}
	if time.Since(start) > time.Second {
		t.Error("the replacement waited")
	}
}

// ---- K1-05: the parent of every new directory is flushed, in order ----

func TestPutFlushesNewDirectoriesUpToTheRoot(t *testing.T) {
	d, err := NewDirStore(filepath.Join(t.TempDir(), "log"))
	if err != nil {
		t.Fatal(err)
	}
	var steps []string
	d.Hook = func(step string) error {
		if strings.HasPrefix(step, "sync-dir ") {
			steps = append(steps, strings.TrimPrefix(step, "sync-dir "))
		}
		return nil
	}
	put := func(k TileKey) []string {
		steps = nil
		var data []byte
		if k.Level == EntryLevel {
			data, _ = EncodeBundle(entriesRange(0, k.Width))
		} else {
			data, _ = EncodeTile(leafHashesOf(k.Width))
		}
		if err := d.PutTile(bg, k, data); err != nil {
			t.Fatal(err)
		}
		return steps
	}
	// The first tile of a store: tile, tile/0 and tile/0/x001 are new.
	got := put(TileKey{0, 1234, 3})
	want := []string{"tile/0/x001/234.p", "tile/0/x001", "tile/0", "tile", "."}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("the first put flushes %v, expected %v", got, want)
	}
	// A tile whose partial-tile directory is new, in a group directory that exists.
	if got := put(TileKey{0, 1235, 3}); !reflect.DeepEqual(got, []string{"tile/0/x001/235.p", "tile/0/x001"}) {
		t.Errorf("a put in an existing directory flushes %v", got)
	}
	// A new directory below an old one: its parent, up to the old one.
	if got := put(TileKey{0, 1, 5}); !reflect.DeepEqual(got, []string{"tile/0/001.p", "tile/0"}) {
		t.Errorf("a put with one new directory flushes %v", got)
	}
	// A bundle: tile/entries is new.
	if got := put(TileKey{EntryLevel, 0, TileWidth}); !reflect.DeepEqual(got, []string{"tile/entries", "tile"}) {
		t.Errorf("a full bundle flushes %v", got)
	}
	// A repeated put writes nothing, but a put that returns nil is durable (the
	// repeat after a crash between the link and the flushes is exactly the
	// crash procedure): it flushes the directory of the tile and every ancestor
	// up to the root (K2-01).
	if got := put(TileKey{0, 1, 5}); !reflect.DeepEqual(got, []string{"tile/0/001.p", "tile/0", "tile", "."}) {
		t.Errorf("a repeated put flushes %v", got)
	}
	if got := put(TileKey{0, 1234, 3}); !reflect.DeepEqual(got, []string{"tile/0/x001/234.p", "tile/0/x001", "tile/0", "tile", "."}) {
		t.Errorf("a repeated put of a deep tile flushes %v", got)
	}
	// The checkpoint: the root.
	steps = nil
	if err := d.PutCheckpoint(bg, []byte("note\n")); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(steps, []string{"."}) {
		t.Errorf("the checkpoint flushes %v", steps)
	}
	// A root that was removed: the checkpoint makes it again and flushes its parent too.
	if err := os.RemoveAll(d.Root()); err != nil {
		t.Fatal(err)
	}
	steps = nil
	if err := d.PutCheckpoint(bg, []byte("note\n")); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(steps, []string{".", ".."}) {
		t.Errorf("the checkpoint of a new root flushes %v", steps)
	}
	// NoSync flushes nothing.
	d.NoSync = true
	steps = nil
	put(TileKey{3, 5, 2})
	if len(steps) != 0 {
		t.Errorf("NoSync flushed %v", steps)
	}
}

// A crash at each flush leaves what the crash procedure of the doc repairs:
// the put reports the error, and a second put of the same tile finishes it.
func TestCrashAtEachFlushOfAPut(t *testing.T) {
	k := TileKey{0, 1234, 3}
	data, _ := EncodeTile(leafHashesOf(3))
	for crashAt := 1; crashAt <= 5; crashAt++ {
		d, err := NewDirStore(filepath.Join(t.TempDir(), "log"))
		if err != nil {
			t.Fatal(err)
		}
		n := 0
		d.Hook = func(step string) error {
			if strings.HasPrefix(step, "sync-dir ") {
				n++
				if n == crashAt {
					return errCrash
				}
			}
			return nil
		}
		if err := d.PutTile(bg, k, data); !errors.Is(err, errCrash) {
			t.Fatalf("crash at flush %d: %v", crashAt, err)
		}
		// The repeat after the crash is what makes the tile durable: it flushes
		// every directory up to the root again, not only the ones the crashed
		// put had left (K2-01).
		var steps []string
		d.Hook = func(step string) error {
			if strings.HasPrefix(step, "sync-dir ") {
				steps = append(steps, strings.TrimPrefix(step, "sync-dir "))
			}
			return nil
		}
		if err := d.PutTile(bg, k, data); err != nil {
			t.Fatalf("the put after the crash at flush %d: %v", crashAt, err)
		}
		if want := []string{"tile/0/x001/234.p", "tile/0/x001", "tile/0", "tile", "."}; !reflect.DeepEqual(steps, want) {
			t.Fatalf("the put after the crash at flush %d flushed %v, expected %v", crashAt, steps, want)
		}
		d.Hook = nil
		if got, err := d.GetTile(bg, k); err != nil || !bytes.Equal(got, data) {
			t.Fatalf("the tile after the crash at flush %d: %v", crashAt, err)
		}
	}
}

// ---- K1-10: bounds ----

func TestRequiredIsBounded(t *testing.T) {
	// The refusal is tested through the inner function with a small bound, and
	// only with sizes that stay small when the check is removed: a mutant that
	// deletes the check must fail this test, not allocate (size/256 keys of 24
	// bytes: the first version of this test gave 2^40 and 2^62 to the public
	// function, and a removed bound took the machine down). The huge sizes are
	// refused by the same single comparison; they are not given to code that
	// could allocate for them.
	const small = 1 << 12
	for _, size := range []uint64{small + 1, small + 256, 1 << 16} {
		keys, err := required(size, small)
		if !errors.Is(err, ErrTooLarge) || keys != nil {
			t.Errorf("size %d, bound %d: %d keys, %v", size, small, len(keys), err)
		}
	}
	// At the bound: 16 full bundles, 16 full tiles of level 0, and one partial
	// tile of level 1 with 16 hashes.
	keys, err := required(small, small)
	if err != nil || len(keys) != 16+16+1 {
		t.Errorf("a size at the bound: %d keys, %v", len(keys), err)
	}
	// The public function: the documented bound, and the first size above it
	// (with the check removed that is a list of 50 MB, no more).
	if MaxRequiredSize != 1<<28 {
		t.Errorf("the documented bound is 2^28, not %d", MaxRequiredSize)
	}
	if _, err := Required(MaxRequiredSize + 1); !errors.Is(err, ErrTooLarge) {
		t.Errorf("a size above the bound: %v", err)
	}
	keys, err = Required(MaxRequiredSize)
	if err != nil {
		t.Fatalf("the largest size: %v", err)
	}
	if len(keys) != 2101265 || cap(keys) != len(keys) { // counted first: one allocation of 50 MB, not a doubling slice
		t.Errorf("the largest size lists %d keys in a slice of capacity %d", len(keys), cap(keys))
	}
	if _, err := Required(0); err != nil {
		t.Error(err)
	}
}

func TestReadRegularRefusesAFileSwappedAfterTheStat(t *testing.T) {
	d := newDirStore(t)
	k := TileKey{0, 0, 2}
	good, _ := EncodeTile([]Hash{{1}, {2}})
	evil, _ := EncodeTile([]Hash{{9}, {9}})
	if err := d.PutTile(bg, k, good); err != nil {
		t.Fatal(err)
	}
	swap := filepath.Join(d.Root(), "swap")
	if err := os.WriteFile(swap, evil, 0o644); err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS == "windows" {
		t.Skip("on Windows os.SameFile finds the identity of an Lstat result by its path, at the time of the call, so it cannot see a swap of one regular file for another")
	}
	done := false
	d.afterStat = func(path string) {
		if !done {
			done = true
			if err := os.Rename(swap, path); err != nil {
				t.Errorf("swap: %v", err)
			}
		}
	}
	if got, err := d.GetTile(bg, k); err == nil {
		t.Fatalf("the swapped file was read: %x", got[:4])
	} else if !strings.Contains(err.Error(), "changed while it was opened") {
		t.Errorf("the error is %v", err)
	}
}

func TestLoadFrontierFileIsLimitedAndSameFile(t *testing.T) {
	dir := t.TempDir()
	l, _ := Create(bg, testOrigin(1), NewMemStore())
	a, _ := l.Append(bg, entriesRange(0, 5))
	path := filepath.Join(dir, "frontier")
	f := l.Frontier()
	if err := SaveFrontierFile(path, f); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadFrontierFile(path, a.Size, a.Root); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { frontierAfterStat = nil })
	// The file grows between the Lstat and the read: only the limit is read
	// and the file is refused.
	frontierAfterStat = func(p string) {
		w, err := os.OpenFile(p, os.O_WRONLY|os.O_APPEND, 0)
		if err != nil {
			t.Error(err)
			return
		}
		defer w.Close()
		if _, err := w.Write(make([]byte, 4<<20)); err != nil {
			t.Error(err)
		}
	}
	_, err := LoadFrontierFile(path, a.Size, a.Root)
	if err == nil || !strings.Contains(err.Error(), "more than") {
		t.Fatalf("a file that grew: %v", err)
	}
	frontierAfterStat = nil
	// A swap for another file.
	if err := SaveFrontierFile(path, f); err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS == "windows" {
		return // see TestReadRegularRefusesAFileSwappedAfterTheStat
	}
	other := filepath.Join(dir, "other")
	if err := SaveFrontierFile(other, f); err != nil {
		t.Fatal(err)
	}
	done := false
	frontierAfterStat = func(p string) {
		if !done {
			done = true
			if err := os.Rename(other, p); err != nil {
				t.Error(err)
			}
		}
	}
	if _, err := LoadFrontierFile(path, a.Size, a.Root); err == nil || !strings.Contains(err.Error(), "changed while it was opened") {
		t.Errorf("a swapped frontier file: %v", err)
	}
}

// ---- K1-12: the surviving mutants ----

// aheadStore returns a store with a log of n leaves, and the store of a larger
// log from which single tiles are copied.
func aheadFixture(t *testing.T, n, more int) (store *MemStore, a Appended, bigger *MemStore) {
	t.Helper()
	store = NewMemStore()
	l, _ := Create(bg, testOrigin(1), store)
	a = mustAppend(t, l, 0, n)
	bigger = NewMemStore()
	lb, _ := Create(bg, testOrigin(1), bigger)
	mustAppend(t, lb, 0, more)
	return store, a, bigger
}

func copyTile(t *testing.T, from, to *MemStore, k TileKey) {
	t.Helper()
	d, err := from.GetTile(bg, k)
	if err != nil {
		t.Fatalf("%s: %v", k.Path(), err)
	}
	if err := to.PutTile(bg, k, d); err != nil {
		t.Fatal(err)
	}
}

func TestOpenSeesTilesAheadOfTheFrontier(t *testing.T) {
	open := func(s Store, a Appended, allow bool) error {
		_, err := Open(bg, testOrigin(1), s, a.Frontier, a.Size, a.Root, Options{AllowTilesAhead: allow})
		return err
	}
	for _, tc := range []struct {
		name string
		n    int // leaves in the log
		more int // leaves in the log that the tile is copied from
		keys []TileKey
	}{
		{"a level 0 tile one entry ahead", 10, 11, []TileKey{{0, 0, 11}}},
		{"an entry bundle one entry ahead", 10, 11, []TileKey{{EntryLevel, 0, 11}}},
		{"the next level 0 tile only", 10, 300, []TileKey{{0, 1, 44}}},
		{"the next entry bundle only", 10, 300, []TileKey{{EntryLevel, 1, 44}}},
		{"a full next tile", 10, 600, []TileKey{{0, 1, TileWidth}}},
		{"a tile at a full boundary", 256, 257, []TileKey{{0, 1, 1}}},
		{"a bundle at a full boundary", 256, 257, []TileKey{{EntryLevel, 1, 1}}},
		{"the tile that completes a partial one", 10, 256, []TileKey{{0, 0, TileWidth}}},
	} {
		store, a, bigger := aheadFixture(t, tc.n, tc.more)
		if err := open(store, a, false); err != nil {
			t.Fatalf("%s: the honest open: %v", tc.name, err)
		}
		for _, k := range tc.keys {
			copyTile(t, bigger, store, k)
		}
		if err := open(store, a, false); !errors.Is(err, ErrTilesAhead) {
			t.Errorf("%s: Open gives %v, expected ErrTilesAhead", tc.name, err)
		}
		if err := open(store, a, true); err != nil {
			t.Errorf("%s: Open with AllowTilesAhead: %v", tc.name, err)
		}
	}
	// A tile of the same width as the frontier's, or narrower, is what a
	// log has and not ahead.
	store, a, bigger := aheadFixture(t, 10, 10)
	copyTile(t, bigger, store, TileKey{0, 0, 10})
	if err := open(store, a, false); err != nil {
		t.Errorf("an identical tile: %v", err)
	}
}

// A crash inside the Append of one entry: the bundle is stored (the first
// put), the tile is not: Open reports the tiles ahead, and the same leaf again
// repairs it.
func TestCrashAfterAOneEntryAppend(t *testing.T) {
	store := NewMemStore()
	l, _ := Create(bg, testOrigin(1), store)
	a := mustAppend(t, l, 0, 10)
	for crashAt := 1; crashAt <= 3; crashAt++ {
		s := NewMemStore()
		lg, _ := Create(bg, testOrigin(1), s)
		mustAppend(t, lg, 0, 10)
		n := 0
		s.BeforePut = func(TileKey) error {
			n++
			if n == crashAt {
				return errCrash
			}
			return nil
		}
		_, err := lg.Append(bg, entriesRange(10, 11))
		if crashAt <= 2 && !errors.Is(err, errCrash) {
			t.Fatalf("crash %d: %v", crashAt, err)
		}
		if crashAt == 3 && err != nil {
			t.Fatalf("no crash expected: %v", err)
		}
		s.BeforePut = nil
		_, oerr := Open(bg, testOrigin(1), s, a.Frontier, a.Size, a.Root, Options{})
		if crashAt == 1 {
			if oerr != nil {
				t.Errorf("crash before any put: %v", oerr)
			}
			continue
		}
		if !errors.Is(oerr, ErrTilesAhead) {
			t.Errorf("crash %d: Open gives %v, expected ErrTilesAhead", crashAt, oerr)
			continue
		}
		l2, err := Open(bg, testOrigin(1), s, a.Frontier, a.Size, a.Root, Options{AllowTilesAhead: true})
		if err != nil {
			t.Fatal(err)
		}
		b, err := l2.Append(bg, entriesRange(10, 11))
		if err != nil || b.Size != 11 {
			t.Errorf("crash %d: the repair: %v", crashAt, err)
		}
	}
}

func TestParseTilePathOverflowGuard(t *testing.T) {
	max := "tile/0/x018/x446/x744/x073/x709/x551/615" // 2^64 - 1
	k, err := ParseTilePath(max)
	if err != nil || k.Index != ^uint64(0) || k.Path() != max {
		t.Fatalf("the largest index: %+v %v", k, err)
	}
	for _, p := range []string{
		"tile/0/x018/x446/x744/x073/x709/x551/616", // 2^64
		"tile/0/x018/x446/x744/x073/x709/x551/999",
		"tile/0/x018/x446/x744/x073/x709/x552/000",
		"tile/0/x019/x000/x000/x000/x000/x000/000",
		"tile/0/x999/x999/x999/x999/x999/x999/999",
		"tile/0/x100/x000/x000/x000/x000/x000/000",
		"tile/entries/x018/x446/x744/x073/x709/x551/616",
	} {
		if _, err := ParseTilePath(p); !errors.Is(err, ErrBadPath) {
			t.Errorf("%s: %v", p, err)
		}
	}
	// One below the edge still parses, in each of the two places the guard
	// looks at.
	for _, p := range []string{"tile/0/x018/x446/x744/x073/x709/x551/614", "tile/0/x018/x446/x744/x073/x709/x550/999"} {
		if k, err := ParseTilePath(p); err != nil || k.Path() != p {
			t.Errorf("%s: %+v %v", p, k, err)
		}
	}
}

// A symbolic link on the way is refused wherever it is: as a directory that
// is not the last element of the path (tile/0), and as the last element (the
// directory of the partial tiles), where only the link check can see it.
func TestDirStoreRefusesLinkedDirectories(t *testing.T) {
	good, _ := EncodeTile([]Hash{{1}, {2}})
	for _, tc := range []struct {
		name  string
		link  string // the path below the root that is made a link
		key   TileKey
		check func(t *testing.T, d *DirStore, elsewhere string)
	}{
		{"the level directory", "tile/0", TileKey{0, 0, 2}, nil},
		{"the directory of partial tiles", "tile/0/000.p", TileKey{0, 0, 2}, func(t *testing.T, d *DirStore, elsewhere string) {
			if _, err := d.Widths(bg, 0, 0); err == nil {
				t.Error("Widths listed a linked directory")
			}
		}},
		{"the entries directory", "tile/entries", TileKey{EntryLevel, 0, TileWidth}, nil},
		{"the directory of an index group", "tile/0/x001", TileKey{0, 1000, 2}, nil},
	} {
		d := newDirStore(t)
		elsewhere := t.TempDir()
		if err := os.MkdirAll(filepath.Dir(d.path(tc.link)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(elsewhere, d.path(tc.link)); err != nil {
			t.Skipf("no symbolic links here: %v", err)
		}
		data := good
		if tc.key.Level == EntryLevel {
			data, _ = EncodeBundle(entriesRange(0, tc.key.Width))
		} else if tc.key.Width == 2 {
			data = good
		}
		if err := d.PutTile(bg, tc.key, data); err == nil {
			t.Errorf("%s: a tile was written through a link", tc.name)
		} else if !strings.Contains(err.Error(), "link") && !strings.Contains(err.Error(), "not a directory") {
			t.Logf("%s: %v", tc.name, err)
		}
		if ents, _ := os.ReadDir(elsewhere); len(ents) != 0 {
			t.Errorf("%s: something was written through the link: %v", tc.name, ents)
		}
		if _, err := d.GetTile(bg, tc.key); err == nil || errors.Is(err, ErrNotFound) {
			t.Errorf("%s: a tile was looked up through a link: %v", tc.name, err)
		}
		// The same content in the linked directory is not read, either.
		if tc.key.Level != EntryLevel {
			if err := os.WriteFile(filepath.Join(elsewhere, "1"), good, 0o644); err != nil {
				t.Fatal(err)
			}
			if _, err := d.GetTile(bg, tc.key); err == nil {
				t.Errorf("%s: a tile behind a link was read", tc.name)
			}
		}
		if tc.check != nil {
			tc.check(t, d, elsewhere)
		}
	}
}

// ---- concurrency: the put flush and checkpoint hooks are safe under -race ----

func TestDirStoreConcurrentPutsWithFlushes(t *testing.T) {
	d, err := NewDirStore(filepath.Join(t.TempDir(), "log"))
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	steps := 0
	d.Hook = func(string) error { mu.Lock(); steps++; mu.Unlock(); return nil }
	var wg sync.WaitGroup
	rng := rand.New(rand.NewSource(1))
	_ = rng
	for g := 0; g < 4; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 5; i++ {
				data, _ := EncodeTile(leafHashesOf(3))
				if err := d.PutTile(bg, TileKey{g, uint64(i * 700), 3}, data); err != nil {
					t.Error(err)
				}
			}
		}(g)
	}
	wg.Wait()
	if steps == 0 {
		t.Error("no hook step")
	}
}

// FuzzCheckpointTextThenParse: whatever Text accepts, ParseCheckpoint accepts
// and returns equal.
func FuzzCheckpointTextThenParse(f *testing.F) {
	f.Add("a/b", uint64(5), "x\ny", uint8(3))
	f.Add(testOrigin(1), uint64(0), "", uint8(0))
	f.Add("o", uint64(1<<62), "line", uint8(20))
	f.Fuzz(func(t *testing.T, origin string, size uint64, ext string, n uint8) {
		c := Checkpoint{Origin: origin, Size: size, Root: LeafHash([]byte("r"))}
		if size == 0 {
			c.Root = EmptyRoot()
		}
		for i := 0; i < int(n%24); i++ {
			c.Extensions = append(c.Extensions, ext)
		}
		text, err := c.Text()
		if err != nil {
			return
		}
		back, err := ParseCheckpoint(text)
		if err != nil {
			t.Fatalf("Text accepted %+v and ParseCheckpoint refuses it: %v", c, err)
		}
		if len(c.Extensions) == 0 {
			c.Extensions = nil
		}
		if !reflect.DeepEqual(back, c) {
			t.Fatalf("Text then ParseCheckpoint gives %+v for %+v", back, c)
		}
	})
}
