// SPDX-License-Identifier: AGPL-3.0-only

package membership

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/api"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/audit"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/cluster"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/extgate"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/extguard"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/fault"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/fwdproof"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/internalapi"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/leader"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/ops"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/peerclient"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/peers"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/pki"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/problem"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/task"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/wire"
)

// Task kinds of this package (kinds of the contract's TaskKind).
const (
	KindAdd    = "server.add"
	KindRemove = "server.remove"
)

// Manager is the leader's membership logic.
type Manager struct {
	Svc     *cluster.Service
	Ops     *ops.Ops
	Peers   *peers.Directory
	Client  *peerclient.Client
	Leaders *leader.Resolver
	// Self is this server's id and Version its kingresd version.
	Self    string
	Version string
	Log     *slog.Logger
	// Audit records an audit event; nil means none.
	Audit func(ctx context.Context, ev audit.Event)
	// SelfPeer is this server's entry of the peer directory.
	SelfPeer peers.Peer
	// OnCertificateInstalled is called after this server stored a renewed
	// certificate: the connections opened with the old one are closed.
	OnCertificateInstalled func()
	// GatherSigner signs the assertions of gather calls when this server is the
	// leader (see Gatherer.Ask).
	GatherSigner *fwdproof.Signer
	// Fault is the fault-injection hook of the tests (nil in production).
	Fault *fault.Hook
	// LedgerRetire ends the publication of the ledger chains of the groups that a server runs, before the server is removed: a final read, the
	// chain-retired events and one aggregate leaf in which the members are retired (nil: the ledger is not published).
	LedgerRetire func(ctx context.Context, server string) error
	// Standbys removes the standbys that go with a server that is removed or leaves
	// (nil: none are managed).
	Standbys interface {
		PurgeServer(ctx context.Context, server string) error
	}

	// escrow and escrowRecipients are the escrow of the CA key (escrow.go).
	escrow           escrowGuard
	escrowRecipients EscrowRecipients
	// escrowActive and escrowTransport are the seams of the tests of the escrow (nil in production).
	escrowActive    func(ctx context.Context) ([]string, error)
	escrowTransport func(ctx context.Context, id, method, path string, in, out any) error
	escrowDB        escrowDB
	// More seams: the serials this CA issued, the servers whose answers are read, which of the servers are revoked.
	escrowIssued   func(ctx context.Context) (map[string]bool, error)
	escrowReadable func(ctx context.Context) ([]string, error)
	escrowRevoked  func(ctx context.Context) (map[string]bool, error)

	gatherMu   sync.Mutex
	gatherAuth map[string]*gatherAuth
}

func (m *Manager) log() *slog.Logger {
	if m.Log == nil {
		return slog.New(slog.DiscardHandler)
	}
	return m.Log
}

type addInput struct {
	ServerID        string  `json:"serverId"`
	InternalAddress string  `json:"internalAddress"`
	PGPort          int     `json:"pgPort"`
	ServerGroup     *string `json:"serverGroup,omitempty"`
	MemoryMB        int     `json:"memoryMegabytes"`
	Fingerprint     string  `json:"seenFingerprint,omitempty"`
	Pinned          bool    `json:"fingerprintPinned"`
	Via             string  `json:"via"` // "addServer" or "joinCluster"
}

// introduction is what the first contact produced.
type introduction struct {
	ServerID        string
	CSR             string
	InternalAddress string
	PGPort          int
	Seen            string
	Pinned          bool
}

// ErrExists means the server is already a member.
var ErrExists = errors.New("the server is already a member of the cluster")

// caseCollision reports whether a server id differs from an existing one only
// by letters' case. Ids are compared as exact strings but a host name is not:
// the certificate of "Node1" is accepted where "node1" is expected, so a server
// that calls itself "Node1" next to the member "node1" could answer for it.
func caseCollision(existing []string, id string) bool {
	for _, e := range existing {
		if e != id && strings.EqualFold(e, id) {
			return true
		}
	}
	return false
}

