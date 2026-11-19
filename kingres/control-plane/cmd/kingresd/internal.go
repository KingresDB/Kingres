// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/cluster"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/fault"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/fence"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/fwdproof"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/internalapi"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/leader"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/lease"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/membership"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/monitor"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/ops"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/pki"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/serverinfo"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/setup"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/task"
)

// internalDeps are what the internal listener needs.
type internalDeps struct {
	addr       string
	svc        *cluster.Service
	node       *setup.Manager
	ring       *monitor.Ring
	log        *slog.Logger
	started    time.Time
	serverHost string
	version    string
	dataDir    string
	// leaders reads the manager leader from the catalog; forward runs a
	// forwarded operation (only ever called on the leader).
	leaders *leader.Resolver
	forward func(ctx context.Context, peer string, req internalapi.ForwardRequest) (internalapi.ForwardResponse, error)
	// gather answers a cluster-wide read for a server of the cluster.
	gather func(ctx context.Context, peer string, req internalapi.ForwardRequest) (internalapi.ForwardResponse, error)
	// keys gives the verifier of a role, against which the identity of a forwarded
	// or gathered request is proven (nil: such requests are refused).
	keys fwdproof.KeySource
	// signer signs the assertions of gather calls with this server's certificate key
	// (only the leader's is ever used).
	signer *fwdproof.Signer
	// receiver answers the membership calls.
	receiver *membership.Receiver
	// members is the membership manager (nil without an internal API).
	members *membership.Manager
	// ops records the tasks of instance operations (nil: none); lastLeader is
	// the leader known last, used while this server's instance is stopped.
	ops        *ops.Ops
	lastLeader *leaderMemory
	// calls dispatches the internal calls of the features of high availability.
	calls *callTable
	// fault is the fault-injection hook of the tests (nil in production).
	fault *fault.Hook
	// fence is the memory of fenced task runs (package fence).
	fence *fence.Registry
}

// serveInternal serves the internal API with mutual TLS until ctx ends. A
// server that has no certificate of a cluster yet presents a self-signed
// bootstrap certificate, which a joining or adding server pins or records
// (design D4); only the bootstrap calls are possible then. The certificate is
// chosen at every handshake, so a server that receives its certificate does
// not restart.
func serveInternal(ctx context.Context, d internalDeps) {
	dir := d.svc.PKIDir()
	var logOnce sync.Once
	get := internalCertificate(dir, d.svc.ServerNames, d.node.State, d.svc.Resetting, func(fp string) {
		logOnce.Do(func() {
			d.log.Info("bootstrap certificate of the internal API", "sha256", pki.FormatFingerprint(fp))
		})
	})
	if d.node.State() == setup.Uninitialized {
		// Log the fingerprint at start so that the administrator can pin it.
		if _, fp, err := pki.EnsureBootstrapCert(dir, d.svc.ServerNames(), time.Now()); err == nil && !d.svc.Joining() {
			d.log.Info("internal API bootstrap certificate", "sha256", pki.FormatFingerprint(fp),
				"hint", "give this fingerprint in the header X-Kingres-Pin-Sha256 when adding or joining this server to pin it")
		}
	}
	me := d.serverHost
	instanceDown := func() bool { return d.ops != nil && d.ops.LocalInstanceDown() }
	member := func(ctx context.Context, peer string) (bool, error) {
		if d.members == nil {
			return false, nil
		}
		return d.members.IsMember(ctx, peer)
	}
	be := internalapi.Backend{
		// While this server's instance is stopped on purpose nothing can be read to check a
		// proof or a membership, and the pipeline behind Forward and Gather answers every
		// session operation with system_database_stopped before it does anything: the
		// checks are left to it (nothing is run for the caller either way).
		VerifyIdentity: skipWhile(instanceDown, newIdentityChecker(d.keys, me)),
		// The assertion of a gather call is checked against the certificate of the leader
		// this server knows: nothing is read from the database for it.
		VerifyAssertion: assertionChecker(d.svc, leaderOf(d), me),
		Authorize: func(_ context.Context, peer string, req internalapi.ForwardRequest) (fwdproof.Assertion, error) {
			return d.signer.Sign(req.ProofMessage(fwdproof.KindGather, peer, me))
		},
		Member:  member,
		Fault:   d.fault,
		Fence:   d.fence,
		Forward: forwardGate(instanceDown, member, d.forward),
		State: func(ctx context.Context) internalapi.State {
			st := internalapi.State{ServerID: me, NodeState: string(d.node.State()), Version: d.version,
				InternalVersion: internalapi.Version, UptimeSeconds: int64(time.Since(d.started).Seconds())}
			if d.members != nil && d.node.State() == setup.Clustered {
				vctx, cancel := context.WithTimeout(ctx, 3*time.Second)
				defer cancel()
				if v, err := d.members.LocalVersions(vctx); err == nil {
					st.PostgreSQL, st.Extensions = v.PostgreSQL, v.Extensions
				}
			}
			return st
		},
		// The leader is the server holding the primary of shard group 0.
		Leader:     leaderOf(d),
		PeerLeader: peerLeaderOf(d),
		Self:       me,
		Gather:     d.gather,
		Revoked:    d.svc.RevokedSerials,
		Handle:     handleCall(d),
		Bootstrap:  d.receiver.Bootstrap,
		Joining:    d.svc.Joining,
		// An instance held back until its group lease is won (the fence marker, or the wait marker
		// of an instance that was not running when kingresd started) cannot read the catalog: its
		// callers get 503 service_unavailable and wait, not 403 not_leader.
		Unavailable: func() bool {
			if lease.Waiting(filepath.Join(d.dataDir, leaseDirName), d.svc.SystemInstance().Role.Group) {
				return true
			}
			// The instance is not (yet) running: right after the lease was won it is starting.
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			running, err := d.svc.Manager().Running(ctx, d.svc.SystemInstance().ID)
			if err == nil && !running {
				return true
			}
			// Running but not accepting connections yet (starting, recovering): the catalog cannot be read.
			c, cerr := d.svc.SystemConn(ctx)
			if cerr != nil {
				return true
			}
			_ = c.Close(context.WithoutCancel(ctx))
			return false
		},
		Resources: func(context.Context) (any, error) {
			return serverinfo.Resources(map[string]string{"data": filepath.Join(d.dataDir, "instances")}, nil), nil
		},
		Logs: func(context.Context, map[string]string) (any, error) {
			return serverinfo.ReadLogs(d.svc.LogDir(), &me, 1)
		},
		Metrics: func(context.Context) (any, error) {
			out := map[string]float64{}
			for s, p := range d.ring.Latest() {
				out[s.Metric+"|"+s.ServerID+"|"+s.Database] = *p.V
			}
			return out, nil
		},
		Exec: func(ctx context.Context, r internalapi.ExecRequest) (internalapi.ExecResult, error) {
			return execLocal(ctx, d.svc, r)
		},
	}
	srv := internalapi.NewHTTPServer(d.addr, internalapi.New(d.svc, be, d.log, nil), internalapi.TLSConfigFunc(get),
		slog.NewLogLogger(d.log.Handler(), slog.LevelWarn), internalapi.ResponseTimeout)
	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", d.addr)
	if err != nil {
		d.log.Error("internal API: listening", "address", d.addr, "error", err)
		return
	}
	d.log.Info("internal API listening", "address", ln.Addr().String())
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := srv.ServeTLS(ln, "", ""); err != nil && !errors.Is(err, http.ErrServerClosed) {
			d.log.Error("internal API", "error", err)
		}
	}()
	<-ctx.Done()
	sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	_ = srv.Shutdown(sctx)
	wg.Wait()
}

