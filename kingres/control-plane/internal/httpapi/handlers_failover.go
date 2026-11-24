// SPDX-License-Identifier: AGPL-3.0-only

package httpapi

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"unicode"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/auth"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/failover"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/internalapi"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/problem"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/task"
)

// Failoverer is what the HTTP layer needs of the failover (internal/failover). The
// operations run on the manager leader (the guard forwards them); the reads are
// answered by the leader too, because the run's records are in its system database.
type Failoverer interface {
	Start(ctx context.Context, actor, server string, req failover.Request) (*task.Task, *failover.Plan, error)
	StartRestore(ctx context.Context, actor string, req failover.RestoreRequest) (*task.Task, *failover.Plan, error)
	Status(ctx context.Context) (failover.Status, error)
	Reports(ctx context.Context, limit int, cursor string) (failover.ReportPage, error)
	Report(ctx context.Context, id int64) (failover.Report, error)
}

func (s *Server) failoverHandlers() map[string]handlerFunc {
	return map[string]handlerFunc{
		"failoverServer":            s.failoverServer,
		"restorePreferredPrimaries": s.restorePreferredPrimaries,
		"getFailoverStatus":         s.getFailoverStatus,
		"listFailoverReports":       s.listFailoverReports,
		"getFailoverReport":         s.getFailoverReport,
		"abandonMapGroupBootstrap":  s.abandonMapGroupBootstrap,
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
	req.ActorIsAdmin = auth.Allows(c.session.ClusterRoles, auth.RoleAdmin)
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

// restorePreferredPrimaries starts the restore of the preferred primaries, or answers its plan (the body is optional).
func (s *Server) restorePreferredPrimaries(c *call, w http.ResponseWriter, r *http.Request) error {
	fo, err := s.needFailover()
	if err != nil {
		return err
	}
	var req failover.RestoreRequest
	if r.ContentLength != 0 {
		if err := s.body(w, r, &req); err != nil {
			return err
		}
	}
	t, plan, err := fo.StartRestore(r.Context(), c.session.Username, req)
	if err != nil {
		return s.failoverProblem(r, err)
	}
	if plan != nil {
		s.writeJSON(w, r, http.StatusOK, plan)
		return nil
	}
	c.addDetails(map[string]any{"taskId": t.ID, "shardGroupIds": req.ShardGroupIDs})
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

// MapFailoverer is what the HTTP layer needs of the failover of the map group (cmd/kingresd, design ha-phase5-part2.md 3.4). The
// leader runs every operation, but when the leader is the server whose map group is being failed over it is gone, so a server that
// does not hear it answers these itself: the request for that server's failover, the status, and the task with id 0 that stands for
// the bootstrap until the task is recorded.
type MapFailoverer interface {
	// Handles says that a request for the failover of server is the failover of the map group that cannot go through the leader.
	Handles(ctx context.Context, server string) bool
	Start(ctx context.Context, actor string, id internalapi.ForwardIdentity, server string, req failover.Request) (task.Task, error)
	Active() bool
	TaskZero() (task.Task, bool)
	Status() (failover.Status, bool)
	// Abandon gives up the bootstrap of the map group that is in progress on this server or on a standby's server (F4-06): false when none is.
	// id is the identity of the operator's session: a declaration that a server is lost travels to the old leader with its proof.
	Abandon(ctx context.Context, actor string, id internalapi.ForwardIdentity, reason, lostServer string) (failover.MapAbandonResult, error)
}

// mapFailoverIntercept answers the operations of the bootstrap of the map group on this server. It returns true when it answered.
func (s *Server) mapFailoverIntercept(c *call, w http.ResponseWriter, r *http.Request) (bool, error) {
	mf := s.deps.MapFailover
	if mf == nil {
		return false, nil
	}
	switch c.op.ID {
	case "failoverServer":
		server := r.PathValue("serverId")
		if !mf.Handles(r.Context(), server) {
			return false, nil
		}
		var req failover.Request
		if err := s.body(w, r, &req); err != nil {
			return true, err
		}
		if req.DryRun {
			return true, problem.New(problem.UnsupportedOp, "The plan of the failover of the map group cannot be asked while the manager leader is gone.")
		}
		t, err := mf.Start(r.Context(), c.session.Username, identityOf(c.session), server, req)
		if err != nil {
			return true, s.failoverProblem(r, err)
		}
		c.addDetails(map[string]any{"serverId": server, "mode": req.Mode, "allowDataLoss": req.AllowDataLoss, "comment": req.Comment, "mapGroup": true})
		w.Header().Set("Location", "/api/v1/failover")
		s.writeJSON(w, r, http.StatusAccepted, taskToAPI(t))
		return true, nil
	case "abandonMapGroupBootstrap":
		return true, s.abandonMapGroupBootstrap(c, w, r)
	case "getFailoverStatus":
		if st, ok := mf.Status(); ok {
			s.writeJSON(w, r, http.StatusOK, st)
			return true, nil
		}
	case "getTask":
		if r.PathValue("taskId") == "0" {
			if t, ok := mf.TaskZero(); ok {
				s.writeJSON(w, r, http.StatusOK, taskToAPI(t))
				return true, nil
			}
		}
	}
	return false, nil
}

// abandonMapGroupBootstrap gives up a stuck bootstrap of the map group (0.18.0). Any server answers it: the leader may be the server that is gone.
func (s *Server) abandonMapGroupBootstrap(c *call, w http.ResponseWriter, r *http.Request) error {
	var req struct {
		Reason       string `json:"reason"`
		LostServerID string `json:"lostServerId"`
	}
	if err := s.bodyClosed(w, r, &req); err != nil {
		return err
	}
	reason := strings.TrimSpace(req.Reason)
	if reason == "" || len(reason) > 500 || strings.ContainsFunc(reason, unicode.IsControl) {
		return problem.New(problem.ValidationFailed, "The reason must be 1 to 500 characters without control characters.")
	}
	mf := s.deps.MapFailover
	if mf == nil {
		return problem.New(problem.UnsupportedOp, "This server cannot give up a bootstrap of the map group (no managed instance or no internal API).")
	}
	res, err := mf.Abandon(r.Context(), c.session.Username, identityOf(c.session), reason, strings.TrimSpace(req.LostServerID))
	if err != nil {
		return s.failoverProblem(r, err)
	}
	c.addDetails(map[string]any{"reason": reason, "lostServerId": strings.TrimSpace(req.LostServerID), "abandoned": res.Abandoned, "resumedServerId": res.ResumedServer})
	s.writeJSON(w, r, http.StatusOK, res)
	return nil
}
