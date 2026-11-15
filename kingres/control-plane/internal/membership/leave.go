// SPDX-License-Identifier: PostgreSQL

package membership

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/audit"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/peerclient"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/problem"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/task"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/wire"
)

// KindLeave is the task kind of a server leaving the cluster by its own
// request (contract TaskKind server.leave); its subject is the server
// (type "server", the id of the server that leaves), like server.remove.
const KindLeave = "server.leave"

// Timings of the end of a leave. They are variables so that tests can shorten
// them.
var (
	// leavePoll is the interval at which the leaving server asks the leader
	// how its task stands.
	leavePoll = 500 * time.Millisecond
	// leaveGrace is how long the leaving server waits, once the leader has
	// refused its (revoked) certificate, for the leader to record the task as
	// done before it resets itself.
	leaveGrace = 4 * time.Second
	// leaveForcedDelay is the time a forced local leave gives the answer to
	// reach the client before the server resets itself.
	leaveForcedDelay = time.Second
)

// leaveInput is the input of a server.leave task. It has the fields of
// removeInput, so that the steps shared with server.remove read it.
type leaveInput struct {
	ServerID string `json:"serverId"`
	// Serial is the serial number of the certificate to revoke (public).
	Serial string `json:"serial,omitempty"`
	Force  bool   `json:"force,omitempty"`
}

// ---------------------------------------------------------------- leader

// LeaveFromPeer is the leader's side of POST /leave-cluster: the server peer
// (named by its certificate, never by the body) asks to leave. The leader
// decides from its own catalog with the order of the contract and starts the
// task.
func (m *Manager) LeaveFromPeer(ctx context.Context, peer string, req wire.LeaveRequest) (wire.LeaveResponse, error) {
	if peer == "" || req.Actor == "" || len(req.Actor) > 63 {
		return wire.LeaveResponse{}, problem.New(problem.BadRequest, "The call needs the administrator who asked.")
	}
	if _, err := m.ca(); err != nil {
		return wire.LeaveResponse{}, err
	}
	f, err := m.leaderFacts(ctx, peer)
	if err != nil {
		m.log().Warn("reading the catalog for a leave failed", "server", peer, "error", err)
		return wire.LeaveResponse{}, problem.New(problem.ServiceUnavailable, "The catalog cannot be read now.").WithRetryAfter(5)
	}
	if peer == m.Self {
		f.HostsMapGroup = true
	}
	if p := decideRemoval(peer, true, f); p != nil {
		return wire.LeaveResponse{}, p
	}
	in := leaveInput{ServerID: peer, Force: req.Force}
	if row, rerr := m.getServer(ctx, peer); rerr == nil {
		in.Serial = row.Serial
	}
	t, err := m.Ops.StartTask(ctx, KindLeave, "server", peer, in, req.Actor)
	if errors.Is(err, task.ErrSubjectBusy) {
		return wire.LeaveResponse{}, problem.New(problem.OperationInProgress, "This server is already being added, removed or detached.")
	}
	if err != nil {
		return wire.LeaveResponse{}, err
	}
	m.audit(ctx, req.Actor, "leaveCluster", peer, audit.OutcomeSuccess, map[string]any{"taskId": t.ID, "phase": "accepted", "requestedBy": peer, "force": req.Force})
	res := wire.LeaveResponse{TaskID: t.ID, Kind: t.Kind, State: string(t.State), CreatedAt: t.CreatedAt, CreatedBy: t.CreatedBy, Leader: m.Self}
	for _, s := range t.Steps {
		res.Steps = append(res.Steps, s.Name)
	}
	return res, nil
}

// LeaveStatus is the leader's side of POST /leave-status: the state of a
// leave task, for the server that is its subject only.
func (m *Manager) LeaveStatus(ctx context.Context, peer string, req wire.LeaveStatusRequest) (wire.LeaveStatusResponse, error) {
	eng := m.Ops.Engine()
	if eng == nil {
		return wire.LeaveStatusResponse{}, problem.New(problem.ServiceUnavailable, "The task engine is not running yet.").WithRetryAfter(5)
	}
	t, err := eng.Get(ctx, req.TaskID)
	if err != nil || t.Kind != KindLeave || t.SubjectType != "server" || t.SubjectID != peer {
		// The same answer for a task that does not exist and for one that
		// belongs to another server.
		return wire.LeaveStatusResponse{}, problem.New(problem.NotFound, "No such leave task for this server.")
	}
	return wire.LeaveStatusResponse{State: string(t.State)}, nil
}

