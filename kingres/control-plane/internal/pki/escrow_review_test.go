// SPDX-License-Identifier: AGPL-3.0-only

package pki

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/hkdf"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func sealAt(t *testing.T, ca *CA, dir, id string, pos Position, now time.Time) EscrowBlob {
	t.Helper()
	kid, pub, err := RecipientOffer(dir, false, now)
	if err != nil {
		t.Fatal(err)
	}
	b, err := SealCA(ca, id, kid, pub, pos, now)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// CAE-1: the order of the copies is (epoch, generation, seq); the wall clock orders nothing.
func TestAFormerLeadersBlobIsRefusedWhateverItsClock(t *testing.T) {
	ca := newCA(t)
	dir := member(t, ca)
	if err := StoreEscrow(dir, "b", sealAt(t, ca, dir, "b", Position{Epoch: 2, Generation: 5, Seq: 1}, t0)); err != nil {
		t.Fatal(err)
	}
	stale := sealAt(t, ca, dir, "b", Position{Epoch: 1, Generation: 5, Seq: 99}, t0.Add(time.Hour))
	if err := StoreEscrow(dir, "b", stale); !errors.Is(err, ErrEscrowRefused) {
		t.Fatalf("a blob of an older epoch with a later clock was stored: %v", err)
	}
	// The new leader is not refused because its clock is behind: a higher epoch wins at an earlier time.
	if err := StoreEscrow(dir, "b", sealAt(t, ca, dir, "b", Position{Epoch: 3, Generation: 1, Seq: 1}, t0.Add(-time.Hour))); err != nil {
		t.Fatalf("a newer epoch with a clock that is behind was refused: %v", err)
	}
	if h, _ := HeldEscrow(dir); h.Position != (Position{Epoch: 3, Generation: 1, Seq: 1}) {
		t.Fatalf("held %+v", h.Position)
	}
	// The same position again (a resend) is accepted.
	if err := StoreEscrow(dir, "b", sealAt(t, ca, dir, "b", Position{Epoch: 3, Generation: 1, Seq: 1}, t0)); err != nil {
		t.Fatalf("a resend: %v", err)
	}
}

func TestPositionOrder(t *testing.T) {
	order := []Position{{1, 1, 1}, {1, 1, 2}, {1, 2, 0}, {2, 0, 0}, {2, 0, 1}}
	for i := range order {
		for j := range order {
			if (i < j) != order[i].Less(order[j]) {
				t.Errorf("%v < %v is %v", order[i], order[j], order[i].Less(order[j]))
			}
		}
	}
}

// CAE-5: a ca.key that was cut short is not "key held".
func TestATruncatedCAKeyIsReplacedByTheCopy(t *testing.T) {
	ca := newCA(t)
	dir := member(t, ca)
	if err := StoreEscrow(dir, "b", sealTo(t, ca, dir, "b", 1, t0)); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, CAKeyFile), []byte("-----BEGIN EC PRIV"), 0o600); err != nil {
		t.Fatal(err)
	}
	res, err := AdoptEscrow(dir, "b", t0)
	if err != nil || res != Adopted {
		t.Fatalf("adopt over a truncated key: %v %v", res, err)
	}
	if _, err := LoadCA(dir); err != nil {
		t.Fatalf("the adopted key does not load: %v", err)
	}
	if m, _ := filepath.Glob(filepath.Join(dir, CAKeyFile+".unusable-*")); len(m) != 1 {
		t.Fatalf("the truncated key was not put aside: %v", m)
	}
}

func TestATruncatedCAKeyWithoutACopyIsNoKey(t *testing.T) {
	dir := member(t, newCA(t))
	if err := os.WriteFile(filepath.Join(dir, CAKeyFile), []byte("-----BEGIN EC PRIV"), 0o600); err != nil {
		t.Fatal(err)
	}
	if res, err := AdoptEscrow(dir, "b", t0); err != nil || res != AdoptNone {
		t.Fatalf("%v %v", res, err)
	}
	if _, err := os.Stat(filepath.Join(dir, CAKeyFile)); err == nil {
		t.Fatal("a truncated ca.key is still where a key is looked for")
	}
}

