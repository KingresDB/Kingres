// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package failover

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/task"
)

// world is a small model of a cluster of three servers for the steps: s1 ran the
// primary of group 1 (node 1), s2 (the leader, the map group's server) and s3 host the
// standbys 10 and 11 (nodes 4 and 5). Every call of the ports is recorded in order.
type world struct {
	mu    sync.Mutex
	log   []string
	nodes []Node
	// shop is the catalog of a second sharded database: its own node ids (nil: no such database).
	shop       []Node
	noFollow   string
	noWatchdog bool
	term       int64
	// the old primary's server
	oldAlive      bool
	oldWritable   bool // its instance still runs
	oldRefusesDem error
	// standbys
	standbys       []StandbyRec
	pos            map[string]Position // by server
	posErr         map[string]error
	facts          GroupFacts
	live           map[string]string
	lease          map[string]bool // server holds the lease of group 1
	leaseHeldByOld bool            // the grantors still renew the old holder
	leaseErr       error           // the call that takes the fence lease fails with this error
	frozen         bool
	// From the first call that asks for the fence lease on, the renewal of the freeze fails with renewFreezeErr, and a
	// state read says the group is not frozen when renewUnfrozen is set (an old primary that is stopped).
	renewFreezeErr error
	renewUnfrozen  bool
	liveFn         func(server string) string // when set, the liveness view of a server (instead of the live map)
	promoted       map[string]bool
	promoteErr     map[string]error // scripted errors for the first calls, by server
	promoteN       map[string]int
	catScript      []any
	catCalls       int
	catalogFollows bool
	primaryNames   []int64
	rebalance      bool
	settings       map[string]LeaseSettings
	settingsErr    map[string]error
	records        int
	flagged        []int64
	alerts         []string
	rows           *memRows
	prepared       int
	// the switchover
	oldHolds                  bool // the old primary's server holds the lease of its group
	quiesceCalls              int  // calls of Quiesce that answer "sessions remain" before it succeeds
	quiesceFail               bool // Quiesce never succeeds
	oldPrepared               int  // prepared transactions on the old primary (decreasing by recovery)
	markReplayed              bool // the standby has replayed the mark
	stopState                 string
	stopStates                []string
	stopN                     int
	promoteReqs               []PromoteRequest
	oldPreparedUnknown        bool // the old primary does not report a count of prepared transactions
	mapGroup                  bool // the group of the world is the map group (0), not 1
	mapStarts                 []MapStartRequest
	mapProofs                 []MapProofRequest
	noPromoteOnProof          bool
	mapProofErr               error
	stateDown                 map[string]bool // the calls to this server fail (a transient error on the state read)
	mapAbandonErr             error           // the candidate answers the abandon with this (a 409 conflict: it promoted)
	mapRefusal                string
	mapStartErr               error
	liveErr                   error            // the primary of the map group cannot be asked
	live2                     *LiveReplication // what it says (nil: what the recorded facts say)
	liveReads                 int
	onMapStart                func()  // runs inside the call that starts the bootstrap of the candidate (a test changes the facts there)
	markErr                   error   // the answer of the mark call
	undoTerms                 []int64 // the terms of the quiesce, unquiesce and resume calls
	unquiesceStale            int     // the unquiesce calls that were refused as stale (the catalog term is higher than the call's, as the callee judges it)
	stopErr                   error   // the answer of the clean stop call
	dbDownAfterStop           bool    // the rows cannot be read after the clean stop (the map group: its system database is the stopped instance)
	onResume                  func()  // called when the old primary is resumed by a rollback (a test reads the lease of the task at that moment)
	onDemote                  func()  // called when the clean stop is asked for, before anything else (a test cancels the context of the step)
	beforeStop                func()  // called when the clean stop is asked for, before it is done (a test reads what the task lease is at that moment)
	afterStop                 func()  // called when the clean stop is done (a test cancels the context of the step, as the engine does when the task lease cannot be renewed)
	notReleased               bool    // the clean stop does not release the lease
	markTerms                 []int64
	stopTerms                 []int64
	stopCandidates            []string // the Candidate of every clean stop request
	stopProofOnly             []bool   // ProofOnly of every clean stop request, in the order they were made
	oldCalleeIgnoresProofOnly bool     // the old server is of a version that does not know the flag: it answers a full stop
	proofNotStopped           bool     // the instance of the old primary runs again when the proof alone is asked for
	promoteRefuse             error    // the answer of a promote call that carries a proof (the callee refuses it)
	verifyAnswers             []bool   // the answers of the proof, in order (the last one repeats)
	verifyErr                 error
	verifyN                   int
	quiesced                  bool
	immediate                 []DemoteRequest // the immediate stops that were asked for
	hardLate                  int             // a hard failover of the map group: the candidate shows as promoted after this many reads of its state (0: at once)
	hardPending               string          // the candidate that promotes late
	oldSrv                    string          // the server whose primary is handed over (default s1)
	self                      string          // the leader's server (default s2)
}

