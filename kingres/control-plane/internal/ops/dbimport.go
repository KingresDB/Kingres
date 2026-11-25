// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package ops

// The task database.import (contract 0.30.0, design data-export-import.md, "The task database.import" and "What may run"): an archive of a database is loaded
// into a NEW database. The archive is hostile input. The rules that hold for every step of this file and of dbimport_*.go:
//
//   - The archive is read again from its bytes (the whole validator, the shape grammar, the SHA-256 of the file); the state of the upload and a cached report are
//     never trusted (they may only make a request fail early).
//   - The work is done in a database whose name only this task uses (kingres_import_<task id>), created by the service role with a name that must not exist, and
//     renamed to the final name as the last act. A failed or cancelled task drops that name and nothing else.
//   - Statements of the archive run on a connection of the signed-in role, one per round trip through the extended protocol, after the shape grammar consumed
//     all of the text of the statement, with search_path = pg_catalog. They never run as the service role. PostgreSQL's privileges are the guarantee; the grammar
//     narrows what is attempted.
//   - The service role runs only calls that the control plane writes itself from a structured document (the catalog), with identifiers that passed sqlx, and
//     it does so while the importing role cannot connect to the private database (the CONNECT privilege is withdrawn and its sessions are ended), so that the
//     role cannot change an object between a check and the call that uses it. Before a call into the schema kingres the extension's schema is verified (extguard).
//   - Nothing that the archive contains reaches a task, a problem, the audit log or the log: a refusal names the kind, the position and the fingerprint of a
//     statement, an error of PostgreSQL goes through transfer.SafeError.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/jackc/pgx/v5"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/api"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/archive"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/importdb"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/problem"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/sqlshape"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/task"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/transfer"
)

// KindDatabaseImport is the task kind of the import of a database archive.
const KindDatabaseImport = "database.import"

// Step names of database.import (the contract lists them).
const (
	stepMCheck      = "Check the archive"
	stepMCreate     = "Create the private database"
	stepMInstall    = "Install the extensions"
	stepMFunctions  = "Check the functions"
	stepMTables     = "Create the tables"
	stepMDistribute = "Distribute the tables"
	stepMViews      = "Create the views"
	stepMData       = "Load the data"
	stepMPost       = "Create the indexes and constraints"
	stepMSeq        = "Set the sequences"
	stepMLedger     = "Enable the ledger and the policies"
	stepMVerify     = "Verify"
	stepMRename     = "Take the final name"
)

// privatePrefix is the start of the name of the database that a task works in; the rest is the id of the task.
const privatePrefix = importdb.Prefix

// privateName is the name of the database that task id works in, and the only database that its failure hook may drop. The nonce is random and made at the request:
// a role with CREATEDB cannot make a database under the name of a task that has not started.
func privateName(taskID int64, nonce string) string {
	return privatePrefix + strconv.FormatInt(taskID, 10) + "_" + nonce
}

// private is the private name of the task with this id.
func (p dbImportParams) private(taskID int64) string { return privateName(taskID, p.Nonce) }

// The limits of the plan of an import (design "Limits and timeouts"): the sizes of the statements are the grammar's (200 000 statements of at most 1 MiB).
const (
	// maxImportReasons bounds the refusals and reasons that a request or a task names.
	maxImportReasons = 20
)

type dbImportParams struct {
	UploadID       int64             `json:"uploadId"`
	Owner          transfer.Identity `json:"owner"`
	Database       string            `json:"database"`
	ExpectedSHA256 string            `json:"expectedSha256,omitempty"`
	AckLedger      bool              `json:"acknowledgeLedgerRestart"`
	ForeignKeys    string            `json:"onForeignKeyViolation"`
	KeepOnFailure  bool              `json:"keepOnFailure"`
	// Servers are the other servers of the cluster at the time of the request: the private database is made on each of them.
	Servers []string `json:"servers,omitempty"`
	// Origin is the server that accepted the request (the leader then). After a change of leader it is not among Servers; it is named so that the steps after the
	// commit point finish the database there too.
	Origin string `json:"origin,omitempty"`
	// Nonce is the random part of the private name and of its marker.
	Nonce string `json:"nonce"`
}

