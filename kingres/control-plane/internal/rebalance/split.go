// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026 RooyeKhat Media Co

package rebalance

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/api"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/problem"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/task"
)

// Splitting a bucket (design shard-phase10.md, sections 3, 9.3 and 21.11). The extension does the work:
// kingres.split_bucket(colocation_id, bucket, parts, verify) on the map group primary, in a session that stays
// open for the whole split, with its record in kingres.bucket_split. This file is what kingresd adds: the
// preview, the checks before a task exists, the task that calls it, follows its progress and settles it after a
// crash with resume_bucket_splits() and cancel_bucket_split().

// Task kind and step names of a split.
const (
	SplitKind       = "bucket.split"
	SplitSubject    = "bucket"
	StepSplitCheck  = "Check that the split can start"
	StepSplitRun    = "Split the bucket"
	StepSplitVerify = "Check the new buckets"
)

// The limits of the extension (README of kingres_shard, "Limits"): bucket numbers up to 9999 and the greatest
// modulus 65536; they are checked here to refuse before a task exists, and again by the extension.
const (
	maxBucketNumber = 9999
	maxModulusLimit = 65536
)

// SplitRequest is the body of splitBucket and previewBucketSplit.
type SplitRequest struct {
	Parts  int
	Verify string
}

type splitInput struct {
	// Modulus is the modulus of the bucket when the split was asked for (the children have Modulus * Parts).
	Modulus    int    `json:"modulus"`
	Database   string `json:"database"`
	Colocation int32  `json:"colocationId"`
	Bucket     int32  `json:"bucket"`
	Parts      int    `json:"parts"`
	Verify     string `json:"verify"`
}

// SplitSubjectID is the subject id of the task of a split.
func SplitSubjectID(db string, colocation, bucket int32) string {
	return db + "/" + strconv.Itoa(int(colocation)) + "/" + strconv.Itoa(int(bucket))
}

func (r *SplitRequest) normalize() []problem.FieldError {
	var errs []problem.FieldError
	if r.Parts == 0 {
		r.Parts = 2
	}
	if r.Parts != 2 && r.Parts != 4 && r.Parts != 8 && r.Parts != 16 {
		errs = append(errs, problem.FieldError{Field: "/parts", Message: "must be 2, 4, 8 or 16", Code: "enum"})
	}
	if r.Verify == "" {
		r.Verify = "count"
	}
	if r.Verify != "count" && r.Verify != "none" {
		errs = append(errs, problem.FieldError{Field: "/verify", Message: "must be count or none", Code: "enum"})
	}
	return errs
}

// bucketRow is one row of the bucket map.
type bucketRow struct {
	Group   int32
	State   string
	Modulus int
	Kind    string
	Base    int
	Max     int
}

