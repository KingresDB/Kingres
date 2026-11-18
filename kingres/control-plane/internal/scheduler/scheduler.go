// SPDX-License-Identifier: AGPL-3.0-only

// Package scheduler runs the recurring jobs of the manager leader (design
// ledger-phase7.md, 6.7). It is the first recurring scheduler of kingresd;
// the phases that need one later (rebalancing policies, partition policies)
// add jobs to it.
//
// The scheduler runs on the manager leader only. When this server becomes the
// leader it waits one lease margin (so that the last act of the previous
// leader is over), increments the term in one transaction (BumpTerm), and
// starts its jobs with a context that is cancelled when leadership is lost. A
// leader change is therefore a restart of the loop: nothing a job needs
// survives in memory.
//
// The term is the fencing token of every write of a job and of the tasks
// that belong to the same work (the key tasks of internal/ledgersign): the
// write is made in a transaction whose first statement is Term.Guard, which
// fails with ErrDeposed when the stored term is not the one this server was
// given. A leader that was deposed and wakes up, after its network or its
// process was paused, holds a smaller term than the stored one and cannot
// write. The task engine's lease epoch does the same for a task row; the term
// covers the writes that are not part of a task.
package scheduler

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"runtime/debug"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
)

// Term is the term of a leader: a number that grows by one each time a server
// takes over as the manager leader and runs the scheduler.
type Term int64

// ErrDeposed means that the stored term is not the term of the caller: another
// leader has taken over (or the caller never held a term). The write that asked
// must not happen, and the caller stops what it does as the leader.
var ErrDeposed = errors.New("scheduler: this server's term is over, another leader has taken over")

// ErrNotLeader means the scheduler holds no term now (this server is not the
// leader, or it is waiting out the lease margin).
var ErrNotLeader = errors.New("scheduler: this server is not the manager leader yet")

// Job is a recurring job. Run is called on the leader only, with the term the
// leader holds, and again after Every (with Jitter, a fraction of Every, added
// or taken away at random). A job runs one pass at a time; a pass that fails is
// logged and tried again at the next time. A pass that returns ErrDeposed ends
// the leadership of this scheduler.
type Job struct {
	Name   string
	Every  time.Duration
	Jitter float64
	Run    func(ctx context.Context, term Term) error
}

// Querier is what Guard needs: a transaction or a connection.
type Querier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// Guard is the first statement of every write made under a term. It takes a
// share lock on the row of the term (so that the next leader's increment waits
// for the write, and the write waits for nobody) and fails with ErrDeposed
// when the stored term differs. It must run inside the transaction of the
// write: a lock taken outside a transaction ends with the statement.
func (t Term) Guard(ctx context.Context, q Querier) error {
	var one int
	err := q.QueryRow(ctx, `SELECT 1 FROM kingres_cp.ledger_cluster WHERE term = $1 FOR SHARE`, int64(t)).Scan(&one)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrDeposed
	}
	if err != nil {
		return fmt.Errorf("scheduler: checking the term: %w", err)
	}
	return nil
}

// BumpTerm increments the stored term and returns the new one. It is the
// default of Config.Bump; the caller runs it on a connection to the system
// database of the manager.
func BumpTerm(ctx context.Context, q Querier) (Term, error) {
	var n int64
	if err := q.QueryRow(ctx, `UPDATE kingres_cp.ledger_cluster SET term = term + 1 RETURNING term`).Scan(&n); err != nil {
		return 0, fmt.Errorf("scheduler: taking a new term: %w", err)
	}
	return Term(n), nil
}

// Config configures a Scheduler.
type Config struct {
	// Self is the server id of this server.
	Self string
	// Leader returns the server id of the manager leader, "" when unknown.
	Leader func(ctx context.Context) (string, error)
	// Margin is the time a new leader waits before it takes a term (the lease
	// margin of the high-availability settings).
	Margin time.Duration
	// Bump increments the stored term and returns it.
	Bump func(ctx context.Context) (Term, error)
	// Poll is how often the leader view is read (default 2 s).
	Poll time.Duration
	// Log receives the lines of the scheduler; nil discards them.
	Log *slog.Logger
}

// Scheduler runs the jobs while this server is the manager leader.
type Scheduler struct {
	cfg  Config
	log  *slog.Logger
	mu   sync.Mutex
	jobs []Job
	term Term
	lead bool
	// Started is incremented every time this scheduler starts leading (tests).
	started int
}

// New builds a scheduler.
func New(cfg Config) (*Scheduler, error) {
	if cfg.Self == "" || cfg.Leader == nil || cfg.Bump == nil {
		return nil, errors.New("scheduler: Self, Leader and Bump are required")
	}
	if cfg.Poll <= 0 {
		cfg.Poll = 2 * time.Second
	}
	log := cfg.Log
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Scheduler{cfg: cfg, log: log}, nil
}