func (p dbImportParams) keepNotValid() bool { return p.ForeignKeys == "keepNotValid" }

// importPlan is what the three SQL files hold, as the shape grammar found it. It carries names and counts, never the text of a statement.
type importPlan struct {
	Functions  []string // every function that an expression calls, sorted
	Tables     map[string]bool
	MatViews   []string // "schema.name" of the materialized views, in file order
	Schemas    []string // the schemas that CREATE SCHEMA creates
	Statements int
}

// importState is what the steps of one import share inside this process. A step that does not find it makes it again from the archive.
type importState struct {
	res  *archive.Result
	plan *importPlan
	// notes are the lines that the last detail of the task lists: the ledger chains that started again, the policies that are disabled, the constraints that are NOT VALID.
	notes []string
}

type imports struct {
	mu sync.Mutex
	m  map[int64]*importState
}

func (o *Ops) importState(id int64) *importState {
	o.imps.mu.Lock()
	defer o.imps.mu.Unlock()
	return o.imps.m[id]
}

func (o *Ops) setImportState(id int64, s *importState) {
	o.imps.mu.Lock()
	defer o.imps.mu.Unlock()
	if o.imps.m == nil {
		o.imps.m = map[int64]*importState{}
	}
	if s == nil {
		delete(o.imps.m, id)
		return
	}
	o.imps.m[id] = s
}

// MaxImportBytes is the largest sum of the sizes of the entries of an archive that an import takes.
func (o *Ops) maxImport() int64 {
	if o.Transfer != nil && o.Transfer.MaxImportBytes > 0 {
		return o.Transfer.MaxImportBytes
	}
	return int64(100) << 30
}

// importLimits are the limits of a validation of an archive of this file size.
func (o *Ops) importLimits(fileSize int64) archive.Limits {
	max := o.maxImport()
	if m := fileSize * MaxExpansion; m < max {
		max = m
	}
	return archive.Limits{MaxImportBytes: max, MaxRatio: MaxEntryRatio}
}

// ---------------------------------------------------------------- the request

// closed set of codes that an archive may be incompatible for (archive.CheckCompatibility); the other reasons of an invalid report are about the file.
var incompatibleCodes = map[string]bool{"format": true, "catalog_version": true, "requires": true, "postgres_major": true, "extension_missing": true, "extension_version": true,
	"encoding": true, "lc_collate": true, "lc_ctype": true, "locale_provider": true, "distributed_schemas": true}

// refusalFromReport turns a cached report that says "not valid" into the refusal of the request; the cache may only make a request fail early, never let one pass.
func refusalFromReport(rep *api.ArchiveReport) error {
	if rep == nil || rep.Valid {
		return nil
	}
	if rep.Statements != nil && len(rep.Statements.Refused) > 0 {
		var rs []api.TableBlocker
		for i, r := range rep.Statements.Refused {
			if i >= maxImportReasons {
				break
			}
			rs = append(rs, api.TableBlocker{Code: "statement_refused", Object: r.Kind + " at " + r.Position, Message: "The statement is not allowed (" + r.Reason + "); its fingerprint is " + r.Fingerprint + "."})
		}
		return problem.New(problem.ArchiveStatementRefused, fmt.Sprintf("The shape grammar refuses %d statement(s) of the archive; nothing was run. Code in a schema is not imported: such a database is a job for pg_dump.", len(rep.Statements.Refused))).WithReasons(rs)
	}
	code := problem.ArchiveInvalid
	for _, r := range rep.Reasons {
		if incompatibleCodes[r.Code] {
			code = problem.ArchiveIncompatible
		}
	}
	var rs []api.TableBlocker
	for i, r := range rep.Reasons {
		if i >= maxImportReasons {
			break
		}
		rs = append(rs, r)
	}
	text := "The archive is not valid."
	if code == problem.ArchiveIncompatible {
		text = "The archive does not fit this cluster."
	}
	return problem.New(code, text).WithReasons(rs)
}

// usableImportName says whether a name may be the final name of an imported database: a valid name that is not a system database and can never be the name
// that a task works in (a request cannot name the private database of a task, so a task cannot be made to work in, or to drop, another task's database).
func usableImportName(n string) bool {
	return dbNameRE.MatchString(n) && !protectedDB[n] && !strings.HasPrefix(n, privatePrefix)
}

