// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/cluster"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/failover"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/failover/journal"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/failover/mapgroup"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/internalapi"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/pki"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/problem"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/rebalance"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/sysdb"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/task"
)

// The calls of the bootstrap of the map group (design ha-phase5-part2.md, 3.4). They are made between servers while the manager
// leader is gone, so they cannot be LeaderOnly: any member may make them (the policy AnyPeer), and the callee proves what it needs
// itself (the catalog it holds names the old server as the primary of the map group, a ready standby of group 0 runs here, a
// majority of the servers is heard, and the lease that makes the promotion safe is taken from a majority).
const (
	callMapStart    = "POST /failover/map/start"
	callMapContinue = "POST /failover/map/continue"
)

// mapFailover is the server's part of the failover of the map group: the candidate that runs the bootstrap, the receiver that
// finds a candidate for a request, and the answers about the bootstrap while no task can be read.
type mapFailover struct {
	svc     *cluster.Service
	self    string
	log     *slog.Logger
	fd      *failoverDeps
	peers   *foPeers
	cat     *foCatalog
	pl      *foPlanner
	rr      *failover.Runner
	journal *journal.Store
	runner  *mapgroup.Runner

	mu      sync.Mutex
	running bool
	lastErr string
}

func newMapFailover(dataDir string, svc *cluster.Service, self string, log *slog.Logger, fd *failoverDeps, peers *foPeers, cat *foCatalog, pl *foPlanner, rr *failover.Runner,
	faultErr func(point string) error) (*mapFailover, error) {
	js, err := journal.Open(filepath.Join(dataDir, "failover"))
	if err != nil {
		return nil, err
	}
	m := &mapFailover{svc: svc, self: self, log: log, fd: fd, peers: peers, cat: cat, pl: pl, rr: rr, journal: js}
	m.runner = &mapgroup.Runner{P: &mapPorts{m: m}, Journal: js, Fault: faultErr,
		Log: func(f string, a ...any) { log.Info(fmt.Sprintf(f, a...)) }}
	return m, nil
}

// sysConn is the connection to the system database of this server's own instance of the map group: the primary once it is
// promoted, the standby before.
func (m *mapFailover) sysConn(ctx context.Context) (*pgx.Conn, error) {
	if li, ok := m.svc.PrimaryOf(0); ok && li.Role.Group == 0 {
		return m.svc.InstanceConn(ctx, li.ID, sysdb.Database)
	}
	for _, li := range m.svc.LocalInstances() {
		if li.Role.Group == 0 && li.Role.IsStandby() {
			if c, err := m.svc.InstanceConn(ctx, li.ID, sysdb.Database); err == nil {
				return c, nil
			}
		}
	}
	// A server that hosts no instance of the map group (the receiver of an operator's request) reads the copy of the catalog that
	// every primary holds: enough for the term and the primary of group 0.
	if c, err := m.svc.SystemConn(ctx); err == nil {
		return c, nil
	}
	return nil, errors.New("this server runs no instance of the map group that can be read")
}

// Handles says whether a failover request for server is the failover of the map group that cannot go through the leader: the
// catalog this server holds names the server as the primary of the map group, and this server does not hear it.
func (m *mapFailover) Handles(ctx context.Context, server string) bool {
	if server == m.self {
		return false
	}
	_, primary, err := (&mapPorts{m: m}).CatalogTerm(ctx)
	if err != nil || primary != server {
		return false
	}
	return m.pl.Live(ctx, server) != "live"
}

func (m *mapFailover) active() bool { return m.journal.Active() }

// Active says whether a bootstrap is in progress on this server.
func (m *mapFailover) Active() bool { return m.active() }

