// SPDX-License-Identifier: AGPL-3.0-only

package standby

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/audit"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/monitor"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/peerclient"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/sysdb"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/task"
)

// KindReconcile is the kind of the task that brings the standbys of the cluster in
// line with the cluster setting replicaCount, group by group. Its subject is
// ("replication", "standbys"), so there is at most one at a time.
const KindReconcile = "standby.reconcile"

// Subject of the reconcile task.
const (
	SubjectType = "replication"
	SubjectID   = "standbys"
)

// Leader is the manager leader's side: it decides, from the catalog and the
// setting replicaCount, which standbys the cluster should have, runs the task that
// creates, removes and rebuilds them, keeps every server's copy of the list
// current, and raises groupUnderReplicated.
type Leader struct {
	Self string
	Sys  SysFunc
	// Agent answers the calls to this server itself.
	Agent *Agent
	// Call is the internal call to another server.
	Call func(ctx context.Context, server, method, path string, in, out any) error
	// StartTask submits a task and runs it in the background.
	StartTask func(ctx context.Context, kind, subjType, subjID string, input any, actor string) (task.Task, error)
	// IsLeader reports whether this server is the manager leader now.
	IsLeader func(ctx context.Context) bool
	// ServiceConn opens a service connection to a database of this server.
	ServiceConn func(ctx context.Context, db string) (*pgx.Conn, error)
	// Registrar records standbys in the sharding catalog (kingres.add_node and
	// remove_node of kingres_shard 0.5; nil in a unit test that has no catalog: the
	// step then ends as skipped).
	Registrar CatalogRegistrar
	Audit     func(ctx context.Context, ev audit.Event)
	Log       *slog.Logger
	// Interval is the period of the check (default 10 s).
	Interval time.Duration
	// RetryAfter is how long a failed create or rebuild waits before the next try
	// (default 30 s).
	RetryAfter time.Duration
	// Busy returns the groups that a failover has taken over: the reconcile loop does not create, remove,
	// rebuild or register a standby of such a group (design 3.3 F1: a group in a failover task is the task's). nil: none.
	Busy func(ctx context.Context) map[int32]bool
	// UnregisterWait is the pause between two tries of a catalog removal that the
	// extension refused with KS009 because the primary still names the standby (default 5 s).
	UnregisterWait time.Duration
	// ReplaceAfter is how long the record of an old primary whose server does not answer counts as a planned standby
	// (setting standby-replace-after, default 30 minutes): after it the record is removed, in the order of extension
	// sequence 8, and the ordinary reconcile places a replacement. The alert oldPrimaryNotRebuilt comes at the shorter
	// of ten minutes and half of it.
	ReplaceAfter time.Duration
	// Dead says that the leader's liveness view calls a server dead (nil: none is).
	Dead func(server string) bool
	// Excluded names the servers that are taken out of the ring: a server that is being drained keeps no standby and gets none
	// (design shard-phase10.md 11.3 step 5). The reconcile then places the standbys it hosted on the servers that remain and removes
	// the old ones. nil: none.
	Excluded func(ctx context.Context) map[string]bool

	st   store
	kick chan struct{}

	roundMu  sync.Mutex
	mu       sync.Mutex
	lastTry  map[int64]time.Time
	underFor map[int32]int
	// alertedOld are the servers with a raised oldPrimaryNotRebuilt (so that the alert is cleared when the cause is gone).
	alertedOld map[string]bool
	// early are the standbys whose entry goes out as Ready before the record is ready (see markReady).
	early map[int64]bool
	// planMu makes the reading of the records and the overlay of early one step against markReady (which writes the record
	// and drops the entry from early under it): a list can never carry a record from before a change together with an
	// overlay from after it (phase 5 review M-B).
	planMu sync.Mutex
	// afterList is called in buildPlan between the reading of the records and the overlay (tests only).
	afterList func()
}

// NewLeader prepares the leader's side; Run starts it.
func NewLeader(l *Leader) *Leader {
	l.st = store{sys: l.Sys}
	l.kick = make(chan struct{}, 1)
	l.lastTry = map[int64]time.Time{}
	l.underFor = map[int32]int{}
	l.early = map[int64]bool{}
	if l.Interval == 0 {
		l.Interval = 10 * time.Second
	}
	if l.RetryAfter == 0 {
		l.RetryAfter = 30 * time.Second
	}
	if l.ReplaceAfter == 0 {
		l.ReplaceAfter = 30 * time.Minute
	}
	if l.UnregisterWait == 0 {
		l.UnregisterWait = 5 * time.Second
	}
	if l.Log == nil {
		l.Log = slog.New(slog.DiscardHandler)
	}
	return l
}

// Kick makes the next check run now (the setting replicaCount changed).
func (l *Leader) Kick() {
	select {
	case l.kick <- struct{}{}:
	default:
	}
}

// Definitions returns the task definition of this package.
func (l *Leader) Definitions() []task.Definition {
	return []task.Definition{{Kind: KindReconcile, Dynamic: l.steps}}
}

// topology is what the catalog says about the groups and the servers.
type topology struct {
	ring   []Server
	groups []groupInfo
}

type groupInfo struct {
	ID      int32
	Primary string
	Port    int
}

func (t topology) group(id int32) (groupInfo, bool) {
	for _, g := range t.groups {
		if g.ID == id {
			return g, true
		}
	}
	return groupInfo{}, false
}

func (t topology) servers() []string {
	out := make([]string, len(t.ring))
	for i, s := range t.ring {
		out[i] = s.ID
	}
	return out
}

