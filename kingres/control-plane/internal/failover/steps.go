// SPDX-License-Identifier: AGPL-3.0-only

package failover

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/fence"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/task"
)

// SystemDB is the database of the system catalog; every sharded database has a catalog of its own besides it.
const SystemDB = "kingres_system"

// Step names. The names of a group's steps are "Group <n>: " plus one of these.
const (
	StepCheck       = "Check that the failover can start"
	StepFreeze      = "Freeze the group"
	StepStop        = "Stop the old primary"
	StepLease       = "Wait for the old lease"
	StepChoose      = "Choose the standby"
	StepHandOver    = "Hand the lease to the new server"
	StepPromote     = "Promote the standby"
	StepCatalog     = "Make the catalog follow"
	StepCheckCat    = "Check the catalog"
	StepRepoint     = "Point the other standbys at the new primary"
	StepRecords     = "Update the standby records"
	StepSyncNames   = "Check synchronous_standby_names"
	StepPrepared    = "Resolve prepared transactions"
	StepClients     = "Check that clients arrive"
	StepUnfreeze    = "Lift the freeze"
	StepWriteReport = "Write the report"
)

// groupSteps is the order of the steps of one group.
var groupSteps = []string{StepFreeze, StepStop, StepLease, StepChoose, StepHandOver, StepPromote, StepCatalog, StepCheckCat,
	StepRepoint, StepRecords, StepSyncNames, StepPrepared, StepClients, StepUnfreeze}

// StepName is the name of a step of a group.
func StepName(group int32, step string) string { return fmt.Sprintf("Group %d: %s", group, step) }

// Fault points of the steps, for the tests that end the process or block at a named
// place inside a step (nil in the product). A point is "<step key>:<where>".
type FaultFunc func(point string)

// Runner runs the steps of the task. Every field but the ports has a default.
type Runner struct {
	Peers Peers
	Cat   Catalog
	Plan  Planner
	// NewRows makes the persistence of one run.
	NewRows func(taskID int64) Rows
	Log     *slog.Logger
	// Sleep and Now are the clock (tests replace them).
	Sleep func(ctx context.Context, d time.Duration) error
	Now   func() time.Time
	Fault FaultFunc
	// Inject is the failure hook of the tests (nil in the product): at "<step name>:before" it answers "fail" (the step fails with
	// a code that is not retried) or "pause" (the task pauses), or "" for nothing.
	Inject func(point string) string

	// The bounds. Each is named, none is derived from a hard-coded formula in a step.
	FreezeTTL          time.Duration // the freeze of a group; renewed by every step that relies on it (default 5 min)
	GoneTTL            time.Duration // the standbys of a dead server are not waited for (default 5 min)
	LeaseWait          time.Duration // the wait for the old lease at the grantors (default 25 s)
	LeasePoll          time.Duration // (default 1 s)
	QuorumWait         time.Duration // a leader without a majority waits this long, then asks (default 60 s)
	ReadBound          time.Duration // the positions must stop moving within this (default 15 s)
	HandWait           time.Duration // the new holder's wait to hold the lease (default 15 s)
	HandTries          int           // (default 3)
	PromoteTries       int           // repeats of pg_promote that did not answer (default 3)
	PromoteUnavailable int           // answers "the instance cannot be reached" that are waited for, a pause each (default 18)
	PromotePause       time.Duration // (default 10 s)
	RepointWait        time.Duration // (default 60 s, in the callee)
	SyncWait           time.Duration // (default 2 min)
	PreparedWait       time.Duration // (default 2 min)
	PreparedEvery      time.Duration // (default 30 s)
	ClientsWait        time.Duration // (default 30 s)
	QuiesceWait        time.Duration // the stop of the writes of a switchover is repeated this long (default 2 min)
	DrainWait          time.Duration // the wait for prepared transactions of a switchover (default 1 min)
	DrainEvery         time.Duration // (default 10 s)
	MarkWait           time.Duration // the wait for the standby to replay the mark (default 10 s)
	VerifyWait         time.Duration // the proof is repeated this long (default 30 s)
	OldServerWait      time.Duration // the calls to the old primary's server after the promotion wait this long (default 15 s)
	MapVerdictWait     time.Duration // the old leader of a planned switchover of the map group waits this long for the candidate to promote or to refuse (default 6 min)
	// PromoteNode are the bounds of the repeat loop of kingres.promote_node.
	PromoteNode PromoteOptions
	// PositionTimeout is the time of one read of a position (default 5 s).
	PositionTimeout time.Duration

	mu sync.Mutex
	// paused is when the writes of a group of a switchover stopped (only for the measurement of the pause).
	paused map[string]time.Time
}

func (r *Runner) log() *slog.Logger {
	if r.Log != nil {
		return r.Log
	}
	return slog.New(slog.DiscardHandler)
}

func (r *Runner) sleep(ctx context.Context, d time.Duration) error {
	if r.Sleep != nil {
		return r.Sleep(ctx, d)
	}
	return sleepCtx(ctx, d)
}

func (r *Runner) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

func orDur(v, d time.Duration) time.Duration {
	if v > 0 {
		return v
	}
	return d
}

func orInt(v, d int) int {
	if v > 0 {
		return v
	}
	return d
}

func (r *Runner) fault(point string) {
	if r.Fault != nil {
		r.Fault(point)
	}
}

// Definition is the task definition (kind server.failover): the steps come from the
// input; a cancel is accepted only before the first destructive step.
func (r *Runner) Definition(onFail func(ctx context.Context, t task.Task)) task.Definition {
	return r.DefinitionOf(KindFailover, onFail)
}

// DefinitionOf is Definition for a kind: server.failover, or replication.restorePreferredPrimaries (the steps come from the input).
func (r *Runner) DefinitionOf(kind string, onFail func(ctx context.Context, t task.Task)) task.Definition {
	return task.Definition{
		Kind:        kind,
		Dynamic:     r.steps,
		Cancellable: Cancellable,
		OnFail:      onFail,
	}
}

// Cancellable is the cancel guard. The point of no return of this revision is the
// first step that stops or fences an old primary (the group has no primary from the
// moment the old one is stopped until a standby is promoted): from the moment a step
// "Stop the old primary" has started, the task rolls forward only. A cancel before
// it only lifts the freezes (the run's OnFail).
func Cancellable(t task.Task) bool {
	if !t.State.Active() {
		return false
	}
	for _, s := range t.Steps {
		if strings.HasSuffix(s.Name, ": "+StepStop) && s.State != task.StepPending {
			return false
		}
		// The promotion is the point of no return of a switchover (design 5.3).
		if strings.HasSuffix(s.Name, ": "+StepPromote) && s.State != task.StepPending {
			return false
		}
	}
	return true
}

// groupStepState is what a group's block has done, read from the step list.
func inputOf(env *task.Env) (Input, error) {
	var in Input
	if err := json.Unmarshal(env.Task.Input, &in); err != nil {
		return in, fmt.Errorf("the input of the task is not valid: %w", err)
	}
	return in, nil
}

type stepFunc func(ctx context.Context, env *task.Env, sc *stepCtx) error

// stepCtx is what a step has at hand.
type stepCtx struct {
	r     *Runner
	in    Input
	rows  Rows
	group int32
	key   string
	env   *task.Env
	// newServer is the server of the pinned standby when a step knows it from its own state and cannot read it from the group row: after the clean stop of
	// the map group the system database is the stopped instance (F5-01).
	newServer string
}

func (s *stepCtx) detail(ctx context.Context, format string, a ...any) {
	_ = s.env.SetDetail(ctx, fmt.Sprintf(format, a...))
}

func (s *stepCtx) event(ctx context.Context, kind string, d map[string]any) {
	var g *int32
	if s.group >= 0 {
		gg := s.group
		g = &gg
	}
	if err := s.rows.Event(ctx, g, s.key, kind, d); err != nil && !errors.Is(err, ErrLeaseLost) {
		s.r.log().Warn("failover: recording an event", "step", s.key, "kind", kind, "error", err)
	}
}

// steps builds the steps from the input.
func (r *Runner) steps(input json.RawMessage) ([]task.Step, error) {
	var in Input
	if err := json.Unmarshal(input, &in); err != nil {
		return nil, err
	}
	out := []task.Step{r.step(StepCheck, -1, StepCheck, false, r.check)}
	for _, g := range mapLast(in) {
		g := g
		if in.graceful() {
			r.addGraceful(&out, g)
			continue
		}
		add := func(name string, irreversible bool, f stepFunc) {
			out = append(out, r.step(StepName(g, name), g, name, irreversible, f))
		}
		add(StepFreeze, false, r.freeze)
		add(StepStop, true, r.stopOld)
		add(StepLease, false, r.takeFence)
		add(StepChoose, false, r.choose)
		add(StepHandOver, false, r.handOver)
		add(StepPromote, true, r.promote)
		add(StepCatalog, false, r.catalogFollows)
		add(StepCheckCat, false, r.checkCatalog)
		add(StepRepoint, false, r.repoint)
		add(StepRecords, false, r.records)
		add(StepSyncNames, false, r.syncNames)
		add(StepPrepared, false, r.prepared)
		add(StepClients, false, r.clients)
		add(StepUnfreeze, false, r.unfreeze)
	}
	out = append(out, r.step(StepWriteReport, -1, StepWriteReport, false, r.report))
	return out, nil
}

func (r *Runner) step(name string, group int32, key string, irreversible bool, f stepFunc) task.Step {
	return task.Step{Name: name, Irreversible: irreversible, Run: func(ctx context.Context, env *task.Env) error {
		in, err := inputOf(env)
		if err != nil {
			return err
		}
		rows := r.NewRows(env.Task.ID)
		// A repeated request that resumed a paused task carries the acceptance of a loss and the targets of this
		// request: the record of the run has them.
		if run, ok, rerr := rows.Run(ctx); rerr == nil && ok && run.Options != nil {
			in.AllowDataLoss, in.Targets = run.Options.AllowDataLoss, run.Options.Targets
		}
		sc := &stepCtx{r: r, in: in, rows: rows, group: group, key: key, env: env}
		if in.Mode == ModeRestore && group >= 0 {
			// A group of a restore that failed before its promotion was rolled back: its remaining steps do nothing and the run goes on with the
			// next group (design 5.5, 4: failing back is not urgent, the report says what happened).
			if gr, ok, gerr := rows.Group(ctx, group); gerr == nil && ok && gr.State == "failed" {
				return task.Skip("the group failed earlier in this restore")
			}
		}
		r.fault(name + ":before")
		if r.Inject != nil {
			switch r.Inject(name + ":before") {
			case "fail":
				return fail(CodeBadPromoteTarget, false, "a failure injected by a test at %s", name)
			case "pause":
				return &PauseError{Reason: "a pause injected by a test at " + name}
			}
		}
		err = f(ctx, env, sc)
		if err != nil && in.Mode == ModeRestore && group >= 0 {
			if skip := sc.restoreContinues(ctx, err); skip != nil {
				return skip
			}
		}
		if err == nil {
			r.fault(name + ":after")
		}
		return err
	}}
}

