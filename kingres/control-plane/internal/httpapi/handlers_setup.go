// SPDX-License-Identifier: AGPL-3.0-only

package httpapi

import (
	"errors"
	"net/http"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/api"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/cluster"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/problem"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/task"
)

// SetupService creates the cluster; implemented by cluster.Service.
type SetupService interface {
	// Enabled reports whether instances can be managed on this server.
	Enabled() bool
	// Init starts the setup task for a validated request.
	Init(req api.InitClusterRequest, actor string) (task.Task, error)
	// Join starts the new server's side of joining a cluster (design D4);
	// Joinable reports whether no setup task is running.
	Join(req api.JoinClusterRequest, pin string) (task.Task, error)
	Joinable() bool
}

// initCluster creates a one-server cluster (D5). The administrator password
// is passed to the task engine as an in-memory secret and never stored.
func (s *Server) initCluster(c *call, w http.ResponseWriter, r *http.Request) error {
	if s.deps.SetupService == nil || !s.deps.SetupService.Enabled() {
		return problem.New(problem.UnsupportedOp,
			"This server is not configured to manage a PostgreSQL instance (set pg-bin-dir); initialization is not available.")
	}
	if p := requireJSON(r); p != nil {
		return p
	}
	var req api.InitClusterRequest
	if p := s.decodeJSON(w, r, &req); p != nil {
		return p
	}
	if errs := cluster.ValidateInit(req, s.deps.DataDir); len(errs) > 0 {
		fe := make([]problem.FieldError, len(errs))
		for i, e := range errs {
			fe[i] = problem.FieldError{Field: e.Field, Message: e.Message, Code: e.Code}
		}
		return problem.Validation("The initialization request is invalid.", fe...)
	}
	t, err := s.deps.SetupService.Init(req, "setup")
	switch {
	case errors.Is(err, task.ErrSubjectBusy):
		return problem.New(problem.OperationInProgress, "A setup task is already running.")
	case err != nil:
		s.log.Error("starting the setup task", "request_id", stateOf(r).id, "error", err)
		return problem.New(problem.InternalError, "The setup task could not be started.")
	}
	w.Header().Set("Location", "/api/v1/setup/state")
	s.writeJSON(w, r, http.StatusAccepted, taskToAPI(t))
	return nil
}

// fingerprinter is implemented by the setup service when it can tell the
// fingerprint of the certificate the internal API presents.
type fingerprinter interface{ InternalFingerprint() string }

// internalFingerprint is that fingerprint, nil when it is not known.
func (s *Server) internalFingerprint() *string {
	f, ok := s.deps.SetupService.(fingerprinter)
	if !ok {
		return nil
	}
	if fp := f.InternalFingerprint(); fp != "" {
		return &fp
	}
	return nil
}

// setupAudience tells whether the caller of the public setup state may read the
// fingerprint of this uninitialized server's certificate: whoever may set the
// server up. That is a request from the machine itself while the panel is
// loopback-only (where the wizard runs), or a request with the setup token of
// this server. The fingerprint is no secret, but a pin only protects when it was
// read through another channel than the one it protects, so the answer is not
// given to everybody who can reach the port.
func (s *Server) setupAudience(c *call, r *http.Request) bool {
	if s.deps.LoopbackOnly && isLoopback(c.ip) {
		return true
	}
	tok := r.Header.Get(headerSetupToken)
	if tok == "" {
		return false
	}
	if blocked, _ := s.setupLimiter.Blocked(c.ip); blocked {
		return false
	}
	ok, err := s.deps.Setup.VerifyToken(tok)
	if err == nil && !ok {
		s.setupLimiter.Record(c.ip)
	}
	return err == nil && ok
}