// StartDatabaseImport answers importDatabase. It checks what is cheap and certain at the request; the archive itself is read again by the first step of the task
// (an archive of 100 GiB cannot be read inside a request), and a cached report is used only to refuse early.
func (o *Ops) StartDatabaseImport(ctx context.Context, actor string, req api.ImportDatabaseRequest) (task.Task, error) {
	x, err := o.xfer(ctx)
	if err != nil {
		return task.Task{}, err
	}
	var errs []problem.FieldError
	fk := req.OnForeignKeyViolation
	switch fk {
	case "":
		fk = "fail"
	case "fail", "keepNotValid":
	default:
		errs = append(errs, field("/onForeignKeyViolation", "must be fail or keepNotValid", "unknown_value"))
	}
	if req.Database != "" && !usableImportName(req.Database) {
		errs = append(errs, field("/database", "must match ^[a-z_][a-z0-9_]{0,62}$, not be a system database and not start with "+privatePrefix, "pattern"))
	}
	if req.ExpectedSHA256 != "" && !sha256RE.MatchString(req.ExpectedSHA256) {
		errs = append(errs, field("/expectedSha256", "must be 64 lower-case hexadecimal digits", "pattern"))
	}
	if len(errs) > 0 {
		return task.Task{}, problem.Validation("The import request is invalid.", errs...)
	}
	st, err := o.settings(ctx)
	if err != nil {
		return task.Task{}, err
	}
	who, err := o.identity(ctx, actor)
	if err != nil {
		return task.Task{}, err
	}
	if err := o.roleChecker().Check(ctx, who); err != nil {
		return task.Task{}, transfer.SafeError(err).Problem()
	}
	up, _, err := o.owned(ctx, actor, req.UploadID, transfer.KindUpload)
	if err != nil {
		return task.Task{}, err
	}
	if up.Purpose != "databaseArchive" {
		return task.Task{}, problem.Validation("The upload is not a database archive.", field("/uploadId", "the purpose of the upload is not databaseArchive", "conflict"))
	}
	// A repeat of the request of an import that is committed and paused goes on with it.
	if t, ok, err := o.resumeCommittedImport(ctx, up, req.Database); ok || err != nil {
		return t, err
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
	if req.ExpectedSHA256 != "" && up.SHA256 != "" && req.ExpectedSHA256 != up.SHA256 {
		return task.Task{}, problem.New(problem.ArchiveInvalid, "The archive is not the file that was expected: its SHA-256 differs from expectedSha256.")
	}
	// A report that was made for these bytes by this validator can only refuse early.
	if rep := cachedReport(up); rep != nil && rep.SHA256 == up.SHA256 {
		if err := refusalFromReport(rep); err != nil {
			return task.Task{}, err
		}
		if rep.Needs != nil && rep.Needs.LedgerRestart && !req.AcknowledgeLedgerRestart {
			return task.Task{}, problem.New(problem.LedgerRestartNotAcknowledged, "The archive has ledger tables. An import enters their rows as the first entries of a new chain; set acknowledgeLedgerRestart to accept it.")
		}
	}
	// The name: the request's, or the one in the manifest.
	name := req.Database
	if name == "" {
		m, merr := o.manifestOf(ctx, x, up)
		if merr != nil {
			return task.Task{}, merr
		}
		name = m.Source.Database
		if !usableImportName(name) {
			return task.Task{}, problem.Validation("The import request is invalid.", field("/database", "the name in the archive is not usable here; give a name", "pattern"))
		}
	}
	if o.Service == nil {
		return task.Task{}, problem.New(problem.UnsupportedOp, "This server has no managed instance.")
	}
	var exists, bad bool
	var why string
	if err := o.asService(ctx, "postgres", func(tx pgx.Tx) error {
		var e error
		if exists, e = dbExists(ctx, tx, name); e != nil {
			return e
		}
		if e = tx.QueryRow(ctx, dangerousRolesSQL, DangerousPredefinedRoles, actor).Scan(&bad); e != nil {
			return e
		}
		why, e = importerRefusal(ctx, tx, actor)
		return e
	}); err != nil {
		return task.Task{}, wrap(err)
	}
	if why != "" {
		return task.Task{}, problem.New(problem.Forbidden, importerText)
	}
	// The free space of every server against the size of the archive once it is expanded: refused before anything is made.
	if m, merr := o.manifestOf(ctx, x, up); merr == nil {
		if err := o.freeSpaceFor(ctx, append([]string{o.ServerHost}, ids(o.others(ctx))...), m.Totals.UncompressedBytes); err != nil {
			return task.Task{}, transfer.SafeError(err).Problem()
		}
	}
	nonce, nerr := newNonce()
	if nerr != nil {
		return task.Task{}, problem.New(problem.InternalError, "A random value could not be made.")
	}
	if exists {
		return task.Task{}, problem.New(problem.AlreadyExists, fmt.Sprintf("Database %q already exists.", name))
	}
	if bad {
		return task.Task{}, problem.New(problem.Forbidden, importDangerousText)
	}
	o.startMu.Lock()
	defer o.startMu.Unlock()
	if err := o.concurrency(ctx, st, actor); err != nil {
		return task.Task{}, err
	}
	p := dbImportParams{UploadID: up.ID, Owner: who, Database: name, ExpectedSHA256: req.ExpectedSHA256, AckLedger: req.AcknowledgeLedgerRestart, ForeignKeys: fk,
		KeepOnFailure: req.KeepOnFailure, Servers: ids(o.others(ctx)), Origin: o.ServerHost, Nonce: nonce}
	t, err := o.StartTask(ctx, KindDatabaseImport, "database", name, p, actor)
	if err != nil {
		return task.Task{}, err
	}
	tid := t.ID
	_ = o.records().Apply(ctx, up.ID, transfer.Update{TaskID: &tid})
	return t, nil
}

const importDangerousText = "An import runs the statements of the archive as the signed-in role, so it is refused for a role with the SUPERUSER, REPLICATION or BYPASSRLS attribute or with access to server files, programs or logs; sign in as another role."

// manifestOf reads the manifest of the archive of a record (the structure and the manifest, nothing else).
func (o *Ops) manifestOf(ctx context.Context, x *xferState, rec transfer.Record) (archive.Manifest, error) {
	dk, err := x.keys.Unwrap(rec.ID, rec.KeyID, rec.WrappedKey)
	if err != nil {
		return archive.Manifest{}, transfer.Errorf(transfer.CodeKeyLost, false, "the key of the file is gone").Problem()
	}
	r, err := x.store.OpenReader(rec.ID, dk)
	if err != nil {
		return archive.Manifest{}, problem.New(problem.NotFound, "There is no such transfer.")
	}
	defer func() { _ = r.Close() }()
	m, err := archive.ReadManifest(r, r.Size(), o.importLimits(r.Size()))
	if err != nil {
		return archive.Manifest{}, problem.New(problem.ArchiveInvalid, "The archive is not valid.")
	}
	return m, nil
}

// ---------------------------------------------------------------- the definition

func (o *Ops) dbImportDefinition() task.Definition {
	return task.Definition{
		Kind: KindDatabaseImport,
		Dynamic: func(in json.RawMessage) ([]task.Step, error) {
			p, err := rawInput[dbImportParams](in)
			if err != nil {
				return nil, err
			}
			steps := []task.Step{
				{Name: stepMCheck, Run: importStep(stepMCheck, o.stepIMCheck)},
				{Name: stepMCreate, Run: importStep(stepMCreate, o.stepIMCreate)},
			}
			steps = append(steps, o.perServer(p.Servers, "Create the private database on server %s", func(ctx context.Context, env *task.Env, s string) error {
				if h := ImportHook; h != nil {
					if err := h(ctx, "Create the private database on server "+s, ""); err != nil {
						return err
					}
				}
				return o.stepIMCreateOn(ctx, env, s)
			})...)
			return append(steps,
				task.Step{Name: stepMInstall, Run: importStep(stepMInstall, o.stepIMInstall)},
				task.Step{Name: stepMFunctions, Run: importStep(stepMFunctions, o.stepIMFunctions)},
				task.Step{Name: stepMTables, Interruptible: true, Run: importStep(stepMTables, o.stepIMTables)},
				task.Step{Name: stepMDistribute, Run: importStep(stepMDistribute, o.stepIMDistribute)},
				task.Step{Name: stepMViews, Interruptible: true, Run: importStep(stepMViews, o.stepIMViews)},
				task.Step{Name: stepMData, Interruptible: true, Run: importStep(stepMData, o.stepIMData)},
				task.Step{Name: stepMPost, Interruptible: true, Run: importStep(stepMPost, o.stepIMPost)},
				task.Step{Name: stepMSeq, Run: importStep(stepMSeq, o.stepIMSequences)},
				task.Step{Name: stepMLedger, Run: importStep(stepMLedger, o.stepIMLedger)},
				task.Step{Name: stepMVerify, Run: importStep(stepMVerify, o.stepIMVerify)},
				task.Step{Name: stepMRename, Run: importStep(stepMRename, o.stepIMRename)},
			), nil
		},
		OnFail:    o.importOnFailHook,
		Committed: importCommittedNote,
		// A cancel is accepted until the sequences are set (design: until step 11 starts); after it the task rolls forward only. A task that passed the commit point
		// of the last step accepts a cancel only while it runs, and the cancel then pauses it (nothing is undone, see Committed); a paused one is resumed, not cancelled.
		Cancellable: func(t task.Task) bool {
			if strings.HasPrefix(t.Mark, importCommitPrefix) {
				return t.State == task.Running
			}
			for _, n := range []string{stepMSeq, stepMLedger, stepMVerify, stepMRename} {
				switch stepState(t, n) {
				case task.StepRunning, task.StepDone, task.StepSkipped:
					return false
				}
			}
			return true
		},
	}
}

// ImportHook is called by a test at the start of each step of database.import (the name of the step and the private name of the task, empty for a step that runs
// on one server); it may block, and an error ends the step.
var ImportHook func(ctx context.Context, step, private string) error

func importStep(name string, run func(ctx context.Context, env *task.Env) error) func(ctx context.Context, env *task.Env) error {
	return xferStep(func(ctx context.Context, env *task.Env) error {
		if h := ImportHook; h != nil {
			private := ""
			if p, err := inputOf[dbImportParams](env.Task); err == nil {
				private = p.private(env.Task.ID)
			}
			if err := h(ctx, name, private); err != nil {
				return err
			}
		}
		return run(ctx, env)
	})
}

// loadImport finds the record of the upload of the task and checks that it is still the caller's, on this server and complete, that the role may still
// do this, and that the data transfer is on.
func (o *Ops) loadImport(ctx context.Context, env *task.Env) (dbImportParams, transfer.Record, *xferState, error) {
	p, err := inputOf[dbImportParams](env.Task)
	if err != nil {
		return p, transfer.Record{}, nil, err
	}
	x, err := o.xfer(ctx)
	if err != nil {
		return p, transfer.Record{}, nil, transfer.Errorf(transfer.CodeLeaderChanged, true, "the staging store of this server is not available; repeat the operation")
	}
	rec, err := o.records().Get(ctx, p.UploadID)
	if err != nil || rec.Kind != transfer.KindUpload || rec.Owner != p.Owner {
		return p, rec, x, transfer.Errorf(transfer.CodeLeaderChanged, true, "the upload is not known to this server; repeat the operation")
	}
	if rec.ServerID != o.ServerHost || rec.State != transfer.StateReady {
		return p, rec, x, transfer.Errorf(transfer.CodeLeaderChanged, true, "the leader changed or the upload is gone; send the file again and repeat the operation")
	}
	if err := o.roleChecker().Check(ctx, rec.Owner); err != nil {
		return p, rec, x, err
	}
	return p, rec, x, nil
}

// refuseDangerousImporter asks the catalog (as the service role: no database of the role is needed yet) whether the role became one that an import refuses.
func (o *Ops) refuseDangerousImporter(ctx context.Context, role string) error {
	var bad bool
	if err := o.asService(ctx, "postgres", func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, dangerousRolesSQL, DangerousPredefinedRoles, role).Scan(&bad)
	}); err != nil {
		return transfer.Errorf(transfer.CodeSessionLost, true, "the check of the role could not be made now; repeat the operation")
	}
	if bad {
		return transfer.Errorf(transfer.CodeForbidden, false, "the role has the SUPERUSER, REPLICATION or BYPASSRLS attribute or access to server files, programs or logs; the statements of an archive run as that role")
	}
	var why string
	if err := o.asService(ctx, "postgres", func(tx pgx.Tx) error {
		var e error
		why, e = importerRefusal(ctx, tx, role)
		return e
	}); err != nil {
		return transfer.Errorf(transfer.CodeSessionLost, true, "the check of the role could not be made now; repeat the operation")
	}
	if why != "" {
		return transfer.Errorf(transfer.CodeForbidden, false, "%s", importerText)
	}
	return nil
}

