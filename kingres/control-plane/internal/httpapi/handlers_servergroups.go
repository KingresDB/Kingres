// SPDX-License-Identifier: PostgreSQL

package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/api"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/internalapi"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/ops"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/pki"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/problem"
)

// serverGroupHandlers are the operations on server groups and the update of a
// server. All three change cluster state and so run on the manager leader (the
// guard forwards them there); the groups live in the leader's system database.
func (s *Server) serverGroupHandlers() map[string]handlerFunc {
	return map[string]handlerFunc{
		"createServerGroup": s.createServerGroup,
		"deleteServerGroup": s.deleteServerGroup,
		"updateServer":      s.updateServer,
	}
}

// bodyObject reads the request body as one JSON object, so that a handler can
// tell an absent member from a null one and answer with the contract's
// validation_failed for an unknown member.
func (s *Server) bodyObject(w http.ResponseWriter, r *http.Request) (map[string]json.RawMessage, error) {
	if p := requireJSON(r); p != nil {
		return nil, p
	}
	r.Body = http.MaxBytesReader(w, r.Body, s.deps.MaxBodyBytes)
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			return nil, problem.New(problem.BadRequest, "The request body is too large.")
		}
		return nil, problem.New(problem.BadRequest, "The request body could not be read.")
	}
	if len(raw) == 0 {
		return nil, problem.New(problem.BadRequest, "The request body is empty.")
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil || obj == nil {
		return nil, problem.New(problem.BadRequest, "The request body must be a JSON object.")
	}
	return obj, nil
}

func (s *Server) createServerGroup(c *call, w http.ResponseWriter, r *http.Request) error {
	o, err := s.needOps()
	if err != nil {
		return err
	}
	raw, err := s.bodyObject(w, r)
	if err != nil {
		return err
	}
	req, errs := ops.ParseServerGroupCreate(raw)
	if len(errs) > 0 {
		return problem.Validation("The server group is invalid.", errs...)
	}
	g, err := o.CreateServerGroup(r.Context(), req)
	if err != nil {
		return err
	}
	c.addDetails(map[string]any{"name": g.Name})
	s.writeJSON(w, r, http.StatusCreated, g)
	return nil
}

func (s *Server) deleteServerGroup(c *call, w http.ResponseWriter, r *http.Request) error {
	o, err := s.needOps()
	if err != nil {
		return err
	}
	name := r.PathValue("serverGroupName")
	if !ops.ValidGroupName(name) {
		return problem.New(problem.NotFound, "Server group "+quoted(name)+" does not exist.")
	}
	if err := o.DeleteServerGroup(r.Context(), name); err != nil {
		return err
	}
	c.addDetails(map[string]any{"name": name})
	w.WriteHeader(http.StatusNoContent)
	return nil
}

// addDetails adds audit details without dropping the ones the guard set (a
// forwarded request carries forwardedBy and originRequestId).
func (c *call) addDetails(kv map[string]any) {
	if c.details == nil {
		c.details = map[string]any{}
	}
	for k, v := range kv {
		c.details[k] = v
	}
}

// quoted shortens and quotes a name taken from a request for a message.
func quoted(n string) string {
	if len(n) > 64 {
		n = n[:64] + "..."
	}
	return strconv.Quote(n)
}

func (s *Server) updateServer(c *call, w http.ResponseWriter, r *http.Request) error {
	o, err := s.needOps()
	if err != nil {
		return err
	}
	pp := newParams(r)
	id := pp.pathServerID("serverId")
	if p := pp.err(); p != nil {
		return p
	}
	raw, err := s.bodyObject(w, r)
	if err != nil {
		return err
	}
	req, errs := ops.ParseServerUpdate(raw)
	if len(errs) > 0 {
		return problem.Validation("The server update is invalid.", errs...)
	}
	cur, err := s.deps.Store.Server(r.Context(), id)
	if err != nil {
		return s.storeProblem(r, err, "Server "+quoted(id)+" does not exist.")
	}
	if err := o.UpdateServer(r.Context(), id, ops.ServerPlacement{ShouldHaveShards: cur.ShouldHaveShards}, req); err != nil {
		return err
	}
	det := map[string]any{}
	if req.ServerGroupSet {
		det["serverGroup"] = req.ServerGroup
	}
	if req.ShouldHaveShards != nil {
		det["shouldHaveShards"] = *req.ShouldHaveShards
	}
	c.addDetails(det)
	out, err := s.deps.Store.Server(r.Context(), id)
	if err != nil {
		return s.storeProblem(r, err, "Server "+quoted(id)+" does not exist.")
	}
	s.completeServers(c, r, []*api.Server{&out.Server})
	s.writeJSON(w, r, http.StatusOK, out.Server)
	return nil
}

