// SPDX-License-Identifier: AGPL-3.0-only

package ops

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/api"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/partsched"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/problem"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/sysdb"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/task"
)

// The tests of the fixes of the second independent review of phase 12 (P12D-01 to P12D-09).

func TestUntrustedOwner(t *testing.T) {
	su, app := ownerInfo{"postgres", true}, ownerInfo{"app_owner", false}
	if got := untrustedOwner(false, su, su); got != "" {
		t.Errorf("superusers are trusted: %q", got)
	}
	if got := untrustedOwner(false, su, app); got != "app_owner" {
		t.Errorf("one owner that is not a superuser: %q", got)
	}
	if got := untrustedOwner(true, app); got != "" {
		t.Errorf("the setting on trusts every owner: %q", got)
	}
	if txt := ownerNotTrustedText("app_owner"); !strings.Contains(txt, "analytics.allowNonSuperuserOwners") || !strings.Contains(txt, `"app_owner"`) {
		t.Errorf("the text names the role and the setting: %s", txt)
	}
}

// allowOwners turns the cluster setting on or off in the system schema of the test instance.
func allowOwners(t *testing.T, c *pgx.Conn, on bool) {
	t.Helper()
	if _, err := sysdb.Migrate(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	v := "false"
	if on {
		v = "true"
	}
	execAll(t, c, `INSERT INTO kingres_cp.cluster_setting (key, value) VALUES ('`+settingNonSuperuserOwners+`', '`+v+`'::jsonb) ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value`)
}

// P12D-04: a table of a role that is not a superuser is refused with a clear code, the scheduler leaves it alone (and says so once), and the setting opens it.
func TestIntegrationANonSuperuserOwnerIsRefusedUnlessTheSettingIsOn(t *testing.T) {
	o, c, _, _ := policyOps(t)
	ctx := context.Background()
	execAll(t, c, `CREATE ROLE app_owner NOLOGIN`)
	if _, err := o.PutPartitionPolicy(ctx, "admin", "postgres", "public", "ev", dailyPolicy()); err != nil {
		t.Fatal(err)
	}
	pol, err := o.GetPartitionPolicy(ctx, "postgres", "public", "ev")
	if err != nil || pol.OwnerTrusted == nil || !*pol.OwnerTrusted || pol.TableOwner == "" {
		t.Fatalf("the owner of the table is a superuser: %+v %v", pol, err)
	}
	execAll(t, c, `ALTER TABLE public.ev OWNER TO app_owner`)
	pol, err = o.GetPartitionPolicy(ctx, "postgres", "public", "ev")
	if err != nil || pol.TableOwner != "app_owner" || pol.OwnerTrusted == nil || *pol.OwnerTrusted {
		t.Fatalf("the policy says whose table it is: %+v %v", pol, err)
	}
	now := time.Now()
	_, err = o.PrepareMaintain(ctx, "admin", "postgres", "public", "ev", &now, "manual")
	if p := wantCode(t, err, problem.PartitionPolicyConflict); !strings.Contains(p.Detail, "analytics.allowNonSuperuserOwners") {
		t.Errorf("the refusal names the setting: %s", p.Detail)
	}
	_, err = o.PutPartitionPolicy(ctx, "admin", "postgres", "public", "ev", dailyPolicy())
	wantCode(t, err, problem.PartitionPolicyConflict)
	// the scheduler: one issue, no task, no failure, however many ticks
	execAll(t, c, `UPDATE kingres.timepart_policy_data SET next_run_at = now() - interval '1 minute'`)
	s, _ := partsched.New(partsched.Config{Backend: PartitionBackend{O: o}})
	for i := 0; i < 4; i++ {
		if started, err := s.Tick(ctx); err != nil || len(started) != 0 {
			t.Fatalf("tick %d: %v %v", i, started, err)
		}
	}
	if tasks, _ := o.Engine().List(ctx, 20); len(tasks) != 0 {
		t.Errorf("%d tasks for a table that is refused", len(tasks))
	}
	if _, _, failures := s.Counts(); failures != 0 {
		t.Errorf("a refusal is not a failure: %d", failures)
	}
	// the setting on: the table runs
	allowOwners(t, c, true)
	pol, _ = o.GetPartitionPolicy(ctx, "postgres", "public", "ev")
	if pol.OwnerTrusted == nil || !*pol.OwnerTrusted {
		t.Errorf("the setting on trusts the owner: %+v", pol)
	}
	if _, err := o.PutPartitionPolicy(ctx, "admin", "postgres", "public", "ev", dailyPolicy()); err != nil {
		t.Fatal(err)
	}
	if got := runMaintain(t, o, time.Now()); got.State != task.Done {
		t.Errorf("with the setting on: %s %+v", got.State, got.Error)
	}
	allowOwners(t, c, false)
	_, err = o.PrepareMaintain(ctx, "admin", "postgres", "public", "ev", &now, "manual")
	wantCode(t, err, problem.PartitionPolicyConflict)
}

// P12D-04: the same rule for the conversion of a table, and the preview says why.
func TestIntegrationAConversionOfANonSuperuserOwnerIsRefusedUnlessTheSettingIsOn(t *testing.T) {
	o, c := storageOps(t)
	ctx := context.Background()
	execAll(t, c, `CREATE ROLE app_owner NOLOGIN`, `CREATE TABLE public.plain_t (id int)`, `ALTER TABLE public.plain_t OWNER TO app_owner`)
	req := api.TableStorageRequest{AccessMethod: "columnar"}
	prev, err := o.PreviewSetStorage(ctx, "admin", "postgres", "public", "plain_t", "", req)
	if err != nil || prev.WillConvert || prev.OwnerTrusted == nil || *prev.OwnerTrusted || prev.TableOwner != "app_owner" {
		t.Fatalf("%+v %v", prev, err)
	}
	found := false
	for _, b := range prev.Blockers {
		found = found || b.Code == "owner_not_trusted"
	}
	if !found {
		t.Errorf("blockers %+v", prev.Blockers)
	}
	_, err = o.PrepareSetStorage(ctx, "admin", "postgres", "public", "plain_t", "", req)
	p := wantCode(t, err, problem.ConversionRefused)
	if bl, ok := p.Blockers.([]api.TableBlocker); !ok || len(bl) == 0 || bl[0].Code != "owner_not_trusted" {
		t.Errorf("blockers %v", p.Blockers)
	}
	allowOwners(t, c, true)
	prev, err = o.PreviewSetStorage(ctx, "admin", "postgres", "public", "plain_t", "", req)
	if err != nil || !prev.WillConvert || prev.OwnerTrusted == nil || !*prev.OwnerTrusted {
		t.Errorf("with the setting on: %+v %v", prev, err)
	}
}

// P12D-04: the setting is a cluster setting that the API accepts and a read gives back.
func TestUpdateClusterSettingsAcceptsTheAnalyticsSetting(t *testing.T) {
	o := &Ops{}
	_, err := o.UpdateClusterSettings(context.Background(), "admin", api.ClusterSettingsUpdateRequest{Analytics: &struct {
		AllowNonSuperuserOwners *bool `json:"allowNonSuperuserOwners,omitempty"`
	}{}})
	wantCode(t, err, problem.ValidationFailed) // the flag is required inside the object
}

// P12D-02: begin_run raises before any run exists. The failure is on the policy: four ticks make one task, and the schedule moved.
func TestIntegrationABeginThatRaisesBacksOffAndMovesTheSchedule(t *testing.T) {
	o, c, _, _ := policyOps(t)
	ctx := context.Background()
	if _, err := o.PutPartitionPolicy(ctx, "admin", "postgres", "public", "ev", dailyPolicy()); err != nil {
		t.Fatal(err)
	}
	execAll(t, c, `DO $$ DECLARE r regprocedure; BEGIN FOR r IN SELECT oid::regprocedure FROM pg_proc WHERE proname = 'timepart_begin_run' AND pronamespace = 'kingres'::regnamespace
		LOOP EXECUTE format('ALTER FUNCTION %s RENAME TO timepart_begin_run_x', r); END LOOP; END $$`)
	s, _ := partsched.New(partsched.Config{Backend: PartitionBackend{O: o}})
	for i := 0; i < 4; i++ {
		if _, err := s.Tick(ctx); err != nil {
			t.Fatal(err)
		}
		o.Wait()
	}
	tasks, _ := o.Engine().List(ctx, 20)
	if len(tasks) != 1 || tasks[0].State != task.Failed {
		t.Fatalf("%d tasks after four ticks: %+v", len(tasks), tasks)
	}
	if n := scalar(t, c, `SELECT count(*) FROM kingres.timepart_run_data`); n != "0" {
		t.Errorf("%s runs", n)
	}
	if ok := scalar(t, c, `SELECT next_run_at > now() + interval '4 minutes' FROM kingres.timepart_policy_data`); ok != "true" {
		t.Error("the failure is not on the policy: next_run_at did not move")
	}
	// a leader that starts afresh (an empty memory) reads the same from the policy
	execAll(t, c, `UPDATE kingres.timepart_policy_data SET next_run_at = now() - interval '1 minute'`)
	o.failed = failedTasks{}
	if started, err := s.Tick(ctx); err != nil || len(started) != 1 {
		t.Fatalf("the policy is due again after its back-off: %v %v", started, err)
	}
	o.Wait()
	execAll(t, c, `UPDATE kingres.timepart_policy_data SET next_run_at = now() - interval '1 minute'`)
	if started, err := s.Tick(ctx); err != nil || len(started) != 0 {
		t.Errorf("the memory of the failure holds the next tick off: %v %v", started, err)
	}
}

// P12D-01: a time far after the last run of the policy is a clock that stepped (or a stop that no policy bridges): the scheduled run is refused as a
// ClockError, a run by hand goes on.
func TestIntegrationAScheduledRunFarAfterTheLastRunIsRefused(t *testing.T) {
	o, c, _, _ := policyOps(t)
	ctx := context.Background()
	if _, err := o.PutPartitionPolicy(ctx, "admin", "postgres", "public", "ev", dailyPolicy()); err != nil {
		t.Fatal(err)
	}
	execAll(t, c, `UPDATE kingres.timepart_policy_data SET next_run_at = now() - interval '1 minute'`)
	_, err := o.StartMaintain(ctx, "postgres", "public", "ev", time.Now().Add(30*24*time.Hour))
	var ce *partsched.ClockError
	if !errors.As(err, &ce) {
		t.Fatalf("a scheduled run 30 days on: %v", err)
	}
	if tasks, _ := o.Engine().List(ctx, 20); len(tasks) != 0 {
		t.Errorf("%d tasks", len(tasks))
	}
	now := time.Now().Add(30 * 24 * time.Hour)
	if _, err := o.PrepareMaintain(ctx, "admin", "postgres", "public", "ev", &now, "manual"); err != nil {
		t.Errorf("a run by hand is bounded by the skew only: %v", err)
	}
	// within the horizon the run is accepted
	if started, err := o.StartMaintain(ctx, "postgres", "public", "ev", time.Now().Add(2*24*time.Hour)); err != nil || !started {
		t.Errorf("two days on: %v %v", started, err)
	}
	o.Wait()
}

// P12D-09: the back-off looks at the last run only.
func TestIntegrationBackOffLooksAtTheLastRunOnly(t *testing.T) {
	o, c, _, _ := policyOps(t)
	ctx := context.Background()
	if _, err := o.PutPartitionPolicy(ctx, "admin", "postgres", "public", "ev", dailyPolicy()); err != nil {
		t.Fatal(err)
	}
	if o.recentlyFailed(ctx, "postgres", "public", "ev") {
		t.Fatal("no run yet")
	}
	execAll(t, c, `ALTER FUNCTION kingres.timepart_run_action(bigint, text) RENAME TO timepart_run_action_x`)
	if got := runMaintain(t, o, time.Now()); got.State != task.Failed {
		t.Fatalf("%s", got.State)
	}
	if !o.recentlyFailed(ctx, "postgres", "public", "ev") {
		t.Fatal("the last run failed a moment ago")
	}
	execAll(t, c, `ALTER FUNCTION kingres.timepart_run_action_x(bigint, text) RENAME TO timepart_run_action`)
	old := maintainBackoff
	maintainBackoff = 0
	if got := runMaintain(t, o, time.Now()); got.State != task.Done {
		t.Fatalf("%s %+v", got.State, got.Error)
	}
	maintainBackoff = old
	if o.recentlyFailed(ctx, "postgres", "public", "ev") {
		t.Error("a run that succeeded after the failure lifts the back-off")
	}
}

// P12D-09: when the task store cannot say whether a run is going, nothing goes on.
type brokenActive struct{ task.Store }

func (brokenActive) ActiveFor(context.Context, string, string, string) (bool, error) {
	return false, errors.New("store down")
}

func TestMaintainRunningFailsClosed(t *testing.T) {
	o := &Ops{}
	e, err := task.NewEngine(brokenActive{task.NewMemStore()}, o.Definitions(), task.Options{Holder: "A", Lease: time.Minute, StoreTries: 1})
	if err != nil {
		t.Fatal(err)
	}
	o.SetEngine(e)
	running, err := o.maintainRunning(context.Background(), "postgres", "public", "ev")
	if running {
		t.Error("no run is reported")
	}
	wantCode(t, err, problem.ServiceUnavailable)
}

// P12D-09: the kind registers the hooks that close a run that no step could close.
func TestMaintainDefinitionHasTheHookAndTheSteps(t *testing.T) {
	o := &Ops{}
	for _, d := range o.Definitions() {
		if d.Kind == KindMaintain {
			if d.OnFail == nil {
				t.Error("partition.maintain has no OnFail: a cancelled task would leave its run going")
			}
			return
		}
	}
	t.Fatal("partition.maintain is not defined")
}

// P12D-03: a standby of a server that was not restarted blocks the conversion, and the message names it and the order of the restart.
func TestAStandbyThatDidNotLoadTheLibraryBlocksAConversion(t *testing.T) {
	c := newFakeCluster("a", "b")
	c.ext["b"].preloaded = map[string]bool{}
	c.ext["b"].notLoadedOn = []string{"instance 2 (standby)"}
	p := storageParams{Database: "shop", Schema: "public", Table: "t", AccessMethod: "columnar"}
	err := c.ops["a"].ensureStorageExtensions(context.Background(), p, true)
	var pr *problem.Problem
	if !errors.As(err, &pr) || pr.Code != problem.ExtensionMissing || !strings.Contains(pr.Detail, "instance 2 (standby)") || !strings.Contains(pr.Detail, "standbys first") {
		t.Fatalf("%v", err)
	}
	// the same on the server's own check: a standby that did not load the library fails the library check the leader asks for
	c.ext["a"].preloaded = map[string]bool{}
	c.ext["a"].notLoadedOn = []string{"instance 2 (standby)"}
	err = c.ops["a"].extCheckLocal(context.Background(), extensionOp{Database: "shop", Extension: ExtColumnar, Preload: true})
	if !errors.As(err, &pr) && err == nil {
		t.Fatal("no refusal")
	}
}