// Add registers a job. It must be called before Run.
func (s *Scheduler) Add(j Job) error {
	if j.Name == "" || j.Run == nil || j.Every <= 0 || j.Jitter < 0 || j.Jitter >= 1 {
		return fmt.Errorf("scheduler: job %q needs a name, a function, a positive interval and a jitter from 0 to below 1", j.Name)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, o := range s.jobs {
		if o.Name == j.Name {
			return fmt.Errorf("scheduler: job %q is registered twice", j.Name)
		}
	}
	s.jobs = append(s.jobs, j)
	return nil
}

// Term returns the term this server holds as the leader, and false when it
// holds none (it is not the leader, or the margin has not passed).
func (s *Scheduler) Term() (Term, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.term, s.lead
}

// Started is the number of times this scheduler has begun to lead.
func (s *Scheduler) Started() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.started
}

func (s *Scheduler) set(t Term, lead bool) {
	s.mu.Lock()
	s.term, s.lead = t, lead
	if lead {
		s.started++
	}
	s.mu.Unlock()
}

func (s *Scheduler) isLeader(ctx context.Context) bool {
	lctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	id, err := s.cfg.Leader(lctx)
	return err == nil && id == s.cfg.Self
}

func sleep(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}

// Run is the loop of the scheduler; it returns when ctx ends.
func (s *Scheduler) Run(ctx context.Context) {
	for ctx.Err() == nil {
		if s.isLeader(ctx) {
			s.lead1(ctx)
		}
		sleep(ctx, s.cfg.Poll)
	}
}

// lead1 is one period of leadership: wait the margin, take a term, run the
// jobs until leadership is lost, a job reports that this term is over, or ctx
// ends.
func (s *Scheduler) lead1(ctx context.Context) {
	// The previous leader's last act is over after one margin. Leadership is looked at again
	// afterwards: a server that was leader for a moment does not take a term.
	sleep(ctx, s.cfg.Margin)
	if ctx.Err() != nil || !s.isLeader(ctx) {
		return
	}
	term, err := s.cfg.Bump(ctx)
	if err != nil {
		if ctx.Err() == nil {
			s.log.Warn("the scheduler could not take a term", "error", err)
		}
		return
	}
	s.log.Info("the scheduler leads", "term", int64(term))
	jctx, cancel := context.WithCancel(ctx)
	s.set(term, true)
	var wg sync.WaitGroup
	s.mu.Lock()
	jobs := append([]Job(nil), s.jobs...)
	s.mu.Unlock()
	for _, j := range jobs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if s.loop(jctx, j, term) {
				cancel()
			}
		}()
	}
	// Watch the leader view; a lost leadership cancels the jobs.
	for jctx.Err() == nil {
		sleep(jctx, s.cfg.Poll)
		if jctx.Err() == nil && !s.isLeader(ctx) {
			s.log.Info("this server is not the manager leader any more, the scheduler stops", "term", int64(term))
			cancel()
		}
	}
	s.set(0, false)
	wg.Wait()
	cancel()
}

// loop runs one job until ctx ends; it reports whether the term was found to be over. The first pass
// comes after a random part of the jitter, so that the jobs of a new leader do not start in one instant.
func (s *Scheduler) loop(ctx context.Context, j Job, term Term) (deposed bool) {
	sleep(ctx, time.Duration(float64(j.Every)*j.Jitter*rand.Float64())) //nolint:gosec // a spread of timers, not a secret
	for ctx.Err() == nil {
		err := s.pass(ctx, j, term)
		switch {
		case err == nil, ctx.Err() != nil:
		case errors.Is(err, ErrDeposed):
			s.log.Warn("a job found that the term is over", "job", j.Name, "term", int64(term))
			return true
		default:
			s.log.Warn("a job failed", "job", j.Name, "error", err)
		}
		sleep(ctx, time.Duration(float64(j.Every)*(1+j.Jitter*(2*rand.Float64()-1)))) //nolint:gosec // as above
	}
	return false
}

// pass runs one pass of a job; a panic is an error of the pass, not the end of the process.
func (s *Scheduler) pass(ctx context.Context, j Job, term Term) (err error) {
	defer func() {
		if r := recover(); r != nil {
			s.log.Error("a job panicked", "job", j.Name, "panic", fmt.Sprint(r), "stack", string(debug.Stack()))
			err = fmt.Errorf("scheduler: job %s panicked", j.Name)
		}
	}()
	return j.Run(ctx, term)
}
