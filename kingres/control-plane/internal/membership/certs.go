// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package membership

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/audit"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/internalapi"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/pki"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/problem"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/task"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/wire"
)

// Task kinds of certificate renewal and revocation (contract TaskKind
// certificate.renew and certificate.revoke); the subject is the server
// (type "server"), like the other kinds that concern a server.
const (
	KindCertRenew  = "certificate.renew"
	KindCertRevoke = "certificate.revoke"
)

// Audit actions that are events of the system, not operations of the contract
// (the contract's AuditAction lists them from 0.5.0 on).
const (
	AuditCertificateRenewed       = "certificateRenewed"
	AuditCertificateRenewalFailed = "certificateRenewalFailed"
)

// codeCAUnavailable is the code of the internal API's answer when the manager
// leader does not hold the CA key.
const codeCAUnavailable = "ca_unavailable"

// codeStaleRevocations is the answer of a server that refuses a revocation list that takes a revocation back.
const codeStaleRevocations = "stale_revocations"

type certInput struct {
	ServerID string `json:"serverId"`
}

// caProblem is the dedicated problem of an operation that needs the CA key
// when this server (the manager leader) does not hold it: the key is only on
// the server that created the cluster (README, "Not done in this increment, and
// the manager's state on a leader change").
func (m *Manager) caProblem() *problem.Problem {
	return problem.New(problem.CertificateAuthorityUnavailable,
		fmt.Sprintf("The manager leader %s does not hold the cluster certificate authority key, so certificates cannot be issued or revoked now. "+
			"The key is on the server that created the cluster; certificates keep working until they expire.", m.Self)).WithRetryAfter(60)
}

// ca returns the CA held by this server or the dedicated problem.
func (m *Manager) ca() (*pki.CA, error) {
	ca, err := m.Svc.CA()
	switch {
	case err == nil:
		return ca, nil
	case errors.Is(err, os.ErrNotExist):
		return nil, m.caProblem()
	}
	return nil, err
}

// liveSerials returns the serial numbers of the certificates of a server that
// may still be valid: those the CA issued and recorded whose end has not
// passed (or is not known), and the one recorded in the server's row.
func (m *Manager) liveSerials(ctx context.Context, id string) ([]string, error) {
	var out []string
	err := m.sys(ctx, func(c *pgx.Conn) error {
		rows, err := c.Query(ctx, `SELECT serial FROM kingres_cp.certificate
			WHERE server_id = $1 AND (not_after IS NULL OR not_after > now()) ORDER BY issued_at`, id)
		if err != nil {
			return err
		}
		out, err = pgx.CollectRows(rows, pgx.RowTo[string])
		return err
	})
	if err != nil {
		return nil, err
	}
	if row, err := m.getServer(ctx, id); err == nil && row.Serial != "" {
		have := false
		for _, s := range out {
			have = have || s == row.Serial
		}
		if !have {
			out = append(out, row.Serial)
		}
	}
	return out, nil
}

// revokeServerCertificates revokes every certificate of a server that may
// still be valid in the CA's list and marks the server's record.
func (m *Manager) revokeServerCertificates(ctx context.Context, ca *pki.CA, id string, also ...string) error {
	serials, err := m.liveSerials(ctx, id)
	if err != nil {
		return err
	}
	for _, s := range also {
		if s != "" {
			serials = append(serials, s)
		}
	}
	for _, s := range serials {
		if err := ca.Revoke(s, time.Now()); err != nil {
			return err
		}
	}
	return m.sys(ctx, func(c *pgx.Conn) error {
		_, err := c.Exec(ctx, `UPDATE kingres_cp.server SET certificate_revoked_at = coalesce(certificate_revoked_at, now()) WHERE server_id = $1`, id)
		return err
	})
}

// issued is a certificate the CA signed for a renewal.
type issued struct {
	CertPEM  string
	Serial   string
	NotAfter time.Time
}