// readBucket reads the bucket from the catalog of db (404 for a colocation group or bucket that does not exist).
func (s *Service) readBucket(ctx context.Context, db string, colocation, bucket int32) (bucketRow, error) {
	var b bucketRow
	err := s.Backend.with(ctx, db, func(c *pgx.Conn) error {
		return c.QueryRow(ctx, `SELECT m.group_id, m.state, m.modulus, co.kind::text, co.bucket_count, co.max_modulus
			FROM kingres.bucket_map m JOIN kingres.colocation co ON co.colocation_id = m.colocation_id
			WHERE m.colocation_id = $1 AND m.bucket = $2`, colocation, bucket).Scan(&b.Group, &b.State, &b.Modulus, &b.Kind, &b.Base, &b.Max)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return b, problem.New(problem.NotFound, fmt.Sprintf("Bucket %d of colocation group %d does not exist in database %s.", bucket, colocation, db))
	}
	var pe *pgconn.PgError
	if errors.As(err, &pe) && (pe.Code == "42P01" || pe.Code == "42703" || pe.Code == "3D000") {
		return b, problem.New(problem.NotFound, "Database "+db+" has no sharding catalog of version 0.8.")
	}
	return b, err
}

// SplitChildren are the buckets a split makes: the bucket keeps its number for i = 0, the others are
// bucket + i * modulus, all with the modulus times parts.
func SplitChildren(bucket int32, modulus, parts int) []api.BucketSplitChild {
	out := make([]api.BucketSplitChild, 0, parts)
	for i := 0; i < parts; i++ {
		b := int(bucket) + i*modulus
		out = append(out, api.BucketSplitChild{Bucket: b, Modulus: modulus * parts, Remainder: b})
	}
	return out
}

// bucketSize reads the size of the bucket's partitions of every table of its colocation group from the extension's
// own plan inputs (the sizes the plan by disk size uses); nil when they cannot be read.
func (s *Service) bucketSize(ctx context.Context, db string, colocation, bucket int32) *int64 {
	var size *int64
	_ = s.Backend.with(ctx, db, func(c *pgx.Conn) error {
		return c.QueryRow(ctx, `SELECT (i->>'size')::bigint FROM jsonb_array_elements(kingres.internal_plan_inputs('by_disk_size', NULL, NULL, ARRAY[$1::int])->'items') i
			WHERE (i->>'c')::int = $1 AND (i->>'b')::int = $2`, colocation, bucket).Scan(&size)
	})
	return size
}

// SplitPreview is previewBucketSplit.
func (s *Service) SplitPreview(ctx context.Context, db string, colocation, bucket int32, req SplitRequest) (api.BucketSplitPreview, error) {
	if errs := req.normalize(); len(errs) > 0 {
		return api.BucketSplitPreview{}, problem.Validation("The split request is invalid.", errs...)
	}
	b, err := s.readBucket(ctx, db, colocation, bucket)
	if err != nil {
		return api.BucketSplitPreview{}, err
	}
	out := api.BucketSplitPreview{Parts: req.Parts, Children: SplitChildren(bucket, b.Modulus, req.Parts), Blockers: []api.DrainBlocker{}}
	size := s.bucketSize(ctx, db, colocation, bucket)
	out.EstimatedSizeBytes = size
	if size != nil {
		each := *size / int64(req.Parts)
		for i := range out.Children {
			out.Children[i].EstimatedSizeBytes = &each
		}
		// the children are written beside the bucket until the old partitions are dropped (design 4.7, example 4)
		extra := *size
		out.ExtraDiskBytes = &extra
	}
	block := func(code problem.Code, detail string) {
		out.Blockers = append(out.Blockers, api.DrainBlocker{Code: string(code), Detail: detail})
	}
	switch {
	case b.Kind == "schema":
		block(problem.SplitUnsupported, "The bucket of a distributed schema cannot be split: a schema lives in one shard group.")
	case b.State != "active":
		block(problem.BucketBusy, "The bucket is "+b.State+"; wait until its move or split has ended.")
	}
	if last := int(bucket) + (req.Parts-1)*b.Modulus; last > maxBucketNumber {
		block(problem.SplitUnsupported, fmt.Sprintf("The new buckets would be numbered up to %d; bucket numbers are limited to %d.", last, maxBucketNumber))
	}
	if b.Modulus*req.Parts > maxModulusLimit {
		block(problem.SplitUnsupported, fmt.Sprintf("The greatest modulus of a group is %d; this split would make %d.", maxModulusLimit, b.Modulus*req.Parts))
	}
	if s.unfinishedSplit(ctx, db, colocation) {
		block(problem.BucketBusy, "A split of this colocation group is unfinished; one split runs at a time.")
	}
	if has, _ := s.hasPrimary(ctx, db, b.Group); !has {
		block(problem.GroupDegraded, fmt.Sprintf("Shard group %d has no active primary.", b.Group))
	}
	if size != nil && s.Backend.Disk != nil {
		server := s.Backend.ServerOf(ctx, db, b.Group)
		if used, total := s.Backend.Disk(ctx, server); used != nil && total != nil && *total > 0 {
			limit := DefaultMaxDiskUsedFraction
			if float64(*used)+1.2*float64(*size) > limit*float64(*total) {
				block(problem.InsufficientDisk, fmt.Sprintf("Server %s would be fuller than %.0f percent: a split needs room for 1.2 times the bucket (%d bytes) until the old partitions are dropped.", server, limit*100, *size))
			}
		}
	}
	out.CanSplit = len(out.Blockers) == 0
	return out, nil
}

func (s *Service) hasPrimary(ctx context.Context, db string, group int32) (bool, error) {
	var has bool
	err := s.Backend.with(ctx, db, func(c *pgx.Conn) error {
		return c.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM kingres.node WHERE group_id = $1 AND role = 'primary' AND is_active)`, group).Scan(&has)
	})
	return has, err
}

// unfinishedSplit tells whether a split of the colocation group is not finished (one at a time per group).
func (s *Service) unfinishedSplit(ctx context.Context, db string, colocation int32) bool {
	var n int
	_ = s.Backend.with(ctx, db, func(c *pgx.Conn) error {
		return c.QueryRow(ctx, `SELECT count(*) FROM kingres.bucket_split WHERE colocation_id = $1 AND phase NOT IN ('cleaning', 'done', 'failed', 'cancelled')`, colocation).Scan(&n)
	})
	return n > 0
}

// Split is splitBucket: refuses what the preview calls a blocker with the problem of the first one, and
// starts the task.
func (s *Service) Split(ctx context.Context, actor, db string, colocation, bucket int32, req SplitRequest) (task.Task, error) {
	if errs := req.normalize(); len(errs) > 0 {
		return task.Task{}, problem.Validation("The split request is invalid.", errs...)
	}
	pv, err := s.SplitPreview(ctx, db, colocation, bucket, req)
	if err != nil {
		return task.Task{}, err
	}
	if len(pv.Blockers) > 0 {
		b := pv.Blockers[0]
		return task.Task{}, problem.New(problem.Code(b.Code), b.Detail)
	}
	// a rebalance that has a move of this bucket is refused by the extension (the bucket is "moving"); a run that
	// plans it is not an obstacle: the split is for an idle bucket (design 9.3)
	eng := s.Engine()
	if eng == nil {
		return task.Task{}, problem.New(problem.ServiceUnavailable, "The task engine is not running yet.").WithRetryAfter(5)
	}
	b, err := s.readBucket(ctx, db, colocation, bucket)
	if err != nil {
		return task.Task{}, err
	}
	in := splitInput{Modulus: b.Modulus, Database: db, Colocation: colocation, Bucket: bucket, Parts: req.Parts, Verify: req.Verify}
	t, err := eng.Submit(ctx, SplitKind, task.Subject{Type: SplitSubject, ID: SplitSubjectID(db, colocation, bucket)}, in, actor)
	if errors.Is(err, task.ErrSubjectBusy) {
		return task.Task{}, problem.New(problem.BucketBusy, "A split of this bucket is already running.")
	}
	var x *task.ExcludedError
	if errors.As(err, &x) {
		return task.Task{}, x.Problem()
	}
	if err != nil {
		return task.Task{}, err
	}
	s.run(eng, t.ID)
	return t, nil
}

func (s *Service) splitDefinition() task.Definition {
	in := func(env *task.Env) (splitInput, error) {
		var v splitInput
		err := json.Unmarshal(env.Task.Input, &v)
		return v, err
	}
	return task.Definition{Kind: SplitKind, Steps: []task.Step{
		{Name: StepSplitCheck, Run: func(ctx context.Context, env *task.Env) error {
			v, err := in(env)
			if err != nil {
				return err
			}
			pv, err := s.SplitPreview(ctx, v.Database, v.Colocation, v.Bucket, SplitRequest{Parts: v.Parts, Verify: v.Verify})
			if err != nil {
				return err
			}
			if len(pv.Blockers) > 0 {
				// the split of this very task may be the unfinished one (a repeated step after a takeover)
				if _, found := s.findSplit(ctx, v); !found {
					return problem.New(problem.Code(pv.Blockers[0].Code), pv.Blockers[0].Detail)
				}
			}
			return nil
		}},
		{Name: StepSplitRun, Interruptible: true, Run: func(ctx context.Context, env *task.Env) error {
			v, err := in(env)
			if err != nil {
				return err
			}
			return s.runSplit(ctx, env, v)
		}},
		{Name: StepSplitVerify, Run: func(ctx context.Context, env *task.Env) error {
			v, err := in(env)
			if err != nil {
				return err
			}
			return s.verifySplit(ctx, v)
		}},
	}}
}

// splitRecord is the extension's record of a split.
type splitRecord struct {
	ID        int64
	Phase     string
	Detail    string
	RowsDone  int64
	RowsTotal *int64
}

// findSplit finds the newest record of a split of the bucket that has not ended in failure or cancellation.
func (s *Service) findSplit(ctx context.Context, v splitInput) (splitRecord, bool) {
	var r splitRecord
	var detail *string
	err := s.Backend.with(ctx, v.Database, func(c *pgx.Conn) error {
		return c.QueryRow(ctx, `SELECT split_id, phase, detail, rows_done, rows_total FROM kingres.bucket_split
			WHERE colocation_id = $1 AND bucket = $2 AND phase NOT IN ('failed', 'cancelled') ORDER BY split_id DESC LIMIT 1`, v.Colocation, v.Bucket).
			Scan(&r.ID, &r.Phase, &detail, &r.RowsDone, &r.RowsTotal)
	})
	if detail != nil {
		r.Detail = *detail
	}
	return r, err == nil
}

func (s *Service) readSplit(ctx context.Context, db string, id int64) (splitRecord, error) {
	r := splitRecord{ID: id}
	var detail *string
	err := s.Backend.with(ctx, db, func(c *pgx.Conn) error {
		return c.QueryRow(ctx, `SELECT phase, detail, rows_done, rows_total FROM kingres.bucket_split WHERE split_id = $1`, id).Scan(&r.Phase, &detail, &r.RowsDone, &r.RowsTotal)
	})
	if detail != nil {
		r.Detail = *detail
	}
	return r, err
}

func splitDone(phase string) bool { return phase == "cleaning" || phase == "done" }

// runSplit starts the split, or finds the one an earlier run of this step started, and follows it to its end.
func (s *Service) runSplit(ctx context.Context, env *task.Env, v splitInput) error {
	if rec, found := s.findSplit(ctx, v); found {
		if splitDone(rec.Phase) {
			return nil
		}
		// a split that an earlier holder started: its session may be gone; the extension settles those
		return s.followSplit(ctx, env, v, rec.ID, true)
	}
	type result struct {
		id  int64
		err error
	}
	finished := make(chan result, 1)
	// The call is made in a session of its own that stays open until the split has ended: its live mark is a
	// session lock, which is how the extension tells a running split from an abandoned one.
	go func() {
		var id int64
		err := s.Backend.with(context.WithoutCancel(ctx), v.Database, func(c *pgx.Conn) error {
			return c.QueryRow(context.WithoutCancel(ctx), `SELECT kingres.split_bucket($1, $2, $3, $4)`, v.Colocation, v.Bucket, v.Parts, v.Verify).Scan(&id)
		})
		finished <- result{id, err}
	}()
	for {
		select {
		case r := <-finished:
			return s.settleSplit(ctx, v, r.id, r.err)
		case <-ctx.Done():
			// the person cancelled, or the lease was lost: cancel what has not reached its switch
			if rec, found := s.findSplit(context.WithoutCancel(ctx), v); found && !splitDone(rec.Phase) {
				_ = s.Backend.with(context.WithoutCancel(ctx), v.Database, func(c *pgx.Conn) error {
					_, err := c.Exec(context.WithoutCancel(ctx), `SELECT kingres.cancel_bucket_split($1)`, rec.ID)
					return err
				})
			}
			return ctx.Err()
		case <-time.After(time.Second):
			if rec, found := s.findSplit(ctx, v); found {
				_ = env.SetDetail(ctx, splitDetail(rec))
			}
		}
	}
}

func splitDetail(r splitRecord) string {
	d := r.Phase
	if r.RowsTotal != nil && *r.RowsTotal > 0 {
		d += fmt.Sprintf(": %d of %d rows", r.RowsDone, *r.RowsTotal)
	}
	if r.Detail != "" {
		d += " (" + r.Detail + ")"
	}
	return d
}

// settleSplit turns the end of the call into the end of the step.
func (s *Service) settleSplit(ctx context.Context, v splitInput, id int64, callErr error) error {
	if callErr == nil {
		return nil
	}
	var pe *pgconn.PgError
	if errors.As(callErr, &pe) {
		msg := clipText(pe.Message, 500)
		switch {
		case pe.Code == "0A000" || strings.Contains(pe.Message, "primary key") || strings.Contains(pe.Message, "not supported"):
			return problem.New(problem.SplitUnsupported, msg)
		case strings.Contains(pe.Message, "is not active") || strings.Contains(pe.Message, "unfinished") || strings.Contains(pe.Message, "already"):
			return problem.New(problem.BucketBusy, msg)
		}
		return fmt.Errorf("the split failed: %s", msg)
	}
	// the connection broke: the extension's own recovery settles the record; ask for it and look
	_ = s.Backend.with(ctx, v.Database, func(c *pgx.Conn) error {
		_, err := c.Exec(ctx, `SELECT kingres.resume_bucket_splits()`)
		return err
	})
	return fmt.Errorf("the connection of the split broke; the extension settles it: %w", callErr)
}

// followSplit waits for a split that another run started; a record whose session is gone is settled with
// resume_bucket_splits() once the live mark is free.
func (s *Service) followSplit(ctx context.Context, env *task.Env, v splitInput, id int64, resume bool) error {
	for {
		rec, err := s.readSplit(ctx, v.Database, id)
		if err != nil {
			return err
		}
		switch {
		case splitDone(rec.Phase):
			return nil
		case rec.Phase == "failed" || rec.Phase == "cancelled":
			return fmt.Errorf("the split ended %s: %s", rec.Phase, rec.Detail)
		}
		_ = env.SetDetail(ctx, splitDetail(rec))
		if resume {
			_ = s.Backend.with(ctx, v.Database, func(c *pgx.Conn) error {
				_, err := c.Exec(ctx, `SELECT kingres.resume_bucket_splits()`)
				return err
			})
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

// verifySplit checks that the map has the children with the new modulus.
func (s *Service) verifySplit(ctx context.Context, v splitInput) error {
	var n int
	err := s.Backend.with(ctx, v.Database, func(c *pgx.Conn) error {
		return c.QueryRow(ctx, `SELECT count(*) FROM kingres.bucket_map WHERE colocation_id = $1 AND modulus = $3::int * $4::int AND bucket % $3::int = $2::int % $3::int`,
			v.Colocation, v.Bucket, v.Modulus, v.Parts).Scan(&n)
	})
	if err != nil {
		return err
	}
	if n < v.Parts {
		return fmt.Errorf("the bucket map has %d of the %d new buckets", n, v.Parts)
	}
	return nil
}