func newWorld() *world {
	w := &world{
		oldSrv:   "s1",
		self:     "s2",
		oldAlive: false,
		term:     5,
		nodes: []Node{
			{ID: 0, Group: 0, Host: "s2", Port: 5400, Role: "primary"},
			{ID: 1, Group: 1, Host: "s1", Port: 5401, Role: "primary"},
			{ID: 4, Group: 1, Host: "s2", Port: 5500, Role: "standby"},
			{ID: 5, Group: 1, Host: "s3", Port: 5501, Role: "standby"},
			{ID: 2, Group: 2, Host: "s3", Port: 5402, Role: "primary"},
		},
		standbys: []StandbyRec{
			{ID: 10, Group: 1, Server: "s2", InstanceID: 101, Port: 5500, State: "ready", CatalogNode: 4},
			{ID: 11, Group: 1, Server: "s3", InstanceID: 101, Port: 5501, State: "ready", CatalogNode: 5},
		},
		pos: map[string]Position{
			"s2": {Server: "s2", Group: 1, InRecovery: true, Timeline: 1, ReceiveLsn: "0/5000000", ReplayLsn: "0/5000000", SystemIdentifier: "9"},
			"s3": {Server: "s3", Group: 1, InRecovery: true, Timeline: 1, ReceiveLsn: "0/4000000", ReplayLsn: "0/4000000", SystemIdentifier: "9"},
		},
		posErr:         map[string]error{},
		facts:          GroupFacts{SyncStreaming: true, LastKnownAge: time.Second, PrimaryPlanned: 1},
		live:           map[string]string{"s1": "dead", "s2": "live", "s3": "live"},
		lease:          map[string]bool{},
		promoted:       map[string]bool{},
		promoteErr:     map[string]error{},
		promoteN:       map[string]int{},
		settings:       map[string]LeaseSettings{"s1": {10000, 3000}, "s2": {10000, 3000}, "s3": {10000, 3000}},
		settingsErr:    map[string]error{},
		catalogFollows: true,
	}
	w.rows = newMemRows()
	return w
}

func (w *world) rec(format string, a ...any) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.log = append(w.log, fmt.Sprintf(format, a...))
}

func (w *world) index(prefix string) int {
	w.mu.Lock()
	defer w.mu.Unlock()
	for i, l := range w.log {
		if len(l) >= len(prefix) && l[:len(prefix)] == prefix {
			return i
		}
	}
	return -1
}

func (w *world) count(prefix string) int {
	w.mu.Lock()
	defer w.mu.Unlock()
	n := 0
	for _, l := range w.log {
		if len(l) >= len(prefix) && l[:len(prefix)] == prefix {
			n++
		}
	}
	return n
}

// ---- Peers

type fpeers struct{ w *world }

func (p fpeers) unreach(server string) error {
	if p.w.stateDown[server] {
		return ErrUnreachable
	}
	if server == p.w.oldSrv && !p.w.oldAlive {
		return ErrUnreachable
	}
	return nil
}

func (p fpeers) Position(_ context.Context, server string, g int32) (Position, error) {
	p.w.rec("position %s", server)
	if err := p.w.posErr[server]; err != nil {
		return Position{}, err
	}
	pos := p.w.pos[server]
	if p.w.promoted[server] {
		pos.InRecovery = false
	}
	return pos, nil
}

