// SPDX-License-Identifier: PostgreSQL

package httpapi

import (
	"context"
	"errors"
	"net/http"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/api"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/cluster"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/pki"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/problem"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/task"
)

// HeaderPin carries the SHA-256 fingerprint the administrator expects of the
// other server's certificate on first contact (hex, colons allowed). The 0.3.0
// contract has no field for it (contract 0.4.0 will), so it is accepted as a
// header on joinCluster and addServer; without it the fingerprint that was seen
// is recorded (trust on first use).
const HeaderPin = "X-Kingres-Pin-Sha256"

// Members is what the HTTP layer needs from the leader's membership logic.
type Members interface {
	AddServer(ctx context.Context, actor string, req api.AddServerRequest, pin string) (task.Task, error)
	RemoveServer(ctx context.Context, actor, id string) (task.Task, error)
}

func (s *Server) memberHandlers() map[string]handlerFunc {
	return map[string]handlerFunc{
		"joinCluster":  s.joinCluster,
		"addServer":    s.addServer,
		"removeServer": s.removeServer,
	}
}

// pin reads and checks the optional pinned fingerprint of the request.
func pinOf(r *http.Request) (string, error) {
	h := r.Header.Get(HeaderPin)
	if h == "" {
		return "", nil
	}
	fp := pki.NormalizeFingerprint(h)
	if fp == "" {
		return "", problem.Validation("The pinned fingerprint is invalid.",
			problem.FieldError{Field: HeaderPin, Message: "must be a SHA-256 fingerprint in hex, with or without colons", Code: "pattern"})
	}
	return fp, nil
}

// joinCluster joins this server to the cluster at existingNodeAddress (D4).
func (s *Server) joinCluster(c *call, w http.ResponseWriter, r *http.Request) error {
	svc := s.deps.SetupService
	if svc == nil || !svc.Enabled() {
		return problem.New(problem.UnsupportedOp,
			"This server is not configured to manage a PostgreSQL instance (set pg-bin-dir); joining is not available.")
	}
	var req api.JoinClusterRequest
	if err := s.body(w, r, &req); err != nil {
		return err
	}
	if errs := cluster.ValidateJoin(req, s.deps.DataDir); len(errs) > 0 {
		fe := make([]problem.FieldError, len(errs))
		for i, e := range errs {
			fe[i] = problem.FieldError{Field: e.Field, Message: e.Message, Code: e.Code}
		}
		return problem.Validation("The join request is invalid.", fe...)
	}
	pin, err := pinOf(r)
	if err != nil {
		return err
	}
	if !s.deps.SetupService.Joinable() {
		return problem.New(problem.OperationInProgress, "A setup task is already running on this server.")
	}
	t, err := svc.Join(req, pin)
	switch {
	case errors.Is(err, task.ErrSubjectBusy):
		return problem.New(problem.OperationInProgress, "A setup task is already running.")
	case err != nil:
		s.log.Error("starting the join task", "request_id", stateOf(r).id, "error", err)
		return problem.New(problem.InternalError, "The join task could not be started.")
	}
	c.details = map[string]any{"existingNodeAddress": req.ExistingNodeAddress, "fingerprintPinned": pin != ""}
	w.Header().Set("Location", "/api/v1/setup/state")
	s.writeJSON(w, r, http.StatusAccepted, taskToAPI(t))
	return nil
}

func (s *Server) needMembers() (Members, error) {
	if s.deps.Members == nil {
		return nil, problem.New(problem.UnsupportedOp, "This server cannot add or remove servers (no internal API or instance management).")
	}
	return s.deps.Members, nil
}

func (s *Server) addServer(c *call, w http.ResponseWriter, r *http.Request) error {
	m, err := s.needMembers()
	if err != nil {
		return err
	}
	var req api.AddServerRequest
	if err := s.body(w, r, &req); err != nil {
		return err
	}
	pin := ""
	if f := forwardedOf(r); f != nil {
		pin = f.pin
	} else if pin, err = pinOf(r); err != nil {
		return err
	}
	t, err := m.AddServer(r.Context(), c.session.Username, req, pin)
	if err != nil {
		return err
	}
	c.details = map[string]any{"address": req.Address, "fingerprintPinned": pin != ""}
	s.accepted(w, r, t)
	return nil
}

func (s *Server) removeServer(c *call, w http.ResponseWriter, r *http.Request) error {
	m, err := s.needMembers()
	if err != nil {
		return err
	}
	pp := newParams(r)
	id := pp.pathServerID("serverId")
	if p := pp.err(); p != nil {
		return p
	}
	t, err := m.RemoveServer(r.Context(), c.session.Username, id)
	if err != nil {
		return err
	}
	s.accepted(w, r, t)
	return nil
}
