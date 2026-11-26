// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package ops

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/extguard"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/internalapi"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/peerclient"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/problem"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/sqlx"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/task"
)

// The analytics extensions (phase 12): kingres_sketch (approximate aggregates, needs no preload), kingres_columnar (the columnar table access method,
// its library must be preloaded because it registers a WAL resource manager) and kingres_timepart (time partitioning, PL/pgSQL, needs no preload).
const (
	ExtSketch   = "kingres_sketch"
	ExtColumnar = "kingres_columnar"
	ExtTimepart = "kingres_timepart"
	// ExtStatStatements is pg_stat_statements of PostgreSQL (contrib): its library is preloaded by every instance (instance.PreloadLibraries), and every database
	// that kingresd makes carries the extension.
	ExtStatStatements = "pg_stat_statements"
)

// analyticsExtensions is the closed set of extensions the operations of this file install and check on other servers.
var analyticsExtensions = map[string]bool{ExtSketch: true, ExtColumnar: true, ExtTimepart: true, ExtStatStatements: true}

// Kinds of ApplyLocal of the extensions: each acts on one database of the server that receives it, with parameters that are structured values.
const (
	// ApplyExtensionCheck: the extension is available in the package of the server (and, with Version, at that default version); with Preload
	// the library is also loaded.
	ApplyExtensionCheck = "extension.check"
	// ApplyExtensionInstall: CREATE EXTENSION IF NOT EXISTS, or ALTER EXTENSION UPDATE when it is installed at another version.
	ApplyExtensionInstall = "extension.install"
	// ApplyExtensionVerify: the installed version equals Version and, for kingres_sketch, self_test() equals Hash.
	ApplyExtensionVerify = "extension.verify"
	// ApplyDatabaseSettings: ALTER DATABASE ... SET (or RESET, for an empty value) of the settings of the closed list.
	ApplyDatabaseSettings = "database.settings"
)

// extensionOp is the parameter of the extension kinds.
type extensionOp struct {
	Database  string `json:"database"`
	Extension string `json:"extension"`
	Version   string `json:"version,omitempty"`
	Hash      string `json:"hash,omitempty"`
	Preload   bool   `json:"preload,omitempty"`
}

// settingsOp is the parameter of ApplyDatabaseSettings.
type settingsOp struct {
	Database string            `json:"database"`
	Settings map[string]string `json:"settings"`
}

// databaseSettingNames is the closed list of the settings ApplyDatabaseSettings sets, with the check of each value.
var databaseSettingNames = map[string]func(string) bool{
	"kingres_shard.count_distinct_mode":       func(v string) bool { return v == "exact" || v == "approximate" },
	"kingres_shard.count_distinct_error_rate": func(v string) bool { return rateRE.MatchString(v) },
	"kingres_shard.enable_sketch_pushdown":    func(v string) bool { return v == "on" || v == "off" },
}

// extError is an error of an extension step: its code is a problem code that the task records as its own.
type extError struct {
	code problem.Code
	msg  string
	// retry says that the same request may succeed later (a lock that is held).
	retry bool
}

func (e *extError) Error() string       { return e.msg }
func (e *extError) TaskCode() string    { return string(e.code) }
func (e *extError) TaskRetryable() bool { return e.retry }

func extErr(code problem.Code, format string, a ...any) *extError {
	return &extError{code: code, msg: fmt.Sprintf(format, a...)}
}

// callError makes the error travel over the internal API with its code.
func (e *extError) callError() *internalapi.CallError {
	status := http.StatusConflict
	if e.code == problem.ExtensionUnavailable {
		status = http.StatusUnprocessableEntity
	}
	return &internalapi.CallError{Status: status, Code: string(e.code), Message: e.msg}
}

// extensionCodes are the codes that cross the internal API.
var extensionCodes = map[string]bool{
	string(problem.ExtensionUnavailable): true, string(problem.ExtensionVersionMismatch): true,
	string(problem.SketchHashMismatch): true, string(problem.ExtensionMissing): true,
}

// asExtError turns the answer of another server (or of this one) into an extError when it carries one of the codes, and returns any other error as it is.
func asExtError(server string, err error) error {
	var ee *extError
	if errors.As(err, &ee) {
		return extErr(ee.code, "server %s: %s", server, ee.msg)
	}
	var ae *peerclient.APIError
	if errors.As(err, &ae) && extensionCodes[ae.Code] {
		return extErr(problem.Code(ae.Code), "server %s: %s", server, ae.Message)
	}
	return err
}