func (p fpeers) State(_ context.Context, server string, g int32) (State, error) {
	if err := p.unreach(server); err != nil {
		return State{}, err
	}
	p.w.rec("state %s", server)
	p.w.mu.Lock()
	if p.w.hardPending != "" && server == p.w.hardPending {
		if p.w.hardLate--; p.w.hardLate <= 0 {
			p.w.promoted[server], p.w.lease[server], p.w.hardPending = true, true, ""
		}
	}
	p.w.mu.Unlock()
	st := State{Server: server, Group: g, Holds: p.w.lease[server], Frozen: p.w.frozen && !(p.w.renewUnfrozen && p.w.count("lease-fence") > 0)}
	if server == p.w.oldSrv {
		f := false
		st.Instances = []StateInstance{{ID: 1, Group: g, Running: p.w.oldWritable, InRecovery: &f, Role: "primary"}}
		if !p.w.oldWritable {
			st.Instances[0].Running = false
		}
		st.Holds = p.w.oldHolds && p.w.oldWritable
		pn := p.w.oldPrepared
		if !p.w.oldPreparedUnknown {
			st.Prepared = &pn
		}
		st.Quiesced = p.w.quiesced
		if p.w.oldWritable {
			yes := !p.w.noWatchdog
			st.Watchdog = &yes
		}
	} else if p.w.promoted[server] {
		f := false
		st.Instances = []StateInstance{{ID: 101, Group: g, Running: true, InRecovery: &f, Role: "primary"}}
		st.Attached = true
		z := p.w.prepared
		st.Prepared = &z
	} else {
		t := true
		st.Instances = []StateInstance{{ID: 101, Group: g, Running: true, InRecovery: &t, Role: "standby"}}
	}
	if p.w.lease[server] {
		st.Adopted = "holder"
	}
	return st, nil
}

func (p fpeers) LeaseSettings(_ context.Context, server string) (LeaseSettings, error) {
	if err := p.w.settingsErr[server]; err != nil {
		return LeaseSettings{}, err
	}
	if err := p.unreach(server); err != nil {
		return LeaseSettings{}, err
	}
	return p.w.settings[server], nil
}

func (p fpeers) Freeze(_ context.Context, server string, g int32, ttl time.Duration) error {
	if err := p.unreach(server); err != nil {
		return err
	}
	if p.w.renewFreezeErr != nil && p.w.count("lease-fence") > 0 {
		p.w.rec("freeze-error %s", server)
		return p.w.renewFreezeErr
	}
	p.w.rec("freeze %s", server)
	p.w.frozen = true
	return nil
}

func (p fpeers) Unfreeze(_ context.Context, server string, g int32) error {
	if err := p.unreach(server); err != nil {
		return err
	}
	p.w.rec("unfreeze %s", server)
	p.w.frozen = false
	return nil
}

func (p fpeers) StandbysGone(_ context.Context, server, gone string, ttl time.Duration) error {
	p.w.rec("gone %s %s", server, gone)
	return nil
}

