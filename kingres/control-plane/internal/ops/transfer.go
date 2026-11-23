// SPDX-License-Identifier: AGPL-3.0-only

package ops

// The data transfer (contract 0.19.0, design kingres/docs/design/data-export-import.md): the staged files, the uploads and the downloads.
// Everything here runs on the manager leader: the files, the keys and the role connections are the leader's. A file belongs to the role that
// made it, by name and OID; another role sees 404 for it. No error, detail or audit entry of this file carries a value, a row, a key or a
// file name that a client sent.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/api"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/problem"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/relay"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/staging"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/sysdb"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/transfer"
)

// Limits of the upload protocol (design "Uploads").
const (
	// ChunkAlign is the multiple of bytes of every chunk but the last.
	ChunkAlign = staging.SegmentSize
	// ChunkBytesMax is the largest chunk the server accepts; the console sends 64 MiB.
	ChunkBytesMax = int64(256) << 20
	// UploadIdleKeep is how long an upload that received nothing is kept; UploadUnusedKeep how long a complete one that no import used.
	UploadIdleKeep   = 48 * time.Hour
	UploadUnusedKeep = 24 * time.Hour
)

// TransferConfig is what the data transfer needs from the process: where the staged files are, and how the role of a transfer is checked.
type TransferConfig struct {
	// Dir and KeyFile are the staging directory and the file with the master key. Forbidden are the directories the staging directory may
	// not be or hold (the data directory of kingresd).
	Dir, KeyFile string
	Forbidden    []string
	// MaxDuration is the longest a transfer task may run; MaxAtomicBytes the largest decompressed CSV an import takes.
	MaxDuration    time.Duration
	MaxAtomicBytes int64
	// CurrentRoles re-derives the cluster roles of a role (auth.Authenticator.CurrentRoles).
	CurrentRoles func(ctx context.Context, role string) ([]string, error)
	// FS and Reserve are for tests of the disk (nil: the real ones).
	FS      staging.FS
	Reserve func(total uint64) uint64
	// Now is the clock (nil: time.Now).
	Now func() time.Time
	// Audit writes a system event (dataTransfer.completed, dataTransfer.purged); nil: none.
	Audit func(ctx context.Context, action string, details map[string]any)
}

type xferState struct {
	cfg   TransferConfig
	store *staging.Store
	keys  *staging.Keys

	mu  sync.Mutex
	res map[int64]*staging.Reservation
}

func (o *Ops) transferNow() time.Time {
	if o.Transfer != nil && o.Transfer.Now != nil {
		return o.Transfer.Now()
	}
	return time.Now()
}

func (o *Ops) records() transfer.Records {
	return transfer.Records{Run: func(ctx context.Context, f func(tx pgx.Tx) error) error {
		err := o.asService(ctx, sysdb.Database, f)
		if errors.Is(err, transfer.ErrNotFound) {
			return err
		}
		return wrap(err)
	}}
}

// xfer opens the staging directory and the master key at the first use. A directory that is not acceptable, or a key that is lost, is
// answered every time it is asked for, so that the cause is not hidden behind a first failure.
func (o *Ops) xfer(ctx context.Context) (*xferState, error) {
	x, created, err := o.openXfer(ctx)
	if created && err == nil {
		// The janitor runs once before the first transfer is accepted (limit 4 of "Where files live").
		if _, serr := o.TransferSweep(ctx); serr != nil {
			o.Log.Warn("the first sweep of the staging directory failed", "error", serr)
		}
	}
	return x, err
}