// Start answers a request for the failover of the map group (HTTP, on any server): the candidate runs the bootstrap, and the
// answer is a task with id 0 while no task can be recorded (the journal's steps), or the recorded task.
func (m *mapFailover) Start(ctx context.Context, actor, server string, req failover.Request) (task.Task, error) {
	if req.Mode != failover.ModeHard {
		return task.Task{}, problem.New(problem.UnsupportedOp, "The map group is failed over in mode hard only.")
	}
	body := mapStartBody{Server: server, RequestedBy: actor, AllowDataLoss: req.AllowDataLoss, Comment: req.Comment}
	cand, err := m.candidate(ctx)
	if err != nil {
		return task.Task{}, err
	}
	if cand == m.self {
		if err := m.begin(ctx, body); err != nil {
			return task.Task{}, err
		}
	} else {
		var out struct{}
		if err := m.peers.do(ctx, cand, http.MethodPost, "/failover/map/start", body, &out); err != nil {
			return task.Task{}, mapCallProblem(err)
		}
	}
	return m.pseudoTask(actor, server), nil
}

// candidate chooses the server that runs the bootstrap: this one when it hosts a ready standby of the map group, else the first
// live server that does.
func (m *mapFailover) candidate(ctx context.Context) (string, error) {
	recs, err := (&mapPorts{m: m}).Standbys(ctx)
	if err != nil {
		return "", problem.New(problem.ServiceUnavailable, "The standbys of the map group cannot be read now.").WithRetryAfter(5)
	}
	var live []string
	for _, s := range recs {
		if s.State != "ready" || s.CatalogNode == 0 {
			continue
		}
		if s.Server == m.self {
			return m.self, nil
		}
		if m.pl.Live(ctx, s.Server) == "live" {
			live = append(live, s.Server)
		}
	}
	sort.Strings(live)
	if len(live) == 0 {
		return "", problem.New(problem.NoReplicaAvailable, "No server that hosts a ready standby of the map group can be reached.")
	}
	return live[0], nil
}

type mapStartBody struct {
	Server        string `json:"server"`
	Trigger       string `json:"trigger,omitempty"`
	RequestedBy   string `json:"requestedBy"`
	AllowDataLoss bool   `json:"allowDataLoss"`
	Comment       string `json:"comment"`
}

// begin runs B1 here (the refusal answers the request) and starts the rest in the background.
func (m *mapFailover) begin(ctx context.Context, b mapStartBody) error {
	if _, err := m.runner.Decide(ctx, mapgroup.Request{OldServer: b.Server, Trigger: nonEmptyStr(b.Trigger, "manual"), RequestedBy: b.RequestedBy, AllowDataLoss: b.AllowDataLoss, Comment: b.Comment}); err != nil {
		var ref *mapgroup.Refusal
		if errors.As(err, &ref) {
			return problem.New(problem.Code(ref.Code), ref.Message)
		}
		return err
	}
	m.resumeInBackground()
	return nil
}

func mapCallProblem(err error) error {
	var ce *failover.CallError
	if errors.As(err, &ce) && ce.Code != "" {
		return problem.New(problem.Code(ce.Code), ce.Message)
	}
	return problem.New(problem.ServiceUnavailable, "The server that promotes the map group cannot be reached.").WithRetryAfter(5)
}

// resumeInBackground runs the steps that are not done, once at a time. A pause is the end of the run (the operator repeats the
// request); another error is retried after a pause of its own until it ends, because the journal makes every step repeatable.
func (m *mapFailover) resumeInBackground() {
	m.mu.Lock()
	if m.running {
		m.mu.Unlock()
		return
	}
	m.running = true
	m.mu.Unlock()
	d := m.fd.d
	bg := context.Background()
	if d != nil && d.BG != nil {
		bg = d.BG
	}
	run := func() {
		defer func() { m.mu.Lock(); m.running = false; m.mu.Unlock() }()
		for tries := 0; tries < 20 && bg.Err() == nil; tries++ {
			_, ok, err := m.runner.Resume(bg)
			if !ok {
				return
			}
			if err == nil {
				m.setErr("")
				return
			}
			m.setErr(err.Error())
			var pe *task.PauseError
			if errors.As(err, &pe) {
				m.log.Warn("the failover of the map group waits for an operator", "reason", pe.Reason)
				return
			}
			var fe *failover.Error
			if errors.As(err, &fe) && !fe.Retryable {
				m.log.Error("the failover of the map group failed", "error", err)
				return
			}
			m.log.Warn("a step of the failover of the map group did not finish; it is repeated", "error", err)
			select {
			case <-bg.Done():
				return
			case <-time.After(5 * time.Second):
			}
		}
	}
	if d != nil && d.WG != nil {
		d.WG.Add(1)
		go func() { defer d.WG.Done(); run() }()
		return
	}
	go run()
}

