// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package ops

// The task archive.validate (contract 0.29.0, design "Archive validation"): it reads an uploaded database archive once from end to end, checks it as the
// importer will, and stores the archive report with the upload. It touches no database but reads the versions of the target from the service role. The result is
// cached by the SHA-256 of the file: an archive whose report was made for the same bytes is not read again.

import (
	"context"
	"encoding/json"
	"errors"
	"sync"

	"github.com/jackc/pgx/v5"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/api"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/archive"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/problem"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/task"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/transfer"
)

// ValidatorVersion is the version of the validator (package archive) and of the shape grammar (package sqlshape) that the cached reports carry. Change it when
// either changes what it accepts: every cached report is then stale. The cache says what a validation found once; an import never trusts it.
const ValidatorVersion = "archive-1/sqlshape-1"

// MaxExpansion is how many times the size of the staged file the entries of an archive may add up to (a ratio of a deflate stream cannot honestly pass about
// 1000, and a CSV of a real table is 5 to 20); MaxEntryRatio is the same for one entry.
const (
	MaxExpansion  = 200
	MaxEntryRatio = 500
)

// archiveLimits are the limits of a validation of a file of this size: the smaller of the limit of the import and MaxExpansion times the file.
func archiveLimits(fileSize int64) archive.Limits {
	max := int64(100) << 30
	if m := fileSize * MaxExpansion; m < max {
		max = m
	}
	return archive.Limits{MaxImportBytes: max, MaxRatio: MaxEntryRatio}
}

// Step names of archive.validate.
const (
	stepVCheck  = "Check the upload"
	stepVRead   = "Read the archive"
	stepVPlan   = "Check the plan"
	stepVReport = "Store the report"
)

type validateParams struct {
	UploadID int64             `json:"uploadId"`
	Owner    transfer.Identity `json:"owner"`
}

// validation is what the steps pass to each other in memory; a step that does not find it makes it again.
type validation struct {
	res     *archive.Result
	err     error
	reasons []archive.Reason
	report  *api.ArchiveReport
}

type validations struct {
	mu sync.Mutex
	m  map[int64]*validation
}

func (o *Ops) validation(id int64) *validation {
	o.vals.mu.Lock()
	defer o.vals.mu.Unlock()
	return o.vals.m[id]
}

func (o *Ops) setValidation(id int64, v *validation) {
	o.vals.mu.Lock()
	defer o.vals.mu.Unlock()
	if o.vals.m == nil {
		o.vals.m = map[int64]*validation{}
	}
	if v == nil {
		delete(o.vals.m, id)
		return
	}
	o.vals.m[id] = v
}

// StartArchiveValidate answers validateDatabaseArchive.
func (o *Ops) StartArchiveValidate(ctx context.Context, actor, uploadID string) (task.Task, error) {
	if _, err := o.xfer(ctx); err != nil {
		return task.Task{}, err
	}
	who, err := o.identity(ctx, actor)
	if err != nil {
		return task.Task{}, err
	}
	if err := o.roleChecker().Check(ctx, who); err != nil {
		return task.Task{}, transfer.SafeError(err).Problem()
	}
	up, _, err := o.owned(ctx, actor, uploadID, transfer.KindUpload)
	if err != nil {
		return task.Task{}, err
	}
	if up.Purpose != "databaseArchive" {
		return task.Task{}, problem.Validation("The upload is not a database archive.", field("/uploadId", "the purpose of the upload is not databaseArchive", "conflict"))
	}
	switch up.State {
	case transfer.StateReceiving:
		return task.Task{}, problem.New(problem.UploadIncomplete, "The upload has not received all its bytes.")
	case transfer.StateReady:
	default:
		return task.Task{}, problem.New(problem.NotFound, "There is no such transfer.")
	}
	if o.taskActive(ctx, up) {
		return task.Task{}, problem.New(problem.OperationInProgress, "Another task is using this upload.")
	}
	st, err := o.settings(ctx)
	if err != nil {
		return task.Task{}, err
	}
	o.startMu.Lock()
	defer o.startMu.Unlock()
	if err := o.concurrency(ctx, st, actor); err != nil {
		return task.Task{}, err
	}
	t, err := o.StartTask(ctx, KindArchiveValidate, "upload", idText(up.ID), validateParams{UploadID: up.ID, Owner: who}, actor)
	if err != nil {
		return task.Task{}, err
	}
	tid := t.ID
	_ = o.records().Apply(ctx, up.ID, transfer.Update{TaskID: &tid})
	return t, nil
}

