// SPDX-License-Identifier: AGPL-3.0-only

package membership

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/pki"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/task"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/wire"
)

type wireOffer = wire.EscrowOfferResponse

// holds writes the list a server reports through GET /revoked (a server that is not the CA holder reports its serial list file).
func (c *escrowCluster) holds(id string, serials ...string) {
	c.t.Helper()
	if err := pki.SaveRevokedList(c.servers[id].Svc.PKIDir(), serials); err != nil {
		c.t.Fatal(err)
	}
}

// CAF-1: a server can make the leader revoke only certificates this CA issued.
func TestAHostileAnswerCannotMakeTheLeaderRevokeWhatItNeverIssued(t *testing.T) {
	c := newEscrowCluster(t, "db1", "db2", "db3")
	ca := c.mgrCA("db1")
	long := ""
	for i := 0; i < 70; i++ {
		long += "a"
	}
	c.issued[long] = true // issued, but longer than any serial can be
	c.holds("db2", "ee05", "aa01", long, "not hex", "")
	c.holds("db3", "ee06")
	c.servers["db1"].reconcileRevocations(context.Background(), ca)
	if ca.IsRevoked("ee05") || ca.IsRevoked("ee06") || ca.IsRevoked(long) {
		t.Fatalf("the leader merged serials it never issued: %v", ca.Revoked())
	}
	if !ca.IsRevoked("aa01") || len(ca.Revoked()) != 1 {
		t.Fatalf("the serial this CA issued was not merged, or something else was: %v", ca.Revoked())
	}
	// And the leader's own certificate cannot be revoked this way even though it is issued: it is not in the list the server reports.
	c.issued["01"] = true
	c.holds("db2", "01")
	c.servers["db1"].reconcileRevocations(context.Background(), ca)
	if !ca.IsRevoked("01") {
		t.Fatal("setup: an issued serial reported by a server is merged")
	}
}

// CAF-1: the answers of revoked servers are not read at all, and no more than a bounded number of serials is taken from one.
func TestTheAnswersOfARevokedServerAreNotRead(t *testing.T) {
	c := newEscrowCluster(t, "db1", "db2", "db3")
	ca := c.mgrCA("db1")
	c.holds("db2", "aa01")
	c.readable = []string{"db3"} // db2 is revoked
	c.servers["db1"].reconcileRevocations(context.Background(), ca)
	if ca.IsRevoked("aa01") {
		t.Fatal("the answer of a revoked server was merged")
	}
	c.readable = nil
	c.servers["db1"].reconcileRevocations(context.Background(), ca)
	if !ca.IsRevoked("aa01") {
		t.Fatal("the answer of a server that is not revoked was not merged")
	}
}

func TestOneServerCannotMakeTheLeaderMergeAnUnboundedList(t *testing.T) {
	c := newEscrowCluster(t, "db1", "db2")
	ca := c.mgrCA("db1")
	var list []string
	for i := 0; i < maxSerialsFromPeer+500; i++ {
		s := fmt.Sprintf("%08x", 0x10000000+i)
		c.issued[s] = true
		list = append(list, s)
	}
	c.holds("db2", list...)
	c.servers["db1"].reconcileRevocations(context.Background(), ca)
	if n := len(ca.Revoked()); n != maxSerialsFromPeer {
		t.Fatalf("%d serials were merged from one server, want %d", n, maxSerialsFromPeer)
	}
}

// CAF-1: when the certificates this CA issued cannot be read, nothing a server says is merged.
func TestNothingIsMergedFromAServerWhenTheIssuedSerialsCannotBeRead(t *testing.T) {
	c := newEscrowCluster(t, "db1", "db2")
	ca := c.mgrCA("db1")
	c.holds("db2", "aa01")
	c.servers["db1"].escrowIssued = func(context.Context) (map[string]bool, error) { return nil, errors.New("no database") }
	c.servers["db1"].reconcileRevocations(context.Background(), ca)
	if ca.IsRevoked("aa01") {
		t.Fatal("a serial was merged without knowing what this CA issued")
	}
}

// CAF-2: a holder whose list number is above the leader's clock does not pin the leader out: it learns the number from the answer of GET
// /revoked (no refusal at all) and, when it was not asked, from the refusal.
func TestALeaderLearnsTheNumberOfAHolderThatIsAheadOfItsClock(t *testing.T) {
	for _, tc := range []struct {
		name      string
		readable  []string
		wantStale int
	}{
		{"from the answer of the servers", nil, 0},
		{"from the refusal", []string{}, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newEscrowCluster(t, "db1", "db2")
			ahead := time.Now().Unix() + 3600
			if err := c.mgrCA("db1").Revoke("aa01", time.Now()); err != nil {
				t.Fatal(err)
			}
			if err := c.install("db2", []string{"aa01"}, c.forgeCRL([]string{"aa01"}, ahead, time.Now())); err != nil {
				t.Fatal(err)
			}
			c.readable = tc.readable
			if c.readable != nil {
				c.readable = []string{}
			}
			if err := c.servers["db1"].distributeRevocations(context.Background()); err != nil {
				t.Fatalf("the distribution of a leader whose clock is behind: %v", err)
			}
			if c.stale != tc.wantStale {
				t.Fatalf("%d refusals, want %d", c.stale, tc.wantStale)
			}
			if got := pki.HeldCRLNumber(c.servers["db2"].Svc.PKIDir()); got <= ahead {
				t.Fatalf("the holder still holds number %d, not above %d", got, ahead)
			}
		})
	}
}