// CAE-5: the key is written durably: the files are flushed (a plain create is not).
func TestTheAdoptedKeyIsFlushedToDisk(t *testing.T) {
	ca := newCA(t)
	dir := member(t, ca)
	if err := StoreEscrow(dir, "b", sealTo(t, ca, dir, "b", 1, t0)); err != nil {
		t.Fatal(err)
	}
	files := 0
	old := syncFile
	syncFile = func(f *os.File) error { files++; return old(f) }
	defer func() { syncFile = old }()
	if _, err := AdoptEscrow(dir, "b", t0); err != nil {
		t.Fatal(err)
	}
	if files < 2 { // revoked.json and ca.key
		t.Fatalf("%d files were flushed", files)
	}
}

func mustKeyPEM(t *testing.T, ca *CA) string {
	t.Helper()
	p, err := ca.escrowPlain()
	if err != nil {
		t.Fatal(err)
	}
	var pl escrowPlain
	if err := json.Unmarshal(p, &pl); err != nil {
		t.Fatal(err)
	}
	return pl.CAKey
}

// CAE-7: what is stored is parsed, so a copy that cannot be adopted is refused now and not at the failover.
func TestACopyWithAnUnreadableRevocationListIsRefused(t *testing.T) {
	ca := newCA(t)
	dir := member(t, ca)
	kid, pub, _ := RecipientOffer(dir, false, t0)
	plain, _ := json.Marshal(map[string]any{"caKey": mustKeyPEM(t, ca), "revoked": "not a list"})
	b, err := sealPlain(plain, ca.CAFingerprint(), "b", kid, pub, Position{Epoch: 1, Generation: 1}, t0)
	if err != nil {
		t.Fatal(err)
	}
	if err := StoreEscrow(dir, "b", b); !errors.Is(err, ErrEscrowRefused) {
		t.Fatalf("stored a copy with an unreadable list: %v", err)
	}
}

// CAE-7: an unreadable held copy does not block a good one.
func TestAnUnreadableHeldCopyDoesNotBlockAGoodOne(t *testing.T) {
	ca := newCA(t)
	dir := member(t, ca)
	if err := os.MkdirAll(escrowDir(dir), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(escrowDir(dir), EscrowBlobFile), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := StoreEscrow(dir, "b", sealTo(t, ca, dir, "b", 1, t0)); err != nil {
		t.Fatalf("a good copy over an unreadable one: %v", err)
	}
}

// CAE-8 (M12): the key file must be private; the check is for systems that have modes.
func TestAnEscrowKeyFileThatOthersCanReadIsRefused(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no file modes")
	}
	dir := member(t, newCA(t))
	if _, _, err := RecipientOffer(dir, false, t0); err != nil {
		t.Fatal(err)
	}
	m, _ := filepath.Glob(filepath.Join(escrowDir(dir), "recipient-*.key"))
	if len(m) != 1 {
		t.Fatal(m)
	}
	if fi, err := os.Stat(m[0]); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode of a new key file: %v %v", fi, err)
	}
	if err := os.Chmod(m[0], 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := RecipientOffer(dir, false, t0); err == nil {
		t.Fatal("a key file readable by others was used")
	}
}

// CAE-8: the fields of the additional data cannot shift into one another.
func TestAdditionalDataFieldsDoNotShift(t *testing.T) {
	a := EscrowBlob{Version: 2, ServerID: "a", KeyID: "bc", CAFingerprint: "d"}
	b := EscrowBlob{Version: 2, ServerID: "ab", KeyID: "c", CAFingerprint: "d"}
	c := EscrowBlob{Version: 2, ServerID: "a|bc", KeyID: "", CAFingerprint: "d"}
	if string(a.aad()) == string(b.aad()) || string(a.aad()) == string(c.aad()) {
		t.Fatal("two different headers have the same additional data")
	}
}

