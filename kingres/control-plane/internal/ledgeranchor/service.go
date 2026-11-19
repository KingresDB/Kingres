// SPDX-License-Identifier: AGPL-3.0-only

package ledgeranchor

import (
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net"
	"net/netip"
	"sync"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/anchor"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/problem"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/scheduler"
)

// Timings of the jobs and of a call.
const (
	// Tick is how often the enqueue job and the monitor look at the clock; the interval of anchoring is a setting
	// that the console changes, so the job is woken often and decides for itself.
	Tick = 10 * time.Second
	// SubmitEvery and UpgradeEvery are the periods of the jobs of design 6.7.
	SubmitEvery  = 30 * time.Second
	UpgradeEvery = 60 * time.Second
	// SubmitTimeout bounds one call of Submit, Resume or Upgrade (the library bounds its own steps too).
	SubmitTimeout = 90 * time.Second
	// UpgradeTimeout is the bound of one Upgrade (correction 90: a few minutes of its own).
	UpgradeTimeout = 5 * time.Minute
	// TestTimeout is the limit of the Test button (6.8).
	TestTimeout = 30 * time.Second
	// BatchSize is the most records one pass of a job takes.
	BatchSize = 100
	// MaxParallelTargets is how many targets one pass works on at the same time: a target that hangs for its
	// timeout holds one place, not the pass.
	MaxParallelTargets = 8
)

// OpenAlert is an open alert of one of the kinds of this package.
type OpenAlert struct {
	Kind, Subject, Severity string
}

// Config configures a Service.
type Config struct {
	Store Store
	Creds *CredStore
	// Term returns the term of the scheduler on this server, false when it holds none (a follower never works).
	Term func() (scheduler.Term, bool)
	// Clock is the clock of this server (default time.Now).
	Clock func() time.Time
	Log   *slog.Logger
	// Alert raises (raise true) or clears an alert of the table kingres_cp.alert; OpenAlerts lists the open alerts of
	// the kinds of this package. Without them no alert is raised.
	Alert      func(ctx context.Context, kind, severity, subject, message string, raise bool)
	OpenAlerts func(ctx context.Context) ([]OpenAlert, error)
	// Databases lists the databases that have ledger tables.
	Databases func(ctx context.Context) ([]string, error)
	// DatabaseExists tells whether a database exists (404 otherwise).
	DatabaseExists func(ctx context.Context, db string) (bool, error)
	// Distribute hands the credentials file of a target to the other servers that may become the leader, and
	// Undistribute takes it away; both are best effort and their errors are logged (correction 181).
	Distribute   func(ctx context.Context, id int64, kind string, file []byte) error
	Undistribute func(ctx context.Context, id int64) error
	// Limits bound the retries of a record (the settings anchor.maxRetryAttempts and anchor.maxRetryAgeHours of the
	// wiring; zero fields mean the defaults of the library).
	Limits anchor.Limits
	// Rand returns a uniform value in [0,1) (the jitter of the backoff).
	Rand func() float64
	// Resolve and Dial replace the lookup and the connection of the egress rule (tests).
	Resolve func(context.Context, string) ([]netip.Addr, error)
	Dial    func(ctx context.Context, network, addr string) (net.Conn, error)
	// Roots replaces the roots of the system for a store that names no CA bundle (tests).
	Roots func() (*x509.CertPool, error)
	// SubmitTimeout and UpgradeTimeout replace the bounds of one call (tests; zero means the constants).
	SubmitTimeout, UpgradeTimeout time.Duration
	// Hook is called at named points of a job ("after-begin", "before-apply", ...); an error it returns ends the
	// step as a crash would (tests of the crash points).
	Hook func(point string) error
}

// Service is the anchoring of the aggregate checkpoints on this server.
type Service struct {
	cfg Config
	log *slog.Logger

	mu          sync.Mutex
	lastEnqueue time.Time
	inflight    map[int64]bool
	exporters   map[string]exporter
}

// New builds a Service.
func New(cfg Config) (*Service, error) {
	if cfg.Store == nil || cfg.Creds == nil || cfg.Term == nil {
		return nil, errors.New("ledgeranchor: Store, Creds and Term are required")
	}
	if cfg.Clock == nil {
		cfg.Clock = time.Now
	}
	if cfg.Rand == nil {
		cfg.Rand = rand.Float64
	}
	if cfg.Roots == nil {
		cfg.Roots = SystemRoots
	}
	log := cfg.Log
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	s := &Service{cfg: cfg, log: log, inflight: map[int64]bool{}, exporters: map[string]exporter{}}
	for _, k := range kinds {
		for f, fn := range k.Exports {
			s.exporters[f] = exporter{kind: k.Name, fn: fn}
		}
	}
	return s, nil
}