// execStatementTimeout bounds a statement of the internal exec call. The request
// context ends when the leader disconnects, but a statement that PostgreSQL
// runs does not stop for that on every path, so the server bounds it itself
// (second review, "internal exec has no statement timeout"). It is longer than
// anything the leader runs through exec (schema and ledger work), shorter than
// forever.
const execStatementTimeout = 15 * time.Minute

// execLocal runs one statement on a local database as the service role through
// the local connection and returns at most RowLimit rows. The caller (the
// leader) is authorized by the internal API; the statement is whatever the
// leader sends and is never logged.
func execLocal(ctx context.Context, svc *cluster.Service, r internalapi.ExecRequest) (internalapi.ExecResult, error) {
	limit := r.RowLimit
	if limit == 0 {
		limit = 1000
	}
	c, err := svc.ServiceConn(ctx, r.Database)
	if err != nil {
		return internalapi.ExecResult{}, errors.New("cannot connect to the database")
	}
	defer func() { _ = c.Close(context.WithoutCancel(ctx)) }()
	if _, err := c.Exec(ctx, "SELECT set_config('statement_timeout', $1, false)", strconv.FormatInt(execStatementTimeout.Milliseconds(), 10)); err != nil {
		return internalapi.ExecResult{}, errors.New("cannot connect to the database")
	}
	rows, err := c.Query(ctx, r.SQL, pgx.QueryExecModeExec) // one statement only
	if err != nil {
		return internalapi.ExecResult{}, errors.New("the statement failed")
	}
	defer rows.Close()
	res := internalapi.ExecResult{Columns: []string{}, Rows: [][]any{}}
	for _, fd := range rows.FieldDescriptions() {
		res.Columns = append(res.Columns, fd.Name)
	}
	total := 0
	for rows.Next() && len(res.Rows) < limit {
		// The size of a result is bounded (CP-04): 1 MiB per value, 16 MiB in all.
		for _, raw := range rows.RawValues() {
			total += len(raw) + 8
			if len(raw) > 1<<20 || total > 16<<20 {
				return res, errors.New("the result is too large")
			}
		}
		v, err := rows.Values()
		if err != nil {
			return res, errors.New("the statement failed")
		}
		res.Rows = append(res.Rows, v)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return res, errors.New("the statement failed")
	}
	res.Tag = rows.CommandTag().String()
	return res, nil
}