func (m *mapFailover) setErr(s string) { m.mu.Lock(); m.lastErr = s; m.mu.Unlock() }

// ResumeOnStart goes on with a journal that a restart of kingresd found in progress (design: "a restarted kingresd reads it first").
func (m *mapFailover) ResumeOnStart() {
	if m.journal.Active() {
		// The adoption of the lease lives in the memory of the process that ended: B3 and B5 are done again (they are idempotent) before
		// B6 needs the promoted instance, which the lease manager starts once the adopted lease is held again.
		if !m.journal.IsDone(journal.StepCatalog) && m.journal.IsDone(journal.StepPromote) {
			_ = m.journal.Undo(journal.StepLease, journal.StepPromote)
		}
		m.log.Warn("a bootstrap of the map group was in progress when kingresd ended; it goes on")
		m.resumeInBackground()
	}
}

// pseudoTask is the task that is answered while none is recorded: id 0, the steps of the journal.
func (m *mapFailover) pseudoTask(actor, server string) task.Task {
	t := task.Task{ID: 0, Kind: failover.KindFailover, State: task.Running, SubjectType: failover.SubjectType, SubjectID: server, CreatedBy: actor,
		CreatedAt: time.Now(), UpdatedAt: time.Now()}
	j, ok := m.journal.Current()
	if ok {
		t.CreatedAt, t.CreatedBy, t.SubjectID = j.StartedAt, nonEmptyStr(j.RequestedBy, actor), j.OldServer
	}
	next := true
	for _, s := range journal.Order {
		st := task.StepPending
		switch {
		case ok && !j.Done[s].IsZero():
			st = task.StepDone
		case next:
			st, next = task.StepRunning, false
		}
		t.Steps = append(t.Steps, task.StepState{Name: "Group 0: " + s, State: st})
	}
	if !next {
		t.Percent = 100 * float64(len(j.Done)) / float64(len(journal.Order))
	}
	m.mu.Lock()
	if m.lastErr != "" {
		t.Detail = m.lastErr
	}
	m.mu.Unlock()
	return t
}

func nonEmptyStr(s, d string) string {
	if s != "" {
		return s
	}
	return d
}

// TaskZero answers GET /tasks/0: the journal's steps while the bootstrap runs.
func (m *mapFailover) TaskZero() (task.Task, bool) {
	if !m.journal.Active() {
		return task.Task{}, false
	}
	j, _ := m.journal.Current()
	return m.pseudoTask(j.RequestedBy, j.OldServer), true
}

// Status answers GET /failover while the bootstrap runs: the task id is null until the task is recorded.
func (m *mapFailover) Status() (failover.Status, bool) {
	if !m.journal.Active() {
		return failover.Status{}, false
	}
	j, _ := m.journal.Current()
	phase := "running"
	st := failover.Status{State: "running", Phase: &phase, ServerID: &j.OldServer, StartedAt: &j.StartedAt, NeedsDecision: []failover.Blocker{},
		Groups: []failover.GroupProgress{{ShardGroupID: 0, State: "running", FromServerID: j.OldServer}}}
	kind, trig := "failover", j.Trigger
	st.Kind, st.Trigger = &kind, &trig
	if j.Pin != nil {
		st.Groups[0].ToServerID = &j.Pin.Server
	}
	if j.Recorded && j.TaskID > 0 {
		id := j.TaskID
		st.TaskID = &id
	}
	m.mu.Lock()
	if m.lastErr != "" {
		st.Groups[0].Message = &m.lastErr
	}
	m.mu.Unlock()
	return st, true
}