// extBackend is what the extension steps need of the local server's PostgreSQL; the production one runs on a service connection, a test supplies its own.
type extBackend interface {
	// Available returns the default version of the extension in the package of this server, "" when the package lacks it. It fails when the database
	// cannot be opened.
	Available(ctx context.Context, db, ext string) (string, error)
	// Installed returns the version of the extension installed in the database, "" when it is not.
	Installed(ctx context.Context, db, ext string) (string, error)
	// Install creates the extension, or updates it to the default version.
	Install(ctx context.Context, db, ext string) error
	// SelfTest returns kingres_sketch.self_test().
	SelfTest(ctx context.Context, db string) (string, error)
	// Preloaded says whether the library is in shared_preload_libraries of the instance of this server.
	Preloaded(ctx context.Context, lib string) (bool, error)
	// PreloadedOnAll returns the instances of this server, standbys included, that are running and have not loaded the library, as text that names them. An instance that is
	// not running loads it when it starts.
	PreloadedOnAll(ctx context.Context, lib string) ([]string, error)
	// SetDatabaseSettings sets the settings that the server knows (an empty value resets one) and returns the names it did not know.
	SetDatabaseSettings(ctx context.Context, db string, settings map[string]string) (unknown []string, err error)
}

func (o *Ops) ext() extBackend {
	if o.extBackendOverride != nil {
		return o.extBackendOverride
	}
	return pgExt{o: o}
}

type pgExt struct{ o *Ops }

func (p pgExt) conn(ctx context.Context, db string) (*pgx.Conn, error) {
	if p.o.Service == nil {
		return nil, problem.New(problem.UnsupportedOp, "This server has no managed instance.")
	}
	return p.o.Service.ServiceConn(ctx, db)
}

func (p pgExt) Available(ctx context.Context, db, ext string) (string, error) {
	c, err := p.conn(ctx, db)
	if err != nil {
		return "", err
	}
	defer func() { _ = c.Close(context.WithoutCancel(ctx)) }()
	var v string
	err = c.QueryRow(ctx, "SELECT default_version FROM pg_catalog.pg_available_extensions WHERE name = $1", ext).Scan(&v)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	return v, err
}

func (p pgExt) Installed(ctx context.Context, db, ext string) (string, error) {
	c, err := p.conn(ctx, db)
	if err != nil {
		return "", err
	}
	defer func() { _ = c.Close(context.WithoutCancel(ctx)) }()
	if !extguard.IsKingres(ext) { // pg_stat_statements of PostgreSQL: it lives in the schema public, which the control plane never reads (see Install)
		var v string
		err = c.QueryRow(ctx, "SELECT extversion FROM pg_catalog.pg_extension WHERE extname = $1", ext).Scan(&v)
		if errors.Is(err, pgx.ErrNoRows) {
			return "", nil
		}
		return v, err
	}
	// Found by extguard, not by name alone: an extension whose schema cannot be trusted is refused, never reported as installed or absent.
	info, err := extguard.Verify(ctx, c, ext)
	if err != nil {
		return "", extUntrusted(err)
	}
	return info.Version, nil
}

// extUntrusted turns the refusal of extguard into the error of the extension operations (the text crosses servers); another error is returned as it is.
func extUntrusted(err error) error {
	if u, ok := extguard.IsUntrusted(err); ok {
		return extErr(problem.ExtensionUnavailable, "the schema %s of %s in this database cannot be trusted (%s): it is not owned by a superuser, a role that is not a superuser can create objects in it, or it holds an object that is not a member of an extension; an operator must restore it", u.Schema, u.Extension, u.Reason)
	}
	return err
}

func (p pgExt) Install(ctx context.Context, db, ext string) error {
	c, err := p.conn(ctx, db)
	if err != nil {
		return err
	}
	defer func() { _ = c.Close(context.WithoutCancel(ctx)) }()
	var def string
	var info extguard.Info
	if extguard.IsKingres(ext) {
		// An extension of KingresDB that is there must be trusted before it is updated; one that is not there is created only when the schema it goes into is not
		// one that a tenant made (extguard.PreInstall): the control file of an extension with "schema = ..." would use it, and the tenant would own what the service
		// role runs.
		info, err = extguard.Verify(ctx, c, ext)
	} else {
		// pg_stat_statements of PostgreSQL (contrib): its schema is public, which a tenant owns, and the control plane never reads anything in it, so extguard
		// (about the schema of the KingresDB extensions) does not apply.
		info.Name = ext
		err = c.QueryRow(ctx, "SELECT extversion FROM pg_catalog.pg_extension WHERE extname = $1", ext).Scan(&info.Version)
		if errors.Is(err, pgx.ErrNoRows) {
			err = nil
		} else if err == nil {
			info.Present = true
		}
	}
	if err != nil {
		return extUntrusted(err)
	}
	cur := info.Version
	if !info.Present {
		if err := extguard.PreInstallExtension(ctx, c, ext); err != nil {
			return extUntrusted(err)
		}
		b := stmt().Text("CREATE EXTENSION IF NOT EXISTS").ID(sqlx.MustIdent(ext))
		if ext == ExtStatStatements {
			b.Text("SCHEMA public") // the service session runs with search_path = pg_catalog: without a schema of its own the extension would be made there, and refused
		}
		return run(ctx, c, b)
	}
	if err := c.QueryRow(ctx, "SELECT default_version FROM pg_catalog.pg_available_extensions WHERE name = $1", ext).Scan(&def); err != nil {
		return err
	}
	if cur == def {
		return nil
	}
	return run(ctx, c, stmt().Text("ALTER EXTENSION").ID(sqlx.MustIdent(ext)).Text("UPDATE"))
}

