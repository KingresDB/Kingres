// SPDX-License-Identifier: AGPL-3.0-only

package httpapi

import (
	"errors"
	"net/http"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/api"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/pgconfig"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/problem"
)

// Configuration from the console (contract 0.8.0, design config-phase13.md). Every operation is
// answered by the manager leader (the reads are in leaderBoundReads, the changes are mutating), which
// is the server that can ask every other server.

func (s *Server) configHandlers() map[string]handlerFunc {
	return map[string]handlerFunc{
		"listConfigParameters":    s.listConfigParameters,
		"getConfigParameter":      s.getConfigParameter,
		"previewConfigChange":     s.previewConfigChange,
		"applyConfigChange":       s.applyConfigChange,
		"listConfigChanges":       s.listConfigChanges,
		"revertConfigChange":      s.revertConfigChange,
		"listConfigServers":       s.listConfigServers,
		"getConfigSummary":        s.getConfigSummary,
		"getClusterTime":          s.getClusterTime,
		"listTimeZones":           s.listTimeZones,
		"restartPendingInstances": s.restartPendingInstances,
	}
}

func (s *Server) needConfig() (*pgconfig.Leader, error) {
	if s.deps.Config == nil {
		return nil, problem.New(problem.UnsupportedOp, "This server is not configured to manage a PostgreSQL instance (pg-bin-dir).")
	}
	return s.deps.Config, nil
}

func (s *Server) listConfigParameters(_ *call, w http.ResponseWriter, r *http.Request) error {
	l, err := s.needConfig()
	if err != nil {
		return err
	}
	pp := newParams(r)
	f := pgconfig.ParamFilter{
		Q: pp.maxLen("q", 200), Category: pp.maxLen("category", 200),
		Kind:  pp.enum("kind", "settable", "managed", "refused"),
		State: pp.enum("state", "applied", "pendingRestart", "failed", "waiting", "unknown"),
	}
	if b := pp.boolQuery("changed"); b != nil {
		f.Changed = *b
	}
	if b := pp.boolQuery("differing"); b != nil {
		f.Differing = *b
	}
	if b := pp.boolQuery("pendingRestart"); b != nil {
		f.Pending = *b
	}
	page := pp.page()
	if p := pp.err(); p != nil {
		return p
	}
	out, err := l.ListParameters(r.Context(), f, page.Limit, page.Cursor)
	if err != nil {
		return configProblem(err)
	}
	s.writeJSON(w, r, http.StatusOK, out)
	return nil
}

func (s *Server) getConfigParameter(_ *call, w http.ResponseWriter, r *http.Request) error {
	l, err := s.needConfig()
	if err != nil {
		return err
	}
	pp := newParams(r)
	name := pp.pathName("name")
	if p := pp.err(); p != nil {
		return p
	}
	out, err := l.GetParameter(r.Context(), name)
	if err != nil {
		if errors.Is(err, pgconfig.ErrUnknownParameter) {
			return problem.New(problem.ConfigParameterUnknown, "PostgreSQL on this server has no such parameter.")
		}
		return configProblem(err)
	}
	s.writeJSON(w, r, http.StatusOK, out)
	return nil
}

func (s *Server) previewConfigChange(_ *call, w http.ResponseWriter, r *http.Request) error {
	l, err := s.needConfig()
	if err != nil {
		return err
	}
	var req api.ConfigChangeRequest
	if err := s.bodyClosed(w, r, &req); err != nil {
		return err
	}
	out, err := l.Preview(r.Context(), req)
	if err != nil {
		return configProblem(err)
	}
	s.writeJSON(w, r, http.StatusOK, out)
	return nil
}

func (s *Server) applyConfigChange(c *call, w http.ResponseWriter, r *http.Request) error {
	l, err := s.needConfig()
	if err != nil {
		return err
	}
	var req api.ConfigChangeRequest
	if err := s.bodyClosed(w, r, &req); err != nil {
		return err
	}
	details := l.ChangeDetails(r.Context(), req)
	t, err := l.StartApply(r.Context(), c.session.Username, req, 0)
	if err != nil {
		return s.configTaskProblem(r, err)
	}
	c.details = map[string]any{"changes": details, "taskId": t.ID}
	s.accepted(w, r, t)
	return nil
}

