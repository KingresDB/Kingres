// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package ops

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/api"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/extguard"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/peerclient"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/problem"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/sysdb"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/task"
)

// The task database.approximateAggregates (phase 12, analytics-phase12-queries.md section 12).

// KindApproximateAggregates is the task kind; its subject is the database, the subject of database.update, so that another database task on the
// same database is refused with operation_in_progress.
const KindApproximateAggregates = "database.approximateAggregates"

// The names of the steps that do not depend on the number of servers.
const (
	stepApproxCheck  = "Check the servers"
	stepApproxVerify = "Verify versions and hashes"
	stepApproxApply  = "Apply the database settings"
	stepApproxRecord = "Record the settings"
	// stepApproxInstall is the format of the step that is made for each server.
	stepApproxInstall = "Install the extension on server %s"
)

const (
	defaultErrorRate = 0.01
	minErrorRate     = 0.0005
	maxErrorRate     = 0.1
)

var rateRE = regexp.MustCompile(`^0\.[0-9]{1,6}$`)

// approxParams is the input of the task.
type approxParams struct {
	Database      string  `json:"database"`
	Enabled       bool    `json:"enabled"`
	CountDistinct string  `json:"countDistinct"`
	ErrorRate     float64 `json:"errorRate"`
	// Servers are every server of the cluster when the task was made, this one included; the task has one install step for each.
	Servers []string `json:"servers"`
	Actor   string   `json:"actor"`
}

// RemoteQuerier runs a read-only statement on another server (the internal call exec of the leader); it is what getApproximateAggregates
// uses to see the version each server has installed. A server that cannot be reached gives an error wrapping peerclient.ErrUnreachable.
type RemoteQuerier interface {
	Query(ctx context.Context, server, db, sql string) ([][]any, error)
}

func (o *Ops) approxDefinition() task.Definition {
	return task.Definition{Kind: KindApproximateAggregates, Dynamic: func(in json.RawMessage) ([]task.Step, error) {
		p, err := rawInput[approxParams](in)
		if err != nil {
			return nil, err
		}
		steps := []task.Step{{Name: stepApproxCheck, Interruptible: true, Run: o.stepApproxCheck}}
		for _, s := range p.Servers {
			steps = append(steps, task.Step{Name: fmt.Sprintf(stepApproxInstall, s), Interruptible: true, Run: func(ctx context.Context, env *task.Env) error {
				return o.stepApproxInstall(ctx, env, s)
			}})
		}
		return append(steps,
			task.Step{Name: stepApproxVerify, Interruptible: true, Run: o.stepApproxVerify},
			task.Step{Name: stepApproxApply, Interruptible: true, Run: o.stepApproxApply},
			task.Step{Name: stepApproxRecord, Run: o.stepApproxRecord}), nil
	}}
}

// PrepareApproximateAggregates validates the request and returns the task input; nothing changes yet.
func (o *Ops) PrepareApproximateAggregates(ctx context.Context, actor, db string, req api.ApproximateAggregatesRequest) (any, error) {
	if !dbNameRE.MatchString(db) || protectedDB[db] {
		return nil, problem.New(problem.NotFound, "No such database.")
	}
	mode, rate, err := validateApprox(req)
	if err != nil {
		return nil, err
	}
	err = o.asService(ctx, sysdb.Database, func(tx pgx.Tx) error {
		if ok, err := dbExists(ctx, tx, db); err != nil {
			return err
		} else if !ok {
			return problem.New(problem.NotFound, fmt.Sprintf("Database %q does not exist.", db))
		}
		var one int
		if err := tx.QueryRow(ctx, "SELECT 1 FROM kingres_cp.database_setting WHERE database_name = $1", db).Scan(&one); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return problem.New(problem.Conflict, "This database was not created by the control plane; its settings cannot be changed here.")
			}
			return err
		}
		return nil
	})
	if err != nil {
		return nil, wrap(err)
	}
	return approxParams{Database: db, Enabled: req.Enabled, CountDistinct: mode, ErrorRate: rate,
		Servers: append([]string{o.ServerHost}, ids(o.others(ctx))...), Actor: actor}, nil
}

