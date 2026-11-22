// SPDX-License-Identifier: AGPL-3.0-only

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
	RenewServerCertificates(ctx context.Context, actor, id string) (task.Task, error)
	RevokeServerCertificates(ctx context.Context, actor, id string) (task.Task, error)
}

func (s *Server) memberHandlers() map[string]handlerFunc {
	return map[string]handlerFunc{
		"joinCluster":              s.joinCluster,
		"addServer":                s.addServer,
		"removeServer":             s.removeServer,
		"leaveCluster":             s.leaveCluster,
		"renewServerCertificates":  s.renewServerCertificates,
		"revokeServerCertificates": s.revokeServerCertificates,
		"getCaEscrow":              s.getCaEscrow,
		"setCaEscrow":              s.setCaEscrow,
		"rotateCaEscrow":           s.rotateCaEscrow,
	}
}

// pin reads and checks the optional pinned fingerprint of the request.
func pinOf(r *http.Request, body *string) (string, error) {
	var fp [2]string
	for i, src := range []struct{ field, v string }{{HeaderPin, r.Header.Get(HeaderPin)}, {"/expectedFingerprint", deref(body)}} {
		if src.v == "" {
			continue
		}
		fp[i] = pki.NormalizeFingerprint(src.v)
		if fp[i] == "" {
			return "", problem.Validation("The pinned fingerprint is invalid.",
				problem.FieldError{Field: src.field, Message: "must be a SHA-256 fingerprint in hex, with or without colons", Code: "pattern"})
		}
	}
	if fp[0] != "" && fp[1] != "" && fp[0] != fp[1] {
		return "", problem.Validation("The pinned fingerprint is given twice, differently.",
			problem.FieldError{Field: "/expectedFingerprint", Message: "differs from the header " + HeaderPin, Code: "invalid"})
	}
	if fp[1] != "" {
		return fp[1], nil
	}
	return fp[0], nil
}

// mergePins combines the pin forwarded with a request and the pin in its body.
func mergePins(forwarded string, body *string) (string, error) {
	if deref(body) == "" {
		return forwarded, nil
	}
	fp := pki.NormalizeFingerprint(*body)
	switch {
	case fp == "":
		return "", problem.Validation("The pinned fingerprint is invalid.",
			problem.FieldError{Field: "/expectedFingerprint", Message: "must be a SHA-256 fingerprint in hex, with or without colons", Code: "pattern"})
	case forwarded != "" && forwarded != fp:
		return "", problem.Validation("The pinned fingerprint is given twice, differently.",
			problem.FieldError{Field: "/expectedFingerprint", Message: "differs from the pin the request was forwarded with", Code: "invalid"})
	}
	return fp, nil
}