func (p pgExt) SelfTest(ctx context.Context, db string) (string, error) {
	c, err := p.conn(ctx, db)
	if err != nil {
		return "", err
	}
	defer func() { _ = c.Close(context.WithoutCancel(ctx)) }()
	// self_test runs as the service role: the extension and its schema are checked first (extguard), so the function is the extension's own.
	info, err := extguard.Verify(ctx, c, ExtSketch)
	if err != nil {
		return "", extUntrusted(err)
	}
	if !info.Present {
		return "", extErr(problem.ExtensionUnavailable, "%s is not installed in the database", ExtSketch)
	}
	var s string
	err = c.QueryRow(ctx, "SELECT kingres_sketch.self_test()").Scan(&s)
	return s, err
}

func (p pgExt) Preloaded(ctx context.Context, lib string) (bool, error) {
	c, err := p.conn(ctx, "postgres")
	if err != nil {
		return false, err
	}
	defer func() { _ = c.Close(context.WithoutCancel(ctx)) }()
	var s string
	if err := c.QueryRow(ctx, "SELECT current_setting('shared_preload_libraries')").Scan(&s); err != nil {
		return false, err
	}
	return libraryListed(s, lib), nil
}

func (p pgExt) PreloadedOnAll(ctx context.Context, lib string) ([]string, error) {
	if p.o.Local == nil {
		ok, err := p.Preloaded(ctx, lib)
		if err != nil || ok {
			return nil, err
		}
		return []string{"the instance of this server"}, nil
	}
	var missing []string
	for _, li := range p.o.Local.LocalInstances() {
		c, err := p.o.Local.InstanceConn(ctx, li.ID, "postgres")
		if err != nil {
			continue // not running (or not reachable now): it loads the list when it starts
		}
		var s string
		err = c.QueryRow(ctx, "SELECT current_setting('shared_preload_libraries')").Scan(&s)
		_ = c.Close(context.WithoutCancel(ctx))
		if err != nil {
			continue
		}
		if !libraryListed(s, lib) {
			missing = append(missing, fmt.Sprintf("instance %d (%s)", li.ID, li.Role.Role))
		}
	}
	return missing, nil
}

// libraryListed reads a shared_preload_libraries value: names separated by commas, with optional blanks and double quotes.
func libraryListed(list, lib string) bool {
	for _, n := range strings.Split(list, ",") {
		n = strings.Trim(strings.TrimSpace(n), `"`)
		n = strings.TrimSuffix(n, ".so")
		if n == lib {
			return true
		}
	}
	return false
}

func (p pgExt) SetDatabaseSettings(ctx context.Context, db string, settings map[string]string) ([]string, error) {
	c, err := p.conn(ctx, db)
	if err != nil {
		return nil, err
	}
	defer func() { _ = c.Close(context.WithoutCancel(ctx)) }()
	names := make([]string, 0, len(settings))
	for n := range settings {
		names = append(names, n)
	}
	sort.Strings(names)
	var unknown []string
	for _, n := range names {
		var have bool
		if err := c.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM pg_catalog.pg_settings WHERE name = $1)", n).Scan(&have); err != nil {
			return unknown, err
		}
		if !have {
			unknown = append(unknown, n)
			continue
		}
		b := stmt().Text("ALTER DATABASE").ID(sqlx.MustIdent(db))
		if v := settings[n]; v == "" {
			b.Text(sqlx.Fixed("RESET " + n))
		} else {
			b.Text(sqlx.Fixed("SET " + n + " =")).Lit(v)
		}
		if err := run(ctx, c, b); err != nil {
			return unknown, err
		}
	}
	return unknown, nil
}

// ---------------------------------------------------------------- the local side of the kinds