// Jobs returns the jobs of anchoring for the scheduler (design 6.7): enqueue, submit, upgrade and the monitor of
// the alerts of 6.9.
func (s *Service) Jobs() []scheduler.Job {
	return []scheduler.Job{
		{Name: "ledger.anchor.enqueue", Every: Tick, Run: s.jobEnqueue},
		{Name: "ledger.anchor.submit", Every: SubmitEvery, Jitter: 0.1, Run: s.jobSubmit},
		{Name: "ledger.anchor.upgrade", Every: UpgradeEvery, Jitter: 0.1, Run: s.jobUpgrade},
		{Name: "ledger.anchor.monitor", Every: Tick, Run: s.jobMonitor},
	}
}

func (s *Service) now() time.Time { return s.cfg.Clock().UTC() }

func (s *Service) hook(point string) error {
	if s.cfg.Hook != nil {
		return s.cfg.Hook(point)
	}
	return nil
}

// term is the term this server holds, or the problem that an operation answers when it holds none.
func (s *Service) term() (scheduler.Term, error) {
	t, ok := s.cfg.Term()
	if !ok {
		return 0, problem.New(problem.ServiceUnavailable, "This server holds no term as the manager leader now; try again.").WithRetryAfter(5)
	}
	return t, nil
}

// claim takes the right to work on one record in this process (one Upgrade at a time per record, duty 4).
func (s *Service) claim(id int64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.inflight[id] {
		return false
	}
	s.inflight[id] = true
	return true
}

func (s *Service) release(id int64) {
	s.mu.Lock()
	delete(s.inflight, id)
	s.mu.Unlock()
}

// Built is a target made from its row: the library's target and what goes with it.
type Built struct {
	Target anchor.Target
	Kind   *Kind
	Red    anchor.Redactor
	Info   Info
	Config []byte
}

// build makes the target of a row: the configuration normalised again, the credentials read (the file of the
// target, or the operator's credentialsPath), the egress rule, the CA bundle and the client. The error is a text
// for the operator and holds no credential.
func (s *Service) build(row TargetRow) (*Built, error) {
	k, err := kindOf(row.Kind)
	if err != nil {
		return nil, err
	}
	cfg, credPath, err := splitConfig(row.Config)
	if err != nil {
		return nil, err
	}
	norm, info, err := k.Normalize(cfg)
	if err != nil {
		return nil, err
	}
	var creds Creds
	switch {
	case credPath != "":
		creds, err = ReadCredentialsFile(credPath, k.Name)
		if err != nil {
			return nil, fmt.Errorf("the credentials file named by credentialsPath cannot be read (it must be a regular file that only its owner can read, with the members of the kind): %w", errors.Unwrap(err))
		}
	case row.CredentialsSet:
		creds, err = s.cfg.Creds.Get(row.ID, k.Name)
		switch {
		case errors.Is(err, ErrNoCredentials):
			return nil, errors.New("this server has no credentials file for the target (it is not a holder, or the key directory was restored without it): enter the credentials again")
		case err != nil:
			return nil, errors.New("the credentials file of the target cannot be opened with the passphrase file of this server")
		}
	}
	eg, err := NewEgress(info.URLs)
	if err != nil {
		return nil, err
	}
	if s.cfg.Resolve != nil {
		eg.Resolve = func(ctx context.Context, h string) ([]netip.Addr, error) { return s.cfg.Resolve(ctx, h) }
	}
	if s.cfg.Dial != nil {
		eg.Dial = func(ctx context.Context, n, a string) (net.Conn, error) { return s.cfg.Dial(ctx, n, a) }
	}
	var roots *x509.CertPool
	switch {
	case info.CABundle != "":
		roots, err = LoadPool(info.CABundle)
		if err != nil {
			return nil, err
		}
	case k.CARequired:
		return nil, errors.New("a CA bundle is required")
	default:
		roots, err = s.cfg.Roots()
		if err != nil {
			return nil, errors.New("the system roots cannot be read: name a CA bundle (caBundlePath)")
		}
	}
	hc := eg.Client(roots)
	t, err := k.Build(BuildEnv{Config: norm, Info: info, Creds: creds, Egress: eg, Client: hc, Clock: anchor.Clock(s.now), TargetID: row.ID})
	if err != nil {
		return nil, redactErr(err, creds)
	}
	return &Built{Target: t, Kind: k, Red: anchor.NewRedactor(creds.Secrets()...), Info: info, Config: norm}, nil
}

// redactErr is err as a text with every credential value removed.
func redactErr(err error, creds Creds) error {
	return errors.New(anchor.NewRedactor(creds.Secrets()...).Clean(err.Error(), 400))
}

// exporter writes the file of a format of a kind from a record.
type exporter struct {
	kind string
	fn   func(Row) ([]byte, error)
}

func (s *Service) timeout(set, def time.Duration) time.Duration {
	if set > 0 {
		return set
	}
	return def
}