func (p fpeers) Demote(_ context.Context, server string, r DemoteRequest) (DemoteAnswer, error) {
	if err := p.unreach(server); err != nil {
		return DemoteAnswer{}, err
	}
	if p.w.oldRefusesDem != nil {
		return DemoteAnswer{}, p.w.oldRefusesDem
	}
	if r.Mode == "fast" {
		if p.w.onDemote != nil {
			p.w.onDemote()
		}
		if r.ProofOnly && p.w.proofNotStopped {
			return DemoteAnswer{Stopped: false, ProofOnly: true}, nil
		}
		p.w.mu.Lock()
		p.w.stopTerms = append(p.w.stopTerms, r.ExpectedTerm)
		p.w.stopCandidates = append(p.w.stopCandidates, r.Candidate)
		p.w.stopProofOnly = append(p.w.stopProofOnly, r.ProofOnly)
		p.w.mu.Unlock()
		if p.w.stopErr != nil {
			return DemoteAnswer{}, p.w.stopErr
		}
		if p.w.beforeStop != nil {
			p.w.beforeStop()
		}
		p.w.rec("demote-fast %s", server)
		p.w.oldWritable = false
		state := p.w.stopState
		if state == "" {
			state = "shut down"
		}
		p.w.mu.Lock()
		if n := p.w.stopN; len(p.w.stopStates) > 0 {
			if n >= len(p.w.stopStates) {
				n = len(p.w.stopStates) - 1
			}
			state = p.w.stopStates[n]
		}
		p.w.stopN++
		p.w.mu.Unlock()
		if p.w.dbDownAfterStop && p.w.rows != nil {
			p.w.rows.mu.Lock()
			p.w.rows.down = true
			p.w.rows.mu.Unlock()
		}
		if p.w.afterStop != nil {
			p.w.afterStop()
		}
		return DemoteAnswer{Stopped: true, ClusterState: state, Checkpoint: "0/6000028", Released: !p.w.notReleased, ProofOnly: r.ProofOnly && !p.w.oldCalleeIgnoresProofOnly}, nil
	}
	p.w.mu.Lock()
	p.w.immediate = append(p.w.immediate, r)
	p.w.mu.Unlock()
	p.w.rec("demote %s to=%s", server, r.TransferTo)
	p.w.oldWritable = false
	if n := len(p.w.mapStarts); r.Group == 0 && p.w.mapGroup && n > 0 && p.w.mapStarts[n-1].Planned != nil && p.w.mapStarts[n-1].Planned.Hard && !p.w.noPromoteOnProof {
		// the candidate of a hard failover of the map group waited for the lease, promotes and holds it
		p.w.mu.Lock()
		if p.w.hardLate > 0 {
			p.w.hardPending = r.TransferTo
		} else {
			p.w.promoted[r.TransferTo], p.w.lease[r.TransferTo] = true, true
		}
		p.w.mu.Unlock()
	}
	if p.w.dbDownAfterStop && p.w.rows != nil {
		p.w.rows.mu.Lock()
		p.w.rows.down = true
		p.w.rows.mu.Unlock()
	}
	if p.w.afterStop != nil {
		p.w.afterStop()
	}
	if r.TransferTo != "" {
		p.w.lease[r.TransferTo] = true
		p.w.leaseHeldByOld = false
	}
	return DemoteAnswer{Stopped: true, Transferred: r.TransferTo != ""}, nil
}

func (p fpeers) Lease(_ context.Context, server string, r LeaseRequest) (LeaseAnswer, error) {
	switch {
	case r.Role == LeaseRelease:
		p.w.rec("lease-release %s", server)
		return LeaseAnswer{}, nil
	case r.TransferTo != "":
		p.w.rec("lease-transfer %s to %s", server, r.TransferTo)
		if !p.w.lease[server] {
			return LeaseAnswer{Transferred: false}, nil
		}
		p.w.lease[server] = false
		p.w.lease[r.TransferTo] = true
		return LeaseAnswer{Transferred: true, Holder: r.TransferTo}, nil
	}
	p.w.rec("lease-%s %s", r.Role, server)
	if p.w.leaseErr != nil {
		return LeaseAnswer{}, p.w.leaseErr
	}
	if p.w.leaseHeldByOld {
		return LeaseAnswer{Held: false, WaitMs: 9000, RefusedBy: map[string]string{"s2": "s1", "s3": "s1"}}, nil
	}
	p.w.lease[server] = true
	return LeaseAnswer{Held: true}, nil
}

func (p fpeers) Promote(_ context.Context, server string, r PromoteRequest) (PromoteAnswer, error) {
	p.w.mu.Lock()
	p.w.promoteReqs = append(p.w.promoteReqs, r)
	p.w.mu.Unlock()
	if p.w.promoteRefuse != nil && r.Checkpoint != "" {
		p.w.rec("promote-refused-proof %s", server)
		return PromoteAnswer{}, p.w.promoteRefuse
	}
	p.w.mu.Lock()
	n := p.w.promoteN[server]
	p.w.promoteN[server]++
	p.w.mu.Unlock()
	if err := p.w.promoteErr[fmt.Sprintf("%s#%d", server, n)]; err != nil {
		p.w.rec("promote-error %s", server)
		return PromoteAnswer{}, err
	}
	lostAnswer := p.w.promoteErr[fmt.Sprintf("%s#%d:after", server, n)]
	if lostAnswer != nil && p.w.lease[server] && !p.w.promoted[server] {
		// The callee did the work and its answer was lost.
		p.w.rec("promote %s", server)
		p.w.promoted[server] = true
		return PromoteAnswer{}, lostAnswer
	}
	if r.ReapplyList {
		p.w.rec("promote-reapply %s", server)
		return PromoteAnswer{Status: "already"}, nil
	}
	if !p.w.lease[server] {
		p.w.rec("promote-refused-lease %s", server)
		return PromoteAnswer{}, &CallError{Status: 409, Code: "lease_not_held"}
	}
	if p.w.promoted[server] {
		p.w.rec("promote-already %s", server)
		return PromoteAnswer{Status: "already", Timeline: 2}, nil
	}
	p.w.rec("promote %s", server)
	p.w.promoted[server] = true
	return PromoteAnswer{Status: "promoted", Timeline: 2, Lsn: "0/5000100"}, nil
}

