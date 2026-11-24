// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/audit"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/cluster"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/failover"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/fault"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/fence"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/ha"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/instance"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/internalapi"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/lease"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/membership"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/monitor"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/ops"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/peerclient"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/standby"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/sysdb"
)

// The calls of the failover task (design ha-phase5-part2.md, 3.1.2). All of them come from
// the manager leader; the server that receives one acts on its own instances only.
const (
	callFailoverLease   = "POST /failover/lease"
	callFailoverDemote  = "POST /failover/demote"
	callFailoverPromote = "POST /failover/promote"
)

// failoverDeps is filled once the high-availability features exist (they are made after the
// service that needs them).
type failoverDeps struct {
	d  *haDeps
	mf *mapFailover
}

// newFailover builds the failover service of the manager leader: the ports of the task over
// the peer client, the extension's functions and the records of the leader.
func newFailover(svc *cluster.Service, o *ops.Ops, m *membership.Manager, stby *standbyParts, reg *ha.Registry, calls *callTable, self string, fh *fault.Hook,
	dataDir string, log *slog.Logger, auditFn func(context.Context, audit.Event), fd *failoverDeps) *failover.Service {
	cat := &foCatalog{svc: svc, log: log}
	pl := &foPlanner{svc: svc, self: self, reg: reg, stby: stby, cat: cat, log: log, audit: auditFn, calls: calls, client: m.Client}
	peers := &foPeers{self: self, calls: calls, client: m.Client}
	r := &failover.Runner{Peers: peers, Cat: cat, Plan: pl, Log: log, Inject: fh.FailoverInjection,
		NewRows: func(id int64) failover.Rows { return &failover.PGRows{Conns: svc, TaskID: id} }}
	fs := &failover.Service{Runner: r, Conns: svc, Engine: o.Engine, Log: log, Audit: auditFn}
	pl.svcRef = fs
	if mf, err := newMapFailover(dataDir, svc, self, log, fd, peers, cat, pl, r, func(point string) error {
		switch fh.FailoverInjection("map:" + point) {
		case "exit":
			log.Warn("the bootstrap of the map group ends kingresd at a crash point (injected by a test)", "point", point)
			os.Exit(137)
		case "hold":
			log.Warn("the bootstrap of the map group holds at a point (injected by a test)", "point", point)
			for fh.FailoverInjection("map:"+point) == "hold" {
				time.Sleep(200 * time.Millisecond)
			}
		}
		return nil
	}); err != nil {
		log.Error("the journal of the failover of the map group cannot be opened", "error", err)
	} else {
		fd.mf = mf
	}
	stby.leader.Busy = func(ctx context.Context) map[int32]bool { return pl.busyGroups(ctx) }
	return fs
}

// ---------------------------------------------------------------------------------
// Peers

type foPeers struct {
	self   string
	calls  *callTable
	client *peerclient.Client
}

// do makes a call to a server; this server's own is answered by its own handler.
func (p *foPeers) do(ctx context.Context, server, method, path string, in, out any) error {
	if server == p.self {
		key := method + " " + strings.SplitN(path, "?", 2)[0]
		var body []byte
		if method == http.MethodGet {
			if i := strings.IndexByte(path, '?'); i >= 0 {
				body = []byte(path[i+1:])
			}
		} else if in != nil {
			var err error
			if body, err = json.Marshal(in); err != nil {
				return err
			}
		}
		res, ok, err := p.calls.handle(ctx, key, p.self, body)
		if !ok {
			return &failover.CallError{Status: 404, Code: "not_found", Message: "no such call"}
		}
		if err != nil {
			return foCallError(err)
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
	err := p.client.Do(ctx, server, method, path, in, out)
	return foPeerError(err)
}

func foCallError(err error) error {
	var ce *internalapi.CallError
	if errors.As(err, &ce) {
		return &failover.CallError{Status: ce.Status, Code: ce.Code, Message: ce.Message}
	}
	return err
}

func foPeerError(err error) error {
	if err == nil {
		return nil
	}
	var ae *peerclient.APIError
	if errors.As(err, &ae) {
		return &failover.CallError{Status: ae.Status, Code: ae.Code, Message: ae.Message}
	}
	if errors.Is(err, peerclient.ErrUnreachable) || errors.Is(err, peerclient.ErrUnknownPeer) || errors.Is(err, peerclient.ErrNotMember) ||
		errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("%w: %v", failover.ErrUnreachable, err)
	}
	return err
}

func (p *foPeers) Position(ctx context.Context, server string, group int32) (failover.Position, error) {
	var out failover.Position
	err := p.do(ctx, server, "GET", "/failover/position?group="+strconv.Itoa(int(group)), nil, &out)
	return out, err
}

func (p *foPeers) State(ctx context.Context, server string, group int32) (failover.State, error) {
	var out failover.State
	err := p.do(ctx, server, "GET", "/failover/state?group="+strconv.Itoa(int(group)), nil, &out)
	return out, err
}

func (p *foPeers) LeaseSettings(ctx context.Context, server string) (failover.LeaseSettings, error) {
	var out struct {
		DurationMs int64 `json:"durationMs"`
		MarginMs   int64 `json:"marginMs"`
	}
	if err := p.do(ctx, server, "GET", "/lease/state", nil, &out); err != nil {
		return failover.LeaseSettings{}, err
	}
	if out.DurationMs == 0 && out.MarginMs == 0 {
		return failover.LeaseSettings{}, &failover.CallError{Status: 404, Code: "not_found", Message: "the server does not report its lease settings"}
	}
	return failover.LeaseSettings{DurationMs: out.DurationMs, MarginMs: out.MarginMs}, nil
}

func (p *foPeers) Freeze(ctx context.Context, server string, group int32, ttl time.Duration) error {
	req := standby.FreezeRequest{Group: group, TTLMs: ttl.Milliseconds()}
	if tok, ok := fence.From(ctx); ok {
		req.Task, req.Epoch = tok.Task, tok.Epoch
	}
	return p.do(ctx, server, "POST", "/failover/freeze", req, nil)
}

func (p *foPeers) Unfreeze(ctx context.Context, server string, group int32) error {
	req := standby.UnfreezeRequest{Group: group}
	if tok, ok := fence.From(ctx); ok {
		req.Task, req.Epoch = tok.Task, tok.Epoch
	}
	return p.do(ctx, server, "POST", "/failover/unfreeze", req, nil)
}

func (p *foPeers) StandbysGone(ctx context.Context, server, gone string, ttl time.Duration) error {
	return p.do(ctx, server, "POST", "/failover/standbys-gone", standby.GoneRequest{Server: gone, TTLMs: ttl.Milliseconds()}, nil)
}

func (p *foPeers) Demote(ctx context.Context, server string, r failover.DemoteRequest) (failover.DemoteAnswer, error) {
	var out failover.DemoteAnswer
	ctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	err := p.do(ctx, server, "POST", "/failover/demote", r, &out)
	return out, err
}

func (p *foPeers) Lease(ctx context.Context, server string, r failover.LeaseRequest) (failover.LeaseAnswer, error) {
	var out failover.LeaseAnswer
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	err := p.do(ctx, server, "POST", "/failover/lease", r, &out)
	return out, err
}

func (p *foPeers) Promote(ctx context.Context, server string, r failover.PromoteRequest) (failover.PromoteAnswer, error) {
	var out failover.PromoteAnswer
	ctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	err := p.do(ctx, server, "POST", "/failover/promote", r, &out)
	return out, err
}

func (p *foPeers) RecoverPrepared(ctx context.Context, server string, group int32, db string) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	return p.do(ctx, server, "POST", "/failover/prepared", struct {
		Group    int32  `json:"group"`
		Database string `json:"database,omitempty"`
	}{group, db}, nil)
}

