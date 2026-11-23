// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
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
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/auth"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/cluster"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/failover"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/failover/journal"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/failover/mapgroup"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/fwdproof"
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
	callMapAbandon  = "POST /failover/map/abandon"
	callMapProof    = "POST /failover/map/proof"
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

	// releaseFence lets go of the fence and holder lease that the bootstrap of this server holds for the map group; tellOldLeader tells the old leader of a planned
	// switchover that its switchover is refused (its task rolls back). They are fields so that a test can see them called.
	releaseFence  func(ctx context.Context) error
	tellOldLeader func(ctx context.Context, oldServer string, taskID int64, reason string) error
	// candidateCheck says whether a server hosts a ready standby of the map group (nil: read from the catalog of this server).
	candidateCheck func(ctx context.Context, server string) bool
	// inRecovery asks the instance of the standby that this server would promote whether it is still in recovery (nil: it cannot be asked, and a promotion that was begun is
	// then never given up).
	inRecovery func(ctx context.Context) (bool, error)
	// lifecycleLock takes the standby agent's lifecycle lock, which the promoting call holds across pg_promote (nil: none is taken, a test without an agent); resumeOldLeader
	// asks the old leader's server to end the hold of its primary (F6-01, F6-02).
	lifecycleLock   func(ctx context.Context) (func(), error)
	resumeOldLeader func(ctx context.Context, oldServer string) (bool, error)
	// The parts of the resolution of a held-back primary that a test stands in for (nil: the real ones): the calls to other servers, the list of servers, the marker, the end of
	// the hold, and the two ways of asking an instance whether it is in recovery.
	candidateRecord func() string // the server that the hold was made for (nil: the record of the lease manager)
	callPeer        func(ctx context.Context, server, method, path string, in, out any) error
	memberList      func(ctx context.Context) ([]string, error)
	heldBack        func() bool
	endHold         func(ctx context.Context) error
	queryInRec      func(ctx context.Context, id int) (bool, error)
	controlDataOf   func(ctx context.Context, id int) (map[string]string, error)

	mu      sync.Mutex
	running bool
	lastErr string

	// releasePending says that the lease of a bootstrap that was given up could not be let go of: the next abandon finishes the release (F7-02).
	releasePending atomic.Bool

	// heldWhy is why the hold of this server's primary of the map group cannot be resolved (empty: it can, or there is none); heldWarned is when it was last named in the log.
	heldWhy    string
	heldWarned time.Time
}

func newMapFailover(dataDir string, svc *cluster.Service, self string, log *slog.Logger, fd *failoverDeps, peers *foPeers, cat *foCatalog, pl *foPlanner, rr *failover.Runner,
	faultErr func(point string) error) (*mapFailover, error) {
	js, err := journal.Open(filepath.Join(dataDir, "failover"))
	if err != nil {
		return nil, err
	}
	m := &mapFailover{svc: svc, self: self, log: log, fd: fd, peers: peers, cat: cat, pl: pl, rr: rr, journal: js}
	m.releaseFence = func(ctx context.Context) error {
		_, err := m.peers.Lease(ctx, m.self, failover.LeaseRequest{Group: 0, Role: failover.LeaseRelease})
		return err
	}
	m.tellOldLeader = func(ctx context.Context, oldServer string, taskID int64, reason string) error {
		return m.peers.do(ctx, oldServer, http.MethodPost, "/failover/map/refused", mapRefusedBody{Reason: reason, TaskID: taskID}, nil)
	}
	m.inRecovery = func(ctx context.Context) (bool, error) {
		cur, ok := m.journal.Current()
		if !ok || cur.Pin == nil {
			return true, nil // nothing is pinned: nothing was promoted
		}
		return m.instanceInRecovery(ctx, cur.Pin.Instance)
	}
	m.lifecycleLock = func(ctx context.Context) (func(), error) {
		if d := m.fd.d; d != nil && d.Standby != nil {
			return d.Standby.agent.LockLifecycle(ctx)
		}
		return func() {}, nil
	}
	m.resumeOldLeader = m.resumeOldLeaderOf
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
	if m.runner != nil && m.runner.HardInProgress(server) {
		// A hard failover of a live leader's server that paused here (the loss was not accepted after the stop): the old leader's task cannot take the
		// repeated request, its system database is the stopped primary, so the bootstrap does.
		return true
	}
	return m.pl.Live(ctx, server) != "live"
}

func (m *mapFailover) active() bool { return m.journal.Active() }

// Active says whether a bootstrap is in progress on this server.
func (m *mapFailover) Active() bool { return m.active() }

