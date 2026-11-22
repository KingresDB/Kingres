// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026 RooyeKhat Media Co

package ledgeranchor

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/anchor"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/api"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/internalapi"
)

// The tests of the fixes of the review of the fixes P7F (docs/reviews/2026-11-22-ledger-phase7-p7f-fixes.md): the rotation generation that decides
// which credentials file is the newer one (P7G-02), the keyed digest (P7G-05), the classes of the failures to take a newer file (P7G-04), the
// pruning of the gate (P7G-06), and the survivors of the review.

// ---- the files: the generation, the stamp, the digest ----

func TestARotationRaisesTheGenerationAndEveryCopyKeepsGenerationAndStamp(t *testing.T) {
	e := newEnv(t)
	tg := e.addWorm("vault")
	d1, err := e.creds.Digest(tg.ID)
	if err != nil || d1.Generation != 1 || len(d1.Digest) != 64 {
		t.Fatalf("%+v %v", d1, err)
	}
	if _, err := e.svc.UpdateTarget(e.ctx(), tg.ID, api.LedgerAnchorTargetUpdate{Credentials: wormCreds()}); err != nil {
		t.Fatal(err)
	}
	d2, _ := e.creds.Digest(tg.ID)
	if d2.Generation != 2 || d2.Digest == d1.Digest {
		t.Fatalf("a rotation with the same credentials is a new rotation: %+v then %+v", d1, d2)
	}
	// a copy on another server, plain or sealed under its own passphrase, is the same rotation: the same generation and digest
	raw, _ := e.creds.Raw(tg.ID)
	pf := e.dir + "/pass"
	if err := os.WriteFile(pf, []byte("a passphrase of this test\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	sealed, err := NewCredStore(t.TempDir(), pf)
	if err != nil {
		t.Fatal(err)
	}
	sealed.Iterations, sealed.MinIterations = 2000, 1000
	// a server that seals does not take a file in clear above generation 0 (P7H-01): the generation of a plain file is not authenticated
	if err := sealed.Install(tg.ID, "worm", append([]byte(nil), raw...)); !errors.Is(err, ErrPlainFile) {
		t.Fatalf("a plain file on a sealed server: %v", err)
	}
	// a copy between two servers that seal with the same passphrase is the same rotation: the same generation and digest
	creds2, _ := e.creds.Get(tg.ID, "worm")
	if _, err := sealed.Rotate(tg.ID, creds2, 1); err != nil {
		t.Fatal(err)
	}
	first, _ := sealed.Raw(tg.ID)
	if strings.Contains(string(first), testSecret) {
		t.Fatal("the sealed file is not sealed")
	}
	second, _ := NewCredStore(t.TempDir(), pf)
	second.Iterations, second.MinIterations = 2000, 1000
	if err := second.Install(tg.ID, "worm", append([]byte(nil), first...)); err != nil {
		t.Fatal(err)
	}
	sd, err := sealed.Digest(tg.ID)
	sd2, err2 := second.Digest(tg.ID)
	if err != nil || err2 != nil || sd != sd2 || sd.Generation != 2 {
		t.Fatalf("copies %+v %+v (%v %v)", sd, sd2, err, err2)
	}
	d2 = sd
	// and it goes back to a plain server unchanged
	sraw, _ := sealed.Raw(tg.ID)
	back, _ := NewCredStore(t.TempDir(), "")
	back.Iterations, back.MinIterations = 2000, 1000
	if err := back.Install(tg.ID, "worm", sraw); err != nil {
		// a plain server cannot open a sealed file: the passphrase is not there (this is the unopenable class)
		if errors.Is(err, ErrInstallWrite) {
			t.Fatalf("a file that does not open here is not a failure of this server: %v", err)
		}
	}
}

// P7G-05: the digest is keyed with the stamp, so it cannot be tested against guesses of the secret by whoever sees only the digest, and two
// rotations of the same credentials do not share one.
func TestTheDigestIsKeyedWithTheStampAndComparedInConstantTimeForm(t *testing.T) {
	e := newEnv(t)
	c := Creds{"accessKeyId": "AKIAFAKEKEYID000001", "secretAccessKey": "a-secret-that-is-the-same"}
	st1, st2 := newStamp(t), newStamp(t)
	f1, _ := e.creds.Encode(c, 4, st1)
	f2, _ := e.creds.Encode(c, 4, st2)
	d1, d2 := e.infoOfFile(f1, 1), e.infoOfFile(f2, 1)
	if d1.Digest == "" || d1.Digest == d2.Digest {
		t.Fatalf("the same credentials of two rotations share a digest: %s %s", d1.Digest, d2.Digest)
	}
	// a different generation, a different member, a different id: a different digest
	f3, _ := e.creds.Encode(c, 5, st1)
	if e.infoOfFile(f3, 1).Digest == d1.Digest {
		t.Error("another generation, the same digest")
	}
	if e.infoOfFile(f1, 2).Digest == d1.Digest {
		t.Error("another target, the same digest")
	}
	c2 := Creds{"accessKeyId": "AKIAFAKEKEYID000001", "secretAccessKey": "a-secret-that-is-the-same-x"}
	f4, _ := e.creds.Encode(c2, 4, st1)
	if e.infoOfFile(f4, 1).Digest == d1.Digest {
		t.Error("another secret, the same digest")
	}
	// the digest is not a hash of the content alone: whoever has the content but not the stamp cannot compute it
	m := map[string]string{"accessKeyId": "AKIAFAKEKEYID000001", "secretAccessKey": "a-secret-that-is-the-same"}
	canon, _ := json.Marshal(m)
	naive := anchorHash("kingres-anchor-credentials-digest\x001\x004\x00" + string(canon))
	if d1.Digest == naive {
		t.Error("the digest is an unkeyed hash of the content")
	}
	if strings.Contains(d1.Digest, "a-secret") {
		t.Error("the digest holds the secret")
	}
	if !sameDigest(d1.Digest, d1.Digest) || sameDigest(d1.Digest, d2.Digest) || sameDigest("", "") || sameDigest(d1.Digest, "") {
		t.Error("sameDigest")
	}
}

func anchorHash(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// P7G-02: a server never writes a file over one of a higher generation, or over another of the same generation.
func TestAServerRefusesAnOlderOrRivalFileWhoeverSendsIt(t *testing.T) {
	e := newEnv(t)
	tg := e.addWorm("vault")
	c, _ := e.creds.Get(tg.ID, "worm")
	if _, err := e.creds.Put(tg.ID, c); err != nil { // generation 2
		t.Fatal(err)
	}
	cur, _ := e.creds.Digest(tg.ID)
	older, _ := e.creds.Encode(Creds{"accessKeyId": c["accessKeyId"], "secretAccessKey": "an-older-secret-of-generation-one"}, 1, newStamp(t))
	rival, _ := e.creds.Encode(Creds{"accessKeyId": c["accessKeyId"], "secretAccessKey": "a-rival-secret-of-generation-two"}, 2, newStamp(t))
	for name, f := range map[string][]byte{"older": older, "rival": rival} {
		if err := e.creds.Install(tg.ID, "worm", append([]byte(nil), f...)); !errors.Is(err, ErrOlderGeneration) {
			t.Errorf("%s: %v", name, err)
		}
		if now, _ := e.creds.Digest(tg.ID); now != cur {
			t.Errorf("%s: the file was changed: %+v", name, now)
		}
	}
	// the same rotation again is harmless, a higher generation is taken
	same, _ := e.creds.Raw(tg.ID)
	if err := e.creds.Install(tg.ID, "worm", same); err != nil {
		t.Errorf("the same rotation again: %v", err)
	}
	higher, _ := e.creds.Encode(c, 3, newStamp(t))
	if err := e.creds.Install(tg.ID, "worm", higher); err != nil {
		t.Errorf("a higher generation: %v", err)
	}
	if now, _ := e.creds.Digest(tg.ID); now.Generation != 3 {
		t.Errorf("%+v", now)
	}
	// the call that holders answer says so with a status of its own
	body, _ := json.Marshal(InstallRequest{TargetID: tg.ID, Kind: "worm", File: base64.StdEncoding.EncodeToString(older)})
	_, err := (&Agent{Creds: e.creds}).Handle(e.ctx(), CallCredentialsInstall, body)
	var ce *internalapi.CallError
	if !errors.As(err, &ce) || ce.Status != http.StatusConflict || ce.Code != "older_generation" {
		t.Fatalf("%v", err)
	}
}

// P7G-02: the generation of a rotation is above the highest that a holder reports, so that a leader that missed rotations does not write a
// number that the others hold already.
func TestARotationByALeaderThatMissedRotationsIsAboveWhatTheHoldersHold(t *testing.T) {
	e := newEnv(t)
	tg := e.addWorm("vault")
	e.svc.cfg.Holders = func(context.Context, []int64) (HolderReport, error) {
		rep := NewHolderReport()
		rep.Copies[tg.ID] = []HolderCopy{{Host: "h2", Generation: 7, Digest: "x"}, {Host: "h3", Generation: 5, Digest: "y"}}
		return rep, nil
	}
	if _, err := e.svc.UpdateTarget(e.ctx(), tg.ID, api.LedgerAnchorTargetUpdate{Credentials: wormCreds()}); err != nil {
		t.Fatal(err)
	}
	if d, _ := e.creds.Digest(tg.ID); d.Generation != 8 {
		t.Fatalf("generation %d, want 8 (one above the highest of the holders)", d.Generation)
	}
	// a leader that holds more than the holders say keeps counting from its own
	e.svc.cfg.Holders = func(context.Context, []int64) (HolderReport, error) { return NewHolderReport(), nil }
	if _, err := e.svc.UpdateTarget(e.ctx(), tg.ID, api.LedgerAnchorTargetUpdate{Credentials: wormCreds()}); err != nil {
		t.Fatal(err)
	}
	if d, _ := e.creds.Digest(tg.ID); d.Generation != 9 {
		t.Fatalf("generation %d, want 9", d.Generation)
	}
}

// ---- files of earlier versions ----

func legacyPlain(c Creds) []byte {
	plain, _ := c.plain()
	return append(append([]byte(CredMagic+"\n"+`{"credentials":`), plain...), '}', '\n')
}

// legacySealed is the file that an earlier version wrote under a passphrase: the members only are sealed.
func legacySealed(t *testing.T, cs *CredStore, c Creds) []byte {
	t.Helper()
	plain, _ := c.plain()
	pass, err := cs.passphrase()
	if err != nil {
		t.Fatal(err)
	}
	defer pass.Destroy()
	salt, nonce := make([]byte, credSalt), make([]byte, credNonce)
	_, _ = io.ReadFull(rand.Reader, salt)
	_, _ = io.ReadFull(rand.Reader, nonce)
	aead, err := credAEAD(pass, uint32(cs.Iterations), salt)
	if err != nil {
		t.Fatal(err)
	}
	sealed := aead.Seal(nil, nonce, plain, credHeader(uint32(cs.Iterations), salt))
	e := base64.StdEncoding.EncodeToString
	return []byte(CredMagic + "\n" + `{"kdf":"` + credKDF + `","iterations":` + strconv.Itoa(cs.Iterations) + `,"salt":"` + e(salt) + `","nonce":"` + e(nonce) + `","ciphertext":"` + e(sealed) + `"}` + "\n")
}

func TestAFileOfAnEarlierVersionIsReadAsGenerationZeroAndIsOlderThanEveryRotation(t *testing.T) {
	e := newEnv(t)
	c := Creds{"accessKeyId": "AKIAFAKEKEYID000001", "secretAccessKey": "a-secret-of-an-earlier-version"}
	if err := os.WriteFile(e.creds.path(7), legacyPlain(c), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := e.creds.Get(7, "worm")
	if err != nil || got["secretAccessKey"].Reveal() != "a-secret-of-an-earlier-version" {
		t.Fatalf("an old plain file is not read: %v", err)
	}
	if d, err := e.creds.Digest(7); err != nil || d.Generation != 0 || d.Digest != "" {
		t.Fatalf("%+v %v", d, err)
	}
	// sealed
	pf := e.dir + "/pass"
	if err := os.WriteFile(pf, []byte("a passphrase of this test\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cs, err := NewCredStore(t.TempDir(), pf)
	if err != nil {
		t.Fatal(err)
	}
	cs.Iterations, cs.MinIterations = 2000, 1000
	if err := os.WriteFile(cs.path(8), legacySealed(t, cs, c), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err = cs.Get(8, "worm")
	if err != nil || got["secretAccessKey"].Reveal() != "a-secret-of-an-earlier-version" {
		t.Fatalf("an old sealed file is not read: %v", err)
	}
	if d, err := cs.Digest(8); err != nil || d.Generation != 0 {
		t.Fatalf("%+v %v", d, err)
	}
	// two files of earlier versions cannot be told apart: they are current to each other, and no holder of an earlier version is judged
	if n, s, k, _ := judgeCopies(CopyInfo{}, []HolderCopy{{Host: "h2"}}); n != nil || len(s) != 0 || len(k) != 0 {
		t.Fatalf("%v %v %v", n, s, k)
	}
	// a file of an earlier version is installed as it is (generation 0), and the first rotation is above it
	if err := e.creds.Install(9, "worm", legacyPlain(c)); err != nil {
		t.Fatal(err)
	}
	if d, _ := e.creds.Digest(9); d.Generation != 0 {
		t.Fatalf("%+v", d)
	}
	if _, err := e.creds.Put(9, c); err != nil {
		t.Fatal(err)
	}
	if d, _ := e.creds.Digest(9); d.Generation != 1 || d.Digest == "" {
		t.Fatalf("%+v", d)
	}
	// a rotation makes a holder of generation 0 stale, and a holder of a higher generation makes a leader of generation 0 take its file
	own, _ := e.creds.Digest(9)
	if _, s, _, _ := judgeCopies(own, []HolderCopy{{Host: "h2"}}); len(s) != 1 {
		t.Fatalf("a holder of an earlier version is not behind a rotation: %v", s)
	}
	if n, _, _, _ := judgeCopies(CopyInfo{}, []HolderCopy{{Host: "h2", Generation: 1, Digest: own.Digest}}); n == nil {
		t.Fatal("a leader of generation 0 does not take a rotation")
	}
	// damaged generations are refused
	for name, body := range map[string]string{
		"generation without a stamp": `{"credentials":{"accessKeyId":"AKIAFAKEKEYID000001","secretAccessKey":"a-secret-of-an-earlier-version"},"generation":3,"stamp":""}`,
		"a stamp without generation": `{"credentials":{"accessKeyId":"AKIAFAKEKEYID000001","secretAccessKey":"a-secret-of-an-earlier-version"},"generation":0,"stamp":"00112233445566778899aabbccddeeff"}`,
		"a huge generation":          `{"credentials":{"accessKeyId":"AKIAFAKEKEYID000001","secretAccessKey":"a-secret-of-an-earlier-version"},"generation":99999999999999,"stamp":"00112233445566778899aabbccddeeff"}`,
		"an unknown member":          `{"credentials":{"accessKeyId":"AKIAFAKEKEYID000001","secretAccessKey":"a-secret-of-an-earlier-version"},"generation":1,"stamp":"00112233445566778899aabbccddeeff","x":1}`,
	} {
		if _, err := e.creds.Decode([]byte(CredMagic+"\n"+body+"\n"), "worm"); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// The upgrade end to end: a leader with a file of an earlier version meets a holder that already has a rotation, and a leader that rotates meets
// a holder with a file of an earlier version.
func TestTheUpgradeFromFilesWithoutAGeneration(t *testing.T) {
	e := newEnv(t)
	tg := e.addWorm("vault")
	valid, _ := e.creds.Get(tg.ID, "worm")
	if err := os.WriteFile(e.creds.path(tg.ID), legacyPlain(valid), 0o600); err != nil {
		t.Fatal(err)
	}
	rot, _ := e.creds.Encode(valid, 1, newStamp(t))
	info := e.infoOfFile(rot, tg.ID)
	e.svc.cfg.Holders = func(context.Context, []int64) (HolderReport, error) {
		rep := NewHolderReport()
		rep.Copies[tg.ID] = []HolderCopy{{Host: "h2", Generation: info.Generation, Digest: info.Digest}}
		return rep, nil
	}
	e.svc.cfg.FetchFrom = func(context.Context, string, int64) ([]byte, error) { return append([]byte(nil), rot...), nil }
	given := 0
	e.svc.cfg.Distribute = func(context.Context, int64, string, []byte) error { given++; return nil }
	if err := e.svc.jobCredentials(e.ctx(), e.term); err != nil {
		t.Fatal(err)
	}
	if d, _ := e.creds.Digest(tg.ID); d != info {
		t.Fatalf("the leader did not take the rotation of the holder: %+v want %+v", d, info)
	}
	if given != 0 {
		t.Fatalf("a leader that was behind gave a file away: %d", given)
	}
	// now the leader rotates and the holder still has a file of an earlier version
	if _, err := e.svc.UpdateTarget(e.ctx(), tg.ID, api.LedgerAnchorTargetUpdate{Credentials: wormCreds()}); err != nil {
		t.Fatal(err)
	}
	given = 0
	e.svc.cfg.Holders = func(context.Context, []int64) (HolderReport, error) {
		rep := NewHolderReport()
		rep.Copies[tg.ID] = []HolderCopy{{Host: "h2"}} // generation 0, no digest
		return rep, nil
	}
	if err := e.svc.jobCredentials(e.ctx(), e.term); err != nil {
		t.Fatal(err)
	}
	if given != 1 {
		t.Fatalf("the holder of an earlier version was given the rotation %d times, want 1", given)
	}
}

// ---- the rotation that a holder missed (P7F-01), by generation ----

// rotated is a leader that has changed the credentials of its target while h2 was down: the leader's file is generation 2, h2 still has generation 1.
type rotated struct {
	e        *env
	id       int64
	given    int
	failGive bool
	hold     func() HolderReport
}

func newRotated(t *testing.T) *rotated {
	t.Helper()
	r := &rotated{e: newEnv(t)}
	e := r.e
	e.svc.cfg.Distribute = func(context.Context, int64, string, []byte) error {
		if r.failGive {
			return errors.New("not reached: h2")
		}
		r.given++
		return nil
	}
	tg := e.addWorm("vault")
	r.id = tg.ID
	r.failGive = true
	if _, err := e.svc.UpdateTarget(e.ctx(), tg.ID, api.LedgerAnchorTargetUpdate{Credentials: wormCreds()}); err != nil {
		t.Fatal(err)
	}
	r.failGive = false
	r.given = 0
	_ = e.runMonitor()
	if e.alerts.severity(AlertFailed, "target:1|credentials") != "warning" {
		t.Fatalf("no alert after the failed distribution: %s", e.alerts.all())
	}
	e.svc.cfg.Holders = func(context.Context, []int64) (HolderReport, error) { return r.hold(), nil }
	r.hold = func() HolderReport {
		cur, _ := e.creds.Digest(tg.ID)
		rep := NewHolderReport()
		rep.Copies[tg.ID] = []HolderCopy{{Host: "h2", Generation: 1, Digest: "d1"}, {Host: "h3", Generation: cur.Generation, Digest: cur.Digest}}
		return rep
	}
	return r
}

func (r *rotated) allCurrent() func() HolderReport {
	return func() HolderReport {
		cur, _ := r.e.creds.Digest(r.id)
		rep := NewHolderReport()
		rep.Copies[r.id] = []HolderCopy{{Host: "h2", Generation: cur.Generation, Digest: cur.Digest}, {Host: "h3", Generation: cur.Generation, Digest: cur.Digest}}
		return rep
	}
}

func TestARotationThatAHolderMissedIsGivenAgainAndTheAlertGoesOnlyThen(t *testing.T) {
	r := newRotated(t)
	e := r.e
	r.failGive = true
	if err := e.svc.jobCredentials(e.ctx(), e.term); err != nil {
		t.Fatal(err)
	}
	_ = e.runMonitor()
	if e.alerts.severity(AlertFailed, "target:1|credentials") != "warning" || !strings.Contains(e.alerts.all(), "h2") {
		t.Fatalf("the alert went although h2 holds the old file: %s", e.alerts.all())
	}
	r.failGive = false
	if err := e.svc.jobCredentials(e.ctx(), e.term); err != nil {
		t.Fatal(err)
	}
	if r.given != 1 {
		t.Fatalf("the file was given %d times to the holders, want 1", r.given)
	}
	_ = e.runMonitor()
	if sev := e.alerts.severity(AlertFailed, "target:1|credentials"); sev != "" {
		t.Fatalf("the alert stays after every holder was given the file: %s", e.alerts.all())
	}
	r.hold = r.allCurrent()
	if err := e.svc.jobCredentials(e.ctx(), e.term); err != nil {
		t.Fatal(err)
	}
	if r.given != 1 {
		t.Fatalf("a current holder was given the file again (%d)", r.given)
	}
}

func TestTheAlertOfAMissedRotationStaysWhileAHolderCannotBeAsked(t *testing.T) {
	r := newRotated(t)
	e := r.e
	r.hold = func() HolderReport {
		cur, _ := e.creds.Digest(r.id)
		rep := NewHolderReport()
		rep.Copies[r.id] = []HolderCopy{{Host: "h3", Generation: cur.Generation, Digest: cur.Digest}}
		rep.Unreached = []string{"h2"}
		return rep
	}
	for i := 0; i < 2; i++ {
		if err := e.svc.jobCredentials(e.ctx(), e.term); err != nil {
			t.Fatal(err)
		}
		_ = e.runMonitor()
		if e.alerts.severity(AlertFailed, "target:1|credentials") != "warning" {
			t.Fatalf("pass %d wiped the alert of a holder that was not asked: %s", i, e.alerts.all())
		}
	}
	if !strings.Contains(e.alerts.all(), "given again every minute") {
		t.Fatalf("the text: %s", e.alerts.all())
	}
	r.hold = r.allCurrent()
	if err := e.svc.jobCredentials(e.ctx(), e.term); err != nil {
		t.Fatal(err)
	}
	_ = e.runMonitor()
	if sev := e.alerts.severity(AlertFailed, "target:1|credentials"); sev != "" {
		t.Fatalf("the alert stays although every holder is current: %s", e.alerts.all())
	}
}

// A pass that gave the file to the holders that were behind clears the alert even when another holder could not be asked (the file went to every
// holder, so none is behind): the flag of the pass is what clears it.
func TestAPassThatGaveTheFileClearsTheAlertEvenWithAHolderThatDidNotAnswerTheQuestion(t *testing.T) {
	r := newRotated(t)
	e := r.e
	r.hold = func() HolderReport {
		cur, _ := e.creds.Digest(r.id)
		rep := NewHolderReport()
		rep.Copies[r.id] = []HolderCopy{{Host: "h2", Generation: 1, Digest: "d1"}, {Host: "h3", Generation: cur.Generation, Digest: cur.Digest}}
		rep.Unreached = []string{"h4"}
		return rep
	}
	if err := e.svc.jobCredentials(e.ctx(), e.term); err != nil {
		t.Fatal(err)
	}
	_ = e.runMonitor()
	if r.given != 1 || e.alerts.severity(AlertFailed, "target:1|credentials") != "" {
		t.Fatalf("given %d: %s", r.given, e.alerts.all())
	}
}

func TestAFailedRedeliveryOfAStaleCopyKeepsTheAlertAndNamesTheHolder(t *testing.T) {
	r := newRotated(t)
	e := r.e
	r.failGive = true
	for i := 0; i < 2; i++ {
		if err := e.svc.jobCredentials(e.ctx(), e.term); err != nil {
			t.Fatal(err)
		}
	}
	_ = e.runMonitor()
	all := e.alerts.all()
	if e.alerts.severity(AlertFailed, "target:1|credentials") != "warning" || !strings.Contains(all, "h2") || strings.Contains(all, "h3") || !strings.Contains(all, "older") {
		t.Fatalf("the alert: %s", all)
	}
}

// ---- P7G-02: no clock, no file system time, no restore from a backup decides which file is the newer one ----

func TestAHolderRestoredFromABackupDoesNotRevertTheRotation(t *testing.T) {
	e := newEnv(t)
	tg := e.addWorm("vault")
	c, _ := e.creds.Get(tg.ID, "worm")
	if _, err := e.creds.Put(tg.ID, c); err != nil { // generation 2: the rotation
		t.Fatal(err)
	}
	own, _ := e.creds.Digest(tg.ID)
	// h3 was restored from a backup that holds generation 1; whatever time its file shows, it reports generation 1
	old, _ := e.creds.Encode(staleCreds(), 1, newStamp(t))
	oldInfo := e.infoOfFile(old, tg.ID)
	e.svc.cfg.Holders = func(context.Context, []int64) (HolderReport, error) {
		rep := NewHolderReport()
		rep.Copies[tg.ID] = []HolderCopy{{Host: "h3", Generation: oldInfo.Generation, Digest: oldInfo.Digest}}
		return rep, nil
	}
	asked := false
	e.svc.cfg.FetchFrom = func(context.Context, string, int64) ([]byte, error) {
		asked = true
		return append([]byte(nil), old...), nil
	}
	var gave []byte
	e.svc.cfg.Distribute = func(_ context.Context, _ int64, _ string, f []byte) error {
		gave = append([]byte(nil), f...)
		return nil
	}
	if err := e.svc.jobCredentials(e.ctx(), e.term); err != nil {
		t.Fatal(err)
	}
	if asked {
		t.Fatal("the leader took the file of a holder that holds an older generation")
	}
	if now, _ := e.creds.Digest(tg.ID); now != own {
		t.Fatalf("the file of the leader changed: %+v", now)
	}
	if len(gave) == 0 || e.infoOfFile(gave, tg.ID) != own {
		t.Fatal("the leader did not give its own file to the holder that was behind")
	}
	// the gate does not take it either
	e.putAgg("shop", 3)
	_ = e.runEnqueue()
	_ = e.runSubmit()
	if asked {
		t.Fatal("the gate took the file of a holder that holds an older generation")
	}
	if rows := e.rows(); len(rows) != 1 || rows[0].State != anchor.StateAnchored {
		t.Fatalf("%+v", rows)
	}
}

func TestTwoFilesOfOneGenerationAreGivenToNobodyAndTheAlertSaysSo(t *testing.T) {
	e := newEnv(t)
	tg := e.addWorm("vault")
	own, _ := e.creds.Digest(tg.ID)
	rival, _ := e.creds.Encode(staleCreds(), own.Generation, newStamp(t))
	ri := e.infoOfFile(rival, tg.ID)
	e.svc.cfg.Holders = func(context.Context, []int64) (HolderReport, error) {
		rep := NewHolderReport()
		rep.Copies[tg.ID] = []HolderCopy{{Host: "h3", Generation: ri.Generation, Digest: ri.Digest}}
		return rep, nil
	}
	asked, gave := false, false
	e.svc.cfg.FetchFrom = func(context.Context, string, int64) ([]byte, error) { asked = true; return nil, errors.New("no") }
	e.svc.cfg.Distribute = func(context.Context, int64, string, []byte) error { gave = true; return nil }
	if err := e.svc.jobCredentials(e.ctx(), e.term); err != nil {
		t.Fatal(err)
	}
	if asked || gave {
		t.Fatalf("a rival file of the same generation was moved (asked %v, gave %v)", asked, gave)
	}
	_ = e.runMonitor()
	all := e.alerts.all()
	if e.alerts.severity(AlertFailed, "target:1|credentials") != "warning" || !strings.Contains(all, "same generation") || !strings.Contains(all, "h3") {
		t.Fatalf("the alert: %s", all)
	}
	// the operator enters the credentials again: the generation is above both and the holders take it
	if _, err := e.svc.UpdateTarget(e.ctx(), tg.ID, api.LedgerAnchorTargetUpdate{Credentials: wormCreds()}); err != nil {
		t.Fatal(err)
	}
	if d, _ := e.creds.Digest(tg.ID); d.Generation != own.Generation+1 {
		t.Fatalf("%+v", d)
	}
}

// ---- a leader that missed a rotation does not anchor with the old credentials ----

func TestALeaderThatMissedARotationTakesTheNewFileBeforeItAnchors(t *testing.T) {
	e, id, raw := staleLeader(t)
	var asked []string
	e.svc.cfg.FetchFrom = func(_ context.Context, host string, tid int64) ([]byte, error) {
		asked = append(asked, host)
		return append([]byte(nil), raw...), nil
	}
	if err := e.runSubmit(); err != nil {
		t.Fatal(err)
	}
	if len(asked) != 1 || asked[0] != "h3" {
		t.Fatalf("asked %v, want h3 once", asked)
	}
	if rows := e.rows(); len(rows) != 1 || rows[0].State != anchor.StateAnchored {
		t.Fatalf("the record was not anchored with the current credentials: %+v", rows)
	}
	for _, r := range e.s3.requests() {
		if strings.Contains(r.header.Get("Authorization"), "an-old-secret") {
			t.Fatal("a request was signed with the old secret")
		}
	}
	c, err := e.creds.Get(id, "worm")
	if err != nil || c["secretAccessKey"].Reveal() != testSecret {
		t.Fatalf("the file of the leader was not replaced: %v", err)
	}
	if d, _ := e.creds.Digest(id); d.Generation != 5 {
		t.Fatalf("%+v", d)
	}
}

func TestALeaderThatMissedARotationAndCannotTakeTheNewFileDoesNotAnchorAndGivesUpAfterTheLimit(t *testing.T) {
	e, _, _ := staleLeader(t)
	e.svc.cfg.FetchFrom = func(context.Context, string, int64) ([]byte, error) { return nil, errors.New("h3 went away") }
	if err := e.runSubmit(); err != nil {
		t.Fatal(err)
	}
	if n := len(e.s3.requests()); n != 0 {
		t.Fatalf("%d requests were made to the store with credentials that a holder has newer", n)
	}
	r := e.rows()[0]
	if r.State == anchor.StateAnchored || r.State == anchor.StateFailed || r.ErrorCode != "credentials_stale" {
		t.Fatalf("the record: state %s code %q: it must wait for the newer file, not fail", r.State, r.ErrorCode)
	}
	// the job says so while it waits
	if err := e.svc.jobCredentials(e.ctx(), e.term); err != nil {
		t.Fatal(err)
	}
	_ = e.runMonitor()
	if !strings.Contains(e.alerts.all(), "asked again every minute") || e.alerts.severity(AlertFailed, "target:1|credentials") != "critical" {
		t.Fatalf("%s", e.alerts.all())
	}
	// after the limit the waiting is over: the records fail for good and the alert says that they are not tried again
	e.clock.Advance(CredStaleLimit + 20*time.Minute)
	if err := e.runSubmit(); err != nil {
		t.Fatal(err)
	}
	r = e.rows()[0]
	if r.State != anchor.StateFailed || r.ErrorCode != "credentials_stale" {
		t.Fatalf("the record after the limit: state %s code %q", r.State, r.ErrorCode)
	}
	if err := e.svc.jobCredentials(e.ctx(), e.term); err != nil {
		t.Fatal(err)
	}
	if p := e.svc.credProblemList()[1]; !strings.Contains(p.Text, "not tried again until Retry") || !p.Critical {
		t.Fatalf("%+v", p)
	}
	if n := len(e.s3.requests()); n != 0 {
		t.Fatalf("%d requests were made", n)
	}
}

// A newer file that cannot be opened here (another passphrase) is final at once: waiting would not help.
func TestANewerFileThatDoesNotOpenHereIsFinalAtOnce(t *testing.T) {
	e, id, _ := staleLeader(t)
	pf := e.dir + "/otherpass"
	if err := os.WriteFile(pf, []byte("another passphrase\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	other, err := NewCredStore(t.TempDir(), pf)
	if err != nil {
		t.Fatal(err)
	}
	other.Iterations, other.MinIterations = 2000, 1000
	valid := Creds{"accessKeyId": anchor.Secret(testKeyID), "secretAccessKey": anchor.Secret(testSecret)}
	sealedFile, err := other.Encode(valid, 5, newStamp(t))
	if err != nil {
		t.Fatal(err)
	}
	e.svc.cfg.FetchFrom = func(context.Context, string, int64) ([]byte, error) { return append([]byte(nil), sealedFile...), nil }
	if err := e.runSubmit(); err != nil {
		t.Fatal(err)
	}
	r := e.rows()[0]
	if r.State != anchor.StateFailed || r.ErrorCode != "credentials_stale" {
		t.Fatalf("state %s code %q: a file that does not open here must not be waited for", r.State, r.ErrorCode)
	}
	if err := e.svc.jobCredentials(e.ctx(), e.term); err != nil {
		t.Fatal(err)
	}
	_ = e.runMonitor()
	if !strings.Contains(e.alerts.all(), "cannot be opened with the passphrase file") {
		t.Fatalf("%s", e.alerts.all())
	}
	if d, _ := e.creds.Digest(id); d.Generation != 2 {
		t.Fatalf("the file of the leader changed: %+v", d)
	}
}

// A holder that says it has a newer file and serves an older one is refused, and the alert says so.
func TestAHolderThatServesAnOlderGenerationThanItSaidIsAnAlert(t *testing.T) {
	e, id, raw := staleLeader(t)
	older, _ := e.creds.Encode(staleCreds(), 1, newStamp(t))
	_ = raw
	e.svc.cfg.FetchFrom = func(context.Context, string, int64) ([]byte, error) { return append([]byte(nil), older...), nil }
	gave := false
	e.svc.cfg.Distribute = func(context.Context, int64, string, []byte) error { gave = true; return nil }
	if err := e.svc.jobCredentials(e.ctx(), e.term); err != nil {
		t.Fatal(err)
	}
	if gave {
		t.Fatal("a leader that could not take the newer file gave its own away")
	}
	_ = e.runMonitor()
	all := e.alerts.all()
	if e.alerts.severity(AlertFailed, "target:1|credentials") != "critical" || !strings.Contains(all, "older than the one of this server") {
		t.Fatalf("%s", all)
	}
	if d, _ := e.creds.Digest(id); d.Generation != 2 {
		t.Fatalf("the file of the leader was replaced by an older one: %+v", d)
	}
}

func TestALeaderWhoseHoldersCannotBeAskedAnchorsWithItsOwnFile(t *testing.T) {
	e, _, _ := staleLeader(t)
	e.svc.cfg.Holders = func(context.Context, []int64) (HolderReport, error) {
		return NewHolderReport(), errors.New("no holder answers")
	}
	called := false
	e.svc.cfg.FetchFrom = func(context.Context, string, int64) ([]byte, error) { called = true; return nil, errors.New("no") }
	if err := e.runSubmit(); err != nil {
		t.Fatal(err)
	}
	if called {
		t.Error("a holder was asked for a file although none could be asked about it")
	}
	if len(e.s3.requests()) == 0 {
		t.Fatal("a leader whose holders do not answer (the old leader is down) must go on with the file it has")
	}
}

func TestTheCredentialsJobOfALeaderThatMissedARotationTakesTheNewFileAndGivesNothingAwayBeforeThat(t *testing.T) {
	e, id, raw := staleLeader(t)
	given := 0
	e.svc.cfg.Distribute = func(context.Context, int64, string, []byte) error { given++; return nil }
	e.svc.cfg.FetchFrom = func(context.Context, string, int64) ([]byte, error) { return append([]byte(nil), raw...), nil }
	// h2 holds generation 3: behind the rotation that h3 has, and newer than the leader's own file. After the leader took the file of h3 it is
	// behind and is given the file in the same pass
	cur := e.infoOfFile(raw, id)
	e.svc.cfg.Holders = func(context.Context, []int64) (HolderReport, error) {
		rep := NewHolderReport()
		rep.Copies[id] = []HolderCopy{{Host: "h3", Generation: cur.Generation, Digest: cur.Digest}, {Host: "h2", Generation: 3, Digest: "d3"}}
		return rep, nil
	}
	if err := e.svc.jobCredentials(e.ctx(), e.term); err != nil {
		t.Fatal(err)
	}
	c, err := e.creds.Get(id, "worm")
	if err != nil || c["secretAccessKey"].Reveal() != testSecret {
		t.Fatalf("the file of the leader was not replaced: %v", err)
	}
	if given != 1 {
		t.Fatalf("the leader gave its file %d times in the pass in which it took the newer one, want 1 (h2 is behind it)", given)
	}
	// when the newer file cannot be taken, the old one is not given to the holders either, and the alert is critical
	given = 0
	if _, err := e.creds.write(id, staleCreds(), 2, newStamp(t)); err != nil {
		t.Fatal(err)
	}
	e.svc.cfg.FetchFrom = func(context.Context, string, int64) ([]byte, error) { return nil, errors.New("h3 went away") }
	if err := e.svc.jobCredentials(e.ctx(), e.term); err != nil {
		t.Fatal(err)
	}
	if given != 0 {
		t.Fatalf("a leader gave away a file that a holder has newer (%d)", given)
	}
	_ = e.runMonitor()
	if e.alerts.severity(AlertFailed, "target:1|credentials") != "critical" || !strings.Contains(e.alerts.all(), "older credentials file") {
		t.Fatalf("the alert: %s", e.alerts.all())
	}
}

// A target whose file this server lacks reaches the fetch through the worker, also when holders are set (the gate must not stop it).
func TestAWorkerWithoutTheFileOfATargetFetchesItThroughTheGate(t *testing.T) {
	e := newEnv(t)
	tg := e.addWorm("vault")
	raw, _ := e.creds.Raw(tg.ID)
	if err := e.creds.Remove(tg.ID); err != nil {
		t.Fatal(err)
	}
	e.svc.cfg.Holders = func(context.Context, []int64) (HolderReport, error) { return NewHolderReport(), nil }
	fetched := 0
	e.svc.cfg.Fetch = func(context.Context, int64) ([]byte, error) { fetched++; return append([]byte(nil), raw...), nil }
	e.putAgg("shop", 3)
	if err := e.runEnqueue(); err != nil {
		t.Fatal(err)
	}
	if err := e.runSubmit(); err != nil {
		t.Fatal(err)
	}
	if fetched != 1 {
		t.Fatalf("the file was fetched %d times, want 1", fetched)
	}
	if rows := e.rows(); len(rows) != 1 || rows[0].State != anchor.StateAnchored {
		t.Fatalf("%+v", rows)
	}
}

// ---- P7G-04: what is a matter of this server is not "no holder has it" ----

func TestAPassphraseFileThatCannotBeReadIsAMatterOfThisServerAndNotNoHolderHavingTheFile(t *testing.T) {
	e := newEnv(t)
	tg := e.addWorm("vault")
	broken, err := NewCredStore(t.TempDir(), e.dir+"/there-is-no-passphrase-file")
	if err != nil {
		t.Fatal(err)
	}
	broken.Iterations, broken.MinIterations = 2000, 1000
	// a file in clear (generation 0, an earlier version) cannot be written under a passphrase that cannot be read
	c0, _ := e.creds.Get(tg.ID, "worm")
	legacy := legacyPlain(c0)
	err = broken.Install(tg.ID, "worm", append([]byte(nil), legacy...))
	if !errors.Is(err, ErrInstallWrite) || !errors.Is(err, ErrPassphraseFile) {
		t.Fatalf("%v", err)
	}
	// nor can a sealed file be opened
	good := newSealedStore(t, e.dir)
	sealed, _ := good.Encode(Creds{"accessKeyId": "AKIAFAKEKEYID000001", "secretAccessKey": "a-secret-of-this-test-0000"}, 3, newStamp(t))
	if err := broken.Install(tg.ID, "worm", sealed); !errors.Is(err, ErrInstallWrite) {
		t.Fatalf("%v", err)
	}
	e.svc.cfg.Creds = broken
	e.svc.cfg.Fetch = func(context.Context, int64) ([]byte, error) { return append([]byte(nil), legacy...), nil }
	trow, _ := e.st.Target(e.ctx(), tg.ID)
	if got := e.svc.fetchMissing(e.ctx(), trow); got != fetchNotInstalled {
		t.Fatalf("fetchMissing says %v, want the failure of this server", got)
	}
}

func newSealedStore(t *testing.T, dir string) *CredStore {
	t.Helper()
	pf := dir + "/sealing-pass"
	if err := os.WriteFile(pf, []byte("a passphrase of this test\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cs, err := NewCredStore(t.TempDir(), pf)
	if err != nil {
		t.Fatal(err)
	}
	cs.Iterations, cs.MinIterations = 2000, 1000
	return cs
}

// ---- P7G-06: what the gate keeps is pruned ----

func TestWhatTheGateKeepsForADeletedTargetIsDropped(t *testing.T) {
	e, id, raw := staleLeader(t)
	e.svc.cfg.FetchFrom = func(context.Context, string, int64) ([]byte, error) { return nil, errors.New("h3 went away") }
	_ = e.runSubmit() // a failure that is noted
	e.svc.mu.Lock()
	n := len(e.svc.credStaleSince)
	e.svc.mu.Unlock()
	if n != 1 {
		t.Fatalf("the failure was not noted (%d)", n)
	}
	e.svc.cfg.FetchFrom = func(context.Context, string, int64) ([]byte, error) { return append([]byte(nil), raw...), nil }
	e.clock.Advance(time.Minute)
	_ = e.runSubmit()
	e.svc.mu.Lock()
	n = len(e.svc.credGate) + len(e.svc.credStaleSince)
	e.svc.mu.Unlock()
	if n != 1 {
		t.Fatalf("the entries after the file was taken: %d, want the one of the gate", n)
	}
	if err := e.svc.DeleteTarget(e.ctx(), id); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.jobCredentials(e.ctx(), e.term); err != nil {
		t.Fatal(err)
	}
	e.svc.mu.Lock()
	n = len(e.svc.credGate) + len(e.svc.credStaleSince)
	e.svc.mu.Unlock()
	if n != 0 {
		t.Fatalf("%d entries are kept for a target that is gone", n)
	}
}

func TestJudgingTheCopiesByGeneration(t *testing.T) {
	own := CopyInfo{Generation: 3, Digest: "aa"}
	h := func(host string, g uint64, d string) HolderCopy {
		return HolderCopy{Host: host, Generation: g, Digest: d}
	}
	newer, stale, conflict, _ := judgeCopies(own, []HolderCopy{h("h2", 3, "aa"), h("h3", 2, "bb"), h("h4", 0, ""), h("h5", 4, "cc"), h("h6", 5, "dd"), h("h7", 3, "ee"), h("h8", 5, "ff")})
	if newer == nil || newer.Host != "h6" {
		t.Fatalf("newer %+v, want the highest generation, the first of two by name (h6)", newer)
	}
	if strings.Join(stale, ",") != "h3,h4" {
		t.Fatalf("stale %v", stale)
	}
	if strings.Join(conflict, ",") != "h7" {
		t.Fatalf("conflict %v: the same generation with another digest", conflict)
	}
	if n, s, k, _ := judgeCopies(own, []HolderCopy{h("h2", 3, "aa")}); n != nil || len(s) != 0 || len(k) != 0 {
		t.Fatalf("the same copy is current: %v %v %v", n, s, k)
	}
	// the number decides, nothing else: a lower generation is stale whatever its digest says
	if n, s, _, _ := judgeCopies(own, []HolderCopy{h("h2", 2, "aa")}); n != nil || len(s) != 1 {
		t.Fatalf("%v %v", n, s)
	}
}

func TestAHolderReportKeepsTheGenerationAndTheDigestOfEachCopy(t *testing.T) {
	rep := NewHolderReport()
	rep.Add("h2", ListResponse{IDs: []int64{1, 2}, Copies: []CopyInfo{{ID: 1, Generation: 4, Digest: "aa"}, {ID: 2, Generation: 0}}}, []int64{1, 2, 3})
	got := rep.Copies[1]
	if len(got) != 1 || got[0] != (HolderCopy{Host: "h2", Generation: 4, Digest: "aa"}) {
		t.Fatalf("%+v", got)
	}
	if len(rep.Lacking[3]) != 1 || rep.Lacking[3][0] != "h2" {
		t.Fatalf("%+v", rep.Lacking)
	}
}

// A newer file that this server cannot store (its passphrase file cannot be read) is waited for, as a full disk is: it is a matter of this server
// and not of the file (P7G-04).
func TestANewerFileThatThisServerCannotStoreIsWaitedFor(t *testing.T) {
	e, _, raw := staleLeader(t)
	e.svc.cfg.FetchFrom = func(context.Context, string, int64) ([]byte, error) { return append([]byte(nil), raw...), nil }
	e.creds.writeErr = errors.New("the disk is full") // the file is still read, and nothing can be written
	if err := e.runSubmit(); err != nil {
		t.Fatal(err)
	}
	r := e.rows()[0]
	if r.State == anchor.StateFailed || r.ErrorCode != "credentials_stale" {
		t.Fatalf("state %s code %q: a failure of this server must be waited for", r.State, r.ErrorCode)
	}
	if err := e.svc.jobCredentials(e.ctx(), e.term); err != nil {
		t.Fatal(err)
	}
	if p := e.svc.credProblemList()[1]; !strings.Contains(p.Text, "asked again every minute") || !strings.Contains(p.Text, "could not be stored here") {
		t.Fatalf("%+v", p)
	}
}

func TestTheNoteOfAFailureToTakeANewerFileIsDroppedWithItsTarget(t *testing.T) {
	e, id, _ := staleLeader(t)
	e.svc.cfg.FetchFrom = func(context.Context, string, int64) ([]byte, error) { return nil, errors.New("h3 went away") }
	_ = e.runSubmit()
	e.svc.mu.Lock()
	n := len(e.svc.credStaleSince)
	e.svc.mu.Unlock()
	if n != 1 {
		t.Fatalf("the failure was not noted (%d)", n)
	}
	if err := e.svc.DeleteTarget(e.ctx(), id); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.jobCredentials(e.ctx(), e.term); err != nil {
		t.Fatal(err)
	}
	e.svc.mu.Lock()
	n = len(e.svc.credStaleSince)
	e.svc.mu.Unlock()
	if n != 0 {
		t.Fatalf("%d notes are kept for a target that is gone", n)
	}
}
