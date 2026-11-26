// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/api"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/gather"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/internalapi"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/monitor"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/ops"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/peerclient"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/problem"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/serverinfo"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/sysdb"
)

// Cluster-wide reads (contract 0.5.0). listLogs, listAlerts and queryMetrics
// answer for the whole cluster from any server: the server that receives the
// request authenticates the caller and checks the role as for any request, asks
// every other server in parallel for its part over POST /internal/v1/gather
// (each with its own deadline), merges the answers with its own and names the
// servers that did not answer (`partial`, `missingServers`). The answering
// server runs the same operation through the same pipeline, with the identity the
// leader's assertion vouches for: the role is derived again from the answering
// server's own catalog (CP-07), so a server cannot ask for more than the caller
// may read. Paging uses keyset cursors over one total order, so a page boundary
// is the same whichever servers answered.

// Gatherer is what the HTTP layer needs to ask the other servers.
type Gatherer interface {
	// Self is this server (its id and ordinal).
	Self(ctx context.Context) (gather.Peer, error)
	// Others lists the other servers of the cluster.
	Others(ctx context.Context) ([]gather.Peer, error)
	// Ask sends a gather call to a server and returns its answer.
	Ask(ctx context.Context, server string, req internalapi.ForwardRequest, max int64) (internalapi.ForwardResponse, error)
}

type gatherKey struct{}

// gathered marks a request that a server of the cluster made on behalf of a
// caller (see ServeGathered); ordinal is this server's ordinal.
type gathered struct {
	peer    string
	ordinal int
}

func gatheredOf(r *http.Request) *gathered {
	g, _ := r.Context().Value(gatherKey{}).(*gathered)
	return g
}

// The operations a server may ask another to run in the cluster-wide mode.
var gatherable = map[string]bool{"listLogs": true, "listAlerts": true, "queryMetrics": true, "acknowledgeAlert": true, "listAuditEvents": true}

const (
	// maxGatherAnswer bounds what one server may answer to a gather call.
	maxGatherAnswer = 16 << 20
	// maxLogMessage bounds one log message in a gathered answer.
	maxLogMessage = 8 << 10
	// maxPeerSeries bounds the number of series a server may answer for one metric.
	maxPeerSeries = 2000
)

// ServeGathered runs one of the cluster-wide reads (or the acknowledgement of an
// alert that a server holds) on this server for the identity the asking server
// vouches for, through the whole pipeline of a public request.
func (s *Server) ServeGathered(ctx context.Context, peer string, fr internalapi.ForwardRequest) (internalapi.ForwardResponse, error) {
	op, found := operationByID(fr.OperationID)
	if !found || !gatherable[op.ID] {
		return internalapi.ForwardResponse{}, errors.New("operation " + strconv.Quote(fr.OperationID) + " is not gathered")
	}
	if err := validateIdentity(fr.Identity); err != nil {
		return internalapi.ForwardResponse{}, err
	}
	req, err := s.operationRequest(ctx, peer, fr, op)
	if err != nil {
		return internalapi.ForwardResponse{}, err
	}
	req = req.WithContext(context.WithValue(req.Context(), gatherKey{}, &gathered{peer: peer, ordinal: fr.Ordinal}))
	return s.serveRequest(req, maxGatherAnswer), nil
}

func (s *Server) gatherTimeout() time.Duration {
	if s.deps.GatherTimeout > 0 {
		return s.deps.GatherTimeout
	}
	return 3 * time.Second
}

// otherServers returns the servers to ask: all the others, or only the one the
// request names (none when it names this server or one that is not a member).
func (s *Server) otherServers(ctx context.Context, self *string, only string) ([]gather.Peer, error) {
	g := s.deps.Gatherer
	if g == nil {
		return nil, nil
	}
	if only != "" && self != nil && *self == only {
		return nil, nil
	}
	others, err := g.Others(ctx)
	if err != nil {
		return nil, err
	}
	if only == "" {
		return others, nil
	}
	for _, p := range others {
		if p.ID == only {
			return []gather.Peer{p}, nil
		}
	}
	return nil, nil
}