// Start answers a request for the failover of the map group (HTTP, on any server): the candidate runs the bootstrap, and the
// answer is a task with id 0 while no task can be recorded (the journal's steps), or the recorded task.
func (m *mapFailover) Start(ctx context.Context, actor string, id internalapi.ForwardIdentity, server string, req failover.Request) (task.Task, error) {
	if req.Mode != failover.ModeHard {
		return task.Task{}, problem.New(problem.UnsupportedOp, "The map group is failed over in mode hard only.")
	}
	if err := m.targetMatchesTheJournal(server, req); err != nil {
		return task.Task{}, err
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
		// The candidate does not take the word of a member for what an operator asked (F4-05): the request travels with the proof of the operator's session,
		// made over exactly these bytes, and the candidate checks it against the verifier of the role it holds in its copy of the system database.
		signed, serr := m.sign(id, body, cand)
		if serr != nil {
			return task.Task{}, problem.New(problem.ServiceUnavailable, "The session cannot prove itself to the server that promotes the map group: "+serr.Error()).WithRetryAfter(5)
		}
		var out struct{}
		if err := m.peers.do(ctx, cand, http.MethodPost, "/failover/map/start", signed, &out); err != nil {
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
	// Planned is set by the old leader's own task for a planned switchover (handleMapStart accepts it from that server only).
	Planned *journal.Planned `json:"planned,omitempty"`
}

// begin runs B1 here (the refusal answers the request) and starts the rest in the background.
func (m *mapFailover) begin(ctx context.Context, b mapStartBody) error {
	if _, err := m.runner.Decide(ctx, mapgroup.Request{OldServer: b.Server, Trigger: nonEmptyStr(b.Trigger, "manual"), RequestedBy: b.RequestedBy, AllowDataLoss: b.AllowDataLoss, Comment: b.Comment, Planned: b.Planned}); err != nil {
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
		return m.heldStatus()
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

func handleMapStart(ctx context.Context, d *haDeps, caller string, body []byte) (any, error) {
	mf := d.MapFailover
	if mf == nil {
		return nil, notStarted
	}
	b, proven, err := openMapStart(ctx, d, caller, body)
	if err != nil {
		return nil, err
	}
	// The call is the request of an operator that a member received (F4-05): it is never automatic (the monitor of this server starts an
	// automatic bootstrap itself), and it does not name the system as its author.
	b.Trigger = "manual"
	if proven != "" {
		b.RequestedBy = proven // the user the proof is about, whatever the request says
	} else if b.RequestedBy == "" || b.RequestedBy == "system" {
		b.RequestedBy = "operator (via " + caller + ")"
	}
	if b.Planned != nil {
		if err := acceptPlanned(mf, caller, &b); err != nil {
			return nil, err
		}
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
func handleMapContinue(ctx context.Context, d *haDeps, caller string, body []byte) (any, error) {
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
	// The caller is a server that hosts a ready standby of the map group (it was a candidate that found this one better), and the journal it
	// sends has done the steps up to B4 and no more (F4-05): what comes after is done here, under this server's own lease.
	if ok, _ := (&mapPorts{m: mf}).isCandidate(ctx, caller); !ok {
		return nil, &internalapi.CallError{Status: http.StatusForbidden, Code: "forbidden", Message: "Only a server that hosts a standby of the map group hands a bootstrap over."}
	}
	for _, step := range []string{journal.StepPromote, journal.StepCatalog, journal.StepLeader, journal.StepGateOpen} {
		delete(j.Done, step)
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
	// A test: the position of this server's own standby cannot be read when the bootstrap ranks the standbys (the verdict is then unknown and the bootstrap pauses).
	if d := p.m.fd.d; d != nil && s == p.m.self && d.Fault.FailoverInjection("pin:unreadable") == "fail" {
		return failover.Position{}, errors.New("the position cannot be read (injected)")
	}
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

// isCandidate says that server hosts a ready, registered standby of the map group.
func (p *mapPorts) isCandidate(ctx context.Context, server string) (bool, error) {
	recs, err := p.Standbys(ctx)
	if err != nil {
		return false, err
	}
	for _, s := range recs {
		if s.Server == server && s.State == "ready" && s.CatalogNode != 0 {
			return true, nil
		}
	}
	return false, nil
}

// ---------------------------------------------------------------------------------
// Giving up a bootstrap (F4-06)

type mapAbandonBody struct {
	Reason string `json:"reason"`
}

type mapAbandonAnswer struct {
	Abandoned bool `json:"abandoned"`
	// ResumedServer names the old leader whose held-back primary was started again by giving a planned switchover up (F6-01).
	ResumedServer string `json:"resumedServer,omitempty"`
}

// abandonLocal gives up the bootstrap in progress on this server; false when none is. A bootstrap that promoted the standby cannot be given
// up (409): the promoted instance holds the lease and the bootstrap goes on to its end.
func (m *mapFailover) abandonLocal(ctx context.Context, reason string) (bool, error) {
	ans, err := m.abandonLocalFull(ctx, reason)
	return ans.Abandoned, err
}

// abandonLocalFull is abandonLocal with what else it did: for a planned switchover it asks the old leader's server to end the hold of its primary.
func (m *mapFailover) abandonLocalFull(ctx context.Context, reason string) (mapAbandonAnswer, error) {
	if !m.journal.Active() {
		// A bootstrap that was given up and whose lease could not be let go of: the repeat of the abandon finishes the release (F7-02).
		if cur, ok := m.journal.Current(); ok && !cur.Abandoned.IsZero() && m.releasePending.Load() {
			if err := m.releaseTheLease(ctx); err != nil {
				return mapAbandonAnswer{}, problem.New(problem.ServiceUnavailable, "The lease of the map group still cannot be let go of; repeat the call.").WithRetryAfter(5)
			}
			return m.afterRelease(ctx, cur, reason), nil
		}
		return mapAbandonAnswer{}, nil
	}
	cur, _ := m.journal.Current()
	// The lifecycle lock of the standby agent is what the promoting call holds across pg_promote (F6-02): the probe, the mark of the journal and the release of the lease
	// happen together, so that a promotion that is running is waited for (and then found, and refused: 409), and one that has not begun is refused by the lease it
	// finds let go of.
	unlock := func() {}
	if m.lifecycleLock != nil {
		u, lerr := m.lifecycleLock(ctx)
		if lerr != nil {
			return mapAbandonAnswer{}, problem.New(problem.ServiceUnavailable, "A promotion, a create, a remove or a rebuild of a standby is running on this server; repeat the call.").WithRetryAfter(5)
		}
		unlock = u
	}
	defer func() { unlock() }()
	if !m.journal.Active() {
		return mapAbandonAnswer{}, nil // given up or finished while the lock was waited for
	}
	err := m.journal.Abandon(reason)
	if errors.Is(err, journal.ErrPromoted) && cur.Done[journal.StepPromote].IsZero() && m.inRecovery != nil {
		// The call that promotes was begun and its answer is not recorded (F5-03): the bootstrap is given up only when the standby is asked and is still in recovery.
		if rec, rerr := m.inRecovery(ctx); rerr == nil && rec {
			err = m.journal.AbandonUnpromoted(reason)
		}
	}
	if err != nil {
		if errors.Is(err, journal.ErrPromoted) {
			return mapAbandonAnswer{}, problem.New(problem.Conflict, "The bootstrap of the map group promoted its standby, or was about to and the standby cannot be shown to be a standby still; it cannot be given up, and goes on to its end.")
		}
		return mapAbandonAnswer{}, err
	}
	m.log.Warn("the bootstrap of the map group was given up", "reason", reason)
	// What the bootstrap holds goes with it (F5-02): the fence and holder lease would otherwise be renewed for a bootstrap that no longer runs, and an old primary
	// that is stopped could not win its lease again. A lease that cannot be let go of fails the abandon (F7-02): the call answers that it must be repeated and the old
	// leader is not asked to resume; the repeat finishes the release (the journal is marked, and the promoting call refuses a journal that was given up).
	if err := m.releaseTheLease(ctx); err != nil {
		return mapAbandonAnswer{}, problem.New(problem.ServiceUnavailable, "The bootstrap was given up, but its lease of the map group could not be let go of; repeat the call.").WithRetryAfter(5)
	}
	unlock()
	unlock = func() {}
	return m.afterRelease(ctx, cur, reason), nil
}

// releaseTheLease lets go of the lease of the bootstrap; a failure is remembered, so that a repeated abandon does it again.
func (m *mapFailover) releaseTheLease(ctx context.Context) error {
	if m.releaseFence == nil {
		return nil
	}
	if err := m.releaseFence(ctx); err != nil {
		m.releasePending.Store(true)
		m.log.Error("letting go of the lease of the map group after giving the bootstrap up failed", "error", err)
		return err
	}
	m.releasePending.Store(false)
	return nil
}

// afterRelease is what follows the release of the lease of a given-up bootstrap: a planned switchover tells the old leader (F5-08) and asks it to end the hold of its
// primary, because its task may be gone (its kingresd restarted, or the task ended) and the system database is the stopped primary, so nothing else would (F6-01).
func (m *mapFailover) afterRelease(ctx context.Context, cur journal.Journal, reason string) mapAbandonAnswer {
	ans := mapAbandonAnswer{Abandoned: true}
	if cur.Planned != nil && m.tellOldLeader != nil {
		if err := m.tellOldLeader(ctx, cur.OldServer, cur.Planned.TaskID, reason); err != nil {
			m.log.Warn("telling the old leader that its switchover of the map group was given up", "error", err)
		}
	}
	if cur.Planned != nil && m.resumeOldLeader != nil {
		if resumed, err := m.resumeOldLeader(ctx, cur.OldServer); err != nil {
			m.log.Warn("asking the old leader to end the hold of its primary of the map group; it does so by itself when it can", "server", cur.OldServer, "error", err)
		} else if resumed {
			ans.ResumedServer = cur.OldServer
		}
	}
	return ans
}

// promoteBlockedByJournal is the refusal of the promoting call on this server for a bootstrap that was given up (F7-02): the call that was queued behind the lifecycle
// lock must not promote what the abandon let go of, whatever became of the release of the lease.
func promoteBlockedByJournal(mf *mapFailover, group int32) error {
	if mf == nil || group != 0 {
		return nil
	}
	if cur, ok := mf.journal.Current(); ok && !cur.Abandoned.IsZero() {
		return &internalapi.CallError{Status: http.StatusConflict, Code: "conflict", Message: "The bootstrap of the map group that asks for this promotion was given up."}
	}
	return nil
}

// Abandon gives up the bootstrap of the map group that is in progress on this server, or on the server of a standby of the map group: the
// request may arrive anywhere, and the leader may be the server that is gone. When none is in progress anywhere, it asks every server to end the hold of its
// primary of the map group that nothing owns (F6-03): the operator's way to start a stranded old leader again. The answer says which server was resumed.
func (m *mapFailover) Abandon(ctx context.Context, actor, reason string) (failover.MapAbandonResult, error) {
	reason = "given up by " + actor + ": " + reason
	if ans, err := m.abandonLocalFull(ctx, reason); ans.Abandoned || err != nil {
		return failover.MapAbandonResult{Abandoned: ans.Abandoned, ResumedServer: ans.ResumedServer}, err
	}
	members, err := m.members(ctx)
	if err != nil {
		return failover.MapAbandonResult{}, problem.New(problem.ServiceUnavailable, "The servers of the cluster cannot be read now.").WithRetryAfter(5)
	}
	var asked, failed int
	for _, s := range members {
		if s == m.self {
			continue
		}
		asked++
		var out mapAbandonAnswer
		if err := m.call(ctx, s, http.MethodPost, "/failover/map/abandon", mapAbandonBody{Reason: reason}, &out); err != nil {
			var ce *failover.CallError
			if errors.As(err, &ce) && ce.Status == http.StatusConflict {
				return failover.MapAbandonResult{}, problem.New(problem.Conflict, ce.Message)
			}
			failed++
			continue
		}
		if out.Abandoned {
			return failover.MapAbandonResult{Abandoned: true, ResumedServer: out.ResumedServer}, nil
		}
	}
	if asked > 0 && failed == asked {
		return failover.MapAbandonResult{}, problem.New(problem.ServiceUnavailable, "No other server of the cluster answered, so it is not known whether a bootstrap of the map group is in progress there.").WithRetryAfter(5)
	}
	// No bootstrap is in progress: a held-back old leader that nothing owns is started again (this server too).
	var res failover.MapAbandonResult
	for _, s := range members {
		var out mapResumeAnswer
		if s == m.self {
			out.Resumed, out.Detail = m.resolveHeldBack(ctx, true)
		} else if err := m.call(ctx, s, http.MethodPost, "/failover/map/resume", mapResumeBody{Strict: true}, &out); err != nil {
			continue
		}
		if out.Resumed {
			res.ResumedServer = s
			break
		}
	}
	return res, nil
}

// handleMapAbandon is the call of a server that was asked to give up the bootstrap: it acts on its own journal only.
func handleMapAbandon(ctx context.Context, d *haDeps, caller string, body []byte) (any, error) {
	mf := d.MapFailover
	if mf == nil {
		return nil, notStarted
	}
	var b mapAbandonBody
	if err := json.Unmarshal(body, &b); err != nil || b.Reason == "" || len(b.Reason) > 600 {
		return nil, badBody("a reason")
	}
	// The bootstrap of a planned switchover is given up by the old leader or by a server that hosts a standby of the map group; an ordinary bootstrap by the
	// server that received the operator's call (F4-06: it may be any server, the leader is the one that is gone) (F5-08).
	if cur, active := mf.journal.Current(); active && mf.journal.Active() && cur.Planned != nil && caller != cur.OldServer && caller != mf.self {
		if !mf.isCandidateServer(ctx, caller) {
			return nil, &internalapi.CallError{Status: http.StatusForbidden, Code: "forbidden", Message: "Only the old leader or a server that hosts a standby of the map group gives up a planned switchover."}
		}
	}
	ans, err := mf.abandonLocalFull(ctx, b.Reason)
	if err != nil {
		var p *problem.Problem
		if errors.As(err, &p) {
			return nil, &internalapi.CallError{Status: p.Status, Code: string(p.Code), Message: p.Detail}
		}
		return nil, err
	}
	return ans, nil
}

// mapProofBody is what the old leader sends after its clean stop (planned switchover): the shutdown checkpoint and the state of the cluster.
type mapProofBody struct {
	Checkpoint   string `json:"checkpoint"`
	ClusterState string `json:"clusterState"`
}

// handleMapProof records the proof of the old leader's clean stop in the journal of the bootstrap. Only the old server of the journal sends it; the
// standby that is promoted on it checks it against what it replayed (switchover_verify), so a wrong proof is refused there and not trusted here.
func handleMapProof(_ context.Context, d *haDeps, caller string, body []byte) (any, error) {
	mf := d.MapFailover
	if mf == nil {
		return nil, notStarted
	}
	var b mapProofBody
	if err := json.Unmarshal(body, &b); err != nil || b.Checkpoint == "" || b.ClusterState == "" || len(b.Checkpoint) > 64 || len(b.ClusterState) > 64 {
		return nil, badBody("a checkpoint and a cluster state")
	}
	cur, ok := mf.journal.Current()
	if !ok || !mf.journal.Active() || cur.Planned == nil {
		return nil, &internalapi.CallError{Status: http.StatusConflict, Code: "conflict", Message: "No planned switchover of the map group is in progress on this server."}
	}
	if caller != cur.OldServer {
		return nil, &internalapi.CallError{Status: http.StatusForbidden, Code: "forbidden", Message: "Only the old leader sends the proof of its stop."}
	}
	if err := mf.journal.SetProof(b.Checkpoint, b.ClusterState); err != nil {
		return nil, &internalapi.CallError{Status: http.StatusConflict, Code: "conflict", Message: err.Error()}
	}
	return struct{}{}, nil
}

// RecordPlanned marks the group 0 row of the old leader's task promoted, with the term after the promotion and the timeline, in the system database that
// this server promoted. The steps of the map group that the new leader's engine runs again read the term from the row (runRowOf).
func (p *mapPorts) RecordPlanned(ctx context.Context, taskID, termAfter, timeline int64, pin *journal.Pin, dataLoss []byte) error {
	c, err := p.m.sysConn(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = c.Close(context.WithoutCancel(ctx)) }()
	// A hard failover of a live leader's server pinned its standby after the stop, so the row has none yet: the pin is written only into a row that has none
	// (a switchover pinned in its task, and keeps its own).
	var chosen *int64
	var node *int32
	var server *string
	if pin != nil {
		chosen, node, server = &pin.StandbyID, ptrInt32(pin.Node), &pin.Server
	}
	tag, err := c.Exec(ctx, `UPDATE kingres_cp.failover_group
		SET state = 'promoted', term_after = $2, timeline = CASE WHEN $3 > 0 THEN $3 ELSE timeline END, promoted_at = COALESCE(promoted_at, now()),
		    chosen_standby_id = COALESCE(chosen_standby_id, $4), new_server = COALESCE(new_server, $5), new_node_id = COALESCE(new_node_id, $6::int),
		    data_loss = CASE WHEN chosen_standby_id IS NULL AND $7::jsonb IS NOT NULL THEN $7::jsonb ELSE data_loss END
		WHERE task_id = $1 AND group_id = 0 AND state IN ('pending', 'frozen', 'fenced', 'pinned', 'promoted')`, taskID, termAfter, timeline, chosen, server, node, nullJSON(dataLoss))
	if err == nil && tag.RowsAffected() == 0 {
		return mapgroup.ErrNoPlannedRow
	}
	return err
}

func ptrInt32(v int32) *int32 { return &v }

// nullJSON is a JSON value for a jsonb parameter, or nil (SQL NULL) for none.
func nullJSON(b []byte) any {
	if len(b) == 0 {
		return nil
	}
	return string(b)
}

// mapStartSigned is the body of POST /failover/map/start that a member sends for a request of an operator (F4-05): the request exactly as it was signed,
// the identity the proof is about (never the key), and the proof.
type mapStartSigned struct {
	Request   json.RawMessage `json:"request"`
	Username  string          `json:"username"`
	Roles     []string        `json:"clusterRoles"`
	RequestID string          `json:"requestId"`
	Proof     *fwdproof.Proof `json:"proof"`
}

// mapStartMessage is what the proof of a map start covers: the kind, the two servers, the user and the roles, the operation and its parameter, and the
// request bytes.
func mapStartMessage(from, to string, s mapStartSigned, server string) fwdproof.Message {
	return internalapi.ForwardRequest{Identity: internalapi.ForwardIdentity{Username: s.Username, ClusterRoles: s.Roles}, OperationID: "failoverServer",
		PathParams: map[string]string{"serverId": server}, ContentType: "application/json", Body: s.Request, RequestID: s.RequestID}.ProofMessage(fwdproof.KindMapStart, from, to)
}

// sign makes the signed request for the candidate to.
func (m *mapFailover) sign(id internalapi.ForwardIdentity, body mapStartBody, to string) (mapStartSigned, error) {
	raw, err := json.Marshal(body)
	if err != nil {
		return mapStartSigned{}, err
	}
	var nonce [8]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return mapStartSigned{}, err
	}
	s := mapStartSigned{Request: raw, Username: id.Username, Roles: id.ClusterRoles, RequestID: hex.EncodeToString(nonce[:])}
	p, err := fwdproof.Sign(id.ProofKey, mapStartMessage(m.self, to, s, body.Server), time.Now())
	if err != nil {
		return mapStartSigned{}, err
	}
	s.Proof = &p
	return s, nil
}

// openMapStart reads the body of POST /failover/map/start. A request of an operator is the signed form: it is accepted only when the proof of the
// operator's session over exactly its bytes verifies (the same check as for a forwarded operation, against the verifier of the role in the copy of the
// system database that this server holds) and the roles the proof names allow the failover (kingres_ops); the decision allowDataLoss it carries is then
// the operator's, and the user it names is the author. The plain form is the planned switchover that the old leader's own task asks for and is
// accepted from nobody else; any other plain request is refused, so a member that is not honest cannot set the decision or the author (F4-05).
// It returns the request and the proven user ("" for the plain form).
func openMapStart(ctx context.Context, d *haDeps, caller string, body []byte) (mapStartBody, string, error) {
	var peek struct {
		Request json.RawMessage `json:"request"`
		Proof   json.RawMessage `json:"proof"`
	}
	_ = json.Unmarshal(body, &peek)
	if peek.Request == nil && peek.Proof == nil {
		var b mapStartBody
		if err := json.Unmarshal(body, &b); err != nil || b.Server == "" {
			return b, "", badBody("a map group failover request")
		}
		if b.Planned == nil {
			return b, "", &internalapi.CallError{Status: http.StatusUnauthorized, Code: "identity_unproven", Message: "The request of an operator comes with the proof of the operator's session."}
		}
		return b, "", nil
	}
	var s mapStartSigned
	if err := json.Unmarshal(body, &s); err != nil || s.Proof == nil || s.Username == "" || len(s.Request) == 0 {
		return mapStartBody{}, "", badBody("a signed map group failover request")
	}
	var b mapStartBody
	if err := json.Unmarshal(s.Request, &b); err != nil || b.Server == "" || b.Planned != nil {
		return b, "", badBody("a map group failover request")
	}
	if d.IdentityCheck == nil {
		return b, "", &internalapi.CallError{Status: http.StatusUnauthorized, Code: "identity_unproven", Message: "This server cannot check the proof of a session."}
	}
	fr := internalapi.ForwardRequest{Identity: internalapi.ForwardIdentity{Username: s.Username, ClusterRoles: s.Roles}, OperationID: "failoverServer",
		PathParams: map[string]string{"serverId": b.Server}, ContentType: "application/json", Body: s.Request, RequestID: s.RequestID, Proof: s.Proof}
	if err := d.IdentityCheck(ctx, fwdproof.KindMapStart, caller, fr); err != nil {
		if errors.Is(err, fwdproof.ErrUnavailable) {
			return b, "", &internalapi.CallError{Status: http.StatusServiceUnavailable, Code: "unavailable", Message: "The identity cannot be checked now."}
		}
		return b, "", &internalapi.CallError{Status: http.StatusUnauthorized, Code: "identity_unproven", Message: "The identity of the request is not proven."}
	}
	if !auth.Allows(auth.Normalize(s.Roles), auth.RoleOps) {
		return b, "", &internalapi.CallError{Status: http.StatusForbidden, Code: "forbidden", Message: "The failover needs the role kingres_ops."}
	}
	return b, s.Username, nil
}

// ---------------------------------------------------------------------------------
// A planned switchover of the map group whose proof is refused after the old leader stopped

const callMapRefused = "POST /failover/map/refused"

type mapRefusedBody struct {
	Reason string `json:"reason"`
	// TaskID is the task of the old leader that asked for the bootstrap: a refusal of another task is not this task's (F5-09).
	TaskID int64 `json:"taskId"`
}

// plannedBox is what the old leader's server remembers of the planned switchover its own task is running: the server of the candidate that it asked to
// start the bootstrap, and the refusal that the candidate reported. A refusal is accepted from that server only.
type plannedBox struct {
	mu      sync.Mutex
	to      string
	task    int64
	refused string
}

var mapPlanned = &plannedBox{}

func (b *plannedBox) started(server string, taskID int64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.to, b.task, b.refused = server, taskID, ""
}

func (b *plannedBox) refuse(server string, taskID int64, reason string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.to == "" || b.to != server || b.task != taskID {
		return false
	}
	b.refused = reason
	return true
}

func (b *plannedBox) refusal(taskID int64) (string, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.refused, b.task == taskID && b.refused != ""
}

// handleMapRefused is the call of the candidate to the old leader (this server) that the standby could not prove the old leader's stop: the task that is
// waiting for the verdict rolls back and the old leader's primary is started again by its lease manager.
func handleMapRefused(_ context.Context, _ *haDeps, caller string, body []byte) (any, error) {
	var b mapRefusedBody
	if err := json.Unmarshal(body, &b); err != nil || b.Reason == "" || len(b.Reason) > 600 {
		return nil, badBody("a reason")
	}
	if !mapPlanned.refuse(caller, b.TaskID, b.Reason) {
		return nil, &internalapi.CallError{Status: http.StatusConflict, Code: "conflict", Message: "No planned switchover of the map group was started on this server for the caller."}
	}
	return struct{}{}, nil
}

// PlannedRefused lets go of the fence lease that this server holds for the map group and tells the old leader.
func (p *mapPorts) PlannedRefused(ctx context.Context, oldServer string, taskID int64, reason string) error {
	if err := p.m.releaseFence(ctx); err != nil {
		return fmt.Errorf("letting go of the lease of the map group: %w", err)
	}
	return p.m.tellOldLeader(ctx, oldServer, taskID, reason)
}

// ---------------------------------------------------------------------------------
// The proof that the candidate reads itself when the old leader's task cannot send it (the leader's kingresd ended after the clean stop)

const callMapCheckpoint = "GET /failover/map/checkpoint"

// handleMapCheckpoint answers how the stopped primary of the map group on this server was left: the shutdown checkpoint and the state of the cluster. It
// is a read of pg_controldata of an instance that does not run (409 not_stopped when it does), so it gives away nothing and changes nothing; the standby
// that is promoted on it checks it against what it replayed (switchover_verify), exactly as when the old leader's task sends it.
func handleMapCheckpoint(ctx context.Context, d *haDeps, _ string, _ []byte) (any, error) {
	li, ok := d.Svc.PrimaryOf(0)
	held := d.LeaseMgr != nil && d.LeaseMgr.HeldBack(0)
	return mapCheckpoint(ctx, d.Svc.Manager(), li.ID, ok, held)
}

// mapCheckpoint is handleMapCheckpoint on the parts that a test can stand in for. The answer is given only while a marker holds the primary of the map group back on
// this server (a hand-over, a fence or a wait): a primary that the start gate would let start never gives one (F5-12).
func mapCheckpoint(ctx context.Context, mgr stopManager, id int, hasPrimary, heldBack bool) (any, error) {
	if !hasPrimary {
		return nil, &internalapi.CallError{Status: http.StatusConflict, Code: "not_primary", Message: "This server runs no primary of the map group."}
	}
	if !heldBack {
		return nil, &internalapi.CallError{Status: http.StatusConflict, Code: "not_held_back", Message: "The primary of the map group is not held back from starting on this server."}
	}
	running, err := mgr.Running(ctx, id)
	if err != nil {
		return nil, &internalapi.CallError{Status: http.StatusServiceUnavailable, Code: "unavailable", Message: "The instance cannot be asked whether it runs."}
	}
	if running {
		return nil, &internalapi.CallError{Status: http.StatusConflict, Code: "not_stopped", Message: "The primary of the map group runs on this server."}
	}
	cd, err := mgr.ControlData(ctx, id)
	if err != nil {
		return nil, &internalapi.CallError{Status: http.StatusServiceUnavailable, Code: "unavailable", Message: "pg_controldata cannot be read: " + err.Error()}
	}
	return failover.MapCheckpoint{Checkpoint: cd["Latest checkpoint location"], ClusterState: cd["Database cluster state"]}, nil
}

// PullProof reads the proof from the old leader's server.
func (p *mapPorts) PullProof(ctx context.Context, oldServer string) (string, string, error) {
	var out failover.MapCheckpoint
	if err := p.m.peers.do(ctx, oldServer, http.MethodGet, "/failover/map/checkpoint", nil, &out); err != nil {
		return "", "", err
	}
	return out.Checkpoint, out.ClusterState, nil
}

// isCandidateServer says whether server hosts a ready standby of the map group.
func (m *mapFailover) isCandidateServer(ctx context.Context, server string) bool {
	if m.candidateCheck != nil {
		return m.candidateCheck(ctx, server)
	}
	ok, _ := (&mapPorts{m: m}).isCandidate(ctx, server)
	return ok
}

// ResumeOldLeader asks the old leader's server to end the hold of its primary of the map group (F6-01).
func (p *mapPorts) ResumeOldLeader(ctx context.Context, oldServer string) error {
	if p.m.resumeOldLeader == nil {
		return nil
	}
	_, err := p.m.resumeOldLeader(ctx, oldServer)
	return err
}

// acceptPlanned checks and normalises the request of the old leader's own task (a planned switchover, or a hard failover of a live leader's server). Only that
// server may say so. A planned switchover says nothing about data loss (there is none to accept); a hard failover carries the acceptance of the operator whose
// request the leader checked, and the facts of the leader's last check before the stop.
func acceptPlanned(mf *mapFailover, caller string, b *mapStartBody) error {
	if caller != b.Server || b.Planned.TaskID <= 0 || b.Planned.StandbyID <= 0 {
		return &internalapi.CallError{Status: http.StatusForbidden, Code: "forbidden", Message: "A planned switchover of the map group is started by the task of the old leader, on its own server."}
	}
	// A planned switchover that was given up is not started again by the same task: the task could not record its rollback (its rows were in the stopped
	// instance and its lease ran out), so the engine runs the step again when the old leader is back, and that run must end as a failure instead of
	// beginning a second switchover (found by the cluster tests of the review of round 2).
	if cur, ok := mf.journal.Current(); ok && !cur.Abandoned.IsZero() && cur.Planned != nil && cur.Planned.TaskID == b.Planned.TaskID {
		return &internalapi.CallError{Status: http.StatusConflict, Code: "conflict", Message: "The planned switchover of this task was given up; it is not started again."}
	}
	if b.Planned.Hard && (b.Planned.Checkpoint != "" || b.Planned.ClusterState != "") {
		// P6: a hard failover has no proof; one that carries a proof is not the request of an honest task.
		return badBody("a hard failover request without a proof")
	}
	b.Planned.Checkpoint, b.Planned.ClusterState = "", ""
	if !b.Planned.Hard {
		b.AllowDataLoss = false
		b.Planned.Facts = nil
	}
	return nil
}

// targetMatchesTheJournal refuses the repeated request of an operator for a hard failover in progress whose target for the map group is not the one the journal was begun
// with (P4): the standby was chosen, and the old primary stopped, for that one; the operator gives the bootstrap up first to change it.
func (m *mapFailover) targetMatchesTheJournal(server string, req failover.Request) error {
	cur, ok := m.journal.Current()
	if !ok || !m.journal.Active() || cur.Planned == nil || !cur.Planned.Hard || cur.OldServer != server {
		return nil
	}
	target := ""
	for _, t := range req.Targets {
		if t.ShardGroupID == 0 {
			target = t.ServerID
		}
	}
	if target != cur.Planned.Target {
		return problem.New(problem.Conflict, "The hard failover of the map group is in progress with target \""+cur.Planned.Target+"\" (empty: the best standby) and this request names \""+target+"\"; give the bootstrap up (abandonMapGroupBootstrap) before asking for another target.")
	}
	return nil
}
