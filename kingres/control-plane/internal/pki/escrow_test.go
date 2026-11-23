// SPDX-License-Identifier: AGPL-3.0-only

package pki

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// member makes the pki directory of a server that is not the CA holder: ca.crt only.
func member(t *testing.T, ca *CA) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "pki")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, CACertFile), ca.CertPEM(), 0o644); err != nil { //nolint:gosec // a certificate is public
		t.Fatal(err)
	}
	return dir
}

// sealTo makes the recipient's offer and seals the CA to it.
func sealTo(t *testing.T, ca *CA, dir, id string, gen int64, now time.Time) EscrowBlob {
	t.Helper()
	kid, pub, err := RecipientOffer(dir, false, now)
	if err != nil {
		t.Fatal(err)
	}
	b, err := SealCA(ca, id, kid, pub, Position{Epoch: 1, Generation: gen, Seq: int64(now.Sub(t0) / time.Minute)}, now)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestEscrowRoundTripGivesTheNewLeaderTheCA(t *testing.T) {
	ca := newCA(t)
	if err := ca.Revoke("abcd01", t0); err != nil {
		t.Fatal(err)
	}
	dir := member(t, ca)
	if err := StoreEscrow(dir, "b", sealTo(t, ca, dir, "b", 1, t0)); err != nil {
		t.Fatal(err)
	}
	// The distributed list (revoked-serials.json) has a serial the sealed copy does not: it is kept.
	if err := SaveRevokedList(dir, []string{"ff02"}); err != nil {
		t.Fatal(err)
	}
	res, err := AdoptEscrow(dir, "b", t0.Add(time.Hour))
	if err != nil || res != Adopted {
		t.Fatalf("adopt: %v %v", res, err)
	}
	nca, err := LoadCA(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !nca.IsRevoked("abcd01") || !nca.IsRevoked("ff02") {
		t.Errorf("revoked after the adoption: %v", nca.Revoked())
	}
	// The adopted CA signs certificates the old CA's pool accepts.
	names := Names{CommonName: "c.example"}
	_, csr, _ := NewKeyAndCSR(names)
	certPEM, _, err := nca.Sign(csr, names, t0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ca.VerifyPeer(parse(t, certPEM), t0); err != nil {
		t.Errorf("a certificate of the adopted CA is refused by the old one: %v", err)
	}
	if fi, err := os.Stat(filepath.Join(dir, CAKeyFile)); err != nil || fi.Mode().Perm()&0o077 != 0 && !isWindows() {
		t.Errorf("ca.key mode %v %v", fi, err)
	}
	// A second adoption does nothing: a revocation made since is not undone by the older copy.
	if err := nca.Revoke("cafe02", t0.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if res, err := AdoptEscrow(dir, "b", t0.Add(3*time.Hour)); err != nil || res != AdoptHeld {
		t.Errorf("second adopt: %v %v", res, err)
	}
	if again, err := LoadCA(dir); err != nil || !again.IsRevoked("cafe02") {
		t.Errorf("a second adoption undid a revocation: %v", err)
	}
}

func isWindows() bool { return filepath.Separator == '\\' }

func TestAdoptWithoutACopyDoesNothing(t *testing.T) {
	dir := member(t, newCA(t))
	if res, err := AdoptEscrow(dir, "b", t0); err != nil || res != AdoptNone {
		t.Fatalf("%v %v", res, err)
	}
	if _, err := os.Stat(filepath.Join(dir, CAKeyFile)); err == nil {
		t.Error("ca.key written without a copy")
	}
}

func TestBlobDoesNotHoldTheKeyInTheClear(t *testing.T) {
	ca := newCA(t)
	dir := member(t, ca)
	b := sealTo(t, ca, dir, "b", 1, t0)
	raw, _ := json.Marshal(b)
	plain, _ := ca.escrowPlain()
	var pl escrowPlain
	_ = json.Unmarshal(plain, &pl)
	kb := bytes.TrimSpace([]byte(pl.CAKey))
	if bytes.Contains(raw, kb) || bytes.Contains(raw, []byte("EC PRIVATE KEY")) {
		t.Fatal("the key is readable in the blob")
	}
}

func TestHostileBlobs(t *testing.T) {
	ca := newCA(t)
	dir := member(t, ca)
	good := sealTo(t, ca, dir, "b", 2, t0)

	cases := map[string]func(b *EscrowBlob){
		"tampered ciphertext":   func(b *EscrowBlob) { b.Ciphertext[len(b.Ciphertext)-1] ^= 1 },
		"tampered ephemeral":    func(b *EscrowBlob) { b.Ephemeral[0] ^= 1 },
		"another server":        func(b *EscrowBlob) { b.ServerID = "c" },
		"relabelled generation": func(b *EscrowBlob) { b.Generation = 99 },
		"relabelled time":       func(b *EscrowBlob) { b.SealedAt = b.SealedAt.Add(time.Second) },
		"relabelled CA":         func(b *EscrowBlob) { b.CAFingerprint = "00" },
		"unknown version":       func(b *EscrowBlob) { b.Version = 7 },
		"unknown escrow key":    func(b *EscrowBlob) { b.KeyID = "0000000000000000" },
		"truncated":             func(b *EscrowBlob) { b.Ciphertext = b.Ciphertext[:5] },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			b := good
			b.Ephemeral = append([]byte(nil), good.Ephemeral...)
			b.Ciphertext = append([]byte(nil), good.Ciphertext...)
			mutate(&b)
			err := StoreEscrow(dir, "b", b)
			if !errors.Is(err, ErrEscrowRefused) {
				t.Fatalf("stored: %v", err)
			}
			if _, err := HeldEscrow(dir); !errors.Is(err, ErrNoEscrow) {
				t.Fatalf("a refused blob was kept: %v", err)
			}
		})
	}
}

func TestAWrongKeyOpensNothing(t *testing.T) {
	ca := newCA(t)
	dir := member(t, ca)
	b := sealTo(t, ca, dir, "b", 1, t0)
	// Another server's escrow key, under the id the blob names.
	other := member(t, ca)
	if _, _, err := RecipientOffer(other, false, t0); err != nil {
		t.Fatal(err)
	}
	keys, _ := readRecipientKeys(other)
	var wrong recipientKeyFile
	for _, k := range keys {
		wrong = k
	}
	wrong.KeyID = b.KeyID
	if _, err := b.open("b", wrong); !errors.Is(err, ErrEscrowRefused) {
		t.Fatalf("a wrong key opened the blob: %v", err)
	}
}

func TestABlobForAnotherCAIsNotAdopted(t *testing.T) {
	// A blob anybody can make for this server (the escrow public key is public) must not replace the cluster's CA.
	real, evil := newCA(t), newCA(t)
	dir := member(t, real)
	if err := StoreEscrow(dir, "b", sealTo(t, evil, dir, "b", 1, t0)); !errors.Is(err, ErrEscrowRefused) {
		t.Fatalf("stored a foreign CA: %v", err)
	}
	// Even a file put on the disk directly is not adopted.
	b := sealTo(t, evil, dir, "b", 1, t0)
	raw, _ := json.Marshal(b)
	if err := os.WriteFile(filepath.Join(dir, EscrowDir, EscrowBlobFile), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if res, err := AdoptEscrow(dir, "b", t0); !errors.Is(err, ErrEscrowRefused) || res == Adopted {
		t.Fatalf("adopted a foreign CA: %v %v", res, err)
	}
	if _, err := os.Stat(filepath.Join(dir, CAKeyFile)); err == nil {
		t.Error("ca.key written from a foreign CA")
	}
}

func TestAnOlderBlobDoesNotReplaceANewerOne(t *testing.T) {
	ca := newCA(t)
	dir := member(t, ca)
	old := sealTo(t, ca, dir, "b", 1, t0)
	if err := StoreEscrow(dir, "b", sealTo(t, ca, dir, "b", 2, t0)); err != nil {
		t.Fatal(err)
	}
	if err := StoreEscrow(dir, "b", old); !errors.Is(err, ErrEscrowRefused) {
		t.Fatalf("replayed an older generation: %v", err)
	}
	if h, _ := HeldEscrow(dir); h.Position.Generation != 2 {
		t.Errorf("held generation %d", h.Position.Generation)
	}
	// Same generation, later seal (a revocation changed the content): accepted.
	if err := StoreEscrow(dir, "b", sealTo(t, ca, dir, "b", 2, t0.Add(time.Minute))); err != nil {
		t.Fatal(err)
	}
}

func TestRotationReplacesTheEscrowKeyAndTheOldOneStopsWorking(t *testing.T) {
	ca := newCA(t)
	dir := member(t, ca)
	first := sealTo(t, ca, dir, "b", 1, t0)
	if err := StoreEscrow(dir, "b", first); err != nil {
		t.Fatal(err)
	}
	kid, pub, err := RecipientOffer(dir, true, t0.Add(time.Hour))
	if err != nil || kid == first.KeyID {
		t.Fatalf("rotation: %v %v", kid, err)
	}
	// Until the new blob arrives the old copy still opens (a failover in between still works).
	if res, err := AdoptEscrow(member2(t, dir, ca), "b", t0); err != nil || res != Adopted {
		t.Fatalf("old copy before the new one: %v %v", res, err)
	}
	second, err := SealCA(ca, "b", kid, pub, Position{Epoch: 1, Generation: 2}, t0.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if err := StoreEscrow(dir, "b", second); err != nil {
		t.Fatal(err)
	}
	keys, _ := readRecipientKeys(dir)
	if _, ok := keys[first.KeyID]; ok || len(keys) != 1 {
		t.Errorf("the old escrow key is still there: %d keys", len(keys))
	}
	// The old blob no longer opens on this server.
	if err := StoreEscrow(dir, "b", first); !errors.Is(err, ErrEscrowRefused) {
		t.Errorf("old blob accepted: %v", err)
	}
}

// member2 copies a server's escrow files to a fresh pki directory, to try an adoption without disturbing the original.
func member2(t *testing.T, from string, ca *CA) string {
	t.Helper()
	to := member(t, ca)
	if err := os.CopyFS(filepath.Join(to, EscrowDir), os.DirFS(filepath.Join(from, EscrowDir))); err != nil {
		t.Fatal(err)
	}
	return to
}

func TestDropRemovesTheCopyAndTheKeys(t *testing.T) {
	ca := newCA(t)
	dir := member(t, ca)
	if err := StoreEscrow(dir, "b", sealTo(t, ca, dir, "b", 1, t0)); err != nil {
		t.Fatal(err)
	}
	if err := DropEscrow(dir); err != nil {
		t.Fatal(err)
	}
	if _, err := HeldEscrow(dir); !errors.Is(err, ErrNoEscrow) {
		t.Fatalf("copy kept: %v", err)
	}
	if keys, _ := readRecipientKeys(dir); len(keys) != 0 {
		t.Fatal("keys kept")
	}
	if res, _ := AdoptEscrow(dir, "b", t0); res == Adopted {
		t.Fatal("adopted after a drop")
	}
	if err := DropEscrow(dir); err != nil {
		t.Fatalf("a second drop: %v", err)
	}
}

func TestContentHashFollowsTheRevocationList(t *testing.T) {
	ca := newCA(t)
	h1, _ := ca.EscrowContentHash()
	h1b, _ := ca.EscrowContentHash()
	if h1 != h1b {
		t.Fatal("the hash is not stable")
	}
	if err := ca.Revoke("aa11", t0); err != nil {
		t.Fatal(err)
	}
	if h2, _ := ca.EscrowContentHash(); h2 == h1 {
		t.Fatal("a revocation does not change the hash")
	}
}

func TestSealRefusesAKeyIDThatIsNotTheKeys(t *testing.T) {
	ca := newCA(t)
	_, pub, _ := RecipientOffer(member(t, ca), false, t0)
	if _, err := SealCA(ca, "b", "0000000000000000", pub, Position{Epoch: 1, Generation: 1}, t0); err == nil {
		t.Fatal("sealed to a mismatching key id")
	}
	if _, err := SealCA(ca, "b", KeyIDOf([]byte{1, 2}), []byte{1, 2}, Position{Epoch: 1, Generation: 1}, t0); err == nil {
		t.Fatal("sealed to a short key")
	}
}

func TestOfferIsStableUntilRotated(t *testing.T) {
	dir := member(t, newCA(t))
	k1, p1, _ := RecipientOffer(dir, false, t0)
	k2, p2, _ := RecipientOffer(dir, false, t0.Add(time.Minute))
	if k1 != k2 || !bytes.Equal(p1, p2) {
		t.Fatal("the offer changed without a rotation")
	}
}

func TestEscrowStateRoundTrip(t *testing.T) {
	dir := member(t, newCA(t))
	if st, err := LoadEscrowState(dir); err != nil || st.Enabled || st.Recipients == nil {
		t.Fatalf("empty: %+v %v", st, err)
	}
	st := EscrowState{Enabled: true, Generation: 3, Recipients: map[string]EscrowRecipientState{"b": {KeyID: "k", Generation: 3}}}
	if err := SaveEscrowState(dir, st); err != nil {
		t.Fatal(err)
	}
	got, err := LoadEscrowState(dir)
	if err != nil || !got.Enabled || got.Generation != 3 || got.Recipients["b"].KeyID != "k" {
		t.Fatalf("%+v %v", got, err)
	}
}

// A blob that opens (it was sealed to this server's public key) but is labelled for another server is refused: the label
// is checked as well as the additional data.
func TestABlobLabelledForAnotherServerIsRefused(t *testing.T) {
	ca := newCA(t)
	dir := member(t, ca)
	kid, pub, err := RecipientOffer(dir, false, t0)
	if err != nil {
		t.Fatal(err)
	}
	b, err := SealCA(ca, "c", kid, pub, Position{Epoch: 1, Generation: 1}, t0) // sealed to b's key, labelled c
	if err != nil {
		t.Fatal(err)
	}
	if err := StoreEscrow(dir, "b", b); !errors.Is(err, ErrEscrowRefused) {
		t.Fatalf("stored a blob labelled for another server: %v", err)
	}
}
