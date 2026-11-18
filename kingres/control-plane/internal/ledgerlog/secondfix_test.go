// SPDX-License-Identifier: AGPL-3.0-only

package ledgerlog

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/ledgerkeys"
)

// The tests of this file are the checks that the second round of fixes needs
// (the numbering is that of the second review: K2-nn).

// ---- K2-07: only "no hard links here" is ErrHardLinksRequired ----

func TestPutTileLinkErrorsAreClassified(t *testing.T) {
	k := TileKey{0, 0, 2}
	data, _ := EncodeTile([]Hash{{1}, {2}})
	for _, tc := range []struct {
		name    string
		errs    []error
		noLinks bool
	}{{"no hard links", noLinkErrnos, true}, {"another failure", otherLinkErrnos, false}} {
		for _, cause := range tc.errs {
			d := newDirStore(t)
			d.link = func(o, n string) error { return &os.LinkError{Op: "link", Old: o, New: n, Err: cause} }
			d.rename = func(string, string) error { t.Error("rename was called"); return nil }
			err := d.PutTile(bg, k, data)
			if err == nil {
				t.Fatalf("%s %v: no error", tc.name, cause)
			}
			if got := errors.Is(err, ErrHardLinksRequired); got != tc.noLinks {
				t.Errorf("%s %v: ErrHardLinksRequired=%v: %v", tc.name, cause, got, err)
			}
			if !errors.Is(err, cause) {
				t.Errorf("%s %v: errors.Is does not find the cause: %v", tc.name, cause, err)
			}
			if tc.noLinks && !strings.Contains(err.Error(), "hard links") {
				t.Errorf("%s %v: the message does not say what is needed: %v", tc.name, cause, err)
			}
			if !tc.noLinks && strings.Contains(err.Error(), "needs a file system with hard links") {
				t.Errorf("%s %v: a failure of this call is called a missing feature: %v", tc.name, cause, err)
			}
			if _, err := os.Lstat(d.path(k.Path())); err == nil {
				t.Errorf("%s %v: a tile exists", tc.name, cause)
			}
			if tmp := tempFilesBelow(t, d.Root()); len(tmp) != 0 {
				t.Errorf("%s %v: temporary files are left: %v", tc.name, cause, tmp)
			}
		}
	}
	// A permission error is not a missing feature either, and stays a permission error.
	d := newDirStore(t)
	d.link = func(string, string) error { return fs.ErrPermission }
	if err := d.PutTile(bg, k, data); errors.Is(err, ErrHardLinksRequired) || !errors.Is(err, fs.ErrPermission) {
		t.Errorf("a refused permission: %v", err)
	}
}

// ---- K2-08: a target that waiting cannot cure is named at once ----

// With the rename failing as a blocked one (the seam says so), a directory in
// the place of the checkpoint is told apart before any wait. This is the same
// check for every system; TestPutCheckpointTargetThatWaitingCannotCure does it
// with real files on Windows.
func TestPutCheckpointDirectoryAtTheTargetIsNotBusy(t *testing.T) {
	d := newDirStore(t)
	if err := os.MkdirAll(d.path("checkpoint"), 0o755); err != nil {
		t.Fatal(err)
	}
	d.renameBusy = func(error) bool { return true }
	d.rename = func(string, string) error { return errBusy }
	d.CheckpointRetry = 2 * time.Second // short, so that a check that is removed fails the test and does not wait
	waited := false
	d.pause = func(context.Context, time.Duration) error {
		waited = true
		time.Sleep(50 * time.Millisecond)
		return nil
	}
	err := d.PutCheckpoint(bg, []byte("new\n"))
	if err == nil || errors.Is(err, ErrCheckpointBusy) || !strings.Contains(err.Error(), "directory") {
		t.Fatalf("a directory in the place of the checkpoint: %v", err)
	}
	if waited {
		t.Error("it waited for a directory to go away")
	}
	if tmp := tempFilesBelow(t, d.Root()); len(tmp) != 0 {
		t.Errorf("temporary files are left: %v", tmp)
	}
	// A plain file that is held stays busy: the wait happens.
	d2 := newDirStore(t)
	if err := d2.PutCheckpoint(bg, []byte("old\n")); err != nil {
		t.Fatal(err)
	}
	d2.renameBusy = func(error) bool { return true }
	d2.rename = func(string, string) error { return errBusy }
	d2.CheckpointRetry = 50 * time.Millisecond
	if err := d2.PutCheckpoint(bg, []byte("new\n")); !errors.Is(err, ErrCheckpointBusy) {
		t.Errorf("a held file: %v", err)
	}
}

// ---- K2-09: the context is honoured ----

