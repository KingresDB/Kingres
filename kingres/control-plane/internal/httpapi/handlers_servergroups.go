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
var forwardedReads = map[string]bool{"listServers": true}

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

func (s *Server) completeFromLeader(c *call, r *http.Request, fw Forwarder, servers []*api.Server) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	leader, self, err := fw.Leader(ctx)
	if err != nil || leader == "" || self {
		return
	}
	resp, err := fw.Forward(ctx, leader, internalapi.ForwardRequest{
		OperationID: "listServers", PathParams: map[string]string{},
		Identity:  internalapi.ForwardIdentity{Username: c.session.Username, ClusterRoles: c.session.ClusterRoles},
		RequestID: stateOf(r).id, SourceAddress: c.ip, Source: "panel",
	})
	if err != nil || resp.Status != http.StatusOK {
		s.log.Warn("the leader's server records could not be read", "request_id", stateOf(r).id, "leader", leader, "error", err, "status", resp.Status)
		return
	}
	var list api.ServerList
	if err := json.Unmarshal(resp.Body, &list); err != nil {
		s.log.Warn("the leader's server list is not valid", "request_id", stateOf(r).id, "leader", leader, "error", err)
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