// askAll sends the same gather call (this operation with the caller's query) to
// every server in peers in parallel and returns the outcomes.
func (s *Server) askAll(c *call, r *http.Request, op string, peers []gather.Peer, maxBytes int64) []gather.Outcome {
	if len(peers) == 0 {
		return nil
	}
	g := s.deps.Gatherer
	base := internalapi.ForwardRequest{
		OperationID: op, Query: r.URL.RawQuery, RequestID: stateOf(r).id, SourceAddress: c.ip, Source: "panel",
		Identity: identityOf(c.session),
	}
	return gather.Fanout(r.Context(), peers, s.gatherTimeout(), func(ctx context.Context, p gather.Peer) ([]byte, error) {
		req := base
		req.Ordinal = p.Ordinal
		resp, err := g.Ask(ctx, p.ID, req, maxBytes)
		if err != nil {
			return nil, err
		}
		if resp.Status != http.StatusOK {
			s.log.Warn("a server refused its part of a cluster-wide read", "request_id", stateOf(r).id, "server", p.ID, "operation", op, "status", resp.Status)
			return nil, gather.ErrRefused
		}
		return resp.Body, nil
	})
}

func missingToAPI(m []gather.Missing) []api.MissingServer {
	if len(m) == 0 {
		return nil
	}
	out := make([]api.MissingServer, len(m))
	for i, x := range m {
		out[i] = api.MissingServer{ServerID: peerclient.CleanText(x.ServerID, 255), Reason: x.Reason}
	}
	return out
}

func invalidCursor() *problem.Problem {
	return problem.Validation("The cursor is not valid.", problem.FieldError{Field: "cursor", Message: "not a cursor issued by this server", Code: "invalid_cursor"})
}

// ------------------------------------------------------------------- logs

type logFilter struct {
	level, q string
	from, to time.Time
	serverID string
	limit    int
	after    *gather.LogKey
}

// severity orders the levels: a filter keeps the entries at that level or worse.
var severity = map[string]int{"debug": 0, "info": 1, "warning": 2, "error": 3}

func (s *Server) parseLogFilter(r *http.Request) (logFilter, *problem.Problem) {
	pp := newParams(r)
	f := logFilter{}
	f.level = pp.enum("level", "debug", "info", "warning", "error")
	component := pp.enum("component", "api", "agent", "manager", "router", "etcd", "patroni", "postgres", "metrics")
	f.from, f.to = pp.timeQuery("from"), pp.timeQuery("to")
	f.q = strings.ToLower(pp.maxLen("q", 200))
	if id := pp.serverIDQuery("serverId"); id != nil {
		f.serverID = *id
	}
	page := pp.page()
	f.limit = page.Limit
	if p := pp.err(); p != nil {
		return f, p
	}
	if page.Cursor != "" {
		k, err := gather.DecodeLogCursor(page.Cursor)
		if err != nil {
			return f, invalidCursor()
		}
		f.after = &k
	}
	if component != "" && component != "postgres" {
		f.limit = -1 // only the instance log exists in this version: no entries
	}
	return f, nil
}

// localRankedLogs reads the log of this server's instance, filters and ranks it.
func (s *Server) localRankedLogs(r *http.Request, f logFilter, self *string) ([]gather.RankedLog, error) {
	var dir string
	if s.deps.LogDir != nil {
		dir = s.deps.LogDir()
	}
	if dir == "" || f.limit < 0 {
		return nil, nil
	}
	all, err := serverinfo.ReadLogs(dir, self, 1)
	if err != nil {
		s.log.Error("reading the instance log", "request_id", stateOf(r).id, "error", err)
		return nil, problem.New(problem.InternalError, "The log could not be read.")
	}
	var sel []api.LogEntry
	for _, e := range all {
		if f.level != "" && severity[e.Level] < severity[f.level] {
			continue
		}
		if (!f.from.IsZero() && e.Timestamp.Before(f.from)) || (!f.to.IsZero() && e.Timestamp.After(f.to)) {
			continue
		}
		if f.q != "" && !strings.Contains(strings.ToLower(e.Message), f.q) {
			continue
		}
		if len(e.Message) > maxLogMessage {
			e.Message = e.Message[:maxLogMessage]
		}
		sel = append(sel, e)
	}
	return gather.RankLogs(sel), nil
}