func (p fpeers) RecoverPrepared(_ context.Context, server string, g int32, db string) error {
	p.w.rec("recover-prepared-on %s", server)
	return nil
}

func (p fpeers) Repoint(_ context.Context, server string, r RepointRequest) (RepointAnswer, error) {
	p.w.rec("repoint %s to %s:%d", server, r.PrimaryHost, r.PrimaryPort)
	return RepointAnswer{Streaming: true, State: "streaming"}, nil
}

// ---- Catalog

type fcat struct{ w *world }

func (c fcat) Databases(context.Context) ([]string, error) {
	if c.w.shop != nil {
		return []string{SystemDB, "shop"}, nil
	}
	return []string{SystemDB}, nil
}

func (c fcat) list(db string) *[]Node {
	if db == "shop" {
		return &c.w.shop
	}
	return &c.w.nodes
}

func (c fcat) Nodes(_ context.Context, db string) ([]Node, error) {
	c.w.mu.Lock()
	defer c.w.mu.Unlock()
	return append([]Node(nil), (*c.list(db))...), nil
}
func (c fcat) Status(_ context.Context, db string) (int64, bool, error) {
	return c.w.term, false, nil
}
func (c fcat) RecoverPrepared(_ context.Context, db string) error {
	c.w.rec("recover-prepared %s", db)
	return nil
}
func (c fcat) CheckCluster(_ context.Context, db string) ([]Incident, error) {
	return nil, nil
}
func (c fcat) PromoteNode(_ context.Context, db string, id int32) (PromoteResult, error) {
	c.w.rec("promote_node %s %d", db, id)
	c.w.mu.Lock()
	defer c.w.mu.Unlock()
	i := c.w.catCalls
	c.w.catCalls++
	if i < len(c.w.catScript) {
		switch a := c.w.catScript[i].(type) {
		case PromoteResult:
			return a, nil
		case error:
			return PromoteResult{}, a
		}
	}
	if c.w.catalogFollows && db != c.w.noFollow {
		nodes := *c.list(db)
		var oldID int32
		for k := range nodes {
			if nodes[k].Group == c.w.group() && nodes[k].Role == "primary" {
				oldID = nodes[k].ID
			}
		}
		for k := range nodes {
			switch nodes[k].ID {
			case id:
				nodes[k].Role = "primary"
			case oldID:
				nodes[k].Role = "standby"
			}
		}
		if db == SystemDB {
			c.w.term++
		}
	}
	return PromoteResult{Status: "already", Term: c.w.term, Final: true, Unreached: []int32{1}}, nil
}

// ---- Planner

type fplan struct{ w *world }

func (p fplan) Self() string { return p.w.self }
func (p fplan) Members(context.Context) ([]string, error) {
	return []string{"s1", "s2", "s3"}, nil
}
func (p fplan) Live(_ context.Context, s string) string {
	if p.w.liveFn != nil {
		return p.w.liveFn(s)
	}
	return p.w.live[s]
}
func (p fplan) Standbys(_ context.Context, g int32) ([]StandbyRec, error) {
	var out []StandbyRec
	for _, s := range p.w.standbys {
		if s.Group == g {
			out = append(out, s)
		}
	}
	return out, nil
}
func (p fplan) Facts(context.Context, int32) (GroupFacts, error) { return p.w.facts, nil }