// extCheckLocal is ApplyExtensionCheck on this server.
func (o *Ops) extCheckLocal(ctx context.Context, p extensionOp) error {
	if !analyticsExtensions[p.Extension] {
		return errors.New("this extension cannot be handled")
	}
	b := o.ext()
	v, err := b.Available(ctx, p.Database, p.Extension)
	if err != nil {
		return extErr(problem.ExtensionUnavailable, "the database %q cannot be opened: %v", p.Database, problemText(err))
	}
	if v == "" {
		return extErr(problem.ExtensionUnavailable, "the package of this server does not contain %s", p.Extension)
	}
	if p.Version != "" && v != p.Version {
		return extErr(problem.ExtensionVersionMismatch, "this server has %s %s, the others have %s", p.Extension, v, p.Version)
	}
	if p.Preload {
		missing, err := b.PreloadedOnAll(ctx, p.Extension)
		if err != nil {
			return err
		}
		if len(missing) > 0 {
			return extErr(problem.ExtensionMissing, "the library %s is not loaded by %s of this server: it was started before the library was added to shared_preload_libraries, so restart it (standbys first)", p.Extension, strings.Join(missing, ", "))
		}
	}
	return nil
}

// extInstallLocal is ApplyExtensionInstall on this server.
func (o *Ops) extInstallLocal(ctx context.Context, p extensionOp) error {
	if !analyticsExtensions[p.Extension] {
		return errors.New("this extension cannot be handled")
	}
	return o.ext().Install(ctx, p.Database, p.Extension)
}

// extVerifyLocal is ApplyExtensionVerify on this server.
func (o *Ops) extVerifyLocal(ctx context.Context, p extensionOp) error {
	if !analyticsExtensions[p.Extension] {
		return errors.New("this extension cannot be handled")
	}
	b := o.ext()
	v, err := b.Installed(ctx, p.Database, p.Extension)
	if err != nil {
		return err
	}
	if v == "" {
		return extErr(problem.ExtensionUnavailable, "%s is not installed in the database", p.Extension)
	}
	if v != p.Version {
		return extErr(problem.ExtensionVersionMismatch, "this server runs %s %s, expected %s", p.Extension, v, p.Version)
	}
	if p.Extension == ExtSketch && p.Hash != "" {
		h, err := b.SelfTest(ctx, p.Database)
		if err != nil {
			return err
		}
		if h != p.Hash {
			return extErr(problem.SketchHashMismatch, "the known-answer values of %s hash differently on this server", p.Extension)
		}
	}
	return nil
}

// settingsLocal is ApplyDatabaseSettings on this server; it returns the names that this server does not know.
func (o *Ops) settingsLocal(ctx context.Context, p settingsOp) ([]string, error) {
	if !dbNameRE.MatchString(p.Database) || protectedDB[p.Database] {
		return nil, errors.New("this database cannot be changed")
	}
	for n, v := range p.Settings {
		ok, known := databaseSettingNames[n]
		if !known || (v != "" && !ok(v)) {
			return nil, errors.New("the setting is not valid")
		}
	}
	return o.ext().SetDatabaseSettings(ctx, p.Database, p.Settings)
}

// applyExtensionKind runs the kinds of this file for ApplyLocal; it reports whether the kind was one of them.
func (o *Ops) applyExtensionKind(ctx context.Context, kind string, raw []byte) (bool, error) {
	switch kind {
	case ApplyExtensionCheck, ApplyExtensionInstall, ApplyExtensionVerify:
		var p extensionOp
		if err := strictJSON(raw, &p); err != nil {
			return true, err
		}
		if !dbNameRE.MatchString(p.Database) || protectedDB[p.Database] && p.Database != "postgres" {
			return true, errors.New("this database cannot be changed")
		}
		var err error
		switch kind {
		case ApplyExtensionCheck:
			err = o.extCheckLocal(ctx, p)
		case ApplyExtensionInstall:
			err = o.extInstallLocal(ctx, p)
		default:
			err = o.extVerifyLocal(ctx, p)
		}
		var ee *extError
		if errors.As(err, &ee) {
			return true, ee.callError()
		}
		return true, err
	case ApplyDatabaseSettings:
		var p settingsOp
		if err := strictJSON(raw, &p); err != nil {
			return true, err
		}
		_, err := o.settingsLocal(ctx, p)
		return true, err
	}
	return false, nil
}

// problemText is the text of an error without driver internals, for a message that goes to the administrator.
func problemText(err error) string {
	var p *problem.Problem
	if errors.As(err, &p) {
		return p.Detail
	}
	return "it cannot be reached"
}

// onEveryServer runs f on this server and on every other server of the list, waiting for a server that cannot be reached; the other servers are
// reached with Fanout.Apply, this one directly. It stops at the first error.
func (o *Ops) onEveryServer(ctx context.Context, env *task.Env, servers []string, doing string, local func() error, remote func(server string) error) error {
	for _, s := range servers {
		if s == o.ServerHost {
			if err := local(); err != nil {
				return asExtError(s, err)
			}
			continue
		}
		err := o.onServer(ctx, env, s, doing, func() error { return remote(s) })
		if err != nil {
			return asExtError(s, err)
		}
	}
	return nil
}