func (o *Ops) openXfer(ctx context.Context) (*xferState, bool, error) {
	o.xferMu.Lock()
	defer o.xferMu.Unlock()
	if o.xferSt != nil {
		return o.xferSt, false, nil
	}
	cfg := o.Transfer
	if cfg == nil || cfg.Dir == "" {
		return nil, false, problem.New(problem.UnsupportedOp, "This server is not configured for the data transfer.")
	}
	st, err := o.settings(ctx)
	if err != nil {
		return nil, false, err
	}
	store, err := staging.Open(staging.Config{FS: cfg.FS, Dir: cfg.Dir, Forbidden: cfg.Forbidden, Quota: st.StagingQuotaBytes, Reserve: cfg.Reserve, Now: cfg.Now})
	if err != nil {
		o.Log.Error("the staging directory is not usable", "error", err)
		return nil, false, problem.New(problem.ServiceUnavailable, "The staging directory of the data transfer cannot be used; see the log of the server.").WithRetryAfter(60)
	}
	rs := o.records()
	keys, err := staging.LoadOrCreateKeys(cfg.FS, cfg.KeyFile, cfg.Dir, func() (bool, error) {
		id, err := rs.MaxID(ctx)
		return id > 0, err
	})
	if err != nil {
		o.Log.Error("the master key of the data transfer cannot be used", "error", err)
		if errors.Is(err, staging.ErrKeyLost) {
			// Every ready file is lost with its key; the rows say so.
			_ = o.markLost(ctx)
		}
		return nil, false, problem.New(problem.ServiceUnavailable, "The master key of the data transfer cannot be used ("+transfer.CodeKeyLost+"); see the log of the server.").WithRetryAfter(60)
	}
	o.xferSt = &xferState{cfg: *cfg, store: store, keys: keys, res: map[int64]*staging.Reservation{}}
	// The quota is the sum over the live files: those that exist when this process starts hold their room before the first request is answered.
	// A failed read is not final for the process: the store is not kept, so the next use opens it again and adopts again (DX15-05).
	live, lerr := rs.OnServer(ctx, o.ServerHost)
	if lerr != nil {
		o.xferSt = nil
		o.Log.Error("the staged files cannot be listed, so their reservations are not adopted; the data transfer is unavailable until it can", "error", lerr)
		return nil, false, problem.New(problem.ServiceUnavailable, "The staged files of the data transfer cannot be listed now; see the log of the server.").WithRetryAfter(30)
	}
	for _, r := range live {
		if _, aerr := o.xferSt.reservation(r); aerr != nil {
			o.Log.Error("adopting the reservation of a staged file failed", "transfer", r.ID)
		}
	}
	return o.xferSt, true, nil
}

// markLost sets every ready file of this server to lost (the key that sealed them is gone).
func (o *Ops) markLost(ctx context.Context) error {
	return wrap(o.asService(ctx, sysdb.Database, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE kingres_cp.data_transfer SET state = 'lost' WHERE server_id = $1 AND state IN ('ready', 'receiving', 'building')`, o.ServerHost)
		return err
	}))
}

func (o *Ops) settings(ctx context.Context) (transfer.Settings, error) {
	var st transfer.Settings
	err := o.asService(ctx, sysdb.Database, func(tx pgx.Tx) error {
		var e error
		st, e = transfer.Load(ctx, tx)
		return e
	})
	return st, wrap(err)
}

// identity is the owner of a new file: the signed-in role by name and OID.
func (o *Ops) identity(ctx context.Context, actor string) (transfer.Identity, error) {
	var oid int64
	err := o.asService(ctx, "postgres", func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT oid::bigint FROM pg_catalog.pg_roles WHERE rolname = $1`, actor).Scan(&oid)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return transfer.Identity{}, problem.New(problem.Forbidden, "The role of this session does not exist any more.")
	}
	if err != nil {
		return transfer.Identity{}, wrap(err)
	}
	return transfer.Identity{Name: actor, OID: oid}, nil
}

// RoleChecker is the check of the role between two steps of a transfer task.
func (o *Ops) roleChecker() transfer.RoleChecker {
	return transfer.RoleChecker{
		Current: func(ctx context.Context, name string) ([]string, error) {
			if o.Transfer == nil || o.Transfer.CurrentRoles == nil {
				return nil, errors.New("no role source")
			}
			return o.Transfer.CurrentRoles(ctx, name)
		},
		OID: func(ctx context.Context, name string) (int64, bool, error) {
			var oid int64
			err := o.asService(ctx, "postgres", func(tx pgx.Tx) error {
				return tx.QueryRow(ctx, `SELECT oid::bigint FROM pg_catalog.pg_roles WHERE rolname = $1`, name).Scan(&oid)
			})
			if errors.Is(err, pgx.ErrNoRows) {
				return 0, false, nil
			}
			return oid, err == nil, err
		},
	}
}