// M2: the CA fingerprint is part of what the tag covers (the check against ca.crt would catch a relabel as well).
func TestTheTagCoversTheCAFingerprint(t *testing.T) {
	ca := newCA(t)
	dir := member(t, ca)
	b := sealTo(t, ca, dir, "b", 1, t0)
	keys, _ := readRecipientKeys(dir)
	b.CAFingerprint = "00" + b.CAFingerprint[2:]
	if _, err := b.open("b", keys[b.KeyID]); !errors.Is(err, ErrEscrowRefused) {
		t.Fatalf("a blob with another CA fingerprint opened: %v", err)
	}
}

// The key is HKDF-SHA256 of the shared secret, salted with the ephemeral and the recipient public keys, with the label as
// info: an independent derivation opens the blob.
func TestTheKeyIsDerivedAsDocumented(t *testing.T) {
	ca := newCA(t)
	dir := member(t, ca)
	b := sealTo(t, ca, dir, "b", 1, t0)
	keys, _ := readRecipientKeys(dir)
	priv, _ := ecdh.X25519().NewPrivateKey(keys[b.KeyID].Private)
	epub, _ := ecdh.X25519().NewPublicKey(b.Ephemeral)
	shared, _ := priv.ECDH(epub)
	salt := append(append([]byte{}, b.Ephemeral...), priv.PublicKey().Bytes()...)
	key, err := hkdf.Key(sha256.New, shared, salt, "kingres ca escrow v1", 32)
	if err != nil {
		t.Fatal(err)
	}
	blk, _ := aes.NewCipher(key)
	gcm, _ := cipher.NewGCM(blk)
	if _, err := gcm.Open(nil, b.Ciphertext[:gcm.NonceSize()], b.Ciphertext[gcm.NonceSize():], b.aad()); err != nil {
		t.Fatalf("the documented derivation does not open the blob: %v", err)
	}
}

// A nonce is never used twice.
func TestTwoSealsDoNotShareANonce(t *testing.T) {
	ca := newCA(t)
	dir := member(t, ca)
	a := sealTo(t, ca, dir, "b", 1, t0)
	b := sealTo(t, ca, dir, "b", 1, t0)
	if string(a.Ciphertext[:12]) == string(b.Ciphertext[:12]) || string(a.Ephemeral) == string(b.Ephemeral) {
		t.Fatal("two seals share a nonce or an ephemeral key")
	}
}

func TestHeldRevokedOpensTheCopy(t *testing.T) {
	ca := newCA(t)
	if err := ca.Revoke("ab12", t0); err != nil {
		t.Fatal(err)
	}
	dir := member(t, ca)
	if _, err := HeldRevoked(dir, "b"); !errors.Is(err, ErrNoEscrow) {
		t.Fatalf("%v", err)
	}
	if err := StoreEscrow(dir, "b", sealTo(t, ca, dir, "b", 1, t0)); err != nil {
		t.Fatal(err)
	}
	got, err := HeldRevoked(dir, "b")
	if err != nil || len(got) != 1 || got[0] != "ab12" {
		t.Fatalf("%v %v", got, err)
	}
}

// forgeCRL signs a list with the given serials and number (a former leader that merged nothing).
func forgeCRL(t *testing.T, ca *CA, serials []string, number int64) ([]byte, error) {
	t.Helper()
	c := &CA{dir: t.TempDir(), cert: ca.cert, pem: ca.pem, key: ca.key, revoked: map[string]time.Time{}, crlNum: number - 1}
	for _, s := range serials {
		c.revoked[s] = t0
	}
	return c.CRL(t0)
}