// forwardedReads are reads that a server asks the leader for, as part of its
// own answer, although the public operation is answered by the server asked
// (see completeServers); the leader accepts them over the forward call only.
var forwardedReads = map[string]bool{"listServers": true, "listDatabases": true, "getDatabase": true, "listLedgerStatus": true, "getLedgerStatus": true}

// completeServers adds to servers what this server cannot read itself. The
// manager leader reads the groups, the fingerprint recorded at the first
// contact and the certificate of every server from its system database (the
// store does); a follower asks the leader, with the caller's identity, and
// takes those values from the leader's list of servers. The certificate of this
// very server is always read from its own file. A leader that cannot be reached
// leaves the values the store had (the group of the shard catalog, which
// kingresd does not write, and null); the answer is not refused for it.
func (s *Server) completeServers(c *call, r *http.Request, servers []*api.Server) {
	if fw := s.deps.Forwarder; fw != nil && c != nil && c.session != nil && forwardedOf(r) == nil {
		s.completeFromLeader(c, r, fw, servers)
	}
	s.fillUptimes(r.Context(), servers)
	if id := s.localServerID(r.Context()); id != nil {
		if exp := s.localCertExpiry(); exp != nil {
			for _, sv := range servers {
				if sv.ID == *id {
					sv.CertificateExpiresAt = exp
				}
			}
		}
	}
}

// fillUptimes asks the servers whose uptime this server does not know (every
// server but itself) for it, in parallel and within two seconds; a server that
// does not answer keeps a null uptime, which the contract allows ("the server's
// agent is not reachable").
func (s *Server) fillUptimes(ctx context.Context, servers []*api.Server) {
	if s.deps.PeerUptime == nil {
		return
	}
	var wg sync.WaitGroup
	cctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	for _, sv := range servers {
		if sv.UptimeSeconds != nil || sv.Status == "dead" || sv.Status == "suspect" {
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			if up, err := s.deps.PeerUptime(cctx, sv.ID); err == nil && up >= 0 {
				sv.UptimeSeconds = &up
			}
		}()
	}
	wg.Wait()
}

func (s *Server) completeFromLeader(c *call, r *http.Request, fw Forwarder, servers []*api.Server) {
	var list api.ServerList
	if !s.askLeaderRead(c, r, fw, "listServers", nil, &list) {
		return
	}
	byID := make(map[string]api.Server, len(list.Items))
	for _, it := range list.Items {
		byID[it.ID] = it
	}
	for _, sv := range servers {
		if l, ok := byID[sv.ID]; ok {
			sv.ServerGroup, sv.SeenFingerprint, sv.FingerprintPinned = l.ServerGroup, l.SeenFingerprint, l.FingerprintPinned
			sv.CertificateExpiresAt = l.CertificateExpiresAt
			sv.CertificateRevokedAt = l.CertificateRevokedAt
			sv.JoinedAt = l.JoinedAt
		}
	}
}

// localCertExpiry reads notAfter of this server's certificate from its file;
// nil when there is none (not clustered yet) or it cannot be read.
func (s *Server) localCertExpiry() *api.Time {
	if s.deps.DataDir == "" {
		return nil
	}
	b, err := os.ReadFile(filepath.Join(s.deps.DataDir, "pki", pki.ServerCertFile))
	if err != nil {
		return nil
	}
	cert, err := pki.ParseCertPEM(b)
	if err != nil {
		return nil
	}
	t := cert.NotAfter.UTC()
	return &t
}