func (p fplan) LiveReplication(context.Context) (LiveReplication, error) {
	p.w.mu.Lock()
	defer p.w.mu.Unlock()
	p.w.liveReads++
	if p.w.liveErr != nil {
		return LiveReplication{}, p.w.liveErr
	}
	if p.w.live2 != nil {
		return *p.w.live2, nil
	}
	return LiveReplication{SyncStreaming: p.w.facts.SyncStreaming, FlushLsn: "0/5000000", Streaming: 1}, nil
}
func (p fplan) PrimaryNames(context.Context, string, int32) ([]int64, bool) {
	if p.w.oldAlive && p.w.primaryNames != nil {
		return p.w.primaryNames, true
	}
	return nil, false
}
func (p fplan) Preferred(_ context.Context, g int32, s string) bool { return s == "s3" }
func (p fplan) Ring(from, to string) int                            { return 0 }
func (p fplan) AfterPromotion(_ context.Context, g int32, s StandbyRec, srv string, port int, old OldPrimaryRec) error {
	p.w.rec("after-promotion %d %s:%d", s.ID, srv, port)
	p.w.records++
	var keep []StandbyRec
	for _, x := range p.w.standbys {
		if x.ID != s.ID {
			keep = append(keep, x)
		}
	}
	p.w.standbys = keep
	return nil
}
func (p fplan) FlagRebuild(_ context.Context, id int64) error {
	p.w.flagged = append(p.w.flagged, id)
	return nil
}
func (p fplan) RebalanceRunning(context.Context) (bool, error) { return p.w.rebalance, nil }
func (p fplan) SyncNamesOf(context.Context, string, int32) (string, int, bool) {
	return "ANY 1 (kingres_standby_10)", 1, true
}
func (p fplan) Alert(_ context.Context, kind, sev, subject, msg string, raise bool) {
	p.w.mu.Lock()
	defer p.w.mu.Unlock()
	if raise {
		p.w.alerts = append(p.w.alerts, kind)
	}
}
func (p fplan) Audit(_ context.Context, action string, g int32, d map[string]any) {
	p.w.rec("audit %s", action)
}

// ---- Rows

type memRows struct {
	mu      sync.Mutex
	run     RunRow
	have    bool
	groups  map[int32]*GroupRow
	events  []string
	outcome string
	lost    bool
	down    bool // the system database is the stopped instance: nothing can be read or written (the map group after its clean stop)
}

func newMemRows() *memRows { return &memRows{groups: map[int32]*GroupRow{}} }

func (m *memRows) check() error {
	if m.down {
		return errors.New("the system database is stopped")
	}
	if m.lost {
		return ErrLeaseLost
	}
	return nil
}

func (m *memRows) Begin(_ context.Context, in Input, term int64, groups []GroupRow) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.check(); err != nil {
		return err
	}
	if !m.have {
		m.run, m.have = RunRow{TaskID: 1, ExpectedTerm: term}, true
	}
	for _, g := range groups {
		g := g
		if _, ok := m.groups[g.Group]; !ok {
			m.groups[g.Group] = &g
		}
	}
	return nil
}
func (m *memRows) Run(context.Context) (RunRow, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.down {
		return RunRow{}, false, errors.New("the system database is stopped")
	}
	return m.run, m.have, nil
}
func (m *memRows) Group(_ context.Context, g int32) (GroupRow, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.down {
		return GroupRow{}, false, errors.New("the system database is stopped")
	}
	r, ok := m.groups[g]
	if !ok {
		return GroupRow{}, false, nil
	}
	return *r, true, nil
}
func (m *memRows) Set(_ context.Context, g int32, u GroupSet) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.check(); err != nil {
		return err
	}
	r := m.groups[g]
	if u.State != nil {
		r.State = *u.State
	}
	if u.NewServer != nil {
		r.NewServer = *u.NewServer
	}
	if u.NewNode != nil {
		r.NewNode = *u.NewNode
	}
	if u.ChosenStandby != nil {
		r.ChosenStandby = *u.ChosenStandby
	}
	if u.TermAfter != nil {
		r.TermAfter = u.TermAfter
	}
	if u.Unreached != nil {
		r.Unreached = u.Unreached
	}
	if u.ClearPin {
		r.ChosenStandby = 0
	}
	if u.LeaseWaitMs != nil {
		r.LeaseWaitMs = u.LeaseWaitMs
	}
	if u.Incidents != nil {
		r.Incidents = u.Incidents
	}
	if u.DataLoss != nil {
		r.DataLoss = u.DataLoss
	}
	if u.Error != nil {
		r.Error = *u.Error
	}
	return nil
}
func (m *memRows) Pin(_ context.Context, g int32, s StandbyRec, d, l []byte) (StandbyRec, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.check(); err != nil {
		return s, false, err
	}
	r := m.groups[g]
	if r.ChosenStandby != 0 {
		return StandbyRec{ID: r.ChosenStandby, Group: g, Server: r.NewServer, CatalogNode: r.NewNode, InstanceID: s.InstanceID}, false, nil
	}
	r.ChosenStandby, r.NewServer, r.NewNode = s.ID, s.Server, s.CatalogNode
	if l != nil {
		r.DataLoss = l
	}
	return s, true, nil
}
func (m *memRows) Event(_ context.Context, g *int32, step, kind string, d map[string]any) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.events = append(m.events, step+"/"+kind)
	return m.check()
}
func (m *memRows) Finish(_ context.Context, outcome, text string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.check(); err != nil {
		return err
	}
	m.outcome = outcome
	return nil
}
func (m *memRows) Abandon(_ context.Context, outcome, text string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.outcome = outcome
	return nil
}

