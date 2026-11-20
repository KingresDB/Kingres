// SPDX-License-Identifier: AGPL-3.0-only

package ledgeranchor

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"regexp"
	"strconv"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/anchor"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/api"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/problem"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/scheduler"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/store"
)

var nameRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9 ._-]{0,62}$`)

func fieldErr(field, msg, code string) problem.FieldError {
	return problem.FieldError{Field: field, Message: msg, Code: code}
}

// clean is a text from a library or from the operating system, reduced to printable ASCII and cut; the libraries
// build their errors from static text, so this is the second guard.
func clean(s string) string { return (anchor.Redactor{}).Clean(s, 300) }

// storeErr hides an error of the store behind a 503; a problem is kept, a deposed term is a 503 with a retry.
func storeErr(err error) error {
	var p *problem.Problem
	switch {
	case errors.As(err, &p):
		return p
	case errors.Is(err, scheduler.ErrDeposed):
		return problem.New(problem.ServiceUnavailable, "This server is no longer the manager leader; try again.").WithRetryAfter(5)
	case errors.Is(err, ErrNotFound):
		return problem.New(problem.NotFound, "No such anchor target or record.")
	}
	return err
}

// Evidence is a file for downloadLedgerAnchor.
type Evidence struct {
	ContentType string
	FileName    string
	Data        []byte
}

func (s *Service) apiTarget(v View) api.LedgerAnchorTarget {
	t := api.LedgerAnchorTarget{ID: v.Row.ID, Kind: v.Row.Kind, Name: v.Row.Name, Enabled: v.Row.Enabled, Config: v.Row.Config,
		CredentialsSet: v.Row.CredentialsSet || hasCredPath(v.Row.Config), NewestAnchoredAt: v.NewestAnchoredAt, State: v.State}
	if len(v.Row.LastTest) > 0 {
		var lt api.LedgerAnchorTest
		if json.Unmarshal(v.Row.LastTest, &lt) == nil {
			t.LastTest = &lt
		}
	}
	if len(t.Config) == 0 {
		t.Config = json.RawMessage(`{}`)
	}
	return t
}

func hasCredPath(cfg json.RawMessage) bool {
	_, p, err := splitConfig(cfg)
	return err == nil && p != ""
}

func apiAnchor(r Row) api.LedgerAnchor {
	a := api.LedgerAnchor{ID: r.ID, TargetName: r.TargetName, Kind: r.Kind, AggregateSize: int64(r.Size), //nolint:gosec // below 2^62
		State: apiState(r.State), Attempts: r.Attempts, NextAttemptAt: r.NextAttemptAt, AnchoredAt: r.AnchoredAt, EvidenceTime: r.EvidenceTime}
	if !r.TargetGone {
		id := r.TargetID
		a.TargetID = &id
	}
	if r.LastError != "" {
		e := r.LastError
		a.LastError = &e
	}
	// a record that waits for a state that nothing has produced shows no due time
	if r.State == anchor.StateAnchored || r.State == anchor.StateFailed || r.State == anchor.StateExpired || r.State == anchor.StateSuperseded {
		a.NextAttemptAt = nil
	}
	return a
}

// Targets is listLedgerAnchorTargets.
func (s *Service) Targets(ctx context.Context) (api.LedgerAnchorTargetList, error) {
	out := api.LedgerAnchorTargetList{Items: []api.LedgerAnchorTarget{}}
	sn, err := s.snapshot(ctx)
	if err != nil {
		return out, storeErr(err)
	}
	for _, v := range sn.Views {
		out.Items = append(out.Items, s.apiTarget(v))
	}
	return out, nil
}

func (s *Service) view(ctx context.Context, id int64) (View, error) {
	sn, err := s.snapshot(ctx)
	if err != nil {
		return View{}, err
	}
	for _, v := range sn.Views {
		if v.Row.ID == id {
			return v, nil
		}
	}
	return View{}, ErrNotFound
}

// Target is getLedgerAnchorTarget.
func (s *Service) Target(ctx context.Context, id int64) (api.LedgerAnchorTarget, error) {
	v, err := s.view(ctx, id)
	if err != nil {
		return api.LedgerAnchorTarget{}, storeErr(err)
	}
	return s.apiTarget(v), nil
}

// shaped is a target request that passed the checks, ready to store.
type shaped struct {
	config    json.RawMessage
	credPath  string
	creds     Creds
	kind      *Kind
	info      Info
	credsSet  bool // a file of ours holds credentials
	removeOld bool // credentials = {}: remove the stored file
}

// shape validates a configuration and credentials for a kind.
func (s *Service) shape(kind *Kind, rawCfg, rawCreds json.RawMessage) (shaped, *problem.Problem) {
	sh := shaped{kind: kind}
	cfg, credPath, err := splitConfig(rawCfg)
	if err != nil {
		return sh, problem.Validation("The configuration is not valid.", fieldErr("/config", clean(err.Error()), "invalid"))
	}
	norm, info, err := kind.Normalize(cfg)
	if err != nil {
		return sh, problem.Validation("The configuration is not valid.", fieldErr("/config", clean(err.Error()), "invalid"))
	}
	if info.CABundle != "" {
		if pem, err := ReadBundle(info.CABundle); err != nil {
			return sh, problem.Validation("The configuration is not valid.", fieldErr("/config/caBundlePath", clean(err.Error()), "invalid"))
		} else if kind.CheckBundle != nil && kind.CheckBundle(pem) != nil {
			return sh, problem.Validation("The configuration is not valid.", fieldErr("/config/caBundlePath", "the bundle is not a set of trust anchors for this kind (it needs a self-signed certificate)", "invalid"))
		} else if _, err := LoadPool(info.CABundle); err != nil {
			return sh, problem.Validation("The configuration is not valid.", fieldErr("/config/caBundlePath", clean(err.Error()), "invalid"))
		}
	} else if kind.CARequired {
		return sh, problem.Validation("The configuration is not valid.", fieldErr("/config/caBundlePath", "a CA bundle is required for this kind", "required"))
	}
	trim := bytes.TrimSpace(rawCreds)
	given := len(trim) > 0 && !bytes.Equal(trim, []byte("null"))
	if given {
		if !kind.CredentialsAllowed {
			return sh, problem.Validation("The credentials are not valid.", fieldErr("/credentials", "this kind of target takes no credentials", "invalid"))
		}
		if credPath != "" {
			return sh, problem.Validation("The credentials are not valid.", fieldErr("/credentials", "credentials and credentialsPath exclude each other", "invalid"))
		}
		c, err := ParseCreds(kind.Name, rawCreds)
		if err != nil {
			return sh, problem.Validation("The credentials are not valid.", fieldErr("/credentials", clean(err.Error()), "invalid"))
		}
		if c == nil {
			sh.removeOld = true
		}
		sh.creds = c
	}
	if credPath != "" {
		if !kind.CredentialsAllowed {
			return sh, problem.Validation("The configuration is not valid.", fieldErr("/config/credentialsPath", "this kind of target takes no credentials", "invalid"))
		}
		if _, err := ReadCredentialsFile(credPath, kind.Name); err != nil {
			return sh, problem.Validation("The configuration is not valid.", fieldErr("/config/credentialsPath",
				"the credentials file cannot be read by this server (a regular file that only its owner can read, holding the members of the kind)", "invalid"))
		}
	}
	joined, err := joinConfig(norm, credPath)
	if err != nil {
		return sh, problem.Validation("The configuration is not valid.", fieldErr("/config", "the configuration is not valid", "invalid"))
	}
	sh.config, sh.credPath, sh.info, sh.credsSet = joined, credPath, info, sh.creds != nil
	return sh, nil
}

func checkName(name string) *problem.Problem {
	if !nameRE.MatchString(name) {
		return problem.Validation("The name is not valid.", fieldErr("/name", "1 to 63 characters: letters, digits, space, dot, hyphen and underscore, starting with a letter or a digit", "invalid"))
	}
	return nil
}

// CreateTarget is createLedgerAnchorTarget.
func (s *Service) CreateTarget(ctx context.Context, actor string, in api.LedgerAnchorTargetCreate) (api.LedgerAnchorTarget, error) {
	term, err := s.term()
	if err != nil {
		return api.LedgerAnchorTarget{}, err
	}
	if p := checkName(in.Name); p != nil {
		return api.LedgerAnchorTarget{}, p
	}
	k, err := kindOf(in.Kind)
	if err != nil {
		return api.LedgerAnchorTarget{}, problem.Validation("The kind is not valid.", fieldErr("/kind", "one of "+joinKinds(), "invalid"))
	}
	sh, p := s.shape(k, in.Config, in.Credentials)
	if p != nil {
		return api.LedgerAnchorTarget{}, p
	}
	if in.Enabled && k.CredentialsRequired && !sh.credsSet && sh.credPath == "" {
		return api.LedgerAnchorTarget{}, problem.Validation("The credentials are not valid.", fieldErr("/credentials", "credentials are required to enable a target of this kind", "required"))
	}
	now := s.now()
	row := TargetRow{Kind: k.Name, Name: in.Name, Enabled: in.Enabled, Config: sh.config, CredentialsSet: sh.credsSet, CreatedBy: actor}
	if in.Enabled {
		row.EnabledSince = &now
	}
	created, err := s.cfg.Store.CreateTarget(ctx, term, row)
	switch {
	case errors.Is(err, ErrExists):
		return api.LedgerAnchorTarget{}, problem.New(problem.AlreadyExists, "An anchor target with this name exists.")
	case err != nil:
		return api.LedgerAnchorTarget{}, storeErr(err)
	}
	if sh.creds != nil {
		if err := s.putCreds(ctx, created.ID, k.Name, sh.creds); err != nil {
			// the row and its credentials go together: without the file the row is taken away again
			if derr := s.cfg.Store.DeleteTarget(ctx, term, created.ID); derr != nil {
				s.log.Warn("a target whose credentials could not be stored was not removed", "target", created.ID, "error", errText(derr))
			}
			return api.LedgerAnchorTarget{}, problem.New(problem.ServiceUnavailable, "The credentials could not be stored on this server.").WithRetryAfter(5)
		}
	}
	return s.Target(ctx, created.ID)
}

func joinKinds() string {
	out := ""
	for i, n := range KindNames() {
		if i > 0 {
			out += ", "
		}
		out += n
	}
	return out
}

// putCreds writes the file of a target on this server and hands it to the other holders (best effort).
func (s *Service) putCreds(ctx context.Context, id int64, kind string, c Creds) error {
	data, err := s.cfg.Creds.Put(id, c)
	if err != nil {
		return err
	}
	defer clear(data)
	if s.cfg.Distribute != nil {
		if err := s.cfg.Distribute(ctx, id, kind, data); err != nil {
			s.log.Warn("the credentials of an anchor target could not be given to every holder", "target", id, "error", errText(err))
		}
	}
	return nil
}

// UpdateTarget is updateLedgerAnchorTarget.
func (s *Service) UpdateTarget(ctx context.Context, id int64, in api.LedgerAnchorTargetUpdate) (api.LedgerAnchorTarget, error) {
	term, err := s.term()
	if err != nil {
		return api.LedgerAnchorTarget{}, err
	}
	row, err := s.cfg.Store.Target(ctx, id)
	if err != nil {
		return api.LedgerAnchorTarget{}, storeErr(err)
	}
	if in.Kind != nil && *in.Kind != row.Kind {
		return api.LedgerAnchorTarget{}, problem.New(problem.ImmutableField, "The kind of a target cannot be changed.")
	}
	k, err := kindOf(row.Kind)
	if err != nil {
		return api.LedgerAnchorTarget{}, problem.New(problem.UnsupportedOp, "This server cannot work with targets of this kind.")
	}
	next := row
	if in.Name != nil {
		if p := checkName(*in.Name); p != nil {
			return api.LedgerAnchorTarget{}, p
		}
		next.Name = *in.Name
	}
	cfg := row.Config
	if len(bytes.TrimSpace(in.Config)) > 0 {
		cfg = in.Config
	}
	sh, p := s.shape(k, cfg, in.Credentials)
	if p != nil {
		return api.LedgerAnchorTarget{}, p
	}
	next.Config = sh.config
	switch {
	case sh.creds != nil:
		next.CredentialsSet = true
	case sh.removeOld:
		next.CredentialsSet = false
	}
	if sh.credPath != "" && sh.creds == nil && !sh.removeOld {
		// the operator's own file replaces ours
		next.CredentialsSet = false
	}
	if in.Enabled != nil {
		next.Enabled = *in.Enabled
	}
	now := s.now()
	switch {
	case next.Enabled && !row.Enabled:
		next.EnabledSince = &now
	case !next.Enabled:
		next.EnabledSince = nil
	}
	if next.Enabled && k.CredentialsRequired && !next.CredentialsSet && sh.credPath == "" {
		return api.LedgerAnchorTarget{}, problem.Validation("The credentials are not valid.", fieldErr("/credentials", "credentials are required to enable a target of this kind", "required"))
	}
	if sh.creds != nil {
		if err := s.putCreds(ctx, id, k.Name, sh.creds); err != nil {
			return api.LedgerAnchorTarget{}, problem.New(problem.ServiceUnavailable, "The credentials could not be stored on this server.").WithRetryAfter(5)
		}
	}
	if _, err := s.cfg.Store.UpdateTarget(ctx, term, next, row.Revision); err != nil {
		switch {
		case errors.Is(err, ErrExists):
			return api.LedgerAnchorTarget{}, problem.New(problem.AlreadyExists, "An anchor target with this name exists.")
		case errors.Is(err, ErrConflict):
			return api.LedgerAnchorTarget{}, problem.New(problem.Conflict, "The target was changed meanwhile; read it and try again.")
		}
		return api.LedgerAnchorTarget{}, storeErr(err)
	}
	if !next.CredentialsSet && row.CredentialsSet {
		s.dropCreds(ctx, id)
	}
	return s.Target(ctx, id)
}

func (s *Service) dropCreds(ctx context.Context, id int64) {
	if err := s.cfg.Creds.Remove(id); err != nil {
		s.log.Warn("the credentials file of an anchor target was not removed", "target", id)
	}
	if s.cfg.Undistribute != nil {
		if err := s.cfg.Undistribute(ctx, id); err != nil {
			s.log.Warn("the credentials of an anchor target were not removed from every holder", "target", id, "error", errText(err))
		}
	}
}

// DeleteTarget is deleteLedgerAnchorTarget; the records stay and name the target by its name.
func (s *Service) DeleteTarget(ctx context.Context, id int64) error {
	term, err := s.term()
	if err != nil {
		return err
	}
	if err := s.cfg.Store.DeleteTarget(ctx, term, id); err != nil {
		return storeErr(err)
	}
	s.dropCreds(ctx, id)
	return nil
}

func apiTest(res anchor.TestResult, red anchor.Redactor, at time.Time) api.LedgerAnchorTest {
	out := api.LedgerAnchorTest{Checks: []api.LedgerAnchorCheck{}, OK: res.OK()}
	for _, c := range res.Checks {
		out.Checks = append(out.Checks, api.LedgerAnchorCheck{Name: red.Clean(c.Name, 60), Status: c.Status, Detail: red.Clean(c.Text, 800)})
	}
	t := at.UTC().Truncate(time.Second)
	out.TestedAt = &t
	return out
}

// TestTarget is testLedgerAnchorTarget: the checks of the kind now, synchronously, within TestTimeout. A failure
// of the remote side is a failed check in the result, not an error.
func (s *Service) TestTarget(ctx context.Context, id int64) (api.LedgerAnchorTest, error) {
	term, err := s.term()
	if err != nil {
		return api.LedgerAnchorTest{}, err
	}
	row, err := s.cfg.Store.Target(ctx, id)
	if err != nil {
		return api.LedgerAnchorTest{}, storeErr(err)
	}
	var out api.LedgerAnchorTest
	b, berr := s.build(row)
	if berr != nil {
		var res anchor.TestResult
		res.Add("target", anchor.CheckFailed, "%s", clean(berr.Error()))
		out = apiTest(res, anchor.Redactor{}, s.now())
	} else {
		cctx, cancel := context.WithTimeout(ctx, TestTimeout)
		res := b.Target.Test(cctx)
		cancel()
		if b.Kind.Name == anchor.KindWORM {
			res.Checks = append(res.Checks, s.clockCheck(ctx))
			res.Checks = append(res.Checks, s.evidenceChecks(ctx, row)...)
		}
		out = apiTest(res, b.Red, s.now())
	}
	if raw, err := json.Marshal(out); err == nil {
		if err := s.cfg.Store.SetLastTest(ctx, term, id, raw); err != nil && !errors.Is(err, ErrNotFound) {
			s.log.Warn("the result of a test of an anchor target was not stored", "target", id, "error", errText(err))
		}
	}
	return out, nil
}

// List is listLedgerAnchors.
func (s *Service) List(ctx context.Context, db string, targetID int64, state string, p store.Page) (api.LedgerAnchorPage, error) {
	page := api.LedgerAnchorPage{Items: []api.LedgerAnchor{}}
	if state != "" && !validAPIState(state) {
		return page, problem.Validation("The state is not valid.", fieldErr("state", "one of pending, submitted, anchored, failed, superseded, expired", "invalid"))
	}
	before, _, err := store.DecodeKey(p.Cursor)
	if err != nil {
		return page, problem.Validation("The cursor is not valid.", fieldErr("cursor", "not a cursor issued by this server", "invalid_cursor"))
	}
	if err := s.knownDB(ctx, db); err != nil {
		return page, err
	}
	rows, err := s.cfg.Store.Records(ctx, Filter{Database: db, TargetID: targetID, State: state, Before: before, Limit: p.Limit + 1})
	if err != nil {
		return page, storeErr(err)
	}
	if len(rows) > p.Limit {
		next := store.EncodeKey(rows[p.Limit-1].ID)
		page.NextCursor = &next
		rows = rows[:p.Limit]
	}
	for _, r := range rows {
		page.Items = append(page.Items, apiAnchor(r))
	}
	return page, nil
}

func (s *Service) knownDB(ctx context.Context, db string) error {
	if s.cfg.DatabaseExists == nil {
		return nil
	}
	ok, err := s.cfg.DatabaseExists(ctx, db)
	if err != nil {
		return err
	}
	if !ok {
		return problem.New(problem.NotFound, "No such database.")
	}
	return nil
}

// AnchorNow is anchorLedgerNow: a record of the newest aggregate checkpoint for every enabled target, without
// waiting for the interval (correction 123: 201 with the records; the job submits them).
func (s *Service) AnchorNow(ctx context.Context, db string) (api.LedgerAnchorList, error) {
	out := api.LedgerAnchorList{Items: []api.LedgerAnchor{}}
	term, err := s.term()
	if err != nil {
		return out, err
	}
	if err := s.knownDB(ctx, db); err != nil {
		return out, err
	}
	agg, err := s.cfg.Store.AggregateOf(ctx, db)
	if errors.Is(err, ErrNotFound) || errors.Is(err, ErrNoAggregate) {
		return out, problem.New(problem.LedgerNotPublished, "The ledger of this database has no published aggregate yet.")
	}
	if err != nil {
		return out, storeErr(err)
	}
	targets, err := s.cfg.Store.Targets(ctx)
	if err != nil {
		return out, storeErr(err)
	}
	now := s.now()
	for _, t := range targets {
		if !t.Enabled {
			continue
		}
		row, _, err := s.cfg.Store.Enqueue(ctx, term, Enqueue{Target: Target{ID: t.ID, Name: t.Name, Kind: t.Kind}, Agg: agg, Now: now})
		if err != nil {
			return out, storeErr(err)
		}
		row.Note = nil
		out.Items = append(out.Items, apiAnchor(row))
	}
	if len(out.Items) == 0 {
		return out, problem.New(problem.Conflict, "No anchor target is enabled.")
	}
	return out, nil
}

// Retry is retryLedgerAnchor: a failed or expired record becomes pending again, due at once. It never raises the
// generation of a WORM key (that is an explicit choice that the contract does not offer, correction 183).
func (s *Service) Retry(ctx context.Context, db string, id int64) (api.LedgerAnchor, error) {
	term, err := s.term()
	if err != nil {
		return api.LedgerAnchor{}, err
	}
	row, err := s.cfg.Store.Record(ctx, id)
	if err != nil {
		return api.LedgerAnchor{}, storeErr(err)
	}
	if row.Database != db {
		return api.LedgerAnchor{}, problem.New(problem.NotFound, "No such anchor record in this database.")
	}
	if row.TargetGone {
		return api.LedgerAnchor{}, problem.New(problem.Conflict, "The target of this record was deleted.")
	}
	rec, err := row.Record.Retry(s.now())
	if err != nil {
		return api.LedgerAnchor{}, problem.New(problem.Conflict, "Only a failed or expired record can be retried (this one is "+apiState(row.State)+").")
	}
	w := row
	w.Record = rec
	w.Alert, w.ErrorCode = "", ""
	if err := s.cfg.Store.Apply(ctx, term, w, row.Version); err != nil {
		if errors.Is(err, ErrConflict) {
			return api.LedgerAnchor{}, problem.New(problem.Conflict, "The record changed meanwhile; read it and try again.")
		}
		return api.LedgerAnchor{}, storeErr(err)
	}
	after, err := s.cfg.Store.Record(ctx, id)
	if err != nil {
		return api.LedgerAnchor{}, storeErr(err)
	}
	return apiAnchor(after), nil
}

// Evidence is downloadLedgerAnchor. format is json (every kind), ots or tsr (the kinds that have them).
func (s *Service) Evidence(ctx context.Context, db string, id int64, format string) (Evidence, error) {
	row, err := s.cfg.Store.Record(ctx, id)
	if err != nil {
		return Evidence{}, storeErr(err)
	}
	if row.Database != db {
		return Evidence{}, problem.New(problem.NotFound, "No such anchor record in this database.")
	}
	if len(row.Evidence) == 0 {
		return Evidence{}, problem.New(problem.NotFound, "This record has no evidence yet.")
	}
	switch format {
	case "", "json":
		at := row.UpdatedAt
		if row.AnchoredAt != nil {
			at = *row.AnchoredAt
		}
		data, err := anchor.EncodeEvidenceFile(anchor.EvidenceFile{Kind: row.Kind, Origin: row.Origin, Size: row.Size,
			PayloadSHA256: hex.EncodeToString(row.PayloadSHA256[:]), CreatedAt: at, Evidence: row.Evidence})
		if err != nil {
			s.log.Error("the evidence of an anchor record does not encode", "record", id, "error", errText(err))
			return Evidence{}, problem.New(problem.ServiceUnavailable, "The evidence of this record cannot be encoded now.").WithRetryAfter(5)
		}
		return Evidence{ContentType: "application/json", FileName: "anchor-" + strconv.FormatInt(id, 10) + ".json", Data: data}, nil
	case "ots", "tsr":
		exp, ok := s.exporters[format]
		if !ok || exp.kind != row.Kind {
			return Evidence{}, problem.New(problem.Unprocessable, "This kind of target has no file in the format "+format+".")
		}
		data, err := exp.fn(row)
		if err != nil {
			return Evidence{}, problem.New(problem.Unprocessable, "The evidence of this record cannot be written in the format "+format+": "+clean(err.Error()))
		}
		return Evidence{ContentType: "application/octet-stream", FileName: "anchor-" + strconv.FormatInt(id, 10) + "." + format, Data: data}, nil
	}
	return Evidence{}, problem.Validation("The format is not valid.", fieldErr("format", "one of json, ots, tsr", "invalid"))
}