// issueRenewal signs a new certificate for the server id from its certificate
// request and records it. The request must be made from the key the server
// holds: the CA never accepts another key for a renewal (a new key is a new
// join). The previous certificate stays valid until it expires (same key); it
// is revoked together with the others when the server's certificates are.
func (m *Manager) issueRenewal(ctx context.Context, id, csr string) (issued, error) {
	ca, err := m.ca()
	if err != nil {
		return issued{}, err
	}
	row, err := m.getServer(ctx, id)
	switch {
	case errors.Is(err, errNoServer):
		return issued{}, problem.New(problem.NotFound, "No such server.")
	case err != nil:
		return issued{}, err
	case row.RevokedAt != nil:
		return issued{}, problem.New(problem.CertificateRevoked,
			"The certificates of server "+id+" were revoked; it gets no new certificate. Remove it and join it again with a new setup token.")
	case row.Status != StatusActive:
		return issued{}, problem.New(problem.Conflict, "Server "+id+" is not an active member of the cluster.")
	}
	if len(row.PublicKey) > 0 {
		pub, perr := pki.CSRPublicKeyDER([]byte(csr))
		if perr != nil {
			return issued{}, problem.New(problem.Unprocessable, "The certificate request is not valid.")
		}
		if !bytes.Equal(pub, row.PublicKey) {
			return issued{}, problem.New(problem.Unprocessable, "The certificate request uses another key than the server was admitted with.")
		}
	}
	names := pki.Names{CommonName: id, DNS: []string{"localhost"}, IPs: serverIPs()}
	crt, serial, err := ca.Sign([]byte(csr), names, time.Now())
	if err != nil {
		return issued{}, problem.New(problem.Unprocessable, "The certificate request was refused.")
	}
	parsed, err := pki.ParseCertPEM(crt)
	if err != nil {
		return issued{}, err
	}
	err = m.sys(ctx, func(c *pgx.Conn) error {
		tx, err := c.Begin(ctx)
		if err != nil {
			return err
		}
		defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
		// The server must not have been revoked while the certificate was signed.
		var revoked *time.Time
		if err := tx.QueryRow(ctx, `SELECT certificate_revoked_at FROM kingres_cp.server WHERE server_id = $1 FOR UPDATE`, id).Scan(&revoked); err != nil {
			return err
		}
		if revoked != nil {
			return errRevokedMeanwhile
		}
		if _, err := tx.Exec(ctx, `UPDATE kingres_cp.server SET certificate_serial = $2, certificate_pem = $3, certificate_renewed_at = now() WHERE server_id = $1`,
			id, serial, string(crt)); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO kingres_cp.certificate (serial, server_id, not_after) VALUES ($1, $2, $3)`, serial, id, parsed.NotAfter); err != nil {
			return err
		}
		// Rows of certificates that expired long ago are of no use.
		if _, err := tx.Exec(ctx, `DELETE FROM kingres_cp.certificate WHERE server_id = $1 AND not_after < now() - interval '30 days'`, id); err != nil {
			return err
		}
		return tx.Commit(ctx)
	})
	if err != nil {
		// What was signed must not outlive a failed record.
		if rerr := ca.Revoke(serial, time.Now()); rerr != nil {
			m.log().Error("revoking a certificate whose record failed", "serial", serial, "error", rerr)
		}
		if errors.Is(err, errRevokedMeanwhile) {
			return issued{}, problem.New(problem.CertificateRevoked, "The certificates of server "+id+" were revoked meanwhile.")
		}
		return issued{}, err
	}
	return issued{CertPEM: string(crt), Serial: serial, NotAfter: parsed.NotAfter}, nil
}

var errRevokedMeanwhile = errors.New("revoked meanwhile")

// RenewFromPeer is the leader's side of POST /pki/renew: the server that the
// caller's certificate names asks for a new certificate. A server renews only
// itself, never another.
func (m *Manager) RenewFromPeer(ctx context.Context, peer string, req wire.RenewRequest) (wire.RenewResponse, error) {
	if peer == "" || req.CSR == "" || len(req.CSR) > 16<<10 {
		return wire.RenewResponse{}, problem.New(problem.BadRequest, "The call needs a certificate request.")
	}
	is, err := m.issueRenewal(ctx, peer, req.CSR)
	if err != nil {
		return wire.RenewResponse{}, err
	}
	res, err := m.renewalAnswer(ctx, is)
	if err != nil {
		return wire.RenewResponse{}, err
	}
	m.audit(ctx, "system", AuditCertificateRenewed, peer, audit.OutcomeSuccess, map[string]any{"via": "renewal job", "serial": is.Serial, "notAfter": is.NotAfter.UTC().Format(time.RFC3339)})
	return res, nil
}

// renewalAnswer builds the answer of a renewal: the certificate with the CA
// certificate and the current revocation list.
func (m *Manager) renewalAnswer(ctx context.Context, is issued) (wire.RenewResponse, error) {
	ca, err := m.ca()
	if err != nil {
		return wire.RenewResponse{}, err
	}
	// The list that goes out with a certificate is at least what the servers hold.
	m.reconcileRevocations(ctx, ca)
	crl, err := m.Svc.CRLPEM()
	if err != nil {
		return wire.RenewResponse{}, err
	}
	return wire.RenewResponse{CACert: string(ca.CertPEM()), ServerCert: is.CertPEM, Serial: is.Serial, NotAfter: is.NotAfter,
		Revoked: ca.Revoked(), CRL: string(crl)}, nil
}

// RenewSelf renews the certificate of this server when it holds the CA: the
// leader signs its own request without the network.
func (m *Manager) RenewSelf(ctx context.Context) (notAfter time.Time, err error) {
	csr, err := m.Svc.RenewalCSR()
	if err != nil {
		return time.Time{}, err
	}
	is, err := m.issueRenewal(ctx, m.Self, string(csr))
	if err != nil {
		return time.Time{}, err
	}
	res, err := m.renewalAnswer(ctx, is)
	if err != nil {
		return time.Time{}, err
	}
	if err := m.installRenewed(ctx, res); err != nil {
		return time.Time{}, err
	}
	m.audit(ctx, "system", AuditCertificateRenewed, m.Self, audit.OutcomeSuccess, map[string]any{"via": "renewal job", "serial": is.Serial, "notAfter": is.NotAfter.UTC().Format(time.RFC3339)})
	return is.NotAfter, nil
}

// RenewViaLeader renews the certificate of this server through the internal API
// of the manager leader, which holds the CA.
func (m *Manager) RenewViaLeader(ctx context.Context, leaderID string) (time.Time, error) {
	if leaderID == m.Self {
		return m.RenewSelf(ctx)
	}
	csr, err := m.Svc.RenewalCSR()
	if err != nil {
		return time.Time{}, err
	}
	var res wire.RenewResponse
	if err := m.do(ctx, leaderID, "POST", "/pki/renew", wire.RenewRequest{CSR: string(csr)}, &res); err != nil {
		return time.Time{}, err
	}
	if err := m.installRenewed(ctx, res); err != nil {
		return time.Time{}, err
	}
	return res.NotAfter, nil
}

// installRenewed stores a renewed certificate on this server and makes the
// connections that were opened with the old one go away.
func (m *Manager) installRenewed(ctx context.Context, res wire.RenewResponse) error {
	if err := m.Svc.InstallRenewed(ctx, res.CACert, res.ServerCert, res.CRL, res.Revoked); err != nil {
		return err
	}
	if m.OnCertificateInstalled != nil {
		m.OnCertificateInstalled()
	}
	return nil
}

// ------------------------------------------------------------ operations

// RenewServerCertificates is POST /servers/{serverId}/certificates/renewals on
// the leader: the task certificate.renew gives the server a new certificate now.
func (m *Manager) RenewServerCertificates(ctx context.Context, actor, id string) (task.Task, error) {
	if _, err := m.ca(); err != nil {
		return task.Task{}, err
	}
	row, err := m.getServer(ctx, id)
	switch {
	case errors.Is(err, errNoServer):
		return task.Task{}, problem.New(problem.NotFound, "No such server.")
	case err != nil:
		return task.Task{}, err
	case row.RevokedAt != nil:
		return task.Task{}, problem.New(problem.CertificateRevoked,
			"The certificates of server "+id+" were revoked; it gets no new certificate. Remove it and join it again with a new setup token.")
	case row.Status != StatusActive:
		return task.Task{}, problem.New(problem.Conflict, "Server "+id+" is not an active member of the cluster.")
	}
	t, err := m.Ops.StartTask(ctx, KindCertRenew, "server", id, certInput{ServerID: id}, actor)
	if errors.Is(err, task.ErrSubjectBusy) {
		return t, problem.New(problem.OperationInProgress, "Another operation on this server is running.")
	}
	return t, err
}

// RevokeServerCertificates is POST /servers/{serverId}/certificates/revocations
// on the leader: the task certificate.revoke revokes every certificate of the
// server and sends the revocation list to every server and instance.
func (m *Manager) RevokeServerCertificates(ctx context.Context, actor, id string) (task.Task, error) {
	if _, err := m.ca(); err != nil {
		return task.Task{}, err
	}
	row, err := m.getServer(ctx, id)
	switch {
	case errors.Is(err, errNoServer):
		return task.Task{}, problem.New(problem.NotFound, "No such server.")
	case err != nil:
		return task.Task{}, err
	case id == m.Self:
		return task.Task{}, problem.New(problem.ServerHostsMapGroup,
			"Server "+id+" is the manager leader and holds the certificate authority; revoking its certificates would lock the cluster out.")
	case row.RevokedAt != nil:
		return task.Task{}, problem.New(problem.CertificateRevoked, "The certificates of server "+id+" are revoked already.")
	case row.Status != StatusActive:
		return task.Task{}, problem.New(problem.Conflict, "Server "+id+" is not an active member of the cluster.")
	}
	t, err := m.Ops.StartTask(ctx, KindCertRevoke, "server", id, certInput{ServerID: id}, actor)
	if errors.Is(err, task.ErrSubjectBusy) {
		return t, problem.New(problem.OperationInProgress, "Another operation on this server is running.")
	}
	return t, err
}

func (m *Manager) certRenewDefinition() task.Definition {
	return task.Definition{Kind: KindCertRenew, Steps: []task.Step{
		{Name: "Check the server", Run: m.stepCertRenewCheck},
		{Name: "Issue the new certificate and install it on the server", Run: m.stepCertRenewIssue},
	}}
}

func (m *Manager) certRevokeDefinition() task.Definition {
	return task.Definition{Kind: KindCertRevoke, Steps: []task.Step{
		{Name: "Revoke the certificates of the server", Run: m.stepCertRevoke},
		{Name: "Send the revocation list to every server", Run: m.stepCertRevokeDistribute},
		{Name: "Close the connections to the server", Run: m.stepCertRevokeClose},
	}}
}

func (m *Manager) stepCertRenewCheck(ctx context.Context, env *task.Env) error {
	in, err := inputOf[certInput](env.Task)
	if err != nil {
		return err
	}
	if _, err := m.ca(); err != nil {
		return err
	}
	row, err := m.getServer(ctx, in.ServerID)
	if err != nil {
		return err
	}
	if row.RevokedAt != nil {
		return errors.New("the certificates of this server were revoked meanwhile")
	}
	return nil
}

func (m *Manager) stepCertRenewIssue(ctx context.Context, env *task.Env) error {
	in, err := inputOf[certInput](env.Task)
	if err != nil {
		return err
	}
	if in.ServerID == m.Self {
		na, err := m.RenewSelf(ctx)
		if err != nil {
			return err
		}
		m.auditRenewOp(ctx, env, in.ServerID, na)
		return nil
	}
	var cres wire.CSRResponse
	err = m.waitUnreachable(ctx, env, in.ServerID, "asking it for a certificate request", func() error {
		return m.do(ctx, in.ServerID, "GET", "/pki/csr", nil, &cres)
	})
	if err != nil {
		return fmt.Errorf("asking server %s for a certificate request: %w", in.ServerID, err)
	}
	is, err := m.issueRenewal(ctx, in.ServerID, cres.CSR)
	if err != nil {
		return err
	}
	res, err := m.renewalAnswer(ctx, is)
	if err != nil {
		return err
	}
	err = m.waitUnreachable(ctx, env, in.ServerID, "installing the certificate", func() error {
		return m.do(ctx, in.ServerID, "POST", "/pki/install", wire.PKIRequest{CACert: res.CACert, ServerCert: res.ServerCert, Revoked: res.Revoked, CRL: res.CRL}, nil)
	})
	if err != nil {
		return fmt.Errorf("installing the new certificate on server %s: %w", in.ServerID, err)
	}
	m.Client.Forget(in.ServerID)
	m.auditRenewOp(ctx, env, in.ServerID, is.NotAfter)
	return nil
}

func (m *Manager) auditRenewOp(ctx context.Context, env *task.Env, id string, notAfter time.Time) {
	m.audit(ctx, env.Task.CreatedBy, "renewServerCertificates", id, audit.OutcomeSuccess,
		map[string]any{"phase": "completed", "taskId": env.Task.ID, "notAfter": notAfter.UTC().Format(time.RFC3339)})
}

func (m *Manager) stepCertRevoke(ctx context.Context, env *task.Env) error {
	in, err := inputOf[certInput](env.Task)
	if err != nil {
		return err
	}
	ca, err := m.ca()
	if err != nil {
		return err
	}
	return m.revokeServerCertificates(ctx, ca, in.ServerID)
}

// stepCertRevokeDistribute sends the revocation list to every server, the
// revoked one included (it accepts the leader's certificate), and publishes it
// to the local instance. A server that cannot be reached makes the task wait,
// as in every other cluster-wide task: the list reaches it when it is back.
func (m *Manager) stepCertRevokeDistribute(ctx context.Context, env *task.Env) error {
	ca, err := m.ca()
	if err != nil {
		return err
	}
	m.reconcileRevocations(ctx, ca)
	if err := m.Svc.PublishLocalCRL(ctx); err != nil {
		return err
	}
	crl, err := m.Svc.CRLPEM()
	if err != nil {
		return err
	}
	// The servers are the active members of the table: the address of a server is the peer directory's business (the row of the server that
	// created the cluster has none, and after a failover of the map group that server is one of the others).
	ids, revoked, err := m.revocationTargets(ctx)
	if err != nil {
		return err
	}
	req := wire.PKIRequest{CACert: string(ca.CertPEM()), Revoked: ca.Revoked(), CRL: string(crl)}
	// A server that refuses does not stop the others (the revoked ones are last and have no say); a server that cannot be reached is waited for, as
	// in every cluster-wide task, but a revoked one is not waited for.
	err = m.distributeAll(ids, revoked, func(id string) error {
		if revoked[id] {
			return m.installRevocations(ctx, id, ca, req)
		}
		return m.waitUnreachable(ctx, env, id, "sending the revocation list", func() error { return m.installRevocations(ctx, id, ca, req) })
	})
	m.escrowAfterRevocation(ctx)
	return err
}

func (m *Manager) stepCertRevokeClose(ctx context.Context, env *task.Env) error {
	in, err := inputOf[certInput](env.Task)
	if err != nil {
		return err
	}
	m.Client.Forget(in.ServerID)
	m.audit(ctx, env.Task.CreatedBy, "revokeServerCertificates", in.ServerID, audit.OutcomeSuccess, map[string]any{"phase": "completed", "taskId": env.Task.ID})
	return nil
}

// ----------------------------------------------- receiving side (any server)

// handleCertCall answers the certificate calls of the internal API.
func (r *Receiver) handleCertCall(ctx context.Context, key, peer string, body []byte) (any, bool, error) {
	m := r.M
	switch key {
	case "POST /pki/renew":
		req, err := decode[wire.RenewRequest](body)
		if err != nil {
			return nil, true, err
		}
		res, err := m.RenewFromPeer(ctx, peer, req)
		return res, true, caCallError(err)
	case "GET /pki/csr":
		csr, err := m.Svc.RenewalCSR()
		if err != nil {
			return nil, true, err
		}
		return wire.CSRResponse{CSR: string(csr)}, true, nil
	}
	return nil, false, nil
}

// caCallError turns the CA problem into the internal API's own code, which
// the renewal job of the calling server recognizes.
func caCallError(err error) error {
	var p *problem.Problem
	if errors.As(err, &p) && p.Code == problem.CertificateAuthorityUnavailable {
		return &internalapi.CallError{Status: http.StatusServiceUnavailable, Code: codeCAUnavailable, Message: p.Detail}
	}
	return toCallError(err)
}

// serverIPs are the addresses besides the server's own name that its
// certificate carries (the names introduce gives to every server).
func serverIPs() []net.IP { return []net.IP{net.ParseIP("127.0.0.1")} }