// readTopology reads kingres.node of the leader's copy of the catalog. The ring is
// the servers in the order of their groups (the order they joined in); a server
// that is not active is no candidate.
func (l *Leader) readTopology(ctx context.Context) (topology, error) {
	c, err := l.ServiceConn(ctx, sysdb.Database)
	if err != nil {
		return topology{}, err
	}
	defer func() { _ = c.Close(context.WithoutCancel(ctx)) }()
	rows, err := c.Query(ctx, `SELECT n.group_id, n.host, n.port, n.role, n.is_active, coalesce(n.server_group, '') FROM kingres.node n
		LEFT JOIN kingres_cp.server s ON s.server_id = n.host
		WHERE coalesce(s.status, 'active') = 'active' ORDER BY n.group_id, n.node_id`)
	if err != nil {
		return topology{}, fmt.Errorf("standby: reading kingres.node: %w", err)
	}
	defer rows.Close()
	var t topology
	seen := map[string]int{}
	for rows.Next() {
		var g int32
		var host, role, zone string
		var port int
		var active bool
		if err := rows.Scan(&g, &host, &port, &role, &active, &zone); err != nil {
			return topology{}, err
		}
		// A server is in the ring whatever its nodes are: after a failover a server can run only standbys (its group
		// moved to another server) and is still a place for the standbys of other groups, and the record of the old
		// primary must find it a valid candidate (design 6.5). The zone of the first row that has one counts.
		if i, ok := seen[host]; !ok {
			seen[host] = len(t.ring)
			t.ring = append(t.ring, Server{ID: host, Zone: zone})
		} else if t.ring[i].Zone == "" && zone != "" {
			t.ring[i].Zone = zone
		}
		if role != "primary" || !active {
			continue
		}
		t.groups = append(t.groups, groupInfo{ID: g, Primary: host, Port: port})
	}
	if err := rows.Err(); err != nil {
		return t, err
	}
	if l.Excluded != nil {
		t.ring = WithoutServers(t.ring, l.Excluded(ctx))
	}
	return t, nil
}

// WithoutServers is the ring without the excluded servers (the order of the rest is kept).
func WithoutServers(ring []Server, excluded map[string]bool) []Server {
	if len(excluded) == 0 {
		return ring
	}
	out := make([]Server, 0, len(ring))
	for _, s := range ring {
		if !excluded[s.ID] {
			out = append(out, s)
		}
	}
	return out
}

func (l *Leader) replicas(ctx context.Context) (int, error) {
	c, err := l.ServiceConn(ctx, sysdb.Database)
	if err != nil {
		return 0, err
	}
	defer func() { _ = c.Close(context.WithoutCancel(ctx)) }()
	var raw []byte
	err = c.QueryRow(ctx, `SELECT value FROM kingres_cp.cluster_setting WHERE key = 'replica.default'`).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("standby: reading the replica count: %w", err)
	}
	var n int
	if err := json.Unmarshal(raw, &n); err != nil || n < 0 || n > 3 {
		return 0, errors.New("standby: the replica count setting is not valid")
	}
	return n, nil
}

// work is what one check found to do.
type work struct {
	Replicas int
	Add      []Entry
	Remove   []Entry
	Resume   []Entry
	Rebuild  []Entry
	// Register are the ready standbys that are not in the catalog yet (a new one, or
	// one made by a version before the registration existed).
	Register []Entry
}

func (w work) empty() bool {
	return len(w.Add)+len(w.Remove)+len(w.Resume)+len(w.Rebuild)+len(w.Register) == 0
}

// diff compares the records with what the catalog and the replica count ask for.
func (l *Leader) diff(t topology, recs []Record, replicas int) work {
	w := work{Replicas: replicas}
	byGroup := map[int32][]Record{}
	for _, r := range recs {
		byGroup[r.Group] = append(byGroup[r.Group], r)
	}
	portOf := func(g int32) int {
		if gi, ok := t.group(g); ok {
			return gi.Port
		}
		return 0
	}
	known := map[int32]bool{}
	for _, g := range t.groups {
		known[g.ID] = true
		cand := Candidates(t.ring, g.Primary)
		var existing []string
		for _, r := range byGroup[g.ID] {
			if r.State == stateRemoving {
				w.Remove = append(w.Remove, r.Entry(g.Port))
				continue
			}
			existing = append(existing, r.Server)
		}
		// A server that is dead is no place for a new standby (the replacement of an old primary that did not return must
		// not be made on it again); a standby that is on it already stays a valid one, a server that merely reboots must
		// not lose it.
		if l.Dead != nil {
			var alive []string
			for _, s := range cand {
				if !l.Dead(s) || slices.Contains(existing, s) {
					alive = append(alive, s)
				}
			}
			cand = alive
		}
		add, remove := Plan(cand, existing, replicas)
		for _, s := range add {
			w.Add = append(w.Add, Entry{Group: g.ID, Server: s, PrimaryServer: g.Primary, PrimaryPort: g.Port})
		}
		rm := map[string]bool{}
		for _, s := range remove {
			rm[s] = true
		}
		for _, r := range byGroup[g.ID] {
			switch {
			case r.State == stateRemoving:
			case rm[r.Server]:
				w.Remove = append(w.Remove, r.Entry(g.Port))
			case r.Diverged && r.State != stateReady:
				// The old primary of a group (or a standby that is ahead of its primary): rebuilt with pg_rewind, never made
				// again as a new standby, whatever its state says.
				w.Rebuild = append(w.Rebuild, r.Entry(g.Port))
			case r.State == stateCreating || r.State == stateRebuilding:
				w.Resume = append(w.Resume, r.Entry(g.Port))
			case r.NeedsRebuild:
				w.Rebuild = append(w.Rebuild, r.Entry(g.Port))
			case r.State == stateReady && r.CatalogNode == nil && r.Port > 0:
				w.Register = append(w.Register, r.Entry(g.Port))
			}
		}
	}
	// A record of a group the catalog does not have any more is removed.
	for g, rs := range byGroup {
		if known[g] {
			continue
		}
		for _, r := range rs {
			w.Remove = append(w.Remove, r.Entry(portOf(g)))
		}
	}
	return w
}