// reservation returns the reservation of a file, adopting it from the record after a restart.
func (x *xferState) reservation(rec transfer.Record) (*staging.Reservation, error) {
	x.mu.Lock()
	defer x.mu.Unlock()
	if r, ok := x.res[rec.ID]; ok {
		return r, nil
	}
	used := int64(0)
	if st, size, err := x.store.State(rec.ID); err == nil && st != staging.StateNone {
		used = size
	}
	r, err := x.store.Adopt(rec.ID, rec.Owner.Name, rec.ReservedBytes, used)
	if err != nil {
		return nil, err
	}
	x.res[rec.ID] = r
	return r, nil
}

func (x *xferState) forget(id int64) {
	x.mu.Lock()
	if r, ok := x.res[id]; ok {
		r.Release()
		delete(x.res, id)
	}
	x.mu.Unlock()
}

// overhead is what the sealed form adds to n bytes of plaintext: the header and a tag per segment.
func overhead(n int64) int64 { return n/4096 + 4096 }

var sha256RE = regexp.MustCompile(`^[0-9a-f]{64}$`)

// uploadReport is what the record of an upload keeps in its report.
type uploadReport struct {
	// DeclaredSHA256 is the checksum the client declared, "" when it declared none.
	DeclaredSHA256 string `json:"declaredSha256,omitempty"`
	Purpose        string `json:"purpose,omitempty"`
	// Archive is the report of the last validation of a database archive (contract 0.40.0); Source is the SHA-256 of the file that it read.
	Archive *api.ArchiveReport `json:"archive,omitempty"`
}

func quotaProblem(err error) error {
	var q *staging.QuotaError
	var d *staging.DiskError
	switch {
	case errors.As(err, &q):
		return problem.New(problem.TransferQuotaExceeded, q.Error())
	case errors.As(err, &d):
		return problem.New(problem.InsufficientDisk, d.Error())
	}
	return nil
}

// CreateUpload makes an upload: the record, the reservation of the declared size, and the sealed file with its key, empty.
func (o *Ops) CreateUpload(ctx context.Context, actor string, req api.CreateDataUploadRequest) (api.DataUpload, int64, error) {
	x, err := o.xfer(ctx)
	if err != nil {
		return api.DataUpload{}, 0, err
	}
	st, err := o.settings(ctx)
	if err != nil {
		return api.DataUpload{}, 0, err
	}
	var errs []problem.FieldError
	if req.Purpose != "tableImport" && req.Purpose != "databaseArchive" {
		errs = append(errs, field("/purpose", "must be tableImport or databaseArchive", "unknown_value"))
	}
	if req.SizeBytes < 1 {
		errs = append(errs, field("/sizeBytes", "must be at least 1", "out_of_range"))
	} else if req.SizeBytes > st.MaxFileBytes {
		errs = append(errs, field("/sizeBytes", "is larger than the largest staged file ("+strconv.FormatInt(st.MaxFileBytes, 10)+" bytes)", "out_of_range"))
	}
	if req.SHA256 != "" && !sha256RE.MatchString(req.SHA256) {
		errs = append(errs, field("/sha256", "must be 64 lower-case hexadecimal characters", "pattern"))
	}
	if len(req.FileName) > 255 {
		errs = append(errs, field("/fileName", "must be at most 255 bytes", "max_length"))
	}
	if len(errs) > 0 {
		return api.DataUpload{}, 0, problem.Validation("The upload request is invalid.", errs...)
	}
	who, err := o.identity(ctx, actor)
	if err != nil {
		return api.DataUpload{}, 0, err
	}
	x.store.SetQuota(st.StagingQuotaBytes)
	rep, _ := json.Marshal(uploadReport{DeclaredSHA256: req.SHA256, Purpose: req.Purpose})
	reserve := req.SizeBytes + overhead(req.SizeBytes)
	rs := o.records()
	now := o.transferNow()
	id, err := rs.Create(ctx, transfer.Record{Kind: transfer.KindUpload, State: transfer.StateReceiving, ServerID: o.ServerHost, Owner: who,
		DeclaredBytes: &req.SizeBytes, ReservedBytes: reserve, Format: uploadFormat(req.Purpose), Purpose: req.Purpose, ExpiresAt: now.Add(UploadIdleKeep)})
	if err != nil {
		return api.DataUpload{}, 0, wrap(err)
	}
	fail := func(e error) (api.DataUpload, int64, error) {
		x.forget(id)
		_ = x.store.Remove(id)
		_ = rs.Remove(context.WithoutCancel(ctx), id)
		return api.DataUpload{}, 0, e
	}
	res, err := x.store.Reserve(id, who.Name, reserve)
	if err != nil {
		if p := quotaProblem(err); p != nil {
			return fail(p)
		}
		return fail(wrap(err))
	}
	x.mu.Lock()
	x.res[id] = res
	x.mu.Unlock()
	w, meta, err := x.store.Create(id, res, x.keys)
	if err != nil {
		if p := quotaProblem(err); p != nil {
			return fail(p)
		}
		return fail(wrap(err))
	}
	if _, err := w.Suspend(); err != nil {
		_ = w.Discard()
		return fail(wrap(err))
	}
	if err := rs.Apply(ctx, id, transfer.Update{KeyID: &meta.KeyID, WrappedKey: meta.WrappedKey, Report: rep}); err != nil {
		return fail(wrap(err))
	}
	return api.DataUpload{UploadID: strconv.FormatInt(id, 10), State: "receiving", SizeBytes: req.SizeBytes, Offset: 0, ChunkBytesMax: ChunkBytesMax,
		ChunkAlign: ChunkAlign, ExpiresAt: now.Add(UploadIdleKeep).UTC()}, id, nil
}

