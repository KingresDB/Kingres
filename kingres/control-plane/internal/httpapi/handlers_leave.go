// SPDX-License-Identifier: PostgreSQL

package httpapi

import (
	"bytes"
	"context"
	"io"
	"net/http"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/problem"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/task"
)

// Leaver is implemented by the Members of a server that can leave its cluster
// (package membership).
type Leaver interface {
	LeaveCluster(ctx context.Context, actor string, force bool) (task.Task, error)
}

// leaveCluster is POST /this-server/leave-cluster: this server asks the
// manager leader to detach it. It is answered here, not forwarded: the server
// that leaves is the one that was asked, and the leader learns who asks from
// the certificate of this server. The answer is the task of the leader.
func (s *Server) leaveCluster(c *call, w http.ResponseWriter, r *http.Request) error {
	m, err := s.needMembers()
	if err != nil {
		return err
	}
	l, ok := m.(Leaver)
	if !ok {
		return problem.New(problem.UnsupportedOp, "This server cannot leave a cluster (no internal API or instance management).")
	}
	var req leaveRequest
	r.Body = http.MaxBytesReader(w, r.Body, s.deps.MaxBodyBytes)
	raw, rerr := io.ReadAll(r.Body)
	if rerr != nil {
		return problem.New(problem.BadRequest, "The request body could not be read.")
	}
	if len(raw) > 0 { // the body is optional
		if p := requireJSON(r); p != nil {
			return p
		}
		r.Body = io.NopCloser(bytes.NewReader(raw))
		if p := s.decodeJSON(w, r, &req); p != nil {
			return p
		}
	}
	force := req.Force != nil && *req.Force
	c.details = map[string]any{"force": force}
	t, err := l.LeaveCluster(r.Context(), c.session.Username, force)
	if err != nil {
		return err
	}
	s.accepted(w, r, t)
	return nil
}

// leaveRequest is the contract's LeaveClusterRequest.
type leaveRequest struct {
	Force *bool `json:"force"`
}