func (m *Manager) leaveDefinition() task.Definition {
	return task.Definition{Kind: KindLeave, Steps: []task.Step{
		{Name: "Check that the server may leave", Run: m.stepLeaveCheck},
		{Name: "Remove the standbys of the server", Run: m.stepRemoveStandbys},
		{Name: "Unregister the server in every database", Run: m.stepRemoveUnregister},
		{Name: "Remove the server from the managed rules of the others", Run: m.stepRemoveTrust},
		{Name: "Forget the server", Run: m.stepLeaveForget},
		// Last: once the certificate is revoked the leaving server can no
		// longer ask the leader anything, and resets itself.
		{Name: "Revoke the certificate and distribute the revocation list", Run: m.stepLeaveRevoke},
	}}
}

func (m *Manager) stepLeaveCheck(ctx context.Context, env *task.Env) error {
	in, err := inputOf[leaveInput](env.Task)
	if err != nil {
		return err
	}
	if _, err := m.getServer(ctx, in.ServerID); errors.Is(err, errNoServer) {
		return nil // an earlier run already finished
	}
	f, err := m.leaderFacts(ctx, in.ServerID)
	if err != nil {
		return err
	}
	// This task is the operation; a rebalance was checked when it started.
	f.OperationRunning, f.RebalanceRunning = false, false
	if in.ServerID == m.Self {
		f.HostsMapGroup = true
	}
	if p := decideRemoval(in.ServerID, true, f); p != nil {
		return errors.New(p.Detail)
	}
	return m.sys(ctx, func(c *pgx.Conn) error {
		_, err := c.Exec(ctx, `UPDATE kingres_cp.server SET status = 'removing' WHERE server_id = $1`, in.ServerID)
		return err
	})
}

func (m *Manager) stepLeaveForget(ctx context.Context, env *task.Env) error {
	in, err := inputOf[leaveInput](env.Task)
	if err != nil {
		return err
	}
	err = m.sys(ctx, func(c *pgx.Conn) error {
		_, err := c.Exec(ctx, `DELETE FROM kingres_cp.server WHERE server_id = $1`, in.ServerID)
		return err
	})
	if err != nil {
		return err
	}
	_ = m.Peers.Delete(in.ServerID)
	return m.distributePeers(ctx, env, in.ServerID)
}

func (m *Manager) stepLeaveRevoke(ctx context.Context, env *task.Env) error {
	in, err := inputOf[leaveInput](env.Task)
	if err != nil {
		return err
	}
	ca, err := m.Svc.CA()
	if err != nil {
		return err
	}
	if err := m.revokeServerCertificates(ctx, ca, in.ServerID, in.Serial); err != nil {
		return err
	}
	m.Client.Forget(in.ServerID)
	if err := m.distributeRevocations(ctx); err != nil {
		return err
	}
	m.audit(ctx, env.Task.CreatedBy, "leaveCluster", in.ServerID, audit.OutcomeSuccess, map[string]any{"taskId": env.Task.ID, "phase": "completed"})
	return nil
}

// ------------------------------------------------------- the leaving server

// Leaver is what the HTTP layer needs of a server that leaves its cluster.
type Leaver interface {
	LeaveCluster(ctx context.Context, actor string, force bool) (task.Task, error)
}

var _ Leaver = (*Manager)(nil)

