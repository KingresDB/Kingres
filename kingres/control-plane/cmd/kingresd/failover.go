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
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/audit"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/cluster"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/failover"
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
type failoverDeps struct{ d *haDeps }

// newFailover builds the failover service of the manager leader: the ports of the task over
// the peer client, the extension's functions and the records of the leader.
func newFailover(svc *cluster.Service, o *ops.Ops, m *membership.Manager, stby *standbyParts, reg *ha.Registry, calls *callTable, self string,
	log *slog.Logger, auditFn func(context.Context, audit.Event), fd *failoverDeps) *failover.Service {
	cat := &foCatalog{svc: svc, log: log}
	pl := &foPlanner{svc: svc, self: self, reg: reg, stby: stby, cat: cat, log: log, audit: auditFn, calls: calls, client: m.Client}
	peers := &foPeers{self: self, calls: calls, client: m.Client}
	r := &failover.Runner{Peers: peers, Cat: cat, Plan: pl, Log: log,
		NewRows: func(id int64) failover.Rows { return &failover.PGRows{Conns: svc, TaskID: id} }}
	fs := &failover.Service{Runner: r, Conns: svc, Engine: o.Engine, Log: log, Audit: auditFn}
	pl.svcRef = fs
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

func (p *foPeers) RecoverPrepared(ctx context.Context, server string, group int32) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	return p.do(ctx, server, "POST", "/failover/prepared", struct {
		Group int32 `json:"group"`
	}{group}, nil)
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

func (c *foCatalog) RecoverPrepared(ctx context.Context) error {
	conn, err := c.session(ctx, sysdb.Database)
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

func (c *foCatalog) CheckCluster(ctx context.Context) ([]failover.Incident, error) {
	conn, err := c.session(ctx, sysdb.Database)
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
}

func (p *foPlanner) sys(ctx context.Context, f func(c *pgx.Conn) error) error {
	c, err := p.svc.ServiceConn(ctx, sysdb.Database)
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
		var lsn *string
		var seen time.Time
		var primary string
		err := c.QueryRow(ctx, `SELECT coalesce(sync_streaming, false), coalesce(without_sync, false), flush_lsn, seen_at, coalesce(primary_server, '') FROM kingres_cp.group_replication_state WHERE group_id = $1`, group).
			Scan(&streaming, &without, &lsn, &seen, &primary)
		if err == nil {
			f.SyncStreaming = streaming && !without
			// The age that counts is the age of the last check at the moment the primary was last heard, not now: a failover
			// takes longer than the check's validity, and the check stops when the primary's server stops answering.
			f.LastKnownAge = time.Since(seen)
			if primary != "" && primary != p.self {
				if silence, ok := p.reg.Liveness().Silence(primary); ok && silence > 0 && silence < f.LastKnownAge {
					f.LastKnownAge -= silence
				}
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

func (p *foPlanner) AfterPromotion(ctx context.Context, group int32, promoted failover.StandbyRec, newServer string, newPort int) error {
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

func staleTerm(ctx context.Context, d *haDeps, expected int64) error {
	if expected < 0 {
		return nil // the caller does not know the term (a negative value says so); 0 is the term of a cluster that never failed over
	}
	c, err := d.Svc.SystemConn(ctx)
	if err != nil {
		d.Log.Warn("failover: the catalog term of this server cannot be read; the call is not judged by it", "error", err)
		return nil
	}
	defer func() { _ = c.Close(context.WithoutCancel(ctx)) }()
	var term int64
	if err := c.QueryRow(ctx, `SELECT term FROM kingres.catalog_status`).Scan(&term); err != nil {
		d.Log.Warn("failover: the catalog term of this server cannot be read; the call is not judged by it", "error", err)
		return nil
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
	if d.LeaseMgr == nil {
		return nil, notStarted
	}
	if r.Role == failover.LeaseRelease {
		d.LeaseMgr.Unadopt(r.Group)
		return failover.LeaseAnswer{}, nil
	}
	if r.Role != failover.LeaseFence && r.Role != failover.LeaseHolder {
		return nil, badBody("a lease request with a known role")
	}
	if err := staleTerm(ctx, d, r.ExpectedTerm); err != nil {
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
	if d.LeaseMgr == nil {
		return nil, notStarted
	}
	if err := staleTerm(ctx, d, r.ExpectedTerm); err != nil {
		return nil, err
	}
	li, ok := d.Svc.PrimaryOf(r.Group)
	if !ok {
		return nil, &internalapi.CallError{Status: http.StatusConflict, Code: "not_primary", Message: "This server runs no primary of the shard group."}
	}
	// No more renewals, and the marker that keeps the primary from starting here again, before anything is stopped.
	if err := d.LeaseMgr.Relinquish(r.Group); err != nil {
		return nil, err
	}
	out := failover.DemoteAnswer{}
	stopCtx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	if err := d.Svc.Manager().StopImmediate(stopCtx, li.ID); err != nil {
		d.Log.Warn("failover: stopping the old primary", "group", r.Group, "instance", li.ID, "error", err)
	}
	running, err := d.Svc.Manager().Running(ctx, li.ID)
	out.Stopped = err == nil && !running
	if out.Stopped && r.TransferTo != "" {
		res, terr := d.LeaseMgr.Transfer(ctx, r.Group, r.TransferTo)
		out.Transferred = terr == nil && res.Granted >= res.Need
		if terr != nil {
			d.Log.Warn("failover: the lease was not handed over; it runs out by itself", "group", r.Group, "error", terr)
		}
	}
	return out, nil
}

func handleFailoverPromote(ctx context.Context, d *haDeps, body []byte) (any, error) {
	var r failover.PromoteRequest
	if err := json.Unmarshal(body, &r); err != nil || r.Group < 0 {
		return nil, badBody("a promote request")
	}
	if d.LeaseMgr == nil || d.Standby == nil {
		return nil, notStarted
	}
	started := time.Now()
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
	if err := staleTerm(ctx, d, r.ExpectedTerm); err != nil {
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
	if !inRecovery {
		// Promoted by an earlier call: make sure what follows a promotion is done, and answer already.
		if !li.Role.Writable() {
			return nil, &internalapi.CallError{Status: http.StatusConflict, Code: "not_standby", Message: "The instance is out of recovery and its role file does not say primary."}
		}
		if err := d.Standby.agent.EmptySyncNames(ctx, li.ID); r.ReapplyList && err != nil {
			return nil, err
		}
		if !d.Registry.Leases().Holds(r.Group) {
			return nil, &internalapi.CallError{Status: http.StatusConflict, Code: "lease_not_held", Message: "This server does not hold the lease of the shard group."}
		}
		_ = d.LeaseMgr.Attach(r.Group)
		return promoteAnswer(ctx, c, "already", started), nil
	}
	if !li.Role.IsStandby() {
		return nil, &internalapi.CallError{Status: http.StatusConflict, Code: "not_standby", Message: "The instance is not a standby."}
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
	if err := d.LeaseMgr.Attach(r.Group); err != nil {
		d.Log.Warn("failover: attaching the lease to the promoted instance", "group", r.Group, "error", err)
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
		Group int32 `json:"group"`
	}
	if err := json.Unmarshal(body, &r); err != nil || r.Group < 0 {
		return nil, badBody("a prepared request")
	}
	li, ok := d.Svc.PrimaryOf(r.Group)
	if !ok {
		return nil, &internalapi.CallError{Status: http.StatusConflict, Code: "not_primary", Message: "This server runs no primary of the shard group."}
	}
	c, err := d.Svc.InstanceConn(ctx, li.ID, sysdb.Database)
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