func parseID(s string) (int64, error) {
	id, err := strconv.ParseInt(s, 10, 64)
	if err != nil || id < 1 {
		return 0, problem.New(problem.NotFound, "There is no such transfer.")
	}
	return id, nil
}

// owned reads a record of the caller; a record of another role, of another kind or that is deleted is 404.
func (o *Ops) owned(ctx context.Context, actor, idText string, kinds ...string) (transfer.Record, transfer.Identity, error) {
	id, err := parseID(idText)
	if err != nil {
		return transfer.Record{}, transfer.Identity{}, err
	}
	who, err := o.identity(ctx, actor)
	if err != nil {
		return transfer.Record{}, transfer.Identity{}, err
	}
	rec, err := o.records().GetOwned(ctx, id, who)
	if errors.Is(err, transfer.ErrNotFound) {
		return transfer.Record{}, who, problem.New(problem.NotFound, "There is no such transfer.")
	}
	if err != nil {
		return transfer.Record{}, who, wrap(err)
	}
	ok := false
	for _, k := range kinds {
		ok = ok || rec.Kind == k
	}
	if !ok {
		return transfer.Record{}, who, problem.New(problem.NotFound, "There is no such transfer.")
	}
	return rec, who, nil
}

// uploadOffset is the offset of an upload as the file says (never the record): the whole segments of a part file, the size of a complete one.
func (o *Ops) uploadOffset(x *xferState, rec transfer.Record) (int64, error) {
	st, _, err := x.store.State(rec.ID)
	if err != nil {
		return 0, err
	}
	switch st {
	case staging.StatePart:
		return x.store.PartOffset(rec.ID)
	case staging.StateFinal:
		if rec.DeclaredBytes != nil {
			return *rec.DeclaredBytes, nil
		}
	}
	return 0, staging.ErrNotFound
}

// uploadFormat is the format a staged upload has: a CSV file, or the zip archive of a database export.
func uploadFormat(purpose string) string {
	if purpose == "databaseArchive" {
		return "archive/1"
	}
	return "csv"
}

func uploadState(rec transfer.Record) string {
	switch rec.State {
	case transfer.StateReceiving:
		return "receiving"
	case transfer.StateReady:
		switch rec.ArchiveState {
		case "validating", "valid", "invalid":
			return rec.ArchiveState
		}
		return "complete"
	case transfer.StateDeleted:
		return "deleted"
	}
	return "lost"
}