func TestContextIsHonoured(t *testing.T) {
	cancelled, cancel := context.WithCancel(bg)
	cancel()
	k := TileKey{0, 0, 2}
	data, _ := EncodeTile([]Hash{{1}, {2}})
	d := newDirStore(t)
	for name, s := range map[string]interface {
		Store
		CheckpointStore
	}{"DirStore": d, "MemStore": NewMemStore()} {
		if err := s.PutTile(cancelled, k, data); !errors.Is(err, context.Canceled) {
			t.Errorf("%s PutTile: %v", name, err)
		}
		if _, err := s.GetTile(cancelled, k); !errors.Is(err, context.Canceled) {
			t.Errorf("%s GetTile: %v", name, err)
		}
		if _, err := s.Widths(cancelled, 0, 0); !errors.Is(err, context.Canceled) {
			t.Errorf("%s Widths: %v", name, err)
		}
		if err := s.PutCheckpoint(cancelled, []byte("x\n")); !errors.Is(err, context.Canceled) {
			t.Errorf("%s PutCheckpoint: %v", name, err)
		}
		if _, err := s.GetCheckpoint(cancelled); !errors.Is(err, context.Canceled) {
			t.Errorf("%s GetCheckpoint: %v", name, err)
		}
		// Nothing was written.
		if _, err := s.GetTile(bg, k); !errors.Is(err, ErrNotFound) {
			t.Errorf("%s: a cancelled put wrote the tile: %v", name, err)
		}
	}
	if _, err := os.Lstat(d.path("checkpoint")); err == nil {
		t.Error("a cancelled PutCheckpoint wrote the checkpoint")
	}
	// The wait of the retry ends with the context, and leaves nothing behind.
	d3 := newDirStore(t)
	d3.renameBusy = func(error) bool { return true }
	d3.rename = func(string, string) error { return errBusy }
	d3.CheckpointRetry = 2 * time.Second // short, so that a wait that ignores the context ends the test
	ctx, stop := context.WithTimeout(bg, 80*time.Millisecond)
	defer stop()
	start := time.Now()
	err := d3.PutCheckpoint(ctx, []byte("new\n"))
	if !errors.Is(err, context.DeadlineExceeded) || errors.Is(err, ErrCheckpointBusy) {
		t.Errorf("a wait cut by the context: %v", err)
	}
	if time.Since(start) > 10*time.Second {
		t.Errorf("the wait took %v", time.Since(start))
	}
	if tmp := tempFilesBelow(t, d3.Root()); len(tmp) != 0 {
		t.Errorf("temporary files are left: %v", tmp)
	}
	// Audit looks at the context at least once per tile, also when the getter
	// does not: it stops within a tile of the cancellation.
	mem := NewMemStore()
	l, _ := Create(bg, testOrigin(3), mem)
	a := mustAppend(t, l, 0, 1100)
	actx, acancel := context.WithCancel(bg)
	defer acancel()
	calls, after := 0, 0
	getter := getterFunc(func(_ context.Context, k TileKey) ([]byte, error) { // ignores its context
		calls++
		if calls == 3 {
			acancel()
		} else if calls > 3 {
			after++
		}
		return mem.GetTile(bg, k)
	})
	if err := Audit(actx, getter, a.Size, a.Root); !errors.Is(err, context.Canceled) {
		t.Fatalf("Audit with a context cancelled in the middle: %v", err)
	}
	if after > 3 {
		t.Errorf("Audit read %d tiles after the cancellation", after)
	}
	if err := Audit(cancelled, mem, a.Size, a.Root); !errors.Is(err, context.Canceled) {
		t.Errorf("Audit with a cancelled context: %v", err)
	}
	if err := Audit(bg, mem, a.Size, a.Root); err != nil {
		t.Errorf("Audit: %v", err)
	}
}

// ---- K2-03: ResignCheckpoint and the keys the note does not show ----

func TestResignCheckpointChecksEveryTrustedKey(t *testing.T) {
	a, b := collisionSigners(t) // two keys with one key id under collisionOrigin()
	signer := testKey(t, 1)
	c := collisionCheckpoint()
	note, err := SignCheckpoint(bg, c, signer)
	if err != nil {
		t.Fatal(err)
	}
	// a is a trusted key with no line in the note (it is in the trust file);
	// b, the new signer, has a's key id. The verifier would refuse the result.
	trusted := []ed25519.PublicKey{signer.Public(), a.Public()}
	out, err := ResignCheckpoint(bg, note, trusted, b)
	wantCollision(t, "a silent trusted key", err, a, b, true)
	if out != nil {
		t.Error("a note came back with the error")
	}
	// Two new signers that collide with each other, neither of them trusted.
	_, err = ResignCheckpoint(bg, note, []ed25519.PublicKey{signer.Public()}, a, b)
	wantCollision(t, "two new signers", err, a, b, true)
	// Two trusted keys that collide.
	_, err = ResignCheckpoint(bg, note, []ed25519.PublicKey{signer.Public(), a.Public(), b.Public()}, testKey(t, 2))
	wantCollision(t, "two trusted keys", err, a, b, true)
	// Without the silent key in trusted nothing is wrong that the function can
	// see: this is why trusted must hold every key of the trust file.
	if _, err := ResignCheckpoint(bg, note, []ed25519.PublicKey{signer.Public()}, b); err != nil {
		t.Errorf("a new signer that collides with nothing the caller named: %v", err)
	}
	// A new signer that is a trusted key without a line is fine.
	two, err := ResignCheckpoint(bg, note, []ed25519.PublicKey{signer.Public(), b.Public()}, b)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := VerifyCheckpoint(two, c.Origin, signer.Public(), b.Public()); err != nil {
		t.Errorf("the re-signed note: %v", err)
	}
}

