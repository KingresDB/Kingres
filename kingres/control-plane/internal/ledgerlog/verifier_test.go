// SPDX-License-Identifier: AGPL-3.0-only

package ledgerlog

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/ledgerkeys"
)

// These tests need a built kingres-verify. KINGRES_VERIFY names it (the older
// name KINGRES_VERIFY_BIN of the cluster tests is read too); without it they
// are skipped.

func verifierBin(t testing.TB) string {
	t.Helper()
	p := os.Getenv("KINGRES_VERIFY")
	if p == "" {
		p = os.Getenv("KINGRES_VERIFY_BIN")
	}
	if p == "" {
		t.Skip("KINGRES_VERIFY does not name a built kingres-verify")
	}
	return p
}

func runVerifier(t testing.TB, args ...string) (int, string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	out, err := exec.CommandContext(ctx, verifierBin(t), args...).CombinedOutput()
	if err == nil {
		return 0, string(out)
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode(), string(out)
	}
	t.Fatalf("running kingres-verify: %v", err)
	return -1, ""
}

// publication is a publication directory with the keys that sign it.
type publication struct {
	t       *testing.T
	dir     string
	trust   string
	now     string
	set     ledgerkeys.Set
	signers map[string]*ledgerkeys.FileSigner
	phase   time.Time
}

var epoch0 = time.Date(2026, 12, 1, 10, 0, 0, 0, time.UTC)