func (o *Ops) uploadView(x *xferState, rec transfer.Record) (api.DataUpload, error) {
	size := int64(0)
	if rec.DeclaredBytes != nil {
		size = *rec.DeclaredBytes
	}
	out := api.DataUpload{UploadID: rec.IDString(), State: uploadState(rec), SizeBytes: size, ChunkBytesMax: ChunkBytesMax, ChunkAlign: ChunkAlign, ExpiresAt: rec.ExpiresAt}
	off, err := o.uploadOffset(x, rec)
	switch {
	case err == nil:
		out.Offset = off
	case errors.Is(err, staging.ErrNotFound):
		return out, err
	default:
		return out, err
	}
	if rec.SHA256 != "" {
		s := rec.SHA256
		out.SHA256 = &s
	}
	if rec.Purpose == "databaseArchive" {
		var rep uploadReport
		if json.Unmarshal(rec.Report, &rep) == nil && rep.Archive != nil && rep.Archive.SHA256 == rec.SHA256 {
			out.Report = rep.Archive
		}
	}
	return out, nil
}

// GetUpload answers getDataUpload. An upload whose file is gone is set to lost and is 404.
func (o *Ops) GetUpload(ctx context.Context, actor, idText string) (api.DataUpload, error) {
	x, err := o.xfer(ctx)
	if err != nil {
		return api.DataUpload{}, err
	}
	rec, _, err := o.owned(ctx, actor, idText, transfer.KindUpload)
	if err != nil {
		return api.DataUpload{}, err
	}
	if rec.State != transfer.StateReceiving && rec.State != transfer.StateReady {
		return api.DataUpload{}, problem.New(problem.NotFound, "There is no such transfer.")
	}
	if rec.ServerID != o.ServerHost {
		// The file is on the server that was the leader. A poll does not decide for good: that server may be the leader again.
		return api.DataUpload{}, problem.New(problem.NotFound, "There is no such transfer.")
	}
	v, err := o.uploadView(x, rec)
	if err != nil {
		o.setLost(ctx, rec.ID)
		x.forget(rec.ID)
		return api.DataUpload{}, problem.New(problem.NotFound, "There is no such transfer.")
	}
	return v, nil
}

func (o *Ops) setLost(ctx context.Context, id int64) {
	lost := transfer.StateLost
	zero := int64(0)
	_ = o.records().Apply(context.WithoutCancel(ctx), id, transfer.Update{State: &lost, ReservedBytes: &zero})
}

// ChunkResult is what the handler of uploadDataChunk reports to the audit: nothing of the data.
type ChunkResult struct {
	Offset   int64
	Complete bool
}