type gatheredLogs struct {
	Items []gather.RankedLog `json:"items"`
}

var logComponents = map[string]bool{"api": true, "agent": true, "manager": true, "router": true, "etcd": true, "patroni": true, "postgres": true, "metrics": true}

// cleanPeerLogs makes the log entries a server answered safe: they are about
// that server only, their text is bounded and without control characters.
func cleanPeerLogs(p gather.Peer, in []gather.RankedLog, max int) []gather.RankedLog {
	out := make([]gather.RankedLog, 0, min(len(in), max))
	for _, rl := range in {
		if len(out) >= max {
			break
		}
		e := rl.Entry
		if e.Timestamp.IsZero() || rl.Rank < 0 {
			continue
		}
		id := p.ID
		e.ServerID = &id
		e.Message = peerclient.CleanText(e.Message, maxLogMessage)
		if _, ok := severity[e.Level]; !ok {
			e.Level = "info"
		}
		if !logComponents[e.Component] {
			e.Component = "postgres"
		}
		out = append(out, gather.RankedLog{Entry: e, Rank: rl.Rank})
	}
	return out
}

// logsPage answers listLogs (cluster-wide) and listThisServerLogs (this server).
func (s *Server) logsPage(c *call, w http.ResponseWriter, r *http.Request, clusterWide bool) error {
	f, perr := s.parseLogFilter(r)
	if perr != nil {
		return perr
	}
	self := s.localServerID(r.Context())
	if g := gatheredOf(r); g != nil {
		local, err := s.localRankedLogs(r, f, self)
		if err != nil {
			return err
		}
		s.writeJSON(w, r, http.StatusOK, gatheredLogs{Items: gather.AfterLog(local, f.after, max(f.limit, 0)+1)})
		return nil
	}
	var lists [][]gather.RankedLog
	var missing []gather.Missing
	wantSelf := !clusterWide || f.serverID == "" || (self != nil && *self == f.serverID)
	if wantSelf {
		local, err := s.localRankedLogs(r, f, self)
		if err != nil {
			return err
		}
		lists = append(lists, gather.AfterLog(local, f.after, max(f.limit, 0)+1))
	}
	if clusterWide && f.limit >= 0 {
		only := f.serverID
		peers, err := s.otherServers(r.Context(), self, only)
		if err != nil {
			s.log.Warn("the list of servers cannot be read; answering for this server only", "request_id", stateOf(r).id, "error", err)
		}
		out := s.askAll(c, r, "listLogs", peers, maxGatherAnswer)
		for _, o := range out {
			if o.Err != nil {
				continue
			}
			var gl gatheredLogs
			if json.Unmarshal(o.Body, &gl) != nil {
				missing = append(missing, gather.Missing{ServerID: o.Peer.ID, Reason: gather.ReasonInvalid})
				continue
			}
			lists = append(lists, cleanPeerLogs(o.Peer, gl.Items, f.limit+1))
		}
		missing = append(missing, gather.MissingOf(out)...)
	}
	items, next := gather.MergeLogs(lists, f.after, max(f.limit, 0))
	page := api.LogPage{Items: items, Partial: len(missing) > 0, MissingServers: missingToAPI(missing)}
	if f.limit < 0 {
		page.Items = []api.LogEntry{}
	}
	if next != nil {
		cur := gather.EncodeLogCursor(*next)
		page.NextCursor = &cur
	}
	s.writeJSON(w, r, http.StatusOK, page)
	return nil
}

