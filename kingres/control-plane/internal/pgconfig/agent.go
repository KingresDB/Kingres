// SPDX-License-Identifier: AGPL-3.0-only

package pgconfig

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/cluster"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/instance"
)

// Keys of the internal calls this package answers (internal-api.md, "Configuration").
const (
	CallState   = "POST /config/state"
	CallApply   = "POST /config/apply"
	CallCheck   = "POST /config/check"
	CallRestart = "POST /config/restart"
	CallTime    = "GET /time"
)

// States of a server or a parameter after an apply.
const (
	StateApplied        = "applied"
	StatePendingRestart = "pendingRestart"
	StateFailed         = "failed"
	StateWaiting        = "waiting"
)

// Agent is the part of phase 13 that runs on every server: it reads PostgreSQL's settings of the
// local instances, writes and checks the override file, reloads, reads back, sets database values,
// restarts an instance and answers the clock probe.
type Agent struct {
	Svc  *cluster.Service
	Self string
	Log  *slog.Logger
	// Now is the wall clock (default time.Now); the fault hook of the tests steps it.
	Now func() time.Time

	mu    sync.Mutex // one apply or restart at a time
	zmu   sync.Mutex
	zones zoneCache
}

type zoneCache struct {
	at         time.Time
	tz, logTZ  string
	refreshing bool
}

// StateRequest asks for the state of this server.
type StateRequest struct {
	// Meta also returns the metadata of every parameter (the leader needs it from its own server).
	Meta bool `json:"meta"`
}

// InstanceInfo is one local instance.
type InstanceInfo struct {
	ID      int      `json:"id"`
	Role    string   `json:"role"`
	Port    int      `json:"port"`
	Running bool     `json:"running"`
	Pending []string `json:"pending,omitempty"`
	// Values are the running and the pending value of the five settings a standby needs at least as high as its primary.
	Values map[string]string `json:"values,omitempty"`
	Token  string            `json:"token,omitempty"`
}

// StateResponse is the answer of CallState.
type StateResponse struct {
	Server   string `json:"server"`
	Revision int64  `json:"revision"`
	Version  string `json:"version"`
	// Settings are the parameters of the primary instance; without Meta only the members that
	// change are set (name, setting, source, pending, error).
	Settings   []Setting      `json:"settings"`
	Hidden     []string       `json:"hidden"`
	Instances  []InstanceInfo `json:"instances"`
	OSTimeZone string         `json:"osTimeZone"`
	Reachable  bool           `json:"reachable"`
	// ConfigHash identifies the settings of the override file of the primary instance (not its header).
	ConfigHash string `json:"configHash"`
}

// ApplyRequest is the complete configuration of one server at a revision.
type ApplyRequest struct {
	Revision int64 `json:"revision"`
	// Values are the parameters of the override file (lower-case name to value), already merged by
	// precedence for this server.
	Values map[string]string `json:"values"`
	// Databases are the database-level values (database to parameter to value) and Removed the
	// parameters that were set for a database and must be reset.
	Databases map[string]map[string]string `json:"databases"`
	Removed   map[string][]string          `json:"removed"`
	// Replicas is the cluster's replica count (for the floors).
	Replicas int `json:"replicas"`
}

// ParamResult is the outcome for one parameter.
type ParamResult struct {
	Name         string `json:"name"`
	State        string `json:"state"`
	Value        string `json:"value,omitempty"`
	PendingValue string `json:"pendingValue,omitempty"`
	Error        string `json:"error,omitempty"`
}

// ApplyResponse is the outcome on one server.
type ApplyResponse struct {
	State      string        `json:"state"`
	Detail     string        `json:"detail,omitempty"`
	Revision   int64         `json:"revision"`
	Parameters []ParamResult `json:"parameters"`
}

// CheckRequest asks PostgreSQL whether a value is valid.
type CheckRequest struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// CheckResponse is PostgreSQL's verdict: Error is empty when the value is valid.
type CheckResponse struct {
	Text  string `json:"text"`
	Error string `json:"error,omitempty"`
}

// RestartRequest restarts one local instance.
type RestartRequest struct {
	Instance int `json:"instance"`
}