// ---- driving the steps

func (w *world) runner() *Runner {
	return &Runner{
		Peers: fpeers{w}, Cat: fcat{w}, Plan: fplan{w},
		NewRows:   func(int64) Rows { return w.rows },
		Sleep:     func(ctx context.Context, d time.Duration) error { return ctx.Err() },
		LeaseWait: 3 * time.Second, LeasePoll: 10 * time.Millisecond, ReadBound: 3 * time.Second,
		PromoteNode: PromoteOptions{Pause: time.Millisecond},
		SyncWait:    time.Millisecond, PreparedWait: time.Millisecond, ClientsWait: time.Millisecond,
	}
}

// clockSleep makes Sleep advance a fake clock so that the bounds end.
func (w *world) fakeClock(r *Runner) {
	now := time.Unix(1_000_000, 0)
	r.Now = func() time.Time { return now }
	r.Sleep = func(ctx context.Context, d time.Duration) error { now = now.Add(d); return ctx.Err() }
}

func hardInput() Input {
	return Input{Mode: ModeHard, ServerID: "s1", Groups: []int32{1}, Trigger: "manual", RequestedBy: "tester"}
}

// runAll runs every step of the task once, in order. It returns the error of the
// first step that fails and its name.
func runAll(t *testing.T, r *Runner, in Input) (string, error) {
	t.Helper()
	raw, _ := json.Marshal(in)
	steps, err := r.steps(raw)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range steps {
		env := &task.Env{Task: task.Task{ID: 1, Input: raw}}
		err := s.Run(context.Background(), env)
		var sk *task.SkipError
		if errors.As(err, &sk) {
			continue
		}
		if err != nil {
			return s.Name, err
		}
	}
	return "", nil
}

func (m *memRows) Warned(context.Context) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, e := range m.events {
		for _, k := range WarningKinds {
			if strings.HasSuffix(e, "/"+k) {
				return true, nil
			}
		}
	}
	for _, g := range m.groups {
		if len(g.Incidents) > 0 {
			return true, nil
		}
	}
	return false, nil
}

func (p fpeers) Quiesce(_ context.Context, server string, r QuiesceRequest) (QuiesceAnswer, error) {
	p.w.mu.Lock()
	p.w.undoTerms = append(p.w.undoTerms, r.ExpectedTerm)
	p.w.mu.Unlock()
	if err := p.unreach(server); err != nil {
		return QuiesceAnswer{}, err
	}
	p.w.mu.Lock()
	defer p.w.mu.Unlock()
	p.w.log = append(p.w.log, "quiesce "+server)
	if p.w.quiesceFail {
		return QuiesceAnswer{Remaining: 3}, nil
	}
	if p.w.quiesceCalls > 0 {
		p.w.quiesceCalls--
		return QuiesceAnswer{Remaining: p.w.quiesceCalls + 1}, nil
	}
	p.w.quiesced = true
	return QuiesceAnswer{Quiesced: true}, nil
}