// CAE-4: a revocation list never goes back.
func TestACRLThatTakesARevocationBackIsStale(t *testing.T) {
	ca := newCA(t)
	if err := ca.Revoke("aa01", t0); err != nil {
		t.Fatal(err)
	}
	old, err := ca.CRL(t0)
	if err != nil {
		t.Fatal(err)
	}
	if err := ca.Revoke("bb02", t0); err != nil {
		t.Fatal(err)
	}
	newer, err := ca.CRL(t0)
	if err != nil {
		t.Fatal(err)
	}
	if err := CheckNotStale(old, newer); err != nil {
		t.Fatalf("a list with more revocations and a higher number: %v", err)
	}
	if err := CheckNotStale(newer, newer); err != nil {
		t.Fatalf("the same list again: %v", err)
	}
	if err := CheckNotStale(newer, old); !errors.Is(err, ErrStaleRevocations) {
		t.Fatalf("the older list over the newer: %v", err)
	}
	// A higher number that lacks a held serial: a leader that came back with an old list.
	stale, err := forgeCRL(t, ca, []string{"aa01"}, 99)
	if err != nil {
		t.Fatal(err)
	}
	if err := CheckNotStale(newer, stale); !errors.Is(err, ErrStaleRevocations) {
		t.Fatalf("a higher number that drops a serial: %v", err)
	}
	if err := CheckNotStale([]byte("garbage"), newer); err != nil {
		t.Fatalf("an unreadable held list must not block: %v", err)
	}
}

func TestMergeRevokedAndTheCRLNumber(t *testing.T) {
	ca := newCA(t)
	if n, err := ca.MergeRevoked([]string{"aa01", "AA01", "bb02", "zz", ""}, t0); err != nil || n != 2 {
		t.Fatalf("%d %v", n, err)
	}
	if n, _ := ca.MergeRevoked([]string{"aa01"}, t0); n != 0 {
		t.Fatal("a known serial was added again")
	}
	if err := ca.EnsureCRLNumberAbove(40); err != nil {
		t.Fatal(err)
	}
	crl, _ := ca.CRL(t0)
	n1, s, err := CRLContent(crl)
	if err != nil || n1 <= 40 || !s["aa01"] || !s["bb02"] {
		t.Fatalf("%d %v %v", n1, s, err)
	}
	if err := ca.EnsureCRLNumberAbove(3); err != nil {
		t.Fatal(err)
	}
	crl2, _ := ca.CRL(t0)
	if n2, _, _ := CRLContent(crl2); n2 <= n1 {
		t.Fatalf("the number did not go up: %d then %d", n1, n2)
	}
	// The number is at least the time in seconds: the lists of two holders of the key are in the order they were issued in.
	if n1 < t0.Unix() {
		t.Fatalf("number %d below the time", n1)
	}
	if err := ca.EnsureCRLNumberAbove(n1 + 1000); err != nil {
		t.Fatal(err)
	}
	crl3, _ := ca.CRL(t0)
	if n3, _, _ := CRLContent(crl3); n3 <= n1+1000 {
		t.Fatalf("the number is not above the floor: %d", n3)
	}
	again, err := LoadCA(ca.dir)
	if err != nil || !again.IsRevoked("bb02") {
		t.Fatalf("%v", err)
	}
}

func TestUnionSerials(t *testing.T) {
	got := UnionSerials([]string{"B", "a"}, nil, []string{"a", "c", ""})
	if len(got) != 3 || got[0] != "a" || got[1] != "b" || got[2] != "c" {
		t.Fatalf("%v", got)
	}
}

// An adoption keeps the revocations of the published list and its number.
func TestAdoptionKeepsThePublishedCRLNumber(t *testing.T) {
	ca := newCA(t)
	dir := member(t, ca)
	if err := StoreEscrow(dir, "b", sealTo(t, ca, dir, "b", 1, t0)); err != nil {
		t.Fatal(err)
	}
	if err := ca.EnsureCRLNumberAbove(20); err != nil {
		t.Fatal(err)
	}
	if err := ca.Revoke("cc03", t0); err != nil {
		t.Fatal(err)
	}
	crl, _ := ca.CRL(t0)
	if err := os.WriteFile(filepath.Join(dir, CRLFile), crl, 0o644); err != nil { //nolint:gosec // public
		t.Fatal(err)
	}
	if _, err := AdoptEscrow(dir, "b", t0); err != nil {
		t.Fatal(err)
	}
	nca, err := LoadCA(dir)
	if err != nil {
		t.Fatal(err)
	}
	next, _ := nca.CRL(t0)
	if n, s, _ := CRLContent(next); n <= 21 || !s["cc03"] {
		t.Fatalf("the adopted CA issues number %d, revoked %v", n, s)
	}
}