func (o *Ops) archiveValidateDefinition() task.Definition {
	return task.Definition{
		Kind: KindArchiveValidate,
		Steps: []task.Step{
			{Name: stepVCheck, Run: xferStep(o.stepVCheck)},
			{Name: stepVRead, Interruptible: true, Run: xferStep(o.stepVRead)},
			{Name: stepVPlan, Run: xferStep(o.stepVPlan)},
			{Name: stepVReport, Run: xferStep(o.stepVReport)},
		},
		OnFail: func(ctx context.Context, t task.Task) {
			p, err := inputOf[validateParams](t)
			if err != nil {
				return
			}
			o.setValidation(p.UploadID, nil)
			// An upload whose validation did not finish is a complete upload again.
			rec, gerr := o.records().Get(ctx, p.UploadID)
			if gerr == nil && rec.ArchiveState == "validating" {
				_ = o.records().Apply(context.WithoutCancel(ctx), rec.ID, transfer.Update{ArchiveState: strPtr("")})
			}
		},
	}
}

func strPtr(s string) *string { return &s }

func (o *Ops) loadValidate(ctx context.Context, env *task.Env) (validateParams, transfer.Record, *xferState, error) {
	p, err := inputOf[validateParams](env.Task)
	if err != nil {
		return p, transfer.Record{}, nil, err
	}
	x, err := o.xfer(ctx)
	if err != nil {
		return p, transfer.Record{}, nil, transfer.Errorf(transfer.CodeLeaderChanged, true, "the staging store of this server is not available; repeat the operation")
	}
	rec, err := o.records().Get(ctx, p.UploadID)
	if err != nil || rec.Kind != transfer.KindUpload {
		return p, rec, x, transfer.Errorf(transfer.CodeLeaderChanged, true, "the upload is not known to this server; repeat the operation")
	}
	if rec.ServerID != o.ServerHost || rec.State != transfer.StateReady {
		return p, rec, x, transfer.Errorf(transfer.CodeLeaderChanged, true, "the leader changed while the archive was being validated; repeat the operation")
	}
	if err := o.roleChecker().Check(ctx, rec.Owner); err != nil {
		return p, rec, x, err
	}
	return p, rec, x, nil
}

func (o *Ops) stepVCheck(ctx context.Context, env *task.Env) error {
	p, rec, _, err := o.loadValidate(ctx, env)
	if err != nil {
		return err
	}
	if err := o.stillEnabled(ctx); err != nil {
		return err
	}
	if rec.Purpose != "databaseArchive" {
		return transfer.Errorf(transfer.CodeForbidden, false, "the upload is not a database archive")
	}
	validating := "validating"
	if err := o.records().Apply(ctx, rec.ID, transfer.Update{ArchiveState: &validating}); err != nil {
		return err
	}
	return env.SetDetail(ctx, "upload "+idText(p.UploadID))
}

// cachedReport is the report that was made for these bytes, if there is one.
func cachedReport(rec transfer.Record) *api.ArchiveReport {
	var rep uploadReport
	if json.Unmarshal(rec.Report, &rep) != nil || rep.Archive == nil || rec.SHA256 == "" || rep.Archive.SHA256 != rec.SHA256 || rep.Version != ValidatorVersion {
		return nil
	}
	return rep.Archive
}

func (o *Ops) readArchive(ctx context.Context, x *xferState, rec transfer.Record) *validation {
	v := &validation{}
	dk, err := x.keys.Unwrap(rec.ID, rec.KeyID, rec.WrappedKey)
	if err != nil {
		v.err = transfer.Errorf(transfer.CodeKeyLost, false, "the key of the file is gone")
		return v
	}
	r, err := x.store.OpenReader(rec.ID, dk)
	if err != nil {
		v.err = transfer.Errorf(transfer.CodeLeaderChanged, true, "the file of the upload is not there; repeat the operation")
		return v
	}
	defer func() { _ = r.Close() }()
	lim := archiveLimits(r.Size())
	lim.SQLFor = func(cat *archive.Catalog) archive.SQLChecker { return sqlChecker(catalogInheritance(cat)) }
	res, err := archive.Validate(r, r.Size(), lim)
	if err != nil {
		var ae *archive.Error
		if errors.As(err, &ae) {
			v.err = ae
		} else if ctx.Err() != nil {
			v.err = ctx.Err()
		} else {
			v.err = transfer.Errorf(transfer.CodeArchiveInvalid, false, "the archive could not be read")
		}
		return v
	}
	res.FileSHA256 = rec.SHA256
	v.res = res
	return v
}

func (o *Ops) stepVRead(ctx context.Context, env *task.Env) error {
	_, rec, x, err := o.loadValidate(ctx, env)
	if err != nil {
		return err
	}
	if rep := cachedReport(rec); rep != nil {
		o.setValidation(rec.ID, &validation{report: rep})
		return env.SetDetail(ctx, "the report of this file was made before")
	}
	v := o.readArchive(ctx, x, rec)
	if v.err != nil && v.res == nil {
		var ae *archive.Error
		if !errors.As(v.err, &ae) {
			return v.err // a failure of the task: the key, the file, the context
		}
	}
	o.setValidation(rec.ID, v)
	return nil
}

