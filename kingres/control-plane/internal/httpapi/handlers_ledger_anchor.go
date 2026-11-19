// SPDX-License-Identifier: AGPL-3.0-only

package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/anchor"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/api"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/ledgeranchor"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/problem"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/store"
)

// LedgerAnchoring is what the HTTP layer needs of the anchoring of the ledger (internal/ledgeranchor). Every
// operation that changes anything runs on the manager leader (the guard forwards it); the reads are answered by
// the leader too, because the targets and the records are in its system database. No answer holds a credential:
// the credentials of a request are read once by the service and are never returned or logged.
type LedgerAnchoring interface {
	Targets(ctx context.Context) (api.LedgerAnchorTargetList, error)
	Target(ctx context.Context, id int64) (api.LedgerAnchorTarget, error)
	CreateTarget(ctx context.Context, actor string, in api.LedgerAnchorTargetCreate) (api.LedgerAnchorTarget, error)
	UpdateTarget(ctx context.Context, id int64, in api.LedgerAnchorTargetUpdate) (api.LedgerAnchorTarget, error)
	DeleteTarget(ctx context.Context, id int64) error
	TestTarget(ctx context.Context, id int64) (api.LedgerAnchorTest, error)
	List(ctx context.Context, db string, targetID int64, state string, p store.Page) (api.LedgerAnchorPage, error)
	AnchorNow(ctx context.Context, db string) (api.LedgerAnchorList, error)
	Retry(ctx context.Context, db string, id int64) (api.LedgerAnchor, error)
	Evidence(ctx context.Context, db string, id int64, format string) (ledgeranchor.Evidence, error)
}

func (s *Server) ledgerAnchorHandlers() map[string]handlerFunc {
	return map[string]handlerFunc{
		"listLedgerAnchorTargets":  s.listLedgerAnchorTargets,
		"createLedgerAnchorTarget": s.createLedgerAnchorTarget,
		"getLedgerAnchorTarget":    s.getLedgerAnchorTarget,
		"updateLedgerAnchorTarget": s.updateLedgerAnchorTarget,
		"deleteLedgerAnchorTarget": s.deleteLedgerAnchorTarget,
		"testLedgerAnchorTarget":   s.testLedgerAnchorTarget,
		"listLedgerAnchors":        s.listLedgerAnchors,
		"anchorLedgerNow":          s.anchorLedgerNow,
		"retryLedgerAnchor":        s.retryLedgerAnchor,
		"downloadLedgerAnchor":     s.downloadLedgerAnchor,
	}
}

func (s *Server) needLedgerAnchoring() (LedgerAnchoring, error) {
	if s.deps.LedgerAnchoring == nil {
		return nil, problem.New(problem.UnsupportedOp, "This server does not anchor the ledger (no managed instance).")
	}
	return s.deps.LedgerAnchoring, nil
}

// anchorProblem keeps a problem as it is and hides every other error behind a 503: the text of an internal error is
// logged (it holds no credential, but could name a path), never sent.
func (s *Server) anchorProblem(r *http.Request, err error, creds ...[]byte) error {
	var p *problem.Problem
	if errors.As(err, &p) {
		return p
	}
	s.log.Error("ledger anchoring", "request_id", stateOf(r).id, "error", ledgeranchorText(err, creds...))
	return problem.New(problem.ServiceUnavailable, "The anchoring of the ledger cannot be read or changed now.").WithRetryAfter(5)
}

func ledgeranchorText(err error, creds ...[]byte) string {
	t := credRedactor(creds...).Clean(err.Error(), 300)
	return t
}

func (s *Server) listLedgerAnchorTargets(_ *call, w http.ResponseWriter, r *http.Request) error {
	la, err := s.needLedgerAnchoring()
	if err != nil {
		return err
	}
	out, err := la.Targets(r.Context())
	if err != nil {
		return s.anchorProblem(r, err)
	}
	s.writeJSON(w, r, http.StatusOK, out)
	return nil
}

func (s *Server) getLedgerAnchorTarget(_ *call, w http.ResponseWriter, r *http.Request) error {
	la, err := s.needLedgerAnchoring()
	if err != nil {
		return err
	}
	pp := newParams(r)
	id := pp.pathInt64("targetId")
	if p := pp.err(); p != nil {
		return p
	}
	out, err := la.Target(r.Context(), id)
	if err != nil {
		return s.anchorProblem(r, err)
	}
	s.writeJSON(w, r, http.StatusOK, out)
	return nil
}

func (s *Server) createLedgerAnchorTarget(c *call, w http.ResponseWriter, r *http.Request) error {
	la, err := s.needLedgerAnchoring()
	if err != nil {
		return err
	}
	var in api.LedgerAnchorTargetCreate
	if err := s.bodyClosed(w, r, &in); err != nil {
		return err
	}
	out, err := la.CreateTarget(r.Context(), c.session.Username, in)
	if err != nil {
		return s.anchorProblem(r, err, in.Credentials)
	}
	// the audit entry names the target and says that credentials were set; it never carries them
	c.addDetails(map[string]any{"targetId": out.ID, "kind": out.Kind, "name": out.Name, "credentialsChanged": credentialsGiven(in.Credentials)})
	w.Header().Set("Location", "/api/v1/ledger/anchor-targets/"+strconv.FormatInt(out.ID, 10))
	s.writeJSON(w, r, http.StatusCreated, out)
	return nil
}

func credentialsGiven(raw []byte) bool {
	t := bytes.TrimSpace(raw)
	return len(t) > 0 && !bytes.Equal(t, []byte("null"))
}

