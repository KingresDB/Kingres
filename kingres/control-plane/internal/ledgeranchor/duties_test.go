// SPDX-License-Identifier: AGPL-3.0-only

package ledgeranchor

import (
	"strings"
	"testing"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/anchor"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/anchor/worm"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/api"
)

func checkOf(res api.LedgerAnchorTest, name string) (api.LedgerAnchorCheck, bool) {
	for _, c := range res.Checks {
		if c.Name == name {
			return c, true
		}
	}
	return api.LedgerAnchorCheck{}, false
}

// Duty 10: a retention that ends within 30 days, an evidence without an attempt and one that was found are warnings of the Test button; a long
// retention is not.
func TestTheTestButtonWarnsAboutRetentionAttemptAndFoundEvidence(t *testing.T) {
	e := newEnv(t)
	tg := e.addWorm("vault") // retentionDays 30 in this environment: the end is inside the 30 days of the warning
	e.putAgg("shop", 1)
	_ = e.runEnqueue()
	// a version of our own bytes that another principal wrote first: found, with no attempt in it
	r := e.rows()[0]
	e.s3.addVersion(worm.ObjectKey("kingres-anchors", r.AggregateID, r.Size), makeNote(r.Origin, r.Size), "COMPLIANCE", e.clock.Now().Add(30*24*time.Hour))
	if err := e.runSubmit(); err != nil {
		t.Fatal(err)
	}
	if e.rows()[0].State != anchor.StateAnchored {
		t.Fatalf("%+v", e.rows()[0])
	}
	res, err := e.svc.TestTarget(e.ctx(), tg.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"retention-ends", "evidence-attempt", "evidence-found"} {
		if c, ok := checkOf(res, name); !ok || c.Status != "warning" {
			t.Errorf("%s: %+v (all %+v)", name, c, res.Checks)
		}
	}
	// the retention that has ended is a failure
	e.clock.Advance(40 * 24 * time.Hour)
	e.s3.clock = e.clock.Now
	res, _ = e.svc.TestTarget(e.ctx(), tg.ID)
	if c, ok := checkOf(res, "retention-ended"); !ok || c.Status != "failed" {
		t.Errorf("retention-ended: %+v", c)
	}
	// a long retention is quiet
	e2 := newEnv(t)
	tg2, err := e2.svc.CreateTarget(e2.ctx(), "a", api.LedgerAnchorTargetCreate{Kind: "worm", Name: "long", Enabled: true, Config: e2.wormConfig(map[string]any{"retentionDays": 3650}), Credentials: wormCreds()})
	if err != nil {
		t.Fatal(err)
	}
	e2.putAgg("shop", 1)
	_ = e2.runEnqueue()
	_ = e2.runSubmit()
	res, _ = e2.svc.TestTarget(e2.ctx(), tg2.ID)
	if c, ok := checkOf(res, "retention"); !ok || c.Status != "ok" {
		t.Errorf("a long retention: %+v", c)
	}
	if _, ok := checkOf(res, "retention-ends"); ok {
		t.Error("a long retention warned")
	}
}

// Duty 9: the clock of this server against the database's, with the text that names store_time.
func TestTheTestButtonComparesTheClockWithTheDatabase(t *testing.T) {
	e := newEnv(t)
	tg := e.addWorm("vault")
	e.st.Now = func() time.Time { return e.clock.Now() }
	res, _ := e.svc.TestTarget(e.ctx(), tg.ID)
	if c, ok := checkOf(res, "server-clock"); !ok || c.Status != "ok" {
		t.Fatalf("%+v", c)
	}
	e.st.Now = func() time.Time { return e.clock.Now().Add(2 * time.Minute) }
	res, _ = e.svc.TestTarget(e.ctx(), tg.ID)
	if c, _ := checkOf(res, "server-clock"); c.Status != "warning" || !strings.Contains(c.Detail, "store_time") {
		t.Errorf("two minutes: %+v", c)
	}
	e.st.Now = func() time.Time { return e.clock.Now().Add(20 * time.Minute) }
	res, _ = e.svc.TestTarget(e.ctx(), tg.ID)
	if c, _ := checkOf(res, "server-clock"); c.Status != "failed" {
		t.Errorf("twenty minutes: %+v", c)
	}
	// the log gets the warning once an hour
	e.logs.Reset()
	_ = e.runMonitor()
	_ = e.runMonitor()
	if n := strings.Count(e.logs.String(), "anchoring: clock"); n != 1 {
		t.Errorf("%d log lines for the clock", n)
	}
}

// Duty 16: the limit of the retries of the settings reaches the record: a store that answers 503 fails the record after the attempts that were set.
func TestTheLimitOfRetriesFromTheSettingsFailsTheRecord(t *testing.T) {
	e, id := oneRecord(t)
	e.svc.cfg.Limits = anchor.Limits{MaxAttempts: 2}
	e.s3.putStatus, e.s3.putCode = 503, "SlowDown"
	for i := 0; i < 4; i++ {
		_ = e.runSubmit()
		e.clock.Advance(2 * time.Hour)
	}
	if r := e.row(id); r.State != anchor.StateFailed || r.Alert != AlertFailed {
		t.Fatalf("%+v", r)
	}
}