// target is what this cluster can take: the PostgreSQL major, the extensions that can be installed and the formats that this build reads.
func (o *Ops) target(ctx context.Context) (archive.Target, error) {
	t := archive.Target{Extensions: map[string]string{}}
	err := o.asServiceRO(ctx, "postgres", func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `SELECT current_setting('server_version_num')::int / 10000`).Scan(&t.PostgresMajor); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `SELECT name::text, default_version FROM pg_available_extensions`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var n, v string
			if err := rows.Scan(&n, &v); err != nil {
				return err
			}
			t.Extensions[n] = v
		}
		return rows.Err()
	})
	return t, err
}

const claimedRows = "the row counts are those that the manifest claims; the records of the CSV entries were not counted"

func hasText(l []string, s string) bool {
	for _, x := range l {
		if x == s {
			return true
		}
	}
	return false
}

// validationOf is the validation of the file of this record: the one that an earlier step made in this process, or a new one. A failure that is not a verdict
// about the archive (the key, the file, a cancel, a read error) fails the task: a report is never made, or cached, from a read that did not finish.
func (o *Ops) validationOf(ctx context.Context, x *xferState, rec transfer.Record) (*validation, error) {
	v := o.validation(rec.ID)
	if v == nil {
		v = o.readArchive(ctx, x, rec)
		var ae *archive.Error
		if v.err != nil && !errors.As(v.err, &ae) {
			return nil, v.err
		}
		o.setValidation(rec.ID, v)
	}
	return v, nil
}

func (o *Ops) stepVPlan(ctx context.Context, env *task.Env) error {
	_, rec, x, err := o.loadValidate(ctx, env)
	if err != nil {
		return err
	}
	v, err := o.validationOf(ctx, x, rec)
	if err != nil {
		return err
	}
	if v.report != nil || v.res == nil {
		return nil
	}
	tgt, err := o.target(ctx)
	if err != nil {
		return transfer.Errorf(transfer.CodeSessionLost, true, "the service role cannot connect to this cluster now; repeat the operation")
	}
	v.reasons = archive.CheckCompatibility(v.res.Manifest, tgt)
	v.reasons = append(v.reasons, archive.CheckCatalogCompatibility(v.res.Catalog, tgt)...)
	rep := archive.BuildReport(v.res, v.reasons, tgt)
	v.report = &rep
	return nil
}

func (o *Ops) stepVReport(ctx context.Context, env *task.Env) error {
	_, rec, x, err := o.loadValidate(ctx, env)
	if err != nil {
		return err
	}
	v, err := o.validationOf(ctx, x, rec)
	if err != nil {
		return err
	}
	if v.report == nil && v.res != nil {
		tgt, terr := o.target(ctx)
		if terr != nil {
			return transfer.Errorf(transfer.CodeSessionLost, true, "the service role cannot connect to this cluster now; repeat the operation")
		}
		reasons := append(archive.CheckCompatibility(v.res.Manifest, tgt), archive.CheckCatalogCompatibility(v.res.Catalog, tgt)...)
		rep := archive.BuildReport(v.res, reasons, tgt)
		v.report = &rep
	}
	rep := v.report
	if rep == nil {
		// The archive is not valid: the report says why. The reasons are fixed sentences with bounded names, never a part of the content.
		r := archive.ReportFromError(v.err, rec.SHA256)
		r.Valid = false
		rep = &r
	}
	rep.SHA256 = rec.SHA256
	// The manifest says how many rows each entry has; the records of the CSV entries are not counted by a validation.
	if rep.Valid && !hasText(rep.Warnings, claimedRows) {
		rep.Warnings = append(rep.Warnings, claimedRows)
	}
	var up uploadReport
	_ = json.Unmarshal(rec.Report, &up)
	up.Archive = rep
	up.Version = ValidatorVersion
	b, _ := json.Marshal(up)
	state := "invalid"
	if rep.Valid {
		state = "valid"
	}
	if err := o.records().Apply(ctx, rec.ID, transfer.Update{Report: b, ArchiveState: &state}); err != nil {
		return err
	}
	o.setValidation(rec.ID, nil)
	if o.Transfer != nil && o.Transfer.Audit != nil {
		o.Transfer.Audit(ctx, "dataTransfer.completed", map[string]any{"kind": "archiveValidation", "transferId": idText(rec.ID), "sha256": rec.SHA256, "valid": rep.Valid, "reasons": len(rep.Reasons),
			"tables": rep.Tables.Local + rep.Tables.Hash + rep.Tables.Reference, "outcome": "success"})
	}
	return nil
}