// freeSpaceFor refuses an import when a server cannot hold the archive once it is expanded (the whole of it may land on one server, and the indexes and the work
// need room besides). A server that cannot tell is not checked.
func (o *Ops) freeSpaceFor(ctx context.Context, servers []string, bytes int64) error {
	if o.DiskFree == nil || bytes <= 0 {
		return nil
	}
	need := bytes + bytes/2 + 64<<20
	for _, s := range servers {
		if free, known := o.DiskFree(ctx, s); known && free < need {
			return transfer.Errorf(transfer.CodeInsufficientDisk, false, "server %s has %d bytes free and the archive needs about %d once it is loaded", safeName(s), free, need)
		}
	}
	return nil
}

// archiveFile is the open archive of a transfer record.
type archiveFile struct {
	r interface {
		io.ReaderAt
		Size() int64
		Close() error
	}
	rec transfer.Record
	st  *importState
}

func (a *archiveFile) close() { _ = a.r.Close() }

// openImport opens the archive of the record and finds what the first step made of it. A process that did not run the first step (a restart, a change of the
// leader) validates again from the bytes: the state is never taken from anywhere else.
func (o *Ops) openImport(ctx context.Context, x *xferState, rec transfer.Record, p dbImportParams, taskID int64) (*archiveFile, error) {
	dk, err := x.keys.Unwrap(rec.ID, rec.KeyID, rec.WrappedKey)
	if err != nil {
		return nil, transfer.Errorf(transfer.CodeKeyLost, false, "the key of the file is gone")
	}
	r, err := x.store.OpenReader(rec.ID, dk)
	if err != nil {
		return nil, transfer.Errorf(transfer.CodeLeaderChanged, true, "the file of the upload is not there; repeat the operation")
	}
	a := &archiveFile{r: r, rec: rec}
	if st := o.importState(taskID); st != nil {
		a.st = st
		return a, nil
	}
	st, err := o.readImportArchive(ctx, a, p)
	if err != nil {
		a.close()
		return nil, err
	}
	o.setImportState(taskID, st)
	a.st = st
	return a, nil
}

