// SPDX-License-Identifier: PostgreSQL

package httpapi

import (
	"errors"
	"net/http"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/api"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/audit"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/contract"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/store"
)

func isNotFound(err error) bool { return errors.Is(err, store.ErrNotFound) }

// listAuditEvents lists the audit log newest first. The cursor is a keyset
// cursor on the event id.
func (s *Server) listAuditEvents(_ *call, w http.ResponseWriter, r *http.Request) error {
	pp := newParams(r)
	page := pp.page()
	f := audit.Filter{
		From:    pp.timeQuery("from"),
		To:      pp.timeQuery("to"),
		Actor:   pp.maxLen("actor", 63),
		Action:  pp.enum("action", contract.AuditActions()...),
		Outcome: pp.enum("outcome", audit.OutcomeSuccess, audit.OutcomeFailure, audit.OutcomeDenied),
		Limit:   page.Limit,
	}
	before, bounded, cerr := store.DecodeKey(page.Cursor)
	if cerr != nil {
		pp.fail("cursor", "not a cursor issued by this server", "invalid_cursor")
	}
	if p := pp.err(); p != nil {
		return p
	}
	if bounded {
		f.BeforeID = before
	}
	events, err := s.deps.Audit.Query(r.Context(), f)
	if err != nil {
		s.log.Error("reading audit log", "request_id", stateOf(r).id, "error", err)
		return s.storeProblem(r, err, "")
	}
	out := api.AuditPage{Items: make([]api.AuditEvent, 0, len(events))}
	if len(events) > page.Limit {
		next := store.EncodeKey(events[page.Limit-1].ID)
		out.NextCursor = &next
		events = events[:page.Limit]
	}
	out.Items = append(out.Items, events...)
	s.writeJSON(w, r, http.StatusOK, out)
	return nil
}
