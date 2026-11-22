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
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/rebalance"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/task"
)

// Rebalancer is what the HTTP layer needs of the rebalance (internal/rebalance).
// The operations that change anything run on the manager leader (the guard
// forwards them); the reads are answered by the leader too, because the run's
// records are in its system database.
type Rebalancer interface {
	Status(ctx context.Context) (api.RebalanceStatus, error)
	Preview(ctx context.Context, req api.RebalancePlanRequest) (api.RebalancePlan, error)
	Start(ctx context.Context, actor string, o rebalance.Options) (task.Task, error)
	Stop(ctx context.Context, actor string) error
	Reports(ctx context.Context, limit int, cursor string) (api.RebalanceReportPage, error)
	Report(ctx context.Context, id int64) (api.RebalanceReport, error)
}

// Drainer is what the HTTP layer needs of the drain of a server; the rebalance service implements it next to Rebalancer.
type Drainer interface {
	DrainPreview(ctx context.Context, serverID string) (api.DrainPreview, error)
	Drain(ctx context.Context, actor, serverID string, req rebalance.DrainRequest) (task.Task, error)
}

func (s *Server) needDrain() (Drainer, error) {
	rb, err := s.needRebalance()
	if err != nil {
		return nil, err
	}
	d, ok := rb.(Drainer)
	if !ok {
		return nil, problem.New(problem.UnsupportedOp, "This server cannot drain a server.")
	}
	return d, nil
}

func (s *Server) previewServerDrain(_ *call, w http.ResponseWriter, r *http.Request) error {
	d, err := s.needDrain()
	if err != nil {
		return err
	}
	pp := newParams(r)
	id := pp.pathServerID("serverId")
	if p := pp.err(); p != nil {
		return p
	}
	out, err := d.DrainPreview(r.Context(), id)
	if err != nil {
		return s.rebalanceProblem(r, err)
	}
	s.writeJSON(w, r, http.StatusOK, out)
	return nil
}

func (s *Server) drainServer(c *call, w http.ResponseWriter, r *http.Request) error {
	d, err := s.needDrain()
	if err != nil {
		return err
	}
	pp := newParams(r)
	id := pp.pathServerID("serverId")
	if p := pp.err(); p != nil {
		return p
	}
	var req api.DrainRequest
	if err := s.optionalBody(w, r, &req); err != nil {
		return err
	}
	if req.MaxConcurrentMoves < 0 || req.MaxConcurrentMoves > 64 {
		return problem.Validation("The drain request is invalid.", problem.FieldError{Field: "/maxConcurrentMoves", Message: "must be 1 to 64", Code: "out_of_range"})
	}
	t, err := d.Drain(r.Context(), c.session.Username, id, rebalance.DrainRequest{RemoveWhenDone: req.RemoveWhenDone,
		MaxConcurrentMoves: req.MaxConcurrentMoves, AcceptFewerReplicas: req.AcceptFewerReplicas, AcknowledgeLocal: req.AcknowledgeLocalData})
	if err != nil {
		return s.rebalanceProblem(r, err)
	}
	c.addDetails(map[string]any{"taskId": t.ID, "serverId": id, "removeWhenDone": req.RemoveWhenDone})
	s.accepted(w, r, t)
	return nil
}

// Splitter is what the HTTP layer needs of the split of a bucket; the rebalance service implements it.
type Splitter interface {
	SplitPreview(ctx context.Context, db string, colocation, bucket int32, req rebalance.SplitRequest) (api.BucketSplitPreview, error)
	Split(ctx context.Context, actor, db string, colocation, bucket int32, req rebalance.SplitRequest) (task.Task, error)
}

func (s *Server) needSplit() (Splitter, error) {
	rb, err := s.needRebalance()
	if err != nil {
		return nil, err
	}
	sp, ok := rb.(Splitter)
	if !ok {
		return nil, problem.New(problem.UnsupportedOp, "This server cannot split buckets.")
	}
	return sp, nil
}

func (s *Server) splitArgs(w http.ResponseWriter, r *http.Request) (db string, colocation, bucket int32, req api.BucketSplitRequest, err error) {
	pp := newParams(r)
	db = pp.pathName("databaseName")
	colocation = pp.pathInt32("colocationId")
	bucket = pp.pathInt32("bucket")
	if p := pp.err(); p != nil {
		return db, colocation, bucket, req, p
	}
	err = s.optionalBody(w, r, &req)
	return db, colocation, bucket, req, err
}

func (s *Server) previewBucketSplit(_ *call, w http.ResponseWriter, r *http.Request) error {
	sp, err := s.needSplit()
	if err != nil {
		return err
	}
	db, colo, bucket, req, err := s.splitArgs(w, r)
	if err != nil {
		return err
	}
	out, err := sp.SplitPreview(r.Context(), db, colo, bucket, rebalance.SplitRequest{Parts: req.Parts, Verify: req.Verify})
	if err != nil {
		return s.rebalanceProblem(r, err)
	}
	s.writeJSON(w, r, http.StatusOK, out)
	return nil
}