func (s *Server) listThisServerLogs(c *call, w http.ResponseWriter, r *http.Request) error {
	return s.logsPage(c, w, r, false)
}

func (s *Server) listLogs(c *call, w http.ResponseWriter, r *http.Request) error {
	return s.logsPage(c, w, r, true)
}

// ------------------------------------------------------------------ alerts

// selfOrdinal is this server's ordinal for alert ids (0 without a cluster).
func (s *Server) selfOrdinal(ctx context.Context) int {
	if s.deps.Gatherer == nil {
		return 0
	}
	p, err := s.deps.Gatherer.Self(ctx)
	if err != nil {
		return 0
	}
	return p.Ordinal
}

type gatheredAlerts struct {
	Items []api.Alert `json:"items"`
}

func cleanPeerAlerts(p gather.Peer, in []api.Alert, max int) []api.Alert {
	out := make([]api.Alert, 0, min(len(in), max))
	for _, a := range in {
		if len(out) >= max {
			break
		}
		// An alert of a server has an id of that server's ordinal.
		if ord, _ := gather.SplitAlertID(a.ID); ord != p.Ordinal || a.RaisedAt.IsZero() {
			continue
		}
		a.Title = peerclient.CleanText(a.Title, 200)
		a.Message = peerclient.CleanText(a.Message, 2000)
		a.Kind = peerclient.CleanText(a.Kind, 64)
		if a.ServerID != nil {
			v := peerclient.CleanText(*a.ServerID, 255)
			a.ServerID = &v
		}
		if a.DatabaseName != nil {
			v := peerclient.CleanText(*a.DatabaseName, 63)
			a.DatabaseName = &v
		}
		if a.AcknowledgedBy != nil {
			v := peerclient.CleanText(*a.AcknowledgedBy, 63)
			a.AcknowledgedBy = &v
		}
		out = append(out, a)
	}
	return out
}

func (s *Server) listAlerts(c *call, w http.ResponseWriter, r *http.Request) error {
	o, err := s.needOps()
	if err != nil {
		return err
	}
	pp := newParams(r)
	state := pp.enum("state", "open", "acknowledged", "resolved")
	severity := pp.enum("severity", "info", "warning", "critical")
	page := pp.page()
	if p := pp.err(); p != nil {
		return p
	}
	var after *gather.AlertKey
	if page.Cursor != "" {
		k, err := gather.DecodeAlertCursor(page.Cursor)
		if err != nil {
			return invalidCursor()
		}
		after = &k
	}
	ordinal := s.selfOrdinal(r.Context())
	if g := gatheredOf(r); g != nil {
		ordinal = g.ordinal
	}
	conn, err := o.Service.ServiceConn(r.Context(), sysdb.Database)
	if err != nil {
		return s.storeProblem(r, errors.Join(errUnavailable, err), "")
	}
	defer func() { _ = conn.Close(context.WithoutCancel(r.Context())) }() //nolint:contextcheck // closing must outlive the request context
	local, err := monitor.ListAfter(r.Context(), conn, state, severity, after, page.Limit+1, ordinal)
	if err != nil {
		s.log.Error("listing alerts", "request_id", stateOf(r).id, "error", err)
		return problem.New(problem.InternalError, "The alerts could not be read.")
	}
	if gatheredOf(r) != nil {
		s.writeJSON(w, r, http.StatusOK, gatheredAlerts{Items: local})
		return nil
	}
	lists := [][]api.Alert{local}
	var missing []gather.Missing
	if s.deps.Gatherer != nil {
		self := s.localServerID(r.Context())
		peers, err := s.otherServers(r.Context(), self, "")
		if err != nil {
			s.log.Warn("the list of servers cannot be read; answering for this server only", "request_id", stateOf(r).id, "error", err)
		}
		out := s.askAll(c, r, "listAlerts", peers, maxGatherAnswer)
		for _, oc := range out {
			if oc.Err != nil {
				continue
			}
			var ga gatheredAlerts
			if json.Unmarshal(oc.Body, &ga) != nil {
				missing = append(missing, gather.Missing{ServerID: oc.Peer.ID, Reason: gather.ReasonInvalid})
				continue
			}
			lists = append(lists, cleanPeerAlerts(oc.Peer, ga.Items, page.Limit+1))
		}
		missing = append(missing, gather.MissingOf(out)...)
	}
	items, next := gather.MergeAlerts(lists, after, page.Limit)
	res := api.AlertPage{Items: items, Partial: len(missing) > 0, MissingServers: missingToAPI(missing)}
	if next != nil {
		cur := gather.EncodeAlertCursor(*next)
		res.NextCursor = &cur
	}
	s.writeJSON(w, r, http.StatusOK, res)
	return nil
}