// newPublication makes the keys and the trust file of the end state: a retired
// key, an active key, and the time the verifier is told it is.
func newPublication(t *testing.T) *publication {
	p := &publication{t: t, dir: t.TempDir(), signers: map[string]*ledgerkeys.FileSigner{}}
	k1, k2 := testKey(t, 1), testKey(t, 77)
	p.signers[k1.Fingerprint()] = k1
	p.signers[k2.Fingerprint()] = k2
	ok := func(s ledgerkeys.Set, err error) ledgerkeys.Set {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	p.set = ok(p.set.Add(k1.Public(), "first", epoch0))
	p.set = ok(p.set.Activate(k1.Fingerprint(), epoch0, 0))
	p.set = ok(p.set.Add(k2.Public(), "second", epoch0.Add(time.Hour)))
	// The trust file of the end of the story, written from the state machine
	// as the control plane will write it: k1 retired, k2 active.
	end := ok(p.set.Activate(k2.Fingerprint(), epoch0.Add(2*time.Hour), 0))
	end = ok(end.Retire(k1.Fingerprint(), epoch0.Add(3*time.Hour), ledgerkeys.Unbounded()))
	tf, err := ledgerkeys.TrustFile(testPrefix, end, ledgerkeys.TrustOptions{})
	if err != nil {
		t.Fatal(err)
	}
	p.trust = filepath.Join(p.dir, "trust.json")
	if err := os.WriteFile(p.trust, tf, 0o644); err != nil {
		t.Fatal(err)
	}
	p.now = epoch0.Add(48 * time.Hour).Format(time.RFC3339)
	return p
}

// setPhase moves the key state machine to the state of the story at stage 0
// (only k1), 1 (k1 and k2 sign: an overlap) or 2 (only k2).
func (p *publication) setPhase(stage int) {
	t := p.t
	k2 := ""
	for fp := range p.signers {
		if fp != p.set[0].Fingerprint() {
			k2 = fp
		}
	}
	switch stage {
	case 1:
		if s, err := p.set.Activate(k2, epoch0.Add(2*time.Hour), 0); err == nil {
			p.set = s
			p.phase = epoch0.Add(2 * time.Hour)
		} else if !errors.Is(err, ledgerkeys.ErrTransition) {
			t.Fatal(err)
		}
	case 2:
		if s, err := p.set.Retire(p.set[0].Fingerprint(), epoch0.Add(3*time.Hour), ledgerkeys.Unbounded()); err == nil {
			p.set = s
			p.phase = epoch0.Add(3 * time.Hour)
		} else if !errors.Is(err, ledgerkeys.ErrTransition) {
			t.Fatal(err)
		}
	default:
		p.phase = epoch0
	}
}

func (p *publication) currentSigners() []ledgerkeys.Signer {
	var out []ledgerkeys.Signer
	ks, err := p.set.Signers(p.phase.Add(time.Minute))
	if err != nil {
		p.t.Fatal(err)
	}
	for _, k := range ks {
		out = append(out, p.signers[k.Fingerprint()])
	}
	return out
}

// logDir returns the directory of a log below the publication.
func (p *publication) logDir(rel string) string { return filepath.Join(p.dir, filepath.FromSlash(rel)) }

// buildLog builds a log of n leaves in the publication with an intermediate
// checkpoint at size a (signed with the first key as it was at the time,
// written to oldNote), and the final checkpoint with the keys of stage.
func (p *publication) buildLog(rng interface{ Intn(int) int }, rel, origin string, entry func(i int) []byte, n, a, stage int) (oldNote string, store *DirStore) {
	t := p.t
	store, err := NewDirStore(p.logDir(rel))
	if err != nil {
		t.Fatal(err)
	}
	store.NoSync = true
	l, err := Create(bg, origin, store)
	if err != nil {
		t.Fatal(err)
	}
	app := func(from, to int) {
		for from < to {
			// Random batching, within MaxBatch.
			step := 1 + rng.Intn(1+rng.Intn(800))
			end := min(from+step, to)
			var es [][]byte
			for i := from; i < end; i++ {
				es = append(es, entry(i))
			}
			if _, err := l.Append(bg, es); err != nil {
				t.Fatal(err)
			}
			from = end
		}
	}
	k1 := p.signers[p.set[0].Fingerprint()]
	app(0, a)
	oldNoteBytes, err := SignCheckpoint(bg, l.Checkpoint(), k1)
	if err != nil {
		t.Fatal(err)
	}
	oldNote = filepath.Join(t.TempDir(), "old.note")
	if err := os.WriteFile(oldNote, oldNoteBytes, 0o644); err != nil {
		t.Fatal(err)
	}
	p.setPhase(stage)
	app(a, n)
	note, err := SignCheckpoint(bg, l.Checkpoint(), p.currentSigners()...)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.PutCheckpoint(bg, note); err != nil {
		t.Fatal(err)
	}
	return oldNote, store
}

func (p *publication) verify(cmd string, extra ...string) (int, string) {
	args := append([]string{cmd, "--trust", p.trust, "--source", p.dir, "--now", p.now}, extra...)
	return runVerifier(p.t, args...)
}

type logCase struct {
	rel     string
	logFlag string
	n, a    int
	old     string
	store   *DirStore
	origin  string
}

func chainID(i int) (id [32]byte) {
	for j := range id {
		id[j] = byte(i*31 + j*7 + 1)
	}
	return
}

func TestPublicationAgainstTheVerifier(t *testing.T) {
	verifierBin(t)
	rng := newRand(t)
	p := newPublication(t)
	sizes := []int{0, 1, 2, 3, 255, 256, 257, 300 + rng.Intn(400)}
	if longTests() {
		sizes = []int{0, 1, 2, 3, 255, 256, 257, 65536 + 3, 4 + rng.Intn(3000), 4 + rng.Intn(3000), 300 + rng.Intn(6000), 66000 + rng.Intn(3000)}
	} else {
		t.Log("the logs of 65,539 and more leaves and the long random sizes run with KINGRES_LONG_TESTS=1; a few hundred leaves run here")
	}
	var cases []logCase
	for i, n := range sizes {
		// The rotation is in the middle: the first logs are signed by the first
		// key, the middle ones by both, the last ones by the second key only.
		stage := 3 * i / len(sizes)
		id := chainID(i)
		origin, err := ChainOrigin(testPrefix, id, 1)
		if err != nil {
			t.Fatal(err)
		}
		rel := fmt.Sprintf("c/%x/e1", id[:])
		a := n
		if n >= 2 {
			a = 1 + rng.Intn(n-1)
			if i%4 == 1 {
				a = n // an old checkpoint of the same size
			}
		}
		lc := logCase{rel: rel, logFlag: fmt.Sprintf("chain:%x:e1", id[:]), n: n, a: a, origin: origin}
		if n == 0 {
			store, err := NewDirStore(p.logDir(rel))
			if err != nil {
				t.Fatal(err)
			}
			p.setPhase(stage)
			note, err := SignCheckpoint(bg, Checkpoint{Origin: origin, Size: 0, Root: EmptyRoot()}, p.currentSigners()...)
			if err != nil {
				t.Fatal(err)
			}
			if err := store.PutCheckpoint(bg, note); err != nil {
				t.Fatal(err)
			}
			lc.store = store
		} else {
			lc.old, lc.store = p.buildLog(rng, rel, origin, entryData, n, a, stage)
		}
		cases = append(cases, lc)
	}
	// An aggregate log with leaves of the codec of 5.2, signed by both keys.
	{
		id := chainID(99)
		origin, _ := AggregateOrigin(testPrefix, id)
		rel := fmt.Sprintf("a/%x", id[:])
		entry := func(i int) []byte {
			var ms []AggregateMember
			for j := 0; j < 1+i%5; j++ {
				ms = append(ms, member(j+i, byte(i)&flagsMask))
			}
			d, err := AggregateEntry{Seq: uint64(i), GeneratedAtUS: int64(i) * 1000, Members: ms}.Marshal()
			if err != nil {
				t.Fatal(err)
			}
			return d
		}
		old, store := p.buildLog(rng, rel, origin, entry, 300, 120, 1)
		cases = append(cases, logCase{rel: rel, logFlag: fmt.Sprintf("aggregate:%x", id[:]), n: 300, a: 120, old: old, store: store, origin: origin})
	}
	// Reopening the first log from its frontier, to be sure the tiles that the
	// verifier reads are the ones the log would continue from.
	for _, lc := range cases {
		if lc.n > 0 {
			l := mustOpenFromTiles(t, lc)
			if l.Size() != uint64(lc.n) {
				t.Fatalf("%s: reopened with %d leaves", lc.rel, l.Size())
			}
		}
	}

	t.Run("checkpoint and consistency are accepted", func(t *testing.T) {
		for _, lc := range cases {
			if st, out := p.verify("checkpoint", "--log", lc.logFlag); st != 0 {
				t.Errorf("%s (n=%d): checkpoint status %d\n%s", lc.rel, lc.n, st, out)
			}
			if st, out := p.verify("checkpoint", "--log", lc.logFlag, "--min-size", strconv.Itoa(lc.n)); st != 0 {
				t.Errorf("%s: checkpoint --min-size: status %d\n%s", lc.rel, st, out)
			}
			if lc.n == 0 {
				r := fmt.Sprintf("0:%s", hexToB64(EmptyRoot()))
				if st, out := p.verify("consistency", "--log", lc.logFlag, "--old", r); st != 0 {
					t.Errorf("%s: consistency from the empty tree: status %d\n%s", lc.rel, st, out)
				}
				continue
			}
			if st, out := p.verify("consistency", "--log", lc.logFlag, "--old", lc.old); st != 0 {
				t.Errorf("%s (a=%d n=%d): consistency status %d\n%s", lc.rel, lc.a, lc.n, st, out)
			}
			// The old checkpoint given as size and root.
			l := mustOpenFromTilesAt(t, lc, lc.a)
			if st, out := p.verify("consistency", "--log", lc.logFlag, "--old", fmt.Sprintf("%d:%s", lc.a, hexToB64(l))); st != 0 {
				t.Errorf("%s: consistency from size and root: status %d\n%s", lc.rel, st, out)
			}
			// A wrong old root is a failure (status 1) when a proof is needed.
			if lc.a > 0 && lc.a < lc.n {
				bad := l
				bad[0] ^= 1
				if st, out := p.verify("consistency", "--log", lc.logFlag, "--old", fmt.Sprintf("%d:%s", lc.a, hexToB64(bad))); st != 1 {
					t.Errorf("%s: a wrong old root gave status %d\n%s", lc.rel, st, out)
				}
			}
		}
	})

	t.Run("a state file follows the log", func(t *testing.T) {
		for _, lc := range cases {
			if lc.n == 0 || lc.a >= lc.n {
				continue
			}
			state := filepath.Join(t.TempDir(), "state.json")
			if st, out := p.verify("checkpoint", "--log", lc.logFlag, "--note", lc.old, "--state", state, "--allow-shared-state-dir"); st != 0 {
				t.Fatalf("%s: first state: %d\n%s", lc.rel, st, out)
			}
			if st, out := p.verify("checkpoint", "--log", lc.logFlag, "--state", state, "--allow-shared-state-dir"); st != 0 {
				t.Fatalf("%s: the state moves to the final checkpoint with a proof: %d\n%s", lc.rel, st, out)
			}
		}
	})

	t.Run("an extension is found by the verifier", func(t *testing.T) {
		// Logs of different content with the same size: the proof from one to
		// the other fails.
		a, b := cases[4], cases[5] // 255 and 256 leaves, different logs
		note, _ := os.ReadFile(a.old)
		_ = note
		st, out := p.verify("consistency", "--log", b.logFlag, "--old", a.old)
		if st == 0 {
			t.Errorf("a checkpoint of another log was taken as the old one\n%s", out)
		}
	})

	t.Run("a changed byte is found", func(t *testing.T) {
		for _, lc := range cases {
			if lc.n == 0 {
				continue
			}
			p.flipChecks(t, lc, rng)
		}
	})
}

func hexToB64(h Hash) string {
	return base64.StdEncoding.EncodeToString(h[:])
}

func mustOpenFromTiles(t *testing.T, lc logCase) *Log { return mustOpenFromTilesLog(t, lc, lc.n) }

// mustOpenFromTilesAt returns the root of the first a leaves, read from the
// tiles of the final log with a Reader (the way a verifier derives it).
func mustOpenFromTilesAt(t *testing.T, lc logCase, a int) Hash {
	t.Helper()
	r, err := NewReader(bg, lc.store, uint64(lc.n))
	if err != nil {
		t.Fatal(err)
	}
	h, err := r.MTH(0, uint64(a))
	if err != nil {
		// A root of an older size that is not a power of two combines nodes of
		// tiles; the reader of the final size has them all.
		t.Fatalf("%s: root at %d: %v", lc.rel, a, err)
	}
	return h
}

// mustOpenFromTilesLog rebuilds a Log from the checkpoint and the right edge
// of the tiles the way a restart does, with a frontier computed by reading the
// leaves back.
func mustOpenFromTilesLog(t *testing.T, lc logCase, n int) *Log {
	t.Helper()
	r, err := NewReader(bg, lc.store, uint64(n))
	if err != nil {
		t.Fatal(err)
	}
	var fr Frontier
	for i := 0; i < n; i++ {
		h, err := r.LeafHash(uint64(i))
		if err != nil {
			t.Fatal(err)
		}
		if err := fr.Append(h); err != nil {
			t.Fatal(err)
		}
	}
	l, err := Open(bg, lc.origin, lc.store, fr.Marshal(), uint64(n), fr.Root(), Options{})
	if err != nil {
		t.Fatalf("%s: %v", lc.rel, err)
	}
	return l
}

// flipChecks damages each kind of file of a log in turn, expects the verifier to
// fail, and restores the file.
func (p *publication) flipChecks(t *testing.T, lc logCase, rng interface{ Intn(int) int }) {
	t.Helper()
	root := p.logDir(lc.rel)
	flipFile := func(path string, pos int, mask byte) (restore func()) {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		bad := bytes.Clone(data)
		bad[pos] ^= mask
		if err := os.WriteFile(path, bad, 0o644); err != nil {
			t.Fatal(err)
		}
		return func() {
			if err := os.WriteFile(path, data, 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	// The checkpoint: the origin, the size, the root, a signature.
	cp := filepath.Join(root, "checkpoint")
	note, _ := os.ReadFile(cp)
	sepPos := bytes.Index(note, []byte("\n\n"))
	nl1 := bytes.IndexByte(note, '\n')
	nl2 := nl1 + 1 + bytes.IndexByte(note[nl1+1:], '\n')
	positions := map[string]int{
		"origin":    len(lc.origin) - 3,
		"size":      nl1 + 1,
		"root":      nl2 + 3,
		"signature": sepPos + 2 + len("\u2014 ") + len(lc.origin) + 1 + 6,
		"key id":    sepPos + 2 + len("\u2014 ") + len(lc.origin) + 1,
	}
	for name, pos := range positions {
		mask := byte(0x01)
		if name == "key id" {
			// Change the first base64 character to another valid one, so that
			// the line stays well formed and only the key id differs.
			c := note[pos]
			target := byte('A')
			if c == 'A' {
				target = 'B'
			}
			mask = c ^ target
		}
		restore := flipFile(cp, pos, mask)
		st, out := p.verify("checkpoint", "--log", lc.logFlag)
		// A signature line whose key id matches no pinned key is ignored, as
		// signed-note says, so with a second valid line the note still passes.
		pn, _ := ParseNote(note)
		if name == "key id" && len(pn.Signatures) > 1 {
			if st != 0 {
				t.Errorf("%s: a changed key id on one of two lines gave status %d\n%s", lc.rel, st, out)
			}
		} else if st == 0 {
			t.Errorf("%s: a changed %s of the checkpoint was accepted\n%s", lc.rel, name, out)
		}
		restore()
	}
	// The trust file: the fingerprint and the key.
	tf, _ := os.ReadFile(p.trust)
	i := bytes.Index(tf, []byte(`"fingerprint": "`)) + len(`"fingerprint": "`)
	for _, pos := range []int{i, i + 20} {
		restore := flipFile(p.trust, pos, 0x01)
		if st, out := p.verify("checkpoint", "--log", lc.logFlag); st == 0 {
			t.Errorf("a changed trust file was accepted\n%s", out)
		}
		restore()
	}
	if lc.a >= lc.n || lc.a == 0 {
		return // no tile is on a proof path of an empty or equal range
	}
	// The tiles on the path of the proof: a byte inside the hashes that the
	// proof uses is changed; the verifier must fail (status 1).
	type use struct {
		k          TileKey
		first, cnt int
	}
	var uses []use
	rd, _ := NewReader(bg, lc.store, uint64(lc.n))
	rd.Trace = func(k TileKey, first, count int) { uses = append(uses, use{k, first, count}) }
	if _, err := rd.ConsistencyProof(uint64(lc.a)); err != nil {
		t.Fatal(err)
	}
	if len(uses) == 0 {
		t.Fatalf("%s: the proof reads no tile", lc.rel)
	}
	rng2 := rng
	flips := 2
	if longTests() {
		flips = 6
	}
	for tries := 0; tries < flips; tries++ {
		u := uses[rng2.Intn(len(uses))]
		path := filepath.Join(root, filepath.FromSlash(u.k.Path()))
		pos := (u.first+rng2.Intn(u.cnt))*HashSize + rng2.Intn(HashSize)
		restore := flipFile(path, pos, byte(1<<uint(rng2.Intn(8))))
		st, out := p.verify("consistency", "--log", lc.logFlag, "--old", lc.old)
		restore()
		if st != 1 {
			t.Errorf("%s: a changed byte %d of %s (hashes %d to %d) gave status %d, expected 1\n%s", lc.rel, pos, u.k.Path(), u.first, u.first+u.cnt, st, out)
		}
	}
	// A tile of the wrong size is a failure; a missing full tile is "could not
	// check" (status 3). A partial tile that is missing falls back to the full one.
	u := uses[0]
	path := filepath.Join(root, filepath.FromSlash(u.k.Path()))
	data, _ := os.ReadFile(path)
	if err := os.WriteFile(path, data[:len(data)-1], 0o644); err != nil {
		t.Fatal(err)
	}
	if st, out := p.verify("consistency", "--log", lc.logFlag, "--old", lc.old); st != 1 {
		t.Errorf("%s: a short tile gave status %d\n%s", lc.rel, st, out)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	for _, u := range uses {
		if !u.k.Full() {
			continue
		}
		path := filepath.Join(root, filepath.FromSlash(u.k.Path()))
		data, _ := os.ReadFile(path)
		_ = os.Remove(path)
		st, out := p.verify("consistency", "--log", lc.logFlag, "--old", lc.old)
		_ = os.WriteFile(path, data, 0o644)
		if st != 3 {
			t.Errorf("%s: a missing tile %s gave status %d, expected 3\n%s", lc.rel, u.k.Path(), st, out)
		}
		break
	}
	// An entry bundle is not read by the verifier of step 1 (it checks the
	// hashes of tiles): the damage is found by Audit instead.
	bk := mustRequired(t, uint64(lc.n))[0] // the first key is an entry bundle
	if bk.Level != EntryLevel {
		t.Fatalf("%s: the first required key is %v", lc.rel, bk)
	}
	bpath := filepath.Join(root, filepath.FromSlash(bk.Path()))
	restore := flipFile(bpath, 5, 0x10)
	st, _ := p.verify("consistency", "--log", lc.logFlag, "--old", lc.old)
	t.Logf("%s: a changed entry bundle: verifier status %d (it does not read bundles)", lc.rel, st)
	if err := Audit(bg, lc.store, uint64(lc.n), mustRoot(t, lc)); err == nil {
		t.Errorf("%s: a changed entry bundle passed the audit", lc.rel)
	}
	restore()
	if err := Audit(bg, lc.store, uint64(lc.n), mustRoot(t, lc)); err != nil {
		t.Errorf("%s: audit after the restore: %v", lc.rel, err)
	}
}

func mustRoot(t *testing.T, lc logCase) Hash {
	t.Helper()
	r, _ := NewReader(bg, lc.store, uint64(lc.n))
	h, err := r.Root()
	if err != nil {
		t.Fatal(err)
	}
	return h
}