// TimeResponse is the answer of the clock probe: the wall clock is read as late as possible.
type TimeResponse struct {
	Now        time.Time `json:"now"`
	OSTimeZone string    `json:"osTimeZone"`
	TimeZone   string    `json:"timeZone"`
	LogTZ      string    `json:"logTimeZone"`
}

const settingsSQL = `SELECT name, category, short_desc, vartype, coalesce(unit, ''), coalesce(min_val, ''), coalesce(max_val, ''),
	coalesce(enumvals, '{}'::text[]), context, coalesce(boot_val, ''), coalesce(setting, ''), pending_restart, source, coalesce(sourcefile, '')
	FROM pg_catalog.pg_settings ORDER BY name`

func (a *Agent) mgr() *instance.Manager { return a.Svc.Manager() }

func (a *Agent) log() *slog.Logger {
	if a.Log == nil {
		return slog.New(slog.DiscardHandler)
	}
	return a.Log
}

// conn opens a service connection to a local instance.
func (a *Agent) conn(ctx context.Context, id int) (*pgx.Conn, error) {
	mk, err := a.mgr().Managed(id)
	if err != nil {
		return nil, err
	}
	return a.Svc.ServiceConnAt(ctx, id, mk.Port, "postgres")
}

func roleOf(id int) string {
	if instance.IsStandbyDirID(id) {
		return "standby"
	}
	return "primary"
}

// instanceIDs returns the local instances, the primary first.
func (a *Agent) instanceIDs() []int {
	ids := a.mgr().IDs()
	sort.Ints(ids)
	return ids
}

// readSettings reads pg_settings and, for the override file, what pg_file_settings says about it
// (V2: the view parses the files when it is queried, so it shows a file that was never loaded).
func readSettings(ctx context.Context, c *pgx.Conn, configFile string) ([]Setting, error) {
	rows, err := c.Query(ctx, settingsSQL)
	if err != nil {
		return nil, err
	}
	var out []Setting
	for rows.Next() {
		var s Setting
		if err := rows.Scan(&s.Name, &s.Category, &s.ShortDesc, &s.VarType, &s.Unit, &s.MinVal, &s.MaxVal, &s.EnumVals, &s.Context, &s.BootVal,
			&s.Setting, &s.PendingRestart, &s.Source, &s.SourceFile); err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	fs, err := fileSettings(ctx, c, configFile)
	if err != nil {
		return nil, err
	}
	for i := range out {
		if f, ok := fs[strings.ToLower(out[i].Name)]; ok {
			switch {
			case f.err == couldNotApply && out[i].Context == "postmaster":
				out[i].PendingValue = f.setting // a restart will bring it
			case f.err == couldNotApply:
				out[i].Error = "PostgreSQL did not accept the value"
			case f.err != "":
				out[i].Error = f.err
			}
		}
	}
	return out, nil
}

type fileRow struct{ setting, err string }

// fileSettings returns, for each parameter of the file, its last row.
func fileSettings(ctx context.Context, c *pgx.Conn, file string) (map[string]fileRow, error) {
	rows, err := c.Query(ctx, `SELECT coalesce(lower(name), ''), coalesce(setting, ''), coalesce(error, '') FROM pg_catalog.pg_file_settings
		WHERE sourcefile = $1 ORDER BY seqno`, file)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]fileRow{}
	for rows.Next() {
		var n string
		var r fileRow
		if err := rows.Scan(&n, &r.setting, &r.err); err != nil {
			return nil, err
		}
		if n == "" && r.err != "" { // a syntax error has no name
			n = "(syntax)"
		}
		out[n] = r
	}
	return out, rows.Err()
}

// hidden returns the parameters PostgreSQL hides from a role without pg_read_all_settings (V9).
func hidden(ctx context.Context, c *pgx.Conn, all []Setting) ([]string, error) {
	tx, err := c.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if _, err := tx.Exec(ctx, "SET LOCAL ROLE pg_database_owner"); err != nil {
		return nil, err
	}
	rows, err := tx.Query(ctx, "SELECT name FROM pg_catalog.pg_settings")
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			rows.Close()
			return nil, err
		}
		seen[n] = true
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	var out []string
	for _, s := range all {
		if !seen[s.Name] {
			out = append(out, s.Name)
		}
	}
	return out, nil
}