// oldServerOf is the server whose primary fails: the server of the request.
func (sc *stepCtx) old() string { return sc.in.oldOf(sc.group) }

// row reads the group row; a missing row is a defect of the first step.
func (sc *stepCtx) row(ctx context.Context) (GroupRow, error) {
	g, ok, err := sc.rows.Group(ctx, sc.group)
	if err != nil {
		return g, err
	}
	if !ok {
		return g, fail(CodeSuperseded, false, "the record of group %d does not exist", sc.group)
	}
	return g, nil
}

func (sc *stepCtx) runRow(ctx context.Context) (RunRow, error) {
	return runRowOf(ctx, sc.rows, sc.in, sc.group)
}

// runRowOf is the record of the run with the term that the calls of group expect.
func runRowOf(ctx context.Context, rows Rows, in Input, group int32) (RunRow, error) {
	run, ok, err := rows.Run(ctx)
	if err != nil {
		return run, err
	}
	if !ok {
		return run, fail(CodeSuperseded, false, "the record of the run does not exist")
	}
	// The term the destructive calls of this group expect is the one the run read at its start, raised by the groups of
	// this run that were promoted before it: each promote_node raises it by one, and the run itself did it. Without this
	// the second group of a server that hosted two primaries met its own run's term and ended "superseded".
	// The groups are taken in the order they run (the map group is last in a switchover and in a hard failover of a live leader's server).
	for _, g := range mapLast(in) {
		if g == group {
			break
		}
		if gr, found, gerr := rows.Group(ctx, g); gerr == nil && found && gr.TermAfter != nil && *gr.TermAfter > run.ExpectedTerm {
			run.ExpectedTerm = *gr.TermAfter
		}
	}
	// The map group of a planned switchover is promoted by the bootstrap of the candidate, which raised the term; the new leader's engine runs the steps
	// that were not recorded with the term its row says (RecordPlanned).
	if group == 0 && (in.graceful() || in.MapLive) {
		if gr, found, gerr := rows.Group(ctx, 0); gerr == nil && found && gr.TermAfter != nil && *gr.TermAfter > run.ExpectedTerm {
			run.ExpectedTerm = *gr.TermAfter
		}
	}
	return run, nil
}

// ---------------------------------------------------------------------------------
// F0

// Refusal is a refusal of a request or of the first step, with the problem code the
// API answers.
type Refusal struct {
	Code    string
	Status  int
	Message string
	Group   *int32
}

func (e *Refusal) Error() string { return e.Code + ": " + e.Message }

