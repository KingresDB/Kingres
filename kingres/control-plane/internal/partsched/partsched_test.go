// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package partsched

import (
	"context"
	"errors"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeBackend is the cluster as the scheduler sees it: policies with a time they are due, the tables that have an unfinished task, and the findings.
type fakeBackend struct {
	mu        sync.Mutex
	policies  map[string]time.Time // subject -> next_run_at
	running   map[string]bool      // unfinished task of the subject
	started   []string
	failOn    map[string]error
	dueErr    error
	checks    []Check
	asOfSeen  []time.Time
	startAsOf []time.Time
	// unread is the list of databases that exist and that Checks could not read.
	unread []string
}

func newFake() *fakeBackend {
	return &fakeBackend{policies: map[string]time.Time{}, running: map[string]bool{}, failOn: map[string]error{}}
}

func split(subject string) Due {
	db, rest, _ := strings.Cut(subject, "/")
	sch, tbl, _ := strings.Cut(rest, ".")
	return Due{Database: db, Schema: sch, Table: tbl}
}

func (f *fakeBackend) Due(_ context.Context, asOf time.Time) ([]Due, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.asOfSeen = append(f.asOfSeen, asOf)
	if f.dueErr != nil {
		return nil, f.dueErr
	}
	var out []Due
	for s, at := range f.policies {
		if !at.After(asOf) {
			out = append(out, split(s))
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Subject() < out[j].Subject() })
	return out, nil
}

func (f *fakeBackend) Start(_ context.Context, d Due, asOf time.Time) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.startAsOf = append(f.startAsOf, asOf)
	s := d.Subject()
	if err := f.failOn[s]; err != nil {
		return false, err
	}
	if f.running[s] {
		return false, nil
	}
	f.running[s] = true
	f.started = append(f.started, s)
	return true, nil
}

func (f *fakeBackend) Checks(context.Context) ([]Check, []string, error) {
	return f.checks, f.unread, nil
}

type fakeAlerts struct {
	mu        sync.Mutex
	open      map[OpenAlert]string
	raised    int
	cleared   []OpenAlert
	failRaise error
}

func newAlerts() *fakeAlerts { return &fakeAlerts{open: map[OpenAlert]string{}} }

func (a *fakeAlerts) Open(context.Context, []string) ([]OpenAlert, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	var out []OpenAlert
	for k := range a.open {
		out = append(out, k)
	}
	return out, nil
}

func (a *fakeAlerts) Raise(_ context.Context, kind, severity, subject, message string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.failRaise != nil {
		return a.failRaise
	}
	a.raised++
	a.open[OpenAlert{kind, subject}] = severity + ": " + message
	return nil
}

func (a *fakeAlerts) Clear(_ context.Context, kind, subject string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.cleared = append(a.cleared, OpenAlert{kind, subject})
	delete(a.open, OpenAlert{kind, subject})
	return nil
}

type fakeClock struct{ t time.Time }

func (c *fakeClock) Now() time.Time { return c.t }

