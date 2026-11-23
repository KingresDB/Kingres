// SPDX-License-Identifier: AGPL-3.0-only

package membership

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/internalapi"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/pki"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/wire"
)

func (c *escrowCluster) hasCopy(id string) bool {
	_, err := held(c.servers[id])
	return err == nil
}

func (c *escrowCluster) mgrCA(id string) *pki.CA {
	c.t.Helper()
	ca, err := c.servers[id].Svc.CA()
	if err != nil {
		c.t.Fatalf("%s holds no CA: %v", id, err)
	}
	return ca
}

func (c *escrowCluster) sync(id string) {
	c.t.Helper()
	if err := c.servers[id].SyncEscrow(context.Background()); err != nil {
		c.t.Fatal(err)
	}
}

// crlSerials reads the serials of the revocation list file a server holds.
func (c *escrowCluster) crlSerials(id string) map[string]bool {
	c.t.Helper()
	b, err := os.ReadFile(filepath.Join(c.servers[id].Svc.PKIDir(), pki.CRLFile))
	if err != nil {
		return nil
	}
	_, s, err := pki.CRLContent(b)
	if err != nil {
		c.t.Fatal(err)
	}
	return s
}

// CAE-6: a standby that is missing from the list for a moment keeps its copy; one that is missing for good loses it.
func TestAStandbyThatIsMissingForAMomentKeepsItsCopy(t *testing.T) {
	c := newEscrowCluster(t, "db1", "db2", "db3")
	want := []string{"db2", "db3"}
	c.leader().SetEscrowRecipients(func(context.Context) ([]string, error) { return want, nil })
	if _, err := c.leader().SetEscrow(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	want = nil
	c.sync("db1")
	c.sync("db1")
	if !c.hasCopy("db2") || !c.hasCopy("db3") {
		t.Fatal("a copy was deleted after two rounds without the standby")
	}
	want = []string{"db2", "db3"} // back: the count starts again
	c.sync("db1")
	want = nil
	c.sync("db1")
	c.sync("db1")
	if !c.hasCopy("db2") {
		t.Fatal("the count of missed rounds was not reset when the standby came back")
	}
	c.sync("db1")
	if c.hasCopy("db2") || c.hasCopy("db3") {
		t.Fatal("a copy stayed after the standby was gone for good")
	}
}

// CAE-3 (a): the new leader remembers who held a copy and takes it away from a server that no longer qualifies.
func TestTheNewLeaderDropsTheCopiesOfServersThatNoLongerQualify(t *testing.T) {
	c := newEscrowCluster(t, "db1", "db2", "db3", "db4")
	ctx := context.Background()
	if _, err := c.leader().SetEscrow(ctx, true); err != nil {
		t.Fatal(err)
	}
	if !c.hasCopy("db4") {
		t.Fatal("db4 has no copy")
	}
	c.down["db1"] = true
	c.active = []string{"db2", "db3"} // db4 was removed while the leader was failing
	if ok, err := c.servers["db2"].AdoptEscrow(ctx); err != nil || !ok {
		t.Fatalf("adopt: %v %v", ok, err)
	}
	c.sync("db2")
	if c.hasCopy("db4") {
		t.Fatal("the new leader did not delete the copy of a server that no longer qualifies")
	}
	if !c.hasCopy("db3") {
		t.Fatal("db3 lost its copy")
	}
	if st := c.status(c.servers["db2"]); !st.enabled || !st.current["db3"] || len(st.states) != 1 {
		t.Fatalf("status %+v", st)
	}
}

// CAE-3 (b): a copy that stayed on a server that was down when the escrow was switched off does not switch it on again.
func TestAStaleCopyDoesNotSwitchTheEscrowBackOn(t *testing.T) {
	c := newEscrowCluster(t, "db1", "db2", "db3")
	ctx := context.Background()
	if _, err := c.leader().SetEscrow(ctx, true); err != nil {
		t.Fatal(err)
	}
	c.down["db2"] = true
	if _, err := c.leader().SetEscrow(ctx, false); err != nil {
		t.Fatal(err)
	}
	if !c.hasCopy("db2") || c.hasCopy("db3") {
		t.Fatal("setup: db2 should be the one stale copy")
	}
	// The leader fails; db2 comes back and becomes the leader with the stale copy.
	c.down["db1"], c.down["db2"] = true, false
	stores := 0
	inner := c.servers["db2"].escrowTransport
	c.servers["db2"].escrowTransport = func(ctx context.Context, id, method, path string, in, out any) error {
		if path == "/pki/escrow/store" {
			stores++
		}
		return inner(ctx, id, method, path, in, out)
	}
	if ok, err := c.servers["db2"].AdoptEscrow(ctx); err != nil || !ok {
		t.Fatalf("adopt: %v %v", ok, err)
	}
	c.sync("db2")
	if st := c.status(c.servers["db2"]); st.enabled {
		t.Fatal("the escrow was switched on again by a stale copy")
	}
	if stores != 0 {
		t.Fatalf("the new leader sealed %d copies after the operator switched the escrow off", stores)
	}
	if c.hasCopy("db2") || c.hasCopy("db3") {
		t.Fatal("a copy was left after the new leader's round")
	}
}

// CAE-3: with no record of the operator's choice a copy means the escrow was on, and the new leader goes on with it.
func TestWithoutARecordACopyMeansTheEscrowWasOn(t *testing.T) {
	c := newEscrowCluster(t, "db1", "db2", "db3")
	ctx := context.Background()
	if _, err := c.leader().SetEscrow(ctx, true); err != nil {
		t.Fatal(err)
	}
	c.db.mu.Lock()
	c.db.has = false
	c.db.mu.Unlock()
	c.down["db1"] = true
	if ok, err := c.servers["db2"].AdoptEscrow(ctx); err != nil || !ok {
		t.Fatalf("%v %v", ok, err)
	}
	if st := c.status(c.servers["db2"]); !st.enabled {
		t.Fatal("the escrow is off after the failover")
	}
}

// CAE-1: a leader of an older epoch seals nothing over a newer one, and its drop is refused.
func TestAFormerLeaderCannotOverrideANewerOne(t *testing.T) {
	c := newEscrowCluster(t, "db1", "db2", "db3")
	ctx := context.Background()
	if _, err := c.leader().SetEscrow(ctx, true); err != nil {
		t.Fatal(err)
	}
	if ok, err := c.servers["db2"].AdoptEscrow(ctx); err != nil || !ok {
		t.Fatalf("adopt: %v %v", ok, err)
	}
	c.sync("db2")
	h1, _ := held(c.servers["db3"])
	if h1.Position.Epoch < 2 {
		t.Fatalf("the new leader sealed epoch %d", h1.Position.Epoch)
	}
	// The former leader is still running with its key (a leader that has not heard that it is replaced) and sends again.
	lca := c.mgrCA("db1")
	if _, err := lca.MergeRevoked([]string{"dead01"}, time.Now()); err != nil { // its content changed: it wants to resend
		t.Fatal(err)
	}
	c.sync("db1")
	h2, _ := held(c.servers["db3"])
	if h2.Position != h1.Position {
		t.Fatalf("a former leader replaced the copy of the newer one: %+v -> %+v", h1.Position, h2.Position)
	}
	if st := c.status(c.leader()); st.states["db3"] == "current" {
		t.Fatalf("the former leader believes its copy is current: %+v", st)
	}
	// And it cannot make a server delete its copy.
	if _, err := c.leader().SetEscrow(ctx, false); err != nil {
		t.Fatal(err)
	}
	if !c.hasCopy("db3") {
		t.Fatal("a former leader deleted the copy of the newer leader's epoch")
	}
}

func TestTheOperatorsRotationTakesAnEpochAboveWhatTheServersHold(t *testing.T) {
	c := newEscrowCluster(t, "db1", "db2")
	ctx := context.Background()
	if _, err := c.leader().SetEscrow(ctx, true); err != nil {
		t.Fatal(err)
	}
	// Another leader sealed epoch 9 and the record of it was lost.
	kid, pub, err := pki.RecipientOffer(c.servers["db2"].Svc.PKIDir(), false, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	b, err := pki.SealCA(c.mgrCA("db1"), "db2", kid, pub, pki.Position{Epoch: 9, Generation: 1, Seq: 1}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err := pki.StoreEscrow(c.servers["db2"].Svc.PKIDir(), "db2", b); err != nil {
		t.Fatal(err)
	}
	if _, err := c.mgrCA("db1").MergeRevoked([]string{"dd04"}, time.Now()); err != nil { // its content changed: it wants to resend
		t.Fatal(err)
	}
	c.sync("db1")
	if st := c.status(c.leader()); st.states["db2"] == "current" {
		t.Fatalf("a leader of a lower epoch overrode a copy: %+v", st)
	}
	st, err := c.leader().RotateEscrow(ctx)
	if err != nil || !st.Complete {
		t.Fatalf("rotation: %+v %v", st, err)
	}
	if h, _ := held(c.servers["db2"]); h.Position.Epoch <= 9 {
		t.Fatalf("the rotation sealed epoch %d", h.Position.Epoch)
	}
}

// CAE-4: the server that comes back with an old revocation list must not undo what was revoked while it was away.
func TestAReturningLeaderMergesWhatTheServersHoldBeforeItDistributes(t *testing.T) {
	c := newEscrowCluster(t, "db1", "db2", "db3")
	ctx := context.Background()
	if _, err := c.leader().SetEscrow(ctx, true); err != nil {
		t.Fatal(err)
	}
	ca1 := c.mgrCA("db1")
	if err := ca1.Revoke("aa01", time.Now()); err != nil {
		t.Fatal(err)
	}
	// db1 fails; db2 takes over, revokes bb02 and distributes. db1 is down: it misses the list.
	c.down["db1"] = true
	if ok, err := c.servers["db2"].AdoptEscrow(ctx); err != nil || !ok {
		t.Fatalf("adopt: %v %v", ok, err)
	}
	ca2 := c.mgrCA("db2")
	if err := ca2.Revoke("bb02", time.Now()); err != nil {
		t.Fatal(err)
	}
	// db1 is down: the first error is about it, the others got the list.
	if err := c.servers["db2"].distributeRevocations(ctx); err == nil {
		t.Fatal("setup: the distribution should report the server that is down")
	}
	if !c.crlSerials("db3")["bb02"] {
		t.Fatal("setup: db3 did not get the revocation of db2")
	}
	if ca1.IsRevoked("bb02") {
		t.Fatal("setup: db1 should not know bb02")
	}
	// db1 returns and becomes the leader again (a second failover): it holds ca.key and an old list.
	c.down["db1"] = false
	c.down["db2"] = true
	if ok, err := c.servers["db1"].AdoptEscrow(ctx); err != nil || !ok {
		t.Fatalf("a returning leader: %v %v", ok, err)
	}
	if !c.mgrCA("db1").IsRevoked("bb02") {
		t.Fatal("the returning leader did not learn of the revocation made while it was away")
	}
	c.down["db2"] = false
	if err := c.servers["db1"].distributeRevocations(ctx); err != nil {
		t.Fatalf("distribution of the returning leader: %v", err)
	}
	for _, id := range []string{"db2", "db3"} {
		got := c.crlSerials(id)
		if !got["aa01"] || !got["bb02"] {
			t.Errorf("%s trusts a certificate that was revoked: %v", id, got)
		}
		if l := c.servers[id].Svc.RevokedSerials(); !slices.Contains(l, "bb02") {
			t.Errorf("%s: serial list %v", id, l)
		}
	}
}

// CAE-5: a ca.key that was cut short is no key: the leader replaces it from its copy, or says it has none.
func TestATruncatedCAKeyOfTheNewLeader(t *testing.T) {
	c := newEscrowCluster(t, "db1", "db2")
	ctx := context.Background()
	if _, err := c.leader().SetEscrow(ctx, true); err != nil {
		t.Fatal(err)
	}
	dir := c.servers["db2"].Svc.PKIDir()
	if err := os.WriteFile(filepath.Join(dir, pki.CAKeyFile), []byte("-----BEGIN EC PRIV"), 0o600); err != nil {
		t.Fatal(err)
	}
	if ok, err := c.servers["db2"].AdoptEscrow(ctx); err != nil || !ok {
		t.Fatalf("a truncated key with a copy: %v %v", ok, err)
	}
	if _, err := c.servers["db2"].Svc.CA(); err != nil {
		t.Fatalf("the replaced key does not load: %v", err)
	}
	// Without a copy it says it has no key.
	c2 := newEscrowCluster(t, "db1", "db2")
	if err := os.WriteFile(filepath.Join(c2.servers["db2"].Svc.PKIDir(), pki.CAKeyFile), []byte("-----BEGIN EC PRIV"), 0o600); err != nil {
		t.Fatal(err)
	}
	if ok, err := c2.servers["db2"].AdoptEscrow(ctx); err != nil || ok {
		t.Fatalf("a truncated key without a copy: %v %v", ok, err)
	}
}

// CAE-4/CAE-3: the state a new leader needs is in the system database after every change.
func TestTheStateIsMirroredToTheSystemDatabase(t *testing.T) {
	c := newEscrowCluster(t, "db1", "db2", "db3")
	ctx := context.Background()
	if _, err := c.leader().SetEscrow(ctx, true); err != nil {
		t.Fatal(err)
	}
	sh, ok, _ := c.db.Load(ctx)
	if !ok || !sh.Enabled || sh.Epoch < 1 || sh.Generation < 1 || len(sh.Holders) != 2 {
		t.Fatalf("mirror %+v %v", sh, ok)
	}
	// A database that cannot be written does not stop the round; the next save writes it.
	c.db.mu.Lock()
	c.db.down = true
	c.db.mu.Unlock()
	if _, err := c.leader().SetEscrow(ctx, false); err != nil {
		t.Fatal(err)
	}
	c.db.mu.Lock()
	c.db.down = false
	c.db.mu.Unlock()
	c.sync("db1")
	c.sync("db1")
	sh, _, _ = c.db.Load(ctx)
	if sh.Enabled {
		t.Fatalf("the choice of the operator did not reach the system database: %+v", sh)
	}
}

// forgeCRL signs a revocation list as another holder of the CA key would: a copy of the CA with the given serials, issued at
// "at" with at least the given number.
func (c *escrowCluster) forgeCRL(serials []string, number int64, at time.Time) []byte {
	c.t.Helper()
	src := c.servers["db1"].Svc.PKIDir()
	dst := c.t.TempDir()
	for _, n := range []string{pki.CACertFile, pki.CAKeyFile} {
		b, err := os.ReadFile(filepath.Join(src, n))
		if err != nil {
			c.t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dst, n), b, 0o600); err != nil {
			c.t.Fatal(err)
		}
	}
	ca, err := pki.LoadCA(dst)
	if err != nil {
		c.t.Fatal(err)
	}
	if _, err := ca.MergeRevoked(serials, at); err != nil {
		c.t.Fatal(err)
	}
	if err := ca.EnsureCRLNumberAbove(number - 1); err != nil {
		c.t.Fatal(err)
	}
	crl, err := ca.CRL(at)
	if err != nil {
		c.t.Fatal(err)
	}
	return crl
}

func (c *escrowCluster) install(id string, serials []string, crl []byte) error {
	req := wire.PKIRequest{CACert: string(c.ca.CertPEM()), Revoked: serials, CRL: string(crl)}
	body, _ := json.Marshal(req)
	_, err := (&Receiver{M: c.servers[id]}).Handle(context.Background(), "POST /pki/install", "db1", body)
	return err
}

var farFuture = time.Now().Unix() + 100000

// CAE-4: a holder refuses a list that takes a revocation back, whatever its number, and a list with a lower number.
func TestAHolderRefusesAListThatTakesARevocationBack(t *testing.T) {
	c := newEscrowCluster(t, "db1", "db2")
	if err := c.install("db2", []string{"aa01", "bb02"}, c.forgeCRL([]string{"aa01", "bb02"}, farFuture, time.Now())); err != nil {
		t.Fatal(err)
	}
	var ce *internalapi.CallError
	// A higher number that lacks bb02.
	if err := c.install("db2", []string{"aa01"}, c.forgeCRL([]string{"aa01"}, farFuture+10, time.Now())); !errors.As(err, &ce) || ce.Code != codeStaleRevocations || ce.Status != 409 {
		t.Fatalf("a list that lacks a revocation: %v", err)
	}
	// Every serial but a lower number.
	if err := c.install("db2", []string{"aa01", "bb02"}, c.forgeCRL([]string{"aa01", "bb02"}, 5, time.Unix(1_000_000, 0))); !errors.As(err, &ce) || ce.Code != codeStaleRevocations {
		t.Fatalf("a list with a lower number: %v", err)
	}
	if got := c.crlSerials("db2"); !got["aa01"] || !got["bb02"] {
		t.Fatalf("a refused list was written: %v", got)
	}
	// A list with more revocations and a higher number is installed, and the serial list of the holder is a union.
	if err := c.install("db2", []string{"cc03"}, c.forgeCRL([]string{"aa01", "bb02", "cc03"}, farFuture+20, time.Now())); err != nil {
		t.Fatal(err)
	}
	l, _ := pki.LoadRevokedList(c.servers["db2"].Svc.PKIDir())
	if !slices.Contains(l, "aa01") || !slices.Contains(l, "cc03") {
		t.Fatalf("the serial list of the holder is not a union: %v", l)
	}
}

// CAE-4: a server that holds the CA key but is not the leader takes the revocations it receives into its own list.
func TestAFormerLeaderThatReceivesAListTakesItIntoItsCA(t *testing.T) {
	c := newEscrowCluster(t, "db1", "db2")
	if err := c.install("db1", []string{"cc03"}, c.forgeCRL([]string{"cc03"}, farFuture, time.Now())); err != nil {
		t.Fatal(err)
	}
	if !c.mgrCA("db1").IsRevoked("cc03") {
		t.Fatal("a server that holds the CA key kept its own list when it received a newer one")
	}
	again, err := pki.LoadCA(c.servers["db1"].Svc.PKIDir())
	if err != nil || !again.IsRevoked("cc03") {
		t.Fatalf("the merge was not saved: %v", err)
	}
}

// CAE-4: a leader that sends a list that goes back is refused, merges what the servers hold and sends the repaired list.
func TestALeaderWhoseListWentBackIsRefusedAndRepairs(t *testing.T) {
	c := newEscrowCluster(t, "db1", "db2")
	ctx := context.Background()
	ca1 := c.mgrCA("db1")
	// db2 holds bb02, which db1 never heard of.
	if err := c.install("db2", []string{"bb02"}, c.forgeCRL([]string{"bb02"}, 1, time.Now())); err != nil {
		t.Fatal(err)
	}
	if ca1.IsRevoked("bb02") {
		t.Fatal("setup")
	}
	// db1 sends without having asked anybody.
	if err := c.servers["db1"].Svc.PublishLocalCRL(ctx); err != nil {
		t.Fatal(err)
	}
	crl, _ := c.servers["db1"].Svc.CRLPEM()
	req := wire.PKIRequest{CACert: string(ca1.CertPEM()), Revoked: ca1.Revoked(), CRL: string(crl)}
	if err := c.servers["db1"].installRevocations(ctx, "db2", ca1, req); err != nil {
		t.Fatalf("the repair: %v", err)
	}
	if !ca1.IsRevoked("bb02") || !c.crlSerials("db2")["bb02"] {
		t.Fatalf("the revocation of bb02 was lost: leader %v holder %v", ca1.Revoked(), c.crlSerials("db2"))
	}
}

// CAE-4: a leader asks the servers what they hold before it sends; it does not rely on being refused.
func TestADistributionAsksTheServersFirst(t *testing.T) {
	c := newEscrowCluster(t, "db1", "db2")
	ctx := context.Background()
	if err := c.install("db2", []string{"bb02"}, c.forgeCRL([]string{"bb02"}, 1, time.Now())); err != nil {
		t.Fatal(err)
	}
	if err := c.servers["db1"].distributeRevocations(ctx); err != nil {
		t.Fatal(err)
	}
	if c.stale != 0 {
		t.Fatalf("the leader sent a list that was refused %d times: it did not read the lists of the servers first", c.stale)
	}
	if !c.mgrCA("db1").IsRevoked("bb02") || !c.crlSerials("db2")["bb02"] {
		t.Fatal("the revocation was lost")
	}
}
