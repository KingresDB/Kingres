// SPDX-License-Identifier: AGPL-3.0-only

package membership

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/cluster"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/internalapi"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/peerclient"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/pki"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/problem"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/setup"
)

// escrowCluster is servers made of a Manager and a Receiver each, joined by a transport that calls the receiver of the
// named server as the leader would over the internal API.
type escrowCluster struct {
	t       *testing.T
	ca      *pki.CA
	servers map[string]*Manager
	down    map[string]bool
	active  []string
	mu      sync.Mutex
	calls   []string
}

func newEscrowServer(t *testing.T, id string) *Manager {
	t.Helper()
	dir := t.TempDir()
	node, err := setup.Open(dir, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	svc, err := cluster.New(cluster.Config{DataDir: dir, ServerHost: id}, node)
	if err != nil {
		t.Fatal(err)
	}
	return &Manager{Svc: svc, Self: id}
}

func newEscrowCluster(t *testing.T, ids ...string) *escrowCluster {
	t.Helper()
	c := &escrowCluster{t: t, servers: map[string]*Manager{}, down: map[string]bool{}}
	leader := newEscrowServer(t, ids[0])
	ca, err := pki.CreateCA(leader.Svc.PKIDir(), "test", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	c.ca = ca
	c.servers[ids[0]] = leader
	for _, id := range ids[1:] {
		m := newEscrowServer(t, id)
		if err := os.MkdirAll(m.Svc.PKIDir(), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(m.Svc.PKIDir(), pki.CACertFile), ca.CertPEM(), 0o644); err != nil { //nolint:gosec // a certificate is public
			t.Fatal(err)
		}
		c.servers[id] = m
		c.active = append(c.active, id)
	}
	for _, m := range c.servers {
		m.escrowActive = func(context.Context) ([]string, error) { return append([]string(nil), c.active...), nil }
		m.escrowTransport = c.transport(m)
	}
	return c
}

func (c *escrowCluster) transport(from *Manager) func(context.Context, string, string, any, any) error {
	return func(ctx context.Context, id, path string, in, out any) error {
		c.mu.Lock()
		c.calls = append(c.calls, id+" "+path)
		down := c.down[id]
		to := c.servers[id]
		c.mu.Unlock()
		if down || to == nil {
			return peerclient.ErrUnreachable
		}
		body, _ := json.Marshal(in)
		res, err := (&Receiver{M: to}).Handle(ctx, "POST "+path, from.Self, body)
		var ce *internalapi.CallError
		if errors.As(err, &ce) {
			return &peerclient.APIError{Status: ce.Status, Code: ce.Code, Message: ce.Message}
		}
		if err != nil {
			return err
		}
		if out != nil {
			b, _ := json.Marshal(res)
			return json.Unmarshal(b, out)
		}
		return nil
	}
}

func (c *escrowCluster) leader() *Manager { return c.servers["db1"] }

func (c *escrowCluster) status(m *Manager) (got struct {
	enabled bool
	current map[string]bool
	states  map[string]string
}) {
	c.t.Helper()
	st, err := m.EscrowStatus(context.Background())
	if err != nil {
		c.t.Fatal(err)
	}
	got.enabled = st.Enabled
	got.current, got.states = map[string]bool{}, map[string]string{}
	for _, r := range st.Recipients {
		got.current[r.ServerID] = r.State == "current"
		got.states[r.ServerID] = r.State
	}
	return got
}

func held(m *Manager) (pki.EscrowHeld, error) { return pki.HeldEscrow(m.Svc.PKIDir()) }

func TestEscrowIsOffUntilTheOperatorSwitchesItOn(t *testing.T) {
	c := newEscrowCluster(t, "db1", "db2", "db3")
	if err := c.leader().SyncEscrow(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(c.calls) != 0 {
		t.Fatalf("the leader called servers with escrow off: %v", c.calls)
	}
	if st := c.status(c.leader()); st.enabled || len(st.current) != 0 {
		t.Fatalf("status %+v", st)
	}
}

func TestSwitchingOnSealsACopyToEveryRecipientAndANewLeaderAdoptsIt(t *testing.T) {
	c := newEscrowCluster(t, "db1", "db2", "db3")
	ctx := context.Background()
	st, err := c.leader().SetEscrow(ctx, true)
	if err != nil {
		t.Fatal(err)
	}
	if !st.Complete || len(st.Recipients) != 2 || st.Generation != 1 {
		t.Fatalf("status %+v", st)
	}
	for _, id := range []string{"db2", "db3"} {
		if _, err := held(c.servers[id]); err != nil {
			t.Errorf("%s holds no copy: %v", id, err)
		}
	}
	// A revocation reaches the copies before the leader dies: the second round sends again.
	lca, err := c.leader().Svc.CA()
	if err != nil {
		t.Fatal(err)
	}
	if err := lca.Revoke("beef01", time.Now()); err != nil {
		t.Fatal(err)
	}
	before := len(c.calls)
	if err := c.leader().SyncEscrow(ctx); err != nil {
		t.Fatal(err)
	}
	if len(c.calls) == before {
		t.Fatal("a revocation did not make the leader send the copies again")
	}
	// The leader's server is lost: db2 becomes the leader.
	ok, err := c.servers["db2"].AdoptEscrow()
	if err != nil || !ok {
		t.Fatalf("adopt: %v %v", ok, err)
	}
	nca, err := c.servers["db2"].Svc.CA()
	if err != nil {
		t.Fatal(err)
	}
	if !nca.IsRevoked("beef01") {
		t.Error("the revocation made before the failover is lost")
	}
	// The new leader goes on: it keeps escrow on and sends to its recipients (db1 is gone, db3 stays).
	c.servers["db2"].escrowActive = func(context.Context) ([]string, error) { return []string{"db3"}, nil }
	c.servers["db2"].escrowTransport = c.transport(c.servers["db2"])
	if st, err := c.servers["db2"].EscrowStatus(ctx); err != nil || !st.Enabled || !st.HoldsKey {
		t.Fatalf("status of the new leader: %+v %v", st, err)
	}
	if err := c.servers["db2"].SyncEscrow(ctx); err != nil {
		t.Fatal(err)
	}
	if st := c.status(c.servers["db2"]); !st.current["db3"] || len(st.current) != 1 {
		t.Fatalf("the new leader's recipients: %+v", st)
	}
}

func TestAdoptWithoutACopyLeavesTheServerWithoutTheKey(t *testing.T) {
	c := newEscrowCluster(t, "db1", "db2")
	ok, err := c.servers["db2"].AdoptEscrow()
	if err != nil || ok {
		t.Fatalf("adopt without a copy: %v %v", ok, err)
	}
	if _, err := c.servers["db2"].Svc.CA(); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("CA after a failed adoption: %v", err)
	}
}

func TestOnlyTheServersThatMayBecomeLeaderGetACopy(t *testing.T) {
	c := newEscrowCluster(t, "db1", "db2", "db3")
	c.leader().SetEscrowRecipients(func(context.Context) ([]string, error) { return []string{"db3", "db9"}, nil })
	if _, err := c.leader().SetEscrow(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	if _, err := held(c.servers["db3"]); err != nil {
		t.Errorf("the standby's server holds no copy: %v", err)
	}
	if _, err := held(c.servers["db2"]); !errors.Is(err, pki.ErrNoEscrow) {
		t.Errorf("a server that is not a candidate holds a copy: %v", err)
	}
	for _, call := range c.calls {
		if call[:3] == "db9" {
			t.Errorf("a server that is not a member was called: %v", c.calls)
		}
	}
}

func TestAServerThatIsNoLongerAMemberLosesItsCopy(t *testing.T) {
	c := newEscrowCluster(t, "db1", "db2", "db3")
	ctx := context.Background()
	if _, err := c.leader().SetEscrow(ctx, true); err != nil {
		t.Fatal(err)
	}
	c.active = []string{"db3"} // db2 was removed
	if err := c.leader().SyncEscrow(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := held(c.servers["db2"]); !errors.Is(err, pki.ErrNoEscrow) {
		t.Errorf("the removed server still holds a copy: %v", err)
	}
	if st := c.status(c.leader()); len(st.current) != 1 || !st.current["db3"] {
		t.Errorf("recipients %+v", st)
	}
	// A removed server that cannot be reached is not a recipient any more either, and the round does not fail.
	c.down["db3"] = true
	c.active = nil
	if err := c.leader().SyncEscrow(ctx); err != nil {
		t.Fatal(err)
	}
	if st := c.status(c.leader()); len(st.states) != 0 {
		t.Errorf("recipients %+v", st)
	}
}

func TestAnUnreachableRecipientIsPendingAndGetsItsCopyLater(t *testing.T) {
	c := newEscrowCluster(t, "db1", "db2", "db3")
	ctx := context.Background()
	c.down["db3"] = true
	st, err := c.leader().SetEscrow(ctx, true)
	if err != nil {
		t.Fatal(err)
	}
	if st.Complete {
		t.Fatal("complete with a recipient that was not reached")
	}
	got := c.status(c.leader())
	if got.states["db2"] != "current" || got.states["db3"] != "pending" {
		t.Fatalf("states %v", got.states)
	}
	c.down["db3"] = false
	if err := c.leader().SyncEscrow(ctx); err != nil {
		t.Fatal(err)
	}
	if got := c.status(c.leader()); got.states["db3"] != "current" {
		t.Fatalf("after the server came back: %v", got.states)
	}
}

func TestARecipientThatRefusesIsShownAsRefused(t *testing.T) {
	c := newEscrowCluster(t, "db1", "db2")
	ctx := context.Background()
	// The server was reset and holds another CA: it refuses every copy.
	other, err := pki.CreateCA(filepath.Join(t.TempDir(), "pki"), "other", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(c.servers["db2"].Svc.PKIDir(), pki.CACertFile), other.CertPEM(), 0o644); err != nil { //nolint:gosec // a certificate is public
		t.Fatal(err)
	}
	if _, err := c.leader().SetEscrow(ctx, true); err != nil {
		t.Fatal(err)
	}
	if got := c.status(c.leader()); got.states["db2"] != "refused" {
		t.Fatalf("states %v", got.states)
	}
	if _, err := held(c.servers["db2"]); !errors.Is(err, pki.ErrNoEscrow) {
		t.Fatalf("a foreign copy was kept: %v", err)
	}
}

func TestSwitchingOffTakesEveryCopyAway(t *testing.T) {
	c := newEscrowCluster(t, "db1", "db2", "db3")
	ctx := context.Background()
	if _, err := c.leader().SetEscrow(ctx, true); err != nil {
		t.Fatal(err)
	}
	c.down["db3"] = true
	if _, err := c.leader().SetEscrow(ctx, false); err != nil {
		t.Fatal(err)
	}
	if _, err := held(c.servers["db2"]); !errors.Is(err, pki.ErrNoEscrow) {
		t.Fatalf("copy kept after switching off: %v", err)
	}
	// db3 could not be told; it stays listed and is told when it is back.
	if got := c.status(c.leader()); got.enabled || len(got.states) != 1 {
		t.Fatalf("status %+v", got)
	}
	c.down["db3"] = false
	if err := c.leader().SyncEscrow(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := held(c.servers["db3"]); !errors.Is(err, pki.ErrNoEscrow) {
		t.Fatalf("copy kept on the server that was down: %v", err)
	}
	if got := c.status(c.leader()); len(got.states) != 0 {
		t.Fatalf("status %+v", got)
	}
}

func TestRotationGivesNewKeysAndANewGeneration(t *testing.T) {
	c := newEscrowCluster(t, "db1", "db2")
	ctx := context.Background()
	if _, err := c.leader().RotateEscrow(ctx); err == nil {
		t.Fatal("rotation while the escrow is off")
	} else {
		var p *problem.Problem
		if !errors.As(err, &p) || p.Code != problem.Conflict {
			t.Fatalf("rotation while off: %v", err)
		}
	}
	if _, err := c.leader().SetEscrow(ctx, true); err != nil {
		t.Fatal(err)
	}
	h1, _ := held(c.servers["db2"])
	st, err := c.leader().RotateEscrow(ctx)
	if err != nil || !st.Complete || st.Generation != 2 {
		t.Fatalf("rotate: %+v %v", st, err)
	}
	h2, _ := held(c.servers["db2"])
	if h2.KeyID == h1.KeyID || h2.Generation != 2 {
		t.Fatalf("the copy was not resealed to a new key: %+v %+v", h1, h2)
	}
	// Still adoptable after the rotation.
	if ok, err := c.servers["db2"].AdoptEscrow(); err != nil || !ok {
		t.Fatalf("adopt after rotation: %v %v", ok, err)
	}
}

func TestOnlyTheLeaderSendsCopies(t *testing.T) {
	c := newEscrowCluster(t, "db1", "db2")
	if _, err := c.leader().SetEscrow(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	// db2 has no CA key: nothing to send even though it could be asked.
	before := len(c.calls)
	if err := c.servers["db2"].SyncEscrow(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(c.calls) != before {
		t.Fatalf("a server without the CA key sent copies: %v", c.calls[before:])
	}
}

func TestSettingEscrowOnAServerWithoutTheCAIsTheDedicatedProblem(t *testing.T) {
	c := newEscrowCluster(t, "db1", "db2")
	_, err := c.servers["db2"].SetEscrow(context.Background(), true)
	var p *problem.Problem
	if !errors.As(err, &p) || p.Code != problem.CertificateAuthorityUnavailable {
		t.Fatalf("%v", err)
	}
}

// The receiving calls are for the leader only (the table of the internal API) and refuse what is not a copy for this server.
func TestReceiverRefusesACopyForAnotherServer(t *testing.T) {
	c := newEscrowCluster(t, "db1", "db2", "db3")
	ctx := context.Background()
	if _, err := c.leader().SetEscrow(ctx, true); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(c.servers["db2"].Svc.PKIDir(), pki.EscrowDir, pki.EscrowBlobFile))
	if err != nil {
		t.Fatal(err)
	}
	var blob pki.EscrowBlob
	if err := json.Unmarshal(raw, &blob); err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(map[string]any{"blob": blob})
	_, err = (&Receiver{M: c.servers["db3"]}).Handle(ctx, "POST /pki/escrow/store", "db1", body)
	var ce *internalapi.CallError
	if !errors.As(err, &ce) || ce.Status != http.StatusUnprocessableEntity || ce.Code != "escrow_refused" {
		t.Fatalf("a copy of another server was stored: %v", err)
	}
	// Unknown members of the body are refused.
	_, err = (&Receiver{M: c.servers["db3"]}).Handle(ctx, "POST /pki/escrow/offer", "db1", []byte(`{"rotate":false,"x":1}`))
	if !errors.As(err, &ce) || ce.Status != http.StatusBadRequest {
		t.Fatalf("offer with an unknown member: %v", err)
	}
}

func TestEscrowCallsAreLeaderOnly(t *testing.T) {
	for _, k := range []string{"POST /pki/escrow/offer", "POST /pki/escrow/store", "POST /pki/escrow/drop"} {
		if internalapi.Calls[k] != internalapi.LeaderOnly {
			t.Errorf("%s is not LeaderOnly", k)
		}
	}
}