// sumOfFile is the SHA-256 of the plaintext of the file, read from its bytes.
func sumOfFile(r io.Reader) (string, error) {
	h := sha256.New()
	if _, err := io.Copy(h, r); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// sortedCopy returns a sorted copy of a list.
func sortedCopy(l []string) []string {
	out := append([]string(nil), l...)
	sort.Strings(out)
	return out
}

var errImportTooMany = errors.New("too many")

// planOf parses the three SQL files of the archive with the shape grammar (collecting names, never text) and returns what the plan needs.
func (o *Ops) planOf(a *archiveFile, res *archive.Result) (*importPlan, error) {
	hints := sqlshape.Hints{InheritanceChildren: catalogInheritance(&res.Catalog)}
	pl := &importPlan{Tables: map[string]bool{}}
	funcs := map[string]bool{}
	byName := map[string]archive.EntryFact{}
	for _, f := range res.Entries {
		byName[f.Name] = f
	}
	sha := map[string]string{}
	for _, e := range res.Manifest.Entries {
		sha[e.Name] = e.SHA256
	}
	for _, n := range []string{archive.PreDataName, archive.ViewsName, archive.PostDataName} {
		f, ok := byName[n]
		if !ok {
			return nil, transfer.Errorf(transfer.CodeArchiveInvalid, false, "the archive has no %s", n)
		}
		er := archive.OpenEntry(a.r, f, sha[n])
		plan, err := sqlshape.ParseFile(er, n, hints)
		_ = er.Close()
		if err != nil {
			return nil, transfer.Errorf(transfer.CodeArchiveInvalid, false, "an SQL file of the archive could not be read as statements")
		}
		if !plan.Valid() {
			return nil, refusedError(plan.Refused)
		}
		pl.Statements += plan.Count()
		for _, fn := range plan.Functions {
			funcs[fn] = true
		}
		for _, st := range plan.Statements {
			switch st.Kind {
			case "CREATE TABLE":
				pl.Tables[st.Name] = true
			case "CREATE MATERIALIZED VIEW":
				pl.MatViews = append(pl.MatViews, st.Name)
			case "CREATE SCHEMA":
				pl.Schemas = append(pl.Schemas, st.Name)
			}
		}
	}
	for fn := range funcs {
		pl.Functions = append(pl.Functions, fn)
	}
	sort.Strings(pl.Functions)
	return pl, nil
}

// refusedError is the error of a task for the statements that the grammar refused: the kinds, the positions and the fingerprints of the first few, never text.
func refusedError(rs []sqlshape.Refusal) error {
	var parts []string
	for i, r := range rs {
		if i >= 5 {
			break
		}
		parts = append(parts, fmt.Sprintf("%s at %s (%s)", r.Kind, r.Position, r.Fingerprint))
	}
	return transfer.Errorf(transfer.CodeStatementRefused, false, "the shape grammar refuses %d statement(s) of the archive: %s; nothing was run", len(rs), strings.Join(parts, ", "))
}

// readImportArchive is the revalidation: the checksum of the file, the whole validator with the shape grammar on the three SQL files, the plan. It does not
// look at the state of the upload or at a cached report. Every refusal is a task error with a fixed text; nothing of the archive is in it.
func (o *Ops) readImportArchive(ctx context.Context, a *archiveFile, p dbImportParams) (*importState, error) {
	size := a.r.Size()
	sum, err := sumOfFile(io.NewSectionReader(a.r, 0, size))
	if err != nil {
		return nil, transfer.Errorf(transfer.CodeLeaderChanged, true, "the file of the upload could not be read; repeat the operation")
	}
	if a.rec.SHA256 != "" && sum != a.rec.SHA256 {
		return nil, transfer.Errorf(transfer.CodeKeyLost, false, "the staged file does not have the checksum it had when it was received")
	}
	if p.ExpectedSHA256 != "" && sum != p.ExpectedSHA256 {
		return nil, transfer.Errorf(transfer.CodeArchiveInvalid, false, "the archive is not the file that was expected: its SHA-256 differs from expectedSha256")
	}
	lim := o.importLimits(size)
	lim.SQLFor = func(cat *archive.Catalog) archive.SQLChecker { return sqlChecker(catalogInheritance(cat)) }
	res, err := archive.Validate(a.r, size, lim)
	if err != nil {
		var ae *archive.Error
		if errors.As(err, &ae) {
			return nil, archiveTaskError(ae)
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, transfer.Errorf(transfer.CodeArchiveInvalid, false, "the archive could not be read")
	}
	res.FileSHA256 = sum
	var refused []sqlshape.Refusal
	for _, n := range []string{archive.PreDataName, archive.ViewsName, archive.PostDataName} {
		for _, rf := range res.SQL[n].Refused {
			refused = append(refused, sqlshape.Refusal{Kind: rf.Kind, Position: rf.Position, Fingerprint: rf.Fingerprint, Reason: rf.Reason})
		}
	}
	if len(refused) > 0 {
		return nil, refusedError(refused)
	}
	plan, err := o.planOf(a, res)
	if err != nil {
		return nil, err
	}
	return &importState{res: res, plan: plan}, nil
}

// archiveTaskError turns the error of the validator into the error of a task: the code, and the first reason as a fixed sentence.
func archiveTaskError(ae *archive.Error) error {
	code := transfer.CodeArchiveInvalid
	if ae.Code == archive.CodeIncompatible {
		code = transfer.CodeArchiveIncompatible
	}
	text := "the archive is not valid"
	if len(ae.Reasons) > 0 {
		text = fmt.Sprintf("the archive is not valid (%s: %s)", safeName(ae.Reasons[0].Code), safeName(ae.Reasons[0].Object))
	}
	return transfer.Errorf(code, false, "%s", text)
}