// UploadChunk appends one chunk. length is the Content-Length of the request. The offset must be the one of the file; every chunk but the last
// is a multiple of ChunkAlign; the chunk may not pass the declared size. After the last byte the sealed file is read once to compute its SHA-256.
func (o *Ops) UploadChunk(ctx context.Context, actor, idText string, offset, length int64, body io.Reader) (ChunkResult, error) {
	x, err := o.xfer(ctx)
	if err != nil {
		return ChunkResult{}, err
	}
	rec, who, err := o.owned(ctx, actor, idText, transfer.KindUpload)
	if err != nil {
		return ChunkResult{}, err
	}
	if rec.State != transfer.StateReceiving || rec.ServerID != o.ServerHost || rec.DeclaredBytes == nil {
		return ChunkResult{}, problem.New(problem.NotFound, "There is no such transfer.")
	}
	declared := *rec.DeclaredBytes
	if err := o.roleChecker().Check(ctx, who); err != nil {
		return ChunkResult{}, transfer.SafeError(err).Problem()
	}
	if length < 0 {
		return ChunkResult{}, problem.Validation("The chunk needs a Content-Length.", field("Content-Length", "is required", "required"))
	}
	have, err := x.store.PartOffset(rec.ID)
	if err != nil {
		o.setLost(ctx, rec.ID)
		x.forget(rec.ID)
		return ChunkResult{}, problem.New(problem.NotFound, "There is no such transfer.")
	}
	if offset != have {
		return ChunkResult{}, problem.New(problem.UploadOffsetMismatch, "The upload is at offset "+strconv.FormatInt(have, 10)+".").
			WithHeader("Upload-Offset", strconv.FormatInt(have, 10))
	}
	if length > ChunkBytesMax || offset+length > declared {
		return ChunkResult{}, problem.New(problem.ChunkTooLarge, "The chunk is larger than the server accepts or reaches past the declared size.")
	}
	last := offset+length == declared
	if length == 0 || (!last && length%ChunkAlign != 0) {
		return ChunkResult{}, problem.Validation("The chunk has a wrong length.", field("Content-Length", "must be a multiple of 65536 unless it is the last chunk, and not empty", "pattern"))
	}
	res, err := x.reservation(rec)
	if err != nil {
		return ChunkResult{}, wrap(err)
	}
	w, err := x.store.OpenAppend(rec.ID, res, x.keys, staging.FileMeta{KeyID: rec.KeyID, WrappedKey: rec.WrappedKey})
	if errors.Is(err, staging.ErrBusy) {
		return ChunkResult{}, problem.New(problem.UploadOffsetMismatch, "Another request is writing this upload.").WithHeader("Upload-Offset", strconv.FormatInt(have, 10))
	}
	if err != nil {
		if p := quotaProblem(err); p != nil {
			return ChunkResult{}, p
		}
		return ChunkResult{}, wrap(err)
	}
	n, cerr := relay.Copy(w, io.LimitReader(body, length))
	if cerr != nil || n != length {
		kept := w.Abort()
		_ = o.records().Apply(context.WithoutCancel(ctx), rec.ID, transfer.Update{Touch: n > 0})
		if cerr == nil {
			cerr = io.ErrUnexpectedEOF
		}
		if p := quotaProblem(cerr); p != nil {
			return ChunkResult{Offset: kept}, p
		}
		return ChunkResult{Offset: kept}, problem.New(problem.BadRequest, "The chunk ended before its Content-Length; the upload is at offset "+strconv.FormatInt(kept, 10)+".").
			WithHeader("Upload-Offset", strconv.FormatInt(kept, 10))
	}
	if !last {
		off, serr := w.Suspend()
		if serr != nil {
			return ChunkResult{Offset: off}, wrap(serr)
		}
		_ = o.records().Apply(context.WithoutCancel(ctx), rec.ID, transfer.Update{Touch: true})
		return ChunkResult{Offset: off}, nil
	}
	sealed, ferr := w.Finish()
	if ferr != nil {
		return ChunkResult{}, wrap(ferr)
	}
	sum, serr := o.sumOf(x, rec)
	if serr != nil {
		return ChunkResult{}, wrap(serr)
	}
	var rep uploadReport
	_ = json.Unmarshal(rec.Report, &rep)
	if rep.DeclaredSHA256 != "" && rep.DeclaredSHA256 != sum {
		o.removeRecord(ctx, x, rec)
		return ChunkResult{}, problem.New(problem.UploadChecksumMismatch, "The SHA-256 of the received file is not the declared one; the upload was deleted.")
	}
	ready, size := transfer.StateReady, sealed.Plaintext
	exp := o.transferNow().Add(UploadUnusedKeep)
	res.Settle()
	if err := o.records().Apply(ctx, rec.ID, transfer.Update{State: &ready, SizeBytes: &size, SHA256: &sum, ExpiresAt: &exp, Touch: true}); err != nil {
		return ChunkResult{}, wrap(err)
	}
	return ChunkResult{Offset: declared, Complete: true}, nil
}