// LeaveCluster is POST /this-server/leave-cluster on the server that leaves.
// It makes the checks of the contract on its own copy of the catalog, asks the
// manager leader to run the task (the leader checks again from its copy and
// answers for the task), and arranges to reset itself when the task ends. With
// force and a leader that cannot be reached it leaves on its own: it resets
// itself and the cluster keeps listing it until it is removed from there.
func (m *Manager) LeaveCluster(ctx context.Context, actor string, force bool) (task.Task, error) {
	f, err := m.catalogFacts(ctx, m.Self)
	if err != nil {
		m.log().Warn("reading the local catalog for a leave failed", "error", err)
		return task.Task{}, problem.New(problem.ServiceUnavailable, "The local copy of the catalog cannot be read now.").WithRetryAfter(5)
	}
	leaderID, lerr := m.Leaders.Get(ctx)
	if lerr == nil && leaderID == m.Self {
		f.HostsMapGroup = true
	}
	if p := decideRemoval(m.Self, true, f); p != nil {
		return task.Task{}, p
	}
	if lerr != nil || leaderID == "" {
		return m.leaveAlone(actor, force, "The manager leader is not known.")
	}
	var res wire.LeaveResponse
	err = m.do(ctx, leaderID, "POST", "/leave-cluster", wire.LeaveRequest{Actor: actor, Force: force}, &res)
	var ae *peerclient.APIError
	switch {
	case err == nil:
		m.arrangeReset(res.TaskID, leaderID)
		return taskOfLeave(res, m.Self), nil
	case errors.Is(err, peerclient.ErrUnreachable), errors.Is(err, peerclient.ErrUnknownPeer):
		return m.leaveAlone(actor, force, "The manager leader cannot be reached.")
	case errors.As(err, &ae) && ae.Status == 401 && ae.Code == "certificate_refused":
		// The certificates of this server were revoked: the cluster does not talk to it.
		return m.leaveAlone(actor, force, "The cluster no longer accepts the certificate of this server.")
	case errors.As(err, &ae):
		return task.Task{}, problemOfAPIError(ae)
	}
	return task.Task{}, err
}

// problemOfAPIError turns the refusal of the leader into the problem the
// client sees: the leader's code when the contract knows it.
func problemOfAPIError(ae *peerclient.APIError) *problem.Problem {
	if p := problem.New(problem.Code(ae.Code), ae.Message); string(p.Code) == ae.Code && p.Status == ae.Status {
		return p
	}
	switch ae.Status {
	case 400:
		return problem.New(problem.BadRequest, ae.Message)
	case 404:
		return problem.New(problem.NotFound, ae.Message)
	case 409:
		return problem.New(problem.Conflict, ae.Message)
	case 422:
		return problem.New(problem.Unprocessable, ae.Message)
	}
	return problem.New(problem.ServiceUnavailable, "The manager leader refused or could not answer the request.").WithRetryAfter(5)
}

func taskOfLeave(res wire.LeaveResponse, self string) task.Task {
	t := task.Task{ID: res.TaskID, Kind: res.Kind, State: task.State(res.State), SubjectType: "server", SubjectID: self,
		CreatedAt: res.CreatedAt, UpdatedAt: res.CreatedAt, CreatedBy: res.CreatedBy}
	for _, n := range res.Steps {
		t.Steps = append(t.Steps, task.StepState{Name: n, State: task.StepPending})
	}
	return t
}

// leaveAlone is the end of a leave when the leader cannot answer. Without
// force it is refused (503). With force the server resets itself shortly
// after the answer was sent; no task exists on any server, so the answer
// carries a finished task with id 0 that cannot be fetched.
func (m *Manager) leaveAlone(actor string, force bool, why string) (task.Task, error) {
	if !force {
		return task.Task{}, problem.New(problem.ServiceUnavailable,
			why+" Try again later, or ask with force to leave without the cluster (it keeps listing this server until it is removed there).").WithRetryAfter(10)
	}
	m.log().Warn("leaving the cluster without the leader", "reason", why, "actor", actor)
	go func() {
		time.Sleep(leaveForcedDelay)
		if err := m.ResetLocal(m.bg()); err != nil {
			m.log().Error("resetting this server after a forced leave failed", "error", err)
		}
	}()
	now := time.Now().UTC()
	t := task.Task{ID: 0, Kind: KindLeave, State: task.Done, SubjectType: "server", SubjectID: m.Self, Percent: 100,
		CreatedAt: now, UpdatedAt: now, CreatedBy: actor}
	t.Steps = []task.StepState{{Name: "Reset this server (the cluster was not reachable)", State: task.StepDone}}
	return t, nil
}