// introduce signs the certificate request of a new server and records the
// server as pending in one step (design D4 step 1 and the security
// requirement that no certificate exists for a server that is not recorded).
// Repeating it for the same key returns the same certificate.
func (m *Manager) introduce(ctx context.Context, in introduction) (string, error) {
	ca, err := m.Svc.CA()
	if err != nil {
		return "", errors.New("this server does not hold the cluster certificate authority")
	}
	names := pki.Names{CommonName: in.ServerID, DNS: []string{"localhost"}, IPs: []net.IP{net.ParseIP("127.0.0.1")}}
	if err := names.Validate(); err != nil {
		return "", err
	}
	if err := (peers.Peer{ServerID: in.ServerID, InternalAddr: in.InternalAddress}).Validate(); err != nil {
		return "", err
	}
	if in.PGPort < 1024 || in.PGPort > 65535 {
		return "", errors.New("the PostgreSQL port of the new server is not valid")
	}
	pub, err := pki.CSRPublicKeyDER([]byte(in.CSR))
	if err != nil {
		return "", err
	}
	var certPEM string
	err = m.sys(ctx, func(c *pgx.Conn) error {
		tx, err := c.Begin(ctx)
		if err != nil {
			return err
		}
		defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
		idRows, err := tx.Query(ctx, `SELECT server_id FROM kingres_cp.server`)
		if err != nil {
			return err
		}
		ids, err := pgx.CollectRows(idRows, pgx.RowTo[string])
		if err != nil {
			return err
		}
		if caseCollision(ids, in.ServerID) {
			return ErrExists
		}
		old, err := scanRow(tx.QueryRow(ctx, `SELECT `+rowCols+` FROM kingres_cp.server WHERE server_id = $1 FOR UPDATE`, in.ServerID))
		switch {
		case err == nil && old.Status == StatusActive:
			return ErrExists
		case err == nil && old.Status == StatusRemoving:
			return ErrExists
		case err == nil && bytes.Equal(old.PublicKey, pub) && old.CertPEM != "":
			certPEM = old.CertPEM
			_, err = tx.Exec(ctx, `UPDATE kingres_cp.server SET seen_fingerprint = $2, fingerprint_pinned = $3, internal_address = $4, pg_port = $5
				WHERE server_id = $1`, in.ServerID, in.Seen, in.Pinned, in.InternalAddress, in.PGPort)
			if err != nil {
				return err
			}
			return tx.Commit(ctx)
		case err != nil && !errors.Is(err, pgx.ErrNoRows):
			return err
		}
		// A pending server with another key: what was signed for it is revoked.
		if err == nil && old.Serial != "" {
			if rerr := ca.Revoke(old.Serial, time.Now()); rerr != nil {
				return rerr
			}
		}
		crt, serial, err := ca.Sign([]byte(in.CSR), names, time.Now())
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `INSERT INTO kingres_cp.server (server_id, status, internal_address, pg_port, certificate_serial, certificate_pem,
				public_key_der, seen_fingerprint, fingerprint_pinned, registering, joined_at)
			VALUES ($1, 'pending', $2, $3, $4, $5, $6, $7, $8, false, now())
			ON CONFLICT (server_id) DO UPDATE SET status = 'pending', internal_address = EXCLUDED.internal_address, pg_port = EXCLUDED.pg_port,
				certificate_serial = EXCLUDED.certificate_serial, certificate_pem = EXCLUDED.certificate_pem, public_key_der = EXCLUDED.public_key_der,
				seen_fingerprint = EXCLUDED.seen_fingerprint, fingerprint_pinned = EXCLUDED.fingerprint_pinned, registering = false`,
			in.ServerID, in.InternalAddress, in.PGPort, serial, string(crt), pub, in.Seen, in.Pinned)
		if err == nil {
			if parsed, perr := pki.ParseCertPEM(crt); perr == nil {
				_, err = tx.Exec(ctx, `INSERT INTO kingres_cp.certificate (serial, server_id, not_after) VALUES ($1, $2, $3) ON CONFLICT DO NOTHING`, serial, in.ServerID, parsed.NotAfter)
			}
		}
		if err == nil {
			err = tx.Commit(ctx)
		}
		if err != nil {
			// What was signed must not outlive a failed record.
			if rerr := ca.Revoke(serial, time.Now()); rerr != nil {
				m.log().Error("revoking a certificate whose record failed", "serial", serial, "error", rerr)
			}
			return err
		}
		certPEM = string(crt)
		return nil
	})
	if err != nil {
		return "", err
	}
	if err := m.Peers.Put(peers.Peer{ServerID: in.ServerID, InternalAddr: in.InternalAddress}); err != nil {
		return "", err
	}
	m.log().Info("server introduced", "server", in.ServerID, "fingerprint_pinned", in.Pinned, "sha256", pki.FormatFingerprint(in.Seen))
	return certPEM, nil
}

func conflictProblem(msg string) *problem.Problem { return problem.New(problem.Conflict, msg) }

// startAdd submits the server.add task for an introduced server.
func (m *Manager) startAdd(ctx context.Context, actor string, in addInput) (task.Task, error) {
	in.MemoryMB = max(in.MemoryMB, m.memoryQuota(ctx))
	t, err := m.Ops.StartTask(ctx, KindAdd, "server", in.ServerID, in, actor)
	switch {
	case errors.Is(err, task.ErrSubjectBusy):
		return t, problem.New(problem.OperationInProgress, "This server is already being added.")
	case err != nil:
		return t, err
	}
	return t, nil
}