// validateApprox checks the request and returns the count-distinct mode and the error rate (0 when the mode is exact).
func validateApprox(req api.ApproximateAggregatesRequest) (string, float64, error) {
	var errs []problem.FieldError
	mode := req.CountDistinct
	switch mode {
	case "":
		mode = "exact"
	case "exact", "approximate":
	default:
		errs = append(errs, field("/countDistinct", "must be exact or approximate", "enum"))
	}
	if mode == "approximate" && !req.Enabled {
		errs = append(errs, field("/countDistinct", "approximate needs enabled to be true", "conflict"))
	}
	rate := 0.0
	switch {
	case req.ErrorRate != nil && mode != "approximate":
		errs = append(errs, field("/errorRate", "is only valid with countDistinct approximate", "conflict"))
	case req.ErrorRate != nil:
		rate = *req.ErrorRate
		if math.IsNaN(rate) || rate < minErrorRate || rate > maxErrorRate {
			errs = append(errs, field("/errorRate", "must be between 0.0005 and 0.1", "out_of_range"))
		}
	case mode == "approximate":
		rate = defaultErrorRate
	}
	if len(errs) > 0 {
		return "", 0, problem.Validation("The request is invalid.", errs...)
	}
	return mode, rate, nil
}

// others of the task are the servers other than this one.
func (p approxParams) others(self string) []string {
	var out []string
	for _, s := range p.Servers {
		if s != self {
			out = append(out, s)
		}
	}
	return out
}

// step 1: every server can install the extension, at one version.
func (o *Ops) stepApproxCheck(ctx context.Context, env *task.Env) error {
	p, err := inputOf[approxParams](env.Task)
	if err != nil {
		return err
	}
	// This server first: its default version is the one the others must have.
	v, err := o.ext().Available(ctx, p.Database, ExtSketch)
	if err != nil {
		return extErr(problem.ExtensionUnavailable, "server %s: the database cannot be opened: %s", o.ServerHost, problemText(err))
	}
	if v == "" {
		return extErr(problem.ExtensionUnavailable, "server %s: the package does not contain %s", o.ServerHost, ExtSketch)
	}
	op := extensionOp{Database: p.Database, Extension: ExtSketch, Version: v}
	return o.onEveryServer(ctx, env, p.others(o.ServerHost), "check the extension",
		func() error { return nil },
		func(s string) error { return o.Fanout.Apply(ctx, s, ApplyExtensionCheck, op) })
}

// step 2 (one for each server): install, or update to the default version.
func (o *Ops) stepApproxInstall(ctx context.Context, env *task.Env, server string) error {
	p, err := inputOf[approxParams](env.Task)
	if err != nil {
		return err
	}
	op := extensionOp{Database: p.Database, Extension: ExtSketch}
	return o.onEveryServer(ctx, env, []string{server}, "install the extension",
		func() error { return o.extInstallLocal(ctx, op) },
		func(s string) error { return o.Fanout.Apply(ctx, s, ApplyExtensionInstall, op) })
}

// step 3: every server runs the version of this one and answers the known-answer values alike. It checks every server and names all that differ.
func (o *Ops) stepApproxVerify(ctx context.Context, env *task.Env) error {
	p, err := inputOf[approxParams](env.Task)
	if err != nil {
		return err
	}
	b := o.ext()
	v, err := b.Installed(ctx, p.Database, ExtSketch)
	if err != nil {
		return err
	}
	if v == "" {
		return extErr(problem.ExtensionUnavailable, "server %s: %s is not installed in the database", o.ServerHost, ExtSketch)
	}
	h, err := b.SelfTest(ctx, p.Database)
	if err != nil {
		return err
	}
	op := extensionOp{Database: p.Database, Extension: ExtSketch, Version: v, Hash: h}
	var bad []*extError
	for _, s := range p.others(o.ServerHost) {
		err := o.onServer(ctx, env, s, "verify the extension", func() error { return o.Fanout.Apply(ctx, s, ApplyExtensionVerify, op) })
		if err == nil {
			continue
		}
		ee, ok := asExtError(s, err).(*extError)
		if !ok {
			return err
		}
		bad = append(bad, ee)
	}
	if len(bad) == 0 {
		return nil
	}
	msgs := make([]string, len(bad))
	for i, e := range bad {
		msgs[i] = e.msg
	}
	return &extError{code: bad[0].code, msg: strings.Join(msgs, "; ")}
}

