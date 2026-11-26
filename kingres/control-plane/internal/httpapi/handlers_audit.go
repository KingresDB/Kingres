// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package httpapi

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/api"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/audit"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/contract"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/gather"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/peerclient"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/problem"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/store"
)

func isNotFound(err error) bool { return errors.Is(err, store.ErrNotFound) }

// gatheredAudit is what a server answers to the gather call of listAuditEvents:
// its own events, with cluster-wide ids.
type gatheredAudit struct {
	Items []api.AuditEvent `json:"items"`
}

// listAuditEvents lists the audit log of the whole cluster, newest first.
//
// Every server writes its own audit file (the actions of a forwarded request
// are written by the leader, sign-ins by the server that took them), so the
// answer is the merge of the files of all servers, like the cluster-wide logs:
// the server that receives the request asks the others in parallel and names
// those that did not answer (partial, missingServers). Event ids carry the
// ordinal of the server that holds the event, and the order is (timestamp, id),
// so a page boundary is the same whichever servers answered.
func (s *Server) listAuditEvents(c *call, w http.ResponseWriter, r *http.Request) error {
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
	if page.Cursor != "" {
		pos, err := gather.DecodeAuditCursor(page.Cursor)
		if err != nil {
			pp.fail("cursor", "not a cursor issued by this server", "invalid_cursor")
		} else {
			f.Below = &pos
		}
	}
	if p := pp.err(); p != nil {
		return p
	}
	ordinal := s.selfOrdinal(r.Context())
	if g := gatheredOf(r); g != nil {
		ordinal = g.ordinal
	}
	f.Ordinal = ordinal
	local, err := s.deps.Audit.Query(r.Context(), f)
	if err != nil {
		s.log.Error("reading audit log", "request_id", stateOf(r).id, "error", err)
		return s.storeProblem(r, err, "")
	}
	self := s.localServerID(r.Context())
	for i := range local {
		local[i].ID = audit.GlobalID(ordinal, local[i].ID)
		local[i].ServerID = self
	}
	if gatheredOf(r) != nil {
		s.writeJSON(w, r, http.StatusOK, gatheredAudit{Items: local})
		return nil
	}

	lists := [][]api.AuditEvent{local}
	var missing []gather.Missing
	if s.deps.Gatherer != nil {
		peers, err := s.otherServers(r.Context(), self, "")
		if err != nil {
			s.log.Warn("the list of servers cannot be read; answering for this server only", "request_id", stateOf(r).id, "error", err)
		}
		out := s.askAll(c, r, "listAuditEvents", peers, maxGatherAnswer)
		for _, o := range out {
			if o.Err != nil {
				continue
			}
			var ga gatheredAudit
			if json.Unmarshal(o.Body, &ga) != nil {
				missing = append(missing, gather.Missing{ServerID: o.Peer.ID, Reason: gather.ReasonInvalid})
				continue
			}
			lists = append(lists, cleanPeerAudit(o.Peer, ga.Items, f.Limit+1))
		}
		missing = append(missing, gather.MissingOf(out)...)
	}
	items, next := gather.MergeAudit(lists, f.Below, f.Limit)
	res := api.AuditPage{Items: items, Partial: len(missing) > 0, MissingServers: missingToAPI(missing)}
	if next != nil {
		cur := gather.EncodeAuditCursor(*next)
		res.NextCursor = &cur
	}
	s.writeJSON(w, r, http.StatusOK, res)
	return nil
}

// cleanPeerAudit makes the events a server answered safe to relay: they are
// about that server only (the ordinal in the id and the server id are set from
// what this server knows of it), their texts have no control characters, and
// there are at most max of them.
func cleanPeerAudit(p gather.Peer, in []api.AuditEvent, max int) []api.AuditEvent {
	out := make([]api.AuditEvent, 0, min(len(in), max))
	for _, e := range in {
		if len(out) >= max {
			break
		}
		if ord, local := audit.SplitID(e.ID); ord != p.Ordinal || local <= 0 || e.Timestamp.IsZero() {
			continue
		}
		id := p.ID
		e.ServerID = &id
		e.Actor = peerclient.CleanText(e.Actor, 255)
		e.ActorAddress = peerclient.CleanText(e.ActorAddress, 255)
		e.ResourceType = peerclient.CleanText(e.ResourceType, 255)
		e.ResourceID = peerclient.CleanText(e.ResourceID, 1024)
		e.Action = peerclient.CleanText(e.Action, 255)
		e.RequestID = peerclient.CleanText(e.RequestID, 255)
		switch e.Source {
		case "panel", "cli", "rest", "system":
		default:
			e.Source = "system"
		}
		switch e.Outcome {
		case audit.OutcomeSuccess, audit.OutcomeFailure, audit.OutcomeDenied:
		default:
			e.Outcome = audit.OutcomeFailure
		}
		out = append(out, e)
	}
	return out
}

// auditReady refuses a state-changing operation while the audit log cannot take
// its record. The operation is audited after it ran (with its outcome), so a
// write that fails then cannot be undone; the check before it makes that case
// rare (a disk that fills between the two) instead of the normal consequence of
// a broken log. Reads, sign-in and the operations that are not audited do not
// need it.
func (s *Server) auditReady(c *call, r *http.Request) *problem.Problem {
	if !c.audit || !c.op.Mutating() {
		return nil
	}
	if err := s.deps.Audit.Ready(r.Context()); err != nil {
		c.audit = false // the log that cannot take the record cannot take a refusal either
		s.log.Error("the audit log cannot be written; refusing the operation", "request_id", stateOf(r).id, "operation", c.op.ID, "error", err)
		return problem.New(problem.ServiceUnavailable, "The audit log cannot be written, so no action is taken.").WithRetryAfter(30)
	}
	return nil
}