func (m *Manager) audit(ctx context.Context, actor, action, resource, outcome string, details map[string]any) {
	if m.Audit == nil {
		return
	}
	m.Audit(ctx, audit.Event{Actor: actor, Source: "system", Action: action, ResourceType: "server", ResourceID: resource,
		Outcome: outcome, Details: details})
}

// AddServer is POST /servers on the leader: the cluster contacts the new
// server at the address of its REST API (its internal API is on the port above
// it), authenticates with the new server's setup token, introduces it and
// starts the task. pin is the expected SHA-256 fingerprint of the new
// server's bootstrap certificate (hex) or "" for trust on first use.
func (m *Manager) AddServer(ctx context.Context, actor string, req api.AddServerRequest, pin string) (task.Task, error) {
	if _, err := m.ca(); err != nil {
		return task.Task{}, err
	}
	var errs []problem.FieldError
	internal, aerr := cluster.InternalAddressOf(req.Address)
	if aerr != nil {
		errs = append(errs, problem.FieldError{Field: "/address", Message: "must be host:port of the new server's REST API", Code: "pattern"})
	}
	if req.SetupToken == "" || len(req.SetupToken) > 256 {
		errs = append(errs, problem.FieldError{Field: "/setupToken", Message: "is required", Code: "required"})
	}
	if req.ServerGroup != nil {
		errs = append(errs, problem.FieldError{Field: "/serverGroup", Message: "server groups are not supported yet; use null", Code: "unsupported"})
	}
	if len(errs) > 0 {
		return task.Task{}, problem.Validation("The add-server request is invalid.", errs...)
	}
	hdr := map[string]string{"X-Setup-Token": req.SetupToken}
	var prep wire.PrepareResponse
	seen, err := peerclient.BootstrapCall(ctx, internal, pin, "POST", "/bootstrap/prepare", hdr, wire.Empty{}, &prep)
	if err := m.bootstrapProblem(err, req.Address, "contacting the new server"); err != nil {
		return task.Task{}, err
	}
	if prep.InternalVersion != internalapi.Version {
		return task.Task{}, problem.New(problem.Unprocessable, fmt.Sprintf("The new server speaks internal API version %s, this cluster %s.", prep.InternalVersion, internalapi.Version))
	}
	_, port, _ := net.SplitHostPort(internal)
	if strconv.Itoa(prep.InternalPort) != port {
		return task.Task{}, problem.New(problem.Unprocessable,
			fmt.Sprintf("The internal API of the new server listens on port %d; it must listen on the port above its REST API (%s).", prep.InternalPort, port))
	}
	if host, _, _ := net.SplitHostPort(internal); !hostMatches(ctx, host, prep.ServerID) {
		return task.Task{}, problem.New(problem.Unprocessable,
			fmt.Sprintf("The new server calls itself %q, which is not the host %q it was reached at (set its server-host).", prep.ServerID, host))
	}
	cert, err := m.introduce(ctx, introduction{ServerID: prep.ServerID, CSR: prep.CSR, InternalAddress: internal, PGPort: prep.PGPort, Seen: seen, Pinned: pin != ""})
	switch {
	case errors.Is(err, ErrExists):
		return task.Task{}, conflictProblem("A server with this id is already a member of the cluster.")
	case err != nil:
		m.log().Warn("introducing a server failed", "server", prep.ServerID, "error", err)
		return task.Task{}, problem.New(problem.Unprocessable, "The certificate request of the new server was refused.")
	}
	ca, _ := m.Svc.CA()
	ins := wire.InstallRequest{CACert: string(ca.CertPEM()), ServerCert: cert}
	if _, err := peerclient.BootstrapCall(ctx, internal, seen, "POST", "/bootstrap/install", hdr, ins, nil); err != nil {
		m.discardPending(context.WithoutCancel(ctx), prep.ServerID)
		return task.Task{}, m.bootstrapProblem(err, req.Address, "installing the certificate on the new server")
	}
	t, err := m.startAdd(ctx, actor, addInput{ServerID: prep.ServerID, InternalAddress: internal, PGPort: prep.PGPort,
		Fingerprint: seen, Pinned: pin != "", Via: "addServer"})
	if err != nil {
		return t, err
	}
	return t, nil
}

