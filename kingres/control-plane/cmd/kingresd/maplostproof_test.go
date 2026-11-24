// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"context"
	"crypto/sha256"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/fwdproof"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/scram"
)

// lostRig is the member s1 that received the operator's request (it signs the proof with the session's client key) and the held-back old leader s2, which checks it
// against the cache of the administrators' keys only: it has no key source, as an old leader whose system database is stopped.
func lostRig(t *testing.T, key []byte) (m *mapFailover, d *haDeps, cache *adminKeys) {
	t.Helper()
	m, _ = abandonRig(t) // the old leader is s2
	m.self = "s1"
	cache = openAdminKeys(t.TempDir(), nil, 0)
	if err := cache.Replace(map[string][]byte{"alice": scram.Stored(key)}); err != nil {
		t.Fatal(err)
	}
	d = &haDeps{ServerHost: "s2", LostIdentityCheck: newIdentityChecker(cache, "s2")}
	return m, d, cache
}

func lostKey(seed byte) []byte {
	key := make([]byte, sha256.Size)
	for i := range key {
		key[i] = byte(i) + seed
	}
	return key
}

// Phase 5 review L2, H1 and M1: the declaration that a server is lost reaches the old leader of the map group, which is held back and can read no verifier, with the
// proof of the operator's session, and is accepted only against the cached key of an administrator. A member that does not hold the session of an administrator cannot make
// it; a proof made for another server, a replay, a changed server name or user and a role below kingres_admin are refused before the hold is looked at.
func TestTheDeclarationThatAServerIsLostIsProvenAtAnOldLeaderThatCannotReadAVerifier(t *testing.T) {
	key := lostKey(7)
	m, d, _ := lostRig(t, key)
	id := opID
	id.Username, id.ProofKey = "alice", key
	ctx := context.Background()
	make1 := func(lost, to string) mapResumeBody {
		b := mapResumeBody{Strict: true, Lost: lost}
		if err := m.signLost(id, &b, to); err != nil {
			t.Fatal(err)
		}
		return b
	}

	// proven by an administrator, with nothing but the cache at the old leader
	if err := openLost(ctx, d, "s1", make1("s9", "s2")); err != nil {
		t.Fatalf("a proven declaration: %v", err)
	}
	// a member that claims it without a proof
	callErr(t, openLost(ctx, d, "s3", mapResumeBody{Strict: true, Lost: "s9", Username: "alice", Roles: id.ClusterRoles}), 401, "identity_unproven")
	// a member that signs for an administrator without his client key (M1): its certificate is not enough
	forged := id
	forged.ProofKey = lostKey(99)
	fb := mapResumeBody{Strict: true, Lost: "s9"}
	if err := m.signLost(forged, &fb, "s2"); err != nil {
		t.Fatal(err)
	}
	callErr(t, openLost(ctx, d, "s1", fb), 401, "identity_unproven")
	// a user that is not an administrator in the cache, with a proof that is right for his own key
	other := id
	other.Username, other.ProofKey = "bob", lostKey(50)
	ob := mapResumeBody{Strict: true, Lost: "s9"}
	if err := m.signLost(other, &ob, "s2"); err != nil {
		t.Fatal(err)
	}
	callErr(t, openLost(ctx, d, "s1", ob), 401, "identity_unproven")
	// made for another server of the cluster, or sent by another member than the one that signed
	callErr(t, openLost(ctx, d, "s1", make1("s9", "s5")), 401, "identity_unproven")
	callErr(t, openLost(ctx, d, "s3", make1("s9", "s2")), 401, "identity_unproven")
	// the declared server, or the user, was changed after it was signed
	c := make1("s9", "s2")
	c.Lost = "s8"
	callErr(t, openLost(ctx, d, "s1", c), 401, "identity_unproven")
	c = make1("s9", "s2")
	c.Username = "mallory"
	callErr(t, openLost(ctx, d, "s1", c), 401, "identity_unproven")
	// a replay
	r := make1("s9", "s2")
	if err := openLost(ctx, d, "s1", r); err != nil {
		t.Fatal(err)
	}
	callErr(t, openLost(ctx, d, "s1", r), 401, "identity_unproven")
	// an operator below kingres_admin: the member does not sign for it, and a body that names a lower role fails the proof
	low := id
	low.ClusterRoles = []string{"kingres_ops"}
	if err := m.signLost(low, &mapResumeBody{Lost: "s9"}, "s2"); err == nil {
		t.Fatal("a proof was made for an operator below kingres_admin")
	}
	lowered := make1("s9", "s2")
	lowered.Roles = []string{"kingres_ops"}
	callErr(t, openLost(ctx, d, "s1", lowered), 401, "identity_unproven")
	// a server that cannot check refuses
	callErr(t, openLost(ctx, &haDeps{ServerHost: "s2"}, "s1", make1("s9", "s2")), 401, "identity_unproven")
	// and the handler asks for it
	d.MapFailover = m
	_, err := handleMapResume(ctx, d, "s3", []byte(`{"strict":true,"lost":"s9"}`))
	callErr(t, err, 401, "identity_unproven")
}

