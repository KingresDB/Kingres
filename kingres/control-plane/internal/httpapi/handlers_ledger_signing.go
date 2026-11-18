// SPDX-License-Identifier: AGPL-3.0-only

package httpapi

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/api"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/problem"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/task"
)

// LedgerSigner is what the HTTP layer needs of the signing keys of the ledger (internal/ledgersign). Every
// operation that changes anything runs on the manager leader (the guard forwards it); the reads are answered by
// the leader too, because the keys, the events and the trust file are in its system database. None of the
// answers holds a private key, a seed or a passphrase.
type LedgerSigner interface {
	Settings(ctx context.Context) (api.LedgerSettingsCluster, error)
	UpdateSettings(ctx context.Context, in api.LedgerSettingsUpdate) (api.LedgerSettingsCluster, error)
	Signing(ctx context.Context) (api.LedgerSigning, error)
	TrustFile(ctx context.Context) ([]byte, error)
	CreateKey(ctx context.Context, actor string, req api.CreateLedgerSigningKeyRequest) (task.Task, error)
	RetireKey(ctx context.Context, actor, fingerprint string) (task.Task, error)
	RevokeKey(ctx context.Context, actor, fingerprint string, req api.RevokeLedgerSigningKeyRequest) (task.Task, error)
}

func (s *Server) ledgerSigningHandlers() map[string]handlerFunc {
	return map[string]handlerFunc{
		"getLedgerSettings":      s.getLedgerSettings,
		"updateLedgerSettings":   s.updateLedgerSettings,
		"getLedgerSigning":       s.getLedgerSigning,
		"createLedgerSigningKey": s.createLedgerSigningKey,
		"retireLedgerSigningKey": s.retireLedgerSigningKey,
		"revokeLedgerSigningKey": s.revokeLedgerSigningKey,
		"getLedgerTrustFile":     s.getLedgerTrustFile,
	}
}

func (s *Server) needLedgerSigner() (LedgerSigner, error) {
	if s.deps.LedgerSigning == nil {
		return nil, problem.New(problem.UnsupportedOp, "This server cannot sign the ledger (no managed instance).")
	}
	return s.deps.LedgerSigning, nil
}

// ledgerProblem keeps a problem as it is and hides every other error behind a 503: the text of an internal
// error is logged, never sent (it could name a path or a server).
func (s *Server) ledgerProblem(r *http.Request, err error) error {
	var p *problem.Problem
	if errors.As(err, &p) {
		return p
	}
	s.log.Error("ledger signing", "request_id", stateOf(r).id, "error", err)
	return problem.New(problem.ServiceUnavailable, "The signing keys of the ledger cannot be read or changed now.").WithRetryAfter(5)
}

func (s *Server) getLedgerSettings(_ *call, w http.ResponseWriter, r *http.Request) error {
	ls, err := s.needLedgerSigner()
	if err != nil {
		return err
	}
	out, err := ls.Settings(r.Context())
	if err != nil {
		return s.ledgerProblem(r, err)
	}
	s.writeJSON(w, r, http.StatusOK, out)
	return nil
}

func (s *Server) updateLedgerSettings(_ *call, w http.ResponseWriter, r *http.Request) error {
	ls, err := s.needLedgerSigner()
	if err != nil {
		return err
	}
	var in api.LedgerSettingsUpdate
	if err := s.bodyClosed(w, r, &in); err != nil {
		return err
	}
	out, err := ls.UpdateSettings(r.Context(), in)
	if err != nil {
		return s.ledgerProblem(r, err)
	}
	s.writeJSON(w, r, http.StatusOK, out)
	return nil
}

func (s *Server) getLedgerSigning(_ *call, w http.ResponseWriter, r *http.Request) error {
	ls, err := s.needLedgerSigner()
	if err != nil {
		return err
	}
	out, err := ls.Signing(r.Context())
	if err != nil {
		return s.ledgerProblem(r, err)
	}
	s.writeJSON(w, r, http.StatusOK, out)
	return nil
}

func (s *Server) getLedgerTrustFile(_ *call, w http.ResponseWriter, r *http.Request) error {
	ls, err := s.needLedgerSigner()
	if err != nil {
		return err
	}
	b, err := ls.TrustFile(r.Context())
	if err != nil {
		return s.ledgerProblem(r, err)
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Disposition", `attachment; filename="trust.json"`)
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(b)
	return nil
}

func (s *Server) createLedgerSigningKey(c *call, w http.ResponseWriter, r *http.Request) error {
	ls, err := s.needLedgerSigner()
	if err != nil {
		return err
	}
	var req api.CreateLedgerSigningKeyRequest
	if r.Body != nil && r.Body != http.NoBody {
		if err := s.optionalBodyClosed(w, r, &req); err != nil {
			return err
		}
	}
	t, err := ls.CreateKey(r.Context(), c.session.Username, req)
	if err != nil {
		return s.ledgerProblem(r, err)
	}
	c.addDetails(map[string]any{"taskId": t.ID})
	s.accepted(w, r, t)
	return nil
}

func (s *Server) retireLedgerSigningKey(c *call, w http.ResponseWriter, r *http.Request) error {
	ls, err := s.needLedgerSigner()
	if err != nil {
		return err
	}
	var empty struct{}
	if r.Body != nil && r.Body != http.NoBody {
		if err := s.optionalBodyClosed(w, r, &empty); err != nil {
			return err
		}
	}
	t, err := ls.RetireKey(r.Context(), c.session.Username, r.PathValue("fingerprint"))
	if err != nil {
		return s.ledgerProblem(r, err)
	}
	c.addDetails(map[string]any{"taskId": t.ID})
	s.accepted(w, r, t)
	return nil
}

func (s *Server) revokeLedgerSigningKey(c *call, w http.ResponseWriter, r *http.Request) error {
	ls, err := s.needLedgerSigner()
	if err != nil {
		return err
	}
	var req api.RevokeLedgerSigningKeyRequest
	if err := s.bodyClosed(w, r, &req); err != nil {
		return err
	}
	t, err := ls.RevokeKey(r.Context(), c.session.Username, r.PathValue("fingerprint"), req)
	if err != nil {
		return s.ledgerProblem(r, err)
	}
	// The details of the audit entry are the task and the stated time; the reason is free text of the
	// operator and is kept in the event, not copied here.
	c.addDetails(map[string]any{"taskId": t.ID, "compromisedSince": req.CompromisedSince.UTC()})
	s.accepted(w, r, t)
	return nil
}

// optionalBodyClosed is optionalBody for a request the contract declares closed.
func (s *Server) optionalBodyClosed(w http.ResponseWriter, r *http.Request, dst any) error {
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, s.deps.MaxBodyBytes))
	if err != nil {
		return problem.New(problem.BadRequest, "The request body could not be read.")
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil
	}
	r.Body = io.NopCloser(bytes.NewReader(raw))
	return s.bodyClosed(w, r, dst)
}