// acknowledgeAlert marks an alert acknowledged. The alert lives on the server
// that raised it, which the ordinal in its id names: this server acknowledges it
// itself, or asks that server to (it audits the acknowledgement).
func (s *Server) acknowledgeAlert(c *call, w http.ResponseWriter, r *http.Request) error {
	o, err := s.needOps()
	if err != nil {
		return err
	}
	pp := newParams(r)
	id := pp.pathInt64("alertId")
	if p := pp.err(); p != nil {
		return p
	}
	ordinal, local := gather.SplitAlertID(id)
	if g := gatheredOf(r); g != nil {
		// Asked by another server: the id is the local one.
		return s.ackLocal(c, w, r, o, id, g.ordinal)
	}
	selfOrd := s.selfOrdinal(r.Context())
	if s.deps.Gatherer == nil || ordinal == 0 || ordinal == selfOrd {
		return s.ackLocal(c, w, r, o, local, selfOrd)
	}
	if id <= 0 {
		return problem.New(problem.NotFound, "No such alert.")
	}
	others, err := s.deps.Gatherer.Others(r.Context())
	if err != nil {
		return problem.New(problem.ServiceUnavailable, "The servers of the cluster cannot be listed now.").WithRetryAfter(5)
	}
	for _, p := range others {
		if p.Ordinal != ordinal {
			continue
		}
		ctx, cancel := context.WithTimeout(r.Context(), s.gatherTimeout())
		defer cancel()
		resp, err := s.deps.Gatherer.Ask(ctx, p.ID, internalapi.ForwardRequest{
			OperationID: "acknowledgeAlert", PathParams: map[string]string{"alertId": strconv.FormatInt(local, 10)},
			RequestID: stateOf(r).id, SourceAddress: c.ip, Source: "panel", Ordinal: p.Ordinal,
			Identity: identityOf(c.session),
		}, maxGatherAnswer)
		if err != nil {
			return problem.New(problem.ServiceUnavailable, "The server that holds this alert cannot be reached.").WithRetryAfter(5)
		}
		// That server audited it; this one must not do it again.
		c.audit = false
		if resp.Status == http.StatusOK {
			var a api.Alert
			if json.Unmarshal(resp.Body, &a) != nil {
				return problem.New(problem.ServiceUnavailable, "The server that holds this alert answered something else.")
			}
			cleaned := cleanPeerAlerts(p, []api.Alert{a}, 1)
			if len(cleaned) == 0 {
				return problem.New(problem.ServiceUnavailable, "The server that holds this alert answered something else.")
			}
			s.writeJSON(w, r, http.StatusOK, cleaned[0])
			return nil
		}
		return peerProblem(resp)
	}
	return problem.New(problem.NotFound, "No such alert.")
}