// A server that never cached the keys of the administrators cannot say: 503, not a refusal that looks final. After a change of the password the old proof is refused.
func TestTheCacheOfTheAdministratorsKeys(t *testing.T) {
	var _ fwdproof.KeySource = (*adminKeys)(nil)
	key := lostKey(7)
	m, d, cache := lostRig(t, key)
	id := opID
	id.Username, id.ProofKey = "alice", key
	ctx := context.Background()
	b := mapResumeBody{Strict: true, Lost: "s9"}
	if err := m.signLost(id, &b, "s2"); err != nil {
		t.Fatal(err)
	}
	empty := &haDeps{ServerHost: "s2", LostIdentityCheck: newIdentityChecker(openAdminKeys(t.TempDir(), nil, 0), "s2")}
	callErr(t, openLost(ctx, empty, "s1", b), 503, "unavailable")

	// the file survives a restart of kingresd
	reopened := openAdminKeys(filepath.Dir(cache.path), nil, 0)
	if got, err := reopened.StoredKey(ctx, "alice"); err != nil || len(got) != sha256.Size {
		t.Fatalf("the cache after a restart: %x %v", got, err)
	}
	if _, err := reopened.StoredKey(ctx, "bob"); !errors.Is(err, scram.ErrNotSCRAM) {
		t.Fatalf("a role that is no administrator: %v", err)
	}
	if fi, err := os.Stat(cache.path); err != nil || (os.PathSeparator == '/' && fi.Mode().Perm()&0o077 != 0) {
		t.Fatalf("the cache file: %v %v", fi, err)
	}

	// a changed password: the refresh replaces the key and the old proof stops working
	verifier, err := scram.Verifier("a new password")
	if err != nil {
		t.Fatal(err)
	}
	parsed, _ := scram.Parse(verifier)
	if err := cache.refresh(ctx, func(context.Context) (map[string]string, error) {
		return map[string]string{"alice": verifier, "carol": "md5abc"}, nil
	}); err != nil {
		t.Fatal(err)
	}
	callErr(t, openLost(ctx, d, "s1", b), 401, "identity_unproven")
	if got, _ := cache.StoredKey(ctx, "alice"); string(got) != string(parsed.StoredKey) {
		t.Fatal("the cache did not take the new key")
	}
	if _, err := cache.StoredKey(ctx, "carol"); !errors.Is(err, scram.ErrNotSCRAM) {
		t.Fatalf("a role without a SCRAM verifier is no administrator key: %v", err)
	}
	// a failed read leaves the cache as it is
	if err := cache.refresh(ctx, func(context.Context) (map[string]string, error) { return nil, errors.New("the instance is down") }); err == nil {
		t.Fatal("a failed read was not reported")
	}
	if _, err := cache.StoredKey(ctx, "alice"); err != nil {
		t.Fatalf("a failed read emptied the cache: %v", err)
	}
}