func (p *foPeers) Repoint(ctx context.Context, server string, r failover.RepointRequest) (failover.RepointAnswer, error) {
	var out failover.RepointAnswer
	ctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	err := p.do(ctx, server, "POST", "/standby/repoint", standby.RepointRequest{Group: r.Group, StandbyID: r.StandbyID, InstanceID: r.InstanceID,
		PrimaryHost: r.PrimaryHost, PrimaryPort: r.PrimaryPort, ExpectedTerm: r.ExpectedTerm}, &out)
	return out, err
}

// ---------------------------------------------------------------------------------
// Catalog: the calls of the extension, each in a session of its own at READ COMMITTED, one
// statement per call, with a named deadline.

type foCatalog struct {
	svc *cluster.Service
	log *slog.Logger
}

// Deadlines of the control calls, named. The README of the extension bounds a call by its
// settings (node_timeout, node_connect_timeout) and the reviews of the extension changed
// the bound twice; the deadline is derived from the settings the call will meet and never
// from a constant in a step. A deadline that ends means "the call did not finish": the
// caller reads the state and repeats (sequence 9), it does not assume anything.
const (
	defaultNodeTimeout    = 60 * time.Second
	defaultConnectTimeout = 10 * time.Second
	controlSlack          = 30 * time.Second
)

// promoteNodeDeadline is the client time-out of one call of kingres.promote_node: at least
// three node timeouts and a connect timeout for every node (the README, "Bounds that apply
// to every call"), and a slack for the commit on the coordinator.
func promoteNodeDeadline(nodeTimeout, connectTimeout time.Duration, nodes int) time.Duration {
	return 3*nodeTimeout + time.Duration(nodes)*connectTimeout + controlSlack
}

// shortControlDeadline is the deadline of a call that sends one statement to each node
// (recover_prepared_transactions).
func shortControlDeadline(nodeTimeout time.Duration) time.Duration {
	return 2*nodeTimeout + controlSlack
}

func (c *foCatalog) session(ctx context.Context, db string) (*pgx.Conn, error) {
	conn, err := c.svc.ServiceConn(ctx, db)
	if err != nil {
		return nil, err
	}
	if _, err := conn.Exec(ctx, `SET SESSION CHARACTERISTICS AS TRANSACTION ISOLATION LEVEL READ COMMITTED`); err != nil {
		_ = conn.Close(context.WithoutCancel(ctx))
		return nil, err
	}
	return conn, nil
}

func settingDuration(ctx context.Context, conn *pgx.Conn, name string, def time.Duration) time.Duration {
	var v string
	if err := conn.QueryRow(ctx, `SELECT current_setting($1, true)`, name).Scan(&v); err != nil || v == "" {
		return def
	}
	if d, err := time.ParseDuration(strings.ReplaceAll(strings.ToLower(v), " ", "")); err == nil && d > 0 {
		return d
	}
	if n, err := strconv.Atoi(v); err == nil && n > 0 {
		return time.Duration(n) * time.Millisecond
	}
	return def
}

func (c *foCatalog) Databases(ctx context.Context) ([]string, error) {
	return catalogDatabases(ctx, c.svc, true)
}