func (s *Server) revertConfigChange(c *call, w http.ResponseWriter, r *http.Request) error {
	l, err := s.needConfig()
	if err != nil {
		return err
	}
	pp := newParams(r)
	id := pp.pathInt64("changeId")
	if p := pp.err(); p != nil {
		return p
	}
	t, err := l.StartRevert(r.Context(), c.session.Username, id)
	if err != nil {
		if errors.Is(err, pgconfig.ErrNoSuchChange) {
			return problem.New(problem.NotFound, "There is no such configuration change.")
		}
		return s.configTaskProblem(r, err)
	}
	c.details = map[string]any{"reverts": id, "taskId": t.ID}
	s.accepted(w, r, t)
	return nil
}

func (s *Server) listConfigChanges(_ *call, w http.ResponseWriter, r *http.Request) error {
	l, err := s.needConfig()
	if err != nil {
		return err
	}
	pp := newParams(r)
	name, scope := pp.maxLen("name", 63), pp.maxLen("scopeId", 253)
	level := pp.enum("level", "cluster", "serverGroup", "server", "database")
	page := pp.page()
	if p := pp.err(); p != nil {
		return p
	}
	out, err := l.History(r.Context(), name, level, scope, page.Limit, page.Cursor)
	if err != nil {
		return configProblem(err)
	}
	s.writeJSON(w, r, http.StatusOK, out)
	return nil
}

func (s *Server) listConfigServers(_ *call, w http.ResponseWriter, r *http.Request) error {
	l, err := s.needConfig()
	if err != nil {
		return err
	}
	out, err := l.Servers(r.Context())
	if err != nil {
		return configProblem(err)
	}
	s.writeJSON(w, r, http.StatusOK, out)
	return nil
}

func (s *Server) getConfigSummary(_ *call, w http.ResponseWriter, r *http.Request) error {
	l, err := s.needConfig()
	if err != nil {
		return err
	}
	out, err := l.Summary(r.Context())
	if err != nil {
		return configProblem(err)
	}
	s.writeJSON(w, r, http.StatusOK, out)
	return nil
}

func (s *Server) getClusterTime(_ *call, w http.ResponseWriter, r *http.Request) error {
	l, err := s.needConfig()
	if err != nil {
		return err
	}
	out, err := l.Time(r.Context())
	if err != nil {
		return configProblem(err)
	}
	s.writeJSON(w, r, http.StatusOK, out)
	return nil
}

func (s *Server) listTimeZones(_ *call, w http.ResponseWriter, r *http.Request) error {
	l, err := s.needConfig()
	if err != nil {
		return err
	}
	zones, err := l.Agent.TimeZones(r.Context())
	if err != nil {
		return configProblem(err)
	}
	s.writeJSON(w, r, http.StatusOK, api.TimeZoneList{Items: zones})
	return nil
}

func (s *Server) restartPendingInstances(c *call, w http.ResponseWriter, r *http.Request) error {
	l, err := s.needConfig()
	if err != nil {
		return err
	}
	var req api.RestartPendingRequest
	if err := s.bodyClosed(w, r, &req); err != nil {
		return err
	}
	t, err := l.StartRestart(r.Context(), c.session.Username, req)
	if err != nil {
		return s.configTaskProblem(r, err)
	}
	c.details = map[string]any{"serverIds": req.ServerIDs, "taskId": t.ID}
	s.accepted(w, r, t)
	return nil
}

// configProblem maps an error of the configuration code to a problem.
func configProblem(err error) error {
	var p *problem.Problem
	if errors.As(err, &p) {
		return p
	}
	return problem.New(problem.ServiceUnavailable, "The configuration of the servers cannot be read now.").WithRetryAfter(5)
}

func (s *Server) configTaskProblem(r *http.Request, err error) error {
	var p *problem.Problem
	if errors.As(err, &p) {
		return p
	}
	return s.taskProblem(r, err)
}