func newSched(t *testing.T, b Backend, a Alerts, clk *fakeClock) *Scheduler {
	t.Helper()
	s, err := New(Config{Backend: b, Alerts: a, Now: clk.Now, Interval: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestTickStartsWhatIsDueAndAsksWithTheInjectedClock(t *testing.T) {
	clk := &fakeClock{t: time.Date(2026, 11, 22, 10, 0, 0, 0, time.UTC)}
	b := newFake()
	b.policies["shop/public.events"] = clk.t.Add(-time.Minute)
	b.policies["shop/public.logs"] = clk.t // due at exactly now
	b.policies["shop/public.later"] = clk.t.Add(time.Second)
	b.policies["crm/public.audit"] = clk.t.Add(-time.Hour)
	s := newSched(t, b, nil, clk)
	got, err := s.Tick(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, d := range got {
		names = append(names, d.Subject())
	}
	if strings.Join(names, ",") != "crm/public.audit,shop/public.events,shop/public.logs" {
		t.Errorf("started %v", names)
	}
	if len(b.startAsOf) != 3 || !b.startAsOf[0].Equal(clk.t) || !b.startAsOf[2].Equal(clk.t) {
		t.Errorf("Start was given %v, the clock says %v", b.startAsOf, clk.t)
	}
	if len(b.asOfSeen) != 1 || !b.asOfSeen[0].Equal(clk.t) {
		t.Errorf("asked at %v, the clock says %v", b.asOfSeen, clk.t)
	}
	// time passes: the one that was not due is
	clk.t = clk.t.Add(2 * time.Second)
	b.running = map[string]bool{}
	got, _ = s.Tick(context.Background())
	if len(got) != 4 {
		t.Errorf("after two seconds: %v", got)
	}
}

func TestOneTaskPerSubjectAndTheNextTickStartsAgainWhenItEnds(t *testing.T) {
	clk := &fakeClock{t: time.Date(2026, 11, 22, 10, 0, 0, 0, time.UTC)}
	b := newFake()
	b.policies["shop/public.events"] = clk.t.Add(-time.Hour)
	s := newSched(t, b, nil, clk)
	if got, _ := s.Tick(context.Background()); len(got) != 1 {
		t.Fatalf("first tick: %v", got)
	}
	// the task is still running at the next ticks: nothing new, however long it takes
	for i := 0; i < 3; i++ {
		clk.t = clk.t.Add(time.Minute)
		if got, err := s.Tick(context.Background()); err != nil || len(got) != 0 {
			t.Fatalf("tick %d while the task runs: %v %v", i, got, err)
		}
	}
	if len(b.started) != 1 {
		t.Fatalf("started %v", b.started)
	}
	// it ends (the policy is due again after run_every)
	b.running["shop/public.events"] = false
	if got, _ := s.Tick(context.Background()); len(got) != 1 {
		t.Errorf("after the task ended: %v", got)
	}
	if ticks, started, failures := s.Counts(); ticks != 5 || started != 2 || failures != 0 {
		t.Errorf("counts %d %d %d", ticks, started, failures)
	}
}

func TestAFailureToStartDoesNotStopTheOthersAndIsTriedAgain(t *testing.T) {
	clk := &fakeClock{t: time.Date(2026, 11, 22, 10, 0, 0, 0, time.UTC)}
	b := newFake()
	b.policies["a/public.t1"] = clk.t
	b.policies["b/public.t2"] = clk.t
	b.failOn["a/public.t1"] = errors.New("the library is not preloaded")
	s := newSched(t, b, nil, clk)
	got, err := s.Tick(context.Background())
	if err != nil || len(got) != 1 || got[0].Subject() != "b/public.t2" {
		t.Fatalf("%v %v", got, err)
	}
	if _, _, failures := s.Counts(); failures != 1 {
		t.Errorf("failures %d", failures)
	}
	delete(b.failOn, "a/public.t1")
	got, _ = s.Tick(context.Background())
	if len(got) != 1 || got[0].Subject() != "a/public.t1" {
		t.Errorf("the next tick: %v", got)
	}
}

func TestADueErrorStartsNothing(t *testing.T) {
	b := newFake()
	b.dueErr = errors.New("the instance is in recovery")
	s := newSched(t, b, nil, &fakeClock{t: time.Now()})
	got, err := s.Tick(context.Background())
	if err == nil || len(got) != 0 || len(b.started) != 0 {
		t.Errorf("%v %v %v", got, err, b.started)
	}
}

func TestACancelledContextStopsTheTick(t *testing.T) {
	clk := &fakeClock{t: time.Now()}
	b := newFake()
	for _, n := range []string{"a/s.t1", "a/s.t2", "a/s.t3"} {
		b.policies[n] = clk.t
	}
	s := newSched(t, b, nil, clk)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	got, err := s.Tick(ctx)
	if !errors.Is(err, context.Canceled) || len(got) != 0 {
		t.Errorf("%v %v", got, err)
	}
}

func TestJobs(t *testing.T) {
	b := newFake()
	s, err := New(Config{Backend: b})
	if err != nil {
		t.Fatal(err)
	}
	jobs := s.Jobs()
	if len(jobs) != 1 || jobs[0].Name != JobSchedule || jobs[0].Every != time.Minute {
		t.Errorf("without alerts: %+v", jobs)
	}
	s, _ = New(Config{Backend: b, Alerts: newAlerts(), Interval: 30 * time.Second, AlertEvery: 3 * time.Minute})
	jobs = s.Jobs()
	if len(jobs) != 2 || jobs[0].Every != 30*time.Second || jobs[1].Name != JobAlerts || jobs[1].Every != 3*time.Minute {
		t.Errorf("with alerts: %+v", jobs)
	}
	if _, err := New(Config{}); err == nil {
		t.Error("no backend accepted")
	}
}

func TestAlertsAreRaisedAndClearedFromTheChecks(t *testing.T) {
	clk := &fakeClock{t: time.Now()}
	b := newFake()
	a := newAlerts()
	s := newSched(t, b, a, clk)
	b.checks = []Check{
		{Database: "shop", Schema: "public", Table: "events", Problem: "ahead_low", Detail: "ends in 1 day"},
		{Database: "shop", Schema: "public", Table: "logs", Problem: "last_run_failed", Detail: "failed: lock_timeout"},
		{Database: "shop", Schema: "public", Table: "other", Problem: "misaligned"}, // not an alert
		{Database: "shop", Schema: "public", Table: "other", Problem: "retire_blocked"},
	}
	if err := s.CheckAlerts(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(a.open) != 2 {
		t.Fatalf("open alerts %v", a.open)
	}
	if _, ok := a.open[OpenAlert{AlertAheadLow, "database:shop|tableschema:public|tablename:events"}]; !ok {
		t.Errorf("no partitionsAheadLow: %v", a.open)
	}
	if v, ok := a.open[OpenAlert{AlertPolicyFailed, "database:shop|tableschema:public|tablename:logs"}]; !ok || !strings.Contains(v, "lock_timeout") {
		t.Errorf("no partitionPolicyFailed: %v", a.open)
	}
	// a second pass with the same findings raises them again (an upsert) and clears nothing
	if err := s.CheckAlerts(context.Background()); err != nil || len(a.cleared) != 0 || len(a.open) != 2 {
		t.Errorf("second pass: %v, cleared %v, open %v", err, a.cleared, a.open)
	}
	// the policy of events was fixed
	b.checks = b.checks[1:2]
	if err := s.CheckAlerts(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(a.cleared) != 1 || a.cleared[0] != (OpenAlert{AlertAheadLow, "database:shop|tableschema:public|tablename:events"}) || len(a.open) != 1 {
		t.Errorf("cleared %v, open %v", a.cleared, a.open)
	}
	// nothing is wrong any more
	b.checks = nil
	if err := s.CheckAlerts(context.Background()); err != nil || len(a.open) != 0 {
		t.Errorf("%v %v", err, a.open)
	}
}

func TestAnAlertThatCannotBeRaisedDoesNotStopTheClearing(t *testing.T) {
	b := newFake()
	a := newAlerts()
	a.open[OpenAlert{AlertAheadLow, "database:gone|tableschema:public|tablename:t"}] = "warning: old"
	a.failRaise = errors.New("the system database is busy")
	s := newSched(t, b, a, &fakeClock{t: time.Now()})
	b.checks = []Check{{Database: "shop", Schema: "public", Table: "events", Problem: "ahead_low"}}
	err := s.CheckAlerts(context.Background())
	if err == nil || len(a.cleared) != 1 {
		t.Errorf("err %v, cleared %v", err, a.cleared)
	}
}

// An alert of a database that could not be read this time is left as it is (a restart, a connection limit, a leader whose instance does not show the table): it is
// neither cleared nor raised again. When the database is read and has no finding, the alert is cleared.
func TestAnAlertOfADatabaseThatWasNotReadIsLeftAlone(t *testing.T) {
	b := newFake()
	a := newAlerts()
	s := newSched(t, b, a, &fakeClock{t: time.Now()})
	keep := OpenAlert{AlertPolicyFailed, "database:shop|tableschema:public|tablename:logs"}
	other := OpenAlert{AlertPolicyFailed, "database:crm|tableschema:public|tablename:audit"}
	a.open[keep], a.open[other] = "warning: x", "warning: y"
	b.unread = []string{"shop"} // shop exists and could not be read
	if err := s.CheckAlerts(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, ok := a.open[keep]; !ok {
		t.Error("the alert of the database that was not read was cleared")
	}
	if _, ok := a.open[other]; ok {
		t.Error("the alert of a database that was read and has no finding stayed")
	}
	b.unread = nil
	if err := s.CheckAlerts(context.Background()); err != nil || len(a.open) != 0 {
		t.Errorf("shop read and fine: %v %v", err, a.open)
	}
}

// A name with a bar or a percent sign cannot break the subject: the segments are escaped and read back as they were.
func TestAlertSubjectsEscapeNames(t *testing.T) {
	c := Check{Database: "sh|op", Schema: "pub%lic", Table: "ev|ents.x", Problem: "ahead_low"}
	k := alertKey(AlertAheadLow, c)
	if strings.Count(k.Subject, "|") != 2 {
		t.Fatalf("subject %q", k.Subject)
	}
	if got := subjectDatabase(k.Subject); got != "sh|op" {
		t.Errorf("database %q", got)
	}
}

// A policy that cannot run because its clock is not credible or its owner is not trusted is one alert for the table, not a failure at every tick: the failures are
// not counted, the alert stays while the reason does, and goes when it does.
func TestAClockOrBlockedPolicyIsOneAlertAndNotAFailurePerTick(t *testing.T) {
	clk := &fakeClock{t: time.Date(2026, 11, 22, 10, 0, 0, 0, time.UTC)}
	b := newFake()
	a := newAlerts()
	s := newSched(t, b, a, clk)
	b.policies["shop/public.clock"] = clk.t
	b.policies["shop/public.owner"] = clk.t
	b.policies["shop/public.fine"] = clk.t
	b.failOn["shop/public.clock"] = &ClockError{Msg: "the clock of the leader is 9m0s ahead of the instance"}
	b.failOn["shop/public.owner"] = &BlockedError{Msg: "the owner is not a superuser and the cluster setting is off"}
	for i := 0; i < 4; i++ {
		started, err := s.Tick(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if i == 0 && (len(started) != 1 || started[0].Table != "fine") {
			t.Fatalf("started %v", started)
		}
		b.running = map[string]bool{}
		if err := s.CheckAlerts(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, failures := s.Counts(); failures != 0 {
		t.Errorf("%d failures counted for refusals that are alerts", failures)
	}
	if len(a.open) != 2 || a.raised != 8 { // an upsert at every pass; two alerts open
		t.Errorf("open %v, raised %d", a.open, a.raised)
	}
	for _, tbl := range []string{"clock", "owner"} {
		if _, ok := a.open[OpenAlert{AlertPolicyFailed, "database:shop|tableschema:public|tablename:" + tbl}]; !ok {
			t.Errorf("no alert for %s: %v", tbl, a.open)
		}
	}
	// the reasons go: the next tick starts them and the alerts are cleared
	delete(b.failOn, "shop/public.clock")
	delete(b.failOn, "shop/public.owner")
	if started, _ := s.Tick(context.Background()); len(started) != 3 {
		t.Errorf("started %v", started)
	}
	if err := s.CheckAlerts(context.Background()); err != nil || len(a.open) != 0 {
		t.Errorf("%v %v", err, a.open)
	}
}

// An alert of a database that is gone, or has no extension any more, is cleared (it is not among the databases that could not be read).
func TestAnAlertOfADroppedDatabaseIsCleared(t *testing.T) {
	b := newFake()
	a := newAlerts()
	s := newSched(t, b, a, &fakeClock{t: time.Now()})
	a.open[OpenAlert{AlertAheadLow, "database:dropped|tableschema:public|tablename:t"}] = "warning: x"
	if err := s.CheckAlerts(context.Background()); err != nil || len(a.open) != 0 {
		t.Errorf("%v %v", err, a.open)
	}
}