func (c *foCatalog) Nodes(ctx context.Context, db string) ([]failover.Node, error) {
	conn, err := c.session(ctx, db)
	if err != nil {
		return nil, err
	}
	defer func() { _ = conn.Close(context.WithoutCancel(ctx)) }()
	rows, err := conn.Query(ctx, `SELECT node_id, group_id, host, port, role FROM kingres.node ORDER BY node_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []failover.Node
	for rows.Next() {
		var n failover.Node
		if err := rows.Scan(&n.ID, &n.Group, &n.Host, &n.Port, &n.Role); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

func (c *foCatalog) Status(ctx context.Context, db string) (int64, bool, error) {
	conn, err := c.session(ctx, db)
	if err != nil {
		return 0, false, err
	}
	defer func() { _ = conn.Close(context.WithoutCancel(ctx)) }()
	var term int64
	var sup bool
	err = conn.QueryRow(ctx, `SELECT term, superseded FROM kingres.catalog_status`).Scan(&term, &sup)
	return term, sup, err
}

func sqlErr(err error) error {
	if err == nil {
		return nil
	}
	var pe *pgconn.PgError
	if errors.As(err, &pe) {
		return &failover.SQLError{Code: pe.Code, Text: pe.Message}
	}
	return &failover.SQLError{Text: err.Error()}
}

func (c *foCatalog) PromoteNode(ctx context.Context, db string, id int32) (failover.PromoteResult, error) {
	conn, err := c.session(ctx, db)
	if err != nil {
		return failover.PromoteResult{}, sqlErr(err)
	}
	defer func() { _ = conn.Close(context.WithoutCancel(ctx)) }()
	nt := settingDuration(ctx, conn, "kingres_shard.node_timeout", defaultNodeTimeout)
	ct := settingDuration(ctx, conn, "kingres_shard.node_connect_timeout", defaultConnectTimeout)
	var nodes int
	_ = conn.QueryRow(ctx, `SELECT count(*) FROM kingres.node`).Scan(&nodes)
	cctx, cancel := context.WithTimeout(ctx, promoteNodeDeadline(nt, ct, nodes))
	defer cancel()
	var doc *string
	if err := conn.QueryRow(cctx, `SELECT kingres.promote_node($1)::text`, id).Scan(&doc); err != nil {
		if ctx.Err() != nil {
			return failover.PromoteResult{}, ctx.Err()
		}
		return failover.PromoteResult{}, sqlErr(err)
	}
	if doc == nil {
		return failover.PromoteResult{}, &failover.SQLError{Code: failover.StateNoSuchNode, Text: "promote_node answered NULL"}
	}
	var v struct {
		Status    string  `json:"status"`
		Term      int64   `json:"term"`
		Final     bool    `json:"final"`
		Unreached []int32 `json:"unreached"`
		BlockedBy []struct {
			Node     int32 `json:"node"`
			Prepared []struct {
				Gid   string `json:"gid"`
				Class string `json:"class"`
			} `json:"prepared"`
		} `json:"blocked_by"`
	}
	if err := json.Unmarshal([]byte(*doc), &v); err != nil {
		return failover.PromoteResult{}, &failover.SQLError{Text: "the answer of promote_node is not valid JSON: " + err.Error()}
	}
	res := failover.PromoteResult{Status: v.Status, Term: v.Term, Final: v.Final, Unreached: v.Unreached}
	for _, b := range v.BlockedBy {
		for _, p := range b.Prepared {
			res.Blocked = append(res.Blocked, failover.Blocked{Gid: p.Gid, Node: b.Node, Class: p.Class})
		}
	}
	return res, nil
}

func (c *foCatalog) RecoverPrepared(ctx context.Context, db string) error {
	conn, err := c.session(ctx, dbOrSystem(db))
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close(context.WithoutCancel(ctx)) }()
	nt := settingDuration(ctx, conn, "kingres_shard.node_timeout", defaultNodeTimeout)
	cctx, cancel := context.WithTimeout(ctx, shortControlDeadline(nt))
	defer cancel()
	_, err = conn.Exec(cctx, `SELECT kingres.recover_prepared_transactions()`)
	return sqlErr(err)
}

func (c *foCatalog) CheckCluster(ctx context.Context, db string) ([]failover.Incident, error) {
	conn, err := c.session(ctx, dbOrSystem(db))
	if err != nil {
		return nil, err
	}
	defer func() { _ = conn.Close(context.WithoutCancel(ctx)) }()
	nt := settingDuration(ctx, conn, "kingres_shard.node_timeout", defaultNodeTimeout)
	cctx, cancel := context.WithTimeout(ctx, shortControlDeadline(nt))
	defer cancel()
	rows, err := conn.Query(cctx, `SELECT check_name, coalesce(node_id::text, ''), coalesce(object, ''), coalesce(detail, '') FROM kingres.check_cluster()`)
	if err != nil {
		return nil, sqlErr(err)
	}
	defer rows.Close()
	var out []failover.Incident
	for rows.Next() {
		var kind, node, obj, detail string
		if err := rows.Scan(&kind, &node, &obj, &detail); err != nil {
			return nil, err
		}
		out = append(out, failover.Incident{Kind: kind, Message: strings.TrimSpace(fmt.Sprintf("node %s %s: %s", node, obj, detail))})
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------------------------
// Planner

type foPlanner struct {
	svc    *cluster.Service
	self   string
	reg    *ha.Registry
	stby   *standbyParts
	cat    *foCatalog
	log    *slog.Logger
	audit  func(context.Context, audit.Event)
	calls  *callTable
	client *peerclient.Client
	svcRef *failover.Service
	// localSys, when set, is where the planner reads the system database: the local copy of a candidate for the map group, whose
	// own system database is not writable (the bootstrap, design 3.4).
	localSys func(ctx context.Context) (*pgx.Conn, error)
}

func (p *foPlanner) sys(ctx context.Context, f func(c *pgx.Conn) error) error {
	var c *pgx.Conn
	var err error
	if p.localSys != nil {
		c, err = p.localSys(ctx)
	} else {
		c, err = p.svc.ServiceConn(ctx, sysdb.Database)
	}
	if err != nil {
		return err
	}
	defer func() { _ = c.Close(context.WithoutCancel(ctx)) }()
	return f(c)
}

func (p *foPlanner) Self() string { return p.self }

func (p *foPlanner) Members(ctx context.Context) ([]string, error) {
	nodes, err := p.cat.Nodes(ctx, sysdb.Database)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var out []string
	for _, n := range nodes {
		if !seen[n.Host] {
			seen[n.Host] = true
			out = append(out, n.Host)
		}
	}
	sort.Strings(out)
	return out, nil
}

func (p *foPlanner) Live(_ context.Context, server string) string {
	if server == p.self {
		return "live"
	}
	return peerStateName(p.reg.Liveness().State(server))
}

func (p *foPlanner) Standbys(ctx context.Context, group int32) ([]failover.StandbyRec, error) {
	var out []failover.StandbyRec
	err := p.sys(ctx, func(c *pgx.Conn) error {
		rows, err := c.Query(ctx, `SELECT standby_id, group_id, server_id, instance_id, coalesce(port, 0), state, coalesce(catalog_node_id, 0), generation
			FROM kingres_cp.standby WHERE group_id = $1 ORDER BY standby_id`, group)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var s failover.StandbyRec
			if err := rows.Scan(&s.ID, &s.Group, &s.Server, &s.InstanceID, &s.Port, &s.State, &s.CatalogNode, &s.Generation); err != nil {
				return err
			}
			out = append(out, s)
		}
		return rows.Err()
	})
	return out, err
}

func (p *foPlanner) Facts(ctx context.Context, group int32) (failover.GroupFacts, error) {
	var f failover.GroupFacts
	err := p.sys(ctx, func(c *pgx.Conn) error {
		var streaming, without bool
		var planned int
		var lsn *string
		var seen time.Time
		var primary string
		err := c.QueryRow(ctx, `SELECT coalesce(sync_streaming, false), coalesce(without_sync, false), flush_lsn, seen_at, coalesce(primary_server, ''), planned_standbys FROM kingres_cp.group_replication_state WHERE group_id = $1`, group).
			Scan(&streaming, &without, &lsn, &seen, &primary, &planned)
		if err == nil {
			f.SyncStreaming = streaming && !without
			f.PrimaryPlanned = planned
			// The age that counts is the age of the last check at the moment the primary was last heard, not now: a failover
			// takes longer than the check's validity, and the check stops when the primary's server stops answering.
			f.LastKnownAge = time.Since(seen)
			if primary != "" {
				f.LastKnownAge = p.unobservedAge(ctx, primary, f.LastKnownAge)
			}
			if lsn != nil {
				f.LastKnownLsn = *lsn
			}
		} else if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		// The databases that commit asynchronously: a per-database synchronous_commit of local or off, or the default of
		// the cluster when the database has none. Every user database counts for every group: a sharded database has
		// buckets in all of them, and a database that is not sharded lives in the map group (design 2.6, simplified).
		def := "sync"
		var dv *string
		if err := c.QueryRow(ctx, `SELECT value #>> '{}' FROM kingres_cp.cluster_setting WHERE key = 'durability.default'`).Scan(&dv); err == nil && dv != nil {
			def = *dv
		}
		rows, err := c.Query(ctx, `SELECT d.datname, coalesce((SELECT split_part(c, '=', 2) FROM pg_db_role_setting r, unnest(r.setconfig) c
				WHERE r.setdatabase = d.oid AND r.setrole = 0 AND c LIKE 'synchronous\_commit=%'), '') FROM pg_database d
			WHERE NOT d.datistemplate AND d.datallowconn AND d.datname NOT IN ('postgres', $1) ORDER BY 1`, sysdb.Database)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var name, sc string
			if err := rows.Scan(&name, &sc); err != nil {
				return err
			}
			if sc == "" {
				sc = def
			}
			switch strings.ToLower(sc) {
			case "local", "off", "async":
				f.AsyncDatabases = append(f.AsyncDatabases, name)
			}
		}
		return rows.Err()
	})
	return f, err
}