var fiveSettings = []string{"max_connections", "max_worker_processes", "max_wal_senders", "max_prepared_transactions", "max_locks_per_transaction"}

// State reads the state of this server's instances.
func (a *Agent) State(ctx context.Context, req StateRequest) (StateResponse, error) {
	resp := StateResponse{Server: a.Self, OSTimeZone: OSTimeZone()}
	for _, id := range a.instanceIDs() {
		mk, err := a.mgr().Managed(id)
		if err != nil {
			continue
		}
		info := InstanceInfo{ID: id, Role: roleOf(id), Port: mk.Port, Token: a.mgr().PostmasterToken(id)}
		up, _ := a.mgr().Running(ctx, id)
		info.Running = up
		if up {
			cfg, _ := a.mgr().ConfigPath(id)
			c, err := a.conn(ctx, id)
			if err == nil {
				set, serr := readSettings(ctx, c, cfg)
				if serr == nil {
					info.Values = map[string]string{}
					for _, s := range set {
						if s.PendingRestart {
							info.Pending = append(info.Pending, s.Name)
						}
						for _, f := range fiveSettings {
							if s.Name == f {
								info.Values[f] = s.Setting
								if s.PendingValue != "" {
									info.Values[f+"/pending"] = s.PendingValue
								}
							}
						}
					}
					if id == cluster.NodeID {
						resp.Reachable = true
						if !req.Meta {
							for i := range set {
								set[i].Category, set[i].ShortDesc, set[i].MinVal, set[i].MaxVal, set[i].EnumVals = "", "", "", "", nil
								set[i].BootVal, set[i].Unit, set[i].VarType, set[i].Context = "", "", "", ""
							}
						}
						resp.Settings = set
						_ = c.QueryRow(ctx, "SHOW server_version").Scan(&resp.Version)
						if req.Meta {
							h, herr := hidden(ctx, c, set)
							if herr != nil {
								a.log().Warn("reading the hidden parameters failed", "error", herr)
							}
							resp.Hidden = h
						}
					}
				}
				_ = c.Close(context.WithoutCancel(ctx))
			}
		}
		resp.Instances = append(resp.Instances, info)
	}
	if t, err := a.mgr().ReadConfig(cluster.NodeID); err == nil {
		resp.Revision = instance.ConfigRevision(t)
		resp.ConfigHash = HashOf(t)
	}
	return resp, nil
}

var fileNameRE = regexp.MustCompile(`(?m)^([a-z][a-z0-9_.]*) = `)

func namesOf(text string) []string {
	var out []string
	for _, m := range fileNameRE.FindAllStringSubmatch(text, -1) {
		out = append(out, m[1])
	}
	return out
}

// sameValue says whether the running value (pg_settings.setting) is the value that was written.
func sameValue(s Setting, wanted, running string) bool {
	switch s.VarType {
	case "integer", "real":
		r, err := Check(s, wanted)
		if err != nil || !r.BaseOK {
			return false
		}
		run, ok := ratOf(running)
		return ok && r.Base.Cmp(run) == 0
	case "bool":
		r, err := Check(s, wanted)
		return err == nil && r.Text == running
	}
	return strings.EqualFold(wanted, running)
}