func (s *Server) splitBucket(c *call, w http.ResponseWriter, r *http.Request) error {
	sp, err := s.needSplit()
	if err != nil {
		return err
	}
	db, colo, bucket, req, err := s.splitArgs(w, r)
	if err != nil {
		return err
	}
	t, err := sp.Split(r.Context(), c.session.Username, db, colo, bucket, rebalance.SplitRequest{Parts: req.Parts, Verify: req.Verify})
	if err != nil {
		return s.rebalanceProblem(r, err)
	}
	c.addDetails(map[string]any{"taskId": t.ID, "databaseName": db, "colocationId": colo, "bucket": bucket, "parts": req.Parts})
	s.accepted(w, r, t)
	return nil
}

func (s *Server) rebalanceHandlers() map[string]handlerFunc {
	return map[string]handlerFunc{
		"previewBucketSplit":   s.previewBucketSplit,
		"splitBucket":          s.splitBucket,
		"drainServer":          s.drainServer,
		"previewServerDrain":   s.previewServerDrain,
		"getRebalanceStatus":   s.getRebalanceStatus,
		"startRebalance":       s.startRebalance,
		"previewRebalancePlan": s.previewRebalancePlan,
		"stopRebalance":        s.stopRebalance,
		"listRebalanceReports": s.listRebalanceReports,
		"getRebalanceReport":   s.getRebalanceReport,
	}
}

func (s *Server) needRebalance() (Rebalancer, error) {
	if s.deps.Rebalance == nil {
		return nil, problem.New(problem.UnsupportedOp, "This server cannot rebalance (no managed instance or no internal API).")
	}
	return s.deps.Rebalance, nil
}

// optionalBody decodes a JSON body into dst when there is one.
func (s *Server) optionalBody(w http.ResponseWriter, r *http.Request, dst any) error {
	if r.Body == nil || r.Body == http.NoBody {
		return nil
	}
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, s.deps.MaxBodyBytes))
	if err != nil {
		return problem.New(problem.BadRequest, "The request body could not be read.")
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil
	}
	r.Body = io.NopCloser(bytes.NewReader(raw))
	return s.body(w, r, dst)
}

func (s *Server) rebalanceProblem(r *http.Request, err error) error {
	var p *problem.Problem
	if errors.As(err, &p) {
		return p
	}
	s.log.Error("rebalance", "request_id", stateOf(r).id, "error", err)
	return problem.New(problem.ServiceUnavailable, "The rebalance state cannot be read now.").WithRetryAfter(5)
}

func (s *Server) getRebalanceStatus(_ *call, w http.ResponseWriter, r *http.Request) error {
	rb, err := s.needRebalance()
	if err != nil {
		return err
	}
	st, err := rb.Status(r.Context())
	if err != nil {
		return s.rebalanceProblem(r, err)
	}
	s.writeJSON(w, r, http.StatusOK, st)
	return nil
}

func (s *Server) previewRebalancePlan(_ *call, w http.ResponseWriter, r *http.Request) error {
	rb, err := s.needRebalance()
	if err != nil {
		return err
	}
	var req api.RebalancePlanRequest
	if err := s.optionalBody(w, r, &req); err != nil {
		return err
	}
	plan, err := rb.Preview(r.Context(), req)
	if err != nil {
		return s.rebalanceProblem(r, err)
	}
	s.writeJSON(w, r, http.StatusOK, plan)
	return nil
}

func (s *Server) startRebalance(c *call, w http.ResponseWriter, r *http.Request) error {
	rb, err := s.needRebalance()
	if err != nil {
		return err
	}
	var req api.RebalanceOptions
	if err := s.optionalBody(w, r, &req); err != nil {
		return err
	}
	t, err := rb.Start(r.Context(), c.session.Username, rebalance.OptionsFromAPI(req))
	if err != nil {
		return s.rebalanceProblem(r, err)
	}
	c.addDetails(map[string]any{"taskId": t.ID})
	s.accepted(w, r, t)
	return nil
}

func (s *Server) stopRebalance(c *call, w http.ResponseWriter, r *http.Request) error {
	rb, err := s.needRebalance()
	if err != nil {
		return err
	}
	if err := rb.Stop(r.Context(), c.session.Username); err != nil {
		return s.rebalanceProblem(r, err)
	}
	st, err := rb.Status(r.Context())
	if err != nil {
		return s.rebalanceProblem(r, err)
	}
	s.writeJSON(w, r, http.StatusOK, st)
	return nil
}

func (s *Server) listRebalanceReports(_ *call, w http.ResponseWriter, r *http.Request) error {
	rb, err := s.needRebalance()
	if err != nil {
		return err
	}
	pp := newParams(r)
	pg := pp.page()
	if p := pp.err(); p != nil {
		return p
	}
	page, err := rb.Reports(r.Context(), pg.Limit, pg.Cursor)
	if err != nil {
		return s.rebalanceProblem(r, err)
	}
	s.writeJSON(w, r, http.StatusOK, page)
	return nil
}

func (s *Server) getRebalanceReport(_ *call, w http.ResponseWriter, r *http.Request) error {
	rb, err := s.needRebalance()
	if err != nil {
		return err
	}
	pp := newParams(r)
	id := pp.pathInt64("reportId")
	if p := pp.err(); p != nil {
		return p
	}
	rep, err := rb.Report(r.Context(), id)
	if err != nil {
		return s.rebalanceProblem(r, err)
	}
	s.writeJSON(w, r, http.StatusOK, rep)
	return nil
}