// ---------------------------------------------------------------------------------
// The callee side

func handleMapStart(ctx context.Context, d *haDeps, body []byte) (any, error) {
	mf := d.MapFailover
	if mf == nil {
		return nil, notStarted
	}
	var b mapStartBody
	if err := json.Unmarshal(body, &b); err != nil || b.Server == "" {
		return nil, badBody("a map group failover request")
	}
	if err := mf.begin(ctx, b); err != nil {
		var p *problem.Problem
		if errors.As(err, &p) {
			return nil, &internalapi.CallError{Status: p.Status, Code: string(p.Code), Message: p.Detail}
		}
		return nil, err
	}
	return struct{}{}, nil
}

// handleMapContinue is the call of a candidate that handed over the lease at B4: this server holds the journal's pin, adopts the
// journal (its steps up to B4 are done there) and resumes at B4's successor.
func handleMapContinue(ctx context.Context, d *haDeps, body []byte) (any, error) {
	mf := d.MapFailover
	if mf == nil {
		return nil, notStarted
	}
	var j journal.Journal
	if err := json.Unmarshal(body, &j); err != nil || j.ID == "" || j.Pin == nil {
		return nil, badBody("a journal")
	}
	if j.Pin.Server != mf.self {
		return nil, &internalapi.CallError{Status: http.StatusConflict, Code: "not_candidate", Message: "The journal pins another server's standby."}
	}
	term, primary, err := (&mapPorts{m: mf}).CatalogTerm(ctx)
	if err != nil || primary != j.OldServer || term != j.Term {
		return nil, &internalapi.CallError{Status: http.StatusConflict, Code: "stale_term", Message: "The catalog of this server does not match the journal."}
	}
	if _, err := mf.journal.Adopt(j); err != nil {
		return nil, &internalapi.CallError{Status: http.StatusConflict, Code: "conflict", Message: err.Error()}
	}
	mf.resumeInBackground()
	return struct{}{}, nil
}

// ---------------------------------------------------------------------------------
// The ports of the bootstrap

type mapPorts struct{ m *mapFailover }

func (p *mapPorts) Self() string { return p.m.self }

func (p *mapPorts) CatalogTerm(ctx context.Context) (int64, string, error) {
	c, err := p.m.sysConn(ctx)
	if err != nil {
		return 0, "", err
	}
	defer func() { _ = c.Close(context.WithoutCancel(ctx)) }()
	var term int64
	if err := c.QueryRow(ctx, `SELECT term FROM kingres.catalog_status`).Scan(&term); err != nil {
		return 0, "", err
	}
	var host string
	if err := c.QueryRow(ctx, `SELECT host FROM kingres.node WHERE group_id = 0 AND role = 'primary'`).Scan(&host); err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return 0, "", err
	}
	return term, host, nil
}