func (m *Manager) bg() context.Context {
	if m.Ops != nil && m.Ops.BG != nil {
		return m.Ops.BG
	}
	return context.Background()
}

// ResetLocal returns this server to the state of a fresh install: instance,
// key and certificates deleted, a new setup token, the peer directory
// reduced to this server. The audit file and the server's own log are not
// touched.
func (m *Manager) ResetLocal(ctx context.Context) error {
	if err := m.Svc.ResetMember(ctx); err != nil {
		return err
	}
	for _, p := range m.Peers.All() {
		m.Client.Forget(p.ServerID)
	}
	if err := m.Peers.Replace(nil); err != nil {
		return err
	}
	if m.SelfPeer.ServerID != "" {
		_ = m.Peers.Put(m.SelfPeer)
	}
	// Connections opened with the certificate that was just deleted must not be used again
	// (the leader checks the certificate at every request).
	if m.OnCertificateInstalled != nil {
		m.OnCertificateInstalled()
	}
	_ = os.Remove(m.leaveMarker())
	return nil
}

// ------------------------------------------------- the end of a normal leave

type leaveMark struct {
	TaskID int64  `json:"taskId"`
	Leader string `json:"leader"`
}

func (m *Manager) leaveMarker() string {
	return filepath.Join(filepath.Dir(m.Svc.PKIDir()), "leave-pending.json")
}

// arrangeReset records that this server has a leave task running and starts
// watching it. The record survives a restart of kingresd (ResumeLeave).
func (m *Manager) arrangeReset(taskID int64, leaderID string) {
	mark := leaveMark{TaskID: taskID, Leader: leaderID}
	if b, err := json.Marshal(mark); err == nil {
		if err := os.WriteFile(m.leaveMarker(), b, 0o600); err != nil {
			m.log().Warn("recording the leave task failed", "error", err)
		}
	}
	m.watchLeave(mark)
}

// ResumeLeave restarts the watch of a leave that was under way when kingresd
// stopped. A record of a server that is no longer clustered is dropped.
func (m *Manager) ResumeLeave() {
	b, err := os.ReadFile(m.leaveMarker())
	if err != nil {
		return
	}
	var mark leaveMark
	if json.Unmarshal(b, &mark) != nil || mark.TaskID == 0 || !m.Svc.Clustered() {
		_ = os.Remove(m.leaveMarker())
		return
	}
	m.watchLeave(mark)
}

// watchLeave resets this server when its leave task ends: when the leader
// reports the task done at once, and when the leader refuses the (revoked)
// certificate, which only happens in the last step, after a short grace for
// the leader to record the end. A failed task (the certificate is still valid)
// leaves the server a member and drops the record.
func (m *Manager) watchLeave(mark leaveMark) {
	ctx := m.bg()
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-time.After(leavePoll):
			}
			var st wire.LeaveStatusResponse
			err := m.do(ctx, mark.Leader, "POST", "/leave-status", wire.LeaveStatusRequest{TaskID: mark.TaskID}, &st)
			var ae *peerclient.APIError
			switch {
			case err == nil && st.State == string(task.Done):
				m.finishLeave()
				return
			case err == nil && st.State == string(task.Failed):
				m.log().Warn("the leave task failed; this server stays in the cluster", "task", mark.TaskID)
				_ = os.Remove(m.leaveMarker())
				return
			case errors.As(err, &ae) && ae.Status == 401:
				// The certificate is revoked: the last step ran.
				select {
				case <-ctx.Done():
					return
				case <-time.After(leaveGrace):
				}
				m.finishLeave()
				return
			}
		}
	}()
}

func (m *Manager) finishLeave() {
	if err := m.ResetLocal(m.bg()); err != nil {
		m.log().Error("resetting this server after leaving the cluster failed", "error", err)
	}
}
