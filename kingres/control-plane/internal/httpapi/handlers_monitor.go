// SPDX-License-Identifier: PostgreSQL

package httpapi

import (
	"context"
	"crypto/subtle"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/api"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/auth"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/monitor"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/problem"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/setup"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/sysdb"
)

func (s *Server) monitorHandlers() map[string]handlerFunc {
	return map[string]handlerFunc{
		"listAlerts":       s.listAlerts,
		"acknowledgeAlert": s.acknowledgeAlert,
		"queryMetrics":     s.queryMetrics,
	}
}

func (s *Server) listAlerts(_ *call, w http.ResponseWriter, r *http.Request) error {
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
	c, err := o.Service.ServiceConn(r.Context(), sysdb.Database)
	if err != nil {
		return s.storeProblem(r, errors.Join(errUnavailable, err), "")
	}
	defer func() { _ = c.Close(context.WithoutCancel(r.Context())) }() //nolint:contextcheck // closing must outlive the request context
	out, err := monitor.List(r.Context(), c, state, severity, page)
	if err != nil {
		return problem.Validation("The cursor is not valid.", problem.FieldError{Field: "cursor", Message: "not a cursor issued by this server", Code: "invalid_cursor"})
	}
	s.writeJSON(w, r, http.StatusOK, out)
	return nil
}

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
	conn, err := o.Service.ServiceConn(r.Context(), sysdb.Database)
	if err != nil {
		return s.storeProblem(r, errors.Join(errUnavailable, err), "")
	}
	defer func() { _ = conn.Close(context.WithoutCancel(r.Context())) }() //nolint:contextcheck // closing must outlive the request context
	a, err := monitor.Acknowledge(r.Context(), conn, id, c.session.Username, s.now())
	switch {
	case errors.Is(err, monitor.ErrNotFound):
		return problem.New(problem.NotFound, "No such alert.")
	case errors.Is(err, monitor.ErrResolved):
		return problem.New(problem.Conflict, "The alert is already resolved.")
	case err != nil:
		s.log.Error("acknowledging an alert", "request_id", stateOf(r).id, "error", err)
		return problem.New(problem.InternalError, "An unexpected error occurred.")
	}
	s.writeJSON(w, r, http.StatusOK, a)
	return nil
}

var metricUnits = map[string]struct {
	unit string
	sum  bool
}{
	"cpuUsagePercent": {"percent", false}, "memoryUsedBytes": {"bytes", true}, "diskUsedBytes": {"bytes", true},
	"connections": {"count", true}, "replicationLagBytes": {"bytes", false}, "replicationLagSeconds": {"seconds", false},
	"databaseSizeBytes": {"bytes", true}, "transactionsPerSecond": {"countPerSecond", true},
	"queriesPerSecond": {"countPerSecond", true}, "bucketsUnderReplicated": {"count", true},
}

func (s *Server) queryMetrics(_ *call, w http.ResponseWriter, r *http.Request) error {
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
	res, err := s.deps.Metrics.Query(metric, from, to, step, srv, database, groupBy, mu.sum)
	if errors.Is(err, monitor.ErrTooManyPoints) {
		return problem.New(problem.Unprocessable, err.Error())
	}
	out := api.MetricQueryResult{Metric: metric, Unit: mu.unit, From: from.UTC(), To: to.UTC(), Step: step, Series: []api.MetricSeries{}}
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

var errUnavailable = errors.New("backing store unavailable")

// metricsEndpoint serves the Prometheus text format at /metrics (outside the
// REST contract). It requires authentication: a bearer token of a session with
// the kingres_stats role or better, or the scrape token configured with
// metrics-token. Without either the endpoint answers 401.
func (s *Server) metricsEndpoint(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	unauth := func() {
		w.Header().Set("WWW-Authenticate", `Bearer realm="kingresd-metrics"`)
		http.Error(w, "authentication required", http.StatusUnauthorized)
	}
	scheme, token, ok := strings.Cut(r.Header.Get("Authorization"), " ")
	token = strings.TrimSpace(token)
	if !ok || !strings.EqualFold(scheme, "Bearer") || token == "" {
		unauth()
		return
	}
	allowed := s.deps.MetricsToken != "" && subtle.ConstantTimeCompare([]byte(token), []byte(s.deps.MetricsToken)) == 1
	if !allowed {
		if sess, err := s.deps.Sessions.Lookup(token); err == nil && sess.Kind == auth.KindToken {
			// The roles are the current ones, not those of the sign-in (CP-02).
			if cur, perr := s.currentRoles(r, sess.Username); perr == nil && auth.Allows(cur, auth.RoleStats) {
				allowed = true
			}
		}
	}
	if !allowed {
		unauth()
		return
	}
	if s.deps.Setup.State() == setup.Uninitialized || s.deps.Metrics == nil {
		http.Error(w, "no metrics", http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	s.deps.Metrics.WritePrometheus(w)
}