// CAF-2: a number far above the clock (a holder with a wrong date) is not followed, and does not stop the others.
func TestANumberFarAboveTheClockIsNotFollowed(t *testing.T) {
	c := newEscrowCluster(t, "db1", "db2", "db3")
	far := time.Now().Add(10 * 24 * time.Hour).Unix()
	if err := c.install("db2", []string{"aa01"}, c.forgeCRL([]string{"aa01"}, far, time.Now())); err != nil {
		t.Fatal(err)
	}
	err := c.servers["db1"].distributeRevocations(context.Background())
	if err == nil {
		t.Fatal("the holder with a number ten days ahead accepted a list")
	}
	if got := pki.HeldCRLNumber(c.servers["db2"].Svc.PKIDir()); got != far {
		t.Fatalf("the holder's list changed: %d", got)
	}
	if n := pki.HeldCRLNumber(c.servers["db3"].Svc.PKIDir()); n == 0 || n > far {
		t.Fatalf("the other server did not get the list: %d", n)
	}
	if n := pki.HeldCRLNumber(c.servers["db1"].Svc.PKIDir()); n >= far {
		t.Fatalf("the leader followed a number ten days ahead: %d", n)
	}
}

// CAF-3: the revoke task goes on past a server that refuses, sends to the revoked server last and ignores what it answers.
func TestARefusingServerDoesNotStopTheRevokeTask(t *testing.T) {
	c := newEscrowCluster(t, "db1", "db2", "db3")
	far := time.Now().Add(10 * 24 * time.Hour).Unix()
	// db2 refuses every list (its number is far ahead); db3 is fine. Revoked or not, the list reaches db3.
	if err := c.install("db2", []string{"aa01"}, c.forgeCRL([]string{"aa01"}, far, time.Now())); err != nil {
		t.Fatal(err)
	}
	c.revoked = map[string]bool{}
	if err := c.servers["db1"].stepCertRevokeDistribute(context.Background(), &task.Env{}); err == nil {
		t.Fatal("a server that refused did not fail the task")
	}
	if pki.HeldCRLNumber(c.servers["db3"].Svc.PKIDir()) == 0 {
		t.Fatal("the server after the refusing one never got the list")
	}
	// The same server, revoked: its refusal has no say.
	c.revoked = map[string]bool{"db2": true}
	c.mu.Lock()
	c.paths = nil
	c.mu.Unlock()
	if err := c.servers["db1"].stepCertRevokeDistribute(context.Background(), &task.Env{}); err != nil {
		t.Fatalf("the refusal of a revoked server failed the task: %v", err)
	}
	var order []string
	c.mu.Lock()
	for _, p := range c.paths {
		if p == "POST db2 /pki/install" || p == "POST db3 /pki/install" {
			order = append(order, p)
		}
	}
	c.mu.Unlock()
	if len(order) < 2 || order[0] != "POST db3 /pki/install" || order[len(order)-1] != "POST db2 /pki/install" {
		t.Fatalf("the revoked server was not last: %v", order)
	}
}

func TestDistributeAllCollectsErrorsAndRetriesOnce(t *testing.T) {
	m := &Manager{}
	calls := map[string]int{}
	err := m.distributeAll([]string{"a", "b", "c"}, map[string]bool{"c": true}, func(id string) error {
		calls[id]++
		switch id {
		case "a":
			return errors.New("refused")
		case "b":
			if calls[id] == 1 {
				return errors.New("busy")
			}
		case "c":
			return errors.New("revoked server refuses")
		}
		return nil
	})
	if err == nil || calls["a"] != 2 || calls["b"] != 2 || calls["c"] != 1 {
		t.Fatalf("%v %v", err, calls)
	}
}