// bootstrapProblem converts the error of a bootstrap call to a problem.
func (m *Manager) bootstrapProblem(err error, address, doing string) error {
	if err == nil {
		return nil
	}
	var ae *peerclient.APIError
	switch {
	case errors.Is(err, peerclient.ErrFingerprintMismatch):
		return problem.New(problem.Unprocessable, "The certificate of the new server does not match the fingerprint you gave; nothing was sent to it.")
	case errors.As(err, &ae) && (ae.Status == 401 || ae.Code == "setup_token_invalid"):
		return problem.New(problem.SetupTokenInvalid, "The new server did not accept the setup token.")
	case errors.As(err, &ae) && ae.Status == 409:
		return conflictProblem("The new server cannot join now: " + ae.Message)
	case errors.As(err, &ae):
		return problem.New(problem.Unprocessable, "The new server refused the request: "+ae.Message)
	}
	m.log().Warn("bootstrap call failed", "address", address, "doing", doing, "error", err)
	return problem.New(problem.ServiceUnavailable, "The new server at "+address+" cannot be reached on its internal API port.").WithRetryAfter(5)
}

// hostMatches reports whether the server id names the host that was dialed:
// the same name, or names that resolve to a common address. It stops a server
// from registering under the identity of another host.
func hostMatches(ctx context.Context, dialed, id string) bool {
	if dialed == id {
		return true
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	a, err1 := net.DefaultResolver.LookupHost(ctx, dialed)
	b, err2 := net.DefaultResolver.LookupHost(ctx, id)
	if err1 != nil || err2 != nil {
		return false
	}
	for _, x := range a {
		for _, y := range b {
			if x == y {
				return true
			}
		}
	}
	return false
}

// JoinFromPeer is the call from the server that verified an administrator
// (design D4 step 1, joining): the leader introduces the new server and starts
// the task.
func (m *Manager) JoinFromPeer(ctx context.Context, req wire.AddServerRequest) (wire.JoinResponse, error) {
	if _, err := m.ca(); err != nil {
		return wire.JoinResponse{}, err
	}
	if req.ServerGroup != nil {
		return wire.JoinResponse{}, problem.New(problem.Unprocessable, "Server groups are not supported yet.")
	}
	if host, _, err := net.SplitHostPort(req.InternalAddress); err != nil || host == "" {
		return wire.JoinResponse{}, problem.New(problem.Unprocessable, "The internal address of the new server is not valid.")
	}
	cert, err := m.introduce(ctx, introduction{ServerID: req.ServerID, CSR: req.CSR, InternalAddress: req.InternalAddress, PGPort: req.PGPort,
		Seen: req.Fingerprint, Pinned: req.PinnedByCaller})
	switch {
	case errors.Is(err, ErrExists):
		return wire.JoinResponse{}, conflictProblem("A server with this id is already a member of the cluster.")
	case err != nil:
		m.log().Warn("introducing a server failed", "server", req.ServerID, "error", err)
		return wire.JoinResponse{}, problem.New(problem.Unprocessable, "The certificate request was refused.")
	}
	t, err := m.startAdd(ctx, req.Actor, addInput{ServerID: req.ServerID, InternalAddress: req.InternalAddress, PGPort: req.PGPort,
		MemoryMB: req.MemoryMegabytes, Fingerprint: req.Fingerprint, Pinned: req.PinnedByCaller, Via: "joinCluster"})
	if err != nil {
		var p *problem.Problem
		if !errors.As(err, &p) || p.Code != problem.OperationInProgress {
			m.discardPending(context.WithoutCancel(ctx), req.ServerID)
		}
		return wire.JoinResponse{}, err
	}
	ca, _ := m.Svc.CA()
	id, name, _ := m.clusterIdentity(ctx)
	return wire.JoinResponse{TaskID: t.ID, CACert: string(ca.CertPEM()), ServerCert: cert, ClusterID: id, ClusterName: name}, nil
}

// discardPending revokes the certificate signed for a pending server and
// forgets the server.
func (m *Manager) discardPending(ctx context.Context, id string) {
	row, err := m.getServer(ctx, id)
	if err != nil || row.Status != StatusPending || row.Registering {
		return
	}
	if row.Serial != "" {
		// A certificate that cannot be revoked now (this server does not hold the CA key, or
		// the list cannot be written) keeps its pending record: forgetting the server would
		// forget that a valid certificate exists. The next sweep, on a leader that can
		// revoke, finishes it.
		ca, err := m.Svc.CA()
		if err == nil {
			err = ca.Revoke(row.Serial, time.Now())
		}
		if err != nil {
			m.log().Error("revoking the certificate of an abandoned server; its record is kept", "server", id, "error", err)
			return
		}
	}
	_ = m.sys(ctx, func(c *pgx.Conn) error {
		_, err := c.Exec(ctx, `DELETE FROM kingres_cp.server WHERE server_id = $1 AND status = 'pending' AND NOT registering`, id)
		return err
	})
	_ = m.Peers.Delete(id)
	m.Client.Forget(id)
	_ = m.distributeRevocations(ctx)
}

// SweepAbandoned revokes the certificates of pending servers that have no
// unfinished task (a leader that stopped between the introduction and the
// task, or a task that was lost) after a grace period.
func (m *Manager) SweepAbandoned(ctx context.Context) {
	rows, err := m.listServers(ctx)
	if err != nil {
		return
	}
	eng := m.Ops.Engine()
	if eng == nil {
		return
	}
	for _, r := range rows {
		if r.Status != StatusPending || time.Since(r.JoinedAt) < 15*time.Minute || m.hasActiveTask(ctx, eng, r.ID) {
			continue
		}
		if !r.Registering {
			m.log().Warn("revoking the certificate of an abandoned pending server", "server", r.ID)
			m.discardPending(ctx, r.ID)
			continue
		}
		// The server may be registered in the shard catalogs, so its record stays (adding it
		// again finishes the job), but it must not keep a valid certificate for ever: after a
		// day without a task its certificates are revoked.
		if r.RevokedAt == nil && time.Since(r.JoinedAt) > abandonedRegisteredAfter {
			m.log().Warn("revoking the certificates of a registered server whose add never finished", "server", r.ID)
			m.revokeAbandoned(ctx, r)
		}
	}
}

// abandonedRegisteredAfter is how long a pending server that was registered
// keeps its certificates when no task works on it (CP2-14).
const abandonedRegisteredAfter = 24 * time.Hour

func (m *Manager) revokeAbandoned(ctx context.Context, r serverRow) {
	ca, err := m.Svc.CA()
	if err == nil {
		err = m.revokeServerCertificates(ctx, ca, r.ID, r.Serial)
	}
	if err == nil {
		m.Client.Forget(r.ID)
		err = m.distributeRevocations(ctx)
	}
	if err != nil {
		m.log().Error("revoking the certificates of an abandoned registered server", "server", r.ID, "error", err)
	}
}

func (m *Manager) hasActiveTask(ctx context.Context, eng *task.Engine, serverID string) bool {
	ts, err := eng.List(ctx, 200)
	if err != nil {
		return true
	}
	for _, t := range ts {
		if t.SubjectType == "server" && t.SubjectID == serverID && t.State.Active() {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------- the task

func (m *Manager) addDefinition() task.Definition {
	return task.Definition{Kind: KindAdd, Steps: []task.Step{
		{Name: "Wait for the new server to hold its certificate", Run: m.stepAddWait},
		{Name: "Prepare the instance on the new server", Run: m.stepAddPrepare},
		{Name: "Trust the new server on every server", Run: m.stepAddTrust},
		{Name: "Copy roles and databases", Run: m.stepAddGlobals},
		{Name: "Register the server in every sharded database", Irreversible: true, Run: m.stepAddRegister},
		{Name: "Record the server", Run: m.stepAddRecord},
		{Name: "Finish and distribute the server directory", Run: m.stepAddFinish},
	}, OnFail: m.addFailed}
}

func inputOf[T any](t task.Task) (T, error) {
	var v T
	if err := json.Unmarshal(t.Input, &v); err != nil {
		return v, fmt.Errorf("reading the task input: %w", err)
	}
	return v, nil
}

// call runs an internal call on a server (the local function when it is this
// server), waiting while the server cannot be reached (design D4: a server that
// is down makes the task wait and say so, it is never skipped).
func (m *Manager) waitUnreachable(ctx context.Context, env *task.Env, server, doing string, f func() error) error {
	announced := false
	for {
		err := f()
		if err == nil {
			if announced {
				_ = env.SetDetail(ctx, "")
			}
			return nil
		}
		if !errors.Is(err, peerclient.ErrUnreachable) {
			return err
		}
		if !announced {
			m.log().Warn("task waits for a server", "server", server, "doing", doing, "error", err)
			announced = true
		}
		_ = env.SetDetail(ctx, fmt.Sprintf("Waiting for server %s, which cannot be reached (%s).", server, doing))
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(3 * time.Second):
		}
	}
}

// do calls a leader-to-server internal call on server.
func (m *Manager) do(ctx context.Context, server, method, path string, in, out any) error {
	return m.Client.Do(ctx, server, method, path, in, out)
}

// doUnchecked is do for a server whose certificates may be revoked (see
// peerclient.DoUnchecked).
func (m *Manager) doUnchecked(ctx context.Context, server, method, path string, in, out any) error {
	return m.Client.DoUnchecked(ctx, server, method, path, in, out)
}

func (m *Manager) stepAddWait(ctx context.Context, env *task.Env) error {
	in, err := inputOf[addInput](env.Task)
	if err != nil {
		return err
	}
	fp := pki.FormatFingerprint(in.Fingerprint)
	mode := "recorded on first use; compare it with the fingerprint the new server logged"
	if in.Pinned {
		mode = "pinned by the administrator"
	}
	base := ""
	if in.Fingerprint != "" {
		base = fmt.Sprintf("Certificate of the new server seen on first contact, SHA-256 %s (%s).", fp, mode)
		_ = env.SetDetail(ctx, base)
	}
	m.ensurePeer(in)
	deadline := time.Now().Add(10 * time.Minute)
	var last error
	for time.Now().Before(deadline) {
		m.ensurePeer(in)
		var st wire.InstanceStatus
		if last = m.do(ctx, in.ServerID, "GET", "/instance/status", nil, &st); last == nil {
			return nil
		}
		var ae *peerclient.APIError
		if errors.As(last, &ae) && ae.Status == 401 {
			return fmt.Errorf("the new server refused our certificate: %w", last)
		}
		_ = env.SetDetail(ctx, strings.TrimSpace(base+fmt.Sprintf(" Waiting for %s to answer with its certificate of this cluster (last answer: %s).", in.ServerID, errorText(last))))
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
	return fmt.Errorf("the new server did not answer with its certificate: %w", last)
}

func (m *Manager) rules(ctx context.Context) ([]api.NetworkAccessRule, bool, error) {
	na, err := m.Ops.GetNetworkAccess(ctx)
	if err != nil {
		return nil, false, err
	}
	return na.Rules, na.TLSRequired, nil
}

func (m *Manager) stepAddPrepare(ctx context.Context, env *task.Env) error {
	in, err := inputOf[addInput](env.Task)
	if err != nil {
		return err
	}
	m.ensurePeer(in)
	ca, err := m.Svc.CA()
	if err != nil {
		return err
	}
	m.reconcileRevocations(ctx, ca)
	if err := m.Svc.PublishLocalCRL(ctx); err != nil {
		return fmt.Errorf("publishing the revocation list: %w", err)
	}
	crl, err := m.Svc.CRLPEM()
	if err != nil {
		return err
	}
	if err := m.do(ctx, in.ServerID, "POST", "/pki/install", wire.PKIRequest{CACert: string(ca.CertPEM()), Revoked: ca.Revoked(), CRL: string(crl)}, nil); err != nil {
		return fmt.Errorf("installing the revocation list on the new server: %w", err)
	}
	rules, tlsReq, err := m.rules(ctx)
	if err != nil {
		return err
	}
	infos, err := m.peerInfos(ctx, in.ServerID, "")
	if err != nil {
		return err
	}
	_, name, _ := m.clusterIdentity(ctx)
	if err := m.do(ctx, in.ServerID, "POST", "/instance/create", wire.CreateInstanceRequest{ClusterName: name, MemoryMegabytes: in.MemoryMB,
		Peers: infos, Rules: rules, TLSRequired: tlsReq}, nil); err != nil {
		return fmt.Errorf("creating the instance on the new server: %w", err)
	}
	return m.do(ctx, in.ServerID, "POST", "/peers", wire.PeersRequest{Peers: infos}, nil)
}

// trustEverywhere sends every active server (and the pending one named
// withPending, which already has its instance) the servers it must trust.
func (m *Manager) trustEverywhere(ctx context.Context, env *task.Env, withPending, without string) error {
	infos, err := m.peerInfos(ctx, withPending, without)
	if err != nil {
		return err
	}
	rules, tlsReq, err := m.rules(ctx)
	if err != nil {
		return err
	}
	req := wire.TrustRequest{Peers: infos, Rules: rules, TLSRequired: tlsReq}
	for _, p := range infos {
		if p.ServerID == withPending {
			continue // it was created with the full list
		}
		if err := m.waitUnreachable(ctx, env, p.ServerID, "applying the trust rules", func() error { return m.trustOn(ctx, p.ServerID, req) }); err != nil {
			return fmt.Errorf("applying the trust rules on %s: %w", p.ServerID, err)
		}
	}
	return nil
}

func (m *Manager) trustOn(ctx context.Context, server string, req wire.TrustRequest) error {
	if server == m.Self {
		return m.Svc.ApplyTrust(ctx, req)
	}
	return m.do(ctx, server, "POST", "/instance/trust", req, nil)
}

func (m *Manager) stepAddTrust(ctx context.Context, env *task.Env) error {
	in, err := inputOf[addInput](env.Task)
	if err != nil {
		return err
	}
	return m.trustEverywhere(ctx, env, in.ServerID, "")
}

func (m *Manager) stepAddGlobals(ctx context.Context, env *task.Env) error {
	in, err := inputOf[addInput](env.Task)
	if err != nil {
		return err
	}
	g, err := m.Svc.CollectGlobals(ctx)
	if err != nil {
		return fmt.Errorf("reading the roles and databases: %w", err)
	}
	if err := m.do(ctx, in.ServerID, "POST", "/globals/apply", g, nil); err != nil {
		return fmt.Errorf("copying the roles and databases to the new server: %w", err)
	}
	return nil
}

// shardedDatabases lists the databases of this server that use kingres_shard.
func (m *Manager) shardedDatabases(ctx context.Context) ([]string, error) {
	g, err := m.Svc.CollectGlobals(ctx)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, d := range g.Databases {
		if d.Sharded {
			out = append(out, d.Name)
		}
	}
	return out, nil
}

// registerNode registers host:port in database db with kingres.add_node.
func (m *Manager) registerNode(ctx context.Context, db, host string, port int) error {
	c, err := m.Svc.ServiceConn(ctx, db)
	if err != nil {
		return err
	}
	defer func() { _ = c.Close(context.WithoutCancel(ctx)) }()
	return registerNodeOn(ctx, c, db, host, port)
}

// registerNodeOn is registerNode on the session c of the service role, which is open to database db. The catalog is read and called as a superuser: its schema must
// be the extension's.
func registerNodeOn(ctx context.Context, c *pgx.Conn, db, host string, port int) error {
	if err := extgate.Require(ctx, c, extguard.Shard, db); err != nil {
		return err
	}
	var have bool
	if err := c.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM kingres.node WHERE host = $1 AND port = $2)`, host, port).Scan(&have); err != nil {
		return err
	}
	if have {
		return nil
	}
	if _, err := c.Exec(ctx, `SELECT kingres.add_node($1, $2)`, host, port); err != nil {
		return fmt.Errorf("registering the node in database %s: %w", db, cleanPG(err))
	}
	return nil
}

func cleanPG(err error) error {
	var pe interface{ SQLState() string }
	if errors.As(err, &pe) {
		return fmt.Errorf("SQLSTATE %s", pe.SQLState())
	}
	return errors.New("the statement failed")
}

func (m *Manager) stepAddRegister(ctx context.Context, env *task.Env) error {
	in, err := inputOf[addInput](env.Task)
	if err != nil {
		return err
	}
	// From here on a failure is not cleaned up: part of the catalog may know
	// the server, so the task rolls forward when it is started again.
	err = m.sys(ctx, func(c *pgx.Conn) error {
		_, err := c.Exec(ctx, `UPDATE kingres_cp.server SET registering = true WHERE server_id = $1`, in.ServerID)
		return err
	})
	if err != nil {
		return err
	}
	dbs, err := m.shardedDatabases(ctx)
	if err != nil {
		return err
	}
	for _, db := range dbs {
		if err := m.registerNode(ctx, db, in.ServerID, in.PGPort); err != nil {
			return err
		}
	}
	// The extensions that had to wait for kingres_shard are installed now.
	g, err := m.Svc.CollectGlobals(ctx)
	if err != nil {
		return fmt.Errorf("reading the databases: %w", err)
	}
	g.Roles = nil
	return m.do(ctx, in.ServerID, "POST", "/globals/apply", g, nil)
}

func (m *Manager) stepAddRecord(ctx context.Context, env *task.Env) error {
	in, err := inputOf[addInput](env.Task)
	if err != nil {
		return err
	}
	return m.sys(ctx, func(c *pgx.Conn) error {
		_, err := c.Exec(ctx, `UPDATE kingres_cp.server SET status = 'active', registering = false, version = $2, joined_at = now()
			WHERE server_id = $1 AND status = 'pending'`, in.ServerID, m.Version)
		return err
	})
}

func (m *Manager) stepAddFinish(ctx context.Context, env *task.Env) error {
	in, err := inputOf[addInput](env.Task)
	if err != nil {
		return err
	}
	id, name, err := m.clusterIdentity(ctx)
	if err != nil {
		return err
	}
	if err := m.do(ctx, in.ServerID, "POST", "/node/finish", wire.FinishRequest{ClusterID: id, ClusterName: name}, nil); err != nil {
		return fmt.Errorf("finishing the join on the new server: %w", err)
	}
	if err := m.distributePeers(ctx, env, ""); err != nil {
		return err
	}
	m.audit(ctx, env.Task.CreatedBy, in.Via+"Done", in.ServerID, audit.OutcomeSuccess, map[string]any{"taskId": env.Task.ID})
	return nil
}

// distributePeers sends the peer directory to every server.
func (m *Manager) distributePeers(ctx context.Context, env *task.Env, without string) error {
	infos, err := m.peerInfos(ctx, "", without)
	if err != nil {
		return err
	}
	list := make([]peers.Peer, len(infos))
	for i, p := range infos {
		list[i] = peers.Peer{ServerID: p.ServerID, InternalAddr: p.InternalAddress, Ordinal: p.Ordinal}
	}
	if d := m.Fault.DirectoryDelay(); d > 0 {
		m.log().Info("the fault hook holds the write of the peer directory", "for", d.String())
		time.Sleep(d)
		m.log().Info("writing the peer directory", "servers", len(list), "without", without)
	}
	if err := m.recordDirectory(list, without); err != nil {
		return err
	}
	for _, p := range infos {
		if p.ServerID == m.Self {
			continue
		}
		if err := m.waitUnreachable(ctx, env, p.ServerID, "sending the server directory", func() error {
			return m.do(ctx, p.ServerID, "POST", "/peers", wire.PeersRequest{Peers: infos}, nil)
		}); err != nil {
			return fmt.Errorf("sending the server directory to %s: %w", p.ServerID, err)
		}
	}
	return nil
}

// distributeRevocations sends the revocation list to every active server and
// publishes it to the local instance.
func (m *Manager) distributeRevocations(ctx context.Context) error {
	ca, err := m.Svc.CA()
	if err != nil {
		return err
	}
	// Before anything is sent: what the servers hold is merged in, so that this list is never smaller than theirs (a leader that was
	// away must not undo the revocations made while it was).
	m.reconcileRevocations(ctx, ca)
	if err := m.Svc.PublishLocalCRL(ctx); err != nil {
		return err
	}
	crl, err := m.Svc.CRLPEM()
	if err != nil {
		return err
	}
	ids, revoked, err := m.revocationTargets(ctx)
	if err != nil {
		return err
	}
	req := wire.PKIRequest{CACert: string(ca.CertPEM()), Revoked: ca.Revoked(), CRL: string(crl)}
	first := m.distributeAll(ids, revoked, func(id string) error { return m.installRevocations(ctx, id, ca, req) })
	m.escrowAfterRevocation(ctx)
	return first
}

// addFailed cleans up after a server.add that failed before the server was
// registered anywhere (design D4): the other servers stop trusting it, it is
// told to discard what it created, and what was signed for it is revoked.
func (m *Manager) addFailed(ctx context.Context, t task.Task) {
	in, err := inputOf[addInput](t)
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	row, err := m.getServer(ctx, in.ServerID)
	if err != nil || row.Status != StatusPending {
		return
	}
	if row.Registering {
		m.log().Warn("a failed server.add is not cleaned up because the server may be registered; add it again to finish", "server", in.ServerID)
		return
	}
	m.log().Warn("cleaning up after a failed server.add", "server", in.ServerID)
	// 1. The others stop trusting it.
	if infos, err := m.peerInfos(ctx, "", in.ServerID); err == nil {
		rules, tlsReq, rerr := m.rules(ctx)
		if rerr == nil {
			for _, p := range infos {
				if e := m.trustOn(ctx, p.ServerID, wire.TrustRequest{Peers: infos, Rules: rules, TLSRequired: tlsReq}); e != nil {
					m.log().Error("withdrawing trust after a failed server.add", "server", p.ServerID, "error", e)
				}
			}
		}
	}
	// 2. The new server discards what it created (while its certificate still works).
	if err := m.do(ctx, in.ServerID, "POST", "/node/reset", wire.Empty{}, nil); err != nil {
		m.log().Warn("the new server could not be told to discard its instance", "server", in.ServerID, "error", err)
	}
	// 3. What was signed for it is revoked.
	m.discardPending(ctx, in.ServerID)
}

// recordDirectory writes the leader's own copy of the peer directory. It adds and
// updates the servers of list and drops only the one that is being removed: the
// directory also holds a server that another task is adding right now (it is
// pending, so it is not in list), and a task that replaced the whole file from a
// list read a moment before made that task fail with "no internal address is
// known" or wait for a server it could no longer reach.
func (m *Manager) recordDirectory(list []peers.Peer, without string) error {
	for _, p := range list {
		if err := m.Peers.Put(p); err != nil {
			return err
		}
	}
	if without != "" {
		return m.Peers.Delete(without)
	}
	return nil
}

// ensurePeer puts the address of the server being added back into the directory
// when it is missing: the task knows it from its input, and a step that cannot
// find it would otherwise wait for a server it will never be able to call.
func (m *Manager) ensurePeer(in addInput) {
	if _, ok := m.Peers.Get(in.ServerID); ok || in.InternalAddress == "" {
		return
	}
	m.log().Warn("the address of a server that is being added was missing from the directory; recorded again", "server", in.ServerID)
	_ = m.Peers.Put(peers.Peer{ServerID: in.ServerID, InternalAddr: in.InternalAddress})
}