// sumOf reads the sealed file of a record once and returns the SHA-256 of its plaintext.
func (o *Ops) sumOf(x *xferState, rec transfer.Record) (string, error) {
	dk, err := x.keys.Unwrap(rec.ID, rec.KeyID, rec.WrappedKey)
	if err != nil {
		return "", err
	}
	r, err := x.store.OpenReader(rec.ID, dk)
	if err != nil {
		return "", err
	}
	defer func() { _ = r.Close() }()
	h := sha256.New()
	if _, err := relay.Copy(h, r); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// removeRecord deletes the file, frees the reservation and marks the record deleted.
func (o *Ops) removeRecord(ctx context.Context, x *xferState, rec transfer.Record) {
	_ = x.store.Remove(rec.ID)
	x.forget(rec.ID)
	del, zero := transfer.StateDeleted, int64(0)
	_ = o.records().Apply(context.WithoutCancel(ctx), rec.ID, transfer.Update{State: &del, ReservedBytes: &zero})
}

// taskActive tells whether the task a record names is unfinished.
func (o *Ops) taskActive(ctx context.Context, rec transfer.Record) bool {
	eng := o.Engine()
	if eng == nil || rec.TaskID == nil {
		return false
	}
	t, err := eng.Get(ctx, *rec.TaskID)
	return err == nil && t.State.Active()
}

// DeleteUpload answers deleteDataUpload: 404 for another role's, 409 while a task uses it.
func (o *Ops) DeleteUpload(ctx context.Context, actor, idText string) (int64, error) {
	x, err := o.xfer(ctx)
	if err != nil {
		return 0, err
	}
	rec, _, err := o.owned(ctx, actor, idText, transfer.KindUpload)
	if err != nil {
		return 0, err
	}
	if o.taskActive(ctx, rec) {
		return 0, problem.New(problem.OperationInProgress, "A task is using this upload.")
	}
	o.removeRecord(ctx, x, rec)
	return rec.ID, nil
}

// exportReport is what the record of an export keeps in its report: facts about the file, never values.
type exportReport struct {
	Rows int64 `json:"rows"`
	// Warnings are the warnings of the manifest of a database archive.
	Warnings    []string               `json:"warnings,omitempty"`
	Encoding    string                 `json:"encoding"`
	Compression string                 `json:"compression,omitempty"`
	Consistency []api.TableConsistency `json:"consistency"`
	RLSApplied  bool                   `json:"rlsApplied"`
	Safe        bool                   `json:"spreadsheetSafe"`
}

func exportView(rec transfer.Record) api.DataExport {
	out := api.DataExport{ExportID: rec.IDString(), Kind: rec.Kind, State: rec.State, Format: rec.Format, CreatedAt: rec.CreatedAt, ExpiresAt: rec.ExpiresAt, DownloadedAt: rec.DownloadedAt}
	if rec.TaskID != nil {
		out.TaskID = *rec.TaskID
	}
	str := func(s string) *string {
		if s == "" {
			return nil
		}
		return &s
	}
	out.Database, out.Schema, out.Table = str(rec.Database), str(rec.Schema), str(rec.Table)
	if rec.State == transfer.StateReady || rec.State == transfer.StateExpired {
		out.SizeBytes = rec.SizeBytes
		out.SHA256 = str(rec.SHA256)
		var rep exportReport
		if json.Unmarshal(rec.Report, &rep) == nil {
			r := rep.Rows
			out.Rows = &r
			out.Consistency = rep.Consistency
			out.RLSApplied = rep.RLSApplied
			out.Warnings = rep.Warnings
			if rec.Kind == transfer.KindTableExport {
				safe := rep.Safe
				out.SpreadsheetSafe = &safe
			}
		}
	}
	return out
}

// ListExports answers listDataExports: the files of the caller, newest first, paged by id.
func (o *Ops) ListExports(ctx context.Context, actor string, limit int, cursor string) (api.DataExportPage, error) {
	if _, err := o.xfer(ctx); err != nil {
		return api.DataExportPage{}, err
	}
	who, err := o.identity(ctx, actor)
	if err != nil {
		return api.DataExportPage{}, err
	}
	var before int64
	if cursor != "" {
		b, perr := strconv.ParseInt(cursor, 10, 64)
		if perr != nil || b < 1 {
			return api.DataExportPage{}, problem.Validation("The cursor is not valid.", field("cursor", "is not a cursor of this list", "pattern"))
		}
		before = b
	}
	recs, err := o.records().List(ctx, who, []string{transfer.KindTableExport, transfer.KindDatabaseExport}, before, limit+1)
	if err != nil {
		return api.DataExportPage{}, wrap(err)
	}
	page := api.DataExportPage{Items: []api.DataExport{}}
	for i, r := range recs {
		if i == limit {
			next := strconv.FormatInt(recs[limit-1].ID, 10)
			page.NextCursor = &next
			break
		}
		page.Items = append(page.Items, exportView(r))
	}
	return page, nil
}

// GetExport answers getDataExport.
func (o *Ops) GetExport(ctx context.Context, actor, idText string) (api.DataExport, error) {
	if _, err := o.xfer(ctx); err != nil {
		return api.DataExport{}, err
	}
	rec, _, err := o.owned(ctx, actor, idText, transfer.KindTableExport, transfer.KindDatabaseExport)
	if err != nil {
		return api.DataExport{}, err
	}
	return exportView(rec), nil
}

// DeleteExport answers deleteDataExport: the creator or any administrator (the operation needs kingres_admin). It returns the creator, so
// that the audit event names it when the actor is another role.
func (o *Ops) DeleteExport(ctx context.Context, actor, idText string) (creator string, err error) {
	x, err := o.xfer(ctx)
	if err != nil {
		return "", err
	}
	id, err := parseID(idText)
	if err != nil {
		return "", err
	}
	rec, gerr := o.records().Get(ctx, id)
	if errors.Is(gerr, transfer.ErrNotFound) || (gerr == nil && (rec.State == transfer.StateDeleted || (rec.Kind != transfer.KindTableExport && rec.Kind != transfer.KindDatabaseExport))) {
		return "", problem.New(problem.NotFound, "There is no such transfer.")
	}
	if gerr != nil {
		return "", wrap(gerr)
	}
	if rec.State == transfer.StateBuilding && o.taskActive(ctx, rec) {
		return "", problem.New(problem.OperationInProgress, "The export is being written; cancel its task first.")
	}
	o.removeRecord(ctx, x, rec)
	if rec.Owner.Name != actor {
		return rec.Owner.Name, nil
	}
	return "", nil
}

// Download is an export that is opened for download.
type Download struct {
	Rec    transfer.Record
	Reader *staging.Reader
	// Encoding is the encoding of the CSV, for the Content-Type.
	Encoding string
}

// OpenDownload finds the file of the caller and opens it. 404 for another role's file or one that is not ready; 410 for one whose time has passed.
func (o *Ops) OpenDownload(ctx context.Context, actor, idText string) (*Download, error) {
	x, err := o.xfer(ctx)
	if err != nil {
		return nil, err
	}
	rec, who, err := o.owned(ctx, actor, idText, transfer.KindTableExport, transfer.KindDatabaseExport)
	if err != nil {
		return nil, err
	}
	if rec.State == transfer.StateExpired || (rec.State == transfer.StateReady && !rec.ExpiresAt.After(o.transferNow())) {
		p := problem.New(problem.NotFound, "The export expired at "+rec.ExpiresAt.Format(time.RFC3339)+".")
		p.Status, p.Title = http.StatusGone, "Expired"
		return nil, p
	}
	if rec.State != transfer.StateReady || rec.ServerID != o.ServerHost {
		return nil, problem.New(problem.NotFound, "There is no such file to download.")
	}
	if err := o.roleChecker().Check(ctx, who); err != nil {
		return nil, transfer.SafeError(err).Problem()
	}
	dk, err := x.keys.Unwrap(rec.ID, rec.KeyID, rec.WrappedKey)
	if err != nil {
		o.setLost(ctx, rec.ID)
		return nil, problem.New(problem.NotFound, "There is no such file to download.")
	}
	r, err := x.store.OpenReader(rec.ID, dk)
	if err != nil {
		o.setLost(ctx, rec.ID)
		return nil, problem.New(problem.NotFound, "There is no such file to download.")
	}
	var rep exportReport
	_ = json.Unmarshal(rec.Report, &rep)
	return &Download{Rec: rec, Reader: r, Encoding: rep.Encoding}, nil
}

// FinishDownload records that the last byte of the file was served.
func (o *Ops) FinishDownload(ctx context.Context, id int64) {
	_ = o.records().Apply(context.WithoutCancel(ctx), id, transfer.Update{Downloaded: true})
}

// String of an id for audit details.
func idText(id int64) string { return fmt.Sprint(id) }

// stillEnabled is the check of the setting that the first step of a task makes: turning dataTransfer.enabled off ends a task that has not begun to move data.
// A task that is moving data is not stopped by it (cancel it).
func (o *Ops) stillEnabled(ctx context.Context) error {
	st, err := o.settings(ctx)
	if err != nil {
		return err
	}
	if !st.Enabled {
		return transfer.Errorf(transfer.CodeForbidden, false, "the data transfer was turned off in the cluster settings")
	}
	return nil
}

// ReopenTransfer forgets the staging store and the reservations of this process, as a restart does; the next use opens it again and adopts the files. For tests.
func (o *Ops) ReopenTransfer() {
	o.xferMu.Lock()
	o.xferSt = nil
	o.xferMu.Unlock()
}