// requirePin refuses a first contact that would trust whatever certificate
// answers, unless the administrator said so (CP2-05): the password of the
// administrator (join) or the setup token and a certificate of the cluster
// (addServer) would go to a machine that was never authenticated.
func requirePin(pin string, allowUnpinned *bool) error {
	if pin != "" || (allowUnpinned != nil && *allowUnpinned) {
		return nil
	}
	return problem.Validation("The certificate of the other server must be pinned.",
		problem.FieldError{Field: "/expectedFingerprint", Code: "required",
			Message: "give the SHA-256 fingerprint of the other server's certificate, or set allowUnpinned to true to trust whatever answers"})
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
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
	pin, err := pinOf(r, req.ExpectedFingerprint)
	if err != nil {
		return err
	}
	if err := requirePin(pin, req.AllowUnpinned); err != nil {
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
		if s.deps.InternalAPIDisabled {
			return nil, problem.New(problem.InternalAPIDisabled,
				"This server was started without its internal API (no internal-listen address), so it cannot add, remove or leave servers; start it with one.")
		}
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
	// On the leader of a forwarded request the header is not there: the server that
	// received it forwarded the one normalized pin (see forwardPin); the body is
	// read as well, and a pin that differs between the two is refused.
	var pin string
	if f := forwardedOf(r); f != nil {
		if pin, err = mergePins(f.pin, req.ExpectedFingerprint); err != nil {
			return err
		}
	} else if pin, err = pinOf(r, req.ExpectedFingerprint); err != nil {
		return err
	}
	if err := requirePin(pin, req.AllowUnpinned); err != nil {
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

// renewServerCertificates is POST /servers/{serverId}/certificates/renewals.
func (s *Server) renewServerCertificates(c *call, w http.ResponseWriter, r *http.Request) error {
	m, err := s.needMembers()
	if err != nil {
		return err
	}
	pp := newParams(r)
	id := pp.pathServerID("serverId")
	if p := pp.err(); p != nil {
		return p
	}
	t, err := m.RenewServerCertificates(r.Context(), c.session.Username, id)
	if err != nil {
		return err
	}
	s.accepted(w, r, t)
	return nil
}

// revokeServerCertificates is POST /servers/{serverId}/certificates/revocations.
func (s *Server) revokeServerCertificates(c *call, w http.ResponseWriter, r *http.Request) error {
	m, err := s.needMembers()
	if err != nil {
		return err
	}
	pp := newParams(r)
	id := pp.pathServerID("serverId")
	if p := pp.err(); p != nil {
		return p
	}
	t, err := m.RevokeServerCertificates(r.Context(), c.session.Username, id)
	if err != nil {
		return err
	}
	s.accepted(w, r, t)
	return nil
}

// CAEscrow is the escrow of the CA key (contract 0.22.0). The leader's Members implements it; a server whose Members does
// not has no escrow to offer.
type CAEscrow interface {
	EscrowStatus(ctx context.Context) (api.CaEscrowStatus, error)
	SetEscrow(ctx context.Context, enabled bool) (api.CaEscrowStatus, error)
	RotateEscrow(ctx context.Context) (api.CaEscrowStatus, error)
}

func (s *Server) needEscrow() (CAEscrow, error) {
	m, err := s.needMembers()
	if err != nil {
		return nil, err
	}
	e, ok := m.(CAEscrow)
	if !ok {
		return nil, problem.New(problem.UnsupportedOp, "This server cannot hold an escrow of the certificate authority key.")
	}
	return e, nil
}

// getCaEscrow is GET /certificate-authority/escrow.
func (s *Server) getCaEscrow(c *call, w http.ResponseWriter, r *http.Request) error {
	e, err := s.needEscrow()
	if err != nil {
		return err
	}
	out, err := e.EscrowStatus(r.Context())
	if err != nil {
		return err
	}
	s.writeJSON(w, r, http.StatusOK, out)
	return nil
}

// setCaEscrow is PUT /certificate-authority/escrow.
func (s *Server) setCaEscrow(c *call, w http.ResponseWriter, r *http.Request) error {
	e, err := s.needEscrow()
	if err != nil {
		return err
	}
	var req api.CaEscrowUpdate
	if err := s.body(w, r, &req); err != nil {
		return err
	}
	if req.Enabled == nil {
		return problem.Validation("The request is invalid.", problem.FieldError{Field: "/enabled", Message: "is required", Code: "required"})
	}
	c.details = map[string]any{"enabled": *req.Enabled}
	out, err := e.SetEscrow(r.Context(), *req.Enabled)
	if err != nil {
		return err
	}
	s.writeJSON(w, r, http.StatusOK, out)
	return nil
}

// rotateCaEscrow is POST /certificate-authority/escrow/rotations.
func (s *Server) rotateCaEscrow(c *call, w http.ResponseWriter, r *http.Request) error {
	e, err := s.needEscrow()
	if err != nil {
		return err
	}
	out, err := e.RotateEscrow(r.Context())
	if err != nil {
		return err
	}
	c.details = map[string]any{"generation": out.Generation}
	s.writeJSON(w, r, http.StatusOK, out)
	return nil
}
