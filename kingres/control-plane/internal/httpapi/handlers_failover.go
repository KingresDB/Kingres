// SPDX-License-Identifier: AGPL-3.0-only

package httpapi

import (
	"context"
	"errors"
	"net/http"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/failover"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/problem"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/task"
)

// Failoverer is what the HTTP layer needs of the failover (internal/failover). The
// operations run on the manager leader (the guard forwards them); the reads are
// answered by the leader too, because the run's records are in its system database.
type Failoverer interface {
	Start(ctx context.Context, actor, server string, req failover.Request) (*task.Task, *failover.Plan, error)
	Status(ctx context.Context) (failover.Status, error)
	Reports(ctx context.Context, limit int, cursor string) (failover.ReportPage, error)
	Report(ctx context.Context, id int64) (failover.Report, error)
}

func (s *Server) failoverHandlers() map[string]handlerFunc {
	return map[string]handlerFunc{
		"failoverServer":      s.failoverServer,
		"getFailoverStatus":   s.getFailoverStatus,
		"listFailoverReports": s.listFailoverReports,
		"getFailoverReport":   s.getFailoverReport,
	}
}

func (s *Server) needFailover() (Failoverer, error) {
	if s.deps.Failover == nil {
		return nil, problem.New(problem.UnsupportedOp, "This server cannot fail over (no managed instance or no internal API).")
	}
	return s.deps.Failover, nil
}

func (s *Server) failoverProblem(r *http.Request, err error) error {
	var p *problem.Problem
	if errors.As(err, &p) {
		return p
	}
	s.log.Error("failover", "request_id", stateOf(r).id, "error", err)
	return problem.New(problem.ServiceUnavailable, "The failover state cannot be read now.").WithRetryAfter(5)
}

func (s *Server) failoverServer(c *call, w http.ResponseWriter, r *http.Request) error {
	fo, err := s.needFailover()
	if err != nil {
		return err
	}
	pp := newParams(r)
	server := pp.pathServerID("serverId")
	if p := pp.err(); p != nil {
		return p
	}
	var req failover.Request
	if err := s.body(w, r, &req); err != nil {
		return err
	}
	t, plan, err := fo.Start(r.Context(), c.session.Username, server, req)
	if err != nil {
		return s.failoverProblem(r, err)
	}
	if plan != nil {
		s.writeJSON(w, r, http.StatusOK, plan)
		return nil
	}
	c.addDetails(map[string]any{"taskId": t.ID, "serverId": server, "mode": req.Mode, "shardGroupIds": req.ShardGroupIDs,
		"allowDataLoss": req.AllowDataLoss, "comment": req.Comment})
	s.accepted(w, r, *t)
	return nil
}

func (s *Server) getFailoverStatus(_ *call, w http.ResponseWriter, r *http.Request) error {
	fo, err := s.needFailover()
	if err != nil {
		return err
	}
	st, err := fo.Status(r.Context())
	if err != nil {
		return s.failoverProblem(r, err)
	}
	s.writeJSON(w, r, http.StatusOK, st)
	return nil
}

func (s *Server) listFailoverReports(_ *call, w http.ResponseWriter, r *http.Request) error {
	fo, err := s.needFailover()
	if err != nil {
		return err
	}
	pp := newParams(r)
	pg := pp.page()
	if p := pp.err(); p != nil {
		return p
	}
	page, err := fo.Reports(r.Context(), pg.Limit, pg.Cursor)
	if err != nil {
		return s.failoverProblem(r, err)
	}
	s.writeJSON(w, r, http.StatusOK, page)
	return nil
}

func (s *Server) getFailoverReport(_ *call, w http.ResponseWriter, r *http.Request) error {
	fo, err := s.needFailover()
	if err != nil {
		return err
	}
	pp := newParams(r)
	id := pp.pathInt64("reportId")
	if p := pp.err(); p != nil {
		return p
	}
	rep, err := fo.Report(r.Context(), id)
	if err != nil {
		return s.failoverProblem(r, err)
	}
	s.writeJSON(w, r, http.StatusOK, rep)
	return nil
}