// Apply writes the override file of every instance of this server, checks it before loading it
// (V2), reloads, reads back and sets the database values. It never leaves a file that PostgreSQL
// rejects: the previous one is put back.
func (a *Agent) Apply(ctx context.Context, req ApplyRequest) (ApplyResponse, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(req.Values) > 1000 {
		return ApplyResponse{}, errors.New("too many parameters")
	}
	resp := ApplyResponse{State: StateApplied, Revision: req.Revision}
	results := map[string]*ParamResult{}
	worse := func(name, state, value, pending, errText string) {
		r := results[name]
		if r == nil {
			r = &ParamResult{Name: name, State: StateApplied}
			results[name] = r
		}
		rank := map[string]int{StateApplied: 0, StatePendingRestart: 1, StateWaiting: 2, StateFailed: 3}
		if rank[state] >= rank[r.State] {
			if state != r.State || errText != "" {
				r.State = state
			}
			if value != "" {
				r.Value = value
			}
			if pending != "" {
				r.PendingValue = pending
			}
			if errText != "" {
				r.Error = errText
			}
		}
	}
	waiting := ""
	var firstMeta map[string]Setting
	var hiddenSet map[string]bool
	for _, id := range a.instanceIDs() {
		if _, err := a.mgr().EnsureConfig(id); err != nil {
			if instance.IsStandbyDirID(id) {
				// A standby that is still being created has no data directory yet: it gets the file once it exists
				// (the leader brings servers up to date, see Run).
				waiting = fmt.Sprintf("standby instance %d is not ready yet", id)
				continue
			}
			return resp, fmt.Errorf("instance %d: %w", id, err)
		}
		oldText, _ := a.mgr().ReadConfig(id)
		text, err := instance.RenderConfig(req.Revision, req.Values)
		if err != nil {
			return resp, err
		}
		up, _ := a.mgr().Running(ctx, id)
		if !up {
			if err := a.mgr().WriteConfig(id, text); err != nil {
				return resp, err
			}
			waiting = fmt.Sprintf("instance %d is not running; it reads the file when it starts", id)
			continue
		}
		cfg, _ := a.mgr().ConfigPath(id)
		c, err := a.conn(ctx, id)
		if err != nil {
			if instance.IsStandbyDirID(id) {
				// A standby that is starting or recovering does not accept connections yet: it has the
				// file and reads it at the next reload; it is reported as waiting, not as failed.
				if err := a.mgr().WriteConfig(id, text); err != nil {
					return resp, err
				}
				waiting = fmt.Sprintf("standby instance %d is starting", id)
				continue
			}
			return resp, fmt.Errorf("connecting to instance %d: %w", id, err)
		}
		err = func() error {
			defer func() { _ = c.Close(context.WithoutCancel(ctx)) }()
			set, err := readSettings(ctx, c, cfg)
			if err != nil {
				return err
			}
			meta := map[string]Setting{}
			for _, s := range set {
				meta[strings.ToLower(s.Name)] = s
			}
			if firstMeta == nil {
				firstMeta = meta
				h, _ := hidden(ctx, c, set)
				hiddenSet = map[string]bool{}
				for _, n := range h {
					hiddenSet[strings.ToLower(n)] = true
				}
			}
			// Re-validate against this instance's own metadata and the lists (defence in depth: the
			// leader checked already; a server on another build, or a compromised leader, is stopped here).
			for name, v := range req.Values {
				s, ok := meta[name]
				if !ok {
					// A parameter of an extension this instance does not load: the file would be an error.
					worse(name, StateFailed, "", "", "PostgreSQL on this server has no parameter "+name)
					continue
				}
				if cl := Classify(s, hiddenSet[name]); cl.Kind != KindSettable {
					return fmt.Errorf("%s cannot be set from the console", name)
				}
				if _, err := Check(s, v); err != nil {
					worse(name, StateFailed, "", "", err.Error())
				}
				if f, why, ok := Floor(name, req.Replicas); ok {
					if r, err := Check(s, v); err == nil && r.BaseOK {
						if min, _ := ratOf(fmt.Sprint(f)); r.Base.Cmp(min) < 0 {
							worse(name, StateFailed, "", "", "below what kingresd needs: "+why)
						}
					}
				}
			}
			for _, r := range results {
				if r.State == StateFailed {
					return nil // nothing is written when a value is invalid here
				}
			}
			// A file that is already the wanted one still needs a reload when the instance runs something else
			// (an earlier apply wrote it but could not reload, for example a standby that was starting).
			drift := false
			for name, v := range req.Values {
				if m, ok := meta[name]; ok && !m.PendingRestart && m.Error == "" && m.PendingValue == "" && !sameValue(m, v, m.Setting) {
					drift = true
				}
			}
			if text != oldText || drift {
				if text != oldText {
					if err := a.mgr().WriteConfig(id, text); err != nil {
						return err
					}
				}
				if bad := fileProblems(ctx, c, cfg, meta); len(bad) > 0 {
					if err := a.mgr().RestoreConfig(id); err != nil {
						a.log().Error("restoring the override file failed", "instance", id, "error", err)
					}
					for n, e := range bad {
						worse(n, StateFailed, "", "", e)
					}
					return nil
				}
				var t0 time.Time
				_ = c.QueryRow(ctx, "SELECT coalesce(pg_catalog.pg_conf_load_time(), '-infinity')").Scan(&t0)
				if err := a.mgr().Reload(ctx, id); err != nil {
					return err
				}
				if err := waitReload(ctx, c, t0); err != nil {
					return err
				}
			}
			// Read back every parameter that is or was in the file.
			names := map[string]bool{}
			for n := range req.Values {
				names[n] = true
			}
			for _, n := range namesOf(oldText) {
				names[n] = true
			}
			after, err := readSettings(ctx, c, cfg)
			if err != nil {
				return err
			}
			for _, s := range after {
				ln := strings.ToLower(s.Name)
				if !names[ln] {
					continue
				}
				want, wanted := req.Values[ln]
				shown := Format(s, s.Setting)
				switch {
				case s.Error != "":
					worse(ln, StateFailed, shown, "", s.Error)
				case s.PendingRestart:
					worse(ln, StatePendingRestart, shown, s.PendingValue, "")
				case wanted && !sameValue(s, want, s.Setting):
					worse(ln, StateFailed, shown, "", fmt.Sprintf("PostgreSQL did not take the value (it runs %s)", shown))
				default:
					worse(ln, StateApplied, shown, "", "")
				}
			}
			return nil
		}()
		if err != nil {
			return resp, fmt.Errorf("instance %d: %w", id, err)
		}
	}
	// Database-level values live in the catalog of each primary instance (standbys replay them, V10).
	dbErrs := a.applyDatabases(ctx, req, firstMeta)
	for n, e := range dbErrs {
		worse(n, StateFailed, "", "", e)
	}
	for _, r := range results {
		resp.Parameters = append(resp.Parameters, *r)
		switch r.State {
		case StateFailed:
			resp.State = StateFailed
		case StatePendingRestart:
			if resp.State != StateFailed {
				resp.State = StatePendingRestart
			}
		}
	}
	sort.Slice(resp.Parameters, func(i, j int) bool { return resp.Parameters[i].Name < resp.Parameters[j].Name })
	if resp.State != StateFailed && waiting != "" {
		resp.State, resp.Detail = StateWaiting, waiting
	}
	return resp, nil
}