// peerProblem turns the error answer of another server into a problem: its
// status and code when they are well formed, nothing else of its text.
func peerProblem(resp internalapi.ForwardResponse) *problem.Problem {
	var p struct {
		Code problem.Code `json:"code"`
	}
	_ = json.Unmarshal(resp.Body, &p)
	switch {
	case resp.Status == http.StatusNotFound:
		return problem.New(problem.NotFound, "No such alert.")
	case resp.Status == http.StatusConflict:
		return problem.New(problem.Conflict, "The alert is already resolved.")
	case resp.Status == http.StatusForbidden:
		return problem.New(problem.Forbidden, "This operation requires a higher cluster role.")
	}
	return problem.New(problem.ServiceUnavailable, "The server that holds this alert could not acknowledge it.").WithRetryAfter(5)
}

func (s *Server) ackLocal(c *call, w http.ResponseWriter, r *http.Request, o *ops.Ops, local int64, ordinal int) error {
	conn, err := o.Service.ServiceConn(r.Context(), sysdb.Database)
	if err != nil {
		return s.storeProblem(r, errors.Join(errUnavailable, err), "")
	}
	defer func() { _ = conn.Close(context.WithoutCancel(r.Context())) }() //nolint:contextcheck // closing must outlive the request context
	a, err := monitor.Acknowledge(r.Context(), conn, local, c.session.Username, s.now())
	switch {
	case errors.Is(err, monitor.ErrNotFound):
		return problem.New(problem.NotFound, "No such alert.")
	case errors.Is(err, monitor.ErrResolved):
		return problem.New(problem.Conflict, "The alert is already resolved.")
	case err != nil:
		s.log.Error("acknowledging an alert", "request_id", stateOf(r).id, "error", err)
		return problem.New(problem.InternalError, "An unexpected error occurred.")
	}
	a.ID = gather.AlertID(ordinal, a.ID)
	s.writeJSON(w, r, http.StatusOK, a)
	return nil
}

// ----------------------------------------------------------------- metrics

type gatheredSeries struct {
	Server   string            `json:"server,omitempty"`
	Database string            `json:"database,omitempty"`
	Points   []api.MetricPoint `json:"points"`
}

type gatheredMetrics struct {
	Series []gatheredSeries `json:"series"`
}