// Run checks every Interval, and when kicked, while this server is the leader.
func (l *Leader) Run(ctx context.Context) {
	for ctx.Err() == nil {
		if l.IsLeader(ctx) {
			l.roundMu.Lock()
			err := l.round(ctx)
			l.roundMu.Unlock()
			if err != nil && ctx.Err() == nil {
				l.Log.Debug("standby: the check of the standbys failed", "error", err)
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(l.Interval):
		case <-l.kick:
		}
	}
}

// ReconcileNow runs one check now, on the caller's request, and returns the
// reconcile task that is unfinished afterwards (the one this check started, or
// one that was running already), or nil when there is none. It answers nil on a
// server that is not the leader.
func (l *Leader) ReconcileNow(ctx context.Context, get func(ctx context.Context, id int64) (task.Task, error)) (*task.Task, error) {
	if !l.IsLeader(ctx) {
		return nil, nil
	}
	l.roundMu.Lock()
	err := l.round(ctx)
	l.roundMu.Unlock()
	if err != nil {
		return nil, err
	}
	var id int64
	err = l.Sys(ctx, func(c *pgx.Conn) error {
		e := c.QueryRow(ctx, `SELECT id FROM kingres_cp.task WHERE kind = $1 AND state IN ('pending', 'running', 'paused') ORDER BY id DESC LIMIT 1`, KindReconcile).Scan(&id)
		if errors.Is(e, pgx.ErrNoRows) {
			id = 0
			return nil
		}
		return e
	})
	if err != nil || id == 0 {
		return nil, err
	}
	t, err := get(ctx, id)
	if err != nil {
		return nil, err
	}
	return &t, nil
}

// round is one check: read the state, start the reconcile task when there is
// work, bring servers that missed a plan up to date, flag standbys whose slot was
// lost, and raise or clear groupUnderReplicated.
func (l *Leader) round(ctx context.Context) error {
	// While a server is being added, removed or leaving, the ring is not final: placing now would put
	// standbys where the finished cluster would not. Nothing is called on other servers either: a
	// server that is still joining answers a call from the leader by asking itself who the leader is,
	// and an answer read before its catalog is complete is cached for a moment, which makes the
	// leader's next call of the join fail with not_leader.
	if l.membershipChanging(ctx) {
		return nil
	}
	t, err := l.readTopology(ctx)
	if err != nil {
		return err
	}
	if len(t.groups) == 0 {
		return nil
	}
	replicas, err := l.replicas(ctx)
	if err != nil {
		return err
	}
	recs, err := l.st.list(ctx)
	if err != nil {
		return err
	}
	statuses := l.poll(ctx, t)
	l.recordReplicationState(ctx, statuses)
	inFailover := map[int32]bool{}
	if l.Busy != nil {
		inFailover = l.Busy(ctx)
	}
	if len(inFailover) > 0 {
		var kept []groupInfo
		for _, g := range t.groups {
			if !inFailover[g.ID] {
				kept = append(kept, g)
			}
		}
		t.groups = kept
		recs = withoutGroups(recs, inFailover)
	}
	for _, r := range recs {
		for _, s := range statuses {
			if ps := s.primaryOf(r.Group); ps != nil {
				for _, id := range ps.NeedsRebuild {
					if id == r.ID && !r.NeedsRebuild && r.State == stateReady {
						if err := l.st.flagRebuild(ctx, r.ID); err == nil {
							l.Log.Warn("standby: a standby lost its slot and will be rebuilt", "standby", r.ID, "group", r.Group)
							r.NeedsRebuild = true
						}
					}
				}
			}
		}
	}
	l.watchOldPrimaries(ctx, recs, statuses)
	recs, err = l.st.list(ctx)
	if err != nil {
		return err
	}
	recs = withoutGroups(recs, inFailover)
	w := l.diff(t, recs, replicas)
	w = l.throttle(w)
	busy := false
	if !w.empty() {
		_, err := l.StartTask(ctx, KindReconcile, SubjectType, SubjectID, w, "system")
		switch {
		case errors.Is(err, task.ErrSubjectBusy):
			busy = true
		case err != nil:
			return fmt.Errorf("starting the standby task: %w", err)
		default:
			busy = true
		}
	}
	l.ensureEverywhere(ctx, t, recs)
	l.catchUp(ctx, t, statuses)
	if !busy {
		l.underReplicated(ctx, t, replicas, recs, statuses)
	}
	return nil
}

// throttle drops the retries of standbys that failed less than RetryAfter ago.
func (l *Leader) throttle(w work) work {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	keep := func(es []Entry) []Entry {
		var out []Entry
		for _, e := range es {
			if e.ID != 0 && now.Sub(l.lastTry[e.ID]) < l.RetryAfter {
				continue
			}
			out = append(out, e)
		}
		return out
	}
	w.Resume, w.Rebuild = keep(w.Resume), keep(w.Rebuild)
	for _, e := range w.Resume {
		l.lastTry[e.ID] = now
	}
	for _, e := range w.Rebuild {
		l.lastTry[e.ID] = now
	}
	return w
}

// poll asks every server for its standby status; a server that does not answer
// is left out.
func (l *Leader) poll(ctx context.Context, t topology) map[string]Status {
	out := map[string]Status{}
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, s := range t.servers() {
		wg.Add(1)
		go func() {
			defer wg.Done()
			cctx, cancel := context.WithTimeout(ctx, 8*time.Second)
			defer cancel()
			var st Status
			if err := l.call(cctx, s, CallStatus, nil, &st); err == nil {
				mu.Lock()
				out[s] = st
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	return out
}

// catchUp pushes the list to the servers that report an older version than the
// current one (a server that was down while a plan was pushed).
func (l *Leader) catchUp(ctx context.Context, t topology, statuses map[string]Status) {
	cur, err := l.st.version(ctx)
	if err != nil || cur == 0 {
		return
	}
	for s, st := range statuses {
		if st.PlanVersion < cur {
			if err := l.pushTo(ctx, s, cur); err != nil {
				l.Log.Debug("standby: bringing a server up to date", "server", s, "error", err)
			}
		}
	}
}

// underReplicated raises groupUnderReplicated for a group whose number of
// streaming standbys is below the replica count, after the condition held for
// three checks in a row (a standby is not counted as missing while it is being
// created); a cluster of one server never raises it (there is nowhere to place
// a standby, and it is the arrangement of a development set-up). The check is
// skipped for a group whose primary server did not answer.
func (l *Leader) underReplicated(ctx context.Context, t topology, replicas int, recs []Record, statuses map[string]Status) {
	if len(t.ring) < 2 {
		return
	}
	var conds []monitor.Condition
	l.mu.Lock()
	for _, g := range t.groups {
		st, ok := statuses[g.Primary]
		ps := st.primaryOf(g.ID)
		if !ok || ps == nil {
			continue
		}
		have := len(ps.Streaming)
		possible := min(replicas, len(t.ring)-1)
		if have >= replicas {
			l.underFor[g.ID] = 0
			continue
		}
		l.underFor[g.ID]++
		if l.underFor[g.ID] < 3 {
			continue
		}
		why := fmt.Sprintf("%d of %d standbys are streaming", have, replicas)
		if possible < replicas {
			why += fmt.Sprintf("; with %d servers at most %d can be placed", len(t.ring), possible)
		}
		conds = append(conds, monitor.Condition{Kind: "groupUnderReplicated", Severity: "warning", Subject: fmt.Sprintf("cluster|group:%d", g.ID),
			Message: fmt.Sprintf("Shard group %d has fewer standbys than the replica count: %s.", g.ID, why)})
	}
	l.mu.Unlock()
	c, err := l.ServiceConn(ctx, sysdb.Database)
	if err != nil {
		return
	}
	defer func() { _ = c.Close(context.WithoutCancel(ctx)) }()
	if err := monitor.ReconcileScope(ctx, c, "cluster", conds, map[string]bool{"groupUnderReplicated": true}, time.Now()); err != nil {
		l.Log.Warn("standby: updating the under-replication alert", "error", err)
	}
}

// call is a call to a server of the cluster; this server's own is answered by its
// agent directly.
func (l *Leader) call(ctx context.Context, server, key string, in, out any) error {
	if server == l.Self {
		var body []byte
		if in != nil {
			var err error
			if body, err = json.Marshal(in); err != nil {
				return err
			}
		}
		res, err := l.Agent.HandleCall(ctx, key, body)
		if err != nil {
			return err
		}
		if out != nil {
			b, err := json.Marshal(res)
			if err != nil {
				return err
			}
			return json.Unmarshal(b, out)
		}
		return nil
	}
	method, path, _ := cutKey(key)
	return l.Call(ctx, server, method, path, in, out)
}

func cutKey(key string) (method, path string, ok bool) {
	for i := 0; i < len(key); i++ {
		if key[i] == ' ' {
			return key[:i], key[i+1:], true
		}
	}
	return "", key, false
}

// buildPlan assembles the list every server is given.
func (l *Leader) buildPlan(ctx context.Context, version int64) (PlanRequest, topology, error) {
	t, err := l.readTopology(ctx)
	if err != nil {
		return PlanRequest{}, t, err
	}
	replicas, err := l.replicas(ctx)
	if err != nil {
		return PlanRequest{}, t, err
	}
	p := PlanRequest{Version: version, Replicas: replicas}
	l.planMu.Lock()
	defer l.planMu.Unlock()
	recs, err := l.st.list(ctx)
	if err != nil {
		return PlanRequest{}, t, err
	}
	if l.afterList != nil {
		l.afterList()
	}
	for _, r := range recs {
		if r.State == stateRemoving {
			continue // a standby on its way out is not named any more
		}
		if g, ok := t.group(r.Group); ok {
			e := r.Entry(g.Port)
			l.mu.Lock()
			if l.early[r.ID] {
				e.Ready = true
			}
			l.mu.Unlock()
			p.Standbys = append(p.Standbys, e)
		}
	}
	return p, t, nil
}

// publish takes a new plan version and sends the list to every server. The
// servers named in must have to take it (the standby that is about to be created
// needs it before its primary can keep a slot for it); the others are best effort:
// a server that missed it is brought up to date by the next check.
func (l *Leader) publish(ctx context.Context, must ...string) error {
	v, err := l.st.bump(ctx)
	if err != nil {
		return err
	}
	p, t, err := l.buildPlan(ctx, v)
	if err != nil {
		return err
	}
	needed := map[string]bool{}
	for _, m := range must {
		needed[m] = true
	}
	servers := append(t.servers(), l.Self)
	sort.Strings(servers)
	var last string
	for i, s := range servers {
		if i > 0 && s == last {
			continue
		}
		last = s
		cctx, cancel := context.WithTimeout(ctx, 20*time.Second)
		err := l.call(cctx, s, CallPlan, p, nil)
		cancel()
		if err != nil {
			if needed[s] {
				return fmt.Errorf("standby: sending the standby list to server %s: %w", s, err)
			}
			l.Log.Debug("standby: a server did not take the standby list", "server", s, "error", err)
		}
	}
	return nil
}

// pushTo sends the current list to one server.
func (l *Leader) pushTo(ctx context.Context, server string, version int64) error {
	p, _, err := l.buildPlan(ctx, version)
	if err != nil {
		return err
	}
	cctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	return l.call(cctx, server, CallPlan, p, nil)
}

func (l *Leader) audit(ctx context.Context, env *task.Env, action string, e Entry, outcome string, extra map[string]any) {
	if l.Audit == nil {
		return
	}
	d := map[string]any{"group": e.Group, "server": e.Server, "taskId": env.Task.ID}
	for k, v := range extra {
		d[k] = v
	}
	l.Audit(ctx, audit.Event{Actor: env.Task.CreatedBy, Source: "system", Action: action, ResourceType: "standby", ResourceID: strconv.FormatInt(e.ID, 10), Outcome: outcome, Details: d})
}

// steps builds the steps of one reconcile task from its input, group by group:
// removals first (they free servers), then creations, then rebuilds, then one
// step that publishes the list and one that registers the standbys in the catalog.
func (l *Leader) steps(input json.RawMessage) ([]task.Step, error) {
	var w work
	if err := json.Unmarshal(input, &w); err != nil {
		return nil, fmt.Errorf("standby: the task input is not valid: %w", err)
	}
	sortEntries := func(es []Entry) []Entry {
		out := append([]Entry(nil), es...)
		sort.SliceStable(out, func(i, j int) bool {
			if out[i].Group != out[j].Group {
				return out[i].Group < out[j].Group
			}
			return out[i].Server < out[j].Server
		})
		return out
	}
	var steps []task.Step
	for _, e := range sortEntries(w.Remove) {
		steps = append(steps, task.Step{Name: fmt.Sprintf("Remove the standby of shard group %d from server %s", e.Group, e.Server),
			Run: func(ctx context.Context, env *task.Env) error { return l.removeStandby(ctx, env, e) }})
	}
	for _, e := range sortEntries(append(append([]Entry(nil), w.Add...), w.Resume...)) {
		steps = append(steps, task.Step{Name: fmt.Sprintf("Create the standby of shard group %d on server %s", e.Group, e.Server),
			Run: func(ctx context.Context, env *task.Env) error { return l.createStandby(ctx, env, e) }})
	}
	for _, e := range sortEntries(w.Rebuild) {
		steps = append(steps, task.Step{Name: fmt.Sprintf("Rebuild the standby of shard group %d on server %s", e.Group, e.Server),
			Run: func(ctx context.Context, env *task.Env) error { return l.rebuildStandby(ctx, env, e) }})
	}
	steps = append(steps,
		task.Step{Name: "Send the list of standbys to every server", Run: func(ctx context.Context, _ *task.Env) error { return l.publish(ctx) }},
		task.Step{Name: CatalogStepName, Run: func(ctx context.Context, env *task.Env) error { return l.registerInCatalog(ctx, env, w) }})
	return steps, nil
}

// createStandby is the step that creates one standby. It is idempotent: the record
// is found or made, the list (with the standby not ready yet) goes to every server
// before anything is created, and the receiving server continues what exists.
func (l *Leader) createStandby(ctx context.Context, env *task.Env, e Entry) error {
	if ok, err := l.stable(ctx, env, e); err != nil || !ok {
		return err
	}
	g, err := l.groupOf(ctx, e.Group)
	if err != nil {
		return err
	}
	rec, err := l.st.ensure(ctx, e.Group, e.Server, g.Primary)
	if err != nil {
		return err
	}
	if rec.State == stateReady && !rec.NeedsRebuild {
		return nil // an earlier run finished it
	}
	_ = env.SetDetail(ctx, fmt.Sprintf("Copying the primary of shard group %d to server %s.", e.Group, e.Server))
	// The primary's server must know the standby (and keep its slot) before the copy starts.
	if err := l.publish(ctx, e.Server, g.Primary); err != nil {
		return err
	}
	entry := rec.Entry(g.Port)
	entry.Ready = false
	var res CreateResponse
	if err := l.call(ctx, e.Server, CallCreate, CreateRequest{Entry: entry}, &res); err != nil {
		l.audit(ctx, env, "standbyCreated", entry, audit.OutcomeFailure, map[string]any{"phase": "create"})
		return fmt.Errorf("creating the standby of shard group %d on %s: %w", e.Group, e.Server, err)
	}
	if gone, err := l.removedMeanwhile(ctx, rec.ID); err != nil {
		return err
	} else if gone {
		// A removal of the server (or of the standby) ran while the copy was made: the copy goes too.
		_ = l.call(ctx, e.Server, CallRemove, RemoveRequest{Entry: entry}, nil)
		return nil
	}
	if err := l.markReady(ctx, rec, g.Primary, res.Port); err != nil {
		return err
	}
	_ = env.SetDetail(ctx, "")
	l.audit(ctx, env, "standbyCreated", entry, audit.OutcomeSuccess, map[string]any{"port": res.Port})
	// The record is ready now: the primary's server must take the final list too, or it would keep an older one that a
	// stale list of another task may have left with the entry not Ready (phase 5 review M-B).
	return l.publish(ctx, g.Primary)
}

// markReady makes the standby of rec ready. The order is the safety rule: a standby a failover may use (a record in the
// state ready, which Preflight needs) must never exist while the primary's server does not know it as ready, because
// that server arms the lease fence of the group only from a ready entry (Agent.HasStandby). So the list with the entry
// as Ready goes to the primary's server first and has to be taken (the call fails otherwise), and only then does the
// record change. The entry stays Ready in every list the leader builds until the record is written, so that a list
// of another task in between does not take it back.
func (l *Leader) markReady(ctx context.Context, rec Record, primary string, port int) error {
	l.mu.Lock()
	l.early[rec.ID] = true
	l.mu.Unlock()
	err := l.publish(ctx, primary)
	// The record is written and the entry released in one step against buildPlan (planMu).
	l.planMu.Lock()
	defer l.planMu.Unlock()
	defer func() {
		l.mu.Lock()
		delete(l.early, rec.ID)
		l.mu.Unlock()
	}()
	if err != nil {
		return err
	}
	return l.st.setState(ctx, rec.ID, stateReady, port)
}

func (l *Leader) removeStandby(ctx context.Context, env *task.Env, e Entry) error {
	rec, found, err := l.st.get(ctx, e.ID)
	if err != nil {
		return err
	}
	if !found {
		return nil
	}
	if err := l.st.markRemoving(ctx, rec.ID); err != nil {
		return err
	}
	// The primary stops naming it and drops its slot; then the standby's own server deletes it.
	if err := l.publish(ctx); err != nil {
		return err
	}
	entry := rec.Entry(e.PrimaryPort)
	if err := l.call(ctx, rec.Server, CallRemove, RemoveRequest{Entry: entry}, nil); err != nil {
		if !errors.Is(err, peerclient.ErrUnreachable) {
			l.audit(ctx, env, "standbyRemoved", entry, audit.OutcomeFailure, nil)
			return fmt.Errorf("removing the standby of shard group %d on %s: %w", e.Group, rec.Server, err)
		}
		// The server is down: its instance is removed when it comes back (its supervision
		// removes standbys that are not in the list), the record goes now.
		_ = env.SetDetail(ctx, fmt.Sprintf("Server %s cannot be reached; its standby is removed when it is back.", rec.Server))
	}
	// The catalog row goes before the record: a record that is gone cannot be found
	// again to finish the removal (a refusal here is repeated by the next check).
	if err := l.unregister(ctx, entry); err != nil {
		return fmt.Errorf("removing the standby from the catalog: %w", err)
	}
	if err := l.st.delete(ctx, rec.ID); err != nil {
		return err
	}
	l.audit(ctx, env, "standbyRemoved", entry, audit.OutcomeSuccess, nil)
	return l.publish(ctx)
}

func (l *Leader) rebuildStandby(ctx context.Context, env *task.Env, e Entry) error {
	if ok, err := l.stable(ctx, env, e); err != nil || !ok {
		return err
	}
	rec, found, err := l.st.get(ctx, e.ID)
	if err != nil || !found {
		return err
	}
	g, err := l.groupOf(ctx, e.Group)
	if err != nil {
		return err
	}
	if rec.State == stateReady {
		if rec, err = l.st.bumpGeneration(ctx, rec.ID); err != nil {
			return err
		}
		if err := l.publish(ctx, rec.Server, g.Primary); err != nil {
			return err
		}
	}
	entry := rec.Entry(g.Port)
	entry.Ready = false
	_ = env.SetDetail(ctx, fmt.Sprintf("Rebuilding the standby of shard group %d on server %s from a new base backup.", e.Group, e.Server))
	var res CreateResponse
	req := RebuildRequest{Entry: entry, Diverged: rec.Diverged}
	if rec.Diverged {
		term, terr := l.catalogTerm(ctx)
		if terr != nil {
			return terr
		}
		req.Term = term
	}
	if err := l.call(ctx, rec.Server, CallRebuild, req, &res); err != nil {
		l.audit(ctx, env, "standbyRebuilt", entry, audit.OutcomeFailure, nil)
		return fmt.Errorf("rebuilding the standby of shard group %d on %s: %w", e.Group, rec.Server, err)
	}
	if rec.CatalogNode != nil && res.Port != 0 && res.Port != rec.Port {
		// The catalog names a node by host and port: a standby that came back on another
		// port is a new node (extension, sequence 6). The old row goes (it is not in the
		// list while it rebuilds), the step that registers makes the new one.
		if err := l.unregister(ctx, rec.Entry(g.Port)); err != nil {
			return fmt.Errorf("removing the old catalog row of a rebuilt standby: %w", err)
		}
		if err := l.st.setCatalogNode(ctx, rec.ID, 0); err != nil {
			return err
		}
	}
	if res.InstanceID > 0 && res.InstanceID != rec.InstanceID {
		// The old primary's directory was found by its server (the failover did not know it).
		if err := l.st.setInstance(ctx, rec.ID, res.InstanceID); err != nil {
			return err
		}
	}
	if res.OldPrimary != nil {
		if b, merr := json.Marshal(res.OldPrimary); merr == nil {
			if err := l.st.recordOldPrimary(ctx, rec.Group, rec.Server, b); err != nil {
				l.Log.Warn("standby: storing the report of the old primary", "standby", rec.ID, "error", err)
			}
		}
	}
	if err := l.markReady(ctx, rec, g.Primary, res.Port); err != nil {
		return err
	}
	if rec.Diverged {
		// The old primary is a standby now: what was raised about it is over.
		l.clearOldPrimaryAlerts(ctx, rec)
		// The failover declared the standbys of this server gone, so that no list waited for them: they are back. Until the
		// declaration lapses (minutes) the primary of the group would not name the standby that was just rebuilt.
		if t, terr := l.readTopology(ctx); terr == nil {
			for _, s := range append(t.servers(), l.Self) {
				cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
				if err := l.call(cctx, s, CallStandbysGone, GoneRequest{Server: rec.Server, Clear: true}, nil); err != nil {
					l.Log.Debug("standby: taking a server back from the gone set", "to", s, "server", rec.Server, "error", err)
				}
				cancel()
			}
		}
	}
	_ = env.SetDetail(ctx, "")
	l.audit(ctx, env, "standbyRebuilt", entry, audit.OutcomeSuccess, nil)
	return l.publish(ctx)
}

// clearOldPrimaryAlerts resolves the alerts about the old primary of a group on a server once it is a standby.
func (l *Leader) clearOldPrimaryAlerts(ctx context.Context, rec Record) {
	subject := fmt.Sprintf("server:%s|group:%d", rec.Server, rec.Group)
	err := l.Sys(ctx, func(c *pgx.Conn) error {
		_, err := c.Exec(ctx, `UPDATE kingres_cp.alert SET cleared_at = now()
			WHERE kind IN ('oldPrimaryRunning', 'oldPrimaryNotStopped', 'oldPrimaryNotRebuilt') AND subject = $1 AND cleared_at IS NULL`, subject)
		return err
	})
	if err != nil {
		l.Log.Warn("standby: clearing the alerts about an old primary", "subject", subject, "error", err)
	}
}

// catalogTerm reads the term of the catalog of the system database: the term a rebuild of an old primary asks under.
func (l *Leader) catalogTerm(ctx context.Context) (int64, error) {
	c, err := l.ServiceConn(ctx, sysdb.Database)
	if err != nil {
		return 0, err
	}
	defer func() { _ = c.Close(context.WithoutCancel(ctx)) }()
	var term int64
	if err := c.QueryRow(ctx, `SELECT term FROM kingres.catalog_status`).Scan(&term); err != nil {
		return 0, fmt.Errorf("standby: reading the catalog term: %w", err)
	}
	return term, nil
}

// watchOldPrimaries looks at the records of old primaries that wait to be rebuilt (diverged, not ready), once per check:
// it notes that the server does not answer (and that it does again), removes the record of a server that has been
// silent for ReplaceAfter (the standby of the group is then made on another server by the ordinary reconcile; the
// order of the removal is the one of sequence 8, in removeStandby), and raises or clears oldPrimaryNotRebuilt
// (design 6.5). The alert comes after the shorter of ten minutes and half of ReplaceAfter, from the creation of the
// record: the server is silent, or it answers and the rebuild does not succeed.
func (l *Leader) watchOldPrimaries(ctx context.Context, recs []Record, statuses map[string]Status) {
	alertAfter := min(10*time.Minute, l.ReplaceAfter/2)
	now := time.Now()
	conds := map[string][]monitor.Condition{} // by server
	for _, r := range recs {
		if !r.Diverged || r.State == stateReady || r.State == stateRemoving {
			continue
		}
		// A server answers when the leader's liveness view does not call it dead (Dead); without that view, when its status was read.
		_, answers := statuses[r.Server]
		if l.Dead != nil {
			answers = !l.Dead(r.Server)
		}
		switch {
		case !answers && r.WaitingSince == nil:
			if err := l.st.setWaiting(ctx, r.ID, true); err != nil {
				l.Log.Debug("standby: noting that a server does not answer", "server", r.Server, "error", err)
			}
		case answers && r.WaitingSince != nil:
			if err := l.st.setWaiting(ctx, r.ID, false); err != nil {
				l.Log.Debug("standby: noting that a server answers again", "server", r.Server, "error", err)
			}
		}
		if !answers && r.WaitingSince != nil && now.Sub(*r.WaitingSince) >= l.ReplaceAfter {
			l.Log.Warn("standby: the server of an old primary has not answered for the replace-after time; its record is removed and a replacement is made elsewhere",
				"standby", r.ID, "group", r.Group, "server", r.Server, "after", l.ReplaceAfter)
			if err := l.st.markRemoving(ctx, r.ID); err != nil {
				l.Log.Warn("standby: removing the record of an old primary that does not return", "standby", r.ID, "error", err)
			}
			continue
		}
		if now.Sub(r.CreatedAt) >= alertAfter {
			why := "The server does not answer."
			if answers {
				why = "The server answers, and the rebuild did not succeed yet."
			}
			conds[r.Server] = append(conds[r.Server], monitor.Condition{Kind: "oldPrimaryNotRebuilt", Severity: "warning",
				Subject: fmt.Sprintf("server:%s|group:%d", r.Server, r.Group),
				Message: fmt.Sprintf("The old primary of shard group %d on server %s has not been turned into a standby %s after the failover. %s", r.Group, r.Server, now.Sub(r.CreatedAt).Round(time.Second), why)})
		}
	}
	l.mu.Lock()
	servers := map[string]bool{}
	for s := range l.alertedOld {
		servers[s] = true
	}
	for s := range conds {
		servers[s] = true
	}
	l.alertedOld = map[string]bool{}
	for s := range conds {
		l.alertedOld[s] = true
	}
	l.mu.Unlock()
	if len(servers) == 0 {
		return
	}
	c, err := l.ServiceConn(ctx, sysdb.Database)
	if err != nil {
		return
	}
	defer func() { _ = c.Close(context.WithoutCancel(ctx)) }()
	for s := range servers {
		if err := monitor.ReconcileScope(ctx, c, "server:"+s, conds[s], map[string]bool{"oldPrimaryNotRebuilt": true}, now); err != nil {
			l.Log.Warn("standby: updating the alert of an old primary that is not rebuilt", "server", s, "error", err)
		}
	}
}

func (l *Leader) groupOf(ctx context.Context, id int32) (groupInfo, error) {
	t, err := l.readTopology(ctx)
	if err != nil {
		return groupInfo{}, err
	}
	g, ok := t.group(id)
	if !ok {
		return groupInfo{}, fmt.Errorf("standby: shard group %d has no primary in the catalog", id)
	}
	return g, nil
}

// registerInCatalog is the step that records the ready standbys in the sharding
// catalog (kingres_shard 0.5, sequence 1) and stores the node id the catalog gave
// them; the list pushed afterwards carries it, and from then on the primary of the
// group names the standby in synchronous_standby_names. It looks at every ready
// standby that has no node id, not only at the ones this task made, so that a
// standby made before the registration existed, or one whose earlier registration
// was refused, is registered by the next task.
func (l *Leader) registerInCatalog(ctx context.Context, env *task.Env, _ work) error {
	if l.Registrar == nil {
		return task.Skip(ErrCatalogUnsupported.Error())
	}
	recs, err := l.st.list(ctx)
	if err != nil {
		return err
	}
	t, err := l.readTopology(ctx)
	if err != nil {
		return err
	}
	changed := false
	var firstErr error
	for _, rec := range recs {
		if rec.State != stateReady || rec.CatalogNode != nil || rec.Port == 0 {
			continue
		}
		g, ok := t.group(rec.Group)
		if !ok {
			continue
		}
		id, err := l.Registrar.Register(ctx, rec.Entry(g.Port))
		if err != nil {
			l.Log.Warn("standby: registering a standby in the catalog", "standby", rec.ID, "group", rec.Group, "error", err)
			if firstErr == nil {
				firstErr = fmt.Errorf("registering the standby of shard group %d on %s in the catalog: %w", rec.Group, rec.Server, err)
			}
			continue
		}
		if err := l.st.setCatalogNode(ctx, rec.ID, id); err != nil {
			return err
		}
		changed = true
		_ = env.SetDetail(ctx, fmt.Sprintf("Registered the standby of shard group %d on %s as node %d.", rec.Group, rec.Server, id))
	}
	if changed {
		if err := l.publish(ctx); err != nil {
			return err
		}
	}
	return firstErr
}

// unregister takes a standby out of the catalog (sequence 8). The list of its
// primary has stopped naming it (the plan without it was pushed before this
// call); until that primary's agent has applied the plan the extension refuses the
// call with KS009 ("cannot commit"), which is repeated for a short time.
func (l *Leader) unregister(ctx context.Context, e Entry) error {
	if l.Registrar == nil {
		return nil
	}
	var err error
	for i := 0; i < 12; i++ {
		if err = l.Registrar.Unregister(ctx, e); err == nil || !IsCatalogCode(err, sqlstateCannotCommit) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(l.UnregisterWait):
		}
	}
	return err
}

// PurgeServer removes every standby that has to go with a server that is being
// removed or leaves: the standbys it runs for other groups and the standbys of
// its own group elsewhere. The slots on the primaries are dropped by the plan that
// no longer names them. Instances on servers that cannot be reached are removed by
// those servers when they are back.
func (l *Leader) PurgeServer(ctx context.Context, server string) error {
	recs, err := l.st.list(ctx)
	if err != nil {
		return err
	}
	t, _ := l.readTopology(ctx)
	changed := false
	for _, r := range recs {
		if r.Server != server && r.PrimaryServer != server {
			continue
		}
		changed = true
		if err := l.st.markRemoving(ctx, r.ID); err != nil {
			return err
		}
	}
	if !changed {
		return nil
	}
	if err := l.publish(ctx); err != nil {
		return err
	}
	for _, r := range recs {
		if r.Server != server && r.PrimaryServer != server {
			continue
		}
		pp := 0
		if g, ok := t.group(r.Group); ok {
			pp = g.Port
		}
		entry := r.Entry(pp)
		if pp == 0 {
			entry.PrimaryPort = 1 // the group is gone: only the instance is removed
		}
		cctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
		err := l.call(cctx, r.Server, CallRemove, RemoveRequest{Entry: entry}, nil)
		cancel()
		if err != nil && !errors.Is(err, peerclient.ErrUnreachable) {
			l.Log.Warn("standby: removing a standby of a server that goes", "standby", r.ID, "server", r.Server, "error", err)
		}
		if err := l.unregister(ctx, entry); err != nil {
			return fmt.Errorf("removing a standby from the catalog: %w", err)
		}
		if err := l.st.delete(ctx, r.ID); err != nil {
			return err
		}
	}
	return l.publish(ctx)
}

// membershipChanging reports whether a server is being added, removed or is
// leaving (an unfinished membership task, or a server record that is not active).
func (l *Leader) membershipChanging(ctx context.Context) bool {
	changing := false
	err := l.Sys(ctx, func(c *pgx.Conn) error {
		return c.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM kingres_cp.task WHERE state IN ('pending', 'running', 'paused')
				AND kind IN ('server.add', 'server.remove', 'server.leave', 'cluster.join'))
			OR EXISTS (SELECT 1 FROM kingres_cp.server WHERE status <> 'active')`).Scan(&changing)
	})
	return err != nil || changing
}

// stable waits while a server is being added, removed or leaving (a standby is not
// made on a server that is about to go, nor while the ring changes) and then checks
// that the standby of e is still wanted: its server is a member, its group has a
// primary. It reports false for a standby that is not wanted any more; the next
// check of the leader works out what is needed then.
func (l *Leader) stable(ctx context.Context, env *task.Env, e Entry) (bool, error) {
	announced := false
	for l.membershipChanging(ctx) {
		if !announced {
			_ = env.SetDetail(ctx, "Waiting for a change of the servers of the cluster to finish.")
			announced = true
		}
		select {
		case <-ctx.Done():
			return false, ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
	if announced {
		_ = env.SetDetail(ctx, "")
	}
	t, err := l.readTopology(ctx)
	if err != nil {
		return false, err
	}
	if _, ok := t.group(e.Group); !ok {
		return false, nil
	}
	for _, s := range t.ring {
		if s.ID == e.Server {
			return true, nil
		}
	}
	return false, nil
}

// removedMeanwhile tells whether the record of a standby was deleted or marked for
// removal while a step of this task worked on it.
func (l *Leader) removedMeanwhile(ctx context.Context, id int64) (bool, error) {
	r, found, err := l.st.get(ctx, id)
	if err != nil {
		return false, err
	}
	return !found || r.State == stateRemoving, nil
}

// withoutGroups drops the records of the groups in busy.
func withoutGroups(recs []Record, busy map[int32]bool) []Record {
	if len(busy) == 0 {
		return recs
	}
	var out []Record
	for _, r := range recs {
		if !busy[r.Group] {
			out = append(out, r)
		}
	}
	return out
}

// Publish sends the standby list to every server under a new version (a failover
// changed the primary of a group and the records of its standbys).
func (l *Leader) Publish(ctx context.Context) error { return l.publish(ctx) }

// recordReplicationState writes, for each group whose primary answered, what the check
// found: the primary's flush position, whether a standby streamed and whether the group
// runs without its synchronous standby. A failover reads it as "the last position of the
// old primary that the leader knew" (design 2.6); it is the only record of the group
// once the primary's server is dead.
func (l *Leader) recordReplicationState(ctx context.Context, statuses map[string]Status) {
	err := l.Sys(ctx, func(c *pgx.Conn) error {
		for server, st := range statuses {
			ps := st.Primaries
			if len(ps) == 0 && st.Primary != nil {
				ps = []PrimaryStatus{*st.Primary}
			}
			for _, p := range ps {
				streaming := len(p.Streaming) > 0 && p.SynchronousStandbyNames != "" && !p.Without
				if _, err := c.Exec(ctx, `INSERT INTO kingres_cp.group_replication_state (group_id, primary_server, flush_lsn, sync_streaming, without_sync, planned_standbys, seen_at)
					VALUES ($1, $2, nullif($3, ''), $4, $5, $6, now())
					ON CONFLICT (group_id) DO UPDATE SET primary_server = $2, flush_lsn = coalesce(nullif($3, ''), kingres_cp.group_replication_state.flush_lsn),
						sync_streaming = $4, without_sync = $5, planned_standbys = $6, seen_at = now()`, p.Group, server, p.FlushLsn, streaming, p.Without, p.Planned); err != nil {
					return err
				}
			}
		}
		return nil
	})
	if err != nil {
		l.Log.Debug("standby: recording the replication state of the groups", "error", err)
	}
}

// ensureEverywhere makes sure every ready, registered standby is also in the catalog of every sharded database (a
// database made after the standby, or a registration that was refused for one database, is repaired here). Errors
// are logged; the next check repeats. Groups in a failover are left alone.
func (l *Leader) ensureEverywhere(ctx context.Context, t topology, recs []Record) {
	ens, ok := l.Registrar.(interface {
		EnsureEverywhere(ctx context.Context, e Entry) error
	})
	if !ok {
		return
	}
	for _, r := range recs {
		if r.State != stateReady || r.CatalogNode == nil || r.Port == 0 {
			continue
		}
		g, ok := t.group(r.Group)
		if !ok {
			continue
		}
		if err := ens.EnsureEverywhere(ctx, r.Entry(g.Port)); err != nil {
			l.Log.Warn("standby: registering a standby in the catalogs of the databases", "standby", r.ID, "group", r.Group, "error", err)
		}
	}
}
