// SPDX-License-Identifier: AGPL-3.0-only

package cluster

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"

	"path/filepath"
	"regexp"

	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/api"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/instance"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/pki"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/scram"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/setup"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/sqlx"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/sysdb"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/task"
)

// KindInit is the task kind of the cluster initialization.
const KindInit = "cluster.init"

// ClusterRoles are the four built-in cluster roles.
var ClusterRoles = []string{"kingres_admin", "kingres_ops", "kingres_readonly", "kingres_stats"}

// FieldError is a validation failure of a request field.
type FieldError struct{ Field, Message, Code string }

var (
	clusterNameRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,62}$`)
	adminNameRE   = regexp.MustCompile(`^[a-z_][a-z0-9_]{0,62}$`)
)

// ValidateInit checks a request without touching anything. dataDir is the
// configured data directory of kingresd.
func ValidateInit(req api.InitClusterRequest, dataDir string) []FieldError {
	var errs []FieldError
	bad := func(f, m, c string) { errs = append(errs, FieldError{f, m, c}) }
	if !clusterNameRE.MatchString(req.ClusterName) {
		bad("/clusterName", "must match ^[A-Za-z0-9][A-Za-z0-9_-]{0,62}$", "pattern")
	}
	if !adminNameRE.MatchString(req.Admin.Username) {
		bad("/admin/username", "must match ^[a-z_][a-z0-9_]{0,62}$", "pattern")
	} else if strings.HasPrefix(req.Admin.Username, "pg_") || req.Admin.Username == instance.ServiceRole ||
		req.Admin.Username == "postgres" || strings.HasPrefix(req.Admin.Username, "kingres") {
		bad("/admin/username", "this role name is reserved", "reserved")
	}
	if n := utf8.RuneCountInString(req.Admin.Password); n < 12 || len(req.Admin.Password) > 1024 {
		bad("/admin/password", "must be 12 to 1024 characters", "out_of_range")
	} else if strings.ContainsRune(req.Admin.Password, 0) {
		bad("/admin/password", "must not contain NUL", "pattern")
	}
	if req.MemoryQuota.TotalMegabytes < 512 || req.MemoryQuota.TotalMegabytes > 1<<24 {
		bad("/memoryQuota/totalMegabytes", "must be at least 512", "out_of_range")
	}
	if req.DefaultReplicaCount != nil && (*req.DefaultReplicaCount < 0 || *req.DefaultReplicaCount > 3) {
		bad("/defaultReplicaCount", "must be 0 to 3", "out_of_range")
	}
	if d := req.DefaultDurability; d != nil && *d != "async" && *d != "sync" && *d != "syncApply" {
		bad("/defaultDurability", "must be async, sync or syncApply", "enum")
	}
	// The data directory is the one kingresd was started with: instance
	// directories are created below it and the request cannot point the
	// manager somewhere else.
	want, _ := filepath.Abs(dataDir)
	if got := req.DataPaths.DataDirectory; !filepath.IsAbs(got) || filepath.Clean(got) != filepath.Clean(want) {
		bad("/dataPaths/dataDirectory", "must be the data directory of this server ("+want+")", "mismatch")
	}
	if req.DataPaths.WalDirectory != nil {
		bad("/dataPaths/walDirectory", "a separate WAL directory is not supported yet; use null", "unsupported")
	}
	if req.DataPaths.LogDirectory != nil {
		bad("/dataPaths/logDirectory", "a separate log directory is not supported yet; use null", "unsupported")
	}
	return errs
}

type initParams struct {
	ClusterID   string `json:"clusterId"`
	ClusterName string `json:"clusterName"`
	AdminUser   string `json:"adminUser"`
	Sharding    bool   `json:"sharding"`
	Ledger      bool   `json:"ledger"`
	Replicas    int    `json:"defaultReplicaCount"`
	Durability  string `json:"defaultDurability"`
	MemoryMB    int    `json:"memoryMegabytes"`
}

// Init validates nothing (call ValidateInit first), creates the setup task
// and starts it in the background with the administrator password as an
// in-memory secret. It returns the task as stored.
func (s *Service) Init(req api.InitClusterRequest, actor string) (task.Task, error) {
	if !s.Enabled() {
		return task.Task{}, ErrDisabled
	}
	id, err := newUUID()
	if err != nil {
		return task.Task{}, err
	}
	p := initParams{ClusterID: id, ClusterName: req.ClusterName, AdminUser: req.Admin.Username,
		Sharding: true, Ledger: true, Replicas: 1, Durability: "sync", MemoryMB: req.MemoryQuota.TotalMegabytes}
	if f := req.Features; f != nil {
		p.Sharding, p.Ledger = f.Sharding, f.Ledger
	}
	if req.DefaultReplicaCount != nil {
		p.Replicas = *req.DefaultReplicaCount
	}
	if req.DefaultDurability != nil {
		p.Durability = *req.DefaultDurability
	}
	ctx := context.Background()
	t, run, err := s.initEngine.SubmitHeld(ctx, KindInit, task.Subject{Type: "cluster", ID: "cluster"}, p, actor)
	if err != nil {
		return task.Task{}, err
	}
	secrets := map[string]string{"adminPassword": req.Admin.Password}
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		if err := run(s.bg, secrets); err != nil && s.bg.Err() == nil {
			s.log.Error("setup task ended with an error", "task", t.ID, "error", err)
		}
	}()
	return t, nil
}

// SetupTask returns the setup task by id.
func (s *Service) SetupTask(ctx context.Context, id int64) (task.Task, error) {
	if s.initStore == nil {
		return task.Task{}, task.ErrNotFound
	}
	return s.initStore.Get(ctx, id)
}

func paramsOf(t task.Task) (initParams, error) {
	var p initParams
	if err := json.Unmarshal(t.Input, &p); err != nil {
		return p, fmt.Errorf("reading the task input: %w", err)
	}
	return p, nil
}

func (s *Service) initDefinition() task.Definition {
	return task.Definition{Kind: KindInit, Steps: []task.Step{
		{Name: "Create the cluster certificate authority", Run: s.stepPKI},
		{Name: "Create the PostgreSQL instance", Run: s.stepInstance},
		{Name: "Create cluster roles and the administrator", NeedsSecret: "adminPassword", Run: s.stepRoles},
		{Name: "Create the system database", Run: s.stepSystemDB},
		{Name: "Install the extensions and register the server", Run: s.stepCatalog},
		{Name: "Record the cluster and finish setup", Irreversible: true, Run: s.stepFinish},
	}}
}

// defaultRules are the managed network rules of a new cluster: password
// logins from the local machine only. They are stored as revision 1.
func defaultRules() []api.NetworkAccessRule {
	return []api.NetworkAccessRule{
		{Type: "host", Databases: []string{"all"}, Users: []string{"all"}, Address: "127.0.0.0/8", Method: "scram-sha-256"},
		{Type: "host", Databases: []string{"all"}, Users: []string{"all"}, Address: "::1/128", Method: "scram-sha-256"},
	}
}

// HBARules converts contract rules to instance rules.
func HBARules(rules []api.NetworkAccessRule) []instance.HBARule {
	out := make([]instance.HBARule, len(rules))
	for i, r := range rules {
		out[i] = instance.HBARule{Type: r.Type, Databases: r.Databases, Users: r.Users, Address: r.Address, Method: r.Method}
	}
	return out
}

// PKIDir is the directory of the certificate authority and the server key.
func (s *Service) PKIDir() string { return filepath.Join(s.cfg.DataDir, "pki") }

// File names of the server's own certificate material in the pki directory.
const (
	ServerCertFile = "server.crt"
	ServerKeyFile  = "server.key"
)

// ServerNames are the names the certificate of this server carries.
func (s *Service) ServerNames() pki.Names {
	n := pki.Names{CommonName: s.cfg.ServerHost, DNS: []string{"localhost"}, IPs: []net.IP{net.ParseIP("127.0.0.1")}}
	return n
}

// stepPKI creates the CA of the cluster and this server's certificate: the key
// is generated here and only the request goes to the CA, which lives in the
// same process on the first server.
func (s *Service) stepPKI(_ context.Context, _ *task.Env) error {
	dir := s.PKIDir()
	ca, err := pki.LoadCA(dir)
	if err != nil {
		if !os.IsNotExist(err) {
			return fmt.Errorf("loading the CA: %w", err)
		}
		if ca, err = pki.CreateCA(dir, "", time.Now()); err != nil {
			return fmt.Errorf("creating the CA: %w", err)
		}
	}
	ca.SetServerLifetime(s.cfg.CertificateLifetime)
	if _, err := os.Stat(filepath.Join(dir, ServerCertFile)); err == nil {
		return nil
	}
	names := s.ServerNames()
	key, csr, err := pki.NewKeyAndCSR(names)
	if err != nil {
		return err
	}
	crt, _, err := ca.Sign(csr, names, time.Now())
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, ServerKeyFile), key, 0o600); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, ServerCertFile), crt, 0o644) //nolint:gosec // a certificate is public
}

func (s *Service) stepInstance(ctx context.Context, env *task.Env) error {
	p, err := paramsOf(env.Task)
	if err != nil {
		return err
	}
	pw, err := s.credentials()
	if err != nil {
		return fmt.Errorf("creating the service credentials: %w", err)
	}
	if !s.mgr.Exists(NodeID) {
		mb := p.MemoryMB
		err := s.mgr.Init(ctx, instance.Spec{
			ID: NodeID, SuperuserPassword: pw, HBA: HBARules(defaultRules()),
			Settings: instance.ManagedSettings{
				Port: s.cfg.PGPort, ListenAddresses: s.cfg.PGListen, ClusterName: p.ClusterName, MemoryMegabytes: mb,
				TLSCertFile: s.cfg.TLSCertFile, TLSKeyFile: s.cfg.TLSKeyFile,
				Extra: map[string]string{
					"shared_buffers":       itoa(mb/4) + "MB",
					"effective_cache_size": itoa(mb/2) + "MB",
					"maintenance_work_mem": itoa(min(mb/16, 2048)) + "MB",
				},
			},
		})
		if err != nil {
			return fmt.Errorf("initializing the instance: %w", err)
		}
	}
	if err := s.mgr.Start(ctx, NodeID); err != nil {
		return fmt.Errorf("starting the instance: %w", err)
	}
	return s.waitReady(ctx)
}

var errNotQuoted = errors.New("internal error: statement could not be built")

func execB(ctx context.Context, c *pgx.Conn, b *sqlx.Builder) error {
	st, err := b.Build()
	if err != nil {
		return errNotQuoted
	}
	_, err = c.Exec(ctx, st.SQL, st.Args...)
	return err
}

func roleExists(ctx context.Context, c *pgx.Conn, name string) (bool, error) {
	var ok bool
	err := c.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM pg_catalog.pg_roles WHERE rolname = $1)", name).Scan(&ok)
	return ok, err
}

func (s *Service) stepRoles(ctx context.Context, env *task.Env) error {
	p, err := paramsOf(env.Task)
	if err != nil {
		return err
	}
	pw, _ := env.Secret("adminPassword")
	verifier, err := scram.Verifier(pw)
	if err != nil {
		return errors.New("the administrator password is not acceptable")
	}
	c, err := s.connect(ctx, "postgres")
	if err != nil {
		return fmt.Errorf("connecting to the instance: %w", err)
	}
	defer func() { _ = c.Close(context.WithoutCancel(ctx)) }()
	for _, r := range ClusterRoles {
		ok, err := roleExists(ctx, c, r)
		if err != nil {
			return err
		}
		if !ok {
			if err := execB(ctx, c, new(sqlx.Builder).Text("CREATE ROLE").ID(sqlx.MustIdent(r)).Text("NOLOGIN")); err != nil {
				return fmt.Errorf("creating role %s: %w", r, err)
			}
		}
	}
	admin, err := sqlx.NewIdent(p.AdminUser)
	if err != nil {
		return err
	}
	ok, err := roleExists(ctx, c, p.AdminUser)
	if err != nil {
		return err
	}
	b := new(sqlx.Builder)
	if ok {
		b.Text("ALTER ROLE").ID(admin)
	} else {
		b.Text("CREATE ROLE").ID(admin)
	}
	b.Text("LOGIN CREATEROLE CREATEDB PASSWORD").Lit(verifier)
	if err := execB(ctx, c, b); err != nil {
		return fmt.Errorf("creating the administrator role: %w", err)
	}
	for _, r := range ClusterRoles {
		if err := execB(ctx, c, new(sqlx.Builder).Text("GRANT").ID(sqlx.MustIdent(r)).Text("TO").ID(admin)); err != nil {
			return fmt.Errorf("granting %s: %w", r, err)
		}
	}
	return nil
}

func (s *Service) stepSystemDB(ctx context.Context, _ *task.Env) error {
	c, err := s.connect(ctx, "postgres")
	if err != nil {
		return err
	}
	var exists bool
	err = c.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM pg_catalog.pg_database WHERE datname = $1)", sysdb.Database).Scan(&exists)
	if err == nil && !exists {
		err = execB(ctx, c, new(sqlx.Builder).Text("CREATE DATABASE").ID(sqlx.MustIdent(sysdb.Database)))
	}
	_ = c.Close(context.WithoutCancel(ctx))
	if err != nil {
		return fmt.Errorf("creating the system database: %w", err)
	}
	sc, err := s.connect(ctx, sysdb.Database)
	if err != nil {
		return err
	}
	defer func() { _ = sc.Close(context.WithoutCancel(ctx)) }()
	if _, err := sysdb.Migrate(ctx, sc); err != nil {
		return fmt.Errorf("applying migrations: %w", err)
	}
	if err := HardenSystemDatabase(ctx, sc); err != nil {
		return err
	}
	return nil
}

func (s *Service) stepCatalog(ctx context.Context, env *task.Env) error {
	p, err := paramsOf(env.Task)
	if err != nil {
		return err
	}
	c, err := s.connect(ctx, sysdb.Database)
	if err != nil {
		return err
	}
	defer func() { _ = c.Close(context.WithoutCancel(ctx)) }()
	if p.Sharding {
		if _, err := c.Exec(ctx, "CREATE EXTENSION IF NOT EXISTS kingres_shard"); err != nil {
			return fmt.Errorf("installing kingres_shard: %w", err)
		}
		var nodes int
		if err := c.QueryRow(ctx, "SELECT count(*) FROM kingres.node").Scan(&nodes); err != nil {
			return fmt.Errorf("reading the node catalog: %w", err)
		}
		if nodes == 0 {
			if _, err := c.Exec(ctx, "SELECT kingres.init_cluster($1, $2)", s.cfg.ServerHost, s.cfg.PGPort); err != nil {
				return fmt.Errorf("registering the node: %w", err)
			}
		}
	}
	if p.Ledger {
		if _, err := c.Exec(ctx, "CREATE EXTENSION IF NOT EXISTS kingres_ledger"); err != nil {
			return fmt.Errorf("installing kingres_ledger: %w", err)
		}
	}
	// pg_stat_statements in the databases the cluster makes itself; the databases of the users get it when they are made (ops installAnalytics), and a server
	// that is added gets it with every database of the leader (ApplyDatabases).
	for _, db := range []string{"postgres", sysdb.Database} {
		if err := s.installStatStatements(ctx, db); err != nil {
			return err
		}
	}
	tx, err := c.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	settings := map[string]any{
		"cluster.id": p.ClusterID, "cluster.name": p.ClusterName,
		"replica.default": p.Replicas, "durability.default": p.Durability,
		"memory.quotaMegabytes": p.MemoryMB,
		"features":              map[string]bool{"sharding": p.Sharding, "ledger": p.Ledger},
	}
	for k, v := range settings {
		j, _ := json.Marshal(v)
		if _, err := tx.Exec(ctx, `INSERT INTO kingres_cp.cluster_setting (key, value) VALUES ($1, $2)
			ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value, updated_at = now()`, k, j); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(ctx, `INSERT INTO kingres_cp.server (server_id, version) VALUES ($1, NULL) ON CONFLICT DO NOTHING`, s.cfg.ServerHost); err != nil {
		return err
	}
	var haveRev bool
	if err := tx.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM kingres_cp.network_revision)").Scan(&haveRev); err != nil {
		return err
	}
	if !haveRev {
		for i, r := range defaultRules() {
			j, _ := json.Marshal(r)
			if _, err := tx.Exec(ctx, "INSERT INTO kingres_cp.network_rule (revision, position, rule) VALUES (1, $1, $2)", i, j); err != nil {
				return err
			}
		}
		if _, err := tx.Exec(ctx, "INSERT INTO kingres_cp.network_revision (revision, updated_by) VALUES (1, $1)", env.Task.CreatedBy); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

func (s *Service) stepFinish(_ context.Context, env *task.Env) error {
	p, err := paramsOf(env.Task)
	if err != nil {
		return err
	}
	if s.setup.State() == setup.Clustered {
		return nil
	}
	host := s.cfg.ServerHost
	if err := s.setup.Transition(setup.Clustered, p.ClusterID, p.ClusterName, &host); err != nil {
		return fmt.Errorf("recording the node state: %w", err)
	}
	s.publishDSN()
	return nil
}

// onSetupTaskChange mirrors the setup task into the node state (served by
// GET /setup/state) and, once it is done, copies it into the system
// database so that GET /tasks/{id} returns it after sign-in.
func (s *Service) onSetupTaskChange(t task.Task) {
	sum := Summary(t)
	if err := s.setup.SetSetupTask(sum); err != nil {
		s.log.Error("recording the setup task state", "error", err)
	}
	if t.State == task.Done {
		s.archive(t)
	}
}

// Summary converts a setup task to the contract's SetupTaskSummary.
func Summary(t task.Task) api.SetupTaskSummary {
	steps := make([]api.SetupTaskStep, len(t.Steps))
	for i, st := range t.Steps {
		steps[i] = api.SetupTaskStep{Name: st.Name, State: st.State}
	}
	sum := api.SetupTaskSummary{ID: t.ID, Kind: t.Kind, State: string(t.State), ProgressPercent: t.Percent,
		Steps: steps, CreatedAt: t.CreatedAt.UTC(), UpdatedAt: t.UpdatedAt.UTC()}
	if t.Error != nil {
		te := &api.TaskError{Code: t.Error.Code, Message: t.Error.Message}
		if t.Error.Step != "" {
			st := t.Error.Step
			te.Step = &st
		}
		r := t.Error.Retryable
		te.Retryable = &r
		sum.Error = te
	}
	if t.Detail != "" {
		d := t.Detail
		sum.Detail = &d
	}
	return sum
}

func (s *Service) archive(t task.Task) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(s.bg), 20*time.Second)
	defer cancel()
	c, err := s.connect(ctx, sysdb.Database)
	if err != nil {
		s.log.Error("archiving the setup task", "error", err)
		return
	}
	defer func() { _ = c.Close(ctx) }()
	prog, _ := json.Marshal(map[string]any{"percent": 100, "steps": t.Steps})
	_, err = c.Exec(ctx, `INSERT INTO kingres_cp.task (id, kind, state, subject_type, subject_id, input, progress, created_at, updated_at, created_by)
		OVERRIDING SYSTEM VALUE VALUES ($1, $2, 'done', $3, $4, $5, $6, $7, $8, $9) ON CONFLICT (id) DO NOTHING`,
		t.ID, t.Kind, t.SubjectType, t.SubjectID, []byte(`{}`), prog, t.CreatedAt, t.UpdatedAt, t.CreatedBy)
	if err == nil {
		_, err = c.Exec(ctx, `SELECT setval(pg_get_serial_sequence('kingres_cp.task', 'id'), (SELECT max(id) FROM kingres_cp.task))`)
	}
	if err != nil {
		s.log.Error("archiving the setup task", "error", err)
	}
}

// HardenSystemDatabase takes from PUBLIC what it has in the system database: CONNECT and TEMPORARY on the database, CREATE in its schema public. CONNECT is
// then given to the cluster roles only (kingres_admin, kingres_ops, kingres_readonly, kingres_stats), which is what a sign-in to the console needs: the
// authenticator connects as the signing-in role, which is a member of one of them. The service role is a superuser and needs nothing. It is idempotent; c is a
// service connection to the system database. A role that has CONNECT of its own or that is a superuser is not touched.
func HardenSystemDatabase(ctx context.Context, c *pgx.Conn) error {
	db := sqlx.MustIdent(sysdb.Database)
	if err := execB(ctx, c, new(sqlx.Builder).Text("REVOKE ALL ON DATABASE").ID(db).Text("FROM PUBLIC")); err != nil {
		return fmt.Errorf("revoking the privileges of PUBLIC on the system database: %w", err)
	}
	for _, r := range ClusterRoles {
		ok, err := roleExists(ctx, c, r)
		if err != nil {
			return err
		}
		if !ok {
			continue
		}
		if err := execB(ctx, c, new(sqlx.Builder).Text("GRANT CONNECT ON DATABASE").ID(db).Text("TO").ID(sqlx.MustIdent(r))); err != nil {
			return fmt.Errorf("granting CONNECT on the system database to %s: %w", r, err)
		}
	}
	if err := execB(ctx, c, new(sqlx.Builder).Text("REVOKE CREATE ON SCHEMA public FROM PUBLIC")); err != nil {
		return fmt.Errorf("revoking CREATE on the schema public of the system database: %w", err)
	}
	return nil
}

// StatStatementsExtension is the extension that every database made by kingresd carries, so that the statistics of the statements of the instance can be read
// from any database. Its library is in shared_preload_libraries of every instance (instance.PreloadLibraries).
const StatStatementsExtension = "pg_stat_statements"

// installStatStatements creates pg_stat_statements in database db when the package of this server has it. A package without it (an older one) leaves the database as
// it is: setting up a cluster does not fail for the statistics.
func (s *Service) installStatStatements(ctx context.Context, db string) error {
	c, err := s.connect(ctx, db)
	if err != nil {
		return err
	}
	defer func() { _ = c.Close(context.WithoutCancel(ctx)) }()
	var avail bool
	if err := c.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM pg_catalog.pg_available_extensions WHERE name = $1)", StatStatementsExtension).Scan(&avail); err != nil {
		return err
	}
	if !avail {
		s.log.Info("the package of this server does not contain pg_stat_statements; the database is made without it", "database", db)
		return nil
	}
	// SCHEMA public: the service session runs with search_path = pg_catalog, and an extension without a schema of its own would be made there (and refused).
	if err := execB(ctx, c, new(sqlx.Builder).Text("CREATE EXTENSION IF NOT EXISTS").ID(sqlx.MustIdent(StatStatementsExtension)).Text("SCHEMA public")); err != nil {
		return fmt.Errorf("installing pg_stat_statements in %s: %w", db, err)
	}
	return nil
}