// GroupsOn returns the groups whose primary runs on server, ascending (the map group
// first, because it is group 0).
func GroupsOn(nodes []Node, server string) []int32 {
	var out []int32
	for _, n := range nodes {
		if n.Role == "primary" && n.Host == server {
			out = append(out, n.Group)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// Preflight checks that a request can start (design F0). It is called by the request
// (before a task exists) and by the first step (the state may have changed).
func (r *Runner) Preflight(ctx context.Context, in Input) *Refusal {
	nodes, err := r.Cat.Nodes(ctx, SystemDB)
	if err != nil {
		return &Refusal{Code: "service_unavailable", Status: 503, Message: "The catalog cannot be read: " + err.Error()}
	}
	if _, superseded, err := r.Cat.Status(ctx, SystemDB); err != nil {
		return &Refusal{Code: "service_unavailable", Status: 503, Message: "The catalog status cannot be read: " + err.Error()}
	} else if superseded {
		return &Refusal{Code: "conflict", Status: 409, Message: "The catalog of this server has been superseded by a newer term."}
	}
	have := map[int32]bool{}
	for _, n := range nodes {
		if n.Role == "primary" && n.Host == in.oldOf(n.Group) {
			have[n.Group] = true
		}
	}
	for _, g := range in.Groups {
		g := g
		if !have[g] {
			return &Refusal{Code: "unprocessable", Status: 422, Message: fmt.Sprintf("Server %s does not run the primary of shard group %d.", in.oldOf(g), g), Group: &g}
		}
		if g == 0 && !in.graceful() && !in.MapLive {
			// Only a request that was resolved here for a live leader's server (MapLive) takes the map group in mode hard; a task that is not one (an
			// automatic failover of the leader's server, which the bootstrap handles when the server is not heard) has no way to hand the map group over.
			return &Refusal{Code: "unsupported_operation", Status: 422, Message: "The failover of the map group while its server is alive is asked of the manager leader with mode hard or graceful; this task cannot do it.", Group: &g}
		}
		if g == 0 && in.MapLive && in.oldOf(0) != r.Plan.Self() {
			return &Refusal{Code: "unsupported_operation", Status: 422, Group: &g,
				Message: "The map group is failed over by the task that runs on the server of its primary (the manager leader); ask the leader."}
		}
	}
	if len(in.Groups) == 0 {
		return &Refusal{Code: "unprocessable", Status: 422, Message: "Server " + in.ServerID + " runs the primary of no shard group."}
	}
	if in.graceful() {
		if ref := r.preflightGraceful(ctx, in); ref != nil {
			return ref
		}
	}
	nodeSet := map[int32]Node{}
	for _, n := range nodes {
		nodeSet[n.ID] = n
	}
	for _, g := range in.Groups {
		g := g
		recs, err := r.Plan.Standbys(ctx, g)
		if err != nil {
			return &Refusal{Code: "service_unavailable", Status: 503, Message: "The standbys cannot be read: " + err.Error()}
		}
		ready := 0
		for _, s := range recs {
			if s.State != "ready" {
				continue
			}
			if s.CatalogNode == 0 {
				return &Refusal{Code: "cluster_inconsistent", Status: 409, Group: &g,
					Message: fmt.Sprintf("The standby %d of shard group %d is not registered in the sharding catalog yet; the failover cannot promote it.", s.ID, g)}
			}
			if n, ok := nodeSet[s.CatalogNode]; !ok || n.Role != "standby" || n.Group != g {
				return &Refusal{Code: "cluster_inconsistent", Status: 409, Group: &g,
					Message: fmt.Sprintf("The standby %d of shard group %d is recorded in the catalog as node %d, which the catalog does not know as a standby of the group.", s.ID, g, s.CatalogNode)}
			}
			ready++
		}
		var oldNode *Node
		for _, n := range nodes {
			if n.Group == g && n.Role == "primary" && n.Host == in.oldOf(g) {
				n := n
				oldNode = &n
			}
		}
		if ref := r.registeredEverywhere(ctx, g, recs, oldNode); ref != nil {
			return ref
		}
		// F2-13: an old primary that answers and runs without a watchdog is covered by its lease manager alone (a hung
		// kingresd could not stop it): the failover does not count it as fenceable and refuses until it has one.
		st, serr := r.Peers.State(ctx, in.oldOf(g), g)
		if serr == nil && st.Watchdog != nil && !*st.Watchdog && len(st.OutOfRecovery()) > 0 {
			return &Refusal{Code: "cluster_inconsistent", Status: 409, Group: &g,
				Message: fmt.Sprintf("No watchdog runs for the primary of shard group %d on %s, so a hung kingresd there could not stop it.", g, in.oldOf(g))}
		}
		// F3-02, F3-12: a server that does not answer cannot be asked whether its lease would stop its primary. The lease
		// stops a primary only on a server whose plan says the group has a standby, so the leader asks its own record of
		// what that server held at the last check; without proof the failover goes on only when the operator accepts it.
		if errors.Is(serr, ErrUnreachable) && !in.AllowDataLoss {
			facts, ferr := r.Plan.Facts(ctx, g)
			if ferr != nil {
				return &Refusal{Code: "service_unavailable", Status: 503, Message: "The replication facts of the group cannot be read: " + ferr.Error()}
			}
			if facts.PrimaryPlanned == 0 {
				return &Refusal{Code: "cluster_inconsistent", Status: 409, Group: &g,
					Message: fmt.Sprintf("Server %s does not answer, and the leader has no proof that it knew of a standby of shard group %d at its last check: its lease would then not stop its primary, which clients may still reach. Repeat the request with allowDataLoss set to true to accept that the old primary is not proven fenced.", in.oldOf(g), g)}
			}
		}
		if ready == 0 {
			return &Refusal{Code: "no_replica_available", Status: 409, Group: &g, Message: fmt.Sprintf("Shard group %d has no standby that is ready to take over.", g)}
		}
		// FO-15: a standby that the primary names in synchronous_standby_names and that is not in the
		// catalog would make every commit on the promoted instance wait for ever.
		if ids, ok := r.Plan.PrimaryNames(ctx, in.oldOf(g), g); ok {
			known := map[int64]bool{}
			for _, s := range recs {
				if s.CatalogNode != 0 {
					if n, ok := nodeSet[s.CatalogNode]; ok && n.Role == "standby" {
						known[s.ID] = true
					}
				}
			}
			for _, id := range ids {
				if !known[id] {
					return &Refusal{Code: "cluster_inconsistent", Status: 409, Group: &g,
						Message: fmt.Sprintf("The primary of shard group %d names the standby %d in synchronous_standby_names, and the catalog does not have it.", g, id)}
				}
			}
		}
		if t := in.Targets[g]; t != "" {
			found := false
			for _, s := range recs {
				if s.Server == t && s.State == "ready" {
					found = true
				}
			}
			if !found {
				return &Refusal{Code: "unprocessable", Status: 422, Group: &g, Message: fmt.Sprintf("Server %s does not run a ready standby of shard group %d.", t, g)}
			}
		}
	}
	members, err := r.Plan.Members(ctx)
	if err != nil {
		return &Refusal{Code: "service_unavailable", Status: 503, Message: "The members cannot be read: " + err.Error()}
	}
	if !r.hearsMajority(ctx, members) {
		return &Refusal{Code: "quorum_unavailable", Status: 503, Message: "This server cannot reach a majority of the servers, so no shard group can be handed to another server safely."}
	}
	if ref := r.leaseSettings(ctx, members); ref != nil {
		return ref
	}
	if running, err := r.Plan.RebalanceRunning(ctx); err != nil {
		return &Refusal{Code: "service_unavailable", Status: 503, Message: "The rebalance state cannot be read: " + err.Error()}
	} else if running {
		return &Refusal{Code: "rebalance_in_progress", Status: 409, Message: "A rebalance is running; stop it first (the failover does not stop it in this version)."}
	}
	return nil
}

// hearsMajority says whether this server counts a strict majority of the members
// live, itself included.
func (r *Runner) hearsMajority(ctx context.Context, members []string) bool {
	live := 0
	for _, m := range members {
		if m == r.Plan.Self() || r.Plan.Live(ctx, m) == "live" {
			live++
		}
	}
	return live*2 > len(members)
}

// leaseSettings checks that every member that answers reports the same lease
// duration and margin (the safety argument needs equal settings). A member that does
// not answer cannot be checked: its grantor is judged by the others and by the
// liveness view; a member of an older version (404) is a version mismatch.
func (r *Runner) leaseSettings(ctx context.Context, members []string) *Refusal {
	var first *LeaseSettings
	firstOf := ""
	for _, m := range members {
		ls, err := r.Peers.LeaseSettings(ctx, m)
		if err != nil {
			if IsOlderServer(err) {
				return &Refusal{Code: "version_mismatch", Status: 409, Message: "Server " + m + " runs an older version of kingresd that does not report its lease settings."}
			}
			continue
		}
		if first == nil {
			x := ls
			first, firstOf = &x, m
			continue
		}
		if ls != *first {
			return &Refusal{Code: "lease_settings_differ", Status: 409,
				Message: fmt.Sprintf("Servers %s and %s use different lease settings (duration %d ms, margin %d ms against %d ms, %d ms).", firstOf, m, first.DurationMs, first.MarginMs, ls.DurationMs, ls.MarginMs)}
		}
	}
	return nil
}

func (r *Runner) check(ctx context.Context, env *task.Env, sc *stepCtx) error {
	// The record of the run is the proof that this step ran: a repeat (after a crash before the step was recorded
	// done) must not judge a cluster that has moved on, and it changes nothing.
	if _, ok, err := sc.rows.Run(ctx); err != nil {
		return err
	} else if ok {
		return nil
	}
	if ref := r.Preflight(ctx, sc.in); ref != nil {
		code := CodeStandbyUnreadable
		retry := false
		switch ref.Code {
		case "quorum_unavailable":
			code, retry = CodeQuorumUnavailable, true
		case "service_unavailable":
			code, retry = CodeQuorumUnavailable, true
		default:
			code = ref.Code
		}
		// The state changed since the request: what the request would have been refused for stops the task before anything is done.
		return fail(code, retry, "%s", ref.Message)
	}
	term, _, err := r.Cat.Status(ctx, SystemDB)
	if err != nil {
		return err
	}
	nodes, err := r.Cat.Nodes(ctx, SystemDB)
	if err != nil {
		return err
	}
	var rows []GroupRow
	for i, g := range sc.in.Groups {
		gr := GroupRow{Group: g, Seq: i, State: "pending", OldServer: sc.in.oldOf(g)}
		for _, n := range nodes {
			if n.Group == g && n.Role == "primary" && n.Host == sc.in.oldOf(g) {
				gr.OldNode = n.ID
			}
		}
		rows = append(rows, gr)
	}
	if err := sc.rows.Begin(ctx, sc.in, term, rows); err != nil {
		return err
	}
	// The groups that a restore leaves alone are in the report, as skipped, with the reason.
	for i, sk := range sc.in.Skipped {
		if err := sc.rows.Begin(ctx, sc.in, term, []GroupRow{{Group: sk.Group, Seq: len(rows) + i, State: "pending"}}); err != nil {
			return err
		}
		text := sk.Reason + ": " + sk.Text
		if err := sc.rows.Set(ctx, sk.Group, GroupSet{State: ptr("skipped"), Error: &text, Finished: true}); err != nil {
			return err
		}
	}
	// The members that did not answer the lease settings, and the servers this one does not hear, are in the first
	// event: a failover judged on a view that was partial says so.
	sc.event(ctx, "checked", map[string]any{"term": term, "groups": sc.in.Groups, "trigger": sc.in.Trigger})
	// F0b: when the server is not live, the primaries of the other groups stop waiting for the standbys that ran on
	// it (extension precondition 3): without it every failover of a dead server would wait for the grace of the others.
	if sc.in.Mode == ModeHard && r.Plan.Live(ctx, sc.in.ServerID) != "live" {
		for _, p := range primaryServers(nodes, sc.in.ServerID) {
			if err := r.Peers.StandbysGone(ctx, p, sc.in.ServerID, orDur(r.GoneTTL, 5*time.Minute)); err != nil {
				sc.event(ctx, "standbysGoneSkipped", map[string]any{"server": p, "error": err.Error()})
			}
		}
	}
	return nil
}

func primaryServers(nodes []Node, except string) []string {
	seen := map[string]bool{}
	var out []string
	for _, n := range nodes {
		if n.Role == "primary" && n.Host != except && !seen[n.Host] {
			seen[n.Host] = true
			out = append(out, n.Host)
		}
	}
	sort.Strings(out)
	return out
}

// ---------------------------------------------------------------------------------
// F1

// ensureFrozen sends the freeze of the group to the old primary's server and reads it
// back (the freeze is persisted on its server and lapses; a wall clock set forward
// can end it early). A server that does not answer needs no freeze: it is dead or cut
// off, and its lease fences it.
func (sc *stepCtx) ensureFrozen(ctx context.Context) error {
	r := sc.r
	ttl := orDur(r.FreezeTTL, 5*time.Minute)
	for try := 0; try < 3; try++ {
		if err := r.Peers.Freeze(ctx, sc.old(), sc.group, ttl); err != nil {
			if errors.Is(err, ErrUnreachable) || IsCode(err, "no_instance") || IsOlderServer(err) {
				return nil
			}
			return err
		}
		st, err := r.Peers.State(ctx, sc.old(), sc.group)
		if err != nil {
			return nil
		}
		if st.Frozen {
			return nil
		}
		if err := r.sleep(ctx, 200*time.Millisecond); err != nil {
			return err
		}
	}
	return fail(CodeSuperseded, true, "the freeze of group %d was not kept by %s", sc.group, sc.old())
}

func (r *Runner) freeze(ctx context.Context, env *task.Env, sc *stepCtx) error {
	if err := sc.ensureFrozen(ctx); err != nil {
		return err
	}
	st := "frozen"
	if err := sc.rows.Set(ctx, sc.group, GroupSet{State: &st, Started: true}); err != nil {
		return err
	}
	sc.event(ctx, "frozen", nil)
	sc.r.Plan.Audit(ctx, "failoverGroupStarted", sc.group, map[string]any{"server": sc.old(), "allowDataLoss": sc.in.AllowDataLoss, "comment": sc.in.Comment})
	return nil
}

// ---------------------------------------------------------------------------------
// F2

func (r *Runner) stopOld(ctx context.Context, env *task.Env, sc *stepCtx) error {
	run, err := sc.runRow(ctx)
	if err != nil {
		return err
	}
	if sc.group == 0 && sc.in.MapLive {
		return r.stopOldMap(ctx, env, sc, run)
	}
	if err := sc.ensureFrozen(ctx); err != nil {
		return err
	}
	if _, err := r.Peers.State(ctx, sc.old(), sc.group); err != nil {
		if errors.Is(err, ErrUnreachable) || IsCode(err, "no_instance") {
			sc.event(ctx, "oldUnreachable", map[string]any{"error": err.Error()})
			return task.Skip("the server does not answer; its lease fences it (the next step)")
		}
		if IsOlderServer(err) {
			return fail(CodeSuperseded, false, "server %s runs an older version of kingresd", sc.old())
		}
	}
	// F3-07: a primary that answers is stopped only when a standby that can take over was read just now, whatever the
	// operator accepted: a stop that is followed by "no candidate" leaves the group down and its old primary held back
	// by the marker. A promotion that this run made already (an instance of the group is out of recovery on a
	// standby's server) is not asked again.
	if ok, err := r.someStandbyReadable(ctx, sc.group); err != nil {
		return err
	} else if !ok {
		return fail(CodeNoCandidate, true, "no standby of shard group %d can be read, so the old primary on %s was not stopped; nothing was changed", sc.group, sc.old())
	}
	ans, err := r.Peers.Demote(ctx, sc.old(), DemoteRequest{Group: sc.group, Mode: "immediate", ExpectedTerm: run.ExpectedTerm, TransferTo: r.Plan.Self()})
	switch {
	case err == nil:
		sc.event(ctx, "demoted", map[string]any{"stopped": ans.Stopped, "transferred": ans.Transferred, "clusterState": ans.ClusterState})
		if !ans.Stopped {
			r.Plan.Alert(ctx, "oldPrimaryNotStopped", "critical", subjectOf(sc.group, sc.old()), fmt.Sprintf("The old primary of shard group %d could not be stopped on %s; the lease keeps it from acting.", sc.group, sc.old()), true)
		}
		return nil
	case IsCode(err, "stale_term"):
		return fail(CodeSuperseded, false, "another failover changed the catalog term of the cluster")
	case errors.Is(err, ErrUnreachable), IsCode(err, "not_primary"), IsCode(err, "no_instance"):
		sc.event(ctx, "demoteSkipped", map[string]any{"error": err.Error()})
		return nil
	default:
		// The server answered with an error of its own: the stop may not have happened. Nothing is promoted while an
		// old primary might still be writing: the lease, not the stop, is the proof, and the next step waits for it.
		sc.event(ctx, "demoteError", map[string]any{"error": err.Error()})
		return nil
	}
}

func subjectOf(group int32, server string) string {
	return fmt.Sprintf("server:%s|group:%d", server, group)
}

// ---------------------------------------------------------------------------------
// F3

// freezeRenewEvery is how often a step that waits renews the freeze of the group.
const freezeRenewEvery = 30 * time.Second

func (r *Runner) takeFence(ctx context.Context, env *task.Env, sc *stepCtx) error {
	run, err := sc.runRow(ctx)
	if err != nil {
		return err
	}
	if sc.mapHandedOverByTheCandidate() {
		if nw, _, perr := sc.pinnedStandby(ctx); perr == nil {
			if handed, herr := sc.mapHandedOver(ctx, nw); herr != nil {
				return herr
			} else if handed {
				return task.Skip("the map group was handed over already")
			}
		}
	}
	if err := sc.ensureFrozen(ctx); err != nil {
		return err
	}
	started := r.now()
	members, err := r.Plan.Members(ctx)
	if err != nil {
		return err
	}
	quorumSince := time.Time{}
	// errSince is when the fence lease call began to fail with an error that is not an answer about the lease; the wait for
	// the lease is bounded for such an error as it is for an answer that says "held by the old holder" (phase 5 review M3).
	errSince := time.Time{}
	frozenAt := r.now()
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		// The freeze of the group lapses (default five minutes): it is kept while the step waits.
		if r.now().Sub(frozenAt) >= freezeRenewEvery {
			if err := sc.ensureFrozen(ctx); err != nil {
				return err
			}
			frozenAt = r.now()
		}
		if !r.hearsMajority(ctx, members) {
			if quorumSince.IsZero() {
				quorumSince = r.now()
			}
			if r.now().Sub(quorumSince) > orDur(r.QuorumWait, time.Minute) {
				r.Plan.Alert(ctx, "failoverBlocked", "warning", subjectOf(sc.group, sc.old()), "The server that runs the failover cannot hear a majority of the servers.", true)
				if sc.in.graceful() {
					// A switchover that waits for a majority is before the promotion and has stopped the writes of the group (and, from the clean stop on,
					// the old primary): it does not wait for an operator with the group down. It goes back through the path of every failure, and the
					// repair of switchovers finishes what a server that cannot be reached still holds (F4-02).
					return sc.rollbackFailed(ctx, CodeQuorumUnavailable, true, "this server cannot hear a majority of the servers; shard group %d was given back to %s", sc.group, sc.old())
				}
				return &PauseError{Reason: "This server cannot hear a majority of the servers (quorum_unavailable); nothing was done. Repair the network or repeat the request from a server that can."}
			}
			sc.detail(ctx, "waiting for a majority of the servers to be heard")
		} else {
			quorumSince = time.Time{}
			ans, err := r.Peers.Lease(ctx, r.Plan.Self(), LeaseRequest{Group: sc.group, Role: LeaseFence, ExpectedTerm: run.ExpectedTerm})
			if err != nil {
				if IsCode(err, "stale_term") {
					return fail(CodeSuperseded, false, "another failover changed the catalog term of the cluster")
				}
				if !errors.Is(err, ErrUnreachable) {
					r.log().Warn("failover: asking for the fence lease", "group", sc.group, "error", err)
				}
				if errSince.IsZero() {
					errSince = r.now()
				}
				sc.detail(ctx, "the lease of the group cannot be taken: %v", err)
				if r.now().Sub(errSince) > orDur(r.LeaseWait, 25*time.Second) {
					r.Plan.Alert(ctx, "failoverBlocked", "warning", subjectOf(sc.group, sc.old()), fmt.Sprintf("The lease of shard group %d could not be taken from this server: %v.", sc.group, err), true)
					if sc.in.graceful() {
						return sc.rollbackFailed(ctx, CodeLeaseUnavailable, true, "the lease of shard group %d could not be taken (%v); shard group %d was given back to %s", sc.group, err, sc.group, sc.old())
					}
					return &PauseError{Reason: fmt.Sprintf("The lease of shard group %d could not be taken from this server (%v); nothing was promoted. Repair the cause and repeat the request.", sc.group, err)}
				}
			} else if ans.Held {
				waited := int(r.now().Sub(started).Milliseconds())
				if err := sc.rows.Set(ctx, sc.group, GroupSet{LeaseWaitMs: &waited, State: ptr("fenced")}); err != nil {
					return err
				}
				sc.event(ctx, "leaseHeld", map[string]any{"waitMs": waited})
				return nil
			} else {
				errSince = time.Time{}
				sc.detail(ctx, "waiting for the old lease: %d ms left at the grantors that refused", ans.WaitMs)
				if r.now().Sub(started) > orDur(r.LeaseWait, 25*time.Second) {
					stillOld := false
					for _, h := range ans.RefusedBy {
						if h == sc.old() {
							stillOld = true
						}
					}
					if stillOld {
						r.Plan.Alert(ctx, "failoverBlocked", "warning", subjectOf(sc.group, sc.old()), fmt.Sprintf("A majority of the servers still hear the holder of the lease of shard group %d (%s); its primary is alive for the cluster.", sc.group, sc.old()), true)
						return fail(CodeLeaseStillHeld, true, "the lease of shard group %d is still held by %s and renewed; its primary is alive for the cluster", sc.group, sc.old())
					}
					return fail(CodeLeaseHeldElsewhere, true, "the lease of shard group %d is held by another server (%v)", sc.group, ans.RefusedBy)
				}
			}
		}
		if err := r.sleep(ctx, orDur(r.LeasePoll, time.Second)); err != nil {
			return err
		}
	}
}

func ptr[T any](v T) *T { return &v }

// ---------------------------------------------------------------------------------
// F4

type candidateRead struct {
	rec StandbyRec
	pos Position
	err error
}

// readPositions reads the position of every standby, twice, a second apart, until the
// receive positions stop moving (WAL in flight when the old primary stopped has
// arrived) or the bound ends; the last values are used then.
func (r *Runner) readPositions(ctx context.Context, group int32, recs []StandbyRec) []candidateRead {
	read := func() []candidateRead {
		out := make([]candidateRead, len(recs))
		var wg sync.WaitGroup
		for i, rec := range recs {
			i, rec := i, rec
			wg.Add(1)
			go func() {
				defer wg.Done()
				cctx, cancel := context.WithTimeout(ctx, orDur(r.PositionTimeout, 5*time.Second))
				defer cancel()
				p, err := r.Peers.Position(cctx, rec.Server, group)
				out[i] = candidateRead{rec: rec, pos: p, err: err}
			}()
		}
		wg.Wait()
		return out
	}
	deadline := r.now().Add(orDur(r.ReadBound, 15*time.Second))
	prev := read()
	for {
		if r.sleep(ctx, time.Second) != nil {
			return prev
		}
		cur := read()
		same := true
		for i := range cur {
			a, _ := prev[i].pos.Flushed()
			b, _ := cur[i].pos.Flushed()
			if (prev[i].err == nil) != (cur[i].err == nil) || a != b {
				same = false
			}
		}
		if same || r.now().After(deadline) {
			return cur
		}
		prev = cur
	}
}

func (r *Runner) choose(ctx context.Context, env *task.Env, sc *stepCtx) error {
	if sc.group == 0 && sc.in.MapLive {
		// The standby of the map group of a live leader's server was ranked and pinned by the bootstrap after the stop (the row of the task has the pin since
		// B7): this step runs on the new leader and has nothing left to choose once the standby is promoted.
		if nw, _, perr := sc.pinnedStandby(ctx); perr == nil {
			if handed, herr := sc.mapHandedOver(ctx, nw); herr != nil {
				return herr
			} else if handed {
				return task.Skip("the map group was handed over already")
			}
		}
	}
	if err := sc.ensureFrozen(ctx); err != nil {
		return err
	}
	gr, err := sc.row(ctx)
	if err != nil {
		return err
	}
	recs, err := r.Plan.Standbys(ctx, sc.group)
	if err != nil {
		return err
	}
	var ready []StandbyRec
	for _, s := range recs {
		if s.State == "ready" || s.ID == gr.ChosenStandby {
			ready = append(ready, s)
		}
	}
	if len(ready) == 0 {
		return fail(CodeNoCandidate, true, "shard group %d has no ready standby", sc.group)
	}
	reads := r.readPositions(ctx, sc.group, ready)
	// An instance of the group that is out of recovery on a standby's server is a promotion that an earlier run
	// made (or somebody did): one is pinned, two or more are a split promotion (invariant I5).
	var promoted []StandbyRec
	for _, c := range reads {
		if c.err == nil && !c.pos.InRecovery {
			promoted = append(promoted, c.rec)
		}
	}
	if len(promoted) > 1 {
		r.Plan.Alert(ctx, "splitPromotion", "critical", fmt.Sprintf("group:%d", sc.group), fmt.Sprintf("Two instances of shard group %d are out of recovery.", sc.group), true)
		return fail(CodeSplitPromotion, false, "two instances of shard group %d are out of recovery (%s and %s); nothing else is done", sc.group, promoted[0].Server, promoted[1].Server)
	}
	if len(promoted) == 1 {
		if gr.ChosenStandby != promoted[0].ID {
			// F3-11: nobody pinned this instance: it was promoted by hand (or by a run that is not this one). It may be
			// behind, may have diverged and may have taken writes, and no verdict exists for it. The request goes on with it
			// only when the operator accepted a loss; the pause names it.
			msg := fmt.Sprintf("the standby of shard group %d on %s was promoted outside this failover; it may be behind or may have diverged", sc.group, promoted[0].Server)
			if !sc.in.AllowDataLoss {
				r.Plan.Alert(ctx, "splitPromotion", "critical", fmt.Sprintf("group:%d", sc.group), "Instance promoted outside the failover: "+promoted[0].Server, true)
				return &PauseError{Reason: "The failover of shard group " + fmt.Sprint(sc.group) + " found that " + msg + ". Repeat the request with allowDataLoss set to true to adopt it, or look at it first."}
			}
			dl, _ := json.Marshal(DataLoss{Verdict: VerdictPossible, Message: msg, AsyncDatabases: []string{}, UnreadableServerIDs: []string{}})
			pinned, _, err := sc.rows.Pin(ctx, sc.group, promoted[0], nil, dl)
			if err != nil {
				return err
			}
			return sc.afterPin(ctx, pinned)
		}
		pinned, _, err := sc.rows.Pin(ctx, sc.group, promoted[0], nil, nil)
		if err != nil {
			return err
		}
		return sc.afterPin(ctx, pinned)
	}
	// F3-06: a pin that this task made before anything was promoted is kept while its standby can be read (the verdict
	// that was accepted is the one of that choice) and withdrawn when it cannot, so that the choice can be made again.
	if gr.ChosenStandby != 0 {
		for _, c := range reads {
			if c.rec.ID != gr.ChosenStandby {
				continue
			}
			if c.err == nil {
				return sc.afterPin(ctx, c.rec)
			}
			if err := sc.rows.Set(ctx, sc.group, GroupSet{ClearPin: true, State: ptr("fenced")}); err != nil {
				return err
			}
			sc.event(ctx, "pinWithdrawn", map[string]any{"standby": gr.ChosenStandby, "reason": "the standby cannot be read and no instance of the group is out of recovery"})
			gr.ChosenStandby = 0
		}
		// A pin whose standby has no record any more (the group was promoted and the record went in F9) is kept: Pin below
		// answers the recorded choice.
	}
	cands := make([]Candidate, 0, len(reads))
	for _, c := range reads {
		cd := Candidate{StandbyID: c.rec.ID, Server: c.rec.Server, Readable: c.err == nil, Pos: c.pos,
			Preferred: r.Plan.Preferred(ctx, sc.group, c.rec.Server), RingDist: r.Plan.Ring(sc.old(), c.rec.Server)}
		if c.err != nil {
			cd.Err = c.err.Error()
		}
		cands = append(cands, cd)
	}
	facts, err := r.Plan.Facts(ctx, sc.group)
	if err != nil {
		return err
	}
	ch := Choose(ChooseInput{Candidates: cands, Target: sc.in.Targets[sc.group], Facts: facts})
	decision, dataLoss := marshalDecision(ch, facts, cands)
	if ch.Chosen == nil {
		return fail(CodeNoCandidate, true, "no standby of shard group %d can be promoted: %s", sc.group, ch.Message)
	}
	if !Accepts(ch.Verdict, sc.in.AllowDataLoss) {
		r.Plan.Alert(ctx, "failoverNeedsDecision", "critical", subjectOf(sc.group, sc.old()), fmt.Sprintf("The failover of shard group %d could lose data: %s", sc.group, ch.Message), true)
		return &PauseError{Reason: fmt.Sprintf("The failover of shard group %d could lose data (%s): %s Repeat the request with allowDataLoss set to true if you accept it.", sc.group, ch.Verdict, ch.Message)}
	}
	var chosen StandbyRec
	for _, s := range ready {
		if s.ID == ch.Chosen.StandbyID {
			chosen = s
		}
	}
	pinned, _, err := sc.rows.Pin(ctx, sc.group, chosen, decision, dataLoss)
	if err != nil {
		return err
	}
	r.Plan.Alert(ctx, "failoverNeedsDecision", "critical", subjectOf(sc.group, sc.old()), "", false)
	return sc.afterPin(ctx, pinned)
}

func (sc *stepCtx) afterPin(ctx context.Context, pinned StandbyRec) error {
	node := pinned.CatalogNode
	if err := sc.rows.Set(ctx, sc.group, GroupSet{State: ptr("pinned"), NewServer: &pinned.Server, NewNode: &node, ChosenStandby: &pinned.ID}); err != nil {
		return err
	}
	sc.event(ctx, "pinned", map[string]any{"standby": pinned.ID, "server": pinned.Server, "node": node})
	return nil
}

func marshalDecision(ch Choice, facts GroupFacts, cands []Candidate) ([]byte, []byte) {
	dl := DataLoss{Verdict: ch.Verdict, Message: ch.Message, GroupRanWithoutSynchronousStandby: !facts.SyncStreaming,
		AsyncDatabases: facts.AsyncDatabases, UnreadableServerIDs: ch.Unreadable}
	if dl.AsyncDatabases == nil {
		dl.AsyncDatabases = []string{}
	}
	if dl.UnreadableServerIDs == nil {
		dl.UnreadableServerIDs = []string{}
	}
	if facts.LastKnownLsn != "" {
		dl.LastKnownPrimaryLsn = &facts.LastKnownLsn
		age := int64(facts.LastKnownAge.Seconds())
		dl.LastKnownAgeSeconds = &age
	}
	if ch.ChosenLsn != 0 {
		l := FormatLSN(ch.ChosenLsn)
		dl.ChosenReceiveLsn = &l
	}
	var outs []CandidateOut
	for _, c := range ch.Order {
		co := CandidateOut{ServerID: c.Server, StandbyID: c.StandbyID, Readable: c.Readable}
		_, ineligible := ch.Ineligible[c.StandbyID]
		co.Eligible = !ineligible
		if reason, ok := ch.Ineligible[c.StandbyID]; ok {
			co.IneligibleReason = &reason
		}
		if c.Readable {
			rl, rp := c.Pos.ReceiveLsn, c.Pos.ReplayLsn
			co.ReceiveLsn, co.ReplayLsn = &rl, &rp
			if v, ok := c.Pos.Flushed(); ok && ch.BestLsn >= v {
				lag := int64(ch.BestLsn - v)
				co.LagBytes = &lag
			}
		}
		if c.SyncState != "" {
			s := c.SyncState
			co.SyncState = &s
		}
		co.Chosen = ch.Chosen != nil && ch.Chosen.StandbyID == c.StandbyID
		outs = append(outs, co)
	}
	d, _ := json.Marshal(outs)
	l, _ := json.Marshal(dl)
	return d, l
}

// ---------------------------------------------------------------------------------
// F5

func (sc *stepCtx) pinnedStandby(ctx context.Context) (StandbyRec, GroupRow, error) {
	gr, err := sc.row(ctx)
	if err != nil {
		return StandbyRec{}, gr, err
	}
	if gr.ChosenStandby == 0 {
		return StandbyRec{}, gr, fail(CodeNoCandidate, true, "no standby of shard group %d is pinned", sc.group)
	}
	recs, err := sc.r.Plan.Standbys(ctx, sc.group)
	if err != nil {
		return StandbyRec{}, gr, err
	}
	for _, s := range recs {
		if s.ID == gr.ChosenStandby {
			return s, gr, nil
		}
	}
	// After F9 the record of the promoted standby is gone; the group row has what is needed.
	return StandbyRec{ID: gr.ChosenStandby, Group: sc.group, Server: gr.NewServer, CatalogNode: gr.NewNode}, gr, nil
}

func (r *Runner) handOver(ctx context.Context, env *task.Env, sc *stepCtx) error {
	run, err := sc.runRow(ctx)
	if err != nil {
		return err
	}
	nw, _, err := sc.pinnedStandby(ctx)
	if err != nil {
		return err
	}
	if handed, herr := sc.mapHandedOver(ctx, nw); herr != nil {
		return herr
	} else if handed {
		return task.Skip("the map group was handed over already")
	}
	// A repeated run: the new server holds the lease already, or has been promoted.
	if st, err := r.Peers.State(ctx, nw.Server, sc.group); err == nil && st.Holds && st.Adopted != "" {
		return task.Skip("the new server holds the lease already")
	}
	return sc.holdLease(ctx, nw, run.ExpectedTerm, 0)
}

// holdLease hands the fence lease of the group to the server of the pinned standby
// and waits until that server holds it. again counts the returns to the fence step.
func (sc *stepCtx) holdLease(ctx context.Context, nw StandbyRec, term int64, again int) error {
	r := sc.r
	self := r.Plan.Self()
	if nw.Server != self {
		ans, err := r.Peers.Lease(ctx, self, LeaseRequest{Group: sc.group, Role: LeaseFence, TransferTo: nw.Server, ExpectedTerm: term})
		if err != nil && IsCode(err, "stale_term") {
			return fail(CodeSuperseded, false, "another failover changed the catalog term of the cluster")
		}
		if err != nil || !ans.Transferred {
			if st, serr := r.Peers.State(ctx, nw.Server, sc.group); serr == nil && st.Holds {
				// The transfer was made by an earlier run.
			} else if again < 1 {
				// The leader no longer holds the lease: take it again and try once more.
				if _, lerr := r.Peers.Lease(ctx, self, LeaseRequest{Group: sc.group, Role: LeaseFence, ExpectedTerm: term}); lerr != nil {
					r.log().Warn("failover: taking the fence lease again", "group", sc.group, "error", lerr)
				}
				if err := r.sleep(ctx, orDur(r.LeasePoll, time.Second)); err != nil {
					return err
				}
				return sc.holdLease(ctx, nw, term, again+1)
			} else {
				return fail(CodeLeaseLost, true, "the lease of shard group %d could not be handed to %s", sc.group, nw.Server)
			}
		}
	}
	tries := orInt(r.HandTries, 3)
	for i := 0; i < tries; i++ {
		deadline := r.now().Add(orDur(r.HandWait, 15*time.Second))
		for {
			ans, err := r.Peers.Lease(ctx, nw.Server, LeaseRequest{Group: sc.group, Role: LeaseHolder, ExpectedTerm: term})
			if err != nil && IsCode(err, "stale_term") {
				return fail(CodeSuperseded, false, "another failover changed the catalog term of the cluster")
			}
			if err == nil && ans.Held {
				sc.event(ctx, "leaseHandedOver", map[string]any{"to": nw.Server})
				return nil
			}
			if !r.now().Before(deadline) {
				break
			}
			if err := r.sleep(ctx, orDur(r.LeasePoll, time.Second)); err != nil {
				return err
			}
		}
	}
	return fail(CodeLeaseLost, true, "the server %s did not hold the lease of shard group %d", nw.Server, sc.group)
}

// ---------------------------------------------------------------------------------
// F6

func (r *Runner) promote(ctx context.Context, env *task.Env, sc *stepCtx) error {
	run, err := sc.runRow(ctx)
	if err != nil {
		return err
	}
	nw, _, err := sc.pinnedStandby(ctx)
	if err != nil {
		return err
	}
	if handed, herr := sc.mapHandedOver(ctx, nw); herr != nil {
		return herr
	} else if handed {
		// The bootstrap of the candidate promoted it (the proof and the lease were its own); what this step records is already in the row.
		sc.event(ctx, "promoted", map[string]any{"status": "handedOver"})
		return task.Skip("the map group was promoted by the bootstrap of the new leader")
	}
	req := PromoteRequest{Group: sc.group, StandbyID: nw.ID, InstanceID: nw.InstanceID, ExpectedTerm: run.ExpectedTerm}
	if sc.in.graceful() {
		if st, serr := r.Peers.State(ctx, nw.Server, sc.group); !(serr == nil && len(st.OutOfRecovery()) > 0) {
			// Not promoted yet: the proof is made again in the call (F4-07). The shutdown checkpoint is read again from the stopped old primary.
			ans, derr := r.Peers.Demote(ctx, sc.old(), DemoteRequest{Group: sc.group, Mode: "fast", ExpectedTerm: run.ExpectedTerm})
			if derr != nil || !ans.Stopped || ans.ClusterState != "shut down" || ans.Checkpoint == "" {
				return sc.rollbackFailed(ctx, CodeSwitchoverUnproven, true, "the shutdown checkpoint of the old primary of shard group %d cannot be read before the promotion (%v)", sc.group, derr)
			}
			req.Checkpoint, req.ClusterState = ans.Checkpoint, ans.ClusterState
		}
	}
	handedBack := false
	unavailable := 0
	for try := 0; ; try++ {
		r.fault("promote:call")
		ans, err := r.Peers.Promote(ctx, nw.Server, req)
		switch {
		case err == nil:
			if serr := sc.rows.Set(ctx, sc.group, GroupSet{State: ptr("promoted"), Timeline: &ans.Timeline, Promoted: true}); serr != nil {
				return serr
			}
			sc.event(ctx, "promoted", map[string]any{"status": ans.Status, "timeline": ans.Timeline, "lsn": ans.Lsn, "tookMs": ans.TookMs})
			r.Plan.Audit(ctx, "failoverGroupPromoted", sc.group, map[string]any{"from": sc.old(), "to": nw.Server, "timeline": ans.Timeline,
				"lsn": ans.Lsn, "allowDataLoss": sc.in.AllowDataLoss, "termBefore": run.ExpectedTerm})
			return nil
		case IsCode(err, "unprovable"):
			// The callee refused before it promoted: this is still before the point of no return.
			return sc.rollbackFailed(ctx, CodeSwitchoverUnproven, true, "the switchover of shard group %d cannot be proven at the promotion: %v", sc.group, err)
		case IsCode(err, "standby_behind"):
			return sc.rollbackFailed(ctx, CodeStandbyBehind, true, "the standby on %s does not hold everything the old primary of shard group %d wrote; it was not promoted", nw.Server, sc.group)
		case IsCode(err, "stale_term"):
			return fail(CodeSuperseded, false, "another failover changed the catalog term of the cluster")
		case IsCode(err, "lease_not_held"):
			if handedBack {
				return fail(CodeLeaseLost, true, "the server %s does not hold the lease of shard group %d", nw.Server, sc.group)
			}
			handedBack = true
			if herr := sc.holdLease(ctx, nw, run.ExpectedTerm, 0); herr != nil {
				return herr
			}
		case IsCode(err, "not_standby"):
			// The instance is not the standby the pin names. Unpin unless it was promoted.
			if st, serr := r.Peers.State(ctx, nw.Server, sc.group); serr == nil && len(st.OutOfRecovery()) > 0 {
				continue
			}
			if serr := sc.rows.Set(ctx, sc.group, GroupSet{ClearPin: true}); serr != nil {
				return serr
			}
			return fail(CodeTargetUnavailable, true, "the instance of the pinned standby on %s is not a standby of shard group %d; the choice was withdrawn", nw.Server, sc.group)
		default:
			// A broken connection, a time-out, a server that did not answer: the state of the callee is the truth.
			if st, serr := r.Peers.State(ctx, nw.Server, sc.group); serr == nil {
				if out := st.OutOfRecovery(); len(out) == 1 && st.Holds && st.Attached {
					continue
				}
			}
			// An instance that is starting (after the restart of its kingresd, the lease is held and the manager starts it, with
			// crash recovery) answers 503 for a while: that is waited for much longer than a call that did not answer.
			if IsCode(err, "unavailable") {
				unavailable++
				if unavailable < orInt(r.PromoteUnavailable, 18) {
					if err := r.sleep(ctx, orDur(r.PromotePause, 10*time.Second)); err != nil {
						return err
					}
					continue
				}
			}
			if try+1 >= orInt(r.PromoteTries, 3) {
				return fail(CodePromoteTimeout, true, "the promotion of the standby on %s did not answer after %d tries: %v", nw.Server, try+1, err)
			}
			if err := r.sleep(ctx, orDur(r.PromotePause, 10*time.Second)); err != nil {
				return err
			}
		}
	}
}

// ---------------------------------------------------------------------------------
// F7 and its check

func (r *Runner) promoteOptions(ctx context.Context, sc *stepCtx, gr GroupRow, nw StandbyRec, term int64) PromoteOptions {
	o := r.PromoteNode
	o.NewNode = gr.NewNode
	o.Group = sc.group
	if o.Sleep == nil {
		o.Sleep = r.Sleep
	}
	if o.Now == nil {
		o.Now = r.Now
	}
	o.Detail = func(s string) { sc.detail(ctx, "%s", s) }
	o.Event = func(kind string, d map[string]any) { sc.event(ctx, kind, d) }
	o.ReapplyList = func(ctx context.Context) error {
		_, err := r.Peers.Promote(ctx, nw.Server, PromoteRequest{Group: sc.group, StandbyID: nw.ID, InstanceID: nw.InstanceID, ExpectedTerm: term, ReapplyList: true})
		return err
	}
	o.Probe = func(ctx context.Context) bool {
		members, err := r.Plan.Members(ctx)
		if err != nil {
			return false
		}
		for _, m := range members {
			cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
			_, err := r.Peers.LeaseSettings(cctx, m)
			cancel()
			if err != nil {
				return false
			}
		}
		return true
	}
	return o
}

// dbTarget is the promotion in the catalog of one database: every sharded database has a catalog of its own, with
// its own node ids, so the new and the old node are found there by host and port.
type dbTarget struct {
	db           string
	newID, oldID int32
	nodes        []Node
}

// targets resolves the new and the old node of the group in the catalog of every database (the system database first).
func (r *Runner) targets(ctx context.Context, gr GroupRow, group int32) ([]dbTarget, error) {
	dbs, err := r.Cat.Databases(ctx)
	if err != nil {
		return nil, err
	}
	sys, err := r.Cat.Nodes(ctx, SystemDB)
	if err != nil {
		return nil, err
	}
	var newN, oldN *Node
	for i := range sys {
		switch sys[i].ID {
		case gr.NewNode:
			newN = &sys[i]
		case gr.OldNode:
			oldN = &sys[i]
		}
	}
	if newN == nil || oldN == nil {
		return nil, fail(CodeBadPromoteTarget, false, "the system catalog has no node %d or %d", gr.NewNode, gr.OldNode)
	}
	var out []dbTarget
	for _, db := range dbs {
		nodes, err := r.Cat.Nodes(ctx, db)
		if err != nil {
			return nil, err
		}
		t := dbTarget{db: db, nodes: nodes}
		for _, n := range nodes {
			if n.Group != group {
				continue
			}
			if n.Host == newN.Host && n.Port == newN.Port {
				t.newID = n.ID
			}
			if n.Host == oldN.Host && n.Port == oldN.Port {
				t.oldID = n.ID
			}
		}
		if t.newID == 0 || t.oldID == 0 {
			return nil, fail(CodeBadPromoteTarget, false, "the catalog of database %s does not have the nodes of the group (new %s:%d, old %s:%d)", db, newN.Host, newN.Port, oldN.Host, oldN.Port)
		}
		out = append(out, t)
	}
	return out, nil
}

// registeredEverywhere refuses a failover while a ready standby of the group is not a standby of the group in the
// catalog of some database: promote_node there would not find it.
func (r *Runner) registeredEverywhere(ctx context.Context, g int32, recs []StandbyRec, oldNode *Node) *Refusal {
	dbs, err := r.Cat.Databases(ctx)
	if err != nil {
		return &Refusal{Code: "service_unavailable", Status: 503, Message: "The databases cannot be listed: " + err.Error()}
	}
	for _, db := range dbs {
		if db == SystemDB {
			continue
		}
		nodes, err := r.Cat.Nodes(ctx, db)
		if err != nil {
			return &Refusal{Code: "service_unavailable", Status: 503, Message: "The catalog of database " + db + " cannot be read: " + err.Error()}
		}
		// F3-04: the old primary must be in the catalog of every database too, by host and port: `targets` finds the old
		// node there and a database without it would end the task after the point of no return.
		if oldNode != nil {
			found := false
			for _, n := range nodes {
				if n.Group == g && n.Host == oldNode.Host && n.Port == oldNode.Port {
					found = true
				}
			}
			if !found {
				return &Refusal{Code: "cluster_inconsistent", Status: 409, Group: &g,
					Message: fmt.Sprintf("The primary of shard group %d (%s:%d) is not in the catalog of database %s.", g, oldNode.Host, oldNode.Port, db)}
			}
		}
		for _, s := range recs {
			if s.State != "ready" {
				continue
			}
			found := false
			for _, n := range nodes {
				if n.Host == s.Server && n.Port == s.Port && n.Role == "standby" && n.Group == g {
					found = true
				}
			}
			if !found {
				return &Refusal{Code: "cluster_inconsistent", Status: 409, Group: &g,
					Message: fmt.Sprintf("The standby %d of shard group %d is not registered in the catalog of database %s.", s.ID, g, db)}
			}
		}
	}
	return nil
}

func (r *Runner) catalogFollows(ctx context.Context, env *task.Env, sc *stepCtx) error {
	run, err := sc.runRow(ctx)
	if err != nil {
		return err
	}
	nw, gr, err := sc.pinnedStandby(ctx)
	if err != nil {
		return err
	}
	if gr.NewNode == 0 {
		return fail(CodeBadPromoteTarget, false, "the standby %d has no node in the catalog", nw.ID)
	}
	targets, err := r.targets(ctx, gr, sc.group)
	if err != nil {
		return sc.targetsPause(ctx, err)
	}
	var sysOut PromoteOutcome
	for _, t := range targets {
		o := r.promoteOptions(ctx, sc, gr, nw, run.ExpectedTerm)
		o.NewNode, o.DB = t.newID, t.db
		out, err := r.promoteNodePastPromotion(ctx, sc, nw, run.ExpectedTerm, o)
		if err != nil {
			var pe *PauseError
			if errors.As(err, &pe) {
				r.Plan.Alert(ctx, "failoverBlocked", "critical", subjectOf(sc.group, sc.old()), "database "+t.db+": "+pe.Reason, true)
			}
			return err
		}
		sc.event(ctx, "catalogPromoted", map[string]any{"database": t.db, "term": out.Term, "unreached": out.Unreached})
		if t.db == SystemDB {
			sysOut = out
		}
	}
	// The nodes that were not told are kept as the servers they run on, for the report and for the sync_catalog that
	// follows when they return.
	var unreached []byte
	if len(sysOut.Unreached) > 0 {
		nodes, _ := r.Cat.Nodes(ctx, SystemDB)
		var hosts []string
		for _, id := range sysOut.Unreached {
			for _, n := range nodes {
				if n.ID == id {
					hosts = append(hosts, n.Host)
				}
			}
		}
		unreached, _ = json.Marshal(hosts)
	}
	if err := sc.rows.Set(ctx, sc.group, GroupSet{TermAfter: &sysOut.Term, Unreached: unreached}); err != nil {
		return err
	}
	r.Plan.Alert(ctx, "failoverBlocked", "critical", subjectOf(sc.group, sc.old()), "", false)
	return nil
}

// checkCatalog reads the result back: promote_node can report success although a node
// that went silent at the very end did not get its part. The new node must be the
// primary of the group in the catalog, the old one a standby, and the term must be
// the one the call reported. A mismatch repeats the call once (it is idempotent) and
// asks the operator when it persists. An old primary that was not reached and still
// runs here is stopped now (the fence of last resort).
func (r *Runner) checkCatalog(ctx context.Context, env *task.Env, sc *stepCtx) error {
	run, err := sc.runRow(ctx)
	if err != nil {
		return err
	}
	nw, gr, err := sc.pinnedStandby(ctx)
	if err != nil {
		return err
	}
	targets, err := r.targets(ctx, gr, sc.group)
	if err != nil {
		return sc.targetsPause(ctx, err)
	}
	for _, t := range targets {
		for attempt := 0; ; attempt++ {
			nodes, err := r.Cat.Nodes(ctx, t.db)
			if err != nil {
				return err
			}
			term, _, err := r.Cat.Status(ctx, t.db)
			if err != nil {
				return err
			}
			before := int64(0) // the term before is known for the system catalog only
			if t.db == SystemDB {
				before = run.ExpectedTerm
				if sc.mapHandedOverByTheCandidate() {
					// The term of the map group's own promotion is what the run row says after it (runRowOf): the term before it is the one the run began with.
					if raw, ok, rerr := sc.rows.Run(ctx); rerr == nil && ok {
						before = raw.ExpectedTerm
					}
				}
			}
			ok, why := catalogMatches(nodes, t.newID, t.oldID, term, before)
			if ok {
				sc.event(ctx, "catalogChecked", map[string]any{"database": t.db, "term": term})
				break
			}
			if attempt >= 1 {
				r.Plan.Alert(ctx, "failoverBlocked", "critical", subjectOf(sc.group, sc.old()), "The catalog of database "+t.db+" does not show the promotion: "+why, true)
				return &PauseError{Reason: "The catalog of database " + t.db + " does not show the promotion of shard group " + fmt.Sprint(sc.group) + " (" + why + "); read the answers in the report, then repeat the request."}
			}
			o := r.promoteOptions(ctx, sc, gr, nw, run.ExpectedTerm)
			o.NewNode, o.DB = t.newID, t.db
			if _, err := r.promoteNodePastPromotion(ctx, sc, nw, run.ExpectedTerm, o); err != nil {
				return err
			}
		}
	}
	// The fence of last resort.
	if st, err := sc.oldState(ctx); err == nil {
		if len(st.OutOfRecovery()) > 0 {
			r.Plan.Alert(ctx, "oldPrimaryRunning", "critical", subjectOf(sc.group, sc.old()), fmt.Sprintf("The old primary of shard group %d is running on %s after the catalog moved; it is being stopped.", sc.group, sc.old()), true)
			if _, derr := r.Peers.Demote(ctx, sc.old(), DemoteRequest{Group: sc.group, Mode: "immediate", ExpectedTerm: run.ExpectedTerm}); derr != nil && !errors.Is(derr, ErrUnreachable) {
				return fail(CodeSplitPromotion, false, "the old primary of shard group %d runs on %s and could not be stopped: %v", sc.group, sc.old(), derr)
			}
			sc.event(ctx, "oldPrimaryStopped", nil)
		}
	}
	return nil
}

// catalogMatches says whether the catalog rows show the promotion: the new node is
// the primary of the group, the old one is not, and the term is above the term the
// run started with.
func catalogMatches(nodes []Node, newID, oldID int32, term, termBefore int64) (bool, string) {
	var newRole, oldRole string
	for _, n := range nodes {
		if n.ID == newID {
			newRole = n.Role
		}
		if n.ID == oldID {
			oldRole = n.Role
		}
	}
	switch {
	case newRole != "primary":
		return false, fmt.Sprintf("node %d is %q", newID, newRole)
	case oldRole == "primary":
		return false, fmt.Sprintf("the old node %d is still a primary", oldID)
	case term <= termBefore:
		return false, fmt.Sprintf("the term is %d, it was %d", term, termBefore)
	}
	return true, ""
}

// ---------------------------------------------------------------------------------
// F8 to F13

func (r *Runner) repoint(ctx context.Context, env *task.Env, sc *stepCtx) error {
	run, err := sc.runRow(ctx)
	if err != nil {
		return err
	}
	nw, _, err := sc.pinnedStandby(ctx)
	if err != nil {
		return err
	}
	nodes, err := r.Cat.Nodes(ctx, SystemDB)
	if err != nil {
		return err
	}
	port := 0
	for _, n := range nodes {
		if n.ID == nw.CatalogNode {
			port = n.Port
		}
	}
	if port == 0 {
		return fail(CodeBadPromoteTarget, false, "the catalog has no port for node %d", nw.CatalogNode)
	}
	recs, err := r.Plan.Standbys(ctx, sc.group)
	if err != nil {
		return err
	}
	done, skipped := 0, 0
	for _, s := range recs {
		if s.ID == nw.ID || s.State != "ready" {
			continue
		}
		ans, err := r.Peers.Repoint(ctx, s.Server, RepointRequest{Group: sc.group, StandbyID: s.ID, InstanceID: s.InstanceID, PrimaryHost: nw.Server, PrimaryPort: port, ExpectedTerm: run.ExpectedTerm})
		switch {
		case err == nil && ans.Streaming:
			done++
		case err == nil:
			// Not streaming within the bound: ahead of the new primary or on another timeline.
			skipped++
			if ferr := r.Plan.FlagRebuild(ctx, s.ID); ferr != nil {
				r.log().Warn("failover: flagging a standby for a rebuild", "standby", s.ID, "error", ferr)
			}
			sc.event(ctx, "repointNotStreaming", map[string]any{"standby": s.ID, "server": s.Server, "state": ans.State})
		case IsCode(err, "stale_term"):
			return fail(CodeSuperseded, false, "another failover changed the catalog term of the cluster")
		default:
			skipped++
			sc.event(ctx, "repointSkipped", map[string]any{"standby": s.ID, "server": s.Server, "error": err.Error()})
		}
	}
	sc.detail(ctx, "%d standbys follow the new primary, %d skipped", done, skipped)
	return nil
}

func (r *Runner) records(ctx context.Context, env *task.Env, sc *stepCtx) error {
	nw, gr, err := sc.pinnedStandby(ctx)
	if err != nil {
		return err
	}
	nodes, err := r.Cat.Nodes(ctx, SystemDB)
	if err != nil {
		return err
	}
	port := 0
	for _, n := range nodes {
		if n.ID == gr.NewNode {
			port = n.Port
		}
	}
	old := OldPrimaryRec{Server: sc.old(), Node: gr.OldNode}
	for _, n := range nodes {
		if n.ID == gr.OldNode {
			old.Port = n.Port
		}
	}
	// The directory of the old primary, when its server answers: it is the instance whose role file says primary of the
	// group (stopped by the demote). A server that does not answer finds its own directory when it is rebuilt.
	if st, err := sc.oldState(ctx); err == nil {
		for _, i := range st.Instances {
			if i.Group == sc.group && i.Role == "primary" && !i.Running {
				old.InstanceID = i.ID
			}
		}
	}
	return r.Plan.AfterPromotion(ctx, sc.group, nw, nw.Server, port, old)
}

func (r *Runner) syncNames(ctx context.Context, env *task.Env, sc *stepCtx) error {
	_, gr, err := sc.pinnedStandby(ctx)
	if err != nil {
		return err
	}
	deadline := r.now().Add(orDur(r.SyncWait, 2*time.Minute))
	for {
		names, planned, ok := r.Plan.SyncNamesOf(ctx, gr.NewServer, sc.group)
		if ok && (names != "" || planned == 0) {
			sc.event(ctx, "syncNames", map[string]any{"names": names, "planned": planned})
			return nil
		}
		if !r.now().Before(deadline) {
			sc.event(ctx, "syncNamesPending", map[string]any{"planned": planned})
			return nil // not a failure: the group runs without a synchronous standby until a standby streams
		}
		if err := r.sleep(ctx, 2*time.Second); err != nil {
			return err
		}
	}
}

func (r *Runner) prepared(ctx context.Context, env *task.Env, sc *stepCtx) error {
	_, gr, err := sc.pinnedStandby(ctx)
	if err != nil {
		return err
	}
	dbs := r.catalogDatabases(ctx, sc)
	deadline := r.now().Add(orDur(r.PreparedWait, 2*time.Minute))
	for {
		// F3-10: every database has a catalog of its own, and the recovery acts on the catalog of the database it runs in.
		for _, db := range dbs {
			if err := r.Cat.RecoverPrepared(ctx, db); err != nil {
				sc.event(ctx, "recoverPreparedError", map[string]any{"database": db, "error": err.Error()})
			} else {
				sc.event(ctx, "preparedRecovered", map[string]any{"database": db})
			}
			if err := r.Peers.RecoverPrepared(ctx, gr.NewServer, sc.group, db); err != nil {
				sc.event(ctx, "recoverPreparedOnNewError", map[string]any{"database": db, "error": err.Error()})
			}
		}
		st, err := r.Peers.State(ctx, gr.NewServer, sc.group)
		if err == nil && (st.Prepared == nil || *st.Prepared == 0) {
			break
		}
		if !r.now().Before(deadline) {
			sc.event(ctx, "preparedRemain", nil)
			break
		}
		if err := r.sleep(ctx, orDur(r.PreparedEvery, 30*time.Second)); err != nil {
			return err
		}
	}
	var incidents []Incident
	for _, db := range dbs {
		ins, err := r.Cat.CheckCluster(ctx, db)
		if err != nil {
			sc.event(ctx, "checkClusterError", map[string]any{"database": db, "error": err.Error()})
			continue
		}
		for _, in := range ins {
			if db != SystemDB {
				in.Message = "database " + db + ": " + in.Message
			}
			incidents = append(incidents, in)
		}
	}
	var rep []IncidentOut
	loss := false
	for _, in := range incidents {
		switch in.Kind {
		case "prepared_rolled_back_after_failover", "participant_lost_committed_transaction":
			loss = true
			fallthrough
		case "foreign_prepared_transaction", "decision_unconfirmed", "standby_unreachable", "standby_not_streaming":
			rep = append(rep, IncidentOut{Kind: in.Kind, Message: in.Message})
		case "standby_not_in_recovery":
			rep = append(rep, IncidentOut{Kind: in.Kind, Message: in.Message})
			r.Plan.Alert(ctx, "oldPrimaryRunning", "critical", subjectOf(sc.group, sc.old()), "check_cluster() reports a standby that is not in recovery: "+in.Message, true)
		}
	}
	if len(rep) > 0 {
		b, _ := json.Marshal(rep)
		if err := sc.rows.Set(ctx, sc.group, GroupSet{Incidents: b}); err != nil {
			return err
		}
	}
	if loss {
		r.Plan.Alert(ctx, "failoverDataLoss", "warning", fmt.Sprintf("group:%d", sc.group), fmt.Sprintf("check_cluster() reports lost or rolled back transactions after the failover of shard group %d.", sc.group), true)
	}
	return nil
}

func (r *Runner) clients(ctx context.Context, env *task.Env, sc *stepCtx) error {
	_, gr, err := sc.pinnedStandby(ctx)
	if err != nil {
		return err
	}
	deadline := r.now().Add(orDur(r.ClientsWait, 30*time.Second))
	for {
		st, err := r.Peers.State(ctx, gr.NewServer, sc.group)
		if err == nil && st.Holds && len(st.OutOfRecovery()) == 1 {
			r.writesResumed(ctx, sc)
			return nil
		}
		if !r.now().Before(deadline) {
			sc.detail(ctx, "warning: the new primary did not report that clients arrive within the bound")
			return nil
		}
		if err := r.sleep(ctx, time.Second); err != nil {
			return err
		}
	}
}

func (r *Runner) unfreeze(ctx context.Context, env *task.Env, sc *stepCtx) error {
	if sc.in.graceful() {
		// The old primary is a standby to be: its access rules are put back before the rebuild starts it again.
		term := int64(-1)
		if run, rerr := sc.runRow(ctx); rerr == nil {
			term = run.ExpectedTerm
		}
		if err := sc.oldUnquiesce(ctx, term); err != nil && !errors.Is(err, ErrUnreachable) && !IsCode(err, "no_instance") {
			r.log().Warn("failover: putting the access rules back", "group", sc.group, "error", err)
		}
	}
	if err := sc.oldUnfreeze(ctx); err != nil && !errors.Is(err, ErrUnreachable) && !IsCode(err, "no_instance") {
		r.log().Warn("failover: lifting the freeze", "group", sc.group, "server", sc.old(), "error", err)
	}
	old := "waitingForServer"
	if st, err := sc.oldState(ctx); err == nil && len(st.OutOfRecovery()) == 0 && !st.RunningUnknown() { // an instance that cannot be looked at may run: the rebuild waits
		old = "rebuildScheduled" // the reconcile loop rebuilds it (oldprimary.go in internal/standby)
	}
	if err := sc.rows.Set(ctx, sc.group, GroupSet{State: ptr("done"), OldPrimary: &old, Finished: true}); err != nil {
		return err
	}
	sc.event(ctx, "done", nil)
	return nil
}

func (r *Runner) report(ctx context.Context, env *task.Env, sc *stepCtx) error {
	outcome := "completed"
	for _, g := range sc.in.Groups {
		gr, ok, err := sc.rows.Group(ctx, g)
		if err != nil {
			return err
		}
		if !ok || gr.State != "done" {
			outcome = "partial"
		}
	}
	// F3-15: steps that never fail say what they could not do; the outcome carries it to the operator.
	if outcome == "completed" {
		if warned, err := sc.rows.Warned(ctx); err != nil {
			return err
		} else if warned {
			outcome = "completedWithWarnings"
		}
	}
	if err := sc.rows.Finish(ctx, outcome, ""); err != nil {
		return err
	}
	if sc.in.Trigger == "automatic" {
		r.Plan.Alert(ctx, "autoFailoverTriggered", "info", "cluster", "An automatic failover of server "+sc.old()+" ended: "+outcome+".", true)
	}
	return nil
}

// OnFail is the hook of a task that the engine recorded as failed or cancelled: the
// freezes are lifted (best effort) and the run's record is closed. It records nothing
// for a run that was only interrupted (the engine does not call it then).
func (r *Runner) OnFail(abandon func(ctx context.Context, taskID int64, outcome, text string)) func(ctx context.Context, t task.Task) {
	return func(ctx context.Context, t task.Task) {
		var in Input
		if err := json.Unmarshal(t.Input, &in); err != nil {
			return
		}
		if in.graceful() {
			r.rollbackUnpromoted(ctx, t, in)
		}
		for _, g := range in.Groups {
			// The task is over: its freeze is lifted whatever epoch holds it (F3-14: only a freeze of this task is lifted).
			_ = r.Peers.Unfreeze(fence.With(ctx, fence.Token{Task: t.ID, Epoch: math.MaxInt64}), in.oldOf(g), g)
			// The fence adoption is not renewed any more: it runs out at the grantors.
			_, _ = r.Peers.Lease(ctx, r.Plan.Self(), LeaseRequest{Group: g, Role: LeaseRelease})
		}
		outcome, text := "failed", "The failover task failed."
		if t.Error != nil {
			text = t.Error.Message
			if t.Error.Code == task.CodeCanceled {
				outcome = "cancelled"
			}
		}
		if abandon != nil {
			abandon(ctx, t.ID, outcome, text)
		}
		if outcome == "failed" {
			for _, g := range in.Groups {
				r.Plan.Audit(ctx, "failoverGroupFailed", g, map[string]any{"error": text})
			}
		}
	}
}

// catalogDatabases lists the databases that have a catalog (the system database first); a list that cannot be read
// is the system database alone, with an event, so that the step still looks where it always looked.
func (r *Runner) catalogDatabases(ctx context.Context, sc *stepCtx) []string {
	dbs, err := r.Cat.Databases(ctx)
	if err != nil || len(dbs) == 0 {
		if err != nil {
			sc.event(ctx, "databasesUnreadable", map[string]any{"error": err.Error()})
		}
		return []string{SystemDB}
	}
	return dbs
}

// targetsPause is what a step does when the nodes of the group cannot be resolved in the catalog of every database after
// the promotion (F3-04): nothing is failed there. The point of no return has passed and the promoted instance holds the
// lease; the task waits for the operator (a database that is being created or dropped, a catalog that cannot be read) and
// the repeated request runs the step again. An error of the context is the engine's.
func (sc *stepCtx) targetsPause(ctx context.Context, err error) error {
	if ctx.Err() != nil || errors.Is(err, ErrLeaseLost) {
		return err
	}
	sc.r.Plan.Alert(ctx, "failoverBlocked", "critical", subjectOf(sc.group, sc.old()), "The catalogs cannot be followed: "+err.Error(), true)
	return &PauseError{Reason: "The catalogs of shard group " + fmt.Sprint(sc.group) + " cannot be moved to the promoted standby yet (" + err.Error() +
		"). The group was promoted and holds the lease; repair what the message names, then repeat the request."}
}

// someStandbyReadable reads the position of every ready standby of the group once and says whether at least one answers
// and is in recovery (F3-07).
func (r *Runner) someStandbyReadable(ctx context.Context, group int32) (bool, error) {
	recs, err := r.Plan.Standbys(ctx, group)
	if err != nil {
		return false, err
	}
	for _, rec := range recs {
		if rec.State != "ready" {
			continue
		}
		cctx, cancel := context.WithTimeout(ctx, orDur(r.PositionTimeout, 5*time.Second))
		p, perr := r.Peers.Position(cctx, rec.Server, group)
		cancel()
		if perr == nil && p.InRecovery {
			return true, nil
		}
	}
	return false, nil
}

// promoteNodePastPromotion runs kingres.promote_node after the point of no return, where nothing is failed (F3-04). A new primary
// that does not accept connections for the whole wait is an instance that is down: its server's kingresd ended while the step
// ran and was away longer than the lease, so the watchdog stopped it, and no lease round of that server asks for the group
// before the catalog names it. The lease is handed to that server again (which starts the instance) and the promotion is asked
// once more (it answers "already"); then the call is tried a second time. When that does not end either, the task pauses with
// the reason: the repeated request does the same again, so a retry can succeed, and a failed task is not what the operator
// meets after the promotion.
func (r *Runner) promoteNodePastPromotion(ctx context.Context, sc *stepCtx, nw StandbyRec, term int64, o PromoteOptions) (PromoteOutcome, error) {
	out, err := RunPromoteNode(ctx, r.Cat, o)
	var fe *Error
	if err == nil || !errors.As(err, &fe) || fe.Code != CodePromoteIncomplete {
		return out, err
	}
	sc.event(ctx, "newPrimaryNotReady", map[string]any{"error": fe.Message})
	if herr := sc.holdLease(ctx, nw, term, 0); herr != nil {
		var he *Error
		if errors.As(herr, &he) && he.Code == CodeSuperseded {
			return out, herr
		}
		return out, sc.targetsPause(ctx, herr)
	}
	if _, perr := r.Peers.Promote(ctx, nw.Server, PromoteRequest{Group: sc.group, StandbyID: nw.ID, InstanceID: nw.InstanceID, ExpectedTerm: term}); perr != nil && IsCode(perr, "stale_term") {
		return out, fail(CodeSuperseded, false, "another failover changed the catalog term of the cluster")
	}
	out, err = RunPromoteNode(ctx, r.Cat, o)
	if err != nil && errors.As(err, &fe) && fe.Code == CodePromoteIncomplete {
		return out, sc.targetsPause(ctx, err)
	}
	return out, err
}

// The calls to the old primary's server after the promotion are best effort: its server may be down, or up with its instance held back, in which case it
// answers "not ready" and a task step would wait for it for minutes (peerclient.NotReadyBound), which stops the failover for a server that is not
// needed any more (the case of the old leader of a planned switchover of the map group that restarted). They wait for OldServerWait and go on. Only the map group of a planned switchover is bounded: there the old leader's instance is the stopped system database.
func (sc *stepCtx) oldCtx(ctx context.Context) (context.Context, context.CancelFunc) {
	if sc.group != 0 || !sc.mapHandedOverByTheCandidate() {
		// Every other group keeps the long wait of a task step: the check that stops an old primary that still runs (the fence of last resort) is reached when the old
		// server answers late or "not ready", and the lease fences in any case, but the alert and the immediate stop are worth the wait there (F5-10).
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, orDur(sc.r.OldServerWait, 15*time.Second))
}

// oldErr reports a wait that ran out as a server that did not answer, unless the step's own context ended.
func (sc *stepCtx) oldErr(ctx, octx context.Context, err error) error {
	if err != nil && octx.Err() != nil && ctx.Err() == nil {
		return fmt.Errorf("%w: %v", ErrUnreachable, err)
	}
	return err
}

func (sc *stepCtx) oldState(ctx context.Context) (State, error) {
	oc, cancel := sc.oldCtx(ctx)
	defer cancel()
	st, err := sc.r.Peers.State(oc, sc.old(), sc.group)
	return st, sc.oldErr(ctx, oc, err)
}

func (sc *stepCtx) oldUnquiesce(ctx context.Context, term int64) error {
	oc, cancel := sc.oldCtx(ctx)
	defer cancel()
	return sc.oldErr(ctx, oc, sc.r.Peers.Unquiesce(oc, sc.old(), QuiesceRequest{Group: sc.group, ExpectedTerm: term}))
}

func (sc *stepCtx) oldUnfreeze(ctx context.Context) error {
	oc, cancel := sc.oldCtx(ctx)
	defer cancel()
	return sc.oldErr(ctx, oc, sc.r.Peers.Unfreeze(oc, sc.old(), sc.group))
}