func (s *Server) updateLedgerAnchorTarget(c *call, w http.ResponseWriter, r *http.Request) error {
	la, err := s.needLedgerAnchoring()
	if err != nil {
		return err
	}
	pp := newParams(r)
	id := pp.pathInt64("targetId")
	if p := pp.err(); p != nil {
		return p
	}
	var in api.LedgerAnchorTargetUpdate
	if err := s.bodyClosed(w, r, &in); err != nil {
		return err
	}
	out, err := la.UpdateTarget(r.Context(), id, in)
	if err != nil {
		return s.anchorProblem(r, err, in.Credentials)
	}
	c.addDetails(map[string]any{"targetId": id, "credentialsChanged": credentialsGiven(in.Credentials)})
	s.writeJSON(w, r, http.StatusOK, out)
	return nil
}

func (s *Server) deleteLedgerAnchorTarget(c *call, w http.ResponseWriter, r *http.Request) error {
	la, err := s.needLedgerAnchoring()
	if err != nil {
		return err
	}
	pp := newParams(r)
	id := pp.pathInt64("targetId")
	if p := pp.err(); p != nil {
		return p
	}
	if err := la.DeleteTarget(r.Context(), id); err != nil {
		return s.anchorProblem(r, err)
	}
	c.addDetails(map[string]any{"targetId": id})
	w.WriteHeader(http.StatusNoContent)
	return nil
}

func (s *Server) testLedgerAnchorTarget(c *call, w http.ResponseWriter, r *http.Request) error {
	la, err := s.needLedgerAnchoring()
	if err != nil {
		return err
	}
	pp := newParams(r)
	id := pp.pathInt64("targetId")
	if p := pp.err(); p != nil {
		return p
	}
	var empty struct{}
	if r.Body != nil && r.Body != http.NoBody {
		if err := s.optionalBodyClosed(w, r, &empty); err != nil {
			return err
		}
	}
	out, err := la.TestTarget(r.Context(), id)
	if err != nil {
		return s.anchorProblem(r, err)
	}
	c.addDetails(map[string]any{"targetId": id, "ok": out.OK})
	s.writeJSON(w, r, http.StatusOK, out)
	return nil
}

func (s *Server) listLedgerAnchors(_ *call, w http.ResponseWriter, r *http.Request) error {
	la, err := s.needLedgerAnchoring()
	if err != nil {
		return err
	}
	pp := newParams(r)
	db := pp.pathName("databaseName")
	target := pp.int64Query("targetId", 1)
	state := pp.enum("state", "pending", "submitted", "anchored", "failed", "superseded", "expired")
	page := pp.page()
	if p := pp.err(); p != nil {
		return p
	}
	var tid int64
	if target != nil {
		tid = *target
	}
	out, err := la.List(r.Context(), db, tid, state, page)
	if err != nil {
		return s.anchorProblem(r, err)
	}
	s.writeJSON(w, r, http.StatusOK, out)
	return nil
}

func (s *Server) anchorLedgerNow(c *call, w http.ResponseWriter, r *http.Request) error {
	la, err := s.needLedgerAnchoring()
	if err != nil {
		return err
	}
	pp := newParams(r)
	db := pp.pathName("databaseName")
	if p := pp.err(); p != nil {
		return p
	}
	var empty struct{}
	if r.Body != nil && r.Body != http.NoBody {
		if err := s.optionalBodyClosed(w, r, &empty); err != nil {
			return err
		}
	}
	out, err := la.AnchorNow(r.Context(), db)
	if err != nil {
		return s.anchorProblem(r, err)
	}
	c.addDetails(map[string]any{"database": db, "records": len(out.Items)})
	s.writeJSON(w, r, http.StatusCreated, out)
	return nil
}

func (s *Server) retryLedgerAnchor(c *call, w http.ResponseWriter, r *http.Request) error {
	la, err := s.needLedgerAnchoring()
	if err != nil {
		return err
	}
	pp := newParams(r)
	db := pp.pathName("databaseName")
	id := pp.pathInt64("anchorId")
	if p := pp.err(); p != nil {
		return p
	}
	var empty struct{}
	if r.Body != nil && r.Body != http.NoBody {
		if err := s.optionalBodyClosed(w, r, &empty); err != nil {
			return err
		}
	}
	out, err := la.Retry(r.Context(), db, id)
	if err != nil {
		return s.anchorProblem(r, err)
	}
	c.addDetails(map[string]any{"database": db, "anchorId": id})
	s.writeJSON(w, r, http.StatusOK, out)
	return nil
}

func (s *Server) downloadLedgerAnchor(_ *call, w http.ResponseWriter, r *http.Request) error {
	la, err := s.needLedgerAnchoring()
	if err != nil {
		return err
	}
	pp := newParams(r)
	db := pp.pathName("databaseName")
	id := pp.pathInt64("anchorId")
	format := pp.enum("format", "json", "ots", "tsr")
	if p := pp.err(); p != nil {
		return p
	}
	ev, err := la.Evidence(r.Context(), db, id, format)
	if err != nil {
		return s.anchorProblem(r, err)
	}
	w.Header().Set("Content-Type", ev.ContentType)
	w.Header().Set("Content-Disposition", `attachment; filename="`+ev.FileName+`"`)
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(ev.Data)
	return nil
}

// credRedactor removes the string values of a credentials object (and nothing else) from a text; a request that failed is logged through it.
func credRedactor(raw ...[]byte) anchor.Redactor {
	var secrets []anchor.Secret
	for _, r := range raw {
		var m map[string]json.RawMessage
		if json.Unmarshal(r, &m) != nil {
			continue
		}
		for _, v := range m {
			var s string
			if json.Unmarshal(v, &s) == nil {
				secrets = append(secrets, anchor.Secret(s))
			}
		}
	}
	return anchor.NewRedactor(secrets...)
}