// N9, N11: the sealed copy and the revoke task are merged in too.
func TestAReturningLeaderMergesTheRevocationsOfItsSealedCopy(t *testing.T) {
	c := newEscrowCluster(t, "db1", "db2")
	ctx := context.Background()
	if err := c.mgrCA("db1").Revoke("ab12", time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := c.leader().SetEscrow(ctx, true); err != nil {
		t.Fatal(err)
	}
	// db2 holds the copy with ab12 in it. Nothing else, no server and no file, knows ab12 now.
	_ = os.Remove(filepath.Join(c.servers["db2"].Svc.PKIDir(), pki.RevokedListFile))
	c.down["db1"] = true
	if ok, err := c.servers["db2"].AdoptEscrow(ctx); err != nil || !ok {
		t.Fatalf("%v %v", ok, err)
	}
	// Adoption merges the copy itself; a second server that adopts from a held key reads it through reconcile.
	if !c.mgrCA("db2").IsRevoked("ab12") {
		t.Fatal("the revocation in the sealed copy is lost")
	}
	if err := os.Remove(filepath.Join(c.servers["db2"].Svc.PKIDir(), pki.RevokedFile)); err != nil {
		t.Fatal(err)
	}
	ca, err := pki.LoadCA(c.servers["db2"].Svc.PKIDir())
	if err != nil {
		t.Fatal(err)
	}
	if ca.IsRevoked("ab12") {
		t.Fatal("setup: the list was not removed")
	}
	c.servers["db2"].reconcileRevocations(ctx, ca)
	if !ca.IsRevoked("ab12") {
		t.Fatal("reconcile did not merge the revocations of the sealed copy")
	}
}

func TestTheRevokeTaskMergesWhatTheServersHoldFirst(t *testing.T) {
	c := newEscrowCluster(t, "db1", "db2")
	c.holds("db2", "bb02")
	if err := c.servers["db1"].stepCertRevokeDistribute(context.Background(), &task.Env{}); err != nil {
		t.Fatal(err)
	}
	if !c.mgrCA("db1").IsRevoked("bb02") {
		t.Fatal("the revoke task sent a list that lacks what a server holds")
	}
	if c.stale != 0 {
		t.Fatalf("the list was refused %d times", c.stale)
	}
}

// N19, N30: the number of the next list is above the one in the published file, and the epoch above the one in the copy held.
func TestAdoptionTakesTheEpochOfTheHeldCopyAndTheNumberOfTheFile(t *testing.T) {
	c := newEscrowCluster(t, "db1", "db2")
	ctx := context.Background()
	kid, pub, err := pki.RecipientOffer(c.servers["db2"].Svc.PKIDir(), false, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	b, err := pki.SealCA(c.mgrCA("db1"), "db2", kid, pub, pki.Position{Epoch: 7, Generation: 3, Seq: 1}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err := pki.StoreEscrow(c.servers["db2"].Svc.PKIDir(), "db2", b); err != nil {
		t.Fatal(err)
	}
	// No state file and no record: only the held copy says where the epoch is. A published list with a high number is on disk.
	published := time.Now().Unix() + 1000
	crl := c.forgeCRL([]string{"aa01"}, published, time.Now())
	if err := c.install("db2", []string{"aa01"}, crl); err != nil {
		t.Fatal(err)
	}
	if ok, err := c.servers["db2"].AdoptEscrow(ctx); err != nil || !ok {
		t.Fatalf("%v %v", ok, err)
	}
	st, err := pki.LoadEscrowState(c.servers["db2"].Svc.PKIDir())
	if err != nil || st.Epoch != 8 || st.Generation != 4 {
		t.Fatalf("epoch %d generation %d, want 8 and 4 (%v)", st.Epoch, st.Generation, err)
	}
	next, err := c.servers["db2"].Svc.CRLPEM()
	if err != nil {
		t.Fatal(err)
	}
	if n, _, _ := pki.CRLContent(next); n <= published {
		t.Fatalf("the next list has number %d, not above the published one", n)
	}
}

// CAF-4, CAF-5: a rotation does not take an epoch above a server's, and a stale leader cannot make a server create escrow keys.
func TestARotationDoesNotTakeAnEpochAboveWhatAServerHolds(t *testing.T) {
	c := newEscrowCluster(t, "db1", "db2")
	ctx := context.Background()
	if _, err := c.leader().SetEscrow(ctx, true); err != nil {
		t.Fatal(err)
	}
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
	keysBefore := countKeys(t, c.servers["db2"])
	st, err := c.leader().RotateEscrow(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if st.Complete {
		t.Fatalf("a leader of a lower epoch rotated over a newer one: %+v", st)
	}
	if h, _ := held(c.servers["db2"]); h.Position.Epoch != 9 {
		t.Fatalf("the copy changed: %+v", h.Position)
	}
	if n := countKeys(t, c.servers["db2"]); n != keysBefore {
		t.Fatalf("a stale leader made the server create escrow keys: %d -> %d", keysBefore, n)
	}
}

func countKeys(t *testing.T, m *Manager) int {
	t.Helper()
	g, _ := filepath.Glob(filepath.Join(m.Svc.PKIDir(), pki.EscrowDir, "recipient-*.key"))
	return len(g)
}

// CAF-6: what the system database says is validated.
func TestTheMirrorIsValidatedWhenItIsRead(t *testing.T) {
	c := newEscrowCluster(t, "db1", "db2")
	ctx := context.Background()
	if _, err := c.leader().SetEscrow(ctx, true); err != nil {
		t.Fatal(err)
	}
	c.db.mu.Lock()
	holders := []string{"bad id with spaces", "x/y", "", "db2"}
	for i := 0; i < 100; i++ {
		holders = append(holders, fmt.Sprintf("h%d", i))
	}
	c.db.val = escrowShared{Enabled: true, Generation: 1 << 62 << 1 >> 1, Epoch: 1<<63 - 1, Holders: holders}
	c.db.has = true
	c.db.mu.Unlock()
	c.down["db1"] = true
	if ok, err := c.servers["db2"].AdoptEscrow(ctx); err != nil || !ok {
		t.Fatalf("%v %v", ok, err)
	}
	st, err := pki.LoadEscrowState(c.servers["db2"].Svc.PKIDir())
	if err != nil {
		t.Fatal(err)
	}
	if st.Epoch <= 0 || st.Epoch > maxCounter+1 || st.Generation <= 0 || st.Generation > maxCounter+1 {
		t.Fatalf("epoch %d generation %d: the counters are not bounded", st.Epoch, st.Generation)
	}
	if len(st.Recipients) > maxHolders {
		t.Fatalf("%d holders were accepted", len(st.Recipients))
	}
	for id := range st.Recipients {
		if !validServerID(id) {
			t.Fatalf("an invalid holder id was accepted: %q", id)
		}
	}
}

func TestServerIDsAreChecked(t *testing.T) {
	for id, want := range map[string]bool{"db1": true, "10.0.0.2": true, "host.example": true, "": false, "a b": false, "a/b": false, `a\b`: false, "a\x00b": false} {
		if validServerID(id) != want {
			t.Errorf("%q: %v", id, !want)
		}
	}
}

// CAF-6: a position that a server reports and that cannot be true is refused.
func TestAnImpossiblePositionOfAServersCopyIsRefused(t *testing.T) {
	c := newEscrowCluster(t, "db1", "db2")
	ctx := context.Background()
	inner := c.servers["db1"].escrowTransport
	c.servers["db1"].escrowTransport = func(ctx context.Context, id, method, path string, in, out any) error {
		err := inner(ctx, id, method, path, in, out)
		if path == "/pki/escrow/offer" && err == nil {
			if o, ok := out.(*wireOffer); ok {
				o.HeldEpoch = -5
			}
		}
		return err
	}
	st, err := c.leader().SetEscrow(ctx, true)
	if err != nil {
		t.Fatal(err)
	}
	if st.Complete || c.hasCopy("db2") {
		t.Fatalf("a copy was sealed after an impossible position: %+v", st)
	}
}

// CAF-7: a copy that cannot be deleted is tried every round at first and rarely later.
func TestADropThatKeepsFailingIsTriedLessOften(t *testing.T) {
	var rs pki.EscrowRecipientState
	due := 0
	for i := 0; i < 100; i++ {
		if dropDue(&rs) {
			due++
		}
	}
	if due < 29+7 || due > 29+8 {
		t.Fatalf("%d attempts in 100 rounds", due)
	}
}

// CAF-9: the list that goes out with a certificate and the one that goes to a new server are at least what the servers hold.
func TestAListSentWithACertificateIsAtLeastWhatTheServersHold(t *testing.T) {
	c := newEscrowCluster(t, "db1", "db2")
	c.holds("db2", "bb02")
	if err := c.servers["db1"].Svc.PublishLocalCRL(context.Background()); err != nil {
		t.Fatal(err)
	}
	is := issued{CertPEM: "x", Serial: "01", NotAfter: time.Now()}
	res, err := c.servers["db1"].renewalAnswer(context.Background(), is)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, s := range res.Revoked {
		found = found || s == "bb02"
	}
	if !found {
		t.Fatalf("the list that goes out with a certificate lacks what a server holds: %v", res.Revoked)
	}
}

// N30: the number of the next list is above the one of the list this server holds in its own file.
func TestTheNextNumberIsAboveTheListInTheOwnFile(t *testing.T) {
	c := newEscrowCluster(t, "db1", "db2")
	high := time.Now().Unix() + 1000
	if err := c.install("db1", []string{"aa01"}, c.forgeCRL([]string{"aa01"}, high, time.Now())); err != nil {
		t.Fatal(err)
	}
	ca := c.mgrCA("db1")
	c.servers["db1"].reconcileRevocations(context.Background(), ca)
	crl, err := ca.CRL(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if n, _, _ := pki.CRLContent(crl); n <= high {
		t.Fatalf("the next list has number %d, not above %d", n, high)
	}
}