// couldNotApply is what pg_file_settings says about a line it did not apply: for a parameter of
// context postmaster it only means that a restart is needed, for any other it means PostgreSQL
// refused the value (checked on the project build: an invalid value gives this text too).
const couldNotApply = "setting could not be applied"

// fileProblems returns PostgreSQL's complaints about the override file: any error except the one
// that marks a postmaster parameter as waiting for a restart. meta maps lower-case names to settings.
func fileProblems(ctx context.Context, c *pgx.Conn, file string, meta map[string]Setting) map[string]string {
	rows, err := fileSettings(ctx, c, file)
	if err != nil {
		return map[string]string{"(file)": "the file could not be checked: " + err.Error()}
	}
	out := map[string]string{}
	for n, r := range rows {
		if r.err == "" {
			continue
		}
		if r.err == couldNotApply && meta[n].Context == "postmaster" {
			continue
		}
		out[n] = r.err
	}
	return out
}

func waitReload(ctx context.Context, c *pgx.Conn, before time.Time) error {
	deadline := time.Now().Add(15 * time.Second)
	for {
		var now time.Time
		if err := c.QueryRow(ctx, "SELECT coalesce(pg_catalog.pg_conf_load_time(), '-infinity')").Scan(&now); err != nil {
			return err
		}
		if now.After(before) {
			return nil
		}
		if time.Now().After(deadline) {
			return errors.New("PostgreSQL did not reload its configuration within 15 seconds")
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// applyDatabases sets and resets the database-level values on the primary instance. It returns the
// parameters that failed with PostgreSQL's text. A database that does not exist here is skipped.
func (a *Agent) applyDatabases(ctx context.Context, req ApplyRequest, meta map[string]Setting) map[string]string {
	errs := map[string]string{}
	if len(req.Databases) == 0 && len(req.Removed) == 0 {
		return errs
	}
	up, _ := a.mgr().Running(ctx, cluster.NodeID)
	if !up {
		return errs
	}
	c, err := a.conn(ctx, cluster.NodeID)
	if err != nil {
		errs["(database)"] = err.Error()
		return errs
	}
	defer func() { _ = c.Close(context.WithoutCancel(ctx)) }()
	exists := func(db string) bool {
		var ok bool
		_ = c.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM pg_catalog.pg_database WHERE datname = $1)", db).Scan(&ok)
		return ok
	}
	// The statement is built by PostgreSQL's own format(): %I quotes the database and the parameter as
	// identifiers and %L the value as a literal, so no text of the request is ever part of the SQL.
	run := func(format, db, name string, value ...string) error {
		var stmt string
		args := append([]any{db, name}, anySlice(value)...)
		if err := c.QueryRow(ctx, "SELECT pg_catalog.format("+format+", "+placeholders(len(args))+")", args...).Scan(&stmt); err != nil {
			return err
		}
		_, err := c.Exec(ctx, stmt)
		return err
	}
	for db, values := range req.Databases {
		if !exists(db) {
			continue
		}
		for name, v := range values {
			if !configNameOK(name) {
				errs[name] = "the name cannot be set"
				continue
			}
			if s, ok := meta[name]; ok {
				if cl := Classify(s, false); !cl.Allows(LevelDatabase) {
					errs[name] = "cannot be set for a database"
					continue
				}
			}
			if err := run(`'ALTER DATABASE %I SET %I TO %L'`, db, name, v); err != nil {
				errs[name] = pgText(err)
			}
		}
	}
	for db, names := range req.Removed {
		if !exists(db) {
			continue
		}
		for _, name := range names {
			if !configNameOK(name) {
				continue
			}
			if err := run(`'ALTER DATABASE %I RESET %I'`, db, name); err != nil {
				errs[name] = pgText(err)
			}
		}
	}
	return errs
}

func configNameOK(n string) bool {
	_, err := instance.RenderConfig(0, map[string]string{n: ""})
	return err == nil
}

func placeholders(n int) string {
	p := make([]string, n)
	for i := range p {
		p[i] = fmt.Sprintf("$%d::text", i+1)
	}
	return strings.Join(p, ", ")
}

func anySlice(s []string) []any {
	out := make([]any, len(s))
	for i, v := range s {
		out[i] = v
	}
	return out
}

// pgText returns PostgreSQL's own message of an error (the part that names the problem).
func pgText(err error) string {
	var pe *pgconn.PgError
	if errors.As(err, &pe) {
		return pe.Message
	}
	return err.Error()
}

// Check asks PostgreSQL whether a value is valid for a parameter (design config-phase13.md,
// "Validation", layers 2 and 3).
func (a *Agent) Check(ctx context.Context, req CheckRequest) (CheckResponse, error) {
	c, err := a.conn(ctx, cluster.NodeID)
	if err != nil {
		return CheckResponse{}, err
	}
	defer func() { _ = c.Close(context.WithoutCancel(ctx)) }()
	set, err := readSettings(ctx, c, "")
	if err != nil {
		return CheckResponse{}, err
	}
	var meta Setting
	found := false
	for _, s := range set {
		if strings.EqualFold(s.Name, req.Name) {
			meta, found = s, true
		}
	}
	if !found {
		return CheckResponse{Error: "PostgreSQL has no parameter " + req.Name}, nil
	}
	r, err := Check(meta, req.Value)
	if err != nil {
		return CheckResponse{Error: strings.TrimPrefix(err.Error(), "invalid value: ")}, nil
	}
	switch {
	case meta.Context == "user" || meta.Context == "superuser":
		tx, err := c.Begin(ctx)
		if err != nil {
			return CheckResponse{}, err
		}
		defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
		var got string
		if err := tx.QueryRow(ctx, "SELECT pg_catalog.set_config($1, $2, true)", meta.Name, r.Text).Scan(&got); err != nil {
			return CheckResponse{Error: pgText(err)}, nil
		}
	case !strings.Contains(meta.Name, "."):
		if err := a.mgr().CheckValue(ctx, cluster.NodeID, strings.ToLower(meta.Name), r.Text); err != nil {
			return CheckResponse{Error: strings.TrimPrefix(err.Error(), "invalid instance input: ")}, nil
		}
	}
	return CheckResponse{Text: r.Text}, nil
}

// RestartInstance restarts one local instance and waits until it accepts connections (and a standby
// streams again). It returns when the instance is back or after at most two minutes.
func (a *Agent) RestartInstance(ctx context.Context, id int) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if _, err := a.mgr().Managed(id); err != nil {
		return err
	}
	if err := a.mgr().Restart(ctx, id); err != nil {
		return err
	}
	deadline := time.Now().Add(2 * time.Minute)
	for {
		c, err := a.conn(ctx, id)
		if err == nil {
			ok := true
			if instance.IsStandbyDirID(id) {
				var n int
				_ = c.QueryRow(ctx, "SELECT count(*) FROM pg_catalog.pg_stat_wal_receiver WHERE status = 'streaming'").Scan(&n)
				ok = n > 0
			}
			_ = c.Close(context.WithoutCancel(ctx))
			if ok {
				return nil
			}
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("instance %d did not come back (or did not stream) within two minutes", id)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
}

// Time answers the clock probe. The zones are cached for a few seconds so that the probe is not
// slowed by a query; the clock is read after them.
func (a *Agent) Time(ctx context.Context) TimeResponse {
	a.zmu.Lock()
	stale := time.Since(a.zones.at) > 5*time.Second
	first := a.zones.at.IsZero()
	a.zmu.Unlock()
	if first {
		a.refreshZones(ctx)
	} else if stale {
		go a.refreshZones(context.WithoutCancel(ctx))
	}
	a.zmu.Lock()
	tz, ltz := a.zones.tz, a.zones.logTZ
	a.zmu.Unlock()
	return TimeResponse{Now: a.now().UTC(), OSTimeZone: OSTimeZone(), TimeZone: tz, LogTZ: ltz}
}

func (a *Agent) refreshZones(ctx context.Context) {
	a.zmu.Lock()
	if a.zones.refreshing {
		a.zmu.Unlock()
		return
	}
	a.zones.refreshing = true
	a.zmu.Unlock()
	var tz, ltz string
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if c, err := a.conn(ctx, cluster.NodeID); err == nil {
		_ = c.QueryRow(ctx, "SELECT current_setting('TimeZone'), current_setting('log_timezone')").Scan(&tz, &ltz)
		_ = c.Close(context.WithoutCancel(ctx))
	}
	a.zmu.Lock()
	a.zones.at, a.zones.tz, a.zones.logTZ, a.zones.refreshing = time.Now(), tz, ltz, false
	a.zmu.Unlock()
}

// OSTimeZone reads the operating system's time zone name: TZ, else the target of /etc/localtime,
// else /etc/timezone; "" when none can be read.
func OSTimeZone() string {
	if tz := os.Getenv("TZ"); tz != "" {
		return strings.TrimPrefix(tz, ":")
	}
	if l, err := os.Readlink("/etc/localtime"); err == nil {
		l = filepath.ToSlash(l)
		if i := strings.Index(l, "zoneinfo/"); i >= 0 {
			return l[i+len("zoneinfo/"):]
		}
	}
	if b, err := os.ReadFile("/etc/timezone"); err == nil {
		return strings.TrimSpace(strings.SplitN(string(b), "\n", 2)[0])
	}
	return ""
}

// HashOf hashes the settings of an override file, ignoring comments (the header carries the revision).
func HashOf(text string) string {
	h := sha256.New()
	for _, l := range strings.Split(text, "\n") {
		if l == "" || strings.HasPrefix(l, "#") {
			continue
		}
		h.Write([]byte(l + "\n"))
	}
	return hex.EncodeToString(h.Sum(nil))
}

func (a *Agent) now() time.Time {
	if a.Now != nil {
		return a.Now()
	}
	return time.Now()
}