// settings returns the database settings of the request: the mode, the error rate (reset when the mode is exact) and the pushdown.
func (p approxParams) settings() map[string]string {
	s := map[string]string{
		"kingres_shard.count_distinct_mode":       p.CountDistinct,
		"kingres_shard.count_distinct_error_rate": "",
		"kingres_shard.enable_sketch_pushdown":    "off",
	}
	if p.Enabled {
		s["kingres_shard.enable_sketch_pushdown"] = "on"
	}
	if p.CountDistinct == "approximate" {
		s["kingres_shard.count_distinct_error_rate"] = strconv.FormatFloat(p.ErrorRate, 'f', -1, 64)
	}
	return s
}

// step 4: ALTER DATABASE ... SET on every server. A server whose kingres_shard does not know a setting (an older version) skips it and the step says so.
func (o *Ops) stepApproxApply(ctx context.Context, env *task.Env) error {
	p, err := inputOf[approxParams](env.Task)
	if err != nil {
		return err
	}
	op := settingsOp{Database: p.Database, Settings: p.settings()}
	var skipped []string
	err = o.onEveryServer(ctx, env, p.Servers, "apply the database settings",
		func() error {
			u, err := o.settingsLocal(ctx, op)
			skipped = append(skipped, u...)
			return err
		},
		func(s string) error { return o.Fanout.Apply(ctx, s, ApplyDatabaseSettings, op) })
	if err != nil {
		return err
	}
	if len(skipped) > 0 {
		sort.Strings(skipped)
		_ = env.SetDetail(ctx, "This server's kingres_shard does not know "+strings.Join(skipped, ", ")+"; the setting was skipped.")
	}
	return nil
}

// step 5: the row of the database in the system database.
func (o *Ops) stepApproxRecord(ctx context.Context, env *task.Env) error {
	p, err := inputOf[approxParams](env.Task)
	if err != nil {
		return err
	}
	if o.approxRecordOverride != nil {
		return o.approxRecordOverride(ctx, p)
	}
	return o.asService(ctx, sysdb.Database, func(tx pgx.Tx) error {
		var rate any
		if p.CountDistinct == "approximate" {
			rate = p.ErrorRate
		}
		tag, err := tx.Exec(ctx, `UPDATE kingres_cp.database_setting SET approx_enabled = $2, approx_count_distinct = $3, approx_error_rate = $4
			WHERE database_name = $1`, p.Database, p.Enabled, p.CountDistinct, rate)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return errors.New("the database has no recorded settings")
		}
		return nil
	})
}

// GetApproximateAggregates reads the recorded setting and, from every server, the version of the extension installed in the database.
func (o *Ops) GetApproximateAggregates(ctx context.Context, db string) (api.ApproximateAggregates, error) {
	out := api.ApproximateAggregates{CountDistinct: "exact", Servers: []api.ApproximateServer{}}
	if !dbNameRE.MatchString(db) || protectedDB[db] {
		return out, problem.New(problem.NotFound, "No such database.")
	}
	err := o.asService(ctx, sysdb.Database, func(tx pgx.Tx) error {
		if ok, err := dbExists(ctx, tx, db); err != nil {
			return err
		} else if !ok {
			return problem.New(problem.NotFound, fmt.Sprintf("Database %q does not exist.", db))
		}
		var rate *float64
		err := tx.QueryRow(ctx, "SELECT approx_enabled, approx_count_distinct, approx_error_rate FROM kingres_cp.database_setting WHERE database_name = $1", db).
			Scan(&out.Enabled, &out.CountDistinct, &rate)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil // not created by the control plane: off
		}
		if rate != nil && out.CountDistinct == "approximate" {
			r := math.Round(*rate*1e6) / 1e6 // the column is a real
			out.ErrorRate = &r
		}
		return err
	})
	if err != nil {
		return out, wrap(err)
	}
	st, err := o.getServerStates(ctx, db)
	if err != nil {
		return out, err
	}
	out.Servers, out.ExtensionVersion = st.Servers, st.Version
	return out, nil
}