func (p *mapPorts) Members(ctx context.Context) ([]string, error) {
	c, err := p.m.sysConn(ctx)
	if err != nil {
		// After a restart the instance of the map group is the promoted one and it is not started until the lease is held again,
		// which is what B3 obtains: the members are then the servers of the peer directory (the catalog of the cluster adds none that
		// the directory lacks).
		if d := p.m.fd.d; d != nil && d.Peers != nil {
			set := map[string]bool{p.m.self: true}
			for _, pe := range d.Peers.All() {
				set[pe.ServerID] = true
			}
			var out []string
			for h := range set {
				out = append(out, h)
			}
			sort.Strings(out)
			return out, nil
		}
		return nil, err
	}
	defer func() { _ = c.Close(context.WithoutCancel(ctx)) }()
	rows, err := c.Query(ctx, `SELECT DISTINCT host FROM kingres.node ORDER BY host`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var h string
		if err := rows.Scan(&h); err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

func (p *mapPorts) Live(ctx context.Context, s string) string { return p.m.pl.Live(ctx, s) }

// Standbys reads the plan file of this server (what the leader last told it): the system database is not writable, and its
// copy may be older than the plan.
func (p *mapPorts) Standbys(context.Context) ([]failover.StandbyRec, error) {
	d := p.m.fd.d
	if d == nil || d.Standby == nil {
		return nil, errors.New("the standby agent is not running")
	}
	var out []failover.StandbyRec
	for _, e := range d.Standby.agent.PlanEntries() {
		if e.Group != 0 {
			continue
		}
		st := "creating"
		if e.Ready {
			st = "ready"
		}
		out = append(out, failover.StandbyRec{ID: e.ID, Group: e.Group, Server: e.Server, InstanceID: e.InstanceID, Port: e.Port, State: st, CatalogNode: e.CatalogNode, Generation: e.Generation})
	}
	return out, nil
}

// Facts reads the leader's last record of group 0 from the local copy of the system database.
func (p *mapPorts) Facts(ctx context.Context) (failover.GroupFacts, error) {
	local := *p.m.pl
	local.localSys = p.m.sysConn
	return local.Facts(ctx, 0)
}

func (p *mapPorts) Position(ctx context.Context, s string) (failover.Position, error) {
	return p.m.peers.Position(ctx, s, 0)
}
func (p *mapPorts) State(ctx context.Context, s string) (failover.State, error) {
	return p.m.peers.State(ctx, s, 0)
}
func (p *mapPorts) Demote(ctx context.Context, s string, r failover.DemoteRequest) (failover.DemoteAnswer, error) {
	return p.m.peers.Demote(ctx, s, r)
}
func (p *mapPorts) Lease(ctx context.Context, s string, r failover.LeaseRequest) (failover.LeaseAnswer, error) {
	return p.m.peers.Lease(ctx, s, r)
}
func (p *mapPorts) Promote(ctx context.Context, s string, r failover.PromoteRequest) (failover.PromoteAnswer, error) {
	return p.m.peers.Promote(ctx, s, r)
}

func (p *mapPorts) FreezeLocal(_ context.Context, ttl time.Duration) error {
	d := p.m.fd.d
	if d == nil || d.Standby == nil {
		return errors.New("the standby agent is not running")
	}
	return d.Standby.agent.FreezeBy(0, ttl, 0, 0)
}

func (p *mapPorts) Continue(ctx context.Context, server string, j journal.Journal) error {
	var out struct{}
	return p.m.peers.do(ctx, server, http.MethodPost, "/failover/map/continue", j, &out)
}

func (p *mapPorts) Catalog() failover.Catalog { return p.m.cat }

func (p *mapPorts) PauseRebalance(ctx context.Context) (int, error) {
	c, err := p.m.sysConn(ctx)
	if err != nil {
		return 0, err
	}
	defer func() { _ = c.Close(context.WithoutCancel(ctx)) }()
	return rebalance.PauseForMapGroupFailover(ctx, c)
}

// RecordTask records the task of the failover from the journal, on the promoted instance: group 0 first, then the ordinary groups
// whose primary runs on the old leader's server (they get the normal steps).
func (p *mapPorts) RecordTask(ctx context.Context, j journal.Journal) (int64, error) {
	c, err := p.m.sysConn(ctx)
	if err != nil {
		return 0, err
	}
	defer func() { _ = c.Close(context.WithoutCancel(ctx)) }()
	rows, err := c.Query(ctx, `SELECT node_id, group_id, host, port, role FROM kingres.node ORDER BY node_id`)
	if err != nil {
		return 0, err
	}
	var nodes []failover.Node
	for rows.Next() {
		var n failover.Node
		if err := rows.Scan(&n.ID, &n.Group, &n.Host, &n.Port, &n.Role); err != nil {
			rows.Close()
			return 0, err
		}
		nodes = append(nodes, n)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	var termAfter int64
	if err := c.QueryRow(ctx, `SELECT term FROM kingres.catalog_status`).Scan(&termAfter); err != nil {
		return 0, err
	}
	var oldNode int32
	for _, n := range nodes {
		if n.Group == 0 && n.Host == j.OldServer {
			oldNode = n.ID
		}
	}
	groups := []int32{0}
	for _, g := range failover.GroupsOn(nodes, j.OldServer) {
		if g != 0 {
			groups = append(groups, g)
		}
	}
	rec := failover.MapRecord{JournalID: j.ID, Input: failover.Input{Mode: failover.ModeHard, ServerID: j.OldServer, Groups: groups, Trigger: j.Trigger,
		RequestedBy: j.RequestedBy, AllowDataLoss: j.AllowDataLoss, Comment: "the map group (bootstrap " + j.ID + ")"},
		OldNode: oldNode, NewNode: j.Pin.Node, NewServer: j.Pin.Server, ChosenStandby: j.Pin.StandbyID, TermBefore: j.Term, TermAfter: termAfter, StartedAt: j.StartedAt}
	dl := failover.DataLoss{Verdict: failover.VerdictUnknown, AsyncDatabases: []string{}, UnreadableServerIDs: []string{}}
	for _, e := range j.Events {
		rec.Events = append(rec.Events, failover.MapEvent{At: e.At, Step: e.Step, Kind: e.Kind, Detail: e.Detail})
		switch e.Kind {
		case "pinned":
			if v, ok := e.Detail["verdict"].(string); ok {
				dl.Verdict = v
			}
			if m, ok := e.Detail["message"].(string); ok {
				dl.Message = m
			}
		case "promoted":
			if tl, ok := e.Detail["timeline"].(float64); ok {
				rec.Timeline = int64(tl)
			}
		case "leaseHeld":
			if w, ok := e.Detail["waitMs"].(float64); ok {
				rec.LeaseWaitMs = int(w)
			}
		}
	}
	rec.DataLoss, _ = json.Marshal(dl)
	id, err := p.m.rr.RecordMapGroupRun(ctx, c, rec)
	if err != nil {
		return 0, err
	}
	if j.Trigger == "automatic" {
		p.m.pl.Alert(ctx, "autoFailoverTriggered", "info", "cluster", "An automatic failover of server "+j.OldServer+" ended: the map group was promoted.", true)
	}
	return id, nil
}

func (p *mapPorts) Alert(ctx context.Context, kind, severity, subject, message string, raise bool) {
	// Before the promotion the system database of this server cannot be written: the alert is then only logged, and the journal
	// has the event. The planner writes it when it can.
	p.m.pl.Alert(ctx, kind, severity, subject, message, raise)
	if raise {
		p.m.log.Warn("the failover of the map group raised an alert", "kind", kind, "message", message)
	}
}

var _ = strconv.Itoa

// engineGate is the gate of the task engine: closed while a bootstrap of the map group is in progress on this server (design 3.4,
// "the engine gate"): a server that was promoted in PostgreSQL and is not yet the recognised map primary must not start tasks that
// call promote_node (the extension would refuse them), nor resume the old leader's rebalance.
func engineGate(fd *failoverDeps) func() bool {
	if fd == nil || fd.mf == nil {
		return nil
	}
	return fd.mf.journal.Gate
}

func (p *mapPorts) CheckAuthority(context.Context) (bool, error) {
	_, err := os.Stat(filepath.Join(p.m.svc.PKIDir(), pki.CAKeyFile))
	if err == nil {
		return true, nil
	}
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	return false, err
}

func (p *mapPorts) MoveSystem(context.Context) error {
	p.m.svc.PublishSystemDSN()
	return nil
}