func (p fpeers) Unquiesce(_ context.Context, server string, r QuiesceRequest) error {
	p.w.mu.Lock()
	p.w.undoTerms = append(p.w.undoTerms, r.ExpectedTerm)
	stale := r.ExpectedTerm >= 0 && r.ExpectedTerm < p.w.term
	if stale {
		p.w.unquiesceStale++
	}
	p.w.mu.Unlock()
	if err := p.unreach(server); err != nil {
		return err
	}
	if stale {
		return &CallError{Status: 409, Code: "stale_term", Message: "The catalog has a newer term than the failover knows."}
	}
	p.w.rec("unquiesce %s", server)
	p.w.mu.Lock()
	p.w.quiesced = false
	p.w.mu.Unlock()
	return nil
}

func (p fpeers) Mark(_ context.Context, server string, r MarkRequest) (MarkAnswer, error) {
	if err := p.unreach(server); err != nil {
		return MarkAnswer{}, err
	}
	p.w.mu.Lock()
	p.w.markTerms = append(p.w.markTerms, r.ExpectedTerm)
	p.w.mu.Unlock()
	if p.w.markErr != nil {
		return MarkAnswer{}, p.w.markErr
	}
	p.w.rec("mark %s", server)
	return MarkAnswer{Mark: "0/6000000"}, nil
}

func (p fpeers) Verify(_ context.Context, server string, r VerifyRequest) (VerifyAnswer, error) {
	if r.Checkpoint == "" {
		p.w.rec("replayed? %s", server)
		return VerifyAnswer{Ok: p.w.markReplayed}, nil
	}
	p.w.mu.Lock()
	n := p.w.verifyN
	p.w.verifyN++
	answers, err := p.w.verifyAnswers, p.w.verifyErr
	p.w.mu.Unlock()
	p.w.rec("verify %s", server)
	if err != nil {
		return VerifyAnswer{}, err
	}
	if len(answers) == 0 {
		return VerifyAnswer{Ok: true}, nil
	}
	if n >= len(answers) {
		n = len(answers) - 1
	}
	return VerifyAnswer{Ok: answers[n]}, nil
}

func (p fpeers) Resume(ctx context.Context, server string, r ResumeRequest) error {
	if err := ctx.Err(); err != nil {
		return err // a call made on a cancelled context does not reach the server
	}
	p.w.mu.Lock()
	p.w.undoTerms = append(p.w.undoTerms, r.ExpectedTerm)
	p.w.mu.Unlock()
	if err := p.unreach(server); err != nil {
		return err
	}
	if p.w.onResume != nil {
		p.w.onResume()
	}
	p.w.rec("resume %s", server)
	p.w.mu.Lock()
	p.w.oldWritable = true
	p.w.mu.Unlock()
	return nil
}

// The calls of the planned switchover of the map group (the fake keeps what it was asked).
func (p fpeers) MapStart(_ context.Context, server string, r MapStartRequest) error {
	p.w.mu.Lock()
	p.w.mapStarts = append(p.w.mapStarts, r)
	p.w.mu.Unlock()
	if err := p.unreach(server); err != nil {
		return err
	}
	p.w.rec("map-start %s", server)
	if p.w.onMapStart != nil {
		p.w.onMapStart()
	}
	return p.w.mapStartErr
}

func (p fpeers) MapProof(_ context.Context, server string, r MapProofRequest) error {
	p.w.mu.Lock()
	p.w.mapProofs = append(p.w.mapProofs, r)
	if p.w.mapRefusal == "" && !p.w.noPromoteOnProof {
		// the candidate promotes on the proof, and holds the lease
		p.w.promoted[server], p.w.lease[server] = true, true
	}
	p.w.mu.Unlock()
	p.w.rec("map-proof %s", server)
	return p.w.mapProofErr
}

func (p fpeers) MapAbandon(ctx context.Context, server, reason string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	p.w.rec("map-abandon %s", server)
	return p.w.mapAbandonErr
}

// group is the group that the world's fake catalog moves: 1, or 0 for a world of the map group.
func (w *world) group() int32 {
	if w.mapGroup {
		return 0
	}
	return 1
}

func (p fpeers) MapRefusal(_ context.Context, _ int64) (string, bool) {
	p.w.mu.Lock()
	defer p.w.mu.Unlock()
	return p.w.mapRefusal, p.w.mapRefusal != ""
}