func TestResignCheckpointRefusesALineOfNoTrustedKey(t *testing.T) {
	k1, k2 := testKey(t, 1), testKey(t, 2)
	c := goodCheckpoint()
	note, _ := SignCheckpoint(bg, c, k1)
	n, err := ParseNote(note)
	if err != nil {
		t.Fatal(err)
	}
	// A forged line under the name of the log and a key id that is nobody's.
	forgedID := ledgerkeys.KeyID(c.Origin, testKey(t, 80).Public())
	n.Signatures = append(n.Signatures, NoteSignature{Name: c.Origin, ID: forgedID, Sig: bytes.Repeat([]byte{7}, ed25519.SignatureSize)})
	forged, err := n.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	out, err := ResignCheckpoint(bg, forged, []ed25519.PublicKey{k1.Public()}, k2)
	if !errors.Is(err, ErrUntrustedLine) || out != nil {
		t.Fatalf("a forged line under an untrusted key id: %v", err)
	}
	// With that key in trusted the line is a line of a trusted key that does not
	// verify, and the note is refused as before.
	if _, err := ResignCheckpoint(bg, forged, []ed25519.PublicKey{k1.Public(), testKey(t, 80).Public()}, k2); !errors.Is(err, ErrBadSignature) {
		t.Errorf("a forged line of a trusted key: %v", err)
	}
	// A line under another name (the cosignature of a witness) is not under
	// the name of the log: it is kept, as it is.
	n2, _ := ParseNote(note)
	n2.Signatures = append(n2.Signatures, NoteSignature{Name: "witness.example/w1", ID: 5, Sig: bytes.Repeat([]byte{9}, ed25519.SignatureSize)})
	withWitness, err := n2.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	out, err = ResignCheckpoint(bg, withWitness, []ed25519.PublicKey{k1.Public()}, k2)
	if err != nil {
		t.Fatal(err)
	}
	got, err := ParseNote(out)
	if err != nil || len(got.Signatures) != 3 || got.Signatures[1].Name != "witness.example/w1" {
		t.Errorf("the lines of the result: %+v, %v", got, err)
	}
}

// ---- K2-12: what the first round's tests did not see ----

// A put that crashed (a hook error) leaves its temporary file, as a crash does;
// one that failed for another reason does not.
func TestCrashLeavesTheTemporaryFileAndAFailureDoesNot(t *testing.T) {
	k := TileKey{0, 0, 2}
	data, _ := EncodeTile([]Hash{{1}, {2}})
	for _, step := range []string{"temp-written", "before-publish", "after-publish"} {
		d := newDirStore(t)
		d.Hook = func(s string) error {
			if s == step {
				return errCrash
			}
			return nil
		}
		if err := d.PutTile(bg, k, data); !errors.Is(err, errCrash) {
			t.Fatalf("%s: %v", step, err)
		}
		if tmp := tempFilesBelow(t, d.Root()); len(tmp) != 1 {
			t.Errorf("a crash at %s left %d temporary files, expected 1: %v", step, len(tmp), tmp)
		}
		if n, err := d.RemoveTemporaryFiles(); err != nil || n != 1 {
			t.Errorf("RemoveTemporaryFiles after %s: %d, %v", step, n, err)
		}
	}
	d := newDirStore(t)
	d.link = func(string, string) error { return errors.New("disk on fire") }
	if err := d.PutTile(bg, k, data); err == nil {
		t.Fatal("no error")
	}
	if tmp := tempFilesBelow(t, d.Root()); len(tmp) != 0 {
		t.Errorf("a failed put left %v", tmp)
	}
}