// runEngineOnLeader runs the task engine on the manager leader only: tasks are
// rows of the leader's system database, and a follower's copy holds none. A
// leader that stops being the leader finishes the step in hand (its lease
// protects the row) and then stops claiming. The leader also sweeps abandoned
// pending servers at start and then hourly.
func runEngineOnLeader(ctx context.Context, eng *task.Engine, leaders *leader.Resolver, self string, members *membership.Manager, log *slog.Logger) {
	var lastSweep time.Time
	for ctx.Err() == nil {
		lctx, cancel := context.WithTimeout(ctx, 3*time.Second)
		id, err := leaders.Get(lctx)
		cancel()
		if err == nil && id == self {
			if members != nil && time.Since(lastSweep) > time.Hour {
				members.SweepAbandoned(ctx)
				lastSweep = time.Now()
			}
			if ran, rerr := eng.RunOnce(ctx); rerr != nil && ctx.Err() == nil {
				log.Error("task engine", "error", rerr)
			} else if ran {
				continue
			}
		}
		select {
		case <-ctx.Done():
		case <-time.After(2 * time.Second):
		}
	}
}

// leaderOf is the leader the internal API authorizes with: the catalog's, and
// while this server's instance is stopped on purpose (the catalog cannot be
// read) the last one known.
func leaderOf(d internalDeps) func() string {
	return func() string {
		if id := d.leaders.Current(); id != "" {
			d.lastLeader.set(id)
			return id
		}
		if d.ops != nil && d.ops.LocalInstanceDown() {
			return d.lastLeader.get()
		}
		return d.lastLeader.recent()
	}
}

// handleCall routes the task calls of instance operations to the task code and
// everything else to the membership receiver.
func handleCall(d internalDeps) func(ctx context.Context, key, peer string, body []byte) (any, error) {
	return func(ctx context.Context, key, peer string, body []byte) (any, error) {
		if d.calls != nil {
			if res, ok, err := d.calls.handle(ctx, key, peer, body); ok {
				return res, err
			}
		}
		switch key {
		case "POST /task/record", "POST /task/settle", "POST /task/reserve":
			if d.ops == nil {
				return nil, &internalapi.CallError{Status: http.StatusNotImplemented, Code: "not_available", Message: "This server cannot record tasks."}
			}
			return d.ops.HandleTaskCall(ctx, key, peer, body)
		}
		return d.receiver.Handle(ctx, key, peer, body)
	}
}

// runVersionCheck compares the versions of the servers every interval while this
// server is the manager leader (alert versionMismatch).
func runVersionCheck(ctx context.Context, members *membership.Manager, leaders *leader.Resolver, self string, every time.Duration, log *slog.Logger) {
	refreshed := false
	for ctx.Err() == nil {
		lctx, cancel := context.WithTimeout(ctx, 3*time.Second)
		id, err := leaders.Get(lctx)
		cancel()
		if err == nil && id == self {
			if !refreshed {
				members.RefreshDirectory(ctx)
				refreshed = true
			}
			cctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
			if err := members.CheckVersions(cctx); err != nil && ctx.Err() == nil {
				log.Warn("checking the versions of the servers", "error", err)
			}
			cancel()
		}
		select {
		case <-ctx.Done():
		case <-time.After(every):
		}
	}
}

// assertionChecker returns the check of the assertion that comes with a gather
// call: the caller (the server named by its certificate) is the asking server.
func assertionChecker(certs fwdproof.PeerVerifier, leader func() string, self string) func(context.Context, string, internalapi.ForwardRequest) error {
	v := &fwdproof.AssertionVerifier{Certs: certs, Leader: leader}
	return func(ctx context.Context, peer string, req internalapi.ForwardRequest) error {
		if req.Assertion == nil {
			return fwdproof.ErrInvalid
		}
		return v.Check(ctx, req.ProofMessage(fwdproof.KindGather, peer, self), *req.Assertion)
	}
}

// skipWhile returns check for the forwarded operations, except that it accepts
// them while skip reports true. Only the kind forward is skipped: the leader
// must never sign a gather assertion for a proof it could not check.
func skipWhile(skip func() bool, check func(context.Context, fwdproof.Kind, string, internalapi.ForwardRequest) error) func(context.Context, fwdproof.Kind, string, internalapi.ForwardRequest) error {
	return func(ctx context.Context, kind fwdproof.Kind, peer string, req internalapi.ForwardRequest) error {
		if kind == fwdproof.KindForward && skip() {
			return nil
		}
		return check(ctx, kind, peer, req)
	}
}