// serverStates is what the servers say about the extension: the state of each and the version they all run (nil when they do not).
type serverStates struct {
	Servers []api.ApproximateServer
	Version *string
}

// getServerStates reads the version of kingres_sketch installed in the database on this server and on every other.
func (o *Ops) getServerStates(ctx context.Context, db string) (serverStates, error) {
	var out serverStates
	type seen struct {
		id      string
		version string // "" not installed
		down    bool
	}
	all := []seen{}
	if v, err := o.ext().Installed(ctx, db, ExtSketch); err != nil {
		all = append(all, seen{id: o.ServerHost, down: true})
	} else {
		all = append(all, seen{id: o.ServerHost, version: v})
	}
	for _, s := range o.others(ctx) {
		if o.Remote == nil {
			all = append(all, seen{id: s.ID, down: true})
			continue
		}
		// extguard's check (version and the reason it cannot be trusted, if any), not the name alone.
		statusSQL, err := extguard.StatusSQL(ExtSketch)
		if err != nil {
			return out, err
		}
		rows, err := o.Remote.Query(ctx, s.ID, db, statusSQL)
		if err == nil && len(rows) > 0 && len(rows[0]) > 1 {
			if reason, _ := rows[0][1].(string); reason != "" {
				return out, problem.New(problem.Conflict, "The schema of "+ExtSketch+" in this database on server "+s.ID+" cannot be trusted ("+reason+
					"); nothing in it is read or called. An operator must restore it (see the alert extensionSchemaUntrusted).")
			}
		}
		switch {
		case err != nil && !errors.Is(err, peerclient.ErrUnreachable) && !isRemoteRefusal(err):
			return out, err
		case err != nil:
			all = append(all, seen{id: s.ID, down: true})
		case len(rows) == 0 || len(rows[0]) == 0:
			all = append(all, seen{id: s.ID})
		default:
			v, _ := rows[0][0].(string)
			all = append(all, seen{id: s.ID, version: v})
		}
	}
	// The reference is the highest version found; a server with a lower one is a mismatch.
	ref := ""
	for _, s := range all {
		if compareVersions(s.version, ref) > 0 {
			ref = s.version
		}
	}
	equal, reachable := true, 0
	for _, s := range all {
		st := api.ApproximateServer{ServerID: s.id}
		switch {
		case s.down:
			st.State = "unreachable"
		case s.version == "":
			st.State = "notInstalled"
			reachable++
			equal = false
		case s.version != ref:
			v := s.version
			st.InstalledVersion, st.State = &v, "versionMismatch"
			reachable++
			equal = false
		default:
			v := s.version
			st.InstalledVersion, st.State = &v, "ok"
			reachable++
		}
		out.Servers = append(out.Servers, st)
	}
	sort.Slice(out.Servers, func(i, j int) bool { return out.Servers[i].ServerID < out.Servers[j].ServerID })
	if equal && reachable > 0 && ref != "" {
		out.Version = &ref
	}
	return out, nil
}

// isRemoteRefusal tells whether another server refused the read (it is then shown as unreachable: the page cannot say more).
func isRemoteRefusal(err error) bool {
	var ae *peerclient.APIError
	return errors.As(err, &ae)
}

// compareVersions compares two extension versions such as 1.0, 0.9 and 0.10 as numbers part by part (0.10 is above 0.9); a part that is not a number is
// compared as text. The empty version is below every other.
func compareVersions(a, b string) int {
	if a == b {
		return 0
	}
	if a == "" {
		return -1
	}
	if b == "" {
		return 1
	}
	pa, pb := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < len(pa) || i < len(pb); i++ {
		x, y := "0", "0" // a missing part is zero: 1.0 and 1.0.0 are the same
		if i < len(pa) {
			x = pa[i]
		}
		if i < len(pb) {
			y = pb[i]
		}
		nx, ex := strconv.Atoi(x)
		ny, ey := strconv.Atoi(y)
		switch {
		case ex == nil && ey == nil && nx != ny:
			if nx < ny {
				return -1
			}
			return 1
		case (ex != nil || ey != nil) && x != y:
			if x < y {
				return -1
			}
			return 1
		}
	}
	return 0
}