func (p *foPlanner) PrimaryNames(ctx context.Context, server string, group int32) ([]int64, bool) {
	var st standby.Status
	cctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	var err error
	if server == p.self {
		var res any
		res, err = p.stby.agent.HandleCall(cctx, standby.CallStatus, nil)
		if s, ok := res.(standby.Status); ok {
			st = s
		}
	} else {
		err = p.client.Do(cctx, server, "GET", "/standby/status", nil, &st)
	}
	if err != nil {
		return nil, false
	}
	var ps *standby.PrimaryStatus
	for i := range st.Primaries {
		if st.Primaries[i].Group == group {
			ps = &st.Primaries[i]
		}
	}
	if ps == nil && st.Primary != nil && st.Primary.Group == group {
		ps = st.Primary
	}
	if ps == nil {
		return nil, false
	}
	var ids []int64
	for name := range standby.ListedNames(ps.SynchronousStandbyNames) {
		if id, err := strconv.ParseInt(strings.TrimPrefix(name, "kingres_standby_"), 10, 64); err == nil {
			ids = append(ids, id)
		}
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return ids, true
}

func (p *foPlanner) Preferred(ctx context.Context, group int32, server string) bool {
	nodes, err := p.cat.Nodes(ctx, sysdb.Database)
	if err != nil {
		return false
	}
	for _, n := range nodes {
		if n.Group == group {
			return n.Host == server // the first node of the group in the catalog is its home
		}
	}
	return false
}

func (p *foPlanner) Ring(from, to string) int { return 0 }

func (p *foPlanner) AfterPromotion(ctx context.Context, group int32, promoted failover.StandbyRec, newServer string, newPort int, old failover.OldPrimaryRec) error {
	err := p.sys(ctx, func(c *pgx.Conn) error {
		tx, err := c.Begin(ctx)
		if err != nil {
			return err
		}
		defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
		if err := leaseProof(ctx, tx); err != nil {
			return err
		}
		// The instance of the promoted standby is the primary now: its record goes; the others follow it.
		if _, err := tx.Exec(ctx, `DELETE FROM kingres_cp.standby WHERE standby_id = $1 AND group_id = $2`, promoted.ID, group); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE kingres_cp.standby SET primary_server = $2, updated_at = now() WHERE group_id = $1`, group, newServer); err != nil {
			return err
		}
		// The old primary is a standby that waits to be rebuilt (design 3.3 F9, 6.1): its record is inserted in the same
		// transaction, rebuilding and diverged, with the generation one higher than it had (a repeated step finds it and
		// changes nothing but the primary). Its directory (0: its server finds it), its port and its node are the ones the
		// failover knows; the registration step finds the node the catalog kept for it.
		if old.Server != "" && old.Server != newServer {
			var id int64
			if err := tx.QueryRow(ctx, `INSERT INTO kingres_cp.standby (group_id, server_id, primary_server, instance_id, port, slot_name, state, needs_rebuild, diverged, generation)
				VALUES ($1, $2, $3, $4, nullif($5, 0), 'pending', 'rebuilding', true, true, 1)
				ON CONFLICT (group_id, server_id) DO UPDATE SET primary_server = EXCLUDED.primary_server, updated_at = now()
				RETURNING standby_id`, group, old.Server, newServer, old.InstanceID, old.Port).Scan(&id); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `UPDATE kingres_cp.standby SET slot_name = $2 WHERE standby_id = $1`, id, standby.SlotName(id)); err != nil {
				return err
			}
		}
		return tx.Commit(ctx)
	})
	if err != nil {
		return err
	}
	if p.stby != nil && p.stby.leader != nil {
		if err := p.stby.leader.Publish(ctx); err != nil {
			p.log.Warn("failover: sending the standby list", "error", err)
		}
		p.stby.leader.Kick()
	}
	return nil
}

func (p *foPlanner) FlagRebuild(ctx context.Context, id int64) error {
	return p.sys(ctx, func(c *pgx.Conn) error {
		_, err := c.Exec(ctx, `UPDATE kingres_cp.standby SET needs_rebuild = true, diverged = true WHERE standby_id = $1`, id)
		return err
	})
}

func (p *foPlanner) RebalanceRunning(ctx context.Context) (bool, error) {
	var n int
	err := p.sys(ctx, func(c *pgx.Conn) error {
		return c.QueryRow(ctx, `SELECT count(*) FROM kingres_cp.task WHERE kind = 'rebalance' AND state IN ('pending', 'running')`).Scan(&n)
	})
	return n > 0, err
}

func (p *foPlanner) SyncNamesOf(ctx context.Context, server string, group int32) (string, int, bool) {
	var st standby.Status
	cctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	var err error
	if server == p.self {
		var res any
		res, err = p.stby.agent.HandleCall(cctx, standby.CallStatus, nil)
		if s, ok := res.(standby.Status); ok {
			st = s
		}
	} else {
		err = p.client.Do(cctx, server, "GET", "/standby/status", nil, &st)
	}
	if err != nil {
		return "", 0, false
	}
	recs, rerr := p.Standbys(ctx, group)
	if rerr != nil {
		return "", 0, false
	}
	planned := 0
	for _, r := range recs {
		if r.State == "ready" {
			planned++
		}
	}
	for _, ps := range st.Primaries {
		if ps.Group == group {
			return ps.SynchronousStandbyNames, planned, true
		}
	}
	return "", planned, false
}

func (p *foPlanner) Alert(ctx context.Context, kind, severity, subject, message string, raise bool) {
	err := p.sys(ctx, func(c *pgx.Conn) error {
		if !raise {
			_, err := c.Exec(ctx, `UPDATE kingres_cp.alert SET cleared_at = now() WHERE kind = $1 AND subject = $2 AND cleared_at IS NULL`, kind, subject)
			return err
		}
		_, err := c.Exec(ctx, `INSERT INTO kingres_cp.alert (severity, kind, subject, message, raised_at)
			VALUES ($1, $2, $3, $4, now()) ON CONFLICT (kind, subject) WHERE cleared_at IS NULL DO UPDATE SET message = $4, severity = $1`, severity, kind, subject, message)
		return err
	})
	if err != nil {
		p.log.Warn("failover: raising an alert", "kind", kind, "error", err)
	}
}

func (p *foPlanner) Audit(ctx context.Context, action string, group int32, details map[string]any) {
	if p.audit == nil {
		return
	}
	p.audit(context.WithoutCancel(ctx), audit.Event{Actor: "system", Source: "system", Action: action, ResourceType: "shardGroup",
		ResourceID: strconv.Itoa(int(group)), Outcome: audit.OutcomeSuccess, Details: details})
}

// LiveReplication asks the primary of the map group (the system database of this server, the leader's) for its standbys and its flush position, within three seconds.
func (p *foPlanner) LiveReplication(ctx context.Context) (failover.LiveReplication, error) {
	var out failover.LiveReplication
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	err := p.sys(ctx, func(c *pgx.Conn) error {
		if err := c.QueryRow(ctx, `SELECT pg_current_wal_flush_lsn()::text`).Scan(&out.FlushLsn); err != nil {
			return err
		}
		rows, err := c.Query(ctx, `SELECT state, sync_state, coalesce(sent_lsn::text, ''), coalesce(replay_lsn::text, '') FROM pg_stat_replication WHERE state = 'streaming'`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var st, sync, sent, replay string
			if err := rows.Scan(&st, &sync, &sent, &replay); err != nil {
				return err
			}
			out.Streaming++
			if sync == "sync" || sync == "quorum" {
				out.SyncStreaming = true
			}
			if a, ok := failover.ParseLSN(sent); ok {
				if b, _ := failover.ParseLSN(out.SentLsn); a > b {
					out.SentLsn = sent
				}
			}
			if a, ok := failover.ParseLSN(replay); ok {
				if b, _ := failover.ParseLSN(out.ReplayLsn); a > b {
					out.ReplayLsn = replay
				}
			}
		}
		return rows.Err()
	})
	return out, err
}

// busyGroups are the groups of an unfinished failover run.
func (p *foPlanner) busyGroups(ctx context.Context) map[int32]bool {
	out := map[int32]bool{}
	_ = p.sys(ctx, func(c *pgx.Conn) error {
		rows, err := c.Query(ctx, `SELECT g.group_id FROM kingres_cp.failover_group g JOIN kingres_cp.failover_run r USING (task_id)
			JOIN kingres_cp.task t ON t.id = r.task_id WHERE t.state IN ('pending', 'running', 'paused')`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var g int32
			if err := rows.Scan(&g); err == nil {
				out[g] = true
			}
		}
		return rows.Err()
	})
	return out
}

// leaseProof proves, inside tx, that the failover run in the context still holds its task.
func leaseProof(ctx context.Context, tx pgx.Tx) error {
	return failover.ProveLease(ctx, tx)
}

var _ = monitor.Condition{}

// ---------------------------------------------------------------------------------
// The callee side: what a server does on its own instances when the leader asks.

func staleTerm(ctx context.Context, d *haDeps, expected int64, strict bool) error {
	if expected < 0 {
		return nil // the caller does not know the term (a negative value says so); 0 is the term of a cluster that never failed over
	}
	c, err := d.Svc.SystemConn(ctx)
	if err != nil {
		d.Log.Warn("failover: the catalog term of this server cannot be read; the call is not judged by it", "error", err)
		return unreadableTerm(strict)
	}
	defer func() { _ = c.Close(context.WithoutCancel(ctx)) }()
	var term int64
	if err := c.QueryRow(ctx, `SELECT term FROM kingres.catalog_status`).Scan(&term); err != nil {
		d.Log.Warn("failover: the catalog term of this server cannot be read; the call is not judged by it", "error", err)
		return unreadableTerm(strict)
	}
	d.Log.Debug("failover: the catalog term of this server", "term", term, "expected", expected)
	if term > expected {
		return &internalapi.CallError{Status: http.StatusConflict, Code: "stale_term", Message: "The catalog has a newer term than the failover knows."}
	}
	return nil
}

func badBody(what string) error {
	return &internalapi.CallError{Status: http.StatusBadRequest, Code: "bad_request", Message: "The body is not " + what + "."}
}

func handleFailoverLease(ctx context.Context, d *haDeps, body []byte) (any, error) {
	var r failover.LeaseRequest
	if err := json.Unmarshal(body, &r); err != nil || r.Group < 0 {
		return nil, badBody("a lease request")
	}
	if err := requireGroup(body, "a lease request"); err != nil {
		return nil, err
	}
	if d.LeaseMgr == nil {
		return nil, notStarted
	}
	if r.Role == failover.LeaseRelease {
		// F3-01: the adoption of a promoted primary (attached) is not released by a failed task: the lease guards a
		// running primary, and forgetting it would let the lease run out and the watchdog stop the only promoted copy.
		if !d.LeaseMgr.Unadopt(r.Group) {
			return failover.LeaseAnswer{Held: true}, nil
		}
		return failover.LeaseAnswer{}, nil
	}
	if r.Role != failover.LeaseFence && r.Role != failover.LeaseHolder {
		return nil, badBody("a lease request with a known role")
	}
	if err := staleTerm(ctx, d, r.ExpectedTerm, false); err != nil {
		return nil, err
	}
	if r.TransferTo != "" {
		res, err := d.LeaseMgr.Transfer(ctx, r.Group, r.TransferTo)
		if err != nil {
			d.Log.Warn("failover: handing the lease over", "group", r.Group, "to", r.TransferTo, "error", err)
			return failover.LeaseAnswer{Transferred: false}, nil
		}
		return failover.LeaseAnswer{Transferred: res.Granted >= res.Need, Holder: r.TransferTo}, nil
	}
	kind := lease.AdoptFence
	if r.Role == failover.LeaseHolder {
		kind = lease.AdoptHolder
	}
	if err := d.LeaseMgr.Adopt(r.Group, kind); err != nil {
		return nil, err
	}
	st := d.LeaseMgr.AdoptionStatus(r.Group)
	return failover.LeaseAnswer{Held: st.Held, Rounds: st.Rounds, WaitMs: st.WaitMs, RefusedBy: st.RefusedBy}, nil
}

func handleFailoverDemote(ctx context.Context, d *haDeps, body []byte) (any, error) {
	var r failover.DemoteRequest
	if err := json.Unmarshal(body, &r); err != nil || r.Group < 0 {
		return nil, badBody("a demote request")
	}
	if err := requireGroup(body, "a demote request"); err != nil {
		return nil, err
	}
	if d.LeaseMgr == nil {
		return nil, notStarted
	}
	// The clean stop of a switchover is asked again to read the proof, when the instance is stopped and its catalog cannot be read:
	// the term is then judged only when it can be read.
	// The term of the run is required (F4-09). The clean stop of a switchover is asked again to read the proof when the instance is stopped and
	// its catalog cannot be read: the term is then judged only when it can be read; while the instance runs, an unreadable term refuses.
	if r.Mode == "fast" && r.ExpectedTerm < 0 {
		return nil, badBody("a demote request with the term of the run")
	}
	if err := staleTerm(ctx, d, r.ExpectedTerm, r.Mode != "fast" || primaryRuns(ctx, d, r.Group)); err != nil {
		return nil, err
	}
	if r.Mode == "fast" {
		if r.Group == 0 && r.Candidate != "" {
			// Before anything is stopped (F7-01): the hold that follows may be ended later only on the word of this server.
			if err := d.LeaseMgr.RecordCandidate(0, r.Candidate); err != nil {
				return nil, err
			}
		}
		return demoteClean(ctx, d, r)
	}
	li, ok := d.Svc.PrimaryOf(r.Group)
	if !ok {
		return nil, &internalapi.CallError{Status: http.StatusConflict, Code: "not_primary", Message: "This server runs no primary of the shard group."}
	}
	return fencedStop(ctx, d.Svc.Manager(), d.LeaseMgr, d.Fault, d.Log, li.ID, r)
}

// The parts of the instance manager and of the lease manager that the fenced stop uses, so that a test can stand in for them.
type immediateManager interface {
	StopImmediate(ctx context.Context, id int) error
	Running(ctx context.Context, id int) (bool, error)
}

type fenceLeases interface {
	RecordCandidate(group int32, server string) error
	Candidate(group int32) string
	ForgetCandidate(group int32)
	Relinquish(group int32) error
	Transfer(ctx context.Context, group int32, to string) (lease.TransferResult, error)
}

// fencedStop is the immediate stop of a failover (mode immediate): the old primary is fenced in this order, each part before the next. (1) A hard failover of the
// map group of a live leader's server names the server the group is handed to (as the clean stop does, F7-01), so that the hold the marker makes is ended later only on
// that server's word. (2) The renewal of the lease ends and the marker that keeps the primary from starting here again is written, before anything is stopped. (3) The
// instance is stopped with an immediate shutdown. (4) Only when it is seen stopped is the lease handed to the server the call names: a primary that is still running
// keeps a lease that is no longer renewed, which runs out and takes the primary with it (the watchdog), and nobody else holds it before that.
func fencedStop(ctx context.Context, mgr immediateManager, lm fenceLeases, fh *fault.Hook, log *slog.Logger, id int, r failover.DemoteRequest) (any, error) {
	wrote := false
	if r.Group == 0 && r.Candidate != "" {
		wrote = lm.Candidate(0) == ""
		if err := lm.RecordCandidate(0, r.Candidate); err != nil {
			return nil, err
		}
	}
	if err := lm.Relinquish(r.Group); err != nil {
		if wrote {
			lm.ForgetCandidate(0) // nothing was changed: the record must not outlive the call that made it
		}
		return nil, err
	}
	out := failover.DemoteAnswer{}
	stopCtx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	if fh.FailoverInjection("demote:nostop") == "skip" {
		// A test: the instance does not stop (a postmaster that hangs); nothing is handed over, the lease runs out and the watchdog stops it.
		log.Warn("failover: the old primary is not stopped (test fault demote:nostop)", "group", r.Group, "instance", id)
	} else if err := mgr.StopImmediate(stopCtx, id); err != nil {
		log.Warn("failover: stopping the old primary", "group", r.Group, "instance", id, "error", err)
	}
	running, err := mgr.Running(ctx, id)
	out.Stopped = err == nil && !running
	if out.Stopped && r.TransferTo != "" {
		res, terr := lm.Transfer(ctx, r.Group, r.TransferTo)
		out.Transferred = terr == nil && res.Granted >= res.Need
		if terr != nil {
			log.Warn("failover: the lease was not handed over; it runs out by itself", "group", r.Group, "error", terr)
		}
		// A test: kingresd ends right after it fenced its primary and handed the lease over (the leader changes at the stop).
		if fh.FailoverInjection("demote:exit") == "exit" {
			os.Exit(137)
		}
	}
	return out, nil
}

func handleFailoverPromote(ctx context.Context, d *haDeps, body []byte) (any, error) {
	var r failover.PromoteRequest
	if err := json.Unmarshal(body, &r); err != nil || r.Group < 0 {
		return nil, badBody("a promote request")
	}
	if err := requireGroup(body, "a promote request"); err != nil {
		return nil, err
	}
	if d.LeaseMgr == nil || d.Standby == nil {
		return nil, notStarted
	}
	// Tests of the planned switchover of the map group: the call answers "unavailable" for as long as the fault is in force (a candidate that never promotes),
	// or holds for 20 s before it does anything (an abandon that arrives while the call is in flight).
	// A test (F6-01): the standby's instance cannot be reached at all, neither by the promoting call nor by the question whether it is still in recovery (the answer of its control
	// data is what then counts).
	if d.Fault.FailoverInjection("promote:unreachable") == "unreachable" {
		testInstanceUnreachable.Store(true)
		return nil, &internalapi.CallError{Status: http.StatusServiceUnavailable, Code: "unavailable", Message: "The instance cannot be reached."}
	}
	if d.Fault.FailoverInjection("promote:unavailable") == "unavailable" {
		return nil, &internalapi.CallError{Status: http.StatusServiceUnavailable, Code: "unavailable", Message: "The instance cannot be reached."}
	}
	if d.Fault.FailoverInjection("promote:slow") == "slow" {
		select {
		case <-time.After(20 * time.Second):
		case <-ctx.Done():
		}
	}
	started := time.Now()
	// The promotion runs under the standby agent's lifecycle lock: the agent proves that a directory is a standby and
	// removes or rebuilds it under the same lock, so a removal cannot be between the proof and the delete while this
	// instance is promoted (F2-08). The role is read after the lock is taken.
	unlock, lerr := d.Standby.agent.LockLifecycle(ctx)
	if lerr != nil {
		return nil, &internalapi.CallError{Status: http.StatusServiceUnavailable, Code: "unavailable", Message: "A create, remove or rebuild of a standby is running on this server; repeat the call."}
	}
	defer unlock()
	// A test (F6-02): the call holds inside the lock, as a promotion does, before the lease is checked and the instance is promoted.
	if d.Fault.FailoverInjection("promote:slowlock") == "slow" {
		select {
		case <-time.After(20 * time.Second):
		case <-ctx.Done():
		}
	}
	// A bootstrap that was given up does not promote (F7-02): the abandon marks the journal under this lock, so a call that waited for it finds the mark here.
	if err := promoteBlockedByJournal(d.MapFailover, r.Group); err != nil {
		return nil, err
	}
	var li *cluster.LocalInstance
	for _, x := range d.Svc.LocalInstances() {
		x := x
		if x.Role.Group == r.Group && (r.InstanceID == 0 || x.ID == r.InstanceID) && (x.Role.IsStandby() || x.Role.Writable()) {
			li = &x
		}
	}
	if li == nil {
		return nil, &internalapi.CallError{Status: http.StatusConflict, Code: "not_standby", Message: "This server runs no standby of the shard group."}
	}
	if err := staleTerm(ctx, d, r.ExpectedTerm, true); err != nil {
		return nil, err
	}
	// An instance that was promoted by an earlier call and whose kingresd restarted since is held back or stopped by the
	// start gate until the lease is held again: it cannot be reached, and the answer that makes the task hand the lease
	// over again is "the lease is not held", not "unavailable" (the manager starts the instance once the adopted lease is held).
	if li.Role.Writable() && !d.Registry.Leases().Holds(r.Group) {
		return nil, &internalapi.CallError{Status: http.StatusConflict, Code: "lease_not_held", Message: "This server does not hold the lease of the shard group."}
	}
	c, err := d.Svc.InstanceConn(ctx, li.ID, "postgres")
	if err != nil {
		return nil, &internalapi.CallError{Status: http.StatusServiceUnavailable, Code: "unavailable", Message: "The instance cannot be reached."}
	}
	defer func() { _ = c.Close(context.WithoutCancel(ctx)) }()
	var inRecovery bool
	if err := c.QueryRow(ctx, `SELECT pg_is_in_recovery()`).Scan(&inRecovery); err != nil {
		return nil, &internalapi.CallError{Status: http.StatusServiceUnavailable, Code: "unavailable", Message: "The instance cannot be asked."}
	}
	action := promoteAction(inRecovery, li.Role)
	if action == promoteRefuse {
		return nil, &internalapi.CallError{Status: http.StatusConflict, Code: "not_standby", Message: "The instance is not a standby and not a promoted one: its role file and its state do not fit."}
	}
	if action == promoteAlready {
		// Promoted by an earlier call: make sure what follows a promotion is done, and answer already.
		if err := d.Standby.agent.EmptySyncNames(ctx, li.ID); r.ReapplyList && err != nil {
			return nil, err
		}
		if !d.Registry.Leases().Holds(r.Group) {
			return nil, &internalapi.CallError{Status: http.StatusConflict, Code: "lease_not_held", Message: "This server does not hold the lease of the shard group."}
		}
		// F3-08: an instance that is writable and not guarded by the lease is not reported as promoted.
		if err := attachPromotedInjected(d, r.Group); err != nil {
			return nil, err
		}
		return promoteAnswer(ctx, c, "already", started), nil
	}
	// 0. A planned switchover makes its proof again here, in the call that promotes (F4-07): the standby must hold everything the old
	//    primary wrote up to its shutdown checkpoint, and nothing beyond it. A standby that is behind, or an old primary that was
	//    started again and wrote more, is refused before anything is changed: the call is still before the point of no return.
	if r.Checkpoint != "" && !r.ReapplyList {
		// A test: the standby has not replayed the shutdown checkpoint yet, for the first FailoverTimes calls (replication lag after the stop).
		if d.Fault.FailoverInjectionOnce("promote:behind") == "fail" {
			return nil, &internalapi.CallError{Status: http.StatusConflict, Code: "standby_behind", Message: "The standby does not hold everything the old primary wrote."}
		}
		vc, verr := d.Svc.InstanceConn(ctx, li.ID, dbOrSystem(""))
		if verr != nil {
			return nil, &internalapi.CallError{Status: http.StatusServiceUnavailable, Code: "unavailable", Message: "The proof cannot be asked: the instance cannot be reached."}
		}
		var ok bool
		verr = vc.QueryRow(ctx, `SELECT kingres.switchover_verify($1::pg_lsn, $2)`, r.Checkpoint, r.ClusterState).Scan(&ok)
		_ = vc.Close(context.WithoutCancel(ctx))
		if verr != nil {
			var pe *pgconn.PgError
			if errors.As(verr, &pe) && pe.Code == "KS006" {
				return nil, &internalapi.CallError{Status: http.StatusConflict, Code: "unprovable", Message: pe.Message}
			}
			return nil, &internalapi.CallError{Status: http.StatusServiceUnavailable, Code: "unavailable", Message: "The proof cannot be asked: " + verr.Error()}
		}
		// A test: the standby cannot prove it (the proof of the planned switchover of the map group is refused after the old leader stopped).
		if d.Fault.FailoverInjection("promote:unproven") == "fail" {
			ok = false
		}
		if !ok {
			return nil, &internalapi.CallError{Status: http.StatusConflict, Code: "standby_behind", Message: "The standby does not hold everything the old primary wrote."}
		}
	}
	// 1. The list of synchronous standbys is emptied first: a promoted standby inherits its primary's list and every
	//    commit on it would wait for ever (extension precondition 3). Harmless while the instance is in recovery.
	if err := d.Standby.agent.EmptySyncNames(ctx, li.ID); err != nil {
		return nil, err
	}
	if r.ReapplyList {
		return failover.PromoteAnswer{Status: "already"}, nil
	}
	// 2. The role file says primary before the instance is one, so that from the moment it is writable nothing treats it as
	//    a standby (it could be deleted or rebuilt); a crash between the two leaves a standby that runs without a lease
	//    and is stopped by the lease manager, which is the safe direction.
	term := int64(0)
	if r.ExpectedTerm > 0 {
		term = r.ExpectedTerm
	}
	if err := d.Svc.Manager().WriteRole(li.ID, instance.Role{Group: r.Group, Role: instance.RolePrimary, Origin: instance.OriginCopy, Term: term}); err != nil {
		return nil, err
	}
	// A test ends the call here (the role file is written, pg_promote was not called): the repeat must finish the promotion (F3-05).
	if d.Fault.FailoverInjectionOnce("promote:afterRoleFile") == "fail" {
		d.Log.Warn("failover: the promote call ends between the role file and pg_promote (injected by a test)", "group", r.Group)
		return nil, &internalapi.CallError{Status: http.StatusServiceUnavailable, Code: "unavailable", Message: "The call ended between the role file and the promotion (injected by a test)."}
	}
	// 3. The lease, checked as the last thing before the promotion with nothing slow between the two.
	if !d.Registry.Leases().Holds(r.Group) {
		// Put the role back: this instance is still a standby and no primary of the group runs here.
		_ = d.Svc.Manager().WriteRole(li.ID, li.Role)
		return nil, &internalapi.CallError{Status: http.StatusConflict, Code: "lease_not_held", Message: "This server does not hold the lease of the shard group."}
	}
	var promoted bool
	if err := c.QueryRow(ctx, `SELECT pg_promote(true, 60)`).Scan(&promoted); err != nil || !promoted {
		// The promotion may still be going on: the repeat asks the instance.
		return nil, &internalapi.CallError{Status: http.StatusServiceUnavailable, Code: "unavailable", Message: "The promotion did not finish within its bound; repeat the call."}
	}
	// 4. The lease guards the instance from now on, and the watchdog covers it.
	if err := attachPromotedInjected(d, r.Group); err != nil {
		d.Log.Warn("failover: attaching the lease to the promoted instance", "group", r.Group, "error", err)
		return nil, err
	}
	return promoteAnswer(ctx, c, "promoted", started), nil
}

func promoteAnswer(ctx context.Context, c *pgx.Conn, status string, started time.Time) failover.PromoteAnswer {
	out := failover.PromoteAnswer{Status: status, TookMs: time.Since(started).Milliseconds()}
	_ = c.QueryRow(ctx, `SELECT (SELECT timeline_id FROM pg_control_checkpoint())::bigint, pg_current_wal_lsn()::text`).Scan(&out.Timeline, &out.Lsn)
	return out
}

func handleFailoverPrepared(ctx context.Context, d *haDeps, body []byte) (any, error) {
	var r struct {
		Group    int32  `json:"group"`
		Database string `json:"database"`
	}
	if err := json.Unmarshal(body, &r); err != nil || r.Group < 0 {
		return nil, badBody("a prepared request")
	}
	if err := requireGroup(body, "a prepared request"); err != nil {
		return nil, err
	}
	li, ok := d.Svc.PrimaryOf(r.Group)
	if !ok {
		return nil, &internalapi.CallError{Status: http.StatusConflict, Code: "not_primary", Message: "This server runs no primary of the shard group."}
	}
	c, err := d.Svc.InstanceConn(ctx, li.ID, dbOrSystem(r.Database))
	if err != nil {
		return nil, &internalapi.CallError{Status: http.StatusServiceUnavailable, Code: "unavailable", Message: "The instance cannot be reached."}
	}
	defer func() { _ = c.Close(context.WithoutCancel(ctx)) }()
	if _, err := c.Exec(ctx, `SET SESSION CHARACTERISTICS AS TRANSACTION ISOLATION LEVEL READ COMMITTED`); err != nil {
		return nil, err
	}
	if _, err := c.Exec(ctx, `SELECT kingres.recover_prepared_transactions()`); err != nil {
		return nil, &internalapi.CallError{Status: http.StatusServiceUnavailable, Code: "unavailable", Message: "The recovery of prepared transactions did not finish; repeat the call."}
	}
	return struct{}{}, nil
}

// registerFailoverCallee registers the calls of the failover task on this server.
func registerFailoverCallee(d *haDeps) {
	reg := func(key string, h func(ctx context.Context, d *haDeps, body []byte) (any, error)) {
		d.Calls.Register(key, func(ctx context.Context, _ string, body []byte) (any, error) { return h(ctx, d, body) })
	}
	reg(callFailoverLease, handleFailoverLease)
	reg(callFailoverDemote, handleFailoverDemote)
	reg(callFailoverPromote, handleFailoverPromote)
	reg("POST /failover/prepared", handleFailoverPrepared)
	registerSwitchoverCallee(reg)
	// The two calls of the map group are open to every member: they are told who calls (F4-05).
	d.Calls.Register(callMapStart, func(ctx context.Context, caller string, body []byte) (any, error) {
		return handleMapStart(ctx, d, caller, body)
	})
	d.Calls.Register(callMapContinue, func(ctx context.Context, caller string, body []byte) (any, error) {
		return handleMapContinue(ctx, d, caller, body)
	})
	d.Calls.Register(callMapAbandon, func(ctx context.Context, caller string, body []byte) (any, error) {
		return handleMapAbandon(ctx, d, caller, body)
	})
	d.Calls.Register(callMapCheckpoint, func(ctx context.Context, caller string, body []byte) (any, error) {
		return handleMapCheckpoint(ctx, d, caller, body)
	})
	d.Calls.Register(callMapRefused, func(ctx context.Context, caller string, body []byte) (any, error) {
		return handleMapRefused(ctx, d, caller, body)
	})
	d.Calls.Register(callMapProof, func(ctx context.Context, caller string, body []byte) (any, error) {
		return handleMapProof(ctx, d, caller, body)
	})
	d.Calls.Register(callMapBootstrap, func(ctx context.Context, caller string, body []byte) (any, error) {
		return handleMapBootstrap(ctx, d, caller, body)
	})
	d.Calls.Register(callMapResume, func(ctx context.Context, caller string, body []byte) (any, error) {
		return handleMapResume(ctx, d, caller, body)
	})
}

var _ = url.Values{}

// catalogDatabases lists the databases that have a sharding catalog (kingres.node): every sharded database has one of
// its own, with its own node ids, besides the system database. withSystem puts the system database first.
func catalogDatabases(ctx context.Context, svc *cluster.Service, withSystem bool) ([]string, error) {
	pc, err := svc.ServiceConn(ctx, "postgres")
	if err != nil {
		return nil, err
	}
	rows, err := pc.Query(ctx, `SELECT datname FROM pg_catalog.pg_database WHERE NOT datistemplate AND datallowconn ORDER BY datname`)
	if err != nil {
		_ = pc.Close(context.WithoutCancel(ctx))
		return nil, err
	}
	var names []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			rows.Close()
			_ = pc.Close(context.WithoutCancel(ctx))
			return nil, err
		}
		names = append(names, n)
	}
	rows.Close()
	_ = pc.Close(context.WithoutCancel(ctx))
	var out []string
	if withSystem {
		out = append(out, sysdb.Database)
	}
	for _, n := range names {
		if n == sysdb.Database || n == "postgres" {
			continue
		}
		c, err := svc.ServiceConn(ctx, n)
		if err != nil {
			var pe *pgconn.PgError
			if errors.As(err, &pe) && pe.Code == "3D000" {
				continue
			}
			return nil, err
		}
		var has bool
		err = c.QueryRow(ctx, `SELECT to_regclass('kingres.node') IS NOT NULL`).Scan(&has)
		_ = c.Close(context.WithoutCancel(ctx))
		if err != nil {
			return nil, err
		}
		if has {
			out = append(out, n)
		}
	}
	return out, nil
}

// unreadableTerm is the answer of a call whose callee cannot read its catalog term. A call that stops or promotes an
// instance is refused then (F3-13): the leader repeats it, and a step that cannot get it done leaves the lease to fence.
// The other calls are not judged by a term that cannot be read.
func unreadableTerm(strict bool) error {
	if !strict {
		return nil
	}
	return &internalapi.CallError{Status: http.StatusServiceUnavailable, Code: "unavailable", Message: "The catalog term of this server cannot be read; repeat the call."}
}

// requireGroup refuses a body that does not name the group: a missing group reads as 0, the map group, and a
// destructive call must never default to it (F3-13).
func requireGroup(body []byte, what string) error {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(body, &m); err != nil {
		return badBody(what)
	}
	if _, ok := m["group"]; !ok {
		return &internalapi.CallError{Status: http.StatusBadRequest, Code: "bad_request", Message: "The body of " + what + " must name the group."}
	}
	return nil
}

// dbOrSystem is the database a prepared-transaction or check call acts in: the named one, the system database when none
// is named (an older leader).
func dbOrSystem(db string) string {
	if db == "" {
		return sysdb.Database
	}
	return db
}

// unobservedAge corrects the age of the last check of a group for a primary's server that this leader did not hear (F3-03,
// failover.UnobservedAge): the silences are asked of the other members, because the silence of the leader alone says
// nothing about the server.
func (p *foPlanner) unobservedAge(ctx context.Context, primary string, plain time.Duration) time.Duration {
	if primary == p.self {
		return plain
	}
	members, err := p.Members(ctx)
	if err != nil {
		return plain
	}
	var silences []time.Duration
	if s, ok := p.reg.Liveness().Silence(primary); ok {
		silences = append(silences, s)
	}
	for _, m := range members {
		if m == p.self || m == primary {
			continue
		}
		cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		var ans LivenessAnswer
		err := p.client.Do(cctx, m, "GET", "/liveness", nil, &ans)
		cancel()
		if err != nil {
			continue
		}
		for _, lp := range ans.Peers {
			if lp.Server == primary && lp.SilenceMs != nil {
				silences = append(silences, time.Duration(*lp.SilenceMs)*time.Millisecond)
			}
		}
	}
	peers := &foPeers{self: p.self, calls: p.calls, client: p.client}
	ls, err := peers.LeaseSettings(ctx, p.self)
	if err != nil {
		return plain
	}
	return failover.UnobservedAge(plain, silences, len(members), time.Duration(ls.DurationMs)*time.Millisecond, time.Duration(ls.MarginMs)*time.Millisecond)
}

// What the promote call does for an instance, from its state and its role file.
const (
	promoteRefuse  = "refuse"  // not a standby and not a promoted instance
	promoteAlready = "already" // out of recovery with the role file primary: an earlier call promoted it
	promoteDo      = "promote" // a standby, or a promotion that an earlier call started and did not finish
)

// promoteAction is the rule of the promote call (F3-05). A role file that says primary on an instance that is still in
// recovery is a promotion that an earlier call began (it wrote the role file, and ended before pg_promote answered): it is
// finished, not refused, or no later call could ever promote that standby.
func promoteAction(inRecovery bool, role instance.Role) string {
	switch {
	case !inRecovery && role.Writable():
		return promoteAlready
	case !inRecovery:
		return promoteRefuse
	case role.IsStandby() || role.Writable():
		return promoteDo
	}
	return promoteRefuse
}

// attacher is what the promote call needs of the lease manager.
type attacher interface{ Attach(group int32) error }

// attachPromoted makes the lease guard the promoted instance (the watchdog covers it from then on). An instance that is
// writable and not guarded is not reported as promoted (F3-08): the answer is lease_not_held, the step hands the lease over
// again and repeats, and the repeat (the instance is out of recovery) attaches or fails.
func attachPromoted(m attacher, group int32) error {
	if err := m.Attach(group); err != nil {
		return &internalapi.CallError{Status: http.StatusConflict, Code: "lease_not_held", Message: "The lease of the shard group could not be attached to the promoted instance: " + err.Error()}
	}
	return nil
}

// attachPromotedInjected is attachPromoted, with the failure that a test injects (a lease lost between the check and the attach, F3-08).
func attachPromotedInjected(d *haDeps, group int32) error {
	if d.Fault.FailoverInjectionOnce("promote:attach") == "fail" {
		d.Log.Warn("failover: the attach of the lease fails (injected by a test)", "group", group)
		return attachPromoted(failingAttacher{}, group)
	}
	return attachPromoted(d.LeaseMgr, group)
}

type failingAttacher struct{}

func (failingAttacher) Attach(int32) error {
	return errors.New("lease: the lease is not held (injected by a test)")
}