func (s *Server) queryMetrics(c *call, w http.ResponseWriter, r *http.Request) error {
	if s.deps.Metrics == nil {
		return problem.New(problem.UnsupportedOp, "Metrics are not collected on this server (no managed instance).")
	}
	pp := newParams(r)
	names := make([]string, 0, len(metricUnits))
	for k := range metricUnits {
		names = append(names, k)
	}
	metric := pp.enum("metric", names...)
	from, to := pp.timeQuery("from"), pp.timeQuery("to")
	for _, f := range []string{"metric", "from", "to", "step"} {
		if r.URL.Query().Get(f) == "" {
			pp.fail(f, "is required", "required")
		}
	}
	step, err := strconv.Atoi(r.URL.Query().Get("step"))
	if r.URL.Query().Get("step") != "" && (err != nil || step < 1 || step > 86400) {
		pp.fail("step", "must be 1 to 86400", "out_of_range")
	}
	server := pp.serverIDQuery("serverId")
	database := pp.maxLen("databaseName", 63)
	groupBy := pp.enum("groupBy", "server", "database")
	if p := pp.err(); p != nil {
		return p
	}
	if !to.After(from) {
		return problem.Validation("The time range is invalid.", problem.FieldError{Field: "to", Message: "must be after from", Code: "out_of_range"})
	}
	srv := ""
	if server != nil {
		srv = *server
	}
	mu := metricUnits[metric]
	self := s.localServerID(r.Context())
	n := monitor.NumPoints(from, to, step)
	wantSelf := gatheredOf(r) != nil || srv == "" || (self != nil && *self == srv)
	var series []monitor.Result
	var missing []gather.Missing
	if wantSelf {
		raw, err := s.deps.Metrics.Raw(metric, from, to, step, "", database)
		if errors.Is(err, monitor.ErrTooManyPoints) {
			return problem.New(problem.Unprocessable, err.Error())
		}
		for _, rs := range raw {
			// The server-scoped series of this server only (the ring holds nothing else).
			if srv != "" && rs.Series.ServerID != "" && rs.Series.ServerID != srv {
				continue
			}
			series = append(series, rs)
		}
	} else if srv != "" && s.deps.Gatherer == nil {
		series = nil
	}
	if gatheredOf(r) != nil {
		out := gatheredMetrics{Series: make([]gatheredSeries, 0, len(series))}
		for _, rs := range series {
			gs := gatheredSeries{Server: rs.Series.ServerID, Database: rs.Series.Database, Points: make([]api.MetricPoint, len(rs.Points))}
			for i, p := range rs.Points {
				gs.Points[i] = api.MetricPoint{Timestamp: p.T.UTC(), Value: p.V}
			}
			out.Series = append(out.Series, gs)
		}
		s.writeJSON(w, r, http.StatusOK, out)
		return nil
	}
	if s.deps.Gatherer != nil {
		peers, err := s.otherServers(r.Context(), self, srv)
		if err != nil {
			s.log.Warn("the list of servers cannot be read; answering for this server only", "request_id", stateOf(r).id, "error", err)
		}
		outs := s.askAll(c, r, "queryMetrics", peers, maxGatherAnswer)
		for _, o := range outs {
			if o.Err != nil {
				continue
			}
			got, ok := cleanPeerSeries(o.Peer, o.Body, metric, from, step, n, database)
			if !ok {
				missing = append(missing, gather.Missing{ServerID: o.Peer.ID, Reason: gather.ReasonInvalid})
				continue
			}
			series = append(series, got...)
		}
		missing = append(missing, gather.MissingOf(outs)...)
	}
	res := monitor.Assemble(from, step, n, series, srv, database, groupBy, mu.sum)
	out := api.MetricQueryResult{Metric: metric, Unit: mu.unit, From: from.UTC(), To: to.UTC(), Step: step, Series: []api.MetricSeries{},
		Partial: len(missing) > 0, MissingServers: missingToAPI(missing)}
	for _, rs := range res {
		ser := api.MetricSeries{Labels: map[string]string{}, Points: make([]api.MetricPoint, len(rs.Points))}
		if rs.Series.ServerID != "" {
			ser.Labels["serverId"] = rs.Series.ServerID
		}
		if rs.Series.Database != "" {
			ser.Labels["databaseName"] = rs.Series.Database
		}
		for i, p := range rs.Points {
			ser.Points[i] = api.MetricPoint{Timestamp: p.T.UTC(), Value: p.V}
		}
		out.Series = append(out.Series, ser)
	}
	s.writeJSON(w, r, http.StatusOK, out)
	return nil
}

// cleanPeerSeries reads the series a server answered: at most maxPeerSeries,
// each with exactly n points of the same steps as asked for, the server label
// forced to the answering server and the database label bounded; ok is false
// when the answer is not well formed.
func cleanPeerSeries(p gather.Peer, body []byte, metric string, from time.Time, step, n int, database string) ([]monitor.Result, bool) {
	var gm gatheredMetrics
	if json.Unmarshal(body, &gm) != nil || len(gm.Series) > maxPeerSeries {
		return nil, false
	}
	out := make([]monitor.Result, 0, len(gm.Series))
	for _, gs := range gm.Series {
		if len(gs.Points) != n {
			return nil, false
		}
		ser := monitor.Series{Metric: metric, Database: peerclient.CleanText(gs.Database, 63)}
		if gs.Server != "" {
			ser.ServerID = p.ID
		}
		if database != "" && ser.Database != database {
			continue
		}
		pts := make([]monitor.Point, n)
		for i, pt := range gs.Points {
			pts[i] = monitor.Point{T: from.Add(time.Duration(i*step) * time.Second), V: pt.Value}
		}
		out = append(out, monitor.Result{Series: ser, Points: pts})
	}
	return out, true
}