// forwardGate wraps the forward call: the forwarding server must be a member of the
// cluster (a server that holds a certificate but was not admitted yet may not
// forward), except while the local instance is stopped, when the pipeline answers
// system_database_stopped and runs nothing.
func forwardGate(instanceDown func() bool, member func(context.Context, string) (bool, error),
	next func(context.Context, string, internalapi.ForwardRequest) (internalapi.ForwardResponse, error),
) func(context.Context, string, internalapi.ForwardRequest) (internalapi.ForwardResponse, error) {
	return func(ctx context.Context, peer string, req internalapi.ForwardRequest) (internalapi.ForwardResponse, error) {
		if !instanceDown() {
			ok, err := member(ctx, peer)
			if err != nil {
				return internalapi.ForwardResponse{}, fmt.Errorf("checking whether %q is a member: %w", peer, err)
			}
			if !ok {
				return internalapi.ForwardResponse{}, fmt.Errorf("%q is not a member of the cluster", peer)
			}
		}
		return next(ctx, peer, req)
	}
}

// newIdentityChecker returns the check of the proof that comes with a forwarded
// or gathered request. Without a key source nothing can be proven and every
// such request is refused.
func newIdentityChecker(keys fwdproof.KeySource, self string) func(context.Context, fwdproof.Kind, string, internalapi.ForwardRequest) error {
	if keys == nil {
		return func(context.Context, fwdproof.Kind, string, internalapi.ForwardRequest) error {
			return fwdproof.ErrInvalid
		}
	}
	c := fwdproof.NewChecker(keys, 0, nil)
	return func(ctx context.Context, kind fwdproof.Kind, peer string, req internalapi.ForwardRequest) error {
		if req.Proof == nil {
			return fwdproof.ErrInvalid
		}
		return c.Check(ctx, req.ProofMessage(kind, peer, self), *req.Proof)
	}
}

// internalCertificate chooses the certificate of a handshake of the internal API: the cluster
// certificate once the server has one, before that (an uninitialized server) the bootstrap
// certificate, which a joining or adding server pins or records. A server that is a member and has no
// certificate presents nothing: it must not offer a certificate that nobody can pin. The exception is
// a server that is resetting itself: it has deleted its certificates and is about to say so in its
// state; for that moment it already presents the bootstrap certificate, so that a call of the leader
// that arrives in between meets "not a member of this cluster" and not a handshake failure that it
// would treat as a server that cannot be reached.
func internalCertificate(dir string, names func() pki.Names, state func() setup.State, resetting func() bool,
	onBootstrap func(fingerprint string)) func(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	return func(*tls.ClientHelloInfo) (*tls.Certificate, error) {
		if pair, err := tls.LoadX509KeyPair(filepath.Join(dir, cluster.ServerCertFile), filepath.Join(dir, cluster.ServerKeyFile)); err == nil {
			return &pair, nil
		}
		if state() != setup.Uninitialized && !resetting() {
			return nil, errors.New("no server certificate")
		}
		pair, fp, err := pki.EnsureBootstrapCert(dir, names(), time.Now())
		if err != nil {
			return nil, err
		}
		onBootstrap(fp)
		return &pair, nil
	}
}

// peerLeaderOf asks the other servers of the peer directory who the manager leader is (GET /leader, which any peer may
// ask), for the calls of policy LeaderOrHeldBack on a server that cannot read its own catalog: the old primary of a
// failover is rebuilt while its instance is stopped. The answer counts only when every server that answered names the
// same leader; it is kept for three seconds.
func peerLeaderOf(d internalDeps) func(ctx context.Context) string {
	var mu sync.Mutex
	var cached string
	var at time.Time
	return func(ctx context.Context) string {
		mu.Lock()
		if cached != "" && time.Since(at) < 3*time.Second {
			c := cached
			mu.Unlock()
			return c
		}
		mu.Unlock()
		if d.members == nil || d.members.Peers == nil || d.members.Client == nil {
			return ""
		}
		var named []string
		for _, p := range d.members.Peers.All() {
			if p.ServerID == d.serverHost {
				continue
			}
			cctx, cancel := context.WithTimeout(ctx, 2*time.Second)
			var out struct {
				Leader string `json:"leader"`
			}
			err := d.members.Client.Do(cctx, p.ServerID, http.MethodGet, "/leader", nil, &out)
			cancel()
			if err == nil && out.Leader != "" {
				named = append(named, out.Leader)
			}
		}
		if len(named) == 0 {
			return ""
		}
		for _, n := range named {
			if n != named[0] {
				return ""
			}
		}
		mu.Lock()
		cached, at = named[0], time.Now()
		mu.Unlock()
		return named[0]
	}
}
