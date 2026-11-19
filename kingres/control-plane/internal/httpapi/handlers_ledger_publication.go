// SPDX-License-Identifier: AGPL-3.0-only

package httpapi

import (
	"context"
	"errors"
	"net/http"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/api"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/problem"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/store"
)

// LedgerPublisher is what the HTTP layer needs of the publication of the ledger (internal/ledgerround): the aggregate of a database, the
// list of its aggregates and the state of the publication. The data is in the system database of the manager leader, so these reads are
// answered by the leader (forward.go). Nothing in an answer holds a secret or a row of a user database.
type LedgerPublisher interface {
	Aggregate(ctx context.Context, db string, size *int64) (api.LedgerAggregate, error)
	Aggregates(ctx context.Context, db string, p store.Page) (api.LedgerAggregatePage, error)
	Publication(ctx context.Context) (api.LedgerPublication, error)
}

func (s *Server) ledgerPublicationHandlers() map[string]handlerFunc {
	return map[string]handlerFunc{
		"getLedgerPublication": s.getLedgerPublication,
		"getLedgerAggregate":   s.getLedgerAggregate,
		"listLedgerAggregates": s.listLedgerAggregates,
	}
}

func (s *Server) needLedgerPublisher() (LedgerPublisher, error) {
	if s.deps.LedgerPublication == nil {
		return nil, problem.New(problem.UnsupportedOp, "This server does not publish the ledger (no managed instance).")
	}
	return s.deps.LedgerPublication, nil
}

// publicationProblem keeps a problem and hides every other error behind a 503.
func (s *Server) publicationProblem(r *http.Request, err error) error {
	var p *problem.Problem
	if errors.As(err, &p) {
		return p
	}
	s.log.Error("ledger publication", "request_id", stateOf(r).id, "error", err)
	return problem.New(problem.ServiceUnavailable, "The publication of the ledger cannot be read now.").WithRetryAfter(5)
}

func (s *Server) getLedgerPublication(_ *call, w http.ResponseWriter, r *http.Request) error {
	lp, err := s.needLedgerPublisher()
	if err != nil {
		return err
	}
	out, err := lp.Publication(r.Context())
	if err != nil {
		return s.publicationProblem(r, err)
	}
	s.writeJSON(w, r, http.StatusOK, out)
	return nil
}

func (s *Server) getLedgerAggregate(_ *call, w http.ResponseWriter, r *http.Request) error {
	lp, err := s.needLedgerPublisher()
	if err != nil {
		return err
	}
	pp := newParams(r)
	db := pp.pathName("databaseName")
	size := pp.int64Query("size", 1)
	if p := pp.err(); p != nil {
		return p
	}
	out, err := lp.Aggregate(r.Context(), db, size)
	if err != nil {
		return s.publicationProblem(r, err)
	}
	s.writeJSON(w, r, http.StatusOK, out)
	return nil
}

func (s *Server) listLedgerAggregates(_ *call, w http.ResponseWriter, r *http.Request) error {
	lp, err := s.needLedgerPublisher()
	if err != nil {
		return err
	}
	pp := newParams(r)
	db := pp.pathName("databaseName")
	page := pp.page()
	if p := pp.err(); p != nil {
		return p
	}
	out, err := lp.Aggregates(r.Context(), db, page)
	if err != nil {
		return s.publicationProblem(r, err)
	}
	s.writeJSON(w, r, http.StatusOK, out)
	return nil
}