// A file that grows between the Lstat that checked its size and the read is
// read only up to the limit (the hook sees how much was read: an unlimited
// read of the grown file would show more, and would not show at all when the
// limited reader is replaced by io.ReadAll).
func TestReadersAreLimitedWhenAFileGrows(t *testing.T) {
	var reads []int64
	readHook = func(n int64) { reads = append(reads, n) }
	t.Cleanup(func() { readHook = nil; frontierAfterStat = nil })
	grow := func(p string) { // by 100 bytes more than any limit here, so more is read than the limit unless the reader is limited
		w, err := os.OpenFile(p, os.O_WRONLY|os.O_APPEND, 0)
		if err != nil {
			t.Error(err)
			return
		}
		defer w.Close()
		if _, err := w.Write(make([]byte, MaxNoteSize+MaxFrontierSize+100)); err != nil {
			t.Error(err)
		}
	}
	// A tile.
	// The directory is removed by hand with retries: on Windows a scanner can hold a
	// file that just grew for a moment, which TempDir does not wait for.
	root, err := os.MkdirTemp("", "ledgerlog-grow")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		for i := 0; i < 40 && os.RemoveAll(root) != nil; i++ {
			time.Sleep(50 * time.Millisecond)
		}
	})
	d, err := NewDirStore(filepath.Join(root, "log"))
	if err != nil {
		t.Fatal(err)
	}
	d.NoSync = true
	k := TileKey{0, 0, 2}
	data, _ := EncodeTile([]Hash{{1}, {2}})
	if err := d.PutTile(bg, k, data); err != nil {
		t.Fatal(err)
	}
	d.afterStat = grow
	reads = nil
	if _, err := d.GetTile(bg, k); err == nil || !strings.Contains(err.Error(), "more than") {
		t.Fatalf("a tile that grew: %v", err)
	}
	if len(reads) != 1 || reads[0] != int64(len(data))+1 {
		t.Errorf("a tile of %d bytes that grew was read as %v bytes, expected one read of the limit and one byte (%d)", len(data), reads, len(data)+1)
	}
	// A checkpoint.
	d.afterStat = nil
	if err := d.PutCheckpoint(bg, []byte("note\n")); err != nil {
		t.Fatal(err)
	}
	d.afterStat = grow
	reads = nil
	if _, err := d.GetCheckpoint(bg); err == nil || !strings.Contains(err.Error(), "more than") {
		t.Fatalf("a checkpoint that grew: %v", err)
	}
	if len(reads) != 1 || reads[0] != MaxNoteSize+1 {
		t.Errorf("a checkpoint that grew was read as %v bytes, expected %d", reads, MaxNoteSize+1)
	}
	// A frontier file.
	l, _ := Create(bg, testOrigin(1), NewMemStore())
	ap, _ := l.Append(bg, entriesRange(0, 5))
	path := filepath.Join(t.TempDir(), "frontier")
	if err := SaveFrontierFile(path, l.Frontier()); err != nil {
		t.Fatal(err)
	}
	frontierAfterStat = grow
	reads = nil
	if _, err := LoadFrontierFile(path, ap.Size, ap.Root); err == nil || !strings.Contains(err.Error(), "more than") {
		t.Fatalf("a frontier file that grew: %v", err)
	}
	if len(reads) != 1 || reads[0] != MaxFrontierSize+1 {
		t.Errorf("a frontier file that grew was read as %v bytes, expected %d", reads, MaxFrontierSize+1)
	}
	// The helper itself, with a reader that has no end: it stops at the limit
	// (and the reader gives up with an error after 8 MiB, so that a helper
	// that lost its limit fails this test instead of reading for ever).
	r := &endless{budget: 8 << 20}
	if _, err := readAtMost(r, "x", 1000); err == nil || r.served != 1001 {
		t.Errorf("an endless reader: %d bytes served, %v", r.served, err)
	}
}

type endless struct{ budget, served int64 }

func (e *endless) Read(p []byte) (int, error) {
	if e.served >= e.budget {
		return 0, errors.New("the reader was read past its budget")
	}
	n := int64(len(p))
	if e.served+n > e.budget {
		n = e.budget - e.served
	}
	e.served += n
	return int(n), nil
}

// NewDirStore flushes the parents of the directories it creates, deepest first.
func TestNewDirStoreFlushesTheParents(t *testing.T) {
	var synced []string
	syncDirHook = func(dir string) { synced = append(synced, dir) }
	t.Cleanup(func() { syncDirHook = nil })
	base := t.TempDir()
	if _, err := NewDirStore(filepath.Join(base, "a", "b", "c")); err != nil {
		t.Fatal(err)
	}
	want := []string{filepath.Join(base, "a", "b"), filepath.Join(base, "a"), base}
	if len(synced) != len(want) {
		t.Fatalf("flushed %v, expected %v", synced, want)
	}
	for i := range want {
		if synced[i] != want[i] {
			t.Errorf("flush %d is %s, expected %s", i, synced[i], want[i])
		}
	}
	// A directory that exists flushes nothing.
	synced = nil
	if _, err := NewDirStore(filepath.Join(base, "a", "b", "c")); err != nil || len(synced) != 0 {
		t.Errorf("an existing directory flushed %v, %v", synced, err)
	}
}