// askLeaderRead asks the manager leader for a read operation with the caller's
// identity and decodes its answer into out. It reports false, and leaves out as
// it was, when this server is the leader, the leader is not known or cannot be
// reached, or its answer is not the expected one: the caller then keeps what
// its own store answered.
func (s *Server) askLeaderRead(c *call, r *http.Request, fw Forwarder, op string, params map[string]string, out any) bool {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	leader, self, err := fw.Leader(ctx)
	if err != nil || leader == "" || self {
		return false
	}
	if params == nil {
		params = map[string]string{}
	}
	resp, err := fw.Forward(ctx, leader, internalapi.ForwardRequest{
		OperationID: op, PathParams: params,
		Identity:  identityOf(c.session),
		RequestID: stateOf(r).id, SourceAddress: c.ip, Source: "panel",
	})
	if err != nil || resp.Status != http.StatusOK {
		s.log.Warn("a read of the leader could not be completed", "request_id", stateOf(r).id, "leader", leader, "operation", op, "error", err, "status", resp.Status)
		return false
	}
	if err := json.Unmarshal(resp.Body, out); err != nil {
		s.log.Warn("the leader's answer is not valid", "request_id", stateOf(r).id, "leader", leader, "operation", op, "error", err)
		return false
	}
	return true
}

// canAskLeader tells whether a follower may complete its answer from the leader.
func (s *Server) canAskLeader(c *call, r *http.Request) (Forwarder, bool) {
	fw := s.deps.Forwarder
	return fw, fw != nil && c != nil && c.session != nil && forwardedOf(r) == nil
}

// The control plane records what it knows about a database (when and by whom it
// was created, the bucket count, the ledger mode and digest interval) in the
// system database of the leader only, so the catalogs of a follower cannot
// answer them: they would be null where the leader's answer has a value. A
// follower completes those fields from the leader's answer and keeps what only
// it knows (its own sizes, connections and table count).
func (s *Server) completeDatabases(c *call, r *http.Request, op string, params map[string]string, dbs []api.Database) {
	fw, ok := s.canAskLeader(c, r)
	if !ok || len(dbs) == 0 {
		return
	}
	var list api.DatabaseList
	if op == "getDatabase" {
		var one api.Database
		if !s.askLeaderRead(c, r, fw, op, params, &one) {
			return
		}
		list.Items = []api.Database{one}
	} else if !s.askLeaderRead(c, r, fw, op, params, &list) {
		return
	}
	byName := make(map[string]api.Database, len(list.Items))
	for _, d := range list.Items {
		byName[d.Name] = d
	}
	for i := range dbs {
		l, ok := byName[dbs[i].Name]
		if !ok {
			continue
		}
		if dbs[i].Sharding.BucketCount == nil {
			dbs[i].Sharding.BucketCount = l.Sharding.BucketCount
		}
		if dbs[i].Ledger.Mode == nil || (dbs[i].Ledger.Mode != nil && *dbs[i].Ledger.Mode == "off" && l.Ledger.Mode != nil) {
			dbs[i].Ledger = l.Ledger
		}
		dbs[i].CreatedBy, dbs[i].CreatedAt = l.CreatedBy, l.CreatedAt
	}
}

// completeLedgerStatus does the same for the mode and digest interval of the
// ledger status.
func (s *Server) completeLedgerStatus(c *call, r *http.Request, op string, params map[string]string, sts []api.LedgerStatus) {
	fw, ok := s.canAskLeader(c, r)
	if !ok || len(sts) == 0 {
		return
	}
	var list api.LedgerStatusList
	if op == "getLedgerStatus" {
		var one api.LedgerStatus
		if !s.askLeaderRead(c, r, fw, op, params, &one) {
			return
		}
		list.Items = []api.LedgerStatus{one}
	} else if !s.askLeaderRead(c, r, fw, op, params, &list) {
		return
	}
	byName := make(map[string]api.LedgerStatus, len(list.Items))
	for _, l := range list.Items {
		byName[l.DatabaseName] = l
	}
	for i := range sts {
		if l, ok := byName[sts[i].DatabaseName]; ok && sts[i].Mode == nil {
			sts[i].Mode, sts[i].DigestIntervalSeconds = l.Mode, l.DigestIntervalSeconds
		}
	}
}
